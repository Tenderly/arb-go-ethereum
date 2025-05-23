// Copyright 2014 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

// Package state provides a caching layer atop the Ethereum state trie.
package state

import (
	"bytes"
	"fmt"
	"maps"
	"math/big"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state/snapshot"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/ethereum/go-ethereum/trie/trienode"
	"github.com/ethereum/go-ethereum/trie/triestate"
	"github.com/holiman/uint256"
	"golang.org/x/sync/errgroup"
)

// TriesInMemory represents the number of layers that are kept in RAM.
const DefaultTriesInMemory = 128

type revision struct {
	id           int
	journalIndex int

	// Arbitrum: track the total balance change across all accounts
	unexpectedBalanceDelta *big.Int
}

type mutationType int

const (
	update mutationType = iota
	deletion
)

type mutation struct {
	typ     mutationType
	applied bool
}

func (m *mutation) copy() *mutation {
	return &mutation{typ: m.typ, applied: m.applied}
}

func (m *mutation) isDelete() bool {
	return m.typ == deletion
}

// StateDB structs within the ethereum protocol are used to store anything
// within the merkle trie. StateDBs take care of caching and storing
// nested states. It's the general query interface to retrieve:
//
// * Contracts
// * Accounts
//
// Once the state is committed, tries cached in stateDB (including account
// trie, storage tries) will no longer be functional. A new state instance
// must be created with new root and updated database for accessing post-
// commit states.
type StateDB struct {
	arbExtraData *ArbitrumExtraData // must be a pointer - can't be a part of StateDB allocation, otherwise its finalizer might not get called

	db         Database
	prefetcher *triePrefetcher
	trie       Trie
	hasher     crypto.KeccakState
	logger     *tracing.Hooks
	snaps      *snapshot.Tree    // Nil if snapshot is not available
	snap       snapshot.Snapshot // Nil if snapshot is not available

	// originalRoot is the pre-state root, before any changes were made.
	// It will be updated when the Commit is called.
	originalRoot common.Hash

	// These maps hold the state changes (including the corresponding
	// original value) that occurred in this **block**.
	accounts       map[common.Hash][]byte                    // The mutated accounts in 'slim RLP' encoding
	storages       map[common.Hash]map[common.Hash][]byte    // The mutated slots in prefix-zero trimmed rlp format
	accountsOrigin map[common.Address][]byte                 // The original value of mutated accounts in 'slim RLP' encoding
	storagesOrigin map[common.Address]map[common.Hash][]byte // The original value of mutated slots in prefix-zero trimmed rlp format

	// This map holds 'live' objects, which will get modified while
	// processing a state transition.
	stateObjects map[common.Address]*stateObject

	// This map holds 'deleted' objects. An object with the same address
	// might also occur in the 'stateObjects' map due to account
	// resurrection. The account value is tracked as the original value
	// before the transition. This map is populated at the transaction
	// boundaries.
	stateObjectsDestruct map[common.Address]*types.StateAccount

	// This map tracks the account mutations that occurred during the
	// transition. Uncommitted mutations belonging to the same account
	// can be merged into a single one which is equivalent from database's
	// perspective. This map is populated at the transaction boundaries.
	mutations map[common.Address]*mutation

	// DB error.
	// State objects are used by the consensus core and VM which are
	// unable to deal with database-level errors. Any error that occurs
	// during a database read is memoized here and will eventually be
	// returned by StateDB.Commit. Notably, this error is also shared
	// by all cached state objects in case the database failure occurs
	// when accessing state of accounts.
	dbErr error

	// The refund counter, also used by state transitioning.
	refund uint64

	// The tx context and all occurred logs in the scope of transaction.
	thash   common.Hash
	txIndex int
	logs    map[common.Hash][]*types.Log
	logSize uint

	// Preimages occurred seen by VM in the scope of block.
	preimages map[common.Hash][]byte

	// Per-transaction access list
	accessList *accessList

	// Transient storage
	transientStorage transientStorage

	// Journal of state modifications. This is the backbone of
	// Snapshot and RevertToSnapshot.
	journal        *journal
	validRevisions []revision
	nextRevisionId int

	// Measurements gathered during execution for debugging purposes
	AccountReads         time.Duration
	AccountHashes        time.Duration
	AccountUpdates       time.Duration
	AccountCommits       time.Duration
	StorageReads         time.Duration
	StorageUpdates       time.Duration
	StorageCommits       time.Duration
	SnapshotAccountReads time.Duration
	SnapshotStorageReads time.Duration
	SnapshotCommits      time.Duration
	TrieDBCommits        time.Duration

	AccountUpdated int
	StorageUpdated int
	AccountDeleted int
	StorageDeleted int

	// Testing hooks
	onCommit func(states *triestate.Set) // Hook invoked when commit is performed

	deterministic bool
}

// New creates a new state from a given trie.
func New(root common.Hash, db Database, snaps *snapshot.Tree) (*StateDB, error) {
	tr, err := db.OpenTrie(root)
	if err != nil {
		return nil, err
	}
	sdb := &StateDB{
		arbExtraData: &ArbitrumExtraData{
			unexpectedBalanceDelta: new(big.Int),
			openWasmPages:          0,
			everWasmPages:          0,
			activatedWasms:         make(map[common.Hash]ActivatedWasm),
			recentWasms:            NewRecentWasms(),
		},

		db:                   db,
		trie:                 tr,
		originalRoot:         root,
		snaps:                snaps,
		accounts:             make(map[common.Hash][]byte),
		storages:             make(map[common.Hash]map[common.Hash][]byte),
		accountsOrigin:       make(map[common.Address][]byte),
		storagesOrigin:       make(map[common.Address]map[common.Hash][]byte),
		stateObjects:         make(map[common.Address]*stateObject),
		stateObjectsDestruct: make(map[common.Address]*types.StateAccount),
		mutations:            make(map[common.Address]*mutation),
		logs:                 make(map[common.Hash][]*types.Log),
		preimages:            make(map[common.Hash][]byte),
		journal:              newJournal(),
		accessList:           newAccessList(),
		transientStorage:     newTransientStorage(),
		hasher:               crypto.NewKeccakState(),
	}
	if sdb.snaps != nil {
		sdb.snap = sdb.snaps.Snapshot(root)
	}
	return sdb, nil
}

func (s *StateDB) FilterTx() {
	s.arbExtraData.arbTxFilter = true
}

func (s *StateDB) ClearTxFilter() {
	s.arbExtraData.arbTxFilter = false
}

func (s *StateDB) IsTxFiltered() bool {
	return s.arbExtraData.arbTxFilter
}

// SetLogger sets the logger for account update hooks.
func (s *StateDB) SetLogger(l *tracing.Hooks) {
	s.logger = l
}

// StartPrefetcher initializes a new trie prefetcher to pull in nodes from the
// state trie concurrently while the state is mutated so that when we reach the
// commit phase, most of the needed data is already hot.
func (s *StateDB) StartPrefetcher(namespace string) {
	if s.prefetcher != nil {
		s.prefetcher.close()
		s.prefetcher = nil
	}
	if s.snap != nil {
		s.prefetcher = newTriePrefetcher(s.db, s.originalRoot, namespace)
	}
}

