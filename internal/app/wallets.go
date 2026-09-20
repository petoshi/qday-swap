package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/petoshi/qday-swap/internal/bitcoinwallet"
	"github.com/petoshi/qday-swap/internal/keystore"
	"github.com/petoshi/qday-swap/internal/swapstate"
	"github.com/petoshi/qday-swap/internal/walletd"
	"github.com/petoshi/qday-swap/internal/walletroot"
)

const qdayWithdrawalFeeAtomic = "1000000000000000000000"

type qdayWithdrawalClient interface {
	CreateWithdrawal(context.Context, walletd.WithdrawalRequest) (walletd.Withdrawal, error)
}

type bitcoinPaymentClient interface {
	QuotePayment(string, int64, bool) (bitcoinwallet.PaymentQuote, error)
	SendPayment(string, int64, int64) (bitcoinwallet.Payment, error)
}

type WithdrawalQuoteRequest struct {
	Asset       string `json:"asset"`
	Destination string `json:"destination"`
	Amount      string `json:"amount,omitempty"`
	Maximum     bool   `json:"maximum,omitempty"`
}

type WithdrawalQuote struct {
	RequestID          string `json:"requestID"`
	Asset              string `json:"asset"`
	Destination        string `json:"destination"`
	Amount             string `json:"amount"`
	AmountAtomic       string `json:"amountAtomic"`
	Fee                string `json:"fee"`
	FeeAtomic          string `json:"feeAtomic"`
	Total              string `json:"total"`
	TotalAtomic        string `json:"totalAtomic"`
	Available          string `json:"available"`
	AvailableAtomic    string `json:"availableAtomic"`
	UnitAtomic         string `json:"unitAtomic"`
	FeeRateSatPerVByte int64  `json:"feeRateSatPerVByte,omitempty"`
}

type SendWithdrawalRequest struct {
	RequestID    string `json:"requestID"`
	Asset        string `json:"asset"`
	Destination  string `json:"destination"`
	AmountAtomic string `json:"amountAtomic"`
	FeeAtomic    string `json:"feeAtomic"`
	UnitAtomic   string `json:"unitAtomic"`
}

type WithdrawalResult struct {
	Asset         string `json:"asset"`
	TransactionID string `json:"transactionID"`
	Status        string `json:"status"`
}

type withdrawalRecord struct {
	Request SendWithdrawalRequest
	Result  WithdrawalResult
}

func newWithdrawalRequestID() (string, error) {
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", fmt.Errorf("create withdrawal request ID: %w", err)
	}
	return "wallet-" + hex.EncodeToString(entropy[:]), nil
}

func positiveAtomic(value, name string) (*big.Int, error) {
	integer, ok := new(big.Int).SetString(value, 10)
	if !ok || integer.Sign() <= 0 {
		return nil, fmt.Errorf("%s is invalid", name)
	}
	return integer, nil
}

func validQDAYAddressShape(value string) bool {
	if len(value) == 141 && strings.HasPrefix(value, "qday1") {
		for _, character := range value[5:] {
			if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f') || (character >= 'A' && character <= 'F')) {
				return false
			}
		}
		return true
	}
	if len(value) != 64 || !strings.EqualFold(value[:5], "qday1") || (value != strings.ToLower(value) && value != strings.ToUpper(value)) {
		return false
	}
	const alphabet = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"
	for _, character := range strings.ToLower(value[5:]) {
		if !strings.ContainsRune(alphabet, character) {
			return false
		}
	}
	return true
}

