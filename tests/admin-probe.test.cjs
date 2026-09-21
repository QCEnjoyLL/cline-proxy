const { test } = require('node:test');
const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const vm = require('node:vm');
const html = readFileSync(require('node:path').join(__dirname, '../cmd/cline-proxy/web/admin.html'), 'utf8');

// Execute the shipped functions, with network and DOM boundaries mocked.
function source(name) {
  const start = html.search(new RegExp('(?:async )?function ' + name + '\\('));
  assert.ok(start >= 0, name + ' exists');
  const end = html.indexOf('\n}', start);
  return html.slice(start, end + 2);
}
function runtime(names, overrides = {}) {
  const ctx = vm.createContext({ URL, AbortController, setTimeout, clearTimeout,
    API: '/admin/api', window: { location: { href: 'http://localhost/admin' } }, ...overrides });
  for (const name of names) vm.runInContext(source(name), ctx);
  return ctx;
}
function reply(status, body, extra = {}) {
  return { status, ok: status >= 200 && status < 300, text: async () => body, ...extra };
}

test('HTML gateway errors keep HTTP status and hide HTML/parser noise', async () => {
  for (const status of [200, 403, 502, 503, 504, 524]) {
    const ctx = runtime(['api'], { fetch: async () => reply(status, '<!DOCTYPE html><h1>secret proxy page</h1>') });
    await assert.rejects(ctx.api('POST', '/upstreams/probe', {}), err => {
      assert.match(err.message, new RegExp('HTTP ' + status));
      assert.doesNotMatch(err.message, /Unexpected token|DOCTYPE|secret proxy page/);
      return true;
    });
  }
});

test('expired sessions redirect with a readable error', async () => {
  for (const response of [reply(401, ''), reply(200, '<html>', { redirected: true, url: 'http://localhost/admin/login' })]) {
    const ctx = runtime(['api'], { fetch: async () => response });
    await assert.rejects(ctx.api('GET', '/stats'), /登录已过期/);
    assert.equal(ctx.window.location.href, '/admin/login');
  }
});

test('API preserves JSON errors and rejects malformed envelopes', async () => {
  const ctx = runtime(['api'], { fetch: async () => reply(200, '{"success":true,"data":{"ok":1}}') });
  assert.equal((await ctx.api('GET', '/stats')).data.ok, 1);
  ctx.fetch = async () => reply(502, '{"success":false,"error":"no active accounts available"}');
  await assert.rejects(ctx.api('GET', '/stats'), /no active accounts/);
  for (const body of ['null', '[]', 'true', '{}']) {
    ctx.fetch = async () => reply(200, body);
    await assert.rejects(ctx.api('GET', '/stats'), /格式不正确|请求失败/);
  }
});

test('bounded requests abort and explain timeout', async () => {
  const ctx = runtime(['api'], { fetch: (_, { signal }) => new Promise((_, reject) => {
    signal.addEventListener('abort', () => reject(Object.assign(new Error(), { name: 'AbortError' })), { once: true });
  }) });
  await assert.rejects(ctx.api('GET', '/stats', null, 5), /请求超时/);
});

test('probe starts asynchronously, polls, and returns warnings with results', async () => {
  const calls = [];
  const responses = [{ jobId: 'job/1' }, { status: 'running' }, { status: 'done', result: { pipeline: 'direct', note: 'partial' } }];
  const ctx = runtime(['runUpstreamProbe'], {
    api: async (...args) => { calls.push(args); return { data: responses.shift() }; },
    setTimeout: fn => { fn(); },
  });
  const result = await ctx.runUpstreamProbe('vendor/model');
  assert.equal(result.note, 'partial');
  assert.equal(calls[0][0], 'POST');
  assert.equal(calls[0][2].async, true);
  assert.equal(calls[0][3], 15000);
  assert.equal(calls[1][1], '/upstreams/probe?jobId=job%2F1');
  assert.equal(calls.length, 3);
});

