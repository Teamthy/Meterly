package httpapi_test

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/auth"
	"github.com/teamthy/meterly/internal/autopower"
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

func newAutoPowerHTTPEnv(t *testing.T, st store.Store, disabled bool) (*payForMeEnv, *mock.MeterReadings) {
	t.Helper()
	loc := autopower.LagosLocation()
	e := &payForMeEnv{
		st:      st,
		logs:    &syncBuf{},
		metrics: observability.NewRegistry(),
		clock:   time.Date(2027, 3, 10, 9, 0, 0, 0, loc).UTC(),
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

	vend := mock.NewVending()
	readings := mock.NewMeterReadings()
	reg := registry.New(st, box, vend, nil)
	reg.Now = e.now

	e.o = orchestrator.New(orchestrator.DefaultConfig(), st, reg, box, ps, vend, nil, log)
	e.o.Now = e.now
	e.o.SetMetrics(e.metrics)
	e.o.SetMeterReadingProvider(readings)

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
		O:                 e.o,
		Auth:              auth.DevAuth{},
		Payments:          ps,
		Notifications:     ns,
		Metrics:           e.metrics,
		Log:               log,
		Limits:            httpapi.Limits{WritesPerMinute: 120},
		AutoPowerDisabled: disabled,
		PublicBaseURL:     "https://meterly.ng",
		Dev:               true,
		DevReadings:       readings,
	}
	srv := httptest.NewServer(srvCfg.Routes())
	t.Cleanup(srv.Close)
	e.c = client{t, srv}
	return e, readings
}

// Verifies FEATURE_AUTOPOWER=false (default) gates all 8 AutoPower & saved
// payment-method routes with 404 FEATURE_DISABLED.
func TestAutoPowerFeatureGateDisabledByDefault(t *testing.T) {
	eachStore(t, func(t *testing.T, st store.Store) {
		e, _ := newAutoPowerHTTPEnv(t, st, true)
		id := uuid.NewString()

		cases := []struct {
			method string
			path   string
			body   map[string]any
		}{
			{"GET", "/v1/me/payment-methods", nil},
			{"DELETE", "/v1/me/payment-methods/" + id, nil},
			{"POST", "/v1/me/payment-methods/" + id + "/save", map[string]any{"consent": true}},
			{"POST", "/v1/autopower/rules", map[string]any{"meter_id": id, "payment_method_id": id, "amount_minor": 500000, "monthly_cap_minor": 2000000}},
			{"GET", "/v1/autopower/rules", nil},
			{"PATCH", "/v1/autopower/rules/" + id, map[string]any{"monthly_cap_minor": 3000000}},
			{"DELETE", "/v1/autopower/rules/" + id, nil},
			{"POST", "/v1/autopower/runs/" + id + "/skip", nil},
		}
		for _, tc := range cases {
			code, resp := e.c.do(tc.method, tc.path, "amina-user", tc.body, nil)
			if code != http.StatusNotFound {
				t.Fatalf("%s %s when disabled: status=%d body=%v, want 404", tc.method, tc.path, code, resp)
			}
			errMap, _ := resp["error"].(map[string]any)
			if errMap["code"] != "FEATURE_DISABLED" {
				t.Fatalf("%s %s error.code=%v, want FEATURE_DISABLED", tc.method, tc.path, errMap["code"])
			}
		}
	})
}

