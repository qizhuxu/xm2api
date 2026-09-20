package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"time"
)

/*
 * 模型发现：GET {base}/api/model/list
 *
 * 上游响应形状（与 xm2api server.mjs fetchUpstreamModels 一致）：
 *   {"code":0,"data":{"models":[{"modelName":...,"modelType":...,"vendorName":...}]}}
 */

type upstreamModel struct {
	ModelName    string `json:"modelName"`
	ModelType    string `json:"modelType"`
	VendorName   string `json:"vendorName"`
	Description  string `json:"description"`
	Billable     any    `json:"billable"`
	DisplayRatio any    `json:"displayRatio"`
	Ratio        *struct {
		InputPricePerM       *float64 `json:"inputPricePerM"`
		OutputPricePerM      *float64 `json:"outputPricePerM"`
		CachedPricePerM      *float64 `json:"cachedPricePerM"`
		ImageResolutionPrices []struct {
			Tier         string  `json:"tier"`
			PricePerImage float64 `json:"pricePerImage"`
		} `json:"imageResolutionPrices"`
	} `json:"ratio"`
}

// modelInfo 对应官方 pluginapi.ModelInfo。字段名（大小写）必须一致：
// 宿主用 encoding/json 按字段名匹配，虽然大小写不敏感，但不要改成 snake_case。
type modelInfo struct {
	ID                         string   `json:"ID"`
	Object                     string   `json:"Object"`
	OwnedBy                    string   `json:"OwnedBy"`
	Type                       string   `json:"Type,omitempty"`
	DisplayName                string   `json:"DisplayName,omitempty"`
	Name                       string   `json:"Name,omitempty"`
	Description                string   `json:"Description,omitempty"`
	SupportedGenerationMethods []string `json:"SupportedGenerationMethods,omitempty"`
	SupportedInputModalities   []string `json:"SupportedInputModalities,omitempty"`
	SupportedOutputModalities  []string `json:"SupportedOutputModalities,omitempty"`
	UserDefined                bool     `json:"UserDefined,omitempty"`
}

// 兜底清单：上游取不到时至少保证 /v1/models 不空。
// 只放实测确认过的两个文本模型，不猜。
var fallbackModels = []upstreamModel{
	{ModelName: "mimo-x-pro-preview", ModelType: "TEXT", VendorName: "Mify"},
	{ModelName: "mimo-x-flash-preview", ModelType: "TEXT", VendorName: "Mify"},
}

// 按 modelType 标注能力。与 xm2api 的 TYPE_CAPS 对齐，都是逐项实测过的。
func toModelInfo(m upstreamModel) modelInfo {
	typ := strings.ToUpper(strings.TrimSpace(m.ModelType))
	info := modelInfo{
		ID:          m.ModelName,
		Object:      "model",
		OwnedBy:     orDefault(m.VendorName, "xiaomi"),
		Type:        typ,
		DisplayName: m.ModelName,
		Name:        m.ModelName,
		Description: m.Description,
	}
	switch typ {
	case "IMAGE_GENERATION":
		info.SupportedGenerationMethods = []string{"image"}
		info.SupportedInputModalities = []string{"text"}
		info.SupportedOutputModalities = []string{"image"}
	case "TTS":
		// 注意：TTS/ASR 实际走的是 chat/completions + 扩展字段，不是 /v1/audio/*
		info.SupportedGenerationMethods = []string{"chat"}
		info.SupportedInputModalities = []string{"text"}
		info.SupportedOutputModalities = []string{"audio"}
	case "ASR":
		info.SupportedGenerationMethods = []string{"chat"}
		info.SupportedInputModalities = []string{"audio"}
		info.SupportedOutputModalities = []string{"text"}
	default: // TEXT
		info.SupportedGenerationMethods = []string{"chat"}
		info.SupportedInputModalities = []string{"text", "image"}
		info.SupportedOutputModalities = []string{"text"}
	}
	return info
}

func orDefault(s, d string) string {
	if strings.TrimSpace(s) == "" {
		return d
	}
	return s
}

func fallbackInfos() []modelInfo {
	out := make([]modelInfo, 0, len(fallbackModels))
	for _, m := range fallbackModels {
		out = append(out, toModelInfo(m))
	}
	return out
}

// matchModel 支持精确匹配和 * / ? 通配（大小写不敏感）。
func matchModel(pattern, name string) bool {
	p := strings.ToLower(strings.TrimSpace(pattern))
	n := strings.ToLower(strings.TrimSpace(name))
	if p == "" {
		return false
	}
	if !strings.ContainsAny(p, "*?[") {
		return p == n
	}
	ok, err := path.Match(p, n)
	return err == nil && ok
}

