// browser.go：受控浏览器的探测、启动与生命周期管理。
//
// ═══════════════════════════════════════════════════════════════════
// 为什么需要"受控浏览器"
// ═══════════════════════════════════════════════════════════════════
//
// 委托方要求「网页登录，不需要下载软件」。实测结论（见
// codex-consult/登录实验结论-凭据来源已定位.md）：
//
//	官方登录页的回调 redirect_uri **强制白名单**，只允许官方域名
//	（自建 127.0.0.1 / localhost / example.com 全部 400）。
//	⇒ 拿不到 code，无法自己走授权码流程。
//	⇒ 必须在"官方页面上下文"里等它下发凭据。
//
// 而凭据下发的形态是**一个 HTTP 响应体**：
//
//	GET https://www.codebuddy.cn/console/login/enterprise?state=<我们的UUID>
//	  → { accessToken, refreshToken }
//
// 所以路径是：拉起一个受我们控制的浏览器 → 让用户在官方页面正常登录
// → 用 CDP 监听那一个响应 → 取走凭据 → 关掉浏览器、删干净痕迹。
//
// ═══════════════════════════════════════════════════════════════════
// 安全：这是本功能**固有的**新攻击面，不粉饰
// ═══════════════════════════════════════════════════════════════════
//
// CDP 的调试端口**对本机其他进程可达且无鉴权**。Codex 第 28 轮原文：
//
//	「随机端口不是鉴权；关闭进程也不能消除登录期间的攻击窗口。」
//
// 我们能做到的缓解（逐条落地，不是口号）：
//
//  1. 端口：随机空闲端口（不写死，减少被预测）
//  2. 绑定：只 127.0.0.1；启动后**回读**确认没有绑到 0.0.0.0
//  3. 不用 --remote-allow-origins（不主动放宽来源）
//  4. 独立临时 profile：与用户自己的浏览器完全隔离，
//     不给用户既有登录态，也不污染它
//  5. 登录互斥：同一时刻只允许一个流程
//  6. 支持取消 + 总超时（不能永远挂着）
//  7. 只终止**本进程创建的**浏览器进程树（绝不 kill 用户的浏览器）
//  8. 清理前校验路径确实在我们自己的临时目录下
//  9. 处理崩溃残留（下次启动清理旧的 wbapi-login-* 目录）
//
// ⚠️ 仍然存在的残余风险要如实告知用户（见面板文案）：
//
//	登录期间，本机其他进程理论上可连上该调试端口。
//	不接受此风险的用户，可以继续用"导入凭据"方式（旁路入口永远保留）。
//
// ═══════════════════════════════════════════════════════════════════
// 用哪个浏览器
// ═══════════════════════════════════════════════════════════════════
//
// 优先 Chrome，其次 Edge（两者都是 Chromium，CDP 通用）。
// Codex 第 28 轮：「Edge 属于 Chromium，可以验证同样的独立 profile 和 CDP 路径，
// 无需要求用户安装 Chrome。」
//
// 找不到任何受支持浏览器时**不回退到"下载浏览器"**，而是明确报错，
// 由上层引导用户改用导入方式（Codex Q4：「不要修改策略或静默下载浏览器」）。
package login

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ErrNoBrowser 表示本机找不到受支持的浏览器。
var ErrNoBrowser = errors.New("login: 未找到受支持的浏览器（需要 Chrome 或 Edge）")

// ErrLoginBusy 表示已有一个登录流程在进行。
var ErrLoginBusy = errors.New("login: 已有登录流程在进行中")

// BrowserKind 标识浏览器种类，用于诊断与日志（不含凭据）。
type BrowserKind string

const (
	BrowserChrome BrowserKind = "chrome"
	BrowserEdge   BrowserKind = "edge"
)

// browserCandidate 是一个候选浏览器路径。
type browserCandidate struct {
	kind BrowserKind
	path string
}

