package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wild-work/internal/deepai"
	logindeepai "wild-work/internal/login_deepai"
	"wild-work/internal/provider"
)

// anyKeyUpstream 接受任意非空 Api-Key 的目录端点（登录/导入测试里要换着 Key 用）。
func anyKeyUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != deepai.PathModels {
			t.Errorf("校验只能打目录端点（零费用），实际 %s", r.URL.Path)
		}
		if strings.TrimSpace(r.Header.Get(deepai.HeaderKey)) == "" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"auth_required"}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"glm-5.3-flash"}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func deepaiAuthFiles(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "deepai-*.json"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	return matches
}

// TestDeepAIThreeEntriesSameUID 三条入口对同一账号产出同一 uid / 同一文件。
func TestDeepAIThreeEntriesSameUID(t *testing.T) {
	srv := anyKeyUpstream(t)
	a, _, dir := newDeepAIApp(t, srv.URL)

	// 入口 1：粘贴 Key（username 未知 ⇒ 按 Key 派生）。
	uid1, _, err := a.SubmitDeepAIKey("key-same", "")
	if err != nil {
		t.Fatalf("粘贴 Key: %v", err)
	}
	// 入口 3：桌面导入（同一个 Key + username）⇒ 必须复用同一条。
	secrets := filepath.Join(dir, "desktop", "userdata", "secrets")
	if err := os.MkdirAll(secrets, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(secrets, "deepai-account.bin"),
		[]byte(`{"apiKey":"key-same","username":"alice","isPro":false}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	uid3, _, err := a.ImportDeepAIDesktop(filepath.Join(dir, "desktop"), "")
	if err != nil {
		t.Fatalf("桌面导入: %v", err)
	}
	// 入口 2：浏览器登录（同 Key 同 username）⇒ 仍是同一条。
	if err := a.completeDeepAILogin(logindeepai.Result{APIKey: "key-same", Username: "alice"}); err != nil {
		t.Fatalf("浏览器登录落盘: %v", err)
	}
	if uid1 != uid3 {
		t.Fatalf("三入口 uid 不一致：粘贴=%s 桌面=%s", uid1, uid3)
	}
	if got := deepaiAuthFiles(t, dir); len(got) != 1 {
		t.Fatalf("同一账号应只有一个凭据文件，实际 %d 个：%v", len(got), got)
	}
}

// TestDeepAILoginSameUsernameNewKeyOverwrites 同一 username 换 Key ⇒ 覆盖同一条（不新增）。
func TestDeepAILoginSameUsernameNewKeyOverwrites(t *testing.T) {
	srv := anyKeyUpstream(t)
	a, _, dir := newDeepAIApp(t, srv.URL)

	if err := a.completeDeepAILogin(logindeepai.Result{APIKey: "key-old", Username: "alice"}); err != nil {
		t.Fatalf("首次登录: %v", err)
	}
	first := deepaiAuthFiles(t, dir)
	if len(first) != 1 {
		t.Fatalf("首次登录应落一个文件，实际 %v", first)
	}
	// 重登换了 Key：同一个 username ⇒ 覆盖自己那条（否则重复号把每日额度拆半）。
	if err := a.completeDeepAILogin(logindeepai.Result{APIKey: "key-new", Username: "alice", IsPro: true}); err != nil {
		t.Fatalf("重登: %v", err)
	}
	files := deepaiAuthFiles(t, dir)
	if len(files) != 1 || files[0] != first[0] {
		t.Fatalf("重登必须覆盖同一条，实际文件 %v（原先 %v）", files, first)
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(raw), "key-new") || strings.Contains(string(raw), "key-old") {
		t.Fatalf("凭据未更新为新 Key：%s", string(raw))
	}
	if !strings.Contains(string(raw), deepai.ExtraIsPro) {
		t.Fatalf("is_pro 未落盘（面板/费率判断要用）：%s", string(raw))
	}
	// 机器指纹必须被保留（换指纹等于换免费额度身份）。
	var doc struct {
		Auth struct {
			MachineID string `json:"machineId"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if strings.TrimSpace(doc.Auth.MachineID) == "" {
		t.Fatal("重登后机器指纹丢失")
	}
}

// TestDeepAILoginDifferentUsernameNewAccount 不同账号 ⇒ 各成一条（两文件、两 uid）。
func TestDeepAILoginDifferentUsernameNewAccount(t *testing.T) {
	srv := anyKeyUpstream(t)
	a, _, dir := newDeepAIApp(t, srv.URL)
	if err := a.completeDeepAILogin(logindeepai.Result{APIKey: "key-a", Username: "alice"}); err != nil {
		t.Fatalf("alice: %v", err)
	}
	if err := a.completeDeepAILogin(logindeepai.Result{APIKey: "key-b", Username: "bob"}); err != nil {
		t.Fatalf("bob: %v", err)
	}
	if got := deepaiAuthFiles(t, dir); len(got) != 2 {
		t.Fatalf("两个不同账号应有两个文件，实际 %v", got)
	}
}

// TestDeepAILoginWiring 登录能力表 / 粘贴回调白名单三处同源（不得各写一份判据）。
func TestDeepAILoginWiring(t *testing.T) {
	if !manualCallbackKind(provider.DeepAI) {
		t.Fatal("DeepAI 的回调写死回环（127.0.0.1:<port>），必须登记为「需要粘贴回调兜底」")
	}
	f, ok := loginFlows[provider.DeepAI]
	if !ok {
		t.Fatal("loginFlows 缺 DeepAI 条目")
	}
	if f.Start == nil || f.Poll == nil || f.Shutdown == nil {
		t.Fatalf("DeepAI 的登录三件套不全：start=%v poll=%v shutdown=%v", f.Start != nil, f.Poll != nil, f.Shutdown != nil)
	}
	if f.openIncognito {
		t.Fatal("登录页复用日常会话是期望行为（同号重登），不得用无痕窗口")
	}
	if f.pollTimeout == nil || f.pollTimeout() != logindeepai.FlowTTL() {
		t.Fatalf("pollTimeout 必须对齐包内 %s（通用 5m 会在用户还在授权页时先关回调口）", logindeepai.FlowTTL())
	}
	if f.pollTimeout() <= 5*time.Minute {
		t.Fatalf("pollTimeout=%s，必须长于通用 loginTimeout(5m)", f.pollTimeout())
	}
}

// TestDeepAIDesktopImportMissingIsReadable 找不到桌面凭据时给出可行动文案（不猜路径）。
func TestDeepAIDesktopImportMissingIsReadable(t *testing.T) {
	srv := anyKeyUpstream(t)
	a, _, _ := newDeepAIApp(t, srv.URL)
	_, _, err := a.ImportDeepAIDesktop(t.TempDir(), "")
	if err == nil || !strings.Contains(err.Error(), "未找到") {
		t.Fatalf("应为「未找到桌面凭据」的可读错误，实际 %v", err)
	}
}
