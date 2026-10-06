package app

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy.local/workbuddy-api/internal/protocol/openai"
	"workbuddy.local/workbuddy-api/internal/provider"
	"workbuddy.local/workbuddy-api/internal/router"
	"workbuddy.local/workbuddy-api/internal/testutil"
)

// stubProvider 是测试用 provider。
type stubProvider struct {
	id     string
	models []provider.Model
	err    error
}

func (s *stubProvider) ID() string { return s.id }
func (s *stubProvider) Models(context.Context) ([]provider.Model, error) {
	return s.models, s.err
}
func (s *stubProvider) Chat(context.Context, provider.ChatRequest, func(provider.Event) error) error {
	return provider.ErrNotImplemented
}
func (s *stubProvider) Checkin(context.Context, string) (provider.CheckinResult, error) {
	return provider.CheckinResult{}, provider.ErrNotImplemented
}
func (s *stubProvider) Credit(context.Context, string) (provider.CreditResult, error) {
	return provider.CreditResult{}, provider.ErrNotImplemented
}

func newTestRouter(t *testing.T) *router.Router {
	t.Helper()
	r := router.New()
	p := &stubProvider{
		id: "workbuddy",
		models: []provider.Model{
			{
				ID: "workbuddy/space-bunny", UpstreamID: "space-bunny", Name: "Space-Bunny",
				Capabilities: provider.Capabilities{
					ContextWindow: 1000000, MaxOutputTokens: 128000, SupportsImages: true,
					Reasoning: &provider.ReasoningCapability{
						Mode:    provider.ModeKnob,
						Levels:  []string{"off", "low", "medium", "high", "xhigh", "max"},
						Default: "high", Verified: true,
					},
				},
			},
		},
	}
	if err := r.Register(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	return r
}

// TestV1ModelsReturnsExpectedShape 是【端到端】测试：
// 启动真实 HTTP 服务，请求 /v1/models，验证返回形状与档位声明。
func TestV1ModelsReturnsExpectedShape(t *testing.T) {
	h := newMux(Deps{Router: newTestRouter(t), Logger: log.New(io.Discard, "", 0)})
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}

	var list openai.ModelList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if list.Object != "list" {
		t.Errorf("object = %q", list.Object)
	}
	if len(list.Data) != 1 {
		t.Fatalf("模型数 = %d", len(list.Data))
	}

	m := list.Data[0]
	if m.ID != "workbuddy/space-bunny" {
		t.Errorf("id = %q", m.ID)
	}
	// DSH 会读的字段
	if m.ContextLength != 1000000 {
		t.Errorf("context_length = %d，期望 1000000", m.ContextLength)
	}
	if m.MaxOutputTokens != 128000 {
		t.Errorf("max_output_tokens = %d", m.MaxOutputTokens)
	}
	if m.OwnedBy != "workbuddy" {
		t.Errorf("owned_by = %q", m.OwnedBy)
	}
	// 档位扩展字段
	if len(m.ReasoningEfforts) != 6 {
		t.Errorf("reasoning_efforts = %v", m.ReasoningEfforts)
	}
	if m.ReasoningDefault != "high" {
		t.Errorf("reasoning_default = %q，期望 high", m.ReasoningDefault)
	}
	if !m.ReasoningVerified {
		t.Error("reasoning_verified 应为 true")
	}
}

// TestV1ModelsEmptyRouter 验证未配置 provider 时返回空列表（不是报错）。
func TestV1ModelsEmptyRouter(t *testing.T) {
	h := newMux(Deps{Logger: log.New(io.Discard, "", 0)})
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（空列表而非错误）", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"data":[]`) {
		t.Errorf("响应应含空数组，实际: %s", body)
	}
}

// TestV1ModelsMethodNotAllowed 验证非 GET 被拒。
func TestV1ModelsMethodNotAllowed(t *testing.T) {
	h := newMux(Deps{Router: newTestRouter(t), Logger: log.New(io.Discard, "", 0)})
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/models", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("状态码 = %d，期望 405", resp.StatusCode)
	}
}

