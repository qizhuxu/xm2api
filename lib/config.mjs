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

  // 环境变量一旦给出就覆盖配置文件（不是"并且"关系）
  if (env.XM2API_LOG !== undefined) cfg.logging.enabled = bool(env.XM2API_LOG, cfg.logging.enabled);
  if (env.XM2API_LOG_BODY !== undefined) cfg.logging.captureBody = bool(env.XM2API_LOG_BODY, cfg.logging.captureBody);

  if (env.XM2API_USE_OPENAI) cfg.client.useOpenAI = bool(env.XM2API_USE_OPENAI, cfg.client.useOpenAI);
  if (env.XM2API_PROXY) cfg.client.baseUrl = env.XM2API_PROXY;
  if (env.XM2API_MODEL) cfg.client.model = env.XM2API_MODEL;
  if (env.XM2API_MAX_TOKENS) cfg.client.maxTokens = Number(env.XM2API_MAX_TOKENS);

  // ---- 归一化 ----
  cfg.server.port = Number(cfg.server.port) || DEFAULTS.server.port;
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
