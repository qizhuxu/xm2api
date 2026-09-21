#!/usr/bin/env node
/**
 * CPA 插件认证文件导出工具 —— Linux 部署「方案一」：Win 本机导出，手动上传。
 *
 * 在已登录小米 MiMo Desktop 的 Windows 本机上运行。从客户端的 Chromium
 * Cookie 库提取 passToken / userId / cUserId，导出 cpa-plugin 认识的 mimo.json：
 *
 *   {"type":"mimo","pass_token":"…","user_id":"…","c_user_id":"…"}
 *
 * 默认顺手做一次 SSO 两阶段交换（sid=mimopc），一举两得：
 *   - 校验 passToken 是否活着（换不出 serviceToken 就别上传了）；
 *   - 把换到的 service_token 一并写入 → 插件开箱即用，不用等首次续期。
 *
 * 用法：
 *   node cpa-auth.mjs                  导出到 data/mimo.json
 *   node cpa-auth.mjs --out D:\mimo.json
 *   node cpa-auth.mjs --sid mimopc     指定 sid（默认 mimopc）
 *   node cpa-auth.mjs --skip-exchange  离线导出，不做 SSO 校验
 *
 * 部署（Linux 上的 CLIProxyAPI）：
 *   scp data/mimo.json user@server:/path/to/cli-proxy-api/auth/mimo.json
 *   CPA 监听 auth 目录，落文件即生效（无反应就重启 cli-proxy-api）。
 *   验证：curl -H "X-Management-Key: <管理密钥>" http://server:port/v0/management/plugins/mimo/status
 *
 * ⚠️ mimo.json 里的 passToken 是长期凭证，语义等同账号通行证：
 *   - 传输只走 scp/sftp，别走聊天工具/网盘；
 *   - 上传完成后删除本机导出文件；
 *   - MiMo 客户端重新登录后 passToken 会换新，届时需要重新导出上传。
 */
import fs from "node:fs";
import path from "node:path";
import { DEFAULT_SID, copyCookies, exchangeServiceToken, extractAccount } from "./lib/pipeline.mjs";

const args = process.argv.slice(2);
const opt = (f, d) => {
  const i = args.indexOf(f);
  return i >= 0 && args[i + 1] ? args[i + 1] : d;
};

const SID = opt("--sid", DEFAULT_SID);
const OUT = path.resolve(opt("--out", "data/mimo.json"));
const SKIP_EXCHANGE = args.includes("--skip-exchange");

const log = (m) => console.log(`  ${m}`);
// 只报长度与前 6 位，凭证值绝不进终端/日志
const mask = (v) => (v ? `${v.slice(0, 6)}…（${v.length} 字符）` : "缺失");

async function main() {
  console.log("CPA 插件认证文件导出（mimo.json）");

  console.log("· 复制 MiMo 客户端 Cookies 库…");
  const copy = copyCookies({ force: true });
  if (!copy.ok) {
    log(`❌ ${copy.error}`);
    return 1;
  }
  log(`✅ ${copy.method}, ${copy.bytes} 字节`);
  if (copy.warning) log(`⚠️ ${copy.warning}`);

  console.log("· 提取账号凭证…");
  const acc = extractAccount();
  if (!acc.ok) {
    log(`❌ ${acc.error}`);
    if (/passToken|未登录/.test(acc.error || "")) {
      log("提示：先在 MiMo Desktop 客户端登录一次，再重新导出。");
    }
    return 1;
  }
  const t = acc.account;
  log(`✅ passToken ${mask(t.passToken?.value)}  userId=${t.userId?.value ?? "缺失"}`);

  const out = {
    type: "mimo",
    pass_token: t.passToken?.value ?? "",
    user_id: t.userId?.value ?? "",
    c_user_id: t.cUserId?.value ?? "",
    sid: SID,
    obtained_at: new Date().toISOString(),
  };

  if (!SKIP_EXCHANGE) {
    console.log(`· SSO 两阶段交换（sid=${SID}）校验 passToken…`);
    const sso = await exchangeServiceToken(SID, t);
    if (!sso.ok) {
      log(`❌ ${sso.error}`);
      log("passToken 可能已过期：在 MiMo Desktop 里重新登录一次再导出。");
      log("（确定离线导出可加 --skip-exchange，但插件要等首次续期成功才能用）");
      return 1;
    }
    out.service_token = sso.serviceToken;
    log(`✅ serviceToken ${mask(sso.serviceToken)} —— 一并写入，插件开箱即用`);
  }

  fs.mkdirSync(path.dirname(OUT), { recursive: true });
  fs.writeFileSync(OUT, JSON.stringify(out, null, 2) + "\n", { mode: 0o600 });
  console.log(`✅ 已写入 ${OUT}`);
  console.log(`
下一步（Linux CPA 服务器）：
  scp "${OUT}" user@server:/path/to/cli-proxy-api/auth/mimo.json
  CPA 监听 auth 目录，落文件即生效；无反应则重启 cli-proxy-api。
  验证：curl -H "X-Management-Key: <管理密钥>" http://server:port/v0/management/plugins/mimo/status

⚠️ mimo.json 含长期凭证 passToken：传输用 scp/sftp；上传后删除本机文件；
   MiMo 客户端重新登录会换新 passToken，届时重新导出上传。`);
  return 0;
}

main()
  .then((code) => {
    process.exitCode = code;
  })
  .catch((err) => {
    console.error("cpa-auth: fatal:", err);
    process.exitCode = 1;
  });
