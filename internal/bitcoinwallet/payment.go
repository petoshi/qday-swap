package bitcoinwallet

import (
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/btcsuite/btcwallet/wallet/txauthor"
)

type PaymentQuote struct {
	Destination        string `json:"destination"`
	Amount             Amount `json:"amount"`
	Fee                Amount `json:"fee"`
	Total              Amount `json:"total"`
	FeeRateSatPerVByte int64  `json:"feeRateSatPerVByte"`
}

type Payment struct {
	PaymentQuote
	TransactionID string `json:"transactionID"`
}

func (c *Client) paymentOutput(destination string, satoshis int64) (*wire.TxOut, string, error) {
	if err := c.requireSwapWallet(); err != nil {
		return nil, "", err
	} else if c.wallet.Locked() {
		return nil, "", errors.New("Bitcoin wallet is locked")
	} else if satoshis <= 0 {
		return nil, "", errors.New("Bitcoin amount must be positive")
	}
	destination = strings.TrimSpace(destination)
	decoded, err := address.DecodeAddress(destination, c.wallet.ChainParams())
	if err != nil || !decoded.IsForNet(c.wallet.ChainParams()) {
		return nil, "", errors.New("invalid Bitcoin address for this network")
	}
	script, err := txscript.PayToAddrScript(decoded)
	if err != nil {
		return nil, "", fmt.Errorf("build Bitcoin destination: %w", err)
	}
	return wire.NewTxOut(satoshis, script), decoded.EncodeAddress(), nil
}

func (c *Client) authorPayment(destination string, satoshis int64, dryRun bool) (*txauthor.AuthoredTx, string, error) {
	output, canonical, err := c.paymentOutput(destination, satoshis)
	if err != nil {
		return nil, "", err
	}
	authored, err := c.wallet.CreateSimpleTx(
		&waddrmgr.KeyScopeBIP0084,
		waddrmgr.DefaultAccountNum,
		[]*wire.TxOut{output},
		1,
		defaultFundingFeeRate,
		wallet.CoinSelectionLargest,
		dryRun,
	)
	if err != nil {
		return nil, "", fmt.Errorf("prepare Bitcoin payment: %w", err)
	}
	return authored, canonical, nil
}

func authoredFee(authored *txauthor.AuthoredTx) (int64, error) {
	if authored == nil || authored.Tx == nil {
		return 0, errors.New("Bitcoin wallet returned an empty payment")
	}
	outputs := int64(0)
	for _, output := range authored.Tx.TxOut {
		if output.Value < 0 || outputs > math.MaxInt64-output.Value {
			return 0, errors.New("Bitcoin payment output total is invalid")
		}
		outputs += output.Value
	}
	fee := int64(authored.TotalInput) - outputs
	if fee <= 0 {
		return 0, errors.New("Bitcoin wallet returned an invalid network fee")
	}
	return fee, nil
}

func (c *Client) quotePayment(destination string, satoshis int64, maximum bool) (PaymentQuote, error) {
	if maximum {
		balance, err := c.Balance()
		if err != nil {
			return PaymentQuote{}, err
		}
		confirmed, err := parseSatoshis(balance.Confirmed.Satoshis)
		if err != nil || confirmed <= 0 {
			return PaymentQuote{}, errors.New("Bitcoin balance is empty")
		}
		low, high, best := int64(1), confirmed, int64(0)
		var lastError error
		for low <= high {
			candidate := low + (high-low)/2
			if _, _, err := c.authorPayment(destination, candidate, true); err != nil {
				lastError = err
				high = candidate - 1
			} else {
				best = candidate
				low = candidate + 1
			}
		}
		if best == 0 {
			if lastError != nil {
				return PaymentQuote{}, lastError
			}
			return PaymentQuote{}, errors.New("Bitcoin balance cannot cover a payment and its fee")
		}
		satoshis = best
	}
	authored, canonical, err := c.authorPayment(destination, satoshis, true)
	if err != nil {
		return PaymentQuote{}, err
	}
	fee, err := authoredFee(authored)
	if err != nil {
		return PaymentQuote{}, err
	} else if satoshis > math.MaxInt64-fee {
		return PaymentQuote{}, errors.New("Bitcoin payment total overflows")
	}
	return PaymentQuote{
		Destination:        canonical,
		Amount:             amount(satoshis),
		Fee:                amount(fee),
		Total:              amount(satoshis + fee),
		FeeRateSatPerVByte: int64(defaultFundingFeeRate) / 1000,
	}, nil
}

func (c *Client) QuotePayment(destination string, satoshis int64, maximum bool) (PaymentQuote, error) {
	c.paymentMu.Lock()
	defer c.paymentMu.Unlock()
	return c.quotePayment(destination, satoshis, maximum)
}

func (c *Client) SendPayment(destination string, satoshis, expectedFee int64) (Payment, error) {
	c.paymentMu.Lock()
	defer c.paymentMu.Unlock()
	quote, err := c.quotePayment(destination, satoshis, false)
	if err != nil {
		return Payment{}, err
	} else if expectedFee <= 0 || quote.Fee.Satoshis != fmt.Sprintf("%d", expectedFee) {
		return Payment{}, errors.New("Bitcoin network fee changed; review the payment again")
	}
	authored, _, err := c.authorPayment(quote.Destination, satoshis, false)
	if err != nil {
		return Payment{}, err
	}
	actualFee, err := authoredFee(authored)
	if err != nil {
		return Payment{}, err
	} else if actualFee != expectedFee {
		return Payment{}, errors.New("Bitcoin network fee changed; review the payment again")
	}
	if err := c.wallet.PublishTransaction(authored.Tx, "QDAY Swap wallet withdrawal"); err != nil {
		return Payment{}, fmt.Errorf("broadcast Bitcoin payment: %w", err)
	}
	return Payment{PaymentQuote: quote, TransactionID: authored.Tx.TxHash().String()}, nil
}

func parseSatoshis(value string) (int64, error) {
	var satoshis int64
	if value == "" {
		return 0, errors.New("Bitcoin amount is missing")
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return 0, errors.New("Bitcoin amount is invalid")
		}
		if satoshis > (math.MaxInt64-int64(character-'0'))/10 {
			return 0, errors.New("Bitcoin amount is too large")
		}
		satoshis = satoshis*10 + int64(character-'0')
	}
	return satoshis, nil
}
