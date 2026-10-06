// Package workbuddy 实现 WorkBuddy（CodeBuddy）渠道的上游适配。
//
// 本文件负责：上游 URL 构造、身份头注入、HTTP client 配置。
//
// 🔴 安全红线（见 docs/upstream-contract.md §二）：
//   - chat 请求【绝不】携带 X-Refresh-Token
//   - refresh token 只允许出现在 refresh 端点
//   - 凭据不得进日志
package workbuddy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"workbuddy.local/workbuddy-api/internal/upstream"
)

// 上游端点。集中在 app/limits.go，避免散落。
const (
	baseChat    = upstream.ChatBase
	baseBilling = upstream.BillingBase

	pathModels       = upstream.PathModels
	pathV3Config     = upstream.PathV3Config
	pathChat         = upstream.PathChat
	pathUserRes      = upstream.PathUserResource
	pathDailyCheckin = upstream.PathDailyCheckin
	pathTokenRefresh = upstream.PathTokenRefresh
	pathLoginAccount = upstream.PathLoginAccount
)

// 伪装标识：让上游把调用记录显示为 WorkBuddy 桌面端。
//
// 这些值来自实测还原（见 docs/upstream-contract.md §3.3）。
//
// ⚠️ 这两个值是**唯一会随上游升级而失效的硬编码**（2026-10-05 评审指出）。
// 若腾讯升级客户端、并要求版本号在下限之上，本工具会**整体失效**。
// 因此它们可以通过环境变量覆盖，无需重新编译：
//
//	WBAPI_CLIENT_UA          覆盖 User-Agent
//	WBAPI_IDE_VERSION        覆盖 X-IDE-Version
//
// 默认值保持实测值不变 —— 覆盖是"应急通道"，不是让用户随便改。
const (
	clientUADefault   = "CLI/2.63.2 CodeBuddy/2.63.2"
	ideVersionDefault = "5.5.4"
	envClientUA       = "WBAPI_CLIENT_UA"
	envIDEVersions    = "WBAPI_IDE_VERSION"
	ideName           = "WorkBuddy"
	ideType           = "WorkBuddy"
	originCN          = "https://www.codebuddy.cn"
	acceptLanguage    = "zh-CN"
	acceptHeader      = "application/json, text/plain, */*"
	codeBuddyFlag     = "1"
	agentPurpose      = "conversation"
)

// clientUA 返回生效的 User-Agent（环境变量优先）。
func clientUA() string { return envOr(envClientUA, clientUADefault) }

// ideVersion 返回生效的 X-IDE-Version（环境变量优先）。
func ideVersion() string { return envOr(envIDEVersions, ideVersionDefault) }

// envOr 返回环境变量值，为空时返回兜底默认。
func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// IdentityInfo 是当前生效的伪装标识（供 /status 与 doctor 展示）。
type IdentityInfo struct {
	// UA / IDEVersions 是实际发出去的值。
	UA          string `json:"clientUA"`
	IDEVersions string `json:"ideVersion"`

	// UACustom / IDEVersionsCustom 表示该值是否来自环境变量覆盖。
	//
	// 为什么要区分：排查"是不是版本问题"时，得先知道
	// "我用的是实测默认值还是有人改过" —— 否则改过的人自己都忘了。
	UACustom          bool `json:"clientUACustom"`
	IDEVersionsCustom bool `json:"ideVersionCustom"`
}

// IdentityVersionsInfo 返回带"是否被覆盖"标记的完整信息。
func IdentityVersionsInfo() IdentityInfo {
	return IdentityInfo{
		UA:                clientUA(),
		IDEVersions:       ideVersion(),
		UACustom:          strings.TrimSpace(os.Getenv(envClientUA)) != "",
		IDEVersionsCustom: strings.TrimSpace(os.Getenv(envIDEVersions)) != "",
	}
}

// IdentityVersions 报告当前生效的伪装版本，供 /status 与 doctor 展示。
//
// 🔴 为什么要暴露它（Codex 评审建议）：
//
//	"上游升级导致版本号被拒"是本项目最脆弱的一环，而它失效时的表现是
//	**所有请求一起失败** —— 用户很难判断是账号问题、网络问题还是版本问题。
//	把当前生效值显式暴露出来，至少能让人一眼看出"我用的是哪个版本"，
//	并在排查时把"改环境变量试试"变成可操作的动作。
func IdentityVersions() (ua, ide string) { return clientUA(), ideVersion() }

// Credential 是一个账号的完整凭证材料。
//
// ⚠️ 本结构【不含】refresh token 字段的日志输出；
// 调用方必须保证 String()/序列化时不泄露。
type Credential struct {
	// AccessToken 用于 Authorization: Bearer。
	AccessToken string

	// UID 用户 ID，用于 X-User-Id 及机器/会话 ID 派生。
	UID string

	// EnterpriseID 企业 ID；为空时发 X-No-Enterprise-Id: 1。
	EnterpriseID string

	// Domain 域；为空时发 X-No-Department-Info: 1。
	Domain string

	// RefreshToken ⚠️ 只允许用于 refresh 端点，绝不进入 chat 请求。
	RefreshToken string
}

