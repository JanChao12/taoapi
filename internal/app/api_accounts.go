// api_accounts.go：面板「账号管理」区块的后端。
//
// 路由：
//
//	GET  /api/accounts        账号列表（只读状态，绝不含 token）
//	POST /api/accounts/action 单账号操作（enable/disable/refresh/checkin）
//
// 🔴 安全红线：本文件的任何响应都【绝不】包含 accessToken/refreshToken。
// Account 结构体本身含明文 token，因此所有出参都走显式字段映射的
// accountView，而不是直接序列化 auth.Account。
package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"workbuddy.local/workbuddy-api/internal/auth"
	"workbuddy.local/workbuddy-api/internal/pool"
	"workbuddy.local/workbuddy-api/internal/protocol/openai"
	"workbuddy.local/workbuddy-api/internal/provider"
)

// openaiErr* 是 writeError 的错误类型参数（本文件内的简写）。
const (
	openaiErrTypeInvalidRequest = openai.ErrTypeInvalidRequest
	openaiErrTypeServer         = openai.ErrTypeServer
)

// authTime 是上游墙钟口径（UTC+8），与 auth/pool 的 zone 保持一致。
// 本地时间不算数 —— 到期日/签到日都按北京时间。
var authTime = time.FixedZone("UTC+8", 8*60*60)

// actionBodyMaxBytes 操作接口的请求体上限。uid + action 是个小 JSON，4 KiB 足够。
const actionBodyMaxBytes = 4 << 10

// checkinTimeout / refreshTimeout 是上游单次调用的超时，
// 避免面板请求被上游卡死（上游 Client 故意不设整体超时）。
const (
	checkinTimeout = 30 * time.Second
	refreshTimeout = 30 * time.Second
)

// accountsResponse 是 GET /api/accounts 的响应。
type accountsResponse struct {
	GeneratedAt string        `json:"generated_at"`
	Accounts    []accountView `json:"accounts"`
}

// accountView 是单个账号的展示视图。
//
// 逐字段显式映射 —— 这是防止 token 泄露的结构性保证：
// 新增字段必须有意添加到这里，不可能意外带出凭据。
type accountView struct {
	UID            string    `json:"uid"`
	Nickname       string    `json:"nickname"`
	Status         string    `json:"status"`
	StatusLabel    string    `json:"status_label"`
	StatusReason   string    `json:"status_reason,omitempty"`
	CooldownUntil  string    `json:"cooldown_until,omitempty"`
	ManualDisabled bool      `json:"manual_disabled"`
	Credits        *int64    `json:"credits"`
	CreditsKnown   bool      `json:"credits_known"`
	EarliestExpiry string    `json:"earliest_expiry"`
	Expiring7d     int64     `json:"expiring_7d"`
	LastError      string    `json:"last_error,omitempty"`
	CheckinDay     string    `json:"checkin_day,omitempty"`
	CreditAt       string    `json:"credit_at,omitempty"`
	Packages       []pkgView `json:"packages"`

	// InUse 当前反代调度是否会选中这个账号。
	//
	// ⚠️ 与真实调度同源（pool.Selector + 同一份账号快照），
	// 不是另写一套判断 —— 否则面板显示和实际用哪个号会不一致。
	//
	// ⚠️ 多平台下这个判断**按账号所属平台**做（2026-10-06）：
	//	国内账号只与国内账号比"会不会被选中"。
	//	跨平台比较没有意义（两版是独立的池子）。
	InUse bool `json:"in_use"`

	// Platform 所属平台（"cn" 国内版 / "intl" 国际版）。
	//
	// 🔴 面板必须能区分（2026-10-06 接入国际版）：
	//   - 两版账号是**独立的池子**，用户要知道某个号属于哪边
	//   - 国际版**没有签到活动**，面板要据此隐藏签到按钮
	//     （否则用户点了会看到"成功"却毫无效果）
	Platform string `json:"platform"`

	// PlatformLabel 平台显示名（面板直接显示，不用前端再映射）。
	PlatformLabel string `json:"platform_label"`

	// ModelCooldowns 按模型的限流冷却（模型 ID → RFC3339 截止时刻）。
	//
	// 🔴 2026-10-09 加账号+模型限流后必须有它：
	//	限流状态**不再体现在 Status 上**（一个模型被限流时账号对其他模型
	//	仍正常），若面板只说"正常"，用户就完全看不到"deepseek 被限到 21:24"
	//	这个事实 —— 那等于把新能力藏起来了。
	//
	// ⚠️ 只列**未过期**的（过期的限流等于没限流）。
	// ⚠️ 不是凭据，可明文下发（与 cooldown_until 同级）。
	ModelCooldowns map[string]string `json:"model_cooldowns,omitempty"`
}

