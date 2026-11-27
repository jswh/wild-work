package deepai

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// TestNormalizeProxy 代理串规整：缺 scheme 补 http://（`host:port` 形态不被当成 scheme）、
// 只接受 http(s)/socks5、必须有端口、不带路径参数、空串 = 直连（不是错误）。
func TestNormalizeProxy(t *testing.T) {
	cases := []struct {
		raw     string
		want    string // 期望 u.String()；空表示应报错
		wantErr bool
	}{
		{"", "", false},
		{"   ", "", false},
		{"127.0.0.1:1080", "http://127.0.0.1:1080", false},
		{"  \"127.0.0.1:1080\" ", "http://127.0.0.1:1080", false},
		{"http://10.0.0.1:8080", "http://10.0.0.1:8080", false},
		{"socks5://127.0.0.1:1080", "socks5://127.0.0.1:1080", false},
		{"https://user:pass@host.example:8443", "https://user:pass@host.example:8443", false},
		{"ftp://host:21", "", true},
		{"http://host", "", true},            // 缺端口
		{"http://", "", true},                // 缺主机
		{"http://host:8080/proxy", "", true}, // 带路径
		{"http://host:8080?x=1", "", true},   // 带参数
		{"ho st:8080", "", true},             // 含空白
	}
	for _, c := range cases {
		u, err := NormalizeProxy(c.raw)
		if c.wantErr {
			if err == nil {
				t.Fatalf("NormalizeProxy(%q) 应报错，实际得到 %v", c.raw, u)
			}
			continue
		}
		if err != nil {
			t.Fatalf("NormalizeProxy(%q) 报错：%v", c.raw, err)
		}
		if c.want == "" {
			if u != nil {
				t.Fatalf("NormalizeProxy(%q) 空串应返回 nil，实际 %v", c.raw, u)
			}
			continue
		}
		if u == nil || u.String() != c.want {
			t.Fatalf("NormalizeProxy(%q)=%v，期望 %q", c.raw, u, c.want)
		}
	}
}

// fakeProxy 假 HTTP 代理：回固定标记报文并记下被谁处理（不真正转发）。
//
// 用 http:// 目标（不解密 TLS）就足以证明「请求从哪条出口出去」——
// 代理收到的是 absolute-URI 的明文请求。
func fakeProxy(t *testing.T, tag string, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, "proxy:"+tag)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// originServer 直连目标：回 "direct"。
func originServer(t *testing.T, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, "direct")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// testTargetHost 测试用的**非环回**目标主机名（httpproxy 对环回恒直连，测不出出口）。
const testTargetHost = "upstream.deepai.test"

// testClientToOrigin 造一个「目标主机名拨到本机 origin」的 client。
//
// 为什么不用 127.0.0.1 当目标：x/net/httpproxy 对环回地址**恒直连**（安全默认，
// 与 NO_PROXY 无关），拿它测代理永远只会得到「直连」，测不出出口选择。
func testClientToOrigin(t *testing.T, origin *httptest.Server) *Client {
	t.Helper()
	addr := origin.Listener.Addr().String()
	bt := baseTransport()
	// 直连路径也要确定性：不带账号出口时不该被本机环境里的 HTTP(S)_PROXY 影响。
	bt.Proxy = nil
	bt.DialContext = func(ctx context.Context, network, target string) (net.Conn, error) {
		var d net.Dialer
		// 只把**目标主机**解析到本机 origin；代理主机必须真拨（否则代理连接也被劫持到
		// origin，看起来就是「直连」——第一版就是这么测错的）。
		if strings.HasPrefix(target, testTargetHost+":") {
			return d.DialContext(ctx, network, addr)
		}
		return d.DialContext(ctx, network, target)
	}
	tr := newAccountTransport(bt)
	c := New()
	c.HTTP.Transport = tr
	c.StreamHTTP.Transport = tr
	return c
}

// doOnce 用渠道 client 发一次 GET（带上账号 ⇒ 走该账号的出口）。
func doOnce(t *testing.T, c *Client, a *auth.Auth, target string) (string, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(withAccount(context.Background(), a), http.MethodGet, target, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body), nil
}

