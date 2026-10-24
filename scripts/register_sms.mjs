#!/usr/bin/env node
/**
 * scripts/register_sms.mjs — 手机号 + 短信验证码 → WorkBuddy OAuth 登录 → 面板导入
 *
 * 把 2026-10-24 手工跑通并踩坑过的流程固化成一条命令：
 *   取 state → 浏览器填 +852/+86 号码 → 发码 → 轮询接码平台取「新」码
 *   → 60 秒窗口内提交 → poll token → POST /panel/api/import/cockpit 导入
 *
 * 用法：
 *   node scripts/register_sms.mjs --phone 70361951 \
 *        --sms "http://217api.com:9900/api/sms/getcode?token=xxx"
 *   # 常用开关：--area +852（默认） --realm cn（默认） --panel <url>
 *   #           --api-key sk-xxx（或 env WB2API_API_KEY） --no-import --json --headful
 *
 * 退出码（上层编排据此判断，不用解析日志）：
 *   0  成功            10 SMS_TIMEOUT        11 CODE_REJECTED
 *   12 ACCOUNT_RESTRICTED（腾讯安全侧拒绝，换号）  13 LOGIN_TIMEOUT
 *   14 POLL_FAILED     15 IMPORT_FAILED      16 BLOCKED_UNKNOWN
 *   2  用法错误        1  其它
 *
 * 三个必须遵守的上游事实（都是实测踩出来的，别改）：
 *   1. 验证码真实有效期 60 秒（短信文案写「5 分钟」是腾讯统一身份的通用话术）。
 *   2. 接码平台只返回「最后一条」短信、不区分新旧 ⇒ 必须先读基线，只接受变化后的新码，
 *      否则会拿上一条过期码去提交（E0010028）。
 *   3. 「发码 → 取码 → 提交」必须在同一个浏览器会话内完成，重开页面必然超时。
 */

import puppeteer from 'puppeteer-core';

/* ─────────────────────────── 参数 ─────────────────────────── */

function parseArgs(argv) {
  const a = {
    area: '+852',
    realm: 'cn',
    panel: 'https://workbuddy2api.1task.cn',
    timeout: 240,
    json: false,
    headful: false,
    import: true,
    chrome: process.env.CHROME_PATH || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
  };
  for (let i = 0; i < argv.length; i++) {
    const k = argv[i];
    const next = () => argv[++i];
    switch (k) {
      case '--phone': a.phone = next(); break;
      case '--sms': case '--sms-url': a.smsUrl = next(); break;
      case '--area': a.area = next(); break;
      case '--realm': a.realm = next(); break;
      case '--panel': a.panel = next(); break;
      case '--api-key': a.apiKey = next(); break;
      case '--chrome': a.chrome = next(); break;
      case '--timeout': a.timeout = parseInt(next(), 10); break;
      case '--no-import': a.import = false; break;
      case '--json': a.json = true; break;
      case '--headful': a.headful = true; break;
      case '-h': case '--help': a.help = true; break;
      default: throw new UsageError(`未知参数 ${k}`);
    }
  }
  if (!a.apiKey) a.apiKey = process.env.WB2API_API_KEY || '';
  return a;
}

class UsageError extends Error {}
class StageError extends Error {
  constructor(code, message, extra = {}) {
    super(message);
    this.code = code;
    this.extra = extra;
  }
}

/* ─────────────────────────── HTTP ─────────────────────────── */

const REALM_BASE = { cn: 'https://copilot.tencent.com', global: 'https://www.workbuddy.ai' };
const CLI_UA = 'CLI/2.63.2 CodeBuddy/2.63.2';

function upstreamHeaders(realm, extra = {}) {
  const origin = REALM_BASE[realm] || REALM_BASE.cn;
  return {
    'Content-Type': 'application/json',
    Accept: 'application/json, text/plain, */*',
    'X-Requested-With': 'XMLHttpRequest',
    Origin: origin,
    Referer: origin + '/',
    'User-Agent': CLI_UA,
    ...extra,
  };
}

