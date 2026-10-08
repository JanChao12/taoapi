/* wbapi 管理面板逻辑 —— 手写，零依赖，零构建
 *
 * 布局：左侧栏三个页（账号管理 / 用量统计 / API 接入）。
 * 轮询：只在「当前激活页 + 页面可见」时拉该页数据（5s）；
 *       visibilitychange 隐藏即停，恢复可见立即拉一次再续定时器。
 */

(function () {
  'use strict';

  var REFRESH_MS = 5000;

  // ── 格式化 ──

  // 大数字转易读：12345 → 1.23w，1234567 → 123.5w
  function fmtNum(n) {
    if (n === null || n === undefined) return '—';
    if (n === 0) return '0';
    if (n < 10000) return String(n);
    return (n / 10000).toFixed(n < 1000000 ? 2 : 1) + 'w';
  }

  function fmtInt(n) {
    if (n === null || n === undefined) return '—';
    return String(n);
  }

  function esc(s) {
    return String(s === null || s === undefined ? '' : s)
      .replace(/&/g, '&amp;').replace(/</g, '&lt;')
      .replace(/>/g, '&gt;').replace(/"/g, '&quot;');
  }

  function todayStr() {
    // 与后端口径一致：UTC+8 墙钟
    var d = new Date(Date.now() + 8 * 3600 * 1000);
    return d.getUTCFullYear() + '-' +
      ('0' + (d.getUTCMonth() + 1)).slice(-2) + '-' +
      ('0' + d.getUTCDate()).slice(-2);
  }

  // 严格早于"今天+7天"即临期（后端 expiring_7d 口径）
  function isExpiringSoon(expireAt) {
    if (!expireAt) return false;
    var t = new Date(expireAt + 'T00:00:00Z');
    if (isNaN(t.getTime())) return false;
    var deadline = new Date(Date.now() + 8 * 3600 * 1000); // UTC+8 的现在
    deadline.setUTCHours(0, 0, 0, 0);
    deadline.setUTCDate(deadline.getUTCDate() + 7);
    return t.getTime() <= deadline.getTime();
  }

  // isCoolingDown 判断账号此刻是否处于限流冷却期。
  //
  // 依据后端给的 cooldown_until（RFC3339，来自 StatusUntil，与调度器同源）。
  // 空/无法解析/已过期 ⇒ 不在冷却 ⇒ 不该显示限流告警。
  //
  // ⚠️ 这里只决定"显不显示"，**不参与调度判断** —— 调度由后端
  //    Selector.Scheduled 独立检查 CooldownUntil（pool.go:285），
  //    前端时间有偏差也不会导致选错账号。
  function isCoolingDown(cooldownUntil) {
    if (!cooldownUntil) return false;
    var t = Date.parse(String(cooldownUntil));
    if (isNaN(t)) return false;
    return t > Date.now();
  }

  // ── 工具 ──

  function setText(id, v) {
    var el = document.getElementById(id);
    if (el) el.textContent = v;
  }

  // 面板写操作令牌（CSRF 主防线，见后端 internal/app/csrf.go）。
  //
  // /api/* 不校验 API key（面板要能在未设 key 时打开），所以需要一道
  // "浏览器跨站请求带不上"的防线 —— 自定义头 X-WBAPI-Panel。
  // 浏览器发跨站请求带自定义头会先触发 CORS 预检，而我们不应答预检，
  // 于是请求被浏览器自己拦下。
  //
  // 账号操作（action / import）也是写操作，同样需要它。
  var panelToken = '';

  function loadPanelToken() {
    return fetch('/api/panel-token', { cache: 'no-store' })
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (d) { if (d && d.token) panelToken = d.token; })
      .catch(function () { /* 降级为不带：后端未启用该防线时仍可用 */ });
  }

  function fetchJSON(url, opts) {
    opts = opts || {};
    if (panelToken) {
      opts.headers = opts.headers || {};
      opts.headers['X-WBAPI-Panel'] = panelToken;
    }
    return fetch(url, opts).then(function (r) {
      if (!r.ok) {
        return r.json().catch(function () { return {}; }).then(function (e) {
          throw new Error((e && e.error && e.error.message) || ('HTTP ' + r.status));
        });
      }
      return r.json();
    });
  }

  // ═══════════════ 页面切换 ═══════════════

  var currentPage = 'accounts'; // accounts | stats | api | settings
  var statsLoaded = false;      // 进程内已拉到过统计数据
  var apiLoaded = false;        // 进程内已拉到过模型目录
  var pages = {};               // page 名 → 该页的加载函数

  function switchPage(name) {
    if (!pages[name]) return;
    currentPage = name;

    var items = document.querySelectorAll('.side-item');
    for (var i = 0; i < items.length; i++) {
      items[i].classList.toggle('active', items[i].getAttribute('data-page') === name);
    }
    var ids = ['accounts', 'stats', 'api', 'settings'];
    for (var j = 0; j < ids.length; j++) {
      var el = document.getElementById('page-' + ids[j]);
      if (el) el.classList.toggle('hidden', ids[j] !== name);
    }

    // 切到某页立即拉一次（不等 5s）
    pages[name]();
    restartPolling();
  }

  function initNav() {
    var items = document.querySelectorAll('.side-item');
    for (var i = 0; i < items.length; i++) {
      items[i].addEventListener('click', function () {
        switchPage(this.getAttribute('data-page'));
      });
    }
  }

  // ═══════════════ 顶栏状态（/status，随任何页轮询） ═══════════════

  var statusLoaded = false;

  function loadStatus() {
    fetchJSON('/status').then(function (d) {
      setText('ver', d.version ? 'v' + d.version : '');
      var prov = (d.providers || []).join(', ') || '未配置';
      // 🔴 运行时间与渠道**分成两行**（2026-10-06 委托方要求。
      //	原来"运行 8m45s · 渠道 workbuddy, workbuddyai"挤一行，
      //	渠道名一长就折断到莫名其妙的位置）。
      setText('uptimeLine', '运行 ' + (d.uptime || '—'));
      setText('serverLine', '渠道 ' + prov);
      statusLoaded = true;
    }).catch(function (e) {
      if (!statusLoaded) {
        setText('uptimeLine', '无法连接服务');
        setText('serverLine', '');
      }
      console.error(e);
    });
  }

  // renderHomeTips 按**实际开关状态**渲染账号页顶部的提示条。
  //
  // 🔴 为什么必须动态（2026-10-06 委托方实测反馈）：
  //
  //	原实现是一句写死在 HTML 里的「✅ 每日自动签到已开启」，
  //	**无论开关是否打开都显示** —— 委托方原话：
  //	  「我明明没开启自动签到，为什么主页显示自动签到已开启，
  //	    只有在我开启时才显示，自动签到和开机自启如果开启了都可以显示在主页提示」
  //
  // 规则：
  //   · 两个都关  → 整块隐藏（不显示任何"已开启"字样）
  //   · 自动签到开 → 绿色提示
  //   · 开机自启开 → 蓝色提示
  //   · 都开      → 两条都显示
  //
  // ⚠️ 数据源：/api/settings 的 autoCheckin / autoStart。
  //	它是**配置值**（用户想不想要），不是"当前是否真的在跑"——
  //	后者是设置页「运行状态」那一行的职责。这里只表达"你开了没"。
  function renderHomeTips() {
    var box = document.getElementById('home-tips');
    if (!box) return;

    fetchJSON('/api/settings').then(function (d) {
      d = d || {};
      var tips = [];
      if (d.autoCheckin === true) {
        tips.push('<div class="tip tip-green">✅ 自动签到已开启 —— ' +
          '服务运行期间自动签到，无需配置</div>');
      }
      if (d.autoStart === true) {
        tips.push('<div class="tip tip-blue">✅ 开机自启已开启 —— ' +
          '登录 Windows 后自动启动服务</div>');
      }
      if (!tips.length) {
        box.innerHTML = '';
        box.classList.add('hidden');
        return;
      }
      box.innerHTML = tips.join('');
      box.classList.remove('hidden');
    }).catch(function (e) {
      // 读不到设置就**不显示**任何"已开启"——宁可少提示，
      // 也不能像原来那样无条件宣称开着（那是误导）。
      box.innerHTML = '';
      box.classList.add('hidden');
      console.error(e);
    });
  }

  // ═══════════════ 账号管理页 ═══════════════

  var ACCT_BADGE = {
    normal: 'badge-normal',             // 绿
    rate_limited: 'badge-rate_limited', // 橙
    banned: 'badge-banned',             // 红
    auth_expired: 'badge-auth_expired', // 红
    disabled: 'badge-disabled',         // 灰
    no_credit: 'badge-no_credit',       // 黄
    transient: 'badge-transient',       // 灰蓝
    unknown: 'badge-transient'          // 灰蓝（unknown 与 transient 同色）
  };

  var acctCache = [];    // 最近一次 /api/accounts 的账号数组
  var acctBusy = false;  // 批量操作进行中，防重入

  function loadAccts() {
    renderHomeTips();   // 顶部提示条随账号页刷新（开关可能在设置页被改过）
    return fetchJSON('/api/accounts').then(function (d) {
      acctCache = d.accounts || [];
      renderAccts(acctCache);
    }).catch(function (e) {
      // 静默：保留上次内容
      console.error(e);
    });
  }

  function renderAccts(list) {
    var wrap = document.getElementById('acct-cards');
    if (!wrap) return;

    setText('acct-count', String(list.length));

    if (!list.length) {
      wrap.innerHTML = '<div class="empty" style="flex:1 1 100%;text-align:center;color:#b0b6bd;padding:22px;">暂无账号 —— 点右上「导入账号」粘贴 JSON 添加</div>';
      return;
    }

    var today = todayStr();
    var html = '';
    for (var i = 0; i < list.length; i++) {
      html += renderAcctCard(list[i], today);
    }
    wrap.innerHTML = html;
  }

  function renderAcctCard(a, today) {
    var st = String(a.status || 'normal');
    var badgeCls = ACCT_BADGE[st] || 'badge-transient';
    var checkedIn = a.checkin_day === today;

    // ── 额度行：大字总额度 + N 个积分包 + 最近更新时间 ──
    var known = a.credits_known && a.credits !== null && a.credits !== undefined;
    var creditHtml = known
      ? '<span class="acct-credit">' + esc(String(a.credits)) + '</span>'
      : '<span class="acct-credit acct-credit-none">—</span>';
    var pkgCount = (a.packages || []).length;
    var pkgCountHtml = pkgCount > 0
      ? '<span class="acct-pkg-count">' + pkgCount + ' 个积分包</span>'
      : '';
    var creditAt = a.credit_at
      ? '<span class="acct-credit-at">更新于 ' + esc(String(a.credit_at).replace('T', ' ').slice(0, 19)) + '</span>'
      : '';

    // ── 最近到期的 2 个包（expired 的排最后；一行：名称、剩余、到期日）──
    var pkgs = (a.packages || []).slice();
    pkgs.sort(function (x, y) {
      var a1 = x.expired ? 1 : 0, b1 = y.expired ? 1 : 0;
      if (a1 !== b1) return a1 - b1;
      var dx = Date.parse(x.expire_at || '') || Infinity;
      var dy = Date.parse(y.expire_at || '') || Infinity;
      return dx - dy;
    });
    var recent = pkgs.slice(0, 2);

    var pkgHtml = '';
    if (pkgCount > 0) {
      pkgHtml += '<div class="acct-pkgs2">';
      for (var i = 0; i < recent.length; i++) {
        pkgHtml += pkgRowHtml(recent[i]);
      }
      pkgHtml += '<button type="button" class="pkg-more" data-uid="' + esc(a.uid) + '">查看全部 ' + pkgCount + ' 个积分包 →</button>';
      pkgHtml += '</div>';
    }

    // ── 上游限流告警 ──
    //
    // 🔴 显示规则的唯一依据是"**此刻是否真的在冷却**"，不是 last_error 有没有值。
    //
    //   为什么（2026-10-06 委托方实测抓到的矛盾）：
    //     后端 EffectiveStatus 是**惰性恢复** —— 冷却到期后状态回到 normal，
    //     但 StatusReason / last_error 被【刻意保留】作历史记录
    //     （auth/account.go:149 注释：「保留 StatusReason 供面板显示'上次为何异常'」）。
    //     于是卡片会同时出现「正常」「⚡反代使用中」「上游限流」三个标签，
    //     而调度器其实已经重新启用它了 —— 用户看到的就是自相矛盾。
    //     委托方原话：「本就是逻辑互斥……这两个肯定有一个显示错了」。
    //     ⇒ 他判断正确：错的是这条红字（它是历史，不是当前状态）。
    //
    //   修法（Codex 第 37 轮 B2 裁定）：冷却中才显示红色；
    //     冷却已过 ⇒ **完全隐藏**（卡片只表达当前状态，历史不该冒充告警）。
    //     这样也不可能再出现"红字 + 使用中"——因为冷却中的账号
    //     不会被调度器选中（pool.go:285 独立检查 CooldownUntil）。
    //
    //   冷却截止时间来自后端 cooldown_until（RFC3339），与调度器同源；
    //   前端只用它决定"显不显示"，不参与调度判断。
    var cooling = isCoolingDown(a.cooldown_until);
    var noteHtml = cooling
      ? '<span class="acct-err" title="上游限流中，暂时不会被选中' +
        (a.cooldown_until ? '；冷却至 ' + esc(String(a.cooldown_until).replace('T', ' ').slice(0, 19)) : '') +
        '">上游限流</span>'
      : '';

    // ── 按钮 ──
    var toggle = a.manual_disabled
      ? '<button type="button" data-act="enable" data-uid="' + esc(a.uid) + '">启用</button>'
      : '<button type="button" data-act="disable" data-uid="' + esc(a.uid) + '">禁用</button>';

    // 🔴 国际版**不显示签到按钮**（2026-10-06 实测）。
    //
    //	国际版上游没有签到活动：调用不报错但返回"活动未开启或已过期"。
    //	继续显示按钮，用户点了只会看到"成功"却毫无效果 —— 那是误导。
    //	（后端也会拒绝该动作，这里是前端就不给入口，避免无谓往返。）
    var checkinBtn = '';
    if (a.platform !== 'intl') {
      checkinBtn = checkedIn
        ? '<button type="button" data-act="checkin" data-uid="' + esc(a.uid) + '" disabled class="checked-ok">✓ 今日已签</button>'
        : '<button type="button" data-act="checkin" data-uid="' + esc(a.uid) + '">签到</button>';
    }

    // ── 平台徽标（多平台下必须能一眼区分账号属于哪边）──
    var platHtml = a.platform_label
      ? '<span class="plat-tag' + (a.platform === 'intl' ? ' plat-intl' : '') + '"' +
        ' title="该账号属于' + esc(a.platform_label) + '，只会在同一平台的请求中被使用">' +
        esc(a.platform_label) + '</span>'
      : '';

    // ── 当前反代是否用这个号（与后端调度同源，见 /api/accounts 的 in_use）──
    var inUseHtml = a.in_use
      ? '<span class="badge badge-inuse" title="当前反代请求会优先使用这个账号">' +
        '⚡ 反代使用中</span>'
      : '';

    return '<div class="acct-card' + (a.manual_disabled ? ' acct-card-disabled' : '')
      + (a.in_use ? ' acct-card-inuse' : '') + '">'
      + '<div class="acct-head">'
      +   platHtml
      +   '<span class="acct-name">' + esc(a.nickname || a.uid) + '</span>'
      +   noteHtml
      +   inUseHtml
      +   '<span class="badge ' + badgeCls + '">' + esc(a.status_label || st) + '</span>'
      + '</div>'
      + '<div class="acct-credit-row">' + creditHtml + pkgCountHtml + creditAt + '</div>'
      + pkgHtml
      + '<div class="acct-actions">'
      +   toggle
      +   '<button type="button" data-act="refresh" data-uid="' + esc(a.uid) + '">刷新</button>'
      +   checkinBtn
      +   '<button type="button" class="acct-del" data-act="remove"'
      +     ' data-uid="' + esc(a.uid) + '"'
      +     ' data-name="' + esc(a.nickname || a.uid) + '">删除</button>'
      + '</div>'
      + '</div>';
  }

  // 卡片上的一行包（紧凑：名称、剩余、到期日；临期橙 / 过期红）
  function pkgRowHtml(p) {
    var soon = !p.expired && isExpiringSoon(p.expire_at);
    var remainCls = p.expired ? ' pkg-expired-txt' : (soon ? ' pkg-soon-txt' : '');
    var expHtml = p.expire_at ? esc(p.expire_at) : '—';
    if (p.expired) expHtml += ' <span class="pkg-tag expired">已过期</span>';
    else if (soon) expHtml += ' <span class="pkg-tag soon">7天内到期</span>';

    return '<div class="pkg-row' + (p.expired ? ' pkg-row-expired' : '') + '">'
      + '<span class="pkg-name">' + esc(p.name || '未命名') + '</span>'
      + '<span class="pkg-remain' + remainCls + '">' + esc(String(p.remain)) + '</span>'
      + '<span class="pkg-exp">' + expHtml + '</span>'
      + '</div>';
  }

  // 对单个账号执行操作；成功后整体刷新账号区
  function acctAction(uid, action) {
    return fetchJSON('/api/accounts/action', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ uid: uid, action: action })
    });
  }

  function initAcctCards() {
    var wrap = document.getElementById('acct-cards');
    if (!wrap) return;

    // 事件委托：卡片按钮 + 「查看全部」链接
    wrap.addEventListener('click', function (ev) {
      var more = ev.target && ev.target.closest ? ev.target.closest('.pkg-more') : null;
      if (more) {
        var uid = more.getAttribute('data-uid');
        for (var i = 0; i < acctCache.length; i++) {
          if (acctCache[i].uid === uid) {
            openPackagesModal(acctCache[i]);
            break;
          }
        }
        return;
      }

      var btn = ev.target && ev.target.closest ? ev.target.closest('button[data-act]') : null;
      if (!btn || btn.disabled) return;
      var act = btn.getAttribute('data-act');

      // 删除是破坏性且不可逆的操作 —— 走二次确认，不直接执行。
      if (act === 'remove') {
        openRemoveConfirm(btn.getAttribute('data-uid'), btn.getAttribute('data-name'));
        return;
      }

      btn.disabled = true;
      acctAction(btn.getAttribute('data-uid'), act)
        .then(function () { return loadAccts(); })
        .catch(function (e) {
          console.error(e);
          btn.disabled = false;
        });
    });
  }

  // 删除账号的二次确认。
  //
  // 🔴 为什么必须二次确认（委托方 2026-10-06 要求加删除按钮时同时确认）：
  //   账号凭据在本机是**唯一副本**（accounts.json）。删除后要么重新走
  //   网页登录、要么重新导入 JSON —— 都不能靠"再点一下"恢复。
  //   面板里唯一比它更危险的是「清空密钥」，那个也是二次确认。
  //
  // 文案要点：说清"删的是什么"（本地记录，不是上游账号）、
  // "怎么恢复"（重新登录/导入）、"是否影响上游"（不影响）。
  function openRemoveConfirm(uid, name) {
    var who = name || uid;
    openModal(
      '<h3>删除账号？</h3>'
      + '<p>即将删除 <b>' + esc(who) + '</b> 的<b>本地记录</b>。</p>'
      + '<ul class="remove-note">'
      +   '<li>删除后本工具不再使用这个账号，<b>上游账号本身不受影响</b>，'
      +     '你仍可在 CodeBuddy 官网正常使用。</li>'
      +   '<li>本机保存的凭据是唯一副本，删除<b>无法撤销</b> —— '
      +     '要恢复需重新「添加账号」登录或重新导入 JSON。</li>'
      +   '<li>若该账号正在进行网页登录，该登录会被一并取消。</li>'
      + '</ul>'
      + '<div class="modal-actions">'
      +   '<button type="button" class="btn" id="btn-remove-cancel">取消</button>'
      +   '<button type="button" class="btn btn-danger" id="btn-remove-ok">确认删除</button>'
      + '</div>',
      true
    );

    var cancel = document.getElementById('btn-remove-cancel');
    if (cancel) cancel.addEventListener('click', closeModal);

    var ok = document.getElementById('btn-remove-ok');
    if (ok) {
      ok.addEventListener('click', function () {
        ok.disabled = true;
        ok.textContent = '删除中…';
        acctAction(uid, 'remove').then(function () {
          closeModal();
          showAcctNotice('已删除账号 ' + who, 'green');
          return loadAccts();
        }).catch(function (e) {
          ok.disabled = false;
          ok.textContent = '确认删除';
          showAcctNotice('删除失败：' + errText(e), 'danger');
          console.error(e);
        });
      });
    }
  }

  function initAcctBulk() {
    var importBtn = document.getElementById('btn-import');
    var checkinAll = document.getElementById('btn-checkin-all');
    var refreshAll = document.getElementById('btn-refresh-all');

    // 🔴 为什么签到后要自动刷新额度（2026-10-06 委托方要求）：
    //   签到会改变额度/签到状态，但此前签完不会自动刷新，用户得手动再点一次
    //   「刷新额度」才看得到变化 —— 委托方原话「我点击全部签到后你签到完成
    //   应该自动刷新一次额度」。
    //
    // 🔴 为什么必须**串行**（签到返回后再刷新）：
    //   并发发出的话，刷新可能先于签到完成，拿回的是**旧额度** ——
    //   用户会看到"签到成功了但数字没变"，比不刷新更误导。
    //
    // 🔴 为什么两步的失败要**分开报告**：
    //   签到成功但刷新失败时，绝不能整体报失败（那会让用户以为没签到、
    //   再点一次）。本项目纪律是"只报告真实发生的事"。
    function runBulk(action) {
      if (acctBusy) return;
      acctBusy = true;
      if (checkinAll) checkinAll.disabled = true;
      if (refreshAll) refreshAll.disabled = true;
      showAcctNotice('', '');   // 清掉上一轮结果

      var isCheckin = action === 'checkin_all';
      if (isCheckin) showAcctNotice('正在签到…', 'plain');

      postBulk(action).then(function (res) {
        var msg = summarizeBulk(res, isCheckin ? '签到' : '刷新额度');
        if (!isCheckin) {
          showAcctNotice(msg.text, msg.bad ? 'danger' : 'green');
          return null;
        }
        // 签到已完成 —— 如实报告签到结果，然后自动刷新额度
        showAcctNotice(msg.text + '，正在刷新额度…', msg.bad ? 'danger' : 'green');
        return postBulk('refresh_all').then(function (rres) {
          var rmsg = summarizeBulk(rres, '额度刷新');
          // 两段结果都保留：签到那段不被刷新那段覆盖
          showAcctNotice(
            msg.text + '；' + rmsg.text,
            (msg.bad || rmsg.bad) ? 'danger' : 'green'
          );
        }).catch(function (e) {
          // 🔴 签到成功但刷新失败 —— 必须说清"签到是成功的"
          console.error(e);
          showAcctNotice(msg.text + '；但额度刷新失败：' + errText(e) +
            '（可手动点「刷新额度」重试）', 'danger');
        });
      }).catch(function (e) {
        console.error(e);
        showAcctNotice((isCheckin ? '签到失败：' : '刷新失败：') + errText(e), 'danger');
      }).then(function () {
        return loadAccts();
      }).then(function () {
        acctBusy = false;
        if (checkinAll) checkinAll.disabled = false;
        if (refreshAll) refreshAll.disabled = false;
      });
    }

    if (checkinAll) checkinAll.addEventListener('click', function () { runBulk('checkin_all'); });
    if (refreshAll) refreshAll.addEventListener('click', function () { runBulk('refresh_all'); });
    if (importBtn) importBtn.addEventListener('click', openImportModal);
  }

  // 发批量动作请求；返回后端的 actionResult。
  function postBulk(action) {
    return fetchJSON('/api/accounts/action', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ action: action })
    });
  }

  function errText(e) {
    return (e && e.message) ? String(e.message) : '未知错误';
  }

  // 把后端的 actionResult 压成一句人话。
  //
  // 后端返回 { ok, total, success, failed, results:[{uid,nickname,ok,message}] }。
  // 为什么要自己拼而不是直接显示 message：委托方要看的是"几个成功几个失败"，
  // 失败时还要知道**是哪几个账号、为什么**。
  //
  // label 由调用方传入 —— 响应体里【没有】能区分签到/刷新的字段，
  // 从返回值猜会猜错（两者形状完全相同）。
  function summarizeBulk(res, label) {
    var total = (res && res.total) || 0;
    var okN = (res && res.success) || 0;
    var badN = (res && res.failed) || 0;

    if (total === 0) return { text: label + '：没有可操作的账号', bad: false };

    var text = label + '：成功 ' + okN + ' 个' + (badN ? '，失败 ' + badN + ' 个' : '');
    if (badN) {
      var names = [];
      var rs = (res && res.results) || [];
      for (var i = 0; i < rs.length && names.length < 3; i++) {
        if (!rs[i].ok) names.push((rs[i].nickname || rs[i].uid || '?') +
          (rs[i].message ? '（' + rs[i].message + '）' : ''));
      }
      if (names.length) text += '：' + names.join('；');
    }
    return { text: text, bad: badN > 0 };
  }

  // showAcctNotice 显示批量操作结果；text 为空时隐藏。
  //
  // 复用 settings.css 里已有的 st-tip-* 与 st-hidden（面板共用一个样式表
  // 命名空间，不再新增 CSS 类 —— 少一处重复定义）。
  function showAcctNotice(text, tone) {
    var el = document.getElementById('acct-notice');
    if (!el) return;
    if (!text) { el.classList.add('st-hidden'); el.textContent = ''; return; }
    el.textContent = text;
    el.className = 'tip ' + (tone === 'plain' ? 'st-tip-plain'
      : tone === 'danger' ? 'st-tip-danger' : 'tip-green');
  }

  // ═══════════════ 模态（单例 overlay） ═══════════════

  var overlayEl = null;
  var modalBodyEl = null;

  function initModal() {
    overlayEl = document.getElementById('overlay');
    modalBodyEl = document.getElementById('modal-body');
    if (!overlayEl) return;

    // 点遮罩（overlay 自身）关闭；点 modal 内部不关
    overlayEl.addEventListener('click', function (ev) {
      if (ev.target === overlayEl) closeModal();
    });
    var closeBtn = document.getElementById('modal-close');
    if (closeBtn) closeBtn.addEventListener('click', closeModal);
    document.addEventListener('keydown', function (ev) {
      if (ev.key === 'Escape') closeModal();
    });
  }

  function openModal(html, small) {
    if (!overlayEl || !modalBodyEl) return;
    modalBodyEl.innerHTML = html;
    var box = overlayEl.querySelector('.modal');
    if (box) box.classList.toggle('modal-sm', !!small);
    overlayEl.classList.add('open');
  }

  function closeModal() {
    // 关模态时一定要停掉登录轮询。
    //
    // 放在这里而不是"只在取消按钮里停"：模态有**三条**关闭路径
    // （Esc 键、点遮罩、关闭按钮），任何一条漏掉都会让轮询在后台
    // 永久空转。这里统一兜住。
    stopLoginPoll();
    if (overlayEl) overlayEl.classList.remove('open');
    if (modalBodyEl) modalBodyEl.innerHTML = '';
  }

  // ── 「查看全部积分包」模态 ──

  function openPackagesModal(a) {
    var pkgs = (a.packages || []).slice();
    // 与卡片一致：未过期在前（按到期日升序），已过期垫底
    pkgs.sort(function (x, y) {
      var a1 = x.expired ? 1 : 0, b1 = y.expired ? 1 : 0;
      if (a1 !== b1) return a1 - b1;
      var dx = Date.parse(x.expire_at || '') || Infinity;
      var dy = Date.parse(y.expire_at || '') || Infinity;
      return dx - dy;
    });

    // 进度条分母：全部包里最大的 remain
    var maxRemain = 0;
    for (var i = 0; i < pkgs.length; i++) {
      if (pkgs[i].remain > maxRemain) maxRemain = pkgs[i].remain;
    }

    var html = '<h3>' + esc(a.nickname || a.uid) + ' · 全部积分包（' + pkgs.length + '）</h3>';
    if (!pkgs.length) {
      html += '<div class="empty" style="text-align:center;color:#b0b6bd;padding:22px;">暂无积分包数据</div>';
    } else {
      html += '<ul class="pkgs-list">';
      for (var j = 0; j < pkgs.length; j++) {
        html += pkgModalItem(pkgs[j], maxRemain);
      }
      html += '</ul>';
    }
    openModal(html);
  }

  function pkgModalItem(p, maxRemain) {
    var soon = !p.expired && isExpiringSoon(p.expire_at);
    var pct = maxRemain > 0 ? Math.max(2, Math.round(p.remain / maxRemain * 100)) : 0;

    var tags = '';
    if (p.expired) tags += ' <span class="pkg-tag expired">已过期</span>';
    else if (soon) tags += ' <span class="pkg-tag soon">7天内到期</span>';

    return '<li>'
      + '<div class="pkg-line">'
      +   '<span class="p-name">' + esc(p.name || '未命名') + '</span>'
      +   '<span class="p-remain">' + esc(String(p.remain)) + '</span>'
      +   '<span class="p-exp">到期 ' + esc(p.expire_at || '—') + tags + '</span>'
      + '</div>'
      + '<div class="pkg-progress"><i style="width:' + pct + '%"></i></div>'
      + '</li>';
  }

  // ── 「添加账号」模态 ──
  //
  // ⚠️ 函数名仍叫 openImportModal（按钮 id 也仍是 btn-import）——
  //	改名的收益只是好看，而 id 被后端测试与面板快照引用，
  //	改动会带来无谓的连锁修改。语义已从"导入"变成"添加账号"。

  function openImportModal() {
    openModal(
      '<h3>添加账号</h3>'
      // ═══════════════════════════════════════════════════════════════
      // 平台选择（2026-10-08 新增）—— 必须先选平台再登录
      // ═══════════════════════════════════════════════════════════════
      //
      // 🔴 为什么必须有这一步：
      //
      //	国内版与国际版是**两套账号体系**（不同域名、不同站点）。
      //	改造前登录页写死 www.codebuddy.cn ⇒ 国际版用户**根本无法添加**。
      //	而程序无法替用户"自动判断"：他还登录，我们无从知道他要绑哪个平台。
      //
      // 平台选择放在最上面：它是后续两种方式（网页登录 / 粘贴凭据）的**前提**。
      // 只有"网页登录"真的需要它（粘贴方式能从 domain 字段自动识别平台），
      // 但放在共同位置更好理解，也避免用户事后才发现进错了站点。
      + '<div class="import-guide">'
      +   '<b>第一步：选择要添加哪个平台的账号</b>'
      +   '<div class="platform-picker" id="platform-picker">'
      +     '<label class="platform-opt"><input type="radio" name="login-platform" value="cn" checked>'
      +       '<span><b>国内版</b><span class="platform-host">www.codebuddy.cn</span></span></label>'
      +     '<label class="platform-opt"><input type="radio" name="login-platform" value="intl">'
      +       '<span><b>国际版</b><span class="platform-host">www.workbuddy.ai</span></span></label>'
      +   '</div>'
      +   '<p class="import-guide-note">选哪个平台，就会打开<b>那个平台</b>的官方登录页。'
      +     '两者账号不通用，请按你实际持有账号的平台选择。</p>'
      + '</div>'
      // ═══════════════════════════════════════════════════════════════
      // 只有「网页登录」一种方式（2026-10-09 委托方要求）
      // ═══════════════════════════════════════════════════════════════
      //
      // 🔴 委托方原话：「方式二：粘贴凭据，取消这个登录方式，太不方便了。」
      //
      //	改造前这里有并列的"方式二：粘贴凭据"（要求用户去装官方客户端、
      //	翻本机凭据文件、手工复制 accessToken）。委托方明确要求删掉它 ——
      //	网页登录已经是完整的替代路径，粘贴方式纯属负担。
      //
      // ⚠️ 删除的范围**只有面板 UI**：
      //	· 后端 `/api/accounts/import` 保留（CLI `wbapi auth import` 仍在用，
      //	  且它也是"网页登录失败时的退路"）
      //	· 因此 app.js 里的 flattenCredential / doImport 一并移除 ——
      //	  它们只服务于这个已删除的输入框，留着就是死代码。
      //
      // ⚠️ 本面板**不会**去自动扫描本机客户端目录（Codex 第 7 轮 C4 的
      //	招牌否决仍有效：那会把外部项目的文件格式变成隐性依赖，
      //	也容易误读 refresh token）。用户想自己粘贴，请走 CLI。
      + '<div class="import-guide">'
      +   '<b>网页登录</b>'
      +   '<p>点下面的按钮会打开一个浏览器窗口，用<b>你要绑定的那个账号</b>正常登录即可，'
      +     '登录成功后凭据会自动保存，无需手动复制任何东西。</p>'
      +   '<p class="import-guide-note">需要本机已安装 Chrome 或 Edge。'
      +     '登录期间会临时启动一个<b>独立</b>的浏览器实例（与你日常用的浏览器互不影响），'
      +     '<b>登录结束后窗口会保留</b>，你可以确认登录结果再自己关掉。</p>'
      +   '<div class="modal-actions" style="margin-top:10px;">'
      +     '<button type="button" class="btn btn-primary" id="btn-web-login">网页登录</button>'
      +     '<span class="modal-hint" id="login-hint">将打开浏览器窗口</span>'
      +   '</div>'
      +   '<div class="login-progress" id="login-progress" style="display:none;">'
      +     '<div class="login-bar"><div class="login-bar-fill" id="login-bar-fill"></div></div>'
      +     '<div class="login-status" id="login-status">准备中…</div>'
      +     '<div class="modal-actions">'
      +       '<button type="button" class="btn" id="btn-login-cancel">取消</button>'
      +     '</div>'
      +   '</div>'
      + '</div>',
      true
    );

    var loginBtn = document.getElementById('btn-web-login');
    if (loginBtn) loginBtn.addEventListener('click', startWebLogin);

    var cancelBtn = document.getElementById('btn-login-cancel');
    if (cancelBtn) cancelBtn.addEventListener('click', cancelWebLogin);
  }

  // selectedPlatform 读当前选中的平台（"添加账号"弹窗里的单选）。
  //
  // 缺省 cn：与改造前行为一致（旧前端不传这个值时服务端也默认 cn）。
  function selectedPlatform() {
    var el = document.querySelector('input[name="login-platform"]:checked');
    return el ? el.value : 'cn';
  }

  // ═══════════════════════════════════════════════════════════════
  // 网页登录（2026-10-05）
  // ═══════════════════════════════════════════════════════════════
  //
  // 流程：
  //   1. POST /api/accounts/login/start  → 后端启动受控浏览器并打开官方登录页
  //   2. 用户在浏览器里正常登录（前端完全不接触账号密码/验证码）
  //   3. 轮询 GET /api/accounts/login/status 直到 done 或出错
  //   4. 后端在拿到凭据后**先验证可用性再落盘**，前端只展示结果
  //
  // 安全：前端**永远**不会收到 token —— 状态接口只返回阶段、脱敏 uid、
  //       昵称、额度、脱敏错误。这条由后端测试守着。

  var loginPollTimer = null;

  // 阶段 → 进度条百分比 + 文案。
  //
  // 为什么用离散阶段而不是真实百分比：后端并不知道"用户还要多久才登完"，
  // 编造一个平滑增长的假进度条是不诚实的。这里给的是**阶段性**反馈。
  var LOGIN_PHASES = {
    'starting':  { pct: 15, text: '正在启动浏览器…' },
    'waiting':   { pct: 45, text: '请在浏览器窗口中完成登录（可扫码或用手机号）…' },
    'capturing': { pct: 75, text: '已检测到登录，正在验证凭据…' },
    'done':      { pct: 100, text: '登录成功' },
    'failed':    { pct: 100, text: '登录失败' },
    'canceled':  { pct: 100, text: '已取消' }
  };

  function setLoginProgress(phase, extra) {
    var box = document.getElementById('login-progress');
    var fill = document.getElementById('login-bar-fill');
    var status = document.getElementById('login-status');
    if (!box || !fill || !status) return;

    box.style.display = 'block';
    var info = LOGIN_PHASES[phase] || { pct: 10, text: phase || '处理中…' };

    // 失败/取消时进度条标红，成功标绿，进行中标蓝。
    fill.className = 'login-bar-fill';
    if (phase === 'failed') fill.className += ' is-error';
    else if (phase === 'done') fill.className += ' is-ok';

    fill.style.width = info.pct + '%';

    var text = info.text;
    if (extra) text = extra;
    status.textContent = text;
    status.className = 'login-status';
    if (phase === 'failed') status.className += ' is-error';
    else if (phase === 'done') status.className += ' is-ok';
  }

  function stopLoginPoll() {
    if (loginPollTimer) {
      clearInterval(loginPollTimer);
      loginPollTimer = null;
    }
  }

  function startWebLogin() {
    var btn = document.getElementById('btn-web-login');
    var hint = document.getElementById('login-hint');
    if (btn) btn.disabled = true;

    // 🔴 必须把用户选的平台发给服务端：它决定打开哪个登录页，
    //	以及凭据捕获的域名白名单（两者必须一致，否则"登录成功抓不到凭据"）。
    var platform = selectedPlatform();
    if (hint) {
      hint.textContent = '正在启动…（' + (platform === 'intl' ? '国际版 workbuddy.ai' : '国内版 codebuddy.cn') + '）';
    }
    setLoginProgress('starting');

    fetchJSON('/api/accounts/login/start', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ platform: platform })
    })
      .then(function () {
        if (hint) hint.textContent = '浏览器窗口已打开';
        setLoginProgress('waiting');
        stopLoginPoll();
        // 2 秒轮询：登录本身是分钟级操作，轮询太密没意义。
        loginPollTimer = setInterval(pollLoginStatus, 2000);
      })
      .catch(function (e) {
        if (hint) hint.textContent = '';
        setLoginProgress('failed', (e && e.message) || '启动失败');
        if (btn) btn.disabled = false;
      });
  }

  function pollLoginStatus() {
    fetchJSON('/api/accounts/login/status')
      .then(function (st) {
        var terminal = (st.phase === 'done' || st.phase === 'failed' ||
                        st.phase === 'canceled' || st.phase === 'idle');

        if (!terminal) {
          setLoginProgress(st.phase);
          return;
        }

        // 终态：停轮询、解锁按钮、刷新账号列表。
        stopLoginPoll();
        var btn = document.getElementById('btn-web-login');
        var hint = document.getElementById('login-hint');
        if (btn) btn.disabled = false;
        if (hint) hint.textContent = '';

        if (st.ok) {
          var msg = '登录成功';
          if (st.nickname) msg += '：' + st.nickname;
          if (typeof st.credits === 'number') msg += '（' + st.credits + ' 可用积分）';
          setLoginProgress('done', msg);
          // 刷新账号卡片与额度（复用现有页面的加载函数）。
          if (pages.accounts) pages.accounts();
        } else if (st.phase === 'canceled') {
          setLoginProgress('canceled', '登录已取消');
        } else if (st.phase === 'idle') {
          // 流程已被清理但没给出结果 —— 极少见（例如服务刚好重启）。
          setLoginProgress('failed', '登录流程已结束，但未取到结果。请重试。');
        } else {
          setLoginProgress('failed', st.error || '登录失败');
        }
      })
      .catch(function (e) {
        // 状态查询失败不立刻判死 —— 可能是瞬时问题，下一轮再试。
        // 只在"连不上服务"这种明确情况下停掉轮询，避免无限空转。
        if (e && e.message && /Failed to fetch|NetworkError|HTTP 5/i.test(e.message)) {
          stopLoginPoll();
          var btn = document.getElementById('btn-web-login');
          if (btn) btn.disabled = false;
          setLoginProgress('failed', '无法连接到服务，请确认面板服务仍在运行');
        }
      });
  }

  function cancelWebLogin() {
    fetchJSON('/api/accounts/login/cancel', { method: 'POST' })
      .then(function () {
        stopLoginPoll();
        setLoginProgress('canceled', '登录已取消');
        var btn = document.getElementById('btn-web-login');
        if (btn) btn.disabled = false;
      })
      .catch(function (e) {
        setLoginProgress('failed', (e && e.message) || '取消失败');
      });
  }

  // ── 说明：原先的「粘贴凭据」导入 UI 已于 2026-10-09 按委托方要求删除 ──
  //
  // 原话：「方式二：粘贴凭据，取消这个登录方式，太不方便了。」
  //
  // 连带删掉了只服务于它的 flattenCredential / doImport / renderImportResult。
  //
  // ⚠️ 后端 `/api/accounts/import` **保留**：CLI `wbapi auth import` 仍在用，
  //    它也是"网页登录不可用（没装 Chrome/Edge）时的退路"。
  //    需要粘贴凭据的用户请走 CLI —— 面板不再提供这个入口。


  // ═══════════════ 用量统计页 ═══════════════

  var currentDays = 1;

  // ── 共用的分页（三个面板各一份状态）──
  //
  // 🔴 澄清（2026-10-06 委托方纠正我的理解）：
  //
  //	他要的"分组"是**三个面板互斥切换**（模型用量 / 账号用量 / 调用记录），
  //	**不是**国内版/国际版那种平台筛选。
  //	第一版我按平台做了，是理解错了。
  //	平台筛选对这三张表价值有限：表里已经带平台标签（国际版紫色），
  //	而"看某个平台的用量"不是高频需求。
  var PAGE_SIZE = 15;

  // pagerState 保存三个列表各自的分页状态。
  var pagerState = {
    models: { page: 1 },
    accts:  { page: 1 },
    log:    { page: 1 }
  };

  // pageSlice 取当前页的数据，并返回分页元信息。
  //
  // 🔴 "当前页超出范围"要自动回退：数据变少时，
  //	原来停在第 3 页的用户会看到空表。这里自动夹到最后一页。
  function pageSlice(rows, page) {
    var total = rows.length;
    var pages = Math.max(1, Math.ceil(total / PAGE_SIZE));
    var p = Math.min(Math.max(1, page), pages);
    var start = (p - 1) * PAGE_SIZE;
    return {
      rows: rows.slice(start, start + PAGE_SIZE),
      page: p,
      pages: pages,
      total: total
    };
  }

  // renderPagerInto 渲染分页条。
  //
  // 🔴 只在需要时显示：一页装得下就不显示分页条（那是噪音）。
  function renderPagerInto(elId, info, onGo) {
    var el = document.getElementById(elId);
    if (!el) return;
    if (info.pages <= 1) {
      el.innerHTML = '';
      el.classList.add('st-hidden');
      return;
    }
    el.classList.remove('st-hidden');

    var html = '';
    html += '<button type="button" class="pg-btn" data-pg="' + (info.page - 1) + '"' +
      (info.page <= 1 ? ' disabled' : '') + '>上一页</button>';
    html += '<span class="pg-info">第 ' + info.page + ' / ' + info.pages +
      ' 页（共 ' + info.total + ' 条）</span>';
    html += '<button type="button" class="pg-btn" data-pg="' + (info.page + 1) + '"' +
      (info.page >= info.pages ? ' disabled' : '') + '>下一页</button>';
    el.innerHTML = html;

    if (!el.getAttribute('data-bound')) {
      el.setAttribute('data-bound', '1');
      el.addEventListener('click', function (ev) {
        var btn = ev.target && ev.target.closest ? ev.target.closest('.pg-btn') : null;
        if (!btn || btn.disabled) return;
        var n = parseInt(btn.getAttribute('data-pg'), 10);
        if (!isNaN(n)) onGo(n);
      });
    }
  }

  // ── 三个面板的互斥切换 ──
  //
  // 一次只显示一个。默认显示「调用记录」（委托方要求）。
  var activeStatPane = 'log';

  function switchStatPane(name) {
    if (!name) return;
    activeStatPane = name;

    var panes = { log: 'pane-log', model: 'pane-model', acct: 'pane-acct' };
    for (var k in panes) {
      if (!Object.prototype.hasOwnProperty.call(panes, k)) continue;
      var el = document.getElementById(panes[k]);
      if (el) el.classList.toggle('hidden', k !== name);
    }

    var tabs = document.querySelectorAll('#stats-tabs .panel-tab');
    for (var i = 0; i < tabs.length; i++) {
      tabs[i].classList.toggle('active', tabs[i].getAttribute('data-stat') === name);
    }
  }

  function initStatsTabs() {
    var nav = document.getElementById('stats-tabs');
    if (!nav) return;
    nav.addEventListener('click', function (ev) {
      var btn = ev.target && ev.target.closest ? ev.target.closest('.panel-tab') : null;
      if (!btn) return;
      switchStatPane(btn.getAttribute('data-stat'));
    });
  }

  // 三个列表的原始数据缓存（翻页基于它，不重复请求）。
  var statsModelsRaw = [];
  var statsAcctsRaw = [];
  var usageLogRaw = [];

  function loadStats() {
    return fetchJSON('/api/stats?days=' + currentDays).then(function (d) {
      statsLoaded = true;
      renderStats(d);
    }).catch(function (e) {
      var body = document.getElementById('model-body');
      if (body && !statsLoaded) body.innerHTML = '<tr><td colspan="8" class="empty">加载失败</td></tr>';
      console.error(e);
    });
  }

  function renderStats(d) {
    var t = d.total || {};

    setText('s-requests', fmtInt(t.requests));
    var failedEl = document.getElementById('s-failed');
    if (failedEl) {
      failedEl.textContent = t.failed > 0 ? ('失败 ' + t.failed + ' 次') : '';
    }

    setText('s-prompt', fmtNum(t.prompt_tokens));
    setText('s-completion', fmtNum(t.completion_tokens));
    setText('s-total', fmtNum(t.total_tokens));
    setText('s-credit', d.credits === null || d.credits === undefined ? '—' : Number(d.credits).toFixed(2));

    // 🔴 数据不完整时必须显式提示（Codex 第 17 轮要求）。
    //
    // 读取端遇到坏行会停止（fail-closed），此时统计**只包含坏行之前的部分**。
    // 若不提示，用户只看到数字变小却不知道原因 —— 比直接报错更误导。
    // 写入不可用（非 Windows 没有文件锁）同理。
    var banner = document.getElementById('stats-warn');
    if (banner) {
      var warns = [];
      if (d.truncated === true) {
        warns.push('部分用量记录损坏，已跳过'
          + (d.truncated_at ? '（' + d.truncated_at + '）' : '')
          + '，以下统计仅包含可读部分');
      }
      if (d.usage_writable === false) {
        warns.push(d.usage_note || '用量写入不可用，统计不会更新');
      }
      if (warns.length) {
        banner.textContent = '⚠️ ' + warns.join('；');
        banner.classList.remove('hidden');
      } else {
        banner.textContent = '';
        banner.classList.add('hidden');
      }
    }

    renderModels(d.models || []);
    renderAcctUsage(d.accounts || []);
    renderHit(t);
  }

  // renderModels 渲染模型用量（带分页）。
  //
  // 委托方要求：「模型用量、账号用量、调用记录……都要做分页处理，
  //	一页最多显示15个记录」+「都设置分组」。
  function renderModels(list) {
    statsModelsRaw = list || [];
    drawModels();
  }

  function drawModels() {
    var body = document.getElementById('model-body');
    if (!body) return;
    var st = pagerState.models;

    var info = pageSlice(statsModelsRaw, st.page);
    st.page = info.page;   // 夹取后的页码要写回（否则翻页按钮状态会错）

    renderPagerInto('model-pager', info, function (n) {
      st.page = n;
      drawModels();
    });

    if (!info.rows.length) {
      body.innerHTML = '<tr><td colspan="7" class="empty">暂无数据</td></tr>';
      return;
    }

    var html = '';
    for (var i = 0; i < info.rows.length; i++) {
      var m = info.rows[i];
      // 缓存命中率由后端算好（cache_hit_rate）。
      //
      // 🔴 null 表示**无样本**（该模型还没被调用过），显示 —；
      //    0 表示**真实 0%**（调用过但一次都没命中），显示 0.0%。
      //    两者含义完全不同 —— 把"没调用过"显示成 0% 会让人
      //    以为缓存完全没生效（Codex 第 40 轮指出，后端已用 null 区分）。
      var rate = m.cache_hit_rate;
      var hitHtml = (rate === null || rate === undefined)
        ? '—'
        : (Number(rate) * 100).toFixed(1) + '%';
      html += '<tr>'
        + '<td class="mono">' + esc(m.model) + '</td>'
        + '<td class="num">' + fmtInt(m.requests) + '</td>'
        + '<td class="num">' + fmtNum(m.prompt_tokens) + '</td>'
        + '<td class="num">' + fmtNum(m.completion_tokens) + '</td>'
        // 合计：委托方 2026-10-09 要求「输入输出后面都加一个合计」。
        // 直接用后端的 total_tokens（与顶部卡片同口径），不前端相加 ——
        // 相加会掩盖"上游没给 usage"的差异（那种情况 token 记 0）。
        + '<td class="num">' + fmtNum(m.total_tokens) + '</td>'
        + '<td class="num">' + hitHtml + '</td>'
        + '<td class="num">' + (m.credits === null || m.credits === undefined ? '—' : Number(m.credits).toFixed(2)) + '</td>'
        + '</tr>';
    }
    body.innerHTML = html;
  }

  function renderAcctUsage(list) {
    statsAcctsRaw = list || [];
    drawAcctUsage();
  }

  function drawAcctUsage() {
    var body = document.getElementById('acct-body');
    if (!body) return;
    var st = pagerState.accts;

    var info = pageSlice(statsAcctsRaw, st.page);
    st.page = info.page;

    renderPagerInto('acct-pager', info, function (n) {
      st.page = n;
      drawAcctUsage();
    });

    if (!info.rows.length) {
      body.innerHTML = '<tr><td colspan="7" class="empty">暂无数据</td></tr>';
      return;
    }

    var html = '';
    for (var i = 0; i < info.rows.length; i++) {
      var a = info.rows[i];
      // 🔴 优先显示**账号昵称**（手机号/邮箱），而不是脱敏 UID。
      //
      //	委托方原话：「为什么不用我账号管理里面的4个账号名字，
      //	不然我都分不清到底是哪个账号的用量」。
      //	脱敏 UID（`eeeeeeee…`）对人类不可读 —— 统计页就是要看
      //	"哪个号用了多少"，只给一串哈希前缀等于没给。
      //
      //	⚠️ 昵称拿不到时（账号已被删除）退回脱敏 UID + 提示 ——
      //	  不能用"猜"补一个已删账号的名字。
      var nameHtml;
      if (a.nickname) {
        nameHtml = '<td>' + esc(a.nickname) + platTagOf(a.platform) + '</td>';
      } else {
        nameHtml = '<td class="mono" title="该账号已不在账号管理中（可能已删除）">' +
          esc(a.account) + '</td>';
      }
      // 缓存命中率：后端未给（分母为 0）时显示 —，不是 0%
      var rate = a.cache_hit_rate;
      var hitHtml = (rate === null || rate === undefined)
        ? '—'
        : (Number(rate) * 100).toFixed(1) + '%';
      // ⚠️ 「成功」「失败」两列已按委托方要求删除（信息量低）。
      html += '<tr>'
        + nameHtml
        + '<td class="num">' + fmtInt(a.requests) + '</td>'
        + '<td class="num">' + fmtNum(a.prompt_tokens) + '</td>'
        + '<td class="num">' + fmtNum(a.completion_tokens) + '</td>'
        + '<td class="num">' + fmtNum(a.total_tokens) + '</td>'
        + '<td class="num">' + hitHtml + '</td>'
        + '<td class="num">' + (a.credits === null || a.credits === undefined ? '—' : Number(a.credits).toFixed(2)) + '</td>'
        + '</tr>';
    }
    body.innerHTML = html;
  }

  // platTagOf 渲染小号平台标签（用在国际版上，避免与国内同名账号混淆）。
  //
  // 国内版不加标签：多数账号是国内的，全都加会显得噪。
  function platTagOf(plat) {
    if (plat === 'intl') {
      return '<span class="plat-tag plat-intl">国际版</span>';
    }
    return '';
  }

  // ── 调用记录（逐条明细）──
  //
  // 委托方要求：
  //   「加入一个新列表显示每一次api的调用情况，包括账号、时间、token、
  //     缓存命中率、积分消耗等信息，让我每使用一次都能查询到记录」
  function loadUsageLog() {
    return fetchJSON('/api/usage/log?days=' + currentDays).then(function (d) {
      renderUsageLog(d || {});
    }).catch(function (e) {
      var body = document.getElementById('usage-log-body');
      if (body) body.innerHTML = '<tr><td colspan="11" class="empty">加载失败</td></tr>';
      console.error(e);
    });
  }

  function renderUsageLog(d) {
    usageLogRaw = d.entries || [];
    var note = document.getElementById('usage-log-note');
    if (note) {
      // 让用户知道后端一共命中了多少（明细只取最近 limit 条）
      if (d.total_matched > usageLogRaw.length) {
        note.textContent = '共 ' + d.total_matched + ' 条，已取最近 ' + usageLogRaw.length + ' 条';
      } else {
        note.textContent = '共 ' + usageLogRaw.length + ' 条';
      }
    }
    drawUsageLog();
  }

  function drawUsageLog() {
    var body = document.getElementById('usage-log-body');
    if (!body) return;
    var st = pagerState.log;

    var info = pageSlice(usageLogRaw, st.page);
    st.page = info.page;

    renderPagerInto('log-pager', info, function (n) {
      st.page = n;
      drawUsageLog();
    });

    if (!info.rows.length) {
      body.innerHTML = '<tr><td colspan="11" class="empty">暂无调用记录</td></tr>';
      return;
    }

    var html = '';
    for (var i = 0; i < info.rows.length; i++) {
      var e = info.rows[i];

      var nameHtml = e.nickname
        ? esc(e.nickname) + platTagOf(e.platform)
        : '<span class="mono" title="账号已不在管理中">' + esc(e.account) + '</span>';

      // token：上游没给 usage 时显示 —，**不能显示 0** ——
      // "没拿到"与"确实是 0"含义不同（见 usage.Event.UsageKnown 的注释）。
      var inTok = e.usage_known ? fmtNum(e.prompt_tokens) : '—';
      var outTok = e.usage_known ? fmtNum(e.completion_tokens) : '—';
      // 合计：委托方 2026-10-09 要求「输入输出后面都加一个合计」。
      // 同样受 usage_known 约束 —— 未知时显示 —，不显示 0。
      var totTok = e.usage_known ? fmtNum(e.total_tokens) : '—';

      var rate = e.cache_hit_rate;
      var hitHtml = (rate === null || rate === undefined)
        ? '—'
        : (Number(rate) * 100).toFixed(1) + '%';

      var creditHtml = (e.credits === null || e.credits === undefined)
        ? '—'
        : Number(e.credits).toFixed(2);

      // 耗时：有首字耗时（TTFT）时拆成「首字 x / 总 y」，
      // 没有时只显示总耗时。
      //
      // 🔴 为什么要拆（委托方 2026-10-09 要求「能不能获取到首字耗时」）：
      //	总耗时对流式请求意义有限 —— 用户体感的是"多久开始出字"。
      //	40 秒总耗时 + 0.8 秒首字 与 40 秒 + 20 秒首字，
      //	是完全不同的体验，只看总数分不出来。
      //
      // ⚠️ 首字耗时只对**流式**请求有意义：非流式要收齐才返回，
      //	首字与总耗时本来就是同一个时刻。后端只在流式路径采集它，
      //	非流式记录里该字段为 null，这里自然退回只显示总耗时。
      var durHtml;
      if (e.ttft_ms === null || e.ttft_ms === undefined) {
        durHtml = fmtInt(e.duration_ms) + 'ms';
      } else {
        durHtml = '<span class="ttft">首字 ' + fmtInt(e.ttft_ms) + 'ms</span>'
          + '<span class="dur-sep">/</span>'
          + fmtInt(e.duration_ms) + 'ms';
      }

      // 流：委托方 2026-10-09 要求显示流信息。
      // 同时给出吞吐（t/s）—— 那是"流得快不快"最直观的指标。
      // ⚠️ 吞吐只在**有 token 数与耗时**时才算；否则显示 —（不编造）。
      var streamHtml;
      if (e.stream) {
        var tps = null;
        if (e.usage_known && e.completion_tokens > 0 && e.duration_ms > 0) {
          tps = e.completion_tokens / (e.duration_ms / 1000);
        }
        streamHtml = '<span class="stream-on">流</span>';
        if (tps !== null) {
          streamHtml += '<span class="stream-tps">' + tps.toFixed(0) + ' t/s</span>';
        }
      } else {
        streamHtml = '<span class="stream-off">非流</span>';
      }

      // 状态：成功绿色；客户端断开单独说明（那不是服务故障）；
      // 上游报错红色并把原因放 tooltip。
      var stHtml;
      if (e.ok) {
        stHtml = '<span class="log-ok">成功</span>';
      } else if (e.status === 'client_disconnected') {
        stHtml = '<span class="log-warn" title="客户端提前断开，不算服务端故障">已断开</span>';
      } else {
        stHtml = '<span class="log-bad" title="' + esc(e.error || '') + '">失败</span>';
      }

      html += '<tr>'
        + '<td class="mono">' + esc(e.time || '') + '</td>'
        + '<td>' + nameHtml + '</td>'
        + '<td class="mono">' + esc(e.model || '') + '</td>'
        + '<td class="num">' + inTok + '</td>'
        + '<td class="num">' + outTok + '</td>'
        + '<td class="num">' + totTok + '</td>'
        + '<td class="num">' + hitHtml + '</td>'
        + '<td class="num">' + creditHtml + '</td>'
        + '<td class="num">' + durHtml + '</td>'
        + '<td>' + streamHtml + '</td>'
        + '<td>' + stHtml + '</td>'
        + '</tr>';
    }
    body.innerHTML = html;
  }

  function initUsageLog() {
    var btn = document.getElementById('btn-refresh-usage-log');
    if (!btn) return;
    btn.addEventListener('click', function () {
      if (btn.disabled) return;
      btn.disabled = true;
      var old = btn.textContent;
      btn.textContent = '刷新中…';
      loadUsageLog().then(function () {
        btn.disabled = false;
        btn.textContent = old;
      });
    });
  }

  // renderHit 更新顶部「缓存命中率」卡片。
  //
  // ⚠️ 原来这里还要更新页面下方的环形图（donut / hit-detail），
  //	但委托方 2026-10-06 要求「图3不需要了你直接删掉」——
  //	那张卡片已从 HTML 移除，这里只保留顶部卡片的更新。
  //	（缓存命中率的"按模型/按账号"维度已在各自表格里显示，
  //	  信息没有丢。）
  function renderHit(t) {
    var hit = t.cache_hit_tokens || 0;
    var miss = t.cache_miss_tokens || 0;
    var denom = hit + miss;
    var pct = denom > 0 ? (hit / denom * 100) : 0;

    // 无样本时显示 —（不是 0%）：那两种情况含义不同。
    setText('s-hitrate', denom > 0 ? pct.toFixed(1) + '%' : '—');
  }

  function initTabs() {
    var tabs = document.querySelectorAll('.tab');
    for (var i = 0; i < tabs.length; i++) {
      tabs[i].addEventListener('click', function () {
        for (var j = 0; j < tabs.length; j++) tabs[j].classList.remove('active');
        this.classList.add('active');
        currentDays = parseInt(this.getAttribute('data-range'), 10) || 1;
        loadStats();
      });
    }
  }

  // ═══════════════ API 接入页 ═══════════════

  function loadApi() {
    loadStatus(); // base 下的模型数从 /status 取
    // 返回 promise：手动刷新按钮据此切换"刷新中/已刷新"状态
    //
    // 🔴 必须走 /api/models（面板同源接口），不能走 /v1/models ——
    // 后者是对外 OpenAI 兼容接口，被 requireAPIKey 保护：用户一旦设了
    // 密钥，这个请求会被 401 拒绝，页面就显示「加载失败」。
    // （2026-10-06 修复；此前"模型数 31 正常但列表加载失败"就是这个原因：
    //   模型数取自不校验 key 的 /status，列表却取自要鉴权的 /v1/models。）
    return fetchJSON('/api/models').then(function (d) {
      apiLoaded = true;
      // 后端已把别名过滤掉（只用真实模型），并给出 aliasCount 供提示；
      // groups 是平台分组（委托方要求顶部可切换）。
      apiGroups = (d && d.groups) || [];
      apiAllModels = (d && d.data) || [];
      // 默认选中第一个平台（而非"全部"）——
      // 这样前缀提示立刻是具体可用的一条，不用用户再点一下。
      if (!apiActivePlat && apiGroups.length) {
        apiActivePlat = apiGroups[0].id;
      }
      renderPlatformTabs(apiGroups);
      renderApiModels(filterByPlatform(apiAllModels, apiActivePlat),
        (d && d.aliasCount) || 0);
      syncPrefixNote();
    }).catch(function (e) {
      var body = document.getElementById('api-model-body');
      if (body && !apiLoaded) body.innerHTML = '<tr><td colspan="6" class="empty">加载失败</td></tr>';
      console.error(e);
      throw e;   // 让按钮显示"刷新失败"
    });
  }

  // apiGroups / apiAllModels / apiActivePlat 是模型页的分组状态。
  //
  // 🔴 分组由**后端**给出（/api/models 的 groups），前端不按 ID 前缀自己切：
  //	前缀规则一变，前端那份复制品就会静默失配（本项目的模型前缀已经改过一次）。
  var apiGroups = [];
  var apiAllModels = [];
  var apiActivePlat = '';   // 空串 = 显示全部
  var lastAliasCount = 0;   // 最近一次的别名数（切换平台重绘要用）

  // renderPlatformTabs 渲染平台分组标签（每个带模型数）。
  //
  // 委托方要求（原话）：
  //   「接口列表我说过要有不同平台的分组……我们应该做两个这样的分组可以选，
  //     将模型数也显示在这里面，选中哪个分组下方的模型列表就只显示对应平台的」
  function renderPlatformTabs(groups) {
    var nav = document.getElementById('api-plat-tabs');
    if (!nav) return;

    if (!groups.length) {
      nav.innerHTML = '';
      nav.classList.add('st-hidden');
      return;
    }
    nav.classList.remove('st-hidden');

    // 🔴 不再提供「全部」标签（2026-10-06 委托方要求：「模型列表里不需要全部这个分组」）。
    //
    //	平台之间没有"混合视图"的实际需求 —— 用户要么看国内、要么看国际，
    //	而"全部"反而让前缀提示无法给出一个明确值（要并列两个前缀）。
    var html = '';
    for (var i = 0; i < groups.length; i++) {
      var g = groups[i];
      html += tabHtml(g.id, g.label, g.count);
    }
    nav.innerHTML = html;
  }

  function tabHtml(id, label, count) {
    var active = (id === apiActivePlat) ? ' active' : '';
    return '<button type="button" class="plat-tab' + active +
      '" data-plat="' + esc(id) + '">' +
      esc(label) + '<span class="plat-count">' + count + '</span></button>';
  }

  // filterByPlatform 按前缀筛出某平台的模型。
  //
  // ⚠️ 用后端给的 prefix 而不是硬编码 "workbuddy/"：
  //	前缀规则由后端持有，前端只是照着用。
  function filterByPlatform(list, platID) {
    if (!platID) return list;
    var prefix = '';
    for (var i = 0; i < apiGroups.length; i++) {
      if (apiGroups[i].id === platID) { prefix = apiGroups[i].prefix; break; }
    }
    if (!prefix) return list;
    return list.filter(function (m) {
      return String(m.id || '').indexOf(prefix) === 0;
    });
  }

  // selectPlatform 切换平台标签，并同步前缀提示文案。
  function selectPlatform(platID) {
    apiActivePlat = platID || '';
    renderPlatformTabs(apiGroups);
    renderApiModels(filterByPlatform(apiAllModels, apiActivePlat), lastAliasCount);
    syncPrefixNote();
  }

  // syncPrefixNote 让"调用时要加前缀"的提示跟随当前选中的平台。
  //
  // ⚠️ 不再需要"全部平台"的分支：委托方已要求去掉「全部」标签，
  //	所以选中项**总是一个具体平台**，前缀提示也总有确定值。
  function syncPrefixNote() {
    var pfx = document.getElementById('api-prefix');
    var ex = document.getElementById('api-prefix-example');
    if (!pfx) return;

    var cur = null;
    for (var i = 0; i < apiGroups.length; i++) {
      if (apiGroups[i].id === apiActivePlat) { cur = apiGroups[i]; break; }
    }
    if (!cur) return;

    pfx.textContent = cur.prefix;
    var pool = filterByPlatform(apiAllModels, apiActivePlat);
    var sample = pool.length ? String(pool[0].id || '') : '';
    if (ex) ex.textContent = sample || (cur.prefix + '...');
  }

  // initPlatformTabs 事件委托（标签是动态渲染的，不能逐个绑）。
  function initPlatformTabs() {
    var nav = document.getElementById('api-plat-tabs');
    if (!nav) return;
    nav.addEventListener('click', function (ev) {
      var btn = ev.target && ev.target.closest ? ev.target.closest('.plat-tab') : null;
      if (!btn) return;
      selectPlatform(btn.getAttribute('data-plat'));
    });
  }

  // renderApiModels 渲染模型表。
  //
  // aliasCount 用于解释"为什么这里比客户端能调的少几条" ——
  // 后端在 /v1/models 里**保留**了别名（客户端可能先查列表再决定是否调用），
  // 但面板只列真实模型（委托方：「映射的不需要出现在这里」）。
  // 两者不一致是刻意的，所以必须在这里说清，否则用户会以为模型丢了。
  function renderApiModels(list, aliasCount) {
    var body = document.getElementById('api-model-body');
    if (!body) return;

    lastAliasCount = aliasCount || 0;
    // ⚠️ 不再更新"模型数"（该显示已按委托方要求移除）。
    //    规模信息在平台分组标签上（每个标签带自己的数量）。

    var note = document.getElementById('api-alias-note');
    if (note) {
      if (aliasCount > 0) {
        note.textContent = '面板只列真实模型；另有 ' + aliasCount +
          ' 个别名（自定义映射）仍可通过 API 调用。';
        note.classList.remove('st-hidden');
      } else {
        note.textContent = '';
        note.classList.add('st-hidden');
      }
    }

    if (!list.length) {
      body.innerHTML = '<tr><td colspan="6" class="empty">暂无模型 —— 先导入账号</td></tr>';
      return;
    }

    var html = '';
    for (var i = 0; i < list.length; i++) {
      var m = list[i];
      html += '<tr>'
        + '<td class="mono">' + esc(stripPrefix(m.id)) + '</td>'
        + '<td>' + esc(m.name || '') + badgeHtml(m) + '</td>'
        + '<td class="num">' + (m.context_length ? fmtNum(m.context_length) : '—') + '</td>'
        + '<td class="num">' + (m.max_output_tokens ? fmtNum(m.max_output_tokens) : '—') + '</td>'
        + '<td class="num">' + creditsHtml(m) + '</td>'
        + '<td>' + effortsHtml(m) + '</td>'
        + '</tr>';
    }
    body.innerHTML = html;
  }

  // stripPrefix 去掉模型 ID 的渠道前缀（只用于**显示**）。
  //
  // 🔴 为什么去掉（2026-10-06 委托方要求）：
  //   「我建议模型id不显示 workbuddy/ 只显示 deepseek-v4.1-flash，
  //     因为我是多平台聚合的反代」
  //   多平台聚合时表格里每行都带一长串前缀，既挤又难扫。
  //   ⚠️ 但**调用时仍必须带前缀** —— 否则会报模型不存在。
  //   所以页面上方有专门的提示文案说明这一点（不能只去掉就完事）。
  function stripPrefix(id) {
    var s = String(id || '');
    var i = s.indexOf('/');
    return i >= 0 ? s.slice(i + 1) : s;
  }

  // badgeHtml 渲染上游下发的运营标签（如「限时免费」「夜间折扣」）。
  //
  // 🔴 颜色直接用上游给的值：那是运营侧刻意选的（红=促销、蓝=折扣），
  //    自己另配一套会和官方客户端对不上，用户会怀疑"哪个才是真的"。
  //    上游没给颜色时退回中性灰。
  //
  // 🔴 优先用 promotion（运营活动）而不是 badge（tags 简写）：
  //	promotion 带**时间窗与开关**，是权威来源；
  //	badge 只是简写，活动过期后它可能还留着 —— 会误导用户
  //	以为现在仍然免费。
  function badgeHtml(m) {
    if (m.promotion && (m.promotion.label || m.promotion.free)) {
      var label = m.promotion.label || (m.promotion.free ? 'Free' : '');
      if (label) {
        // note 是活动说明（如"Aug 6 – Sep 30: Free daily use..."），
        // 放 tooltip 里 —— 标签本身放不下。
        var tip = m.promotion.note || '';
        if (m.promotion.valid_until) {
          tip += (tip ? '；' : '') + '截止 ' + String(m.promotion.valid_until).slice(0, 10);
        }
        return badgeSpan(label, m.promotion.color, tip);
      }
    }
    var b = m.badge;
    if (!b || !b.text) return '';
    return badgeSpan(b.text, b.color, '');
  }

  // badgeSpan 渲染一个徽标（颜色由上游决定，缺失时中性灰）。
  function badgeSpan(text, color, tip) {
    var c = color ? String(color) : '';
    var style = 'background:' + esc(c || '#eef0f3') + ';';
    // 浅色底上要深字、深色底上要白字 —— 用亮度粗略判断，避免"红底红字"看不见。
    style += 'color:' + (isLightColor(c) ? '#1f2328' : '#fff') + ';';
    var title = tip ? ' title="' + esc(tip) + '"' : '';
    return '<span class="model-badge" style="' + style + '"' + title + '>' +
      esc(text) + '</span>';
  }

  // isLightColor 粗略判断颜色亮度（用于决定徽标文字用深色还是白色）。
  // 只认 #RRGGBB；其它形态（含空）一律当浅色处理（用深色字最保险）。
  function isLightColor(hex) {
    var s = String(hex || '').replace('#', '');
    if (s.length !== 6) return true;
    var r = parseInt(s.slice(0, 2), 16);
    var g = parseInt(s.slice(2, 4), 16);
    var b = parseInt(s.slice(4, 6), 16);
    if (isNaN(r) || isNaN(g) || isNaN(b)) return true;
    // 感知亮度（ITU-R BT.601）
    return (r * 299 + g * 587 + b * 114) / 1000 > 150;
  }

  // creditsHtml 渲染倍率。
  //
  // 🔴 三种形态必须分清（对应后端 Model.Credits 的约定）：
  //   credits 缺字段  ⇒ 上游**未定价** ⇒ unknown（**不是** Free！）
  //   credits === 0   ⇒ 明确免费     ⇒ Free
  //   其它            ⇒ x0.03 这样显示
  //   把"没定价"显示成"免费"会直接误导用户以为不花钱。
  function creditsHtml(m) {
    if (m.credits === null || m.credits === undefined) {
      return '<span class="credits-unknown" title="上游未返回该模型的定价">unknown</span>';
    }
    var v = Number(m.credits);
    if (isNaN(v)) {
      return '<span class="credits-unknown">unknown</span>';
    }
    if (v === 0) {
      return '<span class="credits-free">Free</span>';
    }
    return '<span class="credits-x">x' + v.toFixed(2) + '</span>';
  }

  // 思考档位：reasoning_efforts 有值才展示；默认档标注「默认」
  function effortsHtml(m) {
    var efforts = m.reasoning_efforts;
    if (!efforts || !efforts.length) return '<span style="color:#b0b6bd">—</span>';
    var html = '';
    for (var i = 0; i < efforts.length; i++) {
      var def = efforts[i] === m.reasoning_default;
      html += '<span class="effort-tag' + (def ? ' def' : '') + '">' + esc(efforts[i]) + (def ? ' · 默认' : '') + '</span>';
    }
    return html;
  }

  // bindCopy 给某个元素挂"复制它自己的文本/值"的按钮。
  //
  // 统一实现（原来是三段各写一遍）—— 顺带修掉一个隐患：
  // 复制源用 textContent/value 读取，**复制的是页面上真实显示的内容**。
  function bindCopy(btnId, readFn) {
    var btn = document.getElementById(btnId);
    if (!btn) return;
    btn.addEventListener('click', function () {
      var text = readFn();
      if (!text) {
        btn.textContent = '无内容';
        setTimeout(function () { btn.textContent = '复制'; }, 900);
        return;
      }
      var done = function () {
        var old = btn.textContent;
        btn.textContent = '已复制';
        setTimeout(function () { btn.textContent = old; }, 900);
      };
      if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(text).then(done, function (e) {
          console.error(e);
        });
      } else {
        console.error('clipboard unavailable');
      }
    });
  }

  function initCopy() {
    var base = location.origin + '/v1';
    var addrEl = document.getElementById('apiAddr');
    if (addrEl) addrEl.textContent = base;

    bindCopy('btn-copy-base', function () { return base; });
    // 密钥从 DOM 读取（单一事实源）—— 内容由 fillApiCredentials 填入
    bindCopy('btn-copy-key', function () {
      var el = document.getElementById('apiKeyView');
      return el ? String(el.textContent || '') : '';
    });
  }

  // fillApiCredentials 把密钥填到「API 接入」页。
  //
  // 🔴 为什么要（2026-10-06 委托方要求）：
  //    「在api右边加个显示key，以及复制按键，方便用户在这个界面直接接入完整信息」
  //    （后又要求「不要模型名」—— 已移除，只保留地址 + 密钥）
  function fillApiCredentials() {
    // 密钥：用与设置页同一个接口（明文返回，理由见后端 settingsView.APIKey）
    fetchJSON('/api/settings').then(function (d) {
      var el = document.getElementById('apiKeyView');
      if (el) {
        el.textContent = (d && d.apiKey) ? String(d.apiKey) : '（未设置）';
      }
    }).catch(function (e) { console.error(e); });
  }

  // 模型列表的「重新拉取上游目录」按钮（该页不自动轮询）
  //
  // 🔴 2026-10-07 修正语义（此前这个按钮**名不副实**）：
  //
  //	原实现只调 loadApi() → `/api/models`，而那个接口读的是 Router
  //	**内存**；内存只在**启动时**写入一次（serve.go 的 registerOnePlatform）。
  //	⇒ 上游改了模型（新增/下架/改价），点刷新**看不到**，必须重启服务。
  //
  //	现在先 POST `/api/models/refresh` 让**服务端去上游重拉**，
  //	成功后再 loadApi() 重新渲染。这样按钮才真正"刷新最新动态列表"。
  //
  //	⚠️ 逐平台报告：某个平台拉取失败时，它**保留原有列表**
  //	  （服务端 Router.Refresh 保证失败/空返回都不清空），
  //	  所以这里要如实说明"哪个平台没更新"，而不是笼统说"刷新失败"。
  function initApiRefresh() {
    var btn = document.getElementById('btn-refresh-models');
    if (!btn) return;
    var note = document.getElementById('api-refresh-note');

    // 把提示文字恢复成默认说明
    function resetNote() {
      if (note) note.textContent = '进入本页时加载一次。「重新拉取」会让服务真的去上游重取目录';
    }

    btn.addEventListener('click', function () {
      if (btn.disabled) return;          // 防重入
      btn.disabled = true;
      var old = btn.textContent;
      btn.textContent = '拉取中…';
      if (note) { note.textContent = '正在让服务重新拉取上游目录…'; note.classList.remove('bad'); }

      // ① 让服务端去上游重拉（有副作用 ⇒ 后端要求 CSRF 令牌，fetchJSON 已带）
      fetchJSON('/api/models/refresh', { method: 'POST' }).then(function (d) {
        var failed = (d && d.failed) || 0;
        var sources = (d && d.sources) || [];

        // ② 无论成功与否都重新渲染列表（失败时渲染的是**保留下来的**原列表）
        return loadApi().then(function () {
          if (failed > 0) {
            // 如实说明哪些平台没更新 —— 它们的旧列表**仍在生效**
            var bad = sources.filter(function (s) { return !s.ok; })
              .map(function (s) { return s.prefix; }).join(', ');
            if (note) {
              note.textContent = '部分平台未更新（' + bad + '），其原有列表仍在使用';
              note.classList.add('bad');
            }
            btn.textContent = '部分失败';
            return;
          }
          if (note) note.textContent = '已从上游重新拉取（共 ' + ((d && d.total) || 0) + ' 个模型）';
          btn.textContent = '已更新';
        });
      }).then(function () {
        btn.disabled = false;
        setTimeout(function () { btn.textContent = old; }, 1500);
        // 提示文字恢复到默认说明（失败详情保留久一点）
        setTimeout(function () {
          if (note && !note.classList.contains('bad')) resetNote();
        }, 4000);
      }, function (e) {
        // 请求本身失败：保留原有列表，只提示用户
        btn.disabled = false;
        btn.textContent = '拉取失败';
        if (note) {
          note.textContent = '拉取上游目录失败：' + (e && e.message ? e.message : '未知错误') +
            '（原有列表仍在使用）';
          note.classList.add('bad');
        }
        setTimeout(function () { btn.textContent = old; }, 2000);
      });
    });
  }

  // ═══════════════ 轮询：只拉当前激活页 ═══════════════

  var timer = null;

  // 需要定时轮询的页面。API 接入页【不轮询】——
  // 模型目录几乎不变，轮询只会让列表无谓重排（委托方反馈：顺序乱跳）；
  // 改为进入页面时拉一次 + 手动「刷新」按钮。
  var POLLED_PAGES = { accounts: true, stats: true };

  function tick() {
    loadStatus();     // 顶栏信息所有页都用
    if (POLLED_PAGES[currentPage]) {
      pages[currentPage]();
    }
  }

  function restartPolling() {
    if (timer) { clearInterval(timer); timer = null; }
    timer = setInterval(tick, REFRESH_MS);
  }

  // 页面隐藏时暂停轮询，可见时恢复 —— 不浪费资源
  function initVisibility() {
    document.addEventListener('visibilitychange', function () {
      if (document.hidden) {
        if (timer) { clearInterval(timer); timer = null; }
      } else {
        tick();
        restartPolling();
      }
    });
  }

  // ═══════════════ 启动 ═══════════════

  function init() {
    // ⚠️ settings 【必须】注册进 pages。
    //
    // switchPage() 开头是 `if (!pages[name]) return;`，未注册的页面
    // 连切换都做不到（点侧边栏没反应）—— 这是接线时最容易漏、且症状
    // 很像"按钮坏了"的一处。
    //
    // 但它的值是【空函数】而不是刷新逻辑：pages[name] 会被
    // switchPage()（第 93 行）与 tick() 轮询（第 742 行）反复调用，
    // 而设置页的初始化（挂监听器）只能做一次 —— 重复挂会重复触发。
    // 设置页自带刷新定时器（且刻意避开用户正在输入时刷新），
    // 所以这里给一个 no-op，既让页面可切换，又不干扰它自己的节奏。
    pages = {
      accounts: loadAccts,
      stats: function () {
        // 调用记录随统计页一起刷新（两者是同一份数据的聚合与明细）
        loadUsageLog();
        return loadStats();
      },
      // API 接入页：除模型列表外，顺带刷新"接入三件套"（地址/密钥/模型名）——
      // 密钥可能在设置页被改过，进本页时重取一次才准。
      api: function () {
        fillApiCredentials();
        return loadApi();
      },
      settings: function () { /* 见上方注释：设置页自管刷新，这里必须为空 */ }
    };

    // 先取面板令牌再初始化：账号的写操作（action / import）需要它。
    // 取失败也继续 —— 后端未启用该防线时一切照旧；
    // 启用了则写操作会返回明确错误，用户看得到原因。
    loadPanelToken().then(function () {
      initNav();
      initModal();
      initTabs();
      initCopy();
      initApiRefresh();
      initPlatformTabs();
      initStatsTabs();
      initUsageLog();
      initAcctCards();
      initAcctBulk();
      initVisibility();

      // 设置页初始化（挂监听器 + 首次拉取）。
      // settings.js 是一个独立 IIFE，通过这个全局入口暴露；
      // 它内部会自查 #page-settings 是否存在，片段没接上时安静退出。
      if (typeof window.initSettingsPage === 'function') {
        window.initSettingsPage();
      }

      tick();           // 首屏：status + 账号
      restartPolling();
    });
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
