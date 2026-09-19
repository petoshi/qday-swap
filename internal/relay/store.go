package relay

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/petoshi/qday-swap/internal/order"
	"github.com/petoshi/qday-swap/internal/trade"
	"go.etcd.io/bbolt"
)

const MaxOpenOrdersPerMaker = 100

var (
	ordersBucket          = []byte("orders")
	acceptancesBucket     = []byte("acceptances")
	messagesBucket        = []byte("messages")
	messageSequenceBucket = []byte("message-sequences")
	messageHeadBucket     = []byte("message-heads")
	mailboxesBucket       = []byte("mailboxes")
	ErrNotFound           = errors.New("order not found")
	ErrNotOpen            = errors.New("order is not open")
	ErrMakerLimit         = errors.New("maker has too many open orders")
	ErrAcceptanceLimit    = errors.New("order has too many pending acceptances")
	ErrAlreadyAccepted    = errors.New("taker already accepted this order")
	ErrMessageSequence    = errors.New("message sequence is not the next expected value")
)

type Status string

const (
	StatusOpen      Status = "open"
	StatusCancelled Status = "cancelled"
	StatusExpired   Status = "expired"
	StatusMatched   Status = "matched"
)

type Record struct {
	Signed       order.Signed              `json:"signed"`
	Status       Status                    `json:"status"`
	ReceivedAt   int64                     `json:"receivedAt"`
	CancelledAt  int64                     `json:"cancelledAt,omitempty"`
	Cancellation *order.SignedCancellation `json:"cancellation,omitempty"`
	MatchedAt    int64                     `json:"matchedAt,omitempty"`
	Acceptance   *trade.SignedAcceptance   `json:"acceptance,omitempty"`
	Match        *trade.SignedMatch        `json:"match,omitempty"`
}

func (record Record) EffectiveStatus(now time.Time) Status {
	if record.Status == StatusOpen && record.Signed.Order.ExpiresAt <= now.Unix() {
		return StatusExpired
	}
	return record.Status
}

func (record Record) at(now time.Time) Record {
	record.Status = record.EffectiveStatus(now)
	return record
}

type Query struct {
	Status    Status
	Market    string
	GiveAsset string
	Page      int
	Limit     int
}

type ResultPage struct {
	Items      []Record `json:"items"`
	Page       int      `json:"page"`
	PageSize   int      `json:"pageSize"`
	Total      int      `json:"total"`
	TotalPages int      `json:"totalPages"`
}

type Stats struct {
	Open       int         `json:"open"`
	Cancelled  int         `json:"cancelled"`
	Expired    int         `json:"expired"`
	Matched    int         `json:"matched"`
	Total      int         `json:"total"`
	Created24h int         `json:"created24h"`
	LastTrade  *TradePrice `json:"lastTrade,omitempty"`
}

type TradePrice struct {
	QDAYAtomic     string `json:"qdayAtomic"`
	BTCAtomic      string `json:"btcAtomic"`
	QDAYUnitAtomic string `json:"qdayUnitAtomic"`
	MatchedAt      int64  `json:"matchedAt"`
}

type Store struct{ db *bbolt.DB }

func OpenStore(path string) (*Store, error) {
	db, err := bbolt.Open(path, 0600, &bbolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open relay store: %w", err)
	}
	if err := db.Update(func(tx *bbolt.Tx) error {
		for _, name := range [][]byte{ordersBucket, acceptancesBucket, messagesBucket, messageSequenceBucket, messageHeadBucket, mailboxesBucket} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initialize relay store: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Publish(signed order.Signed, now time.Time) (Record, bool, error) {
	if err := signed.Verify(now); err != nil {
		return Record{}, false, err
	}
	var record Record
	created := false
	err := s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(ordersBucket)
		key := []byte(signed.ID)
		if existing := bucket.Get(key); existing != nil {
			return json.Unmarshal(existing, &record)
		}
		openForMaker := 0
		if err := bucket.ForEach(func(_, value []byte) error {
			var candidate Record
			if err := json.Unmarshal(value, &candidate); err != nil {
				return err
			}
			if candidate.Signed.Order.MakerPublicKey == signed.Order.MakerPublicKey && candidate.EffectiveStatus(now) == StatusOpen {
				openForMaker++
			}
			return nil
		}); err != nil {
			return err
		}
		if openForMaker >= MaxOpenOrdersPerMaker {
			return ErrMakerLimit
		}
		record = Record{Signed: signed, Status: StatusOpen, ReceivedAt: now.Unix()}
		encoded, err := json.Marshal(record)
		if err != nil {
			return err
		}
		created = true
		return bucket.Put(key, encoded)
	})
	return record.at(now), created, err
}

func (s *Store) Cancel(cancellation order.SignedCancellation, now time.Time) (Record, bool, error) {
	if err := cancellation.Verify(now); err != nil {
		return Record{}, false, err
	}
	var record Record
	changed := false
	err := s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(ordersBucket)
		key := []byte(cancellation.Cancellation.OrderID)
		value := bucket.Get(key)
		if value == nil {
			return ErrNotFound
		}
		if err := json.Unmarshal(value, &record); err != nil {
			return err
		}
		if record.Signed.Order.MakerPublicKey != cancellation.Cancellation.MakerPublicKey {
			return errors.New("cancellation maker does not own the order")
		}
		if record.Status == StatusCancelled {
			return nil
		}
		if record.EffectiveStatus(now) != StatusOpen {
			return ErrNotOpen
		}
		record.Status = StatusCancelled
		record.CancelledAt = cancellation.Cancellation.CreatedAt
		record.Cancellation = &cancellation
		encoded, err := json.Marshal(record)
		if err != nil {
			return err
		}
		changed = true
		return bucket.Put(key, encoded)
	})
	return record.at(now), changed, err
}

