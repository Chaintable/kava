//go:build !rocksdb

package cmd

import (
	"fmt"

	dbm "github.com/cometbft/cometbft-db"
)

func openReplayRocksDB(path string) (dbm.DB, error) {
	return nil, fmt.Errorf("%s requires a kava build with -tags rocksdb", path)
}
