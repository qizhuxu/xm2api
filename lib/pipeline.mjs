/**
 * 线路2 凭证流水线 —— 纯 Node 实现，供交互脚本（chat.mjs）与启动器直接调用。
 *
 * 整条链路：
 *   1) copyCookies      复制客户端 Chromium Cookies 库 + Local State
 *   2) extractAccount   读库取出 passToken / userId / cUserId
 *   3) exchangeToken    SSO 两阶段换 serviceToken（sid=mimopc）
 *   4) merge            写入 data/sso-session.json#routeCookieHeader
 *
 * 说明：当前客户端把 cookie 明文存在 `value` 列，所以 DPAPI/AES 那一步不是必需的。
 * 若将来遇到 v10/v11 密文，会提示需要先用 Windows DPAPI 解出 AES key。
 */
import fs from "node:fs";
import path from "node:path";
import crypto from "node:crypto";
import { DatabaseSync } from "node:sqlite";
import {
  DATA_DIR,
  COOKIES_COPY,
  KEY_PATH,
  LOCAL_STATE_COPY,
  SESSION_OUT,
  XIAOMI_COOKIES_SRC,
  XIAOMI_LS_SRC,
  decryptChromiumValue,
  ensureDir,
  loadAesKey,
  readSsoSession,
  writeSsoSession,
} from "./chrome-cookie.mjs";

const PHASE1_URL = "https://account.xiaomi.com/pass/serviceLogin";
const SSO_UA = "MiClaw/1.0";
export const DEFAULT_SID = "mimopc";

/* ------------------------------------------------------------------ 1) 复制 */

function sqliteHeaderOk(file) {
  try {
    return fs.readFileSync(file).subarray(0, 16).toString("latin1").startsWith("SQLite format 3");
  } catch {
    return false;
  }
}

/**
 * 复制 Chromium Cookies 库。
 *
 * ⚠️ 锁状态是**动态**的：客户端运行中，Chromium 有时允许其它进程读（可以直接复制），
 * 有时持有独占锁（Node/PowerShell/Python 三种方式都会 EBUSY / WinError 32）。
 * 因此这里：先重试几次（SQLite 锁常常是瞬时的），失败则回退到已有副本。
 *
 * @returns {{ok:boolean, method?:string, bytes?:number, warning?:string, error?:string}}
 */
export function copyCookies({ force = false, retries = 3, retryDelayMs = 500 } = {}) {
  if (!fs.existsSync(XIAOMI_COOKIES_SRC)) {
    return { ok: false, error: `源文件不存在: ${XIAOMI_COOKIES_SRC}` };
  }
  const haveUsableCopy = fs.existsSync(COOKIES_COPY) && sqliteHeaderOk(COOKIES_COPY);
  if (!force && haveUsableCopy) {
    return { ok: true, method: "reuse", bytes: fs.statSync(COOKIES_COPY).size };
  }

  ensureDir(DATA_DIR);
  const errors = [];
  for (let attempt = 1; attempt <= retries; attempt++) {
    const attempts = [
      ["copyFileSync", () => fs.copyFileSync(XIAOMI_COOKIES_SRC, COOKIES_COPY)],
      ["read/write", () => fs.writeFileSync(COOKIES_COPY, fs.readFileSync(XIAOMI_COOKIES_SRC))],
    ];
    for (const [method, fn] of attempts) {
      try {
        fn();
        if (!sqliteHeaderOk(COOKIES_COPY)) {
          errors.push(`${method}: 不是合法 SQLite`);
          continue;
        }
        try {
          if (fs.existsSync(XIAOMI_LS_SRC)) fs.copyFileSync(XIAOMI_LS_SRC, LOCAL_STATE_COPY);
        } catch {}
        return { ok: true, method: `${method}${attempt > 1 ? ` (第 ${attempt} 次)` : ""}`, bytes: fs.statSync(COOKIES_COPY).size };
      } catch (err) {
        errors.push(`${method}: ${err.code || err.message}`);
      }
    }
    if (attempt < retries) sleepSync(retryDelayMs);
  }

  // 复制失败：若已有可用副本，降级复用并给出警告
  if (haveUsableCopy) {
    return {
      ok: true,
      method: "stale-reuse",
      bytes: fs.statSync(COOKIES_COPY).size,
      warning:
        "cookie 库被客户端独占锁住，已回退复用上次的副本。" +
        "如需最新 cookie（例如刚在客户端重新登录过），请完全退出 Xiaomi MiMo（含托盘）后重试。",
    };
  }
  return {
    ok: false,
    error:
      `${errors.join(" | ")} —— cookie 库被锁。` +
      `请完全退出 Xiaomi MiMo（含托盘）后重试。`,
  };
}

/** 同步小睡（复制失败重试之间用；流水线本身是异步的，这里只为简单）。 */
function sleepSync(ms) {
  const sab = new SharedArrayBuffer(4);
  Atomics.wait(new Int32Array(sab), 0, 0, ms);
}

