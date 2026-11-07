package store

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/problem"
)

// List limits (architecture: List endpoints, default 100, maximum 1000).
const (
	DefaultListLimit = 100
	MaxListLimit     = 1000
)

// Exec values of OpRecord.Exec.
const (
	ExecDetached  = "detached"
	ExecInProcess = "inprocess"
)

// ErrStatusConflict means CompareAndSetStatus found a status other than from.
var ErrStatusConflict = errors.New("operation status changed")

// ErrInvalidCursor means a List cursor was not one List returned.
var ErrInvalidCursor = errors.New("invalid list cursor")

// LanePolicy mirrors provider.LanePolicy; store must not import provider.
type LanePolicy string

// Lane policies.
const (
	LaneQueue  LanePolicy = "queue"
	LaneReject LanePolicy = "reject"
)

// Lane mirrors provider.Lane: the lane resolved at admission, which recovery
// reuses without re-resolving it. An empty Key is no lane.
type Lane struct {
	Key    string     `json:"-"`
	Policy LanePolicy `json:"-"`
	Limit  int        `json:"-"`
}

// OpRecord is the stored form of an operation. It is not a wire type
// (go/json-tags-domain-vs-wire): every field is json:"-", the on-disk JSON goes
// through diskRecord, and the API sees only ToAPI's explicit mapping.
type OpRecord struct {
	// Fields that apitypes.Operation also exposes.
	ID            string                   `json:"-"`
	Action        string                   `json:"-"`
	Provider      string                   `json:"-"`
	Target        string                   `json:"-"`
	TokenName     string                   `json:"-"`
	Status        apitypes.OperationStatus `json:"-"`
	Created       time.Time                `json:"-"`
	Started       *time.Time               `json:"-"`
	Ended         *time.Time               `json:"-"`
	Result        json.RawMessage          `json:"-"`
	Error         *problem.Error           `json:"-"`
	OutputEvicted bool                     `json:"-"`

	// Internal fields; they never reach the API.
	Input        json.RawMessage `json:"-"` // Op.Sensitive and catalog-sensitive paths removed
	HasSensitive bool            `json:"-"` // A sensitive value was supplied; it exists only in memory
	Exec         string          `json:"-"` // ExecDetached or ExecInProcess
	StartScope   string          `json:"-"` // Scope that authorized creation, e.g. "scripts:run:deploy-api"
	Lane         Lane            `json:"-"` // Key, policy and limit resolved at admission
}

// ToAPI maps the record to its wire form. Internal fields are never copied.
func (r OpRecord) ToAPI(host string) apitypes.Operation {
	return apitypes.Operation{
		ID:            r.ID,
		Host:          host,
		Action:        r.Action,
		Provider:      r.Provider,
		Target:        r.Target,
		TokenName:     r.TokenName,
		Status:        r.Status,
		Created:       r.Created,
		Started:       copyTime(r.Started),
		Ended:         copyTime(r.Ended),
		Result:        append(json.RawMessage(nil), r.Result...),
		Error:         r.Error,
		OutputEvicted: r.OutputEvicted,
	}
}

func copyTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}

// diskRecord is the JSON stored in the ops bucket.
type diskRecord struct {
	ID            string                   `json:"id"`
	Action        string                   `json:"action"`
	Provider      string                   `json:"provider"`
	Target        string                   `json:"target"`
	TokenName     string                   `json:"tokenName"`
	Status        apitypes.OperationStatus `json:"status"`
	Created       time.Time                `json:"created"`
	Started       *time.Time               `json:"started,omitempty"`
	Ended         *time.Time               `json:"ended,omitempty"`
	Result        json.RawMessage          `json:"result,omitempty"`
	Error         *problem.Error           `json:"error,omitempty"`
	OutputEvicted bool                     `json:"outputEvicted,omitempty"`
	Input         json.RawMessage          `json:"input,omitempty"`
	HasSensitive  bool                     `json:"hasSensitive,omitempty"`
	Exec          string                   `json:"exec"`
	StartScope    string                   `json:"startScope"`
	Lane          diskLane                 `json:"lane"`
}

type diskLane struct {
	Key    string     `json:"key,omitempty"`
	Policy LanePolicy `json:"policy,omitempty"`
	Limit  int        `json:"limit,omitempty"`
}

func encodeRecord(r OpRecord) ([]byte, error) {
	b, err := json.Marshal(diskRecord{
		ID: r.ID, Action: r.Action, Provider: r.Provider, Target: r.Target,
		TokenName: r.TokenName, Status: r.Status, Created: r.Created,
		Started: r.Started, Ended: r.Ended, Result: r.Result, Error: r.Error,
		OutputEvicted: r.OutputEvicted, Input: r.Input, HasSensitive: r.HasSensitive,
		Exec: r.Exec, StartScope: r.StartScope,
		Lane: diskLane{Key: r.Lane.Key, Policy: r.Lane.Policy, Limit: r.Lane.Limit},
	})
	if err != nil {
		return nil, fmt.Errorf("encode operation %s: %v", r.ID, err)
	}
	return b, nil
}

// decodeRecord copies everything out of b, which bbolt only keeps valid for
// the transaction.
func decodeRecord(b []byte) (OpRecord, error) {
	var d diskRecord
	if err := json.Unmarshal(b, &d); err != nil {
		return OpRecord{}, fmt.Errorf("decode operation record: %v", err)
	}
	return OpRecord{
		ID: d.ID, Action: d.Action, Provider: d.Provider, Target: d.Target,
		TokenName: d.TokenName, Status: d.Status, Created: d.Created,
		Started: d.Started, Ended: d.Ended, Result: d.Result, Error: d.Error,
		OutputEvicted: d.OutputEvicted, Input: d.Input, HasSensitive: d.HasSensitive,
		Exec: d.Exec, StartScope: d.StartScope,
		Lane: Lane{Key: d.Lane.Key, Policy: d.Lane.Policy, Limit: d.Lane.Limit},
	}, nil
}

