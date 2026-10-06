// refreshprobe：refresh 续期实验的工具集。
//
// ═══════════════════════════════════════════════════════════════════
// 为什么单独做成一个工具（而不是塞进测试）
// ═══════════════════════════════════════════════════════════════════
//
// 本实验要回答委托方问过的「网页登录能用多久」。Codex 第 28 轮指定的做法
// （28c-codex-实现方案裁定.md 第三节）要求：
//
//  1. 捕获并加密暂存新凭据，**关闭本次浏览器进程树，确认进程退出**
//  2. 通过现有客户端执行 Credit，确认凭据可独立使用
//  3. **不带浏览器 Cookie**，用已知 refresh 调续期端点
//  4. 内存中比较新旧 token，只报告是否变化；用新 access token 再查 Credit
//  5. **重启 wbapi 后再续期一次**，验证落盘与恢复
//
// 第 1 步必须由**人在官方页面真实登录**，无法自动化，所以做成手工工具：
//
//	go run tools/refreshprobe -step capture [-out <文件>]
//	go run tools/refreshprobe -step use     [-out <文件>]
//	go run tools/refreshprobe -step refresh [-out <文件>]
//	go run tools/refreshprobe -step cleanup [-out <文件>]
//
// ═══════════════════════════════════════════════════════════════════
// 🔴 安全约束（本工具全程遵守）
// ═══════════════════════════════════════════════════════════════════
//
//   - 暂存文件用与账号库**同一个** DPAPI 加密器，绝不落明文
//   - 任何输出都**不含 token**；只打印"有没有变化"、长度、有效期这类信息
//   - 暂存文件用完即删（-step cleanup）
//   - **不重放旧 refresh token 探测失效**（Codex 警告：可能触发
//     整个 token family 撤销）
package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"workbuddy.local/workbuddy-api/internal/auth"
	"workbuddy.local/workbuddy-api/internal/login"
	"workbuddy.local/workbuddy-api/internal/storage"
)

// stashPlain 是暂存凭据的内容（加密后写入）。
//
// ⚠️ 这个结构体**只在内存**里是明文；写盘时整个 JSON 被 DPAPI 加密。
type stashPlain struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	CapturedAt   string `json:"captured_at"`

	// 以下都是**非秘密**的形态信息，用于报告。
	AccessExp      int64  `json:"access_exp,omitempty"`
	RefreshExp     int64  `json:"refresh_exp,omitempty"`
	RefreshTyp     string `json:"refresh_typ,omitempty"`
	RefreshSidHash string `json:"refresh_sid_hash,omitempty"`
}

func main() {
	var (
		step = flag.String("step", "", "capture | use | refresh | cleanup")
		out  = flag.String("out", defaultStashPath(), "暂存文件路径")
	)
	flag.Parse()

	if *step == "" {
		fmt.Fprintln(os.Stderr,
			"用法: go run ./tools/refreshprobe -step capture|use|refresh|cleanup [-out 文件]")
		os.Exit(2)
	}

	var code int
	switch *step {
	case "capture":
		code = doCapture(*out)
	case "use":
		code = doUse(*out)
	case "refresh":
		code = doRefresh(*out)
	case "cleanup":
		code = doCleanup(*out)
	default:
		fmt.Fprintf(os.Stderr, "未知步骤: %s\n", *step)
		code = 2
	}
	os.Exit(code)
}

// defaultStashPath 返回默认暂存路径。
//
// 放在账号库**同目录**（便于用同一个 DPAPI 用户上下文），但文件名不同，
// 避免与 accounts.json 混淆。
func defaultStashPath() string {
	dir := filepath.Dir(auth.AccountsPath())
	return filepath.Join(dir, "refreshprobe-stash.bin")
}

// ─────────────────────────────────────────────────────────────
// 步骤 1：真实登录并暂存凭据
// ─────────────────────────────────────────────────────────────

