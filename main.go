package main

import (
	"errors"
	"math/big"

	"github.com/ethereum/go-ethereum/miner/collator"
)

func PluginConstructor(cfg *map[string]interface{}) (collator.Collator, collator.CollatorAPI, error) {
	if cfg == nil {
		return nil, nil, errors.New("expected config")
	}
	config := *cfg

	val, okay := config["maxMergedBundles"]
	if !okay {
		return nil, nil, errors.New("no field maxMergedBundles in config")
	}

	mmb, okay := val.(int64)
	if !okay {
		return nil, nil, errors.New("field maxMergedBundles must be an integer")
	}

	// TODO some sanity check to make sure maxMergedBundles is a reasonable value

	maxMergedBundles := (uint)(mmb)

	collator := MevCollator{
		maxMergedBundles: maxMergedBundles,
		bestProfit:       big.NewInt(0),
	}

	api := NewMevCollatorAPI(&collator)

	return &collator, &api, nil
}

func main() {
	//        var asdf miner.CollatorPluginConstructorFunc = PluginConstructor
}
