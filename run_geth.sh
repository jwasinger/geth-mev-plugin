#!/usr/bin/env bash

./geth  --miner.enablecollatorplugin --miner.collatorpluginfile $(pwd)/geth-mev-collator.so --miner.collatorpluginconfigfile $(pwd)/testnet/mev-collator.toml 
