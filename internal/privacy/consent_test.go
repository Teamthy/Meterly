package privacy_test

import (
	"testing"
	"time"

	"github.com/teamthy/meterly/internal/notifications"
	"github.com/teamthy/meterly/internal/privacy"
)

func TestSnapshotAndAllows(t *testing.T) {
	prefs := notifications.DefaultPreferences()
	now := time.Now().UTC()
	hist := []privacy.Record{
		{Purpose: privacy.PurposeMarketing, Channel: notifications.Email, Status: privacy.StatusGranted, CreatedAt: now.Add(-time.Hour)},
		{Purpose: privacy.PurposeMarketing, Channel: notifications.Email, Status: privacy.StatusWithdrawn, CreatedAt: now},
	}
	snap := privacy.SnapshotFrom("u1", prefs, hist)
	if len(snap.Current) != 6 {
		t.Fatalf("want 6 current cells, got %d", len(snap.Current))
	}
	if privacy.MarketingGranted(hist, notifications.Email) {
		t.Fatal("latest marketing email is withdrawn")
	}
	if !privacy.Allows(privacy.PurposeTransactional, notifications.SMS, prefs, false, false) {
		t.Fatal("default transactional SMS is granted")
	}
	prefs.SMS = false
	if privacy.Allows(privacy.PurposeTransactional, notifications.SMS, prefs, false, false) {
		t.Fatal("withdrawn transactional SMS must block send")
	}
	if privacy.Allows(privacy.PurposeTransactional, notifications.Email, prefs, false, true) {
		t.Fatal("opt-out must block send")
	}
	if privacy.Allows(privacy.PurposeMarketing, notifications.SMS, prefs, false, false) {
		t.Fatal("marketing default is withdrawn")
	}
	if !privacy.Allows(privacy.PurposeMarketing, notifications.SMS, prefs, true, false) {
		t.Fatal("granted marketing SMS should allow")
	}
}

func TestValidateWithdraw(t *testing.T) {
	if _, _, err := privacy.ValidateWithdraw("nope", "SMS"); err == nil {
		t.Fatal("want purpose error")
	}
	p, ch, err := privacy.ValidateWithdraw("transactional", "sms")
	if err != nil || p != privacy.PurposeTransactional || ch != notifications.SMS {
		t.Fatalf("got %s %s %v", p, ch, err)
	}
}