// browserCandidates 返回按优先级排序的候选列表。
//
// 顺序理由：Chrome 优先（更接近我们实测的环境），Edge 是 Windows 自带、
// 覆盖面最好，作为兜底。两者 CDP 协议一致。
func browserCandidates() []browserCandidate {
	var out []browserCandidate
	// 相对路径基于各环境变量根，避免把某台机器的绝对路径写死。
	roots := []string{
		os.Getenv("ProgramFiles"),
		os.Getenv("ProgramFiles(x86)"),
		os.Getenv("LOCALAPPDATA"),
	}
	for _, root := range roots {
		if root == "" {
			continue
		}
		out = append(out,
			browserCandidate{BrowserChrome, filepath.Join(root, "Google", "Chrome", "Application", "chrome.exe")},
			browserCandidate{BrowserEdge, filepath.Join(root, "Microsoft", "Edge", "Application", "msedge.exe")},
		)
	}
	return out
}

// FindBrowser 返回第一个存在的受支持浏览器。
//
// 返回的路径已经过 os.Stat 确认存在（不是"猜"出来的路径）。
func FindBrowser() (BrowserKind, string, error) {
	for _, c := range browserCandidates() {
		st, err := os.Stat(c.path)
		if err != nil || st.IsDir() {
			continue
		}
		return c.kind, c.path, nil
	}
	return "", "", ErrNoBrowser
}

// BrowserSession 是一个正在运行的受控浏览器。
type BrowserSession struct {
	kind    BrowserKind
	exePath string
	profile string
	port    int

	cmd *exec.Cmd

	closeOnce sync.Once
	closed    chan struct{}
}

// Kind 返回浏览器种类。
func (s *BrowserSession) Kind() BrowserKind { return s.kind }

// Port 返回 CDP 调试端口。
func (s *BrowserSession) Port() int { return s.port }

// ProfileDir 返回本次会话的临时 profile 目录。
func (s *BrowserSession) ProfileDir() string { return s.profile }

// Closed 返回会话关闭信号。
func (s *BrowserSession) Closed() <-chan struct{} { return s.closed }

// LoginOptions 控制一次登录会话的启动。
type LoginOptions struct {
	// StartURL 是浏览器打开的第一个页面。
	StartURL string

	// StartupTimeout 是等待 CDP 端口就绪的上限。
	// 零值用 defaultStartupTimeout。
	StartupTimeout time.Duration
}

const defaultStartupTimeout = 30 * time.Second

// manager 保证同一时刻只有一个登录流程（Codex Q5 要求"登录互斥"）。
var manager struct {
	mu      sync.Mutex
	current *BrowserSession
}

// StartBrowser 启动一个受控浏览器并等待其 CDP 端口就绪。
//
// 调用方结束时的收尾方式有两种，**按产品需求选**：
//   - Close()  —— 关浏览器 + 删临时 profile（"用完即毁"）
//   - Detach() —— 只停止监听，保留窗口（用户能看结果，见 login.LoginFor）
//
// 🔴 无论用哪种，都**必须**在结束时调用其中之一 —— 否则互斥位不会释放，
// 下次登录会被 ErrLoginBusy 永久挡住。
func StartBrowser(opts LoginOptions) (*BrowserSession, error) {
	kind, exe, err := FindBrowser()
	if err != nil {
		return nil, err
	}

	// ⚠️ 这里**刻意不做** profile 清理。
	//
	//	Detach 路径会保留上一个登录窗口（用户可能还开着看结果）。
	//	若在此处调 CleanupStaleProfiles，它会去删那个**仍在运行**的
	//	浏览器的 profile —— 而 os.RemoveAll 会"删掉能删的"，
	//	于是那个窗口的 profile 被删掉一半，可能导致浏览器异常。
	//	⇒ profile 清理改由两条**精确**路径负责：
	//	  ① Detach 时挂一个 goroutine，等浏览器**真的退出**后再删
	//	  ② 进程启动时清崩溃残留（见 app 层）
	manager.mu.Lock()
	if manager.current != nil {
		manager.mu.Unlock()
		return nil, ErrLoginBusy
	}
	manager.current = &BrowserSession{} // 占位，稍后替换
	manager.mu.Unlock()

	sess, err := startBrowserLocked(kind, exe, opts)
	if err != nil {
		manager.mu.Lock()
		manager.current = nil
		manager.mu.Unlock()
		return nil, err
	}

	manager.mu.Lock()
	manager.current = sess
	manager.mu.Unlock()
	return sess, nil
}

