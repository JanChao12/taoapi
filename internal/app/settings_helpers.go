package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"workbuddy.local/workbuddy-api/internal/config"
	"workbuddy.local/workbuddy-api/internal/datadir"
)

// settingsBodyMaxBytes 是设置请求体的上限。
//
// 设置里最大的是别名表。给 1 MiB 足够容纳几千条别名，
// 同时防止有人拿这个接口灌大 Body（与聊天接口同样的防御思路）。
const settingsBodyMaxBytes = 1 << 20

// decodeJSONBody 读取并解析请求体，带大小上限。
//
// 与聊天接口一致地先限制再读，避免超大 body 进内存。
func decodeJSONBody(r *http.Request, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, settingsBodyMaxBytes+1))
	if err != nil {
		return fmt.Errorf("读取请求体失败: %w", err)
	}
	if len(body) > settingsBodyMaxBytes {
		return fmt.Errorf("请求体过大（上限 %d 字节）", settingsBodyMaxBytes)
	}
	if len(body) == 0 {
		return fmt.Errorf("请求体为空")
	}
	return json.Unmarshal(body, v)
}

// dataDir 返回数据目录。
//
// 🔴 2026-10-07 改：位置由 `internal/datadir` 统一决定
// （**exe 同目录**，照 wild-work 布局；见该包注释）。
//
// 原先这里自己拼了一遍 `~/.wbapi`，与 auth/config/usage 三处重复 ——
// 四份实现必须永远一致，否则会出现"账号落在 A、配置落在 B"。
func dataDir() string {
	return datadir.Root()
}

// listenAddrForPort 由端口拼出实际监听地址。
//
// host 恒为 127.0.0.1：这是本项目的架构红线（只监听回环），
// 不提供配置项，避免用户误配成 0.0.0.0 把额度接口暴露到局域网。
func listenAddrForPort(_ Deps, port int) string {
	if port <= 0 {
		port = config.DefaultPort
	}
	return fmt.Sprintf("127.0.0.1:%d", port)
}

// portChanged 判断配置里的端口是否与【当前实际监听】的端口不同。
//
// 用来驱动面板的"需重启生效"提示。没有实际监听信息时（如测试环境）
// 保守返回 false，不误报。
func (d Deps) portChanged(configured int) bool {
	if d.ListenPort == 0 {
		return false
	}
	return configured != d.ListenPort
}

// splitHostPort 从 "127.0.0.1:8787" 取出端口；失败返回 0。
func splitHostPort(addr string) int {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return 0
	}
	var p int
	if _, err := fmt.Sscanf(addr[i+1:], "%d", &p); err != nil {
		return 0
	}
	return p
}