// applyExclusions 按 exclude_models 过滤。
//
// 存在的意义：上游目录里有 Doubao-Seedream-5.0-pro（图像模型），但 CPA 的
// /v1/images/generations 有硬编码白名单，插件执行器服务不了它。把它列在
// /v1/models 里会让人以为是可用的，所以给用户一个隐藏的开关。
func applyExclusions(list []modelInfo) []modelInfo {
	patterns := config().ExcludeModels
	if len(patterns) == 0 {
		return list
	}
	out := make([]modelInfo, 0, len(list))
	for _, m := range list {
		skip := false
		for _, p := range patterns {
			if matchModel(p, m.ID) {
				skip = true
				break
			}
		}
		if !skip {
			out = append(out, m)
		}
	}
	return out
}

func fetchUpstreamModels(cookie string) ([]upstreamModel, error) {
	url := config().BaseURL + "/api/model/list"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("accept", "application/json")
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %.200s", res.StatusCode, string(body))
	}
	var parsed struct {
		Code    *int   `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Models []upstreamModel `json:"models"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("上游目录无法解析: %w", err)
	}
	if parsed.Code != nil && *parsed.Code != 0 {
		return nil, fmt.Errorf("上游返回 code=%d %s", *parsed.Code, parsed.Message)
	}
	if len(parsed.Data.Models) == 0 {
		return nil, fmt.Errorf("上游返回了空目录")
	}
	return parsed.Data.Models, nil
}

// resolveModels 带 TTL 缓存地拿模型清单；上游失败时回落缓存/兜底。
func resolveModels(cookie string) ([]modelInfo, string) {
	// ⚠️ modelTTL() 内部会走 config() → mu.RLock()，而 sync.RWMutex 不可重入。
	// 必须在拿写锁**之前**算好 TTL，否则一旦持锁再调 config() 就是永久死锁
	// （会把之后所有请求一起拖死，因为写锁再也不释放）。
	ttl := modelTTL()

	mu.Lock()
	cached := modelCache.models
	at := modelCache.at
	mu.Unlock()

	fresh := !at.IsZero() && time.Since(at) < ttl
	if fresh && len(cached) > 0 {
		return cached, "cache"
	}

	// 没有凭证时不要打上游 —— model.static 在凭证加载前就会被调用，
	// 不带 cookie 问上游必然是 401，白白刷一条 warn。
	if cookie == "" {
		if len(cached) > 0 {
			return cached, "stale"
		}
		return fallbackInfos(), "fallback"
	}

	list, err := fetchUpstreamModels(cookie)
	source := "upstream"
	if err != nil {
		hostLog("warn", "MiMo 模型目录获取失败: "+err.Error())
		if len(cached) > 0 {
			return cached, "stale"
		}
		list = fallbackModels
		source = "fallback"
	}

	out := make([]modelInfo, 0, len(list))
	for _, m := range list {
		if strings.TrimSpace(m.ModelName) == "" {
			continue
		}
		out = append(out, toModelInfo(m))
	}
	if len(out) == 0 {
		out = fallbackInfos()
		source = "fallback"
	}

	if source == "upstream" {
		mu.Lock()
		modelCache.at = time.Now()
		modelCache.models = out
		mu.Unlock()
	}
	return out, source
}

// handleModels 同时服务 model.static 与 model.for_auth。
//
// model.for_auth 带凭证，能拿到 cookie 去问上游；model.static 没有凭证，
// 只能吃缓存或兜底清单。两者返回同一个 catalog，保证模型名一致。
func handleModels(method string, req []byte) []byte {
	cookie := ""
	if method == methodModelForAuth {
		var in struct {
			AuthID       string `json:"AuthID"`
			AuthProvider string `json:"AuthProvider"`
			StorageJSON  []byte `json:"StorageJSON"`
		}
		if err := json.Unmarshal(req, &in); err == nil && len(in.StorageJSON) > 0 {
			var c mimoCred
			if json.Unmarshal(in.StorageJSON, &c) == nil {
				c.normalize()
				cookie = c.routeCookie()
			}
		}
	}

	models, source := resolveModels(cookie)
	// 过滤放在这里而不是缓存里：改了 exclude_models 立刻生效，不用等缓存过期
	models = applyExclusions(models)
	hostLog("debug", fmt.Sprintf("MiMo 模型目录: %d 个（来源 %s）", len(models), source))

	return okResult(map[string]any{
		"Provider": providerKey,
		"Models":   models,
	})
}
