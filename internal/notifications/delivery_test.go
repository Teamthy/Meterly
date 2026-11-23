package notifications

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/secure"
)

func TestDeliveryTransitionsAndBackoff(t *testing.T) {
	now := time.Now().UTC()
	d := Delivery{ID: uuid.New(), Status: StatusPending, CreatedAt: now, UpdatedAt: now}
	if err := d.Transition(StatusSent, now); !errors.Is(err, ErrInvalidStatusTransition) {
		t.Fatalf("pending -> sent must be rejected, got %v", err)
	}
	if err := d.Transition(StatusSending, now); err != nil || d.Status != StatusSending {
		t.Fatalf("pending -> sending: %v", err)
	}
	if err := d.Transition(StatusUnknown, now); err != nil || d.Status != StatusUnknown {
		t.Fatalf("sending -> unknown: %v", err)
	}
	if err := d.Transition(StatusSending, now); err != nil || d.Status != StatusSending {
		t.Fatalf("unknown -> sending: %v", err)
	}
	if err := d.Transition(StatusSent, now); err != nil || !d.Status.Terminal() {
		t.Fatalf("sending -> sent: %v", err)
	}
	if err := d.Transition(StatusPending, now); !errors.Is(err, ErrInvalidStatusTransition) {
		t.Fatalf("terminal -> pending must be rejected: %v", err)
	}

	wantBackoffs := []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute, 1 * time.Hour, 1 * time.Hour}
	for i, want := range wantBackoffs {
		if got := Backoff(i + 1); got != want {
			t.Fatalf("attempt %d: got %s, want %s", i+1, got, want)
		}
	}
}

func TestUserContactEncryptionBindingAndMasking(t *testing.T) {
	box, err := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "v1")
	if err != nil {
		t.Fatal(err)
	}
	uid := uuid.NewString()
	ctEmail, hmacEmail, maskEmail, err := SealUserEmail(box, uid, "  Ada.Lovelace@Example.COM ")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ctEmail), "ada") || strings.Contains(maskEmail, "ada.lovelace") || maskEmail != "a***@example.com" {
		t.Fatalf("email leaked or wrongly masked: %q", maskEmail)
	}
	if hmacEmail != DestinationHMAC(box, "ada.lovelace@example.com") {
		t.Fatal("email HMAC mismatch")
	}
	plainEmail, err := OpenUserEmail(box, uid, ctEmail)
	if err != nil || plainEmail != "ada.lovelace@example.com" {
		t.Fatalf("open email: %q %v", plainEmail, err)
	}
	if _, err := OpenUserEmail(box, uuid.NewString(), ctEmail); err == nil {
		t.Fatal("email ciphertext must be bound to user ID")
	}

	ctPhone, hmacPhone, maskPhone, err := SealUserPhone(box, uid, " +2348012345678 ")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ctPhone), "801234") || strings.Contains(maskPhone, "801234") || !strings.HasSuffix(maskPhone, "5678") {
		t.Fatalf("phone leaked or wrongly masked: %q", maskPhone)
	}
	if hmacPhone != DestinationHMAC(box, "+2348012345678") {
		t.Fatal("phone HMAC mismatch")
	}
	plainPhone, err := OpenUserPhone(box, uid, ctPhone)
	if err != nil || plainPhone != "+2348012345678" {
		t.Fatalf("open phone: %q %v", plainPhone, err)
	}
	if _, err := OpenUserPhone(box, uuid.NewString(), ctPhone); err == nil {
		t.Fatal("phone ciphertext must be bound to user ID")
	}
}

func TestGiftTokenSMSFormatAndI18nFallback(t *testing.T) {
	c, ver, err := RenderGiftTokenSMS(LocaleYO, 500000, "*******5678", "12345678901234567890", "Ada\n")
	if err != nil {
		t.Fatal(err)
	}
	want := "Meterly: ₦5,000 electricity for meter ****5678. Token: 1234-5678-9012-3456-7890. Sent by Ada. Reply STOP to opt out."
	if c.Body != want || ver != CurrentTemplateVersion {
		t.Fatalf("got %q (%s), want %q", c.Body, ver, want)
	}

	// Non-English locales fall back cleanly to English until translations land.
	for _, loc := range []string{LocaleEN, LocaleYO, LocaleIG, LocaleHA, LocalePCM, "fr"} {
		got, v, err := RenderPurchaserTemplate(TemplatePaymentConfirmed, loc)
		if err != nil || v != CurrentTemplateVersion || !strings.Contains(got.Body, "tracked separately") {
			t.Fatalf("locale %q: %+v %s %v", loc, got, v, err)
		}
	}

	// OperatorView never serializes DestinationHMAC.
	d := Delivery{
		ID: uuid.New(), RecipientKind: RecipientGift, Channel: SMS,
		DestinationHMAC: "secret-hmac-value", TemplateID: TemplateGiftTokenSMS,
		TemplateVersion: CurrentTemplateVersion, Status: StatusSent, Attempts: 1,
	}
	raw, _ := json.Marshal(d.RedactedView())
	if strings.Contains(string(raw), "secret-hmac-value") {
		t.Fatalf("operator view leaked hmac: %s", string(raw))
	}
}

func TestIsStopKeyword(t *testing.T) {
	for _, yes := range []string{"STOP", " stop ", "Stop!", "stopall", "UNSUBSCRIBE", "opt out"} {
		if !IsStopKeyword(yes) {
			t.Errorf("expected STOP keyword for %q", yes)
		}
	}
	for _, no := range []string{"", "HELLO", "STOP BY LATER", "BUY 5000"} {
		if IsStopKeyword(no) {
			t.Errorf("unexpected STOP keyword for %q", no)
		}
	}
}
