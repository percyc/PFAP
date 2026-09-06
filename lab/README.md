# PFAP Lab

PFAP Lab 是 PFAP 多节点实验的 Web 控制面。它可以在本机或多台 SSH 服务器上部署同一份不可变 runtime，启动每台机器上的多个 geth 节点，建立 full-mesh 网络，执行预设交易或自动负载，并保存交易凭证、账户状态及性能数据。

当前实现面向单实验人员、可信局域网和可复现实验，不是公网多租户平台。

## 快速开始

在仓库根目录执行：

```bash
cd /home/percy/pfap/PFAP

# geth / C++ 有变化时重建对应组件；仅 Web 变化可跳过 geth
./build.sh geth
./build.sh bundle
./build.sh lab

# 监听所有网卡，适合可信局域网
./lab/run-lan.sh
```

`run-lan.sh` 默认监听 `0.0.0.0:8090`，首次启动会生成随机 Web 密码到 `lab/data/password`。密码不会写入项目文档，该文件权限应保持 `0600`。控制服务重启后登录 Cookie 会失效，需要重新登录。

### 新控制机首次安装（推荐）

控制机负责编译 runtime、运行 Web 控制面并通过 SSH 管理 worker。兼容 runtime 的 C/C++ 部分在 Ubuntu 22.04 Docker 镜像中编译；Go 1.24 工具链从控制机只读挂载到容器。控制机需要 `docker`、`go 1.24`、`git`、`bash` 和 `sha256sum`，不要求宿主机安装 libsnark 的 C++ 依赖。

```bash
git clone <PFAP repository URL>
cd PFAP

# 首次会构建 Ubuntu 22.04 编译镜像；之后复用 Docker 和增量编译缓存
./scripts/build-compatible-runtime.sh

# 构建并启动 Web 控制面
./build.sh lab
./lab/run-lan.sh
```

成功后应存在 `dist/pfap-runtime.tar.gz` 和对应的 `.sha256` 文件。可在相同基线容器中做启动检查：

```bash
docker run --rm -v "$PWD/dist:/dist:ro" pfap-runtime-builder:ubuntu22 \
  bash -lc 'mkdir /tmp/pfap && tar -xzf /dist/pfap-runtime.tar.gz -C /tmp/pfap && \
  LD_LIBRARY_PATH=/tmp/pfap/pfap-runtime/lib /tmp/pfap/pfap-runtime/bin/geth version'
```

不要在普通更新中执行 `./build.sh keys`：重新生成 pk/vk 会改变证明参数。只有电路约束改变并计划创建全新实验网络时才应重新生成密钥。

需要明确轮换密钥时，先停止所有使用旧 runtime 的实验，再执行：

```bash
./scripts/build-compatible-runtime.sh --generate-keys
```

该命令会在兼容容器中生成四类交易的 pk/vk、写入 `dist/build-profile.json`、重新构建并打包 runtime。Lab 部署时按 runtime SHA 将同一套密钥分发给实验的所有节点。

可参考 [`lab.env.example`](lab.env.example) 设置自定义端口和数据路径。直接以 loopback 方式运行：

```bash
./bin/pfap-lab \
  -listen 127.0.0.1:8090 \
  -data ./lab/data/lab.json \
  -password-file ./lab/data/password
```

## 构建产物

| 产物 | 说明 |
| --- | --- |
| `bin/geth` | 支持 PFAP RPC 的 geth |
| `bin/pfap-lab` | Web 控制服务，静态页面已嵌入二进制 |
| `dist/pfap-runtime.tar.gz` | 多服务器部署包，包含 geth、动态库和同一套 `prfKey` |
| `lab/data/lab.json` | 实验、交易、Receipt、快照和负载状态 |
| `lab/worker/artifacts/<sha>/` | 本机 worker 按 SHA-256 缓存的 runtime |
| `lab/worker/experiments/<id>/` | 节点 datadir、IPC、PID 与日志 |

同一网络的所有节点必须使用完全相同的 runtime SHA 和 `prfKey`。

维护中的兼容性恢复补丁是例外：必须保持电路和 pk/vk 完全一致，不改变共识规则。实验的原 `artifactSha` 不覆盖；`recoveryArtifactSha` 记录已预置的恢复包，节点只有实际重启后才记录新的 `runtimeSha`。健康进程不因配置恢复包而重启，报告会保留这些版本信息。不要用这个机制混用不同电路或重新生成的密钥。

## 服务器与部署

### 本机 worker

在“服务器”页面添加：

