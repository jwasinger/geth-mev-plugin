package main

import (
	"math/big"
    "context"
    "errors"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
    "github.com/ethereum/go-ethereum/rpc"
    "github.com/ethereum/go-ethereum/common/hexutil"
)

type ApiImpl struct {
	c *MevCollator
}

// SendBundleArgs represents the arguments for a call.
type SendBundleArgs struct {
    Txs               []hexutil.Bytes `json:"txs"`
    BlockNumber       rpc.BlockNumber `json:"blockNumber"`
    MinTimestamp      *uint64         `json:"minTimestamp"`
    MaxTimestamp      *uint64         `json:"maxTimestamp"`
    RevertingTxHashes []common.Hash   `json:"revertingTxHashes"`
}

func (api *ApiImpl) SendBundle(ctx context.Context, args SendBundleArgs) error {
    var txs types.Transactions
    if len(args.Txs) == 0 {
        return errors.New("bundle missing txs")
    }
    if args.BlockNumber == 0 {
        return errors.New("bundle missing blockNumber")
    }

    for _, encodedTx := range args.Txs {
        tx := new(types.Transaction)
        if err := tx.UnmarshalBinary(encodedTx); err != nil {
            return err
        }
        txs = append(txs, tx)
    }

    var minTimestamp, maxTimestamp uint64
    if args.MinTimestamp != nil {
        minTimestamp = *args.MinTimestamp
    }
    if args.MaxTimestamp != nil {
        maxTimestamp = *args.MaxTimestamp
    }

    blockNumber := big.NewInt(args.BlockNumber.Int64())

	c := api.c
	c.bundleMu.Lock()
	defer c.bundleMu.Unlock()

	c.bundles = append(c.bundles, MevBundle{
		Transactions:      txs,
		BlockNumber:       blockNumber,
		MinTimestamp:      minTimestamp,
		MaxTimestamp:      maxTimestamp,
		RevertingTxHashes: args.RevertingTxHashes,
	})

    return nil
}

type MevCollatorAPI struct {
	impl ApiImpl
}

func NewMevCollatorAPI(c *MevCollator) MevCollatorAPI {
	return MevCollatorAPI{
		impl: ApiImpl{
			c,
		},
	}
}

func (api *MevCollatorAPI) Version() string {
	return "0.1"
}

func (api *MevCollatorAPI) Service() interface{} {
	return &api.impl
}
