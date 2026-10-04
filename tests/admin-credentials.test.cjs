const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const html = fs.readFileSync(require('node:path').join(__dirname, '../cmd/cline-proxy/web/admin.html'), 'utf8');

function runtime(api) {
  const nodes = new Map(), timers = [], calls = [], messages = [];
  const tabs = ['oauth', 'token'].map(mode => ({ dataset: { tab: mode }, classList: { toggle() {} } }));
  const node = id => {
    if (!nodes.has(id)) {
      const classes = new Set();
      nodes.set(id, { value: '', textContent: '', href: '', disabled: false, style: {},
        classList: { add: v => classes.add(v), remove: v => classes.delete(v), toggle() {}, contains: v => classes.has(v) },
        removeAttribute(k) { this[k] = ''; } });
    }
    return nodes.get(id);
  };
  const ctx = vm.createContext({
    _: node, document: { querySelectorAll: () => tabs }, Date,
    accountList: [{ accountId: 'A', email: 'a@example.invalid' }, { accountId: 'B', email: 'b@example.invalid' }, { accountId: 'legacy', email: 'user_2' }],
    credentialAccountId: null, credentialOAuthPending: null, credentialOAuthStarting: false, credentialTokenPending: false,
    creditStates: new Map([['A', { error: 'old' }], ['B', { data: 'keep' }]]),
    toast: (...args) => messages.push(args), loadAccounts() {}, loadStats() {},
    setTimeout: fn => timers.push(fn),
    api: async (...args) => {
      calls.push(args);
      return api ? api(...args) : { data: { sessionId: 'session/A', verificationUri: 'https://login.example.invalid/A', userCode: '123' } };
    },
  });
  for (const name of ['syncCredentialControls', 'restoreCredentialOAuth', 'openCredentialModal', 'closeCredentialModal', 'selectCredentialMode', 'startCredentialOAuth', 'submitCredentialToken']) {
    const start = html.search(new RegExp('(?:async )?function ' + name + '\\('));
    assert.ok(start >= 0, name + ' exists');
    vm.runInContext(html.slice(start, html.indexOf('\n}', start) + 2), ctx);
  }
  return { ctx, node, tabs, timers, calls, messages };
}

test('OAuth URL and code survive closing and switching accounts', async () => {
  const { ctx, node } = runtime();
  ctx.openCredentialModal('A');
  await ctx.startCredentialOAuth();
  ctx.closeCredentialModal();
  ctx.openCredentialModal('B');
  assert.equal(node('credentialOAuthUrl').href, '');
  ctx.openCredentialModal('A');
  assert.equal(node('credentialOAuthUrl').href, 'https://login.example.invalid/A');
  assert.equal(node('credentialOAuthCode').textContent, '123');
  assert.equal(node('credentialOAuthBtn').disabled, true);
  assert.equal(node('credentialTokenBtn').disabled, true);
});

test('pending Token submission blocks OAuth, duplicate submission and dismissal', async () => {
  let finish;
  const { ctx, node, tabs, calls } = runtime(() => new Promise(resolve => { finish = resolve; }));
  ctx.openCredentialModal('A');
  node('credentialTokenInput').value = 'supplied';
  const pending = ctx.submitCredentialToken();
  assert.equal(node('credentialOAuthBtn').disabled, true);
  assert.ok(tabs.every(tab => tab.disabled));
  await ctx.startCredentialOAuth();
  await ctx.submitCredentialToken();
  ctx.closeCredentialModal();
  assert.equal(ctx.credentialAccountId, 'A');
  assert.equal(calls.length, 1);
  finish({ data: {} });
  await pending;
  assert.equal(ctx.credentialAccountId, null);
  assert.equal(ctx.creditStates.has('A'), false);
  assert.equal(node('credentialTokenInput').value, '');
});

test('creating OAuth authorization blocks Token submission', async () => {
  let finish;
  const { ctx, node, calls } = runtime(() => new Promise(resolve => { finish = resolve; }));
  ctx.openCredentialModal('A');
  const pending = ctx.startCredentialOAuth();
  node('credentialTokenInput').value = 'supplied';
  await ctx.submitCredentialToken();
  assert.equal(calls.length, 1);
  assert.equal(node('credentialTokenBtn').disabled, true);
  finish({ data: { sessionId: 'session', verificationUri: 'https://login.example.invalid', userCode: '123' } });
  await pending;
  assert.equal(node('credentialTokenBtn').disabled, true);
});

test('late OAuth success for A leaves an open B modal intact', async () => {
  const { ctx, node, timers, calls } = runtime(async (method) => method === 'GET'
    ? { data: { done: true, success: true, updated: true, email: 'a@example.invalid' } }
    : { data: { sessionId: 'session/A', verificationUri: 'https://login.example.invalid', userCode: '123' } });
  ctx.openCredentialModal('A');
  await ctx.startCredentialOAuth();
  ctx.closeCredentialModal();
  ctx.openCredentialModal('B');
  node('credentialTokenInput').value = 'keep';
  await timers.shift()();
  assert.equal(ctx.credentialAccountId, 'B');
  assert.equal(node('credentialTokenInput').value, 'keep');
  assert.equal(node('credentialOAuthBtn').disabled, false);
  assert.equal(node('credentialOAuthProgress').style.display, 'none');
  assert.equal(ctx.creditStates.has('B'), true);
  assert.equal(calls[1][1], '/oauth/status?sessionId=session%2FA');
  assert.equal(calls[1][3], 15000);
});

test('failed Token update retains rotated token and enables retry', async () => {
  const { ctx, node } = runtime(async () => { throw Object.assign(new Error('mismatch'), { data: { refreshToken: 'rotated' } }); });
  ctx.openCredentialModal('A');
  node('credentialTokenInput').value = 'supplied';
  await ctx.submitCredentialToken();
  assert.equal(node('credentialTokenInput').value, 'rotated');
  assert.equal(node('credentialTokenBtn').disabled, false);
  assert.equal(node('credentialOAuthBtn').disabled, false);
  assert.equal(ctx.credentialAccountId, 'A');
});

test('legacy account clearly explains first verified email binding', () => {
  const { ctx, node } = runtime();
  ctx.openCredentialModal('legacy');
  assert.match(node('credentialIdentityNotice').textContent, /将绑定.*官方邮箱/);
  ctx.closeCredentialModal();
  ctx.openCredentialModal('A');
  assert.match(node('credentialIdentityNotice').textContent, /邮箱与此账号一致/);
});
