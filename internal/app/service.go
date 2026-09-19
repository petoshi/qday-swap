// Package app coordinates the local QDAY Swap wallet, bundled QDAY node and
// durable swap journal. Browser handlers call this package and never receive a
// wallet seed, private key, qday-walletd token or raw signing capability.
package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/petoshi/qday-swap/internal/bitcoinwallet"
	"github.com/petoshi/qday-swap/internal/keystore"
	"github.com/petoshi/qday-swap/internal/swapstate"
	"github.com/petoshi/qday-swap/internal/walletd"
	"github.com/petoshi/qday-swap/internal/walletroot"
)

type Config struct {
	DataDir         string
	WalletdBinary   string
	QDAYListen      string
	QDAYP2P         string
	QDAYPeers       string
	NetworkManifest string
	Network         string
	RelayURL        string
	BitcoinNetwork  string
	BitcoinPeers    []string
	BitcoinAddPeers []string
}

func DefaultDataDir() string {
	switch runtime.GOOS {
	case "windows":
		return filepath.Join(os.Getenv("APPDATA"), "qday-swap")
	case "darwin":
		return filepath.Join(os.Getenv("HOME"), "Library", "Application Support", "qday-swap")
	default:
		return filepath.Join(os.Getenv("HOME"), ".config", "qday-swap")
	}
}

type qdayClient interface {
	Status(context.Context) (walletd.Status, error)
	Balance(context.Context) (walletd.Balance, error)
	Unlock(context.Context, string) error
	Lock(context.Context) error
	CreateAddress(context.Context, string) (walletd.Address, error)
}

type qdayProcess interface {
	Initialize(context.Context, walletroot.Root, string) error
	Start(context.Context) (*walletd.Client, error)
	Stop() error
}

type processFactory func(dataDir string) (qdayProcess, error)

type bitcoinClient interface {
	Status() (bitcoinwallet.Status, error)
	Balance() (bitcoinwallet.Balance, error)
	Unlock(string) error
	Lock()
	ReceiveAddress() (string, error)
}

type bitcoinProcess interface {
	Initialize(context.Context, walletroot.Root, string, time.Time) error
	Start(context.Context) (bitcoinClient, error)
	Stop() error
}

type bitcoinProcessFactory func(dataDir string) (bitcoinProcess, error)

type bitcoinManagerProcess struct{ manager *bitcoinwallet.Manager }

func (p *bitcoinManagerProcess) Initialize(ctx context.Context, root walletroot.Root, password string, birthday time.Time) error {
	return p.manager.Initialize(ctx, root, password, birthday)
}

func (p *bitcoinManagerProcess) Start(ctx context.Context) (bitcoinClient, error) {
	return p.manager.Start(ctx)
}

func (p *bitcoinManagerProcess) Stop() error { return p.manager.Stop() }

type Service struct {
	config         Config
	ctx            context.Context
	factory        processFactory
	bitcoinFactory bitcoinProcessFactory

	mu             sync.RWMutex
	process        qdayProcess
	client         qdayClient
	bitcoinProcess bitcoinProcess
	bitcoinClient  bitcoinClient
	journal        *swapstate.Journal
	root           *walletroot.Root
	lastError      string
}

type State struct {
	Configured     bool                   `json:"configured"`
	Unlocked       bool                   `json:"unlocked"`
	Network        string                 `json:"network"`
	RelayURL       string                 `json:"relayURL"`
	QDAY           *walletd.Status        `json:"qday,omitempty"`
	Balance        *walletd.Balance       `json:"balance,omitempty"`
	Bitcoin        *bitcoinwallet.Status  `json:"bitcoin,omitempty"`
	BitcoinBalance *bitcoinwallet.Balance `json:"bitcoinBalance,omitempty"`
	Error          string                 `json:"error,omitempty"`
}

type SetupResult struct {
	Phrase string `json:"phrase,omitempty"`
	State  State  `json:"state"`
}

func New(ctx context.Context, config Config) (*Service, error) {
	if config.DataDir == "" {
		config.DataDir = DefaultDataDir()
	}
	if config.WalletdBinary == "" {
		config.WalletdBinary = siblingBinary("qday-walletd")
	}
	if config.Network == "" {
		config.Network = "mainnet"
	}
	if config.BitcoinNetwork == "" {
		config.BitcoinNetwork = "mainnet"
	}
	service := &Service{config: config, ctx: ctx}
	service.factory = func(dataDir string) (qdayProcess, error) {
		return walletd.NewManager(walletd.ManagerConfig{
			Binary: config.WalletdBinary, DataDir: dataDir,
			Listen: config.QDAYListen, P2P: config.QDAYP2P, Peers: config.QDAYPeers,
			NetworkManifest: config.NetworkManifest,
		})
	}
	service.bitcoinFactory = func(dataDir string) (bitcoinProcess, error) {
		manager, err := bitcoinwallet.NewManager(bitcoinwallet.Config{
			DataDir: dataDir, Network: config.BitcoinNetwork,
			ConnectPeers: config.BitcoinPeers, AddPeers: config.BitcoinAddPeers,
		})
		if err != nil {
			return nil, err
		}
		return &bitcoinManagerProcess{manager: manager}, nil
	}
	if service.Configured() {
		if err := service.open(); err != nil {
			service.lastError = err.Error()
		}
	}
	return service, nil
}

