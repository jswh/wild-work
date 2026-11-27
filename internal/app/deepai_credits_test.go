package app

import (
	"testing"

	"wild-work/internal/provider"
)

// TestDeepAICreditsTermAndNotNA 面板口径（spec §7.3）：
//   - 显示术语是「Theos」，但**计量单位仍是积分**（credits_unit 空 ⇒ 进合计与折线图）；
//   - **不**属于「无积分」渠道（有真实余额端点，见 internal/deepai/credits.go）。
func TestDeepAICreditsTermAndNotNA(t *testing.T) {
	up := &fakeUpstream{remain: 200}
	a, _ := newTestApp(t, provider.DeepAI, up, "dai-1")
	if got := a.creditsTerm(provider.DeepAI.String()); got != "Theos" {
		t.Fatalf("creditsTerm(deepai)=%q，期望「Theos」", got)
	}
	if got := a.creditsUnit(provider.DeepAI.String()); got != "" {
		t.Fatalf("creditsUnit(deepai)=%q，期望空串（仍是积分口径，进合计与折线图）", got)
	}
	if a.creditsNAKind(provider.DeepAI.String()) {
		t.Fatal("deepai 不能被标成「无积分」：它有自己的余额端点")
	}
}

// TestDeepAINegativeCreditsKeptAsIs 负余额（已超额）如实落快照：不夹 0、不改写。
func TestDeepAINegativeCreditsKeptAsIs(t *testing.T) {
	up := &fakeUpstream{remain: -8}
	a, p := newTestApp(t, provider.DeepAI, up, "dai-1")
	if _, err := a.RefreshCredits("dai-1"); err != nil {
		t.Fatalf("负余额是合法读数，不该报错：%v", err)
	}
	st, _ := p.Status("dai-1")
	if st.Credits != -8 {
		t.Fatalf("credits=%d，期望如实保留 -8（夹 0 会让用户以为额度只是用光而不是超额）", st.Credits)
	}
	if st.CreditsStale {
		t.Fatal("读到合法负数不该标「待刷新」")
	}
	if st.Disabled || st.Cooling {
		t.Fatalf("负余额是计费结果，不是账号故障：disabled=%v cooling=%v", st.Disabled, st.Cooling)
	}
}
