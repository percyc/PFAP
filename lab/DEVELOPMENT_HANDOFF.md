# PFAP 开发交接（2026-08-28）

## 2026-09-07 页面使用体验整理

- 后续矿工预览优化：自动预览、手动勾选、新建和已有实验均将草稿已选矿工置前，两组内保留原节点顺序；只调整展示，不改候选清单或请求内容。54 项前端测试与四页响应式浏览器回归通过。Lab 再发布 PID 479299，备份 `lab/data/miner-preview-release.YsVy7H/`，未启动实验。

- 实验改为全宽紧凑列表，按创建时间倒序，支持名称 / ID / Network ID 搜索、状态筛选和每页 10 条分页；一次仅展开一个实验管理区域。停止 / 恢复操作放在节点列表前，矿工配置按需展开。新建表单和构建密钥档案折叠展示，实时刷新保留草稿与展开状态。
- 交易历史按当前实验隔离，增加节点 / 类型 / 哈希搜索、结果筛选和每页 10 条分页。非总览页隐藏大型介绍区域；总览标题缩小，实验卡片、节点和账户历史区域限制高度，保留滚动访问全部记录。
- 新增 `web/workspace.js`、`web/workspace.css`、`app_workspace_test.cjs`、`browser_workspace_check.cjs`。53 项前端单元测试及 `go test ./...` 通过；模拟数据和实际服务在 1440 / 390 px 下四个页面无横向溢出或 JS 错误，验证包含筛选、翻页、跨页管理、展开保持和新建草稿保持。服务器编辑 / 批量操作浏览器回归通过（仅模拟写入）。
- 仅重建并重启 Lab，PID 463651；备份 `lab/data/ux-release.RPBw67/`。实际服务 100 台服务器、4 个已停止实验、63 笔 confirmed / 3 笔 failed，存储健康。发布前后服务器连接 / 网卡 / 宿主机配置、实验状态 / 矿工数量 / 恢复包选择、交易和负载状态一致。未启动实验、未发送交易、未分发或切换此前的 geth 修复包。

## 先读

1. 根目录 [`README.md`](../README.md)：PFAP/geth/libsnark 的构建和 RPC。
2. [`README.md`](README.md)：PFAP Lab 的运行、实验语义、API、指标和限制。
3. 本文：当前机器状态、已完成改造和后续优先级。

## 当前机器状态

> 本节记录的是 2026-08-28 的历史现场，实验、节点、进程和运行包现状应以实时查询为准。2026-09-07 的 Lab 可靠性更新见下节及 Lab README。

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

### 2026-09-07 磁盘管理

磁盘使用率、可用 GiB、采样路径和过期状态已加入服务器卡片，空间告警独立于 SSH 状态；全局告警以及实验部署/矿工配置均可进入原生“磁盘管理”对话框。候选默认不选中，确认中明确说明 DAG 会重新生成，结果包含逐项错误和按实际分配块估算的释放量。清理已执行但结果持久化失败时，仍显示结果并保留存储告警。

部署、恢复和启用矿工前检查工作目录所在文件系统，目录未建立时使用最近的存在父目录。普通节点底线为 1 GiB / 1024 个 inode；每台 worker 按 `1 GiB + N ×（当前与下一 epoch DAG 上界）` 保守预留。共享 DAG 目录不能消除多矿工同时生成临时文件的占用：epoch 0 时 N=1 为 `3 GiB + 8 MiB`，N=2 为 `5 GiB + 16 MiB`，每 epoch 每个矿工增加 16 MiB，部署额外计入压缩包和解压内容。既有 DAG 不抵扣；准入检查不能替代持续观察余量。

部署 worker 尚未发生写入前的预检错误使用 `orchestrator.DeploymentPreflightError` 标记；API 不执行 StopNodes，仅在状态仍为同次 deploying、SHA 一致、未启动且规划节点未变化时恢复草稿并保存原因。配置与交易记录保留，可重新部署；中断、已使用或发生变化的节点清单不会被清除。实现和回归测试见 `internal/api/deployment_preflight.go` / `deployment_preflight_test.go`。

清理只支持仍有 Lab 实验与原部署清单、所有节点已确认停止且无活动/待核验交易的历史 DAG。检查在内存中保留 10 分钟，获准执行即消费，部分失败也不能复用；重启或服务器配置改变需要重查。历史部署 ID 可以不同于重新注册后的服务器 ID。服务端对照检查清单和文件指纹，不信任客户端字节数，清理与生命周期操作互斥，服务器维护期间禁止编辑/删除。清理意图先持久化，失败则不执行。只逐个 unlink 普通 DAG 文件，检查路径、链接和进程占用，保留 chain、SN、keystore、密钥和实验记录；未登记目录不会自动清理。不要把本地模拟测试当成已对当前运行实验执行清理。

