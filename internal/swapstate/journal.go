// Package swapstate persists the local view of each atomic swap before any
// network side effect is attempted. It provides compare-and-swap transitions
// so two workers cannot fund, claim or refund the same leg concurrently.
package swapstate

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/petoshi/qday-swap/internal/order"
	"github.com/petoshi/qday-swap/internal/trade"
	"go.etcd.io/bbolt"
)

var (
	tradesBucket              = []byte("trades")
	actionsBucket             = []byte("actions")
	pendingAcceptancesBucket  = []byte("pending-acceptances")
	incomingAcceptancesBucket = []byte("incoming-acceptances")
	relayMessagesBucket       = []byte("relay-messages")
	metadataBucket            = []byte("metadata")
	relayCursorKey            = []byte("relay-mailbox-cursor")

	ErrNotFound       = errors.New("swap not found")
	ErrPhaseChanged   = errors.New("swap phase changed")
	ErrInvalidAdvance = errors.New("invalid swap phase transition")
	ErrActionConflict = errors.New("action ID is bound to different transaction data")
)

type Role string

const (
	RoleMaker Role = "maker"
	RoleTaker Role = "taker"
)

type Phase string

const (
	PhaseMatched          Phase = "matched"
	PhaseTermsProposed    Phase = "terms_proposed"
	PhaseTermsAgreed      Phase = "terms_agreed"
	PhaseMakerFunding     Phase = "maker_funding"
	PhaseMakerFunded      Phase = "maker_funded"
	PhaseTakerFunding     Phase = "taker_funding"
	PhaseTakerFunded      Phase = "taker_funded"
	PhaseMakerClaiming    Phase = "maker_claiming"
	PhaseMakerClaimed     Phase = "maker_claimed"
	PhaseTakerClaiming    Phase = "taker_claiming"
	PhaseComplete         Phase = "complete"
	PhaseWaitingForRefund Phase = "waiting_for_refund"
	PhaseRefunding        Phase = "refunding"
	PhaseRefunded         Phase = "refunded"
	PhaseExpired          Phase = "expired"
)

type Swap struct {
	Version        uint16                 `json:"version"`
	ID             string                 `json:"id"`
	Role           Role                   `json:"role"`
	Phase          Phase                  `json:"phase"`
	Order          order.Signed           `json:"order"`
	Acceptance     trade.SignedAcceptance `json:"acceptance"`
	Match          trade.SignedMatch      `json:"match"`
	SecretHash     string                 `json:"secretHash,omitempty"`
	AgreementHash  string                 `json:"agreementHash,omitempty"`
	AgreementJSON  string                 `json:"agreementJSON,omitempty"`
	LocalHelloJSON string                 `json:"localHelloJSON,omitempty"`
	PeerHelloJSON  string                 `json:"peerHelloJSON,omitempty"`
	SentMessages   map[string]string      `json:"sentMessages,omitempty"`
	MakerFunding   string                 `json:"makerFunding,omitempty"`
	TakerFunding   string                 `json:"takerFunding,omitempty"`
	Approved       bool                   `json:"approved"`
	RevealedSecret string                 `json:"revealedSecret,omitempty"`
	MailboxCursor  uint64                 `json:"mailboxCursor"`
	LastError      string                 `json:"lastError,omitempty"`
	CreatedAt      int64                  `json:"createdAt"`
	UpdatedAt      int64                  `json:"updatedAt"`
}

type ActionStatus string

const (
	ActionPrepared  ActionStatus = "prepared"
	ActionBroadcast ActionStatus = "broadcast"
	ActionConfirmed ActionStatus = "confirmed"
)

type Action struct {
	ID             string       `json:"id"`
	SwapID         string       `json:"swapID"`
	Kind           string       `json:"kind"`
	Chain          string       `json:"chain"`
	RawTransaction string       `json:"rawTransaction"`
	TransactionID  string       `json:"transactionID"`
	Status         ActionStatus `json:"status"`
	BlockHeight    uint64       `json:"blockHeight,omitempty"`
	CreatedAt      int64        `json:"createdAt"`
	UpdatedAt      int64        `json:"updatedAt"`
}