func (s *Store) Order(id string, now time.Time) (Record, error) {
	var record Record
	err := s.db.View(func(tx *bbolt.Tx) error {
		value := tx.Bucket(ordersBucket).Get([]byte(id))
		if value == nil {
			return ErrNotFound
		}
		return json.Unmarshal(value, &record)
	})
	return record.at(now), err
}

func (s *Store) List(query Query, now time.Time) (ResultPage, error) {
	if query.Page < 1 {
		query.Page = 1
	}
	if query.Limit < 1 {
		query.Limit = 20
	} else if query.Limit > 100 {
		query.Limit = 100
	}
	if query.Status == "" {
		query.Status = StatusOpen
	}
	var records []Record
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(ordersBucket).ForEach(func(_, value []byte) error {
			var record Record
			if err := json.Unmarshal(value, &record); err != nil {
				return err
			}
			record = record.at(now)
			if query.Status != "all" && record.Status != query.Status {
				return nil
			}
			if query.Market != "" && record.Signed.Order.Market != query.Market {
				return nil
			}
			if query.GiveAsset != "" && record.Signed.Order.Give.Asset != query.GiveAsset {
				return nil
			}
			records = append(records, record)
			return nil
		})
	})
	if err != nil {
		return ResultPage{}, err
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].Signed.Order.CreatedAt == records[j].Signed.Order.CreatedAt {
			return bytes.Compare([]byte(records[i].Signed.ID), []byte(records[j].Signed.ID)) > 0
		}
		return records[i].Signed.Order.CreatedAt > records[j].Signed.Order.CreatedAt
	})
	total := len(records)
	totalPages := 0
	if total > 0 {
		totalPages = (total + query.Limit - 1) / query.Limit
	}
	start := (query.Page - 1) * query.Limit
	if start > total {
		start = total
	}
	end := start + query.Limit
	if end > total {
		end = total
	}
	items := records[start:end]
	if items == nil {
		items = []Record{}
	}
	return ResultPage{Items: items, Page: query.Page, PageSize: query.Limit, Total: total, TotalPages: totalPages}, nil
}

func (s *Store) Stats(now time.Time) (Stats, error) {
	var stats Stats
	cutoff := now.Add(-24 * time.Hour).Unix()
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(ordersBucket).ForEach(func(_, value []byte) error {
			var record Record
			if err := json.Unmarshal(value, &record); err != nil {
				return err
			}
			stats.Total++
			if record.Signed.Order.CreatedAt >= cutoff {
				stats.Created24h++
			}
			switch record.EffectiveStatus(now) {
			case StatusOpen:
				stats.Open++
			case StatusCancelled:
				stats.Cancelled++
			case StatusExpired:
				stats.Expired++
			case StatusMatched:
				stats.Matched++
				if stats.LastTrade == nil || record.MatchedAt > stats.LastTrade.MatchedAt {
					terms := record.Signed.Order
					tradePrice := &TradePrice{QDAYUnitAtomic: terms.QDAYUnitAtomic, MatchedAt: record.MatchedAt}
					if terms.Give.Asset == "QDAY" {
						tradePrice.QDAYAtomic, tradePrice.BTCAtomic = terms.Give.Atomic, terms.Receive.Atomic
					} else {
						tradePrice.QDAYAtomic, tradePrice.BTCAtomic = terms.Receive.Atomic, terms.Give.Atomic
					}
					stats.LastTrade = tradePrice
				}
			}
			return nil
		})
	})
	return stats, err
}
