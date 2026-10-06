# 设置页接线片段（参考，给主智能体接线用）

> 本文件**不是**被 `go:embed` 加载的资源（`panel.go` 的 embed 模式是 `panel/*`，
> 但只有 `index.html` 会被浏览器请求）。它只是一份**可复制的 HTML 片段**，
> 用来核对 `settings.js` / `settings.css` 期望的 DOM 结构。
>
> 如果不想在仓库里留第三份文件，接线完成后删掉本文件即可 ——
> `settings.js` 与 `settings.css` 不依赖它存在。

---

## 1. 侧边栏第 4 项（插在「API 接入」之后）

在 `index.html` 的 `<nav class="side-nav">` 里，`api` 那一行**之后**追加：

```html
      <button type="button" class="side-item" data-page="settings"><span class="ico">⚙</span><span>设置</span></button>
```

## 2. 设置页 section（插在 `#page-api` 那一节之后、`</main>` 之前）

```html
    <!-- ══════════ 页：设置 ══════════ -->
    <section class="page hidden" id="page-settings">

      <!-- toast 容器：settings.js 只改它的 textContent -->
      <div class="st-toast st-hidden" id="st-toast" role="status" aria-live="polite"></div>

      <div class="tip st-tip st-tip-plain st-hidden" id="st-load-err"></div>
      <div class="tip st-tip st-tip-danger st-hidden" id="st-corrupt">
        ⚠️ 配置文件损坏：已回退到默认值，并把原文件重命名为 <code>config.json.corrupt-&lt;时间戳&gt;</code>。请检查后重新设置。
      </div>

      <!-- 顶层一览：把四区的关键状态压成一行，扫一眼就够 -->
      <section class="panel">
        <h2>当前设置一览</h2>
        <div class="st-line">
          <span class="st-toggle-state" id="st-sum-api-key">接口密钥：—</span>
          <span class="st-toggle-state" id="st-sum-port">监听端口：—</span>
          <span class="st-toggle-state" id="st-sum-auto-checkin">自动签到当前：—</span>
          <span class="st-toggle-state" id="st-sum-auto-start">开机自启当前：—</span>
        </div>
      </section>

      <!-- ══ 区 1：密钥 ══ -->
      <section class="panel" id="st-sec-key">
        <div class="panel-head">
          <h2>密钥</h2>
          <div class="st-head-tags"><span class="st-chip st-chip-now">立即生效</span></div>
        </div>

        <div class="st-row">
          <div class="st-row-label">当前密钥</div>
          <div class="st-row-body">
            <span class="st-value" id="st-key-hint">—</span>
            <span class="hint-text st-hint-block">出于安全，这里只显示前 4 位；完整密钥不会回传到面板。</span>

            <!-- 未设密钥时的醒目警示（settings.js 用 st-hidden 控制显隐） -->
            <div class="tip st-tip st-tip-danger st-hidden" id="st-key-warn">
              ⚠️ 未设密钥 —— 本机任何程序都可调用此接口
            </div>
          </div>
        </div>

        <div class="st-row">
          <div class="st-row-label">操作</div>
          <div class="st-row-body">
            <div class="st-line">
              <button type="button" class="btn" id="btn-st-key-change">更换</button>
              <button type="button" class="btn st-hidden" id="btn-st-key-clear">清空</button>
              <span class="hint-text">清空 = 不再校验 Authorization，需要二次确认</span>
            </div>

            <div class="st-inline-form st-hidden" id="st-key-form">
              <span class="st-label" id="st-key-form-label">输入新密钥</span>
              <input type="password" class="st-input st-input-key" id="st-key-input" autocomplete="new-password" spellcheck="false">
              <button type="button" class="btn btn-primary" id="btn-st-key-save">保存</button>
              <button type="button" class="btn st-hidden" id="btn-st-key-cancel">取消</button>
            </div>
            <span class="st-field-err" id="st-key-err"></span>
            <span class="hint-text st-hint-block">建议直接用首启随机生成的 32 位十六进制密钥；更换后旧密钥立即失效。</span>
          </div>
        </div>
      </section>

      <!-- ══ 区 2：网络 ══ -->
      <section class="panel" id="st-sec-net">
        <div class="panel-head">
          <h2>网络</h2>
          <div class="st-head-tags"><span class="st-chip st-chip-later">需重启生效</span></div>
        </div>

        <div class="st-row">
          <div class="st-row-label">监听端口</div>
          <div class="st-row-body">
            <div class="st-line">
              <input type="text" inputmode="numeric" class="st-input st-input-port" id="st-port" maxlength="5" placeholder="8787">
              <button type="button" class="btn" id="btn-st-port-save">保存端口</button>
              <span class="hint-text">范围 1024–65535（&lt;1024 需要管理员权限）</span>
            </div>
            <span class="st-field-err" id="st-port-err"></span>

            <div class="st-line st-collapse st-hidden" id="st-port-msg">
              <span class="tip st-tip st-tip-warn" style="margin:0;">端口已保存 —— <b>需重启生效</b></span>
              <button type="button" class="btn btn-primary" id="btn-st-restart">立即重启</button>
              <span class="hint-text" id="st-restart-msg"></span>
            </div>
          </div>
        </div>

        <div class="st-row">
          <div class="st-row-label">实际监听</div>
          <div class="st-row-body">
            <span class="st-value" id="st-listen">—</span>
            <span class="hint-text st-hint-block">只监听 127.0.0.1（不可配置）—— 不允许暴露到外网。</span>
          </div>
        </div>

        <div class="st-row">
          <div class="st-row-label">重启状态</div>
          <div class="st-row-body">
            <span class="st-value-plain" id="st-restart-state">无需重启</span>
            <span class="hint-text st-hint-block">除端口外，密钥 / 开关 / 映射表都是热生效，改完即用。</span>
          </div>
        </div>
      </section>

      <!-- ══ 区 3：自动化 ══ -->
      <section class="panel" id="st-sec-auto">
        <div class="panel-head">
          <h2>自动化</h2>
          <div class="st-head-tags"><span class="st-chip st-chip-now">立即生效</span></div>
        </div>

        <div class="st-row">
          <div class="st-row-label">自动签到</div>
          <div class="st-row-body">
            <div class="st-line">
              <button type="button" class="st-toggle" id="st-auto-checkin" role="switch" aria-checked="true" aria-label="自动签到"></button>
              <span class="st-toggle-state" id="st-sum-auto-checkin">自动签到当前：—</span>
            </div>
            <span class="hint-text st-hint-block">
              默认<b>开启</b>。服务运行期间自动给未签到账号签到（按北京时间判定当天），关闭后立即停止后续调度。
            </span>
          </div>
        </div>

        <div class="st-row">
          <div class="st-row-label">运行状态</div>
          <div class="st-row-body">
            <div class="st-line">
              <span class="st-toggle-state">已启用/已停用：</span>
              <span class="st-value-plain" id="st-checkin-state">—</span>
              <span class="st-toggle-state" style="margin-left:12px;">上次触发：</span>
              <span class="st-value-plain" id="st-checkin-at">—</span>
            </div>
            <div class="st-line" style="margin-top:6px;">
              <span class="st-toggle-state">结果：</span>
              <span class="st-value-plain" id="st-checkin-result">—</span>
            </div>
            <span class="hint-text st-hint-block">
              无账号时显示「—」（不会执行、也不会发上游请求）。单账号的签到日期见「账号管理」页。
            </span>
          </div>
        </div>

        <div class="st-row">
          <div class="st-row-label">开机自启</div>
          <div class="st-row-body">
            <div class="st-line">
              <button type="button" class="st-toggle" id="st-auto-start" role="switch" aria-checked="false" aria-label="开机自启"></button>
              <span class="st-toggle-state" id="st-sum-auto-start">开机自启当前：—</span>
            </div>
            <span class="hint-text st-hint-block">
              默认<b>关闭</b>。开启后写入注册表 <code>HKCU\Software\Microsoft\Windows\CurrentVersion\Run</code>，登录后自动以 <code>serve</code> 启动。
            </span>
          </div>
        </div>
      </section>

      <!-- ══ 区 4：映射表 ══ -->
      <section class="panel" id="st-sec-alias">
        <div class="panel-head">
          <h2>映射表 <span class="count-chip" id="st-alias-count">共 0 条</span></h2>
          <div class="st-head-tags"><span class="st-chip st-chip-now">立即生效</span></div>
        </div>

        <div class="tip st-tip st-tip-plain">
          把长模型名映射成短别名，客户端就能用 <code>dsf</code> 代替 <code>workbuddy/deepseek-v4.1-flash</code>。
          <b>只解析一层</b>：别名的目标必须是真实模型，<b>不能指向另一个别名</b> —— 否则会形成循环。
          别名区分大小写，且不能包含「/」。
        </div>

        <div class="st-alias-wrap">
          <!-- 已生效（服务端当前状态） -->
          <div>
            <div class="st-sub-head">
              <span class="st-sub-title">当前生效的映射</span>
              <button type="button" class="btn btn-sm" id="btn-st-alias-add">新增映射</button>
            </div>

            <div class="st-inline-form st-hidden" id="st-alias-form">
              <input type="text" class="st-input st-input-alias" id="st-alias-name" placeholder="别名，如 dsf" maxlength="64" spellcheck="false">
              <span class="st-label">→</span>
              <input type="text" class="st-input st-input-target" id="st-alias-target" placeholder="目标模型，如 workbuddy/deepseek-v4.1-flash" list="st-model-options" spellcheck="false">
              <button type="button" class="btn btn-sm" id="btn-st-alias-save">加入草稿</button>
              <button type="button" class="btn btn-sm" id="btn-st-alias-cancel">取消</button>
            </div>
            <span class="st-field-err" id="st-alias-err"></span>

            <table class="tbl">
              <thead><tr><th class="st-td-alias">别名</th><th class="st-td-target">目标模型</th><th class="st-td-act">操作</th></tr></thead>
              <tbody id="st-alias-body"><tr><td colspan="3" class="empty">加载中…</td></tr></tbody>
            </table>
          </div>

          <!-- 草稿（本地待提交） -->
          <div class="st-draft">
            <div class="st-sub-head">
              <span class="st-sub-title">待提交的改动</span>
              <span class="hint-text" id="st-alias-dirty">无未保存改动</span>
            </div>
            <table class="tbl">
              <thead><tr><th class="st-td-state">状态</th><th class="st-td-alias">别名</th><th class="st-td-target">目标模型</th><th class="st-td-act">操作</th></tr></thead>
              <tbody id="st-alias-draft"><tr><td colspan="4" class="empty">还没有本地改动</td></tr></tbody>
            </table>
          </div>
        </div>

        <div class="st-foot-row">
          <span class="hint-text">「保存映射」会一次性替换整张表；刷新页面 = 放弃未保存的改动。</span>
          <button type="button" class="btn btn-primary" id="btn-st-alias-batch-save">保存映射</button>
        </div>

        <div class="st-collapse">
          <div class="st-sub-head">
            <span class="st-sub-title">当前可用模型（只读）</span>
            <span class="st-head-tags">
              <span class="hint-text" id="st-model-note"></span>
              <button type="button" class="btn btn-sm" id="btn-st-toggle-models">查看当前可用模型</button>
            </span>
          </div>
          <div class="st-collapse st-hidden" id="st-model-box">
            <table class="tbl">
              <thead><tr><th>模型 ID</th></tr></thead>
              <tbody id="st-model-body"></tbody>
            </table>
          </div>
        </div>

        <!-- 给「目标模型」输入框做候选提示：只列出真实模型 ID，
             这样正常操作下不会把目标写成另一个别名（那会形成循环） -->
        <datalist id="st-model-options"></datalist>
      </section>

    </section>
```

