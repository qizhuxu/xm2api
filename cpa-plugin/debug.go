package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

/*
 * 插件内部调试日志。
 *
 * 刻意写文件而不是走 host.log：排查「宿主回调本身是否死锁」时，
 * 用宿主日志会把观测手段和被观测对象耦合在一起。
 * 设 MIMO_PLUGIN_DEBUG=1 启动 CPA 即可开启。
 */

var (
	debugOn   = os.Getenv("MIMO_PLUGIN_DEBUG") == "1"
	debugFile = filepath.Join(os.TempDir(), "mimo-plugin.log")
)

func dbg(format string, args ...any) {
	if !debugOn {
		return
	}
	f, err := os.OpenFile(debugFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", time.Now().Format("15:04:05.000"), fmt.Sprintf(format, args...))
}

// trace 记录方法进入/退出与耗时，用来定位卡在哪一步。
func trace(method string) func() {
	start := time.Now()
	dbg("-> %s", method)
	return func() { dbg("<- %s (%s)", method, time.Since(start)) }
}

// redactCookie 只输出 cookie 的键名和长度 —— 绝不能把 serviceToken 写进日志。
func redactCookie(c string) string {
	if strings.TrimSpace(c) == "" {
		return "(empty)"
	}
	names := make([]string, 0, 2)
	for _, part := range strings.Split(c, ";") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) == 2 {
			names = append(names, fmt.Sprintf("%s=<%d chars>", kv[0], len(kv[1])))
		}
	}
	return strings.Join(names, " ") + fmt.Sprintf(" total=%dB", len(c))
}