type Negotiation struct {
	Order      order.Signed           `json:"order"`
	Acceptance trade.SignedAcceptance `json:"acceptance"`
	CreatedAt  int64                  `json:"createdAt"`
	UpdatedAt  int64                  `json:"updatedAt"`
}

type Journal struct{ db *bbolt.DB }

func Open(path string) (*Journal, error) {
	db, err := bbolt.Open(path, 0600, &bbolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open swap journal: %w", err)
	}
	if err := db.Update(func(tx *bbolt.Tx) error {
		for _, name := range [][]byte{tradesBucket, actionsBucket, pendingAcceptancesBucket, incomingAcceptancesBucket, relayMessagesBucket, metadataBucket} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initialize swap journal: %w", err)
	}
	return &Journal{db: db}, nil
}

func (j *Journal) Close() error { return j.db.Close() }

func (j *Journal) SavePendingAcceptance(signedOrder order.Signed, acceptance trade.SignedAcceptance, now time.Time) (Negotiation, bool, error) {
	return j.saveAcceptance(pendingAcceptancesBucket, signedOrder, acceptance, now)
}

func (j *Journal) SaveIncomingAcceptance(signedOrder order.Signed, acceptance trade.SignedAcceptance, now time.Time) (Negotiation, bool, error) {
	return j.saveAcceptance(incomingAcceptancesBucket, signedOrder, acceptance, now)
}

func (j *Journal) PendingAcceptances() ([]Negotiation, error) {
	return j.acceptances(pendingAcceptancesBucket)
}

func (j *Journal) IncomingAcceptances() ([]Negotiation, error) {
	return j.acceptances(incomingAcceptancesBucket)
}

func (j *Journal) PendingAcceptanceByMatch(match trade.SignedMatch) (Negotiation, error) {
	records, err := j.PendingAcceptances()
	if err != nil {
		return Negotiation{}, err
	}
	for _, record := range records {
		if record.Acceptance.ID == match.Match.AcceptanceID && record.Acceptance.Acceptance.TradeID == match.Match.TradeID {
			return record, nil
		}
	}
	return Negotiation{}, ErrNotFound
}

func (j *Journal) IncomingAcceptance(id string) (Negotiation, error) {
	var record Negotiation
	err := j.db.View(func(tx *bbolt.Tx) error {
		encoded := tx.Bucket(incomingAcceptancesBucket).Get([]byte(id))
		if encoded == nil {
			return ErrNotFound
		}
		return json.Unmarshal(encoded, &record)
	})
	return record, err
}

func (j *Journal) RemovePendingAcceptance(id string) error {
	return j.removeAcceptance(pendingAcceptancesBucket, id)
}

func (j *Journal) RemoveIncomingAcceptance(id string) error {
	return j.removeAcceptance(incomingAcceptancesBucket, id)
}

func (j *Journal) SaveRelayMessage(message trade.SignedMessage, now time.Time) (bool, error) {
	// Mailbox delivery can happen after a message expires. Its signed creation
	// time is the stable point for structural and signature verification.
	if err := message.Verify(time.Unix(message.Message.CreatedAt, 0)); err != nil {
		return false, err
	}
	created := false
	err := j.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(relayMessagesBucket)
		if existing := bucket.Get([]byte(message.ID)); existing != nil {
			return nil
		}
		encoded, err := json.Marshal(message)
		if err != nil {
			return err
		}
		created = true
		return bucket.Put([]byte(message.ID), encoded)
	})
	return created, err
}

