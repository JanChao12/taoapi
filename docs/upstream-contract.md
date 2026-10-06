# WorkBuddy 上游契约（直连实测）

> **本文所有内容均通过直连上游取得并验证。**
> 主体验证于 2026-10-04；**登录与续期部分于 2026-10-05 实测补全**（见 1.1/1.3）。
> ⚠️ **不要信任中间层**（如 wild-work）：实测证明它注入额外内容，
> 使 reasoning 测量值偏差 **18 倍**，且其模型目录被裁剪（31 → 17）。

---

## 一、端点总览

| 用途 | 方法 | 端点 |
|---|---|---|
| **模型目录** | GET | `https://copilot.tencent.com/v2/enterprises/personal/models` |
| **对话** | POST | `https://copilot.tencent.com/v2/chat/completions` |
| **额度查询** | **POST** | `https://www.codebuddy.cn/v2/billing/meter/get-user-resource` |
| **每日签到** | POST | `https://www.codebuddy.cn/v2/billing/meter/daily-checkin` |
| **Token 刷新** | POST | `https://copilot.tencent.com/v2/plugin/auth/token/refresh` |
| **登录身份查询** | GET | `https://www.codebuddy.cn/v2/plugin/login/account` |

> ⚠️ **额度查询是 POST 不是 GET**，且**必须带 body**，否则返回 **404**。

---

## 二、🔴 契约类代码的教训（2026-10-05，**改这类代码前必读**）

登录功能实现期间累计踩到 **4 个缺陷**，其中 **2 个是"契约假设错误"**。
它们的共同形态值得单独写下来：

| # | 缺陷 | 类型 |
|---|---|---|
| A | CDP 事件回调内同步调 `Call` ⇒ 与读循环自死锁 | 并发逻辑 |
| B | `SetEventHandler` 在 `Network.enable` 之后注册 | 时序逻辑 |
| **C** | **`response.fromDiskCache` 声明成 `string`，CDP 实际下发 `bool`** | **契约类型** |
| **D** | **凭据响应结构搞错：以为是顶层，实际嵌套在 `data` 里** | **契约结构** |

⚠️ **A/B 是逻辑缺陷，C/D 是契约假设错误 —— 不要混为一谈**（Codex 第 33 轮纠正）。

### C/D 为什么能长期潜伏

1. **早期探针只记了"字段名"，没记类型与嵌套层级**
   ⇒ 文档写了"凭据来自响应体"，但结构是错的。
2. **测试用自己造的假数据**，与实现共享同一个错假设
   ⇒ **测试只是在验证自己的想象**，从来没测出问题。
3. **没有任何真实端到端验证**，直到真实登录才暴露。

### 由此确立的纪律（**后续必须遵守**）

- **契约类代码的测试数据必须来自实测样本**（脱敏后），不能来自实现者假设
- **实测样本要记全**：方法、请求头、**完整嵌套层级**、**每个字段的类型**
- 失败路径不能只覆盖"成功样本"：还要覆盖
  **缺字段 / 类型不符 / 业务码非 0 / 信封形态不符**（见 `login/cdp_test.go`）
- **类型不符是真实发生过的失败模式**，必须有断言守着

---

## 三、⭐ 登录与续期契约（2026-10-05 真实登录实测）

> 本节内容由**真实登录 + 真实续期**取得（非推断）。
> 详细背景见 `codex-consult/33c-dsh-第33轮-五步闭环全通过.md`。

### 凭据下发端点

```
POST https://www.codebuddy.cn/console/login/enterprise?state=<我们的UUID>
  → 200
  {
    "code": 0, "msg": "OK", "requestId": "<uuid>",
    "data": {                          ← 🔴 凭据嵌套在 data 里！
      "accessToken":  "<JWT RS256, 1333 字符>",
      "refreshToken": "<JWT HS512,  698 字符>",
      "expiresIn": …, "refreshExpiresIn": …, "tokenType": "Bearer"
    }
  }
```