// StopPrefetcher terminates a running prefetcher and reports any leftover stats
// from the gathered metrics.
func (s *StateDB) StopPrefetcher() {
	if s.prefetcher != nil {
		s.prefetcher.close()
		s.prefetcher = nil
	}
}

// setError remembers the first non-nil error it is called with.
func (s *StateDB) setError(err error) {
	if s.dbErr == nil {
		s.dbErr = err
	}
}

// Error returns the memorized database failure occurred earlier.
func (s *StateDB) Error() error {
	return s.dbErr
}

func (s *StateDB) AddLog(log *types.Log) {
	s.journal.append(addLogChange{txhash: s.thash})

	log.TxHash = s.thash
	log.TxIndex = uint(s.txIndex)
	log.Index = s.logSize
	if s.logger != nil && s.logger.OnLog != nil {
		s.logger.OnLog(log)
	}
	s.logs[s.thash] = append(s.logs[s.thash], log)
	s.logSize++
}

// GetLogs returns the logs matching the specified transaction hash, and annotates
// them with the given blockNumber and blockHash.
func (s *StateDB) GetLogs(hash common.Hash, blockNumber uint64, blockHash common.Hash) []*types.Log {
	logs := s.logs[hash]
	for _, l := range logs {
		l.BlockNumber = blockNumber
		l.BlockHash = blockHash
	}
	return logs
}

func (s *StateDB) Logs() []*types.Log {
	var logs []*types.Log
	for _, lgs := range s.logs {
		logs = append(logs, lgs...)
	}
	return logs
}

// AddPreimage records a SHA3 preimage seen by the VM.
func (s *StateDB) AddPreimage(hash common.Hash, preimage []byte) {
	if _, ok := s.preimages[hash]; !ok {
		s.journal.append(addPreimageChange{hash: hash})
		s.preimages[hash] = slices.Clone(preimage)
	}
}

// Preimages returns a list of SHA3 preimages that have been submitted.
func (s *StateDB) Preimages() map[common.Hash][]byte {
	return s.preimages
}

// AddRefund adds gas to the refund counter
func (s *StateDB) AddRefund(gas uint64) {
	s.journal.append(refundChange{prev: s.refund})
	s.refund += gas
}

// SubRefund removes gas from the refund counter.
// This method will panic if the refund counter goes below zero
func (s *StateDB) SubRefund(gas uint64) {
	s.journal.append(refundChange{prev: s.refund})
	if gas > s.refund {
		panic(fmt.Sprintf("Refund counter below zero (gas: %d > refund: %d)", gas, s.refund))
	}
	s.refund -= gas
}

// Exist reports whether the given account address exists in the state.
// Notably this also returns true for self-destructed accounts.
func (s *StateDB) Exist(addr common.Address) bool {
	return s.getStateObject(addr) != nil
}

// Empty returns whether the state object is either non-existent
// or empty according to the EIP161 specification (balance = nonce = code = 0)
func (s *StateDB) Empty(addr common.Address) bool {
	so := s.getStateObject(addr)
	return so == nil || so.empty()
}

// GetBalance retrieves the balance from the given address or 0 if object not found
func (s *StateDB) GetBalance(addr common.Address) *uint256.Int {
	stateObject := s.getStateObject(addr)
	if stateObject != nil {
		return stateObject.Balance()
	}
	return common.U2560
}

// GetNonce retrieves the nonce from the given address or 0 if object not found
func (s *StateDB) GetNonce(addr common.Address) uint64 {
	stateObject := s.getStateObject(addr)
	if stateObject != nil {
		return stateObject.Nonce()
	}

	return 0
}

// GetStorageRoot retrieves the storage root from the given address or empty
// if object not found.
func (s *StateDB) GetStorageRoot(addr common.Address) common.Hash {
	stateObject := s.getStateObject(addr)
	if stateObject != nil {
		return stateObject.Root()
	}
	return common.Hash{}
}

// TxIndex returns the current transaction index set by Prepare.
func (s *StateDB) TxIndex() int {
	return s.txIndex
}

func (s *StateDB) GetCode(addr common.Address) []byte {
	stateObject := s.getStateObject(addr)
	if stateObject != nil {
		return stateObject.Code()
	}
	return nil
}

func (s *StateDB) GetCodeSize(addr common.Address) int {
	stateObject := s.getStateObject(addr)
	if stateObject != nil {
		return stateObject.CodeSize()
	}
	return 0
}

func (s *StateDB) GetCodeHash(addr common.Address) common.Hash {
	stateObject := s.getStateObject(addr)
	if stateObject != nil {
		return common.BytesToHash(stateObject.CodeHash())
	}
	return common.Hash{}
}

// GetState retrieves a value from the given account's storage trie.
func (s *StateDB) GetState(addr common.Address, hash common.Hash) common.Hash {
	stateObject := s.getStateObject(addr)
	if stateObject != nil {
		return stateObject.GetState(hash)
	}
	return common.Hash{}
}

// GetCommittedState retrieves a value from the given account's committed storage trie.
func (s *StateDB) GetCommittedState(addr common.Address, hash common.Hash) common.Hash {
	stateObject := s.getStateObject(addr)
	if stateObject != nil {
		return stateObject.GetCommittedState(hash)
	}
	return common.Hash{}
}

// Database retrieves the low level database supporting the lower level trie ops.
func (s *StateDB) Database() Database {
	return s.db
}

func (s *StateDB) HasSelfDestructed(addr common.Address) bool {
	stateObject := s.getStateObject(addr)
	if stateObject != nil {
		return stateObject.selfDestructed
	}
	return false
}

/*
 * SETTERS
 */

// AddBalance adds amount to the account associated with addr.
func (s *StateDB) AddBalance(addr common.Address, amount *uint256.Int, reason tracing.BalanceChangeReason) {
	stateObject := s.getOrNewStateObject(addr)
	if stateObject != nil {
		s.arbExtraData.unexpectedBalanceDelta.Add(s.arbExtraData.unexpectedBalanceDelta, amount.ToBig())
		stateObject.AddBalance(amount, reason)
	}
}

// SubBalance subtracts amount from the account associated with addr.
func (s *StateDB) SubBalance(addr common.Address, amount *uint256.Int, reason tracing.BalanceChangeReason) {
	stateObject := s.getOrNewStateObject(addr)
	if stateObject != nil {
		s.arbExtraData.unexpectedBalanceDelta.Sub(s.arbExtraData.unexpectedBalanceDelta, amount.ToBig())
		stateObject.SubBalance(amount, reason)
	}
}

func (s *StateDB) SetBalance(addr common.Address, amount *uint256.Int, reason tracing.BalanceChangeReason) {
	stateObject := s.getOrNewStateObject(addr)
	if stateObject != nil {
		if amount == nil {
			amount = uint256.NewInt(0)
		}
		prevBalance := stateObject.Balance()
		s.arbExtraData.unexpectedBalanceDelta.Add(s.arbExtraData.unexpectedBalanceDelta, amount.ToBig())
		s.arbExtraData.unexpectedBalanceDelta.Sub(s.arbExtraData.unexpectedBalanceDelta, prevBalance.ToBig())
		stateObject.SetBalance(amount, reason)
	}
}