- 主机：`local`
- 工作目录：当前用户可写目录，例如 `/home/percy/pfap/PFAP/lab/worker`
- P2P 公告地址：其他服务器能访问的本机局域网 IP

本机执行不经过 SSH，但与远端 worker 使用相同的 artifact 缓存、端口检查和实验隔离结构。

### SSH worker

每台远端服务器需要：

- 专用非特权用户和可写工作目录；
- 已加入 `known_hosts` 的主机密钥；
- 非交互式密钥认证；
- `bash`、`tar`、`sha256sum`、`setsid`、`ss`；单节点恢复还需要 `timeout`、`flock`，建议安装 `fuser`（未安装时使用 `/proc` 检查目录占用）；
- 实验前完成时钟同步；
- 防火墙允许实验使用的 P2P 端口。

推荐的 worker 基线是 `x86_64/amd64`、Ubuntu 22.04 或更新版本（glibc 不低于 2.35）。worker 不需要 Docker、Go、GCC、CMake、libsnark 源码或 GMP 开发包；geth 和所需的 C++/GMP 运行库都包含在 runtime 中。ARM64 或 glibc 低于 2.35 的机器需要单独的构建产物，不能直接混入当前实验。

新增一台 worker 的参考准备命令（用户名和路径可自行替换）：

```bash
sudo useradd --create-home --shell /bin/bash pfap
sudo install -d -o pfap -g pfap /opt/pfap-worker
sudo apt-get update
sudo apt-get install -y openssh-server bash tar coreutils util-linux iproute2 psmisc
```

将控制机公钥加入 worker 的 `/home/pfap/.ssh/authorized_keys`，并确认可非交互登录：

```bash
ssh -i /path/to/private_key pfap@WORKER_IP 'uname -m; getconf GNU_LIBC_VERSION; command -v bash tar sha256sum setsid ss'
```

随后在 Lab“服务器”页面添加：主机地址、SSH 端口、用户 `pfap`、私钥路径、工作目录 `/opt/pfap-worker`，以及其他节点能访问的 P2P 公告地址。点击“信任 SSH 主机密钥”，通过可信渠道核对指纹后再保存；然后点击“检查连接”。控制面会在正式上传和创建账户前检查架构、glibc、动态库及命令依赖。

防火墙至少需要允许控制机访问 SSH，并允许实验节点之间访问所分配的 P2P TCP/UDP 端口。RPC 默认只供控制面使用，不应直接暴露到公网。

首次连接采用 TOFU 保存主机密钥，控制面不会关闭 `StrictHostKeyChecking`。更换或重装服务器后若主机密钥变化，必须先核对新指纹，不能直接绕过告警。

服务器页面支持编辑、删除和批量添加。批量添加接受换行、逗号、空格或分号分隔的 IP/主机名列表，并为它们应用同一套 SSH 用户、端口、私钥和工作目录；后端会先完成全部校验再一次性保存，最多 100 台。活动实验使用的服务器只能修改显示名称，连接信息必须在实验停止后修改。服务器被活动或草稿实验引用时不能删除；只被已停止/失败的历史实验引用时可以删除配置，历史实验数据不会被级联删除，远端文件也不会被删除。

runtime 缓存在 `<workDir>/artifacts/<sha256>`；实验位于 `<workDir>/experiments/<experiment-id>`。同一主机的多个节点使用不同 datadir、P2P/RPC 端口，共享只读 runtime、证明密钥和 Ethash DAG。

P2P/RPC 起始端口填 `0` 时自动分配。部署前通过 `ss` 检查冲突；失败实验再次部署时也会重新选择端口。

## 推荐实验流程

1. 构建 `dist/pfap-runtime.tar.gz`。
2. 添加并检查所有服务器。
3. 新建实验，选择每台服务器的节点数和矿工数量。
4. 部署并确认节点为 `running`，peer 数、实际挖矿数量符合预期。
5. 对每个参与隐私交易的节点执行一次 `CreateAccount`。
6. 对付款节点执行 `Mint`。
7. 执行 `Transfer` / `Redeem`，或启动自动规则。
8. 核对账户状态、Receipt、区块号和阶段耗时。
9. 导出实验 JSON 报告。

不要在同一个隐私账户上并发执行 ZK 状态交易。控制面为每个节点维护互斥锁；Transfer 会同时锁住付款方与接收方。Receipt 确认后还会等待新区块，避免下一笔交易在节点隐私序列状态尚未稳定时启动。

