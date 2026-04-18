package types

import (
	"context"

	sdk "github.com/cosmos/cosmos-sdk/types"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/vm"
	dtypes "github.com/evmos/ethermint/debank/types"
	evmtypes "github.com/evmos/ethermint/x/evm/types"
)

// AccountKeeper defines the expected account keeper interface
type AccountKeeper interface {
	GetModuleAccount(ctx sdk.Context, moduleName string) authtypes.ModuleAccountI
	GetModuleAddress(moduleName string) sdk.AccAddress
	GetSequence(sdk.Context, sdk.AccAddress) (uint64, error)
}

// BankKeeper defines the expected bank keeper interface
type BankKeeper interface {
	evmtypes.BankKeeper

	GetSupply(ctx sdk.Context, denom string) sdk.Coin
	SpendableCoins(ctx sdk.Context, addr sdk.AccAddress) sdk.Coins
	SendCoins(ctx sdk.Context, fromAddr sdk.AccAddress, toAddr sdk.AccAddress, amt sdk.Coins) error
}

// EvmKeeper defines the expected interface needed to make EVM transactions.
type EvmKeeper interface {
	// This is actually a gRPC query method
	EstimateGas(ctx context.Context, req *evmtypes.EthCallRequest) (*evmtypes.EstimateGasResponse, error)
	ApplyMessage(ctx sdk.Context, msg core.Message, tracer vm.EVMLogger, commit bool) (*evmtypes.MsgEthereumTxResponse, error)
	// AppendNonEVMStateDiff persists state changes from non-EVM paths (e.g.
	// evmutil mint/burn) so that trace_debankBlock can include them.
	AppendNonEVMStateDiff(ctx sdk.Context, diff dtypes.TransactionStateDiff) error
}
