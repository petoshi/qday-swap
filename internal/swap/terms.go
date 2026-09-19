// Package swap defines the encrypted, immutable QDAY/BTC contract messages
// exchanged by two local applications after a public order is matched.
package swap

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/petoshi/qday-swap/internal/order"
)

const (
	ProtocolVersion = uint16(1)

	PartyMaker = "maker"
	PartyTaker = "taker"

	MessageHello        = "hello"
	MessageAgreement    = "agreement"
	MessageAgreementAck = "agreement_ack"
	MessageFunding      = "funding"

	// The maker locks first and therefore receives the longer refund window.
	// These windows target 48 and 24 hours at the consensus block intervals.
	makerQDAYWindow    = uint64(2_880)
	takerQDAYWindow    = uint64(1_440)
	makerBitcoinWindow = uint32(288)
	takerBitcoinWindow = uint32(144)
)

type QDAYKeys struct {
	Classical string `json:"classical"`
	Reserve   string `json:"reserve"`
	Address   string `json:"address"`
}

type Hello struct {
	Version          uint16   `json:"version"`
	Network          string   `json:"network"`
	OrderID          string   `json:"orderID"`
	TradeID          string   `json:"tradeID"`
	Party            string   `json:"party"`
	QDAY             QDAYKeys `json:"qday"`
	BitcoinPublicKey string   `json:"bitcoinPublicKey"`
	QDAYHeight       uint64   `json:"qdayHeight"`
	BitcoinHeight    uint32   `json:"bitcoinHeight"`
}

type Agreement struct {
	Version              uint16 `json:"version"`
	Network              string `json:"network"`
	OrderID              string `json:"orderID"`
	TradeID              string `json:"tradeID"`
	MakerGives           string `json:"makerGives"`
	QDAYAmountAtomic     string `json:"qdayAmountAtomic"`
	QDAYUnitAtomic       string `json:"qdayUnitAtomic"`
	BitcoinAmountSatoshi int64  `json:"bitcoinAmountSatoshi"`
	SecretHash           string `json:"secretHash"`
	Maker                Hello  `json:"maker"`
	Taker                Hello  `json:"taker"`
	QDAYRefundHeight     uint64 `json:"qdayRefundHeight"`
	BitcoinRefundHeight  uint32 `json:"bitcoinRefundHeight"`
	QDAYConfirmations    uint64 `json:"qdayConfirmations"`
	BitcoinConfirmations int32  `json:"bitcoinConfirmations"`
}

type FundingNotice struct {
	Party          string `json:"party"`
	Asset          string `json:"asset"`
	TransactionID  string `json:"transactionID"`
	RawTransaction string `json:"rawTransaction,omitempty"`
}

type Message struct {
	Version       uint16         `json:"version"`
	Kind          string         `json:"kind"`
	TradeID       string         `json:"tradeID"`
	Hello         *Hello         `json:"hello,omitempty"`
	Agreement     *Agreement     `json:"agreement,omitempty"`
	AgreementHash string         `json:"agreementHash,omitempty"`
	Funding       *FundingNotice `json:"funding,omitempty"`
}

func NewHello(network, orderID, tradeID, party string, keys QDAYKeys, bitcoinPublicKey []byte, qdayHeight uint64, bitcoinHeight uint32) (Hello, error) {
	hello := Hello{
		Version: ProtocolVersion, Network: network, OrderID: orderID,
		TradeID: tradeID, Party: party, QDAY: keys,
		BitcoinPublicKey: hex.EncodeToString(bitcoinPublicKey),
		QDAYHeight:       qdayHeight, BitcoinHeight: bitcoinHeight,
	}
	return hello, hello.Validate(network, orderID, tradeID, party)
}

func (h Hello) Validate(network, orderID, tradeID, party string) error {
	if h.Version != ProtocolVersion {
		return fmt.Errorf("unsupported swap hello version %d", h.Version)
	} else if h.Network != network || h.OrderID != orderID || h.TradeID != tradeID || h.Party != party {
		return errors.New("swap hello does not match the selected trade")
	} else if party != PartyMaker && party != PartyTaker {
		return errors.New("swap participant must be maker or taker")
	} else if err := digestHex("order ID", orderID); err != nil {
		return err
	} else if err := digestHex("trade ID", tradeID); err != nil {
		return err
	} else if err := qdayKeys(h.QDAY); err != nil {
		return err
	}
	encoded, err := hex.DecodeString(h.BitcoinPublicKey)
	if err != nil || len(encoded) != 33 || h.BitcoinPublicKey != strings.ToLower(h.BitcoinPublicKey) {
		return errors.New("Bitcoin swap public key must be 33 lowercase hexadecimal bytes")
	} else if _, err := btcec.ParsePubKey(encoded); err != nil {
		return fmt.Errorf("invalid Bitcoin swap public key: %w", err)
	}
	return nil
}

