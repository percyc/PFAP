const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const test = require('node:test');
const source = fs.readFileSync(path.join(__dirname, 'web/app.js'), 'utf8');
const functionsOnly = source.slice(0, source.indexOf("document.querySelectorAll('nav button').forEach"));
function ui() {
  const elements = Object.fromEntries(['selectAllServers', 'batchTrustServers', 'batchDeleteServers', 'serverSelectionCount', 'clearServerSelection', 'batchServerHostGroup', 'serverSummary', 'serverRows', 'serverResultCount', 'serverPageInfo', 'serverPreviousPage', 'serverNextPage', 'serverSearch', 'serverConnectionFilter', 'serverDiskFilter', 'serverSort', 'serverPageSize', 'serverGroupScope', 'serverGroupInput', 'serverGroupError'].map(id => ['#' + id, {}]));
  elements['#serverHostGroupDialog'] = { showModal() { this.open = true; }, close() { this.open = false; } };
  const context = vm.createContext({ document: { querySelector: s => elements[s], querySelectorAll: () => [] }, FormData: class { constructor(form) { return Object.entries(form.values); } }, setTimeout, clearTimeout });
  vm.runInContext(functionsOnly, context);
  context.elements = elements;
  context.toast = () => {};
  context.refresh = async () => {};
  context.confirm = () => true;
  return context;
}
function fixture(context, count = 22) {
  context.fixture = Array.from({ length: count }, (_, i) => ({ id: `s${i + 1}`, name: `worker-${i + 1}`, host: `10.0.0.${i + 1}`, hostGroup: i < 11 ? 'pve-a' : 'pve-b', status: i % 3 === 0 ? 'error' : i % 3 === 1 ? 'online' : 'unknown', createdAt: new Date(2026, 0, i + 1).toISOString(), lastSuccessAt: new Date().toISOString(), systemInfo: `disk_total_kb=104857600\ndisk_available_kb=${i % 2 ? 20971520 : 524288}\ndisk_path=/srv/lab` }));
  vm.runInContext('state.servers = fixture', context);
}
function view(context, values) { context.viewFixture = values; vm.runInContext('Object.assign(serverView, viewFixture)', context); }
function selected(context, ids) { context.selectionFixture = ids; vm.runInContext('selectedServerIds = new Set(selectionFixture)', context); }
function ids(items) { return Array.from(items, s => s.id); }

test('server inventory paginates 22 servers by natural names, supports address and newest sort, and clamps after deletion', () => {
  const context = ui(); fixture(context);
  let result = context.serverInventory();
  assert.equal(result.pages, 3);
  assert.deepEqual(ids(result.items), Array.from({ length: 10 }, (_, i) => `s${i + 1}`));
  view(context, { page: 3 });
  assert.deepEqual(ids(context.serverInventory().items), ['s21', 's22']);
  view(context, { sort: 'host', page: 1 });
  assert.deepEqual(ids(context.serverInventory().items).slice(0, 3), ['s1', 's2', 's3']);
  view(context, { sort: 'created' });
  assert.equal(context.serverInventory().items[0].id, 's22');
  view(context, { page: 9 });
  vm.runInContext('state.servers = state.servers.slice(0, 9)', context);
  context.renderServerInventory();
  assert.equal(vm.runInContext('serverView.page', context), 1);
  assert.equal(context.elements['#serverNextPage'].disabled, true);
});

test('name, IP, host group and status/disk filters combine without changing selection', () => {
  const context = ui(); fixture(context); selected(context, ['s1', 's22']);
  view(context, { query: 'PVE-B', connection: 'online', disk: 'healthy' });
  const result = context.serverInventory();
  assert.ok(result.items.length > 0);
  assert.ok(Array.from(result.items).every(s => s.hostGroup === 'pve-b' && s.status === 'online' && Number(s.id.slice(1)) % 2 === 0));
  assert.deepEqual(ids(context.selectedServers()), ['s1', 's22']);
  view(context, { query: '10.0.0.22', connection: 'all', disk: 'all' });
  assert.deepEqual(ids(context.serverInventory().items), ['s22']);
  Object.assign(context.elements['#serverSearch'], { value: 'worker-2' });
  for (const [id, value] of [['serverConnectionFilter', 'all'], ['serverDiskFilter', 'all'], ['serverSort', 'name'], ['serverPageSize', '20']]) context.elements['#' + id].value = value;
  view(context, { page: 3 });
  context.updateServerFilters();
  assert.equal(vm.runInContext('serverView.page', context), 1);
  assert.equal(vm.runInContext('serverView.pageSize', context), 20);
  assert.deepEqual(ids(context.selectedServers()), ['s1', 's22']);
});

