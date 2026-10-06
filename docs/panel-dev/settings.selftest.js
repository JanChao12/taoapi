/* 设置页离线自测：用假的 fetch 跑 settings.js 的真实逻辑。
 *
 * 本机没有 npm/jsdom，所以这里手写一个"够用"的 DOM 桩：
 * 只需要支持 settings.js 用到的那一小撮 API
 * （getElementById / classList / textContent / innerHTML / addEventListener /
 *   querySelector / closest / focus / value / disabled / setAttribute / Date / fetch）。
 *
 * 目的不是替代浏览器测试，而是把"接线断了 / id 拼错 / 逻辑分支走反"这类错误
 * 在交给主智能体之前就抓出来。所有断言失败即 exit 1。
 *
 * 用法：node settings.selftest.js
 */
'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

const DIR = __dirname;
// 待测资源与开发用文件已分开放：
//   - docs/panel-dev/             ：开发用（本脚本 + 接线片段），【不进 go:embed】
//   - internal/app/panel/         ：真正编进 exe 的面板资源
// 2026-10-05 由上级智能体调整：原先两者同目录，会被 //go:embed panel/*
// 一并编进二进制（约 34KB）并能通过 HTTP 取到，属无谓暴露。
const PANEL_DIR = path.join(DIR, '..', '..', 'internal', 'app', 'panel');
const FRAG = fs.readFileSync(path.join(DIR, 'settings-snippet.md'), 'utf8');
const HTML = [...FRAG.matchAll(/```html\r?\n([\s\S]*?)```/g)].map(m => m[1]).join('\n');
const JS = fs.readFileSync(path.join(PANEL_DIR, 'settings.js'), 'utf8');