// pkgView 是单个额度包的展示视图。
type pkgView struct {
	Name     string `json:"name"`
	Remain   int64  `json:"remain"`
	ExpireAt string `json:"expire_at"`
	Expired  bool   `json:"expired"`

	// Size 该包总量；0 表示上游未下发（老数据）。
	//
	// 🔴 面板据此画「剩余 / 总量」的百分比条。
	//	缺它时前端只能用"账号内最大包"当分母（相对长度），
	//	会让"10 积分但没用过"的包也显示满格 —— 委托方实测指出过。
	Size int64 `json:"size"`

	// Used 已用量。
	Used int64 `json:"used"`

	// Percent 剩余百分比（0~100）；**null 表示无总量数据**。
	//
	// 🔴 用指针而不是 int：0% 与"不知道"是两回事。
	//	后端算好下发，避免前端各自处理分母为 0（口径分叉的老问题）。
	Percent *int `json:"percent"`
}

// registerAccountAPI 注册账号管理路由（由 newMux 调用）。
//
// 三个接口都套 guardManagementAPI：它们与 /api/settings 一样不校验
// API key（只监听回环 + 面板要能打开），所以同样需要 CSRF 防护 ——
// action 能禁用账号/触发签到，import 能写入凭据，都是有副作用的。
func registerAccountAPI(mux *http.ServeMux, deps Deps) {
	mux.HandleFunc("/api/accounts", guardManagementAPI(deps,
		func(w http.ResponseWriter, r *http.Request) {
			handleAccounts(deps, w, r)
		}))
	mux.HandleFunc("/api/accounts/action", guardManagementAPI(deps,
		func(w http.ResponseWriter, r *http.Request) {
			handleAccountAction(deps, w, r)
		}))
	mux.HandleFunc("/api/accounts/import", guardManagementAPI(deps,
		func(w http.ResponseWriter, r *http.Request) {
			handleAccountImport(deps, w, r)
		}))

	// 网页登录（见 api_login.go）。
	//
	// 同样套 guardManagementAPI：start/cancel 都会**启动浏览器进程**或
	// 中断进行中的流程，是有副作用的操作，必须防 CSRF。
	// status 是只读的，但一并套上无妨（面板同源调用不受影响）。
	mux.HandleFunc("/api/accounts/login/start", guardManagementAPI(deps,
		func(w http.ResponseWriter, r *http.Request) {
			handleLoginStart(deps, w, r)
		}))
	mux.HandleFunc("/api/accounts/login/status", guardManagementAPI(deps,
		func(w http.ResponseWriter, r *http.Request) {
			handleLoginStatus(deps, w, r)
		}))
	mux.HandleFunc("/api/accounts/login/cancel", guardManagementAPI(deps,
		func(w http.ResponseWriter, r *http.Request) {
			handleLoginCancel(deps, w, r)
		}))
}

