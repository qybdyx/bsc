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

// Package core implements the Ethereum consensus protocol.
package core

import (
	"errors"
	"fmt"
	"io"
	"math/big"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	lru "github.com/hashicorp/golang-lru"
	"golang.org/x/crypto/sha3"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/common/prque"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/monitor"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/state/snapshot"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/internal/syncx"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
)

var (
	headBlockGauge     = metrics.NewRegisteredGauge("chain/head/block", nil)
	headHeaderGauge    = metrics.NewRegisteredGauge("chain/head/header", nil)
	headFastBlockGauge = metrics.NewRegisteredGauge("chain/head/receipt", nil)

	justifiedBlockGauge = metrics.NewRegisteredGauge("chain/head/justified", nil)
	finalizedBlockGauge = metrics.NewRegisteredGauge("chain/head/finalized", nil)

	accountReadTimer   = metrics.NewRegisteredTimer("chain/account/reads", nil)
	accountHashTimer   = metrics.NewRegisteredTimer("chain/account/hashes", nil)
	accountUpdateTimer = metrics.NewRegisteredTimer("chain/account/updates", nil)
	accountCommitTimer = metrics.NewRegisteredTimer("chain/account/commits", nil)

	storageReadTimer   = metrics.NewRegisteredTimer("chain/storage/reads", nil)
	storageHashTimer   = metrics.NewRegisteredTimer("chain/storage/hashes", nil)
	storageUpdateTimer = metrics.NewRegisteredTimer("chain/storage/updates", nil)
	storageCommitTimer = metrics.NewRegisteredTimer("chain/storage/commits", nil)

	snapshotAccountReadTimer = metrics.NewRegisteredTimer("chain/snapshot/account/reads", nil)
	snapshotStorageReadTimer = metrics.NewRegisteredTimer("chain/snapshot/storage/reads", nil)
	snapshotCommitTimer      = metrics.NewRegisteredTimer("chain/snapshot/commits", nil)

	blockInsertTimer     = metrics.NewRegisteredTimer("chain/inserts", nil)
	blockValidationTimer = metrics.NewRegisteredTimer("chain/validation", nil)
	blockExecutionTimer  = metrics.NewRegisteredTimer("chain/execution", nil)
	blockWriteTimer      = metrics.NewRegisteredTimer("chain/write", nil)

	blockReorgMeter         = metrics.NewRegisteredMeter("chain/reorg/executes", nil)
	blockReorgAddMeter      = metrics.NewRegisteredMeter("chain/reorg/add", nil)
	blockReorgDropMeter     = metrics.NewRegisteredMeter("chain/reorg/drop", nil)
	blockReorgInvalidatedTx = metrics.NewRegisteredMeter("chain/reorg/invalidTx", nil)

	errInsertionInterrupted        = errors.New("insertion is interrupted")
	errStateRootVerificationFailed = errors.New("state root verification failed")
	errChainStopped                = errors.New("blockchain is stopped")
)

const (
	bodyCacheLimit         = 256
	blockCacheLimit        = 256
	diffLayerCacheLimit    = 1024
	diffLayerRLPCacheLimit = 256
	receiptsCacheLimit     = 10000
	txLookupCacheLimit     = 1024
	maxBadBlockLimit       = 16
	maxFutureBlocks        = 256
	maxTimeFutureBlocks    = 30
	maxBeyondBlocks        = 2048
	prefetchTxNumber       = 100

	diffLayerFreezerRecheckInterval = 3 * time.Second
	diffLayerPruneRecheckInterval   = 1 * time.Second // The interval to prune unverified diff layers
	maxDiffQueueDist                = 2048            // Maximum allowed distance from the chain head to queue diffLayers
	maxDiffLimit                    = 2048            // Maximum number of unique diff layers a peer may have responded
	maxDiffForkDist                 = 11              // Maximum allowed backward distance from the chain head
	maxDiffLimitForBroadcast        = 128             // Maximum number of unique diff layers a peer may have broadcasted

	rewindBadBlockInterval = 1 * time.Second

	// BlockChainVersion ensures that an incompatible database forces a resync from scratch.
	//
	// Changelog:
	//
	// - Version 4
	//   The following incompatible database changes were added:
	//   * the `BlockNumber`, `TxHash`, `TxIndex`, `BlockHash` and `Index` fields of log are deleted
	//   * the `Bloom` field of receipt is deleted
	//   * the `BlockIndex` and `TxIndex` fields of txlookup are deleted
	// - Version 5
	//  The following incompatible database changes were added:
	//    * the `TxHash`, `GasCost`, and `ContractAddress` fields are no longer stored for a receipt
	//    * the `TxHash`, `GasCost`, and `ContractAddress` fields are computed by looking up the
	//      receipts' corresponding block
	// - Version 6
	//  The following incompatible database changes were added:
	//    * Transaction lookup information stores the corresponding block number instead of block hash
	// - Version 7
	//  The following incompatible database changes were added:
	//    * Use freezer as the ancient database to maintain all ancient data
	// - Version 8
	//  The following incompatible database changes were added:
	//    * New scheme for contract code in order to separate the codes and trie nodes
	BlockChainVersion uint64 = 8
)

// CacheConfig contains the configuration values for the trie caching/pruning
// that's resident in a blockchain.
type CacheConfig struct {
	TrieCleanLimit      int           // Memory allowance (MB) to use for caching trie nodes in memory
	TrieCleanJournal    string        // Disk journal for saving clean cache entries.
	TrieCleanRejournal  time.Duration // Time interval to dump clean cache to disk periodically
	TrieCleanNoPrefetch bool          // Whether to disable heuristic state prefetching for followup blocks
	TrieDirtyLimit      int           // Memory limit (MB) at which to start flushing dirty trie nodes to disk
	TrieDirtyDisabled   bool          // Whether to disable trie write caching and GC altogether (archive node)
	TrieTimeLimit       time.Duration // Time limit after which to flush the current in-memory trie to disk
	SnapshotLimit       int           // Memory allowance (MB) to use for caching snapshot entries in memory
	Preimages           bool          // Whether to store preimage of trie key to the disk
	TriesInMemory       uint64        // How many tries keeps in memory
	NoTries             bool          // Insecure settings. Do not have any tries in databases if enabled.

	SnapshotWait bool // Wait for snapshot construction on startup. TODO(karalabe): This is a dirty hack for testing, nuke it
}

// To avoid cycle import
type PeerIDer interface {
	ID() string
}

// defaultCacheConfig are the default caching values if none are specified by the
// user (also used during testing).
var defaultCacheConfig = &CacheConfig{
	TrieCleanLimit: 256,
	TrieDirtyLimit: 256,
	TrieTimeLimit:  5 * time.Minute,
	SnapshotLimit:  256,
	TriesInMemory:  128,
	SnapshotWait:   true,
}

type BlockChainOption func(*BlockChain) (*BlockChain, error)

// BlockChain represents the canonical chain given a database with a genesis
// block. The Blockchain manages chain imports, reverts, chain reorganisations.
//
// Importing blocks in to the block chain happens according to the set of rules
// defined by the two stage Validator. Processing of blocks is done using the
// Processor which processes the included transaction. The validation of the state
// is done in the second part of the Validator. Failing results in aborting of
// the import.
//
// The BlockChain also helps in returning blocks from **any** chain included
// in the database as well as blocks that represents the canonical chain. It's
// important to note that GetBlock can return any block and does not need to be
// included in the canonical one where as GetBlockByNumber always represents the
// canonical chain.
type BlockChain struct {
	chainConfig *params.ChainConfig // Chain & network configuration
	cacheConfig *CacheConfig        // Cache configuration for pruning

	db         ethdb.Database // Low level persistent database to store final content in
	snaps      *snapshot.Tree // Snapshot tree for fast trie leaf access
	triegc     *prque.Prque   // Priority queue mapping block numbers to tries to gc
	gcproc     time.Duration  // Accumulates canonical block processing for trie dumping
	commitLock sync.Mutex     // CommitLock is used to protect above field from being modified concurrently

	// txLookupLimit is the maximum number of blocks from head whose tx indices
	// are reserved:
	//  * 0:   means no limit and regenerate any missing indexes
	//  * N:   means N block limit [HEAD-N+1, HEAD] and delete extra indexes
	//  * nil: disable tx reindexer/deleter, but still index new blocks
	txLookupLimit uint64
	triesInMemory uint64

	hc                  *HeaderChain
	rmLogsFeed          event.Feed
	chainFeed           event.Feed
	chainSideFeed       event.Feed
	chainHeadFeed       event.Feed
	chainBlockFeed      event.Feed
	logsFeed            event.Feed
	blockProcFeed       event.Feed
	finalizedHeaderFeed event.Feed
	scope               event.SubscriptionScope
	genesisBlock        *types.Block

	// This mutex synchronizes chain write operations.
	// Readers don't need to take it, they can just read the database.
	chainmu *syncx.ClosableMutex

	currentBlock          atomic.Value // Current head of the block chain
	currentFastBlock      atomic.Value // Current head of the fast-sync chain (may be above the block chain!)
	highestVerifiedHeader atomic.Value

	stateCache    state.Database // State database to reuse between imports (contains state cache)
	bodyCache     *lru.Cache     // Cache for the most recent block bodies
	bodyRLPCache  *lru.Cache     // Cache for the most recent block bodies in RLP encoded format
	receiptsCache *lru.Cache     // Cache for the most recent receipts per block
	blockCache    *lru.Cache     // Cache for the most recent entire blocks
	txLookupCache *lru.Cache     // Cache for the most recent transaction lookup data.
	futureBlocks  *lru.Cache     // future blocks are blocks added for later processing
	badBlockCache *lru.Cache     // Cache for the blocks that failed to pass MPT root verification

	// trusted diff layers
	diffLayerCache             *lru.Cache   // Cache for the diffLayers
	diffLayerRLPCache          *lru.Cache   // Cache for the rlp encoded diffLayers
	diffLayerChanCache         *lru.Cache   // Cache for the difflayer channel
	diffQueue                  *prque.Prque // A Priority queue to store recent diff layer
	diffQueueBuffer            chan *types.DiffLayer
	diffLayerFreezerBlockLimit uint64

	// untrusted diff layers
	diffMux               sync.RWMutex
	blockHashToDiffLayers map[common.Hash]map[common.Hash]*types.DiffLayer // map[blockHash] map[DiffHash]Diff
	diffHashToBlockHash   map[common.Hash]common.Hash                      // map[diffHash]blockHash
	diffHashToPeers       map[common.Hash]map[string]struct{}              // map[diffHash]map[pid]
	diffNumToBlockHashes  map[uint64]map[common.Hash]struct{}              // map[number]map[blockHash]
	diffPeersToDiffHashes map[string]map[common.Hash]struct{}              // map[pid]map[diffHash]

	quit          chan struct{}  // blockchain quit channel
	wg            sync.WaitGroup // chain processing wait group for shutting down
	running       int32          // 0 if chain is running, 1 when stopped
	procInterrupt int32          // interrupt signaler for block processing

	engine     consensus.Engine
	prefetcher Prefetcher
	validator  Validator // Block and state validator interface
	processor  Processor // Block transaction processor interface
	forker     *ForkChoice
	vmConfig   vm.Config
	pipeCommit bool

	shouldPreserve  func(*types.Block) bool        // Function used to determine whether should preserve the given block.
	terminateInsert func(common.Hash, uint64) bool // Testing hook used to terminate ancient receipt chain insertion.

	// monitor
	doubleSignMonitor *monitor.DoubleSignMonitor
}

