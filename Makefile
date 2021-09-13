.PHONY: all geth plugin copy_to_output

all: geth plugin copy_to_output

geth:
	go build -v -o geth github.com/ethereum/go-ethereum/cmd/geth

plugin:
	go build -v -o mev.plugin -buildmode=plugin .
