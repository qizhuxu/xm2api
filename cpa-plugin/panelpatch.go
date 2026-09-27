package main

// panelpatch.go —— 面板补丁自动注入（v0.2.0 起随插件交付）
//
// 背景：#/auth-files 的 mimo 额度带与 #/quota 的 mimo 额度卡是面板补丁
// （panel-patch/mimo-quota-patch.html）的特性 —— 官方面板 SPA 的额度 UI
// 硬编码只渲染 7 家内置 provider（bundle 实测：aA 组件的 quotaType 分支表，
// 未知类型直接 throw），插件 provider 永远不渲染。此前补丁要手工跑
// apply-patch.mjs 注入 management.html；Docker 部署里面板文件在容器内
// （/CLIProxyAPI/static/management.html），手工打补丁不便。现在由插件在
// 启动时自动注入：把插件 .so 放进 plugins/ 就能拿到与本机一致的面板。
//
// 注入语义与 apply-patch.mjs 严格对齐：补丁源文件**原样**（注释 + <style> +
// <script>，注释自带 --> 收尾）紧贴 <head[^>]*> 注入；以 __mimoQuotaPatch
// 版本标记幂等/升级；官方面板更新覆写文件后（management asset auto-updater
// 会按 GitHub digest 覆写），周期重检自动重打。
//
// ⚠️ 两条实测教训（都写在对应函数注释里）：
//   1. 版本标记必须锚定 if(...)return;window.__mimoQuotaPatch=<v>; 真实赋值行，
//      变更日志注释里也写着同名字段；
//   2. 旧补丁区定位不能用「第一个 <script>」——注释里就写着字面 <script> 字样。

import (
	"embed"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

//go:embed panel-patch/mimo-quota-patch.html
var panelPatchFS embed.FS

// registerOnce 防止重复注册时叠出多条补丁巡检 goroutine。
var registerOnce sync.Once

const patchMarker = "__mimoQuotaPatch"

var (
	headTagRe = regexp.MustCompile(`(?i)<head[^>]*>`)
	bodyTagRe = regexp.MustCompile(`(?i)<body[^>]*>`)
	guardRe   = regexp.MustCompile(`if\s*\(\s*window\.` + patchMarker + `\s*\)\s*return;`)
	versionRe = regexp.MustCompile(`if\s*\(\s*window\.` + patchMarker + `\s*\)\s*return;\s*window\.` + patchMarker + `\s*=\s*([^;]+);`)
	// 注释头锚点：源文件是 "<!--\n  MiMo quota patch for ..."（换行+缩进），
	// 必须容忍空白；「for CLIProxyAPI」确保不会误中 JS 注释里的 "patch v7.x"。
	patchHeadRe = regexp.MustCompile(`<!--\s*MiMo quota patch for`)
)

// patchPayload 补丁完整载荷 = 补丁源文件原样（注释 + <style> + <script>）。
// 与 apply-patch.mjs 的注入语义一致：注释自带收尾，整体插入即合法 HTML。
func patchPayload() string {
	raw, err := panelPatchFS.ReadFile("panel-patch/mimo-quota-patch.html")
	if err != nil {
		return ""
	}
	return string(raw)
}

// patchVersion 补丁载荷的版本标记值。
//
// ⚠️ 必须锚定真实赋值行（if(...)return;window.__mimoQuotaPatch='7.7';）——
// 变更日志注释里也写着 `window.__mimoQuotaPatch=7.4.1` 之类的字样，
// 不锚定会把说明文字当版本号（apply-patch.mjs 的实测教训）。
func patchVersion() string {
	m := versionRe.FindStringSubmatch(patchPayload())
	if m == nil {
		return "?"
	}
	return strings.TrimSpace(m[1])
}

// filePatchVersion 面板文件里现有补丁的版本。
func filePatchVersion(html string) string {
	m := versionRe.FindStringSubmatch(html)
	if m == nil {
		return "?"
	}
	return strings.TrimSpace(m[1])
}

// legacyRange 定位面板文件里已注入补丁的字节范围：从注释头
// 「<!-- MiMo quota patch」到守卫所在脚本块的 </script> 收尾。
//
// ⚠️ 绝不能用「第一个 <script>」定位：补丁变更日志注释里就写着字面
// `<script>`/`</script>` 字样（apply-patch.mjs 早有警告）。v0.2.0 首版按
// 「首个 script 块」找范围，升级替换时把注释收尾连同 <style> 一起吃掉，
// 整页被未闭合 HTML 注释吞掉 —— 面板全灭、补丁标记 undefined（实测踩过）。
func legacyRange(html string) (int, int, bool) {
	g := guardRe.FindStringIndex(html)
	if g == nil {
		return 0, 0, false
	}
	eRel := strings.Index(html[g[0]:], "</script>")
	if eRel < 0 {
		return 0, 0, false
	}
	e := g[0] + eRel + len("</script>")
	m := patchHeadRe.FindStringIndex(html)
	s := -1
	if m != nil {
		s = m[0]
	}
	if s < 0 {
		// 无注释头的旧格式：退化为守卫脚本块本身
		sRel := strings.LastIndex(html[:g[0]], "<script")
		if sRel < 0 {
			return 0, 0, false
		}
		s = sRel
	}
	return s, e, true
}

// injectPanelPatch 给单个面板文件打补丁。返回 (是否本次写入, 说明)。
// 版本感知：同版本 ⇒ 跳过（幂等）；旧版本 ⇒ 整段替换（补丁自身升级）；
// 无标记 ⇒ 紧贴 <head[^>]*> 全新注入（官方面板更新覆写文件后走这条）。
func injectPanelPatch(path string) (bool, string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, "读面板失败: " + err.Error()
	}
	html := string(raw)
	payload := patchPayload()
	if strings.TrimSpace(payload) == "" {
		return false, "补丁载荷为空"
	}

	if strings.Contains(html, patchMarker) {
		cur := filePatchVersion(html)
		if cur == patchVersion() {
			return false, "已打过补丁（版本 " + cur + "）"
		}
		s, e, ok := legacyRange(html)
		if !ok {
			return false, "标记存在但找不到补丁区（面板被手工改动过？）"
		}
		out := html[:s] + payload + "\n" + html[e:]
		if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
			return false, "写面板失败: " + err.Error()
		}
		return true, "补丁已升级 " + cur + " → " + patchVersion()
	}

	loc := headTagRe.FindStringIndex(html)
	if loc == nil {
		loc = bodyTagRe.FindStringIndex(html)
	}
	if loc == nil {
		return false, "找不到 <head>/<body> 注入点"
	}
	out := html[:loc[1]] + "\n" + payload + "\n" + html[loc[1]:]
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		return false, "写面板失败: " + err.Error()
	}
	return true, "补丁 " + patchVersion() + " 已注入"
}

