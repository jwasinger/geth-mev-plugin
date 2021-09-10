package main

import (
	"errors"
	"math/big"
	"sort"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/miner"
	"github.com/ethereum/go-ethereum/params"
)

type MevCollator struct {
	maxMergedBundles uint
	bundleMu         sync.Mutex
	bundles          []MevBundle
	workers          []bundleWorker
	pool             miner.Pool
	closeCh          chan struct{}
}

type MevBundle struct {
	Transactions      types.Transactions
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
	blockState       miner.BlockState
	wg               *sync.WaitGroup
	simulatedBundles []simulatedBundle
	maxMergedBundles uint
	commitMu         *sync.Mutex
	bestProfit       *big.Int
}

type bundleWorker struct {
	id               int
	exitCh           chan struct{}
	newWorkCh        chan bundlerWork
	maxMergedBundles uint
	pool             miner.Pool
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
		coinbaseBalanceAfter := state.GetBalance(bs.Etherbase())
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
func mergeBundles(work bundlerWork, pendingTxs map[common.Address]types.Transactions, locals []common.Address) (miner.BlockState, uint, uint, *big.Int, *big.Int, error) {
	bs := work.blockState
	if len(work.simulatedBundles) == 0 {
		return bs, 0, 0, big.NewInt(0), big.NewInt(0), nil
	}

	beforeBs := bs.Copy()
	resultBs := bs

	totalEth := big.NewInt(0)
	ethSentToCoinbase := big.NewInt(0)
	var numMergedBundles uint = 0
	var numTxs uint = 0

	for _, bundle := range work.simulatedBundles {
		// the floor gas price is 99/100 what was simulated at the top of the block
		floorGasPrice := new(big.Int).Mul(bundle.mevGasPrice, big.NewInt(99))
		floorGasPrice = floorGasPrice.Div(floorGasPrice, big.NewInt(100))

		simmed, err := computeBundleGas(bundle.originalBundle, resultBs, pendingTxs)
		if err != nil {
			if errors.Is(err, miner.ErrInterrupt) {
				return nil, 0, 0, nil, nil, miner.ErrInterrupt
			} else {
				beforeBs = resultBs.Copy()
				continue
			}
		} else if simmed.mevGasPrice.Cmp(floorGasPrice) <= 0 {
			resultBs = beforeBs.Copy()
			continue
		}

		numTxs += uint(len(simmed.originalBundle.Transactions))

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

		err, _ := bs.AddTransaction(tx)
		switch {
		case errors.Is(err, miner.ErrGasLimitReached):
			// Pop the current out-of-gas transaction without shifting in the next from the account
			txs.Pop()

		case errors.Is(err, miner.ErrNonceTooLow):
			// New head notification data race between the transaction pool and miner, shift
			txs.Shift()

		case errors.Is(err, miner.ErrNonceTooHigh):
			// Reorg notification data race between the transaction pool and miner, skip account =
			txs.Pop()

		case errors.Is(err, nil):
			// Everything ok, collect the logs and shift in the next transaction from the same account
			txs.Shift()

		case errors.Is(err, miner.ErrTxTypeNotSupported):
			// Pop the unsupported transaction without shifting in the next from the account
			txs.Pop()
		case errors.Is(err, miner.ErrInterrupt):
			return true
		default:
			// Strange error, discard the transaction and get the next in line (note, the
			// nonce-too-high clause will prevent us from executing in vain).
			txs.Shift()
		}
	}

	return false
}

func fillTransactions(bs miner.BlockState, pendingTxs map[common.Address]types.Transactions, locals []common.Address) bool {
	header := bs.Header()

	if len(pendingTxs) == 0 {
		return true
	}

	// Split the pending transactions into locals and remotes
	localTxs, remoteTxs := make(map[common.Address]types.Transactions), pendingTxs
	for _, account := range locals {
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
		case work := <-c.newWorkCh:
			pendingTxs, _ := c.pool.Pending(true)
			locals := c.pool.Locals()

			// TODO move this block into its own function and do defer on work.wg.Done()
			bs, numTxs, numBundles, totalEth, profit, err := mergeBundles(work, pendingTxs, locals)
			if err != nil {
				work.wg.Done()
				continue
			}
			_ = totalEth

			if numTxs == 0 && work.maxMergedBundles != 0 {
				work.wg.Done()
				continue
			}

			if work.maxMergedBundles != 0 && numBundles != work.maxMergedBundles {
				work.wg.Done()
				continue
			}

			if numTxs == 0 && len(pendingTxs) == 0 {
				work.wg.Done()
				continue
			}

			// TODO add tx-fees to profit
			if !fillTransactions(bs, pendingTxs, locals) {
				work.wg.Done()
				continue
			}

			work.commitMu.Lock()
			if profit.Cmp(work.bestProfit) >= 0 {
				work.bestProfit.Set(profit)
				bs.Commit()
			}
			work.commitMu.Unlock()
			work.wg.Done()
		case <-c.exitCh:
			return
		}
	}
}

