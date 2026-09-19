// Package price provides display-only fiat reference prices. Price data never
// participates in swap settlement or signed trade terms.
package price

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	satoshisPerBitcoin = int64(100_000_000)
	maxResponseBytes   = 1 << 20
	maxPriceLength     = 64
	defaultOutlierBPS  = uint64(200)
)

// Quote is one independently fetched BTC/USD observation.
type Quote struct {
	Source     string    `json:"source"`
	PriceUSD   string    `json:"priceUSD"`
	ObservedAt time.Time `json:"observedAt"`

	price *big.Rat
}

// Snapshot is the median of at least two mutually consistent observations.
// PriceUSD is formatted for display. Exact calculations use the private
// rational retained by the snapshot.
type Snapshot struct {
	PriceUSD   string    `json:"priceUSD"`
	ObservedAt time.Time `json:"observedAt"`
	SpreadBPS  uint64    `json:"spreadBPS"`
	Quotes     []Quote   `json:"quotes"`

	price *big.Rat
}

// Source returns one BTC/USD spot observation without requiring credentials.
type Source interface {
	Name() string
	Fetch(context.Context) (Quote, error)
}

// Aggregator fetches independent spot markets and rejects inconsistent data.
type Aggregator struct {
	sources    []Source
	outlierBPS uint64
}

func NewAggregator(sources ...Source) (*Aggregator, error) {
	if len(sources) < 2 {
		return nil, errors.New("at least two BTC/USD price sources are required")
	}
	seen := make(map[string]bool, len(sources))
	for _, source := range sources {
		if source == nil || strings.TrimSpace(source.Name()) == "" {
			return nil, errors.New("price source has no name")
		} else if seen[source.Name()] {
			return nil, fmt.Errorf("duplicate price source %q", source.Name())
		}
		seen[source.Name()] = true
	}
	return &Aggregator{sources: append([]Source(nil), sources...), outlierBPS: defaultOutlierBPS}, nil
}

// DefaultAggregator uses three public BTC/USD spot markets. The caller may
// provide an HTTP client with its own transport; a bounded default is used when
// client is nil.
func DefaultAggregator(client *http.Client) (*Aggregator, error) {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	return NewAggregator(
		Coinbase(client, "https://api.exchange.coinbase.com/products/BTC-USD/ticker"),
		Kraken(client, "https://api.kraken.com/0/public/Ticker?pair=XBTUSD"),
		Bitstamp(client, "https://www.bitstamp.net/api/v2/ticker/btcusd/"),
	)
}

type sourceResult struct {
	quote Quote
	err   error
}

// Fetch obtains all sources concurrently, removes a lone outlier and returns a
// median only when at least two independent prices agree within two percent.
func (a *Aggregator) Fetch(ctx context.Context) (Snapshot, error) {
	results := make(chan sourceResult, len(a.sources))
	for _, source := range a.sources {
		source := source
		go func() {
			quote, err := source.Fetch(ctx)
			if err == nil {
				quote.Source = source.Name()
				if quote.ObservedAt.IsZero() {
					quote.ObservedAt = time.Now().UTC()
				}
				quote.price, err = parsePositiveDecimal(quote.PriceUSD)
			}
			results <- sourceResult{quote: quote, err: err}
		}()
	}

	quotes := make([]Quote, 0, len(a.sources))
	var failures []string
	for range a.sources {
		result := <-results
		if result.err != nil {
			failures = append(failures, result.err.Error())
			continue
		}
		quotes = append(quotes, result.quote)
	}
	if len(quotes) < 2 {
		return Snapshot{}, fmt.Errorf("BTC/USD unavailable: received %d valid sources (%s)", len(quotes), strings.Join(failures, "; "))
	}

	sort.Slice(quotes, func(i, j int) bool { return quotes[i].price.Cmp(quotes[j].price) < 0 })
	center := median(quotes)
	inliers := quotes[:0]
	for _, quote := range quotes {
		if deviationBPS(quote.price, center) <= a.outlierBPS {
			inliers = append(inliers, quote)
		}
	}
	if len(inliers) < 2 {
		return Snapshot{}, errors.New("BTC/USD sources disagree by more than two percent")
	}
	center = median(inliers)
	spread := spreadBPS(inliers, center)
	observed := inliers[0].ObservedAt
	for _, quote := range inliers[1:] {
		if quote.ObservedAt.After(observed) {
			observed = quote.ObservedAt
		}
	}
	return Snapshot{
		PriceUSD: center.FloatString(2), ObservedAt: observed, SpreadBPS: spread,
		Quotes: append([]Quote(nil), inliers...), price: new(big.Rat).Set(center),
	}, nil
}

