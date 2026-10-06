package openai

import (
	"encoding/json"
	"testing"

	"workbuddy.local/workbuddy-api/internal/provider"
)

// TestFromProviderModelsShapes 验证 provider 模型 → 对外模型的映射。
func TestFromProviderModelsShapes(t *testing.T) {
	in := []provider.Model{
		{
			ID:         "workbuddy/space-bunny",
			UpstreamID: "space-bunny",
			Name:       "Space-Bunny",
			Capabilities: provider.Capabilities{
				ContextWindow:   1000000,
				MaxOutputTokens: 128000,
				SupportsImages:  true,
				SupportsTools:   true,
				Reasoning: &provider.ReasoningCapability{
					Mode:     provider.ModeKnob,
					Levels:   []string{"off", "low", "medium", "high", "xhigh", "max"},
					Default:  "high",
					Verified: true,
				},
			},
		},
	}

	list := FromProviderModels(in)
	if list.Object != "list" {
		t.Errorf("object = %q，期望 list", list.Object)
	}
	if len(list.Data) != 1 {
		t.Fatalf("模型数 = %d", len(list.Data))
	}

	m := list.Data[0]
	if m.ID != "workbuddy/space-bunny" {
		t.Errorf("ID = %q", m.ID)
	}
	if m.OwnedBy != "workbuddy" {
		t.Errorf("OwnedBy = %q，期望 workbuddy（从前缀解析）", m.OwnedBy)
	}
	if m.ContextLength != 1000000 {
		t.Errorf("ContextLength = %d，期望 1000000", m.ContextLength)
	}
	if m.MaxOutputTokens != 128000 {
		t.Errorf("MaxOutputTokens = %d", m.MaxOutputTokens)
	}
	if m.Architecture == nil {
		t.Fatal("Architecture 不应为空")
	}
	if !contains(m.Architecture.InputModalities, "image") {
		t.Errorf("input_modalities = %v，应含 image", m.Architecture.InputModalities)
	}
	if m.ReasoningMode != string(provider.ModeKnob) {
		t.Errorf("ReasoningMode = %q", m.ReasoningMode)
	}
	if !m.ReasoningVerified {
		t.Error("ReasoningVerified 应为 true")
	}
	if len(m.ReasoningEfforts) != 6 {
		t.Errorf("ReasoningEfforts = %v", m.ReasoningEfforts)
	}
}

// TestFromProviderModelsTextOnly 验证纯文本模型的模态描述。
func TestFromProviderModelsTextOnly(t *testing.T) {
	in := []provider.Model{{
		ID:   "workbuddy/hunyuan-chat",
		Name: "Hunyuan-Turbos",
		Capabilities: provider.Capabilities{
			ContextWindow:   200000,
			MaxOutputTokens: 8192,
			SupportsImages:  false,
		},
	}}
	list := FromProviderModels(in)
	m := list.Data[0]
	if m.Architecture == nil {
		t.Fatal("Architecture 不应为空")
	}
	if contains(m.Architecture.InputModalities, "image") {
		t.Error("不支持图片的模型不应声明 image")
	}
	if m.Architecture.Modality != "text->text" {
		t.Errorf("Modality = %q", m.Architecture.Modality)
	}
	if m.ReasoningEfforts != nil {
		t.Errorf("无档位模型不应有 ReasoningEfforts，实际 %v", m.ReasoningEfforts)
	}
}

// TestFromProviderModelsJSONShape 验证序列化后的 JSON 字段名
// 与 DSH 期望的一致（这是与客户端的契约）。
func TestFromProviderModelsJSONShape(t *testing.T) {
	in := []provider.Model{{
		ID:   "workbuddy/glm-5.3",
		Name: "GLM-5.3",
		Capabilities: provider.Capabilities{
			ContextWindow:   1000000,
			MaxOutputTokens: 64000,
			SupportsImages:  true,
			Reasoning: &provider.ReasoningCapability{
				Mode:     provider.ModeKnobFlat,
				Levels:   []string{"off", "low", "high", "max"},
				Default:  "high",
				Verified: true,
			},
		},
	}}

	raw, err := json.Marshal(FromProviderModels(in))
	if err != nil {
		t.Fatal(err)
	}

	var parsed struct {
		Object string `json:"object"`
		Data   []struct {
			ID                string   `json:"id"`
			Object            string   `json:"object"`
			OwnedBy           string   `json:"owned_by"`
			Name              string   `json:"name"`
			ContextLength     int64    `json:"context_length"`
			MaxOutputTokens   int64    `json:"max_output_tokens"`
			ReasoningEfforts  []string `json:"reasoning_efforts"`
			ReasoningDefault  string   `json:"reasoning_default"`
			ReasoningVerified bool     `json:"reasoning_verified"`
			ReasoningMode     string   `json:"reasoning_mode"`
			Architecture      struct {
				InputModalities []string `json:"input_modalities"`
				Modality        string   `json:"modality"`
			} `json:"architecture"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("解析失败: %v\nJSON: %s", err, raw)
	}
	if parsed.Object != "list" {
		t.Errorf("顶层 object = %q", parsed.Object)
	}
	d := parsed.Data[0]
	// DSH 会读的字段
	if d.ID == "" || d.Object != "model" || d.OwnedBy == "" || d.ContextLength == 0 || d.MaxOutputTokens == 0 {
		t.Errorf("DSH 会读的字段不完整: %+v", d)
	}
	// 扩展字段
	if len(d.ReasoningEfforts) != 4 {
		t.Errorf("reasoning_efforts = %v", d.ReasoningEfforts)
	}
	if len(d.Architecture.InputModalities) == 0 {
		t.Error("architecture.input_modalities 为空")
	}
}

// TestOwnerOf 验证 owned_by 解析。
func TestOwnerOf(t *testing.T) {
	cases := map[string]string{
		"workbuddy/space-bunny": "workbuddy",
		"other/model":           "other",
		"noprefix":              "workbuddy",
		"/leading":              "",
	}
	for in, want := range cases {
		if got := ownerOf(in); got != want {
			t.Errorf("ownerOf(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// TestNewErrorShape 验证错误响应形状符合 OpenAI 约定。
func TestNewErrorShape(t *testing.T) {
	e := NewError(ErrTypeInvalidRequest, "bad_model", "未知模型")
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Error.Code != "bad_model" {
		t.Errorf("error.code = %q", parsed.Error.Code)
	}
	if parsed.Error.Type != ErrTypeInvalidRequest {
		t.Errorf("error.type = %q", parsed.Error.Type)
	}
}

// TestNewModelListNil 验证 nil 输入产出空数组而非 null。
func TestNewModelListNil(t *testing.T) {
	raw, err := json.Marshal(NewModelList(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"object":"list","data":[]}`
	if string(raw) != want {
		t.Errorf("序列化 = %s，期望 %s（data 必须是 [] 而不是 null）", raw, want)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
