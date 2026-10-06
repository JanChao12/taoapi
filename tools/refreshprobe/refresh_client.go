// refresh_client.go：续期端点的调用与凭据验证。
//
// ═══════════════════════════════════════════════════════════════════
// 🔴 本文件是全项目**第一次**真正调用续期端点
// ═══════════════════════════════════════════════════════════════════
//
// 接手时的实际状况（已核实，不是推测）：
//
//   - `upstream.PathTokenRefresh` 存在，`Client.TokenRefreshURL()` 存在，
//     但**没有任何生产代码调用它**（唯一的引用是一个断言 URL 字符串的测试）
//   - 因此续期端点的**方法 / 请求头 / 请求体 / 响应结构从未实测过**
//   - `accounts.json` 里存着 refresh token 却从不用它续期；
//     `failover.go` 的注释写着「标 auth_expired 以便尝试 refresh」，
//     而那条 refresh 路径**并不存在**
//
// 所以本文件的第一个任务是**把契约测出来**，而不是假设它。
// 探测顺序刻意从最可能的形态开始，并且**每种形态只试一次**，
// 避免把同一个 refresh token 反复打到端点（Codex 警告：可能触发
// 整个 token family 撤销）。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	"workbuddy.local/workbuddy-api/internal/provider/workbuddy"
	"workbuddy.local/workbuddy-api/internal/upstream"
)

// refreshMaxBytes 续期响应读取上限（防异常对端撑爆内存）。
const refreshMaxBytes = 256 << 10

// refreshResult 是一次续期尝试的**脱敏**结果。
//
// 🔴 绝不包含 token 原文；NewAccess/NewRefresh 只在内存中流转，
// 由调用方立即用于下一次请求或加密落盘。
type refreshResult struct {
	StatusCode int
	BizCode    *int64
	FieldShape string // 响应字段名与长度，用于判断契约

	NewAccess  string
	NewRefresh string

	HasAccess      bool
	HasRefresh     bool
	AccessChanged  bool
	RefreshChanged bool

	// Attempt 记录是第几种形态成功的，便于写进实验报告。
	Attempt string
}

// refreshAttempt 描述一种待试的请求形态。
type refreshAttempt struct {
	Name   string
	Method string
	Path   string
	Header func(*http.Request, string)
	Body   func(string) []byte
}

// refreshToken 调用续期端点。
//
// ⚠️ 设计纪律（Codex 第 28 轮警告）：
//
//	「不要伪造 token，也不要随意重放旧 refresh token 探测失效，
//	  否则可能触发整个 token family 撤销。」
//
// 所以这里**只试有限几种形态**，且**一旦拿到 access token 就立刻停**。
// 不对同一个 token 反复打压测式请求。
func refreshToken(refresh string) (refreshResult, error) {
	var last refreshResult
	var lastErr error

	for _, a := range refreshAttempts() {
		res, err := doRefreshAttempt(a, refresh)
		res.Attempt = a.Name
		if err == nil && res.HasAccess {
			return res, nil
		}
		last, lastErr = res, err

		// 若端点明确以"业务码"拒绝了（而不是 404/405 这类"形态不对"），
		// 说明我们找对了端点但请求被业务拒绝 —— 继续换形态没有意义，
		// 而且多打一次就多一次风险。直接返回。
		if res.BizCode != nil {
			return res, nil
		}
	}

	return last, lastErr
}

// refreshAttempts 返回按可能性排序的请求形态。
//
// 排序依据（推断，不是实测）：
//  1. 官方 CLI 的刷新走 copilot.tencent.com，且项目里既有代码把
//     refresh token 放在 `X-Refresh-Token` 头（安全红线文档专门写它）——
//     说明历史上见过这个头，是最可能的形态
//  2. POST + JSON body {"refreshToken": ...} 是 Keycloak 风格
//  3. GET + Authorization: Bearer 是最保守的兜底
//
// ⚠️ 这些是**待验证的假设**，实验结果会修正它们。
func refreshAttempts() []refreshAttempt {
	p := upstream.PathTokenRefresh

	return []refreshAttempt{
		{
			Name:   "POST + X-Refresh-Token 头（无 body）",
			Method: http.MethodPost,
			Path:   p,
			Header: func(r *http.Request, rt string) {
				r.Header.Set("X-Refresh-Token", rt)
			},
			Body: func(string) []byte { return []byte("{}") },
		},
		{
			Name:   "POST + JSON body {refreshToken}",
			Method: http.MethodPost,
			Path:   p,
			Header: func(r *http.Request, _ string) {},
			Body: func(rt string) []byte {
				b, _ := json.Marshal(map[string]string{"refreshToken": rt})
				return b
			},
		},
		{
			Name:   "POST + Authorization: Bearer <refresh>",
			Method: http.MethodPost,
			Path:   p,
			Header: func(r *http.Request, rt string) {
				r.Header.Set("Authorization", "Bearer "+rt)
			},
			Body: func(string) []byte { return []byte("{}") },
		},
		{
			Name:   "GET + Authorization: Bearer <refresh>",
			Method: http.MethodGet,
			Path:   p,
			Header: func(r *http.Request, rt string) {
				r.Header.Set("Authorization", "Bearer "+rt)
			},
			Body: func(string) []byte { return nil },
		},
	}
}