func simulateBundles(bs miner.BlockState, b []MevBundle, pendingTxs map[common.Address]types.Transactions, locals []common.Address) ([]simulatedBundle, error) {
	result := []simulatedBundle{}

	if len(b) == 0 {
		return []simulatedBundle{}, nil
	}

	bsBefore := bs.Copy()

	for _, bundle := range b {
		simulated, err := computeBundleGas(bundle, bsBefore, pendingTxs)
		bsBefore = bs.Copy()
		if err != nil {
			if errors.Is(miner.ErrInterrupt, err) {
				return nil, err
			} else {
				log.Error("failed to simulate bndle", "err", err)
				continue
			}
		}
		result = append(result, simulated)
	}
	return result, nil
}

func (c *MevCollator) CollateBlock(bs miner.BlockState) {
	// TODO signal to our "normal" worker to start building a normal block
	header := bs.Header()
	bundles := c.eligibleBundles(header.Number, header.Time)

	var bundleBlocksExpected uint
	var wg sync.WaitGroup
	wg.Add(1)
	bestProfit := big.NewInt(0)
	commitMu := sync.Mutex{}

	pendingTxs, _ := c.pool.Pending(true)
	locals := c.pool.Locals()

	// TODO does all these simultaneous calls to Pool.Pending() cause significant overhead?
	c.workers[0].newWorkCh <- bundlerWork{wg: &wg, blockState: bs.Copy(), simulatedBundles: nil, bestProfit: bestProfit, commitMu: &commitMu}

	if len(bundles) > 0 {
		simulatedBundles := make([]simulatedBundle, len(bundles))
		if len(bundles) > int(c.maxMergedBundles) {
			bundleBlocksExpected = c.maxMergedBundles
		} else {
			bundleBlocksExpected = uint(len(bundles))
		}
		wg.Add(int(bundleBlocksExpected))

		simulatedBundles, err := simulateBundles(bs, bundles, pendingTxs, locals)
		if err == nil {
			sort.SliceStable(simulatedBundles, func(i, j int) bool {
				return simulatedBundles[j].mevGasPrice.Cmp(simulatedBundles[i].mevGasPrice) < 0
			})

			for i := 0; i < int(bundleBlocksExpected); i++ {
				c.workers[i+1].newWorkCh <- bundlerWork{blockState: bs.Copy(), wg: &wg, simulatedBundles: simulatedBundles, bestProfit: bestProfit, commitMu: &commitMu}
			}
		} else if err == miner.ErrInterrupt {
			return
		}
	}
	wg.Wait()
}

func (c *MevCollator) Start(pool miner.Pool) {
	c.pool = pool
	for i := 0; i < int(c.maxMergedBundles); i++ {
		worker := bundleWorker{
			exitCh:           c.closeCh,
			newWorkCh:        make(chan bundlerWork),
			maxMergedBundles: uint(i),
			pool:             pool,
			id:               i,
		}

		c.workers = append(c.workers, worker)
		go worker.workerMainLoop()
	}
}

func (c *MevCollator) Close() {
	close(c.closeCh)
}
