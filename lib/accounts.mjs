/**
 * 多账号凭证库：data/accounts/<user_id>.json
 *
 * 文件格式与 cpa 插件的 mimo.json 兼容（snake_case）：
 *   { type:"mimo", pass_token, user_id, c_user_id, service_token,
 *     sid, obtained_at, label, enabled, source }
 *
 * 职责：
 *   - 账号 CRUD（列表永远只回脱敏摘要，token 不出 API）；
 *   - 额度查询（GET 上游 /api/user/usage，Cookie 鉴权）；
 *   - service_token 过期（401）时用 pass_token 走 SSO 两阶段续期并落盘。
 *
 * 与转发的关系：一期转发仍用 data/sso-session.json 的单会话 routeCookieHeader；
 * 账号池的请求级轮询/故障切换是二期（接口已按多账号设计，届时直接接上）。
 */
import fs from "node:fs";
import path from "node:path";
import { config } from "./config.mjs";
import { DATA_DIR, readSsoSession } from "./chrome-cookie.mjs";
import { ensureCredentials, exchangeServiceToken, DEFAULT_SID } from "./pipeline.mjs";

export const ACCOUNTS_DIR = path.join(DATA_DIR, "accounts");

function ensureDir() {
  fs.mkdirSync(ACCOUNTS_DIR, { recursive: true });
}

function idOf(acc) {
  return String(acc?.user_id || acc?.id || "").trim();
}

function fileOf(id) {
  // 文件名只允许简单字符，防路径注入
  const safe = String(id).replace(/[^A-Za-z0-9_.@-]/g, "_");
  return path.join(ACCOUNTS_DIR, `${safe}.json`);
}

/** 对外摘要：永不带 pass_token / service_token 原文。 */
export function summaryOf(acc) {
  return {
    id: idOf(acc),
    label: acc.label || `mimo (${idOf(acc) || "?"})`,
    user_id: acc.user_id || "",
    c_user_id: acc.c_user_id || "",
    sid: acc.sid || DEFAULT_SID,
    enabled: acc.enabled !== false,
    has_pass_token: !!acc.pass_token,
    has_service_token: !!acc.service_token,
    service_token_len: acc.service_token ? String(acc.service_token).length : 0,
    obtained_at: acc.obtained_at || "",
    source: acc.source || "",
  };
}

export function readAccount(id) {
  try {
    const acc = JSON.parse(fs.readFileSync(fileOf(id), "utf8"));
    return idOf(acc) ? acc : null;
  } catch {
    return null;
  }
}

export function listAccounts() {
  ensureDir();
  const out = [];
  for (const name of fs.readdirSync(ACCOUNTS_DIR)) {
    if (!name.endsWith(".json")) continue;
    try {
      const acc = JSON.parse(fs.readFileSync(path.join(ACCOUNTS_DIR, name), "utf8"));
      if (idOf(acc)) out.push(summaryOf(acc));
    } catch {
      /* 坏文件跳过，不整体失败 */
    }
  }
  out.sort((a, b) => String(a.id).localeCompare(String(b.id)));
  return out;
}

export function saveAccount(acc) {
  const id = idOf(acc);
  if (!id) throw new Error("账号缺少 user_id");
  ensureDir();
  fs.writeFileSync(fileOf(id), JSON.stringify(acc, null, 2) + "\n", { mode: 0o600 });
  return summaryOf(acc);
}

export function removeAccount(id) {
  const f = fileOf(id);
  if (!fs.existsSync(f)) return false;
  fs.rmSync(f, { force: true });
  return true;
}

export function setAccountEnabled(id, enabled) {
  const acc = readAccount(id);
  if (!acc) return null;
  acc.enabled = !!enabled;
  return saveAccount(acc);
}

export function setAccountLabel(id, label) {
  const acc = readAccount(id);
  if (!acc) return null;
  acc.label = String(label || "");
  return saveAccount(acc);
}

/**
 * 导入一份 mimo.json（cpa 插件格式，也接受我们导出的格式）。
 * 校验最小字段：user_id +（pass_token 或 service_token 至少其一）。
 */
export function importAccount(json) {
  let acc = null;
  try {
    acc = typeof json === "string" ? JSON.parse(json) : json;
  } catch {
    return { ok: false, error: "不是合法 JSON" };
  }
  if (!acc || typeof acc !== "object" || Array.isArray(acc)) return { ok: false, error: "JSON 结构不对" };
  const id = idOf(acc);
  if (!id) return { ok: false, error: "缺少 user_id（mimo.json 的必填字段）" };
  if (!acc.pass_token && !acc.service_token) {
    return { ok: false, error: "pass_token / service_token 至少要有一个" };
  }
  const norm = {
    type: "mimo",
    user_id: id,
    c_user_id: acc.c_user_id || acc.cUserId || "",
    pass_token: acc.pass_token || acc.passToken || "",
    service_token: acc.service_token || acc.serviceToken || "",
    sid: acc.sid || config.credentials.sid || DEFAULT_SID,
    obtained_at: acc.obtained_at || new Date().toISOString(),
    label: acc.label || `mimo (${id})`,
    enabled: true,
    source: acc.source || "import",
  };
  return { ok: true, account: saveAccount(norm) };
}