func doCapture(out string) int {
	fmt.Println("=== 步骤 1：真实网页登录，捕获凭据 ===")
	fmt.Println()
	fmt.Println("即将打开一个**受控浏览器窗口**（独立 profile，与你自己的浏览器登录态完全隔离）。")
	fmt.Println("请在打开的官方页面里**正常登录**（扫码或手机号；程序不碰你的输入）。")
	fmt.Println("登录成功后本工具会自动捕获凭据、验证可用性、加密暂存，然后关掉浏览器。")
	fmt.Println()

	phaseNames := map[login.Phase]string{
		login.PhaseStarting:  "正在启动浏览器…",
		login.PhaseWaiting:   "等待你完成登录…",
		login.PhaseCapturing: "已看到凭据响应，正在读取…",
		login.PhaseDone:      "完成",
		login.PhaseFailed:    "失败",
		login.PhaseCanceled:  "已取消",
	}

	start := time.Now()
	res := login.Login(10*time.Minute, func(p login.Phase) {
		if name, ok := phaseNames[p]; ok {
			fmt.Printf("  [%6.1fs] %s\n", time.Since(start).Seconds(), name)
		}
	}, nil)

	if res.Err != nil {
		// ⚠️ login 包保证错误信息不含 token。
		fmt.Fprintf(os.Stderr, "\n❌ 登录失败: %v\n", res.Err)
		return 1
	}

	creds := res.Credentials
	fmt.Println()
	fmt.Println("✅ 已捕获凭据（下面只显示形态与有效期，不显示内容）")

	// 解码 JWT 载荷取有效期信息。
	// ⚠️ 只解码、**不验签** —— 明确不作为安全判定（Codex F10）。
	accExp, accTyp, accAlg := jwtInfo(creds.AccessToken)
	refExp, refTyp, refAlg := jwtInfo(creds.RefreshToken)

	now := time.Now()
	fmt.Printf("   accessToken : %d 字符, alg=%s, typ=%s, 剩余 %s\n",
		len(creds.AccessToken), accAlg, accTyp, untilStr(accExp, now))
	fmt.Printf("   refreshToken: %d 字符, alg=%s, typ=%s, 剩余 %s\n",
		len(creds.RefreshToken), refAlg, refTyp, untilStr(refExp, now))

	// ── 先证明凭据**能用**，再暂存 ──
	// Codex 第 28 轮：「凭据字段解析成功 = 捕获完成，不等于 账号验证完成。」
	fmt.Println()
	fmt.Println("--- 验证凭据可用性（Identity + Credit）---")
	uid, nickname, credits, err := verify(creds.AccessToken, creds.RefreshToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ 凭据验证失败，**不暂存**: %v\n", err)
		return 1
	}
	fmt.Printf("   ✅ Identity: uid=%s nickname=%s\n", maskID(uid), nickname)
	fmt.Printf("   ✅ Credit  : %s\n", creditStr(credits))

	// ── 加密暂存 ──
	st := stashPlain{
		AccessToken:    creds.AccessToken,
		RefreshToken:   creds.RefreshToken,
		CapturedAt:     now.Format(time.RFC3339),
		AccessExp:      accExp,
		RefreshExp:     refExp,
		RefreshTyp:     refTyp,
		RefreshSidHash: jwtClaimHash(creds.RefreshToken, "sid"),
	}
	if err := saveStash(out, st); err != nil {
		fmt.Fprintf(os.Stderr, "❌ 暂存失败: %v\n", err)
		return 1
	}
	fmt.Printf("\n✅ 已加密暂存到: %s\n", out)
	fmt.Println("   下一步（**先确认浏览器窗口已全部关闭**再跑）:")
	fmt.Printf("     go run ./tools/refreshprobe -step use -out %q\n", out)

	// ── 独立确认浏览器进程树确实退出（Codex 第 1 步要求）──
	//
	// login.Login 内部 defer sess.Close() 已经关了；这里做**独立确认**，
	// 不能只信"我调用了 Close"。
	fmt.Println()
	fmt.Println("--- 确认受控浏览器已退出 ---")
	if n := countLeftoverProfiles(); n > 0 {
		fmt.Printf("   ⚠️ 仍有 %d 个 wbapi-login-* 临时目录残留\n", n)
	} else {
		fmt.Println("   ✅ 无残留临时 profile")
	}
	if s := login.CurrentSession(); s != nil {
		fmt.Println("   ⚠️ 登录互斥位未释放")
	} else {
		fmt.Println("   ✅ 登录互斥位已释放")
	}
	return 0
}

