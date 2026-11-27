package deepai

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"wild-work/internal/auth"
)

// applyHeaders 写入一次出站请求的全部头。
//
// 顺序与取值都对齐桌面端（Node fetch）：三条 fetch 默认头 + `User-Agent: node`、
// `Content-Type: application/json`（**无 body 的 GET 也带**，桌面端对 /models、/theos
// 一视同仁）、车道版本、实例号、机器指纹，最后才是鉴权头 `Api-Key`。
//
// 刻意**不发**的头：`Authorization`（上游只认 Api-Key）、`sec-ch-ua*` 与浏览器 UA
// （那是网页注册链路的指纹，带到推理面等于自报混用了两种客户端）、
// `X-DeepAI-Code-Proof`（桌面端 0.3.52 起会带，但上游实测不校验，本仓不发）。
//
// affinity 只在对话请求上非空（会话粘性），`/models`、`/theos` 不带。
func applyHeaders(req *http.Request, a *auth.Auth, affinity string) {
	h := req.Header
	h.Set("Content-Type", "application/json")
	h.Set("User-Agent", UserAgent)
	h.Set("Accept", "*/*")
	h.Set("Accept-Language", "*")
	h.Set("Sec-Fetch-Mode", "cors")
	h.Set(HeaderLane, LaneVersion)
	if a == nil {
		return
	}
	a.RLock()
	key, uid := a.AccessToken, a.UID
	a.RUnlock()
	if inst := instanceID(uid); inst != "" {
		h.Set(HeaderInstance, inst)
	}
	if fp := accountMachine(a); fp != "" {
		h.Set(HeaderMachine, fp)
	}
	if affinity != "" {
		h.Set(HeaderAffinity, affinity)
	}
	if key != "" {
		h.Set(HeaderKey, key)
	}
}

// chatPeek 从（已预处理过的）请求体里取渠道需要的最小信息：
// 只关心 `stream`（决定用流式还是非流式 client）。`model` 由调用方显式传入，不从这里取。
type chatPeek struct {
	Stream bool `json:"stream"`
}

func peekChat(body []byte) chatPeek {
	var p chatPeek
	_ = json.Unmarshal(body, &p) // 解析失败不阻断：body 已由 PrepareBody 过一遍，非法时上游会回 400
	return p
}

// affinityOf 派生会话粘性值 `X-Session-Affinity`。
//
// 口径（spec §3.4）：`sha256(uid + "\x00" + 首条 user 文本)` 前 32 hex。
// 掺 uid 是刻意的——同一段对话被多个账号服务时不得共用会话 id，那才是可关联的键；
// 首条 user 文本是「会话锚点」：后续轮次追加历史时它不变，于是同一段对话在整个生命周期内
// 稳定落在同一会话上（上游据此做 prompt cache 亲和与并发限流计数）。
//
// 取不到文本时返回空串（本次不带该头，不编造一个随机值——随机值等于每轮新建会话）。
func affinityOf(a *auth.Auth, body []byte) string {
	if a == nil {
		return ""
	}
	a.RLock()
	uid := a.UID
	a.RUnlock()
	if uid == "" {
		return ""
	}
	anchor := firstUserText(body)
	if anchor == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(uid + "\x00" + anchor))
	return hex.EncodeToString(sum[:16])
}

// firstUserText 取请求体里**首条 user 消息**的纯文本（作为会话锚点）。
//
// content 有两种合法形态：裸字符串，或 `[{type:"text",text:"…"}, …]` 分片数组
// （带图/带缓存断点时上游与客户端都用后者）。分片里只拼文本，图不参与锚点——
// 图片是 base64，掺进哈希会让同一段对话在图片重编码后换会话。
func firstUserText(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var probe struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	for _, m := range probe.Messages {
		if !strings.EqualFold(m.Role, "user") {
			continue
		}
		if txt := textOfContent(m.Content); txt != "" {
			return txt
		}
	}
	return ""
}

// textOfContent 把 content 的两种形态归一成文本（裸串 / 分片数组里的 text 段）。
func textOfContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type != "" && p.Type != "text" {
			continue
		}
		if p.Text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(p.Text)
	}
	return strings.TrimSpace(b.String())
}

// uidOf 锁内取 uid（日志用；零凭据口径只允许打 uid）。
func uidOf(a *auth.Auth) string {
	if a == nil {
		return ""
	}
	a.RLock()
	defer a.RUnlock()
	return a.UID
}
