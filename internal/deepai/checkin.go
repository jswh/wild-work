package deepai

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// 签到环（nonce 回传）与额度短路用的**进程内**账号状态。
//
// 为什么放内存而不是落盘：这些都是「这次进程还算不算新」的短周期状态——nonce 的有效期是
// 上游的 check_in.seconds（默认 600s），额度短路的恢复点是最近一次 refresh_at，
// 重启后从下一轮探测重新学习即可（spec §5/§7.2 明确「重启即丢」）。
// 落盘反而会引入「重启后带着过期 nonce 打上游」的风险。

const (
	// checkinMinInterval 两次回传的最小间隔：上游要求 seconds/2，这里再多一道 30s 兜底
	// （秒数异常大时不被连续触发；「宁可慢一点也别多打」）。
	checkinMinInterval = 30 * time.Second
	// checkinRefusedLock 命中 `check_in_refused` 后的静默锁时长（spec §7.2）。
	checkinRefusedLock = 6 * time.Hour
	// checkinDefaultSeconds 上游没下发 seconds 时的节流口径（与参考实现的 600s 一致）。
	checkinDefaultSeconds = 600
)

// acctState 单账号的签到 / 余额 / 转发状态（零值即「什么都没学到」）。
type acctState struct {
	nonce        string    // 上一轮下发、尚未回传的 nonce
	seconds      int       // 上游要求的回传周期
	attemptAt    time.Time // 上次真正带 nonce 打 /theos 的时刻（30s 兜底间隔）
	ackedAt      time.Time // 上次回传成功的时刻（seconds/2 节流）
	refusedUntil time.Time // 静默锁到期（命中 check_in_refused 后）
	lastRelay    time.Time // 最近一次对话转发（beacon 的 pi 字段）
	refreshAt    time.Time // 余额刷新点（额度短路的恢复点）
	remain       int64     // 上一次读到的余额（被拒时回填，避免「待刷新」噪音）
	hasRemain    bool
}

// checkinDue 判断此刻是否该回传 nonce（纯读，调用方持锁）。
//
// 三条与桌面端一致的规则：静默锁未过 ⇒ 不签；回传成功未满 seconds/2 ⇒ 不签；
// 距上次尝试不足 30s ⇒ 不签。额外一条：还没有 nonce（首次探测）⇒ 没有可回传的东西。
func (st *acctState) checkinDue(now time.Time) bool {
	if st == nil || st.nonce == "" {
		return false
	}
	if !st.refusedUntil.IsZero() && now.Before(st.refusedUntil) {
		return false
	}
	if !st.ackedAt.IsZero() {
		ttl := time.Duration(st.seconds) * time.Second
		if ttl <= 0 {
			ttl = checkinDefaultSeconds * time.Second
		}
		if now.Sub(st.ackedAt) < ttl/2 {
			return false
		}
	}
	if !st.attemptAt.IsZero() && now.Sub(st.attemptAt) < checkinMinInterval {
		return false
	}
	return true
}

// beacons 组装 `X-DeepAI-Code-Beacon`：`v=<版本>;up=<进程存活秒>;pi=<0|1>`。
//
// `pi` = 「最近有没有在转发对话」：上游据此判断客户端是不是真的在用（桌面端同形）。
// 窗口取 seconds（默认 600s）——超过一个签到周期没转发就连它也算「闲置」，这是如实的。
// beaconLocked 组装 beacon 头（调用方持 states.mu；c.started 创建后只读，无需再取锁）。
func (c *Client) beaconLocked(st *acctState, now time.Time) string {
	ttl := time.Duration(st.seconds) * time.Second
	if ttl <= 0 {
		ttl = checkinDefaultSeconds * time.Second
	}
	pi := 0
	if !st.lastRelay.IsZero() && now.Sub(st.lastRelay) < ttl {
		pi = 1
	}
	up := int64(now.Sub(c.started).Seconds())
	if c.started.IsZero() || up < 0 {
		up = 0
	}
	return fmt.Sprintf("v=%s;up=%d;pi=%d", AppVersion, up, pi)
}

// stateOf 取（必要时创建）账号状态。
func (c *Client) stateOf(uid string) *acctState {
	c.states.mu.Lock()
	defer c.states.mu.Unlock()
	return c.stateLocked(uid)
}

// stateLocked 同 stateOf，但要求调用方已持 states.mu（避免同一把锁重入死锁）。
func (c *Client) stateLocked(uid string) *acctState {
	if c.states.m == nil { // 零值 Client（测试里的结构体字面量）也能用
		c.states.m = make(map[string]*acctState)
	}
	st := c.states.m[uid]
	if st == nil {
		st = &acctState{}
		c.states.m[uid] = st
	}
	return st
}

