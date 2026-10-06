// cdp_test.go：凭据端点匹配与凭据解析的护栏测试。
//
// 这些测试钉住 Codex 第 28 轮的核心要求：
//
//	「不能用"URL 包含路径"作为匹配条件。用 net/url 解析后精确校验。」
//
// 为什么这条必须守：如果匹配放宽成子串，攻击面/误判面立刻扩大 ——
// 任何 host 上的同名路径、多个 state、甚至我们把 state 拼错都可能命中。
package login

import (
	"strings"
	"testing"
)

// TestIsCredentialEndpointAcceptsExactMatch 基准：完全正确的 URL 必须命中。
func TestIsCredentialEndpointAcceptsExactMatch(t *testing.T) {
	const st = "11111111-2222-4333-8444-555555555555"
	u := "https://www.codebuddy.cn/console/login/enterprise?state=" + st
	if !isCredentialEndpoint(u, st) {
		t.Fatalf("精确匹配的 URL 应命中: %s", u)
	}
}

// TestIsCredentialEndpointRejectsWrongState 守住 state 必须等于本次生成值。
//
// 这是"状态串扰"防护（Codex 风险清单第 3 条）：
// 上次登录的迟到响应、或别人的 state 都不得触发保存。
func TestIsCredentialEndpointRejectsWrongState(t *testing.T) {
	const mine = "11111111-2222-4333-8444-555555555555"
	const other = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	u := "https://www.codebuddy.cn/console/login/enterprise?state=" + other
	if isCredentialEndpoint(u, mine) {
		t.Fatal("state 不匹配时必须拒绝")
	}
}

// TestIsCredentialEndpointRejectsMultipleStates 守住「state 只能有一个值」。
func TestIsCredentialEndpointRejectsMultipleStates(t *testing.T) {
	const st = "11111111-2222-4333-8444-555555555555"
	u := "https://www.codebuddy.cn/console/login/enterprise?state=" + st + "&state=" + st
	if isCredentialEndpoint(u, st) {
		t.Fatal("state 出现多次时必须拒绝（防参数污染）")
	}
}

// TestIsCredentialEndpointRejectsMissingState 守住缺 state 的情况。
func TestIsCredentialEndpointRejectsMissingState(t *testing.T) {
	const st = "11111111-2222-4333-8444-555555555555"
	u := "https://www.codebuddy.cn/console/login/enterprise"
	if isCredentialEndpoint(u, st) {
		t.Fatal("缺少 state 时必须拒绝")
	}
}

// TestIsCredentialEndpointRejectsWrongHost 守住 host 必须完全相等。
//
// 这条特别重要：攻击者控制的域名若能构造同路径，
// "包含式"匹配会让我们把凭据交给错误的对端。
func TestIsCredentialEndpointRejectsWrongHost(t *testing.T) {
	const st = "11111111-2222-4333-8444-555555555555"
	bad := []string{
		"https://evil.example.com/console/login/enterprise?state=" + st,
		"https://www.codebuddy.cn.evil.com/console/login/enterprise?state=" + st,
		"https://evilcodebuddy.cn/console/login/enterprise?state=" + st,
		"https://www.codebuddy.com.cn/console/login/enterprise?state=" + st,
	}
	for _, u := range bad {
		if isCredentialEndpoint(u, st) {
			t.Fatalf("host 不符时必须拒绝: %s", u)
		}
	}
}

// TestIsCredentialEndpointRejectsWrongPath 守住 path 必须完全相等。
func TestIsCredentialEndpointRejectsWrongPath(t *testing.T) {
	const st = "11111111-2222-4333-8444-555555555555"
	bad := []string{
		"https://www.codebuddy.cn/console/login/enterprise/extra?state=" + st,
		"https://www.codebuddy.cn/console/login/enterpriseX?state=" + st,
		"https://www.codebuddy.cn/console/login/?state=" + st,
		"https://www.codebuddy.cn/console/account/switch?state=" + st,
	}
	for _, u := range bad {
		if isCredentialEndpoint(u, st) {
			t.Fatalf("path 不符时必须拒绝: %s", u)
		}
	}
}

// TestIsCredentialEndpointRejectsNonHTTPS 守住必须 https。
func TestIsCredentialEndpointRejectsNonHTTPS(t *testing.T) {
	const st = "11111111-2222-4333-8444-555555555555"
	bad := []string{
		"http://www.codebuddy.cn/console/login/enterprise?state=" + st,
		"ftp://www.codebuddy.cn/console/login/enterprise?state=" + st,
	}
	for _, u := range bad {
		if isCredentialEndpoint(u, st) {
			t.Fatalf("非 https 必须拒绝: %s", u)
		}
	}
}

