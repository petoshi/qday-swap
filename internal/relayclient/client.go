// Package relayclient talks to the public signed-order relay. It treats every
// response as untrusted transport data; callers still verify signed records.
package relayclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/petoshi/qday-swap/internal/order"
	"github.com/petoshi/qday-swap/internal/relay"
	"github.com/petoshi/qday-swap/internal/trade"
)

const maximumResponse = 2 << 20

type Client struct {
	baseURL string
	http    *http.Client
}

type Status struct {
	Service    string      `json:"service"`
	Version    string      `json:"version"`
	Network    string      `json:"network"`
	PublicURL  string      `json:"publicURL"`
	ServerTime string      `json:"serverTime"`
	Stats      relay.Stats `json:"stats"`
}

type HTTPError struct {
	StatusCode int
	Message    string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("relay HTTP %d: %s", e.StatusCode, e.Message) }

func New(baseURL string, client *http.Client) (*Client, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("relay URL must contain only scheme and host")
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && (parsed.Hostname() == "127.0.0.1" || parsed.Hostname() == "localhost" || parsed.Hostname() == "::1")) {
		return nil, errors.New("relay URL must use HTTPS outside loopback")
	}
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	return &Client{baseURL: baseURL, http: client}, nil
}

func (c *Client) Status(ctx context.Context) (Status, error) {
	var result Status
	err := c.do(ctx, http.MethodGet, "/api/v1/status", nil, &result)
	return result, err
}

func (c *Client) Price(ctx context.Context) (relay.MarketPrice, error) {
	var result relay.MarketPrice
	err := c.do(ctx, http.MethodGet, "/api/v1/price", nil, &result)
	return result, err
}

func (c *Client) Orders(ctx context.Context, status relay.Status, giveAsset string, page, limit int) (relay.ResultPage, error) {
	values := url.Values{}
	if status != "" {
		values.Set("status", string(status))
	}
	if giveAsset != "" {
		values.Set("giveAsset", giveAsset)
	}
	values.Set("market", order.MarketQDAYBTC)
	values.Set("page", strconv.Itoa(page))
	values.Set("limit", strconv.Itoa(limit))
	var result relay.ResultPage
	err := c.do(ctx, http.MethodGet, "/api/v1/orders?"+values.Encode(), nil, &result)
	return result, err
}

func (c *Client) Order(ctx context.Context, id string) (relay.Record, error) {
	var result relay.Record
	err := c.do(ctx, http.MethodGet, "/api/v1/orders/"+id, nil, &result)
	return result, err
}

func (c *Client) Publish(ctx context.Context, signed order.Signed) (relay.Record, error) {
	var result relay.Record
	err := c.do(ctx, http.MethodPost, "/api/v1/orders", signed, &result)
	return result, err
}

func (c *Client) Cancel(ctx context.Context, orderID string, cancellation order.SignedCancellation) (relay.Record, error) {
	var result relay.Record
	err := c.do(ctx, http.MethodPost, "/api/v1/orders/"+orderID+"/cancel", cancellation, &result)
	return result, err
}

func (c *Client) Accept(ctx context.Context, orderID string, acceptance trade.SignedAcceptance) (relay.AcceptanceRecord, error) {
	var result relay.AcceptanceRecord
	err := c.do(ctx, http.MethodPost, "/api/v1/orders/"+orderID+"/accept", acceptance, &result)
	return result, err
}

func (c *Client) Match(ctx context.Context, orderID string, match trade.SignedMatch) (relay.Record, error) {
	var result relay.Record
	err := c.do(ctx, http.MethodPost, "/api/v1/orders/"+orderID+"/match", match, &result)
	return result, err
}

func (c *Client) Message(ctx context.Context, message trade.SignedMessage) (trade.SignedMessage, error) {
	var result trade.SignedMessage
	err := c.do(ctx, http.MethodPost, "/api/v1/messages", message, &result)
	return result, err
}

func (c *Client) Poll(ctx context.Context, poll trade.SignedPoll) (relay.MailboxPage, error) {
	var result relay.MailboxPage
	err := c.do(ctx, http.MethodPost, "/api/v1/mailbox/poll", poll, &result)
	return result, err
}

func (c *Client) do(ctx context.Context, method, path string, requestBody, responseBody any) error {
	var body io.Reader
	if requestBody != nil {
		encoded, err := json.Marshal(requestBody)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "qday-swap/1")
	if requestBody != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("contact DEX relay: %w", err)
	}
	defer response.Body.Close()
	limited := io.LimitReader(response.Body, maximumResponse+1)
	payload, err := io.ReadAll(limited)
	if err != nil {
		return fmt.Errorf("read DEX relay response: %w", err)
	} else if len(payload) > maximumResponse {
		return errors.New("DEX relay response is too large")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var failure struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(payload, &failure) != nil || failure.Error == "" {
			failure.Error = http.StatusText(response.StatusCode)
		}
		return &HTTPError{StatusCode: response.StatusCode, Message: failure.Error}
	}
	if responseBody == nil {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(responseBody); err != nil {
		return fmt.Errorf("decode DEX relay response: %w", err)
	}
	return nil
}
