package main

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"sort"
	"strings"
	"time"
)

/*
 * management_api 能力：在 CPA 管理面板里给 MiMo 加一个凭证状态页。
 *
 * 两类入口，鉴权边界完全不同（官方文档明确区分）：
 *   - 资源页 /v0/resource/plugins/mimo/status —— 浏览器直接打开，**不走管理鉴权**，
 *     所以只能渲染非敏感信息（绝不包含任何 token）。
 *   - 管理路由 GET /v0/management/plugins/mimo/status —— 需要管理密钥，返回 JSON。
 *
 * 两边渲染同一份数据，页面是服务端渲染的纯 HTML，不加载任何外部脚本/资源。
 */

type hostAuthRow struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Status        string `json:"status"`
	StatusMessage string `json:"status_message"`
	Disabled      bool   `json:"disabled"`
	Unavailable   bool   `json:"unavailable"`
	LastRefresh   string `json:"last_refresh,omitempty"`
	Path          string `json:"path,omitempty"`
}

type statusPayload struct {
	Plugin      string         `json:"plugin"`
	Version     string         `json:"version"`
	Provider    string         `json:"provider"`
	Config      map[string]any `json:"config"`
	Models      modelState     `json:"models"`
	Credentials []credState    `json:"credentials"`
	Host        []hostAuthRow  `json:"host_credentials"`
	HostError   string         `json:"host_error,omitempty"`
	Hints       []string       `json:"hints"`
}

func handleManagementRegister(req []byte) []byte {
	return okResult(map[string]any{
		"routes": []map[string]any{
			{"Method": "GET", "Path": "/plugins/mimo/status",
				"Description": "MiMo 凭证与模型目录状态（JSON）"},
		},
		"resources": []map[string]any{
			{"Path": "/status", "Menu": "MiMo 凭证",
				"Description": "MiMo SSO 凭证状态、续期记录与模型目录来源"},
		},
	})
}

func handleManagement(req []byte) []byte {
	var in struct {
		Method string              `json:"Method"`
		Path   string              `json:"Path"`
		Header map[string][]string `json:"Headers"`
		Query  map[string][]string `json:"Query"`
		Body   []byte              `json:"Body"`
	}
	_ = json.Unmarshal(req, &in)
	dbg("management.handle %s %s", in.Method, in.Path)

	payload := collectStatus()

	// 资源页：浏览器打开的 HTML，无管理鉴权 → 只放非敏感信息
	if strings.Contains(in.Path, "/resource/") {
		return okResult(map[string]any{
			"StatusCode": http.StatusOK,
			"Headers":    http.Header{"content-type": []string{"text/html; charset=utf-8"}},
			"Body":       []byte(renderStatusHTML(payload)),
		})
	}

	// 管理 API：需要管理密钥 → 返回完整 JSON
	body, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return okResult(map[string]any{
			"StatusCode": http.StatusInternalServerError,
			"Headers":    http.Header{"content-type": []string{"application/json"}},
			"Body":       []byte(`{"error":"无法序列化状态"}`),
		})
	}
	return okResult(map[string]any{
		"StatusCode": http.StatusOK,
		"Headers":    http.Header{"content-type": []string{"application/json"}},
		"Body":       body,
	})
}

func collectStatus() statusPayload {
	c := config()
	p := statusPayload{
		Plugin:   pluginName,
		Version:  pluginVer,
		Provider: providerKey,
		Config: map[string]any{
			"base_url":        c.BaseURL,
			"sid":             c.SID,
			"web_search_auto": c.WebSearchAuto,
			"refresh_after":   c.RefreshAfter,
			"model_ttl":       c.ModelTTL,
			"exclude_models":  c.ExcludeModels,
		},
		Models:      modelsSnapshot(),
		Credentials: credSnapshot(),
	}
	sort.Slice(p.Credentials, func(i, j int) bool { return p.Credentials[i].AuthID < p.Credentials[j].AuthID })

	// 宿主侧的凭证视图（只读）。拿不到就降级，不影响页面其余部分。
	hostRows, hostErr := listHostAuths()
	p.Host = hostRows
	if hostErr != nil {
		p.HostError = hostErr.Error()
	}

	p.Hints = buildHints(p)
	return p
}

// listHostAuths 通过 host.auth.list 读宿主侧的凭证状态。
// 只读、无副作用；失败时返回错误让页面显示出来，而不是静默吞掉。
func listHostAuths() ([]hostAuthRow, error) {
	res, err := hostCall(methodHostAuthList, map[string]any{})
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Files []struct {
			ID            string `json:"id"`
			Name          string `json:"name"`
			Type          string `json:"type"`
			Provider      string `json:"provider"`
			Status        string `json:"status"`
			StatusMessage string `json:"status_message"`
			Disabled      bool   `json:"disabled"`
			Unavailable   bool   `json:"unavailable"`
			LastRefresh   string `json:"last_refresh"`
			Path          string `json:"path"`
		} `json:"files"`
	}
	if err := json.Unmarshal(res, &parsed); err != nil {
		return nil, fmt.Errorf("host.auth.list 响应无法解析: %w", err)
	}
	out := make([]hostAuthRow, 0, len(parsed.Files))
	for _, f := range parsed.Files {
		// 只看属于本插件的凭证
		if !strings.EqualFold(f.Type, providerKey) && !strings.EqualFold(f.Provider, providerKey) {
			continue
		}
		out = append(out, hostAuthRow{
			ID: f.ID, Name: f.Name, Status: f.Status, StatusMessage: f.StatusMessage,
			Disabled: f.Disabled, Unavailable: f.Unavailable,
			LastRefresh: dashIfZeroTime(f.LastRefresh), Path: f.Path,
		})
	}
	return out, nil
}

