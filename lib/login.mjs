/**
 * 小米账号在线登录（三期）：扫码 + 密码 + 新设备 OTP。
 *
 * 流程逐字移植自 cpa-plugin/login.go（逆向实测的权威版本）与 test/login 脚本：
 *
 *   扫码   GET /longPolling/loginUrl?sid&callback&qs&_qrsize=480
 *          → { loginUrl, qr(可直接 <img src> 的 data-URL), lp(长轮询), timeout }
 *          → 后台轮询 lp：响应里全树扫描 passToken/userId/cUserId（各入口包裹
 *            层级不一致，不能赌形状）；出现 notificationUrl = 风控/二次验证。
 *   密码   ① /pass/serviceLogin 取 qs/_sign/callback
 *          ② POST /pass/serviceLoginAuth2（hash=md5Upper(password)，带
 *            Referer/Origin）→ location=直接成功；notificationUrl=新设备保护→OTP；
 *            其余=错误（captchaUrl / secondValidation 等风控信号如实返回）
 *          ③ GET location（≤2 跳、仅 *.xiaomi.com，防开放重定向）从
 *            Set-Cookie / JSON 全树 / URL query 三处收 passToken/userId/cUserId
 *   OTP    notificationUrl → GET 验证会话（cookie 进 jar）→ /identity/list 查
 *          验证方式（flag 4=短信 8=邮箱 + 掩码文案）→ verify{Phone,Email} 触发
 *          → **POST send{Phone,Email}Ticket**（必须带 X-Requested-With，否则恒 66108）
 *          → 用户提交 ticket → verify 提交（trust=true）→ GET location →
 *          重跑 serviceLogin（jar 已认证）→ 收凭证
 *
 * 安全：password / 验证码只在本模块内存中流转 —— 不打日志、不进错误消息、不落盘。
 * 登录成功即写 data/accounts/（与 mimo.json 兼容），并尽力补一个 service_token。
 */
import crypto from "node:crypto";
import { config } from "./config.mjs";
import { importAccount, renewAccount } from "./accounts.mjs";

/* 运行时取基址：XM2API_LOGIN_PASSPORT 仅供测试指到 mock（生产恒为小米 passport）。 */
const passBase = () => process.env.XM2API_LOGIN_PASSPORT || "https://account.xiaomi.com";
const SSO_UA = "MiClaw/1.0";
const CALLBACK_FALLBACK = "https://mimo-server-cn.xiaomimimo.com/api/sts";

/* ------------------------------------------------------------ 小工具 */

const stripXSSI = (t) => (String(t).startsWith("&&&START&&&") ? String(t).slice(11) : String(t));
const md5Upper = (s) => crypto.createHash("md5").update(s, "utf8").digest("hex").toUpperCase();

function parseJSON(raw) {
  try {
    return JSON.parse(stripXSSI(raw));
  } catch {
    return null;
  }
}

/** 递归全树扫描凭证（userId 兼容 string/number —— 真实响应里是数字）。 */
function findTokens(raw) {
  const root = parseJSON(raw);
  if (!root) return null;
  const out = { passToken: "", userId: "", cUserId: "" };
  const grab = (k, v) => {
    const key = String(k).toLowerCase();
    if (typeof v !== "string" && typeof v !== "number") return;
    const s = String(v);
    if (!s) return;
    if (key === "passtoken" && !out.passToken) out.passToken = s;
    if (key === "userid" && !out.userId) out.userId = s;
    if (key === "cuserid" && !out.cUserId) out.cUserId = s;
  };
  const walk = (v) => {
    if (Array.isArray(v)) return v.forEach(walk);
    if (v && typeof v === "object") {
      for (const [k, x] of Object.entries(v)) {
        grab(k, x);
        walk(x);
      }
    }
  };
  walk(root);
  return out.passToken && out.userId ? out : null;
}