/**
 * 本机一键提取：跑完整凭证链路（复制 Cookies → 读 passToken/userId → SSO 换
 * serviceToken → 写 sso-session.json），再把账号级凭证落进 data/accounts/。
 * 与菜单「11 获取账户凭证并保存」同源（都走 lib/pipeline.mjs）。
 */
export async function extractLocalAccount({ sid = config.credentials.sid || DEFAULT_SID } = {}) {
  const r = await ensureCredentials({ sid, force: true });
  if (!r.ok) return r;
  const sess = readSsoSession() || {};
  const tokens = sess.tokens || {};
  const route = String(sess.routeCookieHeader || "");
  const serviceToken = /serviceToken=([^;]+)/.exec(route)?.[1] || "";
  const userId = tokens.userId?.value || /userId=([^;]+)/.exec(route)?.[1] || "";
  if (!userId) return { ok: false, action: "extract", steps: r.steps, error: "链路成功但没解析到 userId" };
  const acc = {
    type: "mimo",
    user_id: String(userId),
    c_user_id: tokens.cUserId?.value || "",
    pass_token: tokens.passToken?.value || "",
    service_token: serviceToken,
    sid: sess.sso?.sid || sid,
    obtained_at: sess.sso?.obtained_at || new Date().toISOString(),
    label: `mimo (${userId})`,
    enabled: true,
    source: "local-extract",
  };
  return { ok: true, action: "extract", steps: r.steps, account: saveAccount(acc) };
}

/** 账号 → 上游 Cookie 头 */
export function routeCookie(acc) {
  const parts = [];
  if (acc.service_token) parts.push(`serviceToken=${acc.service_token}`);
  if (acc.user_id) parts.push(`userId=${acc.user_id}`);
  return parts.join("; ");
}

/** pass_token → SSO 两阶段换新 service_token 并落盘。 */
export async function renewAccount(acc) {
  if (!acc.pass_token) throw new Error("该账号没有 pass_token，无法自动续期");
  const account = {
    passToken: { value: acc.pass_token },
    userId: { value: acc.user_id },
  };
  if (acc.c_user_id) account.cUserId = { value: acc.c_user_id };
  const sso = await exchangeServiceToken(acc.sid || DEFAULT_SID, account);
  if (!sso.ok) throw new Error(sso.error || "SSO 续期失败");
  acc.service_token = sso.serviceToken;
  acc.obtained_at = new Date().toISOString();
  saveAccount(acc);
  return acc;
}

/**
 * 查额度：GET 上游 /api/user/usage（Cookie 鉴权）。
 * 401 且有 pass_token ⇒ 续期后重试一次（事件驱动，与 cpa 插件同思路）。
 * 返回 { percent, resetDate, resetAt, raw }；失败抛错（调用方按账号捕获）。
 */
export async function fetchUsage(acc, { renew = true } = {}) {
  const doFetch = () =>
    fetch(`${config.server.upstream}/api/user/usage`, {
      headers: { cookie: routeCookie(acc), accept: "application/json" },
      signal: AbortSignal.timeout(20000),
    });

  let res = await doFetch();
  if (res.status === 401 && renew && acc.pass_token) {
    await res.text().catch(() => "");
    await renewAccount(acc);
    res = await doFetch();
  }
  const text = await res.text();
  let j = null;
  try {
    j = JSON.parse(text);
  } catch {}
  if (res.status !== 200) throw new Error(`HTTP ${res.status}: ${text.slice(0, 160)}`);
  if (j?.code != null && j.code !== 0) throw new Error(`上游 code=${j.code} ${j.message || ""}`.trim());
  const d = j?.data ?? j ?? {};
  return {
    percent: typeof d.percent === "number" ? d.percent : null,
    resetDate: d.resetDate || "",
    resetAt: typeof d.resetAt === "number" ? d.resetAt : null,
    raw: d,
  };
}

/** 当前转发会话（sso-session.json）的额度 —— 面板「当前转发账号」一栏用。 */
export async function fetchSessionUsage() {
  const route = readSsoSession()?.routeCookieHeader;
  if (!route) return null;
  const fake = { service_token: /serviceToken=([^;]+)/.exec(route)?.[1] || "", user_id: /userId=([^;]+)/.exec(route)?.[1] || "" };
  return fetchUsage(fake, { renew: false });
}

export { ensureCredentials };