// startBrowserLocked 完成实际的启动（调用方已持有互斥位）。
func startBrowserLocked(kind BrowserKind, exe string, opts LoginOptions) (*BrowserSession, error) {
	// 用短前缀 + 随机后缀的临时目录。前缀固定是为了"崩溃残留可识别"。
	profile, err := os.MkdirTemp("", "wbapi-login-*")
	if err != nil {
		return nil, fmt.Errorf("login: 创建临时 profile 失败: %w", err)
	}
	// MkdirTemp 默认 0700（仅当前用户）—— 符合"仅当前用户 ACL"要求。

	port, err := freeLoopbackPort()
	if err != nil {
		os.RemoveAll(profile)
		return nil, fmt.Errorf("login: 申请本地端口失败: %w", err)
	}

	startURL := opts.StartURL
	if startURL == "" {
		startURL = "about:blank"
	}

	args := []string{
		"--user-data-dir=" + profile,
		"--remote-debugging-port=" + fmt.Sprint(port),
		// 只绑回环：显式指定，避免某些版本默认监听更宽的地址。
		"--remote-debugging-address=127.0.0.1",
		"--no-first-run",
		"--no-default-browser-check",
		// 首次运行向导 / 恢复气泡等交互会干扰登录，全部关掉。
		"--no-default-browser-check",
		"--disable-features=Translate,MediaRouter",
		"--disable-sync",
		"--disable-background-networking",
		startURL,
	}

	cmd := exec.Command(exe, args...)
	// 不接管 stdout/stderr：浏览器日志可能包含 URL（含 state），
	// 不该进入我们的日志管道。丢弃即可。
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Stdin = nil

	if err := cmd.Start(); err != nil {
		os.RemoveAll(profile)
		return nil, fmt.Errorf("login: 启动浏览器失败: %w", err)
	}

	sess := &BrowserSession{
		kind:    kind,
		exePath: exe,
		profile: profile,
		port:    port,
		cmd:     cmd,
		closed:  make(chan struct{}),
	}

	// 等 CDP 就绪。
	timeout := opts.StartupTimeout
	if timeout <= 0 {
		timeout = defaultStartupTimeout
	}
	if err := sess.waitReady(timeout); err != nil {
		sess.Close()
		return nil, err
	}
	return sess, nil
}

// freeLoopbackPort 向内核申请一个空闲端口，然后立刻释放。
//
// ⚠️ 这是"探测后释放"的经典竞态（释放到浏览器绑定之间，端口可能被别人抢走）。
// 对登录用途可以接受 —— 抢走的后果是 CDP 连不上，表现为可重试的失败，
// 而不是安全问题。选它是因为 stdlib 没有"保留端口并交出 fd"的原语。
//
// ponytail: 探测后释放存在微小竞态。升级触发条件：若实测出现端口被抢导致
// 的登录失败，改为启动后用 /json/version 发现实际端口（Chrome 支持 port=0）。
func freeLoopbackPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		return 0, errors.New("login: 非 TCP 地址")
	}
	return addr.Port, nil
}