func siblingBinary(name string) string {
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	executable, err := os.Executable()
	if err == nil {
		candidate := filepath.Join(filepath.Dir(executable), name)
		if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() {
			return candidate
		}
	}
	return name
}

func (s *Service) Configured() bool {
	info, err := os.Stat(filepath.Join(s.config.DataDir, "master.key"))
	return err == nil && info.Mode().IsRegular()
}

func (s *Service) open() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.openLocked()
}

func (s *Service) openLocked() error {
	if s.process != nil && s.client != nil && s.bitcoinProcess != nil && s.bitcoinClient != nil && s.journal != nil {
		return nil
	}
	process, err := s.factory(filepath.Join(s.config.DataDir, "qday"))
	if err != nil {
		return err
	}
	journal, err := swapstate.Open(filepath.Join(s.config.DataDir, "swaps.db"))
	if err != nil {
		return err
	}
	client, err := process.Start(s.ctx)
	if err != nil {
		_ = journal.Close()
		return err
	}
	bitcoinProcess, err := s.bitcoinFactory(filepath.Join(s.config.DataDir, "bitcoin"))
	if err != nil {
		_ = process.Stop()
		_ = journal.Close()
		return err
	}
	bitcoinClient, err := bitcoinProcess.Start(s.ctx)
	if err != nil {
		_ = bitcoinProcess.Stop()
		_ = process.Stop()
		_ = journal.Close()
		return err
	}
	s.process, s.client, s.journal = process, client, journal
	s.bitcoinProcess, s.bitcoinClient = bitcoinProcess, bitcoinClient
	s.lastError = ""
	return nil
}

func (s *Service) Setup(ctx context.Context, password, phrase string) (SetupResult, error) {
	if s.Configured() {
		return SetupResult{}, errors.New("QDAY Swap is already configured")
	} else if len(password) < 12 {
		return SetupResult{}, errors.New("wallet password must contain at least 12 bytes")
	}
	var root walletroot.Root
	var err error
	generated := phrase == ""
	if generated {
		root, err = walletroot.New()
	} else {
		root, err = walletroot.ParsePhrase(phrase)
	}
	if err != nil {
		return SetupResult{}, err
	}
	canonicalPhrase, err := root.Phrase()
	if err != nil {
		clear(root[:])
		return SetupResult{}, err
	}
	parent := filepath.Dir(s.config.DataDir)
	if err := os.MkdirAll(parent, 0700); err != nil {
		clear(root[:])
		return SetupResult{}, err
	}
	stage, err := os.MkdirTemp(parent, ".qday-swap-setup-")
	if err != nil {
		clear(root[:])
		return SetupResult{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(stage)
		}
	}()
	if err := os.Chmod(stage, 0700); err != nil {
		clear(root[:])
		return SetupResult{}, err
	}
	if err := keystore.Create(filepath.Join(stage, "master.key"), password, root); err != nil {
		clear(root[:])
		return SetupResult{}, err
	}
	process, err := s.factory(filepath.Join(stage, "qday"))
	if err != nil {
		clear(root[:])
		return SetupResult{}, err
	}
	if err := process.Initialize(ctx, root, password); err != nil {
		clear(root[:])
		return SetupResult{}, err
	}
	bitcoinProcess, err := s.bitcoinFactory(filepath.Join(stage, "bitcoin"))
	if err != nil {
		clear(root[:])
		return SetupResult{}, err
	}
	birthday := time.Now().UTC()
	if !generated {
		birthday = time.Unix(0, 0).UTC()
	}
	if err := bitcoinProcess.Initialize(ctx, root, password, birthday); err != nil {
		clear(root[:])
		return SetupResult{}, err
	}
	journal, err := swapstate.Open(filepath.Join(stage, "swaps.db"))
	if err != nil {
		clear(root[:])
		return SetupResult{}, err
	}
	if err := journal.Close(); err != nil {
		clear(root[:])
		return SetupResult{}, err
	}
	if _, err := os.Stat(s.config.DataDir); err == nil {
		clear(root[:])
		return SetupResult{}, errors.New("QDAY Swap data directory appeared during setup")
	} else if !errors.Is(err, os.ErrNotExist) {
		clear(root[:])
		return SetupResult{}, err
	}
	if err := os.Rename(stage, s.config.DataDir); err != nil {
		clear(root[:])
		return SetupResult{}, fmt.Errorf("commit QDAY Swap setup: %w", err)
	}
	committed = true
	_ = syncDirectory(parent)

	s.mu.Lock()
	s.root = &root
	err = s.openLocked()
	if err == nil {
		err = s.client.Unlock(ctx, password)
	}
	if err == nil {
		err = s.bitcoinClient.Unlock(password)
		if err != nil {
			_ = s.client.Lock(ctx)
		}
	}
	if err != nil {
		s.lastError = err.Error()
	} else {
		s.lastError = ""
	}
	s.mu.Unlock()
	result := SetupResult{State: s.State(ctx)}
	if generated {
		result.Phrase = canonicalPhrase
	}
	return result, nil
}

