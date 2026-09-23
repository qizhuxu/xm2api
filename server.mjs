/**
 * 线路2 反代 —— SSO 会话 → mimo-server `/api/route/*`
 *
 * 端口：18787（env XM2API_PORT）
 *
 * 凭证：data/sso-session.json 里的 `routeCookieHeader`
 *   （`serviceToken` + `userId`，由 creds.mjs 换取），每个请求实时读取并注入。
 *   调用方不需要任何 API key。
 *
 * 端点：
 *   POST /v1/chat/completions      标准 OpenAI 入口
 *        ├─ tools / tool_choice / parallel_tool_calls → 原生支持（含流式分片）
 *        ├─ tools:[{type:"web_search"}]               → 上游自带联网搜索，回 annotations + 引用
 *        └─ response_format: json_object | json_schema → 原生支持
 *   POST /v1/images/generations    图像（Doubao-Seedream-5.0-pro）
 *   GET  /v1/models                上游目录 + 能力标注
 *   GET  /v1/models/{id}           单个模型
 *   GET  /usage                    账号使用量查询（上游 /api/user/usage）
 *   GET  /__xm2api                 自检（也响应 / 与 /health）
 *
 * 兼容层：请求体里出现非标准 `web_search` 键时翻译成 tools 声明，其余请求逐字节透传。
 * 详见 docs/tools-and-search.md
 */
import path from "node:path";
import fs from "node:fs";
import { fileURLToPath } from "node:url";
import { createRouteServer, listen, sendJson } from "./lib/upstream.mjs";
import { config, CONFIG_PATH } from "./lib/config.mjs";
import { SESSION_OUT } from "./lib/chrome-cookie.mjs";
import { requestGuard, checkAdmin, adminKey, ADMIN_KEY_FILE } from "./lib/admin.mjs";
import {
  listAccounts,
  readAccount,
  removeAccount,
  setAccountEnabled,
  setAccountLabel,
  importAccount,
  extractLocalAccount,
  fetchUsage,
  fetchSessionUsage,
} from "./lib/accounts.mjs";
import { credentialsStatus } from "./lib/pipeline.mjs";

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const PROJECT_ROOT = __dirname;

// 全部来自 config.yaml，可被环境变量覆盖（见 lib/config.mjs）
const PORT = config.server.port;
const HOST = config.server.host;
const LOG_DIR = config.logging.dir;
const MIMO_SERVER = config.server.upstream;

const ROUTES = [
  {
    // Native SSO path
    match: (p) => p === "/route" || p.startsWith("/route/") || p.startsWith("/api/route"),
    target: MIMO_SERVER,
    rewrite: (p) => (p.startsWith("/api") ? p : `/api${p.startsWith("/") ? p : `/${p}`}`),
  },
  {
    // Standard OpenAI entry point backed by the SSO route
    match: (p) => p === "/v1/chat/completions" || p === "/v1/completions",
    target: MIMO_SERVER,
    rewrite: () => "/api/route/chat/completions",
  },
  {
    // 图像生成（上游同样镜像 OpenAI images 接口；模型如 Doubao-Seedream-5.0-pro）
    match: (p) => p === "/v1/images/generations",
    target: MIMO_SERVER,
    rewrite: () => "/api/route/images/generations",
  },
  {
    // 语音合成：上游有这条路径，但当前没配供应商（401 该模型未指定供应商）。
    // TTS 实际走 chat/completions + audio 字段，见 examples/all-models.py
    match: (p) => p === "/v1/audio/speech",
    target: MIMO_SERVER,
    rewrite: () => "/api/route/audio/speech",
  },
  {
    // 语音识别：同 TTS，实际走 chat/completions + input_audio
    match: (p) => p === "/v1/audio/transcriptions",
    target: MIMO_SERVER,
    rewrite: () => "/api/route/audio/transcriptions",
  },
  {
    // Other mimo-server endpoints (user info, model catalog, ...)
    match: (p) => p.startsWith("/api/"),
    target: MIMO_SERVER,
    rewrite: (p) => p,
  },
];

