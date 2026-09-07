const test=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const vm=require('node:vm');
const path=require('node:path');
function ui(){
 const choice={options:[],value:'',set innerHTML(value){this.html=value;this.options=Array.from(value.matchAll(/<option/g))}};
 const fields={serverId:{value:'local'},host:{value:'local'},p2pHost:{value:'192.168.50.219',readOnly:false}};
 const elements={'#serverForm':{elements:fields},'#serverNetworkChoice':choice,'#serverEditor':{open:true},'#serverNetworkStatus':{},'#networkReadButton':{}};
 const context=vm.createContext({$:s=>elements[s],serverFormPending:false,state:{servers:[{id:'local',host:'local',p2pHost:'192.168.50.219'}]},esc:s=>String(s).replaceAll('&','&amp;').replaceAll('<','&lt;').replaceAll('"','&quot;')});
 vm.runInContext(fs.readFileSync(path.join(__dirname,'web/network.js'),'utf8'),context);
 context.resetServerNetworkPicker();
 context.api=async()=>({addresses:[{interface:'ens19',address:'100.99.0.99',prefix:16,state:'UP'}]});
 return {context,elements,fields,choice};
}
test('network inspection is read-only and selection only changes the address draft',async()=>{
 const {context:c,fields,choice,elements}=ui();let calls=0;
 c.api=async(path,options)=>{calls++;assert.equal(path,'/servers/local/network');assert.equal(options,undefined);return {addresses:[{interface:'ens<19',address:'100.99.0.99',prefix:16,state:'UP'}]}};
 const before=JSON.stringify(c.state);await c.readServerNetwork();assert.equal(calls,1);
 assert.equal(fields.p2pHost.value,'192.168.50.219');assert.match(choice.html,/ens&lt;19/);assert.equal(choice.disabled,false);
 choice.value='100.99.0.99';c.chooseServerNetwork(choice);assert.equal(fields.p2pHost.value,'100.99.0.99');assert.equal(JSON.stringify(c.state),before);assert.match(elements['#serverNetworkStatus'].textContent,/尚未保存/);
});
test('active experiment permits discovery but blocks selecting an address',async()=>{
 const {context:c,fields,choice,elements}=ui();fields.p2pHost.readOnly=true;
 await c.readServerNetwork();assert.equal(choice.disabled,true);assert.match(elements['#serverNetworkStatus'].textContent,/先安全停止/);
 choice.value='100.99.0.99';c.chooseServerNetwork(choice);assert.equal(fields.p2pHost.value,'192.168.50.219');
});
test('late network response cannot populate a reopened or changed server form',async()=>{
 const {context:c,choice,fields}=ui();let resolve;c.api=()=>new Promise(r=>resolve=r);
 const pending=c.readServerNetwork();c.resetServerNetworkPicker();fields.serverId.value='other';
 resolve({addresses:[{interface:'old',address:'10.0.0.1',prefix:24,state:'UP'}]});await pending;
 assert.doesNotMatch(choice.html,/10.0.0.1/);assert.equal(choice.disabled,true);
});
test('changed SSH target cannot reuse detected choices and failures preserve manual input',async()=>{
 const {context:c,fields,choice,elements}=ui();await c.readServerNetwork();fields.host.value='different';choice.value='100.99.0.99';c.chooseServerNetwork(choice);
 assert.equal(fields.p2pHost.value,'192.168.50.219');assert.match(elements['#serverNetworkStatus'].textContent,/重新读取/);
 fields.host.value='local';c.api=async()=>{throw Error('SSH unavailable')};await c.readServerNetwork();assert.match(elements['#serverNetworkStatus'].textContent,/SSH unavailable/);assert.equal(fields.p2pHost.value,'192.168.50.219');
});
test('pending discovery suppresses duplicates, unsaved servers do not issue requests',async()=>{
 const {context:c,fields,elements}=ui();let calls=0,resolve;c.api=()=>{calls++;return new Promise(r=>resolve=r)};
 const pending=c.readServerNetwork();await c.readServerNetwork();assert.equal(calls,1);resolve({addresses:[]});await pending;
 fields.serverId.value='';await c.readServerNetwork();assert.equal(calls,1);assert.match(elements['#serverNetworkStatus'].textContent,/先保存/);
});