/** 上游统一 {code,msg,data} 信封；code!=0 抛错（带 code，便于上层区分 pending/失败）。 */
async function upstreamJSON(url, { method = 'GET', realm = 'cn', token, timeoutMs = 20000 } = {}) {
  const ctl = new AbortController();
  const t = setTimeout(() => ctl.abort(), timeoutMs);
  try {
    const res = await fetch(url, {
      method,
      headers: upstreamHeaders(realm, token ? { Authorization: 'Bearer ' + token } : {}),
      body: method === 'POST' ? '{}' : undefined,
      signal: ctl.signal,
    });
    const raw = await res.text();
    let j;
    try { j = JSON.parse(raw); } catch { throw new StageError('STATE_FAILED', `非 JSON 响应 http=${res.status} body=${raw.slice(0, 120)}`); }
    return { status: res.status, body: j };
  } finally {
    clearTimeout(t);
  }
}

async function fetchAuthState(realm) {
  const base = REALM_BASE[realm] || REALM_BASE.cn;
  const { body } = await upstreamJSON(`${base}/v2/plugin/auth/state?platform=CLI`, { method: 'POST', realm });
  if (body.code !== 0 || !body.data?.state || !body.data?.authUrl) {
    throw new StageError('STATE_FAILED', `auth state 失败: ${JSON.stringify(body).slice(0, 160)}`);
  }
  return body.data; // { state, authUrl }
}

/** poll 阶段业务码非 0 = 登录还没完成（"login ing"），要继续等。 */
async function pollToken(realm, state, tries = 14, gapMs = 1500) {
  const base = REALM_BASE[realm] || REALM_BASE.cn;
  let last = '';
  for (let i = 0; i < tries; i++) {
    try {
      const { body } = await upstreamJSON(`${base}/v2/plugin/auth/token?state=${encodeURIComponent(state)}`, { realm });
      const d = body.data || body;
      if (body.code === 0 && d.accessToken) return d;
      last = JSON.stringify(body).slice(0, 140);
    } catch (e) { last = e.message; }
    await sleep(gapMs);
  }
  throw new StageError('POLL_FAILED', `拿不到 token（登录可能没真正完成）：${last}`);
}

async function fetchAccount(realm, state, token) {
  const base = REALM_BASE[realm] || REALM_BASE.cn;
  try {
    const { body } = await upstreamJSON(`${base}/v2/plugin/login/account?state=${encodeURIComponent(state)}`, { realm, token });
    return body.data || {};
  } catch { return {}; }
}

/* ─────────────────────────── 接码平台 ─────────────────────────── */

async function readSms(url, timeoutMs = 8000) {
  const ctl = new AbortController();
  const t = setTimeout(() => ctl.abort(), timeoutMs);
  try {
    const res = await fetch(url, { signal: ctl.signal });
    return (await res.text()).trim();
  } catch { return ''; } finally { clearTimeout(t); }
}

const sixDigits = (s) => { const m = (s || '').match(/(\d{6})/); return m ? m[1] : null; };

/**
 * 只认「变化后的新码」：先记基线，发码后轮询到内容变化才返回。
 * 60 秒窗口内拿不到就返回 null（调用方报 SMS_TIMEOUT）——绝不回退用旧码。
 */
async function waitNewCode(smsUrl, baselineRaw, baselineCode, maxSec = 55) {
  const t0 = Date.now();
  while ((Date.now() - t0) / 1000 < maxSec) {
    await sleep(1000);
    const raw = await readSms(smsUrl);
    if (!raw || /^0\|0$/.test(raw)) continue;
    const code = sixDigits(raw);
    if (!code) continue;
    const changed = raw !== baselineRaw;
    const isNew = baselineCode ? code !== baselineCode : true;
    if (changed && isNew) return { code, raw };
  }
  return null;
}

