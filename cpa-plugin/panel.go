package main

// panel.go —— 独立 mimo 管理面板（Plan B，v0.2.0+）
//
// 官方面板的额度 UI 硬编码 7 家内置 provider，插件补丁又会被官方面板的
// asset updater 覆写（对抗面）⇒ 提供**插件自带的独立管理面板**：
// 资源路由 /v0/resource/plugins/mimo/panel（与面板同源，共享管理密钥域），
// 功能对标 Node 版管理界面：密钥登录、账号管理（启停/删除/刷新额度/刷新
// OAuth 续期）、额度卡片、在线登录（扫码/密码/OTP 复用登录页会话）、
// 用量图表（额度趋势 + 近 30 天请求量，手写 SVG）、模型清单、深浅色。
//
// 默认关闭：standalone_panel: true 才在官方面板侧边栏挂菜单、才响应页面。
// （Menu 留空 = 宿主快照直接跳过，面板不出现菜单项。）

import (
	"embed"
	"net/http"
	"strings"
)

//go:embed panel-page/panel.html
var panelPageFS embed.FS

// standalonePanelEnabled 独立面板开关（缺省 false：默认关闭）。
func standalonePanelEnabled() bool {
	c := config()
	if c.StandalonePanel == nil {
		return false
	}
	return *c.StandalonePanel
}

// panelMenuLabel 面板菜单名：启用才挂菜单。
func panelMenuLabel() string {
	if standalonePanelEnabled() {
		return "MiMo 面板"
	}
	return ""
}

// servePanelPage 独立面板页面（启用时）。
func servePanelPage() []byte {
	if !standalonePanelEnabled() {
		return resourceJSON(404, `{"error":"standalone_panel 未启用（plugins.configs.mimo.standalone_panel: true 开启）"}`)
	}
	raw, err := panelPageFS.ReadFile("panel-page/panel.html")
	if err != nil {
		return resourceJSON(500, `{"error":"面板页面缺失"}`)
	}
	return okResult(map[string]any{
		"StatusCode": http.StatusOK,
		"Headers":    http.Header{"content-type": []string{"text/html; charset=utf-8"}},
		"Body":       raw,
	})
}

// isPanelResource 是否独立面板页面请求（GET 且路径以 /panel 结尾）。
func isPanelResource(method, path string) bool {
	if method != http.MethodGet {
		return false
	}
	p := path
	if i := strings.Index(p, "?"); i >= 0 {
		p = p[:i]
	}
	return strings.HasSuffix(p, "/panel")
}
