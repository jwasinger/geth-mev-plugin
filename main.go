package main

import (
	"errors"
	"math/big"

	"github.com/ethereum/go-ethereum/miner/collator"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/rpc"
)

func PluginConstructor(cfg *map[string]interface{}, stack *node.Node) (collator.Collator, error) {
	if cfg == nil {
		return nil, errors.New("expected config")
	}
	config := *cfg

	val, okay := config["maxMergedBundles"]
	if !okay {
		return nil, errors.New("no field maxMergedBundles in config")
	}

	mmb, okay := val.(int64)
	if !okay {
		return nil, errors.New("field maxMergedBundles must be an integer")
	}

	// TODO some sanity check to make sure maxMergedBundles is a reasonable value

	maxMergedBundles := (uint)(mmb)

	collator := MevCollator{
		maxMergedBundles: maxMergedBundles,
		bestProfit:       big.NewInt(0),
	}

	api := rpc.API{
		Service: MevCollatorAPI{&collator},
		Version: "0.1",
		Namespace: "eth",
		Public: false,
	}

	stack.RegisterAPIs([]rpc.API{
		api,
	})

	return &collator, nil
}

func main() {
	//        var asdf miner.CollatorPluginConstructorFunc = PluginConstructor
}
