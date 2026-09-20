package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

/*
 * 执行器：把 chat/completions 转发到 MiMo 的 /api/route/chat/completions。
 *
 * 上游数据面刻意使用插件自带的 net/http，而不是 host.http.*：
 *   - 流式需要字节级的增量读取控制，宿主的 host.http.do_stream + stream_read
 *     是给「同步调用内消费」设计的，而这里必须在 goroutine 里跨调用生命周期读；
 *   - 少一条依赖宿主回调上下文的路径，失败面更小。
 * 只有下游推送（host.stream.emit/close）和日志用宿主回调 —— 那是结构性必需的。
 */

const upstreamTimeout = 300 * time.Second

var nonChatName = regexp.MustCompile(`(?i)tts|asr|seedream|image|voiceclone|voicedesign|embedding|rerank`)

type execRequest struct {
	AuthID         string              `json:"AuthID"`
	AuthProvider   string              `json:"AuthProvider"`
	Model          string              `json:"Model"`
	Format         string              `json:"Format"`
	Stream         bool                `json:"Stream"`
	SourceFormat   string              `json:"SourceFormat"`
	Payload        []byte              `json:"Payload"`
	OriginalRequest []byte             `json:"OriginalRequest"`
	StorageJSON    []byte              `json:"StorageJSON"`
	Headers        map[string][]string `json:"Headers"`
	StreamID       string              `json:"stream_id"`
	HostCallbackID string              `json:"host_callback_id"`
}

func (r execRequest) cred() (mimoCred, error) {
	var c mimoCred
	if len(r.StorageJSON) > 0 {
		if err := json.Unmarshal(r.StorageJSON, &c); err != nil {
			return c, fmt.Errorf("凭证无法解析: %w", err)
		}
	}
	c.normalize()
	if c.ServiceToken == "" {
		return c, fmt.Errorf("凭证缺少 service_token（auth.refresh 可能还没跑过）")
	}
	return c, nil
}

func (r execRequest) body() []byte {
	if len(r.Payload) > 0 {
		return r.Payload
	}
	return r.OriginalRequest
}

// buildUpstream 组装打给 MiMo 的请求。context 单独传，流式场景要用 Background。
func buildUpstream(ctx context.Context, r execRequest, c mimoCred) (*http.Request, error) {
	body := r.body()
	if len(body) == 0 {
		body = []byte("{}")
	}
	// 在 executor 里做注入，而不是只靠 request.normalize：
	// 实测宿主只在「需要协议翻译」的路径上调用 request_normalizer，
	// 客户端本来就是 chat-completions 时不会调（日志里完全没有 normalize 记录）。
	before := len(body)
	body = maybeInjectWebSearch(r.Model, body)
	if len(body) != before {
		dbg("web_search 已注入 (%dB -> %dB)", before, len(body))
	}

	url := config().BaseURL + "/api/route/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "application/json")
	req.Header.Set("Cookie", c.routeCookie())
	if r.Stream {
		req.Header.Set("accept", "text/event-stream")
	}
	return req, nil
}