func (s *StateDB) ExpectBalanceBurn(amount *big.Int) {
	if amount.Sign() < 0 {
		panic(fmt.Sprintf("ExpectBalanceBurn called with negative amount %v", amount))
	}
	s.arbExtraData.unexpectedBalanceDelta.Add(s.arbExtraData.unexpectedBalanceDelta, amount)
}

func (s *StateDB) SetNonce(addr common.Address, nonce uint64) {
	stateObject := s.getOrNewStateObject(addr)
	if stateObject != nil {
		stateObject.SetNonce(nonce)
	}
}

func (s *StateDB) SetCode(addr common.Address, code []byte) {
	stateObject := s.getOrNewStateObject(addr)
	if stateObject != nil {
		stateObject.SetCode(crypto.Keccak256Hash(code), code)
	}
}

func (s *StateDB) SetState(addr common.Address, key, value common.Hash) {
	stateObject := s.getOrNewStateObject(addr)
	if stateObject != nil {
		stateObject.SetState(key, value)
	}
}

// SetStorage replaces the entire storage for the specified account with given
// storage. This function should only be used for debugging and the mutations
// must be discarded afterwards.
func (s *StateDB) SetStorage(addr common.Address, storage map[common.Hash]common.Hash) {
	// SetStorage needs to wipe existing storage. We achieve this by pretending
	// that the account self-destructed earlier in this block, by flagging
	// it in stateObjectsDestruct. The effect of doing so is that storage lookups
	// will not hit disk, since it is assumed that the disk-data is belonging
	// to a previous incarnation of the object.
	//
	// TODO(rjl493456442) this function should only be supported by 'unwritable'
	// state and all mutations made should all be discarded afterwards.
	if _, ok := s.stateObjectsDestruct[addr]; !ok {
		s.stateObjectsDestruct[addr] = nil
	}
	stateObject := s.getOrNewStateObject(addr)
	for k, v := range storage {
		stateObject.SetState(k, v)
	}
}

// SelfDestruct marks the given account as selfdestructed.
// This clears the account balance.
//
// The account's state object is still available until the state is committed,
// getStateObject will return a non-nil account after SelfDestruct.
func (s *StateDB) SelfDestruct(addr common.Address) {
	stateObject := s.getStateObject(addr)
	if stateObject == nil {
		return
	}
	var (
		prev = new(uint256.Int).Set(stateObject.Balance())
		n    = new(uint256.Int)
	)
	s.journal.append(selfDestructChange{
		account:     &addr,
		prev:        stateObject.selfDestructed,
		prevbalance: prev,
	})

	if s.logger != nil && s.logger.OnBalanceChange != nil && prev.Sign() > 0 {
		s.logger.OnBalanceChange(addr, prev.ToBig(), n.ToBig(), tracing.BalanceDecreaseSelfdestruct)
	}
	stateObject.markSelfdestructed()
	s.arbExtraData.unexpectedBalanceDelta.Sub(s.arbExtraData.unexpectedBalanceDelta, stateObject.data.Balance.ToBig())
	stateObject.data.Balance = n
}

func (s *StateDB) Selfdestruct6780(addr common.Address) {
	stateObject := s.getStateObject(addr)
	if stateObject == nil {
		return
	}
	if stateObject.newContract {
		s.SelfDestruct(addr)
	}
}

// SetTransientState sets transient storage for a given account. It
// adds the change to the journal so that it can be rolled back
// to its previous value if there is a revert.
func (s *StateDB) SetTransientState(addr common.Address, key, value common.Hash) {
	prev := s.GetTransientState(addr, key)
	if prev == value {
		return
	}
	s.journal.append(transientStorageChange{
		account:  &addr,
		key:      key,
		prevalue: prev,
	})
	s.setTransientState(addr, key, value)
}

// setTransientState is a lower level setter for transient storage. It
// is called during a revert to prevent modifications to the journal.
func (s *StateDB) setTransientState(addr common.Address, key, value common.Hash) {
	s.transientStorage.Set(addr, key, value)
}

// GetTransientState gets transient storage for a given account.
func (s *StateDB) GetTransientState(addr common.Address, key common.Hash) common.Hash {
	return s.transientStorage.Get(addr, key)
}

//
// Setting, updating & deleting state object methods.
//

// updateStateObject writes the given object to the trie.
func (s *StateDB) updateStateObject(obj *stateObject) {
	// Track the amount of time wasted on updating the account from the trie
	defer func(start time.Time) { s.AccountUpdates += time.Since(start) }(time.Now())

	// Encode the account and update the account trie
	addr := obj.Address()
	if err := s.trie.UpdateAccount(addr, &obj.data); err != nil {
		s.setError(fmt.Errorf("updateStateObject (%x) error: %v", addr[:], err))
	}
	if obj.dirtyCode {
		s.trie.UpdateContractCode(obj.Address(), common.BytesToHash(obj.CodeHash()), obj.code)
	}
	// Cache the data until commit. Note, this update mechanism is not symmetric
	// to the deletion, because whereas it is enough to track account updates
	// at commit time, deletions need tracking at transaction boundary level to
	// ensure we capture state clearing.
	s.accounts[obj.addrHash] = types.SlimAccountRLP(obj.data)

	// Track the original value of mutated account, nil means it was not present.
	// Skip if it has been tracked (because updateStateObject may be called
	// multiple times in a block).
	if _, ok := s.accountsOrigin[obj.address]; !ok {
		if obj.origin == nil {
			s.accountsOrigin[obj.address] = nil
		} else {
			s.accountsOrigin[obj.address] = types.SlimAccountRLP(*obj.origin)
		}
	}
}

// deleteStateObject removes the given object from the state trie.
func (s *StateDB) deleteStateObject(addr common.Address) {
	// Track the amount of time wasted on deleting the account from the trie
	defer func(start time.Time) { s.AccountUpdates += time.Since(start) }(time.Now())

	// Delete the account from the trie
	if err := s.trie.DeleteAccount(addr); err != nil {
		s.setError(fmt.Errorf("deleteStateObject (%x) error: %v", addr[:], err))
	}
}

