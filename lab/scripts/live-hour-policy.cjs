// Pure preflight/scheduling policy, shared by the one-shot runner and tests.
function runtimeSHA(value) {
 if(!/^[a-f0-9]{64}$/.test(value||''))throw Error('Set PFAP_RUNTIME_SHA to the verified runtime SHA-256');
 return value;
}
function mixedPercent(value){
 if(value===undefined)return null;
 if(!['0','20','40','60','80','100'].includes(value))throw Error('PFAP_TRANSFER_PERCENT must be 0,20,40,60,80,100');
 return Number(value);
}
function spreadTraders(nodes,servers){
 const groups=new Map();
 for(const node of nodes){
  const server=servers.find(s=>s.id===node.serverId);
  if(!server)throw Error('Unknown trader server');
  const key=server.hostGroup||server.id;
  if(!groups.has(key))groups.set(key,[]);
  groups.get(key).push(node);
 }
 const result=[];
 for(let round=0;result.length<nodes.length;round++)for(const group of groups.values())if(group[round])result.push(group[round]);
 return result;
}
// Preparation only: independent accounts may load keys/prove concurrently.
// Bound both total workers and workers in each configured physical host group.
function preparationBatches(nodes,servers,limit=20,perHost=4){
 if(!Number.isInteger(limit)||limit<1||limit>20||!Number.isInteger(perHost)||perHost<1||perHost>limit)throw Error('Invalid preparation concurrency');
 const groups=new Map(servers.map(s=>[s.id,s.hostGroup||s.id]));
 if(new Set(nodes.map(n=>n.id)).size!==nodes.length||nodes.some(n=>!groups.has(n.serverId)))throw Error('Invalid preparation node placement');
 const batches=[];let remaining=[...nodes];
 while(remaining.length){
  const batch=[],rest=[],counts=new Map();
  for(const node of remaining){const group=groups.get(node.serverId),count=counts.get(group)||0;
   if(batch.length<limit&&count<perHost){batch.push(node);counts.set(group,count+1);}else rest.push(node);
  }
  batches.push(batch);remaining=rest;
 }
 return batches;
}
function validateLayout(exp,servers,sha){
 if(exp.nodes.length!==100||exp.placements.length!==100||exp.placements.some(p=>p.count!==1)||new Set(exp.placements.map(p=>p.serverId)).size!==100)throw Error('Require 100 distinct servers with one process each');
 if(exp.artifactSha!==sha||exp.nodes.some(n=>n.runtimeSha!==sha))throw Error('Runtime version does not match the verified artifact');
 const observer=exp.nodes.find(n=>servers.find(s=>s.id===n.serverId)?.host==='local');
 const miners=exp.nodes.filter(n=>n.isMiner);
 const traders=spreadTraders(exp.nodes.filter(n=>!n.isMiner&&n.id!==observer?.id),servers);
 if(!observer||observer.isMiner||miners.length!==5||traders.length!==94)throw Error('Require one observer, five dedicated miners and 94 traders');
 const groups=miners.map(n=>servers.find(s=>s.id===n.serverId)?.hostGroup);
 if(groups.some(g=>!g)||new Set(groups).size!==5)throw Error('Miners must occupy five explicitly labeled distinct host groups');
 return {observer,miners,traders};
}
function requireExclusiveServers(exp,experiments){
 const targets=new Set(exp.placements.map(p=>p.serverId));
 const shared=experiments.filter(other=>other.id!==exp.id&&['running','deploying','resuming','stopping'].includes(other.status)&&(other.placements||[]).some(p=>targets.has(p.serverId)));
 if(shared.length)throw Error('Another active experiment shares these servers: '+shared.map(e=>e.id).join(', '));
}
function networkProgress(exp,sha){
 if(exp.artifactSha&&exp.artifactSha!==sha)throw Error('Experiment runtime differs from PFAP_RUNTIME_SHA');
 if(['draft','failed','stopped','stop-failed','interrupted'].includes(exp.status))throw Error(exp.error||'Experiment is '+exp.status+'; deployment is not running');
 const nodes=exp.nodes||[];
 if(exp.status==='running'&&nodes.length===100&&nodes.every(n=>n.status==='running'&&n.runtimeSha===sha&&n.peers===99&&n.block>=12))return true;
 return {status:exp.status,running:nodes.filter(n=>n.status==='running').length,connected:nodes.filter(n=>n.peers===99).length};
}
module.exports={runtimeSHA,mixedPercent,spreadTraders,preparationBatches,validateLayout,requireExclusiveServers,networkProgress};
