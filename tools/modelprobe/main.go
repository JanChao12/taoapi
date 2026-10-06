// 模型目录真伪核验工具。
//
// 用途（委托方 2026-10-06 要求）：
//
//	「重新在 workbuddy 上游获取真实的模型列表，我怀疑你现在的列表是错误的，
//	  必须再重新获取一遍，并要使用一次每个模型是否真实可用」
//
// 做三件事：
//  1. **重新拉取**上游模型目录（不读本地缓存、不依赖运行中的服务）
//  2. 与本地服务 /v1/models 的列表**逐条比对**，报告差异
//  3. 对每个模型**真实发一次对话**，记录它到底能不能用
//
// 🔴 为什么用 provider.Chat 而不是手搓 HTTP：
//
//	上游**强制 stream:true**（发 false 会 400，见 chat.go 注释），
//	且鉴权头有若干实测派生的字段（机器 ID/会话 ID 等）。
//	手搓请求必然漏掉这些，探测结果就没意义了 —— 必须走真实调用路径。
//
// 🔴 安全：只从 accounts.json 读凭据，绝不打印 token；
// 输出里只有模型 ID、状态码与脱敏错误片段。
//
// 用法：
//
//	go run ./tools/modelprobe --local http://127.0.0.1:8787 --key <面板密钥>
//	go run ./tools/modelprobe --skip-probe     # 只比对目录，不逐个探测
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"workbuddy.local/workbuddy-api/internal/auth"
	"workbuddy.local/workbuddy-api/internal/provider"
	"workbuddy.local/workbuddy-api/internal/provider/workbuddy"
	"workbuddy.local/workbuddy-api/internal/storage"
)

func main() {
	localBase := flag.String("local", "", "本地服务地址（如 http://127.0.0.1:8787），留空跳过比对")
	localKey := flag.String("key", "", "本地服务的面板密钥（用于读 /v1/models）")
	concurrency := flag.Int("c", 3, "并发探测数（上游对并发敏感，默认 3）")
	timeout := flag.Duration("timeout", 90*time.Second, "单个模型探测超时")
	skipProbe := flag.Bool("skip-probe", false, "只比对目录，不逐个探测可用性")
	flag.Parse()

	cred, err := loadFirstCredential()
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取凭据失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("使用账号: %s\n", auth.MaskUID(cred.UID))

	client := workbuddy.NewClient()
	prov := workbuddy.NewProvider(client, cred)

	// ── 步骤 1：重新拉取上游目录 ──
	fmt.Println("\n=== 步骤 1：从上游重新拉取模型目录 ===")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	models, err := prov.Models(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "拉取上游模型目录失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("上游返回 %d 个模型\n", len(models))

	upstreamIDs := make([]string, 0, len(models))
	for _, m := range models {
		upstreamIDs = append(upstreamIDs, m.ID)
	}
	sort.Strings(upstreamIDs)

	// ── 步骤 2：与本地服务比对 ──
	if *localBase != "" {
		fmt.Println("\n=== 步骤 2：与本地服务列表比对 ===")
		compareWithLocal(*localBase, *localKey, upstreamIDs)
	}

	// ── 步骤 3：逐个探测可用性 ──
	if *skipProbe {
		fmt.Println("\n（已跳过可用性探测）")
		fmt.Println("\n=== 上游完整列表 ===")
		for _, id := range upstreamIDs {
			fmt.Println("  " + id)
		}
		return
	}

	fmt.Printf("\n=== 步骤 3：逐个真实调用（并发 %d）===\n", *concurrency)
	results := probeAll(prov, upstreamIDs, *concurrency, *timeout)
	report(results)
}

// loadFirstCredential 从 accounts.json 解出第一个可用账号的凭据。
func loadFirstCredential() (workbuddy.Credential, error) {
	p := auth.NewPersister(auth.AccountsPath(), codec())
	st, err := p.Load()
	if err != nil {
		return workbuddy.Credential{}, err
	}
	for _, a := range st.List() {
		if a.AccessToken == "" || a.ManualDisabled {
			continue
		}
		return workbuddy.Credential{
			AccessToken:  a.AccessToken,
			UID:          a.UID,
			EnterpriseID: a.EnterpriseID,
			Domain:       a.Domain,
			RefreshToken: a.RefreshToken,
		}, nil
	}
	return workbuddy.Credential{}, fmt.Errorf("没有可用账号（都为空或已禁用）")
}

// codec 与 app 包的 storageCodec 规则一致（Windows 上用 DPAPI）。
//
// 这里不 import app：工具不应依赖 app 包（那会把整个服务扯进来）。
func codec() storage.Codec {
	return storage.DPAPICodec{Description: "wbapi 账号凭据"}
}

// compareWithLocal 拉本地 /v1/models 并与上游列表比对。
func compareWithLocal(base, key string, upstreamIDs []string) {
	url := strings.TrimRight(base, "/") + "/v1/models"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "构造请求失败: %v\n", err)
		return
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "请求本地失败: %v\n", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "本地返回 HTTP %d（面板密钥是否正确？）\n", resp.StatusCode)
		return
	}

	var got struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		fmt.Fprintf(os.Stderr, "解析本地响应失败: %v\n", err)
		return
	}

	localIDs := make([]string, 0, len(got.Data))
	for _, m := range got.Data {
		localIDs = append(localIDs, m.ID)
	}
	sort.Strings(localIDs)

	upSet := map[string]bool{}
	for _, id := range upstreamIDs {
		upSet[id] = true
	}
	loSet := map[string]bool{}
	for _, id := range localIDs {
		loSet[id] = true
	}

	fmt.Printf("上游 %d 个 / 本地 %d 个\n", len(upstreamIDs), len(localIDs))

	var missing, extra []string
	for _, id := range upstreamIDs {
		if !loSet[id] {
			missing = append(missing, id)
		}
	}
	for _, id := range localIDs {
		if !upSet[id] {
			extra = append(extra, id)
		}
	}

	if len(missing) == 0 && len(extra) == 0 {
		fmt.Println("✅ 完全一致 —— 本地列表与上游一致，没有过期")
		return
	}
	if len(missing) > 0 {
		fmt.Printf("⚠️ 上游有、本地缺 %d 个：\n", len(missing))
		for _, id := range missing {
			fmt.Printf("   - %s\n", id)
		}
	}
	if len(extra) > 0 {
		fmt.Printf("⚠️ 本地有、上游没有 %d 个（多半是别名或已下线）：\n", len(extra))
		for _, id := range extra {
			fmt.Printf("   + %s\n", id)
		}
	}
}

