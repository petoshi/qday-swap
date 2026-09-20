package localui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/petoshi/qday-swap/internal/app"
	"github.com/petoshi/qday-swap/internal/relay"
	"github.com/petoshi/qday-swap/internal/swapstate"
	"github.com/petoshi/qday-swap/internal/walletd"
)

type fakeApplication struct{}

func (fakeApplication) State(context.Context) app.State {
	return app.State{Configured: true, Network: "mainnet"}
}
func (fakeApplication) Setup(context.Context, string, string) (app.SetupResult, error) {
	return app.SetupResult{}, nil
}
func (fakeApplication) Unlock(context.Context, string) error  { return nil }
func (fakeApplication) Lock(context.Context) error            { return nil }
func (fakeApplication) RecoveryPhrase(string) (string, error) { return "word", nil }
func (fakeApplication) ImportRecovery(context.Context, string, string) (app.State, error) {
	return app.State{Configured: true, Unlocked: true}, nil
}
func (fakeApplication) QDAYReceiveAddress(context.Context) (walletd.Address, error) {
	return walletd.Address{Address: "qday1ptest"}, nil
}
func (fakeApplication) BitcoinReceiveAddress() (string, error) { return "bc1qtest", nil }
func (fakeApplication) QuoteWithdrawal(context.Context, app.WithdrawalQuoteRequest) (app.WithdrawalQuote, error) {
	return app.WithdrawalQuote{RequestID: "wallet-test", Asset: "QDAY", Amount: "1", Fee: "0.001", Total: "1.001"}, nil
}
func (fakeApplication) SendWithdrawal(context.Context, app.SendWithdrawalRequest) (app.WithdrawalResult, error) {
	return app.WithdrawalResult{Asset: "QDAY", TransactionID: "abcdef", Status: "broadcast"}, nil
}
func (fakeApplication) Orders(context.Context, string, int) (relay.ResultPage, error) {
	return relay.ResultPage{Items: []relay.Record{}, Page: 1, PageSize: 20}, nil
}
func (fakeApplication) MarketPrice(context.Context) (relay.MarketPrice, error) {
	return relay.MarketPrice{Pair: "BTC-USD", USD: "81401.385", Source: "test"}, nil
}
func (fakeApplication) MarketTrades(context.Context, int, int64) (relay.TradeHistory, error) {
	return relay.TradeHistory{Items: []relay.Trade{}}, nil
}
func (fakeApplication) QuoteOffer(context.Context, app.QuoteOfferRequest) (app.OfferQuote, error) {
	return app.OfferQuote{Side: "buy", Quantity: "10", BTCAmount: "0.000125"}, nil
}
func (fakeApplication) CreateOffer(context.Context, app.CreateOfferRequest) (relay.Record, error) {
	return relay.Record{}, nil
}
func (fakeApplication) CancelOffer(context.Context, string) (relay.Record, error) {
	return relay.Record{}, nil
}
func (fakeApplication) AcceptOffer(context.Context, string) (swapstate.Negotiation, error) {
	return swapstate.Negotiation{}, nil
}
func (fakeApplication) MatchAcceptance(context.Context, string) (swapstate.Swap, error) {
	return swapstate.Swap{}, nil
}
func (fakeApplication) ApproveSwap(string) (swapstate.Swap, error) {
	return swapstate.Swap{Approved: true}, nil
}
func (fakeApplication) Negotiations() (app.Negotiations, error) {
	return app.Negotiations{Pending: []swapstate.Negotiation{}, Incoming: []swapstate.Negotiation{}, Swaps: []swapstate.Swap{}}, nil
}

