//go:build windows

package storage

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestRenameRetryBudgetAdequacy 是**实验性诊断**（默认跳过），
// 用来回答一个具体问题：**当前 10 次 × 5ms 的重试预算够不够？**
//
// ═══════════════════════════════════════════════════════════════════
// 背景（2026-10-05 的实测链条）
// ═══════════════════════════════════════════════════════════════════
//
//  1. 全量 `go test ./...` 偶发失败，报
//     `rename ...: Access is denied`（测试名每次不同，单独跑必过）；
//  2. 我曾**未能复现**，因此没有证据、也没动生产参数（正确做法）；
//  3. 后加压力诊断后**复现**：32 写者 × 600 次 → 稳定出现若干次失败，
//     且 **100% 是"目标被占用"类**（其它错误 0 次）；
//  4. 剂量-反应关系实测：并发 8 → 0 次；16 → 0 次；32 → 有失败。
//
// 于是问题从"是不是 rename 重试的锅"收敛为：
// **是重试预算（次数/间隔）不足，还是本就不该靠重试？**
//
// ═══════════════════════════════════════════════════════════════════
// 本实验做什么
// ═══════════════════════════════════════════════════════════════════
//
// 在**同一并发压力**下，分别用不同的重试预算跑，比较失败数。
// 这能直接回答"加预算是否有效"，而不是凭直觉调参。
//
// 开启：$env:WBAPI_RUN_RENAME_BUDGET="1"
//
// 注意：这是**实验**，不是回归测试 —— 它注入自己的重试参数，
// 不修改生产常量，也不断言"应该成功"（预算可能本就不该无限加）。
func TestRenameRetryBudgetAdequacy(t *testing.T) {
	if !stressEnabledVar("WBAPI_RUN_RENAME_BUDGET") {
		t.Skip("实验性诊断，默认跳过；设 WBAPI_RUN_RENAME_BUDGET=1 开启")
	}

	const (
		goroutines = 32
		perG       = 600
	)

	// 被测的几档预算（次数, 间隔）
	budgets := []struct {
		retries int
		delay   time.Duration
	}{
		{10, 5 * time.Millisecond},  // 当前生产值
		{30, 5 * time.Millisecond},  // 只加次数
		{10, 20 * time.Millisecond}, // 只加间隔
		{50, 20 * time.Millisecond}, // 两者都加
	}

	for _, b := range budgets {
		dir := testDir(t)
		target := filepath.Join(dir, "budget.json")

		var (
			mu    sync.Mutex
			fails int
		)

		var wg sync.WaitGroup
		start := time.Now()
		for g := 0; g < goroutines; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				for i := 0; i < perG; i++ {
					payload := []byte(fmt.Sprintf(`{"g":%d,"i":%d}`, g, i))
					// 用**指定预算**走同一条 rename 重试控制流
					if err := atomicWriteWithBudget(target, payload, 0o600, b.retries, b.delay); err != nil {
						mu.Lock()
						fails++
						mu.Unlock()
					}
				}
			}(g)
		}
		wg.Wait()
		elapsed := time.Since(start)

		total := goroutines * perG
		t.Logf("预算 %2d 次 × %-6v → 失败 %4d / %d（%.4f%%），耗时 %v",
			b.retries, b.delay, fails, total,
			float64(fails)*100/float64(total), elapsed.Round(time.Millisecond))
	}
}
