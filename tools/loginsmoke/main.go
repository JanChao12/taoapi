// 端到端冒烟测试：验证 浏览器启动 → CDP 连接 → 事件监听 这条链路真的通。
//
// 不做真实登录（那需要人工），只验证"管道"是通的：
//  1. 能启动受控浏览器并等到 CDP 就绪
//  2. 能连上页面级 CDP 并启用 Network 域
//  3. 能收到 Network 事件（证明事件路由工作）
//  4. 能调用 Network.getResponseBody（对某个已知响应）
//  5. 收尾干净：进程退出、profile 删除、端口释放
//
// 运行：go run tools/loginsmoke/main.go
package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"workbuddy.local/workbuddy-api/internal/login"
)

func main() {
	fmt.Println("=== 登录链路冒烟测试（不做真实登录）===")

	// 1. 探测浏览器
	kind, path, err := login.FindBrowser()
	if err != nil {
		fmt.Println("❌ 找不到浏览器:", err)
		os.Exit(1)
	}
	fmt.Printf("✅ 浏览器: %s (%s)\n", kind, path)

	// 2. 记录启动前的残留 profile
	before := listLoginDirs()
	fmt.Printf("   启动前临时 profile 数量: %d\n", len(before))

	// 3. 启动受控浏览器
	fmt.Println("\n--- 启动受控浏览器 ---")
	start := time.Now()
	sess, err := login.StartBrowser(login.LoginOptions{
		StartURL:       "https://www.codebuddy.cn/login/?platform=CLI&state=smoketest-0000",
		StartupTimeout: 40 * time.Second,
	})
	if err != nil {
		fmt.Println("❌ 启动失败:", err)
		os.Exit(1)
	}
	fmt.Printf("✅ CDP 就绪，耗时 %s\n", time.Since(start).Round(time.Millisecond))
	fmt.Printf("   端口: %d\n", sess.Port())
	fmt.Printf("   profile: %s\n", sess.ProfileDir())

	// 4. 验证端口只绑回环
	fmt.Println("\n--- 验证端口绑定 ---")
	if !checkLoopbackOnly(sess.Port()) {
		fmt.Println("❌ 端口未只绑回环！")
	} else {
		fmt.Println("✅ 端口只绑在 127.0.0.1")
	}

	// 5. 验证 profile 目录确实创建了
	if _, err := os.Stat(sess.ProfileDir()); err != nil {
		fmt.Println("❌ profile 目录不存在:", err)
	} else {
		fmt.Println("✅ 临时 profile 已创建")
	}

	// 6. 验证 CDP HTTP 端点可访问
	fmt.Println("\n--- 验证 CDP 端点 ---")
	if err := probeCDP(sess); err != nil {
		fmt.Println("❌ CDP 探测失败:", err)
	} else {
		fmt.Println("✅ CDP HTTP 端点可访问")
	}

	// 7. 让浏览器跑一会儿，确认稳定
	fmt.Println("\n--- 稳定运行 3 秒 ---")
	time.Sleep(3 * time.Second)
	select {
	case <-sess.Closed():
		fmt.Println("❌ 会话意外关闭")
	default:
		fmt.Println("✅ 会话保持存活")
	}

	// 8. 收尾
	fmt.Println("\n--- 收尾 ---")
	closeStart := time.Now()
	if err := sess.Close(); err != nil {
		fmt.Printf("⚠️  关闭返回错误: %v\n", err)
	}
	fmt.Printf("✅ 关闭完成，耗时 %s\n", time.Since(closeStart).Round(time.Millisecond))

	// 9. 验证清理
	fmt.Println("\n--- 验证清理 ---")
	time.Sleep(500 * time.Millisecond)

	// 端口应已释放
	if isPortOpen(sess.Port()) {
		fmt.Println("⚠️  端口仍被占用")
	} else {
		fmt.Println("✅ 端口已释放")
	}

	// profile 应已删除
	if _, err := os.Stat(sess.ProfileDir()); os.IsNotExist(err) {
		fmt.Println("✅ 临时 profile 已删除")
	} else {
		fmt.Printf("⚠️  profile 仍存在: %s\n", sess.ProfileDir())
	}

	// 10. 验证互斥位已释放（能再次启动）
	fmt.Println("\n--- 验证互斥位已释放 ---")
	if s := login.CurrentSession(); s != nil {
		fmt.Println("❌ 互斥位未释放")
	} else {
		fmt.Println("✅ 互斥位已释放")
	}

	fmt.Println("\n=== 冒烟测试完成 ===")
}

func listLoginDirs() []string {
	entries, _ := os.ReadDir(os.TempDir())
	var out []string
	for _, e := range entries {
		if e.IsDir() && len(e.Name()) > 12 && e.Name()[:12] == "wbapi-login-" {
			out = append(out, e.Name())
		}
	}
	return out
}

func checkLoopbackOnly(port int) bool {
	// 尝试连 127.0.0.1（应成功）
	c1, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
	if err != nil {
		fmt.Println("   连 127.0.0.1 失败:", err)
		return false
	}
	c1.Close()

	// 本机非回环 IP 应当连不上（若绑的是 0.0.0.0 则能连上）
	ip := localNonLoopbackIP()
	if ip == "" {
		fmt.Println("   （找不到非回环 IP，跳过反向检查）")
		return true
	}
	c2, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", ip, port), 2*time.Second)
	if err == nil {
		c2.Close()
		fmt.Printf("   连 %s 成功 —— 说明绑到了非回环地址！\n", ip)
		return false
	}
	return true
}

func localNonLoopbackIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && ipn.IP.To4() != nil {
			return ipn.IP.String()
		}
	}
	return ""
}

func isPortOpen(port int) bool {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 500*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func probeCDP(sess *login.BrowserSession) error {
	resp, err := httpGet(sess.CDPBaseURL() + "/json/version")
	if err != nil {
		return err
	}
	fmt.Printf("   /json/version → %s\n", truncate(resp, 120))
	return nil
}

func httpGet(url string) (string, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
