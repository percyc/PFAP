const {test}=require('node:test');
const assert=require('node:assert/strict');
const {runtimeSHA,spreadTraders,validateLayout,requireExclusiveServers}=require('./live-hour-policy.cjs');
const sha='a'.repeat(64);
function fixture(){
 const servers=Array.from({length:100},(_,i)=>({id:'s'+i,host:i===0?'local':'host'+i,hostGroup:'g'+Math.floor(i/20)}));
 const nodes=servers.map((s,i)=>({id:'n'+i,serverId:s.id,isMiner:[1,20,40,60,80].includes(i),runtimeSha:sha}));
 return {servers,exp:{artifactSha:sha,nodes,placements:servers.map(s=>({serverId:s.id,count:1}))}};
}
test('explicit verified runtime is required',()=>{assert.equal(runtimeSHA(sha),sha);for(const value of [undefined,'','abc','A'.repeat(64)])assert.throws(()=>runtimeSHA(value));});
test('round robin preserves every trader and order within a host group',()=>{
 const nodes=[{id:'a',serverId:'1'},{id:'b',serverId:'2'},{id:'c',serverId:'3'},{id:'d',serverId:'4'}];
 const servers=[{id:'1',hostGroup:'x'},{id:'2',hostGroup:'x'},{id:'3',hostGroup:'y'},{id:'4',hostGroup:'y'}];
 assert.deepEqual(spreadTraders(nodes,servers).map(n=>n.id),['a','c','b','d']);assert.deepEqual(spreadTraders([],[]),[]);assert.throws(()=>spreadTraders(nodes,[]));
});
test('100-node role/runtime/placement invariants',()=>{
 const {exp,servers}=fixture();const p=validateLayout(exp,servers,sha);assert.equal(p.traders.length,94);assert.equal(p.miners.length,5);assert.equal(p.observer.id,'n0');assert.equal(new Set(p.traders.slice(0,5).map(n=>servers.find(s=>s.id===n.serverId).hostGroup)).size,5);
 for(const alter of [e=>e.placements[0].count=2,e=>e.placements[0].serverId=e.placements[1].serverId,e=>e.nodes[0].isMiner=true,e=>e.nodes[5].runtimeSha='b'.repeat(64),e=>e.artifactSha='b'.repeat(64)]){const e=structuredClone(exp);alter(e);assert.throws(()=>validateLayout(e,servers,sha));}
 const coLocated=structuredClone(servers);coLocated[20].hostGroup='g0';assert.throws(()=>validateLayout(exp,coLocated,sha));
});
test('reject overlapping active experiment but preserve stopped records',()=>{
 const exp={id:'new',placements:[{serverId:'s1'}]};
 for(const status of ['running','deploying','resuming','stopping'])assert.throws(()=>requireExclusiveServers(exp,[{id:'old',status,placements:[{serverId:'s1'}]}]));
 requireExclusiveServers(exp,[{id:'old',status:'stopped',placements:[{serverId:'s1'}]}]);
 requireExclusiveServers(exp,[{id:'other',status:'running',placements:[{serverId:'s2'}]}]);
});
