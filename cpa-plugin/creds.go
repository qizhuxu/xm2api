package main

import (
	"sync"
	"time"
)

/*
 * 凭证运行时状态。
 *
 * ## 为什么要这么设计（都是实测结论，不是推断）
 *
 * 1. **serviceToken 是会话 cookie**：SSO 阶段 2 的 Set-Cookie 里既没有
 *    Expires 也没有 Max-Age，所以没有任何服务端声明的有效期。
 *    定时刷新（refresh_after）本质上是在**猜**。
 *
 * 2. **失效时上游返回 HTTP 401 + 空 body**：拿坏 token / 空 token / 完全不带
 *    cookie 三种情况都探过，一律 401，body 为空字符串。
 *
 * 结论：以**事件驱动**为主 —— 上游 401 就地重换 token 并重试一次；
 * 定时刷新降级为兜底。既不依赖猜测的 TTL，也不会在 token 还有效时白换。
 *
 * token 缓存在这里、而不是只依赖 auth 文件的原因：executor 每次拿到的
 * StorageJSON 是宿主那份。反应式续期后若只换文件不动缓存，下一个请求又会
 * 拿着旧 token 去撞 401，于是每个请求都要多跑一次 SSO 交换。
 */

type credState struct {
	AuthID      string
	UserID      string
	CanRenew    bool      // 有 pass_token 才能续期
	HasToken    bool      // 当前是否持有 service_token
	TokenLen    int
	ObtainedAt  time.Time // 最近一次成功换取 serviceToken 的时间
	LastTryAt   time.Time // 最近一次尝试续期的时间
	LastError   string    // 最近一次失败原因（空 = 正常）
	RefreshOK   int       // 成功续期次数
	RefreshFail int       // 失败次数
	Reactive    int       // 其中由上游 401 触发的次数
	LastIssued  string    // 这次 token 是怎么来的

	tok  string    // 绝不出结构体、绝不进日志、绝不进管理页
	full mimoCred // 完整凭证（含 pass_token）：quota.fetch 等宿主不带 StorageJSON
	// 的调用要用。未导出字段，encoding/json 不会序列化它，管理页/状态页拿不到。
}

var credStore = struct {
	sync.Mutex
	m map[string]*credState
}{m: map[string]*credState{}}

// updateCred 在锁内改状态。
//
// ⚠️ fn 里**不要**调用 config() / hostCall() 这类会再取锁的东西 ——
// sync.Mutex 不可重入，这个坑在 models.go 里已经踩过一次（整机死锁）。
func updateCred(authID string, fn func(*credState)) {
	if authID == "" {
		return
	}
	credStore.Lock()
	defer credStore.Unlock()
	s := credStore.m[authID]
	if s == nil {
		s = &credState{AuthID: authID}
		credStore.m[authID] = s
	}
	fn(s)
}

// noteParsed 记录 auth.parse / 定时 auth.refresh 的结果。
func noteParsed(authID, userID, token string, canRenew bool, issued string) {
	updateCred(authID, func(s *credState) {
		s.UserID = userID
		s.CanRenew = canRenew
		s.tok = token
		s.HasToken = token != ""
		s.TokenLen = len(token)
		s.ObtainedAt = time.Now()
		s.LastIssued = issued
		if token != "" {
			s.LastError = ""
		}
	})
}

// noteReactive 记录一次由上游 401 触发的续期。
func noteReactive(authID, token string, err error) {
	updateCred(authID, func(s *credState) {
		s.LastTryAt = time.Now()
		s.Reactive++
		if err != nil {
			s.RefreshFail++
			s.LastError = err.Error()
			return
		}
		s.RefreshOK++
		s.tok = token
		s.HasToken = token != ""
		s.TokenLen = len(token)
		s.ObtainedAt = time.Now()
		s.LastIssued = "反应式续期（上游 401 触发）"
		s.LastError = ""
	})
}

func noteRefreshFailed(authID string, err error) {
	updateCred(authID, func(s *credState) {
		s.LastTryAt = time.Now()
		s.RefreshFail++
		if err != nil {
			s.LastError = err.Error()
		}
	})
}

// cachedToken 取出插件已知的最新 token。
// StorageJSON 里带的是宿主那份，可能是旧的；这里优先用插件自己维护的。
func cachedToken(authID string) string {
	credStore.Lock()
	defer credStore.Unlock()
	if s := credStore.m[authID]; s != nil {
		return s.tok
	}
	return ""
}

/* ------------------------------------------------------------ 模型目录状态 */

type modelState struct {
	Source string
	Count  int
	At     time.Time
	Err    string
}

var modelStore = struct {
	sync.Mutex
	v modelState
}{}

