package app

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"workbuddy.local/workbuddy-api/internal/config"
	"workbuddy.local/workbuddy-api/internal/protocol/openai"
)

// ─────────────────────────────────────────────────────────────
// 设置 API（面板「设置」页的后端）
//
// 冻结契约见 docs/第9轮-接口契约-冻结.md §2、§3。
// ─────────────────────────────────────────────────────────────

// apiKeyHintLen 是 GET /api/settings 里回显的 key 前缀长度。
//
// 只回显几位是为了让用户能分辨"我用的是哪个 key"，
// 又不至于把完整凭据通过 HTTP 泄露给本机其他程序。
const apiKeyHintLen = 4

// settingsView 是 GET /api/settings 的响应体。
//
// 🔴 APIKey 会以**明文**返回 —— 这是 2026-10-06 委托方明确决定的例外，
// 详见 APIKey 字段上的说明。除本接口外，任何 API 响应都不得包含它
// （有 TestNonSettingsResponsesNeverContainAPIKey 守着）。
type settingsView struct {
	// APIKeySet 是否已设密钥（空 = 不校验）。
	APIKeySet bool `json:"apiKeySet"`

	// APIKeyHint 形如 "key…"，仅供识别；未设时为空串。
	//
	// 保留它是因为顶部一览（st-sum-api-key）只需要"有没有设"，
	// 不必把明文送进那个更常被渲染的位置。
	APIKeyHint string `json:"apiKeyHint"`

	// APIKey 完整密钥（明文）—— 未设时为空串。
	//
	// ═══════════════════════════════════════════════════════════
	// 🔴 为什么这里破例返回明文（2026-10-06 委托方决定，勿"好心改回去"）
	// ═══════════════════════════════════════════════════════════
	//
	// 此前本字段**不存在**，且有一条测试 TestSettingsGetNeverLeaksKey
	// 断言响应体绝不含明文 key。委托方明确要求改为明文常显，原话：
	//
	//	「密钥不需要隐藏，直接显式展出出来，方便别人复制」
	//	「A，进设置页就明文铺在屏幕上，你都能让别人随便复制了
	//	  这和明码有什么区别吗」
	//
	// 取舍内容（已向委托方说明，他知悉后仍选择明文常显）：
	//   - 收益：面板可直接看到并一键复制完整密钥，无需翻配置文件。
	//   - 代价：任何能打开本面板的本机程序都能读到这个密钥。
	//     ⚠️ 注意 /api/* 面板接口**本就不校验 API key**，所以"本机程序
	//     能调面板"是既成事实；但"能调用"≠"能读到凭据"，本改动确实
	//     新增了一条凭据读取能力（Codex 第 36 轮指出，我最初低估了这点）。
	//
	// 适用边界（超出即需重新评估，不是"永远安全"）：
	//   - 仅限**只监听 127.0.0.1 的自用部署**；
	//   - 这里返回的是**本地 API key**，**不是**上游 accessToken ——
	//     红线第 2 条（token 不进 API 响应）**未被放宽**，仍由
	//     TestUpstreamTokenNeverLeaks 等测试守住；
	//   - 若将来开放非回环访问、允许多人共用、或改变鉴权模型，
	//     **必须重新评估本例外**。
	//
	// ponytail: 回环自用 + 明文常显；开放非回环访问/多人共用是升级触发条件。
	APIKey string `json:"apiKey"`

	Port        int               `json:"port"`
	AutoCheckin bool              `json:"autoCheckin"`
	AutoStart   bool              `json:"autoStart"`
	Aliases     map[string]string `json:"aliases"`

	// ListenAddr 当前实际监听地址（host 恒为 127.0.0.1，不可配）。
	ListenAddr string `json:"listenAddr"`

	// ConfiguredPort / EffectivePort（Codex 第 13 轮要求区分）。
	//
	// 🔴 为什么要分开：端口改动**需重启才生效**。若只返回一个 port，
	// 前端会把它当成"已经在用的端口" —— 用户改完看到 9000，
	// 以为立刻生效了，实际服务还在 8787 上跑。分开之后面板能明确显示
	// "当前生效 8787 → 待生效 9000"。
	//
	//   - ConfiguredPort：配置文件里的值（用户"想要"的端口）
	//   - EffectivePort ：进程此刻真正在监听的端口；未知时为 0
	ConfiguredPort int `json:"configuredPort"`
	EffectivePort  int `json:"effectivePort"`

	// RestartRequired 端口是否已改但尚未生效。
	RestartRequired bool `json:"restartRequired"`

	// ConfigCorrupt 加载时是否发现过损坏的配置（已改名保留）。
	ConfigCorrupt bool `json:"configCorrupt"`

	// DataDir 数据目录，便于用户备份/排查。
	DataDir string `json:"dataDir"`

	// AutoStartSupported 本平台是否支持开机自启（非 Windows 为 false）。
	AutoStartSupported bool `json:"autoStartSupported"`

	// Revision 当前配置版本号（乐观锁用，Codex 第 12 轮要求）。
	//
	// ⚠️ 这只是**展示用**的整数。真正要回传给 PATCH 的是 RevisionToken ——
	// 因为后端只接受带进程代次的完整串（见 normalizeIfRevision 的说明）。
	Revision int64 `json:"revision"`

	// RevisionToken 是**可直接回传的并发令牌**（含进程代次）。
	//
	// 🔴 为什么要单独给这个字段（Codex 第 15 轮）：
	//
	//	PATCH 只接受 `inst-<代次>-rev-<n>` 形态，而 `revision` 是纯整数。
	//	若只暴露整数，前端要么自己拼（不知道代次，会拼错），
	//	要么得额外读 ETag 响应头（容易漏）。
	//	**实测后果**：面板就是照旧发了整数，保存直接失败 ——
	//	前后端契约不一致，而单测各自都"通过"。
	//
	//	直接把令牌放进响应体，前端"原样回传"即可，没有拼装空间。
	RevisionToken string `json:"revisionToken"`
}

