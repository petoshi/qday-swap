package relay

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/petoshi/qday-swap/internal/trade"
	"go.etcd.io/bbolt"
)

const MaxPendingAcceptancesPerOrder = 100

type AcceptanceRecord struct {
	Signed       trade.SignedAcceptance              `json:"signed"`
	ReceivedAt   int64                               `json:"receivedAt"`
	CancelledAt  int64                               `json:"cancelledAt,omitempty"`
	Cancellation *trade.SignedAcceptanceCancellation `json:"cancellation,omitempty"`
}

type AcceptanceCancellationRecord struct {
	Signed          trade.SignedAcceptanceCancellation `json:"signed"`
	ReceivedAt      int64                              `json:"receivedAt"`
	AcceptanceKnown bool                               `json:"acceptanceKnown"`
}

type MailboxKind string

const (
	MailboxAcceptance MailboxKind = "acceptance"
	MailboxMatch      MailboxKind = "match"
	MailboxMessage    MailboxKind = "message"
)

type MailboxItem struct {
	Cursor     uint64               `json:"cursor"`
	Kind       MailboxKind          `json:"kind"`
	ReceivedAt int64                `json:"receivedAt"`
	Acceptance *AcceptanceRecord    `json:"acceptance,omitempty"`
	Match      *trade.SignedMatch   `json:"match,omitempty"`
	Message    *trade.SignedMessage `json:"message,omitempty"`
}

type MailboxPage struct {
	Items   []MailboxItem `json:"items"`
	After   uint64        `json:"after"`
	Next    uint64        `json:"next"`
	HasMore bool          `json:"hasMore"`
}

func (s *Store) SubmitAcceptance(orderID string, acceptance trade.SignedAcceptance, now time.Time) (AcceptanceRecord, bool, error) {
	var result AcceptanceRecord
	created := false
	err := s.db.Update(func(tx *bbolt.Tx) error {
		orderRecord, err := orderRecordIn(tx, orderID, now)
		if err != nil {
			return err
		}
		if orderRecord.EffectiveStatus(now) != StatusOpen {
			return ErrNotOpen
		}
		if err := acceptance.Verify(orderRecord.Signed, now); err != nil {
			return err
		}

		bucket := tx.Bucket(acceptancesBucket)
		if existing := bucket.Get([]byte(acceptance.ID)); existing != nil {
			if err := json.Unmarshal(existing, &result); err != nil {
				return err
			}
			if result.CancelledAt != 0 {
				return ErrAcceptanceCancelled
			}
			return nil
		}
		cancellations := tx.Bucket(acceptanceCancellationsBucket)
		cancelKey := acceptanceCancellationKey(acceptance.ID, acceptance.Acceptance.TakerPublicKey)
		if encoded := cancellations.Get(cancelKey); encoded != nil {
			var cancellation AcceptanceCancellationRecord
			if err := json.Unmarshal(encoded, &cancellation); err != nil {
				return err
			}
			if err := cancellation.Signed.VerifyAcceptance(acceptance, now); err == nil {
				return ErrAcceptanceCancelled
			}
			// A cancellation under the correct composite key should always bind
			// this acceptance. Drop malformed legacy data rather than blocking it.
			if err := cancellations.Delete(cancelKey); err != nil {
				return err
			}
		}
		pending := 0
		if err := bucket.ForEach(func(_, value []byte) error {
			var candidate AcceptanceRecord
			if err := json.Unmarshal(value, &candidate); err != nil {
				return err
			}
			payload := candidate.Signed.Acceptance
			if payload.OrderID != orderID || payload.ExpiresAt <= now.Unix() || candidate.CancelledAt != 0 {
				return nil
			}
			pending++
			if payload.TakerPublicKey == acceptance.Acceptance.TakerPublicKey {
				return ErrAlreadyAccepted
			}
			return nil
		}); err != nil {
			return err
		}
		if pending >= MaxPendingAcceptancesPerOrder {
			return ErrAcceptanceLimit
		}

		result = AcceptanceRecord{Signed: acceptance, ReceivedAt: now.Unix()}
		encoded, err := json.Marshal(result)
		if err != nil {
			return err
		}
		if err := bucket.Put([]byte(acceptance.ID), encoded); err != nil {
			return err
		}
		item := MailboxItem{Kind: MailboxAcceptance, ReceivedAt: now.Unix(), Acceptance: &result}
		if _, err := appendMailbox(tx, orderRecord.Signed.Order.MakerPublicKey, item); err != nil {
			return err
		}
		created = true
		return nil
	})
	return result, created, err
}

