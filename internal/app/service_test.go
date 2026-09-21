package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/petoshi/qday-swap/internal/bitcoin"
	"github.com/petoshi/qday-swap/internal/bitcoinwallet"
	"github.com/petoshi/qday-swap/internal/order"
	"github.com/petoshi/qday-swap/internal/relay"
	"github.com/petoshi/qday-swap/internal/swapstate"
	"github.com/petoshi/qday-swap/internal/trade"
	"github.com/petoshi/qday-swap/internal/walletd"
	"github.com/petoshi/qday-swap/internal/walletroot"
)

const fakeToken = "0001020304050607080900010203040506070809000102030405060708090001"

func testRelayURL(t *testing.T) string {
	t.Helper()
	priceProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"amount":"80000.00","base":"BTC","currency":"USD"}}`)
	}))
	t.Cleanup(priceProvider.Close)
	store, err := relay.OpenStore(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	server, err := relay.NewServer(store, relay.Config{
		Network: "mainnet", PublicURL: "https://dex.pqday.com", Version: "test",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), PriceURL: priceProvider.URL,
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

type failOnceMatchRelay struct {
	relayAPI
	failed bool
}

func (r *failOnceMatchRelay) Match(ctx context.Context, orderID string, match trade.SignedMatch) (relay.Record, error) {
	if !r.failed {
		r.failed = true
		return relay.Record{}, errors.New("simulated relay timeout")
	}
	return r.relayAPI.Match(ctx, orderID, match)
}

type failOnceAcceptRelay struct {
	relayAPI
	failed bool
}

func (r *failOnceAcceptRelay) Accept(ctx context.Context, orderID string, acceptance trade.SignedAcceptance) (relay.AcceptanceRecord, error) {
	if !r.failed {
		r.failed = true
		return relay.AcceptanceRecord{}, errors.New("simulated relay timeout")
	}
	return r.relayAPI.Accept(ctx, orderID, acceptance)
}

type failOnceCancelAcceptanceRelay struct {
	relayAPI
	failed bool
}

func (r *failOnceCancelAcceptanceRelay) CancelAcceptance(ctx context.Context, orderID, acceptanceID string, cancellation trade.SignedAcceptanceCancellation) (relay.AcceptanceCancellationRecord, error) {
	if !r.failed {
		r.failed = true
		return relay.AcceptanceCancellationRecord{}, errors.New("simulated cancellation relay timeout")
	}
	return r.relayAPI.CancelAcceptance(ctx, orderID, acceptanceID, cancellation)
}

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
func (f *fakeBitcoin) QuotePayment(destination string, satoshis int64, maximum bool) (bitcoinwallet.PaymentQuote, error) {
	if !f.unlocked {
		return bitcoinwallet.PaymentQuote{}, os.ErrPermission
	}
	if maximum {
		satoshis = 124_999_000
	}
	return bitcoinwallet.PaymentQuote{
		Destination:        destination,
		Amount:             bitcoinwallet.Amount{Satoshis: fmt.Sprint(satoshis), BTC: atomicDecimal(fmt.Sprint(satoshis), "100000000")},
		Fee:                bitcoinwallet.Amount{Satoshis: "1000", BTC: "0.00001"},
		Total:              bitcoinwallet.Amount{Satoshis: fmt.Sprint(satoshis + 1000), BTC: atomicDecimal(fmt.Sprint(satoshis+1000), "100000000")},
		FeeRateSatPerVByte: 10,
	}, nil
}
func (f *fakeBitcoin) SendPayment(destination string, satoshis, expectedFee int64) (bitcoinwallet.Payment, error) {
	quote, err := f.QuotePayment(destination, satoshis, false)
	if err != nil {
		return bitcoinwallet.Payment{}, err
	} else if expectedFee != 1000 {
		return bitcoinwallet.Payment{}, errors.New("fee changed")
	}
	return bitcoinwallet.Payment{PaymentQuote: quote, TransactionID: "bitcoin-payment"}, nil
}

func (f *fakeBitcoin) WatchContract(context.Context, bitcoin.Contract, uint32) error { return nil }

func (f *fakeBitcoin) PrepareFunding(contract bitcoin.Contract) (bitcoin.Funding, error) {
	pkScript, err := contract.PkScript()
	if err != nil {
		return bitcoin.Funding{}, err
	}
	previous := chainhash.Hash{1}
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(wire.NewTxIn(wire.NewOutPoint(&previous, 1), nil, nil))
	tx.AddTxOut(wire.NewTxOut(contract.Amount, pkScript))
	raw, err := bitcoin.EncodeTransaction(tx)
	if err != nil {
		return bitcoin.Funding{}, err
	}
	return contract.FundingFromRaw(raw)
}

func (f *fakeBitcoin) Broadcast(raw string) (bitcoinwallet.Transaction, error) {
	tx, err := bitcoin.DecodeTransaction(raw)
	if err != nil {
		return bitcoinwallet.Transaction{}, err
	}
	return bitcoinwallet.Transaction{ID: tx.TxID(), Raw: raw, Height: 900_000, Confirmations: 1}, nil
}

func (f *fakeBitcoin) Transaction(string) (bitcoinwallet.Transaction, error) {
	return bitcoinwallet.Transaction{}, bitcoinwallet.ErrTransactionNotFound
}

func (f *fakeBitcoin) FindSpend(bitcoin.Funding) (bitcoinwallet.Transaction, error) {
	return bitcoinwallet.Transaction{}, bitcoinwallet.ErrTransactionNotFound
}

func (f *fakeBitcoin) DestinationScript() ([]byte, error) { return []byte{0x51}, nil }
func (f *fakeBitcoin) ReserveFunding(string) error        { return nil }
func (f *fakeBitcoin) ReleaseFunding(string) error        { return nil }

func TestSetupLockUnlockAndReceiveAddress(t *testing.T) {
	relayURL := testRelayURL(t)
	qdayDestination := "qday1" + strings.Repeat("q", 59)
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
		case "/v1/withdrawals":
			var request walletd.WithdrawalRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			_ = json.NewEncoder(w).Encode(walletd.Withdrawal{
				RequestID: request.RequestID, TransactionID: "qday-payment", Destination: request.Destination,
				Amount: walletd.Amount{Atomic: request.AmountAtomic}, Fee: walletd.Amount{Atomic: request.FeeAtomic}, Status: "broadcast",
			})
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
	phrase, err := service.RecoveryPhrase("correct horse battery staple")
	if err != nil || phrase != result.Phrase {
		t.Fatalf("recovery phrase=%q err=%v", phrase, err)
	}
	if _, err := service.RecoveryPhrase("wrong wallet password"); err == nil {
		t.Fatal("wrong password exported the recovery phrase")
	}
	address, err := service.QDAYReceiveAddress(ctx)
	if err != nil || address.Address != "qday1ptest" {
		t.Fatalf("address=%#v err=%v", address, err)
	}
	bitcoinAddress, err := service.BitcoinReceiveAddress()
	if err != nil || bitcoinAddress != "bc1qtest" {
		t.Fatalf("Bitcoin address=%q err=%v", bitcoinAddress, err)
	}
	qdayQuote, err := service.QuoteWithdrawal(ctx, WithdrawalQuoteRequest{Asset: "QDAY", Destination: qdayDestination, Amount: "1.25"})
	if err != nil {
		t.Fatal(err)
	} else if qdayQuote.Amount != "1.25" || qdayQuote.Fee != "0.001" || qdayQuote.Total != "1.251" {
		t.Fatalf("QDAY withdrawal quote = %#v", qdayQuote)
	}
	qdayPayment, err := service.SendWithdrawal(ctx, SendWithdrawalRequest{
		RequestID: qdayQuote.RequestID, Asset: qdayQuote.Asset, Destination: qdayQuote.Destination,
		AmountAtomic: qdayQuote.AmountAtomic, FeeAtomic: qdayQuote.FeeAtomic, UnitAtomic: qdayQuote.UnitAtomic,
	})
	if err != nil {
		t.Fatal(err)
	} else if qdayPayment.TransactionID != "qday-payment" {
		t.Fatalf("QDAY withdrawal = %#v", qdayPayment)
	}
	qdayMaximum, err := service.QuoteWithdrawal(ctx, WithdrawalQuoteRequest{Asset: "QDAY", Destination: qdayDestination, Maximum: true})
	if err != nil {
		t.Fatal(err)
	} else if qdayMaximum.Amount != "4.999" || qdayMaximum.Total != "5" {
		t.Fatalf("QDAY MAX quote = %#v", qdayMaximum)
	}
	if _, err := service.QuoteWithdrawal(ctx, WithdrawalQuoteRequest{Asset: "QDAY", Destination: qdayDestination, Amount: "5"}); err == nil {
		t.Fatal("QDAY quote allowed amount without room for the network fee")
	}
	bitcoinQuote, err := service.QuoteWithdrawal(ctx, WithdrawalQuoteRequest{Asset: "BTC", Destination: "bc1qdestination", Maximum: true})
	if err != nil {
		t.Fatal(err)
	} else if bitcoinQuote.AmountAtomic != "124999000" || bitcoinQuote.FeeAtomic != "1000" || bitcoinQuote.TotalAtomic != "125000000" {
		t.Fatalf("Bitcoin MAX quote = %#v", bitcoinQuote)
	}
	bitcoinPayment, err := service.SendWithdrawal(ctx, SendWithdrawalRequest{
		RequestID: bitcoinQuote.RequestID, Asset: bitcoinQuote.Asset, Destination: bitcoinQuote.Destination,
		AmountAtomic: bitcoinQuote.AmountAtomic, FeeAtomic: bitcoinQuote.FeeAtomic, UnitAtomic: bitcoinQuote.UnitAtomic,
	})
	if err != nil {
		t.Fatal(err)
	} else if bitcoinPayment.TransactionID != "bitcoin-payment" {
		t.Fatalf("Bitcoin withdrawal = %#v", bitcoinPayment)
	}
	retriedBitcoinPayment, err := service.SendWithdrawal(ctx, SendWithdrawalRequest{
		RequestID: bitcoinQuote.RequestID, Asset: bitcoinQuote.Asset, Destination: bitcoinQuote.Destination,
		AmountAtomic: bitcoinQuote.AmountAtomic, FeeAtomic: bitcoinQuote.FeeAtomic, UnitAtomic: bitcoinQuote.UnitAtomic,
	})
	if err != nil || retriedBitcoinPayment != bitcoinPayment {
		t.Fatalf("idempotent Bitcoin retry = %#v, %v", retriedBitcoinPayment, err)
	}
	if _, err := service.SendWithdrawal(ctx, SendWithdrawalRequest{
		RequestID: bitcoinQuote.RequestID, Asset: bitcoinQuote.Asset, Destination: "bc1qchanged",
		AmountAtomic: bitcoinQuote.AmountAtomic, FeeAtomic: bitcoinQuote.FeeAtomic, UnitAtomic: bitcoinQuote.UnitAtomic,
	}); err == nil {
		t.Fatal("Bitcoin request ID was reused for another destination")
	}
	if err := service.Lock(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RecoveryPhrase("correct horse battery staple"); err == nil {
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

func TestImportRecoveryReplacesWalletWithoutKeepingBackup(t *testing.T) {
	ctx := context.Background()
	relayURL := testRelayURL(t)
	var addressRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/wallet/unlock":
			_, _ = w.Write([]byte(`{"unlocked":true}`))
		case "/v1/wallet/lock":
			_, _ = w.Write([]byte(`{"unlocked":false}`))
		case "/v1/status":
			_ = json.NewEncoder(w).Encode(walletd.Status{Network: "qday-mainnet", Height: 123, ScanHeight: 123, Synced: true, NetworkSynced: true, Unlocked: true, UnitAtomic: order.QDAYLegacyUnit})
		case "/v1/balance":
			balance := walletd.Balance{Synced: true, UnitAtomic: order.QDAYLegacyUnit}
			if addressRequests.Load() != 0 {
				balance.Spendable = walletd.Amount{Atomic: order.QDAYLegacyUnit, QDAY: "1"}
			}
			_ = json.NewEncoder(w).Encode(balance)
		case "/v1/addresses":
			addressRequests.Add(1)
			_ = json.NewEncoder(w).Encode(walletd.Address{Index: 0, Address: "qday1ptest", Reference: mainQDAYReceiveReference, Kind: "deposit"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	dataDir := filepath.Join(t.TempDir(), "qday-swap")
	service, err := New(ctx, Config{DataDir: dataDir, WalletdBinary: "unused", Network: "mainnet", RelayURL: relayURL})
	if err != nil {
		t.Fatal(err)
	}
	service.factory = func(path string) (qdayProcess, error) { return &fakeWalletd{dataDir: path, server: server}, nil }
	bitcoin := &fakeBitcoin{}
	service.bitcoinFactory = func(string) (bitcoinProcess, error) { return bitcoin, nil }
	if _, err := service.Setup(ctx, "correct horse battery staple", ""); err != nil {
		t.Fatal(err)
	} else if got := addressRequests.Load(); got != 1 {
		t.Fatalf("setup registered main QDAY address %d times, want 1", got)
	}
	addressRequests.Store(0)
	replacement, err := walletroot.New()
	if err != nil {
		t.Fatal(err)
	}
	replacementPhrase, err := replacement.Phrase()
	clear(replacement[:])
	if err != nil {
		t.Fatal(err)
	}
	state, err := service.ImportRecovery(ctx, "correct horse battery staple", replacementPhrase)
	if err != nil {
		t.Fatal(err)
	} else if !state.Configured || !state.Unlocked {
		t.Fatalf("import state = %#v", state)
	} else if got := addressRequests.Load(); got != 1 {
		t.Fatalf("import registered main QDAY address %d times, want 1", got)
	} else if state.Balance == nil || state.Balance.Spendable.QDAY != "1" {
		t.Fatalf("import state did not refresh the registered QDAY balance: %#v", state)
	}
	exported, err := service.RecoveryPhrase("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	} else if exported != replacementPhrase {
		t.Fatal("import did not replace the recovery phrase")
	}
	backups, err := filepath.Glob(filepath.Join(filepath.Dir(dataDir), ".qday-swap-replaced-*"))
	if err != nil {
		t.Fatal(err)
	} else if len(backups) != 0 {
		t.Fatalf("import retained wallet backups: %v", backups)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestTwoApplicationsPublishAcceptAndMatchOffer(t *testing.T) {
	relayURL := testRelayURL(t)
	registered := make(map[string]walletd.RegisterSwapRequest)
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
		case "/v1/addresses":
			_ = json.NewEncoder(w).Encode(walletd.Address{Index: 0, Address: "qday1ptest", Reference: mainQDAYReceiveReference, Kind: "deposit"})
		case "/v1/swap-keys":
			var request struct {
				SwapID string `json:"swapID"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			classical := sha256.Sum256([]byte("classical:" + request.SwapID))
			reserve := sha256.Sum256([]byte("reserve:" + request.SwapID))
			_ = json.NewEncoder(w).Encode(walletd.SwapKeyView{SwapID: request.SwapID, Keys: walletd.SwapKeys{
				Classical: fmt.Sprintf("%x", classical), Reserve: fmt.Sprintf("%x", reserve), Address: "qday1ptest" + request.SwapID[:8],
			}})
		case "/v1/swaps":
			var request walletd.RegisterSwapRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			registered[request.SwapID] = request
			_ = json.NewEncoder(w).Encode(walletd.Swap{
				SwapID: request.SwapID, Role: request.Role, SecretHash: request.SecretHash,
				RefundHeight: request.RefundHeight, Height: 12_000,
			})
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
	buyQuote, err := maker.QuoteOffer(context.Background(), QuoteOfferRequest{
		Side: "buy", Quantity: "10", Price: "1", PriceCurrency: "USD",
	})
	if err != nil {
		t.Fatal(err)
	} else if buyQuote.GiveAsset != "BTC" || buyQuote.GiveAmount != "0.000125" || buyQuote.ReceiveAmount != "10" || buyQuote.USDTotal != "10.00" || buyQuote.BTCPerQDAY != "0.0000125" {
		t.Fatalf("buy quote = %#v", buyQuote)
	}
	sellQuote, err := maker.QuoteOffer(context.Background(), QuoteOfferRequest{
		Side: "sell", Quantity: "2.5", Price: "0.00004", PriceCurrency: "BTC",
	})
	if err != nil {
		t.Fatal(err)
	} else if sellQuote.GiveAsset != "QDAY" || sellQuote.GiveAmount != "2.5" || sellQuote.ReceiveAmount != "0.0001" || sellQuote.USDTotal != "8.00" {
		t.Fatalf("sell quote = %#v", sellQuote)
	}
	if _, err := maker.QuoteOffer(context.Background(), QuoteOfferRequest{
		Side: "buy", Quantity: "0.00000001", Price: "0.00000001", PriceCurrency: "BTC",
	}); err == nil || !strings.Contains(err.Error(), "below one satoshi") {
		t.Fatalf("sub-satoshi quote error = %v", err)
	}
	if _, err := maker.QuoteOffer(context.Background(), QuoteOfferRequest{
		Side: "buy", Quantity: "1", Price: "0.00001", PriceCurrency: "BTC",
	}); err == nil || !strings.Contains(err.Error(), "at least 10000 satoshis") {
		t.Fatalf("small Bitcoin quote error = %v", err)
	}
	expensive, err := maker.CreateOffer(context.Background(), CreateOfferRequest{
		GiveAsset: "QDAY", GiveAmount: "0.1", ReceiveAmount: "2",
	})
	if err != nil {
		t.Fatal(err)
	} else if expensive.Signed.Order.ExpiresAt-expensive.Signed.Order.CreatedAt != int64((24*time.Hour)/time.Second) {
		t.Fatalf("default offer lifetime = %d seconds", expensive.Signed.Order.ExpiresAt-expensive.Signed.Order.CreatedAt)
	}
	if _, err := taker.AcceptOffer(context.Background(), expensive.Signed.ID); err == nil || !strings.Contains(err.Error(), "insufficient available BTC") {
		t.Fatalf("underfunded taker error = %v", err)
	}
	cancelled, err := maker.CancelOffer(context.Background(), expensive.Signed.ID)
	if err != nil {
		t.Fatal(err)
	} else if cancelled.Status != relay.StatusCancelled {
		t.Fatalf("cancelled offer status = %q", cancelled.Status)
	}
	state := maker.State(context.Background())
	if state.Funds == nil || state.Funds.OpenOrders != 0 || state.Funds.QDAY.Reserved != "0" || state.Funds.QDAY.Available != "5" {
		t.Fatalf("funds after cancellation = %#v", state.Funds)
	}
	offer, err := maker.CreateOffer(context.Background(), CreateOfferRequest{
		GiveAsset: "QDAY", GiveAmount: "1.25", ReceiveAmount: "0.0001", LifetimeMinutes: 60,
	})
	if err != nil {
		t.Fatal(err)
	} else if offer.Status != relay.StatusOpen {
		t.Fatalf("offer status = %q", offer.Status)
	}
	state = maker.State(context.Background())
	if state.Funds == nil || state.Funds.OpenOrders != 1 || state.Funds.QDAY.Total != "5" || state.Funds.QDAY.Reserved != "1.25" || state.Funds.QDAY.Available != "3.75" {
		t.Fatalf("funds with open sell order = %#v", state.Funds)
	}
	if _, err := maker.QuoteOffer(context.Background(), QuoteOfferRequest{
		Side: "sell", Quantity: "4", Price: "0.0001", PriceCurrency: "BTC",
	}); err == nil || !strings.Contains(err.Error(), "insufficient available QDAY") {
		t.Fatalf("reserved QDAY was offered twice: %v", err)
	}
	if _, err := maker.QuoteWithdrawal(context.Background(), WithdrawalQuoteRequest{
		Asset: "QDAY", Destination: "qday1" + strings.Repeat("q", 59), Amount: "4",
	}); err == nil || !strings.Contains(err.Error(), "open order reservations") {
		t.Fatalf("reserved QDAY was withdrawable: %v", err)
	}
	flakyAcceptRelay := &failOnceAcceptRelay{relayAPI: taker.relay}
	taker.relay = flakyAcceptRelay
	firstPending, err := taker.AcceptOffer(context.Background(), offer.Signed.ID)
	if err != nil {
		t.Fatal(err)
	} else if firstPending.RelaySubmitted || !strings.Contains(firstPending.RelayError, "simulated relay timeout") || firstPending.RelayRetryAt == 0 {
		t.Fatalf("acceptance retry was not recorded: %#v", firstPending)
	}
	taker.lastRelaySync = time.Time{}
	if err := taker.syncRelay(context.Background()); err != nil {
		t.Fatalf("automatic acceptance retry: %v", err)
	}
	negotiations, err := taker.Negotiations()
	if err != nil || len(negotiations.Pending) != 1 {
		t.Fatalf("pending negotiations after automatic retry=%#v err=%v", negotiations.Pending, err)
	}
	pending := negotiations.Pending[0]
	if pending.Acceptance.ID != firstPending.Acceptance.ID {
		t.Fatalf("automatic accept retry changed signed payload: first=%q retry=%q", firstPending.Acceptance.ID, pending.Acceptance.ID)
	} else if pending.Order.ID != offer.Signed.ID || !pending.RelaySubmitted {
		t.Fatalf("pending acceptance was not acknowledged by relay: %#v", pending)
	}
	state = taker.State(context.Background())
	if state.Funds == nil || state.Funds.OpenOrders != 0 || state.Funds.Bitcoin.Reserved != "0.0001" || state.Funds.Bitcoin.Available != "1.2499" {
		t.Fatalf("taker pending acceptance reservation = %#v", state.Funds)
	}
	flakyCancellationRelay := &failOnceCancelAcceptanceRelay{relayAPI: taker.relay}
	taker.relay = flakyCancellationRelay
	cancellationResult, err := taker.CancelAcceptance(context.Background(), pending.Acceptance.ID)
	if err != nil {
		t.Fatal(err)
	} else if cancellationResult.Status != "cancelling" || cancellationResult.RelayAcknowledged {
		t.Fatalf("first acceptance cancellation result = %#v", cancellationResult)
	}
	negotiations, err = taker.Negotiations()
	if err != nil || len(negotiations.Pending) != 1 || negotiations.Pending[0].Cancellation == nil ||
		!strings.Contains(negotiations.Pending[0].CancellationError, "simulated cancellation relay timeout") || negotiations.Pending[0].CancellationRetryAt == 0 {
		t.Fatalf("durable pending cancellation=%#v err=%v", negotiations.Pending, err)
	}
	state = taker.State(context.Background())
	if state.Funds == nil || state.Funds.Bitcoin.Reserved != "0.0001" || state.Funds.Bitcoin.Available != "1.2499" {
		t.Fatalf("funds released before relay acknowledged cancellation = %#v", state.Funds)
	}
	taker.lastRelaySync = time.Time{}
	if err := taker.syncRelay(context.Background()); err != nil {
		t.Fatalf("automatic cancellation retry: %v", err)
	}
	negotiations, err = taker.Negotiations()
	if err != nil || len(negotiations.Pending) != 0 {
		t.Fatalf("pending negotiations after cancellation=%#v err=%v", negotiations.Pending, err)
	}
	state = taker.State(context.Background())
	if state.Funds == nil || state.Funds.Bitcoin.Reserved != "0" || state.Funds.Bitcoin.Available != "1.25" {
		t.Fatalf("relay-acknowledged cancellation did not release funds = %#v", state.Funds)
	}
	maker.lastRelaySync = time.Time{}
	if err := maker.syncRelay(context.Background()); err != nil {
		t.Fatalf("maker cancellation reconciliation: %v", err)
	}
	negotiations, err = maker.Negotiations()
	if err != nil || len(negotiations.Incoming) != 0 || len(negotiations.Swaps) != 0 {
		t.Fatalf("cancelled acceptance remained with maker: %#v err=%v", negotiations, err)
	}
	pending, err = taker.AcceptOffer(context.Background(), offer.Signed.ID)
	if err != nil {
		t.Fatal(err)
	} else if pending.Acceptance.ID == firstPending.Acceptance.ID || !pending.RelaySubmitted {
		t.Fatalf("new acceptance after cancellation = %#v", pending)
	}
	flakyRelay := &failOnceMatchRelay{relayAPI: maker.relay}
	maker.relay = flakyRelay
	maker.lastRelaySync = time.Time{}
	state = maker.State(context.Background())
	if !strings.Contains(state.RelayError, "simulated relay timeout") || state.IncomingTrades != 1 || state.ActiveSwaps != 1 {
		t.Fatalf("maker state after relay timeout = %#v", state)
	}
	negotiations, err = maker.Negotiations()
	if err != nil || len(negotiations.Incoming) != 1 || len(negotiations.Swaps) != 1 {
		t.Fatalf("maker negotiations=%#v err=%v", negotiations, err)
	}
	firstMatch := negotiations.Swaps[0]
	if !firstMatch.RelayMatchPending || firstMatch.Phase != swapstate.PhaseMatched {
		t.Fatalf("unconfirmed relay match became executable: %#v", firstMatch)
	}
	// Cross a timestamp boundary so recreating the signed match would produce a
	// different ID and make the durable journal reject the retry.
	time.Sleep(time.Until(time.Unix(time.Now().Unix()+1, 0)) + 20*time.Millisecond)
	maker.lastRelaySync = time.Time{}
	state = maker.State(context.Background())
	if state.RelayError != "" || state.IncomingTrades != 0 || state.ActiveSwaps != 1 {
		t.Fatalf("maker state after relay retry = %#v", state)
	} else if state.Funds == nil || state.Funds.OpenOrders != 0 || state.Funds.QDAY.Reserved != "1.25" || state.Funds.QDAY.Available != "3.75" {
		t.Fatalf("maker matched swap reservation = %#v", state.Funds)
	}
	negotiations, err = maker.Negotiations()
	if err != nil || len(negotiations.Swaps) != 1 {
		t.Fatalf("maker swaps=%#v err=%v", negotiations.Swaps, err)
	}
	matched := negotiations.Swaps[0]
	if matched.Match.ID != firstMatch.Match.ID {
		t.Fatalf("match retry changed signed payload: first=%q retry=%q", firstMatch.Match.ID, matched.Match.ID)
	} else if matched.Phase != swapstate.PhaseAsyncTakerFunding || matched.Role != swapstate.RoleMaker || matched.RelayMatchPending {
		t.Fatalf("maker swap = %#v", matched)
	}
	// A taker may return after the acceptance deadline even though the maker
	// committed the match before it. Reconciliation must preserve the prepared
	// funding until the taker consumes the durable relay match.
	if err := taker.reconcileNegotiations(
		context.Background(), taker.journal,
		time.Unix(pending.Acceptance.Acceptance.ExpiresAt+1, 0),
	); err != nil {
		t.Fatalf("reconcile matched acceptance after deadline: %v", err)
	}
	if negotiations, err = taker.Negotiations(); err != nil || len(negotiations.Pending) != 1 {
		t.Fatalf("matched acceptance was released after its deadline: %#v err=%v", negotiations.Pending, err)
	}
	cancellationResult, err = taker.CancelAcceptance(context.Background(), pending.Acceptance.ID)
	if err != nil {
		t.Fatalf("cancel already matched acceptance: %v", err)
	} else if cancellationResult.Status != "matched" || cancellationResult.RelayAcknowledged {
		t.Fatalf("already matched cancellation result = %#v", cancellationResult)
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
