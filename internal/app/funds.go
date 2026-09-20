package app

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/petoshi/qday-swap/internal/bitcoinwallet"
	"github.com/petoshi/qday-swap/internal/order"
	"github.com/petoshi/qday-swap/internal/relay"
	"github.com/petoshi/qday-swap/internal/swapstate"
	"github.com/petoshi/qday-swap/internal/trade"
	"github.com/petoshi/qday-swap/internal/walletd"
)

// AssetFunds separates the on-chain wallet total from amounts committed by
// signed offers and swaps which have not funded yet. A reservation is local
// accounting. It does not claim that an open order is already locked on-chain.
type AssetFunds struct {
	Total           string `json:"total"`
	TotalAtomic     string `json:"totalAtomic"`
	Reserved        string `json:"reserved"`
	ReservedAtomic  string `json:"reservedAtomic"`
	Available       string `json:"available"`
	AvailableAtomic string `json:"availableAtomic"`
}

type Funds struct {
	QDAY       AssetFunds `json:"qday"`
	Bitcoin    AssetFunds `json:"bitcoin"`
	OpenOrders int        `json:"openOrders"`
}

type assetReservations struct {
	qday    *big.Int
	bitcoin *big.Int
	orders  int
}

type reservationEntry struct {
	asset  string
	atomic string
}

func (s *Service) fundsSnapshot(ctx context.Context, qdayStatus walletd.Status, qdayBalance walletd.Balance, bitcoinBalance bitcoinwallet.Balance) (Funds, error) {
	reservations, err := s.reservedFunds(ctx)
	if err != nil {
		return Funds{}, fmt.Errorf("verify funds reserved by orders: %w", err)
	}
	qday, err := assetFunds(qdayBalance.Spendable.Atomic, reservations.qday, qdayStatus.UnitAtomic)
	if err != nil {
		return Funds{}, fmt.Errorf("QDAY balance: %w", err)
	}
	bitcoin, err := assetFunds(bitcoinBalance.Confirmed.Satoshis, reservations.bitcoin, "100000000")
	if err != nil {
		return Funds{}, fmt.Errorf("Bitcoin balance: %w", err)
	}
	return Funds{QDAY: qday, Bitcoin: bitcoin, OpenOrders: reservations.orders}, nil
}

func assetFunds(totalAtomic string, reserved *big.Int, unit string) (AssetFunds, error) {
	total, ok := new(big.Int).SetString(totalAtomic, 10)
	if !ok || total.Sign() < 0 {
		return AssetFunds{}, errors.New("wallet returned an invalid balance")
	}
	if reserved == nil || reserved.Sign() < 0 {
		return AssetFunds{}, errors.New("reservation total is invalid")
	}
	available := new(big.Int).Sub(new(big.Int).Set(total), reserved)
	if available.Sign() < 0 {
		available.SetInt64(0)
	}
	return AssetFunds{
		Total: atomicDecimal(total.String(), unit), TotalAtomic: total.String(),
		Reserved: atomicDecimal(reserved.String(), unit), ReservedAtomic: reserved.String(),
		Available: atomicDecimal(available.String(), unit), AvailableAtomic: available.String(),
	}, nil
}