// buildHints 把状态翻译成「该怎么办」，这是这个页面存在的意义。
func buildHints(p statusPayload) []string {
	var hints []string
	if len(p.Credentials) == 0 {
		hints = append(hints, "还没有加载到任何 MiMo 凭证：把 mimo.json 放进 CPA 的 auth 目录。")
	}
	for _, c := range p.Credentials {
		who := c.AuthID
		if c.UserID != "" {
			who = fmt.Sprintf("%s（userId %s）", c.AuthID, c.UserID)
		}
		if !c.CanRenew {
			hints = append(hints, fmt.Sprintf(
				"%s 只有 service_token，没有 pass_token —— 它失效后不会自动续期。建议在 mimo.json 里补上 pass_token。", who))
		}
		if !c.HasToken {
			hints = append(hints, fmt.Sprintf("%s 当前没有可用的 service_token，请求会直接 401。", who))
		}
		if c.LastError != "" {
			hints = append(hints, fmt.Sprintf(
				"%s 最近续期失败：%s —— 通常是 passToken 已失效，需要从 MiMo 客户端重新导出。", who, c.LastError))
		}
	}
	if p.Models.Source == "fallback" {
		hints = append(hints, "模型目录用的是兜底清单（只有两个文本模型）。上游目录没取到，通常是凭证不可用。")
	}
	if p.Models.Err != "" {
		hints = append(hints, "模型目录最近一次拉取报错："+p.Models.Err)
	}
	if p.HostError != "" {
		hints = append(hints, "读宿主凭证列表失败："+p.HostError)
	}
	if len(hints) == 0 {
		hints = append(hints, "一切正常。serviceToken 由上游 401 触发自动续期，定时刷新仅作兜底。")
	}
	return hints
}

/* ------------------------------------------------------------------ HTML */

