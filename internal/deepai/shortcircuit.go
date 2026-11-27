package deepai

import (
	"log"
	"sync"
	"time"

	"wild-work/internal/auth"
)

// 计量模型额度耗尽短路（spec §5）。
//
// 上游 403 `theos_exhausted` / `network_bound` 的语义是「**这一款计量模型**的计费额度用光
// 了」，恢复点是 `/theos` 的 `refresh_at`。若不本地记住，被挑中的号每轮都要打一次上游、
// 拿一次 403、再换号——既浪费往返又提高风控命中面。
//
// **只对计量模型记**：免费模型（`theosPerRequest == 0`）永远不会回这个错误，所以「收到即记」
// 天然只覆盖计量模型，不需要另一份「哪些模型要钱」的判据（那种判据会漂移）。
//
// **不放进共享层的重置点解析**：`provider` 的 softRate 重置点（`softRateResetTs`）是
// 跨渠道的全局口径，改它会影响所有渠道；本表是渠道私有、进程内、重启即丢。

// exhausts 是「(账号, 模型) → 恢复时刻」的进程内表。
type exhausts struct {
	mu sync.Mutex
	m  map[string]time.Time
}

// exhaustKey 短路键：账号 + 上游模型 id（同一个模型在不同号上额度独立，必须分开记）。
func exhaustKey(uid, model string) string {
	if uid == "" || model == "" {
		return ""
	}
	return uid + "\x00" + model
}

// exhaustedUntil 查该 (账号, 模型) 是否还在耗尽窗口内。
func (c *Client) exhaustedUntil(uid, model string) (time.Time, bool) {
	key := exhaustKey(uid, model)
	if key == "" {
		return time.Time{}, false
	}
	c.exhaust.mu.Lock()
	defer c.exhaust.mu.Unlock()
	until, ok := c.exhaust.m[key]
	if !ok {
		return time.Time{}, false
	}
	if !time.Now().Before(until) {
		delete(c.exhaust.m, key) // 过期即清：恢复点之后要重新向上游求证
		return time.Time{}, false
	}
	return until, true
}

// noteExhausted 记住「该号该模型在 refreshAt 之前不用再打了」。
//
// refreshAt 未知（还没成功探测过 `/theos`）时**跳过记录**：拿「现在 + 固定时长」去猜
// 恢复点会在真实刷新点之后继续短路，把可用额度白白藏起来。
func (c *Client) noteExhausted(uid, model string, until time.Time) bool {
	key := exhaustKey(uid, model)
	if key == "" || until.IsZero() || !until.After(time.Now()) {
		return false
	}
	c.exhaust.mu.Lock()
	defer c.exhaust.mu.Unlock()
	c.exhaust.m[key] = until
	log.Printf("deepai theos exhausted uid=%s model=%s until=%s（本模型本地短路至恢复点）",
		uid, model, until.Format(time.RFC3339))
	return true
}

// shortCircuit 在发请求**之前**判短路；命中则返回合成的 403 体（不碰上游）。
//
// 返回合成体而不是 error 的原因：handler 只有拿到 status+body 才会走 `Classify`（⇒ 本渠道
// 的 `ErrSoftRate` ⇒ 模型级冷却 + 同请求内换号）。返回 error 会被当成传输层故障，
// 既不换号也不冷却（网络问题与账号健康无关那条口径），等于把这个信号吞掉。
func (c *Client) shortCircuit(a *auth.Auth, model string) ([]byte, bool) {
	uid := uidOf(a)
	until, ok := c.exhaustedUntil(uid, model)
	if !ok {
		return nil, false
	}
	body := []byte(`{"error":{"code":"theos_exhausted","message":"该模型的计费额度已耗尽，预计 ` +
		until.Format(time.RFC3339) + ` 恢复（免费模型不受影响）"}}`)
	return body, true
}
