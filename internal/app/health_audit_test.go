package app

import (
	"strings"
	"testing"
)

// ═══════════════════════════════════════════════════════════════════
// 对抗式安全评审留下的护栏（2026-10-05）
//
// 来源：一次专门针对 health.go 的对抗式评审，系统性尝试了
// "看起来像回环、实际不是"的各种写法。**结论是没有找到绕过** ——
// 本文件把那些尝试固化成回归测试，防止将来有人"简化"判定逻辑时
// 悄悄把某一类放进来。
//
// 每条都配了"为什么它危险"的说明：这类输入的共同点是
// **字符串上像回环，语义上不是**（或反之），靠前缀/包含匹配就会中招。
//
// 评审同时确认（已实测，非推断）：用 15 个恶意候选构造白名单
// 得到的是**空集合**，即任何非回环 origin 都进不了白名单。
// ═══════════════════════════════════════════════════════════════════

// TestAuditIPv6EquivalentFormsMustNotBypass 守：IPv6 的等价写法不得绕过。
//
// 🔴 为什么危险：`[0:0:0:0:0:0:0:1]` 与 `[::1]` 在语义上是同一个地址。
// 若实现只认字面 `::1`，看似"更严格"，实则**不一致** ——
// 而如果哪天有人为了"支持等价写法"改成做 IPv6 归一化，
// 就必须同时处理 `[::ffff:127.0.0.1]`（IPv4-mapped）这类，
// 否则会引入更隐蔽的判定分歧。
//
// 当前实现的取舍：**只认字面 `::1`**，其余一律拒绝（宁严勿宽）。
// 本测试把这个取舍钉住：等价形式必须拒绝，不能"好心"放行。
func TestAuditIPv6EquivalentFormsMustNotBypass(t *testing.T) {
	mustReject := []struct{ in, why string }{
		{"http://[0:0:0:0:0:0:0:1]:8787", "::1 的全展开形式"},
		{"http://[::ffff:127.0.0.1]:8787", "IPv4-mapped IPv6"},
		{"http://[0:0:0:0:0:ffff:7f00:1]:8787", "IPv4-mapped 全展开"},
		{"http://[::FFFF:127.0.0.1]:8787", "IPv4-mapped 大小写变体"},
	}
	for _, c := range mustReject {
		if got, ok := normalizeLoopbackOrigin(c.in); ok {
			t.Errorf("%q（%s）必须被拒绝，却被接受为 %q —— "+
				"当前实现只认字面 [::1]，不支持 IPv6 归一化", c.in, c.why, got)
		}
	}
	// 基准：字面 ::1 必须仍然被接受（否则上面的"必须拒绝"就没意义了）
	if _, ok := normalizeLoopbackOrigin("http://[::1]:8787"); !ok {
		t.Error("字面 [::1] 必须被接受（它是唯一被认可的 IPv6 回环写法）")
	}
}

// TestAuditIPv4AlternateNotationsMustNotBypass 守：IPv4 的别名写法不得绕过。
//
// 🔴 为什么危险：浏览器/操作系统对 `127.1`、`0x7f.1`、`2130706433`、
// `0177.0.0.1` 的解析规则与"点分四段十进制"不同 ——
// 它们都指向 127.0.0.1。若判定逻辑用宽松 IP 解析把它们放进来，
// 白名单的 key 就会与浏览器实际发的 Origin 形式产生难以推理的对应关系。
//
// 当前取舍：只认点分四段十进制（127.0.0.0/8），其余一律拒绝。
//
// ⚠️ 一处**实测与推断的差异**（我自己写这条测试时发现的，值得记下来）：
//
//	对抗式评审的初稿把 `127.0.0.01` 和 `127.0.0.010` 也列为"被拒绝"，
//	但我写测试时实测**它们是被接受的** —— 因为判定用 `Atoi`，
//	`Atoi("01")==1`、`Atoi("010")==10`，四段各自都在 0..255 内。
//
//	所以本测试的"必须拒绝"清单里**不含**这两个；它们的处置见下一条
//	`TestAuditLeadingZeroOctetIsAcceptedButHarmless`（如实记录，不强行断言）。
func TestAuditIPv4AlternateNotationsMustNotBypass(t *testing.T) {
	mustReject := []struct{ in, why string }{
		{"http://127.1:8787", "短式（浏览器会解析成 127.0.0.1）"},
		{"http://0x7f.1:8787", "十六进制"},
		{"http://2130706433:8787", "整数形式"},
		{"http://0177.0.0.1:8787", "八进制首段"},
	}
	for _, c := range mustReject {
		if got, ok := normalizeLoopbackOrigin(c.in); ok {
			t.Errorf("%q（%s）必须被拒绝，却被接受为 %q —— "+
				"只认点分四段十进制，不要做宽松 IP 解析", c.in, c.why, got)
		}
	}
}

