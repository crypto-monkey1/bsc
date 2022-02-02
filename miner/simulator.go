package miner

import (
	"bytes"
	"context"
	"errors"
	"math"
	"math/big"
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
)

type SignerTxFn func(accounts.Account, *types.Transaction, *big.Int) (*types.Transaction, error)

const offsetInMs = 0
const dumpAllReceipts = false

//calibration params
const percentageSteps = 0.05
const minProcessingTime = 50
const maxProcessingTime = 3000
const numOfTxsForUpdate = 20

type CostumLog struct {
	// Consensus fields:
	// address of the contract that generated the event
	Address common.Address `json:"address" gencodec:"required"`
	// list of topics provided by the contract.
	Topics []common.Hash `json:"topics" gencodec:"required"`
	// supplied by the contract, usually ABI-encoded
	Data string `json:"data" gencodec:"required"`
}

type CostumReceipt struct {
	// Consensus fields: These fields are defined by the Yellow Paper
	Type              uint8        `json:"type,omitempty"`
	PostState         []byte       `json:"root"`
	Status            uint64       `json:"status"`
	CumulativeGasUsed uint64       `json:"cumulativeGasUsed" gencodec:"required"`
	Logs              []*CostumLog `json:"logs"              gencodec:"required"`

	// Implementation fields: These fields are added by geth when processing a transaction.
	// They are stored in the chain database.
	TxHash          common.Hash    `json:"transactionHash" gencodec:"required"`
	ContractAddress common.Address `json:"contractAddress"`
	GasUsed         uint64         `json:"gasUsed" gencodec:"required"`

	// Inclusion information: These fields provide information about the inclusion of the
	// transaction corresponding to this receipt.
	BlockHash        common.Hash `json:"blockHash,omitempty"`
	BlockNumber      *big.Int    `json:"blockNumber,omitempty"`
	TransactionIndex uint        `json:"transactionIndex"`

	ReturnedData string `json:"returnedData"`
	RevertReason string `json:"revertReason"`

	Timestamp int64 `json:"timestamp"`

	Gas      uint64   `json:"gas"`
	GasPrice *big.Int `json:"gasPrice"`

	To    *common.Address `json:"to"`
	From  *common.Address `json:"from"`
	Value *big.Int        `json:"value"`
	Nonce uint64          `json:"nonce"`
	Data  string          `json:"data"`
}

// environment is the worker's current environment and holds all of the current state information.
type simEnvironment struct {
	signer types.Signer

	state     *state.StateDB // apply state changes here
	ancestors mapset.Set     // ancestor set (used for checking uncle parent validity)
	family    mapset.Set     // family set (used for checking uncle invalidity)
	uncles    mapset.Set     // uncle set
	tcount    int            // tx count in cycle
	gasPool   *core.GasPool  // available gas used to pack transactions

	header         *types.Header
	txs            []*types.Transaction
	costumReceipts []*CostumReceipt
	receipts       []*types.Receipt

	block *types.Block

	timeBlockReceived time.Time
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
	// simLogger           *logrus.Logger
	// simLoggerPath       string
	validators      []common.Address
	validatorParams []ValidatorParams
}

type ValidatorParams struct {
	address       common.Address
	offsetPercent float64
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
		// simLogger:           logrus.New(),
	}

	//Init validators struct
	simulator.validators, _ = simulator.getCurrentValidators(simulator.chain.CurrentBlock().Hash())
	simulator.updateValidatorParams(simulator.validators)
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
			simulator.gradeBlock()
			simulator.currentEnv = &simEnvironment{}
			simulator.simulateNextState()
			simulator.simualtingNextState = false

		}

	}
}

func (simulator *Simulator) gradeBlock() {
	tstart := time.Now()
	log.Info("Simulator: Grading block")
	realBlock := simulator.chain.CurrentBlock()
	timeBlockReceived := realBlock.ReceivedAt
	blockProcessingTime := time.Since(timeBlockReceived)
	if simulator.currentEnv == nil {
		log.Info("Simulator: Grading block cannot be done. no sim transactions")
		return
	}
	simBlock := simulator.currentEnv.block

	if simBlock == nil {
		log.Info("Simulator: Grading block cannot be done. no sim transactions")
		return
	}
	diffIn1Not2, diffIn2Not1 := difference(realBlock.Transactions(), simBlock.Transactions())
	procTime := time.Since(tstart)
	numOfTxsInRealBlock := len(realBlock.Transactions())
	numOfTxsInSimBlock := len(simBlock.Transactions())
	if numOfTxsInRealBlock == 0 || numOfTxsInSimBlock == 0 {
		log.Info("Simulator: Grading block cannot be done. no transactions", "numOfTxsInRealBlock", numOfTxsInRealBlock, "numOfTxsInSimBlock", numOfTxsInSimBlock)
		return
	}
	simPercentOutOfReal := int64(100.0 * (1.0 - (float64(len(diffIn1Not2)) / float64(numOfTxsInRealBlock))))
	extraTxPercentInSim := int64(100.0 * (float64(len(diffIn2Not1)) / float64(numOfTxsInSimBlock)))
	validator := realBlock.Coinbase()
	log.Info("Simulator: Grading done", "validator", validator, "simPercentOutOfReal", simPercentOutOfReal, "extraTxPercentInSim", extraTxPercentInSim, "numOfTxsInRealBlock", numOfTxsInRealBlock, "numOfTxsInSimBlock", numOfTxsInSimBlock, "diffInRealNotSim", len(diffIn1Not2), "diffInSimNotReal", len(diffIn2Not1), "blockProcessingTime", blockProcessingTime.Milliseconds(), "procTime", common.PrettyDuration(procTime))

	percentageAddition := 0.0
	if len(diffIn1Not2) > numOfTxsForUpdate {
		percentageAddition += percentageSteps
	}
	if len(diffIn2Not1) > numOfTxsForUpdate {
		percentageAddition -= percentageSteps
	}

	for i, val := range simulator.validatorParams {
		if val.address == validator {
			log.Info("Simulator: current validator offst percentage", "validator", validator, "offsetPercent", simulator.validatorParams[i].offsetPercent, "percentageAddition", percentageAddition)
			if percentageAddition > 0 || percentageAddition < 0 {
				simulator.validatorParams[i].offsetPercent += percentageAddition
				log.Info("Simulator: updating validator offst percentage", "validator", validator, "offsetPercent", simulator.validatorParams[i].offsetPercent)
			}
			break
		}
	}

}

