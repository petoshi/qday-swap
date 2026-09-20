package bitcoinwallet

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/petoshi/qday-swap/internal/walletroot"
)

const testPhrase = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon art"

func TestInitializeCreatesRecoverableEncryptedBitcoinWallet(t *testing.T) {
	root, err := walletroot.ParsePhrase(testPhrase)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(t.TempDir(), "bitcoin")
	manager, err := NewManager(Config{DataDir: directory, Network: "regtest"})
	if err != nil {
		t.Fatal(err)
	}
	birthday := time.Unix(1_700_000_000, 0)
	if err := manager.Initialize(context.Background(), root, "correct horse battery staple", birthday); err != nil {
		t.Fatal(err)
	}
	loader := wallet.NewLoader(&chaincfg.RegressionNetParams, filepath.Join(directory, "wallet"), false, wallet.DefaultDBTimeout, recoveryWindow)
	loaded, err := loader.OpenExistingWallet([]byte(wallet.InsecurePubPassphrase), false)
	if err != nil {
		t.Fatal(err)
	}
	// btcwallet intentionally stores a 48-hour safety margin so a timestamp
	// near a block boundary cannot hide a transaction.
	if got, want := loaded.Manager.Birthday(), birthday.Add(-48*time.Hour); !got.Equal(want) {
		t.Fatalf("birthday = %v, want %v", got, want)
	}
	if err := loaded.Unlock([]byte("wrong password"), nil); err == nil {
		t.Fatal("wrong password unlocked Bitcoin wallet")
	}
	if err := loaded.Unlock([]byte("correct horse battery staple"), nil); err != nil {
		t.Fatal(err)
	}
	loaded.Lock()
	if err := loader.UnloadWallet(); err != nil {
		t.Fatal(err)
	}
	if err := manager.Initialize(context.Background(), root, "correct horse battery staple", birthday); err == nil {
		t.Fatal("initialized an existing Bitcoin wallet twice")
	}
}

func TestFullHistoryInitializePersistsRecoveryOrigin(t *testing.T) {
	root, err := walletroot.ParsePhrase(testPhrase)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(t.TempDir(), "bitcoin")
	manager, err := NewManager(Config{DataDir: directory, Network: "mainnet"})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Initialize(context.Background(), root, "correct horse battery staple", time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	loader := wallet.NewLoader(&chaincfg.MainNetParams, filepath.Join(directory, "wallet"), false, wallet.DefaultDBTimeout, recoveryWindow)
	loaded, err := loader.OpenExistingWallet([]byte(wallet.InsecurePubPassphrase), false)
	if err != nil {
		t.Fatal(err)
	}
	err = walletdb.View(loaded.Database(), func(tx walletdb.ReadTx) error {
		namespace := tx.ReadBucket([]byte("waddrmgr"))
		stamp, verified, err := loaded.Manager.BirthdayBlock(namespace)
		if err != nil {
			return err
		}
		if !verified {
			t.Fatal("full-history recovery origin is not verified")
		}
		if stamp.Height != mainnetNativeSegWitOrigin.Height ||
			stamp.Hash != mainnetNativeSegWitOrigin.Hash ||
			!stamp.Timestamp.Equal(mainnetNativeSegWitOrigin.Timestamp) {

			t.Fatalf("recovery origin = %v, want %v", stamp, mainnetNativeSegWitOrigin)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := loader.UnloadWallet(); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateLegacyFullHistoryToNativeSegWitOrigin(t *testing.T) {
	root, err := walletroot.ParsePhrase(testPhrase)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(t.TempDir(), "bitcoin")
	manager, err := NewManager(Config{DataDir: directory, Network: "mainnet"})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Initialize(context.Background(), root, "correct horse battery staple", time.Time{}); err != nil {
		t.Fatal(err)
	}
	loader := wallet.NewLoader(&chaincfg.MainNetParams, filepath.Join(directory, "wallet"), false, wallet.DefaultDBTimeout, recoveryWindow)
	loaded, err := loader.OpenExistingWallet([]byte(wallet.InsecurePubPassphrase), false)
	if err != nil {
		t.Fatal(err)
	}
	genesis := waddrmgr.BlockStamp{
		Height:    0,
		Hash:      *chaincfg.MainNetParams.GenesisHash,
		Timestamp: chaincfg.MainNetParams.GenesisBlock.Header.Timestamp,
	}
	err = walletdb.Update(loaded.Database(), func(tx walletdb.ReadWriteTx) error {
		namespace := tx.ReadWriteBucket([]byte("waddrmgr"))
		if err := waddrmgr.DeleteBirthdayBlock(namespace); err != nil {
			return err
		}
		if err := loaded.Manager.SetBirthday(namespace, genesis.Timestamp); err != nil {
			return err
		}
		if err := loaded.Manager.SetSyncedTo(namespace, &genesis); err != nil {
			return err
		}
		return loaded.Manager.SetBirthdayBlock(namespace, genesis, true)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.migrateNativeSegWitRecovery(loaded); err != nil {
		t.Fatal(err)
	}
	if got := loaded.SyncedTo(); got.Height != mainnetNativeSegWitOrigin.Height || got.Hash != mainnetNativeSegWitOrigin.Hash {
		t.Fatalf("synced to = %v, want %v", got, mainnetNativeSegWitOrigin)
	}
	if err := loader.UnloadWallet(); err != nil {
		t.Fatal(err)
	}
}

func TestLaggingPeerCutoffUsesQuorumMedian(t *testing.T) {
	if _, ok := laggingPeerCutoff([]int32{961_639, 967_806}); ok {
		t.Fatal("two peers established a pruning cutoff")
	}
	cutoff, ok := laggingPeerCutoff([]int32{961_639, 967_806, 967_806, 967_806, 967_807})
	if !ok {
		t.Fatal("peer quorum did not establish a pruning cutoff")
	}
	if want := int32(967_806 - peerLagLimit); cutoff != want {
		t.Fatalf("cutoff = %d, want %d", cutoff, want)
	}
	if cutoff <= 961_639 {
		t.Fatalf("cutoff %d retained the stale sync peer", cutoff)
	}
}

func TestFormatBTCExact(t *testing.T) {
	tests := map[int64]string{
		0: "0", 1: "0.00000001", 100_000_000: "1", 123_456_789: "1.23456789",
		-150_000_000: "-1.5",
	}
	for satoshis, expected := range tests {
		if actual := formatBTC(satoshis); actual != expected {
			t.Fatalf("formatBTC(%d) = %q, want %q", satoshis, actual, expected)
		}
	}
}