/* ─────────────────────────── 浏览器登录 ─────────────────────────── */

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function switchToPhoneForm(page) {
  for (let i = 0; i < 8; i++) {
    if (await page.$('input.oneid-react-input__inner')) return true;
    await page.evaluate(() => {
      const c = [...document.querySelectorAll('span')].filter((e) => (e.innerText || '').trim() === '手机号');
      if (c.length) c[c.length - 1].click();
    });
    await sleep(1200);
  }
  return !!(await page.$('input.oneid-react-input__inner'));
}

async function pickArea(page, area) {
  const box = await (await page.$('input.oneid-react-input__inner')).boundingBox();
  await page.mouse.click(box.x + box.width / 2, box.y + box.height / 2);
  await sleep(900);
  const ok = await page.evaluate((title) => {
    const o = [...document.querySelectorAll('li.oneid-react-select-option span')]
      .find((s) => (s.getAttribute('title') || '') === title);
    if (!o) return false;
    o.click();
    return true;
  }, area);
  await sleep(500);
  return ok;
}

/**
 * 完整登录。返回 {status:'OK'} 或抛 StageError：
 *   CODE_REJECTED / ACCOUNT_RESTRICTED / LOGIN_TIMEOUT / SMS_TIMEOUT / BLOCKED_UNKNOWN
 */
async function browserLogin({ authUrl, area, phone, smsUrl, headful, chrome, log }) {
  const browser = await puppeteer.launch({
    executablePath: chrome,
    headless: headful ? false : 'new',
    // --no-proxy-server 必须有：会话里的 HTTP_PROXY 会把 oauth2.account.tencent.com 拦掉
    args: ['--no-proxy-server', '--no-sandbox', '--disable-dev-shm-usage', '--lang=zh-CN'],
    defaultViewport: { width: 1280, height: 900 },
  });
  try {
    const page = await browser.newPage();
    let verifyStatus = '';
    page.on('response', async (r) => {
      if (r.url().includes('/v1/auth/sms/code/verify')) {
        try { verifyStatus = r.status() + ' ' + (await r.text()).slice(0, 120); } catch {}
      }
    });

    await page.goto(authUrl, { waitUntil: 'domcontentloaded', timeout: 60000 }).catch(() => {});
    await sleep(5500);

    if (!(await switchToPhoneForm(page))) {
      throw new StageError('BLOCKED_UNKNOWN', '登录页没出现手机号表单（页面结构变化或地域拦截）', {
        text: (await page.evaluate(() => document.body.innerText.slice(0, 200))).replace(/\s+/g, ' '),
      });
    }
    if (!(await pickArea(page, area))) {
      throw new StageError('BLOCKED_UNKNOWN', `区号下拉里找不到 ${area}`);
    }

    const inputs = await page.$$('input.oneid-react-input__inner');
    await inputs[1].click({ clickCount: 3 });
    await page.keyboard.press('Backspace');
    await inputs[1].type(phone, { delay: 45 });
    await page.evaluate(() => {
      const cb = document.querySelector('input.t-checkbox__former');
      if (cb && !cb.checked) (cb.closest('label') || cb.parentElement).click();
    });
    log(`填表完成：${area} ${phone}`);

    // 冷却未结束时等它归零，否则点不动
    const label = await page.evaluate(() => {
      const b = [...document.querySelectorAll('button')].find((e) => /获取验证码|重新获取/.test(e.innerText || ''));
      return b ? b.innerText.trim() : '';
    });
    const cd = label.match(/\((\d+)\)/);
    if (cd) { const w = parseInt(cd[1], 10) + 1; log(`发送冷却中，等 ${w}s`); await sleep(w * 1000); }

    // 基线：发码前平台上的最后一条（可能是上一条已过期码）
    const baselineRaw = await readSms(smsUrl);
    const baselineCode = sixDigits(baselineRaw);
    log(`平台基线：${baselineCode || '(空)'}`);

    const clicked = await page.evaluate(() => {
      const b = [...document.querySelectorAll('button')].find((e) => /获取验证码|重新获取/.test(e.innerText || ''));
      if (!b) return 'no-btn';
      if (b.disabled) return 'disabled';
      b.click();
      return 'ok';
    });
    if (clicked !== 'ok') throw new StageError('BLOCKED_UNKNOWN', `发码按钮不可点（${clicked}）`);
    log('已点发送验证码，等待新码…');

    const got = await waitNewCode(smsUrl, baselineRaw, baselineCode, 55);
    if (!got) throw new StageError('SMS_TIMEOUT', '55 秒内接码平台没出现新验证码（发送可能失败，或平台延迟）');
    log(`收到新码 ${got.code}`);

    // 立刻提交（60 秒窗口）
    const ins2 = await page.$$('input.oneid-react-input__inner');
    await ins2[2].click();
    await page.keyboard.press('Backspace');
    await ins2[2].type(got.code, { delay: 40 });
    await page.evaluate(() => {
      const b = [...document.querySelectorAll('button')].find((e) => (e.innerText || '').includes('手机号登录'));
      if (b) b.click();
    });
    log('已提交验证码');

    // 等跳转 / 判定
    let text = '';
    for (let i = 0; i < 14; i++) {
      await sleep(2000);
      text = (await page.evaluate(() => document.body.innerText.slice(0, 400))).replace(/\s+/g, ' ');
      if (!/\/login\/?\?/.test(page.url())) break;
      if (/验证码错误|请获取并输入验证码/.test(text)) break;
      if (/访问受限|安全策略限制/.test(text)) break;
    }
    const url = page.url();

    if (/访问受限|安全策略限制/.test(text)) {
      throw new StageError('ACCOUNT_RESTRICTED',
        '腾讯安全侧拒绝：账号访问受限（该手机号/OneID 账号被标记，换号，别重试）',
        { url: url.slice(0, 140), verify: verifyStatus, text });
    }
    if (/验证码错误|请获取并输入验证码/.test(text)) {
      throw new StageError('CODE_REJECTED', `验证码被拒（verify=${verifyStatus || '无响应'}）`, { text });
    }
    if (/\/login\/?\?/.test(url)) {
      throw new StageError('LOGIN_TIMEOUT', '提交后 28 秒仍未跳转', { url: url.slice(0, 140), text });
    }
    log(`登录跳转：${url.slice(0, 120)}`);
    return { url, text };
  } finally {
    await browser.close();
  }
}