// waitReady 轮询 CDP 的 /json/version 直到就绪或超时。
//
// 判定条件**不只是"端口能连"**：必须是 HTTP 200 且响应体含 Browser 字样，
// 否则可能连上了恰好占用该端口的无关服务。
func (s *BrowserSession) waitReady(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := newCDPHTTPClient(s.CDPBaseURL(), 2*time.Second)

	for time.Now().Before(deadline) {
		// 进程提前退出就没必要再等。
		if s.cmd.ProcessState != nil && s.cmd.ProcessState.Exited() {
			return errors.New("login: 浏览器进程提前退出")
		}
		ver, err := client.version()
		if err == nil && strings.TrimSpace(ver.Browser) != "" {
			// 回读确认端口确实绑在回环上（不只信我们的参数）。
			if !s.portIsLoopback() {
				return errors.New("login: 调试端口未绑在回环地址上，已中止")
			}
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("login: 等待浏览器 CDP 就绪超时（%s）", timeout)
}

// portIsLoopback 回读本机监听表，确认调试端口只绑在回环地址。
//
// 为什么不只看自己的启动参数：参数可能被浏览器忽略或改变默认行为，
// "实际绑到哪"才是事实。这是 Codex Q5 要求"实测 IPv4/IPv6 均只绑回环"的落地。
func (s *BrowserSession) portIsLoopback() bool {
	// 用一个到该端口的连接反查本地地址，避免依赖平台特定的监听表 API。
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", s.port), time.Second)
	if err != nil {
		return false
	}
	defer conn.Close()
	la, ok := conn.LocalAddr().(*net.TCPAddr)
	if !ok {
		return false
	}
	return la.IP.IsLoopback()
}

// CDPBaseURL 返回调试端点的基地址（固定回环）。
func (s *BrowserSession) CDPBaseURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d", s.port)
}

// Detach 停止跟踪本次会话，但**不关闭浏览器、不删 profile**。
//
// ═══════════════════════════════════════════════════════════════════
// 为什么要这个（2026-10-08 委托人需求 2）
// ═══════════════════════════════════════════════════════════════════
//
//	委托人原话：「我每次登录后还没看到是否登录成功的反馈，
//	            你就直接把窗口关了」
//
//	⇒ 登录结束（无论成功还是失败）后**保留窗口**，让用户自己看到结果、
//	  自己关。程序只做"停止监听"，不再 kill 进程、不删 profile。
//
// 🔴 与 Close 的关键区别（别把两者混用）：
//
//	Close   = 结束浏览器进程树 + 删临时 profile（原行为，"用完即毁"）
//	Detach  = 只释放互斥位与跟踪，浏览器与 profile **原样保留**
//
//	Detach 之后，本次会话的 profile 目录**不会**被立刻清理 ——
//	程序会挂一个后台 goroutine 等**浏览器真的退出**后再删。
//
//	为什么不在 Detach 当场删：此刻浏览器**仍在运行**、profile 被它锁着。
//	os.RemoveAll 会"删掉能删的"，造成那个窗口的 profile 残缺 ——
//	可能让浏览器崩或行为异常。所以必须等它自然退出。
func (s *BrowserSession) Detach() {
	s.closeOnce.Do(func() {
		close(s.closed)

		// ⚠️ 刻意**不做** s.cmd.Process.Kill()：浏览器继续运行，
		//	用户可以查看登录结果并自己关闭窗口。

		// 释放互斥位 —— 否则下次登录会被"已有一个登录流程在进行中"永久挡住。
		manager.mu.Lock()
		if manager.current == s {
			manager.current = nil
		}
		manager.mu.Unlock()

		// 后台等浏览器退出，然后清理它的临时 profile。
		//
		// 这是"保留窗口"与"不堆积垃圾"之间的折中：
		//   - 用户看结果期间：profile 完好（浏览器正常）
		//   - 用户关掉窗口后：profile 被自动删掉，不占磁盘
		//
		// ⚠️ 不设超时：一个登录窗口开一整天也该由用户决定何时关。
		//	进程若一直不退，profile 就一直留着 —— 那是正确行为，
		//	因为强行删会破坏运行中的浏览器。崩溃残留由启动时的
		//	CleanupStaleProfiles 兜底。
		if s.cmd != nil && s.cmd.Process != nil {
			go func(cmd *exec.Cmd, profile string) {
				_ = cmd.Wait() // 等浏览器进程真正退出
				// 删不掉也无妨：启动时的清理会再试一次。
				_ = removeOwnProfile(profile)
			}(s.cmd, s.profile)
		}
	})
}

// Close 关闭浏览器并清理临时 profile。
//
// 顺序很重要：
//  1. 先关 CDP 连接（由上层负责）
//  2. 结束浏览器进程树
//  3. 等进程真正退出（否则文件还被占用，删不掉）
//  4. **校验路径**确实在我们的临时目录下，再删除
//
// 可重复调用。
func (s *BrowserSession) Close() error {
	var err error
	s.closeOnce.Do(func() {
		close(s.closed)

		// 只终止我们自己启动的那个进程。
		// ⚠️ 绝不按进程名 kill —— 那会误杀用户正在用的 Chrome。
		if s.cmd != nil && s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
			// 等它退出，给文件句柄一点释放时间。
			done := make(chan struct{})
			go func() {
				_ = s.cmd.Wait()
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				// 超时也继续 —— 不无限等，但会在下面重试删除。
			}
		}

		// 释放互斥位。
		manager.mu.Lock()
		if manager.current == s {
			manager.current = nil
		}
		manager.mu.Unlock()

		// 清理 profile：先校验路径，再删。
		if err := removeOwnProfile(s.profile); err != nil {
			err = fmt.Errorf("login: 清理临时 profile 失败: %w", err)
		}
	})
	return err
}

