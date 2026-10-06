// cdp.go：CDP 的 HTTP 发现端点 + WebSocket 会话封装。
//
// ═══════════════════════════════════════════════════════════════════
// CDP 的两半
// ═══════════════════════════════════════════════════════════════════
//
// CDP（Chrome DevTools Protocol）有两类接口，实测行为不同：
//
//	A. HTTP 端点（GET /json/version、GET /json/list、PUT /json/new）
//	   —— 可用，但**只给元信息**。实测 /json/cookies、/json/storage 等
//	      全部 404：**HTTP 端点不提供任何存储或响应体数据**。
//
//	B. WebSocket（/devtools/page/<id>、/devtools/browser/<id>）
//	   —— 所有真正的能力都在这边（Network.*、Runtime.*、DOMStorage.*）。
//
// 所以本文件只把 A 用于"发现"，真正的操作全走 B。
//
// ═══════════════════════════════════════════════════════════════════
// 凭据提取的关键设计（Codex 第 28 轮裁定）
// ═══════════════════════════════════════════════════════════════════
//
//	「不能用"URL 包含路径"作为匹配条件。用 net/url 解析后精确校验。」
//
// 必须同时满足：
//   - scheme == https
//   - host   == www.codebuddy.cn
//   - path   完全等于 /console/login/enterprise
//   - state 只有一个值，且等于我们本次生成的那个
//   - 请求来自本程序创建并跟踪的登录 target
//   - 方法为 GET，响应成功
//
// 时序（Codex 明确要求）：
//
//	Network.responseReceived 只用于**登记候选 requestId**；
//	**必须等 Network.loadingFinished** 之后再调 Network.getResponseBody。
//	并处理 loadingFailed、CDP 错误、base64Encoded。
//
// 语义边界（Codex）：
//
//	「凭据字段解析成功 = "捕获完成"，不等于 "账号验证完成"。」
package login

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// cdpHTTPClient 访问 CDP 的 HTTP 发现端点。
type cdpHTTPClient struct {
	base string
	hc   *http.Client
}

// newCDPHTTPClient 构造一个指向 base 的 CDP HTTP 客户端。
//
// ⚠️ base 必须由调用方传入实际端口。早期版本把 base 留空并指望调用方
// 再调 setBase —— 实测那样会让 version() 永远失败，表现为"浏览器永远不就绪"。
// 现在改为构造时必填，把这个坑从接口上堵死。
func newCDPHTTPClient(base string, timeout time.Duration) *cdpHTTPClient {
	return &cdpHTTPClient{
		base: base,
		hc:   &http.Client{Timeout: timeout},
	}
}

// cdpVersion 是 /json/version 的响应子集。
type cdpVersion struct {
	Browser              string `json:"Browser"`
	ProtocolVersion      string `json:"Protocol-Version"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

// cdpTarget 是 /json/list 里的一个 target。
type cdpTarget struct {
	ID                   string `json:"id"`
	Type                 string `json:"type"`
	Title                string `json:"title"`
	URL                  string `json:"url"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

// version 读取 /json/version。
func (c *cdpHTTPClient) version() (cdpVersion, error) {
	var v cdpVersion
	if c.base == "" {
		return v, errors.New("cdp: 未设置 base URL")
	}
	body, err := c.get("/json/version")
	if err != nil {
		return v, err
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return v, fmt.Errorf("cdp: 解析 /json/version 失败: %w", err)
	}
	return v, nil
}

// listTargets 读取 /json/list。
func (c *cdpHTTPClient) listTargets() ([]cdpTarget, error) {
	if c.base == "" {
		return nil, errors.New("cdp: 未设置 base URL")
	}
	body, err := c.get("/json/list")
	if err != nil {
		return nil, err
	}
	var out []cdpTarget
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("cdp: 解析 /json/list 失败: %w", err)
	}
	return out, nil
}

