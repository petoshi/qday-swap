package app

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/petoshi/qday-swap/internal/bitcoin"
	"github.com/petoshi/qday-swap/internal/bitcoinwallet"
	"github.com/petoshi/qday-swap/internal/order"
	"github.com/petoshi/qday-swap/internal/relayclient"
	"github.com/petoshi/qday-swap/internal/swapstate"
	"github.com/petoshi/qday-swap/internal/walletd"
	"github.com/petoshi/qday-swap/internal/walletroot"
)

type engineQDAYChain struct {
	mu       sync.Mutex
	height   uint64
	outputs  map[string]*walletd.SwapOutput
	prepared map[string]enginePreparedQDAY
	claims   map[string]enginePreparedQDAYClaim
}

type enginePreparedQDAY struct {
	swapID   string
	contract string
	amount   string
	txid     string
	raw      string
}

type enginePreparedQDAYClaim struct {
	contract string
	outputID string
	secret   string
}

func engineContractKey(request walletd.RegisterSwapRequest) string {
	return fmt.Sprintf("%s:%d", request.SecretHash, request.RefundHeight)
}

func (q *engineQDAY) contractKey(swapID string) (string, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	request, ok := q.registered[swapID]
	if !ok {
		return "", errors.New("swap not registered")
	}
	return engineContractKey(request), nil
}

type engineQDAY struct {
	marker     byte
	unlocked   bool
	mu         sync.Mutex
	registered map[string]walletd.RegisterSwapRequest
	chain      *engineQDAYChain
}

