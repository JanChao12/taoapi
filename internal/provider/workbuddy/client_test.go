package workbuddy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"workbuddy.local/workbuddy-api/internal/testutil"
)

// testCred 是测试用凭证（不是真实 token）。
func testCred() Credential {
	return Credential{
		AccessToken:  "test-access-token-not-real",
		UID:          "eeeeeeee-1111-2222-3333-444444444444",
		RefreshToken: "test-refresh-token-must-never-be-sent-to-chat",
	}
}

// TestChatHeadersNeverCarryRefreshToken 是【安全红线】测试。
//
// chat 请求绝不能携带 X-Refresh-Token。这条不能靠代码审查记忆，
// 必须由测试强制。
func TestChatHeadersNeverCarryRefreshToken(t *testing.T) {
	cred := testCred()
	req, err := http.NewRequest(http.MethodPost, "https://example.invalid/v2/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	applyChatHeaders(req, cred, newMessageID())

	testutil.AssertNoRefreshTokenHeader(t, req.Header)

	// 双重保险：确保测试本身有效——refresh token 字段确实有值，
	// 否则"没发送"这件事没有意义。
	if cred.RefreshToken == "" {
		t.Fatal("测试无效：cred.RefreshToken 为空，无法证明它没被发送")
	}
}

// TestChatHeadersRequired 验证必需身份头齐全。
func TestChatHeadersRequired(t *testing.T) {
	cred := testCred()
	req, _ := http.NewRequest(http.MethodPost, "https://example.invalid/x", nil)
	applyChatHeaders(req, cred, newMessageID())

	checks := map[string]string{
		"Authorization":       "Bearer " + cred.AccessToken,
		"X-User-Id":           cred.UID,
		"X-IDE-Name":          "WorkBuddy",
		"X-IDE-Type":          "WorkBuddy",
		"X-Product":           "WorkBuddy",
		"X-Agent-Purpose":     "conversation",
		"X-CodeBuddy-Request": "1",
		"X-B3-Sampled":        "1",
		"Origin":              "https://www.codebuddy.cn",
		"Accept-Language":     "zh-CN",
	}
	for k, want := range checks {
		if got := req.Header.Get(k); got != want {
			t.Errorf("头 %s = %q，期望 %q", k, got, want)
		}
	}

	// User-Agent 必须伪装成官方 CLI
	if ua := req.Header.Get("User-Agent"); !strings.Contains(ua, "CodeBuddy") {
		t.Errorf("User-Agent = %q，应包含 CodeBuddy 标识", ua)
	}
}

// TestChatHeadersMissingFieldsUseNoConvention 验证缺省字段用 X-No-* 约定。
func TestChatHeadersMissingFieldsUseNoConvention(t *testing.T) {
	cred := Credential{AccessToken: "t", UID: "u"} // 无 enterprise/domain
	req, _ := http.NewRequest(http.MethodPost, "https://example.invalid/x", nil)
	applyChatHeaders(req, cred, newMessageID())

	if got := req.Header.Get("X-No-Enterprise-Id"); got != "1" {
		t.Errorf("无 EnterpriseID 时应发 X-No-Enterprise-Id: 1，实际 %q", got)
	}
	if got := req.Header.Get("X-No-Department-Info"); got != "1" {
		t.Errorf("无 Domain 时应发 X-No-Department-Info: 1，实际 %q", got)
	}
	if got := req.Header.Get("X-Enterprise-Id"); got != "" {
		t.Errorf("无 EnterpriseID 时不应发 X-Enterprise-Id，实际 %q", got)
	}
}

// TestDeriveStableID 验证机器/会话 ID 派生规则（实测还原）。
func TestDeriveStableID(t *testing.T) {
	cred := testCred()

	m := cred.MachineID()
	s := cred.SessionID()

	// 实测规则产出 36 个 hex 字符
	if len(m) != 36 {
		t.Errorf("MachineID 长度 = %d，期望 36", len(m))
	}
	if len(s) != 36 {
		t.Errorf("SessionID 长度 = %d，期望 36", len(s))
	}
	// 必须稳定（同 uid 同结果）
	if cred.MachineID() != m {
		t.Error("MachineID 不稳定：同一凭证两次调用结果不同")
	}
	// 机器与会话必须不同
	if m == s {
		t.Error("MachineID 与 SessionID 不应相同")
	}
	// 换 uid 必须换值
	other := Credential{UID: "another-uid"}
	if other.MachineID() == m {
		t.Error("不同 uid 派生出相同 MachineID")
	}
	// 必须是合法 hex
	for _, c := range m {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Fatalf("MachineID 含非 hex 字符: %q", c)
		}
	}
}

// TestCredentialStringHidesSecrets 验证打印凭证不会泄露 token。
func TestCredentialStringHidesSecrets(t *testing.T) {
	cred := testCred()
	for _, s := range []string{cred.String(), cred.GoString()} {
		if strings.Contains(s, cred.AccessToken) {
			t.Errorf("Credential 字符串泄露了 access token: %s", s)
		}
		if strings.Contains(s, cred.RefreshToken) {
			t.Errorf("Credential 字符串泄露了 refresh token: %s", s)
		}
	}
}

