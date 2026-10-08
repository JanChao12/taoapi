// serve.go：serve 命令的装配与生命周期。
//
// 从 app.go 拆出来的原因：CLI 命令（cli.go）也要改 app.go 的分派表，
// 拆开后两边互不冲突。
//
// 本文件负责把各组件接起来：
//
//	账号文件 --DPAPI--> auth.Store --pool--> 选号 --chatter--> workbuddy 上游
//	                                      \--> /v1/models、面板、统计
package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"workbuddy.local/workbuddy-api/internal/auth"
	"workbuddy.local/workbuddy-api/internal/autostart"
	"workbuddy.local/workbuddy-api/internal/config"
	"workbuddy.local/workbuddy-api/internal/datadir"
	"workbuddy.local/workbuddy-api/internal/login"
	"workbuddy.local/workbuddy-api/internal/provider"
	"workbuddy.local/workbuddy-api/internal/provider/workbuddy"
	"workbuddy.local/workbuddy-api/internal/router"
	usagepkg "workbuddy.local/workbuddy-api/internal/usage"
)

// runServe 启动 HTTP 服务并阻塞直到收到退出信号。
//
// 命令行入口（`wbapi serve`）：rt 为 nil，表示**非 GUI 模式**
// （不挂托盘、不弹提示、日志走 stderr）。
func runServe(args []string) int {
	return runServeMode(args, nil)
}

// runServeMode 是 serve 的完整实现，带可选的 GUI 运行时。
//
// 🔴 为什么把 GUI 模式也走这里、而不是另写一份 main：
//
//	serve 里有端口解析（flag > 配置）、重启交接与健康握手、
//	drain 超时、"重启失败要恢复并重建后台任务"等一大堆
//	已经被测试和真实验收覆盖过的逻辑。GUI 模式若复制一份，
//	两边必然逐渐分叉（这是本项目已经踩过的教训：
//	"数据目录逻辑在 4 处各写一遍"）。
//
//	⇒ 只把"托盘/弹窗/日志去向"作为参数注入，其余完全共用。
//
// serveFlags 保存 serve 子命令解析出来的 flag 值。
type serveFlags struct {
	addr        *string
	verbose     *bool
	silent      *bool
	restartID   *string
	panelOrigin *string
	fs          *flag.FlagSet
}