func BuildAgreement(signed order.Signed, tradeID, secretHash string, maker, taker Hello) (Agreement, error) {
	terms := signed.Order
	agreement := Agreement{
		Version: ProtocolVersion, Network: terms.Network, OrderID: signed.ID,
		TradeID: tradeID, MakerGives: terms.Give.Asset,
		QDAYUnitAtomic: terms.QDAYUnitAtomic, SecretHash: secretHash,
		Maker: maker, Taker: taker, QDAYConfirmations: 3, BitcoinConfirmations: 1,
	}
	var bitcoinText string
	if terms.Give.Asset == "QDAY" {
		agreement.QDAYAmountAtomic, bitcoinText = terms.Give.Atomic, terms.Receive.Atomic
	} else {
		agreement.QDAYAmountAtomic, bitcoinText = terms.Receive.Atomic, terms.Give.Atomic
	}
	var err error
	agreement.BitcoinAmountSatoshi, err = parseSatoshis(bitcoinText)
	if err != nil {
		return Agreement{}, err
	}
	baseQDAY := max(maker.QDAYHeight, taker.QDAYHeight)
	baseBitcoin := max(maker.BitcoinHeight, taker.BitcoinHeight)
	if terms.Give.Asset == "QDAY" {
		agreement.QDAYRefundHeight = baseQDAY + makerQDAYWindow
		agreement.BitcoinRefundHeight = baseBitcoin + takerBitcoinWindow
	} else {
		agreement.QDAYRefundHeight = baseQDAY + takerQDAYWindow
		agreement.BitcoinRefundHeight = baseBitcoin + makerBitcoinWindow
	}
	return agreement, agreement.Validate(signed, maker, taker)
}

func (a Agreement) Validate(signed order.Signed, maker, taker Hello) error {
	if a.Version != ProtocolVersion {
		return fmt.Errorf("unsupported swap agreement version %d", a.Version)
	} else if a.Network != signed.Order.Network || a.OrderID != signed.ID || a.TradeID != maker.TradeID || a.TradeID != taker.TradeID {
		return errors.New("swap agreement does not match the selected order and trade")
	} else if a.MakerGives != signed.Order.Give.Asset || (a.MakerGives != "QDAY" && a.MakerGives != "BTC") {
		return errors.New("swap agreement direction does not match the order")
	} else if a.QDAYUnitAtomic != signed.Order.QDAYUnitAtomic {
		return errors.New("swap agreement QDAY unit does not match the order")
	} else if err := digestHex("secret hash", a.SecretHash); err != nil {
		return err
	} else if err := maker.Validate(a.Network, a.OrderID, a.TradeID, PartyMaker); err != nil {
		return fmt.Errorf("maker hello: %w", err)
	} else if err := taker.Validate(a.Network, a.OrderID, a.TradeID, PartyTaker); err != nil {
		return fmt.Errorf("taker hello: %w", err)
	} else if !equalHello(a.Maker, maker) || !equalHello(a.Taker, taker) {
		return errors.New("swap agreement changed a participant descriptor")
	}

	var expectedQDAY, expectedBitcoin string
	if signed.Order.Give.Asset == "QDAY" {
		expectedQDAY, expectedBitcoin = signed.Order.Give.Atomic, signed.Order.Receive.Atomic
	} else {
		expectedQDAY, expectedBitcoin = signed.Order.Receive.Atomic, signed.Order.Give.Atomic
	}
	bitcoin, err := parseSatoshis(expectedBitcoin)
	if err != nil {
		return err
	} else if a.QDAYAmountAtomic != expectedQDAY || a.BitcoinAmountSatoshi != bitcoin {
		return errors.New("swap agreement amounts do not match the signed order")
	} else if a.QDAYConfirmations != 3 || a.BitcoinConfirmations != 1 {
		return errors.New("swap agreement confirmation policy is invalid")
	}

	baseQDAY := max(maker.QDAYHeight, taker.QDAYHeight)
	baseBitcoin := max(maker.BitcoinHeight, taker.BitcoinHeight)
	expectedQDAYHeight, expectedBitcoinHeight := baseQDAY+takerQDAYWindow, baseBitcoin+takerBitcoinWindow
	if a.MakerGives == "QDAY" {
		expectedQDAYHeight = baseQDAY + makerQDAYWindow
	} else {
		expectedBitcoinHeight = baseBitcoin + makerBitcoinWindow
	}
	if expectedQDAYHeight < baseQDAY || expectedBitcoinHeight < baseBitcoin ||
		a.QDAYRefundHeight != expectedQDAYHeight || a.BitcoinRefundHeight != expectedBitcoinHeight {
		return errors.New("swap agreement refund heights are invalid")
	}
	return nil
}

