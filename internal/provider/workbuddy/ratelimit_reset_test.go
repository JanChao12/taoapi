package workbuddy

import (
	"os"
	"strings"
	"testing"
	"time"
)

// TestParseRateLimitResetFromRealBodies 守：从**真实抓到的**上游 429 响应体里
// 解析出重置时刻。
//
// ═══════════════════════════════════════════════════════════════════
// 为什么必须用真实字符串（不是编造的样例）
// ═══════════════════════════════════════════════════════════════════
//
//	下面两条 msg 逐字取自 2026-10-09 的服务日志
//	（`D:\tools\TAOAPI\data\logs\wbapi-2026-10-09.log`，共 150 条 429）：
//
//	  · 中文版 111 条
//	  · 英文版   1 条（同一 code=6004，措辞完全不同）
//
//	上游**没有**结构化的 reset 字段，时间只散在散文 msg 里。
//	用编造的样例测会漏掉"措辞有两种语言"这个真实约束。
//
// ⚠️ 关键设计：正则**只匹配时间本身**（`日期 时间 UTC±N`），
//
//	不依赖任何语言措辞 —— 否则上游改文案时会**静默**失效
//	（退回兜底冷却，不报错）。
func TestParseRateLimitResetFromRealBodies(t *testing.T) {
	cases := []struct {
		name string
		body string
		want time.Time // 期望的**绝对时刻**（UTC）
	}{
		{
			// 逐字取自日志（中文，111 条均为此形状，仅时间戳不同）
			name: "中文版真实响应",
			body: `{"code":6004,"msg":"您的使用量已超出频率限制，将在 2026-10-09 21:24:55 UTC+8 重置，您也可以切换其他模型继续使用。","requestId":"96fa9edc1290e69ab442fe9811d7af75"}`,
			// 21:24:55 UTC+8 == 13:24:55 UTC
			want: time.Date(2026, 10, 9, 13, 24, 55, 0, time.UTC),
		},
		{
			// 逐字取自日志（英文，与中文同 code，措辞完全不同）
			name: "英文版真实响应",
			body: `{"code":6004,"msg":"usage exceeds frequency limit, but don't worry, your usage will reset at 2026-10-10 00:08:52 UTC+8, alternatively, you can switch to the other models to continue using it.","requestId":"5459874f3c12d702aaf38a9dad038db5"}`,
			// 次日 00:08:52 UTC+8 == 前一日 16:08:52 UTC
			want: time.Date(2026, 10, 9, 16, 8, 52, 0, time.UTC),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseRateLimitReset([]byte(tc.body))
			if !ok {
				t.Fatalf("解析失败 —— 上游换了措辞或正则过窄。body=%s", tc.body)
			}
			if !got.Equal(tc.want) {
				t.Errorf("解析结果 = %v，期望 %v（时区换算错误？）",
					got.UTC(), tc.want)
			}
		})
	}
}

// TestParseRateLimitResetOffsetZeroPadding 守：偏移补零（UTC+08）也能解析。
//
// 上游实测用 UTC+8，但 "+08" 是同一含义的常见写法 —— 不该因为补零就失配。
func TestParseRateLimitResetOffsetZeroPadding(t *testing.T) {
	body := `{"code":6004,"msg":"将在 2026-10-09 21:24:55 UTC+08 重置"}`
	got, ok := ParseRateLimitReset([]byte(body))
	if !ok {
		t.Fatal("UTC+08 应能解析（与 UTC+8 同义）")
	}
	want := time.Date(2026, 10, 9, 13, 24, 55, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("解析 = %v，期望 %v", got.UTC(), want)
	}
}

// TestParseRateLimitResetFailsClosed 守：解析不到时**如实返回 false**，
// 不猜、不返回零值时间冒充成功。
//
// 调用方据此退回兜底冷却（10 分钟）—— 所以"解析失败"必须是**可检测**的。
func TestParseRateLimitResetFailsClosed(t *testing.T) {
	for _, body := range []string{
		"",                           // 空响应体
		`{"code":6004,"msg":"频率限制"}`, // 没有时间
		`{"code":6004,"msg":"将在 稍后 重置"}`,                           // 措辞在但没有时间
		`{"code":6004,"msg":"reset at 2026-10-09T21:24:55+08:00"}`, // ISO 形式（上游未用过）
		`{"code":6004,"msg":"将在 2026-10-09 重置"}`,                   // 只有日期没有时刻
	} {
		if got, ok := ParseRateLimitReset([]byte(body)); ok {
			t.Errorf("解析 %q 应失败，却返回了 %v —— 不得猜测", body, got)
		}
	}
}

