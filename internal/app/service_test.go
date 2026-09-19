package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/petoshi/qday-swap/internal/bitcoinwallet"
	"github.com/petoshi/qday-swap/internal/order"
	"github.com/petoshi/qday-swap/internal/relay"
	"github.com/petoshi/qday-swap/internal/swapstate"
	"github.com/petoshi/qday-swap/internal/walletd"
	"github.com/petoshi/qday-swap/internal/walletroot"
)

const fakeToken = "0001020304050607080900010203040506070809000102030405060708090001"

func testRelayURL(t *testing.T) string {
	t.Helper()
	store, err := relay.OpenStore(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	server, err := relay.NewServer(store, relay.Config{
		Network: "mainnet", PublicURL: "https://dex.pqday.com", Version: "test",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(func() { httpServer.Close(); _ = store.Close() })
	return httpServer.URL
}

type fakeWalletd struct {
	dataDir string
	server  *httptest.Server
}

func (f *fakeWalletd) Initialize(_ context.Context, _ walletroot.Root, password string) error {
	if password != "correct horse battery staple" {
		return os.ErrPermission
	}
	if err := os.MkdirAll(f.dataDir, 0700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(f.dataDir, "master.key"), []byte("fake"), 0600)
}

func (f *fakeWalletd) Start(context.Context) (*walletd.Client, error) {
	return walletd.NewClient(f.server.URL, fakeToken)
}

func (f *fakeWalletd) Stop() error { return nil }

type fakeBitcoin struct{ unlocked bool }

func (f *fakeBitcoin) Initialize(_ context.Context, _ walletroot.Root, password string, _ time.Time) error {
	if password != "correct horse battery staple" {
		return os.ErrPermission
	}
	return nil
}

func (f *fakeBitcoin) Start(context.Context) (bitcoinClient, error) { return f, nil }
func (f *fakeBitcoin) Stop() error                                  { return nil }
func (f *fakeBitcoin) Status() (bitcoinwallet.Status, error) {
	return bitcoinwallet.Status{Network: "mainnet", HeaderHeight: 900_000, WalletHeight: 900_000, Peers: 8, HeadersSynced: true, WalletSynced: true, Unlocked: f.unlocked}, nil
}
func (f *fakeBitcoin) Balance() (bitcoinwallet.Balance, error) {
	return bitcoinwallet.Balance{
		Confirmed: bitcoinwallet.Amount{Satoshis: "125000000", BTC: "1.25"},
		Total:     bitcoinwallet.Amount{Satoshis: "125000000", BTC: "1.25"},
	}, nil
}
func (f *fakeBitcoin) Unlock(password string) error {
	if password != "correct horse battery staple" {
		return os.ErrPermission
	}
	f.unlocked = true
	return nil
}
func (f *fakeBitcoin) Lock() { f.unlocked = false }
func (f *fakeBitcoin) ReceiveAddress() (string, error) {
	if !f.unlocked {
		return "", os.ErrPermission
	}
	return "bc1qtest", nil
}

func TestSetupLockUnlockAndReceiveAddress(t *testing.T) {
	relayURL := testRelayURL(t)
	var mu sync.Mutex
	unlocked := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+fakeToken {
			http.Error(w, `{"error":"authentication required"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/v1/wallet/unlock":
			unlocked = true
			_, _ = w.Write([]byte(`{"unlocked":true}`))
		case "/v1/wallet/lock":
			unlocked = false
			_, _ = w.Write([]byte(`{"unlocked":false}`))
		case "/v1/status":
			_ = json.NewEncoder(w).Encode(walletd.Status{Network: "qday-mainnet", Height: 123, ScanHeight: 123, Synced: true, NetworkSynced: true, Connections: 3, Unlocked: unlocked, UnitAtomic: "1000000000000000000000000"})
		case "/v1/balance":
			_ = json.NewEncoder(w).Encode(walletd.Balance{Height: 123, Synced: true, UnitAtomic: "1000000000000000000000000", Spendable: walletd.Amount{Atomic: "5000000000000000000000000", QDAY: "5"}})
		case "/v1/addresses":
			_ = json.NewEncoder(w).Encode(walletd.Address{Index: 0, Address: "qday1ptest", Reference: "qday-swap-main-receive-v1", Kind: "deposit"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ctx := context.Background()
	dataDir := filepath.Join(t.TempDir(), "qday-swap")
	service, err := New(ctx, Config{DataDir: dataDir, WalletdBinary: "unused", Network: "mainnet", RelayURL: relayURL})
	if err != nil {
		t.Fatal(err)
	}
	service.factory = func(path string) (qdayProcess, error) {
		return &fakeWalletd{dataDir: path, server: server}, nil
	}
	bitcoin := &fakeBitcoin{}
	service.bitcoinFactory = func(string) (bitcoinProcess, error) { return bitcoin, nil }
	result, err := service.Setup(ctx, "correct horse battery staple", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(strings.Fields(result.Phrase)) != 24 || !result.State.Configured || !result.State.Unlocked {
		t.Fatalf("setup result = %#v", result)
	}
	if result.State.QDAY == nil || result.State.QDAY.Height != 123 || result.State.Balance == nil || result.State.Balance.Spendable.QDAY != "5" {
		t.Fatalf("setup state does not include QDAY status: %#v", result.State)
	}
	if result.State.Bitcoin == nil || result.State.Bitcoin.HeaderHeight != 900_000 || result.State.BitcoinBalance == nil || result.State.BitcoinBalance.Confirmed.BTC != "1.25" {
		t.Fatalf("setup state does not include Bitcoin status: %#v", result.State)
	}
	phrase, err := service.RecoveryPhrase()
	if err != nil || phrase != result.Phrase {
		t.Fatalf("recovery phrase=%q err=%v", phrase, err)
	}
	address, err := service.QDAYReceiveAddress(ctx)
	if err != nil || address.Address != "qday1ptest" {
		t.Fatalf("address=%#v err=%v", address, err)
	}
	bitcoinAddress, err := service.BitcoinReceiveAddress()
	if err != nil || bitcoinAddress != "bc1qtest" {
		t.Fatalf("Bitcoin address=%q err=%v", bitcoinAddress, err)
	}
	if err := service.Lock(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RecoveryPhrase(); err == nil {
		t.Fatal("locked service returned recovery phrase")
	}
	if err := service.Unlock(ctx, "wrong password here"); err == nil {
		t.Fatal("wrong password unlocked service")
	}
	if err := service.Unlock(ctx, "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestTwoApplicationsPublishAcceptAndMatchOffer(t *testing.T) {
	relayURL := testRelayURL(t)
	qdayServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+fakeToken {
			http.Error(w, `{"error":"authentication required"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/wallet/unlock":
			_, _ = w.Write([]byte(`{"unlocked":true}`))
		case "/v1/wallet/lock":
			_, _ = w.Write([]byte(`{"unlocked":false}`))
		case "/v1/status":
			_ = json.NewEncoder(w).Encode(walletd.Status{Network: "qday-mainnet", Height: 12_000, ScanHeight: 12_000, Synced: true, NetworkSynced: true, Connections: 4, Unlocked: true, UnitAtomic: order.QDAYLegacyUnit})
		case "/v1/balance":
			_ = json.NewEncoder(w).Encode(walletd.Balance{Height: 12_000, Synced: true, UnitAtomic: order.QDAYLegacyUnit, Spendable: walletd.Amount{Atomic: "5000000000000000000000000", QDAY: "5"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer qdayServer.Close()

	newApplication := func(name string) *Service {
		service, err := New(context.Background(), Config{
			DataDir: filepath.Join(t.TempDir(), name), WalletdBinary: "unused",
			Network: "mainnet", RelayURL: relayURL,
		})
		if err != nil {
			t.Fatal(err)
		}
		service.factory = func(path string) (qdayProcess, error) {
			return &fakeWalletd{dataDir: path, server: qdayServer}, nil
		}
		bitcoin := &fakeBitcoin{}
		service.bitcoinFactory = func(string) (bitcoinProcess, error) { return bitcoin, nil }
		if _, err := service.Setup(context.Background(), "correct horse battery staple", ""); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = service.Close() })
		return service
	}

	maker := newApplication("maker")
	taker := newApplication("taker")
	offer, err := maker.CreateOffer(context.Background(), CreateOfferRequest{
		GiveAsset: "QDAY", GiveAmount: "1.25", ReceiveAmount: "0.0001", LifetimeMinutes: 60,
	})
	if err != nil {
		t.Fatal(err)
	} else if offer.Status != relay.StatusOpen {
		t.Fatalf("offer status = %q", offer.Status)
	}
	pending, err := taker.AcceptOffer(context.Background(), offer.Signed.ID)
	if err != nil {
		t.Fatal(err)
	} else if pending.Order.ID != offer.Signed.ID {
		t.Fatalf("pending order = %q", pending.Order.ID)
	}
	maker.lastRelaySync = time.Time{}
	state := maker.State(context.Background())
	if state.RelayError != "" || state.IncomingTrades != 1 {
		t.Fatalf("maker state = %#v", state)
	}
	negotiations, err := maker.Negotiations()
	if err != nil || len(negotiations.Incoming) != 1 {
		t.Fatalf("maker negotiations=%#v err=%v", negotiations, err)
	}
	matched, err := maker.MatchAcceptance(context.Background(), negotiations.Incoming[0].Acceptance.ID)
	if err != nil {
		t.Fatal(err)
	} else if matched.Phase != swapstate.PhaseMatched || matched.Role != swapstate.RoleMaker {
		t.Fatalf("maker swap = %#v", matched)
	}
	taker.lastRelaySync = time.Time{}
	state = taker.State(context.Background())
	if state.RelayError != "" || state.PendingTrades != 0 || state.ActiveSwaps != 1 {
		t.Fatalf("taker state = %#v", state)
	}
	negotiations, err = taker.Negotiations()
	if err != nil || len(negotiations.Swaps) != 1 || negotiations.Swaps[0].Role != swapstate.RoleTaker {
		t.Fatalf("taker negotiations=%#v err=%v", negotiations, err)
	}
}
