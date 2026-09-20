//go:build integration

package bitcoinwallet

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/petoshi/qday-swap/internal/bitcoin"
	"github.com/petoshi/qday-swap/internal/walletroot"
)

const (
	regtestRPCUser = "qday-swap-neutrino"
	regtestRPCPass = "qday-swap-neutrino-test-password"
	// Neutrino processes compact filters and wallet notifications
	// asynchronously. Busy CI runners can need more than 20 seconds after a
	// block is mined even though the node accepted it immediately.
	regtestWalletSyncTimeout = 60 * time.Second
)

type regtestNode struct {
	t       *testing.T
	rpcURL  string
	p2p     string
	cmd     *exec.Cmd
	logFile *os.File
	http    *http.Client
}

func freeRegtestPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func startRegtestNode(t *testing.T) *regtestNode {
	t.Helper()
	binary := os.Getenv("BITCOIND")
	if binary == "" {
		t.Skip("BITCOIND is required")
	}
	rpcPort, p2pPort := freeRegtestPort(t), freeRegtestPort(t)
	directory := t.TempDir()
	logFile, err := os.OpenFile(filepath.Join(directory, "bitcoind.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	node := &regtestNode{
		t: t, rpcURL: fmt.Sprintf("http://127.0.0.1:%d", rpcPort),
		p2p: fmt.Sprintf("127.0.0.1:%d", p2pPort), http: &http.Client{
			// Bitcoin Core may close an idle HTTP/1.1 connection while the
			// Neutrino wallet processes a block. A fresh connection per regtest
			// RPC keeps a stale keep-alive from surfacing as an EOF on the next
			// state-changing request.
			Transport: &http.Transport{DisableKeepAlives: true},
			Timeout:   10 * time.Second,
		},
		logFile: logFile,
	}
	node.cmd = exec.Command(binary,
		"-datadir="+directory, "-regtest=1", "-server=1", "-listen=1", "-discover=0", "-dnsseed=0",
		fmt.Sprintf("-port=%d", p2pPort), "-bind="+node.p2p,
		fmt.Sprintf("-rpcport=%d", rpcPort), "-rpcbind=127.0.0.1", "-rpcallowip=127.0.0.1",
		"-rpcuser="+regtestRPCUser, "-rpcpassword="+regtestRPCPass,
		"-fallbackfee=0.0001", "-txindex=1", "-blockfilterindex=1", "-peerblockfilters=1",
	)
	node.cmd.Stdout, node.cmd.Stderr = logFile, logFile
	if err := node.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var height int32
		if node.call(false, &height, "getblockcount") == nil {
			if err := node.call(false, nil, "createwallet", "miner"); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(node.close)
			return node
		}
		time.Sleep(50 * time.Millisecond)
	}
	node.close()
	t.Fatal("Bitcoin Core regtest did not become ready")
	return nil
}

func (n *regtestNode) call(wallet bool, result any, method string, params ...any) error {
	body, err := json.Marshal(map[string]any{"jsonrpc": "1.0", "id": "qday-swap", "method": method, "params": params})
	if err != nil {
		return err
	}
	endpoint := n.rpcURL
	if wallet {
		endpoint += "/wallet/" + url.PathEscape("miner")
	}
	request, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.SetBasicAuth(regtestRPCUser, regtestRPCPass)
	response, err := n.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		return err
	} else if envelope.Error != nil {
		return fmt.Errorf("Bitcoin RPC %d: %s", envelope.Error.Code, envelope.Error.Message)
	} else if result != nil {
		return json.Unmarshal(envelope.Result, result)
	}
	return nil
}

func (n *regtestNode) miningAddress() string {
	var address string
	if err := n.call(true, &address, "getnewaddress", "", "bech32"); err != nil {
		n.t.Fatal(err)
	}
	return address
}

