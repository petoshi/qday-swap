package litecoin

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/wire/v2"
)

type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("litecoin RPC %d: %s", e.Code, e.Message)
}

// Client talks only to the user's local Litecoin Core instance.
type Client struct {
	endpoint string
	wallet   string
	user     string
	password string
	http     *http.Client
	nextID   atomic.Uint64
}

func NewClient(endpoint, wallet, user, password string) (*Client, error) {
	parsed, err := url.Parse(strings.TrimRight(endpoint, "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, errors.New("invalid litecoin RPC endpoint")
	} else if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("litecoin RPC endpoint must use http or https")
	} else if user == "" {
		return nil, errors.New("litecoin RPC user is empty")
	}
	return &Client{
		endpoint: strings.TrimRight(endpoint, "/"), wallet: wallet,
		user: user, password: password, http: &http.Client{Timeout: 15 * time.Second},
	}, nil
}

func (c *Client) rpcURL(wallet bool) string {
	if wallet && c.wallet != "" {
		return c.endpoint + "/wallet/" + url.PathEscape(c.wallet)
	}
	return c.endpoint
}

func (c *Client) call(ctx context.Context, wallet bool, result any, method string, params ...any) error {
	body, err := json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		ID      uint64 `json:"id"`
		Method  string `json:"method"`
		Params  []any  `json:"params"`
	}{JSONRPC: "1.0", ID: c.nextID.Add(1), Method: method, Params: params})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.rpcURL(wallet), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(c.user, c.password)
	response, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("litecoin RPC %s: %w", method, err)
	}
	defer response.Body.Close()
	limited := io.LimitReader(response.Body, 16<<20)
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  *RPCError       `json:"error"`
	}
	if err := json.NewDecoder(limited).Decode(&envelope); err != nil {
		return fmt.Errorf("decode litecoin RPC %s response: %w", method, err)
	} else if envelope.Error != nil {
		return envelope.Error
	} else if response.StatusCode != http.StatusOK {
		return fmt.Errorf("litecoin RPC %s returned HTTP %s", method, response.Status)
	} else if result == nil {
		return nil
	} else if err := json.Unmarshal(envelope.Result, result); err != nil {
		return fmt.Errorf("decode litecoin RPC %s result: %w", method, err)
	}
	return nil
}

type ChainStatus struct {
	Chain                string  `json:"chain"`
	Blocks               uint32  `json:"blocks"`
	Headers              uint32  `json:"headers"`
	VerificationProgress float64 `json:"verificationprogress"`
	InitialBlockDownload bool    `json:"initialblockdownload"`
}

func (c *Client) ChainStatus(ctx context.Context) (ChainStatus, error) {
	var status ChainStatus
	err := c.call(ctx, false, &status, "getblockchaininfo")
	return status, err
}

func (c *Client) Height(ctx context.Context) (uint32, error) {
	var height uint32
	err := c.call(ctx, false, &height, "getblockcount")
	return height, err
}

func (c *Client) NewDestinationScript(ctx context.Context) ([]byte, string, error) {
	var address string
	if err := c.call(ctx, true, &address, "getnewaddress", "", "bech32"); err != nil {
		return nil, "", err
	}
	var info struct {
		Script string `json:"scriptPubKey"`
	}
	if err := c.call(ctx, true, &info, "getaddressinfo", address); err != nil {
		return nil, "", err
	}
	script, err := hex.DecodeString(info.Script)
	if err != nil || len(script) == 0 {
		return nil, "", errors.New("litecoin wallet returned an invalid destination script")
	}
	return script, address, nil
}

// PrepareFunding asks Litecoin Core to select and sign wallet inputs without
// broadcasting. The caller must persist the returned raw transaction first.
func (c *Client) PrepareFunding(ctx context.Context, contract Contract) (Funding, error) {
	pkScript, err := contract.PkScript()
	if err != nil {
		return Funding{}, err
	}
	template := wire.NewMsgTx(2)
	template.AddTxOut(wire.NewTxOut(contract.Amount, pkScript))
	rawTemplate, err := EncodeTransaction(template)
	if err != nil {
		return Funding{}, err
	}
	var funded struct {
		Hex string `json:"hex"`
	}
	if err := c.call(ctx, true, &funded, "fundrawtransaction", rawTemplate); err != nil {
		return Funding{}, err
	}
	var signed struct {
		Hex      string `json:"hex"`
		Complete bool   `json:"complete"`
	}
	if err := c.call(ctx, true, &signed, "signrawtransactionwithwallet", funded.Hex); err != nil {
		return Funding{}, err
	} else if !signed.Complete {
		return Funding{}, errors.New("litecoin wallet did not completely sign the funding transaction")
	}
	return contract.FundingFromRaw(signed.Hex)
}

