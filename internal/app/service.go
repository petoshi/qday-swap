// Package app coordinates the local QDAY Swap wallet, bundled QDAY node and
// durable swap journal. Browser handlers call this package and never receive a
// wallet seed, private key, qday-walletd token or raw signing capability.
package app

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/petoshi/qday-swap/internal/bitcoinwallet"
	"github.com/petoshi/qday-swap/internal/keystore"
	"github.com/petoshi/qday-swap/internal/order"
	"github.com/petoshi/qday-swap/internal/relay"
	"github.com/petoshi/qday-swap/internal/relayclient"
	swapprotocol "github.com/petoshi/qday-swap/internal/swap"
	"github.com/petoshi/qday-swap/internal/swapstate"
	"github.com/petoshi/qday-swap/internal/trade"
	"github.com/petoshi/qday-swap/internal/walletd"
	"github.com/petoshi/qday-swap/internal/walletroot"
)

const mainQDAYReceiveReference = "qday-swap-main-receive-v1"

var errInvalidAsyncFunding = errors.New("invalid asynchronous acceptance funding")

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
	// RequireSwapApproval is reserved for protocol tests and recovery tools.
	// The user-facing application treats a signed offer or acceptance as the
	// authorization to execute those exact terms automatically.
	RequireSwapApproval bool
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

type relayAPI interface {
	Status(context.Context) (relayclient.Status, error)
	Price(context.Context) (relay.MarketPrice, error)
	Trades(context.Context, int, int64) (relay.TradeHistory, error)
	Orders(context.Context, relay.Status, string, int, int) (relay.ResultPage, error)
	OrdersByMaker(context.Context, relay.Status, string, int, int) (relay.ResultPage, error)
	Order(context.Context, string) (relay.Record, error)
	Publish(context.Context, order.Signed) (relay.Record, error)
	Cancel(context.Context, string, order.SignedCancellation) (relay.Record, error)
	Accept(context.Context, string, trade.SignedAcceptance) (relay.AcceptanceRecord, error)
	Match(context.Context, string, trade.SignedMatch) (relay.Record, error)
	Message(context.Context, trade.SignedMessage) (trade.SignedMessage, error)
	Poll(context.Context, trade.SignedPoll) (relay.MailboxPage, error)
}

type Service struct {
	config         Config
	ctx            context.Context
	factory        processFactory
	bitcoinFactory bitcoinProcessFactory
	relay          relayAPI

	mu             sync.RWMutex
	relaySyncMu    sync.Mutex
	negotiationMu  sync.Mutex
	engineMu       sync.Mutex
	withdrawalMu   sync.Mutex
	fundsMu        sync.Mutex
	workerCancel   context.CancelFunc
	workerWG       sync.WaitGroup
	lastRelaySync  time.Time
	process        qdayProcess
	client         qdayClient
	bitcoinProcess bitcoinProcess
	bitcoinClient  bitcoinClient
	journal        *swapstate.Journal
	root           *walletroot.Root
	lastError      string
	withdrawals    map[string]withdrawalRecord
}

func unlockQDAYWallet(ctx context.Context, client qdayClient, password string) error {
	if err := client.Unlock(ctx, password); err != nil {
		return err
	}
	if _, err := client.CreateAddress(ctx, mainQDAYReceiveReference); err != nil {
		_ = client.Lock(ctx)
		return fmt.Errorf("initialize QDAY receive address: %w", err)
	}
	return nil
}

type State struct {
	Configured        bool                   `json:"configured"`
	Unlocked          bool                   `json:"unlocked"`
	Network           string                 `json:"network"`
	RelayURL          string                 `json:"relayURL"`
	QDAY              *walletd.Status        `json:"qday,omitempty"`
	Balance           *walletd.Balance       `json:"balance,omitempty"`
	Bitcoin           *bitcoinwallet.Status  `json:"bitcoin,omitempty"`
	BitcoinBalance    *bitcoinwallet.Balance `json:"bitcoinBalance,omitempty"`
	Funds             *Funds                 `json:"funds,omitempty"`
	Relay             *relayclient.Status    `json:"relay,omitempty"`
	PendingTrades     int                    `json:"pendingTrades"`
	IncomingTrades    int                    `json:"incomingTrades"`
	ActiveSwaps       int                    `json:"activeSwaps"`
	IdentityPublicKey string                 `json:"identityPublicKey,omitempty"`
	RelayError        string                 `json:"relayError,omitempty"`
	Error             string                 `json:"error,omitempty"`
}

type CreateOfferRequest struct {
	GiveAsset       string `json:"giveAsset"`
	GiveAmount      string `json:"giveAmount"`
	ReceiveAmount   string `json:"receiveAmount"`
	LifetimeMinutes uint32 `json:"lifetimeMinutes"`
}

type QuoteOfferRequest struct {
	Side          string `json:"side"`
	Quantity      string `json:"quantity"`
	Price         string `json:"price"`
	PriceCurrency string `json:"priceCurrency"`
}

type OfferQuote struct {
	Side           string `json:"side"`
	Quantity       string `json:"quantity"`
	PriceCurrency  string `json:"priceCurrency"`
	EnteredPrice   string `json:"enteredPrice"`
	GiveAsset      string `json:"giveAsset"`
	GiveAmount     string `json:"giveAmount"`
	ReceiveAmount  string `json:"receiveAmount"`
	BTCAmount      string `json:"btcAmount"`
	BTCPerQDAY     string `json:"btcPerQDAY"`
	USDTotal       string `json:"usdTotal,omitempty"`
	USDPerQDAY     string `json:"usdPerQDAY,omitempty"`
	BitcoinUSD     string `json:"bitcoinUSD,omitempty"`
	PriceSource    string `json:"priceSource,omitempty"`
	PriceUpdatedAt string `json:"priceUpdatedAt,omitempty"`
	PriceStale     bool   `json:"priceStale,omitempty"`
}

type Negotiations struct {
	Pending           []swapstate.Negotiation `json:"pending"`
	Incoming          []swapstate.Negotiation `json:"incoming"`
	Swaps             []swapstate.Swap        `json:"swaps"`
	NotificationReads map[string]string       `json:"notificationReads"`
}

// SwapTransaction is the read-only chain evidence shown by the local
// interface. It contains no wallet key material and cannot authorize a spend.
type SwapTransaction struct {
	Kind          string `json:"kind"`
	Party         string `json:"party"`
	Asset         string `json:"asset"`
	TransactionID string `json:"transactionID"`
	Status        string `json:"status"`
	Confirmations uint64 `json:"confirmations"`
	BlockHeight   uint64 `json:"blockHeight,omitempty"`
	AmountAtomic  string `json:"amountAtomic,omitempty"`
	FeeAtomic     string `json:"feeAtomic,omitempty"`
}

