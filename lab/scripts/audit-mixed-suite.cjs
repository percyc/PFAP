// Read-only experiment audit; writes only a generated Chinese comparison report.
const fs=require('node:fs'),path=require('node:path'),assert=require('node:assert/strict');
const {summarize}=require('./summarize-node-metrics.cjs');
const alphas=[0,20,40,60,80,100];
const rows=[
 ['交易首次上链确认延迟平均值','meanFirstInclusionSeconds','秒'],
 ['交易首次上链确认延迟 P90','p90FirstInclusionSeconds','秒'],
 ['交易首次上链确认延迟 P95','p95FirstInclusionSeconds','秒'],
 ['矿工交易准入验证平均耗时','meanMinerAdmissionMs','ms'],
 ['平均出块间隔','meanBlockIntervalSeconds','秒'],
 ['窗口内规范链区块数','canonicalBlockCount','块'],
 ['窗口内规范链区块平均编码大小','meanCanonicalBlockSizeB','B'],
 ['矿工规范链区块平均验证耗时（有效阶段累计）','meanMinerBlockValidationMs','ms']
];
function validateGroup(alpha,summary,report){
 assert.equal(summary.schemaVersion,3,'Require consistent first-inclusion schema');
 assert.equal(summary.latencyPolicy,'first-local-canonical-inclusion','Wrong latency policy');
 assert.equal(summary.mixture.targetTransferPercent,alpha,'Wrong target alpha');
 assert.equal(summary.completeSamples,true,'Incomplete node timing samples');
 assert.equal(Date.parse(summary.measurementEndsAt)-Date.parse(summary.measurementStartedAt),3600000,'Require exactly one formal hour');
 assert.equal(report.runs.length,1,'Require one exported run');
 assert.equal(report.runs[0].completeWindow,true,'Controller did not certify complete window');
 const run=report.runs[0].configuration;
 assert.equal(run.id,summary.runId,'Wrong run');
 assert.equal(run.status,'completed');assert.equal(run.durationSeconds,3600);assert.equal(run.confirmations,6);
 assert.equal(run.nodeIds.length,94);assert.equal(new Set(run.nodeIds).size,94);
 const e=report.experiment;
 assert.equal(e.id,summary.experimentId,'Wrong experiment report');
 assert.equal(e.nodes.length,100,'Require 100 processes');
 assert.equal(e.placements.length,100,'Require 100 placements');
 assert.equal(new Set(e.placements.map(p=>p.serverId)).size,100,'Require distinct servers');
 assert.ok(e.placements.every(p=>p.count===1),'One process per server');
 assert.equal(e.minerCount,5,'Require fixed five-miner layout');
 assert.equal(e.nodes.filter(n=>n.isMiner).length,5);assert.equal(e.minerSelections.length,5);
 assert.ok(e.nodes.every(n=>n.runtimeSha===e.artifactSha),'Mixed node binaries');
 assert.ok(!run.nodeIds.includes(run.observerNodeId),'Observer participates in transactions');
 assert.ok(run.nodeIds.every(id=>e.nodes.some(n=>n.id===id&&!n.isMiner)),'Invalid trader roles');
 assert.equal(e.status,'stopped','Require final stopped export');
 const tx=report.transactions.filter(t=>t.runPhase==='measuring');
 assert.ok(tx.length>0,'No formal transactions');
 assert.ok(tx.every(t=>['public','transfer'].includes(t.type)&&t.status==='confirmed'&&!t.error&&t.hash),'Unresolved or extraneous formal transaction');
 const publicCount=tx.filter(t=>t.type==='public').length,transferCount=tx.filter(t=>t.type==='transfer').length;
 assert.equal(publicCount,summary.mixture.counts.public);assert.equal(transferCount,summary.mixture.counts.transfer);
 if(alpha===0)assert.equal(transferCount,0);else assert.ok(transferCount>0,'Missing required Transfer samples');
 if(alpha===100)assert.equal(publicCount,0);else assert.ok(publicCount>0,'Missing required Public samples');
 for(const [,key] of rows)assert.ok(Number.isFinite(summary.metrics[key])&&summary.metrics[key]>=0,'Invalid metric '+key);
 assert.ok(summary.metrics.canonicalBlockCount>=2,'Need adjacent canonical blocks');
 assert.equal(summary.coverage.formalTransactions,tx.length);
 assert.equal(summary.coverage.firstInclusionSamples,tx.length);
 assert.equal(summary.coverage.admissionTransactions,tx.length);
 assert.equal(summary.coverage.minerCount,5);
 assert.equal(summary.coverage.admissionSamples,tx.length*5,'Miner admission coverage incomplete');
 assert.equal(summary.coverage.validatedBlocks,summary.metrics.canonicalBlockCount);
 assert.equal(summary.coverage.canonicalBlocks,summary.metrics.canonicalBlockCount);
 return {runtime:e.artifactSha,placements:e.placements.map(p=>p.serverId).sort(),miners:e.minerSelections.map(m=>[m.serverId,m.localIndex]).sort()};
}
function audit(directory){
 const result=JSON.parse(fs.readFileSync(path.join(directory,'results.json')));
 assert.equal(result.length,6,'Six completed groups required');
 assert.deepEqual(result.map(r=>r.alpha),alphas,'Wrong group sequence');
 let layout;
 for(const r of result){
  const dir=path.join(directory,'alpha-'+r.alpha),summary=JSON.parse(fs.readFileSync(path.join(dir,'summary.json'))),report=JSON.parse(fs.readFileSync(path.join(dir,'run.json')));
  const {alpha,...stored}=r;assert.deepEqual(stored,summary,'Campaign summary differs from group evidence');
  const records=JSON.parse(fs.readFileSync(path.join(dir,'node-metrics.json')));
  const recomputed=summarize(report,{...report.runs[0].configuration,transferPercent:alpha},report.experiment.nodes,records);
  assert.deepEqual(summary,recomputed,'Stored metrics differ from retained node evidence');
  const current=validateGroup(alpha,summary,report);
  if(layout)assert.deepEqual(current,layout,'Groups differ in runtime, servers or miners');else layout=current;
 }
 const number=(v,integer=false)=>Number.isFinite(v)?v.toFixed(integer?0:3):'不适用';
 const header='| 指标 | '+alphas.map(a=>'α='+a+'%').join(' | ')+' |\n| --- | '+alphas.map(()=>'---:').join(' | ')+' |\n';
 let md='# 六组 100 节点一小时实验结果\n\n每组 100 台服务器各运行 1 个进程：5 个专用矿工、1 个观察节点、94 个交易节点。各比例组使用独立网络，运行版本及服务器/矿工布局相同；失败后复用已核验网络的新窗口另行披露，不能视为相同链龄或累计历史。\n\n';
 md+=header+rows.map(([label,key,unit])=>'| '+label+'（'+unit+'） | '+result.map(r=>number(r.metrics[key],key==='canonicalBlockCount')).join(' | ')+' |').join('\n');
 md+='\n\n## 交易构成与覆盖\n\n'+header;
 for(const [label,get,integer]of [['实际 Transfer 比例（%）',r=>r.mixture.actualTransferPercent,false],['正式准入 Transfer 数',r=>r.mixture.counts.transfer,true],['正式准入 Public 数',r=>r.mixture.counts.public,true],['首次上链延迟样本数',r=>r.coverage.firstInclusionSamples,true],['矿工准入验证样本数',r=>r.coverage.admissionSamples,true]])md+='| '+label+' | '+result.map(r=>number(get(r),integer)).join(' | ')+' |\n';
 md+='\n## 分类型交易指标\n\n'+header;
 for(const type of ['public','transfer'])for(const [label,key,unit]of rows.slice(0,4))md+='| '+type+' · '+label+'（'+unit+'） | '+result.map(r=>number(r.typeMetrics[type][key])).join(' | ')+' |\n';
 md+='\n## 指标解释\n\n- 延迟：实际广播节点（Transfer 收款方、Public 发送方）的成功对外发送开始至本地规范链收录，使用节点单调时钟，不包含控制器轮询和 SSH 报告采集。不是等待六层确认的时间。\n- 交易按正式阶段准入归组，允许在收尾阶段确认；因此实际比例不是严格按广播时刻归窗的比例。每节点实际比例见各组 summary.json。\n- 准入验证：每个矿工对每笔交易首次成功的交易池验证，先按交易汇总矿工样本再取均值；Public 无零知识证明校验。\n- 区块按头时间戳归入窗口，包含空块。出块间隔为相邻规范链区块头时间戳之差；大小为区块编码字节数。\n- 区块验证为头、体、执行和状态核验的有效阶段累计，排除头验证排队、区块提交及并行签名预取，不应解读为完整导入墙钟时间。矿工自己生成的区块不计零耗时样本。\n- P90/P95 使用排序后的 floor((n-1)×p) 下标，不插值。\n- α=0 是同一 PFAP 客户端的普通交易基线，不是原版以太坊。空闲节点闭环调度会随混合比例改变提供负载；六组各一次不支持统计显著性结论。Public 固定 gas=21000、gasPrice=20 Gwei。\n\n## 证据与窗口\n\n';
 for(const r of result)md+='- α='+r.alpha+'%：'+r.measurementStartedAt+' → '+r.measurementEndsAt+'；实验 '+r.experimentId+'；运行 '+r.runId+'；[组内简表](alpha-'+r.alpha+'/SUMMARY.md)、[完整定义和覆盖](alpha-'+r.alpha+'/summary.json)、[运行报告](alpha-'+r.alpha+'/run.html)。\n';
 for(const r of result){
  const config=path.join(directory,'alpha-'+r.alpha,'config.json');
  if(!fs.existsSync(config))continue;
  const c=JSON.parse(fs.readFileSync(config));
  if(c.priorDrainedAttempt)md+='- α='+r.alpha+'% 复用已完成收尾的网络：先前运行 '+[...(c.priorDrainedAttempt.priorRunIds||[]),c.priorDrainedAttempt.runId].join('、')+' 不计入本窗口，不拼接、不重发；链龄、累计状态和缓存历史与全新准备存在差异，详见保留的失败轮次及质量说明。\n';
  if(c.priorWarmupRunIds?.length||c.priorWarmupRunId)md+='- α='+r.alpha+'% 复用已完成收尾的预热网络：先前运行 '+(c.priorWarmupRunIds||[c.priorWarmupRunId]).join('、')+' 从未进入正式窗口，不计入本次指标；新运行重新预热并独立测量，不重发旧交易。累计链龄、状态和缓存历史差异保留披露。\n';
 }
 md+='\n## 重组观测披露\n\n';
 for(const r of result)md+='- α='+r.alpha+'%：'+r.reorgObservations.length+' 笔竞争收录，首次区块后来被重组 '+r.finalInclusionCoverage.firstReorgedTransactions+' 笔；最终重新收录计时缺失 '+r.finalInclusionCoverage.missing.length+' 笔。主指标保留首次进入当时本地规范链的时间，不以最终收录替换，也不删除重组交易；不是最终性。详情见组内 reorgObservations。\n';
 const quality=path.join(directory,'QUALITY_NOTES.md');
 if(fs.existsSync(quality))md+='\n## 运行质量补充\n\n'+fs.readFileSync(quality,'utf8')+'\n';
 fs.writeFileSync(path.join(directory,'FINAL_SUMMARY.zh.md'),md);
 console.log(JSON.stringify({verified:true,groups:6,runtime:layout.runtime,report:path.resolve(directory,'FINAL_SUMMARY.zh.md')}));
}
if(require.main===module){try{if(!process.argv[2])throw Error('Specify completed campaign directory');audit(path.resolve(process.argv[2]));}catch(e){console.error(e.message);process.exitCode=1;}}
module.exports={validateGroup,rows};
