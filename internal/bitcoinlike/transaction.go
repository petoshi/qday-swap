package bitcoinlike

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/wire/v2"
)

// EncodeTransaction returns the canonical Bitcoin-family wire encoding.
func EncodeTransaction(tx *wire.MsgTx) (string, error) {
	if tx == nil {
		return "", errors.New("transaction is nil")
	}
	var encoded bytes.Buffer
	if err := tx.Serialize(&encoded); err != nil {
		return "", fmt.Errorf("encode transaction: %w", err)
	}
	return hex.EncodeToString(encoded.Bytes()), nil
}

// DecodeTransaction parses a raw Bitcoin-family transaction.
func DecodeTransaction(encoded string) (*wire.MsgTx, error) {
	b, err := hex.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode transaction hex: %w", err)
	}
	tx := wire.NewMsgTx(2)
	if err := tx.Deserialize(bytes.NewReader(b)); err != nil {
		return nil, fmt.Errorf("decode transaction: %w", err)
	}
	return tx, nil
}
