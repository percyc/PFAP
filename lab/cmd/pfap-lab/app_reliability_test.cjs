const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const test = require('node:test');

const source = fs.readFileSync(path.join(__dirname, 'web/app.js'), 'utf8');
const functionsOnly = source.slice(0, source.indexOf("document.querySelectorAll('nav button').forEach"));
function ui(elements = {}) {
  const context = vm.createContext({
    document: { querySelector: selector => elements[selector], querySelectorAll: () => [] },
    setTimeout, clearTimeout,
  });
  vm.runInContext(fs.readFileSync(path.join(__dirname, 'web/miners.js'), 'utf8'), context);
  vm.runInContext(functionsOnly, context);
  context.toast = () => {};
  return context;
}
function state(context, value) {
  context.fixture = value;
  vm.runInContext('Object.assign(state, fixture)', context);
}
function detailContainer() {
  return {
    details: [],
    set innerHTML(html) {
      this.html = html;
      this.details = [...html.matchAll(/<details[^>]*data-detail-key="([^"]+)"[^>]*>/g)]
        .map(match => ({ dataset: { detailKey: match[1] }, open: false }));
    },
    get innerHTML() { return this.html; },
    querySelectorAll() { return this.details; },
  };
}

test('interrupted and stop-failed experiments keep connection and deletion locks; resume needs original identity', async () => {
  const context = ui();
  context.confirm = () => assert.fail('locked deletion must not prompt');
  const base = { id: 'exp', placements: [{ serverId: 'srv', count: 1 }], nodes: [{ id: 'node' }], artifactSha: 'a'.repeat(64) };
  for (const status of ['resuming', 'interrupted', 'stop-failed']) {
    const e = { ...base, status };
    state(context, { experiments: [e] });
    assert.equal(context.serverLocked('srv'), true);
    assert.equal(context.serverConnectionLocked('srv'), true);
    assert.doesNotMatch(context.experimentActions(e), /deleteExperiment|部署新实验/);
    await context.deleteExperiment('exp');
    if (status !== 'resuming') assert.match(context.experimentActions(e), /'resume'/);
    else assert.equal(context.experimentBusy(e), true);
  }
  assert.equal(context.canResume({ ...base, status: 'stopped', artifactSha: '' }), false);
  assert.equal(context.canResume({ ...base, status: 'stopped', nodes: [] }), false);
  assert.equal(context.canFreshDeploy({ status: 'draft', startedAt: '0001-01-01T00:00:00Z' }), true);
  assert.equal(context.canFreshDeploy({ ...base, status: 'failed' }), false);
  assert.match(context.status('unknown', 'node'), /待检查/);
  assert.match(context.status('unknown'), /待核验/);
});

test('unknown transaction keeps nodes busy and prevents duplicate account initialization', async () => {
  const elements = { '#experimentSelect': { value: 'exp' }, '#accountInitialization': {} };
  const context = ui(elements);
  state(context, {
    experiments: [{ id: 'exp', status: 'running', nodes: [{ id: 'node', status: 'running' }] }],
    transactions: [{ id: 'tx', experimentId: 'exp', status: 'unknown', type: 'createAccount', fromNode: 'node' }],
  });
  assert.equal(context.activeTx({ status: 'unknown' }), true);
  assert.equal(context.activeTx({ status: 'cancelled' }), false);
  assert.equal(context.nodeBusy('exp', 'node'), true);
  context.renderAccountInitialization();
  assert.match(elements['#accountInitialization'].innerHTML, /待核验，勿重复初始化/);
  assert.match(elements['#accountInitialization'].innerHTML, /disabled>初始化 0 个待处理节点/);
  context.api = () => assert.fail('unknown initialization must not replay');
  await context.initializeAllAccounts();
});

test('receipt, command, error, and route expansion are independent and survive refresh and scope changes', () => {
  const container = detailContainer();
  const context = ui({ '#rows': container });
  const detail = key => `<details data-detail-key="${key}"><summary>details</summary></details>`;
  const keys = ['transaction:a:command', 'transaction:a:receipt', 'transaction:a:error', 'workload:a:routes', 'server:a:error'];
  const html = keys.map(detail).join('');
  context.patchHTML('#rows', html);
  container.details[1].open = true;
  container.details[3].open = true;
  container.details[4].open = true;
  context.patchHTML('#rows', html + 'updated');
  assert.deepEqual(container.details.map(d => d.open), [false, true, false, true, true]);
  container.details[1].open = false;
  context.patchHTML('#rows', detail('transaction:b:receipt'));
  assert.equal(container.details[0].open, false);
  context.patchHTML('#rows', html);
  assert.deepEqual(container.details.map(d => d.open), [false, false, false, true, true]);
});

