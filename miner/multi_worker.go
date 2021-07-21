package miner

import (
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
)

type multiWorker struct {
	workers       []*worker
	regularWorker *worker
}

func (w *multiWorker) stop() {
	for _, worker := range w.workers {
		worker.stop()
	}
}

func (w *multiWorker) start() {
	// log.Info("Starting multi workers")
	for _, worker := range w.workers {
		// log.Info("Worker started")
		worker.start()
		time.Sleep(150 * time.Millisecond)
	}
}

func (w *multiWorker) close() {
	for _, worker := range w.workers {
		worker.close()
	}
}

func (w *multiWorker) isRunning() bool {
	for _, worker := range w.workers {
		if worker.isRunning() {
			return true
		}
	}
	return false
}

func (w *multiWorker) setExtra(extra []byte) {
	for _, worker := range w.workers {
		worker.setExtra(extra)
	}
}

func (w *multiWorker) setRecommitInterval(interval time.Duration) {
	for _, worker := range w.workers {
		worker.setRecommitInterval(interval)
	}
}

func (w *multiWorker) setEtherbase(addr common.Address) {
	for _, worker := range w.workers {
		worker.setEtherbase(addr)
	}
}

func (w *multiWorker) setEtherbaseParams(addr common.Address, timestamp uint64) {
	for _, worker := range w.workers {
		worker.setEtherbaseParams(addr, timestamp)
	}
}

func (w *multiWorker) unsetEtherbaseParams() {
	for _, worker := range w.workers {
		worker.unsetEtherbaseParams()
	}
}

func (w *multiWorker) enablePreseal() {
	for _, worker := range w.workers {
		worker.enablePreseal()
	}
}

func (w *multiWorker) disablePreseal() {
	for _, worker := range w.workers {
		worker.disablePreseal()
	}
}

func (w *multiWorker) pendingBlock() *types.Block {
	//check which worker has the most updated commit
	latestTime := w.regularWorker.timeOfLastCommit
	latestIndex := 0
	// log.Info("Searching for latest worker", "latestTime", latestTime.Nanosecond(), "latestIndex", latestIndex)
	cnt := 0
	for _, worker := range w.workers {
		// log.Info("inside workers loop", "worker.timeOfLastCommit", worker.timeOfLastCommit.Nanosecond(), "latestTime", latestTime.Nanosecond(), "latestIndex", latestIndex, "cnt", cnt)
		if latestTime.Before(worker.timeOfLastCommit) {
			// log.Info("found more updated commit", "worker.timeOfLastCommit", worker.timeOfLastCommit.Nanosecond(), "latestTime", latestTime.Nanosecond(), "latestIndex", latestIndex, "cnt", cnt)
			latestTime = worker.timeOfLastCommit
			latestIndex = cnt
		}
		cnt = cnt + 1
	}
	// log.Info("Returning most updated", "latestTime", latestTime.Nanosecond(), "latestIndex", latestIndex, "cnt", cnt)
	return w.workers[latestIndex].pendingBlock()
}

func newMultiWorker(config *Config, chainConfig *params.ChainConfig, engine consensus.Engine, eth Backend, mux *event.TypeMux, isLocalBlock func(*types.Block) bool, init bool) *multiWorker {

	regularWorker := newWorker(config, chainConfig, engine, eth, mux, isLocalBlock, init, 0, 0)

	workers := []*worker{regularWorker}

	for i := 1; i <= config.NumOfParallelWorkers; i++ {
		workers = append(workers,
			newWorker(config, chainConfig, engine, eth, mux, isLocalBlock, init, int(50*i), i))
	}

	log.Info("creating multi worker", "config.NumOfParallelWorkers", config.NumOfParallelWorkers, "worker", len(workers))
	return &multiWorker{
		regularWorker: regularWorker,
		workers:       workers,
	}
}
