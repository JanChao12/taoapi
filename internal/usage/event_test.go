package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(t.TempDir())
}

// TestAppendAndRead 验证写入后能读回。
func TestAppendAndRead(t *testing.T) {
	s := newTestStore(t)

	credit := 0.25
	ev := Event{
		Time:             time.Now(),
		Account:          "acct1",
		Model:            "workbuddy/space-bunny",
		Protocol:         "chat",
		OK:               true,
		PromptTokens:     10,
		CompletionTokens: 20,
		TotalTokens:      30,
		ReasoningTokens:  15,
		Credit:           &credit,
	}
	if err := s.Append(ev); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	var got []Event
	if err := s.Read(1, func(e Event) error {
		got = append(got, e)
		return nil
	}); err != nil {
		t.Fatalf("读取失败: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("读到 %d 条，期望 1", len(got))
	}
	g := got[0]
	if g.Model != ev.Model {
		t.Errorf("model = %q", g.Model)
	}
	if g.TotalTokens != 30 {
		t.Errorf("total_tokens = %d", g.TotalTokens)
	}
	if g.Credit == nil || *g.Credit != 0.25 {
		t.Errorf("credit 丢失: %v", g.Credit)
	}
}

// TestCreditNilPreserved 是【关键测试】：
// 上游未返回 credit 时必须保持 nil，与"返回 0"区分开。
func TestCreditNilPreserved(t *testing.T) {
	s := newTestStore(t)

	if err := s.Append(Event{Time: time.Now(), Model: "m", OK: true, Credit: nil}); err != nil {
		t.Fatal(err)
	}

	var got *Event
	_ = s.Read(1, func(e Event) error {
		got = &e
		return nil
	})
	if got == nil {
		t.Fatal("没读到事件")
	}
	if got.Credit != nil {
		t.Errorf("credit 应为 nil（上游未返回），实际 %v —— 不能用 0 冒充", *got.Credit)
	}
}

// TestAppendMultiple 验证追加多条。
func TestAppendMultiple(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < 50; i++ {
		if err := s.Append(Event{Time: time.Now(), Model: "m", OK: true}); err != nil {
			t.Fatal(err)
		}
	}

	n := 0
	_ = s.Read(1, func(Event) error { n++; return nil })
	if n != 50 {
		t.Errorf("读到 %d 条，期望 50", n)
	}
}

// TestReadSkipsMissingDays 验证没有数据的日期不报错。
func TestReadSkipsMissingDays(t *testing.T) {
	s := newTestStore(t)
	if err := s.Read(7, func(Event) error { return nil }); err != nil {
		t.Errorf("没有数据时读取不应报错: %v", err)
	}
}

// TestReadToleratesTruncatedLastLine 验证残缺的最后一行被跳过。
//
// 场景：进程在写入中途被杀，文件最后一行是半截 JSON。
func TestReadToleratesTruncatedLastLine(t *testing.T) {
	s := newTestStore(t)
	if err := s.Append(Event{Time: time.Now(), Model: "good", OK: true}); err != nil {
		t.Fatal(err)
	}

	// 手工追加一行残缺 JSON
	path := filepath.Join(s.Dir(), fileName(time.Now()))
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"model":"truncated","ok":`)
	_ = f.Close()

	var models []string
	if err := s.Read(1, func(e Event) error {
		models = append(models, e.Model)
		return nil
	}); err != nil {
		t.Fatalf("残缺行不应让读取失败: %v", err)
	}
	if len(models) != 1 || models[0] != "good" {
		t.Errorf("应只读到完整的那条，实际 %v", models)
	}
}

// TestReadDaysFilters 验证按天数过滤。
func TestReadDaysFilters(t *testing.T) {
	s := newTestStore(t)

	// 直接写昨天与前天的文件
	old := time.Now().AddDate(0, 0, -5)
	if err := s.Append(Event{Time: old, Model: "old", OK: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(Event{Time: time.Now(), Model: "today", OK: true}); err != nil {
		t.Fatal(err)
	}

	var models []string
	_ = s.Read(1, func(e Event) error { models = append(models, e.Model); return nil })
	if len(models) != 1 || models[0] != "today" {
		t.Errorf("读 1 天应只有 today，实际 %v", models)
	}

	models = nil
	_ = s.Read(7, func(e Event) error { models = append(models, e.Model); return nil })
	if len(models) != 2 {
		t.Errorf("读 7 天应有 2 条，实际 %v", models)
	}
}

// TestFileNameFormat 验证文件命名。
func TestFileNameFormat(t *testing.T) {
	d := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	if got := fileName(d); got != "2026-10-04.jsonl" {
		t.Errorf("fileName = %q", got)
	}
}

// TestReadPropagatesCallbackError 验证回调错误被传出。
func TestReadPropagatesCallbackError(t *testing.T) {
	s := newTestStore(t)
	_ = s.Append(Event{Time: time.Now(), Model: "m", OK: true})

	sentinel := os.ErrClosed
	err := s.Read(1, func(Event) error { return sentinel })
	if err != sentinel {
		t.Errorf("回调错误应原样返回，实际 %v", err)
	}
}

// TestReadIgnoresTruncatedTrailingLine 守：JSONL 末尾的半条记录不影响读取。
//
// 🔴 为什么这条是**读取层的最后一道防线**（Codex 第 14 轮要求）：
//
//	写入用"单次 Write + O_APPEND"，且有跨进程文件锁 —— 但进程被强杀、
//	断电、或将来有人改动写入方式时，仍可能留下**半条记录**。
//	若读取端因此让整个 /api/stats 失败，用户会看到"统计坏了"，
//	而实际上历史数据完好，只有最后一条损坏。
//
//	当前实现用 json.Decoder 逐条解码，遇到解析错误就 break（不是 return err），
//	即"跳过残缺尾部、保留已解析的部分"。这个测试把它钉住。
func TestReadIgnoresTruncatedTrailingLine(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	for i := 0; i < 2; i++ {
		if err := s.Append(Event{Time: time.Now(), Model: "m", OK: true,
			TotalTokens: 10}); err != nil {
			t.Fatal(err)
		}
	}

	// 手工追加一条**残缺**记录（模拟进程被强杀时写到一半）
	path := filepath.Join(dir, fileName(time.Now()))
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"time":"2026-10-05T00:00:00Z","model":"trunc`); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	var got []Event
	if err := s.Read(1, func(ev Event) error {
		got = append(got, ev)
		return nil
	}); err != nil {
		t.Fatalf("末尾有残缺记录时读取不应失败: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("读到 %d 条，期望 2 条（残缺的那条应被跳过而不是让整体失败）",
			len(got))
	}
}