// SaveOutboundMessage persists the exact encrypted envelope and binds it to a
// stable protocol step in the same database transaction. A restart therefore
// retries the same ciphertext and ID instead of reusing a sequence number with
// a different nonce.
func (j *Journal) SaveOutboundMessage(swapID, messageKey string, message trade.SignedMessage, now time.Time) (trade.SignedMessage, bool, error) {
	if strings.TrimSpace(messageKey) == "" || len(messageKey) > 128 {
		return trade.SignedMessage{}, false, errors.New("outbound message key is invalid")
	} else if message.Message.TradeID != swapID {
		return trade.SignedMessage{}, false, errors.New("outbound message belongs to another swap")
	} else if err := message.Verify(time.Unix(message.Message.CreatedAt, 0)); err != nil {
		return trade.SignedMessage{}, false, err
	}
	created := false
	err := j.db.Update(func(tx *bbolt.Tx) error {
		trades := tx.Bucket(tradesBucket)
		encodedSwap := trades.Get([]byte(swapID))
		if encodedSwap == nil {
			return ErrNotFound
		}
		var record Swap
		if err := json.Unmarshal(encodedSwap, &record); err != nil {
			return err
		}
		if existingID := record.SentMessages[messageKey]; existingID != "" {
			encoded := tx.Bucket(relayMessagesBucket).Get([]byte(existingID))
			if encoded == nil {
				return errors.New("outbound message journal is inconsistent")
			}
			var existing trade.SignedMessage
			if err := json.Unmarshal(encoded, &existing); err != nil {
				return err
			}
			if existing.ID != message.ID {
				return errors.New("outbound protocol step is immutable")
			}
			message = existing
			return nil
		}
		messages := tx.Bucket(relayMessagesBucket)
		if encoded := messages.Get([]byte(message.ID)); encoded != nil {
			var existing trade.SignedMessage
			if err := json.Unmarshal(encoded, &existing); err != nil {
				return err
			}
			if existing.Message.TradeID != swapID {
				return errors.New("relay message ID belongs to another swap")
			}
		} else {
			encoded, err := json.Marshal(message)
			if err != nil {
				return err
			}
			if err := messages.Put([]byte(message.ID), encoded); err != nil {
				return err
			}
		}
		if record.SentMessages == nil {
			record.SentMessages = make(map[string]string)
		}
		record.SentMessages[messageKey] = message.ID
		record.UpdatedAt = now.Unix()
		encodedSwap, err := json.Marshal(record)
		if err != nil {
			return err
		}
		created = true
		return trades.Put([]byte(swapID), encodedSwap)
	})
	return message, created, err
}

func (j *Journal) RelayMessage(id string) (trade.SignedMessage, error) {
	var message trade.SignedMessage
	err := j.db.View(func(tx *bbolt.Tx) error {
		encoded := tx.Bucket(relayMessagesBucket).Get([]byte(id))
		if encoded == nil {
			return ErrNotFound
		}
		return json.Unmarshal(encoded, &message)
	})
	return message, err
}

func (j *Journal) RelayMessages(tradeID string) ([]trade.SignedMessage, error) {
	var records []trade.SignedMessage
	err := j.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(relayMessagesBucket).ForEach(func(_, encoded []byte) error {
			var message trade.SignedMessage
			if err := json.Unmarshal(encoded, &message); err != nil {
				return err
			}
			if message.Message.TradeID == tradeID {
				records = append(records, message)
			}
			return nil
		})
	})
	sort.Slice(records, func(i, k int) bool { return records[i].Message.Sequence < records[k].Message.Sequence })
	if records == nil {
		records = []trade.SignedMessage{}
	}
	return records, err
}

func (j *Journal) RelayCursor() (uint64, error) {
	var cursor uint64
	err := j.db.View(func(tx *bbolt.Tx) error {
		encoded := tx.Bucket(metadataBucket).Get(relayCursorKey)
		if len(encoded) == 8 {
			cursor = binary.BigEndian.Uint64(encoded)
		}
		return nil
	})
	return cursor, err
}

func (j *Journal) SetRelayCursor(cursor uint64) error {
	return j.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(metadataBucket)
		current := bucket.Get(relayCursorKey)
		if len(current) == 8 && cursor < binary.BigEndian.Uint64(current) {
			return errors.New("relay mailbox cursor cannot move backwards")
		}
		var encoded [8]byte
		binary.BigEndian.PutUint64(encoded[:], cursor)
		return bucket.Put(relayCursorKey, encoded[:])
	})
}

