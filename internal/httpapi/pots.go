package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/auth"
	"github.com/teamthy/meterly/internal/notifications"
	"github.com/teamthy/meterly/internal/orchestrator"
	"github.com/teamthy/meterly/internal/payrequests"
	"github.com/teamthy/meterly/internal/pots"
)

func (s *Server) requireSplitMeter(w http.ResponseWriter) bool {
	if s.SplitMeterDisabled {
		writeErr(w, http.StatusNotFound, "FEATURE_DISABLED", "split-the-meter funding pots are not enabled")
		return false
	}
	return true
}

func (s *Server) publicPotLimited(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setPublicPayHeaders(w)
		if !s.requireSplitMeter(w) {
			return
		}
		if ok, wait := s.publicPay.Allow(s.clientIP(r)); !ok {
			tooMany(w, wait)
			return
		}
		h(w, r)
	}
}

func (s *Server) createPot(w http.ResponseWriter, r *http.Request) {
	setPublicPayHeaders(w)
	if !s.requireSplitMeter(w) {
		return
	}
	var req struct {
		MeterID         uuid.UUID `json:"meter_id"`
		TargetMinor     int64     `json:"target_minor"`
		SuggestedShares int       `json:"suggested_shares"`
		Deadline        string    `json:"deadline"`
		ExpiresInHours  int       `json:"expires_in_hours"`
		Note            string    `json:"note"`
	}
	if !decode(r, &req) || req.MeterID == uuid.Nil || req.TargetMinor <= 0 {
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "meter_id and positive target_minor are required")
		return
	}
	if req.ExpiresInHours < 0 {
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "expires_in_hours cannot be negative")
		return
	}

	now := s.O.Now().UTC()
	var deadline time.Time
	switch {
	case strings.TrimSpace(req.Deadline) != "":
		parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(req.Deadline))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "deadline must be an RFC3339 timestamp")
			return
		}
		deadline = parsed.UTC()
	case req.ExpiresInHours > 0:
		deadline = now.Add(time.Duration(req.ExpiresInHours) * time.Hour)
	default:
		deadline = now.Add(pots.DefaultMaxHold)
	}

	created, err := s.O.CreatePot(r.Context(), orchestrator.CreatePotParams{
		OwnerID:         userID(r),
		MeterID:         req.MeterID,
		TargetMinor:     req.TargetMinor,
		SuggestedShares: req.SuggestedShares,
		Deadline:        deadline,
		Note:            req.Note,
		PublicBaseURL:   s.publicBaseURL(),
	})
	if err != nil {
		s.mapPotErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) getPot(w http.ResponseWriter, r *http.Request) {
	setPublicPayHeaders(w)
	if !s.requireSplitMeter(w) {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "not found")
		return
	}
	v, err := s.O.GetPotForOwner(r.Context(), userID(r), id)
	if err != nil {
		s.mapPotErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) cancelPot(w http.ResponseWriter, r *http.Request) {
	setPublicPayHeaders(w)
	if !s.requireSplitMeter(w) {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "not found")
		return
	}
	v, err := s.O.CancelPot(r.Context(), userID(r), id)
	if err != nil {
		s.mapPotErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) getPublicPot(w http.ResponseWriter, r *http.Request) {
	v, err := s.O.GetPublicPot(r.Context(), r.PathValue("code"))
	if err != nil {
		s.mapPotErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) contributeToPot(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Idempotency-Key")
	if len(key) < 16 || len(key) > 128 {
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "Idempotency-Key header (16–128 chars) is required")
		return
	}

	rawCode := r.PathValue("code")
	codeHMAC, ok := pots.CodeHMAC(s.O.Box(), rawCode)
	if !ok {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "not found")
		return
	}
	if allowed, wait := s.publicLink.Allow(codeHMAC); !allowed {
		tooMany(w, wait)
		return
	}

	// Optional bearer authentication: if Authorization is present, verify it and
	// attach the contributor's internal user UUID; otherwise treat as guest.
	var contributorUID string
	if strings.TrimSpace(r.Header.Get("Authorization")) != "" && s.Auth != nil {
		p, err := s.Auth.Authenticate(r)
		if err != nil {
			if ok, wait := s.authFails.Allow(s.clientIP(r)); !ok {
				tooMany(w, wait)
				return
			}
			writeErr(w, http.StatusUnauthorized, "UNAUTHENTICATED", "invalid authentication token")
			return
		}
		if !p.Has(auth.RoleCustomer) {
			writeErr(w, http.StatusForbidden, "FORBIDDEN", "insufficient role")
			return
		}
		uid, err := s.O.Store().Repos().Users().EnsureByExternalID(r.Context(), p.Subject)
		if err != nil {
			s.mapErr(w, r, err)
			return
		}
		contributorUID = uid
	}

	var req struct {
		AmountMinor int64  `json:"amount_minor"`
		Email       string `json:"email"`
		Name        string `json:"name"`
		Phone       string `json:"phone"`
		OptInSMS    bool   `json:"opt_in_sms"`
	}
	if !decode(r, &req) || req.AmountMinor <= 0 {
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "positive amount_minor is required")
		return
	}
	if contributorUID == "" && !validEmail(req.Email) {
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "a valid email is required")
		return
	}

	res, _, err := s.O.ContributeToPot(r.Context(), orchestrator.ContributePotRequest{
		Code:              rawCode,
		AmountMinor:       req.AmountMinor,
		ContributorUserID: contributorUID,
		Email:             req.Email,
		Name:              req.Name,
		Phone:             req.Phone,
		OptInSMS:          req.OptInSMS,
		IdempotencyKey:    key,
		IP:                s.clientIP(r),
		UserAgent:         r.UserAgent(),
	})
	if err != nil {
		s.mapPotErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, res)
}

func (s *Server) mapPotErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, orchestrator.ErrPotNotFound):
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "not found")
	case errors.Is(err, orchestrator.ErrPotExpired):
		writeErr(w, http.StatusConflict, "POT_EXPIRED", err.Error())
	case errors.Is(err, orchestrator.ErrPotCancelled):
		writeErr(w, http.StatusConflict, "POT_CANCELLED", err.Error())
	case errors.Is(err, orchestrator.ErrPotAlreadyFunded):
		writeErr(w, http.StatusConflict, "POT_ALREADY_FUNDED", err.Error())
	case errors.Is(err, orchestrator.ErrPotClosed):
		writeErr(w, http.StatusConflict, "POT_CLOSED", err.Error())
	case errors.Is(err, orchestrator.ErrPotNotCancellable):
		writeErr(w, http.StatusConflict, "POT_NOT_CANCELLABLE", err.Error())
	case errors.Is(err, orchestrator.ErrPotUnusable):
		writeErr(w, http.StatusConflict, "POT_UNUSABLE", err.Error())
	case errors.Is(err, pots.ErrInvalidDeadline),
		errors.Is(err, pots.ErrInvalidShares),
		errors.Is(err, payrequests.ErrInvalidNote),
		errors.Is(err, payrequests.ErrInvalidPayerName),
		errors.Is(err, notifications.ErrInvalidEmail),
		errors.Is(err, notifications.ErrInvalidPhone):
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
	default:
		s.mapErr(w, r, err)
	}
}
