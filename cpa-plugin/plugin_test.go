package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

/* ------------------------------------------------------------- SSE 解析 */

func TestToPayload(t *testing.T) {
	cases := []struct{ in, want string }{
		// MiMo 上游的实际形状：冒号后没有空格
		{`data:{"id":"a","choices":[]}`, `{"id":"a","choices":[]}`},
		{`data: {"id":"b","choices":[]}`, `{"id":"b","choices":[]}`},
		// 终止帧与注释要丢掉（CPA 自己补 [DONE]）
		{"data:[DONE]", ""},
		{"data: [DONE]", ""},
		{": keepalive", ""},
		{"event: message", ""},
		{"id: 42", ""},
		{"retry: 1000", ""},
		// 空行 / 非 JSON 一律丢
		{"", ""},
		{"   ", ""},
		{"data:not-json", ""},
		// 上游万一直接发裸 JSON 也要放行
		{`{"id":"c"}`, `{"id":"c"}`},
	}
	for _, c := range cases {
		got := string(toPayload([]byte(c.in)))
		if got != c.want {
			t.Errorf("toPayload(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

/* --------------------------------------------------- web search 兼容层 */

func TestMaybeInjectWebSearch(t *testing.T) {
	setConfig(cfg{WebSearchAuto: true})
	defer setConfig(defaultCfg())

	t.Run("没 tools 时注入", func(t *testing.T) {
		out := maybeInjectWebSearch("mimo-x-flash-preview", []byte(`{"model":"mimo-x-flash-preview"}`))
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(out, &obj); err != nil {
			t.Fatalf("结果是非法 JSON: %v", err)
		}
		if string(obj["tools"]) != `[{"type":"web_search"}]` {
			t.Fatalf("tools = %s, 期望注入 web_search", obj["tools"])
		}
	})

	t.Run("调用方自带 tools 时不动", func(t *testing.T) {
		in := []byte(`{"tools":[{"type":"function","function":{"name":"get_weather"}}]}`)
		if got := string(maybeInjectWebSearch("mimo-x-flash-preview", in)); got != string(in) {
			t.Fatalf("不该改写自带 tools 的请求，得到 %s", got)
		}
	})

	t.Run("幂等：注入两次结果相同", func(t *testing.T) {
		once := maybeInjectWebSearch("mimo-x-flash-preview", []byte(`{"model":"m"}`))
		twice := maybeInjectWebSearch("mimo-x-flash-preview", once)
		if string(once) != string(twice) {
			t.Fatalf("不幂等:\n once=%s\ntwice=%s", once, twice)
		}
	})

	t.Run("TTS/ASR/图像模型不注入", func(t *testing.T) {
		for _, m := range []string{"mimo-v2.5-tts", "mimo-v2.5-asr", "Doubao-Seedream-5.0-pro", "x-voiceclone"} {
			in := []byte(`{"model":"` + m + `"}`)
			if got := string(maybeInjectWebSearch(m, in)); got != string(in) {
				t.Errorf("模型 %s 不该注入，得到 %s", m, got)
			}
		}
	})

	t.Run("开关关闭时不注入", func(t *testing.T) {
		setConfig(cfg{WebSearchAuto: false})
		defer setConfig(cfg{WebSearchAuto: true})
		in := []byte(`{"model":"m"}`)
		if got := string(maybeInjectWebSearch("mimo-x-flash-preview", in)); got != string(in) {
			t.Fatalf("关闭时不该注入，得到 %s", got)
		}
	})
}

/* --------------------------------------------------------- 模型过滤 */

func TestMatchModel(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"Doubao-Seedream-5.0-pro", "Doubao-Seedream-5.0-pro", true}, // 精确
		{"doubao-seedream-5.0-pro", "Doubao-Seedream-5.0-pro", true}, // 大小写不敏感
		{"Doubao-*", "Doubao-Seedream-5.0-pro", true},
		{"*-tts", "mimo-v2.5-tts", true},
		{"mimo-v2.5-*", "mimo-v2.5-tts-voiceclone", true},
		{"mimo-x-*-preview", "mimo-x-flash-preview", true},
		{"Doubao-*", "mimo-x-flash-preview", false},
		{"", "anything", false},                  // 空模式不匹配任何东西
		{"mimo-v2.5-tts", "mimo-v2.5-tts-x", false},
	}
	for _, c := range cases {
		if got := matchModel(c.pattern, c.name); got != c.want {
			t.Errorf("matchModel(%q, %q) = %v, want %v", c.pattern, c.name, got, c.want)
		}
	}
}

func TestApplyExclusions(t *testing.T) {
	list := []modelInfo{
		{ID: "mimo-x-flash-preview"}, {ID: "mimo-x-pro-preview"},
		{ID: "mimo-v2.5-tts"}, {ID: "Doubao-Seedream-5.0-pro"},
	}

	// 不配就原样返回
	setConfig(defaultCfg())
	if got := applyExclusions(list); len(got) != 4 {
		t.Fatalf("默认不该过滤，得到 %d 个", len(got))
	}

	// 隐藏图像模型（README 推荐的用法）
	setConfig(cfg{ExcludeModels: []string{"Doubao-*"}})
	defer setConfig(defaultCfg())
	got := applyExclusions(list)
	if len(got) != 3 {
		t.Fatalf("期望剩 3 个，得到 %d", len(got))
	}
	for _, m := range got {
		if m.ID == "Doubao-Seedream-5.0-pro" {
			t.Fatal("Doubao 应该被过滤掉")
		}
	}

	// 多个模式
	setConfig(cfg{ExcludeModels: []string{"Doubao-*", "*-tts"}})
	if got := applyExclusions(list); len(got) != 2 {
		t.Fatalf("期望剩 2 个，得到 %d", len(got))
	}
}

func TestInvalidateModels(t *testing.T) {
	mu.Lock()
	modelCache.at = time.Now()
	modelCache.models = []modelInfo{{ID: "cached-1"}}
	mu.Unlock()

	invalidateModels()

	mu.Lock()
	at := modelCache.at
	kept := len(modelCache.models)
	mu.Unlock()
	if !at.IsZero() {
		t.Error("时间戳应当被清零，否则缓存仍被视为新鲜")
	}
	if kept == 0 {
		t.Error("不该清空列表 —— 重新拉失败时要能回落到上一份")
	}
}

// 这条覆盖的是一个实际撞到过的退化：CPA 启动时 token 恰好失效，
// model.for_auth 拿到 401，目录退化成两个硬编码模型，而且会一直持续到重启
// （宿主只在加载凭证时做一次发现）。有了磁盘缓存就不会退化。
func TestModelDiskCacheRecoversCatalog(t *testing.T) {
	old := modelsCacheFile
	modelsCacheFile = filepath.Join(t.TempDir(), "models.json")
	defer func() { modelsCacheFile = old }()

	// 清掉内存态，模拟刚启动
	mu.Lock()
	modelCache.at = time.Time{}
	modelCache.models = nil
	mu.Unlock()

	if disk := loadModelsFromDisk(); len(disk) != 0 {
		t.Fatal("空文件时不该读到内容")
	}

	good := []modelInfo{{ID: "mimo-x-flash-preview"}, {ID: "mimo-x-pro-preview"}, {ID: "mimo-v2.5-tts"}}
	saveModelsToDisk(good)

	disk := loadModelsFromDisk()
	if len(disk) != len(good) {
		t.Fatalf("往返后 %d 条，期望 %d 条", len(disk), len(good))
	}
	if disk[2].ID != "mimo-v2.5-tts" {
		t.Errorf("内容不对: %+v", disk)
	}

	// 没凭证时（model.static 的场景）应当走磁盘缓存而不是硬编码兜底
	got, source, _ := resolveModels("")
	if source != "disk" {
		t.Fatalf("来源 = %q，期望 disk", source)
	}
	if len(got) != len(good) {
		t.Fatalf("拿到 %d 条，期望 %d 条", len(got), len(good))
	}
}

/* ------------------------------------------------------------- 凭证 */

func TestCredNormalizeFromCookie(t *testing.T) {
	// 从 xm2api 的 sso-session.json 迁移过来的写法
	c := mimoCred{Cookie: "serviceToken=abc123; userId=42"}
	c.normalize()
	if c.ServiceToken != "abc123" || c.UserID != "42" {
		t.Fatalf("解析 routeCookieHeader 失败: %+v", c)
	}
	if c.Type != providerKey {
		t.Fatalf("type 应默认为 %s，得到 %s", providerKey, c.Type)
	}
	if !c.usable() {
		t.Fatal("usable() 应为 true")
	}
	if got := c.routeCookie(); got != "serviceToken=abc123; userId=42" {
		t.Fatalf("routeCookie = %q", got)
	}
}

func TestCredUsable(t *testing.T) {
	if (mimoCred{}).usable() {
		t.Fatal("空凭证不该 usable")
	}
	if !(mimoCred{PassToken: "p", UserID: "u"}).usable() {
		t.Fatal("只有 pass_token 也应 usable（parse 时会去换）")
	}
	// 缺 userId 时拼不出可用 Cookie
	if got := (mimoCred{ServiceToken: "s"}).routeCookie(); got != "" {
		t.Fatalf("缺 userId 应返回空 cookie，得到 %q", got)
	}
}

func TestAuthDataShape(t *testing.T) {
	next := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	got := authData(mimoCred{Type: providerKey, ServiceToken: "s", UserID: "u1"}, "mimo.json", "", next)

	if got["Provider"] != providerKey {
		t.Errorf("Provider = %v", got["Provider"])
	}
	if got["ID"] != providerKey+"-u1" {
		t.Errorf("ID = %v，期望按 userId 派生", got["ID"])
	}
	// StorageJSON 必须是 []byte：宿主按 base64 解，序列化后应是字符串
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var round map[string]json.RawMessage
	if err := json.Unmarshal(raw, &round); err != nil {
		t.Fatal(err)
	}
	var storage []byte
	if err := json.Unmarshal(round["StorageJSON"], &storage); err != nil {
		t.Fatalf("StorageJSON 不是可 base64 解码的字节: %v", err)
	}
	var back mimoCred
	if err := json.Unmarshal(storage, &back); err != nil {
		t.Fatalf("StorageJSON 里的凭证无法还原: %v", err)
	}
	if back.ServiceToken != "s" || back.UserID != "u1" {
		t.Fatalf("凭证往返丢失: %+v", back)
	}
	if got["NextRefreshAfter"] != "2030-01-02T03:04:05Z" {
		t.Errorf("NextRefreshAfter = %v", got["NextRefreshAfter"])
	}
}

/* --------------------------------------------------------- 错误映射 */

func TestUpstreamErrorMapping(t *testing.T) {
	cases := []struct {
		status   int
		wantCode string
	}{
		{401, "invalid_api_key"},
		{403, "insufficient_quota"},
		{404, "model_not_found"},
		{429, "rate_limit_exceeded"},
		{500, "internal_server_error"},
		{400, "invalid_request_error"},
	}
	for _, c := range cases {
		var env struct {
			OK    bool `json:"ok"`
			Error struct {
				Code       string `json:"code"`
				HTTPStatus int    `json:"http_status"`
			} `json:"error"`
		}
		if err := json.Unmarshal(upstreamError(c.status, []byte(`{"message":"boom"}`)), &env); err != nil {
			t.Fatal(err)
		}
		if env.OK {
			t.Errorf("status %d: ok 应为 false", c.status)
		}
		if env.Error.Code != c.wantCode {
			t.Errorf("status %d: code = %s, want %s", c.status, env.Error.Code, c.wantCode)
		}
		// 这是官方文档点名的坑：漏了 http_status 会被 CPA 降级成 500
		if env.Error.HTTPStatus != c.status {
			t.Errorf("status %d: http_status = %d，必须原样带回", c.status, env.Error.HTTPStatus)
		}
	}
}

func TestExtractUpstreamMessage(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"error":{"message":"bad key"}}`, "bad key"},
		{`{"message":"rate limited"}`, "rate limited"},
		{`{"description":"需要二次验证"}`, "需要二次验证"},
		{`plain text`, "plain text"},
	}
	for _, c := range cases {
		if got := extractUpstreamMessage([]byte(c.in)); got != c.want {
			t.Errorf("extractUpstreamMessage(%s) = %q, want %q", c.in, got, c.want)
		}
	}
}

/* ------------------------------------------------------- 方法分发冒烟 */

func TestDispatchUnknownMethod(t *testing.T) {
	var env struct {
		OK    bool `json:"ok"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(dispatch("no.such.method", nil), &env); err != nil {
		t.Fatal(err)
	}
	if env.OK || env.Error.Code != "unknown_method" {
		t.Fatalf("未知方法应返回 unknown_method，得到 %+v", env)
	}
}

func TestDispatchIdentifiers(t *testing.T) {
	for _, m := range []string{methodAuthIdentifier, methodExecutorIdentifier} {
		var env struct {
			OK     bool `json:"ok"`
			Result struct {
				Identifier string `json:"identifier"`
			} `json:"result"`
		}
		if err := json.Unmarshal(dispatch(m, nil), &env); err != nil {
			t.Fatal(err)
		}
		if !env.OK || env.Result.Identifier != providerKey {
			t.Fatalf("%s: 应返回 identifier=%s，得到 %+v", m, providerKey, env)
		}
	}
}

func TestAuthParseRejectsForeignCredential(t *testing.T) {
	req, _ := json.Marshal(map[string]any{
		"Provider": "other",
		"FileName": "other.json",
		"RawJSON":  []byte(`{"type":"codex","access_token":"x"}`),
	})
	var env struct {
		OK     bool `json:"ok"`
		Result struct {
			Handled bool `json:"Handled"`
		} `json:"result"`
	}
	if err := json.Unmarshal(handleAuthParse(req), &env); err != nil {
		t.Fatal(err)
	}
	if !env.OK {
		t.Fatal("不该报错，应礼貌地交还")
	}
	if env.Result.Handled {
		t.Fatal("别人的凭证不该认领")
	}
}

/* ------------------------------------------------------- 管理页 / 状态 */

// 这条是安全底线：状态页和 JSON 都可能被截图/转发，绝不能带 token。
func TestStatusNeverLeaksToken(t *testing.T) {
	const secret = "SUPER-SECRET-SERVICE-TOKEN-DO-NOT-LEAK"
	noteParsed("test-user-1", "u1", secret, true, "单元测试")

	page := renderStatusHTML(collectStatus())
	if strings.Contains(page, secret) {
		t.Fatal("HTML 状态页泄漏了 service_token")
	}
	if !strings.Contains(page, fmt.Sprintf("%d 字符", len(secret))) {
		t.Error("页面应当显示 token 长度（用于确认「有 token」但不泄漏内容）")
	}

	blob, err := json.Marshal(collectStatus())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), secret) {
		t.Fatal("JSON 状态泄漏了 service_token")
	}
	if strings.Contains(string(blob), `"tok"`) {
		t.Fatal("JSON 里出现了未导出字段 tok")
	}
}

