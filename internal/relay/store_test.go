package relay

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/petoshi/qday-swap/internal/order"
)

func testSigner(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return publicKey, privateKey
}

func testSignedOrder(t *testing.T, publicKey ed25519.PublicKey, privateKey ed25519.PrivateKey, now time.Time, index int) order.Signed {
	t.Helper()
	messageKey := [32]byte{byte(index + 1)}
	payload, err := order.NewPayload("mainnet", order.QDAYLegacyUnit, order.Amount{Asset: "QDAY", Atomic: fmt.Sprintf("%d000000000000000000000000", index+1)}, order.Amount{Asset: "BTC", Atomic: fmt.Sprintf("%d0000", index+1)}, time.Hour, publicKey, messageKey, now.Add(time.Duration(index)*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	signed, err := order.Sign(payload, privateKey, now.Add(time.Duration(index)*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "orders.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestStorePublishListAndPersistence(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	publicKey, privateKey := testSigner(t)
	path := filepath.Join(t.TempDir(), "orders.db")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	published := make([]order.Signed, 23)
	for i := range 23 {
		signed := testSignedOrder(t, publicKey, privateKey, now, i)
		published[i] = signed
		if _, created, err := store.Publish(signed, now.Add(time.Duration(i)*time.Second)); err != nil || !created {
			t.Fatalf("publish %d: created=%v err=%v", i, created, err)
		}
		if _, created, err := store.Publish(signed, now.Add(time.Duration(i)*time.Second)); err != nil || created {
			t.Fatalf("duplicate %d: created=%v err=%v", i, created, err)
		}
	}
	page, err := store.List(Query{Status: StatusOpen, Page: 2, Limit: 20}, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	} else if page.Total != 23 || page.TotalPages != 2 || len(page.Items) != 3 {
		t.Fatalf("unexpected page %#v", page)
	}
	if first, err := store.List(Query{Page: 1, Limit: 20}, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	} else if first.Items[0].Signed.ID != published[22].ID {
		t.Fatal("orders are not newest first")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if stats, err := store.Stats(now.Add(time.Minute)); err != nil || stats.Open != 23 || stats.Total != 23 {
		t.Fatalf("persistent stats: %#v err=%v", stats, err)
	}
}

func TestStoreCancellationOwnershipAndExpiry(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	publicKey, privateKey := testSigner(t)
	store := openTestStore(t)
	signed := testSignedOrder(t, publicKey, privateKey, now, 0)
	if _, _, err := store.Publish(signed, now); err != nil {
		t.Fatal(err)
	}
	_, wrongPrivateKey := testSigner(t)
	_, err := order.NewCancellation(signed, wrongPrivateKey, now.Add(time.Minute))
	if err == nil {
		t.Fatal("created cancellation with another maker key")
	}
	cancellation, err := order.NewCancellation(signed, privateKey, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	record, changed, err := store.Cancel(cancellation, now.Add(time.Minute))
	if err != nil || !changed || record.Status != StatusCancelled {
		t.Fatalf("cancel: %#v changed=%v err=%v", record, changed, err)
	}
	if _, changed, err := store.Cancel(cancellation, now.Add(2*time.Minute)); err != nil || changed {
		t.Fatalf("idempotent cancel: changed=%v err=%v", changed, err)
	}

	expiring := testSignedOrder(t, publicKey, privateKey, now, 1)
	if _, _, err := store.Publish(expiring, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	record, err = store.Order(expiring.ID, now.Add(2*time.Hour))
	if err != nil || record.Status != StatusExpired {
		t.Fatalf("expired order: %#v err=%v", record, err)
	}
	expiredCancellation, err := order.NewCancellation(expiring, privateKey, now.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Cancel(expiredCancellation, now.Add(2*time.Hour)); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("expired cancellation error = %v", err)
	}
}
