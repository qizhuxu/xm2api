package main

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

/*
 * MiMo 凭证。
 *
 * 这里有一个关键的设计判断：**passToken 才是长期凭证，serviceToken 是短期的**。
 * xm2api 原本每次都要去读客户端 Chromium 的 Cookies 库拿 passToken；搬到插件里之后
 * 不再依赖本地 SQLite（也就免掉了 CGO sqlite 依赖和 cookie 库被独占锁住的问题），
 * 改成把 passToken 存进 auth 文件，由 auth.refresh 定时换 serviceToken ——
 * 语义上等价于 OAuth 的 refresh_token / access_token。
 *
 * 三种可用的凭证文件写法：
 *   {"type":"mimo","pass_token":"...","user_id":"...","c_user_id":"..."}   ← 推荐，可自动续期
 *   {"type":"mimo","service_token":"...","user_id":"..."}                  ← 能用，但不会自动续期
 *   {"type":"mimo","cookie":"serviceToken=...; userId=..."}                ← 从 xm2api 的
 *                                                                             sso-session.json 迁移
 */

const (
	ssoUA       = "MiClaw/1.0"
	ssoTimeout  = 20 * time.Second
	maxAuthFile = 1 << 20
)

// 小米 passport 端点。做成变量而不是常量，单测可以指向 httptest 替身。
var phase1URL = "https://account.xiaomi.com/pass/serviceLogin"

type mimoCred struct {
	Type         string `json:"type,omitempty"`
	ServiceToken string `json:"service_token,omitempty"`
	UserID       string `json:"user_id,omitempty"`
	PassToken    string `json:"pass_token,omitempty"`
	CUserID      string `json:"c_user_id,omitempty"`
	SID          string `json:"sid,omitempty"`
	ObtainedAt   string `json:"obtained_at,omitempty"`
	Cookie       string `json:"cookie,omitempty"`
}

// normalize 兼容 xm2api 的 sso-session.json：里面存的是整条 routeCookieHeader。
func (c *mimoCred) normalize() {
	if c.Cookie != "" {
		for _, part := range strings.Split(c.Cookie, ";") {
			kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
			if len(kv) != 2 {
				continue
			}
			switch strings.TrimSpace(kv[0]) {
			case "serviceToken":
				if c.ServiceToken == "" {
					c.ServiceToken = strings.TrimSpace(kv[1])
				}
			case "userId":
				if c.UserID == "" {
					c.UserID = strings.TrimSpace(kv[1])
				}
			case "cUserId":
				if c.CUserID == "" {
					c.CUserID = strings.TrimSpace(kv[1])
				}
			case "passToken":
				if c.PassToken == "" {
					c.PassToken = strings.TrimSpace(kv[1])
				}
			}
		}
		c.Cookie = ""
	}
	if c.Type == "" {
		c.Type = providerKey
	}
}

// usable 判断这是不是一条我们认得的 MiMo 凭证。
func (c mimoCred) usable() bool {
	return c.ServiceToken != "" || c.PassToken != ""
}

// routeCookie 是打上游时要带的 Cookie 头（与 xm2api 的 routeCookieHeader 同形）。
func (c mimoCred) routeCookie() string {
	if c.ServiceToken == "" || c.UserID == "" {
		return ""
	}
	return "serviceToken=" + c.ServiceToken + "; userId=" + c.UserID
}

/* --------------------------------------------------------- SSO 两阶段交换 */

var nonceRe = regexp.MustCompile(`"nonce"\s*:\s*(\d+)`)

type ssoResult struct {
	ServiceToken string
	SID          string
}

