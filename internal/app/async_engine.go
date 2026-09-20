package app

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/petoshi/qday-swap/internal/bitcoin"
	"github.com/petoshi/qday-swap/internal/bitcoinwallet"
	swapprotocol "github.com/petoshi/qday-swap/internal/swap"
	"github.com/petoshi/qday-swap/internal/swapstate"
	"github.com/petoshi/qday-swap/internal/trade"
	"github.com/petoshi/qday-swap/internal/walletd"
)

func localQDAYSwapID(record swapstate.Swap) string {
	if record.Version == trade.AsyncProtocolVersion && record.Role == swapstate.RoleMaker {
		return record.Order.Order.SessionID
	}
	return record.ID
}

func localBitcoinSwapKeyID(record swapstate.Swap) string {
	if record.Version == trade.AsyncProtocolVersion && record.Role == swapstate.RoleMaker {
		return record.Order.Order.SessionID
	}
	return record.ID
}

func localAsyncParty(record swapstate.Swap) string {
	if record.Role == swapstate.RoleTaker {
		return swapprotocol.PartyTaker
	}
	return swapprotocol.PartyMaker
}

func asyncMessageVersion(record swapstate.Swap) uint16 {
	if record.Version == trade.AsyncProtocolVersion {
		return swapprotocol.AsyncProtocolVersion
	}
	return swapprotocol.ProtocolVersion
}

func asyncFundingPackage(value trade.FundingPackage) walletd.TransactionPackage {
	return walletd.TransactionPackage{
		BasisHeight: value.BasisHeight, BasisID: value.BasisID,
		Transactions:   append([]string(nil), value.RawTransactions...),
		TransactionIDs: []string{value.TransactionID},
	}
}

func asyncTakerFundingNotice(record swapstate.Swap) (swapprotocol.FundingNotice, error) {
	value := record.Acceptance.Acceptance.Funding
	notice := swapprotocol.FundingNotice{
		Party: swapprotocol.PartyTaker, Asset: value.Asset, TransactionID: value.TransactionID,
	}
	if value.Asset == "BTC" {
		if len(value.RawTransactions) != 1 {
			return notice, errors.New("Bitcoin acceptance funding must contain one transaction")
		}
		notice.RawTransaction = value.RawTransactions[0]
	}
	return notice, notice.Validate()
}

func registerAsyncContracts(ctx context.Context, wallets engineWallets, record swapstate.Swap, agreement swapprotocol.Agreement) error {
	localID := localQDAYSwapID(record)
	keys, err := wallets.qday.CreateSwapKeys(ctx, localID)
	if err != nil {
		return fmt.Errorf("restore local QDAY swap keys: %w", err)
	}
	local, peer := agreement.Maker, agreement.Taker
	if record.Role == swapstate.RoleTaker {
		local, peer = agreement.Taker, agreement.Maker
	}
	if keys.Keys.Classical != local.QDAY.Classical || keys.Keys.Reserve != local.QDAY.Reserve || keys.Keys.Address != local.QDAY.Address {
		return errors.New("local QDAY swap keys do not match the signed asynchronous agreement")
	}
	role := "recipient"
	localGivesQDAY := (record.Role == swapstate.RoleMaker && agreement.MakerGives == "QDAY") ||
		(record.Role == swapstate.RoleTaker && agreement.MakerGives == "BTC")
	if localGivesQDAY {
		role = "refund"
	}
	if _, err := wallets.qday.RegisterSwap(ctx, walletd.RegisterSwapRequest{
		SwapID: localID, Role: role,
		Counterparty: walletd.SwapKeys{Classical: peer.QDAY.Classical, Reserve: peer.QDAY.Reserve, Address: peer.QDAY.Address},
		SecretHash:   agreement.SecretHash, RefundHeight: agreement.QDAYRefundHeight,
	}); err != nil {
		return fmt.Errorf("register asynchronous QDAY contract: %w", err)
	}
	contract, err := bitcoinContract(agreement)
	if err != nil {
		return err
	}
	if err := wallets.bitcoin.WatchContract(ctx, contract, bitcoinContractWatchHeight(agreement)); err != nil {
		return fmt.Errorf("watch asynchronous Bitcoin contract: %w", err)
	}
	return nil
}

