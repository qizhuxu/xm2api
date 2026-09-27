package main

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

/*
 * 登录 —— Linux 服务器侧凭证获取。
 *
 * ## 逆向结论（详见 investigation-mimo-auth-report.md）
 *
 *   - 小米 passport 的 callback 按 sid 白名单校验，传自定义地址直接
 *     code=10025「Callback连接不合法」（实测）；sid=mimopc 的 callback 固定注册为
 *     https://mimo-server-cn.xiaomimimo.com/api/sts。
 *   - passToken 只落在 .account.xiaomi.com 域的 HttpOnly cookie 里。
 *   ⇒ 「回调地址」原案拿不到 passToken；可行的服务器侧通道有两条：
 *
 * ## 通道 1：扫码登录（凭证发给长轮询方）
 *
 *	GET account.xiaomi.com/longPolling/loginUrl?sid=<sid>&callback=<注册值>&qs=...
 *	  → {"code":0,"loginUrl":...,"qr":<二维码图URL>,"lp":<长轮询URL>,"timeout":300,...}
 *	用户手机扫码确认后，GET <lp> 的响应体里直接带 {userId,cUserId,passToken,...}。
 *	谁发起轮询，凭证就发给谁 —— 轮询在服务器上跑，凭证直接在服务器上产生。
 *
 * ## 通道 2：账号密码登录（serviceLoginAuth2）
 *
 *	GET  /pass/serviceLogin?sid=mimopc&_json=true        → {qs, _sign, callback, ...}
 *	POST /pass/serviceLoginAuth2                          → code=0 + location
 *	  form: sid/callback/qs/user/hash/_json/_locale/_sign
 *	  hash = uppercase(md5(password))                      （小米 web 登录同款）
 *	GET  <location>（不跟随重定向）                       → Set-Cookie passToken/userId
 *
 *	实测：端点存活、sid=mimopc 接受该形态（假账号返回 code=70016「登录验证失败」，
 *	结构与真实响应一致）。真实凭据下若触发风控（notificationUrl / captchaUrl /
 *	secondValidation），插件如实报错并提示改走扫码或方案一 —— 不做任何绕过。
 *	密码只在内存中流经本进程，不落盘、不进日志。
 *
 * ## 官方面板 #/oauth「SSO 登录」桥接（auth.login.start / auth.login.poll RPC）
 *
 *	CPA 宿主对插件 provider 的 OAuth 契约：
 *	  面板 GET /v0/management/mimo-auth-url
 *	    → host.StartLogin → 插件 auth.login.start RPC
 *	    → 返回 {Provider, URL, State, ExpiresAt}，宿主 RegisterPluginOAuthSession；
 *	  面板轮询 GET /v0/management/get-auth-status?state=<State>
 *	    → host.PollLogin → 插件 auth.login.poll RPC
 *	    → {Status: pending|success|error, Message, Auth/Auths}
 *	    → success 时宿主自己落盘 auth 文件（savePluginLoginRecords）。
 *
 *	State 校验（ValidateOAuthState）只允许 [A-Za-z0-9._-]：会话 ID 是 crypto/rand
 *	hex，天然合规。URL 返回**相对路径**（面板与资源页同源，浏览器自动解析到当前
 *	origin，远程部署时不会错指向 127.0.0.1）。
 *
 *	同一会话页同时提供扫码与账号密码两种登录方式 —— 面板按钮点开的是同一登录页。
 *
 * ## 安全
 *
 *   - 会话 ID 来自 crypto/rand（128 bit），资源页无鉴权也只能凭 ID 访问，5 分钟过期；
 *   - qr / lp / loginUrl 是凭证等价物：不进日志、不进状态页、过期即焚；
 *   - 账号密码仅在内存中用于向 passport 换 passToken，响应/日志/页面均不回显；
 *   - poll RPC / status 接口的输出里永远没有 token（Auth.StorageJSON 是给宿主的
 *     base64 凭证本体，走宿主 RPC 通道直接落盘，不经浏览器）。
 */

// 小米 passport 端点（变量便于单测替换成 httptest）。
var (
	qrLoginProvider  = "https://account.xiaomi.com"
	passAuth2URL     = "https://account.xiaomi.com/pass/serviceLoginAuth2"
	sidCallbackBase  = "" // 空 = 用 qrLoginProvider
	authLoginTimeout = 20 * time.Second
)

const (
	qrSessionTTL = 5 * time.Minute
	// sidCallbackFallback：sid=mimopc 的注册回调（实测 serviceLogin JSON 里返回的值）
	sidCallbackFallback = "https://mimo-server-cn.xiaomimimo.com/api/sts"
)

type qrSession struct {
	ID        string    `json:"session"`
	SID       string    `json:"-"`
	Mode      string    `json:"mode,omitempty"` // qr / password
	QR        string    `json:"-"`              // 凭证等价物，只在创建响应里给一次
	LoginURL  string    `json:"-"`
	LP        string    `json:"-"`
	QRTips    string    `json:"-"`
	Timeout   int       `json:"-"`
	CreatedAt time.Time `json:"created_at"`
	Status    string    `json:"status"` // pending / awaiting-otp / success / saved-partial / failed / timeout
	Message   string    `json:"message"`
	AuthFile  string    `json:"auth_file,omitempty"`
	UserID    string    `json:"user_id,omitempty"`

	// OTP：小米「新设备保护」二次验证上下文（notificationUrl 流程）。
	// 只暴露展示字段（方法/掩码）；context/cookie jar/验证码不进任何输出。
	OTP *otpState `json:"otp,omitempty"`

	// cred 在登录完成后缓存最终凭证，供 auth.login.poll RPC 组装 AuthData
	//（宿主收到后自己落盘）。不导出、不进任何 JSON 输出。
	cred *mimoCred
	// payload 是 AuthData 的幂等缓存（auth.login.poll 反复调用返回同一份）。
	payload map[string]any
}

// otpState：小米身份验证（新设备保护）流程上下文。
// 流程依据开源实现（MiService miaccount.py，MiIO 抓包）：
// authStart 建会话 → identity/list 查验证方式 → verifyPhone 触发 →
// sendPhoneTicket 发短信 → 用户收码 → verifyPhone 提交 ticket →
// GET location（set cookie）→ 重跑 serviceLogin 直接返回完整凭证。
type otpState struct {
	Flag   int       `json:"flag,omitempty"`   // 4=手机 8=邮箱
	Method string    `json:"method,omitempty"` // Phone / Email
	Notify string    `json:"notify,omitempty"` // 掩码（138****1234），登录页展示用
	SentAt time.Time `json:"sent_at,omitempty"`

	Context  string         `json:"-"` // notificationUrl 的 context
	SID      string         `json:"-"` // 验证流程使用的 sid
	DeviceID string         `json:"-"` // 随机 deviceId（小米风控的设备标识）
	Jar      http.CookieJar `json:"-"` // 贯穿验证会话的 cookie jar（内存）
}

var qrStore = struct {
	sync.Mutex
	m map[string]*qrSession
}{m: map[string]*qrSession{}}

