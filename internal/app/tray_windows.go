//go:build windows

// tray_windows.go：托盘图标 + 右键菜单 + 左键打开主界面。
//
// ═══════════════════════════════════════════════════════════════════
// 为什么用 syscall 直调 Win32，而不是 getlantern/systray
// ═══════════════════════════════════════════════════════════════════
//
//	本项目硬约束是**零第三方依赖 + 离线可构建**，CI 里还有断言守着。
//	引进 systray 会连带 `golang.org/x/sys` 等依赖，破坏：
//	  · 离线构建（要 vendor 进仓库）
//	  · "自持"约束（不被第三方项目更新/删库连累）
//	  · CI 的零依赖断言
//
//	代价是要自己写消息循环与 Shell_NotifyIcon 调用（本文件），
//	但换来约束不被破坏 —— 与 storage 包用 syscall 而非 cgo 是同一取舍。
//
// ═══════════════════════════════════════════════════════════════════
// 🔴 三个必须记住的实现要点
// ═══════════════════════════════════════════════════════════════════
//
//  1. **托盘图标必须从 RT_GROUP_ICON 资源加载**（LoadImage），
//     不能只 LoadIcon —— 多尺寸图标要靠 group 资源让 Windows 挑尺寸。
//     ⚠️ 若 .ico 是 PNG 压缩条目，LoadImage 会返回句柄却**渲染成坏图**
//     （实测过），必须先把图标转成 DIB。本项目尚未接入图标资源，
//     所以当前用 `LoadIcon(NULL, IDI_APPLICATION)` 作为**占位图标**，
//     保证功能可用；接入自定义图标后只需改 loadTrayIcon。
//
//  2. **消息循环必须在主 goroutine 之前启动**：Windows 的
//     Shell_NotifyIcon 要求创建窗口的线程能持续泵消息。
//     这里**不建窗口**，而是用隐藏窗口 + 消息泵（见 runMessageLoop）。
//     托盘回调（WM_APP+1）也投递到这个窗口。
//
//  3. **左键 = 打开主界面，右键 = 弹菜单**（委托方明确要求）。
//     Windows 的托盘约定是"右键弹菜单"，所以左键行为要自己实现：
//     拦截 WM_LBUTTONUP，直接执行"打开主界面"。
package app

import (
	"fmt"
	"log"
	"os/exec"
	"syscall"
	"unsafe"
)

// ── Win32 常量 ────────────────────────────────────────────

const (
	wmDestroy    = 0x0002
	wmClose      = 0x0010
	wmCommand    = 0x0111
	wmLButtonUp  = 0x0202
	wmLButtonDbl = 0x0203
	wmRButtonUp  = 0x0205
	wmApp        = 0x8000 // WM_APP 起始
	wmTrayNotify = wmApp + 1

	// 托盘回调消息
	nimAdd    = 0x00000000
	nimModify = 0x00000001
	nimDelete = 0x00000002

	// NOTIFYICON_VERSION_4 提供更好的行为（含 WM_CONTEXTMENU 等）
	nimSetVersion = 0x00000004
	notifyIconV4  = 4

	// NOTIFYICONDATA 的 uFlags
	nifMessage = 0x00000001
	nifIcon    = 0x00000002
	nifTip     = 0x00000004
	nifInfo    = 0x00000010

	// 图标加载
	imageIcon = 1
	idcArrow  = 32512 // IDI_APPLICATION
	idcInfo   = 32516 // IDI_INFORMATION

	// 菜单
	mfString    = 0x00000000
	mfSeparator = 0x00000800
	mfGrayed    = 0x00000001

	tpmRightButton = 0x0002
	tpmNonotify    = 0x0080 // 不把选择结果当 WM_COMMAND 发回窗口

	// 菜单项 ID
	menuIDOpen = 1
	menuIDLogs = 2
	menuIDQuit = 3

	// 气泡提示
	niifInfo       = 0x00000001
	niifNoSound    = 0x00000010
	niifRespectQui = 0x00000020
)

// ── Win32 结构体 ──────────────────────────────────────────

// wndClassExW 是窗口类（注册隐藏窗口用）。
type wndClassExW struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     uintptr
	hIcon         uintptr
	hCursor       uintptr
	hbrBackground uintptr
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       uintptr
}

// msgT 是 MSG。
type msgT struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	ptX     int32
	ptY     int32
}

