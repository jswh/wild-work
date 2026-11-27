package deepai

import (
	"bufio"
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