// getStateObject retrieves a state object given by the address, returning nil if
// the object is not found or was deleted in this execution context.
func (s *StateDB) getStateObject(addr common.Address) *stateObject {
	// Prefer live objects if any is available
	if obj := s.stateObjects[addr]; obj != nil {
		return obj
	}
	// Short circuit if the account is already destructed in this block.
	if _, ok := s.stateObjectsDestruct[addr]; ok {
		return nil
	}
	// If no live objects are available, attempt to use snapshots
	var data *types.StateAccount
	if s.snap != nil {
		start := time.Now()
		acc, err := s.snap.Account(crypto.HashData(s.hasher, addr.Bytes()))
		s.SnapshotAccountReads += time.Since(start)

		if err == nil {
			if acc == nil {
				return nil
			}
			data = &types.StateAccount{
				Nonce:    acc.Nonce,
				Balance:  acc.Balance,
				CodeHash: acc.CodeHash,
				Root:     common.BytesToHash(acc.Root),
			}
			if len(data.CodeHash) == 0 {
				data.CodeHash = types.EmptyCodeHash.Bytes()
			}
			if data.Root == (common.Hash{}) {
				data.Root = types.EmptyRootHash
			}
		}
	}
	// If snapshot unavailable or reading from it failed, load from the database
	if data == nil {
		start := time.Now()
		var err error
		data, err = s.trie.GetAccount(addr)
		s.AccountReads += time.Since(start)

		if err != nil {
			s.setError(fmt.Errorf("getDeleteStateObject (%x) error: %w", addr.Bytes(), err))
			return nil
		}
		if data == nil {
			return nil
		}
	}
	// Insert into the live set
	obj := newObject(s, addr, data)
	s.setStateObject(obj)
	return obj
}

func (s *StateDB) setStateObject(object *stateObject) {
	s.stateObjects[object.Address()] = object
}

// getOrNewStateObject retrieves a state object or create a new state object if nil.
func (s *StateDB) getOrNewStateObject(addr common.Address) *stateObject {
	obj := s.getStateObject(addr)
	if obj == nil {
		obj = s.createObject(addr)
	}
	return obj
}

// createObject creates a new state object. The assumption is held there is no
// existing account with the given address, otherwise it will be silently overwritten.
func (s *StateDB) createObject(addr common.Address) *stateObject {
	obj := newObject(s, addr, nil)
	s.journal.append(createObjectChange{account: &addr})
	s.setStateObject(obj)
	return obj
}

// createObject creates a new state object. The assumption is held there is no
// existing account with the given address, otherwise it will be silently overwritten.
func (s *StateDB) createZombie(addr common.Address) *stateObject {
	obj := newObject(s, addr, nil)
	s.journal.append(createZombieChange{account: &addr})
	s.setStateObject(obj)
	return obj
}

// CreateAccount explicitly creates a new state object, assuming that the
// account did not previously exist in the state. If the account already
// exists, this function will silently overwrite it which might lead to a
// consensus bug eventually.
func (s *StateDB) CreateAccount(addr common.Address) {
	s.createObject(addr)
}

// CreateContract is used whenever a contract is created. This may be preceded
// by CreateAccount, but that is not required if it already existed in the
// state due to funds sent beforehand.
// This operation sets the 'newContract'-flag, which is required in order to
// correctly handle EIP-6780 'delete-in-same-transaction' logic.
func (s *StateDB) CreateContract(addr common.Address) {
	obj := s.getStateObject(addr)
	if !obj.newContract {
		obj.newContract = true
		s.journal.append(createContractChange{account: addr})
	}
}

// Copy creates a deep, independent copy of the state.
// Snapshots of the copied state cannot be applied to the copy.
func (s *StateDB) Copy() *StateDB {
	// Copy all the basic fields, initialize the memory ones
	state := &StateDB{
		arbExtraData: &ArbitrumExtraData{
			unexpectedBalanceDelta: new(big.Int).Set(s.arbExtraData.unexpectedBalanceDelta),
			activatedWasms:         make(map[common.Hash]ActivatedWasm, len(s.arbExtraData.activatedWasms)),
			recentWasms:            s.arbExtraData.recentWasms.Copy(),
			openWasmPages:          s.arbExtraData.openWasmPages,
			everWasmPages:          s.arbExtraData.everWasmPages,
			arbTxFilter:            s.arbExtraData.arbTxFilter,
		},

		db:                   s.db,
		trie:                 s.db.CopyTrie(s.trie),
		hasher:               crypto.NewKeccakState(),
		originalRoot:         s.originalRoot,
		accounts:             copySet(s.accounts),
		storages:             copy2DSet(s.storages),
		accountsOrigin:       copySet(s.accountsOrigin),
		storagesOrigin:       copy2DSet(s.storagesOrigin),
		stateObjects:         make(map[common.Address]*stateObject, len(s.stateObjects)),
		stateObjectsDestruct: maps.Clone(s.stateObjectsDestruct),
		mutations:            make(map[common.Address]*mutation, len(s.mutations)),
		dbErr:                s.dbErr,
		refund:               s.refund,
		thash:                s.thash,
		txIndex:              s.txIndex,
		logs:                 make(map[common.Hash][]*types.Log, len(s.logs)),
		logSize:              s.logSize,
		preimages:            maps.Clone(s.preimages),
		journal:              s.journal.copy(),
		validRevisions:       slices.Clone(s.validRevisions),
		nextRevisionId:       s.nextRevisionId,

		// In order for the block producer to be able to use and make additions
		// to the snapshot tree, we need to copy that as well. Otherwise, any
		// block mined by ourselves will cause gaps in the tree, and force the
		// miner to operate trie-backed only.
		snaps: s.snaps,
		snap:  s.snap,
	}
	// Deep copy cached state objects.
	for addr, obj := range s.stateObjects {
		state.stateObjects[addr] = obj.deepCopy(state)
	}
	// Deep copy the object state markers.
	for addr, op := range s.mutations {
		state.mutations[addr] = op.copy()
	}
	// Deep copy the logs occurred in the scope of block
	for hash, logs := range s.logs {
		cpy := make([]*types.Log, len(logs))
		for i, l := range logs {
			cpy[i] = new(types.Log)
			*cpy[i] = *l
		}
		state.logs[hash] = cpy
	}

	// Do we need to copy the access list and transient storage?
	// In practice: No. At the start of a transaction, these two lists are empty.
	// In practice, we only ever copy state _between_ transactions/blocks, never
	// in the middle of a transaction. However, it doesn't cost us much to copy
	// empty lists, so we do it anyway to not blow up if we ever decide copy them
	// in the middle of a transaction.
	state.accessList = s.accessList.Copy()
	state.transientStorage = s.transientStorage.Copy()

	// Arbitrum: copy wasm calls and activated WASMs
	if s.arbExtraData.userWasms != nil {
		state.arbExtraData.userWasms = make(UserWasms, len(s.arbExtraData.userWasms))
		for call, wasm := range s.arbExtraData.userWasms {
			state.arbExtraData.userWasms[call] = wasm
		}
	}
	for moduleHash, asmMap := range s.arbExtraData.activatedWasms {
		// It's fine to skip a deep copy since activations are immutable.
		state.arbExtraData.activatedWasms[moduleHash] = asmMap
	}

	// If there's a prefetcher running, make an inactive copy of it that can
	// only access data but does not actively preload (since the user will not
	// know that they need to explicitly terminate an active copy).
	if s.prefetcher != nil {
		state.prefetcher = s.prefetcher.copy()
	}
	return state
}

// Snapshot returns an identifier for the current revision of the state.
func (s *StateDB) Snapshot() int {
	id := s.nextRevisionId
	s.nextRevisionId++
	s.validRevisions = append(s.validRevisions, revision{id, s.journal.length(), new(big.Int).Set(s.arbExtraData.unexpectedBalanceDelta)})
	return id
}

