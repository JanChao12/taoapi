// Package update 实现「检查新版本 → 下载 → 校验 → 自替换 → 重启」的自动更新。
//
// ═══════════════════════════════════════════════════════════════════
// 🔴 这是本项目**最危险**的一类功能：它把"运行远程字节"变成自动化
// ═══════════════════════════════════════════════════════════════════
//
//	在此之前，信任模型是"用户手动下载、手动运行"——每一步都有人把关。
//	加了自动更新之后，链路上任何一环被攻破，攻击者就能在用户机器上
//	执行任意代码，而这台机器上存着**真实的账号凭据**（auths/accounts.json）。
//
//	因此本包的设计原则（每一条都有对应测试）：
//
//	1. **只认 HTTPS + 固定主机白名单** —— 不接受 http、不接受任意主机，
//	   防止把下载地址做成配置项后被指向恶意服务器。
//	2. **必须校验哈希** —— 用 GitHub Release API 的 `digest` 字段
//	   （格式 `sha256:...`）作为期望值，下载后逐字节比对；不一致就拒绝。
//	   ⚠️ 诚实边界：digest 与资产**同源**（都来自 GitHub API），
//	      所以它防的是"传输途中被篡改 / 分片损坏"，
//	      **防不住"攻击者同时控制 GitHub 账号"**。那条只能靠账号安全。
//	3. **不自作主张** —— 检查与下载由面板显式触发；绝不后台静默替换。
//	4. **可注入的基础 URL** —— 测试用假服务器，生产写死 GitHub。
//
// # 为什么版本比较要单独抽出来
//
//	"要不要更新"的判断错一次，用户就会反复被提示更新（或永远收不到）。
//	所以 Version 比较做成**纯函数 + 表驱动测试**，不依赖网络。
package update

import (
	"fmt"
	"strconv"
	"strings"
)

// Version 是一个可比较的版本号（形如 0.1.4 / v0.1.4 / 0.1.4-taoapi）。
//
// 只解析出**数字段**用于比较；预发布后缀（如 -taoapi）**不参与排序**。
//
// 为什么这样取舍：
//
//	本项目的版本号只用于"比大小、决定要不要更新"，不做语义化排序的
//	完整实现（那要处理 alpha/beta/rc 的复杂优先级）。委托人实际的
//	版本串是 `0.1.4-taoapi` —— 后缀是构建标记，不是预发布语义，
//	把它当预发布会导致 `0.1.4-taoapi` < `0.1.4` 这种反直觉结果。
type Version struct {
	// Numbers 是点分数字段，如 [0,1,4]。
	Numbers []int
	// Raw 是原始字符串（用于展示与错误信息）。
	Raw string
	// Dev 表示这是一个未注入版本的开发构建（raw 为 "dev" 或空）。
	Dev bool
}

// ParseVersion 解析版本串。
//
// 容错规则（都是为了"绝不因为版本串格式怪就不敢更新"）：
//   - 允许前导 `v`（GitHub tag 惯例是 `v0.1.5`）；
//   - 只取数字段，遇到非数字段（如 `-taoapi`）就停止；
//   - "dev"/空 ⇒ Dev=true（本地构建，见 NeedsUpdate 的处理）；
//   - 完全解析不出数字（如 "abc"）⇒ Dev=true，同样按不可比处理。
func ParseVersion(s string) Version {
	raw := s
	s = strings.TrimSpace(s)
	// 去掉前导 v（大小写都容忍）
	if len(s) > 0 && (s[0] == 'v' || s[0] == 'V') {
		s = s[1:]
	}

	// 按 '-' 或 '+' 截断：后缀不参与比较（见 Version 的说明）。
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}

	parts := strings.Split(s, ".")
	nums := make([]int, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			break
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			// 非数字段（例如 "dev" 里的 "dev"）→ 停止解析。
			break
		}
		nums = append(nums, n)
	}

	if len(nums) == 0 {
		// 解析不出任何数字：视为开发构建，不可比。
		return Version{Raw: raw, Dev: true}
	}
	return Version{Numbers: nums, Raw: raw}
}

