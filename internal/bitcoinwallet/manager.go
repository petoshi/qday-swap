// Package bitcoinwallet embeds a native SegWit btcwallet backed by the
// Neutrino compact-filter light client. It validates Bitcoin proof-of-work
// headers and matching transactions without downloading the full chain.
package bitcoinwallet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/btcsuite/btcwallet/walletdb"
	_ "github.com/btcsuite/btcwallet/walletdb/bdb"
	"github.com/lightninglabs/neutrino"
	"github.com/petoshi/qday-swap/internal/walletroot"
)

const (
	recoveryWindow = 250
	filterCache    = 32 << 20
	blockCache     = 32 << 20
)

type Config struct {
	DataDir      string
	Network      string
	ConnectPeers []string
	AddPeers     []string
}

type Status struct {
	Network       string `json:"network"`
	HeaderHeight  int32  `json:"headerHeight"`
	WalletHeight  int32  `json:"walletHeight"`
	Peers         int    `json:"peers"`
	HeadersSynced bool   `json:"headersSynced"`
	WalletSynced  bool   `json:"walletSynced"`
	Unlocked      bool   `json:"unlocked"`
}

type Amount struct {
	Satoshis string `json:"satoshis"`
	BTC      string `json:"btc"`
}

type Balance struct {
	Confirmed Amount `json:"confirmed"`
	Pending   Amount `json:"pending"`
	Immature  Amount `json:"immature"`
	Total     Amount `json:"total"`
}

type Client struct {
	wallet      *wallet.Wallet
	lightClient *chain.NeutrinoClient
	service     *neutrino.ChainService
	network     string
}

func (c *Client) Status() (Status, error) {
	if c == nil || c.wallet == nil || c.service == nil {
		return Status{}, errors.New("Bitcoin wallet is unavailable")
	}
	best, err := c.service.BestBlock()
	if err != nil {
		return Status{}, fmt.Errorf("read Bitcoin best header: %w", err)
	}
	walletHeight := c.wallet.SyncedTo().Height
	return Status{
		Network: c.network, HeaderHeight: best.Height, WalletHeight: walletHeight,
		Peers: len(c.service.Peers()), HeadersSynced: c.service.IsCurrent(),
		WalletSynced: c.wallet.ChainSynced(), Unlocked: !c.wallet.Locked(),
	}, nil
}

func (c *Client) Balance() (Balance, error) {
	if c == nil || c.wallet == nil {
		return Balance{}, errors.New("Bitcoin wallet is unavailable")
	}
	balances, err := c.wallet.CalculateAccountBalances(waddrmgr.DefaultAccountNum, 1)
	if err != nil {
		return Balance{}, fmt.Errorf("calculate Bitcoin account balance: %w", err)
	}
	pending := balances.Total - balances.Spendable - balances.ImmatureReward
	if pending < 0 {
		return Balance{}, errors.New("Bitcoin wallet returned inconsistent balance totals")
	}
	return Balance{
		Confirmed: amount(int64(balances.Spendable)), Pending: amount(int64(pending)),
		Immature: amount(int64(balances.ImmatureReward)), Total: amount(int64(balances.Total)),
	}, nil
}

func (c *Client) Unlock(password string) error {
	if c == nil || c.wallet == nil {
		return errors.New("Bitcoin wallet is unavailable")
	}
	if password == "" {
		return errors.New("Bitcoin wallet password is required")
	}
	secret := []byte(password)
	defer clear(secret)
	if err := c.wallet.Unlock(secret, nil); err != nil {
		return fmt.Errorf("unlock Bitcoin wallet: %w", err)
	}
	return nil
}

func (c *Client) Lock() {
	if c != nil && c.wallet != nil && !c.wallet.Locked() {
		c.wallet.Lock()
	}
}

func (c *Client) ReceiveAddress() (string, error) {
	if c == nil || c.wallet == nil {
		return "", errors.New("Bitcoin wallet is unavailable")
	} else if c.wallet.Locked() {
		return "", errors.New("wallet is locked")
	}
	addr, err := c.wallet.CurrentAddress(waddrmgr.DefaultAccountNum, waddrmgr.KeyScopeBIP0084)
	if err != nil {
		return "", fmt.Errorf("derive Bitcoin receive address: %w", err)
	}
	return addr.EncodeAddress(), nil
}

type Manager struct {
	config Config
	params *chaincfg.Params

	mu          sync.Mutex
	loader      *wallet.Loader
	wallet      *wallet.Wallet
	lightDB     walletdb.DB
	lightClient *chain.NeutrinoClient
	service     *neutrino.ChainService
}

func NewManager(config Config) (*Manager, error) {
	if config.DataDir == "" {
		return nil, errors.New("Bitcoin wallet data directory is required")
	}
	params, network, err := networkParams(config.Network)
	if err != nil {
		return nil, err
	}
	config.Network = network
	return &Manager{config: config, params: params}, nil
}

