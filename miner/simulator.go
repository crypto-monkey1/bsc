package miner

import (
	"bytes"
	"context"
	"errors"
	"math"
	"math/big"
	"os"
	"sort"
	"strings"
	"time"

	mapset "github.com/deckarep/golang-set"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/systemcontracts"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/internal/ethapi"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/sirupsen/logrus"
)

type SignerTxFn func(accounts.Account, *types.Transaction, *big.Int) (*types.Transaction, error)

const offsetInMs = 0

// environment is the worker's current environment and holds all of the current state information.
type simEnvironment struct {
	signer types.Signer

	state     *state.StateDB // apply state changes here
	ancestors mapset.Set     // ancestor set (used for checking uncle parent validity)
	family    mapset.Set     // family set (used for checking uncle invalidity)
	uncles    mapset.Set     // uncle set
	tcount    int            // tx count in cycle
	gasPool   *core.GasPool  // available gas used to pack transactions

	header   *types.Header
	txs      []*types.Transaction
	receipts []*types.Receipt

	block *types.Block

	timeBlockReceived time.Time

	validators []common.Address
}

type Simulator struct {
	chainHeadCh         chan core.ChainHeadEvent
	chainHeadSub        event.Subscription
	eth                 Backend
	chain               *core.BlockChain
	chainConfig         *params.ChainConfig
	currentEnv          *simEnvironment
	config              *Config
	validatorSetABI     abi.ABI
	ethAPI              *ethapi.PublicBlockChainAPI
	simEtherbase        common.Address
	signTxFn            SignerTxFn
	signer              types.Signer
	timeOffset          time.Duration
	simualtingNextState bool
	SimualtingOnState   bool
	timeBlockReceived   time.Time
	simLogger           *logrus.Logger
	simLoggerPath       string
	minimumGasPrice     *big.Int
}

