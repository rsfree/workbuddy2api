'use strict';
/* ── 状态 ─────────────────────────────────────────────────────────── */
const LS_KEY = 'wb2api.key', LS_THEME = 'wb2api.theme';
/* 账号备注与「积分到期提醒」折叠的本机键（同上：模块级声明放顶部，避开 TDZ）。 */
const LS_REMARKS = 'wb2api.remarks.v1', LS_EXP_FOLD = 'wb2api.expfold.v1';
/* 账号池「只看异常」筛选的本机键（all | cool | off）。 */
const LS_ACC_FILTER = 'wb2api.accfilter.v1';
let remarksCache = null;
let theme = localStorage.getItem(LS_THEME) || 'auto';   // auto | light | dark
let view = 'accounts';
let overviewData = null, cfgLoaded = null;
let logPin = true, loginState = null, loginTimer = null;
let refTimer = null;
/* 账号行增量更新用的 DOM 索引（uid → { el, sig }）。模块级声明放顶部，避开 TDZ
   ——renderAccounts 被 go()/轮询调用时，这个区的脚本早已求值完毕。 */
let accRows = new Map();
/* 账号池的「只看异常」筛选：all | cool | off。本机记住上次的选择——运维盯着某类
   异常号排查时会反复进这个页面，每次都要重新点一遍筛选是纯摩擦。 */
let accFilter = 'all';
try { accFilter = localStorage.getItem(LS_ACC_FILTER) || 'all'; } catch (e) {}

/* 账号池视图状态（搜索 / 状态筛选 / 标签筛选 / 排序 / 分页）。
   全部是**内存运算**：数据源仍是 overview 的 5 秒快照，筛选与翻页只重渲染表格，
   不触发任何 api() 调用，也不改轮询节奏——这是「不影响 api 调用性能」的硬约束。
   标签只写 localStorage（wb2api.acctags.v1），不进凭证文件、不上传。 */
const LS_ACC_TAGS = 'wb2api.acctags.v1';
const LS_ACC_VIEW = 'wb2api.accview.v1';
const ACC_STATUS = ['all', 'issue', 'ok', 'cool', 'paused', 'off', 'ent', 'global'];
const ACC_STATUS_LABEL = {
  all: '全部', issue: '需处理', ok: '可用', cool: '冷却',
  paused: '暂停', off: '禁用', ent: '企业版', global: '国际版',
};
/* 排序表：每个比较器都以「导入时间倒序」做二级键，保证同值时顺序稳定
   （不稳定排序会让表格每 5 秒抖一次，运维以为数据在变）。 */
const ACC_SORTS = {
  default: (a, b) => (accRank(a) - accRank(b)) || (parseAPITime(b.imported_at) || 0) - (parseAPITime(a.imported_at) || 0),
  credits_desc: (a, b) => ((b.credits || 0) - (a.credits || 0)) || (parseAPITime(b.imported_at) || 0) - (parseAPITime(a.imported_at) || 0),
  credits_asc: (a, b) => ((a.credits || 0) - (b.credits || 0)) || (parseAPITime(b.imported_at) || 0) - (parseAPITime(a.imported_at) || 0),
  fails_desc: (a, b) => ((b.err_total || 0) - (a.err_total || 0)) || (parseAPITime(b.imported_at) || 0) - (parseAPITime(a.imported_at) || 0),
  success_desc: (a, b) => ((b.success_count || 0) - (a.success_count || 0)) || (parseAPITime(b.imported_at) || 0) - (parseAPITime(a.imported_at) || 0),
  last_success: (a, b) => (parseAPITime(b.last_success) || 0) - (parseAPITime(a.last_success) || 0),
  imported: (a, b) => (parseAPITime(b.imported_at) || 0) - (parseAPITime(a.imported_at) || 0),
  name: (a, b) => String(a.nickname || a.uid).localeCompare(String(b.nickname || b.uid), 'zh-CN'),
};

let accTags = {};
try { accTags = JSON.parse(localStorage.getItem(LS_ACC_TAGS) || '{}') || {}; } catch (e) { accTags = {}; }
if (!accTags || typeof accTags !== 'object' || Array.isArray(accTags)) accTags = {};

let accView = { q: '', status: 'all', tags: [], sort: 'default', size: 50, page: 1 };
try {
  const v = JSON.parse(localStorage.getItem(LS_ACC_VIEW) || '{}') || {};
  if (typeof v.q === 'string') accView.q = v.q;
  if (ACC_STATUS.indexOf(v.status) >= 0) accView.status = v.status;
  if (Array.isArray(v.tags)) accView.tags = v.tags.filter(x => typeof x === 'string');
  if (typeof v.sort === 'string' && ACC_SORTS[v.sort]) accView.sort = v.sort;
  // 分页大小只认白名单：localStorage 被手改成 1 会让表格变成一页一行。
  if ([0, 20, 50, 100, 200].indexOf(Number(v.size)) >= 0) accView.size = Number(v.size);
} catch (e) {}

/* 选中集合只存 uid：切筛选、翻页都不清空，批量操作才能跨页累积。 */
let accSel = new Set();
let tagEditingUid = null;
let tagDraft = [];
/* 通用折叠的真值源：Map<key, bool>，null 表示还没从 localStorage 载入。 */
let foldState = null;
/* 视图级筛选状态（模块级声明放在文件顶部，避免顶层 go() 早于声明执行时踩 TDZ）。 */
let mdFilter = { q: '', realm: '', type: '', cap: '', effort: '', promo: '', sort: 'default' };
// 会拦成长任务的只有「真绑定租户 / 企业版」这一种（概览接口的 enterprise_session）。
// 注意 token_source=enterprise_switch（未绑定租户）**不等于企业号**：实测仍正常计分。
// 2026-10-23 实测：50 个号里 22 个带这个标记、全是个人号，差别只在登录入口。
// 因此前端不再为它渲染徽标 —— 一枚对「能否得分 / 会不会被拦」零判断力的标签，
// 只会让人误以为账号有问题（早前那版把它写成「不计分」就是这么来的）。
// entSession 仍是「真企业会话」清单，renderAccounts / loadOverview 都要用；
// 6ff601b 撤徽标时连带删掉了这一行的声明，导致 loadOverview 首行赋值即
// ReferenceError（严格模式）→ 被 quiet 的 catch 静默吞掉 → 账号池/统计全空白。
let entSession = new Set();
let mdAll = [], mdProbes = {}, mdProbeOf = () => undefined;
let mdLocks = [], mdRecent = [];
let mdDebug = null, mdDebugSeq = 0, mdVideoTaskID = '', mdDebugBusy = false;
let reqFilter = { q: '', outcome: '' };
let reqEntries = [];
let usDim = 'account', usCreditDim = 'account', usSort = 'total';
let usageData = null;
let capacityData = null;
let capacityRequestSeq = 0;
let reqRangeState = null; // 请求记录的时间范围（用量页的见 trangeState）

const $ = id => document.getElementById(id);

/* ── 主题 ─────────────────────────────────────────────────────────── */
/* 两态翻转（浅/深），首次访问跟随系统偏好；点击总是切换可见外观，符合直觉。 */
function effTheme() {
  return theme === 'auto' ? (matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark') : theme;
}
function applyTheme() {
  const eff = effTheme();
  document.documentElement.dataset.theme = eff;
  $('icoTheme').innerHTML = eff === 'light'
    ? '<circle cx="8" cy="8" r="3"/><path d="M8 1v2M8 13v2M1 8h2M13 8h2M3.2 3.2l1.4 1.4M11.4 11.4l1.4 1.4M12.8 3.2l-1.4 1.4M4.6 11.4l-1.4 1.4"/>'
    : '<path d="M13.2 9.6A5.6 5.6 0 0 1 6.4 2.8a5.6 5.6 0 1 0 6.8 6.8z"/>';
  $('btnTheme').title = eff === 'light' ? '切换到深色' : '切换到浅色';
}
addEventListener('change', applyTheme);
$('btnTheme').onclick = () => {
  theme = effTheme() === 'light' ? 'dark' : 'light';
  localStorage.setItem(LS_THEME, theme);
  applyTheme();
};
applyTheme();

/* ── 请求 ─────────────────────────────────────────────────────────── */
/* 两个防护，都是为了「5 秒轮询 + 公网抖动」这个组合：
   1) 超时。裸 fetch 在链路半死时会挂几分钟，既不报错也不返回，界面就永远停在旧值上，
      也没有任何提示。但**超时值必须按方法分档**：POST 里有跑完整条行为链的长任务
      （accounts/{uid}/tasks/auto_all 实测 ~124 秒、批量导入 + onboard 更久），给 POST
      挂 8 秒会把「还在正常跑」误报成「请求超时」——服务端还在消耗真实对话配额，
      比不设超时更糟。
   2) 同路径防重入。上次没回来就跳过本轮（**不排队**）——排队只会让数据更旧，而
      乱序返回会让旧响应盖掉新响应；seq 只认最新一次。
   两条都只对 GET 生效：POST 是用户动作或改状态，跳过等于「点了没反应」。 */
const API_TIMEOUT_MS = 8000;         // GET（轮询）：8 秒不回就当这轮失败，5 秒后再来
/* 逐账号打上游的 GET 另给一档：packages 是 130 个号 / 服务端并发 3 逐个查上游，
   十几到几十秒是常态（见 panel.go 的 sem := make(chan struct{}, 3)），8 秒必假阳性。
   这类慢查询由**调用方**显式 opt-in，比在 api() 里维护路径白名单稳：新接口忘记
   声明只会退回 8 秒并报错，不会悄悄把长任务截断。 */
const API_SLOW_GET_TIMEOUT_MS = 60000;
/* 再上一档：capacity 要重算全池消耗速率 + 预测曲线，线上实测首次调用 8.2 秒（此时 5
   分钟缓存是空的，而「首次进容量页 / 缓存过期」是稳定会撞上的场景），并发时更慢。
   给 90 秒留量：超时只该兜「真挂了」，不该兜「正在正常算」。 */
const API_HEAVY_TIMEOUT_MS = 90000;
const API_POST_TIMEOUT_MS = 300000;  // POST：5 分钟，覆盖 auto_all(~124s)/批量导入
let apiSeq = 0;
const apiLatestSeq = new Map();   // path → 最近一次发出的请求序号
const apiInflight = new Map();    // path → true（GET 在途标记）

async function api(path, opts = {}) {
  const method = String(opts.method || 'GET').toUpperCase();
  const polling = method === 'GET';
  // 调用方可用 opts.timeoutMs 覆盖（慢 GET 见 API_SLOW_GET_TIMEOUT_MS 的说明）
  const timeoutMs = Number(opts.timeoutMs) > 0
    ? Number(opts.timeoutMs)
    : (polling ? API_TIMEOUT_MS : API_POST_TIMEOUT_MS);
  if (polling && apiInflight.has(path)) throw new Error('上一次请求仍在进行中，本轮跳过');
  const h = Object.assign({}, opts.headers || {});
  const k = localStorage.getItem(LS_KEY);
  if (k) h['Authorization'] = 'Bearer ' + k;
  if (opts.body) h['Content-Type'] = 'application/json';
  const init = Object.assign({}, opts, { headers: h });
  delete init.timeoutMs;   // 只是给 api() 看的，不能漏进 fetch 的 init
  const ctl = typeof AbortController === 'function' ? new AbortController() : null;
  let timer = null;
  if (ctl) {
    init.signal = ctl.signal;
    timer = setTimeout(() => ctl.abort(), timeoutMs);
  }
  const seq = ++apiSeq;
  if (polling) { apiLatestSeq.set(path, seq); apiInflight.set(path, true); }
  try {
    const r = await fetch('/panel/api/' + path, init);
    if (polling && apiLatestSeq.get(path) !== seq) throw new Error('响应已过期（有更新的请求）');
    if (r.status === 401) { openKey(); throw new Error('密钥无效或未填写'); }
    const d = await r.json().catch(() => ({}));
    if (!r.ok) {
      // 带上状态码：409 有两种语义（任务锁冲突 / 新号静默期拦截），只有后者允许
      // 带 force=1 重试（见 retryForced）。混为一谈会把互斥锁也一起绕过去。
      const err = new Error(d.error || ('HTTP ' + r.status));
      err.status = r.status;
      throw err;
    }
    return d;
  } catch (e) {
    if (e && e.name === 'AbortError') throw new Error('请求超时（' + (timeoutMs / 1000) + ' 秒无响应）');
    throw e;
  } finally {
    if (timer) clearTimeout(timer);
    if (polling) apiInflight.delete(path);
  }
}
// retryForced 新号静默期 409 的显式越权重试：只在用户二次确认后带 ?force=1 重放一次。
// 判据必须同时满足「409 + 文案含静默期」——任务锁冲突也是 409，重试它等于破坏互斥。
async function retryForced(path, opts, e, what) {
  if (!e || e.status !== 409 || !/静默期/.test(e.message || '')) return null;
  if (!confirm(e.message + '\n\n仍要立刻执行「' + what + '」？\n这会立即消耗真实对话配额，新号在风控窗口内跑重行为链存在被封风险。')) return null;
  return api(path + (path.includes('?') ? '&' : '?') + 'force=1', opts);
}
function toast(msg, cls) {
  const el = document.createElement('div');
  el.className = 'tst ' + (cls || '');
  el.textContent = msg;
  $('toasts').appendChild(el);
  setTimeout(() => el.remove(), 3600);
}
// esc 文本/属性双安全转义。不能只用 div.innerHTML（它转义 <>& 但不转义引号），
// 否则字符串拼进 HTML 属性（如 title="uid: ..."）时引号可闭合属性并注入事件处理器。
// 显式替换 5 个字符：& < > " '（& 必须最先，避免二次转义）。
function esc(s) {
  return String(s == null ? '' : s)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}
function ago(iso) {
  if (!iso || iso.startsWith('0001-')) return '—';
  const s = (Date.now() - new Date(iso)) / 1000;
  if (s < 0) return '刚刚';
  if (s < 60) return Math.floor(s) + ' 秒前';
  if (s < 3600) return Math.floor(s / 60) + ' 分钟前';
  if (s < 86400) return Math.floor(s / 3600) + ' 小时前';
  return Math.floor(s / 86400) + ' 天前';
}
function dur(sec) {
  sec = Math.max(0, Math.round(sec));
  const h = Math.floor(sec / 3600), m = Math.floor(sec % 3600 / 60), s = sec % 60;
  return h ? h + '时' + String(m).padStart(2, '0') + '分' : m ? m + '分' + String(s).padStart(2, '0') + '秒' : s + '秒';
}
// 导入时间：MM-DD HH:mm（跨年补年份）；未记录（旧凭证）显示 —。
// 只负责展示，排序用 parseAPITime 的原始毫秒值。
function importedAtText(iso) {
  const ms = parseAPITime(iso);
  if (!ms) return '—';
  const d = new Date(ms);
  const p = n => String(n).padStart(2, '0');
  const md = p(d.getMonth() + 1) + '-' + p(d.getDate()) + ' ' + p(d.getHours()) + ':' + p(d.getMinutes());
  return d.getFullYear() === new Date().getFullYear() ? md : d.getFullYear() + '-' + md;
}
function importedAtTitle(iso) {
  const ms = parseAPITime(iso);
  if (!ms) return '未记录导入时间（该凭证导入早于本功能上线）';
  return '导入于 ' + fmtLocalDateTime(ms) + '（' + ago(iso) + '）';
}
function parseAPITime(value) {
  const text = String(value || '');
  if (!text || text.startsWith('0001-')) return 0;
  const ms = Date.parse(text);
  return Number.isFinite(ms) ? ms : 0;
}
function fmtLocalDateTime(ms) {
  const d = new Date(ms);
  const p = n => String(n).padStart(2, '0');
  return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) + ' ' +
    p(d.getHours()) + ':' + p(d.getMinutes());
}

/* ── 时间范围控件（用量 / 请求记录共用）────────────────────────────────
   预设项：今天 / 近 24 小时 / 近 3 天 / 近 7 天 / 近 30 天 / 全部历史 / 自定义。

   为什么区间一律由前端算好再发：
     - 「今天」必须是**浏览器本地时区**的 00:00 起。服务端时区未必与浏览器一致
       （容器常挂 TZ=Asia/Shanghai，而浏览器可能在任何时区），让服务端算"今天"
       会在跨时区时切错日子。
     - 「自定义」本来就是用户挑的具体时刻，没有任何服务端推导空间。

   滚动预设（近 N 小时/天）则保留 hours 参数：服务端按整点对齐的滚动窗口与旧
   行为逐位一致，前端自己减 N 小时会多算/少算一个边界桶。 */
const TRANGE_PRESETS = [
  ['today', '今天'],
  ['24', '近 24 小时'],
  ['72', '近 3 天'],
  ['168', '近 7 天'],
  ['720', '近 30 天'],
  ['0', '全部历史'],
  ['custom', '自定义…'],
];
const TRANGE_DEFAULT = '72';
const trangeStates = new Map(); // hostId → { preset, from: Date|null, to: Date|null }

// dtLocalValue / dtLocalParse 与 <input type=datetime-local> 的取值格式互转
// （YYYY-MM-DDTHH:mm，本地时区；ES 里"带时间的日期串"按本地解析，正是我们要的）。
function dtLocalValue(d) {
  const p = n => String(n).padStart(2, '0');
  return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) + 'T' +
    p(d.getHours()) + ':' + p(d.getMinutes());
}
function dtLocalParse(s) {
  if (!s) return null;
  const d = new Date(s);
  return isNaN(d.getTime()) ? null : d;
}

// trangeMidnight 今天 00:00（本地时区）。
function trangeMidnight() {
  const d = new Date();
  d.setHours(0, 0, 0, 0);
  return d;
}

function trangeState(id) {
  if (!trangeStates.has(id)) {
    // 「自定义」的初始值给一段有意义的默认：今天 00:00 → 现在。
    trangeStates.set(id, { preset: TRANGE_DEFAULT, from: trangeMidnight(), to: new Date() });
  }
  return trangeStates.get(id);
}

// trangeRender 画出控件骨架（幂等：重复调用会保留当前状态）。
function trangeRender(id) {
  const host = $(id);
  if (!host) return;
  const st = trangeState(id);
  const custom = st.preset === 'custom';
  host.innerHTML =
    '<select class="tr-preset" aria-label="时间范围">' +
    TRANGE_PRESETS.map(([v, label]) =>
      '<option value="' + v + '"' + (v === st.preset ? ' selected' : '') + '>' + esc(label) + '</option>').join('') +
    '</select>' +
    '<span class="tr-custom"' + (custom ? '' : ' hidden') + '>' +
    '<input type="datetime-local" class="tr-from" value="' + esc(st.from ? dtLocalValue(st.from) : '') + '" aria-label="起始时间">' +
    '<span class="tr-sep">→</span>' +
    '<input type="datetime-local" class="tr-to" value="' + esc(st.to ? dtLocalValue(st.to) : '') + '" aria-label="结束时间">' +
    '</span>';
  const preset = host.querySelector('.tr-preset');
  if (preset) preset.onchange = () => {
    st.preset = preset.value;
    // 从别的预设切到自定义时，把区间重置为"今天 00:00 → 现在"，
    // 免得用户上次留下的半年区间被无声沿用。
    if (st.preset === 'custom' && (!st.from || !st.to)) { st.from = trangeMidnight(); st.to = new Date(); }
    trangeRender(id);
    trangeEmit(id);
  };
  const fromEl = host.querySelector('.tr-from');
  const toEl = host.querySelector('.tr-to');
  const readCustom = () => {
    st.from = dtLocalParse(fromEl.value);
    st.to = dtLocalParse(toEl.value);
    // 起止颠倒就地标红（不静默纠正：用户可能正输到一半）。
    const bad = st.from && st.to && st.from > st.to;
    fromEl.classList.toggle('tr-bad', !!bad);
    toEl.classList.toggle('tr-bad', !!bad);
    if (bad) return;
    trangeEmit(id);
  };
  if (fromEl) fromEl.onchange = readCustom;
  if (toEl) toEl.onchange = readCustom;
}

const trangeHandlers = new Map();
// trangeBind 渲染控件并登记变化回调。**不**在绑定时触发回调：各视图的首次加载
// 由 go() 统一驱动，这里再触发一次会让打开页面时打两遍接口。
function trangeBind(id, onChange, preset) {
  trangeHandlers.set(id, onChange);
  if (preset) trangeState(id).preset = preset;
  trangeRender(id);
}
function trangeEmit(id) {
  const fn = trangeHandlers.get(id);
  if (fn) fn();
}

// trangeQuery 把当前选择翻译成查询参数。
//   rolling=true  → 滚动预设发 hours（服务端整点对齐），今天/自定义发 from/to
//   rolling=false → 一律发 from/to（归档是线性日志，前端算区间更直观）
// 「全部历史」两者都不发。
function trangeQuery(id, rolling) {
  const st = trangeState(id);
  const q = new URLSearchParams();
  const sec = d => Math.floor(d.getTime() / 1000);
  if (st.preset === 'custom') {
    if (st.from) q.set('from', sec(st.from));
    if (st.to) q.set('to', sec(st.to));
    return q;
  }
  if (st.preset === 'today') {
    q.set('from', sec(trangeMidnight()));
    return q;
  }
  // 全部历史：滚动端点（用量）必须**显式**传 hours=0。
  //
  // 后端对「什么都不给」的缺省是 72 小时（见 panel.go 的说明：
  // 「都不给：等同于 hours=72（保持旧调用方行为）」），所以这里返回空 query 会被
  // 当成「近 3 天」—— 正是 issue #121 报的现象：选了「全部历史」，数字却和
  // 「近 3 天」一模一样。
  //
  // 非滚动端点（请求记录）没有缺省窗口：不传 from/to 即"不限起点"，保持空 query。
  if (st.preset === '0') {
    if (rolling) q.set('hours', '0');
    return q;
  }
  if (rolling) { q.set('hours', st.preset); return q; }
  q.set('from', sec(new Date(Date.now() - Number(st.preset) * 3600 * 1000)));
  return q;
}

// trangeLabel 人读口径，用于「用量总览」右上角这类需要回显区间的位置。
function trangeLabel(id) {
  const st = trangeState(id);
  const found = TRANGE_PRESETS.find(p => p[0] === st.preset);
  if (st.preset !== 'custom') return found ? found[1] : '';
  if (!st.from && !st.to) return '自定义';
  const f = d => d ? (d.getMonth() + 1) + '-' + String(d.getDate()).padStart(2, '0') + ' ' +
    String(d.getHours()).padStart(2, '0') + ':' + String(d.getMinutes()).padStart(2, '0') : '…';
  return f(st.from) + ' → ' + f(st.to);
}
function rateLimitMeta(row, now) {
  const model = String(row && row.model || '未知模型');
  const kind = String(row && row.kind || 'rate_limit');
  const resetAt = parseAPITime(row && row.reset_at);
  const until = parseAPITime(row && row.until);
  const deadline = resetAt || until;
  const remaining = deadline > now ? Math.round((deadline - now) / 1000) : 0;
  if (kind === 'model_unavailable') {
    return {
      model,
      kind,
      detail: remaining ? '预计 ' + dur(remaining) + ' 后重试' : '等待重新探测',
      title: model + '\n模型当前不可用' + (deadline ? '\n最早重试：' + fmtLocalDateTime(deadline) : ''),
    };
  }
  const title = [model, resetAt ? '上游重置：' + fmtLocalDateTime(resetAt) : '上游重置：时间未知'];
  // detail 必须显式声明：下面两个分支都只在各自路径里赋值，原先缺声明 ⇒ 严格模式下
  // 走「双时钟」分支时抛 ReferenceError（detail is not defined），renderAccounts 整个
  // 中断、账号池表格变空白。那个分支要求池里真存在 until < reset_at 的账号，平时不
  // 触发；单测把片段切出来单独求值、不带 'use strict'，隐式全局把错误吞了，所以一直
  // 没被发现（v1.13.6 修复）。
  let detail;
  if (until && resetAt && until < resetAt) {
    // 双时钟（issue #171 问题二）：黄框出现/消失由 until 决定（网关为防超长
    // 限流拖死全池，把重试封顶在 now+softRateMax），主文字却按 resetAt 显示，
    // 导致「写的解封时间」与「实际掉框时刻」不一致。此时主文字同时明示两个
    // 时刻与口径，倒计时跟随 until（与实际掉框一致，也与「模型与档位」页
    // 锁池表的 until 口径对齐）。
    const retryIn = until > now ? Math.round((until - now) / 1000) : 0;
    detail = '网关 ' + fmtLocalDateTime(until) + ' 起重试' + (retryIn ? '（剩余 ' + dur(retryIn) + '）' : '') +
      ' · 上游称 ' + fmtLocalDateTime(resetAt) + ' 恢复';
    title.push('网关最早重试：' + fmtLocalDateTime(until));
  } else {
    detail = resetAt
      ? '预计 ' + fmtLocalDateTime(resetAt) + ' 解封' + (remaining ? '（剩余 ' + dur(remaining) + '）' : '')
      : (until ? '预计 ' + fmtLocalDateTime(until) + ' 恢复（剩余 ' + dur(remaining) + '）' : '预计解封时间未知');
  }
  return { model, kind, detail, title: title.join('\n') };
}
function rateLimitRowsHtml(rows, now) {
  const list = Array.isArray(rows) ? rows.filter(row => row && row.model) : [];
  if (!list.length) return '';
  return '<div class="rate-limits">' + list.map(row => {
    const m = rateLimitMeta(row, now);
    return '<div class="rate-limit ' + (m.kind === 'model_unavailable' ? 'model-unavailable' : '') +
      '" title="' + esc(m.title) + '"><b>' + esc(m.model) + '</b><span>' + esc(m.detail) + '</span></div>';
  }).join('') + '</div>';
}

function formatTokenCount(tokens) {
  if (tokens == null || tokens === '') return '—';
  const n = Number(tokens);
  if (!Number.isFinite(n) || n < 0) return '—';
  if (n < 1000) return String(Math.round(n));
  const units = [['k', 1e3], ['m', 1e6], ['b', 1e9]];
  let unit = units[0];
  for (const candidate of units) {
    if (n >= candidate[1]) unit = candidate;
  }
  let value = n / unit[1];
  let rounded = Number(value.toFixed(1));
  // 999999 → 1m，而不是 1000k；四舍五入后自动升级单位。
  const next = units[units.indexOf(unit) + 1];
  if (next && rounded >= 1000) {
    unit = next;
    value = n / unit[1];
    rounded = Number(value.toFixed(1));
  }
  return rounded + unit[0];
}

function formatLatency(ms) {
  if (ms == null || ms === '') return '—';
  const n = Number(ms);
  if (!Number.isFinite(n) || n <= 0) return '—';
  return n < 1000 ? Math.round(n) + 'ms' : (n / 1000).toFixed(1).replace(/\.0$/, '') + 's';
}
function formatRate(rate) {
  if (rate == null || rate === '') return '—';
  const n = Number(rate);
  if (!Number.isFinite(n) || n < 0) return '—';
  return n.toFixed(1) + 'tok/s';
}

/* ── 密钥门 ───────────────────────────────────────────────────────── */
function openKey() { $('keyVeil').classList.add('on'); setTimeout(() => $('keyInput').focus(), 60); }
$('btnKey').onclick = async () => {
  const v = $('keyInput').value.trim();
  if (!v) return;
  localStorage.setItem(LS_KEY, v);
  try {
    await api('overview');
    $('keyErr').hidden = true;
    $('keyVeil').classList.remove('on');
    start();
  } catch (e) { $('keyErr').hidden = false; }
};
$('keyInput').addEventListener('keydown', e => { if (e.key === 'Enter') $('btnKey').click(); });

/* ── 路由 ─────────────────────────────────────────────────────────── */
/* 积分包明细共享缓存：「积分构成」视图与首页「积分到期提醒」卡片共用同一份
   /panel/api/packages 数据（逐账号实时查上游，能省一次是一次）。声明在路由区
   是因为 go() 的初始调用就会触发首页卡片的渲染，必须先于它就位。 */
let lastPackages = null;
let lastPackagesAt = 0;
let expFetching = false;                       // 到期卡片在途标记（防重复打上游）
const EXP_FRESH_MS = 2 * 60 * 1000;            // 缓存新鲜窗口：2 分钟内复用

const TITLES = { accounts: '账号池', usage: '用量', capacity: '容量规划', packages: '积分构成', taskscenter: '任务中心', models: '模型与档位', config: '配置', logs: '运行日志' };
function go(v) {
  view = v;
  document.querySelectorAll('.view').forEach(s => s.hidden = s.id !== 'view-' + v);
  document.querySelectorAll('.nav a').forEach(a => a.classList.toggle('on', a.dataset.view === v));
  $('ttl').textContent = TITLES[v];
  if (v === 'models' && !$('mdBody').children.length) loadModels();
  if (v === 'config') loadConfig();
  if (v === 'logs') loadLogs();
  // 用量页的「路由诊断」三条（sCacheHit / sStickyHit / sAffinityHit）来自 overview 的
  // cache 字段，不属于 loadUsage 的窗口数据，只在 start() 与账号池轮询时更新。若不补拉，
  // 切到本页看到的是「进入页面那一刻」的进程内快照，之后一直是旧值且无任何提示。
  // 进视图拉一次即可（这三条是累计值，不随时间范围变），不塞进 5 秒轮询。
  // 第二参 metricsOnly：本页不显示账号表，跳过 130 行的 renderAccounts 重建——
  // 否则每次切到用量页都会白刷一遍隐藏的账号池表格（用户报的性能问题正是这个）。
  // loadOverview 是 async，不 await：等它返回会拖慢视图切换。
  if (v === 'usage') { loadUsage(); loadOverview(true, true); }
  if (v === 'capacity') loadCapacity();
  if (v === 'packages') loadPackages();
  // 到期提醒卡片：默认可见，进页面即用 overview 快照渲染（零上游请求，随首页统计
  // 每轮刷新）。不再「打开账号池就打上游」——逐账号实时查在账号多时会卡死面板。
  // 完整逐包明细由卡片上的「检查」按钮显式触发（loadPackages/loadExpiry）。
  if (v === 'taskscenter') reattachQueueView();
}
document.querySelectorAll('.nav a').forEach(a => a.onclick = e => { e.preventDefault(); go(a.dataset.view); history.replaceState(null, '', '#' + a.dataset.view); });
/* 首次进入延到本轮脚本求值之后再 go()。
   原因：go() 会同步触发视图的数据加载（loadUsage/loadLogs/loadPackages…），而这些
   函数读到的模块级 let/const（usageRateWarmAt、PK_* 等）在文件后半段才初始化——
   直接深链 #usage / #packages 打开页面时会踩 TDZ（"Cannot access 'x' before
   initialization"），表现为该页永远显示"读取失败"，而点导航进去一切正常。
   延迟 0ms 让整份脚本先求值完，是修这一类问题最省事也最不容易再犯的办法。 */
setTimeout(() => {
  const hash = (location.hash || '#accounts').slice(1);
  go(hash in TITLES ? hash : 'accounts');
}, 0);

/* ── 账号池 ───────────────────────────────────────────────────────── */
/* 账号的冷却态：cool 取「账号级 / 熔断 / 连败降权」三种截止里最晚的那个。
   bucket 是倒计时在**行签名**里的粒度：秒级入签名 = 每轮（5 秒）重建整表，白干；
   完全不入签名又会让「熔断 · 12分30秒」冻在首次渲染那一刻。按分钟入签名后，只有
   冷却中的行会重画、且每分钟最多一次（最后一分钟内才按秒，那时确实该一秒一跳）。 */
function accCool(s) {
  const rest = s.cool_remaining_sec || 0;
  const bl = (new Date(s.breaker_until || 0) - Date.now()) / 1000;
  const dg = (new Date(s.degrade_until || 0) - Date.now()) / 1000;
  const cool = Math.max(rest, bl > 0 ? bl : 0, dg > 0 ? dg : 0);
  const kind = cool <= 0 ? ''
    : bl > Math.max(rest, dg > 0 ? dg : 0) ? '熔断'
      : (dg > rest ? '连败降权' : (s.cool_kind === 'hard_credit' ? '积分冷却' : '限流冷却'));
  const bucket = cool <= 0 ? '' : (cool > 60 ? 'm' + Math.ceil(cool / 60) : 's' + Math.ceil(cool));
  return { cool, kind, bucket };
}
/* 积分条百分比：有总额度按自身 剩余/总额；旧数据无总额 → 退回池内最高=100%。
   企业版不限量（credits_total=-1）恒满格。 */
function accCredPct(s, maxCred) {
  if (s.credits_total === -1) return 100;
  if (s.credits_total > 0) return Math.min(100, Math.round((s.credits || 0) / s.credits_total * 100));
  return Math.round((s.credits || 0) / maxCred * 100);
}
/* accRowSig 行签名：只收「变了就必须重画这一行」的字段。
   **绝不含秒级倒计时**——cool_remaining_sec 每轮都在变，放进来等于每轮全表重建。
   poolMax 也不能直接入签名：它是全池最大值，任何一个号掉分都会把所有行的签名打穿；
   这里放本行算好的百分比，只有真正影响进度条的行才重画。
   两处倒计时（账号冷却、模型级限流）只按分钟桶入签名：完全不收又会让「剩余 5分30秒」
   冻在首次渲染那一刻，那比多画几次更糟。
   ⚠ 已知限制（**不是 bug，别去「修」**）：「最近成功」用 ago()、「导入」用
   importedAtText()，都是随墙上时钟变的相对文案，而签名里只有 last_success/imported_at
   的原值 ⇒ 完全静止的行会停在「5 分钟前」，直到该行别的字段变化才跟着刷新。给时间加
   桶等于每几分钟全表重画，把增量更新的收益吃回去；而真正会冻的只有「既无流量又不在
   冷却」的行，那种行运维看的是绝对值。 */