日志仅覆盖节点 `geth.log`：64 MiB 触发，每分钟尽力检查，保留当前日志加 3 份归档；忙碌或待核验节点延后。使用 copytruncate 保持文件 inode，复制/截断窗口可能丢少量并发日志，阈值也不是硬上限。计时回填会依次扫描保留的历史归档与当前文件；已落库指标保留，过旧且已被替换的原始日志无法补采。控制器日志仍需独立管理。

实现入口为 `internal/api/disk_management.go`、`log_rotation.go`，worker 检查和文件操作位于 `internal/orchestrator/disk.go`、`cache_cleanup.go`、`log_rotation.go`；UI 在 `web/app.js` / `disk.css`。`internal/api/disk_management_test.go` 用临时 JSON store 和不执行脚本的模拟 SSH 验证清理准入、单次检查和存储故障；`cmd/pfap-lab/app_disk_test.cjs` 覆盖前端选择、告警、部分失败和结果保存告警。

本轮已重新构建并更新 Lab 控制服务，没有重启 geth、发送交易或执行生产 DAG 删除。上线验证：`go test -race ./...`、`go vet ./...`、26 项前端测试通过；真实页面在 1440×1000 和 390×844 下无横向溢出及浏览器异常。重新采样后维持 22 台服务器在线、21 个节点运行、节点 2 不可达，交易为 63 confirmed / 3 failed。只读检查 worker-100 的 `/opt/pfap-lab` 可用空间为 0，两个已停止实验的历史 DAG 合计占用 4,328,534,016 B（约 4.03 GiB），尚未清理；用户可在磁盘管理中确认清理后再恢复节点 2。此次更新前的控制器二进制和状态备份位于 `lab/data/disk-release.nsOLAU/`（目录 0700、状态 0600）。

### 2026-09-07 宿主机分组、手动矿工与服务器分页

服务器增加可选 `hostGroup` 物理宿主机标签；适用于多个 LXC 容器属于同一台 PVE/物理机的情况。此字段由用户填写，不从 IP 推断，大小写和两端空白不影响自动分组。新建、编辑、批量新增和批量修改已有服务器均支持；旧客户端编辑时省略该字段会保留旧值，显式空字符串表示清除。`POST /api/servers/batch/host-group` 接收 `{ids, hostGroup}`，最多 100 台、全量验证后一次保存；标注正在使用的服务器不会重新分配当前矿工，也不执行 SSH。

实验支持 `minerMode: auto|manual`。手动选择持久化 `minerSelections: [{serverId, localIndex}]`，矿工数量由清单派生；自动模式按物理宿主机、组内服务器、服务器内节点分层轮流选择，未标注宿主机时回退到服务器配置维度，并明确提示不能保证物理隔离。新建实验、部署预检和真正部署共用目标解析器；部署准入在保存清单时重新读取宿主机元数据。继续实验和单节点恢复保持已保存角色，不因标签改变而重新分配；旧客户端同数量重试也保留原角色，显式提交自动模式才重新计算分布。

Web 在新建和既有实验中显示矿工节点清单、IP、物理宿主机覆盖、磁盘余量及期望/实际角色。模式切换和勾选仅修改本地草稿，点击应用并确认完整的新旧清单后才保存；运行中先确认目标矿工启动，成功后再停用旧矿工，失败保留可核查的部分结果。事件保存节点、服务器及当时宿主机标签快照。对现有离线节点改变矿工角色仍需先恢复并核验，不把离线当作已停止。

服务器页改为全宽紧凑列表，默认每页 10 台，可选 20/50；支持名称/IP/宿主机搜索、连接与磁盘筛选、自然名称/IP/添加时间排序。批量全选仅作用于当前页，翻页保留选择并提示隐藏数量，确认显示实际目标；新增和编辑使用原生对话框，实时监控刷新不覆盖输入和展开状态。保存成功但状态刷新失败时明确提示已保存，不自动重放请求。服务器页隐藏项目介绍区域，多个磁盘告警默认折叠，保留告警总数。

