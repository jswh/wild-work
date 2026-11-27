package deepai

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"wild-work/internal/auth"
)

func testAuth(uid, key, machine string) *auth.Auth {
	return &auth.Auth{Kind: KindName, UID: uid, AccessToken: key, MachineID: machine}
}

func TestApplyHeadersDesktopFingerprint(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://x/deepai_code/models", nil)
	a := testAuth("dai-abc", "key-123", "ABCD-1234")
	applyHeaders(req, a, "")

	// 桌面端 Node fetch 的默认身份：三条件头 + node UA。
	if got := req.Header.Get("User-Agent"); got != "node" {
		t.Fatalf("User-Agent=%q，期望 node（桌面端 fetch 的默认 UA）", got)
	}
	if got := req.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type=%q，无 body 的 GET 也必须带 application/json", got)
	}
	// 鉴权走 Api-Key，绝不发 Authorization。
	if got := req.Header.Get(HeaderKey); got != "key-123" {
		t.Fatalf("%s=%q，期望 key-123", HeaderKey, got)
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Fatalf("不得发 Authorization（上游只认 Api-Key），实际 %q", got)
	}
	// 浏览器指纹头一律不发：带了反而与真实客户端不一致。
	for _, h := range []string{"sec-ch-ua", "sec-ch-ua-mobile", "sec-ch-ua-platform"} {
		if v := req.Header.Get(h); v != "" {
			t.Fatalf("不得发 %s，实际 %q", h, v)
		}
	}
	if got := req.Header.Get(HeaderLane); got != LaneVersion {
		t.Fatalf("%s=%q，期望 %q", HeaderLane, got, LaneVersion)
	}
	if got := req.Header.Get(HeaderProof); got != "" {
		t.Fatalf("本仓不发 Proof 头（上游不校验），实际 %q", got)
	}
	// 机器指纹 = sha256(小写原始 UUID)。
	sum := sha256.Sum256([]byte("abcd-1234"))
	if want := hex.EncodeToString(sum[:]); req.Header.Get(HeaderMachine) != want {
		t.Fatalf("%s=%q，期望 %q（sha256(小写原始)）", HeaderMachine, req.Header.Get(HeaderMachine), want)
	}
	// 实例号：32 位小写 hex，按账号派生。
	inst := req.Header.Get(HeaderInstance)
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(inst) {
		t.Fatalf("%s=%q，期望 32 位小写 hex", HeaderInstance, inst)
	}
	other, _ := http.NewRequest(http.MethodGet, "https://x", nil)
	applyHeaders(other, testAuth("dai-xyz", "key-123", "abcd-1234"), "")
	if other.Header.Get(HeaderInstance) == inst {
		t.Fatal("不同账号不得共用同一个实例号（等于自报「同一客户端进程」）")
	}
	// 非对话请求不带会话粘性头。
	if got := req.Header.Get(HeaderAffinity); got != "" {
		t.Fatalf("目录/余额请求不得带 %s，实际 %q", HeaderAffinity, got)
	}
}

func TestApplyHeadersEmptyMachineSkipsHeader(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://x", nil)
	applyHeaders(req, testAuth("dai-abc", "key-123", ""), "")
	if got := req.Header.Get(HeaderMachine); got != "" {
		t.Fatalf("没有原始机器标识时不得发 %s（发空值等于自报无指纹），实际 %q", HeaderMachine, got)
	}
}

func TestFirstUserText(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"裸串", `{"messages":[{"role":"system","content":"sys"},{"role":"user","content":"你好"}]}`, "你好"},
		{"分片", `{"messages":[{"role":"user","content":[{"type":"text","text":"看图"},{"type":"image_url","image_url":{"url":"data:x"}}]}]}`, "看图"},
		{"跳过非 user", `{"messages":[{"role":"assistant","content":"先答"},{"role":"user","content":"再问"}]}`, "再问"},
		{"纯图 user", `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:x"}}]}]}`, ""},
		{"无 user", `{"messages":[{"role":"system","content":"sys"}]}`, ""},
		{"坏 JSON", `{`, ""},
	}
	for _, c := range cases {
		if got := firstUserText([]byte(c.body)); got != c.want {
			t.Fatalf("%s: firstUserText=%q，期望 %q", c.name, got, c.want)
		}
	}
}

func TestAffinityStableAndAccountScoped(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"同一段对话"}]}`)
	a1 := affinityOf(testAuth("dai-a", "k", ""), body)
	a2 := affinityOf(testAuth("dai-a", "k", ""), body)
	if a1 == "" || a1 != a2 {
		t.Fatalf("同一账号同一会话锚点必须派生同一粘性值，got %q/%q", a1, a2)
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(a1) {
		t.Fatalf("粘性值形态=%q，期望 32 位小写 hex", a1)
	}
	// 掺 uid：同一段对话被不同账号服务时不得共用会话 id。
	if a1 == affinityOf(testAuth("dai-b", "k", ""), body) {
		t.Fatal("不同账号不得派生出相同的会话粘性值")
	}
	// 换一轮（末条 user 变化、首条不变）仍应稳定：锚点取首条 user。
	turn2 := []byte(`{"messages":[{"role":"user","content":"同一段对话"},{"role":"assistant","content":"答"},{"role":"user","content":"继续"}]}`)
	if a1 != affinityOf(testAuth("dai-a", "k", ""), turn2) {
		t.Fatal("追加轮次后首条 user 不变 ⇒ 粘性值必须不变")
	}
	if got := affinityOf(nil, body); got != "" {
		t.Fatalf("无账号时不得派生粘性值，got %q", got)
	}
}

func TestToolArgumentsModelNotTouchedByHeaderCode(t *testing.T) {
	// 头族代码只读 messages，不碰 tool_calls.arguments（防「参数里的 model 被改坏」回归）。
	body := []byte(`{"messages":[{"role":"user","content":"x"}],"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}]}`)
	if got := strings.TrimSpace(firstUserText(body)); got != "x" {
		t.Fatalf("firstUserText=%q", got)
	}
}