// probeResult 是单个模型的探测结果。
type probeResult struct {
	ID       string
	OK       bool
	Err      string
	Elapsed  time.Duration
	GotReply bool
}

// probeAll 并发探测每个模型是否真实可用。
func probeAll(prov provider.Provider, ids []string, concurrency int,
	timeout time.Duration) []probeResult {

	out := make([]probeResult, len(ids))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

	for i, id := range ids {
		wg.Add(1)
		go func(idx int, modelID string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			start := time.Now()
			res := probeOne(prov, modelID, timeout)
			res.ID = modelID
			res.Elapsed = time.Since(start)
			out[idx] = res
		}(i, id)
	}
	wg.Wait()
	return out
}

// probeOne 用真实调用路径探测单个模型。
//
// 判定标准：**必须在流里收到 done 事件**才算可用。
// 只看"HTTP 200"不够 —— 上游可能先回 200 再在流里报错
// （例如模型不存在/无权限），那样会被误判为可用。
//
// 🔴 关键：provider.ChatRequest.Model 要的是**去掉渠道前缀**的裸 ID。
//
//	buildChatBody 把 req.Model 直接写进请求体（chat.go:69），
//	而 router 在调用前已经剥掉 workbuddy/ 前缀。
//	我第一版传了带前缀的 ID ⇒ 31 个模型全部报
//	`model [workbuddy/xxx] service info not found`。
//	**那是探测器的 bug，不是模型真不可用** —— 差点得出全错的结论。
func probeOne(prov provider.Provider, modelID string, timeout time.Duration) probeResult {
	upstreamID := strings.TrimPrefix(modelID, "workbuddy/")

	body := map[string]any{
		"model":      upstreamID,
		"stream":     true,
		"max_tokens": 16,
		"messages": []map[string]any{
			{"role": "user", "content": "hi"},
		},
	}
	raw, _ := json.Marshal(body)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	gotDone := false
	gotContent := false
	var firstErr error

	emit := func(ev provider.Event) error {
		switch ev.Type {
		case provider.EventDone:
			gotDone = true
		case provider.EventContent:
			if ev.Text != "" {
				gotContent = true
			}
		}
		return nil
	}

	err := prov.Chat(ctx, provider.ChatRequest{
		Model:   upstreamID, // 裸 ID（不带 workbuddy/ 前缀）
		RawBody: raw,
		Stream:  true,
	}, emit)

	if err != nil {
		firstErr = err
	}

	// 判定：拿到 done（正常结束）即算可用；有内容更佳但不强求
	// （某些模型对 "hi" 只回思考块，那也是可用的）。
	if gotDone {
		return probeResult{OK: true, GotReply: gotContent}
	}
	msg := "未收到 done 事件（流提前中断）"
	if firstErr != nil {
		msg = trimErr(firstErr.Error())
	}
	return probeResult{Err: msg}
}

func report(results []probeResult) {
	var ok, bad []probeResult
	for _, r := range results {
		if r.OK {
			ok = append(ok, r)
		} else {
			bad = append(bad, r)
		}
	}

	fmt.Printf("\n可用 %d / 不可用 %d / 共 %d\n", len(ok), len(bad), len(results))

	if len(ok) > 0 {
		fmt.Println("\n✅ 可用：")
		for _, r := range ok {
			note := ""
			if !r.GotReply {
				note = "（无文本内容，但正常结束）"
			}
			fmt.Printf("   %-32s %5dms %s\n", r.ID, r.Elapsed.Milliseconds(), note)
		}
	}
	if len(bad) > 0 {
		fmt.Println("\n❌ 不可用：")
		for _, r := range bad {
			fmt.Printf("   %-32s %s\n", r.ID, r.Err)
		}
	}
}

func trimErr(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
