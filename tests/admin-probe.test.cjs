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
