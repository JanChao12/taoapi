# TAOAPI

把 **WorkBuddy（腾讯 CodeBuddy）的积分额度**反代成本地 **OpenAI 兼容 API**，
并自动每日签到的**自用**工具。单 exe、零第三方依赖、内嵌浏览器面板、内存占用极小。

> ⚠️ **这是自用工具，不是公共服务。源码公开仅为供人学习。**
> - **许可证限制**：**禁止修改后再上传/再分发，禁止任何盈利使用** —— 见 [`LICENSE`](LICENSE)
> - 需要**你自己的 WorkBuddy 账号**（国内版 `codebuddy.cn` 与/或国际版 `workbuddy.ai`）
> - 只监听 `127.0.0.1`，**不对外提供**，不代为托管任何人的凭据
> - 凭据加密存储在你的本机（Windows DPAPI），**不会**上传到任何第三方
> - 作者不对因使用本工具导致的账号风险、额度损失或违反上游服务条款的后果负责

---

## 定位

| 项 | 说明 |
|---|---|
| **用途** | 绑定 WorkBuddy 账号 → 每日自动签到 → 把额度反代成本地 OpenAI 兼容 API |
| **使用者** | 仅本人（不分发、不商业） |
| **自持** | 不依赖任何第三方项目的更新/存活；参考项目**不进运行时** |
| **语言** | Go（**纯标准库，零第三方依赖**） |
| **形态** | 单 exe + `embed.FS` 内嵌面板 + 外置浏览器 |

---

## 能力

| 能力 | 状态 |
|---|---|
| 多账号管理（导入 / 列表 / 删除 / 启停） | ✅ 面板 + CLI |
| 每日自动签到（Windows 计划任务） | ✅ |
| 额度查询与「最早到期包」调度 | ✅ |
| Chat 反代（流式 + 非流式） | ✅ |
| **失败自动换号**（最多 2 个账号） | ✅ |
| **网页登录**绑定账号（无需官方客户端） | ✅ 面板内扫码/手机号，登录后保留窗口 |
| 用量统计与报告（按账号昵称区分，含 TTFT 首字耗时 / 吞吐 / 缓存命中） | ✅ 面板 + `report` |
| 面板改端口 + 自动重连 | ✅ |
| **国内版 + 国际版双平台** | ✅ 共 **34** 个模型 |
| **凭证保活**（refresh token 自动换新） | ✅ 定时 + 按需（chat 遇鉴权过期即续期） |
| **账号+模型级限流** | ✅ 冷却时间取自上游 429 自述的重置时刻 |
| **三协议接入** | ✅ OpenAI Chat Completions + **Anthropic Messages** + **OpenAI Responses** |
| **桌面形态**（双击即用 + 托盘图标 + 开机自启） | ✅ 无黑窗（GUI 子系统） |
| **检测新版本 + 一键更新**（哈希校验 + 自替换重启） | ✅ 手动触发，无后台静默更新 |
| 数据目录随 exe（照 wild-work 布局） | ✅ 凭据 DPAPI 加密落盘 |

### 与同类工具相比的差异点

- **零第三方依赖**：`go list -m all` 只有自己 ⇒ 无 vendor、离线可构建、体积小
- **单 exe 双击即用**：无 Electron / WebView2 / Node 运行时，面板是 `go:embed` 内嵌的
  手写 HTML/JS（无构建步骤），浏览器外置
- **内存小**：空闲专用工作集约 **6–12 MB**（完整 RSS ≈ 46 MB 含共享页；
  同口径下比参考项目 wild-work 更小）
- **多协议**：同一服务同时暴露 OpenAI / Anthropic / Responses 三种形状，
  客户端按需选 base_url（Anthropic 地址**不带** `/v1`，SDK 自己追加）
- **限流按模型记**：一个模型被上游限流不会拖累同账号的其他模型
  （实测：DeepSeek 被限后切 glm 仍可用），冷却时长取上游自述的重置时刻
