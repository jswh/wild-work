package deepai

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"wild-work/internal/provider"
)

// Classify 把上游 HTTP 状态码 + 响应体映射成本仓的错误类别。
//
// 顺序纪律（不变量 15）：**429 必须最先判**——限流报文里高频带 `quota` 一类字样，
// 先判余额耗尽会把限流误归成 `ErrHardCredit`（整号 12h 硬冷却）。
//
// 本文件在票 01 只做「状态码级」判定；票 03 再补上游**业务码级**的全表
// （`theos_exhausted` / `network_bound` / `session_busy` / `machine_required` / 形状拒绝 …），
// 两者按「先码级、后状态级」合并，不在此处预留桩分支。
func (c *Client) Classify(status int, body string) provider.ErrKind {
	if status == http.StatusTooManyRequests {
		return provider.ErrSoftRate
	}
	// 3xx：上游在 Key 失效时 302 跳登录页，而客户端用 ErrUseLastResponse 拦住了跟随
	// ⇒ 这里是唯一能看见「跳登录」的地方（见 classifyStatus 的注释）。
	if status >= 300 && status < 400 {
		return provider.ErrSessionDead
	}
	lower := strings.ToLower(body)
	switch {
	case status == http.StatusUnauthorized:
		// 上游把「Key 无效/被删」和「跳登录页」都落在这里；3xx 在 do() 里已被
		// 归一成登录态失效（见 classifyStatus），这里同口径。
		return provider.ErrSessionDead
	case status == http.StatusPaymentRequired:
		return provider.ErrHardCredit
	case status == http.StatusNotFound:
		return provider.ErrModelBlocked
	case status >= 500:
		return provider.ErrServer
	case status >= 400:
		if strings.Contains(lower, "content_filter") || strings.Contains(lower, "content policy") {
			return provider.ErrContentBlocked
		}
		return provider.ErrClient
	}
	return provider.ErrClient
}

// classifyStatus 把「3xx → 登录态失效」的归一规则落到一次判定里。
//
// 为什么把 3xx 也归 `ErrSessionDead`：上游在 Key 失效时的实际形态是 302 跳到登录页
// （`do()` 里用 `ErrUseLastResponse` 拦住了跟随）。若不归一，handler 会把它当
// 「其它 4xx/3xx 业务错误」原文透传，用户看到的是一个空 body，且账号永远不标记失效。
func classifyStatus(client *Client, status int, body string) provider.ErrKind {
	// 3xx 的归一在 Classify 内部完成（handler 走的也是同一个 Classify），这里只是
	// 「渠道内部构造错误对象」时的统一入口，避免两条路径对 3xx 判据不一致。
	return client.Classify(status, body)
}

// upstreamErrorWithStatus 构造类型化错误（基础设施路径用：调用方拿不到 body 时）。
//
// 必须返回 `*provider.Error` 而不是裸 `fmt.Errorf`：自愈链全靠 `errors.As` 取 Kind，
// 裸 error 会让 401 自愈永不生效（R79）。
func upstreamErrorWithStatus(status int, body []byte) error {
	return &provider.Error{
		Kind:   provider.ErrClient,
		Status: status,
		Msg:    fmt.Sprintf("上游返回 HTTP %d", status),
	}
}

// errorMessage 尽力从错误体里取可读文案（OpenAI 风格 `{"error":{"message":…}}` 或 `{"detail":…}`）。
//
// 取不到就返回原串（截断），**不改写**——上游文案是用户排障的第一手材料。
func errorMessage(body []byte) string {
	var probe struct {
		Error struct {
			Message string `json:"message"`
			Code    any    `json:"code"`
		} `json:"error"`
		Detail  string `json:"detail"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &probe); err == nil {
		for _, s := range []string{probe.Error.Message, probe.Detail, probe.Message} {
			if strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