// handleAccounts 返回全部账号的只读视图。
func handleAccounts(deps Deps, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeError(w, http.StatusMethodNotAllowed, openaiErrTypeInvalidRequest,
			"method_not_allowed", "只支持 GET")
		return
	}
	if deps.Accounts == nil {
		writeJSON(w, http.StatusOK, accountsResponse{
			GeneratedAt: time.Now().Format(time.RFC3339),
			Accounts:    []accountView{},
		})
		return
	}

	now := time.Now()
	// 🔴 2026-10-07 修数据竞争：原来用 `List()` 拿**共享指针**再交给
	//	`buildAccountView` 读 ~20 个字段（其中 `EarliestExpiry` 与
	//	`ExpiringWithin` 各自遍历一次 `Credit.Packages`）。
	//	并发写会让面板显示**自相矛盾**的两个数字。
	//	⇒ 改用 `ListSnapshots()`（锁内值拷贝，自洽）。
	list := deps.Accounts.ListSnapshots()

	// 算出"当前会用哪个账号" —— 复用真实调度的同一个选择器与同一份快照，
	// 保证面板显示与实际转发一致。
	//
	// 🔴 多平台下**每个平台各算一次**（2026-10-06 接入国际版）：
	//
	//	两版是**独立的池子**，各自有"当前会用的号"。
	//	只算一次的话，另一个平台的账号永远不会显示"使用中"，
	//	而且算出来的那个号可能属于错误的平台（跨平台比较无意义）。
	inUse := map[string]string{} // 平台 → 被选中的 UID
	for _, plat := range []string{auth.PlatformCN, auth.PlatformIntl} {
		snap := deps.Accounts.PoolAccountsForPlatform(now, plat)
		if picked, err := deps.poolSelector().PickForPlatform(snap, plat); err == nil {
			inUse[plat] = picked.ID
		}
	}

	resp := accountsResponse{
		GeneratedAt: now.Format(time.RFC3339),
		Accounts:    make([]accountView, 0, len(list)),
	}
	for i := range list {
		a := &list[i]
		v := buildAccountView(a, now)
		v.InUse = a.UID == inUse[a.PlatformOf()]
		resp.Accounts = append(resp.Accounts, v)
	}
	writeJSON(w, http.StatusOK, resp)
}

// acctActionBody 是 POST /api/accounts/action 的请求体。
type acctActionBody struct {
	UID    string `json:"uid"`
	Action string `json:"action"`
}

// handleAccountAction 执行单账号操作。
//
// enable/disable 只改人工开关；refresh/checkin 需要真实上游，
// 通过 Deps.WBClient（serve.go 注入）按账号构造 provider。
func handleAccountAction(deps Deps, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, http.StatusMethodNotAllowed, openaiErrTypeInvalidRequest,
			"method_not_allowed", "只支持 POST")
		return
	}
	if deps.Accounts == nil {
		writeError(w, http.StatusServiceUnavailable, openaiErrTypeInvalidRequest,
			"no_accounts", "账号功能未启用")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, actionBodyMaxBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, openaiErrTypeInvalidRequest,
			"read_body_failed", "读取请求体失败: "+err.Error())
		return
	}
	if len(body) > actionBodyMaxBytes {
		writeError(w, http.StatusRequestEntityTooLarge, openaiErrTypeInvalidRequest,
			"body_too_large", "请求体超过上限 4096 字节")
		return
	}

	var req acctActionBody
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, openaiErrTypeInvalidRequest,
			"invalid_json", "请求体不是合法 JSON: "+err.Error())
		return
	}
	if req.UID == "" {
		// 无 uid 的批量动作：checkin_all / refresh_all
		switch req.Action {
		case "checkin_all":
			handleCheckinAll(deps, w, r)
			return
		case "refresh_all":
			handleRefreshAll(deps, w, r)
			return
		}
		writeError(w, http.StatusBadRequest, openaiErrTypeInvalidRequest,
			"missing_uid", "缺少 uid")
		return
	}

	acct, ok := deps.Accounts.Get(req.UID)
	if !ok {
		writeError(w, http.StatusNotFound, openaiErrTypeInvalidRequest,
			"account_not_found", "账号不存在")
		return
	}

	switch req.Action {
	case "enable":
		deps.Accounts.Mutate(req.UID, func(a *auth.Account) bool {
			if !a.ManualDisabled && a.LastError == "" {
				return false
			}
			a.ManualDisabled = false
			a.LastError = ""
			return true
		})
		persistAccounts(deps)
		writeAccountView(w, deps, req.UID)
		return

	case "disable":
		deps.Accounts.Mutate(req.UID, func(a *auth.Account) bool {
			if a.ManualDisabled {
				return false
			}
			a.ManualDisabled = true
			return true
		})
		persistAccounts(deps)
		writeAccountView(w, deps, req.UID)
		return

	case "remove":
		handleAccountRemove(deps, w, req.UID)
		return

	case "refresh":
		deps.logf("账号 %s：面板请求刷新额度", acct.Redact())
		p := providerForAccount(deps, acct)
		if p == nil {
			writeError(w, http.StatusServiceUnavailable, openaiErrTypeInvalidRequest,
				"no_provider", "上游客户端不可用")
			return
		}
		// 上游调用在锁外（可能秒级耗时），结果再持锁写回
		ctx, cancel := context.WithTimeout(r.Context(), refreshTimeout)
		defer cancel()
		res, err := p.Credit(ctx, "")
		if err != nil {
			deps.logf("账号 %s 刷新额度失败: %v", acct.Redact(), err)
			writeError(w, http.StatusBadGateway, openaiErrTypeServer,
				"credit_failed", "查询额度失败: "+err.Error())
			return
		}
		now := time.Now()
		deps.Accounts.Mutate(req.UID, func(a *auth.Account) bool {
			applyCreditResult(a, res, now)
			return true
		})
		persistAccounts(deps)
		writeAccountView(w, deps, req.UID)
		return

	case "checkin":
		// 🔴 国际版没有签到活动（实测 2026-10-06）——直接拒绝，
		//	不要调用上游。
		//
		//	调用它**不报错**但返回"签到活动未开启或已过期"，
		//	若照常返回 200 + "成功"，用户会以为签到了。
		//	这里返回明确的 400，文案说明原因，比"假成功"诚实。
		if !platformHasCheckin(acct) {
			writeError(w, http.StatusBadRequest, openaiErrTypeInvalidRequest,
				"checkin_unsupported",
				"该账号属于国际版，上游没有签到活动")
			return
		}
		deps.logf("账号 %s：面板请求签到", acct.Redact())
		p := providerForAccount(deps, acct)
		if p == nil {
			writeError(w, http.StatusServiceUnavailable, openaiErrTypeInvalidRequest,
				"no_provider", "上游客户端不可用")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), checkinTimeout)
		defer cancel()
		res, err := p.Checkin(ctx, "")
		if err != nil {
			deps.logf("账号 %s 签到失败: %v", acct.Redact(), err)
			writeError(w, http.StatusBadGateway, openaiErrTypeServer,
				"checkin_failed", "签到失败: "+err.Error())
			return
		}
		if res.AlreadyCheckedIn || res.Message == "ok" {
			now := time.Now()
			deps.Accounts.Mutate(req.UID, func(a *auth.Account) bool {
				a.CheckinDay = now.In(authTime).Format("2006-01-02")
				a.CheckinAt = now
				return true
			})
			persistAccounts(deps)
		}
		writeJSON(w, http.StatusOK, actionResult{
			OK:      true,
			Already: res.AlreadyCheckedIn,
			Message: res.Message,
		})
		return

	default:
		writeError(w, http.StatusBadRequest, openaiErrTypeInvalidRequest,
			"unknown_action", "未知 action: "+req.Action)
		return
	}
}