// settingsPatch 是 PATCH /api/settings 的请求体。
//
// 🔴 指针字段的语义是【区分"未修改"与"改成零值"】（Codex 第 9 轮要求）：
// 用值类型时无法表达"我没传 autoCheckin"，会被误解成"要把它设成 false"。
type settingsPatch struct {
	APIKey      *string            `json:"apiKey"`
	APIKeyClear *bool              `json:"apiKeyClear"`
	Port        *int               `json:"port"`
	AutoCheckin *bool              `json:"autoCheckin"`
	AutoStart   *bool              `json:"autoStart"`
	Aliases     *map[string]string `json:"aliases"`

	// IfRevision 是乐观锁：客户端声明"我基于哪一版改"。
	//
	// 🔴 两种形式必须**同构**（Codex 第 14 轮指出的一致性漏洞）：
	//
	//	我最初让它只收整数（`revision=0`），而 ETag 是
	//	`"inst-<gen>-rev-<n>"`。于是"配置损坏被 quarantine 后
	//	revision 从 0 重来"时，旧客户端的 `ifRevision=0` 会**误匹配**，
	//	generation 保护形同虚设 —— 这正是加 generation 要防的场景。
	//
	//	现在两种形式承载同一串值：`inst-<gen>-rev-<n>`。
	//	为兼容"只想给个数字"的脚本，**纯整数也接受**，
	//	但会与当前 generation 拼接后再比较（即整数形式只在本进程内有效，
	//	这正符合脚本"同一次会话里连续调用"的用法）。
	//
	// 用 json.RawMessage 才能同时接住字符串与数字两种 JSON 形态。
	//
	// 缺失时返回 428（**不是"缺失即强制写入"**，Codex 第 12 轮明确否决）。
	IfRevision *json.RawMessage `json:"ifRevision"`
}

// settingsConflict 是 412 冲突时的响应体。
//
// 带上"当前 revision + 当前完整设置"，让前端能直接展示最新状态
// 或做人工合并，而不是只丢一个"版本不匹配"。
type settingsConflict struct {
	Error string `json:"error"`

	// Current 是服务端此刻的设置（已脱敏，与 GET 同构）。
	Current settingsView `json:"current"`

	// Message 给用户看的一句话。
	Message string `json:"message"`
}

