// restart.go：面板「立即重启」的后端。
//
// 场景（委托人确认的方案）：端口改成 8787 以外后必须重启才生效
// （net.Listener 无法热切换），所以面板给一个按钮 —— 比让用户自己去
// 任务管理器杀进程友好得多。
//
// ═══════════════════════════════════════════════════════════════════
// 演进历史（两次被 Codex 否决，都是真缺陷，别再退化回去）
// ═══════════════════════════════════════════════════════════════════
//
// v1（错）：os.Stat 验证 → Start 子进程 → 自己退出。
//
//	问题：端口还被父进程占着，子进程 Listen 必失败并立刻退出，
//	父进程却已退出 —— 服务彻底消失。"验证文件存在"挡不住这种失败。
//
// v2（仍不足）：先 Shutdown 释放端口 → 等端口空 → 拉子进程 →
//
//	观察 1.5 秒"没退出就算成功" → 失败则重新监听原地址。
//	Codex 第 11 轮指出三个真缺陷（我已逐条验证成立）：
//	  a. 恢复正常，但**后台任务没恢复** —— stopBackground 已经跑过，
//	     签到守护被永久停掉，恢复后的服务是个"少了一半功能"的僵尸。
//	  b. **1.5 秒不是健康握手** —— 慢启动会被误判为失败，
//	     而"端口被别的进程抢占"这种失败 1.5 秒后又看不出来。
//	  c. 重复点击被**静默吞掉**（channel 满就丢弃），用户不知道发生了什么。
//
// v3（本文件）：
//   - 显式状态机（idle/restarting），重复请求返回 409 而不是丢弃
//   - 成功判据改为**子进程健康握手**（轮询 /healthz），不是"没退出"
//   - 失败恢复时**重建 server + 重新注册后台任务**，不是只重新 Listen
//   - drain 有明确超时（ShutdownTimeout）
//
// 明确接受的取舍：不做 listener 句柄交接（零停机）。单用户自用工具，
// 重启由用户主动点击，短暂中断可接受。Codex 也认可这一点。
package app

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"

	"workbuddy.local/workbuddy-api/internal/config"
	"workbuddy.local/workbuddy-api/internal/protocol/openai"
)

// restartPhase 是重启的状态（Codex 要求用显式状态而不是"丢弃请求"）。
type restartPhase int32

const (
	restartIdle restartPhase = iota
	restartInProgress
)

// restartState 保存重启状态机。
//
// 用独立结构体而不是裸 channel：状态要能被 HTTP handler **读取**
// 并据此返回 409，而 channel 只能表达"有没有信号"。
type restartState struct {
	mu    sync.Mutex
	phase restartPhase
	ch    chan struct{}

	// pending* 是 handler 与 serve 主循环之间传递"本次交接上下文"的槽位。
	//
	// 为什么不用 channel 传这些值：信号 channel 的容量是 1 且只表达"该重启了"，
	// 把三个字段塞进去会让"信号已发出/未发出"与"值是否被取走"耦合成
	// 难以推理的状态。用持锁的槽位更直白：handler 写、主循环读。
	pendingRestartID string
	pendingOrigin    string
	pendingNewAddr   string
}

func newRestartState() *restartState {
	return &restartState{ch: make(chan struct{}, 1)}
}

// setPending 记录本次交接的上下文（由 handler 在发出信号前调用）。
func (s *restartState) setPending(restartID, origin, newAddr string) {
	s.mu.Lock()
	s.pendingRestartID = restartID
	s.pendingOrigin = origin
	s.pendingNewAddr = newAddr
	s.mu.Unlock()
}

// takePending 取出本次交接的上下文（由 serve 主循环在收到信号后调用）。
func (s *restartState) takePending() (restartID, origin, newAddr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pendingRestartID, s.pendingOrigin, s.pendingNewAddr
}

// begin 尝试进入"重启中"。返回 false 表示已有重启在进行 → 调用方应返回 409。
func (s *restartState) begin() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phase == restartInProgress {
		return false
	}
	s.phase = restartInProgress
	return true
}

// abort 在"已受理但还没真正开始"的阶段失败时回滚状态。
func (s *restartState) abort() {
	s.mu.Lock()
	s.phase = restartIdle
	s.mu.Unlock()
}