// NewBlockChain returns a fully initialised block chain using information
// available in the database. It initialises the default Ethereum Validator and
// Processor.
func NewBlockChain(db ethdb.Database, cacheConfig *CacheConfig, chainConfig *params.ChainConfig, engine consensus.Engine,
	vmConfig vm.Config, shouldPreserve func(block *types.Header) bool, txLookupLimit *uint64,
	options ...BlockChainOption) (*BlockChain, error) {
	if cacheConfig == nil {
		cacheConfig = defaultCacheConfig
	}
	if cacheConfig.TriesInMemory != 128 {
		log.Warn("TriesInMemory isn't the default value(128), you need specify exact same TriesInMemory when prune data",
			"triesInMemory", cacheConfig.TriesInMemory)
	}
	bodyCache, _ := lru.New(bodyCacheLimit)
	bodyRLPCache, _ := lru.New(bodyCacheLimit)
	receiptsCache, _ := lru.New(receiptsCacheLimit)
	blockCache, _ := lru.New(blockCacheLimit)
	txLookupCache, _ := lru.New(txLookupCacheLimit)
	badBlockCache, _ := lru.New(maxBadBlockLimit)

	futureBlocks, _ := lru.New(maxFutureBlocks)
	diffLayerCache, _ := lru.New(diffLayerCacheLimit)
	diffLayerRLPCache, _ := lru.New(diffLayerRLPCacheLimit)
	diffLayerChanCache, _ := lru.New(diffLayerCacheLimit)

	bc := &BlockChain{
		chainConfig: chainConfig,
		cacheConfig: cacheConfig,
		db:          db,
		triegc:      prque.New(nil),
		stateCache: state.NewDatabaseWithConfigAndCache(db, &trie.Config{
			Cache:     cacheConfig.TrieCleanLimit,
			Journal:   cacheConfig.TrieCleanJournal,
			Preimages: cacheConfig.Preimages,
			NoTries:   cacheConfig.NoTries,
		}),
		triesInMemory:         cacheConfig.TriesInMemory,
		quit:                  make(chan struct{}),
		chainmu:               syncx.NewClosableMutex(),
		bodyCache:             bodyCache,
		bodyRLPCache:          bodyRLPCache,
		receiptsCache:         receiptsCache,
		blockCache:            blockCache,
		badBlockCache:         badBlockCache,
		diffLayerCache:        diffLayerCache,
		diffLayerRLPCache:     diffLayerRLPCache,
		diffLayerChanCache:    diffLayerChanCache,
		txLookupCache:         txLookupCache,
		futureBlocks:          futureBlocks,
		engine:                engine,
		vmConfig:              vmConfig,
		diffQueue:             prque.New(nil),
		diffQueueBuffer:       make(chan *types.DiffLayer),
		blockHashToDiffLayers: make(map[common.Hash]map[common.Hash]*types.DiffLayer),
		diffHashToBlockHash:   make(map[common.Hash]common.Hash),
		diffHashToPeers:       make(map[common.Hash]map[string]struct{}),
		diffNumToBlockHashes:  make(map[uint64]map[common.Hash]struct{}),
		diffPeersToDiffHashes: make(map[string]map[common.Hash]struct{}),
	}

	bc.prefetcher = NewStatePrefetcher(chainConfig, bc, engine)
	bc.forker = NewForkChoice(bc, shouldPreserve)
	bc.validator = NewBlockValidator(chainConfig, bc, engine)
	bc.processor = NewStateProcessor(chainConfig, bc, engine)

	var err error
	bc.hc, err = NewHeaderChain(db, chainConfig, engine, bc.insertStopped)
	if err != nil {
		return nil, err
	}
	bc.genesisBlock = bc.GetBlockByNumber(0)
	if bc.genesisBlock == nil {
		return nil, ErrNoGenesis
	}

	var nilBlock *types.Block
	bc.currentBlock.Store(nilBlock)
	bc.currentFastBlock.Store(nilBlock)

	var nilHeader *types.Header
	bc.highestVerifiedHeader.Store(nilHeader)

	// Initialize the chain with ancient data if it isn't empty.
	var txIndexBlock uint64

	if bc.empty() {
		rawdb.InitDatabaseFromFreezer(bc.db)
		// If ancient database is not empty, reconstruct all missing
		// indices in the background.
		frozen, _ := bc.db.ItemAmountInAncient()
		if frozen > 0 {
			txIndexBlock, _ = bc.db.Ancients()
		}
	}
	if err := bc.loadLastState(); err != nil {
		return nil, err
	}

	// Make sure the state associated with the block is available
	head := bc.CurrentBlock()
	if _, err := state.New(head.Root(), bc.stateCache, bc.snaps); err != nil {
		// Head state is missing, before the state recovery, find out the
		// disk layer point of snapshot(if it's enabled). Make sure the
		// rewound point is lower than disk layer.
		var diskRoot common.Hash
		if bc.cacheConfig.SnapshotLimit > 0 {
			diskRoot = rawdb.ReadSnapshotRoot(bc.db)
		}
		if diskRoot != (common.Hash{}) {
			log.Warn("Head state missing, repairing", "number", head.Number(), "hash", head.Hash(), "snaproot", diskRoot)

			snapDisk, err := bc.setHeadBeyondRoot(head.NumberU64(), diskRoot, true)
			if err != nil {
				return nil, err
			}
			// Chain rewound, persist old snapshot number to indicate recovery procedure
			if snapDisk != 0 {
				rawdb.WriteSnapshotRecoveryNumber(bc.db, snapDisk)
			}
		} else {
			log.Warn("Head state missing, repairing", "number", head.Number(), "hash", head.Hash())
			if _, err := bc.setHeadBeyondRoot(head.NumberU64(), common.Hash{}, true); err != nil {
				return nil, err
			}
		}
	}

	// Ensure that a previous crash in SetHead doesn't leave extra ancients
	if frozen, err := bc.db.ItemAmountInAncient(); err == nil && frozen > 0 {
		frozen, err = bc.db.Ancients()
		if err != nil {
			return nil, err
		}
		var (
			needRewind bool
			low        uint64
		)
		// The head full block may be rolled back to a very low height due to
		// blockchain repair. If the head full block is even lower than the ancient
		// chain, truncate the ancient store.
		fullBlock := bc.CurrentBlock()
		if fullBlock != nil && fullBlock.Hash() != bc.genesisBlock.Hash() && fullBlock.NumberU64() < frozen-1 {
			needRewind = true
			low = fullBlock.NumberU64()
		}
		// In fast sync, it may happen that ancient data has been written to the
		// ancient store, but the LastFastBlock has not been updated, truncate the
		// extra data here.
		fastBlock := bc.CurrentFastBlock()
		if fastBlock != nil && fastBlock.NumberU64() < frozen-1 {
			needRewind = true
			if fastBlock.NumberU64() < low || low == 0 {
				low = fastBlock.NumberU64()
			}
		}
		if needRewind {
			log.Error("Truncating ancient chain", "from", bc.CurrentHeader().Number.Uint64(), "to", low)
			if err := bc.SetHead(low); err != nil {
				return nil, err
			}
		}
	}
	// The first thing the node will do is reconstruct the verification data for
	// the head block (ethash cache or clique voting snapshot). Might as well do
	// it in advance.
	bc.engine.VerifyHeader(bc, bc.CurrentHeader(), true)

	// Check the current state of the block hashes and make sure that we do not have any of the bad blocks in our chain
	for hash := range BadHashes {
		if header := bc.GetHeaderByHash(hash); header != nil {
			// get the canonical block corresponding to the offending header's number
			headerByNumber := bc.GetHeaderByNumber(header.Number.Uint64())
			// make sure the headerByNumber (if present) is in our current canonical chain
			if headerByNumber != nil && headerByNumber.Hash() == header.Hash() {
				log.Error("Found bad hash, rewinding chain", "number", header.Number, "hash", header.ParentHash)
				if err := bc.SetHead(header.Number.Uint64() - 1); err != nil {
					return nil, err
				}
				log.Error("Chain rewind was successful, resuming normal operation")
			}
		}
	}

	// Load any existing snapshot, regenerating it if loading failed
	if bc.cacheConfig.SnapshotLimit > 0 {
		// If the chain was rewound past the snapshot persistent layer (causing
		// a recovery block number to be persisted to disk), check if we're still
		// in recovery mode and in that case, don't invalidate the snapshot on a
		// head mismatch.
		var recover bool

		head := bc.CurrentBlock()
		if layer := rawdb.ReadSnapshotRecoveryNumber(bc.db); layer != nil && *layer > head.NumberU64() {
			log.Warn("Enabling snapshot recovery", "chainhead", head.NumberU64(), "diskbase", *layer)
			recover = true
		}
		bc.snaps, _ = snapshot.New(bc.db, bc.stateCache.TrieDB(), bc.cacheConfig.SnapshotLimit, int(bc.cacheConfig.TriesInMemory), head.Root(), !bc.cacheConfig.SnapshotWait, true, recover, bc.stateCache.NoTries())
	}
	// write safe point block number
	rawdb.WriteSafePointBlockNumber(bc.db, bc.CurrentBlock().NumberU64())
	// do options before start any routine
	for _, option := range options {
		bc, err = option(bc)
		if err != nil {
			return nil, err
		}
	}
	// Start future block processor.
	bc.wg.Add(1)
	go bc.updateFutureBlocks()

	// Start tx indexer/unindexer.
	if txLookupLimit != nil {
		bc.txLookupLimit = *txLookupLimit

		bc.wg.Add(1)
		go bc.maintainTxIndex(txIndexBlock)
	}

	// If periodic cache journal is required, spin it up.
	if bc.cacheConfig.TrieCleanRejournal > 0 {
		if bc.cacheConfig.TrieCleanRejournal < time.Minute {
			log.Warn("Sanitizing invalid trie cache journal time", "provided", bc.cacheConfig.TrieCleanRejournal, "updated", time.Minute)
			bc.cacheConfig.TrieCleanRejournal = time.Minute
		}
		triedb := bc.stateCache.TrieDB()
		bc.wg.Add(1)
		go func() {
			defer bc.wg.Done()
			triedb.SaveCachePeriodically(bc.cacheConfig.TrieCleanJournal, bc.cacheConfig.TrieCleanRejournal, bc.quit)
		}()
	}
	// Need persist and prune diff layer
	if bc.db.DiffStore() != nil {
		bc.wg.Add(1)
		go bc.trustedDiffLayerLoop()
	}
	bc.wg.Add(1)
	go bc.untrustedDiffLayerPruneLoop()
	if bc.pipeCommit {
		// check current block and rewind invalid one
		bc.wg.Add(1)
		go bc.rewindInvalidHeaderBlockLoop()
	}

	if bc.doubleSignMonitor != nil {
		bc.wg.Add(1)
		go bc.startDoubleSignMonitor()

	}

	return bc, nil
}

// GetVMConfig returns the block chain VM config.
func (bc *BlockChain) GetVMConfig() *vm.Config {
	return &bc.vmConfig
}

func (bc *BlockChain) cacheReceipts(hash common.Hash, receipts types.Receipts) {
	// TODO, This is a hot fix for the block hash of logs is `0x0000000000000000000000000000000000000000000000000000000000000000` for system tx
	// Please check details in https://github.com/bnb-chain/bsc/issues/443
	// This is a temporary fix, the official fix should be a hard fork.
	const possibleSystemReceipts = 3 // One slash tx, two reward distribute txs.
	numOfReceipts := len(receipts)
	for i := numOfReceipts - 1; i >= 0 && i >= numOfReceipts-possibleSystemReceipts; i-- {
		for j := 0; j < len(receipts[i].Logs); j++ {
			receipts[i].Logs[j].BlockHash = hash
		}
	}
	bc.receiptsCache.Add(hash, receipts)
}

func (bc *BlockChain) cacheDiffLayer(diffLayer *types.DiffLayer, diffLayerCh chan struct{}) {
	// The difflayer in the system is stored by the map structure,
	// so it will be out of order.
	// It must be sorted first and then cached,
	// otherwise the DiffHash calculated by different nodes will be inconsistent
	sort.SliceStable(diffLayer.Codes, func(i, j int) bool {
		return diffLayer.Codes[i].Hash.Hex() < diffLayer.Codes[j].Hash.Hex()
	})
	sort.SliceStable(diffLayer.Destructs, func(i, j int) bool {
		return diffLayer.Destructs[i].Hex() < (diffLayer.Destructs[j].Hex())
	})
	sort.SliceStable(diffLayer.Accounts, func(i, j int) bool {
		return diffLayer.Accounts[i].Account.Hex() < diffLayer.Accounts[j].Account.Hex()
	})
	sort.SliceStable(diffLayer.Storages, func(i, j int) bool {
		return diffLayer.Storages[i].Account.Hex() < diffLayer.Storages[j].Account.Hex()
	})
	for index := range diffLayer.Storages {
		// Sort keys and vals by key.
		sort.Sort(&diffLayer.Storages[index])
	}

	if bc.diffLayerCache.Len() >= diffLayerCacheLimit {
		bc.diffLayerCache.RemoveOldest()
	}

	bc.diffLayerCache.Add(diffLayer.BlockHash, diffLayer)
	close(diffLayerCh)

	if bc.db.DiffStore() != nil {
		// push to priority queue before persisting
		bc.diffQueueBuffer <- diffLayer
	}
}

func (bc *BlockChain) cacheBlock(hash common.Hash, block *types.Block) {
	bc.blockCache.Add(hash, block)
}

// empty returns an indicator whether the blockchain is empty.
// Note, it's a special case that we connect a non-empty ancient
// database with an empty node, so that we can plugin the ancient
// into node seamlessly.
func (bc *BlockChain) empty() bool {
	genesis := bc.genesisBlock.Hash()
	for _, hash := range []common.Hash{rawdb.ReadHeadBlockHash(bc.db), rawdb.ReadHeadHeaderHash(bc.db), rawdb.ReadHeadFastBlockHash(bc.db)} {
		if hash != genesis {
			return false
		}
	}
	return true
}

// GetJustifiedNumber returns the highest justified blockNumber on the branch including and before `header`.
func (bc *BlockChain) GetJustifiedNumber(header *types.Header) uint64 {
	if p, ok := bc.engine.(consensus.PoSA); ok {
		justifiedBlockNumber, _, err := p.GetJustifiedNumberAndHash(bc, header)
		if err == nil {
			return justifiedBlockNumber
		}
	}
	// return 0 when err!=nil
	// so the input `header` will at a disadvantage during reorg
	return 0
}

// getFinalizedNumber returns the highest finalized number before the specific block.
func (bc *BlockChain) getFinalizedNumber(header *types.Header) uint64 {
	if p, ok := bc.engine.(consensus.PoSA); ok {
		if finalizedHeader := p.GetFinalizedHeader(bc, header); finalizedHeader != nil {
			return finalizedHeader.Number.Uint64()
		}
	}

	return 0
}

// loadLastState loads the last known chain state from the database. This method
// assumes that the chain manager mutex is held.
func (bc *BlockChain) loadLastState() error {
	// Restore the last known head block
	head := rawdb.ReadHeadBlockHash(bc.db)
	if head == (common.Hash{}) {
		// Corrupt or empty database, init from scratch
		log.Warn("Empty database, resetting chain")
		return bc.Reset()
	}
	// Make sure the entire head block is available
	currentBlock := bc.GetBlockByHash(head)
	if currentBlock == nil {
		// Corrupt or empty database, init from scratch
		log.Warn("Head block missing, resetting chain", "hash", head)
		return bc.Reset()
	}

	// Everything seems to be fine, set as the head block
	bc.currentBlock.Store(currentBlock)
	headBlockGauge.Update(int64(currentBlock.NumberU64()))
	justifiedBlockGauge.Update(int64(bc.GetJustifiedNumber(currentBlock.Header())))
	finalizedBlockGauge.Update(int64(bc.getFinalizedNumber(currentBlock.Header())))

	// Restore the last known head header
	currentHeader := currentBlock.Header()
	if head := rawdb.ReadHeadHeaderHash(bc.db); head != (common.Hash{}) {
		if header := bc.GetHeaderByHash(head); header != nil {
			currentHeader = header
		}
	}
	bc.hc.SetCurrentHeader(currentHeader)

	// Restore the last known head fast block
	bc.currentFastBlock.Store(currentBlock)
	headFastBlockGauge.Update(int64(currentBlock.NumberU64()))

	if head := rawdb.ReadHeadFastBlockHash(bc.db); head != (common.Hash{}) {
		if block := bc.GetBlockByHash(head); block != nil {
			bc.currentFastBlock.Store(block)
			headFastBlockGauge.Update(int64(block.NumberU64()))
		}
	}
	// Issue a status log for the user
	currentFastBlock := bc.CurrentFastBlock()

	headerTd := bc.GetTd(currentHeader.Hash(), currentHeader.Number.Uint64())
	blockTd := bc.GetTd(currentBlock.Hash(), currentBlock.NumberU64())
	fastTd := bc.GetTd(currentFastBlock.Hash(), currentFastBlock.NumberU64())

	log.Info("Loaded most recent local header", "number", currentHeader.Number, "hash", currentHeader.Hash(), "td", headerTd, "age", common.PrettyAge(time.Unix(int64(currentHeader.Time), 0)))
	log.Info("Loaded most recent local full block", "number", currentBlock.Number(), "hash", currentBlock.Hash(), "td", blockTd, "age", common.PrettyAge(time.Unix(int64(currentBlock.Time()), 0)))
	log.Info("Loaded most recent local fast block", "number", currentFastBlock.Number(), "hash", currentFastBlock.Hash(), "td", fastTd, "age", common.PrettyAge(time.Unix(int64(currentFastBlock.Time()), 0)))
	if pivot := rawdb.ReadLastPivotNumber(bc.db); pivot != nil {
		log.Info("Loaded last fast-sync pivot marker", "number", *pivot)
	}
	return nil
}

// SetHead rewinds the local chain to a new head. Depending on whether the node
// was fast synced or full synced and in which state, the method will try to
// delete minimal data from disk whilst retaining chain consistency.
func (bc *BlockChain) SetHead(head uint64) error {
	if !bc.chainmu.TryLock() {
		return nil
	}
	defer bc.chainmu.Unlock()
	_, err := bc.setHeadBeyondRoot(head, common.Hash{}, false)
	return err
}

func (bc *BlockChain) tryRewindBadBlocks() {
	if !bc.chainmu.TryLock() {
		return
	}
	defer bc.chainmu.Unlock()
	block := bc.CurrentBlock()
	snaps := bc.snaps
	// Verified and Result is false
	if snaps != nil && snaps.Snapshot(block.Root()) != nil &&
		snaps.Snapshot(block.Root()).Verified() && !snaps.Snapshot(block.Root()).WaitAndGetVerifyRes() {
		// Rewind by one block
		log.Warn("current block verified failed, rewind to its parent", "height", block.NumberU64(), "hash", block.Hash())
		bc.futureBlocks.Remove(block.Hash())
		bc.badBlockCache.Add(block.Hash(), time.Now())
		bc.diffLayerCache.Remove(block.Hash())
		bc.diffLayerRLPCache.Remove(block.Hash())
		bc.reportBlock(block, nil, errStateRootVerificationFailed)
		bc.setHeadBeyondRoot(block.NumberU64()-1, common.Hash{}, false)
	}
}

