package deepai

import (
	"bytes"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wild-work/internal/provider"
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

func TestFetchModelsNilAccountRejected(t *testing.T) {
	c := New()
	c.Origin = "http://127.0.0.1:1"
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

// ---- 票 02：动态目录 / 剔除 / 兜底 ----

// catalogServer 起一个假目录端点；handler 决定回什么，并记录收到的 Api-Key。
func catalogServer(t *testing.T, body string, status int, seenKey *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != PathModels {
			t.Errorf("目录必须打 %s，实际 %s", PathModels, r.URL.Path)
		}
		if seenKey != nil {
			*seenKey = r.Header.Get(HeaderKey)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// captureLog 截获一段时间内的日志（log 默认写 stderr），用于断言「跳过/兜底」留痕。
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)
	fn()
	return buf.String()
}

const catalogWithResponses = `{"models":[
 {"id":"glm-5.3-flash","name":"GLM 5.3 Flash","reasoning":true,"input":["text","image"],"contextWindow":320000,"maxTokens":131072,"api":""},
 {"id":"muse-ember","name":"Muse Ember","reasoning":true,"input":["text","image"],"contextWindow":320000,"maxTokens":200000},
 {"id":"gpt-6-astra","name":"GPT 6 Astra","reasoning":true,"input":["text","image"],"contextWindow":320000,"maxTokens":100000,"api":"openai-responses"},
 {"id":"minimax-m3","name":"MiniMax M3","reasoning":false,"input":["text"],"contextWindow":320000,"maxTokens":32000},
 {"id":"glm-5.3-flash","name":"dup","contextWindow":1}
],"default":"glm-5.3-flash"}`

// TestFetchModelsParsesAndSkipsResponsesOnly 上游目录必须「原样反映 + 剔除 responses-only + 留日志」。
func TestFetchModelsParsesAndSkipsResponsesOnly(t *testing.T) {
	var key string
	srv := catalogServer(t, catalogWithResponses, http.StatusOK, &key)
	c := New()
	c.Origin = srv.URL

	var ms []provider.ModelInfo
	logs := captureLog(t, func() {
		var err error
		ms, err = c.FetchModels(testAuth("dai-a", "key-xyz", ""))
		if err != nil {
			t.Fatalf("FetchModels err=%v", err)
		}
	})
	if key != "key-xyz" {
		t.Fatalf("目录请求未带账号凭据（Api-Key=%q）——不带一律 401", key)
	}
	ids := make([]string, 0, len(ms))
	for _, m := range ms {
		ids = append(ids, m.ID)
	}
	want := []string{"glm-5.3-flash", "muse-ember", "minimax-m3"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("目录=%v，期望 %v（剔除 responses-only + 去重，顺序保持上游序）", ids, want)
	}
	if !strings.Contains(logs, "skip responses-only model=gpt-6-astra") {
		t.Fatalf("剔除必须留可检索日志，实际日志：\n%s", logs)
	}
	// 字段映射：目录值原样透传，且标记「来自上游」。
	byID := map[string]provider.ModelInfo{}
	for _, m := range ms {
		byID[m.ID] = m
	}
	if m := byID["glm-5.3-flash"]; m.ContextWindow != 320000 || m.MaxTokens != 131072 ||
		!m.ContextFromAPI || !m.SupportsImages || !m.SupportsTools || !m.SupportsReasoning || m.Name != "GLM 5.3 Flash" {
		t.Fatalf("glm-5.3-flash 映射不对：%+v", m)
	}
	if m := byID["minimax-m3"]; m.SupportsImages || m.SupportsReasoning || m.MaxTokens != 32000 {
		t.Fatalf("纯文本/无思考模型被误标：%+v", m)
	}
	// 无 maxTokens 字段的条目不得被填成快照值（那是把快照冒充成实时值）。
	if m := byID["muse-ember"]; m.MaxTokens != 200000 {
		t.Fatalf("muse-ember maxTokens=%d，期望原样透传 200000", m.MaxTokens)
	}
}

// TestFetchModelsFallsBackToSnapshot 非账号维度的失败一律退回编译快照（27 条）并留日志。
func TestFetchModelsFallsBackToSnapshot(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		status int
	}{
		{"5xx", `{"error":"boom"}`, http.StatusInternalServerError},
		{"空目录", `{"models":[],"default":"glm-5.3-flash"}`, http.StatusOK},
		{"全是responses_only", `{"models":[{"id":"gpt-6-astra","api":"openai-responses"}]}`, http.StatusOK},
		{"不可解析", `not json`, http.StatusOK},
	}
	for _, c2 := range cases {
		srv := catalogServer(t, c2.body, c2.status, nil)
		c := New()
		c.Origin = srv.URL
		var ms []provider.ModelInfo
		logs := captureLog(t, func() {
			var err error
			ms, err = c.FetchModels(testAuth("dai-a", "k", ""))
			if err != nil {
				t.Fatalf("%s: 非账号维度失败不得报错（交由快照兜底），实际 %v", c2.name, err)
			}
		})
		if len(ms) != 27 {
			t.Fatalf("%s: 快照条目=%d，期望 27", c2.name, len(ms))
		}
		if !strings.Contains(logs, "deepai models: fallback to snapshot reason=") {
			t.Fatalf("%s: 兜底必须留日志，实际：\n%s", c2.name, logs)
		}
	}
}

// TestFetchModelsNetworkErrorFallsBack 传输层错误同样退回快照（换号无意义：同 host）。
func TestFetchModelsNetworkErrorFallsBack(t *testing.T) {
	c := New()
	c.Origin = "http://127.0.0.1:1" // 不可达
	var ms []provider.ModelInfo
	logs := captureLog(t, func() {
		var err error
		ms, err = c.FetchModels(testAuth("dai-a", "k", ""))
		if err != nil {
			t.Fatalf("网络失败应交由快照兜底，实际 %v", err)
		}
	})
	if len(ms) != 27 || !strings.Contains(logs, "reason=network") {
		t.Fatalf("网络失败未走快照：n=%d logs=%s", len(ms), logs)
	}
}

// TestFetchModelsUnauthorizedIsTypedError 401 是**账号维度**：必须报类型化错误，
// 让 server 层换下一个候选账号（R133）而不是把整条渠道钉在快照上。
func TestFetchModelsUnauthorizedIsTypedError(t *testing.T) {
	srv := catalogServer(t, `{"error":{"message":"auth_required"}}`, http.StatusUnauthorized, nil)
	c := New()
	c.Origin = srv.URL
	_, err := c.FetchModels(testAuth("dai-a", "k", ""))
	if err == nil {
		t.Fatal("401 必须返回错误，否则健康账号永远轮不到")
	}
	var perr *provider.Error
	if !errors.As(err, &perr) {
		t.Fatalf("必须是 *provider.Error（自愈链靠 errors.As 取分类，R79），实际 %T", err)
	}
	if perr.Kind != provider.ErrSessionDead {
		t.Fatalf("Kind=%s，期望 session_dead（走 R76 自愈通道，不 Disable）", perr.Kind)
	}
	if perr.Status != http.StatusUnauthorized {
		t.Fatalf("Status=%d", perr.Status)
	}
	// 3xx 同口径（上游 Key 失效会 302 跳登录页）。
	srv2 := catalogServer(t, ``, http.StatusFound, nil)
	c2 := New()
	c2.Origin = srv2.URL
	_, err = c2.FetchModels(testAuth("dai-a", "k", ""))
	if perr := (*provider.Error)(nil); !errors.As(err, &perr) || perr.Kind != provider.ErrSessionDead {
		t.Fatalf("3xx 必须归 session_dead，实际 %v", err)
	}
}