// panelHTMLCandidates 面板文件的可能位置：宿主 cwd（Docker 里是 /CLIProxyAPI）、
// 可执行文件旁、以及容器内固定路径。
func panelHTMLCandidates() []string {
	cands := []string{
		filepath.Join("static", "management.html"),
		filepath.Join("bin", "static", "management.html"),
		filepath.Join("/CLIProxyAPI", "static", "management.html"),
	}
	if exe, err := os.Executable(); err == nil {
		d := filepath.Dir(exe)
		cands = append(cands,
			filepath.Join(d, "static", "management.html"),
			filepath.Join(d, "bin", "static", "management.html"),
		)
	}
	return cands
}

// patchPanelOnce 遍历候选位置打补丁；至少命中一个才返回 true。
func patchPanelOnce() bool {
	hit := false
	for _, p := range panelHTMLCandidates() {
		if st, err := os.Stat(p); err != nil || st.IsDir() {
			continue
		}
		ok, why := injectPanelPatch(p)
		if ok {
			hostLog("info", "MiMo 面板补丁已自动注入: "+p+"（"+why+"）")
			hit = true
		} else {
			dbg("面板补丁跳过 %s: %s", p, why)
		}
	}
	return hit
}

// startPanelPatch 启动时打一次，之后每 10 分钟重检一次：
// 官方面板的 asset auto-updater 会按 GitHub digest 覆写 management.html，
// 覆写后补丁标记消失，下一轮重检自动重打（无需重启）。
func startPanelPatch() {
	if !patchPanelEnabled() {
		dbg("面板自动补丁已按配置关闭（patch_panel: false）")
		return
	}
	go func() {
		patchPanelOnce()
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		for range t.C {
			patchPanelOnce()
		}
	}()
}
