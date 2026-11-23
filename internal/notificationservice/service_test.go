package notificationservice_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
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
	"github.com/teamthy/meterly/internal/store/memstore"
	"github.com/teamthy/meterly/internal/store/pgstore"
	"github.com/teamthy/meterly/internal/store/pgstore/pgtest"
	tx "github.com/teamthy/meterly/internal/transactions"
)

func TestMain(m *testing.M) {
	code := m.Run()
	pgtest.DropTemplate()
	os.Exit(code)
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type harness struct {
	t         *testing.T
	ctx       context.Context
	st        store.Store
	pool      *pgxpool.Pool
	box       *secure.Box
	orch      *orchestrator.Orchestrator
	pub       *outbox.Publisher
	svc       *notificationservice.Service
	pay       *mock.Payments
	vend      *mock.Vending
	atFake    *africastalkingtest.Fake
	reFake    *resendtest.Fake
	waFake    *whatsapptest.Fake
	logs      *syncBuf
	muNow     sync.Mutex
	curNow    time.Time
	whSecretR string
}

func newHarness(t *testing.T, st store.Store, pool *pgxpool.Pool, giftSMS bool) *harness {
	t.Helper()
	ctx := context.Background()
	box, err := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "v1")
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuf{}
	log := slog.New(slog.NewJSONHandler(logs, nil))

	pay := mock.NewPayments()
	vend := mock.NewVending()
	remote := mock.NewRemoteCredit()
	reg := registry.New(st, box, vend, remote)
	orch := orchestrator.New(orchestrator.DefaultConfig(), st, reg, box, pay, vend, remote, log)

	atFake := africastalkingtest.New("sandbox", "at_key_test")
	t.Cleanup(atFake.Close)
	reFake := resendtest.New("re_key_test")
	t.Cleanup(reFake.Close)
	waFake := whatsapptest.New("111222333", "wa_tok_test")
	t.Cleanup(waFake.Close)

	whSecretR := "whsec_" + base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))

	h := &harness{
		t: t, ctx: ctx, st: st, pool: pool, box: box, orch: orch,
		pay: pay, vend: vend, atFake: atFake, reFake: reFake, waFake: waFake,
		logs: logs, curNow: time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC),
		whSecretR: whSecretR,
	}
	nowFn := func() time.Time {
		h.muNow.Lock()
		defer h.muNow.Unlock()
		return h.curNow
	}
	orch.Now = nowFn

	atClient, err := africastalking.New(africastalking.Config{
		Username: "sandbox", APIKey: "at_key_test", SenderID: "Meterly",
		WebhookSecret: "at_wh_secret", BaseURL: atFake.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	reClient, err := resend.New(resend.Config{
		APIKey: "re_key_test", From: "Meterly <noreply@meterly.ng>",
		WebhookSecret: whSecretR, BaseURL: reFake.URL, Now: nowFn,
	})
	if err != nil {
		t.Fatal(err)
	}
	waClient, err := whatsapp.New(whatsapp.Config{
		PhoneNumberID: "111222333", AccessToken: "wa_tok_test",
		AppSecret: "wa_app_secret", VerifyToken: "wa_verify", BaseURL: waFake.URL,
	})
	if err != nil {
		t.Fatal(err)
	}

	svc, err := notificationservice.New(notificationservice.Config{
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
		FeatureGiftSMS: giftSMS,
		HMACRetention:  365 * 24 * time.Hour,
		Now:            nowFn,
		Log:            log,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.svc = svc
	h.pub = outbox.NewPublisher(st, outbox.Chain(outbox.LogSink(log), svc.Sink()), log)
	return h
}

func (h *harness) advance(d time.Duration) {
	h.muNow.Lock()
	h.curNow = h.curNow.Add(d)
	h.muNow.Unlock()
}

func (h *harness) createVerifiedUser(ext, email, phone, displayName string) string {
	h.t.Helper()
	uid, err := h.st.Repos().Users().EnsureByExternalID(h.ctx, ext)
	if err != nil {
		h.t.Fatal(err)
	}
	uc := notifications.UserContacts{UserID: uid, DisplayName: displayName}
	if email != "" {
		ct, hm, mask, err := notifications.SealUserEmail(h.box, uid, email)
		if err != nil {
			h.t.Fatal(err)
		}
		uc.EmailCiphertext, uc.EmailHMAC, uc.EmailMasked, uc.EmailKeyVersion = ct, hm, mask, h.box.KeyVersion
	}
	if phone != "" {
		ct, hm, mask, err := notifications.SealUserPhone(h.box, uid, phone)
		if err != nil {
			h.t.Fatal(err)
		}
		uc.PhoneCiphertext, uc.PhoneHMAC, uc.PhoneMasked, uc.PhoneKeyVersion = ct, hm, mask, h.box.KeyVersion
	}
	if email != "" || phone != "" || displayName != "" {
		if err := h.st.Repos().Users().UpsertContacts(h.ctx, uc); err != nil {
			h.t.Fatal(err)
		}
	}
	return uid
}

func (h *harness) buyToken(uid, meterNo string, amountMinor int64, isGift bool, recipientName, giftPhone string, notifyGift bool) *tx.Transaction {
	h.t.Helper()
	m, err := h.orch.Meters.Register(h.ctx, uid, meterNo, "EKEDC", "Home")
	if err != nil {
		h.t.Fatal(err)
	}
	q, err := h.orch.CreateQuote(h.ctx, uid, m.ID, amountMinor, orchestrator.PrefToken)
	if err != nil {
		h.t.Fatal(err)
	}
	idem := "idem-" + uuid.NewString()
	tObj, _, err := h.orch.CreateTransaction(h.ctx, orchestrator.CreateRequest{
		CustomerID:      uid,
		QuoteID:         q.ID,
		Email:           "checkout@meterly.ng",
		IsGift:          isGift,
		RecipientName:   recipientName,
		RecipientPhone:  giftPhone,
		NotifyRecipient: notifyGift,
		IdempotencyKey:  idem,
		RequestHash:     idem,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.pay.Complete(tObj.PaymentProviderRef, true, 0); err != nil {
		h.t.Fatal(err)
	}
	settled, err := h.orch.HandlePaymentNotification(h.ctx, tObj.PaymentProviderRef)
	if err != nil {
		h.t.Fatal(err)
	}
	return settled
}

func eachStore(t *testing.T, fn func(t *testing.T, st store.Store, pool *pgxpool.Pool)) {
	t.Run("memstore", func(t *testing.T) { fn(t, memstore.New(), nil) })
	t.Run("pgstore", func(t *testing.T) {
		pool := pgtest.NewPool(t)
		fn(t, pgstore.New(pool, "SANDBOX"), pool)
	})
}

func TestDedupeUnderTenConcurrentConsumers(t *testing.T) {
	eachStore(t, func(t *testing.T, st store.Store, pool *pgxpool.Pool) {
		h := newHarness(t, st, pool, true)
		uid := h.createVerifiedUser("ada", "ada@example.com", "+2348011112222", "Ada")
		if err := st.Repos().Notifications().SetPreferences(h.ctx, uid, notifications.Preferences{
			SMS: true, Email: true, WhatsApp: true,
		}, h.curNow); err != nil {
			t.Fatal(err)
		}

		settled := h.buyToken(uid, "45000005678", 500000, true, "Chidi", "+2348093334444", true)

		var wg sync.WaitGroup
		errs := make(chan error, 40)
		for worker := 0; worker < 10; worker++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for step := 0; step < 3; step++ {
					if _, err := h.pub.PublishBatch(h.ctx); err != nil {
						errs <- err
						return
					}
					if _, err := h.svc.ProcessBatch(h.ctx, 50); err != nil {
						errs <- err
						return
					}
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("concurrent worker error: %v", err)
		}

		// Expect 2 allowlisted events: payment.confirmed (3 purchaser channels) +
		// token.available (3 purchaser channels + 1 gift SMS) = 7 deliveries, each sent once!
		delivs, err := st.Repos().Notifications().ForTransaction(h.ctx, settled.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(delivs) != 7 {
			t.Fatalf("want 7 deduplicated deliveries, got %d: %+v", len(delivs), delivs)
		}
		for _, d := range delivs {
			if d.Status != notifications.StatusSent || d.Attempts != 1 {
				t.Fatalf("delivery %+v not sent once", d)
			}
		}
		if len(h.atFake.Sent()) != 3 { // 2 purchaser SMS + 1 gift SMS
			t.Fatalf("want 3 SMS sends, got %d", len(h.atFake.Sent()))
		}
		if len(h.reFake.Sent()) != 2 { // 2 purchaser emails
			t.Fatalf("want 2 Email sends, got %d", len(h.reFake.Sent()))
		}
		if len(h.waFake.Sent()) != 2 { // 2 purchaser WhatsApp template messages
			t.Fatalf("want 2 WhatsApp sends, got %d", len(h.waFake.Sent()))
		}
	})
}

func TestGiftSMSRequiresAllThreeConditionsAndExactFormat(t *testing.T) {
	eachStore(t, func(t *testing.T, st store.Store, pool *pgxpool.Pool) {
		h := newHarness(t, st, pool, false) // start with FEATURE_GIFT_SMS=false
		uid := h.createVerifiedUser("ada", "ada@example.com", "+2348011112222", "Ada")

		// Case 1: FEATURE_GIFT_SMS=false -> blocked (SUPPRESSED).
		t1 := h.buyToken(uid, "45000005678", 500000, true, "Bola", "+2348022223333", true)
		_, _ = h.pub.PublishBatch(h.ctx)
		_, _ = h.svc.ProcessBatch(h.ctx, 50)
		assertGiftDeliveryStatus(t, st, t1.ID, notifications.StatusSuppressed, "FEATURE_GIFT_SMS_DISABLED")
		for _, sms := range h.atFake.Sent() {
			if sms.To == "+2348022223333" {
				t.Fatal("gift SMS must not send when FEATURE_GIFT_SMS=false")
			}
		}

		// Enable FEATURE_GIFT_SMS=true for remaining checks.
		h.svc.SetFeatureGiftSMS(true)

		// Case 2: notification_requested=false -> blocked (SUPPRESSED).
		t2 := h.buyToken(uid, "45000005678", 500000, true, "Bola", "", false)
		_, _ = h.pub.PublishBatch(h.ctx)
		_, _ = h.svc.ProcessBatch(h.ctx, 50)
		assertGiftDeliveryStatus(t, st, t2.ID, notifications.StatusSuppressed, "NOTIFICATION_NOT_REQUESTED")

		// Case 3: recipient opted out -> blocked (SUPPRESSED).
		optedPhone := "+2348033334444"
		_, err := st.Repos().Notifications().AddOptOut(h.ctx, notifications.OptOut{
			DestinationHMAC: notifications.DestinationHMAC(h.box, optedPhone),
			Channel:         notifications.SMS,
		})
		if err != nil {
			t.Fatal(err)
		}
		t3 := h.buyToken(uid, "45000005678", 500000, true, "Tunde", optedPhone, true)
		_, _ = h.pub.PublishBatch(h.ctx)
		_, _ = h.svc.ProcessBatch(h.ctx, 50)
		assertGiftDeliveryStatus(t, st, t3.ID, notifications.StatusSuppressed, "RECIPIENT_OPTED_OUT")
		for _, sms := range h.atFake.Sent() {
			if sms.To == optedPhone {
				t.Fatal("gift SMS must not send to opted-out phone")
			}
		}

		// Case 4: all 3 conditions true -> exact SMS format, purchaser messages never contain token.
		giftPhone := "+2348044445555"
		t4 := h.buyToken(uid, "45000005678", 500000, true, "Ngozi", giftPhone, true)
		rawTok, err := h.box.Decrypt(t4.TokenCiphertext)
		if err != nil {
			t.Fatal(err)
		}
		groupedTok, err := notifications.FormatSTSToken(rawTok)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = h.pub.PublishBatch(h.ctx)
		_, _ = h.svc.ProcessBatch(h.ctx, 50)
		assertGiftDeliveryStatus(t, st, t4.ID, notifications.StatusSent, "")

		var giftBody string
		for _, sms := range h.atFake.Sent() {
			if sms.To == giftPhone {
				giftBody = sms.Message
			} else if strings.Contains(sms.Message, rawTok) || strings.Contains(sms.Message, groupedTok) {
				t.Fatalf("purchaser SMS leaked token: %q", sms.Message)
			}
		}
		wantBody := fmt.Sprintf("Meterly: ₦5,000 electricity for meter ****5678. Token: %s. Sent by Ada. Reply STOP to opt out.", groupedTok)
		if giftBody != wantBody {
			t.Fatalf("gift SMS format mismatch:\ngot:  %q\nwant: %q", giftBody, wantBody)
		}
		for _, em := range h.reFake.Sent() {
			if strings.Contains(em.Text, rawTok) || strings.Contains(em.Text, groupedTok) {
				t.Fatalf("purchaser email leaked token: %q", em.Text)
			}
		}
	})
}

func assertGiftDeliveryStatus(t *testing.T, st store.Store, txnID uuid.UUID, want notifications.Status, wantCode string) {
	t.Helper()
	delivs, err := st.Repos().Notifications().ForTransaction(context.Background(), txnID)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range delivs {
		if d.RecipientKind == notifications.RecipientGift {
			if d.Status != want || d.LastErrorCode != wantCode {
				t.Fatalf("gift delivery got status=%s code=%q, want status=%s code=%q", d.Status, d.LastErrorCode, want, wantCode)
			}
			return
		}
	}
	t.Fatalf("no GIFT_RECIPIENT delivery found for txn %s", txnID)
}

func TestOptOutAtSendTimeGiftToSelfAndLostResponseUnknown(t *testing.T) {
	eachStore(t, func(t *testing.T, st store.Store, pool *pgxpool.Pool) {
		h := newHarness(t, st, pool, true)
		uid := h.createVerifiedUser("ada", "ada@example.com", "+2348011112222", "Ada")

		// 1. Enqueue PENDING deliveries first, THEN opt out gift recipient and disable purchaser Email before worker sends.
		giftPhone := "+2348055556666"
		tOpt := h.buyToken(uid, "45000005678", 500000, true, "Kemi", giftPhone, true)
		if _, err := h.pub.PublishBatch(h.ctx); err != nil {
			t.Fatal(err)
		}
		// Now opt out giftPhone and disable purchaser Email before ProcessBatch runs.
		_, _ = st.Repos().Notifications().AddOptOut(h.ctx, notifications.OptOut{
			DestinationHMAC: notifications.DestinationHMAC(h.box, giftPhone),
			Channel:         notifications.SMS,
		})
		_ = st.Repos().Notifications().SetPreferences(h.ctx, uid, notifications.Preferences{SMS: true, Email: false, WhatsApp: false}, h.curNow)
		if _, err := h.svc.ProcessBatch(h.ctx, 50); err != nil {
			t.Fatal(err)
		}
		assertGiftDeliveryStatus(t, st, tOpt.ID, notifications.StatusSuppressed, "RECIPIENT_OPTED_OUT")
		if len(h.reFake.Sent()) != 0 {
			t.Fatalf("disabled email preference at send time must suppress email, got %d", len(h.reFake.Sent()))
		}

		// 2. Gift-to-self phone edge case: purchaser gifts to their own phone (+2348011112222).
		beforeSMS := len(h.atFake.Sent())
		tSelf := h.buyToken(uid, "45000005678", 500000, true, "Ada", "+2348011112222", true)
		_, _ = h.pub.PublishBatch(h.ctx)
		_, _ = h.svc.ProcessBatch(h.ctx, 50)
		delivs, _ := st.Repos().Notifications().ForTransaction(h.ctx, tSelf.ID)
		var sawDupSuppress bool
		for _, d := range delivs {
			if d.TemplateID == notifications.TemplateTokenAvailable && d.Channel == notifications.SMS && d.RecipientKind == notifications.RecipientPurchaser {
				if d.Status != notifications.StatusSuppressed || d.LastErrorCode != "DUPLICATE_DESTINATION" {
					t.Fatalf("want DUPLICATE_DESTINATION on gift-to-self purchaser SMS, got %+v", d)
				}
				sawDupSuppress = true
			}
		}
		if !sawDupSuppress {
			t.Fatal("expected gift-to-self purchaser SMS row")
		}
		// 1 SMS for payment.confirmed + 1 gift_token_sms for token.available = 2 SMS total (not 3).
		if got := len(h.atFake.Sent()) - beforeSMS; got != 2 {
			t.Fatalf("want 2 SMS on gift-to-self purchase, got %d", got)
		}

		// 3. Lost send response on token SMS -> UNKNOWN and NEVER auto-resent!
		_ = st.Repos().Notifications().SetPreferences(h.ctx, uid, notifications.Preferences{SMS: false, Email: false, WhatsApp: false}, h.curNow)
		lostGiftPhone := "+2348077778888"
		tLost := h.buyToken(uid, "45000005678", 500000, true, "Kunle", lostGiftPhone, true)
		_, _ = h.pub.PublishBatch(h.ctx)

		h.atFake.FailAfterApply(1) // provider sends SMS, HTTP 500 loses response
		_, _ = h.svc.ProcessBatch(h.ctx, 50)
		assertGiftDeliveryStatus(t, st, tLost.ID, notifications.StatusUnknown, "PROVIDER_AMBIGUOUS")

		// Even across multiple hours of retries, token SMS MUST NEVER be resent.
		for _, step := range []time.Duration{time.Minute, 10 * time.Minute, 2 * time.Hour, 24 * time.Hour} {
			h.advance(step)
			_, _ = h.svc.ProcessBatch(h.ctx, 50)
		}
		assertGiftDeliveryStatus(t, st, tLost.ID, notifications.StatusUnknown, "PROVIDER_AMBIGUOUS")
		sentToKunle := 0
		for _, sms := range h.atFake.Sent() {
			if sms.To == lostGiftPhone {
				sentToKunle++
			}
		}
		if sentToKunle != 1 {
			t.Fatalf("token SMS must be attempted once and never resent after UNKNOWN, got %d provider sends", sentToKunle)
		}

		// 4. Resend Idempotency-Key deduplicates non-token UNKNOWN retry, and retryable 429 follows 30s, 2m, 10m, 1h backoff to FAILED at attempt 5.
		_ = st.Repos().Notifications().SetPreferences(h.ctx, uid, notifications.Preferences{SMS: true, Email: true, WhatsApp: false}, h.curNow)
		tRetry := h.buyToken(uid, "45000005678", 500000, false, "", "", false)
		_, _ = h.pub.PublishBatch(h.ctx)

		delivsRetry, _ := st.Repos().Notifications().ForTransaction(h.ctx, tRetry.ID)
		var emailID, smsID uuid.UUID
		for _, d := range delivsRetry {
			if d.TemplateID == notifications.TemplatePaymentConfirmed {
				switch d.Channel {
				case notifications.Email:
					emailID = d.ID
				case notifications.SMS:
					smsID = d.ID
				}
			}
		}

		// Email: lost response -> UNKNOWN, then 1 retry after 30s settles SENT with only 1 email at Resend.
		emailsBefore := len(h.reFake.Sent())
		h.reFake.FailAfterApply(1)
		_, _ = h.svc.ProcessOne(h.ctx, emailID)
		dEmail, _ := st.Repos().Notifications().GetDelivery(h.ctx, emailID)
		if dEmail.Status != notifications.StatusUnknown || dEmail.Attempts != 1 {
			t.Fatalf("want UNKNOWN attempt 1 on lost email response, got %+v", dEmail)
		}
		h.advance(35 * time.Second)
		_, _ = h.svc.ProcessOne(h.ctx, emailID)
		dEmail, _ = st.Repos().Notifications().GetDelivery(h.ctx, emailID)
		if dEmail.Status != notifications.StatusSent || len(h.reFake.Sent())-emailsBefore != 1 {
			t.Fatalf("want SENT with 1 deduplicated Resend email, got %+v count=%d", dEmail, len(h.reFake.Sent())-emailsBefore)
		}

		// SMS: 5 consecutive HTTP 429s follow 30s, 2m, 10m, 1h backoff and settle FAILED on attempt 5.
		wantDelays := []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute, time.Hour}
		for i, wantDelay := range wantDelays {
			stepStart := h.curNow
			h.atFake.SetNextHTTPStatus(http.StatusTooManyRequests)
			_, _ = h.svc.ProcessOne(h.ctx, smsID)
			cur, _ := st.Repos().Notifications().GetDelivery(h.ctx, smsID)
			if cur.Status != notifications.StatusPending || cur.Attempts != i+1 || !cur.NextAttemptAt.Equal(stepStart.Add(wantDelay)) {
				t.Fatalf("attempt %d: got status=%s attempts=%d next=%s wantNext=%s",
					i+1, cur.Status, cur.Attempts, cur.NextAttemptAt, stepStart.Add(wantDelay))
			}
			h.advance(wantDelay)
		}
		h.atFake.SetNextHTTPStatus(http.StatusTooManyRequests)
		_, _ = h.svc.ProcessOne(h.ctx, smsID)
		dFinal, err := st.Repos().Notifications().GetDelivery(h.ctx, smsID)
		if err != nil || dFinal.Status != notifications.StatusFailed || dFinal.Attempts != notifications.MaxAttempts || dFinal.LastErrorCode != "RATE_LIMITED" {
			t.Fatalf("after %d 429 attempts want FAILED/RATE_LIMITED, got %+v %v", notifications.MaxAttempts, dFinal, err)
		}
	})
}

func TestPrivacyLogsDBAndRetentionPurge(t *testing.T) {
	eachStore(t, func(t *testing.T, st store.Store, pool *pgxpool.Pool) {
		h := newHarness(t, st, pool, true)
		secretEmail := "secret.purchaser@example.com"
		secretPhone := "+2348019876543"
		secretGiftPhone := "+2348098765432"
		secretMeter := "45099887766"

		uid := h.createVerifiedUser("ada", secretEmail, secretPhone, "Ada")
		settled := h.buyToken(uid, secretMeter, 500000, true, "Zainab", secretGiftPhone, true)
		rawToken, err := h.box.Decrypt(settled.TokenCiphertext)
		if err != nil {
			t.Fatal(err)
		}
		groupedToken, _ := notifications.FormatSTSToken(rawToken)

		_, _ = h.pub.PublishBatch(h.ctx)
		_, _ = h.svc.ProcessBatch(h.ctx, 50)

		// Logs must contain zero raw token, meter number, phone or email.
		logText := h.logs.String()
		for _, forbidden := range []string{secretEmail, secretPhone, secretGiftPhone, secretMeter, rawToken, groupedToken} {
			if strings.Contains(logText, forbidden) {
				t.Fatalf("log output leaked sensitive value %q:\n%s", forbidden, logText)
			}
		}

		// Outbox payloads must contain zero raw token, meter, phone or email.
		evs, err := st.Repos().Outbox().ForAggregate(h.ctx, settled.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range evs {
			raw := fmt.Sprintf("%+v", ev.Payload)
			for _, forbidden := range []string{secretEmail, secretPhone, secretGiftPhone, secretMeter, rawToken, groupedToken} {
				if strings.Contains(raw, forbidden) {
					t.Fatalf("outbox payload leaked %q", forbidden)
				}
			}
		}

		// Postgres raw row inspection: no plaintext email/phone/token/meter in users or notification_deliveries.
		if pool != nil {
			var dump string
			if err := pool.QueryRow(h.ctx, `
				SELECT coalesce(string_agg(row_to_json(u)::text, ' '), '') FROM users u`).Scan(&dump); err != nil {
				t.Fatal(err)
			}
			for _, forbidden := range []string{secretEmail, secretPhone, "801987"} {
				if strings.Contains(dump, forbidden) {
					t.Fatalf("users table leaked plaintext %q: %s", forbidden, dump)
				}
			}
			if err := pool.QueryRow(h.ctx, `
				SELECT coalesce(string_agg(row_to_json(nd)::text, ' '), '') FROM notification_deliveries nd`).Scan(&dump); err != nil {
				t.Fatal(err)
			}
			for _, forbidden := range []string{secretEmail, secretPhone, secretGiftPhone, secretMeter, rawToken, groupedToken} {
				if strings.Contains(dump, forbidden) {
					t.Fatalf("notification_deliveries table leaked %q: %s", forbidden, dump)
				}
			}
		}

		// 12-month retention job purges destination_hmac on settled rows once aged past 365d.
		if purged, err := h.svc.PurgeExpiredHMACs(h.ctx); err != nil || purged != 0 {
			t.Fatalf("fresh deliveries must not be purged yet: purged=%d err=%v", purged, err)
		}
		h.advance(366 * 24 * time.Hour)
		delivsBefore, _ := st.Repos().Notifications().ForTransaction(h.ctx, settled.ID)
		purged, err := h.svc.PurgeExpiredHMACs(h.ctx)
		if err != nil || purged != len(delivsBefore) {
			t.Fatalf("want %d destination_hmacs purged after 12 months, got %d (err=%v)", len(delivsBefore), purged, err)
		}
		delivsAfter, _ := st.Repos().Notifications().ForTransaction(h.ctx, settled.ID)
		for _, d := range delivsAfter {
			if d.DestinationHMAC != "" {
				t.Fatalf("destination_hmac was not cleared by retention job: %+v", d)
			}
		}
	})
}
