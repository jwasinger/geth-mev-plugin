#!/usr/bin/env bash

./geth  --miner.enablecollatorplugin --miner.collatorpluginfile $(pwd)/mev.plugin --miner.collatorpluginconfigfile $(pwd)/local-testnet/config.toml 