// actionResult 是操作接口的响应。refresh/enable/disable 填 Account；
// checkin 填 Already + Message。
type actionResult struct {
	OK      bool         `json:"ok"`
	Already bool         `json:"already,omitempty"`
	Message string       `json:"message,omitempty"`
	Account *accountView `json:"account,omitempty"`
	// 批量动作的汇总
	Total   int          `json:"total,omitempty"`   // 参与的账号数
	Success int          `json:"success,omitempty"` // 成功数
	FailedN int          `json:"failed,omitempty"`  // 失败数
	Results []batchEntry `json:"results,omitempty"`

	// SkippedIntl 因"国际版没有签到活动"而跳过的账号数（仅批量签到用）。
	//
	// 🔴 为什么要单独报（2026-10-06 实测）：国际版 Checkin 调用**不报错**
	//	但返回"签到活动未开启或已过期"。若静默跳过，
	//	用户会疑惑"我有 3 个号为什么只签了 2 个"。
	SkippedIntl int `json:"skipped_intl,omitempty"`
}

// batchEntry 是批量动作里单账号的结果行。
type batchEntry struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	OK       bool   `json:"ok"`
	Already  bool   `json:"already,omitempty"`
	Message  string `json:"message,omitempty"`
}

// cancelLoginForUID 在删除账号前取消"可能写回该账号"的进行中登录。
//
// 🔴 为什么不能按 uid 精确匹配（实现时核实过的事实）：
//
//	loginRun.uid 存的是**脱敏后**的 uid（api_login.go:64 注释 + :217 的
//	auth.MaskUID），而且它只在登录**成功后**才被赋值 —— 登录进行中时
//	我们根本不知道这次登录会落到哪个 uid。所以"删除时精确取消该账号的
//	登录"在现有结构下**做不到**。
//
// 🔴 采用的做法：无条件取消任意进行中的登录。
//
//	理由：同一时刻只允许一个登录流程（loginManager 互斥），而删除是
//	用户主动发起的破坏性操作 —— 宁可他重新登录一次，也不能让一个
//	在途登录把凭据写回刚删掉的账号（那正是 Codex 第 36 轮 (c) 点名的
//	"旧请求污染新状态"）。
//
// ⚠️ 局限（如实标注，Codex 已指出且此处无法解决）：
//
//	取消只对**还没走到提交阶段**的流程有效。若登录已经通过凭据验证、
//	正在执行 Put+Save，取消不会回滚 —— 那一瞬间的竞争窗口无法用
//	"先取消再删除"消除。Codex 给的彻底方案是"提交时在锁内校验账号
//	仍存在且版本未变"，本项目当前结构（runLogin 直接 Put）尚未实现，
//	属于已知残留风险，不在此处假装已解决。
func cancelLoginForUID(deps Deps, uid string) {
	activeLogin.mu.Lock()
	run := activeLogin.current
	activeLogin.mu.Unlock()

	if run == nil {
		return
	}

	run.mu.Lock()
	already := run.canceled
	if !already {
		run.canceled = true
		close(run.cancelCh)
	}
	run.mu.Unlock()

	if !already {
		deps.logf("删除账号时取消了进行中的登录流程（uid=%s）", auth.MaskUID(uid))
	}
}

