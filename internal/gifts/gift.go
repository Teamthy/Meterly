// Package gifts defines encrypted recipient contact metadata. It does not send
// notifications or establish that a recipient has consented to receive them.
package gifts

import (
	"errors"
	"github.com/google/uuid"
	"regexp"
	"time"
)

var phonePattern = regexp.MustCompile(`^\+[1-9][0-9]{7,14}$`)
var ErrInvalidContact = errors.New("gift contact requires an E.164 phone number and an explicit notification request")

type Gift struct {
	TransactionID         uuid.UUID
	PurchaserID           string
	RecipientName         string
	ContactCiphertext     []byte // encrypted; must never be logged or returned to clients
	ContactKeyVersion     string
	NotificationRequested bool // purchaser request, NOT recipient consent
	CreatedAt             time.Time
}

func Validate(isGift bool, phone string, requested bool) error {
	if !isGift && (phone != "" || requested) {
		return ErrInvalidContact
	}
	if phone == "" && !requested {
		return nil
	}
	if !isGift || !requested || !phonePattern.MatchString(phone) {
		return ErrInvalidContact
	}
	return nil
}