test('select-all affects current page, hidden selections remain explicit, and deleted IDs are pruned', () => {
  const context = ui(); fixture(context);
  context.selectCurrentServerPage(true);
  assert.equal(context.selectedServers().length, 10);
  assert.equal(context.elements['#selectAllServers'].checked, true);
  view(context, { page: 3 });
  context.syncServerSelectionUI();
  assert.equal(context.elements['#selectAllServers'].checked, false);
  assert.match(context.elements['#serverSelectionCount'].textContent, /10 台不在当前页/);
  context.selectCurrentServerPage(true);
  assert.equal(context.selectedServers().length, 12);
  context.selectCurrentServerPage(false);
  assert.equal(context.selectedServers().length, 10);
  vm.runInContext("state.servers = state.servers.filter(s => s.id !== 's1')", context);
  context.syncServerSelectionUI();
  assert.equal(vm.runInContext("selectedServerIds.has('s1')", context), false);
  context.clearServerSelection();
  assert.equal(context.selectedServers().length, 0);
});

test('bulk trust and delete confirm every selected name including hidden rows and suppress duplicates', async () => {
  for (const action of ['trust', 'delete']) {
    const context = ui(); fixture(context); selected(context, ['s1', 's22']);
    const calls = []; let finish, message;
    context.confirm = text => { message = text; return true; };
    context.api = (url, options) => { calls.push({ url, body: JSON.parse(options.body) }); return new Promise(resolve => { finish = resolve; }); };
    const fn = action === 'trust' ? 'batchTrustServers' : 'batchDeleteServers';
    const pending = context[fn]();
    await context[fn]();
    context.selectCurrentServerPage(true);
    assert.equal(context.selectedServers().length, 2);
    assert.equal(calls.length, 1);
    assert.deepEqual(calls[0].body.ids, ['s1', 's22']);
    assert.match(message, /共 2 台（其中 1 台不在当前页）/);
    assert.match(message, /worker-22.*不在当前页/);
    finish({ trusted: 2, deleted: 2 });
    await pending;
    assert.equal(context.elements['#selectAllServers'].disabled, false);
  }
});

test('bulk actions retain local/experiment locks and a maximum of 100 exact targets', async () => {
  const context = ui(); fixture(context, 101); selected(context, context.fixture.map(s => s.id));
  context.confirm = () => assert.fail('unsafe selection must not prompt');
  context.api = () => assert.fail('unsafe selection must not mutate');
  context.syncServerSelectionUI();
  assert.equal(context.elements['#batchDeleteServers'].disabled, true);
  assert.equal(context.elements['#batchServerHostGroup'].disabled, true);
  await context.batchDeleteServers(); await context.batchTrustServers();
  selected(context, ['s1']);
  vm.runInContext("state.servers[0].host = 'local'; state.experiments = [{id:'e',status:'interrupted',placements:[{serverId:'s1',count:1}]}]", context);
  context.syncServerSelectionUI();
  assert.equal(context.elements['#batchTrustServers'].disabled, true);
  assert.equal(context.elements['#batchDeleteServers'].disabled, true);
  assert.equal(context.elements['#batchServerHostGroup'].disabled, false, 'metadata edit remains allowed for active servers');
  await context.batchDeleteServers(); await context.batchTrustServers();
});

