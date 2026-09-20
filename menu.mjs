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
 *
 * 也可以双击 start.bat（它只是本脚本的入口）。
 */
import fs from "node:fs";
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
  if (await serverMeta()) {
    console.log(`  ${warn("服务已在运行")} ${META_URL}`);
    const pid = readPid();
    if (pid) console.log(dim(`  pid=${pid}`));
    return true;
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
  try {
    const tail = fs.readFileSync(STDERR_LOG, "utf8").trim().split(/\r?\n/).slice(-6);
    if (tail.length) for (const l of tail) console.log(dim(`  ${l}`));
  } catch {}
  return false;
}

async function doStop() {
  const alive = await serverMeta();
  const pid = readPid();
  if (!alive && !pid) {
    console.log(`  ${warn("服务没在运行")}`);
    return true;
  }
  if (pid) {
    try {
      process.kill(pid);
      console.log(dim(`  已向 pid=${pid} 发送终止信号`));
    } catch (e) {
      console.log(dim(`  kill(${pid}) 失败：${e.message}`));
    }
  }
  for (let i = 0; i < 20; i++) {
    await new Promise((r) => setTimeout(r, 200));
    if (!(await serverMeta(600))) {
      fs.rmSync(PID_FILE, { force: true });
      console.log(`  ${ok("✅ 已停止")}  端口 ${PORT} 已释放`);
      return true;
    }
  }
  console.log(`  ${bad("❌ 仍在响应")} —— 可能不是本脚本启动的进程`);
  if (pid) console.log(dim(`  可手动：taskkill /PID ${pid} /F`));
  else console.log(dim(`  端口占用者不是本脚本记录的 pid，可用 netstat -ano | findstr :${PORT} 查`));
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
  console.log(`    模型      : ${config.server.models.join(", ")}`);
  console.log(
    `    日志      : ${config.logging.enabled ? "开" : "关"}` +
      `，body ${config.logging.captureBody ? "记录" : "不记录"}`
  );

  console.log(c("cyan", "  服务"));
  if (meta) {
    console.log(`    状态      : ${ok("✅ 运行中")}${pid ? dim(`  pid=${pid}`) : ""}`);
    console.log(`    服务侧凭证: present=${meta.credentials?.present} sid=${meta.credentials?.sid ?? "-"}`);
  } else {
    console.log(`    状态      : ${bad("❌ 未运行")}`);
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
 * 包一层的 question：stdin 到 EOF / 被 Ctrl+C 关掉时 resolve(null)，
 * 否则 rl.question 的 Promise 永远不落地，顶层 await 会挂住（exit 13）。
 */

function doAsk(rl) {
  return new Promise((resolve) => {
    (async () => {
      const q = await ask(rl, "  输入要问的话（直接回车取消）: ");
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

/* ------------------------------------------------------------------ 菜单 */

async function printHeader() {
  const meta = await serverMeta(1200);
  const creds = credentialsStatus();
  console.log("");
  console.log(c("cyan", "  ╔══════════════════════════════════════════╗"));
  console.log(c("cyan", "  ║        xm2api 线路2 反代  控制台         ║"));
  console.log(c("cyan", "  ╚══════════════════════════════════════════╝"));
  console.log(`   服务 : ${meta ? ok(`运行中 http://${HOST}:${PORT}`) : bad("已停止")}`);
  console.log(`   凭证 : ${creds.hasRouteCookie ? ok("就绪") : bad("缺失")}${creds.sid ? dim(`  sid=${creds.sid}`) : ""}`);
  console.log(`   配置 : ${dim(path.relative(ROOT, CONFIG_PATH))}`);
  console.log("");
  console.log("   1  启动服务          2  停止服务");
  console.log("   3  重启              4  状态 / 最近请求");
  console.log("   5  刷新凭证          6  健康检查");
  console.log("   7  试问一句          8  查看抓包日志");
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
};

/**
 * 用 async 迭代器消费输入，而不是 rl.question：
 *   - rl.question 在输入被一次性喂入（管道/重定向）时会丢掉已缓冲的行；
 *   - 迭代器在 stdin 结束时干净地返回 done，不会留下不落地的 Promise。
 */
async function interactive() {
  const rl = readline.createInterface({
    input: process.stdin,
    output: process.stdout,
    terminal: process.stdin.isTTY === true,
  });
  const it = rl[Symbol.asyncIterator]();
  rl.on("SIGINT", () => {
    console.log("");
    rl.close();
  });

  const ask = async (prompt) => {
    process.stdout.write(prompt);
    const { value, done } = await it.next();
    return done ? null : String(value);
  };

  for (;;) {
    await printHeader();
    const answer = await ask("  选择: ");
    if (answer === null) break; // stdin 结束
    const key = answer.trim();
    if (key === "0" || key.toLowerCase() === "q") break;
    const action = ACTIONS[key];
    if (!action) {
      console.log(warn("  无效选择"));
    } else {
      console.log("");
      try {
        await action(rl);
      } catch (e) {
        console.log(`  ${bad("❌")} ${e.message}`);
      }
    }
    const pause = await ask(dim("\n  回车返回菜单…"));
    if (pause === null) break;
  }
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