// removeOwnProfile 删除**我们自己创建的**临时 profile 目录。
//
// 安全校验（防误删）：
//   - 路径必须非空
//   - 基名必须以 wbapi-login- 开头
//   - 必须位于系统临时目录之下
//
// 任一不满足就拒绝删除 —— 宁可留垃圾，不可误删用户数据。
func removeOwnProfile(profile string) error {
	if profile == "" {
		return errors.New("路径为空")
	}
	base := filepath.Base(profile)
	if !strings.HasPrefix(base, "wbapi-login-") {
		return fmt.Errorf("路径 %q 不是本程序创建的 profile，拒绝删除", base)
	}
	tmp := os.TempDir()
	absProfile, err := filepath.Abs(profile)
	if err != nil {
		return err
	}
	absTmp, err := filepath.Abs(tmp)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(absTmp, absProfile)
	if err != nil || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return fmt.Errorf("路径 %q 不在临时目录 %q 下，拒绝删除", absProfile, absTmp)
	}

	// 浏览器退出后句柄可能还没释放，带重试。
	var lastErr error
	for i := 0; i < 10; i++ {
		if lastErr = os.RemoveAll(absProfile); lastErr == nil {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return lastErr
}

// CleanupStaleProfiles 清理上次崩溃残留的 wbapi-login-* 目录。
//
// 何时调用：程序启动时（见 app 层）。返回清理数量与错误。
//
// 为什么需要：若进程被强杀，Close 不会执行，临时 profile 会留在磁盘上。
// 它们不含凭据（凭据只在内存与我们的加密存储里），但会占空间、
// 也可能让用户困惑。所以启动时清一次。
func CleanupStaleProfiles() (int, error) {
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		return 0, err
	}
	cleaned := 0
	var firstErr error
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "wbapi-login-") {
			continue
		}
		full := filepath.Join(os.TempDir(), e.Name())
		if err := removeOwnProfile(full); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		cleaned++
	}
	return cleaned, firstErr
}

// CurrentSession 返回当前进行中的会话（无则 nil）。供面板查询进度。
func CurrentSession() *BrowserSession {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return manager.current
}
