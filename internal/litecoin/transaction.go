package litecoin

import (
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/petoshi/qday-swap/internal/bitcoinlike"
)

func EncodeTransaction(tx *wire.MsgTx) (string, error) {
	return bitcoinlike.EncodeTransaction(tx)
}

func DecodeTransaction(encoded string) (*wire.MsgTx, error) {
	return bitcoinlike.DecodeTransaction(encoded)
}
