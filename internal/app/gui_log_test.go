package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ═══════════════════════════════════════════════════════════════════
// GUI 模式的日志落盘测试
// ═══════════════════════════════════════════════════════════════════
//
// 🔴 为什么必须测它（我实测踩到的坑）：
//
//	GUI 子系统（-H=windowsgui）**没有控制台**，日志若没落盘，
//	用户双击后出任何问题都**完全无从排查**（连"服务起没起"都看不到）。
//
//	实测现象：日志文件被**创建**了（0 字节）却什么都没写进去 ——
//	文件存在会让人误以为"日志功能正常"，比完全没有日志更危险。

// TestGUIloggerWritesToFile 守：openLogFile 返回的 writer 真的写进文件。
func TestGUIloggerWritesToFile(t *testing.T) {
	dir := t.TempDir()
	w, path, err := openLogFile(dir)
	if err != nil {
		t.Fatalf("openLogFile 失败: %v", err)
	}
	if path == "" {
		t.Fatal("应返回日志文件路径")
	}

	if _, err := w.Write([]byte("hello-gui-log\n")); err != nil {
		t.Fatalf("写日志失败: %v", err)
	}
	// 文件型 writer 需要 Sync/Close 才能确保落盘（并释放句柄）
	if c, ok := w.(interface{ Sync() error }); ok {
		_ = c.Sync()
	}
	if c, ok := w.(interface{ Close() error }); ok {
		_ = c.Close()
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取日志失败: %v", err)
	}
	if !strings.Contains(string(got), "hello-gui-log") {
		t.Fatalf("日志内容 = %q，期望包含 hello-gui-log", got)
	}
}

// TestGUIloggerWritesViaLogger 守：**newGUIlogger 构造出的 logger 也写文件**。
//
// 这是实际用的路径（runServeMode 里的 logger = rt.logger）。
//
// 🔴 还必须**调用 cleanup** 关掉文件：否则句柄泄漏，
//
//	且测试的 TempDir 删不掉（"being used by another process"）。
//	这正是本测试第一版暴露出来的真实缺陷。
func TestGUIloggerWritesViaLogger(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wbapi-test.log")

	lg, cleanup := newGUIlogger(path)
	defer cleanup()
	lg.Printf("通过 logger 写的一行")

	// 🔴 **不调 cleanup** 就读：守"每条日志立即落盘"这条性质。
	//
	//	我第一版在这里先 cleanup 再读，于是测试全绿 ——
	//	而生产里**实时看不到日志**（数据留在系统文件缓存，
	//	要等进程退出才刷出）。实测：服务跑 30 秒日志恒 0 字节，
	//	杀进程后同一文件立刻变 699 字节、内容完整。
	//
	//	GUI 模式没有控制台 ⇒ 日志是**唯一**排查手段
	//	⇒ "写完即可见"是硬需求，不是优化。
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取日志失败: %v", err)
	}
	if !strings.Contains(string(got), "通过 logger 写的一行") {
		t.Fatalf("🔴 日志未实时落盘：文件内容 = %q —— "+
			"GUI 模式没有控制台，日志必须写完即可见，"+
			"否则排查时看到的是空文件；而且「探测可写」会误判成失败并弹假告警",
			got)
	}
}

