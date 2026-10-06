package workbuddy

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"workbuddy.local/workbuddy-api/internal/provider"
	"workbuddy.local/workbuddy-api/internal/testutil"
)

// newFakeClient 把客户端指向假上游，并返回该假上游。
func newFakeClient(t *testing.T) (*Client, *testutil.FakeUpstream) {
	t.Helper()
	fake := testutil.NewFakeUpstream(t)
	c := NewClient()
	c.SetBases(fake.URL, fake.URL)
	return c, fake
}

// TestModelsAgainstRealFixture 用真实上游样本验证模型目录解析。
//
// 这是核心测试：它固定了「上游字段名 → 我们的模型」的映射关系。
func TestModelsAgainstRealFixture(t *testing.T) {
	c, fake := newFakeClient(t)
	fake.WithJSON("models-listing.json")

	models, err := c.Models(context.Background(), testCred(), PlatformCN)
	if err != nil {
		t.Fatalf("拉取模型失败: %v", err)
	}
	if len(models) == 0 {
		t.Fatal("模型列表为空")
	}

	byID := make(map[string]provider.Model, len(models))
	for _, m := range models {
		byID[m.ID] = m
	}

	// ── deepseek-v4.1-flash：验证权威容量值与档位裁剪 ──
	ds, ok := byID["workbuddy/deepseek-v4.1-flash"]
	if !ok {
		t.Fatalf("缺少 workbuddy/deepseek-v4.1-flash；实际有: %v", keys(byID))
	}
	if ds.UpstreamID != "deepseek-v4.1-flash" {
		t.Errorf("UpstreamID = %q", ds.UpstreamID)
	}
	// 上下文必须是最大值 1M，不是默认档 300k
	if ds.Capabilities.ContextWindow != 1000000 {
		t.Errorf("ContextWindow = %d，期望 1000000（最大值，不是默认档 300000）",
			ds.Capabilities.ContextWindow)
	}
	if ds.Capabilities.MaxOutputTokens != 128000 {
		t.Errorf("MaxOutputTokens = %d，期望 128000", ds.Capabilities.MaxOutputTokens)
	}
	if !ds.Capabilities.SupportsImages {
		t.Error("deepseek 应支持图片")
	}
	// 实测是开关型 → 只有 off/high
	if ds.Capabilities.Reasoning == nil {
		t.Fatal("deepseek 应有档位信息")
	}
	if ds.Capabilities.Reasoning.Mode != provider.ModeSwitch {
		t.Errorf("deepseek 模式 = %q，期望 %q（实测是纯开关）",
			ds.Capabilities.Reasoning.Mode, provider.ModeSwitch)
	}
	if !ds.Capabilities.Reasoning.Verified {
		t.Error("deepseek 档位应标记为已实测验证")
	}
	if got := ds.Capabilities.Reasoning.Levels; len(got) != 2 || got[0] != "off" || got[1] != "high" {
		t.Errorf("deepseek 档位 = %v，期望 [off high]", got)
	}
	if ds.Capabilities.Reasoning.Default != "high" {
		t.Errorf("deepseek 默认档 = %q，期望 high（委托方要求）", ds.Capabilities.Reasoning.Default)
	}

	// ── space-bunny：验证真旋钮 5 档 ──
	sb, ok := byID["workbuddy/space-bunny"]
	if !ok {
		t.Fatalf("缺少 workbuddy/space-bunny")
	}
	if sb.Capabilities.Reasoning == nil {
		t.Fatal("space-bunny 应有档位信息")
	}
	if sb.Capabilities.Reasoning.Mode != provider.ModeKnob {
		t.Errorf("space-bunny 模式 = %q，期望 %q", sb.Capabilities.Reasoning.Mode, provider.ModeKnob)
	}
	wantLevels := []string{"off", "low", "medium", "high", "xhigh", "max"}
	if !equalStrs(sb.Capabilities.Reasoning.Levels, wantLevels) {
		t.Errorf("space-bunny 档位 = %v，期望 %v", sb.Capabilities.Reasoning.Levels, wantLevels)
	}

	// ── glm-5.3：验证 knob-flat ──
	glm, ok := byID["workbuddy/glm-5.3"]
	if !ok {
		t.Fatal("缺少 workbuddy/glm-5.3")
	}
	if glm.Capabilities.Reasoning.Mode != provider.ModeKnobFlat {
		t.Errorf("glm-5.3 模式 = %q，期望 %q", glm.Capabilities.Reasoning.Mode, provider.ModeKnobFlat)
	}

	// ── hunyuan-chat：非思考模型 ──
	hy, ok := byID["workbuddy/hunyuan-chat"]
	if !ok {
		t.Fatal("缺少 workbuddy/hunyuan-chat")
	}
	if hy.Capabilities.Reasoning != nil {
		t.Errorf("hunyuan-chat 不应有档位，实际 = %+v", hy.Capabilities.Reasoning)
	}
	if hy.Capabilities.SupportsImages {
		t.Error("hunyuan-chat 不应支持图片")
	}
}