// RevertToSnapshot reverts all state changes made since the given revision.
func (s *StateDB) RevertToSnapshot(revid int) {
	// Find the snapshot in the stack of valid snapshots.
	idx := sort.Search(len(s.validRevisions), func(i int) bool {
		return s.validRevisions[i].id >= revid
	})
	if idx == len(s.validRevisions) || s.validRevisions[idx].id != revid {
		panic(fmt.Errorf("revision id %v cannot be reverted", revid))
	}
	revision := s.validRevisions[idx]
	snapshot := revision.journalIndex
	s.arbExtraData.unexpectedBalanceDelta = new(big.Int).Set(revision.unexpectedBalanceDelta)

	// Replay the journal to undo changes and remove invalidated snapshots
	s.journal.revert(s, snapshot)
	s.validRevisions = s.validRevisions[:idx]
}

// GetRefund returns the current value of the refund counter.
func (s *StateDB) GetRefund() uint64 {
	return s.refund
}

// Finalise finalises the state by removing the destructed objects and clears
// the journal as well as the refunds. Finalise, however, will not push any updates
// into the tries just yet. Only IntermediateRoot or Commit will do that.
func (s *StateDB) Finalise(deleteEmptyObjects bool) {
	addressesToPrefetch := make([][]byte, 0, len(s.journal.dirties))
	for addr, dirtyCount := range s.journal.dirties {
		isZombie := s.journal.zombieEntries[addr] == dirtyCount
		obj, exist := s.stateObjects[addr]
		if !exist {
			// ripeMD is 'touched' at block 1714175, in tx 0x1237f737031e40bcde4a8b7e717b2d15e3ecadfe49bb1bbc71ee9deb09c6fcf2
			// That tx goes out of gas, and although the notion of 'touched' does not exist there, the
			// touch-event will still be recorded in the journal. Since ripeMD is a special snowflake,
			// it will persist in the journal even though the journal is reverted. In this special circumstance,
			// it may exist in `s.journal.dirties` but not in `s.stateObjects`.
			// Thus, we can safely ignore it here
			continue
		}
		if obj.selfDestructed || (deleteEmptyObjects && obj.empty() && !isZombie) {
			delete(s.stateObjects, obj.address)
			s.markDelete(addr)

			// If ether was sent to account post-selfdestruct it is burnt.
			if bal := obj.Balance(); s.logger != nil && s.logger.OnBalanceChange != nil && obj.selfDestructed && bal.Sign() != 0 {
				s.logger.OnBalanceChange(obj.address, bal.ToBig(), new(big.Int), tracing.BalanceDecreaseSelfdestructBurn)
			}
			// We need to maintain account deletions explicitly (will remain
			// set indefinitely). Note only the first occurred self-destruct
			// event is tracked.
			if _, ok := s.stateObjectsDestruct[obj.address]; !ok {
				s.stateObjectsDestruct[obj.address] = obj.origin
			}
			// Note, we can't do this only at the end of a block because multiple
			// transactions within the same block might self destruct and then
			// resurrect an account; but the snapshotter needs both events.
			delete(s.accounts, obj.addrHash)      // Clear out any previously updated account data (may be recreated via a resurrect)
			delete(s.storages, obj.addrHash)      // Clear out any previously updated storage data (may be recreated via a resurrect)
			delete(s.accountsOrigin, obj.address) // Clear out any previously updated account data (may be recreated via a resurrect)
			delete(s.storagesOrigin, obj.address) // Clear out any previously updated storage data (may be recreated via a resurrect)
		} else {
			obj.finalise(true) // Prefetch slots in the background
			s.markUpdate(addr)
		}
		// At this point, also ship the address off to the precacher. The precacher
		// will start loading tries, and when the change is eventually committed,
		// the commit-phase will be a lot faster
		addressesToPrefetch = append(addressesToPrefetch, common.CopyBytes(addr[:])) // Copy needed for closure
	}
	if s.prefetcher != nil && len(addressesToPrefetch) > 0 {
		s.prefetcher.prefetch(common.Hash{}, s.originalRoot, common.Address{}, addressesToPrefetch)
	}
	// Invalidate journal because reverting across transactions is not allowed.
	s.clearJournalAndRefund()
}

// IntermediateRoot computes the current root hash of the state trie.
// It is called in between transactions to get the root hash that
// goes into transaction receipts.
func (s *StateDB) IntermediateRoot(deleteEmptyObjects bool) common.Hash {
	// Finalise all the dirty storage states and write them into the tries
	s.Finalise(deleteEmptyObjects)

	// If there was a trie prefetcher operating, it gets aborted and irrevocably
	// modified after we start retrieving tries. Remove it from the statedb after
	// this round of use.
	//
	// This is weird pre-byzantium since the first tx runs with a prefetcher and
	// the remainder without, but pre-byzantium even the initial prefetcher is
	// useless, so no sleep lost.
	prefetcher := s.prefetcher
	if s.prefetcher != nil {
		defer func() {
			s.prefetcher.close()
			s.prefetcher = nil
		}()
	}
	// Although naively it makes sense to retrieve the account trie and then do
	// the contract storage and account updates sequentially, that short circuits
	// the account prefetcher. Instead, let's process all the storage updates
	// first, giving the account prefetches just a few more milliseconds of time
	// to pull useful data from disk.
	start := time.Now()
	if s.deterministic {
		addressesToUpdate := make([]common.Address, 0, len(s.mutations))
		for addr := range s.mutations {
			addressesToUpdate = append(addressesToUpdate, addr)
		}
		sort.Slice(addressesToUpdate, func(i, j int) bool { return bytes.Compare(addressesToUpdate[i][:], addressesToUpdate[j][:]) < 0 })
		for _, addr := range addressesToUpdate {
			if obj := s.mutations[addr]; !obj.applied && !obj.isDelete() {
				s.stateObjects[addr].updateRoot()
			}
		}
	} else {
		for addr, op := range s.mutations {
			if op.applied {
				continue
			}
			if op.isDelete() {
				continue
			}
			s.stateObjects[addr].updateRoot()
		}
	}
	s.StorageUpdates += time.Since(start)

	// Now we're about to start to write changes to the trie. The trie is so far
	// _untouched_. We can check with the prefetcher, if it can give us a trie
	// which has the same root, but also has some content loaded into it.
	if prefetcher != nil {
		if trie := prefetcher.trie(common.Hash{}, s.originalRoot); trie != nil {
			s.trie = trie
		}
	}
	// Perform updates before deletions.  This prevents resolution of unnecessary trie nodes
	// in circumstances similar to the following:
	//
	// Consider nodes `A` and `B` who share the same full node parent `P` and have no other siblings.
	// During the execution of a block:
	// - `A` self-destructs,
	// - `C` is created, and also shares the parent `P`.
	// If the self-destruct is handled first, then `P` would be left with only one child, thus collapsed
	// into a shortnode. This requires `B` to be resolved from disk.
	// Whereas if the created node is handled first, then the collapse is avoided, and `B` is not resolved.
	var (
		usedAddrs    [][]byte
		deletedAddrs []common.Address
	)
	for addr, op := range s.mutations {
		if op.applied {
			continue
		}
		op.applied = true

		if op.isDelete() {
			deletedAddrs = append(deletedAddrs, addr)
		} else {
			s.updateStateObject(s.stateObjects[addr])
			s.AccountUpdated += 1
		}
		usedAddrs = append(usedAddrs, common.CopyBytes(addr[:])) // Copy needed for closure
	}
	for _, deletedAddr := range deletedAddrs {
		s.deleteStateObject(deletedAddr)
		s.AccountDeleted += 1
	}
	if prefetcher != nil {
		prefetcher.used(common.Hash{}, s.originalRoot, usedAddrs)
	}
	// Track the amount of time wasted on hashing the account trie
	defer func(start time.Time) { s.AccountHashes += time.Since(start) }(time.Now())

	return s.trie.Hash()
}

