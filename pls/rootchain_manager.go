package pls

import (
	"bytes"
	"context"
	"errors"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/Onther-Tech/plasma-evm/accounts"
	"github.com/Onther-Tech/plasma-evm/accounts/abi"
	"github.com/Onther-Tech/plasma-evm/accounts/abi/bind"
	"github.com/Onther-Tech/plasma-evm/common"
	"github.com/Onther-Tech/plasma-evm/contracts/plasma/rootchain"
	"github.com/Onther-Tech/plasma-evm/core"
	"github.com/Onther-Tech/plasma-evm/core/types"
	"github.com/Onther-Tech/plasma-evm/ethclient"
	"github.com/Onther-Tech/plasma-evm/event"
	"github.com/Onther-Tech/plasma-evm/log"
	"github.com/Onther-Tech/plasma-evm/miner"
	"github.com/Onther-Tech/plasma-evm/miner/epoch"
	"github.com/Onther-Tech/plasma-evm/params"
)

const MAX_EPOCH_EVENTS = 0

var (
	baseCallOpt               = &bind.CallOpts{Pending: false, Context: context.Background()}
	requestableContractABI, _ = abi.JSON(strings.NewReader(rootchain.RequestableContractIABI))
	rootchainContractABI, _   = abi.JSON(strings.NewReader(rootchain.RootChainABI))

	//TODO: sholud delete this after fixing rcm.backend.NetworkId
	rootchainNetworkId = big.NewInt(1337)
)

type invalidExit struct {
	forkNumber  *big.Int
	blockNumber *big.Int
	receipt     *types.Receipt
	index       int64
	proof       []common.Hash
}

type invalidExits []*invalidExit

type RootChainManager struct {
	config *Config
	stopFn func()

	txPool     *core.TxPool
	blockchain *core.BlockChain

	backend           *ethclient.Client
	rootchainContract *rootchain.RootChain

	eventMux       *event.TypeMux
	accountManager *accounts.Manager

	miner    *miner.Miner
	minerEnv *epoch.EpochEnvironment
	state    *rootchainState

	// fork => block number => invalidExits
	invalidExits map[uint64]map[uint64]invalidExits

	// channels
	quit             chan struct{}
	epochPreparedCh  chan *rootchain.RootChainEpochPrepared
	blockFinalizedCh chan *rootchain.RootChainBlockFinalized

	lock sync.RWMutex // Protects the variadic fields (e.g. gas price and etherbase)
}

func (rcm *RootChainManager) RootchainContract() *rootchain.RootChain { return rcm.rootchainContract }
func (rcm *RootChainManager) NRELength() (*big.Int, error) {
	return rcm.rootchainContract.NRELength(baseCallOpt)
}

func NewRootChainManager(
	config *Config,
	stopFn func(),
	txPool *core.TxPool,
	blockchain *core.BlockChain,
	backend *ethclient.Client,
	rootchainContract *rootchain.RootChain,
	eventMux *event.TypeMux,
	accountManager *accounts.Manager,
	miner *miner.Miner,
	env *epoch.EpochEnvironment,
) (*RootChainManager, error) {
	rcm := &RootChainManager{
		config:            config,
		stopFn:            stopFn,
		txPool:            txPool,
		blockchain:        blockchain,
		backend:           backend,
		rootchainContract: rootchainContract,
		eventMux:          eventMux,
		accountManager:    accountManager,
		miner:             miner,
		minerEnv:          env,
		invalidExits:      make(map[uint64]map[uint64]invalidExits),
		quit:              make(chan struct{}),
		epochPreparedCh:   make(chan *rootchain.RootChainEpochPrepared, MAX_EPOCH_EVENTS),
		blockFinalizedCh:  make(chan *rootchain.RootChainBlockFinalized),
	}

	rcm.state = newRootchainState(rcm)

	epochLength, err := rcm.NRELength()
	if err != nil {
		return nil, err
	}

	miner.SetNRBepochLength(epochLength)

	return rcm, nil
}