test('host group update freezes selected scope, sends metadata only, allows explicit clearing and guards pending requests', async () => {
  const context = ui(); fixture(context); selected(context, ['s1', 's22']);
  context.openServerHostGroup();
  assert.match(context.elements['#serverGroupScope'].textContent, /worker-22/);
  context.fixture[0].name = 'name changed after opening';
  context.elements['#serverGroupInput'].value = ' pve-03 ';
  const calls = []; let finish, message;
  context.confirm = text => { message = text; return true; };
  context.api = (url, options) => { calls.push({ url, body: JSON.parse(options.body) }); return new Promise(resolve => { finish = resolve; }); };
  selected(context, ['s2']);
  const pending = context.saveServerHostGroup({ preventDefault() {} });
  await context.saveServerHostGroup({ preventDefault() {} });
  assert.equal(calls.length, 1);
  assert.deepEqual(calls[0], { url: '/servers/batch/host-group', body: { ids: ['s1', 's22'], hostGroup: 'pve-03' } });
  assert.match(message, /不会自动切换现有实验的矿工角色/);
  assert.match(message, /worker-1 \(/);
  assert.doesNotMatch(message, /name changed after opening/);
  finish({ updated: 2 }); await pending;
  context.openServerHostGroup(); context.elements['#serverGroupInput'].value = '';
  context.api = async (_, options) => { assert.equal(JSON.parse(options.body).hostGroup, ''); return { updated: 1 }; };
  await context.saveServerHostGroup({ preventDefault() {} });
  assert.match(message, /清除宿主机标注/);
});

test('compact rows retain escaped group, visible metrics and expandable persistent details', () => {
  const context = ui(); fixture(context);
  const html = context.serverRow({ ...context.fixture[0], hostGroup: '<pve>', lastError: '<ssh failed>' });
  assert.match(html, /宿主机 &lt;pve&gt;/);
  assert.match(html, /0.50 GiB 可用/);
  assert.match(html, /data-detail-key="server:s1:details"/);
  assert.match(html, /&lt;ssh failed&gt;/);
});

test('successful single, batch and host-group saves report refresh failure visibly without reopening or replaying', async () => {
  for (const kind of ['single', 'batch', 'group']) {
    const context = ui(); fixture(context); selected(context, ['s1']);
    const form = { values: { serverId: 's1', name: 'edited', hostGroup: 'pve', hosts: '10.1.0.1\n10.1.0.2', port: '22' }, valid: true,
      elements: Object.fromEntries(['serverId', 'user', 'port', 'workDir'].map(name => [name, { value: '' }])),
      reportValidity() { return this.valid; }, reset() { this.valid = false; } };
    for (const id of ['serverEditorError', 'serverFormTitle', 'saveServer']) context.elements['#' + id] = {};
    context.elements['#serverForm'] = form;
    context.elements['#serverEditor'] = { open: true, close() { this.open = false; } };
    const messages = []; let calls = 0;
    context.toast = message => messages.push(message);
    context.refresh = async () => { throw new Error('state offline'); };
    context.api = async () => { calls++; return kind === 'batch' ? [{ id: 'added' }] : { updated: 1 }; };
    if (kind === 'group') { context.openServerHostGroup(); context.elements['#serverGroupInput'].value = 'pve'; }
    const fn = kind === 'single' ? 'saveServerForm' : kind === 'batch' ? 'saveBatchServerForm' : 'saveServerHostGroup';
    await context[fn]({ preventDefault() {}, currentTarget: form });
    assert.equal(calls, 1);
    assert.match(messages.at(-1), /已成功.*状态刷新失败.*state offline/);
    assert.equal(context.elements[kind === 'group' ? '#serverHostGroupDialog' : '#serverEditor'].open, false);
    assert.equal(context.elements[kind === 'group' ? '#serverGroupError' : '#serverEditorError'].hidden, true);
    await context[fn]({ preventDefault() {}, currentTarget: form });
    assert.equal(calls, 1, 'completed operation must not replay after failed refresh');
  }
});

test('single server requests block duplicate or conflicting actions and all single mutations respect pending bulk operation', async () => {
  const context = ui(); fixture(context); selected(context, ['s1']);
  const calls = []; let finish;
  context.api = (url, options) => { calls.push([url, options.method]); return new Promise(resolve => { finish = resolve; }); };
  const pending = context.deleteServer('s1');
  await context.deleteServer('s1'); await context.checkServer('s1'); await context.trustServer('s1'); await context.batchTrustServers();
  assert.deepEqual(calls, [['/servers/s1', 'DELETE']]);
  finish({}); await pending;
  vm.runInContext("serverBatchPending = 'trust'", context);
  context.confirm = () => assert.fail('single mutation must not prompt while batch runs');
  await context.deleteServer('s2'); await context.checkServer('s2'); await context.trustServer('s2');
  assert.equal(calls.length, 1);
});