func validateAsyncTakerFunding(ctx context.Context, wallets engineWallets, record swapstate.Swap, agreement swapprotocol.Agreement) error {
	prepared := record.Acceptance.Acceptance.Funding
	expectedAsset := assetFundedBy(agreement, swapprotocol.PartyTaker)
	if prepared.Asset != expectedAsset {
		return errors.New("prepared taker funding asset differs from the agreement")
	}
	if prepared.Asset == "BTC" {
		if len(prepared.RawTransactions) != 1 {
			return errors.New("prepared Bitcoin funding must contain exactly one transaction")
		}
		contract, err := bitcoinContract(agreement)
		if err != nil {
			return err
		}
		funding, err := contract.FundingFromRaw(prepared.RawTransactions[0])
		if err != nil {
			return fmt.Errorf("validate prepared Bitcoin funding: %w", err)
		} else if funding.TxID != prepared.TransactionID {
			return errors.New("prepared Bitcoin funding ID differs from its signed bytes")
		}
		return nil
	}
	qday, ok := wallets.qday.(qdayAsyncSwapClient)
	if !ok {
		return errors.New("QDAY wallet does not support portable funding validation")
	}
	recipient, refund := agreement.Maker.QDAY, agreement.Taker.QDAY
	if agreement.MakerGives == "QDAY" {
		recipient, refund = agreement.Taker.QDAY, agreement.Maker.QDAY
	}
	validated, err := qday.ValidateSwapFundingPackage(ctx, localQDAYSwapID(record), walletd.ValidateSwapFundingRequest{
		Package: asyncFundingPackage(prepared), TransactionID: prepared.TransactionID,
		AmountAtomic: agreement.QDAYAmountAtomic, ExpectedUnitAtomic: agreement.QDAYUnitAtomic,
		Contract: &walletd.SwapContractView{
			Recipient:  walletd.SwapKeys{Classical: recipient.Classical, Reserve: recipient.Reserve, Address: recipient.Address},
			Refund:     walletd.SwapKeys{Classical: refund.Classical, Reserve: refund.Reserve, Address: refund.Address},
			SecretHash: agreement.SecretHash, RefundHeight: agreement.QDAYRefundHeight,
		},
	})
	if err != nil {
		return fmt.Errorf("validate prepared QDAY funding: %w", err)
	}
	if len(validated.TransactionIDs) == 0 || validated.TransactionIDs[len(validated.TransactionIDs)-1] != prepared.TransactionID {
		return errors.New("validated QDAY package changed the signed funding transaction ID")
	}
	return nil
}

func (s *Service) initializeAsyncSwap(ctx context.Context, wallets engineWallets, journal *swapstate.Journal, record swapstate.Swap) (swapstate.Swap, error) {
	agreement, err := swapprotocol.BuildAsyncAgreement(record.Order, record.Acceptance)
	if err != nil {
		return record, err
	}
	if err := validateAsyncTakerFunding(ctx, wallets, record, agreement); err != nil {
		return record, err
	}
	if err := registerAsyncContracts(ctx, wallets, record, agreement); err != nil {
		return record, err
	}
	if record.Role == swapstate.RoleTaker && record.Acceptance.Acceptance.Funding.Asset == "BTC" {
		bitcoin, ok := wallets.bitcoin.(bitcoinAsyncSwapClient)
		if !ok {
			return record, errors.New("Bitcoin wallet does not support durable funding reservations")
		}
		if err := bitcoin.ReserveFunding(record.Acceptance.Acceptance.Funding.RawTransactions[0]); err != nil {
			return record, fmt.Errorf("restore accepted Bitcoin funding reservation: %w", err)
		}
	}
	hash, err := agreement.Hash()
	if err != nil {
		return record, err
	}
	encoded, err := json.Marshal(agreement)
	if err != nil {
		return record, err
	}
	now := time.Now().UTC()
	if record.AgreementJSON == "" {
		record, err = journal.SetAgreementDetails(record.ID, agreement.SecretHash, hash, string(encoded), swapstate.PhaseMatched, now)
		if err != nil {
			return record, err
		}
	} else if record.AgreementHash != hash || record.AgreementJSON != string(encoded) {
		return record, errors.New("asynchronous agreement changed after selection")
	}
	if record.TakerFunding == "" {
		notice, err := asyncTakerFundingNotice(record)
		if err != nil {
			return record, err
		}
		noticeJSON, _ := json.Marshal(notice)
		record, err = journal.SetFunding(record.ID, swapprotocol.PartyTaker, string(noticeJSON), now)
		if err != nil {
			return record, err
		}
	}
	if record.Phase == swapstate.PhaseMatched {
		record, err = journal.Advance(record.ID, swapstate.PhaseMatched, swapstate.PhaseAsyncTakerFunding, now)
		if err != nil {
			return record, err
		}
	}
	if !record.Approved {
		record, err = journal.Approve(record.ID, now)
	}
	return record, err
}