// newServeFlagSet 构造 serve 的 flag 集合。
//
// 🔴 抽出来是为了**可测**：`TestServeAcceptsSilentFlag` 要断言
//
//	`--silent` 被真的定义过。不抽出来的话，测试只能去真的跑
//	runServe（会起服务并阻塞，无法在单测里用）——
//	而"flag 没定义"这个缺陷的症状极隐蔽：
//	开机自启因 unknown flag **静默退出**，用户只看到"开机后服务没起来"。
func newServeFlagSet() *serveFlags {
	fs := flag.NewFlagSet(CmdServe, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	// addr 默认空串 = "没指定"，这样才能实现优先级：
	// 命令行 flag > 配置文件 > 内置默认。
	// 若这里写死 DefaultAddr，就永远分不清"用户没传"和"用户传了默认值"，
	// 配置里改的端口会被无声覆盖（Codex 第 9 轮明确指出这个坑）。
	addr := fs.String("addr", "", "监听地址（只用回环；缺省用设置里的端口）")
	verbose := fs.Bool("v", false, "输出更详细的日志")

	// --silent：静默启动（开机自启用），不弹提示框。
	//
	// 🔴 命令行模式（`wbapi serve`）下它只是**被接受** ——
	//	因为命令行模式本来就不弹窗（弹窗只发生在 GUI 模式）。
	//	但必须**接受**它：开机自启注册的命令行是 `serve --silent`，
	//	若不识别这个 flag，自启会因"flag provided but not defined"退出，
	//	表现为"开机后服务没起来"（且没有任何提示，极难排查）。
	silent := fs.Bool("silent", false, "静默启动：不弹提示框（开机自启用）")

	// --restart-id 由父进程在重启交接时传入（见 restart.go）。
	//
	// 它只在**本次交接**内有意义，不落盘、不进配置。
	// 非重启启动时为空串，/healthz 的 restartId 字段随之省略。
	restartID := fs.String("restart-id", "", "（内部使用）本次重启交接标识")

	// --panel-origin 是"面板当前所在的 origin"，同样由父进程经本次交接传入。
	//
	// 用途：把它加进 /healthz 的 CORS 白名单。实测缺了它，**旧**页面
	// 探测**新**端口时会拿到 `TypeError: Failed to fetch`（浏览器拒绝读取），
	// 自动重连 100% 失效 —— 详见 health.go 坑 3。
	panelOrigin := fs.String("panel-origin", "", "（内部使用）面板当前 origin")

	return &serveFlags{
		addr: addr, verbose: verbose, silent: silent,
		restartID: restartID, panelOrigin: panelOrigin, fs: fs,
	}
}

func runServeMode(args []string, rt *guiRuntime) int {
	sf := newServeFlagSet()
	fs := sf.fs
	addr := sf.addr
	verbose := sf.verbose
	silent := sf.silent
	restartID := sf.restartID
	panelOrigin := sf.panelOrigin

	// ⚠️ 这里**故意没有** --addr-override（我一度加过，是错的，已撤）。
	//
	//	当时的想法：父进程若用 --addr 起的，"重启也该保留那个 --addr"。
	//	但那与"面板改端口必须生效"**直接矛盾**：
	//	  父 --addr 8881 → 面板把端口改成 8882 → 重启
	//	  plannedListenAddr 以配置为准给出 8882（面板被告知去 8882）
	//	  但子进程收到 --addr 8881 → 实际监听 8881
	//	  → **服务在 8881、面板被告知去 8882**，三处不一致，面板永远连不上。
	//
	//	端到端验收抓到了这个矛盾（场景 4 导航失败 + 服务端日志显示
	//	子进程仍监听旧端口）。**已撤掉整个 override 机制。**
	//
	//	现在的语义（唯一自洽的）：
	//	  子进程**不带 --addr**，因此按"配置 > 默认"解析，
	//	  与 plannedListenAddr（以配置为准）完全一致。
	//	  这正是原实现的做法，也是委托方要的"面板改端口就生效"。

	if err := fs.Parse(args); err != nil {
		return 2
	}

	var logger *log.Logger
	if rt != nil && rt.logger != nil {
		// GUI 模式：日志已改为"写文件 + stderr"（GUI 子系统没有控制台）
		logger = rt.logger
	} else {
		logger = log.New(os.Stderr, "wbapi ", log.LstdFlags|log.Lmsgprefix)
	}

	// 🔴 `serve --silent`（开机自启注册的命令行）也要有 GUI 运行时 ——
	//	否则开机后：没有托盘图标、日志只写 stderr（GUI 子系统下无处可去）。
	//
	//	⚠️ 但**不弹提示**（silent=true）—— 这正是该 flag 的意义。
	if rt == nil && *silent {
		rt = &guiRuntime{enabled: true, silent: true}
		// 日志落文件（与 runGUI 同样的理由：GUI 子系统没有控制台）
		logPath := filepath.Join(datadir.LogsDir(),
			"wbapi-"+nowFunc().Format("2006-01-02")+".log")
		var closeLog func()
		logger, closeLog = newGUIlogger(logPath)
		defer closeLog()
		rt.logger = logger
		rt.logsDir = datadir.LogsDir()
	}

	// ── 一次性数据迁移（老位置 → exe 同目录）──
	//
	// 2026-10-07：数据目录改为 exe 同目录（照 wild-work 布局）。
	// 老用户的数据在 `%USERPROFILE%\.wbapi`，首次在新位置启动时搬过来。
	//
	// 🔴 迁移是**复制**而不是移动、且**不覆盖**目标已有文件、
	//	**不解析 JSON**（见 datadir.Migrate 的安全原则）——
	//	动的是唯一凭据副本，任何"聪明"的处理都可能丢号。
	if res := datadir.Migrate(); res.Performed {
		logger.Printf("首次启动：已从旧目录迁移数据 %s → %s", res.From, res.To)
		if len(res.Moved) > 0 {
			logger.Printf("  已迁移 %d 个文件：%v", len(res.Moved), res.Moved)
		}
		if len(res.Skipped) > 0 {
			// 跳过而不是覆盖：目标已有更新的数据
			logger.Printf("  跳过 %d 个已存在的文件（未覆盖）：%v",
				len(res.Skipped), res.Skipped)
		}
		for _, e := range res.Errors {
			logger.Printf("  ⚠️ 迁移失败: %s", e)
		}
		// 旧目录保留不删 —— 迁移出问题时用户还有原始数据
		logger.Printf("  旧目录已保留（未删除）：%s", res.From)
	}

	// ── 清理上次崩溃残留的登录临时 profile ──
	//
	// 登录窗口现在由**用户自己关**（委托人需求 2：要能看到登录结果）。
	// 正常路径下 profile 会在浏览器退出后自动删除（见 BrowserSession.Detach）；
	// 但如果进程被强杀、或删除时文件仍被占用，就会留下残留目录。
	// 这里在启动时兜底清一次。
	//
	// ⚠️ 只在启动时做，不在每次登录前做：登录前清理会撞上
	//	"用户上一个登录窗口还开着"的情况，删掉它正在用的 profile。
	if n, cerr := login.CleanupStaleProfiles(); n > 0 {
		logger.Printf("已清理 %d 个残留的登录临时目录", n)
	} else if cerr != nil {
		logger.Printf("⚠️ 清理登录临时目录失败（不影响服务）: %v", cerr)
	}

	// ── 开机自启自愈：exe 被移动/改名后修正注册表里的过期路径 ──
	// 🔴 为什么需要（2026-10-08 委托人提问："文件夹地址换了，自启是不是又失效"）：
	//
	//	Run 键里是**写入那一刻的绝对路径快照**，不是跟着 exe 走的引用。
	//	移动/改名文件夹（甚至只改 exe 文件名）后，自启**静默失效** ——
	//	不弹框、不写日志；而面板的 Enabled() 只查"值是否存在"，
	//	仍显示"已开启" ⇒ **界面在骗用户**。
	//
	//	自愈只在"已启用"时才重写（HealNotEnabled 分支什么都不做）——
	//	绝不能因为"发现没注册"就替用户打开自启，那是越权。
	//
	//	⚠️ 它必须靠**手动运行一次**触发：自启已经失效，程序不会自己起来。
	//	  解决的是"你手动打开一次，以后就自动好了"。
	//
	// 失败不阻止启动：这只是锦上添花，注册表权限异常不该让服务起不来。
	if exe, err := os.Executable(); err == nil {
		switch outcome, herr := autostart.Heal(exe); {
		case herr != nil:
			logger.Printf("⚠️ 开机自启自愈检查失败（不影响服务）: %v", herr)
		case outcome == autostart.HealRewritten:
			logger.Printf("已修正开机自启路径（exe 位置变了）→ %s", exe)
		default:
			// HealNotEnabled / HealUnchanged：都是"不该动"，
			// 但记一行便于排查（用户报告"自启不生效"时能立刻看到结论）。
			logger.Printf("开机自启检查：%s", outcome)
		}
	}

	// ── 先加载设置（需要它里面的端口）──
	settings, err := config.Load(config.Path())
	if err != nil {
		logger.Printf("⚠️ 加载设置失败（用默认值继续）: %v", err)
		settings = config.NewInMemory()
	}
	if settings.Corrupt() {
		logger.Printf("⚠️ 设置文件损坏，已回退默认值并保留原文件（见 %s.corrupt-*）",
			config.Path())
	}

	// 解析最终监听地址：flag 优先，其次设置里的端口。
	//
	// ⚠️ 这里**不做**"把 --addr 记录成 override 并继承给子进程"那套（已撤，见上方说明）：
	// 那会与 plannedListenAddr 的"以配置为准"打架，导致
	// 服务实际监听 A、面板被告知去 B。**子进程一律不带 --addr**，
	// 于是它与 plannedListenAddr 用同一套解析，永不分叉。
	listenAddr := *addr
	if listenAddr == "" {
		listenAddr = fmt.Sprintf("127.0.0.1:%d", settings.Get().Port)
	}

	// 安全边界：只允许回环地址。这是硬约束，不做成可配置项。
	if err := requireLoopback(listenAddr); err != nil {
		fmt.Fprintf(os.Stderr, "wbapi serve: %v\n", err)
		return 2
	}

	// ── 装配依赖 ──
	deps, err := buildDeps(logger, *verbose, splitHostPort(listenAddr), settings)
	if err != nil {
		logger.Printf("装配失败: %v", err)
		return 1
	}

	// 一键重启：由面板 /api/settings/restart 触发。
	// 状态机保证同一时刻只有一次重启在进行（重复点击返回 409）。
	restartSt := newRestartState()
	deps.restart = restartSt

	// 版本检查与更新。
	//
	// 🔴 **没有后台自动更新**：本状态机只是"检查/下载/替换"的记账本，
	//	每一次动作都由用户在面板上显式点击触发（委托方 2026-10-09 明确要求）。
	deps.Update = newUpdateState()

	// 本次交接标识：只有在"被父进程以 --restart-id 拉起"时才非空。
	//
	// 🔴 形态校验必须做（Codex 要求"缺失、格式错误或不匹配时健康握手失败"）：
	// 参数来自命令行，虽然正常情况下由父进程生成，但一个畸形值
	// 不该被回显给面板（那会让面板误判"对面是本次交接的进程"）。
	// 非法值一律当作"没有标识"，退化为非重启启动 —— 宁可不匹配，不可误匹配。
	if *restartID != "" {
		if !validRestartID(*restartID) {
			logger.Printf("⚠️ 传入的 --restart-id 格式非法，已忽略（健康握手将不匹配）")
		} else {
			deps.restartID = *restartID
		}
	}

	// /healthz 的 CORS 白名单：包含"目标服务自己的 origin"，
	// 以及本次交接的**旧** origin（若父进程传了 --panel-origin）。
	//
	// ⚠️ 旧 origin 是**两个**而不是一个：用户可能用 `localhost` 打开，
	// 而命令行传进来的是 `location.origin` 的精确值（实测
	// `http://localhost:8787` 与 `http://127.0.0.1:8787` 是两个不同 Origin）。
	// newAllowedOrigins 会做严格校验，非法值静默丢弃。
	//
	// 白名单**每次重启重建**（这里只由本次交接的两个值构成），
	// 不累积历次旧 origin —— Codex 明确要求，避免无界增长。
	deps.healthOrigins = newAllowedOrigins(
		"http://"+listenAddr, // 新地址（子进程自己的 origin）
		*panelOrigin,         // 旧 origin（本次交接传进来的）
	)

	// 面板写操作令牌（CSRF 主防线，见 csrf.go）。
	// 生成失败不阻止启动 —— 退化为"只靠 Origin + Content-Type 防护"，
	// 并明确记日志，而不是静默降级。
	if tok, terr := newPanelToken(); terr != nil {
		logger.Printf("⚠️ 生成面板令牌失败（写操作防护降级）: %v", terr)
	} else {
		deps.panelToken = tok
	}

	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           newMux(deps),
		ReadHeaderTimeout: ReadHeaderTimeout,
		ReadTimeout:       ReadTimeout,
		WriteTimeout:      0, // SSE 长连接不能用整体写超时，见 limits.go 说明
		IdleTimeout:       IdleTimeout,
		BaseContext:       func(net.Listener) context.Context { return context.Background() },
		ErrorLog:          logger,
	}

	// 绑定端口（先 Listen 再 Serve，启动失败能立刻报错）
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wbapi serve: 监听 %s 失败: %v\n", srv.Addr, err)
		// 🔴 GUI 模式（双击启动）没有控制台，必须弹窗告知失败原因 ——
		// 否则用户双击后"什么都没发生"，完全无从排查。
		if rt != nil && !rt.silent {
			MessageBox("TAOAPI 启动失败",
				"无法监听 "+srv.Addr+"：\n"+err.Error()+
					"\n\n常见原因：\n"+
					"· 该端口已被其他程序占用\n"+
					"· 上一次的 wbapi 进程还在运行\n\n"+
					"可在「设置」页改端口，或先结束占用该端口的进程。",
				true)
		}
		return 1
	}

	logger.Printf("已启动，监听 http://%s", ln.Addr())
	logger.Printf("面板: http://%s/panel/   健康检查: http://%s/healthz", ln.Addr(), ln.Addr())
	logger.Printf("已加载 %d 个账号；暴露 %d 个模型",
		accountCount(deps), len(deps.Router.Models()))
	if len(deps.Router.Models()) == 0 {
		logger.Printf("⚠️ 模型列表为空 —— 请先 `wbapi auth import` 导入账号")
	}
	if *verbose {
		logger.Printf("版本 %s；并发上限 %d；上游 %s",
			Version, MaxConcurrentStreams, UpstreamChatBase)
	}

	// ── 信号与托盘退出通道 ──
	//
	// ⚠️ 必须在托盘回调**之前**建好：托盘"退出"要往 sigCh 投递一个合成信号，
	//	复用 Ctrl+C 那条已验证的优雅关闭路径（而不是另写一套关闭逻辑）。
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	// ── GUI 模式：挂托盘 + 手动启动弹一次提示 ──
	//
	// 🔴 顺序：**先挂托盘，再弹提示**。
	//
	//	弹窗是**阻塞**的（模态），若先弹提示、用户不点确定就一直卡在那，
	//	托盘图标也不出现 —— 用户会以为程序没起来。
	//	反过来先挂托盘，提示框后面弹，用户点掉即有完整功能。
	trayStop := func() {}
	if rt != nil && rt.enabled {
		baseURL := "http://" + ln.Addr().String()
		stop, terr := startTray(trayOptions{
			Logger:  logger,
			BaseURL: baseURL,
			LogsDir: rt.logsDir,
			Tip:     fmt.Sprintf("TAOAPI —— %s", baseURL),
			OnQuit: func() {
				// 托盘"退出"：触发与 Ctrl+C 相同的优雅关闭路径
				logger.Printf("收到托盘退出请求，正在关闭…")
				select {
				case sigCh <- sigQuitFromTray:
				default:
				}
			},
		})
		if terr != nil {
			// 托盘挂不上不该让服务起不来 —— 明确记日志（+ 提示框），继续服务
			logger.Printf("⚠️ 托盘图标启动失败（服务继续运行）: %v", terr)
			if !rt.silent {
				MessageBox("TAOAPI 提示",
					"服务已启动，但托盘图标创建失败：\n"+terr.Error()+
						"\n\n面板地址：\n"+baseURL+"/panel/",
					true)
			}
		} else {
			trayStop = stop
			logger.Printf("托盘图标已就绪（左键打开面板，右键菜单）")
		}

		// 手动启动才弹提示；`--silent`（开机自启）不打扰
		//
		// 🔴 弹窗**必须异步**（放到 goroutine 里）：
		//
		//	MessageBox 是**模态阻塞**的 —— 用户不点"确定"就一直停在那。
		//	我第一版把它放在这里同步调用，而 `srv.Serve(ln)` 在它**后面**
		//	才执行 ⇒ 端口已 Listen 但**从未开始服务**：
		//	  实测现象 = 端口在 Listen、healthz 超时、日志 0 字节。
		//	（日志 0 字节是因为第一行日志排在 Serve 之后。）
		//
		//	⇒ 改为后台 goroutine 弹窗，主流程继续去 Serve。
		if !rt.silent {
			promptText := "OpenAI 兼容 API 地址：\n" + baseURL + "/v1" +
				"\n\n管理面板：\n" + baseURL + "/panel/" +
				"\n\n· 托盘图标：左键打开主界面，右键菜单\n" +
				"· 关闭服务：托盘右键「退出」\n" +
				"· 日志目录：" + rt.logsDir
			go MessageBox("TAOAPI 已启动", promptText, false)
		}
	}

	// 优雅退出
	errCh := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	// 退出前把托盘图标摘掉。
	//
	// 🔴 必须摘：否则通知区会留下一个**点不动的幽灵图标**，
	//	用户只能把鼠标划过去等它消失（这是托盘程序最常见的坏体验）。
	//	用 defer 保证所有退出路径都摘（包括异常返回）。
	defer trayStop()

	// 面板「立即重启」用它打断阻塞：设置页改了端口后需要重启才生效。
	// restartCh 在装配阶段已创建并放进 deps（handler 也要用到同一个），
	// 这里只是取出来等待。
	select {
	case err := <-errCh:
		if err != nil {
			logger.Printf("服务异常退出: %v", err)
			stopBackground(deps)
			return 1
		}
	case sig := <-sigCh:
		logger.Printf("收到 %v，正在关闭…", sig)
		ctx, cancel := context.WithTimeout(context.Background(), ShutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			logger.Printf("关闭超时: %v", err)
		}
	case <-deps.restart.ch:
		logger.Printf("收到重启请求，正在重新启动…")
		// 🔴 顺序至关重要（Codex 第 10、11 轮两次指出，实测都成立）：
		//
		//	错误做法：先 Start 子进程 → 自己退出。
		//	  端口还被本进程占着，子进程 Listen 失败立刻退出，
		//	  而父进程已经退了 —— 结果【服务彻底没了】，且用户只看到断连。
		//	  "先 os.Stat 验证可执行文件"根本挡不住这种失败（文件明明在）。
		//
		//	正确做法：① 先关掉自己的监听（释放端口）
		//	         ② 拉起子进程并做【健康握手】（不是"没退出就算成功"）
		//	         ③ 握手成功才退出；否则自己重新监听【并重建后台任务】。
		//
		// 代价：①②之间有一小段窗口服务不可用（毫秒级，且发生在
		// 用户已知情的重启期间）。换来"重启失败也不丢服务"。
		//
		// 🔴 drain 超时必须【放弃本次重启】（Codex 第 12 轮指出，我采纳）：
		//
		//	我原先的写法是"Shutdown 超时就打日志，继续启动子进程"。
		//	那是错的 —— 超时意味着**仍有流式请求没结束**，它们可能还在写
		//	usage；此时启动子进程就变成父子【同时】写同一个 JSONL，
		//	这不是理论风险而是必然发生。
		//
		//	Codex 给了两个可接受方案：加锁，或"超时就放弃本次重启"。
		//	我选后者：本工具写入频率极低（一次对话一行），而加跨进程文件锁
		//	要引入 LockFileEx + 轮转/读取都要持锁，复杂度和出错面都更大。
		//	放弃重启的代价只是"用户再点一次"，比数据交错可接受得多。
		ctx, cancel := context.WithTimeout(context.Background(), ShutdownTimeout)
		shutdownErr := srv.Shutdown(ctx)
		cancel()

		if shutdownErr != nil {
			// 还有请求没结束 → 不启动子进程（避免父子同时写 usage），
			// 回复原状态并让用户稍后重试。
			logger.Printf("重启放弃：等待在途请求结束后超时（%v）；"+
				"未启动新进程，服务保持在原地址。请稍后重试", shutdownErr)
			deps.restart.abort()
			return recoverAfterFailedRestart(recoverParams{
				Logger:   logger,
				Addr:     listenAddr,
				Verbose:  *verbose,
				Settings: settings,
				Deps:     deps,
			})
		}
		stopBackground(deps)

		// 取出本次交接上下文（restartID / 面板的旧 origin / 新地址）。
		//
		// 🔴 旧 origin 决定"重启后新服务是否允许旧页面读取健康响应"。
		// 实测：新服务若只允许自己的 origin，旧 origin 页面会拿到
		// `TypeError: Failed to fetch`，自动重连 100% 失效（见 health.go 坑 3）。
		// 所以子进程的白名单 = {旧 origin, 新 origin}，且**每次重启重建**、
		// 不累积历次旧 origin。
		pendingID, clientOrigin, newAddr := deps.restart.takePending()
		if pendingID == "" {
			// 正常不会发生（handler 一定先 setPending 再 signal）。
			// 没有标识就无法完成"四条件"握手，宁可明确失败也不放行。
			logger.Printf("重启放弃：未取得本次交接标识；服务保持在原地址")
			deps.restart.abort()
			return recoverAfterFailedRestart(recoverParams{
				Logger:   logger,
				Addr:     listenAddr,
				Verbose:  *verbose,
				Settings: settings,
				Deps:     deps,
			})
		}

		// 🔴 握手必须探测【新】地址，不是本进程正在监听的旧地址。
		//
		//	2026-10-07 修真实缺陷：这里原先把 `listenAddr`（**旧**地址）
		//	传给了 relaunchAndHandshake，而 handler 已经算好的 newAddr
		//	被 `_` 丢弃。后果（实测复现，日志为证）：
		//
		//	  面板改端口 8787 → 4567，点「立即重启」：
		//	    03:48:13 新进程启动成功，监听 127.0.0.1:4567   ← 一切正常
		//	    03:48:28 重启失败: 新进程在 15s 内未通过健康检查
		//	    03:48:28 已恢复到 127.0.0.1:8787               ← 好进程被杀
		//
		//	  因为子进程**不带 --addr**（见 relaunchAndHandshake 说明），
		//	  它按新配置监听 4567；而父进程却去 8787（旧地址）轮询 /healthz
		//	  ⇒ 15 秒内永远匹配不到 ⇒ 误判失败 ⇒ 端口改动**永不生效**。
		//
		//	⚠️ 这个缺陷只在"**端口发生变化**"时暴露：端口不变时
		//	  新旧地址相同，握手照常成功 —— 所以既有的端到端测试
		//	  没抓到它（它们验的是"能不能重启"，没验"改端口后能否生效"）。
		//
		//	修法：用 handler 计算好的 newAddr（= plannedListenAddr，以配置为准），
		//	  它与子进程的解析规则**同源**，不会分叉。
		handshakeAddr := newAddr
		if handshakeAddr == "" {
			// 兜底：理论上 setPending 一定会写 newAddr。
			// 缺了就退回旧地址（端口没变时等价），并明确记日志而不是静默。
			logger.Printf("⚠️ 本次交接未携带新地址，握手回退到原地址 %s", listenAddr)
			handshakeAddr = listenAddr
		}

		if err := relaunchAndHandshake(handshakeAddr, pendingID, clientOrigin, logger); err != nil {
			// 新进程没能就绪 → 恢复原服务，不能让用户的服务消失。
			// ⚠️ 恢复时必须【重建后台任务】：上面 stopBackground 已把
			// 签到守护停掉，只重新 Listen 会得到一个"功能缺一半"的服务
			// 而用户看不出来（Codex 第 11 轮指出的 v2 缺陷）。
			logger.Printf("重启失败: %v；正在恢复原服务…", err)
			deps.restart.abort()
			return recoverAfterFailedRestart(recoverParams{
				Logger:   logger,
				Addr:     listenAddr,
				Verbose:  *verbose,
				Settings: settings,
				Deps:     deps,
			})
		}
		return 0
	}

	stopBackground(deps)
	logger.Printf("已退出")
	return 0
}