func newSessionID() string {
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

func purgeQRExpiredLocked() {
	for id, s := range qrStore.m {
		if time.Since(s.CreatedAt) > qrSessionTTL+time.Duration(s.Timeout+60)*time.Second {
			delete(qrStore.m, id)
		}
	}
}

func getQRSession(id string) *qrSession {
	qrStore.Lock()
	defer qrStore.Unlock()
	purgeQRExpiredLocked()
	s := qrStore.m[id]
	if s == nil || time.Since(s.CreatedAt) > qrSessionTTL+time.Duration(s.Timeout+60)*time.Second {
		return nil
	}
	return s
}

func setQRStatus(s *qrSession, status, msg string) {
	qrStore.Lock()
	defer qrStore.Unlock()
	s.Status = status
	s.Message = msg
}

func loginPagePath(sessionID string) string {
	return "/v0/resource/plugins/mimo/login?session=" + sessionID
}

// stripXSSI 去掉小米响应的 XSSI 前缀（&&&START&&&）。
// serviceLogin(_json=true) 与 longPolling/loginUrl 的响应都可能带这个前缀，
// 不剥掉 json.Unmarshal 会直接报 "invalid character '&' ..."。
func stripXSSI(body []byte) []byte {
	s := strings.TrimLeft(string(body), " \t\r\n")
	s = strings.TrimPrefix(s, "&&&START&&&")
	return []byte(strings.TrimLeft(s, " \t\r\n"))
}

// sidCallback 取某 sid 在小米 passport 注册的回调地址（不可自定义，只能读）。
func sidCallback(sid string) string {
	base := sidCallbackBase
	if base == "" {
		base = qrLoginProvider
	}
	u := fmt.Sprintf("%s/pass/serviceLogin?_locale=zh_CN&_snsNone=true&sid=%s&_json=true",
		base, url.QueryEscape(sid))
	body, err := ssoGet(u, "")
	if err == nil {
		var parsed struct {
			Callback string `json:"callback"`
		}
		if json.Unmarshal(stripXSSI(body), &parsed) == nil && strings.TrimSpace(parsed.Callback) != "" {
			return strings.TrimSpace(parsed.Callback)
		}
	}
	if sid == defaultSID {
		return sidCallbackFallback
	}
	return ""
}

/* --------------------------------------------- QR 会话创建（REST 与 RPC 共用） */

// createQRSession 向小米申请一个扫码登录会话。失败返回 error（调用方决定信封形态）。
func createQRSession(sid string) (*qrSession, error) {
	cb := sidCallback(sid)
	if cb == "" {
		return nil, fmt.Errorf("拿不到 sid=%s 的注册回调地址，无法创建扫码会话（可改用方案一：npm run cpa-auth）", sid)
	}
	qs := "?sid=" + sid + "&_json=true"
	u := fmt.Sprintf("%s/longPolling/loginUrl?sid=%s&callback=%s&qs=%s&_qrsize=480",
		qrLoginProvider, url.QueryEscape(sid), url.QueryEscape(cb), url.QueryEscape(qs))

	body, err := ssoGet(u, "") // UA MiClaw/1.0，匿名，无凭证
	if err != nil {
		return nil, fmt.Errorf("创建扫码会话失败: %w", err)
	}
	var parsed struct {
		Code     *int   `json:"code"`
		Desc     string `json:"desc"`
		Result   string `json:"result"`
		LoginURL string `json:"loginUrl"`
		QR       string `json:"qr"`
		LP       string `json:"lp"`
		Timeout  int    `json:"timeout"`
		QRTips   string `json:"qrTips"`
	}
	if err := json.Unmarshal(stripXSSI(body), &parsed); err != nil || parsed.Code == nil || *parsed.Code != 0 || parsed.LP == "" {
		desc := parsed.Desc
		if err != nil {
			desc = "响应无法解析: " + err.Error()
		}
		return nil, fmt.Errorf("小米拒绝创建扫码会话 code=%v %s", parsed.Code, desc)
	}

	timeout := parsed.Timeout
	if timeout <= 0 {
		timeout = 300
	}
	s := &qrSession{
		ID: newSessionID(), SID: sid, Mode: "qr",
		QR: parsed.QR, LoginURL: parsed.LoginURL, LP: parsed.LP, QRTips: parsed.QRTips,
		Timeout: timeout, CreatedAt: time.Now(), Status: "pending",
		Message: "等待扫码（二维码 " + strconv.Itoa(timeout/60) + " 分钟内有效）；也可用账号密码登录",
	}
	qrStore.Lock()
	purgeQRExpiredLocked()
	qrStore.m[s.ID] = s
	qrStore.Unlock()

	go pollLoginSession(s)
	return s, nil
}

// newBareSession 建一个纯密码登录会话（不打小米 QR 接口）。
func newBareSession(sid string) *qrSession {
	s := &qrSession{
		ID: newSessionID(), SID: sid, Mode: "password",
		CreatedAt: time.Now(), Status: "pending",
		Message: "等待账号密码登录",
	}
	qrStore.Lock()
	purgeQRExpiredLocked()
	qrStore.m[s.ID] = s
	qrStore.Unlock()
	return s
}

/* --------------------------------- login.start REST（curl / 运维自动化入口） */

// handleLoginStart —— POST /v0/management/plugins/mimo/login/start
// 创建登录会话并返回一次性登录页地址（扫码 + 账号密码双通道）。
func handleLoginStart(req []byte) []byte {
	var in struct {
		SID string `json:"sid"`
	}
	_ = json.Unmarshal(req, &in)
	sid := strings.TrimSpace(in.SID)
	if sid == "" {
		sid = config().SID
	}

	s, err := createQRSession(sid)
	if err != nil {
		return errResult("upstream_error", err.Error(), 502)
	}

	qrStore.Lock()
	qr, tips, timeout := s.QR, s.QRTips, s.Timeout
	qrStore.Unlock()

	// 注意：qr 是凭证等价物（扫码即绑定），只在本响应里给一次，绝不进日志/状态页。
	return okResult(map[string]any{
		"session":    s.ID,
		"login_page": loginPagePath(s.ID),
		"qr":         qr,
		"qr_tips":    tips,
		"timeout":    timeout,
		"hint":       "浏览器打开 login_page：可用小米手机扫码确认，或在页面内用账号密码登录；凭证自动写入 auth 目录。",
	})
}

/* --------------------------------------------------- auth.login.start / poll RPC */

// handleAuthLoginStartRPC —— 面板 #/oauth「SSO 登录」的第一步。
// 宿主契约（sdk/pluginapi.AuthLoginStartResponse）：
// {Provider, URL, State, ExpiresAt, Metadata}。State 必须过 ValidateOAuthState
// （[A-Za-z0-9._-]），否则宿主回 502 invalid oauth state —— 这正是旧实现「不能用」的原因。
func handleAuthLoginStartRPC(req []byte) []byte {
	var in struct {
		Provider string         `json:"Provider"`
		BaseURL  string         `json:"BaseURL"`
		Metadata map[string]any `json:"Metadata"`
	}
	_ = json.Unmarshal(req, &in)

	sid := config().SID
	// 面板可以通过 query 参数覆盖 sid（GET /v0/management/mimo-auth-url?sid=xxx → metadata）
	if v, ok := in.Metadata["sid"].(string); ok && strings.TrimSpace(v) != "" {
		sid = strings.TrimSpace(v)
	}

	s, err := createQRSession(sid)
	if err != nil {
		return errResult("upstream_error", "发起登录失败: "+err.Error(), 502)
	}

	return okResult(map[string]any{
		"Provider":  providerKey,
		"URL":       loginPagePath(s.ID), // 相对路径：面板与资源页同源，远程部署不跑偏
		"State":     s.ID,
		"ExpiresAt": s.CreatedAt.Add(time.Duration(s.Timeout) * time.Second).UTC().Format(time.RFC3339),
		"Metadata": map[string]any{
			"session":  s.ID,
			"modes":    "qr,password",
			"page":     loginPagePath(s.ID),
			"password": "/v0/resource/plugins/mimo/login（POST JSON {session,user,password}）",
		},
	})
}

// handleAuthLoginPollRPC —— 面板轮询 get-auth-status 时宿主调用。
// success 时返回的 Auth/Auths 由宿主 savePluginLoginRecords 落盘（含 Metadata，
// usage_snapshot 随之进 auth 文件 JSON）。
func handleAuthLoginPollRPC(req []byte) []byte {
	var in struct {
		Provider string         `json:"Provider"`
		State    string         `json:"State"`
		Metadata map[string]any `json:"Metadata"`
	}
	_ = json.Unmarshal(req, &in)

	s := getQRSession(strings.TrimSpace(in.State))
	if s == nil {
		return okResult(map[string]any{
			"Status":  "error",
			"Message": "登录会话不存在或已过期，请重新发起登录",
		})
	}

	qrStore.Lock()
	status, msg, cred := s.Status, s.Message, s.cred
	qrStore.Unlock()

	switch status {
	case "pending":
		return okResult(map[string]any{"Status": "pending", "Message": msg})
	case "failed", "timeout":
		return okResult(map[string]any{"Status": "error", "Message": msg})
	case "success", "saved-partial":
		if cred == nil {
			return okResult(map[string]any{"Status": "error", "Message": "登录内部状态异常：凭证缺失，请重新发起"})
		}
		ad := authPayloadForSession(s, *cred)
		if ad == nil {
			return okResult(map[string]any{"Status": "error", "Message": "凭证序列化失败，请重新发起登录"})
		}
		return okResult(map[string]any{
			"Status":  "success",
			"Message": msg,
			"Auth":    ad,
			"Auths":   []map[string]any{ad},
		})
	}
	return okResult(map[string]any{"Status": "pending", "Message": msg})
}

// authPayloadForSession 构造宿主要的 AuthData（幂等缓存）。
// 复用 authData()：字段与 auth.parse 完全一致（宿主同一解码路径），Metadata 里
// 自动携带 usage_snapshot（若有），宿主落盘后 auth-files 详情 INFO 可见。
func authPayloadForSession(s *qrSession, c mimoCred) map[string]any {
	qrStore.Lock()
	cached := s.payload
	qrStore.Unlock()
	if cached != nil {
		return cached
	}

	authID := deriveAuthID(c)
	next := time.Now().Add(refreshAfter())
	ad := authData(c, authFileName(c.UserID), authID, next)

	qrStore.Lock()
	s.payload = ad
	qrStore.Unlock()
	return ad
}

/* ---------------------------------------------------- 通道 2：账号密码登录 */

// passwordError 携带小米返回的业务语义，调用方据此出信封。
type passwordError struct {
	Code          int
	Desc          string
	Notification  string // 风控：需要短信/已登录设备确认
	CaptchaURL    string // 风控：需要图形验证码
	SecondValidat bool
}

func (e *passwordError) Error() string {
	switch {
	case e.Notification != "" || e.SecondValidat:
		// 实测：密码正确时小米返回 code=0 + securityStatus=16 + notificationUrl
		// （新设备保护），没有 location 就拿不到 passToken。把验证链接原样
		// 交给用户：浏览器打开 → 完成短信/邮箱验证 → 重新提交即可登录成功。
		head := "密码验证未通过"
		if e.Code == 0 {
			head = "密码正确，但小米要求新设备身份验证（登录保护）"
		}
		msg := head + "。请先打开下面的链接完成验证，然后重新提交本页登录"
		if e.Code != 0 {
			msg += fmt.Sprintf("（code=%d %s）", e.Code, e.Desc)
		}
		if e.Notification != "" {
			msg += "：\n" + e.Notification
		}
		msg += "\n若验证后仍失败，可改用扫码登录，或在 Win 本机 npm run cpa-auth 导出后上传。"
		return msg
	case e.CaptchaURL != "":
		return fmt.Sprintf("小米要求图形验证码，插件无法代答。请打开 %s 完成验证后重试，或改用扫码登录（code=%d %s）", e.CaptchaURL, e.Code, e.Desc)
	default:
		if e.Desc == "" {
			e.Desc = "登录验证失败"
		}
		return fmt.Sprintf("小米拒绝账号密码登录：code=%d %s（密码错误、账号异常或风控；也可改用扫码登录）", e.Code, e.Desc)
	}
}

func md5Upper(s string) string {
	sum := md5.Sum([]byte(s))
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

/* ------------------------------------------------- 小米新设备验证（OTP） */

// otpUA：identity 验证接口用浏览器式 UA（开源实现验证过该流程）。
const otpUA = "Mozilla/5.0 (Linux; Android 13) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Mobile Safari/537.36 MiClaw/1.0"

// otpRequiredError：serviceLoginAuth2 返回 notificationUrl，需要用户验证码。
type otpRequiredError struct{ OTP *otpState }

func (e *otpRequiredError) Error() string {
	return "小米要求新设备身份验证（OTP）"
}

func otpClient(otp *otpState) *http.Client {
	return &http.Client{Timeout: authLoginTimeout, Jar: otp.Jar}
}

func otpBaseCookies(otp *otpState) []*http.Cookie {
	return []*http.Cookie{
		{Name: "deviceId", Value: otp.DeviceID},
		{Name: "sdkVersion", Value: "3.9"},
	}
}

func otpDo(otp *otpState, method, urlStr string, body io.Reader) ([]byte, error) {
	req, err := http.NewRequest(method, urlStr, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", otpUA)
	req.Header.Set("Accept", "application/json, text/html")
	// 小米 identity 接口的来源防护：前端 XMLHttpRequest 一律带这个头，
	// sendEmailTicket 缺它恒 66108（2026-09-23 抓包实锤，见 otp-sendcode-recon.mjs）。
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for _, ck := range otpBaseCookies(otp) {
		req.AddCookie(ck)
	}
	res, err := otpClient(otp).Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, maxAuthFile))
	return raw, nil
}

// otpParseJSON：剥 XSSI 前缀后解析到 map（identity/pass 接口通用）。
func otpParseJSON(raw []byte) map[string]any {
	m := map[string]any{}
	_ = json.Unmarshal(stripXSSI(raw), &m)
	return m
}

func otpCode(m map[string]any) int {
	switch v := m["code"].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return -1
}

func otpDesc(m map[string]any) string {
	for _, k := range []string{"description", "desc", "message", "tips"} {
		if s, _ := m[k].(string); s != "" {
			return s
		}
	}
	return ""
}

// startOTPVerification：auth2 返回 notificationUrl 后自动跑发码流程。
// 成功返回 otpState（等待用户输入验证码）；任何一步失败如实报错。
func startOTPVerification(ntf, sid string) (*otpState, error) {
	ntf = strings.TrimSpace(ntf)
	if !strings.HasPrefix(ntf, "http") {
		ntf = qrLoginProvider + ntf
	}
	u, err := url.Parse(ntf)
	if err != nil {
		return nil, fmt.Errorf("验证地址无效: %w", err)
	}
	ctxParam := u.Query().Get("context")
	sidParam := u.Query().Get("sid")
	if sidParam == "" {
		sidParam = sid
	}
	deviceID := strings.ToUpper(newSessionID())
	jar, _ := cookiejar.New(nil)
	otp := &otpState{Context: ctxParam, SID: sidParam, DeviceID: deviceID, Jar: jar}

	// Step 1：GET authStart 建立验证会话（cookie 进 jar）
	if _, err := otpDo(otp, http.MethodGet, ntf, nil); err != nil {
		return nil, fmt.Errorf("打开小米验证会话失败: %w", err)
	}

	// Step 2：identity/list 查可用验证方式（flag 4=手机 8=邮箱），响应含掩码
	listURL := fmt.Sprintf("%s/identity/list?sid=%s&supportedMask=0&_locale=zh_CN&context=%s",
		qrLoginProvider, url.QueryEscape(sidParam), url.QueryEscape(ctxParam))
	rawList, err := otpDo(otp, http.MethodGet, listURL, nil)
	if err != nil {
		return nil, fmt.Errorf("查询小米验证方式失败: %w", err)
	}
	list := otpParseJSON(rawList)
	if data, _ := list["data"].(map[string]any); data != nil && list["flag"] == nil {
		// 有的环境把字段包在 data 里
		for k, v := range data {
			if _, ok := list[k]; !ok {
				list[k] = v
			}
		}
	}
	flag := 4
	switch v := list["flag"].(type) {
	case float64:
		if int(v) != 0 {
			flag = int(v)
		}
	case int:
		if v != 0 {
			flag = v
		}
	}
	method := "Phone"
	if flag == 8 {
		method = "Email"
	} else {
		flag = 4
	}
	otp.Flag, otp.Method = flag, method
	otp.Notify = ""
	for _, k := range []string{"notify", "notifyPhone", "notifyEmail", "maskedPhone", "maskedEmail", "phone", "email", "tips", "description", "desc"} {
		if s, _ := list[k].(string); s != "" {
			otp.Notify = s
			break
		}
	}

	// Step 3：verifyPhone/verifyEmail 触发验证（_json=true）
	trigURL := fmt.Sprintf("%s/identity/auth/verify%s?_flag=%d&_json=true", qrLoginProvider, method, flag)
	rawTrig, err := otpDo(otp, http.MethodGet, trigURL, nil)
	if err != nil {
		return nil, fmt.Errorf("触发小米验证码失败: %w", err)
	}
	if tm := otpParseJSON(rawTrig); otpCode(tm) != -1 && otpCode(tm) != 0 {
		return nil, fmt.Errorf("触发小米验证码失败: code=%d %s", otpCode(tm), otpDesc(tm))
	}

	// Step 4：真正下发验证码。verifyEmail/verifyPhone GET 只是初始化验证会话
	// （返回 maskedEmail/contentType），真正发码是 POST send{Email,Phone}Ticket，
	// body 与短信完全一致（retry=0&icode=&_json=true）。
	// 2026-09-23 抓包实锤（test/login/otp-sendcode-recon.mjs）：缺
	// X-Requested-With 时 sendEmailTicket 恒 66108 —— 历史上"邮箱发码 API 逆向
	// 失败"的真正原因。此前本函数只给 Phone 发码，Email 账号永远收不到邮件。
	sendURL := fmt.Sprintf("%s/identity/auth/send%sTicket?_dc=%d", qrLoginProvider, method, time.Now().UnixMilli())
	form := url.Values{"retry": {"0"}, "icode": {""}, "_json": {"true"}}
	rawSend, err := otpDo(otp, http.MethodPost, sendURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("发送小米%s验证码失败: %w", methodCN(method), err)
	}
	if sm := otpParseJSON(rawSend); otpCode(sm) != -1 && otpCode(sm) != 0 {
		return nil, fmt.Errorf("发送小米%s验证码失败: code=%d %s", methodCN(method), otpCode(sm), otpDesc(sm))
	}

	otp.SentAt = time.Now()
	return otp, nil
}

// methodCN 把 Phone/Email 映射成中文，用于错误文案。
func methodCN(method string) string {
	if method == "Email" {
		return "邮箱"
	}
	return "短信"
}

// otpSubmit：用户提交验证码 → 完成身份验证 → 重跑 serviceLogin 拿完整凭证。
// 验证码只在本函数内存中流转。
func otpSubmit(otp *otpState, code string) (*qrTokens, error) {
	code = strings.TrimSpace(code)
	if otp == nil {
		return nil, fmt.Errorf("当前会话没有待验证的登录")
	}
	if code == "" {
		return nil, fmt.Errorf("验证码不能为空")
	}

	// Step 6：POST verify{Phone|Email} 提交验证码
	// trust=true：向小米声明「信任此设备」，降低后续账密登录再触发
	// 「新设备保护」邮箱验证的频率（实测每次 API 登录都触发 securityStatus=16）。
	vURL := fmt.Sprintf("%s/identity/auth/verify%s?_dc=%d", qrLoginProvider, otp.Method, time.Now().UnixMilli())
	form := url.Values{"_flag": {strconv.Itoa(otp.Flag)}, "ticket": {code}, "trust": {"true"}, "_json": {"true"}}
	rawV, err := otpDo(otp, http.MethodPost, vURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("提交验证码失败: %w", err)
	}
	vm := otpParseJSON(rawV)
	if loc, _ := vm["location"].(string); strings.TrimSpace(loc) != "" {
		// Step 7：GET location（jar 收下认证 cookie）
		if !strings.HasPrefix(loc, "http") {
			loc = qrLoginProvider + loc
		}
		if _, err := otpDo(otp, http.MethodGet, loc, nil); err != nil {
			return nil, fmt.Errorf("验证通过但认证跳转失败: %w", err)
		}
	} else if c := otpCode(vm); c != 0 {
		return nil, fmt.Errorf("验证码不正确或已过期: code=%d %s", c, otpDesc(vm))
	}

	// Step 8：重跑 serviceLogin —— jar 已认证，响应直接含完整凭证字段
	p0url := fmt.Sprintf("%s/pass/serviceLogin?sid=%s&_json=true&_locale=zh_CN",
		qrLoginProvider, url.QueryEscape(otp.SID))
	rawP, err := otpDo(otp, http.MethodGet, p0url, nil)
	if err != nil {
		return nil, fmt.Errorf("验证完成但恢复登录失败: %w", err)
	}
	pm := otpParseJSON(rawP)
	if c := otpCode(pm); c != 0 {
		return nil, fmt.Errorf("验证完成但恢复登录失败: code=%d %s", c, otpDesc(pm))
	}
	// 全树扫描取凭证（findQRTokens 对 userId 兼容 string/number —— 真实响应里
	// userId 是 JSON 数字，直接 .(string) 断言会静默取空，实测踩过）。
	tok := findQRTokens(rawP)
	if tok == nil {
		tok = &qrTokens{}
	}
	// 兜底：location（Set-Cookie/query 里收 passToken）
	if tok.PassToken == "" {
		if loc, _ := pm["location"].(string); strings.TrimSpace(loc) != "" {
			noRedirect := &http.Client{
				Timeout: authLoginTimeout,
				CheckRedirect: func(*http.Request, []*http.Request) error {
					return http.ErrUseLastResponse
				},
			}
			if bt, err2 := collectTokensFromLocation(noRedirect, loc, otp.SID); err2 == nil {
				tok = bt
			}
		}
	}
	if tok.PassToken == "" {
		return nil, fmt.Errorf("验证通过但小米未返回 passToken，请改用扫码登录")
	}
	return tok, nil
}

// passwordAuthenticate 走小米 web 同款密码登录流程换 passToken。
// user/password 只在本函数内存中流转：不打日志、不进错误消息、不落盘。
func passwordAuthenticate(sid, user, password string) (*qrTokens, error) {
	user = strings.TrimSpace(user)
	if user == "" || password == "" {
		return nil, fmt.Errorf("账号和密码都不能为空")
	}
	if sid == "" {
		sid = defaultSID
	}

	// ---- 阶段 0：serviceLogin 取 qs / _sign / callback ----
	phase0 := fmt.Sprintf("%s/pass/serviceLogin?sid=%s&_json=true&_locale=zh_CN",
		qrLoginProvider, url.QueryEscape(sid))
	body0, err := ssoGet(phase0, "")
	if err != nil {
		return nil, fmt.Errorf("连接小米 passport 失败: %w", err)
	}
	var p0 struct {
		Code     *int   `json:"code"`
		Qs       string `json:"qs"`
		Sign     string `json:"_sign"`
		Callback string `json:"callback"`
	}
	_ = json.Unmarshal(stripXSSI(body0), &p0)
	callback := strings.TrimSpace(p0.Callback)
	if callback == "" {
		callback = sidCallbackFallback
	}

	// ---- 阶段 1：serviceLoginAuth2 ----
	form := url.Values{}
	form.Set("sid", sid)
	form.Set("callback", callback)
	if strings.TrimSpace(p0.Qs) != "" {
		form.Set("qs", p0.Qs)
	} else {
		form.Set("qs", "?sid="+sid+"&_json=true")
	}
	form.Set("user", user)
	form.Set("hash", md5Upper(password))
	form.Set("_json", "true")
	form.Set("_locale", "zh_CN")
	if strings.TrimSpace(p0.Sign) != "" {
		form.Set("_sign", p0.Sign)
	}

	req, err := http.NewRequest(http.MethodPost, passAuth2URL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", ssoUA)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", qrLoginProvider+"/fe/service/login?sid="+url.QueryEscape(sid))
	req.Header.Set("Origin", qrLoginProvider)

	client := &http.Client{Timeout: authLoginTimeout}
	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("小米 passport 请求失败: %w", err)
	}
	body, _ := io.ReadAll(io.LimitReader(res.Body, maxAuthFile))
	res.Body.Close()

	var p1 struct {
		Code             *int   `json:"code"`
		Desc             string `json:"desc"`
		Description      string `json:"description"`
		Location         string `json:"location"`
		NotificationURL  string `json:"notificationUrl"`
		CaptchaURL       string `json:"captchaUrl"`
		SecondValidation bool   `json:"secondValidation"`
		Result           string `json:"result"`
	}
	if err := json.Unmarshal(stripXSSI(body), &p1); err != nil {
		return nil, fmt.Errorf("serviceLoginAuth2 响应无法解析（%.120s）", stripXSSI(body))
	}
	code := -1
	if p1.Code != nil {
		code = *p1.Code
	}
	desc := p1.Desc
	if desc == "" {
		desc = p1.Description
	}
	if code != 0 || strings.TrimSpace(p1.Location) == "" {
		// notificationUrl：小米「新设备保护」→ 自动跑发码流程，等用户输入验证码
		if strings.TrimSpace(p1.NotificationURL) != "" {
			otp, oerr := startOTPVerification(p1.NotificationURL, sid)
			if oerr != nil {
				return nil, fmt.Errorf("小米要求新设备验证，但自动发起验证码失败: %w", oerr)
			}
			return nil, &otpRequiredError{OTP: otp}
		}
		return nil, &passwordError{
			Code: code, Desc: desc,
			Notification: p1.NotificationURL, CaptchaURL: p1.CaptchaURL,
			SecondValidat: p1.SecondValidation,
		}
	}

	// ---- 阶段 2：GET location 收 passToken ----
	// 不跟随重定向：passToken 由第一跳（account.xiaomi.com 域）Set-Cookie 下发，
	// 跟到 mimo-server 的 callback 反而什么也拿不到。
	noRedirect := &http.Client{
		Timeout: authLoginTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	tok, err2 := collectTokensFromLocation(noRedirect, p1.Location, sid)
	if err2 != nil {
		return nil, err2
	}
	return tok, nil
}

// collectTokensFromLocation GET location（最多跟 2 跳，仅限 account.xiaomi.com 域），
// 从响应 Set-Cookie 与响应体 JSON 里收集 passToken/userId/cUserId。
func collectTokensFromLocation(client *http.Client, location, sid string) (*qrTokens, error) {
	next := strings.TrimSpace(location)
	for hop := 0; hop < 2 && next != ""; hop++ {
		req, err := http.NewRequest(http.MethodGet, next, nil)
		if err != nil {
			return nil, fmt.Errorf("登录跳转地址无效: %w", err)
		}
		req.Header.Set("User-Agent", ssoUA)
		res, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("登录跳转请求失败: %w", err)
		}
		body, _ := io.ReadAll(io.LimitReader(res.Body, maxAuthFile))
		res.Body.Close()

		out := &qrTokens{}
		// 1) Set-Cookie（passToken 通常在这里）
		for _, ck := range res.Cookies() {
			switch ck.Name {
			case "passToken":
				if out.PassToken == "" {
					out.PassToken = ck.Value
				}
			case "userId":
				if out.UserID == "" {
					out.UserID = ck.Value
				}
			case "cUserId":
				if out.CUserID == "" {
					out.CUserID = ck.Value
				}
			}
		}
		// 2) 响应体 JSON 全树扫描（location query/JSON 里也可能带）
		if bt := findQRTokens(body); bt != nil {
			if out.PassToken == "" {
				out.PassToken = bt.PassToken
			}
			if out.UserID == "" {
				out.UserID = bt.UserID
			}
			if out.CUserID == "" {
				out.CUserID = bt.CUserID
			}
		}
		// 3) location URL 自身的 query
		if u, uerr := url.Parse(next); uerr == nil {
			q := u.Query()
			if out.PassToken == "" {
				out.PassToken = q.Get("passToken")
			}
			if out.UserID == "" {
				out.UserID = q.Get("userId")
			}
			if out.CUserID == "" {
				out.CUserID = q.Get("cUserId")
			}
		}

		if out.PassToken != "" && out.UserID != "" {
			return out, nil
		}

		// 没收齐：看有没有下一跳（仅限小米 passport 域，防开放重定向）
		loc := strings.TrimSpace(res.Header.Get("Location"))
		if loc == "" {
			var p struct {
				Location string `json:"location"`
			}
			if json.Unmarshal(stripXSSI(body), &p) == nil {
				loc = strings.TrimSpace(p.Location)
			}
		}
		if loc == "" {
			break
		}
		abs, aerr := url.Parse(loc)
		if aerr != nil || !strings.HasSuffix(abs.Host, ".xiaomi.com") {
			break
		}
		if !abs.IsAbs() {
			base, _ := url.Parse(next)
			abs = base.ResolveReference(abs)
		}
		next = abs.String()
	}
	return nil, fmt.Errorf("登录跳转完成后仍未拿到 passToken（风控或页面流程变更），请改用扫码登录或方案一导出")
}