func (rcm *RootChainManager) Start() error {
	if err := rcm.run(); err != nil {
		return err
	}

	go rcm.pingBackend()

	return nil
}

func (rcm *RootChainManager) Stop() error {
	rcm.backend.Close()
	close(rcm.quit)
	return nil
}

func (rcm *RootChainManager) run() error {
	go rcm.runHandlers()
	go rcm.runSubmitter()
	go rcm.runDetector()

	if err := rcm.watchEvents(); err != nil {
		return err
	}

	return nil
}

// watchEvents watchs RootChain contract events
func (rcm *RootChainManager) watchEvents() error {
	filterer, err := rootchain.NewRootChainFilterer(rcm.config.RootChainContract, rcm.backend)
	if err != nil {
		return err
	}

	startBlockNumber := rcm.blockchain.GetRootchainBlockNumber()
	filterOpts := &bind.FilterOpts{
		Start:   startBlockNumber,
		End:     nil,
		Context: context.Background(),
	}

	// iterate to find previous epoch prepared events
	iteratorForEpochPreparedEvent, err := filterer.FilterEpochPrepared(filterOpts)
	if err != nil {
		return err
	}

	log.Info("Iterating epoch prepared event")
	for iteratorForEpochPreparedEvent.Next() {
		e := iteratorForEpochPreparedEvent.Event
		if e != nil {
			rcm.handleEpochPrepared(e)
		}
	}

	// iterate to find previous block finalized events
	iteratorForBlockFinalizedEvent, err := filterer.FilterBlockFinalized(filterOpts)
	if err != nil {
		return err
	}

	log.Info("Iterating block finalized event")
	for iteratorForBlockFinalizedEvent.Next() {
		e := iteratorForBlockFinalizedEvent.Event
		if e != nil {
			rcm.handleBlockFinalzied(e)
		}
	}

	watchOpts := &bind.WatchOpts{
		Context: context.Background(),
		Start:   &startBlockNumber,
	}
	epochPrepareWatchCh := make(chan *rootchain.RootChainEpochPrepared)
	blockFinalizedWatchCh := make(chan *rootchain.RootChainBlockFinalized)

	log.Info("Watching epoch prepared event", "start block number", startBlockNumber)
	epochPrepareSub, err := filterer.WatchEpochPrepared(watchOpts, epochPrepareWatchCh)
	if err != nil {
		return err
	}

	log.Info("Watching block finalized event", "start block number", startBlockNumber)
	blockFinalizedSub, err := filterer.WatchBlockFinalized(watchOpts, blockFinalizedWatchCh)
	if err != nil {
		return err
	}

	go func() {
		for {
			select {
			case e := <-epochPrepareWatchCh:
				if e != nil {
					rcm.epochPreparedCh <- e
				}

			case err := <-epochPrepareSub.Err():
				log.Error("Epoch prepared event subscription error", "err", err)
				rcm.stopFn()
				return

			case e := <-blockFinalizedWatchCh:
				if e != nil {
					rcm.blockFinalizedCh <- e
				}

			case err := <-blockFinalizedSub.Err():
				log.Error("Block finalized event subscription error", "err", err)
				rcm.stopFn()
				return

			case <-rcm.quit:
				return
			}
		}
	}()

	return nil
}

