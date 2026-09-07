const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const test = require('node:test');

const source = fs.readFileSync(path.join(__dirname, 'web/app.js'), 'utf8');
const functionsOnly = source.slice(0, source.indexOf("document.querySelectorAll('nav button').forEach"));
const GiB = 1024 ** 3;
function ui() {
  const elements = Object.fromEntries(['diskWarning', 'diskDialogTitle', 'closeDiskManager', 'refreshDiskManager', 'diskManagerContent', 'diskSelectionSummary', 'cleanupDisk'].map(id => ['#' + id, {}]));
  elements['#diskManager'] = { open: false, showModal() { this.open = true; }, close() { this.open = false; } };
  const context = vm.createContext({ document: { querySelector: s => elements[s], querySelectorAll: () => [] }, setTimeout, clearTimeout });
  vm.runInContext(fs.readFileSync(path.join(__dirname, 'web/miners.js'), 'utf8'), context);
  vm.runInContext(functionsOnly, context);
  context.elements = elements;
  context.render = () => context.renderDiskWarning();
  context.refresh = async () => {};
  context.toast = () => {};
  context.confirm = () => true;
  return context;
}
function state(context, fixture) { context.fixture = fixture; vm.runInContext('Object.assign(state, fixture)', context); }
function panel(context) { return vm.runInContext('diskPanel', context); }
function server(available = 20 * GiB, total = 100 * GiB, extra = {}) {
  return { id: 'srv', name: 'Worker', status: 'online', lastSuccessAt: new Date().toISOString(), workDir: '/work/lab',
    systemInfo: `disk_total_kb=${total / 1024}\ndisk_available_kb=${available / 1024}\ndisk_path=/work/lab`, ...extra };
}
function candidate(id, bytes = GiB) {
  return { experimentId: id, serverId: 'historical-server', path: `/work/lab/experiments/${id}/ethash`, bytes, fingerprint: `fingerprint-${id}`, files: 2 };
}
function report(candidates = [candidate('old')], extra = {}) {
  return { serverId: 'srv', checkedAt: new Date().toISOString(), disk: { path: '/work/lab', totalBytes: 100 * GiB, availableBytes: 2 * GiB, inodesTotal: 1000, inodesAvailable: 100 },
    candidates, logPolicy: { enabled: true, maxBytes: 64 * 1024 ** 2, retainFiles: 3 }, ...extra };
}

test('disk thresholds use both free fraction and bytes, including zero available and exact limits', () => {
  const context = ui();
  for (const [available, total, severity] of [[10, 100, 'warning'], [5, 100, 'critical'], [2, 10, 'warning'], [.9, 10, 'critical'], [0, 100, 'critical'], [3, 10, 'healthy'], [1, 10, 'warning']]) {
    assert.equal(context.diskHealth(server(available * GiB, total * GiB)).severity, severity, `${available}/${total} GiB`);
  }
  for (const systemInfo of ['', 'disk_available_kb=0', 'disk_total_kb=0\ndisk_available_kb=0', 'disk_total_kb=100\ndisk_available_kb=101', 'disk_total_kb=100\ndisk_available_kb=-1']) {
    assert.equal(context.diskHealth(server(0, 0, { systemInfo })).severity, 'unknown');
  }
  assert.equal(context.formatDiskBytes(null), '—');
  assert.equal(context.formatDiskBytes(0), '0 B');
  const inodes = context.diskHealth(server(0, 0, { systemInfo: 'disk_total_inodes=100\ndisk_available_inodes=0' }));
  assert.equal(inodes.inodesTotal, '100');
  assert.equal(inodes.inodesAvailable, '0');
});

test('fresh disk inspection supersedes old metrics without changing failed connection status; newer metrics win later', () => {
  const context = ui(), now = Date.now();
  const worker = server(.5 * GiB, 100 * GiB, { status: 'error', lastError: 'SSH unavailable', lastSuccessAt: new Date(now - 180000).toISOString() });
  assert.equal(context.diskHealth(worker, now).stale, true);
  context.sample = { checkedAt: new Date(now).toISOString(), disk: report().disk };
  vm.runInContext("diskSamples.set('srv', sample)", context);
  assert.equal(context.diskHealth(worker, now).available, 2 * GiB);
  assert.equal(context.diskHealth(worker, now).stale, false);
  assert.equal(worker.status, 'error');
  assert.equal(context.diskHealth(worker, now + 120001).stale, true);
  assert.equal(context.diskHealth({ ...worker, lastSuccessAt: new Date(now + 1000).toISOString() }, now + 1000).available, .5 * GiB);
});

