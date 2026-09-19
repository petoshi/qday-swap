// Package trade defines the signed negotiation records and encrypted relay
// messages used after a public order is selected. The relay can authenticate
// routing metadata, but it cannot decrypt a message or alter its contents.
package trade

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/petoshi/qday-swap/internal/order"
	"golang.org/x/crypto/nacl/box"
)

const (
	ProtocolVersion    = uint16(1)
	MaxPlaintextSize   = 32 << 10
	MaxMailboxPageSize = 100

	minimumAcceptanceLifetime = time.Minute
	maximumAcceptanceLifetime = 15 * time.Minute
	minimumMessageLifetime    = time.Minute
	maximumMessageLifetime    = 30 * 24 * time.Hour
	maximumClockSkew          = 5 * time.Minute
)

var (
	acceptanceDomain = []byte("QDAY_SWAP_ACCEPTANCE_V1")
	matchDomain      = []byte("QDAY_SWAP_MATCH_V1")
	messageDomain    = []byte("QDAY_SWAP_MESSAGE_V1")
	pollDomain       = []byte("QDAY_SWAP_MAILBOX_POLL_V1")
)

type AcceptancePayload struct {
	Version         uint16 `json:"version"`
	Network         string `json:"network"`
	OrderID         string `json:"orderID"`
	TradeID         string `json:"tradeID"`
	TakerPublicKey  string `json:"takerPublicKey"`
	TakerMessageKey string `json:"takerMessageKey"`
	CreatedAt       int64  `json:"createdAt"`
	ExpiresAt       int64  `json:"expiresAt"`
}

type SignedAcceptance struct {
	Acceptance AcceptancePayload `json:"acceptance"`
	ID         string            `json:"id"`
	Signature  string            `json:"signature"`
}

func NewAcceptance(signedOrder order.Signed, lifetime time.Duration, privateKey ed25519.PrivateKey, messageKey [32]byte, now time.Time) (SignedAcceptance, error) {
	if lifetime < minimumAcceptanceLifetime || lifetime > maximumAcceptanceLifetime {
		return SignedAcceptance{}, fmt.Errorf("acceptance lifetime must be between %s and %s", minimumAcceptanceLifetime, maximumAcceptanceLifetime)
	}
	if err := signedOrder.Verify(now); err != nil {
		return SignedAcceptance{}, fmt.Errorf("verify order: %w", err)
	}
	if len(privateKey) != ed25519.PrivateKeySize {
		return SignedAcceptance{}, errors.New("invalid Ed25519 private key")
	}
	tradeID := make([]byte, sha256.Size)
	if _, err := rand.Read(tradeID); err != nil {
		return SignedAcceptance{}, fmt.Errorf("generate trade ID: %w", err)
	}
	expiresAt := now.Add(lifetime).Unix()
	if expiresAt > signedOrder.Order.ExpiresAt {
		expiresAt = signedOrder.Order.ExpiresAt
	}
	payload := AcceptancePayload{
		Version:         ProtocolVersion,
		Network:         signedOrder.Order.Network,
		OrderID:         signedOrder.ID,
		TradeID:         hex.EncodeToString(tradeID),
		TakerPublicKey:  hex.EncodeToString(privateKey.Public().(ed25519.PublicKey)),
		TakerMessageKey: hex.EncodeToString(messageKey[:]),
		CreatedAt:       now.Unix(),
		ExpiresAt:       expiresAt,
	}
	if err := payload.validate(signedOrder, now); err != nil {
		return SignedAcceptance{}, err
	}
	digest := sha256.Sum256(payload.signingBytes())
	return SignedAcceptance{
		Acceptance: payload,
		ID:         hex.EncodeToString(digest[:]),
		Signature:  hex.EncodeToString(ed25519.Sign(privateKey, digest[:])),
	}, nil
}