func (rcm *RootChainManager) runSubmitter() {
	plasmaBlockMinedEvents := rcm.eventMux.Subscribe(core.NewMinedBlockEvent{})
	defer plasmaBlockMinedEvents.Unsubscribe()

	blockSubmitEvents := make(chan *rootchain.RootChainBlockSubmitted)
	blockSubmitWatchOpts := &bind.WatchOpts{
		Start:   nil,
		Context: context.Background(),
	}
	blockFilterer, _ := rcm.rootchainContract.WatchBlockSubmitted(blockSubmitWatchOpts, blockSubmitEvents)
	defer blockFilterer.Unsubscribe()

	w, err := rcm.accountManager.Find(rcm.config.Operator)
	if err != nil {
		log.Error("Failed to get operator wallet", "err", err)
		return
	}

	var (
		nonce    = rcm.state.getNonce()
		gasPrice = rcm.state.gasPrice

		funcName string
		txHash   common.Hash
	)
	// adjust coordinates gas prices at reasonable prices.
	adjust := func(sufficient bool) {
		original := gasPrice
		if sufficient {
			gasPrice = new(big.Int).Mul(new(big.Int).Div(gasPrice, big.NewInt(4)), big.NewInt(3))
		} else {
			gasPrice = new(big.Int).Mul(new(big.Int).Div(gasPrice, big.NewInt(2)), big.NewInt(3))
		}
		rcm.state.gasPrice = gasPrice
		log.Info("Adjust gas price", "original", original, "new", gasPrice)
	}
	// submit sends transaction that submits ORB or NRB
	submit := func(name string, block *types.Block) common.Hash {
		input, err := rootchainContractABI.Pack(
			name,
			big.NewInt(int64(rcm.state.currentFork)),
			block.Header().Root,
			block.Header().TxHash,
			block.Header().ReceiptHash,
		)
		if err != nil {
			log.Error("Failed to pack "+name, "err", err)
		}
		submitTx := types.NewTransaction(nonce, rcm.config.RootChainContract, big.NewInt(int64(rcm.state.costNRB)), params.SubmitBlockGasLimit, gasPrice, input)
		signedTx, err := w.SignTx(rcm.config.Operator, submitTx, rootchainNetworkId)
		if err != nil {
			log.Error("Failed to sign "+funcName, "err", err)
		}
		err = rcm.backend.SendTransaction(context.Background(), signedTx)
		if err != nil {
			log.Error("Failed to send "+funcName, "err", err)
		}
		log.Info("Submit block to rootchain", "hash", signedTx.Hash())
		return signedTx.Hash()
	}

	for {
		// currentFork := big.NewInt(int64(rcm.state.currentFork))
		// lastBlock, err := rcm.lastBlock(currentFork)
		if err != nil {
			log.Error("Failed to get last block", "err", err)
			return
		}

		select {
		case ev := <-plasmaBlockMinedEvents.Chan():
			if ev == nil {
				return
			}

			// if the epoch is completed, stop mining operation and wait next epoch
			if rcm.minerEnv.Completed {
				rcm.miner.Stop()
			}
			log.Error("check")
			rcm.lock.Lock()

			if rcm.minerEnv.IsRequest {
				funcName = "submitORB"
			} else {
				funcName = "submitNRB"
			}
			blockInfo := ev.Data.(core.NewMinedBlockEvent)
			block := blockInfo.Block
			txHash = submit(funcName, block)

			pendingInterval := time.NewTicker(time.Duration(rcm.config.PendingInterval) * time.Second)
			for {
				select {
				case _, ok := <-pendingInterval.C:
					var mutex = &sync.Mutex{}

					if ok {
						mutex.Lock()

						log.Info("Mining submit block time out")
						// 이게 안 맞다.. -> 이거 carl님 요청해보기.
						// if block.Number().Cmp(new(big.Int).Sub(lastBlock, big.NewInt(1))) != 0 {
						// 	break
						// }
						log.Info("nonce", "nonce", nonce)
						if nonce == rcm.state.getNonce() {
							adjust(false)
						} else {
							nonce = rcm.state.getNonce()
						}
						txHash = submit(funcName, block)
						mutex.Unlock()
					}
				case <-blockSubmitEvents:
					pendingInterval.Stop()
					rcm.state.incNonce()
					rcm.lock.Unlock()

					receipt, err := rcm.backend.TransactionReceipt(context.Background(), txHash)
					log.Debug("signed tx receipt", "receipt", receipt, "hash", txHash.String())

					if err != nil {
						log.Error("Failed to send "+funcName, "err", err)
						break
					} else if receipt.Status == 0 {
						log.Error(funcName+" is reverted", "hash", txHash.Hex())
					} else {
						log.Info("Block is submitted", "func", funcName, "number", blockInfo.Block.NumberU64(), "hash", txHash.String(), "gasprice", gasPrice)
					}
					adjust(true)
					break
				}
			}
		case <-rcm.quit:
			return
		}
	}
}

