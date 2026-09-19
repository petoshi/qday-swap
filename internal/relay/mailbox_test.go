package relay

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/petoshi/qday-swap/internal/order"
	"github.com/petoshi/qday-swap/internal/trade"
	"golang.org/x/crypto/nacl/box"
)

type relayTradeFixture struct {
	order                                   order.Signed
	makerPrivate, takerPrivate              ed25519.PrivateKey
	makerMessagePrivate, makerMessagePublic [32]byte
	takerMessagePrivate, takerMessagePublic [32]byte
	acceptance                              trade.SignedAcceptance
	match                                   trade.SignedMatch
}

func newRelayTradeFixture(t *testing.T, now time.Time) relayTradeFixture {
	t.Helper()
	makerPublic, makerPrivate := testSigner(t)
	_, takerPrivate := testSigner(t)
	makerMessagePublic, makerMessagePrivate, err := box.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	takerMessagePublic, takerMessagePrivate, err := box.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := order.NewPayload("mainnet", order.QDAYDefendUnit,
		order.Amount{Asset: "QDAY", Atomic: "1000000000000000000"},
		order.Amount{Asset: "BTC", Atomic: "100000"}, time.Hour,
		makerPublic, *makerMessagePublic, now)
	if err != nil {
		t.Fatal(err)
	}
	signedOrder, err := order.Sign(payload, makerPrivate, now)
	if err != nil {
		t.Fatal(err)
	}
	acceptance, err := trade.NewAcceptance(signedOrder, 10*time.Minute, takerPrivate, *takerMessagePublic, now)
	if err != nil {
		t.Fatal(err)
	}
	match, err := trade.NewMatch(signedOrder, acceptance, makerPrivate, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return relayTradeFixture{
		order: signedOrder, makerPrivate: makerPrivate, takerPrivate: takerPrivate,
		makerMessagePrivate: *makerMessagePrivate, makerMessagePublic: *makerMessagePublic,
		takerMessagePrivate: *takerMessagePrivate, takerMessagePublic: *takerMessagePublic,
		acceptance: acceptance, match: match,
	}
}

func TestDurableNegotiationMailbox(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	fixture := newRelayTradeFixture(t, now)
	store := openTestStore(t)
	if _, _, err := store.Publish(fixture.order, now); err != nil {
		t.Fatal(err)
	}
	if _, created, err := store.SubmitAcceptance(fixture.order.ID, fixture.acceptance, now); err != nil || !created {
		t.Fatalf("submit acceptance: created=%v err=%v", created, err)
	}
	if _, created, err := store.SubmitAcceptance(fixture.order.ID, fixture.acceptance, now); err != nil || created {
		t.Fatalf("idempotent acceptance: created=%v err=%v", created, err)
	}

	makerPoll, err := trade.NewPoll("mainnet", 0, 20, fixture.makerPrivate, now)
	if err != nil {
		t.Fatal(err)
	}
	makerMailbox, err := store.PollMailbox(makerPoll, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(makerMailbox.Items) != 1 || makerMailbox.Items[0].Kind != MailboxAcceptance || makerMailbox.Items[0].Acceptance.Signed.ID != fixture.acceptance.ID {
		t.Fatalf("unexpected maker mailbox %#v", makerMailbox)
	}

	record, changed, err := store.ConfirmMatch(fixture.order.ID, fixture.match, now.Add(time.Second))
	if err != nil || !changed || record.Status != StatusMatched {
		t.Fatalf("confirm match: status=%q changed=%v err=%v", record.Status, changed, err)
	}
	if record, changed, err = store.ConfirmMatch(fixture.order.ID, fixture.match, now.Add(2*time.Second)); err != nil || changed || record.Match.ID != fixture.match.ID {
		t.Fatalf("idempotent match: changed=%v err=%v record=%#v", changed, err, record)
	}

	message, err := trade.EncryptMessage("mainnet", fixture.order.ID, fixture.acceptance.Acceptance.TradeID, 1,
		fixture.makerPrivate, fixture.takerPrivate.Public().(ed25519.PublicKey),
		fixture.makerMessagePrivate, fixture.takerMessagePublic, []byte("contract terms"), time.Hour, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, created, err := store.PublishMessage(message, now.Add(2*time.Second)); err != nil || !created {
		t.Fatalf("publish message: created=%v err=%v", created, err)
	}
	if _, created, err := store.PublishMessage(message, now.Add(3*time.Second)); err != nil || created {
		t.Fatalf("idempotent message: created=%v err=%v", created, err)
	}

	takerPoll, err := trade.NewPoll("mainnet", 0, 1, fixture.takerPrivate, now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	firstPage, err := store.PollMailbox(takerPoll, now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(firstPage.Items) != 1 || firstPage.Items[0].Kind != MailboxMatch || !firstPage.HasMore {
		t.Fatalf("unexpected first taker page %#v", firstPage)
	}
	takerPoll, err = trade.NewPoll("mainnet", firstPage.Next, 20, fixture.takerPrivate, now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	secondPage, err := store.PollMailbox(takerPoll, now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(secondPage.Items) != 1 || secondPage.Items[0].Kind != MailboxMessage || secondPage.HasMore {
		t.Fatalf("unexpected second taker page %#v", secondPage)
	}
	plaintext, err := secondPage.Items[0].Message.Decrypt(fixture.takerPrivate.Public().(ed25519.PublicKey), fixture.takerMessagePrivate, fixture.makerMessagePublic, now.Add(3*time.Second))
	if err != nil || string(plaintext) != "contract terms" {
		t.Fatalf("decrypt mailbox message: plaintext=%q err=%v", plaintext, err)
	}

	skipped, err := trade.EncryptMessage("mainnet", fixture.order.ID, fixture.acceptance.Acceptance.TradeID, 3,
		fixture.makerPrivate, fixture.takerPrivate.Public().(ed25519.PublicKey),
		fixture.makerMessagePrivate, fixture.takerMessagePublic, []byte("skip"), time.Hour, now.Add(4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.PublishMessage(skipped, now.Add(4*time.Second)); !errors.Is(err, ErrMessageSequence) {
		t.Fatalf("skipped sequence error = %v", err)
	}
}

func TestOnlyOneConcurrentAcceptanceCanBeMatched(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	first := newRelayTradeFixture(t, now)
	store := openTestStore(t)
	if _, _, err := store.Publish(first.order, now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.SubmitAcceptance(first.order.ID, first.acceptance, now); err != nil {
		t.Fatal(err)
	}
	_, secondTakerPrivate := testSigner(t)
	secondMessagePublic, _, err := box.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	secondAcceptance, err := trade.NewAcceptance(first.order, 10*time.Minute, secondTakerPrivate, *secondMessagePublic, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.SubmitAcceptance(first.order.ID, secondAcceptance, now); err != nil {
		t.Fatal(err)
	}
	secondMatch, err := trade.NewMatch(first.order, secondAcceptance, first.makerPrivate, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}

	matches := []trade.SignedMatch{first.match, secondMatch}
	results := make(chan error, len(matches))
	var wait sync.WaitGroup
	for _, match := range matches {
		wait.Add(1)
		go func(candidate trade.SignedMatch) {
			defer wait.Done()
			_, _, err := store.ConfirmMatch(first.order.ID, candidate, now.Add(time.Second))
			results <- err
		}(match)
	}
	wait.Wait()
	close(results)
	succeeded, conflicted := 0, 0
	for err := range results {
		if err == nil {
			succeeded++
		} else if errors.Is(err, ErrNotOpen) {
			conflicted++
		} else {
			t.Fatalf("unexpected match error: %v", err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("successful matches=%d conflicts=%d", succeeded, conflicted)
	}
}

func TestSignedMatchCannotExtendAcceptanceTimeout(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	fixture := newRelayTradeFixture(t, now)
	store := openTestStore(t)
	if _, _, err := store.Publish(fixture.order, now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.SubmitAcceptance(fixture.order.ID, fixture.acceptance, now); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := store.ConfirmMatch(fixture.order.ID, fixture.match, now.Add(11*time.Minute)); err == nil || changed {
		t.Fatalf("expired acceptance matched: changed=%v err=%v", changed, err)
	}
}
