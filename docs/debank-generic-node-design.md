# Kava DeBank 通用节点改造设计文档

## 1. 概述

本文档描述将 Kava（chain_id=2222）接入 DeBank 通用节点架构的完整技术方案。通用节点将链上数据的生产（写节点）与消费（读节点）分离，通过 S3 + Kafka 作为数据通道，实现读节点水平扩展和统一 RPC 接口。

### 1.1 基线版本

| 组件 | 版本 | 来源 |
|------|------|------|
| kava | v0.28.2 | github.com/kava-labs/kava |
| ethermint | v0.21.0-kava-v27.0 | github.com/kava-labs/ethermint |
| cosmos-sdk | v0.47.15 | github.com/kava-labs/cosmos-sdk (fork) |
| cometbft | v0.37.18 | github.com/kava-labs/cometbft (fork) |
| go-ethereum | v1.10.26 | upstream |
| Go | 1.22 | - |

### 1.2 参考实现

TAC 链的通用节点改造（已上线）：
- Chaintable/cosmos-evm `chain/tac` 分支
- Chaintable/tacchain `debank` 分支

## 2. 架构

### 2.1 组件关系

```
┌─────────────────────────────────────────────────────────┐
│                     写节点 (Writer)                      │
│  ┌────────────┐    ┌────────────────┐                   │
│  │ kava       │───▶│ background-    │──▶ S3 + Kafka     │
│  │ (debank    │    │ tracer (etl)   │   (nodex_pipeline │
│  │  patched)  │    │                │    _2222)         │
│  └────────────┘    └────────────────┘                   │
└─────────────────────────────────────────────────────────┘
                            │
                            ▼
┌─────────────────────────────────────────────────────────┐
│               一致性校验 (Consistency Checker)            │
│  nodex_pipeline_2222 → 校验 → pipeline_2222             │
└─────────────────────────────────────────────────────────┘
                            │
                            ▼
┌─────────────────────────────────────────────────────────┐
│                     读节点 (Reader)                      │
│  ┌────────────────────────┐                             │
│  │ leafage-evm            │                             │
│  │ --evm-type cosmos      │                             │
│  │ --chain-cfg 2222       │                             │
│  └────────────────────────┘                             │
└─────────────────────────────────────────────────────────┘
```

### 2.2 与 TAC 架构的关键差异

Kava **不需要原生读节点 + noderpcx**。原因：Kava 没有 Cosmos 有状态预编译合约（staking/bank/distribution 等），只有 noop 测试合约。leafage-evm 可以覆盖所有 EVM RPC 请求。

| 对比项 | TAC | Kava |
|--------|-----|------|
| 原生读节点 + noderpcx | 需要 | **不需要** |
| 组件数量 | 5（writer + etl + leafage + native + noderpcx + checker） | **4**（writer + etl + leafage + checker） |
| Cosmos 有状态预编译 | staking/bank/distribution/ICS20/governance/slashing | 无 |
| 无状态预编译 | bech32 + p256 | noop（测试用） |

### 2.3 数据流

```
kava (写节点, trace_debankBlock)
  → background-tracer (etl, 消费 trace 数据)
    → S3: chaintable-nodex-pipeline--apne1-az4--x-s3/2222/
    → Kafka: nodex_pipeline_2222
      → leafage-evm (读节点, 消费 stateDiff 构建本地状态)
      → consistency-checker → Kafka: pipeline_2222 → 业务
```

## 3. 代码改动

所有改动集中在 ethermint 层（Chaintable/ethermint `chain/kava` 分支），kava 主仓库仅修改 go.mod 指向 fork 后的 ethermint。

### 3.1 StateDB Hooks

**文件**: `x/evm/statedb/hooks.go` (新增), `x/evm/statedb/statedb.go` (修改)

StateDB 默认不会通知外部 state 变更事件。DeBank tracer 需要通过 hooks 捕获 account/state/code 变更来构建 stateDiff。

```go
// x/evm/statedb/hooks.go
type Hooks struct {
    OnAccountSet    func(addr common.Address, account Account)
    OnAccountDelete func(addr common.Address)
    OnStateSet      func(addr common.Address, key common.Hash, value []byte)
    OnCodeSet       func(codeHash []byte, code []byte)
    OnLog           func(log *ethtypes.Log)
}
```

hooks 在以下时机触发：
- `AddLog()` — 每次 EVM 产生 log 时
- `Commit()` — tx 执行成功写入 keeper 时，按 dirty object 逐一触发

