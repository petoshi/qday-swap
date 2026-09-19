package relayclient

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/petoshi/qday-swap/internal/order"
	"github.com/petoshi/qday-swap/internal/relay"
)

func TestClientPublishesListsAndCancelsVerifiedOrder(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	store, err := relay.OpenStore(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := relay.NewServer(store, relay.Config{
		Network: "mainnet", PublicURL: "https://dex.pqday.com", Version: "test",
		Now: func() time.Time { return now }, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	client, err := New(httpServer.URL, httpServer.Client())
	if err != nil {
		t.Fatal(err)
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := order.NewPayload("mainnet", order.QDAYLegacyUnit,
		order.Amount{Asset: "QDAY", Atomic: order.QDAYLegacyUnit},
		order.Amount{Asset: "BTC", Atomic: "10000"}, time.Hour, public, [32]byte{1}, now)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := order.Sign(payload, private, now)
	if err != nil {
		t.Fatal(err)
	}
	record, err := client.Publish(context.Background(), signed)
	if err != nil || record.Status != relay.StatusOpen {
		t.Fatalf("publish record=%#v err=%v", record, err)
	}
	page, err := client.Orders(context.Background(), relay.StatusOpen, "QDAY", 1, 20)
	if err != nil || page.Total != 1 || page.Items[0].Signed.ID != signed.ID {
		t.Fatalf("list page=%#v err=%v", page, err)
	}
	cancellation, err := order.NewCancellation(signed, private, now)
	if err != nil {
		t.Fatal(err)
	}
	record, err = client.Cancel(context.Background(), signed.ID, cancellation)
	if err != nil || record.Status != relay.StatusCancelled {
		t.Fatalf("cancel record=%#v err=%v", record, err)
	}
}

func TestClientRejectsInsecureRemoteURL(t *testing.T) {
	if _, err := New("http://dex.pqday.com", nil); err == nil {
		t.Fatal("accepted plaintext remote relay URL")
	}
}
