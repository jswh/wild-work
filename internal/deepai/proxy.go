package deepai

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"

	"golang.org/x/net/http/httpproxy"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// 账号级出口（per-account proxy）。
//
// 为什么要它：上游的每日免费额度按「账号 + 机器指纹 + 出口」发放，同一 IP 上挂多个号
// 会互相稀释甚至触发风控；反过来住宅 IP 到期就要换一条，而换 IP 不该等于「删号重加」。
// 所以「出口」是**账号凭据的一部分**（`auth.Auth.Proxy`），不是全局配置。
//
// 为什么是「一个 client + ctx 分派」而不是「每账号懒建 client」（spec §9）：
// `cmd/wild-work/main.go` 的 `wrapDumps` 在启动期按**具体 client 实例**套 devdump
// 采样器，懒建的 client 永远套不上（`transport_test.go` 静态锁死那张表）。单 client
// 里按 uid 分派 Transport 既保住 devdump，又保住连接池按出口隔离。
//
// 硬纪律：代理串**解析失败要报错，绝不静默退回直连** —— 悄悄直连会让用户以为自己在用
// 独立出口，实际所有号挤在同一 IP 上（票 06 的验收口径）。

// proxyCtxKey 是请求 context 里携带账号的私有键（Transport 据此选出口）。
type proxyCtxKey struct{}

// withAccount 把账号放进请求 context（出站选出口用；nil 账号 = 公共请求）。
func withAccount(ctx context.Context, a *auth.Auth) context.Context {
	if a == nil {
		return ctx
	}
	return context.WithValue(ctx, proxyCtxKey{}, a)
}

// accountFromContext 取出请求上下文里的账号。
func accountFromContext(ctx context.Context) *auth.Auth {
	if ctx == nil {
		return nil
	}
	a, _ := ctx.Value(proxyCtxKey{}).(*auth.Auth)
	return a
}

// proxyOf 锁内取该账号的出口串（空串 = 直连 / 跟随环境代理）。
func proxyOf(a *auth.Auth) string {
	if a == nil {
		return ""
	}
	a.RLock()
	defer a.RUnlock()
	return strings.TrimSpace(a.Proxy)
}

// NormalizeProxy 规整并校验用户填的代理串，返回可用的 `*url.URL`。
//
// 缺 scheme 时补 `http://`：`host:port` 是最常见的粘贴形态，而 `url.Parse("1.2.3.4:8080")`
// 会把 `1.2.3.4` 当成 **scheme**、host 为空 —— 直接拿去用就是**静默直连**
// （票 06 点名的坑）。支持 http / https / socks5 三种 scheme。
func NormalizeProxy(raw string) (*url.URL, error) {
	s := strings.TrimSpace(raw)
	s = strings.Trim(s, "\"'")
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil // 空 = 直连，不是错误
	}
	if strings.ContainsAny(s, " \t\r\n") {
		return nil, fmt.Errorf("代理地址不能包含空白")
	}
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("代理地址无法解析")
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, fmt.Errorf("代理只支持 http(s) 或 socks5，当前是 %q", u.Scheme)
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("代理地址缺少主机名（形如 http://127.0.0.1:1080）")
	}
	if u.Port() == "" {
		return nil, fmt.Errorf("代理地址缺少端口（形如 http://127.0.0.1:1080）")
	}
	// 保留 path/query 会让上游代理配置莫名其妙地不生效：一律拒收，要求干净形态。
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("代理地址不要带路径或参数（形如 http://host:port）")
	}
	u.Path = ""
	return u, nil
}

// proxyHostOnly 只留 host:port（日志/错误体里**绝不**出现代理凭据，票 06 锚点）。
func proxyHostOnly(u *url.URL) string {
	if u == nil {
		return ""
	}
	return u.Host
}

// accountTransport 按 req.Context() 里的账号选择出口。
//
// 缓存键 = uid + "\x00" + 代理串：账号改代理后**下一次请求即生效**（新键 ⇒ 新 Transport），
// 旧的连接池自然闲置回收。无账号的请求（登录、目录拉取的公共请求等）走 base。
type accountTransport struct {
	base http.RoundTripper
	// proto 是「有代理时」克隆的原型：继承 base 上的一切定制（DialContext / TLS 配置 /
	// 连接池上限），只换 Proxy 一栏。用 baseTransport() 现搭一份会让定制静默丢失
	// （典型：测试注入的 DialContext，以及将来可能的 DNS 定制）。
	proto *http.Transport
	mu    sync.Mutex
	cache map[string]http.RoundTripper
	// warned 记「已就该 (uid,proxy) 打过一次日志」，避免每个请求刷屏。
	warned map[string]bool
}

// newAccountTransport 建分派层（base 未给时用默认 Transport）。
func newAccountTransport(base http.RoundTripper) *accountTransport {
	if base == nil {
		base = baseTransport()
	}
	t := &accountTransport{base: base, cache: map[string]http.RoundTripper{}, warned: map[string]bool{}}
	if bt, ok := base.(*http.Transport); ok {
		t.proto = bt // 有代理时克隆它（继承 DialContext 等定制）
	}
	return t
}

