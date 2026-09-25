/* xm2api 管理控制台（三期）：仪表盘 + KPI + 手写 SVG 图表。零框架零依赖。 */
"use strict";

const $ = (s) => document.querySelector(s);
const $$ = (s) => [...document.querySelectorAll(s)];
const esc = (s) => String(s ?? "").replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
const KEY_STORE = "xm2api.mgmtKey";
const POLL_STORE = "xm2api.pollSec";
const THEME_STORE = "xm2api.theme";

let pollTimer = null;

/* ------------------------------------------------ API */

const apiKey = () => sessionStorage.getItem(KEY_STORE) || "";

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
  try { j = await res.json(); } catch {}
  if (res.status === 401) {
    showLogin();
    throw new Error("管理密钥无效或缺失");
  }
  if (!res.ok) throw new Error(j?.error?.message || `HTTP ${res.status}`);
  return j;
}

/* ------------------------------------------------ 主题 / 登录 */

function applyTheme(t) {
  if (t) document.documentElement.setAttribute("data-theme", t);
  else document.documentElement.removeAttribute("data-theme");
}
applyTheme(localStorage.getItem(THEME_STORE) || "");
$("#btn-theme").addEventListener("click", () => {
  const cur = document.documentElement.getAttribute("data-theme")
    || (matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light");
  const next = cur === "dark" ? "light" : "dark";
  localStorage.setItem(THEME_STORE, next);
  applyTheme(next);
});

function showLogin() { $("#login").classList.remove("hidden"); }
function hideLogin() { $("#login").classList.add("hidden"); }

async function tryLogin(key) {
  sessionStorage.setItem(KEY_STORE, key);
  try {
    await api("GET", "/status");
    hideLogin();
    route();
    return true;
  } catch (e) {
    sessionStorage.removeItem(KEY_STORE);
    $("#login-err").textContent = e.message;
    return false;
  }
}
$("#btn-login").addEventListener("click", () => tryLogin($("#key-input").value.trim()));
$("#key-input").addEventListener("keydown", (e) => { if (e.key === "Enter") tryLogin($("#key-input").value.trim()); });

/* ------------------------------------------------ 手写 SVG 图表 */

const PALETTE = ["var(--primary)", "var(--primary-2)", "#f59e0b", "#a78bfa", "#34d399", "#f472b6"];

/** 额度趋势折线图。series: [{name, points:[{t,p}]}]，Y = 剩余 %（0~100）。 */
function lineChart(series) {
  const W = 620, H = 240, P = { l: 38, r: 12, t: 14, b: 26 };
  const pts = series.flatMap((s) => s.points);
  if (pts.length < 2) {
    return `<div class="dim" style="padding:44px 0;text-align:center">数据积累中 —— 每次刷新额度会写入一点（10 分钟节流），两点起画趋势</div>`;
  }
  const t0 = Math.min(...pts.map((p) => p.t)), t1 = Math.max(...pts.map((p) => p.t));
  const x = (t) => P.l + ((t - t0) / Math.max(1, t1 - t0)) * (W - P.l - P.r);
  const y = (p) => P.t + (1 - p / 100) * (H - P.t - P.b);
  const grid = [0, 25, 50, 75, 100].map((v) =>
    `<line x1="${P.l}" y1="${y(v)}" x2="${W - P.r}" y2="${y(v)}" stroke="currentColor" opacity=".08"/>
     <text x="${P.l - 8}" y="${y(v) + 4}" text-anchor="end" font-size="10" fill="currentColor" opacity=".45">${v}%</text>`
  ).join("");
  const lines = series.map((s, si) => {
    const col = PALETTE[si % PALETTE.length];
    const path = s.points.map((p, i) => `${i ? "L" : "M"}${x(p.t).toFixed(1)},${y(p.p).toFixed(1)}`).join("");
    const area = `${path}L${x(s.points[s.points.length - 1].t).toFixed(1)},${y(0)}L${x(s.points[0].t).toFixed(1)},${y(0)}Z`;
    const dots = s.points.slice(-40).map((p) =>
      `<circle cx="${x(p.t).toFixed(1)}" cy="${y(p.p).toFixed(1)}" r="3" fill="${col}">
        <title>${esc(s.name)} · ${new Date(p.t).toLocaleString()} · ${p.p}%</title></circle>`).join("");
    return `<path d="${area}" fill="${col}" opacity=".10"/>
      <path d="${path}" fill="none" stroke="${col}" stroke-width="2.5" stroke-linejoin="round"
        style="filter:drop-shadow(0 2px 6px ${col})"/>${dots}`;
  }).join("");
  return `<svg viewBox="0 0 ${W} ${H}">
    <defs><linearGradient id="qg" x1="0" y1="0" x2="0" y2="1">
      <stop offset="0%" stop-color="var(--primary)" stop-opacity=".25"/><stop offset="100%" stop-color="var(--primary)" stop-opacity="0"/>
    </linearGradient></defs>
    ${grid}${lines}
    <text x="${P.l}" y="${H - 6}" font-size="10" fill="currentColor" opacity=".45">${new Date(t0).toLocaleDateString()}</text>
    <text x="${W - P.r}" y="${H - 6}" font-size="10" text-anchor="end" fill="currentColor" opacity=".45">${new Date(t1).toLocaleDateString()}</text>
  </svg>`;
}

/** 近 30 天请求量柱状图（成功 + 错误叠加）。 */
function barChart(daily) {
  const W = 620, H = 240, P = { l: 38, r: 12, t: 14, b: 26 };
  const max = Math.max(1, ...daily.map((d) => d.requests));
  const bw = (W - P.l - P.r) / daily.length;
  const y = (v) => P.t + (1 - v / max) * (H - P.t - P.b);
  const grid = [0, 0.5, 1].map((f) =>
    `<line x1="${P.l}" y1="${y(max * f)}" x2="${W - P.r}" y2="${y(max * f)}" stroke="currentColor" opacity=".08"/>
     <text x="${P.l - 8}" y="${y(max * f) + 4}" text-anchor="end" font-size="10" fill="currentColor" opacity=".45">${(max * f).toFixed(max < 10 ? 1 : 0)}</text>`
  ).join("");
  const bars = daily.map((d, i) => {
    const hReq = (H - P.t - P.b) * (d.requests / max);
    const hErr = (H - P.t - P.b) * (Math.min(d.errors, d.requests) / max);
    const bx = P.l + i * bw + bw * 0.18;
    const w = bw * 0.64;
    return `<rect x="${bx}" y="${(H - P.b - hReq).toFixed(1)}" width="${w}" height="${Math.max(d.requests ? 2 : 0, hReq).toFixed(1)}"
        rx="3" fill="var(--primary)" opacity=".85"><title>${d.date} · ${d.requests} 次（错误 ${d.errors}）</title></rect>
      <rect x="${bx}" y="${(H - P.b - hReq).toFixed(1)}" width="${w}" height="${hErr.toFixed(1)}" rx="3" fill="var(--bad)">
        <title>${d.date} · 错误 ${d.errors}</title></rect>`;
  }).join("");
  return `<svg viewBox="0 0 ${W} ${H}">${grid}${bars}
    <text x="${P.l}" y="${H - 6}" font-size="10" fill="currentColor" opacity=".45">${daily[0]?.date.slice(5) || ""}</text>
    <text x="${W - P.r}" y="${H - 6}" font-size="10" text-anchor="end" fill="currentColor" opacity=".45">${daily[daily.length - 1]?.date.slice(5) || ""}</text>
  </svg>`;
}

/** 额度环（donut）。 */
function donut(pct, cls) {
  const p = Math.max(0, Math.min(100, pct ?? 0));
  const r = 26, c = 2 * Math.PI * r;
  return `<svg viewBox="0 0 64 64" width="64" height="64">
    <circle cx="32" cy="32" r="${r}" fill="none" stroke="currentColor" opacity=".1" stroke-width="7"/>
    <circle cx="32" cy="32" r="${r}" fill="none" stroke="${cls}" stroke-width="7" stroke-linecap="round"
      stroke-dasharray="${(c * p / 100).toFixed(1)} ${c.toFixed(1)}" transform="rotate(-90 32 32)"
      style="filter:drop-shadow(0 2px 5px ${cls})"/>
    <text x="32" y="37" text-anchor="middle" font-size="14" font-weight="700" fill="currentColor">${Math.round(p)}%</text>
  </svg>`;
}

const qCls = (p) => (p === null ? "var(--warn)" : p >= 70 ? "var(--ok)" : p >= 30 ? "var(--warn)" : "var(--bad)");

/* ------------------------------------------------ 仪表盘 */

async function renderDashboard() {
  const [s, acc, q, hist, daily] = await Promise.all([
    api("GET", "/status"),
    api("GET", "/accounts").catch(() => ({ accounts: [] })),
    api("GET", "/quota").catch(() => null),
    api("GET", "/usage/history").catch(() => ({ history: {} })),
    api("GET", "/usage/daily?days=30").catch(() => ({ daily: [] })),
  ]);

  // 侧栏 + KPI
  $("#svc-dot").className = "dot";
  $("#svc-text").textContent = `运行中 · ${s.host}:${s.port}`;
  const enabled = acc.accounts.filter((a) => a.enabled).length;
  $("#k-accounts").textContent = acc.accounts.length;
  $("#k-accounts-sub").textContent = `启用 ${enabled} · 池 ${s.pool?.enabled ? "开" : "关"}`;
  const today = daily.daily?.[daily.daily.length - 1] || { requests: 0, errors: 0 };
  $("#k-today").textContent = today.requests;
  $("#k-today-sub").textContent = `错误 ${today.errors} · 转发侧统计`;
  const all = [q?.session, ...(q?.accounts || [])].filter((x) => x?.ok && typeof x.percent === "number");
  $("#k-quota").textContent = all.length ? Math.round(all.reduce((n, x) => n + x.percent, 0) / all.length) + "%" : "—";
  $("#k-quota-sub").textContent = all.length ? `${all.length} 个可查凭证` : "点击「刷新数据」更新";
  $("#k-svc").textContent = "运行中";
  $("#k-svc-sub").textContent = `autoRenew ${s.pool?.auto_renew ? "开" : "关"} · 兜底 ${s.pool?.refresh_after_ms ? Math.round(s.pool.refresh_after_ms / 3600000) + "h" : "关"}`;

  // 图表
  const series = [];
  if (hist.history?.session?.length > 1) series.push({ name: "当前转发会话", points: hist.history.session });
  for (const [id, pts] of Object.entries(hist.history || {})) {
    if (id !== "session" && pts.length > 1) series.push({ name: id, points: pts });
  }
  $("#chart-quota").innerHTML = lineChart(series);
  $("#chart-daily").innerHTML = barChart(daily.daily?.length ? daily.daily : []);

  // 账号池摘要
  $("#dash-pool").innerHTML = acc.accounts.length
    ? acc.accounts.map((a) => {
        const st = a.stats || {};
        const cooling = (st.cooldownUntil || 0) > Date.now();
        return `<div class="row-actions" style="margin:6px 0">
          <b>${esc(a.label)}</b>
          <span class="badge ${a.enabled ? "on" : "off"}">${a.enabled ? "启用" : "禁用"}</span>
          ${cooling ? '<span class="badge" style="color:var(--bad)">冷却中</span>' : ""}
          <span class="dim">✓${st.success || 0} ✗${st.failed || 0}${st.renewCount ? ` · 续期×${st.renewCount}` : ""}</span>
        </div>`;
      }).join("")
    : '<span class="dim">账号池为空 —— 用「一键提取」或凭证页导入 mimo.json</span>';
}

/* ------------------------------------------------ 凭证与账号 */

async function renderAccounts() {
  const { accounts } = await api("GET", "/accounts");
  const tb = $("#accounts-table tbody");
  if (!accounts.length) {
    tb.innerHTML = `<tr><td colspan="6" class="dim">还没有账号 —— 用上面的按钮提取或导入</td></tr>`;
    return;
  }
  tb.innerHTML = accounts.map((a) => {
    const creds = [];
    creds.push(a.has_pass_token ? '<span class="ok">passToken ✓</span>' : '<span class="dim">passToken ✗</span>');
    creds.push(a.has_service_token ? `<span class="ok">serviceToken ✓(${esc(a.service_token_len)}B)</span>` : '<span class="dim">serviceToken ✗</span>');
    const s = a.stats || {};
    const cooling = (s.cooldownUntil || 0) > Date.now();
    const poolStat = `<span class="ok">✓${esc(s.success || 0)}</span> <span class="${s.failed ? "bad" : "dim"}">✗${esc(s.failed || 0)}</span>`
      + (s.renewCount ? `<div class="dim">续期 ×${esc(s.renewCount)}</div>` : "")
      + (cooling ? '<div class="bad">冷却中…</div>' : "");
    return `<tr data-id="${esc(a.id)}">
      <td><b>${esc(a.label)}</b><div class="dim mono">${esc(a.user_id)}</div></td>
      <td>${creds.join("<br>")}</td>
      <td class="dim">${esc(a.source || "-")}<div class="dim">${esc((a.obtained_at || "").slice(0, 19).replace("T", " "))}</div></td>
      <td>${poolStat}</td>
      <td>${a.enabled ? '<span class="badge on">启用</span>' : '<span class="badge off">禁用</span>'}</td>
      <td><button data-act="toggle">${a.enabled ? "禁用" : "启用"}</button>
          <button data-act="del" class="danger">删除</button></td>
    </tr>`;
  }).join("");
}

$("#accounts-table").addEventListener("click", async (ev) => {
  const btn = ev.target.closest("button[data-act]");
  if (!btn) return;
  const id = btn.closest("tr").dataset.id;
  try {
    if (btn.dataset.act === "toggle") {
      await api("PATCH", `/accounts/${encodeURIComponent(id)}`, { enabled: btn.textContent.trim() === "启用" });
    } else if (btn.dataset.act === "del") {
      if (!confirm(`删除账号 ${id}？（凭证文件将从 data/accounts 移除）`)) return;
      await api("DELETE", `/accounts/${encodeURIComponent(id)}`);
    }
    await renderAccounts();
  } catch (e) {
    alert("操作失败：" + e.message);
  }
});

async function doExtract() {
  const out = $("#extract-out");
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
}
$("#btn-extract").addEventListener("click", doExtract);
const goCredsExtract = () => { location.hash = "#/creds"; setTimeout(doExtract, 250); };
$("#btn-extract-top").addEventListener("click", goCredsExtract);
$("#btn-dash-extract").addEventListener("click", goCredsExtract);

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

async function renderQuota() {
  const q = await api("GET", "/quota");
  $("#quota-observed").textContent = "更新于 " + (q.observed_at || "").slice(11, 19);
  const cards = [];
  const mk = (title, r) => {
    if (!r) return;
    if (!r.ok) {
      cards.push(`<div class="card kpi"><div class="k-head"><span>${esc(title)}</span></div>
        <div class="bad" style="margin-top:10px">额度获取失败：${esc(r.error)}</div></div>`);
      return;
    }
    const pct = typeof r.percent === "number" ? r.percent : null;
    cards.push(`<div class="card kpi"><div class="k-glow"></div>
      <div class="k-head"><span>${esc(title)}</span><span class="k-ic">◔</span></div>
      <div style="display:flex;align-items:center;gap:14px;margin-top:8px">
        ${donut(pct ?? 0, qCls(pct))}
        <div class="kv">
          <div>剩余</div><div>${pct === null ? "—" : (pct >= 99.95 ? '<span class="ok">额度可用</span>' : `<b>${Math.round(pct)}%</b>`)}</div>
          <div>重置</div><div>${esc(r.resetDate || (r.resetAt ? new Date(r.resetAt * 1000).toLocaleString() : "—"))}</div>
        </div>
      </div></div>`);
  };
  if (q.session) mk("当前转发会话", q.session);
  for (const a of q.accounts || []) mk(a.label || a.id, a);
  $("#quota-cards").innerHTML = cards.join("") || '<div class="card dim">还没有可查的账号</div>';
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
      ["固定密钥", 'env <code>XM2API_ADMIN_KEY</code> 或 config <code>server.adminKey</code>'],
    ])}</div>
    <div class="card"><h3>安全护栏</h3>${kv([
      ["Host 校验", esc(s.security.host_check)],
      ["Origin 校验", esc(s.security.origin_check)],
      ["allowedHosts", esc((s.security.allowed_hosts || []).join(", ") || "（仅本机回环）")],
      ["allowedOrigins", esc((s.security.allowed_origins || []).join(", ") || "（仅同源）")],
    ])}<p class="dim">SDK / curl 不带 Origin，不受影响；任意网页 JS 已无法调用本服务。</p></div>
    <div class="card"><h3>文档</h3><p class="dim">管理界面说明 <code>docs/ui-admin.md</code>；反代说明 <code>README.md</code>；
      池与续期语义见 <code>docs/ui-admin.md</code>。</p></div>`;
  $("#btn-copy-key").addEventListener("click", async () => {
    await navigator.clipboard.writeText(key);
    $("#btn-copy-key").textContent = "已复制";
    setTimeout(() => ($("#btn-copy-key").textContent = "复制"), 1200);
  });
}

function kv(rows) {
  return `<div class="kv">${rows.map(([k, v]) => `<div>${esc(k)}</div><div>${v}</div>`).join("")}</div>`;
}

/* ------------------------------------------------ 路由与启动 */

const VIEWS = {
  dashboard: { title: "仪表盘", sub: "服务、账号池与额度的总览", render: renderDashboard },
  creds: { title: "凭证与账号", sub: "本机提取 / mimo.json 导入 / 多账号管理", render: renderAccounts },
  quota: { title: "额度", sub: "每账号剩余额度与重置周期", render: async () => { await renderQuota(); applyPoll(); } },
  settings: { title: "设置", sub: "池策略 / 安全护栏 / 管理密钥", render: renderSettings },
};

function route() {
  const name = (location.hash.replace(/^#\//, "") || "dashboard").split("?")[0];
  const v = VIEWS[name] ? name : "dashboard";
  $$(".view").forEach((s) => s.classList.add("hidden"));
  $("#view-" + v).classList.remove("hidden");
  $$("#nav a").forEach((a) => a.classList.toggle("active", a.dataset.view === v));
  $("#page-title").textContent = VIEWS[v].title;
  $("#page-sub").textContent = VIEWS[v].sub;
  VIEWS[v].render().catch((e) => console.error(e));
}
window.addEventListener("hashchange", route);

$("#btn-global-refresh").addEventListener("click", () => route());

(async function init() {
  if (!apiKey()) { showLogin(); return; }
  try {
    await api("GET", "/status");
    boot();
  } catch {
    boot();
  }
})();
function boot() { route(); }
