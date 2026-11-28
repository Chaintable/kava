package app

import (
	"bytes"
	"fmt"

	dbm "github.com/cometbft/cometbft-db"
	abci "github.com/cometbft/cometbft/abci/types"
	tmstate "github.com/cometbft/cometbft/proto/tendermint/state"
	"github.com/cometbft/cometbft/state"
	"github.com/cosmos/gogoproto/proto"
	"github.com/evmos/ethermint/rpc/backend"
	rpctypes "github.com/evmos/ethermint/rpc/types"
)

// These Comet v0.37 state readers exit the process on a protobuf decode error.
// Check the records before handing them to Comet for checkpoint resolution.
type replayCometDB struct{ dbm.DB }

func (d replayCometDB) Get(key []byte) ([]byte, error) {
	raw, err := d.DB.Get(key)
	if err != nil || len(raw) == 0 {
		return raw, err
	}
	var record interface{ Unmarshal([]byte) error }
	switch {
	case bytes.HasPrefix(key, []byte("abciResponsesKey:")):
		record = new(tmstate.ABCIResponses)
	case bytes.HasPrefix(key, []byte("validatorsKey:")):
		record = new(tmstate.ValidatorsInfo)
	case bytes.HasPrefix(key, []byte("consensusParamsKey:")):
		record = new(tmstate.ConsensusParamsInfo)
	default:
		return raw, nil
	}
	if err := record.Unmarshal(raw); err != nil {
		return nil, fmt.Errorf("corrupt Comet record %s: %w", key, err)
	}
	return raw, nil
}

// LastResultsHash omits ABCI events and block-gas diagnostics used by the writer.
// Require the original Comet responses to verify those fields as well.
func (r *ReplayArchive) verifyResults(height int64, results *tmstate.ABCIResponses, nextHash []byte) error {
	if !bytes.Equal(state.ABCIResponsesResultsHash(results), nextHash) {
		return fmt.Errorf("DeliverTx results hash mismatch at %d", height)
	}
	want, err := r.state.LoadABCIResponses(height)
	if err != nil {
		return fmt.Errorf("archived ABCI responses at %d: %w", height, err)
	}
	if want.BeginBlock == nil || want.EndBlock == nil || len(want.DeliverTxs) != len(results.DeliverTxs) || !bytes.Equal(state.ABCIResponsesResultsHash(want), nextHash) {
		return fmt.Errorf("inconsistent archived ABCI responses at %d", height)
	}
	if !sameEvents(want.BeginBlock.Events, results.BeginBlock.Events) || !sameEvents(want.EndBlock.Events, results.EndBlock.Events) {
		return fmt.Errorf("block events differ from archive at %d", height)
	}
	if !proto.Equal(want.EndBlock.ConsensusParamUpdates, results.EndBlock.ConsensusParamUpdates) || len(want.EndBlock.ValidatorUpdates) != len(results.EndBlock.ValidatorUpdates) {
		return fmt.Errorf("EndBlock updates differ at %d", height)
	}
	for i, update := range want.EndBlock.ValidatorUpdates {
		if !proto.Equal(&update, &results.EndBlock.ValidatorUpdates[i]) {
			return fmt.Errorf("validator update %d differs at %d", i, height)
		}
	}
	for i, w := range want.DeliverTxs {
		g := results.DeliverTxs[i]
		if w == nil || w.Codespace != g.Codespace || rpctypes.TxExceedBlockGasLimit(w) != rpctypes.TxExceedBlockGasLimit(g) ||
			backend.ShouldIgnoreGasUsed(w) != backend.ShouldIgnoreGasUsed(g) || !sameEvents(w.Events, g.Events) {
			return fmt.Errorf("archived tx outcome or events differ at %d/%d", height, i)
		}
	}
	return nil
}

func sameEvents(a, b []abci.Event) bool {
	if len(a) != len(b) {
		return false
	}
	for i, e := range a {
		other := b[i]
		if e.Type != other.Type || len(e.Attributes) != len(other.Attributes) {
			return false
		}
		for j, v := range e.Attributes {
			// Index is a node-local indexing choice.
			if v.Key != other.Attributes[j].Key || v.Value != other.Attributes[j].Value {
				return false
			}
		}
	}
	return true
}
