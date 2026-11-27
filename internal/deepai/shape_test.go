package deepai

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestShapeRejectReasonTable(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"旧文案", 400, `{"error":{"message":"This request is not one DeepAI Code sends."}}`, "copy_old"},
		{"新文案", 400, `{"error":{"message":"This request cannot be accepted."}}`, "copy_new"},
		{"只给码", 400, `{"error":{"code":"invalid_request_error"}}`, "invalid_request_error"},
		{"真实参数错误", 400, `{"error":{"message":"unknown field foo"}}`, ""},
		{"非 400 不算形状拒绝", 403, `{"error":{"code":"invalid_request_error"}}`, ""},
		{"429 不算", 429, `{"error":{"code":"invalid_request_error"}}`, ""},
	}
	for _, c := range cases {
		if got := shapeRejectReason(c.status, []byte(c.body)); got != c.want {
			t.Fatalf("%s: shapeRejectReason=%q，期望 %q", c.name, got, c.want)
		}
	}
}

// objKeys 按原文顺序取**顶层**键（decoder 逐 token 走，值整体跳过）。
func objKeys(t *testing.T, raw []byte) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		t.Fatalf("reshape 输出不是 JSON 对象：%s（%v）", string(raw), err)
	}
	var keys []string
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			t.Fatalf("读键失败：%v", err)
		}
		keys = append(keys, k.(string))
		if err := skipValue(dec); err != nil {
			t.Fatalf("跳过值失败：%v", err)
		}
	}
	return keys
}

// skipValue 消费一个完整的值（标量取一个 token；对象/数组读到匹配的闭括号）。
func skipValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if _, ok := tok.(json.Delim); !ok {
		return nil
	}
	depth := 1
	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
	}
	return nil
}

// TestReshapeBodyWhitelistOrderAndDropsUnknown 白名单重排：未知字段丢弃、顺序固定。
func TestReshapeBodyWhitelistOrderAndDropsUnknown(t *testing.T) {
	in := `{"user":"someone","seed":1,"messages":[{"role":"system","content":"S"},{"role":"user","content":"hi"}],"model":"glm-5.3-flash","n":2,"stream":true,"temperature":0.7}`
	out := reshapeBody([]byte(in))
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("输出不是合法 JSON：%s", string(out))
	}
	for _, gone := range []string{"user", "n"} {
		if _, ok := obj[gone]; ok {
			t.Fatalf("白名单外字段 %q 未被丢弃：%s", gone, string(out))
		}
	}
	keys := objKeys(t, out)
	want := []string{"model", "messages", "temperature", "stream", "seed"}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("键序=%v，期望按白名单序 %v", keys, want)
	}
	// 已存在 system ⇒ 绝不替换 / 不新增。
	if strings.Contains(string(out), shapeSystemPrompt) {
		t.Fatalf("已有 system 却注入了兜底 system：%s", string(out))
	}
	if strings.Count(string(out), `"role":"system"`) != 1 {
		t.Fatalf("system 条数不对：%s", string(out))
	}
}

// TestReshapeBodyAddsSystemWhenAbsent 缺 system/developer 才补**一条**，且前置。
func TestReshapeBodyAddsSystemWhenAbsent(t *testing.T) {
	in := `{"model":"glm-5.3-flash","messages":[{"role":"user","content":"hi"}]}`
	out := string(reshapeBody([]byte(in)))
	if strings.Count(out, `"role":"system"`) != 1 {
		t.Fatalf("必须恰好补一条 system：%s", out)
	}
	if !strings.Contains(out, shapeSystemPrompt) {
		t.Fatalf("system 内容不是兜底文案：%s", out)
	}
	if strings.Index(out, `"role":"system"`) > strings.Index(out, `"role":"user"`) {
		t.Fatalf("system 必须前置（桌面端顺序）：%s", out)
	}
	// developer（PrepBody 已归一成 system，这里再兜一层）同样视为「已有」。
	dev := `{"messages":[{"role":"developer","content":"D"},{"role":"user","content":"hi"}]}`
	devOut := string(reshapeBody([]byte(dev)))
	if strings.Contains(devOut, shapeSystemPrompt) {
		t.Fatalf("已有 developer 时不得再补 system：%s", devOut)
	}
}

// TestReshapeBodyBrokenInputUntouched 解析失败原样返回（宁可透传上游错误也不发拼坏的 body）。
func TestReshapeBodyBrokenInputUntouched(t *testing.T) {
	in := []byte(`not json`)
	if out := reshapeBody(in); string(out) != string(in) {
		t.Fatalf("坏输入被改写：%q", string(out))
	}
}

