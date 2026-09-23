// mimo-cpa-plugin —— 把小米 MiMo（MiMo Desktop SSO 会话）接入 CLIProxyAPI 的原生插件。
//
// 这是 xm2api「线路2」的 CLIProxyAPI 原生移植：不再需要中间那个 Node 反代进程，
// SSO 凭证、模型发现、请求执行全部在 CPA 进程内完成。
//
// 声明了三个能力：
//
//	auth_provider   —— 解析 mimo.json 凭证，并用 passToken 做两阶段 SSO 换 serviceToken
//	model_provider  —— 从上游 /api/model/list 动态发现模型
//	executor        —— 把 chat/completions 转发到 /api/route/chat/completions（含真流式）
//
// 注意 CPA 的硬性约束：插件 executor 必须有一条同 provider key 的 auth 记录，
// 所以 auth_provider 是必需的，不是可选项。
package main

/*
#include <stdlib.h>
#include "abi.h"
*/
import "C"

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unsafe"

	"gopkg.in/yaml.v3"
)

const (
	providerKey = "mimo"
	pluginVer   = "0.1.0"
	pluginName  = "MiMo (Xiaomi MiMo Desktop SSO)"

	defaultBase = "https://mimo-server-cn.xiaomimimo.com"
	defaultSID  = "mimopc"

	// ABI schema_version：3 表示流式 chunk 上不再回传 OriginalRequest/RequestBody。
	schemaVersion = 3
)

var pluginRepo = "https://github.com/your-org/xm2api"

/* ------------------------------------------------------------------ 配置 */

// cfg 是 plugins.configs.mimo 下的插件自有配置。宿主只解析 enabled/priority，
// 其余字段原样透传，所以这里全部可选、都有默认值。
type cfg struct {
	BaseURL       string      `yaml:"base_url"`
	SID           string      `yaml:"sid"`
	WebSearchAuto bool        `yaml:"web_search_auto"`
	RefreshAfter  string      `yaml:"refresh_after"`
	ModelTTL      string      `yaml:"model_ttl"`
	ExcludeModels []string    `yaml:"exclude_models"`
	LogToHost     bool        `yaml:"log_to_host"`
	OTPAutoMail   *otpMailCfg `yaml:"otp_auto_mail"`
}

func defaultCfg() cfg {
	return cfg{
		BaseURL:       defaultBase,
		SID:           defaultSID,
		WebSearchAuto: false,
		RefreshAfter:  "6h",
		ModelTTL:      "10m",
		LogToHost:     false,
	}
}

var (
	mu      sync.RWMutex
	hostAPI *C.cliproxy_host_api
	cur     = defaultCfg()
	// 模型清单缓存，避免每个请求都打上游
	modelCache struct {
		at     time.Time
		models []modelInfo
	}
)

func config() cfg {
	mu.RLock()
	defer mu.RUnlock()
	return cur
}

func setConfig(c cfg) {
	def := defaultCfg()
	if strings.TrimSpace(c.BaseURL) == "" {
		c.BaseURL = def.BaseURL
	}
	if strings.TrimSpace(c.SID) == "" {
		c.SID = def.SID
	}
	if strings.TrimSpace(c.RefreshAfter) == "" {
		c.RefreshAfter = def.RefreshAfter
	}
	if strings.TrimSpace(c.ModelTTL) == "" {
		c.ModelTTL = def.ModelTTL
	}
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	mu.Lock()
	cur = c
	mu.Unlock()
}

func refreshAfter() time.Duration {
	d, err := time.ParseDuration(config().RefreshAfter)
	if err != nil || d <= 0 {
		return 6 * time.Hour
	}
	return d
}

func modelTTL() time.Duration {
	d, err := time.ParseDuration(config().ModelTTL)
	if err != nil || d <= 0 {
		return 10 * time.Minute
	}
	return d
}

/* ------------------------------------------------------------- JSON 信封 */

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envError       `json:"error,omitempty"`
}

type envError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