func difference(slice1 types.Transactions, slice2 types.Transactions) ([]common.Hash, []common.Hash) {
	diffIn1Not2 := []common.Hash{}
	diffIn2Not1 := []common.Hash{}
	m := map[common.Hash]int{}

	for _, s1Val := range slice1 {
		m[s1Val.Hash()] = m[s1Val.Hash()] + 1
	}
	for _, s2Val := range slice2 {
		m[s2Val.Hash()] = m[s2Val.Hash()] - 1
	}

	for mKey, mVal := range m {
		if mVal == 1 {
			diffIn1Not2 = append(diffIn1Not2, mKey)
		} else if mVal == -1 {
			diffIn2Not1 = append(diffIn2Not1, mKey)
		}
	}

	return diffIn1Not2, diffIn2Not1
}

func (simulator *Simulator) simulateNextState() {
	simulator.simualtingNextState = true
	tstart := time.Now()

	log.Info("Simulator: Starting to simulate next state")
	parent := simulator.chain.CurrentBlock()
	timeBlockReceived := parent.ReceivedAt
	blockProcessingTime := time.Since(timeBlockReceived).Milliseconds()
	num := parent.Number()
	header := &types.Header{
		ParentHash: parent.Hash(),
		Number:     num.Add(num, common.Big1),
		GasLimit:   core.CalcGasLimit(parent, simulator.config.GasFloor, simulator.config.GasCeil),
		Time:       parent.Time() + 3, //FIXME:Need to take it from config for future changes...
		Difficulty: big.NewInt(2),
	}
	tstartGetLatestState := time.Now()
	state, err := simulator.chain.StateAt(parent.Root())
	procTimeGetLatestState := time.Since(tstartGetLatestState)
	log.Info("Simulator. getting latest state processing time", "procTimeGetLatestState", common.PrettyDuration(procTimeGetLatestState))
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

	simulator.validators, err = simulator.getCurrentValidators(parent.Hash())
	if err != nil {
		log.Error("Simulator: Failed to get current validators set", "err", err)
		return
	}
	simulator.updateValidatorParams(simulator.validators)
	sort.Sort(validatorsAscending(simulator.validators))
	log.Info("Simulator: Got validators set", "validators", simulator.validators)
	nextValidator := simulator.validators[(parent.NumberU64()+1)%uint64(len(simulator.validators))]
	log.Info("Simulator: Got next validator", "currentValidator", parent.Coinbase(), "currentDifficulty", parent.Difficulty(), "nextValidator", nextValidator)
	header.Coinbase = nextValidator

	for _, val := range simulator.validatorParams {
		if val.address == nextValidator {
			log.Info("Simulator: Validator params", "nextValidator", nextValidator, "timeBlockReceived", env.timeBlockReceived, "blockProcessingTime", blockProcessingTime, "val.offsetPercent", val.offsetPercent)
			if val.offsetPercent > 0 {
				sleepingTime := int64(float64(blockProcessingTime) * val.offsetPercent)
				if maxProcessingTime < sleepingTime+blockProcessingTime {
					sleepingTime = maxProcessingTime - blockProcessingTime
				}
				log.Info("Simulator: Going to sleep", "nextValidator", nextValidator, "timeBlockReceived", env.timeBlockReceived, "blockProcessingTime", blockProcessingTime, "val.offsetPercent", val.offsetPercent, "sleepingTime", sleepingTime)
				time.Sleep(time.Duration(sleepingTime * 1e6))
				env.timeBlockReceived = time.Now()
				log.Info("Simulator: Awake", "nextValidator", nextValidator, "timeBlockReceived", env.timeBlockReceived, "sleepingTime", sleepingTime)
			} else if val.offsetPercent < 0 {
				minusToOffset := int64(float64(blockProcessingTime) * val.offsetPercent)
				if minProcessingTime > blockProcessingTime+minusToOffset {
					minusToOffset = -1 * (blockProcessingTime - minProcessingTime)
					if minusToOffset > 0 {
						minusToOffset = 0
					}
				}
				d := time.Duration(minusToOffset * 1e6)
				log.Info("Simulator: reducing processing time", "nextValidator", nextValidator, "timeBlockReceived", env.timeBlockReceived, "blockProcessingTime", blockProcessingTime, "val.offsetPercent", val.offsetPercent, "minusToOffset", minusToOffset, "d", d)
				env.timeBlockReceived = env.timeBlockReceived.Add(time.Duration(minusToOffset * 1e6))
				log.Info("Simulator: Reduced processing time", "nextValidator", nextValidator, "timeBlockReceived", env.timeBlockReceived, "minusToOffset", minusToOffset)
			}
			break
		}
	}

	pending, err := simulator.eth.TxPool().Pending()
	if err != nil {
		log.Error("Simulator: Failed to fetch pending transactions", "err", err)
		return
	}

	if len(pending) != 0 {
		txs := types.NewTransactionsByPriceAndNonceForSimulator(env.signer, pending, env.timeBlockReceived, simulator.timeOffset, true)
		if simulator.commitTransactions(env, txs, nil, nil, common.Hash{}) {
			log.Error("Simulator: Something went wrong with commitTransactions")
			return
		}
	}

	simulator.appendBlockRewardsTxs(env)

	env.state.StopPrefetcher()
	env.block = types.NewBlock(env.header, env.txs, nil, env.receipts, trie.NewStackTrie(nil))
	// simulator.logBlock(env.block, env.timeBlockReceived)

	simulator.currentEnv = env
	procTime := time.Since(tstart)
	log.Info("Simulator: Finished simulating next state", "blockNumber", env.block.Number(), "txs", len(env.block.Transactions()), "gasUsed", env.block.GasUsed(), "procTime", common.PrettyDuration(procTime))
}

