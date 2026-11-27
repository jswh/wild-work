package deepai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

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
// 为什么还要这张快照：目录是渠道维度的、且可能拿不到（无账号 / 网络 / 5xx / 空目录）。
// 没有兜底表时整条渠道会从面板消失；有它则最坏情况是「列出一份陈旧但可用的清单」。
// `FetchModels` 是「先拉上游、失败回退本表」，本表是唯一兜底真源。
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

// catalogModel 是上游 `/models` 里单条目（字段并集见 recon §2.2：
// `id,name,reasoning,input,contextWindow,maxTokens,premier,theosPerRequest,api`）。
//
// `premier` 与 `theosPerRequest` 不映射到 ModelInfo：本渠道**没有按 token 计的费率表**
// （`theosPerRequest` 是「每次请求」的单价），塞进费率列就是错误口径（见 FetchModelPricing）。
type catalogModel struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Reasoning     bool     `json:"reasoning"`
	Input         []string `json:"input"`
	ContextWindow int64    `json:"contextWindow"`
	MaxTokens     int64    `json:"maxTokens"`
	API           string   `json:"api"`
}

// catalogPayload 是目录信封：`{models:[…], default:"…"}`。
//
// `default` 不映射：provider.ModelInfo 没有「默认模型」概念（默认值只在渠道内部兜底，
// 客户端选哪个模型由它自己决定）。
type catalogPayload struct {
	Models  []catalogModel `json:"models"`
	Default string         `json:"default"`
}

// responsesOnlyAPI 是「只认 /responses 端点」的 api 值：本仓不实现该端点，
// 列出来会让客户端拿到必然 400 的模型 id（上游会回
// `Model '<id>' is not available on this endpoint`）。
const responsesOnlyAPI = "openai-responses"

// FetchModels 拉取模型目录（`GET /deepai_code/models`，必须带账号 Key —— 不带一律 401）。
//
// 三条纪律（本票的核心）：
//
//  1. **剔除 responses-only 条目并留日志**（`deepai models: skip responses-only model=<id>`）：
//     静默消失会让用户以为「上游下架了」，而实际是本仓不做 `/responses`。
//  2. **失败退回编译快照**（网络 / 5xx / 空目录 / 解析失败）：目录是**渠道维度**的，
//     一次上游抖动不该把整条渠道变成「没有模型」；返回快照 + 打日志
//     `deepai models: fallback to snapshot reason=…`（不写盘、不在渠道内建缓存——
//     缓存与 1h TTL 由 server 层统一负责）。
//  3. **登录态失效（401/3xx）返回类型化错误**而不是快照：这是**账号**维度的问题，
//     交给 server 层既有的机制处理（R133 换下一个候选账号重试 + 负缓存 + 最后
//     静态兜底；分类是 `ErrSessionDead` ⇒ 走 R76 的可自愈通道，**不** Disable 账号）。
//     若这里也吞成快照，「第一个账号凭据死了」就会把整条渠道钉在陈旧目录上，
//     健康账号永远轮不到。其余失败（网络/5xx/空目录）与账号无关，换号也没有意义，
//     故不做无谓的 3 次尝试。
func (c *Client) FetchModels(a *auth.Auth) ([]provider.ModelInfo, error) {
	if a == nil {
		return nil, &provider.Error{Kind: provider.ErrSessionDead, Msg: "deepai: 缺少账号凭据"}
	}
	req, err := c.newRequest(context.Background(), http.MethodGet, PathModels, a, nil, "")
	if err != nil {
		return snapshotFallback("request", err), nil
	}
	body, status, errBody, err := c.do(req, c.HTTP)
	if err != nil {
		// 传输层错误：同渠道所有账号打的是同一个 host，换号没有意义。
		return snapshotFallback("network", err), nil
	}
	if body == nil {
		kind := classifyStatus(c, status, string(errBody))
		if kind == provider.ErrSessionDead {
			// 账号维度：让 server 层换号重试（R133），并把「凭据失效」如实上报（R76 自愈）。
			return nil, &provider.Error{Kind: kind, Status: status, Msg: errorMessage(errBody)}
		}
		return snapshotFallback(fmt.Sprintf("http_%d", status), errors.New(errorMessage(errBody))), nil
	}
	defer body.Close()
	raw, err := io.ReadAll(io.LimitReader(body, maxResponseBytes))
	if err != nil {
		return snapshotFallback("read", err), nil
	}
	infos, skipped := parseCatalog(raw)
	if len(infos) == 0 {
		return snapshotFallback(fmt.Sprintf("empty_catalog(skipped=%d)", skipped), nil), nil
	}
	log.Printf("deepai models: fetched n=%d skipped_responses_only=%d", len(infos), skipped)
	return infos, nil
}

// parseCatalog 解析目录信封并按本仓口径映射成 ModelInfo。
//
// 返回 (条目, 被剔除的 responses-only 数量)。空 id 与重复 id 直接跳过（上游理论不会给，
// 但一条坏记录不该让整张目录解析失败）。
func parseCatalog(raw []byte) ([]provider.ModelInfo, int) {
	var payload catalogPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, 0
	}
	out := make([]provider.ModelInfo, 0, len(payload.Models))
	seen := make(map[string]bool, len(payload.Models))
	skipped := 0
	for _, m := range payload.Models {
		id := strings.TrimSpace(m.ID)
		if id == "" || seen[id] {
			continue
		}
		if m.API == responsesOnlyAPI {
			// 可检索：这条日志回答「为什么某个模型没出现在目录里」。
			log.Printf("deepai models: skip responses-only model=%s", id)
			skipped++
			continue
		}
		seen[id] = true
		name := strings.TrimSpace(m.Name)
		if name == "" {
			name = id
		}
		out = append(out, provider.ModelInfo{
			ID:   id,
			Name: name,
			// 目录值原样透传：ContextFromAPI 表达「这是上游声明」，缺失（0）时面板不展示，
			// 不再用快照值顶替（否则就是把快照冒充成实时值）。
			ContextWindow:     m.ContextWindow,
			MaxTokens:         m.MaxTokens,
			ContextFromAPI:    m.ContextWindow > 0,
			SupportsImages:    hasModality(m.Input, "image"),
			SupportsTools:     true, // 上游回标准 OpenAI tool_calls（recon §5.2 实测）
			SupportsReasoning: m.Reasoning,
		})
	}
	return out, skipped
}

// hasModality 判断目录 `input` 数组是否含某模态（大小写不敏感）。
func hasModality(input []string, want string) bool {
	for _, v := range input {
		if strings.EqualFold(strings.TrimSpace(v), want) {
			return true
		}
	}
	return false
}

// snapshotFallback 记录「为什么走了快照」并返回编译快照（27 条）。
//
// 不挂在 Client 上：它不需要任何客户端状态（纯日志 + 快照），挂上去只会让人以为
// 「换个 Client 就会换一份快照」。
//
// reason 是稳定前缀，便于日志检索；err 可为 nil（如空目录那条）。
func snapshotFallback(reason string, err error) []provider.ModelInfo {
	if err != nil {
		log.Printf("deepai models: fallback to snapshot reason=%s err=%v", reason, err)
	} else {
		log.Printf("deepai models: fallback to snapshot reason=%s", reason)
	}
	return StaticModels()
}
