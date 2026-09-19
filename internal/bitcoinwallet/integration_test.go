package bitcoinwallet

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/petoshi/qday-swap/internal/walletroot"
)

func TestLiveNeutrinoWallet(t *testing.T) {
	if os.Getenv("QDAY_SWAP_BITCOIN_LIVE") != "1" {
		t.Skip("set QDAY_SWAP_BITCOIN_LIVE=1 to use the Bitcoin P2P network")
	}
	root, err := walletroot.ParsePhrase(testPhrase)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(Config{DataDir: t.TempDir(), Network: "mainnet"})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Initialize(context.Background(), root, "correct horse battery staple", time.Now()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client, err := manager.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := manager.Stop(); err != nil {
			t.Error(err)
		}
	}()
	if err := client.Unlock("correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	address, err := client.ReceiveAddress()
	if err != nil {
		t.Fatal(err)
	} else if !strings.HasPrefix(address, "bc1q") {
		t.Fatalf("receive address = %q", address)
	}
	status, err := client.Status()
	if err != nil {
		t.Fatal(err)
	} else if status.Network != "mainnet" || status.HeaderHeight < 0 {
		t.Fatalf("status = %#v", status)
	}
}