/*********************** Simulating on current state ***********************/
func (simulator *Simulator) SimulateOnCurrentStatePriority(addressesToReturnBalances []common.Address, addressesToDeleteFromPending []common.Address, blockNumberToSimulate *big.Int, priorityTx *types.Transaction, txsToInject []types.Transaction, stoppingHash common.Hash, returnedDataHash common.Hash, victimHash common.Hash, outputHashX1 bool, tokenAddress common.Address, pairAddress common.Address) map[string]interface{} {
	simulator.SimualtingOnState = true
	log.Info("Simulator: New SimulateOnCurrentStatePriority call. checking if simulator is free...", "simualtingOnState", simulator.SimualtingOnState, "simualtingNextState", simulator.simualtingNextState, "victimHash", victimHash)

	var parent *types.Block
	var state *state.StateDB
	var err error
	var timeBlockReceived time.Time
	currentBlock := simulator.chain.CurrentBlock()
	currentBlockNum := currentBlock.Number()
	oneBlockBeforeSim := new(big.Int)
	oneBlockBeforeSim = oneBlockBeforeSim.Sub(blockNumberToSimulate, common.Big1)
	twoBlockBeforeSim := new(big.Int)
	twoBlockBeforeSim = twoBlockBeforeSim.Sub(oneBlockBeforeSim, common.Big1)
	blockX1Hashes := []common.Hash{}
	if oneBlockBeforeSim.Cmp(currentBlockNum) == 0 {
		parent = currentBlock
		state, err = simulator.chain.StateAt(parent.Root())
		timeBlockReceived = time.Time{}
		log.Info("Simulator: SimulateOnCurrentStatePriority, One block before simulation", "blockNumberToSimulate", blockNumberToSimulate, "currentBlockNum", currentBlockNum, "timeBlockReceived", timeBlockReceived)
		if err != nil {
			log.Error("Simulator: SimulateOnCurrentStatePriority Failed to create simulator context", "err", err)
			return nil
		}
	} else if twoBlockBeforeSim.Cmp(currentBlockNum) == 0 {
		if simulator.simualtingNextState {
			log.Warn("Simulator: SimulateOnCurrentStatePriority Busy simulating next state", "blockNumberToSimulate", blockNumberToSimulate, "currentBlockNum", currentBlockNum)
			return nil
		}
		if oneBlockBeforeSim.Cmp(simulator.currentEnv.block.Number()) == 0 {
			parent = simulator.currentEnv.block
			tstartCopyState := time.Now()
			state = simulator.currentEnv.state.Copy()
			procTimeCopyState := time.Since(tstartCopyState)
			timeBlockReceived = simulator.currentEnv.timeBlockReceived
			log.Info("Simulator: SimulateOnCurrentStatePriority, Two block before simulation", "procTimeCopyState", common.PrettyDuration(procTimeCopyState), "blockNumberToSimulate", blockNumberToSimulate, "currentBlockNum", currentBlockNum, "timeBlockReceived", timeBlockReceived)
			if outputHashX1 {
				for _, tx := range simulator.currentEnv.block.Transactions() {
					blockX1Hashes = append(blockX1Hashes, tx.Hash())
				}
			}
		} else {
			log.Warn("Simulator: SimulateOnCurrentStatePriority not busy, but simulated next state is not compatible with block to simulate", "blockNumberToSimulate", blockNumberToSimulate, "currentBlockNum", currentBlockNum)
			return nil
		}
	} else {
		log.Warn("Simulator: SimulateOnCurrentStatePriority Current block number is weird", "blockNumberToSimulate", blockNumberToSimulate, "currentBlockNum", currentBlockNum)
		return nil
	}

	tstart := time.Now()

	log.Info("Simulator: SimulateOnCurrentStatePriority Starting to simulate on top of current state")

	num := parent.Number()
	header := &types.Header{
		ParentHash: parent.Hash(),
		Number:     num.Add(num, common.Big1),
		GasLimit:   core.CalcGasLimit(parent, simulator.config.GasFloor, simulator.config.GasCeil),
		Time:       parent.Time() + 3, //FIXME:Need to take it from config for future changes...
		Difficulty: big.NewInt(2),
	}

	nextValidator := simulator.validators[(parent.NumberU64()+1)%uint64(len(simulator.validators))]
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

	//delete from pending
	for _, address := range addressesToDeleteFromPending {
		delete(pending, address)
	}

	//Delete Proirity
	// fromPriority, _ := types.Sender(env.signer, priorityTx)
	// delete(pending, fromPriority)

	//Inject txs
	if len(txsToInject) > 0 {
		// for i, _ := range txsToInject {
		// 	from, _ := types.Sender(env.signer, &txsToInject[i])
		// 	delete(pending, from)
		// }
		for i, _ := range txsToInject {
			//Set time of sending to now in order for it to be sorted last
			txsToInject[i].SetTimeNowPlusOffset(300)
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
		txs := types.NewTransactionsByPriceAndNonceForSimulator(env.signer, pending, timeBlockReceived, simulator.timeOffset, false)
		if simulator.commitTransactions(env, txs, priorityTx, parent.Header(), stoppingHash) {
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
	txArrayReceipts := []CostumReceipt{}
	allReceipts := []CostumReceipt{}
	victimReceipt := CostumReceipt{}
	allHashes := []common.Hash{}
	realtedReceipts := []CostumReceipt{}

	highestGasPrice := big.NewInt(0)
	returnedData := "0"
	if dumpAllReceipts {
		for _, receipt := range env.costumReceipts {
			allReceipts = append(allReceipts, *receipt)
		}
	}
	for i, receipt := range env.costumReceipts {
		if checkReceiptRelation(receipt, tokenAddress, pairAddress) {
			realtedReceipts = append(realtedReceipts, *receipt)
		}
		allHashes = append(allHashes, receipt.TxHash)
		if receipt.TxHash == priorityTx.Hash() {
			txArrayReceipts = append(txArrayReceipts, *receipt)
			highestGasPrice = env.costumReceipts[i+1].GasPrice
		}

		if receipt.TxHash == returnedDataHash {
			returnedData = receipt.ReturnedData
		}

		if receipt.TxHash == victimHash {
			victimReceipt = *receipt
		}

		for _, tx := range txsToInject {
			if receipt.TxHash == tx.Hash() {
				txArrayReceipts = append(txArrayReceipts, *receipt)
			}
		}

	}

	simulatorResult := map[string]interface{}{
		"highestGasPrice":      highestGasPrice,
		"realtedReceipts":      realtedReceipts,
		"txArrayReceipts":      txArrayReceipts,
		"balances":             balances,
		"returnedData":         returnedData,
		"currentStateNotReady": false,
		"wrongBlock":           false,
		"allReceipts":          allReceipts,
		"victimReceipt":        victimReceipt,
		"blockX1Hashes":        blockX1Hashes,
		"allHashes":            allHashes,
	}

	return simulatorResult
}

func checkReceiptRelation(receipt *CostumReceipt, tokenAddress common.Address, pairAddress common.Address) bool {
	if receipt.Logs != nil {
		for _, log := range receipt.Logs {
			if log.Address == tokenAddress || log.Address == pairAddress {
				return true
			}
		}
	}
	return false
}

/*********************** Simulating on current state single tx***********************/
func (simulator *Simulator) SimulateOnCurrentStateSingleTx(blockNumberToSimulate *big.Int, tx *types.Transaction) map[string]interface{} {
	simulator.SimualtingOnState = true
	log.Info("Simulator: New SimulateOnCurrentStateSingleTx call. checking if simulator is free...", "simualtingNextState", simulator.simualtingNextState)
	var parent *types.Block
	var state *state.StateDB
	var err error
	var timeBlockReceived time.Time
	currentBlock := simulator.chain.CurrentBlock()
	currentBlockNum := currentBlock.Number()
	oneBlockBeforeSim := new(big.Int)
	oneBlockBeforeSim = oneBlockBeforeSim.Sub(blockNumberToSimulate, common.Big1)
	twoBlockBeforeSim := new(big.Int)
	twoBlockBeforeSim = twoBlockBeforeSim.Sub(oneBlockBeforeSim, common.Big1)
	if oneBlockBeforeSim.Cmp(currentBlockNum) == 0 {
		parent = currentBlock
		state, err = simulator.chain.StateAt(parent.Root())
		timeBlockReceived = time.Time{}
		log.Info("Simulator: SimulateOnCurrentStateSingleTx, One block before simulation", "blockNumberToSimulate", blockNumberToSimulate, "currentBlockNum", currentBlockNum, "timeBlockReceived", timeBlockReceived)
		if err != nil {
			log.Error("Simulator: SimulateOnCurrentStateSingleTx Failed to create simulator context", "err", err)
			return nil
		}
	} else if twoBlockBeforeSim.Cmp(currentBlockNum) == 0 {
		if simulator.simualtingNextState {
			log.Warn("Simulator: SimulateOnCurrentStateSingleTx Busy simulating next state", "blockNumberToSimulate", blockNumberToSimulate, "currentBlockNum", currentBlockNum)
			return nil
		}
		if oneBlockBeforeSim.Cmp(simulator.currentEnv.block.Number()) == 0 {
			parent = simulator.currentEnv.block
			tstartCopyState := time.Now()
			state = simulator.currentEnv.state.Copy()
			procTimeCopyState := time.Since(tstartCopyState)
			timeBlockReceived = simulator.currentEnv.timeBlockReceived
			log.Info("Simulator: SimulateOnCurrentStateSingleTx, Two block before simulation", "procTimeCopyState", common.PrettyDuration(procTimeCopyState), "blockNumberToSimulate", blockNumberToSimulate, "currentBlockNum", currentBlockNum, "timeBlockReceived", timeBlockReceived)
		} else {
			log.Warn("Simulator: SimulateOnCurrentStateSingleTx not busy, but simulated next state is not compatible with block to simulate", "blockNumberToSimulate", blockNumberToSimulate, "currentBlockNum", currentBlockNum)
			return nil
		}
	} else {
		log.Warn("Simulator: SimulateOnCurrentStateSingleTx Current block number is weird", "blockNumberToSimulate", blockNumberToSimulate, "currentBlockNum", currentBlockNum)
		return nil
	}

	tstart := time.Now()

	log.Info("Simulator: SimulateOnCurrentStateSingleTx Starting to simulate on top of current state")
	num := parent.Number()
	header := &types.Header{
		ParentHash: parent.Hash(),
		Number:     num.Add(num, common.Big1),
		GasLimit:   core.CalcGasLimit(parent, simulator.config.GasFloor, simulator.config.GasCeil),
		Time:       parent.Time() + 3, //FIXME:Need to take it from config for future changes...
		Difficulty: big.NewInt(2),
	}
	nextValidator := simulator.validators[(parent.NumberU64()+1)%uint64(len(simulator.validators))]
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
	// Keep track of transactions which return errors so they can be removed
	env.tcount = 0

	env.gasPool = new(core.GasPool).AddGas(env.header.GasLimit)
	env.gasPool.SubGas(params.SystemTxsGas)

	processorCapacity := 1
	bloomProcessors := core.NewAsyncReceiptBloomGenerator(processorCapacity)

	env.state.Prepare(tx.Hash(), common.Hash{}, env.tcount)
	receipt, err := core.ApplyTransactionForSimulator(simulator.chainConfig, simulator.chain, &env.header.Coinbase, env.gasPool, env.state, env.header, simulator.currentEnv.header, tx, &env.header.GasUsed, *simulator.chain.GetVMConfig(), bloomProcessors)
	if err != nil {
		log.Error("Simulator: Failed Apply tx", "err", err)
		return nil
	}
	from, _ := types.Sender(env.signer, tx)
	costumReceipt := adjustReceipt(receipt, from)
	bloomProcessors.Close()

	procTime := time.Since(tstart)
	log.Info("Simulator: Finished simulating single tx on current state", "blockNumber", env.header.Number, "gasUsed", receipt.GasUsed, "procTime", common.PrettyDuration(procTime))

	simulatorResult := map[string]interface{}{
		"receipt":              costumReceipt,
		"currentStateNotReady": false,
		"wrongBlock":           false,
	}

	return simulatorResult
}

/*********************** Simulating on current state single tx***********************/
func (simulator *Simulator) SimulateOnCurrentStateBundle(addressesToReturnBalances []common.Address, blockNumberToSimulate *big.Int, txs []types.Transaction) map[string]interface{} {
	simulator.SimualtingOnState = true
	log.Info("Simulator: New SimulateOnCurrentStateBundle call. checking if simulator is free...", "simualtingNextState", simulator.simualtingNextState)
	var parent *types.Block
	var state *state.StateDB
	var err error
	var timeBlockReceived time.Time
	currentBlock := simulator.chain.CurrentBlock()
	currentBlockNum := currentBlock.Number()
	oneBlockBeforeSim := new(big.Int)
	oneBlockBeforeSim = oneBlockBeforeSim.Sub(blockNumberToSimulate, common.Big1)
	twoBlockBeforeSim := new(big.Int)
	twoBlockBeforeSim = twoBlockBeforeSim.Sub(oneBlockBeforeSim, common.Big1)
	if oneBlockBeforeSim.Cmp(currentBlockNum) == 0 {
		parent = currentBlock
		state, err = simulator.chain.StateAt(parent.Root())
		timeBlockReceived = time.Time{}
		log.Info("Simulator: SimulateOnCurrentStateBundle, One block before simulation", "blockNumberToSimulate", blockNumberToSimulate, "currentBlockNum", currentBlockNum, "timeBlockReceived", timeBlockReceived)
		if err != nil {
			log.Error("Simulator: SimulateOnCurrentStateBundle Failed to create simulator context", "err", err)
			return nil
		}
	} else if twoBlockBeforeSim.Cmp(currentBlockNum) == 0 {
		if simulator.simualtingNextState {
			log.Warn("Simulator: SimulateOnCurrentStateBundle Busy simulating next state", "blockNumberToSimulate", blockNumberToSimulate, "currentBlockNum", currentBlockNum)
			return nil
		}
		if oneBlockBeforeSim.Cmp(simulator.currentEnv.block.Number()) == 0 {
			parent = simulator.currentEnv.block
			tstartCopyState := time.Now()
			state = simulator.currentEnv.state.Copy()
			procTimeCopyState := time.Since(tstartCopyState)
			timeBlockReceived = simulator.currentEnv.timeBlockReceived
			log.Info("Simulator: SimulateOnCurrentStateBundle, Two block before simulation", "procTimeCopyState", common.PrettyDuration(procTimeCopyState), "blockNumberToSimulate", blockNumberToSimulate, "currentBlockNum", currentBlockNum, "timeBlockReceived", timeBlockReceived)
		} else {
			log.Warn("Simulator: SimulateOnCurrentStateBundle not busy, but simulated next state is not compatible with block to simulate", "blockNumberToSimulate", blockNumberToSimulate, "currentBlockNum", currentBlockNum)
			return nil
		}
	} else {
		log.Warn("Simulator: SimulateOnCurrentStateBundle Current block number is weird", "blockNumberToSimulate", blockNumberToSimulate, "currentBlockNum", currentBlockNum)
		return nil
	}

	tstart := time.Now()

	log.Info("Simulator: SimulateOnCurrentStateBundle Starting to simulate on top of current state")
	num := parent.Number()
	header := &types.Header{
		ParentHash: parent.Hash(),
		Number:     num.Add(num, common.Big1),
		GasLimit:   core.CalcGasLimit(parent, simulator.config.GasFloor, simulator.config.GasCeil),
		Time:       parent.Time() + 3, //FIXME:Need to take it from config for future changes...
		Difficulty: big.NewInt(2),
	}
	nextValidator := simulator.validators[(parent.NumberU64()+1)%uint64(len(simulator.validators))]
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
	// Keep track of transactions which return errors so they can be removed
	env.tcount = 0

	env.gasPool = new(core.GasPool).AddGas(env.header.GasLimit)
	env.gasPool.SubGas(params.SystemTxsGas)

	processorCapacity := 1
	bloomProcessors := core.NewAsyncReceiptBloomGenerator(processorCapacity)
	returnedReceipts := []CostumReceipt{}
	for _, tx := range txs {
		env.state.Prepare(tx.Hash(), common.Hash{}, env.tcount)
		receipt, err := core.ApplyTransactionForSimulator(simulator.chainConfig, simulator.chain, &env.header.Coinbase, env.gasPool, env.state, env.header, simulator.currentEnv.header, &tx, &env.header.GasUsed, *simulator.chain.GetVMConfig(), bloomProcessors)
		if err != nil {
			log.Error("Simulator: Failed Apply tx", "err", err, "txHash", tx.Hash())
			return nil
		}
		from, _ := types.Sender(env.signer, &tx)
		costumReceipt := adjustReceipt(receipt, from)
		log.Info("Simulator: Applied bundle tx", "txHash", tx.Hash())
		returnedReceipts = append(returnedReceipts, costumReceipt)
		env.tcount++
	}

	bloomProcessors.Close()

	//get balances
	balances := make([]*big.Int, len(addressesToReturnBalances))
	for idx, address := range addressesToReturnBalances {
		balances[idx] = env.state.GetBalance(address)
	}

	procTime := time.Since(tstart)
	log.Info("Simulator: Finished simulating bundle on current state", "blockNumber", env.header.Number, "procTime", common.PrettyDuration(procTime))

	simulatorResult := map[string]interface{}{
		"receipts":             returnedReceipts,
		"balances":             balances,
		"currentStateNotReady": false,
		"wrongBlock":           false,
	}

	return simulatorResult
}

/*********************** Simulate costum next two states  ***********************/

func (simulator *Simulator) SimulateNextTwoStates(addressesToReturnBalances []common.Address, addressesToDeleteFromPending []common.Address, x1BlockNumber *big.Int, priorityX2Tx *types.Transaction, x2TxsToInject []types.Transaction, x3TxsToInject []types.Transaction, stoppingHash common.Hash, returnedDataHash common.Hash, victimHash common.Hash, tokenAddress common.Address, pairAddress common.Address) map[string]interface{} {
	//Phase0: make sure we have the right block (x+1)
	tstart := time.Now()
	log.Info("Simulator: Starting to simulate next two states", "timeReceivedX+1", simulator.timeBlockReceived, "timeNow", time.Now(), "victimHash", victimHash)
	currentBlock := simulator.chain.CurrentBlock()
	currentBlockNumber := currentBlock.Number()
	if currentBlockNumber.Cmp(x1BlockNumber) != 0 {
		log.Warn("Simulator: Wrong block", "currentGethBlock", currentBlockNumber, "wantedBlock", x1BlockNumber)
		return nil
	}
	//Phase1: simulate next state (x+2) without specific txs
	x2Header := &types.Header{
		ParentHash: currentBlock.Hash(),
		Number:     x1BlockNumber.Add(x1BlockNumber, common.Big1),
		GasLimit:   core.CalcGasLimit(currentBlock, simulator.config.GasFloor, simulator.config.GasCeil),
		Time:       currentBlock.Time() + 3, //FIXME:Need to take it from config for future changes...
		Difficulty: big.NewInt(2),
	}
	x2State, err := simulator.chain.StateAt(currentBlock.Root())
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

	sort.Sort(validatorsAscending(simulator.validators))
	log.Info("Simulator: Got validators set", "validators", simulator.validators)
	x2Validator := simulator.validators[(currentBlock.NumberU64()+1)%uint64(len(simulator.validators))]
	log.Info("Simulator: Got next validator", "currentValidator", currentBlock.Coinbase(), "currentDifficulty", currentBlock.Difficulty(), "x2Validator", x2Validator)
	x2Header.Coinbase = x2Validator

	x2Pending, err := simulator.eth.TxPool().Pending()
	if err != nil {
		log.Error("Simulator: Failed to fetch pending transactions", "err", err)
		return nil
	}

	//delete from pending
	for _, address := range addressesToDeleteFromPending {
		delete(x2Pending, address)
	}

	//Dismiss txs
	// if len(x3TxsToInject) > 0 {
	// 	for i, _ := range x3TxsToInject {
	// 		from, _ := types.Sender(x2Env.signer, &x3TxsToInject[i])
	// 		log.Info("Simulator: Before Deleting a tx from x2 pending ", "from", from, "hash", x3TxsToInject[i].Hash(), "numTxs", len(x2Pending[from]))
	// 		delete(x2Pending, from)
	// 		log.Info("Simulator: Deleted a tx from x2 pending ", "from", from, "hash", x3TxsToInject[i].Hash())
	// 	}
	// }

	//Delete Proirity
	// fromPriority, _ := types.Sender(x2Env.signer, priorityX2Tx)
	// delete(x2Pending, fromPriority)

	//Inject txs
	if len(x2TxsToInject) > 0 {
		// for i, _ := range x2TxsToInject {
		// 	from, _ := types.Sender(x2Env.signer, &x2TxsToInject[i])
		// 	log.Info("Simulator: Before Deleting a tx from x2 pending ", "from", from, "hash", x2TxsToInject[i].Hash(), "numTxs", len(x2Pending[from]))
		// 	delete(x2Pending, from)
		// 	log.Info("Simulator: Deleted a tx from x2 pending ", "from", from, "hash", x2TxsToInject[i].Hash(), "numTxs", len(x2Pending[from]))
		// }
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
		txs := types.NewTransactionsByPriceAndNonceForSimulator(x2Env.signer, x2Pending, x2Env.timeBlockReceived, simulator.timeOffset, true)
		if simulator.commitTransactions(x2Env, txs, priorityX2Tx, nil, common.Hash{}) {
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
	victimReceiptX2 := CostumReceipt{}
	txArrayReceipts := []CostumReceipt{}
	relatedReceiptsX2 := []CostumReceipt{}
	allHashesX2 := []common.Hash{}
	for _, receipt := range x2Env.costumReceipts {
		if checkReceiptRelation(receipt, tokenAddress, pairAddress) {
			relatedReceiptsX2 = append(relatedReceiptsX2, *receipt)
		}
		allHashesX2 = append(allHashesX2, receipt.TxHash)
		if receipt.TxHash == priorityX2Tx.Hash() {
			txArrayReceipts = append(txArrayReceipts, *receipt)
		}
		if receipt.TxHash == victimHash {
			victimReceiptX2 = *receipt
		}
		for _, tx := range x2TxsToInject {
			if receipt.TxHash == tx.Hash() {
				txArrayReceipts = append(txArrayReceipts, *receipt)
				log.Info("Simulator: Found injected tx reciet of x2 ", "hash", receipt.TxHash, "To", receipt.To, "GasPrice", receipt.GasPrice, "Nonce", receipt.Nonce, "RevertReason", receipt.RevertReason)
			}
		}
		for _, tx := range x3TxsToInject {
			if receipt.TxHash == tx.Hash() {
				log.Info("Simulator: Weird Found injected tx of x3 at reciet of x2 ", "hash", receipt.TxHash, "To", receipt.To, "GasPrice", receipt.GasPrice, "Nonce", receipt.Nonce, "RevertReason", receipt.RevertReason)
			}
		}
	}
	allX2Receipts := []CostumReceipt{}
	if dumpAllReceipts {
		for _, receipt := range x2Env.costumReceipts {
			allX2Receipts = append(allX2Receipts, *receipt)
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
	x3Validator := simulator.validators[(x2Env.block.NumberU64()+1)%uint64(len(simulator.validators))]
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

	//delete from pending
	for _, address := range addressesToDeleteFromPending {
		delete(x3Pending, address)
	}

	//Dismiss txs
	// if len(x2TxsToInject) > 0 {
	// 	for i, _ := range x2TxsToInject {
	// 		from, _ := types.Sender(x3Env.signer, &x2TxsToInject[i])
	// 		log.Info("Simulator: Before Deleting a tx from x3 pending ", "from", from, "hash", x2TxsToInject[i].Hash(), "numTxs", len(x3Pending[from]))
	// 		delete(x3Pending, from)
	// 		log.Info("Simulator: Deleted a tx from x3 pending ", "from", from, "hash", x2TxsToInject[i].Hash())
	// 	}
	// }

	//Inject txs
	if len(x3TxsToInject) > 0 {
		// for i, _ := range x3TxsToInject {
		// 	from, _ := types.Sender(x3Env.signer, &x3TxsToInject[i])
		// 	log.Info("Simulator: Before Deleting a tx from x3 pending ", "from", from, "hash", x3TxsToInject[i].Hash(), "numTxs", len(x3Pending[from]))
		// 	delete(x3Pending, from)
		// 	log.Info("Simulator: Deleted a tx from x3 pending ", "from", from, "hash", x3TxsToInject[i].Hash())
		// }
		for i, _ := range x3TxsToInject {
			//Set time of sending to now in order for it to be sorted last
			x3TxsToInject[i].SetTimeNowPlusOffset(300)
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
		txs := types.NewTransactionsByPriceAndNonceForSimulator(x3Env.signer, x3Pending, x2Env.timeBlockReceived, simulator.timeOffset, false)
		if simulator.commitTransactions(x3Env, txs, nil, x2Env.header, stoppingHash) {
			log.Error("Simulator: Something went wrong with commitTransactions")
			return nil
		}
	}

	x3Env.state.StopPrefetcher()
	x3Env.block = types.NewBlock(x3Env.header, x3Env.txs, nil, x3Env.receipts, trie.NewStackTrie(nil))
	procTime := time.Since(tstart)
	log.Info("Simulator: Finished simulating two states", "x3BlockNumber", x3Env.block.Number(), "txs", len(x3Env.block.Transactions()), "gasUsed", x3Env.block.GasUsed(), "procTime", common.PrettyDuration(procTime))

	//Testing from here
	victimReceiptX3 := CostumReceipt{}
	for _, receipt := range x3Env.costumReceipts {
		if receipt.TxHash == victimHash {
			victimReceiptX3 = *receipt
		}
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

	allX3Receipts := []CostumReceipt{}
	if dumpAllReceipts {
		for _, receipt := range x3Env.costumReceipts {
			allX3Receipts = append(allX3Receipts, *receipt)
		}
	}

	relatedReceiptsX3 := []CostumReceipt{}
	returnedData := "0"
	allHashesX3 := []common.Hash{}
	highestGasPriceX3 := big.NewInt(0)
	for _, receipt := range x3Env.costumReceipts {
		if checkReceiptRelation(receipt, tokenAddress, pairAddress) {
			relatedReceiptsX3 = append(relatedReceiptsX3, *receipt)
		}
		allHashesX3 = append(allHashesX3, receipt.TxHash)

		if receipt.TxHash == returnedDataHash {
			returnedData = receipt.ReturnedData
		}

		oneOfUs := false
		for _, tx := range x3TxsToInject {
			if receipt.TxHash == tx.Hash() {
				oneOfUs = true
				txArrayReceipts = append(txArrayReceipts, *receipt)
			}
		}

		if !oneOfUs {
			if highestGasPriceX3.Cmp(receipt.GasPrice) == -1 {
				highestGasPriceX3 = receipt.GasPrice
			}
		}

	}

	simulatorResult := map[string]interface{}{
		"highestGasPriceX3": highestGasPriceX3,
		"txArrayReceipts":   txArrayReceipts,
		"balances":          balances,
		"returnedData":      returnedData,
		"allX2Receipts":     allX2Receipts,
		"relatedReceiptsX2": relatedReceiptsX2,
		"allX3Receipts":     allX3Receipts,
		"relatedReceiptsX3": relatedReceiptsX3,
		"victimReceiptX2":   victimReceiptX2,
		"victimReceiptX3":   victimReceiptX3,
		"allHashesX2":       allHashesX2,
		"allHashesX3":       allHashesX3,
	}

	return simulatorResult
}

/*********************** Building state ***********************/

func (simulator *Simulator) commitTransactions(env *simEnvironment, txs *types.TransactionsByPriceAndNonce, priorityTx *types.Transaction, prevHeader *types.Header, stoppingHash common.Hash) bool {
	env.gasPool = new(core.GasPool).AddGas(env.header.GasLimit)
	env.gasPool.SubGas(params.SystemTxsGas)

	processorCapacity := 100
	if txs.CurrentSize() < processorCapacity {
		processorCapacity = txs.CurrentSize()
	}
	bloomProcessors := core.NewAsyncReceiptBloomGenerator(processorCapacity)

	txCount := 0
	stopCommit := false

	//Simulate priority first
	if priorityTx != nil {
		env.state.Prepare(priorityTx.Hash(), common.Hash{}, env.tcount)
		err := simulator.commitTransaction(env, priorityTx, simulator.currentEnv.header, bloomProcessors)
		if err != nil {
			log.Error("Simulator: Something went wrong with commiting priority tx", "error", err)
			return true
		}
		env.tcount++
		txCount++
	}

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
		err := simulator.commitTransaction(env, tx, prevHeader, bloomProcessors)
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

func (simulator *Simulator) commitTransaction(env *simEnvironment, tx *types.Transaction, prevHeader *types.Header, receiptProcessors ...core.ReceiptProcessor) error {
	snap := env.state.Snapshot()
	var receipt *types.Receipt
	var err error
	receipt, err = core.ApplyTransactionForSimulator(simulator.chainConfig, simulator.chain, &env.header.Coinbase, env.gasPool, env.state, env.header, prevHeader, tx, &env.header.GasUsed, *simulator.chain.GetVMConfig(), receiptProcessors...)
	// receipt, err = core.ApplyTransaction(simulator.chainConfig, simulator.chain, &env.header.Coinbase, env.gasPool, env.state, env.header, tx, &env.header.GasUsed, *simulator.chain.GetVMConfig(), receiptProcessors...)
	if err != nil {
		env.state.RevertToSnapshot(snap)
		return err
	}
	from, _ := types.Sender(env.signer, tx)
	costumReceipt := adjustReceipt(receipt, from)

	env.txs = append(env.txs, tx)
	env.receipts = append(env.receipts, receipt)
	env.costumReceipts = append(env.costumReceipts, &costumReceipt)

	return nil
}

func adjustReceipt(receipt *types.Receipt, from common.Address) CostumReceipt {
	costumLogs := []*CostumLog{}
	for _, log := range receipt.Logs {
		costumLog := CostumLog{
			Address: log.Address,
			Topics:  log.Topics,
			Data:    hexutil.Encode(log.Data),
		}
		costumLogs = append(costumLogs, &costumLog)
	}

	costumReceipt := CostumReceipt{
		Type:              receipt.Type,
		PostState:         receipt.PostState,
		Status:            receipt.Status,
		CumulativeGasUsed: receipt.CumulativeGasUsed,
		Logs:              costumLogs,
		TxHash:            receipt.TxHash,
		ContractAddress:   receipt.ContractAddress,
		GasUsed:           receipt.GasUsed,
		BlockHash:         receipt.BlockHash,
		BlockNumber:       receipt.BlockNumber,
		TransactionIndex:  receipt.TransactionIndex,
		ReturnedData:      receipt.ReturnedData,
		RevertReason:      receipt.RevertReason,
		Timestamp:         receipt.Timestamp,
		Gas:               receipt.Gas,
		GasPrice:          receipt.GasPrice,
		To:                receipt.To,
		From:              &from,
		Value:             receipt.Value,
		Nonce:             receipt.Nonce,
		Data:              hexutil.Encode(receipt.Data),
	}
	return costumReceipt
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

// getCurrentValidators get current validators
func (simulator *Simulator) updateValidatorParams(validators []common.Address) {
	if len(simulator.validatorParams) == 0 {
		//initialize struct
		for _, val := range validators {
			log.Info("Simulator: creating new validator params", "address", val)
			newVal := ValidatorParams{
				address:       val,
				offsetPercent: 0.0,
			}
			simulator.validatorParams = append(simulator.validatorParams, newVal)
		}
		return
	}

	for _, valAddress := range validators {
		exist := false
		for _, val := range simulator.validatorParams {
			if val.address == valAddress {
				exist = true
				break
			}
		}
		if !exist {
			log.Info("Simulator: creating new validator params", "address", valAddress)
			newVal := ValidatorParams{
				address:       valAddress,
				offsetPercent: 0.0,
			}
			simulator.validatorParams = append(simulator.validatorParams, newVal)
		}

	}
}

/*********************** Utils ***********************/
// func (simulator *Simulator) logBlock(block *types.Block, timeBlockReceived time.Time) {
// 	info, err := os.Stat(simulator.simLoggerPath)
// 	if err != nil {
// 		log.Error("Unable to read info on logrus file")
// 		return
// 	}

// 	currentSizeOfLog := info.Size()
// 	if currentSizeOfLog > 1e7 {
// 		log.Info("logrus file is larger than 10MB. reseting it")
// 		err := os.Remove(simulator.simLoggerPath)
// 		if err != nil {
// 			log.Error("Unable to delete logrus file")
// 		}
// 		file, err := os.OpenFile(simulator.simLoggerPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
// 		if err == nil {
// 			simulator.simLogger.Out = file
// 		} else {
// 			simulator.simLogger.Info("Failed to log to file, using default stderr")
// 		}
// 	}
// 	txs := block.Transactions()
// 	simulator.simLogger.WithFields(logrus.Fields{
// 		"blockNumberSimulated":        block.NumberU64(),
// 		"prevBlockTimeRecieved":       timeBlockReceived.UnixNano(),
// 		"prevBlockTimeRecievedPretty": timeBlockReceived,
// 		"numOfTxs":                    len(txs),
// 		"hash":                        block.Hash(),
// 		"gasUsed":                     block.GasUsed(),
// 		"gasLimit":                    block.GasLimit(),
// 		"difficulty":                  block.Difficulty(),
// 		"coinbase":                    block.Coinbase(),
// 		"timestamp":                   block.Time(),
// 	}).Info("Block summary")
// 	if len(txs) == 0 {
// 		return
// 	}
// 	for i, tx := range txs {
// 		simulator.simLogger.WithFields(logrus.Fields{
// 			"txHash":         tx.Hash(),
// 			"index":          i,
// 			"timeSeen":       tx.TimeSeen().UnixNano(),
// 			"timeSeenPretty": tx.TimeSeen(),
// 			"gasPrice":       tx.GasPrice(),
// 			"gasLimit":       tx.Gas(),
// 			"to":             tx.To(),
// 			"nonce":          tx.Nonce(),
// 			"value":          tx.Value(),
// 			"cost":           tx.Cost(),
// 			"minedBlock":     block.NumberU64(),
// 		}).Info("Tx summary")
// 	}
// }