// stopBackground 依次回收后台任务（签到守护等）。
func stopBackground(deps Deps) {
	for _, fn := range deps.onShutdown {
		if fn != nil {
			fn()
		}
	}
}

// buildDeps 装配服务依赖：设置、账号、路由、上游客户端、用量存储、签到守护。
//
// settings 由调用方传入（runServe 已经加载过一次）——
// 不在这里重复 Load，否则同一个文件被读两次，
// 且"损坏回退"的日志会打两遍。
func buildDeps(logger *log.Logger, verbose bool, listenPort int,
	settings *config.Store) (Deps, error) {

	deps := Deps{
		Logger:     logger,
		Verbose:    verbose,
		Usage:      usagepkg.NewStore(""),
		ListenPort: listenPort,
		Settings:   settings,
	}

	// ── 账号 ──
	codec := storageCodec()
	persister := auth.NewPersister(auth.AccountsPath(), codec)
	store, err := persister.Load()
	if err != nil {
		return deps, fmt.Errorf("加载账号失败: %w", err)
	}
	deps.Persister = persister
	deps.Accounts = store

	// ── 上游客户端（共享连接池；Provider 只是轻包装）──
	client := workbuddy.NewClient()

	// ── 模型路由：**按平台**各拉一次模型目录 ──
	//
	// 🔴 为什么要循环两个平台（2026-10-06 接入国际版）：
	//
	//	两版的模型集**部分重叠但不相同**（国际版的 gpt 系国内没有，
	//	国内的 deepseek 系国际没有）。只注册一个平台，
	//	另一个平台的模型就永远不出现在 /v1/models 里。
	//
	//	每个平台各自挑一个该平台可用账号去拉 —— 用错平台的账号
	//	会让整个目录拉取失败（凭据与端点不匹配）。
	//
	// 🔴 为什么要**逐个账号重试**（2026-10-07 修脆弱点）：
	//
	//	原来只取"第一个未禁用账号"，失败就 `continue` —— 于是
	//	**一个账号 token 失效（或一次网络抖动）就会让整个平台的模型
	//	全部消失**，而服务看起来"正常启动"，用户只看到模型列表变空。
	//
	//	同一个平台的所有账号拉的是**同一份目录**（目录与账号无关，
	//	只与平台端点有关），所以换一个账号重试是完全等价的 ——
	//	这是纯收益的兜底，没有任何口径变化。
	//
	//	⚠️ 但**只注册一次**：同一平台不同账号的 provider ID 相同，
	//	  重复 Register 会用同一个 key 反复覆盖同一批模型。
	//	  所以成功后立刻 break，不是把每个账号都注册一遍。
	deps.Router = router.New()
	registered := 0
	for _, plat := range []workbuddy.Platform{workbuddy.PlatformCN, workbuddy.PlatformIntl} {
		candidates := activeAccountsOfPlatform(store, plat)
		if len(candidates) == 0 {
			continue // 该平台没有账号：跳过（不是错误，用户可能只用一版）
		}
		ok, used, errs := registerOnePlatform(deps.Router, client, candidates, 30*time.Second)
		if ok {
			registered++
			if used > 0 {
				// 记下来：说明前面的账号有问题，值得用户注意
				logger.Printf("已注册 %s 平台模型目录（第 %d 个账号才成功，前 %d 个失败）",
					plat.ProviderID(), used+1, used)
			} else {
				logger.Printf("已注册 %s 平台模型目录", plat.ProviderID())
			}
			// 逐个报告失败原因（成功那次不报）
			for i := 0; i < used && i < len(errs); i++ {
				logger.Printf("  · 账号 %s 失败: %v", candidates[i].UID, errs[i])
			}
			continue
		}
		// 该平台所有账号都失败：模型拉取失败不阻止启动
		// （可能只是网络抖动，账号还能签到/查额度）
		var last error
		if len(errs) > 0 {
			last = errs[len(errs)-1]
		}
		logger.Printf("⚠️ %s 平台模型目录拉取失败（%d 个账号都试过了；服务仍会启动）: %v",
			plat.ProviderID(), len(candidates), last)
	}
	if registered == 0 {
		logger.Printf("⚠️ 没有可用账号，/v1/models 将为空；先运行 `wbapi auth import`")
	}

	// ── 别名（映射表）：从设置装载 ──
	// 别名非法（例如目标模型已不存在）时不阻止启动，只告警并跳过 ——
	// 否则用户改坏一个别名就会把整个服务弄得起不来。
	if aliases := settings.Get().Aliases; len(aliases) > 0 {
		if err := deps.Router.SetAliases(aliases); err != nil {
			logger.Printf("⚠️ 别名表未生效（设置里的映射已忽略）: %v", err)
		} else {
			logger.Printf("已加载 %d 条模型别名", len(aliases))
		}
	}

	// ── 按账号对话的适配器（换号调度用）──
	deps.Chatter = &wbChatter{client: client}
	deps.WBClient = client // 账号管理 API（面板 refresh/checkin）也复用这个连接池

	// ── 内置每日自动签到（可开关，默认【关闭】）──
	//
	// 委托人 2026-10-05 明确：默认关闭，且要能在设置里开关。
	// 装配完成后 close(ready) 通知守护"账号已就绪"，避免用固定 sleep 猜时机。
	daemon, ready := newCheckinDaemon(deps)
	deps.Checkin = daemon
	deps.applyAutoCheckin = func(enabled bool) {
		// 设置页改开关时立即生效（不是等下次启动）
		if enabled {
			daemon.Start()
		} else {
			daemon.Stop()
		}
	}
	deps.onShutdown = append(deps.onShutdown, daemon.Shutdown)
	close(ready) // 账号已装配完成，允许守护开始

	if settings.Get().AutoCheckin {
		daemon.Start()
		logger.Printf("自动签到已启用（账号就绪后立即触发，之后每 30 分钟检查）")
	} else {
		logger.Printf("自动签到未启用（可在面板「设置」页开启）")
	}

	// ── 凭证保活（可开关，默认【开启】）──
	//
	// 🔴 与自动签到**默认值相反**，理由：
	//
	//	签到是"额外的收益动作"，不做也没有损失；而**凭据过期会让
	//	账号直接不可用**，用户被迫重新登录 —— 那正是委托方要消除的痛点
	//	（「让账号不用重复登录」）。所以保活默认开启。
	//
	// ⚠️ 保活**不改变任何账号的可调度性**，也不清除 auth_expired
	//	（见维护备忘 §二之一：刷新成功 ≠ chat 可用）。它只换 token。
	keepalive := newKeepaliveDaemon(deps)
	deps.Keepalive = keepalive
	deps.applyKeepalive = func(enabled bool) {
		if enabled {
			keepalive.Start()
		} else {
			keepalive.Stop()
		}
	}
	deps.onShutdown = append(deps.onShutdown, keepalive.Shutdown)
	keepalive.MarkReady() // 账号已装配完成

	if keepaliveEnabled(settings.Get().Keepalive) {
		keepalive.Start()
		logger.Printf("凭证保活已启用（账号就绪后立即检查，之后每 30 分钟一次；提前 24 小时续期）")
	} else {
		logger.Printf("凭证保活未启用（可在面板「设置」页开启；关闭后凭据过期需重新登录）")
	}

	return deps, nil
}