func (q *engineQDAY) Status(context.Context) (walletd.Status, error) {
	height := uint64(12_000)
	if q.chain != nil {
		height = q.chain.height
	}
	return walletd.Status{Network: "qday-mainnet", Height: height, ScanHeight: height, Synced: true, NetworkSynced: true, Unlocked: q.unlocked, UnitAtomic: order.QDAYLegacyUnit}, nil
}
func (q *engineQDAY) Balance(context.Context) (walletd.Balance, error) {
	return walletd.Balance{Synced: true, UnitAtomic: order.QDAYLegacyUnit, Spendable: walletd.Amount{Atomic: "5000000000000000000000000", QDAY: "5"}}, nil
}
func (q *engineQDAY) Unlock(context.Context, string) error { q.unlocked = true; return nil }
func (q *engineQDAY) Lock(context.Context) error           { q.unlocked = false; return nil }
func (q *engineQDAY) CreateAddress(context.Context, string) (walletd.Address, error) {
	return walletd.Address{Address: "qday1ptest"}, nil
}
func (q *engineQDAY) CreateSwapKeys(_ context.Context, swapID string) (walletd.SwapKeyView, error) {
	classical := make([]byte, 32)
	reserve := make([]byte, 32)
	for i := range classical {
		classical[i] = q.marker
		reserve[i] = q.marker + 1
	}
	return walletd.SwapKeyView{SwapID: swapID, Keys: walletd.SwapKeys{
		Classical: hex.EncodeToString(classical), Reserve: hex.EncodeToString(reserve),
		Address: fmt.Sprintf("qday1ptest%02x", q.marker),
	}}, nil
}
func (q *engineQDAY) RegisterSwap(_ context.Context, request walletd.RegisterSwapRequest) (walletd.Swap, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.registered == nil {
		q.registered = make(map[string]walletd.RegisterSwapRequest)
	}
	if existing, ok := q.registered[request.SwapID]; ok && existing != request {
		return walletd.Swap{}, fmt.Errorf("registration changed")
	}
	q.registered[request.SwapID] = request
	return walletd.Swap{SwapID: request.SwapID, Role: request.Role, SecretHash: request.SecretHash, RefundHeight: request.RefundHeight}, nil
}
func (q *engineQDAY) Swap(_ context.Context, swapID string) (walletd.Swap, error) {
	q.mu.Lock()
	registration, ok := q.registered[swapID]
	q.mu.Unlock()
	if !ok {
		return walletd.Swap{}, errors.New("swap not registered")
	}
	view := walletd.Swap{SwapID: swapID, Role: registration.Role, SecretHash: registration.SecretHash, RefundHeight: registration.RefundHeight, Height: q.chain.height}
	q.chain.mu.Lock()
	defer q.chain.mu.Unlock()
	if output := q.chain.outputs[engineContractKey(registration)]; output != nil {
		view.Outputs = []walletd.SwapOutput{*output}
		view.Status = output.Status
	}
	return view, nil
}
func (q *engineQDAY) FundSwap(_ context.Context, swapID string, request walletd.FundSwapRequest) (walletd.SwapAction, error) {
	prepared, err := q.PrepareSwapFunding(context.Background(), swapID, request)
	if err != nil {
		return walletd.SwapAction{}, err
	}
	_, err = q.BroadcastTransactionPackage(context.Background(), walletd.TransactionPackage{
		BasisHeight: prepared.BasisHeight, BasisID: prepared.BasisID,
		Transactions: []string{prepared.RawTransaction}, TransactionIDs: []string{prepared.TransactionID},
	})
	if err != nil {
		return walletd.SwapAction{}, err
	}
	prepared.Status, prepared.Submitted, prepared.Confirmations = "confirmed", true, 3
	return prepared, nil
}
func (q *engineQDAY) PrepareSwapFunding(_ context.Context, swapID string, request walletd.FundSwapRequest) (walletd.SwapAction, error) {
	contract, err := q.contractKey(swapID)
	if err != nil {
		return walletd.SwapAction{}, err
	}
	q.chain.mu.Lock()
	defer q.chain.mu.Unlock()
	if existing := q.chain.outputs[contract]; existing != nil {
		return walletd.SwapAction{TransactionID: existing.FundingTransaction, Status: "confirmed", Confirmations: existing.Confirmations}, nil
	}
	digest := sha256.Sum256([]byte(fmt.Sprintf("qday-funding:%s:%d", swapID, q.marker)))
	txid := hex.EncodeToString(digest[:])
	raw := base64.RawStdEncoding.EncodeToString([]byte("qday-prepared:" + txid))
	if q.chain.prepared == nil {
		q.chain.prepared = make(map[string]enginePreparedQDAY)
	}
	q.chain.prepared[txid] = enginePreparedQDAY{swapID: swapID, contract: contract, amount: request.AmountAtomic, txid: txid, raw: raw}
	return walletd.SwapAction{
		TransactionID: txid, Status: "prepared", RawTransaction: raw,
		BasisHeight: q.chain.height, BasisID: strings.Repeat("0", 64),
	}, nil
}
func (q *engineQDAY) CancelPreparedSwapFunding(_ context.Context, swapID string) error {
	q.chain.mu.Lock()
	defer q.chain.mu.Unlock()
	for id, prepared := range q.chain.prepared {
		if prepared.swapID == swapID {
			delete(q.chain.prepared, id)
		}
	}
	return nil
}
func (q *engineQDAY) ValidateTransactionPackage(_ context.Context, value walletd.TransactionPackage) (walletd.TransactionPackage, error) {
	q.chain.mu.Lock()
	defer q.chain.mu.Unlock()
	if len(value.TransactionIDs) != 1 {
		return walletd.TransactionPackage{}, errors.New("test QDAY package is invalid")
	}
	if _, ok := q.chain.prepared[value.TransactionIDs[0]]; !ok {
		return walletd.TransactionPackage{}, errors.New("unknown test QDAY package")
	}
	return value, nil
}
func (q *engineQDAY) ValidateSwapFundingPackage(ctx context.Context, _ string, request walletd.ValidateSwapFundingRequest) (walletd.TransactionPackage, error) {
	value, err := q.ValidateTransactionPackage(ctx, request.Package)
	if err != nil {
		return walletd.TransactionPackage{}, err
	}
	q.chain.mu.Lock()
	defer q.chain.mu.Unlock()
	prepared, ok := q.chain.prepared[request.TransactionID]
	if !ok || prepared.amount != request.AmountAtomic {
		return walletd.TransactionPackage{}, errors.New("test QDAY package does not fund the requested contract amount")
	}
	return value, nil
}
func (q *engineQDAY) BroadcastTransactionPackage(ctx context.Context, value walletd.TransactionPackage) (walletd.BroadcastPackageResult, error) {
	q.chain.mu.Lock()
	defer q.chain.mu.Unlock()
	if len(value.TransactionIDs) != 1 {
		return walletd.BroadcastPackageResult{}, errors.New("test QDAY package is invalid")
	}
	if claim, ok := q.chain.claims[value.TransactionIDs[0]]; ok {
		output := q.chain.outputs[claim.contract]
		if output == nil || output.ID != claim.outputID || claim.secret == "" {
			return walletd.BroadcastPackageResult{}, errors.New("test QDAY claim template is incomplete")
		}
		height := q.chain.height
		output.Status, output.RevealedSecret, output.SpendTransaction, output.SpendHeight = "claimed", claim.secret, value.TransactionIDs[0], &height
		return walletd.BroadcastPackageResult{Package: value}, nil
	}
	prepared, ok := q.chain.prepared[value.TransactionIDs[0]]
	if !ok {
		return walletd.BroadcastPackageResult{}, errors.New("unknown test QDAY package")
	}
	height := q.chain.height
	q.chain.outputs[prepared.contract] = &walletd.SwapOutput{
		ID: prepared.txid, Value: walletd.Amount{Atomic: prepared.amount},
		FundingTransaction: prepared.txid, FundingHeight: &height, Confirmations: 3, Status: "funded",
	}
	return walletd.BroadcastPackageResult{Package: value}, nil
}
func (q *engineQDAY) PrepareSwapClaimTemplate(_ context.Context, swapID string, request walletd.SpendSwapRequest) (walletd.SwapAction, error) {
	contract, err := q.contractKey(swapID)
	if err != nil {
		return walletd.SwapAction{}, err
	}
	q.chain.mu.Lock()
	defer q.chain.mu.Unlock()
	output := q.chain.outputs[contract]
	if output == nil || output.ID != request.OutputID {
		return walletd.SwapAction{}, errors.New("unknown QDAY swap output")
	}
	digest := sha256.Sum256([]byte("qday-claim-template:" + contract))
	txid := hex.EncodeToString(digest[:])
	if q.chain.claims == nil {
		q.chain.claims = make(map[string]enginePreparedQDAYClaim)
	}
	q.chain.claims[txid] = enginePreparedQDAYClaim{contract: contract, outputID: request.OutputID}
	raw := base64.RawStdEncoding.EncodeToString([]byte("qday-claim-template:" + txid))
	return walletd.SwapAction{TransactionID: txid, RawTransaction: raw, BasisHeight: q.chain.height, BasisID: strings.Repeat("0", 64), Status: "prepared"}, nil
}
func (q *engineQDAY) CompleteSwapClaimTemplate(_ context.Context, _ string, request walletd.CompleteSwapClaimTemplateRequest) (walletd.TransactionPackage, error) {
	q.chain.mu.Lock()
	defer q.chain.mu.Unlock()
	if len(request.Package.TransactionIDs) != 1 {
		return walletd.TransactionPackage{}, errors.New("test QDAY claim package is invalid")
	}
	claim, ok := q.chain.claims[request.Package.TransactionIDs[0]]
	if !ok || claim.outputID != request.OutputID {
		return walletd.TransactionPackage{}, errors.New("unknown test QDAY claim template")
	}
	claim.secret = request.Secret
	q.chain.claims[request.Package.TransactionIDs[0]] = claim
	value := request.Package
	value.Transactions = []string{base64.RawStdEncoding.EncodeToString([]byte("qday-claim:" + value.TransactionIDs[0] + ":" + request.Secret))}
	return value, nil
}
func (q *engineQDAY) ClaimSwap(_ context.Context, swapID string, request walletd.SpendSwapRequest) (walletd.SwapAction, error) {
	contract, err := q.contractKey(swapID)
	if err != nil {
		return walletd.SwapAction{}, err
	}
	q.chain.mu.Lock()
	defer q.chain.mu.Unlock()
	output := q.chain.outputs[contract]
	if output == nil || output.ID != request.OutputID {
		return walletd.SwapAction{}, errors.New("unknown QDAY swap output")
	}
	digest := sha256.Sum256([]byte("qday-claim:" + swapID))
	height := q.chain.height
	output.Status, output.RevealedSecret, output.SpendTransaction, output.SpendHeight = "claimed", request.Secret, hex.EncodeToString(digest[:]), &height
	return walletd.SwapAction{TransactionID: output.SpendTransaction, Status: "confirmed", Confirmations: 1}, nil
}
func (q *engineQDAY) RefundSwap(_ context.Context, swapID string, request walletd.SpendSwapRequest) (walletd.SwapAction, error) {
	contract, err := q.contractKey(swapID)
	if err != nil {
		return walletd.SwapAction{}, err
	}
	q.chain.mu.Lock()
	defer q.chain.mu.Unlock()
	output := q.chain.outputs[contract]
	if output == nil || output.ID != request.OutputID {
		return walletd.SwapAction{}, errors.New("unknown QDAY swap output")
	}
	digest := sha256.Sum256([]byte("qday-refund:" + swapID))
	height := q.chain.height
	output.Status, output.SpendTransaction, output.SpendHeight = "refunded", hex.EncodeToString(digest[:]), &height
	return walletd.SwapAction{TransactionID: output.SpendTransaction, Status: "confirmed", Confirmations: 1}, nil
}

