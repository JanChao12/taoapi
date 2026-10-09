package workbuddy

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
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

	// RateLimitResetAt 上游在 429 响应里自述的**重置时间**（已转成绝对时刻）。
	//
	// 🔴 只对 429 且解析成功时非零。
	//
	// 实测形状（2026-10-09，两处措辞并存，且**都在中文/英文散文 msg 里**，
	// 没有独立的结构化字段）：
	//
	//	{"code":6004,"msg":"您的使用量已超出频率限制，将在 2026-10-09
	//	 21:24:55 UTC+8 重置，您也可以切换其他模型继续使用。"}
	//	{"code":6004,"msg":"usage exceeds frequency limit, but don't worry,
	//	 your usage will reset at 2026-10-10 00:08:52 UTC+8, alternatively,
	//	 you can switch to the other models to continue using it."}
	//
	// ⚠️ 实测这个时间**只对被限流的那一个模型成立**（委托方实测：
	//	"我实测之前使用DeepSeek过多导致限制，我切换glm后能正常使用"），
	//	所以它必须配**按模型**的冷却使用，不能当账号级冷却。
	RateLimitResetAt time.Time
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

// RateLimitReset 报告上游自述的重置时刻。
//
// 实现**独立的**可选接口而不是塞进 IsRateLimited 那个接口：
// 调用方（app 层）用 errors.As 单独探测它，解析不到就退回默认冷却 ——
// 这样"上游换了措辞"只会降级成"用默认值"，不会让请求直接失败。
func (e *UpstreamError) RateLimitReset() (time.Time, bool) {
	if e.RateLimitResetAt.IsZero() {
		return time.Time{}, false
	}
	return e.RateLimitResetAt, true
}

// reRateLimitReset 从上游的**散文 msg** 里抓「日期 时间 UTC±N」。
//
// ⚠️ 刻意**只匹配时间本身**，不依赖任何语言措辞：
//
//	上游有两种语言版本（中文/英文），措辞随时可能再变，
//	但"2026-10-09 21:24:55 UTC+8"这个形状是机器生成的、稳定的。
//	按措辞匹配（如找"将在"或"reset at"）会在改文案时静默失效。
//
// 实测可匹配的边界（2026-10-09）：
//
//	"2026-10-09 21:24:55 UTC+8"  ✓
//	"2026-10-09 21:24:55 UTC+08" ✓（偏移补零）
//	"2026-10-09T21:24:55+08:00"  ✗（ISO 形式——上游**没有**用过，
//	                               真出现了会解析失败并退回默认冷却）
var reRateLimitReset = regexp.MustCompile(
	`(\d{4}-\d{2}-\d{2})\s+(\d{2}:\d{2}:\d{2})\s*UTC([+-]\d{1,2})`)

// ParseRateLimitReset 从 429 响应体里解析上游自述的重置时刻。
//
// 🔴 必须在**截断之前**用原始字节调用。
//
//	`Body` 字段走 snippet() 按**字节**截断到 300；实测中文响应体 195 字节、
//	英文 240 字节 —— 已经很接近上限了。上游只要把英文措辞写长一点，
//	时间戳就会被截掉，解析**静默失败**并退回默认冷却。
//	（这与第 60 轮那批"测试全绿时藏着"的缺陷是同一类，故在此显式说明。）
//
// 解析失败返回零值 + false；调用方**必须**有默认值兜底，不得当成错误。
func ParseRateLimitReset(body []byte) (time.Time, bool) {
	m := reRateLimitReset.FindSubmatch(body)
	if m == nil {
		return time.Time{}, false
	}
	// 上游给的是 UTC+8 墙钟，转成绝对时刻（time.Time 自带位置无关性）
	off, err := strconv.Atoi(string(m[3]))
	if err != nil {
		return time.Time{}, false
	}
	// 秒级解析；时间串里没有时区后缀，用固定偏移构造
	loc := time.FixedZone("UTC"+string(m[3]), off*3600)
	t, err := time.ParseInLocation("2006-01-02 15:04:05",
		string(m[1])+" "+string(m[2]), loc)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// ErrCheckedInAlready 表示今日已签到。
//
// ⚠️ 上游对「已签到」返回 HTTP 400 + 空 body（实测）。
// 这是成功幂等结果，不是错误，不得计入错误冷却。
var ErrCheckedInAlready = fmt.Errorf("今日已签到")
