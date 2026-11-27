// Package paystacktest is a stateful fake of the Paystack endpoints the
// adapter uses, for contract and end-to-end tests. It enforces the secret
// key, rejects duplicate references like Paystack does, supports reusable
// card authorizations and POST /transaction/charge_authorization, and can
// inject faults. Shapes follow Paystack's documented {status, message, data}
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

// CardAuth configures the card authorization returned on verify and stored
// for future POST /transaction/charge_authorization calls.
type CardAuth struct {
	AuthorizationCode string
	Email             string
	Reusable          bool
	Brand             string
	CardType          string
	Last4             string
	ExpMonth          string
	ExpYear           string
	Channel           string
	Signature         string

	// ChargeStatus overrides the status recorded when charging this
	// authorization ("success" by default; e.g. "failed", "pending", "send_otp").
	ChargeStatus string
	// RejectMessage, when non-empty, makes POST /transaction/charge_authorization
	// return HTTP 400 status=false without creating a transaction record.
	RejectMessage string
}

type txn struct {
	ID        int64
	Reference string
	Email     string
	Amount    int64
	Currency  string
	Status    string
	PaidAt    *time.Time
	Auth      *CardAuth
}

type refund struct {
	ID           int64
	Transaction  int64
	Amount       int64
	Status       string
	MerchantNote string
	TxnRef       string
}

type settlement struct {
	ID              int64
	Status          string
	Currency        string
	TotalAmount     int64
	EffectiveAmount int64
	TotalFees       int64
	SettlementDate  time.Time
}

type Fake struct {
	*httptest.Server
	Secret string

	mu          sync.Mutex
	nextID      int64
	txns        map[string]*txn
	auths       map[string]*CardAuth
	refunds     []*refund
	settlements []*settlement
	// Fault injection: the next N requests to a path prefix get HTTP 500
	// after the operation is applied ("lost response").
	failAfterApply map[string]int
	Requests       map[string]int // "METHOD /path-prefix" -> count
}

func New(secret string) *Fake {
	f := &Fake{
		Secret:         secret,
		nextID:         1000,
		txns:           map[string]*txn{},
		auths:          map[string]*CardAuth{},
		failAfterApply: map[string]int{},
		Requests:       map[string]int{},
	}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

func (f *Fake) SeedTransaction(ref, status string, amount int64, paidAt time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	t := &txn{ID: f.nextID, Reference: ref, Amount: amount, Currency: "NGN", Status: status}
	if !paidAt.IsZero() {
		t.PaidAt = &paidAt
	}
	f.txns[ref] = t
}

func (f *Fake) SeedSettlement(id int64, status string, total, fees, effective int64, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settlements = append(f.settlements, &settlement{
		ID: id, Status: status, Currency: "NGN",
		TotalAmount: total, TotalFees: fees, EffectiveAmount: effective, SettlementDate: at,
	})
}

func (f *Fake) SeedRefund(txnRef, status string, amount int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	var txnID int64
	if t, ok := f.txns[txnRef]; ok {
		txnID = t.ID
	}
	f.refunds = append(f.refunds, &refund{ID: f.nextID, Transaction: txnID, Amount: amount, Status: status, TxnRef: txnRef})
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

// Pay marks a transaction paid and attaches a default reusable card
// authorization unless one was already set via SetTxnAuthorization.
// amount 0 means the initialised amount.
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
	if t.Auth == nil {
		code := "AUTH_fake_" + strconv.FormatInt(t.ID, 10)
		ca := &CardAuth{
			AuthorizationCode: code,
			Email:             strings.ToLower(strings.TrimSpace(t.Email)),
			Reusable:          true,
			Brand:             "visa",
			CardType:          "visa DEBIT",
			Last4:             "4081",
			ExpMonth:          "12",
			ExpYear:           "2030",
			Channel:           "card",
			Signature:         "SIG_fake_" + strconv.FormatInt(t.ID, 10),
			ChargeStatus:      "success",
		}
		t.Auth = ca
		f.auths[code] = ca
	} else if t.Auth.AuthorizationCode != "" {
		if t.Auth.Email == "" {
			t.Auth.Email = strings.ToLower(strings.TrimSpace(t.Email))
		}
		if t.Auth.ChargeStatus == "" {
			t.Auth.ChargeStatus = "success"
		}
		f.auths[t.Auth.AuthorizationCode] = t.Auth
	}
}

// SetTxnAuthorization customizes the authorization returned for a transaction
// (pass nil before Pay to simulate a non-card payment with no authorization).
func (f *Fake) SetTxnAuthorization(ref string, auth *CardAuth) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.txns[ref]
	t.Auth = auth
	if auth != nil && auth.AuthorizationCode != "" {
		if auth.Email == "" {
			auth.Email = strings.ToLower(strings.TrimSpace(t.Email))
		}
		if auth.ChargeStatus == "" {
			auth.ChargeStatus = "success"
		}
		f.auths[auth.AuthorizationCode] = auth
	}
}