## 3. head 里加 CSS 链接（在 style.css 那行之后）

```html
<link rel="stylesheet" href="/panel/settings.css">
```

## 4. body 末尾加脚本

```html
<script src="/panel/settings.js"></script>
<script src="/panel/app.js"></script>
```

顺序**不重要**：`settings.js` 只做两件事 —— 定义 `window.initSettingsPage`、
在 `init()` 里自查 `#page-settings` 存在（不存在就安静退出）。
它不调用 app.js 的任何函数。放前放后都行。

## 5. app.js 需要加的接线（**共 4 处，逐行列出**）

### 5.1 页面注册表（约 766 行，`init()` 里）

```js
// 改前
pages = { accounts: loadAccts, stats: loadStats, api: loadApi };
// 改后
pages = { accounts: loadAccts, stats: loadStats, api: loadApi,
          settings: window.initSettingsPage ? settingsPageLoader : loadAccts };
```

**注意**：`pages[name]` 会被 `switchPage()` 与 `tick()` 当函数调用。
`initSettingsPage` 是「初始化」，不能当刷新函数用（重复调用会重复挂监听器）。
所以要么单独包一层，要么（更简单）**见 5.4 的推荐做法**。

### 5.2 页面隐藏清单（约 86 行，`switchPage()` 里）