// notifyIconDataW 是 NOTIFYICONDATAW。
//
// ⚠️ 字段顺序与大小必须与 Win32 完全一致（含 cbSize 校验）。
// Vista+ 的版本在末尾多了 union（uTimeout/uVersion），
// 这里按 "完整结构" 声明，cbSize 传完整大小。
type notifyIconDataW struct {
	cbSize           uint32
	hWnd             uintptr
	uID              uint32
	uFlags           uint32
	uCallbackMessage uint32
	hIcon            uintptr
	szTip            [128]uint16
	dwState          uint32
	dwStateMask      uint32
	szInfo           [256]uint16
	uVersion         uint32
	szInfoTitle      [64]uint16
	dwInfoFlags      uint32
	guidItem         [16]byte
	hBalloonIcon     uintptr
}

// ── Win32 API ─────────────────────────────────────────────

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procRegisterClassExW = user32.NewProc("RegisterClassExW")
	procCreateWindowExW  = user32.NewProc("CreateWindowExW")
	procDefWindowProcW   = user32.NewProc("DefWindowProcW")
	procGetMessageW      = user32.NewProc("GetMessageW")
	procTranslateMessage = user32.NewProc("TranslateMessage")
	procDispatchMessageW = user32.NewProc("DispatchMessageW")
	procPostQuitMessage  = user32.NewProc("PostQuitMessage")
	procPostMessageW     = user32.NewProc("PostMessageW")
	procDestroyWindow    = user32.NewProc("DestroyWindow")
	procLoadIconW        = user32.NewProc("LoadIconW")
	procLoadImageW       = user32.NewProc("LoadImageW")
	procCreatePopupMenu  = user32.NewProc("CreatePopupMenu")
	procAppendMenuW      = user32.NewProc("AppendMenuW")
	procDestroyMenu      = user32.NewProc("DestroyMenu")
	procTrackPopupMenu   = user32.NewProc("TrackPopupMenu")
	procGetCursorPos     = user32.NewProc("GetCursorPos")
	procSetForegroundWin = user32.NewProc("SetForegroundWindow")
	procMessageBoxW      = user32.NewProc("MessageBoxW")

	procShellNotifyIconW = shell32.NewProc("Shell_NotifyIconW")

	procGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")
)

// pointT 是 POINT。
type pointT struct{ x, y int32 }

// trayState 是托盘的运行状态（全局，因为 WndProc 是 C 回调风格）。
type trayState struct {
	logger  *log.Logger
	baseURL string // 打开面板用，如 http://127.0.0.1:8787
	logsDir string // "查看日志"要打开的目录
	quitFn  func() // 点"退出"时调用

	hwnd    uintptr
	iconAdd bool
	tip     string
}

var gTray *trayState

// trayCallbacks 是托盘消息处理表的**回调指针**。
//
// ⚠️ 必须保存成包级变量：syscall.NewCallback 返回的地址若被 GC 回收，
// 消息一来就会崩。这是 Go 调 Win32 的经典坑。
var (
	wndProcCallback uintptr
	// 消息循环退出信号（tray 停止时关闭）
	trayDoneCh chan struct{}
)

// trayOptions 是启动托盘的参数。
type trayOptions struct {
	Logger  *log.Logger
	BaseURL string // 例：http://127.0.0.1:8787
	LogsDir string
	// OnQuit 用户点托盘"退出"时调用（应触发服务优雅关闭）
	OnQuit func()
	// Tip 鼠标悬停提示文字
	Tip string
}

// startTray 启动托盘（在**独立 OS 线程**上跑消息循环）。
//
// 🔴 必须在独立线程：Windows 要求"创建托盘图标的线程"自己泵消息，
// 而主线程被服务阻塞住（select 等信号）。Go 的 goroutine 会在线程间
// 迁移，所以要用 runtime.LockOSThread 锁一个线程给消息循环。
//
// 返回一个 stop 函数：调用它会移除托盘图标并结束消息循环。
func startTray(opt trayOptions) (stop func(), err error) {
	st := &trayState{
		logger:  opt.Logger,
		baseURL: opt.BaseURL,
		logsDir: opt.LogsDir,
		quitFn:  opt.OnQuit,
		tip:     opt.Tip,
	}
	if st.tip == "" {
		st.tip = "TAOAPI"
	}

	ready := make(chan error, 1)
	trayDoneCh = make(chan struct{})

	go func() {
		// 锁线程：消息循环必须始终在同一个 OS 线程上
		locked := lockOSThread()
		defer locked()

		if e := st.run(ready); e != nil {
			select {
			case ready <- e:
			default:
			}
		}
	}()

	if e := <-ready; e != nil {
		return nil, e
	}
	return func() {
		if st.hwnd != 0 {
			procPostMessageW.Call(st.hwnd, wmClose, 0, 0)
		}
		<-trayDoneCh
	}, nil
}

