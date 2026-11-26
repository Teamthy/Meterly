package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/identity"
	"github.com/teamthy/meterly/internal/notifications"
)

func (s *Server) identityRoutes(mux *patternMux, user, write func(http.HandlerFunc) http.Handler) {
	mux.HandleFunc("POST /v1/auth/passkeys/register/begin", s.passkeyRegisterBegin)
	mux.HandleFunc("POST /v1/auth/passkeys/register/finish", s.passkeyRegisterFinish)
	mux.HandleFunc("POST /v1/auth/passkeys/login/begin", s.passkeyLoginBegin)
	mux.HandleFunc("POST /v1/auth/passkeys/login/finish", s.passkeyLoginFinish)
	mux.HandleFunc("POST /v1/auth/logout", s.logout)
	mux.Handle("GET /v1/me", user(s.getMe))
	mux.Handle("PATCH /v1/me", write(s.patchMe))
	mux.Handle("GET /v1/me/settings", user(s.getSettings))
	mux.Handle("PATCH /v1/me/settings", write(s.patchSettings))
	mux.Handle("GET /v1/me/onboarding", user(s.getOnboarding))
	mux.Handle("POST /v1/me/onboarding/advance", write(s.advanceOnboarding))
	mux.Handle("GET /v1/me/passkeys", user(s.listPasskeys))
	mux.Handle("DELETE /v1/me/passkeys/{id}", write(s.deletePasskey))
}

func (s *Server) requireIdentity(w http.ResponseWriter) bool {
	if s.Identity == nil {
		writeErr(w, 404, "NOT_FOUND", "passkeys are not configured")
		return false
	}
	return true
}

func requestOrigin(r *http.Request, bodyOrigin string) string {
	if o := strings.TrimSpace(bodyOrigin); o != "" {
		return o
	}
	if o := strings.TrimSpace(r.Header.Get("Origin")); o != "" {
		return o
	}
	return ""
}

func decodeJSON(r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	return dec.Decode(v) == nil
}

func (s *Server) optionalUserID(r *http.Request) string {
	if s.Auth == nil {
		return ""
	}
	p, err := s.Auth.Authenticate(r)
	if err != nil {
		return ""
	}
	uid, err := s.O.Store().Repos().Users().EnsureByExternalID(r.Context(), p.Subject)
	if err != nil {
		return ""
	}
	return uid
}

func (s *Server) bearerToken(r *http.Request) string {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return ""
	}
	return strings.TrimSpace(tok)
}

func (s *Server) passkeyRegisterBegin(w http.ResponseWriter, r *http.Request) {
	if !s.requireIdentity(w) {
		return
	}
	var body struct {
		DisplayName string `json:"display_name"`
		Origin      string `json:"origin"`
	}
	_ = decodeJSON(r, &body)
	origin := requestOrigin(r, body.Origin)
	opt, _, err := s.Identity.BeginRegister(r.Context(), origin, body.DisplayName, s.optionalUserID(r))
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, opt)
}

func (s *Server) passkeyRegisterFinish(w http.ResponseWriter, r *http.Request) {
	if !s.requireIdentity(w) {
		return
	}
	var body struct {
		Origin     string                       `json:"origin"`
		Name       string                       `json:"name"`
		Credential identity.AttestationResponse `json:"credential"`
		identity.AttestationResponse
	}
	if !decodeJSON(r, &body) {
		writeErr(w, 400, "INVALID_REQUEST", "invalid request body")
		return
	}
	att := body.Credential
	if att.ClientDataJSON == "" {
		att = body.AttestationResponse
	}
	token, sess, err := s.Identity.FinishRegister(r.Context(), requestOrigin(r, body.Origin), att, body.Name)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"token":      token,
		"expires_at": sess.ExpiresAt,
		"user_id":    sess.UserID,
	})
}

func (s *Server) passkeyLoginBegin(w http.ResponseWriter, r *http.Request) {
	if !s.requireIdentity(w) {
		return
	}
	var body struct {
		Origin string `json:"origin"`
	}
	_ = decodeJSON(r, &body)
	opt, err := s.Identity.BeginLogin(r.Context(), requestOrigin(r, body.Origin))
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, opt)
}