function readSession() {
  try {
    return JSON.parse(fs.readFileSync(SESSION_OUT, "utf8"));
  } catch {
    return null;
  }
}

/**
 * Attach the SSO credentials. `/api/route/*` is not authenticated by the
 * Chromium passToken dump (that yields 401) — it needs the per-sid
 * serviceToken obtained by scripts/06 + 08. Never overrides a caller's cookie.
 */
function inject(headers, target) {
  if (headers.cookie || headers.Cookie) return;
  if (!/mimo-server-cn\./i.test(target.hostname)) return;
  const cookie = readSession()?.routeCookieHeader;
  if (cookie) headers.cookie = cookie;
}

/* ------------------------------------------------ 模型清单（上游 /api/model/list） */

const MODELS_TTL_MS = Number(process.env.XM2API_MODELS_TTL_MS || 5 * 60 * 1000);
let modelsCache = { at: 0, list: null, error: null };

/** 兜底清单：上游取不到时用它，保证 /v1/models 不会失败 */
const FALLBACK_MODELS = [
  { modelName: "mimo-v2.6-pro", modelType: "TEXT", vendorName: "Mify" },
  { modelName: "mimo-v2.6-flash", modelType: "TEXT", vendorName: "Mify" },
];

/**
 * 按 modelType 标注能力。这些不是猜的，是逐项实测过的（见 docs/tools-and-search.md）：
 *   TEXT  → 图像输入 / tools / web_search / json_schema / 流式 全部可用
 *   TTS、ASR → 走的是 chat/completions + 扩展字段，不是 /v1/audio/*
 */
const TYPE_CAPS = {
  TEXT: { api: "/v1/chat/completions", capabilities: ["chat", "streaming", "vision", "tools", "web_search", "json_schema", "reasoning"] },
  IMAGE_GENERATION: { api: "/v1/images/generations", capabilities: ["image"] },
  TTS: { api: "/v1/chat/completions", capabilities: ["speech"], via: "chat/completions + audio{format,voice}" },
  ASR: { api: "/v1/chat/completions", capabilities: ["transcription"], via: "chat/completions + input_audio" },
};

function toOpenAiModel(m) {
  const type = String(m.modelType || "").toUpperCase();
  const caps = TYPE_CAPS[type] || {};
  return {
    id: m.modelName,
    object: "model",
    created: 1789000000,
    owned_by: m.vendorName || "xiaomi",
    // 以下是上游带的、非 OpenAI 标准的补充信息，客户端一般会忽略
    model_type: m.modelType,
    description: m.description,
    billable: m.billable,
    display_ratio: m.displayRatio,
    // 反代补充：实测能力 + 该模型该打哪个端点
    capabilities: caps.capabilities,
    api: caps.api,
    via: caps.via,
    // 推理等级实测：上游根本没实现——low/high 无差异、none 关不掉思考、
    // 非法值（banana/xhigh）照样 200，链路上没人校验。客户端不显示等级选择器是正常的，
    // 详见 README「推理等级」一节
    reasoning_effort: type === "TEXT" ? "unsupported（上游实测忽略，思考固定开启）" : undefined,
    price: m.ratio?.inputPricePerM != null
      ? { input_per_m: m.ratio.inputPricePerM, output_per_m: m.ratio.outputPricePerM, cached_per_m: m.ratio.cachedPricePerM, currency: "CNY" }
      : m.ratio?.imageResolutionPrices
        ? { per_image: Object.fromEntries(m.ratio.imageResolutionPrices.map((p) => [p.tier, p.pricePerImage])), currency: "CNY" }
        : undefined,
  };
}

/** 带上 SSO cookie 去问上游要目录 */
async function fetchUpstreamModels() {
  const cookie = readSession()?.routeCookieHeader;
  const res = await fetch(`${MIMO_SERVER}/api/model/list`, {
    headers: cookie ? { cookie, accept: "application/json" } : { accept: "application/json" },
    signal: AbortSignal.timeout(10000),
  });
  if (!res.ok) throw new Error(`HTTP ${res.status}`);
  const j = await res.json();
  if (j?.code !== 0) throw new Error(`code=${j.code} ${j.message || ""}`.trim());
  const list = j?.data?.models;
  if (!Array.isArray(list) || !list.length) throw new Error("上游返回了空目录");
  return list;
}

