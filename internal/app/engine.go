package app

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/petoshi/qday-swap/internal/bitcoin"
	"github.com/petoshi/qday-swap/internal/bitcoinwallet"
	swapprotocol "github.com/petoshi/qday-swap/internal/swap"
	"github.com/petoshi/qday-swap/internal/swapstate"
	"github.com/petoshi/qday-swap/internal/trade"
	"github.com/petoshi/qday-swap/internal/walletd"
	"github.com/petoshi/qday-swap/internal/walletroot"
)

const (
	engineInterval                    = 5 * time.Second
	engineFailureDisplayAttempts      = 3
	messageLifetime                   = 30 * 24 * time.Hour
	minimumQDAYFundingWindowBlocks    = uint64(360)
	minimumBitcoinFundingWindowBlocks = uint32(36)
)

var errFundingWindowClosed = errors.New("swap agreement is too close to its refund window")

type qdaySwapClient interface {
	Status(context.Context) (walletd.Status, error)
	CreateSwapKeys(context.Context, string) (walletd.SwapKeyView, error)
	RegisterSwap(context.Context, walletd.RegisterSwapRequest) (walletd.Swap, error)
	Swap(context.Context, string) (walletd.Swap, error)
	FundSwap(context.Context, string, walletd.FundSwapRequest) (walletd.SwapAction, error)
	ClaimSwap(context.Context, string, walletd.SpendSwapRequest) (walletd.SwapAction, error)
	RefundSwap(context.Context, string, walletd.SpendSwapRequest) (walletd.SwapAction, error)
}

type qdayAsyncSwapClient interface {
	qdaySwapClient
	PrepareSwapFunding(context.Context, string, walletd.FundSwapRequest) (walletd.SwapAction, error)
	CancelPreparedSwapFunding(context.Context, string) error
	ValidateTransactionPackage(context.Context, walletd.TransactionPackage) (walletd.TransactionPackage, error)
	ValidateSwapFundingPackage(context.Context, string, walletd.ValidateSwapFundingRequest) (walletd.TransactionPackage, error)
	BroadcastTransactionPackage(context.Context, walletd.TransactionPackage) (walletd.BroadcastPackageResult, error)
	PrepareSwapClaimTemplate(context.Context, string, walletd.SpendSwapRequest) (walletd.SwapAction, error)
	CompleteSwapClaimTemplate(context.Context, string, walletd.CompleteSwapClaimTemplateRequest) (walletd.TransactionPackage, error)
}

type bitcoinSwapClient interface {
	Status() (bitcoinwallet.Status, error)
	WatchContract(bitcoin.Contract) error
	PrepareFunding(bitcoin.Contract) (bitcoin.Funding, error)
	Broadcast(string) (bitcoinwallet.Transaction, error)
	Transaction(string) (bitcoinwallet.Transaction, error)
	FindSpend(bitcoin.Funding) (bitcoinwallet.Transaction, error)
	DestinationScript() ([]byte, error)
}

type bitcoinAsyncSwapClient interface {
	bitcoinSwapClient
	ReserveFunding(string) error
	ReleaseFunding(string) error
}

type engineWallets struct {
	qday    qdaySwapClient
	bitcoin bitcoinSwapClient
	root    walletroot.Root
}

type engineFailure struct {
	message  string
	attempts int
}

func (w *engineWallets) clear() { clear(w.root[:]) }

// recordEngineResult keeps short-lived retry errors out of the user-facing
// journal. Chain state can change between consecutive engine calls, so a
// failed automatic attempt is only actionable when the same failure survives
// several retries. A successful retry clears both the pending failure and any
// previously displayed error immediately.
func (s *Service) recordEngineResult(journal *swapstate.Journal, record swapstate.Swap, driveErr error) {
	if driveErr == nil {
		delete(s.engineFailures, record.ID)
		if record.LastError != "" {
			_, _ = journal.SetLastError(record.ID, "", time.Now().UTC())
		}
		return
	}

	message := driveErr.Error()
	if record.LastError == message {
		return
	}
	if record.LastError != "" {
		_, _ = journal.SetLastError(record.ID, "", time.Now().UTC())
	}
	if s.engineFailures == nil {
		s.engineFailures = make(map[string]engineFailure)
	}
	failure := s.engineFailures[record.ID]
	if failure.message != message {
		failure = engineFailure{message: message}
	}
	failure.attempts++
	s.engineFailures[record.ID] = failure
	if failure.attempts >= engineFailureDisplayAttempts {
		_, _ = journal.SetLastError(record.ID, message, time.Now().UTC())
	}
}

func (s *Service) startWorker() {
	workerCtx, cancel := context.WithCancel(s.ctx)
	s.workerCancel = cancel
	s.workerWG.Add(1)
	go func() {
		defer s.workerWG.Done()
		ticker := time.NewTicker(engineInterval)
		defer ticker.Stop()
		for {
			select {
			case <-workerCtx.Done():
				return
			case <-ticker.C:
				tickCtx, tickCancel := context.WithTimeout(workerCtx, 20*time.Second)
				_ = s.syncRelay(tickCtx)
				s.driveSwaps(tickCtx)
				tickCancel()
			}
		}
	}()
}

func (s *Service) engineWalletSnapshot() (engineWallets, *swapstate.Journal, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.root == nil {
		return engineWallets{}, nil, errors.New("wallet is locked")
	} else if s.client == nil || s.bitcoinClient == nil || s.journal == nil {
		return engineWallets{}, nil, errors.New("local swap services are unavailable")
	}
	qday, ok := s.client.(qdaySwapClient)
	if !ok {
		return engineWallets{}, nil, errors.New("QDAY wallet does not support atomic swaps")
	}
	bitcoinClient, ok := s.bitcoinClient.(bitcoinSwapClient)
	if !ok {
		return engineWallets{}, nil, errors.New("Bitcoin wallet does not support atomic swaps")
	}
	return engineWallets{qday: qday, bitcoin: bitcoinClient, root: *s.root}, s.journal, nil
}

