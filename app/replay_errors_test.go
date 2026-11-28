package app

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/big"
	"testing"

	dbm "github.com/cometbft/cometbft-db"
	abci "github.com/cometbft/cometbft/abci/types"
	"github.com/cometbft/cometbft/libs/log"
	tmtypes "github.com/cometbft/cometbft/types"
	"github.com/cosmos/cosmos-sdk/baseapp"
	storetypes "github.com/cosmos/cosmos-sdk/store/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/evmos/ethermint/debank/tracer"
	dtypes "github.com/evmos/ethermint/debank/types"
	rpctypes "github.com/evmos/ethermint/rpc/types"
	evmtypes "github.com/evmos/ethermint/x/evm/types"
	"github.com/stretchr/testify/require"
)

type replayFaultDB struct {
	dbm.DB
	match func(key, value []byte) bool
	value []byte
	err   error
	hits  int
}

func (d *replayFaultDB) Get(key []byte) ([]byte, error) {
	raw, err := d.DB.Get(key)
	if err == nil && d.match != nil && d.match(key, raw) {
		d.hits++
		return d.value, d.err
	}
	return raw, err
}

// Match the stored leaf, not a logical deletion from a newly committed tree.
func replayIAVLLeaf(store string, logicalKey []byte) func([]byte, []byte) bool {
	prefix := []byte("s/k:" + store + "/s")
	return func(key, raw []byte) bool {
		return bytes.HasPrefix(key, prefix) && len(raw) > 0 && raw[0] == 0 && bytes.Contains(raw, logicalKey)
	}
}

func TestReplayVestingScanRejectsTruncation(t *testing.T) {
	SetSDKConfig()
	db := dbm.NewMemDB()
	a := &App{appCodec: MakeEncodingConfig().Marshaler}
	tree := openReplayTree(db, authtypes.StoreKey)
	for i := byte(1); i <= 9; i++ {
		addr := sdk.AccAddress(bytes.Repeat([]byte{i}, common.AddressLength))
		value, err := a.appCodec.MarshalInterface(authtypes.NewBaseAccountWithAddress(addr))
		require.NoError(t, err)
		_, err = tree.Set(authtypes.AddressStoreKey(addr), value)
		require.NoError(t, err)
	}
	hash, version, err := tree.SaveVersion()
	require.NoError(t, err)
	require.NoError(t, tree.Close())
	// A middle leaf is off both search paths used to get the interval ranks.
	key := authtypes.AddressStoreKey(bytes.Repeat([]byte{5}, common.AddressLength))
	fault := &replayFaultDB{DB: db, match: replayIAVLLeaf(authtypes.StoreKey, key), err: io.ErrUnexpectedEOF}
	schedule, err := a.seedVesting(context.Background(), fault, storetypes.CommitID{Version: version, Hash: hash}, 0)
	require.Positive(t, fault.hits, "scan error: %v", err)
	require.ErrorContains(t, err, "incomplete vesting scan")
	require.Nil(t, schedule)
}

