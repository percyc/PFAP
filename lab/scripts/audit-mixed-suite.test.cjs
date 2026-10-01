const {test}=require('node:test'),assert=require('node:assert/strict'),{validateGroup,rows}=require('./audit-mixed-suite.cjs');
function fixture(){
 const nodes=Array.from({length:100},(_,i)=>({id:'n'+i,serverId:'s'+i,isMiner:i<5,runtimeSha:'sha'}));
 const report={experiment:{id:'e',artifactSha:'sha',status:'stopped',minerCount:5,nodes,placements:nodes.map(n=>({serverId:n.serverId,count:1})),minerSelections:nodes.slice(0,5).map(n=>({serverId:n.serverId,localIndex:1}))},runs:[{completeWindow:true,configuration:{id:'w',status:'completed',durationSeconds:3600,confirmations:6,nodeIds:nodes.slice(6).map(n=>n.id),observerNodeId:'n5'}}],transactions:[{type:'public',runPhase:'measuring',status:'confirmed',hash:'tx'}]};
 const summary={experimentId:'e',runId:'w',measurementStartedAt:'2026-01-01T00:00:00Z',measurementEndsAt:'2026-01-01T01:00:00Z',completeSamples:true,mixture:{targetTransferPercent:0,counts:{public:1,transfer:0}},metrics:Object.fromEntries(rows.map(([,k])=>[k,1])),coverage:{formalTransactions:1,firstInclusionSamples:1,admissionTransactions:1,admissionSamples:5,minerCount:5,validatedBlocks:1,canonicalBlocks:1}};
 summary.metrics.canonicalBlockCount=2;summary.coverage.validatedBlocks=2;summary.coverage.canonicalBlocks=2;
 Object.assign(summary,{schemaVersion:3,latencyPolicy:'first-local-canonical-inclusion'});
 return {report,summary};
}
test('one-hour 100-process alpha-zero baseline audit',()=>{const f=fixture();assert.equal(validateGroup(0,f.summary,f.report).placements.length,100);});
test('partial scope or incomplete evidence cannot pass',()=>{
 for(const change of [f=>f.summary.measurementEndsAt='2026-01-01T00:59:59Z',f=>f.report.experiment.nodes.pop(),f=>f.report.runs[0].completeWindow=false,f=>f.report.transactions[0].status='unknown',f=>f.summary.coverage.admissionSamples=4,f=>f.summary.metrics.meanMinerAdmissionMs=null,f=>f.report.experiment.nodes[9].runtimeSha='other',f=>f.report.runs[0].configuration.nodeIds[0]='n5']){const f=fixture();change(f);assert.throws(()=>validateGroup(0,f.summary,f.report));}
});