// End-to-end HTTP + Paystack + M5 heads-up notification + skip + scheduled charge
// + rule update + payment-method deletion + privacy check across memstore & PostgreSQL.
func TestAutoPowerHTTPLifecycleNotificationsAndPrivacy(t *testing.T) {
	eachStore(t, func(t *testing.T, st store.Store) {
		e, _ := newAutoPowerHTTPEnv(t, st, false)
		loc := autopower.LagosLocation()

		// 1. Register meter & seed verified user phone + email for M5 notifications.
		mCode, mBody := e.c.do("POST", "/v1/meters", "amina-user", map[string]any{
			"meter_number": "45088776655",
			"disco_code":   "IKEDC",
			"label":        "Home",
		}, nil)
		if mCode != http.StatusCreated {
			t.Fatalf("register meter = %d %v", mCode, mBody)
		}
		meterID := mBody["meter_id"].(string)

		uid, err := e.st.Repos().Users().EnsureByExternalID(t.Context(), "amina-user")
		if err != nil {
			t.Fatal(err)
		}
		phoneCT, phoneHMAC, phoneMasked, _ := notifications.SealUserPhone(e.box, uid, "+2348031234567")
		emailCT, emailHMAC, emailMasked, _ := notifications.SealUserEmail(e.box, uid, "amina@example.com")
		if err := e.st.Repos().Users().UpsertContacts(t.Context(), notifications.UserContacts{
			UserID:          uid,
			DisplayName:     "Amina",
			PhoneCiphertext: phoneCT,
			PhoneHMAC:       phoneHMAC,
			PhoneMasked:     phoneMasked,
			PhoneKeyVersion: e.box.KeyVersion,
			EmailCiphertext: emailCT,
			EmailHMAC:       emailHMAC,
			EmailMasked:     emailMasked,
			EmailKeyVersion: e.box.KeyVersion,
		}); err != nil {
			t.Fatal(err)
		}

		// 2. Initial card purchase via Paystack fake.
		qCode, qBody := e.c.do("POST", "/v1/electricity/quotes", "amina-user", map[string]any{
			"meter_id":     meterID,
			"amount_minor": 500000,
			"currency":     "NGN",
		}, nil)
		if qCode != http.StatusCreated {
			t.Fatalf("quote = %d %v", qCode, qBody)
		}
		txCode, txBody := e.c.do("POST", "/v1/electricity/transactions", "amina-user", map[string]any{
			"quote_id": qBody["quote_id"],
			"email":    "amina@example.com",
		}, map[string]string{"Idempotency-Key": "idem-autopower-seed-txn-001"})
		if txCode != http.StatusAccepted {
			t.Fatalf("create txn = %d %v", txCode, txBody)
		}
		seedTxnID := txBody["transaction_id"].(string)
		e.payAndWebhook(t, seedTxnID)

		// Drain initial purchase notifications so we isolate AutoPower notifications.
		_, _ = e.pub.PublishBatch(t.Context())
		_, _ = e.notifSvc.ProcessBatch(t.Context(), 50)
		smsBefore := len(e.mockSMS.Sent())

		// 3. Consent=false -> 400 CONSENT_REQUIRED.
		badCode, badBody := e.c.do("POST", "/v1/me/payment-methods/"+seedTxnID+"/save", "amina-user", map[string]any{
			"consent": false,
		}, nil)
		if badCode != http.StatusBadRequest || badBody["error"].(map[string]any)["code"] != "CONSENT_REQUIRED" {
			t.Fatalf("expected 400 CONSENT_REQUIRED, got %d %v", badCode, badBody)
		}

		// 4. Consent=true -> 201 SavedPaymentMethod (never leaking raw authorization_code).
		rawRes, rawBody, _ := e.rawRequest("POST", "/v1/me/payment-methods/"+seedTxnID+"/save", "amina-user", map[string]any{
			"consent": true,
		}, nil)
		if rawRes.StatusCode != http.StatusCreated {
			t.Fatalf("save payment method = %d %s", rawRes.StatusCode, rawBody)
		}
		if rawRes.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("expected Cache-Control: no-store, got %q", rawRes.Header.Get("Cache-Control"))
		}
		if strings.Contains(rawBody, "AUTH_fake_") || strings.Contains(rawBody, "SIG_fake_") {
			t.Fatalf("response leaked sensitive card authorization code/signature: %s", rawBody)
		}
		var savedPM autopower.SavedPaymentMethod
		if err := json.Unmarshal([]byte(rawBody), &savedPM); err != nil {
			t.Fatal(err)
		}

		// List payment methods -> 1 item.
		listPMCode, listPMBody := e.c.do("GET", "/v1/me/payment-methods", "amina-user", nil, nil)
		if listPMCode != http.StatusOK || len(listPMBody["payment_methods"].([]any)) != 1 {
			t.Fatalf("GET /v1/me/payment-methods = %d %v", listPMCode, listPMBody)
		}

		// 5. Low-balance rule on ineligible meter -> 422 METER_NOT_ELIGIBLE
		// (exercise on separate user so amina-user stays under the 10-request write burst).
		_, m2Body := e.c.do("POST", "/v1/meters", "tunde-user", map[string]any{
			"meter_number": "45088776655",
			"disco_code":   "IKEDC",
			"label":        "Home",
		}, nil)
		q2Code, q2Body := e.c.do("POST", "/v1/electricity/quotes", "tunde-user", map[string]any{
			"meter_id":     m2Body["meter_id"],
			"amount_minor": 500000,
			"currency":     "NGN",
		}, nil)
		if q2Code != http.StatusCreated {
			t.Fatalf("tunde quote = %d %v", q2Code, q2Body)
		}
		_, tx2Body := e.c.do("POST", "/v1/electricity/transactions", "tunde-user", map[string]any{
			"quote_id": q2Body["quote_id"],
			"email":    "tunde@example.com",
		}, map[string]string{"Idempotency-Key": "idem-tunde-seed-txn-0001"})
		tundeTxnID := tx2Body["transaction_id"].(string)
		e.payAndWebhook(t, tundeTxnID)
		_, tundePM := e.c.do("POST", "/v1/me/payment-methods/"+tundeTxnID+"/save", "tunde-user", map[string]any{
			"consent": true,
		}, nil)
		lbCode, lbBody := e.c.do("POST", "/v1/autopower/rules", "tunde-user", map[string]any{
			"meter_id":                  m2Body["meter_id"],
			"payment_method_id":         tundePM["payment_method_id"],
			"trigger_type":              "LOW_BALANCE",
			"amount_minor":              500000,
			"monthly_cap_minor":         2000000,
			"low_balance_threshold_kwh": 20.0,
		}, nil)
		if lbCode != http.StatusUnprocessableEntity || lbBody["error"].(map[string]any)["code"] != "METER_NOT_ELIGIBLE" {
			t.Fatalf("expected 422 METER_NOT_ELIGIBLE, got %d %v", lbCode, lbBody)
		}

		// 6. Create monthly schedule rule: 15th & 25th at 07:00 Africa/Lagos, ₦10,000, cap ₦40,000.
		rCode, rBody := e.c.do("POST", "/v1/autopower/rules", "amina-user", map[string]any{
			"meter_id":          meterID,
			"payment_method_id": savedPM.ID.String(),
			"trigger_type":      "SCHEDULE",
			"amount_minor":      1000000,
			"monthly_cap_minor": 4000000,
			"frequency":         "MONTHLY",
			"days_of_month":     []int{15, 25},
			"time_of_day":       "07:00",
		}, nil)
		if rCode != http.StatusCreated {
			t.Fatalf("create rule = %d %v", rCode, rBody)
		}
		ruleID := rBody["rule_id"].(string)
		upcomingMap := rBody["upcoming_run"].(map[string]any)
		mar15RunID := upcomingMap["run_id"].(string)

		// 7. Advance clock to Mar 14 20:00 WAT (11h before Mar 15 07:00 WAT) -> 12h heads-up SMS sent!
		e.clockMu.Lock()
		e.clock = time.Date(2027, 3, 14, 20, 0, 0, 0, loc).UTC()
		e.clockMu.Unlock()

		if _, err := e.o.TickAutoPower(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, err := e.pub.PublishBatch(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, err := e.notifSvc.ProcessBatch(t.Context(), 50); err != nil {
			t.Fatal(err)
		}
		smsMsgs := e.mockSMS.Sent()
		if len(smsMsgs) != smsBefore+1 {
			t.Fatalf("expected 1 heads-up SMS, got %d new messages", len(smsMsgs)-smsBefore)
		}
		headsUpBody := smsMsgs[len(smsMsgs)-1].Body
		if !strings.Contains(headsUpBody, "₦10,000") || !strings.Contains(headsUpBody, "15 Mar 07:00 WAT") {
			t.Fatalf("unexpected heads-up SMS body: %q", headsUpBody)
		}

		// 8. One-tap skip on Mar 15 run -> 200 SKIPPED_USER, upcoming_run advances to Mar 25!
		skCode, skBody := e.c.do("POST", "/v1/autopower/runs/"+mar15RunID+"/skip", "amina-user", nil, nil)
		if skCode != http.StatusOK || skBody["status"] != "SKIPPED_USER" {
			t.Fatalf("skip run = %d %v", skCode, skBody)
		}

		// Mar 15 07:01 WAT arrives -> skipped run does NOT charge!
		e.clockMu.Lock()
		e.clock = time.Date(2027, 3, 15, 7, 1, 0, 0, loc).UTC()
		e.clockMu.Unlock()
		_, _ = e.o.TickAutoPower(t.Context())
		if got := e.fake.Requests["POST /transaction/charge_authorization"]; got != 0 {
			t.Fatalf("skipped run must not call charge_authorization, got %d", got)
		}

		// 9. Advance to Mar 25 07:01 WAT -> Mar 25 run charges saved card on Paystack & vends!
		e.clockMu.Lock()
		e.clock = time.Date(2027, 3, 25, 7, 1, 0, 0, loc).UTC()
		e.clockMu.Unlock()
		if _, err := e.o.TickAutoPower(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got := e.fake.Requests["POST /transaction/charge_authorization"]; got != 1 {
			t.Fatalf("expected 1 charge_authorization on Mar 25, got %d", got)
		}

		// 10. PATCH /v1/autopower/rules/{id} updates monthly_cap_minor.
		pCode, pBody := e.c.do("PATCH", "/v1/autopower/rules/"+ruleID, "amina-user", map[string]any{
			"monthly_cap_minor": 3000000,
		}, nil)
		if pCode != http.StatusOK || int64(pBody["monthly_cap_minor"].(float64)) != 3000000 {
			t.Fatalf("PATCH rule = %d %v", pCode, pBody)
		}

		// 11. DELETE /v1/me/payment-methods/{id} -> 204 and pauses rule!
		delPMRes, _, _ := e.rawRequest("DELETE", "/v1/me/payment-methods/"+savedPM.ID.String(), "amina-user", nil, nil)
		if delPMRes.StatusCode != http.StatusNoContent {
			t.Fatalf("DELETE payment method = %d, want 204", delPMRes.StatusCode)
		}
		lsCode, lsBody := e.c.do("GET", "/v1/autopower/rules", "amina-user", nil, nil)
		if lsCode != http.StatusOK {
			t.Fatalf("GET rules = %d %v", lsCode, lsBody)
		}
		rulesArr := lsBody["rules"].([]any)
		if len(rulesArr) != 1 || rulesArr[0].(map[string]any)["status"] != "PAUSED" {
			t.Fatalf("expected rule PAUSED after deleting payment method, got %v", rulesArr)
		}

		// 12. DELETE /v1/autopower/rules/{id} -> 204 and rule removed from active/paused list.
		delRuleRes, _, _ := e.rawRequest("DELETE", "/v1/autopower/rules/"+ruleID, "amina-user", nil, nil)
		if delRuleRes.StatusCode != http.StatusNoContent {
			t.Fatalf("DELETE rule = %d, want 204", delRuleRes.StatusCode)
		}
		lsCode2, lsBody2 := e.c.do("GET", "/v1/autopower/rules", "amina-user", nil, nil)
		if lsCode2 != http.StatusOK || len(lsBody2["rules"].([]any)) != 0 {
			t.Fatalf("expected 0 rules after DELETE, got %d %v", lsCode2, lsBody2)
		}

		// 13. Privacy check on structured logs: never log raw authorization_code, meter number, phone or email.
		logOutput := e.logs.String()
		for _, secret := range []string{"AUTH_fake_", "45088776655", "+2348031234567", "amina@example.com"} {
			if strings.Contains(logOutput, secret) {
				t.Fatalf("structured logs leaked sensitive value %q:\n%s", secret, logOutput)
			}
		}
	})
}