func (a Agreement) Hash() (string, error) {
	encoded, err := json.Marshal(a)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func (m Message) ValidateBasic() error {
	if m.Version != ProtocolVersion {
		return fmt.Errorf("unsupported encrypted swap message version %d", m.Version)
	} else if err := digestHex("trade ID", m.TradeID); err != nil {
		return err
	}
	switch m.Kind {
	case MessageHello:
		if m.Hello == nil || m.Agreement != nil || m.AgreementHash != "" || m.Funding != nil {
			return errors.New("hello message payload is invalid")
		}
	case MessageAgreement:
		if m.Agreement == nil || m.Hello != nil || m.AgreementHash != "" || m.Funding != nil {
			return errors.New("agreement message payload is invalid")
		}
	case MessageAgreementAck:
		if m.Agreement != nil || m.Hello != nil || m.Funding != nil || digestHex("agreement hash", m.AgreementHash) != nil {
			return errors.New("agreement acknowledgement payload is invalid")
		}
	case MessageFunding:
		if m.Funding == nil || m.Hello != nil || m.Agreement != nil || m.AgreementHash != "" {
			return errors.New("funding message payload is invalid")
		}
		if err := m.Funding.Validate(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown encrypted swap message kind %q", m.Kind)
	}
	return nil
}

func (n FundingNotice) Validate() error {
	if n.Party != PartyMaker && n.Party != PartyTaker {
		return errors.New("funding party must be maker or taker")
	} else if n.Asset != "QDAY" && n.Asset != "BTC" {
		return errors.New("funding asset must be QDAY or BTC")
	} else if err := digestHex("funding transaction ID", n.TransactionID); err != nil {
		return err
	} else if n.Asset == "BTC" && n.RawTransaction == "" {
		return errors.New("Bitcoin funding notice must include the signed transaction")
	} else if n.Asset == "QDAY" && n.RawTransaction != "" {
		return errors.New("QDAY funding notice must not include an external raw encoding")
	}
	if n.RawTransaction != "" {
		decoded, err := hex.DecodeString(n.RawTransaction)
		if err != nil || len(decoded) == 0 || n.RawTransaction != strings.ToLower(n.RawTransaction) {
			return errors.New("funding transaction must be lowercase hexadecimal data")
		}
	}
	return nil
}

func parseSatoshis(value string) (int64, error) {
	integer, ok := new(big.Int).SetString(value, 10)
	if !ok || integer.Sign() <= 0 || !integer.IsInt64() {
		return 0, errors.New("Bitcoin amount does not fit a positive signed 64-bit integer")
	}
	return integer.Int64(), nil
}

func qdayKeys(keys QDAYKeys) error {
	for _, field := range []struct{ name, value string }{{"classical", keys.Classical}, {"reserve", keys.Reserve}} {
		decoded, err := hex.DecodeString(field.value)
		if err != nil || len(decoded) != 32 || field.value != strings.ToLower(field.value) {
			return fmt.Errorf("QDAY %s key must be 32 lowercase hexadecimal bytes", field.name)
		}
	}
	if strings.TrimSpace(keys.Address) == "" || len(keys.Address) > 128 {
		return errors.New("QDAY swap address is invalid")
	}
	return nil
}

func digestHex(name, value string) error {
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size || value != strings.ToLower(value) {
		return fmt.Errorf("%s must be 32 lowercase hexadecimal bytes", name)
	}
	return nil
}

func equalHello(left, right Hello) bool {
	leftEncoded, _ := json.Marshal(left)
	rightEncoded, _ := json.Marshal(right)
	return string(leftEncoded) == string(rightEncoded)
}
