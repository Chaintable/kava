package app

import (
	"context"
	"fmt"

	gogogrpc "github.com/cosmos/gogoproto/grpc"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/eth/tracers"
	"github.com/evmos/ethermint/debank/tracer"
	evmtypes "github.com/evmos/ethermint/x/evm/types"
	"google.golang.org/grpc"
)

// The SDK discards MsgResponses when its block gas meter rolls back a transaction.
// Observe actual message dispatch separately from the VM tracer so a missing
// callback cannot turn an executed message into an apparently empty trace.
type signedCall struct {
	hash     common.Hash
	trace    *tracer.CallTracer
	response *evmtypes.MsgEthereumTxResponse
}

type replayMsgServer struct {
	evmtypes.MsgServer
	router gogogrpc.Server
	calls  []*signedCall
	active *signedCall
	err    error
}

func (s *replayMsgServer) wrap(router gogogrpc.Server) gogogrpc.Server {
	if s == nil {
		return router
	}
	s.router = router
	return s
}

func (s *replayMsgServer) RegisterService(desc *grpc.ServiceDesc, server interface{}) {
	if desc.ServiceName == "ethermint.evm.v1.Msg" {
		s.MsgServer = server.(evmtypes.MsgServer)
		server = s
	}
	s.router.RegisterService(desc, server)
}

func (s *replayMsgServer) EthereumTx(ctx context.Context, msg *evmtypes.MsgEthereumTx) (*evmtypes.MsgEthereumTxResponse, error) {
	call := &signedCall{hash: msg.AsTransaction().Hash()}
	s.calls = append(s.calls, call)
	previous := s.active
	s.active = call
	defer func() { s.active = previous }()
	response, err := s.MsgServer.EthereumTx(ctx, msg)
	call.response = response
	return response, err
}

func (s *replayMsgServer) trace(hash common.Hash) vm.EVMLogger {
	if s.active == nil || s.active.hash != hash || s.active.trace != nil {
		s.err = fmt.Errorf("unexpected signed EVM trace for %s", hash)
		return nil
	}
	s.active.trace = tracer.NewCallTracer(&tracers.Context{TxHash: hash})
	return s.active.trace
}