/* ─────────────────────────── 面板导入 ─────────────────────────── */

function buildCockpitPayload({ uid, nickname, email, accessToken, refreshToken, expiresIn, domain }) {
  // 面板只认 snake_case；expires_at 必须是**毫秒**（填秒会写成 1970）
  return [{
    id: uid,
    uid,
    nickname: nickname || '',
    email: email || '',
    access_token: accessToken,
    refresh_token: refreshToken,
    token_type: 'Bearer',
    expires_at: Date.now() + Number(expiresIn || 0) * 1000,
    domain: domain || '',
    status: 'active',
  }];
}

async function importToPanel({ panel, apiKey, payload }) {
  if (!apiKey) throw new StageError('IMPORT_FAILED', '缺 api_key（--api-key 或 WB2API_API_KEY）');
  const fd = new FormData();
  fd.append('file', new Blob([JSON.stringify(payload)], { type: 'application/json' }), 'account.json');
  const res = await fetch(`${panel.replace(/\/$/, '')}/panel/api/import/cockpit`, {
    method: 'POST',
    headers: { Authorization: 'Bearer ' + apiKey },
    body: fd,
  });
  const text = await res.text();
  let j;
  try { j = JSON.parse(text); } catch { throw new StageError('IMPORT_FAILED', `导入响应非 JSON http=${res.status}: ${text.slice(0, 150)}`); }
  if (!res.ok || j.ok === false) throw new StageError('IMPORT_FAILED', `导入失败 http=${res.status}: ${text.slice(0, 200)}`, { body: j });
  return j;
}

/* ─────────────────────────── 主流程 ─────────────────────────── */