func okResult(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		return errResult("internal_error", err.Error(), 500)
	}
	out, _ := json.Marshal(envelope{OK: true, Result: raw})
	return out
}

// errResult 构造失败信封。**必须**带 http_status，否则 CPA 一律降级成 500，
// 客户端会把 401/429 这类正常业务错误当成网关故障去退避重试。
func errResult(code, msg string, status int) []byte {
	out, _ := json.Marshal(envelope{OK: false, Error: &envError{Code: code, Message: msg, HTTPStatus: status}})
	return out
}

func identifierResult() []byte { return okResult(map[string]string{"identifier": providerKey}) }

/* --------------------------------------------------------- 宿主回调桥 */

// hostCall 调用 host.* 能力，拆掉外层信封返回 result。
func hostCall(method string, payload any) (json.RawMessage, error) {
	mu.RLock()
	h := hostAPI
	mu.RUnlock()
	if h == nil {
		return nil, errors.New("宿主 API 不可用（插件未正确初始化）")
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))

	var reqPtr *C.uint8_t
	if len(body) > 0 {
		reqPtr = (*C.uint8_t)(unsafe.Pointer(&body[0]))
	}

	var buf C.cliproxy_buffer
	rc := C.bridge_host_call(h, cMethod, reqPtr, C.size_t(len(body)), &buf)
	if rc != 0 {
		C.bridge_host_free(h, buf.ptr, buf.len)
		return nil, fmt.Errorf("宿主回调 %s 返回码 %d", method, int(rc))
	}
	if buf.ptr == nil || buf.len == 0 {
		C.bridge_host_free(h, buf.ptr, buf.len)
		return nil, fmt.Errorf("宿主回调 %s 返回空", method)
	}
	raw := C.GoBytes(buf.ptr, C.int(buf.len))
	C.bridge_host_free(h, buf.ptr, buf.len)

	// 宿主返回的也是信封
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("宿主回调 %s 响应无法解析: %w", method, err)
	}
	if !env.OK {
		if env.Error != nil {
			return nil, fmt.Errorf("%s: %s", env.Error.Code, env.Error.Message)
		}
		return nil, fmt.Errorf("宿主回调 %s 失败", method)
	}
	return env.Result, nil
}

/* ------------------------------------------------------------- ABI 导出 */

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	if host != nil {
		mu.Lock()
		hostAPI = host
		mu.Unlock()
	}
	C.bridge_fill_plugin(plugin)
	return 0
}

//export cliproxyGoCall
func cliproxyGoCall(method *C.char, req *C.uint8_t, reqLen C.size_t, out *C.cliproxy_buffer) C.int {
	if out != nil {
		out.ptr = nil
		out.len = 0
	}
	var body []byte
	if req != nil && reqLen > 0 {
		body = C.GoBytes(unsafe.Pointer(req), C.int(reqLen))
	}
	writeBuffer(out, dispatch(C.GoString(method), body))
	return 0
}

//export cliproxyGoFree
func cliproxyGoFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyGoShutdown
func cliproxyGoShutdown() {
	mu.Lock()
	hostAPI = nil
	mu.Unlock()
}

// writeBuffer 把 Go 字节切片拷进宿主提供的缓冲区（宿主之后会调 free_buffer 释放）。
func writeBuffer(out *C.cliproxy_buffer, data []byte) {
	if out == nil || len(data) == 0 {
		return
	}
	p := C.malloc(C.size_t(len(data)))
	if p == nil {
		return
	}
	copy(unsafe.Slice((*byte)(p), len(data)), data)
	out.ptr = p
	out.len = C.size_t(len(data))
}

/* --------------------------------------------------------------- 方法分发 */