func (s *Server) passkeyLoginFinish(w http.ResponseWriter, r *http.Request) {
	if !s.requireIdentity(w) {
		return
	}
	var body struct {
		Origin     string                     `json:"origin"`
		Credential identity.AssertionResponse `json:"credential"`
		identity.AssertionResponse
	}
	if !decodeJSON(r, &body) {
		writeErr(w, 400, "INVALID_REQUEST", "invalid request body")
		return
	}
	as := body.Credential
	if as.ClientDataJSON == "" {
		as = body.AssertionResponse
	}
	token, sess, err := s.Identity.FinishLogin(r.Context(), requestOrigin(r, body.Origin), as)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"token":      token,
		"expires_at": sess.ExpiresAt,
		"user_id":    sess.UserID,
	})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if s.Identity == nil {
		writeJSON(w, 200, map[string]any{"logged_out": true})
		return
	}
	tok := s.bearerToken(r)
	if strings.HasPrefix(tok, identity.SessionPrefix) {
		_ = s.Identity.Logout(r.Context(), tok)
	}
	writeJSON(w, 200, map[string]any{"logged_out": true})
}

func (s *Server) getMe(w http.ResponseWriter, r *http.Request) {
	p, err := s.O.Store().Repos().Users().Profile(r.Context(), userID(r))
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	passkeys := 0
	if s.Identity != nil {
		if list, err := s.Identity.ListPasskeys(r.Context(), userID(r)); err == nil {
			passkeys = len(list)
		}
	}
	writeJSON(w, 200, map[string]any{"profile": p, "passkey_count": passkeys})
}

func (s *Server) patchMe(w http.ResponseWriter, r *http.Request) {
	p, err := s.applyProfilePatch(r)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, p)
}

func (s *Server) applyProfilePatch(r *http.Request) (identity.Profile, error) {
	var body struct {
		DisplayName *string `json:"display_name"`
		Email       *string `json:"email"`
		Phone       *string `json:"phone"`
	}
	if !decodeJSON(r, &body) {
		return identity.Profile{}, errInvalidJSON
	}
	return s.saveProfileFields(r, userID(r), body.DisplayName, body.Email, body.Phone)
}

func (s *Server) saveProfileFields(r *http.Request, uid string, displayName, email, phone *string) (identity.Profile, error) {
	box := s.O.Box()
	var upd notifications.UserContacts
	upd.UserID = uid
	if displayName != nil {
		name := notifications.SanitizeDisplayName(*displayName)
		if err := s.O.Store().Repos().Users().SetDisplayName(r.Context(), uid, name); err != nil {
			return identity.Profile{}, err
		}
		upd.DisplayName = name
	}
	if email != nil && strings.TrimSpace(*email) != "" {
		norm, err := notifications.NormalizeEmail(*email)
		if err != nil {
			return identity.Profile{}, err
		}
		ct, hm, masked, err := notifications.SealUserEmail(box, uid, norm)
		if err != nil {
			return identity.Profile{}, err
		}
		upd.EmailCiphertext, upd.EmailHMAC, upd.EmailMasked, upd.EmailKeyVersion = ct, hm, masked, box.KeyVersion
	}
	if phone != nil && strings.TrimSpace(*phone) != "" {
		norm, err := notifications.NormalizePhone(*phone)
		if err != nil {
			return identity.Profile{}, err
		}
		ct, hm, masked, err := notifications.SealUserPhone(box, uid, norm)
		if err != nil {
			return identity.Profile{}, err
		}
		upd.PhoneCiphertext, upd.PhoneHMAC, upd.PhoneMasked, upd.PhoneKeyVersion = ct, hm, masked, box.KeyVersion
	}
	if upd.DisplayName != "" || len(upd.EmailCiphertext) > 0 || len(upd.PhoneCiphertext) > 0 {
		upd.UpdatedAt = time.Now().UTC()
		if err := s.O.Store().Repos().Users().UpsertContacts(r.Context(), upd); err != nil {
			return identity.Profile{}, err
		}
	}
	return s.O.Store().Repos().Users().Profile(r.Context(), uid)
}