⚠️ **`/console/login/enterprise` 是实测的站点接口**，
**不能称为标准 OIDC token endpoint** —— 它可能随上游变更。

⚠️ 方法实测为 **`POST`**（早期文档记的是 `GET`，已更正）。

### Token 续期端点（**2026-10-05 首次实测**）

```
POST https://copilot.tencent.com/v2/plugin/auth/token/refresh
  Header: X-Refresh-Token: <refreshToken>       ← 实测的认证方式
  → 200
  {
    "code": 0, "msg": "OK", "requestId": "<uuid>",
    "data": {
      "accessToken": …, "refreshToken": …,
      "expiresIn": …, "refreshExpiresIn": …,
      "notBeforePolicy": …, "scope": …, "sessionState": …, "tokenType": …
    }
  }
```

| 项 | 实测结果 |
|---|---|
| 方法 | `POST` |
| 认证 | **`X-Refresh-Token` 头** |
| refresh 是否轮换 | **本次两次请求未轮换**（返回同一个） |
| 返回的 access token | **可用**（用它查额度成功） |

⚠️ **限制（必须如实告知，不得夸大）**：

1. **在 access token 未过期时**，两次 refresh 请求业务成功，
   返回相同且可用的凭据；**未观察到 token 轮换或有效期延长**。
   **到期后的续期能力尚未验证。**
2. **仅验证并实现当前成功的请求形态；其他形态未测试，不承诺兼容。**
3. 样本极小（一个账号、两次请求、同一天）：
   **跨天 / 到期 / 闲置超时 / 服务端撤销**均**未验证**。

⚠️ **`refreshToken` 未轮换只是本次两次请求的观察**，
**实现不得依赖它** —— 必须支持未来轮换（收到新值就原子替换）。

### 身份查询端点

```
GET https://www.codebuddy.cn/v2/plugin/login/account
  Header: Authorization: Bearer <accessToken>
  → 200 {"code":0,"data":{"uid":"…","nickname":"…","phoneNumber":"…",…}}
```

用途：网页登录拿到的凭据**只有 token、没有 uid**，
而 uid 是本项目账号主键，落盘前必须靠它补全。

### 3.1 ⭐ 登录端点（2026-10-05 实测补充，此前**未知**）

> 本节是新调研的结果。**此前本文档只覆盖"对话/签到/额度/刷新"四个能力，
> 登录流程从未被调研过** —— 所以"上游没有登录端点"曾是一个**错误结论**，
> 正确说法是"我们没测过"。以下是实测。

**存在网页登录页**（真实可达，浏览器 UA）：

```
GET https://www.codebuddy.cn/login?platform=CLI&state=<uuid>
  → 301 → /login/?platform=CLI&state=<uuid>
  → 200，加载 SPA（JS bundle 在 download.codebuddy.cn/web/login/<build>/assets/）
```

关键证据（登录页 JS bundle 里的文案，中英双语）：

| 键 | 值 |
|---|---|
| `returnToCli` | **"请返回 CLI 继续"** / "Return to CLI to continue" |
| `loginSuccessful` | 登录成功 |
| `loginFailed` | 登录失败 |

**"请返回 CLI 继续"决定了整个流程的性质**：登录**不通过 redirect 回调**把凭据交给客户端，
而是**服务端按 `state` 记住凭据，客户端轮询取回**（即 device-code / 轮询模式）。

**配套的服务端端点**（从 wild-work 二进制中提取的字符串常量）：

```
/v2/plugin/login/account?state=<uuid>     ← 轮询取回凭据
/v2/plugin/auth/state?platform=CLI        ← 授权状态
/v2/plugin/auth/token/refresh             ← 已在用
```

**实测结果**（裸请求，不带签名头）：

