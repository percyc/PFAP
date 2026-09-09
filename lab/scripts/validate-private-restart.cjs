const fs=require('node:fs');
const {spawnSync}=require('node:child_process');
const path=require('node:path');
const root=path.resolve(__dirname,'../..');
const eid=process.argv[2];
if(!/^exp-[0-9a-f]+$/.test(eid||''))throw Error('Usage: node lab/scripts/validate-private-restart.cjs EXPERIMENT_ID (fresh isolated 4-node test only)');
const base=process.env.PFAP_LAB_URL||'http://127.0.0.1:8090';
async function api(url,body){
 const login=await fetch(base+'/auth/login',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({password:fs.readFileSync(path.join(root,'lab/data/password'),'utf8').trim()}),signal:AbortSignal.timeout(30000)});
 if(!login.ok)throw Error('login failed');
 const cookie=login.headers.get('set-cookie').split(';')[0];
 const response=await fetch(base+'/api'+url,{method:body===undefined?'GET':'POST',headers:{cookie,'Content-Type':'application/json'},body:body===undefined?undefined:JSON.stringify(body),signal:AbortSignal.timeout(120000)});
 const value=await response.json();if(!response.ok)throw Error(response.status+': '+JSON.stringify(value));return value;
}
const pause=ms=>new Promise(r=>setTimeout(r,ms));
const log=(stage,detail={})=>console.log(JSON.stringify({at:new Date().toISOString(),stage,...detail}));
const state=()=>JSON.parse(fs.readFileSync(path.join(root,'lab/data/lab.json')));
const experiment=s=>s.experiments.find(e=>e.id===eid);
async function wait(stage,fn,seconds=1200){
 const end=Date.now()+seconds*1000;let last=0;
 while(Date.now()<end){const result=await fn();if(result===true)return;if(Date.now()-last>45000){log(stage,result||{});last=Date.now()}await pause(5000)}
 throw Error(stage+' timed out; no replay');
}
async function refresh(n){return api('/experiments/'+eid+'/nodes/'+n+'/state')}
async function submit(type,from,to,value){
 await refresh(from);if(to)await refresh(to);
 const t=await api('/transactions',{experimentId:eid,type,fromNode:from,toNode:to||'',value:value||''});
 log('accepted',{id:t.id,type,from,to});
 await wait('confirm',async()=>{const s=state(),v=s.transactions.find(x=>x.id===t.id);if(!v)throw Error('missing tx');if(['failed','unknown','timeout','cancelled'].includes(v.status)||v.error)throw Error(v.id+' '+v.status+' '+v.error);return v.status==='confirmed'&&experiment(s).nodes.every(n=>n.block>=Number(BigInt(v.blockNumber))+5)?true:{id:t.id,status:v.status}});
 await refresh(from);if(to)await refresh(to);log('confirmed',{id:t.id,type});return t;
}
async function crashAndRecover(n){
 const s=state(),e=experiment(s),node=e.nodes.find(x=>x.id===n),server=s.servers.find(x=>x.id===node.serverId);
 if(!node||!server||server.host==='local'||!e.id.match(/^exp-[0-9a-f]+$/)||!server.id.match(/^srv-[0-9a-f]+$/))throw Error('unsafe restart target');
 if(s.transactions.some(t=>t.experimentId===eid&&t.status!=='confirmed'))throw Error('restart requires confirmed checkpoint');
 if(!/^\/[A-Za-z0-9_/-]+$/.test(server.workDir)||server.workDir==='/'||node.localIndex!==1)throw Error('unsafe node directory');
 const dir=server.workDir+'/experiments/'+eid+'/'+server.id+'/node1';
 const script='set -eu\ndir='+JSON.stringify(dir)+'\npid=$(head -c 64 "$dir/geth.pid")\ncase "$pid" in ""|*[!0-9]*) exit 2;; esac\ntest "$pid" -gt 1\ntest "$(basename "$(readlink /proc/$pid/exe)")" = geth\ntr "\\0" "\\n" </proc/$pid/cmdline | grep -Fx -- "$dir" >/dev/null\nkill -KILL "$pid"\n';
 const args=['-o','BatchMode=yes','-o','StrictHostKeyChecking=yes','-o','ConnectTimeout=5','-p',String(server.port||22)];
 if(server.identityFile)args.push('-i',server.identityFile);if(server.knownHostsFile)args.push('-o','UserKnownHostsFile='+server.knownHostsFile);
 args.push(server.user+'@'+server.host,'bash -s');const r=spawnSync('ssh',args,{input:script,encoding:'utf8',timeout:15000});if(r.status!==0)throw Error('validated crash failed '+r.stderr);
 log('crash-injected',{node:n});await pause(2000);await api('/experiments/'+eid+'/nodes/'+n+'/recover',{});
 await wait('recover',async()=>{const node=experiment(state()).nodes.find(x=>x.id===n);return node.status==='running'&&!node.privateStateError&&!node.stateError&&!node.recoveryWarning&&Date.now()-Date.parse(node.lastSeen)<90000?true:{node:n,status:node.status,error:node.recoveryError,privateError:node.privateStateError}},600);
 log('recovered',{node:n});
}
(async()=>{
 if(experiment(state()).nodes.length!==4)throw Error('isolated four-node test required');
 if(state().transactions.some(t=>t.experimentId===eid))throw Error('not fresh; do not repeat script');
 await wait('network',async()=>{const e=experiment(state());if(['failed','interrupted','stop-failed'].includes(e.status))throw Error(e.error||e.status);return e.status==='running'&&e.nodes.every(n=>n.status==='running'&&n.peers>=3&&n.block>=6)?true:{status:e.status,online:e.nodes.filter(n=>n.status==='running').length}},2400);
 const e=experiment(state()),miner=e.nodes.find(n=>n.isMiner),traders=e.nodes.filter(n=>n.index===3||n.index===4);if(!miner||miner.index!==2||e.nodes.filter(n=>n.isMiner).length!==1||traders.length!==2||traders.some(n=>n.isMiner))throw Error('invalid roles: node 2 must be the only miner');
 for(const n of traders){await submit('public',miner.id,n.id,'10000000000000000');await submit('createAccount',n.id);}
 for(const n of traders)await crashAndRecover(n.id);
 for(const n of traders)await submit('mint',n.id,'','1000000');
 await submit('transfer',traders[0].id,traders[1].id,'1');
 for(const n of traders)await crashAndRecover(n.id);
 await submit('transfer',traders[1].id,traders[0].id,'1');
 await submit('redeem',traders[0].id,'','1');
 const finalNodes=experiment(state()).nodes;
 if(BigInt(finalNodes.find(n=>n.id===traders[0].id).zkBalance)!==999999n||BigInt(finalNodes.find(n=>n.id===traders[1].id).zkBalance)!==1000000n)throw Error('unexpected final private balances');
 log('PASS',{eid,confirmed:state().transactions.filter(t=>t.experimentId===eid&&t.status==='confirmed').length,balances:experiment(state()).nodes.filter(n=>traders.some(t=>t.id===n.id)).map(n=>({node:n.id,balance:n.zkBalance}))});
})().catch(e=>{log('HALTED',{error:e.message,noReplay:true});process.exitCode=1});