// CancelAcceptance atomically revokes an acceptance unless the relay has
// already committed it to a match. Unknown acceptances are tombstoned as well,
// which closes the race where an earlier timed-out submit arrives late.
func (s *Store) CancelAcceptance(orderID, acceptanceID string, cancellation trade.SignedAcceptanceCancellation, now time.Time) (AcceptanceCancellationRecord, bool, error) {
	var result AcceptanceCancellationRecord
	created := false
	if err := cancellation.Verify(now); err != nil {
		return result, false, err
	}
	payload := cancellation.Cancellation
	if payload.OrderID != orderID || payload.AcceptanceID != acceptanceID {
		return result, false, errors.New("acceptance cancellation path does not match signed data")
	}
	err := s.db.Update(func(tx *bbolt.Tx) error {
		orderRecord, err := orderRecordIn(tx, orderID, now)
		if err != nil {
			return err
		}
		if payload.Network != orderRecord.Signed.Order.Network {
			return errors.New("acceptance cancellation belongs to a different network")
		}
		if payload.TakerPublicKey == orderRecord.Signed.Order.MakerPublicKey {
			return errors.New("maker cannot cancel a taker acceptance")
		}
		if orderRecord.Status == StatusMatched && orderRecord.Acceptance != nil && orderRecord.Acceptance.ID == acceptanceID {
			return ErrAcceptanceMatched
		}

		cancellations := tx.Bucket(acceptanceCancellationsBucket)
		key := acceptanceCancellationKey(acceptanceID, payload.TakerPublicKey)
		if existing := cancellations.Get(key); existing != nil {
			return json.Unmarshal(existing, &result)
		}
		cancellationCount := 0
		if err := cancellations.ForEach(func(_, value []byte) error {
			var candidate AcceptanceCancellationRecord
			if err := json.Unmarshal(value, &candidate); err != nil {
				return err
			}
			if candidate.Signed.Cancellation.OrderID == orderID {
				cancellationCount++
			}
			return nil
		}); err != nil {
			return err
		}
		if cancellationCount >= MaxAcceptanceCancellationsPerOrder {
			return ErrAcceptanceCancellationLimit
		}

		var acceptance AcceptanceRecord
		acceptanceBucket := tx.Bucket(acceptancesBucket)
		if encoded := acceptanceBucket.Get([]byte(acceptanceID)); encoded != nil {
			if err := json.Unmarshal(encoded, &acceptance); err != nil {
				return err
			}
			if err := cancellation.VerifyAcceptance(acceptance.Signed, now); err != nil {
				return err
			}
			result.AcceptanceKnown = true
		}
		result.Signed = cancellation
		result.ReceivedAt = now.Unix()
		encodedCancellation, err := json.Marshal(result)
		if err != nil {
			return err
		}
		if err := cancellations.Put(key, encodedCancellation); err != nil {
			return err
		}
		if result.AcceptanceKnown {
			acceptance.CancelledAt = now.Unix()
			acceptance.Cancellation = &cancellation
			encodedAcceptance, err := json.Marshal(acceptance)
			if err != nil {
				return err
			}
			if err := acceptanceBucket.Put([]byte(acceptanceID), encodedAcceptance); err != nil {
				return err
			}
		}
		created = true
		return nil
	})
	return result, created, err
}

