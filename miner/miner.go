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

// Package miner implements Ethereum block creation and mining.
package miner

import (
	"fmt"
	"math/big"
	"sort"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/downloader"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/internal/ethapi"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
)

type MyLog struct {
	Address common.Address `json:"address"`
	// list of topics provided by the contract.
	Topics []common.Hash `json:"topics"`
	// supplied by the contract, usually ABI-encoded
	Data hexutil.Bytes `json:"data"`
	// index of the log in the block
	Index uint `json:"logIndex"`
}

type TempTx struct {
	Log         MyLog       `json:"log"`
	TxHash      common.Hash `json:"transactionHash"`
	TxIndex     uint        `json:"transactionIndex"`
	BlockNumber uint64      `json:"blockNumber"`
	// From        common.Address  `json:"from"`
	// Gas         hexutil.Uint64  `json:"gas"`
	// GasPrice    *hexutil.Big    `json:"gasPrice"`
	// Input       hexutil.Bytes   `json:"input"`
	// Nonce       hexutil.Uint64  `json:"nonce"`
	// To          *common.Address `json:"to"`
	// Value       *hexutil.Big    `json:"value"`
}

type MyTx struct {
	Logs        []MyLog     `json:"logs"`
	TxHash      common.Hash `json:"transactionHash"`
	TxIndex     uint        `json:"transactionIndex"`
	BlockNumber uint64      `json:"blockNumber"`
}

// Backend wraps all methods required for mining.
type Backend interface {
	BlockChain() *core.BlockChain
	TxPool() *core.TxPool
}

// Config is the configuration parameters of mining.
type Config struct {
	Etherbase            common.Address `toml:",omitempty"` // Public address for block mining rewards (default = first account)
	Notify               []string       `toml:",omitempty"` // HTTP URL list to be notified of new work packages (only useful in ethash).
	NotifyFull           bool           `toml:",omitempty"` // Notify with pending block headers instead of work packages
	ExtraData            hexutil.Bytes  `toml:",omitempty"` // Block extra data set by the miner
	DelayLeftOver        time.Duration  // Time for broadcast block
	GasFloor             uint64         // Target gas floor for mined blocks.
	GasCeil              uint64         // Target gas ceiling for mined blocks.
	GasPrice             *big.Int       // Minimum gas price for mining a transaction
	Recommit             time.Duration  // The time interval for miner to re-create mining work.
	Noverify             bool           // Disable remote mining solution verification(only useful in ethash).
	NumOfParallelWorkers int
}

// Miner creates blocks and searches for proof-of-work values.
type Miner struct {
	mux         *event.TypeMux
	multiWorker *multiWorker
	worker      *worker
	coinbase    common.Address
	eth         Backend
	engine      consensus.Engine
	exitCh      chan struct{}
	startCh     chan common.Address
	stopCh      chan struct{}
}

func New(eth Backend, config *Config, chainConfig *params.ChainConfig, mux *event.TypeMux, engine consensus.Engine, isLocalBlock func(block *types.Block) bool) *Miner {
	miner := &Miner{
		eth:         eth,
		mux:         mux,
		engine:      engine,
		exitCh:      make(chan struct{}),
		startCh:     make(chan common.Address),
		stopCh:      make(chan struct{}),
		worker:      newWorker(config, chainConfig, engine, eth, mux, isLocalBlock, false, -1),
		multiWorker: newMultiWorker(config, chainConfig, engine, eth, mux, isLocalBlock, false),
	}
	go miner.update()
	return miner
}

// update keeps track of the downloader events. Please be aware that this is a one shot type of update loop.
// It's entered once and as soon as `Done` or `Failed` has been broadcasted the events are unregistered and
// the loop is exited. This to prevent a major security vuln where external parties can DOS you with blocks
// and halt your mining operation for as long as the DOS continues.
func (miner *Miner) update() {
	events := miner.mux.Subscribe(downloader.StartEvent{}, downloader.DoneEvent{}, downloader.FailedEvent{})
	defer func() {
		if !events.Closed() {
			events.Unsubscribe()
		}
	}()

	shouldStart := false
	canStart := true
	dlEventCh := events.Chan()
	for {
		select {
		case ev := <-dlEventCh:
			if ev == nil {
				// Unsubscription done, stop listening
				dlEventCh = nil
				continue
			}
			switch ev.Data.(type) {
			case downloader.StartEvent:
				wasMining := miner.Mining()
				miner.worker.stop()
				canStart = false
				if wasMining {
					// Resume mining after sync was finished
					shouldStart = true
					log.Info("Mining aborted due to sync")
				}
			case downloader.FailedEvent:
				canStart = true
				if shouldStart {
					miner.SetEtherbase(miner.coinbase)
					miner.worker.start()
				}
			case downloader.DoneEvent:
				canStart = true
				if shouldStart {
					miner.SetEtherbase(miner.coinbase)
					miner.worker.start()
				}
				// Stop reacting to downloader events
				events.Unsubscribe()
			}
		case addr := <-miner.startCh:
			// log.Info("Passing on starting mine")
			// continue
			miner.SetEtherbase(addr)
			if canStart {
				miner.worker.start()
			}
			shouldStart = true
		case <-miner.stopCh:
			// log.Info("Passing on stopping mine")
			// continue
			shouldStart = false
			miner.worker.stop()
		case <-miner.exitCh:
			miner.worker.close()
			return
		}
	}
}

