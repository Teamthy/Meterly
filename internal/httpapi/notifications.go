package httpapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/audit"
	"github.com/teamthy/meterly/internal/auth"
	"github.com/teamthy/meterly/internal/notifications"
	"github.com/teamthy/meterly/internal/notificationservice"
	"github.com/teamthy/meterly/internal/providers"
	"github.com/teamthy/meterly/internal/store"
)

func (s *Server) captureVerifiedContacts(ctx context.Context, uid string, p auth.Principal) error {
	box := s.O.Box()
	if box == nil {
		return nil
	}
	name := notifications.SanitizeDisplayName(p.DisplayName)
	if p.VerifiedEmail == "" && p.VerifiedPhone == "" && name == "" {
		return nil
	}

	existing, err := s.O.Store().Repos().Users().Contacts(ctx, uid)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}

	var (
		upd     notifications.UserContacts
		changed bool
	)
	upd.UserID = uid

	if p.VerifiedEmail != "" {
		if norm, err := notifications.NormalizeEmail(p.VerifiedEmail); err == nil {
			wantHMAC := notifications.DestinationHMAC(box, norm)
			if existing.EmailHMAC != wantHMAC || len(existing.EmailCiphertext) == 0 || existing.EmailKeyVersion != box.KeyVersion {
				ct, hm, masked, err := notifications.SealUserEmail(box, uid, norm)
				if err != nil {
					return err
				}
				upd.EmailCiphertext = ct
				upd.EmailHMAC = hm
				upd.EmailMasked = masked
				upd.EmailKeyVersion = box.KeyVersion
				changed = true
			}
		}
	}
	if p.VerifiedPhone != "" {
		if norm, err := notifications.NormalizePhone(p.VerifiedPhone); err == nil {
			wantHMAC := notifications.DestinationHMAC(box, norm)
			if existing.PhoneHMAC != wantHMAC || len(existing.PhoneCiphertext) == 0 || existing.PhoneKeyVersion != box.KeyVersion {
				ct, hm, masked, err := notifications.SealUserPhone(box, uid, norm)
				if err != nil {
					return err
				}
				upd.PhoneCiphertext = ct
				upd.PhoneHMAC = hm
				upd.PhoneMasked = masked
				upd.PhoneKeyVersion = box.KeyVersion
				changed = true
			}
		}
	}
	if name != "" && existing.DisplayName != name {
		upd.DisplayName = name
		changed = true
	}
	if !changed {
		return nil
	}
	upd.UpdatedAt = s.O.Now()
	return s.O.Store().Repos().Users().UpsertContacts(ctx, upd)
}

func (s *Server) getNotificationPreferences(w http.ResponseWriter, r *http.Request) {
	prefs, err := s.O.Store().Repos().Notifications().GetPreferences(r.Context(), userID(r))
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, prefs)
}

func (s *Server) putNotificationPreferences(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SMS      *bool `json:"sms"`
		Email    *bool `json:"email"`
		WhatsApp *bool `json:"whatsapp"`
	}
	if !decode(r, &body) || body.SMS == nil || body.Email == nil || body.WhatsApp == nil {
		writeErr(w, 400, "INVALID_REQUEST", "sms, email and whatsapp boolean fields are required")
		return
	}
	prefs := notifications.Preferences{
		SMS:      *body.SMS,
		Email:    *body.Email,
		WhatsApp: *body.WhatsApp,
	}
	uid := userID(r)
	uidUUID, _ := uuid.Parse(uid)
	now := time.Now().UTC()
	actor := audit.Actor{
		UserID:        uid,
		Type:          audit.ActorUser,
		CorrelationID: audit.CorrelationFrom(r.Context()),
	}
	err := s.O.Store().InTx(r.Context(), func(rp store.Repos) error {
		if err := rp.Notifications().SetPreferences(r.Context(), uid, prefs, now); err != nil {
			return err
		}
		return rp.Audit().Add(r.Context(), actor.Event("notification_preferences.update", "user", uidUUID, map[string]any{
			"sms":      prefs.SMS,
			"email":    prefs.Email,
			"whatsapp": prefs.WhatsApp,
		}))
	})
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, prefs)
}

func (s *Server) notificationInboundChallenge(w http.ResponseWriter, r *http.Request) {
	if s.Notifications == nil {
		writeErr(w, 404, "PROVIDER_NOT_FOUND", "unknown notification provider")
		return
	}
	q := r.URL.Query()
	challenge, err := s.Notifications.VerifyInboundChallenge(
		r.PathValue("provider"),
		q.Get("hub.mode"),
		q.Get("hub.verify_token"),
		q.Get("hub.challenge"),
	)
	switch {
	case errors.Is(err, notificationservice.ErrProviderNotFound):
		writeErr(w, 404, "PROVIDER_NOT_FOUND", "unknown notification provider")
	case errors.Is(err, providers.ErrBadSignature):
		writeErr(w, 401, "INVALID_SIGNATURE", "webhook verification failed")
	case err != nil:
		writeErr(w, 400, "INVALID_REQUEST", "invalid challenge request")
	default:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(challenge))
	}
}

func (s *Server) notificationInbound(w http.ResponseWriter, r *http.Request) {
	if s.Notifications == nil {
		writeErr(w, 404, "PROVIDER_NOT_FOUND", "unknown notification provider")
		return
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, 400, "INVALID_REQUEST", "could not read body")
		return
	}
	err = s.Notifications.HandleInbound(
		r.Context(),
		r.PathValue("provider"),
		r.Header,
		raw,
		audit.CorrelationFrom(r.Context()),
	)
	switch {
	case errors.Is(err, notificationservice.ErrProviderNotFound):
		writeErr(w, 404, "PROVIDER_NOT_FOUND", "unknown notification provider")
	case errors.Is(err, providers.ErrBadSignature):
		writeErr(w, 401, "INVALID_SIGNATURE", "webhook signature invalid")
	case err != nil:
		writeErr(w, 400, "INVALID_WEBHOOK", "malformed webhook payload")
	default:
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}
