package localui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
func (fakeApplication) Unlock(context.Context, string) error { return nil }
func (fakeApplication) Lock(context.Context) error           { return nil }
func (fakeApplication) RecoveryPhrase() (string, error)      { return "word", nil }
func (fakeApplication) QDAYReceiveAddress(context.Context) (walletd.Address, error) {
	return walletd.Address{Address: "qday1ptest"}, nil
}
func (fakeApplication) BitcoinReceiveAddress() (string, error) { return "bc1qtest", nil }
func (fakeApplication) Orders(context.Context, string, int) (relay.ResultPage, error) {
	return relay.ResultPage{Items: []relay.Record{}, Page: 1, PageSize: 20}, nil
}
func (fakeApplication) MarketPrice(context.Context) (relay.MarketPrice, error) {
	return relay.MarketPrice{Pair: "BTC-USD", USD: "81401.385", Source: "test"}, nil
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
func (fakeApplication) Negotiations() (app.Negotiations, error) {
	return app.Negotiations{Pending: []swapstate.Negotiation{}, Incoming: []swapstate.Negotiation{}, Swaps: []swapstate.Swap{}}, nil
}

func TestBootstrapSessionHostAndOriginProtection(t *testing.T) {
	const host = "127.0.0.1:42424"
	server, err := New(fakeApplication{}, host)
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
}