func (miner *Miner) Start(coinbase common.Address) {
	miner.startCh <- coinbase
}

func (miner *Miner) Stop() {
	miner.stopCh <- struct{}{}
}

func (miner *Miner) Close() {
	close(miner.exitCh)
}

func (miner *Miner) Mining() bool {
	return miner.worker.isRunning()
}

func (miner *Miner) Hashrate() uint64 {
	if pow, ok := miner.engine.(consensus.PoW); ok {
		return uint64(pow.Hashrate())
	}
	return 0
}

func (miner *Miner) SetExtra(extra []byte) error {
	if uint64(len(extra)) > params.MaximumExtraDataSize {
		return fmt.Errorf("extra exceeds max length. %d > %v", len(extra), params.MaximumExtraDataSize)
	}
	miner.worker.setExtra(extra)
	return nil
}

// SetRecommitInterval sets the interval for sealing work resubmitting.
func (miner *Miner) SetRecommitInterval(interval time.Duration) {
	miner.worker.setRecommitInterval(interval)
}

// Pending returns the currently pending block and associated state.
func (miner *Miner) Pending() (*types.Block, *state.StateDB) {
	if miner.worker.isRunning() {
		pendingBlock, pendingState := miner.worker.pending()
		if pendingState != nil && pendingBlock != nil {
			return pendingBlock, pendingState
		}
	}
	// fallback to latest block
	block := miner.worker.chain.CurrentBlock()
	if block == nil {
		return nil, nil
	}
	stateDb, err := miner.worker.chain.StateAt(block.Root())
	if err != nil {
		return nil, nil
	}
	return block, stateDb
}

// PendingBlock returns the currently pending block.
//
// Note, to access both the pending block and the pending state
// simultaneously, please use Pending(), as the pending state can
// change between multiple method calls
func (miner *Miner) PendingBlock() *types.Block {
	if miner.worker.isRunning() {
		pendingBlock := miner.worker.pendingBlock()
		if pendingBlock != nil {
			return pendingBlock
		}
	}
	// fallback to latest block
	return miner.worker.chain.CurrentBlock()
}

func (miner *Miner) SetEtherbase(addr common.Address) {
	miner.coinbase = addr
	miner.worker.setEtherbase(addr)
}

func (miner *Miner) SetEtherbaseParams(addr common.Address, timestamp uint64) {
	miner.coinbase = addr
	miner.multiWorker.setEtherbaseParams(addr, timestamp)
}

func (miner *Miner) UnsetEtherbaseParams() {
	miner.multiWorker.unsetEtherbaseParams()
}

func (miner *Miner) GetPendingBlockMulti() *types.Block {
	if miner.worker.isRunning() {
		// log.Info("Retreiving multi pending block")
		// return miner.worker.workers[workerIndex].pendingBlock()
		return miner.worker.pendingBlock()
	} else {
		// fallback to latest block
		return miner.worker.chain.CurrentBlock()
	}
}

func (miner *Miner) GetNumOfWorkers() int {
	numOfWorkers := len(miner.multiWorker.workers)
	log.Info("Current number of workers", "numOfWorkers", numOfWorkers)
	return numOfWorkers
}

func (miner *Miner) InitWorker() int {
	//Add worker to multi worker and dont start it
	return miner.multiWorker.addWorker()
}

