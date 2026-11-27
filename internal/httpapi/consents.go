package httpapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/audit"
	"github.com/teamthy/meterly/internal/notifications"
	"github.com/teamthy/meterly/internal/privacy"
	"github.com/teamthy/meterly/internal/store"
)

type consentView struct {
	Purpose string    `json:"purpose"`
	Channel string    `json:"channel"`
	Status  string    `json:"status"`
	Source  string    `json:"source,omitempty"`
	At      time.Time `json:"at,omitempty"`
}

func consentJSON(rec privacy.Record) consentView {
	v := consentView{Purpose: rec.Purpose, Channel: string(rec.Channel), Status: rec.Status, Source: rec.Source}
	if !rec.CreatedAt.IsZero() {
		v.At = rec.CreatedAt.UTC()
	}
	return v
}

func (s *Server) getConsents(w http.ResponseWriter, r *http.Request) {
	uid := userID(r)
	prefs, err := s.O.Store().Repos().Notifications().GetPreferences(r.Context(), uid)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	history, err := s.O.Store().Repos().Privacy().ListConsents(r.Context(), uid)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	snap := privacy.SnapshotFrom(uid, prefs, history)
	cur := make([]consentView, 0, len(snap.Current))
	for _, rec := range snap.Current {
		cur = append(cur, consentJSON(rec))
	}
	hist := make([]consentView, 0, len(snap.History))
	for _, rec := range snap.History {
		hist = append(hist, consentJSON(rec))
	}
	writeJSON(w, 200, map[string]any{"current": cur, "history": hist})
}

func (s *Server) withdrawConsent(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Purpose string `json:"purpose"`
		Channel string `json:"channel"`
	}
	if !decode(r, &body) {
		writeErr(w, 400, "INVALID_REQUEST", "purpose and channel are required")
		return
	}
	purpose, ch, err := privacy.ValidateWithdraw(body.Purpose, body.Channel)
	if err != nil {
		writeErr(w, 400, "INVALID_REQUEST", err.Error())
		return
	}
	uid := userID(r)
	uidUUID, err := uuid.Parse(uid)
	if err != nil {
		writeErr(w, 401, "UNAUTHENTICATED", "not authenticated")
		return
	}
	now := time.Now().UTC()
	if s.O != nil && s.O.Now != nil {
		now = s.O.Now()
	}
	actor := audit.Actor{UserID: uid, Type: audit.ActorUser, CorrelationID: audit.CorrelationFrom(r.Context())}
	err = s.O.Store().InTx(r.Context(), func(rp store.Repos) error {
		if err := rp.Privacy().AppendConsent(r.Context(), privacy.Record{
			ID: uuid.New(), UserID: uid, Purpose: purpose, Channel: ch,
			Status: privacy.StatusWithdrawn, Source: privacy.SourceUser, CreatedAt: now,
		}); err != nil {
			return err
		}
		if purpose == privacy.PurposeTransactional {
			prefs, err := rp.Notifications().GetPreferences(r.Context(), uid)
			if err != nil {
				return err
			}
			switch ch {
			case notifications.SMS:
				prefs.SMS = false
			case notifications.Email:
				prefs.Email = false
			case notifications.WhatsApp:
				prefs.WhatsApp = false
			}
			if err := rp.Notifications().SetPreferences(r.Context(), uid, prefs, now); err != nil {
				return err
			}
			contacts, err := rp.Users().Contacts(r.Context(), uid)
			if err == nil {
				hmac := ""
				switch ch {
				case notifications.SMS, notifications.WhatsApp:
					hmac = contacts.PhoneHMAC
				case notifications.Email:
					hmac = contacts.EmailHMAC
				}
				if hmac != "" {
					if _, err := rp.Notifications().AddOptOut(r.Context(), notifications.OptOut{
						DestinationHMAC: hmac, Channel: ch, CreatedAt: now,
					}); err != nil {
						return err
					}
				}
			} else if !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
		return rp.Audit().Add(r.Context(), actor.Event("consent.withdraw", "user", uidUUID, map[string]any{
			"purpose": purpose, "channel": string(ch),
		}))
	})
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	s.getConsents(w, r)
}
