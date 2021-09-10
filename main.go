package main

import (
	"errors"
	"github.com/ethereum/go-ethereum/miner"
	"sync"
)

func PluginConstructor(cfg *map[string]interface{}) (miner.Collator, miner.CollatorAPI, error) {
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
		bundleMu:         sync.Mutex{},
		bundles:          []MevBundle{},
	}

	api := NewMevCollatorAPI(&collator)

	return &collator, &api, nil
}

func main() {
	//        var asdf miner.CollatorPluginConstructorFunc = PluginConstructor
}
