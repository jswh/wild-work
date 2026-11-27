// Package login_deepai 实现 DeepAI Code 的**桌面端式浏览器登录**：
// PKCE(S256) + 本机 127.0.0.1 回环回调 + 服务端用授权码换 API Key。
//
// 链路（spec §8.1）：
//  1. Start：生成一次性 state + PKCE，起回环监听（`/auth-callback`），返回授权页 URL
//     （用户在自己的浏览器里完成 Google/Apple/邮箱登录——**复用日常会话是期望行为**：
//     它就是「同一账号重登」；要加第二个号请先退出或换无痕窗口）；
//  2. 上游把浏览器跳回 `http://127.0.0.1:<port>/auth-callback?code=…&state=…`；
//  3. Poll 发现授权码 → `POST /desktop_auth/exchange/` 换 `{api_key, username, is_pro}`；
//  4. 调用方（internal/app）走与「粘贴 Key」**同一条**落盘通道。
//
// 三条硬纪律：
//   - **state 常数时间比对**、授权码**只兑换一次**（`code` 只能兑换一次，重放没有意义）；
//   - **回环绑不上不得把流程钉成终态**（R120 的教训：一次瞬时失败不能毁掉整个流程）——
//     监听失败只降级为「等粘贴回调」，TTL 内仍可完成（远程 dashboard / 无 GUI 服务器就是这样用）；
//   - 登录调用**不走账号代理**（此刻还没有账号，见 spec §9 的作用范围）。
package login_deepai

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"wild-work/internal/deepai"
)

const (
	// flowTTL 一次登录流程的最长等待（回环监听与待交换授权的共同上限）。
	// 取 10 分钟（与 login_codearts / login_lobsterai 一致）：够慢的人登录，也不让回调口长挂。
	flowTTL = 10 * time.Minute
	// callbackPath 上游把浏览器跳回的路径（桌面端同形：固定 `/auth-callback`）。
	callbackPath = "/auth-callback"
	// codeMaxLen 授权码长度上限（防御异常长输入）。
	codeMaxLen = 4096
	// exchangeTimeout 兑换请求的上限（协议没有给值；25s 与参考实现一致）。
	exchangeTimeout = 25 * time.Second
)

// FlowTTL 暴露流程最长存活时间，供调用方（app.pollLogin）对齐等待上限：
// 通用 loginTimeout（5 分钟）比它短，直接沿用会在用户还在授权页时先关掉回调口。
func FlowTTL() time.Duration { return flowTTL }

// ErrPending 表示授权尚未完成（用户在浏览器里还没走完）。
var ErrPending = errors.New("login pending")

// Result 登录成功后拿到的凭据。
//
// Username 是**账号身份**：三个入口（浏览器登录 / 粘贴回调 / 桌面导入）都用它判
// 「同一账号」，落盘时据此决定「覆盖已有那条」还是「新建一条」。
type Result struct {
	APIKey   string
	Username string
	IsPro    bool
}

// session 是一次登录在内存里的运行时状态。
//
// 刻意不落盘：verifier / state / 授权码都是**本次流程的一次性材料**，进程重启即作废，
// 落盘反而会让过期材料在下次启动时被误用。
type session struct {
	mu         sync.Mutex
	state      string
	verifier   string
	port       int
	code       string // 已收到的授权码（只兑换一次）
	errMsg     string // 终态错误（如兑换失败）
	result     *Result
	exchanging bool
	createdAt  time.Time
	ln         net.Listener
	srv        *http.Server
	// listenErr 回环绑不上的原因（**不是终态**：降级为等粘贴回调）。
	listenErr error
}

var (
	curMu sync.Mutex
	cur   *session

	// listen 与 newLoginClient 只为测试可替换；生产路径即 net.Listen / deepai.New。
	listen         = net.Listen
	newLoginClient = func() *deepai.Client { return deepai.New() }
)

// NewClient 登录用 HTTP 客户端（30s 总超时兜底，与兄弟登录包同形）。
func NewClient() *http.Client { return &http.Client{Timeout: 30 * time.Second} }

// Start 发起登录：生成 state + PKCE、起回环监听、返回授权页 URL。
//
// 回环监听失败**不返回错误**（R120）：进程照常给出 URL，用户仍可在浏览器里完成授权、
// 再把打不开的回调 URL 粘回来（面板里那条「粘贴回调地址」入口）。
func Start(_ *http.Client, _ string) (string, error) {
	state, err := deepai.NewState()
	if err != nil {
		return "", err
	}
	verifier, challenge, err := deepai.NewPKCE()
	if err != nil {
		return "", err
	}

	sess := &session{state: state, verifier: verifier, createdAt: time.Now()}
	ln, lerr := listen("tcp", "127.0.0.1:0")
	if lerr != nil {
		// 降级：给上游一个「看起来合法」的端口（上游只把它拼进
		// http://127.0.0.1:<port>/auth-callback；打不开时地址栏里的 code/state 仍可粘贴回来）。
		sess.listenErr = lerr
		sess.port = fallbackPort()
		log.Printf("deepai login: 本机回环监听失败（降级为粘贴回调）err=%v", lerr)
	} else {
		sess.ln = ln
		sess.port = ln.Addr().(*net.TCPAddr).Port
		mux := http.NewServeMux()
		mux.HandleFunc(callbackPath, sess.handleCallback)
		mux.HandleFunc("/", sess.handleCallback) // 上游只带 port，路径细节不依赖
		sess.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		go func() { _ = sess.srv.Serve(ln) }()
	}
	replace(sess)
	return newLoginClient().SignInURL(state, sess.port, challenge), nil
}

// fallbackPort 绑不上回环时给上游的替代端口（只影响 URL 形态，不影响粘贴路径）。
func fallbackPort() int {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 47821
	}
	return 45000 + (int(b[0])<<8|int(b[1]))%10000
}

