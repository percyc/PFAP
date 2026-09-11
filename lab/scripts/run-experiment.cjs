#!/usr/bin/env node
// One-command local-controller workflow. Never chooses a target implicitly.
const fs = require('node:fs');
const path = require('node:path');
const crypto = require('node:crypto');
const {spawn} = require('node:child_process');
const {runtimeSHA, validateLayout, requireExclusiveServers} = require('./live-hour-policy.cjs');
const root = path.resolve(__dirname, '../..');
const help = `Usage: node lab/scripts/run-experiment.cjs --experiment exp-ID --mode fresh|prepared [--start] [--check] [--runtime-sha SHA256]

Runs against the local Lab controller at http://127.0.0.1:8090.
Requires an already deployed 100-server layout (5 miners / 94 traders / 1 local observer).
fresh: prepare a new chain's accounts; prepared: reuse fully confirmed preparation.
--start: explicitly allow resuming this stopped experiment in its original directories.
--check: read-only preflight; never resumes nodes or submits transactions.
Runtime defaults to the SHA-256 of dist/pfap-runtime.tar.gz, checked against every node.
Results and progress: lab/data/one-click-100.*/ (printed before execution).
Success exports reports and stops the experiment; failure preserves nodes and transactions.
Do not run alongside manual operations; Ctrl-C does not cancel accepted remote work.
`;

function options(args) {
 const result = {};
 for(let i=0;i<args.length;i++) {
  const key=args[i];
  if(['--help','--start','--check'].includes(key)) {
   if(result[key])throw Error('Duplicate option '+key);
   result[key]=true;
  } else if(['--experiment','--mode','--runtime-sha'].includes(key)) {
   if(result[key]!==undefined||!args[i+1]||args[i+1].startsWith('--'))throw Error('Missing or duplicate option '+key);
   result[key]=args[++i];
  } else throw Error('Unknown option '+key);
 }
 if(result['--help'])return result;
 if(!/^exp-[a-f0-9]+$/.test(result['--experiment']||''))throw Error('Explicit --experiment exp-ID required');
 if(!['fresh','prepared'].includes(result['--mode']))throw Error('Explicit --mode fresh or prepared required');
 if(result['--runtime-sha'])runtimeSHA(result['--runtime-sha']);
 return result;
}

function preflight(s, opt, sha) {
 const eid=opt['--experiment'], e=s.experiments.find(e=>e.id===eid);
 if(!e)throw Error('Experiment not found');
 requireExclusiveServers(e,s.experiments);
 validateLayout(e,s.servers,sha);
 if(e.status!=='running'&&!(e.status==='stopped'&&opt['--start']))throw Error('Require running experiment, or stopped experiment with --start');
 if(s.workloads.some(w=>w.experimentId===eid&&!['completed','completed-with-errors','cancelled','canceled','interrupted'].includes(w.status)))throw Error('Active or unresolved run exists; inspect it first');
 const tx=s.transactions.filter(t=>t.experimentId===eid);
 if(opt['--mode']==='fresh') {
  if(tx.length||s.workloads.some(w=>w.experimentId===eid))throw Error('fresh requires no prior transactions or runs');
 } else {
  if(tx.some(t=>t.status!=='confirmed'||t.error||!t.hash||(t.type==='transfer'&&(!t.readyAt||t.readyAt.startsWith('0001-')))))throw Error('Unresolved transaction: no automatic replay');
  const {traders}=validateLayout(e,s.servers,sha);
  for(const n of traders)for(const type of ['createAccount','mint'])if(!tx.some(t=>t.fromNode===n.id&&t.type===type))throw Error('Missing confirmed '+type+' for '+n.id);
 }
 return e;
}

function outcome(directory, status, details) {
 const result={status,at:new Date().toISOString(),...details};
 fs.writeFileSync(path.join(directory,'status.json'),JSON.stringify(result,null,2)+'\n');
 const files=['run.html','run.json','run.zip','experiment.html','experiment.json','experiment.zip'].filter(f=>fs.existsSync(path.join(directory,f)));
 fs.writeFileSync(path.join(directory,'RESULT.md'),`# 一键实验执行记录\n\n状态：${status}\n\n实验：${details.experiment}\n\n更新时间：${result.at}\n\n${details.error||''}\n\n${status==='completed'?'完整测量窗口、收尾和节点停止均通过脚本核验。':'尚未证明完整成功。若为失败/中断，请先检查 Web 中的运行、在途交易和节点；不要盲目重跑。节点可能仍在运行，已接受交易不会撤销。'}\n\n日志：[progress.log](progress.log)\n\n${files.map(f=>`- [${f}](${f})`).join('\n')}\n\n报告包含指标定义。100 个服务节点不代表 100 台物理主机，完整窗口也不等同于最大吞吐或统计平稳。\n`);
}

