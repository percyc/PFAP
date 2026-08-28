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
- `bash`、`tar`、`sha256sum`、`setsid`、`ss`；
- 实验前完成时钟同步；
- 防火墙允许实验使用的 P2P 端口。

runtime 缓存在 `<workDir>/artifacts/<sha256>`；实验位于 `<workDir>/experiments/<experiment-id>`。同一主机的多个节点使用不同 datadir、P2P/RPC 端口，共享只读 runtime、证明密钥和 Ethash DAG。

P2P/RPC 起始端口填 `0` 时自动分配。部署前通过 `ss` 检查冲突；失败实验再次部署时也会重新选择端口。

## 推荐实验流程

1. 构建 `dist/pfap-runtime.tar.gz`。
2. 添加并检查所有服务器。
3. 新建实验，选择每台服务器的节点数。
4. 部署并确认节点为 `running`，peer 数符合预期。
5. 对每个参与隐私交易的节点执行一次 `CreateAccount`。
6. 对付款节点执行 `Mint`。
7. 执行 `Transfer` / `Redeem`，或启动自动规则。
8. 核对账户状态、Receipt、区块号和阶段耗时。
9. 导出实验 JSON 报告。

不要在同一个隐私账户上并发执行 ZK 状态交易。控制面为每个节点维护互斥锁；Transfer 会同时锁住付款方与接收方。Receipt 确认后还会等待新区块，避免下一笔交易在节点隐私序列状态尚未稳定时启动。

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

总览 TPS 是整个已保存历史区间的平均值，已标为“历史平均 TPS”；它不等同于某次负载的稳态吞吐。正式结论应按实验、交易类型和 workload 分组，报告样本数、成功率、排队/证明/验证/链上确认的 p50/p95/p99。

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
| POST | `/api/servers/{id}/check` | 检查连接和依赖 |
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