func TestBootstrapSessionHostAndOriginProtection(t *testing.T) {
	const host = "127.0.0.1:42424"
	shutdown := make(chan struct{}, 1)
	server, err := New(fakeApplication{}, host, func() { shutdown <- struct{}{} })
	if err != nil {
		t.Fatal(err)
	}

	unauthorized := httptest.NewRequest(http.MethodGet, "http://"+host+"/api/v1/state", nil)
	unauthorized.Host = host
	response := httptest.NewRecorder()
	server.ServeHTTP(response, unauthorized)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", response.Code)
	}

	bootstrap := httptest.NewRequest(http.MethodGet, server.OpenURL(), nil)
	bootstrap.Host = host
	response = httptest.NewRecorder()
	server.ServeHTTP(response, bootstrap)
	if response.Code != http.StatusSeeOther || len(response.Result().Cookies()) != 1 {
		t.Fatalf("bootstrap status=%d cookies=%#v", response.Code, response.Result().Cookies())
	}
	cookie := response.Result().Cookies()[0]
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("weak session cookie = %#v", cookie)
	}

	stateRequest := httptest.NewRequest(http.MethodGet, "http://"+host+"/api/v1/state", nil)
	stateRequest.Host = host
	stateRequest.AddCookie(cookie)
	response = httptest.NewRecorder()
	server.ServeHTTP(response, stateRequest)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"configured":true`) {
		t.Fatalf("state status=%d body=%q", response.Code, response.Body.String())
	}

	quoteRequest := httptest.NewRequest(http.MethodPost, "http://"+host+"/api/v1/offers/quote", strings.NewReader(`{"side":"buy","quantity":"10","price":"1","priceCurrency":"USD"}`))
	quoteRequest.Host = host
	quoteRequest.Header.Set("Content-Type", "application/json")
	quoteRequest.Header.Set("Origin", "http://"+host)
	quoteRequest.AddCookie(cookie)
	response = httptest.NewRecorder()
	server.ServeHTTP(response, quoteRequest)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"btcAmount":"0.000125"`) {
		t.Fatalf("quote status=%d body=%q", response.Code, response.Body.String())
	}

	walletQuoteRequest := httptest.NewRequest(http.MethodPost, "http://"+host+"/api/v1/wallets/quote", strings.NewReader(`{"asset":"QDAY","destination":"qday1ptest","amount":"1"}`))
	walletQuoteRequest.Host = host
	walletQuoteRequest.Header.Set("Content-Type", "application/json")
	walletQuoteRequest.Header.Set("Origin", "http://"+host)
	walletQuoteRequest.AddCookie(cookie)
	response = httptest.NewRecorder()
	server.ServeHTTP(response, walletQuoteRequest)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"requestID":"wallet-test"`) {
		t.Fatalf("wallet quote status=%d body=%q", response.Code, response.Body.String())
	}

	walletSendRequest := httptest.NewRequest(http.MethodPost, "http://"+host+"/api/v1/wallets/send", strings.NewReader(`{"requestID":"wallet-test","asset":"QDAY","destination":"qday1ptest","amountAtomic":"1","feeAtomic":"1","unitAtomic":"1"}`))
	walletSendRequest.Host = host
	walletSendRequest.Header.Set("Content-Type", "application/json")
	walletSendRequest.Header.Set("Origin", "http://"+host)
	walletSendRequest.AddCookie(cookie)
	response = httptest.NewRecorder()
	server.ServeHTTP(response, walletSendRequest)
	if response.Code != http.StatusCreated || !strings.Contains(response.Body.String(), `"transactionID":"abcdef"`) {
		t.Fatalf("wallet send status=%d body=%q", response.Code, response.Body.String())
	}

	recoveryRequest := httptest.NewRequest(http.MethodPost, "http://"+host+"/api/v1/recovery", strings.NewReader(`{"password":"correct horse battery staple"}`))
	recoveryRequest.Host = host
	recoveryRequest.Header.Set("Content-Type", "application/json")
	recoveryRequest.Header.Set("Origin", "http://"+host)
	recoveryRequest.AddCookie(cookie)
	response = httptest.NewRecorder()
	server.ServeHTTP(response, recoveryRequest)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"phrase":"word"`) {
		t.Fatalf("recovery status=%d body=%q", response.Code, response.Body.String())
	}

	badOrigin := httptest.NewRequest(http.MethodPost, "http://"+host+"/api/v1/lock", strings.NewReader(`{}`))
	badOrigin.Host = host
	badOrigin.Header.Set("Content-Type", "application/json")
	badOrigin.Header.Set("Origin", "https://attacker.example")
	badOrigin.AddCookie(cookie)
	response = httptest.NewRecorder()
	server.ServeHTTP(response, badOrigin)
	if response.Code != http.StatusForbidden {
		t.Fatalf("cross-origin status = %d", response.Code)
	}

	wrongHost := httptest.NewRequest(http.MethodGet, "http://attacker.example/api/v1/state", nil)
	wrongHost.Host = "attacker.example"
	wrongHost.AddCookie(cookie)
	response = httptest.NewRecorder()
	server.ServeHTTP(response, wrongHost)
	if response.Code != http.StatusMisdirectedRequest {
		t.Fatalf("wrong-host status = %d", response.Code)
	}

	shutdownRequest := httptest.NewRequest(http.MethodPost, "http://"+host+"/api/v1/shutdown", strings.NewReader(`{}`))
	shutdownRequest.Host = host
	shutdownRequest.Header.Set("Content-Type", "application/json")
	shutdownRequest.Header.Set("Origin", "http://"+host)
	shutdownRequest.AddCookie(cookie)
	response = httptest.NewRecorder()
	server.ServeHTTP(response, shutdownRequest)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"status":"shutting down"`) {
		t.Fatalf("shutdown status=%d body=%q", response.Code, response.Body.String())
	}
	select {
	case <-shutdown:
	case <-time.After(time.Second):
		t.Fatal("shutdown callback was not called")
	}
}
