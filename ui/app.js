/* xm2api 管理界面（零框架）。API 基址同源 /api/__admin，密钥走 X-Management-Key。 */
"use strict";

const $ = (s) => document.querySelector(s);
const $$ = (s) => [...document.querySelectorAll(s)];
const esc = (s) => String(s ?? "").replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
const KEY_STORE = "xm2api.mgmtKey";
const POLL_STORE = "xm2api.pollSec";

let pollTimer = null;

/* ------------------------------------------------ API */

function apiKey() {
  return sessionStorage.getItem(KEY_STORE) || "";
}

async function api(method, path, body) {
  const res = await fetch("/api/__admin" + path, {
    method,
    headers: {
      "X-Management-Key": apiKey(),
      ...(body !== undefined ? { "content-type": "application/json" } : {}),
    },
    body: body !== undefined ? (typeof body === "string" ? body : JSON.stringify(body)) : undefined,
  });
  let j = null;
  try {
    j = await res.json();
  } catch {}
  if (res.status === 401) {
    showLogin();
    throw new Error("管理密钥无效或缺失");
  }
  if (!res.ok) throw new Error(j?.error?.message || `HTTP ${res.status}`);
  return j;
}

/* ------------------------------------------------ 登录 */

function showLogin() {
  $("#login").classList.remove("hidden");
}
function hideLogin() {
  $("#login").classList.add("hidden");
}

async function tryLogin(key) {
  sessionStorage.setItem(KEY_STORE, key);
  try {
    await api("GET", "/status");
    hideLogin();
    boot();
    return true;
  } catch (e) {
    sessionStorage.removeItem(KEY_STORE);
    $("#login-err").textContent = e.message;
    return false;
  }
}

/* ------------------------------------------------ 状态页 */

function kv(rows) {
  return `<div class="kv">${rows.map(([k, v]) => `<div>${esc(k)}</div><div>${v}</div>`).join("")}</div>`;
}

async function renderStatus() {
  const s = await api("GET", "/status");
  const sess = s.session?.present
    ? `<span class="ok">✅ 就绪</span> <span class="dim">sid=${esc(s.session.sid || "-")} · ${esc((s.session.obtainedAt || "").slice(0, 19).replace("T", " "))}</span>`
    : `<span class="bad">❌ 缺失</span> <span class="dim">${esc(s.session?.fix || "")}</span>`;
  $("#status-cards").innerHTML = `
    <div class="card"><h3>服务</h3>${kv([
      ["监听", `<code>${esc(s.host)}:${esc(s.port)}</code>`],
      ["上游", `<code>${esc(s.upstream)}</code>`],
      ["管理界面", `<code>${esc(s.ui)}</code>`],
      ["配置文件", `<code>${esc(s.config.file)}</code>`],
    ])}</div>
    <div class="card"><h3>转发会话（sso-session）</h3>${kv([
      ["routeCookie", sess],
      ["cookie 库副本", s.credentials?.cookieCopy ? "有" : "无"],
      ["passToken 缓存", s.credentials?.hasPassToken ? "有" : "无"],
      ["sid / 换取时间", `${esc(s.credentials?.sid || "-")} · ${esc((s.credentials?.obtainedAt || "").slice(0, 19).replace("T", " "))}`],
    ])}</div>
    <div class="card"><h3>账号库（data/accounts）</h3>${kv([
      ["账号数", `<b>${esc(s.accounts)}</b> 个`],
      ["日志", s.logging?.enabled ? `开（body ${s.logging.captureBody ? "记录" : "不记录"}）` : "关"],
    ])}</div>
    <div class="card"><h3>安全</h3>${kv([
      ["管理密钥文件", `<code>${esc(s.security.admin_key_file)}</code>`],
      ["Host 校验", esc(s.security.host_check)],
      ["Origin 校验", esc(s.security.origin_check)],
    ])}</div>`;
}

/* ------------------------------------------------ 凭证页 */

