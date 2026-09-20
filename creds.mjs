#!/usr/bin/env node
/**
 * 线路2 凭证工具（非交互）。
 *
 * 用法：
 *   node creds.mjs              查看凭证 + 服务状态
 *   node creds.mjs --ensure     确保凭证可用（有效则复用）
 *   node creds.mjs --refresh    强制重跑整条链路（复制 Cookies → 读账号 → 换 token → 落盘）
 *   node creds.mjs --probe      打一次 /v1/chat/completions，用于健康检查
 *   node creds.mjs --check      只看 cookie 库是否可复制（不动 token）
 *
 * 可选：--sid <sid>（默认 mimopc）、--base <url>（默认取 config.yaml 的 client.baseUrl）
 */
import { DEFAULT_SID, copyCookies, credentialsStatus, ensureCredentials } from "./lib/pipeline.mjs";
import { config } from "./lib/config.mjs";

const args = process.argv.slice(2);
const has = (f) => args.includes(f);
const opt = (f, d) => {
  const i = args.indexOf(f);
  return i >= 0 && args[i + 1] ? args[i + 1] : d;
};

// 命令行 > 环境变量 > config.yaml（由 lib/config.mjs 合并）
const SID = opt("--sid", config.credentials.sid || DEFAULT_SID);
const MODEL = opt("--model", config.client.model);

/*
 * config.client.baseUrl 是 OpenAI 风格的 base（形如 http://host:port/v1），
 * 而 /__xm2api 自检和探针要打的是服务根。这里统一去掉结尾的 /v1 得到根地址，
 * 避免拼成 /v1/v1/chat/completions。
 */
const BASE = opt("--base", config.client.baseUrl).replace(/\/$/, "");
const ROOT_URL = BASE.replace(/\/v1$/, "");

const log = (m) => console.log(`  ${m}`);

async function proxyMeta() {
  try {
    const r = await fetch(`${ROOT_URL}/__xm2api`, { signal: AbortSignal.timeout(3000) });
    return r.ok ? await r.json() : null;
  } catch {
    return null;
  }
}

function printStatus(meta) {
  const s = credentialsStatus();
  console.log("凭证");
  log(`routeCookieHeader : ${s.hasRouteCookie ? "✅ 有" : "❌ 缺失"}`);
  log(`passToken 缓存    : ${s.hasPassToken ? "有" : "无"}`);
  if (s.sid) log(`sid / 获取时间    : ${s.sid} / ${s.obtainedAt}`);
  log(`cookie 库副本     : ${s.cookieCopy ? "有" : "无"}`);
  log(`AES key（非必需） : ${s.aesKey ? "有" : "无"}`);
  log(`session 文件      : ${s.sessionFile}`);
  console.log("服务");
  if (meta) {
    log(`地址              : ${BASE} ✅ 在跑`);
    log(`上游              : ${meta.upstream}`);
    const m = meta.models || {};
    log(`模型来源          : ${m.source || "-"}${m.endpoint ? `  ← ${m.endpoint}` : ""}`);
    log(`模型类型过滤      : ${m.types?.length ? m.types.join(", ") : "全部"}`);
    log(`服务侧凭证        : present=${meta.credentials?.present} sid=${meta.credentials?.sid ?? "-"}`);
  } else {
    log(`地址              : ${BASE} ❌ 未响应（启动：npm run serve）`);
  }
}

async function probe() {
  const res = await fetch(`${ROOT_URL}/v1/chat/completions`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({
      model: MODEL,
      messages: [{ role: "user", content: "say OK" }],
      max_tokens: config.client.maxTokens,
    }),
    signal: AbortSignal.timeout(60000),
  });
  const text = await res.text();
  if (res.status !== 200) {
    console.log(`  ❌ HTTP ${res.status}  ${text.slice(0, 200)}`);
    if (res.status === 401) console.log("  → 凭证失效，执行：node creds.mjs --refresh");
    return 1;
  }
  let j = null;
  try {
    j = JSON.parse(text);
  } catch {}
  const msg = j?.choices?.[0]?.message || {};
  console.log(`  ✅ HTTP 200  ${j?.model || MODEL}  content=${JSON.stringify(msg.content ?? msg.reasoning_content ?? "")}`);
  return 0;
}

async function main() {
  if (has("--check")) {
    const r = copyCookies({ force: false });
    console.log("cookie 库");
    log(r.ok ? `可用（${r.method}, ${r.bytes}B）` : `不可用：${r.error}`);
    if (r.warning) log(`⚠️ ${r.warning}`);
    return r.ok ? 0 : 1;
  }

  if (has("--ensure") || has("--refresh")) {
    const force = has("--refresh");
    console.log(force ? "刷新凭证（强制重跑）" : "确保凭证");
    const r = await ensureCredentials({ sid: SID, force, log: (m) => log(m) });
    if (!r.ok) {
      console.log(`  ❌ ${r.action} 失败：${r.error}`);
      for (const s of r.steps) log(`· ${s}`);
      if (/passToken/.test(r.error || "")) log("提示：passToken 失效需要在 MiMo 客户端重新登录一次。");
      return 1;
    }
    if (r.action === "reuse") log("已有有效凭证，直接复用");
    else for (const s of r.steps) log(`· ${s}`);
    console.log("  ✅ 就绪");
    return 0;
  }

  if (has("--probe")) return probe();

  printStatus(await proxyMeta());
  return 0;
}

main()
  .then((code) => {
    process.exitCode = code;
  })
  .catch((err) => {
    console.error("creds: fatal:", err);
    process.exitCode = 1;
  });