// AuthorizationCodeFor returns the authorization_code generated for a paid ref.
func (f *Fake) AuthorizationCodeFor(ref string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.txns[ref]; ok && t.Auth != nil {
		return t.Auth.AuthorizationCode
	}
	return ""
}

// SetAuthorizationChargeStatus configures future charge_authorization calls for
// authCode to record the given Paystack status ("success", "failed", "pending", "send_otp").
func (f *Fake) SetAuthorizationChargeStatus(authCode, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a, ok := f.auths[authCode]; ok {
		a.ChargeStatus = status
		a.RejectMessage = ""
	}
}

// RejectAuthorization configures future charge_authorization calls for authCode
// to fail at HTTP 400 validation without creating a transaction record.
func (f *Fake) RejectAuthorization(authCode, msg string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a, ok := f.auths[authCode]; ok {
		a.RejectMessage = msg
	}
}

func (f *Fake) SetStatus(ref, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.txns[ref]
	t.Status = status
	if status == "success" && t.PaidAt == nil {
		now := time.Now().UTC()
		t.PaidAt = &now
	}
}

func (f *Fake) Refunds() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.refunds)
}

// References returns every reference initialised or charged so far.
func (f *Fake) References() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for r := range f.txns {
		out = append(out, r)
	}
	return out
}

