package miner

import (
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
)

type multiWorker struct {
	workers      []*worker
	config       *Config
	chainConfig  *params.ChainConfig
	engine       consensus.Engine
	eth          Backend
	chain        *core.BlockChain
	mux          *event.TypeMux
	isLocalBlock func(block *types.Block) bool
	// regularWorker *worker
}

func (w *multiWorker) stop(workerIndex int) {
	// for _, worker := range w.workers {
	// 	worker.stop()
	// }
	log.Info("Stoping worker", "workerIndex", workerIndex)
	w.workers[workerIndex].stopMulti()
}

func (w *multiWorker) start(workerIndex int, maxNumOfTxsToSim int, minGasPriceToSim *big.Int, txsArray []types.Transaction, etherbase common.Address, timestamp uint64, blockNumberToSimBigInt *big.Int, earliestTimeToCommit time.Time, stoppingHash common.Hash) {
	// log.Info("Starting multi workers")
	// for _, worker := range w.workers {
	// 	// log.Info("Worker started")
	// 	worker.start()
	// 	time.Sleep(50 * time.Millisecond)
	// }
	log.Info("Starting worker", "workerIndex", workerIndex, "maxNumOfTxsToSim", maxNumOfTxsToSim, "minGasPriceToSim", minGasPriceToSim, "numOfTxsToSim", len(txsArray), "earliestTimeToCommit", earliestTimeToCommit, "stoppingHash", stoppingHash)
	w.workers[workerIndex].startMulti(maxNumOfTxsToSim, minGasPriceToSim, txsArray, etherbase, timestamp, blockNumberToSimBigInt, earliestTimeToCommit, stoppingHash)
}

func (w *multiWorker) isDone(workerIndex int) bool {
	return w.workers[workerIndex].isBlockReady
}

func (w *multiWorker) pendingReceipts(workerIndex int) []*types.Receipt {
	return w.workers[workerIndex].pendingReceipts()
}

func (w *multiWorker) getTimeOfSim(workerIndex int) time.Duration {
	return w.workers[workerIndex].timeOfSim
}

func (w *multiWorker) pending(workerIndex int) (*types.Block, *state.StateDB) {
	return w.workers[workerIndex].pending()
}

func (w *multiWorker) close(workerIndex int) {
	// for _, worker := range w.workers {
	// 	worker.close()
	// }
	w.workers[workerIndex].close()
}

func (w *multiWorker) isRunning(workerIndex int) bool {
	// for _, worker := range w.workers {
	// 	if worker.isRunning() {
	// 		return true
	// 	}
	// }
	if w.workers[workerIndex].isRunning() {
		return true
	}
	return false
}

// func (w *multiWorker) setExtra(extra []byte) {
// 	for _, worker := range w.workers {
// 		worker.setExtra(extra)
// 	}
// }

// func (w *multiWorker) setRecommitInterval(interval time.Duration) {
// 	for _, worker := range w.workers {
// 		worker.setRecommitInterval(interval)
// 	}
// }

func (w *multiWorker) setEtherbase(addr common.Address, workerIndex int) {
	// for _, worker := range w.workers {
	// 	worker.setEtherbase(addr)
	// }
	w.workers[workerIndex].setEtherbase(addr)
}

func (w *multiWorker) setEtherbaseParams(addr common.Address, timestamp uint64) {
	for _, worker := range w.workers {
		worker.setEtherbaseParams(addr, timestamp)
	}
	// w.workers[workerIndex].setEtherbaseParams(addr, timestamp)
}

func (w *multiWorker) unsetEtherbaseParams() {
	for _, worker := range w.workers {
		worker.unsetEtherbaseParams()
	}
}

// func (w *multiWorker) enablePreseal() {
// 	for _, worker := range w.workers {
// 		worker.enablePreseal()
// 	}
// }

// func (w *multiWorker) disablePreseal() {
// 	for _, worker := range w.workers {
// 		worker.disablePreseal()
// 	}
// }

// func (w *multiWorker) pendingBlock() *types.Block {
// 	//check which worker has the most updated commit
// 	latestTime := w.regularWorker.timeOfLastCommit
// 	latestIndex := 0
// 	// log.Info("Searching for latest worker", "latestTime", latestTime.Nanosecond(), "latestIndex", latestIndex)
// 	cnt := 0
// 	for _, worker := range w.workers {
// 		// log.Info("inside workers loop", "worker.timeOfLastCommit", worker.timeOfLastCommit.Nanosecond(), "latestTime", latestTime.Nanosecond(), "latestIndex", latestIndex, "cnt", cnt)
// 		if latestTime.Before(worker.timeOfLastCommit) {
// 			// log.Info("found more updated commit", "worker.timeOfLastCommit", worker.timeOfLastCommit.Nanosecond(), "latestTime", latestTime.Nanosecond(), "latestIndex", latestIndex, "cnt", cnt)
// 			latestTime = worker.timeOfLastCommit
// 			latestIndex = cnt
// 		}
// 		cnt = cnt + 1
// 	}
// 	// log.Info("Returning most updated", "latestTime", latestTime.Nanosecond(), "latestIndex", latestIndex, "cnt", cnt)
// 	return w.workers[latestIndex].pendingBlock()
// }

func newMultiWorker(config *Config, chainConfig *params.ChainConfig, engine consensus.Engine, eth Backend, mux *event.TypeMux, isLocalBlock func(*types.Block) bool, init bool) *multiWorker {
	// func newMultiWorker() *multiWorker {

	// regularWorker := newWorker(config, chainConfig, engine, eth, mux, isLocalBlock, init, 0, 0)

	workers := []*worker{}

	// for i := 1; i <= config.NumOfParallelWorkers; i++ {
	// 	workers = append(workers,
	// 		newWorker(config, chainConfig, engine, eth, mux, isLocalBlock, init, int(50*i), i))
	// }

	// log.Info("creating multi worker", "config.NumOfParallelWorkers", config.NumOfParallelWorkers, "worker", len(workers))
	return &multiWorker{
		workers:      workers,
		config:       config,
		chainConfig:  chainConfig,
		engine:       engine,
		eth:          eth,
		mux:          mux,
		isLocalBlock: isLocalBlock,
	}
}

func (w *multiWorker) addWorker() int {
	w.workers = append(w.workers,
		newWorker(w.config, w.chainConfig, w.engine, w.eth, w.mux, w.isLocalBlock, false, len(w.workers)))
	log.Info("Added a worker", "workerIndex", len(w.workers)-1, "w.config.Etherbase", w.config.Etherbase)
	w.workers[len(w.workers)-1].setEtherbase(w.config.Etherbase)
	return len(w.workers) - 1
}
