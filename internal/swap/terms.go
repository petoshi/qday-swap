// Package swap defines the encrypted, immutable QDAY/BTC contract messages
// exchanged by two local applications after a public order is matched.
package swap

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/petoshi/qday-swap/internal/order"
	"github.com/petoshi/qday-swap/internal/trade"
)

const (
	ProtocolVersion      = uint16(1)
	AsyncProtocolVersion = uint16(2)

	PartyMaker = "maker"
	PartyTaker = "taker"

	MessageHello         = "hello"
	MessageAgreement     = "agreement"
	MessageAgreementAck  = "agreement_ack"
	MessageFunding       = "funding"
	MessageClaimTemplate = "claim_template"

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

type ClaimTemplate struct {
	Party                string   `json:"party"`
	Asset                string   `json:"asset"`
	FundingTransactionID string   `json:"fundingTransactionID"`
	TransactionID        string   `json:"transactionID"`
	RawTransactions      []string `json:"rawTransactions"`
	BasisHeight          uint64   `json:"basisHeight,omitempty"`
	BasisID              string   `json:"basisID,omitempty"`
}

type Message struct {
	Version       uint16         `json:"version"`
	Kind          string         `json:"kind"`
	TradeID       string         `json:"tradeID"`
	Hello         *Hello         `json:"hello,omitempty"`
	Agreement     *Agreement     `json:"agreement,omitempty"`
	AgreementHash string         `json:"agreementHash,omitempty"`
	Funding       *FundingNotice `json:"funding,omitempty"`
	ClaimTemplate *ClaimTemplate `json:"claimTemplate,omitempty"`
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
	if h.Version != ProtocolVersion && h.Version != AsyncProtocolVersion {
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

func BuildAsyncAgreement(signed order.Signed, acceptance trade.SignedAcceptance) (Agreement, error) {
	if signed.Order.Version != order.AsyncProtocolVersion || acceptance.Acceptance.Version != trade.AsyncProtocolVersion {
		return Agreement{}, errors.New("asynchronous agreement requires version 2 order and acceptance")
	}
	verificationTime := time.Unix(acceptance.Acceptance.CreatedAt, 0)
	if err := acceptance.Verify(signed, verificationTime); err != nil {
		return Agreement{}, fmt.Errorf("verify asynchronous acceptance: %w", err)
	}
	return BuildAsyncTerms(signed, acceptance.Acceptance)
}

// BuildAsyncTerms constructs the immutable two-chain contract before the
// taker's funding transaction is prepared. NewAsyncAcceptance subsequently
// signs these descriptors, heights, hashlock and the exact funding bytes.
func BuildAsyncTerms(signed order.Signed, accepted trade.AcceptancePayload) (Agreement, error) {
	if signed.Order.Version != order.AsyncProtocolVersion || accepted.Version != trade.AsyncProtocolVersion {
		return Agreement{}, errors.New("asynchronous terms require version 2 order and acceptance payload")
	}
	terms := signed.Order
	maker := Hello{
		Version: AsyncProtocolVersion, Network: terms.Network, OrderID: signed.ID,
		TradeID: accepted.TradeID, Party: PartyMaker,
		QDAY:             QDAYKeys{Classical: terms.MakerQDAY.Classical, Reserve: terms.MakerQDAY.Reserve, Address: terms.MakerQDAY.Address},
		BitcoinPublicKey: terms.MakerBitcoinKey, QDAYHeight: terms.MakerQDAYHeight, BitcoinHeight: terms.MakerBTCHeight,
	}
	taker := Hello{
		Version: AsyncProtocolVersion, Network: terms.Network, OrderID: signed.ID,
		TradeID: accepted.TradeID, Party: PartyTaker,
		QDAY:             QDAYKeys{Classical: accepted.TakerQDAY.Classical, Reserve: accepted.TakerQDAY.Reserve, Address: accepted.TakerQDAY.Address},
		BitcoinPublicKey: accepted.TakerBitcoinKey, QDAYHeight: accepted.TakerQDAYHeight, BitcoinHeight: accepted.TakerBTCHeight,
	}
	agreement := Agreement{
		Version: AsyncProtocolVersion, Network: terms.Network, OrderID: signed.ID,
		TradeID: accepted.TradeID, MakerGives: terms.Give.Asset,
		QDAYUnitAtomic: terms.QDAYUnitAtomic, SecretHash: accepted.SecretHash,
		Maker: maker, Taker: taker,
		QDAYRefundHeight: accepted.QDAYRefundHeight, BitcoinRefundHeight: accepted.BTCRefundHeight,
		QDAYConfirmations: 3, BitcoinConfirmations: 1,
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
	return agreement, agreement.validateAsyncPayload(signed, accepted)
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

func (a Agreement) ValidateAsync(signed order.Signed, acceptance trade.SignedAcceptance) error {
	if a.Version != AsyncProtocolVersion || signed.Order.Version != order.AsyncProtocolVersion || acceptance.Acceptance.Version != trade.AsyncProtocolVersion {
		return errors.New("asynchronous agreement protocol version is invalid")
	}
	verificationTime := time.Unix(acceptance.Acceptance.CreatedAt, 0)
	if err := acceptance.Verify(signed, verificationTime); err != nil {
		return err
	}
	return a.validateAsyncPayload(signed, acceptance.Acceptance)
}

func (a Agreement) validateAsyncPayload(signed order.Signed, p trade.AcceptancePayload) error {
	if a.Network != signed.Order.Network || a.OrderID != signed.ID || a.TradeID != p.TradeID || a.MakerGives != signed.Order.Give.Asset {
		return errors.New("asynchronous agreement does not match the selected order")
	} else if a.QDAYUnitAtomic != signed.Order.QDAYUnitAtomic || a.SecretHash != p.SecretHash {
		return errors.New("asynchronous agreement denomination or secret differs from the acceptance")
	} else if err := a.Maker.Validate(a.Network, a.OrderID, a.TradeID, PartyMaker); err != nil {
		return fmt.Errorf("maker descriptor: %w", err)
	} else if err := a.Taker.Validate(a.Network, a.OrderID, a.TradeID, PartyTaker); err != nil {
		return fmt.Errorf("taker descriptor: %w", err)
	}
	expectedMaker := Hello{
		Version: AsyncProtocolVersion, Network: signed.Order.Network, OrderID: signed.ID, TradeID: p.TradeID, Party: PartyMaker,
		QDAY:             QDAYKeys{Classical: signed.Order.MakerQDAY.Classical, Reserve: signed.Order.MakerQDAY.Reserve, Address: signed.Order.MakerQDAY.Address},
		BitcoinPublicKey: signed.Order.MakerBitcoinKey, QDAYHeight: signed.Order.MakerQDAYHeight, BitcoinHeight: signed.Order.MakerBTCHeight,
	}
	expectedTaker := Hello{
		Version: AsyncProtocolVersion, Network: signed.Order.Network, OrderID: signed.ID, TradeID: p.TradeID, Party: PartyTaker,
		QDAY:             QDAYKeys{Classical: p.TakerQDAY.Classical, Reserve: p.TakerQDAY.Reserve, Address: p.TakerQDAY.Address},
		BitcoinPublicKey: p.TakerBitcoinKey, QDAYHeight: p.TakerQDAYHeight, BitcoinHeight: p.TakerBTCHeight,
	}
	if !equalHello(a.Maker, expectedMaker) || !equalHello(a.Taker, expectedTaker) {
		return errors.New("asynchronous agreement changed a participant descriptor")
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
	} else if a.QDAYAmountAtomic != expectedQDAY || a.BitcoinAmountSatoshi != bitcoin || a.QDAYConfirmations != 3 || a.BitcoinConfirmations != 1 {
		return errors.New("asynchronous agreement amounts or confirmation policy are invalid")
	}
	qdayRefund, bitcoinRefund, err := asyncRefundHeights(signed.Order, p)
	if err != nil {
		return err
	} else if a.QDAYRefundHeight != qdayRefund || a.BitcoinRefundHeight != bitcoinRefund {
		return errors.New("asynchronous agreement refund heights are invalid")
	}
	return nil
}

func asyncRefundHeights(terms order.Payload, acceptance trade.AcceptancePayload) (uint64, uint32, error) {
	return trade.AsyncRefundHeights(terms, acceptance.CreatedAt, acceptance.TakerQDAYHeight, acceptance.TakerBTCHeight)
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
	if m.Version != ProtocolVersion && m.Version != AsyncProtocolVersion {
		return fmt.Errorf("unsupported encrypted swap message version %d", m.Version)
	} else if err := digestHex("trade ID", m.TradeID); err != nil {
		return err
	}
	switch m.Kind {
	case MessageHello:
		if m.Version != ProtocolVersion {
			return errors.New("asynchronous swaps do not exchange hello messages")
		}
		if m.Hello == nil || m.Agreement != nil || m.AgreementHash != "" || m.Funding != nil || m.ClaimTemplate != nil {
			return errors.New("hello message payload is invalid")
		}
	case MessageAgreement:
		if m.Version != ProtocolVersion {
			return errors.New("asynchronous swaps do not exchange agreement messages")
		}
		if m.Agreement == nil || m.Hello != nil || m.AgreementHash != "" || m.Funding != nil || m.ClaimTemplate != nil {
			return errors.New("agreement message payload is invalid")
		}
	case MessageAgreementAck:
		if m.Version != ProtocolVersion {
			return errors.New("asynchronous swaps do not exchange agreement acknowledgements")
		}
		if m.Agreement != nil || m.Hello != nil || m.Funding != nil || m.ClaimTemplate != nil || digestHex("agreement hash", m.AgreementHash) != nil {
			return errors.New("agreement acknowledgement payload is invalid")
		}
	case MessageFunding:
		if m.Funding == nil || m.Hello != nil || m.Agreement != nil || m.AgreementHash != "" || m.ClaimTemplate != nil {
			return errors.New("funding message payload is invalid")
		}
		if err := m.Funding.Validate(); err != nil {
			return err
		}
	case MessageClaimTemplate:
		if m.Version != AsyncProtocolVersion || m.ClaimTemplate == nil || m.Funding != nil || m.Hello != nil || m.Agreement != nil || m.AgreementHash != "" {
			return errors.New("claim template message payload is invalid")
		}
		if err := m.ClaimTemplate.Validate(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown encrypted swap message kind %q", m.Kind)
	}
	return nil
}

func (t ClaimTemplate) Validate() error {
	if t.Party != PartyMaker {
		return errors.New("claim template must spend to the maker")
	} else if t.Asset != "QDAY" && t.Asset != "BTC" {
		return errors.New("claim template asset must be QDAY or BTC")
	} else if err := digestHex("claim template funding transaction ID", t.FundingTransactionID); err != nil {
		return err
	} else if err := digestHex("claim template transaction ID", t.TransactionID); err != nil {
		return err
	} else if len(t.RawTransactions) != 1 || t.RawTransactions[0] == "" || len(t.RawTransactions[0]) > 2<<20 {
		return errors.New("claim template must contain one bounded transaction")
	}
	if t.Asset == "QDAY" {
		if t.BasisHeight == 0 || digestHex("claim template QDAY basis ID", t.BasisID) != nil {
			return errors.New("claim template QDAY basis is invalid")
		}
		raw, err := base64.RawStdEncoding.DecodeString(t.RawTransactions[0])
		if err != nil || base64.RawStdEncoding.EncodeToString(raw) != t.RawTransactions[0] {
			return errors.New("QDAY claim template is not canonical base64 data")
		}
	} else {
		if t.BasisHeight != 0 || t.BasisID != "" {
			return errors.New("Bitcoin claim template contains QDAY basis data")
		}
		raw, err := hex.DecodeString(t.RawTransactions[0])
		if err != nil || len(raw) == 0 || hex.EncodeToString(raw) != t.RawTransactions[0] {
			return errors.New("Bitcoin claim template is not lowercase hexadecimal data")
		}
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
