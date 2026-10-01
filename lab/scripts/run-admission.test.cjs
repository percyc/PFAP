const {test}=require('node:test'),assert=require('node:assert/strict'),{startRunWhenIdle}=require('./run-admission.cjs');
test('retry only a definite pre-admission busy response',async()=>{
 let calls=0;const result=await startRunWhenIdle(async()=>{if(++calls<3)throw Error('HTTP 409 {"error":"节点命令或交易仍在执行，请等待结束"}');return {id:'one'};},async()=>{});
 assert.equal(calls,3);assert.equal(result.id,'one');
});
test('uncertain delivery and other conflicts never replay',async()=>{
 for(const message of ['fetch failed','HTTP 500','HTTP 409 already active']){let calls=0;await assert.rejects(startRunWhenIdle(async()=>{calls++;throw Error(message);},async()=>{}));assert.equal(calls,1);}
 let calls=0;await assert.rejects(startRunWhenIdle(async()=>{calls++;throw Error('HTTP 409 节点命令或交易仍在执行，请等待结束');},async()=>{},2));assert.equal(calls,2);
});
