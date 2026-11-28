# Chaintable write node

> Fork of [Kava-Labs/kava](https://github.com/Kava-Labs/kava), with Chaintable pipeline patches.

## Architecture

This repo runs the chain's execution layer with the [Chaintable pipeline](https://github.com/Chaintable/pipeline) tracer embedded. The tracer extracts block data — block headers, transactions, call traces, receipts, events, and state diffs — and ships it to **S3 + Kafka** (see pipeline's [architecture](https://github.com/Chaintable/pipeline/blob/main/docs/architecture.md)). Two consumption paths:

- **Block headers + state diffs** → Kafka + S3 → [leafage-evm](https://github.com/Chaintable/leafage-evm): a lightweight EVM executor serving state queries (`eth_call`, `eth_estimateGas`, …), no P2P sync, no tx storage (see its [architecture](https://github.com/Chaintable/leafage-evm#architecture)).
- **Block files** (transactions · call traces · receipts · events) → S3 → Chaintable's transaction/trace indexing pipeline.

```
Chaintable write node (this repo · producer, embeds pipeline tracer)
        │
        ├─ block headers + state diffs ──────────────────→ Kafka + S3 ─→ leafage-evm (EVM state queries)
        │
        └─ block files (tx · trace · receipts · events) ──→ S3 ─→ Chaintable indexing pipeline (tx/trace data)
```

## Replay a Kava v0.28 archive

The replay commands open a stopped archive under `--home/data` containing `application.db`, `blockstore.db` and `state.db`. Build with `COSMOS_BUILD_OPTIONS=rocksdb make install` for RocksDB archives. The default build supports GoLevelDB.

```sh
kava replay-block 23294356 --home /path/to/kava
kava replay-block 23294355 23294360 --home /path/to/kava > blocks.ndjson
kava replay-serve --home /path/to/kava --listen 127.0.0.1:8545
```

The range is inclusive. The HTTP server exposes `trace_debankBlock` by height or hash, `eth_chainId`, and `eth_blockNumber`. Each result contains the header, blockfile, RLP state diff and validation hash. The v0.28 mainnet segment starts at 14,136,737. Replaying H requires its parent state, the committed IAVL trees at H, historical validators and consensus parameters, the original CometBFT ABCI responses at H (including events), and the H+1 header for verification. A missing or unreadable response fails that height.

Each block executes in a private overlay. Sequential requests reuse the vesting schedule. A new starting height requires one scan of the parent auth store.

---

<p align="center">
  <img src="./kava-logo.svg" width="300">
</p>

<div align="center">

[![version](https://img.shields.io/github/tag/kava-labs/kava.svg)](https://github.com/kava-labs/kava/releases/latest)
[![CircleCI](https://circleci.com/gh/Kava-Labs/kava/tree/master.svg?style=shield)](https://circleci.com/gh/Kava-Labs/kava/tree/master)
[![Go Report Card](https://goreportcard.com/badge/github.com/kava-labs/kava)](https://goreportcard.com/report/github.com/kava-labs/kava)
[![API Reference](https://godoc.org/github.com/Kava-Labs/kava?status.svg)](https://godoc.org/github.com/Kava-Labs/kava)
[![GitHub](https://img.shields.io/github/license/kava-labs/kava.svg)](https://github.com/Kava-Labs/kava/blob/master/LICENSE.md)
[![Twitter Follow](https://img.shields.io/twitter/follow/KAVA_CHAIN.svg?label=Follow&style=social)](https://twitter.com/KAVA_CHAIN)
[![Discord Chat](https://img.shields.io/discord/704389840614981673.svg)](https://discord.com/invite/kQzh3Uv)

</div>

<div align="center">

### [Telegram](https://t.me/kavalabs) | [Medium](https://medium.com/@kava_labs) | [Discord](https://discord.gg/JJYnuCx)

</div>

Reference implementation of Kava, a blockchain for cross-chain DeFi. Built using the [cosmos-sdk](https://github.com/cosmos/cosmos-sdk).

## Mainnet

The current recommended version of the software for mainnet is [v0.26.2](https://github.com/Kava-Labs/kava/releases/tag/v0.26.2) The `master` branch of this repository often contains considerable development work since the last mainnet release and is __not__ runnable on mainnet.

### Installation and Setup
For detailed instructions see [the Kava docs](https://docs.kava.io/docs/nodes-and-validators/validator-node).

```bash
git checkout v0.26.2
make install
```

End-to-end tests of Kava use a tool for generating networks with different configurations: [kvtool](https://github.com/Kava-Labs/kvtool).
This is included as a git submodule at [`tests/e2e/kvtool`](tests/e2e/kvtool/).
When first cloning the repository, if you intend to run the e2e integration tests, you must also
clone the submodules:
```bash
git clone --recurse-submodules https://github.com/Kava-Labs/kava.git
```

Or, if you have already cloned the repo: `git submodule update --init`

## Testnet

For further information on joining the testnet, head over to the [testnet repo](https://github.com/Kava-Labs/kava-testnets).

## Docs

Kava protocol and client documentation can be found in the [Kava docs](https://docs.kava.io).

If you have technical questions or concerns, ask a developer or community member in the [Kava discord](https://discord.com/invite/kQzh3Uv).

## Security

If you find a security issue, please report it to security [at] kavalabs.io. Depending on the verification and severity, a bug bounty may be available.

## License

Copyright © Kava Labs, Inc. All rights reserved.

Licensed under the [Apache v2 License](LICENSE.md).