/** 极简 cookie jar（同一流程内保持会话：OTP 全靠它）。 */
function newJar() {
  return { cookies: {} };
}
function jarHeader(jar) {
  return Object.entries(jar.cookies).map(([k, v]) => `${k}=${v}`).join("; ");
}
function jarAbsorb(jar, res) {
  for (const c of typeof res.headers.getSetCookie === "function" ? res.headers.getSetCookie() : []) {
    const pair = c.split(";")[0].trim();
    const eq = pair.indexOf("=");
    if (eq > 0) jar.cookies[pair.slice(0, eq).trim()] = pair.slice(eq + 1).trim();
  }
}

async function ssoFetch(jar, url, { method = "GET", body, headers = {}, redirect = "manual", signal, timeoutMs = 30000 } = {}) {
  const h = { "User-Agent": SSO_UA, ...headers };
  if (jar && Object.keys(jar.cookies).length) h.Cookie = jarHeader(jar);
  const res = await fetch(url, {
    method,
    headers: h,
    body,
    redirect,
    signal: signal || AbortSignal.timeout(timeoutMs),
  });
  if (jar) jarAbsorb(jar, res);
  return res;
}

function absURL(maybe, base = passBase()) {
  const s = String(maybe || "").trim();
  if (!s) return "";
  if (/^https?:/i.test(s)) return s;
  return base + (s.startsWith("/") ? s : "/" + s);
}

/* ------------------------------------------------------------ 会话存储 */

const qrSessions = new Map(); // id -> { id, status, message, qr, timeout, createdAt, tokens }
const otpStates = new Map(); // id -> { id, jar, method, flag, notify, sid, sentAt, createdAt }

let seq = 0;
const newId = (p) => `${p}-${Date.now().toString(36)}-${(++seq).toString(36)}`;

/* ------------------------------------------------------------ 扫码登录 */

/** 创建扫码会话并立刻开始后台长轮询（对齐 login.go 的 pollLoginSession）。 */
export async function qrStart(sid = config.credentials.sid) {
  const qs = `?sid=${sid}&_json=true`;
  const cb = CALLBACK_FALLBACK;
  const url = `${passBase()}/longPolling/loginUrl?sid=${encodeURIComponent(sid)}&callback=${encodeURIComponent(cb)}&qs=${encodeURIComponent(qs)}&_qrsize=480`;
  const res = await ssoFetch(null, url);
  const j = parseJSON(await res.text()) || {};
  if (j.code !== 0 || !j.lp) {
    return { ok: false, error: `小米拒绝创建扫码会话 code=${j.code ?? "?"} ${j.desc || ""}`.trim() };
  }
  const s = {
    id: newId("qr"),
    status: "pending",
    message: `等待扫码（二维码 ${Math.round((j.timeout || 300) / 60)} 分钟内有效）`,
    qr: j.qr || "",
    lp: j.lp || "",
    loginUrl: j.loginUrl || "",
    timeout: j.timeout > 0 ? j.timeout : 300,
    createdAt: Date.now(),
    tokens: null,
    abort: new AbortController(),
  };
  qrSessions.set(s.id, s);
  pollQR(s).catch(() => {});
  return { ok: true, id: s.id, qr: s.qr, timeout: s.timeout, message: s.message };
}

/** 取消扫码会话（停掉后台长轮询；页面切换/关闭时可用）。 */
export function qrAbort(id) {
  const s = qrSessions.get(String(id || ""));
  if (!s) return false;
  try {
    s.abort?.abort();
  } catch {}
  if (s.status === "pending") {
    s.status = "cancelled";
    s.message = "已取消";
  }
  return true;
}