func (bc *BlockChain) setHeadBeyondRoot(head uint64, root common.Hash, repair bool) (uint64, error) {
	// Track the block number of the requested root hash
	var rootNumber uint64 // (no root == always 0)

	// Retrieve the last pivot block to short circuit rollbacks beyond it and the
	// current freezer limit to start nuking id underflown
	pivot := rawdb.ReadLastPivotNumber(bc.db)
	frozen, _ := bc.db.Ancients()

	updateFn := func(db ethdb.KeyValueWriter, header *types.Header) (uint64, bool) {
		// Rewind the blockchain, ensuring we don't end up with a stateless head
		// block. Note, depth equality is permitted to allow using SetHead as a
		// chain reparation mechanism without deleting any data!
		if currentBlock := bc.CurrentBlock(); currentBlock != nil && header.Number.Uint64() <= currentBlock.NumberU64() {
			newHeadBlock := bc.GetBlock(header.Hash(), header.Number.Uint64())
			lastBlockNum := header.Number.Uint64()
			if newHeadBlock == nil {
				log.Error("Gap in the chain, rewinding to genesis", "number", header.Number, "hash", header.Hash())
				newHeadBlock = bc.genesisBlock
			} else {
				// Block exists, keep rewinding until we find one with state,
				// keeping rewinding until we exceed the optional threshold
				// root hash
				beyondRoot := (root == common.Hash{}) // Flag whether we're beyond the requested root (no root, always true)
				enoughBeyondCount := false
				beyondCount := 0
				for {
					beyondCount++
					// If a root threshold was requested but not yet crossed, check
					if root != (common.Hash{}) && !beyondRoot && newHeadBlock.Root() == root {
						beyondRoot, rootNumber = true, newHeadBlock.NumberU64()
					}

					enoughBeyondCount = beyondCount > maxBeyondBlocks

					if _, err := state.New(newHeadBlock.Root(), bc.stateCache, bc.snaps); err != nil {
						log.Trace("Block state missing, rewinding further", "number", newHeadBlock.NumberU64(), "hash", newHeadBlock.Hash())
						if pivot == nil || newHeadBlock.NumberU64() > *pivot {
							parent := bc.GetBlock(newHeadBlock.ParentHash(), newHeadBlock.NumberU64()-1)
							if parent != nil {
								newHeadBlock = parent
								continue
							}
							log.Error("Missing block in the middle, aiming genesis", "number", newHeadBlock.NumberU64()-1, "hash", newHeadBlock.ParentHash())
							newHeadBlock = bc.genesisBlock
						} else {
							log.Trace("Rewind passed pivot, aiming genesis", "number", newHeadBlock.NumberU64(), "hash", newHeadBlock.Hash(), "pivot", *pivot)
							newHeadBlock = bc.genesisBlock
						}
					}
					if beyondRoot || (enoughBeyondCount && root != common.Hash{}) || newHeadBlock.NumberU64() == 0 {
						if enoughBeyondCount && (root != common.Hash{}) && rootNumber == 0 {
							for {
								lastBlockNum++
								block := bc.GetBlockByNumber(lastBlockNum)
								if block == nil {
									break
								}
								if block.Root() == root {
									rootNumber = block.NumberU64()
									break
								}
							}
						}
						log.Debug("Rewound to block with state", "number", newHeadBlock.NumberU64(), "hash", newHeadBlock.Hash())
						break
					}
					log.Debug("Skipping block with threshold state", "number", newHeadBlock.NumberU64(), "hash", newHeadBlock.Hash(), "root", newHeadBlock.Root())
					newHeadBlock = bc.GetBlock(newHeadBlock.ParentHash(), newHeadBlock.NumberU64()-1) // Keep rewinding
				}
			}
			rawdb.WriteHeadBlockHash(db, newHeadBlock.Hash())

			// Degrade the chain markers if they are explicitly reverted.
			// In theory we should update all in-memory markers in the
			// last step, however the direction of SetHead is from high
			// to low, so it's safe to update in-memory markers directly.
			bc.currentBlock.Store(newHeadBlock)
			headBlockGauge.Update(int64(newHeadBlock.NumberU64()))
			justifiedBlockGauge.Update(int64(bc.GetJustifiedNumber(newHeadBlock.Header())))
			finalizedBlockGauge.Update(int64(bc.getFinalizedNumber(newHeadBlock.Header())))
		}
		// Rewind the fast block in a simpleton way to the target head
		if currentFastBlock := bc.CurrentFastBlock(); currentFastBlock != nil && header.Number.Uint64() < currentFastBlock.NumberU64() {
			newHeadFastBlock := bc.GetBlock(header.Hash(), header.Number.Uint64())
			// If either blocks reached nil, reset to the genesis state
			if newHeadFastBlock == nil {
				newHeadFastBlock = bc.genesisBlock
			}
			rawdb.WriteHeadFastBlockHash(db, newHeadFastBlock.Hash())

			// Degrade the chain markers if they are explicitly reverted.
			// In theory we should update all in-memory markers in the
			// last step, however the direction of SetHead is from high
			// to low, so it's safe the update in-memory markers directly.
			bc.currentFastBlock.Store(newHeadFastBlock)
			headFastBlockGauge.Update(int64(newHeadFastBlock.NumberU64()))
		}
		head := bc.CurrentBlock().NumberU64()

		// If setHead underflown the freezer threshold and the block processing
		// intent afterwards is full block importing, delete the chain segment
		// between the stateful-block and the sethead target.
		var wipe bool
		if head+1 < frozen {
			wipe = pivot == nil || head >= *pivot
		}
		return head, wipe // Only force wipe if full synced
	}
	// Rewind the header chain, deleting all block bodies until then
	delFn := func(db ethdb.KeyValueWriter, hash common.Hash, num uint64) {
		// Ignore the error here since light client won't hit this path
		frozen, _ := bc.db.Ancients()
		if num+1 <= frozen {
			// Truncate all relative data(header, total difficulty, body, receipt
			// and canonical hash) from ancient store.
			if err := bc.db.TruncateAncients(num); err != nil {
				log.Crit("Failed to truncate ancient data", "number", num, "err", err)
			}
			// Remove the hash <-> number mapping from the active store.
			rawdb.DeleteHeaderNumber(db, hash)
		} else {
			// Remove relative body and receipts from the active store.
			// The header, total difficulty and canonical hash will be
			// removed in the hc.SetHead function.
			rawdb.DeleteBody(db, hash, num)
			rawdb.DeleteReceipts(db, hash, num)
		}
		// Todo(rjl493456442) txlookup, bloombits, etc
	}
	// If SetHead was only called as a chain reparation method, try to skip
	// touching the header chain altogether, unless the freezer is broken
	if repair {
		if target, force := updateFn(bc.db, bc.CurrentBlock().Header()); force {
			bc.hc.SetHead(target, updateFn, delFn)
		}
	} else {
		// Rewind the chain to the requested head and keep going backwards until a
		// block with a state is found or fast sync pivot is passed
		log.Warn("Rewinding blockchain", "target", head)
		bc.hc.SetHead(head, updateFn, delFn)
	}
	// Clear out any stale content from the caches
	bc.bodyCache.Purge()
	bc.bodyRLPCache.Purge()
	bc.receiptsCache.Purge()
	bc.blockCache.Purge()
	bc.txLookupCache.Purge()
	bc.futureBlocks.Purge()

	return rootNumber, bc.loadLastState()
}

// SnapSyncCommitHead sets the current head block to the one defined by the hash
// irrelevant what the chain contents were prior.
func (bc *BlockChain) SnapSyncCommitHead(hash common.Hash) error {
	// Make sure that both the block as well at its state trie exists
	block := bc.GetBlockByHash(hash)
	if block == nil {
		return fmt.Errorf("non existent block [%x..]", hash[:4])
	}
	if _, err := trie.NewSecure(block.Root(), bc.stateCache.TrieDB()); err != nil {
		return err
	}

	// If all checks out, manually set the head block.
	if !bc.chainmu.TryLock() {
		return errChainStopped
	}
	bc.currentBlock.Store(block)
	headBlockGauge.Update(int64(block.NumberU64()))
	justifiedBlockGauge.Update(int64(bc.GetJustifiedNumber(block.Header())))
	finalizedBlockGauge.Update(int64(bc.getFinalizedNumber(block.Header())))
	bc.chainmu.Unlock()

	// Destroy any existing state snapshot and regenerate it in the background,
	// also resuming the normal maintenance of any previously paused snapshot.
	if bc.snaps != nil {
		bc.snaps.Rebuild(block.Root())
	}
	log.Info("Committed new head block", "number", block.Number(), "hash", hash)
	return nil
}

// StateAtWithSharedPool returns a new mutable state based on a particular point in time with sharedStorage
func (bc *BlockChain) StateAtWithSharedPool(root common.Hash) (*state.StateDB, error) {
	return state.NewWithSharedPool(root, bc.stateCache, bc.snaps)
}

// Reset purges the entire blockchain, restoring it to its genesis state.
func (bc *BlockChain) Reset() error {
	return bc.ResetWithGenesisBlock(bc.genesisBlock)
}

// ResetWithGenesisBlock purges the entire blockchain, restoring it to the
// specified genesis state.
func (bc *BlockChain) ResetWithGenesisBlock(genesis *types.Block) error {
	// Dump the entire block chain and purge the caches
	if err := bc.SetHead(0); err != nil {
		return err
	}
	if !bc.chainmu.TryLock() {
		return errChainStopped
	}
	defer bc.chainmu.Unlock()

	// Prepare the genesis block and reinitialise the chain
	batch := bc.db.NewBatch()
	rawdb.WriteTd(batch, genesis.Hash(), genesis.NumberU64(), genesis.Difficulty())
	rawdb.WriteBlock(batch, genesis)
	if err := batch.Write(); err != nil {
		log.Crit("Failed to write genesis block", "err", err)
	}
	bc.writeHeadBlock(genesis)

	// Last update all in-memory chain markers
	bc.genesisBlock = genesis
	bc.currentBlock.Store(bc.genesisBlock)
	headBlockGauge.Update(int64(bc.genesisBlock.NumberU64()))
	justifiedBlockGauge.Update(int64(bc.genesisBlock.NumberU64()))
	finalizedBlockGauge.Update(int64(bc.genesisBlock.NumberU64()))
	bc.hc.SetGenesis(bc.genesisBlock.Header())
	bc.hc.SetCurrentHeader(bc.genesisBlock.Header())
	bc.currentFastBlock.Store(bc.genesisBlock)
	headFastBlockGauge.Update(int64(bc.genesisBlock.NumberU64()))
	return nil
}

// Export writes the active chain to the given writer.
func (bc *BlockChain) Export(w io.Writer) error {
	return bc.ExportN(w, uint64(0), bc.CurrentBlock().NumberU64())
}

// ExportN writes a subset of the active chain to the given writer.
func (bc *BlockChain) ExportN(w io.Writer, first uint64, last uint64) error {
	if first > last {
		return fmt.Errorf("export failed: first (%d) is greater than last (%d)", first, last)
	}
	log.Info("Exporting batch of blocks", "count", last-first+1)

	var (
		parentHash common.Hash
		start      = time.Now()
		reported   = time.Now()
	)
	for nr := first; nr <= last; nr++ {
		block := bc.GetBlockByNumber(nr)
		if block == nil {
			return fmt.Errorf("export failed on #%d: not found", nr)
		}
		if nr > first && block.ParentHash() != parentHash {
			return fmt.Errorf("export failed: chain reorg during export")
		}
		parentHash = block.Hash()
		if err := block.EncodeRLP(w); err != nil {
			return err
		}
		if time.Since(reported) >= statsReportLimit {
			log.Info("Exporting blocks", "exported", block.NumberU64()-first, "elapsed", common.PrettyDuration(time.Since(start)))
			reported = time.Now()
		}
	}
	return nil
}

// writeHeadBlock injects a new head block into the current block chain. This method
// assumes that the block is indeed a true head. It will also reset the head
// header and the head fast sync block to this very same block if they are older
// or if they are on a different side chain.
//
// Note, this function assumes that the `mu` mutex is held!
func (bc *BlockChain) writeHeadBlock(block *types.Block) {
	// Add the block to the canonical chain number scheme and mark as the head
	batch := bc.db.NewBatch()
	rawdb.WriteHeadHeaderHash(batch, block.Hash())
	rawdb.WriteHeadFastBlockHash(batch, block.Hash())
	rawdb.WriteCanonicalHash(batch, block.Hash(), block.NumberU64())
	rawdb.WriteTxLookupEntriesByBlock(batch, block)
	rawdb.WriteHeadBlockHash(batch, block.Hash())

	// Flush the whole batch into the disk, exit the node if failed
	if err := batch.Write(); err != nil {
		log.Crit("Failed to update chain indexes and markers", "err", err)
	}
	// Update all in-memory chain markers in the last step
	bc.hc.SetCurrentHeader(block.Header())

	bc.currentFastBlock.Store(block)
	headFastBlockGauge.Update(int64(block.NumberU64()))

	bc.currentBlock.Store(block)
	headBlockGauge.Update(int64(block.NumberU64()))
	justifiedBlockGauge.Update(int64(bc.GetJustifiedNumber(block.Header())))
	finalizedBlockGauge.Update(int64(bc.getFinalizedNumber(block.Header())))
}

// GetDiffLayerRLP retrieves a diff layer in RLP encoding from the cache or database by blockHash
func (bc *BlockChain) GetDiffLayerRLP(blockHash common.Hash) rlp.RawValue {
	// Short circuit if the diffLayer's already in the cache, retrieve otherwise
	if cached, ok := bc.diffLayerRLPCache.Get(blockHash); ok {
		return cached.(rlp.RawValue)
	}
	if cached, ok := bc.diffLayerCache.Get(blockHash); ok {
		diff := cached.(*types.DiffLayer)
		bz, err := rlp.EncodeToBytes(diff)
		if err != nil {
			return nil
		}
		bc.diffLayerRLPCache.Add(blockHash, rlp.RawValue(bz))
		return bz
	}

	// fallback to untrusted sources.
	diff := bc.GetUnTrustedDiffLayer(blockHash, "")
	if diff != nil {
		bz, err := rlp.EncodeToBytes(diff)
		if err != nil {
			return nil
		}
		// No need to cache untrusted data
		return bz
	}

	// fallback to disk
	diffStore := bc.db.DiffStore()
	if diffStore == nil {
		return nil
	}
	rawData := rawdb.ReadDiffLayerRLP(diffStore, blockHash)
	if len(rawData) != 0 {
		bc.diffLayerRLPCache.Add(blockHash, rawData)
	}
	return rawData
}

func (bc *BlockChain) GetDiffAccounts(blockHash common.Hash) ([]common.Address, error) {
	var (
		accounts  []common.Address
		diffLayer *types.DiffLayer
	)

	header := bc.GetHeaderByHash(blockHash)
	if header == nil {
		return nil, fmt.Errorf("no block found")
	}

	if cached, ok := bc.diffLayerCache.Get(blockHash); ok {
		diffLayer = cached.(*types.DiffLayer)
	} else if diffStore := bc.db.DiffStore(); diffStore != nil {
		diffLayer = rawdb.ReadDiffLayer(diffStore, blockHash)
	}

	if diffLayer == nil {
		if header.TxHash != types.EmptyRootHash {
			return nil, ErrDiffLayerNotFound
		}

		return nil, nil
	}

	for _, diffAccounts := range diffLayer.Accounts {
		accounts = append(accounts, diffAccounts.Account)
	}

	if header.TxHash != types.EmptyRootHash && len(accounts) == 0 {
		return nil, fmt.Errorf("no diff account in block, maybe bad diff layer")
	}

	return accounts, nil
}

