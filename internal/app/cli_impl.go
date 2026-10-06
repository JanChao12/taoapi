// cli_impl.go：一次性命令的实现（auth / checkin / status / report / doctor）。
//
// 安全红线：任何输出路径（stdout/stderr/日志）不得出现 accessToken/refreshToken。
// UID 一律经 auth.MaskUID 掩码后输出。
package app

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"workbuddy.local/workbuddy-api/internal/auth"
	"workbuddy.local/workbuddy-api/internal/pool"
	"workbuddy.local/workbuddy-api/internal/provider/workbuddy"
	usagepkg "workbuddy.local/workbuddy-api/internal/usage"
)

// todayCN 返回今天在 UTC+8 的日期串（YYYY-MM-DD）。
//
// 上游墙钟是北京时间；签到归属日必须用它，不能用机器本地时区。
func todayCN() string {
	return time.Now().In(cnZone).Format("2006-01-02")
}

var cnZone = time.FixedZone("UTC+8", 8*60*60)

// loadAccountsForCLI 加载账号文件；空仓库时提示导入命令。
func loadAccountsForCLI() (*auth.Store, *auth.Persister, error) {
	p := auth.NewPersister(auth.AccountsPath(), storageCodec())
	st, err := p.Load()
	if err != nil {
		return nil, nil, err
	}
	return st, p, nil
}

// ─────────────────────────────────────────────────────────────
// auth
// ─────────────────────────────────────────────────────────────

// runAuth 处理 auth 的三个子命令：import / list / remove。
func runAuth(args []string) int {
	if len(args) == 0 {
		authUsage()
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "import":
		return runAuthImport(rest)
	case "list":
		return runAuthList(rest)
	case "remove":
		return runAuthRemove(rest)
	default:
		fmt.Fprintf(os.Stderr, "wbapi auth: 未知子命令 %q\n\n", sub)
		authUsage()
		return 2
	}
}

func authUsage() {
	fmt.Fprint(os.Stderr, `用法:
  wbapi auth import --from-wild-work <auths目录>   从 wild-work 凭据目录一次性导入
  wbapi auth list                                  列出账号
  wbapi auth remove <uid>                          删除账号
`)
}

// wildWorkFile 是 wild-work 凭据文件的形状（仅导入时读取，运行时不依赖）。
type wildWorkFile struct {
	Account struct {
		UID          string `json:"uid"`
		Nickname     string `json:"nickname"`
		EnterpriseID string `json:"enterpriseId"`
	} `json:"account"`
	Auth struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		Domain       string `json:"domain"`
		ExpiresAt    int64  `json:"expiresAt"`
	} `json:"auth"`
}