// handleSettingsGet 返回脱敏后的当前设置。
func handleSettingsGet(deps Deps, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET, PATCH")
		writeError(w, http.StatusMethodNotAllowed, openai.ErrTypeInvalidRequest,
			"method_not_allowed", "只支持 GET/PATCH")
		return
	}
	if deps.Settings == nil {
		writeError(w, http.StatusServiceUnavailable, openai.ErrTypeServer,
			"no_settings", "设置存储未初始化")
		return
	}
	v := deps.settingsView()
	// ETag 与 revision 同源（Codex 第 12 轮要求）：
	// 支持 If-Match 的客户端可以直接用标准语义。
	w.Header().Set("ETag", revisionETag(v.Revision))

	// 🔴 本响应含**明文密钥**，绝不能被缓存（Codex 第 36 轮 T4）。
	// 这挡不住开发者工具/扩展/截图，但能避免"浏览器或中间层把密钥
	// 存成一份额外副本" —— 尤其 Pragma 是给只认 HTTP/1.0 的旧中间层看的。
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")

	writeJSON(w, http.StatusOK, v)
}

// revisionETag 由 revision 生成 ETag 值（带引号的标准形式）。
func revisionETag(rev int64) string {
	return `"` + revisionETagToken(rev) + `"`
}

// revisionETagToken 是不带引号的版本串，供 body 形式与比较使用。
//
// 两种形式（If-Match 头 / body 的 ifRevision）承载**同一个串** ——
// 这是 Codex 第 14 轮要求的"同构"：不能让一个带 generation、
// 另一个只有整数，否则数字版会绕过 generation 保护。
func revisionETagToken(rev int64) string {
	return fmt.Sprintf("inst-%s-rev-%d", processGeneration(), rev)
}

// processGeneration 返回本进程的代次标识（进程内只生成一次）。
var (
	genOnce sync.Once
	genVal  string
)

func processGeneration() string {
	genOnce.Do(func() {
		tok, err := newPanelToken()
		if err != nil {
			// 拿不到随机数时退化为固定值 —— 不 panic。
			// 代价只是"跨进程的 ETag 可能撞车"，属于可接受的降级。
			genVal = "nog"
			return
		}
		// 取前 8 位足够区分进程，且 ETag 不至于太长
		if len(tok) >= 8 {
			tok = tok[:8]
		}
		genVal = tok
	})
	return genVal
}

// checkPrecondition 校验乐观锁前置条件。
//
// 返回 (原因, 是否通过)。通过时原因为空串。
//
// 语义（严格按 Codex 第 12 轮）：
//   - 未提供 ifRevision 且未提供 If-Match → **428 Precondition Required**
//   - 提供了但与当前 revision 不符      → **412 Precondition Failed**
//
// 🔴 为什么缺失要拒绝而不是"缺失即强制写入"（Codex 明确否决后者）：
//
//	若缺失就放行，前端只要漏传一个字段，就重新变成无保护覆盖 ——
//	revision 白做了。要强制覆盖应该走显式的 force 机制，
//	不能把它当成默认行为（安全默认值必须偏保守）。
//
// checkPrecondition 校验乐观锁前置条件。
//
// 返回 (原因, 是否属于"请求本身有问题", 是否通过)。
//
// 第二项用来区分两种失败（Codex 第 13 轮要求）：
//   - true  → 400：请求形态非法或自相矛盾，调用方改请求即可
//   - false → 412：版本不符，属并发冲突，调用方应重新加载
//
// 🔴 用显式布尔而不是"匹配错误文案"：文案会改，
// 靠 strings.Contains 判断迟早出错（我上一版就是这么写错的）。
func checkPrecondition(deps Deps, r *http.Request, patch settingsPatch) (string, bool, bool) {
	cur := deps.Settings.Get().Revision
	want := revisionETagToken(cur) // 不带引号的形式，供 body 比较

	im := strings.TrimSpace(r.Header.Get("If-Match"))

	// 把 body 里的 ifRevision 归一成与 ETag 同构的串
	bodyTok, err := normalizeIfRevision(patch.IfRevision, cur)
	if err != nil {
		// 形态非法属于请求问题 → 400
		return err.Error(), true, false
	}

	// 🔴 同时给了两种形式且**互相矛盾** → 400（Codex 第 13 轮要求）。
	//
	// 为什么不是"任一不符即 412"：矛盾说明调用方自己搞混了
	// （或中间层改写了其一）。无论选哪个都是猜，而在有副作用的写接口上
	// "猜"是危险的。明确报错让调用方修。
	if bodyTok != "" && im != "" && im != "*" &&
		strings.Trim(im, `"`) != bodyTok {
		return "ifRevision 与 If-Match 不一致：请只提供其中一个，" +
			"或让两者表达同一版本", true, false
	}

	// 版本比对：body 形式
	if bodyTok != "" && bodyTok != want {
		return "设置已被更新（当前版本 " + want + "，你基于 " + bodyTok + "）",
			false, false
	}
	// 版本比对：标准头形式（允许带引号或不带）
	if im != "" && im != "*" && strings.Trim(im, `"`) != want {
		return "设置已被更新（当前 ETag=" + revisionETag(cur) + "）", false, false
	}

	return "", false, true
}