// Stop stops the blockchain service. If any imports are currently in progress
// it will abort them using the procInterrupt.
func (bc *BlockChain) Stop() {
	if !atomic.CompareAndSwapInt32(&bc.running, 0, 1) {
		return
	}

	// Unsubscribe all subscriptions registered from blockchain.
	bc.scope.Close()

	// Signal shutdown to all goroutines.
	close(bc.quit)
	bc.StopInsert()

	// Now wait for all chain modifications to end and persistent goroutines to exit.
	//
	// Note: Close waits for the mutex to become available, i.e. any running chain
	// modification will have exited when Close returns. Since we also called StopInsert,
	// the mutex should become available quickly. It cannot be taken again after Close has
	// returned.
	bc.chainmu.Close()
	bc.wg.Wait()

	// Ensure that the entirety of the state snapshot is journalled to disk.
	var snapBase common.Hash
	if bc.snaps != nil {
		var err error
		if snapBase, err = bc.snaps.Journal(bc.CurrentBlock().Root()); err != nil {
			log.Error("Failed to journal state snapshot", "err", err)
		}
	}

	// Ensure the state of a recent block is also stored to disk before exiting.
	// We're writing three different states to catch different restart scenarios:
	//  - HEAD:     So we don't need to reprocess any blocks in the general case
	//  - HEAD-1:   So we don't do large reorgs if our HEAD becomes an uncle
	//  - HEAD-127: So we have a hard limit on the number of blocks reexecuted
	if !bc.cacheConfig.TrieDirtyDisabled {
		triedb := bc.stateCache.TrieDB()

		for _, offset := range []uint64{0, 1, bc.triesInMemory - 1} {
			if number := bc.CurrentBlock().NumberU64(); number > offset {
				recent := bc.GetBlockByNumber(number - offset)

				log.Info("Writing cached state to disk", "block", recent.Number(), "hash", recent.Hash(), "root", recent.Root())
				if err := triedb.Commit(recent.Root(), true, nil); err != nil {
					log.Error("Failed to commit recent state trie", "err", err)
				} else {
					rawdb.WriteSafePointBlockNumber(bc.db, recent.NumberU64())
				}
			}
		}
		if snapBase != (common.Hash{}) {
			log.Info("Writing snapshot state to disk", "root", snapBase)
			if err := triedb.Commit(snapBase, true, nil); err != nil {
				log.Error("Failed to commit recent state trie", "err", err)
			} else {
				rawdb.WriteSafePointBlockNumber(bc.db, bc.CurrentBlock().NumberU64())
			}
		}
		for !bc.triegc.Empty() {
			go triedb.Dereference(bc.triegc.PopItem().(common.Hash))
		}
		if size, _ := triedb.Size(); size != 0 {
			log.Error("Dangling trie nodes after full cleanup")
		}
	}
	// Ensure all live cached entries be saved into disk, so that we can skip
	// cache warmup when node restarts.
	if bc.cacheConfig.TrieCleanJournal != "" {
		triedb := bc.stateCache.TrieDB()
		triedb.SaveCache(bc.cacheConfig.TrieCleanJournal)
	}
	log.Info("Blockchain stopped")
}

// StopInsert interrupts all insertion methods, causing them to return
// errInsertionInterrupted as soon as possible. Insertion is permanently disabled after
// calling this method.
func (bc *BlockChain) StopInsert() {
	atomic.StoreInt32(&bc.procInterrupt, 1)
}

// insertStopped returns true after StopInsert has been called.
func (bc *BlockChain) insertStopped() bool {
	return atomic.LoadInt32(&bc.procInterrupt) == 1
}

func (bc *BlockChain) procFutureBlocks() {
	blocks := make([]*types.Block, 0, bc.futureBlocks.Len())
	for _, hash := range bc.futureBlocks.Keys() {
		if block, exist := bc.futureBlocks.Peek(hash); exist {
			blocks = append(blocks, block.(*types.Block))
		}
	}
	if len(blocks) > 0 {
		sort.Slice(blocks, func(i, j int) bool {
			return blocks[i].NumberU64() < blocks[j].NumberU64()
		})
		// Insert one by one as chain insertion needs contiguous ancestry between blocks
		for i := range blocks {
			bc.InsertChain(blocks[i : i+1])
		}
	}
}

// WriteStatus status of write
type WriteStatus byte

const (
	NonStatTy WriteStatus = iota
	CanonStatTy
	SideStatTy
)

// InsertReceiptChain attempts to complete an already existing header chain with
// transaction and receipt data.
func (bc *BlockChain) InsertReceiptChain(blockChain types.Blocks, receiptChain []types.Receipts, ancientLimit uint64) (int, error) {
	// We don't require the chainMu here since we want to maximize the
	// concurrency of header insertion and receipt insertion.
	bc.wg.Add(1)
	defer bc.wg.Done()

	var (
		ancientBlocks, liveBlocks     types.Blocks
		ancientReceipts, liveReceipts []types.Receipts
	)
	// Do a sanity check that the provided chain is actually ordered and linked
	for i := 0; i < len(blockChain); i++ {
		if i != 0 {
			if blockChain[i].NumberU64() != blockChain[i-1].NumberU64()+1 || blockChain[i].ParentHash() != blockChain[i-1].Hash() {
				log.Error("Non contiguous receipt insert", "number", blockChain[i].Number(), "hash", blockChain[i].Hash(), "parent", blockChain[i].ParentHash(),
					"prevnumber", blockChain[i-1].Number(), "prevhash", blockChain[i-1].Hash())
				return 0, fmt.Errorf("non contiguous insert: item %d is #%d [%x..], item %d is #%d [%x..] (parent [%x..])", i-1, blockChain[i-1].NumberU64(),
					blockChain[i-1].Hash().Bytes()[:4], i, blockChain[i].NumberU64(), blockChain[i].Hash().Bytes()[:4], blockChain[i].ParentHash().Bytes()[:4])
			}
		}
		if blockChain[i].NumberU64() <= ancientLimit {
			ancientBlocks, ancientReceipts = append(ancientBlocks, blockChain[i]), append(ancientReceipts, receiptChain[i])
		} else {
			liveBlocks, liveReceipts = append(liveBlocks, blockChain[i]), append(liveReceipts, receiptChain[i])
		}
	}

	var (
		stats = struct{ processed, ignored int32 }{}
		start = time.Now()
		size  = int64(0)
	)

	// updateHead updates the head fast sync block if the inserted blocks are better
	// and returns an indicator whether the inserted blocks are canonical.
	updateHead := func(head *types.Block) bool {
		if !bc.chainmu.TryLock() {
			return false
		}
		defer bc.chainmu.Unlock()

		// Rewind may have occurred, skip in that case.
		if bc.CurrentHeader().Number.Cmp(head.Number()) >= 0 {
			reorg, err := bc.forker.ReorgNeededWithFastFinality(bc.CurrentFastBlock().Header(), head.Header())
			if err != nil {
				log.Warn("Reorg failed", "err", err)
				return false
			} else if !reorg {
				return false
			}
			rawdb.WriteHeadFastBlockHash(bc.db, head.Hash())
			bc.currentFastBlock.Store(head)
			headFastBlockGauge.Update(int64(head.NumberU64()))
			return true
		}
		return false
	}

	// writeAncient writes blockchain and corresponding receipt chain into ancient store.
	//
	// this function only accepts canonical chain data. All side chain will be reverted
	// eventually.
	writeAncient := func(blockChain types.Blocks, receiptChain []types.Receipts) (int, error) {
		first := blockChain[0]
		last := blockChain[len(blockChain)-1]

		// Ensure genesis is in ancients.
		if first.NumberU64() == 1 {
			if frozen, _ := bc.db.Ancients(); frozen == 0 {
				b := bc.genesisBlock
				td := bc.genesisBlock.Difficulty()
				writeSize, err := rawdb.WriteAncientBlocks(bc.db, []*types.Block{b}, []types.Receipts{nil}, td)
				size += writeSize
				if err != nil {
					log.Error("Error writing genesis to ancients", "err", err)
					return 0, err
				}
				log.Info("Wrote genesis to ancients")
			}
		}
		// Before writing the blocks to the ancients, we need to ensure that
		// they correspond to the what the headerchain 'expects'.
		// We only check the last block/header, since it's a contiguous chain.
		if !bc.HasHeader(last.Hash(), last.NumberU64()) {
			return 0, fmt.Errorf("containing header #%d [%x..] unknown", last.Number(), last.Hash().Bytes()[:4])
		}

		// Write all chain data to ancients.
		td := bc.GetTd(first.Hash(), first.NumberU64())
		writeSize, err := rawdb.WriteAncientBlocks(bc.db, blockChain, receiptChain, td)
		size += writeSize
		if err != nil {
			log.Error("Error importing chain data to ancients", "err", err)
			return 0, err
		}

		// Write tx indices if any condition is satisfied:
		// * If user requires to reserve all tx indices(txlookuplimit=0)
		// * If all ancient tx indices are required to be reserved(txlookuplimit is even higher than ancientlimit)
		// * If block number is large enough to be regarded as a recent block
		// It means blocks below the ancientLimit-txlookupLimit won't be indexed.
		//
		// But if the `TxIndexTail` is not nil, e.g. Geth is initialized with
		// an external ancient database, during the setup, blockchain will start
		// a background routine to re-indexed all indices in [ancients - txlookupLimit, ancients)
		// range. In this case, all tx indices of newly imported blocks should be
		// generated.
		var batch = bc.db.NewBatch()
		for i, block := range blockChain {
			if bc.txLookupLimit == 0 || ancientLimit <= bc.txLookupLimit || block.NumberU64() >= ancientLimit-bc.txLookupLimit {
				rawdb.WriteTxLookupEntriesByBlock(batch, block)
			} else if rawdb.ReadTxIndexTail(bc.db) != nil {
				rawdb.WriteTxLookupEntriesByBlock(batch, block)
			}
			stats.processed++

			if batch.ValueSize() > ethdb.IdealBatchSize || i == len(blockChain)-1 {
				size += int64(batch.ValueSize())
				if err = batch.Write(); err != nil {
					fastBlock := bc.CurrentFastBlock().NumberU64()
					if err := bc.db.TruncateAncients(fastBlock + 1); err != nil {
						log.Error("Can't truncate ancient store after failed insert", "err", err)
					}
					return 0, err
				}
				batch.Reset()
			}
		}

		// Sync the ancient store explicitly to ensure all data has been flushed to disk.
		if err := bc.db.Sync(); err != nil {
			return 0, err
		}
		// Update the current fast block because all block data is now present in DB.
		previousFastBlock := bc.CurrentFastBlock().NumberU64()
		if !updateHead(blockChain[len(blockChain)-1]) {
			// We end up here if the header chain has reorg'ed, and the blocks/receipts
			// don't match the canonical chain.
			if err := bc.db.TruncateAncients(previousFastBlock + 1); err != nil {
				log.Error("Can't truncate ancient store after failed insert", "err", err)
			}
			return 0, errSideChainReceipts
		}

		// Delete block data from the main database.
		batch.Reset()
		canonHashes := make(map[common.Hash]struct{})
		for _, block := range blockChain {
			canonHashes[block.Hash()] = struct{}{}
			if block.NumberU64() == 0 {
				continue
			}
			rawdb.DeleteCanonicalHash(batch, block.NumberU64())
			rawdb.DeleteBlockWithoutNumber(batch, block.Hash(), block.NumberU64())
		}
		// Delete side chain hash-to-number mappings.
		for _, nh := range rawdb.ReadAllHashesInRange(bc.db, first.NumberU64(), last.NumberU64()) {
			if _, canon := canonHashes[nh.Hash]; !canon {
				rawdb.DeleteHeader(batch, nh.Hash, nh.Number)
			}
		}
		if err := batch.Write(); err != nil {
			return 0, err
		}
		return 0, nil
	}

	// writeLive writes blockchain and corresponding receipt chain into active store.
	writeLive := func(blockChain types.Blocks, receiptChain []types.Receipts) (int, error) {
		skipPresenceCheck := false
		batch := bc.db.NewBatch()
		for i, block := range blockChain {
			// Short circuit insertion if shutting down or processing failed
			if bc.insertStopped() {
				return 0, errInsertionInterrupted
			}
			// Short circuit if the owner header is unknown
			if !bc.HasHeader(block.Hash(), block.NumberU64()) {
				return i, fmt.Errorf("containing header #%d [%x..] unknown", block.Number(), block.Hash().Bytes()[:4])
			}
			if !skipPresenceCheck {
				// Ignore if the entire data is already known
				if bc.HasBlock(block.Hash(), block.NumberU64()) {
					stats.ignored++
					continue
				} else {
					// If block N is not present, neither are the later blocks.
					// This should be true, but if we are mistaken, the shortcut
					// here will only cause overwriting of some existing data
					skipPresenceCheck = true
				}
			}
			// Write all the data out into the database
			rawdb.WriteBody(batch, block.Hash(), block.NumberU64(), block.Body())
			rawdb.WriteReceipts(batch, block.Hash(), block.NumberU64(), receiptChain[i])
			rawdb.WriteTxLookupEntriesByBlock(batch, block) // Always write tx indices for live blocks, we assume they are needed

			// Write everything belongs to the blocks into the database. So that
			// we can ensure all components of body is completed(body, receipts,
			// tx indexes)
			if batch.ValueSize() >= ethdb.IdealBatchSize {
				if err := batch.Write(); err != nil {
					return 0, err
				}
				size += int64(batch.ValueSize())
				batch.Reset()
			}
			stats.processed++
		}
		// Write everything belongs to the blocks into the database. So that
		// we can ensure all components of body is completed(body, receipts,
		// tx indexes)
		if batch.ValueSize() > 0 {
			size += int64(batch.ValueSize())
			if err := batch.Write(); err != nil {
				return 0, err
			}
		}
		updateHead(blockChain[len(blockChain)-1])
		return 0, nil
	}

	// Write downloaded chain data and corresponding receipt chain data
	if len(ancientBlocks) > 0 {
		if n, err := writeAncient(ancientBlocks, ancientReceipts); err != nil {
			if err == errInsertionInterrupted {
				return 0, nil
			}
			return n, err
		}
	}
	// Write the tx index tail (block number from where we index) before write any live blocks
	if len(liveBlocks) > 0 && liveBlocks[0].NumberU64() == ancientLimit+1 {
		// The tx index tail can only be one of the following two options:
		// * 0: all ancient blocks have been indexed
		// * ancient-limit: the indices of blocks before ancient-limit are ignored
		if tail := rawdb.ReadTxIndexTail(bc.db); tail == nil {
			if bc.txLookupLimit == 0 || ancientLimit <= bc.txLookupLimit {
				rawdb.WriteTxIndexTail(bc.db, 0)
			} else {
				rawdb.WriteTxIndexTail(bc.db, ancientLimit-bc.txLookupLimit)
			}
		}
	}
	if len(liveBlocks) > 0 {
		if n, err := writeLive(liveBlocks, liveReceipts); err != nil {
			if err == errInsertionInterrupted {
				return 0, nil
			}
			return n, err
		}
	}

	head := blockChain[len(blockChain)-1]
	context := []interface{}{
		"count", stats.processed, "elapsed", common.PrettyDuration(time.Since(start)),
		"number", head.Number(), "hash", head.Hash(), "age", common.PrettyAge(time.Unix(int64(head.Time()), 0)),
		"size", common.StorageSize(size),
	}
	if stats.ignored > 0 {
		context = append(context, []interface{}{"ignored", stats.ignored}...)
	}
	log.Info("Imported new block receipts", context...)

	return 0, nil
}

var lastWrite uint64

// writeBlockWithoutState writes only the block and its metadata to the database,
// but does not write any state. This is used to construct competing side forks
// up to the point where they exceed the canonical total difficulty.
func (bc *BlockChain) writeBlockWithoutState(block *types.Block, td *big.Int) (err error) {
	if bc.insertStopped() {
		return errInsertionInterrupted
	}

	batch := bc.db.NewBatch()
	rawdb.WriteTd(batch, block.Hash(), block.NumberU64(), td)
	rawdb.WriteBlock(batch, block)
	if err := batch.Write(); err != nil {
		log.Crit("Failed to write block into disk", "err", err)
	}
	return nil
}

// writeKnownBlock updates the head block flag with a known block
// and introduces chain reorg if necessary.
func (bc *BlockChain) writeKnownBlock(block *types.Block) error {
	current := bc.CurrentBlock()
	if block.ParentHash() != current.Hash() {
		if err := bc.reorg(current, block); err != nil {
			return err
		}
	}
	bc.writeHeadBlock(block)
	return nil
}

