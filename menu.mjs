#!/usr/bin/env node
/**
 * 交互式启停菜单。
 *
 *   node menu.mjs                交互菜单
 *   node menu.mjs start          直接启动（后台运行）
 *   node menu.mjs stop           直接停止
 *   node menu.mjs restart
 *   node menu.mjs status         状态 + 最近一次请求
 *   node menu.mjs probe          健康检查
 *   node menu.mjs caps           能力自检（工具调用 / 流式分片 / 联网搜索）
 *
 * 停止服务不依赖 pid 文件：先 POST /__xm2api/shutdown 让服务自己退，
 * 失败才退回 logs/server.pid + kill（服务可能是 npm run serve 启的，那样没有 pid 文件）。
 *
 * 也可以双击 start.bat（它只是本脚本的入口）。
 */
import fs from "node:fs";
import net from "node:net";
import path from "node:path";
import readline from "node:readline";
import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";
import { config, CONFIG_PATH } from "./lib/config.mjs";
import { credentialsStatus, ensureCredentials } from "./lib/pipeline.mjs";
import { SESSION_OUT } from "./lib/chrome-cookie.mjs";

const ROOT = path.dirname(fileURLToPath(import.meta.url));
const PID_FILE = path.join(ROOT, "logs", "server.pid");
const STDOUT_LOG = path.join(ROOT, "logs", "server-stdout.log");
const STDERR_LOG = path.join(ROOT, "logs", "server-stderr.log");

const HOST = config.server.host;
const PORT = config.server.port;
const META_URL = `http://${HOST}:${PORT}/__xm2api`;
const CHAT_URL = `http://${HOST}:${PORT}/v1/chat/completions`;

const C = { dim: "\x1b[2m", green: "\x1b[32m", yellow: "\x1b[33m", red: "\x1b[31m", cyan: "\x1b[36m", reset: "\x1b[0m" };
const c = (n, s) => `${C[n] || ""}${s}${C.reset}`;
const ok = (s) => c("green", s);
const bad = (s) => c("red", s);
const warn = (s) => c("yellow", s);
const dim = (s) => c("dim", s);

/* ------------------------------------------------------------------ 状态探测 */

async function serverMeta(timeoutMs = 1500) {
  try {
    const r = await fetch(META_URL, { signal: AbortSignal.timeout(timeoutMs) });
    return r.ok ? await r.json() : null;
  } catch {
    return null;
  }
}

function readPid() {
  try {
    const pid = Number(fs.readFileSync(PID_FILE, "utf8").trim());
    if (!pid) return null;
    process.kill(pid, 0); // 存活探测
    return pid;
  } catch {
    return null;
  }
}

function clearPidFile() {
  fs.rmSync(PID_FILE, { force: true });
}

/** 端口上是谁：ours（我们的服务）/ other（别的程序）/ closed（没人监听） */
async function portState(timeoutMs = 2000) {
  const meta = await serverMeta(timeoutMs);
  if (meta) return { state: "ours", meta };
  const detail = await new Promise((resolve) => {
    const sock = net.connect({ host: HOST, port: PORT });
    const done = (v) => {
      try {
        sock.destroy();
      } catch {}
      resolve(v);
    };
    sock.setTimeout(timeoutMs, () => done("timeout"));
    sock.on("connect", () => done("open"));
    sock.on("error", (e) => done(e.code === "ECONNREFUSED" ? "refused" : `error:${e.code}`));
  });
  return { state: detail === "refused" ? "closed" : "other", detail };
}

/** 等端口彻底不再响应我们的 meta；返回是否已停干净 */
async function waitGone(maxMs) {
  const until = Date.now() + maxMs;
  while (Date.now() < until) {
    await new Promise((r) => setTimeout(r, 250));
    if (!(await serverMeta(600))) return true;
  }
  return false;
}

/** 让服务自己退出 —— 不需要 pid 文件，谁启的都能停 */
async function requestShutdown() {
  try {
    const r = await fetch(`${META_URL}/shutdown`, { method: "POST", signal: AbortSignal.timeout(3000) });
    return r.ok;
  } catch {
    // 服务可能在回完响应后立刻断开连接，这不算失败，交给 waitGone 判定
    return null;
  }
}

