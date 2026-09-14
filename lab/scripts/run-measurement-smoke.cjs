// Isolated seven-node, 20-minute measurement validation. Never replays writes.
const fs=require('node:fs'),path=require('node:path'),crypto=require('node:crypto');
process.chdir(path.resolve(__dirname,'../..'));
const base='http://127.0.0.1:8090';
const mixed=process.argv.includes('--mixed');
if(process.argv.slice(2).some(v=>v!=='--mixed'))throw Error('Only --mixed is supported');
const duration=mixed?120:1200;
const output=fs.mkdtempSync(path.resolve(mixed?'lab/data/mixed-smoke.':'lab/data/measurement-20m.'));
const log=(stage,details={})=>{const line=JSON.stringify({at:new Date().toISOString(),stage,...details});fs.appendFileSync(path.join(output,'progress.log'),line+'\n');console.log(line);};
let cookie,eid,runID;
async function request(p,body){const r=await fetch(base+p,{method:body===undefined?'GET':'POST',headers:{cookie,'Content-Type':'application/json'},body:body===undefined?undefined:JSON.stringify(body),signal:AbortSignal.timeout(180000)});if(!r.ok)throw Error(p+' HTTP '+r.status+' '+(await r.text()).slice(0,500));return r;}
const api=async(p,b)=>(await request('/api'+p,b)).json();
const pause=ms=>new Promise(r=>setTimeout(r,ms));
async function wait(stage,fn,seconds){const end=Date.now()+seconds*1000;let last=0;while(Date.now()<end){const result=await fn();if(result===true)return;if(Date.now()-last>30000){log(stage,result);last=Date.now();}await pause(10000);}throw Error(stage+' timeout; inspect without replay');}
async function state(){return api('/state');}
const exp=s=>s.experiments.find(e=>e.id===eid);
async function transaction(type,from,to,value){
 const t=await api('/transactions',{experimentId:eid,type,fromNode:from,toNode:to||'',value:value||''});log('preparation',{id:t.id,type,from,to});
 await wait('transaction',async()=>{const s=await state(),v=s.transactions.find(v=>v.id===t.id);if(!v)throw Error('Missing tx');if(v.error||['failed','unknown','timeout','cancelled'].includes(v.status))throw Error(t.id+' '+v.status+' '+v.error);const height=Number(BigInt(v.blockNumber||'0'));return v.status==='confirmed'&&exp(s).nodes.every(n=>n.block>=height+5)?true:{id:t.id,status:v.status};},1800);
}
async function reports(){for(const [name,p]of [['experiment','/experiments/'+eid],...(runID?[['run','/workloads/'+runID]]:[])])for(const format of ['json','html','csv']){try{const r=await request('/api'+p+'/report?format='+format);fs.writeFileSync(path.join(output,name+'.'+(format==='csv'?'zip':format)),Buffer.from(await r.arrayBuffer()));}catch(e){log('export-error',{error:e.message});}}}
(async()=>{
 log('output',{output});
 const login=await fetch(base+'/auth/login',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({password:fs.readFileSync('lab/data/password','utf8').trim()}),signal:AbortSignal.timeout(30000)});if(!login.ok)throw Error('login');cookie=login.headers.get('set-cookie').split(';')[0];
 const s=await state();if(s.experiments.some(e=>['running','deploying','resuming','stopping'].includes(e.status)))throw Error('Another experiment active');
 const local=s.servers.find(v=>v.host==='local');
 const group=g=>s.servers.filter(v=>v.host!=='local'&&v.hostGroup===g&&v.status==='online');
 const servers=[local,group('db2')[0],group('db1')[0],group('pv4')[0],group('pv5')[0],group('pv9')[0],group('pv4')[1]];
 if(servers.some(v=>!v)||new Set(servers.map(v=>v.id)).size!==7)throw Error('Missing distinct eligible servers');
 const h=crypto.createHash('sha256');for await(const b of fs.createReadStream('dist/pfap-runtime.tar.gz'))h.update(b);const sha=h.digest('hex');
 const config={name:mixed?'7 nodes / mixed alpha40 / short validation':'7 nodes / node-local metrics / 20 minutes',networkId:mixed?55714:55713,p2pPortBase:37000,rpcPortBase:47000,artifactPath:path.resolve('dist/pfap-runtime.tar.gz'),topology:'full-mesh',minerCount:2,minerMode:'manual',minerSelections:servers.slice(1,3).map(v=>({serverId:v.id,localIndex:1})),placements:servers.map(v=>({serverId:v.id,count:1}))};
 const e=await api('/experiments',config);eid=e.id;fs.writeFileSync(path.join(output,'experiment-id.txt'),eid);fs.writeFileSync(path.join(output,'config.json'),JSON.stringify({experimentId:eid,runtimeSha:sha,...config},null,2));
 await api('/experiments/'+eid+'/deploy',{});log('deploy-started',{eid,sha});
 await wait('network',async()=>{const e=exp(await state());if(['failed','interrupted','stop-failed'].includes(e.status))throw Error(e.error||e.status);return e.status==='running'&&e.nodes.length===7&&e.nodes.every(n=>n.status==='running'&&n.peers===6&&n.runtimeSha===sha&&n.block>=8)?true:{status:e.status,online:e.nodes.filter(n=>n.status==='running').length,connected:e.nodes.filter(n=>n.peers===6).length};},3600);
 const nodes=exp(await state()).nodes,observer=nodes.find(n=>n.serverId===local.id),miners=nodes.filter(n=>n.isMiner),traders=nodes.filter(n=>!n.isMiner&&n.id!==observer.id);
 if(miners.length!==2||traders.length!==4)throw Error('Role mismatch');
 for(const n of traders){await transaction('public',miners[0].id,n.id,'10000000000000000');await transaction('createAccount',n.id);await transaction('mint',n.id,'','1000000');}
 await wait('fresh-accounts',async()=>{const ns=exp(await state()).nodes.filter(n=>traders.some(t=>t.id===n.id));const ready=ns.filter(n=>n.status==='running'&&n.mining===false&&!n.privateStateError&&!n.stateError&&Date.now()-Date.parse(n.lastSeen)<90000&&BigInt(n.zkBalance||'0')>=1000000n);return ready.length===4?true:{ready:ready.length};},600);
 const w=await api('/workloads',{experimentId:eid,name:mixed?'Mixed alpha40 / short validation':'Node-local metrics / 20 minute window',type:mixed?'mixed':'transfer',transferPercent:mixed?40:100,value:'1',strategy:'ready-pool',mode:'saturation',nodeIds:traders.map(n=>n.id),observerNodeId:observer.id,warmupSeconds:120,durationSeconds:duration,confirmations:6,ratePerSecond:1});runID=w.id;fs.writeFileSync(path.join(output,'run-id.txt'),runID);log('run-started',{eid,runID});
 await wait('run',async()=>{const w=(await api('/workloads')).find(w=>w.id===runID);if(!w)throw Error('Missing run');if(w.invalidReason||w.blockError||!['queued','running','draining','completed'].includes(w.status))throw Error(w.invalidReason||w.blockError||w.status);return w.status==='completed'?true:{status:w.status,phase:w.phase,submitted:w.submitted,start:w.measurementStartedAt,end:w.measurementEndsAt};},5400);
 await reports();const r=JSON.parse(fs.readFileSync(path.join(output,'run.json')));if(!r.runs[0].completeWindow)throw Error('Incomplete window');
 await api('/experiments/'+eid+'/stop',{});
 await wait('stopping',async()=>{const e=exp(await state());if(e.status==='stop-failed')throw Error(e.error);return e.status==='stopped'&&e.nodes.every(n=>n.status==='stopped')?true:{status:e.status};},1200);
 await reports();log('COMPLETE',{eid,runID,output});
})().catch(async e=>{log('HALTED',{error:e.message,eid,runID});if(eid&&cookie)await reports();process.exitCode=1;});
