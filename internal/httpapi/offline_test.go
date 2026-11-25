package httpapi_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/teamthy/meterly/internal/auth"
	"github.com/teamthy/meterly/internal/channels/ussd"
	"github.com/teamthy/meterly/internal/channels/ussd/ussdtest"
	"github.com/teamthy/meterly/internal/httpapi"
	"github.com/teamthy/meterly/internal/notifications"
	"github.com/teamthy/meterly/internal/notifications/africastalking"
	"github.com/teamthy/meterly/internal/notifications/africastalking/africastalkingtest"
	"github.com/teamthy/meterly/internal/notifications/whatsapp"
	"github.com/teamthy/meterly/internal/notifications/whatsapp/whatsapptest"
	"github.com/teamthy/meterly/internal/notificationservice"
	"github.com/teamthy/meterly/internal/offline"
	"github.com/teamthy/meterly/internal/orchestrator"
	"github.com/teamthy/meterly/internal/outbox"
	"github.com/teamthy/meterly/internal/providers/mock"
	"github.com/teamthy/meterly/internal/registry"
	"github.com/teamthy/meterly/internal/secure"
	"github.com/teamthy/meterly/internal/store"
	"github.com/teamthy/meterly/internal/store/pgstore"
)

var (
	otpDigitsRE       = regexp.MustCompile(`\b([0-9]{6})\b`)
	checkoutRefRE     = regexp.MustCompile(`https?://[^\s]+/([A-Za-z0-9_-]+)`)
	formattedTokenRE  = regexp.MustCompile(`\b[0-9]{4}(?:-[0-9]{4}){4}\b`)
	raw20DigitTokenRE = regexp.MustCompile(`\b[0-9]{20}\b`)
)