func (n *regtestNode) mine(blocks int, address string) []string {
	var hashes []string
	if err := n.call(true, &hashes, "generatetoaddress", blocks, address); err != nil {
		n.t.Fatal(err)
	}
	return hashes
}

func (n *regtestNode) mineEmpty(address string) string {
	n.t.Helper()
	var result struct {
		Hash string `json:"hash"`
	}
	if err := n.call(false, &result, "generateblock", address, []string{}); err != nil {
		n.t.Fatal(err)
	}
	return result.Hash
}

func (n *regtestNode) receivedSatoshis(address string, confirmations int) int64 {
	n.t.Helper()
	var received float64
	if err := n.call(true, &received, "getreceivedbyaddress", address, confirmations); err != nil {
		n.t.Fatal(err)
	}
	return int64(math.Round(received * 100_000_000))
}

func (n *regtestNode) sendToAddress(address string, btc float64) string {
	n.t.Helper()
	var transactionID string
	if err := n.call(true, &transactionID, "sendtoaddress", address, btc); err != nil {
		n.t.Fatal(err)
	}
	return transactionID
}

func (n *regtestNode) close() {
	if n.cmd != nil && n.cmd.Process != nil {
		_ = n.call(false, nil, "stop")
		done := make(chan error, 1)
		go func() { done <- n.cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = n.cmd.Process.Kill()
			<-done
		}
	}
	if n.logFile != nil {
		_ = n.logFile.Close()
	}
}

