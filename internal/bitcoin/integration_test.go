//go:build integration

package bitcoin

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
)

const (
	integrationRPCUser = "qday-swap-test"
	integrationRPCPass = "qday-swap-test-password"
)

type integrationNode struct {
	t       *testing.T
	binary  string
	dataDir string
	url     string
	cmd     *exec.Cmd
	log     *os.File
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func startIntegrationNode(t *testing.T) *integrationNode {
	t.Helper()
	binary := os.Getenv("BITCOIND")
	if binary == "" {
		t.Skip("BITCOIND is required")
	}
	port := freePort(t)
	n := &integrationNode{t: t, binary: binary, dataDir: t.TempDir(), url: fmt.Sprintf("http://127.0.0.1:%d", port)}
	log, err := os.OpenFile(filepath.Join(n.dataDir, "bitcoin.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	n.log = log
	n.cmd = exec.Command(binary,
		"-datadir="+n.dataDir, "-regtest=1", "-server=1", "-listen=0", "-dnsseed=0", "-discover=0",
		fmt.Sprintf("-rpcport=%d", port), "-rpcbind=127.0.0.1", "-rpcallowip=127.0.0.1",
		"-rpcuser="+integrationRPCUser, "-rpcpassword="+integrationRPCPass, "-fallbackfee=0.0001",
	)
	n.cmd.Stdout, n.cmd.Stderr = log, log
	if err := n.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	admin, err := NewClient(n.url, "", integrationRPCUser, integrationRPCPass)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := admin.Height(ctx)
		cancel()
		if err == nil {
			t.Cleanup(n.close)
			return n
		}
		time.Sleep(50 * time.Millisecond)
	}
	n.close()
	t.Fatal("bitcoind did not become ready")
	return nil
}

func (n *integrationNode) client(t *testing.T, wallet string) *Client {
	t.Helper()
	client, err := NewClient(n.url, wallet, integrationRPCUser, integrationRPCPass)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func (n *integrationNode) createWallet(t *testing.T, name string, disablePrivateKeys bool) *Client {
	t.Helper()
	admin := n.client(t, "")
	if err := admin.call(context.Background(), false, nil, "createwallet", name, disablePrivateKeys); err != nil {
		t.Fatal(err)
	}
	return n.client(t, name)
}

func (n *integrationNode) mine(t *testing.T, wallet *Client, blocks int) []string {
	t.Helper()
	_, address, err := wallet.NewDestinationScript(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var hashes []string
	if err := wallet.call(context.Background(), true, &hashes, "generatetoaddress", blocks, address); err != nil {
		t.Fatal(err)
	}
	return hashes
}

func (n *integrationNode) close() {
	if n.cmd != nil && n.cmd.Process != nil {
		client, _ := NewClient(n.url, "", integrationRPCUser, integrationRPCPass)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = client.call(ctx, false, nil, "stop")
		cancel()
		done := make(chan error, 1)
		go func() { done <- n.cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = n.cmd.Process.Kill()
			<-done
		}
	}
	if n.log != nil {
		_ = n.log.Close()
	}
}

func TestProductionBitcoinAdapter(t *testing.T) {
	node := startIntegrationNode(t)
	funder := node.createWallet(t, "funder", false)
	recipientWallet := node.createWallet(t, "recipient", false)
	observer := node.createWallet(t, "observer", true)
	node.mine(t, funder, 101)

	recipientKey, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	refundKey, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	secret := sha256.Sum256([]byte("production-adapter-secret"))
	hash := sha256.Sum256(secret[:])
	height, err := funder.Height(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	contract, err := NewContract(SatoshisPerBitcoin, hash, recipientKey.PubKey().SerializeCompressed(), refundKey.PubKey().SerializeCompressed(), height+10)
	if err != nil {
		t.Fatal(err)
	}
	if err := observer.WatchContract(context.Background(), contract); err != nil {
		t.Fatal(err)
	}
	funding, err := funder.PrepareFunding(context.Background(), contract)
	if err != nil {
		t.Fatal(err)
	}
	allowed, reason, err := funder.TestMempoolAccept(context.Background(), funding.Raw)
	if err != nil {
		t.Fatal(err)
	} else if !allowed {
		t.Fatalf("funding rejected: %s", reason)
	}
	if txid, err := funder.Broadcast(context.Background(), funding.Raw); err != nil || txid != funding.TxID {
		t.Fatalf("funding broadcast = %q, %v", txid, err)
	}
	node.mine(t, funder, 1)

	destination, _, err := recipientWallet.NewDestinationScript(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	claim, err := contract.BuildClaim(funding, destination, 2_000, recipientKey, secret)
	if err != nil {
		t.Fatal(err)
	}
	allowed, reason, err = recipientWallet.TestMempoolAccept(context.Background(), claim)
	if err != nil {
		t.Fatal(err)
	} else if !allowed {
		t.Fatalf("claim rejected: %s", reason)
	}
	claimID, err := recipientWallet.Broadcast(context.Background(), claim)
	if err != nil {
		t.Fatal(err)
	}
	raw, confirmations, err := observer.RawWalletTransaction(context.Background(), claimID)
	if err != nil {
		t.Fatal(err)
	} else if confirmations != 0 {
		t.Fatalf("mempool claim confirmations = %d", confirmations)
	}
	revealed, err := contract.ExtractSecret(raw, funding)
	if err != nil {
		t.Fatal(err)
	} else if revealed != secret {
		t.Fatal("observer extracted another secret")
	}
	block := node.mine(t, funder, 1)[0]
	raw, confirmations, err = observer.RawWalletTransaction(context.Background(), claimID)
	if err != nil {
		t.Fatal(err)
	} else if confirmations != 1 {
		t.Fatalf("confirmed claim confirmations = %d", confirmations)
	}

	admin := node.client(t, "")
	if err := admin.call(context.Background(), false, nil, "invalidateblock", block); err != nil {
		t.Fatal(err)
	}
	raw, confirmations, err = observer.RawWalletTransaction(context.Background(), claimID)
	if err != nil {
		t.Fatal(err)
	} else if confirmations != 0 {
		t.Fatalf("reorganized claim confirmations = %d", confirmations)
	}
	if _, err := contract.ExtractSecret(raw, funding); err != nil {
		t.Fatal(err)
	}

	if strings.TrimSpace(raw) == "" {
		t.Fatal("observer lost the raw claim")
	}
}