// InProgress 供 handler 判断。
func (s *restartState) InProgress() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.phase == restartInProgress
}

// signal 通知 serve 主循环开始重启（非阻塞，因为状态已由 begin 保证唯一）。
//
// 🔴 不要直接调用它 —— 用 requestRestart。
//
//	本函数只发信号、不填交接上下文，而 serve 主循环**要求**上下文齐全
//	（见 serve.go 的 pendingID == "" 分支：宁可放弃重启也不放行）。
//	单独调用它 = 必然触发出「重启放弃」，这正是 2026-10-09 那个
//	「点更新 → 程序换了但服务没重启」故障的成因。
//	保留为小写私有方法，仅供 requestRestart 内部使用。
func (s *restartState) signal() {
	select {
	case s.ch <- struct{}{}:
	default:
	}
}

// requestRestart 是**唯一**允许用来触发重启的入口。
//
// ═══════════════════════════════════════════════════════════════════
// 🔴 为什么必须收敛成一个入口（2026-10-09 委托方实测故障）
// ═══════════════════════════════════════════════════════════════════
//
//	故障现象（委托方原话）：
//	  "我使用后台面板的更新，在我更新后面板未能重启，浏览器刷新也没用，
//	   我将托盘图标退出，结果打不开了提示端口占用，后台面板也没有更新成功"
//
//	根因：`handleUpdateApply` 走的是**更新**路径，却复用了「改端口重启」
//	的通道，且只调了 signal()、**漏了 setPending()**。于是 serve 主循环
//	拿到空标识（serve.go: `pendingID == ""`）→ **明确放弃重启** →
//	调用 recoverAfterFailedRestart 在原地址原地复活。
//
//	  实测日志（D:\tools\TAOAPI\data\logs\wbapi-2026-10-09.log）：
//	    04:31:53 自动更新：已替换程序…准备重启生效
//	    04:31:53 收到重启请求，正在重新启动…
//	    04:31:53 重启放弃：未取得本次交接标识；服务保持在原地址
//	    04:31:53 已恢复到 127.0.0.1:4545 继续服务（重启未生效…）
//
//	  ⇒ 更新是 100% 必然失败的：exe 被换掉了，跑着的却还是旧版。
//	  ⇒ 且失败方式极具误导性：面板说"更新成功"，服务却纹丝不动。
//
//	修法（比"在 update 分支补一行 setPending"更结实）：
//	  把 setPending + begin + signal 绑成一个**原子入口**，
//	  让"只发信号不填上下文"在结构上不可能再发生。
//	  —— 与 login.StartBrowser 的闸门同一个思路：护栏放在**危险动作发生点**，
//	     而不是逐个调用方去补，将来新增调用方自动受保护。
//
// ⚠️ 它**立刻发信号**，因此只适用于"调用完就可以开始关闭"的场景；
//
//	需要"先把 HTTP 响应写出去、再开始关闭"的场景用 claimRestart（见其注释）。
//
// 返回 false 表示已有重启在进行（调用方应返回 409，不要静默丢弃）。
// addr 传空串时由 serve 主循环回退到当前监听地址（见其兜底分支）。
func (s *restartState) requestRestart(restartID, origin, newAddr string) bool {
	if !s.claimRestart(restartID, origin, newAddr) {
		return false
	}
	s.signal()
	return true
}

// claimRestart 只做"受理 + 填交接上下文"，**不**发信号。
//
// 用途：调用方还需要先写 HTTP 响应（如 202），写完才允许主循环开始关闭 ——
// 若在这里就发信号，主循环可能抢在响应送达前 Shutdown，客户端拿到的是
// 连接重置而不是 202。
//
// 🔴 调用 claimRestart 之后**必须**调用 signal()，否则这次重启永远不会发生：
//
//	状态机已被置为 restartInProgress，而信号没发出 —— 后续所有重启请求
//	都会得到 409，服务却一直不重启。这正是"半截操作"的新形态。
//	（故本函数不导出为"随便调"的接口，仅与 signal 成对出现在
//	 requestRestart / handleRestart 两处，且有测试守着。）
func (s *restartState) claimRestart(restartID, origin, newAddr string) bool {
	if !s.begin() {
		return false
	}
	// 🔴 顺序：先填上下文，再（由调用方）发信号。
	//	反过来会让主循环可能先取到空槽位 —— 明确按"先写后发"来。
	s.setPending(restartID, origin, newAddr)
	return true
}