test('probe reports failed jobs and malformed status payloads', async () => {
  for (const data of [{ status: 'failed', error: 'mock upstream error' }, null, { status: 'unknown' }, { status: 'done' }]) {
    let count = 0;
    const ctx = runtime(['runUpstreamProbe'], { api: async () => ({ data: count++ ? data : { jobId: 'job1' } }) });
    await assert.rejects(ctx.runUpstreamProbe('model'), /mock upstream error|格式不正确/);
  }
});

function modalRuntime(runUpstreamProbe) {
  const nodes = {
    upstreamProbeButton: { disabled: false }, upstreamProbeNotice: { textContent: '' },
    upstreamModalBody: { innerHTML: '<form>preserved</form>' },
    upstreamModal: { classList: { remove() {} } },
    upPinMode: { value: 'strict' }, upRedirect: { value: 'initial' }, upAliases: { value: 'alias' },
  };
  const pins = [{ value: 'provider', checked: true }];
  let opens = 0;
  const ctx = runtime(['probeFromModal', 'closeUpstreamModal', 'readUpstreamForm', 'applyUpstreamForm'], {
    _: id => nodes[id], document: { querySelectorAll: () => pins },
    upstreamEditing: 'model', upstreamModalGeneration: 1,
    runUpstreamProbe, loadUpstreams: async () => {},
    openUpstreamModal: async () => {
      opens++; ctx.upstreamModalGeneration++;
      nodes.upRedirect.value = 'saved'; pins[0].checked = true;
    },
  });
  return { ctx, nodes, pins, opens: () => opens };
}

test('failed modal probe preserves controls and enables retry', async () => {
  const { ctx, nodes, opens } = modalRuntime(async () => { throw new Error('HTTP 504'); });
  await ctx.probeFromModal();
  assert.equal(nodes.upstreamModalBody.innerHTML, '<form>preserved</form>');
  assert.equal(nodes.upRedirect.value, 'initial');
  assert.equal(nodes.upstreamProbeButton.disabled, false);
  assert.match(nodes.upstreamProbeNotice.textContent, /HTTP 504.*配置已保留/);
  assert.equal(opens(), 0);
});

test('edits during a probe survive result rendering', async () => {
  let finish;
  const { ctx, nodes, pins, opens } = modalRuntime(() => new Promise(resolve => { finish = resolve; }));
  const pending = ctx.probeFromModal();
  assert.equal(nodes.upstreamProbeButton.disabled, true);
  nodes.upRedirect.value = 'edited while probing'; pins[0].checked = false;
  finish({ note: 'enumeration warning' });
  await pending;
  assert.equal(opens(), 1);
  assert.equal(nodes.upRedirect.value, 'edited while probing');
  assert.equal(pins[0].checked, false);
  assert.equal(nodes.upstreamProbeNotice.textContent, 'enumeration warning');
});

test('closing or switching the modal prevents a late result from reopening it', async () => {
  for (const switchModel of [false, true]) {
    let finish;
    const { ctx, opens } = modalRuntime(() => new Promise(resolve => { finish = resolve; }));
    const pending = ctx.probeFromModal();
    ctx.closeUpstreamModal();
    if (switchModel) ctx.upstreamEditing = 'other/model';
    finish({}); await pending;
    assert.equal(opens(), 0);
  }
});

test('embedded admin script parses', () => {
  const scripts = [...html.matchAll(/<script\b[^>]*>([\s\S]*?)<\/script>/g)];
  assert.ok(scripts.length);
  for (const script of scripts) new vm.Script(script[1]);
});

