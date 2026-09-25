/**
 * 用量数据采集（三期图表的数据层）：
 *
 *   - data/usage-history.json  额度历史：每账号一条时间线（剩余 %），
 *     10 分钟节流去重、每账号上限 1000 点（环形淘汰）——额度是慢变量，够画 30 天趋势；
 *   - data/usage-daily.json    每日请求计数（转发侧 onResult 汇总）：
 *     { "2026-09-24": { requests, errors } }，只留最近 60 天。
 *
 * 两个文件都是运行数据（gitignored），读写都是小 JSON，直接同步 IO（量级 <100KB）。
 */
import fs from "node:fs";
import path from "node:path";
import { DATA_DIR } from "./chrome-cookie.mjs";

const HISTORY_FILE = path.join(DATA_DIR, "usage-history.json");
const DAILY_FILE = path.join(DATA_DIR, "usage-daily.json");

const HISTORY_CAP_PER_ACCOUNT = 1000;
const DAILY_KEEP_DAYS = 60;
const SNAPSHOT_THROTTLE_MS = 10 * 60 * 1000; // 同账号 10 分钟内只记一点

function readJson(file, fallback) {
  try {
    return JSON.parse(fs.readFileSync(file, "utf8"));
  } catch {
    return fallback;
  }
}

function writeJson(file, obj) {
  fs.mkdirSync(path.dirname(file), { recursive: true });
  fs.writeFileSync(file, JSON.stringify(obj) + "\n");
}

/**
 * 记一个额度快照（内部已按 10 分钟节流）。
 * @param {string} id 账号 id（或 "session" 表示当前转发会话）
 */
export function recordUsageSnapshot(id, percent, resetDate) {
  if (typeof percent !== "number" || !isFinite(percent)) return;
  const key = String(id || "session");
  const hist = readJson(HISTORY_FILE, {});
  const arr = Array.isArray(hist[key]) ? hist[key] : [];
  const now = Date.now();
  const last = arr[arr.length - 1];
  if (last && now - Number(last.t) < SNAPSHOT_THROTTLE_MS) return; // 节流
  arr.push({ t: now, p: Math.round(percent * 10) / 10, r: resetDate || "" });
  while (arr.length > HISTORY_CAP_PER_ACCOUNT) arr.shift();
  hist[key] = arr;
  writeJson(HISTORY_FILE, hist);
}

/** 全部账号的额度历史（图表用）。 */
export function usageHistory() {
  return readJson(HISTORY_FILE, {});
}

/** 每日请求计数 +1。ok=false 记一次错误。 */
export function recordRequest(ok) {
  const day = new Date().toISOString().slice(0, 10);
  const daily = readJson(DAILY_FILE, {});
  if (!daily[day]) daily[day] = { requests: 0, errors: 0 };
  daily[day].requests++;
  if (!ok) daily[day].errors++;
  for (const k of Object.keys(daily)) {
    if (Object.keys(daily).length > DAILY_KEEP_DAYS && k < day) delete daily[k];
  }
  writeJson(DAILY_FILE, daily);
}

/** 近 n 天的每日请求序列（缺失补零）。 */
export function usageDaily(days = 30) {
  const daily = readJson(DAILY_FILE, {});
  const out = [];
  for (let i = days - 1; i >= 0; i--) {
    const d = new Date(Date.now() - i * 86400000).toISOString().slice(0, 10);
    out.push({ date: d, requests: daily[d]?.requests || 0, errors: daily[d]?.errors || 0 });
  }
  return out;
}
