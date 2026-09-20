package order

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
)

const (
	ProtocolVersion      = uint16(1)
	AsyncProtocolVersion = uint16(2)
	MarketQDAYBTC        = "QDAY-BTC"
	QDAYLegacyUnit       = "1000000000000000000000000"
	QDAYDefendUnit       = "1000000000000000000"
	// MinimumBitcoinSwapSatoshis leaves enough value for the independent
	// claim or refund transaction after its network fee. Tiny signed offers
	// are rejected by every client and relay before negotiation starts.
	MinimumBitcoinSwapSatoshis = int64(10_000)

	minimumLifetime     = time.Minute
	maximumLifetime     = 30 * 24 * time.Hour
	maximumClockSkew    = 5 * time.Minute
	maximumAtomicDigits = 64
	messageKeySize      = 32
)

var (
	orderDomain      = []byte("QDAY_SWAP_ORDER_V1")
	asyncOrderDomain = []byte("QDAY_SWAP_ORDER_V2")
	cancelDomain     = []byte("QDAY_SWAP_CANCEL_V1")
)

type Amount struct {
	Asset  string `json:"asset"`
	Atomic string `json:"atomic"`
}

type QDAYKeys struct {
	Classical string `json:"classical"`
	Reserve   string `json:"reserve"`
	Address   string `json:"address"`
}

type Payload struct {
	Version         uint16   `json:"version"`
	Network         string   `json:"network"`
	Market          string   `json:"market"`
	QDAYUnitAtomic  string   `json:"qdayUnitAtomic"`
	MakerPublicKey  string   `json:"makerPublicKey"`
	MakerMessageKey string   `json:"makerMessageKey"`
	Give            Amount   `json:"give"`
	Receive         Amount   `json:"receive"`
	CreatedAt       int64    `json:"createdAt"`
	ExpiresAt       int64    `json:"expiresAt"`
	Nonce           string   `json:"nonce"`
	SessionID       string   `json:"sessionID,omitempty"`
	MakerQDAY       QDAYKeys `json:"makerQDAY,omitempty"`
	MakerBitcoinKey string   `json:"makerBitcoinPublicKey,omitempty"`
	MakerQDAYHeight uint64   `json:"makerQDAYHeight,omitempty"`
	MakerBTCHeight  uint32   `json:"makerBitcoinHeight,omitempty"`
}

type Signed struct {
	Order     Payload `json:"order"`
	ID        string  `json:"id"`
	Signature string  `json:"signature"`
}

type CancellationPayload struct {
	Version        uint16 `json:"version"`
	OrderID        string `json:"orderID"`
	MakerPublicKey string `json:"makerPublicKey"`
	CreatedAt      int64  `json:"createdAt"`
}

type SignedCancellation struct {
	Cancellation CancellationPayload `json:"cancellation"`
	Signature    string              `json:"signature"`
}

func NewPayload(network, qdayUnitAtomic string, give, receive Amount, lifetime time.Duration, publicKey ed25519.PublicKey, messageKey [messageKeySize]byte, now time.Time) (Payload, error) {
	return newPayload(ProtocolVersion, network, qdayUnitAtomic, give, receive, lifetime, publicKey, messageKey, now)
}

func NewAsyncPayload(network, qdayUnitAtomic string, give, receive Amount, lifetime time.Duration, publicKey ed25519.PublicKey, messageKey [messageKeySize]byte, sessionID string, qdayKeys QDAYKeys, bitcoinPublicKey string, qdayHeight uint64, bitcoinHeight uint32, now time.Time) (Payload, error) {
	payload, err := newPayload(AsyncProtocolVersion, network, qdayUnitAtomic, give, receive, lifetime, publicKey, messageKey, now)
	if err != nil {
		return Payload{}, err
	}
	payload.SessionID = sessionID
	payload.MakerQDAY = qdayKeys
	payload.MakerBitcoinKey = bitcoinPublicKey
	payload.MakerQDAYHeight = qdayHeight
	payload.MakerBTCHeight = bitcoinHeight
	if err := payload.Validate(now); err != nil {
		return Payload{}, err
	}
	return payload, nil
}