| 请求 | 结果 |
|---|---|
| `GET /login?platform=CLI&state=<uuid>`（浏览器 UA） | ✅ 301 → 200，页面正常 |
| `GET /v2/plugin/login/account?state=<uuid>` | ❌ **401**（APISIX 网关拒绝） |
| `GET /v2/plugin/auth/state?platform=CLI` | ❌ 404（路径可能需签名或参数不同） |

**401 是网关层拒绝，说明该端点需要一类我们尚未掌握的签名头。**
从 wild-work 二进制里可见它使用的头族（**全部为字符串常量，非凭据**）：

```
code_verifier · login_version · login_channel · login_trace_id
x_device_type · x_app_version · X-Device-Id · X-Timestamp · X-Nonce · X-Sign
```

**即：它同时用了 PKCE（`code_verifier`）+ 设备标识 + 时间戳 + 签名（`X-Sign`）。**

### 3.2 ⭐ 完整登录流程（2026-10-05 实测确认，**不需要逆向签名**）

> 上一版本节写的是"需要逆向 `X-Sign`，结论待定"。**那是错的** ——
> 401 的真实原因是**没带 `Authorization`**，不是缺私有签名。
> 补上 Bearer 后端点直接可用。以下是更正后的完整实测。

**流程（device-code / 轮询式，三步）**：

```
① 本机生成 state（uuid），把用户导向登录页
   GET https://www.codebuddy.cn/login/?platform=CLI&state=<uuid>
       → 301 → /login/?platform=CLI&state=<uuid> → 200（SPA）
   ⚠️ 必须走 ?platform=CLI —— 页面 JS 里有专门判断：
      非 CLI 的平台会走另一套（`platform=open` 是开放平台登录页）

② 用户在浏览器完成登录
   页面文案 `returnToCli: "请返回 CLI 继续"` 说明凭据**不靠 URL 回调**，
   而是**服务端按 state 记住**

③ 客户端轮询取回凭据
   GET https://www.codebuddy.cn/v2/plugin/login/account?state=<uuid>
       Header: Authorization: Bearer <已有token>   ← 必需，否则网关 401
```

**关键实测数据**（本机 3 个真实账号之一，只读）：

| 请求 | 结果 |
|---|---|
| 登录页 `?platform=CLI&state=…` | ✅ 200 |
| `GET /v2/plugin/login/account?state=<新state>`（带 Bearer） | `{"code":12151,"msg":"12151:not choose login account"}` |
| `GET /v2/plugin/login/account`（**不带 state**，带 Bearer） | `{"code":0,"msg":"OK","data":{"uid":…,"nickname":…,"uin":…,"type":"personal","lastLogin":true}}` |
| 不带 `Authorization` 的任何形态（含加 `X-Device-Id`/`X-Timestamp`/`X-Nonce`/`X-Sign`） | ❌ **一律 401**（APISIX 网关，`www-authenticate: Bearer realm="copilot"`） |

**语义解读**：
- **不带 state** = "查我这个 token 是谁" → 返回本账号信息；
- **带 state** = "认领/选择那个待完成的登录会话" → 尚无会话时返回 `12151 not choose login account`。

**`platform=CLI` 是唯一入口**：登录页 JS 里的判断
`/^\/login\/?$/.test(pathname) && platform === "open"` 走开放平台分支，
其余走标准登录 —— 所以必须严格按 `?platform=CLI&state=<uuid>` 这个形态。

**结论：**
| 项 | 状态 |
|---|---|
| 登录页 | ✅ 存在且可达 |
| 完整协议 | ✅ **已掌握**（不需要 `X-Sign` 逆向） |
| 认证方式 | **标准 `Authorization: Bearer`** + 浏览器侧登录 |
| 未验证项 | ⚠️ **"用户在浏览器登录后，轮询是否真能取回新账号凭据"这一步没有端到端跑过**（需要真实登录一次） |

