let networkReadSequence=0,networkReading=false,networkChoiceIdentity='';
function resetServerNetworkPicker(){
 networkReadSequence++;networkReading=false;networkChoiceIdentity='';
 const choice=$('#serverNetworkChoice');if(!choice)return;
 choice.innerHTML='<option value="">读取网卡后选择，或在上方手动填写</option>';choice.disabled=true;
 $('#serverNetworkStatus').textContent='';$('#networkReadButton').disabled=false;
}
function syncServerNetworkPicker(){
 const form=$('#serverForm'),choice=$('#serverNetworkChoice');if(!choice||!form)return;
 choice.disabled=networkReading||serverFormPending||form.elements.p2pHost.readOnly||choice.options.length<2;
 $('#networkReadButton').disabled=networkReading||serverFormPending;
}
async function readServerNetwork(){
 const form=$('#serverForm'),id=form.elements.serverId.value;
 if(networkReading||serverFormPending)return;
 if(!id){$('#serverNetworkStatus').textContent='请先保存服务器的 SSH 配置；也可以直接手动填写 P2P 地址。';return}
 const server=state.servers.find(s=>s.id===id);
 if(!server||form.elements.host.value!==server.host){$('#serverNetworkStatus').textContent='请先保存修改后的 SSH 主机地址，再读取该服务器网卡。';return}
 const sequence=++networkReadSequence;networkReading=true;networkChoiceIdentity='';$('#serverNetworkChoice').innerHTML='<option value="">正在读取网卡…</option>';syncServerNetworkPicker();$('#serverNetworkStatus').textContent='正在读取已保存服务器的网卡，不修改任何网络配置…';
 const current=()=>sequence===networkReadSequence&&$('#serverEditor').open&&form.elements.serverId.value===id&&form.elements.host.value===server.host;
 try{
  const result=await api(`/servers/${encodeURIComponent(id)}/network`);
  if(!current())return;
  networkChoiceIdentity=id+'\0'+server.host;
  const choice=$('#serverNetworkChoice');choice.innerHTML='<option value="">请选择地址（不会自动选择）</option>'+(result.addresses||[]).map(x=>`<option value="${esc(x.address)}">${esc(x.interface)} · ${esc(x.address)}/${Number(x.prefix)} · ${esc(x.state)}</option>`).join('');
  $('#serverNetworkStatus').textContent=result.addresses?.length?(form.elements.p2pHost.readOnly?'已读取网卡；实验仍在使用此服务器，请先安全停止相关实验，再修改通信地址。':'选择仅填入上方地址，点击“保存修改”后生效；不会立即重连已有节点。'):'未找到已启用网卡的可用 IPv4 地址，可手动填写其他节点可达的地址。';
 }catch(error){if(current())$('#serverNetworkStatus').textContent=error.message}
 finally{if(sequence===networkReadSequence){networkReading=false;if(form.elements.host.value!==server.host)$('#serverNetworkStatus').textContent='SSH 主机已变更，请先保存并重新读取网卡。';syncServerNetworkPicker()}}
}
function chooseServerNetwork(select){
 const form=$('#serverForm');
 if(serverFormPending||form.elements.p2pHost.readOnly||!select.value)return;
 if(networkChoiceIdentity!==form.elements.serverId.value+'\0'+form.elements.host.value){$('#serverNetworkStatus').textContent='SSH 主机已变更，请先保存并重新读取网卡。';return}
 form.elements.p2pHost.value=select.value;
 $('#serverNetworkStatus').textContent='已填入实验通信地址，尚未保存。SSH、Web 和系统路由保持不变；恢复实验时才重新建立节点连接。';
}