func (s *Service) reservedFunds(ctx context.Context) (assetReservations, error) {
	identity, _, cleanup, err := s.identities()
	if err != nil {
		return assetReservations{}, err
	}
	defer cleanup()
	publicKey := hex.EncodeToString(identity.Public().(ed25519.PublicKey))

	entries := make(map[string]reservationEntry)
	openOrders := 0
	add := func(key string, amount order.Amount) error {
		if amount.Asset != "QDAY" && amount.Asset != "BTC" {
			return fmt.Errorf("reservation %s has unsupported asset %q", key, amount.Asset)
		}
		value, ok := new(big.Int).SetString(amount.Atomic, 10)
		if !ok || value.Sign() <= 0 {
			return fmt.Errorf("reservation %s has an invalid amount", key)
		}
		if previous, exists := entries[key]; exists {
			if previous.asset != amount.Asset || previous.atomic != amount.Atomic {
				return fmt.Errorf("reservation %s changed its signed amount", key)
			}
			return nil
		}
		entries[key] = reservationEntry{asset: amount.Asset, atomic: amount.Atomic}
		return nil
	}

	now := time.Now().UTC()
	for page := 1; ; page++ {
		result, err := s.relay.OrdersByMaker(ctx, relay.StatusOpen, publicKey, page, 100)
		if err != nil {
			return assetReservations{}, err
		}
		for _, record := range result.Items {
			if err := record.Signed.Verify(now); err != nil {
				return assetReservations{}, fmt.Errorf("relay returned invalid order %s: %w", record.Signed.ID, err)
			}
			if record.Signed.Order.MakerPublicKey == publicKey {
				if err := add("maker:"+record.Signed.ID, record.Signed.Order.Give); err != nil {
					return assetReservations{}, err
				}
				openOrders++
			}
		}
		if page >= result.TotalPages {
			break
		}
		if result.TotalPages > 10_000 {
			return assetReservations{}, errors.New("relay returned too many order pages")
		}
	}

	s.mu.RLock()
	journal := s.journal
	s.mu.RUnlock()
	if journal == nil {
		return assetReservations{}, errors.New("swap journal is unavailable")
	}
	pending, err := journal.PendingAcceptances()
	if err != nil {
		return assetReservations{}, err
	}
	for _, negotiation := range pending {
		if negotiation.Acceptance.Acceptance.ExpiresAt <= now.Unix() {
			continue
		}
		if err := add("taker:"+negotiation.Acceptance.ID, negotiation.Order.Order.Receive); err != nil {
			return assetReservations{}, err
		}
	}
	swaps, err := journal.List()
	if err != nil {
		return assetReservations{}, err
	}
	for _, swap := range swaps {
		if terminalSwapPhase(swap.Phase) || swap.Phase == swapstate.PhaseWaitingForRefund || swap.Phase == swapstate.PhaseRefunding {
			continue
		}
		switch swap.Role {
		case swapstate.RoleMaker:
			if swap.MakerFunding == "" {
				if err := add("maker:"+swap.Order.ID, swap.Order.Order.Give); err != nil {
					return assetReservations{}, err
				}
			}
		case swapstate.RoleTaker:
			reserve := swap.TakerFunding == ""
			if swap.Version == trade.AsyncProtocolVersion {
				// The funding notice is part of the signed acceptance and therefore
				// exists before publication. Keep the amount unavailable until those
				// exact bytes have actually been submitted. Old journals already past
				// this phase remain compatible with the new marker's zero value.
				reserve = !swap.TakerFundingSubmitted && (swap.Phase == swapstate.PhaseMatched || swap.Phase == swapstate.PhaseAsyncTakerFunding)
			}
			if reserve {
				if err := add("taker:"+swap.Acceptance.ID, swap.Order.Order.Receive); err != nil {
					return assetReservations{}, err
				}
			}
		default:
			return assetReservations{}, fmt.Errorf("swap %s has an invalid local role", swap.ID)
		}
	}

	result := assetReservations{qday: new(big.Int), bitcoin: new(big.Int), orders: openOrders}
	for _, entry := range entries {
		value, _ := new(big.Int).SetString(entry.atomic, 10)
		if entry.asset == "QDAY" {
			result.qday.Add(result.qday, value)
		} else {
			result.bitcoin.Add(result.bitcoin, value)
		}
	}
	return result, nil
}

func terminalSwapPhase(phase swapstate.Phase) bool {
	switch phase {
	case swapstate.PhaseComplete, swapstate.PhaseRefunded, swapstate.PhaseExpired:
		return true
	default:
		return false
	}
}
