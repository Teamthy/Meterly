package httpapi_test

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/auth"
	"github.com/teamthy/meterly/internal/httpapi"
	"github.com/teamthy/meterly/internal/notifications"
	"github.com/teamthy/meterly/internal/notifications/africastalking"
	"github.com/teamthy/meterly/internal/notifications/africastalking/africastalkingtest"
	"github.com/teamthy/meterly/internal/notifications/resend"
	"github.com/teamthy/meterly/internal/notifications/resend/resendtest"
	"github.com/teamthy/meterly/internal/notifications/whatsapp"
	"github.com/teamthy/meterly/internal/notifications/whatsapp/whatsapptest"
	"github.com/teamthy/meterly/internal/notificationservice"
	"github.com/teamthy/meterly/internal/orchestrator"
	"github.com/teamthy/meterly/internal/outbox"
	"github.com/teamthy/meterly/internal/providers/mock"
	"github.com/teamthy/meterly/internal/registry"
	"github.com/teamthy/meterly/internal/secure"
	"github.com/teamthy/meterly/internal/store"
)

type testOIDCProvider struct {
	srv *httptest.Server
	key *rsa.PrivateKey
}

func newTestOIDCProvider(t *testing.T) *testOIDCProvider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &testOIDCProvider{key: key}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{{
				"kty": "RSA",
				"kid": "k1",
				"alg": "RS256",
				"use": "sig",
				"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
				"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
			}},
		})
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *testOIDCProvider) token(t *testing.T, sub string, extra map[string]any) string {
	t.Helper()
	now := time.Now().UTC()
	claims := map[string]any{
		"iss": p.srv.URL,
		"aud": "meterly-api",
		"sub": sub,
		"iat": now.Unix(),
		"exp": now.Add(15 * time.Minute).Unix(),
	}
	for k, v := range extra {
		claims[k] = v
	}
	hdrJSON, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "k1", "typ": "JWT"})
	claimsJSON, _ := json.Marshal(claims)
	signingInput := base64.RawURLEncoding.EncodeToString(hdrJSON) + "." + base64.RawURLEncoding.EncodeToString(claimsJSON)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, p.key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func TestNotificationsHTTPEndToEnd(t *testing.T) {
	eachStore(t, func(t *testing.T, st store.Store) {
		ctx := context.Background()
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

		atFake := africastalkingtest.New("sandbox", "at_key")
		t.Cleanup(atFake.Close)
		reFake := resendtest.New("re_key")
		t.Cleanup(reFake.Close)
		waFake := whatsapptest.New("99887766", "wa_tok")
		t.Cleanup(waFake.Close)

		reSecret := "whsec_" + base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
		atClient, _ := africastalking.New(africastalking.Config{
			Username: "sandbox", APIKey: "at_key", SenderID: "Meterly",
			WebhookSecret: "at_secret", BaseURL: atFake.URL,
		})
		reClient, _ := resend.New(resend.Config{
			APIKey: "re_key", From: "Meterly <noreply@meterly.ng>",
			WebhookSecret: reSecret, BaseURL: reFake.URL,
		})
		waClient, _ := whatsapp.New(whatsapp.Config{
			PhoneNumberID: "99887766", AccessToken: "wa_tok",
			AppSecret: "wa_secret", VerifyToken: "wa_challenge_tok", BaseURL: waFake.URL,
		})
		orch.RegisterNotificationProviders(atClient.Name(), reClient.Name(), waClient.Name())

		notifSvc, err := notificationservice.New(notificationservice.Config{
			Store: st,
			Box:   box,
			Providers: map[notifications.Channel]notifications.Provider{
				notifications.SMS:      atClient,
				notifications.Email:    reClient,
				notifications.WhatsApp: waClient,
			},
			InboundVerifiers: map[string]notifications.InboundVerifier{
				"africastalking": atClient,
				"resend":         reClient,
				"whatsapp":       waClient,
			},
			FeatureGiftSMS: true,
			Log:            log,
		})
		if err != nil {
			t.Fatal(err)
		}
		pub := outbox.NewPublisher(st, outbox.Chain(outbox.LogSink(log), notifSvc.Sink()), log)

		apiSrv := &httpapi.Server{
			O: orch, Auth: oidcAuth, Payments: pay, Notifications: notifSvc,
			Log: log, Dev: true, DevPay: pay, DevRemote: remote,
		}
		srv := httptest.NewServer(apiSrv.Routes())
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

		// 1. OIDC user with unverified contacts: never stored, never notified.
		unverTok := idp.token(t, "user-unverified", map[string]any{
			"email":                 "unverified@example.com",
			"email_verified":        false,
			"phone_number":          "+2348010001111",
			"phone_number_verified": false,
		})
		_, mUnver := c.do("POST", "/v1/meters", unverTok, map[string]any{"meter_number": "45011112222", "disco_code": "EKEDC"}, nil)
		_, qUnver := c.do("POST", "/v1/electricity/quotes", unverTok, map[string]any{
			"meter_id": mUnver["meter_id"], "amount_minor": 500000, "currency": "NGN", "delivery_preference": "TOKEN",
		}, nil)
		_, tUnver := c.do("POST", "/v1/electricity/transactions", unverTok, map[string]any{"quote_id": qUnver["quote_id"], "email": "checkout@example.com"},
			map[string]string{"Idempotency-Key": "oidc-unver-01234567"})
		refUnver := tUnver["checkout_url"].(string)[strings.LastIndex(tUnver["checkout_url"].(string), "/")+1:]
		_ = pay.Complete(refUnver, true, 0)
		c.do("POST", "/v1/payments/webhooks/mockpay", "", map[string]any{"reference": refUnver}, map[string]string{"X-Mock-Signature": "dev-signature"})
		drain()
		if len(atFake.Sent()) != 0 || len(reFake.Sent()) != 0 || len(waFake.Sent()) != 0 {
			t.Fatalf("unverified OIDC contacts must never receive notifications: sms=%d email=%d wa=%d",
				len(atFake.Sent()), len(reFake.Sent()), len(waFake.Sent()))
		}

		// 2. OIDC user with verified contacts + preferences API (GET default, PUT WhatsApp opt-in).
		verTok := idp.token(t, "user-verified", map[string]any{
			"email":                 "ada@example.com",
			"email_verified":        true,
			"phone_number":          "+2348022223333",
			"phone_number_verified": true,
			"name":                  "Ada",
		})
		code, prefs := c.do("GET", "/v1/me/notification-preferences", verTok, nil, nil)
		if code != 200 || prefs["sms"] != true || prefs["email"] != true || prefs["whatsapp"] != false {
			t.Fatalf("default GET preferences: %d %v", code, prefs)
		}
		if code, _ := c.do("PUT", "/v1/me/notification-preferences", verTok, map[string]any{"sms": true}, nil); code != 400 {
			t.Fatalf("partial PUT preferences must return 400, got %d", code)
		}
		code, prefs = c.do("PUT", "/v1/me/notification-preferences", verTok, map[string]any{
			"sms": true, "email": true, "whatsapp": true,
		}, nil)
		if code != 200 || prefs["whatsapp"] != true {
			t.Fatalf("PUT preferences: %d %v", code, prefs)
		}

		// 3. Inbound webhooks: challenge handshake, forged signature rejection, and non-user STOP blocking later gift purchase.
		chRes, err := http.Get(srv.URL + "/v1/notifications/inbound/whatsapp?hub.mode=subscribe&hub.verify_token=wa_challenge_tok&hub.challenge=987654")
		if err != nil {
			t.Fatal(err)
		}
		chBody, _ := io.ReadAll(chRes.Body)
		chRes.Body.Close()
		if chRes.StatusCode != 200 || string(chBody) != "987654" {
			t.Fatalf("whatsapp challenge: %d %q", chRes.StatusCode, string(chBody))
		}

		nonUserGiftPhone := "+2348066667777"
		stopForm := url.Values{"from": {nonUserGiftPhone}, "to": {"28901"}, "text": {"STOP"}}.Encode()

		// Forged Africa's Talking signature -> 401.
		forgedReq, _ := http.NewRequest("POST", srv.URL+"/v1/notifications/inbound/africastalking", strings.NewReader(stopForm))
		forgedReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		forgedReq.Header.Set("X-AfricasTalking-Signature", "sha256=deadbeef")
		forgedRes, err := http.DefaultClient.Do(forgedReq)
		if err != nil {
			t.Fatal(err)
		}
		forgedRes.Body.Close()
		if forgedRes.StatusCode != 401 {
			t.Fatalf("forged inbound webhook must return 401, got %d", forgedRes.StatusCode)
		}

		// Valid signed Africa's Talking STOP for non-user gift phone -> 200.
		validReq, _ := http.NewRequest("POST", srv.URL+"/v1/notifications/inbound/africastalking", strings.NewReader(stopForm))
		validReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		validReq.Header.Set("X-AfricasTalking-Signature", africastalking.Sign("at_secret", []byte(stopForm)))
		validRes, err := http.DefaultClient.Do(validReq)
		if err != nil {
			t.Fatal(err)
		}
		validRes.Body.Close()
		if validRes.StatusCode != 200 {
			t.Fatalf("valid inbound STOP returned %d", validRes.StatusCode)
		}

		// New gift purchase to nonUserGiftPhone AFTER inbound STOP -> gift SMS is SUPPRESSED, while purchaser SMS/Email/WhatsApp succeed!
		_, mVer := c.do("POST", "/v1/meters", verTok, map[string]any{"meter_number": "45000005678", "disco_code": "EKEDC"}, nil)
		_, qVer := c.do("POST", "/v1/electricity/quotes", verTok, map[string]any{
			"meter_id": mVer["meter_id"], "amount_minor": 500000, "currency": "NGN", "delivery_preference": "TOKEN",
		}, nil)
		_, tGift := c.do("POST", "/v1/electricity/transactions", verTok, map[string]any{
			"quote_id":         qVer["quote_id"],
			"email":            "ada@example.com",
			"is_gift":          true,
			"recipient_name":   "Emeka",
			"recipient_phone":  nonUserGiftPhone,
			"notify_recipient": true,
		}, map[string]string{"Idempotency-Key": "gift-after-stop-123"})
		refGift := tGift["checkout_url"].(string)[strings.LastIndex(tGift["checkout_url"].(string), "/")+1:]
		_ = pay.Complete(refGift, true, 0)
		c.do("POST", "/v1/payments/webhooks/mockpay", "", map[string]any{"reference": refGift}, map[string]string{"X-Mock-Signature": "dev-signature"})
		drain()

		for _, sms := range atFake.Sent() {
			if sms.To == nonUserGiftPhone {
				t.Fatal("inbound STOP must block subsequent gift SMS to non-user recipient")
			}
		}
		if len(atFake.Sent()) != 2 || len(reFake.Sent()) != 2 || len(waFake.Sent()) != 2 {
			t.Fatalf("expected purchaser SMS=2 Email=2 WhatsApp=2, got sms=%d email=%d wa=%d",
				len(atFake.Sent()), len(reFake.Sent()), len(waFake.Sent()))
		}

		// Also verify Resend complaint webhook and WhatsApp STOP webhook over HTTP.
		ts := strconv.FormatInt(time.Now().UTC().Unix(), 10)
		rePayload, _ := json.Marshal(map[string]any{
			"type": "email.complained",
			"data": map[string]any{"to": []string{"ada@example.com"}},
		})
		reSig, _ := resend.SignWebhook(reSecret, "msg_99", ts, rePayload)
		reReq, _ := http.NewRequest("POST", srv.URL+"/v1/notifications/inbound/resend", bytes.NewReader(rePayload))
		reReq.Header.Set("svix-id", "msg_99")
		reReq.Header.Set("svix-timestamp", ts)
		reReq.Header.Set("svix-signature", reSig)
		reRes, err := http.DefaultClient.Do(reReq)
		if err != nil || reRes.StatusCode != 200 {
			t.Fatalf("resend complaint webhook: %v status=%d", err, reRes.StatusCode)
		}
		reRes.Body.Close()

		waPayload, _ := json.Marshal(map[string]any{
			"object": "whatsapp_business_account",
			"entry": []map[string]any{{
				"changes": []map[string]any{{
					"field": "messages",
					"value": map[string]any{
						"messages": []map[string]any{{"from": "2348022223333", "type": "text", "text": map[string]any{"body": "STOP"}}},
					},
				}},
			}},
		})
		waReq, _ := http.NewRequest("POST", srv.URL+"/v1/notifications/inbound/whatsapp", bytes.NewReader(waPayload))
		waReq.Header.Set("X-Hub-Signature-256", whatsapp.Sign("wa_secret", waPayload))
		waRes, err := http.DefaultClient.Do(waReq)
		if err != nil || waRes.StatusCode != 200 {
			t.Fatalf("whatsapp STOP webhook: %v status=%d", err, waRes.StatusCode)
		}
		waRes.Body.Close()

		// 4. Operator GET /v1/admin/transactions/{id} returns redacted notifications array and audits the view.
		opTok := idp.token(t, "operator-ola", map[string]any{
			"roles": []string{"operator"},
			"amr":   []string{"pwd", "mfa"},
		})
		giftTxnID := tGift["transaction_id"].(string)
		code, detail := c.do("GET", "/v1/admin/transactions/"+giftTxnID, opTok, nil, nil)
		if code != 200 {
			t.Fatalf("operator detail: %d %v", code, detail)
		}
		notifs, ok := detail["notifications"].([]any)
		if !ok || len(notifs) != 7 {
			t.Fatalf("want 7 redacted notification records on admin detail, got %T %v", detail["notifications"], detail["notifications"])
		}

		// Ensure admin detail JSON never contains destination, HMAC, message body or token.
		rawDetail, _ := json.Marshal(detail)
		for _, forbidden := range []string{"ada@example.com", "+2348022223333", nonUserGiftPhone, "45000005678", "destination_hmac"} {
			if strings.Contains(string(rawDetail), forbidden) {
				t.Fatalf("admin detail leaked %q: %s", forbidden, string(rawDetail))
			}
		}

		// Operator view audit recorded against resource_type="notification_deliveries".
		parsedTxnID := uuid.MustParse(giftTxnID)
		viewAudits, err := st.Repos().Audit().ForResource(ctx, "notification_deliveries", parsedTxnID)
		if err != nil || len(viewAudits) != 1 || viewAudits[0].Action != "notification_deliveries.view" {
			t.Fatalf("expected 1 notification_deliveries.view audit event, got %+v %v", viewAudits, err)
		}
	})
}
