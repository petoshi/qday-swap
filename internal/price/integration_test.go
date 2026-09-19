//go:build integration

package price

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestLiveBTCUSDQuorum(t *testing.T) {
	aggregator, err := DefaultAggregator(&http.Client{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	snapshot, err := aggregator.Fetch(ctx)
	if err != nil {
		t.Fatal(err)
	} else if len(snapshot.Quotes) < 2 {
		t.Fatalf("source quorum = %d", len(snapshot.Quotes))
	}
	t.Logf("BTC/USD %s from %d sources, spread %d bps", snapshot.PriceUSD, len(snapshot.Quotes), snapshot.SpreadBPS)
}