// TestModelsAllHavePrefix 验证所有模型 ID 都带渠道前缀。
func TestModelsAllHavePrefix(t *testing.T) {
	c, fake := newFakeClient(t)
	fake.WithJSON("models-listing.json")

	models, err := c.Models(context.Background(), testCred(), PlatformCN)
	if err != nil {
		t.Fatalf("拉取失败: %v", err)
	}
	for _, m := range models {
		if len(m.ID) < len(ProviderID)+1 || m.ID[:len(ProviderID)+1] != ProviderID+"/" {
			t.Errorf("模型 ID %q 缺少 %q 前缀", m.ID, ProviderID+"/")
		}
		if m.UpstreamID == "" {
			t.Errorf("模型 %q 的 UpstreamID 为空", m.ID)
		}
		if m.UpstreamID == m.ID {
			t.Errorf("模型 %q 的 UpstreamID 应去掉前缀", m.ID)
		}
	}
}

// TestModelsSorted 验证输出顺序稳定（便于测试与人工比对）。
func TestModelsSorted(t *testing.T) {
	c, fake := newFakeClient(t)
	fake.WithJSON("models-listing.json")

	models, err := c.Models(context.Background(), testCred(), PlatformCN)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(models); i++ {
		if models[i-1].ID > models[i].ID {
			t.Errorf("模型未排序: %q 出现在 %q 之前", models[i-1].ID, models[i].ID)
		}
	}
}

// TestModelsSendsRequiredHeaders 验证目录请求也带齐身份头。
func TestModelsSendsRequiredHeaders(t *testing.T) {
	c, fake := newFakeClient(t)
	fake.WithJSON("models-listing.json")

	if _, err := c.Models(context.Background(), testCred(), PlatformCN); err != nil {
		t.Fatalf("拉取失败: %v", err)
	}

	rec, ok := fake.LastRequest()
	if !ok {
		t.Fatal("假上游未记录请求")
	}
	if rec.Method != http.MethodGet {
		t.Errorf("方法 = %s，期望 GET", rec.Method)
	}
	checks := map[string]string{
		"X-User-Id":  testCred().UID,
		"X-IDE-Name": "WorkBuddy",
		"X-Product":  "WorkBuddy",
	}
	for k, want := range checks {
		if got := rec.Get(k); got != want {
			t.Errorf("头 %s = %q，期望 %q", k, got, want)
		}
	}
	// 安全红线同样适用于目录请求
	testutil.AssertNoRefreshTokenHeader(t, rec.Header)
}

// TestModelsUnauthorized 验证 401 被正确识别为凭证问题。
func TestModelsUnauthorized(t *testing.T) {
	c, fake := newFakeClient(t)
	fake.WithStatus(http.StatusUnauthorized)

	_, err := c.Models(context.Background(), testCred(), PlatformCN)
	if err == nil {
		t.Fatal("401 应返回错误")
	}
	ue, ok := err.(*UpstreamError)
	if !ok {
		t.Fatalf("错误类型 = %T，期望 *UpstreamError", err)
	}
	if !ue.IsAuthFailure() {
		t.Error("401 应被识别为凭证失效")
	}
}