func (a SignedAcceptance) Verify(signedOrder order.Signed, now time.Time) error {
	if err := signedOrder.Verify(now); err != nil {
		return fmt.Errorf("verify order: %w", err)
	}
	if err := a.Acceptance.validate(signedOrder, now); err != nil {
		return err
	}
	digest := sha256.Sum256(a.Acceptance.signingBytes())
	if a.ID != hex.EncodeToString(digest[:]) {
		return errors.New("acceptance ID does not match signed terms")
	}
	publicKey, err := decodeSigningKey(a.Acceptance.TakerPublicKey, "taker public key")
	if err != nil {
		return err
	}
	signature, err := decodeSignature(a.Signature)
	if err != nil {
		return err
	}
	if !ed25519.Verify(publicKey, digest[:], signature) {
		return errors.New("invalid acceptance signature")
	}
	return nil
}

func (p AcceptancePayload) validate(signedOrder order.Signed, now time.Time) error {
	if p.Version != ProtocolVersion {
		return fmt.Errorf("unsupported acceptance version %d", p.Version)
	}
	if p.Network != signedOrder.Order.Network || p.OrderID != signedOrder.ID {
		return errors.New("acceptance does not belong to this order")
	}
	if _, err := decodeDigest(p.TradeID, "trade ID"); err != nil {
		return err
	}
	if _, err := decodeSigningKey(p.TakerPublicKey, "taker public key"); err != nil {
		return err
	}
	if p.TakerPublicKey == signedOrder.Order.MakerPublicKey {
		return errors.New("maker cannot accept its own order")
	}
	if _, err := DecodeMessageKey(p.TakerMessageKey); err != nil {
		return fmt.Errorf("taker message key: %w", err)
	}
	created := time.Unix(p.CreatedAt, 0)
	expires := time.Unix(p.ExpiresAt, 0)
	if created.After(now.Add(maximumClockSkew)) {
		return errors.New("acceptance creation time is too far in the future")
	}
	if !expires.After(now) {
		return errors.New("acceptance has expired")
	}
	if p.ExpiresAt > signedOrder.Order.ExpiresAt {
		return errors.New("acceptance outlives its order")
	}
	lifetime := expires.Sub(created)
	if lifetime < minimumAcceptanceLifetime || lifetime > maximumAcceptanceLifetime {
		return fmt.Errorf("acceptance lifetime must be between %s and %s", minimumAcceptanceLifetime, maximumAcceptanceLifetime)
	}
	return nil
}

func (p AcceptancePayload) signingBytes() []byte {
	encoder := canonicalEncoder{bytes: append([]byte(nil), acceptanceDomain...)}
	encoder.uint16(p.Version)
	encoder.text(p.Network)
	encoder.text(p.OrderID)
	encoder.text(p.TradeID)
	encoder.text(p.TakerPublicKey)
	encoder.text(p.TakerMessageKey)
	encoder.int64(p.CreatedAt)
	encoder.int64(p.ExpiresAt)
	return encoder.bytes
}

type MatchPayload struct {
	Version        uint16 `json:"version"`
	Network        string `json:"network"`
	OrderID        string `json:"orderID"`
	TradeID        string `json:"tradeID"`
	AcceptanceID   string `json:"acceptanceID"`
	MakerPublicKey string `json:"makerPublicKey"`
	TakerPublicKey string `json:"takerPublicKey"`
	CreatedAt      int64  `json:"createdAt"`
}

type SignedMatch struct {
	Match     MatchPayload `json:"match"`
	ID        string       `json:"id"`
	Signature string       `json:"signature"`
}

func NewMatch(signedOrder order.Signed, acceptance SignedAcceptance, privateKey ed25519.PrivateKey, now time.Time) (SignedMatch, error) {
	if err := acceptance.Verify(signedOrder, now); err != nil {
		return SignedMatch{}, fmt.Errorf("verify acceptance: %w", err)
	}
	if len(privateKey) != ed25519.PrivateKeySize || hex.EncodeToString(privateKey.Public().(ed25519.PublicKey)) != signedOrder.Order.MakerPublicKey {
		return SignedMatch{}, errors.New("private key does not match order maker")
	}
	payload := MatchPayload{
		Version:        ProtocolVersion,
		Network:        signedOrder.Order.Network,
		OrderID:        signedOrder.ID,
		TradeID:        acceptance.Acceptance.TradeID,
		AcceptanceID:   acceptance.ID,
		MakerPublicKey: signedOrder.Order.MakerPublicKey,
		TakerPublicKey: acceptance.Acceptance.TakerPublicKey,
		CreatedAt:      now.Unix(),
	}
	digest := sha256.Sum256(payload.signingBytes())
	return SignedMatch{Match: payload, ID: hex.EncodeToString(digest[:]), Signature: hex.EncodeToString(ed25519.Sign(privateKey, digest[:]))}, nil
}

