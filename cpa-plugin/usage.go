package main

// usage.go —— 用量历史与请求量统计（独立面板的数据层，v0.2.0+）
//
// 存储：os.TempDir()/mimo-cpa-plugin/usage-*.json —— 不放 CPA 的 auth 目录
// （宿主会把 auth 目录下 .json 当凭证文件扫描，models.go 的磁盘缓存同款教训）。
//   - usage-history.json：每凭证额度快照序列（10 分钟节流，每账号 500 点环形淘汰）
//   - usage-daily.json：按日聚合的成功/失败请求数（保留 60 天）
// 任何失败只记 debug，绝不影响凭证/转发链路。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type usagePoint struct {
	At        string  `json:"at"`
	AuthID    string  `json:"auth_id"`
	Percent   float64 `json:"percent"`
	ResetDate string  `json:"reset_date,omitempty"`
}

type usageDay struct {
	Date    string `json:"date"`
	Success int    `json:"success"`
	Failed  int    `json:"failed"`
}

var usageHist = struct {
	sync.Mutex
	points []usagePoint
	last   map[string]time.Time // authID -> 上次记录时间（节流）
}{last: map[string]time.Time{}}

var usageDays = struct {
	sync.Mutex
	days []usageDay
}{}

const (
	usageHistFile   = "usage-history.json"
	usageDaysFile   = "usage-daily.json"
	usageHistCap    = 500 // 每凭证保留点数
	usageDaysCap    = 60  // 保留天数
	usageThrottleMs = 10 * time.Minute
)

func usageDataDir() string {
	return filepath.Join(os.TempDir(), "mimo-cpa-plugin")
}

// noteUsagePoint 记录一个额度快照（10 分钟节流：额度轮询很频繁，别把历史打爆）。
func noteUsagePoint(authID string, u *usageData) {
	if u == nil || authID == "" {
		return
	}
	usageHist.Lock()
	defer usageHist.Unlock()
	if t, ok := usageHist.last[authID]; ok && time.Since(t) < usageThrottleMs {
		return
	}
	usageHist.last[authID] = time.Now()
	usageHist.points = append(usageHist.points, usagePoint{
		At:        time.Now().Format(time.RFC3339),
		AuthID:    authID,
		Percent:   u.Percent,
		ResetDate: u.ResetDate,
	})
	// 每账号环形淘汰
	if n := countPoints(usageHist.points, authID); n > usageHistCap {
		usageHist.points = trimPoints(usageHist.points, authID, usageHistCap)
	}
	saveUsageJSON(usageHistFile, usageHist.points)
}

func countPoints(pts []usagePoint, authID string) int {
	n := 0
	for _, p := range pts {
		if p.AuthID == authID {
			n++
		}
	}
	return n
}

func trimPoints(pts []usagePoint, authID string, keep int) []usagePoint {
	var mine []int
	for i, p := range pts {
		if p.AuthID == authID {
			mine = append(mine, i)
		}
	}
	if len(mine) <= keep {
		return pts
	}
	drop := map[int]bool{}
	for _, i := range mine[:len(mine)-keep] {
		drop[i] = true
	}
	out := pts[:0]
	for i, p := range pts {
		if !drop[i] {
			out = append(out, p)
		}
	}
	return out
}

// usageHistory 全部历史点（旧→新）。
func usageHistory() []usagePoint {
	usageHist.Lock()
	defer usageHist.Unlock()
	if usageHist.points == nil {
		usageHist.points = loadUsageJSON(usageHistFile, usageHist.points)
	}
	out := make([]usagePoint, len(usageHist.points))
	copy(out, usageHist.points)
	return out
}

// noteRequest 记一次转发请求结果（按日聚合）。
func noteRequest(ok bool) {
	usageDays.Lock()
	defer usageDays.Unlock()
	today := time.Now().Format("2006-01-02")
	if len(usageDays.days) == 0 {
		usageDays.days = loadUsageJSON(usageDaysFile, usageDays.days)
	}
	if n := len(usageDays.days); n == 0 || usageDays.days[n-1].Date != today {
		usageDays.days = append(usageDays.days, usageDay{Date: today})
		if len(usageDays.days) > usageDaysCap {
			usageDays.days = usageDays.days[len(usageDays.days)-usageDaysCap:]
		}
	}
	if ok {
		usageDays.days[len(usageDays.days)-1].Success++
	} else {
		usageDays.days[len(usageDays.days)-1].Failed++
	}
	saveUsageJSON(usageDaysFile, usageDays.days)
}

// usageDaily 近 N 天（不足的日子补零），旧→新。
func usageDaily(days int) []usageDay {
	usageDays.Lock()
	defer usageDays.Unlock()
	if usageDays.days == nil {
		usageDays.days = loadUsageJSON(usageDaysFile, usageDays.days)
	}
	if days <= 0 {
		days = 30
	}
	byDate := map[string]usageDay{}
	for _, d := range usageDays.days {
		byDate[d.Date] = d
	}
	out := make([]usageDay, 0, days)
	for i := days - 1; i >= 0; i-- {
		date := time.Now().AddDate(0, 0, -i).Format("2006-01-02")
		d := byDate[date]
		d.Date = date
		out = append(out, d)
	}
	return out
}

// execCounted 按执行结果信封（{ok:...}）记一次请求量后原样返回。
// 挂在 executor.execute / executor.execute_stream 的分发处：图像执行在
// handleExecute 内部分流，同样被覆盖。
func execCounted(res []byte) []byte {
	var env struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(res, &env); err == nil {
		noteRequest(env.OK)
	}
	return res
}

func saveUsageJSON(name string, v any) {
	dir := usageDataDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		dbg("用量历史目录创建失败: %v", err)
		return
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return
	}
	if err := os.WriteFile(filepath.Join(dir, name), raw, 0o600); err != nil {
		dbg("用量历史写入失败: %v", err)
	}
}

func loadUsageJSON[T any](name string, into []T) []T {
	raw, err := os.ReadFile(filepath.Join(usageDataDir(), name))
	if err != nil {
		return into
	}
	var out []T
	if err := json.Unmarshal(raw, &out); err != nil {
		return into
	}
	return out
}
