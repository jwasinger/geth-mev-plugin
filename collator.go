package main

import (
	"context"
	"errors"
	"math/big"
	"sort"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/miner/collator"
)

var (
	errInterrupted = errors.New("work-cycle interrupted by miner: new head block received")
)

type MevCollator struct {
	maxMergedBundles uint
	bundleMu         sync.Mutex
	bundles          []MevBundle
	workers          []bundleWorker
	pool             collator.Pool

	commitMu sync.Mutex
	// these values are used per-work-cycle
	lastParentHash common.Hash
	bestProfit     *big.Int
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

type MergedBundlesStats struct {
	numTxs     uint
	numBundles uint
	totalEth   *big.Int
	profit     *big.Int
}

type bundleWork struct {
	work    collator.BlockCollatorWork
	bundles []MevBundle
}

type bundleWorker struct {
	id               int
	newWorkCh        chan *bundleWork
	maxMergedBundles uint
	collator         *MevCollator
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
	ErrBundleTxFailed   = errors.New("failed to apply tx from bundle")
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

func applyBundle(ctx context.Context, bundle MevBundle, bs collator.BlockState, pendingTxs map[common.Address]types.Transactions) (*simulatedBundle, error) {
	state := bs.State()
	header := bs.Header()
	signer := bs.Signer()

	var totalGasUsed uint64 = 0
	gasFees := new(big.Int)
	ethSentToCoinbase := new(big.Int)

	for _, tx := range bundle.Transactions {
		select {
		case <-ctx.Done():
			return nil, errInterrupted
		default:
		}

		coinbaseBalanceBefore := state.GetBalance(bs.Etherbase())
		receipt, err := bs.AddTransaction(tx)
		if err != nil {
			return nil, ErrBundleTxFailed
		}

		if receipt.Status == types.ReceiptStatusFailed && !containsHash(bundle.RevertingTxHashes, receipt.TxHash) {
			return nil, ErrBundleTxReverted
		}
		totalGasUsed += receipt.GasUsed

		from, err := types.Sender(signer, tx)
		if err != nil {
			return nil, err
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
			return nil, err
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

	return &simulatedBundle{
		mevGasPrice:       new(big.Int).Div(totalEth, new(big.Int).SetUint64(totalGasUsed)),
		totalEth:          totalEth,
		ethSentToCoinbase: ethSentToCoinbase,
		totalGasUsed:      totalGasUsed,
		originalBundle:    bundle,
	}, nil
}

// fill the block with as many bundles as the worker can add
func mergeBundles(work bundleWork, simulatedBundles []simulatedBundle, pendingTxs map[common.Address]types.Transactions, locals []common.Address, maxMergedBundles uint) (*MergedBundlesStats, error) {
	result := &MergedBundlesStats{
		totalEth: big.NewInt(0),
		profit:   big.NewInt(0),
	}

	if len(simulatedBundles) == 0 {
		return result, nil
	}

	totalEth := big.NewInt(0)
	ethSentToCoinbase := big.NewInt(0)
	var numMergedBundles uint = 0
	var numTxs uint = 0

	for _, bundle := range simulatedBundles {
		// the floor gas price is 99/100 what was simulated at the top of the block
		floorGasPrice := new(big.Int).Mul(bundle.mevGasPrice, big.NewInt(99))
		floorGasPrice = floorGasPrice.Div(floorGasPrice, big.NewInt(100))

		blockCopy := work.work.Block.Copy()
		simmed, err := applyBundle(work.work.Ctx, bundle.originalBundle, work.work.Block, pendingTxs)
		if err != nil {
			if errors.Is(err, errInterrupted) {
				return nil, err
			} else {
				work.work.Block = blockCopy
				continue
			}
		} else if simmed.mevGasPrice.Cmp(floorGasPrice) <= 0 {
			work.work.Block = blockCopy
			continue
		}

		numTxs += uint(len(simmed.originalBundle.Transactions))

		totalEth.Add(totalEth, simmed.totalEth)
		ethSentToCoinbase.Add(ethSentToCoinbase, simmed.ethSentToCoinbase)
		result.numBundles++
		result.numTxs += uint(len(bundle.originalBundle.Transactions))
		if numMergedBundles >= maxMergedBundles {
			break
		}
	}

	return result, nil
}

func (w *bundleWorker) bundleWorkMainLoop() {
	for {
		select {
		case work := <-w.newWorkCh:
			if work == nil {
				// channel was closed signalling client exit
				return
			}

			pendingTxs, _ := w.collator.pool.Pending(true)
			locals := w.collator.pool.Locals()

			simulatedBundles, err := simulateBundles(work.work, work.bundles, pendingTxs, locals)
			if err != nil {
				continue
			}

			sort.SliceStable(simulatedBundles, func(i, j int) bool {
				return simulatedBundles[j].mevGasPrice.Cmp(simulatedBundles[i].mevGasPrice) < 0
			})

			mergedBundlesStats, err := mergeBundles(*work, simulatedBundles, pendingTxs, locals, w.maxMergedBundles)
			if err != nil {
				continue
			}

			if mergedBundlesStats.numTxs == 0 && w.maxMergedBundles != 0 {
				continue
			}

			if w.maxMergedBundles != 0 && mergedBundlesStats.numBundles != w.maxMergedBundles {
				continue
			}

			if mergedBundlesStats.numTxs == 0 && len(pendingTxs) == 0 {
				continue
			}

			// TODO add tx-fees to profit
			collator.FillTransactions(work.work.Ctx, work.work.Block, nil, pendingTxs, locals)

			header := work.work.Block.Header()

			w.collator.commitMu.Lock()

			// don't commit if the block is stale or the task doesn't increase profit
			if mergedBundlesStats.profit.Cmp(w.collator.bestProfit) < 0 && w.collator.lastParentHash != header.ParentHash {
				w.collator.commitMu.Unlock()
				continue
			}

			if work.work.Block.Commit() {
				w.collator.bestProfit.Set(mergedBundlesStats.profit)
				w.collator.lastParentHash = header.ParentHash
			}
			log.Info("collator called Commit")
			w.collator.commitMu.Unlock()
		}
	}
}

func simulateBundles(work collator.BlockCollatorWork, b []MevBundle, pendingTxs map[common.Address]types.Transactions, locals []common.Address) ([]simulatedBundle, error) {
	result := []simulatedBundle{}

	if len(b) == 0 {
		return []simulatedBundle{}, nil
	}

	for _, bundle := range b {
		blockCopy := work.Block.Copy()
		simulated, err := applyBundle(work.Ctx, bundle, blockCopy, pendingTxs)
		if err != nil {
			if errors.Is(errInterrupted, err) {
				return nil, err
			} else {
				log.Error("failed to simulate bndle", "err", err)
				continue
			}
		} else {
			result = append(result, *simulated)
		}
	}
	return result, nil
}

func (c *MevCollator) collateBlock(work collator.BlockCollatorWork) {
	header := work.Block.Header()
	bundles := c.eligibleBundles(header.Number, header.Time)

	blockCopy := work.Block.Copy()
	// signal to our "normal" worker to start building a block using the standard strategy
	c.workers[0].newWorkCh <- &bundleWork{work: work, bundles: []MevBundle{}}

	if len(bundles) > 0 {
		var bundleBlocksExpected uint
		if len(bundles) > int(c.maxMergedBundles) {
			bundleBlocksExpected = c.maxMergedBundles
		} else {
			bundleBlocksExpected = uint(len(bundles))
		}

		for i := 0; i < int(bundleBlocksExpected); i++ {
			c.workers[i+1].newWorkCh <- &bundleWork{work: collator.BlockCollatorWork{Block: blockCopy.Copy(), Ctx: work.Ctx}, bundles: bundles}
		}
	}
}

func (c *MevCollator) CollateBlock(bs collator.BlockState, pool collator.Pool) {
	panic("pls implement me")
}

func (c *MevCollator) CollateBlocks(pool collator.Pool, blockCh <-chan collator.BlockCollatorWork, exitCh <-chan struct{}) {
	c.pool = pool
	for i := 0; i < int(c.maxMergedBundles); i++ {
		worker := bundleWorker{
			collator:         c,
			newWorkCh:        make(chan *bundleWork),
			maxMergedBundles: uint(i),
			id:               i,
		}

		c.workers = append(c.workers, worker)
		go worker.bundleWorkMainLoop()
	}

	for {
		select {
		case work := <-blockCh:
			// TODO implement recommit mechanism
			c.collateBlock(work)
		case <-exitCh:
			// TODO close all workers
			for i := 0; i < len(c.workers); i++ {
				close(c.workers[i].newWorkCh)
			}
		}
	}
}