// writeBlockWithState writes block, metadata and corresponding state data to the
// database.
func (bc *BlockChain) writeBlockWithState(block *types.Block, receipts []*types.Receipt, logs []*types.Log, state *state.StateDB) error {
	// Calculate the total difficulty of the block
	ptd := bc.GetTd(block.ParentHash(), block.NumberU64()-1)
	if ptd == nil {
		state.StopPrefetcher()
		return consensus.ErrUnknownAncestor
	}
	// Make sure no inconsistent state is leaked during insertion
	externTd := new(big.Int).Add(block.Difficulty(), ptd)

	// Irrelevant of the canonical status, write the block itself to the database.
	//
	// Note all the components of block(td, hash->number map, header, body, receipts)
	// should be written atomically. BlockBatch is used for containing all components.
	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		blockBatch := bc.db.NewBatch()
		rawdb.WriteTd(blockBatch, block.Hash(), block.NumberU64(), externTd)
		rawdb.WriteBlock(blockBatch, block)
		rawdb.WriteReceipts(blockBatch, block.Hash(), block.NumberU64(), receipts)
		rawdb.WritePreimages(blockBatch, state.Preimages())
		if err := blockBatch.Write(); err != nil {
			log.Crit("Failed to write block into disk", "err", err)
		}
		wg.Done()
	}()

	tryCommitTrieDB := func() error {
		bc.commitLock.Lock()
		defer bc.commitLock.Unlock()

		triedb := bc.stateCache.TrieDB()
		// If we're running an archive node, always flush
		if bc.cacheConfig.TrieDirtyDisabled {
			err := triedb.Commit(block.Root(), false, nil)
			if err != nil {
				return err
			}
		} else {
			// Full but not archive node, do proper garbage collection
			triedb.Reference(block.Root(), common.Hash{}) // metadata reference to keep trie alive
			bc.triegc.Push(block.Root(), -int64(block.NumberU64()))

			if current := block.NumberU64(); current > bc.triesInMemory {
				// If we exceeded our memory allowance, flush matured singleton nodes to disk
				var (
					nodes, imgs = triedb.Size()
					limit       = common.StorageSize(bc.cacheConfig.TrieDirtyLimit) * 1024 * 1024
				)
				if nodes > limit || imgs > 4*1024*1024 {
					triedb.Cap(limit - ethdb.IdealBatchSize)
				}
				// Find the next state trie we need to commit
				chosen := current - bc.triesInMemory

				// If we exceeded out time allowance, flush an entire trie to disk
				if bc.gcproc > bc.cacheConfig.TrieTimeLimit {
					canWrite := true
					if posa, ok := bc.engine.(consensus.PoSA); ok {
						if !posa.EnoughDistance(bc, block.Header()) {
							canWrite = false
						}
					}
					if canWrite {
						// If the header is missing (canonical chain behind), we're reorging a low
						// diff sidechain. Suspend committing until this operation is completed.
						header := bc.GetHeaderByNumber(chosen)
						if header == nil {
							log.Warn("Reorg in progress, trie commit postponed", "number", chosen)
						} else {
							// If we're exceeding limits but haven't reached a large enough memory gap,
							// warn the user that the system is becoming unstable.
							if chosen < lastWrite+bc.triesInMemory && bc.gcproc >= 2*bc.cacheConfig.TrieTimeLimit {
								log.Info("State in memory for too long, committing", "time", bc.gcproc, "allowance", bc.cacheConfig.TrieTimeLimit, "optimum", float64(chosen-lastWrite)/float64(bc.triesInMemory))
							}
							// Flush an entire trie and restart the counters
							triedb.Commit(header.Root, true, nil)
							rawdb.WriteSafePointBlockNumber(bc.db, chosen)
							lastWrite = chosen
							bc.gcproc = 0
						}
					}
				}
				// Garbage collect anything below our required write retention
				wg2 := sync.WaitGroup{}
				for !bc.triegc.Empty() {
					root, number := bc.triegc.Pop()
					if uint64(-number) > chosen {
						bc.triegc.Push(root, number)
						break
					}
					wg2.Add(1)
					go func() {
						triedb.Dereference(root.(common.Hash))
						wg2.Done()
					}()
				}
				wg2.Wait()
			}
		}
		return nil
	}
	// Commit all cached state changes into underlying memory database.
	_, diffLayer, err := state.Commit(bc.tryRewindBadBlocks, tryCommitTrieDB)
	if err != nil {
		return err
	}

	// Ensure no empty block body
	if diffLayer != nil && block.Header().TxHash != types.EmptyRootHash {
		// Filling necessary field
		diffLayer.Receipts = receipts
		diffLayer.BlockHash = block.Hash()
		diffLayer.Number = block.NumberU64()

		diffLayerCh := make(chan struct{})
		if bc.diffLayerChanCache.Len() >= diffLayerCacheLimit {
			bc.diffLayerChanCache.RemoveOldest()
		}
		bc.diffLayerChanCache.Add(diffLayer.BlockHash, diffLayerCh)

		go bc.cacheDiffLayer(diffLayer, diffLayerCh)
	}
	wg.Wait()
	return nil
}

// WriteBlockWithState writes the block and all associated state to the database.
func (bc *BlockChain) WriteBlockAndSetHead(block *types.Block, receipts []*types.Receipt, logs []*types.Log, state *state.StateDB, emitHeadEvent bool) (status WriteStatus, err error) {
	if !bc.chainmu.TryLock() {
		return NonStatTy, errChainStopped
	}
	defer bc.chainmu.Unlock()

	return bc.writeBlockAndSetHead(block, receipts, logs, state, emitHeadEvent)
}

// writeBlockAndSetHead writes the block and all associated state to the database,
// and also it applies the given block as the new chain head. This function expects
// the chain mutex to be held.
func (bc *BlockChain) writeBlockAndSetHead(block *types.Block, receipts []*types.Receipt, logs []*types.Log, state *state.StateDB, emitHeadEvent bool) (status WriteStatus, err error) {
	if err := bc.writeBlockWithState(block, receipts, logs, state); err != nil {
		return NonStatTy, err
	}
	currentBlock := bc.CurrentBlock()
	reorg, err := bc.forker.ReorgNeededWithFastFinality(currentBlock.Header(), block.Header())
	if err != nil {
		return NonStatTy, err
	}
	if reorg {
		// Reorganise the chain if the parent is not the head block
		if block.ParentHash() != currentBlock.Hash() {
			if err := bc.reorg(currentBlock, block); err != nil {
				return NonStatTy, err
			}
		}
		status = CanonStatTy
	} else {
		status = SideStatTy
	}
	// Set new head.
	if status == CanonStatTy {
		bc.writeHeadBlock(block)
	}
	bc.futureBlocks.Remove(block.Hash())

	if status == CanonStatTy {
		bc.chainFeed.Send(ChainEvent{Block: block, Hash: block.Hash(), Logs: logs})
		if len(logs) > 0 {
			bc.logsFeed.Send(logs)
		}
		// In theory we should fire a ChainHeadEvent when we inject
		// a canonical block, but sometimes we can insert a batch of
		// canonicial blocks. Avoid firing too many ChainHeadEvents,
		// we will fire an accumulated ChainHeadEvent and disable fire
		// event here.
		if emitHeadEvent {
			bc.chainHeadFeed.Send(ChainHeadEvent{Block: block})
			if posa, ok := bc.Engine().(consensus.PoSA); ok {
				if finalizedHeader := posa.GetFinalizedHeader(bc, block.Header()); finalizedHeader != nil {
					bc.finalizedHeaderFeed.Send(FinalizedHeaderEvent{finalizedHeader})
				}
			}
		}
	} else {
		bc.chainSideFeed.Send(ChainSideEvent{Block: block})
	}
	return status, nil
}

// addFutureBlock checks if the block is within the max allowed window to get
// accepted for future processing, and returns an error if the block is too far
// ahead and was not added.
//
// TODO after the transition, the future block shouldn't be kept. Because
// it's not checked in the Geth side anymore.
func (bc *BlockChain) addFutureBlock(block *types.Block) error {
	max := uint64(time.Now().Unix() + maxTimeFutureBlocks)
	if block.Time() > max {
		return fmt.Errorf("future block timestamp %v > allowed %v", block.Time(), max)
	}
	if block.Difficulty().Cmp(common.Big0) == 0 {
		// Never add PoS blocks into the future queue
		return nil
	}
	bc.futureBlocks.Add(block.Hash(), block)
	return nil
}

// InsertChain attempts to insert the given batch of blocks in to the canonical
// chain or, otherwise, create a fork. If an error is returned it will return
// the index number of the failing block as well an error describing what went
// wrong. After insertion is done, all accumulated events will be fired.
func (bc *BlockChain) InsertChain(chain types.Blocks) (int, error) {
	// Sanity check that we have something meaningful to import
	if len(chain) == 0 {
		return 0, nil
	}
	bc.blockProcFeed.Send(true)
	defer bc.blockProcFeed.Send(false)

	// Do a sanity check that the provided chain is actually ordered and linked.
	for i := 1; i < len(chain); i++ {
		block, prev := chain[i], chain[i-1]
		if block.NumberU64() != prev.NumberU64()+1 || block.ParentHash() != prev.Hash() {
			log.Error("Non contiguous block insert",
				"number", block.Number(),
				"hash", block.Hash(),
				"parent", block.ParentHash(),
				"prevnumber", prev.Number(),
				"prevhash", prev.Hash(),
			)
			return 0, fmt.Errorf("non contiguous insert: item %d is #%d [%x..], item %d is #%d [%x..] (parent [%x..])", i-1, prev.NumberU64(),
				prev.Hash().Bytes()[:4], i, block.NumberU64(), block.Hash().Bytes()[:4], block.ParentHash().Bytes()[:4])
		}
	}
	// Pre-checks passed, start the full block imports
	if !bc.chainmu.TryLock() {
		return 0, errChainStopped
	}
	defer bc.chainmu.Unlock()
	return bc.insertChain(chain, true, true)
}