func TestCredSnapshotIndependent(t *testing.T) {
	noteParsed("snap-1", "u", "tok-abc", true, "x")
	snap := credSnapshot()
	for i := range snap {
		if snap[i].AuthID == "snap-1" && snap[i].TokenLen != 7 {
			t.Errorf("TokenLen = %d, want 7", snap[i].TokenLen)
		}
	}
	// 改快照不该影响存储
	for i := range snap {
		snap[i].TokenLen = 999
	}
	again := credSnapshot()
	for _, c := range again {
		if c.AuthID == "snap-1" && c.TokenLen != 7 {
			t.Fatal("credSnapshot 返回的应当是拷贝")
		}
	}
}

func TestManagementRegisterShape(t *testing.T) {
	var env struct {
		OK     bool `json:"ok"`
		Result struct {
			Routes []struct {
				Method string `json:"Method"`
				Path   string `json:"Path"`
			} `json:"routes"`
			Resources []struct {
				Path string `json:"Path"`
				Menu string `json:"Menu"`
			} `json:"resources"`
		} `json:"result"`
	}
	if err := json.Unmarshal(handleManagementRegister(nil), &env); err != nil {
		t.Fatal(err)
	}
	if !env.OK {
		t.Fatal("management.register 应当成功")
	}
	wantRoutes := map[string]string{
		"/plugins/mimo/status":          "GET",
		"/plugins/mimo/login/start":     "POST",
		"/plugins/mimo/login/password":  "POST",
		"/plugins/mimo/login/verify":    "POST",
		"/plugins/mimo/login/cancel":    "POST",
		"/plugins/mimo/login/status":    "GET",
	}
	if len(env.Result.Routes) != len(wantRoutes) {
		t.Fatalf("路由注册不对: %+v", env.Result.Routes)
	}
	for _, r := range env.Result.Routes {
		if wantRoutes[r.Path] != r.Method {
			t.Errorf("路由 %s 方法 %s 不对（期望 %s）", r.Path, r.Method, wantRoutes[r.Path])
		}
	}
	if len(env.Result.Resources) != 1 || env.Result.Resources[0].Path != "/login" {
		t.Errorf("资源注册不对: %+v", env.Result.Resources)
	}
	// 需求②：资源路由的 Menu 必须全部留空 —— 宿主对空 Menu 的资源路由不生成
	// 面板菜单（snapshot.go:130-134），否则 management.html#/plugin-pages/mimo 会重现。
	for _, r := range env.Result.Resources {
		if strings.TrimSpace(r.Menu) != "" {
			t.Errorf("资源 %s 带了 Menu=%q，面板会重新出现插件菜单（需求②禁止）", r.Path, r.Menu)
		}
	}
}