// TestIsCredentialEndpointRejectsEmptyWantState 守住「空 state 不匹配任何东西」。
//
// 这条防的是"默认值恰好通过"这类错误：如果实现把空 wantState 当作通配，
// 那么调用方忘了传 state 时会静默接受任意响应 —— 正是我们最想避免的。
//
// ⚠️ 这里为什么必须用「state 参数存在但为空值」的 URL：
// 2026-10-05 逆向对照实验发现，用普通 URL（state=whatever）时，
// 末行 `vals[0] == wantState` 恰好也会拒绝，于是**去掉早返回保护测试仍然绿**
// —— 护栏没被真正钉住。换成 `?state=` 后，u.Query()["state"] == [""]，
// 末行比较会是 "" == "" → true，**只有**早返回能挡住它。
func TestIsCredentialEndpointRejectsEmptyWantState(t *testing.T) {
	cases := []string{
		// 形态一：state 参数存在但为空。
		"https://www.codebuddy.cn/console/login/enterprise?state=",
		"https://www.codebuddy.cn/console/login/enterprise?state=&traceparent=x",
		// 形态二：任意普通 URL 也不得被空 wantState 命中。
		"https://www.codebuddy.cn/console/login/enterprise?state=whatever",
	}
	for _, u := range cases {
		if isCredentialEndpoint(u, "") {
			t.Fatalf("空 wantState 不得匹配任何 URL: %s", u)
		}
	}
}

// TestIsCredentialEndpointRejectsMalformed 守住异常输入不 panic、不误判。
func TestIsCredentialEndpointRejectsMalformed(t *testing.T) {
	const st = "11111111-2222-4333-8444-555555555555"
	bad := []string{
		"",
		"://",
		"not a url",
		"https://",
		"https://www.codebuddy.cn/console/login/enterprise?state=%zz",
		"javascript:alert(1)",
		"data:text/html,<script></script>",
	}
	for _, u := range bad {
		if isCredentialEndpoint(u, st) {
			t.Fatalf("畸形 URL 必须拒绝: %q", u)
		}
	}
}

// TestIsCredentialEndpointAcceptsExtraQueryParams 确认额外查询参数不影响匹配。
//
// 实测里真实 URL 可能带 traceparent 之类的额外参数（登录页就会带）。
// 只要 state 唯一且正确，就应当命中。
func TestIsCredentialEndpointAcceptsExtraQueryParams(t *testing.T) {
	const st = "11111111-2222-4333-8444-555555555555"
	u := "https://www.codebuddy.cn/console/login/enterprise?state=" + st +
		"&traceparent=00-d39570dc975fcfa9f2749ed6ceba7793-cee65cd2452f30c2-01"
	if !isCredentialEndpoint(u, st) {
		t.Fatalf("带额外参数但 state 正确时应命中: %s", u)
	}
}

// ─────────────────────────────────────────────────────────────
// 凭据解析
// ─────────────────────────────────────────────────────────────
//
// ═══════════════════════════════════════════════════════════════════
// 🔴 结构在 2026-10-05 被**真实登录**更正过，测试随之重写
// ═══════════════════════════════════════════════════════════════════
//
// 原先这些测试用的是**我们假设的顶层结构**：
//
//	{"accessToken":"…","refreshToken":"…"}
//
// 而真实响应是**嵌套 + 业务信封**：
//
//	{"code":0,"msg":"OK","data":{"accessToken":"…","refreshToken":"…",…}}
//
// ⇒ 因为测试和实现用的是**同一个错假设**，这个错误从来没被测出来，
//   直到真实登录才暴露（"凭据响应缺少 accessToken"）。
//
// **教训**：契约必须记录完整层级；测试数据必须来自**实测样本**，
// 不能来自实现者的假设 —— 否则测试只是在验证自己的想象。

// realCredBody 是**实测到的真实响应结构**（值用假数据占位）。
func realCredBody(access, refresh string) string {
	return `{"code":0,"msg":"OK","requestId":"00000000-0000-4000-8000-000000000000",` +
		`"data":{"accessToken":"` + access + `","refreshToken":"` + refresh + `",` +
		`"expiresIn":3283200,"refreshExpiresIn":3456000,"tokenType":"Bearer"}}`
}