// normalizeIfRevision 把 body 里的 ifRevision 归一成版本串。
//
// 🔴 **只接受完整 opaque token**（Codex 第 15 轮修正了我的方案）：
//
//	我上一版让"纯整数也接受，拼接当前 generation 后比较"，
//	并以为那是安全的。**那是错的** —— 服务端无法知道这个整数
//	来自哪个进程：
//
//	    旧客户端保存 ifRevision=0
//	    新进程启动后当前 revision 也是 0
//	    新服务把它解释为 inst-NEW-rev-0 → 接受
//
//	generation 防护又被绕开了，而且绕得更隐蔽（我当时还以为修好了）。
//
//	现在整数形态**明确拒绝**（400），理由是它无法承载 generation。
//	脚本请改用 `If-Match: "inst-<gen>-rev-<n>"`，或传 GET 返回的版本串。
//
// 返回空串表示客户端没给这一项（由调用方按 428 处理）。
func normalizeIfRevision(raw *json.RawMessage, _ int64) (string, error) {
	if raw == nil {
		return "", nil
	}
	s := strings.TrimSpace(string(*raw))
	if s == "" || s == "null" {
		return "", nil
	}

	// 只接受字符串形态的 opaque token
	if len(s) >= 2 && s[0] == '"' {
		var str string
		if err := json.Unmarshal(*raw, &str); err != nil {
			return "", fmt.Errorf("ifRevision 不是合法字符串")
		}
		if !strings.HasPrefix(str, "inst-") {
			return "", fmt.Errorf(
				"ifRevision 必须是完整版本串（形如 inst-xxx-rev-3）；" +
					"缺少进程代次的旧格式已被禁用 — 也可改用 If-Match 头")
		}
		return str, nil
	}

	// 数字等其它形态：明确拒绝，不猜、不做"兼容"
	return "", fmt.Errorf(
		"ifRevision 不接受数字：它无法携带进程代次，" +
			"配置损坏重建后可能误匹配旧版本。" +
			"请传 GET 返回的完整版本串，或改用 If-Match 头")
}

// hasPrecondition 判断客户端是否提供了任一形式的乐观锁条件。
func hasPrecondition(r *http.Request, patch settingsPatch) bool {
	if patch.IfRevision != nil {
		return true
	}
	return strings.TrimSpace(r.Header.Get("If-Match")) != ""
}

// writeSettingsConflict 写 412 冲突响应，并带上服务端当前状态。
func writeSettingsConflict(deps Deps, w http.ResponseWriter, reason string) {
	w.Header().Set("ETag", revisionETag(deps.Settings.Get().Revision))
	writeJSON(w, http.StatusPreconditionFailed, settingsConflict{
		Error:   "revision_conflict",
		Current: deps.settingsView(),
		Message: reason + "；请重新加载设置后再保存",
	})
}

// itoa64 把 int64 转字符串（避免为一个转换引入 strconv）。
func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [24]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// settingsView 组装脱敏视图。
func (d Deps) settingsView() settingsView {
	cur := d.Settings.Get()
	v := settingsView{
		APIKeySet:          cur.APIKey != "",
		Port:               cur.Port,
		AutoCheckin:        cur.AutoCheckin,
		AutoStart:          cur.AutoStart,
		Aliases:            cur.Aliases,
		ListenAddr:         listenAddrForPort(d, cur.Port),
		ConfiguredPort:     cur.Port,
		EffectivePort:      d.ListenPort, // 0 表示未知（测试/嵌入式）
		RestartRequired:    d.portChanged(cur.Port),
		ConfigCorrupt:      d.Settings.Corrupt(),
		DataDir:            dataDir(),
		AutoStartSupported: autoStartSupported(),
		Revision:           cur.Revision,
		RevisionToken:      revisionETagToken(cur.Revision),
	}
	if v.Aliases == nil {
		v.Aliases = map[string]string{}
	}
	if cur.APIKey != "" {
		v.APIKeyHint = hintOf(cur.APIKey)
	}
	// 明文（2026-10-06 委托方决定；理由与边界见 settingsView.APIKey 的说明）
	v.APIKey = cur.APIKey
	return v
}