func (j *Journal) saveAcceptance(bucketName []byte, signedOrder order.Signed, acceptance trade.SignedAcceptance, now time.Time) (Negotiation, bool, error) {
	if err := acceptance.Verify(signedOrder, now); err != nil {
		return Negotiation{}, false, err
	}
	record := Negotiation{Order: signedOrder, Acceptance: acceptance, CreatedAt: now.Unix(), UpdatedAt: now.Unix()}
	created := false
	err := j.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketName)
		key := []byte(acceptance.ID)
		if encoded := bucket.Get(key); encoded != nil {
			var existing Negotiation
			if err := json.Unmarshal(encoded, &existing); err != nil {
				return err
			}
			if existing.Order.ID != signedOrder.ID || existing.Acceptance.Acceptance.TradeID != acceptance.Acceptance.TradeID {
				return errors.New("acceptance ID is already bound to different negotiation data")
			}
			record = existing
			return nil
		}
		encoded, err := json.Marshal(record)
		if err != nil {
			return err
		}
		created = true
		return bucket.Put(key, encoded)
	})
	return record, created, err
}

func (j *Journal) acceptances(bucketName []byte) ([]Negotiation, error) {
	var records []Negotiation
	err := j.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketName).ForEach(func(_, encoded []byte) error {
			var record Negotiation
			if err := json.Unmarshal(encoded, &record); err != nil {
				return err
			}
			records = append(records, record)
			return nil
		})
	})
	sort.Slice(records, func(i, k int) bool { return records[i].UpdatedAt > records[k].UpdatedAt })
	if records == nil {
		records = []Negotiation{}
	}
	return records, err
}

func (j *Journal) removeAcceptance(bucketName []byte, id string) error {
	return j.db.Update(func(tx *bbolt.Tx) error { return tx.Bucket(bucketName).Delete([]byte(id)) })
}

func (j *Journal) Create(role Role, signedOrder order.Signed, acceptance trade.SignedAcceptance, match trade.SignedMatch, now time.Time) (Swap, bool, error) {
	if role != RoleMaker && role != RoleTaker {
		return Swap{}, false, errors.New("swap role must be maker or taker")
	}
	// A matched swap remains valid after its short acceptance window expires.
	// Verify against the signed match time rather than the local restart time.
	if err := match.Verify(signedOrder, acceptance, time.Unix(match.Match.CreatedAt, 0)); err != nil {
		return Swap{}, false, err
	}
	record := Swap{
		Version: 1, ID: acceptance.Acceptance.TradeID, Role: role, Phase: PhaseMatched,
		Order: signedOrder, Acceptance: acceptance, Match: match,
		CreatedAt: now.Unix(), UpdatedAt: now.Unix(),
	}
	created := false
	err := j.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(tradesBucket)
		if encoded := bucket.Get([]byte(record.ID)); encoded != nil {
			var existing Swap
			if err := json.Unmarshal(encoded, &existing); err != nil {
				return err
			}
			if existing.Order.ID != record.Order.ID || existing.Acceptance.ID != record.Acceptance.ID || existing.Match.ID != record.Match.ID || existing.Role != role {
				return errors.New("trade ID is already bound to another swap")
			}
			record = existing
			return nil
		}
		encoded, err := json.Marshal(record)
		if err != nil {
			return err
		}
		created = true
		return bucket.Put([]byte(record.ID), encoded)
	})
	return record, created, err
}

func (j *Journal) Swap(id string) (Swap, error) {
	var record Swap
	err := j.db.View(func(tx *bbolt.Tx) error {
		encoded := tx.Bucket(tradesBucket).Get([]byte(id))
		if encoded == nil {
			return ErrNotFound
		}
		return json.Unmarshal(encoded, &record)
	})
	return record, err
}

func (j *Journal) List() ([]Swap, error) {
	var records []Swap
	err := j.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(tradesBucket).ForEach(func(_, encoded []byte) error {
			var record Swap
			if err := json.Unmarshal(encoded, &record); err != nil {
				return err
			}
			records = append(records, record)
			return nil
		})
	})
	sort.Slice(records, func(i, k int) bool { return records[i].UpdatedAt > records[k].UpdatedAt })
	if records == nil {
		records = []Swap{}
	}
	return records, err
}

func (j *Journal) Advance(id string, expected, next Phase, now time.Time) (Swap, error) {
	if !validAdvance(expected, next) {
		return Swap{}, ErrInvalidAdvance
	}
	return j.updateSwap(id, func(record *Swap) error {
		if record.Phase != expected {
			return ErrPhaseChanged
		}
		record.Phase = next
		record.LastError = ""
		record.UpdatedAt = now.Unix()
		return nil
	})
}