// TestExtractCredentialHappyPath 基准：用**实测的真实结构**。
func TestExtractCredentialHappyPath(t *testing.T) {
	body := []byte(realCredBody("aaa.bbb.ccc", "ddd.eee.fff"))
	p, err := extractCredential(body)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if p.accessToken() != "aaa.bbb.ccc" || p.refreshToken() != "ddd.eee.fff" {
		t.Fatal("字段值不符")
	}
	// 业务码与元信息也应解析出来（用于报告有效期）。
	if p.Code != 0 {
		t.Errorf("业务码应为 0，得到 %d", p.Code)
	}
	if p.Data.ExpiresIn == 0 {
		t.Error("expiresIn 未被解析 —— 结构可能又变了")
	}
}

// TestExtractCredentialRejectsTopLevelShape 是**回归断言**：
// 顶层结构（我们原先的错误假设）必须被拒绝。
//
// 为什么保留它：这条正是"曾经以为对、实际错"的形态。
// 若将来有人把实现改回读顶层，本测试会**立刻变红**。
func TestExtractCredentialRejectsTopLevelShape(t *testing.T) {
	wrong := []byte(`{"accessToken":"aaa.bbb.ccc","refreshToken":"ddd.eee.fff"}`)
	if _, err := extractCredential(wrong); err == nil {
		t.Fatal("❌ 顶层结构被接受了 —— 实现可能被改回了错误假设")
	}
}

// TestExtractCredentialRejectsBizError 守住"HTTP 200 不等于业务成功"。
//
// Codex 第 29 轮明确要求：拿到响应后必须校验**业务成功码**。
func TestExtractCredentialRejectsBizError(t *testing.T) {
	// 业务码非 0，但字段齐全 —— 必须拒绝，不能当凭据用。
	bad := []byte(`{"code":12151,"msg":"not choose login account",` +
		`"data":{"accessToken":"a","refreshToken":"b"}}`)
	p, err := extractCredential(bad)
	if err == nil {
		t.Fatal("❌ 业务码非 0 却被接受了 —— 会把错误响应当成凭据")
	}
	if p.Code != 12151 {
		t.Errorf("业务码应被解析出来，得到 %d", p.Code)
	}
	if !strings.Contains(err.Error(), "12151") {
		t.Errorf("错误信息应含业务码以便诊断，实际: %v", err)
	}
}

// TestExtractCredentialRejectsMissingFields 守住字段缺失必须报错。
func TestExtractCredentialRejectsMissingFields(t *testing.T) {
	cases := map[string]string{
		"两个都缺":      `{"code":0,"data":{}}`,
		"缺 refresh": `{"code":0,"data":{"accessToken":"a"}}`,
		"缺 access":  `{"code":0,"data":{"refreshToken":"b"}}`,
		"空字符串":      `{"code":0,"data":{"accessToken":"","refreshToken":""}}`,
		"只有空白":      `{"code":0,"data":{"accessToken":"   ","refreshToken":"  "}}`,
		"没有 data":   `{"code":0,"msg":"OK"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := extractCredential([]byte(body)); err == nil {
				t.Fatal("应报错")
			}
		})
	}
}

// TestExtractCredentialErrorCarriesShape 断言错误里带**结构摘要**。
//
// 理由（真实教训）：2026-10-05 真实登录时只报"缺少 accessToken"，
// 完全不知道真实结构长什么样，只能再加诊断重跑一次。
// 带上结构摘要后，一次失败就能定位。
func TestExtractCredentialErrorCarriesShape(t *testing.T) {
	// 结构不同（凭据在别的字段名里）—— 错误应指出实际结构。
	body := []byte(`{"code":0,"msg":"OK","data":{"token":"x","refresh":"y"}}`)
	_, err := extractCredential(body)
	if err == nil {
		t.Fatal("应报错")
	}
	if !strings.Contains(err.Error(), "实际结构") {
		t.Errorf("错误信息应带结构摘要，实际: %v", err)
	}
	if !strings.Contains(err.Error(), "data") {
		t.Errorf("结构摘要应体现嵌套层级，实际: %v", err)
	}
}

// TestExtractCredentialRejectsMalformedJSON 守住非法 JSON 报错而非静默通过。
func TestExtractCredentialRejectsMalformedJSON(t *testing.T) {
	for _, body := range []string{"", "not json", "{", `[1,2,3]`, `"string"`} {
		if _, err := extractCredential([]byte(body)); err == nil {
			t.Fatalf("非法 JSON 应报错: %q", body)
		}
	}
}

// TestExtractCredentialRejectsWrongTypes 守住**类型不符**必须报错。
//
// ═══════════════════════════════════════════════════════════════════
// 为什么补这条（Codex 第 33 轮要求）
// ═══════════════════════════════════════════════════════════════════
//
// Codex 原话：
//
//	「实测样本应成为脱敏后的契约测试依据，**再补缺字段、错误类型和
//	  失败响应**，避免测试只覆盖成功样本。」
//
// 而且**本项目已有一个真实先例**：`response.fromDiskCache` 声明成
// `string` 而 CDP 下发 `bool`，导致事件解析全部失败（缺陷 C）。
// 类型不符是**真实发生过**的失败模式，必须有断言守着。
func TestExtractCredentialRejectsWrongTypes(t *testing.T) {
	cases := map[string]string{
		// data 是数组而不是对象
		"data 是数组": `{"code":0,"data":[{"accessToken":"a","refreshToken":"b"}]}`,
		// token 是数字而不是字符串
		"token 是数字": `{"code":0,"data":{"accessToken":123,"refreshToken":456}}`,
		// token 是对象
		"token 是对象": `{"code":0,"data":{"accessToken":{},"refreshToken":{}}}`,
		// code 是字符串而不是数字
		"code 是字符串": `{"code":"0","data":{"accessToken":"a","refreshToken":"b"}}`,
		// data 是 null
		"data 是 null": `{"code":0,"data":null}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := extractCredential([]byte(body)); err == nil {
				t.Fatal("❌ 类型不符却被接受了 —— 契约类型假设错了会静默出错")
			}
		})
	}
}