func median(quotes []Quote) *big.Rat {
	mid := len(quotes) / 2
	if len(quotes)%2 == 1 {
		return new(big.Rat).Set(quotes[mid].price)
	}
	return new(big.Rat).Quo(new(big.Rat).Add(quotes[mid-1].price, quotes[mid].price), big.NewRat(2, 1))
}

func deviationBPS(value, center *big.Rat) uint64 {
	delta := new(big.Rat).Sub(value, center)
	if delta.Sign() < 0 {
		delta.Neg(delta)
	}
	delta.Mul(delta, big.NewRat(10_000, 1))
	delta.Quo(delta, center)
	return ratFloorUint64(delta)
}

func spreadBPS(quotes []Quote, center *big.Rat) uint64 {
	spread := new(big.Rat).Sub(quotes[len(quotes)-1].price, quotes[0].price)
	spread.Mul(spread, big.NewRat(10_000, 1))
	spread.Quo(spread, center)
	return ratFloorUint64(spread)
}

func ratFloorUint64(value *big.Rat) uint64 {
	if value.Sign() <= 0 {
		return 0
	}
	integer := new(big.Int).Quo(value.Num(), value.Denom())
	if !integer.IsUint64() {
		return ^uint64(0)
	}
	return integer.Uint64()
}

func parsePositiveDecimal(value string) (*big.Rat, error) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxPriceLength || strings.HasPrefix(value, "+") || strings.HasPrefix(value, "-") || strings.ContainsAny(value, "eE") {
		return nil, errors.New("price is not a positive plain decimal")
	}
	dot := false
	digit := false
	for _, r := range value {
		switch {
		case r >= '0' && r <= '9':
			digit = true
		case r == '.' && !dot:
			dot = true
		default:
			return nil, errors.New("price is not a positive plain decimal")
		}
	}
	price, ok := new(big.Rat).SetString(value)
	if !digit || !ok || price.Sign() <= 0 {
		return nil, errors.New("price is not a positive plain decimal")
	}
	return price, nil
}

func positiveInteger(name, value string) (*big.Int, error) {
	if value == "" || (len(value) > 1 && value[0] == '0') {
		return nil, fmt.Errorf("%s must be a canonical positive integer", name)
	}
	integer, ok := new(big.Int).SetString(value, 10)
	if !ok || integer.Sign() <= 0 {
		return nil, fmt.Errorf("%s must be a canonical positive integer", name)
	}
	return integer, nil
}

// BTCValueUSD returns the exact display-only USD value of a satoshi amount.
func (s Snapshot) BTCValueUSD(satoshis string) (*big.Rat, error) {
	if s.price == nil || s.price.Sign() <= 0 {
		return nil, errors.New("BTC/USD snapshot is empty")
	}
	atoms, err := positiveInteger("satoshis", satoshis)
	if err != nil {
		return nil, err
	}
	value := new(big.Rat).Mul(s.price, new(big.Rat).SetInt(atoms))
	return value.Quo(value, big.NewRat(satoshisPerBitcoin, 1)), nil
}

