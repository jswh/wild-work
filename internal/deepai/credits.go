package deepai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// 额度（`/theos`）与签到副作用。
//
// 上游把「余额」与「签到」放在同一个端点：`GET /deepai_code/theos` 回
// `{enabled, balance, granted, refresh_at, lane_version, check_in:{nonce, seconds}}`。
// 上一轮下发的 `nonce` 在下一轮请求里回传（`X-DeepAI-Code-Check-In`）就是上游认定的
// 「签到」——本渠道**没有独立签到任务**（spec §7.2）。
//
// 余额是**字符串浮点**（可以是负数，实测见过 -7.58），四舍五入落整数；到期列 v1 一律留空：
// 「每日额度是否在 refresh_at 清零」没有经只读探针确认（owner O2 未批），写了就是编造，
// 会把上游的到期清零记成「今日消耗」（R101/R107 的坑，spec §7.1）。

// theosPath 余额 + 签到 nonce 端点。
const theosPath = "/deepai_code/theos"

// theosItemName 明细条目名。**唯一真源**：ledger 差分以 Name 为伪键，改名会产生假流水。
const theosItemName = "Theos"

// errCreditsUnreadable 余额字段读不出来（协议可能已变）：上层保留旧快照标「待刷新」。
var errCreditsUnreadable = errors.New("deepai: /theos 余额无法解析")

// theosInfo 是 `/theos` 的响应（字段名与上游原样，`balance`/`granted` 是字符串）。
type theosInfo struct {
	Enabled     bool   `json:"enabled"`
	Balance     string `json:"balance"`
	Granted     string `json:"granted"`
	RefreshAt   string `json:"refresh_at"`
	LaneVersion int    `json:"lane_version"`
	CheckIn     *struct {
		Nonce   string `json:"nonce"`
		Seconds int    `json:"seconds"`
	} `json:"check_in"`
}

// UserResource 返回可消耗余额（可为负 = 已超额）。
func (c *Client) UserResource(a *auth.Auth) (int64, error) {
	info, err := c.probeTheos(a)
	if err != nil {
		return 0, err
	}
	return balanceInt(info.Balance)
}

// UserResourceDetail 返回余额 + 明细条目。
//
// 口径（spec §7.1）：`Total = Remain`、`Used = 0`——上游没有「已用」口径，**不编造**。
// 于是 `ledger` 的 `channelBooksUsage` 门槛②对该渠道不成立（它按 `Total != Remain` 判），
// 余额下降仍按既有差分归因，不引入「池子缩水」第二套语义。
func (c *Client) UserResourceDetail(a *auth.Auth) (int64, []provider.ResourceItem, error) {
	info, err := c.probeTheos(a)
	if err != nil {
		return 0, nil, err
	}
	remain, err := balanceInt(info.Balance)
	if err != nil {
		return 0, nil, err
	}
	items := []provider.ResourceItem{{
		Name:   theosItemName,
		Total:  remain,
		Used:   0,
		Remain: remain,
		// 到期 v1 留空：refresh_at 与「每日额度清零」的等价关系未实证（spec §7.1）。
		ExpireAt: "",
		Usable:   true,
		Raw:      info.Balance,
	}}
	return remain, items, nil
}

// probeTheos 打一次 `/theos`，并把签到环（nonce 回传 / 静默锁）推进一格。
//
// 失败语义（spec §7.2）：
//   - `check_in_refused` ⇒ 6h 静默锁 + 一行日志 + 返回**内存里的上一份余额**（不报错、
//     不清快照）——签到被拒不代表余额读不出来，把它升级成「待刷新」是噪音；
//   - 其它失败（网络 / 5xx / 401 / 解析失败）⇒ 原样返回错误，上层标记「待刷新」并保留旧快照。
func (c *Client) probeTheos(a *auth.Auth) (theosInfo, error) {
	uid := uidOf(a)
	now := time.Now()
	nonce, beacon, due := c.beginCheckin(uid, now)

	req, err := c.newRequest(context.Background(), http.MethodGet, theosPath, a, nil, "")
	if err != nil {
		return theosInfo{}, err
	}
	req.Header.Set(HeaderBeacon, beacon)
	if due && nonce != "" {
		// 回传即签到：只有拿到过上一轮 nonce 才发这个头（首次探测没有可回传的东西）。
		req.Header.Set(HeaderCheckIn, nonce)
	}
	body, status, errBody, err := c.do(req, c.HTTP)
	if err != nil {
		return theosInfo{}, err
	}
	if status >= 300 {
		msg := errorMessage(errBody)
		if status == http.StatusForbidden && isCheckInRefused(errBody) {
			c.lockCheckin(uid, now, checkinRefusedLock)
			log.Printf("deepai checkin refused uid=%s lock=%s（保留上次余额，不标记待刷新）", uid, checkinRefusedLock)
			return c.lastTheosInfo(uid)
		}
		return theosInfo{}, typedError(status, msg)
	}
	defer body.Close()
	var info theosInfo
	if err := json.NewDecoder(io.LimitReader(body, maxResponseBytes)).Decode(&info); err != nil {
		return theosInfo{}, errCreditsUnreadable
	}
	c.finishCheckin(uid, now, info, due)
	return info, nil
}

// balanceInt 把字符串浮点余额四舍五入成整数（`-7.58` ⇒ `-8`）。
func balanceInt(raw string) (int64, error) {
	f, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("%w（balance=%q）", errCreditsUnreadable, raw)
	}
	return int64(math.Round(f)), nil
}

// typedError 让自愈链能 `errors.As` 取到分类（R79）：额度路径的 401/3xx 必须能被识别成
// `ErrSessionDead`，否则面板永远显示旧的 0 而看不到「凭据失效」。
func typedError(status int, msg string) error {
	kind, _ := classifyKind(status, msg)
	return &provider.Error{
		Kind:   kind,
		Status: status,
		Msg:    msg,
	}
}

// FetchModelPricing 本渠道无独立费率端点。
//
// `theosPerRequest` 是「每次请求的单价」（类似 lingxi 的倍率），不是按 token 计价的
// 费率表，塞进费率列只会误导；故返回空（面板费率页对该渠道不展示模型价格）。
func (c *Client) FetchModelPricing(a *auth.Auth) ([]provider.ModelPricing, error) {
	return nil, nil
}

// DailyCheckin 本渠道**没有独立签到任务**：签到是 `/theos` 探测的副作用
// （上一次下发的 nonce 在下一次探测时回传即算签到，见 checkin.go 的 nonce 环）。
//
// 因此这里返回 nil 而不是 error：调度器在 `CheckinMinutes` 为空时本就不会调用它；
// 万一手工触发，也应当是「什么都不做且不写签到记录」（拓扑见 R150 的闸门）。
func (c *Client) DailyCheckin(a *auth.Auth) error { return nil }