// Poll 检查一次登录结果：未完成返回 ErrPending；拿到授权码就**立即**兑换。
//
// 兑换失败一律终态：授权码是单次票据，同一枚再试不会成功，留着只会让用户等满 TTL。
func Poll(_ *http.Client, _ string) (Result, error) {
	sess := current()
	if sess == nil {
		// 会话已被取消/超时清掉：当作终态（否则面板会永远转圈）。
		return Result{}, errors.New("登录流程已结束，请重新发起")
	}
	sess.mu.Lock()
	expired := time.Since(sess.createdAt) > flowTTL
	if expired {
		sess.mu.Unlock()
		Shutdown()
		return Result{}, errors.New("登录超时（10 分钟未收到授权回调），请重新发起")
	}
	if sess.errMsg != "" {
		msg := sess.errMsg
		sess.mu.Unlock()
		Shutdown()
		return Result{}, errors.New(msg)
	}
	if sess.result != nil {
		res := *sess.result
		sess.mu.Unlock()
		Shutdown()
		return res, nil
	}
	if sess.code == "" || sess.exchanging {
		sess.mu.Unlock()
		return Result{}, ErrPending
	}
	code, verifier := sess.code, sess.verifier
	sess.exchanging = true
	sess.mu.Unlock()

	cred, xerr := exchange(code, verifier)
	sess.mu.Lock()
	sess.exchanging = false
	if xerr != nil {
		sess.errMsg = xerr.Error()
		msg := sess.errMsg
		sess.mu.Unlock()
		log.Printf("deepai login: 换取 API Key 失败 err=%v", xerr)
		Shutdown()
		return Result{}, errors.New(msg)
	}
	res := Result{APIKey: cred.APIKey, Username: cred.Username, IsPro: cred.IsPro}
	sess.result = &res
	sess.mu.Unlock()
	Shutdown()
	return res, nil
}

// exchange 单次兑换（newLoginClient 可替换，测试据此指向本机假上游）。
func exchange(code, verifier string) (deepai.LoginCredentials, error) {
	c := newLoginClient()
	if c.HTTP != nil {
		c.HTTP.Timeout = exchangeTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), exchangeTimeout)
	defer cancel()
	return c.ExchangeLoginCode(ctx, code, verifier)
}

// Shutdown 关闭本机回环监听并丢弃会话（登录结束 / 超时 / 取消时调用；幂等）。
func Shutdown() {
	curMu.Lock()
	sess := cur
	cur = nil
	curMu.Unlock()
	if sess != nil && sess.srv != nil {
		_ = sess.srv.Close()
	}
}

// handleCallback 处理浏览器自动回调。
//
// state 不匹配 / 缺 code 都只回一段人话页面并**继续等**（不置终态）：那种请求可能是
// 别人扫到的、或旧标签页重放的，把它当终态等于让一次无关请求毁掉本次登录（DoS 面）。
func (s *session) handleCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	q := r.URL.Query()
	code, state := q.Get("code"), q.Get("state")
	if code == "" || len(code) > codeMaxLen {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("<h2>缺少授权码</h2><p>请回到面板重新发起登录。</p>"))
		return
	}
	if !stateEqual(state, s.state) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("<h2>state 不匹配</h2><p>这不是本次登录的回调，请回到面板重新发起。</p>"))
		return
	}
	if err := s.submitCode(code); err != nil {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte("<h2>这次登录已处理过</h2><p>授权码只能兑换一次，请回到面板重新发起。</p>"))
		return
	}
	_, _ = w.Write([]byte("<h2>已登录 DeepAI Code</h2><p>可以关掉这个标签页，回到面板。</p>"))
}

// submitCode 收下授权码（只允许第一次；重复提交给可读结论）。
func (s *session) submitCode(code string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.code != "" || s.result != nil {
		return errors.New("这次登录的授权码已经提交过（同一个 code 只能兑换一次）")
	}
	s.code = code
	return nil
}

// SubmitCallbackURL 人工提交回调地址（浏览器与网关不同机时的唯一通用解，R104）。
//
// 接受两种形态：**完整 URL**（地址栏直接复制）与**裸 query 串**（只复制了 `?` 后面的部分）。
// **只粘 code 会被拒**：拿不到 state 就无法确认它属于本次登录（等于允许任意 code 注入）。
func SubmitCallbackURL(raw string) error {
	sess := current()
	if sess == nil {
		return errors.New("没有进行中的 DeepAI 登录流程，请先在面板点「浏览器登录」")
	}
	sess.mu.Lock()
	expired := time.Since(sess.createdAt) > flowTTL
	sess.mu.Unlock()
	if expired {
		Shutdown()
		return errors.New("登录已超时（10 分钟），请重新发起")
	}
	code, state, err := parseCallback(raw)
	if err != nil {
		return err
	}
	if !stateEqual(state, sess.state) {
		return errors.New("回调里的 state 与本次登录不匹配：请粘贴本次登录后的地址栏 URL（不要只复制 code）")
	}
	if err := sess.submitCode(code); err != nil {
		return err
	}
	return nil
}

// stateEqual 常数时间比对 state（长度不同直接判否，避免把长度信息也当作比较维度）。
func stateEqual(a, b string) bool {
	if len(a) != len(b) || a == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func current() *session {
	curMu.Lock()
	defer curMu.Unlock()
	return cur
}

// replace 安装会话（同一时刻只允许一次登录：调用方 loginBusy 已保证，这里再挡一层）。
func replace(s *session) {
	curMu.Lock()
	old := cur
	cur = s
	curMu.Unlock()
	if old != nil && old.srv != nil {
		_ = old.srv.Close()
	}
}
