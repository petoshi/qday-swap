package walletd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

const testToken = "0001020304050607080900010203040506070809000102030405060708090001"

func TestClientAuthenticatesAndDecodesExactAmounts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			t.Fatal("missing bearer token")
		}
		if r.URL.Path != "/v1/balance" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(Balance{
			Height: 42, Synced: true, UnitAtomic: "1000000000000000000000000",
			Spendable: Amount{Atomic: "987654321098765432109876543210", QDAY: "987654.32109876543210987654321"},
		})
	}))
	defer server.Close()
	client, err := NewClient(server.URL, testToken)
	if err != nil {
		t.Fatal(err)
	}
	balance, err := client.Balance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if balance.Spendable.Atomic != "987654321098765432109876543210" {
		t.Fatalf("atomic balance lost precision: %q", balance.Spendable.Atomic)
	}
}

func TestClientPreservesAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "refund height has not been reached"})
	}))
	defer server.Close()
	client, err := NewClient(server.URL, testToken)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.RefundSwap(context.Background(), "trade-1", SpendSwapRequest{OutputID: "deadbeef"})
	var apiError *APIError
	if !errors.As(err, &apiError) || apiError.StatusCode != http.StatusBadRequest || apiError.Message != "refund height has not been reached" {
		t.Fatalf("API error = %#v", err)
	}
}

func TestClientRejectsRemoteAndMalformedEndpoints(t *testing.T) {
	for _, endpoint := range []string{"https://127.0.0.1:19772", "http://example.com:19772", "http://127.0.0.1", "http://user@127.0.0.1:19772"} {
		if _, err := NewClient(endpoint, testToken); err == nil {
			t.Fatalf("accepted endpoint %q", endpoint)
		}
	}
	if _, err := NewClient("http://127.0.0.1:19772", "bad"); err == nil {
		t.Fatal("accepted malformed bearer token")
	}
}
