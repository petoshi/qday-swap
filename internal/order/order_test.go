package order

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
)

func signedTestOrder(t *testing.T, now time.Time) (Signed, ed25519.PrivateKey) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	messageKey := [32]byte{1}
	payload, err := NewPayload("mainnet", QDAYLegacyUnit, Amount{Asset: "QDAY", Atomic: "2500000000000000000000000"}, Amount{Asset: "BTC", Atomic: "150000"}, time.Hour, publicKey, messageKey, now)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := Sign(payload, privateKey, now)
	if err != nil {
		t.Fatal(err)
	}
	return signed, privateKey
}

func signedAsyncTestOrder(t *testing.T, now time.Time) (Signed, ed25519.PrivateKey) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	bitcoinKey, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	payload, err := NewAsyncPayload(
		"mainnet", QDAYDefendUnit,
		Amount{Asset: "QDAY", Atomic: "2500000000000000000"},
		Amount{Asset: "BTC", Atomic: "150000"},
		time.Hour, publicKey, [32]byte{1}, strings.Repeat("2", 64),
		QDAYKeys{Classical: strings.Repeat("3", 64), Reserve: strings.Repeat("4", 64), Address: "qday1maker"},
		hex.EncodeToString(bitcoinKey.PubKey().SerializeCompressed()), 12_000, 900_000, now,
	)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := Sign(payload, privateKey, now)
	if err != nil {
		t.Fatal(err)
	}
	return signed, privateKey
}

func TestAsyncOrderBindsSwapDescriptors(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	signed, _ := signedAsyncTestOrder(t, now)
	if err := signed.Verify(now); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Signed){
		func(value *Signed) { value.Order.SessionID = strings.Repeat("5", 64) },
		func(value *Signed) { value.Order.MakerQDAY.Address = "qday1changed" },
		func(value *Signed) { value.Order.MakerBitcoinKey = "02" + strings.Repeat("0", 64) },
		func(value *Signed) { value.Order.MakerQDAYHeight++ },
		func(value *Signed) { value.Order.MakerBTCHeight++ },
	} {
		tampered := signed
		mutate(&tampered)
		if err := tampered.Verify(now); err == nil {
			t.Fatal("tampered asynchronous order verified")
		}
	}
}

func TestSignedOrderRoundTripAndTampering(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	signed, _ := signedTestOrder(t, now)
	if err := signed.Verify(now); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		mutate func(*Signed)
	}{
		{"amount", func(order *Signed) { order.Order.Give.Atomic = "1" }},
		{"asset", func(order *Signed) { order.Order.Give.Asset = "BTC" }},
		{"expiry", func(order *Signed) { order.Order.ExpiresAt++ }},
		{"id", func(order *Signed) { order.ID = strings.Repeat("0", 64) }},
		{"signature", func(order *Signed) { order.Signature = strings.Repeat("0", 128) }},
		{"message key", func(order *Signed) { order.Order.MakerMessageKey = strings.Repeat("1", 64) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tampered := signed
			test.mutate(&tampered)
			if err := tampered.Verify(now); err == nil {
				t.Fatal("tampered order verified")
			}
		})
	}
}

func TestOrderValidationBoundaries(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	signed, privateKey := signedTestOrder(t, now)
	tests := []struct {
		name   string
		mutate func(*Payload)
	}{
		{"expired", func(order *Payload) { order.ExpiresAt = now.Unix() }},
		{"future", func(order *Payload) {
			order.CreatedAt = now.Add(6 * time.Minute).Unix()
			order.ExpiresAt = now.Add(time.Hour).Unix()
		}},
		{"zero", func(order *Payload) { order.Give.Atomic = "0" }},
		{"leading zero", func(order *Payload) { order.Give.Atomic = "01" }},
		{"decimal", func(order *Payload) { order.Give.Atomic = "1.5" }},
		{"Bitcoin below swap minimum", func(order *Payload) { order.Receive.Atomic = "9999" }},
		{"wrong pair", func(order *Payload) { order.Receive.Asset = "LTC" }},
		{"too long", func(order *Payload) { order.ExpiresAt = order.CreatedAt + int64((31*24*time.Hour)/time.Second) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload := signed.Order
			test.mutate(&payload)
			if _, err := Sign(payload, privateKey, now); err == nil {
				t.Fatal("invalid order signed")
			}
		})
	}
}

func TestSignedCancellation(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	signed, privateKey := signedTestOrder(t, now)
	cancellation, err := NewCancellation(signed, privateKey, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	} else if err := cancellation.Verify(now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	tampered := cancellation
	tampered.Cancellation.OrderID = strings.Repeat("0", 64)
	if err := tampered.Verify(now.Add(time.Minute)); err == nil {
		t.Fatal("tampered cancellation verified")
	}
}

func TestPriceBTCPerQDAYIsExact(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	signed, _ := signedTestOrder(t, now)
	if price := PriceBTCPerQDAY(signed); price.RatString() != "3/5000" {
		t.Fatalf("unexpected price %s", price.RatString())
	}
	reversed := signed
	reversed.Order.Give, reversed.Order.Receive = signed.Order.Receive, signed.Order.Give
	if price := PriceBTCPerQDAY(reversed); price.RatString() != "3/5000" {
		t.Fatalf("unexpected reversed price %s", price.RatString())
	}
}
