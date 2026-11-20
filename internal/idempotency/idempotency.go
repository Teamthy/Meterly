// Package idempotency stores request keys so a retried request returns the
// original result, and a reused key with a different body is rejected.
package idempotency

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

var (
	ErrConflict   = errors.New("idempotency: key reused with a different request")
	ErrInProgress = errors.New("idempotency: request with this key is still in progress")
)

type Record struct {
	Scope       string // customer or API client
	Operation   string
	Key         string
	RequestHash string
	ResultRef   string // empty while in progress
	CreatedAt   time.Time
}

func HashRequest(body []byte) string {
	h := sha256.Sum256(body)
	return hex.EncodeToString(h[:])
}

type Store struct {
	mu  sync.Mutex
	m   map[string]*Record
	ttl time.Duration
	now func() time.Time
}

func NewStore(ttl time.Duration) *Store {
	return &Store{m: map[string]*Record{}, ttl: ttl, now: time.Now}
}

func k(scope, op, key string) string { return scope + "\x00" + op + "\x00" + key }

// Begin reserves a key. If the key exists with the same request hash and a
// result, it returns (resultRef, false, nil): replay the original result.
// If it is new, it returns ("", true, nil): the caller must do the work and
// call Complete (or Abort on failure).
func (s *Store) Begin(scope, op, key, reqHash string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := k(scope, op, key)
	if r, ok := s.m[id]; ok && s.now().Sub(r.CreatedAt) < s.ttl {
		if r.RequestHash != reqHash {
			return "", false, ErrConflict
		}
		if r.ResultRef == "" {
			return "", false, ErrInProgress
		}
		return r.ResultRef, false, nil
	}
	s.m[id] = &Record{Scope: scope, Operation: op, Key: key, RequestHash: reqHash, CreatedAt: s.now()}
	return "", true, nil
}

func (s *Store) Complete(scope, op, key, resultRef string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.m[k(scope, op, key)]; ok {
		r.ResultRef = resultRef
	}
}

// Abort releases a key whose work failed before producing a result, so the
// client may retry.
func (s *Store) Abort(scope, op, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, k(scope, op, key))
}
