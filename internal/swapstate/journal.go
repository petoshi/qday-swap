// Package swapstate persists the local view of each atomic swap before any
// network side effect is attempted. It provides compare-and-swap transitions
// so two workers cannot fund, claim or refund the same leg concurrently.
package swapstate

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
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
)

type Swap struct {
	Version       uint16                 `json:"version"`
	ID            string                 `json:"id"`
	Role          Role                   `json:"role"`
	Phase         Phase                  `json:"phase"`
	Order         order.Signed           `json:"order"`
	Acceptance    trade.SignedAcceptance `json:"acceptance"`
	Match         trade.SignedMatch      `json:"match"`
	SecretHash    string                 `json:"secretHash,omitempty"`
	AgreementHash string                 `json:"agreementHash,omitempty"`
	MailboxCursor uint64                 `json:"mailboxCursor"`
	LastError     string                 `json:"lastError,omitempty"`
	CreatedAt     int64                  `json:"createdAt"`
	UpdatedAt     int64                  `json:"updatedAt"`
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
		PhaseTermsProposed:    {PhaseTermsAgreed, PhaseWaitingForRefund},
		PhaseTermsAgreed:      {PhaseMakerFunding, PhaseWaitingForRefund},
		PhaseMakerFunding:     {PhaseMakerFunded, PhaseWaitingForRefund},
		PhaseMakerFunded:      {PhaseTakerFunding, PhaseWaitingForRefund},
		PhaseTakerFunding:     {PhaseTakerFunded, PhaseWaitingForRefund},
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
		(from == PhaseTakerFunded && to == PhaseTakerFunding) ||
		(from == PhaseMakerClaimed && to == PhaseMakerClaiming) ||
		(from == PhaseComplete && to == PhaseTakerClaiming) ||
		(from == PhaseRefunded && to == PhaseRefunding)
}
