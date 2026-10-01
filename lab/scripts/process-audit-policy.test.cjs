const test = require('node:test');
const assert = require('node:assert/strict');
const {requireStoppedProcessAudit} = require('./process-audit-policy.cjs');
const servers = Array.from({length:100}, (_,i) => 'srv-' + i);
const good = () => ({experimentId:'exp-abc',readOnly:true,results:servers.map(serverId =>
  ({serverId,error:null,processes:[],memoryKiB:{MemTotal:8192000}}))});
test('accept only a complete, successful empty-process inventory', () => {
  assert.doesNotThrow(() => requireStoppedProcessAudit(good(),'exp-abc',servers));
  const mutations = [
    r => {r.experimentId='exp-other';}, r => {r.readOnly=false;},
    r => r.results.pop(), r => r.results.push({...r.results[0]}),
    r => {r.results[1].serverId=r.results[0].serverId;},
    r => {r.results[1].serverId='foreign';},
    r => {r.results[0].error='Inspection timed out';},
    r => {r.results[0].error='Inspection exit 255';},
    r => {delete r.results[0].error;},
    r => {r.results[0].processes=[{pid:42,classification:'target-experiment'}];},
    r => {r.results[0].processes=[{pid:42,classification:'unrecognized'}];},
    r => {delete r.results[0].processes;},
    r => {r.results[0].memoryKiB={};},
    r => {r.results[0].memoryKiB.MemTotal=0;},
  ];
  for(const mutate of mutations){const r=good();mutate(r);assert.throws(() => requireStoppedProcessAudit(r,'exp-abc',servers));}
  assert.throws(() => requireStoppedProcessAudit(good(),'exp-abc',servers.slice(1)));
});