### 3.2 DeBank Tracer 模块

**文件**: `debank/tracer/tracer.go`, `debank/tracer/pipeline.go`, `debank/types/*.go`, `debank/util/*.go`

从 cosmos-evm 移植，实现 `vm.EVMLogger` 接口的 `CallTracer`，在 EVM 执行过程中捕获：
- Call traces（调用链、gas、input/output、error）
- Events（合约 log）
- StateDiff（通过 statedb hooks）

#### go-ethereum v1.10 适配点

cosmos-evm 基于 go-ethereum v1.13，kava 基于 v1.10，存在以下接口差异：

| 差异 | v1.13 (cosmos-evm) | v1.10 (kava) | 适配方式 |
|------|-------|------|---------|
| CaptureEnd 签名 | `CaptureEnd(output, gasUsed, err)` | `CaptureEnd(output, gasUsed, duration, err)` | 加 `_ time.Duration` 参数 |
| tracers.Context | 有 `BlockNumber` 字段 | 无 | 用 `Evm.Context.BlockNumber` 替代 |
| Account.Balance | `*uint256.Int` | `*big.Int` | 用 `new(uint256.Int).SetBytes(balance.Bytes())` 转换 |
| tracers 创建 | `tracers.DefaultDirectory.New(...)` | `tracers.New(...)` | 直接替换 |
| metrics.GaugeInfo | 存在 | 不存在 | 移除 NodeInfo gauge |
| log 包 | `cosmossdk.io/log` | `cometbft/libs/log` | 替换 import |
| uint256.MustFromBig | 存在 | 不存在 | 用 `new(uint256.Int).SetBytes(x.Bytes())` |

### 3.3 Keeper 集成

**文件**: `x/evm/keeper/state_transition.go`, `x/evm/keeper/grpc_query.go`

#### state_transition.go

在 `ApplyMessageWithConfig()` 中，stateDB 创建后检查 tracer 类型，如果是 DeBank CallTracer 则设置 hooks：

```go
stateDB := statedb.New(ctx, k, txConfig)
if dt, ok := tracer.(*dtracer.CallTracer); ok {
    stateDB.SetHooks(&statedb.Hooks{
        OnAccountSet:    dt.OnAccountSet,
        OnAccountDelete: dt.OnAccountDelete,
        OnStateSet:      dt.OnStateSet,
        OnCodeSet:       dt.OnCodeSet,
        OnLog:           dt.OnLog,
    })
}
```

同时修改 tracer 启用条件，从 `vmCfg.Debug` 扩展为 `vmCfg.Debug || tracer != nil`，确保 DeBank tracer 在非 debug 模式下也能收到 `CaptureTxStart`/`CaptureTxEnd` 回调。

#### grpc_query.go

在 `traceTx()` 中识别 `debankTracer`：

```go
if traceConfig.Tracer == dtracer.Name {
    tracer = dtracer.NewCallTracer(tCtx)
} else if traceConfig.Tracer != "" {
    // 标准 tracer 创建
}
```

在 `EthCall()` 中添加 batch 模式支持（见 3.5）。

### 3.4 RPC Namespaces

#### trace namespace（写节点用）

**文件**: `rpc/namespaces/ethereum/trace/api.go`, `rpc/namespaces/ethereum/trace/genesis.go`

核心方法 `trace_debankBlock(blockNrOrHash)` / `trace_debankBlockRaw(blockNrOrHash)`：

1. 获取 block 信息（TendermintBlockByNumber + RPCBlockFromTendermintBlock）
2. 如果 blockHeight == 1，走 genesis 特殊路径（`onGenesisBlock`）
3. 调用 `backend.TraceBlock()` 使用 `debankTracer` 对 block 内每笔 tx 执行 trace
4. 收集 traces/events/stateDiff
5. 调用 `addGasUsedStateDiff()` 补全 gas 相关的 balance 变更
6. 组装 `DebankOutPut` 返回

`addGasUsedStateDiff` 通过额外的 RPC 调用（`eth_getBalance`/`eth_getNonce`/`eth_getCode`）获取各地址的最终状态来修正 stateDiff，因为 gas fee 的扣除和 refund 不经过 EVM stateDB。

#### debank namespace（读节点/原生读节点用）

**文件**: `rpc/namespaces/ethereum/debank/api.go`, `api_multicall.go`, `api_simulate.go`, `api_estimategas.go`

