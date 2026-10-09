package workbuddy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"workbuddy.local/workbuddy-api/internal/provider"
)

// billingMaxResponseBytes 额度响应读取上限。
const billingMaxResponseBytes = 2 << 20

// wallClock 上游时间串的口径：固定 UTC+8。
//
// ⚠️ 必须固定，不能用 time.Local —— 上游下发的 CycleEndTime 是北京时间墙钟，
// 在非 UTC+8 机器上解析会把到期日算错一天（wild-work 源码明确指出此坑）。
var wallClock = time.FixedZone("UTC+8", 8*60*60)

// resourceAccount 是额度响应里的单个套餐。
//
// 字段名直连实测；只用我们真正需要的。
type resourceAccount struct {
	PackageName         string `json:"PackageName"`
	CapacitySize        int64  `json:"CapacitySize"`
	CapacityRemain      int64  `json:"CapacityRemain"`
	CapacityUsed        int64  `json:"CapacityUsed"`
	CycleCapacitySize   int64  `json:"CycleCapacitySize"`
	CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
	CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`

	// CycleEndTime 周期结束时间（"2006-01-02 15:04:05"，UTC+8 墙钟）。
	//
	// ⭐ 这是【积分到期判据】—— 实测确认：
	//   上游不提供 PackageEndTime，DeductionEndTime 是账单扣减窗口（可能是 2035 年）
	//   而非积分有效期。多个套餐的 CycleEndTime 各不相同，正是"哪个先过期"的依据。
	CycleEndTime string `json:"CycleEndTime"`
}

// bill 按周期口径返回 (总额, 已用, 剩余)。
// 周期字段存在时优先用它；剩余负值钳 0。
func (r resourceAccount) bill() (tot, used, remain int64) {
	switch {
	case r.CycleCapacitySize > 0:
		tot, used, remain = r.CycleCapacitySize, r.CycleCapacityUsed, r.CycleCapacityRemain
	case r.CycleCapacityRemain > 0 || r.CycleCapacityUsed > 0:
		tot, used, remain = r.CycleCapacityRemain+r.CycleCapacityUsed, r.CycleCapacityUsed, r.CycleCapacityRemain
	default:
		tot, used, remain = r.CapacitySize, r.CapacityUsed, r.CapacityRemain
	}
	if remain < 0 {
		remain = 0
	}
	return
}

// expireDate 把 CycleEndTime 转为 YYYY-MM-DD；缺失或不可解析时返回空串。
//
// 返回空串表示"上游未下发到期时间"，调用方【不得】用零值时间冒充"永不过期"。
func (r resourceAccount) expireDate() string {
	ts := strings.TrimSpace(r.CycleEndTime)
	if ts == "" {
		return ""
	}
	if t, err := time.ParseInLocation("2006-01-02 15:04:05", ts, wallClock); err == nil {
		return t.Format("2006-01-02")
	}
	return ""
}

// userResourceResp 是额度响应信封。
type userResourceResp struct {
	Code int `json:"code"`
	Data struct {
		Response struct {
			Data struct {
				TotalDosage int64             `json:"TotalDosage"`
				Accounts    []resourceAccount `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	} `json:"Data"`
}

// billingBody 构造额度查询请求体。
//
// ⚠️ 必须是 POST 且带 body，否则上游返回 404（实测）。
func billingBody(now time.Time) []byte {
	b, _ := json.Marshal(map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.AddDate(101, 0, 0).Format("2006-01-02 15:04:05"),
	})
	return b
}