// restartResponse 是重启接口的响应。
type restartResponse struct {
	OK bool `json:"ok"`

	// NewAddr 重启后将监听的地址（host:port）。
	//
	// 前端据此跳转；否则用户改了端口后浏览器会一直连在旧 origin 上。
	NewAddr string `json:"newAddr"`

	// RestartID 是本次重启的交接标识。
	//
	// 前端探测新地址时必须核对它：只有 /healthz 回显的 restartId
	// 与这里一致，才说明对面是**本次**交接启动的进程，
	// 而不是上一次重启遗留的旧进程（详见 health.go）。
	RestartID string `json:"restartId"`

	// Message 给用户看的一句话。
	Message string `json:"message"`
}

// restartRequest 是 POST /api/settings/restart 的可选请求体。
type restartRequest struct {
	// Origin 是**面板当前所在的 origin**（前端填 `location.origin`）。
	//
	// 🔴 为什么必须由前端提供（Codex 第 19 轮，实测支撑）：
	//
	//	用户可能用 `http://localhost:8787` 打开面板，此时
	//	`location.origin` 是 `http://localhost:8787`；而按**监听地址**
	//	推导会得到 `http://127.0.0.1:8787` —— **两者是不同的 Origin 字符串**，
	//	白名单写错一个，浏览器就会拒绝新端口上的健康响应（实测
	//	`TypeError: Failed to fetch`），自动重连彻底失效。
	//
	// ⚠️ 这里的值**不被信任**：服务端会用 normalizeLoopbackOrigin
	// 独立严格校验（只接受 http + 回环 host + 合法端口），
	// 非法值静默丢弃 —— 白名单宁小勿大。
	Origin string `json:"origin"`
}

// handleRestart 处理 POST /api/settings/restart。
func handleRestart(deps Deps, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, http.StatusMethodNotAllowed, openai.ErrTypeInvalidRequest,
			"method_not_allowed", "只支持 POST")
		return
	}
	if deps.restart == nil {
		writeError(w, http.StatusServiceUnavailable, openai.ErrTypeServer,
			"restart_unavailable", "当前运行方式不支持自动重启，请手动重启 wbapi")
		return
	}

	newAddr, err := plannedListenAddr(deps)
	if err != nil {
		writeError(w, http.StatusInternalServerError, openai.ErrTypeServer,
			"restart_failed", err.Error())
		return
	}
	if err := verifyRelaunchable(); err != nil {
		writeError(w, http.StatusInternalServerError, openai.ErrTypeServer,
			"restart_failed", "无法重启: "+err.Error())
		return
	}

	// 本次交接标识：**每次重启都新生成**，绝不复用旧值
	// （复用会让"上一次遗留的旧进程"也匹配成功，标识就失去意义）。
	restartID, err := newRestartID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, openai.ErrTypeServer,
			"restart_failed", "无法生成重启标识: "+err.Error())
		return
	}

	// 解析前端提供的旧 origin（非法则忽略，不影响重启本身）。
	var clientOrigin string
	if r.Body != nil {
		var req restartRequest
		// 请求体可选：解析失败不是错误（老客户端不带 body）。
		if dec := json.NewDecoder(io.LimitReader(r.Body, 4096)); dec.Decode(&req) == nil {
			clientOrigin = req.Origin
		}
	}

	// 🔴 重复点击必须给出确定回应（Codex 要求返回 409），
	// 而不是静默丢弃 —— 否则用户连点几次，完全不知道哪次生效了。
	//
	// 用 claimRestart（而不是 requestRestart）：这里必须先写 202 响应，
	// 写完才能让主循环开始关闭 —— 否则客户端拿到的是连接重置而不是 202。
	// ⚠️ 拆成 claim + signal 两步时，两步**必须都执行**（见 claimRestart 注释）；
	//    漏掉 setPending 正是 2026-10-09「更新不重启」故障的成因。
	if !deps.restart.claimRestart(restartID, clientOrigin, newAddr) {
		writeError(w, http.StatusConflict, openai.ErrTypeInvalidRequest,
			"restart_in_progress", "重启已在进行中，请稍候再试")
		return
	}

	// 202：重启【尚未完成】，只是已受理。
	// 用 200 会让前端以为此刻服务已就绪，立刻去连新地址而失败。
	writeJSON(w, http.StatusAccepted, restartResponse{
		OK:        true,
		NewAddr:   newAddr,
		RestartID: restartID,
		Message:   "已受理重启，服务将短暂中断；若端口有变更，请访问 http://" + newAddr + "/panel/",
	})
	// 必须确认响应真的写出去了：本进程随后就要关闭监听，
	// 没 flush 的话客户端可能收到连接重置而不是 202。
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	// 🔴 响应已送达，现在才允许主循环开始关闭（claim 与 signal 成对，缺一不可）。
	deps.restart.signal()
}

