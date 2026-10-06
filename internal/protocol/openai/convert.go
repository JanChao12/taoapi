package openai

import (
	"workbuddy.local/workbuddy-api/internal/provider"
)

// FromProviderModels 把 provider 层模型转换为对外模型列表。
//
// 转换要点：
//   - ContextLength 取 provider 已解析的【最大值】（上游 maxInputTokens）
//   - 多模态能力映射为 Architecture.InputModalities
//   - 思考档位放进扩展字段（DSH 不读，但保留给其他客户端与面板）
func FromProviderModels(models []provider.Model) ModelList {
	out := make([]Model, 0, len(models))
	for _, m := range models {
		om := Model{
			ID:              m.ID,
			Object:          "model",
			OwnedBy:         ownerOf(m.ID),
			Name:            m.Name,
			ContextLength:   m.Capabilities.ContextWindow,
			MaxOutputTokens: m.Capabilities.MaxOutputTokens,
			Architecture:    architectureOf(m.Capabilities),
		}
		if rc := m.Capabilities.Reasoning; rc != nil {
			om.ReasoningEfforts = rc.Levels
			om.ReasoningDefault = rc.Default
			om.ReasoningVerified = rc.Verified
			om.ReasoningMode = string(rc.Mode)
		}
		// 计费倍率：只有上游真的返回了才带上字段。
		// nil 与 0 的区别见 Model.Credits 的说明（未知 vs 免费）。
		if m.Pricing != nil && m.Pricing.HasMultiplier {
			v := m.Pricing.Multiplier
			om.Credits = &v
		}
		if m.Badge != nil {
			om.Badge = &ModelBadge{Text: m.Badge.Text, Color: m.Badge.Color}
		}
		// 运营活动：优先于 badge 简写（它带时间窗，能避免显示过期活动）
		if m.Promotion != nil {
			om.Promotion = &ModelPromotion{
				Label:      m.Promotion.Label,
				Color:      m.Promotion.Color,
				Free:       m.Promotion.Free,
				Note:       m.Promotion.Note,
				ValidUntil: m.Promotion.ValidUntil,
			}
		}
		out = append(out, om)
	}
	return NewModelList(out)
}

// architectureOf 依据能力构造模态描述。
func architectureOf(c provider.Capabilities) *Architecture {
	inputs := []string{"text"}
	if c.SupportsImages {
		inputs = append(inputs, "image")
	}
	modality := "text->text"
	if c.SupportsImages {
		modality = "text+image->text"
	}
	return &Architecture{
		InputModalities:  inputs,
		OutputModalities: []string{"text"},
		Modality:         modality,
	}
}

// ownerOf 从带前缀的模型 ID 里取出渠道名作为 owned_by。
//
//	"workbuddy/space-bunny" → "workbuddy"
//	"space-bunny"           → "workbuddy"（无前缀时的兜底）
func ownerOf(id string) string {
	for i := 0; i < len(id); i++ {
		if id[i] == '/' {
			return id[:i]
		}
	}
	return "workbuddy"
}
