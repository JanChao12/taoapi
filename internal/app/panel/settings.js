/* wbapi 管理面板「设置」页 —— 手写，零依赖，零构建
 *
 * 契约：docs/第9轮-接口契约-冻结.md §3（设置 API）与 §8（本页规格）。
 *
 *	GET   /api/settings          读取设置（绝不返回明文密钥）
 *	PATCH /api/settings          修改设置（字段缺失 = 未修改）
 *	POST  /api/settings/restart  一键重启（端口变更后需要）
 *
 * 🔴 本文件与 app.js 是两个 IIFE，彼此【不共享】作用域（app.js 里的 esc /
 *    fetchJSON / openModal / closeModal 都是私有函数）。因此这里自带一份
 *    等价的安全版本，绝不依赖 app.js 的内部实现 —— 契约把本页交给独立开发者，
 *    谁也不能假设对方的作用域。唯一复用的是 overlay 的【DOM 契约】：
 *    #overlay / #modal-body / .overlay.open / .modal-sm，由 app.js 的
 *    initModal() 统一接管关闭逻辑（点遮罩 / × / Esc）。
 *
 * 载入顺序无关：脚本标签放 app.js 之前或之后都能工作（app.js 在底部调用
 * switchPage('accounts')，那是异步的用户操作）。
 *
 * 对外只暴露一个入口：window.initSettingsPage()
 */