// plannedListenAddr 计算重启后将使用的监听地址。
//
// ═══════════════════════════════════════════════════════════════════
// 定调依据：委托方 2026-10-05 原话
// ═══════════════════════════════════════════════════════════════════
//
//	"假设后台是 8891 那么反代也是 8891，在后台设置界面里修改了
//	  反代 [端口] 成 8892 后，那么后台和反代都变为 8892。
//	  在浏览器把后台地址端口改成新的 8892 就能刷新打开了。"
//
// 即：**后台面板与反代 API 是同一个 listener**，面板里改端口就是改它 ——
// 所以**面板改的值必须生效**。本函数因此以【配置】为准。
//
// ═══════════════════════════════════════════════════════════════════
// 两个我试过但**错误**的版本（留档，避免后人再"优化"回去）
// ═══════════════════════════════════════════════════════════════════
//
//	❌ v1（原始实现）：只读配置 port。
//	   看起来就是"以配置为准"，问题不在这句话本身，而在**子进程**：
//	   当时父进程若带过 --addr，会把它与"配置"的关系弄成分叉
//	   （详见 v2）。v1 的独立缺陷是 Codex 第 20 轮指出的：
//	   `serve --addr 127.0.0.1:8891`（配置 8787）重启会跳到 8787 ——
//	   与启动时不一致。
//
//	❌ v2（我按 Codex 建议改的第一版）：把父进程的 --addr 记为
//	   addrFromCLI 并**继承给子进程**（经 --addr-override），
//	   让它"一路保留"。
//	   后果是**三处不一致**（端到端验收 + 服务端日志实证）：
//	     父 --addr 8881 → 面板改成 8882 →
//	     plannedListenAddr 以配置为准给出 8882（面板被告知去 8882）
//	     子进程收到 --addr-override 8881 → **实际监听 8881**
//	     → 服务在 8881、面板被告知去 8882 → 面板永远连不上。
//	    验收的现象是"面板不导航"+ 日志里子进程仍监听旧端口。
//
//	✅ v3（当前）：**以配置为准**，且**子进程一律不带 --addr**。
//	   这样本函数与 serve.go 的解析规则完全同源（配置 > 默认），
//	   不存在两处分叉的可能。
//
// ⚠️ 关于 Codex 说的 "flag > 配置" 契约：
//
//	那条约束的是**启动时**（runServe 仍按 flag > 配置）。
//	重启的触发方式只有一种：用户在面板上操作 —— 此时"配置"就是用户
//	刚刚的输入，比陈旧的 --addr 更能代表意图。**以用户最新操作胜出。**
//
// host 恒为 127.0.0.1：只监听回环是硬约束。
func plannedListenAddr(deps Deps) (string, error) {
	port := 0
	if deps.Settings != nil {
		port = deps.Settings.Get().Port
	}
	if port <= 0 {
		return "", fmt.Errorf("设置里的端口无效")
	}
	return fmt.Sprintf("127.0.0.1:%d", port), nil
}

// verifyRelaunchable 做"能不能拉起来"的前置检查。
//
// 只能验证可执行文件本身 —— 端口冲突之类问题必须等父进程释放端口后才知道。
func verifyRelaunchable() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("无法定位可执行文件: %w", err)
	}
	st, err := os.Stat(exe)
	if err != nil {
		return fmt.Errorf("可执行文件不可访问: %w", err)
	}
	if st.IsDir() {
		return fmt.Errorf("可执行文件路径是目录: %s", exe)
	}
	return nil
}