/** 按配置产出模型清单（带 5 分钟缓存；上游失败回落兜底） */
async function resolveModels() {
  // 配置里直接写死一个列表
  if (Array.isArray(config.server.models)) {
    return { list: config.server.models.map((id) => toOpenAiModel({ modelName: id })), source: "config" };
  }
  const now = Date.now();
  if (!modelsCache.list || now - modelsCache.at > MODELS_TTL_MS) {
    try {
      const list = await fetchUpstreamModels();
      modelsCache = { at: now, list, error: null };
    } catch (err) {
      modelsCache = { at: now, list: modelsCache.list, error: err.message };
      if (!modelsCache.list) {
        console.error(`[models] 上游目录获取失败，用兜底清单：${err.message}`);
        return { list: FALLBACK_MODELS.map(toOpenAiModel), source: "fallback", error: err.message };
      }
    }
  }
  const types = (config.server.modelTypes || []).map((t) => String(t).toUpperCase());
  const filtered = types.length
    ? modelsCache.list.filter((m) => types.includes(String(m.modelType).toUpperCase()))
    : modelsCache.list;
  return { list: filtered.map(toOpenAiModel), source: "upstream", error: modelsCache.error };
}

/* ------------------------------------------------ 兼容层：web_search 便捷开关 */

/**
 * 上游只认 `tools: [{type:"web_search"}]`（可带 max_keyword / force_search / limit）。
 * 而 `web_search: true`、`web_search: {enable:true}` 这类写法上游**完全忽略** ——
 * 模型会一本正经地回答"我没有联网能力"。这里把它翻译成标准工具声明。
 *
 * 两档，都由 config.yaml 控制：
 *   webSearchFlag（默认开）—— 请求里出现 `web_search` 键时才翻译
 *   webSearchAuto（默认开）—— 纯聊天请求（没自带 tools）注入裸工具，模型自己决定搜不搜
 *
 * 不满足条件就返回 null，请求体逐字节透传。
 */
const SEARCH_TOOL_KEYS = ["max_keyword", "force_search", "limit"];

/** 目录里标的不是 TEXT 的（TTS/ASR/图像）不能塞工具 —— 它们也走 chat/completions */
const NON_CHAT_NAME = /tts|asr|seedream|image|voiceclone|voicedesign|embedding|rerank/i;

function autoSearchAllowed(name) {
  const n = String(name || "");
  // ① 名字一看就不是聊天模型 → 一定不注入（目录拉不到时也拦得住）
  if (NON_CHAT_NAME.test(n)) return false;
  // ② 目录里有 → 严格按 modelType 判定
  const m = modelsCache.list?.find((x) => x.modelName === n);
  if (m) return String(m.modelType || "").toUpperCase() === "TEXT";
  // ③ 目录里没有（别名如 mimo-pro，或目录没拉到）→ 当成聊天模型
  //    （上面第 ① 条已经挡掉了已知的非聊天模型，这里放行是安全的）
  return true;
}