function accRowSig(s, maxCred) {
  const c = accCool(s);
  const tu = s.token_usage || {};
  const costs = (s.model_costs || []).map(x => x.model + ':' + x.cost_per_1k).join(',');
  const now = Date.now();
  const rateSig = (s.rate_limited_models || []).map(r => {
    const d = parseAPITime(r.until) || parseAPITime(r.reset_at);
    return r.model + '/' + r.kind + '/' + (r.reason || '') + '/' + (d > now ? Math.ceil((d - now) / 60000) : 0);
  }).join(',');
  return [
    s.disabled ? 1 : 0, s.paused ? 1 : 0,
    s.credits, s.credits_total, accCredPct(s, maxCred),
    s.in_flight, s.last_success, s.consecutive_fails, s.success_count, s.err_total,
    rateSig,
    remarkOf(s.uid), c.kind, c.bucket,
    s.nickname, s.realm, s.enterprise, entSession.has(s.uid), s.egress,
    s.checkin_done, s.imported_at, s.reason, costs,
    tu.request_count, tu.total_tokens, tu.last_latency_ms, tu.last_tokens_per_second,
    // 标签与选中态必须进签名：它们决定行内色点与 .sel 底色，
    // 不进签名就会出现「标签存进去了但那一行没变」。
    tagsOf(s.uid).join('+'), accSel.has(s.uid) ? 1 : 0,
  ].join('\u0001');
}
/* 单行 HTML：首次建表与「这一行确实变了」时各走一次。模板与原实现逐字一致。 */
function accRowHtml(s, maxCred) {
  const { cool, kind } = accCool(s);
  let cls = '', tag;
  if (s.disabled) { cls = 'off'; tag = '<span class="tag bad">已禁用</span>'; }
  else if (s.paused) { cls = 'off'; tag = '<span class="tag warn">已暂停选号</span>'; }
  else if (cool > 0) {
    cls = 'cool';
    tag = '<span class="tag warn">' + kind + ' · ' + dur(cool) + '</span>';
  } else tag = '<span class="tag ok">可用</span>' + (s.in_flight ? '' : '');
  const note = s.reason ? '<div class="hint" style="font-size:11.5px;color:var(--ink-3);margin-top:3px">' + esc(s.reason) + '</div>' : '';
  const rateLimits = rateLimitRowsHtml(s.rate_limited_models, Date.now());
  const short = s.uid.length > 16 ? s.uid.slice(0, 16) + '…' : s.uid;
  // 企业版不限量：上游 limitNum == -1，网关以 credits_total=-1 透出（见 upstream
  // enterpriseUnlimitedTotal）。此时剩余额度不参与展示，直接标「不限」。
  const unlimited = s.credits_total === -1;
  const cred = unlimited ? '不限'
    : (s.credits == null ? '—' : (s.credits_total > 0 ? s.credits + '<span class="of">/' + s.credits_total + '</span>' : String(s.credits)));
  const pct = accCredPct(s, maxCred);
  // 成本台账 tooltip（model_costs）：每模型实测单价（≤0 = 实测免费），运维据此
  // 看「为什么总选它」——免费号垄断 / 单价排序一眼可见。
  let credTip;
  if (unlimited) credTip = '企业版不限量（上游 limitNum=-1）';
  else if (s.credits_total > 0) {
    credTip = (s.enterprise ? '企业版剩余额度 ' : '剩余 ')
      + s.credits + ' / ' + (s.enterprise ? '分配 ' : '总额 ') + s.credits_total + '（' + pct + '%）';
  } else credTip = '积分（相对池内最高）';
  const costs = (s.model_costs || []).filter(c => c.model);
  if (costs.length) {
    credTip += '\n实测单价（credits/1K）：\n' + costs.map(c =>
      '  ' + c.model + '：' + (c.cost_per_1k <= 0 ? '免费' : c.cost_per_1k)).join('\n');
  }
  const frozen = s.disabled || cool > 0;
  const tu = s.token_usage || {};
  const req = tu.request_count || 0;
  const totalTok = formatTokenCount(tu.total_tokens);
  const totalTokUnit = totalTok === '—' ? '' : '<em>tok</em>';
  const latency = formatLatency(tu.last_latency_ms);
  const rate = formatRate(tu.last_tokens_per_second);
  const usageTitle = '最近一次：' + req + ' 次 / ' + totalTok + ' / 延迟 ' + latency + ' / ' + rate;
  // data-uid 是增量更新的地基：行级 diff 靠它找回同一行，移动/替换时不用重建整表。
  // 出口串并进 title：密集态里 .id.egress 不占列宽（第三个 inline-block 必折行），
  // 但信息不能丢——悬停整行即可看到。
  const tl = tagsOf(s.uid);
  // 标签用 6px 色点而不是文字：密集态的 .nm 宽度是「昵称 + uid」算死的，
  // 塞进标签文字必折行，行高会从 38px 弹回去。完整标签在 title 里。
  const tagDot = tl.length ? '<i class="acc-tagdot" title="标签：' + esc(tl.join('、')) + '"></i>' : '';
  if (accSel.has(s.uid)) cls += ' sel';
  return '<tr class="' + cls + '" data-uid="' + esc(s.uid) + '" title="uid: ' + esc(s.uid) +
    (s.egress ? ' · 出口: ' + esc(s.egress) : '') + (tl.length ? ' · 标签: ' + esc(tl.join('、')) : '') +
    ' · 双击可编辑标签">' +
    // 色条与复选框同一列：列数恒为 10，mobile.css 的 nth-child 映射才不会错位。
    '<td class="mark"><i aria-hidden="true"></i><input type="checkbox" class="row-chk" data-chk="' +
      esc(s.uid) + '"' + (accSel.has(s.uid) ? ' checked' : '') + ' aria-label="选择该账号"></td>' +
    '<td class="who"><div class="nm">' + remarkName(s.uid, s.nickname ? esc(s.nickname) : '<span style="color:var(--ink-3)">未命名</span>') + tagDot + (s.realm === 'global' ? ' <span class="realm-tag">国际版</span>' : '') + (s.enterprise ? ' <span class="realm-tag">企业版</span>' : '') +
      (entSession.has(s.uid) ? ' <span class="realm-tag warn" title="该凭证绑定企业租户/企业版：行为事件被上游静默丢弃（200 不计分），成长任务与签到不会入账，面板会拦截任务动作。修法：在浏览器退出企业空间，以个人身份重新登录后重新导入。">企业会话·不计分</span>' : '') + '</div><div class="id">' + esc(short) + '</div>' + (s.egress ? '<div class="id egress" title="出口 ' + esc(s.egress) + '">出口 ' + esc(s.egress) + '</div>' : '') + '</td>' +
    '<td>' + tag + note + rateLimits + '</td>' +
    '<td class="cred" title="' + esc(credTip) + '"><div class="n">' + cred + '</div><div class="bar"><i style="width:' + pct + '%"></i></div></td>' +
    '<td class="num">' + (s.success_count || 0) + ' <span style="color:var(--ink-3)">/</span> <span style="color:var(--bad)">' + (s.err_total || 0) + '</span></td>' +
    '<td class="num">' + (s.in_flight || 0) + '</td>' +
    '<td class="num usage-cell" title="' + esc(usageTitle) + '"><span class="usage-line" aria-label="' + esc(usageTitle) + '">' +
      '<span class="usage-item usage-count"><b>' + req + '</b><em>次</em></span>' +
      '<span class="usage-item usage-total"><b>' + totalTok + '</b>' + totalTokUnit + '</span>' +
      '<span class="usage-item usage-latency"><b>' + latency + '</b></span>' +
      '<span class="usage-item usage-rate"><b>' + rate + '</b></span>' +
    '</span></td>' +
    '<td class="num" style="color:var(--ink-3)">' + ago(s.last_success) + '</td>' +
    '<td class="num" style="color:var(--ink-3)" title="' + esc(importedAtTitle(s.imported_at)) + '">' + importedAtText(s.imported_at) + '</td>' +
    '<td class="acts">' +
      // 企业版无个人成长体系（签到 400「企业账号不支持该操作」/ 成长任务 403）：
      // 不渲染「签到」「任务」按钮，只留「额度」——点它走 /balance，企业额度由
      // upstream 的 get-enterprise-user-usage 口径填充。
      (s.enterprise ? '' :
        '<button class="xs ghost" data-a="checkin" data-u="' + esc(s.uid) + '"' + (s.checkin_done ? ' title="今日已签到；点击可重新签到并刷新余额"' : '') + '>' + (s.checkin_done ? '已签' : '签到') + '</button>') +
      // 「额度」按钮只留给企业版：它是企业号**唯一**能用的操作（签到/任务企业版一律
      // 403，故上面那条不渲染），少了它企业号的操作列会空掉。
      // 普通号不再渲染「余额」：额度本来每 5 分钟自动刷新（schedule.balance_refresh），
      // 而「签到」按钮本身也会刷新余额（见其 title）。行内少一个按钮，操作列省约 50px。
      (s.enterprise ? '<button class="xs ghost" data-a="balance" data-u="' + esc(s.uid) + '" title="刷新企业版已分配额度（上游 get-enterprise-user-usage）">额度</button>' : '') +
      (s.enterprise ? '' :
        '<button class="xs ghost" data-a="tasks" data-u="' + esc(s.uid) + '">任务</button>') +
      (frozen ? '<button class="xs primary" data-a="revive" data-u="' + esc(s.uid) + '">解冻</button>'
              : (s.paused ? '<button class="xs primary" data-a="resume" data-u="' + esc(s.uid) + '">恢复选号</button>'
                          : '<button class="xs ghost" data-a="pause" data-u="' + esc(s.uid) + '" title="' + (s.enterprise ? '退出选号，但照常保活 / 刷新额度' : '退出选号，但照常签到 / 活跃上报 / 保活 / 刷新余额') + '">暂停选号</button>')) +
      (s.disabled ? '' : '<button class="xs ghost" data-a="disable" data-u="' + esc(s.uid) + '">禁用</button>') +
      '<button class="xs ghost danger" data-a="remove" data-u="' + esc(s.uid) + '">移除</button>' +
    '</td></tr>';
}

/* 分组键：异常优先。0 = 已禁用，1 = 冷却中，2 = 正常；组内仍按导入时间倒排。
   与行内状态标签同一套判定（disabled 先于 cool），所以「排在上面的」和「标着异常的」
   永远一致——否则会出现一行标着「可用」却夹在冷却号中间。 */
function accRank(s) {
  if (s.disabled) return 0;
  return accCool(s).cool > 0 ? 1 : 2;
}
/* 当前筛选是否命中。口径必须与 accRank / 汇总条一致，否则「条上数了 3 个冷却、
   点进去只有 2 行」这种自相矛盾会立刻被运维当成 bug。 */
function accMatchFilter(s) {
  // 兼容旧调用点（异常汇总条）：真正的筛选口径统一在 accMatches 里，
  // 两处各写一份必然漂移——「条上数了 3 个冷却、点进去只有 2 行」就是这么来的。
  return accMatches(s);
}
/* 异常组 / 正常组的分界哨兵。它带 data-uid 且纳入 accRows 托管——这是硬要求：
   renderAccounts 收尾那句 `while (cursor) remove()` 会把所有非 accRows 管理的节点
   清掉，装饰性节点放进来活不过一轮。 */
const ACC_SEP_KEY = '__sep__';
function accSepHtml(n) {
  // 「其余 N 个号」而不是「正常号」：暂停选号的号既不属于异常组、也不是真正「正常」，
  // 叫「正常」等于给它盖了个错帽子。
  return '<tr class="acc-sep" data-uid="' + ACC_SEP_KEY + '"><td colspan="10">其余 ' + n + ' 个号</td></tr>';
}

/* 账号表的 keyed 增量更新。
   原来是每 5 秒一次 `innerHTML =` 全量重建 130 行、几千个节点，后果：滚动位置回弹、
   hover 的 title tooltip 消失、正在看的行跳到别处、选中的文本被清空、主线程卡顿。
   现在：
     首次      → 仍然一次性 innerHTML 建全表（比逐行 append 快）；
     之后      → 行签名相同就一个字节都不写，不同才重建那一行；新 uid 建行，消失的删行；
     顺序变化  → insertBefore 移动节点（保留节点身份，滚动/选中/tooltip 都不受影响）。
   行内按钮走 accBody 的事件委托 + data-u，所以重建单行不会丢事件绑定。 */
function renderAccounts(list) {
  const tb = $('accBody');
  if (!tb) return;
  const all = list || [];
  // 积分条基准必须取**全池**最大值，与筛选无关：进度条表达的是「这个号还剩多少额度」
  // 的绝对量，基准跟着筛选走就变成相对排名——同一个号切筛选前后百分比会变，
  // 运维会把「筛到只剩低分号时显示满格」误读成「额度很健康」。
  // 顺带一个好处：切筛选时行的 pct 不变，签名也就不会整批失效。
  const maxCred = Math.max(1, ...all.map(s => s.credits || 0));
  // ① 过滤 → ② 排序 → ③ 分页，全在内存里完成（数据源仍是 overview 快照）。
  // 顺序不能换：先分页再排序会让「第 2 页」的含义随排序变，翻页时内容乱跳。
  const shown = all.filter(accMatches);
  const ranked = accSortList(shown);
  const total = ranked.length;
  const size = accView.size > 0 ? accView.size : Math.max(1, total);
  const pages = Math.max(1, Math.ceil(total / size));
  // 页码夹紧：筛选后总数变少时，原来停在第 5 页会看到空表（最容易被当成 bug）。
  if (accView.page > pages) accView.page = pages;
  if (accView.page < 1) accView.page = 1;
  const start = (accView.page - 1) * size;
  const pageRows = ranked.slice(start, start + size);

  // 工具栏与分页器每次都刷（计数随快照变），内部用 dataset.sig 比对，没变就不写 DOM。
  renderAccStatusChips(all);
  renderAccTagChips();
  renderAccPager(total, pages, start, pageRows.length);
  syncAccChkAll(pageRows);
  renderBulkBar();

  if (!pageRows.length) {
    accRows.clear();
    // 两种空态要分开：「池子空」和「筛选没命中」的下一步动作完全不同。
    tb.innerHTML = all.length
      ? '<tr><td colspan="10"><div class="empty"><div class="big">没有匹配的账号</div>' +
        esc(accFilterHint()) + '，' +
        '<button class="xs ghost" data-filter="all">清除全部筛选</button></div></td></tr>'
      : '<tr><td colspan="10"><div class="empty"><div class="big">账号池是空的</div>点击右上角「添加账号」，用浏览器登录一个 WorkBuddy 账号</div></td></tr>';
    applyFolds();
    return;
  }
  // 分界哨兵只在「默认排序 + 未筛选 + 单页」时出现：分页后插入分界会随页码漂移，
  // 反而制造「上一页末尾也有个分界」的错觉。异常汇总条始终在，信息不会丢。
  const abnN = ranked.reduce((n, x) => n + (accRank(x) < 2 ? 1 : 0), 0);
  const sepN = (accView.sort === 'default' && accView.status === 'all' && !accView.q &&
                !accView.tags.length && size >= total && abnN > 0 && abnN < ranked.length)
    ? ranked.length - abnN : 0;
  // items 是本轮期望的完整行序（账号行 + 可能的哨兵）。html 走惰性求值：
  // 签名没变的行连字符串都不拼，这是增量更新省下来的主要成本。
  const items = [];
  pageRows.forEach((s, i) => {
    items.push({ key: s.uid, sig: accRowSig(s, maxCred), html: () => accRowHtml(s, maxCred) });
    if (sepN && i === abnN - 1) {
      items.push({ key: ACC_SEP_KEY, sig: ACC_SEP_KEY + ':' + sepN, html: () => accSepHtml(sepN) });
    }
  });
  if (!accRows.size) {
    tb.innerHTML = items.map(it => it.html()).join('');
    items.forEach((it, i) => accRows.set(it.key, { el: tb.children[i], sig: it.sig }));
    applyFolds();
    return;
  }
  const seen = new Set(items.map(it => it.key));
  for (const [key, row] of accRows) {
    if (seen.has(key)) continue;
    row.el.remove();
    accRows.delete(key);
  }
  // 一个游标走完新顺序：对得上就不动，对不上就 insertBefore 拽过来（移动保留节点身份）。
  // 处理过的行总在游标之前，所以游标之后剩下的必然是这轮不该存在的节点。
  let cursor = tb.firstElementChild;
  for (const it of items) {
    let row = accRows.get(it.key);
    const atCursor = !!row && row.el === cursor;
    if (!row) {
      row = { el: trFromHtml(it.html()), sig: it.sig };
      accRows.set(it.key, row);
    } else if (row.sig !== it.sig) {
      const fresh = trFromHtml(it.html());
      row.el.replaceWith(fresh);
      row.el = fresh;
      row.sig = it.sig;
    }
    if (atCursor) cursor = row.el.nextElementSibling;
    else tb.insertBefore(row.el, cursor);
  }
  while (cursor) { const next = cursor.nextElementSibling; cursor.remove(); cursor = next; }
  applyFolds();
}
/* trFromHtml 用 <template> 解析成节点：不执行脚本、不加载资源，也不像写活 DOM 那样
   让整张表重排重绘。 */
function trFromHtml(html) {
  const tpl = document.createElement('template');
  tpl.innerHTML = html;
  return tpl.content.firstElementChild;
}

/* ── 异常汇总条 ───────────────────────────────────────────────────── */
/* 全池健康时整条 hidden（[hidden] 是 display:none!important，0 高 0 边距，不占位）——
   留一条常驻的空条等于永远浪费 44px，而且「空条」会被读成「一切正常」的装饰，
   反而弱化了异常时的存在感。
   计数口径与 accRank / 行内标签完全一致：已禁用优先（禁用的号即使还在冷却也只算
   「已禁用」），否则看 accCool().cool。 */
function renderAlertBar(accounts) {
  const bar = $('alertBar');
  if (!bar) return;
  let offN = 0, coolN = 0;
  (accounts || []).forEach(s => {
    if (s.disabled) offN++;
    else if (accCool(s).cool > 0) coolN++;
  });
  const total = offN + coolN;
  if (!total) { bar.hidden = true; return; }
  bar.hidden = false;
  // 有点「已禁用」就整条转红：禁用是终态（要人工解冻），冷却会自己好，两者不该同色。
  bar.classList.toggle('sev-bad', offN > 0);
  const dot = $('alertDot');
  if (dot) {
    dot.classList.toggle('bad', offN > 0);
    dot.classList.toggle('warn', offN === 0);
  }
  $('alertText').textContent = total + ' 个号需处理';
  // 计数为 0 的 chip 不渲染：0 是噪声，还会让人以为「点进去有东西」。
  const chips = [];
  if (coolN) chips.push('<button class="xs chip' + (accFilter === 'cool' ? ' on' : '') + '" data-filter="cool">冷却<span class="n">' + coolN + '</span></button>');
  if (offN) chips.push('<button class="xs chip' + (accFilter === 'off' ? ' on' : '') + '" data-filter="off">已禁用<span class="n">' + offN + '</span></button>');
  // 内容没变就不写：每 5 秒重写 innerHTML 会把鼠标正悬停的 chip 的 :hover 态闪掉。
  const box = $('alertChips');
  const html = chips.join('');
  if (box.dataset.sig !== html) {
    box.innerHTML = html;
    box.dataset.sig = html;
  }
}
/* 切换筛选。同一个 chip 再点一次回到 all——否则进了「只看冷却」就没有出口，
   只能刷新页面（汇总条本身在筛选态下仍然显示，不构成 issue #137 那种死锁）。 */
function setAccFilter(f) {
  accFilter = (f === 'cool' || f === 'off') ? f : 'all';
  accView.status = accFilter;
  // 'all' 语义是「清除全部筛选」：搜索词和标签一起清，否则点了「显示全部」
  // 还留着搜索词，屏幕上依然只有几行——那是最容易被当成 bug 的交互。
  if (f === 'all') {
    accView.q = '';
    accView.tags = [];
    if ($('accQ')) $('accQ').value = '';
    syncAccSearchCls();
  }
  accView.page = 1;
  saveAccView();
  try { localStorage.setItem(LS_ACC_FILTER, accFilter); } catch (e) {}
  const accounts = (overviewData && overviewData.accounts) || [];
  renderAccounts(accounts);
  renderAlertBar(accounts);
  syncAccControls();
}
if ($('alertChips')) {
  $('alertChips').addEventListener('click', ev => {
    const chip = ev.target.closest('[data-filter]');
    if (!chip) return;
    const f = chip.getAttribute('data-filter');
    setAccFilter(accFilter === f ? 'all' : f);
  });
}
/* 筛选空态里那个「显示全部」按钮。单独挂一个委托（现有 accBody 监听只认
   button[data-a]，不会误吞它）。 */
$('accBody').addEventListener('click', ev => {
  const b = ev.target.closest('[data-filter]');
  if (b) setAccFilter(b.getAttribute('data-filter'));
});

// renderModelLocks 模型锁池：哪些模型不能用、锁了几个号、还要锁多久。
// 后端 model_locks 已按「整池不可用 → 没号可用 → 部分限流」排好序，这里只做展示。
function renderModelLocks(rows) {
  const tb = $('mlBody');
  if (!tb) return;
  const note = $('mlNote');
  if (!rows || !rows.length) {
    tb.innerHTML = '<tr><td colspan="8"><div class="empty">当前没有模型级限流 —— 所有模型均可选</div></td></tr>';
    if (note) note.textContent = '';
    return;
  }
  const STATE = { locked: ['bad', '整池不可用'], starved: ['warn', '没号可用'], partial: ['warn', '部分限流'] };
  const left = iso => {
    const ms = parseAPITime(iso);
    return ms ? dur(Math.max(0, Math.round((ms - Date.now()) / 1000))) : '—';
  };
  tb.innerHTML = rows.map(r => {
    const st = STATE[r.state] || ['mute', r.state || '—'];
    const realm = r.realm === 'global' ? '国际版' : '国内版';
    return '<tr>' +
      '<td>' + esc(r.model) + '</td>' +
      '<td><span class="realm-tag">' + realm + '</span></td>' +
      '<td><span class="tag ' + st[0] + '">' + st[1] + '</span></td>' +
      '<td class="num">' + (r.servable || 0) + ' / ' + (r.total || 0) + '</td>' +
      '<td class="num">' + (r.locked || 0) + '</td>' +
      '<td class="num">' + left(r.unlock_at || r.fully_unlock_at) + '</td>' +
      '<td class="num">' + left(r.fully_unlock_at) + '</td>' +
      '<td>' + (r.reason ? '<div class="note">' + esc(r.reason) + '</div>' : '—') + '</td>' +
      '</tr>';
  }).join('');
  if (note) {
    const bad = rows.filter(r => r.state === 'locked' || r.state === 'starved').length;
    note.textContent = bad ? bad + ' 个模型整池不可用' : rows.length + ' 个模型部分限流';
  }
}

/* fmtCredShort 积分量缩写，专供 .stats 的「积分剩余 / 总额」用。
   与 fmtK 的区别：fmtK 是整千取整 + 不带 M（用在模型成本等处，别把两者合并，
   改 fmtK 会连锁到那些地方）。
   精度是被**版面**逼出来的，不是审美：26px 等宽字宽约 15.1px，而 .stats 在 1280px
   下把 7 格均分到 145px（内边距 17×2 后内容区只有 ~111px），一格里要放"剩余 / 总额"
   两个数。所以每个数字压到 ≤4 字符：12480 → 12K（1 位小数会变成 12.5K，两个数加
   斜杠共 11 字符 ≈ 166px，仍然撑爆）。精确值始终挂在 title 上。 */
function fmtCredShort(n) {
  n = Number(n || 0);
  const short = v => (v >= 10 ? String(Math.round(v)) : String(Math.round(v * 10) / 10));
  if (n >= 1e6) return short(n / 1e6) + 'M';
  if (n >= 1e3) return short(n / 1e3) + 'K';
  return String(n);
}
/* fmtCredPair 「剩余/总额」的紧凑写法：两个数**共享单位**，单位只写一次
   （1.6/5.2M，而不是 1.6M/5.2M）。
   为什么非要省这两个字符：26px 等宽下 9 字符 ≈ 136px，而 .stats 的单元格最窄只有
   132px（auto-fit 的 132px 下限，窗口 ≈1190 时命中），136px 会直接顶到右邻
   「粘性会话」的「1」上（截图实测两者贴在一起）；8 字符 ≈ 121px 才留得出缝隙。 */
function fmtCredPair(rem, tot) {
  rem = Number(rem || 0);
  tot = Number(tot || 0);
  const short = v => (v >= 10 ? String(Math.round(v)) : String(Math.round(v * 10) / 10));
  if (tot >= 1e6) return short(rem / 1e6) + '/' + short(tot / 1e6) + 'M';
  if (tot >= 1e3) return short(rem / 1e3) + '/' + short(tot / 1e3) + 'K';
  return String(Math.round(rem)) + '/' + String(Math.round(tot));
}

/* loadOverview(quiet, metricsOnly)
   quiet       — 失败不弹 toast（轮询/切视图用）。
   metricsOnly — 只更新顶部与统计量，跳过账号表 / 模型锁池 / 到期卡的重建。用量页
                 调它补「路由诊断」三条时用：那页看不见账号表，130 行 innerHTML
                 白刷一遍纯属浪费，而这个面板的性能痛点正是全量重建。 */
async function loadOverview(quiet, metricsOnly) {
  try {
    const d = await api('overview');
    overviewData = d;
    entSession = new Set(d.enterprise_session || []);
    $('sTotal').textContent = d.total;
    $('sHealthy').textContent = d.healthy;
    $('sCooling').textContent = d.cooling;
    $('sDisabled').textContent = d.disabled;
    // 「剔除封禁号」按钮的计数：banned 是 disabled 中「被上游判死」的子集，口径由
    // 后端 IsUpstreamBannedReason 统一算（前端不重复判定，避免两套正则漂移）。
    if ($('btnPruneBanned')) {
      const bn = d.banned || 0;
      $('btnPruneBanned').textContent = bn ? '剔除封禁号(' + bn + ')' : '剔除封禁号';
    }
    // 暂停选号单列（issue #125）：它只关选号、照常签到保活，与禁用是两种状态。
    if ($('sPaused')) $('sPaused').textContent = d.paused == null ? '-' : d.paused;
    const remSum = (d.accounts || []).reduce((a, s) => a + (s.credits || 0), 0);
    const totSum = (d.accounts || []).reduce((a, s) => a + (s.credits_total || 0), 0);
    // 全值（如 12,480 / 40,000 ≈ 227px）会撑破 .stats 的单元格（auto-fit 在 1280px 下
    // 把 7 格均分到 145px，内容区只有 ~111px），溢出后压在右邻「粘性会话」上。
    // 用共享单位的 K/M 缩写压到 ~121px，精确值挂 title。不改 .stats 列宽——7 视图共用。
    $('sCredits').textContent = totSum > 0 ? fmtCredPair(remSum, totSum) : fmtCredShort(remSum);
    $('sCredits').parentNode.title = '剩余 ' + remSum + ' / 总额 ' + totSum;
    $('sSticky').textContent = d.sticky_sessions;
    // 缓存命中率 / 粘性命中率：分母为 0 时显示 "-"（不是 0%——0% 会被误读成
    // "缓存完全不工作"，而实际只是还没有带 cache 维度的请求）。
    const cm = d.cache || {};
    if ($('sCacheHit')) {
      const den = (cm.cache_hit_tokens || 0) + (cm.cache_miss_tokens || 0);
      $('sCacheHit').textContent = den ? cm.cache_hit_rate + '%' : '-';
    }
    if ($('sStickyHit')) {
      const den = (cm.sticky_hit || 0) + (cm.sticky_busy || 0) + (cm.sticky_miss || 0) + (cm.sticky_new || 0);
      $('sStickyHit').textContent = den ? cm.sticky_hit_rate + '%' : '-';
      // 悬停看明细：同号 / 满载溢出 / 换号重分配 / 新会话。
      $('sStickyHit').parentNode.title = den
        ? 'hit ' + (cm.sticky_hit || 0) + ' · 满载溢出 ' + (cm.sticky_busy || 0) +
          ' · 换号 ' + (cm.sticky_miss || 0) + ' · 新会话 ' + (cm.sticky_new || 0)
        : '还没有带会话键的请求';
    }
    // 前缀亲和：分母为 0 时显示 "-"（0% 会被误读成「机制失效」，实际只是还没有
    // 新建会话）。
    if ($('sAffinityHit')) {
      const den = (cm.affinity_hit || 0) + (cm.affinity_miss || 0) + (cm.affinity_new || 0);
      $('sAffinityHit').textContent = den ? cm.affinity_hit_rate + '%' : '-';
      // 附上前缀种数：命中率为 0 时，种数≈新建次数说明前缀在漂移（客户端 system 里
      // 有会变的内容），种数涨得慢只是还没凑到同前缀的第二个会话。两者都叫 0%，
      // 但一个要改指纹口径、一个什么都不用做。
      const np = d.affinity_prefixes || 0;
      // 前缀占比决定「命中率值多少钱」：占比 5% 时 100% 命中也只省 5% 的 token。
      // 没有前缀体积样本时明说，别让人把手里的 0% 当成机制失效。
      const share = cm.prefix_share_est || 0;
      const shareLine = share > 0
        ? '前缀占 prompt 约 ' + share + '%（平均 ' + Math.round(cm.prefix_avg_bytes || 0) + ' 字节）：' +
          (share < 20 ? '可省空间有限，命中率天然高不了' : '可省空间大，命中率值得追')
        : '还没有前缀体积样本';
      $('sAffinityHit').parentNode.title = den
        ? '同前缀命中 ' + (cm.affinity_hit || 0) + ' · 满载被带离 ' + (cm.affinity_miss || 0) +
          ' · 新建落点 ' + (cm.affinity_new || 0) + '（只统计会话尚无绑定的请求）\n' +
          '已注册前缀 ' + np + ' 种：接近新建次数说明前缀在漂移，远小于则只是样本还没重复\n' +
          shareLine
        : '还没有需要分配落点的新会话（已注册前缀 ' + np + ' 种）\n' + shareLine;
    }
    $('navSub').textContent = 'v' + d.version;
    $('navVer').textContent = 'v' + d.version;
    $('navRedis').textContent = d.redis_mode === 'upstash' ? 'Redis 镜像' : '本地内存';
    $('navState').textContent = d.healthy > 0 ? '服务正常' : (d.total ? '无可用账号' : '待添加账号');
    const p = $('navPulse');
    p.className = 'pulse' + (d.healthy > 0 ? '' : (d.total ? ' warn' : ' bad'));
    $('accNote').textContent = d.in_flight_full ? d.in_flight_full + ' 个账号在途占满' : '';
    const up = Math.floor(d.uptime_sec);
    $('subMeta').textContent = '运行 ' + (up >= 86400 ? Math.floor(up / 86400) + ' 天 ' : '') + Math.floor(up % 86400 / 3600) + ' 时 ' + Math.floor(up % 3600 / 60) + ' 分';
    if (!metricsOnly) {
      // 同一份 accounts 既喂表格也喂汇总条：两个视图的数字必须来自同一次快照，
      // 否则会看到「条上 3 个需处理、表里 2 行异常」这种自相矛盾。
      const accs = d.accounts || [];
      renderAccounts(accs);
      renderAlertBar(accs);
      renderModelLocks(d.model_locks);
      // 到期卡片跟随首页统计同步刷新（用同一份响应里的按日批次，不额外发请求）。
      // 2 分钟内的实时明细最新且更全，保留；过期后由 state 快照接管，避免点过一次
      // 「检查」之后卡片永远停在那一刻的旧数据。
      const liveFresh = lastPackages && (Date.now() - lastPackagesAt) < EXP_FRESH_MS;
      if ($('expBox') && !liveFresh) renderExpirySnapshot(d);
    }
  } catch (e) { if (!quiet) toast(e.message, 'err'); }
}

$('accBody').addEventListener('click', async ev => {
  const b = ev.target.closest('button[data-a]');
  if (!b) return;
  const u = b.dataset.u, a = b.dataset.a;
  if (a === 'remove' && !confirm('移除账号将删除池状态与 auths/ 下的凭证文件，且不可恢复。确认移除？')) return;
  if (a === 'disable' && !confirm('禁用后该账号不再参与选号（保号任务默认也跳过），需手动解冻才能恢复。若只是想临时让位、仍要保号，请改用「暂停选号」。确认禁用？')) return;
  b.disabled = true;
  try {
    if (a === 'checkin') {
      const r = await api('accounts/' + encodeURIComponent(u) + '/checkin', { method: 'POST' });
      toast('签到完成' + (r.credits != null ? '，积分 ' + r.credits + (r.credits_total > 0 ? '/' + r.credits_total : '') : '') + (r.checkin_message ? '（' + r.checkin_message + '）' : ''), 'ok');
    } else if (a === 'balance') {
      const r = await api('accounts/' + encodeURIComponent(u) + '/balance', { method: 'POST' });
      toast('余额已更新：' + r.credits + (r.credits_total > 0 ? ' / ' + r.credits_total : ''), 'ok');
    } else if (a === 'revive') {
      await api('accounts/' + encodeURIComponent(u) + '/revive', { method: 'POST' });
      toast('已解冻', 'ok');
    } else if (a === 'disable') {
      await api('accounts/' + encodeURIComponent(u) + '/disable', { method: 'POST' });
      toast('已禁用', 'ok');
    } else if (a === 'pause') {
      await api('accounts/' + encodeURIComponent(u) + '/pause', { method: 'POST' });
      toast('已暂停选号（签到 / 保活照常）', 'ok');
    } else if (a === 'resume') {
      await api('accounts/' + encodeURIComponent(u) + '/resume', { method: 'POST' });
      toast('已恢复选号', 'ok');
    } else if (a === 'tasks') {
      openTasks(u);
    } else if (a === 'remove') {
      const r = await api('accounts/' + encodeURIComponent(u) + '/remove', { method: 'POST' });
      toast(r.file_error ? '已移除（凭证文件删除失败：' + r.file_error + '）' : '已移除', 'ok');
    }
  } catch (e) { toast(e.message, 'err'); }
  finally { b.disabled = false; loadOverview(true); }
});

$('btnCheckinAll').onclick = async () => {
  try { await api('checkin_all', { method: 'POST' }); toast('全部签到已开始，结果见日志', 'ok'); }
  catch (e) { toast(e.message, 'err'); }
};
$('btnKeepaliveAll').onclick = async () => {
  try { await api('keepalive_all', { method: 'POST' }); toast('全部保活已开始，结果见日志', 'ok'); }
  catch (e) { toast(e.message, 'err'); }
};
$('btnTravelAll').onclick = async () => {
  try { await api('travel_all', { method: 'POST' }); toast('旅行巡检已开始（含领养链路），结果见日志', 'ok'); }
  catch (e) { toast(e.message, 'err'); }
};
$('btnActivityAll').onclick = async () => {
  try { await api('activity_all', { method: 'POST' }); toast('活跃上报已开始，结果见日志', 'ok'); }
  catch (e) { toast(e.message, 'err'); }
};
/* 剔除封禁号：两段式（预演 → 确认 → 按确认清单执行）。
   单段式「点一下就删」在这个接口上不可接受：一次可能删掉上百个账号的凭证，
   而运维看不出到底删的是哪些。预演回的是逐号 uid/昵称/原因/积分，确认框把它们
   摊开，真删时再把这份 uid 清单回传，后端只动这批（期间新封的号留到下次）。 */
$('btnPruneBanned').onclick = async ev => {
  const b = ev.currentTarget;
  b.disabled = true;
  try {
    const pre = await api('accounts/prune_banned', { method: 'POST', body: JSON.stringify({ dry_run: true }) });
    if (!pre.count) { toast('当前没有因上游封禁而废掉的账号', 'ok'); return; }
    const lines = (pre.accounts || []).slice(0, 6).map(s => '· ' + (s.nickname || String(s.uid).slice(0, 12)));
    if (pre.count > lines.length) lines.push('… 等共 ' + pre.count + ' 个');
    const msg = '将剔除 ' + pre.count + ' 个被上游封禁的账号：\n\n' + lines.join('\n')
      + '\n\n冻结积分合计 ' + (pre.frozen_credits || 0) + '（剔除后不再计入池内余额）'
      + '\n凭证文件不删除，归档到：\n' + (pre.archive_dir || '(auths-pruned/)')
      + '\n\n只处理上游封禁号（11140 request illegal 等）；人工禁用、14017 试用未激活、'
      + '冷却中的账号不受影响。\n\n确认剔除？';
    if (!confirm(msg)) return;
    const r = await api('accounts/prune_banned', {
      method: 'POST',
      body: JSON.stringify({ dry_run: false, uids: (pre.accounts || []).map(s => s.uid) }),
    });
    const bad = (r.failed || []).length;
    toast('已剔除 ' + r.removed + ' 个封禁号'
      + (r.archived ? '，凭证已归档 ' + r.archived + ' 份' : '')
      + (bad ? '；' + bad + ' 个凭证归档失败（见日志，重启会把它们载回池子）' : ''),
      bad ? 'err' : 'ok');
  } catch (e) { toast(e.message, 'err'); }
  finally { b.disabled = false; loadOverview(true); }
};

/* ── 模型 ─────────────────────────────────────────────────────────── */
/* 实测上限标注：scripts/probe_max_tokens.py --panel-out 写入探测结果，
   /panel/api/model_probes 只读透传。探测键带域前缀（cn:glm-5.2），模型表
   显示裸名，按「精确命中或 :后缀」关联。无数据时本列退回上游声称值。 */
function fmtK(n) { n = Number(n || 0); return n >= 1000 ? Math.round(n / 1000) + 'K' : String(n); }
function probeDays(ts) {
  if (!ts) return null;
  const t = new Date(String(ts).replace(' ', 'T'));
  const d = (Date.now() - t.getTime()) / 86400000;
  return isNaN(d) ? null : Math.floor(d);
}
function outCell(m, pr) {
  if (!pr) return '<td class="num">' + (m.max_output_tokens ? fmtK(m.max_output_tokens) : '—') + '</td>';
  const tip = '声称 ' + (pr.claimed ? fmtK(pr.claimed) : '?') + ' · 实测 ' + (pr.measured ? fmtK(pr.measured) : '?') +
    (pr.note ? ' · ' + pr.note : '') + (pr.tested_at ? ' · 探测于 ' + pr.tested_at : '');
  const days = probeDays(pr.tested_at);
  const stale = days !== null && days > 30 ? ' · ' + days + ' 天前' : '';
  if (pr.verdict === 'clamped' && pr.measured) {
    if (pr.claimed && pr.measured < pr.claimed) {
      const x = pr.claimed / pr.measured;
      const xs = (x >= 10 ? Math.round(x) : Math.round(x * 10) / 10) + '×';
      return '<td class="num" title="' + esc(tip) + '"><span style="color:var(--warn);font-weight:600">' +
        fmtK(pr.measured) + ' ⚠</span><div class="note">钳制 ' + xs + stale + '</div></td>';
    }
    return '<td class="num" title="' + esc(tip) + '"><span style="color:var(--ok)">' + fmtK(pr.measured) +
      (pr.claimed && pr.measured > pr.claimed ? ' ↑' : ' ✓') + '</span></td>';
  }
  if (pr.verdict === 'at_least' && pr.measured)
    return '<td class="num" title="' + esc(tip) + '"><span style="color:var(--ink-3)">≥' + fmtK(pr.measured) + '</span></td>';
  return '<td class="num" title="' + esc(tip) + '"><span style="color:var(--ink-3)">?</span><div class="note">未测出' + stale + '</div></td>';
}

/* rateCell 倍率列：牌价 vs 生效价。上游 credits 是牌价（转正后基准倍率），
   modelPromotions 给当前生效折扣（限时免费 factor=0 / 夜间五折 0.5 等）——
   WorkBuddy 客户端显示的正是生效价。有折扣：生效价大字 + 标签 + 划线牌价，
   悬停带时段说明；无 factor 只有标签（错峰类）：牌价 + 标签。 */