// exchangeServiceToken 复刻 xm2api lib/pipeline.mjs 的 exchangeServiceToken。
//
// 阶段 1：拿 location / ssecurity / nonce
// 阶段 2：clientSign = urlencode(base64(sha1("nonce=" + nonce [+ "&" + ssecurity])))
//         从 Set-Cookie 里取 serviceToken
func exchangeServiceToken(sid string, c mimoCred) (*ssoResult, error) {
	if c.PassToken == "" || c.UserID == "" {
		return nil, fmt.Errorf("缺少 pass_token / user_id，无法续期")
	}
	if sid == "" {
		sid = defaultSID
	}

	// ---- 阶段 1 ----
	u, err := url.Parse(phase1URL)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("_locale", "zh_CN")
	q.Set("_snsNone", "true")
	q.Set("sid", sid)
	q.Set("_json", "true")
	u.RawQuery = q.Encode()

	cookie := "passToken=" + c.PassToken + "; userId=" + c.UserID
	if c.CUserID != "" {
		cookie += "; cUserId=" + c.CUserID
	}

	body, err := ssoGet(u.String(), cookie)
	if err != nil {
		return nil, fmt.Errorf("阶段1 请求失败: %w", err)
	}
	text := strings.TrimPrefix(string(body), "&&&START&&&")

	var p1 struct {
		Code             *int   `json:"code"`
		Description      string `json:"description"`
		Location         string `json:"location"`
		Ssecurity        string `json:"ssecurity"`
		Nonce            any    `json:"nonce"`
		SecondValidation bool   `json:"secondValidation"`
		NotificationURL  string `json:"notificationUrl"`
	}
	if err := json.Unmarshal([]byte(text), &p1); err != nil {
		return nil, fmt.Errorf("阶段1 响应不是 JSON（%.120s）", text)
	}
	if p1.Code == nil || *p1.Code != 0 {
		code := -1
		if p1.Code != nil {
			code = *p1.Code
		}
		return nil, fmt.Errorf("阶段1 失败 code=%d %s", code, p1.Description)
	}
	if p1.Location == "" {
		return nil, fmt.Errorf("阶段1 未返回 location")
	}
	if p1.SecondValidation && p1.NotificationURL != "" {
		return nil, fmt.Errorf("账号需要二次验证: %s", p1.NotificationURL)
	}

	nonce := ""
	if m := nonceRe.FindStringSubmatch(text); len(m) == 2 {
		nonce = m[1]
	} else if p1.Nonce != nil {
		nonce = strings.TrimSpace(fmt.Sprint(p1.Nonce))
	}

	// ---- 阶段 2 ----
	signInput := "nonce=" + nonce
	if strings.TrimSpace(p1.Ssecurity) != "" {
		signInput += "&" + p1.Ssecurity
	}
	sum := sha1.Sum([]byte(signInput))
	clientSign := url.QueryEscape(base64.StdEncoding.EncodeToString(sum[:]))

	loc := p1.Location
	sep := "&"
	if !strings.Contains(loc, "?") {
		sep = "?"
	}
	body2, setCookies, err := ssoGetWithCookies(loc+sep+"clientSign="+clientSign)
	if err != nil {
		return nil, fmt.Errorf("阶段2 请求失败: %w", err)
	}
	_ = body2

	names := make([]string, 0, len(setCookies))
	for name, val := range setCookies {
		names = append(names, name)
		if name == "serviceToken" || name == sid+"_serviceToken" {
			return &ssoResult{ServiceToken: val, SID: sid}, nil
		}
	}
	return nil, fmt.Errorf("阶段2 响应里没有 serviceToken（拿到: %s）", strings.Join(names, ", "))
}

func ssoGet(rawURL, cookie string) ([]byte, error) {
	body, _, err := ssoGetWithCookiesAndCookie(rawURL, cookie)
	return body, err
}

func ssoGetWithCookies(rawURL string) ([]byte, map[string]string, error) {
	return ssoGetWithCookiesAndCookie(rawURL, "")
}

func ssoGetWithCookiesAndCookie(rawURL, cookie string) ([]byte, map[string]string, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("User-Agent", ssoUA)
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	client := &http.Client{Timeout: ssoTimeout}
	res, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, maxAuthFile))
	if err != nil {
		return nil, nil, err
	}
	out := map[string]string{}
	for _, ck := range res.Cookies() {
		out[ck.Name] = ck.Value
	}
	return body, out, nil
}

/* ------------------------------------------------------- auth.parse / refresh */

// deriveAuthID 是凭证 ID 的唯一来源 —— auth.parse / executor / 状态页必须一致，
// 否则缓存和状态会对不上号。
func deriveAuthID(c mimoCred) string {
	if c.UserID != "" {
		return providerKey + "-" + c.UserID
	}
	return providerKey
}

func authData(c mimoCred, fileName, id string, next time.Time) map[string]any {
	storage, _ := json.Marshal(c)
	if id == "" {
		id = deriveAuthID(c)
	}
	if fileName == "" {
		fileName = providerKey + ".json"
	}
	// label 带上剩余用量：宿主 auth-files 列表与面板列表页直接渲染 label，
	// 这是官方面板上插件 provider 唯一「不用点开详情」就能看到用量的位置。
	label := usageLabelFromSnap(providerKey, c.UserID, usageSnapshotFor(id))
	md := map[string]any{"type": providerKey}
	// 用量快照写进 Metadata → mergedStorageJSON 会把它落进 auth 文件 JSON，
	// 面板 auth-files 详情 INFO 视图直接可见（不含任何 token）。
	// ⚠️ 只放在这里才不会被下次 auth.refresh 落盘时抹掉（宿主保存 = StorageJSON ∪ Metadata）。
	if snap := usageSnapshotFor(id); snap != nil {
		md["usage_snapshot"] = snap
	}
	return map[string]any{
		"Provider":         providerKey,
		"ID":               id,
		"FileName":         fileName,
		"Label":            label,
		"Disabled":         false,
		"StorageJSON":      storage,
		"Metadata":         md,
		"Attributes":       map[string]string{"provider": providerKey},
		"NextRefreshAfter": next.UTC().Format(time.RFC3339),
	}
}