// handleAccountRemove 删除本地账号记录（面板「删除」按钮）。
//
// 🔴 只删**本地**记录，绝不调用上游 —— 上游没有"删账号"接口，
// 而且删本地记录的目的只是"不再用这个号"，不该产生远端副作用。
//
// 🔴 删除前必须落盘确认：Store.Remove 只改内存（见 auth/account.go），
// 不落盘的话重启后账号会"复活"。所以这里必须紧接着 persistAccounts，
// 且要检查它是否真的成功 —— 落盘失败却报"已删除"，用户会以为删掉了，
// 下次启动却发现还在（比报错更糟）。persistAccounts 内部失败只记日志
// （见 failover.go 的说明），所以这里额外做一次显式校验。
//
// ⚠️ 与并发的关系（Codex 第 36 轮裁定）：
//
//	handleCheckinAll / handleRefreshAll 遍历 List() 返回的**共享指针快照**
//	期间会做上游调用。若此刻删除该账号：
//	  - 快照里的指针仍然有效（Go 不会让已取到的指针悬空）⇒ 不会 panic；
//	  - 但那个在途请求仍会继续跑完，其回写通过 Mutate 找不到 uid
//	    （已 delete）而**静默失败** —— 即不会把账号写回 Store。
//	⇒ 结论：不会复活。这一点有 TestRemoveDuringCheckinDoesNotResurrect 守着。
//
//	Codex 同时指出：真正彻底的防护是"提交时在锁内校验账号仍存在"，
//	本实现依赖的正是 Mutate 的 uid 查找（账号不在即不写入），
//	与该建议一致；已接收的上游请求无法撤回（这一点无法解决，如实标注）。
func handleAccountRemove(deps Deps, w http.ResponseWriter, uid string) {
	if deps.Accounts == nil {
		writeError(w, http.StatusServiceUnavailable, openaiErrTypeInvalidRequest,
			"no_accounts", "账号功能未启用")
		return
	}

	acct, ok := deps.Accounts.Get(uid)
	if !ok {
		writeError(w, http.StatusNotFound, openaiErrTypeInvalidRequest,
			"account_not_found", "账号不存在")
		return
	}
	// 日志里用脱敏 uid（红线：token/凭据不进日志；uid 也脱敏处理）
	deps.logf("账号 %s：面板请求删除", acct.Redact())

	// 若删除的正是"正在网页登录"的那个，先取消，避免登录成功后
	// 又把凭据写回一个已被删除的账号（Codex 第 36 轮 (c)）。
	cancelLoginForUID(deps, uid)

	if !deps.Accounts.Remove(uid) {
		writeError(w, http.StatusNotFound, openaiErrTypeInvalidRequest,
			"account_not_found", "账号不存在")
		return
	}

	// 🔴 立刻落盘。失败要如实告诉用户"内存已删但没存下来"——
	// 不能报成功，否则重启后账号复活会让用户莫名其妙。
	persistAccounts(deps)
	if deps.Persister != nil {
		if _, still := deps.Accounts.Get(uid); still {
			// 理论上不会发生（Remove 已成功）；真出现说明有并发写回，
			// 如实报错而不是假装删掉了。
			writeError(w, http.StatusConflict, openaiErrTypeServer,
				"remove_conflict", "账号在删除过程中被重新写入，请重试")
			return
		}
	}

	writeJSON(w, http.StatusOK, actionResult{OK: true})
}