func (rcm *RootChainManager) runHandlers() {
	for {
		select {
		case e := <-rcm.epochPreparedCh:
			if err := rcm.handleEpochPrepared(e); err != nil {
				log.Error("Failed to handle epoch prepared", "err", err)
			} else {
				rcm.blockchain.SetRootchainBlockNumber(e.Raw.BlockNumber)
			}
		case e := <-rcm.blockFinalizedCh:
			if err := rcm.handleBlockFinalzied(e); err != nil {
				log.Error("Failed to handle block finazlied", "err", err)
			} else {
				rcm.blockchain.SetRootchainBlockNumber(e.Raw.BlockNumber)
			}
		case <-rcm.quit:
			return
		}
	}
}

// handleEpochPrepared handles EpochPrepared event from RootChain contract after
// plasma chain is *SYNCED*.
func (rcm *RootChainManager) handleEpochPrepared(ev *rootchain.RootChainEpochPrepared) error {
	rcm.lock.Lock()
	defer rcm.lock.Unlock()

	e := *ev

	if e.EpochIsEmpty {
		log.Info("epoch is empty, jump to next epoch")
		return nil
	}

	length := new(big.Int).Add(new(big.Int).Sub(e.EndBlockNumber, e.StartBlockNumber), big.NewInt(1))

	// start miner
	log.Info("RootChain epoch prepared", "epochNumber", e.EpochNumber, "epochLength", length, "isRequest", e.IsRequest, "userActivated", e.UserActivated, "isEmpty", e.EpochIsEmpty, "ForkNumber", e.ForkNumber, "isRebase", e.Rebase)
	go rcm.miner.Start(params.Operator, &e, false)

	// prepare request tx for ORBs
	if e.IsRequest && !e.EpochIsEmpty {
		events := rcm.eventMux.Subscribe(core.NewMinedBlockEvent{})
		defer events.Unsubscribe()

		numORBs := new(big.Int).Sub(e.EndBlockNumber, e.StartBlockNumber)
		numORBs = new(big.Int).Add(numORBs, big.NewInt(1))

		bodies := make([]types.Transactions, 0, numORBs.Uint64()) // [][]types.Transaction

		currentFork := big.NewInt(int64(rcm.state.currentFork))
		epoch, err := rcm.getEpoch(currentFork, e.EpochNumber)
		if err != nil {
			return err
		}
		log.Debug("rcm.getEpoch", "epoch", epoch)

		// TODO: URE, ORE' should handle requestBlockId in a different way.
		requestBlockId := big.NewInt(int64(epoch.FirstRequestBlockId))

		log.Debug("Num Orbs", "epochNumber", e.EpochNumber, "numORBs", numORBs, "requestBlockId", requestBlockId, "e.EndBlockNumber", e.EndBlockNumber, "e.StartBlockNumber", e.StartBlockNumber)
		for blockNumber := e.StartBlockNumber; blockNumber.Cmp(e.EndBlockNumber) <= 0; {
			begin := time.Now()

			orb, err := rcm.rootchainContract.ORBs(baseCallOpt, requestBlockId)
			if err != nil {
				return err
			}

			numRequests := orb.RequestEnd - orb.RequestStart + 1
			log.Debug("Fetching ORB", "requestBlockId", requestBlockId, "numRequests", numRequests)

			body := make(types.Transactions, 0, numRequests)

			for requestId := orb.RequestStart; requestId <= orb.RequestEnd; {
				request, err := rcm.rootchainContract.EROs(baseCallOpt, big.NewInt(int64(requestId)))
				if err != nil {
					return err
				}

				log.Debug("Request fetched", "requestId", requestId, "hash", common.Bytes2Hex(request.Hash[:]), "request", request)

				var to common.Address
				var input []byte

				if request.IsTransfer {
					to = request.Requestor
				} else {
					to, _ = rcm.rootchainContract.RequestableContracts(baseCallOpt, request.To)
					input, err = requestableContractABI.Pack("applyRequestInChildChain",
						request.IsExit,
						big.NewInt(int64(requestId)),
						request.Requestor,
						request.TrieKey,
						request.TrieValue,
					)
					if err != nil {
						log.Error("Failed to pack applyRequestInChildChain", "err", err)
					}

					log.Debug("Request tx.data", "payload", input)
				}

				requestTx := types.NewTransaction(0, to, request.Value, params.RequestTxGasLimit, params.RequestTxGasPrice, input)

				log.Debug("Request Transaction", "tx", requestTx)

				eroBytes, err := rcm.rootchainContract.GetEROBytes(baseCallOpt, big.NewInt(int64(requestId)))
				if err != nil {
					log.Error("Failed to get ERO bytes", "err", err)
				}

				// TODO: check only in test
				if !bytes.Equal(eroBytes, requestTx.GetRlp()) {
					log.Error("ERO TX and request tx are different", "requestId", requestId, "eroBytes", common.Bytes2Hex(eroBytes), "requestTx.GetRlp()", common.Bytes2Hex(requestTx.GetRlp()))
				}

				body = append(body, requestTx)
				requestId += 1
			}

			log.Info("Request txs fetched", "blockNumber", blockNumber, "requestBlockId", requestBlockId, "numRequests", len(body), "elapsed", time.Since(begin))

			bodies = append(bodies, body)

			blockNumber = new(big.Int).Add(blockNumber, big.NewInt(1))
			requestBlockId = new(big.Int).Add(requestBlockId, big.NewInt(1))
		}

		var numMinedORBs uint64 = 0

		for numMinedORBs < numORBs.Uint64() {
			if err := rcm.txPool.EnqueueReqeustTxs(bodies[numMinedORBs]); err != nil {
				return err
			}

			log.Info("Waiting new request block mined event...")

			e := <-events.Chan()
			block := e.Data.(core.NewMinedBlockEvent).Block

			log.Info("New request block is mined", "block", block)

			if !block.IsRequest() {
				return errors.New("Invalid request block type.")
			}

			receipts := rcm.blockchain.GetReceiptsByHash(block.Hash())

			for _, receipt := range receipts {
				if receipt.Status == 0 {
					log.Error("Request transaction is reverted", "blockNumber", block.Number(), "hash", receipt.TxHash)
				}
			}

			numMinedORBs += 1
		}
	}

	return nil
}

