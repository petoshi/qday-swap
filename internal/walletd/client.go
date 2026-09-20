// Package walletd provides the private loopback client used by QDAY Swap to
// control its bundled qday-walletd process. The bearer token is read from the
// walletd data directory and never exposed to the browser interface.
package walletd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const maxResponseSize = 4 << 20

type Client struct {
	endpoint string
	token    string
	http     *http.Client
}

func NewClient(endpoint, token string) (*Client, error) {
	parsed, err := url.Parse(strings.TrimRight(endpoint, "/"))
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("walletd endpoint must be a plain loopback HTTP URL")
	}
	host, _, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		return nil, errors.New("walletd endpoint must include a port")
	}
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, errors.New("walletd endpoint must use a loopback address")
	}
	if len(token) != 64 {
		return nil, errors.New("walletd token must contain 32 hexadecimal bytes")
	}
	for _, character := range token {
		if !strings.ContainsRune("0123456789abcdef", character) {
			return nil, errors.New("walletd token must contain 32 lowercase hexadecimal bytes")
		}
	}
	return &Client{
		endpoint: strings.TrimRight(endpoint, "/"), token: token,
		http: &http.Client{Timeout: 65 * time.Second},
	}, nil
}

type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("walletd returned HTTP %d: %s", e.StatusCode, e.Message)
}