function rateCell(m) {
  const tip = m.promo_note ? ' title="' + esc(m.promo_note) + '"' : '';
  if (m.promo_factor != null && m.promo_credits) {
    const base = m.credits ? ' <s style="color:var(--ink-3);font-size:11.5px">' + esc(m.credits) + '</s>' : '';
    const label = m.promo_label ? ' <span class="tag ok">' + esc(m.promo_label) + '</span>' : '';
    return '<span' + tip + ' style="cursor:help"><b>' + esc(m.promo_credits) + '</b>' + label + base + '</span>';
  }
  if (m.promo_label) {
    return '<span' + tip + ' style="cursor:help">' + (m.credits ? esc(m.credits) : '—') +
      ' <span class="tag warn">' + esc(m.promo_label) + '</span></span>';
  }
  return m.credits ? esc(m.credits) : '—';
}

async function loadModels() {
  const tb = $('mdBody');
  tb.innerHTML = '<tr><td colspan="8"><div class="empty">正在向上游查询…</div></td></tr>';
  try {
    // 探测、模型锁与最近请求都是本地可选增强：任一失败不影响模型目录本身。
    // 可用性标签不额外发推理请求，只用模型锁和最近真实请求结果合成。
    const [d, pr, st, rm] = await Promise.all([
      api('models'),
      api('model_probes').catch(() => ({})),
      api('status').catch(() => ({})),
      api('request_metrics').catch(() => ({})),
    ]);
    mdAll = d.models || [];
    mdProbes = pr.probes || {};
    mdLocks = Array.isArray(st.model_locks) ? st.model_locks : [];
    mdRecent = Array.isArray(rm.recent) ? rm.recent : [];
    if (!mdAll.length) {
      tb.innerHTML = '<tr><td colspan="8"><div class="empty">上游未返回模型</div></td></tr>';
      $('mdCount').textContent = '';
      $('mdNote').textContent = '上游未返回模型';
      return;
    }
    // 探测键带域前缀（cn:glm-5.2），模型表显示裸名，按「精确命中或 :后缀」关联。
    const probeKeys = Object.keys(mdProbes);
    mdProbeOf = id => mdProbes[id] || mdProbes[probeKeys.find(k => k.endsWith(':' + id))];
    const hit = mdAll.filter(m => mdProbeOf(m.id)).length;
    const warnings = Array.isArray(d.warnings) ? d.warnings : [];
    $('mdNote').textContent = mdAll.length + ' 个模型/功能 · 已刷新降级缓存' +
      (hit ? ' · ' + hit + ' 个有实测上限' : '') +
      (warnings.length ? ' · 部分目录失败：' + warnings.join('；') : '');
    $('mdNote').title = warnings.join('\n');
    renderModels();
  } catch (e) {
    mdAll = [];
    tb.innerHTML = '<tr><td colspan="8"><div class="empty">' + esc(e.message) + '</div></td></tr>';
    $('mdCount').textContent = '';
  }
}

/* ── 模型筛选（按条件查询）───────────────────────────────────────────
   模型目录一次拉全（几十条），筛选与排序全部在前端完成：改条件零延迟，且不会
   因为调一次筛选就打一次上游——/panel/api/models 是直连上游的实时查询，很贵。
   条件之间是 AND；每个条件为空即不参与判定。 */
// mdRateValue 当前生效的积分倍率数值：优先促销价（限时免费 = 0），无倍率记为
// Infinity 排到最后（排序时"没有价格"不该冒充最便宜）。
function mdRateValue(m) {
  const raw = (m.promo_credits != null && m.promo_credits !== '') ? m.promo_credits : m.credits;
  const n = parseFloat(String(raw == null ? '' : raw).replace(/[^\d.]/g, ''));
  return Number.isFinite(n) ? n : Infinity;
}

// mdSearchText 参与关键字搜索的字段（ID / 展示名 / 厂商 / 描述 / 标签）。
function mdSearchText(m) {
  return [m.id, m.name, m.vendor, m.description, (m.tags || []).join(' ')]
    .filter(Boolean).join(' ').toLowerCase();
}

// mdMatch 单个模型是否满足全部筛选条件。
function mdMatch(m, f) {
  f = f || mdFilter;
  if (f.q) {
    const text = mdSearchText(m);
    // 空格分词后逐个匹配：多关键词是 AND，便于"cn 视觉"这类组合查询。
    for (const kw of f.q.toLowerCase().split(/\s+/).filter(Boolean)) {
      if (!text.includes(kw)) return false;
    }
  }
  if (f.realm && !String(m.id || '').startsWith(f.realm + ':')) return false;
  if (f.type && String(m.type || 'chat') !== f.type) return false;
  if (f.cap === 'tool' && !m.supports_tool_call) return false;
  if (f.cap === 'vision' && !m.supports_images) return false;
  if (f.cap === 'reasoning' && !m.supports_reasoning) return false;
  if (f.cap === 'default' && !m.is_default) return false;
  if (f.effort === 'off') {
    if (!m.can_disable_thinking) return false;
  } else if (f.effort && !(m.supported_efforts || []).includes(f.effort)) {
    return false;
  }
  const factor = m.promo_factor == null ? null : Number(m.promo_factor);
  if (f.promo === 'promo' && factor == null && !m.promo_label) return false;
  if (f.promo === 'free' && !(factor === 0)) return false;
  if (f.promo === 'discount' && !(factor != null && factor > 0)) return false;
  return true;
}

// mdSortList 按当前排序条件返回新数组（不改动入参，保持上游原始顺序可回溯）。
function mdSortList(list, f) {
  f = f || mdFilter;
  const out = list.slice();
  const num = v => { const n = Number(v || 0); return Number.isFinite(n) ? n : 0; };
  if (f.sort === 'rate') out.sort((a, b) => mdRateValue(a) - mdRateValue(b));
  else if (f.sort === 'context') out.sort((a, b) => num(b.context_length) - num(a.context_length));
  else if (f.sort === 'output') out.sort((a, b) => num(b.max_output_tokens) - num(a.max_output_tokens));
  else if (f.sort === 'name') out.sort((a, b) => String(a.id || '').localeCompare(String(b.id || '')));
  return out;
}

// mdAvailabilityOf 合成 Global 模型当前可用性：模型锁优先于最近请求结果。
// 只返回有真实本地证据的状态；目录中存在但从未请求过的模型标为“待验证”，
// 避免把“上游列出了模型”误当成“当前账号有权限”。
function mdAvailabilityOf(m, locks, recent) {
  const id = String((m && m.id) || '');
  if (!id.startsWith('global:')) return null;
  locks = Array.isArray(locks) ? locks : mdLocks;
  recent = Array.isArray(recent) ? recent : mdRecent;
  const bare = id.slice('global:'.length);
  const lock = locks.find(r => r && r.realm === 'global' && r.model === bare);
  if (lock) {
    const reason = String(lock.reason || '').trim();
    const lower = reason.toLowerCase();
    if (lock.state === 'partial') {
      return { key: 'partial', label: '部分可用', cls: 'warn',
        tip: '仍有 ' + Number(lock.servable || 0) + ' 个 global 账号可服务；已锁 ' + Number(lock.locked || 0) + ' 个' + (reason ? '；' + reason : '') };
    }
    if (lock.state === 'starved') {
      return { key: 'starved', label: '账号暂不可用', cls: 'mute',
        tip: '没有账号能服务该模型，但并非该模型自身被锁；请检查账号冷却/在途占用' };
    }
    if (lower.startsWith('11158') || lower.includes('not allowed for user type')) {
      return { key: 'denied', label: '无权限', cls: 'bad',
        tip: '当前 global 账号类型不支持该模型' + (reason ? '；' + reason : '') };
    }
    if (lower.startsWith('11102') || lower.includes('service info not found')) {
      return { key: 'missing', label: '上游无此模型', cls: 'bad',
        tip: '上游目录残留，实际请求返回模型不存在' + (reason ? '；' + reason : '') };
    }
    return { key: 'limited', label: '限流/不可用', cls: 'warn',
      tip: '模型级冷却中' + (reason ? '；' + reason : '') };
  }
  const ev = recent.find(e => e && e.model === id);
  if (ev) {
    const status = Number(ev.status || 0);
    const when = ev.time ? new Date(ev.time).toLocaleString('zh-CN', { hour12: false }) : '';
    if (ev.outcome === 'success' || (status >= 200 && status < 300)) {
      return { key: 'ok', label: '可用', cls: 'ok',
        tip: '最近一次真实请求成功' + (when ? ' · ' + when : '') };
    }
    if (status === 429) {
      return { key: 'rate', label: '限流', cls: 'warn',
        tip: '最近一次请求被限流' + (when ? ' · ' + when : '') };
    }
    if (status >= 500) {
      return { key: 'upstream', label: '上游错误', cls: 'bad',
        tip: '最近一次请求上游错误 HTTP ' + status + (when ? ' · ' + when : '') };
    }
    return { key: 'failed', label: '最近失败', cls: 'bad',
      tip: '最近一次请求失败 HTTP ' + (status || '—') + (when ? ' · ' + when : '') };
  }
  return { key: 'unknown', label: '待验证', cls: 'mute',
    tip: '尚无模型锁或近期真实请求记录；目录存在不代表当前账号有权限' };
}

function mdAvailabilityTag(m) {
  const av = mdAvailabilityOf(m);
  if (!av) return '';
  return ' <span class="tag ' + av.cls + ' md-avail" title="' + esc(av.tip) + '">' + esc(av.label) + '</span>';
}

// mdRowHtml 单个模型行（纯渲染，便于独立测试）。
function mdRowHtml(m, pr) {
  const type = m.type || 'chat';
  const eff = (m.supported_efforts || []).slice();
  if (m.can_disable_thinking && eff.length && !eff.includes('off')) eff.push('off（可关）');
  const effs = eff.length ? eff.map(e => '<span class="tag warn">' + esc(e) + '</span>').join(' ')
    : '<span style="color:var(--ink-3);font-size:12.5px">' + (m.supports_reasoning ? '固定档 · 默认 ' + esc(m.default_effort || '?') : '不支持思考') + '</span>';
  // 类型与能力徽标：聊天、图片、视频和独立工具都能直接进入对应调试表单。
  const caps = [];
  const typeNames = { chat: '聊天', image: '图片', video: '视频', search: '搜索', fetch: '抓取' };
  caps.push('<span class="tag ok">' + esc(typeNames[type] || type) + '</span>');
  if (m.is_default) caps.push('<span class="tag ok">默认</span>');
  if (m.supports_tool_call) caps.push('<span class="tag warn">工具</span>');
  if (m.supports_images && type === 'chat') caps.push('<span class="tag warn">视觉</span>');
  if (m.supports_reasoning && !m.can_disable_thinking) caps.push('<span class="tag warn">思考常开</span>');
  const capHtml = '<div class="id" style="margin-top:2px">' + caps.join(' ') + '</div>';
  const tip = m.description ? ' title="' + esc(m.description) + '"' : '';
  return '<tr><td class="mark" aria-hidden="true"><i></i></td><td class="who"' + tip + '><div class="nm"><span class="mid">' + esc(m.id) + '</span>' + mdAvailabilityTag(m) + '</div><div class="id">' + esc(m.name || '') + '</div>' + capHtml + '</td>' +
    '<td class="num">' + rateCell(m) + '</td>' +
    '<td>' + (m.default_effort ? '<span class="tag ok">' + esc(m.default_effort) + '</span>' : '<span style="color:var(--ink-3)">—</span>') + '</td>' +
    '<td class="efs" style="white-space:normal">' + effs + '</td>' +
    '<td class="num">' + (m.context_length ? Math.round(m.context_length / 1000) + 'K' : '—') + '</td>' +
    outCell(m, pr) + '<td><button class="xs" data-md-debug="' + esc(m.id) + '">调试</button></td></tr>';
}

function renderModels() {
  const tb = $('mdBody');
  const list = mdSortList(mdAll.filter(m => mdMatch(m)));
  if (!list.length) {
    tb.innerHTML = '<tr><td colspan="8"><div class="empty">没有符合当前筛选条件的模型</div></td></tr>';
  } else {
    tb.innerHTML = list.map(m => mdRowHtml(m, mdProbeOf(m.id))).join('');
  }
  const filtered = list.length !== mdAll.length;
  $('mdCount').textContent = !mdAll.length ? ''
    : filtered ? '命中 ' + list.length + ' / ' + mdAll.length + ' 个模型'
    : mdAll.length + ' 个模型';
  $('mdCount').className = filtered ? 'note src-off' : 'note';
}

function resetModelFilter() {
  mdFilter = { q: '', realm: '', type: '', cap: '', effort: '', promo: '', sort: 'default' };
  $('mdQ').value = ''; $('mdRealm').value = ''; $('mdType').value = ''; $('mdCap').value = '';
  $('mdEffort').value = ''; $('mdPromo').value = ''; $('mdSort').value = 'default';
  renderModels();
}

// 筛选控件：输入框防抖 120ms（长列表逐字符重排不必每键一次），下拉即时。
let mdQTimer = null;
$('mdQ').oninput = () => {
  clearTimeout(mdQTimer);
  mdQTimer = setTimeout(() => { mdFilter.q = $('mdQ').value.trim(); renderModels(); }, 120);
};
for (const [id, key] of [['mdRealm', 'realm'], ['mdType', 'type'], ['mdCap', 'cap'], ['mdEffort', 'effort'], ['mdPromo', 'promo'], ['mdSort', 'sort']]) {
  const el = $(id);
  if (!el) continue;
  el.onchange = () => { mdFilter[key] = el.value; renderModels(); };
}
$('mdReset').onclick = resetModelFilter;
$('btnModels').onclick = loadModels;

/* ── 模型 / 工具在线调试 ─────────────────────────────────────────────
   浏览器直接请求同源正式 API，不另造一条绕过选号、计费和日志的调试管线。
   只有点击发送才调用上游；不自动重试，视频任务也只在你点击后查询。 */
function mdDebugTemplate(m) {
  const type = (m && m.type) || 'chat';
  const id = String((m && m.id) || '');
  const bare = id.replace(/^(cn|global):/, '');
  if (type === 'image') {
    const tags = Array.isArray(m.tags) ? m.tags : [];
    const editOnly = tags.includes('image-to-image') && !tags.includes('text-to-image');
    return { method: 'POST', endpoint: editOnly ? (m.edits_endpoint || '/v1/images/edits') : (m.endpoint || '/v1/images/generations'), payload: editOnly ? {
      model: id, prompt: '把参考图改成水彩插画风格', image: ['https://dummyimage.com/1024x1024/eeeeee/333333.png?text=cat'], n: 1, size: '1024x1024', response_format: 'url'
    } : {
      model: id, prompt: '一只橘猫，简洁插画', n: 1, size: '1024x1024', response_format: 'url'
    } };
  }
  if (type === 'video') return { method: 'POST', endpoint: m.endpoint || '/v1/videos/generations', payload: {
    model: bare, prompt: '一只橘猫在阳光下伸懒腰', seconds: 6, watermark: false
  } };
  if (type === 'search') return { method: 'POST', endpoint: m.endpoint || '/v1/search/messages', payload: {
    model: 'web-search', max_tokens: 256,
    tools: [{ type: 'web_search_20250305', name: 'web_search', max_uses: 3 }],
    messages: [{ role: 'user', content: [{ type: 'text', text: 'WorkBuddy' }] }]
  } };
  if (type === 'fetch') return { method: 'POST', endpoint: m.endpoint || '/v1/fetch', payload: {
    url: 'https://example.com', prompt: '提取页面标题与正文要点'
  } };
  return { method: 'POST', endpoint: (m && m.endpoint) || '/v1/chat/completions', payload: {
    model: id, messages: [{ role: 'user', content: '请只回复 OK' }], stream: false, max_tokens: 32
  } };
}
function openModelDebug(id) {
  if (mdDebugBusy) { toast('上一请求仍在进行，请等待完成后再切换调试对象', 'err'); return; }
  const model = mdAll.find(m => m && m.id === id);
  if (!model) return;
  mdDebugSeq++;
  mdDebug = mdDebugTemplate(model);
  mdVideoTaskID = '';
  $('mdDebugTitle').textContent = model.id + ' · ' + (model.name || model.type || 'chat');
  $('mdDebugEndpoint').textContent = mdDebug.method + ' ' + mdDebug.endpoint;
  $('mdDebugPayload').value = JSON.stringify(mdDebug.payload, null, 2);
  $('mdDebugResult').textContent = '';
  $('mdDebugMedia').innerHTML = '';
  $('mdDebugStatus').textContent = '尚未发送';
  $('mdDebugPoll').hidden = true;
  $('mdDebugSend').disabled = false;
  $('mdDebugPoll').disabled = false;
  $('mdDebugBox').hidden = false;
  $('mdDebugBox').scrollIntoView({ behavior: 'smooth', block: 'nearest' });
}
function closeModelDebug() {
  if (mdDebugBusy) { toast('请求仍在进行，请等待完成后再关闭', 'err'); return; }
  mdDebugSeq++;
  mdDebug = null;
  mdVideoTaskID = '';
  $('mdDebugBox').hidden = true;
  $('mdDebugSend').disabled = false;
  $('mdDebugPoll').disabled = false;
}
function mdCurlCommand(debug, payload, origin) {
  const body = JSON.stringify(payload).replace(/'/g, "'\"'\"'");
  return 'curl --request ' + debug.method + ' "' + origin + debug.endpoint + '" \\\n' +
    '  --header "Authorization: Bearer ${WB2API_KEY}" \\\n' +
    '  --header "Content-Type: application/json" \\\n' +
    "  --data-raw '" + body + "'";
}
function mdMediaHtml(d) {
  if (!d || typeof d !== 'object') return '';
  const out = [];
  for (const item of (Array.isArray(d.data) ? d.data : [])) {
    if (item && typeof item.b64_json === 'string' && item.b64_json) {
      out.push('<img alt="生成图片" style="max-width:100%;margin-top:10px" src="data:image/png;base64,' + esc(item.b64_json) + '">');
    } else if (item && /^https?:\/\//.test(String(item.url || ''))) {
      out.push('<div><a href="' + esc(item.url) + '" target="_blank" rel="noopener noreferrer">打开生成图片</a></div>');
    }
  }
  if (/^https?:\/\//.test(String(d.url || ''))) {
    out.push('<div><a href="' + esc(d.url) + '" target="_blank" rel="noopener noreferrer">打开生成视频</a></div>');
  }
  return out.join('');
}
async function sendModelDebug(poll) {
  if (mdDebugBusy || !mdDebug || (poll && !mdVideoTaskID)) return;
  let payload = null;
  if (!poll) {
    try {
      payload = JSON.parse($('mdDebugPayload').value);
      if (!payload || Array.isArray(payload) || typeof payload !== 'object') throw new Error('请求体必须是 JSON 对象');
    } catch (e) { toast('JSON 无效：' + e.message, 'err'); return; }
    if (!window.confirm('将向 ' + mdDebug.endpoint + ' 发送真实请求，可能消耗积分。确认继续？')) return;
  }
  const seq = ++mdDebugSeq;
  const endpoint = poll ? '/v1/videos/' + encodeURIComponent(mdVideoTaskID) : mdDebug.endpoint;
  const method = poll ? 'GET' : mdDebug.method;
  const headers = {};
  const key = localStorage.getItem(LS_KEY);
  if (key) headers.Authorization = 'Bearer ' + key;
  if (!poll) headers['Content-Type'] = 'application/json';
  mdDebugBusy = true;
  $('mdDebugSend').disabled = true;
  $('mdDebugPoll').disabled = true;
  $('mdDebugStatus').textContent = method + ' ' + endpoint + ' · 请求中…';
  const started = Date.now();
  try {
    const resp = await fetch(endpoint, { method, headers, body: poll ? undefined : JSON.stringify(payload) });
    const text = await resp.text();
    if (seq !== mdDebugSeq) return;
    let data = null, pretty = text;
    try { data = JSON.parse(text); pretty = JSON.stringify(data, null, 2); } catch (_) { /* SSE/纯文本原样展示 */ }
    if (pretty.length > 200000) pretty = pretty.slice(0, 200000) + '\n… 响应过长，已截断展示';
    $('mdDebugResult').textContent = pretty;
    $('mdDebugMedia').innerHTML = mdMediaHtml(data);
    const rid = resp.headers.get('x-request-id') || resp.headers.get('x-wb2a-upstream-request-id') || '';
    $('mdDebugStatus').textContent = 'HTTP ' + resp.status + ' · ' + (Date.now() - started) + ' ms' + (rid ? ' · ' + rid : '');
    if (!poll && data && data.id && mdDebug.endpoint === '/v1/videos/generations') {
      mdVideoTaskID = String(data.id);
      $('mdDebugPoll').hidden = false;
      $('mdDebugPoll').textContent = '查询视频任务 ' + mdVideoTaskID;
    }
    if (resp.status === 401) openKey();
  } catch (e) {
    if (seq !== mdDebugSeq) return;
    $('mdDebugStatus').textContent = '请求失败 · ' + (Date.now() - started) + ' ms';
    $('mdDebugResult').textContent = e.message;
  } finally {
    if (seq === mdDebugSeq) {
      mdDebugBusy = false;
      $('mdDebugSend').disabled = false;
      $('mdDebugPoll').disabled = false;
    }
  }
}
$('mdBody').addEventListener('click', ev => {
  const btn = ev.target.closest('[data-md-debug]');
  if (btn) openModelDebug(btn.dataset.mdDebug);
});
$('mdDebugSend').onclick = () => sendModelDebug(false);
$('mdDebugPoll').onclick = () => sendModelDebug(true);
$('mdDebugClose').onclick = closeModelDebug;
$('mdDebugCurl').onclick = () => {
  if (!mdDebug) return;
  try {
    const payload = JSON.parse($('mdDebugPayload').value);
    navigator.clipboard.writeText(mdCurlCommand(mdDebug, payload, location.origin))
      .then(() => toast('cURL 已复制；密钥使用 ${WB2API_KEY} 环境变量', 'ok'), e => toast('复制失败：' + e.message, 'err'));
  } catch (e) { toast('JSON 无效：' + e.message, 'err'); }
};

/* ── 日志（频道：全部/任务/对话/系统） ─────────────────────────────── */
let logCh = 'all';
$('logChips').addEventListener('click', ev => {
  const b = ev.target.closest('button[data-ch]');
  if (!b) return;
  logCh = b.dataset.ch;
  document.querySelectorAll('#logChips .chip').forEach(c => c.classList.toggle('on', c === b));
  loadLogs();
});
async function loadLogs() {
  const box = $('logBox');
  const atEnd = box.scrollTop + box.clientHeight >= box.scrollHeight - 24;
  const limit = ($('reqLimit') && $('reqLimit').value) || 100;
  // 时间范围由归档侧过滤（不是前端筛已拉取的条目）：区间落在更早的时间段时，
  // 「最近 N 条」里根本不会有那些记录，必须让服务端按时间取。
  const rq = trangeQuery('reqRange', false);
  rq.set('limit', limit);
  try {
    const [d, metrics, requestRows, credit] = await Promise.all([
      api('logs'),
      api('request_metrics').catch(() => ({})),
      api('request_logs?' + rq.toString()).catch(() => ({ entries: [] })),
      api('credit_history?limit=' + creditLimitValue()).catch(() => null),
    ]);
    // 归档开启时以归档为准——「区间内没有记录」是一个真实结果，不能回落成内存里
    // 的最近 100 条（那会把筛选条件之外、时间范围之外的请求显示出来）。
    // 只有归档关闭时才回落到内存指标，保证没有归档的部署仍能看到最近请求。
    const archiveOn = !!(metrics && metrics.archive && metrics.archive.enabled);
    const recent = archiveOn ? (requestRows.entries || []) : (metrics.recent || []);
    renderRequestMetrics(metrics, recent);
    const entries = (d.entries || []).filter(e => logCh === 'all' || e.ch === logCh);
    box.innerHTML = entries.length
      ? entries.map(e => {
        const lvl = /error|失败|错误/.test(e.text) ? ' e' : /warn|冷却|熔断/.test(e.text) ? ' w' : '';
        const t = e.ts ? new Date(e.ts).toLocaleTimeString('zh-CN', { hour12: false }) : '';
        const ch = logCh === 'all' ? '<i class="lch c-' + esc(e.ch) + '">' + ({ task: '任务', chat: '对话', sys: '系统' }[e.ch] || e.ch) + '</i>' : '';
        return '<span class="ln' + lvl + '">' + ch + esc(t + ' ' + e.text) + '</span>';
      }).join('')
      : '<span style="color:var(--ink-3)">暂无日志</span>';
    if (logPin && atEnd) box.scrollTop = box.scrollHeight;
    const counts = {};
    for (const e of (d.entries || [])) counts[e.ch] = (counts[e.ch] || 0) + 1;
    $('logNote').textContent = logCh === 'all'
      ? '任务 ' + (counts.task || 0) + ' · 对话 ' + (counts.chat || 0) + ' · 系统 ' + (counts.sys || 0)
      : (logCh === 'task' ? '任务' : logCh === 'chat' ? '对话' : '系统') + ' ' + entries.length + ' 行';
    renderCreditHistory(credit);
  } catch (e) { /* 概览已提示 */ }
}

function renderRequestMetrics(m, entries) {
  m = m || {};
  const a = m.archive || {};
  $('reqSummary').textContent =
    '已完成 ' + fmtTok(m.completed) +
    ' · 成功 ' + (m.success_rate == null ? '—' : Number(m.success_rate).toFixed(1) + '%') +
    ' · HTTP ' + (m.http_success_rate == null ? '—' : Number(m.http_success_rate).toFixed(1) + '%') +
    ' · 平均 ' + fmtMs(m.avg_duration_ms) +
    ' · 进行中 ' + String(m.in_flight || 0);
  $('reqNote').textContent = a.enabled
    ? 'JSONL 归档 ' + fmtBytes(a.bytes) + (a.dropped_writes ? ' · 丢弃 ' + a.dropped_writes + ' 条' : '') +
      (a.last_error ? ' · 错误：' + a.last_error : '')
    : '仅内存指标，JSONL 归档已关闭';

  reqEntries = entries || [];
  renderRequestTable();
}

/* ── 积分历史（每次真实查到余额与上次比对，变动即留痕）─────────────────
   纯格式化函数集中在此，便于前端测试按切片断言；变动列 +N / −N（U+2212，
   与请求记录一致），余额用千分位。 */
function creditNum(n) {
  return Number(n || 0).toLocaleString('zh-CN');
}
function creditDeltaText(delta) {
  const d = Number(delta || 0);
  if (d > 0) return '+' + creditNum(d);
  if (d < 0) return '−' + creditNum(-d);
  return '0';
}
function creditTimeText(e) {
  if (!e || !e.time) return '—';
  const d = new Date(e.time);
  // 正常链路 time 由 Go 的 time.Time 序列化，恒为 RFC3339；这里挡的是手工改文件/
  // 将来换格式等脏数据——toLocaleTimeString 遇到非法日期不抛异常，只会渲染出
  // "Invalid Date" 这种没意义的字样。
  return isNaN(d.getTime()) ? '—' : d.toLocaleTimeString('zh-CN', { hour12: false });
}
function creditAccountText(e) {
  e = e || {};
  return e.account ? String(e.account) : (e.uid || '—');
}
function creditBalanceText(e) {
  return creditNum(e && e.after);
}
function creditDescText(e) {
  return '余额 ' + creditNum(e && e.before) + ' → ' + creditNum(e && e.after);
}
function creditEntryText(e) {
  return creditTimeText(e) + ' | ' + creditAccountText(e) + ' | ' + creditDeltaText(e && e.delta) +
    ' | ' + creditBalanceText(e) + ' | ' + creditDescText(e);
}
function creditHistoryNote(entries) {
  entries = entries || [];
  if (!entries.length) return '暂无积分变动记录';
  let net = 0;
  for (const e of entries) net += Number(e && e.delta || 0);
  return entries.length + ' 条 · 净 ' + (net > 0 ? '+' : net < 0 ? '−' : '') + creditNum(Math.abs(net));
}
function renderCreditHistory(d) {
  const body = $('creditBody'), note = $('creditNote');
  if (!body) return;
  if (!d) {
    if (note) note.textContent = '积分历史不可用';
    body.innerHTML = '<tr><td colspan="5" style="color:var(--ink-3)">积分历史不可用</td></tr>';
    return;
  }
  const entries = d.entries || [];
  if (note) note.textContent = creditHistoryNote(entries);
  body.innerHTML = entries.length
    ? entries.map(e => '<tr>' +
      '<td>' + esc(creditTimeText(e)) + '</td>' +
      '<td>' + esc(creditAccountText(e)) + '</td>' +
      '<td class="num">' + esc(creditDeltaText(e && e.delta)) + '</td>' +
      '<td class="num">' + esc(creditBalanceText(e)) + '</td>' +
      '<td>' + esc(creditDescText(e)) + '</td>' +
      '</tr>').join('')
    : '<tr><td colspan="5" style="color:var(--ink-3)">暂无积分变动记录</td></tr>';
}
function creditLimitValue() {
  const el = $('creditLimit');
  const n = el ? Number(el.value) : 100;
  return n === 300 || n === 1000 ? n : 100;
}
async function loadCreditHistory() {
  let d = null;
  try { d = await api('credit_history?limit=' + creditLimitValue()); } catch (e) { d = null; }
  renderCreditHistory(d);
}

/* reqMatch 请求记录筛选：q 对 IP/UA/模型/账号/请求 ID 做空格分词的 AND 包含匹配，
   outcome 精确匹配。两者都在已拉取的条目上做（最多 1000 条），不发新请求。 */
function reqMatch(e, f) {
  f = f || reqFilter;
  if (f.outcome && String(e && e.outcome || '') !== f.outcome) return false;
  if (f.q) {
    const text = [e && e.client_ip, e && e.egress, e && e.user_agent, e && e.model, e && e.account, e && e.request_id]
      .filter(Boolean).join(' ').toLowerCase();
    for (const kw of f.q.toLowerCase().split(/\s+/).filter(Boolean)) {
      if (!text.includes(kw)) return false;
    }
  }
  return true;
}

function reqOutcomeTag(e) {
  const outcome = String(e && e.outcome || '');
  const label = { success: '成功', http_error: 'HTTP 错误', stream_error: '流错误', interrupted: '中断' }[outcome] || outcome || '—';
  const cls = outcome === 'success' ? 'ok'
    : outcome === 'interrupted' ? 'warn'
    : outcome ? 'bad' : 'mute';
  return '<span class="tag ' + cls + '">' + esc(String(e && e.status || '—') + ' ' + label) + '</span>';
}

function reqTokenCell(e) {
  const total = Number(e && e.total_tokens || 0) ||
    (Number(e && e.prompt_tokens || 0) + Number(e && e.completion_tokens || 0));
  return total ? fmtTok(total) : '—';
}

function reqCreditCell(e) {
  if (!e || !e.credit_known) return '<span class="muted">—</span>';
  const v = Number(e.credit);
  return Number.isFinite(v) ? trimFixed(v.toFixed(2)) : '<span class="muted">—</span>';
}

/* renderRequestTable 渲染请求记录表。来源列是这一版的重点：IP 用等宽字体方便扫，
   UA 单行截断（完整值在 title 里，行本身用 requestLogText 作 tooltip）。 */
function renderRequestTable() {
  const list = reqEntries.filter(e => reqMatch(e));
  const tb = $('reqBody');
  if (!tb) return;
  tb.innerHTML = list.map(e => {
    const when = e && e.time ? new Date(e.time).toLocaleTimeString('zh-CN', { hour12: false }) : '—';
    const ip = e && e.client_ip ? e.client_ip : '';
    // 出口 IP：与来源 IP 是相反方向（来源=谁调进来，出口=从哪出去）。同一列里
    // 第二行展示，避免再加一列撑宽表格 / 打乱移动端按列序写死的栅格。
    const eg = e && e.egress ? e.egress : '';
    const egCls = eg === 'direct' ? 'egress off' : 'egress';
    const ua = e && e.user_agent ? e.user_agent : '';
    const rid = e && e.request_id ? e.request_id : '';
    return '<tr title="' + esc(requestLogText(e)) + '">' +
      '<td class="num">' + esc(when) + '</td>' +
      '<td>' + reqOutcomeTag(e) + '</td>' +
      '<td>' + esc(e && e.model || '—') + '</td>' +
      '<td>' + esc(e && e.account || '—') + stickyBadge(e) + '</td>' +
      '<td>' + (ip ? '<span class="clip ip" title="' + esc(ip) + '">' + esc(ip) + '</span>' : '<span class="muted">—</span>') +
        (eg ? '<div class="' + egCls + '" title="出口 ' + esc(eg) + '">→ ' + esc(eg) + '</div>' : '') + '</td>' +
      '<td>' + (ua ? '<span class="clip" title="' + esc(ua) + '">' + esc(ua) + '</span>' : '<span class="muted">—</span>') + '</td>' +
      '<td class="num">' + fmtMs(e && e.duration_ms) + '</td>' +
      '<td class="num">' + reqTokenCell(e) + '</td>' +
      '<td class="num">' + reqCreditCell(e) + '</td>' +
      '<td>' + (rid ? '<span class="clip rid" title="' + esc(rid) + '">' + esc(rid) + '</span>' : '<span class="muted">—</span>') + '</td>' +
      '</tr>';
  }).join('') || '<tr><td colspan="10" class="empty">' +
      (reqEntries.length ? '没有符合当前筛选条件的请求记录' : '暂无请求记录') + '</td></tr>';

  const filtered = list.length !== reqEntries.length;
  // 归档里的旧条目没有来源字段（该功能上线前写入）：这时提示开关/历史原因，
  // 而不是让人以为筛选坏了。
  const hasSource = reqEntries.some(e => e && (e.client_ip || e.user_agent));
  $('reqCount').textContent = !reqEntries.length ? ''
    : (filtered ? '命中 ' + list.length + ' / ' + reqEntries.length + ' 条' : reqEntries.length + ' 条') +
      (hasSource ? '' : ' · 来源未记录');
  $('reqCount').className = (filtered || !hasSource) ? 'note src-off' : 'note';
}

/* 请求记录筛选控件。搜索框防抖 150ms：最多 1000 行重渲染，不必每键一次。
   这段顶层绑定放在 requestLogText 之前，是为了让"纯函数切片"式前端测试
   （slice requestLogText → fmtBytes）只拿到无副作用的格式化函数。 */
let reqQTimer = null;
if ($('reqQ')) $('reqQ').oninput = () => {
  clearTimeout(reqQTimer);
  reqQTimer = setTimeout(() => { reqFilter.q = $('reqQ').value.trim(); renderRequestTable(); }, 150);
};
if ($('reqOutcome')) $('reqOutcome').onchange = () => {
  reqFilter.outcome = $('reqOutcome').value;
  renderRequestTable();
};
if ($('reqLimit')) $('reqLimit').onchange = loadLogs;
if ($('btnReqReload')) $('btnReqReload').onclick = loadLogs;
// 时间范围：默认「全部历史」——请求记录页的历史行为就是"取最近 N 条"，
// 加一个默认收窄的区间会让打开页面时看到的条数凭空变少。
if ($('reqRange')) trangeBind('reqRange', loadLogs, '0');
if ($('creditLimit')) $('creditLimit').onchange = loadCreditHistory;
if ($('btnCreditReload')) $('btnCreditReload').onclick = loadCreditHistory;

/* stickyBadge 会话粘性落点的小角标（运行日志「账号」列第二行）。
   只标「非命中」的三种：换号（miss）会打掉账号级前缀缓存，是排查缓存命中最直接
   的一眼线索；同号（hit）不标，否则整表都是噪声。 */
function stickyBadge(e) {
  const st = (e && e.sticky) || '';
  const label = { busy: '满载溢出', miss: '换号', new: '新会话' }[st];
  if (!label) return '';
  return '<div class="muted sticky-badge" title="' +
    (st === 'busy' ? '绑定号在途满载：本次走了别的号，绑定保留' :
     st === 'miss' ? '绑定号持久不可用：已解绑并重新分配（上游前缀缓存会全冷）' :
     '该会话首次出现：本次分配并绑定') +
    '">' + label + '</div>';
}

function requestLogText(e) {
  const when = e && e.time ? new Date(e.time).toLocaleTimeString('zh-CN', { hour12: false }) : '—';
  const outcomeLabel = { success: '成功', http_error: 'HTTP 错误', stream_error: '流错误', interrupted: '中断' };
  const token = Number(e && e.total_tokens || 0) ||
    (Number(e && e.prompt_tokens || 0) + Number(e && e.completion_tokens || 0));
  let credit = 'credit —';
  if (e && e.credit_known) {
    const value = Number(e.credit);
    if (Number.isFinite(value)) credit = String(Number(value.toFixed(2))) + ' credit';
  }
  return [
    when,
    String(e && e.status || '—') + ' ' + (outcomeLabel[e && e.outcome] || (e && e.outcome) || '—'),
    e && e.model || '—',
    e && e.account || '—',
    e && e.client_ip || '—',
    e && e.egress ? '出口 ' + e.egress : '',
    e && e.user_agent || '—',
    fmtMs(e && e.duration_ms),
    fmtTok(token) + ' tok',
    credit,
    cacheRateText(e && e.cache_hit_tokens, e && e.cache_miss_tokens) === '—' ? '' : '命中 ' + cacheRateText(e && e.cache_hit_tokens, e && e.cache_miss_tokens),
    // 粘性落点：同号=命中缓存的前提，换号=缓存全冷的前提（面板换号率的分母来源）。
    ({ hit: '粘性 同号', busy: '粘性 满载溢出', miss: '粘性 换号', new: '粘性 新会话' })[e && e.sticky] || '',
    e && e.request_id || '—',
  ].filter(Boolean).join(' | ');
}

