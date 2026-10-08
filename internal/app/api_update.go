package app

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"workbuddy.local/workbuddy-api/internal/protocol/openai"
	"workbuddy.local/workbuddy-api/internal/update"
)

// ═══════════════════════════════════════════════════════════════════
// 自动更新（面板「检查更新」）
// ═══════════════════════════════════════════════════════════════════
//
// 🔴 安全定位（改这块前必读）
//
//	这是本项目**最危险**的功能：它把"运行远程字节"变成一次点击。
//	设计上守住的边界（每条都有测试）：
//
//	  · 只走 https + 主机白名单（update.ValidateAssetURL）
//	  · 必须校验 SHA256（用 GitHub Release 的 digest 字段）
//	  · 没有 digest 就**拒绝更新**（不宽松跳过）
//	  · 全部由**用户显式点击**触发，绝无后台静默替换
//	  · 检查（GET）与执行（POST）分离：检查无副作用，执行要过 CSRF
//
//	⚠️ 诚实边界：digest 与资产同源（都来自 GitHub API）。
//	  它防的是"传输被篡改/分片损坏"，**防不住"GitHub 账号被控制"**。
//	  后者只能靠账号安全（2FA、令牌最小权限）。

// updaterClient 是"查询最新 Release / 下载资产"的最小接口。
//
// 🔴 为什么抽接口（而不是直接用 *update.Updater）：
//
//	与 Deps.Chatter / accountChatter 同一个模式 —— 让测试注入替身，
//	从而**不联网**地验证整条链路（含状态机与失败分支）。
//	真实实现 update.Updater 天然满足本接口。
type updaterClient interface {
	LatestRelease(ctx context.Context) (*update.Release, error)
	DownloadAsset(ctx context.Context, rel *update.Release, destDir string) (*update.DownloadResult, error)
}

// updateState 保存一次更新检查/下载的进程内状态。
//
// 为什么要状态机（而不是每次请求现查）：
//
//	面板需要显示"上次检查时间/发现的版本/是否正在下载/上次结果"。
//	而且下载是**长操作**（十几 MB），必须有"进行中"的互斥，
//	否则用户连点会发起多个并发下载（浪费带宽、可能互相覆盖临时文件）。
type updateState struct {
	mu sync.Mutex

	// checking 表示正在检查（防并发检查）
	checking bool
	// downloading 表示正在下载/替换
	downloading bool

	// lastCheck 上次检查完成时间
	lastCheck time.Time
	// latest 上次检查到的最新版本（已规范化为 0.1.5 形式）
	latest string
	// latestTag 原始 tag（v0.1.5），用于展示
	latestTag string
	// hasUpdate 是否有可用更新
	hasUpdate bool
	// checkErr 上次检查的错误（供面板显示）
	checkErr string

	// lastResult 上次执行更新的结果描述
	lastResult string
	// lastResultOK 上次执行是否成功
	lastResultOK bool

	// lastRel 上次检查到的 Release（apply 时用它，避免"直接 apply 任意 URL"）
	lastRel *update.Release
}

// newUpdateState 建状态机。
func newUpdateState() *updateState {
	return &updateState{}
}

// updateStatusResponse 是 GET /api/update 的响应。
type updateStatusResponse struct {
	// Current 当前运行的版本（app.Version）。
	Current string `json:"current"`
	// Dev 当前是否为开发构建（未注入版本号）⇒ 不参与更新检查。
	Dev bool `json:"dev"`

	// ReleaseConfigured 仓库是否已配置更新源。
	// ⚠️ 为 false 时面板应提示"暂无更新源"，而不是"已是最新"。
	ReleaseConfigured bool `json:"releaseConfigured"`

	Latest    string `json:"latest,omitempty"`
	LatestTag string `json:"latestTag,omitempty"`
	HasUpdate bool   `json:"hasUpdate"`

	Checking    bool `json:"checking"`
	Downloading bool `json:"downloading"`

	LastCheck  string `json:"lastCheck,omitempty"`
	CheckError string `json:"checkError,omitempty"`

	LastResult   string `json:"lastResult,omitempty"`
	LastResultOK bool   `json:"lastResultOK"`
}