func (j *Journal) RewindAfterReorg(id string, expected, next Phase, reason string, now time.Time) (Swap, error) {
	if !validRewind(expected, next) {
		return Swap{}, ErrInvalidAdvance
	}
	return j.updateSwap(id, func(record *Swap) error {
		if record.Phase != expected {
			return ErrPhaseChanged
		}
		record.Phase = next
		record.LastError = reason
		record.UpdatedAt = now.Unix()
		return nil
	})
}

func (j *Journal) SetAgreement(id, secretHash, agreementHash string, expected Phase, now time.Time) (Swap, error) {
	if len(secretHash) != 64 || len(agreementHash) != 64 {
		return Swap{}, errors.New("secret and agreement hashes must be 32-byte hexadecimal strings")
	}
	return j.updateSwap(id, func(record *Swap) error {
		if record.Phase != expected {
			return ErrPhaseChanged
		}
		if record.SecretHash != "" && (record.SecretHash != secretHash || record.AgreementHash != agreementHash) {
			return errors.New("swap agreement is immutable")
		}
		record.SecretHash = secretHash
		record.AgreementHash = agreementHash
		record.UpdatedAt = now.Unix()
		return nil
	})
}

func (j *Journal) SetHello(id string, local bool, helloJSON string, now time.Time) (Swap, error) {
	if helloJSON == "" || !json.Valid([]byte(helloJSON)) {
		return Swap{}, errors.New("swap hello must be valid JSON")
	}
	return j.updateSwap(id, func(record *Swap) error {
		current := &record.PeerHelloJSON
		if local {
			current = &record.LocalHelloJSON
		}
		if *current != "" && *current != helloJSON {
			return errors.New("swap hello is immutable")
		}
		*current = helloJSON
		record.UpdatedAt = now.Unix()
		return nil
	})
}

func (j *Journal) SetFunding(id, party, fundingJSON string, now time.Time) (Swap, error) {
	if party != "maker" && party != "taker" {
		return Swap{}, errors.New("funding party must be maker or taker")
	} else if fundingJSON == "" || !json.Valid([]byte(fundingJSON)) {
		return Swap{}, errors.New("funding notice must be valid JSON")
	}
	return j.updateSwap(id, func(record *Swap) error {
		current := &record.MakerFunding
		if party == "taker" {
			current = &record.TakerFunding
		}
		if *current != "" && *current != fundingJSON {
			return errors.New("swap funding notice is immutable")
		}
		*current = fundingJSON
		record.UpdatedAt = now.Unix()
		return nil
	})
}

func (j *Journal) SetAgreementDetails(id, secretHash, agreementHash, agreementJSON string, expected Phase, now time.Time) (Swap, error) {
	if len(secretHash) != 64 || len(agreementHash) != 64 {
		return Swap{}, errors.New("secret and agreement hashes must be 32-byte hexadecimal strings")
	} else if agreementJSON == "" || !json.Valid([]byte(agreementJSON)) {
		return Swap{}, errors.New("swap agreement must be valid JSON")
	}
	return j.updateSwap(id, func(record *Swap) error {
		if record.Phase != expected {
			return ErrPhaseChanged
		}
		if record.SecretHash != "" && (record.SecretHash != secretHash || record.AgreementHash != agreementHash || record.AgreementJSON != agreementJSON) {
			return errors.New("swap agreement is immutable")
		}
		record.SecretHash = secretHash
		record.AgreementHash = agreementHash
		record.AgreementJSON = agreementJSON
		record.UpdatedAt = now.Unix()
		return nil
	})
}

func (j *Journal) Approve(id string, now time.Time) (Swap, error) {
	return j.updateSwap(id, func(record *Swap) error {
		switch record.Phase {
		case PhaseTermsAgreed, PhaseMakerFunding, PhaseMakerFunded, PhaseTakerFunding, PhaseTakerFunded,
			PhaseMakerClaiming, PhaseMakerClaimed, PhaseTakerClaiming:
		default:
			return errors.New("swap terms are not ready for funding approval")
		}
		if record.AgreementJSON == "" {
			return errors.New("swap agreement is missing")
		}
		record.Approved = true
		record.UpdatedAt = now.Unix()
		return nil
	})
}