// doRefreshAttempt 执行单次续期尝试。
func doRefreshAttempt(a refreshAttempt, refresh string) (refreshResult, error) {
	var out refreshResult

	var bodyReader io.Reader
	if b := a.Body(refresh); b != nil {
		bodyReader = bytes.NewReader(b)
	}

	req, err := http.NewRequest(a.Method, baseFor(a)+a.Path, bodyReader)
	if err != nil {
		return out, fmt.Errorf("构造续期请求失败: %w", err)
	}

	// 用**与正式客户端一致**的公共头，避免因缺头被网关拒绝而误判契约。
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "CLI/2.63.2 CodeBuddy/2.63.2")
	req.Header.Set("Accept-Language", "zh-CN")
	req.Header.Set("Origin", "https://www.codebuddy.cn")
	req.Header.Set("Referer", "https://www.codebuddy.cn/")
	a.Header(req, refresh)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return out, fmt.Errorf("请求续期端点失败: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, refreshMaxBytes))
	if err != nil {
		return out, fmt.Errorf("读取续期响应失败: %w", err)
	}
	out.StatusCode = resp.StatusCode
	out.FieldShape = describeFields(raw)

	if resp.StatusCode != http.StatusOK {
		// 🔴 不返回 body —— 可能含 token 或其它敏感内容。
		return out, fmt.Errorf("续期端点返回 HTTP %d（%s）", resp.StatusCode, summarizeErrorBody(raw))
	}

	// 解析：先按 code/data 信封，再退回顶层字段。
	var env struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err == nil && env.Code != 0 {
		c := int64(env.Code)
		out.BizCode = &c
		return out, nil
	}

	// 找 access/refresh：可能直接在顶层，也可能在 data 里。
	for _, candidate := range candidatePayloads(raw, env.Data) {
		var p struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
		}
		if err := json.Unmarshal(candidate, &p); err != nil {
			continue
		}
		if p.AccessToken != "" && p.RefreshToken != "" {
			out.NewAccess = p.AccessToken
			out.NewRefresh = p.RefreshToken
			out.HasAccess = true
			out.HasRefresh = true
			return out, nil
		}
	}

	return out, fmt.Errorf("续期响应里没有找到 accessToken+refreshToken（%s）", out.FieldShape)
}

// baseFor 决定续期用哪个基址。
//
// PathTokenRefresh 是 `/v2/plugin/auth/token/refresh`，与 chat 同基址
// （见 client.go 的 TokenRefreshURL 用的是 baseChat）。
func baseFor(_ refreshAttempt) string { return upstream.ChatBase }

// candidatePayloads 返回可能承载 token 的 JSON 片段。
func candidatePayloads(raw, data json.RawMessage) []json.RawMessage {
	out := []json.RawMessage{raw}
	if len(data) > 0 && string(data) != "null" {
		out = append(out, data)
	}
	return out
}

// describeFields 返回响应里的**字段名与长度**（不含值）。
//
// 用于判断契约：例如"响应里有没有 accessToken 这个键"。
// 🔴 只输出名字与长度 —— 绝不输出值。
func describeFields(raw []byte) string {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Sprintf("(非 JSON，%d 字节)", len(raw))
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s(%s)", k, shapeOf(m[k])))
	}
	return "顶层: " + joinComma(parts)
}

// shapeOf 描述一个值的形态（类型 + 长度），**不含内容**。
func shapeOf(v any) string {
	switch t := v.(type) {
	case string:
		return fmt.Sprintf("string,%d", len(t))
	case float64:
		return "number"
	case bool:
		return "bool"
	case nil:
		return "null"
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return fmt.Sprintf("object{%s}", joinComma(keys))
	case []any:
		return fmt.Sprintf("array,%d", len(t))
	default:
		return "?"
	}
}

// summarizeErrorBody 把非 2xx 的响应体压成**极短**的说明，供诊断。
//
// 🔴 只保留业务码/消息这类结构性信息，绝不放原始报文。
func summarizeErrorBody(raw []byte) string {
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(raw, &env); err == nil && (env.Code != 0 || env.Msg != "") {
		msg := env.Msg
		if len(msg) > 80 {
			msg = msg[:80] + "…"
		}
		return fmt.Sprintf("code=%d msg=%s", env.Code, msg)
	}
	if len(raw) == 0 {
		return "空响应体"
	}
	return fmt.Sprintf("%d 字节非 JSON 响应", len(raw))
}

// joinComma 用 ", " 连接（避免引入 strings 的重复用法）。
func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}

// ─────────────────────────────────────────────────────────────
// 凭据验证（Identity + Credit）
// ─────────────────────────────────────────────────────────────

// verify 用给定凭据跑 Identity + Credit 两步。
//
// Codex 第 28 轮：「捕获成功 ≠ 账号验证完成。」两步都过才算真正可用。
// 返回的 uid/nickname 非秘密；credits 为 nil 表示账号可用但当前无额度包。
func verify(access, refresh string) (string, string, *int64, error) {
	client := workbuddy.NewClient()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 先用占位 uid 换身份（网页登录的凭据没有 uid）。
	tmp := workbuddy.NewProvider(client, workbuddy.Credential{
		AccessToken:  access,
		RefreshToken: refresh,
	})

	id, err := tmp.Identity(ctx)
	if err != nil {
		return "", "", nil, fmt.Errorf("凭据无效或已过期: %w", err)
	}

	// 用真实 uid 重建（部分上游头依赖 uid 派生）。
	p := workbuddy.NewProvider(client, workbuddy.Credential{
		AccessToken:  access,
		RefreshToken: refresh,
		UID:          id.UID,
	})

	res, err := p.Credit(ctx, "")
	if err != nil {
		return "", "", nil, fmt.Errorf("额度查询失败: %w", err)
	}
	if res.Remaining == nil {
		return id.UID, id.Nickname, nil, nil
	}
	v := *res.Remaining
	return id.UID, id.Nickname, &v, nil
}
