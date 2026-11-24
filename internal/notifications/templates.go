package notifications

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/events"
	tx "github.com/teamthy/meterly/internal/transactions"
)

// Supported and planned locale codes. English is implemented; Yoruba, Igbo,
// Hausa and Nigerian Pidgin fall back cleanly to English until translated.
const (
	LocaleEN  = "en"
	LocaleYO  = "yo"
	LocaleIG  = "ig"
	LocaleHA  = "ha"
	LocalePCM = "pcm"
)

const (
	TemplatePaymentConfirmed = "payment_confirmed"
	TemplateTokenAvailable   = "token_available"
	TemplateRemoteConfirmed  = "remote_delivery_confirmed"
	TemplateRemoteFailed     = "remote_delivery_failed"
	TemplateRemoteUnknown    = "remote_delivery_unknown"
	TemplateRefundCompleted  = "refund_completed"
	TemplateGiftTokenSMS     = "gift_token_sms"
	TemplatePotSettled       = "pot_settled"

	CurrentTemplateVersion = "v1"
)

func IsTokenBearing(templateID string) bool {
	return templateID == TemplateGiftTokenSMS
}

type TemplateSpec struct {
	ID                   string
	Version              string
	WhatsAppTemplateName string
}

type localeEntry struct {
	Subject string
	Body    string
}

var purchaserCatalog = map[string]map[string]localeEntry{
	LocaleEN: {
		TemplatePaymentConfirmed: {
			Subject: "Payment confirmed",
			Body:    "Your payment was confirmed. Electricity vending and delivery are tracked separately in Meterly.",
		},
		TemplateTokenAvailable: {
			Subject: "Electricity token available",
			Body:    "Your electricity token is available in Meterly. The token works only on the meter it was bought for; sharing it does not move credit between meters.",
		},
		TemplateRemoteConfirmed: {
			Subject: "Remote delivery confirmed",
			Body:    "Your meter acknowledged the remote electricity credit. View the transaction in Meterly.",
		},
		TemplateRemoteFailed: {
			Subject: "Remote delivery failed",
			Body:    "Remote delivery failed. Check Meterly for the transaction status and support options.",
		},
		TemplateRemoteUnknown: {
			Subject: "Remote delivery status unknown",
			Body:    "We could not confirm remote delivery at the time of this update. Check Meterly for the latest status.",
		},
		TemplateRefundCompleted: {
			Subject: "Refund completed",
			Body:    "Your refund has been confirmed by the payment provider. Check Meterly for the transaction details.",
		},
		TemplatePotSettled: {
			Subject: "Shared electricity pot settled",
			Body:    "The shared Meterly electricity pot you contributed to is funded and electricity has been delivered. Reply STOP to opt out.",
		},
	},
}

var whatsAppTemplateNames = map[string]string{
	TemplatePaymentConfirmed: "meterly_payment_confirmed_v1",
	TemplateTokenAvailable:   "meterly_token_available_v1",
	TemplateRemoteConfirmed:  "meterly_remote_confirmed_v1",
	TemplateRemoteFailed:     "meterly_remote_failed_v1",
	TemplateRemoteUnknown:    "meterly_remote_unknown_v1",
	TemplateRefundCompleted:  "meterly_refund_completed_v1",
}

// NormalizeLocale returns a supported locale tag, defaulting to LocaleEN.
func NormalizeLocale(locale string) string {
	switch strings.ToLower(strings.TrimSpace(locale)) {
	case LocaleEN, LocaleYO, LocaleIG, LocaleHA, LocalePCM:
		return strings.ToLower(strings.TrimSpace(locale))
	default:
		return LocaleEN
	}
}

// TemplateSpecFor maps an allowlisted outbox event and delivery method to its
// purchaser notification template identifier. ok=false means no purchaser
// notification should be sent for this event.
func TemplateSpecFor(e events.Event, method tx.DeliveryMethod) (spec TemplateSpec, ok bool, err error) {
	if e.ID == uuid.Nil || e.AggregateID == uuid.Nil || e.AggregateType != "electricity_transaction" {
		return TemplateSpec{}, false, errors.New("invalid notification event")
	}
	var id string
	switch e.Type {
	case "payment.confirmed":
		id = TemplatePaymentConfirmed
	case "token.available":
		if method != tx.MethodToken {
			return TemplateSpec{}, false, errors.New("token event requires token delivery")
		}
		id = TemplateTokenAvailable
	case "electricity.delivery.confirmed":
		if method == tx.MethodToken {
			return TemplateSpec{}, false, nil
		}
		if method != tx.MethodRemote {
			return TemplateSpec{}, false, errors.New("unknown delivery method")
		}
		id = TemplateRemoteConfirmed
	case "electricity.delivery.failed":
		if method != tx.MethodRemote {
			return TemplateSpec{}, false, errors.New("remote delivery event requires remote method")
		}
		id = TemplateRemoteFailed
	case "electricity.delivery.unknown":
		if method != tx.MethodRemote {
			return TemplateSpec{}, false, errors.New("remote delivery event requires remote method")
		}
		id = TemplateRemoteUnknown
	case "electricity.refund.completed":
		id = TemplateRefundCompleted
	default:
		return TemplateSpec{}, false, nil
	}
	return TemplateSpec{
		ID:                   id,
		Version:              CurrentTemplateVersion,
		WhatsAppTemplateName: whatsAppTemplateNames[id],
	}, true, nil
}

