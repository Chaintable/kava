package app

import (
	"bytes"
	"context"
	"encoding/json"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	dbm "github.com/cometbft/cometbft-db"
	abci "github.com/cometbft/cometbft/abci/types"
	"github.com/cometbft/cometbft/crypto/ed25519"
	"github.com/cometbft/cometbft/libs/log"
	tmstate "github.com/cometbft/cometbft/proto/tendermint/state"
	"github.com/cometbft/cometbft/state"
	"github.com/cometbft/cometbft/store"
	tmtypes "github.com/cometbft/cometbft/types"
	"github.com/cosmos/cosmos-sdk/baseapp"
	"github.com/cosmos/cosmos-sdk/client/tx"
	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	storetypes "github.com/cosmos/cosmos-sdk/store/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/tx/signing"
	authsigning "github.com/cosmos/cosmos-sdk/x/auth/signing"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	vesttypes "github.com/cosmos/cosmos-sdk/x/auth/vesting/types"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	slashtypes "github.com/cosmos/cosmos-sdk/x/slashing/types"
	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
	dtypes "github.com/evmos/ethermint/debank/types"
	rpctypes "github.com/evmos/ethermint/rpc/types"
	evmtypes "github.com/evmos/ethermint/x/evm/types"
	"github.com/stretchr/testify/require"

	utiltypes "github.com/kava-labs/kava/x/evmutil/types"
)

// Decode the public six-field RLP format independently of the writer. Its
// pinned uint256 version implements RLP encoding only.
type replayWireDiff struct {
	Hash, ParentHash common.Hash
	NewAccounts      []struct {
		Address  common.Hash
		Balance  *big.Int
		Nonce    uint64
		CodeHash common.Hash
	}
	DeletedAccounts []common.Hash
	StorageDiff     []struct {
		Address common.Hash
		Values  []struct {
			Index common.Hash
			Value *big.Int
		}
	}
	NewCodes []dtypes.NewCode
}