function portOwnerHint() {
  return (
    `    查占用者：Get-NetTCPConnection -LocalPort ${PORT} -State Listen | Select OwningProcess\n` +
    `    或：      netstat -ano | findstr :${PORT}\n` +
    `    查到 pid：Stop-Process -Id <pid> -Force`
  );
}

/** 今天那份抓包日志的最后 n 行（只读，不改） */
function lastCaptureLines(n = 5) {
  const file = path.join(config.logging.dir, `path2-capture-${new Date().toISOString().slice(0, 10)}.jsonl`);
  try {
    const lines = fs.readFileSync(file, "utf8").trim().split(/\r?\n/).filter(Boolean);
    return { file, lines: lines.slice(-n) };
  } catch {
    return { file, lines: [] };
  }
}

/* ------------------------------------------------------------------ 动作 */

async function doStart() {
  const port = await portState();
  if (port.state === "ours") {
    console.log(`  ${warn("服务已在运行")} ${META_URL}`);
    const pid = readPid();
    if (pid) console.log(dim(`  pid=${pid}`));
    else console.log(dim("  （pid 文件不是本脚本写的，停止请用菜单选 2 —— 它会走服务自带的停止接口）"));
    return true;
  }
  if (port.state === "other") {
    console.log(`  ${bad(`❌ 端口 ${PORT} 被别的程序占用`)}（探测结果：${port.detail}）`);
    console.log(dim(portOwnerHint()));
    console.log(dim(`    或改端口：XM2API_PORT=18790 npm run serve`));
    return false;
  }

  fs.mkdirSync(path.dirname(PID_FILE), { recursive: true });
  const out = fs.openSync(STDOUT_LOG, "a");
  const err = fs.openSync(STDERR_LOG, "a");
  const child = spawn(process.execPath, [path.join(ROOT, "server.mjs")], {
    cwd: ROOT,
    detached: true,
    stdio: ["ignore", out, err],
    env: process.env,
  });
  child.unref();
  fs.writeFileSync(PID_FILE, String(child.pid));

  process.stdout.write("  启动中");
  for (let i = 0; i < 40; i++) {
    await new Promise((r) => setTimeout(r, 250));
    if (await serverMeta(800)) {
      console.log(`\r  ${ok("✅ 已启动")}  http://${HOST}:${PORT}  pid=${child.pid}`);
      console.log(dim(`  日志: ${path.relative(ROOT, STDOUT_LOG)}`));
      return true;
    }
    process.stdout.write(".");
  }
  console.log(`\r  ${bad("❌ 15 秒内没起来")}`);
  clearPidFile();
  try {
    const tail = fs.readFileSync(STDERR_LOG, "utf8").trim().split(/\r?\n/).slice(-6);
    if (tail.length) for (const l of tail) console.log(dim(`  ${l}`));
  } catch {}
  return false;
}

/**
 * 停止服务。
 *
 * 两步走，因为 pid 文件不可靠（服务可能是 npm run serve 启的、pid 文件可能过期
 * 或被别的实例覆盖过 —— 只信它就会出现"明明在跑却停不掉"）：
 *   ① 先让服务自己退：POST /__xm2api/shutdown（旧版本没这条接口，会失败）
 *   ② 再退回 pid 文件 + kill
 * 最后用端口是否还在响应来判定，不靠任何推断。
 */
async function doStop() {
  const port = await portState();
  const pid = readPid();

  if (port.state === "closed" && !pid) {
    console.log(`  ${warn("服务没在运行")}`);
    clearPidFile();
    return true;
  }
  if (port.state === "other") {
    console.log(`  ${bad(`❌ 端口 ${PORT} 上不是本服务`)}（探测结果：${port.detail}）—— 不会去动它`);
    console.log(dim(portOwnerHint()));
    return false;
  }

  // ① 优雅停止：不依赖 pid 文件
  if (port.state === "ours") {
    const sent = await requestShutdown();
    if (sent === false) {
      console.log(dim("  服务自带停止接口返回失败（版本较旧？），改用 pid"));
    } else {
      console.log(dim("  已请求服务自行退出（POST /__xm2api/shutdown）"));
      if (await waitGone(10000)) {
        clearPidFile();
        console.log(`  ${ok("✅ 已停止")}  端口 ${PORT} 已释放`);
        return true;
      }
      console.log(dim("  10 秒没退干净，退回 pid"));
    }
  }

  // ② pid 兜底
  if (pid && pid !== process.pid) {
    try {
      process.kill(pid);
      console.log(dim(`  已向 pid=${pid} 发送终止信号`));
    } catch (e) {
      console.log(dim(`  kill(${pid}) 失败：${e.message}`));
    }
    if (await waitGone(8000)) {
      clearPidFile();
      console.log(`  ${ok("✅ 已停止")}  端口 ${PORT} 已释放`);
      return true;
    }
  } else if (!pid) {
    console.log(dim("  pid 文件里没有可用的 pid（服务不是本脚本启动的）"));
  }

  console.log(`  ${bad("❌ 仍在响应")}  ${META_URL}`);
  console.log(dim(portOwnerHint()));
  return false;
}