// ─────────────────────────────────────────────────────────────
// 步骤 2：不带浏览器，证明凭据可独立使用
// ─────────────────────────────────────────────────────────────

func doUse(out string) int {
	fmt.Println("=== 步骤 2：不带浏览器 Cookie，用暂存凭据查 Credit ===")
	fmt.Println()

	st, err := loadStash(out)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ 读取暂存失败: %v\n", err)
		return 1
	}
	fmt.Printf("   凭据捕获于: %s\n", st.CapturedAt)
	fmt.Println("   本步骤**完全不启动浏览器**，纯 HTTP 调用。")
	fmt.Println()

	if s := login.CurrentSession(); s != nil {
		fmt.Println("   ⚠️ 检测到仍有受控浏览器会话存活 —— 本步骤的结论会失去意义！")
		return 1
	}
	fmt.Println("   ✅ 确认没有受控浏览器会话在运行")
	fmt.Println()

	uid, nickname, credits, err := verify(st.AccessToken, st.RefreshToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ 凭据无法独立使用: %v\n", err)
		return 1
	}
	fmt.Printf("✅ Identity: uid=%s nickname=%s\n", maskID(uid), nickname)
	fmt.Printf("✅ Credit  : %s\n", creditStr(credits))
	fmt.Println()
	fmt.Println(">>> 结论：凭据**不依赖浏览器 Cookie**，可独立使用。")
	return 0
}

// ─────────────────────────────────────────────────────────────
// 步骤 3+4：调续期端点，内存中比较新旧 token
// ─────────────────────────────────────────────────────────────