// insertChain is the internal implementation of InsertChain, which assumes that
// 1) chains are contiguous, and 2) The chain mutex is held.
//
// This method is split out so that import batches that require re-injecting
// historical blocks can do so without releasing the lock, which could lead to
// racey behaviour. If a sidechain import is in progress, and the historic state
// is imported, but then new canon-head is added before the actual sidechain
// completes, then the historic state could be pruned again
func (bc *BlockChain) insertChain(chain types.Blocks, verifySeals, setHead bool) (int, error) {
	// If the chain is terminating, don't even bother starting up.
	if bc.insertStopped() {
		return 0, nil
	}

	// Start a parallel signature recovery (signer will fluke on fork transition, minimal perf loss)
	signer := types.MakeSigner(bc.chainConfig, chain[0].Number())
	go senderCacher.recoverFromBlocks(signer, chain)

	var (
		stats     = insertStats{startTime: mclock.Now()}
		lastCanon *types.Block
	)
	// Fire a single chain head event if we've progressed the chain
	defer func() {
		if lastCanon != nil && bc.CurrentBlock().Hash() == lastCanon.Hash() {
			bc.chainHeadFeed.Send(ChainHeadEvent{lastCanon})
			if posa, ok := bc.Engine().(consensus.PoSA); ok {
				if finalizedHeader := posa.GetFinalizedHeader(bc, lastCanon.Header()); finalizedHeader != nil {
					bc.finalizedHeaderFeed.Send(FinalizedHeaderEvent{finalizedHeader})
				}
			}
		}
	}()
	// Start the parallel header verifier
	headers := make([]*types.Header, len(chain))
	seals := make([]bool, len(chain))

	for i, block := range chain {
		headers[i] = block.Header()
		seals[i] = verifySeals
	}
	abort, results := bc.engine.VerifyHeaders(bc, headers, seals)
	defer close(abort)

	// Peek the error for the first block to decide the directing import logic
	it := newInsertIterator(chain, results, bc.validator)
	block, err := it.next()

	// Left-trim all the known blocks that don't need to build snapshot
	if bc.skipBlock(err, it) {
		// First block (and state) is known
		//   1. We did a roll-back, and should now do a re-import
		//   2. The block is stored as a sidechain, and is lying about it's stateroot, and passes a stateroot
		//      from the canonical chain, which has not been verified.
		// Skip all known blocks that are behind us.
		var (
			reorg   bool
			current = bc.CurrentBlock()
		)
		for block != nil && bc.skipBlock(err, it) {
			reorg, err = bc.forker.ReorgNeededWithFastFinality(current.Header(), block.Header())
			if err != nil {
				return it.index, err
			}
			if reorg {
				// Switch to import mode if the forker says the reorg is necessary
				// and also the block is not on the canonical chain.
				// In eth2 the forker always returns true for reorg decision (blindly trusting
				// the external consensus engine), but in order to prevent the unnecessary
				// reorgs when importing known blocks, the special case is handled here.
				if block.NumberU64() > current.NumberU64() || bc.GetCanonicalHash(block.NumberU64()) != block.Hash() {
					break
				}
			}
			log.Debug("Ignoring already known block", "number", block.Number(), "hash", block.Hash())
			stats.ignored++

			block, err = it.next()
		}
		// The remaining blocks are still known blocks, the only scenario here is:
		// During the fast sync, the pivot point is already submitted but rollback
		// happens. Then node resets the head full block to a lower height via `rollback`
		// and leaves a few known blocks in the database.
		//
		// When node runs a fast sync again, it can re-import a batch of known blocks via
		// `insertChain` while a part of them have higher total difficulty than current
		// head full block(new pivot point).
		for block != nil && bc.skipBlock(err, it) {
			log.Debug("Writing previously known block", "number", block.Number(), "hash", block.Hash())
			if err := bc.writeKnownBlock(block); err != nil {
				return it.index, err
			}
			lastCanon = block

			block, err = it.next()
		}
		// Falls through to the block import
	}
	switch {
	// First block is pruned
	case errors.Is(err, consensus.ErrPrunedAncestor):
		if setHead {
			// First block is pruned, insert as sidechain and reorg only if TD grows enough
			log.Debug("Pruned ancestor, inserting as sidechain", "number", block.Number(), "hash", block.Hash())
			return bc.insertSideChain(block, it)
		} else {
			// We're post-merge and the parent is pruned, try to recover the parent state
			log.Debug("Pruned ancestor", "number", block.Number(), "hash", block.Hash())
			return it.index, bc.recoverAncestors(block)
		}
	// First block is future, shove it (and all children) to the future queue (unknown ancestor)
	case errors.Is(err, consensus.ErrFutureBlock) || (errors.Is(err, consensus.ErrUnknownAncestor) && bc.futureBlocks.Contains(it.first().ParentHash())):
		for block != nil && (it.index == 0 || errors.Is(err, consensus.ErrUnknownAncestor)) {
			log.Debug("Future block, postponing import", "number", block.Number(), "hash", block.Hash())
			if err := bc.addFutureBlock(block); err != nil {
				return it.index, err
			}
			block, err = it.next()
		}
		stats.queued += it.processed()
		stats.ignored += it.remaining()

		// If there are any still remaining, mark as ignored
		return it.index, err

	// Some other error(except ErrKnownBlock) occurred, abort.
	// ErrKnownBlock is allowed here since some known blocks
	// still need re-execution to generate snapshots that are missing
	case err != nil && !errors.Is(err, ErrKnownBlock):
		bc.futureBlocks.Remove(block.Hash())
		stats.ignored += len(it.chain)
		bc.reportBlock(block, nil, err)
		return it.index, err
	}

	for ; block != nil && err == nil || errors.Is(err, ErrKnownBlock); block, err = it.next() {
		// If the chain is terminating, stop processing blocks
		if bc.insertStopped() {
			log.Debug("Abort during block processing")
			break
		}
		// If the header is a banned one, straight out abort
		if BadHashes[block.Hash()] {
			bc.reportBlock(block, nil, ErrBannedHash)
			return it.index, ErrBannedHash
		}
		// If the block is known (in the middle of the chain), it's a special case for
		// Clique blocks where they can share state among each other, so importing an
		// older block might complete the state of the subsequent one. In this case,
		// just skip the block (we already validated it once fully (and crashed), since
		// its header and body was already in the database). But if the corresponding
		// snapshot layer is missing, forcibly rerun the execution to build it.
		if bc.skipBlock(err, it) {
			logger := log.Debug
			if bc.chainConfig.Clique == nil {
				logger = log.Warn
			}
			logger("Inserted known block", "number", block.Number(), "hash", block.Hash(),
				"uncles", len(block.Uncles()), "txs", len(block.Transactions()), "gas", block.GasUsed(),
				"root", block.Root())

			// Special case. Commit the empty receipt slice if we meet the known
			// block in the middle. It can only happen in the clique chain. Whenever
			// we insert blocks via `insertSideChain`, we only commit `td`, `header`
			// and `body` if it's non-existent. Since we don't have receipts without
			// reexecution, so nothing to commit. But if the sidechain will be adpoted
			// as the canonical chain eventually, it needs to be reexecuted for missing
			// state, but if it's this special case here(skip reexecution) we will lose
			// the empty receipt entry.
			if len(block.Transactions()) == 0 {
				rawdb.WriteReceipts(bc.db, block.Hash(), block.NumberU64(), nil)
			} else {
				log.Error("Please file an issue, skip known block execution without receipt",
					"hash", block.Hash(), "number", block.NumberU64())
			}
			if err := bc.writeKnownBlock(block); err != nil {
				return it.index, err
			}
			stats.processed++

			// We can assume that logs are empty here, since the only way for consecutive
			// Clique blocks to have the same state is if there are no transactions.
			lastCanon = block
			continue
		}

		// Retrieve the parent block and it's state to execute on top
		start := time.Now()
		parent := it.previous()
		if parent == nil {
			parent = bc.GetHeader(block.ParentHash(), block.NumberU64()-1)
		}
		statedb, err := state.NewWithSharedPool(parent.Root, bc.stateCache, bc.snaps)
		if err != nil {
			return it.index, err
		}
		bc.updateHighestVerifiedHeader(block.Header())

		// Enable prefetching to pull in trie node paths while processing transactions
		statedb.StartPrefetcher("chain")
		interruptCh := make(chan struct{})
		// For diff sync, it may fallback to full sync, so we still do prefetch
		if len(block.Transactions()) >= prefetchTxNumber && false {
			// do Prefetch in a separate goroutine to avoid blocking the critical path

			// 1.do state prefetch for snapshot cache
			throwaway := statedb.CopyDoPrefetch()
			go bc.prefetcher.Prefetch(block, throwaway, &bc.vmConfig, interruptCh)

			// 2.do trie prefetch for MPT trie node cache
			// it is for the big state trie tree, prefetch based on transaction's From/To address.
			// trie prefetcher is thread safe now, ok to prefetch in a separate routine
			go throwaway.TriePrefetchInAdvance(block, signer)
		}

		//Process block using the parent state as reference point
		substart := time.Now()
		if bc.pipeCommit {
			statedb.EnablePipeCommit()
		}
		statedb.SetExpectedStateRoot(block.Root())
		statedb, receipts, logs, usedGas, err := bc.processor.Process(block, statedb, bc.vmConfig)
		close(interruptCh) // state prefetch can be stopped
		if err != nil {
			bc.reportBlock(block, receipts, err)
			statedb.StopPrefetcher()
			time.Sleep(30 * time.Second)
			return it.index, err
		}
		// Update the metrics touched during block processing
		accountReadTimer.Update(statedb.AccountReads)                 // Account reads are complete, we can mark them
		storageReadTimer.Update(statedb.StorageReads)                 // Storage reads are complete, we can mark them
		accountUpdateTimer.Update(statedb.AccountUpdates)             // Account updates are complete, we can mark them
		storageUpdateTimer.Update(statedb.StorageUpdates)             // Storage updates are complete, we can mark them
		snapshotAccountReadTimer.Update(statedb.SnapshotAccountReads) // Account reads are complete, we can mark them
		snapshotStorageReadTimer.Update(statedb.SnapshotStorageReads) // Storage reads are complete, we can mark them

		blockExecutionTimer.Update(time.Since(substart))

		// Validate the state using the default validator
		substart = time.Now()
		if !statedb.IsLightProcessed() {
			if err := bc.validator.ValidateState(block, statedb, receipts, usedGas); err != nil {
				log.Error("validate state failed", "error", err)
				bc.reportBlock(block, receipts, err)
				statedb.StopPrefetcher()
				return it.index, err
			}
		}
		// bad block: 33851236
		var stopBlock uint64 = 33851236
		if block.NumberU64() == stopBlock {
			log.Info("stopBlock hit sleep 30s", "block number:", stopBlock)
			time.Sleep(30 * time.Second)
			return it.index, fmt.Errorf("stopBlock for debug")
		}

		bc.cacheReceipts(block.Hash(), receipts)
		bc.cacheBlock(block.Hash(), block)
		proctime := time.Since(start)

		// Update the metrics touched during block validation
		accountHashTimer.Update(statedb.AccountHashes) // Account hashes are complete, we can mark them
		storageHashTimer.Update(statedb.StorageHashes) // Storage hashes are complete, we can mark them

		blockValidationTimer.Update(time.Since(substart))

		// Write the block to the chain and get the status.
		substart = time.Now()
		var status WriteStatus
		if !setHead {
			// Don't set the head, only insert the block
			err = bc.writeBlockWithState(block, receipts, logs, statedb)
		} else {
			status, err = bc.writeBlockAndSetHead(block, receipts, logs, statedb, false)
		}
		if err != nil {
			return it.index, err
		}
		// Update the metrics touched during block commit
		accountCommitTimer.Update(statedb.AccountCommits)   // Account commits are complete, we can mark them
		storageCommitTimer.Update(statedb.StorageCommits)   // Storage commits are complete, we can mark them
		snapshotCommitTimer.Update(statedb.SnapshotCommits) // Snapshot commits are complete, we can mark them

		blockWriteTimer.Update(time.Since(substart))
		blockInsertTimer.UpdateSince(start)

		if !setHead {
			// We did not setHead, so we don't have any stats to update
			log.Info("Inserted block", "number", block.Number(), "hash", block.Hash(), "txs", len(block.Transactions()), "elapsed", common.PrettyDuration(time.Since(start)))
			return it.index, nil
		}

		switch status {
		case CanonStatTy:
			log.Debug("Inserted new block", "number", block.Number(), "hash", block.Hash(),
				"uncles", len(block.Uncles()), "txs", len(block.Transactions()), "gas", block.GasUsed(),
				"elapsed", common.PrettyDuration(time.Since(start)),
				"root", block.Root())

			lastCanon = block

			// Only count canonical blocks for GC processing time
			bc.gcproc += proctime

		case SideStatTy:
			log.Debug("Inserted forked block", "number", block.Number(), "hash", block.Hash(),
				"diff", block.Difficulty(), "elapsed", common.PrettyDuration(time.Since(start)),
				"txs", len(block.Transactions()), "gas", block.GasUsed(), "uncles", len(block.Uncles()),
				"root", block.Root())

		default:
			// This in theory is impossible, but lets be nice to our future selves and leave
			// a log, instead of trying to track down blocks imports that don't emit logs.
			log.Warn("Inserted block with unknown status", "number", block.Number(), "hash", block.Hash(),
				"diff", block.Difficulty(), "elapsed", common.PrettyDuration(time.Since(start)),
				"txs", len(block.Transactions()), "gas", block.GasUsed(), "uncles", len(block.Uncles()),
				"root", block.Root())
		}
		stats.processed++
		stats.usedGas += usedGas

		bc.chainBlockFeed.Send(ChainHeadEvent{block})
		dirty, _ := bc.stateCache.TrieDB().Size()
		stats.report(chain, it.index, dirty)
	}

	// Any blocks remaining here? The only ones we care about are the future ones
	if block != nil && errors.Is(err, consensus.ErrFutureBlock) {
		if err := bc.addFutureBlock(block); err != nil {
			return it.index, err
		}
		block, err = it.next()

		for ; block != nil && errors.Is(err, consensus.ErrUnknownAncestor); block, err = it.next() {
			if err := bc.addFutureBlock(block); err != nil {
				return it.index, err
			}
			stats.queued++
		}
	}
	stats.ignored += it.remaining()

	return it.index, err
}

func (bc *BlockChain) updateHighestVerifiedHeader(header *types.Header) {
	if header == nil || header.Number == nil {
		return
	}
	currentHeader := bc.highestVerifiedHeader.Load().(*types.Header)
	if currentHeader == nil {
		bc.highestVerifiedHeader.Store(types.CopyHeader(header))
		return
	}

	newParentTD := bc.GetTd(header.ParentHash, header.Number.Uint64()-1)
	if newParentTD == nil {
		newParentTD = big.NewInt(0)
	}
	oldParentTD := bc.GetTd(currentHeader.ParentHash, currentHeader.Number.Uint64()-1)
	if oldParentTD == nil {
		oldParentTD = big.NewInt(0)
	}
	newTD := big.NewInt(0).Add(newParentTD, header.Difficulty)
	oldTD := big.NewInt(0).Add(oldParentTD, currentHeader.Difficulty)

	if newTD.Cmp(oldTD) > 0 {
		bc.highestVerifiedHeader.Store(types.CopyHeader(header))
		return
	}
}

func (bc *BlockChain) GetHighestVerifiedHeader() *types.Header {
	return bc.highestVerifiedHeader.Load().(*types.Header)
}

// insertSideChain is called when an import batch hits upon a pruned ancestor
// error, which happens when a sidechain with a sufficiently old fork-block is
// found.
//
// The method writes all (header-and-body-valid) blocks to disk, then tries to
// switch over to the new chain if the TD exceeded the current chain.
// insertSideChain is only used pre-merge.
func (bc *BlockChain) insertSideChain(block *types.Block, it *insertIterator) (int, error) {
	var (
		externTd  *big.Int
		lastBlock = block
		current   = bc.CurrentBlock()
	)
	// The first sidechain block error is already verified to be ErrPrunedAncestor.
	// Since we don't import them here, we expect ErrUnknownAncestor for the remaining
	// ones. Any other errors means that the block is invalid, and should not be written
	// to disk.
	err := consensus.ErrPrunedAncestor
	for ; block != nil && errors.Is(err, consensus.ErrPrunedAncestor); block, err = it.next() {
		// Check the canonical state root for that number
		if number := block.NumberU64(); current.NumberU64() >= number {
			canonical := bc.GetBlockByNumber(number)
			if canonical != nil && canonical.Hash() == block.Hash() {
				// Not a sidechain block, this is a re-import of a canon block which has it's state pruned

				// Collect the TD of the block. Since we know it's a canon one,
				// we can get it directly, and not (like further below) use
				// the parent and then add the block on top
				externTd = bc.GetTd(block.Hash(), block.NumberU64())
				continue
			}
			if canonical != nil && canonical.Root() == block.Root() {
				// This is most likely a shadow-state attack. When a fork is imported into the
				// database, and it eventually reaches a block height which is not pruned, we
				// just found that the state already exist! This means that the sidechain block
				// refers to a state which already exists in our canon chain.
				//
				// If left unchecked, we would now proceed importing the blocks, without actually
				// having verified the state of the previous blocks.
				log.Warn("Sidechain ghost-state attack detected", "number", block.NumberU64(), "sideroot", block.Root(), "canonroot", canonical.Root())

				// If someone legitimately side-mines blocks, they would still be imported as usual. However,
				// we cannot risk writing unverified blocks to disk when they obviously target the pruning
				// mechanism.
				return it.index, errors.New("sidechain ghost-state attack")
			}
		}
		if externTd == nil {
			externTd = bc.GetTd(block.ParentHash(), block.NumberU64()-1)
		}
		externTd = new(big.Int).Add(externTd, block.Difficulty())

		if !bc.HasBlock(block.Hash(), block.NumberU64()) {
			start := time.Now()
			if err := bc.writeBlockWithoutState(block, externTd); err != nil {
				return it.index, err
			}
			log.Debug("Injected sidechain block", "number", block.Number(), "hash", block.Hash(),
				"diff", block.Difficulty(), "elapsed", common.PrettyDuration(time.Since(start)),
				"txs", len(block.Transactions()), "gas", block.GasUsed(), "uncles", len(block.Uncles()),
				"root", block.Root())
		}
		lastBlock = block
	}
	// At this point, we've written all sidechain blocks to database. Loop ended
	// either on some other error or all were processed. If there was some other
	// error, we can ignore the rest of those blocks.
	//
	// If the externTd was larger than our local TD, we now need to reimport the previous
	// blocks to regenerate the required state
	reorg, err := bc.forker.ReorgNeededWithFastFinality(current.Header(), lastBlock.Header())
	if err != nil {
		return it.index, err
	}
	if !reorg {
		localTd := bc.GetTd(current.Hash(), current.NumberU64())
		log.Info("Sidechain written to disk", "start", it.first().NumberU64(), "end", it.previous().Number, "sidetd", externTd, "localtd", localTd)
		return it.index, err
	}
	// Gather all the sidechain hashes (full blocks may be memory heavy)
	var (
		hashes  []common.Hash
		numbers []uint64
	)
	parent := it.previous()
	for parent != nil && !bc.HasState(parent.Root) {
		hashes = append(hashes, parent.Hash())
		numbers = append(numbers, parent.Number.Uint64())

		parent = bc.GetHeader(parent.ParentHash, parent.Number.Uint64()-1)
	}
	if parent == nil {
		return it.index, errors.New("missing parent")
	}
	// Import all the pruned blocks to make the state available
	var (
		blocks []*types.Block
		memory common.StorageSize
	)
	for i := len(hashes) - 1; i >= 0; i-- {
		// Append the next block to our batch
		block := bc.GetBlock(hashes[i], numbers[i])
		if block == nil {
			log.Crit("Importing heavy sidechain block is nil", "hash", hashes[i], "number", numbers[i])
		}

		blocks = append(blocks, block)
		memory += block.Size()

		// If memory use grew too large, import and continue. Sadly we need to discard
		// all raised events and logs from notifications since we're too heavy on the
		// memory here.
		if len(blocks) >= 2048 || memory > 64*1024*1024 {
			log.Info("Importing heavy sidechain segment", "blocks", len(blocks), "start", blocks[0].NumberU64(), "end", block.NumberU64())
			if _, err := bc.insertChain(blocks, false, true); err != nil {
				return 0, err
			}
			blocks, memory = blocks[:0], 0

			// If the chain is terminating, stop processing blocks
			if bc.insertStopped() {
				log.Debug("Abort during blocks processing")
				return 0, nil
			}
		}
	}
	if len(blocks) > 0 {
		log.Info("Importing sidechain segment", "start", blocks[0].NumberU64(), "end", blocks[len(blocks)-1].NumberU64())
		return bc.insertChain(blocks, false, true)
	}
	return 0, nil
}

// recoverAncestors finds the closest ancestor with available state and re-execute
// all the ancestor blocks since that.
// recoverAncestors is only used post-merge.
func (bc *BlockChain) recoverAncestors(block *types.Block) error {
	// Gather all the sidechain hashes (full blocks may be memory heavy)
	var (
		hashes  []common.Hash
		numbers []uint64
		parent  = block
	)
	for parent != nil && !bc.HasState(parent.Root()) {
		hashes = append(hashes, parent.Hash())
		numbers = append(numbers, parent.NumberU64())
		parent = bc.GetBlock(parent.ParentHash(), parent.NumberU64()-1)

		// If the chain is terminating, stop iteration
		if bc.insertStopped() {
			log.Debug("Abort during blocks iteration")
			return errInsertionInterrupted
		}
	}
	if parent == nil {
		return errors.New("missing parent")
	}
	// Import all the pruned blocks to make the state available
	for i := len(hashes) - 1; i >= 0; i-- {
		// If the chain is terminating, stop processing blocks
		if bc.insertStopped() {
			log.Debug("Abort during blocks processing")
			return errInsertionInterrupted
		}
		var b *types.Block
		if i == 0 {
			b = block
		} else {
			b = bc.GetBlock(hashes[i], numbers[i])
		}
		if _, err := bc.insertChain(types.Blocks{b}, false, false); err != nil {
			return err
		}
	}
	return nil
}