// run 在已锁定的线程上建隐藏窗口、加托盘图标、跑消息循环。
func (st *trayState) run(ready chan error) error {
	gTray = st

	hInst, _, _ := procGetModuleHandleW.Call(0)

	className := utf16Ptr("TAOAPITrayWnd")
	wndProcCallback = syscall.NewCallback(trayWndProc)

	wc := wndClassExW{
		cbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		lpfnWndProc:   wndProcCallback,
		hInstance:     hInst,
		lpszClassName: className,
	}
	if ret, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); ret == 0 {
		ready <- fmt.Errorf("RegisterClassExW 失败: %v", err)
		return nil
	}

	// 隐藏窗口（0 尺寸、无样式）：只用来收托盘消息
	hwnd, _, err := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(utf16Ptr("TAOAPI"))),
		0, 0, 0, 0, 0,
		0, 0, hInst, 0,
	)
	if hwnd == 0 {
		ready <- fmt.Errorf("CreateWindowExW 失败: %v", err)
		return nil
	}
	st.hwnd = hwnd

	if e := st.addIcon(); e != nil {
		ready <- e
		return nil
	}

	ready <- nil // 通知调用方：托盘已就绪

	// 消息循环
	var msg msgT
	for {
		ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(ret) <= 0 { // 0 = WM_QUIT, -1 = 错误
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
	}

	st.removeIcon()
	close(trayDoneCh)
	return nil
}

// addIcon 把图标加进通知区。
func (st *trayState) addIcon() error {
	nid := notifyIconDataW{
		cbSize:           uint32(unsafe.Sizeof(notifyIconDataW{})),
		hWnd:             st.hwnd,
		uID:              1,
		uFlags:           nifMessage | nifIcon | nifTip,
		uCallbackMessage: wmTrayNotify,
		hIcon:            loadTrayIcon(),
	}
	copyUTF16(nid.szTip[:], st.tip)

	ret, _, err := procShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(&nid)))
	if ret == 0 {
		return fmt.Errorf("Shell_NotifyIcon(NIM_ADD) 失败: %v", err)
	}
	st.iconAdd = true

	// 声明为 V4 版本（更可靠的行为；失败不致命）
	nid.uVersion = notifyIconV4
	procShellNotifyIconW.Call(nimSetVersion, uintptr(unsafe.Pointer(&nid)))
	return nil
}

// removeIcon 从通知区移除图标（退出前必须调用，否则留下"幽灵图标"）。
func (st *trayState) removeIcon() {
	if !st.iconAdd {
		return
	}
	nid := notifyIconDataW{
		cbSize: uint32(unsafe.Sizeof(notifyIconDataW{})),
		hWnd:   st.hwnd,
		uID:    1,
	}
	procShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(&nid)))
	st.iconAdd = false
}

// loadTrayIcon 加载托盘图标。
//
// 🔴 当前是**占位图标**（系统默认信息图标）：
//
//	自定义 .ico 需要嵌进 exe 资源段（.syso），而那部分尚未完成
//	（见 tools/genico；PNG 条目渲染问题还没解决）。
//	接入后把这里换成 `LoadImage(hInst, MAKEINTRESOURCE(1), IMAGE_ICON, 0,0, LR_DEFAULTSIZE|LR_SHARED)`
//	即可 —— 其余托盘逻辑不用动。
func loadTrayIcon() uintptr {
	h, _, _ := procLoadIconW.Call(0, idcInfo)
	if h != 0 {
		return h
	}
	// 兜底：连系统信息图标都拿不到（极罕见），用应用程序图标
	h, _, _ = procLoadIconW.Call(0, idcArrow)
	return h
}

