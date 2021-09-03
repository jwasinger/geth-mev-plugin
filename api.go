package main

import (
    "math/big"
    "github.com/ethereum/go-ethereum/core/types"
    "github.com/ethereum/go-ethereum/common"
)

type apiImpl struct {
    c *MevCollator
}

func (api *apiImpl) SendBundle(txs types.Transactions, blockNumber *big.Int, minTimestamp, maxTimestamp uint64, revertingTxHashes []common.Hash) {
    c := api.c
    c.bundleMu.Lock()
    defer c.bundleMu.Unlock()

	c.bundles = append(c.bundles, MevBundle{
        Transactions:               txs,
        BlockNumber:       blockNumber,
        MinTimestamp:      minTimestamp,
        MaxTimestamp:      maxTimestamp,
        RevertingTxHashes: revertingTxHashes,
    })
}

type MevCollatorAPI struct {
        impl apiImpl
}

func (api *MevCollatorAPI) Version() string {
    return "0.1"
}

func (api *MevCollatorAPI) Service() interface{} {
    return api.impl
}