/* 缓存命中率纯文本（issue #92）：requestLogText 与积分表/kpi 卡共用。
   自包含（不依赖 trimFixed）：前端纯函数切片测试只截取本段。 */
function cacheRateText(hit, miss) {
  const h = Number(hit || 0), m = Number(miss || 0), total = h + m;
  if (!total) return '—';
  return String(Math.round(h / total * 1000) / 10) + '%';
}

/* cacheRateClass 命中率的语义色档：≥90% 绿 / 80–90% 黄 / <80% 红 / 无样本灰。
   阈值必须与表内 cacheCell、积分表 cacheRateCell 完全一致——同一个数字在总览
   显示绿色、在明细显示黄色，会被读成"两张表口径不同"，实际只是阈值漂了。 */
function cacheRateClass(hit, miss) {
  const h = Number(hit || 0), m = Number(miss || 0), tot = h + m;
  if (!tot) return 'c-mute';
  const pct = h / tot * 100;
  return pct >= 90 ? 'c-ok' : (pct >= 80 ? 'c-warn' : 'c-bad');
}

/* CACHE_BLOCK_TOKENS 上游前缀缓存的块大小（token 对齐粒度）。
   实测口径（2026-10-27，池内 577 条「本就有命中」的请求）：568 条命中量是 128 的
   整数倍，%256 只有 271 条 —— 缓存按 128 tok 对齐，prompt 不足一个块时没有任何可
   命中的单元，命中率必然是 0，且与路由/账号完全无关。
   面板据此区分「缓存没在工作」和「这批请求本来就没得缓存」：后者常常是探活/短问答
   打满请求数，按条数算出来的命中率会低到 20%，而 token 加权是 90%+。 */
const CACHE_BLOCK_TOKENS = 128;

/* cacheCell 用量明细表的「缓存命中」单元格：命中率（语义色）+ 命中/未命中占比条。
   分母是 token 而不是请求数 —— 见 CACHE_BLOCK_TOKENS 的注释。
   平均 prompt 低于一个缓存块时，把「物理不可命中」写进 tooltip：一排 0% 很容易被
   读成故障，实际是请求太短，此时优化路由没有意义。 */
function cacheCell(a) {
  const hit = Number(a && a.cache_hit_tokens || 0);
  const miss = Number(a && a.cache_miss_tokens || 0);
  const tot = hit + miss;
  if (!tot) return '<span class="muted">—</span>';
  const pct = hit / tot * 100;
  const color = pct >= 90 ? 'var(--ok)' : (pct >= 80 ? 'var(--warn)' : 'var(--bad)');
  const reqs = Number(a && a.requests || 0);
  const avgP = reqs ? Number(a.prompt_tokens || 0) / reqs : 0;
  let tip = '命中 ' + fmtTok(hit) + ' / 未命中 ' + fmtTok(miss) + ' tok';
  if (avgP && avgP < CACHE_BLOCK_TOKENS) {
    tip += ' · 平均 prompt 仅 ' + Math.round(avgP) + ' tok，低于 ' + CACHE_BLOCK_TOKENS +
      ' tok 缓存块，这部分请求物理上不可能命中';
  }
  return '<span style="color:' + color + '" title="' + esc(tip) + '">' +
    trimFixed(pct.toFixed(1)) + '%</span>' +
    '<span class="us-mix" title="' + esc(tip) + '">' +
    '<i class="h" style="width:' + pct.toFixed(2) + '%"></i>' +
    '<i class="m" style="width:' + (100 - pct).toFixed(2) + '%"></i></span>';
}

/* toolSkipCell 「模型跳过工具」单元格：对话已建立工具使用模式，本轮却一次都没调用。
 *
 * 为什么值得单独一列：这是判断"某个模型值不值得继续用"最直接的证据。声明了 tools、
 * 历史里明明用过工具，本轮却直接改口答文本——通常是模型答非所问或开始幻觉
 * （实测 kimi-k3.1-pro 有 1/4 轮把"北京天气"直接编成"晴 25°C"，一次工具都没调）。
 *
 * 口径（见 upstream.InspectTools）：分母 = 本次**显式声明了 tools** 的请求；
 * 分子 = 其中「历史里有工具痕迹 + 本轮 0 调用」的次数。三个条件缺一不可，
 * 首轮不调工具是模型的正常判断，绝不能计入。
 *
 * 分母为 0（该模型从没被给过工具）显示 "—"：0% 会被读成"从不跳过"，是反向误导。 */
function toolSkipCell(a) {
  const reqs = Number(a && a.tool_requests || 0);
  const skipped = Number(a && a.tool_skipped || 0);
  if (!reqs) return '<span class="muted" title="窗口内该维度没有带 tools 的请求">—</span>';
  const pct = skipped / reqs * 100;
  // 与缓存列相反：这里是**越低越好**。超过 5% 就该看是哪几轮、什么模型。
  const color = pct <= 2 ? 'var(--ok)' : (pct <= 8 ? 'var(--warn)' : 'var(--bad)');
  const tip = '声明 tools 的请求 ' + reqs + ' 次，其中 ' + skipped +
    ' 次「对话已用过工具却本轮 0 调用」(' + trimFixed(pct.toFixed(1)) + '%)';
  const txt = skipped ? trimFixed(pct.toFixed(1)) + '%' : '0%';
  return '<span style="color:' + (skipped ? color : 'var(--ok)') + '" title="' + esc(tip) + '">' +
    txt + '</span>' +
    '<span class="note">' + skipped + '/' + reqs + '</span>';
}

function fmtBytes(bytes) {
  const n = Number(bytes || 0);
  if (n < 1024) return n + ' B';
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB';
  return (n / 1024 / 1024).toFixed(1) + ' MB';
}
$('btnLogPin').onclick = () => {
  logPin = !logPin;
  $('btnLogPin').textContent = '自动滚动：' + (logPin ? '开' : '关');
};

/* ── 配置 ─────────────────────────────────────────────────────────── */
const CFG_MAP = {
  strategy_domestic: ['pool', 'strategy', 'domestic'],
  strategy_international: ['pool', 'strategy', 'international'],
  domestic_account_order: ['pool', 'account_order', 'domestic'],
  international_account_order: ['pool', 'account_order', 'international'],
  listen: ['listen'], api_key: ['api_key'],
  package_detail_limit: ['panel', 'package_detail_limit'],
  checkin_hours: ['schedule', 'checkin_hours'], checkin_enabled: ['schedule', 'checkin_enabled'], growth_hours: ['schedule', 'growth_hours'], growth_enabled: ['schedule', 'growth_enabled'],
  travel_hours: ['schedule', 'travel_hours'], travel_enabled: ['schedule', 'travel_enabled'],
  activity_hours: ['schedule', 'activity_hours'], activity_enabled: ['schedule', 'activity_enabled'],
  keepalive_hours: ['schedule', 'keepalive_hours'], keepalive_enabled: ['schedule', 'keepalive_enabled'],
  balance_refresh_enabled: ['schedule', 'balance_refresh_enabled'], balance_refresh_minutes: ['schedule', 'balance_refresh_minutes'],
  task_sweep_enabled: ['schedule', 'task_sweep_enabled'], task_sweep_minutes: ['schedule', 'task_sweep_minutes'],
  include_disabled_in_tasks: ['schedule', 'include_disabled_in_tasks'],
  new_account_grace_days: ['schedule', 'new_account_grace_days'],
  max_in_flight: ['pool', 'max_in_flight'], max_in_flight_global: ['pool', 'max_in_flight_global'],
  breaker_threshold: ['pool', 'breaker_threshold'],
  degrade_threshold: ['pool', 'degrade_threshold'], degrade_cooldown: ['pool', 'degrade_cooldown'],
  degrade_cooldown_max: ['pool', 'degrade_cooldown_max'],
  cost_explore_interval: ['pool', 'cost_explore_interval'],
  credit_floor: ['pool', 'credit_floor'],
  prefer_expiring: ['pool', 'prefer_expiring'], expiring_soon: ['pool', 'expiring_soon'],
  soft_rate: ['cooldown', 'soft_rate'], soft_rate_max: ['cooldown', 'soft_rate_max'],
  breaker_cooldown: ['pool', 'breaker_cooldown'], breaker_cooldown_max: ['pool', 'breaker_cooldown_max'],
  idle_weight_per_hour: ['pool', 'idle_weight_per_hour'], idle_weight_max: ['pool', 'idle_weight_max'],
  ttl: ['session_sticky', 'ttl'],
  timeout_seconds: ['upstream', 'timeout_seconds'], header_timeout_seconds: ['upstream', 'header_timeout_seconds'],
  idle_timeout_seconds: ['upstream', 'idle_timeout_seconds'], user_agent: ['upstream', 'user_agent'],
  prompt_mode: ['prompt', 'mode'], prompt_file: ['prompt', 'file'],
  sanitize_blacklist_fingerprints: ['features', 'sanitize_blacklist_fingerprints'],
  session_sticky_enabled: ['session_sticky', 'enabled'],
  request_client_info: ['logging', 'request_client_info'],
};
/* 「覆盖型」文本字段：空串本身是有意义的取值（= 回落到内置默认），必须照发。
 *
 * 其余文本字段保持「空 = 不下发」的既有语义——那是防误清空的保护，不是 bug：
 * 表单里某个框没填，通常意味着"没改"，把它当成"请清空"会静默抹掉配置。
 *
 * 但覆盖型字段正好相反：清空 = 明确要求回到默认。漏发它们会让面板显示"已保存"
 * 而值其实没变（issue #102 附带发现 2：user_agent 清空后 config.json 里仍是旧值）。
 *
 * 刻意不含 api_key：清空它 = 关闭整个鉴权，误触代价是网关变成无鉴权公开服务。
 * 该字段（以及提示文案"留空 = 不鉴权"与现状不符的问题）单独处理。
 */
const CLEARABLE_CFG = new Set(['user_agent', 'prompt_file']);

function dig(obj, path) { return path.reduce((o, k) => (o == null ? undefined : o[k]), obj); }
function put(obj, path, val) {
  let o = obj;
  for (let i = 0; i < path.length - 1; i++) { if (typeof o[path[i]] !== 'object' || o[path[i]] === null) o[path[i]] = {}; o = o[path[i]]; }
  o[path[path.length - 1]] = val;
}

async function loadConfig() {
  try {
    const d = await api('config');
    cfgLoaded = d.config;
    $('cfgPath').textContent = d.path || '';
    const f = $('cfgForm');
    for (const [name, path] of Object.entries(CFG_MAP)) {
      const el = f.elements[name];
      if (!el) continue;
      const v = dig(cfgLoaded, path);
      if (el.type === 'checkbox') el.checked = !!v;
      else if (Array.isArray(v)) el.value = v.join(', ');
      else el.value = v == null ? (name.startsWith('strategy_') ? 'weighted_random' : '') : v;
    }
    markDurationFields(); // 回填后重置校验态（清掉残留红框；现值来自后端必然合法）
    $('cfgNote').textContent = '';
  } catch (e) { toast('读取配置失败：' + e.message, 'err'); }
}
function collectConfig() {
  const f = $('cfgForm'), out = {};
  for (const [name, path] of Object.entries(CFG_MAP)) {
    const el = f.elements[name];
    if (!el) continue;
    let v;
    if (el.type === 'checkbox') v = el.checked;
    else if (el.type === 'number') { v = el.value.trim() === '' ? undefined : Number(el.value); }
    else {
      const raw = el.value.trim();
      // 覆盖型字段空串照发（见 CLEARABLE_CFG）；其余空 = 不下发。
      if (name.endsWith('_account_order')) v = raw.split(/[,，\s]+/).filter(Boolean);
      else if (raw === '') v = CLEARABLE_CFG.has(name) ? '' : undefined;
      else if (name.endsWith('_hours')) v = raw.split(/[,，\s]+/).filter(Boolean).map(Number);
      else v = raw;
    }
    if (v !== undefined) put(out, path, v);
  }
  return out;
}
/* Go 时长字段即时校验：空 = 沿用现值（collectConfig 跳过发送）；非空必须是
   ParseDuration 语法（30m / 2h / 600s / 1h30m，可组合可带小数）。与后端
   config.go normalize() 的 time.ParseDuration 同口径，脏值在前端就地标红，
   不再等到保存被拒。 */
const DURATION_RE = /^(\d+(\.\d+)?(ns|us|µs|ms|s|m|h))+$/;
const DURATION_FIELDS = ['soft_rate', 'soft_rate_max', 'breaker_cooldown', 'breaker_cooldown_max',
  'degrade_cooldown', 'degrade_cooldown_max', 'cost_explore_interval', 'expiring_soon', 'ttl'];
const DURATION_TIP = '格式应为 Go 时长：30m / 2h / 600s / 1h30m';
function durationBad(name) {
  const el = $('cfgForm').elements[name];
  if (!el) return false;
  const v = el.value.trim();
  return v !== '' && !DURATION_RE.test(v);
}
function markDurationFields() {
  for (const name of DURATION_FIELDS) {
    const el = $('cfgForm').elements[name];
    if (!el) continue;
    const bad = durationBad(name);
    el.classList.toggle('invalid', bad);
    el.title = bad ? DURATION_TIP : '';
  }
}
$('cfgForm').addEventListener('input', ev => {
  if (DURATION_FIELDS.includes(ev.target.name)) markDurationFields();
});
$('btnEye').onclick = () => {
  const el = $('cfgKey');
  const show = el.type === 'password';
  el.type = show ? 'text' : 'password';
  $('btnEye').textContent = show ? '隐藏' : '显示';
};
$('btnCfgReload').onclick = loadConfig;
$('cfgForm').onsubmit = async ev => {
  ev.preventDefault();
  // 时长字段脏值拦截：标红 + toast 点名，不发保存请求（后端同样会拒，这里前置）。
  markDurationFields();
  const firstBad = DURATION_FIELDS.find(durationBad);
  if (firstBad) {
    const el = $('cfgForm').elements[firstBad];
    el.focus();
    toast('「' + (el.closest('.fld')?.querySelector('.lb')?.textContent || firstBad) + '」' + DURATION_TIP, 'err');
    return;
  }
  const btn = $('btnCfgSave');
  btn.disabled = true; btn.textContent = '保存中…';
  try {
    const r = await api('config', { method: 'POST', body: JSON.stringify(collectConfig()) });
    const n = (r.restart_required || []).length;
    toast(n ? '配置已保存，其中 ' + n + ' 项需重启进程生效' : '配置已保存并立即生效', 'ok');
    // 密钥可能已改：本次会话沿用新值，避免下一次轮询被 401。
    const k = $('cfgKey').value.trim();
    if (k) localStorage.setItem(LS_KEY, k);
    loadConfig();
    loadOverview(true);
  } catch (e) { toast('保存失败：' + e.message, 'err'); }
  finally { btn.disabled = false; btn.textContent = '保存配置'; }
};

/* ── 添加账号 ─────────────────────────────────────────────────────── */
function openAdd() {
  $('addVeil').classList.add('on');
  // 重置到登录标签
  switchAddTab('login');
  $('addPick').hidden = false;
  $('addLoad').hidden = true; $('addReady').hidden = true;
  $('addDone').hidden = true; $('addErr').hidden = true;
  $('importDone').hidden = true; $('importErr').hidden = true;
  $('btnCopyUrl').hidden = true; $('btnOpenUrl').hidden = true;
  $('btnStartLogin').hidden = false; $('btnStartLogin').disabled = false;
  stopPoll();
}
function switchAddTab(tab) {
  document.querySelectorAll('#addTabs .tab').forEach(b => b.classList.toggle('on', b.dataset.tab === tab));
  $('addTabLogin').hidden = tab !== 'login';
  $('addTabImport').hidden = tab !== 'import';
}
document.querySelectorAll('#addTabs .tab').forEach(b => {
  b.onclick = () => switchAddTab(b.dataset.tab);
});
function startAddLogin() {
  const realm = (document.querySelector('input[name="addRealm"]:checked') || {}).value || 'cn';
  $('btnStartLogin').disabled = true;
  $('addLoad').hidden = false; $('addErr').hidden = true;
  api('login/start', { method: 'POST', body: JSON.stringify({ realm }) }).then(r => {
    loginState = r.state;
    $('addUrl').textContent = r.url;
    $('addPick').hidden = true; // 选域锁定（会话已按该域发起）
    $('addLoad').hidden = true; $('addReady').hidden = false;
    $('btnStartLogin').hidden = true;
    $('btnCopyUrl').hidden = false; $('btnOpenUrl').hidden = false;
    loginTimer = setInterval(pollLogin, 3000);
  }).catch(e => {
    $('addLoad').hidden = true;
    $('btnStartLogin').disabled = false;
    $('addErr').hidden = false;
    $('addErr').textContent = e.message;
  });
}
function stopPoll() { if (loginTimer) { clearInterval(loginTimer); loginTimer = null; } }
async function pollLogin() {
  if (!loginState) return;
  try {
    const r = await api('login/poll?state=' + encodeURIComponent(loginState));
    if (r.done) {
      stopPoll();
      $('addReady').hidden = true;
      $('addDone').hidden = false;
      $('addDone').textContent = '已添加 ' + (r.nickname || r.uid) + (r.realm === 'global' ? '（国际版）' : '') + (r.credits >= 0 ? ' · 积分 ' + r.credits + (r.credits_total > 0 ? '/' + r.credits_total : '') : '') + '，账号已载入池中';
      setTimeout(() => { closeAdd(); loadOverview(true); }, 1600);
    }
  } catch (e) {
    stopPoll();
    $('addReady').hidden = true;
    $('addErr').hidden = false;
    $('addErr').textContent = e.message + '（关闭后重新添加）';
  }
}
function closeAdd() { stopPoll(); loginState = null; $('addVeil').classList.remove('on'); }
$('btnCloseAdd').onclick = closeAdd;
$('btnStartLogin').onclick = startAddLogin;
$('btnOpenUrl').onclick = () => open($('addUrl').textContent, '_blank');
$('btnCopyUrl').onclick = () => navigator.clipboard.writeText($('addUrl').textContent)
  .then(() => toast('链接已复制', 'ok'), () => toast('复制失败，请手动选择复制', 'err'));
$('importFile').onchange = async () => {
  const file = $('importFile').files[0];
  if (!file) return;
  $('importDone').hidden = true; $('importErr').hidden = true;
  const fd = new FormData();
  fd.append('file', file);
  const h = {};
  const k = localStorage.getItem(LS_KEY);
  if (k) h['Authorization'] = 'Bearer ' + k;
  try {
    const r = await fetch('/panel/api/import/cockpit', { method: 'POST', body: fd, headers: h });
    const d = await r.json();
    if (!r.ok) throw new Error(d.error || ('HTTP ' + r.status));
    $('importDone').hidden = false;
    $('importDone').textContent = '导入完成：成功 ' + d.imported + ' 个' + (d.skipped ? '，跳过 ' + d.skipped + ' 个' : '');
    if (d.errors && d.errors.length) {
      console.warn('import errors:', d.errors);
    }
    loadOverview(true);
  } catch (e) {
    $('importErr').hidden = false;
    $('importErr').textContent = '导入失败：' + e.message;
  }
  $('importFile').value = '';
};

/* ── 顶部动作 ─────────────────────────────────────────────────────── */
$('btnAdd').onclick = openAdd;
$('btnRefresh').onclick = async () => {
  const b = $('btnRefresh');
  b.disabled = true; b.textContent = '刷新中…';
  try {
    await api('balance_all', { method: 'POST' });
    await loadOverview(true);
    toast('余额已从上游刷新', 'ok');
  } catch (e) { toast('刷新失败：' + e.message, 'err'); await loadOverview(true); }
  finally { b.disabled = false; b.textContent = '刷新'; }
  if (view === 'logs') loadLogs();
};

/* ── 官方补偿（issue #189）─────────────────────────────────────────
   「查状态 → 待领才领」。

   取数与呈现刻意分开：
     - 取数只有手动触发（进页面一次 + 「检查」/「领取」后）。补偿状态没有
       overview 快照可吃，必须逐号打上游，挂进 5s 轮询就是每 5 秒轰炸上游。
     - 呈现走 compCache 的本地重渲染（refreshVisible 每轮调一次），只为让
       倒计时走动 —— 不发任何请求。
   之前这里有个「5 分钟新鲜窗口」分支，但它唯一的非强制调用点是 start()，
   而那里 compCache 必为 null，所以该分支永远不会命中：注释宣称「防轰炸」，
   实际是「根本不刷新」。现已删除，别再加回来。

   声明位置在本段（轮询区之前）是必需的：start() 会调用 loadCompensation()，
   若这些 let 写在 start() 之后，顶层求值会在 TDZ 上炸——
   TestAppJSTopLevelSmoke 正是为这类问题设的闸门。 */
let compCache = null;
let compInFlight = null;   // 在途请求的 promise（非布尔标志：布尔标志配 force 刷新会静默空转）

// compPending 挑出「可领未领」的账号。
// 纯函数：判据独立于渲染，node 侧单测单独钉住。
// 判据与后端 upstream.CompensationStatus.Pending() 对齐——两端各留一份实现是
// 有意为之（前端要在渲染前先算总数），但**必须同步**，改一处就要改另一处。
function compPending(rows) {
  return (rows || []).filter(r => r && !r.skip && !r.error && r.status &&
    r.status.active && !r.status.claimed && (r.status.credit_num || 0) > 0);
}

// compErrored 挑出「查询失败」的账号。
// 单独拎出来，是因为它们**必须显形**：上游 5xx / token 过期时所有账号都会落到
// 这里，若和「没有补偿」一起被过滤掉，卡片就会整块消失，用户看到的是「什么都没有」
// ——而真相是「一个都没查成」。这正是本 PR 要消灭的静默失败。
function compErrored(rows) {
  return (rows || []).filter(r => r && r.error);
}

// compTotalPending 待领总额。
function compTotalPending(rows) {
  return compPending(rows).reduce((s, r) => s + (r.status.credit_num || 0), 0);
}

// compDeadlineText 领取截止倒计时文案。
// 上游 end_time 形如 "2026-10-22 23:59:59" 且**不带时区**，按浏览器本地时区解析
// （与后端积分到期的处理同口径）。urgent=true 表示距截止 <=48h，前端据此染色告警。
function compDeadlineText(endTime, now) {
  const raw = String(endTime == null ? '' : endTime).trim();
  if (!raw) return { text: '无截止信息', urgent: false };
  const t = new Date(raw.replace(' ', 'T')).getTime();
  if (!isFinite(t)) return { text: raw, urgent: false };
  const ms = t - (now || Date.now());
  if (ms <= 0) return { text: '已截止', urgent: true };
  const d = Math.floor(ms / 86400000);
  const h = Math.floor(ms % 86400000 / 3600000);
  const text = d > 0 ? (d + ' 天 ' + h + ' 小时') : (Math.max(1, h) + ' 小时');
  return { text: '剩 ' + text, urgent: ms <= 2 * 86400000 };
}

function renderCompensation(d) {
  const rows = (d && d.accounts) || [];
  const box = $('compBox');
  if (!box) return;
  const active = rows.filter(r => r && !r.error && r.status && r.status.active);
  const failed = compErrored(rows);
  // 「有失败就必须显形」——哪怕一条有效数据都没有。
  if (!active.length && !failed.length) { box.hidden = true; return; }
  box.hidden = false;

  const now = Date.now();
  const okRows = active.map(r => {
    const s = r.status || {};
    const done = !!s.claimed;
    const dl = compDeadlineText(s.end_time, now);
    const name = esc(r.nickname || String(r.uid).slice(0, 8));
    return '<div class="comp-row"><span>' + name + '</span>' +
      (done
        ? '<span class="tag ok">已领取</span>'
        : '<span class="tag warn">待领取</span>' +
          '<span class="tag' + (dl.urgent ? ' bad' : ' mute') + '">' + esc(dl.text) + '</span>') +
      '<span class="tag mute">' + esc(fmtTok(s.credit_num || 0)) + ' 积分</span>' +
      (done ? '' : '<button class="xs" data-comp-uid="' + esc(r.uid) + '">领取</button>') +
      '</div>';
  }).join('');
  // err 文本来自上游响应体，是外部可控字符串，必须 esc。
  const errRows = failed.map(r =>
    '<div class="comp-row"><span>' + esc(r.nickname || String(r.uid).slice(0, 8)) + '</span>' +
    '<span class="tag bad">查询失败</span>' +
    '<span class="tag mute">' + esc(String(r.error).slice(0, 120)) + '</span></div>'
  ).join('');

  $('compList').innerHTML = okRows + errRows;

  const n = compPending(rows).length;
  let note = n ? (n + ' 个账号待领 · 共 ' + fmtTok(compTotalPending(rows)) + ' 积分') : '当前无待领补偿';
  // 混合态下也要把失败数说出来，否则 note 仍读作「没事」。
  if (failed.length) note += ' · ' + failed.length + ' 个账号查询失败';
  $('compNote').textContent = note;
}

// renderCachedCompensation 从缓存重渲染（不发请求），让倒计时走动。
// 由 refreshVisible 每轮调用；无缓存（还没查过）时什么都不做。
function renderCachedCompensation() {
  if (compCache) renderCompensation(compCache);
}

async function loadCompensation() {
  // 已有在途请求就等它跑完，而不是静默返回——领取后紧接着的刷新必须能看到新状态，
  // 否则用户点了「领取」但列表纹丝不动。
  if (compInFlight) { await compInFlight; return; }
  compInFlight = (async () => {
    try {
      const d = await api('compensation');
      compCache = d;
      renderCompensation(d);
    } catch (e) {
      const box = $('compBox');
      if (box) box.hidden = false;
      if ($('compList')) $('compList').innerHTML =
        '<div class="empty">查询失败：' + esc(e.message) + '</div>';
      if ($('compNote')) $('compNote').textContent = '';
    }
  })();
  try {
    await compInFlight;
  } finally {
    compInFlight = null;   // 必须 finally：否则一次异常就让此后所有刷新永久失效
  }
}

// 领取按钮走事件委托：列表每次重渲染都整块换 innerHTML，绑在具体节点上必丢。
if ($('compList')) $('compList').addEventListener('click', async e => {
  const b = e.target.closest('[data-comp-uid]');
  if (!b) return;
  b.disabled = true;
  try {
    const r = await api('accounts/' + encodeURIComponent(b.dataset.compUid) + '/compensation', { method: 'POST' });
    const res = r.result || {};
    if (res.claimed) toast('已领取 ' + fmtTok(res.credit || 0) + ' 积分', 'ok');
    else if (res.error) toast('查询失败，未能确认是否有补偿', 'err');
    else if (res.skip) toast(res.skip_msg || '无需领取', '');
    else toast('领取失败：未知结果', 'err');
    await loadCompensation();
    loadOverview(true);
  } catch (err) {
    toast('领取失败：' + err.message, 'err');
  }
});

if ($('btnCompRefresh')) $('btnCompRefresh').onclick = () => loadCompensation();

if ($('btnCompClaimAll')) $('btnCompClaimAll').onclick = async () => {
  const b = $('btnCompClaimAll');
  b.disabled = true;
  try {
    const r = await api('compensation_all', { method: 'POST' });
    const s = r.summary || {};
    // 顺序要紧：先看有没有账号没拿到结论，再看有没有领到。
    // 只按 claimed 判定的话，「全部查询失败」会被说成「当前没有待领补偿」，
    // 把故障说成了「没活动」——比不说更糟。
    if (s.errored) {
      toast(s.errored + ' 个账号查询失败，未能确认是否有补偿', 'err');
    } else if (s.claimed) {
      toast('已领取 ' + s.claimed + ' 个账号，共 ' + fmtTok(s.credit || 0) + ' 积分', 'ok');
    } else {
      toast('当前没有待领补偿', '');
    }
    await loadCompensation();
    loadOverview(true);
  } catch (e) {
    toast('领取失败：' + e.message, 'err');
  } finally {
    b.disabled = false;
  }
};

/* ── 轮询 ─────────────────────────────────────────────────────────── */
function refreshVisible() {
  if (view === 'accounts') { loadOverview(true); renderCachedCompensation(); }
  else if (view === 'logs') loadLogs();
  else if (view === 'taskscenter') reattachQueueView();
}
function start() {
  loadOverview(true);
  loadCompensation();                       // 补偿状态：首次进页面查一次（之后靠新鲜窗口）
  if (refTimer) clearInterval(refTimer);
  refTimer = setInterval(refreshVisible, 5000);
  checkAuthGate();
}
async function checkAuthGate() {
  try { await api('overview'); }
  catch (e) { if (String(e.message).includes('密钥') || String(e.message).includes('api_key')) return; }
}
start();

/* ── 积分任务 ─────────────────────────────────────────────────────── */
let taskUID = null;

// 可自动完成的任务（与后端 autoActions 表一致）：判据为行为事件、可经网关复现。
// 其余任务需在官方客户端内交互，面板只展示指引（行 title 提示）。
// 注意：键含点号（Model_chat_GLM5.2）必须加引号，否则会被解析成属性访问 + 数字字面量。
const AUTO_TASKS = {
  'chat_5': '上报 5 条对话活跃事件（自动补足差额）',
  'first_buddy': '上报解锁 → 同意协议 → 领取第一只 Buddy',
  'Model_chat_GLM5.2': '接受任务 → glm-5.2 真实对话一次 → 对齐模型上报',
  'RichMeow_Chat': '桌面指纹事件链上报（已验证可点亮）',
  'Buddy_App': '上报「进入 Buddy 应用」事件链（已验证可点亮）',
  'Buddy_App_QQ': '上报「进入企鹅教师助手」事件链（已验证可点亮）',
  'automation_1': '上报「定时任务创建」事件（已验证可点亮）',
  'Library_read': '上报「读资料库介绍」事件（已验证可点亮）',
  'template_5': '上报「使用模板创建任务」事件组 ×5（三账号实测点亮）',
  'playbook_prompt': '上报「灵感案例做同款发送 Prompt」事件组（三账号实测点亮）',
  'create_canvas': '上报「设计创意画布创建」事件组（三账号实测点亮，+300 分）',
  'expert_5': '真实专家召唤+使用链 ×5（专家市场+真实 chat，三账号实测点亮）',
  'Expert_team_use_3': '真实专家团召唤+使用链 ×3（三账号实测点亮）',
  'Hp_Appearance': '设置主题 API + 皮肤生效事件（两账号实测点亮）',
  'black_cat': '夜猫子：23:00–08:00 窗口内 glm-5.2 对话补足（窗口外提示等 23 点排程）',
  'Expert_lighthouse': '真实轻量云专家召唤+使用链（真实对话 requestId，两账号实测点亮）',
  'skill_1': '真实对话 + skill_info 技能加载事件（实测点亮）',
  'school_season': '校园日（小程序口径）：accept → mini 对话+activityId 上报 → 领奖（+100c+5e）',
  'Sequential_Tasks_1': '小程序首对话（小程序口径）：accept → mini 对话上报 → 领奖（+100c+5e）',
  'Sequential_Tasks_2': '小程序选专家对话（小程序口径）：市场专家 id → accept → expert_actual_use 上报 → 领奖（+200c+5e）',
  'Sequential_Tasks_3': '小程序五次对话（小程序口径）：accept → mini 对话上报 ×5（自动补差额）→ 领奖（+300c+5e）',
  'Sequential_Tasks_4': '小程序定时任务（预留，每日零点解锁一环）：accept → 定时任务创建事件 → 领奖（判据待解锁验证）',
  'Sequential_Tasks_5': '小程序使用 GLM5.2（预留）：accept → 带模型字段的 mini 对话上报 → 领奖（判据待解锁验证）',
  'Sequential_Tasks_6': '小程序十次对话（预留）：accept → mini 对话上报 ×target（自动补差额）→ 领奖',
  'Sequential_Tasks_7': '体验灵感功能（PC 限定）：accept → 桌面灵感事件 → 回读确认 → 领取 +500 分 +5 能；零积分账号由周期清扫单独补跑'
};

function openTasks(uid) {
  taskUID = uid;
  $('taskWho').textContent = uid.slice(0, 16);
  $('taskVeil').classList.add('on');
  $('btnTaskReload').hidden = false;
  loadTasks();
}
function closeTasks() { $('taskVeil').classList.remove('on'); taskUID = null; }
$('btnCloseTask').onclick = closeTasks;
$('btnTaskReload').onclick = loadTasks;

// 全部接受：把该账号未接受的任务一次性报名（幂等，跳过已接受/已领取）。
$('btnTaskAcceptAll').onclick = async () => {
  if (!taskUID) return;
  const btn = $('btnTaskAcceptAll');
  btn.disabled = true; btn.textContent = '接受中…';
  try {
    const r = await api('accounts/' + encodeURIComponent(taskUID) + '/tasks/accept_all', { method: 'POST' });
    const n = r.accepted || 0;
    if (r.failed && r.failed.length) {
      toast(`已接受 ${n} 个，${r.failed.length} 个被上游拒绝（可重试）`, 'err');
    } else {
      toast(n ? `已接受 ${n} 个任务` : (r.message || '所有任务均已接受'), 'ok');
    }
  } catch (e) { toast(e.message, 'err'); }
  finally { btn.disabled = false; btn.textContent = '全部接受'; loadTasks(); }
};

// 一键完成全部可自动任务（耗时较长：含真实对话，逐项回读验证）。
$('btnTaskAutoAll').onclick = async () => {
  if (!taskUID) return;
  const btn = $('btnTaskAutoAll');
  if (!confirm('将依次执行：补报对话事件、领取 Buddy、glm-5.2 对话、尝试上报。\n过程约 1-2 分钟（含真实对话），确认继续？')) return;
  btn.disabled = true; btn.textContent = '执行中…';
  try {
    const allUrl = 'accounts/' + encodeURIComponent(taskUID) + '/tasks/auto_all';
    let r;
    try {
      r = await api(allUrl, { method: 'POST' });
    } catch (e) {
      r = await retryForced(allUrl, { method: 'POST' }, e, '一键完成');
      if (!r) throw e;
    }
    const okN = (r.results || []).filter(x => x.status === 'done').length;
    const skipN = (r.results || []).filter(x => x.status === 'skipped').length;
    const errN = (r.results || []).filter(x => x.status === 'error').length;
    toast(`执行完成：成功 ${okN} 项，跳过 ${skipN} 项${errN ? '，失败 ' + errN + ' 项' : ''}`, errN ? 'err' : 'ok');
    console.log('auto_all results:', r.results);
  } catch (e) { toast(e.message, 'err'); }
  finally { btn.disabled = false; btn.textContent = '一键完成可自动任务'; loadTasks(); }
};

async function loadTasks() {
  if (!taskUID) return;
  const st = $('taskState'), tb = $('taskTable');
  st.hidden = false;
  st.className = 'state';
  st.innerHTML = '<span class="dots">查询中</span>';
  tb.hidden = true;
  try {
    const d = await api('accounts/' + encodeURIComponent(taskUID) + '/tasks');
    const list = d.tasks || [];
    if (!list.length) {
      st.className = 'state';
      st.textContent = '该账号暂无任务';
      return;
    }
    // 有进度或可领取的排前面，已领取沉底——一眼看到"现在该做什么"。
    list.sort((a, b) => (a.claimed - b.claimed) || (b.claimable - a.claimable) || String(a.task_code).localeCompare(String(b.task_code)));
    $('taskBody').innerHTML = list.map(t => {
      // 进度：current 可能缺失（0 或被上游省略）——用 ?? 兜底，避免渲染成 "undefined / N"
      const cur = t.current ?? 0, tgt = t.target ?? 0;
      const prog = tgt ? cur + ' / ' + tgt : (tgt === 0 && cur > 0 ? String(cur) : '—');
      const parts = [];
      if (t.credit) parts.push('+' + t.credit + ' 分');
      if (t.energy) parts.push('+' + t.energy + ' 能');
      if (t.reward_buddy) parts.push('Buddy');
      const reward = parts.length ? parts.join(' ') : '—';
      const badge = t.claimed ? '<span class="tag ok">已领取</span>'
        : t.claimable ? '<span class="tag warn">可领取</span>'
        : t.locked ? '<span class="tag mute">未解锁</span>'
        : t.accept_status === 'accepted' ? '<span class="tag mute">进行中</span>'
        : '<span class="tag mute">未接受</span>';
      const acted = t.claimed || t.locked ? ''
        : t.claimable ? '<button class="xs primary" data-t="claim" data-c="' + esc(t.task_code) + '">领取</button>'
        : AUTO_TASKS[t.task_code] ? '<button class="xs primary" data-t="auto" data-c="' + esc(t.task_code) + '" title="' + esc(AUTO_TASKS[t.task_code]) + '">一键完成</button>'
        : t.accept_status === 'accepted' ? ''
        : '<button class="xs" data-t="accept" data-c="' + esc(t.task_code) + '">接受</button>';
      // 操作指引（description/task_desc）挂 title 提示：如何完成交给用户看
      const tip = [t.title, t.task_desc || t.description, t.jump_url ? '跳转：' + t.jump_url : ''].filter(Boolean).join('\n');
      return '<tr title="' + esc(tip) + '"><td class="mark" aria-hidden="true"><i></i></td>' +
        '<td class="who"><div class="nm">' + esc(t.title || t.task_code) + '</div><div class="id">' + esc(t.task_code) + (t.tag ? ' · ' + esc(t.tag) : '') + '</div></td>' +
        '<td class="num">' + esc(prog) + '</td>' +
        '<td class="num">' + esc(reward) + '</td>' +
        '<td>' + badge + '</td>' +
        '<td class="acts">' + acted + '</td></tr>';
    }).join('');
    st.hidden = true;
    tb.hidden = false;
  } catch (e) {
    st.className = 'state err';
    st.textContent = e.message;
  }
}

