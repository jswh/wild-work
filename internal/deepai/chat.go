package deepai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"

	"wild-work/internal/auth"
	"wild-work/internal/idle"
	"wild-work/internal/upstream"
)

// ChatStream 对话请求：`POST {origin}/deepai_code/chat/completions`，OpenAI chat 兼容。
//
// 请求体走本仓统一管线 `upstream.PrepareBody`（不变量 1）：强制 `stream:true`、
// `developer→system`、`tool_choice` 归一、tool schema `$ref` 全量内联、脱敏，并在缺
// `stream_options` 时补 `{include_usage:true}`。上游的非流式面会撞 Cloudflare 100s 上限
// （实测 524），强制流式反而更稳：非流式客户端由 handler 用 Aggregate 还原成 JSON。
//
// 形状兜底（spec §3.5）：上游把「不像桌面端发的请求」拒成 400 时，按桌面端字段白名单重排
// 并在**缺** system 时补一条，**同一账号同一请求内重试恰好一次**；第二次仍失败就把第二个
// 错误体原样回传（首因进日志，不吞、不反复重试）。
//
// 超时口径（R35/R118）：流式走 StreamHTTP（**不设**总超时），拿到响应头之后由 90s 空闲
// 看门狗兜底。传输层错误**原样返回 error**（不计账号错误、不换号：同渠道所有账号打同一
// host，网络问题与账号健康无关）。
func (c *Client) ChatStream(a *auth.Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	prepared := upstream.PrepareBody(body)
	peek := peekChat(prepared)
	affinity := affinityOf(a, prepared)

	// 出口配置错误：**不**打到上游、也不静默退回直连（票 06 的验收口径：悄悄直连会让
	// 用户以为自己在用独立出口，实际所有号挤在同一 IP 上）。回合成的 400 让 handler
	// 走 Classify ⇒ ErrAccountFault（换号 + 面板可见），而不是当成网络抖动透传。
	if _, perr := NormalizeProxy(proxyOf(a)); perr != nil {
		log.Printf("deepai 出口配置有误 uid=%s proxy=%s err=%v（拒绝直连）",
			uidOf(a), proxyHostOnlyBestEffort(proxyOf(a)), perr)
		return nil, http.StatusBadRequest, []byte(
			`{"error":{"code":"proxy_misconfigured","message":"该账号的出口（代理）配置有误，已拒绝直连：请到「账号设置」改正或清空出口"}}`), nil
	}

	// 计量模型额度耗尽短路（spec §5）：恢复点之前不再打上游，直接回合成的 403。
	// 走合成体而不是 error，是为了让 handler 的 Classify 归到 ErrSoftRate（模型级冷却 +
	// 同请求内换号）。免费模型永远收不到 theos_exhausted，所以这里天然只拦计量模型。
	if synthetic, hit := c.shortCircuit(a, peek.Model); hit {
		return nil, http.StatusForbidden, synthetic, nil
	}

	rc, status, errBody, err := c.chatOnce(a, prepared, peek.Stream, affinity)
	if err != nil {
		return nil, 0, nil, err
	}
	if reason := shapeRejectReason(status, errBody); reason != "" {
		// 首因必须留痕：否则「第一次被拒过一次」这件事在生产日志里完全不可见。
		log.Printf("deepai shape fallback uid=%s status=%d reason=%s", uidOf(a), status, reason)
		rc, status, errBody, err := c.chatOnce(a, reshapeBody(prepared), peek.Stream, affinity)
		c.recordOutcome(a, peek.Model, status, errBody)
		return rc, status, errBody, err
	}
	if hint := hintFor(status, errBody); hint != "" {
		// 车道失配 / 漏发机器指纹这类「要人动手」的错误，日志必须醒目且带 uid。
		log.Printf("deepai chat rejected uid=%s status=%d %s err=%s", uidOf(a), status, hint, errorMessage(errBody))
	}
	c.recordOutcome(a, peek.Model, status, errBody)
	return rc, status, errBody, nil
}

// recordOutcome 用一次对话的结果推进账号状态（两项都只做「锦上添花」，不影响返回值）：
//   - 成功 ⇒ 记一次转发时刻（beacon 的 `pi` 字段据此判断客户端是否真在用）；
//   - 403 计量额度耗尽 ⇒ 在恢复点之前对该 (账号, 模型) 本地短路。
func (c *Client) recordOutcome(a *auth.Auth, model string, status int, body []byte) {
	uid := uidOf(a)
	if status < 300 && status > 0 {
		c.markRelay(uid)
		return
	}
	if status != http.StatusForbidden || !isTheosExhausted(body) {
		return
	}
	st := c.stateOf(uid)
	c.states.mu.Lock()
	until := st.refreshAt
	c.states.mu.Unlock()
	if !c.noteExhausted(uid, model, until) {
		log.Printf("deepai theos exhausted uid=%s model=%s 恢复点未知（尚未成功探测 /theos），本次不记短路", uid, model)
	}
}