func recordModels(source string, count int, err error) {
	modelStore.Lock()
	defer modelStore.Unlock()
	modelStore.v = modelState{Source: source, Count: count, At: time.Now()}
	if err != nil {
		modelStore.v.Err = err.Error()
	}
}

func modelsSnapshot() modelState {
	modelStore.Lock()
	defer modelStore.Unlock()
	return modelStore.v
}

/* ---------------------------------------------------------------- 快照 */

// credSnapshot 返回拷贝（不含 token），调用方拿走后不持锁。
func credSnapshot() []credState {
	credStore.Lock()
	defer credStore.Unlock()
	out := make([]credState, 0, len(credStore.m))
	for _, s := range credStore.m {
		c := *s
		c.tok = ""
		c.full = mimoCred{} // 全字段清空，双保险（本就不可序列化）
		out = append(out, c)
	}
	return out
}

/* ------------------------------------------------------- 完整凭证缓存 */

// rememberCred 缓存完整凭证（含 pass_token）。
// 调用点：auth.parse / auth.refresh / 反应式续期 / 扫码登录。
func rememberCred(authID string, c mimoCred) {
	if authID == "" {
		return
	}
	updateCred(authID, func(s *credState) { s.full = c })
}

// credForLookup 按 AuthID 取插件已知凭证，token 优先用缓存里最新的。
// 宿主的 QuotaFetchRequest 只带 AuthID/Metadata，不带 StorageJSON，
// quota.fetch 只能靠这条路径拿凭证。
func credForLookup(authID string) mimoCred {
	credStore.Lock()
	var out mimoCred
	if s := credStore.m[authID]; s != nil {
		out = s.full
		if s.tok != "" {
			out.ServiceToken = s.tok
		}
	}
	// 只有一条凭证时允许按 provider 兜底（宿主给的 AuthID 对不上号也不至于失败）
	if out.ServiceToken == "" && out.PassToken == "" && len(credStore.m) == 1 {
		for _, s := range credStore.m {
			out = s.full
			if s.tok != "" {
				out.ServiceToken = s.tok
			}
		}
	}
	credStore.Unlock()
	out.normalize()
	return out
}

/* ------------------------------------------------------------ 用量快照 */

// usageData 是上游 GET /api/user/usage 的数据部分。
//
// percent 语义实测确认为「剩余百分比」：两轮探测之间随请求消耗递减
// （84.5 → 84.4）。消耗只会让已用量增加，观察到递减 ⇒ 它是剩余量。
type usageData struct {
	Percent   float64 `json:"percent"`
	ResetDate string  `json:"resetDate"`
	ResetAt   int64   `json:"resetAt"`
}

type usageState struct {
	AuthID     string
	Percent    float64
	ResetDate  string
	ResetAt    int64
	ObservedAt time.Time
	Err        string
}

var usageStore = struct {
	sync.Mutex
	m map[string]usageState
}{m: map[string]usageState{}}

func noteUsage(authID string, u *usageData, err error) {
	if authID == "" {
		return
	}
	usageStore.Lock()
	defer usageStore.Unlock()
	s := usageState{AuthID: authID, ObservedAt: time.Now()}
	switch {
	case err != nil:
		s.Err = err.Error()
	case u != nil:
		s.Percent = u.Percent
		s.ResetDate = u.ResetDate
		s.ResetAt = u.ResetAt
	}
	usageStore.m[authID] = s
}

// usageSnapshotFor 返回可进 auth JSON / 状态页的用量快照；无记录返回 nil。
// 不含任何 token，可安全写进文件与管理页。
func usageSnapshotFor(authID string) map[string]any {
	usageStore.Lock()
	defer usageStore.Unlock()
	s, ok := usageStore.m[authID]
	if !ok {
		return nil
	}
	snap := map[string]any{
		"observed_at": s.ObservedAt.Format(time.RFC3339),
		"source":      "GET /api/user/usage",
	}
	if s.Err != "" {
		snap["error"] = s.Err
		return snap
	}
	snap["remaining_percent"] = s.Percent
	if s.ResetDate != "" {
		snap["reset_date"] = s.ResetDate
	}
	if s.ResetAt > 0 {
		snap["reset_at_unix"] = s.ResetAt
	}
	return snap
}

// usageSnapshotsAll 状态页用：全部凭证的用量快照。
func usageSnapshotsAll() []map[string]any {
	credStore.Lock()
	ids := make([]string, 0, len(credStore.m))
	for id := range credStore.m {
		ids = append(ids, id)
	}
	credStore.Unlock()
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		if snap := usageSnapshotFor(id); snap != nil {
			snap["auth_id"] = id
			out = append(out, snap)
		}
	}
	return out
}
