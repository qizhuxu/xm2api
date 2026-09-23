package main

/*
 * 自动收码（auto-OTP）：小米「新设备保护」邮箱验证码的无人值守通道。
 *
 * 背景（实测 2026-09-22）：serviceLoginAuth2 账密登录必触发 securityStatus=16
 * （新设备保护）→ notificationUrl，邮箱验证码这一步小米服务端强制，绕不开。
 * 但当账号是自建临时邮局（qizhuqi.com temp-mail，admin API 自控）时，插件可以
 * 自动收码、自动提交：用户只填账密，其余全自动；超时/失败回落人工输入。
 *
 * 配置（config.yaml plugins.configs.mimo，默认关闭）：
 *   otp_auto_mail:
 *     admin_base: https://m.qizhuqi.com
 *     admin_auth: <x-admin-auth 密钥>
 * 只对能解析出 @ 邮箱地址的账号启用；未配置时行为与人工流程完全一致。
 *
 * 安全：admin_auth 只出现在宿主配置文件与 temp-mail 请求头；验证码只在内存
 * 流转，不落盘、不进日志、不回显。
 */

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// otpMailCfg 对应 plugins.configs.mimo.otp_auto_mail。
type otpMailCfg struct {
	AdminBase string `yaml:"admin_base"`
	AdminAuth string `yaml:"admin_auth"`
}

// otpMailBox 是一个已登录（jwt）的临时邮箱收件箱。
type otpMailBox struct {
	cfg  otpMailCfg
	addr string
	jwt  string
}

// autoMailFor 判断某账号是否适用自动收码，返回配置与规范化邮箱地址。
func autoMailFor(user string) (*otpMailCfg, string) {
	mc := config().OTPAutoMail
	if mc == nil || strings.TrimSpace(mc.AdminBase) == "" || strings.TrimSpace(mc.AdminAuth) == "" {
		return nil, ""
	}
	addr := strings.TrimSpace(user)
	if !strings.Contains(addr, "@") {
		return nil, ""
	}
	return mc, addr
}

func (m *otpMailBox) adminGET(path string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, m.cfg.AdminBase+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-admin-auth", m.cfg.AdminAuth)
	res, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("temp-mail admin %s HTTP %d", path, res.StatusCode)
	}
	return body, nil
}

func (m *otpMailBox) userGET(path string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, m.cfg.AdminBase+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+m.jwt)
	res, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("temp-mail inbox %s HTTP %d", path, res.StatusCode)
	}
	return body, nil
}

type otpMailItem struct {
	ID   any    `json:"id"`
	From string `json:"source"`
}

func (it otpMailItem) id() string {
	switch v := it.ID.(type) {
	case string:
		return v
	case float64:
		return fmt.Sprintf("%.0f", v)
	default:
		return fmt.Sprintf("%v", v)
	}
}

func parseMailItems(raw []byte) []otpMailItem {
	var wrap struct {
		Results []otpMailItem `json:"results"`
		Data    []otpMailItem `json:"data"`
	}
	_ = json.Unmarshal(raw, &wrap)
	if len(wrap.Results) > 0 {
		return wrap.Results
	}
	if len(wrap.Data) > 0 {
		return wrap.Data
	}
	var list []otpMailItem
	_ = json.Unmarshal(raw, &list)
	return list
}