// TestGUIloggerCreatesDir 守：日志目录不存在时自动创建。
func TestGUIloggerCreatesDir(t *testing.T) {
	base := t.TempDir()
	nested := filepath.Join(base, "a", "b", "logs")

	w, path, err := openLogFile(nested)
	if err != nil {
		t.Fatalf("应自动创建目录，却失败: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		t.Fatalf("日志目录未被创建: %v", err)
	}
	if c, ok := w.(interface{ Close() error }); ok {
		_ = c.Close()
	}
}

// TestGUIloggerAppends 守：多次启动**追加**而不是覆盖。
//
// 用户重启服务后，上一次的日志不该消失（排查往往要看"上次怎么挂的"）。
func TestGUIloggerAppends(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wbapi-test.log")

	lg1, clean1 := newGUIlogger(path)
	lg1.Printf("第一次启动")
	clean1()

	lg2, clean2 := newGUIlogger(path)
	lg2.Printf("第二次启动")
	clean2()

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if !strings.Contains(s, "第一次启动") || !strings.Contains(s, "第二次启动") {
		t.Fatalf("日志应追加两次启动的记录，实际 = %q", s)
	}
}

// TestNewGUIloggerHandlesBadPath 守：路径不可写时**不 panic**，退化为 stderr。
//
// 理由：日志写不了不该让服务起不来（那会把"能跑"变成"完全不能用"）。
func TestNewGUIloggerHandlesBadPath(t *testing.T) {
	// Windows 上非法路径（含 NUL 的路径必定失败）
	lg, cleanup := newGUIlogger("bad\x00path/log.txt")
	defer cleanup()
	if lg == nil {
		t.Fatal("不应返回 nil logger")
	}
	lg.Printf("这条应写到 stderr 而不是崩溃")
}

// TestGUIloggerCleanupReleasesFile 守：cleanup 真的释放文件占用。
//
// 🔴 这是上面那个泄漏缺陷的**直接护栏**：不调 cleanup 时，
//
//	Windows 不允许删除已被打开的文件 —— 用它来证明"确实关掉了"。
func TestGUIloggerCleanupReleasesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wbapi-lock.log")

	lg, cleanup := newGUIlogger(path)
	lg.Printf("占住文件")
	cleanup()

	// cleanup 之后再删必须成功（没释放的话 Windows 会报占用）
	if err := os.Remove(path); err != nil {
		t.Fatalf("🔴 cleanup 后文件仍被占用（句柄泄漏）: %v", err)
	}
}

// TestGUIloggerCreatesMissingDirs 守：**父目录不存在时也能写日志**。
//
// 🔴 这是"换个文件夹就必现"的真实缺陷（2026-10-07 委托方实测）：
//
//	把 exe 单独复制到一个空文件夹里双击运行 ⇒ `data/logs/` 尚不存在
//	⇒ `os.OpenFile` 失败（The system cannot find the path specified）
//	⇒ 弹出"无法写入日志文件"的**模态**告警框 ⇒ **卡住整个启动**
//	（服务没起来、目录也没被创建）。
//	用户看到的就是"双击后弹个框，然后什么都没有"。
//
// ⇒ newGUIlogger 必须自己 MkdirAll，不能指望调用方先建好。
func TestGUIloggerCreatesMissingDirs(t *testing.T) {
	base := t.TempDir()
	// 模拟"全新文件夹"：多层不存在的路径
	deep := filepath.Join(base, "fresh", "data", "logs", "wbapi-x.log")

	lg, cleanup := newGUIlogger(deep)
	defer cleanup()
	lg.Printf("全新文件夹里的第一行日志")

	if _, err := os.Stat(deep); err != nil {
		t.Fatalf("🔴 父目录不存在时没能创建日志文件: %v", err)
	}
	got, err := os.ReadFile(deep)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if !strings.Contains(string(got), "全新文件夹里的第一行日志") {
		t.Fatalf("日志内容 = %q，期望含写入的那行", got)
	}
}

// TestGUILogWritableJudgement 守"日志是否可写"的判据。
//
// 🔴 判据必须基于**内容**，而不是 os.Stat 是否报错：
//
//	我第一版用 `os.Stat(logPath)` 判错，于是"父目录还没建"被误判成
//	"日志不可写" ⇒ 假告警 + 模态框阻塞启动。真正该问的是
//	"写进去的东西能不能读回来"。
func TestGUILogWritableJudgement(t *testing.T) {
	dir := t.TempDir()

	// ① 不存在的文件 ⇒ 不可写
	if guiLogWritable(filepath.Join(dir, "nope.log")) {
		t.Error("不存在的文件应判为不可写")
	}

	// ② 空文件 ⇒ 不可写（写了但没内容，等于没日志）
	empty := filepath.Join(dir, "empty.log")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if guiLogWritable(empty) {
		t.Error("空文件应判为不可写")
	}

	// ③ 有内容 ⇒ 可写
	ok := filepath.Join(dir, "ok.log")
	if err := os.WriteFile(ok, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !guiLogWritable(ok) {
		t.Error("有内容的文件应判为可写")
	}
}