// runAuthImport 从 wild-work 的 auths 目录导入国内版账号。
//
// 规则（Codex 第 8 轮确认）：
//   - 只认 workbuddy-*.json，跳过国际版（workbuddyai-*）
//   - uid/accessToken 缺失的文件跳过并警告
//   - 按 UID 去重，可重复执行（覆盖更新）
//   - 导入即 DPAPI 加密落盘；坏文件不阻断其余导入，最终退出码非 0
//   - 不删除、不移动源文件
func runAuthImport(args []string) int {
	fs := newFlagSet("auth import")
	dir := fs.String("from-wild-work", "", "wild-work 的 auths 目录路径")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "wbapi auth import: 缺少 --from-wild-work <目录>")
		return 2
	}

	entries, err := os.ReadDir(*dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wbapi auth import: 读目录失败: %v\n", err)
		return 1
	}

	st, persister, err := loadAccountsForCLI()
	if err != nil {
		fmt.Fprintf(os.Stderr, "wbapi auth import: %v\n", err)
		return 1
	}

	imported, updated, bad := 0, 0, 0
	for _, e := range entries {
		name := e.Name()
		// 两种前缀都要（2026-10-06 起支持国际版）：
		//   workbuddy-*   国内版
		//   workbuddyai-* 国际版
		//
		// ⚠️ 顺序要紧：先判 workbuddyai- 再判 workbuddy-，
		//	因为 "workbuddyai-xxx" 也满足 HasPrefix("workbuddy-") == false
		//	（第 8 个字符是 'a' 不是 '-'），所以两者其实不冲突 ——
		//	但显式写成分别判断更清楚，免得以后有人误改。
		isIntl := strings.HasPrefix(name, "workbuddyai-")
		isCN := strings.HasPrefix(name, "workbuddy-")
		if e.IsDir() || (!isCN && !isIntl) || !strings.HasSuffix(name, ".json") {
			continue
		}

		raw, err := os.ReadFile(filepath.Join(*dir, name))
		if err != nil {
			fmt.Fprintf(os.Stderr, "  跳过 %s: %v\n", name, err)
			bad++
			continue
		}

		var wf wildWorkFile
		if err := json.Unmarshal(raw, &wf); err != nil {
			fmt.Fprintf(os.Stderr, "  跳过 %s: JSON 解析失败\n", name)
			bad++
			continue
		}
		if wf.Account.UID == "" || wf.Auth.AccessToken == "" {
			fmt.Fprintf(os.Stderr, "  跳过 %s: 缺少 uid 或 accessToken\n", name)
			bad++
			continue
		}

		// 🔴 平台由文件名前缀决定，并由 domain 交叉校验。
		//
		//	用文件名判平台是**刻意的**：它是外部事实（wild-work 自己的命名），
		//	而 domain 字段可能为空或写错。两者不一致时**信文件名**，
		//	因为发到错误域名会让凭据外泄 —— 宁可相信约定。
		plat := auth.PlatformCN
		if isIntl {
			plat = auth.PlatformIntl
		}

		acct := &auth.Account{
			UID:            wf.Account.UID,
			Platform:       plat,
			Nickname:       wf.Account.Nickname,
			EnterpriseID:   wf.Account.EnterpriseID,
			Domain:         wf.Auth.Domain,
			AccessToken:    wf.Auth.AccessToken,
			RefreshToken:   wf.Auth.RefreshToken,
			TokenExpiresAt: wf.Auth.ExpiresAt,
			Status:         pool.StatusNormal,
		}
		if _, exists := st.Get(acct.UID); exists {
			updated++
		} else {
			imported++
		}
		st.Put(acct)
	}

	if st.Len() > 0 {
		if err := persister.Save(st); err != nil {
			fmt.Fprintf(os.Stderr, "wbapi auth import: 保存失败: %v\n", err)
			return 1
		}
	}

	// 国际版从 2026-10-06 起也导入，所以不再有"跳过(国际版)"这一项。
	fmt.Printf("导入完成：新增 %d，更新 %d，坏文件 %d，共 %d 个账号\n",
		imported, updated, bad, st.Len())
	fmt.Printf("账号文件：%s（凭据已 DPAPI 加密）\n", auth.AccountsPath())

	// 首次导入后立即刷新额度。
	//
	// 为什么必须做：调度规则把「额度未知」排除（防止错用没额度的号），
	// 若导入后不查一次额度，所有账号都不可调度 —— 服务起得来但一个号都不用，
	// 且日志只有一行 no_available_account，很难排查。
	// （步骤⑦真实回归中实际踩到。）
	if imported+updated > 0 {
		fmt.Println("正在刷新账号额度…")
		refreshCredits(st, 4)
		if err := persister.Save(st); err != nil {
			fmt.Fprintf(os.Stderr, "wbapi auth import: 刷新后保存失败: %v\n", err)
		} else {
			for _, a := range st.List() {
				status := "—"
				if a.Credit.Known {
					status = fmt.Sprintf("%d 分", a.Credit.Remaining)
				} else if a.LastError != "" {
					status = "查询失败"
				}
				fmt.Printf("  %s 额度: %s\n", a.Nickname, status)
			}
		}
	}

	if bad > 0 {
		return 1
	}
	return 0
}

// runAuthList 列出账号概要。
func runAuthList(_ []string) int {
	st, _, err := loadAccountsForCLI()
	if err != nil {
		fmt.Fprintf(os.Stderr, "wbapi auth list: %v\n", err)
		return 1
	}
	if st.Len() == 0 {
		fmt.Println("没有账号。先执行: wbapi auth import --from-wild-work <目录>")
		return 0
	}

	now := time.Now()
	fmt.Printf("%-14s %-12s %-10s %10s  %-12s %s\n",
		"昵称", "UID", "状态", "额度", "最早到期", "7天内到期")
	for _, a := range st.List() {
		eff := a.EffectiveStatus(now)
		earliest := a.Credit.EarliestExpiry()
		if earliest == "" {
			earliest = "(未知)"
		}
		credits := "—"
		if a.Credit.Known {
			credits = fmt.Sprintf("%d", a.Credit.Remaining)
		}
		exp7 := a.Credit.ExpiringWithin(pool.DisplayHorizon, now)
		fmt.Printf("%-14s %-12s %-10s %10s  %-12s %d\n",
			a.Nickname, auth.MaskUID(a.UID), eff.Label(), credits, earliest, exp7)
	}
	return 0
}