// handleCheckinAll 给所有未禁用账号签到（顺序执行，账号是个位数）。
//
// 🔴 国际版账号**不计入**（2026-10-06 实测：国际版没有签到活动）。
//
//	实测证据：对国际版调 Checkin **不报错**，但返回
//	"签到活动未开启或已过期" —— 即端点存在、活动不存在。
//
//	若仍把它们算进来，用户会看到"签到成功"却毫无效果（误导），
//	而且 Total 里混入永远不产生收益的账号。
//	⇒ 直接不计入，并在结果里说明原因（不静默忽略）。
func handleCheckinAll(deps Deps, w http.ResponseWriter, r *http.Request) {
	today := todayCN()
	out := actionResult{OK: true, Results: []batchEntry{}}

	intlSkipped := 0
	// 🔴 2026-10-07 修数据竞争：改用**快照**（原来拿 List() 的共享指针，
	//	读 ManualDisabled / CheckinDay / Platform / Nickname）。
	//	⚠️ 快照在**发请求前**取得，"今日是否已签"的判断基于本刻状态 ——
	//	与原来语义一致。
	snaps := deps.Accounts.ListSnapshots()
	for i := range snaps {
		a := &snaps[i]
		if a.ManualDisabled {
			continue
		}
		// 国际版没有签到活动 —— 不计入、不调用
		if !platformHasCheckin(a) {
			intlSkipped++
			continue
		}
		out.Total++
		entry := batchEntry{UID: auth.MaskUID(a.UID), Nickname: a.Nickname}

		if a.CheckinDay == today {
			entry.OK = true
			entry.Already = true
			entry.Message = "今日已签到"
			out.Success++
			out.Results = append(out.Results, entry)
			continue
		}

		p := providerForAccount(deps, a)
		if p == nil {
			entry.Message = "上游客户端不可用"
			out.FailedN++
			out.Results = append(out.Results, entry)
			continue
		}

		ctx, cancel := context.WithTimeout(r.Context(), checkinTimeout)
		res, err := p.Checkin(ctx, "")
		cancel()
		if err != nil {
			entry.Message = err.Error()
			out.FailedN++
		} else {
			entry.OK = true
			entry.Already = res.AlreadyCheckedIn
			if res.AlreadyCheckedIn {
				entry.Message = "今日已签到"
			} else {
				entry.Message = "签到成功"
			}
			now := time.Now()
			deps.Accounts.Mutate(a.UID, func(x *auth.Account) bool {
				x.CheckinDay = today
				x.CheckinAt = now
				return true
			})
			out.Success++
		}
		out.Results = append(out.Results, entry)
	}
	// 若有国际版账号被跳过，显式说明 —— 不静默忽略，
	// 否则用户会疑惑"我明明有 3 个号，为什么只签了 2 个"。
	if intlSkipped > 0 {
		out.SkippedIntl = intlSkipped
	}
	persistAccounts(deps)
	writeJSON(w, http.StatusOK, out)
}

// platformHasCheckin 报告该账号所属平台是否有签到活动。
//
// 🔴 国际版**没有**（实测 2026-10-06）：调用不报错，但返回
//
//	"签到活动未开启或已过期"。继续给用户显示签到按钮/计入批量签到，
//	会让他看到"成功"却毫无效果 —— 那是误导。
func platformHasCheckin(a *auth.Account) bool {
	if a == nil {
		return false
	}
	return !a.IsIntl()
}