async function doStatus() {
  const meta = await serverMeta();
  const creds = credentialsStatus();
  const pid = readPid();

  console.log(c("cyan", "  配置"));
  console.log(`    文件      : ${path.relative(ROOT, CONFIG_PATH)}`);
  console.log(`    监听      : ${HOST}:${PORT}`);
  console.log(`    上游      : ${config.server.upstream}`);
  console.log(
    `    模型      : ${
      Array.isArray(config.server.models) ? `写死 ${config.server.models.length} 个` : "从上游 /api/model/list 实时拉取"
    }${config.server.modelTypes.length ? `（只暴露 ${config.server.modelTypes.join("/")}）` : ""}`
  );
  console.log(
    `    日志      : ${config.logging.enabled ? "开" : "关"}` +
      `，body ${config.logging.captureBody ? "记录" : "不记录"}`
  );
  console.log(
    `    兼容层    : 翻译 ${config.server.compat.webSearchFlag ? "开" : "关"}` +
      `，主动注入 ${config.server.compat.webSearchAuto ? "开" : "关"}`
  );

  console.log(c("cyan", "  服务"));
  if (meta) {
    console.log(`    状态      : ${ok("✅ 运行中")}${pid ? dim(`  pid=${pid}`) : dim("  pid 未知（不是本脚本启动的）")}`);
    console.log(`    服务侧凭证: present=${meta.credentials?.present} sid=${meta.credentials?.sid ?? "-"}`);
  } else {
    const port = await portState();
    console.log(
      `    状态      : ${port.state === "other" ? bad(`❌ 端口 ${PORT} 被别的程序占用（${port.detail}）`) : bad("❌ 未运行")}`
    );
  }

  console.log(c("cyan", "  凭证"));
  console.log(`    routeCookieHeader : ${creds.hasRouteCookie ? ok("✅ 有") : bad("❌ 缺失")}`);
  console.log(`    passToken 缓存    : ${creds.hasPassToken ? "有" : "无"}`);
  if (creds.sid) console.log(`    换取时间          : ${creds.sid} / ${creds.obtainedAt}`);
  console.log(`    文件              : ${path.relative(ROOT, SESSION_OUT)}`);

  const { file, lines } = lastCaptureLines(5);
  console.log(c("cyan", "  最近请求") + dim(`  (${path.relative(ROOT, file)})`));
  if (!lines.length) console.log(dim("    （今天还没有记录）"));
  for (const l of lines) {
    try {
      const j = JSON.parse(l);
      const t = (j.ts || "").slice(11, 19);
      const status = j.response?.status ?? j.error ?? "-";
      const flag = String(status) === "200" ? ok(status) : bad(status);
      console.log(`    ${dim(t)}  ${j.request?.method} ${j.request?.url} → ${flag}  ${dim(`${j.elapsedMs}ms`)}`);
    } catch {
      console.log(dim(`    ${l.slice(0, 90)}`));
    }
  }
}

async function doRefresh() {
  console.log("  刷新凭证（复制 Cookies → 读账号 → 换 token → 落盘）");
  const r = await ensureCredentials({ sid: config.credentials.sid, force: true, log: (m) => console.log(dim(`    ${m}`)) });
  if (!r.ok) {
    console.log(`  ${bad("❌ 失败")}（${r.action}）：${r.error}`);
    if (/passToken/.test(r.error || "")) console.log(warn("  提示：需要在 MiMo 客户端重新登录一次"));
    return false;
  }
  for (const s of r.steps) console.log(dim(`    · ${s}`));
  console.log(`  ${ok("✅ 凭证就绪")}  （服务每请求实时读取，无需重启）`);
  return true;
}

