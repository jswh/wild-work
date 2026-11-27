// Package deepai 接入 DeepAI Code（`api.deepai.org` 的 `/deepai_code/*` 协议面）。
//
// 协议知识来源：MIT 项目 DeepAICode2API 的 `docs/recon.md` 与 `internal/deepai`
// 客户端行为（2026-11-27 快照，车道 8）。**只取协议事实，不搬代码**：认证流程、
// 请求头族、形状兜底规则、SSE 语义、额度字段都按本仓既有渠道的结构重写。
//
// 与上游的关系（本渠道的几条硬事实）：
//   - 鉴权头是 `Api-Key`（**不是** `Authorization`），值是一枚 UUID 形态的 API Key；
//   - 出站身份是**桌面端 Node fetch**：`User-Agent: node` + 三条 `sec-fetch`/`accept`
//     默认头，**没有**浏览器 UA、**没有** `sec-ch-ua*`（带浏览器指纹反而与真实客户端不一致）；
//   - `X-DeepAI-Code-Machine` = `sha256(小写原始机器 UUID)`：上游按它**按天发放免费额度**，
//     同机多账号共用一枚指纹会互相抵消，故每个账号各持一枚（见 `EnsureMachine`）；
//   - `X-DeepAI-Code-Theos` 是计费车道版本（当前 8）。车道不被支持时上游回
//     `426 lane_version_unsupported`，本工具**不探测、不猜测**车道，只把结论告诉用户去升级；
//   - 上游每轮只发增量，且 `model` 字段常是裸名——由本包回填成客户端请求的全串。
//
// 刻意不做的部分（详见 `.scratch/deepai/spec.md` §1）：`/connect/*` 控制面、
// `web_search` / `find_service`、`sessions/*`、`/responses`（只有 `gpt-6-astra` 走它）、
// 以及 Proof 头（桌面端自 0.3.52 起会带，但上游实测不校验，本仓不发）。
package deepai

import "time"

const (
	// KindName 渠道标识（= provider.Kind 的字符串值）。
	KindName = "deepai"

	// DefaultOrigin 上游基址。桌面端 0.3.46 起默认 `api2.deepai.org`，但 2026-11-20
	// 的复核对协议面零差异；本仓固定老基址，需要切换时改这一处。
	DefaultOrigin = "https://api.deepai.org"

	// PathModels 模型目录。**2026-11-14 起必须带 `Api-Key`**：不带一律 401 `auth_required`；
	// 带任意账号的 Key 即 200（目录不随账号变化）——因此它同时是「粘贴 Key」的零费用校验端点。
	PathModels = "/deepai_code/models"
	// PathChat 对话（OpenAI chat 兼容，`api=openai-responses` 的模型不走这里）。
	PathChat = "/deepai_code/chat/completions"
	// PathTheos 余额 + 签到挑战（配额票接入，见 credits.go）。
	PathTheos = "/deepai_code/theos"

	// LaneVersion 计费车道版本（`X-DeepAI-Code-Theos` 头的值）。
	//
	// 2026-11-25 上游把窗口收敛到 **7 / 8 通过、6 与 9 被拒**；2026-11-27 车道 8 可用。
	// 这是一个**快照常量**：上游再抬车道时表现为 426，本仓按「让用户升级」处理，
	// 不做向上猜测（探测式争取车道等于教用户去撞门槛，见 spec §3.1）。
	LaneVersion = "8"

	// AppVersion 桌面端版本快照（用于心跳/证明串的版本段，本票尚未发送）。
	AppVersion = "0.3.52"

	// UserAgent 出站 UA：桌面端是 Node 进程，`fetch` 的默认 UA 就是 `node`。
	UserAgent = "node"

	// 请求头名。集中一处，避免各调用点拼错（头名拼错不会报错，只会静默不生效）。
	HeaderKey      = "Api-Key"
	HeaderLane     = "X-DeepAI-Code-Theos"
	HeaderInstance = "X-DeepAI-Code-Instance"
	HeaderMachine  = "X-DeepAI-Code-Machine"
	HeaderAffinity = "X-Session-Affinity"
	HeaderCheckIn  = "X-DeepAI-Code-Check-In"
	HeaderBeacon   = "X-DeepAI-Code-Beacon"
	// HeaderProof 只**登记不发**：桌面端 0.3.52 起会在 /theos 与模型调用上带证明串，
	// 但上游 2026-11-25 实测不校验。发它需要复刻一个 attestation helper（不在本仓），
	// 所以这里只保留名字，供「为什么没这个头」的检索入口。
	HeaderProof = "X-DeepAI-Code-Proof"

	// defaultIdleTimeout 流中空闲上限（拿到响应头之后）：防「吐了几字节就彻底卡死」。
	// 不跟随 `upstream.timeout_seconds` 放宽——那种情况多等只是让用户多等（R58/R118 同口径）。
	defaultIdleTimeout = 90 * time.Second
	// defaultRequestTimeout 非流式请求的总超时上限（`upstream.timeout_seconds` 兜底）。
	// 流式 client **不设**总超时（R35）。
	defaultRequestTimeout = 120 * time.Second
	// maxErrBody 上游错误体读取上限（够放下 OpenAI 风格错误体 + 可读提示）。
	maxErrBody = 1 << 20
)

// FarFutureExpiry 返回远期过期时间（+80 年）。
//
// 本渠道凭据是一枚不轮换的 API Key，没有「过期时刻」概念；`ExpiresAt` 必须是远期值而不是
// 0 —— 0 会让 `NeedsRefresh` 恒真，进而每轮刷新都尝试 refresh（不变量 21 同款陷阱）。
func FarFutureExpiry() int64 { return time.Now().AddDate(80, 0, 0).Unix() }
