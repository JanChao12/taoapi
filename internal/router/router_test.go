package router

import (
	"context"
	"errors"
	"testing"

	"workbuddy.local/workbuddy-api/internal/provider"
)

// fakeProvider 是测试用 provider。
type fakeProvider struct {
	id     string
	models []provider.Model
	err    error
}

func (f *fakeProvider) ID() string { return f.id }

func (f *fakeProvider) Models(context.Context) ([]provider.Model, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.models, nil
}

func (f *fakeProvider) Chat(context.Context, provider.ChatRequest, func(provider.Event) error) error {
	return provider.ErrNotImplemented
}
func (f *fakeProvider) Checkin(context.Context, string) (provider.CheckinResult, error) {
	return provider.CheckinResult{}, provider.ErrNotImplemented
}
func (f *fakeProvider) Credit(context.Context, string) (provider.CreditResult, error) {
	return provider.CreditResult{}, provider.ErrNotImplemented
}

func wbProvider() *fakeProvider {
	return &fakeProvider{
		id: "workbuddy",
		models: []provider.Model{
			{ID: "workbuddy/space-bunny", UpstreamID: "space-bunny", Name: "Space-Bunny"},
			{ID: "workbuddy/glm-5.3", UpstreamID: "glm-5.3", Name: "GLM-5.3"},
		},
	}
}

// TestResolveFullName 验证全名解析。
func TestResolveFullName(t *testing.T) {
	r := New()
	if err := r.Register(context.Background(), wbProvider()); err != nil {
		t.Fatal(err)
	}

	p, up, m, err := r.Resolve("workbuddy/space-bunny")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if p.ID() != "workbuddy" {
		t.Errorf("provider = %q", p.ID())
	}
	if up != "space-bunny" {
		t.Errorf("upstreamID = %q，期望去掉前缀", up)
	}
	if m.Name != "Space-Bunny" {
		t.Errorf("name = %q", m.Name)
	}
}

// TestResolveBareName 验证省略前缀也能解析（单渠道便利）。
func TestResolveBareName(t *testing.T) {
	r := New()
	if err := r.Register(context.Background(), wbProvider()); err != nil {
		t.Fatal(err)
	}

	_, up, _, err := r.Resolve("glm-5.3")
	if err != nil {
		t.Fatalf("无前缀解析失败: %v", err)
	}
	if up != "glm-5.3" {
		t.Errorf("upstreamID = %q", up)
	}
}

// TestResolveUnknown 验证未知模型返回明确错误。
func TestResolveUnknown(t *testing.T) {
	r := New()
	if err := r.Register(context.Background(), wbProvider()); err != nil {
		t.Fatal(err)
	}

	_, _, _, err := r.Resolve("workbuddy/does-not-exist")
	if err == nil {
		t.Fatal("未知模型应报错")
	}
	if !errors.Is(err, ErrModelNotFound) {
		t.Errorf("错误应为 ErrModelNotFound，实际: %v", err)
	}
}

// TestResolveEmpty 验证空模型名被拒。
func TestResolveEmpty(t *testing.T) {
	r := New()
	if err := r.Register(context.Background(), wbProvider()); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := r.Resolve("   "); err == nil {
		t.Error("空模型名应报错")
	}
}

// TestRegisterFailureIsolated 验证单个 provider 注册失败不影响其他。
//
// 这是「故障隔离」要求的具体测试。
func TestRegisterFailureIsolated(t *testing.T) {
	r := New()

	// 先注册一个正常的
	if err := r.Register(context.Background(), wbProvider()); err != nil {
		t.Fatal(err)
	}

	// 再注册一个会失败的
	bad := &fakeProvider{id: "broken", err: errors.New("上游不可达")}
	if err := r.Register(context.Background(), bad); err == nil {
		t.Fatal("失败的 provider 应返回错误")
	}

	// 原有的必须仍然可用
	if _, _, _, err := r.Resolve("workbuddy/space-bunny"); err != nil {
		t.Errorf("好渠道被坏渠道影响: %v", err)
	}
	if len(r.Providers()) != 1 {
		t.Errorf("已注册渠道数 = %d，期望 1（坏的不应被注册）", len(r.Providers()))
	}
}

// TestModelsReturnsAll 验证模型列表完整。
func TestModelsReturnsAll(t *testing.T) {
	r := New()
	if err := r.Register(context.Background(), wbProvider()); err != nil {
		t.Fatal(err)
	}
	if got := len(r.Models()); got != 2 {
		t.Errorf("模型数 = %d，期望 2", got)
	}
}

// TestUpstreamModelID 验证去前缀便捷方法。
func TestUpstreamModelID(t *testing.T) {
	r := New()
	if err := r.Register(context.Background(), wbProvider()); err != nil {
		t.Fatal(err)
	}
	got, err := r.UpstreamModelID("workbuddy/glm-5.3")
	if err != nil {
		t.Fatal(err)
	}
	if got != "glm-5.3" {
		t.Errorf("UpstreamModelID = %q，期望 glm-5.3", got)
	}
}