// maybeInjectWebSearch 复刻 xm2api 的 web search compat 层：请求没自带 tools 时
// 追加 {"type":"web_search"}，让模型自己决定搜不搜。
//
// 幂等 —— 已经有 tools（含调用方自己的函数调用）或已给 web_search 标志就原样返回，
// 所以 request.normalize 和 executor 两条路径同时命中也不会重复注入。
func maybeInjectWebSearch(model string, body []byte) []byte {
	if !config().WebSearchAuto || len(body) == 0 || nonChatName.MatchString(model) {
		return body
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	if raw, ok := obj["tools"]; ok {
		var tools []json.RawMessage
		if json.Unmarshal(raw, &tools) == nil && len(tools) > 0 {
			return body
		}
	}
	if _, ok := obj["web_search"]; ok {
		return body
	}
	obj["tools"] = json.RawMessage(`[{"type":"web_search"}]`)
	patched, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return patched
}

/* --------------------------------------------------------------- 非流式 */

func handleExecute(req []byte) []byte {
	var in execRequest
	if err := json.Unmarshal(req, &in); err != nil {
		return errResult("invalid_request", "executor.execute 请求无法解析: "+err.Error(), 400)
	}
	dbg("execute model=%q format=%q stream=%v payload=%dB storage=%dB authID=%q",
		in.Model, in.Format, in.Stream, len(in.Payload), len(in.StorageJSON), in.AuthID)
	c, err := in.cred()
	if err != nil {
		dbg("execute 凭证不可用: %v", err)
		return errResult("invalid_credential", err.Error(), 401)
	}
	dbg("execute cookie=%s", redactCookie(c.routeCookie()))

	ctx, cancel := context.WithTimeout(context.Background(), upstreamTimeout)
	defer cancel()

	hreq, err := buildUpstream(ctx, in, c)
	if err != nil {
		return errResult("internal_error", err.Error(), 500)
	}
	dbg("execute -> POST %s", hreq.URL.String())
	res, err := (&http.Client{Timeout: upstreamTimeout}).Do(hreq)
	if err != nil {
		dbg("execute 上游请求失败: %v", err)
		return errResult("upstream_unavailable", "上游请求失败: "+err.Error(), 502)
	}
	defer res.Body.Close()
	dbg("execute <- HTTP %d ct=%s", res.StatusCode, res.Header.Get("content-type"))
	body, err := io.ReadAll(io.LimitReader(res.Body, 32<<20))
	if err != nil {
		dbg("execute 读取响应失败: %v", err)
		return errResult("upstream_error", "读取上游响应失败: "+err.Error(), 502)
	}
	dbg("execute 响应 %dB", len(body))

	if res.StatusCode >= 400 {
		return upstreamError(res.StatusCode, body)
	}

	out := http.Header{}
	if ct := res.Header.Get("content-type"); ct != "" {
		out.Set("content-type", ct)
	} else {
		out.Set("content-type", "application/json")
	}
	return okResult(map[string]any{"Payload": body, "Headers": out})
}

/* ----------------------------------------------------------------- 流式 */

/*
 * 真流式的关键约束（见 hostcall.go 的说明）：
 * 宿主的 stream bridge 只有 16 个 chunk 缓冲，且下游要等本方法返回后才开始读，
 * 所以**绝不能**在本方法里同步 emit。做法是：
 *   1) 同步把上游请求发出去、拿到响应头 —— 这样上游 401/429 还能正确映射成 HTTP 状态码；
 *   2) 起 goroutine 异步读 body 并 host.stream.emit；
 *   3) 立刻返回 headers。
 * 宿主会通过 cleanupWhenStreamDone 把回调上下文一直保活到流关闭。
 */
func handleExecuteStream(req []byte) []byte {
	var in execRequest
	if err := json.Unmarshal(req, &in); err != nil {
		return errResult("invalid_request", "executor.execute_stream 请求无法解析: "+err.Error(), 400)
	}
	if in.StreamID == "" {
		return errResult("invalid_request", "executor.execute_stream 缺少 stream_id", 400)
	}
	c, err := in.cred()
	if err != nil {
		return errResult("invalid_credential", err.Error(), 401)
	}

	// 用 Background：宿主 RPC 返回后 ctx 会被取消，不能挂在它上面。
	hreq, err := buildUpstream(context.Background(), in, c)
	if err != nil {
		return errResult("internal_error", err.Error(), 500)
	}
	client := &http.Client{Timeout: upstreamTimeout}
	res, err := client.Do(hreq)
	if err != nil {
		return errResult("upstream_unavailable", "上游请求失败: "+err.Error(), 502)
	}

	// 还没吐任何 chunk，这里返回错误宿主能正确映射成 HTTP 状态码
	if res.StatusCode >= 400 {
		defer res.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		return upstreamError(res.StatusCode, body)
	}

	streamID := in.StreamID
	go pumpStream(streamID, res)

	out := http.Header{}
	if ct := res.Header.Get("content-type"); ct != "" {
		out.Set("content-type", ct)
	} else {
		out.Set("content-type", "text/event-stream")
	}
	out.Set("cache-control", "no-cache")
	out.Set("x-accel-buffering", "no")
	return okResult(map[string]any{"Headers": out})
}

// pumpStream 异步把上游 SSE 拆成裸 JSON payload 推给宿主。
//
// ⚠️ 这里踩了两个坑，都是实测出来的（别照抄「原样透传」的直觉）：
//
//  1. CPA 的插件流式通道要的是**裸 JSON 负载**，不是 SSE 行。
//     它自己会给每个 chunk 补 `data: `。原样转发 `data:{...}` 会让客户端收到
//     `data: data:{...}` 的双前缀。补空格也没用（试过，一样双前缀）。
//  2. 因此必须按行解析：只有完整的一行才知道该不该剥前缀；
//     ReadBytes('\n') 同时也避免了 chunk 边界把 `data:` 劈成两半。
//
// `[DONE]` 丢弃 —— CPA 自己会补终止帧。
func pumpStream(streamID string, res *http.Response) {
	defer res.Body.Close()

	var chunks, emitted int
	defer func() {
		if r := recover(); r != nil {
			hostLog("error", fmt.Sprintf("MiMo 流式推送 panic: %v", r))
			dbg("pumpStream panic stream=%s: %v", streamID, r)
			hostStreamClose(streamID, fmt.Sprintf("plugin panic: %v", r))
			return
		}
		// 退出路径也要留痕：客户端断开时靠这条判断 goroutine 有没有正确收尾
		dbg("pumpStream 退出 stream=%s chunks=%d bytes=%d", streamID, chunks, emitted)
	}()

	reader := bufio.NewReaderSize(res.Body, 32*1024)
	pending := make([]byte, 0, 8*1024)

	flush := func(final bool) bool {
		payload := toPayload(pending)
		pending = pending[:0]
		if len(payload) == 0 {
			return true
		}
		if e := hostStreamEmit(streamID, payload); e != nil {
			// 宿主在 stream abort 后会立刻返回错误（stream_bridge.go 的 s.closed），
			// 这里返回即可让上游 body 被 defer 关掉，不会泄漏连接。
			hostLog("debug", "MiMo 流式推送中断（下游可能已断开）: "+e.Error())
			dbg("pumpStream 推送中断: %v", e)
			return false
		}
		chunks++
		emitted += len(payload)
		return true
	}

	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			pending = append(pending, line...)
			complete := line[len(line)-1] == '\n'
			// 超长行保护：万一上游不发换行，别把内存吃光
			if complete || len(pending) > 1<<20 {
				if !flush(false) {
					return
				}
			}
		}
		if err != nil {
			if len(pending) > 0 {
				flush(true)
			}
			if err != io.EOF {
				hostLog("warn", "MiMo 上游流读取失败: "+err.Error())
				hostStreamClose(streamID, err.Error())
				return
			}
			break
		}
	}
	hostStreamClose(streamID, "")
}

