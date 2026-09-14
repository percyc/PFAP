// Read retained node timing logs and an exported completed run. No remote writes.
const fs=require('node:fs'),path=require('node:path'),{execFile}=require('node:child_process');
const root=path.resolve(__dirname,'../..');
const mean=a=>a.length?a.reduce((s,v)=>s+v,0)/a.length:null;
const percentile=(a,p)=>{const sorted=a.slice().sort((a,b)=>a-b);return sorted.length?sorted[Math.floor((sorted.length-1)*p)]:null;};
function summarize(report,w,nodes,records){
 if(!report.runs[0].completeWindow||w.status!=='completed')throw Error('Complete run required');
 const tx=report.transactions.filter(t=>t.runPhase==='measuring'&&['transfer','public'].includes(t.type));
 if(tx.some(t=>t.status!=='confirmed'))throw Error('Unfinished formal transactions');
 const start=Date.parse(w.measurementStartedAt)/1000,end=Date.parse(w.measurementEndsAt)/1000;
 if(!Number.isFinite(start)||!Number.isFinite(end)||end<=start)throw Error('Invalid measurement bounds');
 const observed=report.runs[0].blocks.slice().sort((a,b)=>a.number-b.number);
 if(!observed.length||observed[0].timestamp>start||observed.at(-1).timestamp<end)throw Error('Canonical observations do not bracket the complete header-time window');
 const blocks=report.runs[0].blocks.filter(b=>b.timestamp>=start&&b.timestamp<end).sort((a,b)=>a.number-b.number);
 for(let i=1;i<blocks.length;i++)if(blocks[i].number!==blocks[i-1].number+1||blocks[i].parentHash!==blocks[i-1].hash)throw Error('Non-contiguous canonical window');
 const allBlocks=new Map(report.runs[0].blocks.map(b=>[String(b.number),b]));
 const miners=nodes.filter(n=>n.isMiner).map(n=>n.id);
 for(const [node,rs] of Object.entries(records)){
  if(rs.some(r=>r.kind==='overflow'))throw Error('Timing overflow on '+node);
  if(new Set(rs.map(r=>r.session)).size>1)throw Error('Node restarted; timing sessions need manual review: '+node);
 }
 const delays=[],missingDelays=[],reorgTx=[],delayByType={public:[],transfer:[]};
 const perNode=Object.fromEntries((w.nodeIds||[]).map(id=>[id,{public:0,transfer:0}]));
 for(const t of tx){
  const b=allBlocks.get(String(t.blockNumber));
  const owner=t.type==='transfer'?t.toNode:t.fromNode;
  perNode[owner] ||= {public:0,transfer:0};
  perNode[owner][t.type]++;
  const rs=(records[owner]||[]).filter(r=>r.kind==='inclusion'&&r.hash===t.hash);
  if(rs.some(r=>r.block!==b?.hash))reorgTx.push(t.id);
  const match=rs.find(r=>r.block===b?.hash&&r.ns>=0);
  if(match){delays.push(match.ns/1e9);delayByType[t.type].push(match.ns/1e9);}else missingDelays.push(t.id);
 }
 // A reorg is not silently relabeled as first successful inclusion.
 if(reorgTx.length)throw Error('Reorg observations require separate reporting: '+reorgTx.join(','));
 const admissionPerTx=[],admissionCounts=[],admissionByType={public:[],transfer:[]};
 for(const t of tx){const a=miners.map(n=>(records[n]||[]).find(r=>r.kind==='admission'&&r.hash===t.hash)).filter(Boolean);admissionCounts.push(a.length);if(a.length){const value=mean(a.map(r=>r.ns/1e6));admissionPerTx.push(value);admissionByType[t.type].push(value);}}
 const blockMeans=[],blockCounts=[];
 for(const b of blocks){const a=miners.map(n=>(records[n]||[]).find(r=>r.kind==='block-validation'&&r.hash===b.hash)).filter(Boolean);blockCounts.push(a.length);if(a.length)blockMeans.push(mean(a.map(r=>r.ns/1e6)));}
 const complete=missingDelays.length===0&&admissionCounts.every(n=>n===miners.length)&&blockCounts.every(n=>n>=1)&&blocks.length>=2;
 const counts={public:tx.filter(t=>t.type==='public').length,transfer:tx.filter(t=>t.type==='transfer').length};
 for(const v of Object.values(perNode))v.transferPercent=v.public+v.transfer?100*v.transfer/(v.public+v.transfer):null;
 const typeMetrics=Object.fromEntries(['public','transfer'].map(type=>[type,{transactions:counts[type],firstInclusionSamples:delayByType[type].length,admissionTransactions:admissionByType[type].length,meanFirstInclusionSeconds:mean(delayByType[type]),p90FirstInclusionSeconds:percentile(delayByType[type],.9),p95FirstInclusionSeconds:percentile(delayByType[type],.95),meanMinerAdmissionMs:mean(admissionByType[type])}]));
 return {schemaVersion:2,experimentId:w.experimentId,runId:w.id,measurementStartedAt:w.measurementStartedAt,measurementEndsAt:w.measurementEndsAt,completeSamples:complete,
  mixture:{targetTransferPercent:w.type==='mixed'?w.transferPercent:100,actualTransferPercent:tx.length?100*counts.transfer/tx.length:null,counts,perNode,cohort:'Formal-phase admitted transactions, all confirmed after drain; Transfer attributed to receiver, Public to sender. This is not necessarily the ratio of broadcasts timestamped inside the window.'},typeMetrics,
  metrics:{meanFirstInclusionSeconds:mean(delays),p90FirstInclusionSeconds:percentile(delays,.9),p95FirstInclusionSeconds:percentile(delays,.95),meanMinerAdmissionMs:mean(admissionPerTx),meanBlockIntervalSeconds:blocks.length>=2?(blocks.at(-1).timestamp-blocks[0].timestamp)/(blocks.length-1):null,canonicalBlockCount:blocks.length,meanCanonicalBlockSizeB:mean(blocks.map(b=>b.size)),meanMinerBlockValidationMs:mean(blockMeans)},
  coverage:{formalTransactions:tx.length,firstInclusionSamples:delays.length,missingDelays,minerCount:miners.length,admissionSamples:admissionCounts.reduce((a,b)=>a+b,0),admissionTransactions:admissionPerTx.length,blockValidationSamples:blockCounts.reduce((a,b)=>a+b,0),validatedBlocks:blockMeans.length,canonicalBlocks:blocks.length},
  definitions:{latency:'Receiver node monotonic clock: earliest successful outbound peer-send start to local canonical head insertion. Includes actual propagation/PoW/local import; excludes controller polling and report collection. Formal submission cohort, may finish in drain.',admission:'First successful pool.validateTx per miner+hash. Includes basic, root, serial and both proof checks; excludes pool-lock queue wait. Average miners per transaction, then transactions.',blocks:'Final observed canonical blocks whose header timestamp is in [measurementStartedAt, measurementEndsAt). Empty blocks included. Interval uses adjacent header timestamps, not PoW solution discovery time.',blockValidation:'Per miner+hash first successful imported-block validation: active header worker (including seal), body (including uncles), state setup + Process + ValidateState phases summed. Excludes header-worker queue wait and block commit. Cached signature prefetch is outside these phases. Own mined blocks are not zero samples. Average miners per block, then blocks.',quantiles:'Sorted zero-based floor((n-1)*p), no interpolation.'}};
}
async function collect(output){
 const s=JSON.parse(fs.readFileSync(path.join(root,'lab/data/lab.json'))),eid=fs.readFileSync(path.join(output,'experiment-id.txt'),'utf8').trim(),rid=fs.readFileSync(path.join(output,'run-id.txt'),'utf8').trim(),e=s.experiments.find(e=>e.id===eid),w=s.workloads.find(w=>w.id===rid);
 if(!e||!w)throw Error('Experiment/run missing');
 const q=x=>"'"+x.replace(/'/g,"'\\''")+"'";
 const records={};
 for(const n of e.nodes){
  const v=s.servers.find(v=>v.id===n.serverId),p=v.workDir+'/experiments/'+eid+'/'+v.id+'/node'+n.localIndex+'/geth.log';
  const files=[p+'.3',p+'.2',p+'.1',p];
  let text;
  if(v.host==='local')text=files.filter(p=>fs.existsSync(p)).map(p=>fs.readFileSync(p,'utf8').split('\n').filter(l=>l.startsWith('PFAP_MEASURE ')).join('\n')).join('\n');
  else {
   const args=['-p',String(v.port||22),'-o','BatchMode=yes','-o','ConnectTimeout=10','-o','StrictHostKeyChecking=yes'];if(v.identityFile)args.push('-i',v.identityFile);if(v.knownHostsFile)args.push('-o','UserKnownHostsFile='+v.knownHostsFile);
   args.push(v.user+'@'+v.host,'for f in '+files.map(q).join(' ')+'; do if [ -f "$f" ] && [ ! -L "$f" ]; then grep "^PFAP_MEASURE " -- "$f" || :; fi; done');
   text=await new Promise((resolve,reject)=>execFile('ssh',args,{timeout:30000,maxBuffer:16*1024*1024},(err,out)=>err?reject(Error('Read metrics failed: '+n.id)):resolve(out)));
  }
  records[n.id]=text.split('\n').filter(l=>l.startsWith('PFAP_MEASURE ')).map(l=>JSON.parse(l.slice(13)));
 }
 fs.writeFileSync(path.join(output,'node-metrics.json'),JSON.stringify(records));
 const result=summarize(JSON.parse(fs.readFileSync(path.join(output,'run.json'))),w,e.nodes,records);
 fs.writeFileSync(path.join(output,'summary.json'),JSON.stringify(result,null,2));
 const rows=[['交易首次上链确认延迟平均值','meanFirstInclusionSeconds','秒'],['交易首次上链确认延迟 P90','p90FirstInclusionSeconds','秒'],['交易首次上链确认延迟 P95','p95FirstInclusionSeconds','秒'],['矿工交易准入验证平均耗时','meanMinerAdmissionMs','ms'],['平均出块间隔（区块头时间戳）','meanBlockIntervalSeconds','秒'],['窗口内规范链区块数','canonicalBlockCount','块'],['窗口内规范链区块平均编码大小','meanCanonicalBlockSizeB','B'],['矿工规范链区块平均验证耗时（有效阶段累计）','meanMinerBlockValidationMs','ms']];
 fs.writeFileSync(path.join(output,'SUMMARY.md'),`# 7 节点 · 20 分钟简表\n\n实验：${eid}；运行：${rid}\n\n正式窗口：${w.measurementStartedAt} → ${w.measurementEndsAt}\n\n采样覆盖：${result.completeSamples?'完整':'不完整，不能作为八项全部有效的结果'}；正式 Transfer ${result.coverage.formalTransactions} 笔。\n\n| 指标 | 结果 |\n| --- | ---: |\n${rows.map(([name,key,unit])=>`| ${name} | ${result.metrics[key]===null?'缺失':result.metrics[key].toFixed(key==='canonicalBlockCount'?0:2)+' '+unit} |`).join('\n')}\n\n延迟取收款节点成功发送的开始至本地规范链收录的单调时钟间隔，不含控制器轮询和 SSH 采集。区块按头时间戳归窗；头/体/执行校验阶段累计不含排队、提交及并行签名预取。具体覆盖与定义见 [summary.json](summary.json)，原始节点埋点见 [node-metrics.json](node-metrics.json)。旧 Web 报告仍采用旧计时，不能替代此简表。\n`);
 console.log(JSON.stringify(result,null,2));
 return result;
}
if(require.main===module){if(!process.argv[2])throw Error('Specify evidence directory');collect(path.resolve(process.argv[2])).catch(e=>{console.error(e.message);process.exitCode=1;});}
module.exports={summarize,mean,percentile,collect};