async function main() {
  const started = Date.now();
  let args;
  try { args = parseArgs(process.argv.slice(2)); } catch (e) { console.error(e.message); process.exit(2); }
  if (args.help || !args.phone || !args.smsUrl) {
    console.log(`用法: node scripts/register_sms.mjs --phone 70361951 --sms <接码接口URL> [选项]

  --area    区号，默认 +852（可选 +86 / +853）
  --realm   cn（默认）| global
  --panel   面板地址，默认 https://workbuddy2api.1task.cn
  --api-key 面板 api_key（或 env WB2API_API_KEY）
  --no-import  只登录拿 token，不导入
  --json    机器可读输出
  --headful 显示浏览器（调试）
  --timeout 总超时秒数，默认 240`);
    process.exit(args.help ? 0 : 2);
  }

  // 总超时兜底：任一步卡死时不要让上层无限等（unref 保证正常结束时进程能退出）
  const watchdog = setTimeout(() => {
    const msg = `总超时 ${args.timeout}s（卡在某个阶段，建议 --headful 复跑观察）`;
    if (args.json) console.log(JSON.stringify({ status: 'TIMEOUT', ok: false, error: msg }));
    else console.error(`❌ [TIMEOUT] ${msg}`);
    process.exit(9);
  }, Math.max(30, args.timeout) * 1000);
  watchdog.unref();

  const log = (m) => { if (!args.json) console.error(`[${new Date().toTimeString().slice(0, 8)}] ${m}`); };
  const done = (obj, code) => {
    // 统一出口：--json 输出机器可读，否则人读摘要
    if (args.json) console.log(JSON.stringify({ ...obj, elapsed_sec: +((Date.now() - started) / 1000).toFixed(1) }, null, 1));
    else if (code === 0) console.log(`✅ ${obj.msg || '完成'}`);
    else console.error(`❌ [${obj.status}] ${obj.error}`);
    process.exit(code);
  };

  try {
    log(`[1/5] 取授权链接（realm=${args.realm}）`);
    const { state, authUrl } = await fetchAuthState(args.realm);
    log(`state=${state}`);

    log('[2/5] 浏览器登录');
    await browserLogin({ authUrl, area: args.area, phone: args.phone, smsUrl: args.smsUrl, headful: args.headful, chrome: args.chrome, log });

    log('[3/5] poll token');
    const tok = await pollToken(args.realm, state);
    const acct = await fetchAccount(args.realm, state, tok.accessToken);
    log(`拿到 token：uid=${(acct.uid || '?').slice(0, 8)} nickname=${acct.nickname || '?'}`);

    const payload = buildCockpitPayload({
      uid: acct.uid || '', nickname: acct.nickname, email: acct.email,
      accessToken: tok.accessToken, refreshToken: tok.refreshToken,
      expiresIn: tok.expiresIn, domain: tok.domain,
    });

    let imported = null;
    if (args.import) {
      log('[4/5] 导入面板');
      imported = await importToPanel({ panel: args.panel, apiKey: args.apiKey, payload });
    } else {
      log('[4/5] 跳过导入（--no-import）');
    }

    done({
      status: 'OK',
      ok: true,
      uid: acct.uid, nickname: acct.nickname,
      expires_at: payload[0].expires_at, expires_in: tok.expiresIn,
      domain: tok.domain, realm: args.realm,
      imported, payload_rel: 'masked',
      msg: `导入成功 uid=${(acct.uid || '').slice(0, 8)} credits 由面板签到后自动刷新`,
    }, 0);
  } catch (e) {
    const code = e instanceof StageError ? e.code : 'UNEXPECTED';
    const exitMap = {
      SMS_TIMEOUT: 10, CODE_REJECTED: 11, ACCOUNT_RESTRICTED: 12,
      LOGIN_TIMEOUT: 13, POLL_FAILED: 14, IMPORT_FAILED: 15, BLOCKED_UNKNOWN: 16,
    };
    done({ status: code, ok: false, error: e.message, extra: e.extra || {} }, exitMap[code] ?? 1);
  }
}

main();