test('modal distinguishes metadata candidates from enumerated channels', async () => {
  const nodes = new Proxy({}, { get(target, id) {
    return target[id] ||= { innerHTML: '', value: '', classList: { add() {} } };
  } });
  const ctx = runtime(['openUpstreamModal'], {
    _: id => nodes[id], esc: s => s, attr: s => s,
    upstreamModalGeneration: 0, upstreamEditing: null,
    upstreamEntries: [{ modelId: 'model', upstreams: ['deepseek'], observed: ['deepseek', 'alibaba'], lastProvider: 'deepseek' }],
  });
  await ctx.openUpstreamModal('model');
  assert.match(nodes.upstreamModalBody.innerHTML, /value="deepseek" checked/);
  assert.match(nodes.upstreamModalBody.innerHTML, /最近命中，未验证可严格钉住/);
  assert.match(nodes.upstreamModalBody.innerHTML, /元数据候选，未验证可严格钉住/);
  assert.doesNotMatch(nodes.upstreamModalBody.innerHTML, /未探测到/);
});

test('availability button uses the check endpoint and never adds or copies a model', async () => {
  let checks = 0;
  const card = { dataset: { id: 'vendor/new', installed: '1' } };
  const ctx = runtime(['onModelCardClick', 'onModelCardKey'], {
    checkLibraryModel: async id => { assert.equal(id, 'vendor/new'); checks++; },
    addModels: () => assert.fail('check must not add a model'),
    copyText: () => assert.fail('check must not copy'),
  });
  const target = { closest: selector => selector === '.mcard' ? card : selector === 'button' ? {} : { dataset: { act: 'check' } } };
  await ctx.onModelCardKey({ key: 'Enter', target, preventDefault: () => assert.fail('native button key should not be intercepted') });
  await ctx.onModelCardClick({ target });
  assert.equal(checks, 1);
});

test('availability check deduplicates clicks and synchronizes duplicate cards', async () => {
  const cards = Array.from({length: 2}, () => {
    const button = {}, result = {};
    return { dataset: {id: 'model'}, button, result, querySelector: selector => selector === '.mcheck-result' ? result : button };
  });
  let finish, requests = 0;
  const ctx = runtime(['modelCheckState', 'syncModelChecks', 'checkLibraryModel'], {
    mChecks: new Map(), document: { querySelectorAll: () => cards },
    runUpstreamProbe: (id, path) => { requests++; assert.equal(path, '/models/check'); return new Promise(resolve => { finish = resolve; }); },
  });
  const pending = ctx.checkLibraryModel('model');
  await ctx.checkLibraryModel('model');
  assert.equal(requests, 1);
  for (const card of cards) assert.equal(card.button.disabled, true);
  finish({latencyMs: 1200}); await pending;
  for (const card of cards) {
    assert.equal(card.button.disabled, false);
    assert.match(card.result.textContent, /本次可用 · 1.2 秒/);
  }
  ctx.runUpstreamProbe = async () => { throw new Error('HTTP 429'); };
  await ctx.checkLibraryModel('model');
  assert.match(cards[0].result.textContent, /检测失败：HTTP 429/);
  assert.equal(cards[0].button.textContent, '重新检测');
});

test('model cards show a check button before installation and escape failure messages', () => {
  const escape = s => String(s).replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('"', '&quot;');
  const ctx = runtime(['modelCheckState', 'modelCardsHTML'], {
    mChecks: new Map([['model', {error: '<img src=x onerror=alert(1)>'}]]), esc: escape, attr: escape,
  });
  const markup = ctx.modelCardsHTML([{id: 'model'}], new Set());
  assert.match(markup, /data-act="check"/);
  assert.match(markup, /data-installed="0"/);
  assert.match(markup, /&lt;img/);
  assert.doesNotMatch(markup, /<img/);
});

test('availability polling remains on the model check endpoint', async () => {
  const calls = [];
  const ctx = runtime(['runUpstreamProbe'], {api: async (method, path) => {
    calls.push(path);
    return {data: method === 'POST' ? {jobId: 'check1'} : {status: 'done', result: {latencyMs: 10}}};
  }});
  await ctx.runUpstreamProbe('model', '/models/check');
  assert.deepEqual(calls, ['/models/check', '/models/check?jobId=check1']);
});

