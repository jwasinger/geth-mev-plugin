type MEVCollator struct {
    maxMergedBundles uint
    bundleMu sync.Mutex
    bundles []MevBundle
}

type MevBundle struct {
    Txs               Transactions
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

var (
    ErrBundleTxReverted = errors.New("bundle tx reverted")
)

func computeBundleGas(bundle MEVBundle, bs BlockState) simulatedBundle, err {
    state := bs.State()
    header := bs.Header()
	signer := bs.Signer()

    var totalGasUsed uint64 = 0
    var tempGasUsed uint64
    gasFees := new(big.Int)
    ethSentToCoinbase := new(big.Int)

    for _, tx := range bundle.Transactions {
        coinbaseBalanceBefore := bs.GetBalance(bs.Coinbase())

        err, receipt := bs.AddTransaction(tx)
        if err != nil && err.Is(ErrInterrupt) {
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
        gasPrice, err := tx.EffectiveGasTip(header.BaseFee())
        if err != nil {
            return simulatedBundle{}, err
        }
        gasFeesTx := gasUsed.Mul(gasUsed, gasPrice)
        coinbaseBalanceAfter := state.GetBalance(bs.Coinbase())
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
func mergeBundles(bs BlockState, work BundlerWork) BlockState, error {
    if len(work.simulatedBundles) == 0 {
        return bs
    }

    beforeBs := bs.Copy()
    resultBs := bs

    for _, bundle := range work.simulatedBundles {
        // the floor gas price is 99/100 what was simulated at the top of the block
        floorGasPrice := new(big.Int).Mul(bundle.mevGasPrice, big.NewInt(99))
        floorGasPrice = floorGasPrice.Div(floorGasPrice, big.NewInt(100))

        simmed, err := computeBundleGas(resultBs, bundle)
        if err != nil {
            if err.Is(ErrInterrupt) {
                return nil, ErrInterrupt
            } else {
                resultBs = beforeBs
                continue
            }
        } else if simmed.mevGasPrice.Cmp(floorGasPrice) <= 0 {
            resultBs = beforeBs
            continue
        }
    }

    return resultBs, nil
}

func submitTransactions(bs BlockState, txs *types.TransactionsByPriceAndNonce) bool {
    headerView := bs.Header()
	for {
		// If we don't have enough gas for any further transactions then we're done
		available := headerView.GasLimit() - headerView.GasUsed()
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

func fillTransactions(bs BlockState, pool Pool, state ReadOnlyState) bool {
    headerView := bs.Header()
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
		if submitTransactions(bs, types.NewTransactionsByPriceAndNonce(bs.Signer(), localTxs, headerView.BaseFee())) {
			return false
		}
	}
	if len(remoteTxs) > 0 {
		if submitTransactions(bs, types.NewTransactionsByPriceAndNonce(bs.Signer(), remoteTxs, headerView.BaseFee())) {
			return false
		}
	}
	return true
}
func bundlerMainLoop(collator *MevCollator) {
    for {
        select {
        case bundlerWork := <-newWorkCh:
            if err := mergeBundles(bundlerWork); err != nil {
                continue
            }
            if !fillTransactions(bundlerWork) {
                continue
            }
            collator.SuggestBlock(bundlerWork)
        case _ := <-exitCh:
            return
        }
    }
}

func simulateBundles(bs BlockState, b Bundles) *[]simulatedBundle, error {
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

func (c *MEVCollator) CollateBlock(bs BlockState, pool Pool, state ReadOnlyState) {
    // create a copy of the BlockState and send it to each worker.
    if c.counter == math.MaxUint64 {
        c.counter = 0
    } else {
        c.counter++
    }

    // TODO signal to our "normal" worker to start building a normal block

    bundles, err := c.eligibleBundles(bs)
    if err != nil {
        log.Error("failed to fetch eligible bundles", "err", err)
        return
    }

    bundlesExpeced := 0

    c.bundleWorkers[0].newWorkCh <- bundlerWork{blockState: bs.Copy(), simulatedBundles: , counter: counter}

    if len(bundles) > 0 {
        simulatedBundles := make([]simulatedBundle, len(bundles))

        // 1) simulate each bundle in this go-routine (TODO see if doing it in parallel is worth it in a future iteration)
        simulatedBundles, err := simulateBundles(bs, bundles)
		if err == nil {
            bundlesExpeced := len(bundles)

            sort.SliceStable(simulatedBundles, func(i, j int) bool {
                return simulatedBundles[j].mevGasPrice.Cmp(simulatedBundles[i].mevGasPrice) < 0
            })

            // 2) concurrently build 0..N-1 blocks with 0..N-1 max merged bundles
            for i := 0; i < len(bundles); i++ {
                c.bundleWorkers[i].newWorkCh <- bundlerWork{blockState: bs.Copy(), simulatedBundles: nil, maxMergedBundles: 0}
            }
        } else if err == ErrInterrupt {
            return
        }
    }

    bundlesReceived := 0
    for {
        select {
        case resp := <-c.workResponseCh:
            // don't care about responses that are stale:
            //  responses from previous calls to CollateBlock
            if resp.counter != counter {
                break
            }
            // workers set the blockState to nil in the response if they were
            // interrupted (recommit interrupt, new chain canon head received)
            if resp.blockState == nil {
                return
            }
            bundlesReceived++
            // only interrupt the sealer if a more profitable block is found
            if bestProfit.Cmp(resp.profit) < 0 {
                bestProfit.Set(resp.profit) // copy here just to be overly safe until POC is working
                resp.blockState.Commit()
            }
            // we're done when all the eligible bundle blocks have been returned
            // and the standard-strategy block (zero bundles) has been returned
            if bundlesReceived == bundlesExpected + 1 {
                return
            }
        case _ := <-c.closeCh:
            return
        }
    }
}

func (c *MEVCollator) Start() {
    for i := 0; i < c.maxMergedBundles; i++ {
        worker := bundleWorker{

        }

        go c.collatorMainLoop()
    }
}

func (c *MEVCollator) Close() {
    close(c.closeCh)
}
