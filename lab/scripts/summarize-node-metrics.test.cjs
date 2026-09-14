const {test}=require('node:test'),assert=require('node:assert/strict');
const {summarize,percentile}=require('./summarize-node-metrics.cjs');
function fixture(){
 const tx=[{id:'t1',type:'transfer',runPhase:'measuring',status:'confirmed',hash:'tx1',toNode:'receiver',blockNumber:'1'}];
 const blocks=[{number:1,hash:'b1',parentHash:'b0',timestamp:100,size:1000},{number:2,hash:'b2',parentHash:'b1',timestamp:110,size:2000},{number:3,hash:'b3',parentHash:'b2',timestamp:120,size:9000}];
 const record=(kind,hash,ns,extra={})=>({session:'one',kind,hash,ns,...extra});
 return {report:{runs:[{completeWindow:true,blocks}],transactions:tx},w:{status:'completed',measurementStartedAt:new Date(100000).toISOString(),measurementEndsAt:new Date(120000).toISOString()},nodes:[{id:'m1',isMiner:true},{id:'m2',isMiner:true}],records:{receiver:[record('inclusion','tx1',2e9,{block:'b1'})],m1:[record('admission','tx1',2e6),record('admission','tx1',900e6),record('block-validation','b1',4e6),record('block-validation','b2',8e6)],m2:[record('admission','tx1',4e6),record('block-validation','b1',8e6)]}};
}
test('eight metrics use receiver monotonic sample and equal per-block weighting',()=>{
 const f=fixture(),r=summarize(f.report,f.w,f.nodes,f.records);
 assert.equal(r.completeSamples,true);assert.equal(Object.keys(r.metrics).length,8);
 assert.deepEqual(r.metrics,{meanFirstInclusionSeconds:2,p90FirstInclusionSeconds:2,p95FirstInclusionSeconds:2,meanMinerAdmissionMs:3,meanBlockIntervalSeconds:10,canonicalBlockCount:2,meanCanonicalBlockSizeB:1500,meanMinerBlockValidationMs:7});
});
test('missing samples are explicit, never fabricated as zero',()=>{
 const f=fixture();f.records.receiver=[];f.records.m2=[];const r=summarize(f.report,f.w,f.nodes,f.records);assert.equal(r.completeSamples,false);assert.equal(r.metrics.meanFirstInclusionSeconds,null);assert.deepEqual(r.coverage.missingDelays,['t1']);
});
test('reorg, restarts, incomplete window and chain gaps reject the simplified result',()=>{
 for(const change of [f=>f.records.receiver.push({kind:'inclusion',session:'one',hash:'tx1',block:'orphan',ns:1}),f=>f.records.receiver.push({kind:'broadcast',session:'two'}),f=>f.report.runs[0].completeWindow=false,f=>f.report.runs[0].blocks[1].parentHash='wrong']){const f=fixture();change(f);assert.throws(()=>summarize(f.report,f.w,f.nodes,f.records));}
});
test('percentiles preserve documented zero-based floor convention',()=>{assert.equal(percentile([4,1,3,2],.9),3);assert.equal(percentile([],.95),null);});
test('header-time window requires observations on both boundaries',()=>{const f=fixture();f.report.runs[0].blocks.pop();assert.throws(()=>summarize(f.report,f.w,f.nodes,f.records),/bracket/);});
test('alpha zero uses Public sender timing and preserves zero rather than defaulting to 100',()=>{
 const f=fixture();Object.assign(f.w,{type:'mixed',transferPercent:0,nodeIds:['sender','receiver']});
 Object.assign(f.report.transactions[0],{type:'public',fromNode:'sender'});
 f.records.sender=f.records.receiver;f.records.receiver=[];
 const r=summarize(f.report,f.w,f.nodes,f.records);
 assert.equal(r.completeSamples,true);assert.equal(r.metrics.meanFirstInclusionSeconds,2);
 assert.equal(r.mixture.targetTransferPercent,0);assert.equal(r.mixture.actualTransferPercent,0);
 assert.equal(r.mixture.perNode.sender.public,1);assert.equal(r.mixture.perNode.receiver.transferPercent,null);
 assert.equal(r.typeMetrics.transfer.meanMinerAdmissionMs,null);
});