// chatOnce 发一次对话请求（不含任何重试逻辑）。
func (c *Client) chatOnce(a *auth.Auth, body []byte, stream bool, affinity string) (io.ReadCloser, int, []byte, error) {
	req, err := c.newRequest(context.Background(), http.MethodPost, PathChat, a, body, affinity)
	if err != nil {
		return nil, 0, nil, err
	}
	req, cancel := idle.WithCancel(req)
	hc := c.HTTP
	if stream && c.StreamHTTP != nil {
		hc = c.StreamHTTP
	}
	resp, err := hc.Do(req)
	if err != nil {
		cancel()
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 300 {
		cancel()
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		_ = resp.Body.Close()
		// 上游 ≥400 原样透传（不变量 8）；3xx 同样是失败（ErrUseLastResponse 拦住了跳转，
		// 上游 Key 失效时就是 302 跳登录页）——否则会把一个 HTML 登录页当 SSE 读。
		// 处置由 handler 的 Classify 决定（3xx ⇒ session_dead）。
		return nil, resp.StatusCode, errBody, nil
	}
	return idle.Monitor(resp.Body, c.IdleTimeout(), cancel), resp.StatusCode, nil, nil
}

// Stream 逐帧转发上游 SSE：回填 `model`（客户端全串）与 `reasoning_content`（下游只认
// 这个字段，见 gateway/chat_sse.go），其余内容逐字节透传。
func (c *Client) Stream(w http.ResponseWriter, r io.Reader, model string) (map[string]any, error) {
	var usage map[string]any
	err := upstream.StreamCapture(w, rewriteModelReader(r, model), func(u map[string]any) { usage = u })
	return usage, err
}

// Aggregate 非流式：上游若回普通 JSON 就解成 map；若回的是 SSE（客户端发 `stream=false`
// 但管线强制流式）就聚合 SSE 帧还原成一条完整回答。两条路径都回填 `model` 与
// `message.reasoning_content`。
func (c *Client) Aggregate(r io.Reader, model string) (map[string]any, error) {
	br := bufio.NewReaderSize(r, 64*1024)
	for {
		b, err := br.Peek(1)
		if err != nil {
			return nil, err
		}
		// 跳过 JSON 之前的空白（上游可能在前面加 BOM/空行）。
		if b[0] == ' ' || b[0] == '\t' || b[0] == '\r' || b[0] == '\n' || b[0] == 0xef {
			_, _ = br.ReadByte()
			continue
		}
		if b[0] == '{' {
			var resp map[string]any
			if err := json.NewDecoder(br).Decode(&resp); err != nil {
				return nil, err
			}
			return finalizeAggregate(resp, model), nil
		}
		break
	}
	resp, err := upstream.Aggregate(br)
	if err != nil {
		return nil, err
	}
	return finalizeAggregate(resp, model), nil
}

// finalizeAggregate 回填 model 与 `message.reasoning_content`（已有则不覆盖）。
func finalizeAggregate(resp map[string]any, model string) map[string]any {
	if resp == nil {
		return resp
	}
	if model != "" {
		resp["model"] = model
	}
	choices, _ := resp["choices"].([]any)
	for _, ch := range choices {
		choice, ok := ch.(map[string]any)
		if !ok {
			continue
		}
		msg, ok := choice["message"].(map[string]any)
		if !ok {
			continue
		}
		backfillReasoningContent(msg)
	}
	return resp
}

// backfillReasoningContent 把 `reasoning` 复制一份到 `reasoning_content`（**已有不覆盖**）。
//
// 为什么需要：上游把思考链放在 `reasoning`，而本仓兼容层（gateway/chat_sse.go）只认
// `reasoning_content`——不补这一份，Claude Code 一类客户端看不到任何思考内容。
// 只在「reasoning 是非空字符串 且 reasoning_content 不存在」时才写，绝不改语义。
func backfillReasoningContent(container map[string]any) {
	if container == nil {
		return
	}
	if _, has := container["reasoning_content"]; has {
		return
	}
	switch v := container["reasoning"].(type) {
	case string:
		if v != "" {
			container["reasoning_content"] = v
		}
	}
}

// rewriteModelReader 包一层「逐行改写」的 reader（model 为空时逻辑照跑：
// 只补 reasoning_content，不回填 model）。
func rewriteModelReader(r io.Reader, model string) io.Reader {
	return &modelRewriter{src: bufio.NewReaderSize(r, 64*1024), model: model, want: quotedJSON(model)}
}

// quotedJSON 把字符串编码成 JSON 字面量（一次算好，逐帧复用）；空串返回 nil。
func quotedJSON(s string) []byte {
	if s == "" {
		return nil
	}
	b, err := json.Marshal(s)
	if err != nil {
		return nil
	}
	return b
}

type modelRewriter struct {
	src     *bufio.Reader
	model   string
	want    []byte // quotedJSON(model)，逐帧复用（避免每帧一次 Marshal）
	pending []byte
}

func (m *modelRewriter) Read(p []byte) (int, error) {
	for len(m.pending) == 0 {
		line, err := m.src.ReadBytes('\n')
		if len(line) > 0 {
			m.pending = rewriteFrame(line, m.want)
		}
		if err != nil {
			if len(m.pending) == 0 {
				return 0, err
			}
			break
		}
	}
	n := copy(p, m.pending)
	m.pending = m.pending[n:]
	return n, nil
}

// rewriteFrame 改写一行 SSE 里 `data:` 负载：回填 `model`、补 `reasoning_content`。
//
// 只动**对话增量帧**（负载是 JSON 且含顶层 `choices`）：`[DONE]`、`event:` 行、空行、
// 非 JSON 负载一律原样返回；**不需要改写时返回原始字节切片**（零保真损失：字节级透传）。
// 需要改写时才重新序列化该帧——同帧其它字段的**值**不变（`json.RawMessage` 保留原始
// 字节），只有键序与空白被规范化，客户端解析结果完全一致。
//
// 不用「字符串替换 `"model":"…"`」的做法：`model` 也可能出现在 tool arguments 里
// （`"model": "x"`），字符串替换会改坏工具参数。
func rewriteFrame(line []byte, want []byte) []byte {
	trimmed := bytes.TrimRight(line, "\r\n")
	tail := line[len(trimmed):]
	var payload []byte
	switch {
	case bytes.HasPrefix(trimmed, []byte("data: ")):
		payload = trimmed[len("data: "):]
	case bytes.HasPrefix(trimmed, []byte("data:")):
		payload = trimmed[len("data:"):]
	default:
		return line
	}
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 || payload[0] != '{' {
		return line
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(payload, &obj); err != nil {
		return line
	}
	if _, ok := obj["choices"]; !ok {
		return line
	}
	changed := false
	if want != nil {
		if cur, ok := obj["model"]; !ok || !bytes.Equal(bytes.TrimSpace(cur), want) {
			obj["model"] = want
			changed = true
		}
	}
	if fixed, ok := backfillFrameReasoning(obj); ok {
		obj["choices"] = fixed
		changed = true
	}
	if !changed {
		return line
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return line
	}
	buf := make([]byte, 0, len(out)+len(tail)+6)
	buf = append(buf, "data: "...)
	buf = append(buf, out...)
	buf = append(buf, tail...)
	return buf
}

// backfillFrameReasoning 给每个 choice 的 `delta` 补 `reasoning_content`（已有不覆盖）。
//
// 返回改写后的 choices 原文与「是否改动」；未改动时调用方应保持整帧字节不变。
func backfillFrameReasoning(obj map[string]json.RawMessage) (json.RawMessage, bool) {
	rawChoices, ok := obj["choices"]
	if !ok {
		return nil, false
	}
	var choices []map[string]json.RawMessage
	if err := json.Unmarshal(rawChoices, &choices); err != nil {
		return nil, false
	}
	changed := false
	for _, ch := range choices {
		rawDelta, ok := ch["delta"]
		if !ok {
			continue
		}
		var delta map[string]json.RawMessage
		if err := json.Unmarshal(rawDelta, &delta); err != nil {
			continue
		}
		if _, has := delta["reasoning_content"]; has {
			continue // 上游已给：绝不覆盖
		}
		reasoning, has := delta["reasoning"]
		if !has || !isNonEmptyJSONString(reasoning) {
			continue
		}
		delta["reasoning_content"] = bytes.TrimSpace(reasoning)
		merged, err := json.Marshal(delta)
		if err != nil {
			continue
		}
		ch["delta"] = merged
		changed = true
	}
	if !changed {
		return nil, false
	}
	merged, err := json.Marshal(choices)
	if err != nil {
		return nil, false
	}
	return merged, true
}

// isNonEmptyJSONString 判断一段 JSON 原文是否为「非空字符串字面量」。
func isNonEmptyJSONString(raw json.RawMessage) bool {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return false
	}
	return s != ""
}