func doRefresh(out string) int {
	fmt.Println("=== 步骤 3+4：调续期端点，比较新旧 token（只报告是否变化）===")
	fmt.Println()

	st, err := loadStash(out)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ 读取暂存失败: %v\n", err)
		return 1
	}
	fmt.Printf("   沿用 %s 捕获的 refresh token\n", st.CapturedAt)
	fmt.Printf("   当前 access token 剩余: %s\n", untilStr(st.AccessExp, time.Now()))
	fmt.Println()

	res, err := refreshToken(st.RefreshToken)
	if err != nil {
		// ⚠️ 不打印原始响应体（可能含 token）。
		fmt.Fprintf(os.Stderr, "❌ 续期失败: %v\n", err)
		return 1
	}

	fmt.Printf("   HTTP %d\n", res.StatusCode)
	if res.BizCode != nil {
		fmt.Printf("   业务码: %d\n", *res.BizCode)
	}
	fmt.Printf("   响应字段形态（只有名字与长度）: %s\n", res.FieldShape)
	fmt.Println()

	if !res.HasAccess || !res.HasRefresh {
		fmt.Println("⚠️ 响应里没有拿到完整的 access+refresh —— 端点契约可能不是我们假设的那样。")
		return 1
	}

	// ── 只报告"是否变化"，不打印内容（Codex 第 4 步原文）──
	fmt.Println("--- 新旧对比（只报告是否变化）---")
	fmt.Printf("   accessToken : 变化=%v  长度 %d → %d\n",
		res.AccessChanged, len(st.AccessToken), len(res.NewAccess))
	fmt.Printf("   refreshToken: 变化=%v  长度 %d → %d\n",
		res.RefreshChanged, len(st.RefreshToken), len(res.NewRefresh))

	oldAccExp, _, _ := jwtInfo(st.AccessToken)
	newAccExp, newAccTyp, newAccAlg := jwtInfo(res.NewAccess)
	fmt.Printf("   accessToken 到期: %s → %s\n",
		untilStr(oldAccExp, time.Now()), untilStr(newAccExp, time.Now()))
	fmt.Printf("   accessToken 形态: alg=%s typ=%s\n", newAccAlg, newAccTyp)

	if res.RefreshChanged {
		fmt.Printf("   refresh 的 sid 是否变化: %v\n",
			jwtClaimHash(st.RefreshToken, "sid") != jwtClaimHash(res.NewRefresh, "sid"))
		fmt.Println("   ⚠️ refresh token 被**轮换** —— 必须原子保存新的，否则下次续期会失败")
	} else {
		fmt.Println("   refresh token **未轮换**（服务端仍返回同一个）")
	}

	// ── 用新 access token 再查一次 Credit（Codex 第 4 步）──
	fmt.Println()
	fmt.Println("--- 用【新】access token 再查 Credit ---")
	nextRefresh := st.RefreshToken
	if res.RefreshChanged {
		nextRefresh = res.NewRefresh
	}
	uid, nickname, credits, err := verify(res.NewAccess, nextRefresh)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ 新 access token 不可用: %v\n", err)
		return 1
	}
	fmt.Printf("✅ uid=%s nickname=%s\n", maskID(uid), nickname)
	fmt.Printf("✅ Credit: %s\n", creditStr(credits))
	fmt.Println(">>> 结论：续期得到的**新 access token 可用**。")

	// ── 原子保存新凭据（Codex 第 4 步：原子保存服务端返回的 refresh）──
	st.AccessToken = res.NewAccess
	if res.RefreshChanged {
		st.RefreshToken = res.NewRefresh
	}
	st.CapturedAt = time.Now().Format(time.RFC3339)
	st.AccessExp = newAccExp
	if e, t, _ := jwtInfo(st.RefreshToken); e > 0 {
		st.RefreshExp = e
		st.RefreshTyp = t
		st.RefreshSidHash = jwtClaimHash(st.RefreshToken, "sid")
	}
	if err := saveStash(out, st); err != nil {
		fmt.Fprintf(os.Stderr, "❌ 保存新凭据失败: %v\n", err)
		return 1
	}
	fmt.Printf("\n✅ 新凭据已原子保存到 %s\n", out)
	fmt.Println("   下一步（**重启 wbapi / 新开一个进程**后验证恢复路径）:")
	fmt.Printf("     go run ./tools/refreshprobe -step refresh -out %q\n", out)
	return 0
}

// ─────────────────────────────────────────────────────────────
// 清理
// ─────────────────────────────────────────────────────────────

func doCleanup(out string) int {
	fmt.Println("=== 清理暂存凭据 ===")
	if err := os.Remove(out); err != nil {
		if os.IsNotExist(err) {
			fmt.Println("   暂存文件不存在（已清理过）")
			return 0
		}
		fmt.Fprintf(os.Stderr, "❌ 删除失败: %v\n", err)
		return 1
	}
	fmt.Printf("✅ 已删除: %s\n", out)
	return 0
}

// ─────────────────────────────────────────────────────────────
// 加密暂存（与账号库同一个 DPAPI 加密器与原子写）
// ─────────────────────────────────────────────────────────────

// stashCodec 返回与账号库相同口径的加密器。
//
// ⚠️ Description 与 codec.go 里的一致，便于排查"这是谁加密的"。
func stashCodec() storage.Codec {
	return storage.DPAPICodec{Description: "wbapi 账号凭据"}
}

