package deepai

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"wild-work/internal/provider"
)

// theosServer 假 `/theos`：按调用序号返回预设响应，并记录每次请求看到的 nonce。
type theosResponder struct {
	mu      sync.Mutex
	bodies  []string
	nonces  []string
	beacons []string
	handles []func(w http.ResponseWriter)
	idx     int
}

func (tr *theosResponder) next(w http.ResponseWriter, r *http.Request) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.nonces = append(tr.nonces, r.Header.Get(HeaderCheckIn))
	tr.beacons = append(tr.beacons, r.Header.Get(HeaderBeacon))
	h := tr.handles[min(tr.idx, len(tr.handles)-1)]
	tr.idx++
	h(w)
}

func theosJSON(balance, refreshAt, nonce string) string {
	ci := "null"
	if nonce != "" {
		ci = `{"nonce":"` + nonce + `","seconds":600}`
	}
	return `{"enabled":true,"balance":"` + balance + `","granted":"200","refresh_at":"` +
		refreshAt + `","lane_version":8,"check_in":` + ci + `}`
}

func statusOK(body string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) { _, _ = io.WriteString(w, body) }
}

func status(code int, body string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}
}

func newTheosServer(t *testing.T, resp *theosResponder) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(resp.next))
	t.Cleanup(srv.Close)
	c := New()
	c.Origin = srv.URL
	return c
}

// TestTheosParsesBalanceAndRefreshAt 余额字符串浮点四舍五入、`refresh_at` 记录为恢复点。
func TestTheosParsesBalanceAndRefreshAt(t *testing.T) {
	resp := &theosResponder{handles: []func(http.ResponseWriter){
		statusOK(theosJSON("200.40", "2026-11-27T04:03:00+00:00", "n1")),
	}}
	c := newTheosServer(t, resp)
	remain, err := c.UserResource(testAuth("dai-a", "k", ""))
	if err != nil {
		t.Fatalf("UserResource: %v", err)
	}
	if remain != 200 {
		t.Fatalf("remain=%d，期望 200（200.40 四舍五入）", remain)
	}
	if got := c.stateOf("dai-a").refreshAt.UTC().Format(time.RFC3339); got != "2026-11-27T04:03:00Z" {
		t.Fatalf("refreshAt=%s，期望被记录成恢复点", got)
	}
	// 明细口径：Total=Remain、Used=0（不编造已用）、到期留空（未实证不写）。
	items := []struct{ name string }{}
	resp.handles = append(resp.handles, statusOK(theosJSON("-7.58", "2026-11-27T04:03:00+00:00", "n2")))
	_, det, err := c.UserResourceDetail(testAuth("dai-a", "k", ""))
	if err != nil {
		t.Fatalf("UserResourceDetail: %v", err)
	}
	if len(det) != 1 || det[0].Name != "Theos" {
		t.Fatalf("明细条目=%+v，期望单条 Theos", det)
	}
	if det[0].Total != det[0].Remain || det[0].Used != 0 {
		t.Fatalf("口径应为 Total=Remain、Used=0：%+v", det[0])
	}
	if det[0].Remain != -8 {
		t.Fatalf("-7.58 应为 -8（四舍五入，可为负）：%d", det[0].Remain)
	}
	if det[0].ExpireAt != "" {
		t.Fatalf("到期列必须留空（未实证前不写）：%q", det[0].ExpireAt)
	}
	if det[0].Raw != "-7.58" {
		t.Fatalf("原始串未透出（面板 tooltip 要用）：%q", det[0].Raw)
	}
	if !det[0].Usable {
		t.Fatal("余额条目必须是可消耗池（Usable=true）")
	}
	_ = items
}

// TestTheosBalanceUnreadable 余额读不出来 ⇒ 报错（上层保留旧快照标「待刷新」，不得写 0）。
func TestTheosBalanceUnreadable(t *testing.T) {
	resp := &theosResponder{handles: []func(http.ResponseWriter){statusOK(`{"enabled":true,"balance":"n/a"}`)}}
	c := newTheosServer(t, resp)
	if _, err := c.UserResource(testAuth("dai-a", "k", "")); err == nil {
		t.Fatal("非法余额必须报错（写 0 会被读成额度用光）")
	}
}

