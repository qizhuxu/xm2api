/**
 * 管理鉴权与安全护栏（管理 UI 的地基）。
 *
 * 两件事：
 *
 *   1) 管理 API 鉴权：/api/__admin/* 一律要求 X-Management-Key
 *      （或 Authorization: Bearer <key>）。密钥来源优先级：
 *        env XM2API_ADMIN_KEY > config.server.adminKey > data/admin-key.txt
 *      —— 末者在首启自动生成（32 字节随机、0600 权限），并在启动日志打印一次位置。
 *
 *   2) 全端点安全护栏（修掉安全分析的头号风险：旧版对所有响应发
 *      `Access-Control-Allow-Origin: *`，任意网页都能拿着用户的 MiMo 额度白嫖）：
 *      - **Host 校验**（防 DNS rebinding）：Host 必须指向本服务自己
 *        （127.0.0.1 / localhost / [::1] + server.allowedHosts），否则 421；
 *      - **Origin 校验**（防跨站调用）：带 Origin 且非同源/白名单的浏览器请求
 *        直接 403 —— 任意网页 JS 从此调不动本服务；SDK / curl / Postman 不带
 *        Origin，完全不受影响；
 *      - **CORS 只回给放行的 Origin**，不再发 `ACAO: *`。同源页面（/ui/ 管理界面）
 *        浏览器根本不需要 CORS 头也能正常工作。
 *
 * 逃生口：自建域名/远程访问在 config.yaml 的 server.allowedHosts / server.allowedOrigins
 * 加白（配 0.0.0.0 对外开放前，请务必同时给管理 API 换强密钥）。
 */
import fs from "node:fs";
import path from "node:path";
import crypto from "node:crypto";
import { config } from "./config.mjs";
import { DATA_DIR } from "./chrome-cookie.mjs";

export const ADMIN_KEY_FILE = path.join(DATA_DIR, "admin-key.txt");

let keyCache = null;

/** 管理密钥（进程级缓存）。首次使用时自动生成并落盘。 */
export function adminKey() {
  if (keyCache) return keyCache;
  const fromEnv = String(process.env.XM2API_ADMIN_KEY || "").trim();
  if (fromEnv) return (keyCache = fromEnv);
  const fromCfg = String(config.server.adminKey || "").trim();
  if (fromCfg) return (keyCache = fromCfg);
  try {
    const k = fs.readFileSync(ADMIN_KEY_FILE, "utf8").trim();
    if (k) return (keyCache = k);
  } catch {}
  const k = "xm2api-" + crypto.randomBytes(24).toString("base64url");
  fs.mkdirSync(path.dirname(ADMIN_KEY_FILE), { recursive: true });
  fs.writeFileSync(ADMIN_KEY_FILE, k + "\n", { mode: 0o600 });
  console.log(`[admin] 已生成管理密钥 → ${path.relative(process.cwd(), ADMIN_KEY_FILE)}（管理界面登录 / X-Management-Key 请求头用）`);
  return (keyCache = k);
}

function safeEqual(a, b) {
  const ab = Buffer.from(String(a));
  const bb = Buffer.from(String(b));
  return ab.length === bb.length && crypto.timingSafeEqual(ab, bb);
}

/** 管理 API 鉴权：X-Management-Key 或 Authorization: Bearer。 */
export function checkAdmin(req) {
  const want = adminKey();
  const h = req.headers || {};
  let got = String(h["x-management-key"] || "").trim();
  if (!got) {
    const m = /^Bearer\s+(.+)$/i.exec(String(h.authorization || ""));
    if (m) got = m[1].trim();
  }
  return !!got && safeEqual(got, want);
}

function selfHosts() {
  const p = config.server.port;
  return new Set([
    `127.0.0.1:${p}`, `localhost:${p}`, `[::1]:${p}`,
    ...(config.server.allowedHosts || []).map((x) => String(x).toLowerCase()),
  ]);
}

function selfOrigins() {
  const p = config.server.port;
  return new Set([
    `http://127.0.0.1:${p}`, `http://localhost:${p}`, `http://[::1]:${p}`,
    ...(config.server.allowedOrigins || []).map((x) => String(x).replace(/\/+$/, "")),
  ]);
}

/**
 * 每请求安全护栏。返回：
 *   { reject?: {status, body}, corsOrigin?: string|null }
 * reject 存在 ⇒ 调用方直接应答；corsOrigin 非空 ⇒ 给响应回这个 ACAO。
 */
export function requestGuard(req) {
  // ① Host 校验（防 DNS rebinding：恶意域名解析到 127.0.0.1 也过不了这关）
  const host = String(req.headers.host || "").toLowerCase();
  if (host && !selfHosts().has(host)) {
    return {
      reject: {
        status: 421,
        body: {
          error: {
            message: `Host 不被信任：${host}（防 DNS rebinding；要用自建域名请加 server.allowedHosts）`,
            type: "invalid_request_error",
            code: "untrusted_host",
          },
        },
      },
    };
  }
  // ② Origin 校验（防跨站：任意网页 fetch 本服务白用额度的老路到此为止）
  const origin = String(req.headers.origin || "").replace(/\/+$/, "");
  if (origin) {
    if (!selfOrigins().has(origin)) {
      return {
        reject: {
          status: 403,
          body: {
            error: {
              message: `跨站请求被拒绝：Origin ${origin}（同源的 /ui/ 管理界面不受影响；确需跨站请加 server.allowedOrigins）`,
              type: "invalid_request_error",
              code: "origin_forbidden",
            },
          },
        },
      };
    }
    return { corsOrigin: origin };
  }
  return { corsOrigin: null };
}