// shapeFallbackServer 构造「第一次 400 形状拒绝、第二次 200」的假上游。
func shapeFallbackServer(t *testing.T, rejectBody string, secondStatus int, secondBody string, hits *int32, bodies *[]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(hits, 1)
		raw, _ := io.ReadAll(r.Body)
		if bodies != nil {
			*bodies = append(*bodies, string(raw))
		}
		if n == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, rejectBody)
			return
		}
		w.WriteHeader(secondStatus)
		_, _ = io.WriteString(w, secondBody)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestChatStreamShapeFallbackSucceeds 裸请求（只有 user）第一次被拒、第二次成功：对调用方透明。
func TestChatStreamShapeFallbackSucceeds(t *testing.T) {
	var hits int32
	var bodies []string
	srv := shapeFallbackServer(t,
		`{"error":{"message":"This request is not one DeepAI Code sends."}}`,
		http.StatusOK,
		"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n",
		&hits, &bodies)
	c := New()
	c.Origin = srv.URL
	a := testAuth("dai-abc", "k", "")

	body := []byte(`{"model":"glm-5.3-flash","messages":[{"role":"user","content":"hi"}],"user":"codex"}`)
	logs := captureLog(t, func() {
		rc, status, errBody, err := c.ChatStream(a, body)
		if err != nil {
			t.Fatalf("ChatStream err=%v", err)
		}
		if status != http.StatusOK || rc == nil {
			t.Fatalf("兜底后应成功，status=%d rc=%v body=%s", status, rc, string(errBody))
		}
		_ = rc.Close()
	})
	if hits != 2 {
		t.Fatalf("请求数=%d，期望恰好 2（首次 + 兜底一次）", hits)
	}
	if !strings.Contains(logs, "deepai shape fallback uid=dai-abc") ||
		!strings.Contains(logs, "reason=copy_old") {
		t.Fatalf("首因未留痕（日志须可检索）：\n%s", logs)
	}
	// 第二次请求体：白名单外字段被丢、补了 system、仍带会话粘性所需的 messages。
	second := bodies[1]
	if strings.Contains(second, `"user":"codex"`) {
		t.Fatalf("兜底体未按白名单重排：%s", second)
	}
	if !strings.Contains(second, shapeSystemPrompt) {
		t.Fatalf("兜底体未补 system：%s", second)
	}
	if !strings.Contains(second, `"stream":true`) {
		t.Fatalf("兜底体丢了统一的流式标志：%s", second)
	}
}

// TestChatStreamShapeFallbackSecondFailurePassesThrough 二次仍失败：把**第二个**错误体原样
// 回传，总请求数恰好 2（不反复重试）。
func TestChatStreamShapeFallbackSecondFailurePassesThrough(t *testing.T) {
	var hits int32
	secondBody := `{"error":{"message":"This request cannot be accepted."},"trace":"second"}`
	srv := shapeFallbackServer(t,
		`{"error":{"message":"This request cannot be accepted."},"trace":"first"}`,
		http.StatusBadRequest, secondBody, &hits, nil)
	c := New()
	c.Origin = srv.URL

	var gotStatus int
	var gotBody []byte
	captureLog(t, func() {
		_, status, errBody, err := c.ChatStream(testAuth("dai-a", "k", ""), []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			t.Fatalf("3xx/4xx 不得返回传输层 error：%v", err)
		}
		gotStatus, gotBody = status, errBody
	})
	if hits != 2 {
		t.Fatalf("请求数=%d，期望恰好 2（只重试一次）", hits)
	}
	if gotStatus != http.StatusBadRequest {
		t.Fatalf("status=%d，期望 400", gotStatus)
	}
	if string(gotBody) != secondBody {
		t.Fatalf("回传的不是第二个错误体：%s", string(gotBody))
	}
}

// TestChatStreamNoRetryOnUnrelated400 非形状拒绝的 400 不重试（省一次无意义往返）。
func TestChatStreamNoRetryOnUnrelated400(t *testing.T) {
	var hits int32
	srv := shapeFallbackServer(t,
		`{"error":{"message":"unknown field foo"}}`,
		http.StatusOK, `{}`, &hits, nil)
	c := New()
	c.Origin = srv.URL
	_, status, _, err := c.ChatStream(testAuth("dai-a", "k", ""), []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if status != http.StatusBadRequest {
		t.Fatalf("status=%d，期望原样透传 400", status)
	}
	if hits != 1 {
		t.Fatalf("请求数=%d，非形状拒绝不得重试", hits)
	}
}