// TestReadKeepsRecordsBeforeCorruptLine 守：中间某行损坏时，
// 坏行**之前**的记录仍完整返回。
//
// 当前实现遇到坏行会 break，所以坏行之后的记录读不到。
// 这是有意的取舍 —— 与其"跳过坏行继续读"（可能把乱序数据当正常统计），
// 不如停在坏行，让人能看出"数据在某处断了"。
func TestReadKeepsRecordsBeforeCorruptLine(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	if err := s.Append(Event{Time: time.Now(), Model: "before", OK: true}); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, fileName(time.Now()))
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{ this is not json }\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	if err := s.Append(Event{Time: time.Now(), Model: "after", OK: true}); err != nil {
		t.Fatal(err)
	}

	var got []Event
	if err := s.Read(1, func(ev Event) error {
		got = append(got, ev)
		return nil
	}); err != nil {
		t.Fatalf("中间有坏行时读取不应失败: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("坏行之前的记录必须被返回")
	}
	if got[0].Model != "before" {
		t.Errorf("第一条 = %q，期望 before", got[0].Model)
	}
}

// TestReadResultReportsTruncation 守：截断必须被**显式报出**。
//
// 🔴 Codex 第 17 轮要求：读取端遇到坏行会停止（fail-closed 取舍，可接受），
// 但前提是"读取结果带 partial/warning 状态，面板不能把部分统计显示成完整"。
//
// 没有这个标志时，用户只会看到数字变小而不知道原因 —— 比报错更误导。
func TestReadResultReportsTruncation(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	if err := s.Append(Event{Time: time.Now(), Model: "ok1", OK: true}); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, fileName(time.Now()))
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"time":"2026-10-05T00:00:00Z","model":"trunc`); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	var got []Event
	rr, err := s.ReadResult(1, func(ev Event) error {
		got = append(got, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("读取不应失败: %v", err)
	}
	if !rr.Truncated {
		t.Error("存在残缺记录时 Truncated 必须为 true（否则面板会把部分统计当完整）")
	}
	if rr.TruncatedAt == "" {
		t.Error("应报告截断发生在哪个文件")
	}
	if rr.Events != len(got) {
		t.Errorf("Events 计数 = %d，与实际回调 %d 次不符", rr.Events, len(got))
	}
	if len(got) != 1 {
		t.Errorf("应读到 1 条完整记录，实际 %d 条", len(got))
	}
}

// TestReadResultCleanWhenNoTruncation 守：完整数据时不能误报截断。
//
// 误报会让面板一直显示"数据可能不完整"，同样误导。
func TestReadResultCleanWhenNoTruncation(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	for i := 0; i < 3; i++ {
		if err := s.Append(Event{Time: time.Now(), Model: "m", OK: true}); err != nil {
			t.Fatal(err)
		}
	}

	rr, err := s.ReadResult(1, func(Event) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if rr.Truncated {
		t.Error("完整数据不应报告 Truncated —— 误报同样误导用户")
	}
	if rr.Events != 3 {
		t.Errorf("Events = %d，期望 3", rr.Events)
	}
}

// TestAppendIsSingleWritePerEvent 守：每条事件只写一次、且以换行结尾。
//
// 🔴 为什么这条重要（Codex 第 10 轮提出"重启期间父子进程同时写会交错"）：
//
//	实测（本机，6 个真实进程 × 400 行）证明 O_APPEND 句柄的**单次 Write**
//	在 Windows 上近似原子，行不会互相穿插，所以跨进程写是安全的。
//	但该结论的**前提是"一行一次 Write"** —— 若有人改成多次写
//	（例如引入 bufio、或先写内容再写换行），前提立刻失效，
//	必须改为加进程级文件锁。
//
//	这个测试用"读回的行数 == 写入次数 且 每行都是完整 JSON"来守住前提：
//	一旦变成多段写，并发下就会出现半行，这里会红。
func TestAppendIsSingleWritePerEvent(t *testing.T) {
	s := newTestStore(t)

	const n = 200
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < n/4; i++ {
				// 用较长的字段，让"半行"更容易暴露
				_ = s.Append(Event{
					Time:   time.Now(),
					Model:  "workbuddy/deepseek-v4.1-flash",
					OK:     true,
					Status: StatusOK,
					Error:  strings.Repeat("x", 200),
				})
			}
		}(w)
	}
	wg.Wait()

	raw, err := os.ReadFile(filepath.Join(s.Dir(), fileName(time.Now())))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != n {
		t.Errorf("行数 = %d，期望 %d（多段写会导致行被拆开或合并）", len(lines), n)
	}
	for i, l := range lines {
		var ev Event
		if err := json.Unmarshal([]byte(l), &ev); err != nil {
			t.Fatalf("第 %d 行不是完整 JSON（说明写入被拆分/交错）: %v\n行内容: %.120s",
				i, err, l)
		}
	}
}
