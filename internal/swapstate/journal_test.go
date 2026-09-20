package swapstate

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/petoshi/qday-swap/internal/order"
	"github.com/petoshi/qday-swap/internal/trade"
	"golang.org/x/crypto/nacl/box"
)

func matchedFixture(t *testing.T, now time.Time) (order.Signed, trade.SignedAcceptance, trade.SignedMatch) {
	t.Helper()
	makerPublic, makerPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, takerPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	makerMessage, _, err := box.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	takerMessage, _, err := box.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := order.NewPayload("mainnet", order.QDAYDefendUnit,
		order.Amount{Asset: "QDAY", Atomic: "1000000000000000000"},
		order.Amount{Asset: "BTC", Atomic: "100000"}, time.Hour, makerPublic, *makerMessage, now)
	if err != nil {
		t.Fatal(err)
	}
	signedOrder, err := order.Sign(payload, makerPrivate, now)
	if err != nil {
		t.Fatal(err)
	}
	acceptance, err := trade.NewAcceptance(signedOrder, 10*time.Minute, takerPrivate, *takerMessage, now)
	if err != nil {
		t.Fatal(err)
	}
	match, err := trade.NewMatch(signedOrder, acceptance, makerPrivate, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return signedOrder, acceptance, match
}

func TestJournalSurvivesRestartAndSerializesWorkers(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	signedOrder, acceptance, match := matchedFixture(t, now)
	path := filepath.Join(t.TempDir(), "swaps.db")
	journal, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	record, created, err := journal.Create(RoleMaker, signedOrder, acceptance, match, now.Add(time.Second))
	if err != nil || !created || record.Phase != PhaseMatched {
		t.Fatalf("create: record=%#v created=%v err=%v", record, created, err)
	}
	if _, created, err := journal.Create(RoleMaker, signedOrder, acceptance, match, now.Add(time.Second)); err != nil || created {
		t.Fatalf("idempotent create: created=%v err=%v", created, err)
	}

	results := make(chan error, 2)
	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := journal.Advance(record.ID, PhaseMatched, PhaseTermsProposed, now.Add(2*time.Second))
			results <- err
		}()
	}
	wait.Wait()
	close(results)
	succeeded, conflicted := 0, 0
	for err := range results {
		if err == nil {
			succeeded++
		} else if errors.Is(err, ErrPhaseChanged) {
			conflicted++
		} else {
			t.Fatal(err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("transitions succeeded=%d conflicted=%d", succeeded, conflicted)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	journal, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	record, err = journal.Swap(record.ID)
	if err != nil || record.Phase != PhaseTermsProposed {
		t.Fatalf("reopened record=%#v err=%v", record, err)
	}
}

func TestActionsAreDurableIdempotentAndReorgAware(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	signedOrder, acceptance, match := matchedFixture(t, now)
	journal, err := Open(filepath.Join(t.TempDir(), "swaps.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	record, _, err := journal.Create(RoleTaker, signedOrder, acceptance, match, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	action := Action{ID: record.ID + ":maker-fund", SwapID: record.ID, Kind: "maker-fund", Chain: "BTC", RawTransaction: "02000000", TransactionID: "abcd"}
	prepared, created, err := journal.PrepareAction(action, now.Add(2*time.Second))
	if err != nil || !created || prepared.Status != ActionPrepared {
		t.Fatalf("prepare: %#v created=%v err=%v", prepared, created, err)
	}
	if _, created, err := journal.PrepareAction(action, now.Add(3*time.Second)); err != nil || created {
		t.Fatalf("idempotent prepare: created=%v err=%v", created, err)
	}
	conflict := action
	conflict.RawTransaction = "deadbeef"
	if _, _, err := journal.PrepareAction(conflict, now.Add(3*time.Second)); !errors.Is(err, ErrActionConflict) {
		t.Fatalf("conflicting action error = %v", err)
	}
	if _, err := journal.MarkBroadcast(action.ID, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	confirmed, err := journal.MarkConfirmed(action.ID, 900000, now.Add(4*time.Second))
	if err != nil || confirmed.Status != ActionConfirmed || confirmed.BlockHeight != 900000 {
		t.Fatalf("confirm: %#v err=%v", confirmed, err)
	}
	reorged, err := journal.MarkReorged(action.ID, now.Add(5*time.Second))
	if err != nil || reorged.Status != ActionBroadcast || reorged.BlockHeight != 0 {
		t.Fatalf("reorg: %#v err=%v", reorged, err)
	}
}

func TestPhaseGraphAndMonotonicMailboxCursor(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	signedOrder, acceptance, match := matchedFixture(t, now)
	journal, err := Open(filepath.Join(t.TempDir(), "swaps.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	record, _, err := journal.Create(RoleMaker, signedOrder, acceptance, match, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Advance(record.ID, PhaseMatched, PhaseComplete, now); !errors.Is(err, ErrInvalidAdvance) {
		t.Fatalf("invalid phase jump error = %v", err)
	}
	if _, err := journal.SetMailboxCursor(record.ID, 9, now); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.SetMailboxCursor(record.ID, 8, now); err == nil {
		t.Fatal("mailbox cursor moved backwards")
	}
}

func TestNegotiationsAndRelayCursorSurviveRestart(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	signedOrder, acceptance, match := matchedFixture(t, now)
	path := filepath.Join(t.TempDir(), "swaps.db")
	journal, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, created, err := journal.SavePendingAcceptance(signedOrder, acceptance, now); err != nil || !created {
		t.Fatalf("save pending created=%v err=%v", created, err)
	}
	if _, created, err := journal.SavePendingAcceptance(signedOrder, acceptance, now); err != nil || created {
		t.Fatalf("idempotent pending created=%v err=%v", created, err)
	}
	if _, created, err := journal.SaveIncomingAcceptance(signedOrder, acceptance, now); err != nil || !created {
		t.Fatalf("save incoming created=%v err=%v", created, err)
	}
	if found, err := journal.PendingAcceptanceByMatch(match); err != nil || found.Acceptance.ID != acceptance.ID {
		t.Fatalf("pending by match=%#v err=%v", found, err)
	}
	if err := journal.SetRelayCursor(12); err != nil {
		t.Fatal(err)
	} else if err := journal.SetRelayCursor(11); err == nil {
		t.Fatal("relay cursor moved backwards")
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	journal, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	if cursor, err := journal.RelayCursor(); err != nil || cursor != 12 {
		t.Fatalf("cursor=%d err=%v", cursor, err)
	}
	if records, err := journal.PendingAcceptances(); err != nil || len(records) != 1 {
		t.Fatalf("pending=%#v err=%v", records, err)
	}
	if err := journal.RemovePendingAcceptance(acceptance.ID); err != nil {
		t.Fatal(err)
	}
	if records, err := journal.PendingAcceptances(); err != nil || len(records) != 0 {
		t.Fatalf("pending after remove=%#v err=%v", records, err)
	}
}

func TestNotificationReadsSurviveRestart(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	signedOrder, acceptance, match := matchedFixture(t, now)
	path := filepath.Join(t.TempDir(), "swaps.db")
	journal, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	record, _, err := journal.Create(RoleMaker, signedOrder, acceptance, match, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.SetNotificationRead(record.ID, "first-funded"); err != nil {
		t.Fatal(err)
	}
	if err := journal.SetNotificationRead(record.ID, "not valid"); err == nil {
		t.Fatal("invalid notification milestone was accepted")
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	journal, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	reads, err := journal.NotificationReads()
	if err != nil {
		t.Fatal(err)
	}
	if reads[record.ID] != "first-funded" {
		t.Fatalf("notification read = %q", reads[record.ID])
	}
}

func TestSwapTermsApprovalAndSecretAreDurable(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	signedOrder, acceptance, match := matchedFixture(t, now)
	journal, err := Open(filepath.Join(t.TempDir(), "swaps.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	record, _, err := journal.Create(RoleMaker, signedOrder, acceptance, match, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.SetHello(record.ID, true, `{"party":"maker"}`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.SetHello(record.ID, true, `{"party":"changed"}`, now); err == nil {
		t.Fatal("local hello was changed")
	}
	if _, err := journal.SetHello(record.ID, false, `{"party":"taker"}`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Advance(record.ID, PhaseMatched, PhaseTermsProposed, now); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Advance(record.ID, PhaseTermsProposed, PhaseTermsAgreed, now); err != nil {
		t.Fatal(err)
	}
	secret := sha256.Sum256([]byte("journal secret fixture"))
	secretHash := sha256.Sum256(secret[:])
	agreementHash := sha256.Sum256([]byte("agreement"))
	if _, err := journal.SetAgreementDetails(record.ID, hex.EncodeToString(secretHash[:]), hex.EncodeToString(agreementHash[:]), `{"version":1}`, PhaseTermsAgreed, now); err != nil {
		t.Fatal(err)
	}
	approved, err := journal.Approve(record.ID, now)
	if err != nil || !approved.Approved {
		t.Fatalf("approve=%#v err=%v", approved, err)
	}
	if _, err := journal.SetRevealedSecret(record.ID, hex.EncodeToString(secret[:]), now); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.SetRevealedSecret(record.ID, hex.EncodeToString(make([]byte, 32)), now); err == nil {
		t.Fatal("accepted a different revealed secret")
	}
}

func TestAsyncTakerFundingSubmissionIsDurableAndReorgAware(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	signedOrder, acceptance, match := matchedFixture(t, now)
	path := filepath.Join(t.TempDir(), "swaps.db")
	journal, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	record, _, err := journal.Create(RoleTaker, signedOrder, acceptance, match, now)
	if err != nil {
		t.Fatal(err)
	}
	// The fixture predates the asynchronous payload fields. Change only the
	// protocol discriminator and phase to exercise the journal marker itself.
	if _, err := journal.updateSwap(record.ID, func(current *Swap) error {
		current.Version = trade.AsyncProtocolVersion
		current.Phase = PhaseAsyncTakerFunding
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	marked, err := journal.SetTakerFundingSubmitted(record.ID, true, now.Add(time.Second))
	if err != nil || !marked.TakerFundingSubmitted {
		t.Fatalf("mark funding submitted: %#v, %v", marked, err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	journal, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	marked, err = journal.Swap(record.ID)
	if err != nil || !marked.TakerFundingSubmitted {
		t.Fatalf("submission marker did not survive restart: %#v, %v", marked, err)
	}
	if _, err := journal.Advance(record.ID, PhaseAsyncTakerFunding, PhaseAsyncTakerFunded, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	rewound, err := journal.RewindAfterReorg(record.ID, PhaseAsyncTakerFunded, PhaseAsyncTakerFunding, "funding disappeared", now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	} else if rewound.TakerFundingSubmitted {
		t.Fatal("reorg did not restore the taker funding reservation")
	}
}

func TestOutboundMessageEnvelopeIsAtomicAndImmutable(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	signedOrder, acceptance, match := matchedFixture(t, now)
	path := filepath.Join(t.TempDir(), "swaps.db")
	journal, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	record, _, err := journal.Create(RoleMaker, signedOrder, acceptance, match, now)
	if err != nil {
		t.Fatal(err)
	}
	_, senderPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	recipientPublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, senderMessagePrivate, err := box.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	recipientMessagePublic, _, err := box.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	message, err := trade.EncryptMessage(
		signedOrder.Order.Network, signedOrder.ID, record.ID, 1,
		senderPrivate, recipientPublic, *senderMessagePrivate,
		*recipientMessagePublic, []byte(`{"kind":"hello"}`), time.Hour, now,
	)
	if err != nil {
		t.Fatal(err)
	}
	saved, created, err := journal.SaveOutboundMessage(record.ID, "hello", message, now)
	if err != nil || !created || saved.ID != message.ID {
		t.Fatalf("save=%#v created=%v err=%v", saved, created, err)
	}
	saved, created, err = journal.SaveOutboundMessage(record.ID, "hello", message, now.Add(time.Second))
	if err != nil || created || saved.ID != message.ID {
		t.Fatalf("repeat=%#v created=%v err=%v", saved, created, err)
	}
	other, err := trade.EncryptMessage(
		signedOrder.Order.Network, signedOrder.ID, record.ID, 2,
		senderPrivate, recipientPublic, *senderMessagePrivate,
		*recipientMessagePublic, []byte(`{"kind":"changed"}`), time.Hour, now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := journal.SaveOutboundMessage(record.ID, "hello", other, now); err == nil {
		t.Fatal("changed an immutable outbound protocol step")
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	journal, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	reopened, err := journal.Swap(record.ID)
	if err != nil || reopened.SentMessages["hello"] != message.ID {
		t.Fatalf("reopened=%#v err=%v", reopened, err)
	}
	stored, err := journal.RelayMessage(message.ID)
	if err != nil || stored.ID != message.ID {
		t.Fatalf("stored=%#v err=%v", stored, err)
	}
}