入队时还会检查所有参与节点：存在 `queued / proving / submitted` 交易时，手动请求返回冲突，自动负载跳过本次投递并等待后续周期，不会在节点锁后无限堆积。

### 矿工数量与出块可用性

实验可以配置 `1..节点总数` 个矿工。新实验默认 2 个（只有一个节点时为 1 个）；历史实验升级后仍保留原来的 1 个矿工，不会自动改变实验基线。选择时优先按节点序号跨服务器分配，再选择每台服务器的后续节点。服务器配置不同不一定代表物理机不同，应由实验人员确认故障隔离。

“实验”详情支持保存草稿/停止实验的矿工数量，也支持对运行中实验点击“应用矿工配置”，不重启节点、不清空链、不重新发送交易。部署时先启动所有节点并建立拓扑，再启动矿工；在线调整时先启用并确认目标矿工，再停止被移出的矿工。配置中的角色与实际 `eth.mining` 分开展示，不可达或尚未采样的节点不计入在线挖矿数量。`eth.mining=true` 表示挖矿工作已启用，并非保证已经产出区块，还需观察链高度和 peer 连接。

在线调整失败可能只部分生效，页面会保留目标配置、错误和实测状态，可排除故障后重试相同数量。涉及角色变化的离线节点必须先恢复；恢复、启停和矿工调整不能同时执行。控制器重启中断配置时也会明确显示未完成，不自动重试命令。配置及操作事件随实验报告导出。

只有 1 个矿工时，该节点掉线会暂停出块及交易确认，但不意味着实验数据丢失。多个相互连通、正常挖矿的节点可让其余节点在一个矿工掉线后继续出块；已经故障的交易参与节点仍需单独恢复。要减少主机故障的影响，应把至少 2 个矿工放在不同物理服务器上。

增加矿工也会改变 CPU/内存/DAG 开销、总算力、出块节奏及分叉概率；运行中修改会改变统计基线，严谨对比应使用不同实验并保留配置记录。本功能是出块冗余，不是完整的生产级高可用保证：网络分区、全部矿工失联、磁盘损坏仍会影响实验；当前 PFAP 全局 SMT 在运行中链重组/候选块回滚时的隔离限制仍存在，多矿工隐私交易实验需额外核对链与账户状态。

API：创建实验时传 `minerCount`；调整使用 `POST /api/experiments/{id}/miners`，请求体 `{"minerCount":2}`。运行中返回 `202`，仅表示操作已接受，应继续检查 `miningStatus`、`miningError` 与节点实测 `mining`。

### 实验中途节点掉线与恢复

`unreachable` 表示控制面无法访问节点 IPC，不一定代表服务器离线。先查看“服务器”的连接状态；服务器关机、SSH 不通或磁盘满时，要先恢复主机条件。

实验仍处于 `running` 时，可以在“实验”的节点行，或“交易”的账户卡片点击 **恢复节点**。该操作会：

1. 核对原 runtime SHA、datadir、账户和端口；缺少原数据时拒绝创建新链。
2. 只对已退出的进程执行原地启动。进程尚在但 IPC 不可用时保留进程并显示日志，不强制终止证明计算。
3. 重建节点间连接；按照实验配置恢复矿工的出块角色，不再固定为 node-1。
4. 查询原账户与链状态，保留交易记录，不再次执行 CreateAccount，不重发任何交易。

恢复还会核对已初始化账户是否仍能查询到有效链上隐私状态；异常时显示独立错误并暂停该节点的隐私交易，Public 交易不受隐私状态检查影响。修复后的 geth 支持原 SN 文件中的空 SNS 字段，并在读取损坏的非空 SN 文件时直接报错退出，避免静默使用初始账户状态。

修复版还会在开放 RPC/启动挖矿前，从已确认的规范链重建进程内承诺树（SMT），只恢复公开承诺，不重放余额、不重新广播交易。账户查询返回 `commitmentReady`；恢复后的账户若未通过此检查，将保持隐私交易保护。这里处理的是启动恢复，不解决原有全局 SMT 在运行中发生链重组、候选块回滚时的隔离问题。

恢复中的节点不会接收新的手动或自动 Transfer/隐私交易；恢复失败会保留可展开的错误详情，排除问题后可重试。控制器重启中断恢复任务时会解除“恢复中”状态，允许重新检查。恢复期间不能同时停止实验。