async function doProbe() {
  if (!(await serverMeta())) {
    console.log(`  ${bad("❌ 服务没在运行")} —— 先选 1 启动`);
    return false;
  }
  try {
    const res = await fetch(CHAT_URL, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({
        model: config.client.model,
        messages: [{ role: "user", content: "say OK" }],
        max_tokens: config.client.maxTokens,
      }),
      signal: AbortSignal.timeout(60000),
    });
    const text = await res.text();
    if (res.status !== 200) {
      console.log(`  ${bad(`❌ HTTP ${res.status}`)}  ${text.slice(0, 160)}`);
      if (res.status === 401) console.log(warn("  → 凭证失效，选 5 刷新"));
      return false;
    }
    const j = JSON.parse(text);
    const m = j.choices?.[0]?.message || {};
    console.log(`  ${ok("✅ HTTP 200")}  ${j.model}  content=${JSON.stringify(m.content ?? m.reasoning_content ?? "")}`);
    return true;
  } catch (e) {
    console.log(`  ${bad("❌")} ${e.message}`);
    return false;
  }
}

/**
 * 统一的输入源。interactive() 启动时把 readline 的 async 迭代器挂到这里，
 * 这样 doAsk() 这类模块级动作也能借它读一行，而不用自己再开一个 readline
 * （同一个 stdin 上开两个 interface 会互相抢输入）。
 */
let INPUT = null;

/** 写提示并读一行；输入结束返回 null。 */
async function ask(prompt) {
  if (!INPUT) return null;
  process.stdout.write(prompt);
  const { value, done } = await INPUT.next();
  return done ? null : String(value);
}

function doAsk() {
  return new Promise((resolve) => {
    (async () => {
      const q = await ask("  输入要问的话（直接回车取消）: ");
      const text = (q || "").trim();
      if (!text) return resolve();
      if (!(await serverMeta())) {
        console.log(`  ${bad("❌ 服务没在运行")}`);
        return resolve();
      }
      process.stdout.write(dim("  思考中…\n"));
      try {
        const res = await fetch(CHAT_URL, {
          method: "POST",
          headers: { "content-type": "application/json" },
          body: JSON.stringify({
            model: config.client.model,
            messages: [{ role: "user", content: text }],
            max_tokens: config.client.maxTokens,
          }),
          signal: AbortSignal.timeout(120000),
        });
        const j = JSON.parse(await res.text());
        if (!res.ok) {
          console.log(`  ${bad(`❌ HTTP ${res.status}`)} ${JSON.stringify(j.error?.message || j).slice(0, 160)}`);
        } else {
          const m = j.choices?.[0]?.message || {};
          console.log(`  ${c("cyan", j.model)} ${dim(`${j.usage?.total_tokens} tokens`)}`);
          console.log(`  ${m.content || dim("[只有思维链，max_tokens 可能不够]") + "\n  " + (m.reasoning_content || "")}`);
        }
      } catch (e) {
        console.log(`  ${bad("❌")} ${e.message}`);
      }
      resolve();
    })();
  });
}

function doShowLog() {
  const { file, lines } = lastCaptureLines(20);
  if (!lines.length) {
    console.log(dim(`  ${path.relative(ROOT, file)} 还没有内容`));
    return;
  }
  console.log(dim(`  ${path.relative(ROOT, file)}  最后 ${lines.length} 条`));
  for (const l of lines) {
    try {
      const j = JSON.parse(l);
      console.log(
        `  ${dim((j.ts || "").slice(11, 19))} ${j.request?.method} ${j.request?.url} → ` +
          `${j.response?.status ?? j.error}  ${dim(`${j.elapsedMs}ms`)}  ${dim(`trace=${j.response?.headers?.["x-trace-id"] || "-"}`)}`
      );
    } catch {
      console.log(`  ${l.slice(0, 120)}`);
    }
  }
}

/* ------------------------------------------------------------------ 能力自检 */

