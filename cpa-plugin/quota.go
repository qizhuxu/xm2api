package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

/*
 * quota_provider 能力：让 MiMo「剩余使用量」出现在 CPA 的配额体系里。
 *
 * 数据源：GET {base}/api/user/usage（Cookie 鉴权），实测响应：
 *   {"code":0,"message":"success","data":{"percent":84.4,"resetDate":"2026-09-23","resetAt":1790155916}}
 * percent = 剩余百分比（实测随请求消耗递减 84.5→84.4；消耗只会让已用量增加，
 * 观察到递减 ⇒ 只能是剩余量）。resetAt 是周期重置的 unix 秒。
 *
 * 宿主侧链路（CPA v7.3.9 内置，无需改核心）：
 *   capabilities.quota_provider=true → GET /v0/management/quota/providers 列出 mimo；
 *   POST /v0/management/quota/fetch {"auth_index":"..."} → RPC quota.fetch → 本文件
 *   返回规范化 QuotaFetchResponse{subscription/summary/groups[].buckets[]}，
 *   auth-files 列表条目自动带上 supports_quota/quota_provider 字段。
 *
 * ⚠️ 两个实测约束：
 *   1. 宿主的 QuotaFetchRequest 只带 AuthIndex/AuthID/Provider/Metadata/Attributes，
 *      不带 StorageJSON —— 凭证从插件内存缓存取（creds.go credForLookup）。
 *   2. 当前官方面板 SPA 的额度 UI 硬编码只渲染 7 家内置 provider，不消费插件配额。
 *      所以这里点亮的是后端 API（curl / 第三方面板 / 未来官方面板版本），
 *      同时把用量快照写进 auth JSON（authData 的 Metadata），面板 auth-files
 *      详情 INFO 视图可以直接看到剩余量。
 */

const usageAPIPath = "/api/user/usage"