// keepaliveEnabled 读保活开关，**零值视为开启**。
//
// 🔴 为什么零值要当开启（而不是像 AutoCheckin 那样当关闭）：
//
//	Keepalive 是新加的设置字段。老用户的 config.json 里**没有这个键**，
//	反序列化后是 false。若按 false 处理，升级后老用户的保活**默认是关的**
//	—— 那与他升级前"从不需要重新登录"的既有体验不一致，
//	且他不会知道要去开。
//
//	⇒ 用指针语义（*bool）表达"用户从没表过态"，此时跟随"默认开启"。
//	  用户**显式关掉**过就尊重他的选择（存 false）。
func keepaliveEnabled(v *bool) bool {
	if v == nil {
		return true
	}
	return *v
}

// accountCount 返回账号数（nil 安全）。
func accountCount(d Deps) int {
	if d.Accounts == nil {
		return 0
	}
	return d.Accounts.Len()
}

// pickAnyActive 返回第一个未被人工禁用的账号（仅用于拉模型目录）。
func pickAnyActive(store *auth.Store) *auth.Account {
	return pickAnyActiveOfPlatform(store, "")
}

// registerOnePlatform 用该平台的账号**逐个尝试**拉取并注册模型目录。
//
// 为什么要逐个试（2026-10-07 修脆弱点）：
//
//	原来只取"第一个未禁用账号"，失败即放弃该平台 —— 于是**一个账号
//	token 失效、或一次网络抖动，就会让整个平台的模型全部消失**，
//	而服务看起来"正常启动"，用户只看到模型列表变空。
//
//	同一平台的账号拉的是**同一份目录**（目录只与平台端点有关，
//	与具体账号无关），所以换账号重试完全等价 —— 纯收益的兜底。
//
// 返回值：
//
//	ok   是否成功注册
//	used 成功时用到的账号下标（都失败时为 -1）
//	errs 每个已尝试账号的错误（下标与 accts 对齐，用于日志/测试断言）
//
// ⚠️ 成功即返回，**不做多次注册**：同平台不同账号的 provider ID 相同，
// 重复 Register 会用同一个 key 反复覆盖同一批模型。
func registerOnePlatform(
	r *router.Router,
	client *workbuddy.Client,
	accts []*auth.Account,
	timeout time.Duration,
) (ok bool, used int, errs []error) {
	return attemptModels(r, client, accts, timeout, r.Register)
}

