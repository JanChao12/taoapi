package auth

import (
	"time"

	"workbuddy.local/workbuddy-api/internal/pool"
)

// 时间序列化统一用 RFC3339 UTC。
//
// 理由：跨时区/跨夏令时不会歧义；面板需要展示北京时间时再转。
func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// parseRFC3339 解析时间；空串或非法返回零值。
func parseRFC3339(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// poolStatus 把落盘的字符串还原为调度状态。
//
// 空串（旧文件缺字段）按正常处理 —— 不能因为缺字段就排除账号。
func poolStatus(s string) pool.Status {
	if s == "" {
		return pool.StatusNormal
	}
	return pool.Status(s)
}
