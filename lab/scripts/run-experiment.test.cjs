const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const os=require('node:os');
const path=require('node:path');
const {spawnSync}=require('node:child_process');
const {options,preflight,outcome}=require('./run-experiment.cjs');
const sha='a'.repeat(64);
function fixture() {
 const servers=Array.from({length:100},(_,i)=>({id:'s'+i,host:i===0?'local':'host'+i,hostGroup:'g'+Math.floor(i/20)}));
 const nodes=servers.map((s,i)=>({id:'n'+i,serverId:s.id,isMiner:[1,20,40,60,80].includes(i),runtimeSha:sha}));
 const experiment={id:'exp-ab',status:'running',artifactSha:sha,nodes,placements:servers.map(s=>({serverId:s.id,count:1}))};
 return {servers,experiments:[experiment],transactions:[],workloads:[]};
}
const opt={'--experiment':'exp-ab','--mode':'fresh'};
test('explicit target/mode, no implicit latest selection; help works outside repository',()=>{
 assert.deepEqual(options(['--experiment','exp-ab','--mode','fresh','--check']),{...opt,'--check':true});
 for(const args of [[],['--experiment','exp-ab'],['--mode','prepared'],['--experiment','exp-ab','--mode','auto'],['--help','--bogus'],['--help','--start','--start']])assert.throws(()=>options(args));
 const r=spawnSync(process.execPath,[path.join(__dirname,'run-experiment.cjs'),'--help'],{cwd:os.tmpdir(),encoding:'utf8'});
 assert.equal(r.status,0);assert.match(r.stdout,/--check/);
});
test('fresh preflight and explicit stopped-network resumption',()=>{
 const s=fixture();assert.equal(preflight(s,opt,sha).id,'exp-ab');
 s.experiments[0].status='stopped';assert.throws(()=>preflight(s,opt,sha));
 assert.equal(preflight(s,{...opt,'--start':true},sha).status,'stopped');
 s.experiments[0].status='stop-failed';assert.throws(()=>preflight(s,{...opt,'--start':true},sha));
});
test('reject stale runtime, wrong layout and conflicting experiment',()=>{
 for(const mutate of [s=>s.experiments[0].nodes[2].runtimeSha='b'.repeat(64),s=>s.experiments[0].placements[0].count=2,s=>s.experiments.push({id:'exp-cd',status:'running',placements:[{serverId:'s2'}]}),s=>s.workloads.push({experimentId:'exp-ab',status:'running'})]) {
  const s=fixture();mutate(s);assert.throws(()=>preflight(s,opt,sha));
 }
});
test('prepared mode only reuses confirmed preparation, never unresolved Transfer',()=>{
 const s=fixture(),o={...opt,'--mode':'prepared'};
 assert.throws(()=>preflight(s,o,sha));
 for(const n of s.experiments[0].nodes.filter(n=>!n.isMiner&&n.id!=='n0'))for(const type of ['createAccount','mint'])s.transactions.push({experimentId:'exp-ab',fromNode:n.id,type,status:'confirmed',hash:'0x1'});
 preflight(s,o,sha);assert.throws(()=>preflight(s,opt,sha));
 for(const status of ['unknown','settling','failed','submitted']) {
  const dirty=structuredClone(s);dirty.transactions.push({experimentId:'exp-ab',type:'transfer',status,hash:'0x2'});assert.throws(()=>preflight(dirty,o,sha));
 }
 s.transactions.push({experimentId:'exp-ab',type:'transfer',status:'confirmed',hash:'0x2'});assert.throws(()=>preflight(s,o,sha));
 s.transactions.at(-1).readyAt=new Date().toISOString();preflight(s,o,sha);
});
test('success and failure both produce readable outcome and available report links',()=>{
 const dir=fs.mkdtempSync(path.join(os.tmpdir(),'pfap-outcome-test-'));
 try {
  fs.writeFileSync(path.join(dir,'run.html'),'test');
  for(const status of ['completed','halted']) {
   outcome(dir,status,{experiment:'exp-ab',error:status==='halted'?'test failure':''});
   assert.equal(JSON.parse(fs.readFileSync(path.join(dir,'status.json'))).status,status);
   const md=fs.readFileSync(path.join(dir,'RESULT.md'),'utf8');assert.match(md,/run.html/);assert.match(md,status==='halted'?/test failure/:/完整测量窗口/);
  }
 } finally {for(const f of fs.readdirSync(dir))fs.unlinkSync(path.join(dir,f));fs.rmdirSync(dir);}
});