async function pollQR(s) {
  const deadline = s.createdAt + (s.timeout + 30) * 1000;
  while (Date.now() < deadline && s.status === "pending") {
    try {
      const res = await ssoFetch(null, absURL(s.lp), {
        redirect: "manual",
        timeoutMs: 330000, // 长轮询：服务器会吊住连接，30s 默认超时会误杀
        signal: s.abort?.signal,
      });
      const body = await res.text();
      const tok = findTokens(body);
      if (tok) {
        s.tokens = tok;
        s.status = "confirmed";
        s.message = "扫码确认成功，凭证已获取";
        finishLogin(s, tok, "login-qr");
        return;
      }
      if (body.includes("notificationUrl")) {
        s.status = "failed";
        s.message = "该账号需要二次验证/风控拦截：请改用账号密码登录，或在手机上完成验证后重试";
        return;
      }
      // 未扫码/未确认：继续长轮询
    } catch {
      if (s.abort?.signal?.aborted || s.status !== "pending") return;
      await new Promise((r) => setTimeout(r, 2000));
    }
  }
  if (s.status === "pending") {
    s.status = "timeout";
    s.message = "二维码超时未确认，请重新发起登录";
  }
}

export function qrStatus(id) {
  const s = qrSessions.get(String(id || ""));
  if (!s) return { ok: false, error: "会话不存在或已过期" };
  return {
    ok: true,
    id: s.id,
    status: s.status,
    message: s.message,
    qr: s.status === "pending" ? s.qr : undefined,
    account: s.account || undefined,
  };
}

/* ------------------------------------------------------------ 落盘 */

async function finishLogin(sessionLike, tok, source) {
  const r = importAccount({
    type: "mimo",
    user_id: tok.userId,
    pass_token: tok.passToken,
    c_user_id: tok.cUserId || "",
    source,
  });
  if (r.ok) {
    sessionLike.account = r.account;
    // 尽力补 service_token（池立即可用）；失败不影响登录成功（401 续期会救）
    try {
      const acc = (await import("./accounts.mjs")).readAccount(r.account.id);
      if (acc) await renewAccount(acc);
    } catch {}
  }
  return r;
}

/* ------------------------------------------------------------ 密码登录 */

/**
 * 账号密码登录。返回：
 *   { ok:true, status:"ok", account }
 * | { ok:true, status:"awaiting-otp", id, notify }   ← 验证码已发出
 * | { ok:false, status:"error", code, message, captchaUrl?, secondValidation? }
 */
export async function passwordLogin({ user, password, sid = config.credentials.sid }) {
  user = String(user || "").trim();
  if (!user || !password) return { ok: false, status: "error", code: -1, message: "账号和密码都不能为空" };

  // 阶段 0：取 qs / _sign / callback
  const jar = newJar();
  const p0res = await ssoFetch(jar, `${passBase()}/pass/serviceLogin?sid=${encodeURIComponent(sid)}&_json=true&_locale=zh_CN`);
  const p0 = parseJSON(await p0res.text()) || {};
  const callback = String(p0.callback || CALLBACK_FALLBACK).trim() || CALLBACK_FALLBACK;

  // 阶段 1：serviceLoginAuth2
  const form = new URLSearchParams({
    sid,
    callback,
    qs: String(p0.qs || "").trim() || `?sid=${sid}&_json=true`,
    user,
    hash: md5Upper(String(password)), // 密码只在内存：只出 hash
    _json: "true",
    _locale: "zh_CN",
  });
  if (String(p0._sign || "").trim()) form.set("_sign", p0._sign);
  const a2res = await ssoFetch(jar, `${passBase()}/pass/serviceLoginAuth2`, {
    method: "POST",
    body: form.toString(),
    headers: {
      "Content-Type": "application/x-www-form-urlencoded",
      Referer: `${passBase()}/fe/service/login?sid=${encodeURIComponent(sid)}`,
      Origin: passBase(),
    },
    redirect: "manual",
  });
  const a2 = parseJSON(await a2res.text()) || {};
  const desc = a2.desc || a2.description || "";

  if (String(a2.notificationUrl || "").trim()) {
    // 新设备保护：自动发起验证码，等用户输入
    try {
      const otp = await startOTP(a2.notificationUrl, sid, jar);
      return { ok: true, status: "awaiting-otp", id: otp.id, notify: otp.notify, method: otp.method };
    } catch (e) {
      return { ok: false, status: "error", code: a2.code ?? -1, message: "需要新设备验证，但自动发码失败：" + e.message };
    }
  }
  if (a2.code !== 0 || !String(a2.location || "").trim()) {
    return {
      ok: false,
      status: "error",
      code: a2.code ?? -1,
      message: desc || "登录失败",
      captchaUrl: a2.captchaUrl || undefined,
      secondValidation: !!a2.secondValidation,
    };
  }

  // 阶段 2：location 收凭证（≤2 跳、仅 *.xiaomi.com）
  const tok = await collectTokensFromLocation(a2.location, jar);
  if (!tok.ok) return { ok: false, status: "error", code: a2.code ?? -1, message: tok.error };
  const saved = await finishLogin({ tokens: tok.tokens }, tok.tokens, "login-password");
  return { ok: true, status: "ok", account: saved.account };
}

