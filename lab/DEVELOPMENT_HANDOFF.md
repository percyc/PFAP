# PFAP 开发交接（2026-08-28）

## 先读

1. 根目录 [`README.md`](../README.md)：PFAP/geth/libsnark 的构建和 RPC。
2. [`README.md`](README.md)：PFAP Lab 的运行、实验语义、API、指标和限制。
3. 本文：当前机器状态、已完成改造和后续优先级。

## 当前机器状态

PFAP Lab 监听：

```text
http://192.168.50.219:8090/
```

控制进程 PID 记录在 `lab/data/server.pid`；不要在文档中固化 PID。密码位于
`lab/data/password`，不要提交或复制到聊天/文档。控制服务重启会使登录 Cookie
失效，但不会停止 geth 节点。

已注册本机 worker：

```text
id:       srv-00f0d4a6a698
name:     controller-local
host:     local
workDir:  /home/percy/pfap/PFAP/lab/worker
```

当前有效实验：

```text
id:          exp-7aeec8e4bec7
name:        transfer-verified
status:      running
networkId:   55662
runtime SHA: 6a2ac07d806625063269a2e76f05e79ddb2848029228220d7e62833f11ad60d5
node-1:      P2P 30000, RPC 40000
node-2:      P2P 30001, RPC 40001
```

该实验已验证双方 CreateAccount、Mint、Public、Redeem 和 Transfer。最后一次检查两
节点均 `running`、互相 `peers=1`。账户余额和最新区块以 `/api/state` 为准，不要
把本文数值当作实时状态。

此外，`test/pow/.network` 下还有一套不受 Lab 管理的旧 3 节点网络，使用端口
`20000..20002`，其中 node-1 会持续挖矿并占用一个 CPU 核。不要误认为它属于
`transfer-verified`；需要释放资源时用 `test/pow/network.sh stop`，不要直接删除
datadir。

## 已完成的兼容改造

### 新 Go / C++ 工具链

- 旧 geth 仍使用 vendored GOPATH；`build.sh` 自动建立
  `.gopath/src/github.com/ethereum/go-ethereum` 链接。
- 针对新 Go runtime 修复 memsize/runtime symbols、BN256 generic 路径和相关 flags。
- 针对新 GCC/CMake 修复 libsnark、libff、libfqfft、gtest 以及 C++ 模板/类型兼容。
- `./build.sh geth`、`./build.sh bundle`、`./build.sh lab` 已通过。

这些变更散布于 `build.sh`、`go-ethereum/vendor`、`go-ethereum/crypto/bn256` 和
`libsnark-vnt`。当前 worktree 很脏，均是项目迁移/开发内容；不要 reset 或覆盖。

### Transfer 正确性

- RPC 使用两阶段 Transfer：付款方 `getPayerNextState`，接收方
  `sendTransferTransaction`。
- Transfer 前检查双方 commitment 已上链。
- 节点级锁防止同一隐私状态并发使用；Transfer 按 ID 排序同时锁双方，避免死锁。
- 修复失败回滚：同时恢复 `SequenceNumber`、`SequenceNumberAfter`、随机数和 stage。
- Receipt 后等待新区块再释放锁，避免紧邻交易遇到 `sn is lost`/空哈希。
- Receipt `status=0x0` 会标记 failed，不能只因 receipt 存在就算成功。

关键文件：

- `go-ethereum/internal/ethapi/api.go`
- `go-ethereum/zktx/zktx.go`
- `lab/internal/api/api.go`
- `lab/internal/orchestrator/orchestrator.go`

### PFAP Lab

- 本机 executor（`host=local`）和 SSH executor。
- runtime 按 SHA 缓存；每实验/节点隔离。
- 自动端口分配、端口预检、失败重部署换端口。
- full-mesh、节点监控、停止、报告导出。
- 密码登录、HttpOnly/SameSite Cookie、退出登录。
- 账户查询、快照和历史；账户状态合并到交易页。
- 实际交易指令预览并保存到 `Transaction.Command`。
- Receipt、哈希、区块、公开/隐私余额和 commitment 展示。
- 增量页面刷新，保留表单、焦点和 Receipt 展开状态。
- 自动规则逐笔保存 workload ID、sequence 和实际 route；规则使用 draining 语义。
- 时间拆分为 queue/proof/verify/chain-confirm/end-to-end。
- 证明与验证保存微秒；按交易哈希解析远端 `geth.log`，启动时回填历史单节点交易。