// MachineID 按实测规则派生稳定机器 ID。
//
// 实测规则：sha256("wb2a:machine:" + uid)，取前 36 个 hex 字符。
//
// ⚠️ 该规则是【实测还原】而非官方文档，因此：
//   - 允许通过 Credential 覆盖（见机器 ID 覆盖字段，后续步骤补）
//   - 若上游变更派生方式，只需改这里
func (c Credential) MachineID() string { return deriveStableID(c.UID, "machine") }

// SessionID 按实测规则派生稳定会话 ID。
func (c Credential) SessionID() string { return deriveStableID(c.UID, "session") }

func deriveStableID(uid, purpose string) string {
	sum := sha256.Sum256([]byte("wb2a:" + purpose + ":" + uid))
	return hex.EncodeToString(sum[:18]) // 36 个 hex 字符
}

// Client 是 WorkBuddy 上游客户端。
type Client struct {
	http        *http.Client
	baseChat    string
	baseBilling string

	// overridden 表示基址已被 SetBases 显式指定（测试指假上游）。
	//
	// 置位后 SetPlatform 不再改基址 —— 否则测试会被生产代码
	// 覆盖回真实上游地址，让单测变成联网测试（真实踩过）。
	overridden bool
}

// NewClient 构造上游客户端。
//
// 超时策略（见 internal/app/limits.go）：
//   - 建连/TLS/响应头用短超时
//   - 【不设】http.Client.Timeout —— 那会杀掉长思考流
//   - 流内空闲超时由 SSE 读取层负责（SSEIdleTimeout）
func NewClient() *Client {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   upstream.DialTimeout,
			KeepAlive: upstream.KeepAlive,
		}).DialContext,
		TLSHandshakeTimeout:   upstream.TLSHandshakeTimeout,
		ResponseHeaderTimeout: upstream.ResponseHeaderTimeout,
		MaxIdleConns:          16,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       upstream.IdleConnTimeout,
		ForceAttemptHTTP2:     true,
	}
	return &Client{
		http: &http.Client{
			Transport: transport,
			// 故意不设 Timeout：见上方说明。
		},
		baseChat:    baseChat,
		baseBilling: baseBilling,
	}
}

// HTTPClient 暴露底层客户端（测试用）。
func (c *Client) HTTPClient() *http.Client { return c.http }

// SetBases 覆盖上游基址。
//
// 两种用途：
//  1. **测试**把请求指向假上游（最常用）
//  2. 生产按平台切换（国内/国际）—— 但那条路径请用 SetPlatform，
//     它会在"已被显式覆盖"时**不覆盖**（见下）
//
// 🔴 overridden 标记的作用（2026-10-06 加）：
//
//	测试的写法是 `SetBases(fake)` 之后才构造 provider。
//	若生产代码（workbuddyProviderFor）在构造时无条件按平台设基址，
//	就会把测试的假上游地址**覆盖回生产地址** —— 请求打到真实上游，
//	单元测试**从隔离变成了联网**（本项目真实踩过，表现为大规模 401）。
//
//	所以：SetBases 一旦被调用就记下 overridden，
//	SetPlatform 看到该标记就不再动基址。
func (c *Client) SetBases(chat, billing string) {
	if chat != "" {
		c.baseChat = chat
		c.overridden = true
	}
	if billing != "" {
		c.baseBilling = billing
		c.overridden = true
	}
}

// SetPlatform 按平台设置基址 —— **除非基址已被显式覆盖**。
//
// 返回是否真的改了（测试可据此断言）。
func (c *Client) SetPlatform(chat, billing string) bool {
	if c.overridden {
		return false
	}
	c.baseChat = chat
	c.baseBilling = billing
	return true
}

// ChatURL 返回对话端点完整 URL。
func (c *Client) ChatURL() string { return c.baseChat + pathChat }

// ModelsURL 返回模型目录完整 URL。
func (c *Client) ModelsURL() string { return c.baseChat + pathModels }

// V3ConfigURL 返回 /v3/config 完整 URL（国际版完整模型配置）。
//
// ⚠️ 用 baseChat（与模型目录同基址）：国际版 chat 与 billing 同域名，
// 实测该路径在 www.workbuddy.ai 上可用。
func (c *Client) V3ConfigURL() string { return c.baseChat + pathV3Config }

// UserResourceURL 返回额度查询完整 URL。
func (c *Client) UserResourceURL() string { return c.baseBilling + pathUserRes }

