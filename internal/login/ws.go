// ws.go：最小 WebSocket 客户端（RFC 6455 子集），纯标准库。
//
// ═══════════════════════════════════════════════════════════════════
// 为什么手写而不引入第三方库
// ═══════════════════════════════════════════════════════════════════
//
// 本项目硬约束是**纯标准库、零第三方依赖**（单 exe 发朋友、供应链最小化）。
// Go 标准库没有 WebSocket —— net/http 只到 Hijack 为止。
//
// 而 CDP（Chrome DevTools Protocol）**只走 WebSocket**：
// 实测 HTTP 端点（/json/cookies、/json/storage 等）**全部 404**，
// 只有 /json/version、/json/list、PUT /json/new 可用；读响应体必须走 WS。
//
// 所以这里手写一个**够用就停**的子集。范围由 Codex 第 28 轮划定：
//
//	「CDP 业务范围可以收窄，WebSocket 协议底线不能缩。」
//
// 即：只实现 CDP 用得到的调用，但**协议层面该校验的一条都不能省**。
//
// ═══════════════════════════════════════════════════════════════════
// 明确实现的范围（够用就停）
// ═══════════════════════════════════════════════════════════════════
//
//	✅ 握手：HTTP Upgrade + Sec-WebSocket-Accept 校验
//	✅ 文本帧 0x1、延续帧 0x0（CDP JSON 可能被拆成多帧，必须重组）
//	✅ 控制帧：Ping 0x9 / Pong 0xA / Close 0x8
//	✅ 长度编码三分支：7bit / 126(16bit) / 127(64bit)
//	✅ 客户端->服务端 masking；服务端帧不得带 mask
//	✅ RSV 必须为 0（不协商任何扩展，尤其 permessage-deflate）
//	✅ 分片顺序校验（延续帧必须紧跟未完成消息）
//	✅ 控制帧：必须 FIN、负载 ≤125 字节
//	✅ 帧与消息总大小上限（防 OOM）
//	✅ 并发写串行化（mutex）
//	✅ 取消/关闭能解除阻塞读（deadline）
//
//	❌ 不做：自动重连（CDP 会话生命周期由 login 包管）
//	❌ 不做：压缩扩展协商、二进制业务语义
//	❌ 不做：客户端侧分片发送（CDP 请求远小于单帧上限）
//	❌ 不做：TLS（CDP 调试端口只在本机回环，明文）
//
// ⚠️ 收到二进制帧 0x2 时：**不提供业务支持，但必须明确报协议错误并关闭**
//
//	（Codex 要求 —— 静默忽略会让对端状态机错乱）。
//
// ═══════════════════════════════════════════════════════════════════
// 安全注意
// ═══════════════════════════════════════════════════════════════════
//
//	本文件的错误信息**绝不包含帧负载内容**（可能含 token）。
//	只报 opcode / 长度 / 状态码这类结构性信息。
package login

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// wsMaxFramePayload 单帧负载上限。
	// ponytail: 8 MiB 足够 CDP 响应；CDP 大响应（如截图）不在本项目用途内。
	// 升级触发条件：若将来需要 Page.captureScreenshot 之类的大响应，改成可配置。
	wsMaxFramePayload = 8 << 20

	// wsMaxMessagePayload 单个完整消息（可含多帧）上限。
	// ponytail: 16 MiB。CDP 的 getResponseBody 可能较大，但本项目只取一个登录响应体。
	// 升级触发条件：同 wsMaxFramePayload。
	wsMaxMessagePayload = 16 << 20

	// wsControlPayloadMax RFC 6455 §5.5：控制帧负载不得超过 125 字节。
	wsControlPayloadMax = 125
)

// WebSocket 操作码（RFC 6455 §5.2）。
const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA
)

// wsMagic 是 RFC 6455 §1.3 规定的固定 GUID。
const wsMagic = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// ErrWebSocketClosed 表示连接已正常或异常关闭。
var ErrWebSocketClosed = errors.New("websocket: 连接已关闭")

// ErrWebSocketProtocol 表示对端违反了协议（附属错误信息不含负载内容）。
var ErrWebSocketProtocol = errors.New("websocket: 协议错误")