func (c *Client) request(ctx context.Context, method, path string, input, output any, authenticated bool) error {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return fmt.Errorf("encode walletd request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.endpoint+path, body)
	if err != nil {
		return err
	}
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if authenticated {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("call walletd %s: %w", path, err)
	}
	defer response.Body.Close()
	encoded, err := io.ReadAll(io.LimitReader(response.Body, maxResponseSize+1))
	if err != nil {
		return fmt.Errorf("read walletd response: %w", err)
	} else if len(encoded) > maxResponseSize {
		return errors.New("walletd response exceeded size limit")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var problem struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(encoded, &problem) != nil || problem.Error == "" {
			problem.Error = strings.TrimSpace(string(encoded))
		}
		if problem.Error == "" {
			problem.Error = response.Status
		}
		return &APIError{StatusCode: response.StatusCode, Message: problem.Error}
	}
	if output == nil {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	if err := decoder.Decode(output); err != nil {
		return fmt.Errorf("decode walletd response: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("walletd response contains multiple JSON values")
	}
	return nil
}

type Status struct {
	Network             string `json:"network"`
	Genesis             string `json:"genesis"`
	Height              uint64 `json:"height"`
	ScanHeight          uint64 `json:"scanHeight"`
	Synced              bool   `json:"synced"`
	NetworkSynced       bool   `json:"networkSynced"`
	Connections         int    `json:"connections"`
	InboundConnections  int    `json:"inboundConnections"`
	MempoolTransactions int    `json:"mempoolTransactions"`
	Unlocked            bool   `json:"unlocked"`
	Addresses           int    `json:"addresses"`
	UnitAtomic          string `json:"unitAtomic"`
	QDAYHeight          uint64 `json:"qdayHeight"`
	PQDay               bool   `json:"pqDay"`
}

type Amount struct {
	Atomic string `json:"atomic"`
	QDAY   string `json:"qday"`
}

type Balance struct {
	Height      uint64 `json:"height"`
	Synced      bool   `json:"synced"`
	UnitAtomic  string `json:"unitAtomic"`
	Spendable   Amount `json:"spendable"`
	Immature    Amount `json:"immature"`
	PendingIn   Amount `json:"pendingIn"`
	OutputCount int    `json:"outputCount"`
	ShieldUntil uint64 `json:"shieldUntil,omitempty"`
}

type Address struct {
	Index     uint64    `json:"index"`
	Address   string    `json:"address"`
	Reference string    `json:"reference,omitempty"`
	Kind      string    `json:"kind"`
	CreatedAt time.Time `json:"createdAt"`
}

type WithdrawalRequest struct {
	RequestID          string `json:"requestID"`
	Destination        string `json:"destination"`
	AmountAtomic       string `json:"amountAtomic"`
	FeeAtomic          string `json:"feeAtomic,omitempty"`
	ExpectedUnitAtomic string `json:"expectedUnitAtomic"`
}

type Withdrawal struct {
	RequestID     string    `json:"requestID"`
	Kind          string    `json:"kind"`
	TransactionID string    `json:"transactionID"`
	Destination   string    `json:"destination"`
	Amount        Amount    `json:"amount"`
	Fee           Amount    `json:"fee"`
	Basis         string    `json:"basis"`
	Status        string    `json:"status"`
	Confirmations uint64    `json:"confirmations"`
	BlockHeight   *uint64   `json:"blockHeight,omitempty"`
	BlockID       string    `json:"blockID,omitempty"`
	CreatedAt     time.Time `json:"createdAt"`
	LastError     string    `json:"lastError,omitempty"`
}

type SwapKeys struct {
	Classical string `json:"classical"`
	Reserve   string `json:"reserve"`
	Address   string `json:"address"`
}

type SwapKeyView struct {
	SwapID    string    `json:"swapID"`
	Keys      SwapKeys  `json:"keys"`
	CreatedAt time.Time `json:"createdAt"`
}

type RegisterSwapRequest struct {
	SwapID       string   `json:"swapID"`
	Role         string   `json:"role"`
	Counterparty SwapKeys `json:"counterparty"`
	SecretHash   string   `json:"secretHash"`
	RefundHeight uint64   `json:"refundHeight"`
}

type FundSwapRequest struct {
	AmountAtomic       string `json:"amountAtomic"`
	FeeAtomic          string `json:"feeAtomic,omitempty"`
	ExpectedUnitAtomic string `json:"expectedUnitAtomic"`
}

type SpendSwapRequest struct {
	OutputID  string `json:"outputID"`
	FeeAtomic string `json:"feeAtomic,omitempty"`
	Secret    string `json:"secret,omitempty"`
}

type SwapAction struct {
	ActionID       string    `json:"actionID"`
	Kind           string    `json:"kind"`
	OutputID       string    `json:"outputID,omitempty"`
	TransactionID  string    `json:"transactionID"`
	Destination    string    `json:"destination"`
	Amount         Amount    `json:"amount"`
	Fee            Amount    `json:"fee"`
	Basis          string    `json:"basis"`
	Status         string    `json:"status"`
	Confirmations  uint64    `json:"confirmations"`
	BlockHeight    *uint64   `json:"blockHeight,omitempty"`
	BlockID        string    `json:"blockID,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
	LastError      string    `json:"lastError,omitempty"`
	RawTransaction string    `json:"rawTransaction"`
	Submitted      bool      `json:"submitted"`
	BasisHeight    uint64    `json:"basisHeight"`
	BasisID        string    `json:"basisID"`
}

type TransactionPackage struct {
	BasisHeight    uint64   `json:"basisHeight"`
	BasisID        string   `json:"basisID"`
	Transactions   []string `json:"transactions"`
	TransactionIDs []string `json:"transactionIDs"`
}

type BroadcastPackageResult struct {
	Package TransactionPackage `json:"package"`
	Known   bool               `json:"known"`
}

type ValidateSwapFundingRequest struct {
	Package            TransactionPackage `json:"package"`
	TransactionID      string             `json:"transactionID"`
	AmountAtomic       string             `json:"amountAtomic"`
	ExpectedUnitAtomic string             `json:"expectedUnitAtomic"`
	Contract           *SwapContractView  `json:"contract,omitempty"`
}

type SwapContractView struct {
	Recipient    SwapKeys `json:"recipient"`
	Refund       SwapKeys `json:"refund"`
	SecretHash   string   `json:"secretHash"`
	RefundHeight uint64   `json:"refundHeight"`
}

type CompleteSwapClaimTemplateRequest struct {
	Package  TransactionPackage `json:"package"`
	OutputID string             `json:"outputID"`
	Secret   string             `json:"secret"`
}

type SwapOutput struct {
	ID                 string  `json:"id"`
	Value              Amount  `json:"value"`
	Spendable          *Amount `json:"spendable,omitempty"`
	MaturityHeight     uint64  `json:"maturityHeight"`
	FundingTransaction string  `json:"fundingTransaction"`
	FundingHeight      *uint64 `json:"fundingHeight,omitempty"`
	Confirmations      uint64  `json:"confirmations"`
	Status             string  `json:"status"`
	SpendTransaction   string  `json:"spendTransaction,omitempty"`
	SpendHeight        *uint64 `json:"spendHeight,omitempty"`
	RevealedSecret     string  `json:"revealedSecret,omitempty"`
}

type Swap struct {
	SwapID       string       `json:"swapID"`
	Role         string       `json:"role"`
	Status       string       `json:"status"`
	Local        SwapKeys     `json:"local"`
	Recipient    SwapKeys     `json:"recipient"`
	Refund       SwapKeys     `json:"refund"`
	SecretHash   string       `json:"secretHash"`
	RefundHeight uint64       `json:"refundHeight"`
	Address      string       `json:"address"`
	Height       uint64       `json:"height"`
	Outputs      []SwapOutput `json:"outputs"`
	Actions      []SwapAction `json:"actions"`
	CreatedAt    time.Time    `json:"createdAt"`
}

func (c *Client) Health(ctx context.Context) error {
	return c.request(ctx, http.MethodGet, "/healthz", nil, &struct {
		OK bool `json:"ok"`
	}{}, false)
}

func (c *Client) Status(ctx context.Context) (Status, error) {
	var value Status
	err := c.request(ctx, http.MethodGet, "/v1/status", nil, &value, true)
	return value, err
}

func (c *Client) Balance(ctx context.Context) (Balance, error) {
	var value Balance
	err := c.request(ctx, http.MethodGet, "/v1/balance", nil, &value, true)
	return value, err
}

func (c *Client) Unlock(ctx context.Context, password string) error {
	request := struct {
		Password string `json:"password"`
	}{Password: password}
	err := c.request(ctx, http.MethodPost, "/v1/wallet/unlock", request, &struct{}{}, true)
	request.Password = ""
	return err
}

func (c *Client) Lock(ctx context.Context) error {
	return c.request(ctx, http.MethodPost, "/v1/wallet/lock", struct{}{}, &struct{}{}, true)
}

func (c *Client) CreateAddress(ctx context.Context, reference string) (Address, error) {
	var value Address
	err := c.request(ctx, http.MethodPost, "/v1/addresses", struct {
		Reference string `json:"reference"`
	}{Reference: reference}, &value, true)
	return value, err
}

func (c *Client) CreateWithdrawal(ctx context.Context, request WithdrawalRequest) (Withdrawal, error) {
	var value Withdrawal
	err := c.request(ctx, http.MethodPost, "/v1/withdrawals", request, &value, true)
	return value, err
}

func (c *Client) CreateSwapKeys(ctx context.Context, swapID string) (SwapKeyView, error) {
	var value SwapKeyView
	err := c.request(ctx, http.MethodPost, "/v1/swap-keys", struct {
		SwapID string `json:"swapID"`
	}{SwapID: swapID}, &value, true)
	return value, err
}

func (c *Client) RegisterSwap(ctx context.Context, request RegisterSwapRequest) (Swap, error) {
	var value Swap
	err := c.request(ctx, http.MethodPost, "/v1/swaps", request, &value, true)
	return value, err
}

func (c *Client) Swap(ctx context.Context, swapID string) (Swap, error) {
	var value Swap
	err := c.request(ctx, http.MethodGet, "/v1/swaps/"+url.PathEscape(swapID), nil, &value, true)
	return value, err
}

func (c *Client) FundSwap(ctx context.Context, swapID string, request FundSwapRequest) (SwapAction, error) {
	return c.swapAction(ctx, swapID, "fund", request)
}

func (c *Client) PrepareSwapFunding(ctx context.Context, swapID string, request FundSwapRequest) (SwapAction, error) {
	return c.swapAction(ctx, swapID, "prepare-funding", request)
}

func (c *Client) CancelPreparedSwapFunding(ctx context.Context, swapID string) error {
	path := "/v1/swaps/" + url.PathEscape(swapID) + "/prepare-funding"
	return c.request(ctx, http.MethodDelete, path, nil, nil, true)
}

func (c *Client) ValidateTransactionPackage(ctx context.Context, request TransactionPackage) (TransactionPackage, error) {
	var value TransactionPackage
	err := c.request(ctx, http.MethodPost, "/v1/transactions/validate", request, &value, true)
	return value, err
}

func (c *Client) ValidateSwapFundingPackage(ctx context.Context, swapID string, request ValidateSwapFundingRequest) (TransactionPackage, error) {
	var value TransactionPackage
	path := "/v1/swaps/" + url.PathEscape(swapID) + "/validate-funding"
	err := c.request(ctx, http.MethodPost, path, request, &value, true)
	return value, err
}

func (c *Client) BroadcastTransactionPackage(ctx context.Context, request TransactionPackage) (BroadcastPackageResult, error) {
	var value BroadcastPackageResult
	err := c.request(ctx, http.MethodPost, "/v1/transactions/broadcast", request, &value, true)
	return value, err
}

func (c *Client) ClaimSwap(ctx context.Context, swapID string, request SpendSwapRequest) (SwapAction, error) {
	return c.swapAction(ctx, swapID, "claim", request)
}

func (c *Client) PrepareSwapClaimTemplate(ctx context.Context, swapID string, request SpendSwapRequest) (SwapAction, error) {
	return c.swapAction(ctx, swapID, "prepare-claim", request)
}

func (c *Client) CompleteSwapClaimTemplate(ctx context.Context, swapID string, request CompleteSwapClaimTemplateRequest) (TransactionPackage, error) {
	var value TransactionPackage
	path := "/v1/swaps/" + url.PathEscape(swapID) + "/complete-claim"
	err := c.request(ctx, http.MethodPost, path, request, &value, true)
	return value, err
}

func (c *Client) RefundSwap(ctx context.Context, swapID string, request SpendSwapRequest) (SwapAction, error) {
	return c.swapAction(ctx, swapID, "refund", request)
}

func (c *Client) swapAction(ctx context.Context, swapID, action string, request any) (SwapAction, error) {
	var value SwapAction
	path := "/v1/swaps/" + url.PathEscape(swapID) + "/" + action
	err := c.request(ctx, http.MethodPost, path, request, &value, true)
	return value, err
}

func (c *Client) Ready(ctx context.Context) (Status, error) {
	var value Status
	err := c.request(ctx, http.MethodGet, "/readyz", nil, &value, false)
	return value, err
}

func Pagination(limit, offset int) string {
	values := url.Values{}
	values.Set("limit", strconv.Itoa(limit))
	values.Set("offset", strconv.Itoa(offset))
	return values.Encode()
}
