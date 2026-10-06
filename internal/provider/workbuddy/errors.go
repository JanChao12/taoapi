package workbuddy

import (
	"fmt"
	"strings"
)

// UpstreamError 表示上游返回了非成功结果。
//
// 区分「HTTP 状态码错误」与「业务 code 非 0」两种情况，
// 因为上游两种都会用（如签到已签到是 HTTP 400 + 空 body）。
type UpstreamError struct {
	// StatusCode HTTP 状态码。
	StatusCode int

	// BizCode 业务 code（响应体里的 code 字段）；未解析到时为 0。
	BizCode int

	// BizMsg 业务消息。
	BizMsg string

	// Body 响应体片段（已截断，不含凭据）。
	Body string

	// Op 操作名（models/chat/credit/checkin），用于诊断。
	Op string
}

func (e *UpstreamError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "上游 %s 失败: HTTP %d", e.Op, e.StatusCode)
	if e.BizCode != 0 {
		fmt.Fprintf(&b, ", 业务 code=%d", e.BizCode)
	}
	if e.BizMsg != "" {
		fmt.Fprintf(&b, ", msg=%q", e.BizMsg)
	}
	if e.Body != "" {
		fmt.Fprintf(&b, ", body=%s", e.Body)
	}
	return b.String()
}

// IsAuthFailure 判断是否为凭证失效（需要重新授权）。
//
// 401/403 视为凭证问题；上游也可能用业务 code 表达，这里只处理 HTTP 层。
func (e *UpstreamError) IsAuthFailure() bool {
	return e.StatusCode == 401 || e.StatusCode == 403
}

// IsRateLimited 判断是否为限流。
func (e *UpstreamError) IsRateLimited() bool {
	return e.StatusCode == 429
}

// ErrCheckedInAlready 表示今日已签到。
//
// ⚠️ 上游对「已签到」返回 HTTP 400 + 空 body（实测）。
// 这是成功幂等结果，不是错误，不得计入错误冷却。
var ErrCheckedInAlready = fmt.Errorf("今日已签到")
