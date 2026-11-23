package httpapi_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image/png"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/auth"
	"github.com/teamthy/meterly/internal/httpapi"
	"github.com/teamthy/meterly/internal/notifications"
	"github.com/teamthy/meterly/internal/notificationservice"
	"github.com/teamthy/meterly/internal/observability"
	"github.com/teamthy/meterly/internal/orchestrator"
	"github.com/teamthy/meterly/internal/outbox"
	"github.com/teamthy/meterly/internal/providers/mock"
	"github.com/teamthy/meterly/internal/providers/paystack"
	"github.com/teamthy/meterly/internal/providers/paystack/paystacktest"
	"github.com/teamthy/meterly/internal/registry"
	"github.com/teamthy/meterly/internal/secure"
	"github.com/teamthy/meterly/internal/store"
)

type recordingSender struct {
	mu   sync.Mutex
	msgs []notifications.Message
}

func (s *recordingSender) Send(_ context.Context, m notifications.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs = append(s.msgs, m)
	return nil
}

func (s *recordingSender) Sent() []notifications.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]notifications.Message(nil), s.msgs...)
}

type payForMeEnv struct {
	c         client
	st        store.Store
	box       *secure.Box
	o         *orchestrator.Orchestrator
	fake      *paystacktest.Fake
	notifSvc  *notificationservice.Service
	pub       *outbox.Publisher
	mockSMS   *recordingSender
	mockEmail *recordingSender
	metrics   *observability.Registry
	logs      *syncBuf
	clockMu   sync.RWMutex
	clock     time.Time
}

func (e *payForMeEnv) now() time.Time {
	e.clockMu.RLock()
	defer e.clockMu.RUnlock()
	return e.clock
}

func (e *payForMeEnv) advanceClock(d time.Duration) {
	e.clockMu.Lock()
	e.clock = e.clock.Add(d)
	e.clockMu.Unlock()
}

func newPayForMeEnv(t *testing.T, st store.Store, limits httpapi.Limits, disabled bool) *payForMeEnv {
	t.Helper()
	e := &payForMeEnv{
		st:      st,
		logs:    &syncBuf{},
		metrics: observability.NewRegistry(),
		clock:   time.Now().UTC().Truncate(time.Second),
	}
	log := slog.New(slog.NewJSONHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	e.fake = paystacktest.New(psSecret)
	t.Cleanup(e.fake.Close)

	ps, err := paystack.New(paystack.Config{SecretKey: psSecret, BaseURL: e.fake.URL})
	if err != nil {
		t.Fatal(err)
	}
	box, err := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "v1")
	if err != nil {
		t.Fatal(err)
	}
	e.box = box

	// Use remote=nil so PREPAID purchases follow TOKEN vending deterministically.
	vend := mock.NewVending()
	reg := registry.New(st, box, vend, nil)
	reg.Now = e.now

	e.o = orchestrator.New(orchestrator.DefaultConfig(), st, reg, box, ps, vend, nil, log)
	e.o.Now = e.now
	e.o.SetMetrics(e.metrics)

	e.mockSMS = &recordingSender{}
	e.mockEmail = &recordingSender{}
	ns, err := notificationservice.New(notificationservice.Config{
		Store: st,
		Box:   box,
		Providers: map[notifications.Channel]notifications.Provider{
			notifications.SMS:   notifications.WrapSender("mocksms", notifications.SMS, e.mockSMS),
			notifications.Email: notifications.WrapSender("mockemail", notifications.Email, e.mockEmail),
		},
		Metrics: e.metrics,
		Now:     e.now,
		Log:     log,
	})
	if err != nil {
		t.Fatal(err)
	}
	e.notifSvc = ns
	e.pub = outbox.NewPublisher(st, ns.Sink(), log)

	srvCfg := &httpapi.Server{
		O:                e.o,
		Auth:             auth.DevAuth{},
		Payments:         ps,
		Notifications:    ns,
		Metrics:          e.metrics,
		Log:              log,
		Limits:           limits,
		PayForMeDisabled: disabled,
		PublicBaseURL:    "https://meterly.ng",
		Dev:              true,
	}
	srv := httptest.NewServer(srvCfg.Routes())
	t.Cleanup(srv.Close)
	e.c = client{t, srv}
	return e
}