```js
// 改前
var ids = ['accounts', 'stats', 'api'];
// 改后
var ids = ['accounts', 'stats', 'api', 'settings'];
```

**漏了这行 = 切到设置页后旧页面不隐藏**（三页叠在一起）。

### 5.3 `currentPage` 注释（约 73 行，仅注释，可选）

```js
var currentPage = 'accounts'; // accounts | stats | api | settings
```

### 5.4 轮询（**推荐：不要加 `settings`**）

`POLLED_PAGES`（约 737 行）保持原样即可：

```js
var POLLED_PAGES = { accounts: true, stats: true };
```

**理由**：`settings.js` 自带定时器 —— 只在设置页可见、且用户没在本页聚焦输入时
才刷新（`tick()` 里判断 `activePage() === 'settings'` + `document.activeElement`
是否落在 `#page-settings` 内）。把 `settings` 加进 `POLLED_PAGES` 会有两个后果：
① 两套定时器重复请求；② 用户正在输端口/别名时被轮询覆盖输入框。

### 5.5 init() 里加一行（约 774 行，`initAcctBulk()` 之后）

```js
if (typeof window.initSettingsPage === 'function') window.initSettingsPage();
```

**这行是必须的**。原因：`pages[name]` 只有在用户点侧边栏时才会被调用，
而 `initSettingsPage` 内部要挂事件监听器并做首次 `GET /api/settings` ——
**必须无条件执行一次**，不能等用户点。所以它是「初始化」而不是「刷新」。

