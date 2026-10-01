// Only this exact 409 is known to occur before workload insertion. Never retry
// transport failures, unknown responses, transaction writes, or other conflicts.
async function startRunWhenIdle(start,pause=ms=>new Promise(r=>setTimeout(r,ms)),attempts=60){
 for(let i=0;i<attempts;i++){
  try{return await start();}catch(e){
   if(!e.message.includes('HTTP 409')||!e.message.includes('节点命令或交易仍在执行，请等待结束')||i===attempts-1)throw e;
   await pause(2000);
  }
 }
}
module.exports={startRunWhenIdle};
