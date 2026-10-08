package responses

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// 本文件的取值/赋值小工具。
//
// ⚠️ 与 anthropic 包的 util.go 是**刻意重复**的：Go 的 internal 包不能
// 跨包共享未导出函数，而为这几个纯函数抽一个公共包，会让两个协议包
// 产生一个"谁都不拥有"的中间层（改一处要同时想两边）。
// 重复的是 60 行无状态纯函数，风险远小于耦合。

// asString 宽容取字符串。
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

// asInt 宽容取整数。
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

// firstNonEmpty 返回首个非空串。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// orEmptyObject 参数为空时返回空 JSON Schema。
//
// 🔴 上游要求 tools[].function.parameters 存在，缺失会直接 400。
func orEmptyObject(v any) any {
	if m, ok := v.(map[string]any); ok && len(m) > 0 {
		return m
	}
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

// stringify 把任意值转成字符串。
func stringify(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	}
	if raw, err := json.Marshal(v); err == nil {
		return string(raw)
	}
	return ""
}

// randSuffix 生成 12 位十六进制随机后缀。
func randSuffix() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "000000000000"
	}
	return hex.EncodeToString(b[:])
}

// intToStr 无依赖的 int64 → string。
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