// SwapDetails builds the role-aware, read-only chain evidence used by the
// local progress screen. It never exposes wallet key material or asks either
// wallet to sign or broadcast a transaction.
func (s *Service) SwapDetails(ctx context.Context, swapID string) (SwapDetails, error) {
	wallets, journal, err := s.engineWalletSnapshot()
	if err != nil {
		return SwapDetails{}, err
	}
	defer wallets.clear()
	record, err := journal.Swap(swapID)
	if err != nil {
		return SwapDetails{}, err
	}
	details := SwapDetails{Swap: record, Transactions: []SwapTransaction{}}
	if status, statusErr := wallets.qday.Status(ctx); statusErr == nil {
		details.QDAYHeight = status.Height
	}
	if status, statusErr := wallets.bitcoin.Status(); statusErr == nil && status.WalletHeight > 0 {
		details.BitcoinHeight = uint64(status.WalletHeight)
	}

	actions, err := journal.Actions(record.ID)
	if err != nil {
		return SwapDetails{}, err
	}
	var qdayView walletd.Swap
	qdayAvailable := false
	if view, viewErr := wallets.qday.Swap(ctx, localQDAYSwapID(record)); viewErr == nil {
		qdayView, qdayAvailable = view, true
		if view.Height > details.QDAYHeight {
			details.QDAYHeight = view.Height
		}
	}

	agreement, agreementErr := agreementFromRecord(record)
	for _, party := range []string{swapprotocol.PartyTaker, swapprotocol.PartyMaker} {
		notice, ok, noticeErr := fundingFromRecord(record, party)
		if noticeErr != nil {
			return SwapDetails{}, noticeErr
		} else if !ok {
			continue
		}
		transaction := SwapTransaction{
			Kind: "funding", Party: party, Asset: notice.Asset, TransactionID: notice.TransactionID,
			Status: "broadcast",
		}
		if agreementErr == nil {
			if notice.Asset == "QDAY" {
				transaction.AmountAtomic = agreement.QDAYAmountAtomic
			} else {
				transaction.AmountAtomic = strconv.FormatInt(agreement.BitcoinAmountSatoshi, 10)
			}
		}
		if party == swapprotocol.PartyTaker && record.Version == trade.AsyncProtocolVersion && !record.TakerFundingSubmitted {
			transaction.Status = "prepared"
		}
		if notice.Asset == "QDAY" && qdayAvailable {
			for _, output := range qdayView.Outputs {
				if output.FundingTransaction != notice.TransactionID {
					continue
				}
				transaction.AmountAtomic = output.Value.Atomic
				transaction.Confirmations = output.Confirmations
				if output.FundingHeight != nil {
					transaction.BlockHeight = *output.FundingHeight
				}
				transaction.Status = chainTransactionStatus(output.Confirmations)
				break
			}
		} else if notice.Asset == "BTC" && transaction.Status != "prepared" {
			if chainTransaction, lookupErr := wallets.bitcoin.Transaction(notice.TransactionID); lookupErr == nil {
				transaction.Confirmations = positiveConfirmations(chainTransaction.Confirmations)
				if chainTransaction.Height > 0 {
					transaction.BlockHeight = uint64(chainTransaction.Height)
				}
				transaction.Status = chainTransactionStatus(transaction.Confirmations)
			}
		}
		addSwapTransaction(&details.Transactions, transaction)
	}

	if qdayAvailable {
		for _, output := range qdayView.Outputs {
			if output.SpendTransaction == "" {
				continue
			}
			funder := fundingParty(record, "QDAY", output.FundingTransaction)
			if funder == "" {
				continue
			}
			kind, party := "claim", oppositeParty(funder)
			if output.Status == "refunded" {
				kind, party = "refund", funder
			}
			transaction := SwapTransaction{
				Kind: kind, Party: party, Asset: "QDAY", TransactionID: output.SpendTransaction,
				Status: "broadcast",
			}
			if output.SpendHeight != nil {
				transaction.BlockHeight = *output.SpendHeight
				if qdayView.Height >= *output.SpendHeight {
					transaction.Confirmations = qdayView.Height - *output.SpendHeight + 1
				}
				transaction.Status = "confirmed"
			}
			for _, action := range qdayView.Actions {
				if action.TransactionID == transaction.TransactionID {
					transaction.AmountAtomic = action.Amount.Atomic
					transaction.FeeAtomic = action.Fee.Atomic
					if action.Confirmations > transaction.Confirmations {
						transaction.Confirmations = action.Confirmations
					}
					if action.BlockHeight != nil {
						transaction.BlockHeight = *action.BlockHeight
					}
					transaction.Status = chainTransactionStatus(transaction.Confirmations)
					break
				}
			}
			addSwapTransaction(&details.Transactions, transaction)
		}
	}

	if agreementErr == nil {
		contract, contractErr := bitcoinContract(agreement)
		for _, party := range []string{swapprotocol.PartyTaker, swapprotocol.PartyMaker} {
			if contractErr != nil {
				break
			}
			notice, ok, noticeErr := fundingFromRecord(record, party)
			if noticeErr != nil {
				return SwapDetails{}, noticeErr
			} else if !ok || notice.Asset != "BTC" || notice.RawTransaction == "" {
				continue
			}
			funding, fundingErr := contract.FundingFromRaw(notice.RawTransaction)
			if fundingErr != nil {
				continue
			}
			spend, spendErr := wallets.bitcoin.FindSpend(funding)
			if spendErr != nil {
				continue
			}
			kind, spender := "claim", oppositeParty(party)
			if _, extractErr := contract.ExtractSecret(spend.Raw, funding); extractErr != nil {
				if refund, refundErr := contract.IsRefund(spend.Raw, funding); refundErr != nil || !refund {
					continue
				}
				kind, spender = "refund", party
			}
			transaction := SwapTransaction{
				Kind: kind, Party: spender, Asset: "BTC", TransactionID: spend.ID,
				Status:        chainTransactionStatus(positiveConfirmations(spend.Confirmations)),
				Confirmations: positiveConfirmations(spend.Confirmations),
			}
			if spend.Height > 0 {
				transaction.BlockHeight = uint64(spend.Height)
			}
			if decoded, decodeErr := bitcoin.DecodeTransaction(spend.Raw); decodeErr == nil {
				var outputTotal int64
				for _, output := range decoded.TxOut {
					outputTotal += output.Value
				}
				transaction.AmountAtomic = strconv.FormatInt(outputTotal, 10)
				if funding.Amount >= outputTotal {
					transaction.FeeAtomic = strconv.FormatInt(funding.Amount-outputTotal, 10)
				}
			}
			addSwapTransaction(&details.Transactions, transaction)
		}
	}

	// Prepared actions make an upcoming on-chain step visible before it reaches
	// either mempool. Observed chain transactions above always win.
	for _, action := range actions {
		kind, party := actionKindParty(action.Kind)
		if kind == "" || party == "" {
			continue
		}
		transaction := SwapTransaction{
			Kind: kind, Party: party, Asset: strings.ToUpper(action.Chain), TransactionID: action.TransactionID,
			Status: string(action.Status), BlockHeight: action.BlockHeight,
		}
		if action.Status == swapstate.ActionConfirmed {
			transaction.Confirmations = 1
		}
		addSwapTransaction(&details.Transactions, transaction)
	}
	return details, nil
}

func chainTransactionStatus(confirmations uint64) string {
	if confirmations > 0 {
		return "confirmed"
	}
	return "broadcast"
}

func positiveConfirmations(confirmations int32) uint64 {
	if confirmations > 0 {
		return uint64(confirmations)
	}
	return 0
}

func fundingParty(record swapstate.Swap, asset, transactionID string) string {
	for _, party := range []string{swapprotocol.PartyMaker, swapprotocol.PartyTaker} {
		notice, ok, err := fundingFromRecord(record, party)
		if err == nil && ok && notice.Asset == asset && notice.TransactionID == transactionID {
			return party
		}
	}
	return ""
}

func actionKindParty(kind string) (string, string) {
	party := swapprotocol.PartyMaker
	if strings.HasPrefix(kind, swapprotocol.PartyTaker+"-") {
		party = swapprotocol.PartyTaker
	} else if !strings.HasPrefix(kind, swapprotocol.PartyMaker+"-") {
		return "", ""
	}
	if strings.Contains(kind, "funding") {
		return "funding", party
	} else if strings.Contains(kind, "claim") {
		return "claim", party
	} else if strings.Contains(kind, "refund") {
		return "refund", party
	}
	return "", ""
}

func addSwapTransaction(transactions *[]SwapTransaction, next SwapTransaction) {
	if next.TransactionID == "" {
		return
	}
	for index, current := range *transactions {
		if current.Kind != next.Kind || current.Party != next.Party || current.Asset != next.Asset {
			continue
		}
		nextRank, currentRank := swapTransactionRank(next.Status), swapTransactionRank(current.Status)
		if nextRank > currentRank || (nextRank == currentRank && next.Confirmations > current.Confirmations) {
			(*transactions)[index] = next
		} else {
			if current.AmountAtomic == "" && next.AmountAtomic != "" {
				(*transactions)[index].AmountAtomic = next.AmountAtomic
			}
			if current.FeeAtomic == "" && next.FeeAtomic != "" {
				(*transactions)[index].FeeAtomic = next.FeeAtomic
			}
			if current.BlockHeight == 0 && next.BlockHeight != 0 {
				(*transactions)[index].BlockHeight = next.BlockHeight
			}
		}
		return
	}
	*transactions = append(*transactions, next)
}

func swapTransactionRank(status string) int {
	switch status {
	case "confirmed":
		return 3
	case "broadcast":
		return 2
	case "prepared":
		return 1
	default:
		return 0
	}
}

func (s *Service) driveSwaps(ctx context.Context) {
	if !s.engineMu.TryLock() {
		return
	}
	defer s.engineMu.Unlock()
	wallets, journal, err := s.engineWalletSnapshot()
	if err != nil {
		return
	}
	defer wallets.clear()
	records, err := journal.List()
	if err != nil {
		return
	}
	for _, record := range records {
		s.recordEngineResult(journal, record, s.driveSwap(ctx, wallets, journal, record))
	}
}

func (s *Service) driveSwap(ctx context.Context, wallets engineWallets, journal *swapstate.Journal, record swapstate.Swap) error {
	if record.Version == trade.AsyncProtocolVersion {
		return s.driveAsyncSwap(ctx, wallets, journal, record)
	}
	if err := s.ensureLocalHello(ctx, wallets, journal, record); err != nil {
		return err
	}
	if err := s.processPeerMessages(ctx, wallets, journal, record.ID); err != nil {
		return err
	}
	record, err := journal.Swap(record.ID)
	if err != nil {
		return err
	}
	if record.Role == swapstate.RoleMaker && record.Phase == swapstate.PhaseMatched && record.PeerHelloJSON != "" {
		if err := s.proposeAgreement(ctx, wallets, journal, record); err != nil {
			return err
		}
	}
	// Process a possible acknowledgement already stored by the mailbox poll
	// after the maker created its agreement during this tick.
	if err := s.processPeerMessages(ctx, wallets, journal, record.ID); err != nil {
		return err
	}
	return s.driveExecution(ctx, wallets, journal, record.ID)
}

