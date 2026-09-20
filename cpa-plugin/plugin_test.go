package main

import (
	"encoding/json"
	"os"
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
