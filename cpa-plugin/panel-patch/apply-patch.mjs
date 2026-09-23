#!/usr/bin/env node
/**
 * 把 mimo-quota-patch.html 注入 CPA 官方面板 static/management.html。
 *
 * 为什么要有这个脚本（而不是 README 里那段手写 python）：
 *   v7.4.1 把版本标记写成 `window.__mimoQuotaPatch=7.4.1`——`7.4.1` 不是合法
 *   数字字面量，整个 <script> 解析失败，补丁一行都没跑：卡片消失、刷新按钮
 *   消失，页面上只留一句控制台错误。手写注入没有任何校验，所以静默炸了。
 *   这里在写盘**之前**用 node:vm 解析每个内联脚本（只解析、不执行），语法错
 *   就直接中止、原文件不动。顺带做幂等检查（勿在旧补丁上叠加）。
 *
 * 用法：
 *   node apply-patch.mjs                       # 默认 bin: %TEMP%\cpa-test\bin
 *   node apply-patch.mjs --bin "D:\cpa\bin"
 *   node apply-patch.mjs --dry-run             # 只校验，不写盘
 *   node apply-patch.mjs --restore             # 只还原干净原版
 *   node apply-patch.mjs --head                # 换注入点（默认插在首个 <head> 后）
 *
 * 干净来源优先级：static/management.html.orig → static/mgmt-unpatched.html
 * 两者都没有就报错（避免把补丁叠在补丁上；此时请重新下载官方面板）。
 */
import fs from "node:fs";
import path from "node:path";
import vm from "node:vm";
import { fileURLToPath } from "node:url";

const HERE = path.dirname(fileURLToPath(import.meta.url));
const args = process.argv.slice(2);
const has = (f) => args.includes(f);
const opt = (f, d) => {
  const i = args.indexOf(f);
  return i >= 0 && args[i + 1] ? args[i + 1] : d;
};

const DRY = has("--dry-run");
const PATCH_FILE = path.resolve(opt("--patch", path.join(HERE, "mimo-quota-patch.html")));
const BIN = path.resolve(
  opt("--bin", path.join(process.env.TEMP || process.env.TMP || ".", "cpa-test", "bin"))
);

const ok = (s) => console.log(`  ✅ ${s}`);
const info = (s) => console.log(`  ·  ${s}`);
const fail = (s) => {
  console.error(`  ❌ ${s}`);
  process.exit(1);
};

/* ---------------------------------------------- 1) 内联脚本语法校验 */

/** 去掉 HTML 注释：补丁头部的说明里就写着 `<script>`/`</script>` 字样，
 *  不先剥掉会把注释文本当成脚本内容（v7.5 首次自检就踩了这个）。 */
function stripHtmlComments(html) {
  return html.replace(/<!--[\s\S]*?-->/g, "");
}

/** 抽出 patch 里所有 <script>…</script> 的内容（不含 src= 的外链） */
function inlineScripts(html) {
  const out = [];
  const re = /<script\b([^>]*)>([\s\S]*?)<\/script>/gi;
  let m;
  const text = stripHtmlComments(html);
  while ((m = re.exec(text))) {
    if (/\bsrc\s*=/i.test(m[1])) continue;
    out.push(m[2]);
  }
  return out;
}

/**
 * 语法校验：vm.Script 以「脚本」身份解析（不是函数体），
 * 所以顶层 return 之类也会被抓出来。只解析，不执行。
 */
