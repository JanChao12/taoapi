// 国际版（WorkBuddyAI）完整侦查工具。
//
// 用途（2026-10-06，委托方要求"没验证的都需要验证一次"）：
// 逐项实测国际版的**全部能力**，不留"推断"。
//
// 覆盖：
//  1. 模型目录（含倍率/badge/maxOutput）
//  2. **逐个模型的可用性**（真发一次对话）
//  3. 额度查询端点
//  4. 签到端点（回答"国际版有没有签到活动"）
//  5. refresh token 续期
//  6. reasoning_effort 语义（国际版 vs 国内版是否一致）
//  7. deepseek-v4.1-flash-sg 是否存在
//
// 🔴 必须走项目自己的 Client（SetBases 换基址）：
//
//	手搓 HTTP 会漏一串实测派生的头，得到"400 + 空响应"，
//	极易误判成"平台不兼容"（本工具的前身已经踩过这个坑）。
//
// 用法：
//
//	go run ./tools/wbaiprobe                  # 全部跑一遍
//	go run ./tools/wbaiprobe -only models     # 只跑某项
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"workbuddy.local/workbuddy-api/internal/provider"
	"workbuddy.local/workbuddy-api/internal/provider/workbuddy"
)

// intlBase 是国际版基址（实测：www.workbuddy.ai 页面自报
// IS_INTERNATIONAL_EDITION=true；chat 与 billing 同域名）。
const intlBase = "https://www.workbuddy.ai"

// abstractModels 是委托方明确要求**砍掉**的 5 个抽象档位。
//
// 原话：「5 个抽象档位：default-model/fast-model/balanced-model/
//
//	primary-model/deep-model 砍掉我不要」
var abstractModels = map[string]bool{
	"default-model":  true,
	"fast-model":     true,
	"balanced-model": true,
	"primary-model":  true,
	"deep-model":     true,
}

func main() {
	authFile := flag.String("auth", "", "wild-work 国际版凭据 JSON 路径")
	only := flag.String("only", "", "只跑某一项: models|chat|credit|checkin|refresh|effort|sg")
	concurrency := flag.Int("c", 3, "逐个模型探测的并发数")
	flag.Parse()

	path := *authFile
	if path == "" {
		p, err := findIntlCredential()
		if err != nil {
			fmt.Fprintf(os.Stderr, "找不到国际版凭据: %v\n", err)
			os.Exit(1)
		}
		path = p
	}
	cred, nickname, err := loadCredential(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "解析凭据失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("账号: %s\n凭据: %s\n\n", nickname, filepath.Base(path))

	client := workbuddy.NewClient()
	// 🔴 基址必须**显式设置** —— NewProviderFor 刻意不改基址
	//    （否则会把测试里指向假上游的 SetBases 覆盖回生产地址，
	//      那会让单测变成联网测试，见 NewProviderFor 的注释）。
	chatBase, billingBase := workbuddy.PlatformIntl.BaseURLs()
	client.SetBases(chatBase, billingBase)
	prov := workbuddy.NewProviderFor(client, cred, workbuddy.PlatformIntl)

	want := func(name string) bool { return *only == "" || *only == name }

	var models []provider.Model
	if want("models") || want("chat") || want("sg") || want("effort") {
		models = section("模型目录", func() []provider.Model { return fetchModels(prov) })
	}
	if want("chat") {
		sectionVoid("逐个模型可用性", func() { probeAllModels(prov, models, *concurrency) })
	}
	if want("effort") {
		sectionVoid("reasoning_effort 语义", func() { probeEffort(prov, models) })
	}
	if want("sg") {
		sectionVoid("deepseek 系列是否存在", func() { probeSG(prov) })
	}
	if want("credit") {
		sectionVoid("额度查询", func() { probeCredit(prov) })
	}
	if want("checkin") {
		sectionVoid("签到", func() { probeCheckin(prov) })
	}
	if want("refresh") {
		sectionVoid("refresh 续期", func() { probeRefresh(client, cred) })
	}
}

// ── 各段实现 ──

func fetchModels(prov provider.Provider) []provider.Model {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	models, err := prov.Models(ctx)
	if err != nil {
		fmt.Printf("❌ 拉取失败: %v\n", err)
		return nil
	}
	fmt.Printf("共 %d 个模型\n\n", len(models))
	fmt.Printf("  %-30s %-8s %-9s %s\n", "ID", "倍率", "maxOut", "badge")
	fmt.Println("  " + strings.Repeat("-", 70))
	for _, m := range models {
		rate := "—"
		if m.Pricing != nil && m.Pricing.HasMultiplier {
			rate = fmt.Sprintf("x%.2f", m.Pricing.Multiplier)
		}
		badge := ""
		if m.Badge != nil {
			badge = m.Badge.Text
		}
		up := m.UpstreamID
		mark := ""
		if abstractModels[up] {
			mark = "  ← 应砍掉"
		}
		fmt.Printf("  %-30s %-8s %-9d %s%s\n",
			m.ID, rate, m.Capabilities.MaxOutputTokens, badge, mark)
	}
	return models
}

type probeResult struct {
	id   string
	ok   bool
	err  string
	ms   int64
	text string
}

func probeAllModels(prov provider.Provider, models []provider.Model, conc int) {
	out := make([]probeResult, len(models))
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup

	for i, m := range models {
		wg.Add(1)
		go func(idx int, mm provider.Model) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			start := time.Now()
			ok, txt, err := chatOnce(prov, mm.UpstreamID)
			out[idx] = probeResult{id: mm.ID, ok: ok, err: err,
				ms: time.Since(start).Milliseconds(), text: txt}
		}(i, m)
	}
	wg.Wait()

	okN, badN := 0, 0
	fmt.Println("✅ 可用：")
	for _, r := range out {
		if r.ok {
			okN++
			fmt.Printf("   %-30s %5dms  %s\n", r.id, r.ms, trimN(r.text, 28))
		}
	}
	fmt.Println("\n❌ 不可用：")
	for _, r := range out {
		if !r.ok {
			badN++
			fmt.Printf("   %-30s %s\n", r.id, r.err)
		}
	}
	fmt.Printf("\n合计: 可用 %d / 不可用 %d / 共 %d\n", okN, badN, len(out))
}