/* ---------------------------------------------------- 管理 REST：login/password */

// handleLoginPassword —— POST /v0/management/plugins/mimo/login/password
// body: {"session"?: "...", "user": "...", "password": "..."}
// session 可省略：省略时插件建一个内部会话直接执行；带 session（登录页表单 /
// 面板 OAuth 会话）时结果灌进该会话，面板 poll 即可取走。
func handleLoginPassword(req []byte) []byte {
	var in struct {
		Session  string `json:"session"`
		User     string `json:"user"`
		Password string `json:"password"`
	}
	_ = json.Unmarshal(req, &in)
	user := strings.TrimSpace(in.User)
	if user == "" || in.Password == "" {
		return errResult("invalid_request", "user 与 password 都是必填", 400)
	}

	s := getQRSession(strings.TrimSpace(in.Session))
	if s == nil {
		s = newBareSession(config().SID)
	}

	// 自动收码准备：配置了 otp_auto_mail 且账号是邮箱时，先取收件箱基线
	// （发码前的时间截面），OTP 触发后由 autoOTPWorker 无人值守完成收码+提交。
	var mailBox *otpMailBox
	var mailBaseline map[string]bool
	if mc, addr := autoMailFor(user); mc != nil {
		if box, base, err := connectMailbox(*mc, addr); err == nil {
			mailBox, mailBaseline = box, base
		} else {
			dbg("auto-otp: 收件箱连接失败（降级人工输入）: %v", err)
		}
	}

	tok, err := passwordAuthenticate(s.SID, user, in.Password)
	if err != nil {
		// 小米「新设备保护」：验证码已自动发出，进入等待输入状态（非错误）
		var oe *otpRequiredError
		if errors.As(err, &oe) {
			methodCN := "手机"
			notify := ""
			if oe.OTP != nil {
				if oe.OTP.Method == "Email" {
					methodCN = "邮箱"
				}
				notify = oe.OTP.Notify
			}
			target := notify
			if target == "" {
				target = "绑定的" + methodCN
			}
			msg := fmt.Sprintf("小米要求新设备验证：验证码已发送到 %s，请在下方输入验证码完成登录", target)
			if mailBox != nil {
				msg += "（已开启自动收码，通常无需手动操作）"
			}
			qrStore.Lock()
			s.Mode = "password"
			s.OTP = oe.OTP
			s.Status = "awaiting-otp"
			s.Message = msg
			qrStore.Unlock()
			if mailBox != nil && oe.OTP != nil {
				go autoOTPWorker(s, oe.OTP, mailBox, mailBaseline)
			}
			otpInfo := map[string]any{}
			if oe.OTP != nil {
				otpInfo = map[string]any{"method": oe.OTP.Method, "notify": oe.OTP.Notify}
			}
			return okResult(map[string]any{
				"session": s.ID,
				"status":  "awaiting-otp",
				"message": msg,
				"otp":     otpInfo,
			})
		}
		setQRStatus(s, "failed", err.Error())
		return errResult("auth_failed", err.Error(), http.StatusUnauthorized)
	}

	finishLogin(s, tok)
	qrStore.Lock()
	status, msg, uid, af := s.Status, s.Message, s.UserID, s.AuthFile
	qrStore.Unlock()
	return okResult(map[string]any{
		"session":   s.ID,
		"status":    status,
		"message":   msg,
		"user_id":   uid,
		"auth_file": af,
	})
}