type engineBitcoinChain struct {
	mu     sync.Mutex
	height int32
	txs    map[string]bitcoinwallet.Transaction
}

type engineBitcoin struct {
	unlocked          bool
	mu                sync.Mutex
	watched           []bitcoin.Contract
	marker            byte
	chain             *engineBitcoinChain
	failNextBroadcast bool
}

func (b *engineBitcoin) Status() (bitcoinwallet.Status, error) {
	height := int32(900_000)
	if b.chain != nil {
		height = b.chain.height
	}
	return bitcoinwallet.Status{Network: "mainnet", HeaderHeight: height, WalletHeight: height, HeadersSynced: true, WalletSynced: true, Unlocked: b.unlocked}, nil
}
func (b *engineBitcoin) Balance() (bitcoinwallet.Balance, error) {
	return bitcoinwallet.Balance{Confirmed: bitcoinwallet.Amount{Satoshis: "100000000", BTC: "1"}, Total: bitcoinwallet.Amount{Satoshis: "100000000", BTC: "1"}}, nil
}
func (b *engineBitcoin) Unlock(string) error { b.unlocked = true; return nil }
func (b *engineBitcoin) Lock()               { b.unlocked = false }
func (b *engineBitcoin) ReceiveAddress() (string, error) {
	return "bc1qtest", nil
}
func (b *engineBitcoin) WatchContract(contract bitcoin.Contract) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.watched = append(b.watched, contract)
	return nil
}
func (b *engineBitcoin) PrepareFunding(contract bitcoin.Contract) (bitcoin.Funding, error) {
	pkScript, err := contract.PkScript()
	if err != nil {
		return bitcoin.Funding{}, err
	}
	previous := chainhash.Hash{}
	previous[0] = b.marker
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(wire.NewTxIn(wire.NewOutPoint(&previous, uint32(b.marker)), nil, nil))
	tx.AddTxOut(wire.NewTxOut(contract.Amount, pkScript))
	raw, err := bitcoin.EncodeTransaction(tx)
	if err != nil {
		return bitcoin.Funding{}, err
	}
	return contract.FundingFromRaw(raw)
}
func (b *engineBitcoin) Broadcast(raw string) (bitcoinwallet.Transaction, error) {
	b.mu.Lock()
	if b.failNextBroadcast {
		b.failNextBroadcast = false
		b.mu.Unlock()
		return bitcoinwallet.Transaction{}, errors.New("simulated broadcast interruption")
	}
	b.mu.Unlock()
	tx, err := bitcoin.DecodeTransaction(raw)
	if err != nil {
		return bitcoinwallet.Transaction{}, err
	}
	transaction := bitcoinwallet.Transaction{ID: tx.TxID(), Raw: raw, Confirmations: 1, Height: b.chain.height}
	b.chain.mu.Lock()
	if existing, ok := b.chain.txs[transaction.ID]; ok {
		transaction = existing
	} else {
		b.chain.txs[transaction.ID] = transaction
	}
	b.chain.mu.Unlock()
	return transaction, nil
}
func (b *engineBitcoin) Transaction(txid string) (bitcoinwallet.Transaction, error) {
	b.chain.mu.Lock()
	defer b.chain.mu.Unlock()
	transaction, ok := b.chain.txs[txid]
	if !ok {
		return bitcoinwallet.Transaction{}, bitcoinwallet.ErrTransactionNotFound
	}
	return transaction, nil
}
func (b *engineBitcoin) FindSpend(funding bitcoin.Funding) (bitcoinwallet.Transaction, error) {
	b.chain.mu.Lock()
	defer b.chain.mu.Unlock()
	for _, transaction := range b.chain.txs {
		tx, err := bitcoin.DecodeTransaction(transaction.Raw)
		if err != nil {
			return bitcoinwallet.Transaction{}, err
		}
		for _, input := range tx.TxIn {
			if input.PreviousOutPoint.Hash == funding.Hash && input.PreviousOutPoint.Index == funding.Vout {
				return transaction, nil
			}
		}
	}
	return bitcoinwallet.Transaction{}, bitcoinwallet.ErrTransactionNotFound
}
func (b *engineBitcoin) DestinationScript() ([]byte, error) { return []byte{0x51}, nil }
func (b *engineBitcoin) ReserveFunding(string) error        { return nil }
func (b *engineBitcoin) ReleaseFunding(string) error        { return nil }

