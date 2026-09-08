const fs=require('node:fs'),vm=require('node:vm'),path=require('node:path'),test=require('node:test'),assert=require('node:assert/strict');
const source=fs.readFileSync(path.join(__dirname,'web/app.js'),'utf8');
function render(status,events){
 const context=vm.createContext({Date});
 vm.runInContext(source.slice(source.indexOf('function deploymentProgress(')),context);
 context.state={events};context.esc=x=>String(x).replaceAll('<','&lt;');context.experimentNodeCount=()=>300;
 return context.deploymentProgress({id:'e',status});
}
test('deployment progress uses real counts and escapes target labels',()=>{
 const html=render('deploying',[{experimentId:'e',kind:'deploy-progress',at:new Date().toISOString(),fields:{stage:'上传运行包',server:'<worker>',totalNodes:300,completedNodes:22}}]);
 assert.match(html,/22 \/ 300/);assert.match(html,/&lt;worker>/);assert.match(html,/value="22"/);assert.match(html,/仍需组网/);
});
test('legacy deployment does not invent a percentage and completed runs hide progress',()=>{
 assert.match(render('deploying',[]),/无精确节点进度/);
 assert.doesNotMatch(render('deploying',[]),/<progress/);
 assert.equal(render('running',[]),'');
});
test('new attempt discards previous progress',()=>{
 const html=render('deploying',[{experimentId:'e',kind:'deploy-progress',at:'2026-01-01',fields:{completedNodes:300,totalNodes:300}},{experimentId:'e',kind:'lifecycle',message:'deployment started',at:'2026-01-02'}]);
 assert.doesNotMatch(html,/<progress/);
});
