// 验证 /v2/plugin/login/account 端点是否可用于"查询当前 token 是谁"。
//
// 背景：网页登录拿到的凭据只有 accessToken/refreshToken，**没有 uid**。
// 而本项目的凭据结构需要 uid（它是账号主键，且用于派生 X-User-Id 等头）。
// 第 27 轮调研时实测过：该端点不带 state 时返回当前 token 的身份。
//
// 本工具用真实账号验证这一点，输出**只有字段名与非敏感值**，不回显 token。
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"workbuddy.local/workbuddy-api/internal/auth"
	"workbuddy.local/workbuddy-api/internal/storage"
)

func main() {
	fmt.Println("=== 验证 /v2/plugin/login/account 身份查询 ===")

	// 用既有的 Persister 读取（复用生产路径的 codec，不自己猜格式）。
	p := auth.NewPersister(auth.AccountsPath(), storage.DPAPICodec{Description: "wbapi 账号凭据"})
	store, err := p.Load()
	if err != nil {
		fmt.Println("读账号失败:", err)
		os.Exit(1)
	}
	list := store.List()
	if len(list) == 0 {
		fmt.Println("没有账号可测")
		os.Exit(1)
	}
	a := list[0]
	fmt.Printf("用账号: uid=%s nickname=%s\n", auth.MaskUID(a.UID), a.Nickname)

	// 试几个候选端点
	candidates := []struct {
		method string
		url    string
	}{
		{"GET", "https://www.codebuddy.cn/v2/plugin/login/account"},
		{"GET", "https://www.codebuddy.cn/v2/plugin/login/account?state="},
	}

	client := &http.Client{Timeout: 15 * time.Second}
	for _, c := range candidates {
		fmt.Printf("\n--- %s %s ---\n", c.method, c.url)
		req, err := http.NewRequest(c.method, c.url, nil)
		if err != nil {
			fmt.Println("构造请求失败:", err)
			continue
		}
		req.Header.Set("Authorization", "Bearer "+a.AccessToken)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "CLI/2.63.2 CodeBuddy/2.63.2")

		resp, err := client.Do(req)
		if err != nil {
			fmt.Println("请求失败:", err)
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		fmt.Printf("HTTP %d\n", resp.StatusCode)

		// 只打印结构，值的长度而非内容（除 uid/nickname 这类非秘密字段）
		var m map[string]any
		if json.Unmarshal(body, &m) == nil {
			printShape(m, "  ")
		} else {
			fmt.Printf("  非 JSON，长度 %d\n", len(body))
		}
	}
}

// printShape 打印 JSON 结构。对疑似秘密的长字符串只报长度。
func printShape(v any, indent string) {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			switch tv := val.(type) {
			case string:
				if isSensitiveKey(k) {
					fmt.Printf("%s%s: string(len=%d) [已隐去]\n", indent, k, len(tv))
				} else {
					fmt.Printf("%s%s: %q\n", indent, k, tv)
				}
			case map[string]any, []any:
				fmt.Printf("%s%s:\n", indent, k)
				printShape(tv, indent+"  ")
			default:
				fmt.Printf("%s%s: %v\n", indent, k, tv)
			}
		}
	case []any:
		for i, item := range x {
			fmt.Printf("%s[%d]\n", indent, i)
			printShape(item, indent+"  ")
		}
	}
}

func isSensitiveKey(k string) bool {
	switch k {
	case "accessToken", "refreshToken", "token", "machineToken", "idToken",
		"sessionToken", "secret", "password":
		return true
	}
	return false
}