func (rcm *RootChainManager) handleBlockFinalzied(ev *rootchain.RootChainBlockFinalized) error {
	rcm.lock.Lock()
	defer rcm.lock.Unlock()

	e := *ev

	log.Info("RootChain block finalized", "forkNumber", e.ForkNumber, "blockNubmer", e.BlockNumber)

	callerOpts := &bind.CallOpts{
		Pending: true,
		Context: context.Background(),
	}

	w, err := rcm.accountManager.Find(rcm.config.Operator)
	if err != nil {
		log.Error("Failed to get operator wallet", "err", err)
	}

	block, err := rcm.rootchainContract.GetBlock(callerOpts, e.ForkNumber, e.BlockNumber)
	if err != nil {
		return err
	}

	if block.IsRequest {
		invalidExits := rcm.invalidExits[e.ForkNumber.Uint64()][e.BlockNumber.Uint64()]
		for i := 0; i < len(invalidExits); i++ {

			var proofs []byte
			for j := 0; j < len(invalidExits[i].proof); j++ {
				proof := invalidExits[i].proof[j].Bytes()
				proofs = append(proofs, proof...)
			}
			// TODO: ChallengeExit receipt check
			input, err := rootchainContractABI.Pack("challengeExit", e.ForkNumber, e.BlockNumber, big.NewInt(invalidExits[i].index), invalidExits[i].receipt.GetRlp(), proofs)
			if err != nil {
				log.Error("Failed to pack challengeExit", "error", err)
			}

			Nonce := rcm.state.getNonce()
			challengeTx := types.NewTransaction(Nonce, rcm.config.RootChainContract, big.NewInt(0), params.SubmitBlockGasLimit, params.SubmitBlockGasPrice, input)

			signedTx, err := w.SignTx(rcm.config.Operator, challengeTx, rootchainNetworkId)
			if err != nil {
				log.Error("Failed to sign challengeTx", "err", err)
			}

			err = rcm.backend.SendTransaction(context.Background(), signedTx)
			if err != nil {
				log.Error("Failed to send challengeTx", "err", err)
			} else {
				log.Info("challengeExit is submitted", "exit request number", invalidExits[i].index, "hash", signedTx.Hash().Hex())
			}
		}
	}

	return nil
}