相关实现：`internal/model/miners.go`、`internal/api/mining.go`、`server_groups.go`、`web/miners.js` / `miners.css`、`web/app.js` / `servers.css`。同时修复安全停止检查中进程在两次 `/proc` 读取之间正常退出导致误报的观察竞态；保留原 PID、启动时间、可执行文件和数据目录归属检查，并对仍存活但已改变归属的进程继续返回未知，绝不追加信号。

本轮验证：`go test -race ./...`、`go vet ./...`、46 项前端测试通过；服务器和矿工界面的模拟桌面 1440×1000 / 手机 390×844 检查通过，无横向溢出或浏览器异常。停止归属竞态的正反回归另通过 `-race -count=10`。补充防止连续刷新解除自动预览禁用、旧节点清单缺失/重复本地编号时的手动选择保护，以及更新接口显式空模式不得覆盖旧配置的测试。

已重新构建并更新 Lab 控制服务（PID 410192），没有重启 geth、发送交易或执行清理。真实页面验证 22 台服务器按 10 台分为 3 页，跨页选择、弹窗草稿、矿工选择预览以及桌面/手机布局均通过；测试禁止所有业务写请求，未发生写入。上线前后比对确认宿主机归属、矿工模式/数量/选择/角色以及交易与自动规则结果不变，存储健康。等待重新采样后，本次观测为 22 台服务器在线、22 个节点运行，交易仍为 63 confirmed / 3 failed。更新前控制器二进制和状态备份位于 `lab/data/inventory-release.HdlJ05/`（目录 0700、状态 0600）。未填写真实宿主机归属、未切换生产矿工；当前运行实验的期望矿工仍为 node-1 / node-2。用户应先在服务器页批量标注同一物理宿主机的容器，再在实验的手动选择中提交矿工清单。

### 2026-09-07 实验通信网卡选择与停止维护等待

服务器编辑框增加“读取服务器网卡”，通过只读 `GET /api/servers/{id}/network` 在已保存服务器上执行固定的 `ip -j address show`（15 秒超时），展示启用且非 DOWN 网卡的非回环/非链路本地 IPv4 地址。不会自动选第一张网卡，选择只填入 `p2pHost` 草稿，保存前确认；不修改 SSH 主机、Web 监听、操作系统路由或网卡配置。活动实验仍锁定通信配置，但可以读取网卡；已有实验需要先安全停止、保存地址、按原目录恢复，并核对 peers。新服务器先保存 SSH 配置再读取，也可手动输入。读取失败保留手动输入且清空旧候选；异步响应不能串入新表单或变更后的 SSH 目标。

停止失败的一个原因是后台日志归档在整次远端检查期间持有全局生命周期锁，原停止接口使用 TryLock 立即返回“正在执行实验操作或磁盘维护”。现在停止准入最多等待 90 秒；原子等待计数使新的自动日志检查在加锁前后都让路，维护仍持有原有锁和远端安全期限。超时不提交停止、不强行解锁、不修改节点或交易。前端保持请求中提示与防重复提交；真正的生命周期状态冲突仍拒绝。测试使用临时 store 与受控锁验证等待、优先级、超时和锁归属，没有对生产实验执行停止测试。

实现位于 `internal/api/network.go` / `network_test.go`、`lifecycle.go` / `stop_admission_test.go`、`log_rotation.go`、`web/network.js`、`app_network_test.cjs`。`go test -race ./...`、`go vet ./...`、51 项前端测试通过；模拟浏览器验证活动锁定、取消/确认保存、草稿刷新保留及桌面/手机布局。仅重新构建和重启 Lab 控制器（PID 422310），更新前备份 `lab/data/network-release.FbDxhd/`。线上只读验证读取到 controller-local 的 ens18 / 192.168.50.219 和 ens19 / 100.99.0.99，另验证 worker-100 的远端网卡读取；无浏览器错误或横向溢出，存储健康。上线前后核对网络配置、实验状态、矿工角色和交易/规则结果未改变；controller-local 的 P2P 地址仍为用户原配置，尚未替用户切换到 100.99.0.99。

### 2026-09-07 经用户授权处理两个退出残留进程

实验 `exp-989791633f1b` 的停止已进入节点退出阶段，并非仍被维护锁阻挡。20 个节点已停止，node-7（100.99.0.105，PID 3000）和 node-12（100.99.0.110，PID 3193）收到 TERM 后持续存活；日志显示链状态写盘、数据库关闭及 Already shutting down。用户明确同意定向强制退出这两个残留进程。

