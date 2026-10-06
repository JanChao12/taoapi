# refresh 续期实验结论（2026-10-06 实测）

> 本文件是 `查看进度.ps1` 判定"refresh 续期实验"是否完成的依据。
> 内容全部来自**真实登录 + 真实续期**，非推断。
> 相关裁定见 `codex-consult/33-codex-第33轮-裁定.md`。

---

## 一、背景：动手前这个端点是**从未被调用过**的

接手时核实（不是推测）：

- `upstream.PathTokenRefresh` 存在、`Client.TokenRefreshURL()` 存在，
  但**没有任何生产代码调用它**（唯一引用是一个断言 URL 字符串的测试）
- ⇒ 续期端点的**方法 / 请求头 / 请求体 / 响应结构从未实测过**
- `failover.go` 注释写着「标 auth_expired 以便尝试 refresh」，
  而**那条 refresh 路径并不存在**（已于 2026-10-05 修正该注释）
- `auth.Account.TokenExpiresAt` 只被存取、**从不参与任何判断**

---

## 二、实测获得的契约（**全项目首次**）

```
POST https://copilot.tencent.com/v2/plugin/auth/token/refresh
  Header: X-Refresh-Token: <refreshToken>
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
| 认证方式 | **`X-Refresh-Token` 头** |
| 响应结构 | 与登录端点同形态（`{code,msg,requestId,data{…}}`） |
| 返回的 access token | **可用**（用它查额度成功） |

⚠️ 我按可能性排序尝试，**第一种形态就命中**，于是**立即停止**，
未再试其他形态 —— 这是有意为之，守住 Codex 第 28 轮关于
**token family 撤销**的警告。

---

## 三、Codex 五步闭环的执行结果

| 步骤 | 要求 | 结果 |
|---|---|---|
| ① | 新建隔离 profile，不载旧凭据 | ✅ 全新 `os.MkdirTemp` profile |
| ② | **生产函数**捕获本次登录数据 | ✅ 117 秒捕获成功 |
| ③ | 检查必需字段及业务成功码 | ✅ 字段齐 + `code=0` |
| ④ | 用凭据查额度、**核对账号** | ✅ `uid=cccccccc` = `13800000008`，2xxx 分 |
| ⑤ | refresh + **重启后恢复** | ✅ 两次续期均成功，第二次是**全新进程** |

### ④ 的三条门槛（Codex 点名要证明的）

- 是**本次**凭据、无旧 token 回退 ✅
- 用它能成功查账号/额度 ✅
- **账号与刚登录的一致** ✅

⚠️ 顺带用**实测**回答了 Codex 第 29 轮的悬置问题：
**页面最后那个 `/console/auth/login` 400 没有阻止已验证的 API 使用。**
（不再是推断。）

### ⑤ 的两次续期

**第一次**（捕获后立刻）：

```
HTTP 200
accessToken : 变化=false  长度 1333 → 1333
refreshToken: 变化=false  长度  698 → 698
refresh token 未轮换（服务端仍返回同一个）
✅ 续期得到的 access token 可用（再次查到 2xxx 分）
```

**第二次**（**全新进程**，模拟重启 wbapi，从加密落盘文件读取）：

```
沿用 2026-10-06T00:38:05 捕获的 refresh token
HTTP 200
refresh token 未轮换
✅ 续期得到的 access token 可用
✅ 新凭据已原子保存
```

⇒ **落盘与恢复路径已验证。**

---

## 四、🔴 限制（Codex 给定措辞，**必须如实转述**）

### 限制 1 —— 到期后的续期**未验证**

> 在 access token 未过期时，两次 refresh 请求业务成功，返回相同且可用的凭据；
> **未观察到 token 轮换或有效期延长**。**到期后的续期能力尚未验证。**

⚠️ "因为还有 37 天，所以原样返回" —— 这是**推断**，不是服务端规则。
⚠️ "refresh 不轮换" 只能限定为「**本次两次请求**未轮换」。

### 限制 2 —— 只验证了一种形态

> **仅验证并实现当前成功的请求形态；其他形态未测试，不承诺兼容。**

⚠️ 支持一种已验证形态**本身不是缺陷**（Codex 明确），
也不需要为探索其他形态继续提交真实 token。

### 限制 3 —— 样本极小

一个账号、两次续期、同一天、同网络环境。
**跨天 / 到期 / 闲置超时 / 服务端撤销 均未验证。**

⚠️ 尤其：Codex 第 28 轮问过的 **"offline token 是否有闲置超时"**
**仍然完全未验证**（可能短于 40 天）。

---

## 五、🔴 实现侧的重要约束（Codex 要求，未来接入时必守）

`refreshToken` **未轮换只是本次两次请求的观察**，
**实现不得依赖它** —— 必须支持未来轮换（收到新值就原子替换）。

**生产自动续期目前尚未接入**（单独立项）。接入时必须覆盖：

- 同账号**并发续期去重**
- 返回新 token 时的**原子替换**
- **失败分类**与**有限重试**
- ⚠️ **不能把所有 401/403 都当作过期**
- ⚠️ **不能在已输出流式内容后重放对话**

---

## 六、实验工具与安全

工具：`tools/refreshprobe/`（`-step capture|use|refresh|cleanup`）

- 凭据用与账号库**同一个 DPAPI 加密器**暂存，**绝不落明文**
- 输出只含"是否变化"、长度、有效期这类信息，**不含 token**
- 实验结束已 `-step cleanup` 删除暂存文件
- **未伪造 token、未重放旧 refresh token 探测失效**