async function chat(body, { stream = false } = {}) {
  const t0 = Date.now();
  const res = await fetch(CHAT_URL, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(body),
    signal: AbortSignal.timeout(180000),
  });
  const text = await res.text();
  return { status: res.status, ms: Date.now() - t0, text, stream };
}

function sseData(text) {
  const out = [];
  for (const line of text.split(/\r?\n/)) {
    const m = /^data:\s*(.*)$/.exec(line);
    if (!m || m[1] === "[DONE]") continue;
    try { out.push(JSON.parse(m[1])); } catch {}
  }
  return out;
}

/**
 * 逐项打上游，验证 tools / web_search 真的能用。
 * 都是真实计费请求，所以只挑最省的几发（TEXT 模型目前免费额度内）。
 */
async function doCaps() {
  if (!(await serverMeta())) {
    console.log(`  ${bad("❌ 服务没在运行")} —— 先选 1 启动`);
    return false;
  }
  const model = config.client.model;
  let pass = 0, fail = 0;
  const line = (name, okFlag, detail) => {
    console.log(`  ${okFlag ? ok("✅") : bad("❌")} ${name.padEnd(22)} ${dim(detail)}`);
    okFlag ? pass++ : fail++;
  };
  console.log(dim(`  模型 ${model}，共 3 项（工具调用 / 流式分片 / 联网搜索）\n`));

  const F = {
    type: "function",
    function: {
      name: "get_weather",
      description: "查询城市天气",
      parameters: { type: "object", properties: { city: { type: "string" } }, required: ["city"] },
    },
  };

  // ① 非流式工具调用
  {
    const r = await chat({ model, messages: [{ role: "user", content: "北京天气？调用工具。" }], tools: [F], max_tokens: 512 });
    let j = null;
    try { j = JSON.parse(r.text); } catch {}
    const tc = j?.choices?.[0]?.message?.tool_calls?.[0];
    line(
      "① 工具调用",
      r.status === 200 && !!tc,
      r.status !== 200
        ? `HTTP ${r.status} ${r.text.slice(0, 90)}`
        : tc
          ? `${tc.function.name}(${tc.function.arguments})  ${r.ms}ms`
          : `没有 tool_calls，finish=${j?.choices?.[0]?.finish_reason}`
    );
  }

  // ② 流式工具分片能否拼回完整 arguments
  {
    const r = await chat(
      { model, messages: [{ role: "user", content: "上海天气？调用工具。" }], tools: [F], stream: true, max_tokens: 512 },
      { stream: true }
    );
    const evs = sseData(r.text);
    const tcs = evs.filter((e) => e.choices?.[0]?.delta?.tool_calls);
    let name = "", args = "";
    for (const e of tcs) for (const t of e.choices[0].delta.tool_calls) {
      if (t.function?.name) name += t.function.name;
      if (t.function?.arguments) args += t.function.arguments;
    }
    let parsed = false;
    try { JSON.parse(args); parsed = true; } catch {}
    line(
      "② 流式工具分片",
      r.status === 200 && !!name && parsed,
      r.status !== 200 ? `HTTP ${r.status}` : `${tcs.length} 个分片 → ${name}(${args})  ${r.ms}ms`
    );
  }

  // ③ 联网搜索（走反代兼容层，客户端只要写 web_search: true）
  {
    const r = await chat({ model, messages: [{ role: "user", content: "搜索：今天有什么科技新闻？" }], web_search: true, max_tokens: 600 });
    let j = null;
    try { j = JSON.parse(r.text); } catch {}
    const ann = j?.choices?.[0]?.message?.annotations || [];
    const wsu = j?.usage?.web_search_usage;
    line(
      "③ 联网搜索",
      r.status === 200 && ann.length > 0,
      r.status !== 200
        ? `HTTP ${r.status} ${r.text.slice(0, 90)}`
        : ann.length
          ? `${ann.length} 条引用，web_search_usage=${JSON.stringify(wsu)}  ${r.ms}ms`
          : `没有 annotations（prompt_tokens=${j?.usage?.prompt_tokens}，可能是没触发搜索）`
    );
  }

  console.log("");
  return fail === 0;
}

/* ------------------------------------------------------------------ 菜单 */

/** 终端才清屏；管道/重定向时不清，免得往日志里塞控制字符。XM2API_NO_CLEAR=1 可关掉。 */
const WANT_CLEAR = process.stdout.isTTY === true && !process.env.XM2API_NO_CLEAR;

