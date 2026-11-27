package deepai

import (
	"context"
	"net/http"
	"strings"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// NormalizeKey 规整用户粘贴的凭据。
//
// 用户常把 DevTools 里整行 `Api-Key: 3f2a…` 一起粘进来、或带上引号/尾部空白。
// 这里只做无损规整（去空白、去包裹引号、去 `Api-Key:` 前缀），**不猜**内容形态——
// 上游 Key 的形态未公开，任何「必须以 sk- 开头」之类的前置校验都会误杀合法 Key。
func NormalizeKey(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.Trim(s, "\"'")
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, ':'); i > 0 && strings.EqualFold(strings.TrimSpace(s[:i]), HeaderKey) {
		s = strings.TrimSpace(s[i+1:])
		s = strings.Trim(s, "\"'")
		s = strings.TrimSpace(s)
	}
	// 整段请求头粘贴（含换行）时只取第一行有效值。
	if i := strings.IndexAny(s, "\r\n"); i > 0 {
		s = strings.TrimSpace(s[:i])
	}
	return s
}

// KeyUID 由 API Key 派生稳定的账号 uid（`dai-` + sha256(key) 前 16 位）。
//
// 为什么不用上游身份字段：`/models` 不随账号变化、也没有「我是谁」端点（`/theos`
// 只给余额与签到挑战），Key 本身是唯一稳定标识。派生而非明文：uid 会出现在面板、
// 文件名、日志里，不得含凭据（不变量 2）。
func KeyUID(key string) string {
	sum := sha256Hex(strings.TrimSpace(key))
	return "dai-" + sum[:16]
}

// ValidateKey 做一次**零费用**校验：`GET /deepai_code/models` 带 `Api-Key`。
//
// 为什么是它：对话要花钱（计量模型按次扣 Theos）、`/theos` 会推进签到环，
// 而目录端点带任意有效 Key 即 200 且**不改变任何状态**（2026-11-13 实测）。
// 校验通过即认为 Key 可用；后续对话若回 401 会走「登录态失效可自愈」重新提示粘贴。
//
// 返回 uid 便于调用方立刻落盘（一次往返拿全所需信息）。
func (c *Client) ValidateKey(ctx context.Context, raw string) (string, error) {
	key := NormalizeKey(raw)
	if key == "" {
		return "", &provider.Error{Kind: provider.ErrBadParams, Msg: "API Key 不能为空"}
	}
	uid := KeyUID(key)
	probe := &auth.Auth{Kind: KindName, UID: uid, AccessToken: key}
	req, err := c.newRequest(ctx, http.MethodGet, PathModels, probe, nil, "")
	if err != nil {
		return "", err
	}
	body, status, errBody, err := c.do(req, c.HTTP)
	if err != nil {
		return "", err
	}
	if body == nil {
		return "", &provider.Error{
			Kind:   classifyStatus(c, status, string(errBody)),
			Status: status,
			Msg:    keyRejectMessage(status, errBody),
		}
	}
	_ = body.Close()
	return uid, nil
}

// keyRejectMessage 把「Key 被拒」翻译成用户能自查的话。
//
// 上游对无效 Key 的报文与「跳登录页」的 302 同源，只说「无效」对用户帮助有限；
// 这里补上三条自救路径（生成新 Key / 粘贴的是 Key 而不是别的凭据 / 代理是否被拒）。
func keyRejectMessage(status int, body []byte) string {
	up := errorMessage(body)
	switch {
	case status == http.StatusUnauthorized:
		return "API Key 无效或已失效（上游回 401）。请在 DeepAI Code 桌面端退出后重新登录并复制新的 Key；" +
			"如果你粘贴的是登录凭据而非 API Key，请改用「浏览器登录」。"
	case status >= 300 && status < 400:
		return "API Key 未被接受（上游跳转登录页）。请重新登录 DeepAI Code 桌面端并复制新的 Key。"
	}
	return "上游拒绝了这次校验（HTTP " + http.StatusText(status) + "）：" + up
}