// recoverParams 是 recoverAfterFailedRestart 的入参。
//
// 抽成结构体是因为参数已有 5 个，位置参数容易传错顺序
// （尤其 Verbose 是 bool，传反了编译器不一定报错）。
//
// Logger 用具体的 *log.Logger 而不是 loggerLike 接口：
// buildDeps 需要它，而接口无法隐式转回具体类型。
type recoverParams struct {
	Logger   *log.Logger
	Addr     string
	Verbose  bool
	Settings *config.Store
	Deps     Deps
}

// recoverAfterFailedRestart 是"重启失败后恢复服务"的**生产入口**。
//
// 🔴 为什么必须抽成生产函数而不是只写在 runServe 里（Codex 第 15 轮要求）：
//
//	我原先把这段逻辑内联在 runServe 的重启分支里，
//	测试只能"按相同顺序手工重建 server" —— 那有三个问题：
//	  1. 测试构造顺序可能与生产不同，验的不是同一段代码；
//	  2. 生产分支里的状态清理（stopBackground、restart.abort）测试可能漏掉；
//	  3. 生产分支将来改了，测试仍会通过但已不再覆盖它。
//
//	抽出来后 runServe 与测试**调用同一个函数**，测试才真正是关卡。
//
// 职责：重建完整 deps（含后台任务）→ 新建 listener 与 http.Server
// （不能用被 Shutdown 过的旧对象）→ 阻塞服务直到退出。
func recoverAfterFailedRestart(p recoverParams) int {
	logger := p.Logger
	if logger == nil {
		logger = log.Default()
	}

	return restoreAfterFailedRestart(logger, p.Addr, func() (Deps, []func()) {
		fresh, err := buildDeps(logger, p.Verbose,
			splitHostPort(p.Addr), p.Settings)
		if err != nil {
			// 装配失败也要能继续服务 —— 至少让面板打得开、看得到错误，
			// 而不是整个服务消失。
			logger.Printf("恢复时装配失败（后台任务将不可用）: %v", err)
			return Deps{Logger: logger, Settings: p.Settings}, nil
		}
		// 沿用同一个重启状态机（否则恢复后的服务无法再次触发重启）
		fresh.restart = p.Deps.restart
		return fresh, fresh.onShutdown
	})
}

// loggerLike 让本文件只依赖"能打日志"这一件事，便于测试。
type loggerLike interface{ Printf(format string, v ...any) }