// attemptModels 是 registerOnePlatform / refreshOnePlatform 的公共实现：
// 逐个账号尝试，把"拉取并写入路由"这一步交给 apply。
//
// 抽出来是为了让**启动注册**与**运行期刷新**共用同一套重试语义 ——
// 两处各写一遍容易出现"刷新漏了重试"这类不对称缺陷。
//
// apply 用哪种语义由调用方决定：
//   - 启动用 `Router.Register`（合并，只跑一次，不存在残留）
//   - 刷新用 `Router.Refresh`（先删旧条目，避免下架模型滞留）
func attemptModels(
	r *router.Router,
	client *workbuddy.Client,
	accts []*auth.Account,
	timeout time.Duration,
	apply func(context.Context, provider.Provider) error,
) (ok bool, used int, errs []error) {
	for i, acct := range accts {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		p := workbuddyProviderFor(client, acct)
		err := apply(ctx, p)
		cancel()
		if err == nil {
			return true, i, errs
		}
		errs = append(errs, err)
	}
	return false, -1, errs
}

// attemptModels 的签名引用 provider.Provider —— 见文件头的 import 说明。

// refreshOnePlatform 用该平台的账号逐个尝试**刷新**模型目录。
//
// 与 registerOnePlatform 的区别只有"写入语义"：
//   - 注册：`Router.Register`（合并）
//   - 刷新：`Router.Refresh`（先删该 provider 旧条目再写）
//
// 🔴 为什么刷新必须用 Refresh：Register 只写不删 ⇒ 上游**下架**的模型
// 会永远留在列表里，用户选到它只会报错。那正是本项目一直在防的
// "看起来能用、实际报错"。
func refreshOnePlatform(
	r *router.Router,
	client *workbuddy.Client,
	accts []*auth.Account,
	timeout time.Duration,
) (ok bool, used int, errs []error) {
	return attemptModels(r, client, accts, timeout, r.Refresh)
}

