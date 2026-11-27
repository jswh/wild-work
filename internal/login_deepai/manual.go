package login_deepai

import (
	"errors"
	"net/url"
	"strings"
)

// 粘贴回调地址的解析（浏览器与网关不同机时的唯一通用解）。
//
// 用户能提供的东西有三种，只有前两种是完整的：
//  1. 完整 URL：`http://127.0.0.1:43123/auth-callback?code=…&state=…`（地址栏直接复制）
//  2. 裸 query 串：`code=…&state=…`（只复制了问号后面的部分）
//  3. 只有 code —— **必须拒绝**：拿不到 state 就无法确认它属于本次登录，
//     接受它等于允许任意 code 注入（别人的/旧的/自己拼的）。

// parseCallback 从粘贴内容里取出 (code, state)。
func parseCallback(raw string) (string, string, error) {
	s := strings.TrimSpace(raw)
	s = strings.Trim(s, "\"'")
	s = strings.TrimSpace(s)
	if s == "" {
		return "", "", errors.New("请粘贴登录后浏览器地址栏里的完整回调链接（含 code=…&state=…）")
	}
	if looksLikeBareCode(s) {
		return "", "", errors.New("这只像是一个 code：请粘贴**完整**的回调链接（含 state=…），" +
			"只给 code 无法确认它属于本次登录")
	}
	values, err := parseQuery(s)
	if err != nil {
		return "", "", errors.New("无法解析粘贴的回调地址：请粘贴地址栏里的整条链接" +
			"（形如 http://127.0.0.1:端口/auth-callback?code=…&state=…）")
	}
	code := strings.TrimSpace(values.Get("code"))
	state := strings.TrimSpace(values.Get("state"))
	if code == "" || len(code) > codeMaxLen {
		return "", "", errors.New("回调里没有可用的 code：请粘贴完整的回调链接")
	}
	if state == "" {
		return "", "", errors.New("回调里没有 state：请粘贴完整的回调链接，不要只复制 code")
	}
	return code, state, nil
}

// parseQuery 同时接受完整 URL / 路径+query / 裸 query 三种形态。
func parseQuery(s string) (url.Values, error) {
	switch {
	case strings.Contains(s, "://"):
		u, err := url.Parse(s)
		if err != nil {
			return nil, err
		}
		return u.Query(), nil
	case strings.Contains(s, "?"):
		// 形如 `/auth-callback?code=…`（用户可能只复制了路径部分）
		u, err := url.Parse(s)
		if err != nil {
			return nil, err
		}
		return u.Query(), nil
	default:
		return url.ParseQuery(s)
	}
}

// looksLikeBareCode 判断粘贴内容是不是「只有一个 code」。
//
// 裸 query 串一定含 `=`；裸 code 不含 `=` 且不含 `://`。这条判据只是为了给出
// **可行动** 的提示（真正的拒绝靠后面「缺 state」那条，两处不重复）。
func looksLikeBareCode(s string) bool {
	return !strings.Contains(s, "=") && !strings.Contains(s, "://") && !strings.Contains(s, "/")
}