// 资源页走 HTML，管理路由走 JSON —— 这条区分是鉴权边界，不能搞反。
func TestManagementHandleRoutesByPath(t *testing.T) {
	pageReq, _ := json.Marshal(map[string]any{"Method": "GET", "Path": "/v0/resource/plugins/mimo/status"})
	var pageEnv struct {
		OK     bool `json:"ok"`
		Result struct {
			Headers map[string][]string `json:"Headers"`
			Body    []byte              `json:"Body"`
		} `json:"result"`
	}
	if err := json.Unmarshal(handleManagement(pageReq), &pageEnv); err != nil {
		t.Fatal(err)
	}
	if ct := pageEnv.Result.Headers["content-type"]; len(ct) == 0 || !strings.Contains(ct[0], "text/html") {
		t.Fatalf("资源页应返回 HTML，得到 %v", pageEnv.Result.Headers)
	}

	apiReq, _ := json.Marshal(map[string]any{"Method": "GET", "Path": "/v0/management/plugins/mimo/status"})
	var apiEnv struct {
		Result struct {
			Headers map[string][]string `json:"Headers"`
		} `json:"result"`
	}
	if err := json.Unmarshal(handleManagement(apiReq), &apiEnv); err != nil {
		t.Fatal(err)
	}
	if ct := apiEnv.Result.Headers["content-type"]; len(ct) == 0 || !strings.Contains(ct[0], "application/json") {
		t.Fatalf("管理路由应返回 JSON，得到 %v", apiEnv.Result.Headers)
	}

	// 扫码登录管理路由必须包成 {StatusCode,Headers,Body} —— 宿主要求 management.handle
	// 的 RPC 结果是 HTTP 响应描述，直接返回业务信封会让宿主报 handler failed。
	stReq, _ := json.Marshal(map[string]any{
		"Method": "GET", "Path": "/v0/management/plugins/mimo/login/status",
		"Query": map[string][]string{"session": {"none"}},
	})
	var stEnv struct {
		OK     bool `json:"ok"`
		Result struct {
			StatusCode int `json:"StatusCode"`
			Headers    map[string][]string
			Body       []byte `json:"Body"`
		} `json:"result"`
	}
	if err := json.Unmarshal(handleManagement(stReq), &stEnv); err != nil {
		t.Fatal(err)
	}
	if !stEnv.OK || stEnv.Result.StatusCode != 404 {
		t.Fatalf("login/status(不存在会话) 应包成 HTTP 404 描述: %+v", stEnv)
	}
	if !strings.Contains(string(stEnv.Result.Body), "not_found") {
		t.Fatalf("404 body = %s", stEnv.Result.Body)
	}
}

/* -------------------------------------- 配额 / 图像 / 扫码登录（本轮新增） */

func TestQuotaDescribeAndCapability(t *testing.T) {
	var env struct {
		OK     bool `json:"ok"`
		Result struct {
			SupportedProviders []string `json:"supported_providers"`
			DisplayName        string   `json:"display_name"`
			SupportsReset      bool     `json:"supports_reset"`
		} `json:"result"`
	}
	if err := json.Unmarshal(handleQuotaDescribe(nil), &env); err != nil {
		t.Fatal(err)
	}
	if !env.OK || len(env.Result.SupportedProviders) != 1 || env.Result.SupportedProviders[0] != "mimo" {
		t.Fatalf("quota.describe 不对: %+v", env)
	}
	if env.Result.SupportsReset {
		t.Fatal("supports_reset 必须为 false：MiMo 用量周期由上游管理")
	}

	var reg struct {
		OK     bool `json:"ok"`
		Result struct {
			Capabilities map[string]any `json:"capabilities"`
		} `json:"result"`
	}
	if err := json.Unmarshal(handleRegister(nil), &reg); err != nil {
		t.Fatal(err)
	}
	if v, _ := reg.Result.Capabilities["quota_provider"].(bool); !v {
		t.Fatal("capabilities.quota_provider 必须为 true，否则宿主不调 quota.*")
	}
	fmts, _ := reg.Result.Capabilities["executor_input_formats"].([]any)
	found := false
	for _, f := range fmts {
		if s, _ := f.(string); s == "openai-image" {
			found = true
		}
	}
	if !found {
		t.Fatalf("executor_input_formats 必须含 openai-image: %v", fmts)
	}
}