test('credit rendering distinguishes zero, unknown, and stale balance after failure', () => {
  const escape = s => String(s).replaceAll('<', '&lt;').replaceAll('"', '&quot;');
  const states = new Map();
  const ctx = runtime(['creditCellHTML'], { creditStates: states, esc: escape, attr: escape, jsStr: s => s, fmtDateTime: () => 'time' });
  assert.match(ctx.creditCellHTML('id'), />—</);
  states.set('id', {data: {balance: 0, checkedAt: 1}});
  assert.match(ctx.creditCellHTML('id'), />0.0000</);
  states.set('id', {data: {balance: 0.499962, checkedAt: 1}});
  assert.match(ctx.creditCellHTML('id'), />0.5000</);
  states.set('id', {data: {balance: 0.499186, checkedAt: 1}});
  assert.match(ctx.creditCellHTML('id'), />0.4992</);
  states.set('id', {error: '<private>'});
  assert.match(ctx.creditCellHTML('id'), />查询失败</);
  assert.doesNotMatch(ctx.creditCellHTML('id'), /0.0000|<private>/);
  states.set('id', {data: {balance: 0.5, checkedAt: 1}, error: 'HTTP 503'});
  assert.match(ctx.creditCellHTML('id'), /0.5000 ⚠/);
  assert.match(ctx.creditCellHTML('id'), /当前显示上次余额/);
});

test('credit requests limit concurrency, deduplicate, and skip queued rows after paging', async () => {
  const pending = [], calls = [];
  const ctx = runtime(['queueVisibleCredits', 'requestAccountCredit', 'drainCreditQueue', 'fetchAccountCredit'], {
    creditStates: new Map(), creditQueue: [], creditRunning: 0, modalAccountId: null,
    syncCreditViews() {},
    api: async (_, path) => { calls.push(path); return new Promise(resolve => pending.push(resolve)); },
  });
  ctx.queueVisibleCredits(['a','b','c','d','e'].map(accountId => ({accountId})));
  ctx.requestAccountCredit('a');
  assert.equal(calls.length, 3);
  ctx.queueVisibleCredits([{accountId:'f'}]);
  for (const resolve of pending.splice(0)) resolve({data:{balance:0.5,checkedAt:1}});
  await new Promise(setImmediate);
  assert.equal(calls.length, 4);
  assert.ok(calls[3].endsWith('accountId=f'));
  pending.shift()({data:{balance:0,checkedAt:2}});
  await new Promise(setImmediate);
  assert.equal(ctx.creditRunning, 0);
  assert.equal(ctx.creditStates.get('f').data.balance, 0);
  ctx.requestAccountCredit('f');
  assert.equal(calls.length, 4, 'fresh balance should use browser cache');
});

test('account list has an independent Credit column and queries only current page', () => {
  let queried;
  const nodes = { accountTableBody: {} };
  const ctx = runtime(['renderAccountPage'], {
    _: id => nodes[id], accountList: ['a','b','c'].map(accountId => ({accountId,email:accountId,status:'active'})),
    accountPage: 2, accountPageSize: 1, accountSelected: new Set(),
    esc: s => s, attr: s => s, jsStr: s => s, creditCellHTML: () => '0.5000',
    updateAccountPager() {}, queueVisibleCredits: rows => { queried = rows; },
  });
  ctx.renderAccountPage();
  assert.equal(queried.length, 1);
  assert.equal(queried[0].accountId, 'b');
  assert.match(nodes.accountTableBody.innerHTML, /data-credit-id="b">0.5000/);
  assert.equal((nodes.accountTableBody.innerHTML.match(/<td\b/g) || []).length, 9);
});

test('token section explains on-demand refresh rather than account expiry', () => {
  const ctx = runtime(['renderTokenSection'], { esc: s => s, jsStr: s => s, fmtDateTime: () => 'past' });
  const result = ctx.renderTokenSection({accountId:'test',expiresAt:Date.now()-1000});
  assert.match(result, /Access Token 刷新时间/);
  assert.match(result, /下次使用时自动刷新/);
  assert.match(result, /不代表账号到期/);
  assert.match(result, /不是每天定时刷新/);
});
