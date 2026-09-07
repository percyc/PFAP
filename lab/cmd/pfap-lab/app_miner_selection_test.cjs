const assert=require('node:assert/strict');
const fs=require('node:fs');
const path=require('node:path');
const vm=require('node:vm');
const test=require('node:test');
const source=fs.readFileSync(path.join(__dirname,'web/app.js'),'utf8');
const app=source.slice(0,source.indexOf("document.querySelectorAll('nav button').forEach"));
const miners=fs.readFileSync(path.join(__dirname,'web/miners.js'),'utf8');
function ui(elements={}){
 const context=vm.createContext({document:{querySelector:selector=>elements[selector],querySelectorAll:()=>[]},setTimeout,clearTimeout});
 vm.runInContext(miners,context);vm.runInContext(app,context);context.render=()=>{};context.toast=()=>{};context.refresh=async()=>{};return context;
}
function setState(context,data){context.fixture=data;vm.runInContext('Object.assign(state,fixture)',context)}
function plain(value){return JSON.parse(JSON.stringify(value))}
test('previews put selected miners first without changing identities, candidate order or request payload',()=>{
 const context=ui(),data=fixture();setState(context,data);const e=data.experiments[0],before=JSON.stringify(e);
 for(const isNew of [false,true])for(const mode of ['auto','manual']){
  const experiment=isNew?{...e,nodes:[]}:e,draft={mode,count:'2',selections:['c:2','a:2'],seeded:true};
  const payload=JSON.stringify(context.minerRequestBody(experiment,draft)),candidates=context.minerCandidates(experiment),chosen=new Set(context.minerDraftNodes(experiment,draft).map(context.minerSelectionKey));
  const expected=[...candidates.filter(n=>chosen.has(context.minerSelectionKey(n))),...candidates.filter(n=>!chosen.has(context.minerSelectionKey(n)))].map(context.minerSelectionKey);
  const html=context.minerChecklist(experiment,draft,{isNew}),actual=[...html.matchAll(/data-miner-key="([^"]+)"/g)].map(m=>m[1]);
  assert.deepEqual(actual,plain(expected));assert.equal(JSON.stringify(context.minerRequestBody(experiment,draft)),payload);
 }
 assert.equal(JSON.stringify(e),before);
});
function fixture(){
 const servers=[{id:'a',name:'A',host:'10.0.0.1',hostGroup:'Rack X'},{id:'b',name:'B',host:'10.0.0.2',hostGroup:' rack x '},{id:'c',name:'C',host:'10.0.0.3',hostGroup:'Rack Y'}];
 const placements=servers.map(server=>({serverId:server.id,count:2}));
 const nodes=placements.flatMap((placement,offset)=>[1,2].map(localIndex=>({id:`n${offset*2+localIndex}`,name:`node-${offset*2+localIndex}`,index:offset*2+localIndex,serverId:placement.serverId,localIndex,isMiner:localIndex===1&&placement.serverId!=='c',status:'running',mining:localIndex===1&&placement.serverId!=='c'})));
 return {servers,experiments:[{id:'exp',name:'Choice',status:'running',minerCount:2,placements,nodes}]};
}
test('auto selection matches nested physical-host and server round robin',()=>{
 const context=ui(),data=fixture();setState(context,data);
 const order=context.automaticMinerOrder(context.minerCandidates(data.experiments[0]));
 assert.deepEqual(plain(order.map(node=>node.id)),['n1','n5','n3','n6','n2','n4']);
 const coverage=context.minerCoverage([order[0],order[2]]);assert.equal(coverage.known,1);assert.equal(coverage.unknown,0);
 data.servers[1].hostGroup='';data.servers[1].host=data.servers[0].host;setState(context,data);
 const unknown=context.minerCoverage(data.experiments[0].nodes.filter(node=>node.localIndex===1));
 assert.equal(unknown.known,2);assert.equal(unknown.unknown,1);
 assert.match(context.minerCoverageText([data.experiments[0].nodes[2]]),/0 个已标注物理主机.*物理主机未知/);
});
test('host metadata changes produce an explicit same-count preview without changing saved roles',()=>{
 const context=ui(),data=fixture();setState(context,data);
 context.api=()=>assert.fail('metadata or preview must not apply roles');
 const experiment=data.experiments[0],draft=context.minerDraft(experiment);
 assert.deepEqual(plain(context.savedMinerNodes(experiment).map(node=>node.id)),['n1','n3']);
 assert.deepEqual(plain(context.minerDraftNodes(experiment,draft).map(node=>node.id)),['n1','n5']);
 assert.equal(context.minerDraftChanged(experiment,draft),true);
 const html=context.minerConfiguration(experiment);
 assert.match(html,/已保存期望/);assert.match(html,/尚未改变当前角色/);assert.match(html,/miner-apply" >应用配置/);
 assert.equal(data.experiments[0].nodes[2].isMiner,true);assert.equal(data.experiments[0].nodes[4].isMiner,false);
});
test('switching to manual seeds persisted desired nodes and mode changes are draft-only',()=>{
 const context=ui(),data=fixture();setState(context,data);context.api=()=>assert.fail('switching modes must not write');
 context.changeMinerMode('exp','manual');
 const draft=context.minerDraft(data.experiments[0]);assert.equal(draft.mode,'manual');assert.deepEqual(plain(draft.selections),['a:1','b:1']);
 assert.deepEqual(plain(context.minerRequestBody(data.experiments[0],draft)),{minerMode:'manual',minerSelections:[{serverId:'a',localIndex:1},{serverId:'b',localIndex:1}]});
 context.toggleMinerChoice({dataset:{minerExperiment:'exp',minerKey:'c:2'},checked:true});
 assert.equal(context.minerDraftNodes(data.experiments[0],context.minerDraft(data.experiments[0])).length,3);
 context.changeMinerMode('exp','auto');context.changeMinerMode('exp','manual');
 assert.ok(context.minerDraft(data.experiments[0]).selections.includes('c:2'),'manual draft must survive mode switches');
});
test('manual apply confirms old/new nodes and known hosts and suppresses duplicate pending writes',async()=>{
 const context=ui(),data=fixture();setState(context,data);context.changeMinerMode('exp','manual');
 context.toggleMinerChoice({dataset:{minerExperiment:'exp',minerKey:'b:1'},checked:false});
 context.toggleMinerChoice({dataset:{minerExperiment:'exp',minerKey:'c:2'},checked:true});
 let confirmation='',finish;const requests=[];
 context.confirm=message=>{confirmation=message;return false};
 const event={preventDefault(){},currentTarget:{reportValidity:()=>true,elements:{minerCount:{value:'2'}}}};
 context.api=()=>assert.fail('dismissed confirmation must not submit');await context.applyMinerCount(event,'exp');
 assert.match(confirmation,/当前期望：[\s\S]*node-3/);assert.match(confirmation,/提交后期望：[\s\S]*node-6/);assert.match(confirmation,/2 个已标注物理主机/);
 context.confirm=()=>true;context.api=(url,options)=>{requests.push({url,body:JSON.parse(options.body)});return new Promise(resolve=>{finish=resolve})};
 const pending=context.applyMinerCount(event,'exp');await context.applyMinerCount(event,'exp');
 assert.deepEqual(requests,[{url:'/experiments/exp/miners',body:{minerMode:'manual',minerSelections:[{serverId:'a',localIndex:1},{serverId:'c',localIndex:2}]}}]);
 const busy=context.minerConfiguration(data.experiments[0]);assert.match(busy,/miner-apply" disabled>调整中/);assert.match(busy,/type="radio"[^>]*disabled/);
 finish({miningStatus:'updating'});await pending;
});
test('manual validation rejects empty, duplicate, and stale selections',()=>{
 const context=ui(),data=fixture();setState(context,data);const experiment=data.experiments[0];
 assert.throws(()=>context.minerRequestBody(experiment,{mode:'manual',selections:[],count:'2'}),/1 至 6/);
 assert.throws(()=>context.minerRequestBody(experiment,{mode:'manual',selections:['a:1','a:1'],count:'2'}),/重复/);
 assert.throws(()=>context.minerRequestBody(experiment,{mode:'manual',selections:['outside:1'],count:'2'}),/部署清单/);
 assert.throws(()=>context.minerRequestBody({...experiment,nodes:[],placements:[{serverId:'a',count:101}]},{mode:'auto',count:'2',selections:[]}),/最多 100/);
 const html=context.minerChecklist(experiment,{mode:'manual',count:'1',selections:['b:1']});
 assert.match(html,/node-3/);assert.match(html,/10\.0\.0\.2/);assert.match(html,/物理主机 rack x/);assert.match(html,/磁盘空间尚未采集/);assert.match(html,/当前期望矿工 · 实际正在挖矿/);
});
function newFormContext(){
 const input={value:'1',setCustomValidity(value){this.validation=value}},count={value:'2'},select={selectedOptions:[{value:'a'},{value:'b'}]},preview={innerHTML:''};
 const form={elements:{minerCount:input,count},reportValidity:()=>true,querySelector:()=>null};
 const context=ui({'#experimentForm':form,'#serverSelect':select,'#minerPlacementPreview':preview});setState(context,fixture());return {context,form,input,count,select,preview};
}
test('new manual choices use placement IDs, derive count and visibly prune removed nodes',()=>{
 const {context,input,select,preview}=newFormContext();context.updateMinerPlacement();context.changeNewMinerMode('manual');
 let body=context.minerRequestBody(context.newMinerExperiment(),context.newMinerDraft());
 assert.deepEqual(plain(body),{minerMode:'manual',minerSelections:[{serverId:'a',localIndex:1},{serverId:'b',localIndex:1}]});
 context.toggleMinerChoice({dataset:{newMiner:'true',minerKey:'a:2'},checked:true});assert.equal(input.value,'3');
 select.selectedOptions=[{value:'b'}];context.updateMinerPlacement();assert.equal(input.value,'1');assert.match(preview.innerHTML,/已移除 2 个无效选择/);
 body=context.minerRequestBody(context.newMinerExperiment(),context.newMinerDraft());assert.deepEqual(plain(body.minerSelections),[{serverId:'b',localIndex:1}]);
 context.toggleMinerChoice({dataset:{newMiner:'true',minerKey:'b:1'},checked:false});assert.equal(input.value,'0');assert.notEqual(input.validation,'');
});
test('new experiment submits an explicit manual list once without requiring experiment IDs',async()=>{
 const {context,form}=newFormContext();context.updateMinerPlacement();context.changeNewMinerMode('manual');
 context.FormData=class{constructor(){return Object.entries({name:'Created',artifactPath:'dist/runtime.tgz',networkId:'12',p2pPortBase:'0',rpcPortBase:'0'})}};
 let finish;const requests=[];context.api=(url,options)=>{requests.push({url,body:JSON.parse(options.body)});return new Promise(resolve=>{finish=resolve})};
 const event={target:form,preventDefault(){}};const pending=context.submitExperimentWithMiners(event);await context.submitExperimentWithMiners(event);
 assert.equal(requests.length,1);assert.equal(requests[0].url,'/experiments');assert.equal(requests[0].body.minerMode,'manual');assert.equal('minerCount' in requests[0].body,false);
 assert.deepEqual(requests[0].body.minerSelections,[{serverId:'a',localIndex:1},{serverId:'b',localIndex:1}]);
 finish({id:'new'});await pending;
});
test('temporarily invalid node-count edits preserve the new manual draft',()=>{
 const {context,count}=newFormContext();context.updateMinerPlacement();context.changeNewMinerMode('manual');
 const selected=plain(context.newMinerDraft().selections);
 for(const value of ['', '1.5', '101']){count.value=value;context.updateMinerPlacement();assert.deepEqual(plain(context.newMinerDraft().selections),selected)}
 count.value='2';context.updateMinerPlacement();assert.equal(context.minerDraftValidation(context.newMinerExperiment(),context.newMinerDraft()),'');
});
test('repeated identical refresh keeps auto previews disabled, preserves manual drafts, and disables all pending fields',()=>{
 const {context,form}=newFormContext();
 const controls=[{name:'name'},{name:'count'},{name:'minerMode'},{name:'serverId'},{name:'minerNode'},{name:'minerNode'}];
 form.querySelectorAll=()=>controls;
 const assertAuto=()=>{assert.ok(controls.filter(field=>field.name==='minerNode').every(field=>field.disabled));assert.ok(controls.filter(field=>field.name!=='minerNode').every(field=>!field.disabled))};
 context.updateMinerPlacement();assertAuto();context.updateMinerPlacement();assertAuto();
 context.changeNewMinerMode('manual');assert.ok(controls.every(field=>!field.disabled));
 context.toggleMinerChoice({dataset:{newMiner:'true',minerKey:'a:2'},checked:true});
 const selected=plain(context.newMinerDraft().selections);
 context.updateMinerPlacement();context.updateMinerPlacement();assert.deepEqual(plain(context.newMinerDraft().selections),selected);assert.ok(controls.every(field=>!field.disabled));
 vm.runInContext('newMinerSelection.pending=true',context);context.updateMinerPlacement();context.updateMinerPlacement();assert.ok(controls.every(field=>field.disabled));
 vm.runInContext('newMinerSelection.pending=false',context);context.updateMinerPlacement();assert.ok(controls.every(field=>!field.disabled));
 context.changeNewMinerMode('auto');context.updateMinerPlacement();assertAuto();
 context.changeNewMinerMode('manual');context.updateMinerPlacement();assert.deepEqual(plain(context.newMinerDraft().selections),selected);assert.ok(controls.every(field=>!field.disabled));
});
test('legacy nodes with missing, zero or duplicate local indices cannot invent manual targets or submit',async()=>{
 for(const kind of ['missing','zero','duplicate']){
  const context=ui(),data=fixture(),experiment=data.experiments[0];
  if(kind==='missing')delete experiment.nodes[0].localIndex;
  else if(kind==='zero')experiment.nodes[0].localIndex=0;
  else experiment.nodes[1].localIndex=experiment.nodes[0].localIndex;
  const before=JSON.stringify(experiment.nodes);setState(context,data);
  context.api=()=>assert.fail('invalid legacy manifest must not submit manual roles');
  assert.match(context.manualMinerManifestProblem(experiment),/缺少可核验的本地编号/);
  const html=context.minerConfiguration(experiment);assert.match(html,/id="miners-exp-mode-manual"[^>]*disabled/);assert.doesNotMatch(html,/id="miners-exp-mode-auto"[^>]*disabled/);assert.match(html,/无法安全手动选择/);
  context.changeMinerMode('exp','manual');assert.equal(context.minerDraft(experiment).mode,'auto');
  const draft={mode:'manual',count:'1',selections:['a:1'],seeded:true,removed:0};
  assert.throws(()=>context.minerRequestBody(experiment,draft),/缺少可核验的本地编号/);
  context.invalidDraft=draft;vm.runInContext("minerDrafts.set('exp',invalidDraft)",context);
  await context.applyMinerCount({preventDefault(){},currentTarget:{reportValidity:()=>true,elements:{minerCount:{value:'1'}}}},'exp');
  assert.equal(JSON.stringify(experiment.nodes),before,'legacy node manifest must not be rewritten');
  if(kind!=='duplicate')assert.equal(context.minerCandidates(experiment)[0].localIndex,0,'must not guess a positive local index');
  assert.deepEqual(plain(context.minerRequestBody(experiment,{mode:'auto',count:'2',selections:[]})),{minerMode:'auto',minerCount:2});
 }
});
test('experiment refresh restores the focused manual checkbox identity',()=>{
 const target={innerHTML:'',querySelectorAll:()=>[]},context=ui({'#experimentRows':target});let focused='';
 context.document.activeElement={id:'miners-exp-node-c-2',dataset:{minerExperiment:'exp'}};
 context.document.getElementById=id=>({focus(){focused=id}});
 context.renderExperimentRows('<section>refreshed telemetry</section>');assert.equal(focused,'miners-exp-node-c-2');
});