// activeAccountsOfPlatform 返回指定平台**全部**未禁用的账号。
//
// plat 为空 ⇒ 不限平台（兼容既有调用）。
//
// 🔴 为什么需要"全部"而不只是第一个（2026-10-07）：
//
//	启动时拉模型目录若只用第一个账号，该账号 token 失效（或一次网络抖动）
//	就会让**整个平台的模型消失**，而且不重试、不换号。
//	同平台所有账号拉的是同一份目录，所以逐个重试是等价的兜底。
//
// ⚠️ 顺序即 store 的顺序（稳定），便于日志里定位"第几个账号成功"。
func activeAccountsOfPlatform(store *auth.Store, plat workbuddy.Platform) []*auth.Account {
	if store == nil {
		return nil
	}
	want := ""
	if plat != "" {
		want = auth.NormalizePlatform(string(plat))
	}
	var out []*auth.Account
	for _, a := range store.ListByPlatform(want) {
		if !a.ManualDisabled {
			out = append(out, a)
		}
	}
	return out
}

// pickAnyActiveOfPlatform 返回指定平台第一个未禁用的账号。
//
// plat 为空 ⇒ 不限平台（兼容既有调用）。
//
// 🔴 拉模型目录必须用**同平台**的账号：国际端点不认国内 token，
// 用错账号会让整个目录拉取失败（表现为"国际版没模型"）。
func pickAnyActiveOfPlatform(store *auth.Store, plat workbuddy.Platform) *auth.Account {
	accts := activeAccountsOfPlatform(store, plat)
	if len(accts) == 0 {
		return nil
	}
	return accts[0]
}

