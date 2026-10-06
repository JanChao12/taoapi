// ws_test.go：WebSocket 帧层护栏测试。
//
// 这些测试的目标不是"覆盖率"，而是**把 Codex 第 28 轮划定的协议底线钉住**：
// 分片重组、控制帧规则、掩码规则、RSV、长度编码、大小上限。
//
// ⚠️ 每个测试都要能在**实现被改坏时变红**。若某条底线没有对应测试，
// 就等于没有底线（全局 AGENTS.md：「评审结论要落成代码里的护栏，否则等于没评」）。
package login

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// ─────────────────────────────────────────────────────────────
// 测试辅助：一个最小的"假服务端"，用于构造任意帧序列
// ─────────────────────────────────────────────────────────────

// fakeServer 起一个本地 TCP 监听，完成 WebSocket 握手，
// 然后按脚本发送原始帧字节。返回连接地址与完成的信号。
type fakeServer struct {
	ln   net.Listener
	addr string
}

func newFakeServer(t *testing.T, script func(c net.Conn)) *fakeServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	fs := &fakeServer{ln: ln, addr: ln.Addr().String()}
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		// 完成握手：读请求头，回 101。
		br := bufio.NewReader(conn)
		key := ""
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				break
			}
			if strings.HasPrefix(strings.ToLower(line), "sec-websocket-key:") {
				key = strings.TrimSpace(line[len("sec-websocket-key:"):])
			}
		}
		if key == "" {
			return
		}
		resp := "HTTP/1.1 101 Switching Protocols\r\n" +
			"Upgrade: websocket\r\n" +
			"Connection: Upgrade\r\n" +
			"Sec-WebSocket-Accept: " + computeAcceptKey(key) + "\r\n\r\n"
		if _, err := conn.Write([]byte(resp)); err != nil {
			return
		}
		script(conn)
	}()
	t.Cleanup(func() {
		ln.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	})
	return fs
}

// wsURL 返回可用的 ws:// 地址。
func (f *fakeServer) wsURL() string { return "ws://" + f.addr + "/devtools/page/test" }

// ─────────────────────────────────────────────────────────────
// 测试辅助：构造原始帧（服务端->客户端，**不掩码**）
// ─────────────────────────────────────────────────────────────

// buildFrame 构造一个服务端帧。
// 手动控制 fin / opcode / 长度编码，以便测试各分支。
func buildFrame(t *testing.T, fin bool, opcode byte, payload []byte, forceLen int) []byte {
	t.Helper()
	var b bytes.Buffer
	b0 := opcode
	if fin {
		b0 |= 0x80
	}
	b.WriteByte(b0)

	n := len(payload)
	switch {
	case forceLen == 126 || (forceLen == 0 && n > 125 && n <= 0xFFFF):
		b.WriteByte(126)
		var ext [2]byte
		binary.BigEndian.PutUint16(ext[:], uint16(n))
		b.Write(ext[:])
	case forceLen == 127 || (forceLen == 0 && n > 0xFFFF):
		b.WriteByte(127)
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(n))
		b.Write(ext[:])
	default:
		b.WriteByte(byte(n))
	}
	b.Write(payload)
	return b.Bytes()
}

