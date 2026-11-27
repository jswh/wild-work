package deepai

import (
	"net/http"
	"strings"
	"testing"

	"wild-work/internal/provider"
)

// TestClassifyTable 逐行锁死 spec §5 的错误映射表（判定顺序固定，见 classifyKind 注释）。
func TestClassifyTable(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   provider.ErrKind
	}{
		// 1. 3xx（Key 失效跳登录页）
		{"3xx-302", http.StatusFound, ``, provider.ErrSessionDead},
		{"3xx-307", http.StatusTemporaryRedirect, ``, provider.ErrSessionDead},
		// 2. 401 / auth_required / invalid_api_key
		{"401", http.StatusUnauthorized, `{"error":"auth_required"}`, provider.ErrSessionDead},
		{"403-invalid_api_key", http.StatusForbidden, `{"error":{"code":"invalid_api_key"}}`, provider.ErrSessionDead},
		// 3. 账号准入：邮箱未验证 / 必须社交登录
		{"403-email_unverified", http.StatusForbidden, `{"error":"email_unverified"}`, provider.ErrAccountFault},
		{"403-social_login", http.StatusForbidden, `{"error":"social_login_required"}`, provider.ErrAccountFault},
		// 4. 计费额度耗尽 / 网络绑定：**只冻模型**，不得整号硬冷却
		{"403-theos_exhausted", http.StatusForbidden, `{"error":{"code":"theos_exhausted"}}`, provider.ErrSoftRate},
		{"403-network_bound", http.StatusForbidden, `{"error":"network_bound"}`, provider.ErrSoftRate},
		// 5. 并发会话占满
		{"409-session_busy", http.StatusConflict, `{"error":"session_busy"}`, provider.ErrSoftRate},
		// 6. 车道不匹配：请求级透传
		{"426", http.StatusUpgradeRequired, `{"error":"lane_version_unsupported"}`, provider.ErrPassthrough},
		{"400-lane", http.StatusBadRequest, `{"error":"lane_version_unsupported"}`, provider.ErrPassthrough},
		// 7. 漏发机器指纹（本工具的 bug）
		{"400-machine_required", http.StatusBadRequest, `{"error":"machine_required"}`, provider.ErrAccountFault},
		// 9. 其它 400/422
		{"400", http.StatusBadRequest, `{"error":{"message":"bad"}}`, provider.ErrBadParams},
		{"422", http.StatusUnprocessableEntity, `{"detail":"bad"}`, provider.ErrBadParams},
		// 10. 404
		{"404", http.StatusNotFound, `{"error":"no such model"}`, provider.ErrModelBlocked},
		// 11. 429
		{"429", http.StatusTooManyRequests, `{"error":"rate limited"}`, provider.ErrSoftRate},
		// 12. 其它 403
		{"403-other", http.StatusForbidden, `{"error":"forbidden region"}`, provider.ErrAccountFault},
		// 13. 5xx
		{"500", http.StatusInternalServerError, ``, provider.ErrServer},
		{"503", http.StatusServiceUnavailable, `upstream down`, provider.ErrServer},
		// 14. 其它 4xx
		{"418", http.StatusTeapot, ``, provider.ErrClient},
		// 内容拦截（400 + 审核文案）
		{"400-content_filter", http.StatusBadRequest, `{"error":{"code":"content_filter"}}`, provider.ErrContentBlocked},
		// 402 是明确的权益不足（与 403 theos_exhausted 不同口径）
		{"402", http.StatusPaymentRequired, `{"error":"no theos"}`, provider.ErrHardCredit},
	}
	for _, c2 := range cases {
		if got := New().Classify(c2.status, c2.body); got != c2.want {
			t.Fatalf("%s: Classify(%d,%q)=%s，期望 %s", c2.name, c2.status, c2.body, got, c2.want)
		}
	}
}

// TestClassify429Wins 429 必须优先于「余额/额度」类判据（不变量 15）。
func TestClassify429Wins(t *testing.T) {
	for _, body := range []string{`insufficient quota`, `{"error":"theos_exhausted"}`, `quota exceeded`} {
		if got := New().Classify(http.StatusTooManyRequests, body).String(); got != "soft_rate" {
			t.Fatalf("429+%q 被归成 %s，必须先判限流", body, got)
		}
	}
}

// TestClassifyCheckInRefusedIsNotAccountFault 签到被拒（`/theos` 的心跳不被接受）不得被
// 读成「账号坏了」或「额度耗尽」：积分路径把它当非错误处理，对话面这条兜底必须同样温和。
func TestClassifyCheckInRefusedIsNotAccountFault(t *testing.T) {
	got := New().Classify(http.StatusForbidden, `{"error":{"code":"check_in_refused"}}`)
	if got != provider.ErrPassthrough {
		t.Fatalf("check_in_refused 归成 %s，期望 passthrough（不罚号不轮转）", got)
	}
}

// TestClassifyHintForHumanAction 只有「要人动手」的两类错误带提示，且提示点明动作。
func TestClassifyHintForHumanAction(t *testing.T) {
	lane := hintFor(http.StatusUpgradeRequired, []byte(`{"error":"lane_version_unsupported"}`))
	if !strings.Contains(lane, "车道") || !strings.Contains(lane, "升级") {
		t.Fatalf("车道提示不可行动：%q", lane)
	}
	machine := hintFor(http.StatusBadRequest, []byte(`{"error":"machine_required"}`))
	if !strings.Contains(machine, HeaderMachine) {
		t.Fatalf("machine_required 提示未指向漏发的头：%q", machine)
	}
	if got := hintFor(http.StatusTooManyRequests, []byte(`slow down`)); got != "" {
		t.Fatalf("429 不该有人类动作提示，got %q", got)
	}
	if got := hintFor(http.StatusForbidden, []byte(`{"error":"theos_exhausted"}`)); got != "" {
		t.Fatalf("额度耗尽不该有人类动作提示（会自动恢复），got %q", got)
	}
}