async function renderAccounts() {
  const { accounts } = await api("GET", "/accounts");
  const tb = $("#accounts-table tbody");
  if (!accounts.length) {
    tb.innerHTML = `<tr><td colspan="6" class="dim">还没有账号 —— 用上面的按钮提取或导入</td></tr>`;
    return;
  }
  tb.innerHTML = accounts
    .map((a) => {
      const creds = [];
      creds.push(a.has_pass_token ? '<span class="ok">passToken ✓</span>' : '<span class="dim">passToken ✗</span>');
      creds.push(a.has_service_token ? `<span class="ok">serviceToken ✓(${esc(a.service_token_len)}B)</span>` : '<span class="dim">serviceToken ✗</span>');
      const s = a.stats || {};
      const cooling = (s.cooldownUntil || 0) > Date.now();
      const poolStat =
        `<span class="ok">✓${esc(s.success || 0)}</span> <span class="${s.failed ? "bad" : "dim"}">✗${esc(s.failed || 0)}</span>` +
        (s.renewCount ? `<div class="dim">续期 ×${esc(s.renewCount)}</div>` : "") +
        (cooling ? '<div class="bad">冷却中…</div>' : "");
      return `<tr data-id="${esc(a.id)}">
        <td><b>${esc(a.label)}</b><div class="dim mono">${esc(a.user_id)}</div></td>
        <td>${creds.join("<br>")}</td>
        <td class="dim">${esc(a.source || "-")}<div class="dim">${esc((a.obtained_at || "").slice(0, 19).replace("T", " "))}</div></td>
        <td>${poolStat}</td>
        <td>${a.enabled ? '<span class="badge on">启用</span>' : '<span class="badge off">禁用</span>'}</td>
        <td>
          <button data-act="toggle">${a.enabled ? "禁用" : "启用"}</button>
          <button data-act="del" class="danger">删除</button>
        </td>
      </tr>`;
    })
    .join("");
}

$("#accounts-table").addEventListener("click", async (ev) => {
  const btn = ev.target.closest("button[data-act]");
  if (!btn) return;
  const id = btn.closest("tr").dataset.id;
  try {
    if (btn.dataset.act === "toggle") {
      const on = btn.textContent.trim() === "启用";
      await api("PATCH", `/accounts/${encodeURIComponent(id)}`, { enabled: on });
    } else if (btn.dataset.act === "del") {
      if (!confirm(`删除账号 ${id}？（凭证文件将从 data/accounts 移除）`)) return;
      await api("DELETE", `/accounts/${encodeURIComponent(id)}`);
    }
    await renderAccounts();
  } catch (e) {
    alert("操作失败：" + e.message);
  }
});

$("#btn-extract").addEventListener("click", async () => {
  const out = $("#extract-out");
  out.classList.remove("hidden");
  out.textContent = "提取中（复制 Cookies → 读凭证 → SSO 换 token → 落盘）…";
  $("#btn-extract").disabled = true;
  try {
    const r = await api("POST", "/accounts/extract");
    out.textContent = (r.steps || []).map((s) => "· " + s).join("\n") + `\n✅ 已保存：${r.account?.label}`;
    await renderAccounts();
  } catch (e) {
    out.textContent = "❌ " + e.message;
  } finally {
    $("#btn-extract").disabled = false;
  }
});

$("#import-file").addEventListener("change", async (ev) => {
  const f = ev.target.files?.[0];
  if (f) $("#import-text").value = await f.text();
});

$("#btn-import").addEventListener("click", async () => {
  const raw = $("#import-text").value.trim();
  if (!raw) return;
  try {
    const r = await api("POST", "/accounts/import", raw);
    $("#import-out").textContent = `✅ 已导入 ${r.account?.label}`;
    $("#import-text").value = "";
    await renderAccounts();
  } catch (e) {
    $("#import-out").textContent = "❌ " + e.message;
  }
});

/* ------------------------------------------------ 额度页 */

function quotaCard(title, r) {
  if (!r) return "";
  if (!r.ok) {
    return `<div class="card"><h3>${esc(title)}</h3><div class="bad">额度获取失败：${esc(r.error)}</div></div>`;
  }
  const pct = typeof r.percent === "number" ? r.percent : null;
  const cls = pct === null ? "qmid" : pct >= 70 ? "qhigh" : pct >= 30 ? "qmid" : "qlow";
  const w = pct === null ? 0 : Math.max(0, Math.min(100, pct));
  return `<div class="card"><h3>${esc(title)}</h3>
    ${kv([
      ["剩余用量", pct === null ? "—" : (pct >= 99.95 ? '<span class="ok">额度可用</span>' : `<b>${Math.round(pct)}%</b>`)],
      ["重置", esc(r.resetDate || (r.resetAt ? new Date(r.resetAt * 1000).toLocaleString() : "—"))],
    ])}
    <div class="qbar"><i class="${cls}" style="width:${w}%"></i></div>
  </div>`;
}

async function renderQuota() {
  const q = await api("GET", "/quota");
  $("#quota-observed").textContent = "更新于 " + (q.observed_at || "").slice(11, 19);
  const parts = [];
  if (q.session) parts.push(quotaCard("当前转发会话（sso-session）", q.session));
  for (const a of q.accounts || []) parts.push(quotaCard(a.label || a.id, a));
  $("#quota-cards").innerHTML = parts.join("") || '<div class="card dim">还没有可查的账号</div>';
}