// dialFake 连到假服务端并返回 wsConn。
func dialFake(t *testing.T, fs *fakeServer) *wsConn {
	t.Helper()
	c, err := wsDial(fs.wsURL(), 3*time.Second)
	if err != nil {
		t.Fatalf("握手失败: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// ─────────────────────────────────────────────────────────────
// 握手
// ─────────────────────────────────────────────────────────────

// TestComputeAcceptKeyMatchesRFCVector 用 RFC 6455 §1.3 的官方样例钉住算法。
func TestComputeAcceptKeyMatchesRFCVector(t *testing.T) {
	// RFC 6455 §1.3：key "dGhlIHNhbXBsZSBub25jZQ==" 应得
	// "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="
	got := computeAcceptKey("dGhlIHNhbXBsZSBub25jZQ==")
	want := "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="
	if got != want {
		t.Fatalf("Sec-WebSocket-Accept 不符 RFC 向量\n got=%q\nwant=%q", got, want)
	}
}

// TestWsDialRejectsNonLoopback 守住"CDP 只连本机"这条边界。
func TestWsDialRejectsNonLoopback(t *testing.T) {
	for _, host := range []string{"ws://example.com:9222/x", "ws://10.0.0.1:9222/x"} {
		if _, err := wsDial(host, time.Second); err == nil {
			t.Fatalf("%s 应被拒绝（非回环）", host)
		}
	}
}

// TestWsDialRejectsNonWSScheme 只允许 ws://（调试端口是明文回环）。
func TestWsDialRejectsNonWSScheme(t *testing.T) {
	if _, err := wsDial("wss://127.0.0.1:9222/x", time.Second); err == nil {
		t.Fatal("wss:// 应被拒绝")
	}
}

// ─────────────────────────────────────────────────────────────
// 帧解析
// ─────────────────────────────────────────────────────────────

// TestReadSingleTextFrame 基本路径。
func TestReadSingleTextFrame(t *testing.T) {
	fs := newFakeServer(t, func(c net.Conn) {
		c.Write(buildFrame(t, true, opText, []byte(`{"id":1}`), 0))
		time.Sleep(200 * time.Millisecond)
	})
	c := dialFake(t, fs)
	msg, err := c.ReadMessage(time.Now().Add(2 * time.Second))
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if string(msg) != `{"id":1}` {
		t.Fatalf("内容不符: %q", msg)
	}
}

// TestReadFragmentedMessage 守住分片重组 —— Codex 明确要求：
// 「一条响应体仍可能被底层拆成多个帧」。
func TestReadFragmentedMessage(t *testing.T) {
	fs := newFakeServer(t, func(c net.Conn) {
		// "hello " + "frag" + "mented"
		c.Write(buildFrame(t, false, opText, []byte("hello "), 0))
		c.Write(buildFrame(t, false, opContinuation, []byte("frag"), 0))
		c.Write(buildFrame(t, true, opContinuation, []byte("mented"), 0))
		time.Sleep(200 * time.Millisecond)
	})
	c := dialFake(t, fs)
	msg, err := c.ReadMessage(time.Now().Add(2 * time.Second))
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if string(msg) != "hello fragmented" {
		t.Fatalf("分片重组不符: %q", msg)
	}
}

// TestReadMessageRejectsContinuationWithoutStart 守住分片顺序校验。
func TestReadMessageRejectsContinuationWithoutStart(t *testing.T) {
	fs := newFakeServer(t, func(c net.Conn) {
		c.Write(buildFrame(t, true, opContinuation, []byte("orphan"), 0))
		time.Sleep(200 * time.Millisecond)
	})
	c := dialFake(t, fs)
	_, err := c.ReadMessage(time.Now().Add(2 * time.Second))
	if !errors.Is(err, ErrWebSocketProtocol) {
		t.Fatalf("孤立延续帧应报协议错误，得到: %v", err)
	}
}

// TestReadMessageRejectsNewDataFrameDuringFragmentation 守住分片中间插入新帧。
func TestReadMessageRejectsNewDataFrameDuringFragmentation(t *testing.T) {
	fs := newFakeServer(t, func(c net.Conn) {
		c.Write(buildFrame(t, false, opText, []byte("a"), 0))
		c.Write(buildFrame(t, true, opText, []byte("b"), 0)) // 非法：分片未完成
		time.Sleep(200 * time.Millisecond)
	})
	c := dialFake(t, fs)
	_, err := c.ReadMessage(time.Now().Add(2 * time.Second))
	if !errors.Is(err, ErrWebSocketProtocol) {
		t.Fatalf("分片未完成时的新数据帧应报协议错误，得到: %v", err)
	}
}

// TestReadMessageRejectsMaskedServerFrame 守住「服务端帧不得掩码」。
// 这条如果失守，说明掩码校验被删了 —— 会让解析结果错乱。
func TestReadMessageRejectsMaskedServerFrame(t *testing.T) {
	fs := newFakeServer(t, func(c net.Conn) {
		payload := []byte("x")
		// 手工构造带掩码位的帧。
		frame := []byte{0x81, 0x80 | byte(len(payload)), 1, 2, 3, 4}
		frame = append(frame, payload[0]^1)
		c.Write(frame)
		time.Sleep(200 * time.Millisecond)
	})
	c := dialFake(t, fs)
	_, err := c.ReadMessage(time.Now().Add(2 * time.Second))
	if !errors.Is(err, ErrWebSocketProtocol) {
		t.Fatalf("带掩码的服务端帧应报协议错误，得到: %v", err)
	}
}

// TestReadMessageRejectsNonZeroRSV 守住「不协商扩展 ⇒ RSV 必须为 0」。
func TestReadMessageRejectsNonZeroRSV(t *testing.T) {
	fs := newFakeServer(t, func(c net.Conn) {
		// RSV1 置位（0x40），且声明 permessage-deflate 式压缩。
		frame := []byte{0x80 | 0x40 | opText, 0x01, 'a'}
		c.Write(frame)
		time.Sleep(200 * time.Millisecond)
	})
	c := dialFake(t, fs)
	_, err := c.ReadMessage(time.Now().Add(2 * time.Second))
	if !errors.Is(err, ErrWebSocketProtocol) {
		t.Fatalf("RSV 非零应报协议错误，得到: %v", err)
	}
}

// TestReadMessageRejectsBinaryFrame 守住「收到二进制必须明确报错并关闭」。
func TestReadMessageRejectsBinaryFrame(t *testing.T) {
	fs := newFakeServer(t, func(c net.Conn) {
		c.Write(buildFrame(t, true, opBinary, []byte{1, 2, 3}, 0))
		time.Sleep(200 * time.Millisecond)
	})
	c := dialFake(t, fs)
	_, err := c.ReadMessage(time.Now().Add(2 * time.Second))
	if !errors.Is(err, ErrWebSocketProtocol) {
		t.Fatalf("二进制帧应报协议错误，得到: %v", err)
	}
}

// TestReadMessageRejectsFragmentedControlFrame 守住「控制帧必须 FIN」。
func TestReadMessageRejectsFragmentedControlFrame(t *testing.T) {
	fs := newFakeServer(t, func(c net.Conn) {
		c.Write(buildFrame(t, false, opPing, []byte("p"), 0)) // FIN=0
		time.Sleep(200 * time.Millisecond)
	})
	c := dialFake(t, fs)
	_, err := c.ReadMessage(time.Now().Add(2 * time.Second))
	if !errors.Is(err, ErrWebSocketProtocol) {
		t.Fatalf("非 FIN 控制帧应报协议错误，得到: %v", err)
	}
}

// TestReadMessageRejectsOversizedControlFrame 守住「控制帧 ≤125 字节」。
func TestReadMessageRejectsOversizedControlFrame(t *testing.T) {
	fs := newFakeServer(t, func(c net.Conn) {
		big := make([]byte, 126)
		// 用 126 扩展长度编码，明确超过 125。
		c.Write(buildFrame(t, true, opPing, big, 126))
		time.Sleep(200 * time.Millisecond)
	})
	c := dialFake(t, fs)
	_, err := c.ReadMessage(time.Now().Add(2 * time.Second))
	if !errors.Is(err, ErrWebSocketProtocol) {
		t.Fatalf("超长控制帧应报协议错误，得到: %v", err)
	}
}

// TestReadMessageRejectsOversizedFrame 守住单帧大小上限（防 OOM）。
func TestReadMessageRejectsOversizedFrame(t *testing.T) {
	fs := newFakeServer(t, func(c net.Conn) {
		// 只发帧头，声明一个超过上限的长度，不发负载。
		var hdr [10]byte
		hdr[0] = 0x80 | opText
		hdr[1] = 127
		binary.BigEndian.PutUint64(hdr[2:], uint64(wsMaxFramePayload)+1)
		c.Write(hdr[:])
		time.Sleep(300 * time.Millisecond)
	})
	c := dialFake(t, fs)
	_, err := c.ReadMessage(time.Now().Add(2 * time.Second))
	if !errors.Is(err, ErrWebSocketProtocol) {
		t.Fatalf("超大帧应报协议错误，得到: %v", err)
	}
}

// TestReadMessageHandlesPingAndPong 守住 Ping 自动回 Pong、Pong 被忽略。
func TestReadMessageHandlesPingAndPong(t *testing.T) {
	pingSeen := make(chan []byte, 1)
	fs := newFakeServer(t, func(c net.Conn) {
		// 先发 ping 与 pong，再发实际消息。
		c.Write(buildFrame(t, true, opPing, []byte("hi"), 0))
		c.Write(buildFrame(t, true, opPong, []byte("yo"), 0))
		c.Write(buildFrame(t, true, opText, []byte("payload"), 0))

		// 读客户端回的 Pong（掩码帧）。
		br := bufio.NewReader(c)
		hdr := make([]byte, 2)
		if _, err := br.Read(hdr); err == nil {
			masked := hdr[1]&0x80 != 0
			ln := int(hdr[1] & 0x7F)
			if masked {
				mask := make([]byte, 4)
				br.Read(mask)
				body := make([]byte, ln)
				br.Read(body)
				for i := range body {
					body[i] ^= mask[i%4]
				}
				pingSeen <- body
			} else {
				body := make([]byte, ln)
				br.Read(body)
				pingSeen <- body
			}
		}
		time.Sleep(300 * time.Millisecond)
	})
	c := dialFake(t, fs)
	msg, err := c.ReadMessage(time.Now().Add(3 * time.Second))
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if string(msg) != "payload" {
		t.Fatalf("应跳过控制帧拿到 payload，得到 %q", msg)
	}
	select {
	case body := <-pingSeen:
		if string(body) != "hi" {
			t.Fatalf("Pong 应回显 Ping 负载，得到 %q", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("未收到 Pong 回应")
	}
}

// TestReadMessageOnCloseFrameReturnsClosed 守住 Close 帧处理。
func TestReadMessageOnCloseFrameReturnsClosed(t *testing.T) {
	fs := newFakeServer(t, func(c net.Conn) {
		c.Write(buildFrame(t, true, opClose, nil, 0))
		time.Sleep(200 * time.Millisecond)
	})
	c := dialFake(t, fs)
	_, err := c.ReadMessage(time.Now().Add(2 * time.Second))
	if !errors.Is(err, ErrWebSocketClosed) {
		t.Fatalf("Close 帧应返回 ErrWebSocketClosed，得到: %v", err)
	}
}

// TestReadMessageOnEOFReturnsClosed 守住对端直接断开。
func TestReadMessageOnEOFReturnsClosed(t *testing.T) {
	fs := newFakeServer(t, func(c net.Conn) {
		// 立刻关闭。
	})
	c := dialFake(t, fs)
	_, err := c.ReadMessage(time.Now().Add(2 * time.Second))
	if !errors.Is(err, ErrWebSocketClosed) {
		t.Fatalf("EOF 应返回 ErrWebSocketClosed，得到: %v", err)
	}
}

// TestReadMessageExtendedLengthEncodings 守住 16 位与 64 位长度分支。
func TestReadMessageExtendedLengthEncodings(t *testing.T) {
	cases := []struct {
		name     string
		size     int
		forceLen int
	}{
		{"16位长度", 300, 126},
		{"64位长度", 70000, 127},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := bytes.Repeat([]byte("a"), tc.size)
			fs := newFakeServer(t, func(c net.Conn) {
				c.Write(buildFrame(t, true, opText, payload, tc.forceLen))
				time.Sleep(300 * time.Millisecond)
			})
			c := dialFake(t, fs)
			msg, err := c.ReadMessage(time.Now().Add(5 * time.Second))
			if err != nil {
				t.Fatalf("读取失败: %v", err)
			}
			if len(msg) != tc.size {
				t.Fatalf("长度不符: got=%d want=%d", len(msg), tc.size)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────
// 写出侧
// ─────────────────────────────────────────────────────────────

// TestWriteTextIsMasked 守住「客户端必须掩码」。
// 未掩码的客户端帧会被合规服务端直接断开。
func TestWriteTextIsMasked(t *testing.T) {
	got := make(chan []byte, 1)
	fs := newFakeServer(t, func(c net.Conn) {
		br := bufio.NewReader(c)
		hdr := make([]byte, 2)
		if _, err := br.Read(hdr); err != nil {
			return
		}
		masked := hdr[1]&0x80 != 0
		ln := int(hdr[1] & 0x7F)
		var body []byte
		if masked {
			mask := make([]byte, 4)
			br.Read(mask)
			body = make([]byte, ln)
			br.Read(body)
			for i := range body {
				body[i] ^= mask[i%4]
			}
		} else {
			body = make([]byte, ln)
			br.Read(body)
		}
		got <- append([]byte{boolByte(masked)}, body...)
		time.Sleep(200 * time.Millisecond)
	})
	c := dialFake(t, fs)
	if err := c.WriteText([]byte("cmd")); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	select {
	case v := <-got:
		if v[0] != 1 {
			t.Fatal("客户端帧必须带掩码位")
		}
		if string(v[1:]) != "cmd" {
			t.Fatalf("负载不符: %q", v[1:])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("未收到客户端帧")
	}
}

func boolByte(b bool) byte {
	if b {
		return 1
	}
	return 0
}

// TestWriteToClosedConnFails 守住「关闭后写入返回 ErrWebSocketClosed」。
func TestWriteToClosedConnFails(t *testing.T) {
	fs := newFakeServer(t, func(c net.Conn) {
		time.Sleep(500 * time.Millisecond)
	})
	c := dialFake(t, fs)
	c.Close()
	if err := c.WriteText([]byte("x")); !errors.Is(err, ErrWebSocketClosed) {
		t.Fatalf("关闭后写入应返回 ErrWebSocketClosed，得到: %v", err)
	}
}

// TestConcurrentWritesAreSerialized 守住「并发写必须串行化」。
//
// 若去掉 writeMu，多个 goroutine 的帧字节会交错，
// 服务端解析必然错乱 —— 这里用"帧数完整且各自可解析"来抓。
func TestConcurrentWritesAreSerialized(t *testing.T) {
	const writers = 8
	const perWriter = 20
	received := make(chan int, writers*perWriter)

	fs := newFakeServer(t, func(c net.Conn) {
		br := bufio.NewReader(c)
		for i := 0; i < writers*perWriter; i++ {
			// ⚠️ 必须用 io.ReadFull，不能用 br.Read。
			//
			// `Read` 只保证"返回至少 1 字节"，TCP 在负载下会把一个帧
			// 拆成多次投递 —— 于是 body 只读到一部分，测试报
			// "帧被截断: 收到 55 字节，期望 100"，**误判成并发写没串行化**。
			//
			// 这是测试自身的缺陷（偶发，且只在整包负载下出现），
			// 与生产代码无关。实测：单独跑 5/5 通过，整包跑偶发失败。
			hdr := make([]byte, 2)
			if _, err := io.ReadFull(br, hdr); err != nil {
				return
			}
			ln := int(hdr[1] & 0x7F)

			var mask []byte
			if hdr[1]&0x80 != 0 {
				mask = make([]byte, 4)
				if _, err := io.ReadFull(br, mask); err != nil {
					return
				}
			}

			body := make([]byte, ln)
			if ln > 0 {
				if _, err := io.ReadFull(br, body); err != nil {
					return
				}
			}
			if mask != nil {
				for j := range body {
					body[j] ^= mask[j%4]
				}
			}
			received <- len(body)
		}
	})

	c := dialFake(t, fs)
	payload := bytes.Repeat([]byte("z"), 100)
	done := make(chan struct{}, writers)
	for w := 0; w < writers; w++ {
		go func() {
			for i := 0; i < perWriter; i++ {
				c.WriteText(payload)
			}
			done <- struct{}{}
		}()
	}
	for w := 0; w < writers; w++ {
		<-done
	}

	// 服务端应能完整解出 writers*perWriter 个长度正确的帧。
	timeout := time.After(5 * time.Second)
	count := 0
	for count < writers*perWriter {
		select {
		case n := <-received:
			if n != len(payload) {
				t.Fatalf("帧被截断: 收到 %d 字节，期望 %d", n, len(payload))
			}
			count++
		case <-timeout:
			t.Fatalf("只解出 %d/%d 个完整帧 —— 并发写未串行化", count, writers*perWriter)
		}
	}
}

// ─────────────────────────────────────────────────────────────
// 错误信息不得泄露负载
// ─────────────────────────────────────────────────────────────

// TestProtocolErrorsDoNotLeakPayload 守住「错误信息不含帧内容」。
//
// 理由：帧负载可能含 token。任何把负载拼进 error 的写法都是泄露通道。
func TestProtocolErrorsDoNotLeakPayload(t *testing.T) {
	secret := "SUPERSECRETTOKENVALUE1234567890"
	fs := newFakeServer(t, func(c net.Conn) {
		// 用一个会触发协议错误的形态承载秘密内容。
		frame := []byte{0x80 | 0x40 | opText, byte(len(secret))}
		frame = append(frame, []byte(secret)...)
		c.Write(frame)
		time.Sleep(200 * time.Millisecond)
	})
	c := dialFake(t, fs)
	_, err := c.ReadMessage(time.Now().Add(2 * time.Second))
	if err == nil {
		t.Fatal("应报错")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("错误信息泄露了帧负载: %v", err)
	}
}

// TestAcceptKeyMismatchRejected 守住握手校验（防被非 WS 服务欺骗）。
func TestAcceptKeyMismatchRejected(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			if strings.TrimRight(line, "\r\n") == "" {
				break
			}
		}
		// 故意回一个错误的 Accept。
		conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\n" +
			"Upgrade: websocket\r\n" +
			"Connection: Upgrade\r\n" +
			"Sec-WebSocket-Accept: WRONGVALUE\r\n\r\n"))
		time.Sleep(300 * time.Millisecond)
	}()

	if _, err := wsDial("ws://"+ln.Addr().String()+"/x", 2*time.Second); err == nil {
		t.Fatal("Accept 校验失败时应拒绝连接")
	}
}

// TestHandshakeRejectsNon101 守住非 101 响应被拒。
func TestHandshakeRejectsNon101(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			if strings.TrimRight(line, "\r\n") == "" {
				break
			}
		}
		conn.Write([]byte("HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n"))
		time.Sleep(200 * time.Millisecond)
	}()

	if _, err := wsDial("ws://"+ln.Addr().String()+"/x", 2*time.Second); err == nil {
		t.Fatal("非 101 应被拒绝")
	}
}

// TestHandshakeRejectsUnexpectedExtension 守住「不协商压缩」。
func TestHandshakeRejectsUnexpectedExtension(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		key := ""
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				break
			}
			if strings.HasPrefix(strings.ToLower(line), "sec-websocket-key:") {
				key = strings.TrimSpace(line[len("sec-websocket-key:"):])
			}
		}
		conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\n" +
			"Upgrade: websocket\r\n" +
			"Connection: Upgrade\r\n" +
			"Sec-WebSocket-Accept: " + computeAcceptKey(key) + "\r\n" +
			"Sec-WebSocket-Extensions: permessage-deflate\r\n\r\n"))
		time.Sleep(300 * time.Millisecond)
	}()

	if _, err := wsDial("ws://"+ln.Addr().String()+"/x", 2*time.Second); err == nil {
		t.Fatal("服务端坚持协商扩展时应拒绝")
	}
}

// TestIsLoopbackHost 守住回环判定（含 IPv6）。
func TestIsLoopbackHost(t *testing.T) {
	yes := []string{"127.0.0.1", "localhost", "::1"}
	no := []string{"0.0.0.0", "10.0.0.1", "example.com", "192.168.1.1"}
	for _, h := range yes {
		if !isLoopbackHost(h) {
			t.Fatalf("%q 应判为回环", h)
		}
	}
	for _, h := range no {
		if isLoopbackHost(h) {
			t.Fatalf("%q 不应判为回环", h)
		}
	}
}