> ⚠️ **仍未验证的关键一步**：上面第 ③ 步取回的是"已有 token 对应账号"。
> **"浏览器登录一个全新账号后，第 ③ 步能否拿到那个新账号的凭据"尚未实测** ——
> 这需要真实走一次完整登录。本项目的取回时机/返回结构属**推断**，不是实测。

---

## 四、🔴 安全红线

| 规则 | 说明 |
|---|---|
| **chat 请求绝不能带 `X-Refresh-Token`** | refresh token 只能出现在 refresh 端点 |
| refresh token 不得进日志/事件/Git | 脱敏必须在写入前完成 |
| 本服务只监听 `127.0.0.1` | 不接受改成对外网卡 |

**这三条要写成测试断言，不靠代码审查记忆。**

---

## 五、请求头（chat）

### 5.1 必需头

```
Content-Type: application/json
Accept: application/json, text/plain, */*
X-Requested-With: XMLHttpRequest
X-CodeBuddy-Request: 1
Origin: https://www.codebuddy.cn
Referer: https://www.codebuddy.cn/
User-Agent: CLI/2.63.2 CodeBuddy/2.63.2
Accept-Language: zh-CN
Authorization: Bearer <accessToken>
X-User-Id: <uid>
```

### 7.2 缺省字段的约定（与官方 CLI 一致）

**没有值的字段不是省略，而是发 `X-No-*` 标记**：

```
X-No-Enterprise-Id: 1      （无企业 ID 时）
X-No-Department-Info: 1    （无 domain 时）
X-No-Authorization: 1      （无 token 时）
X-No-User-Id: 1            （无 uid 时）
```

### 6.3 用量归属头（伪装成桌面端）

```
X-Agent-Purpose: conversation
X-IDE-Name: WorkBuddy
X-IDE-Type: WorkBuddy
X-IDE-Version: 5.5.4
X-Product: WorkBuddy
```

#### ⭐ 版本号是否被校验：**实测不校验**（2026-10-05 证伪实验）

评审曾担心："这两个版本号硬编码，腾讯升级 CLI 后旧版被拒会让项目整体失效。"
**我做了证伪实验，结论是当前不校验** ——

| 实验 | `User-Agent` | `X-IDE-Version` | 结果 |
|---|---|---|---|
| A 基线 | `CLI/2.63.2 CodeBuddy/2.63.2` | `5.5.4` | ✅ 200 |
| B 极旧 | `CLI/0.0.1 CodeBuddy/0.0.1` | `0.0.1` | ✅ 200 |
| C 极新 | `CLI/99.99.99 CodeBuddy/99.99.99` | `99.99.99` | ✅ 200 |
| D 完全不像客户端 | `curl/8.0` | `not-a-version` | ✅ 200 |

D 组之后 `/v1/models` 也正常（31 个模型）。
→ **上游把这两个值当"用量归属标识"，不是鉴权门槛。**

**但"现在不校验"≠"将来不校验"**，所以保留应急通道（成本为零）：
`WBAPI_CLIENT_UA` / `WBAPI_IDE_VERSION` 两个环境变量可覆盖，
**出问题改环境变量即可，不必重新编译**（用户手上只有 exe）。
当前生效值可在 `/status` 的 `upstream.identity` 看到（含是否被覆盖的标记）。

**未验证项（诚实标注）**：本实验只测了"值不同会不会被拒"，
**没有**测"上游是否按版本号做用量归属统计" ——
若它影响计费展示，改这些值可能让用量记录归到别的客户端名下。

### 5.4 设备与链路追踪头

```
X-Machine-ID: sha256("wb2a:machine:" + uid)  → 取前 36 个 hex 字符
X-Session-ID: sha256("wb2a:session:" + uid)  → 取前 36 个 hex 字符

X-Conversation-Request-ID: <随机 16 字节 hex>
X-Conversation-Message-ID: <随机 16 字节 hex>
X-Request-ID: <同 Message-ID>
X-Root-Request-ID: <同 Conversation-Request-ID>
X-B3-TraceId: <同 Message-ID>
X-B3-SpanId: <Message-ID 前 16 字符>
X-B3-Sampled: 1
```