test('server card and global alert preserve connectivity and display real path, GiB, severity and stale age', () => {
  const context = ui(), worker = server(.5 * GiB, 100 * GiB, { lastSuccessAt: '2020-01-01T00:00:00Z' });
  state(context, { servers: [worker, server(20 * GiB, 100 * GiB, { id: 'healthy' })] });
  const html = context.serverRow(worker);
  assert.match(html, /class="status ">online/);
  assert.match(html, /0.50 GiB 可用/);
  assert.match(html, /磁盘空间严重不足/);
  assert.match(html, /\/work\/lab/);
  assert.match(html, /采样已过期/);
  context.renderDiskWarning();
  assert.equal(context.elements['#diskWarning'].hidden, false);
  assert.match(context.elements['#diskWarning'].innerHTML, /1 台服务器磁盘空间告警/);
  assert.match(context.elements['#diskWarning'].innerHTML, /2020/);
  assert.doesNotMatch(context.elements['#diskWarning'].innerHTML, /尚未查询/);
  state(context, { servers: [server()] });
  context.renderDiskWarning();
  assert.equal(context.elements['#diskWarning'].hidden, true);
});

test('experiment disk hint includes only its placement and node servers and escapes names and paths', () => {
  const context = ui();
  state(context, { servers: [server(GiB, 100 * GiB, { name: '<worker>' }), server(GiB, 100 * GiB, { id: 'other', name: 'Unrelated' })] });
  const html = context.experimentDiskHint({ placements: [{ serverId: 'srv' }], nodes: [{ serverId: 'srv' }] });
  assert.match(html, /&lt;worker&gt;/);
  assert.match(html, /新增矿工前会重新检查/);
  assert.doesNotMatch(html, /Unrelated/);
  assert.equal((html.match(/data-disk-server=/g) || []).length, 1);
});

test('dialog inspects using GET, starts with no selected candidates, and recovers from inspection errors', async () => {
  const context = ui();
  state(context, { servers: [server()] });
  let calls = 0;
  context.api = async (url, options) => { assert.equal(url, '/servers/srv/disk'); assert.equal(options, undefined); if (++calls === 1) throw new Error('<offline>'); return report(); };
  await context.openDiskManager('srv');
  assert.equal(context.elements['#diskManager'].open, true);
  assert.match(context.elements['#diskManagerContent'].innerHTML, /&lt;offline&gt;/);
  assert.equal(context.elements['#cleanupDisk'].disabled, true);
  await context.loadDiskManager();
  assert.equal(panel(context).error, '');
  assert.equal(context.selectedDiskTargets().length, 0);
  assert.match(context.elements['#diskSelectionSummary'].textContent, /已选 0 项/);
  assert.doesNotMatch(context.elements['#diskManagerContent'].innerHTML, /type="checkbox"[^>]*\schecked(?:\s|>)/);
  assert.match(context.elements['#diskManagerContent'].innerHTML, /64.00 MiB/);
  assert.match(context.elements['#diskManagerContent'].innerHTML, /保留 3 份历史日志/);
  assert.match(context.elements['#diskManagerContent'].innerHTML, /待核验交易时延后轮转/);
  assert.match(context.elements['#diskManagerContent'].innerHTML, /可用 inode 100 \/ 1,000/);
});

test('late GET completion cannot overwrite a newer server dialog', async () => {
  const context = ui();
  state(context, { servers: [server(), server(0, 100 * GiB, { id: 'second' })] });
  let finish;
  context.api = url => url === '/servers/srv/disk' ? new Promise(resolve => { finish = resolve; }) : Promise.resolve(report([], { serverId: 'second' }));
  const first = context.openDiskManager('srv');
  await context.openDiskManager('second');
  finish(report());
  await first;
  assert.equal(panel(context).data.serverId, 'second');
  assert.equal(panel(context).loading, false);
});

