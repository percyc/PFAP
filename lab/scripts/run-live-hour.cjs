// Explicit one-shot live experiment runner. Never retries writes or unknown txs.
// Usage: PFAP_RUNTIME_SHA=verified_sha node lab/scripts/run-live-hour.cjs EXPERIMENT_ID OUTPUT_DIRECTORY
const fs = require('node:fs');
const path = require('node:path');
const {spawn} = require('node:child_process');
process.chdir(path.resolve(__dirname,'../..'));
const {runtimeSHA,mixedPercent,preparationBatches,validateLayout,requireExclusiveServers,networkProgress}=require('./live-hour-policy.cjs');
const {startRunWhenIdle}=require('./run-admission.cjs');
const {requireStoppedProcessAudit}=require('./process-audit-policy.cjs');
const sha=runtimeSHA(process.env.PFAP_RUNTIME_SHA);
const alpha=mixedPercent(process.env.PFAP_TRANSFER_PERCENT);
const [eid, output, mode] = process.argv.slice(2);
if(mode && !['--resume-preparation','--prepared-run'].includes(mode)) throw Error('Unknown mode');
const resume=mode==='--resume-preparation';
const prepared=mode==='--prepared-run';
if (!/^exp-[a-f0-9]+$/.test(eid || '') || !output) throw Error('Specify experiment ID and output directory');
fs.mkdirSync(output, {recursive:true});
const marker=path.join(output,'started.json');
if(resume){
 const prior=fs.existsSync(marker)?JSON.parse(fs.readFileSync(marker)):null;
 if(!prior||prior.eid!==eid||prior.runtimeSha!==sha||(prior.transferPercent??null)!==alpha)throw Error('Resume marker missing or experiment/runtime/mixture differs');
}else{
 if(fs.existsSync(marker)) throw Error('Already started: inspect persisted progress; do not replay');
 fs.writeFileSync(marker,JSON.stringify({eid,runtimeSha:sha,transferPercent:alpha,publicGasPriceWei:'20000000000',publicGasLimit:21000,mixedScheduler:alpha===null?null:'broadcaster-five-slot-quota/oldest-own-admission/largest-balance-payer-v1',preparation:prepared?{mode:'reuse-confirmed-accounts'}:{mode:'fresh',publicFundingWorkers:5,createAccountWorkers:alpha===null?5:20,mintWorkers:alpha===null?1:20,maxProofWorkersPerHostGroup:alpha===null?5:4},startedAt:new Date().toISOString()}),{flag:'wx'});
}
fs.writeFileSync(path.join(output,'experiment-id.txt'),eid);
const base='http://127.0.0.1:8090';
let cookie;
let acceptedRun;
const log=(stage,details={})=>console.log(JSON.stringify({at:new Date().toISOString(),stage,...details}));
const pause=ms=>new Promise(r=>setTimeout(r,ms));
async function request(url,body){
 const r=await fetch(base+url,{method:body===undefined?'GET':'POST',headers:{cookie,'Content-Type':'application/json'},body:body===undefined?undefined:JSON.stringify(body),signal:AbortSignal.timeout(180000)});
 if(!r.ok)throw Error('HTTP '+r.status+' '+url+' '+(await r.text()).slice(0,500));
 return r;
}
const api=async(url,body)=>(await request('/api'+url,body)).json();
const state=()=>api('/state');
function experiment(s){const e=s.experiments.find(e=>e.id===eid);if(!e)throw Error('Missing experiment');return e;}
async function wait(stage,fn,seconds){
 const deadline=Date.now()+seconds*1000;let previous='';let last=0;
 while(Date.now()<deadline){const result=await fn();if(result===true)return;
  const text=JSON.stringify(result);if(text!==previous||Date.now()-last>60000){log(stage,result);previous=text;last=Date.now();}await pause(10000);
 }throw Error(stage+' timed out; no automatic resubmission');
}
async function refresh(id){await api(`/experiments/${eid}/nodes/${id}/state`);}
async function tx(type,from,to,value){
 await refresh(from);if(to)await refresh(to);
 const prior=(await api('/transactions')).filter(t=>t.experimentId===eid&&t.type===type&&t.fromNode===from&&(t.toNode||'')===(to||''));
 if(prior.length>1||prior.some(t=>t.status!=='confirmed'||t.error||!t.hash||(type!=='createAccount'&&BigInt(t.value)!==BigInt(value))))throw Error('Preparation record requires inspection; no retry');
 if(prior.length&&!resume)throw Error('Unexpected existing preparation');
 const t=prior[0]||await api('/transactions',{experimentId:eid,type,fromNode:from,toNode:to,value});
 if(prior.length)log('reuse-confirmed-preparation',{id:t.id,type});
 log('transaction',{id:t.id,type,from,to});
 await wait('confirmation',async()=>{
  const s=await state(),e=experiment(s),current=s.transactions.find(v=>v.id===t.id);
  if(!current)throw Error('Missing accepted transaction');
  if(['unknown','failed','timeout','cancelled','canceled'].includes(current.status)||current.error)throw Error(t.id+' '+current.status+' '+current.error);
  if(current.status==='confirmed'){
   const height=Number(BigInt(current.blockNumber));
   if(e.nodes.every(n=>n.status==='running'&&n.block>=height+5))return true;
  }return {id:t.id,status:current.status};
 },1800);
 await refresh(from);if(to)await refresh(to);
}
async function batch(jobs){
 const results=await Promise.allSettled(jobs.map(f=>f()));
 const failures=results.filter(v=>v.status==='rejected');
 if(failures.length)throw Error(failures.map(v=>v.reason.message).join('; '));
}
async function exports(run){
 const errors=[];
 for(const [name,url] of [['experiment','/experiments/'+eid],...(run?[['run','/workloads/'+run]]:[])]){
  for(const format of ['json','html','csv']){
   try {
    const r=await request('/api'+url+'/report?format='+format);
    fs.writeFileSync(path.join(output,name+'.'+(format==='csv'?'zip':format)),Buffer.from(await r.arrayBuffer()));
   } catch(e) { errors.push(name+'/'+format+': '+e.message); }
  }
 }
 if(errors.length)throw Error(errors.join('; '));
}
async function auditStoppedProcesses(){
 const reportPath=path.resolve(output,'process-audit-after-stop.json');
 await new Promise((resolve,reject)=>{
  const child=spawn(process.execPath,['lab/scripts/audit-node-processes.cjs',eid,reportPath],{stdio:'inherit'});
  child.once('error',reject);
  child.once('exit',(code,signal)=>code===0?resolve():reject(Error('Stopped process audit failed: '+(signal||code))));
 });
 const report=JSON.parse(fs.readFileSync(reportPath));
 requireStoppedProcessAudit(report,eid,experiment(await state()).nodes.map(n=>n.serverId));
 log('process-audit-complete',{servers:100,remainingGeth:0,report:reportPath});
}
(async()=>{
 const r=await fetch(base+'/auth/login',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({password:fs.readFileSync('lab/data/password','utf8').trim()}),signal:AbortSignal.timeout(30000)});
 if(!r.ok)throw Error('Login failed');cookie=r.headers.get('set-cookie').split(';')[0];
 const initial=await state();
 requireExclusiveServers(experiment(initial),initial.experiments);
 const existing=initial.transactions.filter(t=>t.experimentId===eid);
 if(prepared){
  const cfg=JSON.parse(fs.readFileSync(path.join(output,'config.json')));
  if(cfg.priorDrainedAttempt){
   const {requireDrainedAttempt}=require('./preparation-resume-policy.cjs');
   const p=cfg.priorDrainedAttempt;
   requireDrainedAttempt(initial,eid,{eid,runtimeSha:sha,transferPercent:alpha},sha,alpha,p.runId,p.rejectedIds,p.priorRunIds||[]);
  }else if(initial.workloads.some(w=>w.experimentId===eid&&!['completed','completed-with-errors','cancelled','canceled','interrupted'].includes(w.status))||existing.some(t=>t.status!=='confirmed'||t.error||!t.hash||(t.type==='transfer'&&(!t.readyAt||t.readyAt.startsWith('0001-')))))throw Error('Prepared experiment still has unresolved transactions or active runs');
 }else if(initial.workloads.some(w=>w.experimentId===eid)||(!resume&&existing.length)||existing.some(t=>t.status!=='confirmed'||t.error||!t.hash||!['public','createAccount','mint'].includes(t.type)))throw Error('Not a fresh or fully confirmed preparation-only experiment');
 await wait('network',async()=>{
  const e=experiment(await state());
  return networkProgress(e,sha);
 },7200);
 const s=await state(),e=experiment(s);
 requireExclusiveServers(e,s.experiments);
 const {observer,miners,traders}=validateLayout(e,s.servers,sha);
 log('roles',{observer:observer.id,miners:miners.map(n=>n.id),traders:traders.map(n=>n.id)});
 if(prepared){
  for(const n of traders)for(const type of ['createAccount','mint'])if(!existing.some(t=>t.fromNode===n.id&&t.type===type))throw Error('Missing confirmed preparation for '+n.id);
 }else{
 for(let i=0;i<traders.length;i+=5){
  if(alpha!==null)await wait('funding-miner-balances',async()=>{
   const current=experiment(await state());
   const funders=miners.slice(0,Math.min(5,traders.length-i));
   const ready=funders.filter(m=>{const n=current.nodes.find(n=>n.id===m.id);return n?.status==='running'&&Date.now()-Date.parse(n.lastSeen)<90000&&BigInt(n.publicBalance||'0')>=1000420000000000000n;});
   return ready.length===funders.length?true:{ready:ready.length,total:funders.length};
  },1800);
  await batch(traders.slice(i,i+5).map((n,j)=>()=>tx('public',miners[j].id,n.id,alpha===null?'10000000000000000':'1000000000000000000')));
  log('funded',{done:Math.min(i+5,traders.length),total:traders.length});
 }
 const proofBatches=preparationBatches(traders,s.servers,alpha===null?5:20,alpha===null?5:4);
 log('preparation-plan',{maxWorkers:alpha===null?5:20,maxPerHostGroup:alpha===null?5:4,stage:'independent-account-proofs',measurementSchedulerChanged:false});
 let created=0;
 for(const nodes of proofBatches){
  await batch(nodes.map(n=>()=>tx('createAccount',n.id,'','')));
  created+=nodes.length;log('created',{done:created,total:traders.length});
 }
 // Different accounts initialize independently; never overlap tasks on one
 // account. Bound cold loading to four accounts per configured host group.
 // Preserve the legacy runner's serial Mint preparation when no mixture is set.
 let minted=0;
 for(const nodes of alpha===null?traders.map(n=>[n]):proofBatches){
  await batch(nodes.map(n=>()=>tx('mint',n.id,'','1000000')));
  minted+=nodes.length;log('minted',{done:minted,total:traders.length});
 }
 }
 // Monitor provides fresh samples without a long serial 100-node refresh sweep.
 await wait('fresh-accounts',async()=>{
  const e=experiment(await state());const ready=e.nodes.filter(n=>traders.some(t=>t.id===n.id)&&n.status==='running'&&n.mining===false&&!n.stateError&&!n.privateStateError&&Date.now()-Date.parse(n.lastSeen)<90000&&BigInt(n.zkBalance||'0')>=(prepared?1n:1000000n));
  return ready.length===94?true:{ready:ready.length,total:94};
 },600);
 const run=await startRunWhenIdle(()=>api('/workloads',{experimentId:eid,name:alpha===null?'100 nodes / 94 traders / 1 hour':`100 nodes / alpha ${alpha}% / 1 hour`,type:alpha===null?'transfer':'mixed',transferPercent:alpha??100,value:'1',strategy:'ready-pool',mode:'saturation',nodeIds:traders.map(n=>n.id),observerNodeId:observer.id,warmupSeconds:600,warmupTimeoutSeconds:7200,durationSeconds:3600,confirmations:6,ratePerSecond:1}));
 acceptedRun=run.id;
 fs.writeFileSync(path.join(output,'run-id.txt'),run.id);log('run-started',{id:run.id});
 await wait('run',async()=>{
  const w=(await api('/workloads')).find(w=>w.id===run.id);if(!w)throw Error('Missing run');
  if(w.invalidReason||w.blockError)throw Error(w.invalidReason||w.blockError);
  if(w.status==='completed')return true;
  if(!['queued','running','draining'].includes(w.status))throw Error('Run '+w.status);
  return {id:w.id,status:w.status,phase:w.phase,submitted:w.submitted,start:w.measurementStartedAt,end:w.measurementEndsAt};
 },14400);
 await exports(run.id);
 const report=await api('/workloads/'+run.id+'/report?format=json');
 if(!report.runs[0]?.completeWindow)throw Error('Incomplete measurement window');
 await api('/experiments/'+eid+'/stop',{});
 await wait('stopping',async()=>{const e=experiment(await state());if(e.status==='stop-failed')throw Error(e.error);return e.status==='stopped'&&e.nodes.every(n=>n.status==='stopped')?true:{status:e.status,stopped:e.nodes.filter(n=>n.status==='stopped').length};},3600);
 await auditStoppedProcesses();
 await exports(run.id);log('COMPLETE',{eid,run:run.id});
})().catch(async e=>{
 log('HALTED',{error:e.message,note:'No replay or state rollback. Inspect persisted transactions and running nodes.'});
 if(cookie)try{await exports(acceptedRun);}catch(exportError){log('EXPORT_FAILED',{error:exportError.message});}
 process.exitCode=1;
});