// snapshot 读一份状态快照（锁内取，避免并发读写）。
func (s *updateState) snapshot(current string) updateStatusResponse {
	s.mu.Lock()
	defer s.mu.Unlock()

	resp := updateStatusResponse{
		Current:      current,
		Dev:          update.ParseVersion(current).Dev,
		Latest:       s.latest,
		LatestTag:    s.latestTag,
		HasUpdate:    s.hasUpdate,
		Checking:     s.checking,
		Downloading:  s.downloading,
		CheckError:   s.checkErr,
		LastResult:   s.lastResult,
		LastResultOK: s.lastResultOK,
	}
	if !s.lastCheck.IsZero() {
		resp.LastCheck = s.lastCheck.Format(time.RFC3339)
	}
	return resp
}

// handleUpdateStatus 处理 GET /api/update（只读，无需 CSRF）。
func handleUpdateStatus(deps Deps, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeError(w, http.StatusMethodNotAllowed, openai.ErrTypeInvalidRequest,
			"method_not_allowed", "只支持 GET")
		return
	}
	if deps.Update == nil {
		writeError(w, http.StatusServiceUnavailable, openai.ErrTypeServer,
			"update_unavailable", "当前运行方式不支持更新检查")
		return
	}
	resp := deps.Update.snapshot(Version)
	// 当前是开发构建 ⇒ 明确告诉面板，别显示"已是最新"骗人
	if resp.Dev {
		resp.ReleaseConfigured = false
	} else {
		resp.ReleaseConfigured = true
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleUpdateCheck 处理 POST /api/update/check —— 去 GitHub 查最新 Release。
//
// 有副作用（发起外网请求、改状态）⇒ 走 guardManagementAPI（防 CSRF）。
func handleUpdateCheck(deps Deps, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, http.StatusMethodNotAllowed, openai.ErrTypeInvalidRequest,
			"method_not_allowed", "只支持 POST")
		return
	}
	if deps.Update == nil {
		writeError(w, http.StatusServiceUnavailable, openai.ErrTypeServer,
			"update_unavailable", "当前运行方式不支持更新检查")
		return
	}

	st := deps.Update
	st.mu.Lock()
	if st.checking {
		st.mu.Unlock()
		writeError(w, http.StatusConflict, openai.ErrTypeInvalidRequest,
			"check_in_progress", "正在检查更新，请稍候")
		return
	}
	st.checking = true
	st.mu.Unlock()

	// 无论如何都要清掉 checking，否则一次失败会把面板永久卡在"检查中"。
	defer func() {
		st.mu.Lock()
		st.checking = false
		st.lastCheck = time.Now()
		st.mu.Unlock()
	}()

	up := deps.Updater
	if up == nil {
		up = update.NewUpdater()
	}

	// 检查是网络操作，给它一个上限（GitHub 正常在 1 秒内返回）
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	rel, err := up.LatestRelease(ctx)
	if err != nil {
		st.mu.Lock()
		st.checkErr = err.Error()
		st.hasUpdate = false
		st.mu.Unlock()
		writeError(w, http.StatusBadGateway, openai.ErrTypeServer,
			"update_check_failed", "检查更新失败: "+err.Error())
		return
	}

	st.mu.Lock()
	st.checkErr = ""
	if rel == nil {
		// 仓库还没有任何 Release —— 不是错误（委托人当前就是这个状态）
		st.latest = ""
		st.latestTag = ""
		st.hasUpdate = false
	} else {
		st.latest = rel.Version
		st.latestTag = rel.Tag
		st.hasUpdate = update.NeedsUpdate(Version, rel.Version)
	}
	deps.Update.lastRel = rel
	st.mu.Unlock()

	writeJSON(w, http.StatusOK, st.snapshot(Version))
}