// handleLoginVerify：POST /plugins/mimo/login/verify {session, code}
// 提交小米新设备验证的短信/邮箱验证码，完成账密登录的最后一跳。
// 验证码只在内存中流转：不落盘、不进日志、不回显。
func handleLoginVerify(req []byte) []byte {
	var in struct {
		Session string `json:"session"`
		Code    string `json:"code"`
	}
	_ = json.Unmarshal(req, &in)
	code := strings.TrimSpace(in.Code)
	if code == "" {
		return errResult("invalid_request", "code 必填", 400)
	}
	s := getQRSession(strings.TrimSpace(in.Session))
	if s == nil {
		return errResult("not_found", "登录会话不存在或已过期，请重新发起登录", 404)
	}
	qrStore.Lock()
	otp := s.OTP
	st := s.Status
	qrStore.Unlock()
	if otp == nil || st != "awaiting-otp" {
		return errResult("invalid_request", "当前会话没有待验证的登录，请先提交账号密码", 400)
	}
	tok, err := otpSubmit(otp, code)
	if err != nil {
		// 验证码输错可重试：保持 awaiting-otp 状态
		setQRStatus(s, "awaiting-otp", err.Error())
		return errResult("otp_failed", err.Error(), http.StatusUnauthorized)
	}
	finishLogin(s, tok)
	qrStore.Lock()
	status, msg, uid, af := s.Status, s.Message, s.UserID, s.AuthFile
	qrStore.Unlock()
	return okResult(map[string]any{
		"session":   s.ID,
		"status":    status,
		"message":   msg,
		"user_id":   uid,
		"auth_file": af,
	})
}