// SetTxContext sets the current transaction hash and index which are
// used when the EVM emits new state logs. It should be invoked before
// transaction execution.
func (s *StateDB) SetTxContext(thash common.Hash, ti int) {
	s.thash = thash
	s.txIndex = ti

	// Arbitrum: clear memory charging state for new tx
	s.arbExtraData.openWasmPages = 0
	s.arbExtraData.everWasmPages = 0
}

func (s *StateDB) clearJournalAndRefund() {
	if len(s.journal.entries) > 0 {
		s.journal = newJournal()
		s.refund = 0
	}
	s.validRevisions = s.validRevisions[:0] // Snapshots can be created without journal entries
}

// fastDeleteStorage is the function that efficiently deletes the storage trie
// of a specific account. It leverages the associated state snapshot for fast
// storage iteration and constructs trie node deletion markers by creating
// stack trie with iterated slots.
func (s *StateDB) fastDeleteStorage(addrHash common.Hash, root common.Hash) (common.StorageSize, map[common.Hash][]byte, *trienode.NodeSet, error) {
	iter, err := s.snaps.StorageIterator(s.originalRoot, addrHash, common.Hash{})
	if err != nil {
		return 0, nil, nil, err
	}
	defer iter.Release()

	var (
		size  common.StorageSize
		nodes = trienode.NewNodeSet(addrHash)
		slots = make(map[common.Hash][]byte)
	)
	stack := trie.NewStackTrie(func(path []byte, hash common.Hash, blob []byte) {
		nodes.AddNode(path, trienode.NewDeleted())
		size += common.StorageSize(len(path))
	})
	for iter.Next() {
		slot := common.CopyBytes(iter.Slot())
		if err := iter.Error(); err != nil { // error might occur after Slot function
			return 0, nil, nil, err
		}
		size += common.StorageSize(common.HashLength + len(slot))
		slots[iter.Hash()] = slot

		if err := stack.Update(iter.Hash().Bytes(), slot); err != nil {
			return 0, nil, nil, err
		}
	}
	if err := iter.Error(); err != nil { // error might occur during iteration
		return 0, nil, nil, err
	}
	if stack.Hash() != root {
		return 0, nil, nil, fmt.Errorf("snapshot is not matched, exp %x, got %x", root, stack.Hash())
	}
	return size, slots, nodes, nil
}

// slowDeleteStorage serves as a less-efficient alternative to "fastDeleteStorage,"
// employed when the associated state snapshot is not available. It iterates the
// storage slots along with all internal trie nodes via trie directly.
func (s *StateDB) slowDeleteStorage(addr common.Address, addrHash common.Hash, root common.Hash) (common.StorageSize, map[common.Hash][]byte, *trienode.NodeSet, error) {
	tr, err := s.db.OpenStorageTrie(s.originalRoot, addr, root, s.trie)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("failed to open storage trie, err: %w", err)
	}
	it, err := tr.NodeIterator(nil)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("failed to open storage iterator, err: %w", err)
	}
	var (
		size  common.StorageSize
		nodes = trienode.NewNodeSet(addrHash)
		slots = make(map[common.Hash][]byte)
	)
	for it.Next(true) {
		if it.Leaf() {
			slots[common.BytesToHash(it.LeafKey())] = common.CopyBytes(it.LeafBlob())
			size += common.StorageSize(common.HashLength + len(it.LeafBlob()))
			continue
		}
		if it.Hash() == (common.Hash{}) {
			continue
		}
		size += common.StorageSize(len(it.Path()))
		nodes.AddNode(it.Path(), trienode.NewDeleted())
	}
	if err := it.Error(); err != nil {
		return 0, nil, nil, err
	}
	return size, slots, nodes, nil
}

// deleteStorage is designed to delete the storage trie of a designated account.
// It could potentially be terminated if the storage size is excessively large,
// potentially leading to an out-of-memory panic. The function will make an attempt
// to utilize an efficient strategy if the associated state snapshot is reachable;
// otherwise, it will resort to a less-efficient approach.
func (s *StateDB) deleteStorage(addr common.Address, addrHash common.Hash, root common.Hash) (map[common.Hash][]byte, *trienode.NodeSet, error) {
	var (
		start = time.Now()
		err   error
		size  common.StorageSize
		slots map[common.Hash][]byte
		nodes *trienode.NodeSet
	)
	// The fast approach can be failed if the snapshot is not fully
	// generated, or it's internally corrupted. Fallback to the slow
	// one just in case.
	if s.snap != nil {
		size, slots, nodes, err = s.fastDeleteStorage(addrHash, root)
	}
	if s.snap == nil || err != nil {
		size, slots, nodes, err = s.slowDeleteStorage(addr, addrHash, root)
	}
	if err != nil {
		return nil, nil, err
	}
	// Report the metrics
	n := int64(len(slots))

	slotDeletionMaxCount.UpdateIfGt(int64(len(slots)))
	slotDeletionMaxSize.UpdateIfGt(int64(size))

	slotDeletionTimer.UpdateSince(start)
	slotDeletionCount.Mark(n)
	slotDeletionSize.Mark(int64(size))

	return slots, nodes, nil
}