func (s *Service) ensureLocalHello(ctx context.Context, wallets engineWallets, journal *swapstate.Journal, record swapstate.Swap) error {
	if record.LocalHelloJSON == "" {
		keys, err := wallets.qday.CreateSwapKeys(ctx, record.ID)
		if err != nil {
			return fmt.Errorf("create QDAY swap keys: %w", err)
		}
		qdayStatus, err := s.client.Status(ctx)
		if err != nil {
			return fmt.Errorf("read QDAY swap height: %w", err)
		}
		bitcoinStatus, err := s.bitcoinClient.Status()
		if err != nil {
			return fmt.Errorf("read Bitcoin swap height: %w", err)
		} else if bitcoinStatus.WalletHeight < 0 || uint64(bitcoinStatus.WalletHeight) > math.MaxUint32 {
			return errors.New("Bitcoin wallet height is outside the supported range")
		}
		key, err := wallets.root.BitcoinSwapKey(record.ID)
		if err != nil {
			return err
		}
		party := swapprotocol.PartyMaker
		if record.Role == swapstate.RoleTaker {
			party = swapprotocol.PartyTaker
		}
		hello, err := swapprotocol.NewHello(
			s.config.Network, record.Order.ID, record.ID, party,
			swapprotocol.QDAYKeys{Classical: keys.Keys.Classical, Reserve: keys.Keys.Reserve, Address: keys.Keys.Address},
			key.PubKey().SerializeCompressed(), qdayStatus.Height, uint32(bitcoinStatus.WalletHeight),
		)
		if err != nil {
			return err
		}
		encoded, _ := json.Marshal(hello)
		if _, err := journal.SetHello(record.ID, true, string(encoded), time.Now().UTC()); err != nil {
			return err
		}
		record, err = journal.Swap(record.ID)
		if err != nil {
			return err
		}
	}
	var hello swapprotocol.Hello
	if err := decodeStrictJSON([]byte(record.LocalHelloJSON), &hello); err != nil {
		return fmt.Errorf("decode local swap hello: %w", err)
	}
	message := swapprotocol.Message{Version: swapprotocol.ProtocolVersion, Kind: swapprotocol.MessageHello, TradeID: record.ID, Hello: &hello}
	return s.sendProtocolMessage(ctx, wallets.root, journal, record, "hello", message)
}

type decodedPeerMessage struct {
	envelope trade.SignedMessage
	payload  swapprotocol.Message
}