// workbuddyProviderFor 用指定账号构造 provider。
//
// Client（连接池）共享，Credential 按账号区分 —— Provider 本身是轻对象。
// CLI 命令（status/checkin/doctor）也用这个函数，保持单一构造点。
//
// 🔴 平台从**账号**推导（2026-10-06 接入国际版）：
//
//	账号自带 Platform 字段，所以不需要调用方额外传平台参数 ——
//	少一个参数就少一处"忘记传/传错"的机会。
//
//	⚠️ 但基址必须在**每次构造时**按该账号的平台设置：
//	共享的 client 会被不同平台的账号交替使用，
//	基址残留上一个平台的值会把凭据发到**错误的上游域名**。
func workbuddyProviderFor(client *workbuddy.Client, a *auth.Account) *workbuddy.Provider {
	plat := platformOfAccount(a)
	chatBase, billingBase := plat.BaseURLs()
	// SetPlatform 而非 SetBases：基址若已被显式覆盖（测试指假上游），
	// 它**不会**改 —— 否则单测会被指到真实上游（真实踩过，见其注释）。
	client.SetPlatform(chatBase, billingBase)
	return workbuddy.NewProviderFor(client, credentialOf(a), plat)
}

// platformOfAccount 把账号的平台字段转成 provider 层的 Platform。
func platformOfAccount(a *auth.Account) workbuddy.Platform {
	if a != nil && a.IsIntl() {
		return workbuddy.PlatformIntl
	}
	return workbuddy.PlatformCN
}

// credentialOf 从账号记录构造上游凭据。
func credentialOf(a *auth.Account) workbuddy.Credential {
	return workbuddy.Credential{
		AccessToken:  a.AccessToken,
		UID:          a.UID,
		EnterpriseID: a.EnterpriseID,
		Domain:       a.Domain,
		RefreshToken: a.RefreshToken,
	}
}

// wbChatter 把 workbuddy 适配成 accountChatter（换号调度接口）。
type wbChatter struct {
	client *workbuddy.Client
}

// ChatWithAccount 实现 accountChatter：用指定账号发起对话。
func (w *wbChatter) ChatWithAccount(ctx context.Context, acct *auth.Account,
	req provider.ChatRequest, emit func(provider.Event) error) error {
	return workbuddyProviderFor(w.client, acct).Chat(ctx, req, emit)
}