// Get returns the record with the given ID, or ErrNotFound.
func (s *Store) Get(id string) (OpRecord, error) {
	var rec OpRecord
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bucketOps).Get([]byte(id))
		if v == nil {
			return fmt.Errorf("operation %q: %w", id, ErrNotFound)
		}
		var err error
		rec, err = decodeRecord(v)
		return err
	})
	return rec, err
}

// CompareAndSetStatus sets the status of id to to, in one write transaction,
// only if its current status is from. Otherwise it changes nothing and
// returns ErrStatusConflict, so of two racing transitions out of Queued
// exactly one wins. Moving to Running sets Started; moving to a terminal
// status sets Ended. It returns the record as committed or, with
// ErrStatusConflict, as it currently is.
func (s *Store) CompareAndSetStatus(id string, from, to apitypes.OperationStatus) (OpRecord, error) {
	if !validStatus(from) || !validStatus(to) {
		return OpRecord{}, fmt.Errorf("compare-and-set %q: invalid status %q to %q", id, from, to)
	}
	if from.Terminal() {
		return OpRecord{}, fmt.Errorf("compare-and-set %q: %s is terminal", id, from)
	}
	var rec OpRecord
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketOps)
		v := b.Get([]byte(id))
		if v == nil {
			return fmt.Errorf("operation %q: %w", id, ErrNotFound)
		}
		cur, err := decodeRecord(v)
		if err != nil {
			return err
		}
		if cur.Status != from {
			rec = cur
			return fmt.Errorf("operation %q is %s, not %s: %w", id, cur.Status, from, ErrStatusConflict)
		}
		now := s.nowUTC()
		cur.Status = to
		if to == apitypes.StatusRunning && cur.Started == nil {
			cur.Started = &now
		}
		if to.Terminal() && cur.Ended == nil {
			cur.Ended = &now
		}
		enc, err := encodeRecord(cur)
		if err != nil {
			return err
		}
		if err := b.Put([]byte(id), enc); err != nil {
			return fmt.Errorf("store operation %q: %v", id, err)
		}
		rec = cur
		return nil
	})
	return rec, err
}

func validStatus(s apitypes.OperationStatus) bool {
	switch s {
	case apitypes.StatusQueued, apitypes.StatusRunning, apitypes.StatusSucceeded,
		apitypes.StatusFailed, apitypes.StatusCanceled:
		return true
	}
	return false
}

// ListNonTerminal returns every Queued or Running record in ID order, which
// is creation order, for the startup rebuild of lanes and queues.
func (s *Store) ListNonTerminal() ([]OpRecord, error) {
	var out []OpRecord
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketOps).ForEach(func(_, v []byte) error {
			rec, err := decodeRecord(v)
			if err != nil {
				return err
			}
			if !rec.Status.Terminal() {
				out = append(out, rec)
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListQuery selects a page of records. Zero values match everything.
type ListQuery struct {
	Status        apitypes.OperationStatus // Exact status
	Provider      string                   // Exact provider name
	CreatedAfter  time.Time                // Created strictly after
	CreatedBefore time.Time                // Created strictly before
	Limit         int                      // <= 0 means DefaultListLimit; capped at MaxListLimit
	Cursor        string                   // A previous page's NextCursor
}

func (q ListQuery) match(r OpRecord) bool {
	if q.Status != "" && r.Status != q.Status {
		return false
	}
	if q.Provider != "" && r.Provider != q.Provider {
		return false
	}
	if !q.CreatedAfter.IsZero() && !r.Created.After(q.CreatedAfter) {
		return false
	}
	if !q.CreatedBefore.IsZero() && !r.Created.Before(q.CreatedBefore) {
		return false
	}
	return true
}

// Page is one page of List.
type Page struct {
	Records    []OpRecord
	NextCursor string // Empty on the last page
}

// List returns matching records newest-first. The cursor is the position
// after the last record returned, so records created after a page was
// fetched never shift later pages: their IDs sort above every cursor.
func (s *Store) List(q ListQuery) (Page, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultListLimit
	}
	limit = min(limit, MaxListLimit)

	var after []byte
	if q.Cursor != "" {
		id, err := decodeCursor(q.Cursor)
		if err != nil {
			return Page{}, err
		}
		after = id
	}

	var page Page
	err := s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(bucketOps).Cursor()
		var k, v []byte
		if after == nil {
			k, v = c.Last()
		} else if k, _ = c.Seek(after); k == nil {
			// Seek lands on the first key >= the cursor; nil means none.
			k, v = c.Last()
		} else {
			k, v = c.Prev()
		}
		for ; k != nil; k, v = c.Prev() {
			rec, err := decodeRecord(v)
			if err != nil {
				return err
			}
			if !q.match(rec) {
				continue
			}
			if len(page.Records) == limit {
				// One more match exists, so there is a next page.
				page.NextCursor = encodeCursor(page.Records[limit-1].ID)
				return nil
			}
			page.Records = append(page.Records, rec)
		}
		return nil
	})
	if err != nil {
		return Page{}, err
	}
	return page, nil
}

func encodeCursor(id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(id))
}

func decodeCursor(c string) ([]byte, error) {
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil || len(b) != len("00000000-0000-0000-0000-000000000000") {
		return nil, ErrInvalidCursor
	}
	return b, nil
}