func newPayload(version uint16, network, qdayUnitAtomic string, give, receive Amount, lifetime time.Duration, publicKey ed25519.PublicKey, messageKey [messageKeySize]byte, now time.Time) (Payload, error) {
	if lifetime < minimumLifetime || lifetime > maximumLifetime {
		return Payload{}, fmt.Errorf("order lifetime must be between %s and %s", minimumLifetime, maximumLifetime)
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return Payload{}, fmt.Errorf("generate nonce: %w", err)
	}
	payload := Payload{
		Version:         version,
		Network:         network,
		Market:          MarketQDAYBTC,
		QDAYUnitAtomic:  qdayUnitAtomic,
		MakerPublicKey:  hex.EncodeToString(publicKey),
		MakerMessageKey: hex.EncodeToString(messageKey[:]),
		Give:            give,
		Receive:         receive,
		CreatedAt:       now.Unix(),
		ExpiresAt:       now.Add(lifetime).Unix(),
		Nonce:           hex.EncodeToString(nonce),
	}
	if version == ProtocolVersion {
		if err := payload.Validate(now); err != nil {
			return Payload{}, err
		}
	}
	return payload, nil
}

func (p Payload) Validate(now time.Time) error {
	if p.Version != ProtocolVersion && p.Version != AsyncProtocolVersion {
		return fmt.Errorf("unsupported order version %d", p.Version)
	}
	switch p.Network {
	case "mainnet", "testnet", "regtest":
	default:
		return errors.New("network must be mainnet, testnet or regtest")
	}
	if p.Market != MarketQDAYBTC {
		return fmt.Errorf("unsupported market %q", p.Market)
	}
	if p.QDAYUnitAtomic != QDAYLegacyUnit && p.QDAYUnitAtomic != QDAYDefendUnit {
		return errors.New("QDAY unit does not match a consensus denomination")
	}
	if err := validatePair(p.Give, p.Receive); err != nil {
		return err
	}
	if _, err := parsePublicKey(p.MakerPublicKey); err != nil {
		return err
	}
	if _, err := parseMessageKey(p.MakerMessageKey); err != nil {
		return err
	}
	if p.Version == AsyncProtocolVersion {
		if _, err := parseDigest(p.SessionID, "session ID"); err != nil {
			return err
		} else if err := p.MakerQDAY.validate(); err != nil {
			return fmt.Errorf("maker QDAY keys: %w", err)
		} else if err := validateBitcoinPublicKey(p.MakerBitcoinKey); err != nil {
			return fmt.Errorf("maker Bitcoin key: %w", err)
		} else if p.MakerQDAYHeight == 0 || p.MakerBTCHeight == 0 {
			return errors.New("maker chain heights must be positive")
		}
	} else if p.SessionID != "" || p.MakerQDAY != (QDAYKeys{}) || p.MakerBitcoinKey != "" || p.MakerQDAYHeight != 0 || p.MakerBTCHeight != 0 {
		return errors.New("version 1 order contains asynchronous protocol fields")
	}
	nonce, err := hex.DecodeString(p.Nonce)
	if err != nil || len(nonce) != 16 || p.Nonce != strings.ToLower(p.Nonce) {
		return errors.New("nonce must be 16 lowercase hexadecimal bytes")
	}
	created := time.Unix(p.CreatedAt, 0)
	expires := time.Unix(p.ExpiresAt, 0)
	if created.After(now.Add(maximumClockSkew)) {
		return errors.New("order creation time is too far in the future")
	}
	if !expires.After(now) {
		return errors.New("order has expired")
	}
	lifetime := expires.Sub(created)
	if lifetime < minimumLifetime || lifetime > maximumLifetime {
		return fmt.Errorf("order lifetime must be between %s and %s", minimumLifetime, maximumLifetime)
	}
	return nil
}

func validatePair(give, receive Amount) error {
	if give.Asset == receive.Asset {
		return errors.New("give and receive assets must differ")
	}
	if !((give.Asset == "QDAY" && receive.Asset == "BTC") || (give.Asset == "BTC" && receive.Asset == "QDAY")) {
		return errors.New("first release supports only the QDAY-BTC market")
	}
	if err := validateAtomic("give", give.Atomic); err != nil {
		return err
	}
	if err := validateAtomic("receive", receive.Atomic); err != nil {
		return err
	}
	bitcoinAtomic := give.Atomic
	if receive.Asset == "BTC" {
		bitcoinAtomic = receive.Atomic
	}
	bitcoin, _ := new(big.Int).SetString(bitcoinAtomic, 10)
	if bitcoin.Cmp(big.NewInt(MinimumBitcoinSwapSatoshis)) < 0 {
		return fmt.Errorf("Bitcoin swap amount must be at least %d satoshis", MinimumBitcoinSwapSatoshis)
	}
	return nil
}

func validateAtomic(name, value string) error {
	if value == "" || len(value) > maximumAtomicDigits {
		return fmt.Errorf("%s amount must contain 1 to %d decimal digits", name, maximumAtomicDigits)
	}
	if value[0] == '0' && len(value) != 1 {
		return fmt.Errorf("%s amount is not canonical", name)
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return fmt.Errorf("%s amount must be an unsigned atomic integer", name)
		}
	}
	if value == "0" {
		return fmt.Errorf("%s amount must be positive", name)
	}
	return nil
}