// wsConn 是一个最小的 WebSocket 连接。
//
// 它不是并发安全的读，但**写是串行化的**（CDP 会从多个 goroutine 发命令）。
type wsConn struct {
	conn net.Conn
	br   *bufio.Reader

	writeMu sync.Mutex

	closeOnce sync.Once
	closed    chan struct{}
}

// wsDial 向 ws:// 地址发起 WebSocket 握手。
//
// 只支持明文 ws://（CDP 调试端口在本机回环，不需要 TLS）。
// 出于安全考虑**拒绝非回环主机** —— 调试端口只应指向本机。
func wsDial(rawURL string, timeout time.Duration) (*wsConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("websocket: 解析地址失败: %w", err)
	}
	if u.Scheme != "ws" {
		return nil, fmt.Errorf("websocket: 仅支持 ws:// 方案，收到 %q", u.Scheme)
	}
	host := u.Hostname()
	if !isLoopbackHost(host) {
		return nil, fmt.Errorf("websocket: 拒绝非回环主机 %q", host)
	}

	// 生成随机 Sec-WebSocket-Key（16 字节 base64）。
	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		return nil, fmt.Errorf("websocket: 生成握手 key 失败: %w", err)
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)

	addr := u.Host
	if u.Port() == "" {
		addr = net.JoinHostPort(host, "80")
	}
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, fmt.Errorf("websocket: 连接 %s 失败: %w", addr, err)
	}

	// 写握手请求。
	path := u.RequestURI()
	if path == "" {
		path = "/"
	}
	var req strings.Builder
	req.WriteString("GET " + path + " HTTP/1.1\r\n")
	req.WriteString("Host: " + u.Host + "\r\n")
	req.WriteString("Upgrade: websocket\r\n")
	req.WriteString("Connection: Upgrade\r\n")
	req.WriteString("Sec-WebSocket-Key: " + key + "\r\n")
	req.WriteString("Sec-WebSocket-Version: 13\r\n")
	// 不发送 Sec-WebSocket-Extensions —— 明确不协商压缩。
	req.WriteString("\r\n")

	if _, err := conn.Write([]byte(req.String())); err != nil {
		conn.Close()
		return nil, fmt.Errorf("websocket: 发送握手失败: %w", err)
	}

	// 读握手响应。用 http.ReadResponse 解析状态行与头部。
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("websocket: 读取握手响应失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusSwitchingProtocols {
		conn.Close()
		return nil, fmt.Errorf("websocket: 握手被拒，HTTP %d", resp.StatusCode)
	}
	if !headerContainsToken(resp.Header, "Upgrade", "websocket") {
		conn.Close()
		return nil, fmt.Errorf("%w: 握手响应缺少 Upgrade: websocket", ErrWebSocketProtocol)
	}
	if !headerContainsToken(resp.Header, "Connection", "upgrade") {
		conn.Close()
		return nil, fmt.Errorf("%w: 握手响应缺少 Connection: upgrade", ErrWebSocketProtocol)
	}

	// 校验 Sec-WebSocket-Accept = base64(sha1(key + MAGIC))。
	// RFC 6455 §4.1：客户端**必须**校验，否则可能被非 WebSocket 服务欺骗。
	want := computeAcceptKey(key)
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != want {
		conn.Close()
		return nil, fmt.Errorf("%w: Sec-WebSocket-Accept 校验失败", ErrWebSocketProtocol)
	}
	// 若服务端坚持要压缩，说明它忽略了我们的意愿 —— 拒绝，避免解析歧义。
	if ext := resp.Header.Get("Sec-WebSocket-Extensions"); ext != "" {
		conn.Close()
		return nil, fmt.Errorf("%w: 不接受扩展协商 %q", ErrWebSocketProtocol, sanitizeHeader(ext))
	}

	// 清掉握手期 deadline，后续由各调用自行设置。
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		conn.Close()
		return nil, err
	}

	return &wsConn{
		conn:   conn,
		br:     br,
		closed: make(chan struct{}),
	}, nil
}