$('taskBody').addEventListener('click', async ev => {
  const b = ev.target.closest('button[data-t]');
  if (!b || !taskUID) return;
  const kind = b.dataset.t, code = b.dataset.c;
  b.disabled = true;
  try {
    if (kind === 'auto') {
      // 一键完成：后端执行动作 → 回读进度 → 汇报（耗时可到分钟级，含真实对话）
      b.textContent = '执行中…';
      const oneUrl = 'accounts/' + encodeURIComponent(taskUID) + '/tasks/auto';
      const oneOpts = { method: 'POST', body: JSON.stringify({ task_code: code }) };
      let r;
      try {
        r = await api(oneUrl, oneOpts);
      } catch (e) {
        r = await retryForced(oneUrl, oneOpts, e, code);
        if (!r) throw e;
      }
      if (r.skipped) {
        toast(r.message || '已跳过', 'ok');
      } else {
        const advanced = r.progress_before !== r.progress_after;
        let msg = r.message || '已执行';
        if (r.progress_after) msg += `（进度 ${r.progress_before} → ${r.progress_after}）`;
        if (r.claimed) msg += '，奖励已自动到账';
        else if (r.claimable) msg += r.claim_error ? '，可点「领取」重试' : '';
        else if (r.attempt && !advanced) msg += '；进度未动，该任务可能需要官方客户端';
        toast(msg, (r.claimed || advanced) ? 'ok' : 'err');
      }
      loadOverview(true);
    } else {
      const path = 'accounts/' + encodeURIComponent(taskUID) + '/tasks/' + (kind === 'claim' ? 'claim' : 'accept');
      const body = kind === 'claim' ? { task_code: code } : { task_codes: [code] };
      await api(path, { method: 'POST', body: JSON.stringify(body) });
      toast(kind === 'claim' ? '已领取奖励' : '已接受任务', 'ok');
      if (kind === 'claim') loadOverview(true);
    }
  } catch (e) { toast(e.message, 'err'); }
  finally { loadTasks(); }
});

/* ── 任务中心：开学季 + 全账号扫描/队列 ──────────────────────────── */
// 开学季任务单元：✓ 已领（绿）｜◐ x/y 进行中（琥珀）｜○ 未做（灰）
function staskHTML(t) {
  if (!t) return '<span class="stask todo"><span class="mark">·</span>—</span>';
  if (t.status === 'claimed') return '<span class="stask ok"><span class="mark">✓</span>已领</span>';
  if (t.status === 'completed') return '<span class="stask warn"><span class="mark">◆</span>可领</span>';
  if (t.status === 'in_progress') {
    const fr = t.target_count ? '<span class="fr">' + t.progress + '/' + t.target_count + '</span>' : '';
    return '<span class="stask warn"><span class="mark">◐</span>' + fr + '</span>';
  }
  return '<span class="stask todo"><span class="mark">○</span>未做</span>';
}
const LUCK_SVG = '<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.4"><path d="M3.2 5.2 5 1.8l3 2.4 3-2.4 1.8 3.4-1.4 2.6 1.4 2.6-3.4 2.2H6l-3.4-2.2 1.4-2.6z" opacity=".9"/><circle cx="8" cy="9" r="1.1" fill="currentColor" stroke="none"/></svg>';
const SCHOOL_TITLES = {
  share_invite: '分享活动 +100c', desktop_chat_1_time: '桌面端体验 +100c（单次）',
  chat_3_times: '和 AI 对话 3 次 +50c', expert_use: '召唤开学季专家 +50c',
  task_student_verify: '学生认证 +100c（需真实认证，不做）',
};

/* ── 精简 QR 编码器（券码二维码用）────────────────────────────────────
   规格子集：byte 模式、ECC L、版本 1-5（全部单纠错块，免块交织）、固定掩码 0。
   完整性：规范允许任选掩码（解码器按格式信息位自行去掩码），固定掩码不影响
   可扫描性；已用 python qrcode 库对多输入多版本做逐像素交叉验证（强制 byte
   模式 + mask 0，5/5 全部 diff=0）。面板 CSP 只允许 self，外链 QR 服务不可用。 */
// qr_gen.js —— 精简 QR 编码器（浏览器用 + node 可跑交叉验证）
// 规格子集：byte 模式、ECC L、版本 1-5（全部单纠错块，免块交织）、固定掩码 0。
// 完整性说明：规范允许编码器任选掩码（解码器按格式信息位自行去掩码），
// 固定掩码不影响可扫描性；券码为短文本，v1-5（26 字节起）绰绰有余。

// GF(256) 对数/指数表（本原多项式 0x11d）
const QR_EXP = new Array(512), QR_LOG = new Array(256);
(() => {
  let x = 1;
  for (let i = 0; i < 255; i++) { QR_EXP[i] = x; QR_LOG[x] = i; x <<= 1; if (x & 0x100) x ^= 0x11d; }
  for (let i = 255; i < 512; i++) QR_EXP[i] = QR_EXP[i - 255];
})();
const gmul = (a, b) => (a && b) ? QR_EXP[QR_LOG[a] + QR_LOG[b]] : 0;

// 各版本参数（下标 = 版本-1）：[数据码字数, 纠错码字数]，ECC L 单块
const QR_V = [[19, 7], [34, 10], [55, 15], [80, 20], [108, 26]];
// 对齐图案中心坐标（v2+；与定位图案重叠的位置在放置时跳过）
const QR_ALIGN = [[], [6, 18], [6, 22], [6, 26], [6, 30]];
const QR_MASK = (r, c) => (r + c) % 2 === 0; // 掩码模式 0

// 生成多项式（最高次系数在前，g[0] 恒为 1）
function qrGenPoly(deg) {
  let g = [1];
  for (let i = 0; i < deg; i++) {
    const a = QR_EXP[i], ng = new Array(g.length + 1).fill(0);
    ng[0] = g[0];
    for (let j = 1; j < g.length; j++) ng[j] = g[j] ^ gmul(a, g[j - 1]);
    ng[g.length] = gmul(a, g[g.length - 1]);
    g = ng;
  }
  return g;
}

// Reed-Solomon 求余（综合除法），返回 deg 个纠错码字
function rsRem(data, deg) {
  const g = qrGenPoly(deg);
  const res = data.concat(new Array(deg).fill(0));
  for (let i = 0; i < data.length; i++) {
    const f = res[i];
    if (f) for (let j = 0; j < g.length; j++) res[i + j] ^= gmul(g[j], f);
  }
  return res.slice(data.length);
}

// 文本 → 码字流（byte 模式：0100 + 8 位计数 + 数据 + 终止符 + 0xEC/0x11 填充）
function qrDataCodewords(text, dataCap) {
  const bytes = Array.from(new TextEncoder().encode(text));
  const bits = [];
  const push = (val, n) => { for (let i = n - 1; i >= 0; i--) bits.push((val >> i) & 1); };
  push(4, 4);            // byte 模式
  push(bytes.length, 8); // v1-9 计数 8 位
  for (const b of bytes) push(b, 8);
  const cap = dataCap * 8;
  push(0, Math.min(4, cap - bits.length));   // 终止符
  while (bits.length % 8) bits.push(0);
  const out = [];
  for (let i = 0; i < bits.length; i += 8) {
    let v = 0; for (const b of bits.slice(i, i + 8)) v = (v << 1) | b;
    out.push(v);
  }
  for (let p = 0; out.length < dataCap; p ^= 1) out.push(p ? 0x11 : 0xEC);
  return out;
}

// 主入口：text → 布尔矩阵（true=深色模块）
function qrMatrix(text) {
  const bytes = Array.from(new TextEncoder().encode(text));
  // 版本选择：需求 ≈ 2 码字头 + 文本长度，取首个放得下的版本
  let ver = 0;
  for (let v = 0; v < QR_V.length; v++) { if (bytes.length + 2 <= QR_V[v][0]) { ver = v + 1; break; } }
  if (!ver) throw new Error('QR: text too long (>' + QR_V[4][0] + ' bytes)');
  const [dataCap, ecCap] = QR_V[ver - 1];
  const n = 17 + 4 * ver;

  const M = Array.from({ length: n }, () => new Array(n).fill(false));
  const F = Array.from({ length: n }, () => new Array(n).fill(false)); // 功能模块占位

  const setF = (r, c, v) => { M[r][c] = v; F[r][c] = true; };
  // 定位图案 + 分隔带
  const finder = (r0, c0) => {
    for (let r = -1; r <= 7; r++) for (let c = -1; c <= 7; c++) {
      const rr = r0 + r, cc = c0 + c;
      if (rr < 0 || cc < 0 || rr >= n || cc >= n) continue;
      const dark = r >= 0 && r <= 6 && c >= 0 && c <= 6 && (r === 0 || r === 6 || c === 0 || c === 6 || (r >= 2 && r <= 4 && c >= 2 && c <= 4));
      setF(rr, cc, dark);
    }
  };
  finder(0, 0); finder(0, n - 7); finder(n - 7, 0);
  // 校正图形（仅贯穿两定位图案之间：8..n-9，不得覆盖定位图案本体）
  for (let r = 8; r <= n - 9; r++) setF(r, 6, r % 2 === 0);
  for (let c = 8; c <= n - 9; c++) setF(6, c, c % 2 === 0);
  // 对齐图案（v2+，跳过与定位重叠处）
  const align = QR_ALIGN[ver - 1] || [];
  for (const ar of align) for (const ac of align) {
    if (F[ar][ac]) continue;
    for (let r = -2; r <= 2; r++) for (let c = -2; c <= 2; c++)
      setF(ar + r, ac + c, Math.max(Math.abs(r), Math.abs(c)) !== 1);
  }
  // 暗模块 + 格式信息（ECC L=01，掩码 0）——BCH(15,5) + 0x5412 异或。
  // 位序遵循规范（与 python qrcode 逐位对齐验证）：bit i 从 LSB 起数，
  // 副本一走左上角 L 形、副本二走右下 L 形。
  let fmt = (1 << 3) | 0; // L<<3 | mask
  let rem = fmt << 10;
  for (let i = 14; i >= 10; i--) if ((rem >> i) & 1) rem ^= 0x537 << (i - 10);
  fmt = ((fmt << 10) | rem) ^ 0x5412; // 15 位
  const fb = i => (fmt >> i) & 1;
  // 副本一（左上）：位 0..5 → (i,8)；6 → (7,8)；7 → (8,8)
  for (let i = 0; i <= 5; i++) setF(i, 8, !!fb(i));
  setF(7, 8, !!fb(6)); setF(8, 8, !!fb(7));
  // 副本一续 + 副本二（右下）：位 8..14 → (n-15+i, 8)；位 0..7 → (8, n-1-i)；8 → (8,7)；9..14 → (8,14-i)
  for (let i = 8; i <= 14; i++) setF(n - 15 + i, 8, !!fb(i));
  for (let i = 0; i <= 7; i++) setF(8, n - 1 - i, !!fb(i));
  setF(8, 7, !!fb(8));
  for (let i = 9; i <= 14; i++) setF(8, 14 - i, !!fb(i));
  // 暗模块（恒为深色，位于副本一垂直段末端）
  setF(n - 8, 8, true);


  // 数据码字 + 纠错码字 → 位流
  const dcw = qrDataCodewords(text, dataCap);
  const cw = dcw.concat(rsRem(dcw, ecCap));
  const bits = [];
  for (const b of cw) for (let i = 7; i >= 0; i--) bits.push((b >> i) & 1);

  // 蛇形放置（成对列，从右向左，跳过第 6 列），写数据时直接异或掩码
  let bi = 0, up = true;
  for (let x = n - 1; x > 0; x -= 2) {
    if (x === 6) x--;
    for (let i = 0; i < n; i++) {
      const r = up ? n - 1 - i : i;
      for (const c of [x, x - 1]) {
        if (F[r][c]) continue;
        const bit = bi < bits.length ? bits[bi++] : 0;
        M[r][c] = bit ? !QR_MASK(r, c) : QR_MASK(r, c);
      }
    }
    up = !up;
  }
  return M;
}

// 矩阵 → SVG（quiet zone 4 模块）
function qrSVG(M, px) {
  const n = M.length, q = 4, total = n + q * 2;
  let s = '<svg viewBox="0 0 ' + total + ' ' + total + '" width="' + px + '" height="' + px + '" shape-rendering="crispEdges" role="img" style="background:#fff">';
  for (let r = 0; r < n; r++) for (let c = 0; c < n; c++)
    if (M[r][c]) s += '<rect x="' + (c + q) + '" y="' + (r + q) + '" width="1" height="1"/>';
  return s + '</svg>';
}

/* ── 开学季券码查询（弹窗，仿活动页 #/prizes?tab=vouchers）──────────── */
/* copyText：clipboard API 只在 secure context（https/localhost）可用，
   远程 http 面板会拿不到 navigator.clipboard → 降级 execCommand。 */
function copyText(text) {
  if (navigator.clipboard && window.isSecureContext) return navigator.clipboard.writeText(text);
  return new Promise((resolve, reject) => {
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.style.cssText = 'position:fixed;opacity:0';
    document.body.appendChild(ta);
    ta.select();
    try { document.execCommand('copy') ? resolve() : reject(new Error('copy failed')); }
    catch (e) { reject(e); }
    finally { ta.remove(); }
  });
}

function vcCard(v) {
  const expired = v.valid_to && new Date(v.valid_to) < new Date();
  return '<div class="vc' + (expired ? ' expired' : '') + '">' +
    '<div class="hd"><span class="nm">' + esc(v.prize_name || v.sku_code || '券') + '</span>' +
    (expired ? '<span class="tag bad">已过期</span>' : '<span class="tag ok">可使用</span>') + '</div>' +
    '<div class="meta">' +
      (v.valid_to ? '有效期至 ' + esc(v.valid_to) : '长期有效') +
      (v.granted_at ? ' · ' + esc(v.granted_at.slice(0, 10)) + ' 抽中' : '') +
    '</div>' +
    '<div class="sep"></div>' +
    '<div class="ft"><span class="lab">券码</span><code>' + esc(v.code || '-') + '</code>' +
    '<span class="acts">' +
      (v.code ? '<button class="xs ghost" data-qr="' + esc(v.code) + '">二维码</button>' : '') +
      '<button class="xs ghost" data-copy="' + esc(v.code || '') + '">复制</button>' +
    '</span></div>' +
    '</div>';
}

async function loadSchoolVouchers() {
  const body = $('vcBody');
  $('vcVeil').classList.add('on');
  body.innerHTML = '<div class="state"><span class="dots">查询中</span></div>';
  $('vcNote').textContent = '';
  try {
    const d = await api('school/vouchers');
    const arr = d.accounts || [];
    const ok = arr.filter(a => !a.error);
    const total = ok.reduce((n, a) => n + (a.vouchers || []).length, 0);
    body.innerHTML = ok.filter(a => (a.vouchers || []).length).map(a =>
      '<div class="vc-acct"><span class="nm">' + esc(a.nickname || a.uid) + '</span>' +
      '<span>' + a.vouchers.length + ' 张</span></div>' +
      a.vouchers.map(vcCard).join('')
    ).join('') || '<div class="empty"><div class="big">🎟️</div>还没有抽到券</div>';
    $('vcNote').textContent = total ? total + ' 张券 · ' + ok.filter(a => !(a.vouchers || []).length).length + ' 个账号未抽中' : '';
    const errs = arr.filter(a => a.error);
    if (errs.length) {
      body.insertAdjacentHTML('beforeend', '<div class="note" style="color:var(--warn);margin-top:8px">查询失败：' +
        errs.map(a => esc(a.nickname || a.uid.slice(0, 8)) + '（' + esc(a.error) + '）').join('、') + '</div>');
    }
    body.querySelectorAll('button[data-copy]').forEach(b => b.onclick = async () => {
      try { await copyText(b.dataset.copy); toast('券码已复制', 'ok'); }
      catch (e) { toast('复制失败，请手动选择券码', 'err'); }
    });
    // 二维码：券码本体编码为 QR（到店出示扫描），点击切换显示/隐藏
    body.querySelectorAll('button[data-qr]').forEach(b => b.onclick = () => {
      const card = b.closest('.vc');
      const old = card.querySelector('.vc-qr');
      if (old) { old.remove(); return; }
      const box = document.createElement('div');
      box.className = 'vc-qr';
      try { box.innerHTML = qrSVG(qrMatrix(b.dataset.qr), 148); }
      catch (e) { box.innerHTML = '<span class="note">二维码生成失败：' + esc(e.message) + '</span>'; }
      card.appendChild(box);
    });
  } catch (e) {
    body.innerHTML = '<div class="state err">' + esc(e.message) + '</div>';
  }
}
$('btnSchoolVouchers').onclick = loadSchoolVouchers;
$('btnVcClose').onclick = () => $('vcVeil').classList.remove('on');
$('btnVcRefresh').onclick = loadSchoolVouchers;

/* 成长任务队列。lastQueueSeq 记录本页启动过的队列代次：执行结束后的残留 items
   （running=false 但 seq 停在旧值）不再回写视图——否则扫描结果 3 秒后被上一轮
   队列状态覆盖。 */
let queueTimer = null, lastQueueSeq = 0;
const GROWTH_TITLES = {}; // code → 展示名（扫描时从任务列表带出）
$('btnScanAll').onclick = async () => {
  const b = $('btnScanAll');
  // 停掉队列轮询：显式扫描 = 切到待办视图。否则在途队列的下一 tick 会把扫描
  // 结果冲掉重渲染回队列视图（服务端执行不受影响，只是不再实时回写本视图）。
  if (queueTimer) { clearInterval(queueTimer); queueTimer = null; }
  b.disabled = true; b.textContent = '扫描中…';
  try {
    const d = await api('tasks/scan_all', { method: 'POST' });
    renderQueue(groupItems(d), null, '没有待办任务 🎉', '全部账号的成长任务与开学季活动都已完成，明日再来。');
    // 新任务：清扫 / 扫描会把它记进任务目录并自动接受，这里只做提示。
    if (d.new_count) toast('发现 ' + d.new_count + ' 个新任务（已自动接受，可自动的会立即执行）', 'ok');
    else if (d.pending_count) {
      const manual = d.pending_count - (d.auto_count || 0);
      if (manual > 0) toast(manual + ' 项任务暂无自动动作（标「未适配」），需在客户端完成或补登记', 'warn');
    }
  } catch (e) { toast(e.message, 'err'); }
  finally { b.disabled = false; b.textContent = '扫描待办'; }
};
$('btnRunQueue').onclick = async () => {
  const conc = Number($('qcConc').value) || 1;
  if (!confirm('扫描全部账号待办并排队执行（账号并发 ' + conc + '，账号内串行）。\n含真实对话的任务耗时较长，确认继续？')) return;
  const b = $('btnRunQueue');
  b.disabled = true; b.textContent = '启动中…';
  try {
    const r = await api('tasks/run_queue', { method: 'POST', body: JSON.stringify({ concurrency: conc }) });
    if (!r.started) { toast(r.message || '没有待办任务', 'ok'); return; }
    lastQueueSeq = r.seq || 0;
    toast('队列已启动：' + r.total + ' 项（并发 ' + conc + '）', 'ok');
    startQueuePolling();
  } catch (e) { toast(e.message, 'err'); }
  finally { b.disabled = false; b.textContent = '执行全部待办'; }
};
// 扫描结果 → 分组条目（无执行状态）
function groupItems(d) {
  const groups = [];
  for (const a of (d.accounts || [])) {
    const rows = [];
    for (const t of (a.growth || [])) {
      GROWTH_TITLES[t.task_code] = t.title || t.task_code;
      // auto=false：任务还没有自动动作（新增任务待适配）；new=true：任务目录里首次出现。
      rows.push({ kind: 'growth', code: t.task_code, prog: t.target ? t.current + '/' + t.target : '—', status: 'scan', auto: t.auto, isNew: !!t.new });
    }
    if (rows.length) groups.push({ uid: a.uid, nick: a.nickname, rows });
  }
  return groups;
}
const ST_WORDS = { done: '完成', running: '执行中', error: '失败', skipped: '跳过', pending: '排队', scan: '待执行' };
function qrowHTML(it) {
  const isSchool = it.kind === 'school';
  const title = isSchool ? '开学季闭环' : (GROWTH_TITLES[it.code] || it.code);
  const dotCls = it.status === 'scan' ? 'wait' : it.status === 'running' ? 'run' : it.status === 'error' ? 'err' : it.status === 'skipped' ? 'skip' : it.status === 'done' ? 'done' : 'wait';
  const stWord = it.status === 'scan' ? '待执行' : (ST_WORDS[it.status] || it.status);
  const tags = (it.isNew ? '<span class="tag warn">新</span>' : '') +
    (it.auto === false ? '<span class="tag mute">未适配</span>' : '');
  return '<div class="qrow" title="' + esc(it.message || '') + '">' +
    '<span class="code">' + esc(it.code) + '</span>' +
    '<span class="name"><span class="t">' + esc(title) + '</span>' + (isSchool ? '<span class="tag mute">开学季</span>' : '') + tags + '</span>' +
    '<span class="prog">' + esc(it.prog || '') + '</span>' +
    '<span class="st"><span class="qdot ' + dotCls + '"></span>' + stWord + '</span>' +
    '<span class="msg">' + esc(it.message || '') + '</span>' +
    '</div>';
}
function renderQueue(groups, progress, emptyTitle, emptyDesc) {
  const empty = $('tcEmpty'), list = $('qcList');
  if (!groups.length) {
    empty.style.display = '';
    if (emptyTitle) empty.querySelector('.t').textContent = emptyTitle;
    if (emptyDesc) empty.querySelector('.d').textContent = emptyDesc;
    list.innerHTML = '';
    $('qProg').hidden = true; $('qcSummary').textContent = '';
    return;
  }
  empty.style.display = 'none';
  empty.style.display = 'none';
  let total = 0;
  list.innerHTML = groups.map(g => {
    total += g.rows.length;
    return '<div class="qgroup"><header><span class="nm">' + esc(g.nick || g.uid.slice(0, 12)) + '</span>' +
      '<span class="cnt">' + g.rows.length + ' 项待办</span></header>' +
      g.rows.map(qrowHTML).join('') + '</div>';
  }).join('');
  $('qcSummary').textContent = total + ' 项';
  updateProgress(progress);
}
function updateProgress(q) {
  if (!q || !q.items) { $('qProg').hidden = true; return; }
  const total = q.items.length;
  const done = q.items.filter(it => it.status === 'done' || it.status === 'error' || it.status === 'skipped').length;
  $('qProg').hidden = false;
  $('qBarFill').style.width = (total ? Math.round(done / total * 100) : 0) + '%';
  $('qProgText').textContent = (q.running ? '执行中 ' : '已结束 ') + done + ' / ' + total;
}
// 队列状态 → 分组（执行时轮询）
function groupsFromQueue(items) {
  const by = new Map();
  for (const it of items) {
    if (!by.has(it.uid)) by.set(it.uid, { uid: it.uid, nick: it.nickname, rows: [] });
    by.get(it.uid).rows.push({
      kind: it.kind, code: it.code,
      prog: it.kind === 'school' ? '—' : '',
      status: it.status, message: it.message,
    });
  }
  return Array.from(by.values());
}
function startQueuePolling() {
  if (queueTimer) clearInterval(queueTimer);
  queueTimer = setInterval(async () => {
    let q;
    try { q = await api('tasks/queue'); } catch (e) { return; }
    if (!q.started) return;
    // 只渲染本页启动过的那轮队列（刷新页面后不再接管旧队列）。
    if (lastQueueSeq && q.seq !== lastQueueSeq) return;
    if (q.running) {
      renderQueue(groupsFromQueue(q.items || []), q);
      return;
    }
    // 结束：终态只渲染这一次，随即停表。此后残留的 items（running=false）不再
    // 回写视图——曾把用户刚点开的「扫描待办」结果在下一个 tick 冲掉。
    renderQueue(groupsFromQueue(q.items || []), q);
    clearInterval(queueTimer); queueTimer = null;
    toast('任务队列执行结束', 'ok');
  }, 3000);
}
// reattachQueueView 切回任务中心视图时恢复队列进度：仅当本页启动的队列仍在
// 执行才重新开轮询（残留态/别页队列不接管——视图不被旧结果冲掉）。
function reattachQueueView() {
  // 全程异步：go() 在顶层（app.js ~143 行）被调用时，本文件下方 let/const
  //（queueTimer/lastQueueSeq 等）尚未初始化——同步读取即 TDZ ReferenceError
  // 使整个脚本中断。await 之后才碰它们（旧 pollQueueOnce 正是靠开头的 await
  // 侥幸安全）。queueTimer 的"已在跑"判定也挪到 await 后，语义不变。
  (async () => {
    try {
      const q = await api('tasks/queue');
      if (queueTimer) return; // 轮询已在跑（跨视图不中断）
      if (q.started && q.running && (!lastQueueSeq || q.seq === lastQueueSeq)) startQueuePolling();
    } catch (e) { /* 静默 */ }
  })();
}

/* ── 用量 ─────────────────────────────────────────────────────────── */
/* 图表用原生 SVG 手绘：面板是 go:embed 单文件、无构建步骤，引入图表库
   就得带上打包器，得不偿失。这里只需要堆叠柱状图，二十行足够。 */

function fmtTok(n) {
  n = Number(n || 0);
  if (n >= 1e9) return (n / 1e9).toFixed(2) + 'B';
  if (n >= 1e6) return (n / 1e6).toFixed(2) + 'M';
  if (n >= 1e3) return (n / 1e3).toFixed(1) + 'k';
  return String(n);
}
function fmtMs(ms) {
  ms = Number(ms || 0);
  if (!ms) return '—';
  if (ms >= 1000) return (ms / 1000).toFixed(2) + 's';
  return Math.round(ms) + 'ms';
}
function fmtRate(r) { return r ? Number(r).toFixed(1) + ' tok/s' : '—'; }
function trimFixed(s) {
  if (!String(s).includes('.')) return String(s);
  return String(s).replace(/0+$/, '').replace(/\.$/, '');
}
function fmtCredit(n) {
  const v = Number(n || 0);
  if (!Number.isFinite(v)) return '—';
  return trimFixed(v.toFixed(2));
}
function fmtCreditRatio(v, samples, tokens) {
  if (!samples || !tokens) return '—';
  const n = Number(v || 0);
  if (!Number.isFinite(n)) return '—';
  return trimFixed(n.toFixed(4)) + ' / 1M';
}
function fmtModelRate(rate) {
  const s = String(rate || '').trim();
  return s ? 'x' + s : '—';
}

function usStat(v, k, cls) {
  return '<div class="stat ' + (cls || '') + '"><div class="v">' + esc(v) +
         '</div><div class="k">' + esc(k) + '</div></div>';
}

/* usKpi 用量页的指标卡。比账号池的 .stat 多两样：语义色轨（cls）与副标题（sub，
   放"占比 / 均速率"这类解释性数字）；bar 是卡片内的构成条 HTML，只有需要时才传。 */
function usKpi(v, k, cls, sub, bar) {
  return '<div class="kpi ' + (cls || '') + '">' +
    '<div class="k">' + esc(k) + '</div>' +
    '<div class="v">' + esc(v) + '</div>' +
    (bar || '') +
    (sub ? '<div class="s">' + esc(sub) + '</div>' : '') +
    '</div>';
}

/* usMixBar prompt/completion 占比条。宽度按百分比而不是固定像素：表列宽随窗口变化，
   像素宽度在窄屏会溢出、宽屏又显得没信息。 */
function usMixBar(prompt, completion, total) {
  const t = Number(total || 0);
  if (!t) return '';
  const pp = Math.max(0, Math.min(100, Number(prompt || 0) / t * 100));
  const pc = Math.max(0, Math.min(100, Number(completion || 0) / t * 100));
  return '<span class="us-mix" title="prompt ' + pp.toFixed(1) + '% · completion ' + pc.toFixed(1) + '%">' +
    '<i class="p" style="width:' + pp.toFixed(2) + '%"></i>' +
    '<i class="c" style="width:' + pc.toFixed(2) + '%"></i>' +
    '</span>';
}

/* usPct 占比文案（0 值不显示 "0.0%"，直接 —，避免一行全是零）。 */
function usPct(part, total) {
  const t = Number(total || 0);
  if (!t) return '—';
  return (Number(part || 0) / t * 100).toFixed(1) + '%';
}

/* usRow 生成一行。mid 是插在「名称」之后、请求数之前的额外单元格（如「域」列）。
   withPerf 控制延迟/速率两列；列开关显式传入，避免调用方改动后与表头错列。 */
/* nameHtml 非空时替代 esc(name)（已转义的富文本，见 remarkName）：账号维度的第一列
   需要把备注渲染成主标识、uid8 作为浅色虚体跟随，其余维度仍走纯文本转义。 */
function usRow(name, sub, a, mid, withPerf, nameHtml) {
  return '<tr>' +
    '<td class="mark" aria-hidden="true"></td>' +
    '<td>' + (nameHtml || esc(name)) + (sub ? '<div class="note">' + esc(sub) + '</div>' : '') + '</td>' +
    (mid || '') +
    '<td class="num">' + fmtTok(a.requests) + '</td>' +
    '<td class="num">' + (a.errors ? '<span style="color:var(--warn)">' + fmtTok(a.errors) + '</span>' : '—') + '</td>' +
    '<td class="num">' + fmtTok(a.prompt_tokens) + '</td>' +
    '<td class="num">' + fmtTok(a.completion_tokens) + '</td>' +
    '<td class="num">' + fmtTok(a.total_tokens) +
      usMixBar(a.prompt_tokens, a.completion_tokens, a.total_tokens) + '</td>' +
    '<td class="num">' + cacheCell(a) + '</td>' +
    '<td class="num">' + toolSkipCell(a) + '</td>' +
    (withPerf
      ? '<td class="num">' + fmtMs(a.avg_latency_ms) + '</td>' +
        '<td class="num">' + fmtRate(a.avg_tokens_per_second) + '</td>'
      : '') +
    '</tr>';
}

/* ── 用量明细：三个维度共用一张表 + 页内切换 ──────────────────────────
   账号 / 模型 / 域三张表此前各自占一个 box，页面纵向拉得很长且表头结构几乎一样。
   现在合成一个 box：表头由维度定义生成，行渲染复用 usRow，切换零请求。 */
const US_DIMS = {
  account: { key: 'by_account', title: '账号', withRealm: true, withPerf: true, span: 12 },
  model: { key: 'by_model', title: '模型', withRealm: false, withPerf: false, span: 9 },
  realm: { key: 'by_realm', title: 'realm', withRealm: false, withPerf: false, span: 9 },
};

// usSortRows 按当前排序字段降序（默认合计 Token，最大者最相关）。
function usSortRows(rows, sort) {
  const out = rows.slice();
  const num = v => { const n = Number(v || 0); return Number.isFinite(n) ? n : 0; };
  const val = a => sort === 'requests' ? num(a.requests)
    : sort === 'errors' ? num(a.errors)
    : sort === 'latency' ? num(a.avg_latency_ms)
    : num(a.total_tokens);
  out.sort((a, b) => val(b) - val(a));
  return out;
}

function usDimHead(dim) {
  const m = US_DIMS[dim] || US_DIMS.account;
  return '<tr><th class="mark" aria-hidden="true"></th><th>' + esc(m.title) + '</th>' +
    (m.withRealm ? '<th>域</th>' : '') +
    '<th class="num">请求</th><th class="num">失败</th>' +
    '<th class="num">Prompt</th><th class="num">Completion</th><th class="num">合计</th>' +
    '<th class="num">缓存命中</th>' +
    '<th class="num">工具跳过</th>' +
    (m.withPerf ? '<th class="num">均延迟</th><th class="num">均速率</th>' : '') +
    '</tr>';
}

/* usTabsHtml 维度切换按钮（带条数徽标）。整段 innerHTML 重写而不是逐个改 class：
   容器的 click 监听是委托式的，换掉子节点不会丢事件，代码也更短。 */
function usTabsHtml(dims, active, counts) {
  return dims.map(([k, label]) => {
    const n = counts ? counts[k] : null;
    return '<button data-dim="' + k + '"' + (k === active ? ' class="on"' : '') + '>' +
      esc(label) + (n == null ? '' : '<span class="cnt">' + n + '</span>') + '</button>';
  }).join('');
}

const US_DIM_TABS = [['account', '按账号'], ['model', '按模型'], ['realm', '按域']];

function renderUsageDim() {
  const d = usageData || {};
  const m = US_DIMS[usDim] || US_DIMS.account;
  const rows = usSortRows(d[m.key] || [], usSort);
  $('usDimTabs').innerHTML = usTabsHtml(US_DIM_TABS, usDim, {
    account: (d.by_account || []).length,
    model: (d.by_model || []).length,
    realm: (d.by_realm || []).length,
  });
  $('usDimHead').innerHTML = usDimHead(usDim);
  $('usDimBody').innerHTML = rows.map(x => {
    const isAcct = usDim === 'account';
    const key = isAcct ? String(x.key || '').slice(0, 8) : x.key;
    return usRow(key,
      isAcct ? (x.extra || '') : '',
      x,
      isAcct ? '<td class="num">' + esc(x.realm || '') + '</td>' : '',
      m.withPerf,
      isAcct ? remarkName(x.key, esc(key)) : '');
  }).join('') || '<tr><td colspan="' + m.span + '" class="empty">暂无数据</td></tr>';
  $('usDimNote').textContent = rows.length + ' 行 · 请求数含失败尝试';
}

/* 积分扣除：按账号 / 按模型两个维度共用一张表（同上，两个 box 合成一个）。 */
function usCreditHead(dim) {
  return dim === 'model'
    ? '<tr><th class="mark" aria-hidden="true"></th><th>模型</th><th>积分倍率</th>' +
      '<th class="num">请求</th><th class="num">扣除积分</th>' +
      '<th class="num">有效样本 Token</th><th class="num">积分 / 1M Token</th><th class="num">缓存命中率</th></tr>'
    : '<tr><th class="mark" aria-hidden="true"></th><th>账号</th>' +
      '<th class="num">请求</th><th class="num">扣除积分</th>' +
      '<th class="num">有效样本 Token</th><th class="num">积分 / 1M Token</th><th class="num">缓存命中率</th></tr>';
}

/* 缓存命中率（issue #92）：颜色即健康度——≥90% 绿 / 80–90% 黄 / <80% 红，
   样本不足灰。title 带命中/未命中绝对量，供逐项核对。 */
function cacheRateCell(hit, miss) {
  const h = Number(hit || 0), m = Number(miss || 0), total = h + m;
  if (!total) return '<span class="muted">—</span>';
  const pct = h / total * 100;
  const color = pct >= 90 ? 'var(--ok)' : (pct >= 80 ? 'var(--warn)' : 'var(--bad)');
  const txt = trimFixed(pct.toFixed(1)) + '%';
  return '<span style="color:' + color + '" title="命中 ' + fmtTok(h) + ' / 未命中 ' + fmtTok(m) + ' tok">' + txt + '</span>';
}