// Credit 实现 provider.Provider：查询额度。
func (p *Provider) Credit(ctx context.Context, _ string) (provider.CreditResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.client.UserResourceURL(), bytes.NewReader(billingBody(time.Now())))
	if err != nil {
		return provider.CreditResult{}, fmt.Errorf("构造额度请求失败: %w", err)
	}
	applyBillingHeaders(req, p.cred)

	resp, err := p.client.http.Do(req)
	if err != nil {
		return provider.CreditResult{}, fmt.Errorf("请求额度失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, billingMaxResponseBytes))
	if err != nil {
		return provider.CreditResult{}, fmt.Errorf("读取额度响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return provider.CreditResult{}, &UpstreamError{
			StatusCode: resp.StatusCode, Body: snippet(body), Op: "credit",
		}
	}

	var payload userResourceResp
	if err := json.Unmarshal(body, &payload); err != nil {
		return provider.CreditResult{}, fmt.Errorf("解析额度失败: %w", err)
	}
	if payload.Code != 0 {
		return provider.CreditResult{}, &UpstreamError{
			StatusCode: resp.StatusCode, BizCode: payload.Code, Op: "credit",
			Body: snippet(body),
		}
	}

	accts := payload.Data.Response.Data.Accounts
	out := provider.CreditResult{
		Accounts: make([]provider.CreditAccount, 0, len(accts)),
	}
	var total int64
	for _, a := range accts {
		// 🔴 用 bill() 返回的**同一个** tot，不要自己再取 a.CycleCapacitySize
		//    （2026-10-09 修）。
		//
		//	bill() 有三种口径（周期字段优先 → 半周期 → 非周期），
		//	而 a.CycleCapacitySize 只是**其中一种**来源。原实现
		//	`_, used, remain := a.bill()` 把 tot 丢掉、再写死
		//	`Size: a.CycleCapacitySize` —— 当上游走的是**非周期**
		//	分支（CapacitySize）时，Size 会是 0 而 remain 正常
		//	⇒ 面板拿不到总量，百分比条（remain/Size）全部失真。
		//
		//	现在 tot 与 used/remain **同源**，不可能分叉。
		tot, used, remain := a.bill()
		total += remain
		out.Accounts = append(out.Accounts, provider.CreditAccount{
			PackageName: a.PackageName,
			Remain:      remain,
			Used:        used,
			Size:        tot,
			ExpireAt:    a.expireDate(),
		})
	}
	if len(accts) > 0 {
		t := total
		out.Remaining = &t
	}

	// 按到期日升序，便于"谁先过期"一眼可见
	sort.SliceStable(out.Accounts, func(i, j int) bool {
		a, b := out.Accounts[i].ExpireAt, out.Accounts[j].ExpireAt
		if a == "" {
			return false // 无到期日的排最后
		}
		if b == "" {
			return true
		}
		return a < b
	})
	return out, nil
}

// ─────────────────────────────────────────────────────────────
// 签到
// ─────────────────────────────────────────────────────────────

// Checkin 实现 provider.Provider：执行每日签到。
//
// ⚠️ 关键实测（两种"已签到"形态都真实出现过）：
//   - HTTP 400 + 空 body（2026-10-04 观测）
//   - HTTP 400 + JSON body {code:10001, msg:"今天已签到，请明天再来"}（2026-10-05 步骤⑦观测）
//
// 两者都是幂等成功，绝不能计入失败/冷却。
func (p *Provider) Checkin(ctx context.Context, _ string) (provider.CheckinResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.client.DailyCheckinURL(), bytes.NewReader([]byte("{}")))
	if err != nil {
		return provider.CheckinResult{}, fmt.Errorf("构造签到请求失败: %w", err)
	}
	applyBillingHeaders(req, p.cred)

	resp, err := p.client.http.Do(req)
	if err != nil {
		return provider.CheckinResult{}, fmt.Errorf("请求签到失败: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))

	if resp.StatusCode == http.StatusOK {
		var env struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
		}
		_ = json.Unmarshal(body, &env)
		if env.Code == 0 {
			return provider.CheckinResult{Message: "ok"}, nil
		}
		// 业务码非 0：按 msg 判断是否"已签到"
		if isAlreadyCheckedIn(env.Msg) {
			return provider.CheckinResult{AlreadyCheckedIn: true, Message: env.Msg}, nil
		}
		return provider.CheckinResult{}, &UpstreamError{
			StatusCode: resp.StatusCode, BizCode: env.Code, BizMsg: env.Msg, Op: "checkin",
		}
	}

	// 非 2xx：先看 body 是否为"已签到"形态（JSON code=10001 / 关键词），
	// 再判"空 body"形态 —— 顺序不能反，否则带 body 的幂等场景会被误判失败。
	if resp.StatusCode == http.StatusBadRequest {
		var env struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
		}
		if err := json.Unmarshal(bytes.TrimSpace(body), &env); err == nil && env.Code != 0 {
			if isAlreadyCheckedIn(env.Msg) || env.Code == checkinAlreadyCode {
				return provider.CheckinResult{AlreadyCheckedIn: true, Message: env.Msg}, nil
			}
		}
		if len(bytes.TrimSpace(body)) == 0 {
			return provider.CheckinResult{AlreadyCheckedIn: true, Message: "今日已签到"}, nil
		}
	}

	return provider.CheckinResult{}, &UpstreamError{
		StatusCode: resp.StatusCode, Body: snippet(body), Op: "checkin",
	}
}

// checkinAlreadyCode 上游"今日已签到"的业务码（2026-10-05 步骤⑦实测）。
//
// 与 msg 关键词判定互为补充：上游改文案时码不变，改码时文案在。
const checkinAlreadyCode = 10001

// isAlreadyCheckedIn 判断业务消息是否表示"已签到"。
func isAlreadyCheckedIn(msg string) bool {
	m := strings.ToLower(msg)
	for _, kw := range []string{"已签到", "重复签到", "already", "duplicate"} {
		if strings.Contains(m, kw) {
			return true
		}
	}
	return false
}
