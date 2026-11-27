package deepai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
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
// 超时口径（R35/R118）：流式走 StreamHTTP（**不设**总超时），拿不到响应头之前由
// 非流式的 `upstream.timeout_seconds` 之外的传输层兜底；拿到响应头之后由 90s 空闲
// 看门狗兜底。传输层错误**原样返回 error**（不计账号错误、不换号：同渠道所有账号打同一
// host，网络问题与账号健康无关）。
func (c *Client) ChatStream(a *auth.Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	prepared := upstream.PrepareBody(body)
	peek := peekChat(prepared)
	req, err := c.newRequest(context.Background(), http.MethodPost, PathChat, a, prepared, affinityOf(a, prepared))
	if err != nil {
		return nil, 0, nil, err
	}
	req, cancel := idle.WithCancel(req)
	hc := c.HTTP
	if peek.Stream && c.StreamHTTP != nil {
		hc = c.StreamHTTP
	}
	resp, err := hc.Do(req)
	if err != nil {
		cancel()
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		cancel()
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
		_ = resp.Body.Close()
		// 上游 ≥400 原样透传（不变量 8）：处置由 handler 的 Classify 决定。
		return nil, resp.StatusCode, errBody, nil
	}
	return idle.Monitor(resp.Body, c.IdleTimeout(), cancel), resp.StatusCode, nil, nil
}

// Stream 逐帧转发上游 SSE，并把每帧的 `model` 回填成客户端请求的全串。
//
// 为什么必须回填（provider.go 的 Stream 契约）：上游回的常是裸名，客户端看到
// `deepai/gpt-6-luna` 变成 `gpt-6-luna` 会以为模型被换掉了。回填只发生在**确实需要改**
// 的帧上，其余字节逐帧透传（含 `[DONE]`、注释帧、`event:` 帧、usage 末帧）。
func (c *Client) Stream(w http.ResponseWriter, r io.Reader, model string) (map[string]any, error) {
	var usage map[string]any
	err := upstream.StreamCapture(w, rewriteModelReader(r, model), func(u map[string]any) { usage = u })
	return usage, err
}

// Aggregate 非流式：上游若回普通 JSON 就解成 map 并回填 model；若回的是 SSE
// （客户端发 `stream=false` 但管线强制流式）就聚合 SSE 帧还原成一条完整回答。
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
			if model != "" {
				resp["model"] = model
			}
			return resp, nil
		}
		break
	}
	resp, err := upstream.Aggregate(br)
	if err != nil {
		return nil, err
	}
	if model != "" {
		resp["model"] = model
	}
	return resp, nil
}

// rewriteModelReader 包一层「逐行改写 model 字段」的 reader（model 为空时原样返回）。
func rewriteModelReader(r io.Reader, model string) io.Reader {
	if model == "" {
		return r
	}
	return &modelRewriter{src: bufio.NewReaderSize(r, 64*1024), model: model}
}

type modelRewriter struct {
	src     *bufio.Reader
	model   string
	pending []byte
}

func (m *modelRewriter) Read(p []byte) (int, error) {
	for len(m.pending) == 0 {
		line, err := m.src.ReadBytes('\n')
		if len(line) > 0 {
			m.pending = rewriteModelLine(line, m.model)
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

// rewriteModelLine 改写一行 SSE 里 `data:` 负载的 `model` 字段。
//
// 只动**对话增量帧**（负载是 JSON 且含顶层 `choices`）：`[DONE]`、`event:` 行、
// 空行、非 JSON 负载一律原样返回；帧里 `model` 已是目标值也原样返回（字节级透传）。
// 需要改写时才重新序列化该帧——同帧其它字段的**值**不变（`json.RawMessage` 保留
// 原始字节），只有键序与空白被规范化，客户端解析结果完全一致。
//
// 不用「字符串替换 `"model":"…"`」的做法：`model` 也可能出现在 tool arguments 里
// （`"model": "x"`），字符串替换会改坏工具参数。
func rewriteModelLine(line []byte, model string) []byte {
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
	want, err := json.Marshal(model)
	if err != nil {
		return line
	}
	if cur, ok := obj["model"]; ok && bytes.Equal(bytes.TrimSpace(cur), want) {
		return line
	}
	obj["model"] = want
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
