package conversation_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/conversation"
)

type fakeBackend struct {
	now          time.Time
	verified     bool
	reverify     bool
	meters       []conversation.MeterOption
	ussdSessions map[string]*conversation.Session
	waSession    *conversation.Session
	seenWAMIDs   map[string]bool
	lastOTP      string
	otpAttempts  int
	purchases    []conversation.PurchaseRequest
	resends      int
	hasToken     bool
	optedOut     bool
	todaySpend   int64
}

func newFakeBackend(t0 time.Time) *fakeBackend {
	return &fakeBackend{
		now:          t0,
		ussdSessions: map[string]*conversation.Session{},
		seenWAMIDs:   map[string]bool{},
		hasToken:     true,
	}
}

func (f *fakeBackend) ResolveCaller(_ context.Context, rawPhone string, now time.Time) (conversation.CallerIdentity, error) {
	st := conversation.LinkUnverified
	if f.reverify {
		st = conversation.LinkReverifyRequired
	} else if f.verified {
		st = conversation.LinkVerified
	}
	return conversation.CallerIdentity{
		UserID:   "user-1",
		Phone:    rawPhone,
		HMAC:     "hmac-phone-1",
		Link:     &conversation.PhoneLink{PhoneHMAC: "hmac-phone-1", UserID: "user-1", Status: st, LastActiveAt: now},
		Verified: f.verified && !f.reverify,
	}, nil
}

func (f *fakeBackend) RecordOptOut(_ context.Context, _ conversation.Channel, _, _ string, _ time.Time) error {
	f.optedOut = true
	return nil
}

func (f *fakeBackend) CheckWhatsAppMessageOnce(_ context.Context, msgID, _ string, _ time.Time) (bool, error) {
	if f.seenWAMIDs[msgID] {
		return false, nil
	}
	f.seenWAMIDs[msgID] = true
	return true, nil
}

