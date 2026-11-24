package httpapi_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/teamthy/meterly/internal/auth"
	"github.com/teamthy/meterly/internal/httpapi"
	"github.com/teamthy/meterly/internal/ledger"
	"github.com/teamthy/meterly/internal/money"
	"github.com/teamthy/meterly/internal/notifications"
	"github.com/teamthy/meterly/internal/orchestrator"
	"github.com/teamthy/meterly/internal/pots"
	"github.com/teamthy/meterly/internal/providers/paystack"
	"github.com/teamthy/meterly/internal/store"
)

func (e *payForMeEnv) payContribAndWebhook(t *testing.T, providerRef string) {
	t.Helper()
	e.fake.Pay(providerRef, 0)
	body, _ := json.Marshal(map[string]any{
		"event": "charge.success",
		"data":  map[string]any{"reference": providerRef, "status": "success"},
	})
	req, _ := http.NewRequest("POST", e.c.srv.URL+"/v1/payments/webhooks/paystack", bytes.NewReader(body))
	req.Header.Set("X-Paystack-Signature", paystack.Sign(psSecret, body))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("webhook status = %d, want 200", res.StatusCode)
	}
}

// End-to-end HTTP + Paystack + M5 notification consent/opt-out + privacy audit
// across both memstore and PostgreSQL.
func TestSplitMeterHTTPFlowConsentAndPrivacy(t *testing.T) {
	eachStore(t, func(t *testing.T, st store.Store) {
		e := newPayForMeEnv(t, st, httpapi.Limits{
			WritesPerMinute:     120,
			PublicPayPerMinute:  120,
			PublicLinkPerMinute: 60,
		}, false)

		// Pre-register a global STOP opt-out on +2348039998888 so we verify M5 opt-out enforcement.
		optedOutPhone := "+2348039998888"
		optedOutHMAC := notifications.DestinationHMAC(e.box, optedOutPhone)
		if _, err := e.st.Repos().Notifications().AddOptOut(t.Context(), notifications.OptOut{
			DestinationHMAC: optedOutHMAC,
			Channel:         notifications.SMS,
			CreatedAt:       e.now(),
		}); err != nil {
			t.Fatal(err)
		}

		// Register meter for organiser.
		mCode, meterBody := e.c.do("POST", "/v1/meters", "organiser-user", map[string]any{
			"meter_number": "45012345678",
			"disco_code":   "IKEDC",
			"label":        "Shared Flat",
		}, nil)
		if mCode != http.StatusCreated {
			t.Fatalf("register meter = %d %v", mCode, meterBody)
		}
		meterID := meterBody["meter_id"].(string)

		// Create a Split-the-Meter pot: ₦6,100 target (₦6,000 electricity + ₦100 fee), 3 shares.
		rawCreated, rawCreatedBody, _ := e.rawRequest("POST", "/v1/pots", "organiser-user", map[string]any{
			"meter_id":         meterID,
			"target_minor":     610000,
			"suggested_shares": 3,
			"expires_in_hours": 48,
			"note":             "Block B shared prepaid meter",
		}, nil)
		if rawCreated.StatusCode != http.StatusCreated {
			t.Fatalf("POST /v1/pots = %d: %s", rawCreated.StatusCode, rawCreatedBody)
		}
		if rawCreated.Header.Get("Cache-Control") != "no-store" || rawCreated.Header.Get("Referrer-Policy") != "no-referrer" {
			t.Fatalf("missing no-store/no-referrer headers on pot creation: %v", rawCreated.Header)
		}
		var created orchestrator.CreatedPot
		if err := json.Unmarshal([]byte(rawCreatedBody), &created); err != nil {
			t.Fatal(err)
		}
		if created.Code == "" || !strings.HasPrefix(created.ShareURL, "https://meterly.ng/pot/") {
			t.Fatalf("unexpected share URL / code: %+v", created)
		}
		qrBytes, err := base64.StdEncoding.DecodeString(created.QRPNGBase64)
		if err != nil {
			t.Fatalf("invalid qr base64: %v", err)
		}
		if _, err := png.Decode(bytes.NewReader(qrBytes)); err != nil {
			t.Fatalf("invalid qr PNG: %v", err)
		}

		// Contributor 1 (guest, opt_in_sms=true, consented phone +2348031112222) + idempotency replay & conflict check.
		idem1 := "idem-pot-http-contrib-0001"
		c1Payload := map[string]any{
			"amount_minor": 200000,
			"email":        "tunde.secret@example.com",
			"name":         "Tunde Okafor",
			"phone":        "+2348031112222",
			"opt_in_sms":   true,
		}
		r1, r1Body, _ := e.rawRequest("POST", "/v1/public/pots/"+created.Code+"/contribute", "", c1Payload, map[string]string{"Idempotency-Key": idem1})
		if r1.StatusCode != http.StatusAccepted {
			t.Fatalf("contribute 1 = %d: %s", r1.StatusCode, r1Body)
		}
		var res1 orchestrator.ContributePotResult
		_ = json.Unmarshal([]byte(r1Body), &res1)

		// Replay with identical payload -> same contribution ID & provider_ref!
		r1Replay, r1ReplayBody, _ := e.rawRequest("POST", "/v1/public/pots/"+created.Code+"/contribute", "", c1Payload, map[string]string{"Idempotency-Key": idem1})
		if r1Replay.StatusCode != http.StatusAccepted {
			t.Fatalf("contribute 1 replay = %d", r1Replay.StatusCode)
		}
		var res1Replay orchestrator.ContributePotResult
		_ = json.Unmarshal([]byte(r1ReplayBody), &res1Replay)
		if res1Replay.ContributionID != res1.ContributionID || res1Replay.ProviderRef != res1.ProviderRef {
			t.Fatalf("idempotency replay mismatch: %+v vs %+v", res1, res1Replay)
		}

		// Same Idempotency-Key with different amount -> 409 IDEMPOTENCY_CONFLICT!
		r1Conflict, _, _ := e.rawRequest("POST", "/v1/public/pots/"+created.Code+"/contribute", "", map[string]any{
			"amount_minor": 250000,
			"email":        "tunde.secret@example.com",
			"name":         "Tunde Okafor",
			"phone":        "+2348031112222",
			"opt_in_sms":   true,
		}, map[string]string{"Idempotency-Key": idem1})
		if r1Conflict.StatusCode != http.StatusConflict {
			t.Fatalf("want 409 IDEMPOTENCY_CONFLICT, got %d", r1Conflict.StatusCode)
		}

		e.payContribAndWebhook(t, res1.ProviderRef)
		e.advanceClock(time.Second)

		// Contributor 2 (authenticated user, opt_in_sms=false -> NO SMS).
		r2, r2Body, _ := e.rawRequest("POST", "/v1/public/pots/"+created.Code+"/contribute", "authed-roommate", map[string]any{
			"amount_minor": 200000,
			"email":        "amina.secret@example.com",
			"name":         "Amina Bello",
			"opt_in_sms":   false,
		}, map[string]string{"Idempotency-Key": "idem-pot-http-contrib-0002"})
		if r2.StatusCode != http.StatusAccepted {
			t.Fatalf("contribute 2 = %d: %s", r2.StatusCode, r2Body)
		}
		var res2 orchestrator.ContributePotResult
		_ = json.Unmarshal([]byte(r2Body), &res2)
		e.payContribAndWebhook(t, res2.ProviderRef)
		e.advanceClock(time.Second)

		// Contributor 3 (guest, opt_in_sms=true on opted-out phone +2348039998888, overfunds with ₦2,500 when ₦2,100 needed -> ₦400 excess refunded via Paystack!).
		r3, r3Body, _ := e.rawRequest("POST", "/v1/public/pots/"+created.Code+"/contribute", "", map[string]any{
			"amount_minor": 250000,
			"email":        "anon.secret@example.com",
			"name":         "",
			"phone":        optedOutPhone,
			"opt_in_sms":   true,
		}, map[string]string{"Idempotency-Key": "idem-pot-http-contrib-0003"})
		if r3.StatusCode != http.StatusAccepted {
			t.Fatalf("contribute 3 = %d: %s", r3.StatusCode, r3Body)
		}
		var res3 orchestrator.ContributePotResult
		_ = json.Unmarshal([]byte(r3Body), &res3)
		e.payContribAndWebhook(t, res3.ProviderRef)

		// Inspect public pot view: verify initials only and ZERO PII/token leaks!
		pubRaw, pubBodyStr, _ := e.rawRequest("GET", "/v1/public/pots/"+created.Code, "", nil, nil)
		if pubRaw.StatusCode != http.StatusOK {
			t.Fatalf("GET public pot = %d: %s", pubRaw.StatusCode, pubBodyStr)
		}
		for _, forbidden := range []string{
			"45012345678",
			"TEST CUSTOMER",
			"12 Test Street",
			"tunde.secret@example.com",
			"amina.secret@example.com",
			"anon.secret@example.com",
			"Tunde Okafor",
			"Amina Bello",
			"+2348031112222",
			optedOutPhone,
		} {
			if strings.Contains(pubBodyStr, forbidden) {
				t.Fatalf("public pot response leaked sensitive string %q: %s", forbidden, pubBodyStr)
			}
		}
		var pubView orchestrator.PublicPotView
		if err := json.Unmarshal([]byte(pubBodyStr), &pubView); err != nil {
			t.Fatal(err)
		}
		if pubView.Status != pots.StatusSettled || pubView.FundedMinor != 610000 || pubView.RemainingMinor != 0 {
			t.Fatalf("unexpected public pot view: %+v", pubView)
		}

		// Organiser inspects GET /v1/pots/{id}, retrieves transaction ID and reads STS token.
		ownerRaw, ownerBodyStr, _ := e.rawRequest("GET", "/v1/pots/"+created.ID.String(), "organiser-user", nil, nil)
		if ownerRaw.StatusCode != http.StatusOK {
			t.Fatalf("GET owner pot = %d: %s", ownerRaw.StatusCode, ownerBodyStr)
		}
		for _, forbidden := range []string{"tunde.secret@example.com", "+2348031112222", optedOutPhone} {
			if strings.Contains(ownerBodyStr, forbidden) {
				t.Fatalf("owner pot detail leaked contributor contact %q", forbidden)
			}
		}
		var detail orchestrator.PotDetailView
		_ = json.Unmarshal([]byte(ownerBodyStr), &detail)
		if detail.Status != pots.StatusSettled || detail.RefundedMinor != 40000 || len(detail.Allocations) != 1 {
			t.Fatalf("unexpected owner pot detail: %+v", detail)
		}
		txnID := detail.Allocations[0].TransactionID

		tokCode, tokRes := e.c.do("GET", fmt.Sprintf("/v1/electricity/transactions/%s/token", txnID), "organiser-user", nil, nil)
		if tokCode != http.StatusOK {
			t.Fatalf("organiser token read = %d %v", tokCode, tokRes)
		}
		rawToken := tokRes["token"].(string)
		if len(rawToken) < 20 {
			t.Fatalf("expected valid STS token for organiser, got %q", rawToken)
		}

		// Cancelling an already-settled pot must fail with 409 POT_NOT_CANCELLABLE.
		cancelRes, _, _ := e.rawRequest("POST", "/v1/pots/"+created.ID.String()+"/cancel", "organiser-user", nil, nil)
		if cancelRes.StatusCode != http.StatusConflict {
			t.Fatalf("cancel settled pot = %d, want 409", cancelRes.StatusCode)
		}

		// Publish outbox -> process M5 notifications and verify contributor SMS consent & STOP opt-out!
		if _, err := e.pub.PublishBatch(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, err := e.notifSvc.ProcessBatch(t.Context(), 50); err != nil {
			t.Fatal(err)
		}

		sentSMS := e.mockSMS.Sent()
		if len(sentSMS) != 1 {
			t.Fatalf("expected exactly 1 contributor SMS (only +2348031112222), got %d: %+v", len(sentSMS), sentSMS)
		}
		if sentSMS[0].Destination != "+2348031112222" {
			t.Fatalf("SMS sent to %q, want +2348031112222", sentSMS[0].Destination)
		}
		if strings.Contains(sentSMS[0].Body, rawToken) || strings.Contains(sentSMS[0].Body, "45012345678") {
			t.Fatalf("contributor SMS leaked raw STS token or full meter number: %q", sentSMS[0].Body)
		}

		// Verify logs never leaked raw pot code, contributor emails, phones or raw STS token.
		logStr := e.logs.String()
		for _, secret := range []string{
			created.Code,
			rawToken,
			"45012345678",
			"tunde.secret@example.com",
			"amina.secret@example.com",
			"+2348031112222",
			optedOutPhone,
		} {
			if strings.Contains(logStr, secret) {
				t.Fatalf("structured logs leaked %q", secret)
			}
		}

		// Verify ledger balances: TrialBalance == 0 and POT_FUNDS_HELD == 0!
		tb, err := e.st.Repos().Ledger().TrialBalance(t.Context())
		if err != nil || tb != 0 {
			t.Fatalf("trial balance = %d (%v), want 0", tb, err)
		}
		potHeld, err := e.st.Repos().Ledger().Balance(t.Context(), ledger.PotFundsHeld, money.NGN)
		if err != nil || potHeld != 0 {
			t.Fatalf("POT_FUNDS_HELD = %d (%v), want 0", potHeld, err)
		}
	})
}

func TestSplitMeterFeatureGateAndCancelHTTP(t *testing.T) {
	eachStore(t, func(t *testing.T, st store.Store) {
		e := newPayForMeEnv(t, st, httpapi.Limits{WritesPerMinute: 60, PublicPayPerMinute: 60, PublicLinkPerMinute: 60}, false)

		mCode, meterBody := e.c.do("POST", "/v1/meters", "org-cancel", map[string]any{
			"meter_number": "45012345678",
			"disco_code":   "IKEDC",
			"label":        "Flat",
		}, nil)
		if mCode != http.StatusCreated {
			t.Fatalf("register meter = %d %v", mCode, meterBody)
		}
		meterID := meterBody["meter_id"].(string)

		var created orchestrator.CreatedPot
		r, rBody, _ := e.rawRequest("POST", "/v1/pots", "org-cancel", map[string]any{
			"meter_id":         meterID,
			"target_minor":     510000,
			"expires_in_hours": 24,
		}, nil)
		if r.StatusCode != http.StatusCreated {
			t.Fatalf("create pot = %d: %s", r.StatusCode, rBody)
		}
		_ = json.Unmarshal([]byte(rBody), &created)

		// One contributor pays ₦2,000, then organiser cancels via POST /v1/pots/{id}/cancel.
		_, rcBody, _ := e.rawRequest("POST", "/v1/public/pots/"+created.Code+"/contribute", "", map[string]any{
			"amount_minor": 200000,
			"email":        "refundme@example.com",
			"name":         "Kemi Lawal",
		}, map[string]string{"Idempotency-Key": "idem-cancel-http-0001"})
		var cres orchestrator.ContributePotResult
		_ = json.Unmarshal([]byte(rcBody), &cres)
		e.payContribAndWebhook(t, cres.ProviderRef)

		// Stranger cannot view or cancel organiser's pot (uniform 404).
		if stranger, _, _ := e.rawRequest("GET", "/v1/pots/"+created.ID.String(), "stranger", nil, nil); stranger.StatusCode != http.StatusNotFound {
			t.Fatalf("stranger GET = %d, want 404", stranger.StatusCode)
		}
		if strangerCancel, _, _ := e.rawRequest("POST", "/v1/pots/"+created.ID.String()+"/cancel", "stranger", nil, nil); strangerCancel.StatusCode != http.StatusNotFound {
			t.Fatalf("stranger cancel = %d, want 404", strangerCancel.StatusCode)
		}

		cancelRaw, cancelBody, _ := e.rawRequest("POST", "/v1/pots/"+created.ID.String()+"/cancel", "org-cancel", nil, nil)
		if cancelRaw.StatusCode != http.StatusOK {
			t.Fatalf("owner cancel = %d: %s", cancelRaw.StatusCode, cancelBody)
		}
		var cancelled orchestrator.PotDetailView
		_ = json.Unmarshal([]byte(cancelBody), &cancelled)
		if cancelled.Status != pots.StatusRefunded || cancelled.RefundedMinor != 200000 {
			t.Fatalf("expected REFUNDED with 200000 refunded, got %+v", cancelled)
		}

		// Verify SplitMeterDisabled=true returns 404 FEATURE_DISABLED across all 5 endpoints.
		disabledSrv := httptest.NewServer((&httpapi.Server{
			O:                  e.o,
			Auth:               auth.DevAuth{},
			SplitMeterDisabled: true,
		}).Routes())
		defer disabledSrv.Close()
		dc := client{t, disabledSrv}

		for _, tc := range []struct {
			method, path, user string
			body               any
		}{
			{"POST", "/v1/pots", "org-cancel", map[string]any{"meter_id": meterID, "target_minor": 200000}},
			{"GET", "/v1/pots/" + created.ID.String(), "org-cancel", nil},
			{"POST", "/v1/pots/" + created.ID.String() + "/cancel", "org-cancel", nil},
			{"GET", "/v1/public/pots/" + created.Code, "", nil},
			{"POST", "/v1/public/pots/" + created.Code + "/contribute", "", map[string]any{"amount_minor": 100000, "email": "a@example.com"}},
		} {
			code, resp := dc.do(tc.method, tc.path, tc.user, tc.body, map[string]string{"Idempotency-Key": "idem-disabled-000001"})
			errObj, _ := resp["error"].(map[string]any)
			if code != http.StatusNotFound || errObj["code"] != "FEATURE_DISABLED" {
				t.Fatalf("%s %s when disabled = %d %v, want 404 FEATURE_DISABLED", tc.method, tc.path, code, resp)
			}
		}
	})
}
