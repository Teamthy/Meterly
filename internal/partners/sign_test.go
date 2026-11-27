package partners

import (
	"testing"
	"time"
)

func TestSignAndVerifyReplayWindow(t *testing.T) {
	full, _, err := GenerateWebhookSecret()
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"id":"evt","type":"endpoint.ping"}`)
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	sig, err := SignBody([]string{full}, now.Unix(), body)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifySignature([]string{full}, sig, body, now, WebhookReplayWindow); err != nil {
		t.Fatalf("verify now: %v", err)
	}
	if err := VerifySignature([]string{full}, sig, body, now.Add(WebhookReplayWindow+time.Second), WebhookReplayWindow); err == nil {
		t.Fatal("expected replay window rejection")
	}
	if err := VerifySignature([]string{full}, sig, append(body, 'x'), now, WebhookReplayWindow); err == nil {
		t.Fatal("expected body mismatch rejection")
	}
	other, _, err := GenerateWebhookSecret()
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifySignature([]string{other}, sig, body, now, WebhookReplayWindow); err == nil {
		t.Fatal("expected wrong-secret rejection")
	}
}

func TestSignRotationOverlapAcceptsEitherSecret(t *testing.T) {
	oldFull, _, err := GenerateWebhookSecret()
	if err != nil {
		t.Fatal(err)
	}
	newFull, _, err := GenerateWebhookSecret()
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"type":"purchase.vended"}`)
	now := time.Date(2026, 11, 23, 15, 0, 0, 0, time.UTC)
	sig, err := SignBody([]string{newFull, oldFull}, now.Unix(), body)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifySignature([]string{newFull}, sig, body, now, WebhookReplayWindow); err != nil {
		t.Fatalf("new secret: %v", err)
	}
	if err := VerifySignature([]string{oldFull}, sig, body, now, WebhookReplayWindow); err != nil {
		t.Fatalf("old secret during overlap: %v", err)
	}
}
