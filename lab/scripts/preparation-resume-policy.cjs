function requirePreparationOnly(s,eid,marker,sha,alpha){
 const e=s.experiments.find(e=>e.id===eid);
 if(!e||e.status!=='running'||e.artifactSha!==sha)throw Error('Resume requires existing running network with matching runtime');
 if(marker.eid!==eid||marker.runtimeSha!==sha||marker.transferPercent!==alpha)throw Error('Preparation marker differs');
 if(s.workloads.some(w=>w.experimentId===eid))throw Error('A workload already exists; never restart it through preparation resume');
 const tx=s.transactions.filter(t=>t.experimentId===eid);
 if(tx.some(t=>t.status!=='confirmed'||t.error||!t.hash||t.runPhase||!['public','createAccount','mint'].includes(t.type)))throw Error('Unresolved or non-preparation transactions; no replay');
 if(s.experiments.some(x=>x.id!==eid&&['running','deploying','resuming','stopping'].includes(x.status)))throw Error('Another experiment is active');
 return e;
}
function requireFinishedWarmup(s,eid,marker,sha,alpha,runId,priorRunIds=[]){
 const e=s.experiments.find(e=>e.id===eid),runs=s.workloads.filter(w=>w.experimentId===eid);
 if(!e||e.status!=='running'||e.artifactSha!==sha||marker.eid!==eid||marker.runtimeSha!==sha||marker.transferPercent!==alpha)throw Error('Warmup restart network or marker differs');
 const allowed=new Set([...priorRunIds,runId]),zero=t=>!t||t.startsWith('0001-');
 if(allowed.size!==priorRunIds.length+1||runs.length!==allowed.size||runs.some(w=>!allowed.has(w.id)))throw Error('Every preceding warmup must be explicitly identified in retained evidence');
 // The controller can stop on its warmup deadline without a user stop flag.
 // Admit only this exact reason, with no measured phase and all txs ready below.
 const safelyStopped=w=>w.stopRequested||w.invalidReason==='预热超时：不是所有参与账户都完成了安全交易';
 if(runs.some(w=>w.type!=='mixed'||w.transferPercent!==alpha||!safelyStopped(w)||w.status!=='completed-with-errors'||!zero(w.measurementStartedAt)||!zero(w.measurementEndsAt)||w.blockError))throw Error('Prior workload is not a fully drained pre-measurement warmup');
 const tx=s.transactions.filter(t=>t.experimentId===eid);
 if(tx.some(t=>t.status!=='confirmed'||t.error||!t.hash||(t.workloadId&&(!allowed.has(t.workloadId)||t.runPhase!=='warmup'||zero(t.readyAt)))))throw Error('Unresolved or measured transactions cannot be replayed');
 if(s.experiments.some(x=>x.id!==eid&&['running','deploying','resuming','stopping'].includes(x.status)))throw Error('Another experiment is active');
 return e;
}
// A new window may reuse a drained network, never reuse/rewrite an old run.
// Only explicitly audited pre-proof rejections can be excluded from readiness.
function requireDrainedAttempt(s,eid,marker,sha,alpha,runId,rejectedIds,priorRunIds=[]){
 const zero=t=>!t||t.startsWith('0001-');
 const e=s.experiments.find(e=>e.id===eid),runs=s.workloads.filter(w=>w.experimentId===eid);
 if(!e||e.status!=='running'||e.artifactSha!==sha||marker.eid!==eid||marker.runtimeSha!==sha||marker.transferPercent!==alpha)throw Error('Drained attempt network or marker differs');
 const allowed=new Set([...priorRunIds,runId]);
 if(allowed.size!==priorRunIds.length+1||runs.length!==allowed.size||runs.some(w=>!allowed.has(w.id)||w.type!=='mixed'||w.transferPercent!==alpha||w.status!=='completed-with-errors'||!w.stopRequested||w.blockError))throw Error('Attempt is not fully drained');
 // An empty explicit list is valid only when EVERY transaction below is
 // confirmed and ready (for example, a drained readiness-timeout attempt).
 if(!Array.isArray(rejectedIds)||new Set(rejectedIds).size!==rejectedIds.length)throw Error('Explicit rejected transaction IDs required');
 const tx=s.transactions.filter(t=>t.experimentId===eid),rejected=new Set(rejectedIds);
 if(rejectedIds.some(id=>!tx.some(t=>t.id===id)))throw Error('Rejected transaction missing');
 for(const t of tx){
  if(rejected.has(t.id)){
   const expected=t.type==='transfer'?'未开始付款方证明：节点不可达或正在恢复，请等待节点恢复后再发送交易':t.type==='public'?'未发送交易：节点不可达或正在恢复，请等待节点恢复后再发送交易':null;
   // This exact wrapper is produced before beginTransactionRPC succeeds.
   // Still require explicit IDs and absence of every execution marker; a
   // generic context deadline (possibly after submission) is NOT safe.
   const admissionTimeout=t.type==='public'&&t.error==='未发送交易：admission wait ended (last rejection: 节点不可达或正在恢复，请等待节点恢复后再发送交易): context deadline exceeded';
   if(!allowed.has(t.workloadId)||!expected||t.status!=='failed'||t.hash||t.executionStage||!zero(t.provingAt)||!zero(t.submissionAttemptedAt)||!zero(t.broadcastAt)||t.receipt||(t.error!==expected&&!admissionTimeout))throw Error('Rejection may have executed; no reuse');
  }else if(t.status!=='confirmed'||t.error||!t.hash||(t.workloadId&&(!allowed.has(t.workloadId)||zero(t.readyAt))))throw Error('Unresolved transaction; no reuse');
 }
 if(s.experiments.some(x=>x.id!==eid&&['running','deploying','resuming','stopping'].includes(x.status)))throw Error('Another experiment is active');
 return e;
}
module.exports={requirePreparationOnly,requireFinishedWarmup,requireDrainedAttempt};
