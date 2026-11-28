//go:build rocksdb

package cmd

import (
	dbm "github.com/cometbft/cometbft-db"
	"github.com/linxGnu/grocksdb"
)

func openReplayRocksDB(path string) (dbm.DB, error) {
	options := grocksdb.NewDefaultOptions()
	defer options.Destroy()
	options.SetMaxOpenFiles(256)
	raw, err := grocksdb.OpenDbForReadOnly(options, path, false)
	if err != nil {
		return nil, err
	}
	return dbm.NewRocksDBWithRawDB(raw, grocksdb.NewDefaultReadOptions(), grocksdb.NewDefaultWriteOptions(), grocksdb.NewDefaultWriteOptions()), nil
}
