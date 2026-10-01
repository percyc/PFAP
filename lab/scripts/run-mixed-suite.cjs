// Explicit six-window campaign. Gate on a completed mixed smoke test; never
// retry accepted writes or unknown transactions. Separate fresh chain per group.
const fs=require('node:fs'),path=require('node:path'),crypto=require('node:crypto'),{spawn}=require('node:child_process');
const {collect}=require('./summarize-node-metrics.cjs');
const {requirePreparationOnly,requireFinishedWarmup,requireDrainedAttempt}=require('./preparation-resume-policy.cjs');
const root=path.resolve(__dirname,'../..');process.chdir(root);
const [gateDir,output,mode,...extra]=process.argv.slice(2);
if(extra.length||(mode&&!['--resume-preparation','--resume-after-warmup','--resume-after-drained-attempt'].includes(mode)))throw Error('Unknown resume mode');
const resume=!!mode;
if(!gateDir||!output)throw Error('Usage: node run-mixed-suite.cjs SMOKE_EVIDENCE NEW_OUTPUT_DIRECTORY');
fs.mkdirSync(output,{recursive:true});
const marker=path.join(output,'started.json');
if(resume){
 const prior=JSON.parse(fs.readFileSync(marker));
 if(prior.gate!==path.resolve(gateDir)||JSON.stringify(prior.alphas)!==JSON.stringify([0,20,40,60,80,100]))throw Error('Campaign marker differs');
}else fs.writeFileSync(marker,JSON.stringify({startedAt:new Date().toISOString(),gate:path.resolve(gateDir),alphas:[0,20,40,60,80,100]}),{flag:'wx'});
const log=(stage,details={})=>{const line=JSON.stringify({at:new Date().toISOString(),stage,...details});fs.appendFileSync(path.join(output,'progress.log'),line+'\n');console.log(line);};
const pause=ms=>new Promise(r=>setTimeout(r,ms));
const localState=()=>JSON.parse(fs.readFileSync('lab/data/lab.json'));
let cookie;
async function api(p,body){
 const r=await fetch('http://127.0.0.1:8090/api'+p,{method:body===undefined?'GET':'POST',headers:{cookie,'Content-Type':'application/json'},body:body===undefined?undefined:JSON.stringify(body),signal:AbortSignal.timeout(180000)});
 if(!r.ok)throw Error('HTTP '+r.status+' '+p+' '+(await r.text()).slice(0,300));return r.json();
}
async function child(args,env,logFile){
 const fd=fs.openSync(logFile,'a');
 try{await new Promise((resolve,reject)=>{const p=spawn(process.execPath,args,{cwd:root,env:{...process.env,...env},stdio:['ignore',fd,fd]});p.once('error',reject);p.once('exit',(code,signal)=>code===0?resolve():reject(Error('Runner stopped: '+(signal||code)+'; inspect '+logFile)));});}
 finally{fs.closeSync(fd);}
}
(async()=>{
 if(resume)log('resume-requested',{mode});
 log('waiting-for-smoke',{gateDir});
 const deadline=Date.now()+2*3600000;
 let gate;
 while(Date.now()<deadline){
  const file=path.join(gateDir,'progress.log');
  const lines=fs.existsSync(file)?fs.readFileSync(file,'utf8').trim().split('\n').filter(Boolean).map(x=>JSON.parse(x)):[];
  if(lines.some(v=>v.stage==='HALTED'))throw Error('Smoke halted; campaign not started');
  if(lines.some(v=>v.stage==='COMPLETE')){gate=localState();break;}
  await pause(15000);
 }
 if(!gate)throw Error('Smoke gate deadline exceeded; campaign not started');
 const gateRun=fs.readFileSync(path.join(gateDir,'run-id.txt'),'utf8').trim();
 const w=gate.workloads.find(w=>w.id===gateRun),e=gate.experiments.find(e=>e.id===w?.experimentId);
 if(!w||w.type!=='mixed'||w.status!=='completed'||e.status!=='stopped')throw Error('Smoke not safely complete and stopped');
 const txs=gate.transactions.filter(t=>t.workloadId===gateRun);
 if(txs.some(t=>t.status!=='confirmed'||t.error)||!['public','transfer'].every(type=>txs.some(t=>t.type===type)))throw Error('Smoke must confirm both transaction types without errors');
 const smokeSummary=await collect(path.resolve(gateDir));
 if(!smokeSummary.completeSamples)throw Error('Smoke timing coverage incomplete; campaign not started');
 const hash=crypto.createHash('sha256');for await(const b of fs.createReadStream('dist/pfap-runtime.tar.gz'))hash.update(b);const sha=hash.digest('hex');
 if(e.artifactSha!==sha)throw Error('Runtime changed after smoke');
 const login=await fetch('http://127.0.0.1:8090/auth/login',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({password:fs.readFileSync('lab/data/password','utf8').trim()}),signal:AbortSignal.timeout(30000)});
 if(!login.ok)throw Error('Login failed');cookie=login.headers.get('set-cookie').split(';')[0];
 const s=await api('/state');
 const baseline=s.experiments.find(e=>e.id==='exp-c17eebe55019');
 if(!baseline||baseline.placements.length!==100||baseline.placements.some(p=>p.count!==1))throw Error('Verified 100-server baseline layout missing');
 const selections=baseline.nodes.filter(n=>n.isMiner).map(n=>({serverId:n.serverId,localIndex:n.localIndex}));
 if(selections.length!==5)throw Error('Baseline must have five dedicated miners');
 const results=resume&&fs.existsSync(path.join(output,'results.json'))?JSON.parse(fs.readFileSync(path.join(output,'results.json'))):[];
 if(JSON.stringify(results.map(r=>r.alpha))!==JSON.stringify([0,20,40,60,80,100].slice(0,results.length)))throw Error('Completed groups are not a valid prefix');
 for(const r of results){
  const liveRun=s.workloads.find(w=>w.id===r.runId),liveExp=s.experiments.find(e=>e.id===r.experimentId);
  if(!r.completeSamples||liveRun?.status!=='completed'||liveExp?.status!=='stopped'||liveExp?.artifactSha!==sha)throw Error('Prior completed group needs review');
 }
 for(const alpha of [0,20,40,60,80,100]){
  if(results.some(r=>r.alpha===alpha))continue;
  const live=await api('/state');
  if(baseline.placements.some(p=>!live.servers.some(v=>v.id===p.serverId&&v.status==='online')))throw Error('Not all 100 selected servers online');
  const dir=path.resolve(output,'alpha-'+alpha);
  let e,childMode=[];
  if(resume&&fs.existsSync(dir)){
   const cfg=JSON.parse(fs.readFileSync(path.join(dir,'config.json'))),prior=JSON.parse(fs.readFileSync(path.join(dir,'started.json')));
   if(cfg.runtimeSha!==sha)throw Error('Saved group runtime differs');
   if(mode==='--resume-after-drained-attempt'){
    const oldRun=fs.readFileSync(path.join(dir,'run-id.txt'),'utf8').trim();
    const rejectedIds=JSON.parse(process.env.PFAP_PREPROOF_REJECTED_IDS||'null');
    const priorRunIds=cfg.priorDrainedAttempt?[...(cfg.priorDrainedAttempt.priorRunIds||[]),cfg.priorDrainedAttempt.runId]:[];
    e=requireDrainedAttempt(live,cfg.experimentId,prior,sha,alpha,oldRun,rejectedIds,priorRunIds);
    const archive=dir+'-interrupted-'+oldRun;
    if(fs.existsSync(archive))throw Error('Attempt archive already exists');
    fs.renameSync(dir,archive);fs.mkdirSync(dir);
    fs.writeFileSync(path.join(dir,'config.json'),JSON.stringify({...cfg,priorDrainedAttempt:{runId:oldRun,priorRunIds,rejectedIds,evidence:archive}},null,2));
    fs.writeFileSync(path.join(dir,'experiment-id.txt'),e.id);
    childMode=['--prepared-run'];log('group-new-window-after-drained-attempt',{alpha,experimentId:e.id,oldRun,archive,rejectedIds});
   }else if(mode==='--resume-after-warmup'){
    const oldRun=fs.readFileSync(path.join(dir,'run-id.txt'),'utf8').trim();
    const priorRunIds=cfg.priorWarmupRunIds||(cfg.priorWarmupRunId?[cfg.priorWarmupRunId]:[]);
    e=requireFinishedWarmup(live,cfg.experimentId,prior,sha,alpha,oldRun,priorRunIds);
    const archive=dir+'-warmup-'+oldRun;
    if(fs.existsSync(archive))throw Error('Warmup archive already exists; inspect, do not replay');
    fs.renameSync(dir,archive);fs.mkdirSync(dir);
    fs.writeFileSync(path.join(dir,'config.json'),JSON.stringify({...cfg,priorWarmupRunId:oldRun,priorWarmupRunIds:[...priorRunIds,oldRun],priorWarmupEvidence:archive},null,2));
    fs.writeFileSync(path.join(dir,'experiment-id.txt'),e.id);
    childMode=['--prepared-run'];log('group-new-run-after-warmup',{alpha,experimentId:e.id,oldRun,archive});
   }else{
    e=requirePreparationOnly(live,cfg.experimentId,prior,sha,alpha);
    childMode=['--resume-preparation'];log('group-resume-preparation',{alpha,experimentId:e.id});
   }
  }else{
  if(live.experiments.some(e=>['running','deploying','resuming','stopping'].includes(e.status)))throw Error('Another experiment is active');
  fs.mkdirSync(dir);
  const config={name:`100 nodes / mixed alpha ${alpha}% / 1 hour`,networkId:55800+alpha,p2pPortBase:38000,rpcPortBase:48000,artifactPath:path.resolve('dist/pfap-runtime.tar.gz'),topology:'full-mesh',minerMode:'manual',minerCount:5,minerSelections:selections,placements:baseline.placements};
  e=await api('/experiments',config);
  fs.writeFileSync(path.join(dir,'config.json'),JSON.stringify({experimentId:e.id,runtimeSha:sha,...config},null,2));
  fs.writeFileSync(path.join(dir,'experiment-id.txt'),e.id);
  log('group-deploy',{alpha,experimentId:e.id});
  await api('/experiments/'+e.id+'/deploy',{});
  }
  await child(['lab/scripts/run-live-hour.cjs',e.id,dir,...childMode],{PFAP_RUNTIME_SHA:sha,PFAP_TRANSFER_PERCENT:String(alpha)},path.join(dir,'progress.log'));
  const summary=await collect(dir);
  if(!summary.completeSamples)throw Error('Incomplete metric coverage at alpha '+alpha+'; no automatic next group');
  results.push({alpha,...summary});fs.writeFileSync(path.join(output,'results.json'),JSON.stringify(results,null,2));
  log('group-complete',{alpha,experimentId:e.id,runId:summary.runId});
 }
 const keys=Object.keys(results[0].metrics);
 fs.writeFileSync(path.join(output,'SUMMARY.md'),'# 六组一小时窗口实验\n\n| 指标 | '+results.map(r=>'α='+r.alpha+'%').join(' | ')+' |\n| --- | '+results.map(()=>'---:').join(' | ')+' |\n'+keys.map(k=>'| '+k+' | '+results.map(r=>r.metrics[k]??'缺失').join(' | ')+' |').join('\n')+'\n\n单位、分类指标、实际比例及覆盖见各组 summary.json。\n');
 log('auditing-complete-suite',{groups:results.length});
 await child(['lab/scripts/audit-mixed-suite.cjs',path.resolve(output)],{},path.join(output,'audit.log'));
 log('COMPLETE',{groups:results.length,report:path.resolve(output,'FINAL_SUMMARY.zh.md')});
})().catch(e=>{log('HALTED',{error:e.message,note:'No automatic replay, rollback, or retry. Retain evidence.'});process.exitCode=1;});
