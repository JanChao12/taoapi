package app

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"runtime"
	"time"

	"workbuddy.local/workbuddy-api/internal/auth"
	"workbuddy.local/workbuddy-api/internal/config"
	"workbuddy.local/workbuddy-api/internal/pool"
	"workbuddy.local/workbuddy-api/internal/protocol/openai"
	"workbuddy.local/workbuddy-api/internal/provider"
	"workbuddy.local/workbuddy-api/internal/provider/workbuddy"
	"workbuddy.local/workbuddy-api/internal/router"
	usagepkg "workbuddy.local/workbuddy-api/internal/usage"
)

// Deps 是服务运行所需的依赖。
//
// 用显式注入而不是全局变量：测试可以传入假 provider。
type Deps struct {
	// Router 模型路由；为 nil 时 /v1/models 返回空列表并记录警告。
	Router *router.Router

	// Logger 日志。
	Logger *log.Logger

	// Verbose 详细日志。
	Verbose bool

	// Usage 用量事件存储；为 nil 时统计接口返回空数据。
	Usage *usagepkg.Store

	// Accounts 账号仓库；为 nil 时不启用换号调度（退化到单 provider）。
	Accounts *auth.Store

	// Persister 账号持久化器；为 nil 时状态变更不落盘。
	Persister *auth.Persister

	// Chatter 按账号发起对话的能力；为 nil 时走退化路径。
	Chatter accountChatter

	// WBClient WorkBuddy 上游客户端（共享连接池）；
	// 账号管理 API 的 refresh/checkin 用它按账号构造 provider。为 nil 时不可用。
	WBClient *workbuddy.Client

	// Settings 本地设置存储（面板「设置」页的后端）。
	//
	// 为 nil 时：/api/settings 返回 503，且 /v1/* 【不鉴权】
	// （等价于"没设 key"，这是刻意的退化 —— 测试里常用）。
	Settings *config.Store

	// ListenPort 当前实际监听的端口。
	//
	// 只用于判断"配置里的端口是否需要重启才生效"，不参与监听本身。
	// 为 0 表示未知（测试环境），此时不提示重启。
	ListenPort int

	// applyAutoCheckin 在自动签到开关变化时被调用，使其立即生效。
	//
	// 为 nil 时开关仍会保存，只是不即时改变运行中的调度
	// （测试与未启用签到的场景）。
	applyAutoCheckin func(enabled bool)

	// Checkin 自动签到守护（可为 nil）。
	//
	// 只用于把运行状态暴露给面板（"已启用/上次触发/结果"），
	// 真正的启停由 applyAutoCheckin 驱动。
	Checkin *checkinDaemon

	// Keepalive 凭证保活守护（可为 nil）。
	//
	// 定时检查各账号 access token 是否临近过期并续期，
	// 让账号**不必重复登录**。见 keepalive.go 的说明与限制。
	Keepalive *keepaliveDaemon

	// applyKeepalive 在保活开关变化时被调用，使其立即生效。
	//
	// 为 nil 时开关仍会保存，只是不即时改变运行中的调度（测试场景）。
	applyKeepalive func(enabled bool)

	// Update 更新检查/下载的进程内状态机（可为 nil ⇒ 接口返回 503）。
	//
	// ⚠️ 这是本项目**最危险**的功能：它把"运行远程字节"变成一次点击。
	//	设计边界见 api_update.go 文件头（https + 主机白名单 + 强制校验
	//	SHA256 + 必须用户显式点击，绝无后台静默替换）。
	Update *updateState

	// Updater 是 GitHub Release 客户端；为 nil 时按需构造。
	//
	// 抽成字段**只为测试注入**（生产用 update.NewUpdater()）。
	Updater updaterClient

	// restart 重启状态机（含通知 channel）。
	//
	// 面板改了端口后需要重启才生效，用它触发。
	// 为 nil 时重启接口返回 503（测试与嵌入式场景）。
	//
	// 为什么是状态机而不是裸 channel（Codex 第 11 轮）：
	// 裸 channel 只能表达"有没有信号"，无法让 handler 判断
	// "已有重启在进行中"并返回 409；用"channel 满就丢弃"代替状态，
	// 会让用户连点多次却不知道哪次生效。
	restart *restartState

	// panelToken 是管理接口写操作必须携带的自定义头令牌。
	//
	// 见 csrf.go 的说明：/api/* 不校验 API key，所以需要一道
	// "浏览器跨站请求带不上"的防线。进程启动时随机生成，
	// 由面板页面读取后放进请求头。
	// 为空表示不启用该防线（部分测试与进程内嵌入式用法）。
	panelToken string

	// restartID 是**本次重启交接**的标识（非重启启动时为空串）。
	//
	// 由父进程生成 → 随 202 返回给面板 → 以命令行参数传给子进程 →
	// 子进程在 /healthz 里回显。面板据此确认"对面是本次交接的那个进程"，
	// 而不是上一次重启遗留的旧进程（详见 health.go）。
	//
	// ⚠️ 它**不是凭据**：不授予任何权限，只用于关联。
	restartID string

	// healthOrigins 是 /healthz 允许读取的回环 origin 白名单。
	//
	// 必须包含【旧】origin（面板当前所在地址）与【新】origin（重启后地址）：
	// 实测证明，只允许目标服务自己的 origin 会让浏览器拒绝读取，
	// 自动重连 100% 失效（见 health.go 的坑 3）。
	//
	// 每次重启重建，不累积历次旧 origin。
	healthOrigins *allowedOrigins

	// dataDirOverride 仅供测试注入数据目录（生产走 dataDir()）。
	dataDirOverride string

	// onShutdown 服务退出时需要回收的后台任务（如签到守护）。
	//
	// ⚠️ Deps 按值传递，本切片必须在 buildDeps 返回前填充完毕；
	// 运行期追加是未定义行为（会丢失在某个副本上）。
	onShutdown []func()
}

