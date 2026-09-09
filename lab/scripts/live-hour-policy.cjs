// Pure preflight/scheduling policy, shared by the one-shot runner and tests.
function runtimeSHA(value) {
 if(!/^[a-f0-9]{64}$/.test(value||''))throw Error('Set PFAP_RUNTIME_SHA to the verified runtime SHA-256');
 return value;
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
module.exports={runtimeSHA,spreadTraders,validateLayout,requireExclusiveServers};