func (j *Journal) SetRevealedSecret(id, secret string, now time.Time) (Swap, error) {
	decoded, err := hex.DecodeString(secret)
	if err != nil || len(decoded) != sha256.Size || secret != strings.ToLower(secret) {
		return Swap{}, errors.New("revealed secret must be 32 lowercase hexadecimal bytes")
	}
	return j.updateSwap(id, func(record *Swap) error {
		hash := sha256.Sum256(decoded)
		if hex.EncodeToString(hash[:]) != record.SecretHash {
			return errors.New("revealed secret does not match the swap hash")
		}
		if record.RevealedSecret != "" && record.RevealedSecret != secret {
			return errors.New("swap secret is immutable")
		}
		record.RevealedSecret = secret
		record.UpdatedAt = now.Unix()
		return nil
	})
}

func (j *Journal) SetLastError(id, message string, now time.Time) (Swap, error) {
	if len(message) > 2_000 {
		message = message[:2_000]
	}
	return j.updateSwap(id, func(record *Swap) error {
		record.LastError = message
		record.UpdatedAt = now.Unix()
		return nil
	})
}

func (j *Journal) SetMailboxCursor(id string, cursor uint64, now time.Time) (Swap, error) {
	return j.updateSwap(id, func(record *Swap) error {
		if cursor < record.MailboxCursor {
			return errors.New("mailbox cursor cannot move backwards")
		}
		record.MailboxCursor = cursor
		record.UpdatedAt = now.Unix()
		return nil
	})
}

func (j *Journal) PrepareAction(action Action, now time.Time) (Action, bool, error) {
	if action.ID == "" || action.SwapID == "" || action.Kind == "" || action.Chain == "" || action.RawTransaction == "" || action.TransactionID == "" {
		return Action{}, false, errors.New("prepared action is incomplete")
	}
	if _, err := j.Swap(action.SwapID); err != nil {
		return Action{}, false, err
	}
	action.Status = ActionPrepared
	action.CreatedAt = now.Unix()
	action.UpdatedAt = now.Unix()
	created := false
	err := j.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(actionsBucket)
		if encoded := bucket.Get([]byte(action.ID)); encoded != nil {
			var existing Action
			if err := json.Unmarshal(encoded, &existing); err != nil {
				return err
			}
			if existing.SwapID != action.SwapID || existing.Kind != action.Kind || existing.Chain != action.Chain || existing.RawTransaction != action.RawTransaction || existing.TransactionID != action.TransactionID {
				return ErrActionConflict
			}
			action = existing
			return nil
		}
		encoded, err := json.Marshal(action)
		if err != nil {
			return err
		}
		created = true
		return bucket.Put([]byte(action.ID), encoded)
	})
	return action, created, err
}

func (j *Journal) MarkBroadcast(id string, now time.Time) (Action, error) {
	return j.updateAction(id, func(action *Action) error {
		if action.Status == ActionBroadcast {
			return nil
		}
		if action.Status != ActionPrepared {
			return errors.New("only a prepared action can be marked broadcast")
		}
		action.Status = ActionBroadcast
		action.UpdatedAt = now.Unix()
		return nil
	})
}

func (j *Journal) MarkConfirmed(id string, height uint64, now time.Time) (Action, error) {
	if height == 0 {
		return Action{}, errors.New("confirmation height must be positive")
	}
	return j.updateAction(id, func(action *Action) error {
		if action.Status != ActionBroadcast && action.Status != ActionConfirmed {
			return errors.New("only a broadcast action can be confirmed")
		}
		action.Status = ActionConfirmed
		action.BlockHeight = height
		action.UpdatedAt = now.Unix()
		return nil
	})
}

func (j *Journal) MarkReorged(id string, now time.Time) (Action, error) {
	return j.updateAction(id, func(action *Action) error {
		if action.Status != ActionConfirmed {
			return errors.New("only a confirmed action can be reorganized")
		}
		action.Status = ActionBroadcast
		action.BlockHeight = 0
		action.UpdatedAt = now.Unix()
		return nil
	})
}