func (s *Store) ConfirmMatch(orderID string, match trade.SignedMatch, now time.Time) (Record, bool, error) {
	var result Record
	changed := false
	err := s.db.Update(func(tx *bbolt.Tx) error {
		orderRecord, err := orderRecordIn(tx, orderID, now)
		if err != nil {
			return err
		}
		if orderRecord.Status == StatusMatched && orderRecord.Match != nil && orderRecord.Match.ID == match.ID {
			result = orderRecord
			return nil
		}
		if orderRecord.EffectiveStatus(now) != StatusOpen {
			return ErrNotOpen
		}
		encodedAcceptance := tx.Bucket(acceptancesBucket).Get([]byte(match.Match.AcceptanceID))
		if encodedAcceptance == nil {
			return errors.New("acceptance not found")
		}
		var acceptance AcceptanceRecord
		if err := json.Unmarshal(encodedAcceptance, &acceptance); err != nil {
			return err
		}
		if acceptance.CancelledAt != 0 {
			return ErrAcceptanceCancelled
		}
		cancelKey := acceptanceCancellationKey(acceptance.Signed.ID, acceptance.Signed.Acceptance.TakerPublicKey)
		if tx.Bucket(acceptanceCancellationsBucket).Get(cancelKey) != nil {
			return ErrAcceptanceCancelled
		}
		// The relay enforces the acceptance deadline using its own clock. A
		// maker-controlled signed timestamp cannot extend a taker's consent.
		if err := match.Verify(orderRecord.Signed, acceptance.Signed, now); err != nil {
			return err
		}
		orderRecord.Status = StatusMatched
		orderRecord.MatchedAt = match.Match.CreatedAt
		orderRecord.Acceptance = &acceptance.Signed
		orderRecord.Match = &match
		encodedOrder, err := json.Marshal(orderRecord)
		if err != nil {
			return err
		}
		if err := tx.Bucket(ordersBucket).Put([]byte(orderID), encodedOrder); err != nil {
			return err
		}
		item := MailboxItem{Kind: MailboxMatch, ReceivedAt: now.Unix(), Match: &match}
		if _, err := appendMailbox(tx, acceptance.Signed.Acceptance.TakerPublicKey, item); err != nil {
			return err
		}
		result = orderRecord
		changed = true
		return nil
	})
	return result.at(now), changed, err
}

func (s *Store) PublishMessage(message trade.SignedMessage, now time.Time) (trade.SignedMessage, bool, error) {
	if err := message.Verify(now); err != nil {
		return trade.SignedMessage{}, false, err
	}
	created := false
	err := s.db.Update(func(tx *bbolt.Tx) error {
		orderRecord, err := orderRecordIn(tx, message.Message.OrderID, now)
		if err != nil {
			return err
		}
		if orderRecord.Status != StatusMatched || orderRecord.Acceptance == nil || orderRecord.Match == nil {
			return errors.New("order does not have a matched trade")
		}
		if message.Message.Network != orderRecord.Signed.Order.Network || message.Message.TradeID != orderRecord.Match.Match.TradeID {
			return errors.New("message does not belong to the matched trade")
		}
		maker := orderRecord.Signed.Order.MakerPublicKey
		taker := orderRecord.Acceptance.Acceptance.TakerPublicKey
		validDirection := (message.Message.SenderPublicKey == maker && message.Message.RecipientPublicKey == taker) ||
			(message.Message.SenderPublicKey == taker && message.Message.RecipientPublicKey == maker)
		if !validDirection {
			return errors.New("message participants do not match the trade")
		}
		if message.Message.CreatedAt < orderRecord.MatchedAt {
			return errors.New("message predates the matched trade")
		}

		messages := tx.Bucket(messagesBucket)
		if existing := messages.Get([]byte(message.ID)); existing != nil {
			return nil
		}
		sequenceKey := encodeSequenceKey(message.Message.TradeID, message.Message.SenderPublicKey, message.Message.Sequence)
		sequences := tx.Bucket(messageSequenceBucket)
		if existingID := sequences.Get(sequenceKey); existingID != nil {
			if bytes.Equal(existingID, []byte(message.ID)) {
				return nil
			}
			return ErrMessageSequence
		}
		headKey := []byte(message.Message.TradeID + "\x00" + message.Message.SenderPublicKey)
		heads := tx.Bucket(messageHeadBucket)
		var current uint64
		if encodedHead := heads.Get(headKey); len(encodedHead) == 8 {
			current = binary.BigEndian.Uint64(encodedHead)
		}
		if current == ^uint64(0) || message.Message.Sequence != current+1 {
			return ErrMessageSequence
		}
		encodedMessage, err := json.Marshal(message)
		if err != nil {
			return err
		}
		if err := messages.Put([]byte(message.ID), encodedMessage); err != nil {
			return err
		}
		if err := sequences.Put(sequenceKey, []byte(message.ID)); err != nil {
			return err
		}
		var encodedHead [8]byte
		binary.BigEndian.PutUint64(encodedHead[:], message.Message.Sequence)
		if err := heads.Put(headKey, encodedHead[:]); err != nil {
			return err
		}
		item := MailboxItem{Kind: MailboxMessage, ReceivedAt: now.Unix(), Message: &message}
		if _, err := appendMailbox(tx, message.Message.RecipientPublicKey, item); err != nil {
			return err
		}
		created = true
		return nil
	})
	return message, created, err
}