// get 发起 GET 并返回响应体（限长，防被恶意/异常对端撑爆内存）。
func (c *cdpHTTPClient) get(path string) ([]byte, error) {
	resp, err := c.hc.Get(c.base + path)
	if err != nil {
		return nil, fmt.Errorf("cdp: 请求 %s 失败: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cdp: %s 返回 HTTP %d", path, resp.StatusCode)
	}
	// 限长读取：CDP 的发现端点响应都很小。
	const maxDiscoveryBytes = 4 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDiscoveryBytes+1))
	if err != nil {
		return nil, fmt.Errorf("cdp: 读取 %s 失败: %w", path, err)
	}
	if len(body) > maxDiscoveryBytes {
		return nil, fmt.Errorf("cdp: %s 响应超过 %d 字节上限", path, maxDiscoveryBytes)
	}
	return body, nil
}

// ─────────────────────────────────────────────────────────────
// CDP 会话（WebSocket）
// ─────────────────────────────────────────────────────────────

// cdpSession 是一条到某个 target 的 CDP 会话。
//
// 读写分离：
//   - 一个 goroutine 持续读，按 id 分派响应、按 method 分派事件
//   - 调用方通过 Call 发命令并按 id 等响应
type cdpSession struct {
	ws *wsConn

	mu       sync.Mutex
	nextID   int
	pending  map[int]chan cdpMessage
	evMu     sync.Mutex
	closed   chan struct{}
	closeErr error

	// onEvent 可选的事件回调（用于登记候选 requestId 等）。
	// 只经回调传递，不做缓冲 —— 见 readLoop 里的说明。
	onEvent func(cdpEvent)
}

