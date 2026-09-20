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
import { SESSION_OUT } from "./creds/chrome-cookie.mjs";

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const PROJECT_ROOT = __dirname;

const PORT = Number(process.env.XM2API_PORT || 18787);
const HOST = process.env.XM2API_HOST || "127.0.0.1";
const LOG_DIR = path.join(PROJECT_ROOT, "logs");

// 可被 XM2API_MIMO_SERVER 覆盖（仅用于测试/指向自建上游；默认就是官方 route 主机）
const MIMO_SERVER = process.env.XM2API_MIMO_SERVER || "https://mimo-server-cn.xiaomimimo.com";

/** Models verified to be accepted by the SSO route. */
const ROUTE_MODELS = ["mimo-pro", "mimo-flash", "mimo-x-pro-preview", "mimo-x-flash-preview"];

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

const routeServer = createRouteServer({
  name: "xm2api 线路2 (SSO route)",
  port: PORT,
  host: HOST,
  routes: ROUTES,
  inject,
  logDir: LOG_DIR,
  logPrefix: "path2",
  local: [
    {
      method: "GET",
      path: "/v1/models",
      handler: (req, res) =>
        sendJson(res, 200, {
          object: "list",
          data: ROUTE_MODELS.map((id) => ({ id, object: "model", created: 1789000000, owned_by: "xiaomi" })),
        }),
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
      models: ROUTE_MODELS,
      credentials: session?.routeCookieHeader
        ? { present: true, sid: session.sso?.sid || null, obtainedAt: session.sso?.obtainedAt || null }
        : { present: false, fix: "node creds.mjs --refresh" },
      sessionFile: path.relative(PROJECT_ROOT, SESSION_OUT) || SESSION_OUT,
      hint: [
        `OpenAI base_url:   http://127.0.0.1:${PORT}/v1   (api_key 可填任意字符串)`,
        `Legacy path:       http://127.0.0.1:${PORT}/route/chat/completions`,
        `Meta:              http://127.0.0.1:${PORT}/__xm2api`,
        "401 → 刷新凭证:     node creds.mjs --refresh",
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