type SwapDetails struct {
	Swap          swapstate.Swap    `json:"swap"`
	Transactions  []SwapTransaction `json:"transactions"`
	QDAYHeight    uint64            `json:"qdayHeight,omitempty"`
	BitcoinHeight uint64            `json:"bitcoinHeight,omitempty"`
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
	if config.RelayURL == "" {
		config.RelayURL = "https://dex.pqday.com"
	}
	service := &Service{config: config, ctx: ctx}
	relayClient, err := relayclient.New(config.RelayURL, nil)
	if err != nil {
		return nil, err
	}
	service.relay = relayClient
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
	service.startWorker()
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
		err = unlockQDAYWallet(ctx, s.client, password)
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
	if err := unlockQDAYWallet(ctx, s.client, password); err != nil {
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

func (s *Service) QDAYReceiveAddress(ctx context.Context) (walletd.Address, error) {
	s.mu.RLock()
	client, unlocked := s.client, s.root != nil
	s.mu.RUnlock()
	if client == nil {
		return walletd.Address{}, errors.New("QDAY node is unavailable")
	} else if !unlocked {
		return walletd.Address{}, errors.New("wallet is locked")
	}
	return client.CreateAddress(ctx, mainQDAYReceiveReference)
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

func (s *Service) Orders(ctx context.Context, giveAsset string, page int) (relay.ResultPage, error) {
	if page < 1 {
		page = 1
	}
	if giveAsset != "" && giveAsset != "QDAY" && giveAsset != "BTC" {
		return relay.ResultPage{}, errors.New("give asset must be QDAY or BTC")
	}
	result, err := s.relay.Orders(ctx, relay.StatusOpen, giveAsset, page, 100)
	if err != nil {
		return relay.ResultPage{}, err
	}
	now := time.Now()
	for _, record := range result.Items {
		if err := record.Signed.Verify(now); err != nil {
			return relay.ResultPage{}, fmt.Errorf("relay returned invalid order %s: %w", record.Signed.ID, err)
		}
	}
	return result, nil
}

func (s *Service) MarketPrice(ctx context.Context) (relay.MarketPrice, error) {
	return s.relay.Price(ctx)
}

func (s *Service) MarketTrades(ctx context.Context, limit int, since int64) (relay.TradeHistory, error) {
	if limit < 1 || limit > 2_000 {
		return relay.TradeHistory{}, errors.New("trade history limit must be between 1 and 2000")
	} else if since < 0 {
		return relay.TradeHistory{}, errors.New("trade history start time is invalid")
	}
	return s.relay.Trades(ctx, limit, since)
}

func (s *Service) CreateOffer(ctx context.Context, request CreateOfferRequest) (relay.Record, error) {
	s.fundsMu.Lock()
	defer s.fundsMu.Unlock()
	identity, messagePublic, cleanup, err := s.identities()
	if err != nil {
		return relay.Record{}, err
	}
	defer cleanup()
	if request.GiveAsset != "QDAY" && request.GiveAsset != "BTC" {
		return relay.Record{}, errors.New("give asset must be QDAY or BTC")
	}
	if request.LifetimeMinutes == 0 {
		request.LifetimeMinutes = 24 * 60
	}
	lifetime := time.Duration(request.LifetimeMinutes) * time.Minute
	if lifetime < time.Minute || lifetime > 30*24*time.Hour {
		return relay.Record{}, errors.New("offer lifetime must be between 1 minute and 30 days")
	}
	qdayStatus, qdayBalance, bitcoinBalance, err := s.walletSnapshot(ctx)
	if err != nil {
		return relay.Record{}, err
	}
	funds, err := s.fundsSnapshot(ctx, qdayStatus, qdayBalance, bitcoinBalance)
	if err != nil {
		return relay.Record{}, err
	}
	var give, receive order.Amount
	if request.GiveAsset == "QDAY" {
		give = order.Amount{Asset: "QDAY", Atomic: ""}
		receive = order.Amount{Asset: "BTC", Atomic: ""}
		give.Atomic, err = decimalToAtomic(request.GiveAmount, qdayStatus.UnitAtomic)
		if err == nil {
			receive.Atomic, err = decimalToAtomic(request.ReceiveAmount, "100000000")
		}
		if err == nil {
			err = requireAvailable(give.Atomic, funds.QDAY.AvailableAtomic, "QDAY")
		}
	} else {
		give = order.Amount{Asset: "BTC", Atomic: ""}
		receive = order.Amount{Asset: "QDAY", Atomic: ""}
		give.Atomic, err = decimalToAtomic(request.GiveAmount, "100000000")
		if err == nil {
			receive.Atomic, err = decimalToAtomic(request.ReceiveAmount, qdayStatus.UnitAtomic)
		}
		if err == nil {
			err = requireAvailable(give.Atomic, funds.Bitcoin.AvailableAtomic, "BTC")
		}
	}
	if err != nil {
		return relay.Record{}, err
	}
	now := time.Now().UTC()
	sessionID, err := trade.NewTradeID()
	if err != nil {
		return relay.Record{}, err
	}
	s.mu.RLock()
	var rootCopy walletroot.Root
	haveRoot := s.root != nil
	if haveRoot {
		rootCopy = *s.root
	}
	qdayClient := s.client
	bitcoinClient := s.bitcoinClient
	s.mu.RUnlock()
	if !haveRoot || qdayClient == nil || bitcoinClient == nil {
		clear(rootCopy[:])
		return relay.Record{}, errors.New("local swap wallets are unavailable")
	}
	defer clear(rootCopy[:])
	qdaySwap, ok := qdayClient.(interface {
		CreateSwapKeys(context.Context, string) (walletd.SwapKeyView, error)
	})
	if !ok {
		return relay.Record{}, errors.New("QDAY wallet does not support atomic swaps")
	}
	qdayKeys, err := qdaySwap.CreateSwapKeys(ctx, sessionID)
	if err != nil {
		return relay.Record{}, fmt.Errorf("create maker QDAY swap keys: %w", err)
	}
	bitcoinStatus, err := bitcoinClient.Status()
	if err != nil {
		return relay.Record{}, fmt.Errorf("read Bitcoin swap height: %w", err)
	} else if bitcoinStatus.WalletHeight <= 0 || uint64(bitcoinStatus.WalletHeight) > uint64(^uint32(0)) {
		return relay.Record{}, errors.New("Bitcoin wallet height is outside the supported range")
	}
	bitcoinKey, err := rootCopy.BitcoinSwapKey(sessionID)
	if err != nil {
		return relay.Record{}, err
	}
	payload, err := order.NewAsyncPayload(
		s.config.Network, qdayStatus.UnitAtomic, give, receive, lifetime,
		identity.Public().(ed25519.PublicKey), messagePublic, sessionID,
		order.QDAYKeys{Classical: qdayKeys.Keys.Classical, Reserve: qdayKeys.Keys.Reserve, Address: qdayKeys.Keys.Address},
		hex.EncodeToString(bitcoinKey.PubKey().SerializeCompressed()), qdayStatus.Height, uint32(bitcoinStatus.WalletHeight), now,
	)
	if err != nil {
		return relay.Record{}, err
	}
	signed, err := order.Sign(payload, identity, now)
	if err != nil {
		return relay.Record{}, err
	}
	record, err := s.relay.Publish(ctx, signed)
	if err != nil {
		return relay.Record{}, err
	}
	if err := record.Signed.Verify(now); err != nil || record.Signed.ID != signed.ID {
		if err == nil {
			err = errors.New("relay returned a different order")
		}
		return relay.Record{}, err
	}
	return record, nil
}

func (s *Service) QuoteOffer(ctx context.Context, request QuoteOfferRequest) (OfferQuote, error) {
	s.fundsMu.Lock()
	defer s.fundsMu.Unlock()
	side := strings.ToLower(strings.TrimSpace(request.Side))
	if side != "buy" && side != "sell" {
		return OfferQuote{}, errors.New("choose BUY QDAY or SELL QDAY")
	}
	currency := strings.ToUpper(strings.TrimSpace(request.PriceCurrency))
	if currency != "USD" && currency != "BTC" {
		return OfferQuote{}, errors.New("price currency must be USD or BTC")
	}
	enteredPrice, err := positiveDecimal(request.Price, "price")
	if err != nil {
		return OfferQuote{}, err
	}
	qdayStatus, qdayBalance, bitcoinBalance, err := s.walletSnapshot(ctx)
	if err != nil {
		return OfferQuote{}, err
	}
	funds, err := s.fundsSnapshot(ctx, qdayStatus, qdayBalance, bitcoinBalance)
	if err != nil {
		return OfferQuote{}, err
	}
	qdayAtomic, err := decimalToAtomic(request.Quantity, qdayStatus.UnitAtomic)
	if err != nil {
		return OfferQuote{}, fmt.Errorf("QDAY quantity: %w", err)
	} else if len(qdayAtomic) > 64 {
		return OfferQuote{}, errors.New("QDAY quantity is too large")
	}
	qdayAtoms, _ := new(big.Int).SetString(qdayAtomic, 10)
	qdayUnit, _ := new(big.Int).SetString(qdayStatus.UnitAtomic, 10)
	quantity := new(big.Rat).SetFrac(qdayAtoms, qdayUnit)

	var market relay.MarketPrice
	var bitcoinUSD *big.Rat
	if currency == "USD" {
		market, err = s.relay.Price(ctx)
		if err != nil {
			return OfferQuote{}, errors.New("BTC/USD price is temporarily unavailable; enter the price in BTC")
		}
		bitcoinUSD, err = positiveDecimal(market.USD, "BTC/USD price")
		if err != nil {
			return OfferQuote{}, errors.New("price relay returned an invalid BTC/USD price")
		}
	} else if fetched, priceErr := s.relay.Price(ctx); priceErr == nil {
		if parsed, parseErr := positiveDecimal(fetched.USD, "BTC/USD price"); parseErr == nil {
			market, bitcoinUSD = fetched, parsed
		}
	}

	totalBTC := new(big.Rat).Mul(quantity, enteredPrice)
	if currency == "USD" {
		totalBTC.Quo(totalBTC, bitcoinUSD)
	}
	totalSatoshis := roundPositiveRat(new(big.Rat).Mul(totalBTC, big.NewRat(100_000_000, 1)))
	if totalSatoshis.Sign() <= 0 {
		return OfferQuote{}, errors.New("total is below one satoshi; increase the price or quantity")
	} else if totalSatoshis.Cmp(big.NewInt(order.MinimumBitcoinSwapSatoshis)) < 0 {
		return OfferQuote{}, fmt.Errorf("Bitcoin total must be at least %d satoshis (0.0001 BTC)", order.MinimumBitcoinSwapSatoshis)
	}
	maximumBitcoinSatoshis := big.NewInt(2_100_000_000_000_000)
	if totalSatoshis.Cmp(maximumBitcoinSatoshis) > 0 {
		return OfferQuote{}, errors.New("Bitcoin total exceeds the maximum supply")
	}
	satoshis := totalSatoshis.String()
	if side == "buy" {
		err = requireAvailable(satoshis, funds.Bitcoin.AvailableAtomic, "BTC")
	} else {
		err = requireAvailable(qdayAtomic, funds.QDAY.AvailableAtomic, "QDAY")
	}
	if err != nil {
		return OfferQuote{}, err
	}

	btcAmount := atomicDecimal(satoshis, "100000000")
	quantityText := atomicDecimal(qdayAtomic, qdayStatus.UnitAtomic)
	effectiveBTCPerQDAY := new(big.Rat).SetFrac(
		new(big.Int).Mul(totalSatoshis, qdayUnit),
		new(big.Int).Mul(qdayAtoms, big.NewInt(100_000_000)),
	)
	quote := OfferQuote{
		Side: side, Quantity: quantityText, PriceCurrency: currency,
		EnteredPrice: strings.TrimSpace(request.Price), BTCAmount: btcAmount,
		BTCPerQDAY: decimalRat(effectiveBTCPerQDAY, 16),
	}
	if side == "buy" {
		quote.GiveAsset, quote.GiveAmount, quote.ReceiveAmount = "BTC", btcAmount, quantityText
	} else {
		quote.GiveAsset, quote.GiveAmount, quote.ReceiveAmount = "QDAY", quantityText, btcAmount
	}
	if bitcoinUSD != nil {
		usdTotal := new(big.Rat).Mul(new(big.Rat).SetFrac(totalSatoshis, big.NewInt(100_000_000)), bitcoinUSD)
		usdPerQDAY := new(big.Rat).Mul(effectiveBTCPerQDAY, bitcoinUSD)
		quote.USDTotal = usdTotal.FloatString(2)
		quote.USDPerQDAY = decimalRat(usdPerQDAY, 8)
		quote.BitcoinUSD = market.USD
		quote.PriceSource = market.Source
		quote.PriceUpdatedAt = market.UpdatedAt
		quote.PriceStale = market.Stale
	}
	return quote, nil
}

func (s *Service) CancelOffer(ctx context.Context, orderID string) (relay.Record, error) {
	s.fundsMu.Lock()
	defer s.fundsMu.Unlock()
	identity, _, cleanup, err := s.identities()
	if err != nil {
		return relay.Record{}, err
	}
	defer cleanup()
	now := time.Now().UTC()
	record, err := s.relay.Order(ctx, orderID)
	if err != nil {
		return relay.Record{}, err
	}
	if err := record.Signed.Verify(now); err != nil {
		return relay.Record{}, err
	}
	if record.Signed.Order.MakerPublicKey != hex.EncodeToString(identity.Public().(ed25519.PublicKey)) {
		return relay.Record{}, errors.New("this wallet does not own the offer")
	}
	cancellation, err := order.NewCancellation(record.Signed, identity, now)
	if err != nil {
		return relay.Record{}, err
	}
	return s.relay.Cancel(ctx, orderID, cancellation)
}

func (s *Service) AcceptOffer(ctx context.Context, orderID string) (swapstate.Negotiation, error) {
	s.negotiationMu.Lock()
	defer s.negotiationMu.Unlock()
	s.fundsMu.Lock()
	defer s.fundsMu.Unlock()
	identity, messagePublic, cleanup, err := s.identities()
	if err != nil {
		return swapstate.Negotiation{}, err
	}
	defer cleanup()
	now := time.Now().UTC()
	record, err := s.relay.Order(ctx, orderID)
	if err != nil {
		return swapstate.Negotiation{}, err
	}
	if record.Status != relay.StatusOpen {
		return swapstate.Negotiation{}, errors.New("offer is no longer open")
	} else if err := record.Signed.Verify(now); err != nil {
		return swapstate.Negotiation{}, err
	}
	qdayStatus, qdayBalance, bitcoinBalance, err := s.walletSnapshot(ctx)
	if err != nil {
		return swapstate.Negotiation{}, err
	} else if record.Signed.Order.QDAYUnitAtomic != qdayStatus.UnitAtomic {
		return swapstate.Negotiation{}, errors.New("offer uses a different QDAY consensus unit")
	}
	funds, err := s.fundsSnapshot(ctx, qdayStatus, qdayBalance, bitcoinBalance)
	if err != nil {
		return swapstate.Negotiation{}, err
	}
	if record.Signed.Order.Give.Asset == "QDAY" {
		err = requireAvailable(record.Signed.Order.Receive.Atomic, funds.Bitcoin.AvailableAtomic, "BTC")
	} else {
		err = requireAvailable(record.Signed.Order.Receive.Atomic, funds.QDAY.AvailableAtomic, "QDAY")
	}
	if err != nil {
		return swapstate.Negotiation{}, err
	}
	s.mu.RLock()
	journal := s.journal
	s.mu.RUnlock()
	if journal == nil {
		return swapstate.Negotiation{}, errors.New("swap journal is unavailable")
	}
	pendingRecords, err := journal.PendingAcceptances()
	if err != nil {
		return swapstate.Negotiation{}, err
	}
	for _, pending := range pendingRecords {
		if pending.Order.ID != record.Signed.ID {
			continue
		}
		if pending.Acceptance.Acceptance.ExpiresAt <= now.Unix() {
			s.releasePendingFunding(ctx, pending)
			if err := journal.RemovePendingAcceptance(pending.Acceptance.ID); err != nil {
				return swapstate.Negotiation{}, err
			}
			continue
		}
		if err := pending.Acceptance.Verify(record.Signed, now); err != nil {
			return swapstate.Negotiation{}, fmt.Errorf("verify saved acceptance: %w", err)
		}
		// A timeout does not mean the relay rejected the acceptance. Resend the
		// exact signed record so one click can be retried without opening a second trade.
		if _, err := s.relay.Accept(ctx, orderID, pending.Acceptance); err != nil {
			return pending, err
		}
		return pending, nil
	}
	if record.Signed.Order.Version != order.AsyncProtocolVersion {
		return swapstate.Negotiation{}, errors.New("legacy online-only offers are no longer accepted")
	}
	wallets, _, err := s.engineWalletSnapshot()
	if err != nil {
		return swapstate.Negotiation{}, err
	}
	defer wallets.clear()
	qdayAsync, ok := wallets.qday.(qdayAsyncSwapClient)
	if !ok {
		return swapstate.Negotiation{}, errors.New("QDAY wallet does not support prepared swap funding")
	}
	bitcoinAsync, ok := wallets.bitcoin.(bitcoinAsyncSwapClient)
	if !ok {
		return swapstate.Negotiation{}, errors.New("Bitcoin wallet does not support prepared swap funding")
	}
	tradeID, err := trade.NewTradeID()
	if err != nil {
		return swapstate.Negotiation{}, err
	}
	qdayKeys, err := qdayAsync.CreateSwapKeys(ctx, tradeID)
	if err != nil {
		return swapstate.Negotiation{}, fmt.Errorf("create taker QDAY swap keys: %w", err)
	}
	bitcoinStatus, err := bitcoinAsync.Status()
	if err != nil {
		return swapstate.Negotiation{}, fmt.Errorf("read Bitcoin swap height: %w", err)
	} else if bitcoinStatus.WalletHeight <= 0 || uint64(bitcoinStatus.WalletHeight) > uint64(^uint32(0)) {
		return swapstate.Negotiation{}, errors.New("Bitcoin wallet height is outside the supported range")
	}
	bitcoinKey, err := wallets.root.BitcoinSwapKey(tradeID)
	if err != nil {
		return swapstate.Negotiation{}, err
	}
	secret, secretHash, err := wallets.root.SwapSecret(tradeID)
	clear(secret[:])
	if err != nil {
		return swapstate.Negotiation{}, err
	}
	qdayRefundHeight, bitcoinRefundHeight, err := trade.AsyncRefundHeights(record.Signed.Order, now.Unix(), qdayStatus.Height, uint32(bitcoinStatus.WalletHeight))
	if err != nil {
		return swapstate.Negotiation{}, err
	}
	unsignedAcceptance := trade.AcceptancePayload{
		Version: trade.AsyncProtocolVersion, Network: record.Signed.Order.Network, OrderID: record.Signed.ID, TradeID: tradeID,
		TakerPublicKey: hex.EncodeToString(identity.Public().(ed25519.PublicKey)), TakerMessageKey: hex.EncodeToString(messagePublic[:]),
		CreatedAt: now.Unix(), ExpiresAt: record.Signed.Order.ExpiresAt,
		TakerQDAY:       order.QDAYKeys{Classical: qdayKeys.Keys.Classical, Reserve: qdayKeys.Keys.Reserve, Address: qdayKeys.Keys.Address},
		TakerBitcoinKey: hex.EncodeToString(bitcoinKey.PubKey().SerializeCompressed()),
		TakerQDAYHeight: qdayStatus.Height, TakerBTCHeight: uint32(bitcoinStatus.WalletHeight),
		SecretHash: hex.EncodeToString(secretHash[:]), QDAYRefundHeight: qdayRefundHeight, BTCRefundHeight: bitcoinRefundHeight,
	}
	agreement, err := swapprotocol.BuildAsyncTerms(record.Signed, unsignedAcceptance)
	if err != nil {
		return swapstate.Negotiation{}, err
	}
	localGivesQDAY := agreement.MakerGives == "BTC"
	qdayRole := "recipient"
	if localGivesQDAY {
		qdayRole = "refund"
	}
	if _, err := qdayAsync.RegisterSwap(ctx, walletd.RegisterSwapRequest{
		SwapID: tradeID, Role: qdayRole,
		Counterparty: walletd.SwapKeys{Classical: agreement.Maker.QDAY.Classical, Reserve: agreement.Maker.QDAY.Reserve, Address: agreement.Maker.QDAY.Address},
		SecretHash:   agreement.SecretHash, RefundHeight: agreement.QDAYRefundHeight,
	}); err != nil {
		return swapstate.Negotiation{}, fmt.Errorf("register asynchronous QDAY contract: %w", err)
	}
	bitcoinContract, err := bitcoinContract(agreement)
	if err != nil {
		return swapstate.Negotiation{}, err
	} else if err := bitcoinAsync.WatchContract(bitcoinContract); err != nil {
		return swapstate.Negotiation{}, fmt.Errorf("watch asynchronous Bitcoin contract: %w", err)
	}
	var funding trade.FundingPackage
	preparedQDAY := false
	preparedBitcoin := ""
	committed := false
	defer func() {
		if committed {
			return
		}
		if preparedQDAY {
			_ = qdayAsync.CancelPreparedSwapFunding(context.Background(), tradeID)
		}
		if preparedBitcoin != "" {
			_ = bitcoinAsync.ReleaseFunding(preparedBitcoin)
		}
	}()
	if record.Signed.Order.Receive.Asset == "QDAY" {
		action, err := qdayAsync.PrepareSwapFunding(ctx, tradeID, walletd.FundSwapRequest{
			AmountAtomic: agreement.QDAYAmountAtomic, ExpectedUnitAtomic: agreement.QDAYUnitAtomic,
		})
		if err != nil {
			return swapstate.Negotiation{}, fmt.Errorf("prepare QDAY acceptance funding: %w", err)
		}
		preparedQDAY = true
		funding = trade.FundingPackage{
			Asset: "QDAY", TransactionID: action.TransactionID,
			RawTransactions: []string{action.RawTransaction}, BasisHeight: action.BasisHeight, BasisID: action.BasisID,
		}
	} else {
		prepared, err := bitcoinAsync.PrepareFunding(bitcoinContract)
		if err != nil {
			return swapstate.Negotiation{}, fmt.Errorf("prepare Bitcoin acceptance funding: %w", err)
		}
		preparedBitcoin = prepared.Raw
		funding = trade.FundingPackage{Asset: "BTC", TransactionID: prepared.TxID, RawTransactions: []string{prepared.Raw}}
	}
	acceptance, err := trade.NewAsyncAcceptance(
		record.Signed, tradeID, unsignedAcceptance.TakerQDAY, unsignedAcceptance.TakerBitcoinKey,
		unsignedAcceptance.TakerQDAYHeight, unsignedAcceptance.TakerBTCHeight, unsignedAcceptance.SecretHash,
		qdayRefundHeight, bitcoinRefundHeight, funding, identity, messagePublic, now,
	)
	if err != nil {
		return swapstate.Negotiation{}, err
	}
	pending, _, err := journal.SavePendingAcceptance(record.Signed, acceptance, now)
	if err != nil {
		return swapstate.Negotiation{}, err
	}
	committed = true
	if _, err := s.relay.Accept(ctx, orderID, acceptance); err != nil {
		return pending, err
	}
	return pending, nil
}

func (s *Service) MatchAcceptance(ctx context.Context, acceptanceID string) (swapstate.Swap, error) {
	s.negotiationMu.Lock()
	defer s.negotiationMu.Unlock()
	identity, _, cleanup, err := s.identities()
	if err != nil {
		return swapstate.Swap{}, err
	}
	defer cleanup()
	s.mu.RLock()
	journal := s.journal
	s.mu.RUnlock()
	if journal == nil {
		return swapstate.Swap{}, errors.New("swap journal is unavailable")
	}
	return s.matchAcceptance(ctx, journal, identity, acceptanceID)
}

func (s *Service) matchAcceptance(ctx context.Context, journal *swapstate.Journal, identity ed25519.PrivateKey, acceptanceID string) (swapstate.Swap, error) {
	negotiation, err := journal.IncomingAcceptance(acceptanceID)
	if err != nil {
		return swapstate.Swap{}, err
	}
	now := time.Now().UTC()
	record, err := journal.Swap(negotiation.Acceptance.Acceptance.TradeID)
	if errors.Is(err, swapstate.ErrNotFound) {
		match, matchErr := trade.NewMatch(negotiation.Order, negotiation.Acceptance, identity, now)
		if matchErr != nil {
			return swapstate.Swap{}, matchErr
		}
		if negotiation.Acceptance.Acceptance.Version == trade.AsyncProtocolVersion {
			wallets, _, walletErr := s.engineWalletSnapshot()
			if walletErr != nil {
				return swapstate.Swap{}, walletErr
			}
			candidate := swapstate.Swap{
				Version: trade.AsyncProtocolVersion, ID: negotiation.Acceptance.Acceptance.TradeID,
				Role: swapstate.RoleMaker, Phase: swapstate.PhaseMatched,
				Order: negotiation.Order, Acceptance: negotiation.Acceptance, Match: match,
			}
			agreement, agreementErr := swapprotocol.BuildAsyncAgreement(candidate.Order, candidate.Acceptance)
			if agreementErr == nil {
				agreementErr = validateAsyncTakerFunding(ctx, wallets, candidate, agreement)
			}
			wallets.clear()
			if agreementErr != nil {
				return swapstate.Swap{}, fmt.Errorf("%w: %v", errInvalidAsyncFunding, agreementErr)
			}
		}
		record, _, err = journal.Create(swapstate.RoleMaker, negotiation.Order, negotiation.Acceptance, match, now)
	} else if err == nil {
		if record.Role != swapstate.RoleMaker || record.Order.ID != negotiation.Order.ID || record.Acceptance.ID != negotiation.Acceptance.ID {
			return swapstate.Swap{}, errors.New("trade ID is already bound to another local swap")
		}
	}
	if err != nil {
		return swapstate.Swap{}, err
	}
	if record.Version == trade.AsyncProtocolVersion && (record.AgreementJSON == "" || record.Phase == swapstate.PhaseMatched) {
		wallets, _, walletErr := s.engineWalletSnapshot()
		if walletErr != nil {
			return record, walletErr
		}
		record, err = s.initializeAsyncSwap(ctx, wallets, journal, record)
		wallets.clear()
		if err != nil {
			return record, fmt.Errorf("validate asynchronous acceptance funding: %w", err)
		}
	}
	// The exact signed match is persisted before contacting the relay. A retry
	// after a timeout must resend those bytes instead of creating another match.
	if _, err := s.relay.Match(ctx, negotiation.Order.ID, record.Match); err != nil {
		return record, err
	}
	if err := journal.RemoveIncomingAcceptance(acceptanceID); err != nil {
		return record, err
	}
	return record, nil
}

func (s *Service) Negotiations() (Negotiations, error) {
	s.mu.RLock()
	journal := s.journal
	s.mu.RUnlock()
	if journal == nil {
		return Negotiations{}, errors.New("swap journal is unavailable")
	}
	pending, err := journal.PendingAcceptances()
	if err != nil {
		return Negotiations{}, err
	}
	incoming, err := journal.IncomingAcceptances()
	if err != nil {
		return Negotiations{}, err
	}
	swaps, err := journal.List()
	if err != nil {
		return Negotiations{}, err
	}
	reads, err := journal.NotificationReads()
	return Negotiations{Pending: pending, Incoming: incoming, Swaps: swaps, NotificationReads: reads}, err
}

func (s *Service) AcknowledgeSwapNotification(swapID, milestone string) error {
	s.mu.RLock()
	journal := s.journal
	s.mu.RUnlock()
	if journal == nil {
		return errors.New("swap journal is unavailable")
	}
	return journal.SetNotificationRead(swapID, milestone)
}

func (s *Service) identities() (ed25519.PrivateKey, [32]byte, func(), error) {
	s.mu.RLock()
	if s.root == nil {
		s.mu.RUnlock()
		return nil, [32]byte{}, func() {}, errors.New("wallet is locked")
	}
	root := *s.root
	s.mu.RUnlock()
	identity, err := root.OrderIdentity()
	if err != nil {
		clear(root[:])
		return nil, [32]byte{}, func() {}, err
	}
	messagePrivate, messagePublic, err := root.MessageIdentity()
	clear(root[:])
	if err != nil {
		clear(identity)
		return nil, [32]byte{}, func() {}, err
	}
	cleanup := func() {
		clear(identity)
		clear(messagePrivate[:])
	}
	return identity, messagePublic, cleanup, nil
}

func (s *Service) walletSnapshot(ctx context.Context) (walletd.Status, walletd.Balance, bitcoinwallet.Balance, error) {
	s.mu.RLock()
	qdayClient, bitcoinClient := s.client, s.bitcoinClient
	s.mu.RUnlock()
	if qdayClient == nil || bitcoinClient == nil {
		return walletd.Status{}, walletd.Balance{}, bitcoinwallet.Balance{}, errors.New("local wallets are unavailable")
	}
	status, err := qdayClient.Status(ctx)
	if err != nil {
		return walletd.Status{}, walletd.Balance{}, bitcoinwallet.Balance{}, err
	} else if !status.Synced {
		return walletd.Status{}, walletd.Balance{}, bitcoinwallet.Balance{}, errors.New("QDAY wallet is still synchronizing")
	}
	qdayBalance, err := qdayClient.Balance(ctx)
	if err != nil {
		return walletd.Status{}, walletd.Balance{}, bitcoinwallet.Balance{}, err
	}
	bitcoinStatus, err := bitcoinClient.Status()
	if err != nil {
		return walletd.Status{}, walletd.Balance{}, bitcoinwallet.Balance{}, err
	} else if !bitcoinStatus.WalletSynced {
		return walletd.Status{}, walletd.Balance{}, bitcoinwallet.Balance{}, errors.New("Bitcoin wallet is still synchronizing")
	}
	bitcoinBalance, err := bitcoinClient.Balance()
	return status, qdayBalance, bitcoinBalance, err
}

func decimalToAtomic(value, unit string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || strings.HasPrefix(value, "+") || strings.HasPrefix(value, "-") {
		return "", errors.New("amount must be a positive decimal number")
	}
	decimals := len(unit) - 1
	if decimals < 0 || unit != "1"+strings.Repeat("0", decimals) {
		return "", errors.New("asset unit is not a decimal power")
	}
	parts := strings.Split(value, ".")
	if len(parts) > 2 || parts[0] == "" || (len(parts) == 2 && parts[1] == "") {
		return "", errors.New("amount must be a plain decimal number")
	}
	whole, fraction := parts[0], ""
	if len(parts) == 2 {
		fraction = parts[1]
	}
	if len(fraction) > decimals {
		return "", fmt.Errorf("amount has more than %d decimal places", decimals)
	}
	for _, part := range []string{whole, fraction} {
		for _, character := range part {
			if character < '0' || character > '9' {
				return "", errors.New("amount must be a plain decimal number")
			}
		}
	}
	whole = strings.TrimLeft(whole, "0")
	if whole == "" {
		whole = "0"
	}
	encoded := strings.TrimLeft(whole+fraction+strings.Repeat("0", decimals-len(fraction)), "0")
	if encoded == "" {
		return "", errors.New("amount must be greater than zero")
	}
	return encoded, nil
}

func positiveDecimal(value, name string) (*big.Rat, error) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 64 || strings.HasPrefix(value, "+") || strings.HasPrefix(value, "-") || strings.ContainsAny(value, "eE") {
		return nil, fmt.Errorf("%s must be a positive plain decimal", name)
	}
	dot, digit := false, false
	for _, character := range value {
		switch {
		case character >= '0' && character <= '9':
			digit = true
		case character == '.' && !dot:
			dot = true
		default:
			return nil, fmt.Errorf("%s must be a positive plain decimal", name)
		}
	}
	result, ok := new(big.Rat).SetString(value)
	if !digit || !ok || result.Sign() <= 0 {
		return nil, fmt.Errorf("%s must be a positive plain decimal", name)
	}
	return result, nil
}

func roundPositiveRat(value *big.Rat) *big.Int {
	quotient, remainder := new(big.Int), new(big.Int)
	quotient.QuoRem(value.Num(), value.Denom(), remainder)
	if new(big.Int).Lsh(remainder, 1).Cmp(value.Denom()) >= 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	return quotient
}

func atomicDecimal(atomic, unit string) string {
	digits := len(unit) - 1
	value := atomic
	if len(value) <= digits {
		value = strings.Repeat("0", digits+1-len(value)) + value
	}
	whole := value[:len(value)-digits]
	if digits == 0 {
		return whole
	}
	fraction := strings.TrimRight(value[len(value)-digits:], "0")
	if fraction == "" {
		return whole
	}
	return whole + "." + fraction
}

func decimalRat(value *big.Rat, places int) string {
	formatted := value.FloatString(places)
	formatted = strings.TrimRight(strings.TrimRight(formatted, "0"), ".")
	if formatted == "" || formatted == "-0" {
		return "0"
	}
	return formatted
}

func requireAvailable(wanted, available, asset string) error {
	want, okWant := new(big.Int).SetString(wanted, 10)
	have, okHave := new(big.Int).SetString(available, 10)
	if !okWant || !okHave || have.Sign() < 0 {
		return errors.New("wallet returned an invalid balance")
	}
	if want.Cmp(have) > 0 {
		return fmt.Errorf("insufficient available %s balance after open order reservations", asset)
	}
	return nil
}

func (s *Service) syncRelay(ctx context.Context) error {
	s.mu.RLock()
	unlocked := s.root != nil
	journal := s.journal
	s.mu.RUnlock()
	if !unlocked || journal == nil {
		return nil
	}
	if !s.relaySyncMu.TryLock() {
		return nil
	}
	defer s.relaySyncMu.Unlock()
	if time.Since(s.lastRelaySync) < 3*time.Second {
		return nil
	}
	s.lastRelaySync = time.Now()
	identity, _, cleanup, err := s.identities()
	if err != nil {
		return err
	}
	defer cleanup()
	identityPublic := hex.EncodeToString(identity.Public().(ed25519.PublicKey))
	now := time.Now().UTC()
	if err := s.reconcileNegotiations(ctx, journal, now); err != nil {
		return err
	}
	cursor, err := journal.RelayCursor()
	if err != nil {
		return err
	}
	for pages := 0; pages < 10; pages++ {
		now = time.Now().UTC()
		// A version 2 acceptance can contain a large post-quantum QDAY funding
		// transaction. Fetch one mailbox record at a time to keep relay responses
		// bounded independently of the number of queued buyers.
		poll, err := trade.NewPoll(s.config.Network, cursor, 1, identity, now)
		if err != nil {
			return err
		}
		page, err := s.relay.Poll(ctx, poll)
		if err != nil {
			return err
		}
		for _, item := range page.Items {
			switch item.Kind {
			case relay.MailboxAcceptance:
				if item.Acceptance == nil {
					return errors.New("relay returned an empty acceptance mailbox item")
				}
				acceptance := item.Acceptance.Signed
				orderRecord, err := s.relay.Order(ctx, acceptance.Acceptance.OrderID)
				if err != nil {
					return err
				}
				verificationTime := now
				if acceptance.Acceptance.ExpiresAt <= now.Unix() {
					verificationTime = time.Unix(item.ReceivedAt, 0)
				}
				if err := acceptance.Verify(orderRecord.Signed, verificationTime); err != nil {
					return fmt.Errorf("invalid acceptance in relay mailbox: %w", err)
				}
				if orderRecord.Signed.Order.MakerPublicKey != identityPublic {
					return errors.New("relay routed an acceptance to the wrong identity")
				}
				if orderRecord.Status == relay.StatusMatched {
					if orderRecord.Acceptance != nil && orderRecord.Match != nil && orderRecord.Acceptance.ID == acceptance.ID {
						if _, _, err := journal.Create(swapstate.RoleMaker, orderRecord.Signed, *orderRecord.Acceptance, *orderRecord.Match, now); err != nil {
							return err
						}
					}
					if err := journal.RemoveIncomingAcceptance(acceptance.ID); err != nil {
						return err
					}
					continue
				}
				if orderRecord.Status != relay.StatusOpen || acceptance.Acceptance.ExpiresAt <= now.Unix() {
					continue
				}
				if _, _, err := journal.SaveIncomingAcceptance(orderRecord.Signed, acceptance, now); err != nil {
					return err
				}
				// An order has identical terms for every taker, so the first valid
				// acceptance wins automatically. The signed offer authorizes funding
				// only after both clients validate the complete immutable agreement.
				s.negotiationMu.Lock()
				_, err = s.matchAcceptance(ctx, journal, identity, acceptance.ID)
				s.negotiationMu.Unlock()
				if err != nil {
					if errors.Is(err, errInvalidAsyncFunding) {
						if removeErr := journal.RemoveIncomingAcceptance(acceptance.ID); removeErr != nil {
							return removeErr
						}
					} else {
						return fmt.Errorf("automatically match acceptance: %w", err)
					}
				}
			case relay.MailboxMatch:
				if item.Match == nil {
					return errors.New("relay returned an empty match mailbox item")
				}
				negotiation, err := journal.PendingAcceptanceByMatch(*item.Match)
				if errors.Is(err, swapstate.ErrNotFound) {
					orderRecord, fetchErr := s.relay.Order(ctx, item.Match.Match.OrderID)
					if fetchErr != nil {
						return fetchErr
					} else if orderRecord.Acceptance == nil {
						return errors.New("matched order does not contain its acceptance")
					}
					negotiation = swapstate.Negotiation{Order: orderRecord.Signed, Acceptance: *orderRecord.Acceptance}
				} else if err != nil {
					return err
				}
				if negotiation.Acceptance.Acceptance.TakerPublicKey != identityPublic {
					return errors.New("relay routed a match to the wrong identity")
				}
				if _, _, err := journal.Create(swapstate.RoleTaker, negotiation.Order, negotiation.Acceptance, *item.Match, now); err != nil {
					return err
				}
				if err := journal.RemovePendingAcceptance(negotiation.Acceptance.ID); err != nil {
					return err
				}
			case relay.MailboxMessage:
				if item.Message == nil {
					return errors.New("relay returned an empty message mailbox item")
				}
				if _, err := journal.SaveRelayMessage(*item.Message, now); err != nil {
					return fmt.Errorf("invalid encrypted relay message: %w", err)
				}
			default:
				return fmt.Errorf("relay returned unknown mailbox item %q", item.Kind)
			}
			if item.Cursor <= cursor {
				return errors.New("relay mailbox cursor did not advance")
			}
			cursor = item.Cursor
			if err := journal.SetRelayCursor(cursor); err != nil {
				return err
			}
		}
		if !page.HasMore {
			return nil
		}
	}
	// Continue from the durable cursor on the next worker tick. Bounding one
	// pass keeps a busy order from starving wallet and chain processing.
	return nil
}

func (s *Service) reconcileNegotiations(ctx context.Context, journal *swapstate.Journal, now time.Time) error {
	records := make(map[string]relay.Record)
	orderRecord := func(orderID string) (relay.Record, error) {
		if record, ok := records[orderID]; ok {
			return record, nil
		}
		record, err := s.relay.Order(ctx, orderID)
		if err == nil {
			records[orderID] = record
		}
		return record, err
	}
	pending, err := journal.PendingAcceptances()
	if err != nil {
		return err
	}
	for _, negotiation := range pending {
		remove := negotiation.Acceptance.Acceptance.ExpiresAt <= now.Unix()
		if !remove {
			record, err := orderRecord(negotiation.Order.ID)
			if err != nil {
				return err
			}
			selected := record.Status == relay.StatusMatched && record.Acceptance != nil && record.Acceptance.ID == negotiation.Acceptance.ID
			remove = record.Status != relay.StatusOpen && !selected
		}
		if remove {
			s.releasePendingFunding(ctx, negotiation)
			if err := journal.RemovePendingAcceptance(negotiation.Acceptance.ID); err != nil {
				return err
			}
		} else if negotiation.Acceptance.Acceptance.Version == trade.AsyncProtocolVersion {
			funding := negotiation.Acceptance.Acceptance.Funding
			if funding.Asset == "BTC" && len(funding.RawTransactions) == 1 {
				s.mu.RLock()
				bitcoinClient := s.bitcoinClient
				s.mu.RUnlock()
				if bitcoin, ok := bitcoinClient.(bitcoinAsyncSwapClient); ok {
					if err := bitcoin.ReserveFunding(funding.RawTransactions[0]); err != nil {
						return fmt.Errorf("restore pending Bitcoin funding reservation: %w", err)
					}
				}
			}
		}
	}
	incoming, err := journal.IncomingAcceptances()
	if err != nil {
		return err
	}
	for _, negotiation := range incoming {
		remove := negotiation.Acceptance.Acceptance.ExpiresAt <= now.Unix()
		if !remove {
			record, err := orderRecord(negotiation.Order.ID)
			if err != nil {
				return err
			}
			selected := record.Status == relay.StatusMatched && record.Acceptance != nil && record.Acceptance.ID == negotiation.Acceptance.ID
			remove = record.Status != relay.StatusOpen && !selected
		}
		if remove {
			if err := journal.RemoveIncomingAcceptance(negotiation.Acceptance.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Service) releasePendingFunding(ctx context.Context, negotiation swapstate.Negotiation) {
	if negotiation.Acceptance.Acceptance.Version != trade.AsyncProtocolVersion {
		return
	}
	funding := negotiation.Acceptance.Acceptance.Funding
	s.mu.RLock()
	qdayClient, bitcoinClient := s.client, s.bitcoinClient
	s.mu.RUnlock()
	if funding.Asset == "QDAY" {
		if qday, ok := qdayClient.(qdayAsyncSwapClient); ok {
			_ = qday.CancelPreparedSwapFunding(ctx, negotiation.Acceptance.Acceptance.TradeID)
		}
	} else if funding.Asset == "BTC" && len(funding.RawTransactions) == 1 {
		if bitcoinClient, ok := bitcoinClient.(bitcoinAsyncSwapClient); ok {
			_ = bitcoinClient.ReleaseFunding(funding.RawTransactions[0])
		}
	}
}

func (s *Service) State(ctx context.Context) State {
	s.mu.RLock()
	state := State{Configured: s.Configured(), Unlocked: s.root != nil, Network: s.config.Network, RelayURL: s.config.RelayURL, Error: s.lastError}
	client, bitcoinClient := s.client, s.bitcoinClient
	s.mu.RUnlock()
	if state.Unlocked {
		if identity, _, cleanup, err := s.identities(); err == nil {
			state.IdentityPublicKey = hex.EncodeToString(identity.Public().(ed25519.PublicKey))
			cleanup()
		}
	}
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
	relayCtx, relayCancel := context.WithTimeout(ctx, 5*time.Second)
	defer relayCancel()
	if err := s.syncRelay(relayCtx); err != nil {
		state.RelayError = err.Error()
	}
	if status, err := s.relay.Status(relayCtx); err != nil {
		state.RelayError = joinError(state.RelayError, err)
	} else {
		state.Relay = &status
	}
	if negotiations, err := s.Negotiations(); err == nil {
		state.PendingTrades = len(negotiations.Pending)
		state.IncomingTrades = len(negotiations.Incoming)
		for _, record := range negotiations.Swaps {
			if !terminalSwapPhase(record.Phase) {
				state.ActiveSwaps++
			}
		}
	} else if state.Configured {
		state.RelayError = joinError(state.RelayError, err)
	}
	if state.Unlocked && state.QDAY != nil && state.Balance != nil && state.BitcoinBalance != nil {
		fundsCtx, fundsCancel := context.WithTimeout(ctx, 5*time.Second)
		s.fundsMu.Lock()
		funds, err := s.fundsSnapshot(fundsCtx, *state.QDAY, *state.Balance, *state.BitcoinBalance)
		s.fundsMu.Unlock()
		fundsCancel()
		if err != nil {
			state.RelayError = joinError(state.RelayError, err)
		} else {
			state.Funds = &funds
		}
	}
	return state
}

func (s *Service) Close() error {
	if s.workerCancel != nil {
		s.workerCancel()
		s.workerWG.Wait()
	}
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
