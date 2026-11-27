package deepai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPKCEPairShape verifier 43 字符、challenge = base64url(sha256(verifier)) 且两者都无填充。
func TestPKCEPairShape(t *testing.T) {
	verifier, challenge, err := NewPKCE()
	if err != nil {
		t.Fatalf("NewPKCE: %v", err)
	}
	if len(verifier) != 43 || len(challenge) != 43 {
		t.Fatalf("verifier/challenge 长度=%d/%d，期望都是 43", len(verifier), len(challenge))
	}
	if !ValidPKCEVerifier(verifier) {
		t.Fatalf("verifier 形态非法：%q", verifier)
	}
	if got := PKCEChallenge(verifier); got != challenge {
		t.Fatalf("challenge 与 verifier 不配对：%q vs %q", got, challenge)
	}
	// RFC 7636 附录 B 的示例向量（口径锁死：SHA-256 + base64url 无填充）。
	if got := PKCEChallenge("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"); got != "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" {
		t.Fatalf("S256 口径与 RFC 向量不符：%q", got)
	}
	v2, _, err := NewPKCE()
	if err != nil || v2 == verifier {
		t.Fatalf("两次 PKCE 应互不相同：%q / %q (err=%v)", verifier, v2, err)
	}
	if ValidPKCEVerifier("xx") {
		t.Fatal("长度非法的 verifier 不得通过")
	}
}

// TestStateIsLongRandomHex 一次性 state：64 字符 hex、每次不同。
func TestStateIsLongRandomHex(t *testing.T) {
	a, err := NewState()
	if err != nil {
		t.Fatalf("NewState: %v", err)
	}
	b, _ := NewState()
	if len(a) != 64 || strings.Trim(a, "0123456789abcdef") != "" {
		t.Fatalf("state 形态不对：%q", a)
	}
	if a == b {
		t.Fatal("两次 state 撞了")
	}
}

// TestSignInURLHasAllParams 授权页 URL 四个参数齐全（含恒定发送的 PKCE 两参）。
func TestSignInURLHasAllParams(t *testing.T) {
	c := New()
	c.Origin = "https://api.example.test/"
	verifier, challenge, _ := NewPKCE()
	state, _ := NewState()
	u := c.SignInURL(state, 43123, challenge)
	for _, want := range []string{
		"https://api.example.test/deepai_code/desktop_auth/?state=" + state,
		"&port=43123",
		"&code_challenge=" + challenge,
		"&code_challenge_method=S256",
	} {
		if !strings.Contains(u, want) {
			t.Fatalf("授权 URL 缺 %q：%s", want, u)
		}
	}
	if strings.Contains(u, "//deepai_code") {
		t.Fatalf("origin 尾部斜杠没处理：%s", u)
	}
	_ = verifier
}

// TestExchangeLoginCodeRoundTrip 兑换请求体带 code + code_verifier，响应解析 key/username/isPro。
func TestExchangeLoginCodeRoundTrip(t *testing.T) {
	var gotPath, gotBody, gotKeyHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKeyHeader = r.Header.Get(HeaderKey)
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		_, _ = io.WriteString(w, `{"api_key":" 3f2a-key ","username":"alice","is_pro":true}`)
	}))
	defer srv.Close()
	c := New()
	c.Origin = srv.URL
	cred, err := c.ExchangeLoginCode(context.Background(), "code-1", "verifier-1")
	if err != nil {
		t.Fatalf("ExchangeLoginCode: %v", err)
	}
	if gotPath != PathExchangeLogin {
		t.Fatalf("兑换路径=%q，期望 %q", gotPath, PathExchangeLogin)
	}
	if gotKeyHeader != "" {
		t.Fatal("登录兑换时还没有账号凭据，不得带 Api-Key 头")
	}
	var sent map[string]string
	if err := json.Unmarshal([]byte(gotBody), &sent); err != nil {
		t.Fatalf("请求体不是 JSON：%s", gotBody)
	}
	if sent["code"] != "code-1" || sent["code_verifier"] != "verifier-1" {
		t.Fatalf("兑换请求体不对：%v", sent)
	}
	if cred.APIKey != "3f2a-key" || cred.Username != "alice" || !cred.IsPro {
		t.Fatalf("响应解析不对：%+v", cred)
	}
}

// TestExchangeLoginCodeErrors 400 PKCE 不匹配 / 缺 Key 都要给可行动的文案。
func TestExchangeLoginCodeErrors(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"pkce", 400, `{"error":"Invalid sign-in proof."}`, "PKCE 不匹配"},
		{"expired", 400, `{"error":"code not found"}`, "授权码无效或已过期"},
		{"server", 502, `upstream`, "换取 API Key 失败"},
	}
	for _, c2 := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c2.status)
			_, _ = io.WriteString(w, c2.body)
		}))
		c := New()
		c.Origin = srv.URL
		_, err := c.ExchangeLoginCode(context.Background(), "c", "v")
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), c2.want) {
			t.Fatalf("%s: err=%v，期望含 %q", c2.name, err, c2.want)
		}
	}

	// 上游 2xx 但没给 Key：不能当成成功（半成品凭据落盘后不可用）。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"username":"x"}`)
	}))
	defer srv.Close()
	c := New()
	c.Origin = srv.URL
	if _, err := c.ExchangeLoginCode(context.Background(), "c", "v"); err == nil {
		t.Fatal("没有 api_key 时不得成功")
	}
	if _, err := c.ExchangeLoginCode(context.Background(), "  ", "v"); err == nil {
		t.Fatal("空授权码应在本地被拒（不打上游）")
	}
}