// TestParseRateLimitResetSurvivesBeyondSnippetLimit 守：时间戳位于
// snippet 截断点（300 字节）**之后**时仍能解析。
//
// ═══════════════════════════════════════════════════════════════════
// 这是最容易埋雷的一处（2026-10-09 实测数据）
// ═══════════════════════════════════════════════════════════════════
//
//	`UpstreamError.Body` 走 snippet() 按**字节**截断到 300：
//	  · 中文响应体实测 195 字节（安全）
//	  · 英文响应体实测 240 字节（已贴近上限）
//
//	上游只要把英文措辞写长一点，时间戳就会落到 300 字节之外。
//	⇒ 解析**必须**在截断之前用原始字节做（chat.go 就是这么接的）。
//	本测试证明 ParseRateLimitReset 本身能处理超过 300 字节的输入；
//	若有人把它改成"从 ue.Body 里解析"，这条不会红，但会静默退化成兜底 ——
//	所以另有一条源码断言守在 chat.go 侧（见下）。
func TestParseRateLimitResetSurvivesBeyondSnippetLimit(t *testing.T) {
	// 构造：前面垫 400 字节的英文长句，时间戳在其后
	long := strings.Repeat("x", 400)
	body := `{"code":6004,"msg":"` + long +
		` reset at 2026-10-11 08:00:00 UTC+8, alternatively, switch models."}`

	if len(body) <= 300 {
		t.Fatalf("测试构造无效：body 只有 %d 字节，没有超过 300", len(body))
	}
	got, ok := ParseRateLimitReset([]byte(body))
	if !ok {
		t.Fatal("300 字节之后的时间戳应能解析（解析发生在截断之前）")
	}
	want := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC) // 08:00 UTC+8
	if !got.Equal(want) {
		t.Errorf("解析 = %v，期望 %v", got.UTC(), want)
	}
}

// TestRateLimitResetAccessibleViaInterface 守：上层能通过**可选接口**
// 取到重置时刻（app 层用 errors.As 探测，不依赖具体渠道包）。
func TestRateLimitResetAccessibleViaInterface(t *testing.T) {
	ue := &UpstreamError{
		StatusCode:       429,
		Op:               "chat",
		RateLimitResetAt: time.Date(2026, 10, 9, 13, 24, 55, 0, time.UTC),
	}
	var r interface{ RateLimitReset() (time.Time, bool) }
	if !asInterface(ue, &r) {
		t.Fatal("UpstreamError 必须实现 RateLimitReset() 供 app 层探测")
	}
	got, ok := r.RateLimitReset()
	if !ok {
		t.Fatal("RateLimitReset() 应返回 true（字段非零）")
	}
	if !got.Equal(ue.RateLimitResetAt) {
		t.Errorf("返回 %v，期望 %v", got, ue.RateLimitResetAt)
	}

	// 零值 ⇒ false（调用方据此退回兜底冷却）
	empty := &UpstreamError{StatusCode: 429}
	if _, ok := empty.RateLimitReset(); ok {
		t.Error("未解析到时间时应返回 false，不能返回零值时间冒充成功")
	}
}

// asInterface 是 errors.As 的最小替身，避免本包测试引入额外依赖。
func asInterface(err error, target any) bool {
	type resetter interface{ RateLimitReset() (time.Time, bool) }
	if r, ok := err.(resetter); ok {
		if p, ok := target.(*interface{ RateLimitReset() (time.Time, bool) }); ok {
			*p = r
			return true
		}
	}
	return false
}

// TestChatParsesResetBeforeTruncation 守：chat.go 在**截断之前**解析重置时间。
//
// 为什么需要源码断言（而不是行为测试）：
//
//	行为上无法区分"从原始字节解析"与"从截断后的字符串解析" ——
//	两种写法在响应体短时结果相同。只有当上游措辞变长时才分叉，
//	而那时是**静默**退化成兜底（不报错）。所以只能审接线点。
//
// 反向对照：把 chat.go 里的 `ParseRateLimitReset(snippetBuf)` 改成
// `ParseRateLimitReset([]byte(errObj.Body))`，本条立刻红。
func TestChatParsesResetBeforeTruncation(t *testing.T) {
	src, err := os.ReadFile("chat.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "ParseRateLimitReset(snippetBuf)") {
		t.Error("chat.go 必须用**原始读取的字节**（snippetBuf）解析重置时间 ——\n" +
			"  若改成从 errObj.Body（已按 300 字节截断）解析，上游把措辞写长一点\n" +
			"  就会静默丢掉时间戳并退化成兜底冷却（英文响应体实测已 240 字节，很接近）。")
	}
	if strings.Contains(string(src), "ParseRateLimitReset([]byte(errObj.Body))") {
		t.Error("检测到从**截断后**的 Body 解析重置时间 —— 会静默失效")
	}
}