> ⚠️ **派生规则是实测还原的，不是官方文档。**
> 实现时要**允许从配置覆写**，以便上游变更时不必改代码。

---

## 五乙、⭐ 强制 `stream: true`

**发 `stream: false` → HTTP 400。**

后果：**非流式响应必须由我们自己聚合**。
- 流式路径：透传（逐事件转换 + `Flush`）
- 非流式路径：**消费同一条上游流**，聚合后返回一个 Chat JSON

**绝不能向上游发 `stream: false` 再等 JSON。**

---

## 六、流式响应形状

### 6.1 事件顺序

```
data: {"choices":[{"delta":{"role":"assistant", ...}}]}          ← role
data: {"choices":[{"delta":{"reasoning_content":"我们", ...}}]}   ← 思考（很多）
   ...（实测 41 个）
data: {"choices":[{"delta":{"content":"可用", ...}}]}             ← 正文（很少）
data: {"choices":[{"delta":{...},"finish_reason":"stop"}],"usage":{...}}  ← 末帧带 usage
data: [DONE]
```

### 7.2 ⚠️ delta 的字段永远全部存在，空值用空串/null/数组

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

### 6.3 ⚠️ 思考与正文不是分开的事件

实测 45 个 data 行中：**41 个只带 `reasoning_content`，1 个只带 `content`，0 个同时带**。
→ 解析器要**分别累加**，不要假设"先思考完再正文"。

### 6.4 usage（在末帧，需 `stream_options.include_usage`）

```json
"usage": {
  "prompt_tokens": 35,
  "completion_tokens": 43,
  "total_tokens": 78,
  "completion_tokens_details": { "reasoning_tokens": 41 },
  "completion_thinking_tokens": 41,
  "credit": 0,
  "prompt_cache_hit_tokens": 0,
  "prompt_cache_miss_tokens": 35
}
```

**`credit` = 本次消耗额度，`completion_thinking_tokens` = 思考 token。**

### 6.5 响应头（实测）

```
Content-Type: text/event-stream
Cache-Control: no-cache
Transfer-Encoding: chunked
Server: APISIX/3.9.1
Traceid / X-Request-Id / X-User-Id / EO-LOG-UUID
```

---

## 七、模型目录响应

### 7.1 关键字段

```json
{
  "code": 0, "msg": "OK",
  "data": {
    "models": [
      {
        "id": "deepseek-v4.1-flash",
        "name": "Deepseek-V4.1-Flash",
        "maxInputTokens": 1000000,
        "maxOutputTokens": 128000,
        "maxAllowedSize": 1000000,
        "contextWindow": { "defaultLength": 300000, "supportedLengths": [300000, 600000, 1000000] },
        "reasoning": { "effort": "high", "summary": "auto" },
        "supportsImages": true,
        "supportsToolCall": true,
        "supportsReasoning": true,
        "credits": "x0.11 credits",
        "vendor": "f",
        "temperature": 1
      }
    ]
  }
}
```

### 7.2 ⚠️ 上下文默认不给满

多数模型 `defaultLength` = **300,000**，标称 1M。
**想用满必须显式传 `context_length`**（可选 300k / 600k / 1M）。

### 7.3 模型数量

上游实际返回 **31 个**（中间层只显示 17 个）。
**不要硬编码数量**，每次以实际响应为准。

---

## 八、⭐ reasoning：逐模型不同，必须实测

上游 `reasoning` 字段有两种形态：

```json
// 形态 A：声明多档（space-bunny / glm-5.3 / kimi-k2.8-preview / hy3-x）
"reasoning": {
  "supportedEfforts": ["low","medium","high","xhigh","max"],
  "defaultEffort": "max",
  "canDisableThinking": false,
  "summary": "auto"
}

// 形态 B：只给固定档（deepseek 系 / kimi 多数 / minimax 等）
"reasoning": { "effort": "high", "summary": "auto" }
```