// handleDestruction processes all destruction markers and deletes the account
// and associated storage slots if necessary. There are four possible situations
// here:
//
//   - the account was not existent and be marked as destructed
//
//   - the account was not existent and be marked as destructed,
//     however, it's resurrected later in the same block.
//
//   - the account was existent and be marked as destructed
//
//   - the account was existent and be marked as destructed,
//     however it's resurrected later in the same block.
//
// In case (a), nothing needs be deleted, nil to nil transition can be ignored.
//
// In case (b), nothing needs be deleted, nil is used as the original value for
// newly created account and storages
//
// In case (c), **original** account along with its storages should be deleted,
// with their values be tracked as original value.
//
// In case (d), **original** account along with its storages should be deleted,
// with their values be tracked as original value.
func (s *StateDB) handleDestruction(nodes *trienode.MergedNodeSet) error {
	// Short circuit if geth is running with hash mode. This procedure can consume
	// considerable time and storage deletion isn't supported in hash mode, thus
	// preemptively avoiding unnecessary expenses.
	if s.db.TrieDB().Scheme() == rawdb.HashScheme {
		return nil
	}
	for addr, prev := range s.stateObjectsDestruct {
		// The original account was non-existing, and it's marked as destructed
		// in the scope of block. It can be case (a) or (b).
		// - for (a), skip it without doing anything.
		// - for (b), track account's original value as nil. It may overwrite
		//   the data cached in s.accountsOrigin set by 'updateStateObject'.
		addrHash := crypto.Keccak256Hash(addr[:])
		if prev == nil {
			if _, ok := s.accounts[addrHash]; ok {
				s.accountsOrigin[addr] = nil // case (b)
			}
			continue
		}
		// It can overwrite the data in s.accountsOrigin set by 'updateStateObject'.
		s.accountsOrigin[addr] = types.SlimAccountRLP(*prev) // case (c) or (d)

		// Short circuit if the storage was empty.
		if prev.Root == types.EmptyRootHash {
			continue
		}
		// Remove storage slots belong to the account.
		slots, set, err := s.deleteStorage(addr, addrHash, prev.Root)
		if err != nil {
			return fmt.Errorf("failed to delete storage, err: %w", err)
		}
		if s.storagesOrigin[addr] == nil {
			s.storagesOrigin[addr] = slots
		} else {
			// It can overwrite the data in s.storagesOrigin[addrHash] set by
			// 'object.updateTrie'.
			for key, val := range slots {
				s.storagesOrigin[addr][key] = val
			}
		}
		if err := nodes.Merge(set); err != nil {
			return err
		}
	}
	return nil
}

// GetTrie returns the account trie.
func (s *StateDB) GetTrie() Trie {
	return s.trie
}

// Commit writes the state to the underlying in-memory trie database.
// Once the state is committed, tries cached in stateDB (including account
// trie, storage tries) will no longer be functional. A new state instance
// must be created with new root and updated database for accessing post-
// commit states.
//
// The associated block number of the state transition is also provided
// for more chain context.
func (s *StateDB) Commit(block uint64, deleteEmptyObjects bool) (common.Hash, error) {
	if s.arbExtraData.arbTxFilter {
		return common.Hash{}, ErrArbTxFilter
	}
	// Short circuit in case any database failure occurred earlier.
	if s.dbErr != nil {
		return common.Hash{}, fmt.Errorf("commit aborted due to earlier error: %v", s.dbErr)
	}
	// Finalize any pending changes and merge everything into the tries
	s.IntermediateRoot(deleteEmptyObjects)

	// Commit objects to the trie, measuring the elapsed time
	var (
		accountTrieNodesUpdated int
		accountTrieNodesDeleted int
		storageTrieNodesUpdated int
		storageTrieNodesDeleted int
		nodes                   = trienode.NewMergedNodeSet()
		wasmCodeWriter          = s.db.WasmStore().NewBatch()
	)
	// Handle all state deletions first
	if err := s.handleDestruction(nodes); err != nil {
		return common.Hash{}, err
	}
	// Handle all state updates afterwards, concurrently to one another to shave
	// off some milliseconds from the commit operation. Also accumulate the code
	// writes to run in parallel with the computations.
	start := time.Now()
	var (
		code    = s.db.DiskDB().NewBatch()
		lock    sync.Mutex
		root    common.Hash
		workers errgroup.Group
	)
	// Schedule the account trie first since that will be the biggest, so give
	// it the most time to crunch.
	//
	// TODO(karalabe): This account trie commit is *very* heavy. 5-6ms at chain
	// heads, which seems excessive given that it doesn't do hashing, it just
	// shuffles some data. For comparison, the *hashing* at chain head is 2-3ms.
	// We need to investigate what's happening as it seems something's wonky.
	// Obviously it's not an end of the world issue, just something the original
	// code didn't anticipate for.
	workers.Go(func() error {
		// Write the account trie changes, measuring the amount of wasted time
		newroot, set, err := s.trie.Commit(true)
		if err != nil {
			return err
		}
		root = newroot

		// Merge the dirty nodes of account trie into global set
		lock.Lock()
		defer lock.Unlock()

		if set != nil {
			if err = nodes.Merge(set); err != nil {
				return err
			}
			accountTrieNodesUpdated, accountTrieNodesDeleted = set.Size()
		}
		s.AccountCommits = time.Since(start)
		return nil
	})
	// Schedule each of the storage tries that need to be updated, so they can
	// run concurrently to one another.
	//
	// TODO(karalabe): Experimentally, the account commit takes approximately the
	// same time as all the storage commits combined, so we could maybe only have
	// 2 threads in total. But that kind of depends on the account commit being
	// more expensive than it should be, so let's fix that and revisit this todo.
	for addr, op := range s.mutations {
		if op.isDelete() {
			continue
		}
		// Write any contract code associated with the state object
		obj := s.stateObjects[addr]
		if obj.code != nil && obj.dirtyCode {
			rawdb.WriteCode(code, common.BytesToHash(obj.CodeHash()), obj.code)
			obj.dirtyCode = false
		}
		// Run the storage updates concurrently to one another
		workers.Go(func() error {
			// Write any storage changes in the state object to its storage trie
			set, err := obj.commit()
			if err != nil {
				return err
			}
			// Merge the dirty nodes of storage trie into global set. It is possible
			// that the account was destructed and then resurrected in the same block.
			// In this case, the node set is shared by both accounts.
			lock.Lock()
			defer lock.Unlock()

			if set != nil {
				if err = nodes.Merge(set); err != nil {
					return err
				}
				updates, deleted := set.Size()
				storageTrieNodesUpdated += updates
				storageTrieNodesDeleted += deleted
			}
			s.StorageCommits = time.Since(start) // overwrite with the longest storage commit runtime
			return nil
		})
	}
	// Schedule the code commits to run concurrently too. This shouldn't really
	// take much since we don't often commit code, but since it's disk access,
	// it's always yolo.
	workers.Go(func() error {
		if code.ValueSize() > 0 {
			if err := code.Write(); err != nil {
				log.Crit("Failed to commit dirty codes", "error", err)
			}
		}
		return nil
	})

	// Arbitrum: write Stylus programs to disk
	for moduleHash, asmMap := range s.arbExtraData.activatedWasms {
		rawdb.WriteActivation(wasmCodeWriter, moduleHash, asmMap)
	}
	if len(s.arbExtraData.activatedWasms) > 0 {
		s.arbExtraData.activatedWasms = make(map[common.Hash]ActivatedWasm)
	}

	workers.Go(func() error {
		if wasmCodeWriter.ValueSize() > 0 {
			if err := wasmCodeWriter.Write(); err != nil {
				log.Crit("Failed to commit dirty stylus codes", "error", err)
			}
		}
		return nil
	})
	// Wait for everything to finish and update the metrics
	if err := workers.Wait(); err != nil {
		return common.Hash{}, err
	}
	accountUpdatedMeter.Mark(int64(s.AccountUpdated))
	storageUpdatedMeter.Mark(int64(s.StorageUpdated))
	accountDeletedMeter.Mark(int64(s.AccountDeleted))
	storageDeletedMeter.Mark(int64(s.StorageDeleted))
	accountTrieUpdatedMeter.Mark(int64(accountTrieNodesUpdated))
	accountTrieDeletedMeter.Mark(int64(accountTrieNodesDeleted))
	storageTriesUpdatedMeter.Mark(int64(storageTrieNodesUpdated))
	storageTriesDeletedMeter.Mark(int64(storageTrieNodesDeleted))
	s.AccountUpdated, s.AccountDeleted = 0, 0
	s.StorageUpdated, s.StorageDeleted = 0, 0

	// If snapshotting is enabled, update the snapshot tree with this new version
	if s.snap != nil {
		start = time.Now()
		// Only update if there's a state transition (skip empty Clique blocks)
		if parent := s.snap.Root(); parent != root {
			if err := s.snaps.Update(root, parent, s.convertAccountSet(s.stateObjectsDestruct), s.accounts, s.storages); err != nil {
				log.Warn("Failed to update snapshot tree", "from", parent, "to", root, "err", err)
			}
			// Keep TriesInMemory diff layers in the memory, persistent layer is 129th.
			// - head layer is paired with HEAD state
			// - head-1 layer is paired with HEAD-1 state
			// - head-127 layer(bottom-most diff layer) is paired with HEAD-127 state
			if err := s.snaps.Cap(root, DefaultTriesInMemory); err != nil {
				log.Warn("Failed to cap snapshot tree", "root", root, "layers", DefaultTriesInMemory, "err", err)
			}
		}
		s.SnapshotCommits += time.Since(start)
		s.snap = nil
	}

	s.arbExtraData.unexpectedBalanceDelta.Set(new(big.Int))

	if root == (common.Hash{}) {
		root = types.EmptyRootHash
	}
	origin := s.originalRoot
	if origin == (common.Hash{}) {
		origin = types.EmptyRootHash
	}
	if root != origin {
		start = time.Now()
		set := triestate.New(s.accountsOrigin, s.storagesOrigin)
		if err := s.db.TrieDB().Update(root, origin, block, nodes, set); err != nil {
			return common.Hash{}, err
		}
		s.originalRoot = root
		s.TrieDBCommits += time.Since(start)

		if s.onCommit != nil {
			s.onCommit(set)
		}
	}
	// Clear all internal flags at the end of commit operation.
	s.accounts = make(map[common.Hash][]byte)
	s.storages = make(map[common.Hash]map[common.Hash][]byte)
	s.accountsOrigin = make(map[common.Address][]byte)
	s.storagesOrigin = make(map[common.Address]map[common.Hash][]byte)
	s.mutations = make(map[common.Address]*mutation)
	s.stateObjectsDestruct = make(map[common.Address]*types.StateAccount)
	return root, nil
}