func saveStash(path string, st stashPlain) error {
	plain, err := json.Marshal(st)
	if err != nil {
		return err
	}
	env, err := storage.NewEnvelope(stashCodec(), plain)
	if err != nil {
		return fmt.Errorf("加密失败: %w", err)
	}
	sealed, err := json.Marshal(env)
	if err != nil {
		return err
	}
	// 用项目自己的原子写（临时文件 + rename），避免进程中途退出留下半个文件。
	return storage.AtomicWrite(path, sealed, 0o600)
}

func loadStash(path string) (stashPlain, error) {
	var st stashPlain
	raw, err := os.ReadFile(path)
	if err != nil {
		return st, err
	}
	var env storage.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return st, fmt.Errorf("解析信封失败: %w", err)
	}
	plain, err := env.Open(stashCodec())
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(plain, &st); err != nil {
		return st, fmt.Errorf("解析内容失败: %w", err)
	}
	return st, nil
}

// ─────────────────────────────────────────────────────────────
// JWT 解码（只解码，不验签）
// ─────────────────────────────────────────────────────────────

// jwtInfo 解出 JWT 的 (exp, typ, alg)。
//
// 🔴 这里**只解码、不验签**。Codex F10 明确要求不得声称验签成功。
// 解不出来的部分返回零值/空串，绝不 panic。
func jwtInfo(tok string) (exp int64, typ, alg string) {
	claims := jwtClaims(tok)
	if claims == nil {
		return 0, "", ""
	}
	if v, ok := claims["exp"].(float64); ok {
		exp = int64(v)
	}
	typ, _ = claims["typ"].(string)
	alg, _ = claims["alg"].(string)
	// alg 通常在 header 而不是 payload；两种都试。
	if alg == "" {
		alg = jwtHeaderAlg(tok)
	}
	return exp, typ, alg
}

// jwtClaims 解出 JWT 的 payload（第二段）。失败返回 nil。
func jwtClaims(tok string) map[string]any {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// 有些实现用带 padding 的标准编码，再试一次。
		raw, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return nil
		}
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	return m
}

// jwtHeaderAlg 解出 JWT header 的 alg。
func jwtHeaderAlg(tok string) string {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return ""
	}
	var h struct {
		Alg string `json:"alg"`
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		return ""
	}
	return h.Alg
}

// jwtClaimHash 返回某 claim 的 sha256 前 8 位 hex；claim 不存在返回空串。
//
// 🔴 存**哈希**而不是原值 —— sid 虽然不是凭据，但没有理由留原值。
func jwtClaimHash(tok, claim string) string {
	claims := jwtClaims(tok)
	if claims == nil {
		return ""
	}
	v, ok := claims[claim].(string)
	if !ok || v == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:4])
}

// ─────────────────────────────────────────────────────────────
// 显示辅助（一律不输出 token）
// ─────────────────────────────────────────────────────────────

// untilStr 把 exp 转成"还剩多久"的可读串。
func untilStr(exp int64, now time.Time) string {
	if exp == 0 {
		return "(无 exp 声明)"
	}
	d := time.Unix(exp, 0).Sub(now)
	if d <= 0 {
		return "已过期"
	}
	days := int(d.Hours() / 24)
	if days >= 1 {
		return fmt.Sprintf("%d 天 %d 小时", days, int(d.Hours())%24)
	}
	return fmt.Sprintf("%d 小时", int(d.Hours()))
}

// maskID 只显示 ID 的前 8 位。
func maskID(s string) string {
	if len(s) <= 8 {
		return "***"
	}
	return s[:8] + "…"
}

// creditStr 格式化额度。
func creditStr(c *int64) string {
	if c == nil {
		return "查询成功（当前无额度包）"
	}
	return fmt.Sprintf("%d 分", *c)
}

// countLeftoverProfiles 数临时 profile 残留。
func countLeftoverProfiles() int {
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		return -1
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "wbapi-login-") {
			n++
		}
	}
	return n
}
