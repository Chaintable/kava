package app

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"

	clog "cosmossdk.io/log"
	dbm "github.com/cometbft/cometbft-db"
	"github.com/cometbft/cometbft/libs/log"
	"github.com/cosmos/cosmos-sdk/store/rootmulti"
	storetypes "github.com/cosmos/cosmos-sdk/store/types"
	"github.com/cosmos/cosmos-sdk/store/wrapper"
	sdk "github.com/cosmos/cosmos-sdk/types"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	vesting "github.com/cosmos/cosmos-sdk/x/auth/vesting/exported"
	vesttypes "github.com/cosmos/cosmos-sdk/x/auth/vesting/types"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	"github.com/cosmos/iavl"
	iavldb "github.com/cosmos/iavl/db"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	dtypes "github.com/evmos/ethermint/debank/types"
	evmtypes "github.com/evmos/ethermint/x/evm/types"
	"github.com/holiman/uint256"

	utiltypes "github.com/kava-labs/kava/x/evmutil/types"
)

// Each address is scheduled at the first block second that can change its
// spendable balance. Delegations and incentive extensions also modify x/auth.
type vestingSchedule map[common.Address]int64

func (v vestingSchedule) update(addr common.Address, account authtypes.AccountI, now int64) {
	delete(v, addr)
	va, ok := account.(vesting.VestingAccount)
	if !ok {
		return
	}
	switch acc := va.(type) {
	case *vesttypes.PermanentLockedAccount:
		return
	case *vesttypes.PeriodicVestingAccount:
		if now >= acc.EndTime {
			return
		}
		at := acc.StartTime
		for _, period := range acc.VestingPeriods {
			at += period.Length
			if at > now {
				v[addr] = min(at, acc.EndTime)
				return
			}
		}
		v[addr] = acc.EndTime
	case *vesttypes.DelayedVestingAccount:
		if now < acc.EndTime {
			v[addr] = acc.EndTime
		}
	case *vesttypes.ContinuousVestingAccount:
		if now < acc.EndTime {
			v[addr] = max(now, acc.StartTime) + 1
		}
	default:
		// An account with custom LockedCoins still gets checked at each new second.
		v[addr] = now + 1
	}
}

// Scan the authenticated parent directly. Scanning through the SDK write
// overlay would cache every physical IAVL node read for the whole account set.
func (a *App) seedVesting(ctx context.Context, db dbm.DB, id storetypes.CommitID, now int64) (vestingSchedule, error) {
	tree := openReplayTree(db, authtypes.StoreKey)
	defer tree.Close()
	parent, err := tree.GetImmutable(id.Version)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(parent.Hash(), id.Hash) {
		return nil, fmt.Errorf("vesting parent root mismatch at %d", id.Version)
	}
	prefix := authtypes.AddressStoreKeyPrefix
	it, err := parent.Iterator(prefix, storetypes.PrefixEndBytes(prefix), true)
	if err != nil {
		return nil, err
	}
	defer it.Close()
	schedule := vestingSchedule{}
	for ; it.Valid(); it.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		key := it.Key()[len(prefix):]
		if len(key) != common.AddressLength {
			continue
		}
		var account authtypes.AccountI
		if err := a.appCodec.UnmarshalInterface(it.Value(), &account); err != nil {
			return nil, err
		}
		if !bytes.Equal(key, account.GetAddress()) {
			return nil, fmt.Errorf("auth address mismatch for %X", key)
		}
		schedule.update(common.BytesToAddress(key), account, now)
	}
	return schedule, it.Error()
}

type replayChanges struct {
	addresses map[common.Address]struct{}
	slots     map[common.Address][]common.Hash
	codes     map[common.Hash][]byte
}