async function main(args) {
 const opt=options(args);
 if(opt['--help']){console.log(help);return;}
 process.chdir(root);
 const eid=opt['--experiment'];
 let sha=opt['--runtime-sha'];
 const data=path.join(root,'lab/data');
 let directory, lockOwned=false;
 const lock=path.join(data,`one-click-${eid}.lock`);
 if(!opt['--check']) {
  directory=fs.mkdtempSync(path.join(data,'one-click-100.'));
  console.log('Results: '+directory);
  outcome(directory,'running',{experiment:eid,runtimeSha:sha});
 }
 try {
  if(!sha) {
   const hash=crypto.createHash('sha256');
   for await(const chunk of fs.createReadStream('dist/pfap-runtime.tar.gz'))hash.update(chunk);
   sha=hash.digest('hex');
  }
  if(directory) {
   fs.mkdirSync(lock);lockOwned=true;
   fs.writeFileSync(path.join(lock,'owner.json'),JSON.stringify({pid:process.pid,experiment:eid,directory}));
  }
  const base='http://127.0.0.1:8090';
  const login=await fetch(base+'/auth/login',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({password:fs.readFileSync(path.join(data,'password'),'utf8').trim()}),signal:AbortSignal.timeout(30000)});
  if(!login.ok)throw Error('Lab login failed: HTTP '+login.status);
  const cookie=login.headers.get('set-cookie')?.split(';')[0];
  if(!cookie)throw Error('Missing login cookie');
  async function api(url,body) {
   const r=await fetch(base+'/api'+url,{method:body===undefined?'GET':'POST',headers:{cookie,'Content-Type':'application/json'},body:body===undefined?undefined:JSON.stringify(body),signal:AbortSignal.timeout(180000)});
   if(!r.ok)throw Error(url+' HTTP '+r.status+' '+(await r.text()).slice(0,500));
   return r.json();
  }
  const e=preflight(await api('/state'),opt,sha);
  console.log(JSON.stringify({stage:'preflight-passed',experiment:eid,runtimeSha:sha,mode:opt['--mode'],willResume:e.status==='stopped',checkOnly:!!opt['--check']}));
  if(opt['--check'])return;
  if(e.status==='stopped')await api('/experiments/'+eid+'/resume',{});
  const log=fs.openSync(path.join(directory,'progress.log'),'a',0o600);
  let code;
  try {
   code=await new Promise((resolve,reject)=>{
    const child=spawn(process.execPath,[path.join(__dirname,'run-live-hour.cjs'),eid,directory,...(opt['--mode']==='prepared'?['--prepared-run']:[])],{cwd:root,env:{...process.env,PFAP_RUNTIME_SHA:sha},stdio:['ignore',log,log]});
    const interrupt=()=>child.kill('SIGTERM');
    const cleanup=()=>{process.removeListener('SIGINT',interrupt);process.removeListener('SIGTERM',interrupt);};
    process.on('SIGINT',interrupt);process.on('SIGTERM',interrupt);
    child.once('error',error=>{cleanup();reject(error);});
    child.once('exit',(code,signal)=>{cleanup();resolve(code===null?'signal '+signal:code);});
   });
  } finally {fs.closeSync(log);}
  if(code!==0)throw Error('Runner exited '+code+'; see progress.log. No replay or automatic rollback.');
  outcome(directory,'completed',{experiment:eid,runtimeSha:sha});
  console.log('COMPLETE: '+path.join(directory,'RESULT.md'));
 } catch(e) {
  if(directory)outcome(directory,'halted',{experiment:eid,runtimeSha:sha,error:e.message});
  throw e;
 } finally {
  if(lockOwned){fs.unlinkSync(path.join(lock,'owner.json'));fs.rmdirSync(lock);}
 }
}
if(require.main===module)main(process.argv.slice(2)).catch(e=>{console.error(e.message);process.exitCode=1;});
module.exports={options,preflight,outcome,main};
