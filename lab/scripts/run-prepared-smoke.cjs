// Continue an initialized, idle seven-node network. Never repeat preparation.
const fs=require('node:fs'),path=require('node:path');
const {startRunWhenIdle}=require('./run-admission.cjs');
const root=path.resolve(__dirname,'../..');process.chdir(root);
const [eid,output]=process.argv.slice(2);
if(!/^exp-[a-f0-9]+$/.test(eid||'')||!output)throw Error('Specify experiment and NEW evidence directory');
fs.mkdirSync(output,{recursive:true});fs.writeFileSync(path.join(output,'started.json'),JSON.stringify({eid,at:new Date().toISOString()}),{flag:'wx'});
fs.writeFileSync(path.join(output,'experiment-id.txt'),eid);
let cookie,rid;
const log=(stage,details={})=>{const line=JSON.stringify({at:new Date().toISOString(),stage,...details});fs.appendFileSync(path.join(output,'progress.log'),line+'\n');console.log(line);};
async function request(p,body){const r=await fetch('http://127.0.0.1:8090'+p,{method:body===undefined?'GET':'POST',headers:{cookie,'Content-Type':'application/json'},body:body===undefined?undefined:JSON.stringify(body),signal:AbortSignal.timeout(180000)});if(!r.ok)throw Error('HTTP '+r.status+' '+p+' '+(await r.text()).slice(0,400));return r;}
const api=async(p,b)=>(await request('/api'+p,b)).json();
const pause=ms=>new Promise(r=>setTimeout(r,ms));
async function wait(stage,fn,seconds){const until=Date.now()+seconds*1000;let last=0;while(Date.now()<until){const v=await fn();if(v===true)return;if(Date.now()-last>30000){log(stage,v);last=Date.now();}await pause(10000);}throw Error(stage+' timed out');}
async function reports(){for(const [name,p]of [['experiment','/experiments/'+eid],...(rid?[['run','/workloads/'+rid]]:[])])for(const format of ['json','html','csv']){const r=await request('/api'+p+'/report?format='+format);fs.writeFileSync(path.join(output,name+'.'+(format==='csv'?'zip':format)),Buffer.from(await r.arrayBuffer()));}}
(async()=>{
 const login=await fetch('http://127.0.0.1:8090/auth/login',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({password:fs.readFileSync('lab/data/password','utf8').trim()}),signal:AbortSignal.timeout(30000)});if(!login.ok)throw Error('login');cookie=login.headers.get('set-cookie').split(';')[0];
 const s=await api('/state'),e=s.experiments.find(e=>e.id===eid);
 if(!e||e.status!=='running'||e.nodes.length!==7||s.workloads.some(w=>w.experimentId===eid))throw Error('Require untouched prepared seven-node network, without any prior workload');
 const ts=s.transactions.filter(t=>t.experimentId===eid);
 if(ts.some(t=>t.status!=='confirmed'||!t.hash||t.error))throw Error('Unresolved preparation; no replay');
 const observer=e.nodes.find(n=>s.servers.find(v=>v.id===n.serverId)?.host==='local'),miners=e.nodes.filter(n=>n.isMiner),traders=e.nodes.filter(n=>!n.isMiner&&n.id!==observer?.id);
 if(!observer||miners.length!==2||traders.length!==4)throw Error('Role mismatch');
 for(const n of traders)if(!['createAccount','mint'].every(type=>ts.some(t=>t.fromNode===n.id&&t.type===type)))throw Error('Missing prepared account');
 await wait('fresh-accounts',async()=>{const state=await api('/state'),e=state.experiments.find(e=>e.id===eid);const ready=e.nodes.filter(n=>n.status==='running'&&n.peers===6&&!n.stateError&&!n.privateStateError&&Date.now()-Date.parse(n.lastSeen)<90000);return ready.length===7?true:{ready:ready.length};},600);
 const config={experimentId:eid,name:'Mixed 40% / prepared network / strict warmup',type:'mixed',transferPercent:40,value:'1',strategy:'ready-pool',mode:'saturation',nodeIds:traders.map(n=>n.id),observerNodeId:observer.id,warmupSeconds:120,durationSeconds:120,confirmations:6,ratePerSecond:1};
 fs.writeFileSync(path.join(output,'config.json'),JSON.stringify({...config,runtimeSha:e.artifactSha},null,2));
 const w=await startRunWhenIdle(()=>api('/workloads',config));rid=w.id;fs.writeFileSync(path.join(output,'run-id.txt'),rid);log('run-started',{eid,runID:rid});
 await wait('run',async()=>{const w=(await api('/workloads')).find(w=>w.id===rid);if(!w||w.invalidReason||w.blockError||!['queued','running','draining','completed'].includes(w.status))throw Error(w?.invalidReason||w?.blockError||w?.status||'missing run');return w.status==='completed'?true:{status:w.status,phase:w.phase,submitted:w.submitted,start:w.measurementStartedAt,end:w.measurementEndsAt};},5400);
 await reports();const report=JSON.parse(fs.readFileSync(path.join(output,'run.json')));if(!report.runs[0].completeWindow)throw Error('Incomplete measurement window');
 await api('/experiments/'+eid+'/stop',{});
 await wait('stopping',async()=>{const e=(await api('/state')).experiments.find(e=>e.id===eid);if(e.status==='stop-failed')throw Error(e.error||e.status);return e.status==='stopped'&&e.nodes.every(n=>n.status==='stopped')?true:{status:e.status};},1200);
 await reports();log('COMPLETE',{eid,runID:rid,output});
})().catch(async e=>{log('HALTED',{error:e.message,eid,runID:rid});if(cookie)try{await reports();}catch(e){log('export-error',{error:e.message});}process.exitCode=1;});
