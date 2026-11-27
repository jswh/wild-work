package login_deepai

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wild-work/internal/deepai"
)

// fakeUpstream 假兑换端点：记下收到的 code/verifier，回 api_key/username/is_pro。
type fakeUpstream struct {
	srv      *httptest.Server
	gotCode  string
	gotVerif string
	hits     int
	status   int
	body     string
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{status: http.StatusOK, body: `{"api_key":"key-1","username":"alice","is_pro":true}`}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits++
		raw, _ := io.ReadAll(r.Body)
		f.gotCode = extractJSONField(string(raw), "code")
		f.gotVerif = extractJSONField(string(raw), "code_verifier")
		w.WriteHeader(f.status)
		_, _ = io.WriteString(w, f.body)
	}))
	t.Cleanup(f.srv.Close)
	// 把登录包的 client factory 指到假上游（Origin 可注入）。
	old := newLoginClient
	newLoginClient = func() *deepai.Client {
		c := deepai.New()
		c.Origin = f.srv.URL
		return c
	}
	t.Cleanup(func() { newLoginClient = old })
	return f
}

// extractJSONField 极简 JSON 取值（测试里只看两个字符串字段，避免再引 json 依赖阅读负担）。
func extractJSONField(body, key string) string {
	marker := `"` + key + `":"`
	i := strings.Index(body, marker)
	if i < 0 {
		return ""
	}
	rest := body[i+len(marker):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// TestStartReturnsDesktopAuthURL 授权 URL 四参齐全（含恒定的 PKCE 两参），并起了回环监听。
func TestStartReturnsDesktopAuthURL(t *testing.T) {
	newFakeUpstream(t)
	defer Shutdown()
	url, err := Start(nil, "")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	for _, want := range []string{
		"/deepai_code/desktop_auth/?state=",
		"&port=",
		"&code_challenge=",
		"&code_challenge_method=S256",
	} {
		if !strings.Contains(url, want) {
			t.Fatalf("授权 URL 缺 %q：%s", want, url)
		}
	}
	sess := current()
	if sess == nil || sess.srv == nil {
		t.Fatal("Start 应起本机回环监听（浏览器自动回调那条路）")
	}
}

// TestLoopbackCallbackCompletesLogin 自动回调（state 正确）→ Poll 兑换成功。
func TestLoopbackCallbackCompletesLogin(t *testing.T) {
	up := newFakeUpstream(t)
	defer Shutdown()
	if _, err := Start(nil, ""); err != nil {
		t.Fatalf("Start: %v", err)
	}
	sess := current()
	port := sess.port
	resp, err := http.Get(callbackURL(port, "code-abc", sess.state))
	if err != nil {
		t.Fatalf("回调请求: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("回调 status=%d，期望 200", resp.StatusCode)
	}
	res, err := Poll(nil, "")
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if res.APIKey != "key-1" || res.Username != "alice" || !res.IsPro {
		t.Fatalf("登录结果不对：%+v", res)
	}
	if up.gotCode != "code-abc" || up.gotVerif == "" {
		t.Fatalf("兑换请求没带上 code/verifier：code=%q verifier=%q", up.gotCode, up.gotVerif)
	}
	if up.hits != 1 {
		t.Fatalf("兑换应只打一次上游，实际 %d", up.hits)
	}
}

// TestLoopbackStateMismatchKeepsFlowPending state 错了只回 400、流程继续等（不是终态）。
func TestLoopbackStateMismatchKeepsFlowPending(t *testing.T) {
	newFakeUpstream(t)
	defer Shutdown()
	if _, err := Start(nil, ""); err != nil {
		t.Fatalf("Start: %v", err)
	}
	sess := current()
	// 先发一次错 state。
	resp, err := http.Get(callbackURL(sess.port, "code-x", "bogus-state"))
	if err != nil {
		t.Fatalf("回调请求: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("错 state 应 400，实际 %d", resp.StatusCode)
	}
	if _, perr := Poll(nil, ""); !errors.Is(perr, ErrPending) {
		t.Fatalf("错 state 后仍应是 pending，实际 %v", perr)
	}
	// 正确 state 仍能完成（一次无关请求不得毁掉本次登录）。
	resp2, err := http.Get(callbackURL(sess.port, "code-ok", sess.state))
	if err != nil {
		t.Fatalf("回调请求: %v", err)
	}
	_ = resp2.Body.Close()
	if _, perr := Poll(nil, ""); perr != nil {
		t.Fatalf("正确 state 后应完成，实际 %v", perr)
	}
}

// TestSubmitCallbackURLForms 粘贴：完整 URL / 裸 query 都能完成；只粘 code 被拒。
func TestSubmitCallbackURLForms(t *testing.T) {
	for _, c2 := range []struct {
		name string
		make func(state string) string
	}{
		{"完整 URL", func(st string) string { return "http://127.0.0.1:43210/auth-callback?code=c1&state=" + st }},
		{"裸 query", func(st string) string { return "code=c1&state=" + st }},
		{"路径+query", func(st string) string { return "/auth-callback?code=c1&state=" + st }},
	} {
		up := newFakeUpstream(t)
		Shutdown()
		if _, err := Start(nil, ""); err != nil {
			t.Fatalf("%s: Start: %v", c2.name, err)
		}
		st := current().state
		if err := SubmitCallbackURL(c2.make(st)); err != nil {
			t.Fatalf("%s: SubmitCallbackURL: %v", c2.name, err)
		}
		res, err := Poll(nil, "")
		if err != nil {
			t.Fatalf("%s: Poll: %v", c2.name, err)
		}
		if res.APIKey != "key-1" || up.gotCode != "c1" {
			t.Fatalf("%s: 结果不对：%+v code=%q", c2.name, res, up.gotCode)
		}
	}

	// 只粘 code：必须拒绝，并且给出「拿不到 state」的原因。
	Shutdown()
	if _, err := Start(nil, ""); err != nil {
		t.Fatalf("Start: %v", err)
	}
	err := SubmitCallbackURL("c1")
	Shutdown()
	if err == nil || !strings.Contains(err.Error(), "state") {
		t.Fatalf("只粘 code 必须被拒且说明缺 state，实际 %v", err)
	}

	// 裸 code 粘成 URL 形态但缺 state：同样拒绝。
	Shutdown()
	if _, err := Start(nil, ""); err != nil {
		t.Fatalf("Start: %v", err)
	}
	err = SubmitCallbackURL("http://127.0.0.1:1234/auth-callback?code=c1")
	Shutdown()
	if err == nil || !strings.Contains(err.Error(), "state") {
		t.Fatalf("缺 state 必须被拒，实际 %v", err)
	}
}

// TestSubmitCallbackURLWrongState 粘贴的 state 不属于本次登录 ⇒ 拒绝且流程不终止。
func TestSubmitCallbackURLWrongState(t *testing.T) {
	newFakeUpstream(t)
	defer Shutdown()
	if _, err := Start(nil, ""); err != nil {
		t.Fatalf("Start: %v", err)
	}
	sess := current()
	err := SubmitCallbackURL("code=c1&state=deadbeef")
	if err == nil || !strings.Contains(err.Error(), "state") {
		t.Fatalf("错 state 必须被拒，实际 %v", err)
	}
	if _, perr := Poll(nil, ""); !errors.Is(perr, ErrPending) {
		t.Fatalf("错 state 后仍是 pending，实际 %v", perr)
	}
	// 正确 state 仍可完成。
	if err := SubmitCallbackURL("code=c2&state=" + sess.state); err != nil {
		t.Fatalf("正确 state 应接受：%v", err)
	}
	if _, err := Poll(nil, ""); err != nil {
		t.Fatalf("Poll: %v", err)
	}
}

// TestDuplicateCallbackRejected 授权码只能兑换一次：重复提交给可读提示。
func TestDuplicateCallbackRejected(t *testing.T) {
	newFakeUpstream(t)
	defer Shutdown()
	if _, err := Start(nil, ""); err != nil {
		t.Fatalf("Start: %v", err)
	}
	sess := current()
	if err := SubmitCallbackURL("code=c1&state=" + sess.state); err != nil {
		t.Fatalf("首次提交: %v", err)
	}
	err := SubmitCallbackURL("code=c1&state=" + sess.state)
	if err == nil || !strings.Contains(err.Error(), "只能兑换一次") {
		t.Fatalf("重复提交应给「只能兑换一次」的提示，实际 %v", err)
	}
}

// TestLoopbackUnavailableDegradesToPaste 回环绑不上 ⇒ 不报错、仍给 URL，粘贴路径可完成。
func TestLoopbackUnavailableDegradesToPaste(t *testing.T) {
	old := listen
	listen = func(network, addr string) (net.Listener, error) {
		return nil, errors.New("bind: address already in use")
	}
	t.Cleanup(func() { listen = old })
	up := newFakeUpstream(t)
	defer Shutdown()

	url, err := Start(nil, "")
	if err != nil {
		t.Fatalf("回环绑不上不得报错（R120：降级为等粘贴）：%v", err)
	}
	sess := current()
	if sess.srv != nil {
		t.Fatal("降级时不应有监听")
	}
	if sess.port <= 0 || !strings.Contains(url, "&port=") {
		t.Fatalf("降级时仍要给一个合法端口（URL 形态）：port=%d url=%s", sess.port, url)
	}
	// TTL 内粘贴回调仍能完成登录。
	if err := SubmitCallbackURL("code=degraded&state=" + sess.state); err != nil {
		t.Fatalf("降级后粘贴应可用：%v", err)
	}
	res, err := Poll(nil, "")
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if res.APIKey != "key-1" || up.gotCode != "degraded" {
		t.Fatalf("降级路径结果不对：%+v code=%q", res, up.gotCode)
	}
}

// TestExchangeFailureIsTerminal 兑换失败（授权码失效）⇒ 终态错误，不无限 pending。
func TestExchangeFailureIsTerminal(t *testing.T) {
	up := newFakeUpstream(t)
	up.status = http.StatusBadRequest
	up.body = `{"error":"code not found"}`
	defer Shutdown()
	if _, err := Start(nil, ""); err != nil {
		t.Fatalf("Start: %v", err)
	}
	sess := current()
	if err := SubmitCallbackURL("code=stale&state=" + sess.state); err != nil {
		t.Fatalf("提交: %v", err)
	}
	_, err := Poll(nil, "")
	if err == nil || !strings.Contains(err.Error(), "授权码无效或已过期") {
		t.Fatalf("应给出可行动的终态错误，实际 %v", err)
	}
	if _, perr := Poll(nil, ""); perr == nil {
		t.Fatal("失败后不得再返回 pending（会话已清理）")
	}
}

// TestPollWithoutSession 没有流程时 Poll 给终态错误（面板不会永远转圈）。
func TestPollWithoutSession(t *testing.T) {
	Shutdown()
	if _, err := Poll(nil, ""); err == nil {
		t.Fatal("无会话时应返回终态错误")
	}
	if err := SubmitCallbackURL("code=c&state=s"); err == nil {
		t.Fatal("无会话时应拒绝粘贴回调（先发起登录）")
	}
}

// TestSubmitCallbackURLQuotedPaste 用户常连引号一起粘（curl/终端复制）。
func TestSubmitCallbackURLQuotedPaste(t *testing.T) {
	newFakeUpstream(t)
	defer Shutdown()
	if _, err := Start(nil, ""); err != nil {
		t.Fatalf("Start: %v", err)
	}
	st := current().state
	if err := SubmitCallbackURL("\"code=qc&state=" + st + "\""); err != nil {
		t.Fatalf("带引号的粘贴应被容忍：%v", err)
	}
	if _, err := Poll(nil, ""); err != nil {
		t.Fatalf("Poll: %v", err)
	}
}

// callbackURL 拼一条回环回调地址。
func callbackURL(port int, code, state string) string {
	return "http://127.0.0.1:" + itoa(port) + callbackPath + "?code=" + code + "&state=" + state
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestDesktopCredentialsParsed 桌面凭据明文 JSON 解析（含字段名大小写口径）。
func TestDesktopCredentialsParsed(t *testing.T) {
	dir := t.TempDir()
	secrets := filepath.Join(dir, "userdata", "secrets")
	if err := os.MkdirAll(secrets, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(secrets, credentialFile)
	if err := os.WriteFile(path, []byte(`{"apiKey":"  key-9  ","username":"bob","isPro":false}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cred, err := FindDesktopCredentials(dir)
	if err != nil {
		t.Fatalf("FindDesktopCredentials: %v", err)
	}
	if strings.TrimSpace(cred.APIKey) != "key-9" || cred.Username != "bob" || cred.IsPro {
		t.Fatalf("解析不对：%+v", cred)
	}
	if cred.Path != path {
		t.Fatalf("Path=%q，期望 %q", cred.Path, path)
	}
}

// TestDesktopCredentialsMissing 找不到就明确报「未找到」，不猜路径。
func TestDesktopCredentialsMissing(t *testing.T) {
	dir := t.TempDir()
	_, err := FindDesktopCredentials(dir)
	if err == nil {
		t.Fatal("空目录应报错")
	}
	if !strings.Contains(err.Error(), "未找到 DeepAI Code 桌面端凭据") {
		t.Fatalf("文案不可行动：%v", err)
	}
	if !strings.Contains(err.Error(), dir) {
		t.Fatalf("应列出尝试过的目录（便于自查）：%v", err)
	}
}

// TestDesktopCredentialsRejectsEmptyKey 文件存在但没有 apiKey ⇒ 报错（不得静默导入空 Key）。
func TestDesktopCredentialsRejectsEmptyKey(t *testing.T) {
	dir := t.TempDir()
	secrets := filepath.Join(dir, "userdata", "secrets")
	if err := os.MkdirAll(secrets, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(secrets, credentialFile)
	if err := os.WriteFile(path, []byte(`{"username":"bob"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := FindDesktopCredentials(dir); err == nil || !strings.Contains(err.Error(), "apiKey") {
		t.Fatalf("缺 apiKey 应报错，实际 %v", err)
	}
}