test('cleanup sends only explicit selections after confirmation, preserves historical identity, blocks duplicates, and reports partial success', async () => {
  const context = ui(), candidates = [candidate('a'), candidate('b', 2 * GiB), candidate('untouched')];
  state(context, { servers: [server()] });
  const calls = [], confirmations = [];
  let finish, refreshes = 0;
  context.refresh = async () => { refreshes++; };
  context.api = (url, options) => { calls.push({ url, options }); return options?.method === 'POST' ? new Promise(resolve => { finish = resolve; }) : Promise.resolve(report(candidates)); };
  await context.openDiskManager('srv');
  await context.cleanupSelectedDisk();
  assert.equal(calls.length, 1);
  context.selectDiskCandidate(0, true);
  context.selectDiskCandidate(1, true);
  assert.match(context.elements['#diskSelectionSummary'].textContent, /3.00 GiB/);
  context.confirm = message => { confirmations.push(message); return confirmations.length > 1; };
  await context.cleanupSelectedDisk();
  assert.equal(calls.length, 1, 'cancelled confirmation must not send a request');
  const cleanup = context.cleanupSelectedDisk();
  await context.cleanupSelectedDisk();
  context.closeDiskManager();
  assert.equal(context.elements['#diskManager'].open, true);
  context.selectDiskCandidate(2, true);
  assert.equal(context.selectedDiskTargets().length, 2);
  assert.equal(calls.length, 2);
  assert.equal(calls[1].url, '/servers/srv/disk/cleanup');
  assert.deepEqual(JSON.parse(calls[1].options.body), { targets: candidates.slice(0, 2) });
  assert.match(confirmations[1], /可重新生成的 DAG/);
  assert.match(confirmations[1], /不删除链数据、账户或实验记录/);
  finish({ freedBytes: GiB, results: [{ experimentId: 'a', path: candidates[0].path, freedBytes: GiB }, { experimentId: 'b', path: candidates[1].path, error: '<files changed>' }] });
  await cleanup;
  assert.equal(calls.length, 3);
  assert.equal(calls[2].options, undefined, 'refresh disk with GET');
  assert.equal(refreshes, 1);
  assert.equal(context.selectedDiskTargets().length, 0);
  assert.equal(context.elements['#cleanupDisk'].disabled, true);
  assert.match(context.elements['#diskManagerContent'].innerHTML, /部分项目未清理/);
  assert.match(context.elements['#diskManagerContent'].innerHTML, /&lt;files changed&gt;/);
  assert.match(context.elements['#diskManagerContent'].innerHTML, /估算释放 1.00 GiB/);
  assert.match(context.elements['#diskManagerContent'].innerHTML, /文件系统可用空间以重新检查为准/);
});

test('cleanup error remains visible after refreshed inventory and does not auto-retry destructive requests', async () => {
  const context = ui();
  state(context, { servers: [server()] });
  let posts = 0;
  context.api = async (_, options) => { if (options?.method === 'POST') { posts++; throw new Error('fingerprint changed'); } return report(); };
  await context.openDiskManager('srv');
  context.selectDiskCandidate(0, true);
  await context.cleanupSelectedDisk();
  assert.equal(posts, 1);
  assert.match(context.elements['#diskManagerContent'].innerHTML, /清理未完成：fingerprint changed/);
  assert.equal(context.elements['#cleanupDisk'].disabled, true);
  assert.equal(context.elements['#closeDiskManager'].disabled, false);
});

test('completed cleanup retains its storage warning alongside actual results after inventory refresh', async () => {
  const context = ui();
  state(context, { servers: [server()] });
  context.api = async (_, options) => options?.method === 'POST'
    ? { freedBytes: GiB, results: [{ experimentId: 'old', path: '/work/lab/experiments/old/ethash', freedBytes: GiB }], storageWarning: '清理已执行，但结果持久化失败：<disk full>' }
    : report();
  await context.openDiskManager('srv');
  context.selectDiskCandidate(0, true);
  await context.cleanupSelectedDisk();
  const html = context.elements['#diskManagerContent'].innerHTML;
  assert.match(html, /role="alert">清理已执行，但结果持久化失败：&lt;disk full&gt;/);
  assert.match(html, /不要根据旧记录重复清理/);
  assert.match(html, /估算释放 1.00 GiB/);
  assert.equal(context.elements['#cleanupDisk'].disabled, true);
});