var errInvalidJSON = errors.New("invalid request body")

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	p, err := s.O.Store().Repos().Users().Profile(r.Context(), userID(r))
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	prefs, err := s.O.Store().Repos().Notifications().GetPreferences(r.Context(), userID(r))
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"profile": p, "notifications": prefs})
}

func (s *Server) patchSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DisplayName *string `json:"display_name"`
		Email       *string `json:"email"`
		Phone       *string `json:"phone"`
		SMS         *bool   `json:"sms"`
		EmailOn     *bool   `json:"email_enabled"`
		WhatsApp    *bool   `json:"whatsapp"`
	}
	if !decodeJSON(r, &body) {
		writeErr(w, 400, "INVALID_REQUEST", "invalid request body")
		return
	}
	uid := userID(r)
	if body.DisplayName != nil || (body.Email != nil && strings.TrimSpace(*body.Email) != "") || (body.Phone != nil && strings.TrimSpace(*body.Phone) != "") {
		if _, err := s.saveProfileFields(r, uid, body.DisplayName, body.Email, body.Phone); err != nil {
			s.mapErr(w, r, err)
			return
		}
	}
	if body.SMS != nil && body.EmailOn != nil && body.WhatsApp != nil {
		prefs := notifications.Preferences{SMS: *body.SMS, Email: *body.EmailOn, WhatsApp: *body.WhatsApp}
		if err := s.O.Store().Repos().Notifications().SetPreferences(r.Context(), uid, prefs, time.Now().UTC()); err != nil {
			s.mapErr(w, r, err)
			return
		}
	}
	p, err := s.O.Store().Repos().Users().Profile(r.Context(), uid)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	prefs, err := s.O.Store().Repos().Notifications().GetPreferences(r.Context(), uid)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"profile": p, "notifications": prefs})
}

func (s *Server) getOnboarding(w http.ResponseWriter, r *http.Request) {
	p, err := s.O.Store().Repos().Users().Profile(r.Context(), userID(r))
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"step":         p.OnboardingStep,
		"steps":        identity.Steps,
		"completed_at": p.OnboardingCompletedAt,
	})
}

func (s *Server) advanceOnboarding(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Step string `json:"step"`
	}
	if !decodeJSON(r, &body) || !identity.ValidStep(body.Step) {
		writeErr(w, 400, "INVALID_REQUEST", "step is required")
		return
	}
	uid := userID(r)
	p, err := s.O.Store().Repos().Users().Profile(r.Context(), uid)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	if !identity.CanAdvance(p.OnboardingStep, body.Step) {
		s.mapErr(w, r, identity.ErrStepOrder)
		return
	}
	var done *time.Time
	if body.Step == identity.StepDone {
		t := time.Now().UTC()
		done = &t
	}
	if err := s.O.Store().Repos().Users().SetOnboarding(r.Context(), uid, body.Step, done); err != nil {
		s.mapErr(w, r, err)
		return
	}
	p, err = s.O.Store().Repos().Users().Profile(r.Context(), uid)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"step":         p.OnboardingStep,
		"steps":        identity.Steps,
		"completed_at": p.OnboardingCompletedAt,
	})
}

func (s *Server) listPasskeys(w http.ResponseWriter, r *http.Request) {
	if !s.requireIdentity(w) {
		return
	}
	list, err := s.Identity.ListPasskeys(r.Context(), userID(r))
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	if list == nil {
		list = []identity.CredentialView{}
	}
	writeJSON(w, 200, map[string]any{"data": list})
}

func (s *Server) deletePasskey(w http.ResponseWriter, r *http.Request) {
	if !s.requireIdentity(w) {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, "NOT_FOUND", "passkey not found")
		return
	}
	if err := s.Identity.DeletePasskey(r.Context(), userID(r), id); err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": true})
}