// collectLogs collects the logs that were generated or removed during
// the processing of the block that corresponds with the given hash.
// These logs are later announced as deleted or reborn.
func (bc *BlockChain) collectLogs(hash common.Hash, removed bool) []*types.Log {
	number := bc.hc.GetBlockNumber(hash)
	if number == nil {
		return nil
	}
	receipts := rawdb.ReadReceipts(bc.db, hash, *number, bc.chainConfig)

	var logs []*types.Log
	for _, receipt := range receipts {
		for _, log := range receipt.Logs {
			l := *log
			if removed {
				l.Removed = true
			}
			logs = append(logs, &l)
		}
	}
	return logs
}

// mergeLogs returns a merged log slice with specified sort order.
func mergeLogs(logs [][]*types.Log, reverse bool) []*types.Log {
	var ret []*types.Log
	if reverse {
		for i := len(logs) - 1; i >= 0; i-- {
			ret = append(ret, logs[i]...)
		}
	} else {
		for i := 0; i < len(logs); i++ {
			ret = append(ret, logs[i]...)
		}
	}
	return ret
}

// reorg takes two blocks, an old chain and a new chain and will reconstruct the
// blocks and inserts them to be part of the new canonical chain and accumulates
// potential missing transactions and post an event about them.
// Note the new head block won't be processed here, callers need to handle it
// externally.
func (bc *BlockChain) reorg(oldBlock, newBlock *types.Block) error {
	var (
		newChain    types.Blocks
		oldChain    types.Blocks
		commonBlock *types.Block

		deletedTxs []common.Hash
		addedTxs   []common.Hash

		deletedLogs [][]*types.Log
		rebirthLogs [][]*types.Log
	)
	// Reduce the longer chain to the same number as the shorter one
	if oldBlock.NumberU64() > newBlock.NumberU64() {
		// Old chain is longer, gather all transactions and logs as deleted ones
		for ; oldBlock != nil && oldBlock.NumberU64() != newBlock.NumberU64(); oldBlock = bc.GetBlock(oldBlock.ParentHash(), oldBlock.NumberU64()-1) {
			oldChain = append(oldChain, oldBlock)
			for _, tx := range oldBlock.Transactions() {
				deletedTxs = append(deletedTxs, tx.Hash())
			}

			// Collect deleted logs for notification
			logs := bc.collectLogs(oldBlock.Hash(), true)
			if len(logs) > 0 {
				deletedLogs = append(deletedLogs, logs)
			}
		}
	} else {
		// New chain is longer, stash all blocks away for subsequent insertion
		for ; newBlock != nil && newBlock.NumberU64() != oldBlock.NumberU64(); newBlock = bc.GetBlock(newBlock.ParentHash(), newBlock.NumberU64()-1) {
			newChain = append(newChain, newBlock)
		}
	}
	if oldBlock == nil {
		return fmt.Errorf("invalid old chain")
	}
	if newBlock == nil {
		return fmt.Errorf("invalid new chain")
	}
	// Both sides of the reorg are at the same number, reduce both until the common
	// ancestor is found
	for {
		// If the common ancestor was found, bail out
		if oldBlock.Hash() == newBlock.Hash() {
			commonBlock = oldBlock
			break
		}
		// Remove an old block as well as stash away a new block
		oldChain = append(oldChain, oldBlock)
		for _, tx := range oldBlock.Transactions() {
			deletedTxs = append(deletedTxs, tx.Hash())
		}

		// Collect deleted logs for notification
		logs := bc.collectLogs(oldBlock.Hash(), true)
		if len(logs) > 0 {
			deletedLogs = append(deletedLogs, logs)
		}
		newChain = append(newChain, newBlock)

		// Step back with both chains
		oldBlock = bc.GetBlock(oldBlock.ParentHash(), oldBlock.NumberU64()-1)
		if oldBlock == nil {
			return fmt.Errorf("invalid old chain")
		}
		newBlock = bc.GetBlock(newBlock.ParentHash(), newBlock.NumberU64()-1)
		if newBlock == nil {
			return fmt.Errorf("invalid new chain")
		}
	}

	// Ensure the user sees large reorgs
	if len(oldChain) > 0 && len(newChain) > 0 {
		logFn := log.Info
		msg := "Chain reorg detected"
		if len(oldChain) > 63 {
			msg = "Large chain reorg detected"
			logFn = log.Warn
		}
		logFn(msg, "number", commonBlock.Number(), "hash", commonBlock.Hash(),
			"drop", len(oldChain), "dropfrom", oldChain[0].Hash(), "add", len(newChain), "addfrom", newChain[0].Hash())
		blockReorgAddMeter.Mark(int64(len(newChain)))
		blockReorgDropMeter.Mark(int64(len(oldChain)))
		blockReorgMeter.Mark(1)
	} else if len(newChain) > 0 {
		// Special case happens in the post merge stage that current head is
		// the ancestor of new head while these two blocks are not consecutive
		log.Info("Extend chain", "add", len(newChain), "number", newChain[0].Number(), "hash", newChain[0].Hash())
		blockReorgAddMeter.Mark(int64(len(newChain)))
	} else {
		// len(newChain) == 0 && len(oldChain) > 0
		// rewind the canonical chain to a lower point.
		log.Error("Impossible reorg, please file an issue", "oldnum", oldBlock.Number(), "oldhash", oldBlock.Hash(), "oldblocks", len(oldChain), "newnum", newBlock.Number(), "newhash", newBlock.Hash(), "newblocks", len(newChain))
	}
	// Insert the new chain(except the head block(reverse order)),
	// taking care of the proper incremental order.
	for i := len(newChain) - 1; i >= 1; i-- {
		// Insert the block in the canonical way, re-writing history
		bc.writeHeadBlock(newChain[i])

		// Collect the new added transactions.
		for _, tx := range newChain[i].Transactions() {
			addedTxs = append(addedTxs, tx.Hash())
		}
	}

	// Delete useless indexes right now which includes the non-canonical
	// transaction indexes, canonical chain indexes which above the head.
	indexesBatch := bc.db.NewBatch()
	for _, tx := range types.HashDifference(deletedTxs, addedTxs) {
		rawdb.DeleteTxLookupEntry(indexesBatch, tx)
	}
	// Delete any canonical number assignments above the new head
	number := bc.CurrentBlock().NumberU64()
	for i := number + 1; ; i++ {
		hash := rawdb.ReadCanonicalHash(bc.db, i)
		if hash == (common.Hash{}) {
			break
		}
		rawdb.DeleteCanonicalHash(indexesBatch, i)
	}
	if err := indexesBatch.Write(); err != nil {
		log.Crit("Failed to delete useless indexes", "err", err)
	}

	// Collect the logs
	for i := len(newChain) - 1; i >= 1; i-- {
		// Collect reborn logs due to chain reorg
		logs := bc.collectLogs(newChain[i].Hash(), false)
		if len(logs) > 0 {
			rebirthLogs = append(rebirthLogs, logs)
		}
	}
	// If any logs need to be fired, do it now. In theory we could avoid creating
	// this goroutine if there are no events to fire, but realistcally that only
	// ever happens if we're reorging empty blocks, which will only happen on idle
	// networks where performance is not an issue either way.
	if len(deletedLogs) > 0 {
		bc.rmLogsFeed.Send(RemovedLogsEvent{mergeLogs(deletedLogs, true)})
	}
	if len(rebirthLogs) > 0 {
		bc.logsFeed.Send(mergeLogs(rebirthLogs, false))
	}
	if len(oldChain) > 0 {
		for i := len(oldChain) - 1; i >= 0; i-- {
			bc.chainSideFeed.Send(ChainSideEvent{Block: oldChain[i]})
		}
	}
	return nil
}

// InsertBlockWithoutSetHead executes the block, runs the necessary verification
// upon it and then persist the block and the associate state into the database.
// The key difference between the InsertChain is it won't do the canonical chain
// updating. It relies on the additional SetChainHead call to finalize the entire
// procedure.
func (bc *BlockChain) InsertBlockWithoutSetHead(block *types.Block) error {
	if !bc.chainmu.TryLock() {
		return errChainStopped
	}
	defer bc.chainmu.Unlock()

	_, err := bc.insertChain(types.Blocks{block}, true, false)
	return err
}

// SetChainHead rewinds the chain to set the new head block as the specified
// block. It's possible that after the reorg the relevant state of head
// is missing. It can be fixed by inserting a new block which triggers
// the re-execution.
func (bc *BlockChain) SetChainHead(newBlock *types.Block) error {
	if !bc.chainmu.TryLock() {
		return errChainStopped
	}
	defer bc.chainmu.Unlock()

	// Run the reorg if necessary and set the given block as new head.
	if newBlock.ParentHash() != bc.CurrentBlock().Hash() {
		if err := bc.reorg(bc.CurrentBlock(), newBlock); err != nil {
			return err
		}
	}
	bc.writeHeadBlock(newBlock)

	// Emit events
	logs := bc.collectLogs(newBlock.Hash(), false)
	bc.chainFeed.Send(ChainEvent{Block: newBlock, Hash: newBlock.Hash(), Logs: logs})
	if len(logs) > 0 {
		bc.logsFeed.Send(logs)
	}
	bc.chainHeadFeed.Send(ChainHeadEvent{Block: newBlock})
	log.Info("Set the chain head", "number", newBlock.Number(), "hash", newBlock.Hash())
	return nil
}

func (bc *BlockChain) updateFutureBlocks() {
	futureTimer := time.NewTicker(5 * time.Second)
	defer futureTimer.Stop()
	defer bc.wg.Done()
	for {
		select {
		case <-futureTimer.C:
			bc.procFutureBlocks()
		case <-bc.quit:
			return
		}
	}
}

func (bc *BlockChain) rewindInvalidHeaderBlockLoop() {
	recheck := time.NewTicker(rewindBadBlockInterval)
	defer func() {
		recheck.Stop()
		bc.wg.Done()
	}()
	for {
		select {
		case <-recheck.C:
			bc.tryRewindBadBlocks()
		case <-bc.quit:
			return
		}
	}
}

func (bc *BlockChain) trustedDiffLayerLoop() {
	recheck := time.NewTicker(diffLayerFreezerRecheckInterval)
	defer func() {
		recheck.Stop()
		bc.wg.Done()
	}()
	for {
		select {
		case diff := <-bc.diffQueueBuffer:
			bc.diffQueue.Push(diff, -(int64(diff.Number)))
		case <-bc.quit:
			// Persist all diffLayers when shutdown, it will introduce redundant storage, but it is acceptable.
			// If the client been ungracefully shutdown, it will missing all cached diff layers, it is acceptable as well.
			var batch ethdb.Batch
			for !bc.diffQueue.Empty() {
				diff, _ := bc.diffQueue.Pop()
				diffLayer := diff.(*types.DiffLayer)
				if batch == nil {
					batch = bc.db.DiffStore().NewBatch()
				}
				rawdb.WriteDiffLayer(batch, diffLayer.BlockHash, diffLayer)
				if batch.ValueSize() > ethdb.IdealBatchSize {
					if err := batch.Write(); err != nil {
						log.Error("Failed to write diff layer", "err", err)
						return
					}
					batch.Reset()
				}
			}
			if batch != nil {
				// flush data
				if err := batch.Write(); err != nil {
					log.Error("Failed to write diff layer", "err", err)
					return
				}
				batch.Reset()
			}
			return
		case <-recheck.C:
			currentHeight := bc.CurrentBlock().NumberU64()
			var batch ethdb.Batch
			for !bc.diffQueue.Empty() {
				diff, prio := bc.diffQueue.Pop()
				diffLayer := diff.(*types.DiffLayer)

				// if the block not old enough
				if int64(currentHeight)+prio < int64(bc.triesInMemory) {
					bc.diffQueue.Push(diffLayer, prio)
					break
				}
				canonicalHash := bc.GetCanonicalHash(uint64(-prio))
				// on the canonical chain
				if canonicalHash == diffLayer.BlockHash {
					if batch == nil {
						batch = bc.db.DiffStore().NewBatch()
					}
					rawdb.WriteDiffLayer(batch, diffLayer.BlockHash, diffLayer)
					staleHash := bc.GetCanonicalHash(uint64(-prio) - bc.diffLayerFreezerBlockLimit)
					rawdb.DeleteDiffLayer(batch, staleHash)
				}
				if batch != nil && batch.ValueSize() > ethdb.IdealBatchSize {
					if err := batch.Write(); err != nil {
						panic(fmt.Sprintf("Failed to write diff layer, error %v", err))
					}
					batch.Reset()
				}
			}
			if batch != nil {
				if err := batch.Write(); err != nil {
					panic(fmt.Sprintf("Failed to write diff layer, error %v", err))
				}
				batch.Reset()
			}
		}
	}
}

func (bc *BlockChain) startDoubleSignMonitor() {
	eventChan := make(chan ChainHeadEvent, monitor.MaxCacheHeader)
	sub := bc.SubscribeChainHeadEvent(eventChan)
	defer func() {
		sub.Unsubscribe()
		close(eventChan)
		bc.wg.Done()
	}()

	for {
		select {
		case event := <-eventChan:
			if bc.doubleSignMonitor != nil {
				bc.doubleSignMonitor.Verify(event.Block.Header())
			}
		case <-bc.quit:
			return
		}
	}
}

func (bc *BlockChain) GetUnTrustedDiffLayer(blockHash common.Hash, pid string) *types.DiffLayer {
	bc.diffMux.RLock()
	defer bc.diffMux.RUnlock()
	if diffs, exist := bc.blockHashToDiffLayers[blockHash]; exist && len(diffs) != 0 {
		if len(diffs) == 1 {
			// return the only one diff layer
			for _, diff := range diffs {
				return diff
			}
		}
		// pick the one from exact same peer if we know where the block comes from
		if pid != "" {
			if diffHashes, exist := bc.diffPeersToDiffHashes[pid]; exist {
				for diff := range diffs {
					if _, overlap := diffHashes[diff]; overlap {
						return bc.blockHashToDiffLayers[blockHash][diff]
					}
				}
			}
		}
		// Do not find overlap, do random pick
		for _, diff := range diffs {
			return diff
		}
	}
	return nil
}

func (bc *BlockChain) removeDiffLayers(diffHash common.Hash) {
	bc.diffMux.Lock()
	defer bc.diffMux.Unlock()

	// Untrusted peers
	pids := bc.diffHashToPeers[diffHash]
	invalidDiffHashes := make(map[common.Hash]struct{})
	for pid := range pids {
		invaliDiffHashesPeer := bc.diffPeersToDiffHashes[pid]
		for invaliDiffHash := range invaliDiffHashesPeer {
			invalidDiffHashes[invaliDiffHash] = struct{}{}
		}
		delete(bc.diffPeersToDiffHashes, pid)
	}
	for invalidDiffHash := range invalidDiffHashes {
		delete(bc.diffHashToPeers, invalidDiffHash)
		affectedBlockHash := bc.diffHashToBlockHash[invalidDiffHash]
		if diffs, exist := bc.blockHashToDiffLayers[affectedBlockHash]; exist {
			delete(diffs, invalidDiffHash)
			if len(diffs) == 0 {
				delete(bc.blockHashToDiffLayers, affectedBlockHash)
			}
		}
		delete(bc.diffHashToBlockHash, invalidDiffHash)
	}
}

func (bc *BlockChain) untrustedDiffLayerPruneLoop() {
	recheck := time.NewTicker(diffLayerPruneRecheckInterval)
	defer func() {
		recheck.Stop()
		bc.wg.Done()
	}()
	for {
		select {
		case <-bc.quit:
			return
		case <-recheck.C:
			bc.pruneDiffLayer()
		}
	}
}