func TestOfflineUSSDAndWhatsAppEndToEnd(t *testing.T) {
	eachStore(t, func(t *testing.T, st store.Store) {
		ctx := context.Background()

		// Start controllable clock at 10:00 Africa/Lagos (09:00 UTC) tomorrow so:
		// 1) simulated time is >= real wall-clock time (satisfying IdempotencyRepo TTLs), and
		// 2) multi-hour steps stay within the same Africa/Lagos calendar day until explicitly advanced.
		tomorrowUTC := time.Now().UTC().Add(24 * time.Hour).Truncate(24 * time.Hour)
		var clockMu sync.Mutex
		curNow := tomorrowUTC.Add(9 * time.Hour) // 10:00 WAT
		nowFn := func() time.Time {
			clockMu.Lock()
			defer clockMu.Unlock()
			return curNow
		}
		advance := func(d time.Duration) time.Time {
			clockMu.Lock()
			defer clockMu.Unlock()
			curNow = curNow.Add(d)
			return curNow
		}

		var logBuf bytes.Buffer
		log := slog.New(slog.NewJSONHandler(&logBuf, nil))
		box, err := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "v1")
		if err != nil {
			t.Fatal(err)
		}

		idp := newTestOIDCProvider(t)
		oidcAuth, err := auth.NewOIDC(auth.OIDCConfig{
			Issuer:    idp.srv.URL,
			Audience:  "meterly-api",
			JWKSURL:   idp.srv.URL + "/jwks",
			MFAValues: []string{"mfa"},
			Log:       log,
		})
		if err != nil {
			t.Fatal(err)
		}

		pay, vend, remote := mock.NewPayments(), mock.NewVending(), mock.NewRemoteCredit()
		orch := orchestrator.New(orchestrator.DefaultConfig(), st, registry.New(st, box, vend, remote), box, pay, vend, remote, log)
		orch.Now = nowFn

		atFake := africastalkingtest.New("sandbox", "at_key")
		t.Cleanup(atFake.Close)
		waFake := whatsapptest.New("99887766", "wa_tok")
		waFake.SetClock(nowFn)
		t.Cleanup(waFake.Close)

		atClient, err := africastalking.New(africastalking.Config{
			Username: "sandbox", APIKey: "at_key", SenderID: "Meterly",
			WebhookSecret: "at_secret", BaseURL: atFake.URL,
		})
		if err != nil {
			t.Fatal(err)
		}
		waSecret := "wa_app_secret_m11"
		waVerifyTok := "wa_verify_tok_m11"
		waClient, err := whatsapp.New(whatsapp.Config{
			PhoneNumberID: "99887766", AccessToken: "wa_tok",
			AppSecret: waSecret, VerifyToken: waVerifyTok, BaseURL: waFake.URL,
		})
		if err != nil {
			t.Fatal(err)
		}
		orch.RegisterNotificationProviders(atClient.Name(), waClient.Name())

		notifSvc, err := notificationservice.New(notificationservice.Config{
			Store: st,
			Box:   box,
			Providers: map[notifications.Channel]notifications.Provider{
				notifications.SMS:      atClient,
				notifications.WhatsApp: waClient,
			},
			InboundVerifiers: map[string]notifications.InboundVerifier{
				"africastalking": atClient,
				"whatsapp":       waClient,
			},
			FeatureGiftSMS: true,
			Now:            nowFn,
			Log:            log,
		})
		if err != nil {
			t.Fatal(err)
		}
		pub := outbox.NewPublisher(st, outbox.Chain(outbox.LogSink(log), notifSvc.Sink()), log)

		ussdSecret := "ussd_shared_secret_m11"
		ussdAdp, err := ussd.New(ussd.Config{
			SharedSecret: ussdSecret,
			AllowedCIDRs: []string{"127.0.0.1/32", "196.201.214.0/24"},
		})
		if err != nil {
			t.Fatal(err)
		}

		offSvc, err := offline.New(offline.Config{
			Store:                   st,
			Orch:                    orch,
			Box:                     box,
			SMSProvider:             atClient,
			WhatsAppProvider:        waClient,
			USSDAdapter:             ussdAdp,
			WhatsAppParser:          waClient,
			MinAmountMinor:          100000,   // ₦1,000
			MaxAmountMinor:          50000000, // ₦500,000
			FeeMinor:                10000,    // ₦100
			UnverifiedDailyCapMinor: 2000000,  // ₦20,000
			VerifiedDailyCapMinor:   20000000, // ₦200,000
			ReverifyAfter:           30 * 24 * time.Hour,
			PhoneMsgsPerMinute:      120,
			Now:                     nowFn,
			Log:                     log,
		})
		if err != nil {
			t.Fatal(err)
		}

		// 1. Verify feature-flag gating when disabled (default state).
		disabledSrv := &httpapi.Server{
			O: orch, Auth: oidcAuth, Payments: pay, Notifications: notifSvc, Offline: offSvc,
			USSDDisabled: true, WhatsAppDisabled: true,
			Limits: httpapi.Limits{ClientIPHeader: "X-Forwarded-For", WebhooksPerMinute: 500, WritesPerMinute: 500},
			Log:    log, Dev: true, DevPay: pay, DevRemote: remote,
		}
		disabledTS := httptest.NewServer(disabledSrv.Routes())
		t.Cleanup(disabledTS.Close)
		dc := client{t: t, srv: disabledTS}

		sc, body := dc.do("POST", "/v1/channels/ussd/inbound", "", map[string]any{"sessionId": "s1", "phoneNumber": "+2348031112222"}, nil)
		if sc != http.StatusNotFound || body["error"].(map[string]any)["code"] != "FEATURE_DISABLED" {
			t.Fatalf("expected 404 FEATURE_DISABLED for disabled USSD, got %d %v", sc, body)
		}
		sc, _ = dc.do("GET", "/v1/channels/whatsapp/inbound?hub.mode=subscribe&hub.verify_token="+waVerifyTok+"&hub.challenge=123", "", nil, nil)
		if sc != http.StatusNotFound {
			t.Fatalf("expected 404 for disabled WhatsApp GET challenge, got %d", sc)
		}

		// Enable USSD + WhatsApp on active test server.
		apiSrv := &httpapi.Server{
			O: orch, Auth: oidcAuth, Payments: pay, Notifications: notifSvc, Offline: offSvc,
			USSDDisabled: false, WhatsAppDisabled: false,
			Limits: httpapi.Limits{ClientIPHeader: "X-Forwarded-For", WebhooksPerMinute: 500, WritesPerMinute: 500},
			Log:    log, Dev: true, DevPay: pay, DevRemote: remote,
		}
		handler := apiSrv.Routes()
		srv := httptest.NewServer(handler)
		t.Cleanup(srv.Close)
		c := client{t: t, srv: srv}

		drain := func() {
			t.Helper()
			if _, err := pub.PublishBatch(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := notifSvc.ProcessBatch(ctx, 50); err != nil {
				t.Fatal(err)
			}
		}

		completeRef := func(ref string) {
			t.Helper()
			if err := pay.Complete(ref, true, 0); err != nil {
				t.Fatal(err)
			}
			sc, _ := c.do("POST", "/v1/payments/webhooks/mockpay", "", map[string]any{"reference": ref},
				map[string]string{"X-Mock-Signature": "dev-signature"})
			if sc != http.StatusOK {
				t.Fatalf("mockpay webhook failed with %d", sc)
			}
			drain()
		}

		sendWA := func(msgID, fromPhone, text string, ts time.Time, openWindow bool) int {
			t.Helper()
			if openWindow {
				waFake.OpenCustomerServiceWindow(fromPhone, 24*time.Hour)
			}
			digits := strings.TrimPrefix(fromPhone, "+")
			payload := map[string]any{
				"object": "whatsapp_business_account",
				"entry": []any{
					map[string]any{
						"id": "waba-1",
						"changes": []any{
							map[string]any{
								"field": "messages",
								"value": map[string]any{
									"messages": []any{
										map[string]any{
											"from":      digits,
											"id":        msgID,
											"timestamp": strconv.FormatInt(ts.Unix(), 10),
											"type":      "text",
											"text":      map[string]any{"body": text},
										},
									},
								},
							},
						},
					},
				},
			}
			raw, _ := json.Marshal(payload)
			mac := hmac.New(sha256.New, []byte(waSecret))
			mac.Write(raw)
			req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/channels/whatsapp/inbound", bytes.NewReader(raw))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			_, _ = io.ReadAll(resp.Body)
			return resp.StatusCode
		}

		// 2. Webhook authentication checks: spoofed IP / invalid signatures -> 401.
		ussdPayload := []byte(url.Values{
			"sessionId":   {"spoof-1"},
			"serviceCode": {"*347*88#"},
			"phoneNumber": {"+2348031112222"},
			"text":        {""},
		}.Encode())
		// Spoofed IP outside allowlist with valid HMAC -> 401
		spoofIPReq, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/channels/ussd/inbound", bytes.NewReader(ussdPayload))
		spoofIPReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		spoofIPReq.Header.Set("X-Forwarded-For", "203.0.113.99")
		spoofIPReq.Header.Set("X-USSD-Signature", ussd.Sign(ussdSecret, ussdPayload))
		resp, err := http.DefaultClient.Do(spoofIPReq)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected 401 on non-allowlisted USSD IP, got %d", resp.StatusCode)
		}

		// Allowlisted IP with bad HMAC -> 401
		badSigReq, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/channels/ussd/inbound", bytes.NewReader(ussdPayload))
		badSigReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		badSigReq.Header.Set("X-Forwarded-For", "196.201.214.10")
		badSigReq.Header.Set("X-USSD-Signature", "sha256=deadbeef")
		resp, err = http.DefaultClient.Do(badSigReq)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected 401 on invalid USSD HMAC, got %d", resp.StatusCode)
		}

		// WhatsApp GET challenge verification (401 on bad token, 200 echoing challenge on valid token) + POST 401 on bad HMAC.
		badChResp, _ := http.Get(srv.URL + "/v1/channels/whatsapp/inbound?hub.mode=subscribe&hub.verify_token=wrong&hub.challenge=ch_ok")
		badChResp.Body.Close()
		if badChResp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected 401 on wrong WhatsApp verify_token, got %d", badChResp.StatusCode)
		}
		goodChResp, _ := http.Get(srv.URL + "/v1/channels/whatsapp/inbound?hub.mode=subscribe&hub.verify_token=" + waVerifyTok + "&hub.challenge=ch_ok_99")
		chBytes, _ := io.ReadAll(goodChResp.Body)
		goodChResp.Body.Close()
		if goodChResp.StatusCode != http.StatusOK || string(chBytes) != "ch_ok_99" {
			t.Fatalf("expected 200 ch_ok_99 on WhatsApp verify handshake, got %d %q", goodChResp.StatusCode, string(chBytes))
		}
		badWAReq, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/channels/whatsapp/inbound", strings.NewReader(`{"object":"whatsapp_business_account"}`))
		badWAReq.Header.Set("Content-Type", "application/json")
		badWAReq.Header.Set("X-Hub-Signature-256", "sha256=0000")
		badWAResp, _ := http.DefaultClient.Do(badWAReq)
		badWAResp.Body.Close()
		if badWAResp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected 401 on bad WhatsApp X-Hub-Signature-256, got %d", badWAResp.StatusCode)
		}

		// 3. OIDC-verified user auto-links phone + saves meter -> USSD saved-meter purchase -> SMS link -> payment -> token SMS.
		phoneAda := "+2348031112222"
		meterAda := "45022223333"
		adaTok := idp.token(t, "oidc-ada", map[string]any{
			"email":                 "ada@meterly.ng",
			"email_verified":        true,
			"phone_number":          phoneAda,
			"phone_number_verified": true,
			"name":                  "Ada",
		})
		sc, savedMeter := c.do("POST", "/v1/meters", adaTok, map[string]any{
			"meter_number": meterAda, "disco_code": "EKEDC", "label": "Home",
		}, nil)
		if sc != http.StatusCreated || savedMeter["meter_id"] == nil {
			t.Fatalf("save meter failed: %d %v", sc, savedMeter)
		}

		sim := &ussdtest.Simulator{
			Handler:     handler,
			Path:        "/v1/channels/ussd/inbound",
			Secret:      ussdSecret,
			RemoteAddr:  "127.0.0.1:50000",
			ServiceCode: "*347*88#",
		}

		// Dial USSD root menu -> every response validated <= 182 chars by ussdtest.Simulator.
		uSess, code, reply, err := sim.Dial("at-sess-ada-1", phoneAda)
		if err != nil || code != http.StatusOK || !strings.Contains(reply, "1. Buy for my meter") || !strings.Contains(reply, "2. Buy for saved meter") {
			t.Fatalf("USSD dial failed: code=%d reply=%q err=%v", code, reply, err)
		}
		// Pick "2. Buy for saved meter" -> shows masked saved meter -> pick "1" -> enter "5000" -> confirm "1"
		if _, reply, err = uSess.Input("2"); err != nil || !strings.Contains(reply, "223333") && !strings.Contains(reply, "3333") {
			t.Fatalf("expected masked saved meter list, got %q err=%v", reply, err)
		}
		if _, reply, err = uSess.Input("1"); err != nil || !strings.Contains(reply, "Enter amount") {
			t.Fatalf("expected amount prompt, got %q err=%v", reply, err)
		}
		if _, reply, err = uSess.Input("5000"); err != nil || !strings.Contains(reply, "1. Confirm") {
			t.Fatalf("expected confirm prompt, got %q err=%v", reply, err)
		}
		smsBeforeConfirm := len(atFake.Sent())
		if _, reply, err = uSess.Input("1"); err != nil || !strings.HasPrefix(reply, "END ") || !strings.Contains(reply, "Payment link sent by SMS") {
			t.Fatalf("expected END payment link confirmation, got %q err=%v", reply, err)
		}

		// Duplicate aggregator callback on confirm step must return identical END response and send no duplicate checkout SMS.
		dupCode, dupReply, err := uSess.ReplayLast()
		if err != nil || dupCode != http.StatusOK || dupReply != reply {
			t.Fatalf("duplicate USSD confirm callback mismatch: code=%d got=%q want=%q err=%v", dupCode, dupReply, reply, err)
		}
		smsAfterConfirm := atFake.Sent()
		if len(smsAfterConfirm) != smsBeforeConfirm+1 {
			t.Fatalf("expected exactly 1 checkout link SMS even after duplicate callback, got %d -> %d", smsBeforeConfirm, len(smsAfterConfirm))
		}
		checkoutSMS := smsAfterConfirm[len(smsAfterConfirm)-1].Message
		refMatch := checkoutRefRE.FindStringSubmatch(checkoutSMS)
		if len(refMatch) < 2 {
			t.Fatalf("could not extract payment reference from SMS %q", checkoutSMS)
		}

		// Complete payment -> outbox drains -> token delivered via SMS.
		completeRef(refMatch[1])
		smsAfterVend := atFake.Sent()
		var firstToken string
		for _, m := range smsAfterVend[len(smsAfterConfirm):] {
			if tok := formattedTokenRE.FindString(m.Message); tok != "" {
				firstToken = tok
			}
		}
		if firstToken == "" {
			t.Fatalf("expected formatted 20-digit STS token in newly sent SMS, got %+v", smsAfterVend[len(smsAfterConfirm):])
		}

		// 4. Mid-flow USSD timeout (~180s) creates ZERO transactions.
		adaUID, _ := st.Repos().Users().EnsureByExternalID(ctx, "oidc-ada")
		txBeforeTimeout, _ := st.Repos().Transactions().ListForCustomer(ctx, adaUID)
		timeoutSess, _, _, err := sim.Dial("at-sess-timeout", phoneAda)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = timeoutSess.Input("1"); err != nil {
			t.Fatal(err)
		}
		if _, reply, err = timeoutSess.Input("3000"); err != nil || !strings.Contains(reply, "1. Confirm") {
			t.Fatalf("expected confirm screen before timeout, got %q err=%v", reply, err)
		}
		// Advance > 180s TTL and attempt to confirm -> session timed out, NO transaction created!
		advance(185 * time.Second)
		if _, reply, err = timeoutSess.Input("1"); err != nil || !strings.HasPrefix(reply, "END Session timed out") {
			t.Fatalf("expected END Session timed out after 185s, got %q err=%v", reply, err)
		}
		txAfterTimeout, _ := st.Repos().Transactions().ListForCustomer(ctx, adaUID)
		if len(txAfterTimeout) != len(txBeforeTimeout) {
			t.Fatalf("expired USSD session must create zero transactions: before=%d after=%d", len(txBeforeTimeout), len(txAfterTimeout))
		}

		// 5. Scoped, rate-limited (<= 3/day), audited "Last token" resend + cross-caller isolation.
		for i := 1; i <= 3; i++ {
			smsBeforeResend := len(atFake.Sent())
			rtSess, _, _, err := sim.Dial(fmt.Sprintf("at-sess-resend-%d", i), phoneAda)
			if err != nil {
				t.Fatal(err)
			}
			_, reply, err = rtSess.Input("3")
			if err != nil || !strings.HasPrefix(reply, "END ") || !strings.Contains(reply, "Last token") {
				t.Fatalf("resend #%d failed: reply=%q err=%v", i, reply, err)
			}
			resentSMS := atFake.Sent()
			if len(resentSMS) != smsBeforeResend+1 || !strings.Contains(resentSMS[len(resentSMS)-1].Message, firstToken) {
				t.Fatalf("resend #%d SMS missing caller's token %s: %+v", i, firstToken, resentSMS[smsBeforeResend:])
			}
		}
		// 4th resend on same Africa/Lagos day must be denied.
		rt4, _, _, err := sim.Dial("at-sess-resend-4", phoneAda)
		if err != nil {
			t.Fatal(err)
		}
		if _, reply, err = rt4.Input("3"); err != nil || !strings.Contains(reply, "max 3 token resends/day") {
			t.Fatalf("expected 4th token resend to hit 3/day limit, got %q err=%v", reply, err)
		}

		// Different OIDC-verified caller who saves the SAME meter must NEVER receive Ada's token via "Last token".
		phoneOther := "+2348097778888"
		otherTok := idp.token(t, "oidc-other", map[string]any{
			"phone_number":          phoneOther,
			"phone_number_verified": true,
		})
		sc, _ = c.do("POST", "/v1/meters", otherTok, map[string]any{
			"meter_number": meterAda, "disco_code": "EKEDC", "label": "SharedFlat",
		}, nil)
		if sc != http.StatusCreated {
			t.Fatalf("other user save meter failed: %d", sc)
		}
		otherSess, _, _, err := sim.Dial("at-sess-other-lasttoken", phoneOther)
		if err != nil {
			t.Fatal(err)
		}
		if _, reply, err = otherSess.Input("3"); err != nil || strings.Contains(reply, firstToken) || !strings.Contains(reply, "No completed token purchase") {
			t.Fatalf("caller isolation violated on Last token: got %q err=%v", reply, err)
		}

		// 6. Unverified phone daily cap -> HTTP OTP link (/v1/channels/phone/link/{start,verify}) -> verified cap -> SIM-swap inactivity reverification.
		phoneBola := "+2348054445555"
		meterBola := "45033334444"
		bolaSess, _, reply, err := sim.Dial("at-sess-bola-unver", phoneBola)
		if err != nil || !strings.Contains(reply, "5. Verify phone") {
			t.Fatalf("expected unverified menu with option 5, got %q err=%v", reply, err)
		}
		// Select 1 (Buy for my meter) -> DisCo 1 (IKEDC) -> meter -> ₦25,000 (exceeds ₦20,000 unverified daily cap!)
		if _, _, err = bolaSess.Input("1"); err != nil {
			t.Fatal(err)
		}
		if _, _, err = bolaSess.Input("1"); err != nil {
			t.Fatal(err)
		}
		if _, reply, err = bolaSess.Input(meterBola); err != nil || !strings.Contains(reply, "Enter amount") {
			t.Fatalf("expected meter validation + amount prompt, got %q err=%v", reply, err)
		}
		if _, reply, err = bolaSess.Input("25000"); err != nil || !strings.Contains(reply, "Unverified cap") {
			t.Fatalf("expected unverified daily cap rejection on ₦25,000, got %q err=%v", reply, err)
		}
		// ₦4,000 fits inside unverified cap -> succeeds!
		if _, reply, err = bolaSess.Input("4000"); err != nil || !strings.Contains(reply, "1. Confirm") {
			t.Fatalf("expected confirm prompt for ₦4,000, got %q err=%v", reply, err)
		}
		if _, reply, err = bolaSess.Input("1"); err != nil || !strings.HasPrefix(reply, "END Payment link sent") {
			t.Fatalf("expected END payment link for Bola unverified purchase, got %q err=%v", reply, err)
		}

		// Link Bola's phone to her authenticated account via /v1/channels/phone/link/start + /verify.
		bolaTok := idp.token(t, "oidc-bola-no-phone-claim", map[string]any{
			"email":          "bola@meterly.ng",
			"email_verified": true,
		})
		smsIdx := len(atFake.Sent())
		sc, startResp := c.do("POST", "/v1/channels/phone/link/start", bolaTok, map[string]any{"phone": phoneBola}, nil)
		if sc != http.StatusOK || startResp["status"] != "otp_sent" {
			t.Fatalf("phone link start failed: %d %v", sc, startResp)
		}
		otpSMS := atFake.Sent()[smsIdx].Message
		otpMatch := otpDigitsRE.FindStringSubmatch(otpSMS)
		if len(otpMatch) < 2 {
			t.Fatalf("expected 6-digit OTP in SMS %q", otpSMS)
		}
		otpCode := otpMatch[1]

		// Wrong OTP returns 422 INVALID_OTP; right OTP returns 200 verified.
		sc, badVerify := c.do("POST", "/v1/channels/phone/link/verify", bolaTok, map[string]any{"phone": phoneBola, "code": "000000"}, nil)
		if sc != http.StatusUnprocessableEntity || badVerify["error"].(map[string]any)["code"] != "INVALID_OTP" {
			t.Fatalf("expected 422 INVALID_OTP on wrong OTP, got %d %v", sc, badVerify)
		}
		sc, okVerify := c.do("POST", "/v1/channels/phone/link/verify", bolaTok, map[string]any{"phone": phoneBola, "code": otpCode}, nil)
		if sc != http.StatusOK || okVerify["status"] != "verified" {
			t.Fatalf("expected 200 verified on valid OTP, got %d %v", sc, okVerify)
		}

		// Now verified Bola can initiate a ₦25,000 purchase (above ₦20k unverified cap, within ₦200k verified cap)!
		bolaVerSess, _, reply, err := sim.Dial("at-sess-bola-ver", phoneBola)
		if err != nil || strings.Contains(reply, "5. Verify phone") {
			t.Fatalf("verified phone menu should not prompt initial verify: %q err=%v", reply, err)
		}
		_, reply, _ = bolaVerSess.Input("1") // Bola's saved meter from earlier USSD validation is now linked!
		// If meter was saved to shadow user prior to link, Bola can either have saved meter or enter DisCo; check prompt:
		if strings.Contains(reply, "Select DisCo") {
			_, _, _ = bolaVerSess.Input("1")
			_, _, _ = bolaVerSess.Input(meterBola)
		}
		if _, reply, err = bolaVerSess.Input("25000"); err != nil || !strings.Contains(reply, "1. Confirm") {
			t.Fatalf("verified phone should allow ₦25,000 purchase, got %q err=%v", reply, err)
		}
		if _, reply, err = bolaVerSess.Input("2"); err != nil || !strings.Contains(reply, "Purchase cancelled") {
			t.Fatalf("expected cancel on choice 2, got %q err=%v", reply, err)
		}

		// SIM-swap defense: advance 31 days (> ReverifyAfter=30d) of channel inactivity -> phone requires OTP re-verification!
		advance(31 * 24 * time.Hour)
		bolaStaleSess, _, reply, err := sim.Dial("at-sess-bola-stale", phoneBola)
		if err != nil || !strings.Contains(reply, "5. Re-verify phone") {
			t.Fatalf("expected SIM-swap inactivity re-verify menu after 31 days, got %q err=%v", reply, err)
		}
		// Attempt ₦25,000 while stale -> capped back to unverified ₦20,000 limit!
		_, reply, _ = bolaStaleSess.Input("1")
		if strings.Contains(reply, "Select DisCo") {
			_, _, _ = bolaStaleSess.Input("1")
			_, _, _ = bolaStaleSess.Input(meterBola)
		}
		if _, reply, err = bolaStaleSess.Input("25000"); err != nil || !strings.Contains(reply, "Unverified cap") {
			t.Fatalf("stale phone must enforce unverified cap until reverified, got %q err=%v", reply, err)
		}

		// Re-verify directly inside USSD via option 5 -> SMS OTP -> verified cap restored!
		smsReverIdx := len(atFake.Sent())
		reverifySess, _, _, err := sim.Dial("at-sess-bola-reverify", phoneBola)
		if err != nil {
			t.Fatal(err)
		}
		if _, reply, err = reverifySess.Input("5"); err != nil || !strings.Contains(reply, "6-digit code") {
			t.Fatalf("expected OTP prompt on option 5, got %q err=%v", reply, err)
		}
		reverifyOTP := otpDigitsRE.FindStringSubmatch(atFake.Sent()[smsReverIdx].Message)[1]
		if _, reply, err = reverifySess.Input(reverifyOTP); err != nil || !strings.Contains(reply, "Phone verified") {
			t.Fatalf("expected Phone verified on USSD OTP, got %q err=%v", reply, err)
		}

		// 7. WhatsApp Cloud API E2E:
		// (a) Purchase inside 24h window -> token delivered as free-form "text" in chat;
		// (b) Duplicate wamid + out-of-order older message handled safely;
		// (c) Purchase completed >24h after last user message -> token delivered via approved "template";
		// (d) Inbound "STOP" integrates with M5 opt_outs.
		waBefore := len(waFake.Sent())
		t0 := nowFn()
		if sc := sendWA("wamid.buy.1", phoneAda, "buy", t0, true); sc != http.StatusOK {
			t.Fatalf("WhatsApp 'buy' failed: %d", sc)
		}
		// Duplicate delivery of exact same wamid must be ignored (no extra outbound message).
		waCountAfterBuy := len(waFake.Sent())
		if waCountAfterBuy != waBefore+1 || !strings.Contains(waFake.Sent()[waCountAfterBuy-1].TextBody, "Select meter") {
			t.Fatalf("expected masked saved meter prompt on WhatsApp 'buy', sent=%+v", waFake.Sent()[waBefore:])
		}
		_ = sendWA("wamid.buy.1", phoneAda, "buy", t0, false)
		if len(waFake.Sent()) != waCountAfterBuy {
			t.Fatalf("duplicate WhatsApp wamid must produce zero additional replies")
		}

		// Select meter "1" at t0+10s
		_ = sendWA("wamid.buy.2", phoneAda, "1", t0.Add(10*time.Second), true)
		waCountAfterMeter := len(waFake.Sent())
		// Out-of-order delayed message with older timestamp (t0+2s < t0+10s) must be ignored without corrupting StateAwaitAmount!
		_ = sendWA("wamid.stale.old", phoneAda, "cancel", t0.Add(2*time.Second), false)
		if len(waFake.Sent()) != waCountAfterMeter {
			t.Fatalf("out-of-order older WhatsApp message must be ignored")
		}

		// Enter amount "4000" at t0+20s -> confirm "1" at t0+30s -> checkout link sent in chat
		_ = sendWA("wamid.buy.3", phoneAda, "4000", t0.Add(20*time.Second), true)
		_ = sendWA("wamid.buy.4", phoneAda, "1", t0.Add(30*time.Second), true)
		waCheckoutMsg := waFake.Sent()[len(waFake.Sent())-1]
		if waCheckoutMsg.Type != "text" || !strings.Contains(waCheckoutMsg.TextBody, "Pay ") {
			t.Fatalf("expected WhatsApp checkout link text, got %+v", waCheckoutMsg)
		}
		waRef1 := checkoutRefRE.FindStringSubmatch(waCheckoutMsg.TextBody)[1]

		// Complete within 24h customer-service window -> token sent as free-form "text"!
		advance(2 * time.Minute)
		waBeforeVend1 := len(waFake.Sent())
		completeRef(waRef1)
		var foundInWindowText bool
		for _, m := range waFake.Sent()[waBeforeVend1:] {
			if m.Type == "text" && formattedTokenRE.MatchString(m.TextBody) {
				foundInWindowText = true
			}
		}
		if !foundInWindowText {
			t.Fatalf("expected free-form text token inside 24h WhatsApp window, got %+v", waFake.Sent()[waBeforeVend1:])
		}

		// Start a second WhatsApp purchase, then advance 25 hours (> 24h window) before payment webhook arrives -> must fall back to "template"!
		t1 := nowFn()
		_ = sendWA("wamid.late.1", phoneAda, "buy", t1, true)
		_ = sendWA("wamid.late.2", phoneAda, "1", t1.Add(5*time.Second), true)
		_ = sendWA("wamid.late.3", phoneAda, "3500", t1.Add(10*time.Second), true)
		_ = sendWA("wamid.late.4", phoneAda, "1", t1.Add(15*time.Second), true)
		waRef2 := checkoutRefRE.FindStringSubmatch(waFake.Sent()[len(waFake.Sent())-1].TextBody)[1]

		advance(25 * time.Hour) // 24h customer-service window is now CLOSED on waFake!
		waBeforeVend2 := len(waFake.Sent())
		completeRef(waRef2)
		var foundTemplateFallback bool
		for _, m := range waFake.Sent()[waBeforeVend2:] {
			if m.Type == "template" && m.TemplateName == "meterly_offline_token_v1" {
				foundTemplateFallback = true
			}
		}
		if !foundTemplateFallback {
			t.Fatalf("expected meterly_offline_token_v1 template outside 24h WhatsApp window, got %+v", waFake.Sent()[waBeforeVend2:])
		}

		// Inbound "STOP" records M5 opt-out in opt_outs table.
		t2 := nowFn()
		_ = sendWA("wamid.stop.1", phoneAda, "STOP", t2, true)
		optedOut, err := st.Repos().Notifications().IsOptedOut(ctx, notifications.DestinationHMAC(box, phoneAda), notifications.WhatsApp)
		if err != nil || !optedOut {
			t.Fatalf("expected WhatsApp STOP to persist opt-out: optedOut=%v err=%v", optedOut, err)
		}

		// 8. Verify audit trail and zero raw PII/token leakage in logs and DB tables.
		adaTxns, err := st.Repos().Transactions().ListForCustomer(ctx, adaUID)
		if err != nil || len(adaTxns) == 0 {
			t.Fatalf("expected Ada transactions: %v", err)
		}
		var resendAuditCount int
		for _, txn := range adaTxns {
			evs, aErr := st.Repos().Audit().ForResource(ctx, "transaction", txn.ID)
			if aErr != nil {
				t.Fatal(aErr)
			}
			for _, ev := range evs {
				if ev.Action == "offline.token.resent" {
					resendAuditCount++
				}
			}
		}
		if resendAuditCount != 3 {
			t.Fatalf("expected 3 audited offline.token.resent events for Ada, got %d", resendAuditCount)
		}

		logs := logBuf.String()
		for _, forbidden := range []string{
			meterAda, meterBola,
			phoneAda, strings.TrimPrefix(phoneAda, "+"),
			phoneBola, strings.TrimPrefix(phoneBola, "+"),
			phoneOther, strings.TrimPrefix(phoneOther, "+"),
			strings.ReplaceAll(firstToken, "-", ""),
			firstToken,
		} {
			if strings.Contains(logs, forbidden) {
				t.Fatalf("structured logs leaked sensitive value %q", forbidden)
			}
		}
		if raw20DigitTokenRE.MatchString(logs) {
			t.Fatalf("structured logs leaked raw 20-digit STS token")
		}

		if pgSt, ok := st.(*pgstore.Store); ok {
			var combinedDB strings.Builder
			for _, tbl := range []string{
				"phone_channel_links",
				"channel_otps",
				"conversation_sessions",
				"processed_channel_messages",
				"offline_purchases",
				"offline_token_resends",
			} {
				var dump string
				q := fmt.Sprintf(`SELECT coalesce(string_agg(row_to_json(t)::text, ' '), '') FROM %s t`, tbl)
				if err := pgSt.Pool().QueryRow(ctx, q).Scan(&dump); err != nil {
					t.Fatalf("scan %s: %v", tbl, err)
				}
				combinedDB.WriteString(dump)
				combinedDB.WriteByte(' ')
			}
			dbStr := combinedDB.String()
			for _, forbidden := range []string{
				phoneAda, strings.TrimPrefix(phoneAda, "+"),
				phoneBola, strings.TrimPrefix(phoneBola, "+"),
				phoneOther, strings.TrimPrefix(phoneOther, "+"),
				meterAda, meterBola,
				otpCode, reverifyOTP,
				firstToken, strings.ReplaceAll(firstToken, "-", ""),
			} {
				if strings.Contains(dbStr, forbidden) {
					t.Fatalf("offline PostgreSQL tables leaked raw secret/PII %q", forbidden)
				}
			}
		}
	})
}