// chatOnce 发一次最小对话（国际版要求首条是 system）。
func chatOnce(prov provider.Provider, model string) (bool, string, string) {
	return chatWithEffort(prov, model, "")
}

func chatWithEffort(prov provider.Provider, model, effort string) (bool, string, string) {
	body := map[string]any{
		"model":      model,
		"stream":     true,
		"max_tokens": 24,
		"messages": []map[string]any{

			{"role": "system", "content": "You are a helpful assistant."},
			{"role": "user", "content": "Reply with exactly: PONG"},
		},
	}
	if effort != "" {
		body["reasoning_effort"] = effort
	}
	raw, _ := json.Marshal(body)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	var gotDone bool
	var text strings.Builder
	err := prov.Chat(ctx, provider.ChatRequest{
		Model: model, RawBody: raw, Stream: true, ReasoningEffort: effort,
	}, func(ev provider.Event) error {
		switch ev.Type {
		case provider.EventContent:
			text.WriteString(ev.Text)
		case provider.EventDone:
			gotDone = true
		}
		return nil
	})
	if err != nil {
		return false, "", trim(err.Error())
	}
	if !gotDone {
		return false, "", "未收到 done（流中断）"
	}
	return true, text.String(), ""
}

// probeEffort 测 reasoning_effort 在国际版是否被接受。
func probeEffort(prov provider.Provider, models []provider.Model) {
	var targets []string
	for _, m := range models {
		up := m.UpstreamID
		if up == "glm-5.3" || up == "gpt-5.6-luna" || up == "hy3" {
			targets = append(targets, up)
		}
	}
	if len(targets) == 0 {
		fmt.Println("（没有可测的目标模型）")
		return
	}

	efforts := []string{"", "minimal", "low", "medium", "high", "max"}
	for _, model := range targets {
		fmt.Printf("\n模型 %s：\n", model)
		for _, ef := range efforts {
			label := ef
			if label == "" {
				label = "(不传)"
			}
			ok, txt, err := chatWithEffort(prov, model, ef)
			if !ok {
				fmt.Printf("   %-10s ❌ %s\n", label, err)
				continue
			}
			fmt.Printf("   %-10s ✅ %s\n", label, trimN(txt, 40))
		}
	}
}

// probeSG 测"国内版有、但国际版目录里没有"的模型能否调用。
//
// 🔴 这一步的意义（2026-10-06 实测发现）：
//
//	国际版目录只列 18 个，但实测 `deepseek-v4.1-flash-sg` 与
//	`deepseek-v4.1-flash` **不在目录里却能成功调用**。
//	说明"目录"与"实际可调用集"**不完全是同一件事**。
//
//	这直接关系到本项目的裁剪决策：如果面板只显示目录里的模型，
//	用户就看不到那些"虽未列出但能用"的模型。
//	⇒ 必须测清楚，不能靠目录推断。
func probeSG(prov provider.Provider) {
	// 国内版 16 个模型的裸 ID（国际版目录里大部分没有）
	candidates := []string{
		"deepseek-v4-pro", "deepseek-v4.1-flash", "glm-5.1", "glm-5.2",
		"glm-5.3", "glm-5.3-flash", "glm-5v-turbo", "hy3", "hy3-x",
		"hy4-preview", "kimi-k2.6", "kimi-k2.7", "kimi-k2.8-preview",
		"kimi-k3-1", "minimax-m3", "space-bunny",
	}
	for _, c := range candidates {
		ok, txt, err := chatOnce(prov, c)
		if ok {
			fmt.Printf("   %-28s ✅ %s\n", c, trimN(txt, 30))
		} else {
			fmt.Printf("   %-28s ❌ %s\n", c, err)
		}
	}
}