(function () {
  'use strict';

  // ── 与 app.js 同风格的私有工具（作用域不共享，各存一份）──

  function esc(s) {
    return String(s === null || s === undefined ? '' : s)
      .replace(/&/g, '&amp;').replace(/</g, '&lt;')
      .replace(/>/g, '&gt;').replace(/"/g, '&quot;');
  }

  function setText(id, v) {
    var el = document.getElementById(id);
    if (el) el.textContent = v;
  }

  // 面板写操作令牌（CSRF 主防线，见后端 internal/app/csrf.go）。
  //
  // 🔴 为什么前端要带这个头：
  //   /api/* 不校验 API key（面板必须能在未设 key 时打开），
  //   所以需要一道"浏览器跨站请求带不上"的防线。
  //   自定义头 X-WBAPI-Panel 就是它 —— 浏览器发跨站请求要带自定义头
  //   会先发 CORS 预检，而我们不应答预检，请求就被浏览器自己拦下了。
  //
  // 令牌从 /api/panel-token 取（同源 GET，不需要令牌）。
  // 取不到时降级为不带 —— 后端若未启用该防线仍可工作；
  // 若后端启用了，写操作会得到明确错误而不是静默失败。
  var panelToken = '';

  function loadPanelToken() {
    return fetch('/api/panel-token', { cache: 'no-store' })
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (d) { if (d && d.token) panelToken = d.token; })
      .catch(function () { /* 忽略：降级为不带令牌 */ });
  }

  // 与 app.js 的 fetchJSON 等价：非 2xx 时取出 OpenAI 风格的 error.message
  function fetchJSON(url, opts) {
    opts = opts || {};
    // 所有请求统一带上令牌头（GET 不需要，但带上无害且更简单）
    if (panelToken) {
      opts.headers = opts.headers || {};
      opts.headers['X-WBAPI-Panel'] = panelToken;
    }
    return fetch(url, opts).then(function (r) {
      if (!r.ok) {
        return r.json().catch(function () { return {}; }).then(function (e) {
          var err = new Error((e && e.error && e.error.message) || ('HTTP ' + r.status));
          // 带上状态码与响应体：乐观锁冲突（412）需要拿到服务端当前值
          err.status = r.status;
          err.payload = e;

          // 🔴 403 = 令牌失效（最常见原因：服务重启过，令牌每次启动都换）
          //
          // Codex 第 15 轮定的策略：**只刷新令牌、不自动重试写请求**。
          // 理由：若第一次其实已经成功（只是响应丢了），自动重试会重复应用；
          // 而在"改设置"这种有副作用的写操作上，重复应用比"让用户再点一次"糟得多。
          //
          // 所以我们刷新令牌 + 明确提示，用户再点保存即可成功 ——
          // 比他手动刷新整个页面友好，又不会替他做有风险的决定。
          if (r.status === 403) {
            err.tokenExpired = true;
            loadPanelToken(); // 异步刷新，不阻塞错误抛出
          }
          throw err;
        });
      }
      return r.json();
    });
  }

  // 拼接用户可见文本的唯一出口：调用方给的是文本，这里统一转义。
  // 所有 innerHTML 只接受本文件用 esc() 结果拼出的字符串。
  function joinErr(prefix, e) {
    return esc(prefix + '：' + ((e && e.message) || e || '未知错误'));
  }

  function numOrNull(v) {
    if (v === null || v === undefined || v === '') return null;
    var n = Number(v);
    return isFinite(n) ? n : null;
  }

  // 时间戳（RFC3339 或日期字符串）→ "YYYY-MM-DD HH:MM:SS"；取不到就返回 null。
  // 返回【未转义】的纯文本，由调用方决定怎么放（这里只放进 textContent）。
  function fmtTime(v) {
    if (v === null || v === undefined || v === '') return null;
    var s = String(v);
    var t = Date.parse(s);
    if (isNaN(t)) {
      // 后端可能给 "2026-10-05 13:20:00" 这类已经格式化好的串
      return s.replace('T', ' ').slice(0, 19);
    }
    var d = new Date(t);
    function p(n) { return ('0' + n).slice(-2); }
    return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate())
      + ' ' + p(d.getHours()) + ':' + p(d.getMinutes()) + ':' + p(d.getSeconds());
  }

  // ── 模块状态 ──

  var settingsLoaded = false;  // 本页已成功拉到过 /api/settings（失败重试时不覆盖已渲染内容）
  var lastSettings = null;     // 最近一次的 GET /api/settings 快照
  var busy = false;            // 保存进行中，防重入
  // ⚠️ 密钥输入行的"展开状态"已随新交互取消（2026-10-06）：
  //    输入框常驻可编辑，不再需要 keyFormOpen。
  //    相关根因见 renderSettings 里"密钥"一节的说明。
  var modelsState = { loaded: false, loading: false, list: [] };

  // 防并发重叠的定时器：只在设置页可见时刷「自动签到状态」
  var tickTimer = null;
  var TICK_MS = 5000;

  var PAGES = ['accounts', 'stats', 'api', 'settings'];

  // 端口校验常量（与契约 §1 第 4 条一致；后端仍会再校验一次）
  var PORT_MIN = 1024;
  var PORT_MAX = 65535;

  // ── 只读展示：当前可用模型（喂「目标模型」的 datalist）──

  function loadModels() {
    if (modelsState.loaded || modelsState.loading) return;
    modelsState.loading = true;
    // 🔴 必须走 /api/models（面板同源接口），不能走 /v1/models ——
    // 后者要鉴权，设了密钥后会 401，导致「目标模型」的候选项为空。
    // （2026-10-06 修复，与 app.js 的模型列表是同一个 bug。）
    fetchJSON('/api/models').then(function (d) {
      var data = (d && d.data) || [];
      var out = [];
      for (var i = 0; i < data.length; i++) {
        if (data[i] && data[i].id) out.push(String(data[i].id));
      }
      out.sort();
      modelsState.loaded = true;
      modelsState.list = out;
      renderModelOptions(out.length ? '已加载 ' + out.length + ' 个模型' : '暂无模型（先导入账号）');
    }).catch(function (e) {
      // 拿不到模型目录不影响别名表编辑：输入框本来就是自由文本
      modelsState.loaded = false;
      renderModelOptions('模型列表加载失败，仍可手动输入');
      console.error(e);
    }).then(function () {
      modelsState.loading = false;
    });
  }

  // 把模型列表灌进 <datalist>。只允许填【真实存在的模型 ID】——
  // 别名的目标是 workbuddy/xxx，绝不能填另一个别名（那只解析一层，会直接失败）。
  // 用 <option> 而不是 <select>：既保证 no-camel 是合法目标，又不写第二个状态字段。
  function renderModelOptions(note) {
    var dl = document.getElementById('st-model-options');
    if (dl) {
      var html = '';
      for (var i = 0; i < modelsState.list.length; i++) {
        var id = modelsState.list[i];
        html += '<option value="' + esc(id) + '"></option>';
      }
      dl.innerHTML = html;
    }
    if (note) setText('st-model-note', note);
  }

  // ── 「当前可用模型」只读下拉伸降已删除（2026-10-09 委托方要求）──
  //
  //	原话：「图8『当前可用模型（只读）』没用删掉」。
  //	同一份模型清单在「API 接入」页已完整展示（带名称/上下文/倍率/档位），
  //	这里原先只有一个光秃秃的 ID 列表 —— 信息量严格更少，两处并存
  //	只会让人怀疑哪份准。
  //
  //	⚠️ 但 modelsState 与 loadModels() **必须保留**：
  //	  「目标模型」输入框的 <datalist> 候选就是用它填的（renderModelOptions）。
  //	  删掉它会让别名只能靠手打，打错成另一个别名会成环（只解析一层）。

  // ── 加载 / 渲染 ──

  // loadSeq 是"最新一次请求"的序号，用于丢弃**过期的响应**。
  //
  // 🔴 为什么需要（Codex 第 37 轮 E 点名）：轮询每 5 秒发一次 GET，
  //    网络抖动时可能两次重叠。若先发的响应**后**到达，它会用**旧快照**
  //    覆盖刚渲染的新内容 —— 用户会看到数值"倒回去"，也可能把正在编辑
  //    的状态冲掉。加序号后，只有最新一次请求的响应才允许落地。
  var loadSeq = 0;

  function loadSettings() {
    loadModels();    // 只读展示，不阻塞设置本身
    loadUpdateStatus();  // 版本状态（只读 GET，失败不影响设置页）
    var seq = ++loadSeq;
    return fetchJSON('/api/settings').then(function (d) {
      if (seq !== loadSeq) return;   // 已有更新的请求发出，本次结果作废
      settingsLoaded = true;
      lastSettings = d || {};
      renderSettings(lastSettings);
    }).catch(function (e) {
      if (seq !== loadSeq) return;   // 过期请求的失败也不必提示
      // 静默：保留上次内容；从未成功过才给一句提示
      var box = document.getElementById('st-load-err');
      if (box && !settingsLoaded) {
        box.textContent = '设置读取失败：' + ((e && e.message) || '未知错误');
        box.classList.remove('st-hidden');
      }
      console.error(e);
    });
  }

  // loadUpdateStatus 读版本/更新状态（GET，无副作用）。
  //
  // 🔴 刻意**只读、绝不自动检查**：不在这里调 /api/update/check。
  //	那会变成"面板一打开就偷偷访问 GitHub" —— 与委托方
  //	"只有点按钮才检测"的要求相悖。
  function loadUpdateStatus() {
    return fetchJSON('/api/update').then(function (d) {
      renderUpdateStatus(d);
    }).catch(function (e) {
      // 服务不支持时静默（该区块显示「—」即可，不该刷红）
      console.error(e);
    });
  }

  // （syncModelList 已随「当前可用模型」折叠表一起删除。）

  // checkinRuntime 读自动签到的运行状态。
  //
  // 后端契约（settings.go 的 checkinStatusView）：
  //   checkinRun: { enabled, running, lastRunAt, lastResult, skippedNoAccounts }
  //
  // 🔴 曾经这里是一串"宽泛取字段名"（checkinLastAt / checkin.lastAt /
  //	checkinLastResult / …），那些名字**后端一个都没有**，且从来没有过，
  //	所以整行永远是「—」。宽泛回退的害处正在这里：契约不一致被静默吞掉，
  //	单看前端"没报错"像是正常。现在改成**只认一个形状**，与
  //	keepaliveRuntime 一致；缺字段就显示「—」（守护未装配时确实没有该对象）。
  function checkinRuntime(d) {
    d = d || {};
    var raw = (d.checkinRun && typeof d.checkinRun === 'object') ? d.checkinRun : null;
    if (!raw) return { at: '—', result: '—' };

    var result = raw.lastResult || '—';
    // 顺序要紧：正在跑 > 无账号跳过 > 上次结果。
    //	"没账号"是最容易被误读成"坏了"的状态，所以显式说出来，
    //	而不是留一个「—」让用户去猜。
    if (raw.skippedNoAccounts) result = '未执行（无账号）';
    if (raw.running) result = '签到中…';
    return {
      at: fmtTime(raw.lastRunAt) || '—',
      result: String(result)
    };
  }

  // keepaliveRuntime 读凭证保活的运行状态。
  //
  // 后端契约（settings.go 的 keepaliveStatusView）：
  //   keepaliveRun: { enabled, running, lastRunAt, lastResult, lastError }
  //
  // ⚠️ 不存在历史字段名，所以只认一个形状 —— 猜字段名反而会掩盖契约不一致
  //	（checkinRuntime 就是这么死的，见那里的说明）。
  //	缺字段就显示「—」，不报错（守护未装配时确实没有这个对象）。
  function keepaliveRuntime(d) {
    d = d || {};
    var raw = (d.keepaliveRun && typeof d.keepaliveRun === 'object') ? d.keepaliveRun : null;
    if (!raw) return { at: '—', result: '—' };

    // 有错误优先显示错误（那是最需要用户知道的信息）。
    var result = raw.lastError ? ('⚠️ ' + raw.lastError) : (raw.lastResult || '—');
    if (raw.running) result = '检查中…';
    return {
      at: fmtTime(raw.lastRunAt) || '—',
      result: String(result)
    };
  }

  function aliasesOf(d) {
    var m = d.aliases;
    if (!m || typeof m !== 'object') return {};
    return m;
  }

  function aliasCount(d) {
    var m = aliasesOf(d);
    var n = 0;
    for (var k in m) {
      if (Object.prototype.hasOwnProperty.call(m, k)) n++;
    }
    return n;
  }

  function renderSettings(d) {
    d = d || {};

    // ── 密钥（直接可编辑的明文输入框 —— 2026-10-06 委托方重新设计）──
    //
    // 🔴 为什么改成"直接编辑"而不是"点更换再展开表单"：
    //
    //   原设计是一个「更换」按钮 + 点击后展开的输入行，但委托方实测
    //   **点了完全没反应且不报错**，反复修不到位。他明确要求：
    //
    //     「如果你修不好你就将 key 的框里设置为可直接输入的类型框，
    //       我直接在框里改为我想要的 key 然后点一下更换就直接生效，
    //       就像修改端口一样」
    //
    //   这个设计**消灭了整类 bug**：没有"展开/收起"状态、没有显隐切换、
    //   没有 openKeyForm/closeKeyForm 与轮询的竞态 ——
    //   输入框永远在那儿，和端口输入框完全一致。
    //
    // 🔴 唯一仍需小心的：**用户正在输入时轮询不得覆盖内容**。
    //   这是 Codex 第 36 轮 T1/T4 第 3、4 条的要求，也是最容易
    //   再次踩到的坑 —— 用户打了半天的 key 被 5 秒一次的轮询清掉。
    //   判据：input 处于 focus 且值已被用户改过（dirty）时不覆写。
    var keySet = d.apiKeySet === true;
    var keyText = keySet ? String(d.apiKey || '') : '';

    var keyInput = document.getElementById('st-key-input');
    if (keyInput) {
      // 只在"用户没在编辑这个框"时同步服务端的值。
      // isKeyInputDirty() 见下方定义 —— 它同时看 focus 与用户是否改过。
      if (!isKeyInputDirty()) {
        keyInput.value = keyText;
        keyDirty = false;
      }
    }

    var hint = document.getElementById('st-key-hint');
    if (hint) {
      // 顶部一览仍显示"已设置/未设置"（那一行很窄，塞不下明文）
      hint.textContent = keySet ? '已设置' : '未设置';
    }
    var warn = document.getElementById('st-key-warn');
    if (warn) warn.classList.toggle('st-hidden', keySet);

    // 清空按钮只在有 key 时有意义
    var clearBtn = document.getElementById('btn-st-key-clear');
    if (clearBtn) clearBtn.classList.toggle('st-hidden', !keySet);

    // ⚠️ 这里**不**设置任何控件的 .disabled —— 按钮禁用只有一个来源：
    //    setBusy()。此前散落的 `xxx.disabled = false` 与 setBusy 打架，
    //    是「点按钮没反应」的根因（Codex 第 37 轮 A2）。

    // ── 网络 ──
    var portEl = document.getElementById('st-port');
    if (portEl && document.activeElement !== portEl) {
      portEl.value = (d.port === null || d.port === undefined) ? '' : String(d.port);
    }
    setText('st-port-err', '');
    var portMsg = document.getElementById('st-port-msg');
    if (portMsg) portMsg.classList.toggle('st-hidden', d.restartRequired !== true);
    setText('st-listen', d.listenAddr ? String(d.listenAddr) : '—');
    setText('st-restart-state', d.restartRequired === true ? '有改动待重启' : '无需重启');

    // 配置损坏（契约 §1 第 7 条：后端会报 configCorrupt）
    var corrupt = document.getElementById('st-corrupt');
    if (corrupt) corrupt.classList.toggle('st-hidden', d.configCorrupt !== true);

    // ── 自动化 ──
    //
    // 🔴 两个开关的默认值必须与后端 internal/config.Default() 一致，
    // 否则"后端没给字段时前端自己猜"会显示成与真实状态相反：
    //   - 自动签到：后端默认 false（委托人 2026-10-05 明确"默认关"）
    //   - 开机自启：后端默认 false
    // 早期这里写的是 `d.autoCheckin !== false`（把缺失当成 true），
    // 那是按"默认开"的老设计写的，委托人改默认值后就成了真 bug ——
    // 字段缺失时界面会显示"已启用"，而服务实际并没在签到。
    var autoCheckin = d.autoCheckin === true;   // 默认关（与后端一致）
    var autoStart = d.autoStart === true;       // 默认关

    // 凭证保活：后端默认 **开**（与上面两个相反）。
    //
    // 🔴 为什么要显式区分（2026-10-09）：
    //
    //	后端把"老配置里没这个键"与"用户明确关掉"区分开（Keepalive 是
    //	*bool），GET /api/settings 已经把 nil 归一成 true 再下发，
    //	所以这里**只认 true/false 本身** —— 不要写 `!== false` 之类的
    //	"缺省当开"，那会在后端将来改成默认关时静默不一致
    //	（自动签到就踩过这个坑，见上面的注释）。
    var keepalive = d.keepalive === true;
    setToggle('st-auto-checkin', autoCheckin);
    setToggle('st-auto-start', autoStart);
    setToggle('st-keepalive', keepalive);

    var apiKeyLine = d.apiKeySet === true ? '已设置' : '未设置';
    // 🔴 顶层一览与「自动化」区**各有独立的 id**（2026-10-09 修）。
    //
    //	原来两处共用 st-sum-auto-checkin / st-sum-keepalive / st-sum-auto-start，
    //	而 getElementById 只返回文档里**第一个**匹配 → setText 永远只更新
    //	顶层那份，区里那三行停在 HTML 写死的「—」。
    //	委托方实测反馈「为什么图3四个状态都是—」，这是成因之一。
    //	现在两份都显式更新。
    setText('st-ovw-auto-checkin', '自动签到当前：' + (autoCheckin ? '已启用' : '已停用'));
    setText('st-ovw-keepalive', '凭证保活当前：' + (keepalive ? '已启用' : '已停用'));
    setText('st-ovw-auto-start', '开机自启当前：' + (autoStart ? '已启用' : '已停用'));
    setText('st-sum-auto-checkin', '自动签到当前：' + (autoCheckin ? '已启用' : '已停用'));
    setText('st-sum-keepalive', '凭证保活当前：' + (keepalive ? '已启用' : '已停用'));
    setText('st-sum-auto-start', '开机自启当前：' + (autoStart ? '已启用' : '已停用'));
    setText('st-sum-api-key', '接口密钥：' + apiKeyLine);
    setText('st-sum-port', '监听端口：' + ((d.port === null || d.port === undefined) ? '—' : d.port));

    // 签到的运行状态（上次触发 / 结果）——
    // 🔴 不再更新 st-checkin-state：那个「已启用/已停用」元素随
    //	"运行状态"整行一起删掉了（2026-10-09 委托方要求：运行状态并进
    //	自动签到栏，且只保留"上次触发"与"结果"两条）。
    //	开关状态上面 st-sum-auto-checkin 已经表达了，重复一条没意义。
    var rt = checkinRuntime(d);
    setText('st-checkin-at', rt.at);
    setText('st-checkin-result', rt.result);

    // 保活运行状态（上次检查时间 / 结果）。
    var kp = keepaliveRuntime(d);
    setText('st-keepalive-at', kp.at);
    setText('st-keepalive-result', kp.result);

    // ── 映射表 ──
    // 注意顺序：renderAliases 会清空草稿并重绘草稿表，必须在它之后
    // 再更新计数，否则计数用的是被覆盖前的旧值。
    renderAliases(aliasesOf(d));
    setText('st-alias-count', '共 ' + aliasCount(d) + ' 条');
  }

  function setToggle(id, on) {
    var el = document.getElementById(id);
    if (!el) return;
    el.classList.toggle('on', on);
    el.setAttribute('aria-checked', on ? 'true' : 'false');
    el.disabled = false;
  }

  function renderAliases(map) {
    aliasDraft.clear();   // 整表重渲染 = 服务端状态刷新，草稿不再适用
    var body = document.getElementById('st-alias-body');
    if (!body) return;

    var keys = [];
    for (var k in map) {
      if (Object.prototype.hasOwnProperty.call(map, k)) keys.push(k);
    }
    keys.sort();

    if (!keys.length) {
      body.innerHTML = '<tr><td colspan="3" class="empty">暂无映射 —— 点右上「新增映射」添加</td></tr>';
      return;
    }

    var html = '';
    for (var i = 0; i < keys.length; i++) {
      var alias = keys[i];
      var target = map[alias];
      if (target === null || target === undefined) target = '';
      // 服务端那一行的「删除」只是移进草稿（st-pending），
      // 真正落盘要等「保存映射」；本地新加的同样住在草稿里。
      // 这样「保存」永远是一次 PATCH = 一次整表原子替换，不存在半提交。
      html += '<tr>'
        + '<td class="mono st-td-alias">' + esc(alias) + '</td>'
        + '<td class="mono st-td-target">' + (target === '' ? '<span class="st-none">—</span>' : esc(target)) + '</td>'
        + '<td class="st-td-act">'
        +   '<button type="button" class="btn btn-sm" data-st-act="del-alias" data-alias="' + esc(alias) + '">删除</button>'
        + '</td>'
        + '</tr>';
    }
    body.innerHTML = html;
    renderAliasDraft();
  }

  // ── 草稿（本地新增 / 本地待删除）──
  //
  // 为什么要有草稿这一层：契约里 aliases 是【整表替换】。
  // 如果每加一条就立刻 PATCH，用户想加 3 条就要发 3 次请求、失败一次就半途而废；
  // 更糟的是"删一行 + 加一行"的换目标操作会出现"新目标已生效、旧条目还在"的
  // 中间态，而那正好是自映射/循环的来源。所以：
  //   新增 → 进草稿；服务端已存在的行点「删除」→ 进待删除草稿；
  //   「保存映射」= 一次 PATCH，提交 (服务端现有 − 待删除 + 新增)。
  // 左列「状态」把两者区分开，刷新页面 = 放弃草稿（服务端状态为准）。

  var aliasDraft = {
    add: [],     // [{name, target}]
    del: [],     // [name]
    clear: function () { this.add.length = 0; this.del.length = 0; }
  };

  function draftHasAdd(name) {
    for (var i = 0; i < aliasDraft.add.length; i++) {
      if (aliasDraft.add[i].name === name) return true;
    }
    return false;
  }

  function draftHasDel(name) {
    for (var i = 0; i < aliasDraft.del.length; i++) {
      if (aliasDraft.del[i] === name) return true;
    }
    return false;
  }

  // 结果条目：name → target；target 为 null 表示这一条会被删掉
  function aliasResult() {
    var out = [];
    var seen = {};
    var base = lastSettings ? aliasesOf(lastSettings) : {};
    var k;

    for (k in base) {
      if (!Object.prototype.hasOwnProperty.call(base, k)) continue;
      seen[k] = true;
      out.push({ name: k, target: draftHasDel(k) ? null : base[k] });
    }
    for (var i = 0; i < aliasDraft.add.length; i++) {
      var a = aliasDraft.add[i];
      if (seen[a.name]) continue;   // 服务端已有同名条目，以服务端那条为准
      seen[a.name] = true;
      out.push({ name: a.name, target: a.target });
    }
    out.sort(function (x, y) { return x.name < y.name ? -1 : (x.name > y.name ? 1 : 0); });
    return out;
  }

  function aliasDirty() {
    return aliasDraft.add.length > 0 || aliasDraft.del.length > 0;
  }

  function renderAliasDraft() {
    var body = document.getElementById('st-alias-draft');
    if (body) {
      var rows = aliasDraft.add.concat(aliasDraft.del);
      if (!rows.length) {
        body.innerHTML = '<tr><td colspan="4" class="empty">'
          + '还没有本地改动 —— 左侧「删除」与「新增」都会先记在这里，'
          + '点「保存映射」才真正提交（一次整表替换）</td></tr>';
      } else {
        var html = '';
        var i;
        for (i = 0; i < aliasDraft.add.length; i++) {
          html += '<tr>'
            + '<td class="st-td-alias">新增</td>'
            + '<td class="mono">' + esc(aliasDraft.add[i].name) + '</td>'
            + '<td class="mono">' + esc(aliasDraft.add[i].target) + '</td>'
            + '<td class="st-td-act"><button type="button" class="btn btn-sm" '
            + 'data-st-act="del-add" data-idx="' + i + '">撤回</button></td>'
            + '</tr>';
        }
        for (i = 0; i < aliasDraft.del.length; i++) {
          html += '<tr>'
            + '<td class="st-td-del">待删除</td>'
            + '<td class="mono">' + esc(aliasDraft.del[i]) + '</td>'
            + '<td class="st-none">—</td>'
            + '<td class="st-td-act"><button type="button" class="btn btn-sm" '
            + 'data-st-act="del-del" data-idx="' + i + '">撤回</button></td>'
            + '</tr>';
        }
        body.innerHTML = html;
      }
    }

    var dirty = aliasDirty();
    // ⚠️ 只控制「保存映射」（批量提交）的可用性，不碰「加入草稿」——
    // 后者由表单的展开/收起决定，两者混用会让「加入草稿」被误禁用。
    var saveBtn = document.getElementById('btn-st-alias-batch-save');
    if (saveBtn) saveBtn.disabled = !dirty || busy;
    setText('st-alias-dirty', dirty
      ? ('有 ' + (aliasDraft.add.length + aliasDraft.del.length) + ' 处未保存的改动')
      : '无未保存改动');
  }

  // ── PATCH /api/settings ──
  //
  // 请求体【只放本次要改的字段】—— 契约 §3：字段缺失 = 未修改。
  // 尤其注意 apiKey 与 apiKeyClear 绝不能同时出现（后端会 400），
  // 所以本页的所有调用点都只经过这一个函数，由它保证二选一。
  //
  // 🔴 乐观锁（Codex 第 12 轮要求）：必须带上 ifRevision。
  //
  //   后端在缺 ifRevision 时返回 428、不匹配时返回 412 ——
  //   这是刻意的：若"缺失就放行"，前端漏传一个字段就退化成
  //   无保护的整表覆盖，乐观锁白做。
  //
  //   冲突（412）时后端会返回当前完整设置，我们用它刷新页面
  //   并把冲突原因显示给用户，让 TA 重新决定 —— 不自动重试，
  //   因为"自动覆盖"正是我们要防的。

  function patchSettings(patch) {
    if (busy) return Promise.reject(new Error('上一个保存还没结束'));
    setBusy(true);   // 唯一入口：同时置 busy 与禁用按钮（见 setBusy 注释）

    // 🔴 乐观锁：必须带**完整版本串**（Codex 第 15 轮）。
    //
    //   后端只接受 `inst-<代次>-rev-<n>` 形态 —— 因为纯数字无法携带
    //   进程代次：配置损坏重建后 revision 会从 0 重来，旧页面手里的
    //   数字 0 会**误匹配**新配置，乐观锁被绕过。
    //
    //   串从哪来：GET 返回的 ETag（形如 `"inst-xxx-rev-3"`），
    //   去掉引号即是。若拿不到就传空串，让后端以 428 明确拒绝 ——
    //   绝不退化成"不带版本直接写"（那正是要防的无保护覆盖）。
    var revToken = '';
    if (lastSettings && lastSettings.revisionToken) {
      revToken = String(lastSettings.revisionToken);
    } else if (typeof lastSettings.revision === 'number' &&
               lastSettings.etag) {
      revToken = String(lastSettings.etag).replace(/"/g, '');
    }
    var body = {};
    for (var k in patch) { if (Object.prototype.hasOwnProperty.call(patch, k)) body[k] = patch[k]; }
    if (revToken) {
      body.ifRevision = revToken;
    }

    return fetchJSON('/api/settings', {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body)
    }).then(function (d) {
      settingsLoaded = true;
      // PATCH 成功返回与 GET 相同的结构体 → 直接用返回值刷新整页，
      // 不额外发一次 GET（避免两次读之间状态不一致）
      lastSettings = d || lastSettings;
      renderSettings(lastSettings);
      return lastSettings;
    }, function (e) {
      // 412 冲突：用服务端返回的当前状态刷新，并提示用户重新加载
      if (e && e.status === 412 && e.payload && e.payload.current) {
        lastSettings = e.payload.current;
        renderSettings(lastSettings);
        showConflictNotice(e.payload.message || '设置已被其他页面修改，已为你载入最新值');
      }
      throw e;
    }).then(function (d) {
      finishBusy();
      return d;
    }, function (e) {
      finishBusy();
      throw e;
    });
  }

  function setBusy(b) {
    // 只禁用会写盘的按钮；输入框不锁（用户仍可继续编辑）。
    // 注意这里【不含】映射表的「保存映射」—— 它的可用性由草稿状态决定
    // （renderAliasDraft 负责），否则一次无关的保存会把它的禁用态改错。
    //
    // 🔴 本函数是这批按钮 disabled 状态的【唯一写入源】（Codex 第 37 轮 A2）。
    //    renderSettings 里原先散落的 `xxx.disabled = false` 已全部移除 ——
    //    那种写法会造成"轮询解禁 / busy 又禁"的竞态，而按钮一旦 disabled，
    //    浏览器**根本不派发 click**，用户看到的就是"点了完全没反应且不报错"。
    busy = !!b;   // 与 DOM 状态保持同源，避免 busy 变量与按钮态不一致
    // ⚠️ 列表里已移除 btn-st-key-change / btn-st-key-cancel ——
    //    新密钥交互取消了这两个按钮（2026-10-06）。
    var ids = ['btn-st-key-save', 'btn-st-key-clear',
      'btn-st-port-save', 'st-auto-checkin', 'st-auto-start',
      'btn-st-alias-add', 'btn-st-alias-cancel', 'btn-st-alias-batch-save'];
    for (var i = 0; i < ids.length; i++) {
      var el = document.getElementById(ids[i]);
      if (el) el.disabled = !!b;
    }
    if (!b) renderAliasDraft();   // 解锁后按草稿状态恢复「保存映射」的可用性
  }

  function finishBusy() {
    // 🔴 兜底：无论正常/异常路径都恢复可交互（Codex 第 37 轮 A2 第 4 条）。
    //    宁可放过一次"理论上的重入"，也不能让按钮永久卡死 ——
    //    卡死时用户没有任何自救手段（不报错、点不动），只能刷新页面。
    setBusy(false);
    // 再用最新快照整页渲染一次，保证所有控件状态与服务端一致
    if (lastSettings) renderSettings(lastSettings);
  }

  function save(patch, okMsg) {
    return patchSettings(patch).then(function () {
      toast(okMsg, 'ok');
    }).catch(function (e) {
      // 令牌失效（服务重启过）→ 给出可操作的提示，而不是一句"失败"
      if (e && e.tokenExpired) {
        toast('会话令牌已更新（服务可能重启过），请再点一次「保存端口」', 'bad');
        console.error(e);
        return;
      }
      toast(joinErr('保存失败', e), 'bad');
      console.error(e);
    });
  }

  // ── 密钥（直接编辑，与「端口」同一交互）──
  //
  // 🔴 设计变更（2026-10-06 委托方要求）：
  //   取消「更换」按钮 + 展开表单的旧交互，改成**输入框永远可编辑**、
  //   改完点「保存」即生效。委托方原话：
  //     「将 key 的框里设置为可直接输入的类型框，我直接在框里改为我想要
  //       的 key 然后点一下更换就直接生效，就像修改端口一样」
  //
  //   收益：没有展开/收起状态 ⇒ 没有竞态 ⇒ 消灭了「点了没反应」整类 bug。

  // keyDirty 记录用户是否已经手动改过输入框。
  //
  // 用途：轮询（renderSettings）同步服务端值前要问它 ——
  // 用户打了半天的新 key 绝不能被 5 秒一次的轮询冲掉。
  var keyDirty = false;

  // isKeyInputDirty 判断"现在能不能安全地覆写输入框"。
  //
  // 判据（两者任一即视为"别动它"）：
  //   - 输入框正被聚焦（用户正在这里打字）
  //   - 已被用户改过（keyDirty）且还没保存
  function isKeyInputDirty() {
    var input = document.getElementById('st-key-input');
    if (!input) return false;
    if (document.activeElement === input) return true;
    return keyDirty;
  }

  function saveKey() {
    var input = document.getElementById('st-key-input');
    if (!input) return;
    var v = input.value.trim();
    // 空值不走这里 —— 要取消保护请点「清空」（它带二次确认）。
    // 这样避免"手滑清空输入框 + 保存"把鉴权悄悄关掉。
    if (!v) {
      setText('st-key-err', '密钥不能为空 —— 要取消校验请点「清空」');
      return;
    }
    setText('st-key-err', '');
    // 只发 apiKey 一个字段：其余字段一律不动（契约 §3 的缺失语义）
    save({ apiKey: v }, '密钥已更新（立即生效）').then(function () {
      keyDirty = false;   // 已落盘，之后可以安全地被服务端值覆盖
    });
  }

  // ⚠️ 复制按钮已移除（2026-10-06 委托方要求）：
  //    「现在key已经明文显示了，不需要复制按钮了」
  //    明文就在输入框里，用户可以直接选中 —— 多一个按钮反而占地方。

  // 清空 = 解除鉴权，本机任何进程都能调这个接口 → 必须二次确认
  function clearKey() {
    openConfirm(
      '清空密钥？',
      '<p>清空后 <b>本机任何程序都可以调用此接口</b>，不再需要 Authorization。</p>'
      + '<p class="st-danger-line">这项操作立即生效，无法撤销（只能重新设置一个新密钥）。</p>',
      '确认清空',
      function () {
        // 与 saveKey 互斥：清空时【只发】apiKeyClear，绝不带 apiKey（否则后端 400）
        return save({ apiKeyClear: true }, '密钥已清空（立即生效）').then(function () {
          var input = document.getElementById('st-key-input');
          if (input) input.value = '';
          keyDirty = false;
        });
      }
    );
  }

  // bindKeyInput 给密钥输入框挂上"用户改过没有"的追踪 + 回车保存。
  //
  // 为什么需要：轮询会用服务端值同步输入框，若不知道用户改过没有，
  // 就会把用户正在输入的新 key 冲掉（Codex 第 36 轮 T4 第 4 条）。
  function bindKeyInput() {
    var input = document.getElementById('st-key-input');
    if (!input) return;
    input.addEventListener('input', function () { keyDirty = true; });
    input.addEventListener('keydown', function (ev) {
      // 回车即保存 —— 与「端口」输入框同一习惯
      if (ev.key === 'Enter') {
        ev.preventDefault();
        saveKey();
      }
    });
  }

  // ── 网络（端口）──

  function savePort() {
    var el = document.getElementById('st-port');
    if (!el) return;

    var v = el.value.trim();
    setText('st-port-err', '');

    if (!/^\d+$/.test(v)) {
      setText('st-port-err', '端口必须是数字');
      return;
    }
    var p = Number(v);
    if (p < PORT_MIN || p > PORT_MAX) {
      setText('st-port-err', '端口范围 ' + PORT_MIN + '–' + PORT_MAX + '（<1024 需要管理员权限）');
      return;
    }
    if (lastSettings && lastSettings.port === p) {
      toast('端口未变化', 'ok');
      return;
    }
    // 只发 port：端口变更后端只标记 restartRequired，不热切换
    save({ port: p }, '端口已保存 —— 需重启生效');
  }

  function restartServer() {
    openConfirm(
      '立即重启服务？',
      '<p>服务会用新端口重新拉起（先确保新进程能起来，再退出旧进程）。</p>'
      + '<p>重启期间本页面会短暂断开；恢复后会自动重连，'
      + '若端口有变更会自动跳到新地址。</p>',
      '立即重启',
      function () {
        // 新的一轮操作开始 → 清掉上一轮的失败态（否则用户会以为还没重试）
        clearRestartFailure();
        setText('st-restart-msg', '正在重启…');
        return fetchJSON('/api/settings/restart', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          // 🔴 必须带上**面板当前所在 origin**（location.origin）。
          //
          // 原因（实测）：重启后新进程要把"旧 origin"加进 /healthz 的
          // CORS 白名单，否则旧页面读不到新端口的健康响应
          // （浏览器报 Failed to fetch），自动重连 100% 失效。
          //
          // 且**不能**由服务端按监听地址推导：用户可能用 localhost 打开，
          // `http://localhost:8787` 与 `http://127.0.0.1:8787`
          // 是两个不同的 Origin 字符串（实测白名单不匹配即被拒）。
          // 服务端会对这个值做独立严格校验，非法则忽略。
          body: JSON.stringify({ origin: location.origin })
        }).then(function (d) {
          toast('已提交重启 —— 页面将自动重连', 'ok');
          // 把服务端算好的新地址与本次交接标识传给重连逻辑：
          // 端口变了也能跳过去，且能确认对面是**本次**交接的进程。
          pollRestart(d && d.newAddr ? d.newAddr : '',
                     d && d.restartId ? d.restartId : '');
        }).catch(function (e) {
          // 失败时后端保持运行（契约 §3），页面也保持可用。
          //
          // 🔴 必须留下**持久可见**的失败状态（Codex 第 20 轮指出）：
          //
          //	原实现把 st-restart-msg 清回空串，理由是"操作失败不用留痕"。
          //	但审核指出：后端恢复可用 **不等于用户知道重启失败了** ——
          //	清空之后页面上没有任何痕迹，用户以为点过了就完了，
	          //	而实际服务还跑在旧端口上。那是**静默失败**。
          //
          //	所以这里改成写入一句可操作的具体信息，并**保留**它
          //	（由下一次成功重启或用户重新操作来覆盖）。
          //
          //	⚠️ 同时这也修掉一个"会撒谎的观测点"：
          //	清空行为让 st-restart-msg 在**失败时永远是空的**，
          //	拿它当"handler 有没有跑"的判据会得到假阴性
          //	（我在第 19 轮据此误判过"点击没生效"，详见维护备忘 §六之四坑 C）。
          toast(joinErr('重启失败', e), 'bad');
          setRestartFailure(joinErr('重启失败', e) +
            '；服务仍在旧地址运行。请检查端口是否被占用，或改为手动重启。');
          console.error(e);
        });
      }
    );
  }

  // setRestartFailure 写入**持久**的重启失败提示。
  //
  // 与 toast 的区别：toast 几秒后消失，而"重启失败"是需要用户处理的状态，
  // 不能只闪一下。这里直接写进 st-restart-msg 并保留。
  function setRestartFailure(text) {
    setText('st-restart-msg', text);
    var el = document.getElementById('st-restart-msg');
    if (el) {
      // 用红色类名让它显眼（不加则沿用普通 hint 样式）
      el.classList.add('st-restart-failed');
    }
  }

  // clearRestartFailure 在成功路径上清掉失败态。
  function clearRestartFailure() {
    var el = document.getElementById('st-restart-msg');
    if (el) el.classList.remove('st-restart-failed');
  }

  // 重启后自动重连（Codex 第 15/17 轮要求，第 19 轮重做判据）。
  //
  // 🔴 为什么不能只 reload：端口变了的话**旧 origin 就失效了**，
  // `location.reload()` 会一直失败，用户看到"连不上"却不知道该去哪。
  // 所以要用服务端返回的 newAddr 探测，就绪后**跳到新地址**。
  //
  // ═══════════════════════════════════════════════════════════════
  // 🔴 探活判据的演进（两版都是错的，别再退化回去）
  // ═══════════════════════════════════════════════════════════════
  //
  // v1（错）：`fetch(newOrigin + '/healthz', {mode:'no-cors'})`，
  //   只看 ".then 到了没"。
  //   **实测：503 / 404 / 200 三者全部 resolve**，且 type 恒为 'opaque'、
  //   status 恒为 0 —— 完全无法区分。探针于是退化成"端口是否有人监听"，
  //   会跳到未就绪的服务，甚至跳到恰好占着那个端口的无关程序。
  //
  // v2（仍错）：改用普通 CORS fetch，但只检查 `response.ok`。
  //   **实测：无关服务若返回 `Access-Control-Allow-Origin: *` + 200**，
  //   同样得到 ok:true 且 body 可读 → 仍会误判并跳过去。
  //
  // v3（当前）：普通 CORS fetch + **四条件全中**才导航：
  //     response.ok && service==='TAOAPI' && ready===true && restartId===本次标识
  //   第四项用于排除"上一次重启遗留的、还活着的旧进程"
  //   （它同样会返回 service/ready，只有交接标识能区分是不是**本次**的）。
  function pollRestart(newAddr, restartId) {
    var deadline = Date.now() + 30000;
    var oldOrigin = location.origin;
    var newOrigin = '';
    if (newAddr) {
      newOrigin = 'http://' + newAddr;
    }
    // 端口没变时 newOrigin === oldOrigin，语义等同"就 reload"。
    var portChanged = !!newOrigin && newOrigin !== oldOrigin;

    function giveUp() {
      // 超时也要留下**持久**且可操作的提示（Codex 第 20 轮要求）。
      //
      // 这一条尤其重要：它对应"新进程起来了但我们连不上"的情形 ——
      // 用户此时既看不到旧地址（已断）也到不了新地址，
      // 如果提示一闪而过或为空，他只会觉得"刷新一下就好了"，
      // 而实际上需要手动访问新地址或检查端口。
      setRestartFailure(
        '重启后未能连接，请手动访问' +
        (newOrigin ? ' ' + newOrigin + '/panel/' : '原地址') +
        '；若仍不可用，请检查端口是否被占用。');
    }

    // 端口**没**变：旧地址恢复可用就 reload（reload 后仍是同一 origin）。
    function probeSameOrigin() {
      if (Date.now() > deadline) { giveUp(); return; }
      fetch('/status', { cache: 'no-store' }).then(function (r) {
        if (r.ok) { location.reload(); return; }
        setTimeout(probeSameOrigin, 1000);
      }).catch(function () {
        setTimeout(probeSameOrigin, 1000);
      });
    }

    // 探测**旧地址**是否已恢复服务 —— 用于及早发现"重启失败、已回滚"。
    //
    // ═══════════════════════════════════════════════════════════════
    // 🔴 判据必须严格，不能"旧地址能访问就算回滚"（Codex 第 21 轮指出）
    // ═══════════════════════════════════════════════════════════════
    //
    //	我第一版就是那么写的，存在**竞态**：
    //	  202 发出后，旧服务可能**还没真正开始关闭**（服务端要 sleep 300ms
    //	  才发重启信号，Shutdown 也需要时间）。此时旧地址当然可用 ——
    //	  但它不代表"回滚"，而是"重启还没开始"。
    //	  若据此立刻提示"重启未生效"，而新端口随后成功 ready，
    //	  用户就会看到**错误的失败提示**。
    //
    //	所以现在要求**两个条件同时成立**才判定回滚：
    //	  ① 新地址已经**明确失败过**（至少探到一次连不上/非 200）
    //	  ② 旧地址在①之后仍然可访问
    //	即"新的上不去 + 旧的还在" —— 这**足以判定**为回滚。
    //
    //	⚠️ 措辞注意（Codex 第 22 轮）：**不要**说成"唯一指向回滚"。
    //	  它是"足以判定"，不是逻辑上的充分必要条件 ——
    //	  若将来出现超过 2 秒的慢关闭/慢启动，边界上仍可能有误判。
    //	  （该边界风险已降为压力测试项，当前不再是发布阻塞。）
    //
    //	另外加一个时间下限：前 2 秒内不判定（那正是"重启还没开始"的窗口）。
    var recoveryNoted = false;
    var newProbeFailedOnce = false;
    var recoveryCheckAfter = Date.now() + 2000;

    function probeOldForRecovery() {
      if (recoveryNoted) return;
      // 条件①：新地址必须已经失败过
      if (!newProbeFailedOnce) return;
      // 时间下限：避开"重启尚未真正开始"的窗口
      if (Date.now() < recoveryCheckAfter) return;
      // 条件②：旧地址仍然可访问
      fetch('/status', { cache: 'no-store' }).then(function (r) {
        if (!r.ok || recoveryNoted || !newProbeFailedOnce) return;
        if (Date.now() < recoveryCheckAfter) return;
        recoveryNoted = true;
        setRestartFailure(
          '重启可能未生效：服务仍在旧地址 ' + oldOrigin + '/panel/。' +
          (newOrigin ? '若新地址 ' + newOrigin + ' 打不开，即为重启失败；' : '') +
          '请检查端口是否被占用。');
      }).catch(function () { /* 旧地址不可用属正常，忽略 */ });
    }

    // 端口**变了**：只探新地址。
    //
    // 🔴 不能先探旧地址再回退（原实现就是那样，实测有 bug）：
    //	本函数在 202 后**立刻**被调用，而服务端要 300ms 才发重启信号，
    //	此刻旧服务仍活着 → `fetch('/status')` 成功 → 走 `location.reload()`
    //	→ 页面在旧 origin 上刷新，重启导航逻辑随之丢失。
    //	实测：服务确实重启成功（新端口 /healthz 返回本次 restartId），
    //	但页面**一次探测都没发出**，URL 始终停在旧端口 —— 静默失效。
    function probeNewOrigin() {
      if (Date.now() > deadline) { giveUp(); return; }

      // 🔴 顺带探一次**旧地址**，用来及早发现"重启失败并已恢复"。
      //
      //	为什么需要（Codex 第 20 轮的关切，我实测确认的缺口）：
      //	  重启失败时，父进程会**回滚到旧端口继续服务**，
      //	  但它对面板的 202 早已发出去，**没有任何通道把失败告诉面板**。
      //	  面板于是只能傻等到 30 秒 deadline 才提示 ——
      //	  期间用户看到的是"正在重启…"，既不知道失败了，也不知道服务其实还活着。
      //
      //	判定方式（两个条件同时成立，见 probeOldForRecovery 的说明）：
      //	  ① 新地址已明确失败过   ② 旧地址此时仍可访问
      //	  —— 只有"新的上不去 + 旧的还在"才唯一指向回滚。
      probeOldForRecovery();
      // 普通 CORS fetch（**不是** no-cors），并按四条件判定。
      //
      // no-cors 会把 503/404/200 全变成"resolve + opaque"，
      // 根本分不出对面就绪没有（实测，见上方 v1 说明）。
      // 普通 CORS 模式下：没有正确 CORS 头会 **reject**，
      // 有正确头才 resolve 且能读到 status 与 body。
      fetch(newOrigin + '/healthz', { cache: 'no-store' })
        .then(function (r) {
          if (!r.ok) {
            // 对面回答了但没就绪（如 503）→ 继续等，**不要**跳。
            // ⚠️ 这也算"新地址失败过"：503 说明那边还不是可用服务，
            //    与"连不上"同样能构成回滚判定所需的那个前提。
            newProbeFailedOnce = true;
            setTimeout(probeNewOrigin, 1000);
            return null;
          }
          return r.json().catch(function () { return null; });
        })
        .then(function (h) {
          if (h === null || h === undefined) {
            return; // 上面已经安排了重试
          }
          // 四条件全中才导航（缺一不可）。
          //
          // 🔴 这里的服务标识必须与后端 healthServiceName 一致。
          //	2026-10-07 实测缺陷：产品改名 wbapi → TAOAPI 时**只改了后端常量**，
          //	前端这里仍写死 'wbapi' ⇒ 第四条件永远不成立 ⇒
          //	面板一直探到 30 秒 deadline，走 giveUp() 报
          //	「重启后未能连接，请手动访问 http://127.0.0.1:<新端口>/panel/」——
          //	**而新端口其实一直是好的**（用户手动访问能开）。
          //	即"重启明明成功了，面板却说失败"。
          //	护栏：`TestPanelServiceNameMatchesBackend`（跨端一致性，防再改名漏改）。
          if (h.service !== 'TAOAPI' || h.ready !== true) {
            // 对面不是我们的服务（或没就绪）→ 不跳。
            setTimeout(probeNewOrigin, 1000);
            return;
          }
          if (restartId && h.restartId !== restartId) {
            // 是 TAOAPI，但不是**本次**交接的进程
            // （可能是上次重启遗留的旧进程）→ 不跳。
            // ⚠️ 这也是"新地址没能提供本次交接的服务" → 计入失败前提。
            newProbeFailedOnce = true;
            setTimeout(probeNewOrigin, 1000);
            return;
          }
          // 确认为本次交接的新进程 —— 跳到新地址。
          // 令牌是**每次启动重新生成**的，所以新页面会自己重新取，
          // 这里不需要（也无法）把旧令牌带过去。
          location.href = newOrigin + '/panel/';
        })
        .catch(function () {
          // 连接被拒 / CORS 被拒 / JSON 解析失败 → 都算没就绪。
          // 🔴 记为"新地址失败过"：这是回滚判定的必要条件①。
          newProbeFailedOnce = true;
          setTimeout(probeNewOrigin, 1000);
        });
    }

    // 开始前先等一小会儿：202 已返回但服务端 300ms 后才真正重启，
    // 立刻探测会撞上"旧服务仍在"的窗口。
    setTimeout(portChanged ? probeNewOrigin : probeSameOrigin,
               portChanged ? 800 : 300);
  }

  // ── 自动化开关 ──

  function toggleAutoCheckin() {
    var cur = lastSettings ? lastSettings.autoCheckin !== false : true;
    var next = !cur;
    // 只发 autoCheckin：后端热生效，立即停止/开始后续调度
    save({ autoCheckin: next }, '自动签到已' + (next ? '启用' : '停用') + '（立即生效）');
  }

  function toggleAutoStart() {
    var cur = lastSettings ? lastSettings.autoStart === true : false;
    var next = !cur;
    save({ autoStart: next }, '开机自启已' + (next ? '启用' : '停用') + '（立即生效）');
  }

  // toggleKeepalive 切换凭证保活。
  //
  // 🔴 关掉时的告警措辞很重要：关掉不会立刻有任何变化，
  //	但**将来**凭据过期时会需要重新登录 —— 用户必须知道这个后果。
  function toggleKeepalive() {
    var cur = lastSettings ? lastSettings.keepalive === true : true;
    var next = !cur;
    var msg = next
      ? '凭证保活已启用（立即生效）'
      : '凭证保活已停用 —— 凭据过期后将需要重新登录';
    save({ keepalive: next }, msg);
  }

  // ── 版本与更新（2026-10-09）──
  //
  // 🔴 与"自动更新"的区别（委托方明确要求）：
  //
  //	原话：「没有软件自动更新功能，但是有检测最新版本按钮，
  //	       检测出新版本后会有更新版本的按钮，点了就自动安装并更新」
  //
  //	⇒ 后端**没有任何定时/后台**的更新逻辑（全项目搜不到 update 的 ticker）。
  //	  两个按钮分别对应两个接口：
  //	    「检测新版本」 → POST /api/update/check（只查，不改任何东西）
  //	    「更新到新版本」→ POST /api/update/apply（下载+校验+替换+重启）
  //	  后者只在**检测到有更新时**才显示（后端也再校验一次，双保险）。

  // updateState 保存最近一次检测结果，供"是否显示更新按钮"判断。
  var updateState = { hasUpdate: false, latest: '', busy: false };

  function renderUpdateStatus(d) {
    d = d || {};
    updateState.hasUpdate = d.hasUpdate === true;
    updateState.latest = d.latest || '';

    setText('st-ver-current', d.current ? ('v' + d.current) : '—');

    // 开发构建：明确说明不参与更新检查（否则用户会以为"已是最新"）
    if (d.dev) {
      setText('st-ver-note', '当前是开发构建（版本号未注入），不参与更新检查。');
    } else if (d.releaseConfigured === false) {
      setText('st-ver-note', '当前运行方式不支持检查更新。');
    }

    var applyBtn = document.getElementById('btn-st-update-apply');
    if (applyBtn) {
      // 只在**确实有更新**时显示「更新」按钮。
      //
      // ⚠️ 不要因为"上次检测过"就常显 —— 那会让用户在一个
      //	其实已是最新的版本上看到"更新"按钮，属于骗人。
      if (updateState.hasUpdate && !updateState.busy) {
        applyBtn.classList.remove('st-hidden');
        applyBtn.textContent = '更新到 v' + updateState.latest;
      } else {
        applyBtn.classList.add('st-hidden');
      }
    }

    // 检测/下载状态
    if (d.checking) {
      setText('st-update-msg', '正在检测…');
    } else if (d.downloading) {
      setText('st-update-msg', '正在下载并校验…');
      showUpdateProgress(true);
    } else {
      showUpdateProgress(false);
      if (d.checkError) {
        setText('st-update-msg', '');
        setText('st-update-result', '⚠️ 检测失败：' + d.checkError);
      } else if (d.hasUpdate) {
        setText('st-update-msg', '发现新版本 v' + d.latest);
      } else if (d.latest) {
        setText('st-update-msg', '已是最新版本');
      } else if (d.lastCheck) {
        setText('st-update-msg', '已是最新版本（官方仓库暂无更新发布）');
      }
    }
    if (d.lastResult) {
      setText('st-update-result', (d.lastResultOK ? '✅ ' : '⚠️ ') + d.lastResult);
    }
  }

  function showUpdateProgress(on) {
    var box = document.getElementById('st-update-progress');
    if (!box) return;
    box.classList.toggle('st-hidden', !on);
    var bar = document.getElementById('st-update-bar');
    if (bar) bar.style.width = on ? '100%' : '0%';
  }

  // checkUpdate 点「检测新版本」。
  function checkUpdate() {
    var btn = document.getElementById('btn-st-update-check');
    if (btn) btn.disabled = true;
    setText('st-update-msg', '正在检测…');
    setText('st-update-result', '');

    fetchJSON('/api/update/check', { method: 'POST' })
      .then(function (d) {
        renderUpdateStatus(d);
      })
      .catch(function (e) {
        setText('st-update-msg', '');
        setText('st-update-result', '⚠️ 检测失败：' + ((e && e.message) || '未知错误'));
        console.error(e);
      })
      .then(function () {
        if (btn) btn.disabled = false;
      });
  }

  // applyUpdate 点「更新到新版本」。
  //
  // 🔴 这是一次**会替换正在运行的程序**的操作 ⇒ 必须二次确认。
  //	确认框里要说清后果（服务会重启、面板会短暂断开），
  //	而不是一句干巴巴的"确定吗"。
  function applyUpdate() {
    if (!updateState.hasUpdate) return;
    openConfirm(
      '更新到 v' + updateState.latest + '？',
      '<p>将下载新版本、<b>校验 SHA256</b>，通过后替换当前程序并<b>自动重启服务</b>。</p>'
      + '<p>重启期间面板会短暂断开，之后请刷新页面。</p>'
      + '<p class="st-danger-line">校验不通过会中止更新，不会留下半成品。</p>',
      '确认更新',
      function () {
        updateState.busy = true;
        var btn = document.getElementById('btn-st-update-apply');
        if (btn) btn.disabled = true;
        setText('st-update-result', '正在下载并校验，请勿关闭…');
        showUpdateProgress(true);

        return fetchJSON('/api/update/apply', { method: 'POST' })
          .then(function (d) {
            setText('st-update-result', '✅ ' + ((d && d.message) || '更新完成'));
            // 服务即将重启：提示用户稍后刷新。
            setText('st-update-msg', '服务正在重启，请稍候刷新页面…');
          })
          .catch(function (e) {
            updateState.busy = false;
            showUpdateProgress(false);
            if (btn) btn.disabled = false;
            setText('st-update-result', '⚠️ 更新失败：' + ((e && e.message) || '未知错误'));
            console.error(e);
          });
      }
    );
  }

  // ── 映射表 ──

  function openAliasForm() {
    var form = document.getElementById('st-alias-form');
    if (!form) return;
    form.classList.remove('st-hidden');
    setText('st-alias-err', '');
    var a = document.getElementById('st-alias-name');
    var t = document.getElementById('st-alias-target');
    if (a) { a.value = ''; a.disabled = false; }
    if (t) { t.value = ''; t.disabled = false; }
    var saveBtn = document.getElementById('btn-st-alias-save');
    if (saveBtn) saveBtn.disabled = false;
    loadModels();   // 展开表单时顺手拉一次模型目录（已加载过则不重复请求）
    if (a) a.focus();
  }

  function closeAliasForm() {
    var form = document.getElementById('st-alias-form');
    if (form) form.classList.add('st-hidden');
    setText('st-alias-err', '');
  }

  // 本地校验只做"能明显看出错"的几条；真正的合法性（能否解析、是否循环）
  // 由后端 router 层判定（契约 §4），失败会以错误消息回到这里。
  function validateAlias(name, target) {
    if (!name) return '别名不能为空';
    if (name.indexOf('/') >= 0) return '别名不能包含 “/”（避免与 workbuddy/ 前缀混淆）';
    if (name.length > 64) return '别名长度不能超过 64';
    if (!target) return '目标模型不能为空';

    var map = lastSettings ? aliasesOf(lastSettings) : {};
    if (name === target) return '禁止自映射（' + name + ' → ' + name + '）';
    if (Object.prototype.hasOwnProperty.call(map, name)) {
      return '别名 “' + name + '” 已存在 —— 请先删除原条目（面板不做原地修改）';
    }
    // 只允许一层解析：目标必须是真实模型，不能指向另一个别名
    if (Object.prototype.hasOwnProperty.call(map, target)) {
      return '目标 “' + target + '” 本身是别名 —— 只允许一层解析，禁止循环';
    }
    if (modelsState.loaded && modelsState.list.length) {
      var found = false;
      for (var i = 0; i < modelsState.list.length; i++) {
        if (modelsState.list[i] === target) { found = true; break; }
      }
      if (!found) return '目标 “' + target + '” 不在当前可用模型列表里（大小写敏感）';
    }
    return '';
  }

  function addAlias() {
    var nameEl = document.getElementById('st-alias-name');
    var targetEl = document.getElementById('st-alias-target');
    if (!nameEl || !targetEl) return;

    var name = nameEl.value.trim();
    var target = targetEl.value.trim();

    var err = validateAlias(name, target);
    if (err) {
      setText('st-alias-err', err);
      return;
    }
    setText('st-alias-err', '');

    // 只进草稿，不发请求 —— 与左侧「删除」一起等「保存映射」
    aliasDraft.add.push({ name: name, target: target });
    nameEl.value = '';
    targetEl.value = '';
    nameEl.focus();
    renderAliasDraft();
  }

  // 服务端已存在的行：移进待删除草稿
  function stageDeleteAlias(alias) {
    if (!draftHasDel(alias)) aliasDraft.del.push(alias);
    renderAliasDraft();
  }

  // 撤回草稿项
  function undoDraft(kind, idx) {
    var i = parseInt(idx, 10);
    var list = kind === 'del-add' ? aliasDraft.add : aliasDraft.del;
    if (isNaN(i) || i < 0 || i >= list.length) return;
    list.splice(i, 1);
    renderAliasDraft();
  }

  // 「保存映射」= 一次 PATCH 整表替换。
  // 只允许一层解析、禁止循环的【最终判定在后端 router 层】（契约 §4），
  // 这里已经是"服务的当前别名 − 待删除 + 新增"的完整结果表，不会丢条目。
  function saveAliases() {
    if (!aliasDirty()) return;

    var rows = aliasResult();
    var map = {};
    for (var i = 0; i < rows.length; i++) {
      if (rows[i].target === null) continue;   // 待删除
      map[rows[i].name] = rows[i].target;
    }

    save({ aliases: map }, '映射已保存（立即生效）').then(function () {
      // 成功后服务端返回的表已经是最新的，renderSettings 会重绘并把草稿清空
      aliasDraft.clear();
      closeAliasForm();
      renderAliasDraft();
    });
  }

  // ── 二次确认：复用 app.js 的单例 overlay（DOM 契约，不是它的函数）──
  //
  // app.js 的 initModal() 已接管 #overlay 的关闭（点遮罩 / × / Esc）。
  // 注意：它用 innerHTML 填充 #modal-body，所以每次重填都会把里面
  // 老节点上的监听器一起丢掉 —— 每次打开都要重新挂，不能缓存节点。

  function openConfirm(title, bodyHtml, okText, onOk) {
    var overlay = document.getElementById('overlay');
    var modalBody = document.getElementById('modal-body');
    if (!overlay || !modalBody) return;

    modalBody.innerHTML =
      '<h3>' + esc(title) + '</h3>'
      + '<div class="st-confirm">' + bodyHtml + '</div>'
      + '<div class="modal-actions">'
      +   '<button type="button" class="btn" id="st-confirm-ok">' + esc(okText) + '</button>'
      +   '<button type="button" class="btn" id="st-confirm-cancel">取消</button>'
      + '</div>';

    var box = overlay.querySelector('.modal');
    if (box) box.classList.add('modal-sm');
    overlay.classList.add('open');

    var cancel = document.getElementById('st-confirm-cancel');
    if (cancel) cancel.addEventListener('click', closeConfirm);

    var ok = document.getElementById('st-confirm-ok');
    if (ok) {
      ok.addEventListener('click', function () {
        ok.disabled = true;
        Promise.resolve().then(onOk).catch(function (e) {
          ok.disabled = false;
          console.error(e);
        }).then(function () {
          // 操作本身自己的 toast 会给出结果；这里只负责收掉弹窗
          if (!busy) closeConfirm();
        });
      });
    }
  }

  function closeConfirm() {
    var overlay = document.getElementById('overlay');
    var modalBody = document.getElementById('modal-body');
    if (overlay) overlay.classList.remove('open');
    if (modalBody) modalBody.innerHTML = '';
  }

  // ── toast（与 app.js 一致：不引库、纯 DOM）──

  var toastTimer = null;

  // 乐观锁冲突提示（412）。
  //
  // 为什么不自动重试：自动重试 = 自动覆盖，而那正是乐观锁要防的事。
  // 正确做法是把冲突告诉用户、载入服务端最新值，让他重新决定。
  function showConflictNotice(msg) {
    toast(msg, 'bad');
    var el = document.getElementById('st-load-err');
    if (el) {
      el.classList.remove('st-hidden');
      el.innerHTML = joinErr('⚠️ 设置冲突', msg);
    }
  }

  function toast(msg, kind) {
    var el = document.getElementById('st-toast');
    if (!el) return;
    el.className = 'st-toast ' + (kind === 'bad' ? 'st-toast-bad' : 'st-toast-ok');
    el.textContent = msg;
    if (toastTimer) clearTimeout(toastTimer);
    toastTimer = setTimeout(function () { el.className = 'st-toast st-hidden'; }, 2600);
  }

  // ── 事件绑定（只挂一次）──

  function on(id, fn) {
    var el = document.getElementById(id);
    if (el) el.addEventListener('click', fn);
  }

  // 映射表的按钮都在动态渲染的行里，用事件委托（委托挂在容器上，只挂一次）
  function bindRowActions(containerId) {
    var box = document.getElementById(containerId);
    if (!box) return;
    box.addEventListener('click', function (ev) {
      var btn = ev.target && ev.target.closest ? ev.target.closest('button[data-st-act]') : null;
      if (!btn || btn.disabled) return;
      var act = btn.getAttribute('data-st-act');
      if (act === 'del-alias') stageDeleteAlias(btn.getAttribute('data-alias'));
      else if (act === 'del-add' || act === 'del-del') undoDraft(act, btn.getAttribute('data-idx'));
    });
  }

  function initOnce() {
    // 密钥区（新交互：输入框直接编辑 + 保存/清空）
    // 不再有「更换」「取消」「复制」—— 输入框永远在那儿，无需展开；
    // 明文可见，无需复制按钮。
    on('btn-st-key-save', saveKey);
    on('btn-st-key-clear', clearKey);
    bindKeyInput();

    on('btn-st-port-save', savePort);
    on('btn-st-restart', restartServer);

    // 开关：绑在开关元素自身（id 即 st-auto-*），HTML 片段与 CSS 都按这个 id 写
    on('st-auto-checkin', toggleAutoCheckin);
    on('st-auto-start', toggleAutoStart);
    on('st-keepalive', toggleKeepalive);

    // 版本与更新：两个按钮，都只在用户点击时才动（无后台自动更新）
    on('btn-st-update-check', checkUpdate);
    on('btn-st-update-apply', applyUpdate);

    // 「加入草稿」只进本地草稿；「保存映射」才发 PATCH —— 两个按钮职责不同，
    // 必须是两个不同的处理函数（曾把两者都绑到 saveAliases，表现是点「加入草稿」
    // 直接提交、草稿表永远空）。
    on('btn-st-alias-add', openAliasForm);
    on('btn-st-alias-save', addAlias);
    on('btn-st-alias-cancel', closeAliasForm);
    on('btn-st-alias-batch-save', saveAliases);
    // （btn-st-toggle-models 的绑定已随「当前可用模型」折叠表删除。）

    // 映射表两张表都是动态渲染的行，统一用事件委托（只挂一次）
    bindRowActions('st-alias-body');
    bindRowActions('st-alias-draft');

    // 密钥框：Enter 直接保存
    var keyInput = document.getElementById('st-key-input');
    if (keyInput) {
      keyInput.addEventListener('keydown', function (ev) {
        if (ev.key === 'Enter') { ev.preventDefault(); saveKey(); }
      });
    }
    // 别名框：Enter 直接添加
    var aliasInputs = ['st-alias-name', 'st-alias-target'];
    for (var i = 0; i < aliasInputs.length; i++) {
      (function (id) {
        var el = document.getElementById(id);
        if (el) {
          el.addEventListener('keydown', function (ev) {
            if (ev.key === 'Enter') { ev.preventDefault(); addAlias(); }
          });
        }
      })(aliasInputs[i]);
    }

    // 端口输入：实时提示范围错误（不拦输入，只提示）
    var portEl = document.getElementById('st-port');
    if (portEl) {
      portEl.addEventListener('input', function () {
        var v = portEl.value.trim();
        if (v === '') { setText('st-port-err', ''); return; }
        if (!/^\d+$/.test(v)) { setText('st-port-err', '端口必须是数字'); return; }
        var p = Number(v);
        setText('st-port-err', (p < PORT_MIN || p > PORT_MAX)
          ? ('端口范围 ' + PORT_MIN + '–' + PORT_MAX + '（<1024 需要管理员权限）')
          : '');
      });
    }
  }

  // ── 轮询：只在设置页可见时刷新 ──
  //
  // 不写进 app.js 的 pages 表，所以默认【不会】被它的 setInterval 自动刷新；
  // 由本文件自己的定时器负责，并在切页/隐藏时停掉，避免做无用请求。
  //
  // ⚠️ 本函数就是给 app.js 的 pages.settings 用的（切换页时调用一次）。

  function refresh() {
    return loadSettings();
  }

  function activePage() {
    for (var i = 0; i < PAGES.length; i++) {
      var el = document.getElementById('page-' + PAGES[i]);
      if (el && !el.classList.contains('hidden')) return PAGES[i];
    }
    return '';
  }

  function tick() {
    if (document.hidden) return;
    if (activePage() !== 'settings') return;
    if (document.activeElement && document.activeElement.closest
      && document.activeElement.closest('#page-settings')) {
      return;   // 用户正在本页输入/操作：不要打断（也避免覆盖端口输入框）
    }
    refresh();
  }

  function startTick() {
    if (tickTimer) return;
    tickTimer = setInterval(tick, TICK_MS);
  }

  function stopTick() {
    if (tickTimer) { clearInterval(tickTimer); tickTimer = null; }
  }

  // ── 入口 ──

  function init() {
    if (!document.getElementById('page-settings')) return;   // 页面片段未接上：安静退出

    initOnce();

    // 🔴 显式初始化一次"空闲态"（Codex 第 37 轮 A2 第 4 条）。
    //    若本次初始化前有残留的禁用态（例如页面被 bfcache 恢复、
    //    或上一次会话异常结束），这一步保证按钮从一开始就是可点的。
    setBusy(false);

    document.addEventListener('visibilitychange', function () {
      if (document.hidden) { stopTick(); } else { refresh(); startTick(); }
    });

    refresh();
    startTick();
  }

  window.initSettingsPage = function () {
    try {
      // 先取令牌再初始化：所有写操作都要带它（见 fetchJSON 的说明）。
      // 取失败也继续 —— 后端若未启用该防线仍可工作；
      // 后端启用了则写操作会返回明确错误，用户能看到原因。
      loadPanelToken().then(function () {
        try {
          init();
        } catch (e) {
          console.error('initSettingsPage 失败：', e);
        }
      });
    } catch (e) {
      // 设置页出问题绝不能连累 app.js 的初始化
      console.error('initSettingsPage 失败：', e);
    }
  };
})();