历史实验默认仍由 node-1 单独出块，可在实验详情中调整矿工数量；恢复后应检查链高度继续增长、peer 数恢复。自动负载只跳过暂不可用节点的投递，不会补发跳过的次数。已经广播但未确认的交易应先核对原 hash 的 Receipt；中途失败的 Transfer 还应核对双方状态，不要盲目重发。若控制器本身重启，原自动任务及交易等待协程不会自动续跑，不能仅凭历史 `submitted` 状态判断是否上链。

**旧运行包限制：** 独立的隐私账户 `AccountSK` 只在进程内存中，`SN` 文件保存序列、随机数、承诺与余额，但不保存该秘密。原地恢复后旧代码会使用默认回退逻辑，页面会对已初始化账户显示提示。这不等于账户一定不可用，也不代表已验证隐私交易连续性；不要为此再次 CreateAccount 或删除数据。可靠的隐私账户灾难恢复还需要运行包增加秘密的原子持久化、加载与崩溃一致性处理。

## 交易与执行指令

交易表单会在提交前实时显示实际执行计划。提交后该计划保存到交易的 `command` 字段和实验报告。

```javascript
eth.sendCreateAccountTransaction({from: eth.accounts[0]})
eth.sendMintTransaction({from: eth.accounts[0], value: "0x100"})
eth.sendRedeemTransaction({from: eth.accounts[0], value: "0x1"})
```

Transfer 是两阶段操作：

```javascript
// 付款节点：生成 proof，并暂存下一本地状态
JSON.stringify(eth.getPayerNextState("0x01", "0x1"))

// 接收节点：带入付款方输出并广播最终交易
eth.sendTransferTransaction({
  from: eth.accounts[0], value: "0x1", rs: "0x01",
  cmtANew: payer.cmtANew, snAOld: payer.snAOld, proofA: payer.proofA
})
```

Transfer 前会即时查询双方账户。付款方必须完成 CreateAccount + Mint，接收方必须完成 CreateAccount，双方当前 commitment 必须已进入全局状态 Merkle 树。失败时付款方的 `SequenceNumber`、`SequenceNumberAfter`、随机数和 stage 会整体回滚。

## 账户与交易页面

账户状态已合并到交易工作区。选择实验后可查看：

- 节点账户地址、公开余额（wei）、隐私余额（十六进制和十进制）；
- commitment、隐私状态所在区块；
- 当前链高度、peer 数、最后查询时间；
- 账户状态变更历史；
- 交易指令、哈希、Receipt 与性能阶段。

每个节点支持手动即时查询；交易确认后自动刷新发送方和接收方。

页面通过 SSE 和 5 秒兜底轮询获取状态，但使用增量 DOM 更新：不变的表单、选项和卡片不会重建，高频区块字段只修改文本，刷新请求会去重且不会并发。Receipt 展开状态和正在编辑的表单值应保持不变。

## 自动交易

自动规则当前支持 `round-robin`：

- Mint / Redeem / CreateAccount：依次选择执行节点；
- Transfer / Public：`node[i] -> node[(i+1) % N]`。

每笔自动交易保存 `workloadId` 和 `sequence`。页面可展开查看实际发送节点、接收节点和最终状态。达到规则时长后只停止投递，规则进入 `draining`；所有已投递交易最终结束后才变为 `completed` 或 `completed-with-errors`。

PFAP 证明通常远慢于投递间隔。自动规则是开放式投递器，节点锁会把过量请求排队。正式实验应根据交易类型设置合理速率，并同时观察排队时间。

## 性能字段与统计口径

| 字段 | 含义 |
| --- | --- |
| `submittedAt` | 请求进入 Lab 队列的时间（历史命名，实际是 queued time） |
| `provingAt` | 获得节点锁并开始执行/生成证明 |
| `broadcastAt` | 获得交易哈希、广播完成 |
| `confirmedAt` | 观察到链上 Receipt |

时间分解：

- 排队：`provingAt - submittedAt`
- 证明：C++ `gen ... proof Use Time`，无法提取时使用阶段墙钟时间
- 验证：C++ `verify ... proof Use Time`
- 链上确认：`confirmedAt - broadcastAt`
- 端到端：`confirmedAt - submittedAt`

原始精度使用 `proofDurationUs` / `verifyDurationUs` 保存。页面按量级显示 `µs`、`ms` 或 `s`，不会把亚毫秒值截断为 `0 ms`。控制面通过交易哈希在对应节点 `geth.log` 中定位计时边界，并在启动时为历史单节点 ZK 交易回填微秒数据。