func (bc *BlockChain) pruneDiffLayer() {
	currentHeight := bc.CurrentBlock().NumberU64()
	bc.diffMux.Lock()
	defer bc.diffMux.Unlock()
	sortNumbers := make([]uint64, 0, len(bc.diffNumToBlockHashes))
	for number := range bc.diffNumToBlockHashes {
		sortNumbers = append(sortNumbers, number)
	}
	sort.Slice(sortNumbers, func(i, j int) bool {
		return sortNumbers[i] <= sortNumbers[j]
	})
	staleBlockHashes := make(map[common.Hash]struct{})
	for _, number := range sortNumbers {
		if number >= currentHeight-maxDiffForkDist {
			break
		}
		affectedHashes := bc.diffNumToBlockHashes[number]
		if affectedHashes != nil {
			for affectedHash := range affectedHashes {
				staleBlockHashes[affectedHash] = struct{}{}
			}
			delete(bc.diffNumToBlockHashes, number)
		}
	}
	staleDiffHashes := make(map[common.Hash]struct{})
	for blockHash := range staleBlockHashes {
		if diffHashes, exist := bc.blockHashToDiffLayers[blockHash]; exist {
			for diffHash := range diffHashes {
				staleDiffHashes[diffHash] = struct{}{}
				delete(bc.diffHashToBlockHash, diffHash)
				delete(bc.diffHashToPeers, diffHash)
			}
		}
		delete(bc.blockHashToDiffLayers, blockHash)
	}
	for diffHash := range staleDiffHashes {
		for p, diffHashes := range bc.diffPeersToDiffHashes {
			delete(diffHashes, diffHash)
			if len(diffHashes) == 0 {
				delete(bc.diffPeersToDiffHashes, p)
			}
		}
	}
}

// Process received diff layers
func (bc *BlockChain) HandleDiffLayer(diffLayer *types.DiffLayer, pid string, fulfilled bool) error {
	// Basic check
	currentHeight := bc.CurrentBlock().NumberU64()
	if diffLayer.Number > currentHeight && diffLayer.Number-currentHeight > maxDiffQueueDist {
		log.Debug("diff layers too new from current", "pid", pid)
		return nil
	}
	if diffLayer.Number < currentHeight && currentHeight-diffLayer.Number > maxDiffForkDist {
		log.Debug("diff layers too old from current", "pid", pid)
		return nil
	}

	if diffLayer.DiffHash.Load() == nil {
		return fmt.Errorf("unexpected difflayer which diffHash is nil from peeer %s", pid)
	}
	diffHash := diffLayer.DiffHash.Load().(common.Hash)

	bc.diffMux.Lock()
	defer bc.diffMux.Unlock()
	if blockHash, exist := bc.diffHashToBlockHash[diffHash]; exist && blockHash == diffLayer.BlockHash {
		return nil
	}

	if !fulfilled && len(bc.diffPeersToDiffHashes[pid]) > maxDiffLimitForBroadcast {
		log.Debug("too many accumulated diffLayers", "pid", pid)
		return nil
	}

	if len(bc.diffPeersToDiffHashes[pid]) > maxDiffLimit {
		log.Debug("too many accumulated diffLayers", "pid", pid)
		return nil
	}
	if _, exist := bc.diffPeersToDiffHashes[pid]; exist {
		if _, alreadyHas := bc.diffPeersToDiffHashes[pid][diffHash]; alreadyHas {
			return nil
		}
	} else {
		bc.diffPeersToDiffHashes[pid] = make(map[common.Hash]struct{})
	}
	bc.diffPeersToDiffHashes[pid][diffHash] = struct{}{}
	if _, exist := bc.diffNumToBlockHashes[diffLayer.Number]; !exist {
		bc.diffNumToBlockHashes[diffLayer.Number] = make(map[common.Hash]struct{})
	}
	bc.diffNumToBlockHashes[diffLayer.Number][diffLayer.BlockHash] = struct{}{}

	if _, exist := bc.diffHashToPeers[diffHash]; !exist {
		bc.diffHashToPeers[diffHash] = make(map[string]struct{})
	}
	bc.diffHashToPeers[diffHash][pid] = struct{}{}

	if _, exist := bc.blockHashToDiffLayers[diffLayer.BlockHash]; !exist {
		bc.blockHashToDiffLayers[diffLayer.BlockHash] = make(map[common.Hash]*types.DiffLayer)
	}
	bc.blockHashToDiffLayers[diffLayer.BlockHash][diffHash] = diffLayer
	bc.diffHashToBlockHash[diffHash] = diffLayer.BlockHash

	return nil
}

// skipBlock returns 'true', if the block being imported can be skipped over, meaning
// that the block does not need to be processed but can be considered already fully 'done'.
func (bc *BlockChain) skipBlock(err error, it *insertIterator) bool {
	// We can only ever bypass processing if the only error returned by the validator
	// is ErrKnownBlock, which means all checks passed, but we already have the block
	// and state.
	if !errors.Is(err, ErrKnownBlock) {
		return false
	}
	// If we're not using snapshots, we can skip this, since we have both block
	// and (trie-) state
	if bc.snaps == nil {
		return true
	}
	var (
		header     = it.current() // header can't be nil
		parentRoot common.Hash
	)
	// If we also have the snapshot-state, we can skip the processing.
	if bc.snaps.Snapshot(header.Root) != nil {
		return true
	}
	// In this case, we have the trie-state but not snapshot-state. If the parent
	// snapshot-state exists, we need to process this in order to not get a gap
	// in the snapshot layers.
	// Resolve parent block
	if parent := it.previous(); parent != nil {
		parentRoot = parent.Root
	} else if parent = bc.GetHeaderByHash(header.ParentHash); parent != nil {
		parentRoot = parent.Root
	}
	if parentRoot == (common.Hash{}) {
		return false // Theoretically impossible case
	}
	// Parent is also missing snapshot: we can skip this. Otherwise process.
	if bc.snaps.Snapshot(parentRoot) == nil {
		return true
	}
	return false
}

// maintainTxIndex is responsible for the construction and deletion of the
// transaction index.
//
// User can use flag `txlookuplimit` to specify a "recentness" block, below
// which ancient tx indices get deleted. If `txlookuplimit` is 0, it means
// all tx indices will be reserved.
//
// The user can adjust the txlookuplimit value for each launch after fast
// sync, Geth will automatically construct the missing indices and delete
// the extra indices.
func (bc *BlockChain) maintainTxIndex(ancients uint64) {
	defer bc.wg.Done()

	// Before starting the actual maintenance, we need to handle a special case,
	// where user might init Geth with an external ancient database. If so, we
	// need to reindex all necessary transactions before starting to process any
	// pruning requests.
	if ancients > 0 {
		var from = uint64(0)
		if bc.txLookupLimit != 0 && ancients > bc.txLookupLimit {
			from = ancients - bc.txLookupLimit
		}
		rawdb.IndexTransactions(bc.db, from, ancients, bc.quit)
	}

	// indexBlocks reindexes or unindexes transactions depending on user configuration
	indexBlocks := func(tail *uint64, head uint64, done chan struct{}) {
		defer func() { done <- struct{}{} }()

		// If the user just upgraded Geth to a new version which supports transaction
		// index pruning, write the new tail and remove anything older.
		if tail == nil {
			if bc.txLookupLimit == 0 || head < bc.txLookupLimit {
				// Nothing to delete, write the tail and return
				rawdb.WriteTxIndexTail(bc.db, 0)
			} else {
				// Prune all stale tx indices and record the tx index tail
				rawdb.UnindexTransactions(bc.db, 0, head-bc.txLookupLimit+1, bc.quit)
			}
			return
		}
		// If a previous indexing existed, make sure that we fill in any missing entries
		if bc.txLookupLimit == 0 || head < bc.txLookupLimit {
			if *tail > 0 {
				// It can happen when chain is rewound to a historical point which
				// is even lower than the indexes tail, recap the indexing target
				// to new head to avoid reading non-existent block bodies.
				end := *tail
				if end > head+1 {
					end = head + 1
				}
				rawdb.IndexTransactions(bc.db, 0, end, bc.quit)
			}
			return
		}
		// Update the transaction index to the new chain state
		if head-bc.txLookupLimit+1 < *tail {
			// Reindex a part of missing indices and rewind index tail to HEAD-limit
			rawdb.IndexTransactions(bc.db, head-bc.txLookupLimit+1, *tail, bc.quit)
		} else {
			// Unindex a part of stale indices and forward index tail to HEAD-limit
			rawdb.UnindexTransactions(bc.db, *tail, head-bc.txLookupLimit+1, bc.quit)
		}
	}

	// Any reindexing done, start listening to chain events and moving the index window
	var (
		done   chan struct{}                  // Non-nil if background unindexing or reindexing routine is active.
		headCh = make(chan ChainHeadEvent, 1) // Buffered to avoid locking up the event feed
	)
	sub := bc.SubscribeChainHeadEvent(headCh)
	if sub == nil {
		return
	}
	defer sub.Unsubscribe()

	for {
		select {
		case head := <-headCh:
			if done == nil {
				done = make(chan struct{})
				go indexBlocks(rawdb.ReadTxIndexTail(bc.db), head.Block.NumberU64(), done)
			}
		case <-done:
			done = nil
		case <-bc.quit:
			if done != nil {
				log.Info("Waiting background transaction indexer to exit")
				<-done
			}
			return
		}
	}
}

func (bc *BlockChain) isCachedBadBlock(block *types.Block) bool {
	if timeAt, exist := bc.badBlockCache.Get(block.Hash()); exist {
		putAt := timeAt.(time.Time)
		if time.Since(putAt) >= badBlockCacheExpire {
			bc.badBlockCache.Remove(block.Hash())
			return false
		}
		return true
	}
	return false
}

// reportBlock logs a bad block error.
func (bc *BlockChain) reportBlock(block *types.Block, receipts types.Receipts, err error) {
	rawdb.WriteBadBlock(bc.db, block)

	var receiptString string
	for i, receipt := range receipts {
		receiptString += fmt.Sprintf("\t %d: cumulative: %v gas: %v contract: %v status: %v tx: %v logs: %v bloom: %x state: %x\n",
			i, receipt.CumulativeGasUsed, receipt.GasUsed, receipt.ContractAddress.Hex(),
			receipt.Status, receipt.TxHash.Hex(), receipt.Logs, receipt.Bloom, receipt.PostState)
	}
	log.Error(fmt.Sprintf(`
########## BAD BLOCK #########
Chain config: %v

Number: %v
Hash: 0x%x
%v

Error: %v
##############################
`, bc.chainConfig, block.Number(), block.Hash(), receiptString, err))
}

// InsertHeaderChain attempts to insert the given header chain in to the local
// chain, possibly creating a reorg. If an error is returned, it will return the
// index number of the failing header as well an error describing what went wrong.
//
// The verify parameter can be used to fine tune whether nonce verification
// should be done or not. The reason behind the optional check is because some
// of the header retrieval mechanisms already need to verify nonces, as well as
// because nonces can be verified sparsely, not needing to check each.
func (bc *BlockChain) InsertHeaderChain(chain []*types.Header, checkFreq int) (int, error) {
	if len(chain) == 0 {
		return 0, nil
	}
	start := time.Now()
	if i, err := bc.hc.ValidateHeaderChain(chain, checkFreq); err != nil {
		return i, err
	}

	if !bc.chainmu.TryLock() {
		return 0, errChainStopped
	}
	defer bc.chainmu.Unlock()
	_, err := bc.hc.InsertHeaderChain(chain, start, bc.forker)
	return 0, err
}

func (bc *BlockChain) TriesInMemory() uint64 { return bc.triesInMemory }

// Options
func EnableLightProcessor(bc *BlockChain) (*BlockChain, error) {
	bc.processor = NewLightStateProcessor(bc.Config(), bc, bc.engine)
	return bc, nil
}

func EnablePipelineCommit(bc *BlockChain) (*BlockChain, error) {
	bc.pipeCommit = true
	return bc, nil
}

func EnablePersistDiff(limit uint64) BlockChainOption {
	return func(chain *BlockChain) (*BlockChain, error) {
		chain.diffLayerFreezerBlockLimit = limit
		return chain, nil
	}
}

func EnableBlockValidator(chainConfig *params.ChainConfig, engine consensus.Engine, mode VerifyMode, peers verifyPeers) BlockChainOption {
	return func(bc *BlockChain) (*BlockChain, error) {
		if mode.NeedRemoteVerify() {
			vm, err := NewVerifyManager(bc, peers, mode == InsecureVerify)
			if err != nil {
				return nil, err
			}
			go vm.mainLoop()
			bc.validator = NewBlockValidator(chainConfig, bc, engine, EnableRemoteVerifyManager(vm))
		}
		return bc, nil
	}
}

func EnableDoubleSignChecker(bc *BlockChain) (*BlockChain, error) {
	bc.doubleSignMonitor = monitor.NewDoubleSignMonitor()
	return bc, nil
}

func (bc *BlockChain) GetVerifyResult(blockNumber uint64, blockHash common.Hash, diffHash common.Hash) *VerifyResult {
	var res VerifyResult
	res.BlockNumber = blockNumber
	res.BlockHash = blockHash

	if blockNumber > bc.CurrentHeader().Number.Uint64()+maxDiffForkDist {
		res.Status = types.StatusBlockTooNew
		return &res
	} else if blockNumber > bc.CurrentHeader().Number.Uint64() {
		res.Status = types.StatusBlockNewer
		return &res
	}

	header := bc.GetHeaderByHash(blockHash)
	if header == nil {
		if blockNumber > bc.CurrentHeader().Number.Uint64()-maxDiffForkDist {
			res.Status = types.StatusPossibleFork
			return &res
		}

		res.Status = types.StatusImpossibleFork
		return &res
	}

	diff := bc.GetTrustedDiffLayer(blockHash)
	if diff != nil {
		if diff.DiffHash.Load() == nil {
			hash, err := CalculateDiffHash(diff)
			if err != nil {
				res.Status = types.StatusUnexpectedError
				return &res
			}

			diff.DiffHash.Store(hash)
		}

		if diffHash != diff.DiffHash.Load().(common.Hash) {
			res.Status = types.StatusDiffHashMismatch
			return &res
		}

		res.Status = types.StatusFullVerified
		res.Root = header.Root
		return &res
	}

	res.Status = types.StatusPartiallyVerified
	res.Root = header.Root
	return &res
}

func (bc *BlockChain) GetTrustedDiffLayer(blockHash common.Hash) *types.DiffLayer {
	var diff *types.DiffLayer
	if cached, ok := bc.diffLayerCache.Get(blockHash); ok {
		diff = cached.(*types.DiffLayer)
		return diff
	}

	diffStore := bc.db.DiffStore()
	if diffStore != nil {
		diff = rawdb.ReadDiffLayer(diffStore, blockHash)
	}
	return diff
}

func CalculateDiffHash(d *types.DiffLayer) (common.Hash, error) {
	if d == nil {
		return common.Hash{}, fmt.Errorf("nil diff layer")
	}

	diff := &types.ExtDiffLayer{
		BlockHash: d.BlockHash,
		Receipts:  make([]*types.ReceiptForStorage, 0),
		Number:    d.Number,
		Codes:     d.Codes,
		Destructs: d.Destructs,
		Accounts:  d.Accounts,
		Storages:  d.Storages,
	}

	for index, account := range diff.Accounts {
		full, err := snapshot.FullAccount(account.Blob)
		if err != nil {
			return common.Hash{}, fmt.Errorf("decode full account error: %v", err)
		}
		// set account root to empty root
		diff.Accounts[index].Blob = snapshot.SlimAccountRLP(full.Nonce, full.Balance, common.Hash{}, full.CodeHash)
	}

	rawData, err := rlp.EncodeToBytes(diff)
	if err != nil {
		return common.Hash{}, fmt.Errorf("encode new diff error: %v", err)
	}

	hasher := sha3.NewLegacyKeccak256()
	_, err = hasher.Write(rawData)
	if err != nil {
		return common.Hash{}, fmt.Errorf("hasher write error: %v", err)
	}

	var hash common.Hash
	hasher.Sum(hash[:0])
	return hash, nil
}