- **调度透明**：按「账号内最早到期的包」选号（不是"N 天临期总量"），
  面板直接显示"下一个请求会优先选谁"
- **数据诚实**：模型目录动态拉取，不做静态兜底 —— 上游拉取失败就如实少列，
  不编数据；面板显示的每个数字都来自上游或真实采集

### 模型（实测 **34** 个）

模型目录**动态**从上游获取（**不是**写死的静态表）：

| 平台 | 前缀 | 数量 | 目录来源 |
|---|---|---|---|
| 国内版 | `workbuddy/` | 16 | `GET /v2/.../models` |
| 国际版 | `workbuddyai/` | 18 | **`GET /v3/config`** 优先 + `/v2` 补齐 |

- 国际版模型目录读 **`/v3/config`**（返回 22 条，含 `/v2` 没有的
  `deepseek-v4.1-flash-sg`、`glm-5.3-flash`、`kimi-k2.8-preview`、`gpt-6-astra` 等）
- 🔴 **没有静态兜底表**：`/v3/config` 拉取失败时，国际版就**如实少列**那 5 个模型。
  静态快照只会在"上游拉取失败"时启用，而那一刻恰恰最可能伴随
  "上游改了定价 / 下架了模型"——它会在最不该被信任的时候
  给出假价格、复活已下架的模型。**宁可少列，不可假列。**
  （实测：`/v3` 正常时这 5 个模型本来就由上游给出，所以**正常运行一个都不会少**。）
- 面板有「**刷新**」按钮 → `POST /api/models/refresh` **真的去上游重拉**，
  不是只重读内存（上游下架的模型也会随之消失）
- ⚠️ **`/v3/config` 的 HTTP 200 不能当作凭据有效** —— 空目录（252 字节）也是 200，
  代码按「`code==0` 且 `data.models` 非空」双重校验

---

## 快速开始

**方式一：下载现成的 exe（推荐，无需 Go 环境）**