// QDAYUnitUSD derives the USD value of one currently displayed QDAY from an
// exact offer. qdayAtomic is the QDAY side of the offer, satoshis is the BTC
// side, and unitAtomic comes from the QDAY chain state at the trade height.
func (s Snapshot) QDAYUnitUSD(qdayAtomic, satoshis, unitAtomic string) (*big.Rat, error) {
	qday, err := positiveInteger("QDAY atomic amount", qdayAtomic)
	if err != nil {
		return nil, err
	}
	btc, err := positiveInteger("satoshis", satoshis)
	if err != nil {
		return nil, err
	}
	unit, err := positiveInteger("QDAY unit", unitAtomic)
	if err != nil {
		return nil, err
	}
	if s.price == nil || s.price.Sign() <= 0 {
		return nil, errors.New("BTC/USD snapshot is empty")
	}
	value := new(big.Rat).Mul(s.price, new(big.Rat).SetInt(btc))
	value.Mul(value, new(big.Rat).SetInt(unit))
	denominator := new(big.Int).Mul(qday, big.NewInt(satoshisPerBitcoin))
	return value.Quo(value, new(big.Rat).SetInt(denominator)), nil
}

// FormatUSD rounds an exact rational to a UI string with the requested number
// of decimal places.
func FormatUSD(value *big.Rat, decimals int) (string, error) {
	if value == nil || value.Sign() < 0 || decimals < 0 || decimals > 12 {
		return "", errors.New("invalid USD value")
	}
	return value.FloatString(decimals), nil
}

type httpSource struct {
	name   string
	url    string
	client *http.Client
	decode func([]byte) (string, time.Time, error)
}

func (s *httpSource) Name() string { return s.name }

func (s *httpSource) Fetch(ctx context.Context) (Quote, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return Quote{}, fmt.Errorf("%s request: %w", s.name, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "qday-swap/price")
	response, err := s.client.Do(req)
	if err != nil {
		return Quote{}, fmt.Errorf("%s: %w", s.name, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return Quote{}, fmt.Errorf("%s returned HTTP %s", s.name, response.Status)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return Quote{}, fmt.Errorf("%s response: %w", s.name, err)
	} else if len(body) > maxResponseBytes {
		return Quote{}, fmt.Errorf("%s response is too large", s.name)
	}
	price, observed, err := s.decode(body)
	if err != nil {
		return Quote{}, fmt.Errorf("%s response: %w", s.name, err)
	}
	if observed.IsZero() {
		observed = time.Now().UTC()
	}
	return Quote{Source: s.name, PriceUSD: price, ObservedAt: observed}, nil
}

func Coinbase(client *http.Client, endpoint string) Source {
	return &httpSource{name: "Coinbase", url: endpoint, client: client, decode: func(body []byte) (string, time.Time, error) {
		var response struct {
			Price string `json:"price"`
			Time  string `json:"time"`
		}
		if err := json.Unmarshal(body, &response); err != nil {
			return "", time.Time{}, err
		}
		observed, _ := time.Parse(time.RFC3339Nano, response.Time)
		return response.Price, observed, nil
	}}
}

func Kraken(client *http.Client, endpoint string) Source {
	return &httpSource{name: "Kraken", url: endpoint, client: client, decode: func(body []byte) (string, time.Time, error) {
		var response struct {
			Error  []string `json:"error"`
			Result map[string]struct {
				Close []string `json:"c"`
			} `json:"result"`
		}
		if err := json.Unmarshal(body, &response); err != nil {
			return "", time.Time{}, err
		} else if len(response.Error) != 0 {
			return "", time.Time{}, errors.New(strings.Join(response.Error, ", "))
		} else if len(response.Result) != 1 {
			return "", time.Time{}, fmt.Errorf("expected one ticker, got %d", len(response.Result))
		}
		for _, ticker := range response.Result {
			if len(ticker.Close) == 0 {
				return "", time.Time{}, errors.New("ticker has no last trade")
			}
			return ticker.Close[0], time.Time{}, nil
		}
		panic("unreachable")
	}}
}

func Bitstamp(client *http.Client, endpoint string) Source {
	return &httpSource{name: "Bitstamp", url: endpoint, client: client, decode: func(body []byte) (string, time.Time, error) {
		var response struct {
			Last      string `json:"last"`
			Timestamp string `json:"timestamp"`
		}
		if err := json.Unmarshal(body, &response); err != nil {
			return "", time.Time{}, err
		}
		seconds, err := strconv.ParseInt(response.Timestamp, 10, 64)
		if err != nil {
			return "", time.Time{}, errors.New("invalid ticker timestamp")
		}
		return response.Last, time.Unix(seconds, 0).UTC(), nil
	}}
}
