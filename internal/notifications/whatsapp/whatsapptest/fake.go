// Package whatsapptest provides a stateful fake of the Meta WhatsApp Cloud API
// for contract and end-to-end tests, enforcing the 24-hour customer-service
// window (error 131047 on free-form text outside an active window) and
// supporting fault injection ("applied but response lost", HTTP 5xx, 429).
package whatsapptest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"
)

type SentMessage struct {
	WAMID        string
	To           string
	Type         string
	TemplateName string
	LanguageCode string
	TextBody     string
}

type Fake struct {
	*httptest.Server
	PhoneNumberID string
	AccessToken   string

	mu             sync.Mutex
	nextID         int64
	sent           []SentMessage
	openWindows    map[string]time.Time // digits without leading '+' -> window expiry
	now            func() time.Time
	failAfterApply int
	nextHTTPStatus int
	delay          time.Duration
}

func New(phoneNumberID, accessToken string) *Fake {
	f := &Fake{
		PhoneNumberID: phoneNumberID,
		AccessToken:   accessToken,
		nextID:        3000,
		openWindows:   map[string]time.Time{},
		now:           time.Now,
	}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

func (f *Fake) OpenCustomerServiceWindow(e164Phone string, d time.Duration) {
	digits := strings.TrimPrefix(strings.TrimSpace(e164Phone), "+")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.openWindows[digits] = f.now().Add(d)
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

func (f *Fake) Sent() []SentMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]SentMessage, len(f.sent))
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

	if r.Header.Get("Authorization") != "Bearer "+f.AccessToken {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": 190, "message": "Invalid OAuth access token"}})
		return
	}
	if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/"+f.PhoneNumberID+"/messages") {
		http.NotFound(w, r)
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.nextHTTPStatus != 0 {
		code := f.nextHTTPStatus
		f.nextHTTPStatus = 0
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": "injected error"}})
		return
	}

	var req struct {
		MessagingProduct string `json:"messaging_product"`
		To               string `json:"to"`
		Type             string `json:"type"`
		Template         struct {
			Name     string `json:"name"`
			Language struct {
				Code string `json:"code"`
			} `json:"language"`
		} `json:"template"`
		Text struct {
			Body string `json:"body"`
		} `json:"text"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil || req.MessagingProduct != "whatsapp" || req.To == "" {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": 100, "message": "Invalid parameter"}})
		return
	}
	digits := strings.TrimPrefix(req.To, "+")

	if req.Type == "text" {
		exp, ok := f.openWindows[digits]
		if !ok || !f.now().Before(exp) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{
					"code":    131047,
					"type":    "OAuthException",
					"message": "Re-engagement message: more than 24 hours have passed since the customer last replied",
				},
			})
			return
		}
	} else if req.Type != "template" || req.Template.Name == "" {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": 132001, "message": "Template name does not exist"}})
		return
	}

	f.nextID++
	wamid := "wamid.HBgN" + strconv.FormatInt(f.nextID, 10)
	f.sent = append(f.sent, SentMessage{
		WAMID:        wamid,
		To:           digits,
		Type:         req.Type,
		TemplateName: req.Template.Name,
		LanguageCode: req.Template.Language.Code,
		TextBody:     req.Text.Body,
	})
	if f.failAfterApply > 0 {
		f.failAfterApply--
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"messaging_product": "whatsapp",
		"contacts":          []map[string]any{{"input": digits, "wa_id": digits}},
		"messages":          []map[string]any{{"id": wamid}},
	})
}