func (s *Service) QuoteWithdrawal(ctx context.Context, request WithdrawalQuoteRequest) (WithdrawalQuote, error) {
	request.Asset = strings.ToUpper(strings.TrimSpace(request.Asset))
	request.Destination = strings.TrimSpace(request.Destination)
	if request.Destination == "" {
		return WithdrawalQuote{}, errors.New("destination address is required")
	}
	requestID, err := newWithdrawalRequestID()
	if err != nil {
		return WithdrawalQuote{}, err
	}

	s.mu.RLock()
	unlocked := s.root != nil
	qdayClient, bitcoinClient := s.client, s.bitcoinClient
	s.mu.RUnlock()
	if !unlocked {
		return WithdrawalQuote{}, errors.New("wallet is locked")
	}

	switch request.Asset {
	case "QDAY":
		if !validQDAYAddressShape(request.Destination) {
			return WithdrawalQuote{}, errors.New("enter a complete QDAY address")
		}
		if qdayClient == nil {
			return WithdrawalQuote{}, errors.New("QDAY wallet is unavailable")
		}
		status, err := qdayClient.Status(ctx)
		if err != nil {
			return WithdrawalQuote{}, err
		} else if !status.NetworkSynced || !status.Synced {
			return WithdrawalQuote{}, errors.New("QDAY wallet is still synchronizing")
		}
		balance, err := qdayClient.Balance(ctx)
		if err != nil {
			return WithdrawalQuote{}, err
		}
		available, err := positiveAtomic(balance.Spendable.Atomic, "QDAY balance")
		if err != nil {
			return WithdrawalQuote{}, err
		}
		fee, _ := new(big.Int).SetString(qdayWithdrawalFeeAtomic, 10)
		amount := new(big.Int)
		if request.Maximum {
			amount.Sub(available, fee)
			if amount.Sign() <= 0 {
				return WithdrawalQuote{}, errors.New("QDAY balance cannot cover the network fee")
			}
		} else {
			atomic, err := decimalToAtomic(request.Amount, status.UnitAtomic)
			if err != nil {
				return WithdrawalQuote{}, err
			}
			if _, ok := amount.SetString(atomic, 10); !ok {
				return WithdrawalQuote{}, errors.New("QDAY amount is invalid")
			}
		}
		total := new(big.Int).Add(new(big.Int).Set(amount), fee)
		if total.Cmp(available) > 0 {
			return WithdrawalQuote{}, errors.New("amount plus network fee exceeds the spendable QDAY balance")
		}
		return WithdrawalQuote{
			RequestID: requestID, Asset: "QDAY", Destination: request.Destination,
			Amount: atomicDecimal(amount.String(), status.UnitAtomic), AmountAtomic: amount.String(),
			Fee: atomicDecimal(fee.String(), status.UnitAtomic), FeeAtomic: fee.String(),
			Total: atomicDecimal(total.String(), status.UnitAtomic), TotalAtomic: total.String(),
			Available: atomicDecimal(available.String(), status.UnitAtomic), AvailableAtomic: available.String(),
			UnitAtomic: status.UnitAtomic,
		}, nil

	case "BTC", "BITCOIN":
		if bitcoinClient == nil {
			return WithdrawalQuote{}, errors.New("Bitcoin wallet is unavailable")
		}
		status, err := bitcoinClient.Status()
		if err != nil {
			return WithdrawalQuote{}, err
		} else if !status.HeadersSynced || !status.WalletSynced {
			return WithdrawalQuote{}, errors.New("Bitcoin wallet is still synchronizing")
		}
		payments, ok := bitcoinClient.(bitcoinPaymentClient)
		if !ok {
			return WithdrawalQuote{}, errors.New("Bitcoin wallet does not support payments")
		}
		balance, err := bitcoinClient.Balance()
		if err != nil {
			return WithdrawalQuote{}, err
		}
		available, err := positiveAtomic(balance.Confirmed.Satoshis, "Bitcoin balance")
		if err != nil {
			return WithdrawalQuote{}, err
		}
		var satoshis int64
		if !request.Maximum {
			atomic, err := decimalToAtomic(request.Amount, "100000000")
			if err != nil {
				return WithdrawalQuote{}, err
			}
			value, ok := new(big.Int).SetString(atomic, 10)
			if !ok || !value.IsInt64() || value.Sign() <= 0 {
				return WithdrawalQuote{}, errors.New("Bitcoin amount is too large")
			}
			satoshis = value.Int64()
		}
		quote, err := payments.QuotePayment(request.Destination, satoshis, request.Maximum)
		if err != nil {
			return WithdrawalQuote{}, err
		}
		amountAtomic, err := positiveAtomic(quote.Amount.Satoshis, "Bitcoin amount")
		if err != nil {
			return WithdrawalQuote{}, err
		}
		feeAtomic, err := positiveAtomic(quote.Fee.Satoshis, "Bitcoin fee")
		if err != nil {
			return WithdrawalQuote{}, err
		}
		total := new(big.Int).Add(new(big.Int).Set(amountAtomic), feeAtomic)
		if total.Cmp(available) > 0 {
			return WithdrawalQuote{}, errors.New("amount plus network fee exceeds the confirmed Bitcoin balance")
		}
		return WithdrawalQuote{
			RequestID: requestID, Asset: "BTC", Destination: quote.Destination,
			Amount: quote.Amount.BTC, AmountAtomic: quote.Amount.Satoshis,
			Fee: quote.Fee.BTC, FeeAtomic: quote.Fee.Satoshis,
			Total: quote.Total.BTC, TotalAtomic: quote.Total.Satoshis,
			Available: balance.Confirmed.BTC, AvailableAtomic: balance.Confirmed.Satoshis,
			UnitAtomic: "100000000", FeeRateSatPerVByte: quote.FeeRateSatPerVByte,
		}, nil
	default:
		return WithdrawalQuote{}, errors.New("asset must be QDAY or BTC")
	}
}

