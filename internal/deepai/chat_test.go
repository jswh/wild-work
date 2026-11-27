package deepai

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// sseReturn 构造一个只回固定 SSE 文本的假上游。
func sseReturn(body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	}))
}

func TestRewriteModelLineInsertsWhenMissing(t *testing.T) {
	in := []byte("data: {\"id\":\"gen-1\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n")
	out := rewriteModelLine(in, "deepai/glm-5.3-flash")
	var obj map[string]any
	payload := strings.TrimPrefix(strings.TrimSpace(string(out)), "data: ")
	if err := json.Unmarshal([]byte(payload), &obj); err != nil {
		t.Fatalf("改写后不是合法 JSON：%s（%v）", string(out), err)
	}
	if obj["model"] != "deepai/glm-5.3-flash" {
		t.Fatalf("model=%v，期望回填客户端全串", obj["model"])
	}
	// 其余字段必须原样（值不变）。
	choices, _ := obj["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices 被改坏：%s", string(out))
	}
	if !strings.HasSuffix(string(out), "\n") {
		t.Fatal("改写后必须保留行尾换行（否则 SSE 帧粘连）")
	}
}

func TestRewriteModelLineReplacesExisting(t *testing.T) {
	in := []byte("data: {\"model\":\"glm-5.3-flash\",\"choices\":[{\"delta\":{}}]}\n")
	out := string(rewriteModelLine(in, "deepai/glm-5.3-flash"))
	if !strings.Contains(out, `"model":"deepai/glm-5.3-flash"`) {
		t.Fatalf("裸名未被替换：%s", out)
	}
}

func TestRewriteModelLinePassThrough(t *testing.T) {
	cases := [][]byte{
		[]byte("data: [DONE]\n\n"),
		[]byte("\n"),
		[]byte(": keep-alive\n"),
		[]byte("event: ping\n"),
		[]byte("data: {\"choices\":[{\"delta\":{}}],\"model\":\"deepai/x\"}\n"), // 已是目标值 ⇒ 字节级透传
	}
	for _, in := range cases {
		want := in
		if string(in) == "data: {\"choices\":[{\"delta\":{}}],\"model\":\"deepai/x\"}\n" {
			want = in
		}
		if got := rewriteModelLine(in, "deepai/x"); string(got) != string(want) {
			t.Fatalf("无需改写的行必须逐字节透传：in=%q got=%q", string(in), string(got))
		}
	}
	// 非对话帧（无 choices，如 usage-only 帧）也不改。
	usageOnly := []byte("data: {\"id\":\"x\",\"usage\":{\"prompt_tokens\":1}}\n")
	if got := rewriteModelLine(usageOnly, "deepai/x"); string(got) != string(usageOnly) {
		t.Fatalf("usage 帧被改写：%q", string(got))
	}
}

func TestRewriteModelLineKeepsToolArguments(t *testing.T) {
	// tool_calls 参数里也可能出现 "model" 字样：字符串替换式改写会把它改坏，本实现不会。
	in := []byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"model\\\":\\\"x\\\"}\"}}]}}]}\n")
	out := string(rewriteModelLine(in, "deepai/glm-5.3-flash"))
	if !strings.Contains(out, `\"model\":\"x\"`) {
		t.Fatalf("工具参数被改坏：%s", out)
	}
	if !strings.Contains(out, `"model":"deepai/glm-5.3-flash"`) {
		t.Fatalf("顶层 model 未回填：%s", out)
	}
}