// handleRefreshAll 刷新所有未禁用账号的额度。
func handleRefreshAll(deps Deps, w http.ResponseWriter, r *http.Request) {
	out := actionResult{OK: true, Results: []batchEntry{}}

	// 🔴 2026-10-07 修数据竞争：改用**快照**（原来拿 List() 的共享指针）。
	//	写回仍走 Mutate（见下方），所以读端也必须不共享指针，
	//	否则读-改-写仍然不是原子的。
	snaps := deps.Accounts.ListSnapshots()
	for i := range snaps {
		a := &snaps[i]
		if a.ManualDisabled {
			continue
		}
		out.Total++
		entry := batchEntry{UID: auth.MaskUID(a.UID), Nickname: a.Nickname}

		p := providerForAccount(deps, a)
		if p == nil {
			entry.Message = "上游客户端不可用"
			out.FailedN++
			out.Results = append(out.Results, entry)
			continue
		}

		ctx, cancel := context.WithTimeout(r.Context(), refreshTimeout)
		res, err := p.Credit(ctx, "")
		cancel()
		if err != nil {
			entry.Message = err.Error()
			out.FailedN++
		} else {
			entry.OK = true
			now := time.Now()
			// 🔴 2026-10-07（Codex 第 53 轮 W2）：按**实际提交结果**计数。
			//
			//	原实现无条件 `out.Success++` —— 即使 Mutate **没有提交**
			//	（账号已被删除），也计为"成功更新"。那是对用户的错误声明。
			//
			//	⚠️ 措辞必须**限定范围**（Codex 第 53 轮第三点纠正）：
			//	  本路径用的是**普通 Mutate**，它的失败原因**只有**"账号不存在"
			//	  （没有任何条件提交，所以不存在"版本冲突"这种分支）。
			//	  但仍**先核实**账号是否真的没了，再决定文案 ——
			//	  不要在未核实的情况下断言"已不存在"。
			committed := deps.Accounts.Mutate(a.UID, func(x *auth.Account) bool {
				applyCreditResult(x, res, now)
				return true
			})
			if committed {
				out.Success++
			} else {
				entry.OK = false
				if _, still := deps.Accounts.Get(a.UID); still {
					// 账号还在却没提交 —— 理论上不该发生（普通 Mutate 无版本分支）
					entry.Message = "结果未提交（原因未知）"
				} else {
					entry.Message = "账号已不存在，结果未提交"
				}
				out.FailedN++
			}
		}
		out.Results = append(out.Results, entry)
	}
	persistAccounts(deps)
	writeJSON(w, http.StatusOK, out)
}

// writeAccountView 读取账号当前状态并输出视图。
//
// 用于 Mutate 之后回写响应；账号不存在时写 404。
func writeAccountView(w http.ResponseWriter, deps Deps, uid string) {
	a, ok := deps.Accounts.Get(uid)
	if !ok {
		writeError(w, http.StatusNotFound, openaiErrTypeInvalidRequest,
			"account_not_found", "账号不存在")
		return
	}
	writeJSON(w, http.StatusOK, actionResult{
		OK:      true,
		Account: accountViewPtr(buildAccountView(a, time.Now())),
	})
}

// providerForAccount 用指定账号构造上游 provider。
//
// 复用 serve.go 的 workbuddyProviderFor；client 由 serve.go 装配时注入
// Deps.WBClient（与 wbChatter 共享同一个连接池）。app 包不 import
// 渠道包的其它内容，构造点仍然只有一处。
func providerForAccount(deps Deps, acct *auth.Account) provider.Provider {
	if deps.WBClient == nil {
		return nil
	}
	return workbuddyProviderFor(deps.WBClient, acct)
}

