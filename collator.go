package main

import (
    "math/big"
    "sync"
    "errors"

    "github.com/ethereum/go-ethereum/core/types"
    "github.com/ethereum/go-ethereum/common"
    "github.com/ethereum/go-ethereum/miner"
)

type MevCollator struct {
    maxMergedBundles uint
    bundleMu sync.Mutex
    bundles []MevBundle
}

type MevBundle struct {
    Transactions types.Transactions
    BlockNumber       *big.Int
    MinTimestamp      uint64
    MaxTimestamp      uint64
    RevertingTxHashes []common.Hash
}

type simulatedBundle struct {
    mevGasPrice       *big.Int
    totalEth          *big.Int
    ethSentToCoinbase *big.Int
    totalGasUsed      uint64
    originalBundle    MevBundle
}

type bundlerWork struct {
    blockState miner.BlockState
    wg *sync.WaitGroup
    simulatedBundles []simulatedBundle
    maxMergedBundles uint
	pendingTxs map[common.Address]types.Transactions
	commitMu *sync.Mutex
}

type bundleWorker struct {
    exitCh chan struct{}
    newWorkCh chan bundlerWork
}

func containsHash(arr []common.Hash, match common.Hash) bool {
    for _, elem := range arr {
        if elem == match {
            return true
        }
    }
    return false
}

var (
	ErrBundleTxReverted = errors.New("bundle tx was reverted (not in allowed reverted list)")
)

// eligibleBundles returns a list of bundles valid for the given blockNumber/blockTimestamp
// also prunes bundles that are outdated
func (c *MevCollator) eligibleBundles(blockNumber *big.Int, blockTimestamp uint64) []MevBundle {
        c.bundleMu.Lock()
        defer c.bundleMu.Unlock()

    // returned values
    var ret []MevBundle
    // rolled over values
    var bundles []MevBundle

    for _, bundle := range c.bundles {
        // Prune outdated bundles
        if (bundle.MaxTimestamp != 0 && blockTimestamp > bundle.MaxTimestamp) || blockNumber.Cmp(bundle.BlockNumber) > 0 {
            continue
        }

        // Roll over future bundles
        if (bundle.MinTimestamp != 0 && blockTimestamp < bundle.MinTimestamp) || blockNumber.Cmp(bundle.BlockNumber) < 0 {
            bundles = append(bundles, bundle)
            continue
        }

        // return the ones which are in time
        ret = append(ret, bundle)
        // keep the bundles around internally until they need to be pruned
        bundles = append(bundles, bundle)
    }

    c.bundles = bundles
    return ret
}

func computeBundleGas(bundle MevBundle, bs miner.BlockState, pendingTxs map[common.Address]types.Transactions) (simulatedBundle, error) {
    state := bs.State()
    header := bs.Header()
	signer := bs.Signer()

    var totalGasUsed uint64 = 0
    var tempGasUsed uint64
    gasFees := new(big.Int)
    ethSentToCoinbase := new(big.Int)

    for _, tx := range bundle.Transactions {
        coinbaseBalanceBefore := state.GetBalance(bs.Etherbase())

        err, receipt := bs.AddTransaction(tx)
        if err != nil && errors.Is(err, miner.ErrInterrupt) {
            return simulatedBundle{}, err
        }
        if receipt.Status == types.ReceiptStatusFailed && !containsHash(bundle.RevertingTxHashes, receipt.TxHash) {
            return simulatedBundle{}, ErrBundleTxReverted
        }
		totalGasUsed += receipt.GasUsed

        from, err := types.Sender(signer, tx)
        if err != nil {
            return simulatedBundle{}, err
        }
        txInPendingPool := false
        if accountTxs, ok := pendingTxs[from]; ok {
            // check if tx is in pending pool
            txNonce := tx.Nonce()

            for _, accountTx := range accountTxs {
                if accountTx.Nonce() == txNonce {
                    txInPendingPool = true
                    break
                }
            }
        }
        gasUsed := new(big.Int).SetUint64(receipt.GasUsed)
        gasPrice, err := tx.EffectiveGasTip(header.BaseFee)
        if err != nil {
            return simulatedBundle{}, err
        }
        gasFeesTx := gasUsed.Mul(gasUsed, gasPrice)
        coinbaseBalanceAfter := state.GetBalance(bs.Etherbase)
        coinbaseDelta := big.NewInt(0).Sub(coinbaseBalanceAfter, coinbaseBalanceBefore)
        coinbaseDelta.Sub(coinbaseDelta, gasFeesTx)
        ethSentToCoinbase.Add(ethSentToCoinbase, coinbaseDelta)

        if !txInPendingPool {
            // If tx is not in pending pool, count the gas fees
            gasFees.Add(gasFees, gasFeesTx)
        }
    }
    totalEth := new(big.Int).Add(ethSentToCoinbase, gasFees)

    return simulatedBundle{
        mevGasPrice:       new(big.Int).Div(totalEth, new(big.Int).SetUint64(totalGasUsed)),
        totalEth:          totalEth,
        ethSentToCoinbase: ethSentToCoinbase,
        totalGasUsed:      totalGasUsed,
        originalBundle:    bundle,
    }, nil
}

