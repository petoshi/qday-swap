package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
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
	mu      sync.Mutex
	height  uint64
	outputs map[string]*walletd.SwapOutput
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
	if output := q.chain.outputs[swapID]; output != nil {
		view.Outputs = []walletd.SwapOutput{*output}
		view.Status = output.Status
	}
	return view, nil
}
func (q *engineQDAY) FundSwap(_ context.Context, swapID string, request walletd.FundSwapRequest) (walletd.SwapAction, error) {
	q.chain.mu.Lock()
	defer q.chain.mu.Unlock()
	if existing := q.chain.outputs[swapID]; existing != nil {
		return walletd.SwapAction{TransactionID: existing.FundingTransaction, Status: "confirmed", Confirmations: existing.Confirmations}, nil
	}
	digest := sha256.Sum256([]byte(fmt.Sprintf("qday-funding:%s:%d", swapID, q.marker)))
	txid := hex.EncodeToString(digest[:])
	height := q.chain.height
	q.chain.outputs[swapID] = &walletd.SwapOutput{
		ID: hex.EncodeToString(digest[:]), Value: walletd.Amount{Atomic: request.AmountAtomic},
		FundingTransaction: txid, FundingHeight: &height, Confirmations: 3, Status: "funded",
	}
	return walletd.SwapAction{TransactionID: txid, Status: "confirmed", Confirmations: 3}, nil
}
func (q *engineQDAY) ClaimSwap(_ context.Context, swapID string, request walletd.SpendSwapRequest) (walletd.SwapAction, error) {
	q.chain.mu.Lock()
	defer q.chain.mu.Unlock()
	output := q.chain.outputs[swapID]
	if output == nil || output.ID != request.OutputID {
		return walletd.SwapAction{}, errors.New("unknown QDAY swap output")
	}
	digest := sha256.Sum256([]byte("qday-claim:" + swapID))
	height := q.chain.height
	output.Status, output.RevealedSecret, output.SpendTransaction, output.SpendHeight = "claimed", request.Secret, hex.EncodeToString(digest[:]), &height
	return walletd.SwapAction{TransactionID: output.SpendTransaction, Status: "confirmed", Confirmations: 1}, nil
}
func (q *engineQDAY) RefundSwap(_ context.Context, swapID string, request walletd.SpendSwapRequest) (walletd.SwapAction, error) {
	q.chain.mu.Lock()
	defer q.chain.mu.Unlock()
	output := q.chain.outputs[swapID]
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
		config: Config{Network: "mainnet", BitcoinNetwork: "mainnet", RelayURL: relayURL},
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
	qdayChain := &engineQDAYChain{height: 12_000, outputs: make(map[string]*walletd.SwapOutput)}
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

	maker.driveSwaps(context.Background())
	taker.driveSwaps(context.Background())
	maker.lastRelaySync, taker.lastRelaySync = time.Time{}, time.Time{}
	if err := maker.syncRelay(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := taker.syncRelay(context.Background()); err != nil {
		t.Fatal(err)
	}
	maker.driveSwaps(context.Background())
	taker.lastRelaySync = time.Time{}
	if err := taker.syncRelay(context.Background()); err != nil {
		t.Fatal(err)
	}
	taker.driveSwaps(context.Background())
	maker.lastRelaySync = time.Time{}
	if err := maker.syncRelay(context.Background()); err != nil {
		t.Fatal(err)
	}
	maker.driveSwaps(context.Background())

	makerRecord, err := maker.journal.Swap(makerSwap.ID)
	if err != nil {
		t.Fatal(err)
	}
	takerRecord, err := taker.journal.Swap(makerSwap.ID)
	if err != nil {
		t.Fatal(err)
	}
	if makerRecord.Phase != swapstate.PhaseTermsAgreed || takerRecord.Phase != swapstate.PhaseTermsAgreed {
		t.Fatalf("phases maker=%s (%s), taker=%s (%s)", makerRecord.Phase, makerRecord.LastError, takerRecord.Phase, takerRecord.LastError)
	}
	if makerRecord.AgreementJSON == "" || makerRecord.AgreementJSON != takerRecord.AgreementJSON || makerRecord.AgreementHash != takerRecord.AgreementHash {
		t.Fatalf("agreements differ\nmaker=%#v\ntaker=%#v", makerRecord, takerRecord)
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
	if makerQDAY.registered[makerSwapID].Role != makerQDAYRole || takerQDAY.registered[makerSwapID].Role != takerQDAYRole {
		t.Fatalf("QDAY roles maker=%q taker=%q", makerQDAY.registered[makerSwapID].Role, takerQDAY.registered[makerSwapID].Role)
	}

	if _, err := maker.ApproveSwap(makerSwapID); err != nil {
		t.Fatal(err)
	}
	taker.lastRelaySync = time.Time{}
	if err := taker.syncRelay(context.Background()); err != nil {
		t.Fatal(err)
	}
	taker.driveSwaps(context.Background())
	if _, err := taker.ApproveSwap(makerSwapID); err != nil {
		t.Fatal(err)
	}
	maker.lastRelaySync = time.Time{}
	if err := maker.syncRelay(context.Background()); err != nil {
		t.Fatal(err)
	}
	maker.driveSwaps(context.Background())
	taker.driveSwaps(context.Background())
	maker.driveSwaps(context.Background())

	makerRecord, _ := maker.journal.Swap(makerSwapID)
	takerRecord, _ := taker.journal.Swap(makerSwapID)
	if makerRecord.Phase != swapstate.PhaseComplete || takerRecord.Phase != swapstate.PhaseComplete {
		t.Fatalf("completed phases maker=%s (%s), taker=%s (%s)", makerRecord.Phase, makerRecord.LastError, takerRecord.Phase, takerRecord.LastError)
	}
	if makerRecord.RevealedSecret == "" || makerRecord.RevealedSecret != takerRecord.RevealedSecret {
		t.Fatalf("revealed secrets maker=%q taker=%q", makerRecord.RevealedSecret, takerRecord.RevealedSecret)
	}

	// Claims live on independent chains. If the first claim is reorganized
	// after the second one confirmed, both clients must notice and the maker
	// must replay the exact journaled claim instead of declaring the swap done.
	if giveAsset == "QDAY" {
		action, err := maker.journal.Action(makerSwapID + ":maker-claim")
		if err != nil {
			t.Fatal(err)
		}
		trade.bitcoinChain.mu.Lock()
		delete(trade.bitcoinChain.txs, action.TransactionID)
		trade.bitcoinChain.mu.Unlock()
	} else {
		trade.qdayChain.mu.Lock()
		output := trade.qdayChain.outputs[makerSwapID]
		output.Status, output.RevealedSecret, output.SpendTransaction, output.SpendHeight = "funded", "", "", nil
		trade.qdayChain.mu.Unlock()
	}
	taker.driveSwaps(context.Background())
	takerRecord, _ = taker.journal.Swap(makerSwapID)
	if takerRecord.Phase != swapstate.PhaseMakerClaiming {
		t.Fatalf("taker did not rewind missing maker claim: %s (%s)", takerRecord.Phase, takerRecord.LastError)
	}
	maker.driveSwaps(context.Background())
	maker.driveSwaps(context.Background())
	taker.driveSwaps(context.Background())
	makerRecord, _ = maker.journal.Swap(makerSwapID)
	takerRecord, _ = taker.journal.Swap(makerSwapID)
	if makerRecord.Phase != swapstate.PhaseComplete || takerRecord.Phase != swapstate.PhaseComplete {
		t.Fatalf("claim replay did not recover completion: maker=%s (%s), taker=%s (%s)", makerRecord.Phase, makerRecord.LastError, takerRecord.Phase, takerRecord.LastError)
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
			if _, err := trade.maker.ApproveSwap(trade.swapID); err != nil {
				t.Fatal(err)
			}
			trade.taker.lastRelaySync = time.Time{}
			if err := trade.taker.syncRelay(context.Background()); err != nil {
				t.Fatal(err)
			}
			trade.taker.driveSwaps(context.Background())
			makerRecord, err := trade.maker.journal.Swap(trade.swapID)
			if err != nil {
				t.Fatal(err)
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
				trade.qdayChain.mu.Lock()
				output := trade.qdayChain.outputs[trade.swapID]
				output.Status, output.SpendTransaction, output.SpendHeight = "funded", "", nil
				trade.qdayChain.mu.Unlock()
			} else {
				action, err := trade.maker.journal.Action(trade.swapID + ":maker-refund")
				if err != nil {
					t.Fatal(err)
				}
				trade.bitcoinChain.mu.Lock()
				delete(trade.bitcoinChain.txs, action.TransactionID)
				trade.bitcoinChain.mu.Unlock()
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
	} else if record.Phase != swapstate.PhaseMakerFunding || record.MakerFunding != "" {
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
	} else if record.MakerFunding == "" || record.Phase != swapstate.PhaseMakerFunded {
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
		output := trade.qdayChain.outputs[trade.swapID]
		if output == nil || output.FundingTransaction != notice.TransactionID {
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
			if _, err := trade.maker.ApproveSwap(trade.swapID); err != nil {
				t.Fatal(err)
			}
			trade.taker.lastRelaySync = time.Time{}
			if err := trade.taker.syncRelay(context.Background()); err != nil {
				t.Fatal(err)
			}
			trade.taker.driveSwaps(context.Background())
			if _, err := trade.taker.ApproveSwap(trade.swapID); err != nil {
				t.Fatal(err)
			}
			record, err := trade.taker.journal.Swap(trade.swapID)
			if err != nil {
				t.Fatal(err)
			} else if record.Phase != swapstate.PhaseMakerClaiming {
				t.Fatalf("taker did not reach pre-claim phase: %s (%s)", record.Phase, record.LastError)
			}

			setEngineFundingConfirmations(t, trade, record, "maker", 0)
			trade.taker.driveSwaps(context.Background())
			record, _ = trade.taker.journal.Swap(trade.swapID)
			if record.Phase != swapstate.PhaseMakerFunding {
				t.Fatalf("lost maker funding did not rewind before claim: %s (%s)", record.Phase, record.LastError)
			}
			if len(trade.qdayChain.outputs) > 1 || len(trade.bitcoinChain.txs) > 1 {
				t.Fatal("a claim was broadcast while maker funding was unconfirmed")
			}

			setEngineFundingConfirmations(t, trade, record, "maker", 3)
			trade.taker.driveSwaps(context.Background())
			trade.maker.lastRelaySync = time.Time{}
			if err := trade.maker.syncRelay(context.Background()); err != nil {
				t.Fatal(err)
			}
			for range 3 {
				trade.maker.driveSwaps(context.Background())
				trade.taker.driveSwaps(context.Background())
			}
			makerRecord, _ := trade.maker.journal.Swap(trade.swapID)
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
