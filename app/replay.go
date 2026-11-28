package app

import (
	"bytes"
	"context"
	"fmt"

	dbm "github.com/cometbft/cometbft-db"
	abci "github.com/cometbft/cometbft/abci/types"
	"github.com/cometbft/cometbft/libs/log"
	tmstate "github.com/cometbft/cometbft/proto/tendermint/state"
	"github.com/cometbft/cometbft/state"
	"github.com/cometbft/cometbft/store"
	tmtypes "github.com/cometbft/cometbft/types"
	"github.com/cosmos/cosmos-sdk/baseapp"
	pruning "github.com/cosmos/cosmos-sdk/store/pruning/types"
	"github.com/cosmos/cosmos-sdk/store/rootmulti"
	sdk "github.com/cosmos/cosmos-sdk/types"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rlp"
	rpctypes "github.com/evmos/ethermint/rpc/types"

	"github.com/kava-labs/kava/app/params"
)

const (
	ReplayChainID           = "kava_2222-10"
	ReplayFirstHeight int64 = 14136737 // v0.28.0 mainnet upgrade
)

// ReplayArchive serializes calls so consecutive heights can reuse their vesting
// schedule. Each block gets a fresh app and an isolated, bounded write overlay.
type ReplayArchive struct {
	blocks        *store.BlockStore
	state         state.Store
	db            dbm.DB
	encoding      params.EncodingConfig
	scratch       string
	slot          chan struct{}
	vesting       vestingSchedule
	vestingParent common.Hash
}

func NewReplayArchive(blockDB, stateDB, appDB dbm.DB, scratch string, encoding params.EncodingConfig) *ReplayArchive {
	return &ReplayArchive{
		blocks:   store.NewBlockStore(blockDB),
		state:    state.NewStore(replayCometDB{stateDB}, state.StoreOptions{}),
		db:       appDB,
		encoding: encoding,
		scratch:  scratch,
		slot:     make(chan struct{}, 1),
	}
}

// LastHeight needs a successor header to authenticate the application and tx results.
func (r *ReplayArchive) LastHeight() int64 {
	return min(r.blocks.Height()-1, rootmulti.GetLatestVersion(r.db))
}

