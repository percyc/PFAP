const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const test = require('node:test');

const source = fs.readFileSync(path.join(__dirname, 'web/app.js'), 'utf8');
const functionsOnly = source.slice(0, source.indexOf("document.querySelectorAll('nav button').forEach"));

function ui(elements = {}) {
  const context = vm.createContext({
    document: { querySelector: selector => elements[selector] },
    setTimeout,
    clearTimeout,
  });
  vm.runInContext(functionsOnly, context);
  return context;
}

test('legacy experiments retain one miner and undeployed placements count correctly', () => {
  const context = ui();
  assert.equal(context.configuredMinerCount({}), 1);
  assert.equal(context.experimentNodeCount({ placements: [{ count: 2 }, { count: 3 }] }), 5);
  assert.equal(context.nodeIsMiner({}, { index: 1 }), true);
  assert.equal(context.nodeIsMiner({}, { index: 2 }), false);
  assert.equal(context.nodeIsMiner({}, { index: 1, isMiner: false }), false);
});

test('actual miner count excludes offline and unobserved nodes, and detects extra mining nodes', () => {
  const context = ui();
  const experiment = {
    minerCount: 3,
    nodes: [
      { isMiner: true, status: 'running', mining: true },
      { isMiner: true, status: 'unreachable', mining: true },
      { isMiner: true, status: 'running' },
      { isMiner: false, status: 'running', mining: true },
    ],
  };
  assert.deepEqual(JSON.parse(JSON.stringify(context.miningStats(experiment))), { target: 3, active: 2, unknown: 2 });
  assert.match(context.nodeMiningLabel(experiment, experiment.nodes[1]), /挖矿状态未知/);
  assert.match(context.nodeMiningLabel(experiment, experiment.nodes[3]), /普通节点 · 正在挖矿/);
});

test('new experiment defaults to two miners when capacity allows and preserves a user edit', () => {
  const minerInput = { value: '1', setCustomValidity(value) { this.validation = value; } };
  const countInput = { value: '1' };
  const serverSelect = { selectedOptions: [{ value: 'a' }, { value: 'b' }] };
  const preview = { innerHTML: '' };
  const context = ui({
    '#experimentForm': { elements: { minerCount: minerInput, count: countInput } },
    '#serverSelect': serverSelect,
    '#minerPlacementPreview': preview,
  });
  context.updateMinerPlacement();
  assert.equal(minerInput.value, '2');
  assert.equal(minerInput.max, '2');
  assert.equal(minerInput.validation, '');

  vm.runInContext('newMinerCountEdited = true', context);
  minerInput.value = '3';
  context.updateMinerPlacement();
  assert.equal(minerInput.value, '3', 'capacity changes must not silently overwrite an edited value');
  assert.match(minerInput.validation, /1 至 2/);

  countInput.value = '2';
  context.updateMinerPlacement();
  assert.equal(minerInput.max, '4');
  assert.equal(minerInput.value, '3');
  assert.equal(minerInput.validation, '');

  serverSelect.selectedOptions = [{ value: 'a' }];
  minerInput.value = '2';
  context.updateMinerPlacement();
  assert.match(preview.innerHTML, /同一台服务器/);
});

test('new experiment rejects zero capacity and fractional or excessive miners', () => {
  const minerInput = { value: '1', setCustomValidity(value) { this.validation = value; } };
  const serverSelect = { selectedOptions: [] };
  const context = ui({
    '#experimentForm': { elements: { minerCount: minerInput, count: { value: '1' } } },
    '#serverSelect': serverSelect,
    '#minerPlacementPreview': { innerHTML: '' },
  });
  context.updateMinerPlacement();
  assert.notEqual(minerInput.validation, '');
  serverSelect.selectedOptions = [{ value: 'a' }];
  context.updateMinerPlacement();
  assert.equal(minerInput.value, '1');
  assert.equal(minerInput.validation, '');
  vm.runInContext('newMinerCountEdited = true', context);
  for (const value of ['0', '1.5', '2', '']) {
    minerInput.value = value;
    context.updateMinerPlacement();
    assert.notEqual(minerInput.validation, '', `reject ${JSON.stringify(value)}`);
  }
});

