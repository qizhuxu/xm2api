/**
 * config.yaml 加载器。
 *
 * 优先级：环境变量 > config.yaml > 内置默认值
 * 配置文件缺失或字段缺失时全部走默认值，不会报错。
 */
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import YAML from "yaml";

const __dirname = path.dirname(fileURLToPath(import.meta.url));
export const PROJECT_ROOT = path.resolve(__dirname, "..");
export const CONFIG_PATH = process.env.XM2API_CONFIG || path.join(PROJECT_ROOT, "config.yaml");

const DEFAULTS = {
  server: {
    host: "127.0.0.1",
    port: 18787,
    upstream: "https://mimo-server-cn.xiaomimimo.com",
    // "upstream" = 从上游 /api/model/list 拉取；也可以直接给数组写死
    models: "upstream",
    modelTypes: [],
    // 上游无响应多久算超时。图像生成 / 联网搜索单次可跑到 10~40 秒，
    // 流式响应的计时是"空闲超时"（有分片就重置），所以给 5 分钟很安全。
    upstreamTimeoutMs: 300000,
    // 管理 API（/api/__admin/*，管理 UI 用）的鉴权密钥。留空 ⇒ 自动从
    // data/admin-key.txt 读取或首启自动生成（env XM2API_ADMIN_KEY 优先级更高）。
    // ⚠️ 别把真实密钥写进 config.yaml 提交 —— 需要固定密钥时用环境变量。
    adminKey: "",
    // 安全护栏白名单（见 lib/admin.mjs）：
    //   allowedHosts   允许的 Host 头（默认只认本机回环，防 DNS rebinding）
    //   allowedOrigins 允许的跨站 Origin（默认只认同源；浏览器跨站请求一律 403）
    allowedHosts: [],
    allowedOrigins: [],
    // —— 二期：多账号池与自动续期 ——
    // pool.enabled=true 时转发从 data/accounts/ 按 strategy 选号注入；
    // 池空/关闭 ⇒ 回落 sso-session 单会话（单账号行为不劣化）。
    // 坏号（401/403）冷却 cooldownMs 并跳过；禁用账号跳过。
    pool: {
      enabled: true,
      strategy: "round-robin",
      cooldownMs: 300000,
    },
    // 转发 401 自动续期：用该账号 pass_token 走 SSO 换新 serviceToken 后重试一次，
    // 再失败换号重试一次。仅非流式、尚未向客户端写响应时进行（流式绝不回头）。
    autoRenew: true,
    // 定时兜底续期（毫秒）：账号 service_token 比这更旧就主动换新；0=纯 401 事件驱动。
    // 注意 serviceToken 无有效期声明（会话 cookie），事件驱动是主力，这只是兜底。
    refreshAfterMs: 21600000,
    compat: {
      // 见 docs/tools-and-search.md：上游只认 tools:[{type:"web_search"}]，
      // 而多数 OpenAI 客户端不会发这个。打开后，请求体里出现非标准的
      // web_search: true / {max_keyword,force_search,limit} 时会被翻译成标准工具声明。
      // 只有出现这个键才会改写，其余请求依旧是逐字节原样转发。
      webSearchFlag: true,
      // 更主动的一档：不带 tools 的 TEXT 聊天请求会注入裸 tools:[{type:"web_search"}]，
      // 由模型自己决定搜不搜（普通问题实测不加钱、不加时延）。
      // 代价是这类请求的 body 会被改写，不再是逐字节透传；
      // 自带 tools 的请求（函数调用/agent）一概不插手。
      // 设为 false 就回到"你不要求就一个字都不改"。
      webSearchAuto: true,
    },
  },
  credentials: {
    sid: "mimopc",
  },
  logging: {
    enabled: true,
    dir: "logs",
    captureBody: true,
    maxBodyChars: 20000,
  },
  client: {
    useOpenAI: true,
    baseUrl: "http://127.0.0.1:18787/v1",
    apiKey: "xm2api",
    model: "mimo-pro",
    maxTokens: 512,
    stream: true,
  },
};

function isPlainObject(v) {
  return v && typeof v === "object" && !Array.isArray(v);
}

function merge(base, over) {
  const out = { ...base };
  for (const [k, v] of Object.entries(over || {})) {
    if (v === undefined || v === null) continue;
    out[k] = isPlainObject(v) && isPlainObject(base[k]) ? merge(base[k], v) : v;
  }
  return out;
}

const bool = (v, d) => {
  if (v === undefined) return d;
  const s = String(v).trim().toLowerCase();
  if (["1", "true", "yes", "on"].includes(s)) return true;
  if (["0", "false", "no", "off"].includes(s)) return false;
  return d;
};