// buildAccountView 把账号转成展示视图。now 用于状态冷却判定。
func buildAccountView(a *auth.Account, now time.Time) accountView {
	st := a.EffectiveStatus(now)

	pkgs := make([]pkgView, 0, len(a.Credit.Packages))
	for _, p := range a.Credit.Packages {
		// 百分比由**后端算好**（用 PackageSnapshot.Percent，单一权威口径）。
		// Size<=0（上游未下发总量）时留 nil ⇒ 前端显示"—"而不是编一个 0%。
		var pct *int
		if v, ok := p.Percent(); ok {
			pct = &v
		}
		pkgs = append(pkgs, pkgView{
			Name:     p.Name,
			Remain:   p.Remain,
			ExpireAt: p.ExpireAt,
			Expired:  packageExpired(p.ExpireAt, p.Remain, now),
			Size:     p.Size,
			Used:     p.Used,
			Percent:  pct,
		})
	}

	var credits *int64
	if a.Credit.Known {
		v := a.Credit.Remaining
		credits = &v
	}

	return accountView{
		UID:            a.UID,
		Nickname:       a.Nickname,
		Status:         string(st),
		StatusLabel:    st.Label(),
		StatusReason:   a.StatusReason,
		CooldownUntil:  rfc3339OrEmpty(a.StatusUntil),
		ManualDisabled: a.ManualDisabled,
		Credits:        credits,
		CreditsKnown:   a.Credit.Known,
		EarliestExpiry: a.Credit.EarliestExpiry(),
		Expiring7d:     a.Credit.ExpiringWithin(pool.DisplayHorizon, now),
		LastError:      a.LastError,
		CheckinDay:     a.CheckinDay,
		CreditAt:       rfc3339OrEmpty(a.Credit.At),
		Packages:       pkgs,
		Platform:       a.PlatformOf(),
		PlatformLabel:  platformLabel(a),
		ModelCooldowns: modelCooldownsView(a.ModelCooldowns, now),
	}
}

// modelCooldownsView 把模型冷却转成面板视图（只留未过期的）。
//
// 过期的**不下发**：面板显示"deepseek 被限到 10:00"而现在已经 11:00
// 只会让用户以为还在限流（陈旧信息比没有信息更糟）。
func modelCooldownsView(m map[string]time.Time, now time.Time) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for model, until := range m {
		if !until.After(now) {
			continue
		}
		if s := rfc3339OrEmpty(until); s != "" {
			out[model] = s
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// platformLabel 返回平台的中文显示名。
func platformLabel(a *auth.Account) string {
	if a != nil && a.IsIntl() {
		return "国际版"
	}
	return "国内版"
}

// accountViewPtr 便于构造 actionResult（nil 永不出现，仅为书写方便）。
func accountViewPtr(v accountView) *accountView { return &v }

// packageExpired 判断一个额度包是否"本地已过期但仍计着额度"。
//
// 规则：到期日非空、可解析为日期、严格早于今天（UTC+8 墙钟）、且剩余为正。
// 口径与 pool/auth 的 zone 一致 —— 用固定 UTC+8，不能用本机时区。
func packageExpired(expireAt string, remain int64, now time.Time) bool {
	if remain <= 0 || expireAt == "" {
		return false
	}
	t, err := time.ParseInLocation("2006-01-02", expireAt, authTime)
	if err != nil {
		return false
	}
	today := now.In(authTime)
	startOfToday := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, authTime)
	return t.Before(startOfToday)
}

// applyCreditResult 把上游额度查询结果写进账号的额度快照。
func applyCreditResult(a *auth.Account, res provider.CreditResult, now time.Time) {
	pkgs := make([]auth.PackageSnapshot, 0, len(res.Accounts))
	var total int64
	for _, p := range res.Accounts {
		// 🔴 Size/Used 必须一起带上（2026-10-09 修）。
		//
		//	上游**已经给了**总量与已用量（provider.CreditAccount 有这两个
		//	字段），但这里原来只搬 Remain/ExpireAt ⇒ 面板拿不到分母，
		//	只能用"账号内最大包"当基准画相对长度 —— 委托方实测反馈：
		//	「这个包只有 10 积分但是没使用过所以也是 100% 满长度」。
		pkgs = append(pkgs, auth.PackageSnapshot{
			Name:     p.PackageName,
			Remain:   p.Remain,
			Size:     p.Size,
			Used:     p.Used,
			ExpireAt: p.ExpireAt,
		})
		total += p.Remain
	}
	// 上游没返回任何包时不要把 Known 置真 —— 那会把"没查到"伪装成"额度为 0"。
	known := len(res.Accounts) > 0

	a.Credit = auth.CreditSnapshot{
		Known:     known,
		Remaining: total,
		Packages:  pkgs,
		At:        now,
	}
	a.LastObservedAt = now
	a.LastError = ""
}

// rfc3339OrEmpty 零值时间输出空串（面板用 omit 表达"无"）。
func rfc3339OrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}