// handleUpdateApply 处理 POST /api/update/apply —— 下载 + 校验 + 替换 + 重启。
//
// 这是最危险的一步：它会**替换正在运行的 exe**。因此：
//   - 必须走 guardManagementAPI（防 CSRF：恶意网页不能触发）
//   - 必须已经检查过（有 latestRel），不允许"直接 apply 一个任意 URL"
//   - 校验失败就中止，绝不留半成品
func handleUpdateApply(deps Deps, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, http.StatusMethodNotAllowed, openai.ErrTypeInvalidRequest,
			"method_not_allowed", "只支持 POST")
		return
	}
	if deps.Update == nil {
		writeError(w, http.StatusServiceUnavailable, openai.ErrTypeServer,
			"update_unavailable", "当前运行方式不支持自动更新")
		return
	}

	st := deps.Update
	st.mu.Lock()
	if st.downloading {
		st.mu.Unlock()
		writeError(w, http.StatusConflict, openai.ErrTypeInvalidRequest,
			"update_in_progress", "更新已在进行中")
		return
	}
	rel := st.lastRel
	if rel == nil || !st.hasUpdate {
		st.mu.Unlock()
		writeError(w, http.StatusConflict, openai.ErrTypeInvalidRequest,
			"no_update", "没有可用的更新，请先「检查更新」")
		return
	}
	if rel.AssetURL == "" {
		st.mu.Unlock()
		writeError(w, http.StatusConflict, openai.ErrTypeInvalidRequest,
			"no_asset", "该版本没有可下载的程序文件")
		return
	}
	st.downloading = true
	st.lastResult = ""
	st.mu.Unlock()

	fail := func(status int, code, msg string) {
		st.mu.Lock()
		st.downloading = false
		st.lastResult = msg
		st.lastResultOK = false
		st.mu.Unlock()
		writeError(w, status, openai.ErrTypeServer, code, msg)
	}

	up := deps.Updater
	if up == nil {
		up = update.NewUpdater()
	}

	// 下载到 exe 同目录（同一分区才能保证 Rename 是原子改名而非跨盘拷贝）
	destDir := datadirExeDir()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	res, err := up.DownloadAsset(ctx, rel, destDir)
	if err != nil {
		fail(http.StatusBadGateway, "update_download_failed",
			"下载或校验失败: "+err.Error())
		return
	}

	// ── 替换 exe ──
	oldPath, err := update.ReplaceSelf(res.Path)
	if err != nil {
		fail(http.StatusInternalServerError, "update_replace_failed",
			"替换程序失败: "+err.Error())
		return
	}

	msg := "已更新到 " + rel.Version + "，服务即将重启以生效"
	st.mu.Lock()
	st.lastResult = msg
	st.lastResultOK = true
	st.downloading = false
	st.hasUpdate = false
	st.mu.Unlock()

	if deps.Logger != nil {
		deps.Logger.Printf("自动更新：已替换程序（旧版保留在 %s），准备重启生效", oldPath)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"version": rel.Version,
		"message": msg,
	})

	// 🔴 重启复用既有的重启机制（与面板改端口走同一条路）。
	//
	//	为什么不在本请求里直接 exec：必须先把 200 响应写出去，
	//	否则面板收不到"更新成功"就要面对连接中断。
	//	restartState 的 signal 正是"稍后重启"的既有通道。
	if deps.restart != nil {
		go func() {
			time.Sleep(500 * time.Millisecond)
			deps.restart.signal()
		}()
	}
}

// datadirExeDir 返回当前 exe 所在目录（更新下载的落点）。
//
// 🔴 必须与 exe 同目录：update.ReplaceSelf 用 os.Rename 做改名交换，
// 而 Rename 只在**同一分区**内是原子的改名；跨分区会退化成"拷贝+删除"，
// 既慢又可能在半途失败留下不一致状态。
func datadirExeDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}