func NewSimulator(eth Backend, chainConfig *params.ChainConfig, config *Config, ethAPI *ethapi.PublicBlockChainAPI, simEtherbase common.Address, signTxFn SignerTxFn) *Simulator {

	vABI, err := abi.JSON(strings.NewReader(validatorSetABI))
	if err != nil {
		panic(err)
	}

	simulator := &Simulator{
		chainHeadCh:         make(chan core.ChainHeadEvent, chainHeadChanSize),
		eth:                 eth,
		chain:               eth.BlockChain(),
		chainConfig:         chainConfig,
		config:              config,
		validatorSetABI:     vABI,
		ethAPI:              ethAPI,
		simEtherbase:        simEtherbase,
		signTxFn:            signTxFn,
		signer:              types.NewEIP155Signer(chainConfig.ChainID),
		timeOffset:          time.Duration(offsetInMs * 1e6),
		simualtingNextState: false,
		SimualtingOnState:   false,
		simLogger:           logrus.New(),
		minimumGasPrice:     big.NewInt(5000000000), //Set to 5gwei
	}
	path, err := os.Getwd()
	if err != nil {
		log.Error("Couldnt get current directory path")
	}
	path = path + "/../gethSimLogs"
	log.Info("Creating logs dir", "path", path)
	err = os.MkdirAll(path, os.ModePerm)
	if err != nil {
		log.Error("Couldnt create logs dir")
	}
	simulator.simLoggerPath = path + "/simulatedBlocks.log"
	simulator.simLogger.SetFormatter(&logrus.JSONFormatter{})
	file, err := os.OpenFile(simulator.simLoggerPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err == nil {
		simulator.simLogger.Out = file
	} else {
		simulator.simLogger.Info("Failed to log to file, using default stderr")
	}
	simulator.chainHeadSub = simulator.eth.BlockChain().SubscribeChainHeadEvent(simulator.chainHeadCh)
	log.Info("Simulator: Initialized")
	go simulator.mainLoop()
	return simulator
}

func (simulator *Simulator) mainLoop() {
	for {
		select {
		case head := <-simulator.chainHeadCh:
			log.Info("Simulator: New block processed. checking if simulator is free...", "hash", head.Block.Hash(), "simualtingOnState", simulator.SimualtingOnState, "simualtingNextState", simulator.simualtingNextState)
			simulator.timeBlockReceived = time.Now()
			//Before we reset the current env we need to ensure we are not in the middle of simulation
			for {
				if simulator.simualtingNextState || simulator.SimualtingOnState {
					time.Sleep(10 * time.Millisecond)
				} else {
					break
				}
			}
			simulator.currentEnv = &simEnvironment{}
			simulator.simulateNextState()
			simulator.simualtingNextState = false
		}
	}
}

func (simulator *Simulator) simulateNextState() {
	simulator.simualtingNextState = true
	tstart := time.Now()

	log.Info("Simulator: Starting to simulate next state")
	parent := simulator.chain.CurrentBlock()
	num := parent.Number()
	header := &types.Header{
		ParentHash: parent.Hash(),
		Number:     num.Add(num, common.Big1),
		GasLimit:   core.CalcGasLimit(parent, simulator.config.GasFloor, simulator.config.GasCeil),
		Time:       parent.Time() + 3, //FIXME:Need to take it from config for future changes...
		Difficulty: big.NewInt(2),
	}
	state, err := simulator.chain.StateAt(parent.Root())
	if err != nil {
		log.Error("Simulator: Failed to create simulator context", "err", err)
		return
	}

	env := &simEnvironment{
		signer:            types.MakeSigner(simulator.chainConfig, header.Number),
		state:             state,
		ancestors:         mapset.NewSet(),
		family:            mapset.NewSet(),
		uncles:            mapset.NewSet(),
		header:            header,
		timeBlockReceived: simulator.timeBlockReceived,
	}
	env.state.StartPrefetcher("miner")
	// Keep track of transactions which return errors so they can be removed
	env.tcount = 0

	env.validators, err = simulator.getCurrentValidators(parent.Hash())
	if err != nil {
		log.Error("Simulator: Failed to get current validators set", "err", err)
		return
	}
	sort.Sort(validatorsAscending(env.validators))
	log.Info("Simulator: Got validators set", "validators", env.validators)
	nextValidator := env.validators[(parent.NumberU64()+1)%uint64(len(env.validators))]
	log.Info("Simulator: Got next validator", "currentValidator", parent.Coinbase(), "currentDifficulty", parent.Difficulty(), "nextValidator", nextValidator)
	header.Coinbase = nextValidator

	pending, err := simulator.eth.TxPool().Pending()
	if err != nil {
		log.Error("Simulator: Failed to fetch pending transactions", "err", err)
		return
	}

	if len(pending) != 0 {
		txs := types.NewTransactionsByPriceAndNonceForSimulator(env.signer, pending, env.timeBlockReceived, simulator.timeOffset, true, simulator.minimumGasPrice)
		if simulator.commitTransactions(env, txs, env.header.Number, common.Hash{}) {
			log.Error("Simulator: Something went wrong with commitTransactions")
			return
		}
	}

	simulator.appendBlockRewardsTxs(env)

	env.state.StopPrefetcher()
	env.block = types.NewBlock(env.header, env.txs, nil, env.receipts, trie.NewStackTrie(nil))
	simulator.logBlock(env.block, env.timeBlockReceived)

	simulator.currentEnv = env
	procTime := time.Since(tstart)
	log.Info("Simulator: Finished simulating next state", "blockNumber", env.block.Number(), "txs", len(env.block.Transactions()), "gasUsed", env.block.GasUsed(), "procTime", common.PrettyDuration(procTime))
}

/*********************** Simulating on current state ***********************/
func (simulator *Simulator) SimulateOnCurrentState(addressesToReturnBalances []common.Address, previousBlockNumber *big.Int, txsToInject []types.Transaction, stoppingHash common.Hash, stopReceiptHash common.Hash, returnedDataHash common.Hash) map[string]interface{} {

	log.Info("Simulator: New SimulateOnCurrentState call. checking if simulator is free...", "simualtingOnState", simulator.SimualtingOnState, "simualtingNextState", simulator.simualtingNextState)
	for {
		if simulator.simualtingNextState || simulator.SimualtingOnState {
			time.Sleep(10 * time.Millisecond)
		} else {
			break
		}
	}
	currentBlock := simulator.chain.CurrentBlock()
	currentBlockNum := currentBlock.Number()
	if currentBlockNum.Cmp(previousBlockNumber) != 0 {
		log.Warn("Simulator: Wrong block", "currentGethBlock", currentBlockNum, "wantedBlock", previousBlockNumber)
		return nil
	}
	simulator.SimualtingOnState = true

	tstart := time.Now()

	log.Info("Simulator: Starting to simulate on top of current state")
	parent := simulator.currentEnv.block
	num := parent.Number()
	header := &types.Header{
		ParentHash: parent.Hash(),
		Number:     num.Add(num, common.Big1),
		GasLimit:   core.CalcGasLimit(parent, simulator.config.GasFloor, simulator.config.GasCeil),
		Time:       parent.Time() + 3, //FIXME:Need to take it from config for future changes...
		Difficulty: big.NewInt(2),
	}
	state := simulator.currentEnv.state.Copy()
	nextValidator := simulator.currentEnv.validators[(parent.NumberU64()+1)%uint64(len(simulator.currentEnv.validators))]
	log.Info("Simulator: Got next validator", "currentValidator", parent.Coinbase(), "currentDifficulty", parent.Difficulty(), "nextValidator", nextValidator)
	header.Coinbase = nextValidator
	env := &simEnvironment{
		signer:    types.MakeSigner(simulator.chainConfig, header.Number),
		state:     state,
		ancestors: mapset.NewSet(),
		family:    mapset.NewSet(),
		uncles:    mapset.NewSet(),
		header:    header,
	}
	env.state.StartPrefetcher("miner")
	// Keep track of transactions which return errors so they can be removed
	env.tcount = 0

	pending, err := simulator.eth.TxPool().Pending()
	if err != nil {
		log.Error("Simulator: Failed to fetch pending transactions", "err", err)
		return nil
	}

	//Inject txs
	if len(txsToInject) > 0 {
		for i, _ := range txsToInject {
			from, _ := types.Sender(env.signer, &txsToInject[i])
			delete(pending, from)
		}
		for i, _ := range txsToInject {
			//Set time of sending to now in order for it to be sorted last
			txsToInject[i].SetTimeNowPlusOffset(100)
			from, _ := types.Sender(env.signer, &txsToInject[i])
			if len(pending[from]) == 0 {
				txsArray := make(types.Transactions, 0, 1)
				txsArray = append(txsArray, &txsToInject[i])
				pending[from] = txsArray
				log.Info("Simulator: Injected a tx to pending (first tx from)", "from", from, "hash", txsToInject[i].Hash())
			} else {
				pending[from] = append(pending[from], &txsToInject[i])
				log.Info("Simulator: Injected a tx to pending (from exist in pending)", "from", from, "hash", txsToInject[i].Hash())
			}
		}
	}

	if len(pending) != 0 {
		txs := types.NewTransactionsByPriceAndNonceForSimulator(env.signer, pending, simulator.currentEnv.timeBlockReceived, simulator.timeOffset, false, simulator.minimumGasPrice)
		if simulator.commitTransactions(env, txs, env.header.Number, stoppingHash) {
			log.Error("Simulator: Something went wrong with commitTransactions")
			return nil
		}
	}

	env.state.StopPrefetcher()
	env.block = types.NewBlock(env.header, env.txs, nil, env.receipts, trie.NewStackTrie(nil))
	procTime := time.Since(tstart)
	log.Info("Simulator: Finished simulating on current state", "blockNumber", env.block.Number(), "txs", len(env.block.Transactions()), "gasUsed", env.block.GasUsed(), "procTime", common.PrettyDuration(procTime))

	//Process output
	//get balances
	balances := make([]*big.Int, len(addressesToReturnBalances))
	for idx, address := range addressesToReturnBalances {
		balances[idx] = env.state.GetBalance(address)
	}

	//get receipts
	returnedReceipts := []types.Receipt{}
	txArrayReceipts := []types.Receipt{}

	keepAdding := true
	returnedData := "0"
	for _, receipt := range env.receipts {
		if keepAdding {
			returnedReceipts = append(returnedReceipts, *receipt)
		}

		if receipt.TxHash == stopReceiptHash {
			keepAdding = false
		}

		if receipt.TxHash == returnedDataHash {
			returnedData = receipt.ReturnedData
		}

		for _, tx := range txsToInject {
			if receipt.TxHash == tx.Hash() {
				txArrayReceipts = append(txArrayReceipts, *receipt)
			}
		}

	}

	simulatorResult := map[string]interface{}{
		"nextBlockReceipts": returnedReceipts,
		"txArrayReceipts":   txArrayReceipts,
		"balances":          balances,
		"returnedData":      returnedData,
	}

	return simulatorResult
}

/*********************** Simulate costum next two states  ***********************/

func (simulator *Simulator) SimulateNextTwoStates(addressesToReturnBalances []common.Address, previousBlockNumber *big.Int, x2TxsToInject []types.Transaction, x3TxsToInject []types.Transaction, stoppingHash common.Hash, stopReceiptHash common.Hash, returnedDataHash common.Hash) map[string]interface{} {
	//Phase0: make sure we have the right block (x+1)
	tstart := time.Now()
	log.Info("Simulator: Starting to simulate next two states", "timeReceivedX+1", simulator.timeBlockReceived, "timeNow", time.Now())
	x1Block := simulator.chain.CurrentBlock()
	x1Num := x1Block.Number()
	if x1Num.Cmp(previousBlockNumber) != 0 {
		log.Warn("Simulator: Wrong block", "currentGethBlock", x1Num, "wantedBlock", previousBlockNumber)
		return nil
	}
	//Phase1: simulate next state (x+2) without specific txs
	x2Header := &types.Header{
		ParentHash: x1Block.Hash(),
		Number:     x1Num.Add(x1Num, common.Big1),
		GasLimit:   core.CalcGasLimit(x1Block, simulator.config.GasFloor, simulator.config.GasCeil),
		Time:       x1Block.Time() + 3, //FIXME:Need to take it from config for future changes...
		Difficulty: big.NewInt(2),
	}
	x2State, err := simulator.chain.StateAt(x1Block.Root())
	if err != nil {
		log.Error("Simulator: Failed to create simulator context", "err", err)
		return nil
	}

	x2Env := &simEnvironment{
		signer:            types.MakeSigner(simulator.chainConfig, x2Header.Number),
		state:             x2State,
		ancestors:         mapset.NewSet(),
		family:            mapset.NewSet(),
		uncles:            mapset.NewSet(),
		header:            x2Header,
		timeBlockReceived: simulator.timeBlockReceived,
	}
	x2Env.state.StartPrefetcher("miner")
	// Keep track of transactions which return errors so they can be removed
	x2Env.tcount = 0

	x2Env.validators, err = simulator.getCurrentValidators(x1Block.Hash())
	if err != nil {
		log.Error("Simulator: Failed to get current validators set", "err", err)
		return nil
	}
	sort.Sort(validatorsAscending(x2Env.validators))
	log.Info("Simulator: Got validators set", "validators", x2Env.validators)
	x2Validator := x2Env.validators[(x1Block.NumberU64()+1)%uint64(len(x2Env.validators))]
	log.Info("Simulator: Got next validator", "currentValidator", x1Block.Coinbase(), "currentDifficulty", x1Block.Difficulty(), "x2Validator", x2Validator)
	x2Header.Coinbase = x2Validator

	x2Pending, err := simulator.eth.TxPool().Pending()
	if err != nil {
		log.Error("Simulator: Failed to fetch pending transactions", "err", err)
		return nil
	}

	//Dismiss txs
	if len(x3TxsToInject) > 0 {
		for i, _ := range x3TxsToInject {
			from, _ := types.Sender(x2Env.signer, &x3TxsToInject[i])
			log.Info("Simulator: Before Deleting a tx from x2 pending ", "from", from, "hash", x3TxsToInject[i].Hash(), "numTxs", len(x2Pending[from]))
			delete(x2Pending, from)
			log.Info("Simulator: Deleted a tx from x2 pending ", "from", from, "hash", x3TxsToInject[i].Hash())
		}
	}
	//Inject txs
	if len(x2TxsToInject) > 0 {
		for i, _ := range x2TxsToInject {
			from, _ := types.Sender(x2Env.signer, &x2TxsToInject[i])
			log.Info("Simulator: Before Deleting a tx from x2 pending ", "from", from, "hash", x2TxsToInject[i].Hash(), "numTxs", len(x2Pending[from]))
			delete(x2Pending, from)
			log.Info("Simulator: Deleted a tx from x2 pending ", "from", from, "hash", x2TxsToInject[i].Hash(), "numTxs", len(x2Pending[from]))
		}
		for i, _ := range x2TxsToInject {
			x2TxsToInject[i].SetTime(x2Env.timeBlockReceived.Add(-simulator.timeOffset))
			from, _ := types.Sender(x2Env.signer, &x2TxsToInject[i])
			if len(x2Pending[from]) == 0 {
				txsArray := make(types.Transactions, 0, 1)
				txsArray = append(txsArray, &x2TxsToInject[i])
				x2Pending[from] = txsArray
				log.Info("Simulator: Injected a tx to pending (first tx from)", "from", from, "hash", x2TxsToInject[i].Hash(), "numTxs", len(x2Pending[from]))
			} else {
				x2Pending[from] = append(x2Pending[from], &x2TxsToInject[i])
				log.Info("Simulator: Injected a tx to pending (from exist in pending)", "from", from, "hash", x2TxsToInject[i].Hash(), "numTxs", len(x2Pending[from]))
			}
		}
	}
	if len(x2Pending) != 0 {
		txs := types.NewTransactionsByPriceAndNonceForSimulator(x2Env.signer, x2Pending, x2Env.timeBlockReceived, simulator.timeOffset, true, simulator.minimumGasPrice)
		if simulator.commitTransactions(x2Env, txs, x2Env.header.Number, common.Hash{}) {
			log.Error("Simulator: Something went wrong with commitTransactions")
			return nil
		}
	}

	simulator.appendBlockRewardsTxs(x2Env)
	x2Env.state.StopPrefetcher()
	x2Env.block = types.NewBlock(x2Env.header, x2Env.txs, nil, x2Env.receipts, trie.NewStackTrie(nil))
	firstStateProcTime := time.Since(tstart)
	log.Info("Simulator: Finished simulating first state", "blockNumber", x2Env.block.Number(), "txs", len(x2Env.block.Transactions()), "gasUsed", x2Env.block.GasUsed(), "procTime", common.PrettyDuration(firstStateProcTime))
	//Testing from here
	for _, receipt := range x2Env.receipts {
		for _, tx := range x2TxsToInject {
			if receipt.TxHash == tx.Hash() {
				log.Info("Simulator: Found injected tx reciet of x2 ", "hash", receipt.TxHash, "To", receipt.To, "GasPrice", receipt.GasPrice, "Nonce", receipt.Nonce, "RevertReason", receipt.RevertReason)
			}
		}
		for _, tx := range x3TxsToInject {
			if receipt.TxHash == tx.Hash() {
				log.Info("Simulator: Weird Found injected tx of x3 at reciet of x2 ", "hash", receipt.TxHash, "To", receipt.To, "GasPrice", receipt.GasPrice, "Nonce", receipt.Nonce, "RevertReason", receipt.RevertReason)
			}
		}
	}
	//Testing to here
	//phase2: simulate state (x+3) with inject txs
	x2Num := x2Env.block.Number()
	x3Header := &types.Header{
		ParentHash: x2Env.block.Hash(),
		Number:     x2Num.Add(x2Num, common.Big1),
		GasLimit:   core.CalcGasLimit(x2Env.block, simulator.config.GasFloor, simulator.config.GasCeil),
		Time:       x2Env.block.Time() + 3, //FIXME:Need to take it from config for future changes...
		Difficulty: big.NewInt(2),
	}
	x3State := x2Env.state.Copy()
	x3Validator := x2Env.validators[(x2Env.block.NumberU64()+1)%uint64(len(x2Env.validators))]
	log.Info("Simulator: Got next validator", "x2Validator", x2Env.block.Coinbase(), "currentDifficulty", x2Env.block.Difficulty(), "x3Validator", x3Validator)
	x3Header.Coinbase = x3Validator
	x3Env := &simEnvironment{
		signer:    types.MakeSigner(simulator.chainConfig, x3Header.Number),
		state:     x3State,
		ancestors: mapset.NewSet(),
		family:    mapset.NewSet(),
		uncles:    mapset.NewSet(),
		header:    x3Header,
	}
	x3Env.state.StartPrefetcher("miner")
	// Keep track of transactions which return errors so they can be removed
	x3Env.tcount = 0

	x3Pending, err := simulator.eth.TxPool().Pending()
	if err != nil {
		log.Error("Simulator: Failed to fetch pending transactions", "err", err)
		return nil
	}

	//Dismiss txs
	if len(x2TxsToInject) > 0 {
		for i, _ := range x2TxsToInject {
			from, _ := types.Sender(x3Env.signer, &x2TxsToInject[i])
			log.Info("Simulator: Before Deleting a tx from x3 pending ", "from", from, "hash", x2TxsToInject[i].Hash(), "numTxs", len(x3Pending[from]))
			delete(x3Pending, from)
			log.Info("Simulator: Deleted a tx from x3 pending ", "from", from, "hash", x2TxsToInject[i].Hash())
		}
	}

	//Inject txs
	if len(x3TxsToInject) > 0 {
		for i, _ := range x3TxsToInject {
			from, _ := types.Sender(x3Env.signer, &x3TxsToInject[i])
			log.Info("Simulator: Before Deleting a tx from x3 pending ", "from", from, "hash", x3TxsToInject[i].Hash(), "numTxs", len(x3Pending[from]))
			delete(x3Pending, from)
			log.Info("Simulator: Deleted a tx from x3 pending ", "from", from, "hash", x3TxsToInject[i].Hash())
		}
		for i, _ := range x3TxsToInject {
			//Set time of sending to now in order for it to be sorted last
			x3TxsToInject[i].SetTimeNowPlusOffset(100)
			from, _ := types.Sender(x3Env.signer, &x3TxsToInject[i])
			if len(x3Pending[from]) == 0 {
				txsArray := make(types.Transactions, 0, 1)
				txsArray = append(txsArray, &x3TxsToInject[i])
				x3Pending[from] = txsArray
				log.Info("Simulator: Injected a tx to pending (first tx from)", "from", from, "hash", x3TxsToInject[i].Hash(), "numTxs", len(x3Pending[from]))
			} else {
				x3Pending[from] = append(x3Pending[from], &x3TxsToInject[i])
				log.Info("Simulator: Injected a tx to pending (from exist in pending)", "from", from, "hash", x3TxsToInject[i].Hash(), "numTxs", len(x3Pending[from]))
			}
		}
	}

	if len(x3Pending) != 0 {
		txs := types.NewTransactionsByPriceAndNonceForSimulator(x3Env.signer, x3Pending, x2Env.timeBlockReceived, simulator.timeOffset, false, simulator.minimumGasPrice)
		if simulator.commitTransactions(x3Env, txs, x3Env.header.Number, stoppingHash) {
			log.Error("Simulator: Something went wrong with commitTransactions")
			return nil
		}
	}

	x3Env.state.StopPrefetcher()
	x3Env.block = types.NewBlock(x3Env.header, x3Env.txs, nil, x3Env.receipts, trie.NewStackTrie(nil))
	procTime := time.Since(tstart)
	log.Info("Simulator: Finished simulating two states", "x3BlockNumber", x3Env.block.Number(), "txs", len(x3Env.block.Transactions()), "gasUsed", x3Env.block.GasUsed(), "procTime", common.PrettyDuration(procTime))

	//Testing from here
	for _, receipt := range x3Env.receipts {
		for _, tx := range x2TxsToInject {
			if receipt.TxHash == tx.Hash() {
				log.Info("Simulator: Weird!! Found injected tx from x2 at reciet of x3 ", "hash", receipt.TxHash, "To", receipt.To, "GasPrice", receipt.GasPrice, "Nonce", receipt.Nonce, "RevertReason", receipt.RevertReason)
			}
		}
		for _, tx := range x3TxsToInject {
			if receipt.TxHash == tx.Hash() {
				log.Info("Simulator: Found injected tx reciet of x3 ", "hash", receipt.TxHash, "To", receipt.To, "GasPrice", receipt.GasPrice, "Nonce", receipt.Nonce, "RevertReason", receipt.RevertReason)
			}
		}
	}
	//Testing to here
	//Process output
	//get balances
	balances := make([]*big.Int, len(addressesToReturnBalances))
	for idx, address := range addressesToReturnBalances {
		balances[idx] = x3Env.state.GetBalance(address)
	}

	//get receipts
	returnedReceipts := []types.Receipt{}
	txArrayReceipts := []types.Receipt{}

	keepAdding := true
	returnedData := "0"
	for _, receipt := range x3Env.receipts {
		if keepAdding {
			returnedReceipts = append(returnedReceipts, *receipt)
		}

		if receipt.TxHash == stopReceiptHash {
			keepAdding = false
		}

		if receipt.TxHash == returnedDataHash {
			returnedData = receipt.ReturnedData
		}

		for _, tx := range x3TxsToInject {
			if receipt.TxHash == tx.Hash() {
				txArrayReceipts = append(txArrayReceipts, *receipt)
			}
		}
	}

	simulatorResult := map[string]interface{}{
		"nextBlockReceipts": returnedReceipts,
		"txArrayReceipts":   txArrayReceipts,
		"balances":          balances,
		"returnedData":      returnedData,
	}

	return simulatorResult
}

/*********************** Building state ***********************/

func (simulator *Simulator) commitTransactions(env *simEnvironment, txs *types.TransactionsByPriceAndNonce, blockNumber *big.Int, stoppingHash common.Hash) bool {
	env.gasPool = new(core.GasPool).AddGas(env.header.GasLimit)
	env.gasPool.SubGas(params.SystemTxsGas)

	processorCapacity := 100
	if txs.CurrentSize() < processorCapacity {
		processorCapacity = txs.CurrentSize()
	}
	bloomProcessors := core.NewAsyncReceiptBloomGenerator(processorCapacity)

	txCount := 0
	stopCommit := false

	for {
		// If we don't have enough gas for any further transactions then we're done
		if env.gasPool.Gas() < params.TxGas {
			log.Info("Simulator: Not enough gas for further transactions", "have", env.gasPool, "want", params.TxGas)
			break
		}
		// Retrieve the next transaction and abort if all done
		tx := txs.Peek()
		if tx == nil {
			break
		}
		if stopCommit {
			break
		}

		if stoppingHash == tx.Hash() {
			log.Info("Got stopping hash tx", "hash", tx.Hash())
			stopCommit = true
		}

		// Error may be ignored here. The error has already been checked
		// during transaction acceptance is the transaction pool.
		//
		// We use the eip155 signer regardless of the current hf.
		// from, _ := types.Sender(w.current.signer, tx)
		// Check whether the tx is replay protected. If we're not in the EIP155 hf
		// phase, start ignoring the sender until we do.
		if tx.Protected() && !simulator.chainConfig.IsEIP155(env.header.Number) {
			txs.Pop()
			continue
		}
		// Start executing the transaction
		env.state.Prepare(tx.Hash(), common.Hash{}, env.tcount)
		err := simulator.commitTransaction(env, tx, blockNumber, bloomProcessors)
		switch {
		case errors.Is(err, core.ErrGasLimitReached):
			// Pop the current out-of-gas transaction without shifting in the next from the account
			txs.Pop()

		case errors.Is(err, core.ErrNonceTooLow):
			// New head notification data race between the transaction pool and miner, shift
			txs.Shift()

		case errors.Is(err, core.ErrNonceTooHigh):
			// Reorg notification data race between the transaction pool and miner, skip account =
			txs.Pop()

		case errors.Is(err, nil):
			// Everything ok, collect the logs and shift in the next transaction from the same account
			env.tcount++
			txs.Shift()
			txCount++

		case errors.Is(err, core.ErrTxTypeNotSupported):
			// Pop the unsupported transaction without shifting in the next from the account
			txs.Pop()

		default:
			// Strange error, discard the transaction and get the next in line (note, the
			// nonce-too-high clause will prevent us from executing in vain).
			txs.Shift()
		}
	}
	bloomProcessors.Close()
	return false
}

func (simulator *Simulator) commitTransaction(env *simEnvironment, tx *types.Transaction, blockNumber *big.Int, receiptProcessors ...core.ReceiptProcessor) error {
	snap := env.state.Snapshot()
	var receipt *types.Receipt
	var err error
	receipt, err = core.ApplyTransactionForSimulator(simulator.chainConfig, simulator.chain, &env.header.Coinbase, env.gasPool, env.state, env.header, tx, &env.header.GasUsed, *simulator.chain.GetVMConfig(), blockNumber, receiptProcessors...)
	if err != nil {
		env.state.RevertToSnapshot(snap)
		return err
	}

	env.txs = append(env.txs, tx)
	env.receipts = append(env.receipts, receipt)

	return nil
}

/*********************** Block rewards txs ***********************/
// callmsg implements core.Message to allow passing it as a transaction simulator.
type callmsg struct {
	ethereum.CallMsg
}

func (m callmsg) From() common.Address { return m.CallMsg.From }
func (m callmsg) Nonce() uint64        { return 0 }
func (m callmsg) CheckNonce() bool     { return false }
func (m callmsg) To() *common.Address  { return m.CallMsg.To }
func (m callmsg) GasPrice() *big.Int   { return m.CallMsg.GasPrice }
func (m callmsg) Gas() uint64          { return m.CallMsg.Gas }
func (m callmsg) Value() *big.Int      { return m.CallMsg.Value }
func (m callmsg) Data() []byte         { return m.CallMsg.Data }

func (simulator *Simulator) appendBlockRewardsTxs(env *simEnvironment) error {
	balance := env.state.GetBalance(consensus.SystemAddress)
	if balance.Cmp(common.Big0) <= 0 {
		log.Info("Simulator: No rewards in block", "block number", env.header.Number)
		return nil
	}

	env.state.SetBalance(consensus.SystemAddress, big.NewInt(0))
	env.state.AddBalance(simulator.simEtherbase, balance)
	maxSystemBalance := new(big.Int).Mul(big.NewInt(100), big.NewInt(params.Ether))
	doDistributeSysReward := env.state.GetBalance(common.HexToAddress(systemcontracts.SystemRewardContract)).Cmp(maxSystemBalance) < 0
	if doDistributeSysReward {
		var rewards = new(big.Int)
		const systemRewardPercent = 4 // it means 1/2^4 = 1/16 percentage of gas fee incoming will be distributed to system
		rewards = rewards.Rsh(balance, systemRewardPercent)
		if rewards.Cmp(common.Big0) > 0 {
			err := simulator.distributeToSystem(rewards, env)
			if err != nil {
				return err
			}
			log.Info("Simulatotr: Distribute to system reward pool", "block number", env.header.Number, "amount", rewards)
			balance = balance.Sub(balance, rewards)
		}
	}
	log.Info("Simulatotr: Distribute to validator contract", "block number", env.header.Number, "amount", balance)
	return simulator.distributeToValidator(balance, env)
}

func (simulator *Simulator) distributeToSystem(amount *big.Int, env *simEnvironment) error {
	// get system message
	msg := simulator.getSystemMessage(simulator.simEtherbase, common.HexToAddress(systemcontracts.SystemRewardContract), nil, amount)
	// apply message
	return simulator.applyTransaction(msg, env)
}

func (simulator *Simulator) distributeToValidator(amount *big.Int, env *simEnvironment) error {
	// method
	method := "deposit"

	// get packed data
	data, err := simulator.validatorSetABI.Pack(method,
		env.header.Coinbase,
	)
	if err != nil {
		log.Error("Unable to pack tx for deposit", "error", err)
		return err
	}
	// get system message
	msg := simulator.getSystemMessage(simulator.simEtherbase, common.HexToAddress(systemcontracts.ValidatorContract), data, amount)
	// apply message
	return simulator.applyTransaction(msg, env)
}

// get system message
func (simulator *Simulator) getSystemMessage(from, toAddress common.Address, data []byte, value *big.Int) callmsg {
	return callmsg{
		ethereum.CallMsg{
			From:     from,
			Gas:      math.MaxUint64 / 2,
			GasPrice: big.NewInt(0),
			Value:    value,
			To:       &toAddress,
			Data:     data,
		},
	}
}

func (simulator *Simulator) applyTransaction(msg callmsg, env *simEnvironment) (err error) {
	nonce := env.state.GetNonce(msg.From())
	expectedTx := types.NewTransaction(nonce, *msg.To(), msg.Value(), msg.Gas(), msg.GasPrice(), msg.Data())

	expectedTx, err = simulator.signTxFn(accounts.Account{Address: msg.From()}, expectedTx, simulator.chainConfig.ChainID)
	if err != nil {
		return err
	}
	env.state.Prepare(expectedTx.Hash(), common.Hash{}, len(env.txs))
	gasUsed, err := simulator.applyMessage(msg, env)
	if err != nil {
		return err
	}
	env.txs = append(env.txs, expectedTx)
	var root []byte
	if simulator.chainConfig.IsByzantium(env.header.Number) {
		env.state.Finalise(true)
	} else {
		root = env.state.IntermediateRoot(simulator.chainConfig.IsEIP158(env.header.Number)).Bytes()
	}
	env.header.GasUsed += gasUsed
	receipt := types.NewReceipt(root, false, env.header.GasUsed)
	receipt.TxHash = expectedTx.Hash()
	receipt.GasUsed = gasUsed

	// Set the receipt logs and create a bloom for filtering
	receipt.Logs = env.state.GetLogs(expectedTx.Hash())
	receipt.Bloom = types.CreateBloom(types.Receipts{receipt})
	receipt.BlockHash = env.state.BlockHash()
	receipt.BlockNumber = env.header.Number
	receipt.TransactionIndex = uint(env.state.TxIndex())
	env.receipts = append(env.receipts, receipt)
	env.state.SetNonce(msg.From(), nonce+1)
	return nil
}

func (simulator *Simulator) applyMessage(msg callmsg, env *simEnvironment) (uint64, error) {
	// Create a new context to be used in the EVM environment
	context := core.NewEVMBlockContext(env.header, simulator.chain, &simulator.simEtherbase)
	// Create a new environment which holds all relevant information
	// about the transaction and calling mechanisms.
	vmenv := vm.NewEVM(context, vm.TxContext{Origin: msg.From(), GasPrice: big.NewInt(0)}, env.state, simulator.chainConfig, vm.Config{})
	// Apply the transaction to the current state (included in the env)
	ret, returnGas, err := vmenv.Call(
		vm.AccountRef(msg.From()),
		*msg.To(),
		msg.Data(),
		msg.Gas(),
		msg.Value(),
	)
	if err != nil {
		log.Error("Simulator: apply message failed", "msg", string(ret), "err", err)
	}
	return msg.Gas() - returnGas, err
}

/*********************** Validators functionalities ***********************/
type validatorsAscending []common.Address

func (s validatorsAscending) Len() int           { return len(s) }
func (s validatorsAscending) Less(i, j int) bool { return bytes.Compare(s[i][:], s[j][:]) < 0 }
func (s validatorsAscending) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }

// getCurrentValidators get current validators
func (simulator *Simulator) getCurrentValidators(blockHash common.Hash) ([]common.Address, error) {
	// block
	blockNr := rpc.BlockNumberOrHashWithHash(blockHash, false)

	// method
	method := "getValidators"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // cancel when we are finished consuming integers

	data, err := simulator.validatorSetABI.Pack(method)
	if err != nil {
		log.Error("Simulator: Unable to pack tx for getValidators", "error", err)
		return nil, err
	}
	// call
	msgData := (hexutil.Bytes)(data)
	toAddress := common.HexToAddress(systemcontracts.ValidatorContract)
	gas := (hexutil.Uint64)(uint64(math.MaxUint64 / 2))
	result, err := simulator.ethAPI.Call(ctx, ethapi.CallArgs{
		Gas:  &gas,
		To:   &toAddress,
		Data: &msgData,
	}, blockNr, nil)
	if err != nil {
		return nil, err
	}

	var (
		ret0 = new([]common.Address)
	)
	out := ret0

	if err := simulator.validatorSetABI.UnpackIntoInterface(out, method, result); err != nil {
		return nil, err
	}

	valz := make([]common.Address, len(*ret0))
	for i, a := range *ret0 {
		valz[i] = a
	}
	return valz, nil
}

