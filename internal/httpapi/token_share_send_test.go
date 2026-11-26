package httpapi_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
	"github.com/teamthy/meterly/internal/providers/mock"
	"github.com/teamthy/meterly/internal/registry"
	"github.com/teamthy/meterly/internal/secure"
	"github.com/teamthy/meterly/internal/store/memstore"
)

func TestTokenShareSendSMSEmailWhatsApp(t *testing.T) {
	st := memstore.New()
	logs := &syncBuf{}
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	box, err := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "t1")
	if err != nil {
		t.Fatal(err)
	}
	pay, vend, remote := mock.NewPayments(), mock.NewVending(), mock.NewRemoteCredit()
	o := orchestrator.New(orchestrator.DefaultConfig(), st, registry.New(st, box, vend, remote), box, pay, vend, remote, log)

	atFake := africastalkingtest.New("sandbox", "at_key")
	t.Cleanup(atFake.Close)
	reFake := resendtest.New("re_key")
	t.Cleanup(reFake.Close)
	waFake := whatsapptest.New("99887766", "wa_tok")
	t.Cleanup(waFake.Close)

	atClient, _ := africastalking.New(africastalking.Config{
		Username: "sandbox", APIKey: "at_key", SenderID: "Meterly",
		WebhookSecret: "at_secret", BaseURL: atFake.URL,
	})
	reClient, _ := resend.New(resend.Config{
		APIKey: "re_key", From: "Meterly <noreply@meterly.ng>",
		WebhookSecret: "whsec_test", BaseURL: reFake.URL,
	})
	waClient, _ := whatsapp.New(whatsapp.Config{
		PhoneNumberID: "99887766", AccessToken: "wa_tok",
		AppSecret: "wa_secret", VerifyToken: "wa_challenge", BaseURL: waFake.URL,
	})
	o.RegisterNotificationProviders(atClient.Name(), reClient.Name(), waClient.Name())

	notifSvc, err := notificationservice.New(notificationservice.Config{
		Store: st, Box: box,
		Providers: map[notifications.Channel]notifications.Provider{
			notifications.SMS: atClient, notifications.Email: reClient, notifications.WhatsApp: waClient,
		},
		FeatureGiftSMS: true,
		Log:            log,
	})
	if err != nil {
		t.Fatal(err)
	}

	s := &httpapi.Server{O: o, Auth: auth.DevAuth{}, Payments: pay, Notifications: notifSvc, Log: log, Dev: true, DevPay: pay, DevRemote: remote}
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)
	c := client{t: t, srv: srv}

	const meter = "45012345678"
	code, m := c.do("POST", "/v1/meters", "ada", map[string]any{"meter_number": meter, "disco_code": "IKEDC", "label": "Home"}, nil)
	if code != 201 {
		t.Fatalf("register %d %v", code, m)
	}
	code, q := c.do("POST", "/v1/electricity/quotes", "ada", map[string]any{
		"meter_id": m["meter_id"], "amount_minor": 500000, "currency": "NGN", "delivery_preference": "TOKEN",
	}, nil)
	if code != 201 {
		t.Fatalf("quote %d %v", code, q)
	}
	code, txn := c.do("POST", "/v1/electricity/transactions", "ada", map[string]any{"quote_id": q["quote_id"], "email": "ada@example.com"},
		map[string]string{"Idempotency-Key": "share-send-txn-1234"})
	if code != 202 {
		t.Fatalf("create %d %v", code, txn)
	}
	id := txn["transaction_id"].(string)
	ref := txn["checkout_url"].(string)[strings.LastIndex(txn["checkout_url"].(string), "/")+1:]
	c.do("POST", "/v1/dev/payments/"+ref+"/complete", "", map[string]any{}, nil)
	if code, _ := c.do("POST", "/v1/payments/webhooks/mockpay", "", map[string]any{"reference": ref}, map[string]string{"X-Mock-Signature": "dev-signature"}); code != 200 {
		t.Fatalf("webhook %d", code)
	}
	path := "/v1/electricity/transactions/" + id + "/token/shares"
	if code, _ := c.do("POST", path, "ada", map[string]any{"channel": "SMS"}, map[string]string{"Idempotency-Key": "share-sms-no-phone-1"}); code != 422 {
		t.Fatalf("SMS without verified phone: %d", code)
	}

	contact := map[string]string{
		"X-Dev-Verified-Phone": "+2348022223333",
		"X-Dev-Verified-Email": "ada@example.com",
		"X-Dev-Display-Name":   "Ada",
	}
	code, tok := c.do("GET", "/v1/electricity/transactions/"+id+"/token", "ada", nil, contact)
	if code != 200 {
		t.Fatalf("token %d %v", code, tok)
	}
	rawTok, _ := tok["token"].(string)
	if rawTok == "" {
		t.Fatal("empty token")
	}

	code, share := c.do("POST", path, "ada", map[string]any{"channel": "SMS"}, mergeHdr(contact, "Idempotency-Key", "share-sms-self-123456"))
	if code != 201 {
		t.Fatalf("SMS self %d %v", code, share)
	}
	if _, err := notifSvc.ProcessBatch(context.Background(), 20); err != nil {
		t.Fatal(err)
	}
	if len(atFake.Sent()) != 1 || atFake.Sent()[0].To != "+2348022223333" || !strings.Contains(atFake.Sent()[0].Message, "Token:") {
		t.Fatalf("sms payload %+v", atFake.Sent())
	}

	code, _ = c.do("POST", path, "ada", map[string]any{"channel": "EMAIL"}, mergeHdr(contact, "Idempotency-Key", "share-email-self-1234"))
	if code != 201 {
		t.Fatalf("email self %d", code)
	}
	if _, err := notifSvc.ProcessBatch(context.Background(), 20); err != nil {
		t.Fatal(err)
	}
	if len(reFake.Sent()) != 1 || !strings.Contains(reFake.Sent()[0].Text, "Token:") {
		t.Fatalf("email payload %+v", reFake.Sent())
	}

	phoneHMAC := notifications.DestinationHMAC(box, "+2348022223333")
	if err := st.Repos().Notifications().RecordWhatsAppInbound(context.Background(), phoneHMAC, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	waFake.OpenCustomerServiceWindow("+2348022223333", 24*time.Hour)
	code, _ = c.do("POST", path, "ada", map[string]any{"channel": "WHATSAPP"}, mergeHdr(contact, "Idempotency-Key", "share-wa-self-1234567"))
	if code != 201 {
		t.Fatalf("whatsapp self %d", code)
	}
	if _, err := notifSvc.ProcessBatch(context.Background(), 20); err != nil {
		t.Fatal(err)
	}
	if len(waFake.Sent()) != 1 || !strings.Contains(waFake.Sent()[0].TextBody, "Token:") {
		t.Fatalf("wa payload %+v", waFake.Sent())
	}

	o.SetFeatureGiftSMS(false)
	if code, body := c.do("POST", path, "ada", map[string]any{"channel": "SMS", "destination": "+2348099990000"}, mergeHdr(contact, "Idempotency-Key", "share-sms-other-off1")); code != 403 {
		t.Fatalf("third-party without flag: %d %v", code, body)
	}
	o.SetFeatureGiftSMS(true)
	code, other := c.do("POST", path, "ada", map[string]any{"channel": "SMS", "destination": "+2348099990000"}, mergeHdr(contact, "Idempotency-Key", "share-sms-other-on12"))
	if code != 201 {
		t.Fatalf("third-party %d %v", code, other)
	}
	if _, err := notifSvc.ProcessBatch(context.Background(), 20); err != nil {
		t.Fatal(err)
	}
	foundOther := false
	for _, sms := range atFake.Sent() {
		if sms.To == "+2348099990000" && strings.Contains(sms.Message, "Token:") && strings.Contains(sms.Message, "Sent by") {
			foundOther = true
		}
	}
	if !foundOther {
		t.Fatalf("missing third-party SMS: %+v", atFake.Sent())
	}

	if _, err := st.Repos().Notifications().AddOptOut(context.Background(), notifications.OptOut{
		DestinationHMAC: notifications.DestinationHMAC(box, "+2348099990000"),
		Channel:         notifications.SMS,
		CreatedAt:       time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if code, _ := c.do("POST", path, "ada", map[string]any{"channel": "SMS", "destination": "+2348099990000"}, mergeHdr(contact, "Idempotency-Key", "share-sms-opted-out12")); code != 422 {
		t.Fatalf("opted-out dest: %d", code)
	}

	if code, _ := c.do("POST", path, "eve", map[string]any{"channel": "SMS"}, mergeHdr(contact, "Idempotency-Key", "share-sms-eve-123456")); code != 404 {
		t.Fatalf("other user send: %d", code)
	}

	code, list := c.do("GET", path, "ada", nil, contact)
	if code != 200 {
		t.Fatalf("list %d %v", code, list)
	}
	data, _ := list["data"].([]any)
	if len(data) < 4 {
		t.Fatalf("list want >=4, got %v", list)
	}

	rawLogs := logs.String()
	if strings.Contains(rawLogs, rawTok) || strings.Contains(rawLogs, "+2348022223333") || strings.Contains(rawLogs, "+2348099990000") {
		t.Fatal("logs leaked token or destination")
	}
	rawList, _ := json.Marshal(list)
	if strings.Contains(string(rawList), "+2348022223333") || strings.Contains(string(rawList), rawTok) {
		t.Fatalf("list leaked dest/token: %s", rawList)
	}
}

func mergeHdr(base map[string]string, k, v string) map[string]string {
	out := make(map[string]string, len(base)+1)
	for kk, vv := range base {
		out[kk] = vv
	}
	out[k] = v
	return out
}
