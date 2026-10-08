package workbuddy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ═══════════════════════════════════════════════════════════════════════
// Token 续期（凭证保活）
// ═══════════════════════════════════════════════════════════════════════
//
// 实测契约（2026-10-05 国内版首次实测，见 docs/upstream-contract.md §三）：
//
//	POST {baseChat}/v2/plugin/auth/token/refresh
//	  Header: X-Refresh-Token: <refreshToken>
//	  → 200 {"code":0,"msg":"OK","requestId":"…",
//	         "data":{"accessToken":…,"refreshToken":…,
//	                 "expiresIn":…,"refreshExpiresIn":…,…}}
//
// 🔴 安全红线（本项目三条红线之一）：
//
//	refresh token **只允许**出现在本端点（X-Refresh-Token 头）。
//	绝不能进入 chat 请求 —— 由 TestChatHeadersNeverCarryRefreshToken 守着。
//	本文件是**唯一**允许设置该头的地方。
//
// ⚠️ 必须如实记住的两条限制（Codex 第 33 轮给定措辞，不得夸大）：
//
//  1. **「到期后的续期」从未验证**。已实测的两次续期都是 access token
//     **未过期**时调的，服务端返回的还是**同一个** token（未轮换、
//     有效期未延长）。所以本功能能让账号**不必重复登录**，
//     但不能承诺"永不过期"。
//  2. 样本极小（一个账号、两次请求、同一天）。**跨天 / 到期 /
//     闲置超时 / 服务端撤销 均未验证。**
//
// ⚠️ 实现不得依赖"refresh token 不轮换"：
//
//	那只是本次两次请求的观察。**收到新值就必须原子替换**，
//	否则将来上游开始轮换时会立刻失效（refresh token 一次性）。

// RefreshResult 是一次续期的结果。
//
// 🔴 本结构含 token，【绝不】序列化到日志/事件/API 响应。
type RefreshResult struct {
	// AccessToken 新的 access token。
	AccessToken string

	// RefreshToken 服务端返回的 refresh token。
	//
	// ⚠️ 可能等于旧的（实测未轮换），**也可能不同**（将来轮换）。
	// 调用方必须**无条件**用返回值覆盖存储 —— 只在"变化时"覆盖
	// 会在轮换场景下丢凭据。
	RefreshToken string

	// ExpiresIn access token 有效期（秒）；0 表示上游未给。
	ExpiresIn int64

	// RefreshExpiresIn refresh token 有效期（秒）；0 表示上游未给。
	RefreshExpiresIn int64

	// Rotated 报告 refresh token 是否**发生了变化**（仅用于展示/日志）。
	//
	// ⚠️ 不要用它决定"是否保存" —— 见 RefreshToken 的说明。
	Rotated bool
}

// refreshResponse 是续期端点的响应信封（实测形态，与登录端点同构）。
type refreshResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		AccessToken      string `json:"accessToken"`
		RefreshToken     string `json:"refreshToken"`
		ExpiresIn        int64  `json:"expiresIn"`
		RefreshExpiresIn int64  `json:"refreshExpiresIn"`
	} `json:"data"`
}

// refreshMaxResponseBytes 续期响应体上限。
//
// 凭据响应实测约 2-4 KB；给 64 KB 足够，同时防上游异常时读爆内存。
const refreshMaxResponseBytes = 64 << 10