function webSearchCompat(bodyBuf, { pathname }) {
  const { webSearchFlag, webSearchAuto } = config.server.compat;
  if (!webSearchFlag && !webSearchAuto) return null;
  if (!/(^|\/)chat\/completions$/.test(pathname) && pathname !== "/v1/completions") return null;

  let j;
  try {
    j = JSON.parse(bodyBuf.toString("utf8"));
  } catch {
    return null; // 不是 JSON，原样转发
  }
  if (!j || typeof j !== "object" || Array.isArray(j)) return null;

  const hasKey = Object.prototype.hasOwnProperty.call(j, "web_search");
  if (!hasKey && !webSearchAuto) return null; // 默认路径：一个字都不改
  if (!hasKey) {
    // auto 档的两条边界：
    //  ① 调用方自带 tools（函数调用/agent 场景）→ 不插手。
    //     实测塞进去的 web_search 会跟调用方的函数抢："上海天气如何？"模型会
    //     改去联网而不调 get_weather。要不要搜索交给调用方自己决定。
    //  ② TTS/ASR 也走 chat/completions → 不能塞工具
    if (Array.isArray(j.tools) && j.tools.length) return null;
    if (!autoSearchAllowed(j.model)) return null;
  }

  const ws = hasKey ? j.web_search : undefined;
  if (hasKey) delete j.web_search;

  const tools = Array.isArray(j.tools) ? [...j.tools] : [];
  const already = tools.some((t) => t && (t.type === "web_search" || t.type === "builtin_web_search"));

  // web_search:false —— 显式"这次别搜"，同时也不做 auto 注入
  if (hasKey && (ws === false || ws === null)) {
    return { body: Buffer.from(JSON.stringify(j)), rewrites: ["丢掉 web_search=false（本次不联网）"] };
  }
  // 形态不认识（字符串等）→ 原样转发，别乱动
  if (hasKey && ws !== true && (typeof ws !== "object" || Array.isArray(ws))) return null;
  // 调用方自己已经声明了搜索工具
  if (already) {
    return hasKey
      ? { body: Buffer.from(JSON.stringify(j)), rewrites: ["web_search 键已删除（tools 里本来就有 web_search）"] }
      : null;
  }

  const tool = { type: "web_search" };
  if (hasKey && ws !== true) for (const k of SEARCH_TOOL_KEYS) if (ws[k] !== undefined) tool[k] = ws[k];
  tools.push(tool);
  j.tools = tools;
  return {
    body: Buffer.from(JSON.stringify(j)),
    rewrites: [
      hasKey
        ? `web_search:${JSON.stringify(ws)} → tools += ${JSON.stringify(tool)}`
        : `auto 注入 ${JSON.stringify(tool)}（模型自己决定搜不搜）`,
    ],
  };
}

/* ------------------------------------------------ 未匹配端点的友好 404 */

const SUPPORTED = [
  "POST /v1/chat/completions      （chat / tools / web_search / json_schema / TTS / ASR）",
  "POST /v1/completions           同 chat/completions",
  "POST /v1/images/generations    图像生成",
  "POST /route/chat/completions   等价旧路径",
  "POST /api/*                    直通上游 mimo-server",
  "GET  /v1/models                模型清单（含 capabilities / price）",
  "GET  /v1/models/{id}           单个模型",
  "GET  /usage                    账号使用量查询（上游 /api/user/usage）",
  "GET  /ui/                      管理界面（静态单页：状态/凭证/额度/设置）",
  "ALL  /api/__admin/*            管理 API（需 X-Management-Key，见 data/admin-key.txt）",
  "GET  /__xm2api                 自检",
];

function onUnmatched(req, res, pathname) {
  sendJson(res, 404, {
    error: {
      message: `反代没有这条路径：${req.method} ${pathname}`,
      type: "invalid_request_error",
      code: "unsupported_endpoint",
    },
    supported: SUPPORTED,
  });
  return true;
}

/* ------------------------------------------------ 本机停止接口 */

/**
 * POST /__xm2api/shutdown —— 只接受本机回环地址。
 *
 * 为什么要有它：菜单原先只靠 logs/server.pid 去 kill，pid 文件一旦过期或缺失
 * （服务是用别的方式启动的、或文件被别的实例覆盖过）就完全停不掉。
 * 有这条接口之后，不管服务是谁启的（菜单 / npm run serve / 手动 node server.mjs），
 * 只要能连上就能让它自己干净退出 —— 不再依赖 pid 文件。
 */
const PID_FILE = path.join(PROJECT_ROOT, "logs", "server.pid");

/** 只删自己写的那个 pid 文件，别把别的实例的删了 */
function clearOwnPidFile() {
  try {
    if (Number(fs.readFileSync(PID_FILE, "utf8").trim()) === process.pid) fs.rmSync(PID_FILE, { force: true });
  } catch {}
}

async function shutdown(reason) {
  console.log(`[shutdown] ${reason}，正在关闭…`);
  await routeServer.close();
  clearOwnPidFile();
  console.log("[shutdown] 已停止");
  process.exit(0);
}

