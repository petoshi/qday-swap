package relay

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/petoshi/qday-swap/internal/order"
	"github.com/petoshi/qday-swap/internal/trade"
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
	messageKey := [32]byte{1}
	payload, err := order.NewPayload("testnet", order.QDAYLegacyUnit, order.Amount{Asset: "QDAY", Atomic: order.QDAYLegacyUnit}, order.Amount{Asset: "BTC", Atomic: "1000"}, time.Hour, publicKey, messageKey, now)
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
	for _, route := range []string{"/", "/orders", "/activity", "/protocol", "/order/example"} {
		response, err := server.Client().Get(server.URL + route)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		} else if response.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("QDAY Order Explorer")) {
			t.Fatalf("route %s status=%d", route, response.StatusCode)
		} else if response.Header.Get("Content-Security-Policy") == "" || response.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("route %s missing security headers", route)
		}
	}
	for _, asset := range []string{"/app.js?v=5", "/styles.css?v=5"} {
		response, err := server.Client().Get(server.URL + asset)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-cache" {
			t.Fatalf("asset %s status=%d cache-control=%q", asset, response.StatusCode, response.Header.Get("Cache-Control"))
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

func TestBitcoinUSDPriceIsValidatedCachedAndSurvivesProviderFailure(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	requests := 0
	provider := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		requests++
		response.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(response, `{"data":{"amount":"81401.385","base":"BTC","currency":"USD"}}`)
	}))
	store := openTestStore(t)
	relayServer, err := NewServer(store, Config{
		Network: "mainnet", PublicURL: "https://dex.pqday.com", Version: "test",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return now },
		PriceURL: provider.URL, HTTPClient: provider.Client(), PriceTTL: 20 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(relayServer.Handler())
	t.Cleanup(server.Close)

	for range 2 {
		response, decoded := requestJSON(t, server.Client(), http.MethodGet, server.URL+"/api/v1/price", nil)
		if response.StatusCode != http.StatusOK || decoded["usd"] != "81401.385" || decoded["source"] != "Coinbase" {
			t.Fatalf("price status=%d body=%#v", response.StatusCode, decoded)
		}
	}
	if requests != 1 {
		t.Fatalf("provider requests=%d, want 1", requests)
	}

	provider.Close()
	now = now.Add(21 * time.Second)
	response, decoded := requestJSON(t, server.Client(), http.MethodGet, server.URL+"/api/v1/price", nil)
	if response.StatusCode != http.StatusOK || decoded["stale"] != true || decoded["usd"] != "81401.385" {
		t.Fatalf("stale price status=%d body=%#v", response.StatusCode, decoded)
	}
}

func TestNegotiationAndEncryptedMailboxAPI(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	server, _ := testHTTPServer(t, now)
	fixture := newRelayTradeFixture(t, now)

	response, _ := requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/v1/orders", fixture.order)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("publish status=%d", response.StatusCode)
	}
	response, decoded := requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/v1/orders/"+fixture.order.ID+"/accept", fixture.acceptance)
	if response.StatusCode != http.StatusCreated || decoded["signed"].(map[string]any)["id"] != fixture.acceptance.ID {
		t.Fatalf("accept status=%d body=%#v", response.StatusCode, decoded)
	}

	makerPoll, err := trade.NewPoll("mainnet", 0, 20, fixture.makerPrivate, now)
	if err != nil {
		t.Fatal(err)
	}
	response, decoded = requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/v1/mailbox/poll", makerPoll)
	items := decoded["items"].([]any)
	if response.StatusCode != http.StatusOK || len(items) != 1 || items[0].(map[string]any)["kind"] != string(MailboxAcceptance) {
		t.Fatalf("maker poll status=%d body=%#v", response.StatusCode, decoded)
	}

	response, decoded = requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/v1/orders/"+fixture.order.ID+"/match", fixture.match)
	if response.StatusCode != http.StatusOK || decoded["status"] != string(StatusMatched) {
		t.Fatalf("match status=%d body=%#v", response.StatusCode, decoded)
	}
	response, decoded = requestJSON(t, server.Client(), http.MethodGet, server.URL+"/api/v1/status", nil)
	stats := decoded["stats"].(map[string]any)
	lastTrade := stats["lastTrade"].(map[string]any)
	if response.StatusCode != http.StatusOK || lastTrade["qdayAtomic"] == "" || lastTrade["btcAtomic"] == "" || lastTrade["matchedAt"] != float64(fixture.match.Match.CreatedAt) {
		t.Fatalf("last trade status=%d body=%#v", response.StatusCode, decoded)
	}
	message, err := trade.EncryptMessage("mainnet", fixture.order.ID, fixture.acceptance.Acceptance.TradeID, 1,
		fixture.makerPrivate, fixture.takerPrivate.Public().(ed25519.PublicKey), fixture.makerMessagePrivate,
		fixture.takerMessagePublic, []byte("signed contract proposal"), time.Hour, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	response, decoded = requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/v1/messages", message)
	if response.StatusCode != http.StatusCreated || decoded["id"] != message.ID {
		t.Fatalf("message status=%d body=%#v", response.StatusCode, decoded)
	}

	takerPoll, err := trade.NewPoll("mainnet", 0, 20, fixture.takerPrivate, now)
	if err != nil {
		t.Fatal(err)
	}
	response, decoded = requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/v1/mailbox/poll", takerPoll)
	items = decoded["items"].([]any)
	if response.StatusCode != http.StatusOK || len(items) != 2 || items[0].(map[string]any)["kind"] != string(MailboxMatch) || items[1].(map[string]any)["kind"] != string(MailboxMessage) {
		t.Fatalf("taker poll status=%d body=%#v", response.StatusCode, decoded)
	}

	tamperedPoll := takerPoll
	tamperedPoll.Poll.After = 10
	response, decoded = requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/v1/mailbox/poll", tamperedPoll)
	if response.StatusCode != http.StatusBadRequest || !strings.Contains(decoded["error"].(string), "signature") {
		t.Fatalf("tampered poll status=%d body=%#v", response.StatusCode, decoded)
	}
}