func (t *accountTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	a := accountFromContext(req.Context())
	raw := proxyOf(a)

	if raw == "" {
		return t.base.RoundTrip(req) // 直连 / 跟随环境代理（含 NO_PROXY，原生支持）
	}
	tr, err := t.transportFor(uidOf(a), raw)
	if err != nil {
		// 账号级错误，且**不换直连**：宁可让这个号失败，也不能让用户误以为出口独立。
		return nil, err
	}
	return tr.RoundTrip(req)
}

// transportFor 取（必要时建）某账号某个代理串对应的 Transport。
func (t *accountTransport) transportFor(uid, raw string) (http.RoundTripper, error) {
	key := uid + "\x00" + raw
	t.mu.Lock()
	if tr, ok := t.cache[key]; ok {
		t.mu.Unlock()
		return tr, nil
	}
	t.mu.Unlock()

	u, err := NormalizeProxy(raw)
	if err != nil {
		t.warnOnce(key, uid, raw, err)
		return nil, proxyConfigError(uid, u, err)
	}
	// NO_PROXY 必须显式尊重（自己设了代理就绕开了 ProxyFromEnvironment）：
	// 环回/内网地址被劫持到代理上会让本地回环请求与内网端点全部失败（R171 同款坑）。
	cfg := &httpproxyConfig{
		HTTPProxy:  u.String(),
		HTTPSProxy: u.String(),
		NoProxy:    firstNonEmptyEnv("NO_PROXY", "no_proxy"),
	}
	pf := cfg.proxyFunc()
	bt := baseTransport()
	if t.proto != nil {
		bt = t.proto.Clone()
	}
	bt.Proxy = func(r *http.Request) (*url.URL, error) {
		if r == nil || r.URL == nil {
			return nil, nil
		}
		return pf(r.URL)
	}
	t.mu.Lock()
	// 双检：并发首用只建一份（被挤掉的那份由 GC 回收，连接池未使用所以无泄漏成本）。
	if tr, ok := t.cache[key]; ok {
		t.mu.Unlock()
		return tr, nil
	}
	t.cache[key] = bt
	t.mu.Unlock()
	return bt, nil
}

// warnOnce 每个 (uid, proxy) 只打一行配置错误日志（只含 host:port，不含凭据）。
func (t *accountTransport) warnOnce(key, uid, raw string, err error) {
	t.mu.Lock()
	already := t.warned[key]
	t.warned[key] = true
	t.mu.Unlock()
	if already {
		return
	}
	log.Printf("deepai 代理配置有误 uid=%s proxy=%s err=%v（该账号直连已被禁用：绝不静默退回直连）",
		uid, proxyHostOnlyBestEffort(raw), err)
}

// proxyConfigError 构造账号级错误（handler/面板能看出「是这个号的出口配错了」）。
func proxyConfigError(uid string, u *url.URL, cause error) error {
	return &provider.Error{
		Kind: provider.ErrAccountFault,
		Msg: fmt.Sprintf("代理配置有误（uid=%s 出口=%s）：%v。请在「账号设置」里改正或清空出口",
			uid, proxyHostOnly(u), cause),
	}
}

// proxyHostOnlyBestEffort 从任意串里尽量取出 host:port（日志用；解析失败就给原文截断）。
func proxyHostOnlyBestEffort(raw string) string {
	if u, err := NormalizeProxy(raw); err == nil && u != nil {
		return u.Host
	}
	s := raw
	if i := strings.LastIndex(s, "@"); i >= 0 { // 去掉 user:pass@
		s = s[i+1:]
	}
	if len(s) > 64 {
		s = s[:64] + "…"
	}
	return s
}

func firstNonEmptyEnv(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

// proxyMisconfigured 判断一个错误是不是「出口配错」（ChatStream 据此回合成的账号级错误，
// 这样 handler 会换号而不是把它当网络抖动透传）。
func proxyMisconfigured(err error) bool {
	return err != nil && strings.Contains(err.Error(), "代理配置有误")
}

// httpproxyConfig 是 httpproxy.Config 的最小镜像（隔离依赖，便于测试替换）。
type httpproxyConfig struct {
	HTTPProxy  string
	HTTPSProxy string
	NoProxy    string
}

// proxyFunc 复刻 ProxyFromEnvironment 的匹配语义（后缀通配 + CIDR + 环回豁免），
// 但只用**本账号**配的那一条代理（其它环境变量不参与，避免账号出口被环境悄悄改写）。
func (c *httpproxyConfig) proxyFunc() func(*url.URL) (*url.URL, error) {
	cfg := &httpproxy.Config{
		HTTPProxy:  c.HTTPProxy,
		HTTPSProxy: c.HTTPSProxy,
		NoProxy:    c.NoProxy,
		CGI:        false,
	}
	return cfg.ProxyFunc()
}