// TestBillingHeaders 验证额度/签到请求头。
func TestBillingHeaders(t *testing.T) {
	cred := testCred()
	req, _ := http.NewRequest(http.MethodPost, "https://example.invalid/x", nil)
	applyBillingHeaders(req, cred)

	if got := req.Header.Get("Authorization"); got != "Bearer "+cred.AccessToken {
		t.Errorf("Authorization = %q", got)
	}
	if got := req.Header.Get("X-User-Id"); got != cred.UID {
		t.Errorf("X-User-Id = %q，期望 %q", got, cred.UID)
	}
	// 账单请求同样不应带 refresh token
	testutil.AssertNoRefreshTokenHeader(t, req.Header)
}

// TestTraceHeaders 验证链路追踪头齐全且 SpanId 为 16 字符。
func TestTraceHeaders(t *testing.T) {
	mid := newMessageID()
	if len(mid) != 32 {
		t.Fatalf("messageID 长度 = %d，期望 32", len(mid))
	}
	req, _ := http.NewRequest(http.MethodPost, "https://example.invalid/x", nil)
	setTraceHeaders(req, mid)

	if got := req.Header.Get("X-Request-ID"); got != mid {
		t.Errorf("X-Request-ID = %q，期望 %q", got, mid)
	}
	if got := req.Header.Get("X-B3-TraceId"); got != mid {
		t.Errorf("X-B3-TraceId = %q，期望 %q", got, mid)
	}
	span := req.Header.Get("X-B3-SpanId")
	if len(span) != 16 {
		t.Errorf("X-B3-SpanId 长度 = %d，期望 16", len(span))
	}
	if req.Header.Get("X-Root-Request-ID") == "" {
		t.Error("缺少 X-Root-Request-ID")
	}
	// 会话 ID 与消息 ID 必须不同
	if req.Header.Get("X-Conversation-Request-ID") == mid {
		t.Error("X-Conversation-Request-ID 不应等于 message ID")
	}
}

// TestNewMessageIDUnique 验证消息 ID 不重复。
func TestNewMessageIDUnique(t *testing.T) {
	seen := make(map[string]bool, 1000)
	for i := 0; i < 1000; i++ {
		id := newMessageID()
		if seen[id] {
			t.Fatalf("第 %d 次生成重复 ID: %s", i, id)
		}
		seen[id] = true
	}
}

// TestClientURLs 验证端点 URL 拼装正确。
func TestClientURLs(t *testing.T) {
	c := NewClient()
	if got := c.ChatURL(); got != "https://copilot.tencent.com/v2/chat/completions" {
		t.Errorf("ChatURL = %q", got)
	}
	if got := c.ModelsURL(); got != "https://copilot.tencent.com/v2/enterprises/personal/models" {
		t.Errorf("ModelsURL = %q", got)
	}
	if got := c.UserResourceURL(); got != "https://www.codebuddy.cn/v2/billing/meter/get-user-resource" {
		t.Errorf("UserResourceURL = %q", got)
	}
	if got := c.DailyCheckinURL(); got != "https://www.codebuddy.cn/v2/billing/meter/daily-checkin" {
		t.Errorf("DailyCheckinURL = %q", got)
	}
	if got := c.TokenRefreshURL(); got != "https://copilot.tencent.com/v2/plugin/auth/token/refresh" {
		t.Errorf("TokenRefreshURL = %q", got)
	}
}

// TestClientNoOverallTimeout 是【关键】测试：
// 上游客户端绝不能设置整体超时，否则会杀掉长思考流。
func TestClientNoOverallTimeout(t *testing.T) {
	c := NewClient()
	if c.HTTPClient().Timeout != 0 {
		t.Errorf("http.Client.Timeout = %v，必须为 0（不设整体超时）——"+
			"否则长思考流会被中途杀掉；流内空闲超时应由 SSE 读取层负责",
			c.HTTPClient().Timeout)
	}
	tr, ok := c.HTTPClient().Transport.(*http.Transport)
	if !ok {
		t.Fatal("Transport 类型不是 *http.Transport")
	}
	if tr.ResponseHeaderTimeout == 0 {
		t.Error("ResponseHeaderTimeout 为 0，应设置（等待上游响应头的超时）")
	}
}

// TestSetBases 验证可切换到假上游。
func TestSetBases(t *testing.T) {
	c := NewClient()
	c.SetBases("http://127.0.0.1:1", "http://127.0.0.1:2")
	if got := c.ChatURL(); got != "http://127.0.0.1:1/v2/chat/completions" {
		t.Errorf("切换后 ChatURL = %q", got)
	}
	if got := c.DailyCheckinURL(); got != "http://127.0.0.1:2/v2/billing/meter/daily-checkin" {
		t.Errorf("切换后 DailyCheckinURL = %q", got)
	}
}

