package notifications

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"

	"errors"
	"github.com/google/uuid"
	"sync"
)

var ErrDuplicateMismatch = errors.New("notification idempotency conflict")

// MockSender is a process-local test double, not a production dedupe store.
// It stores only keyed digests and a count, never message bodies or destinations.
type MockSender struct {
	mu   sync.Mutex
	key  [32]byte
	sent map[[32]byte][32]byte
}

func NewMockSender() (*MockSender, error) {
	s := &MockSender{sent: make(map[[32]byte][32]byte)}
	if _, err := rand.Read(s.key[:]); err != nil {
		return nil, err
	}
	return s, nil
}
func (s *MockSender) digest(parts ...string) [32]byte {
	h := hmac.New(sha256.New, s.key[:])
	for _, p := range parts {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(p)))
		h.Write(length[:])
		h.Write([]byte(p))
	}
	var result [32]byte
	copy(result[:], h.Sum(nil))
	return result
}
func (s *MockSender) Send(ctx context.Context, m Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.EventID == uuid.Nil || m.TransactionID == uuid.Nil || m.Destination == "" || m.Body == "" || (m.Channel != SMS && m.Channel != Email && m.Channel != WhatsApp) {
		return errors.New("invalid notification")
	}
	// Channel and destination scope the event: one event can go to several people.
	key := s.digest(m.EventID.String(), string(m.Channel), m.Destination)
	value := s.digest(m.TransactionID.String(), m.Subject, m.Body)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if prev, ok := s.sent[key]; ok {
		if prev != value {
			return ErrDuplicateMismatch
		}
		return nil
	}
	s.sent[key] = value
	return nil
}
func (s *MockSender) Count() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.sent) }

var _ SMSSender = (*MockSender)(nil)
var _ EmailSender = (*MockSender)(nil)
var _ WhatsAppSender = (*MockSender)(nil)