test('workload elapsed time and rate freeze at submission stop, with cancelled and unknown counts separate', () => {
  const context = ui();
  const workload = { id: 'w', status: 'draining', submitted: 4, attempted: 7, skippedBusy: 2, skippedUnavailable: 1, stopRequested: true,
    startedAt: '2026-09-07T00:00:00Z', submissionStoppedAt: '2026-09-07T00:00:10Z', finishedAt: '2026-09-07T00:01:00Z' };
  assert.equal(context.workloadTiming(workload, Date.parse('2026-09-07T05:00:00Z')).elapsed, 10);
  assert.equal(context.workloadTiming(workload).rate, 0.4);
  state(context, { transactions: ['confirmed', 'cancelled', 'unknown', 'queued'].map((status, i) => ({ workloadId: 'w', status, sequence: i })) });
  const html = context.workloadRow(workload);
  for (const expected of ['尝试 7', '节点忙跳过 2', '不可用跳过 1', '处理中 1', '待核验 1', '已取消 1', '10.0 s', '0.400 笔/s']) assert.ok(html.includes(expected), expected);
  assert.doesNotMatch(html, /onclick="stopWorkload/);
});

test('workload stop confirms drain semantics, suppresses duplicate pending requests, and never stops experiment', async () => {
  const context = ui();
  state(context, { workloads: [{ id: 'w', name: 'Load', status: 'running' }] });
  let confirmation = '', finish;
  const calls = [];
  context.confirm = message => { confirmation = message; return true; };
  context.renderWorkloads = () => {};
  context.refresh = async () => {};
  context.api = (url, options) => { calls.push([url, options.method]); return new Promise(resolve => { finish = resolve; }); };
  const pending = context.stopWorkload('w');
  await context.stopWorkload('w');
  assert.deepEqual(calls, [['/workloads/w/stop', 'POST']]);
  assert.match(confirmation, /已经入队.*继续处理/);
  assert.match(confirmation, /实验节点继续运行/);
  finish({ status: 'draining', stopRequested: true });
  await pending;
  await context.stopWorkload('w');
  assert.equal(calls.length, 1);
});

test('resume uses explicit endpoint and experiment stop describes cancellation versus unknown outcomes', async () => {
  const context = ui();
  const calls = [], confirmations = [];
  context.confirm = message => { confirmations.push(message); return true; };
  context.render = () => {};
  context.refresh = async () => {};
  context.api = async (url, options) => { calls.push([url, options.method]); return { status: url.endsWith('/stop') ? 'stopping' : 'resuming' }; };
  state(context, { experiments: [{ id: 'exp', name: 'Original', status: 'interrupted', artifactSha: 'b'.repeat(64), nodes: [{ id: 'node' }] }] });
  await context.experiment('exp', 'deploy');
  assert.equal(calls.length, 0);
  await context.experiment('exp', 'resume');
  await context.experiment('exp', 'resume');
  assert.deepEqual(calls, [['/experiments/exp/resume', 'POST']]);
  state(context, { experiments: [{ id: 'exp', name: 'Original', status: 'running' }] });
  await context.experiment('exp', 'stop');
  assert.deepEqual(calls[1], ['/experiments/exp/stop', 'POST']);
  assert.match(confirmations[1], /尚未开始的排队交易会取消/);
  assert.match(confirmations[1], /执行中或等待确认的交易将标为待核验/);
});

test('transaction reconciliation is read-only with hash validation and a pending guard', async () => {
  const context = ui();
  const tx = { id: 'tx', status: 'unknown', hash: '0x' + 'c'.repeat(64), executionStage: 'submit', reconciliationError: '<offline>', lastCheckedAt: '2026-09-07T00:00:00Z' };
  state(context, { transactions: [tx] });
  const html = context.transactionReconciliation(tx);
  assert.match(html, /只读/);
  assert.match(html, /submit/);
  assert.match(html, /&lt;offline&gt;/);
  assert.match(html, /transaction:tx:reconciliation/);
  assert.match(context.transactionReconciliation({ id: 'x', status: 'unknown' }), /不能自动重放/);
  const calls = []; let finish;
  context.renderTransactions = () => {};
  context.refresh = async () => {};
  context.api = (url, options) => { calls.push([url, options.method]); return new Promise(resolve => { finish = resolve; }); };
  const pending = context.reconcileTransaction('tx');
  await context.reconcileTransaction('tx');
  assert.deepEqual(calls, [['/transactions/tx/reconcile', 'POST']]);
  finish({ status: 'confirmed', receipt: '{}' });
  await pending;
  await context.reconcileTransaction('tx');
  assert.equal(calls.length, 1);
});

test('storage warning still renders when state refresh fails and clears only on a healthy response', async () => {
  const banner = detailContainer();
  const context = ui({ '#storageWarning': banner });
  context.render = () => {};
  context.api = async url => {
    if (url === '/state') throw new Error('state unavailable');
    if (url === '/storage-health') return { degraded: true, lastError: '<disk full>', backupPath: '/data/state.json.bak', lastSavedAt: '2026-09-07T00:00:00Z' };
    return {};
  };
  await assert.rejects(context.refresh(), /state unavailable/);
  assert.equal(banner.hidden, false);
  assert.match(banner.innerHTML, /&lt;disk full&gt;/);
  assert.match(banner.innerHTML, /state.json.bak/);
  assert.match(banner.innerHTML, /最近成功保存/);
  context.api = async url => url === '/storage-health' ? { degraded: false } : url === '/state' ? { experiments: [] } : {};
  await context.refresh();
  assert.equal(banner.hidden, true);
});

test('server error displays cached metrics and last-success time without replacing them with error text', () => {
  const context = ui();
  const html = context.serverRow({ id: 'srv', status: 'error', lastError: '<SSH denied>', lastSuccessAt: '2026-09-07T00:00:00Z', systemInfo: 'load1=0.00\ncpus=4\nmemory_kb=100\nmemory_available_kb=25' });
  assert.match(html, /0.00/);
  assert.match(html, /75.0%/);
  assert.match(html, /最近成功/);
  assert.match(html, /&lt;SSH denied&gt;/);
  assert.match(html, /server:srv:error/);
  assert.match(html, /保留最近一次成功采样/);
});