新版 runtime 还记录交易构造和交易验证标记。Transfer 分别保存付款端和收款端的证明生成、证明验证、交易/状态包生成及交易验证；旧 runtime 没有带交易哈希的验证标记时显示为空，不使用 Receipt 等待时间代替。执行 `./build.sh keys` 或 `./build.sh all` 会生成 `dist/build-profile.json`，记录密钥耗时、pk/vk 大小以及构建主机和工具链。生成新密钥会改变证明参数，不能为了补历史数据在运行中的网络上随意执行。

总览 TPS 是整个已保存历史区间的平均值，已标为“历史平均 TPS”；它不等同于某次负载的稳态吞吐。正式结论应按实验、交易类型和 workload 分组，报告样本数、成功率、排队/证明/验证/链上确认的 p50/p95/p99。

### 隐私账户初始化

节点部署时创建的是普通 EOA 地址。参与 Mint、Redeem 或 Transfer 前，每个节点还需要在当前实验链上成功执行一次 CreateAccount。交易页提供“一键初始化”，只为尚未初始化、状态正常且没有活动交易的节点排入一次 CreateAccount；同一服务器上的初始化串行执行，不同服务器可并行。重复 CreateAccount 会重置节点本地隐私状态，因此单笔接口也会拒绝已初始化节点，自动交易规则不提供 CreateAccount 类型。

初始化不会在部署后静默运行，因为它会产生真实交易并进入实验性能统计；Public-only 实验也不需要这一步。对于全新的隐私交易实验，应在开始 Mint/Transfer/Redeem 前显式执行一次批量初始化。同一实验停止再启动时会自动识别已有链上状态并跳过。

## Web 安全

- 密码来自 password file；登录后使用 HttpOnly、SameSite Cookie；
- 不要提交 `lab/data/password`、`lab/data/lab.json` 或 SSH 私钥；
- 局域网监听不等于公网安全，公网必须增加 HTTPS、访问控制和审计；
- 服务器配置保存 SSH 私钥路径，不复制私钥内容。

## API

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/api/state` | 完整 Dashboard 快照 |
| POST | `/api/servers` | 添加本机或 SSH worker |
| POST | `/api/servers/batch` | 原子批量添加最多 100 个 worker |
| POST | `/api/servers/batch/trust-host-keys` | 批量扫描并信任 SSH 主机密钥（先全部扫描，再写入） |
| POST | `/api/servers/batch/delete` | 原子批量删除未被活动/草稿实验使用的配置 |
| PUT | `/api/servers/{id}` | 编辑服务器配置 |
| DELETE | `/api/servers/{id}` | 删除未被活动/草稿实验使用的配置 |
| POST | `/api/servers/{id}/check` | 检查连接和依赖 |
| POST | `/api/experiments/{id}/initialize-accounts` | 为运行实验中尚未初始化且空闲的节点批量排队 CreateAccount |
| POST | `/api/experiments` | 创建实验 manifest |
| POST | `/api/experiments/{id}/deploy` | 异步部署/启动 |
| POST | `/api/experiments/{id}/stop` | 停止托管节点 |
| GET | `/api/experiments/{id}/report` | 导出实验报告 |
| POST | `/api/experiments/{id}/nodes/{nodeId}/state` | 即时查询账户状态 |
| GET/POST | `/api/transactions` | 查询或排队交易 |
| GET/POST | `/api/workloads` | 查询或启动自动规则 |
| GET | `/api/metrics?experimentId=...` | 成功率、TPS、p50/p95 |
| GET | `/api/events/stream` | SSE 事件流 |

## Native 与 Docker

推荐混合策略：正式性能实验使用宿主机 native worker，避免 bridge/NAT、overlay filesystem、cgroup 和容器调度噪声；Docker 适合功能回归、CI、快速清理和高节点数 smoke test。当前已实现 native local/SSH executor，尚未实现 Docker executor。

## 已知限制

- JSON 状态存储只适合单控制进程、单用户；多用户应迁移到 PostgreSQL。
- 没有任务取消、失败交易重试按钮和细粒度 RBAC。
- 自动规则没有自适应背压；高于节点能力时会形成队列。
- 指标筛选和图表仍需加强，尤其是 workload 稳态 TPS、p99 和时间序列。
- `lab/scenarios/*.json` 是设计样例，尚未提供场景导入执行器。
- Docker worker 尚未实现。
- 控制服务重启不会停止 geth；会恢复 running 实验监控，但登录需重做。

开发交接、关键源码和当前验证状态见 [`DEVELOPMENT_HANDOFF.md`](DEVELOPMENT_HANDOFF.md)。