// DailyCheckinURL 返回签到完整 URL。
func (c *Client) DailyCheckinURL() string { return c.baseBilling + pathDailyCheckin }

// TokenRefreshURL 返回 token 刷新完整 URL。
func (c *Client) TokenRefreshURL() string { return c.baseChat + pathTokenRefresh }

// LoginAccountURL 返回"当前 token 是谁"的查询端点。
//
// 用途：网页登录拿到的凭据只有 token、没有 uid，而 uid 是账号主键，
// 落盘前必须靠这个端点补全（见 identity.go）。
func (c *Client) LoginAccountURL() string { return c.baseBilling + pathLoginAccount }

// commonHeaders 设置所有上游请求共享的头。
func commonHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", acceptHeader)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("X-CodeBuddy-Request", codeBuddyFlag)
	req.Header.Set("Origin", originCN)
	req.Header.Set("Referer", originCN+"/")
	// ⚠️ User-Agent 用 Set 在这里是合法的（Go 不保留该头）。
	req.Header.Set("User-Agent", clientUA())
	req.Header.Set("Accept-Language", acceptLanguage)
}

// applyChatHeaders 注入 chat 请求头。
//
// 🔴 本函数【绝不】设置 X-Refresh-Token —— 这是安全红线，
//
//	并由 TestChatHeadersNeverCarryRefreshToken 断言。
func applyChatHeaders(req *http.Request, cred Credential, messageID string) {
	commonHeaders(req)

	if cred.AccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+cred.AccessToken)
	} else {
		req.Header.Set("X-No-Authorization", "1")
	}

	if cred.UID != "" {
		req.Header.Set("X-User-Id", cred.UID)
		req.Header.Set("X-Machine-ID", cred.MachineID())
		req.Header.Set("X-Session-ID", cred.SessionID())
	} else {
		req.Header.Set("X-No-User-Id", "1")
	}

	if cred.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", cred.EnterpriseID)
	} else {
		req.Header.Set("X-No-Enterprise-Id", "1")
	}

	if cred.Domain != "" {
		req.Header.Set("X-Domain", cred.Domain)
	} else {
		req.Header.Set("X-No-Department-Info", "1")
	}

	// 用量归属（伪装成桌面端）
	req.Header.Set("X-Agent-Purpose", agentPurpose)
	req.Header.Set("X-IDE-Name", ideName)
	req.Header.Set("X-IDE-Type", ideType)
	req.Header.Set("X-IDE-Version", ideVersion())
	req.Header.Set("X-Product", ideName)

	// 链路追踪
	setTraceHeaders(req, messageID)
}

// applyBillingHeaders 注入额度/签到请求头。
func applyBillingHeaders(req *http.Request, cred Credential) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", clientUA())
	if cred.AccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+cred.AccessToken)
	}
	if cred.UID != "" {
		req.Header.Set("X-User-Id", cred.UID)
	}
	if cred.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", cred.EnterpriseID)
		req.Header.Set("X-Tenant-Id", cred.EnterpriseID)
	}
	if cred.Domain != "" {
		req.Header.Set("X-Domain", cred.Domain)
	}
}

// setTraceHeaders 注入会话与链路追踪头。
func setTraceHeaders(req *http.Request, messageID string) {
	convID := newMessageID()
	req.Header.Set("X-Conversation-Request-ID", convID)
	req.Header.Set("X-Conversation-Message-ID", messageID)
	req.Header.Set("X-Request-ID", messageID)
	req.Header.Set("X-Root-Request-ID", convID)
	req.Header.Set("X-B3-TraceId", messageID)
	spanID := messageID
	if len(spanID) > 16 {
		spanID = spanID[:16]
	}
	req.Header.Set("X-B3-SpanId", spanID)
	req.Header.Set("X-B3-Sampled", "1")
}

// newMessageID 生成 32 个 hex 字符的随机 ID（16 字节）。
func newMessageID() string {
	b := make([]byte, 16)
	if _, err := cryptoRead(b); err != nil {
		// 退化路径：用时间纳秒填充，保证仍有值（不会用于安全用途）。
		n := time.Now().UnixNano()
		for i := 0; i < 8; i++ {
			b[i] = byte(n >> (8 * i))
			b[15-i] = byte(n >> (8 * (7 - i)))
		}
	}
	return hex.EncodeToString(b)
}

// String 故意不暴露任何凭据材料，避免误入日志。
func (c Credential) String() string {
	return fmt.Sprintf("Credential{uid=%s, hasToken=%t, hasRefresh=%t}",
		maskUID(c.UID), c.AccessToken != "", c.RefreshToken != "")
}

// GoString 同 String，避免 %#v 泄露。
func (c Credential) GoString() string { return c.String() }

func maskUID(uid string) string {
	if len(uid) <= 8 {
		return "***"
	}
	return uid[:8] + "…"
}
