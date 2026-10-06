# Fixture 使用说明（假上游测试样本）

> **全部数据取自 2026-10-04 直连 WorkBuddy 上游的真实响应**，仅脱敏与裁剪，
> **不含任何凭据**。用于 `internal/testutil/fake_upstream.go` 的契约测试。

---

## 一、为什么需要这些 fixture

从前面实测得出两条硬教训：

1. **中间层不可信** —— 经 wild-work 测出的 reasoning 值与直连差 **18 倍**，
   说明中间层会注入额外内容。**我们的测试样本必须直连抓取。**
2. **上游行为无法猜** —— 上游对 `reasoning_effort` 的接受范围、
   流式分片形状、`reasoning_content` 的存在与否，**都必须靠真实样本固定**。

→ 这些 fixture 就是**回归基线**：以后改 provider 代码，跑一遍就能知道有没有破坏协议。

---

## 二、文件清单与用途

| 文件 | 类型 | 用途 |
|---|---|---|
| `MANIFEST.json` | 索引 | **总目录**：每个 fixture 的用途、断言、实测值 |
| `models-listing.json` | 上游响应 | 假上游的 `/v2/enterprises/personal/models` |
| `v1-models-exposed.json` | 期望输出 | 反代对外 `/v1/models` 应有的形状 |
| `sse-deepseek-v4.1-flash-high.txt` | ★ 上游流 | **最完整样本**：41 个思考分片 + 1 个正文分片 |
| `sse-deepseek-noeffort.txt` | ★ 上游流 | 证明「不传档位 = 完全不思考」 |
| `sse-space-bunny-max.txt` | 上游流 | 平滑旋钮型模型样本 |
| `sse-glm-5.3-max.txt` | 上游流 | max 突变型模型样本 |
| `chat-completion-aggregated.json` | 期望输出 | 非流式聚合后应有的形状 |
| `billing-user-resource.json` | 上游响应 | 额度查询（真实数据） |
| `billing-daily-checkin.json` | 上游响应 | 签到（已签到场景） |

---

## 三、★ 核心测试矩阵：三个模型、三种模式

这是本项目**最重要的一组测试**，fake upstream 必须能分别断言：

| 模型 | 模式 | 档位 | 关键断言 |
|---|---|---|---|
| `space-bunny` | **knob（真旋钮）** | low→max 共 5 档 | 均值单调递增，档位有真实效果 |
| `glm-5.3` | **knob-low-flat** | low/high/max | max 档思考量跳 18 倍 |
| `deepseek-v4.1-flash` | **switch（纯开关）** | off/high | **不传 = 完全不思考，必须显式注入默认 high** |

**实测证据**（n=6/档，直连）：

```
space-bunny  : 44 → 53.7 → 61.7 → 79.7 → 83.7   严格单调，low 上限 47 < xhigh 下限 55
glm-5.3      : low 中位 78 / high 178 / max 3232  后两档差 18 倍
deepseek     : 各档 178–257 随机排列；不传时 = 0  实为开关，非旋钮
```

---

## 四、两个 fixture 的对照（最关键的一对）

同一模型、同一问题，**只改「传不传档位」**：

| | `sse-deepseek-v4.1-flash-high.txt` | `sse-deepseek-noeffort.txt` |
|---|---|---|
| 请求 `reasoning_effort` | `"high"` | **不传** |
| data 行数 | **45** | **4** |
| 思考分片数 | **41** | **0** |
| 思维链字符 | 70 | **0** |
| `reasoning_tokens` | 41 | **0** |

**→ 这一对是「必须显式注入默认档位」的铁证。**

---

## 五、⚠️ 写解析器必须知道的 3 个上游细节

### 5.1 delta 的字段是「全部存在、空值为空串」

不是「缺字段」，而是**六个字段永远都在**，不需要的用 `""` 表示：