func (m *Manager) Initialize(_ context.Context, root walletroot.Root, password string, birthday time.Time) error {
	if len(password) < 12 {
		return errors.New("Bitcoin wallet password must contain at least 12 bytes")
	}
	if birthday.IsZero() {
		birthday = m.params.GenesisBlock.Header.Timestamp
	}
	// btcwallet stores a 48-hour safety margin. Clamp imports so that margin
	// lands on genesis instead of before the network existed.
	minimumBirthday := m.params.GenesisBlock.Header.Timestamp.Add(48 * time.Hour)
	if birthday.Before(minimumBirthday) {
		birthday = minimumBirthday
	}
	walletDir := filepath.Join(m.config.DataDir, "wallet")
	loader := wallet.NewLoader(m.params, walletDir, false, wallet.DefaultDBTimeout, recoveryWindow)
	exists, err := loader.WalletExists()
	if err != nil {
		return fmt.Errorf("check Bitcoin wallet: %w", err)
	} else if exists {
		return errors.New("Bitcoin wallet already exists")
	}
	seed, err := root.BitcoinSeed()
	if err != nil {
		return err
	}
	privatePass := []byte(password)
	_, err = loader.CreateNewWallet([]byte(wallet.InsecurePubPassphrase), privatePass, seed[:], birthday)
	clear(seed[:])
	clear(privatePass)
	if err != nil {
		return fmt.Errorf("create Bitcoin wallet: %w", err)
	}
	if err := loader.UnloadWallet(); err != nil {
		return fmt.Errorf("close new Bitcoin wallet: %w", err)
	}
	return nil
}

func (m *Manager) Start(ctx context.Context) (*Client, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.wallet != nil {
		return &Client{wallet: m.wallet, lightClient: m.lightClient, service: m.service, network: m.config.Network}, nil
	}
	if err := os.MkdirAll(m.config.DataDir, 0700); err != nil {
		return nil, err
	}
	lightDir := filepath.Join(m.config.DataDir, "neutrino")
	if err := os.MkdirAll(lightDir, 0700); err != nil {
		return nil, err
	}
	lightPath := filepath.Join(lightDir, "neutrino.db")
	lightDB, err := openOrCreateDB(lightPath)
	if err != nil {
		return nil, fmt.Errorf("open Bitcoin light-client database: %w", err)
	}
	service, err := neutrino.NewChainService(neutrino.Config{
		DataDir: lightDir, Database: lightDB, ChainParams: *m.params,
		ConnectPeers: m.config.ConnectPeers, AddPeers: m.config.AddPeers,
		FilterCacheSize: filterCache, BlockCacheSize: blockCache,
		PersistToDisk: true, BroadcastTimeout: 30 * time.Second,
	})
	if err != nil {
		_ = lightDB.Close()
		return nil, fmt.Errorf("create Bitcoin light client: %w", err)
	}
	lightClient := chain.NewNeutrinoClient(m.params, service)
	if err := lightClient.Start(ctx); err != nil {
		_ = lightDB.Close()
		return nil, fmt.Errorf("start Bitcoin light client: %w", err)
	}
	loader := wallet.NewLoader(m.params, filepath.Join(m.config.DataDir, "wallet"), false, wallet.DefaultDBTimeout, recoveryWindow)
	loaded, err := loader.OpenExistingWallet([]byte(wallet.InsecurePubPassphrase), false)
	if err != nil {
		lightClient.Stop()
		lightClient.WaitForShutdown()
		_ = service.Stop()
		_ = lightDB.Close()
		return nil, fmt.Errorf("open Bitcoin wallet: %w", err)
	}
	loaded.SynchronizeRPC(lightClient)
	m.loader, m.wallet, m.lightDB = loader, loaded, lightDB
	m.lightClient, m.service = lightClient, service
	return &Client{wallet: loaded, lightClient: lightClient, service: service, network: m.config.Network}, nil
}

func (m *Manager) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result error
	if m.wallet != nil && !m.wallet.Locked() {
		m.wallet.Lock()
	}
	if m.loader != nil {
		if err := m.loader.UnloadWallet(); err != nil {
			result = err
		}
	}
	if m.lightClient != nil {
		m.lightClient.Stop()
		m.lightClient.WaitForShutdown()
	}
	if m.service != nil {
		if err := m.service.Stop(); result == nil {
			result = err
		}
	}
	if m.lightDB != nil {
		if err := m.lightDB.Close(); result == nil {
			result = err
		}
	}
	m.loader, m.wallet, m.lightDB, m.lightClient, m.service = nil, nil, nil, nil, nil
	return result
}

func openOrCreateDB(path string) (walletdb.DB, error) {
	if _, err := os.Stat(path); err == nil {
		return walletdb.Open("bdb", path, true, wallet.DefaultDBTimeout, false)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return walletdb.Create("bdb", path, true, wallet.DefaultDBTimeout, false)
}

func networkParams(name string) (*chaincfg.Params, string, error) {
	switch name {
	case "", "mainnet":
		return &chaincfg.MainNetParams, "mainnet", nil
	case "testnet", "testnet3":
		return &chaincfg.TestNet3Params, "testnet3", nil
	case "regtest":
		return &chaincfg.RegressionNetParams, "regtest", nil
	default:
		return nil, "", fmt.Errorf("unsupported Bitcoin network %q", name)
	}
}

func amount(satoshis int64) Amount {
	return Amount{Satoshis: fmt.Sprintf("%d", satoshis), BTC: formatBTC(satoshis)}
}

func formatBTC(satoshis int64) string {
	sign := ""
	if satoshis < 0 {
		sign = "-"
		satoshis = -satoshis
	}
	whole, fraction := satoshis/100_000_000, satoshis%100_000_000
	if fraction == 0 {
		return fmt.Sprintf("%s%d", sign, whole)
	}
	formatted := fmt.Sprintf("%08d", fraction)
	for len(formatted) > 0 && formatted[len(formatted)-1] == '0' {
		formatted = formatted[:len(formatted)-1]
	}
	return fmt.Sprintf("%s%d.%s", sign, whole, formatted)
}
