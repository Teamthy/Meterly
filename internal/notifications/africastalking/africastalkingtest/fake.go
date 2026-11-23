// Package africastalkingtest provides a stateful fake of Africa's Talking SMS
// endpoints for contract and end-to-end tests, including fault injection
// ("applied but response lost", HTTP 5xx, 429 and recipient status overrides).
package africastalkingtest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"time"
)

type SentSMS struct {
	MessageID string
	To        string
	From      string
	Message   string
}

type rcptOverride struct {
	StatusCode int
	Status     string
}

type Fake struct {
	*httptest.Server
	Username string
	APIKey   string

	mu              sync.Mutex
	nextID          int64
	sent            []SentSMS
	failAfterApply  int
	nextHTTPStatus  int
	delay           time.Duration
	recipientStatus map[string]rcptOverride
}

func New(username, apiKey string) *Fake {
	f := &Fake{
		Username:        username,
		APIKey:          apiKey,
		nextID:          1000,
		recipientStatus: map[string]rcptOverride{},
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

func (f *Fake) SetRecipientStatus(phone string, code int, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recipientStatus[phone] = rcptOverride{StatusCode: code, Status: status}
}

func (f *Fake) Sent() []SentSMS {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]SentSMS, len(f.sent))
	copy(out, f.sent)
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

	if r.Header.Get("apiKey") != f.APIKey {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost || r.URL.Path != "/version1/messaging" {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	if r.FormValue("username") != f.Username || r.FormValue("to") == "" || r.FormValue("message") == "" {
		http.Error(w, "Invalid parameters", http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.nextHTTPStatus != 0 {
		code := f.nextHTTPStatus
		f.nextHTTPStatus = 0
		http.Error(w, "Injected HTTP status", code)
		return
	}

	to := r.FormValue("to")
	rcpt := rcptOverride{StatusCode: 101, Status: "Success"}
	if ov, ok := f.recipientStatus[to]; ok {
		rcpt = ov
	}

	f.nextID++
	msgID := "ATPid_" + strconv.FormatInt(f.nextID, 10)
	if rcpt.StatusCode >= 100 && rcpt.StatusCode < 200 {
		f.sent = append(f.sent, SentSMS{
			MessageID: msgID,
			To:        to,
			From:      r.FormValue("from"),
			Message:   r.FormValue("message"),
		})
	}
	if f.failAfterApply > 0 {
		f.failAfterApply--
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"SMSMessageData": map[string]any{
			"Message": "Sent to 1/1 Total Cost: NGN 4.0000",
			"Recipients": []map[string]any{{
				"statusCode": rcpt.StatusCode,
				"number":     to,
				"status":     rcpt.Status,
				"cost":       "NGN 4.0000",
				"messageId":  msgID,
			}},
		},
	})
}