// RefreshToken 用 refresh token 换一份新的凭据。
//
// 返回 error 时**不得**改动任何已存凭据（调用方负责）。
//
// 🔴 只发一个请求，不做"多形态重试"：
//
//	实测第一种形态就命中。继续试其他形态等于**反复提交真实 refresh token**，
//	而 Codex 第 28 轮警告过：同一个 refresh token 反复打可能触发
//	**整个 token family 被撤销** —— 那会让用户彻底无法登录。
func (c *Client) RefreshToken(ctx context.Context, cred Credential) (RefreshResult, error) {
	var out RefreshResult

	if strings.TrimSpace(cred.RefreshToken) == "" {
		return out, fmt.Errorf("续期失败：该账号没有 refresh token（需重新登录）")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.TokenRefreshURL(), nil)
	if err != nil {
		return out, fmt.Errorf("构造续期请求失败: %w", err)
	}

	// 🔴 这是全项目唯一设置该头的地方（安全红线）。
	req.Header.Set("X-Refresh-Token", cred.RefreshToken)
	// 复用公共头，保持与官方客户端一致的指纹。
	commonHeaders(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return out, fmt.Errorf("请求续期端点失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, refreshMaxResponseBytes))
	if err != nil {
		return out, fmt.Errorf("读取续期响应失败: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return out, &UpstreamError{
			StatusCode: resp.StatusCode,
			Body:       snippet(body),
			Op:         "token-refresh",
		}
	}

	var payload refreshResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return out, fmt.Errorf("解析续期响应失败: %w (前 200 字节: %s)", err, snippet(body))
	}
	if payload.Code != 0 {
		return out, &UpstreamError{
			StatusCode: resp.StatusCode,
			BizCode:    payload.Code,
			BizMsg:     payload.Msg,
			Op:         "token-refresh",
		}
	}

	// 缺 access token ⇒ 视为失败，绝不写入半个凭据。
	//
	// refresh token 缺失则**沿用旧的**：实测服务端总会返回，
	// 但若某个版本不返回，把凭据清空会让账号立刻不可用 —— 保守处理。
	newAccess := strings.TrimSpace(payload.Data.AccessToken)
	if newAccess == "" {
		return out, fmt.Errorf("续期响应缺少 accessToken（不覆盖已有凭据）")
	}
	newRefresh := strings.TrimSpace(payload.Data.RefreshToken)

	out.AccessToken = newAccess
	out.ExpiresIn = payload.Data.ExpiresIn
	out.RefreshExpiresIn = payload.Data.RefreshExpiresIn
	if newRefresh == "" {
		out.RefreshToken = cred.RefreshToken
	} else {
		out.RefreshToken = newRefresh
		out.Rotated = newRefresh != cred.RefreshToken
	}
	return out, nil
}

// AccessTokenExpiry 解码 access token 的 exp（Unix 秒）；无 exp 返回 0。
//
// 🔴 只解码、**不验签** —— 本地解出的 exp 不是安全判据，
// 只用于"什么时候该去续期"这种调度决策（Codex F10 的要求）。
func AccessTokenExpiry(token string) int64 {
	claims := jwtPayload(token)
	if claims == nil {
		return 0
	}
	if v, ok := claims["exp"].(float64); ok {
		return int64(v)
	}
	return 0
}

// NeedsRefresh 报告该凭据是否已到"该续期"的时候。
//
// 判据（两条任一成立即需要）：
//   - access token 的 exp 已进入 horizon 窗口内（或已过期）
//   - **解不出 exp**（未知）且 `age` 显示距上次续期已超过 horizon
//
// ⚠️ 第二条是为了兜住"token 不是 JWT / 格式变了"的情况：
// 此时 exp=0 永远不满足第一条，账号会**静默地永不续期**。
//
// 但"未知"也不能无脑续（每次调用都发请求 = 打爆端点），所以用
// lastRefresh 做节流：未知 exp 时**每个 horizon 只试一次**。
func NeedsRefresh(accessToken string, lastRefresh time.Time, now time.Time, horizon time.Duration) bool {
	exp := AccessTokenExpiry(accessToken)
	if exp > 0 {
		// 有明确 exp：进入窗口就续。
		return !now.Add(horizon).Before(time.Unix(exp, 0))
	}
	// 无 exp：按"距上次尝试的时间"节流。
	if lastRefresh.IsZero() {
		return true
	}
	return now.Sub(lastRefresh) >= horizon
}

// jwtPayload 解出 JWT 的 payload（第二段）。失败返回 nil。
//
// ⚠️ 与 refreshprobe 的实现同口径：允许无 padding 的 base64url
// 与带 padding 的标准编码两种。解不出来返回 nil，绝不 panic。
func jwtPayload(tok string) map[string]any {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return nil
	}
	raw, err := base64URLDecode(parts[1])
	if err != nil {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	return m
}

// ToCredential 把续期结果套回原凭据，得到一份可直接使用的新凭据。
//
// 保留 UID / EnterpriseID / Domain —— 续期不改变账号身份，只换 token。
func (r RefreshResult) ToCredential(old Credential) Credential {
	old.AccessToken = r.AccessToken
	old.RefreshToken = r.RefreshToken
	return old
}

// base64URLDecode 解 base64url（JWT 段）。
//
// 先按无 padding（JWT 标准）解，失败再按带 padding 解 ——
// 有些实现会带 padding，两种都试能少一类"解不出来"的假故障。
func base64URLDecode(s string) ([]byte, error) {
	if raw, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return raw, nil
	}
	return base64.URLEncoding.DecodeString(s)
}
