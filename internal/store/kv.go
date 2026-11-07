package store

import (
	"errors"
	"fmt"

	bolt "go.etcd.io/bbolt"
)

// KVGet returns a copy of the value stored under key, and whether it exists.
func (s *Store) KVGet(key string) ([]byte, bool, error) {
	var (
		out []byte
		ok  bool
	)
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bucketKV).Get([]byte(key))
		if v != nil {
			// bbolt values are only valid inside the transaction.
			out, ok = append([]byte{}, v...), true
		}
		return nil
	})
	if err != nil {
		return nil, false, fmt.Errorf("kv get %q: %v", key, err)
	}
	return out, ok, nil
}

// KVPut stores value under key, replacing any previous value.
func (s *Store) KVPut(key string, value []byte) error {
	if key == "" {
		return errors.New("kv put: empty key")
	}
	err := s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketKV).Put([]byte(key), value)
	})
	if err != nil {
		return fmt.Errorf("kv put %q: %v", key, err)
	}
	return nil
}

// KVDelete removes key. Removing an absent key is not an error.
func (s *Store) KVDelete(key string) error {
	err := s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketKV).Delete([]byte(key))
	})
	if err != nil {
		return fmt.Errorf("kv delete %q: %v", key, err)
	}
	return nil
}