function clearScreen() {
  if (!WANT_CLEAR) return;
  // 2J 清屏、3J 清回滚缓冲、H 光标归位
  process.stdout.write("\x1b[2J\x1b[3J\x1b[H");
}

async function printHeader() {
  const port = await portState(1200);
  const creds = credentialsStatus();
  const svc =
    port.state === "ours"
      ? ok(`运行中 http://${HOST}:${PORT}`)
      : port.state === "other"
        ? bad(`端口 ${PORT} 被别的程序占用（${port.detail}）`)
        : bad("已停止");
  console.log("");
  console.log(c("cyan", "  ╔══════════════════════════════════════════╗"));
  console.log(c("cyan", "  ║        xm2api 线路2 反代  控制台         ║"));
  console.log(c("cyan", "  ╚══════════════════════════════════════════╝"));
  console.log(`   服务 : ${svc}`);
  console.log(`   凭证 : ${creds.hasRouteCookie ? ok("就绪") : bad("缺失")}${creds.sid ? dim(`  sid=${creds.sid}`) : ""}`);
  console.log(`   配置 : ${dim(path.relative(ROOT, CONFIG_PATH))}`);
  console.log("");
  console.log("   1  启动服务          2  停止服务");
  console.log("   3  重启              4  状态 / 最近请求");
  console.log("   5  刷新凭证          6  健康检查");
  console.log("   7  试问一句          8  查看抓包日志");
  console.log("   9  能力自检（工具调用 / 联网搜索）");
  console.log("   0  退出");
  console.log("");
}

const ACTIONS = {
  1: doStart,
  2: doStop,
  3: async () => (await doStop()) && (await doStart()),
  4: doStatus,
  5: doRefresh,
  6: doProbe,
  7: doAsk,
  8: doShowLog,
  9: doCaps,
};

/**
 * 用 async 迭代器消费输入，而不是 rl.question：
 *   - rl.question 在输入被一次性喂入（管道/重定向）时会丢掉已缓冲的行；
 *   - 迭代器在 stdin 结束时干净地返回 done，不会留下不落地的 Promise。
 */
async function interactive() {
  const isTty = process.stdin.isTTY === true;
  if (!isTty) {
    console.log(warn("  提示：stdin 不是终端（管道/重定向）。菜单只消费已喂入的行，用完即退出。"));
  }
  const rl = readline.createInterface({
    input: process.stdin,
    output: process.stdout,
    terminal: isTty,
  });
  const it = rl[Symbol.asyncIterator]();
  INPUT = it; // 供 doAsk() 等模块级动作复用同一个输入源
  rl.on("SIGINT", () => {
    console.log("");
    rl.close();
  });

  for (;;) {
    clearScreen();
    await printHeader();
    const answer = await ask("  选择: ");
    if (answer === null) {
      console.log(warn("  （输入结束，退出）"));
      break;
    }
    const key = answer.trim();
    if (key === "0" || key.toLowerCase() === "q") break;
    const action = ACTIONS[key];
    if (!action) {
      console.log(warn("  无效选择"));
    } else {
      console.log("");
      try {
        await action();
      } catch (e) {
        console.log(`  ${bad("❌")} ${e.message}`);
      }
    }
    const pause = await ask(dim("\n  回车返回菜单…"));
    if (pause === null) {
      console.log(warn("  （输入结束，退出）"));
      break;
    }
  }
  INPUT = null;
  rl.close();
  console.log(dim("  再见"));
}

/* ------------------------------------------------------------------ 入口 */

const cmd = (process.argv[2] || "").toLowerCase();
const CLI = {
  start: doStart,
  stop: doStop,
  restart: ACTIONS[3],
  status: doStatus,
  probe: doProbe,
  refresh: doRefresh,
  log: doShowLog,
  caps: doCaps,
};

if (cmd) {
  const fn = CLI[cmd];
  if (!fn) {
    console.log(`用法: node menu.mjs [${Object.keys(CLI).join("|")}]   不带参数则进交互菜单`);
    process.exitCode = 2;
  } else {
    const r = await fn();
    process.exitCode = r === false ? 1 : 0;
  }
} else {
  await interactive();
}