function renderCreditDim() {
  const d = usageData || {};
  $('usCreditHead').innerHTML = usCreditHead(usCreditDim);
  const empty = '暂无积分扣除记录；升级前仅含 Token 的历史不会伪造积分。';
  if (usCreditDim === 'model') {
    const models = d.credit_by_model || [];
    $('usCreditBody').innerHTML = models.map(row =>
      '<tr>' +
        '<td class="mark" aria-hidden="true"></td>' +
        '<td>' + esc(row.key || '—') + '</td>' +
        '<td>' + esc(fmtModelRate(row.rate)) + '</td>' +
        '<td class="num">' + fmtTok(row.requests) + '</td>' +
        '<td class="num">' + fmtCredit(row.credits) + '</td>' +
        '<td class="num">' + fmtTok(row.credit_tokens) + '</td>' +
        '<td class="num">' + fmtCreditRatio(row.credits_per_1m_tokens, row.credit_samples, row.credit_tokens) + '</td>' +
        '<td class="num">' + cacheRateCell(row.cache_hit_tokens, row.cache_miss_tokens) + '</td>' +
      '</tr>'
    ).join('') || '<tr><td colspan="8" class="empty">' + empty + '</td></tr>';
  } else {
    const accounts = d.credit_by_account || [];
    $('usCreditBody').innerHTML = accounts.map(row => {
      const uid = String(row.key || '');
      const account = row.nickname || uid.slice(0, 8) || '—';
      return '<tr>' +
        '<td class="mark" aria-hidden="true"></td>' +
        '<td>' + remarkName(uid, esc(account)) + '<div class="note">' + esc(row.realm || '') + ' · ' + esc(uid.slice(0, 8)) + '</div></td>' +
        '<td class="num">' + fmtTok(row.requests) + '</td>' +
        '<td class="num">' + fmtCredit(row.credits) + '</td>' +
        '<td class="num">' + fmtTok(row.credit_tokens) + '</td>' +
        '<td class="num">' + fmtCreditRatio(row.credits_per_1m_tokens, row.credit_samples, row.credit_tokens) + '</td>' +
        '<td class="num">' + cacheRateCell(row.cache_hit_tokens, row.cache_miss_tokens) + '</td>' +
        '</tr>';
    }).join('') || '<tr><td colspan="7" class="empty">' + empty + '</td></tr>';
  }
  $('usCreditTabs').innerHTML = usTabsHtml([['account', '按账号'], ['model', '按模型']], usCreditDim, {
    account: (d.credit_by_account || []).length,
    model: (d.credit_by_model || []).length,
  });
}

function renderUsage(d) {
  usageData = d || {};
  const t = usageData.totals || {};
  const total = Number(t.total_tokens || 0);
  const pt = Number(t.prompt_tokens || 0);
  const ct = Number(t.completion_tokens || 0);
  const reqs = Number(t.requests || 0);
  const errs = Number(t.errors || 0);
  const okRate = reqs ? (reqs - errs) / reqs * 100 : null;
  // 构成条要的是合法 CSS 宽度，usPct 在无样本时返回 "—"，不能直接拼进 style。
  const pctW = (part) => total ? Math.max(0, Math.min(100, Number(part || 0) / total * 100)).toFixed(2) + '%' : '0%';
  // 六张卡：主指标用强调色，completion 用成功色（与图表里的绿柱呼应），
  // 失败/延迟只在有值时上语义色——全绿全黄的仪表盘等于没有重点。
  $('usStats').innerHTML =
    usKpi(fmtTok(reqs), '请求数', 'c-accent',
      errs ? '其中失败 ' + errs + ' 次' : '全部成功') +
    usKpi(fmtTok(total), '总 token', 'c-accent',
      'prompt ' + usPct(pt, total) + ' · 输出 ' + usPct(ct, total),
      '<div class="kbar"><i style="width:' + pctW(pt) + ';background:var(--accent)"></i>' +
      '<i style="width:' + pctW(ct) + ';background:var(--ok)"></i></div>') +
    usKpi(fmtTok(pt), 'prompt', 'c-soft', '占比 ' + usPct(pt, total)) +
    usKpi(fmtTok(ct), 'completion', 'c-ok', '占比 ' + usPct(ct, total)) +
    usKpi(String(errs), '失败尝试', errs ? 'c-warn' : 'c-mute',
      okRate == null ? '—' : (errs ? '成功率 ' + okRate.toFixed(1) + '%' : '成功率 100%')) +
    usKpi(fmtMs(t.avg_latency_ms), '平均延迟', 'c-soft',
      t.avg_tokens_per_second ? '吐字 ' + fmtRate(t.avg_tokens_per_second) : '无速率样本') +
    // 缓存命中率进总览：它是和请求数/延迟同级的运行指标。口径是 token 加权，
    // 详细拆解（以及"为什么低"的结构性解释）在下方「缓存效率」区。
    usKpi(cacheRateText(t.cache_hit_tokens, t.cache_miss_tokens), '缓存命中率',
      cacheRateClass(t.cache_hit_tokens, t.cache_miss_tokens),
      t.cache_hit_tokens || t.cache_miss_tokens
        ? '命中 ' + fmtTok(t.cache_hit_tokens) + ' · 未命中 ' + fmtTok(t.cache_miss_tokens)
        : '上游未返回缓存字段');

  // 卡片、明细表与时序图全部按所选窗口统计（切窗口数字随之变化）；
  // 「全部历史」含 90 天前折叠出的日桶。这里标注当前口径与数据起点。
  const winLabel = trangeLabel('usRange');
  // 服务端回显的实际区间优先（自定义区间下它就是权威口径）；滚动窗口没有回显，
  // 用控件自己的标签。
  const rangeEcho = usageData.window_from
    ? String(usageData.window_from).replace('T', ' ').slice(0, 16) +
      (usageData.window_to ? ' → ' + String(usageData.window_to).replace('T', ' ').slice(0, 16) : ' → 现在')
    : '';
  const note = (rangeEcho || winLabel ? (rangeEcho || winLabel) + ' · ' : '') +
    (usageData.buckets || 0) + ' 个分桶' +
    (usageData.since ? ' · 数据自 ' + usageData.since.replace('T', ' ') : '') +
    (usageData.file_bytes ? ' · 文件 ' + (usageData.file_bytes / 1024).toFixed(1) + ' KB' : '');
  $('usNote').textContent = note;
  $('usNote').title = note; // 窄屏单行截断时靠悬停看全

  // 积分扣除的四张卡片与说明。
  $('usCreditStats').innerHTML =
    usKpi(fmtCredit(t.credits), '扣除积分', 'c-accent', '按上游 usage.credit 累计') +
    usKpi(fmtTok(t.credit_tokens), '匹配 Token', 'c-mute', '与积分同时观测到的 Token') +
    usKpi(fmtCreditRatio(t.credits_per_1m_tokens, t.credit_samples, t.credit_tokens),
      '平均积分 / 1M Token', 'c-ok', '越低越划算') +
    usKpi(String(t.credit_samples || 0), '有效积分样本', 'c-mute', '缺字段的历史不参与折算');
  $('usCreditNote').textContent =
    (usageData.credit_by_account || []).length + ' 个账号 · ' +
    (usageData.credit_by_model || []).length + ' 个模型倍率分组 · 仅统计与积分同时观测到的 Token';

  renderUsageDim();
  renderCreditDim();
  renderCacheBox(d);
  renderUsageChart(usageData.series || []);
}

// 维度切换 / 排序控件。
if ($('usDimTabs')) $('usDimTabs').addEventListener('click', ev => {
  const b = ev.target.closest('button[data-dim]');
  if (!b) return;
  usDim = b.dataset.dim;
  renderUsageDim();
});
if ($('usCreditTabs')) $('usCreditTabs').addEventListener('click', ev => {
  const b = ev.target.closest('button[data-dim]');
  if (!b) return;
  usCreditDim = b.dataset.dim;
  renderCreditDim();
});
if ($('usSort')) $('usSort').onchange = () => {
  usSort = $('usSort').value;
  renderUsageDim();
};

/* renderUsageChart 画堆叠柱状图。
 *
 * x 轴是**真实时间轴**，不是按序号等距。这一点很重要：数据里存在 1 小时的
 * 间隔，也存在 6~8 小时的断档（没请求的时段不产生桶），等距排布会把 8 小时
 * 画得和 1 小时一样宽，让「什么时候用的」完全失真。
 *
 * 另外不再用 preserveAspectRatio="none"：那会把 viewBox 横向拉伸到容器宽度，
 * 柱子和文字都变形。改为固定比例、按容器宽度自适应高度。
 *
 * viewBox 取 1200×200（原 760×180）：SVG 以 width:100% 渲染，高宽比决定实际
 * 高度——旧比例在 1500px 宽的主区里会撑到 ~355px，只有一两根柱子时整块几乎是
 * 空白。宽 viewBox 把同宽度下的高度压到 ~250px，与下方表格的视觉重量相当。
 *
 * 时间轴用本地时间解析（后端返回的就是本地时区），day 点按当天 00:00 参与定位，
 * 与 hour 点在同一个连续轴上——日桶本来就是他那天所有小时的聚合。
 */

/* parsePointTime 把后端的 t 解析成毫秒时间戳。 */
function parsePointTime(p) {
  // hour: "2026-09-16T13"  day: "2026-09-16"
  const s = p.t.length === 13 ? p.t + ':00:00' : p.t + 'T00:00:00';
  const d = new Date(s);
  return isNaN(d.getTime()) ? null : d.getTime();
}

/* fmtTokTimeLabel 时间桶的短标签，与 x 轴刻度同一口径（日桶 MM-DD，小时桶 HH:00）。 */
function fmtTokTimeLabel(p) {
  const d = new Date(p.t);
  return p.scope === 'day'
    ? (d.getMonth() + 1) + '-' + String(d.getDate()).padStart(2, '0')
    : String(d.getHours()).padStart(2, '0') + ':00';
}

function renderUsageChart(series) {
  const host = $('usChart');

  // 丢掉时间解析不出来的点，而不是让 NaN 传染整张图。
  const pts = [];
  for (const p of series) {
    const t = parsePointTime(p);
    if (t === null) continue;
    const pt = Number(p.prompt_tokens || 0);
    const ct = Number(p.completion_tokens || 0);
    pts.push({ t, scope: p.scope, raw: p.t, pt, ct, tt: Number(p.total_tokens || 0) || (pt + ct),
               req: p.requests || 0 });
  }
  if (!pts.length) {
    host.innerHTML = '<div class="us-empty">暂无用量数据。发起一次对话后再刷新。</div>';
    $('usChartNote').textContent = '—';
    return;
  }

  const W = 1200, H = 200, PL = 58, PR = 14, PT = 18, PB = 30;
  const iw = W - PL - PR, ih = H - PT - PB;

  const t0 = pts[0].t;
  const t1 = pts[pts.length - 1].t;
  const span = Math.max(1, t1 - t0);

  const max = Math.max(1, ...pts.map(p => p.tt));
  const peak = pts.reduce((a, b) => (b.tt > a.tt ? b : a), pts[0]);
  const avg = pts.reduce((s, p) => s + p.tt, 0) / pts.length;
  $('usChartNote').textContent =
    pts.length + ' 个点 · 峰值 ' + fmtTok(peak.tt) + ' @ ' + fmtTokTimeLabel(peak) +
    ' · 均值 ' + fmtTok(avg);

  // 柱宽取「最小真实间隔」的 70%，并夹在合理区间内——窗口拉到 30 天时柱子会
  // 变细，但不会细到看不见。
  let minGap = Infinity;
  for (let i = 1; i < pts.length; i++) minGap = Math.min(minGap, pts[i].t - pts[i - 1].t);
  if (!isFinite(minGap) || minGap <= 0) minGap = span;
  const slot = iw * (minGap / span);
  const bw = Math.max(2, Math.min(30, slot * 0.7));

  // 首尾各让出半个柱宽：否则第一个点和最后一个点的柱子会各有一半跑到绘图区外
  // （末点柱子贴着卡片右边缘被切掉），刻度仍用同一个 xOf，标签与柱子始终对齐。
  const xOf = t => PL + bw / 2 + (t - t0) / span * Math.max(1, iw - bw);
  const yOf = v => PT + ih - ih * (v / max);

  let out = '<svg viewBox="0 0 ' + W + ' ' + H + '" role="img" ' +
            'preserveAspectRatio="xMidYMid meet">';

  // 柱体渐变：顶部实、底部略透，堆叠时两段仍能一眼分清（纯色块并排会糊成一片）。
  // 注意 stop-color 必须走 style 而不是 presentation 属性——Blink/WebKit 不解析
  // 属性里的 var()，写成 stop-color="var(--accent)" 会整条渐变失效（柱子全透明）。
  out += '<defs>' +
    '<linearGradient id="usGradP" x1="0" y1="0" x2="0" y2="1">' +
    '<stop offset="0" style="stop-color:var(--accent);stop-opacity:1"/>' +
    '<stop offset="1" style="stop-color:var(--accent);stop-opacity:.6"/></linearGradient>' +
    '<linearGradient id="usGradC" x1="0" y1="0" x2="0" y2="1">' +
    '<stop offset="0" style="stop-color:var(--ok);stop-opacity:1"/>' +
    '<stop offset="1" style="stop-color:var(--ok);stop-opacity:.6"/></linearGradient>' +
    '</defs>';

  // y 轴网格 + 刻度
  for (let i = 0; i <= 4; i++) {
    const y = PT + ih - (ih * i / 4);
    out += '<line class="gl" x1="' + PL + '" y1="' + y.toFixed(1) + '" x2="' + (W - PR) +
           '" y2="' + y.toFixed(1) + '"/>';
    out += '<text class="tk" x="' + (PL - 6) + '" y="' + (y + 3.5).toFixed(1) +
           '" text-anchor="end">' + fmtTok(max * i / 4) + '</text>';
  }

  // 均值参考线：一眼看出"这根是不是异常高"，比只给刻度省心。
  // 标签放左侧：右侧常被峰值柱占用（峰值柱往往就是最后一根），贴左不会被压住。
  if (avg > 0 && avg < max) {
    const y = yOf(avg);
    out += '<line class="avg" x1="' + PL + '" y1="' + y.toFixed(1) + '" x2="' + (W - PR) +
           '" y2="' + y.toFixed(1) + '"/>';
    out += '<text class="tk-avg" x="' + (PL + 5) + '" y="' + (y - 4).toFixed(1) +
           '" text-anchor="start">均值 ' + fmtTok(avg) + '</text>';
  }

  // 柱子
  const yBase = PT + ih;
  for (const p of pts) {
    const x = xOf(p.t) - bw / 2;
    const hTot = ih * (p.tt / max);
    const hP = p.tt ? hTot * (p.pt / p.tt) : 0;
    const hC = Math.max(p.tt && p.ct ? 1 : 0, hTot - hP);
    // 每根柱子包一个 <g>，把 <title> 放进去。
    //
    // 为什么必须包裹：SVG 里 <title> 描述的是它的**父元素**。此前 <title> 是
    // <rect> 的兄弟节点（rect 自闭合，无法包含子节点），于是全部平铺在 <svg>
    // 根下 —— 整张图只有一个 tooltip（浏览器取第一个），悬停任何柱子都显示同一
    // 份数据（issue #128）。包进 <g> 后 tooltip 跟随该柱，且堆叠的两段
    //（prompt + completion）共用同一个提示。
    out += '<g><title>' + esc(p.raw) + '  ' + fmtTok(p.pt) + ' prompt / ' +
           fmtTok(p.ct) + ' completion / ' + p.req + ' 次</title>';
    // 圆角只给堆叠顶端（贴轴的底边保持方角，柱子才像"立"在基线上）。
    // 类名用 usbar 而不是 bar：账号池的积分条是 .bar{height:3px}，而 SVG2 里
    // height 是 rect 的 CSS 几何属性，同名类会把每根柱子压成 3px 高（踩过）。
    if (hP > 0) out += '<rect class="usbar" x="' + x.toFixed(2) + '" y="' + (yBase - hP).toFixed(2) +
      '" width="' + bw.toFixed(2) + '" height="' + hP.toFixed(2) +
      '" fill="url(#usGradP)"' + (hC > 0 ? '' : ' rx="1.5"') + '/>';
    if (hC > 0) out += '<rect class="usbar" x="' + x.toFixed(2) + '" y="' + (yBase - hP - hC).toFixed(2) +
      '" width="' + bw.toFixed(2) + '" height="' + hC.toFixed(2) +
      '" fill="url(#usGradC)" rx="1.5"/>';
    out += '</g>';
  }

  // 峰值标注：柱子够窄时文字压在柱顶，够宽时贴右侧避免和柱体重叠。
  {
    const px = xOf(peak.t);
    const py = yOf(peak.tt);
    const anchor = px > W - PR - 90 ? 'end' : 'middle';
    out += '<text class="tk-peak" x="' + Math.max(PL, Math.min(W - PR, px)).toFixed(1) +
           '" y="' + Math.max(10, py - 5).toFixed(1) + '" text-anchor="' + anchor + '">' +
           '峰值 ' + fmtTok(peak.tt) + '</text>';
  }

  // x 轴基线画在柱子之后，避免压在柱底
  out += '<line class="ax" x1="' + PL + '" y1="' + yBase + '" x2="' + (W - PR) +
         '" y2="' + yBase + '"/>';

  // x 轴刻度：按真实时间等距取 6 个位置，取该位置**最近的实际柱子**做标签，
  // 所以标签永远落在有数据的点上，不会指到空档里。
  const TICKS = Math.min(6, pts.length);
  const usedLabel = new Set();
  for (let k = 0; k < TICKS; k++) {
    const target = t0 + span * (TICKS === 1 ? 0.5 : k / (TICKS - 1));
    let bi = 0, best = Infinity;
    for (let i = 0; i < pts.length; i++) {
      const d = Math.abs(pts[i].t - target);
      if (d < best) { best = d; bi = i; }
    }
    if (usedLabel.has(bi)) continue;
    usedLabel.add(bi);
    const p = pts[bi];
    // 首尾标签靠边对齐，避免被裁掉
    const cx = xOf(p.t);
    const anchor = cx < PL + 14 ? 'start' : (cx > W - PR - 14 ? 'end' : 'middle');
    out += '<text class="tk" x="' + Math.max(PL, Math.min(W - PR, cx)).toFixed(1) +
           '" y="' + (PT + ih + 15) + '" text-anchor="' + anchor + '">' + esc(fmtTokTimeLabel(p)) + '</text>';
  }

  // 跨天时补一条日期分隔线，让「日界」在长窗口里可见
  let prevDay = null;
  for (const p of pts) {
    const d = new Date(p.t).getDate();
    if (prevDay !== null && d !== prevDay) {
      const x = xOf(p.t).toFixed(1);
      out += '<line class="gl" x1="' + x + '" y1="' + PT + '" x2="' + x + '" y2="' +
             (PT + ih) + '" style="opacity:.45"/>';
    }
    prevDay = d;
  }

  out += '</svg>';
  host.innerHTML = out;
}

/* renderCacheBox 缓存效率区（四张 KPI + 一张时序图）。
 *
 * 为什么单开一块而不是并进「用量总览」那六张卡：缓存要回答三个不同层次的问题——
 * ① 命中率多少；② 命中/未命中各多少 token；③ 这个命中率是不是**结构性**的。
 * 前两个塞得进卡片，第三个只能靠解释性文案；混在一排运行指标里会被直接扫过去，
 * 于是「kimi-k3.1-pro 命中率 19%」这种采样错觉又会被当成故障来查。
 *
 * 口径：命中率 = 命中 / (命中 + 未命中)，**token 加权**。绝不按请求条数——
 * 短请求（< 128 tok 一个缓存块）物理不可命中，条数口径会被它们整体拉低
 * （实测同一模型两种口径能差 70 个百分点）。
 */
function renderCacheBox(d) {
  const t = (d && d.totals) || {};
  const hit = Number(t.cache_hit_tokens || 0);
  const miss = Number(t.cache_miss_tokens || 0);
  const tot = hit + miss;
  const reqs = Number(t.requests || 0);
  const pt = Number(t.prompt_tokens || 0);
  const avgP = reqs ? pt / reqs : 0;
  const pct = tot ? hit / tot * 100 : 0;
  const cls = !tot ? 'c-mute' : (pct >= 90 ? 'c-ok' : (pct >= 80 ? 'c-warn' : 'c-bad'));

  // 平均 prompt 不足一个缓存块时，低命中率是结构性的、不是故障：把结论直接写进
  // 副标题，用户不必悬停也能读对。反之才提示口径本身。
  const sub = avgP && avgP < CACHE_BLOCK_TOKENS
    ? '平均 prompt ' + Math.round(avgP) + ' tok < ' + CACHE_BLOCK_TOKENS + ' tok 缓存块，结构性不可命中'
    : 'token 加权，非请求条数';

  $('usCacheStats').innerHTML =
    usKpi(tot ? trimFixed(pct.toFixed(1)) + '%' : '—', '缓存命中率', cls, sub,
      '<div class="kbar"><i style="width:' + (tot ? pct.toFixed(2) : 0) + '%;background:var(--ok)"></i>' +
      '<i style="width:' + (tot ? (100 - pct).toFixed(2) : 0) + '%;background:var(--bad)"></i></div>') +
    usKpi(fmtTok(hit), '命中 token', 'c-ok',
      tot ? '占缓存观测 ' + usPct(hit, tot) : '无缓存观测') +
    usKpi(fmtTok(miss), '未命中 token', miss ? 'c-bad' : 'c-mute',
      miss ? '按未命中单价计费' : (tot ? '全部命中' : '无缓存观测')) +
    usKpi(fmtTok(pt), 'prompt 总量', 'c-soft',
      tot ? '观测覆盖 ' + usPct(tot, pt) : '上游未返回缓存字段');

  const nc = $('usCacheNote');
  nc.textContent = '命中率 = 命中 / (命中 + 未命中) token · 含失败尝试';
  nc.title = nc.textContent;

  renderCacheChart((d && d.series) || []);
  renderCacheModels((d && d.by_model) || []);
}

/* renderCacheModels 按模型的缓存排行。
 *
 * 🔴 排序键是**未命中 token**，不是命中率。这是实测数据给出的结论（2026-10-28，
 * 线上 6 小时窗口）：
 *
 *	deepseek-v4.1-flash   命中率 94.6%   未命中 58.2M tok   ← 率最高，浪费却最多
 *	glm-5.3               命中率 26.5%   未命中  3.1M tok   ← 率最低，浪费只有前者的 5%
 *	kimi-k3.1-pro         命中率 66.8%   未命中 11.0M tok
 *
 * 单看命中率会把 glm-5.3 判成"最差的模型"，实际上它重算的 token 连 deepseek 的
 * 零头都不到；而 deepseek 的 94.6% 一眼看过去像"没问题"，它的绝对重算量却是
 * 全场第一。命中率低的模型如果量小，管它没有收益。
 *
 * 所以：**条形长度 = 命中/未命中的构成，右侧数字 = 未命中绝对量，颜色 = 命中率档位**。
 * 三个维度一起看才不会误判——这也和面板别处"命中率 × 前缀占比一起看"的口径一致。
 *
 * 只列有缓存观测的模型（hit+miss>0）：没有观测的模型画成 0% 会凭空造出"崩了"的假信号。
 */
function renderCacheModels(rows) {
  const host = $('usCacheModels');
  if (!host) return;
  const note = $('usCacheModelNote');

  const list = (rows || []).map(x => {
    const hit = Number(x.cache_hit_tokens || 0);
    const miss = Number(x.cache_miss_tokens || 0);
    const tot = hit + miss;
    const reqs = Number(x.requests || 0);
    const pt = Number(x.prompt_tokens || 0);
    // 平均 prompt = 该模型的典型上下文长度。不足一个缓存块时低命中率是结构性的
    //（上游按 128 tok 对齐，物理上凑不满一块），不是模型表现差。
    const avgP = reqs ? pt / reqs : 0;
    return {
      key: String(x.key || ''),
      hit: hit, miss: miss, tot: tot, reqs: reqs,
      rate: tot ? hit / tot * 100 : 0,
      avgP: avgP,
      structural: avgP > 0 && avgP < CACHE_BLOCK_TOKENS,
    };
  }).filter(x => x.tot > 0);

  if (!list.length) {
    host.innerHTML = '<div class="us-empty">窗口内没有可归因到模型的缓存观测。</div>';
    if (note) note.textContent = '—';
    return;
  }

  list.sort((a, b) => b.miss - a.miss);
  const totalMiss = list.reduce((s, x) => s + x.miss, 0);
  // 只画前 8 个，其余合并成一行：超过 8 行就开始需要滚动，纵向比较反而失效。
  const TOP = 8;
  const shown = list.slice(0, TOP);
  const rest = list.slice(TOP);

  const rateCls = p => (p >= 90 ? 'c-ok' : (p >= 80 ? 'c-warn' : 'c-bad'));
  const pctStr = p => trimFixed(p.toFixed(1)) + '%';

  let html = '<div class="cm-row cm-head">' +
    '<span>模型</span><span>命中 / 未命中</span>' +
    '<span style="text-align:right">命中率</span>' +
    '<span style="text-align:right">未命中</span></div>';

  for (const x of shown) {
    const hp = x.hit / x.tot * 100;
    const tip = x.key + '\n' +
      '命中 ' + fmtTok(x.hit) + ' tok · 未命中 ' + fmtTok(x.miss) + ' tok\n' +
      '命中率 ' + pctStr(x.rate) + '（' + x.reqs + ' 次请求）\n' +
      '平均 prompt ' + Math.round(x.avgP) + ' tok' +
      (x.structural ? '（低于 ' + CACHE_BLOCK_TOKENS + ' tok 缓存块，结构性不可命中）' : '');
    html += '<div class="cm-row" title="' + esc(tip) + '">' +
      '<span class="cm-name">' + esc(x.key) +
        (x.structural ? '<span class="mk" title="平均 prompt 不足一个缓存块">结构</span>' : '') +
      '</span>' +
      '<span class="cm-bar"><i class="h" style="width:' + hp.toFixed(2) + '%"></i>' +
        '<i class="m" style="width:' + (100 - hp).toFixed(2) + '%"></i></span>' +
      '<span class="cm-rate ' + rateCls(x.rate) + '">' + pctStr(x.rate) + '</span>' +
      '<span class="cm-miss">' + fmtTok(x.miss) + '</span>' +
      '</div>';
  }

  if (rest.length) {
    const rh = rest.reduce((s, x) => s + x.hit, 0);
    const rm = rest.reduce((s, x) => s + x.miss, 0);
    const rt = rh + rm;
    const rp = rt ? rh / rt * 100 : 0;
    html += '<div class="cm-row cm-rest" title="' +
      esc('其余 ' + rest.length + ' 个模型合计（汇总行，不参与排名）') + '">' +
      '<span class="cm-name">其余 ' + rest.length + ' 个（合计）</span>' +
      '<span class="cm-bar"><i class="h" style="width:' + (rt ? rp.toFixed(2) : 0) + '%"></i>' +
        '<i class="m" style="width:' + (rt ? (100 - rp).toFixed(2) : 0) + '%"></i></span>' +
      '<span class="cm-rate ' + rateCls(rp) + '">' + (rt ? pctStr(rp) : '—') + '</span>' +
      '<span class="cm-miss">' + fmtTok(rm) + '</span></div>';
  }

  host.innerHTML = html;
  if (note) {
    const topMiss = shown.reduce((s, x) => s + x.miss, 0);
    note.textContent = list.length + ' 个模型有缓存观测 · 前 ' + shown.length +
      ' 个占未命中 ' + usPct(topMiss, totalMiss);
    note.title = '未命中 token = 本可复用却重新计算的量，直接按未命中单价计费';
  }
}

/* renderCacheChart 缓存的时序视图：柱 = 命中/未命中 token（左轴），折线 = 命中率（右轴）。
 *
 * 双轴的理由：柱高读**绝对量**（这小时用了多少 token），折线读**比例**（这小时有多少
 * 是复用的）。只看柱高看不出退化——请求量下降时 miss 也下降，柱子变矮但命中率可能
 * 是 0；只看折线又分不清「90% 命中」是 10 tok 还是 10M tok，而这两者的费用差着量级。
 *
 * 没有缓存观测的桶直接跳过而不是按 0% 画：那段时间多半是失败/探活请求（上游连 usage
 * 都没回），画成 0% 会凭空造出一段「命中率崩了」的假信号。
 */
function renderCacheChart(series) {
  const host = $('usCacheChart');
  if (!host) return;
  const pts = [];
  for (const p of series || []) {
    const t = parsePointTime(p);
    if (t === null) continue;
    const hit = Number(p.cache_hit_tokens || 0);
    const miss = Number(p.cache_miss_tokens || 0);
    const tot = hit + miss;
    if (!tot) continue;
    pts.push({ t: t, raw: p.t, scope: p.scope, hit: hit, miss: miss, tot: tot,
               rate: hit / tot * 100, req: p.requests || 0 });
  }
  if (!pts.length) {
    host.innerHTML = '<div class="us-empty">窗口内没有缓存观测记录。上游不返回 prompt_cache_hit_tokens 时此图留空。</div>';
    $('usCacheChartNote').textContent = '—';
    return;
  }

  const W = 1200, H = 200, PL = 58, PR = 46, PT = 18, PB = 30;
  const iw = W - PL - PR, ih = H - PT - PB, yBase = PT + ih;
  const t0 = pts[0].t, t1 = pts[pts.length - 1].t;
  const span = Math.max(1, t1 - t0);
  const max = Math.max(1, ...pts.map(p => p.tot));
  const peak = pts.reduce((a, b) => (b.tot > a.tot ? b : a), pts[0]);
  const sumHit = pts.reduce((s, p) => s + p.hit, 0);
  const sumTot = pts.reduce((s, p) => s + p.tot, 0);
  $('usCacheChartNote').textContent =
    pts.length + ' 个点 · 整体 ' + trimFixed((sumHit / sumTot * 100).toFixed(1)) + '%' +
    ' · 峰值 ' + fmtTok(peak.tot) + ' @ ' + fmtTokTimeLabel(peak);

  let minGap = Infinity;
  for (let i = 1; i < pts.length; i++) minGap = Math.min(minGap, pts[i].t - pts[i - 1].t);
  if (!isFinite(minGap) || minGap <= 0) minGap = span;
  const slot = iw * (minGap / span);
  const bw = Math.max(2, Math.min(30, slot * 0.7));
  const xOf = t => PL + bw / 2 + (t - t0) / span * Math.max(1, iw - bw);
  const yTok = v => PT + ih - ih * (v / max);
  const yRate = v => PT + ih - ih * (v / 100);

  let out = '<svg viewBox="0 0 ' + W + ' ' + H + '" role="img" preserveAspectRatio="xMidYMid meet">';
  // 渐变走 style 而不是 presentation 属性：Blink/WebKit 不解析属性里的 var()，
  // 写成 stop-color="var(--ok)" 会让整条渐变失效（柱子全透明，踩过）。
  out += '<defs>' +
    '<linearGradient id="usCGradH" x1="0" y1="0" x2="0" y2="1">' +
    '<stop offset="0" style="stop-color:var(--ok);stop-opacity:1"/>' +
    '<stop offset="1" style="stop-color:var(--ok);stop-opacity:.55"/></linearGradient>' +
    '<linearGradient id="usCGradM" x1="0" y1="0" x2="0" y2="1">' +
    '<stop offset="0" style="stop-color:var(--bad);stop-opacity:1"/>' +
    '<stop offset="1" style="stop-color:var(--bad);stop-opacity:.55"/></linearGradient>' +
    '</defs>';

  for (let i = 0; i <= 4; i++) {
    const y = PT + ih - (ih * i / 4);
    out += '<line class="gl" x1="' + PL + '" y1="' + y.toFixed(1) + '" x2="' + (W - PR) +
           '" y2="' + y.toFixed(1) + '"/>';
    out += '<text class="tk" x="' + (PL - 6) + '" y="' + (y + 3.5).toFixed(1) +
           '" text-anchor="end">' + fmtTok(max * i / 4) + '</text>';
  }
  // 右轴只标 0/50/100%：五个百分比刻度和左轴的 token 刻度会在同一水平线上打架。
  for (const r of [0, 50, 100]) {
    out += '<text class="tk" x="' + (W - PR + 7) + '" y="' + (yRate(r) + 3.5).toFixed(1) +
           '" text-anchor="start">' + r + '%</text>';
  }

  for (const p of pts) {
    const x = xOf(p.t) - bw / 2;
    const hTot = ih * (p.tot / max);
    const hH = p.tot ? hTot * (p.hit / p.tot) : 0;
    const hM = Math.max(p.tot && p.miss ? 1 : 0, hTot - hH);
    // <title> 必须包在柱子的 <g> 里：SVG 的 title 描述父元素，平铺到根下会让整张图
    // 共用一个 tooltip（悬停哪根都显示第一根的数据，issue #128）。
    out += '<g><title>' + esc(p.raw) + '  命中 ' + fmtTok(p.hit) + ' / 未命中 ' + fmtTok(p.miss) +
           ' = ' + trimFixed(p.rate.toFixed(1)) + '% · ' + p.req + ' 次</title>';
    if (hH > 0) out += '<rect class="usbar" x="' + x.toFixed(2) + '" y="' + (yBase - hH).toFixed(2) +
      '" width="' + bw.toFixed(2) + '" height="' + hH.toFixed(2) +
      '" fill="url(#usCGradH)"' + (hM > 0 ? '' : ' rx="1.5"') + '/>';
    if (hM > 0) out += '<rect class="usbar" x="' + x.toFixed(2) + '" y="' + (yBase - hH - hM).toFixed(2) +
      '" width="' + bw.toFixed(2) + '" height="' + hM.toFixed(2) +
      '" fill="url(#usCGradM)" rx="1.5"/>';
    out += '</g>';
  }

  // 命中率折线画在柱子之上（否则被柱体遮住），点太密时省掉圆点只留折线。
  let line = '';
  for (const p of pts) {
    line += (line ? ' ' : '') + xOf(p.t).toFixed(1) + ',' + yRate(p.rate).toFixed(1);
  }
  out += '<polyline class="rt" points="' + line + '"/>';
  if (pts.length <= 80) {
    for (const p of pts) {
      out += '<circle class="rt-dot" cx="' + xOf(p.t).toFixed(1) + '" cy="' +
             yRate(p.rate).toFixed(1) + '" r="2"/>';
    }
  }

  out += '<line class="ax" x1="' + PL + '" y1="' + yBase + '" x2="' + (W - PR) +
         '" y2="' + yBase + '"/>';

  const TICKS = Math.min(6, pts.length);
  const usedLabel = new Set();
  for (let k = 0; k < TICKS; k++) {
    const target = t0 + span * (TICKS === 1 ? 0.5 : k / (TICKS - 1));
    let bi = 0, best = Infinity;
    for (let i = 0; i < pts.length; i++) {
      const dd = Math.abs(pts[i].t - target);
      if (dd < best) { best = dd; bi = i; }
    }
    if (usedLabel.has(bi)) continue;
    usedLabel.add(bi);
    const p = pts[bi], cx = xOf(p.t);
    const anchor = cx < PL + 14 ? 'start' : (cx > W - PR - 14 ? 'end' : 'middle');
    out += '<text class="tk" x="' + Math.max(PL, Math.min(W - PR, cx)).toFixed(1) +
           '" y="' + (PT + ih + 15) + '" text-anchor="' + anchor + '">' +
           esc(fmtTokTimeLabel(p)) + '</text>';
  }

  let prevDay = null;
  for (const p of pts) {
    const dd = new Date(p.t).getDate();
    if (prevDay !== null && dd !== prevDay) {
      const x = xOf(p.t).toFixed(1);
      out += '<line class="gl" x1="' + x + '" y1="' + PT + '" x2="' + x + '" y2="' +
             (PT + ih) + '" style="opacity:.45"/>';
    }
    prevDay = dd;
  }

  out += '</svg>';
  host.innerHTML = out;
}

function fmtTokTip(v) { return fmtTok(v); }

let usageRateWarmAt = 0;
async function warmUsageModelRates() {
  if (Date.now() - usageRateWarmAt < 10 * 60 * 1000) return;
  try {
    await api('models');
  } catch (e) {
    // 倍率回填是可选增强；失败不阻塞用量统计，10 分钟后再试。
  }
  usageRateWarmAt = Date.now();
}

async function loadUsage() {
  const q = trangeQuery('usRange', true);
  try {
    await warmUsageModelRates();
    const d = await api('usage?' + q.toString());
    renderUsage(d);
  } catch (e) {
    // 失败时三块都要清干净：只改图表会留下上一次窗口的数字，看起来像"刷新成功"。
    usageData = null;
    $('usChart').innerHTML = '<div class="us-empty">读取用量失败：' + esc(e.message) + '</div>';
    $('usChartNote').textContent = '—';
    $('usStats').innerHTML = '';
    $('usCacheStats').innerHTML = '';
    $('usCacheChart').innerHTML = '<div class="us-empty">读取用量失败</div>';
    $('usCacheChartNote').textContent = '—';
    if ($('usCacheModels')) $('usCacheModels').innerHTML = '';
    if ($('usCacheModelNote')) $('usCacheModelNote').textContent = '—';
    $('usCacheNote').textContent = '—';
    $('usCreditStats').innerHTML = '';
    $('usNote').textContent = '—';
    $('usCreditNote').textContent = '—';
    $('usDimNote').textContent = '—';
    $('usDimHead').innerHTML = '';
    $('usCreditHead').innerHTML = '';
    $('usDimTabs').innerHTML = usTabsHtml(US_DIM_TABS, usDim, null);
    $('usCreditTabs').innerHTML = usTabsHtml([['account', '按账号'], ['model', '按模型']], usCreditDim, null);
    $('usDimBody').innerHTML = '<tr><td colspan="12" class="empty">读取用量失败</td></tr>';
    $('usCreditBody').innerHTML = '<tr><td colspan="7" class="empty">读取用量失败</td></tr>';
  }
}