func newEngineServiceAt(t *testing.T, relayURL string, root walletroot.Root, qday *engineQDAY, bitcoinClient *engineBitcoin, journalPath string) *Service {
	t.Helper()
	relayAPI, err := relayclient.New(relayURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := swapstate.Open(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{
		config: Config{Network: "mainnet", BitcoinNetwork: "mainnet", RelayURL: relayURL, RequireSwapApproval: true},
		ctx:    context.Background(), relay: relayAPI, client: qday,
		bitcoinClient: bitcoinClient, journal: journal, root: &root,
	}
	t.Cleanup(func() { _ = service.Close() })
	return service
}

func newEngineService(t *testing.T, relayURL string, root walletroot.Root, qday *engineQDAY, bitcoinClient *engineBitcoin) *Service {
	t.Helper()
	return newEngineServiceAt(t, relayURL, root, qday, bitcoinClient, filepath.Join(t.TempDir(), "swaps.db"))
}

func TestEngineNegotiatesExecutesAndCompletesBothDirections(t *testing.T) {
	for _, test := range []struct {
		name, giveAsset, giveAmount, receiveAmount, makerQDAYRole, takerQDAYRole string
	}{
		{"maker gives QDAY", "QDAY", "1", "0.0001", "refund", "recipient"},
		{"maker gives BTC", "BTC", "0.0001", "1", "recipient", "refund"},
	} {
		t.Run(test.name, func(t *testing.T) {
			runEngineTrade(t, test.giveAsset, test.giveAmount, test.receiveAmount, test.makerQDAYRole, test.takerQDAYRole)
		})
	}
}

type negotiatedEngineTrade struct {
	maker, taker               *Service
	makerQDAY, takerQDAY       *engineQDAY
	makerBitcoin, takerBitcoin *engineBitcoin
	qdayChain                  *engineQDAYChain
	bitcoinChain               *engineBitcoinChain
	swapID                     string
	makerJournalPath           string
}

func negotiateEngineTrade(t *testing.T, giveAsset, giveAmount, receiveAmount string) negotiatedEngineTrade {
	t.Helper()
	relayURL := testRelayURL(t)
	makerRoot, err := walletroot.New()
	if err != nil {
		t.Fatal(err)
	}
	takerRoot, err := walletroot.New()
	if err != nil {
		t.Fatal(err)
	}
	qdayChain := &engineQDAYChain{height: 12_000, outputs: make(map[string]*walletd.SwapOutput), prepared: make(map[string]enginePreparedQDAY)}
	bitcoinChain := &engineBitcoinChain{height: 900_000, txs: make(map[string]bitcoinwallet.Transaction)}
	makerQDAY, takerQDAY := &engineQDAY{marker: 1, unlocked: true, chain: qdayChain}, &engineQDAY{marker: 3, unlocked: true, chain: qdayChain}
	makerBitcoin, takerBitcoin := &engineBitcoin{unlocked: true, marker: 1, chain: bitcoinChain}, &engineBitcoin{unlocked: true, marker: 3, chain: bitcoinChain}
	makerJournalPath := filepath.Join(t.TempDir(), "maker-swaps.db")
	maker := newEngineServiceAt(t, relayURL, makerRoot, makerQDAY, makerBitcoin, makerJournalPath)
	taker := newEngineService(t, relayURL, takerRoot, takerQDAY, takerBitcoin)

	offer, err := maker.CreateOffer(context.Background(), CreateOfferRequest{
		GiveAsset: giveAsset, GiveAmount: giveAmount, ReceiveAmount: receiveAmount, LifetimeMinutes: 60,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := taker.AcceptOffer(context.Background(), offer.Signed.ID); err != nil {
		t.Fatal(err)
	}
	maker.lastRelaySync = time.Time{}
	if err := maker.syncRelay(context.Background()); err != nil {
		t.Fatal(err)
	}
	negotiations, err := maker.Negotiations()
	if err != nil || len(negotiations.Incoming) != 0 || len(negotiations.Swaps) != 1 {
		t.Fatalf("maker negotiations=%#v err=%v", negotiations, err)
	}
	makerSwap := negotiations.Swaps[0]
	taker.lastRelaySync = time.Time{}
	if err := taker.syncRelay(context.Background()); err != nil {
		t.Fatal(err)
	}

	makerRecord, err := maker.journal.Swap(makerSwap.ID)
	if err != nil {
		t.Fatal(err)
	}
	takerRecord, err := taker.journal.Swap(makerSwap.ID)
	if err != nil {
		t.Fatal(err)
	}
	if makerRecord.Phase != swapstate.PhaseAsyncTakerFunding || takerRecord.Phase != swapstate.PhaseMatched {
		t.Fatalf("phases maker=%s (%s), taker=%s (%s)", makerRecord.Phase, makerRecord.LastError, takerRecord.Phase, takerRecord.LastError)
	}
	if makerRecord.AgreementJSON == "" || takerRecord.AgreementJSON != "" {
		t.Fatalf("maker must validate terms before matching while offline taker initializes on return\nmaker=%#v\ntaker=%#v", makerRecord, takerRecord)
	}
	if len(makerQDAY.registered) != 1 || len(takerQDAY.registered) != 1 || len(makerBitcoin.watched) != 1 || len(takerBitcoin.watched) != 1 {
		t.Fatalf("contracts registered makerQDAY=%d takerQDAY=%d makerBTC=%d takerBTC=%d", len(makerQDAY.registered), len(takerQDAY.registered), len(makerBitcoin.watched), len(takerBitcoin.watched))
	}
	if len(qdayChain.outputs) != 0 || len(bitcoinChain.txs) != 0 {
		t.Fatalf("funding happened before approval: qday=%d bitcoin=%d", len(qdayChain.outputs), len(bitcoinChain.txs))
	}
	return negotiatedEngineTrade{
		maker: maker, taker: taker, makerQDAY: makerQDAY, takerQDAY: takerQDAY,
		makerBitcoin: makerBitcoin, takerBitcoin: takerBitcoin,
		qdayChain: qdayChain, bitcoinChain: bitcoinChain, swapID: makerSwap.ID,
		makerJournalPath: makerJournalPath,
	}
}

func runEngineTrade(t *testing.T, giveAsset, giveAmount, receiveAmount, makerQDAYRole, takerQDAYRole string) {
	trade := negotiateEngineTrade(t, giveAsset, giveAmount, receiveAmount)
	maker, taker := trade.maker, trade.taker
	makerQDAY, takerQDAY := trade.makerQDAY, trade.takerQDAY
	makerSwapID := trade.swapID
	makerRecord, _ := maker.journal.Swap(makerSwapID)
	if makerQDAY.registered[makerRecord.Order.Order.SessionID].Role != makerQDAYRole || takerQDAY.registered[makerSwapID].Role != takerQDAYRole {
		t.Fatalf("QDAY roles maker=%q taker=%q", makerQDAY.registered[makerRecord.Order.Order.SessionID].Role, takerQDAY.registered[makerSwapID].Role)
	}
	for range 8 {
		maker.lastRelaySync, taker.lastRelaySync = time.Time{}, time.Time{}
		if err := maker.syncRelay(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := taker.syncRelay(context.Background()); err != nil {
			t.Fatal(err)
		}
		maker.driveSwaps(context.Background())
		taker.driveSwaps(context.Background())
	}

	makerRecord, _ = maker.journal.Swap(makerSwapID)
	takerRecord, _ := taker.journal.Swap(makerSwapID)
	if makerRecord.Phase != swapstate.PhaseComplete || takerRecord.Phase != swapstate.PhaseComplete {
		t.Fatalf("completed phases maker=%s (%s), taker=%s (%s)", makerRecord.Phase, makerRecord.LastError, takerRecord.Phase, takerRecord.LastError)
	}
	if makerRecord.RevealedSecret == "" || makerRecord.RevealedSecret != takerRecord.RevealedSecret {
		t.Fatalf("revealed secrets maker=%q taker=%q", makerRecord.RevealedSecret, takerRecord.RevealedSecret)
	}

}

func TestEngineOnlyReportsPersistentFailures(t *testing.T) {
	trade := negotiateEngineTrade(t, "BTC", "0.0001", "1")
	record, err := trade.maker.journal.Swap(trade.swapID)
	if err != nil {
		t.Fatal(err)
	}
	retryErr := errors.New("chain changed while signing; retry")
	for attempt := 1; attempt <= engineFailureDisplayAttempts; attempt++ {
		trade.maker.recordEngineResult(trade.maker.journal, record, retryErr)
		current, err := trade.maker.journal.Swap(trade.swapID)
		if err != nil {
			t.Fatal(err)
		}
		if attempt < engineFailureDisplayAttempts && current.LastError != "" {
			t.Fatalf("transient failure was shown after attempt %d: %q", attempt, current.LastError)
		} else if attempt == engineFailureDisplayAttempts && current.LastError != retryErr.Error() {
			t.Fatalf("persistent failure = %q, want %q", current.LastError, retryErr)
		}
	}
	record, err = trade.maker.journal.Swap(trade.swapID)
	if err != nil {
		t.Fatal(err)
	}
	trade.maker.recordEngineResult(trade.maker.journal, record, nil)
	record, err = trade.maker.journal.Swap(trade.swapID)
	if err != nil {
		t.Fatal(err)
	} else if record.LastError != "" {
		t.Fatalf("successful retry left error %q", record.LastError)
	}
}

func TestEngineExecutesSignedTradeWithoutSecondApproval(t *testing.T) {
	trade := negotiateEngineTrade(t, "QDAY", "1", "0.0001")
	trade.maker.config.RequireSwapApproval = false
	trade.taker.config.RequireSwapApproval = false
	for range 8 {
		trade.maker.lastRelaySync, trade.taker.lastRelaySync = time.Time{}, time.Time{}
		if err := trade.maker.syncRelay(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := trade.taker.syncRelay(context.Background()); err != nil {
			t.Fatal(err)
		}
		trade.maker.driveSwaps(context.Background())
		trade.taker.driveSwaps(context.Background())
	}
	makerRecord, err := trade.maker.journal.Swap(trade.swapID)
	if err != nil {
		t.Fatal(err)
	}
	takerRecord, err := trade.taker.journal.Swap(trade.swapID)
	if err != nil {
		t.Fatal(err)
	}
	if !makerRecord.Approved || !takerRecord.Approved {
		t.Fatalf("signed trade was not automatically authorized: maker=%v taker=%v", makerRecord.Approved, takerRecord.Approved)
	}
	if makerRecord.Phase != swapstate.PhaseComplete || takerRecord.Phase != swapstate.PhaseComplete {
		t.Fatalf("automatic trade did not complete: maker=%s (%s), taker=%s (%s)", makerRecord.Phase, makerRecord.LastError, takerRecord.Phase, takerRecord.LastError)
	}
}

func TestEngineCompletesWhenParticipantsReturnSequentially(t *testing.T) {
	for _, test := range []struct {
		name, giveAsset, giveAmount, receiveAmount string
	}{
		{"maker gives QDAY", "QDAY", "1", "0.0001"},
		{"maker gives Bitcoin", "BTC", "0.0001", "1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			trade := negotiateEngineTrade(t, test.giveAsset, test.giveAmount, test.receiveAmount)

			// The taker is offline. The maker can relay the exact funding package
			// signed with the acceptance, fund its own leg and leave a claim
			// template which needs only the taker's already committed secret.
			trade.maker.driveSwaps(context.Background())
			makerRecord, err := trade.maker.journal.Swap(trade.swapID)
			if err != nil {
				t.Fatal(err)
			} else if makerRecord.Phase != swapstate.PhaseAsyncTakerClaiming || makerRecord.MakerClaimTemplate == "" {
				t.Fatalf("maker did not finish its single online turn: %s (%s), template=%v", makerRecord.Phase, makerRecord.LastError, makerRecord.MakerClaimTemplate != "")
			}

			// The maker never runs again. On the taker's next opening, the relay
			// delivers both the maker funding notice and claim template. The taker
			// claims its leg and relays the completed maker claim in one turn.
			trade.taker.lastRelaySync = time.Time{}
			if err := trade.taker.syncRelay(context.Background()); err != nil {
				t.Fatal(err)
			}
			trade.taker.driveSwaps(context.Background())
			takerRecord, err := trade.taker.journal.Swap(trade.swapID)
			if err != nil {
				t.Fatal(err)
			} else if takerRecord.Phase != swapstate.PhaseComplete {
				t.Fatalf("taker did not complete both legs after returning: %s (%s)", takerRecord.Phase, takerRecord.LastError)
			} else if takerRecord.RevealedSecret == "" {
				t.Fatal("completed sequential swap did not reveal its committed secret")
			}

			makerRecord, err = trade.maker.journal.Swap(trade.swapID)
			if err != nil {
				t.Fatal(err)
			} else if makerRecord.Phase != swapstate.PhaseAsyncTakerClaiming {
				t.Fatalf("maker journal changed while its application was offline: %s", makerRecord.Phase)
			}
		})
	}
}

func TestSwapDetailsShowBothFundingAndClaimTransactions(t *testing.T) {
	trade := negotiateEngineTrade(t, "QDAY", "1", "0.0001")
	trade.maker.driveSwaps(context.Background())
	trade.taker.lastRelaySync = time.Time{}
	if err := trade.taker.syncRelay(context.Background()); err != nil {
		t.Fatal(err)
	}
	trade.taker.driveSwaps(context.Background())

	details, err := trade.taker.SwapDetails(context.Background(), trade.swapID)
	if err != nil {
		t.Fatal(err)
	} else if details.Swap.Phase != swapstate.PhaseComplete {
		t.Fatalf("detail phase = %s", details.Swap.Phase)
	}
	found := make(map[string]SwapTransaction)
	for _, transaction := range details.Transactions {
		found[transaction.Kind+":"+transaction.Party+":"+transaction.Asset] = transaction
	}
	for _, key := range []string{
		"funding:taker:BTC",
		"funding:maker:QDAY",
		"claim:taker:QDAY",
		"claim:maker:BTC",
	} {
		transaction, ok := found[key]
		if !ok {
			t.Fatalf("missing %s in %#v", key, details.Transactions)
		} else if transaction.TransactionID == "" || transaction.Status != "confirmed" || transaction.Confirmations == 0 {
			t.Fatalf("incomplete %s evidence: %#v", key, transaction)
		}
	}
}

func TestEngineRefundsEitherMakerAssetAndRecoversAfterReorg(t *testing.T) {
	for _, test := range []struct {
		name, giveAsset, giveAmount, receiveAmount string
	}{
		{"QDAY refund", "QDAY", "1", "0.0001"},
		{"Bitcoin refund", "BTC", "0.0001", "1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			trade := negotiateEngineTrade(t, test.giveAsset, test.giveAmount, test.receiveAmount)
			trade.maker.driveSwaps(context.Background())
			makerRecord, err := trade.maker.journal.Swap(trade.swapID)
			if err != nil {
				t.Fatal(err)
			} else if makerRecord.Phase != swapstate.PhaseAsyncTakerClaiming {
				t.Fatalf("maker did not reach claim wait before refund test: %s (%s)", makerRecord.Phase, makerRecord.LastError)
			}
			agreement, err := agreementFromRecord(makerRecord)
			if err != nil {
				t.Fatal(err)
			}
			if test.giveAsset == "QDAY" {
				trade.qdayChain.mu.Lock()
				trade.qdayChain.height = agreement.QDAYRefundHeight
				trade.qdayChain.mu.Unlock()
			} else {
				trade.bitcoinChain.mu.Lock()
				trade.bitcoinChain.height = int32(agreement.BitcoinRefundHeight)
				trade.bitcoinChain.mu.Unlock()
			}
			for range 3 {
				trade.maker.driveSwaps(context.Background())
			}
			makerRecord, _ = trade.maker.journal.Swap(trade.swapID)
			if makerRecord.Phase != swapstate.PhaseRefunded {
				t.Fatalf("refund did not complete: %s (%s)", makerRecord.Phase, makerRecord.LastError)
			}

			if test.giveAsset == "QDAY" {
				makerFunding, ok, err := fundingFromRecord(makerRecord, "maker")
				if err != nil || !ok {
					t.Fatalf("maker QDAY funding notice: %#v, %v", makerFunding, err)
				}
				trade.qdayChain.mu.Lock()
				var output *walletd.SwapOutput
				for _, candidate := range trade.qdayChain.outputs {
					if candidate.FundingTransaction == makerFunding.TransactionID {
						output = candidate
						break
					}
				}
				if output != nil {
					output.Status, output.SpendTransaction, output.SpendHeight = "funded", "", nil
				}
				trade.qdayChain.mu.Unlock()
				if output == nil {
					t.Fatal("maker QDAY funding output is missing")
				}
			} else {
				action, err := trade.maker.journal.Action(trade.swapID + ":maker-refund")
				if err != nil {
					t.Fatal(err)
				}
				trade.bitcoinChain.mu.Lock()
				delete(trade.bitcoinChain.txs, action.TransactionID)
				trade.bitcoinChain.mu.Unlock()
				trade.makerBitcoin.mu.Lock()
				trade.makerBitcoin.failNextBroadcast = true
				trade.makerBitcoin.mu.Unlock()
			}
			trade.maker.driveSwaps(context.Background())
			makerRecord, _ = trade.maker.journal.Swap(trade.swapID)
			if makerRecord.Phase != swapstate.PhaseRefunding {
				t.Fatalf("refund reorg did not rewind: %s (%s)", makerRecord.Phase, makerRecord.LastError)
			}
			for range 2 {
				trade.maker.driveSwaps(context.Background())
			}
			makerRecord, _ = trade.maker.journal.Swap(trade.swapID)
			if makerRecord.Phase != swapstate.PhaseRefunded {
				t.Fatalf("refund replay did not complete: %s (%s)", makerRecord.Phase, makerRecord.LastError)
			}
		})
	}
}

func TestEngineRestartBroadcastsExactPreparedFunding(t *testing.T) {
	trade := negotiateEngineTrade(t, "BTC", "0.0001", "1")
	trade.makerBitcoin.mu.Lock()
	trade.makerBitcoin.failNextBroadcast = true
	trade.makerBitcoin.mu.Unlock()
	if _, err := trade.maker.ApproveSwap(trade.swapID); err != nil {
		t.Fatal(err)
	}
	actionID := trade.swapID + ":maker-funding"
	prepared, err := trade.maker.journal.Action(actionID)
	if err != nil {
		t.Fatal(err)
	} else if prepared.Status != swapstate.ActionPrepared || prepared.RawTransaction == "" {
		t.Fatalf("prepared action=%#v", prepared)
	}
	record, err := trade.maker.journal.Swap(trade.swapID)
	if err != nil {
		t.Fatal(err)
	} else if record.Phase != swapstate.PhaseAsyncMakerFunding || record.MakerFunding != "" {
		t.Fatalf("swap moved after interrupted broadcast: %#v", record)
	}

	root := *trade.maker.root
	if err := trade.maker.journal.Close(); err != nil {
		t.Fatal(err)
	}
	trade.maker.journal = nil
	trade.maker.root = nil
	journal, err := swapstate.Open(trade.makerJournalPath)
	if err != nil {
		t.Fatal(err)
	}
	restarted := &Service{
		config: trade.maker.config, ctx: context.Background(), relay: trade.maker.relay,
		client: trade.makerQDAY, bitcoinClient: trade.makerBitcoin, journal: journal, root: &root,
	}
	t.Cleanup(func() { _ = restarted.Close() })
	trade.makerQDAY.unlocked = true
	trade.makerBitcoin.unlocked = true
	restarted.driveSwaps(context.Background())

	broadcast, err := restarted.journal.Action(actionID)
	if err != nil {
		t.Fatal(err)
	} else if broadcast.Status != swapstate.ActionConfirmed {
		t.Fatalf("restarted action=%#v", broadcast)
	} else if broadcast.RawTransaction != prepared.RawTransaction || broadcast.TransactionID != prepared.TransactionID {
		t.Fatal("restart changed the prepared Bitcoin transaction")
	}
	record, err = restarted.journal.Swap(trade.swapID)
	if err != nil {
		t.Fatal(err)
	} else if record.MakerFunding == "" || record.Phase != swapstate.PhaseAsyncTakerClaiming {
		t.Fatalf("restart did not resume funding: %#v", record)
	}
	trade.bitcoinChain.mu.Lock()
	_, published := trade.bitcoinChain.txs[prepared.TransactionID]
	trade.bitcoinChain.mu.Unlock()
	if !published {
		t.Fatal("prepared Bitcoin funding was not broadcast after restart")
	}
}

func setEngineFundingConfirmations(t *testing.T, trade negotiatedEngineTrade, record swapstate.Swap, party string, confirmations int32) {
	t.Helper()
	notice, ok, err := fundingFromRecord(record, party)
	if err != nil {
		t.Fatal(err)
	} else if !ok {
		t.Fatalf("%s funding notice is missing", party)
	}
	if notice.Asset == "QDAY" {
		trade.qdayChain.mu.Lock()
		defer trade.qdayChain.mu.Unlock()
		var output *walletd.SwapOutput
		for _, candidate := range trade.qdayChain.outputs {
			if candidate.FundingTransaction == notice.TransactionID {
				output = candidate
				break
			}
		}
		if output == nil {
			t.Fatalf("QDAY %s funding output is missing", party)
		}
		output.Confirmations = uint64(max(confirmations, 0))
		return
	}
	trade.bitcoinChain.mu.Lock()
	defer trade.bitcoinChain.mu.Unlock()
	transaction, ok := trade.bitcoinChain.txs[notice.TransactionID]
	if !ok {
		t.Fatalf("Bitcoin %s funding transaction is missing", party)
	}
	transaction.Confirmations = confirmations
	if confirmations == 0 {
		transaction.Height = 0
	} else if transaction.Height == 0 {
		transaction.Height = trade.bitcoinChain.height
	}
	trade.bitcoinChain.txs[notice.TransactionID] = transaction
}

func TestEngineWillNotClaimWhileEitherFundingLostConfirmations(t *testing.T) {
	for _, test := range []struct {
		name, giveAsset, giveAmount, receiveAmount string
	}{
		{"maker QDAY funding reorg", "QDAY", "1", "0.0001"},
		{"maker Bitcoin funding reorg", "BTC", "0.0001", "1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			trade := negotiateEngineTrade(t, test.giveAsset, test.giveAmount, test.receiveAmount)
			trade.maker.driveSwaps(context.Background())
			makerRecord, err := trade.maker.journal.Swap(trade.swapID)
			if err != nil {
				t.Fatal(err)
			} else if makerRecord.Phase != swapstate.PhaseAsyncTakerClaiming {
				t.Fatalf("maker did not finish asynchronous funding: %s (%s)", makerRecord.Phase, makerRecord.LastError)
			}
			setEngineFundingConfirmations(t, trade, makerRecord, "maker", 0)

			trade.taker.lastRelaySync = time.Time{}
			if err := trade.taker.syncRelay(context.Background()); err != nil {
				t.Fatal(err)
			}
			trade.taker.driveSwaps(context.Background())
			record, err := trade.taker.journal.Swap(trade.swapID)
			if err != nil {
				t.Fatal(err)
			} else if record.Phase != swapstate.PhaseAsyncMakerFunding {
				t.Fatalf("lost maker funding did not rewind before claim: %s (%s)", record.Phase, record.LastError)
			} else if record.RevealedSecret != "" {
				t.Fatal("the taker revealed its secret while maker funding was unconfirmed")
			}

			setEngineFundingConfirmations(t, trade, record, "maker", 3)
			trade.taker.driveSwaps(context.Background())
			for range 3 {
				trade.maker.driveSwaps(context.Background())
				trade.taker.driveSwaps(context.Background())
			}
			makerRecord, _ = trade.maker.journal.Swap(trade.swapID)
			takerRecord, _ := trade.taker.journal.Swap(trade.swapID)
			if makerRecord.Phase != swapstate.PhaseComplete || takerRecord.Phase != swapstate.PhaseComplete {
				t.Fatalf("swap did not recover after funding reconfirmed: maker=%s (%s), taker=%s (%s)", makerRecord.Phase, makerRecord.LastError, takerRecord.Phase, takerRecord.LastError)
			}
		})
	}
}

func TestEngineExpiresStaleAgreementBeforeAnyFunding(t *testing.T) {
	trade := negotiateEngineTrade(t, "QDAY", "1", "0.0001")
	record, err := trade.maker.journal.Swap(trade.swapID)
	if err != nil {
		t.Fatal(err)
	}
	agreement, err := agreementFromRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	trade.qdayChain.mu.Lock()
	trade.qdayChain.height = agreement.QDAYRefundHeight - minimumQDAYFundingWindowBlocks + 1
	trade.qdayChain.mu.Unlock()
	trade.bitcoinChain.mu.Lock()
	trade.bitcoinChain.height = int32(agreement.BitcoinRefundHeight - minimumBitcoinFundingWindowBlocks + 1)
	trade.bitcoinChain.mu.Unlock()

	if _, err := trade.maker.ApproveSwap(trade.swapID); err != nil {
		t.Fatal(err)
	}
	record, err = trade.maker.journal.Swap(trade.swapID)
	if err != nil {
		t.Fatal(err)
	} else if record.Phase != swapstate.PhaseExpired {
		t.Fatalf("stale agreement phase = %s (%s)", record.Phase, record.LastError)
	}
	if len(trade.qdayChain.outputs) != 0 || len(trade.bitcoinChain.txs) != 0 {
		t.Fatalf("stale agreement moved funds: qday=%d bitcoin=%d", len(trade.qdayChain.outputs), len(trade.bitcoinChain.txs))
	}
}

func TestEngineExpiresLateUnsubmittedTakerFunding(t *testing.T) {
	for _, test := range []struct {
		name, giveAsset, giveAmount, receiveAmount string
	}{
		{"taker prepared Bitcoin", "QDAY", "1", "0.0001"},
		{"taker prepared QDAY", "BTC", "0.0001", "1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			trade := negotiateEngineTrade(t, test.giveAsset, test.giveAmount, test.receiveAmount)
			record, err := trade.maker.journal.Swap(trade.swapID)
			if err != nil {
				t.Fatal(err)
			}
			agreement, err := agreementFromRecord(record)
			if err != nil {
				t.Fatal(err)
			}
			trade.qdayChain.mu.Lock()
			trade.qdayChain.height = agreement.QDAYRefundHeight - minimumQDAYFundingWindowBlocks + 1
			trade.qdayChain.mu.Unlock()
			trade.bitcoinChain.mu.Lock()
			trade.bitcoinChain.height = int32(agreement.BitcoinRefundHeight - minimumBitcoinFundingWindowBlocks + 1)
			trade.bitcoinChain.mu.Unlock()

			// The acceptance contains signed funding bytes, but neither participant
			// published them. Returning too close to the refund window must release
			// the reservation rather than broadcast a stale contract.
			trade.taker.driveSwaps(context.Background())
			record, err = trade.taker.journal.Swap(trade.swapID)
			if err != nil {
				t.Fatal(err)
			} else if record.Phase != swapstate.PhaseExpired || record.TakerFundingSubmitted {
				t.Fatalf("late unsubmitted taker funding = %#v", record)
			}
			if len(trade.qdayChain.outputs) != 0 || len(trade.bitcoinChain.txs) != 0 {
				t.Fatalf("late taker funding moved funds: qday=%d bitcoin=%d", len(trade.qdayChain.outputs), len(trade.bitcoinChain.txs))
			}
			if test.receiveAmount == "1" && len(trade.qdayChain.prepared) != 0 {
				t.Fatalf("expired QDAY acceptance retained %d prepared transaction(s)", len(trade.qdayChain.prepared))
			}
		})
	}
}
