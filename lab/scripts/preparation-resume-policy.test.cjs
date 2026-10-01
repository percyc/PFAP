const {test}=require('node:test'),assert=require('node:assert/strict'),{requirePreparationOnly}=require('./preparation-resume-policy.cjs');
const fixture=()=>({experiments:[{id:'e',status:'running',artifactSha:'sha'}],workloads:[],transactions:[{experimentId:'e',status:'confirmed',type:'public',hash:'h'}]});
const marker={eid:'e',runtimeSha:'sha',transferPercent:0};
test('new window after drained attempt accepts only explicitly identified pre-proof rejections',()=>{
 const {requireDrainedAttempt}=require('./preparation-resume-policy.cjs');
 const make=()=>({...fixture(),workloads:[{id:'w',experimentId:'e',type:'mixed',transferPercent:0,status:'completed-with-errors',stopRequested:true}],transactions:[{id:'rejected',experimentId:'e',workloadId:'w',type:'transfer',status:'failed',error:'未开始付款方证明：节点不可达或正在恢复，请等待节点恢复后再发送交易'},{id:'ok',experimentId:'e',workloadId:'w',status:'confirmed',hash:'h',readyAt:'2026-09-16T00:00:00Z'}]});
 assert.equal(requireDrainedAttempt(make(),'e',marker,'sha',0,'w',['rejected']).id,'e');
 for(const change of [s=>s.workloads[0].status='draining',s=>s.workloads[0].stopRequested=false,s=>s.workloads[0].blockError='gap',s=>s.transactions[0].status='unknown',s=>s.transactions[0].provingAt='2026-09-16T00:00:00Z',s=>s.transactions[0].submissionAttemptedAt='2026-09-16T00:00:00Z',s=>s.transactions[0].broadcastAt='2026-09-16T00:00:00Z',s=>s.transactions[0].executionStage='payer-proof',s=>s.transactions[0].hash='h',s=>s.transactions[0].receipt='{}',s=>s.transactions[0].error='different failure',s=>s.transactions[1].readyAt='',s=>s.transactions[1].status='settling',s=>s.workloads.push({...s.workloads[0],id:'other'})]){const s=make();change(s);assert.throws(()=>requireDrainedAttempt(s,'e',marker,'sha',0,'w',['rejected']));}
 assert.throws(()=>requireDrainedAttempt(make(),'e',marker,'sha',0,'w',[]));
 const drained=make();drained.transactions=drained.transactions.filter(t=>t.id==='ok');
 assert.equal(requireDrainedAttempt(drained,'e',marker,'sha',0,'w',[]).id,'e');
 for(const change of [s=>s.transactions[0].status='settling',s=>s.transactions[0].error='waiting',s=>s.transactions[0].readyAt='',s=>s.workloads[0].status='draining']){const x=structuredClone(drained);change(x);assert.throws(()=>requireDrainedAttempt(x,'e',marker,'sha',0,'w',[]));}
 assert.throws(()=>requireDrainedAttempt(make(),'e',marker,'sha',0,'w',['missing']));
 const multi=make();
 multi.workloads.push({...multi.workloads[0],id:'w2'});
 multi.transactions.push({...multi.transactions[0],id:'public-rejected',workloadId:'w2',type:'public',error:'未发送交易：节点不可达或正在恢复，请等待节点恢复后再发送交易'});
 assert.equal(requireDrainedAttempt(multi,'e',marker,'sha',0,'w2',['rejected','public-rejected'],['w']).id,'e');
 assert.throws(()=>requireDrainedAttempt(multi,'e',marker,'sha',0,'w2',['rejected','public-rejected']));
 assert.throws(()=>requireDrainedAttempt(multi,'e',marker,'sha',0,'w2',['public-rejected'],['w']));
 multi.transactions[2].submissionAttemptedAt='2026-09-16T00:00:00Z';
 assert.throws(()=>requireDrainedAttempt(multi,'e',marker,'sha',0,'w2',['rejected','public-rejected'],['w']));
});
test('resume reuses confirmed preparation and preserves alpha zero',()=>assert.equal(requirePreparationOnly(fixture(),'e',marker,'sha',0).id,'e'));
test('audited public pre-admission timeout is reusable only without execution evidence',()=>{
 const {requireDrainedAttempt}=require('./preparation-resume-policy.cjs');
 const s={...fixture(),workloads:[{id:'w',experimentId:'e',type:'mixed',transferPercent:0,status:'completed-with-errors',stopRequested:true}],transactions:[{id:'rejected',experimentId:'e',workloadId:'w',type:'public',status:'failed',error:'未发送交易：admission wait ended (last rejection: 节点不可达或正在恢复，请等待节点恢复后再发送交易): context deadline exceeded'}]};
 assert.equal(requireDrainedAttempt(s,'e',marker,'sha',0,'w',['rejected']).id,'e');
 for(const [key,value] of Object.entries({type:'transfer',status:'unknown',hash:'h',executionStage:'submit',provingAt:'2026-09-18',submissionAttemptedAt:'2026-09-18',broadcastAt:'2026-09-18',receipt:'{}',error:'未发送交易：context deadline exceeded'})){
  const changed=structuredClone(s);changed.transactions[0][key]=value;
  assert.throws(()=>requireDrainedAttempt(changed,'e',marker,'sha',0,'w',['rejected']));
 }
 assert.throws(()=>requireDrainedAttempt(s,'e',marker,'sha',0,'w',[]));
});
test('unknown sends, existing runs, foreign workloads and marker changes fail closed',()=>{
 for(const change of [s=>s.transactions[0].status='unknown',s=>s.transactions[0].hash='',s=>s.transactions[0].type='transfer',s=>s.workloads.push({experimentId:'e',status:'completed'}),s=>s.experiments[0].status='stopped',s=>s.experiments.push({id:'other',status:'running'})]){const s=fixture();change(s);assert.throws(()=>requirePreparationOnly(s,'e',marker,'sha',0));}
 assert.throws(()=>requirePreparationOnly(fixture(),'e',marker,'sha',20));
});
test('warmup restart requires identified, fully drained, never-measured run',()=>{
 const {requireFinishedWarmup}=require('./preparation-resume-policy.cjs');
 const make=()=>({...fixture(),workloads:[{id:'w',experimentId:'e',type:'mixed',transferPercent:0,status:'completed-with-errors',stopRequested:true,measurementStartedAt:'0001-01-01T00:00:00Z'}],transactions:[{experimentId:'e',workloadId:'w',runPhase:'warmup',status:'confirmed',hash:'h',readyAt:'2026-09-15T00:00:00Z'}]});
 assert.equal(requireFinishedWarmup(make(),'e',marker,'sha',0,'w').id,'e');
 const prior=make();prior.workloads.push({...prior.workloads[0],id:'older'});
 assert.throws(()=>requireFinishedWarmup(prior,'e',marker,'sha',0,'w'));
 assert.equal(requireFinishedWarmup(prior,'e',marker,'sha',0,'w',['older']).id,'e');
 prior.workloads[1].measurementStartedAt='2026-09-15T00:00:00Z';
 assert.throws(()=>requireFinishedWarmup(prior,'e',marker,'sha',0,'w',['older']));
 for(const change of [s=>s.workloads[0].status='draining',s=>s.workloads[0].stopRequested=false,s=>s.workloads[0].measurementStartedAt='2026-09-15T00:00:00Z',s=>s.workloads[0].blockError='gap',s=>s.transactions[0].status='unknown',s=>s.transactions[0].readyAt='',s=>s.transactions[0].runPhase='measuring',s=>s.workloads.push({...s.workloads[0],id:'another'})]){const s=make();change(s);assert.throws(()=>requireFinishedWarmup(s,'e',marker,'sha',0,'w'));}
});
test('automatic warmup timeout may reuse only fully confirmed unmeasured accounts',()=>{
 const {requireFinishedWarmup}=require('./preparation-resume-policy.cjs');
 const make=()=>({...fixture(),workloads:[{id:'w',experimentId:'e',type:'mixed',transferPercent:0,status:'completed-with-errors',stopRequested:false,invalidReason:'预热超时：不是所有参与账户都完成了安全交易'}],transactions:[{experimentId:'e',workloadId:'w',runPhase:'warmup',status:'confirmed',hash:'h',readyAt:'2026-09-18T00:00:00Z'}]});
 assert.equal(requireFinishedWarmup(make(),'e',marker,'sha',0,'w').id,'e');
 for(const change of [s=>s.workloads[0].invalidReason='other failure',s=>s.workloads[0].status='draining',s=>s.workloads[0].measurementStartedAt='2026-09-18T00:00:00Z',s=>s.workloads[0].measurementEndsAt='2026-09-18T01:00:00Z',s=>s.workloads[0].blockError='gap',s=>s.transactions[0].status='unknown',s=>s.transactions[0].readyAt='',s=>s.transactions[0].error='failure',s=>s.transactions[0].hash='',s=>s.transactions[0].runPhase='measuring']){const s=make();change(s);assert.throws(()=>requireFinishedWarmup(s,'e',marker,'sha',0,'w'));}
});