// TestFakeUpstreamCatchesRefreshToken 是【元测试】：
// 验证 fake upstream 真的能抓到违规请求。
// 如果这个测试失败，说明我们的安全网本身是坏的。
func TestFakeUpstreamCatchesRefreshToken(t *testing.T) {
	// 用一个不 t.Fatal 的 fake（FailOnViolation=false），
	// 这样我们可以直接观察它记录了什么。
	fake := testutil.NewFakeUpstream(t)
	fake.FailOnViolation = false
	fake.WithSSE("sse-space-bunny-max.txt")

	// 故意发一个带 X-Refresh-Token 的请求
	req, _ := http.NewRequest(http.MethodPost, fake.URL+"/v2/chat/completions",
		strings.NewReader(`{"stream":true}`))
	req.Header.Set("Authorization", "Bearer x")
	req.Header.Set("X-User-Id", "u")
	req.Header.Set("X-IDE-Name", "WorkBuddy")
	req.Header.Set("X-Product", "WorkBuddy")
	req.Header.Set("X-Conversation-Request-ID", "a")
	req.Header.Set("X-Conversation-Message-ID", "b")
	req.Header.Set("X-Request-ID", "c")
	req.Header.Set("X-Refresh-Token", "LEAKED") // ← 违规

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求假上游失败: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	rec, ok := fake.LastRequest()
	if !ok {
		t.Fatal("假上游没有记录到请求")
	}
	if rec.Get("X-Refresh-Token") != "LEAKED" {
		t.Fatal("假上游没能记录 X-Refresh-Token —— 安全网失效")
	}

	// 现在验证断言逻辑本身能识别它
	fake2 := testutil.NewFakeUpstream(t)
	fake2.FailOnViolation = false
	fake2.AssertChatContract(rec)
	if !fake2.ViolationDetected() {
		t.Error("断言逻辑未能识别出携带 X-Refresh-Token 的请求")
	}
}

// TestFixtureLoaded 验证 fixture 能被正确嵌入和读取。
func TestFixtureLoaded(t *testing.T) {
	// 最完整的一个：41 个思考分片
	high := testutil.FixtureLines("sse-deepseek-v4.1-flash-high.txt")
	dataLines := 0
	for _, l := range high {
		if strings.HasPrefix(l, "data: ") {
			dataLines++
		}
	}
	// 实测（go run 精确统计）：44 个 data 行 = 43 个 JSON 帧 + 1 个 [DONE]
	if dataLines != 44 {
		t.Errorf("high fixture 的 data 行数 = %d，期望 44（43 帧 + DONE）", dataLines)
	}

	// 不传档位：只有 4 帧 + DONE
	noeffort := testutil.FixtureLines("sse-deepseek-noeffort.txt")
	n := 0
	for _, l := range noeffort {
		if strings.HasPrefix(l, "data: ") {
			n++
		}
	}
	// 实测（go run 精确统计）：3 个 data 行 = 2 个 JSON 帧 + 1 个 [DONE]
	if n != 3 {
		t.Errorf("noeffort fixture 的 data 行数 = %d，期望 3（2 帧 + DONE）", n)
	}
}

// TestFixtureModelsListingParses 验证模型目录 fixture 可解析。
func TestFixtureModelsListingParses(t *testing.T) {
	var payload struct {
		Code int `json:"code"`
		Data struct {
			Models []struct {
				ID              string `json:"id"`
				MaxInputTokens  int64  `json:"maxInputTokens"`
				MaxOutputTokens int64  `json:"maxOutputTokens"`
				SupportsImages  bool   `json:"supportsImages"`
			} `json:"models"`
		} `json:"data"`
	}
	testutil.LoadFixtureJSON(t, "models-listing.json", &payload)

	if payload.Code != 0 {
		t.Errorf("code = %d，期望 0", payload.Code)
	}
	if len(payload.Data.Models) == 0 {
		t.Fatal("模型列表为空")
	}

	// 找到 deepseek，验证权威值是 1M / 128k
	var found bool
	for _, m := range payload.Data.Models {
		if m.ID == "deepseek-v4.1-flash" {
			found = true
			if m.MaxInputTokens != 1000000 {
				t.Errorf("deepseek maxInputTokens = %d，期望 1000000", m.MaxInputTokens)
			}
			if m.MaxOutputTokens != 128000 {
				t.Errorf("deepseek maxOutputTokens = %d，期望 128000", m.MaxOutputTokens)
			}
			if !m.SupportsImages {
				t.Error("deepseek 应支持图片")
			}
		}
	}
	if !found {
		t.Error("fixture 中缺少 deepseek-v4.1-flash")
	}
}

// TestFixtureJSONValid 验证所有 JSON fixture 都能解析。
func TestFixtureJSONValid(t *testing.T) {
	names := []string{
		"models-listing.json",
		"v1-models-exposed.json",
		"chat-completion-aggregated.json",
		"billing-user-resource.json",
		"billing-daily-checkin.json",
		"MANIFEST.json",
	}
	for _, n := range names {
		var v any
		if err := json.Unmarshal(testutil.Fixture(n), &v); err != nil {
			t.Errorf("fixture %s 不是合法 JSON: %v", n, err)
		}
	}
}

// TestCtxNotUsedYet 占位：确保 context 导入有意义（后续步骤会真正使用）。
var _ = context.Background