test('existing configuration retains drafts, allows failed same-count retry, and blocks conflicting operations', () => {
  const context = ui();
  const experiment = { id: 'exp-1', status: 'running', minerCount: 2, placements: [{ count: 4 }], nodes: [] };
  vm.runInContext("minerDrafts.set('exp-1', '3')", context);
  assert.match(context.minerConfiguration(experiment), /value="3"/);
  vm.runInContext('minerDrafts.clear()', context);
  assert.match(context.minerConfiguration(experiment), /miner-apply" disabled/);
  const failed = { ...experiment, miningStatus: 'failed', miningError: 'offline node' };
  assert.match(context.minerConfiguration(failed), /miner-apply" >重试应用/);
  const busy = { ...experiment, miningStatus: 'updating' };
  assert.match(context.minerConfiguration(busy), /miner-apply" disabled>调整中/);
  assert.match(context.experimentActions(busy), /disabled title="请等待矿工配置调整完成"/);
});

test('running miner change is explicit, numeric, and does not invoke deploy', async () => {
  const context = ui();
  const requests = [];
  let confirmation = '';
  context.confirm = message => { confirmation = message; return true; };
  context.api = async (url, options) => {
    requests.push({ url, body: JSON.parse(options.body) });
    return { miningStatus: 'updating' };
  };
  context.toast = () => {};
  context.render = () => {};
  context.refresh = async () => {};
  vm.runInContext("state.experiments = [{id:'exp-1',name:'Test',status:'running',minerCount:1}]", context);
  await context.applyMinerCount({ preventDefault() {}, currentTarget: { elements: { minerCount: { value: '2' } }, reportValidity: () => true } }, 'exp-1');
  assert.deepEqual(requests, [{ url: '/experiments/exp-1/miners', body: { minerCount: 2 } }]);
  assert.match(confirmation, /不会重启节点、清空数据或重发交易/);
  assert.match(confirmation, /总算力与出块节奏可能变化/);
});

test('same-count reapply detects wrong-role mining even when the active total matches', () => {
  const context = ui();
  const experiment = {
    id: 'exp-1', status: 'running', minerCount: 1,
    nodes: [
      { id: 'n1', isMiner: true, status: 'running', mining: false },
      { id: 'n2', isMiner: false, status: 'running', mining: true },
    ],
  };
  assert.equal(context.miningStats(experiment).active, 1);
  assert.equal(context.miningNeedsReapply(experiment), true);
  assert.match(context.miningHealth(experiment), /miner-health-warning/);
  assert.match(context.minerConfiguration(experiment), /miner-apply" >重新应用/);
  const correct = { ...experiment, nodes: experiment.nodes.map(n => ({ ...n, mining: n.isMiner })) };
  assert.equal(context.miningNeedsReapply(correct), false);
  assert.match(context.minerConfiguration(correct), /miner-apply" disabled/);
  const unknown = { ...experiment, nodes: [{ id: 'n1', status: 'running', isMiner: true }] };
  assert.equal(context.miningNeedsReapply(unknown), true);
  assert.match(context.minerConfiguration(unknown), /miner-apply" >重新应用/);
});

test('miner application disables recovery immediately and account cards refresh during the operation', async () => {
  const elements = { '#experimentSelect': { value: 'exp-1' }, '#accountCards': {}, '#accountHistory': {} };
  const context = ui(elements);
  vm.runInContext("state.experiments = [{id:'exp-1',status:'running',minerCount:1,nodes:[{id:'n1',name:'node-1',status:'unreachable',isMiner:true}]}]", context);
  context.renderAccounts();
  assert.match(elements['#accountCards'].innerHTML, /node-recover"[^>]* title="保留现有数据/);
  assert.doesNotMatch(elements['#accountCards'].innerHTML, /node-recover"[^>]* disabled/);

  vm.runInContext("minerRequests.add('exp-1')", context);
  context.renderAccounts();
  assert.match(elements['#accountCards'].innerHTML, /node-recover"[^>]* disabled title="请等待矿工配置调整完成"/);

  vm.runInContext("minerRequests.clear(); state.experiments[0].miningStatus = 'updating'", context);
  context.renderAccounts();
  assert.match(elements['#accountCards'].innerHTML, /node-recover"[^>]* disabled title="请等待矿工配置调整完成"/);
  context.confirm = () => { assert.fail('recovery must not prompt while mining is updating'); };
  await context.recoverNode('exp-1', 'n1');

  vm.runInContext("state.experiments[0].miningStatus = ''", context);
  context.renderAccounts();
  assert.doesNotMatch(elements['#accountCards'].innerHTML, /node-recover"[^>]* disabled/);
});