### 5.6 接线后的完整 diff 形状（供 review）

```diff
-  var currentPage = 'accounts'; // accounts | stats | api
+  var currentPage = 'accounts'; // accounts | stats | api | settings
-    var ids = ['accounts', 'stats', 'api'];
+    var ids = ['accounts', 'stats', 'api', 'settings'];
     initAcctCards();
     initAcctBulk();
+    if (typeof window.initSettingsPage === 'function') window.initSettingsPage();
     initVisibility();
```

**只有 3 行有实质改动**，`pages` 注册表与 `POLLED_PAGES` 都不用动
（设置页走自己的初始化 + 自己的定时器）。

---

## 6. 自测

`settings.selftest.js` 是离线自测脚本（本机 node 可跑，**不参与 embed、不影响发布**）：

```powershell
& "<DSH 运行时目录>\node\bin\node.exe" `
  "internal\app\panel\settings.selftest.js"
```

它从本文件里抽出 HTML 片段，用手写 DOM 桩 + 假 fetch 跑 `settings.js` 的真实逻辑，
断言 60+ 条（密钥语义、端口校验、开关、映射表草稿、XSS 转义、缺字段容错…）。
**发版前删掉本文件与 `settings.selftest.js` 即可**，两者都不会被 `go:embed` 引用。