/* ------------------------------------------------------- 2) 从库里取账号信息 */

const WANTED = ["passToken", "userId", "cUserId"];

/**
 * 读 Cookies 库，取出 passToken / userId / cUserId。
 * 明文与 v10/v11 密文都能处理（密文需要 data/chrome-aes-key.bin）。
 */
export function extractAccount() {
  if (!fs.existsSync(COOKIES_COPY)) return { ok: false, error: `缺少 cookie 副本: ${COOKIES_COPY}` };
  let key = null;
  try {
    key = loadAesKey();
  } catch {
    key = null;
  }

  const db = new DatabaseSync(COOKIES_COPY, { readOnly: true });
  let rows;
  try {
    rows = db
      .prepare(
        `SELECT host_key, name, value, encrypted_value
         FROM cookies WHERE name IN (${WANTED.map(() => "?").join(",")})`
      )
      .all(...WANTED);
  } finally {
    db.close();
  }

  const found = {};
  let encryptedOnly = 0;
  for (const row of rows) {
    const enc = row.encrypted_value;
    const hasEnc = enc && enc.length;
    let value = row.value ? String(row.value) : "";
    if (!value && hasEnc) {
      if (!key) {
        encryptedOnly++;
        continue;
      }
      const buf = Buffer.isBuffer(enc) ? enc : Buffer.from(enc);
      const pt = decryptChromiumValue(buf, key);
      if (!pt) {
        encryptedOnly++;
        continue;
      }
      value = pt.toString("utf8");
    }
    if (!value) continue;
    // 同名的优先 xiaomi 域
    const host = row.host_key || "";
    const prev = found[row.name];
    if (!prev || (/xiaomi/.test(host) && !/xiaomi/.test(prev.host))) {
      found[row.name] = { value, host };
    }
  }

  if (encryptedOnly && !found.passToken) {
    return {
      ok: false,
      error:
        `cookie 是 v10/v11 密文，而本地没有解 key（缺 ${path.relative(DATA_DIR, KEY_PATH)}）。` +
        `当前客户端是明文存储，出现密文说明客户端换了存储方式：` +
        `需要用 Windows DPAPI 解开 Local State 里的 os_crypt.encrypted_key，` +
        `把 32 字节结果写到 ${path.relative(DATA_DIR, KEY_PATH)} 后重试。` +
        `注意 DPAPI 与本机 Windows 用户绑定，换机器/换用户都解不开。`,
    };
  }
  if (!found.passToken || !found.userId) {
    return { ok: false, error: "库里没有 passToken / userId —— 客户端可能未登录" };
  }
  return { ok: true, account: found, encryptedOnly };
}

/* -------------------------------------------------- 3) SSO 两阶段换 serviceToken */

function stripSsoPrefix(text) {
  return text.startsWith("&&&START&&&") ? text.slice("&&&START&&&".length) : text;
}

export async function exchangeServiceToken(sid, account, { timeoutMs = 20000 } = {}) {
  const url = new URL(PHASE1_URL);
  for (const [k, v] of Object.entries({ _locale: "zh_CN", _snsNone: "true", sid, _json: "true" })) {
    url.searchParams.append(k, v);
  }
  const cookie = [
    `passToken=${account.passToken.value}`,
    `userId=${account.userId.value}`,
    account.cUserId && `cUserId=${account.cUserId.value}`,
  ]
    .filter(Boolean)
    .join("; ");

  const res1 = await fetch(url, {
    headers: { Cookie: cookie, "User-Agent": SSO_UA },
    signal: AbortSignal.timeout(timeoutMs),
  });
  const text = stripSsoPrefix(await res1.text());
  let json = null;
  try {
    json = JSON.parse(text);
  } catch {}
  const nonce = (text.match(/"nonce"\s*:\s*(\d+)/) || [])[1] || String(json?.nonce ?? "");
  const code = json?.code ?? null;
  if (code !== 0) {
    return { ok: false, error: `Phase 1 失败 code=${code} ${json?.description || ""}`, phase1: { code } };
  }
  const loc = String(json.location || "");
  const ssecurity = String(json.ssecurity || "");
  if (!loc) return { ok: false, error: "Phase 1 未返回 location" };
  if (json.secondValidation && json.notificationUrl) {
    return { ok: false, error: `账号需要二次验证: ${json.notificationUrl}` };
  }

  let signInput = `nonce=${nonce}`;
  if (ssecurity && ssecurity.trim()) signInput += `&${ssecurity}`;
  const sha1 = crypto.createHash("sha1").update(signInput).digest();
  const clientSign = encodeURIComponent(sha1.toString("base64"));

  const res2 = await fetch(`${loc}&clientSign=${clientSign}`, {
    headers: { "User-Agent": SSO_UA },
    signal: AbortSignal.timeout(timeoutMs),
  });
  const setCookies = typeof res2.headers.getSetCookie === "function" ? res2.headers.getSetCookie() : [];
  const cookies = {};
  for (const raw of setCookies) {
    const pair = raw.split(";")[0].trim();
    const eq = pair.indexOf("=");
    if (eq > 0) cookies[pair.slice(0, eq).trim()] = pair.slice(eq + 1).trim();
  }
  const serviceToken = cookies.serviceToken || cookies[`${sid}_serviceToken`] || null;
  if (!serviceToken) {
    return {
      ok: false,
      error: `Phase 2 响应里没有 serviceToken（拿到: ${Object.keys(cookies).join(", ") || "无"}）`,
    };
  }
  return { ok: true, serviceToken, cookies, sid, httpStatus: res2.status };
}