func renderStatusHTML(p statusPayload) string {
	var b strings.Builder
	b.WriteString(`<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">`)
	b.WriteString(`<meta name="viewport" content="width=device-width,initial-scale=1">`)
	b.WriteString(`<title>MiMo 凭证状态</title><style>
:root{color-scheme:light dark}
body{font:14px/1.6 -apple-system,BlinkMacSystemFont,"Segoe UI","PingFang SC","Microsoft YaHei",sans-serif;
     margin:0;padding:24px;max-width:960px;background:#fafafa;color:#1a1a1a}
@media(prefers-color-scheme:dark){body{background:#16181d;color:#e6e6e6}
  .card{background:#1e2128!important;border-color:#2c313a!important}
  th{background:#232830!important}td,th{border-color:#2c313a!important}
  code{background:#232830!important}}
h1{font-size:18px;margin:0 0 4px}
.sub{color:#888;font-size:12px;margin-bottom:20px}
.card{background:#fff;border:1px solid #e4e4e7;border-radius:10px;padding:16px 18px;margin-bottom:16px}
h2{font-size:14px;margin:0 0 12px;font-weight:600}
table{border-collapse:collapse;width:100%;font-size:13px}
th,td{border:1px solid #e8e8ea;padding:6px 10px;text-align:left;vertical-align:top}
th{background:#f4f4f6;font-weight:600;white-space:nowrap}
code{background:#f0f0f2;padding:1px 5px;border-radius:4px;font-size:12px}
.ok{color:#0a7d38;font-weight:600}.bad{color:#c02626;font-weight:600}.warn{color:#b26a00;font-weight:600}
ul{margin:0;padding-left:20px}li{margin:4px 0}
.kv{display:grid;grid-template-columns:auto 1fr;gap:2px 14px;font-size:13px}
.kv div:nth-child(odd){color:#888;white-space:nowrap}
footer{color:#999;font-size:12px;margin-top:20px}
</style></head><body>`)

	fmt.Fprintf(&b, `<h1>%s</h1><div class="sub">插件 v%s · provider <code>%s</code></div>`,
		html.EscapeString(p.Plugin), html.EscapeString(p.Version), html.EscapeString(p.Provider))

	/* ---- 诊断建议放最前面，因为这才是来看这个页面的原因 ---- */
	b.WriteString(`<div class="card"><h2>诊断</h2><ul>`)
	for _, h := range p.Hints {
		cls := "ok"
		if strings.Contains(h, "失败") || strings.Contains(h, "401") || strings.Contains(h, "没有") {
			cls = "bad"
		} else if strings.Contains(h, "建议") || strings.Contains(h, "兜底") || strings.Contains(h, "只有") {
			cls = "warn"
		}
		fmt.Fprintf(&b, `<li class="%s">%s</li>`, cls, html.EscapeString(h))
	}
	b.WriteString(`</ul></div>`)

	/* ---- 凭证 ---- */
	b.WriteString(`<div class="card"><h2>凭证（本插件视角）</h2>`)
	if len(p.Credentials) == 0 {
		b.WriteString(`<p>暂无。</p>`)
	} else {
		b.WriteString(`<table><tr><th>AuthID</th><th>可续期</th><th>service_token</th><th>最近换取</th><th>续期 成功/失败</th><th>来源</th></tr>`)
		for _, c := range p.Credentials {
			renew := `<span class="bad">否</span>`
			if c.CanRenew {
				renew = `<span class="ok">是</span>`
			}
			tok := `<span class="bad">无</span>`
			if c.HasToken {
				tok = fmt.Sprintf(`<span class="ok">有</span> <code>%d 字符</code>`, c.TokenLen)
			}
			fmt.Fprintf(&b, `<tr><td><code>%s</code></td><td>%s</td><td>%s</td><td>%s</td><td>%d / %d</td><td>%s</td></tr>`,
				html.EscapeString(c.AuthID), renew, tok,
				fmtTime(c.ObtainedAt), c.RefreshOK, c.RefreshFail, html.EscapeString(orDash(c.LastIssued)))
		}
		b.WriteString(`</table>`)
		if n := reactiveTotal(p.Credentials); n > 0 {
			fmt.Fprintf(&b, `<p style="margin:10px 0 0;color:#888;font-size:12px">其中 %d 次是上游返回 401 后自动续期的（serviceToken 是会话 cookie，没有声明有效期，只能这样发现失效）。</p>`, n)
		}
	}
	b.WriteString(`</div>`)

	/* ---- 宿主视角 ---- */
	b.WriteString(`<div class="card"><h2>凭证（宿主视角 · host.auth.list）</h2>`)
	if p.HostError != "" {
		fmt.Fprintf(&b, `<p class="warn">读取失败：%s</p>`, html.EscapeString(p.HostError))
	} else if len(p.Host) == 0 {
		b.WriteString(`<p>宿主没有报告属于 MiMo 的凭证。</p>`)
	} else {
		b.WriteString(`<table><tr><th>名称</th><th>状态</th><th>说明</th><th>禁用</th><th>不可用</th><th>最近刷新</th></tr>`)
		for _, h := range p.Host {
			fmt.Fprintf(&b, `<tr><td><code>%s</code></td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>`,
				html.EscapeString(orDash(h.Name)), html.EscapeString(orDash(h.Status)),
				html.EscapeString(orDash(h.StatusMessage)), yesNo(h.Disabled), yesNo(h.Unavailable),
				html.EscapeString(orDash(h.LastRefresh)))
		}
		b.WriteString(`</table>`)
	}
	b.WriteString(`</div>`)

	/* ---- 模型目录 ---- */
	m := p.Models
	fmt.Fprintf(&b, `<div class="card"><h2>模型目录</h2><div class="kv">
<div>来源</div><div>%s</div><div>数量</div><div>%d</div><div>更新时间</div><div>%s</div>`,
		html.EscapeString(orDash(m.Source)), m.Count, fmtTime(m.At))
	if m.Err != "" {
		fmt.Fprintf(&b, `<div>最近错误</div><div class="warn">%s</div>`, html.EscapeString(m.Err))
	}
	b.WriteString(`</div></div>`)

	/* ---- 配置 ---- */
	b.WriteString(`<div class="card"><h2>配置</h2><div class="kv">`)
	keys := make([]string, 0, len(p.Config))
	for k := range p.Config {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, `<div>%s</div><div><code>%s</code></div>`,
			html.EscapeString(k), html.EscapeString(fmt.Sprintf("%v", p.Config[k])))
	}
	b.WriteString(`</div></div>`)

	fmt.Fprintf(&b, `<footer>生成于 %s · 本页不含任何 token，可安全分享截图</footer>`,
		time.Now().Format("2006-01-02 15:04:05"))
	b.WriteString(`</body></html>`)
	return b.String()
}

func reactiveTotal(cs []credState) int {
	n := 0
	for _, c := range cs {
		n += c.Reactive
	}
	return n
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Format("2006-01-02 15:04:05")
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

// dashIfZeroTime 把 Go 的零值时间换成占位符 —— 宿主没填这个字段时
// 会序列化成 "0001-01-01T00:00:00Z"，直接显示很难看。
func dashIfZeroTime(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "0001-01-01") {
		return "—"
	}
	return s
}

func yesNo(v bool) string {
	if v {
		return `<span class="bad">是</span>`
	}
	return "否"
}
