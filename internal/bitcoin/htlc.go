package bitcoin

import "github.com/petoshi/qday-swap/internal/bitcoinlike"

const SatoshisPerBitcoin = bitcoinlike.AtomsPerCoin

type Contract = bitcoinlike.Contract
type Funding = bitcoinlike.Funding

func NewContract(amount int64, secretHash [32]byte, recipientPubKey, refundPubKey []byte, refundHeight uint32) (Contract, error) {
	return bitcoinlike.NewContract(amount, secretHash, recipientPubKey, refundPubKey, refundHeight)
}