function applyPoll() {
  const sec = Number(localStorage.getItem(POLL_STORE) ?? 60);
  $("#poll-select").value = String(sec);
  if (pollTimer) clearInterval(pollTimer);
  if (sec > 0) pollTimer = setInterval(() => renderQuota().catch(() => {}), sec * 1000);
}

$("#poll-select").addEventListener("change", (ev) => {
  localStorage.setItem(POLL_STORE, ev.target.value);
  applyPoll();
});
$("#btn-quota-refresh").addEventListener("click", () => renderQuota().catch((e) => alert(e.message)));

/* ------------------------------------------------ 设置页 */

async function renderSettings() {
  const s = await api("GET", "/status");
  const key = apiKey();
  const masked = key ? key.slice(0, 8) + "…" + key.slice(-4) : "—";
  const p = s.pool || {};
  $("#settings-cards").innerHTML = `
    <div class="card"><h3>账号池与续期（二期）</h3>${kv([
      ["池策略", p.enabled ? `开 · ${esc(p.strategy || "round-robin")} · 坏号冷却 ${Math.round((p.cooldown_ms || 0) / 1000)}s` : "关（回落 sso-session 单会话）"],
      ["401 自动续期", p.auto_renew ? '<span class="ok">开</span> —— pass_token 续期→原号重试，失败换号；只回马一次；<b>流式不重试</b>' : "关"],
      ["定时兜底续期", p.refresh_after_ms ? `每 ${Math.round(p.refresh_after_ms / 3600000)} 小时` : "关（纯 401 事件驱动）"],
    ])}<p class="dim">serviceToken 是无有效期声明的会话 cookie —— 401 事件驱动是主力，定时只是兜底。</p></div>
    <div class="card"><h3>管理密钥</h3>${kv([
      ["当前密钥", `<code>${esc(masked)}</code> <button id="btn-copy-key">复制</button>`],
      ["存放位置", `<code>${esc(s.security.admin_key_file)}</code>`],
      ["固定密钥", 'env <code>XM2API_ADMIN_KEY</code> 或 config <code>server.adminKey</code>（勿提交真实密钥）'],
    ])}</div>
    <div class="card"><h3>安全护栏</h3>${kv([
      ["Host 校验", esc(s.security.host_check)],
      ["Origin 校验", esc(s.security.origin_check)],
      ["allowedHosts", esc((s.security.allowed_hosts || []).join(", ") || "（仅本机回环）")],
      ["allowedOrigins", esc((s.security.allowed_origins || []).join(", ") || "（仅同源）")],
    ])}<p class="dim">SDK / curl 不带 Origin，不受影响；任意网页 JS 已无法调用本服务。</p></div>
    <div class="card"><h3>文档</h3><p class="dim">管理界面说明 <code>docs/ui-admin.md</code>；反代说明 <code>README.md</code>；
      池与续期语义同见 <code>docs/ui-admin.md</code> 二期章节。</p></div>`;
  $("#btn-copy-key").addEventListener("click", async () => {
    await navigator.clipboard.writeText(key);
    $("#btn-copy-key").textContent = "已复制";
    setTimeout(() => ($("#btn-copy-key").textContent = "复制"), 1200);
  });
}

/* ------------------------------------------------ 路由与启动 */

const TABS = {
  status: renderStatus,
  creds: async () => {
    await renderAccounts();
  },
  quota: async () => {
    await renderQuota();
    applyPoll();
  },
  settings: renderSettings,
};

function route() {
  const name = (location.hash.replace(/^#\//, "") || "status").split("?")[0];
  const tab = TABS[name] ? name : "status";
  $$(".tab").forEach((s) => s.classList.add("hidden"));
  $("#tab-" + tab).classList.remove("hidden");
  $$("#tabs a").forEach((a) => a.classList.toggle("active", a.dataset.tab === tab));
  TABS[tab]().catch((e) => {
    console.error(e);
  });
}

window.addEventListener("hashchange", route);

$("#btn-login").addEventListener("click", () => tryLogin($("#key-input").value.trim()));
$("#key-input").addEventListener("keydown", (e) => {
  if (e.key === "Enter") tryLogin($("#key-input").value.trim());
});

async function boot() {
  route();
}

(async function init() {
  if (!apiKey()) {
    showLogin();
    return;
  }
  try {
    await api("GET", "/status");
    boot();
  } catch {
    /* 401 已弹登录框；其他错误照常进页面展示 */
    boot();
  }
})();
