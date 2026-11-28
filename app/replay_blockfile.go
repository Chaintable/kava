package app

import (
	"bytes"
	"fmt"
	"math/big"
	"strings"

	abci "github.com/cometbft/cometbft/abci/types"
	tmstate "github.com/cometbft/cometbft/proto/tendermint/state"
	tmtypes "github.com/cometbft/cometbft/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/gogoproto/proto"
	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/evmos/ethermint/debank/tracer"
	dtypes "github.com/evmos/ethermint/debank/types"
	"github.com/evmos/ethermint/rpc/backend"
	"github.com/evmos/ethermint/rpc/namespaces/ethereum/trace"
	rpctypes "github.com/evmos/ethermint/rpc/types"
	evmtypes "github.com/evmos/ethermint/x/evm/types"
)

type signedCall struct {
	hash  common.Hash
	trace *tracer.CallTracer
}

func (a *App) replayBlockFile(block *tmtypes.Block, results *tmstate.ABCIResponses, observed [][]signedCall, decode sdk.TxDecoder, gasLimit int64, after sdk.Context) (*dtypes.BlockFile, *dtypes.Header, error) {
	file := &dtypes.BlockFile{
		Txs:         []dtypes.Transaction{},
		Traces:      []dtypes.Trace{},
		ErrorTraces: []dtypes.Trace{},
		Events:      []dtypes.Event{},
		ErrorEvents: []dtypes.Event{},
	}
	addrs := trace.ReplayAffectedAddresses(results.BeginBlock.Events, results.EndBlock.Events, results.DeliverTxs)
	chainID := a.evmKeeper.ChainID()
	fee := a.evmKeeper.GetBaseFee(after, a.evmKeeper.GetParams(after).ChainConfig.EthereumConfig(chainID))
	var hashes []interface{}
	var gas uint64
	for _, res := range results.DeliverTxs {
		if backend.ShouldIgnoreGasUsed(res) {
			break
		}
		gas += uint64(res.GasUsed)
	}
	for i, raw := range block.Txs {
		res := results.DeliverTxs[i]
		tx, err := decode(raw)
		if err != nil {
			// The SDK may have rejected an undecodable transaction.
			if res.Code != 0 {
				continue
			}
			return nil, nil, err
		}
		var data sdk.TxMsgData
		if res.Code == 0 {
			if err := data.Unmarshal(res.Data); err != nil {
				return nil, nil, err
			}
		}
		calls := observed[i]
		for j, msg := range tx.GetMsgs() {
			eth, ok := msg.(*evmtypes.MsgEthereumTx)
			if !ok {
				if res.Code != 0 && msg.ValidateBasic() != nil {
					continue
				}
				for _, addr := range msg.GetSigners() {
					if len(addr) == common.AddressLength {
						addrs[common.BytesToAddress(addr)] = struct{}{}
					}
				}
				continue
			}
			reported := rpctypes.TxSuccessOrExceedsBlockGasLimit(res)
			from, err := eth.GetSender(chainID)
			if err != nil {
				if !reported {
					continue
				}
				return nil, nil, err
			}
			addrs[from] = struct{}{}
			if !reported {
				continue
			}
			etx := eth.AsTransaction()
			hash := etx.Hash()
			if etx.To() != nil {
				addrs[*etx.To()] = struct{}{}
			}
			var call *tracer.CallTracer
			if len(calls) != 0 {
				if calls[0].hash != hash {
					return nil, nil, fmt.Errorf("tx[%d]/msg[%d]: observed %s, want %s", i, j, calls[0].hash, hash)
				}
				call, calls = calls[0].trace, calls[1:]
			}
			if err := appendReplayTransaction(file, addrs, call, etx, from, fee, j, res, &data); err != nil {
				return nil, nil, fmt.Errorf("tx[%d]/msg[%d] %s: %w", i, j, hash, err)
			}
			hashes = append(hashes, hash)
		}
		if rpctypes.TxSuccessOrExceedsBlockGasLimit(res) && len(calls) != 0 {
			return nil, nil, fmt.Errorf("tx[%d]: %d unmatched signed calls", i, len(calls))
		}
	}
	for addr := range addrs {
		file.StorageContracts = append(file.StorageContracts, strings.ToLower(addr.Hex()))
	}
	var bloom ethtypes.Bloom
	for _, event := range results.EndBlock.Events {
		if event.Type == evmtypes.EventTypeBlockBloom {
			for _, attr := range event.Attributes {
				if attr.Key == evmtypes.AttributeKeyEthereumBloom {
					bloom = ethtypes.BytesToBloom([]byte(attr.Value))
				}
			}
		}
	}
	var miner common.Address
	if validator, ok := a.stakingKeeper.GetValidatorByConsAddr(after, sdk.ConsAddress(block.ProposerAddress)); ok {
		miner = common.BytesToAddress(validator.GetOperator())
	}
	if gasLimit == -1 {
		gasLimit = int64(^uint32(0))
	}
	fields := rpctypes.FormatBlock(block.Header, block.Size(), gasLimit, new(big.Int).SetUint64(gas), hashes, bloom, miner, fee)
	file.Block = tracer.BuildPipelineBlock(fields)
	return file, tracer.BuildPilelineBlockHeader(fields), nil
}