func (rcm *RootChainManager) runDetector() {
	events := rcm.eventMux.Subscribe(core.NewMinedBlockEvent{})
	defer events.Unsubscribe()

	caller, err := rootchain.NewRootChainCaller(rcm.config.RootChainContract, rcm.backend)
	if err != nil {
		log.Warn("failed to make new root chain caller", "error", err)
		return
	}

	// TODO: check callOpts first if caller doesn't work.
	callerOpts := &bind.CallOpts{
		Pending: false,
		Context: context.Background(),
	}

	for {
		select {
		case ev := <-events.Chan():
			rcm.lock.Lock()
			if rcm.minerEnv.IsRequest {
				var invalidExitsList invalidExits

				forkNumber, err := caller.CurrentFork(callerOpts)
				if err != nil {
					log.Warn("failed to get current fork number", "error", err)
				}

				if rcm.invalidExits[forkNumber.Uint64()] == nil {
					rcm.invalidExits[forkNumber.Uint64()] = make(map[uint64]invalidExits)
				}

				blockInfo := ev.Data.(core.NewMinedBlockEvent)
				blockNumber := blockInfo.Block.Number()
				receipts := rcm.blockchain.GetReceiptsByHash(blockInfo.Block.Hash())

				// TODO: should check if the request[i] is enter or exit request. Undo request will make posterior enter request.
				for i := 0; i < len(receipts); i++ {
					if receipts[i].Status == types.ReceiptStatusFailed {
						invalidExit := &invalidExit{
							forkNumber:  forkNumber,
							blockNumber: blockNumber,
							receipt:     receipts[i],
							index:       int64(i),
							proof:       types.GetMerkleProof(receipts, i),
						}
						invalidExitsList = append(invalidExitsList, invalidExit)

						log.Info("Invalid Exit Detected", "invalidExit", invalidExit, "forkNumber", forkNumber, "blockNumber", blockNumber)
					}
				}
				rcm.invalidExits[forkNumber.Uint64()][blockNumber.Uint64()] = invalidExitsList
			}
			rcm.lock.Unlock()

		case <-rcm.quit:
			return
		}
	}
}

func (rcm *RootChainManager) getEpoch(forkNumber, epochNumber *big.Int) (*PlasmaEpoch, error) {
	b, err := rcm.rootchainContract.GetEpoch(baseCallOpt, forkNumber, epochNumber)

	if err != nil {
		return nil, err
	}

	return newPlasmaEpoch(b), nil
}
func (rcm *RootChainManager) getBlock(forkNumber, blockNumber *big.Int) (*PlasmaBlock, error) {
	b, err := rcm.rootchainContract.GetBlock(baseCallOpt, forkNumber, blockNumber)

	if err != nil {
		return nil, err
	}

	return newPlasmaBlock(b), nil
}
func (rcm *RootChainManager) lastBlock(forkNumber *big.Int) (*big.Int, error) {
	num, err := rcm.rootchainContract.LastBlock(baseCallOpt, forkNumber)
	if err != nil {
		return nil, err
	}
	return num, nil
}

// pingBackend checks rootchain backend is alive.
func (rcm *RootChainManager) pingBackend() {
	ticker := time.NewTicker(3 * time.Second)

	for {
		select {
		case <-ticker.C:
			if _, err := rcm.backend.SyncProgress(context.Background()); err != nil {
				log.Error("Rootchain provider doesn't respond", "err", err)
				ticker.Stop()
				rcm.stopFn()
				return
			}
		case <-rcm.quit:
			ticker.Stop()
			return
		}
	}
}