### 实测三模式（直连，n=6/档）

| 模型 | 模式 | 证据 |
|---|---|---|
| `space-bunny` | **knob（真旋钮）** | 均值 44→53.7→61.7→79.7→83.7 严格单调；low 上限 47 < xhigh 下限 55 |
| `glm-5.3` | **knob-low-flat** | low 中位 78 / high 178 / **max 3232**（跳 18 倍） |
| `deepseek-v4.1-flash` | **switch（纯开关）** | 各档 178–257 随机排列；**不传 = 0** |

### 🔴 最重要的一条

> **对 deepseek 系，不传 `reasoning_effort` = 完全不思考。**
> 上游声明的 `effort: "high"` **不是服务端默认行为**，是给客户端 UI 的默认值。
> **服务端必须显式注入默认档位。**

**测试铁证**：`fixtures/sse-deepseek-v4.1-flash-high.txt`（45 行 data，41 思考分片）
vs `fixtures/sse-deepseek-noeffort.txt`（4 行 data，0 思考分片）。

### 合法档位白名单（实测被接受的）

```
minimal · low · medium · high · xhigh · max · ultra · none
```

被拒绝（400）：`x-high`、`maximum`、`extreme`、`auto`、`highest`、`veryhigh`

---

## 九、其他实测结论

| 项 | 结论 |
|---|---|
| **`max_tokens` 上限** | 上游**不校验**（发 100 万都接受）；真实上限见各模型 `maxOutputTokens` |
| **`max_completion_tokens` 别名** | 上游**只认 `max_tokens`**，别名会被忽略并回落默认值（32000） |
| **`developer` role** | 上游白名单不含它，会触发内容过滤误杀 → **必须归一为 `system`** |
| **`tool_choice`** | 上游要求是 **string**，传对象会 400（code=11101） |
| **`reasoning_content`** | OpenAI 规范外字段，**必须原样透传**，不能过滤 |

---

## 十、额度查询请求体

```json
{
  "PageNumber": 1,
  "PageSize": 100,
  "ProductCode": "p_tcaca",
  "Status": [0, 3],
  "PackageEndTimeRangeBegin": "<当前时间 yyyy-MM-dd HH:mm:ss>",
  "PackageEndTimeRangeEnd": "<当前时间 + 101 年>"
}
```

**响应关键路径**：`data.Response.Data.Accounts[]`
→ 含 `CapacityRemain` / `CapacityUsed` / `CapacitySize` / `CycleCapacityRemain` / `ExpiredTime`
→ `TotalDosage` = 总剩余额度

---

## 十一、签到

```
POST https://www.codebuddy.cn/v2/billing/meter/daily-checkin
Body: {}
```

| 情况 | 响应 | 处理 |
|---|---|---|
| 首次签到成功 | `code: 0` | 成功 |
| **今日已签到** | **HTTP 400 + 空 body** | **判定为 already_checked_in，不算失败、不进冷却** |
| 业务失败 | `code != 0` | 按 `msg` 分类 |

> ⚠️ 「已签到 = 400」是实测观察。真实成功响应待次日首次签到时补录 fixture。

---

## 十二、复现方式

**必须用 `HttpClient`（或 Go 的 `http.Client`），不能用 .NET `HttpWebRequest`**
—— 后者禁止用 `Headers.Set` 设置 `User-Agent` / `Referer`。

**凭据来源**（当前过渡期）：wild-work 的 `D:\tools\wild-work\auths\workbuddy-*.json`
- `auth.accessToken` → `Authorization: Bearer`
- `account.uid` → `X-User-Id`
- 其余身份头按 §3.4 派生

**⚠️ 本项目运行时不得依赖该目录** —— 见 `docs/credential-policy.md`（待写）。