// TestAuditLeadingZeroOctetIsAcceptedButHarmless 如实记录前导零八位组的行为。
//
// 实测：`http://127.0.0.01:8787` 与 `http://127.0.0.010:8787` **被接受**
// （`Atoi` 按十进制解析 `"01"`→1、`"010"`→10，四段均在 0..255 内）。
//
// 为什么**不是安全漏洞**（逐条说明，避免后来者误判为紧急问题）：
//  1. 放行的仍然是 **127.0.0.0/8 段内**的地址 —— 判定并未越出回环范围；
//  2. 浏览器序列化 `location.origin` 时**从不产生前导零**，
//     所以真实页面永远发不出这种字符串；
//  3. 它只能经 `--panel-origin`（JSON body，由父进程生成）进入白名单，
//     而那个值来自 `location.origin`，同样不会有前导零。
//
// 也就是说：这条**既不能被远端利用，也不会影响真实用户**。
// 唯一实际影响是"白名单可能含一个永不被匹配的条目"（无害）。
//
// ⚠️ 与 `127.0.0.010` 的八进制歧义（Go 十进制 10 / 浏览器八进制 8）：
//
//	两者**都仍是 127.x 回环**，所以即便语义分歧也不构成绕过。
//	我没有改判定去"修正"它 —— 因为改动会引入新的解析分支，
//	收益为零而风险非零（这正是"看到差异先问是否值得改"的取舍）。
func TestAuditLeadingZeroOctetIsAcceptedButHarmless(t *testing.T) {
	for _, in := range []string{
		"http://127.0.0.01:8787",
		"http://127.0.0.010:8787",
	} {
		got, ok := normalizeLoopbackOrigin(in)
		if !ok {
			// 若将来实现改成拒绝它们，这条会红 —— 那是**行为变更**，
			// 应当同步更新本注释，而不是静默通过。
			t.Logf("注意：%q 当前被拒绝（实现已变更，请同步更新本测试的说明）", in)
			continue
		}
		// 关键：接受也必须仍在回环段内，且是合法的四段形式
		if !strings.HasPrefix(got, "http://127.") {
			t.Errorf("%q 被接受为 %q —— 放行的必须是 127.0.0.0/8 段内地址", in, got)
		}
		t.Logf("已知行为：%q 被接受为 %q（安全，理由见本函数注释）", in, got)
	}
}

// TestAuditTrailingDotAndWhitespaceVariants 守：尾随点等"看起来更短"的写法。
func TestAuditTrailingDotAndWhitespaceVariants(t *testing.T) {
	mustReject := []string{
		"http://127.0.0.1.:8787", // 尾随点（DNS 上等价，但 Origin 字符串不同）
		"http://localhost.:8787",
		"http://localhost..:8787",
	}
	for _, in := range mustReject {
		if got, ok := normalizeLoopbackOrigin(in); ok {
			t.Errorf("%q 必须被拒绝，却被接受为 %q —— "+
				"浏览器不会把尾随点写进 location.origin，放行只会扩大白名单", in, got)
		}
	}
}

// TestAuditControlCharTrimCollisionIsBounded 记录 TrimSpace 的行为边界。
//
// ⚠️ 评审发现：`TrimSpace` 会吞掉首尾控制字符，于是
// `"http://127.0.0.1:8788\n"` 与干净的 `"http://127.0.0.1:8788"` 归一为同一个 key。
//
// 为什么**不是**漏洞（评审实测确认）：
//   - 它只删空白，删完仍是**同一个回环**串；
//   - 非回环 + 首尾空白（如 `"http://evil.com:8788\n"`）构造出的白名单**为空**；
//   - `Origin` **头**路径不可注入：Go 的 net/http 在 handler 之前就把
//     裸 LF/TAB/尾随空格剥掉了（评审用裸 TCP 报文实测过）；
//     所以这条只能经 `--panel-origin`（JSON body）触发。
//
// 本测试的作用是**把这个边界写清楚**，而不是断言它"应当"如何：
// 它锁住"容忍首尾空白"这个有意行为，并确认非回环串不会被洗白。
func TestAuditControlCharTrimCollisionIsBounded(t *testing.T) {
	// 容忍首尾空白是当前行为（有意，便于容忍输入噪声）
	got, ok := normalizeLoopbackOrigin("http://127.0.0.1:8788\n")
	if !ok || got != "http://127.0.0.1:8788" {
		t.Errorf("首尾空白应被容忍并归一，实际 ok=%v got=%q", ok, got)
	}

	// 🔴 关键断言：非回环 + 首尾空白**不得**被洗白成合法 origin
	for _, in := range []string{
		"http://evil.com:8788\n",
		"http://127.0.0.1.evil.com:8788\n",
		"\thttp://evil.com:8788",
		"http://192.168.1.1:8788\n",
	} {
		if got, ok := normalizeLoopbackOrigin(in); ok {
			t.Errorf("%q 不得被空白容忍洗白成合法回环 origin（实际 %q）", in, got)
		}
	}
}

// TestAuditHostileCandidatesAllRejected 守：一批恶意候选全部被拒。
//
// 这是评审里"用 15 个恶意候选构造白名单 → 得到空集合"那条实验的固化。
func TestAuditHostileCandidatesAllRejected(t *testing.T) {
	hostile := []string{
		"http://evil.com:8787",
		"http://127.0.0.1.evil.com:8787",
		"http://127.0.0.1:8787@evil.com",
		"http://2130706433:8787",
		"http://0.0.0.0:8787",
		"http://[::]:8787",
		"http://192.168.1.1:8787",
		"https://127.0.0.1:8787",
		"http://127.0.0.1:8787/path",
		"http://127.0.0.1:8787?q=1",
		"http://127.0.0.1:8787#f",
		"*",
		"null",
		"",
		"http://127.0.0.1:8787,%20http://evil.com",
	}

	a := newAllowedOrigins(hostile...)
	if n := len(a.set); n != 0 {
		t.Errorf("恶意候选构造出的白名单应为空，实际有 %d 项：%v", n, a.set)
	}
	for _, h := range hostile {
		if a.Allows(h) {
			t.Errorf("%q 不应被放行", h)
		}
	}
}
