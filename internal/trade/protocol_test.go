package trade

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"github.com/petoshi/qday-swap/internal/order"
	"golang.org/x/crypto/nacl/box"
)

type protocolFixture struct {
	order                      order.Signed
	makerPrivate, takerPrivate ed25519.PrivateKey
	makerMessagePrivate        [32]byte
	makerMessagePublic         [32]byte
	takerMessagePrivate        [32]byte
	takerMessagePublic         [32]byte
	acceptance                 SignedAcceptance
	match                      SignedMatch
}

func newProtocolFixture(t *testing.T, now time.Time) protocolFixture {
	t.Helper()
	makerPublic, makerPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	takerPublic, takerPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_ = takerPublic
	makerMessagePublic, makerMessagePrivate, err := box.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	takerMessagePublic, takerMessagePrivate, err := box.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := order.NewPayload("mainnet", order.QDAYDefendUnit,
		order.Amount{Asset: "QDAY", Atomic: "2500000000000000000"},
		order.Amount{Asset: "BTC", Atomic: "150000"}, time.Hour, makerPublic, *makerMessagePublic, now)
	if err != nil {
		t.Fatal(err)
	}
	signedOrder, err := order.Sign(payload, makerPrivate, now)
	if err != nil {
		t.Fatal(err)
	}
	acceptance, err := NewAcceptance(signedOrder, 10*time.Minute, takerPrivate, *takerMessagePublic, now)
	if err != nil {
		t.Fatal(err)
	}
	match, err := NewMatch(signedOrder, acceptance, makerPrivate, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return protocolFixture{signedOrder, makerPrivate, takerPrivate, *makerMessagePrivate, *makerMessagePublic, *takerMessagePrivate, *takerMessagePublic, acceptance, match}
}

func TestAcceptanceAndMatchBindEveryParticipant(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	fixture := newProtocolFixture(t, now)
	if err := fixture.acceptance.Verify(fixture.order, now); err != nil {
		t.Fatal(err)
	}
	if err := fixture.match.Verify(fixture.order, fixture.acceptance, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	tamperedAcceptance := fixture.acceptance
	tamperedAcceptance.Acceptance.TradeID = strings.Repeat("1", 64)
	if err := tamperedAcceptance.Verify(fixture.order, now); err == nil {
		t.Fatal("tampered acceptance verified")
	}
	tamperedMatch := fixture.match
	tamperedMatch.Match.TakerPublicKey = fixture.order.Order.MakerPublicKey
	if err := tamperedMatch.Verify(fixture.order, fixture.acceptance, now.Add(time.Second)); err == nil {
		t.Fatal("tampered match verified")
	}
}

func TestEncryptedMessageRoundTripAndTampering(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	fixture := newProtocolFixture(t, now)
	takerPublic := fixture.takerPrivate.Public().(ed25519.PublicKey)
	message, err := EncryptMessage("mainnet", fixture.order.ID, fixture.acceptance.Acceptance.TradeID, 1,
		fixture.makerPrivate, takerPublic, fixture.makerMessagePrivate, fixture.takerMessagePublic,
		[]byte(`{"kind":"contract","payload":"public terms only"}`), time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := message.Decrypt(takerPublic, fixture.takerMessagePrivate, fixture.makerMessagePublic, now)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(plaintext, []byte(`"kind":"contract"`)) {
		t.Fatalf("unexpected plaintext %q", plaintext)
	}

	tampered := message
	tampered.Message.Ciphertext = message.Message.Ciphertext[:len(message.Message.Ciphertext)-1] + "A"
	if _, err := tampered.Decrypt(takerPublic, fixture.takerMessagePrivate, fixture.makerMessagePublic, now); err == nil {
		t.Fatal("tampered ciphertext decrypted")
	}
	wrongPublic, _, err := box.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := message.Decrypt(takerPublic, fixture.takerMessagePrivate, *wrongPublic, now); err == nil {
		t.Fatal("message decrypted with the wrong sender key")
	}
}

func TestMailboxPollSignatureAndWindow(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	poll, err := NewPoll("mainnet", 42, 20, privateKey, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := poll.Verify(now); err != nil {
		t.Fatal(err)
	}
	if err := poll.Verify(now.Add(6 * time.Minute)); err == nil {
		t.Fatal("stale mailbox authentication verified")
	}
	tampered := poll
	tampered.Poll.After++
	if err := tampered.Verify(now); err == nil {
		t.Fatal("tampered mailbox cursor verified")
	}
}
