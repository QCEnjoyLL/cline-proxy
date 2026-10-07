const { test } = require('node:test');
const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const vm = require('node:vm');
const html = readFileSync(require('node:path').join(__dirname, '../cmd/cline-proxy/web/admin.html'), 'utf8');

function source(name) {
  const start = html.search(new RegExp('(?:async )?function ' + name + '\\('));
  assert.ok(start >= 0, name + ' exists');
  return html.slice(start, html.indexOf('\n}', start) + 2);
}

function runtime(installed, overrides = {}) {
  const nodes = new Proxy({}, {get(target, id) { return target[id] ||= {style:{}, value:'attention'}; }});
  const toasts = [];
  const escape = s => String(s).replace(/[&<>"']/g, c => ({'&':'&amp;', '<':'&lt;', '>':'&gt;', '"':'&quot;', "'":'&#39;'}[c]));
  const ctx = vm.createContext({
    _: id => nodes[id], esc: escape, attr: escape,
    mInstalled: installed, mAuditJob:null, mAuditPolling:false, mAuditBusy:false,
    mAuditSelected:new Set(), mAuditSelectionSeen:new Set(),
    toast: (...args) => toasts.push(args), confirm: () => true,
    setTimeout: fn => { fn(); },
    api: () => assert.fail('unexpected network request'),
    loadModels: async () => {}, loadConfig: async () => {}, loadUpstreams: async () => {},
    ...overrides,
  });
  for (const name of ['applyModelAudit', 'modelAuditRows', 'renderModelAudit', 'pollModelAudit',
    'startModelAudit', 'stopModelAudit', 'loadModelAudit', 'selectModelAuditSuggestions',
    'clearModelAuditSelection', 'removeAuditedModels']) {
    vm.runInContext(source(name), ctx);
  }
  return {ctx, nodes, toasts};
}

function item(id, catalog, status, suggested = false, extras = {}) {
  return {modelId:id, upstreamModel:id, catalog, status, suggested, latencyMs:20, ...extras};
}
function job(items, extras = {}) {
  return {jobId:'batch/1', status:'done', stage:'done', completed:items.length, items, ...extras};
}

test('batch results separate membership and failures, keep working aliases and escape IDs', () => {
  const items = [
    item('good', 'present', 'available'), item('removed', 'missing', 'unavailable', true, {error:'HTTP 404'}),
    item('limited', 'present', 'failed', false, {error:'HTTP 429'}),
    item('unlisted', 'missing', 'available'), item('alias', 'present', 'available', false, {upstreamModel:'good'}),
    item('old"<id>', 'unknown', 'failed', false, {error:'<script>private</script>'}),
  ];
  const {ctx, nodes} = runtime(items.map(m => m.modelId));
  ctx.applyModelAudit(job(items));
  assert.deepEqual([...ctx.mAuditSelected], ['removed']);
  assert.match(nodes.modelAuditBody.innerHTML, /不在清单中.*本次不可用/);
  assert.match(nodes.modelAuditBody.innerHTML, /HTTP 429.*默认保留/);
  assert.match(nodes.modelAuditBody.innerHTML, /虽未列出，本次仍可用，默认保留/);
  assert.doesNotMatch(nodes.modelAuditBody.innerHTML, /data-audit-id="good"|data-audit-id="alias"|<script>/);
  assert.match(nodes.modelAuditBody.innerHTML, /old&quot;&lt;id&gt;/);
  assert.equal(nodes.modelAuditRemove.textContent, '移除选中 (1)');
  assert.equal(nodes.modelAuditRemove.disabled, false);
  nodes.modelAuditFilter.value = 'all';
  ctx.renderModelAudit();
  assert.match(nodes.modelAuditBody.innerHTML, /目标在清单中.*重定向至 good/);
});

test('polling never reselects unchecked results and prunes removed models', () => {
  const {ctx, nodes} = runtime(['a', 'b']);
  ctx.applyModelAudit(job([item('a', 'missing', 'unavailable', true), item('b', 'present', 'checking')], {status:'running', completed:1}));
  assert.equal(nodes.modelAuditRemove.disabled, true);
  ctx.clearModelAuditSelection();
  ctx.applyModelAudit(job([item('a', 'missing', 'unavailable', true), item('b', 'present', 'unavailable', true)]));
  assert.deepEqual([...ctx.mAuditSelected], ['b']);
  ctx.selectModelAuditSuggestions();
  assert.deepEqual([...ctx.mAuditSelected], ['a', 'b']);
  ctx.mInstalled = ['b'];
  ctx.renderModelAudit();
  assert.deepEqual([...ctx.mAuditSelected], ['b']);
  assert.doesNotMatch(nodes.modelAuditBody.innerHTML, /data-audit-id="a"/);
  ctx.applyModelAudit(job([item('b', 'unknown', 'skipped')], {jobId:'new-batch', status:'cancelled'}));
  assert.equal(ctx.mAuditSelected.size, 0);
  assert.match(nodes.modelAuditBody.innerHTML, /未完成检测/);
  assert.equal(nodes.modelAuditRemove.disabled, true);
});

test('one click starts a bounded server job and polls to completion', async () => {
  const calls = [];
  let loads = 0;
  const pending = job([item('a', 'unknown', 'pending')], {status:'running', stage:'catalog', completed:0});
  const done = job([item('a', 'missing', 'unavailable', true)]);
  const {ctx, nodes} = runtime(['a'], {
    loadModels: async () => {loads++;},
    api: async (...args) => {calls.push(args); return {data: calls.length === 1 ? pending : done};},
  });
  await ctx.startModelAudit();
  await new Promise(setImmediate);
  assert.equal(loads, 1);
  assert.deepEqual(calls.map(c => c.slice(0, 2)), [['POST','/models/audit'], ['GET','/models/audit?jobId=batch%2F1']]);
  assert.ok(calls.every(c => c[3] === 15000));
  assert.equal(ctx.mAuditPolling, false);
  assert.equal(ctx.mAuditBusy, false);
  assert.match(nodes.modelAuditNotice.textContent, /检测完成.*已检测 1 \/ 1/);
  assert.equal(nodes.modelAuditStop.style.display, 'none');
});

test('page reload restores an active job without starting another one', async () => {
  const calls = [];
  const {ctx} = runtime(['a'], {api: async (method, path) => {
    calls.push([method, path]);
    return {data:job([item('a', 'present', calls.length === 1 ? 'checking' : 'available')], {status:calls.length === 1 ? 'running' : 'done'})};
  }});
  await ctx.loadModelAudit();
  await new Promise(setImmediate);
  assert.deepEqual(calls, [['GET','/models/audit'], ['GET','/models/audit?jobId=batch%2F1']]);
  assert.equal(ctx.mAuditJob.status, 'done');
  assert.equal(ctx.mAuditSelected.size, 0);
});

test('poll errors preserve results and let the user reconnect', async () => {
  const {ctx, nodes, toasts} = runtime(['a'], {api: async () => {throw new Error('gateway offline');}});
  ctx.applyModelAudit(job([item('a', 'unknown', 'pending')], {status:'running'}));
  await ctx.pollModelAudit();
  assert.equal(ctx.mAuditJob.status, 'running');
  assert.equal(ctx.mAuditPolling, false);
  assert.equal(nodes.modelAuditStart.textContent, '恢复检测进度');
  assert.equal(nodes.modelAuditStart.disabled, false);
  assert.match(toasts.at(-1)[0], /gateway offline/);
});

test('stopping keeps completed selections and ignores unfinished models', async () => {
  const calls = [];
  const {ctx, nodes} = runtime(['a', 'b'], {api: async (...args) => {
    calls.push(args);
    return {data:job([item('a','missing','unavailable',true), item('b','unknown','skipped')], {status:'cancelled', completed:1})};
  }});
  ctx.applyModelAudit(job([item('a','missing','unavailable',true), item('b','unknown','checking')], {status:'running', completed:1}));
  await ctx.stopModelAudit();
  assert.equal(calls[0][1], '/models/audit/stop');
  assert.equal(calls[0][2].jobId, 'batch/1');
  assert.deepEqual([...ctx.mAuditSelected], ['a']);
  assert.match(nodes.modelAuditNotice.textContent, /检测已停止/);
  assert.equal(nodes.modelAuditRemove.disabled, false);
});

test('batch removal sends only selected enabled IDs and updates model/config/upstream views', async () => {
  const calls = [];
  const {ctx, nodes} = runtime(['a', 'b'], {
    api: async (method, path, body) => {
      calls.push([method, path, [...body.ids]]);
      return {data:{removed:['a'], skipped:[]}};
    },
    loadModels: async () => {calls.push('models');},
    loadConfig: async () => {calls.push('config');},
    loadUpstreams: async () => {calls.push('upstreams');},
  });
  ctx.applyModelAudit(job([item('a','missing','unavailable',true), item('b','present','available'), item('already-gone','missing','unavailable',true)]));
  await ctx.removeAuditedModels();
  assert.deepEqual(calls, [['POST','/models/delete-batch',['a']], 'models', 'config', 'upstreams']);
  assert.deepEqual([...ctx.mInstalled], ['b']);
  assert.equal(ctx.mAuditSelected.size, 0);
  assert.equal(nodes.modelAuditRemove.disabled, true);
});

test('failed removal retains selections, and cancellation or an active audit never deletes', async () => {
  const {ctx, nodes, toasts} = runtime(['a'], {api: async () => {throw new Error('storage failed');}});
  const done = job([item('a','missing','unavailable',true)]);
  ctx.applyModelAudit(done);
  await ctx.removeAuditedModels();
  assert.deepEqual([...ctx.mAuditSelected], ['a']);
  assert.equal(nodes.modelAuditRemove.disabled, false);
  assert.match(toasts.at(-1)[0], /storage failed/);
  ctx.api = () => assert.fail('must not remove');
  ctx.confirm = () => false;
  await ctx.removeAuditedModels();
  ctx.confirm = () => true;
  ctx.applyModelAudit({...done, status:'running'});
  await ctx.removeAuditedModels();
});