// The four KV values and timestamps are from rpc.data.kava.io at
// H23294355/H23294356. Only this account's projection is a mainnet fixture.
func TestReplayArchive(t *testing.T) {
	SetSDKConfig()
	var f struct {
		Address                      common.Address
		ParentTime, BlockTime        time.Time
		ParentBalance, BlockBalance  string
		Auth, Ukava, Xrpb, Remainder []byte
	}
	raw, err := os.ReadFile("testdata/replay_vesting.json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &f))
	// Place the synthetic archive across the end of the v0.27 segment.
	const parentHeight = replayLastHeight - 8
	dir := t.TempDir()
	open := func(name string) dbm.DB {
		db, err := dbm.NewGoLevelDB(name, dir)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, db.Close()) })
		return db
	}
	appDB, blockDB, stateDB := open("application"), open("blockstore"), open("state")
	encoding := MakeEncodingConfig()
	canonical := NewApp(log.NewNopLogger(), appDB, t.TempDir(), nil, encoding, DefaultOptions, baseapp.SetChainID(ReplayChainID))
	val := tmtypes.NewValidator(ed25519.GenPrivKey().PubKey(), 1)
	vals := tmtypes.NewValidatorSet([]*tmtypes.Validator{val})
	replay := NewReplayArchive(blockDB, stateDB, appDB, t.TempDir(), encoding)
	var locked authtypes.GenesisAccount
	require.NoError(t, encoding.Marshaler.UnmarshalInterface(f.Auth, &locked))
	key := secp256k1.GenPrivKey()
	owner := sdk.AccAddress(key.PubKey().Address())
	ethKey, err := crypto.GenerateKey()
	require.NoError(t, err)
	sender := crypto.PubkeyToAddress(ethKey.PublicKey)
	// A nonzero nonce at the future CREATE address makes geth reject the
	// creation before it emits a root frame.
	collision := crypto.CreateAddress(sender, 4)
	accs := []authtypes.GenesisAccount{
		authtypes.NewBaseAccount(owner, key.PubKey(), 0, 0),
		locked,
		authtypes.NewBaseAccountWithAddress(sender.Bytes()),
		authtypes.NewBaseAccount(collision.Bytes(), nil, 0, 1),
	}
	ukava, ok := sdk.NewIntFromString(string(f.Ukava))
	require.True(t, ok)
	xrpb, ok := sdk.NewIntFromString(string(f.Xrpb))
	require.True(t, ok)
	gen := genesisStateWithValSet(&TestApp{App: *canonical}, NewDefaultGenesisState(), vals, accs,
		banktypes.Balance{Address: owner.String(), Coins: sdk.NewCoins(sdk.NewInt64Coin("ukava", 10_000_000), sdk.NewInt64Coin("uone", 100), sdk.NewInt64Coin("utwo", 100))},
		banktypes.Balance{Address: sdk.AccAddress(sender.Bytes()).String(), Coins: sdk.NewCoins(sdk.NewInt64Coin("ukava", 1_000_000_000))},
		banktypes.Balance{Address: authtypes.NewModuleAddress(utiltypes.ModuleName).String(), Coins: sdk.NewCoins(sdk.NewInt64Coin("ukava", 1))},
		banktypes.Balance{Address: locked.GetAddress().String(), Coins: sdk.NewCoins(sdk.NewCoin("ukava", ukava), sdk.NewCoin("xrpb", xrpb))})
	slash := slashtypes.DefaultGenesisState()
	consAddr := sdk.ConsAddress(val.Address).String()
	slash.SigningInfos = []slashtypes.SigningInfo{{Address: consAddr, ValidatorSigningInfo: slashtypes.ValidatorSigningInfo{Address: consAddr, StartHeight: parentHeight}}}
	gen[slashtypes.ModuleName] = encoding.Marshaler.MustMarshalJSON(slash)
	var ethGen evmtypes.GenesisState
	encoding.Marshaler.MustUnmarshalJSON(gen[evmtypes.ModuleName], &ethGen)
	ethGen.Params.EvmDenom = "akava"
	gen[evmtypes.ModuleName] = encoding.Marshaler.MustMarshalJSON(&ethGen)
	ug := utiltypes.DefaultGenesisState()
	var rem utiltypes.Account
	require.NoError(t, rem.Unmarshal(f.Remainder))
	ug.Accounts = []utiltypes.Account{rem}
	for _, denom := range []string{"uone", "utwo"} {
		ug.Params.AllowedCosmosDenoms = append(ug.Params.AllowedCosmosDenoms, utiltypes.NewAllowedCosmosCoinERC20Token(denom, denom, denom, 6))
	}
	gen[utiltypes.ModuleName] = encoding.Marshaler.MustMarshalJSON(ug)
	genJSON, err := json.Marshal(gen)
	require.NoError(t, err)
	cons := tmtypes.DefaultConsensusParams()
	cons.Block.MaxGas = 6_012_345
	params := cons.ToProto()
	canonical.InitChain(abci.RequestInitChain{Time: f.ParentTime, ChainId: ReplayChainID, InitialHeight: parentHeight, ConsensusParams: &params, AppStateBytes: genJSON})

	// Build the reference archive by calling ABCI directly with tracing disabled.
	bs := store.NewBlockStore(blockDB)
	sm := state.NewStore(stateDB, state.StoreOptions{})
	var prev tmtypes.BlockID
	var appHash, resultsHash []byte
	height := parentHeight
	commitBlock := func(at time.Time, transactions ...tmtypes.Tx) *tmtypes.Block {
		commit := &tmtypes.Commit{Height: height - 1, BlockID: prev, Signatures: []tmtypes.CommitSig{tmtypes.NewCommitSigAbsent()}}
		block := tmtypes.MakeBlock(height, transactions, commit, nil)
		block.Header.Populate(block.Version, ReplayChainID, at, prev, vals.Hash(), vals.Hash(), cons.Hash(), appHash, resultsHash, val.Address)
		res := &tmstate.ABCIResponses{}
		votes := []abci.VoteInfo{{Validator: tmtypes.TM2PB.Validator(val)}}
		if height == parentHeight {
			votes = nil
		}
		begin := canonical.BeginBlock(abci.RequestBeginBlock{Hash: block.Hash(), Header: *block.Header.ToProto(), LastCommitInfo: abci.CommitInfo{Votes: votes}})
		res.BeginBlock = &begin
		for _, raw := range transactions {
			delivered := canonical.DeliverTx(abci.RequestDeliverTx{Tx: raw})
			res.DeliverTxs = append(res.DeliverTxs, &delivered)
		}
		end := canonical.EndBlock(abci.RequestEndBlock{Height: height})
		res.EndBlock = &end
		appHash = canonical.Commit().Data
		resultsHash = state.ABCIResponsesResultsHash(res)
		require.NoError(t, sm.SaveABCIResponses(height, res))
		parts, err := block.MakePartSet(tmtypes.BlockPartSizeBytes)
		require.NoError(t, err)
		bs.SaveBlock(block, parts, commit)
		// The replay request uses the H-1 validators and consensus params at H.
		require.NoError(t, sm.Bootstrap(state.State{ChainID: ReplayChainID, InitialHeight: parentHeight, LastBlockHeight: height - 1, Validators: vals, NextValidators: vals, LastValidators: vals, LastHeightValidatorsChanged: height, LastHeightConsensusParamsChanged: height, ConsensusParams: *cons}))
		prev = tmtypes.BlockID{Hash: block.Hash(), PartSetHeader: parts.Header()}
		height++
		return block
	}
	parent := commitBlock(f.ParentTime)
	before := canonical.NewUncachedContext(false, *parent.Header.ToProto())
	record, _, err := canonical.replayAccount(before, f.Address)
	require.NoError(t, err)
	require.Equal(t, f.ParentBalance, record.Balance.ToBig().String())
	t.Run("invalid archived EVM denomination", func(t *testing.T) {
		for _, denom := range []string{"", "ukava"} {
			ctx, _ := before.CacheContext()
			params := canonical.evmKeeper.GetParams(ctx)
			params.EvmDenom = denom
			ctx.KVStore(canonical.keys[evmtypes.StoreKey]).Set(evmtypes.KeyPrefixParams, canonical.appCodec.MustMarshal(&params))
			_, _, err := canonical.replayAccount(ctx, f.Address)
			require.ErrorContains(t, err, "EVM balance denomination")
		}
	})
	t.Run("underfunded non-ukava vesting lock", func(t *testing.T) {
		ctx, _ := before.CacheContext()
		addr := locked.GetAddress()
		canonical.accountKeeper.SetAccount(ctx, vesttypes.NewDelayedVestingAccount(
			authtypes.NewBaseAccountWithAddress(addr), sdk.NewCoins(sdk.NewInt64Coin("xrpb", 1)), f.BlockTime.Unix()))
		ctx.KVStore(canonical.keys[banktypes.StoreKey]).Delete(banktypes.CreatePrefixedAccountStoreKey(addr, []byte("xrpb")))
		got, _, err := canonical.replayAccount(ctx, f.Address)
		require.NoError(t, err)
		require.Equal(t, rem.Balance.BigInt(), got.Balance.ToBig())
		require.Equal(t, canonical.evmKeeper.GetBalance(ctx, f.Address), got.Balance.ToBig())
	})

	convert := func(denom string, amount int64) *utiltypes.MsgConvertCosmosCoinToERC20 {
		msg := utiltypes.NewMsgConvertCosmosCoinToERC20(owner.String(), sender.Hex(), sdk.NewInt64Coin(denom, amount))
		return &msg
	}
	signNative := func(sequence uint64, msgs ...sdk.Msg) tmtypes.Tx {
		builder := encoding.TxConfig.NewTxBuilder()
		require.NoError(t, builder.SetMsgs(msgs...))
		builder.SetGasLimit(4_000_000)
		builder.SetFeeAmount(sdk.NewCoins(sdk.NewInt64Coin("ukava", 2)))
		mode := signing.SignMode_SIGN_MODE_DIRECT
		sig := signing.SignatureV2{PubKey: key.PubKey(), Sequence: sequence, Data: &signing.SingleSignatureData{SignMode: mode}}
		require.NoError(t, builder.SetSignatures(sig))
		num := canonical.accountKeeper.GetAccount(before, owner).GetAccountNumber()
		sig, err := tx.SignWithPrivKey(mode, authsigning.SignerData{ChainID: ReplayChainID, AccountNumber: num, Sequence: sequence}, builder, key, encoding.TxConfig, sequence)
		require.NoError(t, err)
		require.NoError(t, builder.SetSignatures(sig))
		raw, err := encoding.TxConfig.TxEncoder()(builder.GetTx())
		require.NoError(t, err)
		return raw
	}
	signEth := func(n uint64, to *common.Address, input []byte) tmtypes.Tx {
		etx := ethtypes.NewTx(&ethtypes.LegacyTx{Nonce: n, GasPrice: big.NewInt(2_000_000_000), Gas: 200_000, To: to, Value: big.NewInt(7), Data: input})
		signed, err := ethtypes.SignTx(etx, ethtypes.LatestSignerForChainID(big.NewInt(2222)), ethKey)
		require.NoError(t, err)
		var msg evmtypes.MsgEthereumTx
		require.NoError(t, msg.FromEthereumTx(signed))
		built, err := msg.BuildTx(encoding.TxConfig.NewTxBuilder(), "akava")
		require.NoError(t, err)
		raw, err := encoding.TxConfig.TxEncoder()(built)
		require.NoError(t, err)
		return raw
	}
	empty := commitBlock(f.BlockTime)
	// Pack both Ethereum messages into a single Cosmos transaction. Their
	// message positions and charged gas must remain independent.
	var ethMsgs []sdk.Msg
	builder := encoding.TxConfig.NewTxBuilder()
	var fees sdk.Coins
	for _, raw := range []tmtypes.Tx{signEth(0, &f.Address, nil), signEth(1, nil, common.FromHex("0x60006000a060006000fd"))} {
		inner, err := encoding.TxConfig.TxDecoder()(raw)
		require.NoError(t, err)
		ethMsgs = append(ethMsgs, inner.GetMsgs()...)
		fees = fees.Add(inner.(sdk.FeeTx).GetFee()...)
		builder, err = encoding.TxConfig.WrapTxBuilder(inner)
		require.NoError(t, err)
	}
	require.NoError(t, builder.SetMsgs(ethMsgs...))
	builder.SetGasLimit(400000)
	builder.SetFeeAmount(fees)
	batch, err := encoding.TxConfig.TxEncoder()(builder.GetTx())
	require.NoError(t, err)
	mixed := commitBlock(f.BlockTime.Add(6*time.Second), signNative(0, convert("uone", 5)), batch)
	native := commitBlock(f.BlockTime.Add(12*time.Second), signNative(1, convert("utwo", 3)))
	failed := commitBlock(f.BlockTime.Add(18*time.Second), signNative(2, convert("uone", 2), convert("uone", 1_000_000)))
	// Deploy a contract that initializes slot zero to one, then clears it and emits LOG0 on call.
	contract := crypto.CreateAddress(sender, 2)
	deploy := commitBlock(f.BlockTime.Add(24*time.Second), signEth(2, nil, common.FromHex("0x6001600055600b6011600039600b6000f3600060005560006000a000")))
	clear := commitBlock(f.BlockTime.Add(30*time.Second), signEth(3, &contract, nil), signEth(4, nil, []byte{0x00}))
	mixedResults, err := sm.LoadABCIResponses(mixed.Height)
	require.NoError(t, err)
	require.Zero(t, mixedResults.DeliverTxs[0].Code, mixedResults.DeliverTxs[0].Log)
	require.Zero(t, mixedResults.DeliverTxs[1].Code, mixedResults.DeliverTxs[1].Log)
	var msgData sdk.TxMsgData
	require.NoError(t, msgData.Unmarshal(mixedResults.DeliverTxs[1].Data))
	require.Len(t, msgData.MsgResponses, 2)
	var transfer evmtypes.MsgEthereumTxResponse
	require.NoError(t, transfer.Unmarshal(msgData.MsgResponses[0].Value))
	gasPerTransfer := int64(transfer.GasUsed)
	require.Positive(t, gasPerTransfer)
	var transfers []tmtypes.Tx
	for n := int64(0); n < cons.Block.MaxGas/gasPerTransfer+3; n++ {
		transfers = append(transfers, signEth(uint64(n+5), &contract, nil))
	}
	full := commitBlock(f.BlockTime.Add(36*time.Second), transfers...)
	// The constructor writes slot zero, emits LOG0, and returns runtime code.
	constructor := common.FromHex("0x600160005560006000a0600a6016600039600a6000f3602a60005260206000f3")
	ctx := canonical.NewUncachedContext(false, *full.Header.ToProto())
	firstNonce := canonical.accountKeeper.GetAccount(ctx, sender.Bytes()).GetSequence()
	creations := make([]tmtypes.Tx, len(transfers))
	for i := range creations {
		creations[i] = signEth(firstNonce+uint64(i), nil, constructor)
	}
	deployments := commitBlock(f.BlockTime.Add(42*time.Second), creations...)
	successor := commitBlock(f.BlockTime.Add(48 * time.Second))
	commitBlock(f.BlockTime.Add(54 * time.Second))
	// Reopen the block-store height cache after the archive is frozen.
	replay.blocks = store.NewBlockStore(blockDB)
	require.Equal(t, replayLastHeight, replay.LastHeight())
	successorHash := common.BytesToHash(successor.Hash())
	_, err = replay.Replay(context.Background(), rpctypes.BlockNumberOrHash{BlockHash: &successorHash})
	require.ErrorContains(t, err, "outside available Kava v0.27 range")
	frozen := canonical.LastCommitID()
	at := func(block *tmtypes.Block) (*dtypes.BlockFile, *replayWireDiff) {
		hash := common.BytesToHash(block.Hash())
		v, err := replay.Replay(context.Background(), rpctypes.BlockNumberOrHash{BlockHash: &hash})
		require.NoError(t, err)
		var d replayWireDiff
		require.NoError(t, rlp.DecodeBytes(v.StateDiff, &d))
		require.Equal(t, common.BytesToHash(block.AppHash), d.Hash)
		return v.BlockFile, &d
	}
	checkReplayArchiveFaults(t, replay, blockDB, stateDB, empty.Height, f.Address)
	file, diff := at(empty)
	require.Empty(t, file.Txs)
	require.Equal(t, common.BytesToHash(parent.AppHash), diff.ParentHash)
	checkAddr := crypto.Keccak256Hash(f.Address.Bytes())
	var found bool
	for _, r := range diff.NewAccounts {
		if r.Address == checkAddr {
			require.Equal(t, f.BlockBalance, r.Balance.String())
			found = true
		}
	}
	require.True(t, found, "unlock across an empty block")
	file, diff = at(mixed)
	require.Len(t, file.Txs, 2)
	require.True(t, file.Txs[0].Status)
	require.False(t, file.Txs[1].Status)
	require.NotEmpty(t, file.Traces)
	require.NotEmpty(t, file.ErrorTraces)
	require.Len(t, file.ErrorEvents, 1)
	require.NotEmpty(t, diff.NewCodes)
	require.NotEmpty(t, diff.StorageDiff)
	file, again := at(native)
	require.Empty(t, file.Txs)
	require.NotEmpty(t, again.StorageDiff)
	require.Len(t, again.NewCodes, 1)
	require.Equal(t, diff.NewCodes[0], again.NewCodes[0])
	file, reverted := at(failed)
	require.Empty(t, file.Txs)
	require.Empty(t, reverted.StorageDiff)
	ownerHash := crypto.Keccak256Hash(owner)
	found = false
	for _, r := range reverted.NewAccounts {
		if r.Address == ownerHash {
			require.Equal(t, uint64(3), r.Nonce)
			found = true
		}
	}
	require.True(t, found, "failed Cosmos transaction kept ante sequence")
	_, created := at(deploy)
	file, cleared := at(clear)
	earlyFailure, err := sm.LoadABCIResponses(clear.Height)
	require.NoError(t, err)
	require.Zero(t, earlyFailure.DeliverTxs[1].Code)
	var rejectedData sdk.TxMsgData
	require.NoError(t, rejectedData.Unmarshal(earlyFailure.DeliverTxs[1].Data))
	require.Len(t, rejectedData.MsgResponses, 1)
	var rejected evmtypes.MsgEthereumTxResponse
	require.NoError(t, rejected.Unmarshal(rejectedData.MsgResponses[0].Value))
	require.Equal(t, vm.ErrContractAddressCollision.Error(), rejected.VmError)
	require.Len(t, file.Txs, 2)
	require.False(t, file.Txs[1].Status)
	require.Equal(t, rejected.GasUsed, file.Txs[1].GasUsed.Uint64())
	require.Empty(t, file.ErrorTraces)
	require.Empty(t, file.ErrorEvents)
	slotHash := crypto.Keccak256Hash(make([]byte, 32))
	addressHash := crypto.Keccak256Hash(contract.Bytes())
	valueAt := func(diff *replayWireDiff) *big.Int {
		for _, group := range diff.StorageDiff {
			if group.Address == addressHash {
				for _, v := range group.Values {
					if v.Index == slotHash {
						return v.Value
					}
				}
			}
		}
		return nil
	}
	require.Equal(t, big.NewInt(1), valueAt(created))
	require.NotNil(t, valueAt(cleared))
	require.Zero(t, valueAt(cleared).Sign())
	file, _ = at(full)
	actual, err := sm.LoadABCIResponses(full.Height)
	require.NoError(t, err)
	var listed int
	var rolledBack, neverRan bool
	for i, res := range actual.DeliverTxs {
		if rpctypes.TxExceedBlockGasLimit(res) {
			rolledBack = true
			require.False(t, file.Txs[i].Status)
			require.Equal(t, big.NewInt(200000), file.Txs[i].GasUsed)
		}
		if !rpctypes.TxSuccessOrExceedsBlockGasLimit(res) {
			neverRan = true
			continue
		}
		listed++
	}
	require.True(t, rolledBack)
	require.True(t, neverRan)
	require.Len(t, file.Txs, listed)
	require.NotEmpty(t, file.ErrorTraces)
	require.Len(t, file.ErrorEvents, 1)
	t.Run("executed block OOG requires tracer", func(t *testing.T) { checkReplayMissingOOGTrace(t, replay, full, val) })
	blockFile, changes := at(deployments)
	t.Run("deployment rolled back by block gas", func(t *testing.T) {
		require.NotEmpty(t, blockFile.Txs)
		failedTx := blockFile.Txs[len(blockFile.Txs)-1]
		require.False(t, failedTx.Status)
		require.Len(t, blockFile.ErrorTraces, 1)
		root := blockFile.ErrorTraces[0]
		require.Equal(t, failedTx.ID, root.TxID)
		require.Equal(t, "create", root.CallCreateType)
		require.Equal(t, "exceeds block gas limit", root.Error)
		require.Equal(t, common.Address{}.Hex(), root.To)
		require.Equal(t, "0x", root.Output.String())
		require.Len(t, blockFile.ErrorEvents, 1)
		require.NotEmpty(t, blockFile.Traces)
		require.Equal(t, "0x602a60005260206000f3", blockFile.Traces[0].Output.String())

		committed := crypto.CreateAddress(sender, firstNonce)
		discarded := crypto.CreateAddress(sender, failedTx.Nonce.Uint64())
		require.Contains(t, blockFile.StorageContracts, strings.ToLower(committed.Hex()))
		require.NotContains(t, blockFile.StorageContracts, strings.ToLower(discarded.Hex()))
		discardedHash := crypto.Keccak256Hash(discarded.Bytes())
		for _, entry := range changes.NewAccounts {
			require.NotEqual(t, discardedHash, entry.Address)
		}
		for _, entry := range changes.StorageDiff {
			require.NotEqual(t, discardedHash, entry.Address)
		}
	})
	_, retry := at(empty)
	require.Equal(t, common.BytesToHash(parent.AppHash), retry.ParentHash)
	require.Equal(t, frozen, canonical.LastCommitID())
	bad := *native.Header.ToProto()
	bad.AppHash = bytes.Repeat([]byte{0xff}, 32)
	next, err := tmtypes.HeaderFromProto(&bad)
	require.NoError(t, err)
	_, err = replay.execute(context.Background(), mixed, &empty.Header, &next, vals, cons.Block.MaxGas)
	require.Error(t, err)
}

func TestReplayRejectsMissingIAVLVersion(t *testing.T) {
	db := dbm.NewMemDB()
	tree := openReplayTree(db, authtypes.StoreKey)
	defer tree.Close()
	addr := common.HexToAddress("0xabcd")
	_, err := tree.Set(authtypes.AddressStoreKey(addr.Bytes()), []byte{1})
	require.NoError(t, err)
	save := func() storetypes.CommitID {
		hash, h, err := tree.SaveVersion()
		require.NoError(t, err)
		return storetypes.CommitID{Version: h, Hash: hash}
	}
	parent := save()
	current := save()
	changes := replayChanges{addresses: map[common.Address]struct{}{}}
	require.NoError(t, readStoreChanges(db, authtypes.StoreKey, 2, parent, current, &changes))
	require.Empty(t, changes.addresses)
	require.Error(t, readStoreChanges(db, authtypes.StoreKey, 3, current, storetypes.CommitID{Version: 3}, &changes))
	require.NoError(t, tree.DeleteVersionsTo(1))
	require.ErrorContains(t, readStoreChanges(db, authtypes.StoreKey, 2, parent, current, &changes), "root 1")
}