function isLoopback(req) {
  const ra = String(req.socket.remoteAddress || "");
  return ra === "127.0.0.1" || ra === "::1" || ra === "::ffff:127.0.0.1";
}

const SHUTDOWN_ROUTE = {
  method: "POST",
  path: "/__xm2api/shutdown",
  handler: (req, res) => {
    if (!isLoopback(req)) {
      sendJson(res, 403, { error: { message: `只接受本机请求（来自 ${req.socket.remoteAddress}）`, type: "invalid_request_error" } });
      return;
    }
    // 带 Origin 的一定是浏览器发的。本机网页也能 POST 到 127.0.0.1，
    // 不加这道判断，随便一个网页就能把你的反代关掉。
    if (req.headers.origin) {
      sendJson(res, 403, {
        error: { message: `拒绝来自浏览器的停止请求（Origin: ${req.headers.origin}）`, type: "invalid_request_error" },
      });
      return;
    }
    sendJson(res, 200, { ok: true, stopping: true, port: PORT });
    // 留 200ms 让客户端先把响应读走，再动连接
    setTimeout(() => shutdown("收到本机停止请求"), 200);
  },
};

/* ---------------------------- 管理 UI（/ui/ 静态单页）与管理 API（/api/__admin/*） */

const UI_DIR = path.join(PROJECT_ROOT, "ui");
const UI_MIME = {
  ".html": "text/html; charset=utf-8",
  ".js": "text/javascript; charset=utf-8",
  ".css": "text/css; charset=utf-8",
  ".svg": "image/svg+xml",
  ".png": "image/png",
  ".ico": "image/x-icon",
  ".json": "application/json; charset=utf-8",
};

/** 静态管理界面（零框架单页）。只读 ui/ 目录、防目录穿越。 */
function serveUi(req, res, pathname) {
  const rel = pathname.replace(/^\/ui\/?/, "") || "index.html";
  const file = path.normalize(path.join(UI_DIR, rel));
  if (!file.startsWith(UI_DIR + path.sep)) {
    sendJson(res, 404, { error: { message: "not found", type: "invalid_request_error" } });
    return;
  }
  try {
    const buf = fs.readFileSync(file);
    res.writeHead(200, {
      "content-type": UI_MIME[path.extname(file).toLowerCase()] || "application/octet-stream",
      "cache-control": "no-cache",
    });
    res.end(buf);
  } catch {
    sendJson(res, 404, { error: { message: `静态资源不存在：${rel}`, type: "invalid_request_error" } });
  }
}

function readBody(req) {
  return new Promise((resolve) => {
    const chunks = [];
    req.on("data", (c) => chunks.push(c));
    req.on("end", () => resolve(Buffer.concat(chunks)));
    req.on("error", () => resolve(Buffer.alloc(0)));
  });
}

/** 管理 API 统一鉴权包装（X-Management-Key 或 Authorization: Bearer）。 */
const withAdmin = (fn) => (req, res, pathname) => {
  if (!checkAdmin(req)) {
    sendJson(res, 401, {
      error: {
        message: "管理 API 需要 X-Management-Key 请求头（密钥见 data/admin-key.txt 或启动日志）",
        type: "invalid_request_error",
        code: "unauthorized",
      },
    });
    return;
  }
  fn(req, res, pathname);
};

async function adminStatus(req, res) {
  const session = readSession();
  sendJson(res, 200, {
    ok: true,
    name: "xm2api 线路2 (SSO route)",
    port: PORT,
    host: HOST,
    upstream: MIMO_SERVER,
    ui: `http://${HOST}:${PORT}/ui/`,
    config: { file: path.relative(PROJECT_ROOT, CONFIG_PATH) || CONFIG_PATH, source: config.meta?.source },
    session: session?.routeCookieHeader
      ? { present: true, sid: session.sso?.sid || null, obtainedAt: session.sso?.obtainedAt || null }
      : { present: false, fix: "凭证页 →「从本机 MiMo 客户端提取」（或 npm run refresh）" },
    credentials: credentialsStatus(),
    accounts: listAccounts().length,
    security: {
      admin_key_file: path.relative(PROJECT_ROOT, ADMIN_KEY_FILE) || ADMIN_KEY_FILE,
      host_check: "开（防 DNS rebinding）",
      origin_check: "开（浏览器跨站请求 403；同源 /ui/ 与 SDK 不受影响）",
      allowed_hosts: config.server.allowedHosts,
      allowed_origins: config.server.allowedOrigins,
    },
    logging: { enabled: config.logging.enabled, captureBody: config.logging.captureBody },
  });
}

