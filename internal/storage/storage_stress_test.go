// storage_stress_test.go —— 原子写入的 Windows 并发压力诊断
//
// ═══════════════════════════════════════════════════════════════════
// 用途与定位（Codex 第 22 轮建议）
// ═══════════════════════════════════════════════════════════════════
//
// 背景：2026-10-05 验证中曾观察到 **1 次**
// `TestAtomicWriteIsAtomicUnderConcurrency` 失败，但此后单测 12 轮、
// 整包 6 轮、全量 6 轮均**未能复现**，因此无法确认成因。
//
// Codex 的建议原话：
//
//	"建议保留一次独立压力命令，例如提高并发和循环次数，记录是否出现
//	  rename 重试耗尽；它作为诊断测试，不要用测试重试掩盖失败。
//	  若同一错误再次出现，应重新打开阻塞项。"
//
// 所以本文件是**诊断**，不是常规回归：
//   - 默认**跳过**，不拖慢日常 `go test ./...`
//   - 显式开启才跑，且并发/轮次都拉到远超常规测试
//   - 失败信息里**明确区分**"是 rename 重试预算用尽"还是"别的错误"
//     —— 这正是当初取不到的关键信息
//
// 开启方式：
//
//	$env:WBAPI_RUN_STORAGE_STRESS="1"
//	go test -count=1 -run TestStorageStress -v ./internal/storage/
//
// 可调参数（环境变量）：
//
//	WBAPI_STRESS_GOROUTINES  并发写者数（默认 16，常规测试是 8）
//	WBAPI_STRESS_PER_G       每个写者的写入次数（默认 400，常规是 200）
package storage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// stressEnabled 判断是否显式开启压力诊断。
func stressEnabled() bool { return stressEnabledVar("WBAPI_RUN_STORAGE_STRESS") }

// stressEnabledVar 判断某个环境变量是否被显式设为真。
func stressEnabledVar(env string) bool {
	v := os.Getenv(env)
	return v == "1" || strings.EqualFold(v, "true")
}

func stressInt(env string, def int) int {
	if s := os.Getenv(env); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// TestStorageStressAtomicWriteUnderHeavyConcurrency 是**诊断用**的重度并发压测。
//
// 默认跳过；开启后跑 goroutines × perG 次原子写入，统计：
//   - 总写入次数、失败次数、失败率
//   - 失败是否集中在"rename 重试耗尽"（本诊断最关心的那一类）
//   - 耗时与每次写入的平均耗时
//
// 🔴 关键设计：失败时**不**只说"失败了"，而是把**错误分类**打出来。
// 当初就是因为拿不到失败信息，才无法判断成因（是不是重试预算用尽）。
func TestStorageStressAtomicWriteUnderHeavyConcurrency(t *testing.T) {
	if !stressEnabled() {
		t.Skip("压力诊断默认跳过；设 WBAPI_RUN_STORAGE_STRESS=1 开启" +
			"（这**不是**常规回归测试，见文件头说明）")
	}

	goroutines := stressInt("WBAPI_STRESS_GOROUTINES", 16)
	perG := stressInt("WBAPI_STRESS_PER_G", 400)

	dir := testDir(t)
	target := filepath.Join(dir, "stress.json")

	type failure struct {
		err  error
		g, i int
	}
	var (
		mu       sync.Mutex
		failures []failure
	)

	start := time.Now()
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				payload := []byte(fmt.Sprintf(`{"g":%d,"i":%d}`, g, i))
				if err := AtomicWrite(target, payload, 0o600); err != nil {
					mu.Lock()
					failures = append(failures, failure{err: err, g: g, i: i})
					mu.Unlock()
				}
			}
		}(g)
	}
	wg.Wait()
	elapsed := time.Since(start)

	total := goroutines * perG
	t.Logf("压力结果：%d 个写者 × %d 次 = %d 次原子写入，耗时 %v（平均 %v/次）",
		goroutines, perG, total, elapsed.Round(time.Millisecond),
		(elapsed / time.Duration(total)).Round(time.Microsecond))

	if len(failures) == 0 {
		t.Log("✅ 无失败：本次未能复现，rename 重试预算在压测下未用尽")
		return
	}

	// ── 有失败：分类并打印，这正是当初缺的信息 ──
	//
	// 分类依据是本项目的重试实现（file.go 的 renameWithRetry）：
	// 只有"目标被占用"类错误才重试（errno 5/32 或文本兜底），
	// 重试 renameRetries 次后仍失败才返回。
	var exhausted, other []failure
	for _, f := range failures {
		msg := strings.ToLower(f.err.Error())
		isBusy := strings.Contains(msg, "access is denied") ||
			strings.Contains(msg, "sharing violation") ||
			strings.Contains(msg, "being used by another process")
		if isBusy {
			exhausted = append(exhausted, f)
		} else {
			other = append(other, f)
		}
	}

	t.Errorf("压力下出现 %d/%d 次失败（%.4f%%）",
		len(failures), total, float64(len(failures))*100/float64(total))
	t.Errorf("  ├─ 「目标被占用」类（= rename 重试 %d 次 × %v 后仍失败）：%d 次",
		renameRetries, renameRetryDelay, len(exhausted))
	t.Errorf("  └─ 其它错误：%d 次", len(other))

	for _, f := range append(exhausted, other...) {
		t.Errorf("     写者 g=%d 第 %d 次：%v", f.g, f.i, f.err)
		if errors.Is(f.err, os.ErrPermission) {
			t.Errorf("       （errors.Is os.ErrPermission 为真）")
		}
	}

	if len(exhausted) > 0 {
		t.Errorf("🔴 结论：**rename 重试预算被用尽** —— "+
			"当前是 %d 次 × %v。若确认是这个成因，"+
			"应考虑提高预算或改用别的退避策略（**但需先有此证据**，"+
			"不要凭猜测调参）。", renameRetries, renameRetryDelay)
	} else {
		t.Errorf("🔴 结论：失败**不是**重试预算用尽，属另一类错误，需要单独定位。")
	}
}
