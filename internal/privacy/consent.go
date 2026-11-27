package privacy

import (
	"fmt"
	"strings"

	"github.com/teamthy/meterly/internal/notifications"
)

// Snapshot is the queryable consent ledger for one user.
type Snapshot struct {
	Current []Record `json:"current"`
	History []Record `json:"history"`
}

// SnapshotFrom builds current (transactional from prefs, marketing from the
// latest history row, default withdrawn) plus full history.
func SnapshotFrom(userID string, prefs notifications.Preferences, history []Record) Snapshot {
	latest := map[string]Record{}
	for _, rec := range history {
		latest[rec.Purpose+":"+string(rec.Channel)] = rec
	}
	var current []Record
	for _, ch := range channels {
		stStatus := StatusWithdrawn
		if prefs.Enabled(ch) {
			stStatus = StatusGranted
		}
		current = append(current, Record{
			UserID:  userID,
			Purpose: PurposeTransactional,
			Channel: ch,
			Status:  stStatus,
		})
		mStatus := StatusWithdrawn
		if rec, ok := latest[PurposeMarketing+":"+string(ch)]; ok {
			mStatus = rec.Status
		}
		current = append(current, Record{
			UserID:  userID,
			Purpose: PurposeMarketing,
			Channel: ch,
			Status:  mStatus,
		})
	}
	return Snapshot{Current: current, History: history}
}

func MarketingGranted(history []Record, ch notifications.Channel) bool {
	var latest string
	var at int64
	for _, rec := range history {
		if rec.Purpose != PurposeMarketing || rec.Channel != ch {
			continue
		}
		ts := rec.CreatedAt.UnixNano()
		if ts >= at {
			at = ts
			latest = rec.Status
		}
	}
	return latest == StatusGranted
}

// Allows reports whether a send of this purpose/channel may go out.
func Allows(purpose string, ch notifications.Channel, prefs notifications.Preferences, marketingGranted bool, optedOut bool) bool {
	if optedOut {
		return false
	}
	switch purpose {
	case PurposeTransactional:
		return prefs.Enabled(ch)
	case PurposeMarketing:
		return marketingGranted
	default:
		return false
	}
}

func ValidPurpose(p string) bool {
	return p == PurposeTransactional || p == PurposeMarketing
}

func ParseChannel(s string) (notifications.Channel, bool) {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case string(notifications.SMS):
		return notifications.SMS, true
	case string(notifications.Email):
		return notifications.Email, true
	case string(notifications.WhatsApp):
		return notifications.WhatsApp, true
	default:
		return "", false
	}
}

func Channels() []notifications.Channel { return append([]notifications.Channel(nil), channels...) }

func ValidateWithdraw(purpose, channel string) (string, notifications.Channel, error) {
	p := strings.ToUpper(strings.TrimSpace(purpose))
	if !ValidPurpose(p) {
		return "", "", fmt.Errorf("privacy: purpose must be TRANSACTIONAL or MARKETING")
	}
	ch, ok := ParseChannel(channel)
	if !ok {
		return "", "", fmt.Errorf("privacy: channel must be SMS, EMAIL or WHATSAPP")
	}
	return p, ch, nil
}