| 方法 | 用途 |
|------|------|
| `debank_simulateTransactions` | 批量交易模拟，返回每笔 tx 的 traces + events |
| `debank_multicall` | 并行 EVM call，50 个 call 上限 |
| `debank_estimateGas` | gas 估算，支持 block context |

multicall 中 native token 信息硬编码为 KAVA：
```go
case "name", "symbol":
    res, _ := method.Outputs.Pack("KAVA")
case "decimals":
    res, _ := method.Outputs.Pack(uint8(18))
```

#### namespace 注册

**文件**: `rpc/apis.go`

在 `init()` 中注册 `trace` 和 `debank` 两个 namespace，通过 `--json-rpc.api` 启动参数控制启用：
- 写节点: `--json-rpc.api=eth,txpool,personal,net,debug,web3,trace`
- 原生读节点: `--json-rpc.api=eth,txpool,personal,net,debug,web3,debank`

### 3.5 Batch SimulateTransactions

**文件**: `x/evm/types/tx_args.go`, `x/evm/keeper/grpc_query.go`, `rpc/namespaces/ethereum/debank/api_simulate.go`

#### TransactionArgs 扩展

```go
type TransactionArgs struct {
    // ... 标准字段 ...

    // DeBank batch simulation support
    BlockHash *common.Hash      `json:"blockHash,omitempty"`
    Args      []TransactionArgs `json:"args,omitempty"`
}
```

#### EthCall batch 处理

当 `len(args.Args) > 0` 时进入 batch 模式：

```
客户端 → SimulateTransactions([]CallArgs)
  → 构建嵌套 TransactionArgs{Args: [...], BlockHash: &hash}
  → JSON marshal → EthCall gRPC
  → Keeper 检测 args.Args > 0
  → 循环执行每笔 tx:
    - 自动获取 nonce
    - 创建 CallTracer
    - ApplyMessageWithConfig(commit=true)  ← 状态累积
    - 收集 traces/events/gasUsed
  → 返回 []DebankSingleSimulateResult
```

关键设计：`commit=true` 使得 batch 内前序 tx 的状态变更对后续 tx 可见，模拟真实的交易执行顺序。

### 3.6 辅助类型

**文件**: `x/evm/types/simulate_result.go`, `rpc/types/debank.go`, `rpc/types/trace.go`, `rpc/types/types.go`

| 类型 | 用途 |
|------|------|
| `DebankSingleSimulateResult` | 单笔模拟结果（code/err/gasUsed/traces/events） |
| `DebankTrace` / `DebankEvent` | 模拟返回的 trace 和 event 结构 |
| `DebankBlockContext` | block 选择（by number 或 by hash） |
| `DebankSimulateResp` / `DebankSimulateStats` | 批量模拟响应 |
| `DebankOutPutJs` | trace_debankBlock 返回结构 |
| `BlockOverrides` | block 参数覆盖 |

错误码定义：
- `-39000` SimulateErrorReverted — 执行回滚
- `-39001` SimulateErrorInsufficientBalance — 余额不足
- `-39004` SimulateErrorUnknown — 未知错误

## 4. 部署

### 4.1 链信息

| 参数 | 值 |
|------|---|
| Chain ID (Cosmos) | `kava_2222-10` |
| Chain ID (EVM) | `2222` |
| Binary | `kava` |
| Home 目录 | `/var/data`（容器内） |
| 原生代币 | KAVA |
| EVM denom | `akava`（1 KAVA = 10^18 akava） |
| Cosmos denom | `ukava`（1 KAVA = 10^6 ukava） |
| min-gas-prices | `0.025ukava;1000000000akava` |
| EVM RPC | 8545 (HTTP) / 8546 (WS) |
| P2P | 26656 |
| DB Backend | RocksDB v8.1.1（archival 推荐） |
| Archival 内存 | 128-256 GB |
| Genesis | `https://kava-genesis-files.s3.us-east-1.amazonaws.com/kava_2222-10/genesis.json` |

### 4.2 Docker 镜像

- ECR: `294354037686.dkr.ecr.ap-northeast-1.amazonaws.com/blockchain/kava-x`
- Dockerfile: `Dockerfile.debank`
- CI: `.github/workflows/build.debank.yml`
- 触发: PR to `debank` / release / manual dispatch
- 架构: amd64 + arm64 multi-arch

