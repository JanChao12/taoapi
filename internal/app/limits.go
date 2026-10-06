package app

import (
	"time"

	"workbuddy.local/workbuddy-api/internal/upstream"
)

// 本文件集中所有资源限额与验收参数（依据 docs/limits.md，经 DSH × Codex 确认）。
//
// ⚠️ 修改这些值时必须同步更新 docs/limits.md 与压力测试。

const (
	// DefaultAddr 服务默认监听地址。只绑回环，不对外。
	DefaultAddr = "127.0.0.1:8787"

	// MaxConcurrentStreams 同时进行的上游流上限。
	// 超限时立即返回 429，而不是无限创建 goroutine/连接。
	// 依据：单用户场景，8 足够；同时是压力测试的达标线。
	MaxConcurrentStreams = 8

	// MaxRequestBodyBytes 入站请求体上限（8 MiB）。
	// 说明：2 MiB 对大上下文/工具调用过紧，故取 8 MiB。
	MaxRequestBodyBytes = 8 << 20
)

// HTTP 超时。
//
// ⚠️ 关键：这里【没有】整体 WriteTimeout 的语义。
//
//	SSE 长连接的总时长取决于模型思考时间（实测 max 档单次可达 95 秒以上），
//	若用 http.Server.WriteTimeout 或 http.Client.Timeout 设固定总时长，
//	会把长思考流中途杀掉。
//	正确做法：连接/响应头用短超时，流内用「两次读取之间」的空闲超时。
const (
	// ReadHeaderTimeout 读取请求头的超时。
	ReadHeaderTimeout = 10 * time.Second

	// ReadTimeout 读取整个请求（含 body）的超时。
	// 请求体本身不大（≤8 MiB），10 秒足够。
	ReadTimeout = 30 * time.Second

	// IdleTimeout keep-alive 空闲超时。
	IdleTimeout = 120 * time.Second

	// ShutdownTimeout 收到退出信号后等待在途请求结束的时间。
	ShutdownTimeout = 10 * time.Second
)

// 上游连接相关的超时（Transport 层）。
const (
	// UpstreamDialTimeout 与上游建连超时（定义在 upstream 包）。
	UpstreamDialTimeout = upstream.DialTimeout

	// UpstreamTLSHandshakeTimeout TLS 握手超时。
	UpstreamTLSHandshakeTimeout = upstream.TLSHandshakeTimeout

	// UpstreamResponseHeaderTimeout 等待上游响应头的超时。
	// 上游会先返回 200 + text/event-stream 头，然后才慢慢推数据，
	// 所以这个超时只覆盖"建连到出响应头"这一段。
	UpstreamResponseHeaderTimeout = upstream.ResponseHeaderTimeout

	// SSEIdleTimeout 两次 SSE 读取之间的最大间隔（空闲超时）。
	// ⚠️ 这是「流内空闲」超时，不是「整个请求」超时。
	// 超过该间隔没有任何数据到达，才判定上游卡死并关闭连接。
	SSEIdleTimeout = 120 * time.Second
)

// 上游端点：定义在 internal/upstream 包，这里做别名以便 app 内部引用。
//
// 之所以外移：provider 实现需要这些常量，但不应依赖 app 包
// （否则 app 的集成测试引入 provider 会形成 import cycle）。
const (
	// UpstreamChatBase 对话与模型目录。
	UpstreamChatBase = upstream.ChatBase

	// UpstreamBillingBase 额度与签到。
	UpstreamBillingBase = upstream.BillingBase
)

// 对外暴露的产品标识。
const (
	// ModelPrefix 模型名渠道前缀。为将来接入多平台预留。
	ModelPrefix = "workbuddy/"

	// ProductName 本服务对外名称（2026-10-06 委托人改名为 TAOAPI）。
	//
	// ⚠️ 改名**不影响兼容性**：这个值只用于 /status 的展示与
	//	健康检查的自识别，不参与任何协议字段或磁盘格式。
	//	/data 目录名、exe 名、账号文件路径都**没变** ——
	//	改了会让升级后找不到旧数据。
	ProductName = "TAOAPI"
)

// 存储与日志限额。
const (
	// JSONLMaxFileBytes 单个 JSONL 文件上限（16 MiB）。
	// 与"按日切换"共同构成轮转条件，先到者触发。
	JSONLMaxFileBytes = 16 << 20

	// JSONLRotateDaily 是否按日轮转。
	JSONLRotateDaily = true
)