func (s *Service) SendWithdrawal(ctx context.Context, request SendWithdrawalRequest) (WithdrawalResult, error) {
	request.Asset = strings.ToUpper(strings.TrimSpace(request.Asset))
	request.Destination = strings.TrimSpace(request.Destination)
	if request.RequestID == "" || !strings.HasPrefix(request.RequestID, "wallet-") {
		return WithdrawalResult{}, errors.New("withdrawal request ID is invalid")
	}
	amount, err := positiveAtomic(request.AmountAtomic, "amount")
	if err != nil {
		return WithdrawalResult{}, err
	}
	fee, err := positiveAtomic(request.FeeAtomic, "fee")
	if err != nil {
		return WithdrawalResult{}, err
	}
	s.withdrawalMu.Lock()
	defer s.withdrawalMu.Unlock()
	if previous, ok := s.withdrawals[request.RequestID]; ok {
		if previous.Request != request {
			return WithdrawalResult{}, errors.New("withdrawal request ID is already bound to another payment")
		}
		return previous.Result, nil
	}
	remember := func(result WithdrawalResult) WithdrawalResult {
		if s.withdrawals == nil {
			s.withdrawals = make(map[string]withdrawalRecord)
		}
		s.withdrawals[request.RequestID] = withdrawalRecord{Request: request, Result: result}
		return result
	}

	s.mu.RLock()
	unlocked := s.root != nil
	qdayClient, bitcoinClient := s.client, s.bitcoinClient
	s.mu.RUnlock()
	if !unlocked {
		return WithdrawalResult{}, errors.New("wallet is locked")
	}

	switch request.Asset {
	case "QDAY":
		if qdayClient == nil {
			return WithdrawalResult{}, errors.New("QDAY wallet is unavailable")
		}
		if request.FeeAtomic != qdayWithdrawalFeeAtomic {
			return WithdrawalResult{}, errors.New("QDAY network fee changed; review the payment again")
		}
		status, err := qdayClient.Status(ctx)
		if err != nil {
			return WithdrawalResult{}, err
		} else if request.UnitAtomic != status.UnitAtomic {
			return WithdrawalResult{}, errors.New("QDAY denomination changed; review the payment again")
		} else if !status.NetworkSynced || !status.Synced {
			return WithdrawalResult{}, errors.New("QDAY wallet is still synchronizing")
		}
		balance, err := qdayClient.Balance(ctx)
		if err != nil {
			return WithdrawalResult{}, err
		}
		available, err := positiveAtomic(balance.Spendable.Atomic, "QDAY balance")
		if err != nil {
			return WithdrawalResult{}, err
		}
		if new(big.Int).Add(new(big.Int).Set(amount), fee).Cmp(available) > 0 {
			return WithdrawalResult{}, errors.New("amount plus network fee exceeds the spendable QDAY balance")
		}
		withdrawals, ok := qdayClient.(qdayWithdrawalClient)
		if !ok {
			return WithdrawalResult{}, errors.New("QDAY wallet does not support withdrawals")
		}
		created, err := withdrawals.CreateWithdrawal(ctx, walletd.WithdrawalRequest{
			RequestID: request.RequestID, Destination: request.Destination,
			AmountAtomic: request.AmountAtomic, FeeAtomic: request.FeeAtomic,
			ExpectedUnitAtomic: request.UnitAtomic,
		})
		if err != nil {
			return WithdrawalResult{}, err
		}
		return remember(WithdrawalResult{Asset: "QDAY", TransactionID: created.TransactionID, Status: created.Status}), nil

	case "BTC", "BITCOIN":
		if bitcoinClient == nil {
			return WithdrawalResult{}, errors.New("Bitcoin wallet is unavailable")
		} else if request.UnitAtomic != "100000000" || !amount.IsInt64() || !fee.IsInt64() || amount.Int64() > math.MaxInt64-fee.Int64() {
			return WithdrawalResult{}, errors.New("Bitcoin payment is invalid")
		}
		status, err := bitcoinClient.Status()
		if err != nil {
			return WithdrawalResult{}, err
		} else if !status.HeadersSynced || !status.WalletSynced {
			return WithdrawalResult{}, errors.New("Bitcoin wallet is still synchronizing")
		}
		payments, ok := bitcoinClient.(bitcoinPaymentClient)
		if !ok {
			return WithdrawalResult{}, errors.New("Bitcoin wallet does not support payments")
		}
		payment, err := payments.SendPayment(request.Destination, amount.Int64(), fee.Int64())
		if err != nil {
			return WithdrawalResult{}, err
		}
		return remember(WithdrawalResult{Asset: "BTC", TransactionID: payment.TransactionID, Status: "broadcast"}), nil
	default:
		return WithdrawalResult{}, errors.New("asset must be QDAY or BTC")
	}
}