func (s *Service) processPeerMessages(ctx context.Context, wallets engineWallets, journal *swapstate.Journal, swapID string) error {
	record, err := journal.Swap(swapID)
	if err != nil {
		return err
	}
	messages, err := journal.RelayMessages(swapID)
	if err != nil {
		return err
	}
	localPublic, peerPublic, messagePrivate, peerMessagePublic, err := messageKeys(wallets.root, record)
	if err != nil {
		return err
	}
	defer clear(messagePrivate[:])
	decoded := make([]decodedPeerMessage, 0, len(messages))
	for _, envelope := range messages {
		if envelope.Message.SenderPublicKey == hex.EncodeToString(localPublic) {
			continue
		}
		if envelope.Message.SenderPublicKey != hex.EncodeToString(peerPublic) ||
			envelope.Message.RecipientPublicKey != hex.EncodeToString(localPublic) ||
			envelope.Message.Network != s.config.Network || envelope.Message.OrderID != record.Order.ID {
			return errors.New("encrypted message participants or trade context do not match")
		}
		plaintext, err := envelope.Decrypt(localPublic, messagePrivate, peerMessagePublic, time.Unix(envelope.Message.CreatedAt, 0))
		if err != nil {
			return fmt.Errorf("decrypt swap message: %w", err)
		}
		var payload swapprotocol.Message
		if err := decodeStrictJSON(plaintext, &payload); err != nil {
			return fmt.Errorf("decode swap message: %w", err)
		} else if err := payload.ValidateBasic(); err != nil {
			return err
		} else if payload.TradeID != swapID {
			return errors.New("decrypted message belongs to another trade")
		}
		decoded = append(decoded, decodedPeerMessage{envelope: envelope, payload: payload})
	}
	for _, message := range decoded {
		record, err = journal.Swap(swapID)
		if err != nil {
			return err
		}
		switch message.payload.Kind {
		case swapprotocol.MessageHello:
			party := swapprotocol.PartyTaker
			if record.Role == swapstate.RoleTaker {
				party = swapprotocol.PartyMaker
			}
			if err := message.payload.Hello.Validate(s.config.Network, record.Order.ID, swapID, party); err != nil {
				return err
			}
			encoded, _ := json.Marshal(message.payload.Hello)
			if _, err := journal.SetHello(swapID, false, string(encoded), time.Now().UTC()); err != nil {
				return err
			}
		case swapprotocol.MessageAgreement:
			if record.Role != swapstate.RoleTaker {
				return errors.New("taker agreement was sent to the maker")
			}
			if err := s.acceptAgreement(ctx, wallets, journal, record, *message.payload.Agreement); err != nil {
				return err
			}
		case swapprotocol.MessageAgreementAck:
			if record.Role != swapstate.RoleMaker {
				return errors.New("maker agreement acknowledgement was sent to the taker")
			}
			if record.AgreementHash == "" || message.payload.AgreementHash != record.AgreementHash {
				return errors.New("peer acknowledged another swap agreement")
			}
			if record.Phase == swapstate.PhaseTermsProposed {
				if _, err := journal.Advance(swapID, swapstate.PhaseTermsProposed, swapstate.PhaseTermsAgreed, time.Now().UTC()); err != nil {
					return err
				}
			}
		case swapprotocol.MessageFunding:
			if err := s.acceptFundingNotice(ctx, wallets, journal, record, *message.payload.Funding); err != nil {
				return err
			}
		case swapprotocol.MessageClaimTemplate:
			if err := acceptMakerClaimTemplate(ctx, wallets, journal, record, *message.payload.ClaimTemplate); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Service) proposeAgreement(ctx context.Context, wallets engineWallets, journal *swapstate.Journal, record swapstate.Swap) error {
	local, peer, err := decodeHellos(record)
	if err != nil {
		return err
	}
	secret, secretHash, err := wallets.root.SwapSecret(record.ID)
	clear(secret[:])
	if err != nil {
		return err
	}
	agreement, err := swapprotocol.BuildAgreement(record.Order, record.ID, hex.EncodeToString(secretHash[:]), local, peer)
	if err != nil {
		return err
	}
	if err := registerContracts(ctx, wallets, record, agreement); err != nil {
		return err
	}
	agreementHash, err := agreement.Hash()
	if err != nil {
		return err
	}
	encoded, _ := json.Marshal(agreement)
	if _, err := journal.SetAgreementDetails(record.ID, agreement.SecretHash, agreementHash, string(encoded), swapstate.PhaseMatched, time.Now().UTC()); err != nil {
		return err
	}
	if _, err := journal.Advance(record.ID, swapstate.PhaseMatched, swapstate.PhaseTermsProposed, time.Now().UTC()); err != nil {
		return err
	}
	record, err = journal.Swap(record.ID)
	if err != nil {
		return err
	}
	message := swapprotocol.Message{Version: swapprotocol.ProtocolVersion, Kind: swapprotocol.MessageAgreement, TradeID: record.ID, Agreement: &agreement}
	return s.sendProtocolMessage(ctx, wallets.root, journal, record, "agreement", message)
}

func (s *Service) acceptAgreement(ctx context.Context, wallets engineWallets, journal *swapstate.Journal, record swapstate.Swap, agreement swapprotocol.Agreement) error {
	if record.PeerHelloJSON == "" {
		return errors.New("maker agreement arrived before its hello")
	}
	local, peer, err := decodeHellos(record)
	if err != nil {
		return err
	}
	maker, taker := peer, local
	if err := agreement.Validate(record.Order, maker, taker); err != nil {
		return err
	}
	hash, err := agreement.Hash()
	if err != nil {
		return err
	}
	encoded, _ := json.Marshal(agreement)
	if record.AgreementJSON == "" {
		if err := registerContracts(ctx, wallets, record, agreement); err != nil {
			return err
		}
		if _, err := journal.SetAgreementDetails(record.ID, agreement.SecretHash, hash, string(encoded), swapstate.PhaseMatched, time.Now().UTC()); err != nil {
			return err
		}
		if _, err := journal.Advance(record.ID, swapstate.PhaseMatched, swapstate.PhaseTermsProposed, time.Now().UTC()); err != nil {
			return err
		}
	} else if record.AgreementHash != hash || record.AgreementJSON != string(encoded) {
		return errors.New("maker changed an accepted swap agreement")
	}
	record, err = journal.Swap(record.ID)
	if err != nil {
		return err
	}
	ack := swapprotocol.Message{Version: swapprotocol.ProtocolVersion, Kind: swapprotocol.MessageAgreementAck, TradeID: record.ID, AgreementHash: hash}
	if err := s.sendProtocolMessage(ctx, wallets.root, journal, record, "agreement_ack", ack); err != nil {
		return err
	}
	if record.Phase == swapstate.PhaseTermsProposed {
		_, err = journal.Advance(record.ID, swapstate.PhaseTermsProposed, swapstate.PhaseTermsAgreed, time.Now().UTC())
	}
	return err
}

func registerContracts(ctx context.Context, wallets engineWallets, record swapstate.Swap, agreement swapprotocol.Agreement) error {
	local, peer, err := decodeHellos(record)
	if err != nil {
		return err
	}
	localGivesQDAY := (record.Role == swapstate.RoleMaker && agreement.MakerGives == "QDAY") ||
		(record.Role == swapstate.RoleTaker && agreement.MakerGives == "BTC")
	role := "recipient"
	if localGivesQDAY {
		role = "refund"
	}
	if _, err := wallets.qday.RegisterSwap(ctx, walletd.RegisterSwapRequest{
		SwapID: record.ID, Role: role,
		Counterparty: walletd.SwapKeys{Classical: peer.QDAY.Classical, Reserve: peer.QDAY.Reserve, Address: peer.QDAY.Address},
		SecretHash:   agreement.SecretHash, RefundHeight: agreement.QDAYRefundHeight,
	}); err != nil {
		return fmt.Errorf("register QDAY contract: %w", err)
	}
	contract, err := bitcoinContract(agreement)
	if err != nil {
		return err
	}
	if err := wallets.bitcoin.WatchContract(contract); err != nil {
		return fmt.Errorf("watch Bitcoin contract: %w", err)
	}
	_ = local // Local keys are included in walletd's deterministic registration.
	return nil
}

func bitcoinContract(agreement swapprotocol.Agreement) (bitcoin.Contract, error) {
	secretHash, err := decode32(agreement.SecretHash, "swap secret hash")
	if err != nil {
		return bitcoin.Contract{}, err
	}
	makerKey, err := hex.DecodeString(agreement.Maker.BitcoinPublicKey)
	if err != nil {
		return bitcoin.Contract{}, err
	}
	takerKey, err := hex.DecodeString(agreement.Taker.BitcoinPublicKey)
	if err != nil {
		return bitcoin.Contract{}, err
	}
	recipient, refund := makerKey, takerKey
	if agreement.MakerGives == "BTC" {
		recipient, refund = takerKey, makerKey
	}
	return bitcoin.NewContract(agreement.BitcoinAmountSatoshi, secretHash, recipient, refund, agreement.BitcoinRefundHeight)
}

func (s *Service) sendProtocolMessage(ctx context.Context, root walletroot.Root, journal *swapstate.Journal, record swapstate.Swap, messageKey string, payload swapprotocol.Message) error {
	if existingID := record.SentMessages[messageKey]; existingID != "" {
		existing, err := journal.RelayMessage(existingID)
		if err != nil {
			return err
		}
		_, err = s.relay.Message(ctx, existing)
		return err
	}
	localPublic, peerPublic, messagePrivate, peerMessagePublic, err := messageKeys(root, record)
	if err != nil {
		return err
	}
	defer clear(messagePrivate[:])
	identity, err := root.OrderIdentity()
	if err != nil {
		return err
	}
	defer clear(identity)
	if !bytes.Equal(identity.Public().(ed25519.PublicKey), localPublic) {
		return errors.New("local order identity does not match the selected trade")
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	messages, err := journal.RelayMessages(record.ID)
	if err != nil {
		return err
	}
	sequence := uint64(1)
	localHex := hex.EncodeToString(localPublic)
	for _, message := range messages {
		if message.Message.SenderPublicKey == localHex && message.Message.Sequence >= sequence {
			sequence = message.Message.Sequence + 1
		}
	}
	now := time.Now().UTC()
	envelope, err := trade.EncryptMessage(
		s.config.Network, record.Order.ID, record.ID, sequence,
		identity, peerPublic, messagePrivate, peerMessagePublic,
		encoded, messageLifetime, now,
	)
	if err != nil {
		return err
	}
	envelope, _, err = journal.SaveOutboundMessage(record.ID, messageKey, envelope, now)
	if err != nil {
		return err
	}
	_, err = s.relay.Message(ctx, envelope)
	return err
}

func messageKeys(root walletroot.Root, record swapstate.Swap) (localPublic, peerPublic ed25519.PublicKey, localMessagePrivate, peerMessagePublic [32]byte, err error) {
	identity, err := root.OrderIdentity()
	if err != nil {
		return nil, nil, localMessagePrivate, peerMessagePublic, err
	}
	defer clear(identity)
	localPublic = append(ed25519.PublicKey(nil), identity.Public().(ed25519.PublicKey)...)
	localMessagePrivate, _, err = root.MessageIdentity()
	if err != nil {
		return nil, nil, localMessagePrivate, peerMessagePublic, err
	}
	peerSigningHex, peerMessageHex := record.Acceptance.Acceptance.TakerPublicKey, record.Acceptance.Acceptance.TakerMessageKey
	if record.Role == swapstate.RoleTaker {
		peerSigningHex, peerMessageHex = record.Order.Order.MakerPublicKey, record.Order.Order.MakerMessageKey
	}
	peerPublic, err = decodeEd25519(peerSigningHex)
	if err != nil {
		clear(localMessagePrivate[:])
		return nil, nil, localMessagePrivate, peerMessagePublic, err
	}
	peerMessagePublic, err = trade.DecodeMessageKey(peerMessageHex)
	if err != nil {
		clear(localMessagePrivate[:])
		return nil, nil, localMessagePrivate, peerMessagePublic, err
	}
	return localPublic, peerPublic, localMessagePrivate, peerMessagePublic, nil
}

func decodeHellos(record swapstate.Swap) (local, peer swapprotocol.Hello, err error) {
	if record.LocalHelloJSON == "" || record.PeerHelloJSON == "" {
		return local, peer, errors.New("both swap participant descriptors are required")
	}
	if err = decodeStrictJSON([]byte(record.LocalHelloJSON), &local); err != nil {
		return local, peer, err
	}
	if err = decodeStrictJSON([]byte(record.PeerHelloJSON), &peer); err != nil {
		return local, peer, err
	}
	return local, peer, nil
}

func decodeStrictJSON(encoded []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("JSON payload contains multiple values")
	}
	return nil
}

func decodeEd25519(encoded string) (ed25519.PublicKey, error) {
	decoded, err := hex.DecodeString(encoded)
	if err != nil || len(decoded) != ed25519.PublicKeySize || encoded != strings.ToLower(encoded) {
		return nil, errors.New("participant signing key is invalid")
	}
	return ed25519.PublicKey(decoded), nil
}

func decode32(encoded, name string) ([32]byte, error) {
	var result [32]byte
	decoded, err := hex.DecodeString(encoded)
	if err != nil || len(decoded) != len(result) || encoded != strings.ToLower(encoded) {
		return result, fmt.Errorf("%s is invalid", name)
	}
	copy(result[:], decoded)
	return result, nil
}

// ApproveSwap remains an idempotent recovery path for journals created by
// versions which required a second browser confirmation. New swaps are
// authorized by the maker's signed offer or the taker's signed acceptance.
func (s *Service) ApproveSwap(id string) (swapstate.Swap, error) {
	s.mu.RLock()
	journal := s.journal
	unlocked := s.root != nil
	s.mu.RUnlock()
	if !unlocked {
		return swapstate.Swap{}, errors.New("wallet is locked")
	} else if journal == nil {
		return swapstate.Swap{}, errors.New("swap journal is unavailable")
	}
	record, err := journal.Approve(id, time.Now().UTC())
	if err != nil {
		return swapstate.Swap{}, err
	}
	ctx, cancel := context.WithTimeout(s.ctx, 20*time.Second)
	s.driveSwaps(ctx)
	cancel()
	return record, nil
}

func (s *Service) acceptFundingNotice(ctx context.Context, wallets engineWallets, journal *swapstate.Journal, record swapstate.Swap, notice swapprotocol.FundingNotice) error {
	if record.AgreementJSON == "" {
		return errors.New("funding notice arrived before the agreement")
	}
	expectedParty := swapprotocol.PartyTaker
	if record.Role == swapstate.RoleTaker {
		expectedParty = swapprotocol.PartyMaker
	}
	if notice.Party != expectedParty {
		return errors.New("peer sent a funding notice for the local participant")
	}
	agreement, err := agreementFromRecord(record)
	if err != nil {
		return err
	}
	if notice.Asset != assetFundedBy(agreement, notice.Party) {
		return errors.New("funding notice asset does not match the agreement")
	}
	if notice.Asset == "BTC" {
		contract, err := bitcoinContract(agreement)
		if err != nil {
			return err
		}
		funding, err := contract.FundingFromRaw(notice.RawTransaction)
		if err != nil {
			return fmt.Errorf("verify peer Bitcoin funding: %w", err)
		} else if funding.TxID != notice.TransactionID {
			return errors.New("Bitcoin funding transaction ID does not match its signed bytes")
		}
		if _, err := wallets.bitcoin.Broadcast(notice.RawTransaction); err != nil {
			return fmt.Errorf("relay peer Bitcoin funding: %w", err)
		}
	}
	encoded, _ := json.Marshal(notice)
	_, err = journal.SetFunding(record.ID, notice.Party, string(encoded), time.Now().UTC())
	return err
}

func (s *Service) driveExecution(ctx context.Context, wallets engineWallets, journal *swapstate.Journal, swapID string) error {
	for range 12 {
		record, err := journal.Swap(swapID)
		if err != nil {
			return err
		}
		if record.AgreementJSON == "" || record.Phase == swapstate.PhaseMatched || record.Phase == swapstate.PhaseTermsProposed {
			return nil
		}
		agreement, err := agreementFromRecord(record)
		if err != nil {
			return err
		}
		// Publishing an exact signed offer and explicitly accepting one are the
		// two user authorizations for an automatic swap. By this point both
		// participant descriptors, amounts, refund heights and the immutable
		// agreement have been validated. No second browser click is required.
		if !record.Approved && !s.config.RequireSwapApproval {
			switch record.Phase {
			case swapstate.PhaseTermsAgreed, swapstate.PhaseMakerFunding, swapstate.PhaseMakerFunded, swapstate.PhaseTakerFunding, swapstate.PhaseTakerFunded:
				record, err = journal.Approve(swapID, time.Now().UTC())
				if err != nil {
					return err
				}
			}
		}
		if record.Phase != swapstate.PhaseComplete && record.Phase != swapstate.PhaseRefunded && record.Phase != swapstate.PhaseExpired {
			refunding, err := s.maybeStartRefund(ctx, wallets, journal, record, agreement)
			if err != nil {
				return err
			} else if refunding {
				continue
			}
		}

		switch record.Phase {
		case swapstate.PhaseTermsAgreed:
			if record.Role == swapstate.RoleMaker && record.Approved {
				if _, err := journal.Advance(swapID, record.Phase, swapstate.PhaseMakerFunding, time.Now().UTC()); err != nil {
					return err
				}
				continue
			}
			if record.Role == swapstate.RoleTaker && record.MakerFunding != "" {
				if _, err := journal.Advance(swapID, record.Phase, swapstate.PhaseMakerFunding, time.Now().UTC()); err != nil {
					return err
				}
				continue
			}
			return nil

		case swapstate.PhaseMakerFunding:
			if record.Role == swapstate.RoleMaker {
				if !record.Approved {
					return nil
				}
				if err := s.ensureLocalFunding(ctx, wallets, journal, record, agreement, swapprotocol.PartyMaker); err != nil {
					return err
				}
				record, _ = journal.Swap(swapID)
			}
			notice, ok, err := fundingFromRecord(record, swapprotocol.PartyMaker)
			if err != nil || !ok {
				return err
			}
			confirmed, err := fundingConfirmed(ctx, wallets, agreement, notice)
			if err != nil {
				return err
			} else if !confirmed {
				return nil
			}
			if _, err := journal.Advance(swapID, record.Phase, swapstate.PhaseMakerFunded, time.Now().UTC()); err != nil {
				return err
			}
			continue

		case swapstate.PhaseMakerFunded:
			notice, ok, err := fundingFromRecord(record, swapprotocol.PartyMaker)
			if err != nil {
				return err
			}
			if ok {
				confirmed, err := fundingConfirmed(ctx, wallets, agreement, notice)
				if err != nil {
					return err
				} else if !confirmed {
					_, err = journal.RewindAfterReorg(swapID, record.Phase, swapstate.PhaseMakerFunding, "maker funding lost confirmations", time.Now().UTC())
					return err
				}
			}
			if record.Role == swapstate.RoleTaker && record.Approved {
				if _, err := journal.Advance(swapID, record.Phase, swapstate.PhaseTakerFunding, time.Now().UTC()); err != nil {
					return err
				}
				continue
			}
			if record.Role == swapstate.RoleMaker && record.TakerFunding != "" {
				if _, err := journal.Advance(swapID, record.Phase, swapstate.PhaseTakerFunding, time.Now().UTC()); err != nil {
					return err
				}
				continue
			}
			return nil

		case swapstate.PhaseTakerFunding:
			confirmed, err := partyFundingConfirmed(ctx, wallets, agreement, record, swapprotocol.PartyMaker)
			if err != nil {
				return err
			} else if !confirmed {
				if _, err := journal.RewindAfterReorg(swapID, record.Phase, swapstate.PhaseMakerFunding, "maker funding lost confirmations", time.Now().UTC()); err != nil {
					return err
				}
				continue
			}
			if record.Role == swapstate.RoleTaker {
				if !record.Approved {
					return nil
				}
				if err := s.ensureLocalFunding(ctx, wallets, journal, record, agreement, swapprotocol.PartyTaker); err != nil {
					return err
				}
				record, _ = journal.Swap(swapID)
			}
			notice, ok, err := fundingFromRecord(record, swapprotocol.PartyTaker)
			if err != nil || !ok {
				return err
			}
			confirmed, err = fundingConfirmed(ctx, wallets, agreement, notice)
			if err != nil {
				return err
			} else if !confirmed {
				return nil
			}
			if _, err := journal.Advance(swapID, record.Phase, swapstate.PhaseTakerFunded, time.Now().UTC()); err != nil {
				return err
			}
			continue

		case swapstate.PhaseTakerFunded:
			confirmed, err := partyFundingConfirmed(ctx, wallets, agreement, record, swapprotocol.PartyMaker)
			if err != nil {
				return err
			} else if !confirmed {
				if _, err := journal.RewindAfterReorg(swapID, record.Phase, swapstate.PhaseMakerFunding, "maker funding lost confirmations", time.Now().UTC()); err != nil {
					return err
				}
				continue
			}
			notice, ok, err := fundingFromRecord(record, swapprotocol.PartyTaker)
			if err != nil {
				return err
			}
			if ok {
				confirmed, err := fundingConfirmed(ctx, wallets, agreement, notice)
				if err != nil {
					return err
				} else if !confirmed {
					_, err = journal.RewindAfterReorg(swapID, record.Phase, swapstate.PhaseTakerFunding, "taker funding lost confirmations", time.Now().UTC())
					return err
				}
			}
			if _, err := journal.Advance(swapID, record.Phase, swapstate.PhaseMakerClaiming, time.Now().UTC()); err != nil {
				return err
			}
			continue

		case swapstate.PhaseMakerClaiming:
			confirmed, err := partyFundingConfirmed(ctx, wallets, agreement, record, swapprotocol.PartyMaker)
			if err != nil {
				return err
			} else if !confirmed {
				if _, err := journal.RewindAfterReorg(swapID, record.Phase, swapstate.PhaseMakerFunding, "maker funding lost confirmations before claim", time.Now().UTC()); err != nil {
					return err
				}
				continue
			}
			confirmed, err = partyFundingConfirmed(ctx, wallets, agreement, record, swapprotocol.PartyTaker)
			if err != nil {
				return err
			} else if !confirmed {
				if _, err := journal.RewindAfterReorg(swapID, record.Phase, swapstate.PhaseTakerFunding, "taker funding lost confirmations before claim", time.Now().UTC()); err != nil {
					return err
				}
				continue
			}
			if record.Role == swapstate.RoleMaker {
				if err := ensureLocalClaim(ctx, wallets, journal, record, agreement, swapprotocol.PartyMaker); err != nil {
					return err
				}
			}
			found, _, secret, err := observeClaim(ctx, wallets, agreement, swapprotocol.PartyMaker, record)
			if err != nil {
				return err
			} else if !found {
				return nil
			}
			if _, err := journal.SetRevealedSecret(swapID, secret, time.Now().UTC()); err != nil {
				return err
			}
			if _, err := journal.Advance(swapID, record.Phase, swapstate.PhaseMakerClaimed, time.Now().UTC()); err != nil {
				return err
			}
			continue

		case swapstate.PhaseMakerClaimed:
			found, _, _, err := observeClaim(ctx, wallets, agreement, swapprotocol.PartyMaker, record)
			if err != nil {
				return err
			} else if !found {
				if _, err := journal.RewindAfterReorg(swapID, record.Phase, swapstate.PhaseMakerClaiming, "maker claim disappeared", time.Now().UTC()); err != nil {
					return err
				}
				continue
			}
			if record.Role == swapstate.RoleTaker {
				if _, err := journal.Advance(swapID, record.Phase, swapstate.PhaseTakerClaiming, time.Now().UTC()); err != nil {
					return err
				}
				continue
			}
			found, _, _, err = observeClaim(ctx, wallets, agreement, swapprotocol.PartyTaker, record)
			if err != nil {
				return err
			} else if !found {
				return nil
			}
			if _, err := journal.Advance(swapID, record.Phase, swapstate.PhaseTakerClaiming, time.Now().UTC()); err != nil {
				return err
			}
			continue

		case swapstate.PhaseTakerClaiming:
			found, _, _, err := observeClaim(ctx, wallets, agreement, swapprotocol.PartyMaker, record)
			if err != nil {
				return err
			} else if !found {
				if _, err := journal.RewindAfterReorg(swapID, record.Phase, swapstate.PhaseMakerClaiming, "maker claim disappeared", time.Now().UTC()); err != nil {
					return err
				}
				continue
			}
			if record.Role == swapstate.RoleTaker {
				if err := ensureLocalClaim(ctx, wallets, journal, record, agreement, swapprotocol.PartyTaker); err != nil {
					return err
				}
			}
			found, confirmed, _, err := observeClaim(ctx, wallets, agreement, swapprotocol.PartyTaker, record)
			if err != nil {
				return err
			} else if !found || !confirmed {
				return nil
			}
			if _, err := journal.Advance(swapID, record.Phase, swapstate.PhaseComplete, time.Now().UTC()); err != nil {
				return err
			}
			continue

		case swapstate.PhaseComplete:
			found, _, _, err := observeClaim(ctx, wallets, agreement, swapprotocol.PartyMaker, record)
			if err != nil {
				return err
			} else if !found {
				_, err = journal.RewindAfterReorg(swapID, record.Phase, swapstate.PhaseMakerClaiming, "maker claim disappeared", time.Now().UTC())
				return err
			}
			found, confirmed, _, err := observeClaim(ctx, wallets, agreement, swapprotocol.PartyTaker, record)
			if err != nil {
				return err
			} else if !found || !confirmed {
				_, err = journal.RewindAfterReorg(swapID, record.Phase, swapstate.PhaseTakerClaiming, "final claim lost confirmations", time.Now().UTC())
				return err
			}
			return nil

		case swapstate.PhaseWaitingForRefund:
			if _, err := journal.Advance(swapID, record.Phase, swapstate.PhaseRefunding, time.Now().UTC()); err != nil {
				return err
			}
			continue

		case swapstate.PhaseRefunding:
			confirmed, err := ensureLocalRefund(ctx, wallets, journal, record, agreement)
			if err != nil {
				return err
			} else if !confirmed {
				return nil
			}
			if _, err := journal.Advance(swapID, record.Phase, swapstate.PhaseRefunded, time.Now().UTC()); err != nil {
				return err
			}
			continue

		case swapstate.PhaseRefunded:
			confirmed, err := refundStateConfirmed(ctx, wallets, record, agreement)
			if err != nil {
				return err
			} else if !confirmed {
				_, err = journal.RewindAfterReorg(swapID, record.Phase, swapstate.PhaseRefunding, "refund lost confirmations", time.Now().UTC())
				return err
			}
			return nil

		case swapstate.PhaseExpired:
			return nil
		}
	}
	return errors.New("swap engine exceeded its transition limit")
}

func (s *Service) ensureLocalFunding(ctx context.Context, wallets engineWallets, journal *swapstate.Journal, record swapstate.Swap, agreement swapprotocol.Agreement, party string) error {
	if existing, ok, err := fundingFromRecord(record, party); err != nil {
		return err
	} else if ok {
		message := swapprotocol.Message{Version: asyncMessageVersion(record), Kind: swapprotocol.MessageFunding, TradeID: record.ID, Funding: &existing}
		return s.sendProtocolMessage(ctx, wallets.root, journal, record, "funding:"+party, message)
	}
	if err := ensureFundingWindows(ctx, wallets, agreement); err != nil {
		return err
	}
	asset := assetFundedBy(agreement, party)
	var notice swapprotocol.FundingNotice
	if asset == "QDAY" {
		action, err := wallets.qday.FundSwap(ctx, localQDAYSwapID(record), walletd.FundSwapRequest{
			AmountAtomic: agreement.QDAYAmountAtomic, ExpectedUnitAtomic: agreement.QDAYUnitAtomic,
		})
		if err != nil {
			return fmt.Errorf("fund QDAY swap contract: %w", err)
		}
		notice = swapprotocol.FundingNotice{Party: party, Asset: asset, TransactionID: action.TransactionID}
	} else {
		contract, err := bitcoinContract(agreement)
		if err != nil {
			return err
		}
		actionID := record.ID + ":" + party + "-funding"
		action, err := journal.Action(actionID)
		if errors.Is(err, swapstate.ErrNotFound) {
			funding, err := wallets.bitcoin.PrepareFunding(contract)
			if err != nil {
				return err
			}
			action, _, err = journal.PrepareAction(swapstate.Action{
				ID: actionID, SwapID: record.ID, Kind: party + "-funding", Chain: "BTC",
				RawTransaction: funding.Raw, TransactionID: funding.TxID,
			}, time.Now().UTC())
			if err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		transaction, err := wallets.bitcoin.Broadcast(action.RawTransaction)
		if err != nil {
			return err
		}
		if _, err := reconcileBitcoinAction(journal, action, transaction); err != nil {
			return err
		}
		notice = swapprotocol.FundingNotice{Party: party, Asset: asset, TransactionID: action.TransactionID, RawTransaction: action.RawTransaction}
	}
	if err := notice.Validate(); err != nil {
		return err
	}
	encoded, _ := json.Marshal(notice)
	if _, err := journal.SetFunding(record.ID, party, string(encoded), time.Now().UTC()); err != nil {
		return err
	}
	record, _ = journal.Swap(record.ID)
	message := swapprotocol.Message{Version: asyncMessageVersion(record), Kind: swapprotocol.MessageFunding, TradeID: record.ID, Funding: &notice}
	return s.sendProtocolMessage(ctx, wallets.root, journal, record, "funding:"+party, message)
}

func fundingConfirmed(ctx context.Context, wallets engineWallets, agreement swapprotocol.Agreement, notice swapprotocol.FundingNotice) (bool, error) {
	if notice.Asset == "BTC" {
		if _, err := wallets.bitcoin.Broadcast(notice.RawTransaction); err != nil {
			return false, err
		}
		transaction, err := wallets.bitcoin.Transaction(notice.TransactionID)
		if errors.Is(err, bitcoinwallet.ErrTransactionNotFound) {
			return false, nil
		} else if err != nil {
			return false, err
		}
		return transaction.Confirmations >= agreement.BitcoinConfirmations, nil
	}
	view, err := wallets.qday.Swap(ctx, agreement.TradeID)
	if err != nil {
		return false, err
	}
	for _, output := range view.Outputs {
		if output.FundingTransaction != notice.TransactionID {
			continue
		}
		if output.Value.Atomic != agreement.QDAYAmountAtomic {
			return false, errors.New("QDAY funding amount does not match the agreement")
		}
		return output.Confirmations >= agreement.QDAYConfirmations, nil
	}
	return false, nil
}

func partyFundingConfirmed(ctx context.Context, wallets engineWallets, agreement swapprotocol.Agreement, record swapstate.Swap, party string) (bool, error) {
	notice, ok, err := fundingFromRecord(record, party)
	if err != nil || !ok {
		return false, err
	}
	return fundingConfirmed(ctx, wallets, agreement, notice)
}

func ensureLocalClaim(ctx context.Context, wallets engineWallets, journal *swapstate.Journal, record swapstate.Swap, agreement swapprotocol.Agreement, party string) error {
	secret, err := localSwapSecret(wallets.root, record, party)
	if err != nil {
		return err
	}
	defer clear(secret[:])
	funder := oppositeParty(party)
	funding, ok, err := fundingFromRecord(record, funder)
	if err != nil {
		return err
	} else if !ok {
		return errors.New("counterparty funding notice is missing")
	}
	asset := assetFundedBy(agreement, funder)
	if asset == "QDAY" {
		view, err := wallets.qday.Swap(ctx, localQDAYSwapID(record))
		if err != nil {
			return err
		}
		for _, output := range view.Outputs {
			if output.FundingTransaction == funding.TransactionID && output.Value.Atomic == agreement.QDAYAmountAtomic {
				_, err := wallets.qday.ClaimSwap(ctx, localQDAYSwapID(record), walletd.SpendSwapRequest{OutputID: output.ID, Secret: hex.EncodeToString(secret[:])})
				return err
			}
		}
		return errors.New("QDAY contract funding output is not visible")
	}
	contract, err := bitcoinContract(agreement)
	if err != nil {
		return err
	}
	btcFunding, err := contract.FundingFromRaw(funding.RawTransaction)
	if err != nil {
		return err
	}
	actionID := record.ID + ":" + party + "-claim"
	action, err := journal.Action(actionID)
	if errors.Is(err, swapstate.ErrNotFound) {
		destination, err := wallets.bitcoin.DestinationScript()
		if err != nil {
			return err
		}
		key, err := wallets.root.BitcoinSwapKey(localBitcoinSwapKeyID(record))
		if err != nil {
			return err
		}
		raw, err := contract.BuildClaim(btcFunding, destination, bitcoinSpendFee(contract.Amount), key, secret)
		if err != nil {
			return err
		}
		transaction, err := bitcoin.DecodeTransaction(raw)
		if err != nil {
			return err
		}
		action, _, err = journal.PrepareAction(swapstate.Action{
			ID: actionID, SwapID: record.ID, Kind: party + "-claim", Chain: "BTC",
			RawTransaction: raw, TransactionID: transaction.TxID(),
		}, time.Now().UTC())
		if err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	transaction, err := wallets.bitcoin.Broadcast(action.RawTransaction)
	if err != nil {
		return err
	}
	_, err = reconcileBitcoinAction(journal, action, transaction)
	return err
}

func observeClaim(ctx context.Context, wallets engineWallets, agreement swapprotocol.Agreement, claimant string, record swapstate.Swap) (found, confirmed bool, secret string, err error) {
	funder := oppositeParty(claimant)
	funding, ok, err := fundingFromRecord(record, funder)
	if err != nil || !ok {
		return false, false, "", err
	}
	if funding.Asset == "BTC" {
		contract, err := bitcoinContract(agreement)
		if err != nil {
			return false, false, "", err
		}
		btcFunding, err := contract.FundingFromRaw(funding.RawTransaction)
		if err != nil {
			return false, false, "", err
		}
		spend, err := wallets.bitcoin.FindSpend(btcFunding)
		if errors.Is(err, bitcoinwallet.ErrTransactionNotFound) {
			return false, false, "", nil
		} else if err != nil {
			return false, false, "", err
		}
		revealed, err := contract.ExtractSecret(spend.Raw, btcFunding)
		if err != nil {
			return false, false, "", nil
		}
		return true, spend.Confirmations > 0, hex.EncodeToString(revealed[:]), nil
	}
	view, err := wallets.qday.Swap(ctx, localQDAYSwapID(record))
	if err != nil {
		return false, false, "", err
	}
	for _, output := range view.Outputs {
		if output.FundingTransaction == funding.TransactionID && output.RevealedSecret != "" {
			return true, output.Status == "claimed", output.RevealedSecret, nil
		}
	}
	return false, false, "", nil
}

func (s *Service) maybeStartRefund(ctx context.Context, wallets engineWallets, journal *swapstate.Journal, record swapstate.Swap, agreement swapprotocol.Agreement) (bool, error) {
	if record.Phase == swapstate.PhaseWaitingForRefund || record.Phase == swapstate.PhaseRefunding {
		return false, nil
	}
	party := swapprotocol.PartyMaker
	if record.Role == swapstate.RoleTaker {
		party = swapprotocol.PartyTaker
	}
	notice, ok, err := fundingFromRecord(record, party)
	if err != nil {
		return false, err
	}
	if !ok {
		// A process can stop after its wallet accepted the exact transaction but
		// before the funding notice reached the swap journal. Recover that narrow
		// crash window before deciding that no funds moved.
		recovered, found, recoverErr := recoverSubmittedLocalFunding(ctx, wallets, journal, record, agreement, party)
		if recoverErr != nil {
			return false, recoverErr
		} else if found {
			encoded, _ := json.Marshal(recovered)
			record, err = journal.SetFunding(record.ID, party, string(encoded), time.Now().UTC())
			if err != nil {
				return false, err
			}
			notice, ok = recovered, true
		}
	}
	if ok && record.Version == trade.AsyncProtocolVersion && party == swapprotocol.PartyTaker && !record.TakerFundingSubmitted {
		// Version 2 stores the taker's signed funding bytes before they have been
		// published. Do not confuse that durable template with on-chain funding.
		// The maker may have relayed it while this application was offline, so
		// observe both chain and mempool before restoring the local marker.
		observed, observeErr := fundingObservedForRecord(ctx, wallets, record, agreement, notice)
		if observeErr != nil {
			return false, observeErr
		}
		if observed {
			record, err = journal.SetTakerFundingSubmitted(record.ID, true, time.Now().UTC())
			if err != nil {
				return false, err
			}
		} else {
			ok = false
		}
	}
	if !ok {
		if err := ensureFundingWindows(ctx, wallets, agreement); err != nil {
			if !errors.Is(err, errFundingWindowClosed) {
				return false, err
			}
			if releaseErr := releasePreparedLocalFunding(ctx, wallets, journal, record, agreement, party); releaseErr != nil {
				return false, releaseErr
			}
			if _, advanceErr := journal.Advance(record.ID, record.Phase, swapstate.PhaseExpired, time.Now().UTC()); advanceErr != nil {
				return false, advanceErr
			}
			return true, nil
		}
		// If only the peer funded and later took its unilateral refund, this
		// side still needs a terminal state. Otherwise a safely abandoned swap
		// would remain in the active list forever.
		peerFunding, peerFunded, err := fundingFromRecord(record, oppositeParty(party))
		if err != nil || !peerFunded {
			return false, err
		}
		refunded, err := refundConfirmed(ctx, wallets, record, agreement, peerFunding)
		if err != nil || !refunded {
			return false, err
		}
		if _, err := journal.Advance(record.ID, record.Phase, swapstate.PhaseWaitingForRefund, time.Now().UTC()); err != nil {
			return false, err
		}
		return true, nil
	}
	// Once the counterparty claim reveals the secret, claiming the other leg
	// takes priority over a refund even if the wall clock reaches a timeout.
	claimant := oppositeParty(party)
	claimed, _, secret, err := observeClaim(ctx, wallets, agreement, claimant, record)
	if err != nil {
		return false, err
	} else if claimed {
		if record.RevealedSecret == "" {
			_, _ = journal.SetRevealedSecret(record.ID, secret, time.Now().UTC())
		}
		return false, nil
	}
	due := false
	if notice.Asset == "QDAY" {
		status, err := s.client.Status(ctx)
		if err != nil {
			return false, err
		}
		due = status.Height >= agreement.QDAYRefundHeight
	} else {
		status, err := s.bitcoinClient.Status()
		if err != nil {
			return false, err
		}
		due = status.WalletHeight >= int32(agreement.BitcoinRefundHeight)
	}
	if !due {
		return false, nil
	}
	if _, err := journal.Advance(record.ID, record.Phase, swapstate.PhaseWaitingForRefund, time.Now().UTC()); err != nil {
		return false, err
	}
	return true, nil
}

func recoverSubmittedLocalFunding(ctx context.Context, wallets engineWallets, journal *swapstate.Journal, record swapstate.Swap, agreement swapprotocol.Agreement, party string) (swapprotocol.FundingNotice, bool, error) {
	asset := assetFundedBy(agreement, party)
	if asset == "QDAY" {
		view, err := wallets.qday.Swap(ctx, localQDAYSwapID(record))
		if err != nil {
			return swapprotocol.FundingNotice{}, false, err
		}
		for _, action := range view.Actions {
			if action.Kind != "fund" || !action.Submitted {
				continue
			}
			notice := swapprotocol.FundingNotice{Party: party, Asset: asset, TransactionID: action.TransactionID}
			return notice, true, notice.Validate()
		}
		return swapprotocol.FundingNotice{}, false, nil
	}
	action, err := journal.Action(record.ID + ":" + party + "-funding")
	if errors.Is(err, swapstate.ErrNotFound) {
		return swapprotocol.FundingNotice{}, false, nil
	} else if err != nil {
		return swapprotocol.FundingNotice{}, false, err
	}
	submitted := action.Status != swapstate.ActionPrepared
	if !submitted {
		_, transactionErr := wallets.bitcoin.Transaction(action.TransactionID)
		submitted = transactionErr == nil
		if transactionErr != nil && !errors.Is(transactionErr, bitcoinwallet.ErrTransactionNotFound) {
			return swapprotocol.FundingNotice{}, false, transactionErr
		}
	}
	if !submitted {
		return swapprotocol.FundingNotice{}, false, nil
	}
	notice := swapprotocol.FundingNotice{Party: party, Asset: asset, TransactionID: action.TransactionID, RawTransaction: action.RawTransaction}
	return notice, true, notice.Validate()
}

func releasePreparedLocalFunding(ctx context.Context, wallets engineWallets, journal *swapstate.Journal, record swapstate.Swap, agreement swapprotocol.Agreement, party string) error {
	asset := assetFundedBy(agreement, party)
	if asset == "QDAY" {
		qday, ok := wallets.qday.(qdayAsyncSwapClient)
		if !ok {
			// Legacy clients publish funding in the same operation and therefore
			// have no separately prepared transaction to release.
			return nil
		}
		if record.Version != trade.AsyncProtocolVersion || party != swapprotocol.PartyTaker {
			view, err := qday.Swap(ctx, localQDAYSwapID(record))
			if err != nil {
				return err
			}
			prepared := false
			for _, action := range view.Actions {
				if action.Kind == "fund" {
					if action.Submitted {
						return errors.New("refusing to release submitted QDAY funding")
					}
					prepared = true
				}
			}
			if !prepared {
				return nil
			}
		}
		if err := qday.CancelPreparedSwapFunding(ctx, localQDAYSwapID(record)); err != nil {
			return fmt.Errorf("release prepared QDAY funding: %w", err)
		}
		return nil
	}
	bitcoin, ok := wallets.bitcoin.(bitcoinAsyncSwapClient)
	if !ok {
		return nil
	}
	if record.Version == trade.AsyncProtocolVersion && party == swapprotocol.PartyTaker {
		prepared := record.Acceptance.Acceptance.Funding
		if prepared.Asset == "BTC" && len(prepared.RawTransactions) == 1 {
			if err := bitcoin.ReleaseFunding(prepared.RawTransactions[0]); err != nil {
				return fmt.Errorf("release prepared Bitcoin taker funding: %w", err)
			}
		}
		return nil
	}
	action, err := journal.Action(record.ID + ":" + party + "-funding")
	if errors.Is(err, swapstate.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	if err := bitcoin.ReleaseFunding(action.RawTransaction); err != nil {
		return fmt.Errorf("release prepared Bitcoin %s funding: %w", party, err)
	}
	return nil
}

func ensureFundingWindows(ctx context.Context, wallets engineWallets, agreement swapprotocol.Agreement) error {
	qdayStatus, err := wallets.qday.Status(ctx)
	if err != nil {
		return fmt.Errorf("read QDAY refund window: %w", err)
	}
	bitcoinStatus, err := wallets.bitcoin.Status()
	if err != nil {
		return fmt.Errorf("read Bitcoin refund window: %w", err)
	} else if bitcoinStatus.WalletHeight < 0 {
		return errors.New("Bitcoin wallet height is invalid")
	}
	qdayRemaining := uint64(0)
	if qdayStatus.Height < agreement.QDAYRefundHeight {
		qdayRemaining = agreement.QDAYRefundHeight - qdayStatus.Height
	}
	bitcoinHeight := uint32(bitcoinStatus.WalletHeight)
	bitcoinRemaining := uint32(0)
	if bitcoinHeight < agreement.BitcoinRefundHeight {
		bitcoinRemaining = agreement.BitcoinRefundHeight - bitcoinHeight
	}
	if qdayRemaining < minimumQDAYFundingWindowBlocks || bitcoinRemaining < minimumBitcoinFundingWindowBlocks {
		return fmt.Errorf(
			"%w (QDAY %d blocks, Bitcoin %d blocks remaining)", errFundingWindowClosed,
			qdayRemaining, bitcoinRemaining,
		)
	}
	return nil
}

func ensureLocalRefund(ctx context.Context, wallets engineWallets, journal *swapstate.Journal, record swapstate.Swap, agreement swapprotocol.Agreement) (bool, error) {
	party := swapprotocol.PartyMaker
	if record.Role == swapstate.RoleTaker {
		party = swapprotocol.PartyTaker
	}
	funding, ok, err := fundingFromRecord(record, party)
	if err != nil {
		return false, err
	}
	if !ok {
		peerFunding, peerFunded, err := fundingFromRecord(record, oppositeParty(party))
		if err != nil || !peerFunded {
			return false, err
		}
		return refundConfirmed(ctx, wallets, record, agreement, peerFunding)
	}
	if funding.Asset == "QDAY" {
		view, err := wallets.qday.Swap(ctx, localQDAYSwapID(record))
		if err != nil {
			return false, err
		}
		for _, output := range view.Outputs {
			if output.FundingTransaction != funding.TransactionID {
				continue
			}
			if output.Status == "refunded" {
				return true, nil
			}
			_, err := wallets.qday.RefundSwap(ctx, localQDAYSwapID(record), walletd.SpendSwapRequest{OutputID: output.ID})
			return false, err
		}
		return false, errors.New("QDAY refund output is not visible")
	}
	contract, err := bitcoinContract(agreement)
	if err != nil {
		return false, err
	}
	btcFunding, err := contract.FundingFromRaw(funding.RawTransaction)
	if err != nil {
		return false, err
	}
	actionID := record.ID + ":" + party + "-refund"
	action, err := journal.Action(actionID)
	if errors.Is(err, swapstate.ErrNotFound) {
		destination, err := wallets.bitcoin.DestinationScript()
		if err != nil {
			return false, err
		}
		key, err := wallets.root.BitcoinSwapKey(localBitcoinSwapKeyID(record))
		if err != nil {
			return false, err
		}
		raw, err := contract.BuildRefund(btcFunding, destination, bitcoinSpendFee(contract.Amount), key)
		if err != nil {
			return false, err
		}
		transaction, err := bitcoin.DecodeTransaction(raw)
		if err != nil {
			return false, err
		}
		action, _, err = journal.PrepareAction(swapstate.Action{
			ID: actionID, SwapID: record.ID, Kind: party + "-refund", Chain: "BTC",
			RawTransaction: raw, TransactionID: transaction.TxID(),
		}, time.Now().UTC())
		if err != nil {
			return false, err
		}
	} else if err != nil {
		return false, err
	}
	transaction, err := wallets.bitcoin.Broadcast(action.RawTransaction)
	if err != nil {
		return false, err
	}
	action, err = reconcileBitcoinAction(journal, action, transaction)
	if err != nil {
		return false, err
	}
	if transaction.Confirmations == 0 {
		transaction, err = wallets.bitcoin.Transaction(action.TransactionID)
		if errors.Is(err, bitcoinwallet.ErrTransactionNotFound) {
			return false, nil
		} else if err != nil {
			return false, err
		}
	}
	return transaction.Confirmations > 0, nil
}

func refundStateConfirmed(ctx context.Context, wallets engineWallets, record swapstate.Swap, agreement swapprotocol.Agreement) (bool, error) {
	party := swapprotocol.PartyMaker
	if record.Role == swapstate.RoleTaker {
		party = swapprotocol.PartyTaker
	}
	funding, ok, err := fundingFromRecord(record, party)
	if err != nil {
		return false, err
	}
	if !ok {
		funding, ok, err = fundingFromRecord(record, oppositeParty(party))
		if err != nil || !ok {
			return false, err
		}
	}
	return refundConfirmed(ctx, wallets, record, agreement, funding)
}

func refundConfirmed(ctx context.Context, wallets engineWallets, record swapstate.Swap, agreement swapprotocol.Agreement, funding swapprotocol.FundingNotice) (bool, error) {
	if funding.Asset == "QDAY" {
		view, err := wallets.qday.Swap(ctx, localQDAYSwapID(record))
		if err != nil {
			return false, err
		}
		for _, output := range view.Outputs {
			if output.FundingTransaction != funding.TransactionID {
				continue
			}
			if output.Value.Atomic != agreement.QDAYAmountAtomic {
				return false, errors.New("QDAY refund amount does not match the agreement")
			}
			return output.Status == "refunded", nil
		}
		return false, nil
	}
	contract, err := bitcoinContract(agreement)
	if err != nil {
		return false, err
	}
	btcFunding, err := contract.FundingFromRaw(funding.RawTransaction)
	if err != nil {
		return false, err
	}
	spend, err := wallets.bitcoin.FindSpend(btcFunding)
	if errors.Is(err, bitcoinwallet.ErrTransactionNotFound) {
		return false, nil
	} else if err != nil || spend.Confirmations < 1 {
		return false, err
	}
	refunded, err := contract.IsRefund(spend.Raw, btcFunding)
	if err != nil {
		return false, nil
	}
	return refunded, nil
}

func agreementFromRecord(record swapstate.Swap) (swapprotocol.Agreement, error) {
	var agreement swapprotocol.Agreement
	if err := decodeStrictJSON([]byte(record.AgreementJSON), &agreement); err != nil {
		return agreement, err
	}
	return agreement, nil
}

func fundingFromRecord(record swapstate.Swap, party string) (swapprotocol.FundingNotice, bool, error) {
	encoded := record.MakerFunding
	if party == swapprotocol.PartyTaker {
		encoded = record.TakerFunding
	}
	if encoded == "" {
		return swapprotocol.FundingNotice{}, false, nil
	}
	var notice swapprotocol.FundingNotice
	if err := decodeStrictJSON([]byte(encoded), &notice); err != nil {
		return notice, false, err
	}
	if err := notice.Validate(); err != nil {
		return notice, false, err
	}
	return notice, true, nil
}

func assetFundedBy(agreement swapprotocol.Agreement, party string) string {
	if party == swapprotocol.PartyMaker {
		return agreement.MakerGives
	}
	if agreement.MakerGives == "QDAY" {
		return "BTC"
	}
	return "QDAY"
}

func oppositeParty(party string) string {
	if party == swapprotocol.PartyMaker {
		return swapprotocol.PartyTaker
	}
	return swapprotocol.PartyMaker
}

func localSwapSecret(root walletroot.Root, record swapstate.Swap, party string) ([32]byte, error) {
	secretOwner := swapprotocol.PartyMaker
	if record.Version == trade.AsyncProtocolVersion {
		secretOwner = swapprotocol.PartyTaker
	}
	localParty := swapprotocol.PartyMaker
	if record.Role == swapstate.RoleTaker {
		localParty = swapprotocol.PartyTaker
	}
	if party != secretOwner || localParty != secretOwner {
		if record.RevealedSecret == "" {
			return [32]byte{}, errors.New("claim secret has not been revealed")
		}
		return decode32(record.RevealedSecret, "revealed swap secret")
	}
	secret, _, err := root.SwapSecret(record.ID)
	return secret, err
}

func bitcoinSpendFee(amount int64) int64 {
	fee := amount / 100
	if fee < 1_000 {
		fee = 1_000
	}
	if fee > 10_000 {
		fee = 10_000
	}
	if fee >= amount {
		fee = amount / 2
	}
	return fee
}

func reconcileBitcoinAction(journal *swapstate.Journal, action swapstate.Action, transaction bitcoinwallet.Transaction) (swapstate.Action, error) {
	now := time.Now().UTC()
	var err error
	if action.Status == swapstate.ActionPrepared {
		action, err = journal.MarkBroadcast(action.ID, now)
		if err != nil {
			return action, err
		}
	}
	if transaction.Confirmations > 0 && transaction.Height > 0 {
		return journal.MarkConfirmed(action.ID, uint64(transaction.Height), now)
	}
	if action.Status == swapstate.ActionConfirmed {
		return journal.MarkReorged(action.ID, now)
	}
	return action, nil
}
