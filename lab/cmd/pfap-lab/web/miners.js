// Miner choices use placement identity, including before an experiment has an ID.
const newMinerSelection = {mode:'auto', selections:[], seeded:false, removed:0, pending:false, autoCount:null};
function minerSelectionKey(node){return `${node.serverId}:${Number(node.localIndex)}`}
function minerServer(node){return state.servers.find(server=>server.id===node.serverId)||{id:node.serverId,name:node.serverId||'未知服务器',host:''}}
function minerHostGroup(node){return String(minerServer(node).hostGroup||'').trim()}
function minerCandidates(experiment){
 const placements=experiment.placements||[],nodes=experiment.nodes||[],result=[];
 if(nodes.length){
  for(const node of nodes.slice().sort((a,b)=>(Number(a.index)||0)-(Number(b.index)||0)||String(a.id||'').localeCompare(String(b.id||'')))){
   result.push({...node,localIndex:Number(node.localIndex)||0,name:node.name||`node-${Number(node.index)||result.length+1}`});
  }
 }else for(const placement of placements){
  const count=Number(placement.count);if(!Number.isInteger(count)||count<1||count>100)continue;
  for(let localIndex=1;localIndex<=count&&result.length<101;localIndex++)result.push({serverId:placement.serverId,localIndex,index:result.length+1,name:`node-${result.length+1}`});
 }
 return result;
}
function manualMinerManifestProblem(experiment){
 const seen=new Set();
 for(const node of experiment.nodes||[]){
  const localIndex=Number(node.localIndex),key=minerSelectionKey(node);
  if(!node.serverId||!Number.isInteger(localIndex)||localIndex<1||seen.has(key))return '节点部署清单缺少可核验的本地编号，无法安全手动选择';
  seen.add(key);
 }
 return '';
}
function automaticMinerOrder(nodes){
 const servers=new Map();for(const node of nodes){if(!servers.has(node.serverId))servers.set(node.serverId,[]);servers.get(node.serverId).push(node)}
 const hosts=new Map();for(const [id,queue] of servers){const group=minerHostGroup(queue[0]).toLowerCase(),key=group?`known:${group}`:`unknown:${id}`;if(!hosts.has(key))hosts.set(key,[]);hosts.get(key).push(queue)}
 const roundRobin=queues=>{const result=[];for(let i=0;queues.some(queue=>i<queue.length);i++)for(const queue of queues)if(queue[i])result.push(queue[i]);return result};
 return roundRobin([...hosts.values()].map(roundRobin));
}
function savedMinerNodes(experiment,nodes=minerCandidates(experiment)){
 if(experiment.minerMode==='manual'){
  const keys=new Set((experiment.minerSelections||[]).map(minerSelectionKey));return nodes.filter(node=>keys.has(minerSelectionKey(node)));
 }
 if((experiment.nodes||[]).length)return nodes.filter(node=>nodeIsMiner(experiment,node));
 return automaticMinerOrder(nodes).slice(0,configuredMinerCount(experiment));
}
function minerCoverage(nodes){
 const groups=new Map(),unknown=new Set(),servers=new Set();
 for(const node of nodes){servers.add(node.serverId);const group=minerHostGroup(node);if(group)groups.set(group.toLowerCase(),group);else unknown.add(node.serverId)}
 return {known:groups.size,groups:[...groups.values()],unknown:unknown.size,servers:servers.size};
}
function minerCoverageText(nodes){const c=minerCoverage(nodes);return `${nodes.length} 个矿工 · ${c.known} 个已标注物理主机${c.unknown?` · ${c.unknown} 个服务器配置的物理主机未知`:''}`}
function minerNodeText(node){const server=minerServer(node),group=minerHostGroup(node);return `${node.name} / ${server.name} / ${server.host||'IP 未记录'} / ${group?`物理主机 ${group}`:'物理主机未知'}`}
function minerDraft(experiment){
 let draft=minerDrafts.get(experiment.id);
 if(draft&&typeof draft==='object'){
  const allowed=new Set(minerCandidates(experiment).map(minerSelectionKey)),previous=draft.selections;
  draft.selections=previous.filter(key=>allowed.has(key));draft.removed+=previous.length-draft.selections.length;return draft;
 }
 const saved=savedMinerNodes(experiment);
 return {mode:experiment.minerMode==='manual'?'manual':'auto',count:draft===undefined?String(configuredMinerCount(experiment)):String(draft),selections:saved.map(minerSelectionKey),seeded:experiment.minerMode==='manual',removed:0};
}
function minerDraftNodes(experiment,draft,nodes=minerCandidates(experiment)){
 return draft.mode==='manual'?nodes.filter(node=>draft.selections.includes(minerSelectionKey(node))):automaticMinerOrder(nodes).slice(0,Math.max(0,Number(draft.count)||0));
}
function minerDraftValidation(experiment,draft){
 const nodes=minerCandidates(experiment),total=experimentNodeCount(experiment),selected=minerDraftNodes(experiment,draft,nodes),count=draft.mode==='manual'?selected.length:Number(draft.count);
 if(draft.mode==='manual'&&manualMinerManifestProblem(experiment))return manualMinerManifestProblem(experiment);
 if((experiment.placements||[]).some(placement=>!Number.isInteger(Number(placement.count))||Number(placement.count)<1))return '每台节点数必须为 1 至 100 的整数';
 if(total<1)return '请先选择部署服务器并填写每台节点数';
 if(total>100)return '每个实验最多 100 个节点，请减少服务器或每台节点数';
 if(draft.mode==='manual'&&new Set(draft.selections).size!==draft.selections.length)return '矿工节点清单不能重复';
 if(draft.mode==='manual'&&draft.selections.some(key=>!nodes.some(node=>minerSelectionKey(node)===key)))return '部分所选节点已不在部署清单中，请重新选择';
 return Number.isInteger(count)&&count>=1&&count<=Math.min(total,100)?'':`请选择 1 至 ${Math.min(total,100)} 个矿工${draft.mode==='auto'?'，节点数必须为整数':''}`;
}
function minerDraftChanged(experiment,draft){
 if(draft.mode!==(experiment.minerMode==='manual'?'manual':'auto'))return true;
 if(draft.mode==='auto'&&Number(draft.count)!==configuredMinerCount(experiment))return true;
 const keys=nodes=>nodes.map(node=>node.id||minerSelectionKey(node)).sort().join('|');return keys(minerDraftNodes(experiment,draft))!==keys(savedMinerNodes(experiment));
}
function minerRequestBody(experiment,draft){
 const error=minerDraftValidation(experiment,draft);if(error)throw new Error(error);
 return draft.mode==='manual'?{minerMode:'manual',minerSelections:minerDraftNodes(experiment,draft).map(node=>({serverId:node.serverId,localIndex:node.localIndex}))}:{minerMode:'auto',minerCount:Number(draft.count)};
}
function minerChecklist(experiment,draft,{isNew=false,busy=false}={}){
 const nodes=minerCandidates(experiment),selectedNodes=minerDraftNodes(experiment,draft),selected=new Set(selectedNodes.map(node=>node.id||minerSelectionKey(node))),id=isNew?'new':experiment.id,manualProblem=manualMinerManifestProblem(experiment);
 if(!nodes.length)return '';
 // Presentation only: retain candidate order within each group; never reorder deployment or selection payloads.
 const selectedFirst=[...nodes.filter(node=>selected.has(node.id||minerSelectionKey(node))),...nodes.filter(node=>!selected.has(node.id||minerSelectionKey(node)))];
 return `<details class="miner-node-picker" data-detail-key="miner:${esc(id)}:nodes" ${nodes.length<=6?'open':''}><summary>${draft.mode==='manual'?'勾选矿工节点':'查看自动选择预览'} <span>${selected.size} / ${nodes.length}</span></summary><p class="miner-config-hint">已选矿工优先展示，各组内保持原节点顺序；勾选仅修改草稿，提交后才生效。</p><div class="miner-node-options">${selectedFirst.map(node=>{
  const key=minerSelectionKey(node),server=minerServer(node),group=minerHostGroup(node),disk=diskHealth(server),actual=!node.id?'尚未部署':node.status!=='running'?'实际状态未知':node.mining===true?'实际正在挖矿':node.mining===false?'实际未挖矿':'实际状态待采集',desired=node.id?`当前期望${nodeIsMiner(experiment,node)?'矿工':'普通节点'} · `:'';
  return `<label class="miner-node-choice"><input type="checkbox" id="miners-${esc(id)}-node-${esc(node.serverId)}-${node.localIndex}${manualProblem?'-'+esc(node.id||node.name):''}" name="minerNode" value="${esc(key)}" data-miner-experiment="${isNew?'':esc(id)}" data-miner-key="${esc(key)}" ${isNew?'data-new-miner="true"':''} onchange="toggleMinerChoice(this)" ${selected.has(node.id||key)?'checked':''} ${busy||draft.mode!=='manual'||manualProblem?'disabled':''}><span><strong>${esc(node.name)} <small>${esc(server.name)}</small></strong><span>${esc(server.host||'IP 未记录')} · ${group?`物理主机 ${esc(group)}`:'<em>物理主机未知</em>'}</span><small>${esc(diskSpaceLabel(disk))}${disk.stale?' · 采样需更新':''}</small><small>${desired}${actual}</small></span></label>`;
 }).join('')}</div></details>`;
}
function minerPreview(experiment,draft,options={}){
 const selected=minerDraftNodes(experiment,draft),coverage=minerCoverage(selected),error=minerDraftValidation(experiment,draft);
 return `<div class="miner-choice-summary" role="status"><b>${esc(minerCoverageText(selected))}</b>${error?`<span class="miner-health-warning">${esc(error)}</span>`:''}${draft.removed?`<span class="miner-health-warning">部署清单已变化，已移除 ${draft.removed} 个无效选择，请重新核对。</span>`:''}${selected.length===1?'<span class="miner-health-warning">仅 1 个矿工存在出块单点风险。</span>':''}${coverage.servers===1&&selected.length>1?'<span class="miner-health-warning">矿工都在同一台服务器配置上。</span>':''}${coverage.unknown?'<small>未填写物理主机标签的服务器不计入已知主机覆盖；不同 IP 或服务器配置不代表物理隔离。</small>':''}${coverage.known===1&&!coverage.unknown&&selected.length>1?'<small class="miner-health-warning">所选矿工在同一已标注物理主机上。</small>':''}<small>${draft.mode==='auto'?'自动选择优先分散到已标注物理主机；未标注时按服务器配置轮流选择。':'手动选择按勾选清单保存，矿工数量由清单决定。'}${options.isNew?'':' 这是提交后的目标预览，尚未改变当前角色。'}</small></div>${minerChecklist(experiment,draft,options)}`;
}
function minerModeFields(id,draft,busy,isNew=false,manualProblem=''){
 return `<fieldset class="miner-mode-switch"><legend>矿工选择方式</legend>${[['auto','自动分散'],['manual','手动选节点']].map(([mode,label])=>`<label><input type="radio" id="miners-${esc(id)}-mode-${mode}" name="minerMode" value="${mode}" ${draft.mode===mode?'checked':''} ${busy||mode==='manual'&&manualProblem?'disabled':''} ${isNew?'':`data-miner-experiment="${esc(id)}"`} onchange="${isNew?'changeNewMinerMode(this.value)':`changeMinerMode(this.dataset.minerExperiment,this.value)`}">${label}</label>`).join('')}</fieldset>${manualProblem?`<p class="miner-health-warning" role="alert">${esc(manualProblem)}</p>`:''}`;
}
function minerSelectionConfiguration(experiment){
 const draft=minerDraft(experiment),total=experimentNodeCount(experiment),busy=experimentBusy(experiment),editable=['draft','stopped','running'].includes(experiment.status),changed=minerDraftChanged(experiment,draft),error=minerDraftValidation(experiment,draft),saved=savedMinerNodes(experiment),count=draft.mode==='manual'?minerDraftNodes(experiment,draft).length:draft.count,manualProblem=manualMinerManifestProblem(experiment);
 const retry=!changed&&experiment.miningError,needsReapply=!changed&&miningNeedsReapply(experiment),label=busy?'调整中…':retry?'重试应用':needsReapply?'重新应用':'应用配置';
 return `<section class="miner-configuration"><div class="miner-configuration-head"><b>出块配置</b><div class="meta">${miningHealth(experiment)}</div></div><p class="miner-saved-selection"><b>已保存期望 · ${experiment.minerMode==='manual'?'手动':'自动'}</b> ${esc(minerCoverageText(saved))}<span>${esc(saved.map(node=>node.name+' / '+minerServer(node).name).join('、')||'尚无已部署矿工')}</span></p>${editable?`<form class="miner-count-form miner-selection-form" data-miner-form="${esc(experiment.id)}" onsubmit="applyMinerCount(event,this.dataset.minerForm)">${minerModeFields(experiment.id,draft,busy,false,manualProblem)}<div class="miner-selection-actions"><label for="miners-${esc(experiment.id)}">${draft.mode==='manual'?'已选矿工数':'矿工节点数'}<input id="miners-${esc(experiment.id)}" name="minerCount" type="number" min="1" max="${Math.min(total,100)}" step="1" ${draft.mode==='manual'?'readonly':'required'} value="${esc(count)}" data-miner-experiment="${esc(experiment.id)}" oninput="editMinerCount(this,this.dataset.minerExperiment)" ${busy?'disabled':''}></label><button type="submit" class="action secondary miner-apply" ${busy||error||(!changed&&!experiment.miningError&&!miningNeedsReapply(experiment))?'disabled':''}>${label}</button></div><div id="miner-preview-${esc(experiment.id)}">${minerPreview(experiment,draft,{busy})}</div></form>`:''}<p class="miner-config-hint">切换方式与勾选仅修改草稿，点击“应用配置”后才生效。多矿工会改变总算力、出块节奏及分叉概率，性能对比时请保持配置一致。</p>${experiment.status==='running'&&miningStats(experiment).active===0&&!busy?'<p class="miner-health-warning">暂未观测到在线挖矿节点，请检查矿工连接与挖矿状态。</p>':''}${busy?'<p class="miner-config-progress" role="status">正在调整矿工，完成后会自动更新实际状态；请勿重复操作。</p>':''}${experiment.miningError?`<p class="node-recovery-error breakable" role="alert">矿工配置未完全生效：${esc(experiment.miningError)}。请核对各节点实际挖矿状态，再重试。</p>`:''}${experimentDiskHint(experiment)}</section>`;
}
function changeMinerMode(id,mode){
 const experiment=state.experiments.find(item=>item.id===id);if(!experiment||experimentBusy(experiment)||!['auto','manual'].includes(mode))return;
 if(mode==='manual'&&manualMinerManifestProblem(experiment)){toast(manualMinerManifestProblem(experiment));return}
 const draft=minerDraft(experiment);
 if(mode==='manual'&&!draft.seeded){draft.selections=savedMinerNodes(experiment).map(minerSelectionKey);draft.seeded=true}
 draft.mode=mode;minerDrafts.set(id,draft);render();
}
function toggleMinerChoice(input){
 if(input.dataset.newMiner){
  if(newMinerSelection.pending||newMinerSelection.mode!=='manual')return;
  newMinerSelection.selections=newMinerSelection.selections.filter(key=>key!==input.dataset.minerKey);if(input.checked)newMinerSelection.selections.push(input.dataset.minerKey);newMinerSelection.removed=0;updateMinerPlacement();return;
 }
 const experiment=state.experiments.find(item=>item.id===input.dataset.minerExperiment);if(!experiment||experimentBusy(experiment))return;
 if(manualMinerManifestProblem(experiment))return;
 const draft=minerDraft(experiment);if(draft.mode!=='manual')return;
 draft.selections=draft.selections.filter(key=>key!==input.dataset.minerKey);if(input.checked)draft.selections.push(input.dataset.minerKey);minerDrafts.set(experiment.id,draft);render();
}
function editMinerSelectionCount(input,id){
 const experiment=state.experiments.find(item=>item.id===id);if(!experiment||experimentBusy(experiment))return;
 const draft=minerDraft(experiment);draft.count=input.value;minerDrafts.set(id,draft);
 const error=minerDraftValidation(experiment,draft);input.setCustomValidity?.(error);
 const button=input.form?.querySelector('.miner-apply');if(button)button.disabled=!!error||(!minerDraftChanged(experiment,draft)&&!experiment.miningError&&!miningNeedsReapply(experiment));
 patchHTML(`#miner-preview-${id}`,minerPreview(experiment,draft));
}
function newMinerExperiment(){const form=$('#experimentForm'),count=Number(form.elements.count.value);return {placements:[...$('#serverSelect').selectedOptions].map(option=>({serverId:option.value,count}))}}
function newMinerDraft(){return {...newMinerSelection,count:$('#experimentForm').elements.minerCount.value}}
function changeNewMinerMode(mode){
 if(newMinerSelection.pending||!['auto','manual'].includes(mode))return;
 if(mode==='manual'&&newMinerSelection.mode==='auto')newMinerSelection.autoCount=$('#experimentForm').elements.minerCount.value;
 if(mode==='manual'&&!newMinerSelection.seeded){newMinerSelection.selections=minerDraftNodes(newMinerExperiment(),newMinerDraft()).map(minerSelectionKey);newMinerSelection.seeded=true}
 if(mode==='auto'&&newMinerSelection.autoCount!==null)$('#experimentForm').elements.minerCount.value=newMinerSelection.autoCount;
 newMinerSelection.mode=mode;updateMinerPlacement();
}
function updateNewMinerPlacement(){
 const form=$('#experimentForm');if(!form)return;
 const experiment=newMinerExperiment(),input=form.elements.minerCount,total=experimentNodeCount(experiment),allowed=new Set(minerCandidates(experiment).map(minerSelectionKey)),previous=newMinerSelection.selections;
 const validLayout=Number.isInteger(Number(form.elements.count.value))&&Number(form.elements.count.value)>=1&&total<=100;
 if(validLayout||!experiment.placements.length){newMinerSelection.selections=previous.filter(key=>allowed.has(key));newMinerSelection.removed+=previous.length-newMinerSelection.selections.length}
 input.max=String(Math.min(100,Math.max(total,1)));
 if(newMinerSelection.mode==='manual')input.value=String(newMinerSelection.selections.length);else if(!newMinerCountEdited)input.value=String(Math.min(2,Math.max(total,1)));
 input.readOnly=newMinerSelection.mode==='manual';input.required=!input.readOnly;
 for(const field of form.querySelectorAll?.('input,select')||[])field.disabled=newMinerSelection.pending||(field.name==='minerNode'&&newMinerSelection.mode!=='manual');
 const draft=newMinerDraft(),error=minerDraftValidation(experiment,draft);input.setCustomValidity(error);
 const title=$('#newMinerCountLabel');if(title)title.textContent=draft.mode==='manual'?'已选矿工数（由勾选决定）':'矿工节点数';
 const summary=$('#minerPlacementPreview'),html=total?`<b>${experiment.placements.length} 台服务器 · ${total} 个节点</b>${minerPreview(experiment,draft,{isNew:true,busy:newMinerSelection.pending})}`:'先选择部署服务器，并填写每台节点数。';
 const focused=document.activeElement?.dataset?.newMiner?document.activeElement.id:'';
 // patchHTML remembers expanded node lists across telemetry refreshes.
 if(summary){patchHTML('#minerPlacementPreview',html);if(focused)document.getElementById(focused)?.focus({preventScroll:true})}
 const submit=form.querySelector?.('button[type="submit"]');if(submit)submit.disabled=!!error||newMinerSelection.pending;
 patchHTML('#newExperimentDiskHint',diskPlacementHint(experiment.placements.map(placement=>placement.serverId)));
}
async function submitMinerSelection(event,id){
 event.preventDefault();const experiment=state.experiments.find(item=>item.id===id),form=event.currentTarget;if(!experiment||experimentBusy(experiment)||!form.reportValidity())return;
 const draft=minerDraft(experiment);if(draft.mode==='auto')draft.count=form.elements.minerCount.value;
 let body;try{body=minerRequestBody(experiment,draft)}catch(error){toast(error.message);return}
 const previous=savedMinerNodes(experiment),next=minerDraftNodes(experiment,draft),describe=nodes=>`${minerCoverageText(nodes)}\n${nodes.map(minerNodeText).join('\n')||'无'}`;
 if(!confirm(`将实验“${experiment.name}”的矿工选择改为${draft.mode==='manual'?'手动':'自动分散'}？\n\n当前期望：${describe(previous)}\n\n提交后期望：${describe(next)}\n\n${experiment.status==='running'?'控制器会直接启停现有节点的挖矿任务；不会重启节点、清空数据或重发交易。离线节点的变更仍需通过服务端安全检查。':'本次保存期望配置，下一次部署或恢复时应用。'}\n总算力与出块节奏可能变化，本次调整会记录为实验事件。`))return;
 minerRequests.add(id);render();try{const result=await api(`/experiments/${encodeURIComponent(id)}/miners`,{method:'POST',body:JSON.stringify(body)});minerDrafts.delete(id);toast(result?.miningStatus==='updating'||experiment.status==='running'?'矿工配置调整已进入后台，结果将自动更新':'矿工配置已保存');await refresh()}catch(error){toast(error.message);await refresh()}finally{minerRequests.delete(id);render()}
}
async function submitExperimentWithMiners(event){
 event.preventDefault();if(newMinerSelection.pending)return;
 updateMinerPlacement();const form=event.target;if(!form.reportValidity())return;
 const values=Object.fromEntries(new FormData(form)),experiment=newMinerExperiment();let miners;try{miners=minerRequestBody(experiment,newMinerDraft())}catch(error){toast(error.message);return}
 const body={name:values.name,artifactPath:values.artifactPath,networkId:+values.networkId,p2pPortBase:+values.p2pPortBase,rpcPortBase:+values.rpcPortBase,topology:'full-mesh',placements:experiment.placements,...miners};
 newMinerSelection.pending=true;updateMinerPlacement();try{await api('/experiments',{method:'POST',body:JSON.stringify(body)});toast('实验已创建');await refresh()}catch(error){toast(error.message)}finally{newMinerSelection.pending=false;updateMinerPlacement()}
}