// runAuthRemove 删除指定账号。
func runAuthRemove(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "用法: wbapi auth remove <uid>")
		return 2
	}
	uid := args[0]

	st, persister, err := loadAccountsForCLI()
	if err != nil {
		fmt.Fprintf(os.Stderr, "wbapi auth remove: %v\n", err)
		return 1
	}
	if !st.Remove(uid) {
		// 允许用 UID 前缀匹配，方便从 list 输出复制
		full := ""
		for _, a := range st.List() {
			if strings.HasPrefix(a.UID, strings.TrimSuffix(uid, "…")) {
				full = a.UID
				break
			}
		}
		if full == "" || !st.Remove(full) {
			fmt.Fprintf(os.Stderr, "wbapi auth remove: 找不到账号 %s\n", auth.MaskUID(uid))
			return 1
		}
		uid = full
	}
	if err := persister.Save(st); err != nil {
		fmt.Fprintf(os.Stderr, "wbapi auth remove: 保存失败: %v\n", err)
		return 1
	}
	fmt.Printf("已删除 %s\n", auth.MaskUID(uid))
	return 0
}

// ─────────────────────────────────────────────────────────────
// status
// ─────────────────────────────────────────────────────────────

// runStatusCmd 刷新并显示所有账号的额度与状态。
//
// 并发 4：账号是个位数，信号量比 errgroup 简单且零依赖。
func runStatusCmd(_ []string) int {
	st, persister, err := loadAccountsForCLI()
	if err != nil {
		fmt.Fprintf(os.Stderr, "wbapi status: %v\n", err)
		return 1
	}
	if st.Len() == 0 {
		fmt.Println("没有账号。先执行: wbapi auth import --from-wild-work <目录>")
		return 0
	}

	refreshCredits(st, 4)
	_ = persister.Save(st) // 刷新结果落盘；失败不影响展示

	now := time.Now()
	fmt.Printf("（%s 刷新）\n\n", now.Format("2006-01-02 15:04:05"))
	fmt.Printf("%-14s %-12s %-10s %10s  %-12s %12s\n",
		"昵称", "UID", "状态", "额度", "最早到期", "7天内到期")

	list := st.List()
	sort.Slice(list, func(i, j int) bool { return list[i].UID < list[j].UID })
	for _, a := range list {
		eff := a.EffectiveStatus(now)
		earliest := a.Credit.EarliestExpiry()
		if earliest == "" {
			earliest = "(未知)"
		}
		credits := "—"
		if a.Credit.Known {
			credits = fmt.Sprintf("%d", a.Credit.Remaining)
		}
		exp7 := a.Credit.ExpiringWithin(pool.DisplayHorizon, now)
		fmt.Printf("%-14s %-12s %-10s %10s  %-12s %12d\n",
			a.Nickname, auth.MaskUID(a.UID), eff.Label(), credits, earliest, exp7)
	}
	return 0
}

// refreshCredits 并发刷新每个账号的额度快照。
//
// 单账号失败不阻断其余。
//
// 🔴 2026-10-07 修既存数据竞争（Codex 第 52 轮指出的高优先级待办）：
//
//	原实现从 `st.List()` 拿**共享指针**，在 goroutine 里直接写
//	`a.LastError / a.Credit.*` —— 无锁写入，与换号路径
//	（`failover.go`）会同时写同一个 Account。
//
//	现在：**读**用快照（锁内值拷贝）、**写**在 `Mutate` 回调内、
//	并用 `MutateIfRev` 做**条件提交**（期间被改过就丢弃本次结果）。
//
// ⚠️ 注意不能用 "Get 拿指针 → 改 → 再调 Mutate"：
//
//	先改共享指针再调 Mutate **补救不了**已经发生的无锁写入。
func refreshCredits(st *auth.Store, concurrency int) {
	type job struct {
		snap auth.Account
		rev  uint64
	}
	var jobs []job
	for _, uid := range st.UIDs() { // 只拿 ID（值）
		snap, ok := st.Snapshot(uid) // 锁内值拷贝
		if !ok {
			continue
		}
		rev, ok := st.Rev(uid) // 版本基线
		if !ok {
			continue
		}
		jobs = append(jobs, job{snap: snap, rev: rev})
	}

	client := workbuddy.NewClient()
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

	for _, j := range jobs {
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()

			// 用**快照**构造 provider（只读凭据；不碰共享指针）
			p := workbuddyProviderFor(client, &j.snap)
			cr, err := p.Credit(ctx, "")
			now := time.Now()

			if err != nil {
				st.MutateIfRev(j.snap.UID, j.rev, func(a *auth.Account) bool {
					a.LastObservedAt = now
					a.LastError = err.Error()
					return true
				})
				return
			}

			st.MutateIfRev(j.snap.UID, j.rev, func(a *auth.Account) bool {
				a.LastObservedAt = now
				a.LastError = ""
				a.Credit.Known = cr.Remaining != nil
				if cr.Remaining != nil {
					a.Credit.Remaining = *cr.Remaining
				}
				a.Credit.At = now
				a.Credit.Packages = a.Credit.Packages[:0]
				for _, pa := range cr.Accounts {
					a.Credit.Packages = append(a.Credit.Packages, auth.PackageSnapshot{
						Name:     pa.PackageName,
						Remain:   pa.Remain,
						ExpireAt: pa.ExpireAt,
					})
				}
				return true
			})
		}(j)
	}
	wg.Wait()
}

