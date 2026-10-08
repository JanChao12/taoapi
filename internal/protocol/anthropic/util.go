package anthropic

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// 本文件的取值/赋值小工具。
//
// 设计取舍（照搬参考实现的判断）：Anthropic 请求体的字段类型在客户端之间
// 并不统一 —— 同一个语义可能是 string / number / bool / null。典型例子：
//
//	system    : "..."  或  [{type:"text",text:"..."}]
//	content   : "..."  或  [{type:"text",...}]
//	tool_result.content : "..."  或  [{type:"text",...}]
//
// 因此统一走「宽容取值」而不是严格结构体解码 —— 否则要为兼容个别客户端
// 层层加 fallback 字段，代码会变得比协议本身还复杂。

// asString 宽容取字符串：string 原样；number/bool 转字面量；其余返回空串。
func asString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	case float64:
		if t == float64(int64(t)) {
			return json.Number(intToStr(int64(t))).String()
		}
		raw, _ := json.Marshal(t)
		return string(raw)
	case bool:
		if t {
			return "true"
		}
		return "false"
	}
	return ""
}

// asBool 宽容取布尔。
func asBool(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return strings.EqualFold(strings.TrimSpace(t), "true")
	}
	return false
}

// asInt 宽容取整数（json 解码后数字是 float64）。
func asInt(v any) (int, bool) {
	switch t := v.(type) {
	case float64:
		return int(t), true
	case int:
		return t, true
	case int64:
		return int(t), true
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return int(n), true
		}
	case string:
		var n int
		if err := json.Unmarshal([]byte(t), &n); err == nil {
			return n, true
		}
	}
	return 0, false
}

// asFloat 宽容取浮点。
func asFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case json.Number:
		if f, err := t.Float64(); err == nil {
			return f, true
		}
	}
	return 0, false
}

// isEmptyValue 判定 null / 空串 / 空数组 / 空对象。
func isEmptyValue(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(t) == ""
	case []any:
		return len(t) == 0
	case map[string]any:
		return len(t) == 0
	}
	return false
}

// flattenText 从 string 或 content block 数组里抽出纯文本。
//
// 用途：system（顶层字段）与 tool_result.content 都允许两种形状。
func flattenText(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []any:
		var sb strings.Builder
		for _, b := range t {
			bm, ok := b.(map[string]any)
			if !ok {
				continue
			}
			// 只取文本块；图片/工具块在这里没有文本语义
			if s := asString(bm["text"]); s != "" {
				sb.WriteString(s)
			}
		}
		return sb.String()
	}
	return ""
}

// firstNonEmpty 返回首个非空串。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// orEmptyObject 参数为空时返回一个空 JSON Schema 对象。
//
// 🔴 上游要求 tools[].function.parameters 存在，缺失会直接 400。
//
//	Anthropic 的 input_schema 理论上必填，但客户端漏发时我们不能把
//	null 透传上去 —— 那等于把一个必然失败的请求发给上游。
func orEmptyObject(v any) any {
	if m, ok := v.(map[string]any); ok && len(m) > 0 {
		return m
	}
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

// stringify 把任意值转成字符串（tool_result 的非文本内容兜底）。
func stringify(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []any:
		if s := flattenText(t); strings.TrimSpace(s) != "" {
			return s
		}
	}
	if raw, err := json.Marshal(v); err == nil {
		return string(raw)
	}
	return ""
}

// randSuffix 生成 12 位十六进制随机后缀，用于拼装 msg_/toolu_ id。
//
// 随机源不可用时退化为固定串 —— 它只用于 id 唯一性，
// 退化不影响正确性（宁可重复也不 panic）。
func randSuffix() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "000000000000"
	}
	return hex.EncodeToString(b[:])
}

// intToStr 无依赖的 int64 → string（避免为 asString 引入 strconv 之外的路径）。
func intToStr(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [24]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
