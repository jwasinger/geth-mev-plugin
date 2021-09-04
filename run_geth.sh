#!/usr/bin/env bash

./go-ethereum/build/bin/geth  --miner.enablecollatorplugin --miner.collatorpluginfile $(pwd)/geth-mev-collator.so --miner.collatorpluginconfigfile $(pwd)/testnet/mev-collator.toml 