func waitFor(t *testing.T, timeout time.Duration, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func TestNeutrinoSwapFundingClaimRestartAndReorg(t *testing.T) {
	node := startRegtestNode(t)
	root, err := walletroot.ParsePhrase(testPhrase)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(Config{DataDir: t.TempDir(), Network: "regtest", ConnectPeers: []string{node.p2p}})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Initialize(context.Background(), root, "correct horse battery staple", time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client, err := manager.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Stop()
	if err := client.Unlock("correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	receive, err := client.ReceiveAddress()
	if err != nil {
		t.Fatal(err)
	}
	node.mine(1, receive)
	waitFor(t, 30*time.Second, "immature Neutrino coinbase", func() bool {
		status, statusErr := client.Status()
		balance, balanceErr := client.Balance()
		return statusErr == nil && balanceErr == nil && status.WalletHeight >= 1 &&
			balance.Immature.Satoshis != "0"
	})
	if balance, err := client.Balance(); err != nil {
		t.Fatal(err)
	} else if balance.Confirmed.Satoshis != "0" || balance.Pending.Satoshis != "0" {
		t.Fatalf("immature coinbase reported as spendable: %+v", balance)
	}
	node.mine(101, node.miningAddress())
	waitFor(t, 30*time.Second, "Neutrino wallet funding", func() bool {
		status, statusErr := client.Status()
		balance, balanceErr := client.Balance()
		return statusErr == nil && balanceErr == nil && status.WalletHeight >= 102 && balance.Confirmed.Satoshis != "0"
	})

	recipientKey, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x41}, 32))
	refundKey, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x42}, 32))
	secret := sha256.Sum256([]byte("neutrino swap integration secret"))
	secretHash := sha256.Sum256(secret[:])
	status, _ := client.Status()

	// Reproduce an offline counterparty: the contract payment confirms before
	// this wallet imports the P2WSH script. Registering the contract later must
	// scan from the signed agreement height and recover the confirmed payment.
	historicalSecretHash := sha256.Sum256([]byte("funded while counterparty was offline"))
	historicalContract, err := bitcoin.NewContract(500_000, historicalSecretHash, recipientKey.PubKey().SerializeCompressed(), refundKey.PubKey().SerializeCompressed(), uint32(status.WalletHeight+20))
	if err != nil {
		t.Fatal(err)
	}
	historicalWitnessScript, err := historicalContract.WitnessScript()
	if err != nil {
		t.Fatal(err)
	}
	historicalWitnessHash := sha256.Sum256(historicalWitnessScript)
	historicalAddress, err := address.NewAddressWitnessScriptHash(historicalWitnessHash[:], client.wallet.ChainParams())
	if err != nil {
		t.Fatal(err)
	}
	historicalStartHeight := uint32(status.WalletHeight)
	historicalTransactionID := node.sendToAddress(historicalAddress.String(), 0.005)
	node.mine(1, node.miningAddress())
	waitFor(t, regtestWalletSyncTimeout, "wallet tip after offline contract funding", func() bool {
		current, statusErr := client.Status()
		return statusErr == nil && current.WalletHeight > status.WalletHeight
	})
	if _, lookupErr := client.Transaction(historicalTransactionID); !errors.Is(lookupErr, ErrTransactionNotFound) {
		t.Fatalf("unwatched historical contract transaction was unexpectedly known: %v", lookupErr)
	}
	watchContext, watchCancel := context.WithTimeout(ctx, regtestWalletSyncTimeout)
	if err := client.WatchContract(watchContext, historicalContract, historicalStartHeight); err != nil {
		watchCancel()
		t.Fatal(err)
	}
	watchCancel()
	waitFor(t, regtestWalletSyncTimeout, "historical contract transaction recovery", func() bool {
		transaction, lookupErr := client.Transaction(historicalTransactionID)
		return lookupErr == nil && transaction.Confirmations >= 1
	})

	status, _ = client.Status()
	contract, err := bitcoin.NewContract(1_000_000, secretHash, recipientKey.PubKey().SerializeCompressed(), refundKey.PubKey().SerializeCompressed(), uint32(status.WalletHeight+20))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.WatchContract(ctx, contract, uint32(status.WalletHeight)); err != nil {
		t.Fatal(err)
	}
	funding, err := client.PrepareFunding(contract)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Broadcast(funding.Raw); err != nil {
		t.Fatal(err)
	}
	if err := manager.Stop(); err != nil {
		t.Fatal(err)
	}
	client, err = manager.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Unlock("correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Broadcast(funding.Raw); err != nil {
		t.Fatal(err)
	}
	confirmationAddress := node.miningAddress()
	node.mine(1, confirmationAddress)
	waitFor(t, regtestWalletSyncTimeout, "funding confirmation", func() bool {
		transaction, lookupErr := client.Transaction(funding.TxID)
		return lookupErr == nil && transaction.Confirmations >= 1
	})

	destination, err := client.DestinationScript()
	if err != nil {
		t.Fatal(err)
	}
	claimRaw, err := contract.BuildClaim(funding, destination, 2_000, recipientKey, secret)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := client.Broadcast(claimRaw)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, regtestWalletSyncTimeout, "claim observation", func() bool {
		spend, findErr := client.FindSpend(funding)
		return findErr == nil && spend.ID == claim.ID
	})
	claimBlock := node.mine(1, confirmationAddress)[0]
	waitFor(t, regtestWalletSyncTimeout, "claim confirmation", func() bool {
		spend, findErr := client.FindSpend(funding)
		if findErr != nil || spend.Confirmations < 1 {
			return false
		}
		revealed, extractErr := contract.ExtractSecret(spend.Raw, funding)
		return extractErr == nil && revealed == secret
	})
	if err := node.call(false, nil, "invalidateblock", claimBlock); err != nil {
		t.Fatal(err)
	}
	// An SPV peer cannot announce that its tip became shorter. Build a longer
	// replacement branch without the claim so Neutrino observes an actual
	// competing chain and btcwallet rolls the transaction back to unmined.
	node.mineEmpty(confirmationAddress)
	node.mineEmpty(confirmationAddress)
	waitFor(t, regtestWalletSyncTimeout, "reorganized claim in mempool", func() bool {
		spend, findErr := client.FindSpend(funding)
		return findErr == nil && spend.Confirmations == 0
	})
	node.mine(1, confirmationAddress)
	waitFor(t, regtestWalletSyncTimeout, "reconfirmed claim", func() bool {
		spend, findErr := client.FindSpend(funding)
		return findErr == nil && spend.Confirmations >= 1
	})
}