// TestTheosUpstream5xxSurfacesError 5xx ⇒ 返回错误（不吞成 0）。
func TestTheosUpstream5xxSurfacesError(t *testing.T) {
	resp := &theosResponder{handles: []func(http.ResponseWriter){status(503, `upstream down`)}}
	c := newTheosServer(t, resp)
	if _, err := c.UserResource(testAuth("dai-a", "k", "")); err == nil {
		t.Fatal("5xx 必须报错")
	}
}

// TestCheckinNonceRing 首次探测只领 nonce；第二轮（半衰期已过）回传上一枚；静默锁期间不回传。
func TestCheckinNonceRing(t *testing.T) {
	resp := &theosResponder{handles: []func(http.ResponseWriter){
		statusOK(theosJSON("100", "2026-11-27T04:03:00+00:00", "nonce-1")),
		statusOK(theosJSON("100", "2026-11-27T04:03:00+00:00", "nonce-2")),
		statusOK(theosJSON("100", "2026-11-27T04:03:00+00:00", "nonce-3")),
	}}
	c := newTheosServer(t, resp)

	if _, err := c.UserResource(testAuth("dai-a", "k", "")); err != nil {
		t.Fatalf("首次探测: %v", err)
	}
	if resp.nonces[0] != "" {
		t.Fatalf("首次探测没有可回传的 nonce，却发了 %q", resp.nonces[0])
	}
	// 未到半衰期（seconds/2 = 300s）⇒ 不回传。
	if _, err := c.UserResource(testAuth("dai-a", "k", "")); err != nil {
		t.Fatalf("第二次探测: %v", err)
	}
	if resp.nonces[1] != "" {
		t.Fatalf("半衰期内不该回传，实际发了 %q", resp.nonces[1])
	}
	// 把上次成功时刻往后拨 400s（> 300s 半衰期、> 30s 兜底）⇒ 应回传 nonce-1。
	c.states.mu.Lock()
	st := c.stateLocked("dai-a")
	st.ackedAt = time.Now().Add(-400 * time.Second)
	st.attemptAt = time.Now().Add(-400 * time.Second)
	c.states.mu.Unlock()
	if _, err := c.UserResource(testAuth("dai-a", "k", "")); err != nil {
		t.Fatalf("第三次探测: %v", err)
	}
	// 每次探测上游都会换发新 nonce（第二次探测回了 nonce-2），回传的必须是**最新**那枚——
	// 回传旧 nonce 上游不认（它是「上一轮的那一枚」）。
	if resp.nonces[2] != "nonce-2" {
		t.Fatalf("到点应回传最新 nonce-2，实际 %q", resp.nonces[2])
	}
	// beacon 形态：v=<版本>;up=<秒>;pi=<0|1>。
	if b := resp.beacons[2]; !strings.HasPrefix(b, "v="+AppVersion+";up=") || !strings.Contains(b, ";pi=") {
		t.Fatalf("beacon 形态不对：%q", b)
	}
}

// TestCheckinRefusedLocksWithoutError 403 check_in_refused ⇒ 6h 静默锁、不报错、余额回填上次值。
func TestCheckinRefusedLocksWithoutError(t *testing.T) {
	resp := &theosResponder{handles: []func(http.ResponseWriter){
		statusOK(theosJSON("123.00", "2026-11-27T04:03:00+00:00", "n1")),
		status(403, `{"error":{"code":"check_in_refused"}}`),
		statusOK(theosJSON("111.00", "2026-11-27T04:03:00+00:00", "n3")),
	}}
	c := newTheosServer(t, resp)
	a := testAuth("dai-a", "k", "")

	if got, err := c.UserResource(a); err != nil || got != 123 {
		t.Fatalf("首探:%d,%v", got, err)
	}
	logs := captureLog(t, func() {
		got, err := c.UserResource(a)
		if err != nil {
			t.Fatalf("check_in_refused 不得报错（会误标待刷新）：%v", err)
		}
		if got != 123 {
			t.Fatalf("被拒时应回填上次余额 123，实际 %d", got)
		}
	})
	if !strings.Contains(logs, "deepai checkin refused uid=dai-a") {
		t.Fatalf("被拒未留日志：\n%s", logs)
	}
	if until := c.refusedUntil("dai-a"); until.Before(time.Now().Add(5 * time.Hour)) {
		t.Fatalf("静默锁时长不足 6h：%s", until)
	}
	// 锁期内即使到了回传时点也不该再带 nonce（宁可慢一点也别多打）。
	c.states.mu.Lock()
	st := c.stateLocked("dai-a")
	st.ackedAt = time.Now().Add(-10 * time.Minute)
	st.attemptAt = time.Now().Add(-10 * time.Minute)
	c.states.mu.Unlock()
	if _, err := c.UserResource(a); err != nil {
		t.Fatalf("锁期探测: %v", err)
	}
	if resp.nonces[2] != "" {
		t.Fatalf("静默锁内仍回传了 nonce %q", resp.nonces[2])
	}
}