func (e *payForMeEnv) payAndWebhook(t *testing.T, txnID string) {
	t.Helper()
	var ref string
	for _, r := range e.fake.References() {
		if strings.HasPrefix(r, txnID+".") {
			if txObj, _ := e.st.Repos().Transactions().ByPaymentRef(t.Context(), r); txObj != nil {
				ref = r
				break
			}
		}
	}
	if ref == "" {
		t.Fatalf("no Paystack reference committed for txn %s", txnID)
	}
	e.fake.Pay(ref, 0)
	body, _ := json.Marshal(map[string]any{
		"event": "charge.success",
		"data":  map[string]any{"reference": ref, "status": "success"},
	})
	req, _ := http.NewRequest("POST", e.c.srv.URL+"/v1/payments/webhooks/paystack", bytes.NewReader(body))
	req.Header.Set("X-Paystack-Signature", paystack.Sign(psSecret, body))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("webhook status = %d", res.StatusCode)
	}
}

func (e *payForMeEnv) rawRequest(method, path, token string, body any, headers map[string]string) (*http.Response, string, map[string]any) {
	e.c.t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, e.c.srv.URL+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.c.t.Fatal(err)
	}
	defer res.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(res.Body)
	var parsed map[string]any
	_ = json.Unmarshal(buf.Bytes(), &parsed)
	return res, buf.String(), parsed
}