// hintOf 生成密钥提示串（前 N 位 + 省略号）。
func hintOf(key string) string {
	if len(key) <= apiKeyHintLen {
		return strings.Repeat("•", len(key))
	}
	return key[:apiKeyHintLen] + "…"
}

// handleSettingsPatch 修改设置。
//
// 成功返回与 GET 相同的脱敏视图，前端无需再拉一次。
func handleSettingsPatch(deps Deps, w http.ResponseWriter, r *http.Request) {
	if deps.Settings == nil {
		writeError(w, http.StatusServiceUnavailable, openai.ErrTypeServer,
			"no_settings", "设置存储未初始化")
		return
	}

	var patch settingsPatch
	if err := decodeJSONBody(r, &patch); err != nil {
		writeError(w, http.StatusBadRequest, openai.ErrTypeInvalidRequest,
			"invalid_json", "请求体不是合法 JSON: "+err.Error())
		return
	}

	// ── 乐观锁前置检查（Codex 第 12 轮要求，顺序在一切写入之前）──
	//
	// 客户端可以给两种形式，任选其一：
	//   - body 里的 ifRevision
	//   - 标准的 If-Match 头（值为 GET 返回的 ETag，如 "rev-3"）
	//
	// 🔴 缺失 → 428 Precondition Required（不是放行）。
	// Codex 明确否决了"缺失即强制写入"：那样前端漏传一个字段
	// 就重新变成无保护覆盖，revision 形同虚设。
	if !hasPrecondition(r, patch) {
		w.Header().Set("ETag", revisionETag(deps.Settings.Get().Revision))
		writeError(w, http.StatusPreconditionRequired, openai.ErrTypeInvalidRequest,
			"precondition_required",
			"缺少 ifRevision：请先 GET /api/settings 取 revision，再带着它提交"+
				"（本接口用乐观锁防止并发覆盖）")
		return
	}
	if reason, badRequest, ok := checkPrecondition(deps, r, patch); !ok {
		// 参数本身有问题（形态非法、两者矛盾）→ 400：调用方改请求即可
		// 版本不符 → 412：并发冲突，调用方应重新加载
		//
		// 用返回的布尔区分，而不是去匹配错误文案 ——
		// 文案会改，靠字符串判断迟早出错（我上一版就是这么写错的）。
		if badRequest {
			writeError(w, http.StatusBadRequest, openai.ErrTypeInvalidRequest,
				"bad_precondition", reason)
			return
		}
		writeSettingsConflict(deps, w, reason)
		return
	}

	// 语义冲突：既给新值又要清空 —— 明确报错好过猜用户意图
	if patch.APIKey != nil && patch.APIKeyClear != nil && *patch.APIKeyClear {
		writeError(w, http.StatusBadRequest, openai.ErrTypeInvalidRequest,
			"conflicting_key_change", "不能同时设置 apiKey 与 apiKeyClear")
		return
	}

	// 端口校验放在写盘之前，失败时不影响其他字段
	if patch.Port != nil {
		if err := config.ValidatePort(*patch.Port); err != nil {
			writeError(w, http.StatusBadRequest, openai.ErrTypeInvalidRequest,
				"invalid_port", err.Error())
			return
		}
	}

	// 别名要做语义校验（目标存在、禁自映射、禁循环）。
	// 结构校验在 config 层，语义校验需要 router 的模型知识，故在此处做。
	if patch.Aliases != nil {
		if err := deps.validateAliases(*patch.Aliases); err != nil {
			writeError(w, http.StatusBadRequest, openai.ErrTypeInvalidRequest,
				"invalid_aliases", err.Error())
			return
		}
	}

	// 开机自启：先动注册表，成功后再落盘。
	// 反过来会在注册表失败时留下"配置说已启用、实际没启用"的不一致。
	restoreAutoStart := false
	if patch.AutoStart != nil {
		if err := setAutoStart(*patch.AutoStart); err != nil {
			writeError(w, http.StatusInternalServerError, openai.ErrTypeServer,
				"autostart_failed", "设置开机自启失败: "+err.Error())
			return
		}
		restoreAutoStart = true
	}

	err := deps.Settings.Mutate(func(s *config.Settings) error {
		if patch.APIKey != nil {
			s.APIKey = *patch.APIKey
		}
		if patch.APIKeyClear != nil && *patch.APIKeyClear {
			s.APIKey = ""
		}
		if patch.Port != nil {
			s.Port = *patch.Port
		}
		if patch.AutoCheckin != nil {
			s.AutoCheckin = *patch.AutoCheckin
		}
		if patch.AutoStart != nil {
			s.AutoStart = *patch.AutoStart
		}
		if patch.Aliases != nil {
			s.Aliases = *patch.Aliases
		}
		return nil
	})
	if err != nil {
		// 落盘失败 → 把注册表回滚，避免"配置说没启用、注册表却启用了"
		if restoreAutoStart {
			_ = setAutoStart(deps.Settings.Get().AutoStart)
		}
		writeError(w, http.StatusInternalServerError, openai.ErrTypeServer,
			"save_failed", "保存设置失败: "+err.Error())
		return
	}

	// 别名热生效：写盘成功后立刻替换 router 的别名表
	if patch.Aliases != nil && deps.Router != nil {
		if err := deps.Router.SetAliases(*patch.Aliases); err != nil {
			writeError(w, http.StatusInternalServerError, openai.ErrTypeServer,
				"alias_apply_failed", "设置已保存但别名未生效: "+err.Error())
			return
		}
	}

	// 自动签到开关立即生效（不是等下次启动）。
	//
	// ⚠️ 必须判 nil（2026-10-05 由 TestCSRFAllowsSameOriginWrite 抓到的真实 panic）：
	// Deps.applyAutoCheckin 是可选回调，测试或未装配签到守护的场景下为 nil。
	// 之前直接调用 → 改开关时 handler 直接 panic，客户端收到 EOF
	// （而不是一个可读的错误），排查时非常难定位。
	if patch.AutoCheckin != nil && deps.applyAutoCheckin != nil {
		deps.applyAutoCheckin(*patch.AutoCheckin)
	}

	writeJSON(w, http.StatusOK, deps.settingsView())
}