// Prepare handles the preparatory steps for executing a state transition with.
// This method must be invoked before state transition.
//
// Berlin fork:
// - Add sender to access list (2929)
// - Add destination to access list (2929)
// - Add precompiles to access list (2929)
// - Add the contents of the optional tx access list (2930)
//
// Potential EIPs:
// - Reset access list (Berlin)
// - Add coinbase to access list (EIP-3651)
// - Reset transient storage (EIP-1153)
func (s *StateDB) Prepare(rules params.Rules, sender, coinbase common.Address, dst *common.Address, precompiles []common.Address, list types.AccessList) {
	if rules.IsBerlin {
		// Clear out any leftover from previous executions
		al := newAccessList()
		s.accessList = al

		al.AddAddress(sender)
		if dst != nil {
			al.AddAddress(*dst)
			// If it's a create-tx, the destination will be added inside evm.create
		}
		for _, addr := range precompiles {
			al.AddAddress(addr)
		}
		for _, el := range list {
			al.AddAddress(el.Address)
			for _, key := range el.StorageKeys {
				al.AddSlot(el.Address, key)
			}
		}
		if rules.IsShanghai { // EIP-3651: warm coinbase
			al.AddAddress(coinbase)
		}
	}
	// Reset transient storage at the beginning of transaction execution
	s.transientStorage = newTransientStorage()
}

// AddAddressToAccessList adds the given address to the access list
func (s *StateDB) AddAddressToAccessList(addr common.Address) {
	if s.accessList.AddAddress(addr) {
		s.journal.append(accessListAddAccountChange{&addr})
	}
}

// AddSlotToAccessList adds the given (address, slot)-tuple to the access list
func (s *StateDB) AddSlotToAccessList(addr common.Address, slot common.Hash) {
	addrMod, slotMod := s.accessList.AddSlot(addr, slot)
	if addrMod {
		// In practice, this should not happen, since there is no way to enter the
		// scope of 'address' without having the 'address' become already added
		// to the access list (via call-variant, create, etc).
		// Better safe than sorry, though
		s.journal.append(accessListAddAccountChange{&addr})
	}
	if slotMod {
		s.journal.append(accessListAddSlotChange{
			address: &addr,
			slot:    &slot,
		})
	}
}

// AddressInAccessList returns true if the given address is in the access list.
func (s *StateDB) AddressInAccessList(addr common.Address) bool {
	return s.accessList.ContainsAddress(addr)
}

// SlotInAccessList returns true if the given (address, slot)-tuple is in the access list.
func (s *StateDB) SlotInAccessList(addr common.Address, slot common.Hash) (addressPresent bool, slotPresent bool) {
	return s.accessList.Contains(addr, slot)
}

// convertAccountSet converts a provided account set from address keyed to hash keyed.
func (s *StateDB) convertAccountSet(set map[common.Address]*types.StateAccount) map[common.Hash]struct{} {
	ret := make(map[common.Hash]struct{}, len(set))
	for addr := range set {
		obj, exist := s.stateObjects[addr]
		if !exist {
			ret[crypto.Keccak256Hash(addr[:])] = struct{}{}
		} else {
			ret[obj.addrHash] = struct{}{}
		}
	}
	return ret
}

// copySet returns a deep-copied set.
func copySet[k comparable](set map[k][]byte) map[k][]byte {
	copied := make(map[k][]byte, len(set))
	for key, val := range set {
		copied[key] = common.CopyBytes(val)
	}
	return copied
}

// copy2DSet returns a two-dimensional deep-copied set.
func copy2DSet[k comparable](set map[k]map[common.Hash][]byte) map[k]map[common.Hash][]byte {
	copied := make(map[k]map[common.Hash][]byte, len(set))
	for addr, subset := range set {
		copied[addr] = make(map[common.Hash][]byte, len(subset))
		for key, val := range subset {
			copied[addr][key] = common.CopyBytes(val)
		}
	}
	return copied
}

func (s *StateDB) markDelete(addr common.Address) {
	if _, ok := s.mutations[addr]; !ok {
		s.mutations[addr] = &mutation{}
	}
	s.mutations[addr].applied = false
	s.mutations[addr].typ = deletion
}

func (s *StateDB) markUpdate(addr common.Address) {
	if _, ok := s.mutations[addr]; !ok {
		s.mutations[addr] = &mutation{}
	}
	s.mutations[addr].applied = false
	s.mutations[addr].typ = update
}