// cdpMessage 是 CDP 的 JSON 消息（请求、响应、事件共用）。
type cdpMessage struct {
	ID     int             `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *cdpError       `json:"error,omitempty"`
}

// cdpError 是 CDP 错误对象。注意：**不回显任何可能含敏感数据的字段**。
type cdpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *cdpError) Error() string {
	return fmt.Sprintf("CDP 错误 %d: %s", e.Code, sanitizeCDPText(e.Message))
}

// cdpEvent 是一条 CDP 事件。
type cdpEvent struct {
	Method string
	Params json.RawMessage
}

// sanitizeCDPText 截断并过滤 CDP 返回的文本，避免把大段原始数据写进错误。
func sanitizeCDPText(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// newCDPSession 在给定 ws URL 上建立会话并启动读循环。
func newCDPSession(wsURL string, timeout time.Duration) (*cdpSession, error) {
	ws, err := wsDial(wsURL, timeout)
	if err != nil {
		return nil, err
	}
	s := &cdpSession{
		ws:      ws,
		pending: make(map[int]chan cdpMessage),
		closed:  make(chan struct{}),
	}
	go s.readLoop()
	return s, nil
}

// readLoop 持续读消息并分派。
func (s *cdpSession) readLoop() {
	defer close(s.closed)
	for {
		raw, err := s.ws.ReadMessage(time.Time{})
		if err != nil {
			s.mu.Lock()
			s.closeErr = err
			// 唤醒所有等待者，避免它们永久阻塞。
			for id, ch := range s.pending {
				close(ch)
				delete(s.pending, id)
			}
			s.mu.Unlock()
			return
		}

		var msg cdpMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			// 无法解析的消息不该让整个会话崩掉，但如果它看起来像响应，
			// 等待者会超时并报错。这里只跳过。
			continue
		}

		if msg.ID != 0 {
			s.mu.Lock()
			ch, ok := s.pending[msg.ID]
			if ok {
				delete(s.pending, msg.ID)
			}
			s.mu.Unlock()
			if ok {
				ch <- msg
				close(ch)
			}
			continue
		}

		if msg.Method != "" {
			// 事件不经队列缓冲，直接回调。
			//
			// ⚠️ 早先的实现把事件 append 进一个切片"供稍后读取"，但没有任何
			// 调用方读它，也没有上限 —— 一个完整的登录过程会持续产生
			// Network.* 事件，等于给进程挂了个只增不减的内存泄漏。
			// 本项目内存红线 60 MB，不能留这种"看起来无害"的堆积。
			// 现在只有回调一条路径；没有回调就直接丢弃（我们只关心
			// 少数几类事件，且由编排层按需登记）。
			s.evMu.Lock()
			fn := s.onEvent
			s.evMu.Unlock()
			if fn != nil {
				fn(cdpEvent{Method: msg.Method, Params: msg.Params})
			}
		}
	}
}

// SetEventHandler 设置事件回调。必须在 Call 之前设置。
func (s *cdpSession) SetEventHandler(fn func(cdpEvent)) {
	s.evMu.Lock()
	s.onEvent = fn
	s.evMu.Unlock()
}

// Call 发送一条 CDP 命令并等待其响应。
func (s *cdpSession) Call(method string, params any, timeout time.Duration) (json.RawMessage, error) {
	s.mu.Lock()
	s.nextID++
	id := s.nextID
	ch := make(chan cdpMessage, 1)
	s.pending[id] = ch
	s.mu.Unlock()

	var rawParams json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			s.mu.Lock()
			delete(s.pending, id)
			s.mu.Unlock()
			return nil, fmt.Errorf("cdp: 序列化参数失败: %w", err)
		}
		rawParams = b
	}

	req := cdpMessage{ID: id, Method: method, Params: rawParams}
	payload, err := json.Marshal(req)
	if err != nil {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		return nil, fmt.Errorf("cdp: 序列化请求失败: %w", err)
	}
	if err := s.ws.WriteText(payload); err != nil {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		return nil, err
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case msg, ok := <-ch:
		if !ok {
			s.mu.Lock()
			e := s.closeErr
			s.mu.Unlock()
			if e != nil {
				return nil, fmt.Errorf("cdp: 会话已关闭: %w", e)
			}
			return nil, ErrWebSocketClosed
		}
		if msg.Error != nil {
			return nil, msg.Error
		}
		return msg.Result, nil
	case <-timer.C:
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		return nil, fmt.Errorf("cdp: 命令 %s 超时（%s）", method, timeout)
	}
}

// Close 关闭会话。
func (s *cdpSession) Close() error { return s.ws.Close() }

// ─────────────────────────────────────────────────────────────
// 凭据目标判定（Codex 要求的精确匹配）
// ─────────────────────────────────────────────────────────────

// 凭据端点的匹配规则见 credmatch.go（含测试接缝与拒绝原因）。
// 这里只保留响应体的解析。

// credentialPayload 是凭据端点的响应体（只取我们需要的字段）。
//
// ═══════════════════════════════════════════════════════════════════
// 🔴 真实结构（2026-10-05 真实登录实测，**与早期假设不同**）
// ═══════════════════════════════════════════════════════════════════
//
// 早期实现（以及交接文档的记录）假设凭据在**顶层**：
//
//	{"accessToken":"…","refreshToken":"…"}          ← 错的
//
// 实测的真实结构是**嵌套在 `data` 里**，且带业务码信封：
//
//	{"code":0,"msg":"OK","requestId":"…","data":{
//	    "accessToken":"…","refreshToken":"…",
//	    "expiresIn":…,"refreshExpiresIn":…,"tokenType":"…"}}
//
// ⇒ 早期探针只记了"响应体里有 accessToken/refreshToken 这两个字段名"，
//
//	**没有记嵌套层级**，于是把结构搞错了。所有测试也都按错的结构造数据，
//	所以**从来没测出这个问题** —— 直到真实登录。
//
// ⚠️ 教训：契约必须记录**完整层级**，只记字段名会漏掉这一类错误。
type credentialPayload struct {
	// ── 业务信封 ──
	Code int    `json:"code"`
	Msg  string `json:"msg"`

	// ── 凭据（嵌套在 data 里）──
	Data struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`

		// 以下是**非秘密**的元信息，用于报告有效期。
		ExpiresIn        int64  `json:"expiresIn"`
		RefreshExpiresIn int64  `json:"refreshExpiresIn"`
		TokenType        string `json:"tokenType"`
	} `json:"data"`
}