// TestCheckinLockNotClearedBySuccessfulProbe 锁期内「不带 nonce 的余额探测」成功，
// **不得**把 6h 静默锁提前抹掉（否则每 10 分钟的刷新都会再撞一次 check_in_refused）。
func TestCheckinLockNotClearedBySuccessfulProbe(t *testing.T) {
	resp := &theosResponder{handles: []func(http.ResponseWriter){
		statusOK(theosJSON("50", "2026-11-27T04:03:00+00:00", "n1")),
		statusOK(theosJSON("50", "2026-11-27T04:03:00+00:00", "n2")),
	}}
	c := newTheosServer(t, resp)
	a := testAuth("dai-a", "k", "")
	if _, err := c.UserResource(a); err != nil {
		t.Fatalf("首探: %v", err)
	}
	lockUntil := time.Now().Add(checkinRefusedLock)
	c.lockCheckin("dai-a", time.Now(), checkinRefusedLock)
	if _, err := c.UserResource(a); err != nil {
		t.Fatalf("锁期探测: %v", err)
	}
	if got := c.refusedUntil("dai-a"); got.Before(lockUntil.Add(-time.Second)) {
		t.Fatalf("成功的余额探测把静默锁抹掉了：%s < %s", got, lockUntil)
	}
}

// TestCheckinMinInterval 距上次尝试不足 30s ⇒ 不回传（兜底间隔）。
func TestCheckinMinInterval(t *testing.T) {
	st := &acctState{nonce: "n", seconds: 2, ackedAt: time.Now().Add(-time.Hour)}
	now := time.Now()
	if !st.checkinDue(now) {
		t.Fatal("满足半衰期时应到点")
	}
	st.attemptAt = now.Add(-5 * time.Second)
	if st.checkinDue(now) {
		t.Fatal("距上次尝试 5s 不该再打（30s 兜底）")
	}
}

// TestCheckinDueRespectsRefusedWindow 静默锁优先于其它条件。
func TestCheckinDueRespectsRefusedWindow(t *testing.T) {
	now := time.Now()
	st := &acctState{nonce: "n", seconds: 2, ackedAt: now.Add(-time.Hour), refusedUntil: now.Add(time.Hour)}
	if st.checkinDue(now) {
		t.Fatal("锁期内不得签到")
	}
}

