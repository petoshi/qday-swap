package localui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/petoshi/qday-swap/internal/app"
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
