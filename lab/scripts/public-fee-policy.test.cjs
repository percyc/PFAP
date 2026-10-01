const {test}=require('node:test'),assert=require('node:assert/strict'),fs=require('node:fs'),path=require('node:path');
const root=path.resolve(__dirname,'../..');
test('Lab Public fee covers the default mining pool threshold, not merely the ordinary pool',()=>{
 const config=fs.readFileSync(path.join(root,'go-ethereum/eth/config.go'),'utf8');
 const mining=fs.readFileSync(path.join(root,'go-ethereum/eth/api.go'),'utf8');
 const lab=fs.readFileSync(path.join(root,'lab/internal/api/mixed_run.go'),'utf8');
 const gwei=config.match(/GasPrice:\s+big.NewInt\((\d+) \* params.Shannon\)/);
 const fee=lab.match(/gasPrice:\\\"(\d+)\\\"/);
 assert.ok(gwei,'Review changed runtime mining fee defaults');assert.ok(fee,'Lab must explicitly specify Public gas price');
 assert.match(mining,/txPool.SetGasPrice\(price\)/);
 assert.ok(BigInt(fee[1])>=BigInt(gwei[1])*1000000000n,'Local pool acceptance is insufficient: miner threshold must be met');
});