if ($('btnUsage')) $('btnUsage').onclick = loadUsage;
// 时间范围控件绑定：任何改动（预设切换 / 自定义起止）都重新拉一次用量。
if ($('usRange')) trangeBind('usRange', loadUsage);

/* ── 积分构成 ─────────────────────────────────────────────────────── */
/* 一个账号的余额是若干积分包之和。包按来源命名（「国内运营裂变包」「拉新权益包」
   「个人体验版」…），面额从 6 到 1500 不等，且**按次发放**。所以两个任务完成度
   完全一致的账号，余额可能差上千——差别只在包里。这里把逐包明细摊开，并给每个
   包名一个稳定配色，跨账号对比时同色即同类。 */

const PK_COLORS = ['#4f8cff', '#25b08b', '#e8a33d', '#c96bd6', '#e2607a',
                   '#5aa9e6', '#8fbf3f', '#b58b5a', '#7d8fa8', '#d4785c'];
const PK_ACCOUNT_COLORS = ['#4f8cff', '#25b08b', '#e8a33d', '#c96bd6',
                           '#e2607a', '#20a4a4', '#8fbf3f', '#d4785c',
                           '#7c83db', '#c48a2f', '#b45f8c', '#5aa9e6'];

function pkColor(i) { return PK_COLORS[i % PK_COLORS.length]; }

// pkAccountColorMap 按 UID 稳定分配颜色：排序后分配，账号刷新/重排不会换色。
function pkAccountColorMap(list) {
  const uids = (list || [])
    .filter(a => a && !a.error && a.uid)
    .map(a => String(a.uid))
    .sort();
  const colors = new Map();
  uids.forEach((uid, i) => colors.set(uid, PK_ACCOUNT_COLORS[i % PK_ACCOUNT_COLORS.length]));
  return colors;
}

/* pkBySource 把包按名称归并，得到「来源 → 面额/余额/个数」。这是对比的关键视图：
   两个号的差异一定体现在某几个来源的面额上。 */
function pkBySource(packs) {
  const m = new Map();
  for (const p of packs) {
    // 分组键用 code + name，而不是只 name：上游给「首登赠送」和普通活动包用了
    // **同一个 PackageName 和同一个 PackageCode**，只按 name 会把两类混成一类，
    // 那正是当初「两个号为何差 1500」看不出来的原因。这里至少把 code 带进键里，
    // 并在卡片上显示最早的发放时间。
    const k = (p.package_code || '') + '|' + (p.name || '(未命名)');
    const e = m.get(k) || {
      key: k, name: p.name || '(未命名)', code: p.package_code || '',
      n: 0, remain: 0, size: 0, used: 0, minEnd: '', minCreated: '',
    };
    e.n += 1;
    e.remain += Number(p.remain || 0);
    e.size += Number(p.size || 0);
    e.used += Number(p.used || 0);
    const t = (p.end_time || '').slice(0, 10);
    if (t && (!e.minEnd || t < e.minEnd)) e.minEnd = t;
    const c = (p.created_at || '').slice(0, 10);
    if (c && (!e.minCreated || c < e.minCreated)) e.minCreated = c;
    m.set(k, e);
  }
  return [...m.values()].sort((a, b) => b.size - a.size);
}

const PK_DEFAULT_DETAIL_LIMIT = 5;

function pkDetailLimitValue(raw) {
  const n = Number(raw);
  return Number.isFinite(n) && n > 0 ? Math.floor(n) : PK_DEFAULT_DETAIL_LIMIT;
}

function pkDetailLimit(cfg) {
  return pkDetailLimitValue(cfg && cfg.panel && cfg.panel.package_detail_limit);
}

const PK_DAY_MS = 24 * 3600 * 1000;

function pkExpiryMs(p) {
  const raw = Number(p && p.expires_at);
  if (Number.isFinite(raw) && raw > 0) return raw;
  const text = String((p && p.end_time) || '').trim();
  if (!text) return null;
  let iso = text.includes('T') ? text : text.replace(' ', 'T');
  if (!/(?:Z|[+-]\d\d:\d\d)$/.test(iso)) iso += '+08:00';
  const parsed = Date.parse(iso);
  return Number.isFinite(parsed) ? parsed : null;
}

// pkDetailCompare 只服务单账号逐包明细：正余额包先按到期时间挑选默认展示项，
// 其余正余额包与已用完包分别折叠；同一到期时间按面额降序。主键跟随视图排序
// 模式（pkSortMode，声明在本区块末尾的绑定块）：end_asc 到期升序、size_desc
// 面额降序（同面额按到期升序）。typeof 守卫：前端 harness 的区域切片求值里
// 没有该全局，回落 end_asc（= 上游原有语义，切片测试的期望序不受影响）。
function pkDetailCompare(a, b) {
  const sizeOf = p => {
    const n = Number(p && p.size);
    return Number.isFinite(n) ? n : 0;
  };
  const mode = (typeof pkSortMode === 'string' && pkSortMode) || 'end_asc';
  if (mode === 'size_desc') {
    const d = sizeOf(b) - sizeOf(a);
    if (d !== 0) return d;
  }
  const ea = pkExpiryMs(a), eb = pkExpiryMs(b);
  if (ea == null && eb != null) return 1;
  if (ea != null && eb == null) return -1;
  if (ea != null && eb != null && ea !== eb) return ea - eb;
  return sizeOf(b) - sizeOf(a);
}

function pkDetailGroups(packs, limit) {
  const active = [], used = [];
  let usedSize = 0, restSize = 0, restRemain = 0;
  for (const p of packs || []) {
    const remain = Number(p && p.remain);
    if (remain > 0) {
      active.push(p);
      continue;
    }
    used.push(p);
    const size = Number(p && p.size);
    if (Number.isFinite(size)) usedSize += size;
  }
  active.sort(pkDetailCompare);
  used.sort(pkDetailCompare);
  const visible = active.slice(0, pkDetailLimitValue(limit));
  const rest = active.slice(visible.length);
  for (const p of rest) {
    const size = Number(p && p.size);
    if (Number.isFinite(size)) restSize += size;
    const remain = Number(p && p.remain);
    if (Number.isFinite(remain)) restRemain += remain;
  }
  return { visible, rest, used, restSize, restRemain, usedSize };
}

function pkCreditOpacity(days) {
  if (days == null || !Number.isFinite(Number(days))) return 1;
  return 0.25 + 0.75 * Math.max(0, Math.min(29, Number(days) - 1)) / 29;
}

function pkExpiryText(expiresAt) {
  if (!expiresAt) return '无到期时间';
  const diff = expiresAt - Date.now();
  if (diff <= 0) return '已到期';
  const minutes = Math.max(1, Math.ceil(diff / 60000));
  if (minutes < 60) return '剩余 ' + minutes + ' 分钟';
  const hours = Math.ceil(diff / 3600000);
  if (hours < 24) return '剩余 ' + hours + ' 小时';
  return '剩余 ' + Math.ceil(diff / PK_DAY_MS) + ' 天';
}

function pkExpiryDateTime(expiresAt) {
  if (!expiresAt) return '—';
  return new Date(expiresAt).toLocaleString('zh-CN', {
    timeZone: 'Asia/Shanghai', hour12: false,
    year: 'numeric', month: '2-digit', day: '2-digit',
    hour: '2-digit', minute: '2-digit', second: '2-digit',
  });
}

function pkAccountSegments(a, now) {
  let balance = Math.max(0, Number(a.remain || 0));
  const out = [];
  for (const p of a.packages || []) {
    const remain = Number(p.remain || 0);
    if (!Number.isFinite(remain) || remain <= 0 || balance <= 0) continue;
    const amount = Math.min(balance, remain);
    const expiresAt = pkExpiryMs(p);
    out.push({
      amount,
      expiresAt,
      days: expiresAt == null ? null : Math.max(0, Math.ceil((expiresAt - now) / PK_DAY_MS)),
      source: p.name || '积分',
      uid: String(a.uid || ''),
      accountName: a.nickname || String(a.uid || '').slice(0, 8) || '未命名账号',
    });
    balance -= amount;
  }
  return out.sort((x, y) => {
    if (x.expiresAt == null && y.expiresAt != null) return 1;
    if (x.expiresAt != null && y.expiresAt == null) return -1;
    return (x.expiresAt || 0) - (y.expiresAt || 0);
  });
}

// summarizeCreditDays 对齐 WorkDaddy：按精确剩余天数逐行聚合，无有效到期时间的余额
// 不进入图表，也不猜测到期日。账号内先按总余额约束逐包金额，避免上游重复记录膨胀。
function summarizeCreditDays(list, now) {
  const buckets = new Map();
  let unavailable = 0;
  for (const a of list || []) {
    if (a.error || !Number.isFinite(Number(a.remain))) {
      unavailable++;
      continue;
    }
    for (const segment of pkAccountSegments(a, now)) {
      if (segment.days == null) continue;
      let row = buckets.get(segment.days);
      if (!row) {
        row = { days: segment.days, credits: 0, segments: [] };
        buckets.set(segment.days, row);
      }
      row.credits += segment.amount;
      row.segments.push(segment);
    }
  }
  const rows = [...buckets.values()].sort((a, b) => a.days - b.days);
  for (const row of rows) {
    row.segments.sort((a, b) =>
      (a.expiresAt || Infinity) - (b.expiresAt || Infinity) ||
      a.accountName.localeCompare(b.accountName) ||
      a.source.localeCompare(b.source));
  }
  return { rows, accountCount: (list || []).length, unavailable };
}

function renderExpiryDistribution(list, now) {
  const summary = summarizeCreditDays(list, now);
  const colors = pkAccountColorMap(list);
  const rows = summary.rows.map(row => {
    const total = row.credits || 1;
    const nodes = row.segments.map(segment => {
      const color = colors.get(segment.uid) || 'var(--accent)';
      const title = segment.source + '\n' + fmtTok(segment.amount) + ' 积分\n到期时间 ' +
        pkExpiryDateTime(segment.expiresAt) + '（' + pkExpiryText(segment.expiresAt) + '）\n' +
        segment.accountName;
      return '<span class="pk-expiry-seg" style="--seg-color:' + color +
        ';opacity:' + pkCreditOpacity(segment.days).toFixed(5) +
        ';flex:' + Math.max(0.008, segment.amount / total).toFixed(4) +
        ' 1 0" title="' + esc(title) + '" aria-label="' + esc(title) + '"></span>';
    }).join('');
    return '<div class="pk-expiry-row"><span>' + esc(row.days === 0 ? '已到期' : row.days + ' 天') +
      '</span><div class="pk-expiry-track">' + nodes + '</div><b>' + esc(fmtTok(row.credits)) +
      '</b></div>';
  }).join('');
  const foot = summary.accountCount + ' 个账号' +
    (summary.unavailable ? ' · ' + summary.unavailable + ' 个未获取余额' : '');
  const legend = (list || []).filter(a =>
    a && !a.error && a.uid && pkAccountSegments(a, now).some(s => s.days != null)
  ).map(a => '<span><i style="background:' + (colors.get(String(a.uid)) || 'var(--accent)') +
    '"></i>' + esc(a.nickname || String(a.uid).slice(0, 8)) + '</span>').join('');
  const hdr = '<div class="pk-expiry-hdr"><span>剩余天数</span><span style="text-align:center">各账号该批剩余</span><b>剩余积分</b></div>';
  $('pkExpiry').innerHTML = (rows
    ? hdr + '<div class="pk-expiry-chart">' + rows + '</div>'
    : '<div class="pk-expiry-empty">暂无可汇总积分</div>') +
    (legend ? '<div class="pk-expiry-legend">' + legend + '</div>' : '') +
    '<div class="pk-expiry-foot">' + esc(foot) + '</div>';
}

function renderPackages(d, detailLimit) {
  const list = (d.accounts || []);
  const now = Date.now();
  const expiryColors = pkAccountColorMap(list);
  renderExpiryDistribution(list, now);
  if (!list.length) {
    $('pkSummary').innerHTML = '<div class="empty">没有账号</div>';
    return;
  }

  // 包名 → 稳定色号（跨账号一致，方便肉眼对齐）
  const names = [];
  for (const a of list) for (const s of pkBySource(a.packages || [])) {
    if (!names.includes(s.key)) names.push(s.key);
  }
  names.sort((x, y) => {
    const sz = n => Math.max(...list.map(a => {
      const f = pkBySource(a.packages || []).find(s => s.key === n);
      return f ? f.size : 0;
    }));
    return sz(y) - sz(x);
  });
  const colorOf = n => pkColor(names.indexOf(n));
  // 键 → 展示名，供卡片与明细表共用（同一来源必然同色同名）。
  const labelOf = {};
  for (const a of list) for (const s of pkBySource(a.packages || [])) labelOf[s.key] = s;

  const maxRemain = Math.max(1, ...list.map(a => Number(a.remain || 0)));

  $('pkSummary').innerHTML = list.map(a => {
    if (a.error) {
      return '<div class="pk-card"><div class="who"><span class="nm">' +
        esc((a.nickname || a.uid.slice(0, 8))) + '</span>' +
        '<span class="realm">' + esc(a.realm || '') + '</span></div>' +
        '<div class="err">查询失败：' + esc(a.error) + '</div></div>';
    }
    const srcs = pkBySource(a.packages || []);
    const total = Math.max(1, Number(a.size || 0));
    const bar = srcs.map(s =>
      '<i style="width:' + (s.size / total * 100).toFixed(2) + '%;background:' +
      colorOf(s.key) + '" title="' + esc(s.name) + ' ' + fmtTok(s.size) + '"></i>'
    ).join('');
    const legend = srcs.map(s =>
      '<span><i style="background:' + colorOf(s.key) + '"></i>' +
      esc(s.name.replace(/^CodeBuddy/, '')) + ' x' + s.n + ' · ' + fmtTok(s.size) +
      (s.minCreated ? ' · 首发 ' + esc(s.minCreated.slice(5)) : '') + '</span>'
    ).join('');
    const expiry = pkAccountSegments(a, now);
    const expiryTotal = Math.max(1, expiry.reduce((sum, s) => sum + s.amount, 0));
    const expiryColor = expiryColors.get(String(a.uid)) || 'var(--accent)';
    const expiryBar = expiry.length ? '<div class="expirybar" role="img" aria-label="积分到期分布">' +
      expiry.map(s => {
        const title = s.source + '\n' + fmtTok(s.amount) + ' 积分\n到期时间 ' +
          pkExpiryDateTime(s.expiresAt) + '（' + pkExpiryText(s.expiresAt) + '）';
        return '<i style="background:' + expiryColor +
          ';opacity:' + pkCreditOpacity(s.days).toFixed(5) +
          ';flex:' + Math.max(0.008, s.amount / expiryTotal).toFixed(4) +
          ' 1 0" title="' + esc(title) + '"></i>';
      }).join('') + '</div>' : '';
    return '<div class="pk-card">' +
      '<div class="who"><span class="nm">' + esc(a.nickname || a.uid.slice(0, 8)) + '</span>' +
      '<span class="realm">' + esc(a.realm || '') + '</span></div>' +
      '<div class="big">' + fmtTok(a.remain) + '</div>' +
      '<div class="sub">共 ' + fmtTok(a.size) + ' · ' + (a.packages || []).length +
      ' 个包 · 占最高 ' + (Number(a.remain || 0) / maxRemain * 100).toFixed(0) + '%</div>' +
      '<div class="mixbar">' + bar + '</div>' +
      expiryBar +
      '<div class="pk-legend">' + legend + '</div>' +
      '</div>';
  }).join('');

  $('pkNote').textContent = list.length + ' 个账号 · 实时查询上游';

  // 逐包明细：每个账号一个表，排序规则由视图顶部的选择器决定（默认到期近的在前）
  $('pkDetail').innerHTML = list.map(a => {
    if (a.error) return '';
    const groups = pkDetailGroups(a.packages || [], detailLimit);
    const rowOf = (p, rowGroup) => {
      const k = (p.package_code || '') + '|' + (p.name || '(未命名)');
      const sub = (p.sub_product_code || '').replace(/^sp_tcaca_codebuddyide_?/, '') ||
                  (p.package_code || '').replace(/^TCACA_/, '');
      return '<tr' + (rowGroup ? ' class="pk-hidden-row pk-' + rowGroup +
        '-row" data-pk-row="' + rowGroup + '" hidden' : '') +
        '><td class="mark" aria-hidden="true"><i style="background:' +
        colorOf(k) + '"></i></td>' +
      '<td>' + esc(p.name || '(未命名)') +
        (sub ? '<div class="note">' + esc(sub) + '</div>' : '') + '</td>' +
      '<td class="num">' + fmtTok(p.size) + '</td>' +
      '<td class="num">' + fmtTok(p.remain) + '</td>' +
      '<td class="num">' + fmtTok(p.used) + '</td>' +
      '<td class="num">' + esc((p.created_at || '').slice(0, 16).replace('T', ' ') || '—') + '</td>' +
      '<td class="num">' + esc((p.end_time || '').slice(0, 10) || '—') + '</td>' +
      '</tr>';
    };
    const groupSummary = (group, label, count, size, remain) =>
      '<tr class="pk-group-summary"><td colspan="7"><button type="button" class="pk-group-toggle"' +
      ' data-pk-group="' + group + '" data-count="' + count + '" data-size="' + size +
      '" data-remain="' + remain + '" aria-expanded="false">' + label + '，展开</button></td></tr>';
    const rows = groups.visible.map(p => rowOf(p, '')).join('');
    const restSummary = groups.rest.length
      ? groupSummary('rest', '其余未用完 ' + groups.rest.length + ' 个包（面额合计 ' +
          fmtTok(groups.restSize) + ' · 剩余 ' + fmtTok(groups.restRemain) + '）',
          groups.rest.length, groups.restSize, groups.restRemain) +
        groups.rest.map(p => rowOf(p, 'rest')).join('')
      : '';
    const usedSummary = groups.used.length
      ? groupSummary('used', '已用完 ' + groups.used.length + ' 个包（面额合计 ' +
          fmtTok(groups.usedSize) + '）', groups.used.length, groups.usedSize, 0) +
        groups.used.map(p => rowOf(p, 'used')).join('')
      : '';
    return '<div class="box"><header><h3>' +
      esc(a.nickname || a.uid.slice(0, 8)) + ' · ' + esc(a.realm || '') +
      '</h3><span class="grow"></span><span class="note">余额 ' + fmtTok(a.remain) +
      ' / 总额 ' + fmtTok(a.size) + ' · 可用 ' + (groups.visible.length + groups.rest.length) + ' 个包' +
      (groups.used.length ? ' / 已用完 ' + groups.used.length + ' 个' : '') +
      ' · 默认展示最早到期 ' + pkDetailLimitValue(detailLimit) + ' 条（' +
      esc(PK_SORT_LABELS[pkSortMode] || '') + '）</span>' +
      '</header><div class="tbl-wrap"><table class="acc"><thead><tr>' +
      '<th class="mark" aria-hidden="true"></th><th>包名 / 来源</th>' +
      '<th class="num">面额</th><th class="num">剩余</th><th class="num">已用</th>' +
      '<th class="num">发放</th><th class="num">到期</th>' +
      '</tr></thead><tbody>' + rows + restSummary + usedSummary + '</tbody></table></div></div>';
  }).join('');
}

if ($('pkDetail')) $('pkDetail').addEventListener('click', ev => {
  const btn = ev.target.closest('button[data-pk-group]');
  if (!btn) return;
  const body = btn.closest('tbody');
  if (!body) return;
  const group = btn.dataset.pkGroup;
  const expanded = btn.getAttribute('aria-expanded') === 'true';
  body.querySelectorAll('tr[data-pk-row="' + group + '"]').forEach(row => { row.hidden = expanded; });
  const count = btn.dataset.count || '0';
  const size = btn.dataset.size || '0';
  const remain = btn.dataset.remain || '0';
  btn.setAttribute('aria-expanded', String(!expanded));
  if (group === 'rest') {
    btn.textContent = expanded
      ? '其余未用完 ' + count + ' 个包（面额合计 ' + fmtTok(size) + ' · 剩余 ' +
        fmtTok(remain) + '），展开'
      : '收起其余未用完 ' + count + ' 个包';
  } else {
    btn.textContent = expanded
      ? '已用完 ' + count + ' 个包（面额合计 ' + fmtTok(size) + '），展开'
      : '收起已用完 ' + count + ' 个包';
  }
});

/* ── 逐包明细排序模式 ─────────────────────────────────────────────── */
/* 逐包明细的排序规则（选择持久化在 localStorage，跨会话记住）：
   end_asc   到期升序（默认）——快过期的包排最前，提醒优先消耗；无到期时间的
             包（上游没下发 end_time）没有可比的日期，统一垫底，不掺进日期序里；
   size_desc 面额降序——原来的展示口径，看「钱从哪来」。
   行序统一由 pkDetailCompare 实现（明细折叠分组共用同一比较器，切换模式时
   折叠组内行序同步跟随）。本块整体放在 renderPackages / renderExpiryDistribution
   之后：前端 harness 按区域切片求值（[PK_ACCOUNT_COLORS, renderExpiryDistribution)
   与 [PK_DEFAULT_DETAIL_LIMIT, renderPackages)），顶层 localStorage/$ 语句落进
   切片区会让无 DOM 桩的求值环境 ReferenceError——上游测试的切片边界不动。 */
const LS_PK_SORT = 'pkSortMode';
let pkSortMode = localStorage.getItem(LS_PK_SORT) || 'end_asc';
const PK_SORT_LABELS = { end_asc: '按到期升序 · 近的在前', size_desc: '按面额降序' };
// 排序切换时重排明细需要 detailLimit（上游折叠配置），loadPackages 拉到后缓存。
let lastDetailLimit = PK_DEFAULT_DETAIL_LIMIT;

// 排序规则控件：恢复上次选择并绑定切换。
if ($('pkSort')) {
  $('pkSort').value = pkSortMode;
  if ($('pkSort').value !== pkSortMode) {        // localStorage 里存了废弃值：回落默认
    pkSortMode = 'end_asc';
    localStorage.setItem(LS_PK_SORT, pkSortMode);   // 不用 removeItem：harness 桩无此方法
  }
  $('pkSort').onchange = () => {
    pkSortMode = $('pkSort').value;
    localStorage.setItem(LS_PK_SORT, pkSortMode);
    if (lastPackages) renderPackages(lastPackages, lastDetailLimit);   // 数据在内存，直接重排
  };
}

// loadPackages 按需拉取：有缓存（上次任意入口拉到的 packages 数据）先直接渲染，
// 不再「切进积分构成页就打一次上游」——逐账号实时查询在账号多时既慢又压上游。
// 无缓存（首次进入）才自动查一次；显式刷新（force=true，点「刷新」按钮）始终实时查。
async function loadPackages(force) {
  if (!force && lastPackages) {
    renderPackages(lastPackages, lastDetailLimit);
    const ageMin = Math.floor((Date.now() - lastPackagesAt) / 60000);
    if (ageMin > 0) $('pkNote').textContent = lastPackages.accounts.length + ' 个账号 · ' + ageMin + ' 分钟前的数据，点「刷新」更新';
    return;
  }
  $('pkSummary').innerHTML = '<div class="empty">查询中…（逐账号向上游实时查询）</div>';
  $('pkDetail').innerHTML = '';
  $('pkExpiry').innerHTML = '<div class="pk-expiry-empty">查询中…</div>';
  try {
    const [d, c] = await Promise.all([
      api('packages', { timeoutMs: API_SLOW_GET_TIMEOUT_MS }),   // 逐账号查上游，慢
      api('config').catch(() => null),
    ]);
    lastPackages = d;                            // 缓存供首页到期卡片与排序切换复用
    lastPackagesAt = Date.now();
    lastDetailLimit = pkDetailLimit(c && c.config);
    renderPackages(d, lastDetailLimit);
  } catch (e) {
    $('pkSummary').innerHTML = '<div class="empty">读取失败：' + esc(e.message) + '</div>';
    $('pkExpiry').innerHTML = '<div class="pk-expiry-empty">读取失败：' + esc(e.message) + '</div>';
  }
}

/* ── 积分到期提醒（首页卡片）────────────────────────────────────────── */
/* 积分不是永久的：签到/任务发的裂变包约一个月失效。只看「剩余积分 ÷ 日消耗」
   会系统性偏乐观——用不完的部分到期直接蒸发。这里把「最近要过期的是哪批、
   有多少、到期前每天要至少消耗多少」顶到首页，数据源与「积分构成」共用
   （lastPackages 缓存，EXP_FRESH_MS 内复用，不重复打上游）。 */

// expBatches 把某账号的包聚合成「到期日 → 该日作废积分」升序列表。
// 只统计 remain>0 且有到期时间的包——没余额/长期包到期没有任何影响。
function expBatches(packs) {
  const byDay = new Map();
  for (const p of packs || []) {
    const r = Number(p.remain || 0);
    const t = (p.end_time || '').slice(0, 10);
    if (r <= 0 || !t) continue;
    byDay.set(t, (byDay.get(t) || 0) + r);
  }
  return [...byDay.entries()]
    .map(([date, remain]) => ({ date, remain }))
    .sort((a, b) => (a.date < b.date ? -1 : 1));
}

function expDaysLeft(dateStr, today) {
  return Math.round((new Date(dateStr + 'T00:00:00') - today) / 86400000);
}

// expiryRowsHtml 到期提醒的逐账号行渲染。**「检查」与首页快照共用这一个函数**，
// 保证两条数据路径（逐包明细 / state 里的按日批次）输出完全一致：排序、今天/明天
// 文案、日均需耗、7 天内合计、随后批次都只有一份实现。
// list 每项形态：{ uid, nickname, packages:[{end_time, remain}], error? }。
function expiryRowsHtml(list) {
  const today = new Date(); today.setHours(0, 0, 0, 0);
  // 按「最近到期」升序排（issue #125）。
  //
  // 后端 /panel/api/packages 是按**余额降序**返回的，恰好把 CN 账号都排在前面、
  // global 排在末尾，看起来像"按域分组"，其实只是余额顺序。这里再排一次，让
  // "最快过期的排最前"这个唯一重要的顺序成立，且不分域。
  //
  // 排序键用最早到期批次的日期（expBatches 已按日期升序，故 [0] 即最早）。
  // 查询失败的账号、以及 7 天内无到期的账号没有可比较的到期时间，统一排在最后，
  // 保持它们原本的相对顺序（稳定排序）。
  const keyed = list.map(a => {
    let key = null;
    if (!a.error) {
      const bs = expBatches(a.packages).filter(b => expDaysLeft(b.date, today) >= 0);
      if (bs.length) key = bs[0].date;
    }
    return { a, key };
  });
  keyed.sort((x, y) => {
    if (x.key !== y.key) {
      if (x.key === null) return 1;
      if (y.key === null) return -1;
      return x.key < y.key ? -1 : 1;
    }
    if (x.key === null) return 0;
    // 同日到期的账号按昵称/uid 兜底排序：两条数据路径（检查的余额降序、
    // overview 的 uid 升序）输入顺序不同，加固定 tie-break 才能保证输出一致。
    const ax = String(x.a.nickname || x.a.uid || '');
    const ay = String(y.a.nickname || y.a.uid || '');
    return ax < ay ? -1 : ax > ay ? 1 : 0;
  });
  // 「7 天内无到期」的账号**不再逐个占一行**：130 个号里绝大多数是这种，会把
  // 整张卡片撑成 130 行、把下面的账号表挤到几屏之外（实测 1700px 窗口都看不到表）。
  // 收集成末尾一行汇总；有到期的、查询失败的照旧逐行显示（那才是要看的信息）。
  const rows = [];
  let noExpN = 0;
  keyed.forEach(({ a }) => {
    const name = a.nickname || String(a.uid || '').slice(0, 8);
    // 注意：这里必须是 rows.push 而不能 return —— 外层已经从 map 改成 forEach，
    // return 的字符串会被丢弃，查询失败的账号会静默消失（测试 TestAppJSExpirySortedByExpiry
    // 就是靠这条抓住的）。
    if (a.error) {
      rows.push('<div class="exp-row"><span class="exp-dot" style="background:var(--ink-3)"></span>' +
        '<span class="exp-nm">' + esc(name) + '</span>' +
        '<span class="exp-main err">查询失败：' + esc(a.error) + '</span></div>');
      return;
    }
    const bs = expBatches(a.packages).filter(b => expDaysLeft(b.date, today) >= 0);
    if (!bs.length) { noExpN++; return; }
    const first = bs[0];
    const days = expDaysLeft(first.date, today);
    const daily = Math.ceil(first.remain / Math.max(1, days));
    const week = bs.filter(b => expDaysLeft(b.date, today) <= 7)
      .reduce((s, b) => s + b.remain, 0);
    // 危险度：≤3 天红（不抓紧就真没了）、≤7 天琥珀、更远绿。
    // 上游扣包是 FEFO（按失效时刻升序，实测两号口径一致）：这些快过期批次正是
    // 被消耗得最快的，日均需耗给的是「哪怕单靠这个账号的自然流量也能对齐」的参照。
    const cls = days <= 3 ? 'var(--bad)' : days <= 7 ? 'var(--warn)' : 'var(--ok)';
    const dayWord = days === 0 ? '今天到期' : days === 1 ? '明天到期' : days + ' 天后到期';
    const more = bs.length > 4 ? '　等 ' + bs.length + ' 批' : '';
    const rest = bs.slice(1, 4).map(b =>
      '随后 ' + esc(b.date.slice(5)) + ' · ' + fmtTok(b.remain)).join('　') + more;
    rows.push('<div class="exp-row"><span class="exp-dot" style="background:' + cls + '"></span>' +
      '<span class="exp-nm">' + esc(name) + '</span>' +
      '<span class="exp-main">最近到期 <b>' + esc(first.date) + '</b>（' + dayWord +
      '）· 该批 <b>' + fmtTok(first.remain) + '</b> 积分 · 到期前日均需耗 ≥<b>' +
      fmtTok(daily) + '</b>' +
      (week > first.remain ? ' · 7 天内合计 ' + fmtTok(week) : '') +
      (rest ? '<div class="note">' + rest + '</div>' : '') +
      '</span></div>');
  });
  if (noExpN > 0) {
    rows.push('<div class="exp-row"><span class="exp-dot" style="background:var(--ok)"></span>' +
      '<span class="exp-nm">其余 ' + noExpN + ' 个号</span>' +
      '<span class="exp-main">7 天内无到期积分</span></div>');
  }
  return rows.join('');
}

function renderExpiry(d) {
  const list = (d.accounts || []);
  const rows = expiryRowsHtml(list);
  $('expList').innerHTML = rows || '<div class="empty">没有账号</div>';
  // 数据新鲜度透明化：走缓存时标注年龄，免得把旧数据误当实时。
  const ageMin = lastPackages ? Math.floor((Date.now() - lastPackagesAt) / 60000) : 0;
  $('expNote').textContent = (lastPackagesAt && ageMin > 0)
    ? list.length + ' 个账号 · ' + ageMin + ' 分钟前的数据，可点「检查」刷新'
    : list.length + ' 个账号 · 实时查询上游';
  $('expBox').hidden = false;
}

/* 快照版到期提醒：不查上游，直接用 /overview 里的 credits_expiry_buckets。
   后端每次余额刷新（默认每 5 分钟）按自然日聚合并写入 state.json，口径与
   「检查」的逐包按日汇总完全相同；这里把批次还原成 packages 形态后直接复用
   expiryRowsHtml，因此排序/今天明天/日均需耗/7 天内合计/随后批次全部一致。

   旧 state（升级后尚未跑过一轮余额刷新，或 Redis 快照来自旧版本）没有 buckets，
   退回只显示最早一批，保证卡片不空白；下一轮刷新后自动变成完整视图。 */
function renderExpirySnapshot(d) {
  const list = (d && d.accounts) || [];
  const hasBuckets = list.some(a => Array.isArray(a.credits_expiry_buckets) && a.credits_expiry_buckets.length);
  if (hasBuckets) {
    const isoDay = v => {
      const dt = new Date(v);
      if (isNaN(dt.getTime())) return '';
      const p2 = n => String(n).padStart(2, '0');
      return dt.getFullYear() + '-' + p2(dt.getMonth() + 1) + '-' + p2(dt.getDate());
    };
    // 逐账号取批次；升级过渡期/单号刷新失败的账号可能还没有 buckets，用旧的
    // 「最早一批」字段兜底，避免在同一次渲染里被误报成「无到期」。
    const packagesOf = a => {
      const bs = a.credits_expiry_buckets || [];
      if (bs.length) {
        return bs.map(b => ({ end_time: (b.date || '') + 'T00:00:00', remain: b.remain }));
      }
      const day = isoDay(a.credits_earliest_expiry);
      return (day && a.credits_earliest_remaining)
        ? [{ end_time: day + 'T00:00:00', remain: a.credits_earliest_remaining }]
        : [];
    };
    renderExpiry({
      accounts: list.map(a => ({
        uid: a.uid,
        nickname: a.nickname,
        realm: a.realm,
        packages: packagesOf(a),
      })),
    });
    $('expNote').textContent = snapshotNoteText(list.length, d);
    return;
  }
  // 旧 state 兜底：只有最早一批的日期与数量。
  const today = new Date(); today.setHours(0, 0, 0, 0);
  const keyed = list.map(a => {
    // 后端给的是 RFC3339 带时区；无到期信息（长期包/未刷新）的账号排在最后。
    let key = null, ms = 0;
    if (a.credits_earliest_expiry) {
      const t = new Date(a.credits_earliest_expiry);
      if (!isNaN(t.getTime())) { ms = t.getTime(); key = ms; }
    }
    return { a, key, ms };
  });
  keyed.sort((x, y) => {
    if (x.key === null && y.key === null) return 0;
    if (x.key === null) return 1;
    if (y.key === null) return -1;
    return x.key - y.key;
  });
  // 与 expiryRowsHtml 同一处理：「无近期到期」的账号不逐个占一行，末尾汇总。
  // 否则 130 个号会把这张卡片撑成 130 行，账号表被挤到几屏之外。
  const rows = [];
  let noExpN = 0;
  keyed.forEach(({ a, ms }) => {
    const nm = esc(a.nickname || String(a.uid || '').slice(0, 8));
    if (!ms || !a.credits_earliest_remaining) { noExpN++; return; }
    // 与 expDaysLeft 同口径：按本地自然日算，而不是对精确时间戳 ceil（后者会把
    // 明天/今天各多算一天，日均需耗也随之少一半）。
    const dt = new Date(ms);
    const p2 = n => String(n).padStart(2, '0');
    const when = dt.getFullYear() + '-' + p2(dt.getMonth() + 1) + '-' + p2(dt.getDate());
    const days = Math.max(0, Math.round((new Date(when + 'T00:00:00') - today) / 86400000));
    const remain = Number(a.credits_earliest_remaining || 0);
    const daily = Math.ceil(remain / Math.max(1, days));
    const cls = days <= 3 ? 'var(--bad)' : days <= 7 ? 'var(--warn)' : 'var(--ok)';
    const dayWord = days === 0 ? '今天到期' : days === 1 ? '明天到期' : days + ' 天后到期';
    rows.push('<div class="exp-row"><span class="exp-dot" style="background:' + cls + '"></span>' +
      '<span class="exp-nm">' + nm + '</span>' +
      '<span class="exp-main">最近到期 <b>' + esc(when) + '</b>（' + dayWord +
      '）· 该批 <b>' + fmtTok(remain) + '</b> 积分 · 到期前日均需耗 ≥<b>' +
      fmtTok(daily) + '</b>' +
      '<div class="note">仅最早一批，点「检查」看全部到期批次</div>' +
      '</span></div>');
  });
  if (noExpN > 0) {
    rows.push('<div class="exp-row"><span class="exp-dot" style="background:var(--ok)"></span>' +
      '<span class="exp-nm">其余 ' + noExpN + ' 个号</span>' +
      '<span class="exp-main">无近期到期批次</span></div>');
  }
  $('expList').innerHTML = rows.join('') || '<div class="empty">没有账号</div>';
  $('expNote').textContent = snapshotNoteText(list.length, d);
  $('expBox').hidden = false;
}

// snapshotNoteText 快照卡片的数据新鲜度标注。界面本身 5 秒轮询一次，但卡片数据
// 只随余额后台刷新更新（默认每 5 分钟）——不标出来的话，5 秒级的界面刷新会让人
// 误以为数据也是 5 秒级新鲜。放在 renderExpirySnapshot 之后：前端 harness 按
// 「renderExpirySnapshot 起」切片求值，函数声明提升让它在同一切片内可用。
function snapshotNoteText(count, d) {
  const iv = Number(d && d.balance_refresh_sec) || 0;
  const at = d && d.balance_refreshed_at ? new Date(d.balance_refreshed_at).getTime() : 0;
  if (iv <= 0) {
    return count + ' 个账号 · 快照（来自本地状态 · 未开启后台刷新，点「检查」拉实时明细）';
  }
  const every = iv % 60 === 0 ? iv / 60 + ' 分钟' : iv + ' 秒';
  let tail = '每 ' + every + '后台刷新';
  if (at && !isNaN(at)) {
    const min = Math.floor((Date.now() - at) / 60000);
    tail += '，' + (min < 1 ? '刚刚更新' : min + ' 分钟前更新');
  }
  return count + ' 个账号 · 快照（' + tail + '，点「检查」拉实时明细）';
}

