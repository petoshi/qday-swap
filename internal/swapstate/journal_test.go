package swapstate

import (
	"crypto/ed25519"
	"crypto/rand"
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
