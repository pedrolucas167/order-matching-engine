package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json/v2"
	"errors"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
)

var (
	ErrInvalidDurableState = errors.New("invalid or unsupported durable engine state")
	ErrDurableStorage      = errors.New("durable storage failed; close and reopen before retrying")
)

const maxDurableStateBytes = 64 << 20

var durableBucket = []byte("order-engine-v1")
var durableStateKey = []byte("committed-state")

func openDurableStore(path string) (*bolt.DB, *durableState, error) {
	if path == "" {
		return nil, nil, ErrInvalidDurableState
	}
	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: 100 * time.Millisecond})
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrDurableStorage, err)
	}
	var state *durableState
	err = db.View(func(tx *bolt.Tx) error {
		var integrityError error
		for checkError := range tx.Check() {
			if checkError != nil {
				integrityError = ErrInvalidDurableState
			}
		}
		if integrityError != nil {
			return integrityError
		}
		bucket := tx.Bucket(durableBucket)
		if bucket == nil {
			key, _ := tx.Cursor().First()
			if key != nil {
				return ErrInvalidDurableState
			}
			return nil
		}
		encoded := bucket.Get(durableStateKey)
		if len(encoded) < 36 || len(encoded) > maxDurableStateBytes+36 || !bytes.Equal(encoded[:4], []byte("OME1")) {
			return ErrInvalidDurableState
		}
		payload := encoded[36:]
		digest := sha256.Sum256(payload)
		if !bytes.Equal(encoded[4:36], digest[:]) {
			return ErrInvalidDurableState
		}
		state = new(durableState)
		if json.Unmarshal(payload, state, json.RejectUnknownMembers(true)) != nil {
			return ErrInvalidDurableState
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, nil, err
	}
	return db, state, nil
}

func writeDurableState(db *bolt.DB, payload []byte) error {
	if len(payload) > maxDurableStateBytes {
		return ErrInvalidDurableState
	}
	digest := sha256.Sum256(payload)
	encoded := append([]byte("OME1"), digest[:]...)
	encoded = append(encoded, payload...)
	return db.Update(func(tx *bolt.Tx) error {
		bucket, err := tx.CreateBucketIfNotExists(durableBucket)
		if err != nil {
			return err
		}
		return bucket.Put(durableStateKey, encoded)
	})
}
