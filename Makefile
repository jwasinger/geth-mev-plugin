.PHONY: all geth plugin

all: geth plugin

geth:
	go build -v -o geth github.com/ethereum/go-ethereum/cmd/geth

plugin:
	go build -v -o mev.plugin -buildmode=plugin .