func TestNeutrinoPaymentPartialAndMaximum(t *testing.T) {
	node := startRegtestNode(t)
	root, err := walletroot.ParsePhrase(testPhrase)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(root[:])
	manager, err := NewManager(Config{DataDir: t.TempDir(), Network: "regtest", ConnectPeers: []string{node.p2p}})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Initialize(context.Background(), root, "correct horse battery staple", time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client, err := manager.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Stop()
	if err := client.Unlock("correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	receive, err := client.ReceiveAddress()
	if err != nil {
		t.Fatal(err)
	}
	node.mine(1, receive)
	node.mine(101, node.miningAddress())
	waitFor(t, 30*time.Second, "spendable payment funding", func() bool {
		status, statusErr := client.Status()
		balance, balanceErr := client.Balance()
		return statusErr == nil && balanceErr == nil && status.WalletHeight >= 102 && balance.Confirmed.Satoshis == "5000000000"
	})

	partialRecipient := node.miningAddress()
	partialQuote, err := client.QuotePayment(partialRecipient, 100_000_000, false)
	if err != nil {
		t.Fatal(err)
	} else if partialQuote.Amount.Satoshis != "100000000" || partialQuote.Fee.Satoshis == "0" || partialQuote.Total.Satoshis == "" {
		t.Fatalf("partial payment quote = %#v", partialQuote)
	}
	fee, err := parseSatoshis(partialQuote.Fee.Satoshis)
	if err != nil {
		t.Fatal(err)
	}
	partial, err := client.SendPayment(partialQuote.Destination, 100_000_000, fee)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "partial payment in isolated mempool", func() bool {
		return node.receivedSatoshis(partialRecipient, 0) == 100_000_000
	})
	node.mine(1, node.miningAddress())
	waitFor(t, 20*time.Second, "partial payment confirmation and wallet change", func() bool {
		transaction, transactionErr := client.Transaction(partial.TransactionID)
		balance, balanceErr := client.Balance()
		return transactionErr == nil && balanceErr == nil && transaction.Confirmations >= 1 &&
			balance.Confirmed.Satoshis != "0" && node.receivedSatoshis(partialRecipient, 1) == 100_000_000
	})

	remaining, err := client.Balance()
	if err != nil {
		t.Fatal(err)
	}
	maximumRecipient := node.miningAddress()
	maximumQuote, err := client.QuotePayment(maximumRecipient, 0, true)
	if err != nil {
		t.Fatal(err)
	} else if maximumQuote.Total.Satoshis != remaining.Confirmed.Satoshis {
		t.Fatalf("MAX total %s does not consume confirmed balance %s", maximumQuote.Total.Satoshis, remaining.Confirmed.Satoshis)
	}
	maximumAmount, err := parseSatoshis(maximumQuote.Amount.Satoshis)
	if err != nil {
		t.Fatal(err)
	}
	maximumFee, err := parseSatoshis(maximumQuote.Fee.Satoshis)
	if err != nil {
		t.Fatal(err)
	}
	maximum, err := client.SendPayment(maximumQuote.Destination, maximumAmount, maximumFee)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "MAX payment in isolated mempool", func() bool {
		return node.receivedSatoshis(maximumRecipient, 0) == maximumAmount
	})
	node.mine(1, node.miningAddress())
	waitFor(t, 20*time.Second, "MAX payment confirmation and empty sender", func() bool {
		transaction, transactionErr := client.Transaction(maximum.TransactionID)
		balance, balanceErr := client.Balance()
		return transactionErr == nil && balanceErr == nil && transaction.Confirmations >= 1 &&
			balance.Confirmed.Satoshis == "0" && node.receivedSatoshis(maximumRecipient, 1) == maximumAmount
	})
}
