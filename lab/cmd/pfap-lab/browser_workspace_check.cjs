// NODE_PATH=<Playwright installation>/node_modules node lab/cmd/pfap-lab/browser_workspace_check.cjs
const fs=require('node:fs'),path=require('node:path'),os=require('node:os'),assert=require('node:assert/strict');
const {chromium}=require('playwright');
const output=fs.mkdtempSync(path.join(os.tmpdir(),'pfap-workspace-ui-'));
const fixture={servers:[],experiments:Array.from({length:23},(_,i)=>({id:`exp-${i}`,name:`实验 ${i} · Transfer 多节点基准测试`,status:i===22?'running':'stopped',networkId:55661,minerCount:2,topology:'full-mesh',artifactSha:'a'.repeat(64),createdAt:new Date(2026,0,i+1).toISOString(),nodes:Array.from({length:22},(_,n)=>({id:`node-${n+1}`,name:`node-${n+1}`,index:n+1,status:i===22?'running':'stopped',isMiner:n<2,mining:n<2,account:'0x'+'a'.repeat(40)})),placements:[]})),transactions:Array.from({length:26},(_,i)=>({id:`tx-${i}`,experimentId:'exp-22',type:'mint',status:i===25?'unknown':'confirmed',fromNode:'node-1',submittedAt:new Date(2026,1,i+1).toISOString()})),events:[],workloads:[],accountSnapshots:[]};
(async()=>{const browser=await chromium.launch({headless:true,args:['--no-sandbox']});try{
 const page=await browser.newPage({viewport:{width:1440,height:1000}}),errors=[],writes=[];
 page.on('pageerror',e=>errors.push(e.message));
 await page.addInitScript(()=>{window.EventSource=class{};window.setInterval=()=>0;});
 await page.route('**/*',route=>{const req=route.request(),url=new URL(req.url());if(url.pathname.startsWith('/api/')){if(req.method()!=='GET')writes.push(url.pathname);return route.fulfill({json:url.pathname==='/api/state'?fixture:{}});}const file=path.join(__dirname,'web',url.pathname==='/'?'index.html':url.pathname.slice(1));return route.fulfill({path:file,contentType:file.endsWith('.js')?'application/javascript':file.endsWith('.css')?'text/css':'text/html'});});
 await page.goto('http://workspace.test/');await page.click('[data-tab="experiments"]');
 await page.waitForSelector('.experiment-entry');assert.equal(await page.locator('.experiment-entry').count(),10);assert.match(await page.locator('.experiment-entry').first().innerText(),/实验 22/);assert.equal(await page.locator('.experiment-detail').count(),0);
 await page.locator('[data-manage-experiment="exp-22"]').click();assert.equal(await page.locator('.experiment-detail').count(),1);assert.equal(await page.locator('.node-line').count(),22);
 await page.evaluate(()=>render());assert.equal(await page.locator('.experiment-detail').count(),1);
 await page.locator('[data-manage-experiment="exp-21"]').click();assert.equal(await page.locator('.experiment-detail').count(),1);
 await page.locator('#experimentSearch').fill('exp-0');assert.equal(await page.locator('.experiment-entry').count(),1);
 await page.locator('#experimentSearch').fill('nothing');assert.equal(await page.locator('.experiment-entry').count(),0);
 await page.getByRole('button',{name:'重置筛选'}).click();await page.locator('#experimentPagination').getByRole('button',{name:'下一页'}).click();assert.match(await page.locator('.experiment-entry').first().innerText(),/实验 12/);
 await page.evaluate(()=>showExperiment('exp-0'));assert.equal(await page.locator('[data-experiment="exp-0"]').count(),1);
 await page.getByRole('button',{name:'新建实验',exact:true}).click();await page.locator('#experimentForm input[name="name"]').fill('draft retained');await page.evaluate(()=>render());assert.equal(await page.locator('#experimentForm input[name="name"]').inputValue(),'draft retained');
 await page.click('[data-tab="transactions"]');assert.equal(await page.locator('.tx').count(),10);assert.equal(await page.locator('.tx').first().getAttribute('data-tx'),'tx-25');await page.locator('#transactionStatusFilter').selectOption('active');assert.equal(await page.locator('.tx').count(),1);await page.locator('#transactionStatusFilter').selectOption('');await page.locator('#transactionPagination').getByRole('button',{name:'下一页'}).click();assert.equal(await page.locator('.tx').count(),10);
 for(const width of [1440,390]){await page.setViewportSize({width,height:1000});for(const tab of ['overview','servers','experiments','transactions']){await page.click(`[data-tab="${tab}"]`);await page.evaluate(()=>window.scrollTo(0,0));assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth+1),true,`${tab} overflow at ${width}`);await page.screenshot({path:path.join(output,`${tab}-${width}.png`)});}}
 assert.deepEqual(errors,[]);assert.deepEqual(writes,[]);console.log(JSON.stringify({result:'passed',screenshots:output,browserErrors:errors.length,mutations:writes.length}));
}finally{await browser.close();}})().catch(e=>{console.error(e);process.exitCode=1;});