func handleAuthParse(req []byte) []byte {
	var in struct {
		Provider string `json:"Provider"`
		Path     string `json:"Path"`
		FileName string `json:"FileName"`
		RawJSON  []byte `json:"RawJSON"`
	}
	if err := json.Unmarshal(req, &in); err != nil {
		return errResult("invalid_request", "auth.parse 请求无法解析: "+err.Error(), 400)
	}

	var c mimoCred
	if err := json.Unmarshal(in.RawJSON, &c); err != nil {
		// 不是我们的格式，交还给宿主继续找别的解析器
		return okResult(map[string]any{"Handled": false})
	}
	c.normalize()
	if !c.usable() {
		return okResult(map[string]any{"Handled": false})
	}
	if c.SID == "" {
		c.SID = config().SID
	}

	next := time.Now().Add(refreshAfter())

	// 只给了 pass_token 时，parse 阶段就先换一次，用户少一步操作。
	if c.ServiceToken == "" && c.PassToken != "" {
		if sso, err := exchangeServiceToken(c.SID, c); err == nil {
			c.ServiceToken = sso.ServiceToken
			c.ObtainedAt = time.Now().UTC().Format(time.RFC3339)
			hostLog("info", "MiMo 凭证已通过 passToken 换取 serviceToken")
		} else {
			hostLog("warn", "MiMo 凭证阶段1换 token 失败（保留 pass_token，等 refresh 重试）: "+err.Error())
		}
	}
	if c.ServiceToken == "" {
		// 还没换成，早点重试
		next = time.Now().Add(2 * time.Minute)
	}

	authID := deriveAuthID(c)
	noteParsed(authID, c.UserID, c.ServiceToken, c.PassToken != "", "auth.parse")
	rememberCred(authID, c)
	// 先从文件里的 usage_snapshot 恢复内存用量（CPA 重启后 label 自愈），
	// 再异步取新鲜快照覆盖。顺序不能反，否则离线时文件快照会被空记录顶掉。
	restoreUsageFromRaw(authID, in.RawJSON)
	// 顺手取用量快照（失败不影响凭证加载）；放在 AuthData 组装之前，
	// 这样快照随本次 AuthData 一起进 auth 文件。
	tryAttachUsage(authID, c)
	dbg("auth.parse id=%s user=%s token=%dB canRenew=%v", authID, c.UserID, len(c.ServiceToken), c.PassToken != "")

	return okResult(map[string]any{
		"Handled": true,
		"Auth":    authData(c, in.FileName, authID, next),
	})
}

func handleAuthRefresh(req []byte) []byte {
	var in struct {
		AuthID      string          `json:"AuthID"`
		AuthProvider string         `json:"AuthProvider"`
		StorageJSON []byte          `json:"StorageJSON"`
		Metadata    json.RawMessage `json:"Metadata"`
	}
	if err := json.Unmarshal(req, &in); err != nil {
		return errResult("invalid_request", "auth.refresh 请求无法解析: "+err.Error(), 400)
	}

	var c mimoCred
	if err := json.Unmarshal(in.StorageJSON, &c); err != nil {
		return errResult("invalid_credential", "凭证内容无法解析: "+err.Error(), 400)
	}
	c.normalize()
	// 宿主 refresh 请求的 Metadata 里可能带着上次落盘的 usage_snapshot，
	// 先恢复内存用量（label/详情在重启后依然有数据），再由 tryAttachUsage 覆盖。
	restoreUsageFromRaw(deriveAuthID(c), in.Metadata)

	next := time.Now().Add(refreshAfter())

	switch {
	case c.PassToken != "":
		sid := c.SID
		if sid == "" {
			sid = config().SID
		}
		sso, err := exchangeServiceToken(sid, c)
		if err != nil {
			// 续期失败：不要立刻重试打爆上游，退避 5 分钟
			hostLog("warn", "MiMo serviceToken 续期失败: "+err.Error())
			dbg("auth.refresh 失败 id=%s: %v", in.AuthID, err)
			noteRefreshFailed(deriveAuthID(c), err)
			next = time.Now().Add(5 * time.Minute)
			break
		}
		c.ServiceToken = sso.ServiceToken
		c.SID = sso.SID
		c.ObtainedAt = time.Now().UTC().Format(time.RFC3339)
		hostLog("info", "MiMo serviceToken 已续期")
		dbg("auth.refresh 成功 id=%s token=%dB", in.AuthID, len(c.ServiceToken))
		noteParsed(deriveAuthID(c), c.UserID, c.ServiceToken, true, "定时续期")
		rememberCred(deriveAuthID(c), c)
		tryAttachUsage(deriveAuthID(c), c) // 续期成功后刷新用量快照，随 AuthData 落盘
	case c.ServiceToken != "":
		// 只有短凭证，没法续期；给个长周期避免宿主空转
		hostLog("debug", "凭证里没有 pass_token，跳过续期")
		noteParsed(deriveAuthID(c), c.UserID, c.ServiceToken, false, "auth文件（不可续期）")
	default:
		return errResult("invalid_credential", "凭证既没有 service_token 也没有 pass_token", 401)
	}

	return okResult(map[string]any{
		"Auth":             authData(c, "", in.AuthID, next),
		"NextRefreshAfter": next.UTC().Format(time.RFC3339),
	})
}