func txnPayload(t *txn) map[string]any {
	ch := "card"
	var authMap any
	if t.Auth != nil {
		if t.Auth.Channel != "" {
			ch = t.Auth.Channel
		}
		authMap = map[string]any{
			"authorization_code": t.Auth.AuthorizationCode,
			"card_type":          t.Auth.CardType,
			"brand":              t.Auth.Brand,
			"last4":              t.Auth.Last4,
			"exp_month":          t.Auth.ExpMonth,
			"exp_year":           t.Auth.ExpYear,
			"channel":            ch,
			"reusable":           t.Auth.Reusable,
			"signature":          t.Auth.Signature,
		}
	}
	return map[string]any{
		"id":            t.ID,
		"status":        t.Status,
		"reference":     t.Reference,
		"amount":        t.Amount,
		"currency":      t.Currency,
		"channel":       ch,
		"paid_at":       t.PaidAt,
		"customer":      map[string]any{"email": t.Email},
		"authorization": authMap,
	}
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+f.Secret {
		write(w, 401, false, "Invalid key", nil)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	key := r.Method + " " + r.URL.Path
	for _, p := range []string{"/transaction/initialize", "/transaction/charge_authorization", "/transaction/verify/", "/refund"} {
		if strings.HasPrefix(r.URL.Path, p) {
			f.Requests[r.Method+" "+p]++
		}
	}
	if r.URL.Path == "/transaction" {
		f.Requests[r.Method+" /transaction"]++
	}
	if r.URL.Path == "/settlement" || strings.HasPrefix(r.URL.Path, "/settlement/") {
		f.Requests[r.Method+" /settlement"]++
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
		if !validReference(body.Reference) {
			// Paystack: "Only -, ., = and alphanumeric characters allowed."
			reply(400, false, "Invalid transaction reference", nil)
			return
		}
		if _, dup := f.txns[body.Reference]; dup {
			reply(400, false, "Duplicate Transaction Reference", nil)
			return
		}
		f.nextID++
		cur := body.Currency
		if cur == "" {
			cur = "NGN"
		}
		f.txns[body.Reference] = &txn{ID: f.nextID, Reference: body.Reference, Email: body.Email, Amount: amt, Currency: cur, Status: "abandoned"}
		reply(200, true, "Authorization URL created", map[string]any{
			"authorization_url": "https://checkout.paystack.com/fake" + strconv.FormatInt(f.nextID, 10),
			"access_code":       "fake" + strconv.FormatInt(f.nextID, 10),
			"reference":         body.Reference,
		})

	case r.Method == "POST" && r.URL.Path == "/charge":
		// Documented Charge API (https://paystack.com/docs/api/charge) used by
		// the certify harness with Paystack test cards.
		var body struct {
			Email     string `json:"email"`
			Amount    string `json:"amount"`
			Currency  string `json:"currency"`
			Reference string `json:"reference"`
			Card      struct {
				Number      string `json:"number"`
				CVV         string `json:"cvv"`
				ExpiryMonth string `json:"expiry_month"`
				ExpiryYear  string `json:"expiry_year"`
			} `json:"card"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.Email == "" || body.Card.Number == "" {
			reply(400, false, "Invalid request", nil)
			return
		}
		amt, err := strconv.ParseInt(body.Amount, 10, 64)
		if err != nil || amt <= 0 {
			reply(400, false, "Invalid amount", nil)
			return
		}
		ref := body.Reference
		if ref == "" {
			f.nextID++
			ref = "chg" + strconv.FormatInt(f.nextID, 10)
		}
		if !validReference(ref) {
			reply(400, false, "Invalid transaction reference", nil)
			return
		}
		st := "failed"
		if body.Card.Number == "4084084084084081" { // documented success test card
			st = "success"
		}
		t, ok := f.txns[ref]
		if !ok {
			f.nextID++
			cur := body.Currency
			if cur == "" {
				cur = "NGN"
			}
			t = &txn{ID: f.nextID, Reference: ref, Email: body.Email, Amount: amt, Currency: cur, Status: st}
			f.txns[ref] = t
		} else {
			t.Status = st
			if amt > 0 {
				t.Amount = amt
			}
		}
		if st == "success" {
			now := time.Now().UTC()
			t.PaidAt = &now
			if t.Auth == nil {
				code := "AUTH_fake_" + strconv.FormatInt(t.ID, 10)
				ca := &CardAuth{
					AuthorizationCode: code, Email: strings.ToLower(strings.TrimSpace(t.Email)),
					Reusable: true, Brand: "visa", CardType: "visa DEBIT", Last4: "4081",
					ExpMonth: "12", ExpYear: "2030", Channel: "card",
					Signature: "SIG_fake_" + strconv.FormatInt(t.ID, 10), ChargeStatus: "success",
				}
				t.Auth = ca
				f.auths[code] = ca
			}
		}
		reply(200, true, "Charge attempted", txnPayload(t))

	case r.Method == "POST" && (r.URL.Path == "/charge/submit_pin" || r.URL.Path == "/charge/submit_otp"):
		var body struct {
			Reference string `json:"reference"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.Reference == "" {
			reply(400, false, "Invalid request", nil)
			return
		}
		t, ok := f.txns[body.Reference]
		if !ok {
			reply(400, false, "Transaction reference not found", nil)
			return
		}
		t.Status = "success"
		now := time.Now().UTC()
		t.PaidAt = &now
		reply(200, true, "Charge attempted", txnPayload(t))

	case r.Method == "POST" && r.URL.Path == "/transaction/charge_authorization":
		var body struct {
			AuthorizationCode string `json:"authorization_code"`
			Email             string `json:"email"`
			Amount            string `json:"amount"`
			Currency          string `json:"currency"`
			Reference         string `json:"reference"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.AuthorizationCode == "" || body.Email == "" || body.Reference == "" {
			reply(400, false, "Invalid request", nil)
			return
		}
		amt, err := strconv.ParseInt(body.Amount, 10, 64)
		if err != nil || amt <= 0 {
			reply(400, false, "Invalid amount", nil)
			return
		}
		if !validReference(body.Reference) {
			// Paystack: "Only -, ., = and alphanumeric characters allowed."
			reply(400, false, "Invalid transaction reference", nil)
			return
		}
		if _, dup := f.txns[body.Reference]; dup {
			reply(400, false, "Duplicate Transaction Reference", nil)
			return
		}
		auth, ok := f.auths[body.AuthorizationCode]
		if !ok || !auth.Reusable {
			reply(400, false, "Authorization does not exist or is not reusable", nil)
			return
		}
		if auth.RejectMessage != "" {
			reply(400, false, auth.RejectMessage, nil)
			return
		}
		if !strings.EqualFold(strings.TrimSpace(body.Email), strings.TrimSpace(auth.Email)) {
			reply(400, false, "Customer email does not match authorization", nil)
			return
		}
		f.nextID++
		cur := body.Currency
		if cur == "" {
			cur = "NGN"
		}
		st := auth.ChargeStatus
		if st == "" {
			st = "success"
		}
		var paidAt *time.Time
		if st == "success" {
			now := time.Now().UTC()
			paidAt = &now
		}
		cpAuth := *auth
		t := &txn{
			ID:        f.nextID,
			Reference: body.Reference,
			Email:     body.Email,
			Amount:    amt,
			Currency:  cur,
			Status:    st,
			PaidAt:    paidAt,
			Auth:      &cpAuth,
		}
		f.txns[body.Reference] = t
		reply(200, true, "Charge attempted", txnPayload(t))

	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/transaction/verify/"):
		ref := strings.TrimPrefix(r.URL.Path, "/transaction/verify/")
		t, ok := f.txns[ref]
		if !ok {
			reply(400, false, "Transaction reference not found", nil)
			return
		}
		reply(200, true, "Verification successful", txnPayload(t))

	case r.Method == "GET" && r.URL.Path == "/transaction":
		out := []map[string]any{}
		for _, t := range f.txns {
			out = append(out, txnPayload(t))
		}
		reply(200, true, "Transactions retrieved", out)

	case r.Method == "GET" && r.URL.Path == "/settlement":
		out := []map[string]any{}
		for _, s := range f.settlements {
			out = append(out, map[string]any{
				"id": s.ID, "status": s.Status, "currency": s.Currency,
				"total_amount": s.TotalAmount, "effective_amount": s.EffectiveAmount, "total_fees": s.TotalFees,
				"settlement_date": s.SettlementDate,
			})
		}
		reply(200, true, "Settlements retrieved", out)

	case r.Method == "GET" && r.URL.Path == "/refund":
		id, _ := strconv.ParseInt(r.URL.Query().Get("transaction"), 10, 64)
		filter := r.URL.Query().Get("transaction") != ""
		out := []map[string]any{}
		for _, rf := range f.refunds {
			if filter && rf.Transaction != id {
				continue
			}
			out = append(out, map[string]any{
				"id": rf.ID, "status": rf.Status, "amount": rf.Amount, "merchant_note": rf.MerchantNote,
				"transaction": map[string]any{"reference": rf.TxnRef},
			})
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
		rf := &refund{ID: f.nextID, Transaction: t.ID, Amount: amt, Status: "pending", MerchantNote: body.MerchantNote, TxnRef: t.Reference}
		f.refunds = append(f.refunds, rf)
		reply(200, true, "Refund has been queued for processing", map[string]any{"id": rf.ID, "status": rf.Status, "amount": rf.Amount, "merchant_note": rf.MerchantNote})

	default:
		write(w, 404, false, "Not found", nil)
	}
}

// validReference mirrors Paystack's rule for transaction references: only
// letters, digits, '-', '.' and '=' are allowed.
func validReference(ref string) bool {
	if ref == "" {
		return false
	}
	for _, r := range ref {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.', r == '=':
		default:
			return false
		}
	}
	return true
}