// accessToken / refreshToken 是取值的便利方法。
func (p credentialPayload) accessToken() string { return strings.TrimSpace(p.Data.AccessToken) }
func (p credentialPayload) refreshToken() string {
	return strings.TrimSpace(p.Data.RefreshToken)
}

// describeCredentialShape 返回响应体的**结构摘要**（只有字段名与类型，无值）。
//
// ═══════════════════════════════════════════════════════════════════
// 为什么需要它（2026-10-05 真实登录实测）
// ═══════════════════════════════════════════════════════════════════
//
// 真实登录第一次被成功捕获到，但解析失败：
//
//	❌ 凭据响应缺少 accessToken 或 refreshToken
//
// 此前所有测试用的都是**我们假设的**结构
// （`{"accessToken":…,"refreshToken":…}`），从没验证过真实响应长什么样。
// 这正是 Codex 第 29 轮要求的"检查必需字段及业务成功码"那一步。
//
// ⇒ 把真实结构打出来，才能知道是：
//   - 字段名不同（如 data.accessToken 嵌套）
//   - 字段名大小写不同
//   - 或响应是**另一种形态**（例如错误信封 / 分两步下发）
//
// 🔴 只输出**字段名与类型/长度**，绝不输出值（可能是凭据）。
func describeCredentialShape(body []byte) string {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		// 不是 JSON 对象 —— 只报长度与首字符，不报内容。
		if len(body) == 0 {
			return "空响应体"
		}
		return fmt.Sprintf("非 JSON（%d 字节，首字符 %q）", len(body), body[0])
	}

	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"("+shapeOfValue(m[k])+")")
	}
	out := strings.Join(parts, ", ")
	if len(out) > 400 {
		out = out[:400] + "…"
	}
	return out
}

// shapeOfValue 描述一个 JSON 值的形态（类型 + 长度），**不含内容**。
func shapeOfValue(v any) string {
	switch t := v.(type) {
	case string:
		// 只给长度 —— 字符串可能是 token。
		return fmt.Sprintf("string,%d", len(t))
	case float64:
		return "number"
	case bool:
		return "bool"
	case nil:
		return "null"
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return "object{" + strings.Join(keys, ",") + "}"
	case []any:
		return fmt.Sprintf("array,%d", len(t))
	default:
		return "?"
	}
}

// extractCredential 从响应体解析凭据。
//
// 校验（Codex 第 29 轮要求）：
//  1. **业务码必须为 0** —— 「匹配到 200 不等于业务成功」
//  2. 两个 token 都必须非空
//
// 返回的错误**不含 token 内容**（可带结构摘要）。
func extractCredential(body []byte) (credentialPayload, error) {
	var p credentialPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return p, fmt.Errorf("login: 凭据响应解析失败: %w", err)
	}

	// 🔴 先看业务码：HTTP 200 但业务失败的情形必须拒绝，
	// 否则会把"错误响应"当成凭据（Codex：不把 HTTP 200 当业务成功）。
	if p.Code != 0 {
		// 只带业务码与脱敏消息，不带响应体。
		return p, fmt.Errorf("login: 凭据端点返回业务错误（code=%d, msg=%q）",
			p.Code, sanitizeCDPText(p.Msg))
	}

	if p.accessToken() == "" || p.refreshToken() == "" {
		// 🔴 带上**结构摘要** —— 否则只报"缺少字段"，无法知道真实结构。
		// 2026-10-05 真实登录正是在这里失败，而当时无从判断原因
		// （实际原因：字段嵌套在 data 里，而代码读的是顶层）。
		return p, fmt.Errorf("login: 凭据响应缺少 accessToken 或 refreshToken（实际结构: %s）",
			describeCredentialShape(body))
	}
	return p, nil
}