func (s *Service) broadcastAsyncTakerFunding(ctx context.Context, wallets engineWallets, record swapstate.Swap, agreement swapprotocol.Agreement) error {
	prepared := record.Acceptance.Acceptance.Funding
	if prepared.Asset == "BTC" {
		if _, err := wallets.bitcoin.Broadcast(prepared.RawTransactions[0]); err != nil {
			return fmt.Errorf("broadcast accepted Bitcoin funding: %w", err)
		}
		if record.Role == swapstate.RoleTaker {
			if bitcoin, ok := wallets.bitcoin.(bitcoinAsyncSwapClient); ok {
				_ = bitcoin.ReleaseFunding(prepared.RawTransactions[0])
			}
		}
		return nil
	}
	qday, ok := wallets.qday.(qdayAsyncSwapClient)
	if !ok {
		return errors.New("QDAY wallet does not support portable funding broadcast")
	}
	if record.Role == swapstate.RoleTaker {
		// This also marks the locally prepared action as submitted, releasing its
		// input reservation after an external maker broadcast or a restart.
		_, err := qday.FundSwap(ctx, localQDAYSwapID(record), walletd.FundSwapRequest{
			AmountAtomic: agreement.QDAYAmountAtomic, ExpectedUnitAtomic: agreement.QDAYUnitAtomic,
		})
		return err
	}
	_, err := qday.BroadcastTransactionPackage(ctx, asyncFundingPackage(prepared))
	return err
}