func probeCredit(prov provider.Provider) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := prov.Credit(ctx, "")
	if err != nil {
		fmt.Printf("❌ 额度查询失败: %v\n", trim(err.Error()))
		return
	}
	rem := int64(-1)
	if res.Remaining != nil {
		rem = *res.Remaining
	}
	fmt.Printf("✅ 额度查询成功  剩余=%d  包数=%d\n", rem, len(res.Accounts))
	for _, p := range res.Accounts {
		fmt.Printf("     - %s  remain=%d  expire=%s\n", p.PackageName, p.Remain, p.ExpireAt)
	}
}

// probeCheckin 测签到端点。
//
// 🔴 意义：回答"**国际版到底有没有签到活动**" ——
// 若上游不支持，面板对国际版就不该显示签到按钮，
// 否则用户点了只会得到一个看不懂的错误。
func probeCheckin(prov provider.Provider) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := prov.Checkin(ctx, "")
	if err != nil {
		fmt.Printf("❌ 签到失败: %v\n", trim(err.Error()))
		fmt.Println("   ⇒ 结合错误内容判断国际版是否有签到活动")
		return
	}
	fmt.Printf("✅ 签到成功  已签到=%v  消息=%s\n", res.AlreadyCheckedIn, res.Message)
}

// probeRefresh 测 refresh token 续期。
//
// ⚠️ 项目现状（已核实的注释）：**续期自动续期从未实现** ——
//
//	`Client.TokenRefreshURL()` 全项目零调用方
//	（见 failover.go:380、pool.go:54 的注释）。
//	这里直接打端点只是为了**测出国际版的契约**，
//	不代表生产代码已经有续期能力。
//
// 🔴 Codex 警告：同一个 refresh token 反复打可能触发整个 token family 撤销。
//
//	所以本函数**总共只发一个请求**，且只报告响应字段名，不打印 token 值。
func probeRefresh(client *workbuddy.Client, cred workbuddy.Credential) {
	if cred.RefreshToken == "" {
		fmt.Println("（凭据里没有 refresh token，跳过）")
		return
	}
	url := client.TokenRefreshURL()
	fmt.Printf("端点: %s\n", url)

	body, _ := json.Marshal(map[string]string{"refreshToken": cred.RefreshToken})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		fmt.Printf("❌ 构造请求失败: %v\n", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "CLI/2.63.2 CodeBuddy/2.63.2")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Printf("❌ 请求失败: %v\n", trim(err.Error()))
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))

	fmt.Printf("HTTP %d  响应 %d 字节\n", resp.StatusCode, len(raw))
	if resp.StatusCode != http.StatusOK {
		fmt.Printf("响应片段: %s\n", trim(string(raw)))
		return
	}
	// 只报告**字段名与长度**，绝不打印 token 值
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err == nil {
		names := make([]string, 0, len(m))
		for k := range m {
			names = append(names, k)
		}
		sort.Strings(names)
		fmt.Printf("顶层字段: %s\n", strings.Join(names, ", "))
		if d, ok := m["data"].(map[string]any); ok {
			dn := make([]string, 0, len(d))
			for k, v := range d {
				if s, ok := v.(string); ok {
					dn = append(dn, fmt.Sprintf("%s(len=%d)", k, len(s)))
				} else {
					dn = append(dn, k)
				}
			}
			sort.Strings(dn)
			fmt.Printf("data 字段: %s\n", strings.Join(dn, ", "))
		}
	}
}

// ── 工具 ──

func section(name string, fn func() []provider.Model) []provider.Model {
	fmt.Printf("══════ %s ══════\n", name)
	r := fn()
	fmt.Println()
	return r
}

func sectionVoid(name string, fn func()) {
	fmt.Printf("══════ %s ══════\n", name)
	fn()
	fmt.Println()
}

func trim(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 220 {
		s = s[:220] + "…"
	}
	return s
}

func trimN(s string, n int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) > n {
		s = s[:n] + "…"
	}
	return s
}

func findIntlCredential() (string, error) {
	dir := `D:\tools\wild-work\auths`
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "workbuddyai-") && strings.HasSuffix(e.Name(), ".json") {
			return filepath.Join(dir, e.Name()), nil
		}
	}
	return "", fmt.Errorf("%s 下没有 workbuddyai-*.json", dir)
}

func loadCredential(path string) (workbuddy.Credential, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return workbuddy.Credential{}, "", err
	}
	var wf struct {
		Account struct {
			UID          string `json:"uid"`
			Nickname     string `json:"nickname"`
			EnterpriseID string `json:"enterpriseId"`
			Domain       string `json:"domain"`
		} `json:"account"`
		Auth struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(raw, &wf); err != nil {
		return workbuddy.Credential{}, "", err
	}
	return workbuddy.Credential{
		AccessToken:  wf.Auth.AccessToken,
		UID:          wf.Account.UID,
		EnterpriseID: wf.Account.EnterpriseID,
		Domain:       wf.Account.Domain,
		RefreshToken: wf.Auth.RefreshToken,
	}, wf.Account.Nickname, nil
}
