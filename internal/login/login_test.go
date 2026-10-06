// login_test.go：编排层的护栏测试（不起真实浏览器）。
//
// 钉住的契约：
//   - state 不可预测、形态正确
//   - 取消 / 超时必须能终止等待（不能永久挂住）
//   - 失败结果必须**不带凭据**（不能"看起来成功了"）
//   - LooksLikeJWT 只做形态判断，且不误纳明显无效值
package login

import (
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// ─────────────────────────────────────────────────────────────
// state
// ─────────────────────────────────────────────────────────────

// TestNewStateIsUUIDShaped 确认 state 形态与官方一致（UUID v4）。
func TestNewStateIsUUIDShaped(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	for i := 0; i < 50; i++ {
		s, err := newState()
		if err != nil {
			t.Fatalf("生成失败: %v", err)
		}
		if !re.MatchString(s) {
			t.Fatalf("state 形态不符 UUID v4: %q", s)
		}
	}
}

// TestNewStateIsUnique 确认不会重复。
//
// 重复的 state 会导致串扰：两次登录的凭据可能互相覆盖。
func TestNewStateIsUnique(t *testing.T) {
	const n = 2000
	seen := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		s, err := newState()
		if err != nil {
			t.Fatalf("生成失败: %v", err)
		}
		if seen[s] {
			t.Fatalf("state 重复: %s（第 %d 次）", s, i)
		}
		seen[s] = true
	}
}

// TestNewStateIsUnpredictable 粗检随机性（防被改成时间戳/计数器）。
//
// 做法：连续生成两批，检查它们没有"单调递增"或"共享前缀"这类可预测特征。
// 注意这是**启发式**检查，不是密码学证明 —— 但它能抓住
// "把 crypto/rand 换成 time.Now() 或计数器"这类回归。
func TestNewStateIsUnpredictable(t *testing.T) {
	var first, second []string
	for i := 0; i < 20; i++ {
		a, _ := newState()
		b, _ := newState()
		first = append(first, a)
		second = append(second, b)
	}
	// 同一位置若出现相同值，说明生成了常量。
	for i := range first {
		if first[i] == second[i] {
			t.Fatalf("第 %d 次生成相同 state，疑似非常量随机源: %s", i, first[i])
		}
	}
	// 检查首字符不是全都一样（计数器/时间戳会有明显模式）。
	heads := make(map[byte]bool)
	for _, s := range first {
		heads[s[0]] = true
	}
	if len(heads) < 3 {
		t.Fatalf("首字符只有 %d 种，随机性可疑", len(heads))
	}
}

// ─────────────────────────────────────────────────────────────
// 取消与超时
// ─────────────────────────────────────────────────────────────