func (p Payload) signingBytes() []byte {
	domain := orderDomain
	if p.Version == AsyncProtocolVersion {
		domain = asyncOrderDomain
	}
	encoder := canonicalEncoder{bytes: append([]byte(nil), domain...)}
	encoder.uint16(p.Version)
	encoder.text(p.Network)
	encoder.text(p.Market)
	encoder.text(p.QDAYUnitAtomic)
	encoder.text(p.MakerPublicKey)
	encoder.text(p.MakerMessageKey)
	encoder.text(p.Give.Asset)
	encoder.text(p.Give.Atomic)
	encoder.text(p.Receive.Asset)
	encoder.text(p.Receive.Atomic)
	encoder.int64(p.CreatedAt)
	encoder.int64(p.ExpiresAt)
	encoder.text(p.Nonce)
	if p.Version == AsyncProtocolVersion {
		encoder.text(p.SessionID)
		encoder.text(p.MakerQDAY.Classical)
		encoder.text(p.MakerQDAY.Reserve)
		encoder.text(p.MakerQDAY.Address)
		encoder.text(p.MakerBitcoinKey)
		encoder.uint64(p.MakerQDAYHeight)
		encoder.uint32(p.MakerBTCHeight)
	}
	return encoder.bytes
}

func (k QDAYKeys) validate() error {
	for _, field := range []struct{ name, value string }{{"classical", k.Classical}, {"reserve", k.Reserve}} {
		decoded, err := hex.DecodeString(field.value)
		if err != nil || len(decoded) != 32 || field.value != strings.ToLower(field.value) {
			return fmt.Errorf("%s key must be 32 lowercase hexadecimal bytes", field.name)
		}
	}
	if strings.TrimSpace(k.Address) == "" || len(k.Address) > 128 {
		return errors.New("address is invalid")
	}
	return nil
}

func validateBitcoinPublicKey(value string) error {
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != 33 || value != strings.ToLower(value) {
		return errors.New("public key must be 33 lowercase hexadecimal bytes")
	}
	if _, err := btcec.ParsePubKey(decoded); err != nil {
		return errors.New("public key must be valid compressed secp256k1 data")
	}
	return nil
}

func (p Payload) ID() string {
	digest := sha256.Sum256(p.signingBytes())
	return hex.EncodeToString(digest[:])
}

func Sign(payload Payload, privateKey ed25519.PrivateKey, now time.Time) (Signed, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return Signed{}, errors.New("invalid Ed25519 private key")
	}
	if err := payload.Validate(now); err != nil {
		return Signed{}, err
	}
	publicKey := privateKey.Public().(ed25519.PublicKey)
	if payload.MakerPublicKey != hex.EncodeToString(publicKey) {
		return Signed{}, errors.New("private key does not match maker public key")
	}
	digest := sha256.Sum256(payload.signingBytes())
	return Signed{
		Order:     payload,
		ID:        hex.EncodeToString(digest[:]),
		Signature: hex.EncodeToString(ed25519.Sign(privateKey, digest[:])),
	}, nil
}

func (s Signed) Verify(now time.Time) error {
	if err := s.Order.Validate(now); err != nil {
		return err
	}
	if s.ID != s.Order.ID() {
		return errors.New("order ID does not match signed terms")
	}
	publicKey, err := parsePublicKey(s.Order.MakerPublicKey)
	if err != nil {
		return err
	}
	signature, err := parseSignature(s.Signature)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(s.Order.signingBytes())
	if !ed25519.Verify(publicKey, digest[:], signature) {
		return errors.New("invalid order signature")
	}
	return nil
}

func NewCancellation(order Signed, privateKey ed25519.PrivateKey, now time.Time) (SignedCancellation, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return SignedCancellation{}, errors.New("invalid Ed25519 private key")
	}
	payload := CancellationPayload{
		Version: ProtocolVersion, OrderID: order.ID,
		MakerPublicKey: order.Order.MakerPublicKey, CreatedAt: now.Unix(),
	}
	if hex.EncodeToString(privateKey.Public().(ed25519.PublicKey)) != payload.MakerPublicKey {
		return SignedCancellation{}, errors.New("private key does not match maker public key")
	}
	digest := sha256.Sum256(payload.signingBytes())
	return SignedCancellation{Cancellation: payload, Signature: hex.EncodeToString(ed25519.Sign(privateKey, digest[:]))}, nil
}

func (p CancellationPayload) signingBytes() []byte {
	encoder := canonicalEncoder{bytes: append([]byte(nil), cancelDomain...)}
	encoder.uint16(p.Version)
	encoder.text(p.OrderID)
	encoder.text(p.MakerPublicKey)
	encoder.int64(p.CreatedAt)
	return encoder.bytes
}