// fill the block with as many bundles as the worker can add
// return the BlockState 
func mergeBundles(work bundlerWork) (miner.BlockState, uint, uint, *big.Int, *big.Int, error) {
	bs := work.blockState
    if len(work.simulatedBundles) == 0 {
        return bs, 0, 0, big.NewInt(0), big.NewInt(0), nil
    }

    beforeBs := bs.Copy()
    resultBs := bs

    totalEth := big.NewInt(0)
    ethSentToCoinbase := big.NewInt(0)
    var numMergedBundles uint = 0

    for _, bundle := range work.simulatedBundles {
        // the floor gas price is 99/100 what was simulated at the top of the block
        floorGasPrice := new(big.Int).Mul(bundle.mevGasPrice, big.NewInt(99))
        floorGasPrice = floorGasPrice.Div(floorGasPrice, big.NewInt(100))

        simmed, err := computeBundleGas(bundle, resultBs, work.pendingTxs)
        if err != nil {
            if err.Is(miner.ErrInterrupt) {
                return nil, 0, 0, nil, nil, ErrInterrupt
            } else {
                beforeBs = resultBs.Copy()
                continue
            }
        } else if simmed.mevGasPrice.Cmp(floorGasPrice) <= 0 {
            resultBs = beforeBs.Copy()
            continue
        }

        totalEth.Add(totalEth, simmed.totalEth)
        ethSentToCoinbase.Add(ethSentToCoinbase, simmed.ethSentToCoinbase)
        numMergedBundles++
        if numMergedBundles >= work.maxMergedBundles {
            break
        }
    }

    return resultBs, numMergedBundles, numTxs, totalEth, ethSentToCoinbase, nil
}

func submitTransactions(bs miner.BlockState, txs *types.TransactionsByPriceAndNonce) bool {
    header := bs.Header()
	for {
		// If we don't have enough gas for any further transactions then we're done
		available := header.GasLimit - header.GasUsed
		if available < params.TxGas {
			break
		}
		// Retrieve the next transaction and abort if all done
		tx := txs.Peek()
		if tx == nil {
			break
		}
		// Enough space for this tx?
		if available < tx.Gas() {
			txs.Pop()
			continue
		}
		from, _ := types.Sender(bs.Signer(), tx)

		err, _ := bs.AddTransaction(tx)
		switch {
		case errors.Is(err, ErrGasLimitReached):
			// Pop the current out-of-gas transaction without shifting in the next from the account
			txs.Pop()

		case errors.Is(err, ErrNonceTooLow):
			// New head notification data race between the transaction pool and miner, shift
			txs.Shift()

		case errors.Is(err, ErrNonceTooHigh):
			// Reorg notification data race between the transaction pool and miner, skip account =
			txs.Pop()

		case errors.Is(err, nil):
			// Everything ok, collect the logs and shift in the next transaction from the same account
			txs.Shift()

		case errors.Is(err, ErrTxTypeNotSupported):
			// Pop the unsupported transaction without shifting in the next from the account
			txs.Pop()
		case errors.Is(err, ErrInterrupt):
			return true
		default:
			// Strange error, discard the transaction and get the next in line (note, the
			// nonce-too-high clause will prevent us from executing in vain).
			txs.Shift()
		}
	}

	return false
}