// RenderPurchaserTemplate renders a non-token purchaser template in the
// requested locale, falling back to English when a translation is not yet
// registered.
func RenderPurchaserTemplate(templateID, locale string) (Content, string, error) {
	loc := NormalizeLocale(locale)
	if byID, ok := purchaserCatalog[loc]; ok {
		if entry, ok := byID[templateID]; ok {
			return Content{Subject: entry.Subject, Body: entry.Body}, CurrentTemplateVersion, nil
		}
	}
	if entry, ok := purchaserCatalog[LocaleEN][templateID]; ok {
		return Content{Subject: entry.Subject, Body: entry.Body}, CurrentTemplateVersion, nil
	}
	return Content{}, "", fmt.Errorf("notifications: unknown template %q", templateID)
}

// WhatsAppTemplateName returns the pre-approved WhatsApp template name for a
// Meterly template ID.
func WhatsAppTemplateName(templateID string) string {
	return whatsAppTemplateNames[templateID]
}

// FormatNairaMinor formats kobo as Naira (e.g. 500000 -> "₦5,000").
func FormatNairaMinor(minor int64) string {
	neg := minor < 0
	if neg {
		minor = -minor
	}
	naira := minor / 100
	kobo := minor % 100
	intStr := formatThousands(naira)
	prefix := "₦"
	if neg {
		prefix = "-₦"
	}
	if kobo == 0 {
		return prefix + intStr
	}
	return fmt.Sprintf("%s%s.%02d", prefix, intStr, kobo)
}

func formatThousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	rem := len(s) % 3
	if rem > 0 {
		out = append(out, s[:rem]...)
		if len(s) > rem {
			out = append(out, ',')
		}
	}
	for i := rem; i < len(s); i += 3 {
		out = append(out, s[i:i+3]...)
		if i+3 < len(s) {
			out = append(out, ',')
		}
	}
	return string(out)
}

// FormatSTSToken groups a 20-digit STS token as 1234-5678-9012-3456-7890.
func FormatSTSToken(raw string) (string, error) {
	var digits strings.Builder
	for _, r := range raw {
		switch {
		case r >= '0' && r <= '9':
			digits.WriteRune(r)
		case r == '-' || r == ' ':
		default:
			return "", errors.New("notifications: STS token contains non-digit characters")
		}
	}
	d := digits.String()
	if len(d) != 20 {
		return "", errors.New("notifications: STS token must have 20 digits")
	}
	return d[0:4] + "-" + d[4:8] + "-" + d[8:12] + "-" + d[12:16] + "-" + d[16:20], nil
}

// ShortMaskedMeter formats the meter suffix as ****5678 regardless of full meter length.
func ShortMaskedMeter(maskedOrRaw string) string {
	s := strings.TrimSpace(maskedOrRaw)
	if len(s) <= 4 {
		return "****"
	}
	suffix := s[len(s)-4:]
	for _, r := range suffix {
		if r < '0' || r > '9' {
			return "****"
		}
	}
	return "****" + suffix
}

// RenderGiftTokenSMS formats the token-bearing SMS for a gift recipient.
// Format: "Meterly: ₦5,000 electricity for meter ****5678. Token: 1234-5678-9012-3456-7890. Sent by Ada. Reply STOP to opt out."
func RenderGiftTokenSMS(locale string, amountMinor int64, maskedMeter, rawToken, senderDisplayName string) (Content, string, error) {
	if amountMinor <= 0 {
		return Content{}, "", errors.New("notifications: gift amount must be positive")
	}
	tok, err := FormatSTSToken(rawToken)
	if err != nil {
		return Content{}, "", err
	}
	meter := ShortMaskedMeter(maskedMeter)
	sender := SanitizeDisplayName(senderDisplayName)
	if sender == "" {
		sender = "a Meterly user"
	}
	_ = NormalizeLocale(locale) // reserved for localized gift templates
	body := fmt.Sprintf(
		"Meterly: %s electricity for meter %s. Token: %s. Sent by %s. Reply STOP to opt out.",
		FormatNairaMinor(amountMinor), meter, tok, sender,
	)
	return Content{Body: body}, CurrentTemplateVersion, nil
}
