package relay

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/petoshi/qday-swap/internal/order"
)

func testHTTPServer(t *testing.T, now time.Time) (*httptest.Server, *Store) {
	t.Helper()
	store := openTestStore(t)
	server, err := NewServer(store, Config{
		Network: "mainnet", PublicURL: "https://dex.pqday.com", Version: "test",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	return httpServer, store
}

func requestJSON(t *testing.T, client *http.Client, method, endpoint string, body any) (*http.Response, map[string]any) {
	t.Helper()
	var encoded io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		encoded = bytes.NewReader(payload)
	}
	request, err := http.NewRequest(method, endpoint, encoded)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var decoded map[string]any
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	return response, decoded
}

func TestOrderAPIEndToEnd(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	server, _ := testHTTPServer(t, now)
	publicKey, privateKey := testSigner(t)
	signed := testSignedOrder(t, publicKey, privateKey, now, 0)

	response, decoded := requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/v1/orders", signed)
	if response.StatusCode != http.StatusCreated || decoded["status"] != "open" {
		t.Fatalf("publish status=%d body=%#v", response.StatusCode, decoded)
	}
	response, _ = requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/v1/orders", signed)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("idempotent publish status=%d", response.StatusCode)
	}

	response, decoded = requestJSON(t, server.Client(), http.MethodGet, server.URL+"/api/v1/orders?status=open&page=1&limit=20", nil)
	if response.StatusCode != http.StatusOK || decoded["total"] != float64(1) {
		t.Fatalf("list status=%d body=%#v", response.StatusCode, decoded)
	}
	response, decoded = requestJSON(t, server.Client(), http.MethodGet, server.URL+"/api/v1/orders/"+signed.ID, nil)
	if response.StatusCode != http.StatusOK || decoded["status"] != "open" {
		t.Fatalf("detail status=%d body=%#v", response.StatusCode, decoded)
	}

	cancellation, err := order.NewCancellation(signed, privateKey, now)
	if err != nil {
		t.Fatal(err)
	}
	response, decoded = requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/v1/orders/"+signed.ID+"/cancel", cancellation)
	if response.StatusCode != http.StatusOK || decoded["status"] != "cancelled" {
		t.Fatalf("cancel status=%d body=%#v", response.StatusCode, decoded)
	}
	response, decoded = requestJSON(t, server.Client(), http.MethodGet, server.URL+"/api/v1/status", nil)
	stats := decoded["stats"].(map[string]any)
	if response.StatusCode != http.StatusOK || stats["cancelled"] != float64(1) || stats["open"] != float64(0) {
		t.Fatalf("relay status=%d body=%#v", response.StatusCode, decoded)
	}
}

func TestOrderAPIRejectsWrongNetworkAndTampering(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	server, _ := testHTTPServer(t, now)
	publicKey, privateKey := testSigner(t)
	payload, err := order.NewPayload("testnet", order.QDAYLegacyUnit, order.Amount{Asset: "QDAY", Atomic: order.QDAYLegacyUnit}, order.Amount{Asset: "BTC", Atomic: "1000"}, time.Hour, publicKey, now)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := order.Sign(payload, privateKey, now)
	if err != nil {
		t.Fatal(err)
	}
	response, decoded := requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/v1/orders", signed)
	if response.StatusCode != http.StatusBadRequest || !strings.Contains(decoded["error"].(string), "mainnet") {
		t.Fatalf("wrong network status=%d body=%#v", response.StatusCode, decoded)
	}

	signed = testSignedOrder(t, publicKey, privateKey, now, 0)
	signed.Order.Receive.Atomic = "1"
	response, decoded = requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/v1/orders", signed)
	if response.StatusCode != http.StatusBadRequest || !strings.Contains(decoded["error"].(string), "ID") {
		t.Fatalf("tamper status=%d body=%#v", response.StatusCode, decoded)
	}
}

func TestWebRoutesAndSecurityHeaders(t *testing.T) {
	server, _ := testHTTPServer(t, time.Unix(1_800_000_000, 0))
	health, err := server.Client().Get(server.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	healthBody, err := io.ReadAll(health.Body)
	health.Body.Close()
	if err != nil || health.StatusCode != http.StatusOK || string(healthBody) != "ok\n" {
		t.Fatalf("health status=%d body=%q err=%v", health.StatusCode, healthBody, err)
	}
	for _, route := range []string{"/", "/activity", "/protocol", "/order/example"} {
		response, err := server.Client().Get(server.URL + route)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		} else if response.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("QDAY DEX")) {
			t.Fatalf("route %s status=%d", route, response.StatusCode)
		} else if response.Header.Get("Content-Security-Policy") == "" || response.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("route %s missing security headers", route)
		}
	}
	response, err := server.Client().Get(server.URL + "/api/nope")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotFound || !strings.HasPrefix(response.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("unknown API status=%d content-type=%q", response.StatusCode, response.Header.Get("Content-Type"))
	}
}
