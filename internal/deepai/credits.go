package deepai

import (
	"errors"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// 额度与签到（票 01 只留显式占位，票 04 落地）。
//
// 为什么返回错误而不是「0 积分、无明细」：0 会让面板把这条渠道渲染成
// 「0 可用积分」（用户以为额度用光），而实际是「本工具还没接这个端点」。
// 返回错误时面板显示「待刷新」并留上一份快照，语义如实。

// errCreditsNotWired 是票 01 阶段的显式占位错误（票 04 接入 `/theos` 后删除）。
var errCreditsNotWired = errors.New("deepai: 额度查询尚未接入（待配额票）")

// UserResource 返回可消耗余额。
//
// 票 04 的口径：`/theos` 的 `balance`（字符串浮点、可为负），四舍五入落整数。
func (c *Client) UserResource(a *auth.Auth) (int64, error) {
	return 0, errCreditsNotWired
}

// UserResourceDetail 返回余额 + 明细条目。
//
// 票 04 的口径：主条目 = 当前余额；到期列 v1 留空（`refresh_at` 是否等于「每日额度清零
// 时刻」未经只读探针确认前不写任何值，见 spec §7.1）。
func (c *Client) UserResourceDetail(a *auth.Auth) (int64, []provider.ResourceItem, error) {
	return 0, nil, errCreditsNotWired
}

// FetchModelPricing 本渠道无独立费率端点。
//
// `theosPerRequest` 是「每次请求的单价」（类似 lingxi 的倍率），不是按 token 计价的
// 费率表，塞进费率列只会误导；故返回空（面板费率页对该渠道不展示模型价格）。
func (c *Client) FetchModelPricing(a *auth.Auth) ([]provider.ModelPricing, error) {
	return nil, nil
}

// DailyCheckin 本渠道**没有独立签到任务**：签到是 `/theos` 探测的副作用
// （上一次下发的 nonce 在下一次探测时回传即算签到，见票 04 的 nonce 环）。
//
// 因此这里返回 nil 而不是 error：调度器在 `CheckinMinutes` 为空时本就不会调用它；
// 万一手工触发，也应当是「什么都不做且不写签到记录」（拓扑见 R150 的闸门）。
func (c *Client) DailyCheckin(a *auth.Auth) error { return nil }