/* ---------------------------------------------------- 后台长轮询（扫码） */

func pollLoginSession(s *qrSession) {
	deadline := s.CreatedAt.Add(time.Duration(s.Timeout+30) * time.Second)
	client := &http.Client{Timeout: 330 * time.Second} // 长轮询：单次请求服务器会吊住
	for time.Now().Before(deadline) {
		qrStore.Lock()
		st := s.Status
		qrStore.Unlock()
		if st != "pending" { // 已被密码登录/取消等其它路径结束
			return
		}
		req, err := http.NewRequest(http.MethodGet, s.LP, nil)
		if err != nil {
			setQRStatus(s, "failed", "轮询请求构造失败: "+err.Error())
			return
		}
		req.Header.Set("User-Agent", ssoUA)
		res, err := client.Do(req)
		if err != nil {
			// 网络抖动给一次重试机会，不直接判死
			if time.Now().After(deadline.Add(-30 * time.Second)) {
				setQRStatus(s, "failed", "轮询失败: "+err.Error())
				return
			}
			time.Sleep(2 * time.Second)
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		res.Body.Close()

		tok := findQRTokens(body)
		if tok != nil && tok.PassToken != "" && tok.UserID != "" {
			finishLogin(s, tok)
			return
		}
		// 二次验证 / 风控：直接失败并提示人工路径（凭证值不进消息）
		if strings.Contains(string(body), "notificationUrl") {
			setQRStatus(s, "failed", "该账号需要二次验证/风控拦截，请改用账号密码登录、方案一（npm run cpa-auth）或在手机上完成验证后重试")
			return
		}
		// 其它情况视为「尚未扫码确认」，继续轮询
	}
	setQRStatus(s, "timeout", "二维码超时未确认，请重新发起登录")
}

type qrTokens struct {
	PassToken string
	UserID    string
	CUserID   string
}

// findQRTokens 递归遍历 JSON 找 passToken/userId/cUserId。
// 小米不同入口的响应包裹层级不一致（result/data/顶层都出现过），
// 与其赌某一种形状，不如全树扫描。
func findQRTokens(body []byte) *qrTokens {
	var v any
	if json.Unmarshal(stripXSSI(body), &v) != nil {
		return nil
	}
	out := &qrTokens{}
	walkForTokens(v, out)
	if out.PassToken == "" && out.UserID == "" {
		return nil
	}
	return out
}

func walkForTokens(v any, out *qrTokens) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			s, isStr := val.(string)
			switch strings.ToLower(k) {
			case "passtoken":
				if isStr && out.PassToken == "" && s != "" {
					out.PassToken = s
				}
			case "userid":
				if out.UserID != "" {
					break
				}
				if isStr {
					out.UserID = s
				} else if n, ok := val.(float64); ok {
					out.UserID = strconv.FormatInt(int64(n), 10)
				}
			case "cuserid":
				if out.CUserID != "" {
					break
				}
				if isStr {
					out.CUserID = s
				} else if n, ok := val.(float64); ok {
					out.CUserID = strconv.FormatInt(int64(n), 10)
				}
			}
			walkForTokens(val, out)
		}
	case []any:
		for _, e := range t {
			walkForTokens(e, out)
		}
	}
}