func (s *Store) PollMailbox(poll trade.SignedPoll, now time.Time) (MailboxPage, error) {
	if err := poll.Verify(now); err != nil {
		return MailboxPage{}, err
	}
	page := MailboxPage{Items: []MailboxItem{}, After: poll.Poll.After, Next: poll.Poll.After}
	err := s.db.View(func(tx *bbolt.Tx) error {
		mailbox := tx.Bucket(mailboxesBucket).Bucket([]byte(poll.Poll.RecipientPublicKey))
		if mailbox == nil {
			return nil
		}
		var start [8]byte
		if poll.Poll.After == ^uint64(0) {
			return nil
		}
		binary.BigEndian.PutUint64(start[:], poll.Poll.After+1)
		cursor := mailbox.Cursor()
		key, value := cursor.Seek(start[:])
		for key != nil && len(page.Items) < int(poll.Poll.Limit) {
			var item MailboxItem
			if err := json.Unmarshal(value, &item); err != nil {
				return err
			}
			page.Items = append(page.Items, item)
			page.Next = item.Cursor
			key, value = cursor.Next()
		}
		page.HasMore = key != nil
		return nil
	})
	return page, err
}

func orderRecordIn(tx *bbolt.Tx, orderID string, now time.Time) (Record, error) {
	value := tx.Bucket(ordersBucket).Get([]byte(orderID))
	if value == nil {
		return Record{}, ErrNotFound
	}
	var record Record
	if err := json.Unmarshal(value, &record); err != nil {
		return Record{}, err
	}
	return record.at(now), nil
}

func appendMailbox(tx *bbolt.Tx, recipient string, item MailboxItem) (uint64, error) {
	mailbox, err := tx.Bucket(mailboxesBucket).CreateBucketIfNotExists([]byte(recipient))
	if err != nil {
		return 0, err
	}
	cursor, err := mailbox.NextSequence()
	if err != nil {
		return 0, err
	}
	item.Cursor = cursor
	encoded, err := json.Marshal(item)
	if err != nil {
		return 0, err
	}
	var key [8]byte
	binary.BigEndian.PutUint64(key[:], cursor)
	if err := mailbox.Put(key[:], encoded); err != nil {
		return 0, err
	}
	return cursor, nil
}

func encodeSequenceKey(tradeID, sender string, sequence uint64) []byte {
	prefix := []byte(tradeID + "\x00" + sender + "\x00")
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], sequence)
	return append(prefix, encoded[:]...)
}

func (item MailboxItem) validateShape() error {
	count := 0
	if item.Acceptance != nil {
		count++
	}
	if item.Match != nil {
		count++
	}
	if item.Message != nil {
		count++
	}
	if count != 1 {
		return fmt.Errorf("mailbox item must contain exactly one signed record")
	}
	return nil
}

func acceptanceCancellationKey(acceptanceID, takerPublicKey string) []byte {
	return []byte(acceptanceID + "\x00" + takerPublicKey)
}