func TestPayForMeEndToEndPrivacyNotificationsAndReplay(t *testing.T) {
	eachStore(t, func(t *testing.T, st store.Store) {
		e := newPayForMeEnv(t, st, httpapi.Limits{}, false)

		// Register meter for requester "ada" and attach verified phone + display name.
		code, m := e.c.do("POST", "/v1/meters", "ada", map[string]any{
			"meter_number": "45012345678",
			"disco_code":   "IKEDC",
			"label":        "Home Meter",
		}, nil)
		if code != 201 {
			t.Fatalf("register meter: %d %v", code, m)
		}
		meterID := m["meter_id"].(string)
		discoCustomerName, _ := m["customer_name"].(string)

		adaUID, err := st.Repos().Users().EnsureByExternalID(t.Context(), "ada")
		if err != nil {
			t.Fatal(err)
		}
		ctPhone, hmPhone, maskPhone, _ := notifications.SealUserPhone(e.box, adaUID, "+2348012345678")
		if err := st.Repos().Users().UpsertContacts(t.Context(), notifications.UserContacts{
			UserID:          adaUID,
			PhoneCiphertext: ctPhone,
			PhoneHMAC:       hmPhone,
			PhoneMasked:     maskPhone,
			PhoneKeyVersion: e.box.KeyVersion,
			DisplayName:     "Adaeze",
		}); err != nil {
			t.Fatal(err)
		}

		// Create RANGE Pay-for-Me link.
		res, _, created := e.rawRequest("POST", "/v1/payment-requests", "ada", map[string]any{
			"meter_id":         meterID,
			"amount_mode":      "RANGE",
			"min_amount_minor": 200_000,
			"max_amount_minor": 1_000_000,
			"note":             "Light don go, abeg 🙏",
		}, nil)
		if res.StatusCode != 201 {
			t.Fatalf("create payment request: %d %v", res.StatusCode, created)
		}
		if res.Header.Get("Cache-Control") != "no-store" || res.Header.Get("Referrer-Policy") != "no-referrer" {
			t.Fatalf("missing security headers on create: %v", res.Header)
		}

		linkCode := created["code"].(string)
		shareURL := created["share_url"].(string)
		if shareURL != "https://meterly.ng/p/"+linkCode {
			t.Fatalf("unexpected share_url %q", shareURL)
		}
		qrBytes, err := base64.StdEncoding.DecodeString(created["qr_png_base64"].(string))
		if err != nil {
			t.Fatalf("decode qr base64: %v", err)
		}
		if _, err := png.Decode(bytes.NewReader(qrBytes)); err != nil {
			t.Fatalf("decode qr png: %v", err)
		}

		// Audit event recorded for creation.
		reqUUID := uuid.MustParse(created["id"].(string))
		audits, err := st.Repos().Audit().ForResource(t.Context(), "payment_request", reqUUID)
		if err != nil || len(audits) != 1 || audits[0].Action != "payment_request.created" {
			t.Fatalf("expected payment_request.created audit, got %+v %v", audits, err)
		}

		// Public GET returns masked info only — never address, DisCo customer_name, or full meter number.
		pubRes, pubRaw, pubView := e.rawRequest("GET", "/v1/public/pay/"+linkCode, "", nil, nil)
		if pubRes.StatusCode != 200 {
			t.Fatalf("public view: %d %s", pubRes.StatusCode, pubRaw)
		}
		if pubRes.Header.Get("Cache-Control") != "no-store" || pubRes.Header.Get("Referrer-Policy") != "no-referrer" {
			t.Fatalf("missing public security headers: %v", pubRes.Header)
		}
		if pubView["requester_display_name"] != "ada" || pubView["masked_meter_number"] != "*******5678" || pubView["note"] != "Light don go, abeg 🙏" {
			t.Fatalf("unexpected public view: %v", pubView)
		}
		for _, forbidden := range []string{"45012345678", "address", "customer_name"} {
			if strings.Contains(pubRaw, forbidden) {
				t.Fatalf("public view leaked %q: %s", forbidden, pubRaw)
			}
		}
		if discoCustomerName != "" && strings.Contains(pubRaw, discoCustomerName) {
			t.Fatalf("public view leaked DisCo customer_name %q: %s", discoCustomerName, pubRaw)
		}

		// Out-of-bounds RANGE amounts are rejected.
		if c, _ := e.c.do("POST", "/v1/public/pay/"+linkCode+"/checkout", "", map[string]any{
			"amount_minor": 150_000, "email": "uncle.tunde@example.com",
		}, map[string]string{"Idempotency-Key": "range-too-low-0001"}); c != 422 {
			t.Fatalf("expected 422 below min_amount_minor, got %d", c)
		}
		if c, _ := e.c.do("POST", "/v1/public/pay/"+linkCode+"/checkout", "", map[string]any{
			"amount_minor": 1_500_000, "email": "uncle.tunde@example.com",
		}, map[string]string{"Idempotency-Key": "range-too-high-001"}); c != 422 {
			t.Fatalf("expected 422 above max_amount_minor, got %d", c)
		}

		// Valid guest checkout + idempotent replay.
		coRes, coRaw, co := e.rawRequest("POST", "/v1/public/pay/"+linkCode+"/checkout", "", map[string]any{
			"amount_minor": 500_000,
			"email":        "uncle.tunde@example.com",
			"payer_name":   "Uncle Tunde",
		}, map[string]string{"Idempotency-Key": "guest-checkout-0001"})
		if coRes.StatusCode != 202 {
			t.Fatalf("checkout: %d %s", coRes.StatusCode, coRaw)
		}
		if coRes.Header.Get("Cache-Control") != "no-store" || coRes.Header.Get("Referrer-Policy") != "no-referrer" {
			t.Fatalf("missing checkout security headers: %v", coRes.Header)
		}
		txnID := co["transaction_id"].(string)
		statusCode := co["status_code"].(string)

		// Replay with identical Idempotency-Key returns same transaction_id and status_code.
		co2Code, co2 := e.c.do("POST", "/v1/public/pay/"+linkCode+"/checkout", "", map[string]any{
			"amount_minor": 500_000,
			"email":        "uncle.tunde@example.com",
			"payer_name":   "Uncle Tunde",
		}, map[string]string{"Idempotency-Key": "guest-checkout-0001"})
		if co2Code != 202 || co2["transaction_id"] != txnID || co2["status_code"] != statusCode {
			t.Fatalf("idempotent replay mismatch: %d %v", co2Code, co2)
		}
		// Replay with mutated body returns 409 IDEMPOTENCY_CONFLICT.
		if c, _ := e.c.do("POST", "/v1/public/pay/"+linkCode+"/checkout", "", map[string]any{
			"amount_minor": 600_000,
			"email":        "uncle.tunde@example.com",
			"payer_name":   "Uncle Tunde",
		}, map[string]string{"Idempotency-Key": "guest-checkout-0001"}); c != 409 {
			t.Fatalf("expected 409 on mutated idempotency replay, got %d", c)
		}

		// Complete Paystack payment and run outbox + M5 notifications.
		e.payAndWebhook(t, txnID)
		if _, err := e.pub.PublishBatch(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, err := e.notifSvc.ProcessBatch(t.Context(), 50); err != nil {
			t.Fatal(err)
		}

		// Guest status endpoint returns COMPLETED + token-free receipt; NEVER the STS token.
		stRes, stRaw, stBody := e.rawRequest("GET", "/v1/public/pay/status/"+statusCode, "", nil, nil)
		if stRes.StatusCode != 200 || stBody["status"] != "COMPLETED" {
			t.Fatalf("public status: %d %s", stRes.StatusCode, stRaw)
		}
		if stRes.Header.Get("Cache-Control") != "no-store" || stRes.Header.Get("Referrer-Policy") != "no-referrer" {
			t.Fatalf("missing status security headers: %v", stRes.Header)
		}

		// Requester retrieves the 20-digit STS token.
		tokCode, tokBody := e.c.do("GET", "/v1/electricity/transactions/"+txnID+"/token", "ada", nil, nil)
		if tokCode != 200 {
			t.Fatalf("requester token fetch: %d %v", tokCode, tokBody)
		}
		rawToken := tokBody["token"].(string)
		if len(rawToken) < 20 {
			t.Fatalf("unexpected STS token %q", rawToken)
		}
		if strings.Contains(stRaw, rawToken) || strings.Contains(stRaw, `"token"`) {
			t.Fatalf("public status leaked STS token: %s", stRaw)
		}

		// Guest payer received ONLY the token-free payment receipt email; requester received SMS alerts.
		emails := e.mockEmail.Sent()
		if len(emails) != 1 || emails[0].Destination != "uncle.tunde@example.com" {
			t.Fatalf("expected 1 receipt email to guest payer, got %+v", emails)
		}
		if strings.Contains(emails[0].Body, rawToken) || strings.Contains(strings.ToLower(emails[0].Body), "token") {
			t.Fatalf("guest payer receipt email must never mention or contain token: %q", emails[0].Body)
		}
		if smsSent := e.mockSMS.Sent(); len(smsSent) != 2 {
			t.Fatalf("expected requester to receive 2 SMS notifications (payment.confirmed + token.available), got %d", len(smsSent))
		}

		// Logs and Prometheus metrics NEVER leak codes, emails, payer names or STS tokens.
		mRec := httptest.NewRecorder()
		e.metrics.Handler().ServeHTTP(mRec, httptest.NewRequest("GET", "/metrics", nil))
		metricsText := mRec.Body.String()
		logsText := e.logs.String()
		for _, secret := range []string{linkCode, statusCode, "uncle.tunde@example.com", "Uncle Tunde", rawToken, "45012345678"} {
			if strings.Contains(logsText, secret) {
				t.Fatalf("logs leaked secret %q", secret)
			}
			if strings.Contains(metricsText, secret) {
				t.Fatalf("/metrics leaked secret %q", secret)
			}
		}
	})
}

func TestPayForMeSingleUseConcurrencyRace(t *testing.T) {
	eachStore(t, func(t *testing.T, st store.Store) {
		e := newPayForMeEnv(t, st, httpapi.Limits{PublicPayPerMinute: 600, PublicLinkPerMinute: 600}, false)
		_, m := e.c.do("POST", "/v1/meters", "ada", map[string]any{"meter_number": "45012345678", "disco_code": "IKEDC"}, nil)
		_, created := e.c.do("POST", "/v1/payment-requests", "ada", map[string]any{
			"meter_id":     m["meter_id"],
			"amount_mode":  "FIXED",
			"amount_minor": 500_000,
			"max_payments": 1,
		}, nil)
		linkCode := created["code"].(string)

		const racers = 8
		var wg sync.WaitGroup
		wg.Add(racers)
		codes := make([]int, racers)
		errCodes := make([]string, racers)
		for i := 0; i < racers; i++ {
			go func(idx int) {
				defer wg.Done()
				c, body := e.c.do("POST", "/v1/public/pay/"+linkCode+"/checkout", "", map[string]any{
					"email": "racer@example.com",
				}, map[string]string{"Idempotency-Key": "concurrent-race-" + uuid.NewString()})
				codes[idx] = c
				if errObj, ok := body["error"].(map[string]any); ok {
					errCodes[idx], _ = errObj["code"].(string)
				}
			}(i)
		}
		wg.Wait()

		wins, alreadyPaid := 0, 0
		for i, c := range codes {
			switch {
			case c == 202:
				wins++
			case c == 409 && errCodes[i] == "LINK_ALREADY_PAID":
				alreadyPaid++
			default:
				t.Fatalf("unexpected race outcome idx=%d status=%d code=%q", i, c, errCodes[i])
			}
		}
		if wins != 1 || alreadyPaid != racers-1 {
			t.Fatalf("want 1 winner and %d LINK_ALREADY_PAID, got wins=%d alreadyPaid=%d", racers-1, wins, alreadyPaid)
		}
	})
}

func TestPayForMeAbandonedPaymentReopensSlotAndMidCheckoutExpiryHonoured(t *testing.T) {
	eachStore(t, func(t *testing.T, st store.Store) {
		e := newPayForMeEnv(t, st, httpapi.Limits{}, false)
		_, m := e.c.do("POST", "/v1/meters", "ada", map[string]any{"meter_number": "45012345678", "disco_code": "IKEDC"}, nil)

		// 1. Abandoned checkout reopens a single-use link when Resume expires the unpaid transaction.
		_, pr1 := e.c.do("POST", "/v1/payment-requests", "ada", map[string]any{
			"meter_id":         m["meter_id"],
			"amount_mode":      "FIXED",
			"amount_minor":     300_000,
			"expires_in_hours": 72,
		}, nil)
		code1 := pr1["code"].(string)

		c1, co1 := e.c.do("POST", "/v1/public/pay/"+code1+"/checkout", "", map[string]any{
			"email": "abandoned@example.com",
		}, map[string]string{"Idempotency-Key": "abandon-first-0001"})
		if c1 != 202 {
			t.Fatalf("first checkout: %d %v", c1, co1)
		}
		// While in-flight, link reports PAID and blocks a second payer.
		if c, body := e.c.do("POST", "/v1/public/pay/"+code1+"/checkout", "", map[string]any{
			"email": "second@example.com",
		}, map[string]string{"Idempotency-Key": "abandon-blocked-01"}); c != 409 || body["error"].(map[string]any)["code"] != "LINK_ALREADY_PAID" {
			t.Fatalf("expected 409 LINK_ALREADY_PAID while first checkout pending, got %d %v", c, body)
		}

		// Advance past PaymentExpiry (2h default) while still within 72h link TTL, and run Resume.
		e.advanceClock(3 * time.Hour)
		txn1ID := uuid.MustParse(co1["transaction_id"].(string))
		resumed, err := e.o.Resume(t.Context(), txn1ID)
		if err != nil || resumed.Status() != "FAILED" {
			t.Fatalf("expected abandoned txn to fail on Resume, got %+v %v", resumed, err)
		}

		// Link is OPEN again and a new guest can check out and complete payment!
		_, reopenedView := e.c.do("GET", "/v1/public/pay/"+code1, "", nil, nil)
		if reopenedView["status"] != "OPEN" || reopenedView["payments_count"].(float64) != 0 {
			t.Fatalf("expected reopened link OPEN with payments_count=0, got %v", reopenedView)
		}
		c2, co2 := e.c.do("POST", "/v1/public/pay/"+code1+"/checkout", "", map[string]any{
			"email": "rescuer@example.com",
		}, map[string]string{"Idempotency-Key": "rescuer-checkout-01"})
		if c2 != 202 {
			t.Fatalf("rescuer checkout failed: %d %v", c2, co2)
		}
		e.payAndWebhook(t, co2["transaction_id"].(string))
		_, st2 := e.c.do("GET", "/v1/public/pay/status/"+co2["status_code"].(string), "", nil, nil)
		if st2["status"] != "COMPLETED" {
			t.Fatalf("expected rescuer payment COMPLETED, got %v", st2)
		}

		// 2. Link expires while a checkout is open -> new checkouts fail with LINK_EXPIRED,
		//    but the already-created checkout is still honoured when paid!
		_, pr2 := e.c.do("POST", "/v1/payment-requests", "ada", map[string]any{
			"meter_id":         m["meter_id"],
			"amount_mode":      "FIXED",
			"amount_minor":     400_000,
			"expires_in_hours": 1,
			"max_payments":     2,
		}, nil)
		code2 := pr2["code"].(string)

		cOpen, coOpen := e.c.do("POST", "/v1/public/pay/"+code2+"/checkout", "", map[string]any{
			"email": "ontime@example.com",
		}, map[string]string{"Idempotency-Key": "mid-expiry-open-01"})
		if cOpen != 202 {
			t.Fatalf("open checkout: %d %v", cOpen, coOpen)
		}

		// Advance 90m: past 1h link expiry, still within 2h PaymentExpiry.
		e.advanceClock(90 * time.Minute)
		if cLate, lateBody := e.c.do("POST", "/v1/public/pay/"+code2+"/checkout", "", map[string]any{
			"email": "too.late@example.com",
		}, map[string]string{"Idempotency-Key": "mid-expiry-late-01"}); cLate != 409 || lateBody["error"].(map[string]any)["code"] != "LINK_EXPIRED" {
			t.Fatalf("expected 409 LINK_EXPIRED after TTL, got %d %v", cLate, lateBody)
		}

		// Already-created checkout completes cleanly!
		e.payAndWebhook(t, coOpen["transaction_id"].(string))
		_, stOpen := e.c.do("GET", "/v1/public/pay/status/"+coOpen["status_code"].(string), "", nil, nil)
		if stOpen["status"] != "COMPLETED" {
			t.Fatalf("expected already-created checkout to be honoured after link expiry, got %v", stOpen)
		}
	})
}

func TestPayForMeEdgeCasesRevokedGrantSelfPayAntiPhishingAndRateLimits(t *testing.T) {
	eachStore(t, func(t *testing.T, st store.Store) {
		e := newPayForMeEnv(t, st, httpapi.Limits{
			WritesPerMinute:     600,
			PublicPayPerMinute:  4,
			PublicLinkPerMinute: 2,
			ClientIPHeader:      "X-Forwarded-For",
		}, false)
		_, m := e.c.do("POST", "/v1/meters", "ada", map[string]any{"meter_number": "45012345678", "disco_code": "IKEDC"}, nil)
		meterID := m["meter_id"].(string)

		// Anti-phishing note rejection.
		for _, badNote := range []string{"Pay at https://evil.example/pay", "Visit meterly.ng/fake", strings.Repeat("x", 141)} {
			if c, _ := e.c.do("POST", "/v1/payment-requests", "ada", map[string]any{
				"meter_id": meterID, "amount_mode": "FIXED", "amount_minor": 200_000, "note": badNote,
			}, nil); c != 400 {
				t.Fatalf("expected 400 for bad note %q, got %d", badNote, c)
			}
		}

		// Cancellation audited + blocks checkout with 409 LINK_CANCELLED; non-owner gets 404.
		_, toCancel := e.c.do("POST", "/v1/payment-requests", "ada", map[string]any{
			"meter_id": meterID, "amount_mode": "FIXED", "amount_minor": 250_000,
		}, nil)
		cancelID := toCancel["id"].(string)
		cancelCode := toCancel["code"].(string)

		if c, _ := e.c.do("DELETE", "/v1/payment-requests/"+cancelID, "mallory", nil, nil); c != 404 {
			t.Fatalf("non-owner cancel must return 404, got %d", c)
		}
		if c, body := e.c.do("DELETE", "/v1/payment-requests/"+cancelID, "ada", nil, nil); c != 200 || body["status"] != "CANCELLED" {
			t.Fatalf("owner cancel failed: %d %v", c, body)
		}
		audits, err := st.Repos().Audit().ForResource(t.Context(), "payment_request", uuid.MustParse(cancelID))
		if err != nil || len(audits) != 2 || audits[1].Action != "payment_request.cancelled" {
			t.Fatalf("expected created + cancelled audit events, got %+v %v", audits, err)
		}
		if c, body := e.c.do("POST", "/v1/public/pay/"+cancelCode+"/checkout", "", map[string]any{
			"email": "guest@example.com",
		}, map[string]string{"Idempotency-Key": "cancelled-link-001", "X-Forwarded-For": "198.51.100.1"}); c != 409 || body["error"].(map[string]any)["code"] != "LINK_CANCELLED" {
			t.Fatalf("expected 409 LINK_CANCELLED, got %d %v", c, body)
		}

		// Payer == Requester works and suppresses duplicate email notification.
		adaUID, _ := st.Repos().Users().EnsureByExternalID(t.Context(), "ada")
		ctEm, hmEm, maskEm, _ := notifications.SealUserEmail(e.box, adaUID, "ada@example.com")
		_ = st.Repos().Users().UpsertContacts(t.Context(), notifications.UserContacts{
			UserID: adaUID, EmailCiphertext: ctEm, EmailHMAC: hmEm, EmailMasked: maskEm, EmailKeyVersion: e.box.KeyVersion,
		})
		_, selfLink := e.c.do("POST", "/v1/payment-requests", "ada", map[string]any{
			"meter_id": meterID, "amount_mode": "FIXED", "amount_minor": 300_000,
		}, nil)
		cSelf, coSelf := e.c.do("POST", "/v1/public/pay/"+selfLink["code"].(string)+"/checkout", "", map[string]any{
			"email": "ada@example.com",
		}, map[string]string{"Idempotency-Key": "self-pay-checkout1", "X-Forwarded-For": "198.51.100.2"})
		if cSelf != 202 {
			t.Fatalf("payer==requester checkout failed: %d %v", cSelf, coSelf)
		}
		e.payAndWebhook(t, coSelf["transaction_id"].(string))
		_, _ = e.pub.PublishBatch(t.Context())
		_, _ = e.notifSvc.ProcessBatch(t.Context(), 50)
		// Ada gets payment.confirmed + token.available (2 total emails, not 3 — payer duplicate suppressed!).
		if got := len(e.mockEmail.Sent()); got != 2 {
			t.Fatalf("expected duplicate payer==requester email to be suppressed (2 total emails), got %d", got)
		}

		// Requester's meter grant revoked after link creation -> 409 LINK_UNUSABLE!
		_, revLink := e.c.do("POST", "/v1/payment-requests", "ada", map[string]any{
			"meter_id": meterID, "amount_mode": "FIXED", "amount_minor": 300_000,
		}, nil)
		if c, _ := e.c.do("DELETE", "/v1/meters/"+meterID, "ada", nil, nil); c != 204 {
			t.Fatalf("remove saved meter: %d", c)
		}
		if c, body := e.c.do("POST", "/v1/public/pay/"+revLink["code"].(string)+"/checkout", "", map[string]any{
			"email": "guest@example.com",
		}, map[string]string{"Idempotency-Key": "revoked-grant-0001", "X-Forwarded-For": "198.51.100.3"}); c != 409 || body["error"].(map[string]any)["code"] != "LINK_UNUSABLE" {
			t.Fatalf("expected 409 LINK_UNUSABLE after meter access removal, got %d %v", c, body)
		}

		// Uniform 404 + per-IP enumeration rate limit (4/min) + per-link checkout rate limit (2/min across distinct IPs).
		for i := 0; i < 4; i++ {
			c, body := e.c.do("GET", "/v1/public/pay/short", "", nil, map[string]string{"X-Forwarded-For": "203.0.113.99"})
			if c != 404 || body["error"].(map[string]any)["code"] != "NOT_FOUND" {
				t.Fatalf("probe %d: expected uniform 404 NOT_FOUND, got %d %v", i, c, body)
			}
		}
		if c, body := e.c.do("GET", "/v1/public/pay/short", "", nil, map[string]string{"X-Forwarded-For": "203.0.113.99"}); c != 429 || body["error"].(map[string]any)["code"] != "RATE_LIMITED" {
			t.Fatalf("expected 429 RATE_LIMITED on 5th probe from same IP, got %d %v", c, body)
		}

		// Re-register meter as "bob" and verify per-link checkout rate limit across distinct client IPs.
		_, m2 := e.c.do("POST", "/v1/meters", "bob", map[string]any{"meter_number": "45012345678", "disco_code": "IKEDC"}, nil)
		_, rlLink := e.c.do("POST", "/v1/payment-requests", "bob", map[string]any{
			"meter_id": m2["meter_id"], "amount_mode": "RANGE", "min_amount_minor": 200_000, "max_amount_minor": 500_000,
		}, nil)
		rlCode := rlLink["code"].(string)
		for i := 0; i < 2; i++ {
			_, _ = e.c.do("POST", "/v1/public/pay/"+rlCode+"/checkout", "", map[string]any{
				"amount_minor": 100_000, "email": "probe@example.com",
			}, map[string]string{"Idempotency-Key": "per-link-probe-" + uuid.NewString(), "X-Forwarded-For": "192.0.2." + string(rune('1'+i))})
		}
		if c, body := e.c.do("POST", "/v1/public/pay/"+rlCode+"/checkout", "", map[string]any{
			"amount_minor": 250_000, "email": "probe@example.com",
		}, map[string]string{"Idempotency-Key": "per-link-probe-" + uuid.NewString(), "X-Forwarded-For": "192.0.2.50"}); c != 429 || body["error"].(map[string]any)["code"] != "RATE_LIMITED" {
			t.Fatalf("expected 429 RATE_LIMITED from per-link limiter across distinct IPs, got %d %v", c, body)
		}

		// Feature flag disabled returns 404 FEATURE_DISABLED on both auth and public endpoints.
		off := newPayForMeEnv(t, st, httpapi.Limits{}, true)
		if c, body := off.c.do("GET", "/v1/payment-requests", "ada", nil, nil); c != 404 || body["error"].(map[string]any)["code"] != "FEATURE_DISABLED" {
			t.Fatalf("expected 404 FEATURE_DISABLED when flag is off, got %d %v", c, body)
		}
		if c, body := off.c.do("GET", "/v1/public/pay/"+rlCode, "", nil, nil); c != 404 || body["error"].(map[string]any)["code"] != "FEATURE_DISABLED" {
			t.Fatalf("expected 404 FEATURE_DISABLED on public endpoint when flag is off, got %d %v", c, body)
		}
	})
}