// relaunchAndHandshake 拉起新进程并以【健康握手】判定成功。
//
// 🔴 成功判据是"新进程能响应 /healthz 且**四条件全中**"（Codex 第 11 轮定方向，
// 第 19 轮补全判据），而不是"1.5 秒内没退出"：
//   - 慢启动（磁盘慢、杀软扫描）会被后者误判为失败；
//   - 端口被别的进程抢占这类问题，后者根本看不出来。
//
// addr 是新进程将要监听的地址（用于握手探测）。
// 子进程**不带 --addr**：否则旧的显式端口会覆盖用户刚保存的新端口。
//
// restartID 是本次交接标识；clientOrigin 是面板当前所在 origin（可能为空）。
// 两者都**以参数数组**传给子进程（不手工拼接命令行 —— Codex 要求）：
//   - restartID 让子进程在 /healthz 回显，父进程据此确认"对面是本次拉起的进程"；
//   - clientOrigin 让子进程把**旧** origin 加进 CORS 白名单 ——
//     实测缺了它，旧页面读不到新端口的健康响应，自动重连 100% 失效。
//
// **不传 --addr**：见下方 args 构造处的说明（传了会让两端口径分叉）。
func relaunchAndHandshake(addr, restartID, clientOrigin string, logger loggerLike) error {
	// 等端口真空出来（Shutdown 返回后内核可能还持有极短时间）。
	// 注意这仍有 TOCTOU：检查通过后别的进程可能抢先占用 ——
	// 所以真正的判据是后面的健康握手，这里只是尽量提高成功率。
	if !waitForPortFree(addr, 3*time.Second) {
		return fmt.Errorf("端口 %s 在 3 秒内未释放", addr)
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("无法定位可执行文件: %w", err)
	}

	// 用参数数组而不是拼字符串：标识是受控的 hex，但仍不该走命令行拼接
	// （Codex 明确要求；拼接会让"标识里含空格/引号"这类假设悄悄进入代码）。
	args := []string{CmdServe}
	// 🔴 必须带 --silent：让**重启后的子进程也有托盘图标与退出通道**。
	//
	// ═══════════════════════════════════════════════════════════════
	// 2026-10-07 委托人实测缺陷（原话："我的托盘图标消失了退出不了"）
	// ═══════════════════════════════════════════════════════════════
	//
	//	重启前：GUI 模式，有托盘，右键「退出」可用。
	//	重启后：子进程只带 --restart-id/--panel-origin ⇒ serve.go 里
	//	        `rt == nil && *silent` 不成立 ⇒ **rt 保持 nil**
	//	        ⇒ 不挂托盘、没有窗口、日志只写 stderr。
	//	后果：**托盘图标永久消失，且没有任何退出手段** ——
	//	      用户只能去任务管理器强杀（`taskkill` 不带 /F 会被拒：
	//	      "This process can only be terminated forcefully"，
	//	      实测确认，因为该进程 MainWindowHandle=0，没有窗口消息通道）。
	//
	//	为什么用 --silent 而不是新增一个开关：
	//	  serve.go L150-155 已经把 `--silent` 定义为"**挂 GUI 运行时但不弹提示**"，
	//	  这正是重启子进程需要的语义 —— 复用既有契约，不新增分支。
	//	  （`--silent` 抑制的只是启动提示框，见 serve.go L366 `if !rt.silent`。）
	args = append(args, FlagSilent)
	if restartID != "" {
		args = append(args, relaunchIDFlag, restartID)
	}
	if clientOrigin != "" {
		args = append(args, relaunchOriginFlag, clientOrigin)
	}
	// ⚠️ 这里**故意不传** --addr（我一度传过 --addr-override，是错的，已撤）。
	//
	//	子进程不带 --addr → 按"配置 > 默认"解析 → 与 plannedListenAddr 一致。
	//	若传了父进程的 --addr，子进程会监听旧端口，
	//	而 plannedListenAddr（以配置为准）已经把新端口告诉了面板 ——
	//	两边分叉，面板永远连不上。端到端验收抓到了这个矛盾。

	cmd := exec.Command(exe, args...)
	cmd.Dir = mustGetwd()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = nil

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("无法启动新进程: %w", err)
	}

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	// 健康握手：轮询 /healthz，直到新进程**确认就绪**或超时。
	// 同时监听"子进程已退出"——那说明它启动失败，不必等满超时。
	deadline := time.Now().Add(relaunchHandshakeTimeout)
	for time.Now().Before(deadline) {
		select {
		case err := <-exited:
			return fmt.Errorf("新进程启动后退出: %v", err)
		default:
		}

		if healthMatches(addr, restartID) {
			logger.Printf("新进程已就绪 (pid %d)，本进程退出", cmd.Process.Pid)
			return nil
		}
		time.Sleep(150 * time.Millisecond)
	}

	// 超时仍未就绪：杀掉它，避免留下一个半死不活的进程占着端口
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
	return fmt.Errorf("新进程在 %v 内未通过健康检查", relaunchHandshakeTimeout)
}

// relaunchIDFlag 是把交接标识传给子进程的命令行参数名。
//
// 用参数传而不是环境变量/配置文件：它只对**本次交接**有意义，
// 活着的时间就是这次进程交接，不该进配置、也不该落盘。
const relaunchIDFlag = "--restart-id"

// relaunchOriginFlag 是把"面板当前 origin"传给子进程的命令行参数名。
//
// 子进程据此把旧 origin 加进 /healthz 的 CORS 白名单。
// 它同样只对本次交接有意义（重启完成后旧地址就没人用了）。
const relaunchOriginFlag = "--panel-origin"