func dispatch(method string, req []byte) []byte {
	defer trace(method)()
	switch method {
	case methodPluginRegister, methodPluginReconfigure:
		return handleRegister(req)
	case methodPluginQuiesce, methodPluginShutdown:
		return okResult(map[string]any{})

	case methodAuthIdentifier, methodExecutorIdentifier:
		return identifierResult()

	case methodAuthParse:
		return handleAuthParse(req)
	case methodAuthRefresh:
		return handleAuthRefresh(req)
	case methodAuthLoginStart:
		// 面板 #/oauth「SSO 登录」入口：返回一次性登录页（扫码 + 账号密码双通道）
		return handleAuthLoginStartRPC(req)
	case methodAuthLoginPoll:
		return handleAuthLoginPollRPC(req)

	case methodModelStatic, methodModelForAuth:
		return handleModels(method, req)

	case methodExecutorExecute:
		return handleExecute(req)
	case methodExecutorExecuteStream:
		return handleExecuteStream(req)
	case methodExecutorCountTokens:
		return handleCountTokens(req)

	case methodRequestNormalize:
		return handleNormalize(req)

	case methodQuotaIdentifier:
		return identifierResult()
	case methodQuotaDescribe:
		return handleQuotaDescribe(req)
	case methodQuotaFetch:
		return handleQuotaFetch(req)
	case methodQuotaReset:
		return handleQuotaReset(req)

	case methodManagementRegister:
		return handleManagementRegister(req)
	case methodManagementHandle:
		return handleManagement(req)
	}
	return errResult("unknown_method", "未实现的方法: "+method, 0)
}

func handleRegister(req []byte) []byte {
	var in struct {
		ConfigYAML    []byte `json:"config_yaml"`
		SchemaVersion uint32 `json:"schema_version"`
	}
	if err := json.Unmarshal(req, &in); err == nil && len(in.ConfigYAML) > 0 {
		var parsed cfg
		if err := yaml.Unmarshal(in.ConfigYAML, &parsed); err != nil {
			hostLog("warn", "插件配置解析失败，沿用默认值: "+err.Error())
		} else {
			setConfig(parsed)
		}
	}

	resp := map[string]any{
		"schema_version": schemaVersion,
		"metadata": map[string]any{
			"Name":             pluginName,
			"Version":          pluginVer,
			"Author":           "xm2api",
			"GitHubRepository": pluginRepo,
			"ConfigFields": []map[string]any{
				{"Name": "base_url", "Type": "string", "Description": "MiMo 上游地址，默认 " + defaultBase},
				{"Name": "sid", "Type": "string", "Description": "SSO 服务标识，默认 " + defaultSID},
				{"Name": "web_search_auto", "Type": "boolean", "Description": "请求未自带 tools 时自动追加 {\"type\":\"web_search\"}（等价于 CPA 的 payload 规则，二选一）"},
				{"Name": "refresh_after", "Type": "string", "Description": "多久主动换一次 serviceToken，默认 6h"},
				{"Name": "model_ttl", "Type": "string", "Description": "模型清单缓存时长，默认 10m"},
				{"Name": "exclude_models", "Type": "array", "Description": "要从模型列表里隐藏的模型名，支持 * 通配。例如 [\"Doubao-*\"]。图像模型（Doubao-Seedream-5.0-pro）现已可经插件服务 /v1/images/generations，是否隐藏取决于客户端需求"},
				{"Name": "log_to_host", "Type": "boolean", "Description": "把插件事件写进宿主日志（启动 CPA 的终端）。默认关闭 —— 事件只进 %TEMP%/mimo-plugin.log 文件日志，不刷终端"},
			},
		},
		"capabilities": map[string]any{
			"auth_provider":        true,
			"model_provider":       true,
			"executor":             true,
			"executor_model_scope": "both",
			// "openai-image" 是 CPA 内部图像执行的格式串：declared 包含它，
			// /v1/images/* 的请求 payload 才会直通插件 executor（见 README §图像生成）。
			"executor_input_formats":  []string{"chat-completions", "openai-image"},
			"executor_output_formats": []string{"chat-completions", "openai-image"},
			"request_normalizer":      true,
			"management_api":          true,
			// quota_provider：管理面 /v0/management/quota/* 由此点亮（剩余使用量）
			"quota_provider": true,
		},
	}
	return okResult(resp)
}

// main 在 c-shared 构建下不会被调用，但 package main 必须有它才能链接。
func main() {}