/** 读取并合并配置。传 file 可指定别的路径。 */
export function loadConfig(file = CONFIG_PATH) {
  let fromFile = {};
  let source = "默认值（没有配置文件）";
  try {
    if (fs.existsSync(file)) {
      fromFile = YAML.parse(fs.readFileSync(file, "utf8")) || {};
      source = path.relative(PROJECT_ROOT, file) || file;
    }
  } catch (err) {
    console.error(`[config] 解析 ${file} 失败，改用默认值：${err.message}`);
  }

  const cfg = merge(DEFAULTS, fromFile);

  // ---- 环境变量覆盖 ----
  const env = process.env;
  if (env.XM2API_HOST) cfg.server.host = env.XM2API_HOST;
  if (env.XM2API_PORT) cfg.server.port = Number(env.XM2API_PORT);
  if (env.XM2API_MIMO_SERVER) cfg.server.upstream = env.XM2API_MIMO_SERVER;
  if (env.XM2API_SID) cfg.credentials.sid = env.XM2API_SID;
  if (env.XM2API_UPSTREAM_TIMEOUT_MS) cfg.server.upstreamTimeoutMs = Number(env.XM2API_UPSTREAM_TIMEOUT_MS);
  if (env.XM2API_ALLOWED_HOSTS) {
    cfg.server.allowedHosts = env.XM2API_ALLOWED_HOSTS.split(",").map((s) => s.trim()).filter(Boolean);
  }
  if (env.XM2API_ALLOWED_ORIGINS) {
    cfg.server.allowedOrigins = env.XM2API_ALLOWED_ORIGINS.split(",").map((s) => s.trim()).filter(Boolean);
  }
  if (env.XM2API_COMPAT_WEBSEARCH !== undefined) {
    cfg.server.compat.webSearchFlag = bool(env.XM2API_COMPAT_WEBSEARCH, cfg.server.compat.webSearchFlag);
  }
  if (env.XM2API_COMPAT_WEBSEARCH_AUTO !== undefined) {
    cfg.server.compat.webSearchAuto = bool(env.XM2API_COMPAT_WEBSEARCH_AUTO, cfg.server.compat.webSearchAuto);
  }

  // 环境变量一旦给出就覆盖配置文件（不是"并且"关系）
  if (env.XM2API_LOG !== undefined) cfg.logging.enabled = bool(env.XM2API_LOG, cfg.logging.enabled);
  if (env.XM2API_LOG_BODY !== undefined) cfg.logging.captureBody = bool(env.XM2API_LOG_BODY, cfg.logging.captureBody);

  if (env.XM2API_USE_OPENAI) cfg.client.useOpenAI = bool(env.XM2API_USE_OPENAI, cfg.client.useOpenAI);
  if (env.XM2API_PROXY) cfg.client.baseUrl = env.XM2API_PROXY;
  if (env.XM2API_MODEL) cfg.client.model = env.XM2API_MODEL;
  if (env.XM2API_MAX_TOKENS) cfg.client.maxTokens = Number(env.XM2API_MAX_TOKENS);

  // ---- 归一化 ----
  cfg.server.port = Number(cfg.server.port) || DEFAULTS.server.port;
  cfg.server.upstreamTimeoutMs = Number(cfg.server.upstreamTimeoutMs) || DEFAULTS.server.upstreamTimeoutMs;
  if (!isPlainObject(cfg.server.pool)) cfg.server.pool = { ...DEFAULTS.server.pool };
  cfg.server.pool.enabled = cfg.server.pool.enabled !== false;
  cfg.server.pool.strategy = String(cfg.server.pool.strategy || "round-robin");
  cfg.server.pool.cooldownMs = Number(cfg.server.pool.cooldownMs) || DEFAULTS.server.pool.cooldownMs;
  cfg.server.autoRenew = cfg.server.autoRenew !== false;
  cfg.server.refreshAfterMs = Number(cfg.server.refreshAfterMs ?? DEFAULTS.server.refreshAfterMs);
  if (!isPlainObject(cfg.server.compat)) cfg.server.compat = { ...DEFAULTS.server.compat };
  cfg.server.compat.webSearchFlag = cfg.server.compat.webSearchFlag !== false;
  cfg.server.compat.webSearchAuto = cfg.server.compat.webSearchAuto !== false;
  cfg.logging.maxBodyChars = Number(cfg.logging.maxBodyChars) || DEFAULTS.logging.maxBodyChars;
  cfg.client.maxTokens = Number(cfg.client.maxTokens) || DEFAULTS.client.maxTokens;
  cfg.client.baseUrl = String(cfg.client.baseUrl).replace(/\/+$/, "");
  cfg.server.upstream = String(cfg.server.upstream).replace(/\/+$/, "");
  // models 支持两种形态：字符串 "upstream"（默认）或写死的数组
  if (Array.isArray(cfg.server.models)) {
    cfg.server.models = cfg.server.models.map((m) => String(m)).filter(Boolean);
    if (!cfg.server.models.length) cfg.server.models = DEFAULTS.server.models;
  } else {
    const s = String(cfg.server.models ?? "upstream").trim().toLowerCase();
    cfg.server.models = s === "config" ? DEFAULTS.server.models : "upstream";
  }
  if (!Array.isArray(cfg.server.modelTypes)) cfg.server.modelTypes = [];
  else cfg.server.modelTypes = cfg.server.modelTypes.map((t) => String(t)).filter(Boolean);
  if (!Array.isArray(cfg.server.allowedHosts)) cfg.server.allowedHosts = [];
  else cfg.server.allowedHosts = cfg.server.allowedHosts.map((t) => String(t)).filter(Boolean);
  if (!Array.isArray(cfg.server.allowedOrigins)) cfg.server.allowedOrigins = [];
  else cfg.server.allowedOrigins = cfg.server.allowedOrigins.map((t) => String(t)).filter(Boolean);
  cfg.logging.dir = path.isAbsolute(String(cfg.logging.dir))
    ? String(cfg.logging.dir)
    : path.join(PROJECT_ROOT, String(cfg.logging.dir || "logs"));

  Object.defineProperty(cfg, "meta", {
    value: { source, path: file },
    enumerable: false,
  });
  return cfg;
}

/** 进程级单例。 */
export const config = loadConfig();