func (s *Service) driveAsyncSwap(ctx context.Context, wallets engineWallets, journal *swapstate.Journal, record swapstate.Swap) error {
	var err error
	if record.AgreementJSON == "" || record.Phase == swapstate.PhaseMatched {
		record, err = s.initializeAsyncSwap(ctx, wallets, journal, record)
		if err != nil {
			return err
		}
	}
	if err := s.processPeerMessages(ctx, wallets, journal, record.ID); err != nil {
		return err
	}
	for range 16 {
		record, err = journal.Swap(record.ID)
		if err != nil {
			return err
		}
		agreement, err := agreementFromRecord(record)
		if err != nil {
			return err
		} else if err := agreement.ValidateAsync(record.Order, record.Acceptance); err != nil {
			return err
		}
		if record.Phase != swapstate.PhaseComplete && record.Phase != swapstate.PhaseRefunded && record.Phase != swapstate.PhaseExpired {
			contract, contractErr := bitcoinContract(agreement)
			if contractErr != nil {
				return contractErr
			}
			if watchErr := wallets.bitcoin.WatchContract(ctx, contract, bitcoinContractWatchHeight(agreement)); watchErr != nil {
				return fmt.Errorf("restore asynchronous Bitcoin contract watch: %w", watchErr)
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
		case swapstate.PhaseAsyncTakerFunding:
			notice, _, err := fundingFromRecord(record, swapprotocol.PartyTaker)
			if err != nil {
				return err
			}
			confirmed, err := fundingConfirmedForRecord(ctx, wallets, record, agreement, notice)
			if err != nil {
				return err
			}
			if confirmed && !record.TakerFundingSubmitted {
				record, err = journal.SetTakerFundingSubmitted(record.ID, true, time.Now().UTC())
				if err != nil {
					return err
				}
			}
			if !confirmed {
				if err := s.broadcastAsyncTakerFunding(ctx, wallets, record, agreement); err != nil {
					return err
				}
				record, err = journal.SetTakerFundingSubmitted(record.ID, true, time.Now().UTC())
				if err != nil {
					return err
				}
				confirmed, err = fundingConfirmedForRecord(ctx, wallets, record, agreement, notice)
				if err != nil || !confirmed {
					return err
				}
			}
			if _, err := journal.Advance(record.ID, record.Phase, swapstate.PhaseAsyncTakerFunded, time.Now().UTC()); err != nil {
				return err
			}
			continue

		case swapstate.PhaseAsyncTakerFunded:
			confirmed, err := partyFundingConfirmedForRecord(ctx, wallets, record, agreement, swapprotocol.PartyTaker)
			if err != nil {
				return err
			} else if !confirmed {
				if _, err := journal.RewindAfterReorg(record.ID, record.Phase, swapstate.PhaseAsyncTakerFunding, "taker funding lost confirmations", time.Now().UTC()); err != nil {
					return err
				}
				continue
			}
			if _, err := journal.Advance(record.ID, record.Phase, swapstate.PhaseAsyncMakerFunding, time.Now().UTC()); err != nil {
				return err
			}
			continue

		case swapstate.PhaseAsyncMakerFunding:
			confirmed, err := partyFundingConfirmedForRecord(ctx, wallets, record, agreement, swapprotocol.PartyTaker)
			if err != nil {
				return err
			} else if !confirmed {
				if _, err := journal.RewindAfterReorg(record.ID, record.Phase, swapstate.PhaseAsyncTakerFunding, "taker funding lost confirmations before maker funding", time.Now().UTC()); err != nil {
					return err
				}
				continue
			}
			if record.Role == swapstate.RoleMaker {
				if err := s.ensureMakerClaimTemplate(ctx, wallets, journal, record, agreement); err != nil {
					return err
				}
				record, _ = journal.Swap(record.ID)
				if err := s.ensureLocalFunding(ctx, wallets, journal, record, agreement, swapprotocol.PartyMaker); err != nil {
					return err
				}
				record, _ = journal.Swap(record.ID)
			}
			notice, ok, err := fundingFromRecord(record, swapprotocol.PartyMaker)
			if err != nil || !ok {
				return err
			}
			confirmed, err = fundingConfirmedForRecord(ctx, wallets, record, agreement, notice)
			if err != nil || !confirmed {
				return err
			}
			if _, err := journal.Advance(record.ID, record.Phase, swapstate.PhaseAsyncMakerFunded, time.Now().UTC()); err != nil {
				return err
			}
			continue

		case swapstate.PhaseAsyncMakerFunded:
			takerConfirmed, err := partyFundingConfirmedForRecord(ctx, wallets, record, agreement, swapprotocol.PartyTaker)
			if err != nil {
				return err
			} else if !takerConfirmed {
				if _, err := journal.RewindAfterReorg(record.ID, record.Phase, swapstate.PhaseAsyncTakerFunding, "taker funding lost confirmations", time.Now().UTC()); err != nil {
					return err
				}
				continue
			}
			makerConfirmed, err := partyFundingConfirmedForRecord(ctx, wallets, record, agreement, swapprotocol.PartyMaker)
			if err != nil {
				return err
			} else if !makerConfirmed {
				if _, err := journal.RewindAfterReorg(record.ID, record.Phase, swapstate.PhaseAsyncMakerFunding, "maker funding lost confirmations", time.Now().UTC()); err != nil {
					return err
				}
				continue
			}
			if record.Role == swapstate.RoleTaker && record.MakerClaimTemplate == "" {
				return nil
			}
			if _, err := journal.Advance(record.ID, record.Phase, swapstate.PhaseAsyncTakerClaiming, time.Now().UTC()); err != nil {
				return err
			}
			continue

		case swapstate.PhaseAsyncTakerClaiming:
			takerConfirmed, err := partyFundingConfirmedForRecord(ctx, wallets, record, agreement, swapprotocol.PartyTaker)
			if err != nil {
				return err
			} else if !takerConfirmed {
				if _, err := journal.RewindAfterReorg(record.ID, record.Phase, swapstate.PhaseAsyncTakerFunding, "taker funding lost before claim", time.Now().UTC()); err != nil {
					return err
				}
				continue
			}
			makerConfirmed, err := partyFundingConfirmedForRecord(ctx, wallets, record, agreement, swapprotocol.PartyMaker)
			if err != nil {
				return err
			} else if !makerConfirmed {
				if _, err := journal.RewindAfterReorg(record.ID, record.Phase, swapstate.PhaseAsyncMakerFunding, "maker funding lost before claim", time.Now().UTC()); err != nil {
					return err
				}
				continue
			}
			if record.Role == swapstate.RoleTaker {
				if record.MakerClaimTemplate == "" {
					return nil
				}
				if err := ensureLocalClaim(ctx, wallets, journal, record, agreement, swapprotocol.PartyTaker); err != nil {
					return err
				}
				if err := broadcastProxyMakerClaim(ctx, wallets, journal, record, agreement); err != nil {
					return err
				}
			}
			found, _, secret, err := observeClaimForRecord(ctx, wallets, record, agreement, swapprotocol.PartyTaker)
			if err != nil || !found {
				return err
			}
			if _, err := journal.SetRevealedSecret(record.ID, secret, time.Now().UTC()); err != nil {
				return err
			}
			if _, err := journal.Advance(record.ID, record.Phase, swapstate.PhaseAsyncTakerClaimed, time.Now().UTC()); err != nil {
				return err
			}
			continue

		case swapstate.PhaseAsyncTakerClaimed:
			found, _, _, err := observeClaimForRecord(ctx, wallets, record, agreement, swapprotocol.PartyTaker)
			if err != nil {
				return err
			} else if !found {
				if _, err := journal.RewindAfterReorg(record.ID, record.Phase, swapstate.PhaseAsyncTakerClaiming, "taker claim lost after reorg", time.Now().UTC()); err != nil {
					return err
				}
				continue
			}
			if _, err := journal.Advance(record.ID, record.Phase, swapstate.PhaseAsyncMakerClaiming, time.Now().UTC()); err != nil {
				return err
			}
			continue

		case swapstate.PhaseAsyncMakerClaiming:
			takerFound, takerConfirmed, _, err := observeClaimForRecord(ctx, wallets, record, agreement, swapprotocol.PartyTaker)
			if err != nil {
				return err
			} else if !takerFound {
				if _, err := journal.RewindAfterReorg(record.ID, record.Phase, swapstate.PhaseAsyncTakerClaiming, "taker claim lost after reorg", time.Now().UTC()); err != nil {
					return err
				}
				continue
			}
			takerFundingConfirmed, err := partyFundingConfirmedForRecord(ctx, wallets, record, agreement, swapprotocol.PartyTaker)
			if err != nil {
				return err
			} else if !takerFundingConfirmed {
				if _, err := journal.RewindAfterReorg(record.ID, record.Phase, swapstate.PhaseAsyncTakerFunding, "taker funding lost before maker claim", time.Now().UTC()); err != nil {
					return err
				}
				continue
			}
			if record.Role == swapstate.RoleMaker {
				if err := ensureLocalClaim(ctx, wallets, journal, record, agreement, swapprotocol.PartyMaker); err != nil {
					return err
				}
			} else if err := broadcastProxyMakerClaim(ctx, wallets, journal, record, agreement); err != nil {
				return err
			}
			found, confirmed, _, err := observeClaimForRecord(ctx, wallets, record, agreement, swapprotocol.PartyMaker)
			if err != nil || !found || !confirmed {
				return err
			}
			if !takerConfirmed {
				return nil
			}
			if _, err := journal.Advance(record.ID, record.Phase, swapstate.PhaseComplete, time.Now().UTC()); err != nil {
				return err
			}
			continue

		case swapstate.PhaseComplete:
			takerFound, takerConfirmed, _, err := observeClaimForRecord(ctx, wallets, record, agreement, swapprotocol.PartyTaker)
			if err != nil {
				return err
			} else if !takerFound || !takerConfirmed {
				if _, err := journal.RewindAfterReorg(record.ID, record.Phase, swapstate.PhaseAsyncTakerClaiming, "taker claim lost confirmations", time.Now().UTC()); err != nil {
					return err
				}
				continue
			}
			makerFound, makerConfirmed, _, err := observeClaimForRecord(ctx, wallets, record, agreement, swapprotocol.PartyMaker)
			if err != nil {
				return err
			} else if !makerFound || !makerConfirmed {
				if _, err := journal.RewindAfterReorg(record.ID, record.Phase, swapstate.PhaseAsyncMakerClaiming, "maker claim lost confirmations", time.Now().UTC()); err != nil {
					return err
				}
				continue
			}
			return nil

		case swapstate.PhaseWaitingForRefund:
			if _, err := journal.Advance(record.ID, record.Phase, swapstate.PhaseRefunding, time.Now().UTC()); err != nil {
				return err
			}
			continue

		case swapstate.PhaseRefunding:
			confirmed, err := ensureLocalRefund(ctx, wallets, journal, record, agreement)
			if err != nil || !confirmed {
				return err
			}
			if _, err := journal.Advance(record.ID, record.Phase, swapstate.PhaseRefunded, time.Now().UTC()); err != nil {
				return err
			}
			continue

		case swapstate.PhaseRefunded:
			confirmed, err := refundStateConfirmed(ctx, wallets, record, agreement)
			if err != nil {
				return err
			} else if !confirmed {
				if _, err := journal.RewindAfterReorg(record.ID, record.Phase, swapstate.PhaseRefunding, "refund lost confirmations", time.Now().UTC()); err != nil {
					return err
				}
				continue
			}
			return nil

		case swapstate.PhaseExpired:
			return nil
		default:
			return fmt.Errorf("asynchronous swap entered legacy phase %q", record.Phase)
		}
	}
	return errors.New("asynchronous swap engine exceeded its transition limit")
}

func (s *Service) ensureMakerClaimTemplate(ctx context.Context, wallets engineWallets, journal *swapstate.Journal, record swapstate.Swap, agreement swapprotocol.Agreement) error {
	if record.Role != swapstate.RoleMaker {
		return errors.New("only the maker can prepare its claim template")
	}
	if record.MakerClaimTemplate != "" {
		var template swapprotocol.ClaimTemplate
		if err := decodeStrictJSON([]byte(record.MakerClaimTemplate), &template); err != nil {
			return err
		}
		message := swapprotocol.Message{
			Version: swapprotocol.AsyncProtocolVersion, Kind: swapprotocol.MessageClaimTemplate,
			TradeID: record.ID, ClaimTemplate: &template,
		}
		return s.sendProtocolMessage(ctx, wallets.root, journal, record, "maker-claim-template", message)
	}
	funding, ok, err := fundingFromRecord(record, swapprotocol.PartyTaker)
	if err != nil {
		return err
	} else if !ok {
		return errors.New("taker funding is missing before maker claim template")
	}
	template := swapprotocol.ClaimTemplate{
		Party: swapprotocol.PartyMaker, Asset: funding.Asset, FundingTransactionID: funding.TransactionID,
	}
	if funding.Asset == "QDAY" {
		qday, ok := wallets.qday.(qdayAsyncSwapClient)
		if !ok {
			return errors.New("QDAY wallet does not support asynchronous claim templates")
		}
		view, err := qday.Swap(ctx, localQDAYSwapID(record))
		if err != nil {
			return err
		}
		var outputID string
		for _, output := range view.Outputs {
			if output.FundingTransaction == funding.TransactionID && output.Value.Atomic == agreement.QDAYAmountAtomic {
				outputID = output.ID
				break
			}
		}
		if outputID == "" {
			return errors.New("confirmed QDAY taker funding output is unavailable for the maker claim template")
		}
		action, err := qday.PrepareSwapClaimTemplate(ctx, localQDAYSwapID(record), walletd.SpendSwapRequest{OutputID: outputID})
		if err != nil {
			return err
		}
		template.TransactionID = action.TransactionID
		template.RawTransactions = []string{action.RawTransaction}
		template.BasisHeight, template.BasisID = action.BasisHeight, action.BasisID
	} else {
		contract, err := bitcoinContract(agreement)
		if err != nil {
			return err
		}
		btcFunding, err := contract.FundingFromRaw(funding.RawTransaction)
		if err != nil {
			return err
		}
		destination, err := wallets.bitcoin.DestinationScript()
		if err != nil {
			return err
		}
		key, err := wallets.root.BitcoinSwapKey(localBitcoinSwapKeyID(record))
		if err != nil {
			return err
		}
		raw, err := contract.BuildClaimTemplate(btcFunding, destination, bitcoinSpendFee(contract.Amount), key)
		if err != nil {
			return err
		}
		tx, err := bitcoin.DecodeTransaction(raw)
		if err != nil {
			return err
		}
		template.TransactionID = tx.TxID()
		template.RawTransactions = []string{raw}
	}
	if err := template.Validate(); err != nil {
		return err
	}
	encoded, _ := json.Marshal(template)
	record, err = journal.SetMakerClaimTemplate(record.ID, string(encoded), time.Now().UTC())
	if err != nil {
		return err
	}
	message := swapprotocol.Message{
		Version: swapprotocol.AsyncProtocolVersion, Kind: swapprotocol.MessageClaimTemplate,
		TradeID: record.ID, ClaimTemplate: &template,
	}
	return s.sendProtocolMessage(ctx, wallets.root, journal, record, "maker-claim-template", message)
}

func acceptMakerClaimTemplate(ctx context.Context, wallets engineWallets, journal *swapstate.Journal, record swapstate.Swap, template swapprotocol.ClaimTemplate) error {
	if record.Role != swapstate.RoleTaker {
		return errors.New("maker claim template was routed to the maker")
	} else if err := template.Validate(); err != nil {
		return err
	}
	agreement, err := agreementFromRecord(record)
	if err != nil {
		return err
	}
	funding, ok, err := fundingFromRecord(record, swapprotocol.PartyTaker)
	if err != nil {
		return err
	} else if !ok || template.Asset != funding.Asset || template.FundingTransactionID != funding.TransactionID {
		return errors.New("maker claim template does not spend the signed taker funding")
	}
	actionID := record.ID + ":maker-claim-proxy"
	if _, err := journal.Action(actionID); err == nil {
		encoded, _ := json.Marshal(template)
		_, err = journal.SetMakerClaimTemplate(record.ID, string(encoded), time.Now().UTC())
		return err
	} else if !errors.Is(err, swapstate.ErrNotFound) {
		return err
	}
	secret, hash, err := wallets.root.SwapSecret(record.ID)
	if err != nil {
		return err
	}
	defer clear(secret[:])
	if hex.EncodeToString(hash[:]) != agreement.SecretHash {
		return errors.New("local asynchronous secret differs from the signed agreement")
	}
	action := swapstate.Action{
		ID: actionID, SwapID: record.ID, Kind: "maker-claim-proxy", Chain: template.Asset,
		TransactionID: template.TransactionID,
	}
	if template.Asset == "BTC" {
		contract, err := bitcoinContract(agreement)
		if err != nil {
			return err
		}
		btcFunding, err := contract.FundingFromRaw(funding.RawTransaction)
		if err != nil {
			return err
		}
		action.RawTransaction, err = contract.CompleteClaimTemplate(template.RawTransactions[0], btcFunding, secret)
		if err != nil {
			return err
		}
		completed, err := bitcoin.DecodeTransaction(action.RawTransaction)
		if err != nil {
			return err
		} else if completed.TxID() != template.TransactionID {
			return errors.New("completing the Bitcoin claim template changed its transaction ID")
		}
	} else {
		qday, ok := wallets.qday.(qdayAsyncSwapClient)
		if !ok {
			return errors.New("QDAY wallet does not support asynchronous claim templates")
		}
		view, err := qday.Swap(ctx, localQDAYSwapID(record))
		if err != nil {
			return err
		}
		var outputID string
		for _, output := range view.Outputs {
			if output.FundingTransaction == funding.TransactionID && output.Value.Atomic == agreement.QDAYAmountAtomic {
				outputID = output.ID
				break
			}
		}
		if outputID == "" {
			return errors.New("QDAY taker funding output is unavailable for claim template completion")
		}
		completed, err := qday.CompleteSwapClaimTemplate(ctx, localQDAYSwapID(record), walletd.CompleteSwapClaimTemplateRequest{
			Package: walletd.TransactionPackage{
				BasisHeight: template.BasisHeight, BasisID: template.BasisID,
				Transactions: template.RawTransactions, TransactionIDs: []string{template.TransactionID},
			},
			OutputID: outputID, Secret: hex.EncodeToString(secret[:]),
		})
		if err != nil {
			return err
		}
		encoded, err := json.Marshal(completed)
		if err != nil {
			return err
		}
		action.RawTransaction = string(encoded)
	}
	if _, _, err := journal.PrepareAction(action, time.Now().UTC()); err != nil {
		return err
	}
	encoded, _ := json.Marshal(template)
	_, err = journal.SetMakerClaimTemplate(record.ID, string(encoded), time.Now().UTC())
	return err
}

func broadcastProxyMakerClaim(ctx context.Context, wallets engineWallets, journal *swapstate.Journal, record swapstate.Swap, agreement swapprotocol.Agreement) error {
	action, err := journal.Action(record.ID + ":maker-claim-proxy")
	if err != nil {
		return err
	}
	// A process can stop after the completed claim reached the network but before
	// the journal advanced. Once the spend is observable, broadcasting its old
	// portable QDAY package again is both unnecessary and invalid after the
	// transaction confirms because proof updating removes confirmed transactions.
	found, _, _, err := observeClaimForRecord(ctx, wallets, record, agreement, swapprotocol.PartyMaker)
	if err != nil {
		return err
	} else if found {
		if action.Status == swapstate.ActionPrepared {
			_, err = journal.MarkBroadcast(action.ID, time.Now().UTC())
		}
		return err
	}
	if action.Chain == "BTC" {
		transaction, err := wallets.bitcoin.Broadcast(action.RawTransaction)
		if err != nil {
			return err
		}
		_, err = reconcileBitcoinAction(journal, action, transaction)
		return err
	}
	qday, ok := wallets.qday.(qdayAsyncSwapClient)
	if !ok {
		return errors.New("QDAY wallet does not support portable claim broadcast")
	}
	var completed walletd.TransactionPackage
	if err := decodeStrictJSON([]byte(action.RawTransaction), &completed); err != nil {
		return err
	}
	if _, err := qday.BroadcastTransactionPackage(ctx, completed); err != nil {
		return err
	}
	if action.Status == swapstate.ActionPrepared {
		_, err = journal.MarkBroadcast(action.ID, time.Now().UTC())
	}
	return err
}

func fundingConfirmedForRecord(ctx context.Context, wallets engineWallets, record swapstate.Swap, agreement swapprotocol.Agreement, notice swapprotocol.FundingNotice) (bool, error) {
	if notice.Asset == "BTC" {
		transaction, err := wallets.bitcoin.Transaction(notice.TransactionID)
		if errors.Is(err, bitcoinwallet.ErrTransactionNotFound) {
			return false, nil
		} else if err != nil {
			return false, err
		}
		return transaction.Confirmations >= agreement.BitcoinConfirmations, nil
	}
	view, err := wallets.qday.Swap(ctx, localQDAYSwapID(record))
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

func fundingObservedForRecord(ctx context.Context, wallets engineWallets, record swapstate.Swap, agreement swapprotocol.Agreement, notice swapprotocol.FundingNotice) (bool, error) {
	if notice.Asset == "BTC" {
		_, err := wallets.bitcoin.Transaction(notice.TransactionID)
		if errors.Is(err, bitcoinwallet.ErrTransactionNotFound) {
			return false, nil
		}
		return err == nil, err
	}
	view, err := wallets.qday.Swap(ctx, localQDAYSwapID(record))
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
		return true, nil
	}
	return false, nil
}

func partyFundingConfirmedForRecord(ctx context.Context, wallets engineWallets, record swapstate.Swap, agreement swapprotocol.Agreement, party string) (bool, error) {
	notice, ok, err := fundingFromRecord(record, party)
	if err != nil || !ok {
		return false, err
	}
	return fundingConfirmedForRecord(ctx, wallets, record, agreement, notice)
}

func observeClaimForRecord(ctx context.Context, wallets engineWallets, record swapstate.Swap, agreement swapprotocol.Agreement, claimant string) (bool, bool, string, error) {
	return observeClaim(ctx, wallets, agreement, claimant, record)
}