async function loadExpiry(force) {
  if (!$('expBox')) return;
  if (expFetching) return;
  const fresh = lastPackages && (Date.now() - lastPackagesAt) < EXP_FRESH_MS;
  if (fresh && !force) { renderExpiry(lastPackages); return; }
  expFetching = true;
  $('expBox').hidden = false;
  if (!$('expList').children.length) $('expList').innerHTML = '<div class="empty">查询中…（逐账号向上游实时查询）</div>';
  $('expNote').textContent = '查询中…';
  try {
    const d = await api('packages', { timeoutMs: API_SLOW_GET_TIMEOUT_MS });   // 逐账号查上游，慢
    lastPackages = d;                            // 与「积分构成」视图共用同一份缓存
    lastPackagesAt = Date.now();
    renderExpiry(d);
  } catch (e) {
    $('expNote').textContent = '查询失败：' + esc(e.message);
  }
  expFetching = false;
}

if ($('btnExp')) $('btnExp').onclick = () => loadExpiry(true);

// 「刷新」始终实时查（显式 force，不依赖事件对象真值）
if ($('btnPk')) $('btnPk').onclick = () => loadPackages(true);

/* ══ 账号备注（只写本机 localStorage；账号池 / 用量两个视图共用同一份映射）══════
   键取 uid 前 8 位——与 renderUsageDim 账号维度的 key 和积分扣除表的 uid 前缀同源，
   三个表格因此都能直接 join。展示层只替换文本并挂 .rm-ghost 虚体，账号标识本身
   仍由服务端下发，不写回后端；备注是纯展示层信息，不参与选号 / 计费 / 鉴权。 */
function remarksAll() {
  if (remarksCache) return remarksCache;
  try { remarksCache = JSON.parse(localStorage.getItem(LS_REMARKS)) || {}; }
  catch (e) { remarksCache = {}; }
  return remarksCache;
}
function remarkKey(uid) { return String(uid == null ? '' : uid).slice(0, 8); }
function remarkOf(uid) {
  const k = remarkKey(uid);
  return k ? String(remarksAll()[k] || '') : '';
}
/* nameHtml 必须是已转义的 HTML；有备注时返回「备注 + 原标识（浅色虚体）」 */
function remarkName(uid, nameHtml) {
  const r = remarkOf(uid);
  return r ? esc(r) + '<span class="rm-ghost">' + nameHtml + '</span>' : nameHtml;
}
function storeRemarks(map) {
  remarksCache = map;
  try { localStorage.setItem(LS_REMARKS, JSON.stringify(map)); } catch (e) {}
}

/* 编辑器的行来自 overview 缓存（账号池加载过就有），另外补上「已有备注但当前列表里
   没有」的键，避免账号被移除后备注看起来凭空消失。 */
function remarkFormRows() {
  const rows = ((overviewData && overviewData.accounts) || [])
    .map(s => ({ uid: remarkKey(s.uid), who: s.nickname || remarkKey(s.uid) }))
    .filter(r => r.uid);
  for (const k of Object.keys(remarksAll())) {
    if (!rows.some(r => r.uid === k)) rows.push({ uid: k, who: k });
  }
  return rows;
}
function renderRemarkBody() {
  const box = $('remarkBody');
  if (!box) return;
  const rows = remarkFormRows();
  box.innerHTML = rows.length
    ? rows.map(r => '<div class="rm-row"><span class="rm-who" title="' + esc(r.who) + '">' + esc(r.who) + '</span>' +
        '<input type="text" data-uid="' + esc(r.uid) + '" value="' + esc(remarksAll()[r.uid] || '') + '" placeholder="备注（留空即清除）" maxlength="24"></div>').join('')
    : '<div class="empty">暂无账号：先在「账号池」加载一次账号列表。</div>';
}
function openRemarks() {
  renderRemarkBody();
  if ($('remarkVeil')) $('remarkVeil').classList.add('on');
}
function closeRemarks() { if ($('remarkVeil')) $('remarkVeil').classList.remove('on'); }
function saveRemarkForm() {
  const map = {};
  document.querySelectorAll('#remarkBody input[data-uid]').forEach(inp => {
    const v = inp.value.trim();
    if (v) map[inp.getAttribute('data-uid')] = v;
  });
  storeRemarks(map);
  closeRemarks();
  // 备注只影响本机展示：重渲染当前视图即可，无需后端参与。
  if (view === 'accounts') loadOverview(true);
  else if (view === 'usage' && usageData) renderUsage(usageData);
  toast('备注已保存（仅本机浏览器）');
}
if ($('btnRemarks')) $('btnRemarks').onclick = openRemarks;
if ($('btnRemarkClose')) $('btnRemarkClose').onclick = closeRemarks;
if ($('btnRemarkClear')) $('btnRemarkClear').onclick = () => { storeRemarks({}); renderRemarkBody(); };
if ($('btnRemarkSave')) $('btnRemarkSave').onclick = saveRemarkForm;

/* ══ 通用折叠引擎 ═════════════════════════════════════════════════════════
   声明式接线：按钮写 data-fold="key"，容器写 data-fold-body="key"，键名对上即生效。
   真值源是内存里的 foldState（Map），localStorage 只是持久化副本 —— 渲染路径
   （每 5 秒的 applyFolds 重放）不能有 IO，两边只在用户点击时同步。
   ⚠ 折叠容器与组头必须是**静态 HTML**：由 innerHTML 生成的容器会被下一轮重绘
   整个抹掉，展开态随之丢失。renderAccounts 末尾统一调 applyFolds() 重放，
   重建行之后仍在的折叠区会恢复成用户上次的选择。
   按钮契约与首页「积分到期提醒」保持一致：aria-expanded + 文案「收起 / 展开」。 */
const FOLD_LS = 'wb2api.folds.v1';
function foldAll() {
  if (foldState) return foldState;
  foldState = new Map();
  try {
    const raw = JSON.parse(localStorage.getItem(FOLD_LS) || '{}');
    if (raw && typeof raw === 'object') Object.keys(raw).forEach(k => foldState.set(k, raw[k] === true));
  } catch (e) {}
  return foldState;
}
function foldSet(key, folded) {
  foldAll().set(key, !!folded);
  const o = {};
  foldState.forEach((v, k) => { o[k] = v; });
  try { localStorage.setItem(FOLD_LS, JSON.stringify(o)); } catch (e) {}
  applyFolds();
}
function applyFolds() {
  const st = foldAll();
  const bodies = new Map();
  document.querySelectorAll('[data-fold-body]').forEach(el => bodies.set(el.getAttribute('data-fold-body'), el));
  document.querySelectorAll('[data-fold]').forEach(btn => {
    const key = btn.getAttribute('data-fold');
    const folded = st.get(key) === true;
    const body = bodies.get(key);
    if (body) body.hidden = folded;
    btn.textContent = folded ? '展开' : '收起';
    btn.setAttribute('aria-expanded', folded ? 'false' : 'true');
  });
}
document.addEventListener('click', ev => {
  const btn = ev.target && ev.target.closest ? ev.target.closest('[data-fold]') : null;
  if (!btn) return;
  ev.preventDefault();
  const key = btn.getAttribute('data-fold');
  foldSet(key, foldAll().get(key) !== true);
});
applyFolds();

/* ══ 首页「积分到期提醒」折叠（选择记在本机，默认展开）═════════════════════
   展开态是原行为：点「检查」就要能立刻看到结果，所以默认不折叠；用户收起一次后
   由 localStorage 记住。折叠只作用于列表容器，卡片标题与「检查」按钮始终可见。 */
function expFolded() { return localStorage.getItem(LS_EXP_FOLD) === '1'; }
function applyExpFold() {
  const pad = $('expPad'), btn = $('btnExpFold');
  if (!pad || !btn) return;
  const folded = expFolded();
  pad.hidden = folded;
  btn.textContent = folded ? '展开' : '收起';
  btn.setAttribute('aria-expanded', folded ? 'false' : 'true');
}
if ($('btnExpFold')) {
  $('btnExpFold').onclick = () => {
    try { localStorage.setItem(LS_EXP_FOLD, expFolded() ? '0' : '1'); } catch (e) {}
    applyExpFold();
  };
}
applyExpFold();


/* ── 容量规划 ───────────────────────────────────────────────────────
   后端读取现有内存快照和脱敏请求归档；本页不触发模型调用，也不会自动调参。 */
function capNumber(v, digits = 0) {
  if (v === null || v === undefined || !Number.isFinite(Number(v))) return '—';
  return Number(v).toLocaleString('zh-CN', { minimumFractionDigits: digits, maximumFractionDigits: digits });
}
function capKpi(label, value, sub, cls = '') {
  return '<div class="kpi"><div class="lb">' + esc(label) + '</div><div class="v ' + esc(cls) + '">' +
    esc(value) + '</div><div class="sub">' + esc(sub) + '</div></div>';
}
async function loadCapacity(force = false) {
  const seq = ++capacityRequestSeq;
  const btn = $('btnCapacity');
  btn.disabled = true;
  $('capNote').textContent = '正在读取服务数据并分析…';
  try {
    const q = new URLSearchParams({ days: $('capDays').value, hours: $('capHours').value });
    if (force) q.set('refresh', '1');
    // 缓存未命中时会真的重算全池（实测 8.2s），必须挂重档超时；force 的 refresh=1
    // 走的是同一条调用，所以一处覆盖两种入口。
    const d = await api('capacity?' + q.toString(), { timeoutMs: API_HEAVY_TIMEOUT_MS });
    if (seq !== capacityRequestSeq) return; // 旧响应不得覆盖较新的筛选条件。
    capacityData = d;
    renderCapacity(d);
  } catch (e) {
    if (seq !== capacityRequestSeq) return;
    $('capNote').textContent = '分析失败：' + e.message;
    toast('容量分析失败：' + e.message, 'err');
  } finally {
    if (seq === capacityRequestSeq) btn.disabled = false;
  }
}
function renderCapacity(d) {
  const days = d.parameters && d.parameters.forecast_days || 7;
  const hours = d.parameters && d.parameters.recent_hours || 24;
  const h = d.forecast.horizons[String(days)];
  const main = h.planned_50pct_replenishment;
  const stable = h.stable_replenishment;
  const none = h.no_replenishment;
  const n = main.additional_accounts;
  $('capHero').textContent = n === null ? '积分样本或单号额度不足，无法可靠换算补号数' :
    (n > 0 ? '未来 ' + days + ' 天建议补 ' + n + ' 个号' : '未来 ' + days + ' 天无需为积分补号');
  $('capStats').innerHTML =
    capKpi('预计日耗', capNumber(d.usage.daily_credits_forecast), 'credits / day · 积分样本 ' + capNumber(d.usage.credit_sample_coverage_pct, 1) + '%', 'c-accent') +
    capKpi('当前可用余额', capNumber(d.pool.credits_remaining), d.forecast.runway_days_without_headroom === null ?
      (d.usage.credit_forecast_available ? '当前假设下不耗尽' : '暂无可靠续航') :
      '预计净续航 ' + capNumber(d.forecast.runway_days_without_headroom, 1) + ' 天') +
    capKpi('近期成功率', capNumber(d.traffic.success_rate, 2) + '%', capNumber(d.traffic.requests) + ' 个逻辑请求 / 近 ' + hours + 'h') +
    capKpi('可服务账号', capNumber(d.pool.accounts_available_estimate), '总账号 ' + d.pool.accounts_total + ' · 有余额 ' + d.pool.accounts_credit_positive) +
    capKpi('单号容量基线', capNumber(d.pool.new_account_credit_unit_p25), '启用账号总额度 P25') +
    capKpi('并发峰值估算', capNumber(d.traffic.peak_5m_in_flight_estimate), '池上限估算 ' + capNumber(d.pool.in_flight_pool_limit_estimate));
  $('capRecommendations').innerHTML = (d.recommendations || []).map(r =>
    '<article class="cap-rec ' + esc(String(r.priority).toLowerCase()) + '"><div class="cap-title"><span class="tag">' +
    esc(r.priority) + '</span>' + esc(r.title) + '</div><p>' + esc(r.action) + '</p><small>' + esc(r.evidence) + '</small></article>'
  ).join('') || '<div class="empty">当前没有建议</div>';
  const scenarioRows = [];
  Object.keys(d.forecast.horizons).sort((a, b) => Number(a) - Number(b)).forEach(k => {
    const horizon = d.forecast.horizons[k];
    [['planned_50pct_replenishment', '计划（50% 补给）'], ['stable_replenishment', '补给稳定'], ['no_replenishment', '不计补给']].forEach(([key, label]) => {
      const s = horizon[key];
      scenarioRows.push('<tr><td>' + esc(k) + ' 天</td><td>' + esc(label) + '</td><td class="num">' +
        capNumber(s.credits_shortfall) + '</td><td class="num"><b>' + capNumber(s.additional_accounts) + '</b></td><td class="num">' +
        capNumber(s.credits_usable_after_expiry) + '</td><td class="num">' + capNumber(s.credits_replenishment_assumed) + '</td></tr>');
    });
  });
  $('capForecastBody').innerHTML = scenarioRows.join('');
  $('capModelsBody').innerHTML = (d.traffic.models || []).slice(0, 30).map(m =>
    '<tr><td>' + esc(m.model) + '</td><td class="num">' + capNumber(m.requests) + '</td><td class="num">' +
    capNumber(m.success_rate, 1) + '%</td><td class="num">' + (m.recent_6h_success_rate === null ? '—' : capNumber(m.recent_6h_success_rate, 1) + '%') +
    '</td><td class="num">' + capNumber(m.credits) + '</td><td class="num">' + m.http_400 + ' / ' + m.http_429 + ' / ' + m.http_503 + '</td></tr>'
  ).join('') || '<tr><td colspan="6"><div class="empty">所选窗口没有请求归档</div></td></tr>';
  const cutoff = new Date(d.generated_at);
  const generated = Number.isNaN(cutoff.getTime()) ? String(d.generated_at || '') : cutoff.toLocaleString('zh-CN', { hour12: false });
  $('capNote').textContent = '数据截止 ' + generated + ' · 可信度 ' + d.confidence +
    (d.parameters.cached ? ' · 5 分钟缓存' : ' · 实时重算') + ' · 只读分析，无上游调用';
  $('capMethod').textContent = '日耗：' + d.method.daily_burn + '\n' +
    '计划补给：最近 24h 正向积分变动的 50%；单号额度：有效账号 credits_total 的 P25；所有情景含 20% 安全余量。\n' +
    '积分样本不全时按已观测单次尝试均值外推；净续航按各批次实际到期时间分段计算，null 表示当前假设下不耗尽或数据不足。\n' +
    '用量 q/e 是账号尝试口径；请求归档才是客户端逻辑请求与严格成功率。并发为基于已完成请求时长的 5 分钟估算，不等于实时租约峰值。\n' +
    '本次预测窗口：稳定补给 ' + capNumber(stable.additional_accounts) + ' 个；不计补给 ' + capNumber(none.additional_accounts) +
    ' 个。归档覆盖 ' + capNumber(d.coverage.request_hours / 24, 1) + ' 天；历史较短时请每日复核，不把点估计当承诺。';
}
$('btnCapacity').onclick = () => loadCapacity(true);
$('capDays').onchange = () => loadCapacity();
$('capHours').onchange = () => loadCapacity();

/* ══ 成长资产（签到活动 + 能量）══════════════════════════════════════════
   为什么独立卡片：这两组数据每个账号各打一次上游（60 号 ≈ 120 个请求、10~20 秒），
   不能进 /overview 热路径——那会拖慢每一次刷新。这里按需拉：进页面 3 秒后自动拉一次
   （每会话一次），之后靠「检查」手动刷新。
   「未签」是最有行动价值的信号：漏一天就是 100 分，而且连签档位直接断档。 */
const LS_GROW_FOLD = 'wb2api.growFold';
const GROW_AUTO_KEY = 'wb2api.growAutoAt';
const GROW_AUTO_GAP_MS = 30 * 60 * 1000;   // 同一个会话里半小时内不重复自动拉
let growFetching = false;

function growFolded() {
  // 默认**收起**：账号池是主视图，这张卡展开时空态要占 120px，把表格推到首屏外。
  // 用户展开过一次后按 localStorage 记住（没存过 → 收起）。
  const v = localStorage.getItem(LS_GROW_FOLD);
  return v === null ? true : v === '1';
}
function applyGrowFold() {
  const pad = $('growPad'), btn = $('btnGrowFold');
  if (!pad || !btn) return;
  const folded = growFolded();
  pad.hidden = folded;
  btn.textContent = folded ? '展开' : '收起';
  btn.setAttribute('aria-expanded', folded ? 'false' : 'true');
}
function growName(r) { return r.nickname || String(r.uid || '').slice(0, 8); }

function renderGrowthAssets(d) {
  const s = d.summary || {}, a = d.activity || {}, rows = d.accounts || [];
  const chips = [];
  if (a.season) chips.push('<span class="chip">S' + a.season + ' ' + esc(a.theme_name || '签到活动') + '</span>');
  if (a.end_time) chips.push('<span class="chip">截止 ' + esc(String(a.end_time).slice(0, 10)) + '</span>');
  chips.push('<span class="chip">已签 ' + s.checked_in + '/' + s.accounts + '</span>');
  if (s.missing) chips.push('<span class="chip on">未签 ' + s.missing + '</span>');
  chips.push('<span class="chip">能量 ' + (s.energy_total || 0) + '</span>');
  if (s.credits_total) chips.push('<span class="chip">活动累计 ' + s.credits_total + ' 分</span>');
  if (s.errors) chips.push('<span class="chip on">查询失败 ' + s.errors + '</span>');

  const miss = rows.filter(r => !r.err && !r.today_checked_in)
    .sort((x, y) => x.streak_days - y.streak_days);
  const low = rows.filter(r => !r.err).sort((x, y) => x.streak_days - y.streak_days).slice(0, 6);

  let html = '<div class="chips" style="flex-wrap:wrap">' + chips.join('') + '</div>';
  html += '<div class="empty" style="margin-top:8px">' + (miss.length
    ? '未签：' + miss.map(r => esc(growName(r)) + '（连签 ' + r.streak_days + ' 天）').join(' · ')
    : '今日全部已签') + '</div>';
  if (low.length) {
    html += '<div class="empty">连签最短：' + low.map(r =>
      esc(growName(r)) + ' ' + r.streak_days + ' 天/本周 ' + r.week_checkin_days + ' 天').join(' · ') + '</div>';
  }
  $('growList').innerHTML = html;
}

async function loadGrowthAssets() {
  if (growFetching || !$('growList')) return;
  growFetching = true;
  $('growNote').textContent = '查询中…';
  try {
    const d = await api('growth_assets', { timeoutMs: API_SLOW_GET_TIMEOUT_MS });
    renderGrowthAssets(d);
    $('growNote').textContent = '上次查询 ' + new Date().toLocaleTimeString('zh-CN', { hour12: false }) +
      (d.elapsed ? ' · 耗时 ' + (d.elapsed / 1000).toFixed(1) + 's' : '');
    try { sessionStorage.setItem(GROW_AUTO_KEY, String(Date.now())); } catch (e) {}
  } catch (e) {
    $('growNote').textContent = '查询失败：' + esc(e.message);
  }
  growFetching = false;
}

if ($('btnGrow')) $('btnGrow').onclick = () => loadGrowthAssets();
if ($('btnGrowFold')) {
  $('btnGrowFold').onclick = () => {
    try { localStorage.setItem(LS_GROW_FOLD, growFolded() ? '0' : '1'); } catch (e) {}
    applyGrowFold();
  };
}
applyGrowFold();
// 进页面自动拉一次：延迟 3 秒让首屏先渲染，避免和 overview 抢带宽。
setTimeout(() => {
  // 收起状态不自动拉：拉起 60 个号 = 120 次上游请求，用户根本没在看这张卡时不该花。
  if (growFolded()) return;
  let seen = 0;
  try { seen = Number(sessionStorage.getItem(GROW_AUTO_KEY) || 0); } catch (e) {}
  if (Date.now() - seen > GROW_AUTO_GAP_MS) loadGrowthAssets();
}, 3000);


/* ══ 账号池：筛选 / 排序 / 分页 / 标签 ═══════════════════════════════════
   设计约束（用户明确要求「不影响 api 调用性能」）：
     · 数据源只有 overview 的 5 秒快照，这里一行网络请求都没有；
     · 过滤/排序/分页是纯数组运算，225 个号一轮 < 1ms；
     · 标签写 localStorage，与凭证、后端状态完全解耦。

   口径纪律：状态判定统一走 accCool / accRank，不再另写一份，
   否则「汇总条说有 3 个冷却、表里只有 2 行」这种自相矛盾立刻会被当成 bug。 */

/* 单账号是否命中当前筛选（状态 + 标签 + 搜索）。 */
function accMatches(s) {
  const cool = accCool(s).cool;
  switch (accView.status) {
    case 'issue': if (!(s.disabled || cool > 0)) return false; break;
    case 'ok': if (s.disabled || s.paused || cool > 0) return false; break;
    case 'cool': if (s.disabled || !(cool > 0)) return false; break;
    case 'paused': if (!s.paused) return false; break;
    case 'off': if (!s.disabled) return false; break;
    case 'ent': if (!s.enterprise) return false; break;
    case 'global': if (s.realm !== 'global') return false; break;
    default: break;
  }
  if (accView.tags.length) {
    const t = tagsOf(s.uid);
    // 多个标签之间是「或」：选了「主力」+「备用」应该看到两拨号，不是交集。
    if (!accView.tags.some(x => t.indexOf(x) >= 0)) return false;
  }
  if (accView.q) {
    const q = accView.q.toLowerCase();
    const hay = ((s.nickname || '') + ' ' + s.uid + ' ' + (remarkOf(s.uid) || '') + ' ' +
      tagsOf(s.uid).join(' ')).toLowerCase();
    if (hay.indexOf(q) < 0) return false;
  }
  return true;
}

function accSortList(list) {
  const f = ACC_SORTS[accView.sort] || ACC_SORTS.default;
  return list.slice().sort(f);
}

function accFilterHint() {
  const parts = [];
  if (accView.status !== 'all') parts.push('状态=' + (ACC_STATUS_LABEL[accView.status] || accView.status));
  if (accView.tags.length) parts.push('标签=' + accView.tags.join('/'));
  if (accView.q) parts.push('搜索「' + accView.q + '」');
  return parts.length ? ('当前筛选：' + parts.join(' · ')) : '当前没有筛选条件';
}

function saveAccView() {
  try {
    localStorage.setItem(LS_ACC_VIEW, JSON.stringify({
      q: accView.q, status: accView.status, tags: accView.tags,
      sort: accView.sort, size: accView.size,
    }));
  } catch (e) {}
}
function saveAccTags() { try { localStorage.setItem(LS_ACC_TAGS, JSON.stringify(accTags)); } catch (e) {} }
function tagsOf(uid) { return (accTags[uid] || []).slice(); }
function setAccTags(uid, list) {
  // 去重 + 去空 + 上限 6：标签是「分类」不是「备注」，多了反而没法筛。
  const v = Array.from(new Set((list || []).map(x => String(x).trim()).filter(Boolean))).slice(0, 6);
  if (v.length) accTags[uid] = v; else delete accTags[uid];
  saveAccTags();
}
/* 全部标签及使用次数，按次数倒序（最常用的排最前，打标签时一眼可选）。 */
function allAccTags() {
  const m = {};
  Object.keys(accTags).forEach(uid => {
    (accTags[uid] || []).forEach(t => { m[t] = (m[t] || 0) + 1; });
  });
  return Object.keys(m).map(t => [t, m[t]]).sort((a, b) => (b[1] - a[1]) || a[0].localeCompare(b[0], 'zh-CN'));
}

function syncAccSearchCls() {
  const box = $('accSearch');
  if (box) box.classList.toggle('has-q', !!accView.q);
}
/* 把状态写回控件（初始化、点击 chip、清除筛选后都要同步，否则控件显示与实际不符）。 */
function syncAccControls() {
  if ($('accQ')) $('accQ').value = accView.q;
  if ($('accSort')) $('accSort').value = accView.sort;
  if ($('accSize')) $('accSize').value = String(accView.size);
  syncAccSearchCls();
}

function renderAccStatusChips(all) {
  const box = $('accStatusChips');
  if (!box) return;
  const cnt = { all: all.length, issue: 0, ok: 0, cool: 0, paused: 0, off: 0, ent: 0, global: 0 };
  all.forEach(s => {
    const cool = accCool(s).cool;
    // 计数口径与行内状态标签一致：已禁用优先（禁用的号即使还在冷却也只算禁用）。
    if (s.disabled) cnt.off++;
    else if (s.paused) cnt.paused++;
    else if (cool > 0) cnt.cool++;
    else cnt.ok++;
    if (s.disabled || cool > 0) cnt.issue++;
    if (s.enterprise) cnt.ent++;
    if (s.realm === 'global') cnt.global++;
  });
  const html = ACC_STATUS.filter(k => k === 'all' || cnt[k] > 0).map(k =>
    '<button type="button" class="xs chip' + (accView.status === k ? ' on' : '') +
    '" data-st="' + k + '">' + ACC_STATUS_LABEL[k] +
    (k === 'all' ? '' : '<span class="n">' + cnt[k] + '</span>') + '</button>').join('');
  if (box.dataset.sig !== html) { box.innerHTML = html; box.dataset.sig = html; }
}

function renderAccTagChips() {
  const row = $('accTagRow'), box = $('accTagChips');
  if (!row || !box) return;
  const list = allAccTags();
  row.hidden = !list.length;
  if (!list.length) { box.innerHTML = ''; box.dataset.sig = ''; return; }
  const html = list.map(([t, n]) =>
    '<button type="button" class="xs chip' + (accView.tags.indexOf(t) >= 0 ? ' on' : '') +
    '" data-tag="' + esc(t) + '">' + esc(t) + '<span class="n">' + n + '</span></button>').join('');
  if (box.dataset.sig !== html) { box.innerHTML = html; box.dataset.sig = html; }
}

function renderAccPager(total, pages, start, n) {
  const info = $('accPageInfo'), num = $('accPageNum');
  const prev = $('accPrev'), next = $('accNext');
  if (!info) return;
  info.textContent = total
    ? ('第 ' + (start + 1) + '–' + (start + n) + ' 条 · 共 ' + total + ' 个号')
    : '没有匹配的账号';
  if (num) num.textContent = pages > 1 ? (accView.page + ' / ' + pages) : '';
  if (prev) prev.disabled = accView.page <= 1;
  if (next) next.disabled = accView.page >= pages;
}

function syncAccChkAll(pageRows) {
  const all = $('accChkAll');
  if (!all) return;
  const n = pageRows.length;
  const sel = pageRows.filter(s => accSel.has(s.uid)).length;
  all.checked = n > 0 && sel === n;
  // indeterminate 必须显式给：只设 checked 会让「选了 3/50」看起来像「一个都没选」。
  all.indeterminate = sel > 0 && sel < n;
  all.title = '全选当前页（' + n + ' 个）';
}

function renderBulkBar() {
  const bar = $('bulkbar'), n = $('bulkN');
  if (!bar) return;
  const on = accSel.size > 0;
  bar.classList.toggle('on', on);
  // body 留白与浮层同源：只加一次类，避免每条 5 秒快照都写 style。
  document.body.classList.toggle('bulk-on', on);
  if (n) n.textContent = '已选 ' + accSel.size;
}

function refreshAccTable() {
  const accounts = (overviewData && overviewData.accounts) || [];
  renderAccounts(accounts);
  renderAlertBar(accounts);
}

function goAccPage(p) {
  const accounts = (overviewData && overviewData.accounts) || [];
  const total = accounts.filter(accMatches).length;
  const size = accView.size > 0 ? accView.size : Math.max(1, total);
  const pages = Math.max(1, Math.ceil(total / size));
  const next = Math.min(Math.max(1, p), pages);
  if (next === accView.page) return;
  accView.page = next;
  refreshAccTable();
  // 翻页后表格顶部可能已在视口外：此时才滚，避免每次翻页都强制滚动。
  const box = $('accBody') && $('accBody').closest('.box');
  if (box && box.getBoundingClientRect().top < 0) box.scrollIntoView({ block: 'start', behavior: 'smooth' });
}

/* ── 标签浮层（单账号）────────────────────────────────────────────── */
function openTagSheet(uid) {
  const list = (overviewData && overviewData.accounts) || [];
  const s = list.find(x => x.uid === uid);
  tagEditingUid = uid;
  tagDraft = tagsOf(uid);
  if ($('tagWho')) {
    $('tagWho').textContent = (s ? (s.nickname || '未命名') : uid.slice(0, 8)) + ' · ' + uid.slice(0, 8);
  }
  if ($('tagInput')) $('tagInput').value = '';
  renderTagPool();
  const sheet = $('tagSheet'), mask = $('tagMask');
  if (!sheet || !mask) return;
  sheet.hidden = false; mask.hidden = false;
  // 先显示再下一帧加 .on，transition 才有起点（同帧加类会被合并成无动画）。
  requestAnimationFrame(() => { mask.classList.add('on'); sheet.classList.add('on'); });
  setTimeout(() => { if ($('tagInput')) $('tagInput').focus(); }, 60);
}
function closeTagSheet() {
  const sheet = $('tagSheet'), mask = $('tagMask');
  if (!sheet || !mask) return;
  sheet.classList.remove('on'); mask.classList.remove('on');
  setTimeout(() => { sheet.hidden = true; mask.hidden = true; }, 240);
  tagEditingUid = null;
  tagDraft = [];
}
function renderTagPool() {
  const pool = $('tagPool');
  if (!pool) return;
  const known = allAccTags().map(x => x[0]).filter(t => tagDraft.indexOf(t) < 0).slice(0, 8);
  pool.innerHTML =
    tagDraft.map(t => '<button type="button" class="xs chip on" data-un="' + esc(t) + '">' + esc(t) + ' ✕</button>').join('') +
    known.map(t => '<button type="button" class="xs chip" data-addtag="' + esc(t) + '">+ ' + esc(t) + '</button>').join('');
}
function addTagDraft(v) {
  const t = String(v || '').trim();
  if (!t) return;
  if (tagDraft.indexOf(t) < 0 && tagDraft.length < 6) tagDraft.push(t);
  renderTagPool();
}
function saveTagSheet() {
  if (!tagEditingUid) return;
  setAccTags(tagEditingUid, tagDraft);
  closeTagSheet();
  refreshAccTable();
  toast('标签已保存（只存本机）', 'ok');
}

/* ── 事件绑定 ─────────────────────────────────────────────────────── */
let accQTimer = 0;
if ($('accQ')) {
  // 防抖 140ms：225 个号过滤一次不到 1ms，防抖只是为了不在输入法组词时反复重排表格。
  $('accQ').addEventListener('input', ev => {
    const v = ev.target.value || '';
    clearTimeout(accQTimer);
    accQTimer = setTimeout(() => {
      accView.q = v.trim();
      accView.page = 1;
      syncAccSearchCls();
      saveAccView();
      refreshAccTable();
    }, 140);
  });
}
if ($('accQClear')) {
  $('accQClear').onclick = () => {
    accView.q = ''; accView.page = 1;
    if ($('accQ')) $('accQ').value = '';
    syncAccSearchCls(); saveAccView(); refreshAccTable();
  };
}
if ($('accStatusChips')) {
  $('accStatusChips').addEventListener('click', ev => {
    const b = ev.target.closest('[data-st]');
    if (!b) return;
    const k = b.getAttribute('data-st');
    accView.status = ACC_STATUS.indexOf(k) >= 0 ? k : 'all';
    accFilter = (accView.status === 'cool' || accView.status === 'off') ? accView.status : 'all';
    try { localStorage.setItem(LS_ACC_FILTER, accFilter); } catch (e) {}
    accView.page = 1;
    saveAccView(); refreshAccTable();
  });
}
if ($('accTagChips')) {
  $('accTagChips').addEventListener('click', ev => {
    const b = ev.target.closest('[data-tag]');
    if (!b) return;
    const t = b.getAttribute('data-tag');
    const i = accView.tags.indexOf(t);
    if (i >= 0) accView.tags.splice(i, 1); else accView.tags.push(t);
    accView.page = 1;
    saveAccView(); refreshAccTable();
  });
}
if ($('accSort')) {
  $('accSort').onchange = ev => {
    const v = ev.target.value;
    accView.sort = ACC_SORTS[v] ? v : 'default';
    accView.page = 1;
    saveAccView(); refreshAccTable();
  };
}
if ($('accSize')) {
  $('accSize').onchange = ev => {
    const n = Number(ev.target.value);
    accView.size = [0, 20, 50, 100, 200].indexOf(n) >= 0 ? n : 50;
    accView.page = 1;
    saveAccView(); refreshAccTable();
  };
}
if ($('accPrev')) $('accPrev').onclick = () => goAccPage(accView.page - 1);
if ($('accNext')) $('accNext').onclick = () => goAccPage(accView.page + 1);

if ($('accChkAll')) {
  $('accChkAll').onchange = ev => {
    const accounts = (overviewData && overviewData.accounts) || [];
    const ranked = accSortList(accounts.filter(accMatches));
    const size = accView.size > 0 ? accView.size : Math.max(1, ranked.length);
    const start = (accView.page - 1) * size;
    const pageRows = ranked.slice(start, start + size);
    if (ev.target.checked) pageRows.forEach(s => accSel.add(s.uid));
    else pageRows.forEach(s => accSel.delete(s.uid));
    refreshAccTable();
  };
}
// 行内复选框：**不手动切 class**，交给 renderAccounts 的 keyed 增量。
// 手动 toggle 会让 DOM 与「行签名缓存」不一致：勾选时手动加 .sel，清除选择时
// 签名回到旧值（与缓存相同）→ 那一行不会被重建 → 高亮永远擦不掉（实测踩到）。
// 签名里含 accSel，所以重渲染只会替换**变化的那一行**，不是整表。
$('accBody').addEventListener('change', ev => {
  const box = ev.target.closest('[data-chk]');
  if (!box) return;
  const uid = box.getAttribute('data-chk');
  if (box.checked) accSel.add(uid); else accSel.delete(uid);
  renderAccounts((overviewData && overviewData.accounts) || []);
});
// 双击行编辑标签：不占操作列宽度，也不影响行内其它按钮的点击。
$('accBody').addEventListener('dblclick', ev => {
  const tr = ev.target.closest('tr[data-uid]');
  if (!tr) return;
  const uid = tr.getAttribute('data-uid');
  if (!uid || uid === ACC_SEP_KEY) return;
  openTagSheet(uid);
});

if ($('bulkApply')) {
  $('bulkApply').onclick = () => {
    const v = $('bulkTag') ? $('bulkTag').value.trim() : '';
    if (!v) { toast('先输入标签名', 'warn'); return; }
    accSel.forEach(uid => setAccTags(uid, tagsOf(uid).concat([v])));
    if ($('bulkTag')) $('bulkTag').value = '';
    refreshAccTable();
    toast('已给 ' + accSel.size + ' 个号加上「' + v + '」', 'ok');
  };
}
if ($('bulkTag')) {
  $('bulkTag').addEventListener('keydown', ev => {
    if (ev.key === 'Enter' && $('bulkApply')) $('bulkApply').click();
  });
}
if ($('bulkClear')) {
  $('bulkClear').onclick = () => { accSel.clear(); refreshAccTable(); };
}

if ($('tagAdd')) $('tagAdd').onclick = () => { addTagDraft($('tagInput').value); if ($('tagInput')) $('tagInput').value = ''; };
if ($('tagInput')) {
  $('tagInput').addEventListener('keydown', ev => {
    if (ev.key === 'Enter') { ev.preventDefault(); addTagDraft(ev.target.value); ev.target.value = ''; }
  });
}
if ($('tagPool')) {
  $('tagPool').addEventListener('click', ev => {
    const un = ev.target.closest('[data-un]');
    if (un) {
      const t = un.getAttribute('data-un');
      tagDraft = tagDraft.filter(x => x !== t);
      renderTagPool();
      return;
    }
    const add = ev.target.closest('[data-addtag]');
    if (add) addTagDraft(add.getAttribute('data-addtag'));
  });
}
if ($('tagSave')) $('tagSave').onclick = saveTagSheet;
if ($('tagCancel')) $('tagCancel').onclick = closeTagSheet;
if ($('tagMask')) $('tagMask').onclick = closeTagSheet;
document.addEventListener('keydown', ev => {
  if (ev.key === 'Escape' && tagEditingUid) closeTagSheet();
});

syncAccControls();