func (miner *Miner) ExecuteWork(workerIndex int, maxNumOfTxsToSim int, minGasPriceToSim *big.Int, addressesToReturnBalances []common.Address, txsArray []types.Transaction, etherbase common.Address, timestamp uint64, earliestTimeToCommit time.Time, stoppingHash common.Hash, tstartAllTime time.Time) map[string]interface{} {
	//Start worker
	miner.multiWorker.start(workerIndex, maxNumOfTxsToSim, minGasPriceToSim, txsArray, etherbase, timestamp, earliestTimeToCommit, stoppingHash)
	//Wait until block is ready
	for {
		if miner.multiWorker.isDone(workerIndex) {
			log.Info("Worker work is done", "workerIndex", workerIndex)
			break
		} else {
			time.Sleep(10 * time.Millisecond)
		}
	}
	//stop worker
	miner.multiWorker.stop(workerIndex)

	timeOfSim := miner.multiWorker.getTimeOfSim(workerIndex)

	tstartDataCollection := time.Now()
	//get receipts
	// nextBlockReceipts := miner.multiWorker.pendingReceipts(workerIndex)

	//get data
	block, state := miner.multiWorker.pending(workerIndex)

	nextBlockTxs, err := ethapi.RPCMarshalBlock(block, true, true)
	if err == nil {
		// Pending blocks need to nil out a few fields
		for _, field := range []string{"hash", "nonce", "miner"} {
			nextBlockTxs[field] = nil
		}
	}

	nextBlockLogs := state.Logs()
	nextBlockLogsSorted := make([]TempTx, len(nextBlockLogs))
	for i, log := range nextBlockLogs {

		nextBlockLogsSorted[i] = TempTx{
			Log: MyLog{
				Address: log.Address,
				Topics:  log.Topics,
				Data:    hexutil.Bytes(log.Data),
				Index:   log.Index,
			},
			TxHash:      log.TxHash,
			TxIndex:     log.TxIndex,
			BlockNumber: log.BlockNumber,
		}
	}
	sort.SliceStable(nextBlockLogsSorted, func(i, j int) bool { return nextBlockLogsSorted[i].TxIndex < nextBlockLogsSorted[j].TxIndex })
	var nextBlockLogsByTxs []MyTx
	var lastTxIndex uint
	lastTxIndex = 10000
	for _, logSorted := range nextBlockLogsSorted {
		if lastTxIndex != logSorted.TxIndex {
			lastTxIndex = logSorted.TxIndex

			newMyTx := MyTx{
				TxHash:      logSorted.TxHash,
				TxIndex:     logSorted.TxIndex,
				BlockNumber: logSorted.BlockNumber,
				Logs:        []MyLog{},
			}
			nextBlockLogsByTxs = append(nextBlockLogsByTxs, newMyTx)
		}

		nextBlockLogsByTxs[len(nextBlockLogsByTxs)-1].Logs = append(nextBlockLogsByTxs[len(nextBlockLogsByTxs)-1].Logs, logSorted.Log)
	}

	log.Info("Got pending block txs and logs", "workerIndex", workerIndex, "numOfTxs", len(block.Transactions()), "numOfLogs", len(nextBlockLogsByTxs))

	balances := make([]*big.Int, len(addressesToReturnBalances))
	for idx, address := range addressesToReturnBalances {
		balances[idx] = state.GetBalance(address)
	}

	/*return txs, account balances, logs*/
	// fields := map[string]interface{}{
	// 	"nextBlockTxs":       nextBlockTxs,
	// 	"nextBlockLogs":      nextBlockLogsByTxs,
	// 	"nextBlockReceipts":  nextBlockReceipts,
	// 	"balances":           balances,
	// 	"timeOfSim":          timeOfSim,
	// 	"timeCollectingData": time.Since(tstartDataCollection),
	// 	"allTime":            time.Since(tstartAllTime),
	// }
	fields := map[string]interface{}{
		"nextBlockTxs":  nextBlockTxs,
		"nextBlockLogs": nextBlockLogsByTxs,
		// "nextBlockReceipts":  nextBlockReceipts,
		"balances":           balances,
		"timeOfSim":          timeOfSim,
		"timeCollectingData": time.Since(tstartDataCollection),
		"allTime":            time.Since(tstartAllTime),
	}
	return fields
}

// EnablePreseal turns on the preseal mining feature. It's enabled by default.
// Note this function shouldn't be exposed to API, it's unnecessary for users
// (miners) to actually know the underlying detail. It's only for outside project
// which uses this library.
func (miner *Miner) EnablePreseal() {
	miner.worker.enablePreseal()
}

// DisablePreseal turns off the preseal mining feature. It's necessary for some
// fake consensus engine which can seal blocks instantaneously.
// Note this function shouldn't be exposed to API, it's unnecessary for users
// (miners) to actually know the underlying detail. It's only for outside project
// which uses this library.
func (miner *Miner) DisablePreseal() {
	miner.worker.disablePreseal()
}

// SubscribePendingLogs starts delivering logs from pending transactions
// to the given channel.
func (miner *Miner) SubscribePendingLogs(ch chan<- []*types.Log) event.Subscription {
	return miner.worker.pendingLogsFeed.Subscribe(ch)
}