func (s *Service) RecoveryPhrase(password string) (string, error) {
	if password == "" {
		return "", errors.New("wallet password is required")
	}
	s.mu.RLock()
	unlocked := s.root != nil
	s.mu.RUnlock()
	if !unlocked {
		return "", errors.New("wallet is locked")
	}
	root, err := keystore.Open(filepath.Join(s.config.DataDir, "master.key"), password)
	if err != nil {
		return "", errors.New("wallet password is incorrect")
	}
	defer clear(root[:])
	return root.Phrase()
}

func (s *Service) stageImportedWallet(ctx context.Context, directory, password, phrase string) (walletroot.Root, error) {
	root, err := walletroot.ParsePhrase(phrase)
	if err != nil {
		return walletroot.Root{}, err
	}
	fail := func(err error) (walletroot.Root, error) {
		clear(root[:])
		return walletroot.Root{}, err
	}
	if err := os.Chmod(directory, 0700); err != nil {
		return fail(err)
	} else if err := keystore.Create(filepath.Join(directory, "master.key"), password, root); err != nil {
		return fail(err)
	}
	process, err := s.factory(filepath.Join(directory, "qday"))
	if err != nil {
		return fail(err)
	} else if err := process.Initialize(ctx, root, password); err != nil {
		return fail(err)
	}
	bitcoinProcess, err := s.bitcoinFactory(filepath.Join(directory, "bitcoin"))
	if err != nil {
		return fail(err)
	} else if err := bitcoinProcess.Initialize(ctx, root, password, time.Unix(0, 0).UTC()); err != nil {
		return fail(err)
	}
	journal, err := swapstate.Open(filepath.Join(directory, "swaps.db"))
	if err != nil {
		return fail(err)
	} else if err := journal.Close(); err != nil {
		return fail(err)
	}
	return root, nil
}