// Compare 比较两个版本：a<b 返回 -1，a==b 返回 0，a>b 返回 1。
//
// 缺位按 0 补（`0.2` == `0.2.0`），这是版本比较的通行做法。
func (a Version) Compare(b Version) int {
	n := len(a.Numbers)
	if len(b.Numbers) > n {
		n = len(b.Numbers)
	}
	for i := 0; i < n; i++ {
		var x, y int
		if i < len(a.Numbers) {
			x = a.Numbers[i]
		}
		if i < len(b.Numbers) {
			y = b.Numbers[i]
		}
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
	}
	return 0
}

// NeedsUpdate 判断"当前版本是否需要更新到远程版本"。
//
// 🔴 关键取舍：**当前版本是 dev（本地构建）时，永远返回 false。**
//
//	开发机上的版本串是 "dev"（没走 -ldflags 注入）。若不特判，
//	`ParseVersion("dev")` 得不到数字，与远程一比会被判成"需要更新"，
//	于是**开发者在自己的机器上每次都被提示升级**，甚至可能把自己
//	正在调试的 exe 覆盖掉。这属于"把开发者当普通用户"的错误。
//
//	返回 false 的语义是"不提示更新"，不是"已是最新"——面板可以据此
//	显示"开发构建，不参与更新检查"。
func NeedsUpdate(current, latest string) bool {
	cur := ParseVersion(current)
	if cur.Dev {
		return false
	}
	lat := ParseVersion(latest)
	if lat.Dev {
		// 远程版本解析不出（tag 命名异常）⇒ 不敢动，当作没有更新。
		return false
	}
	return lat.Compare(cur) > 0
}

// NormalizeTag 把 GitHub tag 规范成展示用的版本串。
//
// tag 惯例是 `v0.1.5`，展示时去掉前导 v（与 `wbapi version` 的输出一致）。
func NormalizeTag(tag string) string {
	t := strings.TrimSpace(tag)
	if len(t) > 0 && (t[0] == 'v' || t[0] == 'V') {
		// 只在去掉 v 之后仍以数字开头时才去掉，
		// 避免把 "version1" 这种误伤成 "ersion1"。
		if len(t) > 1 && t[1] >= '0' && t[1] <= '9' {
			return t[1:]
		}
	}
	return t
}

// ParseDigest 解析 GitHub 的 `digest` 字段（形如 `sha256:abcdef...`）。
//
// 返回小写 hex 与算法名；
//   - 字段为空、或算法不是 sha256、或 hex 长度不是 64 ⇒ 返回错误。
//
// 🔴 为什么长度也要严格校验：`digest` 缺失时若"宽松跳过校验"，
//
//	自动更新就退化成"下载什么就跑什么"——那正是本包最不能接受的失败模式。
//	宁可拒绝更新，也不能在没有校验值的情况下替换 exe。
func ParseDigest(d string) (algo string, hex string, err error) {
	s := strings.TrimSpace(d)
	if s == "" {
		return "", "", fmt.Errorf("update：下载源未提供校验值（digest 为空），拒绝更新")
	}
	i := strings.IndexByte(s, ':')
	if i <= 0 || i == len(s)-1 {
		return "", "", fmt.Errorf("update：校验值格式不正确: %q（期望 sha256:<64位hex>）", s)
	}
	algo = strings.ToLower(strings.TrimSpace(s[:i]))
	hex = strings.ToLower(strings.TrimSpace(s[i+1:]))
	if algo != "sha256" {
		return "", "", fmt.Errorf("update：不支持的校验算法 %q（只接受 sha256）", algo)
	}
	if len(hex) != 64 {
		return "", "", fmt.Errorf("update：sha256 校验值长度应为 64，实际 %d", len(hex))
	}
	for _, c := range hex {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return "", "", fmt.Errorf("update：校验值含非 hex 字符: %q", hex)
		}
	}
	return algo, hex, nil
}