function adminAccountsList(req, res) {
  sendJson(res, 200, { accounts: listAccounts() });
}

async function adminAccountsExtract(req, res) {
  const r = await extractLocalAccount();
  if (!r.ok) {
    sendJson(res, 502, { error: { message: `${r.action || "extract"} 失败：${r.error}`, type: "upstream_error" }, steps: r.steps || [] });
    return;
  }
  sendJson(res, 200, { ok: true, steps: r.steps || [], account: r.account });
}

async function adminAccountsImport(req, res) {
  const raw = (await readBody(req)).toString("utf8");
  const r = importAccount(raw);
  if (!r.ok) {
    sendJson(res, 400, { error: { message: r.error, type: "invalid_request_error" } });
    return;
  }
  sendJson(res, 200, { ok: true, account: r.account });
}

async function adminAccountsPatch(req, res, pathname) {
  const id = decodeURIComponent(pathname.split("/").pop());
  let j = {};
  try {
    j = JSON.parse((await readBody(req)).toString("utf8") || "{}");
  } catch {}
  let out = null;
  if (j.enabled !== undefined) out = setAccountEnabled(id, j.enabled);
  else if (j.label !== undefined) out = setAccountLabel(id, j.label);
  if (!out) {
    sendJson(res, 404, { error: { message: `没有这个账号：${id}`, type: "invalid_request_error", code: "not_found" } });
    return;
  }
  sendJson(res, 200, { ok: true, account: out });
}

function adminAccountsDelete(req, res, pathname) {
  const id = decodeURIComponent(pathname.split("/").pop());
  if (!removeAccount(id)) {
    sendJson(res, 404, { error: { message: `没有这个账号：${id}`, type: "invalid_request_error", code: "not_found" } });
    return;
  }
  sendJson(res, 200, { ok: true, removed: id });
}

async function adminQuota(req, res) {
  const accounts = listAccounts().filter((a) => a.enabled);
  const results = await Promise.all(
    accounts.map(async (s) => {
      const acc = readAccount(s.id);
      try {
        const u = await fetchUsage(acc);
        return { id: s.id, label: s.label, ok: true, ...u };
      } catch (e) {
        return { id: s.id, label: s.label, ok: false, error: String(e.message || e) };
      }
    })
  );
  let session = null;
  try {
    const u = await fetchSessionUsage();
    if (u) session = { ok: true, ...u };
  } catch (e) {
    session = { ok: false, error: String(e.message || e) };
  }
  sendJson(res, 200, { session, accounts: results, observed_at: new Date().toISOString() });
}

/* ------------------------------------------------ 服务 */

