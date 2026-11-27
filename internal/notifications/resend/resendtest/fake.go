// Package resendtest provides a stateful fake of the Resend email API for
// contract and end-to-end tests, supporting Idempotency-Key deduplication,
// status queries and fault injection ("applied but response lost", 5xx, 429).
package resendtest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"
)

type SentEmail struct {
	ID             string
	From           string
	To             []string
	Subject        string
	Text           string
	IdempotencyKey string
	LastEvent      string
}

type Fake struct {
	*httptest.Server
	APIKey string

	mu             sync.Mutex
	nextID         int64
	emailsByID     map[string]*SentEmail
	emailsByIdem   map[string]string
	order          []string
	failAfterApply int
	nextHTTPStatus int
	delay          time.Duration
}

func New(apiKey string) *Fake {
	f := &Fake{
		APIKey:       apiKey,
		nextID:       2000,
		emailsByID:   map[string]*SentEmail{},
		emailsByIdem: map[string]string{},
	}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

func (f *Fake) FailAfterApply(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failAfterApply = n
}

func (f *Fake) SetNextHTTPStatus(code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextHTTPStatus = code
}

func (f *Fake) SetDelay(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delay = d
}

func (f *Fake) SetLastEvent(id, lastEvent string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if em, ok := f.emailsByID[id]; ok {
		em.LastEvent = lastEvent
	}
}

func (f *Fake) Sent() []SentEmail {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]SentEmail, 0, len(f.order))
	for _, id := range f.order {
		cp := *f.emailsByID[id]
		cp.To = append([]string(nil), cp.To...)
		out = append(out, cp)
	}
	return out
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	delay := f.delay
	f.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}

	if r.Header.Get("Authorization") != "Bearer "+f.APIKey {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{"name": "invalid_api_key", "message": "API key is invalid"})
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.nextHTTPStatus != 0 {
		code := f.nextHTTPStatus
		f.nextHTTPStatus = 0
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"name": "injected_error", "message": "injected failure"})
		return
	}

	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/emails":
		var body struct {
			From    string   `json:"from"`
			To      []string `json:"to"`
			Subject string   `json:"subject"`
			Text    string   `json:"text"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.From == "" || len(body.To) == 0 || body.Text == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnprocessableEntity)
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "validation_error", "message": "missing required fields"})
			return
		}
		idem := r.Header.Get("Idempotency-Key")
		if idem != "" {
			if existingID, ok := f.emailsByIdem[idem]; ok {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(map[string]any{"id": existingID})
				return
			}
		}
		f.nextID++
		id := "re_" + strconv.FormatInt(f.nextID, 10)
		em := &SentEmail{
			ID:             id,
			From:           body.From,
			To:             body.To,
			Subject:        body.Subject,
			Text:           body.Text,
			IdempotencyKey: idem,
			LastEvent:      "sent",
		}
		f.emailsByID[id] = em
		f.order = append(f.order, id)
		if idem != "" {
			f.emailsByIdem[idem] = id
		}
		if f.failAfterApply > 0 {
			f.failAfterApply--
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id})

	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/emails/"):
		id := strings.TrimPrefix(r.URL.Path, "/emails/")
		em, ok := f.emailsByID[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "not_found", "message": "Email not found"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":         em.ID,
			"object":     "email",
			"from":       em.From,
			"to":         em.To,
			"subject":    em.Subject,
			"text":       em.Text,
			"last_event": em.LastEvent,
		})

	default:
		http.NotFound(w, r)
	}
}
