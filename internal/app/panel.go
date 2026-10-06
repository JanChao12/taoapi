package app

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"

	"workbuddy.local/workbuddy-api/internal/protocol/openai"
)

// panelFS 把面板静态资源编进二进制。
//
// 单文件分发的关键：用户只需要一个 exe，不需要附带任何文件。
//
//go:embed panel/*
var panelFS embed.FS

// panelHandler 提供面板静态资源。
//
// 路径映射：
//
//	/panel/            → index.html
//	/panel/app.js      → panel/app.js
//	/panel/style.css   → panel/style.css
//
// ⚠️ 必须显式禁止强缓存（委托人 2026-10-05 反馈"新功能看不到"的根因）：
//
//	http.FileServer 对 embed.FS 不设任何 Cache-Control，浏览器于是按启发式规则
//	【强缓存】app.js / style.css。这两个文件名是固定的，服务端换了代码 URL 却没变，
//	浏览器就一直用旧副本 —— 表现就是"新加的功能（徽章、刷新按钮）看不到"，
//	而服务端下发的其实是新文件。排查时极易误判成"改错了/回滚了"。
//
//	no-cache 不是"不缓存"，而是"每次必须回源校验"：配合 FileServer 自动产生的
//	Last-Modified/ETag，未变动时返回 304，开销极小，但改了代码立刻生效。
func panelHandler() http.Handler {
	sub, err := fs.Sub(panelFS, "panel")
	if err != nil {
		// 编译期 embed 失败才会到这里；返回一个明确错误的 handler。
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeError(w, http.StatusInternalServerError, openai.ErrTypeServer,
				"panel_unavailable", "面板资源不可用")
		})
	}
	fileServer := http.StripPrefix("/panel/", http.FileServer(http.FS(sub)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache, must-revalidate")
		fileServer.ServeHTTP(w, r)
	})
}

// servePanelIndex 在访问 / 时跳到面板。
func servePanelIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		writeError(w, http.StatusNotFound, openai.ErrTypeInvalidRequest,
			"not_found", "not found: "+r.URL.Path)
		return
	}
	// 相对跳转，保持用户访问的主机与端口
	http.Redirect(w, r, "/panel/", http.StatusFound)
}

// isPanelPath 判断是否面板路径（便于统一日志/鉴权）。
func isPanelPath(p string) bool {
	return strings.HasPrefix(p, "/panel")
}