func (f *fakeBackend) LoadOrCreateUSSDSession(_ context.Context, sessionID, phoneHMAC, userID string, now time.Time) (*conversation.Session, bool, error) {
	if s, ok := f.ussdSessions[sessionID]; ok {
		cp := *s
		return &cp, false, nil
	}
	s := &conversation.Session{
		ID:                uuid.New(),
		Channel:           conversation.ChannelUSSD,
		ProviderSessionID: sessionID,
		PhoneHMAC:         phoneHMAC,
		UserID:            userID,
		State:             conversation.StateMenu,
		ExpiresAt:         now.Add(conversation.USSDSessionTTL),
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	f.ussdSessions[sessionID] = s
	cp := *s
	return &cp, true, nil
}

func (f *fakeBackend) LoadActiveWhatsAppSession(_ context.Context, _ string, now time.Time) (*conversation.Session, error) {
	if f.waSession == nil || f.waSession.Expired(now) {
		return nil, nil
	}
	cp := *f.waSession
	return &cp, nil
}

func (f *fakeBackend) CreateWhatsAppSession(_ context.Context, phoneHMAC, userID string, now time.Time) (*conversation.Session, error) {
	s := &conversation.Session{
		ID:                uuid.New(),
		Channel:           conversation.ChannelWhatsApp,
		ProviderSessionID: uuid.NewString(),
		PhoneHMAC:         phoneHMAC,
		UserID:            userID,
		State:             conversation.StateMenu,
		ExpiresAt:         now.Add(conversation.WhatsAppSessionTTL),
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	f.waSession = s
	cp := *s
	return &cp, nil
}

func (f *fakeBackend) SaveSession(_ context.Context, sess *conversation.Session) error {
	cp := *sess
	if sess.Channel == conversation.ChannelUSSD {
		f.ussdSessions[sess.ProviderSessionID] = &cp
	} else {
		f.waSession = &cp
	}
	return nil
}

func (f *fakeBackend) ListSavedMeters(_ context.Context, _ string) ([]conversation.MeterOption, error) {
	return append([]conversation.MeterOption(nil), f.meters...), nil
}

func (f *fakeBackend) RegisterMeter(_ context.Context, _, disco, num string) (conversation.MeterOption, error) {
	if len(num) < 11 || strings.HasPrefix(num, "0") {
		return conversation.MeterOption{}, conversation.ErrInvalidAmount
	}
	m := conversation.MeterOption{
		MeterID: uuid.New(),
		Masked:  "*******" + num[len(num)-4:],
		Disco:   disco,
	}
	f.meters = append(f.meters, m)
	return m, nil
}

func (f *fakeBackend) TodayPhoneSpendMinor(_ context.Context, _ string, _ time.Time) (int64, error) {
	return f.todaySpend, nil
}

func (f *fakeBackend) IssueOTP(_ context.Context, _, _, _ string, _ time.Time) error {
	f.lastOTP = "654321"
	f.otpAttempts = 0
	return nil
}

func (f *fakeBackend) VerifyOTP(_ context.Context, _, _, candidate string, _ time.Time) (string, bool, bool, error) {
	f.otpAttempts++
	if candidate == f.lastOTP && f.lastOTP != "" {
		f.verified = true
		f.reverify = false
		return "user-1", true, false, nil
	}
	if f.otpAttempts >= conversation.OTPMaxAttempts {
		return "", false, true, nil
	}
	return "", false, false, nil
}

func (f *fakeBackend) ExecutePurchase(_ context.Context, req conversation.PurchaseRequest) (conversation.PurchaseResult, error) {
	f.purchases = append(f.purchases, req)
	f.todaySpend += req.AmountMinor
	return conversation.PurchaseResult{
		TransactionID: uuid.New(),
		CheckoutURL:   "https://checkout.paystack.com/offline_123",
		TotalMinor:    req.AmountMinor + 10_000,
		FeeMinor:      10_000,
	}, nil
}

func (f *fakeBackend) ResendLastToken(_ context.Context, _ conversation.Channel, _, _, _ string, _ time.Time) (conversation.ResendResult, error) {
	if !f.hasToken {
		return conversation.ResendResult{}, conversation.ErrNoSavedToken
	}
	if f.resends >= conversation.MaxDailyTokenResends {
		return conversation.ResendResult{}, conversation.ErrResendLimitReached
	}
	f.resends++
	return conversation.ResendResult{
		TransactionID: uuid.New(),
		MeterMasked:   "****5678",
		AmountMinor:   500_000,
		ResendsToday:  f.resends,
	}, nil
}

func TestEngineTableDrivenUSSDFlowsTimeoutAndDuplicateCallbacks(t *testing.T) {
	t0 := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	ctx := context.Background()

	t.Run("happy_path_saved_meter_and_idempotent_duplicate_callback", func(t *testing.T) {
		fb := newFakeBackend(t0)
		fb.verified = true
		fb.meters = []conversation.MeterOption{
			{MeterID: uuid.New(), Masked: "*******1111", Disco: "IKEDC", Label: "Home"},
			{MeterID: uuid.New(), Masked: "*******2222", Disco: "EKEDC", Label: "Shop"},
		}
		eng := conversation.NewEngine(conversation.EngineConfig{Now: func() time.Time { return fb.now }}, fb)

		hops := []struct {
			rawText    string
			wantPrefix string
			wantSubstr string
		}{
			{"", "CON ", "1. Buy for my meter"},
			{"2", "CON ", "1. Home (****1111)"},
			{"2*2", "CON ", "Meter ****2222 (EKEDC)"},
			{"2*2*5000", "CON ", "Buy N5000 (+N100 fee = N5100)"},
			{"2*2*5000*1", "END ", "Payment link sent by SMS for N5100"},
		}
		var lastReply string
		for _, h := range hops {
			fb.now = fb.now.Add(5 * time.Second)
			reply, err := eng.HandleUSSD(ctx, conversation.USSDTurnInput{
				SessionID:   "ATUid_happy",
				ServiceCode: "*347*88#",
				PhoneNumber: "+2348030001111",
				RawText:     h.rawText,
			})
			if err != nil {
				t.Fatalf("hop %q err: %v", h.rawText, err)
			}
			if len(reply) > conversation.MaxUSSDChars {
				t.Fatalf("hop %q exceeded 182 chars (%d): %q", h.rawText, len(reply), reply)
			}
			if !strings.HasPrefix(reply, h.wantPrefix) || !strings.Contains(reply, h.wantSubstr) {
				t.Fatalf("hop %q got %q, want prefix %q + %q", h.rawText, reply, h.wantPrefix, h.wantSubstr)
			}
			lastReply = reply
		}
		if len(fb.purchases) != 1 || conversation.IdempotencyKey(conversation.ChannelUSSD, "ATUid_happy", fb.purchases[0].Step) != "ussd:ATUid_happy:4" {
			t.Fatalf("unexpected purchases: %+v", fb.purchases)
		}

		// Duplicate aggregator callback for final confirm hop MUST return cached reply without second purchase!
		dupReply, err := eng.HandleUSSD(ctx, conversation.USSDTurnInput{
			SessionID:   "ATUid_happy",
			ServiceCode: "*347*88#",
			PhoneNumber: "+2348030001111",
			RawText:     "2*2*5000*1",
		})
		if err != nil || dupReply != lastReply || len(fb.purchases) != 1 {
			t.Fatalf("duplicate callback must be idempotent: got %q err=%v purchases=%d", dupReply, err, len(fb.purchases))
		}
	})

	t.Run("timeout_after_180s_creates_zero_transactions", func(t *testing.T) {
		fb := newFakeBackend(t0)
		fb.verified = true
		fb.meters = []conversation.MeterOption{{MeterID: uuid.New(), Masked: "*******1111", Disco: "IKEDC"}}
		eng := conversation.NewEngine(conversation.EngineConfig{Now: func() time.Time { return fb.now }}, fb)

		_, _ = eng.HandleUSSD(ctx, conversation.USSDTurnInput{SessionID: "ATUid_exp", PhoneNumber: "+2348030001111", RawText: ""})
		_, _ = eng.HandleUSSD(ctx, conversation.USSDTurnInput{SessionID: "ATUid_exp", PhoneNumber: "+2348030001111", RawText: "1"})
		_, _ = eng.HandleUSSD(ctx, conversation.USSDTurnInput{SessionID: "ATUid_exp", PhoneNumber: "+2348030001111", RawText: "1*5000"})

		// Advance clock past 180s TTL before user dials "1" to confirm!
		fb.now = fb.now.Add(181 * time.Second)
		reply, err := eng.HandleUSSD(ctx, conversation.USSDTurnInput{
			SessionID:   "ATUid_exp",
			PhoneNumber: "+2348030001111",
			RawText:     "1*5000*1",
		})
		if err != nil || !strings.HasPrefix(reply, "END Session timed out") {
			t.Fatalf("expected END timeout reply, got %q (%v)", reply, err)
		}
		if len(fb.purchases) != 0 {
			t.Fatalf("timed-out USSD session must create zero transactions, got %d", len(fb.purchases))
		}
	})

	t.Run("sim_swap_inactivity_otp_and_3_per_day_last_token_limit", func(t *testing.T) {
		fb := newFakeBackend(t0)
		fb.verified = true
		fb.reverify = true // inactive > N days
		eng := conversation.NewEngine(conversation.EngineConfig{Now: func() time.Time { return fb.now }}, fb)

		// Option 3 ("Last token") on stale phone requires SMS OTP first.
		_, _ = eng.HandleUSSD(ctx, conversation.USSDTurnInput{SessionID: "ATUid_tok1", PhoneNumber: "+2348030001111", RawText: ""})
		r1, _ := eng.HandleUSSD(ctx, conversation.USSDTurnInput{SessionID: "ATUid_tok1", PhoneNumber: "+2348030001111", RawText: "3"})
		if !strings.HasPrefix(r1, "CON ") || !strings.Contains(r1, "6-digit SMS code") {
			t.Fatalf("expected OTP prompt on SIM-swap reverification, got %q", r1)
		}
		r2, _ := eng.HandleUSSD(ctx, conversation.USSDTurnInput{SessionID: "ATUid_tok1", PhoneNumber: "+2348030001111", RawText: "3*" + fb.lastOTP})
		if !strings.HasPrefix(r2, "END Last token for ****5678 sent by SMS (1/3 today).") {
			t.Fatalf("expected token resend 1/3 after OTP verify, got %q", r2)
		}

		// Resends 2 and 3 succeed; resend 4 hits daily cap.
		for i := 2; i <= 3; i++ {
			sid := "ATUid_tok_" + string(rune('0'+i))
			_, _ = eng.HandleUSSD(ctx, conversation.USSDTurnInput{SessionID: sid, PhoneNumber: "+2348030001111", RawText: ""})
			rn, _ := eng.HandleUSSD(ctx, conversation.USSDTurnInput{SessionID: sid, PhoneNumber: "+2348030001111", RawText: "3"})
			if !strings.Contains(rn, "/3 today") {
				t.Fatalf("resend %d unexpected: %q", i, rn)
			}
		}
		_, _ = eng.HandleUSSD(ctx, conversation.USSDTurnInput{SessionID: "ATUid_tok_4", PhoneNumber: "+2348030001111", RawText: ""})
		r4, _ := eng.HandleUSSD(ctx, conversation.USSDTurnInput{SessionID: "ATUid_tok_4", PhoneNumber: "+2348030001111", RawText: "3"})
		if !strings.Contains(r4, "Daily limit reached") {
			t.Fatalf("4th resend must be rejected, got %q", r4)
		}
	})
}

func TestEngineWhatsAppOutOfOrderDuplicateAndStop(t *testing.T) {
	t0 := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	fb := newFakeBackend(t0)
	fb.verified = true
	fb.meters = []conversation.MeterOption{{MeterID: uuid.New(), Masked: "*******5678", Disco: "IKEDC", Label: "Flat 2"}}
	eng := conversation.NewEngine(conversation.EngineConfig{Now: func() time.Time { return fb.now }}, fb)
	ctx := context.Background()

	// 1. "buy" lists saved meters.
	out1, err := eng.HandleWhatsApp(ctx, conversation.WhatsAppTurnInput{
		MessageID: "wamid.1", PhoneNumber: "+2348039990000", Text: "buy", Timestamp: t0,
	})
	if err != nil || !strings.Contains(out1.Reply, "Flat 2 (****5678)") {
		t.Fatalf("buy reply: %+v (%v)", out1, err)
	}

	// Duplicate webhook for wamid.1 is ignored.
	dup, err := eng.HandleWhatsApp(ctx, conversation.WhatsAppTurnInput{
		MessageID: "wamid.1", PhoneNumber: "+2348039990000", Text: "buy", Timestamp: t0,
	})
	if err != nil || !dup.Duplicate || dup.Reply != "" {
		t.Fatalf("expected Duplicate=true, got %+v (%v)", dup, err)
	}

	// 2. Pick meter "1" at t0 + 10s -> state becomes AWAIT_AMOUNT.
	out2, _ := eng.HandleWhatsApp(ctx, conversation.WhatsAppTurnInput{
		MessageID: "wamid.2", PhoneNumber: "+2348039990000", Text: "1", Timestamp: t0.Add(10 * time.Second),
	})
	if !strings.Contains(out2.Reply, "Enter amount in Naira") {
		t.Fatalf("pick meter reply: %+v", out2)
	}

	// Out-of-order delayed message from t0 + 5s arrives AFTER t0 + 10s -> ignored without corrupting AWAIT_AMOUNT!
	ooo, _ := eng.HandleWhatsApp(ctx, conversation.WhatsAppTurnInput{
		MessageID: "wamid.old", PhoneNumber: "+2348039990000", Text: "garbage", Timestamp: t0.Add(5 * time.Second),
	})
	if !ooo.IgnoredOutOfOrder {
		t.Fatalf("expected IgnoredOutOfOrder=true, got %+v", ooo)
	}

	// 3. Enter amount "4000" -> AWAIT_CONFIRM.
	out3, _ := eng.HandleWhatsApp(ctx, conversation.WhatsAppTurnInput{
		MessageID: "wamid.3", PhoneNumber: "+2348039990000", Text: "4000", Timestamp: t0.Add(20 * time.Second),
	})
	if !strings.Contains(out3.Reply, "Buy N4000") {
		t.Fatalf("amount reply: %+v", out3)
	}

	// 4. Reply "STOP" -> records M5 opt-out and cancels session without creating a transaction!
	stopOut, _ := eng.HandleWhatsApp(ctx, conversation.WhatsAppTurnInput{
		MessageID: "wamid.stop", PhoneNumber: "+2348039990000", Text: "STOP", Timestamp: t0.Add(25 * time.Second),
	})
	if !stopOut.OptedOut || !fb.optedOut || len(fb.purchases) != 0 {
		t.Fatalf("STOP should opt out and cancel session: %+v optedOut=%v purchases=%d", stopOut, fb.optedOut, len(fb.purchases))
	}
}
