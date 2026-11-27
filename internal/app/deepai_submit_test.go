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

	"wild-work/internal/config"
	"wild-work/internal/deepai"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
)

// deepaiFakeUpstream 是「只认对的 Key、只回目录」的假上游：
// 校验路径必须零费用（对话花 Theos、/theos 推进签到环），故只允许 /models。
type deepaiFakeUpstream struct {
	key  string
	hits int
}

func (f *deepaiFakeUpstream) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits++
		if r.URL.Path != deepai.PathModels {
			t.Errorf("校验只能打目录端点（零费用），实际 %s", r.URL.Path)
		}
		if r.Header.Get(deepai.HeaderKey) != f.key {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"auth_required"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"glm-5.3-flash"}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newDeepAIApp(t *testing.T, origin string) (*App, *pool.Pool, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.StateFile = filepath.Join(dir, "state.json")
	cfg.AuthDir = dir

	client := deepai.New()
	client.Origin = origin
	p := pool.New(filepath.Join(dir, "state-deepai.json"))
	a, err := New(Options{
		ConfigPath: filepath.Join(dir, "config.json"),
		Config:     cfg,
		Runtimes: map[provider.Kind]*Runtime{
			provider.DeepAI: {Kind: provider.DeepAI, Pool: p, Upstream: client},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(a.Close)
	return a, p, dir
}

// TestSubmitDeepAIKeyPersistsAccount 粘贴 Key → 零费用校验 → 落盘 → 进池（接口级）。
func TestSubmitDeepAIKeyPersistsAccount(t *testing.T) {
	fake := &deepaiFakeUpstream{key: "key-abc"}
	srv := fake.server(t)
	a, p, dir := newDeepAIApp(t, srv.URL)

	uid, warn, err := a.SubmitDeepAIKey("  key-abc  ")
	if err != nil {
		t.Fatalf("SubmitDeepAIKey: %v", err)
	}
	if warn != "" {
		t.Fatalf("首次添加不应告警：%q", warn)
	}
	if uid != deepai.KeyUID("key-abc") {
		t.Fatalf("uid=%q，期望由 Key 派生", uid)
	}
	if fake.hits != 1 {
		t.Fatalf("校验请求数=%d，期望恰好一次（不许多打上游）", fake.hits)
	}

	// 落盘：文件内容必须含 Key、原始机器 UUID（不是派生值）、远期过期时间。
	raw, err := os.ReadFile(filepath.Join(dir, "deepai-"+uid+".json"))
	if err != nil {
		t.Fatalf("凭据未落盘: %v", err)
	}
	var doc struct {
		Auth struct {
			AccessToken string `json:"accessToken"`
			ExpiresAt   int64  `json:"expiresAt"`
			MachineID   string `json:"machineId"`
		} `json:"auth"`
		Account struct {
			UID      string `json:"uid"`
			Nickname string `json:"nickname"`
		} `json:"account"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("凭据不是合法 JSON: %v", err)
	}
	if doc.Auth.AccessToken != "key-abc" {
		t.Fatalf("accessToken 未保存（或未规整空白）")
	}
	if doc.Auth.MachineID == "" || strings.Contains(doc.Auth.MachineID, " ") {
		t.Fatalf("机器指纹缺失/形态异常：%q", doc.Auth.MachineID)
	}
	if doc.Auth.ExpiresAt <= time.Now().AddDate(1, 0, 0).Unix() {
		t.Fatalf("expiresAt=%d，必须是远期值（0 会让 NeedsRefresh 恒真）", doc.Auth.ExpiresAt)
	}
	if doc.Account.UID != uid || doc.Account.Nickname == "" {
		t.Fatalf("account 段异常：uid=%q nick=%q", doc.Account.UID, doc.Account.Nickname)
	}

	// 进池（reloadAccounts 必须先落盘再重载，否则面板看不到账号）。
	if _, ok := p.Status(uid); !ok {
		t.Fatal("新账号未进入池（reloadAccounts 漏了 deepai 分支？）")
	}

	// 第二次粘贴同一枚 Key：允许，但必须告警「重复添加不增加额度」。
	_, warn2, err := a.SubmitDeepAIKey("key-abc")
	if err != nil {
		t.Fatalf("重复添加不应报错：%v", err)
	}
	if !strings.Contains(warn2, "重复添加") {
		t.Fatalf("重复 Key 必须告警，实际 %q", warn2)
	}
}

// TestSubmitDeepAIKeyKeepsRenamedNickname 重贴凭据不得冲掉用户手工改过的显示名（R137 同款）。
func TestSubmitDeepAIKeyKeepsRenamedNickname(t *testing.T) {
	fake := &deepaiFakeUpstream{key: "key-abc"}
	srv := fake.server(t)
	a, _, dir := newDeepAIApp(t, srv.URL)

	uid, _, err := a.SubmitDeepAIKey("key-abc")
	if err != nil {
		t.Fatalf("首次添加: %v", err)
	}
	// 模拟用户在面板改名：直接改 auth 文件里的 nickname 并重新加载一次。
	path := filepath.Join(dir, "deepai-"+uid+".json")
	raw, _ := os.ReadFile(path)
	if err := os.WriteFile(path, []byte(strings.Replace(string(raw), `"nickname": "DeepAI`, `"nickname": "我的主力号`, 1)), 0o600); err != nil {
		t.Fatalf("改名: %v", err)
	}
	if _, _, err := a.SubmitDeepAIKey("key-abc"); err != nil {
		t.Fatalf("重贴: %v", err)
	}
	after, _ := os.ReadFile(path)
	if !strings.Contains(string(after), "我的主力号") {
		t.Fatalf("重贴凭据冲掉了用户改的名字：%s", string(after))
	}
}

// TestSubmitDeepAIKeyRejectsBadKey 401 必须变成可自助的错误，且不落盘。
func TestSubmitDeepAIKeyRejectsBadKey(t *testing.T) {
	fake := &deepaiFakeUpstream{key: "right"}
	srv := fake.server(t)
	a, _, dir := newDeepAIApp(t, srv.URL)

	if _, _, err := a.SubmitDeepAIKey("wrong"); err == nil {
		t.Fatal("401 必须报错")
	} else if !strings.Contains(err.Error(), "API Key 无效或已失效") {
		t.Fatalf("错误文案不可自助：%v", err)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "deepai-*.json"))
	if len(matches) != 0 {
		t.Fatalf("校验失败不得落盘，实际写了 %v", matches)
	}
	if _, _, err := a.SubmitDeepAIKey("   "); err == nil {
		t.Fatal("空 Key 必须在本地拒绝（不打上游）")
	}
}