### 4.3 写节点

```yaml
services:
  kava:
    image: 294354037686.dkr.ecr.ap-northeast-1.amazonaws.com/blockchain/kava-x:{tag}
    ports:
      - 8545:8545
      - 8546:8546
    volumes:
      - /data/kava:/var/data
    entrypoint:
      - /usr/bin/kava
      - start
      - --home=/var/data
      - --log_level=info
      - --json-rpc.api=eth,txpool,personal,net,debug,web3,trace
      - --json-rpc.enable=true
      - --json-rpc.address=0.0.0.0:8545
      - --json-rpc.ws-address=0.0.0.0:8546
      - --metrics
      - --json-rpc.metrics-address=0.0.0.0:6065

  etl:
    image: 294354037686.dkr.ecr.ap-northeast-1.amazonaws.com/background-tracer:amd64-v0.1.21
    entrypoint:
      - /app/etl
      - fetch
      - --region=ap-northeast-1
      - --nodex-bucket=chaintable-nodex-pipeline--apne1-az4--x-s3
      - --chain-table-bucket=chaintable-pipeline--apne1-az4--x-s3
      - --brokers={kafka_broker}
      - --topic=nodex_pipeline_2222
      - --rpc-addr=http://127.0.0.1:8545
      - --max-task-queue-size=256
```

### 4.4 读节点

```yaml
services:
  leafage-evm-x-kava:
    image: 294354037686.dkr.ecr.ap-northeast-1.amazonaws.com/leafage-evm-x:{version}
    command:
      - standalone
      - --db-path=/nodex
      - --listen-addr=0.0.0.0:8565
      - --chain-cfg=2222
      - --archive
      - --db-cache=8192
      - --meta={本机IP}:8565
      - --kafka-s3-config={"topic":"nodex_pipeline_2222",...}
      - --etcd-config={"endpoints":[...],...}
      - --genesis-number=1
```

leafage-evm 已内置 Cosmos 支持（`--chain-cfg=2222` 即可），无需代码改动。内置能力：
- Bech32 预编译 (0x400): hex ↔ bech32 地址转换
- P256 预编译 (0x100): secp256r1 签名验证
- 有状态预编译阻断 (0x800-0x806): 返回 UnsupportedPrecompile

### 4.5 一致性校验

```yaml
services:
  consistency-checkerx:
    image: 294354037686.dkr.ecr.ap-northeast-1.amazonaws.com/consistency-checkerx:amd64-v1.0.9
    command: ["-config", "/config.yml"]
```

config.yml 关键参数：
```yaml
chain_id: 2222
inner_new_block_topic: "nodex_pipeline_2222"
outer_new_block_topic: "pipeline_2222"
ready_ratio: 0.8
check_num: 3
```

### 4.6 节点同步策略

Kava 经历了多次硬分叉升级（v0.26 → v0.27 → v0.28），从 genesis 同步需要在不同高度切换 binary。**推荐使用 Nodies archival 快照**：

```bash
aria2c -s 10 -x 10 https://download.nodies.org/download/latest/snapshot/{region}/kava/mainnet/archive
```

快照使用 RocksDB 格式，需要编译时启用：`make install COSMOS_BUILD_OPTIONS=rocksdb`

## 5. 待完善事项

### 5.1 genesis.go

`rpc/namespaces/ethereum/trace/genesis.go` 中的 `evmGenesisStateStr` 当前为 TAC 的 genesis 数据（multicall3、CREATE2 deployer 合约等）。需要从 Kava 的 genesis.json 中提取 `app_state.evm` 部分替换。

如果使用快照从非 genesis 高度启动写节点，block 1 不会被 trace，此项可延后处理。

### 5.2 RocksDB Docker 镜像

当前 `Dockerfile.debank` 使用默认的 goleveldb。archival 节点建议使用 RocksDB，需要额外的 `Dockerfile.debank-rocksdb` 参考 kava 仓库的 `Dockerfile-rocksdb`。

## 6. 仓库信息

| 仓库 | 分支 | 说明 |
|------|------|------|
| Chaintable/ethermint | `chain/kava` | ethermint DeBank 改造，基于 v0.21.0-kava-v27.0 |
| Chaintable/kava | `debank` | 主分支，go.mod 指向 Chaintable/ethermint chain/kava |
| Chaintable/kava | `chain/kava` | 开发分支，PR 到 debank 触发 CI |
