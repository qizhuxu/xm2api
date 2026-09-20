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
 *   POST /v1/chat/completions      标准 OpenAI 入口（模型：mimo-pro | mimo-flash）
 *   POST /route/chat/completions   等价旧路径
 *   GET  /v1/models                本地生成，不打上游
 *   GET  /__xm2api                 自检（也响应 / 与 /health）
 */
import path from "node:path";
import fs from "node:fs";
import { fileURLToPath } from "node:url";
import { createRouteServer, listen, sendJson } from "./lib/upstream.mjs";
import { config, CONFIG_PATH } from "./lib/config.mjs";
import { SESSION_OUT } from "./lib/chrome-cookie.mjs";

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
  { modelName: "mimo-x-pro-preview", modelType: "TEXT", vendorName: "Mify" },
  { modelName: "mimo-x-flash-preview", modelType: "TEXT", vendorName: "Mify" },
];

function toOpenAiModel(m) {
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

const routeServer = createRouteServer({
  name: "xm2api 线路2 (SSO route)",
  port: PORT,
  host: HOST,
  routes: ROUTES,
  inject,
  logDir: LOG_DIR,
  logPrefix: "path2",
  logging: config.logging,
  local: [
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
        "401 → 刷新凭证:     npm run refresh",
        "改端口/日志/客户端实现：编辑 config.yaml",
      ],
    };
  },
});

listen(routeServer, {
  onReady: () => {
    console.log(`logs → ${LOG_DIR}`);
    console.log(`meta → http://${HOST}:${PORT}/__xm2api`);
    console.log(`data → ${path.join(PROJECT_ROOT, "data")}`);
  },
});
