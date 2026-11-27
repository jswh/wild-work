package deepai

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStaticCatalogShape(t *testing.T) {
	catalog := StaticModels()
	if len(catalog) != 27 {
		t.Fatalf("目录条目=%d，期望 27（上游 28 个里剔除 responses-only 的 gpt-6-astra）", len(catalog))
	}
	byID := map[string]providerModel{}
	for _, m := range catalog {
		if m.ID == "" || m.Name == "" {
			t.Fatalf("条目缺 id/name：%+v", m)
		}
		if m.ContextWindow <= 0 {
			t.Fatalf("%s 缺上下文（目录里每个模型都有 contextWindow）", m.ID)
		}
		if !m.ContextFromAPI {
			t.Fatalf("%s 的上下文取自上游快照，ContextFromAPI 必须为 true", m.ID)
		}
		if !m.SupportsTools {
			t.Fatalf("%s 必须声明工具能力（桌面端每个对话请求都带 tools）", m.ID)
		}
		byID[m.ID] = providerModel{m.ContextWindow, m.MaxTokens, m.SupportsImages, m.SupportsReasoning}
	}
	if _, ok := byID["gpt-6-astra"]; ok {
		t.Fatal("gpt-6-astra 只认 /responses，本仓不实现该端点 ⇒ 不得出现在目录里")
	}
	// 有证据的字段值抽查。
	if got := byID["glm-5.3-flash"]; got.ctx != 320000 || got.max != 131072 || !got.img || !got.reason {
		t.Fatalf("glm-5.3-flash=%+v，期望 320000/131072/图片/思考", got)
	}
	if got := byID["glyph-cluster"]; got.ctx != 256000 || got.img {
		t.Fatalf("glyph-cluster=%+v，期望 256000 且纯文本", got)
	}
	if got := byID["claude-haiku-5-5"]; got.ctx != 95000 || got.max != 64000 {
		t.Fatalf("claude-haiku-5-5=%+v，期望 95000/64000", got)
	}
	// 纯文本五个。
	for _, id := range []string{"deepai-cyber", "glm-5.3", "glyph-cluster", "solar-mini-4", "minimax-m3"} {
		if byID[id].img {
			t.Fatalf("%s 是纯文本模型，不得声明图片能力", id)
		}
	}
	// 未取证的思考能力不得声明。
	for _, id := range []string{"kimi-k3", "gpt-6.1-sol", "deepseek-v4.1-flash"} {
		if byID[id].reason {
			t.Fatalf("%s 的思考能力未取证，不得声明（R49）", id)
		}
	}
	// 未取证的输出上限必须留 0（不编造）。
	if got := byID["kimi-k3"]; got.max != 0 {
		t.Fatalf("kimi-k3 maxTokens=%d，未取证必须留 0", got.max)
	}
}

type providerModel struct {
	ctx    int64
	max    int64
	img    bool
	reason bool
}

func TestStaticModelsReturnsCopy(t *testing.T) {
	a := StaticModels()
	a[0].ID = "tampered"
	if b := StaticModels(); b[0].ID == "tampered" {
		t.Fatal("StaticModels 必须返回副本：调用方改写不得污染共享快照")
	}
}

func TestFetchModelsIgnoresNetworkInTicket01(t *testing.T) {
	// 票 01 的 FetchModels 不联网：Origin 指向不可达地址也照样出 27 条。
	c := New()
	c.Origin = "http://127.0.0.1:1"
	ms, err := c.FetchModels(testAuth("dai-a", "k", ""))
	if err != nil {
		t.Fatalf("FetchModels err=%v", err)
	}
	if len(ms) != 27 {
		t.Fatalf("条目=%d", len(ms))
	}
	if _, err := c.FetchModels(nil); err == nil {
		t.Fatal("无凭据必须报错（否则会拿空 Key 去打上游）")
	}
}

func TestClassifyStatusTable(t *testing.T) {
	c := New()
	cases := []struct {
		status int
		body   string
		want   string
	}{
		{http.StatusTooManyRequests, `{"error":{"message":"quota exceeded"}}`, "soft_rate"},
		{http.StatusUnauthorized, `{"error":"auth_required"}`, "session_dead"},
		{http.StatusPaymentRequired, `{"error":"no theos"}`, "hard_credit"},
		{http.StatusNotFound, `{"error":"no such model"}`, "model_blocked"},
		{http.StatusInternalServerError, ``, "server"},
		{http.StatusBadRequest, `{"error":{"message":"bad"}}`, "client"},
		{http.StatusBadRequest, `{"error":{"code":"content_filter"}}`, "content_blocked"},
		{http.StatusFound, ``, "session_dead"}, // 302 跳登录页 = 凭据失效（见 classify.go 注释）
	}
	for _, c2 := range cases {
		if got := c.Classify(c2.status, c2.body).String(); got != c2.want {
			t.Fatalf("Classify(%d,%q)=%s，期望 %s", c2.status, c2.body, got, c2.want)
		}
	}
	// 429 必须优先于「余额」判据（不变量 15）。
	if got := c.Classify(http.StatusTooManyRequests, `insufficient quota`).String(); got != "soft_rate" {
		t.Fatalf("429+quota 被归成 %s，必须先判限流", got)
	}
}

func TestClassifyRedirectAsSessionDead(t *testing.T) {
	c := New()
	for _, status := range []int{301, 302, 307} {
		if got := classifyStatus(c, status, "").String(); got != "session_dead" {
			t.Fatalf("3xx(%d) 应归 session_dead，got %s", status, got)
		}
	}
	if got := classifyStatus(c, http.StatusOK, "").String(); got != "client" {
		t.Fatalf("2xx 走默认分类，got %s", got)
	}
}

func TestValidateKeyOKAndRejected(t *testing.T) {
	var seenKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenKey = r.Header.Get(HeaderKey)
		if r.URL.Path != PathModels {
			t.Errorf("校验必须打目录端点（零费用），实际 %s", r.URL.Path)
		}
		if seenKey == "bad" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"auth_required"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"glm-5.3-flash"}]}`))
	}))
	defer srv.Close()
	c := New()
	c.Origin = srv.URL

	uid, err := c.ValidateKey(t.Context(), "  good-key  ")
	if err != nil {
		t.Fatalf("合法 Key 应通过：%v", err)
	}
	if uid != KeyUID("good-key") {
		t.Fatalf("uid=%q，期望由 Key 派生", uid)
	}
	if seenKey != "good-key" {
		t.Fatalf("粘贴值未规整（发了 %q）", seenKey)
	}

	_, err = c.ValidateKey(t.Context(), "bad")
	if err == nil {
		t.Fatal("401 必须报错")
	}
	if !strings.Contains(err.Error(), "API Key 无效或已失效") {
		t.Fatalf("错误文案不可自助：%v", err)
	}

	if _, err := c.ValidateKey(t.Context(), "   "); err == nil {
		t.Fatal("空 Key 必须本地拒绝（不打上游）")
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0) }

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