func (m SignedMatch) Verify(signedOrder order.Signed, acceptance SignedAcceptance, now time.Time) error {
	if err := acceptance.Verify(signedOrder, now); err != nil {
		return fmt.Errorf("verify acceptance: %w", err)
	}
	payload := m.Match
	if payload.Version != ProtocolVersion {
		return fmt.Errorf("unsupported match version %d", payload.Version)
	}
	if payload.Network != signedOrder.Order.Network || payload.OrderID != signedOrder.ID || payload.TradeID != acceptance.Acceptance.TradeID || payload.AcceptanceID != acceptance.ID {
		return errors.New("match does not bind the selected order and acceptance")
	}
	if payload.MakerPublicKey != signedOrder.Order.MakerPublicKey || payload.TakerPublicKey != acceptance.Acceptance.TakerPublicKey {
		return errors.New("match participants do not match the order and acceptance")
	}
	created := time.Unix(payload.CreatedAt, 0)
	if created.After(now.Add(maximumClockSkew)) {
		return errors.New("match creation time is too far in the future")
	}
	if payload.CreatedAt < acceptance.Acceptance.CreatedAt || payload.CreatedAt > acceptance.Acceptance.ExpiresAt {
		return errors.New("match was not created during the acceptance window")
	}
	digest := sha256.Sum256(payload.signingBytes())
	if m.ID != hex.EncodeToString(digest[:]) {
		return errors.New("match ID does not match signed terms")
	}
	publicKey, err := decodeSigningKey(payload.MakerPublicKey, "maker public key")
	if err != nil {
		return err
	}
	signature, err := decodeSignature(m.Signature)
	if err != nil {
		return err
	}
	if !ed25519.Verify(publicKey, digest[:], signature) {
		return errors.New("invalid match signature")
	}
	return nil
}

func (p MatchPayload) signingBytes() []byte {
	encoder := canonicalEncoder{bytes: append([]byte(nil), matchDomain...)}
	encoder.uint16(p.Version)
	encoder.text(p.Network)
	encoder.text(p.OrderID)
	encoder.text(p.TradeID)
	encoder.text(p.AcceptanceID)
	encoder.text(p.MakerPublicKey)
	encoder.text(p.TakerPublicKey)
	encoder.int64(p.CreatedAt)
	return encoder.bytes
}

type MessagePayload struct {
	Version            uint16 `json:"version"`
	Network            string `json:"network"`
	OrderID            string `json:"orderID"`
	TradeID            string `json:"tradeID"`
	Sequence           uint64 `json:"sequence"`
	SenderPublicKey    string `json:"senderPublicKey"`
	RecipientPublicKey string `json:"recipientPublicKey"`
	CreatedAt          int64  `json:"createdAt"`
	ExpiresAt          int64  `json:"expiresAt"`
	Nonce              string `json:"nonce"`
	Ciphertext         string `json:"ciphertext"`
}

type SignedMessage struct {
	Message   MessagePayload `json:"message"`
	ID        string         `json:"id"`
	Signature string         `json:"signature"`
}