func (c *replayChanges) collect(store string, p *iavl.KVPair) error {
	key := p.Key
	switch {
	case store == authtypes.StoreKey && bytes.HasPrefix(key, authtypes.AddressStoreKeyPrefix):
		key = key[len(authtypes.AddressStoreKeyPrefix):]
	case store == banktypes.StoreKey && bytes.HasPrefix(key, banktypes.BalancesPrefix):
		addr, denom, err := banktypes.AddressAndDenomFromBalancesStore(key[len(banktypes.BalancesPrefix):])
		if err != nil || denom == "" {
			return fmt.Errorf("malformed bank balance key %X", key)
		}
		// SpendableCoins subtracts locked coins across all denoms.
		key = addr
	case store == utiltypes.StoreKey && bytes.HasPrefix(key, utiltypes.AccountStoreKeyPrefix):
		key = key[len(utiltypes.AccountStoreKeyPrefix):]
		if len(key) == 0 || int(key[0]) != len(key)-1 {
			return fmt.Errorf("malformed evmutil account key %X", p.Key)
		}
		key = key[1:]
	case store == evmtypes.StoreKey && bytes.HasPrefix(key, evmtypes.KeyPrefixStorage):
		key = key[len(evmtypes.KeyPrefixStorage):]
		if len(key) != common.AddressLength+common.HashLength {
			return fmt.Errorf("malformed EVM storage key %X", p.Key)
		}
		addr := common.BytesToAddress(key[:common.AddressLength])
		c.slots[addr] = append(c.slots[addr], common.BytesToHash(key[common.AddressLength:]))
		return nil
	case store == evmtypes.StoreKey && bytes.HasPrefix(key, evmtypes.KeyPrefixCode):
		key = key[len(evmtypes.KeyPrefixCode):]
		if len(key) != common.HashLength {
			return fmt.Errorf("malformed EVM code key %X", p.Key)
		}
		if !p.Delete && len(p.Value) > 0 {
			return c.addCode(common.BytesToHash(key), p.Value)
		}
		return nil
	default:
		return nil
	}
	if len(key) == common.AddressLength {
		c.addresses[common.BytesToAddress(key)] = struct{}{}
	}
	return nil
}

func (c *replayChanges) addCode(hash common.Hash, code []byte) error {
	if len(code) == 0 || crypto.Keccak256Hash(code) != hash {
		return fmt.Errorf("bytecode missing or invalid for %s", hash)
	}
	c.codes[hash] = code
	return nil
}

func archiveCommitInfo(db dbm.DB, height int64, appHash []byte) (map[string]storetypes.CommitID, error) {
	info, err := rootmulti.NewStore(db, log.NewNopLogger()).GetCommitInfo(height)
	if err != nil {
		return nil, err
	}
	if info.Version != height || !bytes.Equal(info.Hash(), appHash) {
		return nil, fmt.Errorf("archive CommitInfo disagrees with header at %d", height)
	}
	ids := make(map[string]storetypes.CommitID, len(info.StoreInfos))
	for _, s := range info.StoreInfos {
		ids[s.Name] = s.CommitId
	}
	return ids, nil
}

func openReplayTree(db dbm.DB, name string) *iavl.MutableTree {
	prefix := dbm.NewPrefixDB(db, []byte("s/k:"+name+"/"))
	return iavl.NewMutableTree(iavldb.NewWrapper(wrapper.NewCosmosDB(prefix)), 128, true, clog.NewNopLogger())
}