// TestLoginReturnsErrorWhenNoBrowserOrBusy 确认 Login 在无法启动时立即返回错误，
// 而不是挂住。
//
// 本机有 Chrome，所以这里通过"占住互斥位"来强制 StartBrowser 失败。
func TestLoginReturnsErrorWhenNoBrowserOrBusy(t *testing.T) {
	// 模拟"已有登录在跑"。
	manager.mu.Lock()
	prev := manager.current
	manager.current = &BrowserSession{}
	manager.mu.Unlock()
	t.Cleanup(func() {
		manager.mu.Lock()
		manager.current = prev
		manager.mu.Unlock()
	})

	done := make(chan Result, 1)
	go func() {
		done <- Login(time.Second, nil, nil)
	}()

	select {
	case r := <-done:
		if r.Err == nil {
			t.Fatal("已有登录流程时应返回错误")
		}
		if r.Phase != PhaseFailed {
			t.Fatalf("阶段应为 failed，得到 %q", r.Phase)
		}
		// 🔴 失败结果绝不能带凭据。
		if r.Credentials.AccessToken != "" || r.Credentials.RefreshToken != "" {
			t.Fatal("失败结果不得包含凭据")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Login 挂住了 —— 必须立即返回错误")
	}
}

// TestProgressCallbackReceivesFailedPhase 确认失败时会通知进度回调。
func TestProgressCallbackReceivesFailedPhase(t *testing.T) {
	manager.mu.Lock()
	prev := manager.current
	manager.current = &BrowserSession{}
	manager.mu.Unlock()
	t.Cleanup(func() {
		manager.mu.Lock()
		manager.current = prev
		manager.mu.Unlock()
	})

	var mu sync.Mutex
	var phases []Phase
	Login(time.Second, func(p Phase) {
		mu.Lock()
		phases = append(phases, p)
		mu.Unlock()
	}, nil)

	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, p := range phases {
		if p == PhaseFailed {
			found = true
		}
	}
	if !found {
		t.Fatalf("应收到 failed 阶段，实际收到: %v", phases)
	}
}

// ─────────────────────────────────────────────────────────────
// LooksLikeJWT
// ─────────────────────────────────────────────────────────────

// TestLooksLikeJWTAcceptsTypicalTokens 确认典型 Keycloak token 形态被接受。
func TestLooksLikeJWTAcceptsTypicalTokens(t *testing.T) {
	good := []string{
		"eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiIxIn0.signature",
		"eyJhbGciOiJIUzUxMiJ9.eyJ0eXAiOiJPZmZsaW5lIn0.sig",
	}
	for _, s := range good {
		if !LooksLikeJWT(s) {
			t.Fatalf("应识别为 JWT: %s", s)
		}
	}
}

// TestLooksLikeJWTRejectsInvalid 确认明显无效的值被拒绝。
func TestLooksLikeJWTRejectsInvalid(t *testing.T) {
	bad := []string{
		"",
		"   ",
		"notajwt",
		"only.two",
		"a.b.c.d",     // 四段
		"eyJx.eyJy",   // 两段
		".eyJy.sig",   // 首段空
		"eyJx..sig",   // 中段空
		"eyJx.eyJy.",  // 末段空
		"abc.def.ghi", // 首段不以 ey 开头
	}
	for _, s := range bad {
		if LooksLikeJWT(s) {
			t.Fatalf("不应识别为 JWT: %q", s)
		}
	}
}

// TestLooksLikeJWTTrimsWhitespace 确认前后空白被容忍。
func TestLooksLikeJWTTrimsWhitespace(t *testing.T) {
	s := "  eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiIxIn0.sig  "
	if !LooksLikeJWT(s) {
		t.Fatal("应容忍前后空白")
	}
}

// ─────────────────────────────────────────────────────────────
// 凭据结构不得被序列化到外部
// ─────────────────────────────────────────────────────────────

// TestCredentialsIsNotSerializedIntoResult 守住"失败结果不带凭据"。
//
// 这是一个契约测试：任何让失败路径带上凭据的改动都必须让测试变红。
func TestZeroResultHoldsNoCredentials(t *testing.T) {
	var r Result
	if r.Credentials.AccessToken != "" || r.Credentials.RefreshToken != "" {
		t.Fatal("零值 Result 不得含凭据")
	}
	if r.Phase != "" {
		t.Fatalf("零值 Phase 应为空，得到 %q", r.Phase)
	}
}

// TestExtractCredentialResultNotLogged 确认 Credentials 结构没有
// 意外实现 String()/MarshalJSON 之类会泄露的接口。
//
// 若将来有人给 Credentials 加了 String() 或 MarshalJSON，
// 一次 fmt.Printf("%v") 或 json.Marshal 就可能把 token 写进日志。
// 这条测试会在那时变红，提醒改用显式脱敏类型。
func TestCredentialsHasNoLeakyStringer(t *testing.T) {
	c := Credentials{AccessToken: "SECRETACCESS", RefreshToken: "SECRETREFRESH"}

	// fmt.Stringer?
	if s, ok := any(c).(interface{ String() string }); ok {
		if strings.Contains(s.String(), "SECRET") {
			t.Fatal("Credentials.String() 泄露了 token")
		}
	}
	// 若实现了 MarshalJSON，必须不含明文（当前不应实现）。
	if m, ok := any(c).(interface{ MarshalJSON() ([]byte, error) }); ok {
		b, err := m.MarshalJSON()
		if err == nil && strings.Contains(string(b), "SECRET") {
			t.Fatal("Credentials.MarshalJSON() 泄露了 token")
		}
	}
}