// connectMailbox 登录取件箱并抓一份邮件 id 基线（发码前的时间截面）。
func connectMailbox(cfg otpMailCfg, addr string) (*otpMailBox, map[string]bool, error) {
	box := &otpMailBox{cfg: cfg, addr: addr}
	raw, err := box.adminGET("/admin/address?limit=20&offset=0&keyword=" + url.QueryEscape(addr))
	if err != nil {
		return nil, nil, err
	}
	var wrap struct {
		Results []struct {
			Name string `json:"name"`
			ID   any    `json:"id"`
		} `json:"results"`
		Data []struct {
			Name string `json:"name"`
			ID   any    `json:"id"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &wrap)
	list := wrap.Results
	if len(list) == 0 {
		list = wrap.Data
	}
	mailID := ""
	for _, a := range list {
		if a.Name == addr {
			mailID = fmt.Sprintf("%v", a.ID)
			break
		}
	}
	if mailID == "" {
		return nil, nil, fmt.Errorf("邮箱地址不存在: %s", addr)
	}
	pwRaw, err := box.adminGET("/admin/show_password/" + mailID)
	if err != nil {
		return nil, nil, err
	}
	var pw struct {
		JWT string `json:"jwt"`
	}
	_ = json.Unmarshal(pwRaw, &pw)
	if pw.JWT == "" {
		return nil, nil, fmt.Errorf("邮箱 %s 取 jwt 失败", addr)
	}
	box.jwt = pw.JWT

	baseline := map[string]bool{}
	if mails, err := box.fetchMails(); err == nil {
		for _, m := range mails {
			baseline[m.id()] = true
		}
	}
	return box, baseline, nil
}

func (m *otpMailBox) fetchMails() ([]otpMailItem, error) {
	raw, err := m.userGET("/api/mails?limit=20&offset=0")
	if err != nil {
		return nil, err
	}
	return parseMailItems(raw), nil
}

// fetchMailTexts 取一封邮件的正文块（html/text/subject），多端点兼容。
func (m *otpMailBox) fetchMailTexts(id string) []string {
	for _, p := range []string{"/api/parsed_mail/" + id, "/api/mails/" + id, "/api/mail/" + id} {
		raw, err := m.userGET(p)
		if err != nil {
			continue
		}
		var detail struct {
			Result map[string]any `json:"result"`
			HTML   any            `json:"html"`
			Text   any            `json:"text"`
			Subj   any            `json:"subject"`
			Raw    any            `json:"raw"`
		}
		_ = json.Unmarshal(raw, &detail)
		obj := map[string]any{}
		if detail.Result != nil {
			obj = detail.Result
		} else {
			_ = json.Unmarshal(raw, &obj)
		}
		var out []string
		for _, k := range []string{"html", "text", "subject", "raw"} {
			if v, ok := obj[k]; ok {
				if s := fmt.Sprintf("%v", v); s != "" && s != "<nil>" {
					out = append(out, s)
				}
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

var (
	otpSemanticRe = regexp.MustCompile(`验证码[^0-9]{0,12}(\d{6})`)
	otpAnyRe      = regexp.MustCompile(`\b(\d{6})\b`)
)

// extractOTPCode 从邮件文本提取 6 位验证码（优先小米语境「验证码：123456」）。
func extractOTPCode(texts []string) string {
	for _, t := range texts {
		if m := otpSemanticRe.FindStringSubmatch(t); m != nil {
			return m[1]
		}
	}
	for _, t := range texts {
		if m := otpAnyRe.FindStringSubmatch(t); m != nil {
			return m[1]
		}
	}
	return ""
}

// updatePending 仅在会话仍处于 awaiting-otp 时改写状态（避免覆盖竞态下的 success）。
func updatePending(s *qrSession, status, msg string) {
	qrStore.Lock()
	if s.Status == "awaiting-otp" {
		s.Status = status
		s.Message = msg
	}
	qrStore.Unlock()
}

// autoOTPWorker：轮询邮箱等小米验证邮件 → 提取验证码 → otpSubmit → finishLogin。
// 任何失败只降级提示人工输入，绝不影响已建立的 awaiting-otp 会话。
func autoOTPWorker(s *qrSession, otp *otpState, box *otpMailBox, baseline map[string]bool) {
	const maxTries = 45 // ≈3 分钟（4s 间隔）
	for i := 0; i < maxTries; i++ {
		time.Sleep(4 * time.Second)

		qrStore.Lock()
		st := s.Status
		qrStore.Unlock()
		if st != "awaiting-otp" {
			return // 用户已手动完成/会话已结束
		}

		mails, err := box.fetchMails()
		if err != nil {
			continue
		}
		for _, it := range mails {
			id := it.id()
			if baseline[id] {
				continue
			}
			baseline[id] = true
			if it.From != "" && !strings.Contains(strings.ToLower(it.From), "xiaomi") {
				continue
			}
			code := extractOTPCode(box.fetchMailTexts(id))
			if code == "" {
				continue
			}
			dbg("auto-otp: 从邮件 %s 提取到验证码，自动提交", id)
			tok, err := otpSubmit(otp, code)
			if err != nil {
				updatePending(s, "awaiting-otp", "自动提交验证码失败："+err.Error()+"，请手动输入验证码")
				return
			}
			finishLogin(s, tok)
			return
		}
	}
	updatePending(s, "awaiting-otp", "自动收码超时（3 分钟），请手动输入验证码")
}
