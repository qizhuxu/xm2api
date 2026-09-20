package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

/*
 * 宿主回调封装。
 *
 * 踩过的坑：CPA 内部对 JSON 字段的命名不统一 —— pluginapi 里的结构体多数没有
 * json tag（于是编出 StatusCode），而宿主自有的 RPC 结构体用 snake_case
 * （status_code）。Go 的 encoding/json 反序列化虽然大小写不敏感，但**不忽略下划线**，
 * 所以 "status_code" 匹配不上 StatusCode。
 * 这里统一把 key 归一化成「小写 + 去下划线」再查，两种写法都能吃。
 */

func decodeLoose(raw []byte) map[string]json.RawMessage {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	out := make(map[string]json.RawMessage, len(m))
	for k, v := range m {
		out[strings.ToLower(strings.ReplaceAll(k, "_", ""))] = v
	}
	return out
}

func fieldStr(m map[string]json.RawMessage, key string) string {
	if v, ok := m[key]; ok {
		var s string
		if json.Unmarshal(v, &s) == nil {
			return s
		}
	}
	return ""
}

func fieldInt(m map[string]json.RawMessage, key string) int {
	if v, ok := m[key]; ok {
		var n int
		if json.Unmarshal(v, &n) == nil {
			return n
		}
	}
	return 0
}

func fieldBool(m map[string]json.RawMessage, key string) bool {
	if v, ok := m[key]; ok {
		var b bool
		if json.Unmarshal(v, &b) == nil {
			return b
		}
	}
	return false
}

// fieldBytes 处理 []byte 字段：Go 的 encoding/json 用 base64 表示字节切片。
func fieldBytes(m map[string]json.RawMessage, key string) []byte {
	if v, ok := m[key]; ok {
		var b []byte
		if json.Unmarshal(v, &b) == nil {
			return b
		}
	}
	return nil
}

func fieldHeader(m map[string]json.RawMessage, key string) http.Header {
	if v, ok := m[key]; ok {
		var h http.Header
		if json.Unmarshal(v, &h) == nil {
			return h
		}
	}
	return http.Header{}
}

func hostLog(level, msg string) {
	_, _ = hostCall(methodHostLog, map[string]any{"level": level, "message": msg})
}

/* --------------------------------------------------- 下游流式推送（关键） */

/*
 * host.stream.emit 是执行器插件把 chunk 推给宿主的唯一途径。
 *
 * ⚠️ 硬约束：宿主的 stream bridge 只有 16 个 chunk 的队列（streamBridgeBufferSize = 16），
 * 而下游消费者要等 executor.execute_stream **返回之后**才开始读。
 * 所以同步 emit 超过 16 个 chunk 必然死锁 —— 必须起 goroutine 异步推。
 * 见 internal/pluginhost/stream_bridge.go 与 rpc_client_stream.go。
 */

func hostStreamEmit(streamID string, payload []byte) error {
	if streamID == "" {
		return errors.New("缺少 stream_id")
	}
	_, err := hostCall(methodHostStreamEmit, map[string]any{
		"stream_id": streamID,
		"payload":   payload,
	})
	return err
}

func hostStreamClose(streamID, errMsg string) {
	if streamID == "" {
		return
	}
	payload := map[string]any{"stream_id": streamID}
	if errMsg != "" {
		payload["error"] = errMsg
	}
	if _, err := hostCall(methodHostStreamClose, payload); err != nil {
		hostLog("warn", fmt.Sprintf("host.stream.close 失败: %v", err))
	}
}
