package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wild-work/internal/deepai"
	logindeepai "wild-work/internal/login_deepai"
	"wild-work/internal/provider"
)

// authProxyOf 读凭据文件里的 proxy 字段。
func authProxyOf(t *testing.T, dir, uid string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "deepai-"+uid+".json"))
	if err != nil {
		t.Fatalf("read auth: %v", err)
	}
	var doc struct {
		Auth struct {
			Proxy string `json:"proxy"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("auth json: %v", err)
	}
	return doc.Auth.Proxy
}

// TestDeepAISubmitNormalizesProxy 粘贴 Key 时填的出口被规整后落盘（host:port 补 http://）。
func TestDeepAISubmitNormalizesProxy(t *testing.T) {
	srv := anyKeyUpstream(t)
	a, _, dir := newDeepAIApp(t, srv.URL)
	uid, _, err := a.SubmitDeepAIKey("key-p", " 127.0.0.1:1080 ")
	if err != nil {
		t.Fatalf("SubmitDeepAIKey: %v", err)
	}
	if got := authProxyOf(t, dir, uid); got != "http://127.0.0.1:1080" {
		t.Fatalf("proxy=%q，期望规整为 http://127.0.0.1:1080", got)
	}
}

// TestDeepAISubmitRejectsBadProxy 代理串非法 ⇒ 当场报错且不落盘（不得入库后才炸）。
func TestDeepAISubmitRejectsBadProxy(t *testing.T) {
	srv := anyKeyUpstream(t)
	a, _, dir := newDeepAIApp(t, srv.URL)
	_, _, err := a.SubmitDeepAIKey("key-p", "ftp://nope:21")
	if err == nil || !strings.Contains(err.Error(), "代理配置有误") {
		t.Fatalf("非法代理必须报「代理配置有误」，实际 %v", err)
	}
	if got := deepaiAuthFiles(t, dir); len(got) != 0 {
		t.Fatalf("校验失败不得落盘，实际 %v", got)
	}
}

// TestDeepAISubmitKeepsExistingProxy 重贴凭据（不填出口）不得把已配的出口冲掉。
func TestDeepAISubmitKeepsExistingProxy(t *testing.T) {
	srv := anyKeyUpstream(t)
	a, _, dir := newDeepAIApp(t, srv.URL)
	uid, _, err := a.SubmitDeepAIKey("key-p", "http://10.1.2.3:8080")
	if err != nil {
		t.Fatalf("首次: %v", err)
	}
	if _, _, err := a.SubmitDeepAIKey("key-p", ""); err != nil {
		t.Fatalf("重贴: %v", err)
	}
	if got := authProxyOf(t, dir, uid); got != "http://10.1.2.3:8080" {
		t.Fatalf("重贴凭据把出口冲掉了：%q", got)
	}
}

// TestChangeAccountProxyWiring 改出口接口：只对 deepai 开放、非法串拒收、改后落盘并可清空。
func TestChangeAccountProxyWiring(t *testing.T) {
	srv := anyKeyUpstream(t)
	a, _, dir := newDeepAIApp(t, srv.URL)
	uid, _, err := a.SubmitDeepAIKey("key-p", "")
	if err != nil {
		t.Fatalf("先加号: %v", err)
	}

	// 只对「有出口语义」的渠道开放（别的渠道写进去是静默无效，比没有更坏）。
	if err := a.ChangeAccountProxy(provider.Comate.String(), uid, "http://1.2.3.4:80"); err == nil {
		t.Fatal("非 deepai 渠道必须拒绝改出口")
	}
	// 非法串拒收（不得静默清空/静默保留）。
	if err := a.ChangeAccountProxy(provider.DeepAI.String(), uid, "socks4://x:1"); err == nil ||
		!strings.Contains(err.Error(), "代理配置有误") {
		t.Fatalf("非法代理必须报错，实际 %v", err)
	}
	// 未找到账号。
	if err := a.ChangeAccountProxy(provider.DeepAI.String(), "dai-nope", "http://1.2.3.4:80"); err == nil {
		t.Fatal("不存在的 uid 必须报错")
	}
	// 正常改。
	if err := a.ChangeAccountProxy(provider.DeepAI.String(), uid, "http://9.9.9.9:3128"); err != nil {
		t.Fatalf("改出口: %v", err)
	}
	if got := authProxyOf(t, dir, uid); got != "http://9.9.9.9:3128" {
		t.Fatalf("改后未落盘：%q", got)
	}
	// 清空 = 直连。
	if err := a.ChangeAccountProxy(provider.DeepAI.String(), uid, "  "); err != nil {
		t.Fatalf("清空: %v", err)
	}
	if got := authProxyOf(t, dir, uid); got != "" {
		t.Fatalf("清空后仍留着出口：%q", got)
	}
}

// TestProxyCapableKindWhitelist 出口语义白名单目前只有 deepai（新增渠道必须显式登记）。
func TestProxyCapableKindWhitelist(t *testing.T) {
	if !proxyCapableKind(provider.DeepAI) {
		t.Fatal("deepai 必须支持账号出口")
	}
	for _, k := range []provider.Kind{provider.WorkBuddy, provider.Comate, provider.Cline, provider.ChatGPT} {
		if proxyCapableKind(k) {
			t.Fatalf("%s 不该被登记为有出口语义（它的 Transport 不读 Proxy 字段）", k)
		}
	}
}

// TestPersistKeepsProxyAcrossLoginEntries 三条入口都保留/接受出口（登录路径用 start 时暂存的值）。
func TestPersistKeepsProxyAcrossLoginEntries(t *testing.T) {
	srv := anyKeyUpstream(t)
	a, _, dir := newDeepAIApp(t, srv.URL)
	// 模拟 /api/login/start 里填的代理（登录时账号还不存在，先暂存）。
	a.muLogin.Lock()
	a.loginProxy = "http://7.7.7.7:1080"
	a.muLogin.Unlock()
	if err := a.completeDeepAILogin(logindeepai.Result{APIKey: "key-login", Username: "alice"}); err != nil {
		t.Fatalf("登录落盘: %v", err)
	}
	files := deepaiAuthFiles(t, dir)
	if len(files) != 1 {
		t.Fatalf("应落一个文件，实际 %v", files)
	}
	var uid string
	for _, f := range files {
		uid = strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), "deepai-"), ".json")
	}
	if got := authProxyOf(t, dir, uid); got != "http://7.7.7.7:1080" {
		t.Fatalf("登录路径没把出口落盘：%q", got)
	}
	_ = deepai.ExtraUsername
}