// hostAuthSave 把凭证 JSON 写进宿主 auth 目录（host.auth.save RPC）。
// 抽成变量便于单测替换（测试环境没有宿主 API）。
//
// ⚠️ json 字段必须传 json.RawMessage（序列化输出凭证 JSON 对象原文）。
// 传 []byte 会被 encoding/json 编码成 base64 字符串，宿主
// （pluginhost/auth_callbacks.go:277）按 map 解析时报
// "invalid auth json: cannot unmarshal string into Go value of type map[...]"。
var hostAuthSave = func(name string, raw []byte) error {
	_, err := hostCall(methodHostAuthSave, map[string]any{"name": name, "json": json.RawMessage(raw)})
	return err
}

// finishLogin 拿到 passToken 后：换 serviceToken（尽力）→ host.auth.save 落盘 →
// 更新插件内存状态。换取失败也落盘：pass_token 才是长期凭证，auth.parse 会重试。
// 扫码与账号密码两条通道在这里汇合；幂等 —— QR 轮询与密码登录并发结束时只处理一次。
func finishLogin(s *qrSession, tok *qrTokens) {
	qrStore.Lock()
	if s.Status == "success" || s.Status == "saved-partial" {
		qrStore.Unlock()
		return
	}
	qrStore.Unlock()

	c := mimoCred{
		Type:       providerKey,
		PassToken:  tok.PassToken,
		UserID:     tok.UserID,
		CUserID:    tok.CUserID,
		SID:        s.SID,
		ObtainedAt: time.Now().UTC().Format(time.RFC3339),
	}

	status, msg := "saved-partial", ""
	sso, err := exchangeServiceToken(s.SID, c)
	if err != nil {
		msg = "passToken 已拿到，但换取 serviceToken 失败: " + err.Error() +
			"（pass_token 已写入，宿主加载时会自动重试换取）"
	} else {
		c.ServiceToken = sso.ServiceToken
		status = "success"
		msg = "凭证已换取并写入"
	}

	raw, merr := json.Marshal(c)
	if merr != nil {
		setQRStatus(s, "failed", "凭证序列化失败: "+merr.Error())
		return
	}
	// 每账号一个 auth 文件（mimo-<userId>.json）：多账号并存互不顶替，
	// 同一账号重登/续期覆盖同名文件。
	fname := authFileName(c.UserID)
	if err := hostAuthSave(fname, raw); err != nil {
		setQRStatus(s, "failed", "凭证已获取但写入 auth 目录失败: "+err.Error())
		return
	}

	authID := deriveAuthID(c)
	noteParsed(authID, c.UserID, c.ServiceToken, true, "登录页登录")
	rememberCred(authID, c)
	invalidateModels()
	// 异步取用量快照：poll RPC 懒构建 AuthData 时带上 usage_snapshot，宿主落盘后
	// 面板 auth-files 详情 INFO 可见。失败不影响登录。usageGo 可被测试等待落定。
	usageGo(func() { tryAttachUsage(authID, c) })

	qrStore.Lock()
	s.Status = status
	s.UserID = c.UserID
	s.AuthFile = fname
	s.cred = &c
	s.Message = fmt.Sprintf("%s（userId %s → auth/%s，宿主热加载生效）", msg, c.UserID, fname)
	qrStore.Unlock()
	hostLog("info", fmt.Sprintf("MiMo 登录成功：userId=%s，凭证已写入 auth/%s", c.UserID, fname))
}