// ⚠️ 曾有一个 relaunchAddrFlag = "--addr-override"，**已删除**。
//
//	它当时是为了满足"flag > 配置 在重启后也成立"，但与
//	"面板改端口必须生效"直接矛盾：父 --addr 8881 → 面板改成 8882
//	→ plannedListenAddr 给出 8882（面板被告知去 8882），
//	而子进程收到 --addr 8881 实际监听 8881 → **服务与面板告知的地址不一致**。
//	端到端验收抓到了它，整个机制已撤除。
//
//	现在的原则：**子进程不带 --addr**，一律按"配置 > 默认"解析，
//	与 plannedListenAddr 共用同一套规则，从根上避免两处口径分叉。

// healthMatches 探测 addr 上的 /healthz 并**校验四条件**。
//
// 四条件（与面板端一致，详见 health.go）：
//
//	HTTP 200 && service=="wbapi" && ready==true && restartId==wantID
//
// 只判 200 是不够的：实测"恰好占着端口、返回通配 CORS + 200 的无关程序"
// 会让仅检查状态码的实现误判成功。
//
// 🔴 wantID 为空时**直接判不健康**，不"跳过校验"。
//
//	这里刻意不用"空 = 不校验"的宽松语义：那条路径会让调用方漏传标识时
//	静默退化成"只要 200 就算成功"，而 200 恰恰是本轮实测证明不可信的判据。
//	重启握手**必须**带标识（serve.go 已保证拿不到标识就放弃重启），
//	所以空值只会出现在误用时 —— 此时失败比放行安全。
func healthMatches(addr, wantID string) bool {
	if wantID == "" {
		return false
	}
	client := &http.Client{Timeout: 800 * time.Millisecond}
	resp, err := client.Get("http://" + addr + HealthPath)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}

	// 限制读取长度：对面若不是我们的服务，不该让它把内存读爆。
	var h healthResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&h); err != nil {
		return false
	}
	if h.Service != healthServiceName || !h.Ready {
		return false
	}
	// 对面是**别的**进程（可能是上次重启遗留的旧进程）→ 不算成功。
	return h.RestartID == wantID
}

// relaunchHandshakeTimeout 是健康握手的总超时。
//
// 取 15 秒：要给"冷启动 + 拉模型目录"留出时间（实测正常启动约 2-3 秒，
// 首次拉取上游模型目录可能更久）。比 v2 的固定 1.5 秒宽松得多，
// 因为现在判据是"真的就绪"而不是"没退出"。
const relaunchHandshakeTimeout = 15 * time.Second

// restoreAfterFailedRestart 在重启失败后把服务重新拉起来并继续跑。
//
// 🔴 Codex 第 11 轮指出 v2 的恢复是假的：只重新 Listen，
// 但后台任务（签到守护）已被 stopBackground 停掉且没有重建 ——
// 恢复后的服务少了功能，用户看不出来。
//
// 所以本函数做三件事：
//  1. 用**新的** listener（原 server 已被 Shutdown，不可复用）
//  2. 用**新的** http.Server 对象（Codex：Shutdown 后原 server 不可 Serve）
//  3. **重新装配完整 deps**（含新的签到守护），而不是只重建监听
//
// rebuild 回调返回重新装配好的 Deps 与清理函数 —— 装配逻辑只有
// buildDeps 一处说了算，不在本文件复制一份。
func restoreAfterFailedRestart(logger loggerLike, addr string,
	rebuild func() (Deps, []func())) int {

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		logger.Printf("恢复失败，无法重新监听 %s: %v", addr, err)
		return 1
	}
	logger.Printf("已恢复到 %s 继续服务（重启未生效，请修正后重试）", ln.Addr())

	restored, cleanup := rebuild()
	defer func() {
		for _, fn := range cleanup {
			if fn != nil {
				fn()
			}
		}
	}()

	srv := &http.Server{
		Addr:              addr,
		Handler:           newMux(restored),
		ReadHeaderTimeout: ReadHeaderTimeout,
		ReadTimeout:       ReadTimeout,
		IdleTimeout:       IdleTimeout,
	}
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		logger.Printf("恢复后的服务退出: %v", err)
		return 1
	}
	return 0
}

// mustGetwd 返回工作目录；失败时返回空串（交给 exec 用默认值）。
func mustGetwd() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return wd
}

// waitForPortFree 等待端口可被监听（即已释放）。
func waitForPortFree(addr string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			_ = ln.Close()
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}
