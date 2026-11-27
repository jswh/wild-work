package deepai

import (
	"bytes"
	"encoding/json"
	"strings"
)

// 形状兜底（反应式，只重试**一次**）。
//
// 上游在「node UA + 已知客户端指纹 + 请求体不像桌面端发的/缺 system」这一窄条件下回
// `400 This request is not one DeepAI Code sends.`（旧文案）或
// `400 This request cannot be accepted.`（2026-11-13 起），也可能只给
// `error.code == "invalid_request_error"`。
//
// 为什么必须**反应式**：兜底会补一条 system 消息，那是**改变采样条件**（等于给模型下系统
// 指令）。对每个「没有 system」的正常请求都预防式注入，等于悄悄改了用户的语义，
// 也违背「只透传声明/不编造」的纪律。正常请求根本不会命中这条 400，所以等到被拒再补。

// shapeSystemPrompt 是兜底时补的那条 system（与桌面端默认提示逐字一致）。
const shapeSystemPrompt = "You are a helpful AI assistant. Answer the user's request directly."

// shapeWhitelist 是桌面端实测字段并集（17 键，顺序即出站顺序）。
//
// 少一个字段就等于「比真实客户端少发」，而严格节点会把「多出来的未知字段」直接拒掉，
// 所以兜底时**只留白名单内的键**并按此顺序重排。
var shapeWhitelist = []string{
	"model", "messages", "tools", "tool_choice", "parallel_tool_calls",
	"max_tokens", "max_completion_tokens", "temperature", "top_p", "stop",
	"stream", "stream_options", "store", "seed", "service_tier",
	"reasoning_effort", "prompt_cache_key",
}

// shapeRejectReason 判断这次 400 是否为「形状拒绝」，命中返回可检索的原因串。
//
// 三条判据任一命中即算（文案会变，码能兜住）。非 400 一律不命中：其它 400 是真实参数错误，
// 重试同一份参数只会浪费一次往返。
func shapeRejectReason(status int, body []byte) string {
	if status != 400 {
		return ""
	}
	var probe struct {
		Error struct {
			Message string `json:"message"`
			Code    any    `json:"code"`
		} `json:"error"`
		Detail  string `json:"detail"`
		Message string `json:"message"`
	}
	text := string(body)
	if err := json.Unmarshal(body, &probe); err == nil {
		for _, s := range []string{probe.Error.Message, probe.Detail, probe.Message} {
			if s != "" {
				text = s
				break
			}
		}
	}
	switch {
	case strings.Contains(text, "This request is not one DeepAI Code sends."):
		return "copy_old"
	case strings.Contains(text, "This request cannot be accepted."):
		return "copy_new"
	}
	// 文案再换也能兜住：只认 code。用 RawMessage 比较避免把数字码/嵌套码误读。
	if err := json.Unmarshal(body, &probe); err == nil {
		if code, ok := probe.Error.Code.(string); ok && code == "invalid_request_error" {
			return "invalid_request_error"
		}
	}
	return ""
}

// reshapeBody 按桌面端字段白名单重排请求体，并在**缺** system/developer 时补一条默认 system。
//
// 三条硬纪律：
//   - 白名单外的键**丢弃**（严格节点会因未知字段拒请求）；
//   - 已有 system/developer ⇒ **绝不替换、绝不新增第二条**（用户的系统提示词是语义的一部分）；
//   - 解析失败原样返回（宁可把上游的第二个错误透传，也不要发出一个自己拼坏的 body）。
func reshapeBody(body []byte) []byte {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	if msgs, ok := obj["messages"]; ok {
		if fixed, changed := ensureSystemMessage(msgs); changed {
			obj["messages"] = fixed
		}
	}
	var buf bytes.Buffer
	buf.WriteByte('{')
	first := true
	for _, k := range shapeWhitelist {
		raw, ok := obj[k]
		if !ok {
			continue
		}
		if !first {
			buf.WriteByte(',')
		}
		first = false
		key, err := json.Marshal(k)
		if err != nil {
			return body
		}
		buf.Write(key)
		buf.WriteByte(':')
		buf.Write(bytes.TrimSpace(raw))
	}
	buf.WriteByte('}')
	return buf.Bytes()
}

// ensureSystemMessage 在缺 system/developer 时**前置**一条默认 system。
//
// 前置而不是追加：桌面端的 system 就是第一条（Tail 追加会让模型先读到用户内容再读指令）。
// 已有 system（任意位置，含 developer 归一后的 system）⇒ 原样返回、changed=false。
func ensureSystemMessage(raw json.RawMessage) (json.RawMessage, bool) {
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err != nil {
		return raw, false
	}
	for _, m := range list {
		var probe struct {
			Role string `json:"role"`
		}
		if err := json.Unmarshal(m, &probe); err != nil {
			continue
		}
		if strings.EqualFold(probe.Role, "system") || strings.EqualFold(probe.Role, "developer") {
			return raw, false
		}
	}
	sys, err := json.Marshal(map[string]any{"role": "system", "content": shapeSystemPrompt})
	if err != nil {
		return raw, false
	}
	out := make([]json.RawMessage, 0, len(list)+1)
	out = append(out, sys)
	out = append(out, list...)
	merged, err := json.Marshal(out)
	if err != nil {
		return raw, false
	}
	return merged, true
}