// TestMempoolAccept validates a transaction against the current Litecoin tip.
func (c *Client) TestMempoolAccept(ctx context.Context, raw string) (bool, string, error) {
	var result []struct {
		Allowed      bool   `json:"allowed"`
		RejectReason string `json:"reject-reason"`
	}
	if err := c.call(ctx, false, &result, "testmempoolaccept", []string{raw}); err != nil {
		return false, "", err
	} else if len(result) != 1 {
		return false, "", fmt.Errorf("litecoin testmempoolaccept returned %d entries", len(result))
	}
	return result[0].Allowed, result[0].RejectReason, nil
}

// Broadcast is idempotent for a transaction already known to Litecoin Core.
func (c *Client) Broadcast(ctx context.Context, raw string) (string, error) {
	tx, err := DecodeTransaction(raw)
	if err != nil {
		return "", err
	}
	expected := tx.TxID()
	var txid string
	if err := c.call(ctx, false, &txid, "sendrawtransaction", raw); err != nil {
		var rpcErr *RPCError
		if errors.As(err, &rpcErr) && (rpcErr.Code == -27 || strings.Contains(strings.ToLower(rpcErr.Message), "already")) {
			return expected, nil
		}
		return "", err
	} else if txid != expected {
		return "", fmt.Errorf("litecoin returned transaction ID %s, expected %s", txid, expected)
	}
	return txid, nil
}

// RawWalletTransaction returns a transaction relevant to the configured
// wallet, including an imported watch-only contract.
func (c *Client) RawWalletTransaction(ctx context.Context, txid string) (string, int, error) {
	var result struct {
		Hex           string `json:"hex"`
		Confirmations int    `json:"confirmations"`
	}
	if err := c.call(ctx, true, &result, "gettransaction", txid, true, true); err != nil {
		return "", 0, err
	} else if result.Hex == "" {
		return "", 0, errors.New("litecoin wallet returned an empty transaction")
	}
	return result.Hex, result.Confirmations, nil
}

// WatchContract imports the exact P2WSH output script without a rescan. It
// supports descriptor and legacy Litecoin Core wallets.
func (c *Client) WatchContract(ctx context.Context, contract Contract) error {
	pkScript, err := contract.PkScript()
	if err != nil {
		return err
	}
	var walletInfo struct {
		Descriptors bool `json:"descriptors"`
	}
	if err := c.call(ctx, true, &walletInfo, "getwalletinfo"); err != nil {
		return err
	}
	scriptHex := hex.EncodeToString(pkScript)
	if !walletInfo.Descriptors {
		if err := c.call(ctx, true, nil, "importaddress", scriptHex, "qday-swap", false, false); err != nil {
			var rpcErr *RPCError
			if errors.As(err, &rpcErr) && strings.Contains(strings.ToLower(rpcErr.Message), "already") {
				return nil
			}
			return err
		}
		return nil
	}
	var descriptor struct {
		Descriptor string `json:"descriptor"`
	}
	if err := c.call(ctx, false, &descriptor, "getdescriptorinfo", "raw("+scriptHex+")"); err != nil {
		return err
	}
	var imported []struct {
		Success  bool      `json:"success"`
		Error    *RPCError `json:"error"`
		Warnings []string  `json:"warnings"`
	}
	request := []map[string]any{{"desc": descriptor.Descriptor, "timestamp": "now", "active": false, "label": "qday-swap"}}
	if err := c.call(ctx, true, &imported, "importdescriptors", request); err != nil {
		return err
	} else if len(imported) != 1 {
		return fmt.Errorf("litecoin importdescriptors returned %d entries", len(imported))
	} else if !imported[0].Success {
		if imported[0].Error != nil && strings.Contains(strings.ToLower(imported[0].Error.Message), "already") {
			return nil
		} else if imported[0].Error != nil {
			return imported[0].Error
		}
		return errors.New("litecoin Core refused the contract watch descriptor")
	}
	return nil
}
