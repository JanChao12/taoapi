// identity.go：查询当前 token 对应的账号身份。
//
// ═══════════════════════════════════════════════════════════════════
// 为什么需要它
// ═══════════════════════════════════════════════════════════════════
//
// 网页登录（见 internal/login）拿到的凭据响应**只有两个 token**：
//
//	{"accessToken":"…","refreshToken":"…"}
//
// **没有 uid**。而本项目的 auth.Account 以 uid 为主键，
// 且 provider 层要用它派生 X-User-Id / MachineID / SessionID
// （见 client.go 的 Credential.MachineID）。
//
// 所以"网页登录"这条路径必须能**用 token 反查 uid**，否则拿到的凭据
// 根本没法落盘成一条合法账号。
//
// ═══════════════════════════════════════════════════════════════════
// 实测依据（2026-10-05，真实账号）
// ═══════════════════════════════════════════════════════════════════
//
//	GET /v2/plugin/login/account
//	Authorization: Bearer <accessToken>
//	→ HTTP 200
//	{"code":0,"msg":"OK","data":{
//	   "uid":"<uuid>",
//	   "nickname":"<phone>",
//	   "phoneNumber":"<phone>",
//	   "uin":"<uin>",
//	   "type":"personal", ...}}
//
// ⚠️ **字段名与形状是实测的；上面几个取值已脱敏**（脱敏前是真实账号的
// 手机号/UID/UIN）。要看真实样本请自行用凭据调用该端点，
// **不要把这里的占位值当成上游真实返回**。
//
// ⚠️ 该端点**不带 state** 时返回"当前 token 是谁"；
//
//	带 state 时是"认领待完成的登录会话"（另一用途，见第 27 轮调研）。
//	这里只用前者。
//
// ⚠️ 这是**实测的站点接口**，不是标准 OIDC userinfo，可能随上游变更。
//
// ═══════════════════════════════════════════════════════════════════
// 与"验证凭据可用性"的关系
// ═══════════════════════════════════════════════════════════════════
//
// Codex 第 28 轮强调「捕获凭据 ≠ 账号验证完成」。本方法**本身**就构成
// 一次有效性验证：能换出 uid 就说明这个 token 被上游认可。
// 但调用方仍应再查一次额度（Credit）以确认"能用"，两条一起才够。
package workbuddy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// identityMaxResponseBytes 身份响应上限。
// 实测响应约 700 字节；给 64 KiB 足够且能防异常对端。
const identityMaxResponseBytes = 64 << 10

// Identity 是账号身份信息（只含非秘密字段）。
type Identity struct {
	// UID 账号唯一标识 —— 本项目账号结构的主键。
	UID string

	// Nickname 显示名（通常是手机号）。
	Nickname string

	// UIN 腾讯账号体系里的数字 ID（面板展示用，非必需）。
	UIN string

	// PhoneNumber 绑定手机号（面板展示用）。
	PhoneNumber string

	// AccountType 账号类型（如 "personal"）。
	AccountType string
}

// identityResp 是 /v2/plugin/login/account 的响应结构。
type identityResp struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		UID         string `json:"uid"`
		Nickname    string `json:"nickname"`
		UIN         string `json:"uin"`
		PhoneNumber string `json:"phoneNumber"`
		Type        string `json:"type"`
	} `json:"data"`
}

// Identity 用当前凭据查询账号身份。
//
// 用途：网页登录后把"只有 token"的凭据补全成"可落盘的账号"。
//
// 返回的错误**不含 token**。若 token 无效，上游返回非 200 或 code != 0，
// 这里转成 *UpstreamError，调用方据此拒绝落盘。
func (p *Provider) Identity(ctx context.Context) (Identity, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		p.client.LoginAccountURL(), nil)
	if err != nil {
		return Identity{}, fmt.Errorf("构造身份请求失败: %w", err)
	}
	// 该端点用 Bearer 鉴权（实测），与额度端点的头不同，
	// 所以这里单独设置而不是复用 applyBillingHeaders。
	req.Header.Set("Authorization", "Bearer "+p.cred.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Language", acceptLanguage)
	req.Header.Set("User-Agent", clientUA())
	req.Header.Set("Origin", originCN)

	resp, err := p.client.http.Do(req)
	if err != nil {
		return Identity{}, fmt.Errorf("请求身份失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, identityMaxResponseBytes))
	if err != nil {
		return Identity{}, fmt.Errorf("读取身份响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return Identity{}, &UpstreamError{
			StatusCode: resp.StatusCode, Body: snippet(body), Op: "identity",
		}
	}

	var payload identityResp
	if err := json.Unmarshal(body, &payload); err != nil {
		return Identity{}, fmt.Errorf("解析身份响应失败: %w", err)
	}
	if payload.Code != 0 {
		return Identity{}, &UpstreamError{
			StatusCode: resp.StatusCode, BizCode: payload.Code, Op: "identity",
			Body: snippet(body),
		}
	}

	uid := strings.TrimSpace(payload.Data.UID)
	if uid == "" {
		// code=0 但没 uid —— 契约变了，明确报错而不是落一个空主键的账号。
		return Identity{}, fmt.Errorf("身份响应缺少 uid（上游契约可能已变更）")
	}

	return Identity{
		UID:         uid,
		Nickname:    strings.TrimSpace(payload.Data.Nickname),
		UIN:         strings.TrimSpace(payload.Data.UIN),
		PhoneNumber: strings.TrimSpace(payload.Data.PhoneNumber),
		AccountType: strings.TrimSpace(payload.Data.Type),
	}, nil
}