func EncryptMessage(network, orderID, tradeID string, sequence uint64, senderPrivate ed25519.PrivateKey, recipientPublic ed25519.PublicKey, senderMessagePrivate, recipientMessagePublic [32]byte, plaintext []byte, lifetime time.Duration, now time.Time) (SignedMessage, error) {
	if len(plaintext) == 0 || len(plaintext) > MaxPlaintextSize {
		return SignedMessage{}, fmt.Errorf("message plaintext must contain 1 to %d bytes", MaxPlaintextSize)
	}
	if lifetime < minimumMessageLifetime || lifetime > maximumMessageLifetime {
		return SignedMessage{}, fmt.Errorf("message lifetime must be between %s and %s", minimumMessageLifetime, maximumMessageLifetime)
	}
	if len(senderPrivate) != ed25519.PrivateKeySize || len(recipientPublic) != ed25519.PublicKeySize {
		return SignedMessage{}, errors.New("invalid message participant signing key")
	}
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return SignedMessage{}, fmt.Errorf("generate message nonce: %w", err)
	}
	ciphertext := box.Seal(nil, plaintext, &nonce, &recipientMessagePublic, &senderMessagePrivate)
	payload := MessagePayload{
		Version:            ProtocolVersion,
		Network:            network,
		OrderID:            orderID,
		TradeID:            tradeID,
		Sequence:           sequence,
		SenderPublicKey:    hex.EncodeToString(senderPrivate.Public().(ed25519.PublicKey)),
		RecipientPublicKey: hex.EncodeToString(recipientPublic),
		CreatedAt:          now.Unix(),
		ExpiresAt:          now.Add(lifetime).Unix(),
		Nonce:              hex.EncodeToString(nonce[:]),
		Ciphertext:         base64.RawStdEncoding.EncodeToString(ciphertext),
	}
	if err := payload.validate(now); err != nil {
		return SignedMessage{}, err
	}
	digest := sha256.Sum256(payload.signingBytes())
	return SignedMessage{Message: payload, ID: hex.EncodeToString(digest[:]), Signature: hex.EncodeToString(ed25519.Sign(senderPrivate, digest[:]))}, nil
}

func (m SignedMessage) Verify(now time.Time) error {
	if err := m.Message.validate(now); err != nil {
		return err
	}
	digest := sha256.Sum256(m.Message.signingBytes())
	if m.ID != hex.EncodeToString(digest[:]) {
		return errors.New("message ID does not match signed envelope")
	}
	publicKey, err := decodeSigningKey(m.Message.SenderPublicKey, "sender public key")
	if err != nil {
		return err
	}
	signature, err := decodeSignature(m.Signature)
	if err != nil {
		return err
	}
	if !ed25519.Verify(publicKey, digest[:], signature) {
		return errors.New("invalid message signature")
	}
	return nil
}

func (m SignedMessage) Decrypt(recipientPublic ed25519.PublicKey, recipientMessagePrivate, senderMessagePublic [32]byte, now time.Time) ([]byte, error) {
	if err := m.Verify(now); err != nil {
		return nil, err
	}
	if m.Message.RecipientPublicKey != hex.EncodeToString(recipientPublic) {
		return nil, errors.New("message is addressed to a different recipient")
	}
	nonceBytes, _ := hex.DecodeString(m.Message.Nonce)
	var nonce [24]byte
	copy(nonce[:], nonceBytes)
	ciphertext, _ := base64.RawStdEncoding.DecodeString(m.Message.Ciphertext)
	plaintext, ok := box.Open(nil, ciphertext, &nonce, &senderMessagePublic, &recipientMessagePrivate)
	if !ok {
		return nil, errors.New("message authentication failed")
	}
	return plaintext, nil
}