// trayWndProc 是隐藏窗口的消息处理。
func trayWndProc(hwnd uintptr, msg uint32, wParam, lParam uintptr) uintptr {
	switch msg {
	case wmTrayNotify:
		// lParam 低 16 位是鼠标消息
		switch uint32(lParam) & 0xFFFF {
		case wmLButtonUp, wmLButtonDbl:
			// 🔴 委托方要求：**左键直接进主界面**（不是弹菜单）
			if gTray != nil {
				gTray.openPanel()
			}
		case wmRButtonUp:
			if gTray != nil {
				gTray.showMenu()
			}
		}
		return 0

	case wmCommand:
		switch uint32(wParam) & 0xFFFF {
		case menuIDOpen:
			if gTray != nil {
				gTray.openPanel()
			}
		case menuIDLogs:
			if gTray != nil {
				gTray.openLogs()
			}
		case menuIDQuit:
			if gTray != nil && gTray.quitFn != nil {
				gTray.quitFn()
			}
			// 让消息循环退出
			procDestroyWindow.Call(hwnd)
		}
		return 0

	case wmClose:
		procDestroyWindow.Call(hwnd)
		return 0

	case wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	}
	ret, _, _ := procDefWindowProcW.Call(hwnd, uintptr(msg), wParam, lParam)
	return ret
}

// showMenu 在鼠标处弹出右键菜单。
func (st *trayState) showMenu() {
	hMenu, _, _ := procCreatePopupMenu.Call()
	if hMenu == 0 {
		return
	}
	defer procDestroyMenu.Call(hMenu)

	appendMenu(hMenu, mfString, menuIDOpen, "打开主界面")
	appendMenu(hMenu, mfString, menuIDLogs, "查看日志")
	appendMenu(hMenu, mfSeparator, 0, "")
	appendMenu(hMenu, mfString, menuIDQuit, "退出")

	var pt pointT
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))

	// 🔴 必须先 SetForegroundWindow，否则菜单点外面不会消失
	//（这是 Win32 托盘的经典坑，MSDN 明确要求）
	procSetForegroundWin.Call(st.hwnd)

	// TPM_RETURNCMD 让我们直接拿到选择结果 —— 比等 WM_COMMAND 更简单可靠
	const tpmReturnCmd = 0x0100
	cmd, _, _ := procTrackPopupMenu.Call(
		hMenu,
		tpmRightButton|tpmNonotify|tpmReturnCmd,
		uintptr(pt.x), uintptr(pt.y),
		0, st.hwnd, 0,
	)

	// 不在这里处理选择：统一走 WM_COMMAND 分支会让逻辑分散。
	// 这里直接把结果转成一条 WM_COMMAND 投给自己。
	if cmd != 0 {
		procPostMessageW.Call(st.hwnd, wmCommand, cmd, 0)
	}
}

func appendMenu(hMenu uintptr, flags uint32, id uintptr, text string) {
	var p uintptr
	if text != "" {
		p = uintptr(unsafe.Pointer(utf16Ptr(text)))
	}
	procAppendMenuW.Call(hMenu, uintptr(flags), id, p)
}

// openPanel 用系统默认浏览器打开面板。
func (st *trayState) openPanel() {
	if st.baseURL == "" {
		return
	}
	url := st.baseURL + "/panel/"
	// 用 cmd /c start 以便不依赖具体浏览器关联（rundll32 亦可）
	cmd := exec.Command("cmd", "/c", "start", "", url)
	if err := cmd.Start(); err != nil {
		if st.logger != nil {
			st.logger.Printf("打开面板失败: %v", err)
		}
	}
}

// openLogs 打开日志目录（资源管理器）。
func (st *trayState) openLogs() {
	if st.logsDir == "" {
		return
	}
	cmd := exec.Command("explorer", st.logsDir)
	if err := cmd.Start(); err != nil {
		if st.logger != nil {
			st.logger.Printf("打开日志目录失败: %v", err)
		}
	}
}

// ── 小工具 ───────────────────────────────────────────────

// utf16Ptr 把 Go 字符串转成 NUL 结尾的 UTF-16 指针。
func utf16Ptr(s string) *uint16 {
	p, err := syscall.UTF16PtrFromString(s)
	if err != nil {
		// 只可能因含 NUL 而失败；退回一个空串，绝不 panic
		empty, _ := syscall.UTF16PtrFromString("")
		return empty
	}
	return p
}

// copyUTF16 把字符串写入定长 UTF-16 缓冲（自动截断 + NUL 结尾）。
func copyUTF16(dst []uint16, s string) {
	src, err := syscall.UTF16FromString(s)
	if err != nil {
		src = []uint16{0}
	}
	n := len(src)
	if n > len(dst) {
		n = len(dst) - 1
	}
	copy(dst[:n], src[:n])
	dst[n] = 0
}