function assertPatchParses(patchText) {
  const scripts = inlineScripts(patchText);
  if (!scripts.length) fail("补丁里没有内联 <script>，注入它没有意义");
  scripts.forEach((src, i) => {
    try {
      new vm.Script(src, { filename: `mimo-quota-patch.html#script[${i}]` });
    } catch (err) {
      const ln = Number((String(err.stack).match(/#script\[\d+\]:(\d+)/) || [])[1] || 0);
      const lines = src.split("\n");
      const ctx = ln
        ? "\n" +
          lines
            .slice(Math.max(0, ln - 3), ln)
            .map((l, k) => `       ${Math.max(1, ln - 2) + k}: ${l}`)
            .join("\n")
        : "";
      fail(`补丁内联脚本[${i}]语法错误：${err.message}${ctx}\n     （写盘已中止，原文件未改动）`);
    }
  });
  ok(`内联脚本语法校验通过（${scripts.length} 段，vm.Script 解析）`);
}

/* ------------------------------------------------------ 2) 定位文件 */

const HTML = path.join(BIN, "static", "management.html");
const CANDIDATES = [
  path.join(BIN, "static", "management.html.orig"),
  path.join(BIN, "static", "mgmt-unpatched.html"),
];

const MARKER = "__mimoQuotaPatch";
/** 只认真正的代码行（if(...)return; window.__mimoQuotaPatch=<值>;），
 *  头部注释里也写着这个赋值，别把说明文字当版本号。 */
const PATCH_VERSION = (() => {
  const m = fs
    .readFileSync(PATCH_FILE, "utf8")
    .match(/if\s*\(\s*window\.__mimoQuotaPatch\s*\)\s*return;\s*window\.__mimoQuotaPatch\s*=\s*([^;]+);/);
  return m ? m[1].trim() : "(未找到版本标记)";
})();

if (!fs.existsSync(PATCH_FILE)) fail(`找不到补丁文件：${PATCH_FILE}`);
const patch = fs.readFileSync(PATCH_FILE, "utf8");
if (!fs.existsSync(path.dirname(HTML))) fail(`找不到面板目录：${path.dirname(HTML)}`);

console.log(`补丁：${PATCH_FILE}`);
info(`版本标记：window.${MARKER}=${PATCH_VERSION}`);
assertPatchParses(patch);

/** 干净原版：不含补丁标记的第一顺位候选 */
const cleanFile = CANDIDATES.find(
  (f) => fs.existsSync(f) && !fs.readFileSync(f, "utf8").includes(MARKER)
);
if (!cleanFile) {
  fail(
    `没有可用的干净原版（${CANDIDATES.map((f) => path.basename(f)).join(" / ")} 都不存在或已含补丁）。\n` +
      `     请先从 CPA 发行包/备份恢复一份干净的 static/management.html.orig，勿在旧补丁上叠加。`
  );
}
const clean = fs.readFileSync(cleanFile, "utf8");
info(`干净来源：${cleanFile}（${Buffer.byteLength(clean, "utf8")} 字节）`);

/* ------------------------------------------------------ 3) 注入 */

if (has("--restore")) {
  if (DRY) {
    console.log("  (dry-run) 只校验，不还原");
    process.exit(0);
  }
  if (fs.existsSync(HTML)) fs.copyFileSync(HTML, `${HTML}.prev`);
  fs.writeFileSync(HTML, clean);
  ok(`已还原干净原版（旧文件备份到 ${path.basename(HTML)}.prev）`);
  process.exit(0);
}

const headRe = /<head[^>]*>/i;
if (!headRe.test(clean)) fail("干净原版里找不到 <head>，注入点不可用");
const injected = clean.replace(headRe, (m) => `${m}\n${patch}`);

const scriptsInjected = inlineScripts(injected).length;
const bClean = Buffer.byteLength(clean, "utf8"), bInj = Buffer.byteLength(injected, "utf8");
info(`注入后内联脚本数：${scriptsInjected}（原 ${inlineScripts(clean).length}）`);
info(`体积：${bClean} → ${bInj} 字节（+${bInj - bClean}）`);

if (DRY) {
  ok("dry-run：校验通过，未写盘");
  process.exit(0);
}

/* 备份当前版本（若已打过旧补丁，留一份 .prev 方便回滚） */
if (fs.existsSync(HTML)) {
  const cur = fs.readFileSync(HTML, "utf8");
  if (cur.includes(MARKER)) {
    fs.copyFileSync(HTML, `${HTML}.prev`);
    info(`旧补丁已备份到 ${path.basename(HTML)}.prev`);
  }
}
fs.writeFileSync(HTML, injected);

/* ------------------------------------------------------ 4) 写后复检 */

const written = fs.readFileSync(HTML, "utf8");
if (!written.includes(MARKER)) fail("写盘后复检失败：文件里没有补丁标记");
/* 只复检**我们注入的那段脚本** —— 官方面板自己的内联脚本是 ES module
   （含 import.meta / export），用 vm.Script 按经典脚本解析必然报错，别拿它
   当补丁的体检结果（v7.5 首次自检踩过）。 */
const ourScript = inlineScripts(patch)[0];
if (!inlineScripts(written).includes(ourScript)) fail("写盘后复检失败：注入的脚本与原补丁不一致");
assertPatchParses(patch);
ok(`已写入 ${HTML}（${Buffer.byteLength(written, "utf8")} 字节）`);
console.log(`
下一步：
  1. 浏览器硬刷新面板（Ctrl+F5）——静态文件，无需重启 CPA
  2. 控制台应出现 window.__mimoQuotaPatch === ${PATCH_VERSION}
  3. #/quota 每账号一张卡；#/auth-files mimo 卡内有额度区；插件开关即时生效`);