// TestPerAccountProxyRoutesEachAccount 两个账号各配一个代理 ⇒ 各自的请求落到各自的出口。
func TestPerAccountProxyRoutesEachAccount(t *testing.T) {
	var p1Hits, p2Hits, directHits atomic.Int32
	p1 := fakeProxy(t, "one", &p1Hits)
	p2 := fakeProxy(t, "two", &p2Hits)
	origin := originServer(t, &directHits)
	// NO_PROXY 必须清掉：本机回环默认豁免会让代理形同虚设（那是另一条用例）。
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("http_proxy", "")

	c := testClientToOrigin(t, origin)
	a1 := testAuth("dai-1", "k", "")
	a1.Proxy = p1.URL
	a2 := testAuth("dai-2", "k", "")
	a2.Proxy = p2.URL
	target := "http://" + testTargetHost + "/deepai_code/models"

	if body, err := doOnce(t, c, a1, target); err != nil || body != "proxy:one" {
		t.Fatalf("账号 1 没走自己的出口：body=%q err=%v", body, err)
	}
	if body, err := doOnce(t, c, a2, target); err != nil || body != "proxy:two" {
		t.Fatalf("账号 2 没走自己的出口：body=%q err=%v", body, err)
	}
	if p1Hits.Load() != 1 || p2Hits.Load() != 1 {
		t.Fatalf("代理命中次数不对：p1=%d p2=%d", p1Hits.Load(), p2Hits.Load())
	}
	if directHits.Load() != 0 {
		t.Fatalf("配了代理却直连了（%d 次）——这正是「静默直连」事故", directHits.Load())
	}
}

// TestNoProxyGoesDirect 无代理 ⇒ 直连；另一个号配了代理不影响它。
func TestNoProxyGoesDirect(t *testing.T) {
	var p1Hits, directHits atomic.Int32
	p1 := fakeProxy(t, "one", &p1Hits)
	origin := originServer(t, &directHits)
	t.Setenv("NO_PROXY", "")
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("http_proxy", "")

	c := testClientToOrigin(t, origin)
	plain := testAuth("dai-plain", "k", "")
	withProxy := testAuth("dai-proxy", "k", "")
	withProxy.Proxy = p1.URL
	target := "http://" + testTargetHost + "/deepai_code/models"

	if body, err := doOnce(t, c, plain, target); err != nil || body != "direct" {
		t.Fatalf("无代理账号应直连：body=%q err=%v", body, err)
	}
	if body, err := doOnce(t, c, withProxy, target); err != nil || body != "proxy:one" {
		t.Fatalf("有代理账号应走代理：body=%q err=%v", body, err)
	}
	if directHits.Load() != 1 || p1Hits.Load() != 1 {
		t.Fatalf("命中次数不对：direct=%d proxy=%d", directHits.Load(), p1Hits.Load())
	}
}

// TestInvalidProxyFailsWithoutDirectFallback 代理串非法 ⇒ 账号级错误，**绝不**静默直连。
func TestInvalidProxyFailsWithoutDirectFallback(t *testing.T) {
	var directHits atomic.Int32
	origin := originServer(t, &directHits)
	t.Setenv("NO_PROXY", "")
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("http_proxy", "")

	c := New()
	a := testAuth("dai-bad", "k", "")
	a.Proxy = "ftp://not-supported:21"

	_, err := doOnce(t, c, a, origin.URL)
	if err == nil {
		t.Fatal("非法代理串必须报错（不得静默直连）")
	}
	var pe *provider.Error
	if !errors.As(err, &pe) || pe.Kind != provider.ErrAccountFault {
		t.Fatalf("应为账号级错误（errors.As 到 provider.Error/ErrAccountFault），实际 %v", err)
	}
	if !strings.Contains(err.Error(), "代理配置有误") {
		t.Fatalf("错误文案要能指向出口：%v", err)
	}
	if directHits.Load() != 0 {
		t.Fatalf("非法代理下仍然直连了 %d 次（静默直连事故）", directHits.Load())
	}
}

// TestProxyRespectsNoProxy NO_PROXY 命中的目标必须直连（环回/内网不得被劫持到代理）。
func TestProxyRespectsNoProxy(t *testing.T) {
	var p1Hits, directHits atomic.Int32
	p1 := fakeProxy(t, "one", &p1Hits)
	origin := originServer(t, &directHits)
	t.Setenv("NO_PROXY", "upstream.deepai.test,.deepai.test")
	t.Setenv("no_proxy", "upstream.deepai.test,.deepai.test")
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("http_proxy", "")

	c := testClientToOrigin(t, origin)
	a := testAuth("dai-1", "k", "")
	a.Proxy = p1.URL
	if body, err := doOnce(t, c, a, "http://"+testTargetHost+"/deepai_code/models"); err != nil || body != "direct" {
		t.Fatalf("NO_PROXY 命中的目标应直连：body=%q err=%v", body, err)
	}
	if p1Hits.Load() != 0 {
		t.Fatalf("NO_PROXY 命中却仍走了代理（%d 次）", p1Hits.Load())
	}
	if directHits.Load() != 1 {
		t.Fatalf("直连命中次数=%d，期望 1", directHits.Load())
	}
}

