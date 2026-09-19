package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/petoshi/qday-swap/internal/walletd"
	"github.com/petoshi/qday-swap/internal/walletroot"
)

const fakeToken = "0001020304050607080900010203040506070809000102030405060708090001"

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

func TestSetupLockUnlockAndReceiveAddress(t *testing.T) {
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
	service, err := New(ctx, Config{DataDir: dataDir, WalletdBinary: "unused", Network: "mainnet", RelayURL: "https://dex.pqday.com"})
	if err != nil {
		t.Fatal(err)
	}
	service.factory = func(path string) (qdayProcess, error) {
		return &fakeWalletd{dataDir: path, server: server}, nil
	}
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
	phrase, err := service.RecoveryPhrase()
	if err != nil || phrase != result.Phrase {
		t.Fatalf("recovery phrase=%q err=%v", phrase, err)
	}
	address, err := service.QDAYReceiveAddress(ctx)
	if err != nil || address.Address != "qday1ptest" {
		t.Fatalf("address=%#v err=%v", address, err)
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
