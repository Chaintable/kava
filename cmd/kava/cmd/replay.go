package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	dbm "github.com/cometbft/cometbft-db"
	"github.com/cosmos/cosmos-sdk/client/flags"
	"github.com/ethereum/go-ethereum/common/hexutil"
	gethrpc "github.com/ethereum/go-ethereum/rpc"
	rpctypes "github.com/evmos/ethermint/rpc/types"
	"github.com/spf13/cobra"
	"github.com/syndtr/goleveldb/leveldb/opt"

	"github.com/kava-labs/kava/app"
	"github.com/kava-labs/kava/app/params"
)

// The offline commands bypass the node's config pre-run, which initializes
// missing files in --home. The archive can be mounted read-only.
func withReplayArchive(cmd *cobra.Command, encoding params.EncodingConfig, run func(*app.ReplayArchive) error) (err error) {
	home, err := cmd.Flags().GetString(flags.FlagHome)
	if err != nil {
		return err
	}
	scratch, err := os.MkdirTemp("", "kava-replay-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	var databases []dbm.DB
	defer func() {
		for _, db := range databases {
			err = errors.Join(err, db.Close())
		}
	}()
	dir := filepath.Join(home, "data")
	for _, name := range []string{"blockstore", "state", "application"} {
		path := filepath.Join(dir, name+".db")
		if _, err := os.Stat(filepath.Join(path, "CURRENT")); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		options, err := filepath.Glob(filepath.Join(path, "OPTIONS-*"))
		if err != nil {
			return err
		}
		var db dbm.DB
		if len(options) > 0 {
			db, err = openReplayRocksDB(path)
		} else {
			db, err = dbm.NewGoLevelDBWithOpts(name, dir, &opt.Options{ReadOnly: true, ErrorIfMissing: true})
		}
		if err != nil {
			return fmt.Errorf("open %s read-only: %w", path, err)
		}
		databases = append(databases, db)
	}
	archive := app.NewReplayArchive(databases[0], databases[1], databases[2], scratch, encoding)
	return run(archive)
}

func newReplayCmd(encoding params.EncodingConfig) *cobra.Command {
	return &cobra.Command{
		Use:               "replay-block HEIGHT [END]",
		Short:             "Replay an inclusive range from a stopped Kava v0.28 archive, emitting one writer result per line",
		Args:              cobra.RangeArgs(1, 2),
		PersistentPreRunE: func(*cobra.Command, []string) error { return nil },
		RunE: func(cmd *cobra.Command, args []string) error {
			first, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return err
			}
			last := first
			if len(args) == 2 {
				last, err = strconv.ParseInt(args[1], 10, 64)
				if err != nil {
					return err
				}
			}
			if first < app.ReplayFirstHeight || last < first {
				return fmt.Errorf("invalid v0.28 range %d..%d", first, last)
			}
			return withReplayArchive(cmd, encoding, func(r *app.ReplayArchive) error {
				if last > r.LastHeight() {
					return fmt.Errorf("requested end %d exceeds verified archive range ending at %d", last, r.LastHeight())
				}
				out := json.NewEncoder(cmd.OutOrStdout())
				for h := first; h <= last; h++ {
					n := rpctypes.BlockNumber(h)
					result, err := r.Replay(cmd.Context(), rpctypes.BlockNumberOrHash{BlockNumber: &n})
					if err != nil {
						return fmt.Errorf("block %d: %w", h, err)
					}
					if err := out.Encode(result); err != nil {
						return err
					}
				}
				return nil
			})
		},
	}
}

type replayTraceAPI struct{ archive *app.ReplayArchive }

func (s replayTraceAPI) DebankBlock(ctx context.Context, height rpctypes.BlockNumberOrHash) (*rpctypes.DebankOutPutJs, error) {
	return s.archive.Replay(ctx, height)
}

type replayEthAPI struct{ archive *app.ReplayArchive }

func (s replayEthAPI) BlockNumber() hexutil.Uint64 { return hexutil.Uint64(s.archive.LastHeight()) }
func (s replayEthAPI) ChainId() hexutil.Uint64     { return 2222 }

func newReplayServeCmd(encoding params.EncodingConfig) *cobra.Command {
	var listen string
	cmd := &cobra.Command{
		Use:               "replay-serve",
		Short:             "Serve trace_debankBlock from a stopped Kava v0.28 archive",
		Args:              cobra.NoArgs,
		PersistentPreRunE: func(*cobra.Command, []string) error { return nil },
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return withReplayArchive(cmd, encoding, func(r *app.ReplayArchive) error {
				if r.LastHeight() < app.ReplayFirstHeight {
					return fmt.Errorf("archive ends before Kava v0.28")
				}
				rpc := gethrpc.NewServer()
				defer rpc.Stop()
				if err := rpc.RegisterName("trace", replayTraceAPI{r}); err != nil {
					return err
				}
				if err := rpc.RegisterName("eth", replayEthAPI{r}); err != nil {
					return err
				}
				listener, err := net.Listen("tcp", listen)
				if err != nil {
					return err
				}
				server := &http.Server{Handler: rpc, ReadHeaderTimeout: 10 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
				done := make(chan error, 1)
				go func() { done <- server.Serve(listener) }()
				select {
				case err := <-done:
					stop()
					return errors.Join(err, server.Shutdown(context.Background()))
				case <-ctx.Done():
					// Drain handlers before closing their database handles.
					return server.Shutdown(context.Background())
				}
			})
		},
	}
	cmd.Flags().StringVar(&listen, "listen", "127.0.0.1:8545", "HTTP JSON-RPC listen address")
	return cmd
}