func (j *Journal) Actions(swapID string) ([]Action, error) {
	var actions []Action
	err := j.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(actionsBucket).ForEach(func(_, encoded []byte) error {
			var action Action
			if err := json.Unmarshal(encoded, &action); err != nil {
				return err
			}
			if action.SwapID == swapID {
				actions = append(actions, action)
			}
			return nil
		})
	})
	sort.Slice(actions, func(i, k int) bool { return actions[i].CreatedAt < actions[k].CreatedAt })
	if actions == nil {
		actions = []Action{}
	}
	return actions, err
}

func (j *Journal) Action(id string) (Action, error) {
	var action Action
	err := j.db.View(func(tx *bbolt.Tx) error {
		encoded := tx.Bucket(actionsBucket).Get([]byte(id))
		if encoded == nil {
			return ErrNotFound
		}
		return json.Unmarshal(encoded, &action)
	})
	return action, err
}

func (j *Journal) updateSwap(id string, update func(*Swap) error) (Swap, error) {
	var record Swap
	err := j.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(tradesBucket)
		encoded := bucket.Get([]byte(id))
		if encoded == nil {
			return ErrNotFound
		}
		if err := json.Unmarshal(encoded, &record); err != nil {
			return err
		}
		if err := update(&record); err != nil {
			return err
		}
		encoded, err := json.Marshal(record)
		if err != nil {
			return err
		}
		return bucket.Put([]byte(id), encoded)
	})
	return record, err
}

func (j *Journal) updateAction(id string, update func(*Action) error) (Action, error) {
	var action Action
	err := j.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(actionsBucket)
		encoded := bucket.Get([]byte(id))
		if encoded == nil {
			return errors.New("action not found")
		}
		if err := json.Unmarshal(encoded, &action); err != nil {
			return err
		}
		if err := update(&action); err != nil {
			return err
		}
		encoded, err := json.Marshal(action)
		if err != nil {
			return err
		}
		return bucket.Put([]byte(id), encoded)
	})
	return action, err
}

func validAdvance(from, to Phase) bool {
	allowed := map[Phase][]Phase{
		PhaseMatched:          {PhaseTermsProposed, PhaseWaitingForRefund},
		PhaseTermsProposed:    {PhaseTermsAgreed, PhaseWaitingForRefund, PhaseExpired},
		PhaseTermsAgreed:      {PhaseMakerFunding, PhaseWaitingForRefund, PhaseExpired},
		PhaseMakerFunding:     {PhaseMakerFunded, PhaseWaitingForRefund, PhaseExpired},
		PhaseMakerFunded:      {PhaseTakerFunding, PhaseWaitingForRefund, PhaseExpired},
		PhaseTakerFunding:     {PhaseTakerFunded, PhaseWaitingForRefund, PhaseExpired},
		PhaseTakerFunded:      {PhaseMakerClaiming, PhaseWaitingForRefund},
		PhaseMakerClaiming:    {PhaseMakerClaimed, PhaseWaitingForRefund},
		PhaseMakerClaimed:     {PhaseTakerClaiming, PhaseWaitingForRefund},
		PhaseTakerClaiming:    {PhaseComplete, PhaseWaitingForRefund},
		PhaseWaitingForRefund: {PhaseRefunding},
		PhaseRefunding:        {PhaseRefunded},
	}
	for _, candidate := range allowed[from] {
		if candidate == to {
			return true
		}
	}
	return false
}

func validRewind(from, to Phase) bool {
	return (from == PhaseMakerFunded && to == PhaseMakerFunding) ||
		(from == PhaseTakerFunding && to == PhaseMakerFunding) ||
		(from == PhaseTakerFunded && to == PhaseMakerFunding) ||
		(from == PhaseMakerClaiming && to == PhaseMakerFunding) ||
		(from == PhaseMakerClaiming && to == PhaseTakerFunding) ||
		(from == PhaseTakerFunded && to == PhaseTakerFunding) ||
		(from == PhaseMakerClaimed && to == PhaseMakerClaiming) ||
		(from == PhaseTakerClaiming && to == PhaseMakerClaiming) ||
		(from == PhaseComplete && to == PhaseMakerClaiming) ||
		(from == PhaseComplete && to == PhaseTakerClaiming) ||
		(from == PhaseRefunded && to == PhaseRefunding)
}