/* --------------------------------------------------------------- 4) 合并落盘 */

export function mergeIntoSession({ sid, serviceToken, cookieNames }) {
  const session = readSsoSession();
  if (!session) return { ok: false, error: "缺少 sso-session.json —— 先执行 extractAccount 步骤" };
  const userId = session.tokens?.userId?.value;
  if (!userId) return { ok: false, error: "session 里没有 userId" };
  writeSsoSession({
    ...session,
    routeCookieHeader: `serviceToken=${serviceToken}; userId=${userId}`,
    sso: {
      sid,
      obtainedAt: new Date().toISOString(),
      serviceTokenLength: serviceToken.length,
      cookieNames: cookieNames || [],
    },
  });
  return { ok: true, userId };
}

/* ------------------------------------------------------------------ 编排 */

export function routeCookiePresent() {
  const c = readSsoSession()?.routeCookieHeader;
  return !!(c && c.includes("serviceToken=") && c.includes("userId="));
}

function writeAccountSession(found, encryptedOnly) {
  // 保留已有的 routeCookieHeader（若还有效），只更新 passToken 等
  const prev = readSsoSession() || {};
  const cookies = Object.entries(found).map(([name, v]) => ({
    host: v.host,
    name,
    value: v.value,
  }));
  writeSsoSession({
    ...prev,
    source: "chat-pipeline",
    cookieHeader: cookies.map((c) => `${c.name}=${c.value}`).join("; "),
    cookies,
    tokens: Object.fromEntries(Object.entries(found).map(([k, v]) => [k, { value: v.value, host: v.host }])),
    counts: { ...(prev.counts || {}), extracted: cookies.length, encryptedOnly: encryptedOnly || 0 },
  });
}

/**
 * 确保凭证可用：已有效则直接复用，否则跑完整链路。
 * @returns {Promise<{ok:boolean, action:string, steps:string[], error?:string}>}
 */
export async function ensureCredentials({ sid = DEFAULT_SID, force = false, log = () => {} } = {}) {
  const steps = [];
  if (!force && routeCookiePresent()) {
    return { ok: true, action: "reuse", steps: ["routeCookieHeader 已存在且格式有效"] };
  }

  log("复制 Cookies…");
  // force = 调用方要求刷新：尝试重新复制，被锁则自动回退到已有副本
  const copy = copyCookies({ force });
  if (!copy.ok) return { ok: false, action: "copy", steps, error: copy.error };
  steps.push(`复制 Cookies（${copy.method}, ${copy.bytes}B）`);
  if (copy.warning) {
    log(`⚠️ ${copy.warning}`);
    steps.push(`⚠️ ${copy.warning}`);
  }

  log("读取账号信息…");
  const acc = extractAccount();
  if (!acc.ok) return { ok: false, action: "extract", steps, error: acc.error };
  steps.push(`取出 ${Object.keys(acc.account).join(" / ")}`);
  writeAccountSession(acc.account, acc.encryptedOnly);

  log(`换 serviceToken（sid=${sid}）…`);
  const sso = await exchangeServiceToken(sid, acc.account);
  if (!sso.ok) return { ok: false, action: "sso", steps, error: sso.error };
  steps.push(`换到 serviceToken（${sso.serviceToken.length} 字符）`);

  const merged = mergeIntoSession({ sid, serviceToken: sso.serviceToken, cookieNames: Object.keys(sso.cookies) });
  if (!merged.ok) return { ok: false, action: "merge", steps, error: merged.error };
  steps.push("已写入 routeCookieHeader");
  return { ok: true, action: "refresh", steps };
}

/** 供交互脚本 /status 使用。 */
export function credentialsStatus() {
  const session = readSsoSession();
  return {
    cookieCopy: fs.existsSync(COOKIES_COPY),
    aesKey: fs.existsSync(KEY_PATH),
    hasPassToken: !!session?.tokens?.passToken,
    hasRouteCookie: routeCookiePresent(),
    sid: session?.sso?.sid || null,
    obtainedAt: session?.sso?.obtainedAt || null,
    sessionFile: SESSION_OUT,
  };
}