func fillTransactions(bs miner.BlockState, txs map[common.Address]types.Transactions) bool {
    header := bs.Header()
	txs, err := pool.Pending(true)
	if err != nil {
		log.Error("could not get pending transactions from the pool", "err", err)
		return true
	}
	if len(txs) == 0 {
		return true
	}
	// Split the pending transactions into locals and remotes
	localTxs, remoteTxs := make(map[common.Address]types.Transactions), txs
	for _, account := range pool.Locals() {
		if accountTxs := remoteTxs[account]; len(accountTxs) > 0 {
			delete(remoteTxs, account)
			localTxs[account] = accountTxs
		}
	}
	if len(localTxs) > 0 {
		if submitTransactions(bs, types.NewTransactionsByPriceAndNonce(bs.Signer(), localTxs, header.BaseFee)) {
			return false
		}
	}
	if len(remoteTxs) > 0 {
		if submitTransactions(bs, types.NewTransactionsByPriceAndNonce(bs.Signer(), remoteTxs, header.BaseFee)) {
			return false
		}
	}
	return true
}
func (c *bundleWorker) workerMainLoop() {
    for {
        select {
        case work := <-newWorkCh:
            // TODO move this block into its own function and do defer on work.wg.Done()
            bs, numTxs, numBundles, totalEth, profit, err := mergeBundles(work)
            if err != nil {
                work.wg.Done()
                continue
            }

            if numTxs == 0 && work.maxMergedBundles != 0 {
                work.wg.Done()
                continue
            }

            if work.maxMergedBundles != 0 && numBundles != work.maxMergedBundles {
                work.wg.Done()
                continue
            }

            if !fillTransactions(bs, work.txs) {
                work.wg.Done()
                continue
            }
            work.commitMu.Lock()
            if work.profit.Cmp(work.bestProfit) > 0 {
                work.bestProfit.Set(work.profit)
                bs.Commit()
            }
            work.commitMu.Unlock()
            work.wg.Done()
        case <-exitCh:
            return
        }
    }
}

func simulateBundles(bs miner.BlockState, b []MevBundle) (*[]simulatedBundle, error) {
    result = []simulatedBundle{}

    if len(b) == 0 {
        return &[]simulatedBundle{}, nil
    }

    bsBefore := bs.Copy()

    for _, bundle := range b {
        simulated, err := computeBundleGas(bsBefore, bundle)
        bsBefore = bs.Copy()
        if err != nil {
            if err.Is(ErrInterrupt) {
                return err
            } else {
                log.Error("failed to simulate bndle", "err", err)
                continue
            }
        }
        result = append(result, simulated)

        // TODO is copying the blockState before each call to bundleGas and resetting after
        // better than reverting all txs ?
        // ---
        // reset the blockState back to what it was before this call to computeBundleGas
        for i, _ := range bundle.Transactions {
            if !bs.RevertTransaction() {
                return nil, errors.New("failed to revert transaction")
            }
        }
    }
    return &result
}

func (c *MevCollator) CollateBlock(bs miner.BlockState, pool miner.Pool) {
    // create a copy of the BlockState and send it to each worker.
    if c.counter == math.MaxUint64 {
        c.counter = 0
    } else {
        c.counter++
    }

    // TODO signal to our "normal" worker to start building a normal block

	header := bs.Header()
    bundles, err := c.eligibleBundles(header.BlockNumber, header.Timestamp)
    if err != nil {
        log.Error("failed to fetch eligible bundles", "err", err)
        return
    }

    var bundleBlocksExpected uint
    var wg sync.WaitGroup
    wg.Add(1)
    bestProfit := big.NewInt(0)
    commitMu := sync.Mutex{}

    pendingTxs, err := pool.Pending(true)
    _ = err // can actually ignore err here because pool.Pending never returns an error

    c.bundleWorkers[0].newWorkCh <- bundlerWork{wg: &wg, blockState: bs.Copy(), simulatedBundles: nil, counter: counter, bestProfit: bestProfit, commitMu: &commitMu, pendingTxs: pendingTxs}

    if len(bundles) > 0 {
        simulatedBundles := make([]simulatedBundle, len(bundles))
        if len(bundles) > maxMergedBundles {
            bundleBlocksExpected := maxMergedBundles
        } else {
            bundleBlocksExpected := len(bundles)
        }
        wg.Add(bundleBlocksExpected)

        // 1) simulate each bundle in this go-routine (TODO see if doing it in parallel is worth it in a future iteration)
        simulatedBundles, err := simulateBundles(bs, bundles)
		if err == nil {
            sort.SliceStable(simulatedBundles, func(i, j int) bool {
                return simulatedBundles[j].mevGasPrice.Cmp(simulatedBundles[i].mevGasPrice) < 0
            })

            // 2) concurrently build 0..N-1 blocks with 0..N-1 max merged bundles
            for i := 0; i < bundleBlocksExpected; i++ {
                c.bundleWorkers[i + 1].newWorkCh <- bundlerWork{blockState: bs.Copy(), wg: &wg, simulatedBundles: simulatedBundles, maxMergedBundles: i + 1, bestProfit: bestProfit, commitMu: &commitMu, pendingTxs: pendingTxs}
            }
        } else if err == ErrInterrupt {
            return
        }
    }
    wg.Wait()
}

func (c *MevCollator) Start() {
    for i := 0; i < c.maxMergedBundles; i++ {
        workers = append(workers, bundleWorker{
            exitCh: c.closeCh,
            newWorkCh: make(chan bundlerWork),
        })

        go workers[i].workerMainLoop()
    }
}

func (c *MevCollator) Close() {
    close(c.closeCh)
}