// TestModelsBizCodeNonZero 验证业务 code 非 0 被识别。
func TestModelsBizCodeNonZero(t *testing.T) {
	c, fake := newFakeClient(t)
	fake.JSONBody = []byte(`{"code":40001,"msg":"token invalid"}`)

	_, err := c.Models(context.Background(), testCred(), PlatformCN)
	if err == nil {
		t.Fatal("业务 code 非 0 应返回错误")
	}
	ue, ok := err.(*UpstreamError)
	if !ok {
		t.Fatalf("错误类型 = %T", err)
	}
	if ue.BizCode != 40001 {
		t.Errorf("BizCode = %d，期望 40001", ue.BizCode)
	}
	if ue.BizMsg != "token invalid" {
		t.Errorf("BizMsg = %q", ue.BizMsg)
	}
}

// TestModelsSkipsEmptyID 验证缺 id 的记录被跳过而不是让整个目录失败。
func TestModelsSkipsEmptyID(t *testing.T) {
	c, fake := newFakeClient(t)
	fake.JSONBody = []byte(`{"code":0,"msg":"OK","data":{"models":[
		{"id":"","name":"bad"},
		{"id":"good-one","name":"Good","maxInputTokens":1000,"maxOutputTokens":100}
	]}}`)

	models, err := c.Models(context.Background(), testCred(), PlatformCN)
	if err != nil {
		t.Fatalf("不应因单条坏记录失败: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("模型数 = %d，期望 1（坏记录被跳过）", len(models))
	}
	if models[0].UpstreamID != "good-one" {
		t.Errorf("保留的模型 = %q", models[0].UpstreamID)
	}
}

// TestModelsUnverifiedUsesDeclared 验证未实测的模型回退到上游声明，
// 并标记 Verified=false。
func TestModelsUnverifiedUsesDeclared(t *testing.T) {
	c, fake := newFakeClient(t)
	fake.JSONBody = []byte(`{"code":0,"msg":"OK","data":{"models":[
		{"id":"brand-new","name":"Brand New","maxInputTokens":500000,"maxOutputTokens":32000,
		 "reasoning":{"supportedEfforts":["low","high","xhigh"],"defaultEffort":"high"}}
	]}}`)

	models, err := c.Models(context.Background(), testCred(), PlatformCN)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 {
		t.Fatalf("模型数 = %d", len(models))
	}
	rc := models[0].Capabilities.Reasoning
	if rc == nil {
		t.Fatal("应推断出档位信息")
	}
	if rc.Verified {
		t.Error("未实测的模型不应标记 Verified=true")
	}
	if rc.Default != "high" {
		t.Errorf("默认档 = %q，期望 high（取上游 defaultEffort）", rc.Default)
	}
	want := []string{"off", "low", "high", "xhigh"}
	if !equalStrs(rc.Levels, want) {
		t.Errorf("档位 = %v，期望 %v", rc.Levels, want)
	}
}

// TestCleanEffortsFiltersInvalid 验证非法档位被过滤掉。
//
// 上游对非法档位值返回 400，所以绝不能透传。
func TestCleanEffortsFiltersInvalid(t *testing.T) {
	in := []string{"low", "x-high", "high", "maximum", "auto", "MAX", "", "  ", "ultra", "low"}
	got := cleanEfforts(in)
	want := []string{"low", "high", "max", "ultra"}
	if !equalStrs(got, want) {
		t.Errorf("cleanEfforts = %v，期望 %v", got, want)
	}
}

// TestConvertModelContextFallback 验证没有 maxInputTokens 时用上下文档位兜底。
func TestConvertModelContextFallback(t *testing.T) {
	var wm wireModel
	if err := json.Unmarshal([]byte(`{
		"id":"m1","name":"M1","maxOutputTokens":1000,
		"contextWindow":{"defaultLength":300000,"supportedLengths":[300000,600000,1000000]}
	}`), &wm); err != nil {
		t.Fatal(err)
	}
	got, ok := convertModel(wm, PlatformCN)
	if !ok {
		t.Fatal("convertModel 应成功")
	}
	if got.Capabilities.ContextWindow != 1000000 {
		t.Errorf("ContextWindow = %d，期望 1000000（取 supportedLengths 最大值）",
			got.Capabilities.ContextWindow)
	}
}

// TestConvertModelEmptyIDRejected 验证空 id 被拒。
func TestConvertModelEmptyIDRejected(t *testing.T) {
	if _, ok := convertModel(wireModel{ID: "  "}, PlatformCN); ok {
		t.Error("空 id 应被拒绝")
	}
}

// ── 测试辅助 ──

func keys(m map[string]provider.Model) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