func (s *Service) closeWalletComponentsLocked(ctx context.Context) error {
	if s.root != nil {
		clear(s.root[:])
		s.root = nil
	}
	var result error
	if s.client != nil {
		if err := s.client.Lock(ctx); err != nil {
			result = err
		}
	}
	if s.bitcoinClient != nil {
		s.bitcoinClient.Lock()
	}
	if s.journal != nil {
		if err := s.journal.Close(); result == nil {
			result = err
		}
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

func (s *Service) ImportRecovery(ctx context.Context, password, phrase string) (State, error) {
	if !s.Configured() {
		return State{}, errors.New("QDAY Swap is not configured")
	} else if len(password) < 12 {
		return State{}, errors.New("wallet password must contain at least 12 bytes")
	}
	parent := filepath.Dir(s.config.DataDir)
	stage, err := os.MkdirTemp(parent, ".qday-swap-import-")
	if err != nil {
		return State{}, err
	}
	stageExists := true
	defer func() {
		if stageExists {
			_ = os.RemoveAll(stage)
		}
	}()
	root, err := s.stageImportedWallet(ctx, stage, password, phrase)
	if err != nil {
		return State{}, err
	}
	rootOwned := true
	defer func() {
		if rootOwned {
			clear(root[:])
		}
	}()

	old, err := os.MkdirTemp(parent, ".qday-swap-replaced-")
	if err != nil {
		return State{}, err
	}
	if err := os.Remove(old); err != nil {
		return State{}, err
	}
	oldExists := false
	defer func() {
		if oldExists {
			_ = os.RemoveAll(old)
		}
	}()

	s.relaySyncMu.Lock()
	defer s.relaySyncMu.Unlock()
	s.negotiationMu.Lock()
	defer s.negotiationMu.Unlock()
	s.engineMu.Lock()
	defer s.engineMu.Unlock()
	s.withdrawalMu.Lock()
	defer s.withdrawalMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.journal == nil {
		return State{}, errors.New("swap journal is unavailable")
	}
	pending, err := s.journal.PendingAcceptances()
	if err != nil {
		return State{}, err
	}
	incoming, err := s.journal.IncomingAcceptances()
	if err != nil {
		return State{}, err
	}
	swaps, err := s.journal.List()
	if err != nil {
		return State{}, err
	}
	active := len(pending) + len(incoming)
	for _, swap := range swaps {
		switch swap.Phase {
		case swapstate.PhaseComplete, swapstate.PhaseRefunded, swapstate.PhaseExpired:
		default:
			active++
		}
	}
	if active != 0 {
		return State{}, errors.New("finish or refund every active swap before importing another recovery phrase")
	}

	commitCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.closeWalletComponentsLocked(commitCtx); err != nil {
		_ = s.openLocked()
		return State{}, fmt.Errorf("stop current wallet: %w", err)
	}
	if err := os.Rename(s.config.DataDir, old); err != nil {
		_ = s.openLocked()
		return State{}, fmt.Errorf("prepare wallet import: %w", err)
	}
	oldExists = true
	if err := os.Rename(stage, s.config.DataDir); err != nil {
		_ = os.Rename(old, s.config.DataDir)
		oldExists = false
		_ = s.openLocked()
		return State{}, fmt.Errorf("install imported wallet: %w", err)
	}
	stageExists = false
	rollback := func(cause error) (State, error) {
		_ = s.closeWalletComponentsLocked(commitCtx)
		failed := stage
		_ = os.RemoveAll(failed)
		if renameErr := os.Rename(s.config.DataDir, failed); renameErr != nil {
			s.lastError = joinError(cause.Error(), renameErr)
			return State{}, cause
		}
		stageExists = true
		if renameErr := os.Rename(old, s.config.DataDir); renameErr != nil {
			s.lastError = joinError(cause.Error(), renameErr)
			return State{}, cause
		}
		oldExists = false
		if openErr := s.openLocked(); openErr != nil {
			s.lastError = joinError(cause.Error(), openErr)
		}
		return State{}, cause
	}
	if err := s.openLocked(); err != nil {
		return rollback(fmt.Errorf("open imported wallet: %w", err))
	}
	if err := unlockQDAYWallet(commitCtx, s.client, password); err != nil {
		return rollback(fmt.Errorf("unlock imported QDAY wallet: %w", err))
	}
	if err := s.bitcoinClient.Unlock(password); err != nil {
		return rollback(fmt.Errorf("unlock imported Bitcoin wallet: %w", err))
	}
	s.root = &root
	rootOwned = false
	s.withdrawals = nil
	s.lastError = ""
	if err := os.RemoveAll(old); err != nil {
		s.lastError = fmt.Sprintf("remove replaced wallet data: %v", err)
		return State{}, errors.New(s.lastError)
	}
	oldExists = false
	_ = syncDirectory(parent)

	// Build State after releasing s.mu; State needs its own read lock.
	s.mu.Unlock()
	result := s.State(commitCtx)
	s.mu.Lock()
	return result, nil
}