// chatServer 假对话端点：按序号返回预设响应，并统计被打了多少次。
func chatServer(t *testing.T, hits *int, handles ...func(http.ResponseWriter)) string {
	t.Helper()
	idx := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*hits++
		if idx >= len(handles) {
			idx = len(handles) - 1
		}
		h := handles[idx]
		idx++
		h(w)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestShortCircuitHitsWithoutUpstream (账号,模型) 命中时本地返回 403、一个请求都不发。
func TestShortCircuitHitsWithoutUpstream(t *testing.T) {
	var hits int
	url := chatServer(t, &hits, statusOK("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	c := New()
	c.Origin = url
	a := testAuth("dai-a", "k", "")
	// 先让账号知道恢复点（/theos 已探测过），再记一次耗尽。
	c.states.mu.Lock()
	c.stateLocked("dai-a").refreshAt = time.Now().Add(time.Hour)
	refreshAt := c.stateLocked("dai-a").refreshAt
	c.states.mu.Unlock()
	if !c.noteExhausted("dai-a", "glm-5.3-flash", refreshAt) {
		t.Fatal("noteExhausted 应记下恢复点")
	}
	body := []byte(`{"model":"glm-5.3-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	rc, status, errBody, err := c.ChatStream(a, body)
	if err != nil {
		t.Fatalf("短路不得返回传输层 error（会绕过 Classify）：%v", err)
	}
	if rc != nil {
		t.Fatal("短路不应有响应体")
	}
	if status != http.StatusForbidden {
		t.Fatalf("status=%d，期望合成的 403", status)
	}
	if kind := c.Classify(status, string(errBody)); kind != provider.ErrSoftRate || !strings.Contains(string(errBody), "theos_exhausted") {
		t.Fatalf("合成体必须能被归成 soft_rate（实际 %s）：%s", kind, string(errBody))
	}
	if hits != 0 {
		t.Fatalf("短路仍打了上游 %d 次", hits)
	}
	// 另一个模型不受影响。
	if _, st2, _, err2 := c.ChatStream(a, []byte(`{"model":"glm-5-free","messages":[{"role":"user","content":"hi"}],"stream":true}`)); err2 != nil || st2 != 200 {
		t.Fatalf("同号另一模型不该被短路：status=%d err=%v", st2, err2)
	}
	// 另一个账号同模型也不受影响（键含 uid）。
	if _, st3, _, _ := c.ChatStream(testAuth("dai-b", "k", ""), body); st3 != 200 {
		t.Fatalf("另一账号不该被短路：status=%d", st3)
	}
}

// TestShortCircuitRecordsOnlyWithKnownRefreshAt 恢复点未知 ⇒ 不记短路（避免拿猜的时长藏额度）。
func TestShortCircuitRecordsOnlyWithKnownRefreshAt(t *testing.T) {
	c := New()
	if c.noteExhausted("dai-a", "m", time.Time{}) {
		t.Fatal("恢复点未知时不得记短路")
	}
	if _, ok := c.exhaustedUntil("dai-a", "m"); ok {
		t.Fatal("未记录却命中了短路")
	}
}

// TestChat403TheosExhaustedRecordsShortCircuit 真收到 403 theos_exhausted ⇒ 记短路（恢复点取
// 上一次 /theos 的 refresh_at），下一次同 (账号,模型) 请求不再打上游。
func TestChat403TheosExhaustedRecordsShortCircuit(t *testing.T) {
	// 一个 server 同时冒充 /theos 与 /chat/completions，省一个 Client 的 Origin 来回切。
	var hits int
	refreshAt := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/theos") {
			_, _ = io.WriteString(w, theosJSON("0", refreshAt, "n1"))
			return
		}
		hits++
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"code":"theos_exhausted"}}`)
	}))
	defer srv.Close()
	c := New()
	c.Origin = srv.URL
	a := testAuth("dai-a", "k", "")
	if _, err := c.UserResource(a); err != nil {
		t.Fatalf("预探测: %v", err)
	}

	body := []byte(`{"model":"glm-5.3-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	_, st, _, err := c.ChatStream(a, body)
	if err != nil || st != http.StatusForbidden {
		t.Fatalf("首次应拿到上游 403：status=%d err=%v", st, err)
	}
	if hits != 1 {
		t.Fatalf("首次应打上游 1 次，实际 %d", hits)
	}
	until, ok := c.exhaustedUntil("dai-a", "glm-5.3-flash")
	if !ok {
		t.Fatal("收到 theos_exhausted 后应记短路")
	}
	if got := until.UTC().Format(time.RFC3339); got != refreshAt {
		t.Fatalf("短路恢复点应取 refresh_at=%s，实际 %s", refreshAt, got)
	}
	// 第二次：本地短路，不再打上游。
	if _, st2, _, _ := c.ChatStream(a, body); st2 != http.StatusForbidden || hits != 1 {
		t.Fatalf("第二次应本地短路：status=%d hits=%d", st2, hits)
	}
}