async function collectTokensFromLocation(location, jar) {
  let next = absURL(location);
  for (let hop = 0; hop < 2 && next; hop++) {
    const res = await ssoFetch(jar, next, { redirect: "manual" });
    const body = await res.text();
    const out = { passToken: "", userId: "", cUserId: "" };
    // 1) Set-Cookie  2) body 全树  3) URL query —— 三处收割
    for (const [k, v] of Object.entries(jar.cookies)) {
      const lk = k.toLowerCase();
      if (lk === "passtoken" && !out.passToken) out.passToken = v;
      if (lk === "userid" && !out.userId) out.userId = v;
      if (lk === "cuserid" && !out.cUserId) out.cUserId = v;
    }
    const t = findTokens(body);
    if (t) {
      out.passToken ||= t.passToken;
      out.userId ||= t.userId;
      out.cUserId ||= t.cUserId;
    }
    try {
      const q = new URL(next).searchParams;
      out.passToken ||= q.get("passToken") || "";
      out.userId ||= q.get("userId") || "";
      out.cUserId ||= q.get("cUserId") || "";
    } catch {}
    if (out.passToken && out.userId) return { ok: true, tokens: out };

    const loc = String(res.headers.get("location") || parseJSON(body)?.location || "").trim();
    if (!loc) break;
    const abs = new URL(absURL(loc, next));
    if (!/(^|\.)xiaomi\.com$/i.test(abs.hostname)) break; // 防开放重定向
    next = abs.toString();
  }
  return { ok: false, error: "登录跳转完成后仍未拿到 passToken（风控或流程变更），请改用扫码登录或本机提取" };
}

/* ------------------------------------------------------------ 新设备 OTP */

/**
 * 发起验证并**立刻发码**。返回 { id, method, flag, notify }。
 * notify = 验证方式掩码文案（如 "a***@qq.com" / "188****1234"）。
 */