// validateAliases 做别名的语义校验。
func (d Deps) validateAliases(m map[string]string) error {
	if err := config.ValidateAliases(m); err != nil {
		return err
	}
	if d.Router == nil {
		return nil
	}
	return d.Router.SetAliases(m) // router 会做目标存在性 + 循环校验；失败不生效
}

// ─────────────────────────────────────────────────────────────
// /v1/* 鉴权
// ─────────────────────────────────────────────────────────────

// requireAPIKey 包装 /v1/* 的处理器，做本地密钥校验。
//
// 规则（契约 §2）：
//   - apiKey 非空 → 必须带 Authorization: Bearer <key>，不匹配返回 401
//   - apiKey 为空 → 放行（委托人要求"key 可以为空"）
//
// 只用【常量时间比较】：普通的 == 会因提前返回而泄露"前几位猜对了"的时序信息。
// 对只监听回环的本地服务来说这属于防御性编程，成本几乎为零。
func requireAPIKey(deps Deps, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var want string
		if deps.Settings != nil {
			want = deps.Settings.Get().APIKey
		}
		if want == "" {
			next(w, r)
			return
		}

		got := bearerToken(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="wbapi"`)
			writeError(w, http.StatusUnauthorized, openai.ErrTypeInvalidRequest,
				"invalid_api_key", "API key 不正确或缺失")
			return
		}
		next(w, r)
	}
}

// bearerToken 从 Authorization 头取出 Bearer token。
//
// 兼容大小写不敏感的 "bearer"（部分客户端这么发）。
func bearerToken(h string) string {
	const prefix = "bearer "
	if len(h) < len(prefix) {
		return ""
	}
	if !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}