func TestQuotaFetchMapping(t *testing.T) {
	// 上游替身：实测形状 {"code":0,"data":{"percent":84.4,"resetDate":"2026-09-23","resetAt":...}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/user/usage" {
			t.Errorf("请求路径 = %s", r.URL.Path)
		}
		if !strings.Contains(r.Header.Get("Cookie"), "serviceToken=st-1") {
			t.Errorf("Cookie = %s", r.Header.Get("Cookie"))
		}
		_, _ = w.Write([]byte(`{"code":0,"message":"success","data":{"percent":84.4,"resetDate":"2026-09-23","resetAt":1790155916}}`))
	}))
	defer srv.Close()
	setConfig(cfg{BaseURL: srv.URL, SID: "mimopc", RefreshAfter: "6h", ModelTTL: "10m"})
	// 宿主的 QuotaFetchRequest 不带 StorageJSON，凭证走插件内存缓存这条路
	rememberCred("mimo-1", mimoCred{Type: "mimo", ServiceToken: "st-1", UserID: "1", PassToken: "pt-1"})

	in, _ := json.Marshal(map[string]any{"auth_id": "mimo-1", "provider": "mimo"})
	raw := handleQuotaFetch(in)
	var env struct {
		OK     bool   `json:"ok"`
		Result struct {
			Summary []struct {
				Key   string  `json:"key"`
				Value float64 `json:"value"`
			} `json:"summary"`
			Groups []struct {
				Buckets []struct {
					RemainingFraction float64 `json:"remainingFraction"`
					ResetTime         string  `json:"resetTime"`
					Window            string  `json:"window"`
				} `json:"buckets"`
			} `json:"groups"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if !env.OK {
		t.Fatalf("quota.fetch 失败: %s", raw)
	}
	if len(env.Result.Summary) != 1 || env.Result.Summary[0].Value != 84.4 {
		t.Fatalf("summary = %+v", env.Result.Summary)
	}
	if len(env.Result.Groups) != 1 || len(env.Result.Groups[0].Buckets) != 1 {
		t.Fatalf("groups = %+v", env.Result.Groups)
	}
	b := env.Result.Groups[0].Buckets[0]
	if b.RemainingFraction < 0.843 || b.RemainingFraction > 0.845 {
		t.Fatalf("remainingFraction = %v（应为 percent/100）", b.RemainingFraction)
	}
	if !strings.HasPrefix(b.ResetTime, "2026-09-23") {
		t.Fatalf("resetTime = %s", b.ResetTime)
	}

	// 顺手采集的快照要能进 auth JSON / 状态页
	snap := usageSnapshotFor("mimo-1")
	if snap == nil {
		t.Fatal("usageSnapshotFor 不应为 nil")
	}
	if v, _ := snap["remaining_percent"].(float64); v != 84.4 {
		t.Fatalf("snapshot = %+v", snap)
	}
}

func TestQuotaFetchNoCred(t *testing.T) {
	// 多放一条无关凭证，确保 provider 级兜底不会误命中
	rememberCred("mimo-dummy", mimoCred{Type: "mimo", ServiceToken: "x", UserID: "2"})
	in, _ := json.Marshal(map[string]any{"auth_id": "mimo-nonexistent"})
	raw := handleQuotaFetch(in)
	var env struct {
		OK    bool `json:"ok"`
		Error struct {
			HTTPStatus int `json:"http_status"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if env.OK || env.Error.HTTPStatus != 401 {
		t.Fatalf("无凭证应返回 401 信封: %s", raw)
	}
}

func TestIsImageExec(t *testing.T) {
	cases := []struct {
		format, source, model string
		want                  bool
	}{
		{"openai-image", "", "Doubao-Seedream-5.0-pro", true},
		{"", "openai-image", "Doubao-Seedream-5.0-pro", true},
		{"", "", "Doubao-Seedream-5.0-pro", true}, // 宿主没带格式串 → 模型名判据
		{"chat-completions", "", "mimo-v2.5-tts", false},
		{"chat-completions", "", "mimo-x-pro-preview", false},
		{"chat-completions", "", "Doubao-Seedream-5.0-pro", false}, // 宿主说 chat 就走 chat
	}
	for _, c := range cases {
		got := isImageExec(execRequest{Format: c.format, SourceFormat: c.source, Model: c.model})
		if got != c.want {
			t.Errorf("isImageExec(%q,%q,%q) = %v, want %v", c.format, c.source, c.model, got, c.want)
		}
	}
}

func TestImageModelTypePassesCPAWhitelist(t *testing.T) {
	// CPA /v1/images/* 白名单只认 registry ModelInfo.Type == "openai-image"
	// （openai_images_handlers.go:257-258），宿主原样收录插件上报的 Type。
	info := toModelInfo(upstreamModel{ModelName: "Doubao-Seedream-5.0-pro", ModelType: "IMAGE_GENERATION"})
	if info.Type != "openai-image" {
		t.Fatalf("Type = %q，必须是 openai-image 才能过 CPA 图像白名单", info.Type)
	}
	if len(info.SupportedOutputModalities) != 1 || info.SupportedOutputModalities[0] != "image" {
		t.Fatalf("modalities = %v", info.SupportedOutputModalities)
	}
}

func TestFindQRTokens(t *testing.T) {
	// 小米 QR 长轮询响应的包裹层级不固定（result/data/顶层都可能），全树扫描
	body := []byte(`{"code":0,"result":{"data":{"cUserId":"9","passToken":"V1:secret","userId":1000000001}}}`)
	tok := findQRTokens(body)
	if tok == nil || tok.PassToken != "V1:secret" || tok.UserID != "1000000001" || tok.CUserID != "9" {
		t.Fatalf("findQRTokens = %+v", tok)
	}
	if findQRTokens([]byte(`{"code":70016,"desc":"登录验证失败"}`)) != nil {
		t.Fatal("未扫码/失败响应不应解析出凭证")
	}
	if findQRTokens([]byte("not-json")) != nil {
		t.Fatal("非法 JSON 应返回 nil")
	}
}

func TestLoginStatusNeverLeaksQR(t *testing.T) {
	// qr/lp/loginUrl 都是凭证等价物：状态输出/页面 JSON 绝不能带它们
	s := &qrSession{
		ID: "sess-test", CreatedAt: time.Now(), Status: "pending", Message: "等待扫码",
		LP: "https://x/lp?token=SECRETLP", QR: "https://x/qr?token=SECRETQR",
		LoginURL: "https://x/l?s=SECRETL", QRTips: "用小米手机扫码",
	}
	qrStore.Lock()
	qrStore.m["sess-test"] = s
	qrStore.Unlock()
	defer func() {
		qrStore.Lock()
		delete(qrStore.m, "sess-test")
		qrStore.Unlock()
	}()

	out := string(handleLoginStatus(map[string][]string{"session": {"sess-test"}}))
	for _, secret := range []string{"SECRETLP", "SECRETQR", "SECRETL"} {
		if strings.Contains(out, secret) {
			t.Fatalf("状态输出泄漏 %s: %s", secret, out)
		}
	}

	// 登录页 HTML 同样不能泄漏 lp（qr 图地址按设计展示给扫码者，但 lp 绝不出）
	page := renderLoginPage(s, "")
	if strings.Contains(page, "SECRETLP") || strings.Contains(page, "SECRETL") {
		t.Fatal("登录页泄漏了 lp/loginUrl")
	}
}

func TestAuthDataCarriesUsageSnapshot(t *testing.T) {
	// 用量快照必须放在 Metadata：宿主落盘 = StorageJSON ∪ Metadata，
	// 放别处会在下次 auth.refresh 时被抹掉（内核 mergedStorageJSON 语义）。
	noteUsage("mimo-9", &usageData{Percent: 50, ResetDate: "2026-10-01"}, nil)
	data := authData(mimoCred{Type: "mimo", PassToken: "p", UserID: "9"}, "", "mimo-9", time.Now())
	md, _ := data["Metadata"].(map[string]any)
	snap, ok := md["usage_snapshot"].(map[string]any)
	if !ok {
		t.Fatalf("Metadata 缺 usage_snapshot: %+v", md)
	}
	if v, _ := snap["remaining_percent"].(float64); v != 50 {
		t.Fatalf("usage_snapshot = %+v", snap)
	}
	// 快照绝不能含凭证值
	raw, _ := json.Marshal(data)
	if strings.Contains(string(raw), "pass_token") && strings.Contains(string(raw), `"p"`) {
		// pass_token 本身在 StorageJSON 里是设计使然（auth 文件本来就要存凭证），
		// 这里只断言 usage_snapshot 里没有它。
		snapRaw, _ := json.Marshal(snap)
		if strings.Contains(string(snapRaw), "pass") {
			t.Fatalf("usage_snapshot 泄漏凭证: %s", snapRaw)
		}
	}
}

/* ------------------------------------------------- 实网测试（可选开启） */

// TestLiveSSORefresh 直接跑一次真实的 SSO 两阶段交换。
// 默认跳过；设 MIMO_AUTH_FILE 指向一个含 pass_token 的 mimo.json 才执行。
//
//	MIMO_AUTH_FILE=/path/to/mimo.json go test -run Live -v
func TestLiveSSORefresh(t *testing.T) {
	path := os.Getenv("MIMO_AUTH_FILE")
	if path == "" {
		t.Skip("未设 MIMO_AUTH_FILE，跳过实网测试")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var c mimoCred
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	c.normalize()
	if c.PassToken == "" {
		t.Skip("凭证里没有 pass_token，无法测续期")
	}

	req, _ := json.Marshal(map[string]any{"AuthID": "live-test", "StorageJSON": raw})
	var env struct {
		OK     bool `json:"ok"`
		Error  *struct{ Message string } `json:"error"`
		Result struct {
			Auth struct {
				StorageJSON []byte `json:"StorageJSON"`
			} `json:"Auth"`
			NextRefreshAfter string `json:"NextRefreshAfter"`
		} `json:"result"`
	}
	if err := json.Unmarshal(handleAuthRefresh(req), &env); err != nil {
		t.Fatal(err)
	}
	if !env.OK {
		msg := ""
		if env.Error != nil {
			msg = env.Error.Message
		}
		t.Fatalf("auth.refresh 失败: %s", msg)
	}

	var refreshed mimoCred
	if err := json.Unmarshal(env.Result.Auth.StorageJSON, &refreshed); err != nil {
		t.Fatal(err)
	}
	if refreshed.ServiceToken == "" {
		t.Fatal("续期后仍没有 service_token")
	}
	if env.Result.NextRefreshAfter == "" {
		t.Fatal("缺少 NextRefreshAfter")
	}
	// 只报告长度，不打印 token
	t.Logf("续期成功: service_token=%d 字符, next=%s", len(refreshed.ServiceToken), env.Result.NextRefreshAfter)
}

// TestLiveReactiveRefresh 验证「上游 401 → 自动续期 → 重试」这条链路。
//
// 做法：故意把 service_token 写坏（上游实测会回 401），然后直接调 executor，
// 期望它自己换完 token 并把请求跑成功。同时确认插件缓存被更新 ——
// 否则下一个请求还会拿着旧的坏 token 再撞一次 401。
func TestLiveReactiveRefresh(t *testing.T) {
	path := os.Getenv("MIMO_AUTH_FILE")
	if path == "" {
		t.Skip("未设 MIMO_AUTH_FILE，跳过实网测试")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var c mimoCred
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	c.normalize()
	if c.PassToken == "" {
		t.Skip("凭证里没有 pass_token，无法测续期")
	}

	const poisoned = "definitely-not-a-valid-service-token"
	authID := deriveAuthID(c)
	c.ServiceToken = poisoned
	bad, _ := json.Marshal(c)

	req, _ := json.Marshal(map[string]any{
		"AuthID":      authID,
		"Model":       "mimo-x-flash-preview",
		"Format":      "openai",
		"Payload":     []byte(`{"model":"mimo-x-flash-preview","messages":[{"role":"user","content":"hi"}],"max_tokens":8}`),
		"StorageJSON": bad,
	})

	start := time.Now()
	var env struct {
		OK     bool `json:"ok"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
		Result struct {
			Payload []byte `json:"Payload"`
		} `json:"result"`
	}
	if err := json.Unmarshal(handleExecute(req), &env); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)

	if !env.OK {
		msg := ""
		if env.Error != nil {
			msg = env.Error.Message
		}
		t.Fatalf("坏 token 应当被自动续期救回来，实际失败: %s", msg)
	}
	t.Logf("坏 token 请求成功（含一次续期+重试），耗时 %s，上游返回 %d 字节", elapsed.Round(time.Millisecond), len(env.Result.Payload))

	if tok := cachedToken(authID); tok == "" || tok == poisoned {
		t.Fatalf("缓存没被更新（len=%d），下个请求会再撞一次 401", len(tok))
	}
	t.Log("插件缓存已更新为续期后的 token")
}

/* ------------- 面板 #/oauth SSO 桥接 + 账号密码登录（auth.login.start/poll） ------------- */

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

// startPassportMock 起一个小米 passport 替身，并把插件的端点变量/宿主落盘钩子
// 全部指过来。返回 (server, lp 通道, host.auth.save 收集通道)。
func startPassportMock(t *testing.T) (*httptest.Server, chan []byte, chan []byte) {
	t.Helper()
	lpCh := make(chan []byte, 2)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/pass/serviceLogin":
			// OTP Step7 的 location：下发认证 cookie（模拟验证通过的浏览器会话）
			if r.URL.Query().Get("verified") == "1" {
				w.Header().Add("Set-Cookie", "serviceAuth=verified; Path=/")
				_, _ = w.Write([]byte(`{"code":0,"result":"ok"}`))
				return
			}
			// OTP Step8：jar 带着认证 cookie 重跑 serviceLogin → 返回完整凭证
			//（真实小米就是这个行为：验证 cookie 让 serviceLogin 直接 code=0 全字段）
			if c, err := r.Cookie("serviceAuth"); err == nil && c.Value == "verified" {
				fmt.Fprintf(w, `{"code":0,"result":"ok","userId":"777","passToken":"V1:otptoken","cUserId":"c777","location":%q,"nonce":4321,"ssecurity":"mocksec"}`,
					srv.URL+"/sts?user=777")
				return
			}
			// 兼两个消费者：passwordAuthenticate 阶段0 读 qs/_sign/callback；
			// exchangeServiceToken 阶段1 读 code/location/nonce。
			fmt.Fprintf(w, `{"code":0,"qs":"%%3Fsid%%3Dmimopc%%26_json%%3Dtrue","_sign":"MOCKSIGN","nonce":4321,"callback":%q,"location":%q}`,
				srv.URL+"/sts", srv.URL+"/sts?user=mock")
		case "/pass/serviceLoginAuth2":
			_ = r.ParseForm()
			switch r.Form.Get("user") {
			case "good@example.com":
				fmt.Fprintf(w, `{"code":0,"location":%q,"result":"ok"}`,
					srv.URL+"/finish?passToken=V1:pwtoken&userId=77&cUserId=c77")
			case "risk@example.com":
				// 小米「新设备保护」：notificationUrl → 插件自动跑 identity 发码流程
				fmt.Fprintf(w, `{"code":0,"result":"ok","securityStatus":16,"notificationUrl":%q,"location":"","description":"成功"}`,
					srv.URL+"/fe/service/identity/authStart?sid=mimopc&context=OTPCTX123")
			case "cookie@example.com":
				fmt.Fprintf(w, `{"code":0,"location":%q,"result":"ok"}`, srv.URL+"/cookie-only")
			default:
				_, _ = w.Write([]byte(`{"code":70016,"desc":"登录验证失败","result":"error","location":""}`))
			}
		/* ---- 小米 identity 新设备验证流程（OTP） ---- */
		case "/fe/service/identity/authStart":
			w.Header().Add("Set-Cookie", "idrs=mocksession; Path=/")
			_, _ = w.Write([]byte("<html>identity authStart</html>"))
		case "/identity/list":
			_, _ = w.Write([]byte(`&&&START&&&{"code":0,"flag":4,"notifyPhone":"138****1234","result":"ok"}`))
		case "/identity/auth/verifyPhone":
			if r.Method == http.MethodPost {
				// 提交验证码：123456 正确，其余错误
				_ = r.ParseForm()
				if r.Form.Get("ticket") == "123456" {
					fmt.Fprintf(w, `{"code":0,"result":"ok","location":%q}`,
						srv.URL+"/pass/serviceLogin?sid=mimopc&_json=true&verified=1")
				} else {
					_, _ = w.Write([]byte(`&&&START&&&{"code":70013,"result":"error","desc":"验证码错误，请重新输入"}`))
				}
				return
			}
			// GET：触发发送
			_, _ = w.Write([]byte(`&&&START&&&{"code":0,"result":"ok"}`))
		case "/identity/auth/sendPhoneTicket":
			_, _ = w.Write([]byte(`&&&START&&&{"code":0,"result":"ok"}`))
		case "/finish":
			_, _ = w.Write([]byte(`{"code":0,"result":"ok"}`))
		case "/cookie-only":
			w.Header().Add("Set-Cookie", "passToken=V1:ck; Path=/")
			w.Header().Add("Set-Cookie", "userId=55; Path=/")
			w.Header().Add("Set-Cookie", "cUserId=c55; Path=/")
			_, _ = w.Write([]byte("ok"))
		case "/longPolling/loginUrl":
			fmt.Fprintf(w, `{"code":0,"loginUrl":"https://mock/login","qr":"https://mock/qr.png","lp":%q,"timeout":300,"qrTips":"用小米手机扫码"}`, srv.URL+"/lp")
		case "/lp":
			select {
			case b := <-lpCh:
				_, _ = w.Write(b)
			case <-time.After(4 * time.Second):
				_, _ = w.Write([]byte(`{"code":1,"desc":"poll wait timeout"}`))
			}
		case "/sts": // exchangeServiceToken 阶段2：下发 serviceToken
			w.Header().Add("Set-Cookie", "serviceToken=MOCKST; Path=/")
			_, _ = w.Write([]byte("ok"))
		case "/api/user/usage":
			_, _ = w.Write([]byte(`{"code":0,"data":{"percent":77.7,"resetDate":"2026-10-01","resetAt":1790000000}}`))
		default:
			http.NotFound(w, r)
		}
	}))

	oldQR, oldP1, oldP2, oldBase := qrLoginProvider, phase1URL, passAuth2URL, sidCallbackBase
	oldSave, oldCfg := hostAuthSave, config()
	qrLoginProvider, sidCallbackBase = srv.URL, srv.URL
	phase1URL, passAuth2URL = srv.URL+"/pass/serviceLogin", srv.URL+"/pass/serviceLoginAuth2"
	saved := make(chan []byte, 4)
	hostAuthSave = func(name string, raw []byte) error { saved <- raw; return nil }
	setConfig(cfg{BaseURL: srv.URL, SID: "mimopc", RefreshAfter: "6h", ModelTTL: "10m"})
	t.Cleanup(func() {
		qrLoginProvider, phase1URL, passAuth2URL, sidCallbackBase = oldQR, oldP1, oldP2, oldBase
		hostAuthSave = oldSave
		setConfig(oldCfg)
		srv.Close()
	})
	return srv, lpCh, saved
}

// TestAuthLoginStartPollRPCBridge —— 面板 #/oauth「SSO 登录」的完整宿主契约：
// auth.login.start 返回合法 State + 登录页 URL；poll 从 pending → success，
// Auth/Auths 是宿主 savePluginLoginRecords 的落盘输入。
func TestAuthLoginStartPollRPCBridge(t *testing.T) {
	_, lpCh, saved := startPassportMock(t)

	raw := handleAuthLoginStartRPC(mustJSON(map[string]any{
		"Provider": "mimo", "BaseURL": "http://127.0.0.1:8318/v0/management/oauth-callback"}))
	var env struct {
		OK     bool `json:"ok"`
		Result struct {
			Provider string `json:"Provider"`
			URL      string `json:"URL"`
			State    string `json:"State"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if !env.OK {
		t.Fatalf("auth.login.start 失败: %s", raw)
	}
	state := env.Result.State
	if state == "" {
		t.Fatal("State 不能为空：宿主 ValidateOAuthState 拒绝空 state → 502（旧实现面板登录不可用的根因）")
	}
	for _, r := range state {
		ok := r == '-' || r == '_' || r == '.' ||
			(r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		if !ok {
			t.Fatalf("State %q 含非法字符 %q，过不了宿主 ValidateOAuthState", state, r)
		}
	}
	if !strings.HasPrefix(env.Result.URL, "/v0/resource/plugins/mimo/login?session=") || !strings.Contains(env.Result.URL, state) {
		t.Fatalf("URL = %q：应为同源相对路径且携带会话 ID", env.Result.URL)
	}

	// poll #1：QR 长轮询吊在 mock /lp 上 → pending
	raw = handleAuthLoginPollRPC(mustJSON(map[string]any{"Provider": "mimo", "State": state}))
	var poll struct {
		OK     bool `json:"ok"`
		Result struct {
			Status  string `json:"Status"`
			Message string `json:"Message"`
			Auth    struct {
				Provider    string         `json:"Provider"`
				ID          string         `json:"ID"`
				FileName    string         `json:"FileName"`
				StorageJSON []byte         `json:"StorageJSON"`
				Metadata    map[string]any `json:"Metadata"`
			} `json:"Auth"`
			Auths []map[string]any `json:"Auths"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &poll); err != nil {
		t.Fatal(err)
	}
	if !poll.OK || poll.Result.Status != "pending" {
		t.Fatalf("扫码未确认时应 pending: %s", raw)
	}

	// 扫码确认：mock lp 返回凭证
	lpCh <- []byte(`{"code":0,"result":{"cUserId":"c42","passToken":"V1:bridgetoken","userId":4242}}`)
	deadline := time.Now().Add(10 * time.Second)
	for {
		raw = handleAuthLoginPollRPC(mustJSON(map[string]any{"Provider": "mimo", "State": state}))
		_ = json.Unmarshal(raw, &poll)
		if poll.Result.Status != "pending" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待登录完成超时: %s", raw)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if poll.Result.Status != "success" {
		t.Fatalf("扫码确认后应 success: %s", raw)
	}

	a := poll.Result.Auth
	if a.Provider != "mimo" || a.FileName != "mimo.json" || a.ID != "mimo-4242" {
		t.Fatalf("Auth = %+v", a)
	}
	var stored mimoCred
	if err := json.Unmarshal(a.StorageJSON, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.PassToken != "V1:bridgetoken" || stored.UserID != "4242" || stored.ServiceToken != "MOCKST" {
		t.Fatalf("StorageJSON 凭证不对: %+v", stored)
	}
	if len(poll.Result.Auths) != 1 {
		t.Fatalf("Auths 应恰有 1 条: %v", poll.Result.Auths)
	}
	// 用量快照随 AuthData.Metadata 交给宿主 → 落盘后 auth-files 详情可见
	if snap, _ := a.Metadata["usage_snapshot"].(map[string]any); snap == nil {
		t.Logf("警告：Metadata 无 usage_snapshot（异步采集可能未完成）: %+v", a.Metadata)
	}

	// REST/资源路径与宿主 poll 是双通道落盘：host.auth.save 也应被调用
	select {
	case b := <-saved:
		var c mimoCred
		if err := json.Unmarshal(b, &c); err != nil || c.PassToken != "V1:bridgetoken" {
			t.Fatalf("host.auth.save 收到 %s", b)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("finishLogin 未调用 hostAuthSave")
	}

	// poll 幂等：success 后再 poll 返回同一份
	raw2 := handleAuthLoginPollRPC(mustJSON(map[string]any{"Provider": "mimo", "State": state}))
	if !strings.Contains(string(raw2), `"success"`) {
		t.Fatalf("重复 poll 应幂等 success: %s", raw2)
	}
	// 未知 state：宿主要求 error（面板显示失败而不是永远转圈）
	raw3 := handleAuthLoginPollRPC(mustJSON(map[string]any{"Provider": "mimo", "State": "deadbeefdeadbeefdeadbeefdeadbeef"}))
	if !strings.Contains(string(raw3), `"error"`) {
		t.Fatalf("未知会话应 error: %s", raw3)
	}
}

// TestPasswordLoginFlow —— 通道 2：serviceLoginAuth2 账号密码登录。
// 小米风控如实报错、密码不回显、成功路径落盘。
func TestPasswordLoginFlow(t *testing.T) {
	_, _, saved := startPassportMock(t)

	var envErr struct {
		OK    bool `json:"ok"`
		Error struct {
			Code       string `json:"code"`
			Message    string `json:"message"`
			HTTPStatus int    `json:"http_status"`
		} `json:"error"`
	}

	// 密码错误：小米 code=70016 → 401 + 原样语义
	raw := handleLoginPassword(mustJSON(map[string]any{"user": "bad@example.com", "password": "wrongpw"}))
	_ = json.Unmarshal(raw, &envErr)
	if envErr.OK || envErr.Error.HTTPStatus != 401 || !strings.Contains(envErr.Error.Message, "70016") {
		t.Fatalf("密码错误应 401 + 小米语义: %s", raw)
	}

	// 小米新设备保护：notificationUrl → 自动发码 → awaiting-otp（含掩码）
	raw = handleLoginPassword(mustJSON(map[string]any{"user": "risk@example.com", "password": "whatever"}))
	var otpEnv struct {
		OK     bool `json:"ok"`
		Result struct {
			Session string `json:"session"`
			Status  string `json:"status"`
			Message string `json:"message"`
			OTP     struct {
				Method string `json:"method"`
				Notify string `json:"notify"`
			} `json:"otp"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &otpEnv)
	if !otpEnv.OK || otpEnv.Result.Status != "awaiting-otp" {
		t.Fatalf("新设备保护应进入 awaiting-otp: %s", raw)
	}
	if otpEnv.Result.OTP.Method != "Phone" || otpEnv.Result.OTP.Notify != "138****1234" ||
		!strings.Contains(otpEnv.Result.Message, "138****1234") {
		t.Fatalf("OTP 状态应带验证方式与掩码: %s", raw)
	}

	// 验证码错误 → otp_failed 401，会话保持 awaiting-otp（可重试）
	raw = handleLoginVerify(mustJSON(map[string]any{"session": otpEnv.Result.Session, "code": "000000"}))
	var vErr struct {
		OK    bool `json:"ok"`
		Error struct {
			Code       string `json:"code"`
			HTTPStatus int    `json:"http_status"`
			Message    string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &vErr)
	if vErr.OK || vErr.Error.HTTPStatus != 401 || !strings.Contains(vErr.Error.Message, "验证码") {
		t.Fatalf("错误验证码应 401: %s", raw)
	}
	if s := getQRSession(otpEnv.Result.Session); s == nil || s.Status != "awaiting-otp" {
		t.Fatal("验证码错误后会话应保持 awaiting-otp")
	}

	// 验证码正确 → 完整登录成功 + 落盘（mock 返回 V1:otptoken/userId=777）
	raw = handleLoginVerify(mustJSON(map[string]any{"session": otpEnv.Result.Session, "code": "123456"}))
	var otpOK struct {
		OK     bool `json:"ok"`
		Result struct {
			Status   string `json:"status"`
			UserID   string `json:"user_id"`
			AuthFile string `json:"auth_file"`
		} `json:"result"`
	}
	_ = json.Unmarshal(raw, &otpOK)
	if !otpOK.OK || otpOK.Result.Status != "success" || otpOK.Result.UserID != "777" {
		t.Fatalf("OTP 验证码正确应登录成功: %s", raw)
	}
	for _, secret := range []string{"123456", "000000", "whatever"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("响应泄漏验证码/密码 %q: %s", secret, raw)
		}
	}
	select {
	case b := <-saved:
		var c mimoCred
		if err := json.Unmarshal(b, &c); err != nil || c.PassToken != "V1:otptoken" || c.UserID != "777" {
			t.Fatalf("OTP 登录后 host.auth.save 收到 %s", b)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OTP 登录成功后未落盘")
	}

	// 空密码：400
	raw = handleLoginPassword(mustJSON(map[string]any{"user": "good@example.com"}))
	_ = json.Unmarshal(raw, &envErr)
	if envErr.OK || envErr.Error.HTTPStatus != 400 {
		t.Fatalf("缺密码应 400: %s", raw)
	}

	// 正确：location query 携带 passToken/userId
	raw = handleLoginPassword(mustJSON(map[string]any{"user": "good@example.com", "password": "correct-horse"}))
	var ok struct {
		OK     bool `json:"ok"`
		Result struct {
			Session  string `json:"session"`
			Status   string `json:"status"`
			UserID   string `json:"user_id"`
			AuthFile string `json:"auth_file"`
		} `json:"result"`
	}
	_ = json.Unmarshal(raw, &ok)
	if !ok.OK {
		t.Fatalf("密码正确应成功: %s", raw)
	}
	if ok.Result.Status != "success" || ok.Result.UserID != "77" || ok.Result.AuthFile != "mimo.json" {
		t.Fatalf("result = %+v", ok.Result)
	}
	for _, secret := range []string{"correct-horse", "wrongpw", "whatever"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("响应泄漏密码 %q: %s", secret, raw)
		}
	}
	select {
	case b := <-saved:
		var c mimoCred
		if err := json.Unmarshal(b, &c); err != nil || c.PassToken != "V1:pwtoken" || c.UserID != "77" {
			t.Fatalf("host.auth.save 收到 %s", b)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("密码登录成功后未落盘")
	}

	// cookie 通道：location 响应靠 Set-Cookie 下发 passToken（真实小米的典型形态）
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	tok, err := collectTokensFromLocation(client, passAuth2URL[:strings.Index(passAuth2URL, "/pass/")] + "/cookie-only", "mimopc")
	if err != nil || tok.PassToken != "V1:ck" || tok.UserID != "55" || tok.CUserID != "c55" {
		t.Fatalf("cookie 通道收集失败 tok=%+v err=%v", tok, err)
	}
}

// TestResourcePasswordRouteWrap —— 登录页密码表单走资源路由（POST），错误包成
// {StatusCode,Headers,Body}；且响应绝不回显密码。
func TestResourcePasswordRouteWrap(t *testing.T) {
	req, _ := json.Marshal(map[string]any{
		"Method": "POST", "Path": "/v0/resource/plugins/mimo/login",
		"Query": map[string][]string{"session": {"none"}},
		"Body":  []byte(`{"session":"none","user":"u","password":""}`),
	})
	var env struct {
		OK     bool `json:"ok"`
		Result struct {
			StatusCode int    `json:"StatusCode"`
			Body       []byte `json:"Body"`
		} `json:"result"`
	}
	if err := json.Unmarshal(handleManagement(req), &env); err != nil {
		t.Fatal(err)
	}
	if env.Result.StatusCode != 400 || !strings.Contains(string(env.Result.Body), "invalid_request") {
		t.Fatalf("空密码应 HTTP 400 invalid_request: %+v body=%s", env, env.Result.Body)
	}

	// 面板 OAuth 的 manage 路由也注册了 password
	reg, _ := json.Marshal(map[string]any{"Method": "POST", "Path": "/v0/management/plugins/mimo/login/password",
		"Body": []byte(`{"user":"x","password":""}`)})
	var mEnv struct {
		Result struct {
			StatusCode int    `json:"StatusCode"`
			Body       []byte `json:"Body"`
		} `json:"result"`
	}
	if err := json.Unmarshal(handleManagement(reg), &mEnv); err != nil {
		t.Fatal(err)
	}
	if mEnv.Result.StatusCode != 400 {
		t.Fatalf("管理路由 password 缺参应 400: %+v", mEnv)
	}
}

/* ------------------------------------------------------------ 用量 → label 落盘 */

// TestPersistUsageToAuthFiles —— 用量快照经 host.auth.list/get/save 通道写进
// auth 文件的 label + usage_snapshot；label/percent 无变化时防抖跳过。
func TestPersistUsageToAuthFiles(t *testing.T) {
	// 凭证缓存：persist 按 user_id 匹配文件
	authID := providerKey + "-77"
	rememberCred(authID, mimoCred{UserID: "77", ServiceToken: "tokA", PassToken: "pA"})
	noteUsage(authID, &usageData{Percent: 82.2, ResetDate: "2026-09-23"}, nil)

	var saveCalls int
	var savedName string
	var savedPayload map[string]any

	oldList, oldGet, oldSave := hostAuthListCall, hostAuthGetCall, hostAuthSaveFileCall
	t.Cleanup(func() { hostAuthListCall, hostAuthGetCall, hostAuthSaveFileCall = oldList, oldGet, oldSave })

	// idx1 的文件 JSON：save 后更新，模拟宿主写盘后 get 返回新内容（防抖验证依赖它）
	idx1JSON := `{"json":{"user_id":"77","service_token":"tokA","label":"` + providerKey + `"}}`
	hostAuthListCall = func() (json.RawMessage, error) {
		return json.Marshal(map[string]any{"files": []map[string]any{
			{"auth_index": "idx1", "name": "mimo.json", "provider": providerKey, "label": providerKey},
			{"auth_index": "idx2", "name": "other.json", "provider": "codex"}, // 不相干 provider
			{"auth_index": "idx3", "name": "mimo-999.json", "provider": providerKey, "label": providerKey},
		}})
	}
	hostAuthGetCall = func(idx string) (json.RawMessage, error) {
		switch idx {
		case "idx1": // 属于本凭证（user_id=77）
			return json.RawMessage(idx1JSON), nil
		case "idx3": // 别的账号
			return json.RawMessage(`{"json":{"user_id":"999","service_token":"tokB"}}`), nil
		}
		return json.RawMessage(`{}`), nil
	}
	hostAuthSaveFileCall = func(name string, payload []byte) (json.RawMessage, error) {
		saveCalls++
		savedName = name
		_ = json.Unmarshal(payload, &savedPayload)
		idx1JSON = `{"json":` + string(payload) + `}`
		return json.RawMessage(`{"name":"` + name + `"}`), nil
	}

	persistUsageToAuthFiles(authID, usageSnapshotAsUsage(authID))

	if saveCalls != 1 || savedName != "mimo.json" {
		t.Fatalf("应恰好保存 mimo.json 一次：calls=%d name=%q", saveCalls, savedName)
	}
	wantLabel := "mimo (77) · 剩余 82% · 2026-09-23 重置"
	if got, _ := savedPayload["label"].(string); got != wantLabel {
		t.Fatalf("label = %q, want %q", got, wantLabel)
	}
	snap, _ := savedPayload["usage_snapshot"].(map[string]any)
	if snap == nil {
		t.Fatalf("payload 缺 usage_snapshot: %+v", savedPayload)
	}
	if pct, _ := snap["remaining_percent"].(float64); pct != 82.2 {
		t.Fatalf("usage_snapshot.remaining_percent = %v, want 82.2", pct)
	}
	// 红线检查的正确边界：auth 文件本体就该含凭证（这是它的职责），
	// 但展示数据（label/usage_snapshot）绝不允许携带任何 token。
	snapRaw, _ := json.Marshal(snap)
	labelRaw, _ := json.Marshal(savedPayload["label"])
	if strings.Contains(string(snapRaw), "tokA") || strings.Contains(string(snapRaw), "pA") ||
		strings.Contains(string(labelRaw), "tokA") {
		t.Fatalf("展示数据泄漏凭证: snap=%s label=%s", snapRaw, labelRaw)
	}

	// 防抖：同 percent 再次调用不再写盘
	persistUsageToAuthFiles(authID, usageSnapshotAsUsage(authID))
	if saveCalls != 1 {
		t.Fatalf("label/percent 未变不应重复写盘：calls=%d", saveCalls)
	}
}

func usageSnapshotAsUsage(authID string) *usageData {
	snap := usageSnapshotFor(authID)
	u := &usageData{}
	if snap != nil {
		if v, ok := snap["remaining_percent"].(float64); ok {
			u.Percent = v
		}
		if v, ok := snap["reset_date"].(string); ok {
			u.ResetDate = v
		}
	}
	return u
}

// TestUsageLabelAndLoginPage —— label 格式 + 登录页含管理密钥提交要素。
func TestUsageLabelAndLoginPage(t *testing.T) {
	if got := usageLabelFromSnap(providerKey, "", nil); got != providerKey {
		t.Fatalf("nil snap label = %q", got)
	}
	snap := map[string]any{"remaining_percent": 80.7, "reset_date": "2026-09-23"}
	want := "mimo (123) · 剩余 81% · 2026-09-23 重置"
	if got := usageLabelFromSnap(providerKey, "123", snap); got != want {
		t.Fatalf("label = %q, want %q", got, want)
	}

	page := renderLoginPage(nil, "")
	for _, needle := range []string{
		"pwKey",                                          // 管理密钥输入框
		"/v0/management/plugins/mimo/login/password",     // 提交走管理路由
		"X-Management-Key",                               // 鉴权 header
		"sessionStorage",                                 // 密钥只存标签页
		"提交失败：服务器返回 HTTP",                        // 非 JSON 防御文案
	} {
		if !strings.Contains(page, needle) {
			t.Fatalf("登录页缺少 %q", needle)
		}
	}
	// 密码绝不进页面
	if strings.Contains(page, "qwedc") {
		t.Fatal("登录页不应出现任何密码内容")
	}
}

// TestHostAuthSavePayloadShape —— host.auth.save 请求的 json 字段必须序列化为
// 凭证 JSON 对象原文。回归锁：线上故障是插件传 []byte → base64 字符串 → 宿主
// map 解析报 "invalid auth json: cannot unmarshal string into Go value of
// type map[string]interface {}"（pluginhost/auth_callbacks.go:277）。
func TestHostAuthSavePayloadShape(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"type": providerKey, "pass_token": "tok", "user_id": "1"})

	// 正确形态：json.RawMessage → 序列化为对象原文
	req, _ := json.Marshal(map[string]any{"name": providerKey + ".json", "json": json.RawMessage(raw)})
	var parsed map[string]any
	if err := json.Unmarshal(req, &parsed); err != nil {
		t.Fatal(err)
	}
	m, ok := parsed["json"].(map[string]any)
	if !ok || m["type"] != providerKey {
		t.Fatalf("宿主视角 json 字段应为对象（凭证原文）: %s", req)
	}

	// 反例：[]byte → base64 字符串（这正是故障形态），宿主解析必然报错
	badReq, _ := json.Marshal(map[string]any{"name": "x.json", "json": raw})
	var badParsed map[string]any
	_ = json.Unmarshal(badReq, &badParsed)
	var badMap map[string]any
	if err := json.Unmarshal([]byte(badParsed["json"].(string)), &badMap); err == nil {
		t.Fatalf("反例应复现宿主报错（base64 字符串无法解析为 map）: %s", badReq)
	}
	t.Logf("host.auth.save payload 形状正确: %s", req)
}

// TestRestoreUsageFromRaw —— 文件里的 usage_snapshot 恢复内存用量（重启自愈）。
func TestRestoreUsageFromRaw(t *testing.T) {
	id := providerKey + "-888"
	raw := []byte(`{"user_id":"888","service_token":"x","usage_snapshot":{"remaining_percent":64.4,"reset_date":"2026-10-01"}}`)
	restoreUsageFromRaw(id, raw)
	snap := usageSnapshotFor(id)
	if snap == nil {
		t.Fatal("restoreUsageFromRaw 未恢复快照")
	}
	if pct, _ := snap["remaining_percent"].(float64); pct != 64.4 {
		t.Fatalf("remaining_percent = %v, want 64.4", pct)
	}
	label := usageLabelFromSnap(providerKey, "888", snap)
	if !strings.Contains(label, "剩余 64%") {
		t.Fatalf("恢复后 label 应带用量: %q", label)
	}
}
