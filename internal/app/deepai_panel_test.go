package app

import (
	"os"
	"strings"
	"testing"
)

// TestDeepAIDialogHasFourWiredEntries 面板四段入口齐全**且都绑了处理函数**。
//
// 为什么用静态断言：死按钮事故已发生 3 次（按钮在、handler 漏绑），而这类问题
// 在单元测试与后端接口测试里都看不见。判据只看「id 存在」+「app.js 里出现对该 id 的
// 赋值」，不做 DOM 级别渲染（浏览器层归 e2e 票）。
func TestDeepAIDialogHasFourWiredEntries(t *testing.T) {
	html, err := os.ReadFile("../../cmd/wild-work/web/index.html")
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	js, err := os.ReadFile("../../cmd/wild-work/web/app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	// 四段入口的按钮 id（登录 / 粘贴回调 / 粘贴 Key / 桌面导入）+ 两个输入框。
	for _, id := range []string{
		"btnDeepAILogin", "btnDeepAICallbackSubmit", "btnDeepAIKeySubmit", "btnDeepAIImport",
		"deepaiKeyInput", "deepaiCallbackInput",
	} {
		if !strings.Contains(string(html), `id="`+id+`"`) {
			t.Fatalf("面板缺入口 %q（四段入口必须齐全）", id)
		}
		if !strings.Contains(string(js), `$("`+id+`")`) {
			t.Fatalf("面板入口 %q 在 app.js 里从未被取用（死按钮）", id)
		}
	}
	// 每段的标题必须写明是「方式X：」，让用户看得出有四条路可走。
	for _, heading := range []string{"方式一：浏览器登录", "方式二：粘贴回调地址", "方式三：粘贴桌面端 API Key", "方式四：从本机"} {
		if !strings.Contains(string(html), heading) {
			t.Fatalf("面板缺分段标题 %q", heading)
		}
	}
	// 回调入口必须走与自动回调**同一个**端点（不得另开一条通道）。
	if !strings.Contains(string(js), `api("/api/login/callback"`) {
		t.Fatal("粘贴回调必须提交到 /api/login/callback（与自动回调同一结论通道）")
	}
	if !strings.Contains(string(js), `api("/api/login/deepai_import"`) {
		t.Fatal("桌面导入必须走 /api/login/deepai_import")
	}
}
