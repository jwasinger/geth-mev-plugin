type apiImpl struct {
    c *MevCollator
}

func (a *apiImpl) SendBundle(txs types.Transactions, blockNumber *big.Int, minTimestamp, maxTimestamp uint64, revertingTxHashes []common.Hash) {
    c.bundleMu.Lock()
    defer c.bundleMu.Unlock()

	c.bundles = append(c.bundles, types.MevBundle{
        Txs:               txs,
        BlockNumber:       blockNumber,
        MinTimestamp:      minTimestamp,
        MaxTimestamp:      maxTimestamp,
        RevertingTxHashes: revertingTxHashes,
    })
}

/*
// TODO need to provide an interface which provides a copy of the state for rpc to play with
func (a *apiImpl) CallBundle() {

}
*/

type MevCollatorAPI struct {
        impl apiImpl
}

func (api *MevCollatorAPI) Version() string {
    return "0.1"
}

func (api *MevCollatorAPI) Service() interface{} {
    return api.impl
}