// fetchUsage 打上游用量接口；401 且有 pass_token 时复用反应式续期后重试一次。
func fetchUsage(authID string, c mimoCred) (*usageData, error) {
	do := func(c mimoCred) (*http.Response, error) {
		req, err := http.NewRequest(http.MethodGet, config().BaseURL+usageAPIPath, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("accept", "application/json")
		req.Header.Set("Cookie", c.routeCookie())
		client := &http.Client{Timeout: 20 * time.Second}
		return client.Do(req)
	}

	res, err := do(c)
	if err != nil {
		return nil, err
	}
	if res.StatusCode == http.StatusUnauthorized && c.PassToken != "" {
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
		res.Body.Close()
		if rerr := renewServiceToken(authID, &c); rerr != nil {
			return nil, rerr
		}
		res, err = do(c)
		if err != nil {
			return nil, err
		}
	}
	defer res.Body.Close()

	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %.200s", res.StatusCode, string(body))
	}
	var parsed struct {
		Code    *int       `json:"code"`
		Message string     `json:"message"`
		Data    *usageData `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("用量响应无法解析: %w", err)
	}
	if parsed.Code != nil && *parsed.Code != 0 {
		return nil, fmt.Errorf("上游 code=%d %s", *parsed.Code, parsed.Message)
	}
	if parsed.Data == nil {
		return nil, fmt.Errorf("上游未返回 data（账号可能没有用量订阅）")
	}
	return parsed.Data, nil
}

// tryAttachUsage 在 auth.parse / auth.refresh 里顺手取用量快照。
// 失败不致命：用量查询绝不能影响凭证续期本身。
func tryAttachUsage(authID string, c mimoCred) {
	if c.ServiceToken == "" || c.routeCookie() == "" {
		return
	}
	u, err := fetchUsage(authID, c)
	noteUsage(authID, u, err)
	if err != nil {
		dbg("用量快照获取失败 authID=%s: %v", authID, err)
		return
	}
	// 异步把用量写进 auth 文件 label + usage_snapshot（面板列表直接可见）。
	usageGo(func() { persistUsageToAuthFiles(authID, u) })
}

/* ------------------------------------------------- 用量 → 面板可见 label */

// host.auth.* 通道（可测试替换）。宿主这组回调把 auth 文件的路径/内容/保存
// 都开放给插件：list 返回带 Path/AuthIndex/Label 的条目，get 返回凭证 JSON，
// save 写物理文件并刷新宿主内存记录的 label —— 面板 auth-files 列表随之更新。
var (
	hostAuthListCall = func() (json.RawMessage, error) {
		return hostCall(methodHostAuthList, map[string]any{})
	}
	hostAuthGetCall = func(authIndex string) (json.RawMessage, error) {
		return hostCall(methodHostAuthGet, map[string]any{"auth_index": authIndex})
	}
	hostAuthSaveFileCall = func(name string, payload []byte) (json.RawMessage, error) {
		// json 字段必须是凭证 JSON 对象原文（json.RawMessage）：传 []byte 会被
		// 编码成 base64，宿主 map 解析报 "invalid auth json"（实测）。
		return hostCall(methodHostAuthSave, map[string]any{"name": name, "json": json.RawMessage(payload)})
	}
)

// usageLabelFromSnap 生成面板 auth-files 列表直接渲染的 label：
// "mimo (用户) · 剩余 82% · 2026-09-23 重置"。snap 为 nil 时退回纯 provider label。
func usageLabelFromSnap(provider, userID string, snap map[string]any) string {
	l := provider
	if userID != "" {
		l = provider + " (" + userID + ")"
	}
	if snap != nil {
		if pct, ok := snap["remaining_percent"].(float64); ok {
			l += fmt.Sprintf(" · 剩余 %.0f%%", pct)
			if rd, _ := snap["reset_date"].(string); rd != "" {
				l += " · " + rd + " 重置"
			}
		}
	}
	return l
}

// restoreUsageFromRaw 从 auth 文件 JSON 里的 usage_snapshot 恢复内存用量记录。
// 场景：CPA 重启后插件内存清空；文件里持久化的快照让 label 与详情立即有用量，
// 随后 tryAttachUsage 再用实时数据覆盖。已有记录时不覆盖。
func restoreUsageFromRaw(authID string, raw []byte) {
	if authID == "" || len(raw) == 0 {
		return
	}
	if usageSnapshotFor(authID) != nil {
		return
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return
	}
	snap, _ := m["usage_snapshot"].(map[string]any)
	if snap == nil {
		return
	}
	u := &usageData{}
	if pct, ok := snap["remaining_percent"].(float64); ok {
		u.Percent = pct
	}
	if rd, ok := snap["reset_date"].(string); ok {
		u.ResetDate = rd
	}
	if ra, ok := snap["reset_at_unix"].(float64); ok {
		u.ResetAt = int64(ra)
	}
	if u.Percent == 0 && u.ResetDate == "" && u.ResetAt == 0 {
		return
	}
	noteUsage(authID, u, nil)
}

// persistUsageToAuthFiles 把用量快照写进 auth 文件的 label + usage_snapshot：
//   - 面板 auth-files 列表（渲染 label）→ 直接看到「剩余 xx%」
//   - 详情 INFO（宿主合并文件 JSON）→ usage_snapshot 全量快照
//
// 走宿主 host.auth.list/get/save 回调（file 来源与 host 管理来源统一处理）；
// save 由宿主落盘并刷新内存记录，插件不碰 auth 目录路径。label/快照没有变化
// 时跳过写盘，避免频繁触发热重载。任何失败只记 debug 日志，绝不影响凭证链路。
func persistUsageToAuthFiles(authID string, u *usageData) {
	if u == nil {
		return
	}
	raw, err := hostAuthListCall()
	if err != nil {
		dbg("用量落盘：host.auth.list 失败: %v", err)
		return
	}
	var list struct {
		Files []struct {
			AuthIndex string `json:"auth_index"`
			Name      string `json:"name"`
			Provider  string `json:"provider"`
			Type      string `json:"type"`
			Label     string `json:"label"`
		} `json:"files"`
	}
	if json.Unmarshal(raw, &list) != nil {
		return
	}
	snap := usageSnapshotFor(authID)
	cred := credForLookup(authID)
	saved := 0
	for _, f := range list.Files {
		if f.Provider != providerKey && f.Type != providerKey {
			continue
		}
		if f.Name == "" {
			continue
		}
		graw, gerr := hostAuthGetCall(f.AuthIndex)
		if gerr != nil {
			dbg("用量落盘：host.auth.get %s 失败: %v", f.Name, gerr)
			continue
		}
		var g struct {
			JSON json.RawMessage `json:"json"`
		}
		if json.Unmarshal(graw, &g) != nil || len(g.JSON) == 0 {
			continue
		}
		var m map[string]any
		if json.Unmarshal(g.JSON, &m) != nil {
			continue
		}
		// 只改属于该凭证的文件：user_id 对得上，或 user_id 缺失时按 service_token 对。
		fileUID, _ := m["user_id"].(string)
		if cred.UserID != "" && fileUID != "" && fileUID != cred.UserID {
			continue
		}
		if fileUID == "" && cred.UserID != "" && cred.ServiceToken != "" {
			if tok, _ := m["service_token"].(string); tok != "" && tok != cred.ServiceToken {
				continue
			}
		}
		uid := fileUID
		if uid == "" {
			uid = cred.UserID
		}
		newLabel := usageLabelFromSnap(providerKey, uid, snap)
		curLabel := f.Label
		if l, _ := m["label"].(string); l != "" {
			curLabel = l
		}
		oldSnap, _ := m["usage_snapshot"].(map[string]any)
		if curLabel == newLabel && sameUsagePercent(oldSnap, u.Percent) {
			continue
		}
		m["label"] = newLabel
		if snap != nil {
			m["usage_snapshot"] = snap
		}
		payload, merr := json.Marshal(m)
		if merr != nil {
			continue
		}
		if _, serr := hostAuthSaveFileCall(f.Name, payload); serr != nil {
			dbg("用量落盘：host.auth.save %s 失败: %v", f.Name, serr)
			continue
		}
		saved++
		dbg("用量已写入 auth 文件 %s: %s", f.Name, newLabel)
	}
	if saved > 0 {
		hostLog("info", fmt.Sprintf("MiMo 剩余用量已更新到 auth 文件 label（%.0f%%）", u.Percent))
	}
}

func sameUsagePercent(old map[string]any, pct float64) bool {
	if old == nil {
		return false
	}
	v, ok := old["remaining_percent"].(float64)
	return ok && v == pct
}

/* ------------------------------------------------------- quota.* RPC */

func handleQuotaDescribe(req []byte) []byte {
	return okResult(map[string]any{
		"supported_providers": []string{providerKey},
		"display_name":        "MiMo (Xiaomi) 剩余使用量",
		"supports_reset":      false,
	})
}

func handleQuotaFetch(req []byte) []byte {
	var in struct {
		AuthIndex   string         `json:"auth_index"`
		AuthID      string         `json:"auth_id"`
		Provider    string         `json:"provider"`
		StorageJSON []byte         `json:"storage_json"`
		Metadata    map[string]any `json:"metadata"`
	}
	_ = json.Unmarshal(req, &in)

	// 凭证解析顺序：storage_json（万一宿主将来带上）→ 插件内存缓存
	var c mimoCred
	if len(in.StorageJSON) > 0 {
		_ = json.Unmarshal(in.StorageJSON, &c)
		c.normalize()
	}
	if c.ServiceToken == "" || c.PassToken == "" {
		lookup := credForLookup(in.AuthID)
		if c.ServiceToken == "" {
			c.ServiceToken = lookup.ServiceToken
		}
		if c.PassToken == "" {
			c.PassToken = lookup.PassToken
		}
		if c.UserID == "" {
			c.UserID = lookup.UserID
		}
		if c.CUserID == "" {
			c.CUserID = lookup.CUserID
		}
		if c.SID == "" {
			c.SID = lookup.SID
		}
	}
	if c.ServiceToken == "" && c.PassToken == "" {
		return errResult("invalid_credential",
			"没有可用的 MiMo 凭证（宿主未带 storage_json，插件内存缓存也为空）", 401)
	}

	authID := in.AuthID
	if authID == "" {
		authID = deriveAuthID(c)
	}

	u, err := fetchUsage(authID, c)
	if err != nil {
		noteUsage(authID, nil, err)
		return errResult("upstream_unavailable", "MiMo 用量查询失败: "+err.Error(), 502)
	}
	noteUsage(authID, u, nil)
	dbg("quota.fetch authID=%s remaining=%.1f%% reset=%s", authID, u.Percent, u.ResetDate)
	// 实时查询也顺手刷新 auth 文件 label（面板列表可见）
	usageGo(func() { persistUsageToAuthFiles(authID, u) })

	resetTime := u.ResetDate
	if u.ResetAt > 0 {
		resetTime = time.Unix(u.ResetAt, 0).UTC().Format(time.RFC3339)
	}
	window := "当前周期"
	if u.ResetDate != "" {
		window = "至 " + u.ResetDate
	}

	// 规范化配额响应：面板渲染组件认 remainingFraction（0~1）+ resetTime。
	return okResult(map[string]any{
		"subscription": map[string]any{"plan": "MiMo (Xiaomi)"},
		"summary": []map[string]any{{
			"key": "remaining_percent", "label": "剩余用量",
			"value": u.Percent, "unit": "%", "format": "number",
		}},
		"groups": []map[string]any{{
			"displayName": "MiMo 用量周期",
			"buckets": []map[string]any{{
				"window":            window,
				"remainingFraction": u.Percent / 100,
				"resetTime":         resetTime,
				"description":       fmt.Sprintf("上游 /api/user/usage：剩余 %.1f%%，%s 重置", u.Percent, orDash(u.ResetDate)),
			}},
		}},
	})
}

func handleQuotaReset(req []byte) []byte {
	// describe 里已声明 supports_reset=false；这里兜底应答，避免 unknown_method。
	return okResult(map[string]any{
		"success": false,
		"message": "MiMo 用量周期由上游管理，无法重置，等待 resetDate 自动恢复",
	})
}
