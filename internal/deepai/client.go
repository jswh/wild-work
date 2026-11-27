package deepai

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"time"

	"wild-work/internal/auth"
)

// Client 是 DeepAI Code 渠道的上游客户端。
//
// 两个 http.Client 的分工是硬约束（R35）：非流式 `HTTP` 带总超时兜底，
// 流式 `StreamHTTP` **不得**设 `Timeout`（它覆盖整个生命周期，含读 body，
// 长回答会被从中间掐断）；两者共用同一个 Transport 以复用连接池。
type Client struct {
	Origin     string
	HTTP       *http.Client
	StreamHTTP *http.Client
	idle       time.Duration
}

// New 按默认超时构造客户端（`upstream.timeout_seconds` 的接线在 main 装配处）。
func New() *Client { return NewWithTimeout(defaultRequestTimeout) }

// NewWithTimeout 显式指定非流式总超时。
func NewWithTimeout(requestTimeout time.Duration) *Client {
	if requestTimeout <= 0 {
		requestTimeout = defaultRequestTimeout
	}
	transport := newTransport()
	return &Client{
		Origin: DefaultOrigin,
		HTTP: &http.Client{
			Transport: transport,
			Timeout:   requestTimeout,
			// 上游鉴权失败时是 302 跳到登录页：跟随只会把「凭据失效」伪装成 200 的 HTML。
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		// 流式：不设 Timeout（R35），与 HTTP 共用 Transport。
		StreamHTTP: &http.Client{
			Transport:     transport,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		idle: defaultIdleTimeout,
	}
}

// IdleTimeout 返回实际生效的流中空闲上限（零值客户端退回默认，供测试与日志断言）。
func (c *Client) IdleTimeout() time.Duration {
	if c.idle > 0 {
		return c.idle
	}
	return defaultIdleTimeout
}

// newTransport 建标准连接池（与其它渠道同口径，不启用 HTTP/2 强制）。
func newTransport() *http.Transport {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: time.Second,
		ForceAttemptHTTP2:     true,
	}
}

// newRequest 组装一次出站请求（头族见 applyHeaders）。
//
// GET 也带 `Content-Type: application/json`（与桌面端一致，见 constants.go 的注释）。
func (c *Client) newRequest(ctx context.Context, method, path string, a *auth.Auth, body []byte, affinity string) (*http.Request, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Origin+path, rd)
	if err != nil {
		return nil, err
	}
	applyHeaders(req, a, affinity)
	return req, nil
}

// do 发一次请求并读错误体：状态码 ≥400 时返回 (nil, status, body, nil)，
// 由调用方（或 handler 的 Classify）决定处置——上游 ≥400 一律原样透传（不变量 8）。
func (c *Client) do(req *http.Request, client *http.Client) (io.ReadCloser, int, []byte, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
		return nil, resp.StatusCode, errBody, nil
	}
	return resp.Body, resp.StatusCode, nil, nil
}

// RefreshToken 空实现：本渠道凭据是一枚**不会轮换**的 API Key（无 refresh 端点）。
//
// 需要它是因为 `provider.Upstream` 要求；返回 nil 让「到期前刷新」这条路径安静通过。
// 凭据失效（401 / 3xx）走 `ErrSessionDead`，用户重新粘贴 Key 或重新登录即可自愈（R76）。
func (c *Client) RefreshToken(*auth.Auth) error { return nil }
