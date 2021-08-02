package main

import (
    "github.com/ethereum/go-ethereum/miner"
)

type MEVCollator struct {

}

func (MEVCollator) CollateBlock(bs miner.BlockState, pool miner.Pool) bool {
    return false
}

interface CollatorAPI {
    func InitAPI(c Collator)
}

type MEVCollatorAPI struct {
    collator *MEVCollator
}

func (MEVCollatorAPI *api) AddBundle() {

}

func CreateCollatorAPI(c MEVCollator) MEVCollatorAPI {

}

func CreateCollator() MEVCollator {

}