/* ------------------------------------------------- login.cancel / status */

func handleLoginCancel(req []byte) []byte {
	var in struct {
		Session string `json:"session"`
	}
	_ = json.Unmarshal(req, &in)
	s := getQRSession(strings.TrimSpace(in.Session))
	if s == nil {
		return errResult("not_found", "会话不存在或已过期", 404)
	}
	setQRStatus(s, "failed", "已取消")
	return okResult(map[string]any{"session": s.ID, "status": "failed"})
}

func handleLoginStatus(query map[string][]string) []byte {
	id := ""
	if v := query["session"]; len(v) > 0 {
		id = strings.TrimSpace(v[0])
	}
	s := getQRSession(id)
	if s == nil {
		return errResult("not_found", "会话不存在或已过期", 404)
	}
	// 只输出非敏感字段
	return okResult(map[string]any{
		"session":    s.ID,
		"status":     s.Status,
		"message":    s.Message,
		"created_at": s.CreatedAt.Format(time.RFC3339),
		"user_id":    s.UserID,
		"auth_file":  s.AuthFile,
	})
}

/* ------------------------------------------------------- 登录页 HTML */

// renderLoginPage —— 扫码 + 账号密码双通道登录页。
// 面板 #/oauth 的 SSO 登录按钮打开的就是这一页；curl login/start 后浏览器打开的
// 也是它。密码表单 POST 到同源资源路由（无需管理密钥，凭 session ID 访问）。
func renderLoginPage(session *qrSession, rawQR string) string {
	qr := ""
	tips := ""
	status := "not_found"
	msg := "会话不存在或已过期，请重新发起登录"
	if session != nil {
		qr = session.QR
		tips = session.QRTips
		status = session.Status
		msg = session.Message
	}
	if rawQR != "" {
		qr = rawQR
	}
	esc := htmlEscape
	var b strings.Builder
	b.WriteString(`<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">`)
	b.WriteString(`<meta name="viewport" content="width=device-width,initial-scale=1">`)
	b.WriteString(`<title>MiMo 登录</title><style>
:root{color-scheme:light dark}
body{font:14px/1.6 -apple-system,"Segoe UI","PingFang SC","Microsoft YaHei",sans-serif;
     margin:0;padding:24px;max-width:520px;background:#fafafa;color:#1a1a1a;text-align:center}
@media(prefers-color-scheme:dark){body{background:#16181d;color:#e6e6e6}.card{background:#1e2128;border-color:#2c313a}}
.card{background:#fff;border:1px solid #e4e4e7;border-radius:12px;padding:24px;margin-bottom:16px}
h1{font-size:18px;margin:0 0 8px}
.qr{max-width:280px;width:100%;background:#fff;padding:12px;border-radius:8px;border:1px solid #eee}
.tips{color:#888;font-size:13px;margin-top:10px}
#st{font-weight:600;margin-top:14px}
.ok{color:#0a7d38}.bad{color:#c02626}.warn{color:#b26a00}
code{background:rgba(128,128,128,.15);padding:1px 6px;border-radius:4px;font-size:12px}
.tabs{display:flex;gap:8px;justify-content:center;margin:0 0 16px}
.tab{padding:7px 18px;border:1px solid #d4d4d8;border-radius:999px;background:transparent;
     cursor:pointer;font-size:13px;color:inherit}
.tab.on{background:#1a1a1a;color:#fff;border-color:#1a1a1a}
@media(prefers-color-scheme:dark){.tab.on{background:#e6e6e6;color:#16181d;border-color:#e6e6e6}}
.pane{display:none}.pane.on{display:block}
input{width:100%;box-sizing:border-box;padding:9px 12px;margin:6px 0;border:1px solid #d4d4d8;
      border-radius:8px;font-size:14px;background:inherit;color:inherit}
button.go{width:100%;padding:10px;margin-top:8px;border:0;border-radius:8px;background:#1a1a1a;
      color:#fff;font-size:14px;cursor:pointer}
@media(prefers-color-scheme:dark){button.go{background:#e6e6e6;color:#16181d}}
</style></head><body>`)
	b.WriteString(`<div class="card"><h1>MiMo 登录</h1>`)
	b.WriteString(`<div class="tabs"><button class="tab on" id="tabQR" type="button">扫码登录</button>` +
		`<button class="tab" id="tabPW" type="button">账号密码登录</button></div>`)

	/* 扫码 pane */
	b.WriteString(`<div class="pane on" id="paneQR">`)
	if qr != "" {
		fmt.Fprintf(&b, `<img class="qr" src="%s" alt="登录二维码">`, esc(qr))
	} else {
		b.WriteString(`<p class="warn">本会话没有二维码（账号密码登录会话），可切换到「账号密码登录」。</p>`)
	}
	if tips != "" {
		fmt.Fprintf(&b, `<div class="tips">%s</div>`, esc(tips))
	} else {
		b.WriteString(`<div class="tips">用小米手机（系统设置 → 小米账号）扫码并确认登录</div>`)
	}
	b.WriteString(`</div>`)

	/* 账号密码 pane */
	b.WriteString(`<div class="pane" id="panePW">
<div class="tips" style="text-align:left">输入小米账号（手机号/邮箱）与密码，插件直接向小米
passport 换取 passToken。密码仅在内存中使用，不落盘、不写日志、不回显。<br>
<b>无需 CPA 管理密钥</b>：提交走本页一次性会话的资源路由（GET + 加密头），与扫码页同一暴露面。<br>
若小米要求新设备验证：已配置自动收码的邮箱会<b>自动</b>收到并提交验证码；否则在下方手动输入。</div>
<input id="pwUser" placeholder="小米账号（手机号 / 邮箱 / ID）" autocomplete="off">
<input id="pwPass" type="password" placeholder="密码" autocomplete="off">
<button class="go" id="pwGo" type="button">登录</button>
<div class="tips" id="pwMsg"></div>
<div id="pwOtpBox" style="display:none;margin-top:10px;border-top:1px dashed #d4d4d8;padding-top:8px">
<div class="tips" id="otpHint" style="text-align:left"></div>
<input id="pwOtpCode" placeholder="短信 / 邮箱验证码" autocomplete="off">
<button class="go" id="pwOtpGo" type="button">提交验证码</button>
</div>
</div>`)

	fmt.Fprintf(&b, `<div id="st" class="warn">%s</div><div class="tips" id="msg">%s</div>`,
		esc(status), esc(msg))
	b.WriteString(`</div><div class="tips">凭证落盘后 CPA 自动热加载；本页 5 分钟内有效，刷新请重新发起。<br>
本页不显示任何凭证；扫码结果由服务器侧长轮询接收。</div>`)
	sessionID := ""
	if session != nil {
		sessionID = session.ID
	}
	fmt.Fprintf(&b, `<script>
const sid=%q; const st=document.getElementById('st'); const ms=document.getElementById('msg');
const cls={pending:'warn',success:'ok','saved-partial':'warn',failed:'bad',timeout:'bad',not_found:'bad'};
function escHtml(s){ return String(s).replace(/[&<>"]/g,function(c){return {'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c];}); }
// 消息里的 http(s) 链接转成可点击（小米新设备验证链接 notificationUrl 就靠它直达）
function linkify(s){ return escHtml(s).replace(/(https?:\/\/[^\s<]+)/g,'<a href="$1" target="_blank" rel="noreferrer">$1</a>').replace(/\n/g,'<br>'); }
function apply(d){ if(!d)return; if(d.status){ st.textContent=d.status; st.className=cls[d.status]??'warn'; ms.innerHTML=linkify(d.message??''); } return d.status; }
async function tick(){
  if(!sid) return;
  try{
    const r=await fetch('?session='+sid+'&poll=1',{headers:{'accept':'application/json'}});
    const txt=await r.text(); let j=null; try{ j=JSON.parse(txt); }catch(e){}
    if(!j){ setTimeout(tick,4000); return; }
    const s=apply(j.result ?? j);
    if(s==='pending') setTimeout(tick,2000);
  }catch(e){ setTimeout(tick,4000); }
}
// tab 切换
const tQR=document.getElementById('tabQR'), tPW=document.getElementById('tabPW');
const pQR=document.getElementById('paneQR'), pPW=document.getElementById('panePW');
function tab(which){ const q=which==='qr';
  tQR.classList.toggle('on',q); tPW.classList.toggle('on',!q);
  pQR.classList.toggle('on',q); pPW.classList.toggle('on',!q); }
tQR.onclick=()=>tab('qr'); tPW.onclick=()=>tab('pw');
// 密码登录：宿主资源路由 ServeResourceHTTP 只转发 GET（POST 直接 404 空体，
// 实测 v7.3.9）。提交走资源路由 GET + x-mimo-req 头（base64 JSON）：
// 密码不进 URL（不进宿主访问日志）、不需要管理密钥、随本页一次性会话过期。
const pwMsg=document.getElementById('pwMsg');
function encReq(obj){
  const bytes=new TextEncoder().encode(JSON.stringify(obj));
  let bin=''; bytes.forEach(b=>{bin+=String.fromCharCode(b);});
  return btoa(bin);
}
async function reqOp(op,payload){
  const r=await fetch('?session='+encodeURIComponent(sid)+'&op='+op,{
    method:'GET',
    headers:{'accept':'application/json','x-mimo-req':encReq(payload)}});
  const txt=await r.text(); let j=null; try{ j=JSON.parse(txt); }catch(e){}
  return {r,txt,j};
}
document.getElementById('pwGo').onclick=async()=>{
  const u=document.getElementById('pwUser').value.trim();
  const p=document.getElementById('pwPass').value;
  if(!u||!p){ pwMsg.textContent='账号和密码都要填'; pwMsg.className='tips bad'; return; }
  pwMsg.textContent='正在向小米 passport 提交…'; pwMsg.className='tips warn';
  document.getElementById('pwPass').value='';   // 提交后立即清空密码输入框
  try{
    const {r,txt,j}=await reqOp('password',{session:sid,user:u,password:p});
    if(!j){
      pwMsg.textContent='提交失败：服务器返回 HTTP '+r.status+(txt?('（'+txt.slice(0,160)+'）'):'（空响应）');
      pwMsg.className='tips bad'; return;
    }
    const d=j.result ?? j;
    if(!r.ok || d.error){ pwMsg.innerHTML=linkify(typeof d.error==='string'?d.error:('HTTP '+r.status)); pwMsg.className='tips bad'; }
    else {
      pwMsg.innerHTML=linkify(d.message??'登录完成'); pwMsg.className='tips ok'; apply(d);
      if(d.status==='awaiting-otp'){ showOtp(d.message); }
    }
  }catch(e){ pwMsg.textContent='提交失败: '+e; pwMsg.className='tips bad'; }
};
// 小米新设备验证：显示验证码输入区
const otpBox=document.getElementById('pwOtpBox'), otpHint=document.getElementById('otpHint');
function showOtp(msg){ otpHint.textContent=(msg||'小米要求验证，请输入收到的验证码')+''; otpBox.style.display='block'; document.getElementById('pwOtpCode').focus(); }
function hideOtp(){ otpBox.style.display='none'; }
document.getElementById('pwOtpGo').onclick=async()=>{
  const code=document.getElementById('pwOtpCode').value.trim();
  if(!code){ pwMsg.textContent='请输入收到的验证码'; pwMsg.className='tips bad'; return; }
  pwMsg.textContent='正在提交验证码…'; pwMsg.className='tips warn';
  document.getElementById('pwOtpCode').value='';
  try{
    const {r,txt,j}=await reqOp('verify',{session:sid,code:code});
    if(!j){ pwMsg.textContent='提交失败：服务器返回 HTTP '+r.status+(txt?('（'+txt.slice(0,160)+'）'):'（空响应）'); pwMsg.className='tips bad'; return; }
    const d=j.result ?? j;
    if(!r.ok || d.error){ pwMsg.innerHTML=linkify(typeof d.error==='string'?d.error:('HTTP '+r.status)); pwMsg.className='tips bad'; }
    else { pwMsg.innerHTML=linkify(d.message??'登录完成'); pwMsg.className='tips ok'; apply(d); hideOtp(); }
  }catch(e){ pwMsg.textContent='提交失败: '+e; pwMsg.className='tips bad'; }
};
tick();
</script>`, sessionID)
	b.WriteString(`</body></html>`)
	return b.String()
}

// htmlEscape —— login.go 复用一个小实现避免依赖散落。
func htmlEscape(s string) string {
	repl := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;")
	return repl.Replace(s)
}