func (p MessagePayload) validate(now time.Time) error {
	if p.Version != ProtocolVersion {
		return fmt.Errorf("unsupported message version %d", p.Version)
	}
	if p.Network != "mainnet" && p.Network != "testnet" && p.Network != "regtest" {
		return errors.New("message network is invalid")
	}
	if _, err := decodeDigest(p.OrderID, "order ID"); err != nil {
		return err
	}
	if _, err := decodeDigest(p.TradeID, "trade ID"); err != nil {
		return err
	}
	if p.Sequence == 0 {
		return errors.New("message sequence starts at one")
	}
	if _, err := decodeSigningKey(p.SenderPublicKey, "sender public key"); err != nil {
		return err
	}
	if _, err := decodeSigningKey(p.RecipientPublicKey, "recipient public key"); err != nil {
		return err
	}
	if p.SenderPublicKey == p.RecipientPublicKey {
		return errors.New("message sender and recipient must differ")
	}
	nonce, err := hex.DecodeString(p.Nonce)
	if err != nil || len(nonce) != 24 || p.Nonce != strings.ToLower(p.Nonce) {
		return errors.New("message nonce must be 24 lowercase hexadecimal bytes")
	}
	ciphertext, err := base64.RawStdEncoding.DecodeString(p.Ciphertext)
	if err != nil || base64.RawStdEncoding.EncodeToString(ciphertext) != p.Ciphertext || len(ciphertext) <= box.Overhead || len(ciphertext) > MaxPlaintextSize+box.Overhead {
		return errors.New("message ciphertext is invalid or too large")
	}
	created := time.Unix(p.CreatedAt, 0)
	expires := time.Unix(p.ExpiresAt, 0)
	if created.After(now.Add(maximumClockSkew)) {
		return errors.New("message creation time is too far in the future")
	}
	if !expires.After(now) {
		return errors.New("message has expired")
	}
	lifetime := expires.Sub(created)
	if lifetime < minimumMessageLifetime || lifetime > maximumMessageLifetime {
		return fmt.Errorf("message lifetime must be between %s and %s", minimumMessageLifetime, maximumMessageLifetime)
	}
	return nil
}

func (p MessagePayload) signingBytes() []byte {
	encoder := canonicalEncoder{bytes: append([]byte(nil), messageDomain...)}
	encoder.uint16(p.Version)
	encoder.text(p.Network)
	encoder.text(p.OrderID)
	encoder.text(p.TradeID)
	encoder.uint64(p.Sequence)
	encoder.text(p.SenderPublicKey)
	encoder.text(p.RecipientPublicKey)
	encoder.int64(p.CreatedAt)
	encoder.int64(p.ExpiresAt)
	encoder.text(p.Nonce)
	encoder.text(p.Ciphertext)
	return encoder.bytes
}

type PollPayload struct {
	Version            uint16 `json:"version"`
	Network            string `json:"network"`
	RecipientPublicKey string `json:"recipientPublicKey"`
	After              uint64 `json:"after"`
	Limit              uint16 `json:"limit"`
	CreatedAt          int64  `json:"createdAt"`
	Nonce              string `json:"nonce"`
}

type SignedPoll struct {
	Poll      PollPayload `json:"poll"`
	Signature string      `json:"signature"`
}

func NewPoll(network string, after uint64, limit uint16, privateKey ed25519.PrivateKey, now time.Time) (SignedPoll, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return SignedPoll{}, errors.New("invalid Ed25519 private key")
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return SignedPoll{}, fmt.Errorf("generate poll nonce: %w", err)
	}
	payload := PollPayload{
		Version: ProtocolVersion, Network: network,
		RecipientPublicKey: hex.EncodeToString(privateKey.Public().(ed25519.PublicKey)),
		After:              after, Limit: limit, CreatedAt: now.Unix(), Nonce: hex.EncodeToString(nonce),
	}
	if err := payload.validate(now); err != nil {
		return SignedPoll{}, err
	}
	digest := sha256.Sum256(payload.signingBytes())
	return SignedPoll{Poll: payload, Signature: hex.EncodeToString(ed25519.Sign(privateKey, digest[:]))}, nil
}

func (p SignedPoll) Verify(now time.Time) error {
	if err := p.Poll.validate(now); err != nil {
		return err
	}
	publicKey, err := decodeSigningKey(p.Poll.RecipientPublicKey, "recipient public key")
	if err != nil {
		return err
	}
	signature, err := decodeSignature(p.Signature)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(p.Poll.signingBytes())
	if !ed25519.Verify(publicKey, digest[:], signature) {
		return errors.New("invalid mailbox poll signature")
	}
	return nil
}