// ─────────────────────────────────────────────────────────────
// checkin
// ─────────────────────────────────────────────────────────────

// runCheckinCmd 执行签到。
//
// --due：只处理今天（UTC+8）还没签的账号 —— 供计划任务每日调用。
func runCheckinCmd(args []string) int {
	fs := newFlagSet("checkin")
	due := fs.Bool("due", false, "只签到今天未签的账号（计划任务用）")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	st, persister, err := loadAccountsForCLI()
	if err != nil {
		fmt.Fprintf(os.Stderr, "wbapi checkin: %v\n", err)
		return 1
	}
	if st.Len() == 0 {
		fmt.Println("没有账号。先执行: wbapi auth import --from-wild-work <目录>")
		return 0
	}

	today := todayCN()
	client := workbuddy.NewClient()
	failed := 0

	// 🔴 2026-10-07 修数据竞争：原实现 `for _, a := range st.List()` 拿共享
	//	指针就地写 LastError / CheckinDay / CheckinAt（绕过锁）。
	//	本循环虽为单线程顺序执行，但写共享指针本身就违反 Store 约定，
	//	且与同进程其它路径（以及将来可能的并发调用）会踩。
	//	⇒ 读用快照、写只传 UID（锁内改）。
	for _, uid := range st.UIDs() {
		a, ok := st.Snapshot(uid)
		if !ok {
			continue
		}
		if a.ManualDisabled {
			fmt.Printf("%-14s 跳过（人工禁用）\n", a.Nickname)
			continue
		}
		if *due && a.CheckinDay == today {
			fmt.Printf("%-14s 跳过（今日已签）\n", a.Nickname)
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		res, err := workbuddyProviderFor(client, &a).Checkin(ctx, "")
		cancel()

		switch {
		case err != nil:
			failed++
			st.Mutate(uid, func(x *auth.Account) bool {
				x.LastError = err.Error()
				x.LastObservedAt = time.Now()
				return true
			})
			fmt.Printf("%-14s ❌ %v\n", a.Nickname, err)
		case res.AlreadyCheckedIn:
			st.Mutate(uid, func(x *auth.Account) bool {
				x.CheckinDay = today
				x.CheckinAt = time.Now()
				return true
			})
			fmt.Printf("%-14s ✓ 今日已签到\n", a.Nickname)
		default:
			st.Mutate(uid, func(x *auth.Account) bool {
				x.CheckinDay = today
				x.CheckinAt = time.Now()
				x.LastError = ""
				return true
			})
			fmt.Printf("%-14s ✓ 签到成功 %s\n", a.Nickname, res.Message)
		}
	}

	if err := persister.Save(st); err != nil {
		fmt.Fprintf(os.Stderr, "wbapi checkin: 保存失败: %v\n", err)
	}
	if failed > 0 {
		return 1
	}
	return 0
}

// ─────────────────────────────────────────────────────────────
// report
// ─────────────────────────────────────────────────────────────

// runReportCmd 生成静态 HTML 用量报告（最近 30 天）。
func runReportCmd(_ []string) int {
	store := usagepkg.NewStore("")
	totals := struct {
		requests, ok, prompt, completion, total, reasoning int64
		credits                                            float64
		creditSeen                                         bool
	}{}
	type modelRow struct {
		model       string
		requests    int64
		totalTokens int64
	}
	models := map[string]*modelRow{}

	err := store.Read(30, func(ev usagepkg.Event) error {
		totals.requests++
		if ev.OK {
			totals.ok++
		}
		totals.prompt += ev.PromptTokens
		totals.completion += ev.CompletionTokens
		totals.total += ev.TotalTokens
		totals.reasoning += ev.ReasoningTokens
		if ev.Credit != nil {
			totals.creditSeen = true
			totals.credits += *ev.Credit
		}
		m, ok := models[ev.Model]
		if !ok {
			m = &modelRow{model: ev.Model}
			models[ev.Model] = m
		}
		m.requests++
		m.totalTokens += ev.TotalTokens
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "wbapi report: %v\n", err)
		return 1
	}

	// 简单表格页；零 JS。
	var b strings.Builder
	b.WriteString(`<!DOCTYPE html><html lang="zh-CN"><head><meta charset="utf-8">
<title>wbapi 用量报告（30 天）</title><style>
body{font-family:system-ui,sans-serif;margin:2rem;color:#1f2937}
h1{font-size:1.4rem}.cards{display:flex;gap:1rem;flex-wrap:wrap;margin:1rem 0}
.card{border:1px solid #e5e7eb;border-radius:8px;padding:1rem 1.4rem}
.card b{display:block;font-size:1.5rem}table{border-collapse:collapse;width:100%}
th,td{border-bottom:1px solid #e5e7eb;padding:.5rem;text-align:left}
th{color:#6b7280;font-weight:500}</style></head><body>
<h1>wbapi 用量报告（最近 30 天）</h1><div class="cards">`)
	card := func(label, val string) {
		b.WriteString(`<div class="card">` + label + `<b>` + val + `</b></div>`)
	}
	card("请求", fmt.Sprint(totals.requests))
	card("成功", fmt.Sprint(totals.ok))
	card("Token 总计", fmt.Sprint(totals.total))
	if totals.creditSeen {
		card("积分消耗", fmt.Sprintf("%.2f", totals.credits))
	}
	b.WriteString(`</div><table><tr><th>模型</th><th>调用</th><th>Token</th></tr>`)
	for _, m := range models {
		fmt.Fprintf(&b, "<tr><td>%s</td><td>%d</td><td>%d</td></tr>",
			htmlEsc(m.model), m.requests, m.totalTokens)
	}
	b.WriteString(`</table></body></html>`)

	out := "wbapi-report.html"
	abs, _ := filepath.Abs(out)
	if err := os.WriteFile(out, []byte(b.String()), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "wbapi report: 写入失败: %v\n", err)
		return 1
	}
	fmt.Printf("已生成 %s\n", abs)
	return 0
}