// TestExtractCredentialRejectsUnexpectedEnvelope 守住"信封形态不符"。
//
// 真实响应是 `{code,msg,requestId,data{…}}`。若上游改成别的形态
// （例如少了 data 层、或凭据直接在顶层），必须**明确失败**而不是
// 悄悄返回空凭据。
func TestExtractCredentialRejectsUnexpectedEnvelope(t *testing.T) {
	cases := map[string]string{
		"凭据在顶层（旧错假设）":    `{"accessToken":"a","refreshToken":"b"}`,
		"凭据在更深的层级":       `{"code":0,"data":{"result":{"accessToken":"a","refreshToken":"b"}}}`,
		"只有 msg 没有 data": `{"code":0,"msg":"OK"}`,
		"空对象":            `{}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := extractCredential([]byte(body)); err == nil {
				t.Fatal("❌ 信封形态不符却被接受了")
			}
		})
	}
}

// TestExtractCredentialErrorDoesNotLeakToken 守住错误信息不含 token。
//
// 这是安全红线：token 不进日志/事件/API 响应。错误信息是常见泄露通道。
// ⚠️ 现在错误里会带**结构摘要**，所以这条断言更重要 ——
// 结构摘要只能有字段名与长度，绝不能出现值。
func TestExtractCredentialErrorDoesNotLeakToken(t *testing.T) {
	secret := "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.SUPERSECRETPAYLOAD.signature"
	// 只有 accessToken 有值，refreshToken 缺失 → 触发错误。
	body := []byte(`{"code":0,"data":{"accessToken":"` + secret + `"}}`)
	_, err := extractCredential(body)
	if err == nil {
		t.Fatal("应报错")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("错误信息泄露了 token: %v", err)
	}
	if strings.Contains(err.Error(), "SUPERSECRETPAYLOAD") {
		t.Fatalf("错误信息泄露了 token 片段: %v", err)
	}
}

// TestCDPErrorSanitized 守住 CDP 错误文本被清洗与截断。
func TestCDPErrorSanitized(t *testing.T) {
	long := strings.Repeat("x", 5000)
	e := &cdpError{Code: -32000, Message: long + "\n" + "line2"}
	s := e.Error()
	if len(s) > 400 {
		t.Fatalf("错误信息未被截断，长度 %d", len(s))
	}
	if strings.Contains(s, "\n") {
		t.Fatal("错误信息不应含换行（日志注入）")
	}
	if !strings.Contains(s, "-32000") {
		t.Fatal("应保留错误码")
	}
}

// TestSanitizeCDPTextStripsControlChars 守住控制字符被去掉。
func TestSanitizeCDPTextStripsControlChars(t *testing.T) {
	in := "a\x00b\x07c\x1bd"
	out := sanitizeCDPText(in)
	for _, r := range out {
		if r < 0x20 || r == 0x7f {
			t.Fatalf("仍含控制字符: %q", out)
		}
	}
}