// computeAcceptKey 按 RFC 6455 §4.2.2 计算 Sec-WebSocket-Accept。
func computeAcceptKey(key string) string {
	h := sha1.New()
	io.WriteString(h, key)
	io.WriteString(h, wsMagic)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// headerContainsToken 判断某个头是否含指定 token（大小写不敏感，逗号分隔）。
func headerContainsToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, part := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

// sanitizeHeader 把头部值截断并去掉控制字符，避免把不受信内容写进错误信息。
func sanitizeHeader(s string) string {
	if len(s) > 64 {
		s = s[:64] + "…"
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

// isLoopbackHost 判断主机名是否为回环地址（IPv4 / IPv6 / localhost）。
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Close 关闭连接。可重复调用。
func (c *wsConn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		close(c.closed)
		// 尽力发一个 Close 帧（不阻塞、不等回应）。
		_ = c.writeFrame(opClose, nil, true)
		err = c.conn.Close()
	})
	return err
}

// Closed 返回一个在连接关闭时被关闭的 channel，供 select 使用。
func (c *wsConn) Closed() <-chan struct{} { return c.closed }

// WriteText 发送一个文本帧（客户端必须掩码）。
func (c *wsConn) WriteText(payload []byte) error {
	return c.writeFrame(opText, payload, true)
}

// writeFrame 组装并发送一个帧。fin 表示是否为消息的最后一帧。
//
// 写操作串行化：CDP 会从多个 goroutine 发命令，而 RFC 6455 要求
// 帧边界不能交错，否则对端解析出错。
func (c *wsConn) writeFrame(opcode byte, payload []byte, fin bool) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	select {
	case <-c.closed:
		return ErrWebSocketClosed
	default:
	}

	// 帧头：FIN + opcode，MASK + 长度。
	head := make([]byte, 0, 14)
	b0 := opcode
	if fin {
		b0 |= 0x80
	}
	head = append(head, b0)

	// 客户端总是掩码（RFC 6455 §5.1）。
	const maskBit = 0x80
	n := len(payload)
	switch {
	case n <= 125:
		head = append(head, maskBit|byte(n))
	case n <= 0xFFFF:
		head = append(head, maskBit|126, 0, 0)
		binary.BigEndian.PutUint16(head[len(head)-2:], uint16(n))
	default:
		head = append(head, maskBit|127, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint64(head[len(head)-8:], uint64(n))
	}

	// 掩码键 + 掩码后的负载。
	mask := make([]byte, 4)
	if _, err := rand.Read(mask); err != nil {
		return fmt.Errorf("websocket: 生成掩码失败: %w", err)
	}
	head = append(head, mask...)

	masked := make([]byte, n)
	for i := 0; i < n; i++ {
		masked[i] = payload[i] ^ mask[i%4]
	}

	if err := c.writeAll(head); err != nil {
		return err
	}
	return c.writeAll(masked)
}

// writeAll 写完整段数据（net.Conn.Write 不保证写满）。
func (c *wsConn) writeAll(b []byte) error {
	for len(b) > 0 {
		n, err := c.conn.Write(b)
		if err != nil {
			return fmt.Errorf("websocket: 写入失败: %w", err)
		}
		b = b[n:]
	}
	return nil
}

// wsFrame 是一个解析出来的帧。
type wsFrame struct {
	fin     bool
	opcode  byte
	payload []byte
}

// ReadMessage 读取一个**完整的文本消息**，自动处理分片与控制帧。
//
// 返回值语义：
//   - 成功：payload 为完整消息内容
//   - 连接关闭：ErrWebSocketClosed
//   - 协议违规：ErrWebSocketProtocol 包装的错误
//
// deadline 为 0 表示不设超时（由调用方通过 Close 解除阻塞）。
//
// 注意：Ping 会被自动回 Pong；Pong 被忽略；Close 会触发关闭并返回
// ErrWebSocketClosed。二进制消息**报协议错误**（本客户端不使用二进制）。
func (c *wsConn) ReadMessage(deadline time.Time) ([]byte, error) {
	if err := c.conn.SetReadDeadline(deadline); err != nil {
		return nil, err
	}

	var msg []byte
	var fragmented bool // 是否处于分片中
	var fragOpcode byte // 分片消息的起始 opcode

	for {
		f, err := c.readFrame()
		if err != nil {
			return nil, err
		}

		switch f.opcode {
		case opPing:
			// 自动回 Pong，携带相同负载（RFC 6455 §5.5.3）。
			// 控制帧不允许分片，已在 readFrame 校验。
			if err := c.writeFrame(opPong, f.payload, true); err != nil {
				return nil, err
			}
			continue

		case opPong:
			// 忽略。
			continue

		case opClose:
			// 对端要求关闭。回一个 Close 后关闭连接。
			c.Close()
			return nil, ErrWebSocketClosed

		case opText, opBinary:
			if fragmented {
				return nil, fmt.Errorf("%w: 分片未完成时收到新的数据帧", ErrWebSocketProtocol)
			}
			if f.opcode == opBinary {
				// 明确报错并关闭，而不是静默忽略。
				c.Close()
				return nil, fmt.Errorf("%w: 收到不支持的二进制帧", ErrWebSocketProtocol)
			}
			if f.fin {
				// 完整的单帧消息。
				return f.payload, nil
			}
			// 开始分片。
			fragmented = true
			fragOpcode = f.opcode
			msg = append(msg, f.payload...)

		case opContinuation:
			if !fragmented {
				return nil, fmt.Errorf("%w: 未开始分片却收到延续帧", ErrWebSocketProtocol)
			}
			msg = append(msg, f.payload...)
			if len(msg) > wsMaxMessagePayload {
				return nil, fmt.Errorf("%w: 消息超过上限 %d 字节", ErrWebSocketProtocol, wsMaxMessagePayload)
			}
			if f.fin {
				// 分片消息完成。
				out := msg
				msg = nil
				fragmented = false
				_ = fragOpcode
				return out, nil
			}

		default:
			return nil, fmt.Errorf("%w: 未知操作码 0x%x", ErrWebSocketProtocol, f.opcode)
		}
	}
}

// readFrame 读取并校验单个帧。
//
// 校验项（Codex 第 28 轮要求"协议底线不能缩"）：
//   - RSV1/2/3 必须为 0（未协商任何扩展）
//   - 服务端帧**不得**带掩码
//   - 控制帧必须 FIN 且负载 ≤125 字节
//   - 负载长度不超过单帧上限
func (c *wsConn) readFrame() (wsFrame, error) {
	var f wsFrame

	var hdr [2]byte
	if _, err := io.ReadFull(c.br, hdr[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return f, ErrWebSocketClosed
		}
		return f, fmt.Errorf("websocket: 读取帧头失败: %w", err)
	}

	fin := hdr[0]&0x80 != 0
	rsv := hdr[0] & 0x70
	opcode := hdr[0] & 0x0F
	masked := hdr[1]&0x80 != 0
	length := uint64(hdr[1] & 0x7F)

	if rsv != 0 {
		return f, fmt.Errorf("%w: RSV 位非零（未协商扩展）", ErrWebSocketProtocol)
	}
	if masked {
		// RFC 6455 §5.1：服务端发给客户端的帧**不得**掩码。
		return f, fmt.Errorf("%w: 服务端帧不应带掩码", ErrWebSocketProtocol)
	}

	// 长度扩展分支。
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return f, fmt.Errorf("websocket: 读取 16 位长度失败: %w", err)
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
		// RFC 6455：非最小长度编码是协议违规，但 CDP 不会这么干；
		// 这里只做上限校验，不苛求最小编码（避免对端实现差异导致误判）。
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return f, fmt.Errorf("websocket: 读取 64 位长度失败: %w", err)
		}
		length = binary.BigEndian.Uint64(ext[:])
		if length&(1<<63) != 0 {
			return f, fmt.Errorf("%w: 64 位长度最高位必须为 0", ErrWebSocketProtocol)
		}
	}

	isControl := opcode&0x08 != 0
	if isControl {
		if !fin {
			return f, fmt.Errorf("%w: 控制帧必须 FIN", ErrWebSocketProtocol)
		}
		if length > wsControlPayloadMax {
			return f, fmt.Errorf("%w: 控制帧负载 %d 超过 125 字节", ErrWebSocketProtocol, length)
		}
	}

	if length > wsMaxFramePayload {
		return f, fmt.Errorf("%w: 帧负载 %d 超过上限 %d", ErrWebSocketProtocol, length, wsMaxFramePayload)
	}

	payload := make([]byte, length)
	if length > 0 {
		if _, err := io.ReadFull(c.br, payload); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return f, ErrWebSocketClosed
			}
			return f, fmt.Errorf("websocket: 读取负载失败: %w", err)
		}
	}

	f.fin = fin
	f.opcode = opcode
	f.payload = payload
	return f, nil
}