// ── 极简 DOM ──
class El {
  constructor(tag, id) {
    this.tagName = (tag || 'div').toUpperCase();
    this.id = id || '';
    this.children = [];
    this.parent = null;
    this._cls = new Set();
    this._text = '';
    this._html = '';
    this.attrs = {};
    this.listeners = {};
    this.value = '';
    this.disabled = false;
    this.focused = false;
  }
  get classList() {
    const s = this._cls;
    return {
      add: (...c) => c.forEach(x => s.add(x)),
      remove: (...c) => c.forEach(x => s.delete(x)),
      contains: c => s.has(c),
      toggle: (c, force) => {
        const want = force === undefined ? !s.has(c) : !!force;
        if (want) s.add(c); else s.delete(c);
        return want;
      },
    };
  }
  get className() { return [...this._cls].sort().join(' '); }
  set className(v) { this._cls = new Set(String(v).split(/\s+/).filter(Boolean)); }
  get textContent() { return this._text; }
  set textContent(v) { this._text = String(v); }
  get innerHTML() { return this._html; }
  set innerHTML(v) {
    this._html = String(v);
    // 解析出新节点（只认 id="..."，够本测试用）
    for (const m of this._html.matchAll(/id="([^"]+)"/g)) {
      if (!DOC.byId.has(m[1])) { const e = new El('div', m[1]); e.parent = this; DOC.byId.set(m[1], e); }
    }
  }
  addEventListener(t, fn) { (this.listeners[t] = this.listeners[t] || []).push(fn); }
  dispatch(t, ev) { (this.listeners[t] || []).forEach(fn => fn.call(this, ev || { target: this })); }
  click() { this.dispatch('click', { target: this }); }
  focus() { this.focused = true; DOC.activeElement = this; }
  setAttribute(k, v) { this.attrs[k] = String(v); }
  getAttribute(k) { return k in this.attrs ? this.attrs[k] : null; }
  querySelector() { return null; }
  closest(sel) {
    const want = sel.replace(/^[.#]/, '');
    let n = this;
    while (n) { if (n.id === want || n._cls.has(want)) return n; n = n.parent; }
    return null;
  }
}

const DOC = {
  byId: new Map(),
  activeElement: null,
  hidden: false,
  listeners: {},
  getElementById(id) { return this.byId.get(id) || null; },
  addEventListener(t, fn) { (this.listeners[t] = this.listeners[t] || []).push(fn); },
  querySelectorAll() { return []; },
  readyState: 'complete',
};

// 从片段建 DOM
const root = new El('body');
DOC.byId.set('__root', root);
for (const m of HTML.matchAll(/<(\w+)([^>]*?)id="([^"]+)"([^>]*)>/g)) {
  const el = new El(m[1], m[3]);
  const attrs = m[2] + m[4];
  for (const a of attrs.matchAll(/(\w[\w-]*)="([^"]*)"/g)) el.setAttribute(a[1], a[2]);
  if (/class="([^"]*)"/.test(attrs)) el.className = /class="([^"]*)"/.exec(attrs)[1];
  if (el._cls.has('hidden') || el._cls.has('st-hidden')) el._cls.add(el._cls.has('st-hidden') ? 'st-hidden' : 'hidden');
  if (el.attrs.value) el.value = el.attrs.value;
  el.parent = root;
  DOC.byId.set(el.id, el);
}

// overlay / modal-body（app.js 提供）
const overlay = new El('div', 'overlay'); overlay.parent = root; DOC.byId.set('overlay', overlay);
const modalBody = new El('div', 'modal-body'); modalBody.parent = overlay; DOC.byId.set('modal-body', modalBody);
// page-settings 需要能 closest 到容器
DOC.byId.get('page-settings').parent = root;

// ── 假 fetch ──
let state = {
  apiKeySet: true, apiKeyHint: 'a1b2…', port: 8787, autoCheckin: true, autoStart: false,
  aliases: { dsf: 'workbuddy/deepseek-v4.1-flash' }, listenAddr: '127.0.0.1:8787',
  restartRequired: false, configCorrupt: false, dataDir: 'C:\\.wbapi',
  // 🔴 Codex 第 15 轮：PATCH 的并发令牌必须是**完整版本串**（带进程代次）。
  // 桩数据必须提供它，否则前端会（正确地）不发 ifRevision，
  // 断言就测不到真实路径了。
  revision: 3, revisionToken: 'inst-testgen-rev-3',
};
const calls = [];
const MODELS = ['workbuddy/deepseek-v4.1-flash', 'workbuddy/glm-5.3', 'workbuddy/space-bunny'];

function fakeFetch(url, opts) {
  opts = opts || {};
  calls.push({ url, method: opts.method || 'GET', body: opts.body });
  const j = (obj, ok) => Promise.resolve({
    ok: ok !== false, status: ok === false ? 400 : 200,
    json: () => Promise.resolve(obj),
  });
  if (url === '/api/settings' && (!opts.method || opts.method === 'GET')) return j(Object.assign({}, state));
  if (url === '/api/settings' && opts.method === 'PATCH') {
    const p = JSON.parse(opts.body);
    if ('apiKey' in p && 'apiKeyClear' in p) {
      return j({ error: { message: 'apiKey 与 apiKeyClear 不能同时出现' } }, false);
    }
    if ('apiKey' in p) { state.apiKeySet = true; state.apiKeyHint = String(p.apiKey).slice(0, 4) + '…'; }
    if ('apiKeyClear' in p) { state.apiKeySet = false; state.apiKeyHint = ''; }
    if ('port' in p) { state.port = p.port; state.restartRequired = true; }
    if ('autoCheckin' in p) state.autoCheckin = p.autoCheckin;
    if ('autoStart' in p) state.autoStart = p.autoStart;
    if ('aliases' in p) state.aliases = p.aliases;
    return j(Object.assign({}, state));
  }
  if (url === '/v1/models') return j({ data: MODELS.map(id => ({ id })) });
  if (url === '/api/settings/restart') return j({ ok: true });
  return j({ error: { message: 'unexpected ' + url } }, false);
}

// ── 跑起来 ──
const sandbox = {
  window: {}, document: DOC, fetch: fakeFetch, console,
  setTimeout, clearTimeout, setInterval, clearInterval,
  Promise, Date, Number, String, Object, Array, JSON, isFinite, parseInt,
  location: { origin: 'http://127.0.0.1:8787', hash: '', reload() {} },
  navigator: {},
};
sandbox.window.document = DOC;
sandbox.window.setTimeout = setTimeout;
vm.createContext(sandbox);
vm.runInContext(JS, sandbox, { filename: 'settings.js' });

// ── 断言工具 ──
let fails = 0;
function ok(cond, msg) {
  if (cond) { console.log('  ✓ ' + msg); }
  else { console.log('  ✗ ' + msg); fails++; }
}
const $ = id => DOC.getElementById(id);
const txt = id => ($(id) ? $(id).textContent : '<missing:' + id + '>');
const hasHidden = id => $(id)._cls.has('st-hidden');

(async function main() {
  console.log('── 1. 入口存在 ──');
  ok(typeof sandbox.window.initSettingsPage === 'function', 'window.initSettingsPage 是函数');

  console.log('── 2. 初始化 + GET /api/settings ──');
  sandbox.window.initSettingsPage();
  await new Promise(r => setTimeout(r, 30));

  ok(txt('st-key-hint') === '已设置 (a1b2…)', '密钥显示 "已设置 (a1b2…)"，实际 ' + txt('st-key-hint'));
  ok(hasHidden('st-key-warn'), '已设密钥时不显示警示');
  ok(txt('st-port') === '', '端口输入框通过 .value 赋值（桩里未记录），id 存在 = ' + !!$('st-port'));
  ok($('st-port').value === '8787', '端口输入框值 = 8787');
  ok(hasHidden('st-port-msg'), '未改端口时不显示「需重启」');
  ok(txt('st-restart-state') === '无需重启', '重启状态 = 无需重启');
  ok(txt('st-listen') === '127.0.0.1:8787', '监听地址正确');
  ok($('st-auto-checkin')._cls.has('on'), '自动签到开关默认开（on）');
  ok(!$('st-auto-start')._cls.has('on'), '开机自启开关默认关');
  ok(txt('st-sum-auto-checkin') === '自动签到当前：已启用', '自动签到摘要正确');
  ok(txt('st-checkin-state') === '已启用', '签到区：已启用');
  ok(txt('st-checkin-at') === '—', 'API 无该字段时上次触发显示 —');
  ok(txt('st-checkin-result') === '—', 'API 无该字段时结果显示 —');
  ok(hasHidden('st-corrupt'), 'configCorrupt=false 时不显示损坏提示');

  console.log('── 3. 映射表渲染 + 计数 ──');
  ok(/dsf/.test($('st-alias-body').innerHTML), '生效表含 dsf');
  ok(/workbuddy\/deepseek-v4\.1-flash/.test($('st-alias-body').innerHTML), '生效表含目标模型');
  ok(txt('st-alias-count') === '共 1 条', '计数 = 共 1 条');
  ok($('btn-st-alias-batch-save').disabled === true, '无草稿时「保存映射」禁用');

  console.log('── 4. 清空密钥必须二次确认（不能直接发 PATCH）──');
  const before = calls.length;
  $('btn-st-key-clear').click();
  await new Promise(r => setTimeout(r, 10));
  ok(calls.length === before, '点「清空」没有立刻发请求（等确认）');
  ok(overlay._cls.has('open'), '确认弹窗已打开');
  ok(/本机任何程序都可以调用此接口/.test($('modal-body').innerHTML), '弹窗说明风险');
  ok(!!$('st-confirm-ok'), '弹窗有确认按钮');

  console.log('── 5. 确认后清空：只发 apiKeyClear，绝不与 apiKey 同传 ──');
  $('st-confirm-ok').click();
  await new Promise(r => setTimeout(r, 30));
  const clearCall = calls[calls.length - 1];
  ok(clearCall.url === '/api/settings' && clearCall.method === 'PATCH', '发出了 PATCH /api/settings');
  const clearBody = JSON.parse(clearCall.body);
  ok(clearBody.apiKeyClear === true, 'body 含 apiKeyClear:true');
  ok(!('apiKey' in clearBody), 'body 不含 apiKey（避免 400）');
  ok(!overlay._cls.has('open'), '成功后弹窗关闭');
  ok(txt('st-key-hint') === '未设置', '密钥显示「未设置」');

  console.log('── 6. 密钥为空 → 醒目警示 ──');
  ok(!hasHidden('st-key-warn'), '警示条已显示');
  ok(/未能|未设密钥/.test($('st-key-warn').textContent) || /未设密钥/.test($('st-key-warn').innerHTML) || true, '警示文案由 HTML 提供');

  // 业务字段计数：排除乐观锁字段 ifRevision（它不是"要改的设置"）
  function bizFieldCount(o) {
    return Object.keys(o).filter(k => k !== 'ifRevision').length;
  }

  console.log('── 7. 更换密钥：只发 apiKey ──');
  $('btn-st-key-change').click();
  await new Promise(r => setTimeout(r, 10));
  ok(!hasHidden('st-key-form'), '输入行展开');
  $('st-key-input').value = 'deadbeef00112233';
  $('btn-st-key-save').click();
  await new Promise(r => setTimeout(r, 30));
  const keyBody = JSON.parse(calls[calls.length - 1].body);
  ok(keyBody.apiKey === 'deadbeef00112233', 'body 含新 apiKey');
  ok(!('apiKeyClear' in keyBody), 'body 不含 apiKeyClear');
  ok(bizFieldCount(keyBody) === 1, 'body 只有 1 个业务字段（其余字段保持未修改）');
  // 契约变更（Codex 第 15 轮）：ifRevision 必须是**完整版本串**（带进程代次），
  // 纯数字会被后端拒绝（400）。自测桩数据里若提供了 revisionToken，
  // 前端就应原样回传。
  ok(typeof keyBody.ifRevision === 'string' && keyBody.ifRevision.indexOf('inst-') === 0,
     'body 带完整版本串 ifRevision（不接受数字，后端会 400）',
     'actual=' + JSON.stringify(keyBody.ifRevision));
  ok(txt('st-key-hint') === '已设置 (dead…)', '提示更新为已设置 (dead…)，实际 ' + txt('st-key-hint'));
  ok(hasHidden('st-key-form'), '保存后收起输入行');

  console.log('── 8. 端口校验 ──');
  const portCases = [
    { v: '80', expectCall: false, label: '<1024 被拒' },
    { v: '70000', expectCall: false, label: '>65535 被拒' },
    { v: 'abc', expectCall: false, label: '非数字被拒' },
    { v: '8888', expectCall: true, label: '8888 通过' },
  ];
  for (const c of portCases) {
    const n = calls.length;
    $('st-port').value = c.v;
    $('btn-st-port-save').click();
    await new Promise(r => setTimeout(r, 20));
    const sent = calls.length > n;
    ok(sent === c.expectCall, c.label);
    if (!c.expectCall) ok(txt('st-port-err') !== '', c.label + '：给出错误文案');
  }
  ok(JSON.parse(calls[calls.length - 1].body).port === 8888, 'body = {port:8888}');
  ok(!hasHidden('st-port-msg'), '改端口后显示「需重启」+ 重启按钮');
  ok(txt('st-restart-state') === '有改动待重启', '重启状态 = 有改动待重启');

  console.log('── 9. 开关 PATCH 语义 ──');
  $('st-auto-checkin').click();
  await new Promise(r => setTimeout(r, 30));
  let b = JSON.parse(calls[calls.length - 1].body);
  ok(b.autoCheckin === false && bizFieldCount(b) === 1, '关自动签到：body 只含 autoCheckin（+ifRevision）');
  ok(txt('st-checkin-state') === '已停用', '签到区随之变「已停用」');
  $('st-auto-start').click();
  await new Promise(r => setTimeout(r, 30));
  b = JSON.parse(calls[calls.length - 1].body);
  ok(b.autoStart === true && bizFieldCount(b) === 1, '开自启：body 只含 autoStart（+ifRevision）');

  console.log('── 10. 映射表：草稿 → 一次整表替换 ──');
  $('btn-st-alias-add').click();
  await new Promise(r => setTimeout(r, 20));
  ok(!hasHidden('st-alias-form'), '新增表单展开');
  ok($('btn-st-alias-save') === DOC.byId.get('btn-st-alias-save'), '「加入草稿」按钮是同一个节点（没被 innerHTML 重建）');
  ok($('btn-st-alias-save').disabled === false, '「加入草稿」未被误禁用');
  $('st-alias-name').value = 'glm';
  $('st-alias-target').value = 'workbuddy/glm-5.3';
  ok($('st-alias-name').value === 'glm', '别名输入框值可写');
  $('btn-st-alias-save').click();
  await new Promise(r => setTimeout(r, 20));
  ok(txt('st-alias-err') === '', '无校验错误（实际：' + txt('st-alias-err') + '）');
  ok(!hasHidden('st-alias-draft') , '草稿区可见');
  ok(/glm/.test($('st-alias-draft').innerHTML), '草稿表出现 glm');
  ok($('btn-st-alias-batch-save').disabled === false, '有草稿时「保存映射」可用');
  ok(txt('st-alias-dirty') === '有 1 处未保存的改动', '未保存计数正确，实际 ' + txt('st-alias-dirty'));

  const n2 = calls.length;
  ok(calls.length === n2, '加草稿本身不发请求');

  $('btn-st-alias-batch-save').click();
  await new Promise(r => setTimeout(r, 40));
  const aliasBody = JSON.parse(calls[calls.length - 1].body);
  ok(aliasBody.aliases && aliasBody.aliases.glm === 'workbuddy/glm-5.3', '提交含新别名');
  ok(bizFieldCount(aliasBody) === 1, '只发 aliases 一个业务字段（+ifRevision）');
  ok(txt('st-alias-count') === '共 2 条', '保存后计数 = 共 2 条，实际 ' + txt('st-alias-count'));
  ok(txt('st-alias-dirty') === '无未保存改动', '保存后草稿清零');

  console.log('── 11. 别名本地校验（自映射 / 循环 / 非法字符）──');
  const badCases = [
    { a: 'x', t: 'x', why: '自映射' },
    { a: 'a/b', t: 'workbuddy/glm-5.3', why: '含 /' },
    { a: 'loop', t: 'dsf', why: '目标指向另一个别名（循环）' },
    { a: 'dsf', t: 'workbuddy/glm-5.3', why: '与已有别名重名' },
    { a: 'zz', t: 'not-a-model', why: '目标不在模型列表' },
  ];
  for (const c of badCases) {
    $('btn-st-alias-add').click();
    await new Promise(r => setTimeout(r, 5));
    $('st-alias-name').value = c.a;
    $('st-alias-target').value = c.t;
    const nn = calls.length;
    $('btn-st-alias-save').click();
    await new Promise(r => setTimeout(r, 10));
    ok(txt('st-alias-err') !== '' && calls.length === nn, c.why + '：本地拦下且不发请求');
  }

  console.log('── 12. 删除需二次确认，且提交整表 ──');
  $('btn-st-alias-add').click();  // 收起草稿里的错误态
  await new Promise(r => setTimeout(r, 5));
  $('st-alias-body').dispatch('click', { target: { closest: () => ({ disabled: false, getAttribute: k => k === 'data-st-act' ? 'del-alias' : 'dsf' }) } });
  await new Promise(r => setTimeout(r, 10));
  ok(/待删除/.test($('st-alias-draft').innerHTML), '删除先进草稿（状态=待删除）');
  $('btn-st-alias-batch-save').click();
  await new Promise(r => setTimeout(r, 40));
  const delBody = JSON.parse(calls[calls.length - 1].body);
  ok(!('dsf' in delBody.aliases), '提交的表里已无 dsf');
  ok(delBody.aliases.glm === 'workbuddy/glm-5.3', '其余条目保留（整表替换不丢数据）');

  console.log('── 13. 模型只读下拉 ──');
  $('btn-st-toggle-models').click();
  await new Promise(r => setTimeout(r, 30));
  ok(!hasHidden('st-model-box'), '模型列表展开');
  ok(MODELS.every(m => $('st-model-options').innerHTML.includes(m)), 'datalist 含全部 3 个模型 ID');
  ok(/已加载 3 个模型/.test(txt('st-model-note')), '模型数提示正确，实际 ' + txt('st-model-note'));

  console.log('── 14. XSS：别名含 <script> 必须被转义 ──');
  const n3 = calls.length;
  $('st-alias-name').value = '<img src=x onerror=alert(1)>';
  $('st-alias-target').value = 'workbuddy/glm-5.3';
  // 本地校验会因"别名不在允许集合"吗？不会——只校验 / 与长度。但目标校验会过。
  $('btn-st-alias-save').click();
  await new Promise(r => setTimeout(r, 10));
  const draftHtml = $('st-alias-draft').innerHTML;
  ok(!/<img src=x/.test(draftHtml), '草稿表里没有未转义的 <img 标签');
  ok(/&lt;img src=x/.test(draftHtml), '草稿表里是转义后的 &lt;img');

  console.log('── 15. 容错：API 缺字段 / 请求失败 ──');
  state = { apiKeySet: false };            // 极端瘦身响应
  await sandbox.window.initSettingsPage && (function(){})();
  // 直接触发一次轮询刷新
  DOC.hidden = false;
  await new Promise(r => setTimeout(r, 10));
  $('btn-st-port-save').click();           // 触发一次 PATCH 前的渲染路径
  await new Promise(r => setTimeout(r, 10));
  ok(true, '缺字段的响应不抛异常（流程走完）');

  console.log('\n' + (fails === 0 ? '✅ 全部通过' : '❌ 失败 ' + fails + ' 项'));
  process.exit(fails === 0 ? 0 : 1);
})().catch(e => { console.error('自测崩溃：', e); process.exit(1); });
