// Package paystacktest is a stateful fake of the Paystack endpoints the
// adapter uses, for contract and end-to-end tests. It enforces the secret
// key, rejects duplicate references like Paystack does, and can inject
// faults. Shapes follow Paystack's documented {status, message, data}
// envelope.
package paystacktest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"
)

type txn struct {
	ID        int64
	Reference string
	Email     string
	Amount    int64
	Currency  string
	Status    string
	PaidAt    *time.Time
}

type refund struct {
	ID           int64
	Transaction  int64
	Amount       int64
	Status       string
	MerchantNote string
}

type Fake struct {
	*httptest.Server
	Secret string

	mu      sync.Mutex
	nextID  int64
	txns    map[string]*txn
	refunds []*refund
	// Fault injection: the next N requests to a path prefix get HTTP 500
	// after the operation is applied ("lost response").
	failAfterApply map[string]int
	Requests       map[string]int // "METHOD /path-prefix" -> count
}

func New(secret string) *Fake {
	f := &Fake{Secret: secret, nextID: 1000, txns: map[string]*txn{}, failAfterApply: map[string]int{}, Requests: map[string]int{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

func write(w http.ResponseWriter, code int, status bool, msg string, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"status": status, "message": msg, "data": data})
}

// FailAfterApply makes the next n requests whose "METHOD path" starts with
// prefix return HTTP 500 after the change was applied.
func (f *Fake) FailAfterApply(prefix string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failAfterApply[prefix] = n
}

// Pay marks a transaction paid. amount 0 means the initialised amount.
func (f *Fake) Pay(ref string, amount int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.txns[ref]
	t.Status = "success"
	if amount > 0 {
		t.Amount = amount
	}
	now := time.Now().UTC()
	t.PaidAt = &now
}

func (f *Fake) SetStatus(ref, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.txns[ref].Status = status
}

func (f *Fake) Refunds() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.refunds)
}

// References returns every reference initialised so far.
func (f *Fake) References() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for r := range f.txns {
		out = append(out, r)
	}
	return out
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+f.Secret {
		write(w, 401, false, "Invalid key", nil)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	key := r.Method + " " + r.URL.Path
	for _, p := range []string{"/transaction/initialize", "/transaction/verify/", "/refund"} {
		if strings.HasPrefix(r.URL.Path, p) {
			f.Requests[r.Method+" "+p]++
		}
	}
	lose := false
	for p, n := range f.failAfterApply {
		if n > 0 && strings.HasPrefix(key, p) {
			f.failAfterApply[p] = n - 1
			lose = true
		}
	}
	reply := func(code int, status bool, msg string, data any) {
		if lose {
			write(w, 500, false, "Internal error", nil)
			return
		}
		write(w, code, status, msg, data)
	}

	switch {
	case r.Method == "POST" && r.URL.Path == "/transaction/initialize":
		var body struct {
			Email     string `json:"email"`
			Amount    string `json:"amount"`
			Currency  string `json:"currency"`
			Reference string `json:"reference"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.Email == "" || body.Reference == "" {
			reply(400, false, "Invalid request", nil)
			return
		}
		amt, err := strconv.ParseInt(body.Amount, 10, 64)
		if err != nil || amt <= 0 {
			reply(400, false, "Invalid amount", nil)
			return
		}
		if _, dup := f.txns[body.Reference]; dup {
			reply(400, false, "Duplicate Transaction Reference", nil)
			return
		}
		f.nextID++
		f.txns[body.Reference] = &txn{ID: f.nextID, Reference: body.Reference, Email: body.Email, Amount: amt, Currency: body.Currency, Status: "abandoned"}
		reply(200, true, "Authorization URL created", map[string]any{
			"authorization_url": "https://checkout.paystack.com/fake" + strconv.FormatInt(f.nextID, 10),
			"access_code":       "fake" + strconv.FormatInt(f.nextID, 10),
			"reference":         body.Reference,
		})
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/transaction/verify/"):
		ref := strings.TrimPrefix(r.URL.Path, "/transaction/verify/")
		t, ok := f.txns[ref]
		if !ok {
			reply(400, false, "Transaction reference not found", nil)
			return
		}
		reply(200, true, "Verification successful", map[string]any{
			"id": t.ID, "status": t.Status, "reference": t.Reference, "amount": t.Amount, "currency": t.Currency, "paid_at": t.PaidAt,
		})
	case r.Method == "GET" && r.URL.Path == "/refund":
		id, _ := strconv.ParseInt(r.URL.Query().Get("transaction"), 10, 64)
		out := []map[string]any{}
		for _, rf := range f.refunds {
			if rf.Transaction == id {
				out = append(out, map[string]any{"id": rf.ID, "status": rf.Status, "amount": rf.Amount, "merchant_note": rf.MerchantNote})
			}
		}
		reply(200, true, "Refunds retrieved", out)
	case r.Method == "POST" && r.URL.Path == "/refund":
		var body struct {
			Transaction  string `json:"transaction"`
			Amount       string `json:"amount"`
			MerchantNote string `json:"merchant_note"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			reply(400, false, "Invalid request", nil)
			return
		}
		t, ok := f.txns[body.Transaction]
		if !ok || t.Status != "success" {
			reply(400, false, "Transaction not found or not refundable", nil)
			return
		}
		amt, _ := strconv.ParseInt(body.Amount, 10, 64)
		if amt <= 0 {
			amt = t.Amount
		}
		var reversed int64
		for _, rf := range f.refunds {
			if rf.Transaction == t.ID && rf.Status != "failed" {
				reversed += rf.Amount
			}
		}
		if reversed+amt > t.Amount {
			reply(400, false, "Transaction has been fully reversed", nil)
			return
		}
		f.nextID++
		rf := &refund{ID: f.nextID, Transaction: t.ID, Amount: amt, Status: "pending", MerchantNote: body.MerchantNote}
		f.refunds = append(f.refunds, rf)
		reply(200, true, "Refund has been queued for processing", map[string]any{"id": rf.ID, "status": rf.Status, "amount": rf.Amount, "merchant_note": rf.MerchantNote})
	default:
		write(w, 404, false, "Not found", nil)
	}
}