func (r *ReplayArchive) Replay(ctx context.Context, id rpctypes.BlockNumberOrHash) (out *rpctypes.DebankOutPutJs, err error) {
	select {
	case r.slot <- struct{}{}:
		defer func() { <-r.slot }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	// IAVL and several SDK database readers report corrupt/pruned data by panicking.
	defer func() {
		if p := recover(); p != nil {
			out, err = nil, fmt.Errorf("Kava replay: %v", p)
		}
	}()
	var height int64
	switch {
	case id.BlockNumber != nil && id.BlockHash == nil:
		height = int64(*id.BlockNumber)
		if *id.BlockNumber == rpctypes.EthLatestBlockNumber {
			height = r.LastHeight()
		}
	case id.BlockHash != nil && id.BlockNumber == nil:
		meta := r.blocks.LoadBlockMetaByHash(id.BlockHash.Bytes())
		if meta == nil || !bytes.Equal(meta.Header.Hash(), id.BlockHash.Bytes()) {
			return nil, fmt.Errorf("unknown block %s", id.BlockHash)
		}
		height = meta.Header.Height
	default:
		return nil, fmt.Errorf("expected a block height or hash")
	}
	if height < ReplayFirstHeight || height > r.LastHeight() {
		return nil, fmt.Errorf("height %d outside available Kava v0.28 range [%d,%d]", height, ReplayFirstHeight, r.LastHeight())
	}
	block := r.blocks.LoadBlock(height)
	parent, next := r.blocks.LoadBlockMeta(height-1), r.blocks.LoadBlockMeta(height+1)
	if block == nil || parent == nil || next == nil {
		return nil, fmt.Errorf("missing block or adjacent headers at %d", height)
	}
	if id.BlockHash != nil && common.BytesToHash(block.Hash()) != *id.BlockHash {
		return nil, fmt.Errorf("block hash is not canonical at %d", height)
	}
	vals, err := r.state.LoadValidators(height - 1)
	if err != nil {
		return nil, err
	}
	cons, err := r.state.LoadConsensusParams(height)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(cons.Hash(), block.ConsensusHash) {
		return nil, fmt.Errorf("consensus parameters mismatch at %d", height)
	}
	return r.execute(ctx, block, &parent.Header, &next.Header, vals, cons.Block.MaxGas)
}

func (r *ReplayArchive) execute(ctx context.Context, block *tmtypes.Block, parent, next *tmtypes.Header, vals *tmtypes.ValidatorSet, gasLimit int64) (*rpctypes.DebankOutPutJs, error) {
	h := block.Height
	if block.ChainID != ReplayChainID || parent.ChainID != block.ChainID || next.ChainID != block.ChainID ||
		parent.Height != h-1 || next.Height != h+1 || !bytes.Equal(block.LastBlockID.Hash, parent.Hash()) ||
		!bytes.Equal(next.LastBlockID.Hash, block.Hash()) || !block.Time.After(parent.Time) {
		return nil, fmt.Errorf("broken Kava header chain at %d", h)
	}
	if err := block.ValidateBasic(); err != nil {
		return nil, fmt.Errorf("invalid block %d: %w", h, err)
	}
	if block.LastCommit.Height != h-1 || !block.LastCommit.BlockID.Equals(block.LastBlockID) {
		return nil, fmt.Errorf("last commit does not identify parent at %d", h)
	}
	if !bytes.Equal(vals.Hash(), parent.ValidatorsHash) || len(vals.Validators) != block.LastCommit.Size() {
		return nil, fmt.Errorf("last commit validator set mismatch at %d", h)
	}
	votes := make([]abci.VoteInfo, len(vals.Validators))
	for i, v := range vals.Validators {
		sig := block.LastCommit.Signatures[i]
		if !sig.Absent() && !bytes.Equal(sig.ValidatorAddress, v.Address) {
			return nil, fmt.Errorf("last commit vote %d at %d", i, h)
		}
		votes[i] = abci.VoteInfo{Validator: tmtypes.TM2PB.Validator(v), SignedLastBlock: !sig.Absent()}
	}

	opts := DefaultOptions
	opts.SkipLoadLatest = true
	messages := new(replayMsgServer)
	opts.replayMessages = messages
	a := NewApp(log.NewNopLogger(), newOverlayDB(r.db), r.scratch, nil, r.encoding, opts,
		baseapp.SetChainID(block.ChainID), baseapp.SetIAVLDisableFastNode(true), baseapp.SetIAVLCacheSize(128),
		baseapp.SetPruning(pruning.NewPruningOptions(pruning.PruningNothing)))
	if messages.MsgServer == nil {
		return nil, fmt.Errorf("EVM message service was not registered")
	}
	if err := a.LoadHeight(h - 1); err != nil {
		return nil, fmt.Errorf("open parent %d: %w", h-1, err)
	}
	if id := a.LastCommitID(); id.Version != h-1 || !bytes.Equal(id.Hash, block.AppHash) {
		return nil, fmt.Errorf("parent AppHash mismatch at %d", h)
	}
	beforeStore, err := a.CommitMultiStore().CacheMultiStoreWithVersion(h - 1)
	if err != nil {
		return nil, err
	}
	before := sdk.NewContext(beforeStore, *parent.ToProto(), false, a.Logger())
	schedule := r.vesting
	if schedule == nil || r.vestingParent != common.BytesToHash(parent.Hash()) {
		id := a.CommitMultiStore().GetCommitKVStore(a.keys[authtypes.StoreKey]).LastCommitID()
		schedule, err = a.seedVesting(ctx, r.db, id, parent.Time.Unix())
		if err != nil {
			return nil, fmt.Errorf("seed vesting at %d: %w", h-1, err)
		}
	}

	begin := a.BeginBlock(abci.RequestBeginBlock{
		Hash:                block.Hash(),
		Header:              *block.Header.ToProto(),
		LastCommitInfo:      abci.CommitInfo{Round: block.LastCommit.Round, Votes: votes},
		ByzantineValidators: block.Evidence.Evidence.ToABCI(),
	})
	a.evmKeeper.SetTransactionTracer(messages.trace)
	observed := make([][]*signedCall, len(block.Txs))
	results := &tmstate.ABCIResponses{BeginBlock: &begin, DeliverTxs: make([]*abci.ResponseDeliverTx, len(block.Txs))}
	for i, raw := range block.Txs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		messages.calls = nil
		res := a.DeliverTx(abci.RequestDeliverTx{Tx: raw})
		if messages.err != nil {
			return nil, fmt.Errorf("trace at %d/tx[%d]: %w", h, i, messages.err)
		}
		observed[i] = messages.calls
		results.DeliverTxs[i] = &res
	}
	a.evmKeeper.SetTransactionTracer(nil)
	end := a.EndBlock(abci.RequestEndBlock{Height: h})
	results.EndBlock = &end
	if err := r.verifyResults(h, results, next.LastResultsHash); err != nil {
		return nil, err
	}
	if commit := a.Commit(); !bytes.Equal(commit.Data, next.AppHash) {
		return nil, fmt.Errorf("replayed AppHash %X at %d, want %X", commit.Data, h, next.AppHash)
	}
	after := a.NewUncachedContext(false, *block.Header.ToProto())
	file, header, err := a.replayBlockFile(block, results, observed, r.encoding.TxConfig.TxDecoder(), gasLimit, after)
	if err != nil {
		return nil, fmt.Errorf("blockfile %d: %w", h, err)
	}
	// Only publish the updated schedule after the complete output is ready.
	r.vesting = nil
	diff, err := a.replayStateDiff(ctx, r.db, before, after, next.AppHash, schedule, file)
	if err != nil {
		return nil, fmt.Errorf("state diff %d: %w", h, err)
	}
	wire, err := rlp.EncodeToBytes(diff)
	if err != nil {
		return nil, err
	}
	out := &rpctypes.DebankOutPutJs{
		BlockFile:      file,
		Header:         header,
		StateDiff:      wire,
		ValidationHash: file.Validation().ValidationHash,
	}
	r.vesting, r.vestingParent = schedule, common.BytesToHash(block.Hash())
	return out, nil
}