```json
"delta": {
  "content": "",
  "reasoning_content": "我们需要",
  "function_call": null,
  "refusal": "",
  "tool_calls": [],
  "extra_fields": null
}
```

**→ 不能靠「字段是否存在」判断类型，必须看值是否非空。**

### 5.2 思考与正文不是分开发的事件

实测：45 个 data 行里，**41 个只带 `reasoning_content`，1 个只带 `content`，0 个同时带**。
但字段结构上两者并存 → 解析器要**分别累加**，不能假设「先思考完再正文」。

### 5.3 上游强制 `stream: true`

**发 `stream: false` 直接 400。** 所以：
- 流式路径：透传
- **非流式路径：必须消费同一条流，自己聚合**

`chat-completion-aggregated.json` 就是聚合后的目标形状。

---

## 六、安全红线（测试必须断言）

| 必须**发送** | 必须**不发送** |
|---|---|
| `stream: true` | 🔴 **`X-Refresh-Token`**（chat 请求绝不能带） |
| `Authorization: Bearer …` | `stream: false` |
| `X-User-Id` / `X-Machine-ID` / `X-Session-ID` | |
| `User-Agent: CLI/2.63.2 CodeBuddy/2.63.2` | |
| `X-IDE-Name: WorkBuddy` / `X-Product: WorkBuddy` | |

**fake upstream 收到违规请求必须让测试立即失败**，而不是靠代码审查记忆。

---

## 七、怎么用（Go 侧建议）

```go
// internal/testutil/fake_upstream.go 的职责
type FakeUpstream struct {
    t *testing.T
}

// 1) 启动假上游，返回 baseURL
// 2) 收到请求时断言：
//    - Header 里没有 X-Refresh-Token      （安全红线）
//    - stream == true                     （上游强制）
//    - 必需身份头齐全
// 3) 按 model + reasoning_effort 选择对应的 .txt fixture 回放
// 4) 非流式请求时，仍然回放同一份 SSE（模拟上游行为）
```

**回放建议**：SSE fixture **原样逐行写出**，不要重新序列化
（因为要测试解析器对真实分片边界的容忍度）。

---

## 八、数据来源与可复现性

| 项 | 值 |
|---|---|
| 抓取时间 | 2026-10-04 |
| 上游 | `https://copilot.tencent.com` |
| 方式 | **直连**（绕过 wild-work），完整还原请求头 |
| 凭据 | **未写入任何 fixture** |
| 原始完整模型目录 | `data/upstream/models-workbuddycn-2026-10-04.json`（31 模型） |

**已知局限**：
- `billing-daily-checkin.json` 只有**已签到**场景（当天已签过），
  **真实签到成功响应待补**（次日首次签到时抓）
- 其余 27 个模型的档位真实性**未逐个实测**（只验证了 3 个代表模型）

---

## 九、复现命令

抓取脚本参考（PowerShell，需从 wild-work 借用凭据）：

```powershell
# 必须用 HttpClient，不能用 HttpWebRequest
# （.NET 禁止用 Headers.Set 设置 User-Agent / Referer）
Add-Type -AssemblyName System.Net.Http
$handler = New-Object System.Net.Http.HttpClientHandler
$handler.UseProxy = $false
$client = New-Object System.Net.Http.HttpClient($handler)
# ... 见 direct-effort-test.ps1 的完整实现
```

**关键请求头**（缺一不可）：

```
Authorization: Bearer <token>        X-User-Id: <uid>
X-Machine-ID: sha256("wb2a:machine:"+uid)[0:36]
X-Session-ID: sha256("wb2a:session:"+uid)[0:36]
User-Agent: CLI/2.63.2 CodeBuddy/2.63.2
X-IDE-Name/Type: WorkBuddy            X-Product: WorkBuddy
X-CodeBuddy-Request: 1                Origin/Referer: https://www.codebuddy.cn
X-Conversation-Request-ID / X-Conversation-Message-ID
X-Request-ID / X-Root-Request-ID / X-B3-*
```