到 [Releases](https://github.com/JanChao12/taoapi/releases/latest) 下载 `taoapi.exe`，
双击即可 —— 无控制台黑窗，托盘图标常驻，数据目录自动建在 exe 同目录。

**方式二：自己构建（需要 Go 1.22+）**

```powershell
go build -trimpath -ldflags "-H=windowsgui" -o taoapi.exe ./cmd/wbapi
.\taoapi.exe serve
# 面板：http://127.0.0.1:8787/panel/
```

> 带版本号构建：`scripts/build.ps1 -Version x.y.z`（自动做 PE 子系统自检，
> 漏 `-H=windowsgui` 会在构建阶段就报错，而不是等用户看到黑窗）。

在面板里**网页登录**绑定账号（首次绑定不需要装 CodeBuddy 客户端），
然后在 DSH 等客户端里把 base URL 指向 `http://127.0.0.1:8787/v1`。

**三种协议的 base_url**（最容易配错的一处）：

| 协议 | base_url | 说明 |
|---|---|---|
| OpenAI Chat Completions | `http://127.0.0.1:8787/v1` | SDK 自己追加 `/chat/completions` |
| Anthropic Messages | `http://127.0.0.1:8787` | **不带 `/v1`** —— SDK 自己追加 `/v1/messages` |
| OpenAI Responses | `http://127.0.0.1:8787/v1` | SDK 自己追加 `/responses` |

模型 ID 要带**平台前缀**：国内 `workbuddy/deepseek-v4.1-pro`、国际 `workbuddyai/gpt-5.6-luna`。

### 命令

| 命令 | 用途 |
|---|---|
| `wbapi serve [--addr 127.0.0.1:8787]` | 启动服务 + 面板 |
| `wbapi auth import\|list\|remove` | 账号凭证管理 |
| `wbapi checkin [--due]` | 执行签到（`--due` 供计划任务调用，只处理到期的） |
| `wbapi status` | 查看账号状态与额度 |
| `wbapi report` | 生成静态 HTML 用量报告 |
| `wbapi doctor` | 诊断（连通性、凭据、配置） |
| `wbapi version` | 打印版本 |

---

## ⚠️ 三条安全红线（有测试守着，不许绕）

1. **chat 请求绝不能带 `X-Refresh-Token`**（refresh token 只用于 refresh 端点）
2. **token 不进日志 / 事件 / Git / 任何 API 响应**
3. **只监听 `127.0.0.1`**（`serve` 拒绝非回环地址）

这三条写成测试断言（`testutil/fake_upstream.go`），不靠代码审查记忆。
另有两条并发约束（`Account` 字段的读写必须走 `Store` 的锁内快照/变更 API），
详见 [`docs/维护备忘.md`](docs/维护备忘.md) §二之一。

**凭证保活也守住同一条红线**：refresh 请求**只发往 refresh 端点**，绝不超过
每账号一年约 9~10 次（提前 24 小时才真发请求，其余时候只看本地时间，零网络）。

---

## 上游关键约束（详见 [docs/upstream-contract.md](docs/upstream-contract.md)）

| # | 约束 |
|---|---|
| 1 | **强制 `stream: true`** —— 非流式上游直接 400 ⇒ **非流式必须自己聚合** |
| 2 | **额度查询是 POST 且必须带 body**，否则 404 |
| 3 | **签到「已签到」返回 HTTP 400 + 空 body** ⇒ 判定为成功幂等，不算失败 |
| 4 | **积分过期判据是 `CycleEndTime`**（不是 `DeductionEndTime`） |
| 5 | **`developer` role 必须归一为 `system`**（上游白名单不含它） |
| 6 | **`max_completion_tokens` 必须转成 `max_tokens`**（上游只认后者） |
| 7 | ⚠️ **「models 请求成功」不能证明账号能用** —— billing/chat 鉴权口径不同（实测） |

---

## ⚠️ 关于思考档位（最容易踩的坑）

**上游的档位是逐模型不同的，实测三种模式**：

| 模型 | 模式 |
|---|---|
| `space-bunny` | **真旋钮**（5 档单调有效） |
| `glm-5.3` | **低档 ≈ 不思考，max 突变** |
| `deepseek-v4.1-flash` | **纯开关**（档位无差异，但**不传 = 完全不思考**） |

**关键**：`不传 reasoning_effort` ≠ 用默认档。对 deepseek 系是**完全不思考**，
所以**服务端必须显式注入默认 `high`**。

**测试铁证**（`internal/testutil/fixtures/`）：
- `sse-deepseek-v4.1-flash-high.txt` → 41 个思考分片
- `sse-deepseek-noeffort.txt` → **0 个思考分片**

**DSH 接入注意**：DSH **不从 `/v1/models` 读档位**（只读 id/name/contextWindow/maxTokens），
档位**必须**通过配置声明 → 见 [`docs/dsh-workbuddy.yaml`](docs/dsh-workbuddy.yaml)。

---

## 目录结构

```
workbuddy-api/
├─ go.mod                          module workbuddy.local/workbuddy-api
├─ cmd/wbapi/main.go               命令分派
├─ internal/
│  ├─ app/                         生命周期、配置、组件装配、HTTP 路由、面板
│  │  ├─ panel/                    内嵌前端（index.html / app.js / style.css）
│  │  ├─ failover.go               换号逻辑（accountChatter 反向抽象）
│  │  ├─ persist.go                账号落盘
│  │  └─ stats.go / usage_log.go   用量统计
│  ├─ auth/                        账号凭证、导入、存储边界、快照与条件提交
│  ├─ provider/
│  │  ├─ provider.go               小接口：ID/Capabilities/Models/Chat/Checkin
│  │  └─ workbuddy/                渠道实现（国内 + 国际双平台）
│  │     ├─ client.go              上游 URL + 身份头 + HTTP client
│  │     ├─ models.go              模型目录 + 平台前缀
│  │     ├─ chat.go                强制 stream:true 的请求构造
│  │     ├─ sse.go                 上游流解析
│  │     ├─ reasoning.go           逐模型档位映射 + 默认 high
│  │     ├─ identity.go            网页登录后反查 uid
│  │     └─ checkin.go             签到 / 额度
│  ├─ protocol/openai/             入站协议：types / chat 双路径 / sse 编码
│  ├─ router/                      provider 选择、前缀、刷新与错误隔离
│  ├─ storage/                     JSON 原子写 + 按日 JSONL + DPAPI
│  ├─ login/                       网页登录（CDP 驱动）
│  ├─ usage/                       UsageEvent
│  └─ testutil/
│     ├─ fake_upstream.go          假上游（断言头/stream/分片）
│     └─ fixtures/                 ★ 真实样本（go:embed）
├─ docs/
│  ├─ upstream-contract.md         ★ 上游契约（直连实测）
│  ├─ 维护备忘.md                  ★ 改代码前必读（红线、设计"为什么"、本机坑）
│  └─ dsh-workbuddy.yaml           DSH 接入配置样例
└─ scripts/                        离线构建 / 压测
```

---

## 构建与验证

```powershell
# 构建（零第三方依赖，无需 vendor，可离线）
go build -o taoapi.exe ./cmd/wbapi

# 离线构建验证（自持约束）
$env:GOPROXY = "off"; go build ./cmd/wbapi

# 检查点四连
gofmt -l .            # 无输出 = 干净
go build ./...
go vet ./...
go test -count=1 ./...
```

带版本号构建：`scripts/build.ps1 -Version x.y.z`。

> ⚠️ **本机无 gcc ⇒ `go test -race` 不可用**。并发相关结论的依据是
> **静态审计 + 确定性测试**，**不要**表述为"并发已验证"。

---

## 自持原则

| 做法 | 说明 |
|---|---|
| 参考项目**不进运行时** | 其他同类项目只作为行为资料，不作为 module / 子模块 / 外部服务 |
| 零第三方依赖 | `go list -m all` 只有自己 ⇒ 无需 vendor，离线可构建 |
| 离线可构建 | 断开网络后仍能 `go build` |
| provider 隔离 | 单渠道故障不影响其他渠道、不影响公共 handler |
| 上游快照 | 保存版本化请求样本与模型能力快照（fixtures） |

**边界**：无法摆脱对商业上游（WorkBuddy 服务本身）的依赖。
约束是「不受**开源项目**生命周期连累」，不是「不受任何第三方影响」。

---

## 相关文档

| 文件 | 内容 |
|---|---|
| [`docs/维护备忘.md`](docs/维护备忘.md) | **改代码前必读**：红线、设计"为什么"、本机坑、未闭合项 |
| [`docs/upstream-contract.md`](docs/upstream-contract.md) | 上游契约：端点、头、流形状、档位、安全红线 |
| [`docs/dsh-workbuddy.yaml`](docs/dsh-workbuddy.yaml) | DSH 接入配置样例 |
| [`internal/testutil/fixtures/README.md`](internal/testutil/fixtures/README.md) | fixture 使用说明 |
| [`internal/testutil/fixtures/MANIFEST.json`](internal/testutil/fixtures/MANIFEST.json) | fixture 索引与断言 |

---

## 许可证

**自定义「源码可见」许可证 —— 仅供学习。** 详见 [`LICENSE`](LICENSE)。

一句话概括：

| | |
|---|---|
| ✅ **可以** | 阅读、研究源码；在本地为学习目的编译运行 |
| ❌ **不可以** | **修改后再上传/再分发**（含衍生版） |
| ❌ **不可以** | **任何形式的盈利使用**（售卖、有偿服务、广告变现等） |
| ❌ **不可以** | 移除许可证与版权声明 |

⚠️ 这**不是** MIT / Apache / GPL 等 OSI 开源许可证，请勿如此理解。
采用这种方式是因为本项目直接对接商业上游服务，作者不希望衍生分发
带来不必要的风险。其他用途请先联系作者取得书面许可。
