// Package hestest is a stateful fake of Meterly's HES gateway contract.
// It is not a Hexing/Conlog/Mojec simulator.
package hestest

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

type record struct {
	ProviderRef string
	Status      string
	FailureCode string
	Meter       string
	At          time.Time
}

type Fake struct {
	*httptest.Server
	Secret string

	mu       sync.Mutex
	byKey    map[string]*record
	eligible map[string]bool
	Calls    int
}

func New(secret string) *Fake {
	f := &Fake{
		Secret:   secret,
		byKey:    map[string]*record{},
		eligible: map[string]bool{},
	}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

func (f *Fake) Allow(meter string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.eligible[meter] = true
}

func (f *Fake) Settle() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, rec := range f.byKey {
		if rec.Status == "ACCEPTED" {
			rec.Status = "CONFIRMED"
			rec.At = time.Now().UTC()
		}
	}
}

func (f *Fake) FailPending() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, rec := range f.byKey {
		if rec.Status == "ACCEPTED" {
			rec.Status = "FAILED"
			rec.FailureCode = "METER_REJECTED"
			rec.At = time.Now().UTC()
		}
	}
}

func (f *Fake) Has(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.byKey[key]
	return ok
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.Calls++
	f.mu.Unlock()

	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/eligibility":
		f.eligibility(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/credit":
		f.credit(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/credit":
		f.status(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (f *Fake) eligibility(w http.ResponseWriter, r *http.Request) {
	if !f.check(w, r, true) {
		return
	}
	var req struct {
		MeterNumber string `json:"meter_number"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil {
		http.Error(w, "bad json", 400)
		return
	}
	ok := f.isEligible(req.MeterNumber)
	writeJSON(w, 200, map[string]any{"eligible": ok, "reachable": ok})
}

func (f *Fake) credit(w http.ResponseWriter, r *http.Request) {
	body, ok := f.readSigned(w, r)
	if !ok {
		return
	}
	var req struct {
		MeterNumber    string `json:"meter_number"`
		IdempotencyKey string `json:"idempotency_key"`
		Token          string `json:"token"`
	}
	if json.Unmarshal(body, &req) != nil || req.IdempotencyKey == "" {
		http.Error(w, "bad json", 400)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if rec, exists := f.byKey[req.IdempotencyKey]; exists {
		writeJSON(w, 200, creditBody(rec))
		return
	}
	if !eligibleLocked(f, req.MeterNumber) {
		writeJSON(w, 200, map[string]any{"status": "FAILED", "failure_code": "NOT_ELIGIBLE"})
		return
	}
	rec := &record{
		ProviderRef: "hh_" + uuid.NewString()[:12],
		Status:      "ACCEPTED",
		Meter:       req.MeterNumber,
		At:          time.Now().UTC(),
	}
	f.byKey[req.IdempotencyKey] = rec
	if strings.HasPrefix(req.MeterNumber, "77") {
		rec.Status = "CONFIRMED"
		writeJSON(w, 500, map[string]any{"status": "CONFIRMED"})
		return
	}
	writeJSON(w, 200, creditBody(rec))
}

func (f *Fake) status(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("idempotency_key")
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.byKey[key]
	if !ok {
		writeJSON(w, 200, map[string]any{"status": "NOT_FOUND"})
		return
	}
	writeJSON(w, 200, creditBody(rec))
}

func (f *Fake) isEligible(meter string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return eligibleLocked(f, meter)
}

func eligibleLocked(f *Fake, meter string) bool {
	if f.eligible[meter] {
		return true
	}
	return strings.HasPrefix(meter, "7")
}

func (f *Fake) check(w http.ResponseWriter, r *http.Request, signed bool) bool {
	if !signed {
		return true
	}
	body, ok := f.readSigned(w, r)
	if !ok {
		return false
	}
	r.Body = io.NopCloser(strings.NewReader(string(body)))
	return true
}

func (f *Fake) readSigned(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read", 400)
		return nil, false
	}
	got, err := hex.DecodeString(r.Header.Get("X-Meterly-Signature"))
	if err != nil || len(got) == 0 {
		http.Error(w, "sig", 401)
		return nil, false
	}
	mac := hmac.New(sha256.New, []byte(f.Secret))
	mac.Write(raw)
	want := mac.Sum(nil)
	if !hmac.Equal(got, want) {
		http.Error(w, "sig", 401)
		return nil, false
	}
	return raw, true
}

func creditBody(rec *record) map[string]any {
	return map[string]any{
		"provider_ref": rec.ProviderRef,
		"status":       rec.Status,
		"failure_code": rec.FailureCode,
		"at":           rec.At.UTC().Format(time.RFC3339),
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