export async function startOTP(notificationUrl, sid, jar = newJar()) {
  const ntf = absURL(notificationUrl);
  const u = new URL(ntf);
  const ctx = u.searchParams.get("context") || "";
  const sidParam = u.searchParams.get("sid") || sid;

  // Step 1: 打开验证会话（cookie 进 jar）
  await ssoFetch(jar, ntf);

  // Step 2: identity/list 查验证方式（flag 4=短信 8=邮箱）
  const listRes = await ssoFetch(jar,
    `${passBase()}/identity/list?sid=${encodeURIComponent(sidParam)}&supportedMask=0&_locale=zh_CN&context=${encodeURIComponent(ctx)}`);
  let list = parseJSON(await listRes.text()) || {};
  if (list.data && typeof list.data === "object" && list.flag === undefined) list = { ...list, ...list.data };
  let flag = 4;
  if (Number(list.flag) === 8) flag = 8;
  const method = flag === 8 ? "Email" : "Phone";
  const notify =
    ["notify", "notifyPhone", "notifyEmail", "maskedPhone", "maskedEmail", "phone", "email", "tips", "description", "desc"]
      .map((k) => list[k]).find((v) => typeof v === "string" && v) || "";

  // Step 3: verify{Phone,Email} 触发验证会话
  const trigRes = await ssoFetch(jar, `${passBase()}/identity/auth/verify${method}?_flag=${flag}&_json=true`);
  const trig = parseJSON(await trigRes.text());
  if (trig && trig.code !== undefined && trig.code !== -1 && trig.code !== 0) {
    throw new Error(`触发验证码失败 code=${trig.code} ${trig.desc || ""}`);
  }

  // Step 4: 真正发码 —— send{Phone,Email}Ticket，body 与短信一致；
  // ⚠️ 必须带 X-Requested-With，否则邮箱发码恒 66108（2026-09-23 抓包实锤）
  const sendRes = await ssoFetch(jar, `${passBase()}/identity/auth/send${method}Ticket?_dc=${Date.now()}`, {
    method: "POST",
    body: new URLSearchParams({ retry: "0", icode: "", _json: "true" }).toString(),
    headers: {
      "Content-Type": "application/x-www-form-urlencoded",
      "X-Requested-With": "XMLHttpRequest",
    },
  });
  const sm = parseJSON(await sendRes.text());
  if (sm && sm.code !== undefined && sm.code !== -1 && sm.code !== 0) {
    throw new Error(`发送验证码失败 code=${sm.code} ${sm.desc || ""}`);
  }

  const otp = {
    id: newId("otp"),
    jar,
    method,
    flag,
    notify,
    sid: sidParam,
    sentAt: Date.now(),
    createdAt: Date.now(),
  };
  otpStates.set(otp.id, otp);
  return otp;
}

/** 提交验证码完成登录。code 只在内存中流转。 */
export async function otpSubmit(id, code) {
  const otp = otpStates.get(String(id || ""));
  if (!otp) return { ok: false, status: "error", message: "会话不存在或已过期" };
  code = String(code || "").trim();
  if (!code) return { ok: false, status: "error", message: "验证码不能为空" };

  // Step 6: verify{Phone,Email} 提交 ticket（trust=true 降低再次触发频率）
  const vRes = await ssoFetch(otp.jar, `${passBase()}/identity/auth/verify${otp.method}?_dc=${Date.now()}`, {
    method: "POST",
    body: new URLSearchParams({ _flag: String(otp.flag), ticket: code, trust: "true", _json: "true" }).toString(),
    headers: {
      "Content-Type": "application/x-www-form-urlencoded",
      "X-Requested-With": "XMLHttpRequest",
    },
  });
  const vm = parseJSON(await vRes.text()) || {};
  if (String(vm.location || "").trim()) {
    // Step 7: GET location（jar 收认证 cookie）
    await ssoFetch(otp.jar, absURL(vm.location), { redirect: "manual" });
  } else if (vm.code !== undefined && vm.code !== 0) {
    return { ok: false, status: "error", message: `验证码不正确或已过期 code=${vm.code} ${vm.desc || ""}` };
  }

  // Step 8: 重跑 serviceLogin（jar 已认证）→ 收完整凭证
  const pRes = await ssoFetch(otp.jar,
    `${passBase()}/pass/serviceLogin?sid=${encodeURIComponent(otp.sid)}&_json=true&_locale=zh_CN`);
  const body = await pRes.text();
  let tok = findTokens(body);
  const pm = parseJSON(body) || {};
  if (!tok && String(pm.location || "").trim()) {
    const t2 = await collectTokensFromLocation(pm.location, otp.jar);
    if (t2.ok) tok = t2.tokens;
  }
  if (!tok) return { ok: false, status: "error", message: "验证通过但小米未返回 passToken，请改用扫码登录" };

  const saved = await finishLogin({ tokens: tok }, tok, "login-otp");
  otpStates.delete(otp.id);
  return { ok: true, status: "ok", account: saved.account };
}

export function otpStatus(id) {
  const otp = otpStates.get(String(id || ""));
  if (!otp) return { ok: false, error: "会话不存在或已过期" };
  return { ok: true, id: otp.id, method: otp.method, notify: otp.notify };
}