/*********************** Utils ***********************/
func (simulator *Simulator) logBlock(block *types.Block, timeBlockReceived time.Time) {
	info, err := os.Stat(simulator.simLoggerPath)
	if err != nil {
		log.Error("Unable to read info on logrus file")
		return
	}

	currentSizeOfLog := info.Size()
	if currentSizeOfLog > 1e7 {
		log.Info("logrus file is larger than 10MB. reseting it")
		err := os.Remove(simulator.simLoggerPath)
		if err != nil {
			log.Error("Unable to delete logrus file")
		}
		file, err := os.OpenFile(simulator.simLoggerPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
		if err == nil {
			simulator.simLogger.Out = file
		} else {
			simulator.simLogger.Info("Failed to log to file, using default stderr")
		}
	}
	txs := block.Transactions()
	simulator.simLogger.WithFields(logrus.Fields{
		"blockNumberSimulated":        block.NumberU64(),
		"prevBlockTimeRecieved":       timeBlockReceived.UnixNano(),
		"prevBlockTimeRecievedPretty": timeBlockReceived,
		"numOfTxs":                    len(txs),
		"hash":                        block.Hash(),
		"gasUsed":                     block.GasUsed(),
		"gasLimit":                    block.GasLimit(),
		"difficulty":                  block.Difficulty(),
		"coinbase":                    block.Coinbase(),
		"timestamp":                   block.Time(),
	}).Info("Block summary")
	if len(txs) == 0 {
		return
	}
	for i, tx := range txs {
		simulator.simLogger.WithFields(logrus.Fields{
			"txHash":         tx.Hash(),
			"index":          i,
			"timeSeen":       tx.TimeSeen().UnixNano(),
			"timeSeenPretty": tx.TimeSeen(),
			"gasPrice":       tx.GasPrice(),
			"to":             tx.To(),
			"minedBlock":     block.NumberU64(),
		}).Info("Tx summary")
	}
}