// poolSelector 返回账号选择器。
//
// 说明：Deps 是【按值传递】的，所以不能把 sync.Once/Selector 放进结构体
// （会触发 go vet 的 copylocks 检查，也确实会共享错状态）。
// Selector 本身无状态（只有一个时间源），每次新建的成本可忽略，
// 因此不做缓存 —— 简单且没有共享问题。
func (d *Deps) poolSelector() *pool.Selector {
	return pool.NewSelector()
}

// newMux 构造路由。
func newMux(deps Deps) http.Handler {
	logger := deps.Logger
	if logger == nil {
		logger = log.Default()
	}

	mux := http.NewServeMux()

	// ── 健康检查 ──
	//
	// 响应形状与 CORS 策略见 health.go 的包注释（Codex 第 19 轮定稿）。
	// 这里只负责把它接到路由上，不重复实现判定逻辑。
	mux.Handle(HealthPath, healthHandler(deps.healthOrigins, deps.restartID))

	// ── 运行状态 ──
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			writeError(w, http.StatusMethodNotAllowed, openai.ErrTypeInvalidRequest,
				"method_not_allowed", "只支持 GET")
			return
		}
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)

		providers := []string{}
		modelCount := 0
		aliasCount := 0
		if deps.Router != nil {
			providers = deps.Router.Providers()
			// 🔴 用 ModelsWithoutAliases —— 与面板「模型列表」同一口径。
			//
			//	此前这里用 Models()（含别名），于是面板首屏「模型数」显示的是
			//	"模型 + 别名"的条数，而列表加载后又变成模型的条数 ——
			//	同一个页面上两个数字对不上，用户会以为哪边算错了。
			//	委托方原话：「模型列表删除auto后是16个模型，
			//	你的模型数也应该动态更新为16才是」。
			modelCount = len(deps.Router.ModelsWithoutAliases())
			aliasCount = deps.Router.AliasCount()
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"service":    ProductName,
			"version":    Version,
			"uptime":     time.Since(startedAt).Round(time.Second).String(),
			"providers":  providers,
			"modelCount": modelCount,
			"aliasCount": aliasCount,
			"limits": map[string]any{
				"maxConcurrentStreams": MaxConcurrentStreams,
				"maxRequestBodyBytes":  MaxRequestBodyBytes,
				"sseIdleTimeout":       SSEIdleTimeout.String(),
			},
			"memory": map[string]any{
				"heapAllocBytes": ms.HeapAlloc,
				"sysBytes":       ms.Sys,
				"numGoroutine":   runtime.NumGoroutine(),
			},
			"upstream": map[string]any{
				"chatBase":    UpstreamChatBase,
				"billingBase": UpstreamBillingBase,
				// 伪装版本：本工具最脆弱的一环（上游若改为校验版本会整体失效）。
				// 暴露当前生效值 + 是否被环境变量覆盖，便于排查"是不是版本问题"。
				// 实测（2026-10-05）：上游**不校验**这两个值 ——
				// CLI/0.0.1、99.99.99、甚至 curl/8.0 都能正常对话（见维护备忘）。
				"identity": workbuddy.IdentityVersionsInfo(),
			},
		})
	})

	// ── /v1/models ──
	// 🔴 /v1/* 全部经 requireAPIKey：设了 key 就必须带对，否则 401。
	// 未设 key（空串）时放行 —— 委托人要求"key 可以为空"。
	mux.HandleFunc("/v1/models", requireAPIKey(deps, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			writeError(w, http.StatusMethodNotAllowed, openai.ErrTypeInvalidRequest,
				"method_not_allowed", "只支持 GET")
			return
		}
		if deps.Router == nil {
			// 未配置 provider 时返回空列表而不是错误：
			// 这样客户端能正常加载，只是看不到模型。
			writeJSON(w, http.StatusOK, openai.NewModelList(nil))
			return
		}
		models := deps.Router.Models()
		writeJSON(w, http.StatusOK, openai.FromProviderModels(models))
	}))

	// ── /v1/chat/completions ──
	mux.HandleFunc("/v1/chat/completions", requireAPIKey(deps, func(w http.ResponseWriter, r *http.Request) {
		handleChat(deps, w, r)
	}))

	// ── /v1/messages（Anthropic Messages）──
	//
	// 🔴 为什么加它（委托方 2026-10-09 需求）：让反代同时服务两类客户端 ——
	//	DSH 的协议下拉框有 OpenAI Chat / OpenAI Responses / Anthropic Messages
	//	三种，此前只有第一种能用。
	//
	// 🔴 委托方拍板的三条边界：
	//   1. **只新增**本路由，/v1/chat/completions 完全不变
	//   2. 鉴权**同时接受** x-api-key 与 Authorization: Bearer
	//   3. 做不到的特性**明确报错**（不静默丢弃）
	//
	// ⚠️ 路径用 "/v1/messages" 前缀注册（不是精确匹配）：客户端会带
	//	query string（实测 DSH 发 /v1/messages?beta=true）。ServeMux 的
	//	精确模式会匹配带 query 的请求（query 不参与路径匹配），但
	//	/v1/messages/count_tokens 需要单独一条 —— 见下。
	mux.HandleFunc("/v1/messages", requireAPIKeyAny(deps, func(w http.ResponseWriter, r *http.Request) {
		handleAnthropicMessages(deps, w, r)
	}))

	// ── /v1/messages/count_tokens ──
	//
	// 🔴 为什么必须实现（参考实现提醒，我原本漏了）：
	//	Claude Code 等客户端用它做**上下文预算** —— 缺这个端点，
	//	客户端会以为服务不支持而放弃或降级行为。
	//
	// ⚠️ Go 的 ServeMux 会优先匹配更长的模式，所以这条与上面的
	//	"/v1/messages" 不冲突（更具体的胜出）。
	mux.HandleFunc("/v1/messages/count_tokens", requireAPIKeyAny(deps,
		func(w http.ResponseWriter, r *http.Request) {
			handleAnthropicCountTokens(deps, w, r)
		}))

	// ── /v1/responses（OpenAI Responses API）──
	//
	// 🔴 为什么加它：DSH 的协议下拉框三种，这是第三种。
	//	至此 OpenAI Chat / Anthropic Messages / OpenAI Responses 全部可用。
	//
	// 委托方三条边界与 anthropic 路径相同：
	//  1. **只新增**本路由，/v1/chat/completions 完全不变
	//  2. 鉴权同时接受 x-api-key 与 Authorization: Bearer
	//  3. 做不到的特性明确报错（store / previous_response_id /
	//     服务端工具 / 未知 item 类型 —— 见 protocol/responses 包注释）
	//
	// ⚠️ 与 /v1/chat/completions 的路径不冲突（不同字面量）。
	mux.HandleFunc("/v1/responses", requireAPIKeyAny(deps,
		func(w http.ResponseWriter, r *http.Request) {
			handleResponses(deps, w, r)
		}))

	// ── 面板（静态资源，go:embed）──
	mux.Handle("/panel/", panelHandler())

	// ── 面板数据接口 ──
	mux.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) {
		handleStats(deps, w, r)
	})

	// ── 调用明细（逐条记录，面板「调用记录」表）──
	//
	// 与 /api/stats 的分工：stats 是聚合汇总，本接口是逐条明细。
	// 只读，所以不需要 guardManagementAPI（参照 /status 的取舍）。
	mux.HandleFunc("/api/usage/log", func(w http.ResponseWriter, r *http.Request) {
		handleUsageLog(deps, w, r)
	})

	// ── 面板的模型列表（同源，不校验 key）──
	//
	// 🔴 为什么面板不能直接用 /v1/models：那是对外接口，被 requireAPIKey
	// 包着 —— 用户设了密钥后面板会被 401，页面显示「加载失败」。
	// 详见 api_models.go 的说明。
	mux.HandleFunc("/api/models", func(w http.ResponseWriter, r *http.Request) {
		handlePanelModels(deps, w, r)
	})

	// ── 重新拉取上游模型目录（面板「刷新」按钮的**真正**后端）──
	//
	// 🔴 为什么需要它（2026-10-07）：面板原有的「刷新」只调 `/api/models`，
	// 而那读的是 Router **内存**；内存只在**启动时**写入一次
	// （serve.go 的 registerOnePlatform）。⇒ 上游改了模型，点刷新看不到，
	// 必须重启服务。本接口补上"服务端重拉"这一环。
	//
	// 🔴 必须走 guardManagementAPI：它会改路由表（有副作用）。
	// 项目约定：有副作用的写操作一律套 CSRF 防护（见 csrf.go），
	// 否则你浏览器里打开的恶意网页可以触发它。
	mux.HandleFunc("/api/models/refresh", guardManagementAPI(deps,
		func(w http.ResponseWriter, r *http.Request) {
			handlePanelModelsRefresh(deps, w, r)
		}))

	// ── 版本检查与更新（面板「关于/更新」）──
	//
	// 🔴 设计要点（与委托方 2026-10-09 的要求一致）：
	//   - **没有后台自动更新**：绝不会自己下载替换 exe
	//   - GET  /api/update       读状态（无副作用）
	//   - POST /api/update/check 去 GitHub 查最新版（用户点「检测新版本」）
	//   - POST /api/update/apply 下载+校验+替换+重启（用户点「更新」）
	//
	// ⚠️ check 与 apply 都有副作用 ⇒ 必须过 CSRF 防护，
	//	否则你打开的恶意网页可以诱导本机下载并替换程序。
	mux.HandleFunc("/api/update", func(w http.ResponseWriter, r *http.Request) {
		handleUpdateStatus(deps, w, r)
	})
	mux.HandleFunc("/api/update/check", guardManagementAPI(deps,
		func(w http.ResponseWriter, r *http.Request) {
			handleUpdateCheck(deps, w, r)
		}))
	mux.HandleFunc("/api/update/apply", guardManagementAPI(deps,
		func(w http.ResponseWriter, r *http.Request) {
			handleUpdateApply(deps, w, r)
		}))

	// ── 设置接口（面板「设置」页）──
	// 与 /v1/* 不同，本接口【不校验 API key】：它只监听回环，且面板要能在
	// 未设 key 时打开（否则鸡生蛋）。契约见 docs/第9轮-接口契约-冻结.md §2。
	//
	// 🔴 但"不校验 key"必须配 CSRF 防护（Codex 第 11、12 轮连续提出）：
	// 否则你浏览器里打开的恶意网页可以 fetch 到本接口改 key / 触发重启。
	// guardManagementAPI 只拦有副作用的方法，GET 不受影响。
	mux.HandleFunc("/api/settings", guardManagementAPI(deps,
		func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				handleSettingsGet(deps, w, r)
			case http.MethodPatch:
				handleSettingsPatch(deps, w, r)
			default:
				w.Header().Set("Allow", "GET, PATCH")
				writeError(w, http.StatusMethodNotAllowed, openai.ErrTypeInvalidRequest,
					"method_not_allowed", "只支持 GET/PATCH")
			}
		}))

	// ── 一键重启（端口变更后生效）──
	mux.HandleFunc("/api/settings/restart", guardManagementAPI(deps,
		func(w http.ResponseWriter, r *http.Request) {
			handleRestart(deps, w, r)
		}))

	// ── 面板写操作令牌（CSRF 主防线，见 csrf.go）──
	//
	// 同源 GET 即可取到：它本身不是秘密（要防的是"别的网站的页面
	// 拿不到它"，而不是"本机读不到"）。真正的防护来自"跨站请求带不上
	// 自定义头"，而不是令牌的保密性。
	//
	// Codex 第 13 轮要求：必须带 Cache-Control: no-store ——
	// 否则浏览器/中间层可能把令牌缓存下来，重启后旧值仍被复用。
	mux.HandleFunc("/api/panel-token", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			writeError(w, http.StatusMethodNotAllowed, openai.ErrTypeInvalidRequest,
				"method_not_allowed", "只支持 GET")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, map[string]any{"token": deps.panelToken})
	})

	// ── 账号管理接口（面板「账号」区块）──
	registerAccountAPI(mux, deps)

	// ── 其余路径 404（JSON）──
	mux.HandleFunc("/", servePanelIndex)

	return mux
}

// writeJSON 写 JSON 响应。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// writeError 写 OpenAI 兼容错误响应。
func writeError(w http.ResponseWriter, status int, errType, code, msg string) {
	writeJSON(w, status, openai.NewError(errType, code, msg))
}

// startedAt 记录进程启动时间，供 /status 计算 uptime。
var startedAt = time.Now()

// 供后续步骤使用（避免未使用导入）。
var _ = context.Background
var _ = provider.Model{}
