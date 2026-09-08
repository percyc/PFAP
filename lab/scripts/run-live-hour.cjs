// Explicit one-shot live experiment runner. Never retries writes or unknown txs.
// Usage: node lab/scripts/run-live-hour.cjs EXPERIMENT_ID OUTPUT_DIRECTORY
const fs = require('node:fs');
const path = require('node:path');
const [eid, output] = process.argv.slice(2);
if (!/^exp-[a-f0-9]+$/.test(eid || '') || !output) throw Error('Specify experiment ID and output directory');
fs.mkdirSync(output, {recursive:true});
const marker=path.join(output,'started.json');
if(fs.existsSync(marker)) throw Error('Already started: inspect persisted progress; do not replay');
fs.writeFileSync(marker,JSON.stringify({eid,startedAt:new Date().toISOString()}),{flag:'wx'});
const base='http://127.0.0.1:8090';
const sha='c3933f1bd7be12d3b9b1cb96ae92547e9c5a338a66186599ef05d8e72e509070';
let cookie;
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
 const t=await api('/transactions',{experimentId:eid,type,fromNode:from,toNode:to,value});
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
 for(const [name,url] of [['experiment','/experiments/'+eid],...(run?[['run','/workloads/'+run]]:[])]){
  for(const format of ['json','html','csv']){
   const r=await request('/api'+url+'/report?format='+format);
   fs.writeFileSync(path.join(output,name+'.'+(format==='csv'?'zip':format)),Buffer.from(await r.arrayBuffer()));
  }
 }
}
(async()=>{
 const r=await fetch(base+'/auth/login',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({password:fs.readFileSync('lab/data/password','utf8').trim()}),signal:AbortSignal.timeout(30000)});
 if(!r.ok)throw Error('Login failed');cookie=r.headers.get('set-cookie').split(';')[0];
 const initial=await state();
 if(initial.transactions.some(t=>t.experimentId===eid)||initial.workloads.some(w=>w.experimentId===eid))throw Error('Not a fresh experiment');
 await wait('network',async()=>{
  const e=experiment(await state());
  if(['failed','stopped','stop-failed','interrupted'].includes(e.status))throw Error(e.error||e.status);
  if(e.status==='running'&&e.nodes.length===100&&e.nodes.every(n=>n.status==='running'&&n.runtimeSha===sha&&n.peers===99&&n.block>=12))return true;
  return {status:e.status,running:e.nodes.filter(n=>n.status==='running').length,connected:e.nodes.filter(n=>n.peers===99).length};
 },7200);
 const s=await state(),e=experiment(s),observer=e.nodes.find(n=>s.servers.find(v=>v.id===n.serverId)?.host==='local');
 const miners=e.nodes.filter(n=>n.isMiner),traders=e.nodes.filter(n=>!n.isMiner&&n.id!==observer?.id);
 if(!observer||miners.length!==5||traders.length!==94)throw Error('Unexpected roles');
 log('roles',{observer:observer.id,miners:miners.map(n=>n.id),traders:traders.map(n=>n.id)});
 for(let i=0;i<traders.length;i+=5){
  await batch(traders.slice(i,i+5).map((n,j)=>()=>tx('public',miners[j].id,n.id,'10000000000000000')));
  log('funded',{done:Math.min(i+5,traders.length),total:traders.length});
 }
 for(let i=0;i<traders.length;i+=5){
  await batch(traders.slice(i,i+5).map(n=>()=>tx('createAccount',n.id,'','')));
  log('created',{done:Math.min(i+5,traders.length),total:traders.length});
 }
 for(let i=0;i<traders.length;i++){
  await tx('mint',traders[i].id,'','1000000');log('minted',{done:i+1,total:traders.length});
 }
 // Monitor provides fresh samples without a long serial 100-node refresh sweep.
 await wait('fresh-accounts',async()=>{
  const e=experiment(await state());const ready=e.nodes.filter(n=>traders.some(t=>t.id===n.id)&&n.status==='running'&&n.mining===false&&Date.now()-Date.parse(n.lastSeen)<90000&&BigInt(n.zkBalance||'0')>=1000000n);
  return ready.length===94?true:{ready:ready.length,total:94};
 },600);
 const run=await api('/workloads',{experimentId:eid,name:'100 nodes / 94 traders / 1 hour',type:'transfer',value:'1',strategy:'ready-pool',mode:'saturation',nodeIds:traders.map(n=>n.id),observerNodeId:observer.id,warmupSeconds:600,durationSeconds:3600,confirmations:6,ratePerSecond:1});
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
 await exports(run.id);log('COMPLETE',{eid,run:run.id});
})().catch(e=>{log('HALTED',{error:e.message,note:'No replay or state rollback. Inspect persisted transactions and running nodes.'});process.exitCode=1;});