const routeServer = createRouteServer({
  name: "xm2api 线路2 (SSO route)",
  port: PORT,
  host: HOST,
  routes: ROUTES,
  inject,
  logDir: LOG_DIR,
  logPrefix: "path2",
  logging: config.logging,
  upstreamTimeoutMs: config.server.upstreamTimeoutMs,
  transform: webSearchCompat,
  onUnmatched,
  guard: requestGuard,
  local: [
    SHUTDOWN_ROUTE,
    { method: "GET", path: /^\/ui(\/.*)?$/, handler: (req, res, p) => serveUi(req, res, p) },
    { method: "GET", path: "/api/__admin/status", handler: withAdmin(adminStatus) },
    { method: "GET", path: "/api/__admin/accounts", handler: withAdmin(adminAccountsList) },
    { method: "POST", path: "/api/__admin/accounts/extract", handler: withAdmin(adminAccountsExtract) },
    { method: "POST", path: "/api/__admin/accounts/import", handler: withAdmin(adminAccountsImport) },
    { method: "PATCH", path: /^\/api\/__admin\/accounts\/[^/]+$/, handler: withAdmin(adminAccountsPatch) },
    { method: "DELETE", path: /^\/api\/__admin\/accounts\/[^/]+$/, handler: withAdmin(adminAccountsDelete) },
    { method: "GET", path: "/api/__admin/quota", handler: withAdmin(adminQuota) },
    {
      method: "GET",
      path: "/v1/models",
      handler: async (req, res) => {
        const { list, source, error } = await resolveModels();
        const body = { object: "list", data: list };
        if (error) body.warning = `上游模型目录刷新失败，可能不是最新：${error}`;
        body.source = source;
        sendJson(res, 200, body);
      },
    },
    {
      // OpenAI 标准的单模型查询，不少客户端（Cherry Studio / LobeChat 等）会先探这个
      method: "GET",
      path: /^\/v1\/models\/[^/]+$/,
      handler: async (req, res, pathname) => {
        const id = decodeURIComponent(pathname.slice("/v1/models/".length));
        const { list, source, error } = await resolveModels();
        const found = list.find((m) => m.id === id);
        if (!found) {
          const ids = list.map((m) => m.id);
          sendJson(res, 404, {
            error: {
              message: `没有这个模型：${id}`,
              type: "invalid_request_error",
              code: "model_not_found",
            },
            available: ids,
            hint: "注意反代只暴露上游 /api/model/list 里的模型；模型名区分大小写。",
            source,
            warning: error ? `上游目录刷新失败：${error}` : undefined,
          });
          return;
        }
        sendJson(res, 200, found);
      },
    },
    {
      // 账号使用量查询：上游 /api/user/usage（Cookie 鉴权），响应原样透传
      method: "GET",
      path: /^\/(v1\/)?usage$/,
      handler: async (req, res) => {
        const cookie = readSession()?.routeCookieHeader;
        if (!cookie) {
          sendJson(res, 401, {
            error: {
              message: "没有 SSO 会话凭证，无法查询使用量",
              type: "auth_error",
              code: "no_credentials",
            },
            fix: "npm run refresh",
          });
          return;
        }
        try {
          const r = await fetch(`${MIMO_SERVER}/api/user/usage`, {
            headers: { cookie, accept: "application/json" },
            signal: AbortSignal.timeout(config.server.upstreamTimeoutMs || 60000),
          });
          const text = await r.text();
          let body;
          try {
            body = JSON.parse(text);
          } catch {
            body = { raw: text };
          }
          if (!r.ok) {
            sendJson(res, r.status, {
              error: { message: `上游 /api/user/usage 返回 HTTP ${r.status}`, type: "upstream_error" },
              upstream: body,
            });
            return;
          }
          sendJson(res, 200, body);
        } catch (e) {
          sendJson(res, 502, {
            error: { message: `查询使用量失败：${e.message || e}`, type: "upstream_error" },
          });
        }
      },
    },
  ],
  meta: () => {
    const session = readSession();
    return {
      ok: true,
      name: "xm2api 线路2 (SSO route)",
      route: "2",
      port: PORT,
      upstream: MIMO_SERVER + "/api/route/chat/completions",
      models: {
        source: Array.isArray(config.server.models) ? "config" : "upstream",
        endpoint: MIMO_SERVER + "/api/model/list",
        types: config.server.modelTypes,
      },
      // 实测过的上游能力，别照抄 OpenAI 文档想当然
      capabilities: {
        tools: true,                       // function calling，流式分片也正常
        parallel_tool_calls: true,
        tool_choice: ["auto", "none", "required", { type: "function", function: { name: "..." } }],
        web_search: {
          native: 'tools: [{type:"web_search"}]  →  message.annotations[] + usage.web_search_usage',
          tool_options: { max_keyword: "改写关键词条数", force_search: "提高搜索概率（不是强制）", limit: "参考网页条数" },
          default_behavior: config.server.compat.webSearchAuto
            ? "反代默认给不带 tools 的 TEXT 聊天请求注入 web_search 工具，由模型自己决定搜不搜（想纯透传就把 webSearchAuto 关掉）"
            : "反代默认不主动联网：请求里没提 web_search 就一个字都不改",
          compat_flag: config.server.compat.webSearchFlag
            ? '接受非标准 web_search: true / {max_keyword,force_search,limit}，由反代翻译成工具声明'
            : "翻译已关闭（config.yaml → server.compat.webSearchFlag）",
          compat_auto: config.server.compat.webSearchAuto
            ? '已开启（默认）：不带 tools 的 TEXT 聊天请求会注入裸 tools:[{type:"web_search"}]；自带工具（函数调用）的请求不碰'
            : "未开启（config.yaml → server.compat.webSearchAuto）—— 需要每个请求都带搜索能力时再打开",
        },
        vision: "content[].type=image_url（data: 或 http(s) 均可）",
        json_schema: true,
        stream_options: { include_usage: true },
        not_supported: ["n>1（400 n is not supported）", "legacy functions/function_call（静默忽略）", "thinking / enable_thinking（无法关闭思维链）", "reasoning_effort（low/medium/high 实测无差异、none 也关不掉、非法值照样 200——上游没实现）"],
      },
      credentials: session?.routeCookieHeader
        ? { present: true, sid: session.sso?.sid || null, obtainedAt: session.sso?.obtainedAt || null }
        : { present: false, fix: "npm run refresh" },
      sessionFile: path.relative(PROJECT_ROOT, SESSION_OUT) || SESSION_OUT,
      config: { file: path.relative(PROJECT_ROOT, CONFIG_PATH) || CONFIG_PATH, source: config.meta?.source },
      logging: { enabled: config.logging.enabled, captureBody: config.logging.captureBody },
      hint: [
        `OpenAI base_url:   http://127.0.0.1:${PORT}/v1   (api_key 可填任意字符串)`,
        `Legacy path:       http://127.0.0.1:${PORT}/route/chat/completions`,
        `Meta:              http://127.0.0.1:${PORT}/__xm2api`,
        `使用量:            GET http://127.0.0.1:${PORT}/usage（账号剩余用量）`,
        `管理界面:          http://127.0.0.1:${PORT}/ui/（密钥见 data/admin-key.txt）`,
        `联网搜索:          请求体加 "web_search": true，或 tools:[{type:"web_search"}]`,
        `推理等级:          上游不支持（reasoning_effort 实测无效），客户端无需设置；思考内容照常返回`,
        `工具调用:          tools + tool_choice，用法与 OpenAI 一致`,
        `停止服务:          POST http://127.0.0.1:${PORT}/__xm2api/shutdown（仅本机）`,
        "401 → 刷新凭证:     npm run refresh",
        "改端口/日志/兼容层：编辑 config.yaml",
      ],
    };
  },
});