// TestProxyChangeTakesEffectImmediately 改代理后**下一次**请求即走新出口（缓存键含代理串）。
func TestProxyChangeTakesEffectImmediately(t *testing.T) {
	var p1Hits, p2Hits atomic.Int32
	p1 := fakeProxy(t, "one", &p1Hits)
	p2 := fakeProxy(t, "two", &p2Hits)
	origin := originServer(t, new(atomic.Int32))
	t.Setenv("NO_PROXY", "")
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("http_proxy", "")

	c := testClientToOrigin(t, origin)
	a := testAuth("dai-1", "k", "")
	a.Proxy = p1.URL
	target := "http://" + testTargetHost + "/deepai_code/models"
	if body, _ := doOnce(t, c, a, target); body != "proxy:one" {
		t.Fatalf("首次应是 p1：%q", body)
	}
	a.Lock()
	a.Proxy = p2.URL
	a.Unlock()
	if body, _ := doOnce(t, c, a, target); body != "proxy:two" {
		t.Fatalf("改代理后应立即走 p2，实际 %q", body)
	}
	// 清空 ⇒ 回到直连（配置文件里 proxy 为空串即可）。
	a.Lock()
	a.Proxy = ""
	a.Unlock()
	if body, _ := doOnce(t, c, a, target); body != "direct" {
		t.Fatalf("清空出口后应直连，实际 %q", body)
	}
}

// TestChatStreamRejectsMisconfiguredProxy 出口配错时对话**不打上游**、回合成的 400 且可被分类。
func TestChatStreamRejectsMisconfiguredProxy(t *testing.T) {
	var hits atomic.Int32
	origin := originServer(t, &hits)
	c := New()
	c.Origin = origin.URL
	a := testAuth("dai-bad", "k", "")
	a.Proxy = "ftp://nope:21"

	rc, status, body, err := c.ChatStream(a, []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	if err != nil {
		t.Fatalf("不得返回传输层 error（会绕过 Classify）：%v", err)
	}
	if rc != nil {
		t.Fatal("拒绝型响应不该有 body reader")
	}
	if status != http.StatusBadRequest {
		t.Fatalf("status=%d，期望合成的 400", status)
	}
	if !strings.Contains(string(body), "proxy_misconfigured") {
		t.Fatalf("合成体应带 proxy_misconfigured：%s", string(body))
	}
	if kind := c.Classify(status, string(body)); kind != provider.ErrAccountFault {
		t.Fatalf("应归 ErrAccountFault（换号可见），实际 %s", kind)
	}
	if hits.Load() != 0 {
		t.Fatalf("出口配错仍打了上游 %d 次", hits.Load())
	}
}

// TestAuthFileRoundTripProxy 凭据文件的 proxy 字段可读可写，老文件读出空串（向后兼容）。
func TestAuthFileRoundTripProxy(t *testing.T) {
	dir := t.TempDir()
	a := &auth.Auth{
		Kind:        KindName,
		UID:         "dai-1",
		AccessToken: "k",
		ExpiresAt:   FarFutureExpiry(),
		Proxy:       "http://127.0.0.1:1080",
		FilePath:    dir + "/deepai-dai-1.json",
	}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err := auth.LoadDeepAIDir(dir)
	if err != nil || len(loaded) != 1 {
		t.Fatalf("load: %v n=%d", err, len(loaded))
	}
	if loaded[0].Proxy != "http://127.0.0.1:1080" {
		t.Fatalf("proxy 未回读：%q", loaded[0].Proxy)
	}

	// 老文件（没有 proxy 字段）⇒ 空串，不报错。
	old := `{"auth":{"accessToken":"k2","refreshToken":"","expiresAt":99},"account":{"uid":"dai-2","nickname":"n"}}`
	legacy, err := auth.Parse([]byte(old))
	if err != nil {
		t.Fatalf("老文件解析失败：%v", err)
	}
	if legacy.Proxy != "" {
		t.Fatalf("老文件应读出空出口，实际 %q", legacy.Proxy)
	}
}