执行前重新核验 PID 文件、启动时间、完整运行包路径及可执行文件 inode/SHA、精确 datadir 参数；确认 IPC 已关闭、最新中断后存在 Database closed 且无链数据库文件描述符。通过 Linux pidfd 向已核验的两个进程发送 SIGKILL，避免 PID 复用误伤；均确认退出。随后调用原有停止接口重新检查全部节点，实验已为 stopped，22/22 节点均确认 stopped，错误为空，交易记录及结果未变。未删除链、账户或密钥文件，未重启节点，未切换 P2P 地址。本次是运维处置，不代表 geth 退出卡住的底层原因已修复；也未增加自动强杀行为。

### 2026-09-07 geth P2P 退出死锁修复（新包待预置到旧实验）

本次 geth 改动仅涉及 `go-ethereum/p2p/server.go` 的两个退出依赖：`Stop` 持有 `srv.lock` 等待网络循环退出，而循环中的 `encHandshakeChecks` 调用 `Self()` 会再次等待该锁；改为读取本轮启动后不变的 `ourHandshake.ID`，仍拒绝自连接。监听循环原先无条件等待握手名额，名额可能由同样等待 `srv.lock` 的 `SetupConn` 占用；改为同时监听 `srv.quit`，停止不再依赖归还名额。保留 Stop 的串行化、进程归属核验和正常 TERM 退出，不增加自动 KILL，不改交易、电路或共识。

新增 `go-ethereum/p2p/shutdown_test.go`，两个确定性测试先在旧代码上均复现超时，修复后 `-race -count=50` 通过。`p2p` 与 `node` 包测试通过；另用 `-tags generic -race -gcflags=all=-d=checkptr=0 -vet=off -count=10` 重复验证通过。该兼容参数用于旧 SHA3 unsafe 实现与 Go 1.24 的 checkptr 冲突，并未关闭数据竞争检测。扩展到 discover/discv5 的测试仍存在旧 URL 错误文案断言及 checkptr 兼容失败，本轮未修改这些模块，不能宣称整个旧 geth 仓库测试全绿。

通过 `scripts/build-compatible-runtime.sh` 在原 Ubuntu 22.04 构建环境重打包，未执行生成密钥操作；8 个 pk/vk 文件与更新前运行包、当前工作区及新包逐一 SHA-256 核对一致。隔离且禁用外网的临时容器中，真实新 geth 连续 3 次正常 TERM 退出，约 104–105 ms；没有使用生产 datadir，也没有启动旧实验。更新前 geth、运行包及构建信息备份位于 `lab/data/shutdown-release.CnFxVy/`。

新 geth SHA 为 `0b58a5e051776b19581de4959d24b76dcbb33e14062bc1db8cb29fe75145936a`；新 `dist/pfap-runtime.tar.gz` SHA 为 `a5532a3fb534040073c1eb03c9bb80609e93d5639ef9ff74047b872d92546efc`。当前实验 `exp-989791633f1b` 仍保持 stopped / 22 个节点 stopped，原 `artifactSha` 为 e4772b…，原首选恢复包为 20c7e2…，尚未切换到新包。已询问用户是否预置新包到当前实验的全部节点并设为下次恢复版本；未收到该选择前不替换旧实验绑定，不启动节点。仅重新生成 dist 不会让旧实验使用本修复。

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
node --test cmd/pfap-lab/app_*_test.cjs

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
dist/pfap-runtime.tar.gz  a5532a3fb534040073c1eb03c9bb80609e93d5639ef9ff74047b872d92546efc
bin/geth                   0b58a5e051776b19581de4959d24b76dcbb33e14062bc1db8cb29fe75145936a
bin/pfap-lab               b5d5e285784534e8e5ddddafdd6b188942f66b9789e69a68dafc34b3fd57cd1c
```

注意：当前 `dist` runtime 是已部署实验使用的 geth 版本。若再次修改 geth，必须
`build.sh geth && build.sh bundle`，新实验才会使用新二进制；已有运行节点不会热更新。

## 下一阶段优先级

2026-09-07 已补充：原运行包/原目录继续实验、逐节点核验停止、控制器重启后的交易待核验、只读 Receipt 核验、自动规则停止新增投递与跳过统计，以及事务式 JSON 保存/备份和页面故障详情。本轮仅改动 Lab，不涉及 geth、证明电路或密钥。相关状态机实现位于 `internal/api/lifecycle.go`、`reconciliation.go`、`workload_control.go`，进程归属检查位于 `internal/orchestrator/lifecycle.go`。新语义及限制以 Lab README 为准；以下为历史待办，部分已由本轮替代。

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
