const {test}=require('node:test'),assert=require('node:assert/strict');
const {preparationBatches,spreadTraders}=require('./live-hour-policy.cjs');
test('94 independent accounts initialize once, at most 20 globally and 4 per host group',()=>{
 const servers=Array.from({length:94},(_,i)=>({id:'s'+i,hostGroup:'h'+Math.floor(i/20)}));
 const nodes=spreadTraders(servers.map((s,i)=>({id:'n'+i,serverId:s.id})),servers);
 const batches=preparationBatches(nodes,servers);
 assert.equal(batches.length,5);
 assert.deepEqual(batches.flat(),nodes);
 for(const batch of batches){
  assert.ok(batch.length<=20);
  const counts={};for(const n of batch){const group=servers.find(s=>s.id===n.serverId).hostGroup;counts[group]=(counts[group]||0)+1;}
  assert.ok(Object.values(counts).every(n=>n<=4));
 }
});
test('unbalanced placements obey host cap; invalid or duplicated accounts are rejected',()=>{
 const servers=Array.from({length:10},(_,i)=>({id:'s'+i,hostGroup:'same'})),nodes=servers.map((s,i)=>({id:'n'+i,serverId:s.id}));
 assert.deepEqual(preparationBatches(nodes,servers).map(b=>b.length),[4,4,2]);
 assert.deepEqual(preparationBatches([],servers),[]);
 assert.throws(()=>preparationBatches([...nodes,nodes[0]],servers));
 assert.throws(()=>preparationBatches([{id:'n',serverId:'missing'}],servers));
 for(const [limit,perHost] of [[0,1],[21,4],[20,0],[5,6]])assert.throws(()=>preparationBatches(nodes,servers,limit,perHost));
});
