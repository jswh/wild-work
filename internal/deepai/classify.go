package deepai

import (
	"encoding/json"
	"net/http"
	"strings"

	"wild-work/internal/provider"
)

// classifyKind 是本渠道**唯一**的错误判定规则表：Classify（handler 调用）与渠道内
// 诊断日志（ChatStream）都从这里取结论，避免两处判据漂移。
//
// 顺序纪律（不变量 15）：**429 必须最先判**——限流报文里高频带 `quota` 一类字样，
// 先判余额耗尽会把限流误归成 `ErrHardCredit`（整号 12h 硬冷却）。
//
// 返回的 hint 非空时表示「这条错误需要人类动作」，调用方负责打醒目日志（带 uid）。
// 分类本身不记日志：Classify 拿不到账号上下文，而日志必须有 uid 才可定位。
func classifyKind(status int, body string) (provider.ErrKind, string) {
	if status == http.StatusTooManyRequests {
		return provider.ErrSoftRate, ""
	}
	// 3xx：上游在 Key 失效时 302 跳登录页，而客户端用 ErrUseLastResponse 拦住了跟随
	// ⇒ 这里是唯一能看见「跳登录」的地方。
	if status >= 300 && status < 400 {
		return provider.ErrSessionDead, ""
	}
	lower := strings.ToLower(body)
	has := func(sub string) bool { return strings.Contains(lower, sub) }
	hasAny := func(subs ...string) bool {
		for _, s := range subs {
			if strings.Contains(lower, s) {
				return true
			}
		}
		return false
	}
	switch {
	case status == http.StatusUnauthorized, hasAny("auth_required", "invalid_api_key", "invalid api key"):
		// 登录态失效：可自愈标记（R76），**不** Disable。
		return provider.ErrSessionDead, ""
	case status == 426 || has("lane_version_unsupported"):
		// 车道不匹配是**本工具的常量过期**，与账号健康无关：原文透传、不罚号不轮转。
		return provider.ErrPassthrough, "车道版本不被上游接受（lane_version_unsupported）：需要升级本工具的 deepai 车道常量（constants.go 的 LaneVersion），不是账号问题"
	case has("machine_required"):
		// 上游要求机器指纹而本工具没发出来 ⇒ 是我们的 bug。归账号级错误会去罚一个健康账号，
		// 掩盖真正的故障点；这里按账号级分类但**必须**有醒目日志指向漏发头。
		return provider.ErrAccountFault, "上游报 machine_required：本工具漏发了 X-DeepAI-Code-Machine（机器指纹），请按此关键词查 issue"
	case has("check_in_refused"):
		// 签到被拒（`/theos` 的心跳/证明不被接受）不罚号、不轮转，也不该让调用方看到
		// 「账号坏了」。积分路径（票 04）把这种 403 当**非错误**处理（只日志 + 静默锁），
		// 这条分类只是兜底：万一它出现在对话面上，也不会被读成账号故障或额度耗尽。
		return provider.ErrPassthrough, ""
	case hasAny("email_unverified", "social_login_required"):
		// 账号侧准入问题：换号通常有效（别的号已验邮箱/已绑社交），故软冷却 + 轮换。
		return provider.ErrAccountFault, ""
	case hasAny("theos_exhausted", "network_bound"):
		// **计费额度耗尽只影响计量模型**（theosPerRequest>0），12 个免费模型照常可用。
		// 归 ErrHardCredit 会整号硬冷却 12h —— 等于把主力免费模型一起冻掉，故归软限流：
		// 号×模型短冷却 + 换号（token 04 另加 ≤refresh_at 的本地短路）。
		return provider.ErrSoftRate, ""
	case status == http.StatusConflict, has("session_busy"):
		// 并发会话占满：换号/稍后再来都能解，属瞬时背压。
		return provider.ErrSoftRate, ""
	case status == http.StatusNotFound:
		return provider.ErrModelBlocked, ""
	case status >= 500:
		return provider.ErrServer, ""
	case status == http.StatusPaymentRequired:
		// 402 是明确的「钱/权益不足」语义，与 403 theos_exhausted 不同：后者只冻模型。
		return provider.ErrHardCredit, ""
	case hasAny("content_filter", "content policy", "content_policy"):
		return provider.ErrContentBlocked, ""
	case status == http.StatusBadRequest, status == http.StatusUnprocessableEntity:
		return provider.ErrBadParams, ""
	case status == http.StatusForbidden:
		// 其它 403：账号侧准入（地区/风控），换号可能有效。
		return provider.ErrAccountFault, ""
	}
	return provider.ErrClient, ""
}

// Classify 把上游 HTTP 状态码 + 响应体映射成本仓的错误类别。
func (c *Client) Classify(status int, body string) provider.ErrKind {
	kind, _ := classifyKind(status, body)
	return kind
}

// hintFor 返回该次失败对应的「需要人类动作」提示（无则空串）。
func hintFor(status int, body []byte) string {
	_, hint := classifyKind(status, string(body))
	return hint
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
