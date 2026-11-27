package deepai

import (
	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// 静态模型目录（编译期快照）。
//
// 取值依据：上游 `/models` 的 2026-11-27 快照（车道 8）+ 同日全量模型指纹记录。
// 逐字段口径（**不编造**，R49）：
//   - `ContextWindow`：目录声明值——除 `glyph-cluster`（256,000，260k 会被上游硬拒）
//     与 `claude-haiku-5-5`（95,000）外，全目录都是 320,000；
//   - `MaxTokens`：仅两处有实测/目录证据（`glm-5.3-flash` 131,072、
//     `claude-haiku-5-5` 64,000）。其余 25 个**留 0（未声明）**——不知道就不填，
//     填一个猜的值会让客户端按错误上限组请求；
//   - `SupportsImages`：目录里 `input` 含 image 的 23 个；纯文本仅 5 个
//     （`deepai-cyber` / `glm-5.3` / `glyph-cluster` / `solar-mini-4` / `minimax-m3`）；
//   - `SupportsTools`：全目录 true（桌面端每个对话请求都带 `tools`，上游回标准
//     OpenAI 形状的 `tool_calls` + `finish_reason: tool_calls`）；
//   - `SupportsReasoning`：只标**有证据**的三个（`glm-5.3-flash` 是桌面端默认条目的
//     `reasoning:true`；`deepai-cyber` / `muse-ember` 在桌面端 `thinkingLevelMap` 里
//     暴露档位，且 2026-11-15 实测六档全收）。其余模型是否支持思考**未取证**，
//     故不声明——票 02 的动态目录会带上游的真实标注。
//
// 为什么还要这张快照：目录是渠道维度的、且可能在启动瞬间拿不到（无账号/网络失败）。
// 没有兜底表时整条渠道会从面板消失；有它则最坏情况是「列出一份陈旧但可用的清单」。
// 票 02 落地后 `FetchModels` 改为「先拉上游、失败回退本表」，本表仍是唯一兜底真源。
//
// `gpt-6-astra`（`api=openai-responses`，付费 50）**不在表内**：它只认 `/responses`
// 端点，本仓不实现该端点，列出来会让客户端拿到必然 400 的模型 id（剔除逻辑见票 02）。
var staticCatalog = buildCatalog([]modelSeed{
	// 免费 12（theosPerRequest "0"）
	{id: "glm-5.3-flash", ctx: 320000, max: 131072, img: true, reason: true},
	{id: "deepseek-v4.1-flash", ctx: 320000, img: true},
	{id: "mimo-2.6-flash", ctx: 320000, img: true},
	{id: "qwen-3.8-flash", ctx: 320000, img: true},
	{id: "minimax-m3.1-flash", ctx: 320000, img: true},
	{id: "muse-ember", ctx: 320000, img: true, reason: true},
	{id: "solar-mini-4", ctx: 320000},
	{id: "gpt-6-luna", ctx: 320000, img: true},
	{id: "claude-haiku-5-5", ctx: 95000, max: 64000, img: true},
	{id: "mimo-2.6-pro", ctx: 320000, img: true},
	{id: "glyph-cluster", ctx: 256000},
	{id: "deepai-cyber", ctx: 320000, reason: true},
	// 付费 16（按 theosPerRequest 升序）
	{id: "minimax-m3", ctx: 320000},
	{id: "glm-5.3", ctx: 320000},
	{id: "kimi-k3", ctx: 320000, img: true},
	{id: "kimi-k3.1-pro", ctx: 320000, img: true},
	{id: "gpt-6.1-terra", ctx: 320000, img: true},
	{id: "muse-spark-1.3", ctx: 320000, img: true},
	{id: "gemini-4-hydrogen", ctx: 320000, img: true},
	{id: "gpt-6.1-sol", ctx: 320000, img: true},
	{id: "gemini-4-argon", ctx: 320000, img: true},
	{id: "gemini-4-carbon", ctx: 320000, img: true},
	{id: "claude-sonnet-5-5", ctx: 320000, img: true},
	{id: "claude-opus-5-5", ctx: 320000, img: true},
	{id: "muse-wonder", ctx: 320000, img: true},
	{id: "qwen-3.8-max-prime", ctx: 320000, img: true},
	{id: "claude-fable-5-2", ctx: 320000, img: true},
})

// modelSeed 是快照条目的紧凑写法（tools 恒 true，故不进结构体）。
type modelSeed struct {
	id     string
	ctx    int64
	max    int64
	img    bool
	reason bool
}

func buildCatalog(seeds []modelSeed) []provider.ModelInfo {
	out := make([]provider.ModelInfo, 0, len(seeds))
	for _, s := range seeds {
		out = append(out, provider.ModelInfo{
			// 上游还下发 `name`（人类可读名）；快照里没有逐条记录，用 id 兜底而不是
			// 自己造一个（"GLM 5.3 Flash" 这类拼装就是编造）。票 02 的动态目录会带真名。
			ID:   s.id,
			Name: s.id,
			// ContextFromAPI 表达「这是上游声明值」——快照的确取自上游目录（非估算），
			// 只是冻结在编译期。无证据的字段一律 0/false，面板据此不展示。
			ContextWindow:     s.ctx,
			MaxTokens:         s.max,
			ContextFromAPI:    s.ctx > 0,
			SupportsImages:    s.img,
			SupportsTools:     true,
			SupportsReasoning: s.reason,
		})
	}
	return out
}

// StaticModels 返回静态兜底目录（server.Runtime.StaticModels 用）。
//
// 返回副本：调用方（handler 的模型过滤/面板）会改写条目，共享同一个底层数组
// 会让一次改写污染后续所有请求。
func StaticModels() []provider.ModelInfo {
	out := make([]provider.ModelInfo, len(staticCatalog))
	copy(out, staticCatalog)
	return out
}

// FetchModels 拉取模型目录。
//
// **票 01 阶段：不联网**，直接返回编译快照。理由是「一次能出字的对话」是这一票的
// 验收目标，先把鉴权/头族/流式打通；动态目录（含 `api=openai-responses` 剔除、
// 上游失败回退本表、401 自愈）是票 02 的交付物，届时本函数改为「拉上游 → 失败回退」。
func (c *Client) FetchModels(a *auth.Auth) ([]provider.ModelInfo, error) {
	if a == nil {
		return nil, &provider.Error{Kind: provider.ErrSessionDead, Msg: "deepai: 缺少账号凭据"}
	}
	return StaticModels(), nil
}
