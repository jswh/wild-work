package deepai

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"wild-work/internal/auth"
)

func TestNewUUIDShape(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	seen := map[string]bool{}
	for i := 0; i < 32; i++ {
		id := NewUUID()
		if !re.MatchString(id) {
			t.Fatalf("UUID=%q，不符合 UUIDv4 形态", id)
		}
		if seen[id] {
			t.Fatalf("UUID 重复：%q", id)
		}
		seen[id] = true
	}
}

func TestFingerprintLowercasesBeforeHashing(t *testing.T) {
	sum := sha256.Sum256([]byte("abcd-1234"))
	want := hex.EncodeToString(sum[:])
	for _, raw := range []string{"abcd-1234", "ABCD-1234", "  ABCD-1234  "} {
		if got := Fingerprint(raw); got != want {
			t.Fatalf("Fingerprint(%q)=%q，期望 %q（先小写去空白再 sha256）", raw, got, want)
		}
	}
	if got := Fingerprint("   "); got != "" {
		t.Fatalf("空原始值必须返回空串（调用方据此不发该头），got %q", got)
	}
}

func TestEnsureMachineFillsAndPersists(t *testing.T) {
	dir := t.TempDir()
	a := &auth.Auth{Kind: KindName, UID: "dai-x", AccessToken: "k", ExpiresAt: 4102444800}
	a.FilePath = filepath.Join(dir, "deepai-dai-x.json")
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("SaveAtomic: %v", err)
	}
	if !EnsureMachine(a) {
		t.Fatal("首次调用应写入机器指纹并返回 true")
	}
	if a.MachineID == "" {
		t.Fatal("MachineID 仍为空")
	}
	if EnsureMachine(a) {
		t.Fatal("已有机器指纹时不得重复生成（换了指纹等于换了机器，免费额度会重新计算）")
	}
	// 落盘：重新读文件必须拿回同一个原始 UUID（不是派生值）。
	raw, err := os.ReadFile(a.FilePath)
	if err != nil {
		t.Fatalf("读回凭据: %v", err)
	}
	if !strings.Contains(string(raw), a.MachineID) {
		t.Fatalf("凭据文件里没有机器标识：%s", string(raw))
	}
}

func TestEnsureMachineNilSafe(t *testing.T) {
	if EnsureMachine(nil) {
		t.Fatal("nil 账号不应报写入")
	}
}

func TestNormalizeKey(t *testing.T) {
	cases := []struct{ in, want string }{
		{"  3f2a  ", "3f2a"},
		{`"3f2a"`, "3f2a"},
		{"Api-Key: 3f2a", "3f2a"},
		{"api-key:3f2a", "3f2a"},
		{"Api-Key: 3f2a\nX-Other: 1", "3f2a"},
		{"", ""},
	}
	for _, c := range cases {
		if got := NormalizeKey(c.in); got != c.want {
			t.Fatalf("NormalizeKey(%q)=%q，期望 %q", c.in, got, c.want)
		}
	}
}

func TestKeyUIDDerivedNotPlaintext(t *testing.T) {
	key := "3f2a9c1d-aaaa-bbbb-cccc-1234567890ab"
	uid := KeyUID(key)
	if !strings.HasPrefix(uid, "dai-") || len(uid) != len("dai-")+16 {
		t.Fatalf("uid=%q，期望 dai- + 16 位哈希", uid)
	}
	if strings.Contains(uid, key[:8]) {
		t.Fatalf("uid 含凭据明文：%q", uid)
	}
	if uid != KeyUID("  "+key+"  ") {
		t.Fatal("同一 Key 的 uid 必须稳定（空白不影响）")
	}
	if uid == KeyUID(key+"x") {
		t.Fatal("不同 Key 必须得到不同 uid")
	}
	if !regexp.MustCompile(`^dai-[0-9a-f]{16}$`).MatchString(uid) {
		t.Fatalf("uid 形态=%q", uid)
	}
}