// TestStatusReportsModelCount 验证 /status 报告模型数与渠道。
func TestStatusReportsModelCount(t *testing.T) {
	h := newMux(Deps{Router: newTestRouter(t), Logger: log.New(io.Discard, "", 0)})
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var st struct {
		Providers  []string `json:"providers"`
		ModelCount int      `json:"modelCount"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if st.ModelCount != 1 {
		t.Errorf("modelCount = %d，期望 1", st.ModelCount)
	}
	if len(st.Providers) != 1 || st.Providers[0] != "workbuddy" {
		t.Errorf("providers = %v", st.Providers)
	}
}

// TestIntegrationWithRealFixture 用【真实上游样本】走完整链路：
// 假上游 → workbuddy client → router → /v1/models。
//
// 这是最有价值的集成测试：它证明「真实上游响应能被正确转换成 DSH 能用的模型列表」。
func TestIntegrationWithRealFixture(t *testing.T) {
	// 1) 起假上游，回放真实 fixture
	fake := testutil.NewFakeUpstream(t)
	fake.WithJSON("models-listing.json")

	// 2) 把 workbuddy client 指向假上游
	wbClient := newWorkbuddyClientForTest(t, fake.URL)
	wbProv := newWorkbuddyProviderForTest(wbClient)

	// 3) 注册到 router
	r := router.New()
	if err := r.Register(context.Background(), wbProv); err != nil {
		t.Fatalf("注册失败: %v", err)
	}

	// 4) 起真实服务
	h := newMux(Deps{Router: r, Logger: log.New(io.Discard, "", 0)})
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var list openai.ModelList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}

	// fixture 里有 4 个模型
	if len(list.Data) != 4 {
		t.Fatalf("模型数 = %d，期望 4（来自真实 fixture）", len(list.Data))
	}

	byID := make(map[string]openai.Model, len(list.Data))
	for _, m := range list.Data {
		byID[m.ID] = m
	}

	// 验证 deepseek：容量取最大值 + 档位被裁剪为开关型
	ds, ok := byID["workbuddy/deepseek-v4.1-flash"]
	if !ok {
		t.Fatalf("缺少 deepseek；实际有 %v", mapKeys(byID))
	}
	if ds.ContextLength != 1000000 {
		t.Errorf("deepseek context_length = %d，期望 1000000（最大值）", ds.ContextLength)
	}
	if ds.MaxOutputTokens != 128000 {
		t.Errorf("deepseek max_output_tokens = %d，期望 128000", ds.MaxOutputTokens)
	}
	if ds.ReasoningMode != string(provider.ModeSwitch) {
		t.Errorf("deepseek reasoning_mode = %q，期望 switch（实测）", ds.ReasoningMode)
	}
	if len(ds.ReasoningEfforts) != 2 {
		t.Errorf("deepseek reasoning_efforts = %v，期望 [off high]（实测裁剪）", ds.ReasoningEfforts)
	}

	// 验证 space-bunny：真旋钮 5 档
	sb := byID["workbuddy/space-bunny"]
	if sb.ReasoningMode != string(provider.ModeKnob) {
		t.Errorf("space-bunny reasoning_mode = %q", sb.ReasoningMode)
	}
	if len(sb.ReasoningEfforts) != 6 {
		t.Errorf("space-bunny reasoning_efforts = %v", sb.ReasoningEfforts)
	}

	// 验证 hunyuan-chat：非思考模型无档位
	hy := byID["workbuddy/hunyuan-chat"]
	if len(hy.ReasoningEfforts) != 0 {
		t.Errorf("hunyuan-chat 不应有档位，实际 %v", hy.ReasoningEfforts)
	}

	// 全部模型必须带前缀
	for _, m := range list.Data {
		if !strings.HasPrefix(m.ID, "workbuddy/") {
			t.Errorf("模型 %q 缺前缀", m.ID)
		}
	}
}

func mapKeys(m map[string]openai.Model) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
