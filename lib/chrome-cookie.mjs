/**
 * Chromium cookie crypto helpers for Xiaomi MiMo Desktop — 线路2 only.
 * AES-256-GCM layout: b"v10"|"v11" + nonce(12) + ciphertext + tag(16)
 * Key comes from Local State os_crypt.encrypted_key → DPAPI unwrap
 * (see scripts/02-copy-and-unwrap.py).
 *
 * NOTE: on the current client build the cookies are stored PLAINTEXT
 * (encrypted_value is empty), so the AES/DPAPI path is not required to read them.
 */
import fs from "node:fs";
import path from "node:path";
import crypto from "node:crypto";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";

const __dirname = path.dirname(fileURLToPath(import.meta.url));
/** 线路2 root = <project>/path2 */
export const ROOT = path.resolve(__dirname, "..");
export const DATA_DIR = path.join(ROOT, "data");
/** @deprecated alias — data used to live in <project>/data/path2 */
export const PATH2_DIR = DATA_DIR;
export const KEY_PATH = path.join(DATA_DIR, "chrome-aes-key.bin");
export const COOKIES_COPY = path.join(DATA_DIR, "Cookies");
export const SESSION_OUT = path.join(DATA_DIR, "sso-session.json");
export const LOCAL_STATE_COPY = path.join(DATA_DIR, "Local-State.json");

export const MIMO_USERDATA =
  process.env.MIMO_USERDATA || path.join(process.env.APPDATA || "", "Xiaomi MiMo");

export const XIAOMI_COOKIES_SRC = path.join(
  MIMO_USERDATA,
  "Partitions",
  "xiaomi-account",
  "Network",
  "Cookies"
);
export const XIAOMI_LS_SRC = path.join(MIMO_USERDATA, "Local State");

export const INTERESTING_HOSTS = [
  "xiaomi.com",
  "account.xiaomi.com",
  "xiaomimimo.com",
  "mimo-server-cn.xiaomimimo.com",
  "api.xiaomimimo.com",
];

export const INTERESTING_NAMES = [
  "passToken",
  "serviceToken",
  "userId",
  "cUserId",
  "userId",
  "mimopc_slh",
  "mimopc_ph",
  "_serviceToken",
  "ph",
];

export function ensureDir(p) {
  fs.mkdirSync(p, { recursive: true });
  return p;
}

export function loadAesKey(keyPath = KEY_PATH) {
  if (!fs.existsSync(keyPath)) {
    throw new Error(`AES key missing: ${keyPath} (run path2/scripts/02-unwrap-key.py first)`);
  }
  const key = fs.readFileSync(keyPath);
  if (key.length !== 32) throw new Error(`AES key length ${key.length}, expected 32`);
  return key;
}

/** Decrypt one Chromium encrypted_value blob. Returns Buffer or null. */
export function decryptChromiumValue(blob, key) {
  if (!blob || blob.length < 3 + 12 + 16) return null;
  const prefix = blob.subarray(0, 3).toString("latin1");
  if (prefix !== "v10" && prefix !== "v11") return null;
  const nonce = blob.subarray(3, 15);
  const ctTag = blob.subarray(15);
  const tag = ctTag.subarray(ctTag.length - 16);
  const ct = ctTag.subarray(0, ctTag.length - 16);
  try {
    const d = crypto.createDecipheriv("aes-256-gcm", key, nonce);
    d.setAuthTag(tag);
    return Buffer.concat([d.update(ct), d.final()]);
  } catch {
    return null;
  }
}

export function hostMatches(host, needles) {
  const h = String(host || "").toLowerCase();
  return needles.some((n) => h === n || h.endsWith(`.${n}`) || h.includes(n));
}

export function nameMatches(name, needles) {
  const n = String(name || "").toLowerCase();
  return needles.some((x) => n === x.toLowerCase() || n.includes(x.toLowerCase()));
}

export function buildCookieHeader(rows, { hosts = INTERESTING_HOSTS } = {}) {
  const parts = [];
  const seen = new Set();
  for (const row of rows) {
    if (!row?.value) continue;
    const host = row.host_key || row.host || "";
    const name = row.name;
    if (hosts?.length && !hostMatches(host, hosts)) continue;
    const key = `${name}=${row.value}`;
    if (seen.has(key)) continue;
    seen.add(key);
    parts.push(`${name}=${row.value}`);
  }
  return parts.join("; ");
}

/** Persist a reusable SSO session object for proxy injection. */
export function writeSsoSession(session) {
  ensureDir(DATA_DIR);
  const payload = {
    ...session,
    updatedAt: new Date().toISOString(),
  };
  fs.writeFileSync(SESSION_OUT, JSON.stringify(payload, null, 2) + "\n", "utf8");
  return payload;
}

export function readSsoSession(file = SESSION_OUT) {
  try {
    return JSON.parse(fs.readFileSync(file, "utf8"));
  } catch {
    return null;
  }
}

export function listMimoProcesses() {
  try {
    const r = spawnSync("tasklist", ["/FI", "IMAGENAME eq Xiaomi MiMo.exe", "/FO", "CSV", "/NH"], {
      encoding: "utf8",
    });
    const lines = String(r.stdout || "")
      .split(/\r?\n/)
      .filter((l) => l.includes("Xiaomi MiMo"));
    return lines.map((l) => {
      const parts = l.replace(/"/g, "").split(",");
      return { name: parts[0], pid: Number(parts[1]) };
    });
  } catch {
    return [];
  }
}
