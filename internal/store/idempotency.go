// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/opid"
)

// IdempotencyWindow is how long a stored key answers for its operation
// (FR-7). Past it the key is absent and may be used again.
const IdempotencyWindow = 24 * time.Hour

// IdempotencyKey identifies a mutating request for replay detection. A key is
// scoped to the token that sent it. An empty Key means the request has none.
type IdempotencyKey struct {
	TokenID     string
	Key         string
	RequestHash string // Hash of the canonical request; tells a replay from a reuse
}

// IdempotencyEntry is a stored, unexpired key.
type IdempotencyEntry struct {
	OperationID string
	RequestHash string
	Expires     time.Time
}

// CreateResult reports what CreateWithIdempotency did.
type CreateResult struct {
	// Existing is true when the key was already stored: nothing was created,
	// and ID and RequestHash are the stored operation's. The caller compares
	// RequestHash with its own to tell a replay from a reused key.
	Existing    bool
	ID          string
	RequestHash string
}

// idemValue is the JSON stored in the idem bucket.
type idemValue struct {
	OperationID string    `json:"op"`
	RequestHash string    `json:"hash"`
	Expires     time.Time `json:"expires"`
}

// idemBucketKey is sha256 over the length-prefixed token ID and the key, so no
// two (token, key) pairs share a bucket key.
func idemBucketKey(tokenID, key string) []byte {
	h := sha256.New()
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(tokenID)))
	h.Write(n[:])
	h.Write([]byte(tokenID))
	h.Write([]byte(key))
	return h.Sum(nil)
}

// lookupIdem returns the unexpired entry for k, if any.
func (s *Store) lookupIdem(tx *bolt.Tx, k []byte, now time.Time) (idemValue, bool, error) {
	v := tx.Bucket(bucketIdem).Get(k)
	if v == nil {
		return idemValue{}, false, nil
	}
	var e idemValue
	if err := json.Unmarshal(v, &e); err != nil {
		return idemValue{}, false, fmt.Errorf("decode idempotency entry: %v", err)
	}
	if !now.Before(e.Expires) {
		return idemValue{}, false, nil
	}
	return e, true, nil
}

// LookupIdempotency returns the stored, unexpired entry for a token's key.
func (s *Store) LookupIdempotency(tokenID, key string) (IdempotencyEntry, bool, error) {
	if key == "" {
		return IdempotencyEntry{}, false, nil
	}
	var (
		e  idemValue
		ok bool
	)
	err := s.db.View(func(tx *bolt.Tx) error {
		var err error
		e, ok, err = s.lookupIdem(tx, idemBucketKey(tokenID, key), s.nowUTC())
		return err
	})
	if err != nil || !ok {
		return IdempotencyEntry{}, false, err
	}
	return IdempotencyEntry{OperationID: e.OperationID, RequestHash: e.RequestHash, Expires: e.Expires}, true, nil
}

// CreateWithIdempotency is the only way to create an operation record. In one
// write transaction it looks up the key and, if no unexpired entry exists,
// inserts the record as Queued with a new ID and Created time, and stores the
// key (Data Flow step 10). If the key is already stored it inserts nothing
// and returns the stored operation ID and request hash. bbolt runs one write
// transaction at a time, so concurrent calls with one key create one record.
func (s *Store) CreateWithIdempotency(rec OpRecord, idem IdempotencyKey) (CreateResult, error) {
	if rec.ID != "" {
		return CreateResult{}, errors.New("create operation: ID is assigned by the store")
	}
	if rec.Action == "" || rec.Provider == "" {
		return CreateResult{}, errors.New("create operation: action and provider are required")
	}
	if idem.Key != "" && idem.TokenID == "" {
		return CreateResult{}, errors.New("create operation: idempotency key without a token ID")
	}

	var res CreateResult
	err := s.db.Update(func(tx *bolt.Tx) error {
		now := s.nowUTC()
		var ik []byte
		if idem.Key != "" {
			ik = idemBucketKey(idem.TokenID, idem.Key)
			e, ok, err := s.lookupIdem(tx, ik, now)
			if err != nil {
				return err
			}
			if ok {
				res = CreateResult{Existing: true, ID: e.OperationID, RequestHash: e.RequestHash}
				return nil
			}
		}

		// The ID is generated inside the write transaction, so key order is
		// commit order.
		rec.ID = opid.New()
		rec.Status = apitypes.StatusQueued
		rec.Created = now
		rec.Started, rec.Ended = nil, nil
		enc, err := encodeRecord(rec)
		if err != nil {
			return err
		}
		if err := tx.Bucket(bucketOps).Put([]byte(rec.ID), enc); err != nil {
			return fmt.Errorf("store operation %s: %v", rec.ID, err)
		}
		if ik != nil {
			v, err := json.Marshal(idemValue{OperationID: rec.ID, RequestHash: idem.RequestHash, Expires: now.Add(IdempotencyWindow)})
			if err != nil {
				return fmt.Errorf("encode idempotency entry: %v", err)
			}
			if err := tx.Bucket(bucketIdem).Put(ik, v); err != nil {
				return fmt.Errorf("store idempotency entry: %v", err)
			}
		}
		res = CreateResult{ID: rec.ID, RequestHash: idem.RequestHash}
		return nil
	})
	if err != nil {
		return CreateResult{}, err
	}
	return res, nil
}

// PurgeExpiredIdempotency deletes every key past its expiry, in one write
// transaction, and returns how many it deleted. Lookups already ignore an
// expired key, so this only reclaims space: whether it has run never
// changes an answer (FR-7).
func (s *Store) PurgeExpiredIdempotency() (int, error) {
	var expired [][]byte
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketIdem)
		now := s.nowUTC()
		err := b.ForEach(func(k, v []byte) error {
			var e idemValue
			if err := json.Unmarshal(v, &e); err != nil {
				return fmt.Errorf("decode idempotency entry: %v", err)
			}
			if !now.Before(e.Expires) {
				// k is only valid for the transaction.
				expired = append(expired, slices.Clone(k))
			}
			return nil
		})
		if err != nil {
			return err
		}
		for _, k := range expired {
			if err := b.Delete(k); err != nil {
				return fmt.Errorf("delete idempotency entry: %v", err)
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return len(expired), nil
}
