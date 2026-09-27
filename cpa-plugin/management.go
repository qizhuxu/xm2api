package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"sort"
	"strings"
	"time"
)

/*
 * management_api 能力。
 *
 * 按需求调整（2025-09）：**不再向官方面板注册 plugin-pages 页面**。
 * 原来的「MiMo 凭证」页面（management.html#/plugin-pages/mimo/0）只有状态展示、
 * 没有管理功能，用户明确不需要。面板菜单由 management.register 的 resources
 * 数组生成，这里返回空数组即可让菜单消失。
 *
 * 保留的入口：
 *   - 管理路由 GET /v0/management/plugins/mimo/status —— 需要管理密钥（X-Management-Key
 *     或 Authorization: Bearer），返回 JSON，供运维 curl 查询。
 *
 * renderStatusHTML 与资源页分支保留：单测仍然直接调用它们验证「状态输出绝不泄漏
 * token」这一安全性质（TestStatusNeverLeaksToken），且后续若加回调登录等功能可复用。
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
	Plugin      string           `json:"plugin"`
	Version     string           `json:"version"`
	Provider    string           `json:"provider"`
	Config      map[string]any   `json:"config"`
	Models      modelState       `json:"models"`
	Credentials []credState      `json:"credentials"`
	Host        []hostAuthRow    `json:"host_credentials"`
	HostError   string           `json:"host_error,omitempty"`
	Usage       []map[string]any `json:"usage,omitempty"`
	Hints       []string         `json:"hints"`
}

func handleManagementRegister(req []byte) []byte {
	// resources 里的 Menu 一律留空：宿主快照对空 Menu 的资源路由直接跳过
	// （internal/pluginhost/snapshot.go:130-134），面板不会出现 plugin-pages/mimo
	// 菜单（用户需求②）。/login 资源路由仅供扫码登录页按一次性会话 ID 访问。
	return okResult(map[string]any{
		"routes": []map[string]any{
			{"Method": "GET", "Path": "/plugins/mimo/status",
				"Description": "MiMo 凭证、用量与模型目录状态（JSON，需要管理密钥）"},
			{"Method": "POST", "Path": "/plugins/mimo/login/start",
				"Description": "创建登录会话（需要管理密钥），返回一次性登录页地址（扫码+账号密码）"},
			{"Method": "POST", "Path": "/plugins/mimo/login/password",
				"Description": "账号密码登录（需要管理密钥）；body {session?,user,password}，密码不落盘不进日志；小米新设备验证时返回 status=awaiting-otp"},
			{"Method": "POST", "Path": "/plugins/mimo/login/verify",
				"Description": "提交小米新设备验证的短信/邮箱验证码（需要管理密钥）；body {session,code}"},
			{"Method": "POST", "Path": "/plugins/mimo/login/cancel",
				"Description": "取消登录会话"},
			{"Method": "GET", "Path": "/plugins/mimo/login/status",
				"Description": "查询登录会话状态"},
		},
		"resources": []map[string]any{
			{"Path": "/login", "Menu": "",
				"Description": "MiMo 登录页（扫码+账号密码双通道；一次性会话 ID 访问；Menu 留空故不进面板菜单）"},
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

	// ---- 资源路由（无管理鉴权）：登录页 / 会话状态轮询 / 账号密码登录 ----
	// 只输出非敏感信息；qr/lp/loginUrl 等凭证等价物绝不出现，密码绝不回显。
	if strings.Contains(in.Path, "/resource/") {
		id := ""
		if v := in.Query["session"]; len(v) > 0 {
			id = strings.TrimSpace(v[0])
		}
		// POST：登录页密码表单提交（凭 session ID 访问，与扫码页同一暴露面）
		if in.Method == http.MethodPost {
			return wrapResourcePassword(in.Body, id)
		}
		// GET + op=password|verify：免管理密钥的提交通道。宿主资源路由只转发
		// GET（POST 404 空体，实测 v7.3.9），页面提交只能走 GET；payload 放
		// x-mimo-req 头（base64 JSON），密码不进 URL、不进宿主访问日志。
		// 只打 header 键名（req= 若在 query 绝不 dump，防止密码进日志）。
		if v := in.Query["op"]; len(v) > 0 && strings.TrimSpace(v[0]) != "" {
			hk := make([]string, 0, len(in.Header))
			for k := range in.Header {
				hk = append(hk, k)
			}
			dbg("resource GET op=%s header-keys=%v", strings.TrimSpace(v[0]), hk)
			raw := decodeMimoReq(in.Header, in.Query)
			switch strings.TrimSpace(v[0]) {
			case "password":
				return wrapManagementHTTP(handleLoginPassword(raw))
			case "verify":
				return wrapManagementHTTP(handleLoginVerify(raw))
			default:
				return resourceJSON(400, `{"error":"unknown op"}`)
			}
		}
		s := getQRSession(id)
		if v := in.Query["poll"]; len(v) > 0 && strings.TrimSpace(v[0]) != "" {
			if s == nil {
				return resourceJSON(404, `{"error":"session not found or expired"}`)
			}
			st, _ := json.Marshal(map[string]any{
				"session": s.ID, "status": s.Status, "message": s.Message,
				"user_id": s.UserID, "auth_file": s.AuthFile,
			})
			return resourceJSON(200, string(st))
		}
		return okResult(map[string]any{
			"StatusCode": http.StatusOK,
			"Headers":    http.Header{"content-type": []string{"text/html; charset=utf-8"}},
			"Body":       []byte(renderLoginPage(s, "")),
		})
	}

	// ---- 管理路由（宿主已校验管理密钥）----
	// ⚠️ 宿主要求 management.handle 的 RPC 结果是 {StatusCode,Headers,Body} 的
	// HTTP 响应描述；业务 handler 返回的是 {ok,result|error} 信封，必须经
	// wrapManagementHTTP 转换，否则宿主报 "plugin management handler failed"。
	switch {
	case strings.HasSuffix(in.Path, "/login/start") && in.Method == http.MethodPost:
		return wrapManagementHTTP(handleLoginStart(in.Body))
	case strings.HasSuffix(in.Path, "/login/password") && in.Method == http.MethodPost:
		return wrapManagementHTTP(handleLoginPassword(in.Body))
	case strings.HasSuffix(in.Path, "/login/verify") && in.Method == http.MethodPost:
		return wrapManagementHTTP(handleLoginVerify(in.Body))
	case strings.HasSuffix(in.Path, "/login/cancel") && in.Method == http.MethodPost:
		return wrapManagementHTTP(handleLoginCancel(in.Body))
	case strings.Contains(in.Path, "/login/status"):
		return wrapManagementHTTP(handleLoginStatus(in.Query))
	}

	// 默认：状态 JSON（凭证/用量/模型目录/配置）
	payload := collectStatus()
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

func resourceJSON(status int, body string) []byte {
	return okResult(map[string]any{
		"StatusCode": status,
		"Headers":    http.Header{"content-type": []string{"application/json"}},
		"Body":       []byte(body),
	})
}

// decodeMimoReq 解出免密钥提交通道的 payload：优先 x-mimo-req 头（base64 JSON），
// 兜底 req= query（万一宿主将来剥自定义头）。密码只在返回的字节里，绝不打印。
func decodeMimoReq(h map[string][]string, q map[string][]string) []byte {
	enc := ""
	for k, v := range h {
		if strings.EqualFold(k, "x-mimo-req") && len(v) > 0 {
			enc = strings.TrimSpace(v[0])
			break
		}
	}
	if enc == "" {
		if v := q["req"]; len(v) > 0 {
			enc = strings.TrimSpace(v[0])
		}
	}
	if enc == "" {
		return []byte("{}")
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return []byte("{}")
	}
	return raw
}

// wrapResourcePassword 处理登录页密码表单的 POST：
// body JSON {session,user,password}（session 也可从 query 补），结果转成资源路由
// 的 HTTP 响应描述。密码只在 handleLoginPassword 内存中流转。
func wrapResourcePassword(body []byte, querySession string) []byte {
	var in struct {
		Session string `json:"session"`
	}
	_ = json.Unmarshal(body, &in)
	if strings.TrimSpace(in.Session) == "" && strings.TrimSpace(querySession) != "" {
		merged := map[string]any{}
		_ = json.Unmarshal(body, &merged)
		merged["session"] = strings.TrimSpace(querySession)
		if b, err := json.Marshal(merged); err == nil {
			body = b
		}
	}
	env := handleLoginPassword(body)
	var e envelope
	_ = json.Unmarshal(env, &e)
	status := http.StatusOK
	payload := []byte(e.Result)
	if !e.OK {
		status = http.StatusUnauthorized
		payload = []byte(`{"error":"login failed"}`)
		if e.Error != nil {
			if e.Error.HTTPStatus != 0 {
				status = e.Error.HTTPStatus
			}
			msg, _ := json.Marshal(map[string]any{"error": e.Error.Message, "code": e.Error.Code})
			payload = msg
		}
	}
	return resourceJSON(status, string(payload))
}

// wrapManagementHTTP 把业务信封 {ok,result|error} 转成宿主要的
// HTTP 响应描述 {StatusCode,Headers,Body}。
//
// ⚠️ Body 必须是 []byte 类型（json 序列化成 base64 字符串）：宿主的响应结构体
// 里 Body 是 []byte 字段，如果这里放 json.RawMessage，会序列化成裸 JSON 对象，
// 宿主 unmarshal 失败 → "plugin management handler failed"。
func wrapManagementHTTP(env []byte) []byte {
	var e envelope
	_ = json.Unmarshal(env, &e)
	status := http.StatusOK
	body := []byte(e.Result)
	if !e.OK {
		status = http.StatusInternalServerError
		if e.Error != nil {
			if e.Error.HTTPStatus != 0 {
				status = e.Error.HTTPStatus
			}
			msg, _ := json.Marshal(map[string]any{"error": e.Error.Message, "code": e.Error.Code})
			body = msg
		} else {
			body = []byte(`{"error":"plugin handler failed"}`)
		}
	}
	if len(body) == 0 {
		body = []byte("{}")
	}
	return okResult(map[string]any{
		"StatusCode": status,
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
			"log_to_host":     c.LogToHost,
			"patch_panel":     patchPanelEnabled(),
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

	// 剩余使用量快照（GET /api/user/usage，auth.parse/refresh 时采集）
	p.Usage = usageSnapshotsAll()

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

	/* ---- 剩余使用量快照 ---- */
	if len(p.Usage) > 0 {
		b.WriteString(`<div class="card"><h2>剩余使用量（GET /api/user/usage 快照）</h2>` +
			`<table><tr><th>AuthID</th><th>剩余</th><th>重置日期</th><th>采集时间</th><th>备注</th></tr>`)
		for _, u := range p.Usage {
			remaining := "—"
			if v, ok := u["remaining_percent"]; ok {
				remaining = fmt.Sprintf("%v%%", v)
			}
			fmt.Fprintf(&b, `<tr><td><code>%s</code></td><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>`,
				html.EscapeString(mapStr(u, "auth_id")),
				html.EscapeString(remaining),
				html.EscapeString(mapStr(u, "reset_date")),
				html.EscapeString(mapStr(u, "observed_at")),
				html.EscapeString(mapStr(u, "error")))
		}
		b.WriteString(`</table><p style="color:#888;font-size:12px;margin:10px 0 0">` +
			`实时查询：<code>POST /v0/management/quota/fetch {"auth_index":"..."}</code>（需管理密钥）；` +
			`面板 auth-files 详情 INFO 视图里也能看到 <code>usage_snapshot</code> 字段。</p></div>`)
	}

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

// mapStr 从 map[string]any 里取字段转字符串，缺失/空值给占位符。
func mapStr(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok || v == nil {
		return "—"
	}
	s := strings.TrimSpace(fmt.Sprintf("%v", v))
	if s == "" {
		return "—"
	}
	return s
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