func (s *Service) Unlock(ctx context.Context, password string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.Configured() {
		return errors.New("QDAY Swap is not configured")
	}
	if s.client == nil {
		if err := s.openLocked(); err != nil {
			s.lastError = err.Error()
			return err
		}
	}
	root, err := keystore.Open(filepath.Join(s.config.DataDir, "master.key"), password)
	if err != nil {
		return err
	}
	if err := s.client.Unlock(ctx, password); err != nil {
		clear(root[:])
		return err
	}
	if err := s.bitcoinClient.Unlock(password); err != nil {
		_ = s.client.Lock(ctx)
		clear(root[:])
		return err
	}
	if s.root != nil {
		clear(s.root[:])
	}
	s.root = &root
	s.lastError = ""
	return nil
}

func (s *Service) Lock(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.root != nil {
		clear(s.root[:])
		s.root = nil
	}
	if s.client == nil {
		if s.bitcoinClient != nil {
			s.bitcoinClient.Lock()
		}
		return nil
	}
	err := s.client.Lock(ctx)
	if s.bitcoinClient != nil {
		s.bitcoinClient.Lock()
	}
	return err
}

func (s *Service) RecoveryPhrase() (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.root == nil {
		return "", errors.New("wallet is locked")
	}
	return s.root.Phrase()
}

func (s *Service) QDAYReceiveAddress(ctx context.Context) (walletd.Address, error) {
	s.mu.RLock()
	client, unlocked := s.client, s.root != nil
	s.mu.RUnlock()
	if client == nil {
		return walletd.Address{}, errors.New("QDAY node is unavailable")
	} else if !unlocked {
		return walletd.Address{}, errors.New("wallet is locked")
	}
	return client.CreateAddress(ctx, "qday-swap-main-receive-v1")
}

func (s *Service) BitcoinReceiveAddress() (string, error) {
	s.mu.RLock()
	client, unlocked := s.bitcoinClient, s.root != nil
	s.mu.RUnlock()
	if client == nil {
		return "", errors.New("Bitcoin wallet is unavailable")
	} else if !unlocked {
		return "", errors.New("wallet is locked")
	}
	return client.ReceiveAddress()
}

func (s *Service) State(ctx context.Context) State {
	s.mu.RLock()
	state := State{Configured: s.Configured(), Unlocked: s.root != nil, Network: s.config.Network, RelayURL: s.config.RelayURL, Error: s.lastError}
	client, bitcoinClient := s.client, s.bitcoinClient
	s.mu.RUnlock()
	queryCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if client != nil {
		status, err := client.Status(queryCtx)
		if err != nil {
			state.Error = joinError(state.Error, err)
		} else {
			state.QDAY = &status
			if status.Synced {
				balance, err := client.Balance(queryCtx)
				if err != nil {
					state.Error = joinError(state.Error, err)
				} else {
					state.Balance = &balance
				}
			}
		}
	}
	if bitcoinClient != nil {
		status, err := bitcoinClient.Status()
		if err != nil {
			state.Error = joinError(state.Error, err)
		} else {
			state.Bitcoin = &status
			balance, err := bitcoinClient.Balance()
			if err != nil {
				state.Error = joinError(state.Error, err)
			} else {
				state.BitcoinBalance = &balance
			}
		}
	}
	return state
}

func (s *Service) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.root != nil {
		clear(s.root[:])
		s.root = nil
	}
	var result error
	if s.client != nil {
		lockCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = s.client.Lock(lockCtx)
		cancel()
	}
	if s.bitcoinClient != nil {
		s.bitcoinClient.Lock()
	}
	if s.journal != nil {
		result = s.journal.Close()
	}
	if s.process != nil {
		if err := s.process.Stop(); result == nil {
			result = err
		}
	}
	if s.bitcoinProcess != nil {
		if err := s.bitcoinProcess.Stop(); result == nil {
			result = err
		}
	}
	s.process, s.client, s.bitcoinProcess, s.bitcoinClient, s.journal = nil, nil, nil, nil, nil
	return result
}

func joinError(existing string, err error) string {
	if err == nil {
		return existing
	} else if existing == "" {
		return err.Error()
	}
	return existing + "; " + err.Error()
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
