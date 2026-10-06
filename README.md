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
| **硬约束** | 目标机器 **8 GB 内存**；`serve` 空闲 RSS **≤30 MB**（实测约 17 MB） |
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
| **网页登录**绑定账号（无需官方客户端） | ✅ 面板内扫码/手机号 |
| 用量统计与报告（按账号昵称区分） | ✅ 面板 + `report` |
| 面板改端口 + 自动重连 | ✅ |
| **国内版 + 国际版双平台** | ✅ 共 **34** 个模型 |
| 自动续期（refresh token 换新） | ❌ **尚未接入** —— 凭据失效后需重新登录 |

### 模型（实测 **34** 个）

模型目录**动态**从上游获取（**不是**写死的静态表）：

| 平台 | 前缀 | 数量 | 目录来源 |
|---|---|---|---|
| 国内版 | `workbuddy/` | 16 | `GET /v2/.../models` |
| 国际版 | `workbuddyai/` | 18 | **`GET /v3/config`** 优先 + `/v2` 补齐 |

- 国际版曾用静态表，现改为读 **`/v3/config`**（返回 22 条，含 `/v2` 没有的
  `deepseek-v4.1-flash-sg`、`glm-5.3-flash`、`kimi-k2.8-preview`、`gpt-6-astra` 等）
- 🔴 **静态表退役为兜底**：仅在 `/v3/config` 拉取失败时使用
- 面板有「**刷新**」按钮 → `POST /api/models/refresh` **真的去上游重拉**，
  不是只重读内存（上游下架的模型也会随之消失）
- ⚠️ **`/v3/config` 的 HTTP 200 不能当作凭据有效** —— 空目录（252 字节）也是 200，
  代码按「`code==0` 且 `data.models` 非空」双重校验

---

## 快速开始

```powershell
# 1. 构建（需要 Go 1.22+）
go build -o wbapi.exe ./cmd/wbapi

# 2. 启动服务（只监听回环）
.\wbapi.exe serve

# 3. 打开面板
#    http://127.0.0.1:8787/panel/
```

在面板里**网页登录**绑定账号（首次绑定不需要装 CodeBuddy 客户端），
然后在 DSH 等客户端里把 base URL 指向 `http://127.0.0.1:8787/v1`。

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
go build -o wbapi.exe ./cmd/wbapi

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