func readStoreChanges(db dbm.DB, name string, height int64, parent, current storetypes.CommitID, c *replayChanges) error {
	tree := openReplayTree(db, name)
	defer tree.Close()
	for i, id := range []storetypes.CommitID{parent, current} {
		version := height - 1 + int64(i)
		if id.Version != version {
			return fmt.Errorf("missing %s commit at %d", name, version)
		}
		root, err := tree.GetImmutable(version)
		if err != nil {
			return fmt.Errorf("%s root %d: %w", name, version, err)
		}
		if !bytes.Equal(root.Hash(), id.Hash) {
			return fmt.Errorf("%s root hash mismatch at %d", name, version)
		}
	}
	// This Kava fork takes an inclusive end and silently clamps missing
	// versions. Require both adjacent roots above and exactly the H callback.
	seen := false
	err := tree.TraverseStateChanges(height, height, func(v int64, changes *iavl.ChangeSet) error {
		if seen || v != height {
			return fmt.Errorf("unexpected %s changes at %d", name, v)
		}
		seen = true
		for _, pair := range changes.Pairs {
			if err := c.collect(name, pair); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil && !seen {
		return fmt.Errorf("missing %s change set at %d", name, height)
	}
	return err
}

func (a *App) replayAccount(ctx sdk.Context, addr common.Address) (dtypes.NewAccount, bool, error) {
	balance := a.evmKeeper.GetBalance(ctx, addr)
	amount, overflow := uint256.FromBig(balance)
	if balance.Sign() < 0 || overflow {
		return dtypes.NewAccount{}, false, fmt.Errorf("invalid akava balance for %s", addr)
	}
	record := dtypes.NewAccount{Address: crypto.Keccak256Hash(addr[:]), Balance: amount, CodeHash: common.BytesToHash(evmtypes.EmptyCodeHash)}
	acc := a.evmKeeper.GetAccountWithoutBalance(ctx, addr)
	if acc != nil {
		record.Nonce, record.CodeHash = acc.Nonce, common.BytesToHash(acc.CodeHash)
	}
	return record, acc != nil || !amount.IsZero(), nil
}

func (a *App) replayStateDiff(ctx context.Context, db dbm.DB, before, after sdk.Context, nextHash []byte, schedule vestingSchedule, file *dtypes.BlockFile) (*dtypes.BlockStorageDiff, error) {
	h := after.BlockHeight()
	old, err := archiveCommitInfo(db, h-1, after.BlockHeader().AppHash)
	if err != nil {
		return nil, err
	}
	current, err := archiveCommitInfo(db, h, nextHash)
	if err != nil {
		return nil, err
	}
	c := replayChanges{
		addresses: make(map[common.Address]struct{}),
		slots:     make(map[common.Address][]common.Hash),
		codes:     make(map[common.Hash][]byte),
	}
	for _, name := range []string{authtypes.StoreKey, banktypes.StoreKey, evmtypes.StoreKey, utiltypes.StoreKey} {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := readStoreChanges(db, name, h, old[name], current[name], &c); err != nil {
			return nil, err
		}
	}
	for addr, due := range schedule {
		if due <= after.BlockTime().Unix() {
			c.addresses[addr] = struct{}{}
		}
	}
	diff := &dtypes.BlockStorageDiff{Hash: common.BytesToHash(after.BlockHeader().AppHash), ParentHash: common.BytesToHash(before.BlockHeader().AppHash)}
	contracts := map[string]struct{}{}
	for _, addr := range file.StorageContracts {
		contracts[addr] = struct{}{}
	}
	for addr := range c.addresses {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		prev, existed, err := a.replayAccount(before, addr)
		if err != nil {
			return nil, err
		}
		now, exists, err := a.replayAccount(after, addr)
		if err != nil {
			return nil, err
		}
		schedule.update(addr, a.accountKeeper.GetAccount(after, addr.Bytes()), after.BlockTime().Unix())
		if exists == existed && prev.Nonce == now.Nonce && prev.CodeHash == now.CodeHash && prev.Balance.Eq(now.Balance) {
			continue
		}
		contracts[strings.ToLower(addr.Hex())] = struct{}{}
		if !exists {
			diff.DeletedAccounts = append(diff.DeletedAccounts, prev.Address)
			continue
		}
		diff.NewAccounts = append(diff.NewAccounts, now)
		if now.CodeHash != prev.CodeHash && now.CodeHash != common.BytesToHash(evmtypes.EmptyCodeHash) {
			if err := c.addCode(now.CodeHash, a.evmKeeper.GetCode(after, now.CodeHash)); err != nil {
				return nil, err
			}
		}
	}
	for addr, slots := range c.slots {
		storage := dtypes.AccountStorageDiff{Address: crypto.Keccak256Hash(addr[:])}
		for _, slot := range slots {
			value := a.evmKeeper.GetState(after, addr, slot)
			if value == a.evmKeeper.GetState(before, addr, slot) {
				continue
			}
			storage.Values = append(storage.Values, dtypes.IndexValuePair{Index: crypto.Keccak256Hash(slot[:]), Value: new(uint256.Int).SetBytes(value[:])})
		}
		if len(storage.Values) > 0 {
			contracts[strings.ToLower(addr.Hex())] = struct{}{}
			sort.Slice(storage.Values, func(i, j int) bool { return bytes.Compare(storage.Values[i].Index[:], storage.Values[j].Index[:]) < 0 })
			diff.StorageDiff = append(diff.StorageDiff, storage)
		}
	}
	for hash, code := range c.codes {
		diff.NewCodes = append(diff.NewCodes, dtypes.NewCode{CodeHash: hash, Code: code})
	}
	sort.Slice(diff.NewAccounts, func(i, j int) bool {
		return bytes.Compare(diff.NewAccounts[i].Address[:], diff.NewAccounts[j].Address[:]) < 0
	})
	sort.Slice(diff.DeletedAccounts, func(i, j int) bool { return bytes.Compare(diff.DeletedAccounts[i][:], diff.DeletedAccounts[j][:]) < 0 })
	sort.Slice(diff.NewCodes, func(i, j int) bool {
		return bytes.Compare(diff.NewCodes[i].CodeHash[:], diff.NewCodes[j].CodeHash[:]) < 0
	})
	sort.Slice(diff.StorageDiff, func(i, j int) bool {
		return bytes.Compare(diff.StorageDiff[i].Address[:], diff.StorageDiff[j].Address[:]) < 0
	})
	file.StorageContracts = make([]string, 0, len(contracts))
	for addr := range contracts {
		file.StorageContracts = append(file.StorageContracts, addr)
	}
	sort.Strings(file.StorageContracts)
	return diff, nil
}