func (p PollPayload) validate(now time.Time) error {
	if p.Version != ProtocolVersion {
		return fmt.Errorf("unsupported mailbox poll version %d", p.Version)
	}
	if p.Network != "mainnet" && p.Network != "testnet" && p.Network != "regtest" {
		return errors.New("mailbox poll network is invalid")
	}
	if _, err := decodeSigningKey(p.RecipientPublicKey, "recipient public key"); err != nil {
		return err
	}
	if p.Limit == 0 || p.Limit > MaxMailboxPageSize {
		return fmt.Errorf("mailbox page size must be between 1 and %d", MaxMailboxPageSize)
	}
	created := time.Unix(p.CreatedAt, 0)
	if created.Before(now.Add(-maximumClockSkew)) || created.After(now.Add(maximumClockSkew)) {
		return errors.New("mailbox poll time is outside the allowed clock window")
	}
	nonce, err := hex.DecodeString(p.Nonce)
	if err != nil || len(nonce) != 16 || p.Nonce != strings.ToLower(p.Nonce) {
		return errors.New("mailbox poll nonce must be 16 lowercase hexadecimal bytes")
	}
	return nil
}

func (p PollPayload) signingBytes() []byte {
	encoder := canonicalEncoder{bytes: append([]byte(nil), pollDomain...)}
	encoder.uint16(p.Version)
	encoder.text(p.Network)
	encoder.text(p.RecipientPublicKey)
	encoder.uint64(p.After)
	encoder.uint16(p.Limit)
	encoder.int64(p.CreatedAt)
	encoder.text(p.Nonce)
	return encoder.bytes
}

func DecodeMessageKey(encoded string) ([32]byte, error) {
	var key [32]byte
	decoded, err := hex.DecodeString(encoded)
	if err != nil || len(decoded) != len(key) || encoded != strings.ToLower(encoded) {
		return key, errors.New("must be 32 lowercase hexadecimal bytes")
	}
	allZero := true
	for _, value := range decoded {
		allZero = allZero && value == 0
	}
	if allZero {
		return key, errors.New("must not be zero")
	}
	copy(key[:], decoded)
	return key, nil
}

func decodeSigningKey(encoded, name string) (ed25519.PublicKey, error) {
	decoded, err := hex.DecodeString(encoded)
	if err != nil || len(decoded) != ed25519.PublicKeySize || encoded != strings.ToLower(encoded) {
		return nil, fmt.Errorf("%s must be 32 lowercase hexadecimal bytes", name)
	}
	return ed25519.PublicKey(decoded), nil
}

func decodeSignature(encoded string) ([]byte, error) {
	decoded, err := hex.DecodeString(encoded)
	if err != nil || len(decoded) != ed25519.SignatureSize || encoded != strings.ToLower(encoded) {
		return nil, errors.New("signature must be 64 lowercase hexadecimal bytes")
	}
	return decoded, nil
}

func decodeDigest(encoded, name string) ([]byte, error) {
	decoded, err := hex.DecodeString(encoded)
	if err != nil || len(decoded) != sha256.Size || encoded != strings.ToLower(encoded) {
		return nil, fmt.Errorf("%s must be 32 lowercase hexadecimal bytes", name)
	}
	return decoded, nil
}

type canonicalEncoder struct{ bytes []byte }

func (e *canonicalEncoder) uint16(value uint16) {
	var buffer [2]byte
	binary.BigEndian.PutUint16(buffer[:], value)
	e.bytes = append(e.bytes, buffer[:]...)
}

func (e *canonicalEncoder) uint64(value uint64) {
	var buffer [8]byte
	binary.BigEndian.PutUint64(buffer[:], value)
	e.bytes = append(e.bytes, buffer[:]...)
}

func (e *canonicalEncoder) int64(value int64) { e.uint64(uint64(value)) }

func (e *canonicalEncoder) text(value string) {
	var buffer [4]byte
	binary.BigEndian.PutUint32(buffer[:], uint32(len(value)))
	e.bytes = append(e.bytes, buffer[:]...)
	e.bytes = append(e.bytes, value...)
}