func htmlEsc(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

// ─────────────────────────────────────────────────────────────
// doctor
// ─────────────────────────────────────────────────────────────

// runDoctorCmd 诊断：账号文件、解密、上游连通、端口。
func runDoctorCmd(_ []string) int {
	results := 0 // 非零项计数

	// 1. 账号文件
	path := auth.AccountsPath()
	if _, err := os.Stat(path); err == nil {
		fmt.Printf("✓ 账号文件存在: %s\n", path)
	} else {
		fmt.Printf("✗ 账号文件不存在: %s（先 auth import）\n", path)
		results++
	}

	// 2. 解密加载
	st, _, err := loadAccountsForCLI()
	if err != nil {
		fmt.Printf("✗ 账号加载失败: %v\n", err)
		results++
	} else {
		fmt.Printf("✓ 账号加载成功（%d 个）\n", st.Len())
	}

	// 3. 上游连通（逐账号 Credit 轻探）
	if st != nil && st.Len() > 0 {
		client := workbuddy.NewClient()
		okCnt := 0
		for _, a := range st.List() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			_, err := workbuddyProviderFor(client, a).Credit(ctx, "")
			cancel()
			if err != nil {
				fmt.Printf("✗ 账号 %s 上游失败: %v\n", a.Nickname, err)
				results++
			} else {
				okCnt++
			}
		}
		fmt.Printf("✓ 上游连通 %d/%d\n", okCnt, st.Len())
	}

	// 4. 端口可用
	ln, err := net.Listen("tcp", DefaultAddr)
	if err != nil {
		fmt.Printf("✗ 端口 %s 被占用: %v\n", DefaultAddr, err)
		results++
	} else {
		_ = ln.Close()
		fmt.Printf("✓ 端口 %s 可用\n", DefaultAddr)
	}

	if results > 0 {
		fmt.Printf("\n诊断完成：%d 项异常\n", results)
		return 1
	}
	fmt.Println("\n诊断完成：全部正常")
	return 0
}

// newFlagSet 是 CLI 各命令共用的 flag 构造（统一错误输出）。
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}