// TestChatStreamWire 端到端校验一次请求的形状：管线改写 + 头族 + 会话粘性。
func TestChatStreamWire(t *testing.T) {
	var gotBody map[string]any
	var hdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr = r.Header.Clone()
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	c := New()
	c.Origin = srv.URL
	a := testAuth("dai-abc", "key-1", "ABCD-5678")

	body := []byte(`{"model":"glm-5.3-flash","stream":false,"messages":[{"role":"developer","content":"be nice"},{"role":"user","content":"你好"}]}`)
	rc, status, errBody, err := c.ChatStream(a, body)
	if err != nil || rc == nil {
		t.Fatalf("ChatStream err=%v status=%d body=%s", err, status, string(errBody))
	}
	defer rc.Close()

	if hdr.Get(HeaderKey) != "key-1" {
		t.Fatalf("Api-Key 头缺失：%v", hdr)
	}
	if hdr.Get("Authorization") != "" {
		t.Fatal("不得发 Authorization")
	}
	if hdr.Get(HeaderAffinity) == "" {
		t.Fatal("对话请求必须带会话粘性头")
	}
	// 统一管线：强制流式 + 补 include_usage + developer→system。
	if gotBody["stream"] != true {
		t.Fatalf("stream=%v，管线必须强制 true", gotBody["stream"])
	}
	if so, _ := gotBody["stream_options"].(map[string]any); so["include_usage"] != true {
		t.Fatalf("stream_options=%v，必须补 include_usage", gotBody["stream_options"])
	}
	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) < 1 {
		t.Fatalf("messages 丢失：%v", gotBody)
	}
	if role, _ := msgs[0].(map[string]any)["role"].(string); role != "system" {
		t.Fatalf("首条 role=%q，developer 必须归一成 system", role)
	}

	// 流式回放：经 Stream 的回填逻辑后 model 必须是客户端全串、[DONE] 保留。
	rec := httptest.NewRecorder()
	if _, err := c.Stream(rec, rc, "deepai/glm-5.3-flash"); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	out := rec.Body.String()
	if !strings.Contains(out, `"model":"deepai/glm-5.3-flash"`) {
		t.Fatalf("流里没有回填 model：%s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("[DONE] 丢失：%s", out)
	}
}

func TestChatStreamErrorPassthrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"slow down"}}`)
	}))
	defer srv.Close()
	c := New()
	c.Origin = srv.URL
	rc, status, errBody, err := c.ChatStream(testAuth("dai-a", "k", ""), []byte(`{"messages":[]}`))
	if err != nil {
		t.Fatalf("≥400 不得返回传输层 error：%v", err)
	}
	if rc != nil {
		t.Fatal("≥400 时不得返回 body reader")
	}
	if status != http.StatusTooManyRequests || !strings.Contains(string(errBody), "slow down") {
		t.Fatalf("状态/错误体未原样透传：status=%d body=%s", status, string(errBody))
	}
}

func TestStreamClientHasNoTotalTimeout(t *testing.T) {
	c := New()
	if c.StreamHTTP.Timeout != 0 {
		t.Fatalf("StreamHTTP.Timeout=%v，流式 client 不得设总超时（R35）", c.StreamHTTP.Timeout)
	}
	if c.HTTP.Timeout <= 0 {
		t.Fatal("非流式 client 必须保留总超时兜底")
	}
	if c.IdleTimeout() != defaultIdleTimeout {
		t.Fatalf("空闲上限=%v，期望 %v", c.IdleTimeout(), defaultIdleTimeout)
	}
	if c.HTTP.Transport != c.StreamHTTP.Transport {
		t.Fatal("两个 client 必须共用 Transport（否则连接池翻倍）")
	}
}

func TestAggregateFromSSE(t *testing.T) {
	sse := sseReturn("data: {\"choices\":[{\"delta\":{\"content\":\"he\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"llo\"}}]}\n\ndata: [DONE]\n\n")
	defer sse.Close()
	resp, err := http.Get(sse.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	c := New()
	out, err := c.Aggregate(resp.Body, "deepai/glm-5.3-flash")
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if out["model"] != "deepai/glm-5.3-flash" {
		t.Fatalf("model=%v，期望回填全串", out["model"])
	}
	choices, _ := out["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("聚合结果没有 choices：%v", out)
	}
	msg, _ := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "hello" {
		t.Fatalf("content=%v，期望 hello", msg["content"])
	}
}

// TestModelRewriterStreamingOrder 校验改写器不会攒批（逐行吐出、不易序）。
func TestModelRewriterStreamingOrder(t *testing.T) {
	src := "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\ndata: [DONE]\n\n"
	r := bufio.NewScanner(rewriteModelReader(strings.NewReader(src), "deepai/x"))
	var lines []string
	for r.Scan() {
		lines = append(lines, r.Text())
	}
	if len(lines) < 4 {
		t.Fatalf("行数=%d，流被压缩：%v", len(lines), lines)
	}
	if lines[1] != "" {
		t.Fatalf("空行（帧分隔符）必须保留：%q", lines[1])
	}
}

// TestChatStreamRedirectIsErrorStatus 302（Key 失效跳登录页）必须当失败回传，
// 不得把 HTML 登录页当 SSE 流读（否则空 delta + 永冷却不下来的账号）。
func TestChatStreamRedirectIsErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://api.deepai.org/login")
		w.WriteHeader(http.StatusFound)
		_, _ = io.WriteString(w, "<html>login</html>")
	}))
	defer srv.Close()
	c := New()
	c.Origin = srv.URL
	rc, status, errBody, err := c.ChatStream(testAuth("dai-a", "k", ""), []byte(`{"messages":[]}`))
	if err != nil {
		t.Fatalf("3xx 不得返回传输层 error（要交给 handler 分类）：%v", err)
	}
	if rc != nil || status != http.StatusFound {
		t.Fatalf("rc=%v status=%d，期望 nil/302", rc, status)
	}
	if !strings.Contains(string(errBody), "login") {
		t.Fatalf("错误体未透传：%q", string(errBody))
	}
	if got := c.Classify(status, string(errBody)).String(); got != "session_dead" {
		t.Fatalf("302 应归 session_dead，got %s", got)
	}
}

// ---- 票 03：思考链补写（reasoning → reasoning_content）----

// TestFrameBackfillsReasoningContent delta.reasoning 非空且无 reasoning_content ⇒ 补一份。
func TestFrameBackfillsReasoningContent(t *testing.T) {
	in := []byte(`data: {"choices":[{"index":0,"delta":{"reasoning":"先想一步"}}]}` + "\n")
	out := rewriteModelLine(in, "deepai/glm-5.3-flash")
	if !bytes.Contains(out, []byte(`"reasoning_content":"先想一步"`)) {
		t.Fatalf("未补 reasoning_content：%s", out)
	}
	// 原字段必须保留（下游可能也读它；覆盖语义不是本票的事）。
	if !bytes.Contains(out, []byte(`"reasoning":"先想一步"`)) {
		t.Fatalf("原 reasoning 被删：%s", out)
	}
	// model 同时回填。
	if !bytes.Contains(out, []byte(`"model":"deepai/glm-5.3-flash"`)) {
		t.Fatalf("model 未回填：%s", out)
	}
	if !bytes.HasSuffix(out, []byte("\n")) {
		t.Fatal("行尾换行丢失（SSE 帧会粘连）")
	}
}

// TestFrameDoesNotOverwriteExistingReasoningContent 上游已给 reasoning_content ⇒ 绝不覆盖。
func TestFrameDoesNotOverwriteExistingReasoningContent(t *testing.T) {
	in := []byte(`data: {"model":"deepai/glm-5.3-flash","choices":[{"delta":{"reasoning":"上游的","reasoning_content":"已有的"}}]}` + "\n")
	out := rewriteModelLine(in, "deepai/glm-5.3-flash")
	if string(out) != string(in) {
		t.Fatalf("无需改写的帧必须逐字节透传，实际：%s", out)
	}
}

// TestFrameIgnoresEmptyOrNonStringReasoning 空串 / 非字符串的 reasoning 不补（不编造内容）。
func TestFrameIgnoresEmptyOrNonStringReasoning(t *testing.T) {
	for _, delta := range []string{`{"reasoning":""}`, `{"reasoning":null}`, `{"reasoning":["x"]}`} {
		in := []byte(`data: {"model":"deepai/x","choices":[{"delta":` + delta + `}]}` + "\n")
		if out := rewriteModelLine(in, "deepai/x"); string(out) != string(in) {
			t.Fatalf("delta=%s 被改写成 %s（不得编造 reasoning_content）", delta, out)
		}
	}
}

// TestFrameNonReasoningFrameByteIdentical 不含 reasoning 的普通帧（model 已正确）逐字节透传。
func TestFrameNonReasoningFrameByteIdentical(t *testing.T) {
	in := []byte(`data: {"model":"deepai/x",  "choices":[{"delta":{"content":"hi"}}]}` + "\r\n")
	if out := rewriteModelLine(in, "deepai/x"); string(out) != string(in) {
		t.Fatalf("普通帧被改写：%q", string(out))
	}
}

// TestStreamEndToEndReasoningBackfill 端到端：SSE 里的 reasoning 帧对下游可见为
// reasoning_content，且 [DONE] 依然原样。
func TestStreamEndToEndReasoningBackfill(t *testing.T) {
	srv := sseReturn("data: {\"choices\":[{\"delta\":{\"reasoning\":\"思考\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"答案\"}}]}\n\ndata: [DONE]\n\n")
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	rec := httptest.NewRecorder()
	if _, err := New().Stream(rec, resp.Body, "deepai/glm-5.3-flash"); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	out := rec.Body.String()
	if !strings.Contains(out, `"reasoning_content":"思考"`) {
		t.Fatalf("下游看不到思考内容：%s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("[DONE] 丢失：%s", out)
	}
}

// TestAggregateBackfillsReasoning 非流式：message.reasoning → message.reasoning_content。
func TestAggregateBackfillsReasoning(t *testing.T) {
	// JSON 形态
	jsonBody := `{"id":"gen-1","model":"glm-5.3-flash","choices":[{"message":{"role":"assistant","content":"答","reasoning":"想"}}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, jsonBody)
	}))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	out, err := New().Aggregate(resp.Body, "deepai/glm-5.3-flash")
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	msg := out["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["reasoning_content"] != "想" {
		t.Fatalf("JSON 聚合未补 reasoning_content：%+v", msg)
	}
	if out["model"] != "deepai/glm-5.3-flash" {
		t.Fatalf("model 未回填：%v", out["model"])
	}

	// SSE 形态（管线强制流式 ⇒ 非流式客户端也会拿到 SSE）
	sse := sseReturn("data: {\"choices\":[{\"delta\":{\"reasoning\":\"想\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"答\"}}]}\n\ndata: [DONE]\n\n")
	defer sse.Close()
	resp2, err := http.Get(sse.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp2.Body.Close()
	out2, err := New().Aggregate(resp2.Body, "deepai/glm-5.3-flash")
	if err != nil {
		t.Fatalf("Aggregate(SSE): %v", err)
	}
	msg2 := out2["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg2["reasoning_content"] != "想" {
		t.Fatalf("SSE 聚合未补 reasoning_content：%+v", msg2)
	}
}

// TestAggregateKeepsExistingReasoningContent 聚合也不覆盖上游已给的 reasoning_content。
func TestAggregateKeepsExistingReasoningContent(t *testing.T) {
	resp := map[string]any{
		"choices": []any{map[string]any{
			"message": map[string]any{"reasoning": "上游", "reasoning_content": "已有"},
		}},
	}
	finalizeAggregate(resp, "deepai/x")
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["reasoning_content"] != "已有" {
		t.Fatalf("覆盖了已有的 reasoning_content：%v", msg["reasoning_content"])
	}
}
