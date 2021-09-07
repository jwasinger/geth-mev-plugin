# geth-mev-plugin

## Setup

Build geth (from this tag https://github.com/jwasinger/go-ethereum/releases/tag/collator) and copy the `geth` binary into the top-level folder of this repo.

Build the plugin

`make`

## Usage

Run geth with plugin mode enabled and the mev-plugin specified:

`./run_geth.sh`