// 预热模型目录：兼容层 auto 档要靠 modelType 判断哪些模型能塞搜索工具
// （TTS/ASR 也走 chat/completions，给它们塞 web_search 会坏事）
resolveModels().catch(() => {});

// 管理密钥启动即生成/加载（而不是等第一个鉴权请求）：启动日志立刻可见，
// UI/脚本也能在起服务后马上读到 data/admin-key.txt。
adminKey();

listen(routeServer, {
  onReady: () => {
    console.log(`logs → ${LOG_DIR}`);
    console.log(`meta → http://${HOST}:${PORT}/__xm2api`);
    console.log(`ui   → http://${HOST}:${PORT}/ui/ （管理界面）`);
    console.log(`data → ${path.join(PROJECT_ROOT, "data")}`);
    console.log(`stop → POST http://${HOST}:${PORT}/__xm2api/shutdown （或菜单选 2）`);
  },
});

// 前台运行时 Ctrl+C / 被 kill 也走同一条清理路径，别留下过期 pid 文件。
// 注意 pid 文件是菜单写的（写的是本进程 pid），所以这里只删属于自己的那份。
for (const sig of ["SIGINT", "SIGTERM", "SIGHUP"]) {
  process.on(sig, () => {
    shutdown(`收到 ${sig}`);
  });
}