func checkReplayArchiveFaults(t *testing.T, replay *ReplayArchive, blocks, state dbm.DB, height int64, addr common.Address) {
	t.Helper()
	n := rpctypes.BlockNumber(height)
	id := rpctypes.BlockNumberOrHash{BlockNumber: &n}
	good, err := replay.Replay(context.Background(), id)
	require.NoError(t, err)
	atKey := func(prefix string, h int64) func([]byte, []byte) bool {
		want := []byte(fmt.Sprintf("%s:%d", prefix, h))
		return func(key, _ []byte) bool { return bytes.Equal(key, want) }
	}
	resultKey := atKey("abciResponsesKey", height)
	results, err := replay.state.LoadABCIResponses(height)
	require.NoError(t, err)
	results.BeginBlock.Events = append(results.BeginBlock.Events, abci.Event{Type: "unexpected"})
	different, err := results.Marshal()
	require.NoError(t, err)
	for _, tc := range []struct {
		name, want string
		appFault   bool
		match      func([]byte, []byte) bool
		value      []byte
		err        error
	}{
		{"missing vesting leaf", "seed vesting", true, replayIAVLLeaf(authtypes.StoreKey, authtypes.AddressStoreKey(addr.Bytes())), nil, nil},
		{"missing ukava leaf", "node", true, replayIAVLLeaf(banktypes.StoreKey, banktypes.CreatePrefixedAccountStoreKey(addr.Bytes(), []byte("ukava"))), nil, nil},
		{"missing ABCI", "archived ABCI responses", false, resultKey, nil, nil},
		{"corrupt ABCI", "corrupt Comet record", false, resultKey, []byte{0xff}, nil},
		{"unreadable ABCI", "unexpected EOF", false, resultKey, nil, io.ErrUnexpectedEOF},
		{"different ABCI events", "block events differ", false, resultKey, different, nil},
		{"corrupt validators", fmt.Sprint(height - 1), false, atKey("validatorsKey", height-1), []byte{0xff}, nil},
		{"corrupt consensus params", "corrupt Comet record", false, atKey("consensusParamsKey", height), []byte{0xff}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stateDB, appDB := state, replay.db
			fault := &replayFaultDB{match: tc.match, value: tc.value, err: tc.err}
			if tc.appFault {
				fault.DB, appDB = appDB, fault
			} else {
				fault.DB, stateDB = stateDB, fault
			}
			reader := NewReplayArchive(blocks, stateDB, appDB, t.TempDir(), replay.encoding)
			out, err := reader.Replay(context.Background(), id)
			require.Positive(t, fault.hits)
			require.ErrorContains(t, err, tc.want)
			require.Nil(t, out)
			fault.match = nil
			out, err = reader.Replay(context.Background(), id)
			require.NoError(t, err)
			require.Equal(t, good.StateDiff, out.StateDiff)
			require.Equal(t, good.ValidationHash, out.ValidationHash)
		})
	}
}

// Execute a real block without attaching a tracer. The message server still
// records the EVM receipt even when the SDK later discards DeliverTx.Data.
func checkReplayMissingOOGTrace(t *testing.T, replay *ReplayArchive, block *tmtypes.Block, val *tmtypes.Validator) {
	t.Helper()
	opts := DefaultOptions
	opts.SkipLoadLatest = true
	messages := new(replayMsgServer)
	opts.replayMessages = messages
	a := NewApp(log.NewNopLogger(), newOverlayDB(replay.db), t.TempDir(), nil, replay.encoding, opts, baseapp.SetChainID(ReplayChainID), baseapp.SetIAVLDisableFastNode(true))
	require.NoError(t, a.LoadHeight(block.Height-1))
	a.BeginBlock(abci.RequestBeginBlock{Hash: block.Hash(), Header: *block.Header.ToProto(), LastCommitInfo: abci.CommitInfo{Votes: []abci.VoteInfo{{Validator: tmtypes.TM2PB.Validator(val)}}}})
	for _, raw := range block.Txs {
		messages.calls = nil
		res := a.DeliverTx(abci.RequestDeliverTx{Tx: raw})
		if !rpctypes.TxExceedBlockGasLimit(&res) {
			continue
		}
		require.Len(t, messages.calls, 1)
		call := messages.calls[0]
		require.NotNil(t, call.response)
		require.Len(t, call.response.Logs, 1)
		require.Nil(t, call.trace)
		cosmosTx, err := replay.encoding.TxConfig.TxDecoder()(raw)
		require.NoError(t, err)
		eth := cosmosTx.GetMsgs()[0].(*evmtypes.MsgEthereumTx)
		from, err := eth.GetSender(big.NewInt(2222))
		require.NoError(t, err)
		file := new(dtypes.BlockFile)
		assemble := func(c *signedCall) error {
			return appendReplayTransaction(file, map[common.Address]struct{}{}, c, eth.AsTransaction(), from, big.NewInt(0), 0, &res, &sdk.TxMsgData{})
		}
		require.ErrorContains(t, assemble(call), "missing tracer")
		call.trace = new(tracer.CallTracer)
		require.ErrorContains(t, assemble(call), "missing root call")
		require.Empty(t, file.Txs)
		// A block gas failure during ante never dispatched the message.
		require.NoError(t, assemble(nil))
		require.Len(t, file.Txs, 1)
		require.False(t, file.Txs[0].Status)
		require.Empty(t, file.ErrorTraces)
		return
	}
	t.Fatal("expected an executed EVM transaction rolled back by the block gas meter")
}