func (c SignedCancellation) Verify(now time.Time) error {
	payload := c.Cancellation
	if payload.Version != ProtocolVersion {
		return fmt.Errorf("unsupported cancellation version %d", payload.Version)
	}
	if _, err := parseDigest(payload.OrderID, "order ID"); err != nil {
		return err
	}
	publicKey, err := parsePublicKey(payload.MakerPublicKey)
	if err != nil {
		return err
	}
	created := time.Unix(payload.CreatedAt, 0)
	if created.After(now.Add(maximumClockSkew)) {
		return errors.New("cancellation time is too far in the future")
	}
	signature, err := parseSignature(c.Signature)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(payload.signingBytes())
	if !ed25519.Verify(publicKey, digest[:], signature) {
		return errors.New("invalid cancellation signature")
	}
	return nil
}

func parsePublicKey(encoded string) (ed25519.PublicKey, error) {
	decoded, err := hex.DecodeString(encoded)
	if err != nil || len(decoded) != ed25519.PublicKeySize || encoded != strings.ToLower(encoded) {
		return nil, errors.New("maker public key must be 32 lowercase hexadecimal bytes")
	}
	return ed25519.PublicKey(decoded), nil
}

func parseMessageKey(encoded string) ([]byte, error) {
	decoded, err := hex.DecodeString(encoded)
	if err != nil || len(decoded) != messageKeySize || encoded != strings.ToLower(encoded) {
		return nil, errors.New("maker message key must be 32 lowercase hexadecimal bytes")
	}
	allZero := true
	for _, value := range decoded {
		allZero = allZero && value == 0
	}
	if allZero {
		return nil, errors.New("maker message key must not be zero")
	}
	return decoded, nil
}

func parseSignature(encoded string) ([]byte, error) {
	decoded, err := hex.DecodeString(encoded)
	if err != nil || len(decoded) != ed25519.SignatureSize || encoded != strings.ToLower(encoded) {
		return nil, errors.New("signature must be 64 lowercase hexadecimal bytes")
	}
	return decoded, nil
}

func parseDigest(encoded, name string) ([]byte, error) {
	decoded, err := hex.DecodeString(encoded)
	if err != nil || len(decoded) != sha256.Size || encoded != strings.ToLower(encoded) {
		return nil, fmt.Errorf("%s must be 32 lowercase hexadecimal bytes", name)
	}
	return decoded, nil
}

func PriceBTCPerQDAY(s Signed) *big.Rat {
	give, okGive := new(big.Int).SetString(s.Order.Give.Atomic, 10)
	receive, okReceive := new(big.Int).SetString(s.Order.Receive.Atomic, 10)
	qdayUnit, okUnit := new(big.Int).SetString(s.Order.QDAYUnitAtomic, 10)
	if !okGive || !okReceive || !okUnit || give.Sign() <= 0 || receive.Sign() <= 0 || qdayUnit.Sign() <= 0 {
		return new(big.Rat)
	}
	var qdayAtomic, satoshis *big.Int
	if s.Order.Give.Asset == "QDAY" {
		qdayAtomic, satoshis = give, receive
	} else {
		qdayAtomic, satoshis = receive, give
	}
	numerator := new(big.Int).Mul(satoshis, qdayUnit)
	denominator := new(big.Int).Mul(qdayAtomic, big.NewInt(100_000_000))
	return new(big.Rat).SetFrac(numerator, denominator)
}

type canonicalEncoder struct{ bytes []byte }

func (e *canonicalEncoder) uint16(value uint16) {
	var buffer [2]byte
	binary.BigEndian.PutUint16(buffer[:], value)
	e.bytes = append(e.bytes, buffer[:]...)
}

func (e *canonicalEncoder) int64(value int64) {
	var buffer [8]byte
	binary.BigEndian.PutUint64(buffer[:], uint64(value))
	e.bytes = append(e.bytes, buffer[:]...)
}

func (e *canonicalEncoder) uint32(value uint32) {
	var buffer [4]byte
	binary.BigEndian.PutUint32(buffer[:], value)
	e.bytes = append(e.bytes, buffer[:]...)
}

func (e *canonicalEncoder) uint64(value uint64) {
	var buffer [8]byte
	binary.BigEndian.PutUint64(buffer[:], value)
	e.bytes = append(e.bytes, buffer[:]...)
}

func (e *canonicalEncoder) text(value string) {
	var buffer [4]byte
	binary.BigEndian.PutUint32(buffer[:], uint32(len(value)))
	e.bytes = append(e.bytes, buffer[:]...)
	e.bytes = append(e.bytes, value...)
}
