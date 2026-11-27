package deepai

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"wild-work/internal/provider"
)

// 桌面端登录链（PKCE + 本机回环回调 + 授权码换 Key）的协议层。
//
// 官方桌面端形态（spec §8.1）：
//
//	GET  /deepai_code/desktop_auth/?state=…&port=…&code_challenge=…&code_challenge_method=S256
//	POST /deepai_code/desktop_auth/exchange/   {"code": …, "code_verifier": …}
//	  → {"api_key": …, "username": …, "is_pro": …}
//
// 参数顺序与桌面端一致（便于和抓包逐字对照）；`code_challenge` **恒定发送**：
// 缺失上游仍给 200，但格式非法会被回 400 `Invalid sign-in proof.`（实测），
// 与其赌「不发也能过」，不如按官方形态发全。
//
// 为什么放在渠道包而不是 login_deepai：这些是**上游协议**（端点/参数/响应形状），
// login_deepai 只负责「本机监听 + 状态机 + 落盘编排」；协议知识集中在这里，将来
// 上游变了只改一处。参考实现同款分层。

const (
	// PathDesktopAuth 授权页（浏览器打开）。
	PathDesktopAuth = "/deepai_code/desktop_auth/"
	// PathExchangeLogin 授权码换 API Key（服务端 POST）。
	PathExchangeLogin = "/deepai_code/desktop_auth/exchange/"

	// pkcMethod 唯一支持的摘要算法（也是桌面端唯一使用的）。
	pkcMethod = "S256"
	// pkceVerifierBytes verifier 的熵：32 字节 → 43 字符 base64url。
	pkceVerifierBytes = 32
	// pkceLen S256 verifier/challenge 的固定长度（上游按格式校验，长度不对即 400）。
	pkceLen = 43
	// stateBytes 一次性 state 的熵：32 字节 → 64 字符 hex。
	stateBytes = 32
)

// NewPKCE 生成一对 (verifier, challenge)；verifier 只存在内存里，兑换完即随会话销毁。
func NewPKCE() (string, string, error) {
	buf := make([]byte, pkceVerifierBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("PKCE 随机数生成失败: %w", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(buf)
	return verifier, PKCEChallenge(verifier), nil
}

// PKCEChallenge 计算 S256 challenge：base64url(sha256(verifier))，无填充。
func PKCEChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// ValidPKCEVerifier 报告 verifier 是否符合 S256 形态（43 字符 base64url）。
func ValidPKCEVerifier(verifier string) bool {
	if len(verifier) != pkceLen {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(verifier)
	return err == nil
}

// NewState 生成一次性 state（64 字符 hex）：每次登录一枚、只活到本次流程结束。
//
// 长随机而不是短串：state 的唯一职责是「证明回调属于**本次**登录」（防串号），
// 长度换来的是猜测成本；比对时按常数时间走（见 login_deepai）。
func NewState() (string, error) {
	buf := make([]byte, stateBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("state 随机数生成失败: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// SignInURL 按官方形态拼授权页 URL（参数顺序也与桌面端一致）。
//
// port 是回环回调端口（上游把浏览器跳回 `http://127.0.0.1:<port>/?code=…&state=…`）。
func (c *Client) SignInURL(state string, port int, challenge string) string {
	origin := strings.TrimRight(c.Origin, "/")
	if origin == "" {
		origin = DefaultOrigin
	}
	return fmt.Sprintf("%s%s?state=%s&port=%d&code_challenge=%s&code_challenge_method=%s",
		origin, PathDesktopAuth, state, port, challenge, pkcMethod)
}

// LoginCredentials 是授权码换回来的账号凭据。
//
// Username 是**账号身份**：三入口（浏览器登录 / 粘贴回调 / 桌面导入）都拿它来判
// 「同一账号」——同一个 username 换了 Key 就是「重登换 Key」，必须覆盖同一条账号，
// 否则会出现「重复号 + 每日额度被拆半」（spec §8.5）。
type LoginCredentials struct {
	APIKey   string
	Username string
	IsPro    bool
}

// ExchangeLoginCode 用授权码 + verifier 换 API Key（服务端调用，浏览器不参与）。
//
// 无 Api-Key 头：登录时还不存在账号凭据（newRequest 对 nil 账号只发公共头）。
// 错误按用户可读的方式给出：PKCE 不匹配 / 授权码失效是两条不同的自救路径。
func (c *Client) ExchangeLoginCode(ctx context.Context, code, verifier string) (LoginCredentials, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return LoginCredentials{}, &provider.Error{Kind: provider.ErrBadParams, Msg: "授权码为空"}
	}
	payload := map[string]string{"code": code}
	if verifier != "" {
		// 上游按 `code_challenge` 校验 verifier；空串会被判「不是同一个客户端」。
		payload["code_verifier"] = verifier
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return LoginCredentials{}, err
	}
	req, err := c.newRequest(ctx, http.MethodPost, PathExchangeLogin, nil, body, "")
	if err != nil {
		return LoginCredentials{}, err
	}
	rc, status, errBody, err := c.do(req, c.HTTP)
	if err != nil {
		return LoginCredentials{}, err
	}
	if status >= 300 {
		return LoginCredentials{}, &provider.Error{
			Kind:   provider.ErrClient,
			Status: status,
			Msg:    exchangeErrorMessage(status, errBody),
		}
	}
	defer rc.Close()
	var out struct {
		APIKey   string `json:"api_key"`
		Username string `json:"username"`
		IsPro    bool   `json:"is_pro"`
	}
	if err := json.NewDecoder(io.LimitReader(rc, maxResponseBytes)).Decode(&out); err != nil {
		return LoginCredentials{}, fmt.Errorf("登录响应解析失败: %w", err)
	}
	key := NormalizeKey(out.APIKey)
	if key == "" {
		return LoginCredentials{}, errors.New("登录响应里没有有效的 API Key")
	}
	return LoginCredentials{
		APIKey:   key,
		Username: strings.TrimSpace(out.Username),
		IsPro:    out.IsPro,
	}, nil
}

// exchangeErrorMessage 把「换 Key 失败」翻译成两条自查路径。
func exchangeErrorMessage(status int, body []byte) string {
	up := errorMessage(body)
	if strings.Contains(strings.ToLower(up), "sign-in proof") {
		return "登录回调校验失败（PKCE 不匹配）：请重新发起登录，不要手工拼接回调 URL"
	}
	if status == http.StatusBadRequest {
		return "授权码无效或已过期（只能兑换一次，且 5 分钟内有效）：请重新发起登录。" +
			"上游原文：" + up
	}
	return fmt.Sprintf("换取 API Key 失败（HTTP %s）：%s", http.StatusText(status), up)
}