func appendReplayTransaction(file *dtypes.BlockFile, addrs map[common.Address]struct{}, call *tracer.CallTracer, tx *ethtypes.Transaction, from common.Address, fee *big.Int, msgIndex int, outer *abci.ResponseDeliverTx, data *sdk.TxMsgData) error {
	// The RPC charges the full signed gas limit for post-execution block OOG.
	// Ante may also hit this condition before the first VM call.
	failure, charged := "exceeds block gas limit", tx.Gas()
	var response evmtypes.MsgEthereumTxResponse
	if outer.Code == 0 {
		if msgIndex >= len(data.MsgResponses) || data.MsgResponses[msgIndex] == nil || call == nil {
			return fmt.Errorf("missing signed execution or response")
		}
		msg := data.MsgResponses[msgIndex]
		if msg.TypeUrl != "/"+proto.MessageName(&response) {
			return fmt.Errorf("unexpected response %s", msg.TypeUrl)
		}
		if err := response.Unmarshal(msg.Value); err != nil {
			return err
		}
		if response.Hash != tx.Hash().Hex() {
			return fmt.Errorf("response hash mismatch: %s", response.Hash)
		}
		failure, charged = response.VmError, response.GasUsed
		if len(data.MsgResponses) == 1 {
			charged = uint64(outer.GasUsed)
		}
	}
	if call != nil && (outer.Code == 0 || call.Evm != nil) {
		tr, err := call.ReplayResult(failure)
		if err != nil {
			return err
		}
		if outer.Code == 0 && (tr.Transaction.ID != tx.Hash().Hex() || tr.Transaction.GasUsed == nil || tr.Transaction.GasUsed.Uint64() != response.GasUsed) {
			return fmt.Errorf("traced execution differs from message response")
		}
		var roots int
		for _, list := range [][]dtypes.Trace{tr.Traces, tr.ErrorTraces} {
			for _, frame := range list {
				if len(frame.TraceAddress) == 0 {
					roots++
					if (frame.Error == "") != (failure == "") {
						return fmt.Errorf("root status differs from message result")
					}
				}
			}
		}
		if roots != 1 {
			return fmt.Errorf("expected one root call, got %d", roots)
		}
		if len(tr.Events) != len(response.Logs) {
			return fmt.Errorf("committed log count %d, want %d", len(tr.Events), len(response.Logs))
		}
		for i, log := range response.Logs {
			e := &tr.Events[i]
			topics := e.Topics
			if e.Selector != "" {
				topics = append([]string{e.Selector}, topics...)
			}
			if log == nil || !strings.EqualFold(e.Address, log.Address) || !bytes.Equal(e.Data, log.Data) || !equalTopics(topics, log.Topics) {
				return fmt.Errorf("committed log %d differs from message result", i)
			}
			e.LogIndex = int64(len(file.Events) + i)
		}
		for _, addr := range tr.StorageContracts {
			addrs[common.HexToAddress(addr)] = struct{}{}
		}
		file.Traces = append(file.Traces, tr.Traces...)
		file.ErrorTraces = append(file.ErrorTraces, tr.ErrorTraces...)
		file.Events = append(file.Events, tr.Events...)
		file.ErrorEvents = append(file.ErrorEvents, tr.ErrorEvents...)
	}
	transaction := tracer.BuildPipelineTransaction(tx, int64(len(file.Txs)), from, new(big.Int).SetUint64(charged), fee, failure == "")
	// The Kava writer exposes the caps from core.Message on legacy transactions as well.
	transaction.GasFeeCap, transaction.GasTipCap = tx.GasFeeCap(), tx.GasTipCap()
	file.Txs = append(file.Txs, transaction)
	return nil
}

func equalTopics(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !strings.EqualFold(a[i], b[i]) {
			return false
		}
	}
	return true
}