// toPayload 把上游的一行还原成裸 JSON；不是有效负载就返回 nil（丢弃）。
func toPayload(line []byte) []byte {
	t := bytes.TrimSpace(line)
	if len(t) == 0 {
		return nil
	}
	switch {
	case bytes.HasPrefix(t, []byte("data:")):
		t = bytes.TrimSpace(t[len("data:"):])
	case bytes.HasPrefix(t, []byte("event:")),
		bytes.HasPrefix(t, []byte("id:")),
		bytes.HasPrefix(t, []byte("retry:")),
		bytes.HasPrefix(t, []byte(":")):
		// SSE 注释/控制字段直接丢弃
		return nil
	}
	if len(t) == 0 || bytes.Equal(t, []byte("[DONE]")) {
		return nil
	}
	if !json.Valid(t) {
		return nil
	}
	out := make([]byte, len(t))
	copy(out, t)
	return out
}

/* ----------------------------------------------------------- 其它方法 */

func handleCountTokens(req []byte) []byte {
	var in execRequest
	_ = json.Unmarshal(req, &in)
	body := in.body()
	// 粗略估算：按字节数 / 4。对中文会低估、对代码会略高估。
	// 上游没有暴露 tokenizer，这里只保证 /v1/messages/count_tokens 有返回值。
	tokens := (len(body) + 3) / 4
	payload, _ := json.Marshal(map[string]any{"total_tokens": tokens})
	return okResult(map[string]any{
		"Payload": payload,
		"Headers": http.Header{"content-type": []string{"application/json"}},
	})
}

// handleNormalize 服务 request.normalize。
//
// 注意：宿主只在需要协议翻译的路径上调用它（例如 Claude/Gemini 客户端进来时）。
// 原生 chat-completions → chat-completions 不会调，所以 executor 里也做了一次
// 同样的注入（幂等，不会重复）。
func handleNormalize(req []byte) []byte {
	var in struct {
		FromFormat string `json:"FromFormat"`
		ToFormat   string `json:"ToFormat"`
		Model      string `json:"Model"`
		Stream     bool   `json:"Stream"`
		Body       []byte `json:"Body"`
	}
	if err := json.Unmarshal(req, &in); err != nil {
		return errResult("invalid_request", "request.normalize 请求无法解析: "+err.Error(), 400)
	}
	// 非 chat 协议一律不碰
	if (in.ToFormat != "" && in.ToFormat != "chat-completions") ||
		(in.FromFormat != "" && in.FromFormat != "chat-completions") {
		return okResult(map[string]any{"Body": in.Body})
	}
	return okResult(map[string]any{"Body": maybeInjectWebSearch(in.Model, in.Body)})
}

/* ------------------------------------------------------------ 错误映射 */

// upstreamError 把上游 4xx/5xx 映射成 CPA 认得的结构化错误。
// 必须带 http_status，否则 CPA 一律降级成 500。
func upstreamError(status int, body []byte) []byte {
	code := "invalid_request_error"
	switch {
	case status == 401:
		code = "invalid_api_key"
	case status == 403:
		code = "insufficient_quota"
	case status == 404:
		code = "model_not_found"
	case status == 429:
		code = "rate_limit_exceeded"
	case status >= 500:
		code = "internal_server_error"
	}
	msg := extractUpstreamMessage(body)
	if msg == "" {
		msg = fmt.Sprintf("上游返回 HTTP %d", status)
	}
	hostLog("warn", fmt.Sprintf("MiMo 上游 %d: %.300s", status, msg))
	return errResult(code, msg, status)
}

// extractUpstreamMessage 尽量从上游错误体里挖出人话。
func extractUpstreamMessage(body []byte) string {
	var parsed struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		Message     string `json:"message"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil {
		switch {
		case parsed.Error != nil && parsed.Error.Message != "":
			return parsed.Error.Message
		case parsed.Message != "":
			return parsed.Message
		case parsed.Description != "":
			return parsed.Description
		}
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 500 {
		s = s[:500]
	}
	return s
}