// beginCheckin 在发请求**之前**决定这一步要不要回传 nonce，并写回「尝试时刻」。
//
// 返回 (要回传的 nonce, beacon 头值, 是否回传)。注意 `attemptAt` 只在实际回传时推进：
// 首次探测（无 nonce 可回传）不该占用 30s 兜底间隔，否则第一轮之后的首次回传会被无谓推迟。
func (c *Client) beginCheckin(uid string, now time.Time) (string, string, bool) {
	st := c.stateOf(uid)
	c.states.mu.Lock()
	defer c.states.mu.Unlock()
	due := st.checkinDue(now)
	beacon := c.beaconLocked(st, now)
	// 每次探测都算一次「尝试」：30s 兜底间隔防的是「连续两次探测」而不是「连续两次回传」，
	// 否则刚拿到 nonce 的那一轮会被立刻回传（上游看到的就是高频签到）。
	st.attemptAt = now
	if !due {
		return "", beacon, false
	}
	return st.nonce, beacon, true
}

// finishCheckin 请求成功后推进环：记住新 nonce（下一轮回传）与成功时刻。
func (c *Client) finishCheckin(uid string, now time.Time, info theosInfo, acked bool) {
	st := c.stateOf(uid)
	c.states.mu.Lock()
	defer c.states.mu.Unlock()
	if info.CheckIn != nil {
		st.nonce = info.CheckIn.Nonce
		if info.CheckIn.Seconds > 0 {
			st.seconds = info.CheckIn.Seconds
		}
	}
	if acked && info.CheckIn != nil {
		st.ackedAt = now
	}
	if ra, err := time.Parse(time.RFC3339, strings.TrimSpace(info.RefreshAt)); err == nil {
		st.refreshAt = ra
	}
	if v, err := balanceInt(info.Balance); err == nil {
		st.remain, st.hasRemain = v, true
	}
	// **不**在这里清 refusedUntil：被拒的那一轮之后，下一轮「不带 nonce 的余额探测」照样
	// 能拿到 200（被拒的只是回传动作），若在这里解锁，6h 静默锁会被每 10 分钟的刷新
	// 立刻抹掉 ⇒ 又变成每轮都往上游撞一次 check_in_refused。锁只按时间自然到期。
}

// lockCheckin 打静默锁（命中 `check_in_refused`）。
func (c *Client) lockCheckin(uid string, now time.Time, d time.Duration) {
	st := c.stateOf(uid)
	c.states.mu.Lock()
	defer c.states.mu.Unlock()
	st.refusedUntil = now.Add(d)
}

// refusedUntil 返回静默锁到期时刻（零值 = 未锁；测试与诊断用）。
func (c *Client) refusedUntil(uid string) time.Time {
	st := c.stateOf(uid)
	c.states.mu.Lock()
	defer c.states.mu.Unlock()
	return st.refusedUntil
}

// lastTheosInfo 用内存里的上一份余额拼一个响应（签到被拒时的兜底）。
func (c *Client) lastTheosInfo(uid string) (theosInfo, error) {
	st := c.stateOf(uid)
	c.states.mu.Lock()
	defer c.states.mu.Unlock()
	if !st.hasRemain {
		// 从没读到过余额就被拒：没有可回填的东西，如实报错让面板显示「待刷新」。
		return theosInfo{}, errCreditsUnreadable
	}
	return theosInfo{Enabled: true, Balance: fmt.Sprintf("%d", st.remain)}, nil
}

// markRelay 记录一次对话转发（beacon 的 `pi` 字段据此判断「最近有没有在干活」）。
func (c *Client) markRelay(uid string) {
	if uid == "" {
		return
	}
	st := c.stateOf(uid)
	c.states.mu.Lock()
	defer c.states.mu.Unlock()
	st.lastRelay = time.Now()
}

// isCheckInRefused 判断 403 体是不是签到被拒（与其它 403 区分：那些要罚号）。
func isCheckInRefused(body []byte) bool {
	return strings.Contains(strings.ToLower(string(body)), "check_in_refused")
}

// states 是 Client 上的账号状态表（惰性初始化，零值可用）。
type states struct {
	mu sync.Mutex
	m  map[string]*acctState
}

// isTheosExhausted 判断 403 体是不是「计量模型额度耗尽」（theos_exhausted / network_bound）。
//
// `network_bound` 一并算：它的语义是「这枚 Key 绑定的出口机额度也耗尽了」，恢复点同样是
// `refresh_at`（与检查「本地出口能否复用」无关），所以短路口径同一条。
func isTheosExhausted(body []byte) bool {
	s := strings.ToLower(string(body))
	return strings.Contains(s, "theos_exhausted") || strings.Contains(s, "network_bound")
}
