package price

import (
	"context"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type fixedSource struct {
	name  string
	price string
	err   error
}

func (s fixedSource) Name() string { return s.name }
func (s fixedSource) Fetch(context.Context) (Quote, error) {
	return Quote{PriceUSD: s.price, ObservedAt: time.Unix(1_700_000_000, 0).UTC()}, s.err
}

func TestAggregatorMedianAndOutlier(t *testing.T) {
	aggregator, err := NewAggregator(
		fixedSource{name: "a", price: "80000.00"},
		fixedSource{name: "b", price: "80040.00"},
		fixedSource{name: "bad", price: "120000.00"},
	)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := aggregator.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	} else if snapshot.PriceUSD != "80020.00" {
		t.Fatalf("median price = %s", snapshot.PriceUSD)
	} else if len(snapshot.Quotes) != 2 {
		t.Fatalf("inlier count = %d", len(snapshot.Quotes))
	} else if snapshot.SpreadBPS != 4 {
		t.Fatalf("spread = %d bps", snapshot.SpreadBPS)
	}
}

func TestAggregatorRejectsDisagreementAndOneSource(t *testing.T) {
	aggregator, err := NewAggregator(
		fixedSource{name: "a", price: "80000"},
		fixedSource{name: "b", price: "90000"},
		fixedSource{name: "down", err: fmt.Errorf("offline")},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := aggregator.Fetch(context.Background()); err == nil {
		t.Fatal("disagreeing sources accepted")
	}

	aggregator, err = NewAggregator(
		fixedSource{name: "a", price: "80000"},
		fixedSource{name: "down", err: fmt.Errorf("offline")},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := aggregator.Fetch(context.Background()); err == nil {
		t.Fatal("single source accepted")
	}
}

func TestExactOfferValuationUsesCurrentQDAYUnit(t *testing.T) {
	aggregator, err := NewAggregator(
		fixedSource{name: "a", price: "80000"},
		fixedSource{name: "b", price: "80000"},
	)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := aggregator.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	total, err := snapshot.BTCValueUSD("2500000")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := FormatUSD(total, 2); got != "2000.00" {
		t.Fatalf("BTC value = %s", got)
	}

	// 100 QDAY for 0.025 BTC means one QDAY is exactly 20 USD. The
	// displayed QDAY unit is passed explicitly instead of assuming 10^24.
	unit := new(big.Int).Exp(big.NewInt(10), big.NewInt(24), nil)
	qday := new(big.Int).Mul(big.NewInt(100), unit)
	perQDAY, err := snapshot.QDAYUnitUSD(qday.String(), "2500000", unit.String())
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := FormatUSD(perQDAY, 4); got != "20.0000" {
		t.Fatalf("QDAY price = %s", got)
	}
}

func TestHTTPSourceDecoders(t *testing.T) {
	tests := []struct {
		name string
		body string
		new  func(*http.Client, string) Source
	}{
		{"coinbase", `{"price":"80123.45","time":"2026-09-19T10:00:00Z"}`, Coinbase},
		{"kraken", `{"error":[],"result":{"XXBTZUSD":{"c":["80123.46","0.1"]}}}`, Kraken},
		{"bitstamp", `{"last":"80123.47","timestamp":"1789812000"}`, Bitstamp},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			quote, err := test.new(server.Client(), server.URL).Fetch(context.Background())
			if err != nil {
				t.Fatal(err)
			} else if quote.PriceUSD == "" || quote.ObservedAt.IsZero() {
				t.Fatalf("incomplete quote: %+v", quote)
			}
		})
	}
}

func TestRejectsNonPlainPrices(t *testing.T) {
	for _, value := range []string{"", "0", "-1", "+1", "1e5", "NaN", "1.2.3"} {
		if _, err := parsePositiveDecimal(value); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
}