## 代码地图

```text
lab/cmd/pfap-lab/main.go             HTTP 入口、嵌入静态资源
lab/cmd/pfap-lab/auth.go             密码和 Cookie
lab/cmd/pfap-lab/web/                单页 UI
lab/internal/model/model.go          持久化 JSON 模型
lab/internal/store/store.go          单进程原子 JSON store
lab/internal/remote/remote.go        SSH/local 命令与复制
lab/internal/orchestrator/            runtime 部署、节点启动、attach、日志计时
lab/internal/api/api.go              API、监控、交易、workload、metrics
lab/run-lan.sh                       LAN 启动和密码生成
lab/deploy/pfap-lab.service          loopback systemd 示例
lab/scenarios/zk-baseline.json       设计样例（尚无导入器）
```

## 构建与验证

Web/Lab 修改：

```bash
cd /home/percy/pfap/PFAP/lab
gofmt -w <changed-go-files>
go test ./...
go vet ./...

cd /home/percy/pfap/PFAP
./build.sh lab
```

geth Go 修改：

```bash
cd /home/percy/pfap/PFAP
./build.sh geth
./build.sh bundle
```

C++/电路修改按根 README 执行 libsnark、keys、install-libs、install-keys。约束变化
会使旧 pk/vk 失效，必须让所有节点使用新的一致密钥。

安全重启控制服务（节点会继续运行）：

```bash
cd /home/percy/pfap/PFAP
old_pid=$(cat lab/data/server.pid)
kill "$old_pid"
nohup setsid ./lab/run-lan.sh >lab/data/server.log 2>&1 < /dev/null &
printf '%s\n' "$!" >lab/data/server.pid
```

然后重新登录，检查 `/api/state`、两节点 peer、账户状态和静态资源。不要把上述操作
用于 geth PID；geth 应由实验 Stop 或 `network.sh stop` 管理。

## 当前产物校验值

交接时的产物如下；重新构建后变化是正常的：

```text
dist/pfap-runtime.tar.gz  6a2ac07d806625063269a2e76f05e79ddb2848029228220d7e62833f11ad60d5
bin/geth                   85c5d4a4f32b7c2e41c5c5cb1f48f3ec838261cb48d288de494b9baef798e936
bin/pfap-lab               5aee020635554affaa8e08cc2dd63a394d047d14901797ccb91191a8c04d2c0c
```

注意：当前 `dist` runtime 是已部署实验使用的 geth 版本。若再次修改 geth，必须
`build.sh geth && build.sh bundle`，新实验才会使用新二进制；已有运行节点不会热更新。

## 下一阶段优先级

1. workload/实验维度指标：稳态 TPS、p99、时间序列、CSV/JSON 导出和图表。
2. workload 背压：按节点 capacity/max-inflight 投递，而不只是排队。
3. 失败交易重试/取消，以及明确的可重试错误分类。
4. 场景 JSON 导入、setup/workload/assertions 自动执行。
5. 多服务器真实 SSH 验证（时钟偏差、断线、artifact 缓存、P2P 防火墙）。
6. Docker executor，仅用于 CI/smoke；native 保留为正式性能基线。
7. PostgreSQL、RBAC、审计和 HTTPS 生产化。

## 容易踩坑的地方

- CreateAccount 必须在每个隐私参与者节点执行一次；不要因页面选择错误重复给同一节点开户。
- node ID 与账户地址不同，Public 指令必须把目标 node 解析为目标账户地址。
- `submittedAt` 是历史字段名，语义是进入 Lab 队列，不是广播；广播时间是 `broadcastAt`。
- C++ timing 输出在 geth 日志，不在 attach stdout；计时解析必须按 hash 边界。
- Transfer 的付款 proof 不产生独立链上 hash；总 proof 时间应使用两阶段墙钟或分别采集。
- 总览“历史平均 TPS”跨越空闲时间，不能直接用作论文/实验结论。
- 控制进程重启与节点重启是两件事；不要为更新 Web 而误杀 geth。
- 当前 JSON store 只能有一个写入控制进程。
