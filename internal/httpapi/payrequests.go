package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/notifications"
	"github.com/teamthy/meterly/internal/orchestrator"
	"github.com/teamthy/meterly/internal/payrequests"
)

func setPublicPayHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

func (s *Server) publicBaseURL() string {
	if u := strings.TrimRight(strings.TrimSpace(s.PublicBaseURL), "/"); u != "" {
		return u
	}
	return "https://meterly.ng"
}

func (s *Server) requirePayForMe(w http.ResponseWriter) bool {
	if s.PayForMeDisabled {
		writeErr(w, http.StatusNotFound, "FEATURE_DISABLED", "pay-for-me links are not enabled")
		return false
	}
	return true
}

func (s *Server) publicPayLimited(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setPublicPayHeaders(w)
		if !s.requirePayForMe(w) {
			return
		}
		if ok, wait := s.publicPay.Allow(s.clientIP(r)); !ok {
			tooMany(w, wait)
			return
		}
		h(w, r)
	}
}

func (s *Server) createPaymentRequest(w http.ResponseWriter, r *http.Request) {
	setPublicPayHeaders(w)
	if !s.requirePayForMe(w) {
		return
	}
	var req struct {
		MeterID        uuid.UUID `json:"meter_id"`
		AmountMode     string    `json:"amount_mode"`
		AmountMinor    int64     `json:"amount_minor"`
		MinAmountMinor int64     `json:"min_amount_minor"`
		MaxAmountMinor int64     `json:"max_amount_minor"`
		Note           string    `json:"note"`
		ExpiresInHours int       `json:"expires_in_hours"`
		MaxPayments    int       `json:"max_payments"`
	}
	if !decode(r, &req) || req.MeterID == uuid.Nil || req.AmountMode == "" {
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "meter_id and amount_mode are required")
		return
	}
	if req.ExpiresInHours < 0 || req.MaxPayments < 0 {
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "expires_in_hours and max_payments cannot be negative")
		return
	}
	var ttl time.Duration
	if req.ExpiresInHours > 0 {
		ttl = time.Duration(req.ExpiresInHours) * time.Hour
	}

	created, err := s.O.CreatePaymentRequest(r.Context(), orchestrator.CreatePayRequestParams{
		RequesterID:    userID(r),
		MeterID:        req.MeterID,
		AmountMode:     payrequests.AmountMode(strings.ToUpper(strings.TrimSpace(req.AmountMode))),
		AmountMinor:    req.AmountMinor,
		MinAmountMinor: req.MinAmountMinor,
		MaxAmountMinor: req.MaxAmountMinor,
		Note:           req.Note,
		TTL:            ttl,
		MaxPayments:    req.MaxPayments,
		PublicBaseURL:  s.publicBaseURL(),
	})
	if err != nil {
		s.mapPayRequestErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) listPaymentRequests(w http.ResponseWriter, r *http.Request) {
	if !s.requirePayForMe(w) {
		return
	}
	list, err := s.O.ListPaymentRequests(r.Context(), userID(r))
	if err != nil {
		s.mapPayRequestErr(w, r, err)
		return
	}
	if list == nil {
		list = []orchestrator.PayRequestView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": list})
}

func (s *Server) cancelPaymentRequest(w http.ResponseWriter, r *http.Request) {
	if !s.requirePayForMe(w) {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "not found")
		return
	}
	res, err := s.O.CancelPaymentRequest(r.Context(), userID(r), id)
	if err != nil {
		s.mapPayRequestErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) getPublicPayLink(w http.ResponseWriter, r *http.Request) {
	v, err := s.O.GetPublicPayLink(r.Context(), r.PathValue("code"))
	if err != nil {
		s.mapPayRequestErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) checkoutPublicPayLink(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Idempotency-Key")
	if len(key) < 16 || len(key) > 128 {
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "Idempotency-Key header (16–128 chars) is required")
		return
	}

	rawCode := r.PathValue("code")
	codeHMAC, ok := payrequests.LinkCodeHMAC(s.O.Box(), rawCode)
	if !ok {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "not found")
		return
	}
	if allowed, wait := s.publicLink.Allow(codeHMAC); !allowed {
		tooMany(w, wait)
		return
	}

	var req struct {
		AmountMinor int64  `json:"amount_minor"`
		Email       string `json:"email"`
		PayerName   string `json:"payer_name"`
	}
	if !decode(r, &req) {
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid request body")
		return
	}
	if !validEmail(req.Email) {
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "a valid email is required")
		return
	}

	res, _, err := s.O.CheckoutPublicPayLink(r.Context(), orchestrator.PublicCheckoutRequest{
		Code:           rawCode,
		AmountMinor:    req.AmountMinor,
		Email:          req.Email,
		PayerName:      req.PayerName,
		IdempotencyKey: key,
		IP:             s.clientIP(r),
		UserAgent:      r.UserAgent(),
	})
	if err != nil && (res == nil || res.Transaction == nil) {
		s.mapPayRequestErr(w, r, err)
		return
	}
	if err != nil {
		// Never log link code or payer email.
		s.Log.Warn("public checkout payment initialisation failed", "txn", res.Transaction.ID, "err", err)
		writeErr(w, http.StatusBadGateway, "PROVIDER_UNAVAILABLE", "payment provider unavailable; retry with the same Idempotency-Key")
		return
	}

	t := res.Transaction
	writeJSON(w, http.StatusAccepted, map[string]any{
		"transaction_id": t.ID,
		"status":         t.Status(),
		"payment_status": t.Payment,
		"amount_minor":   t.Amount.Minor,
		"fee_minor":      t.Fee.Minor,
		"total_minor":    t.Total.Minor,
		"currency":       t.Total.Currency,
		"checkout_url":   t.CheckoutURL,
		"status_code":    res.StatusCode,
		"status_url":     s.publicBaseURL() + "/p/status/" + res.StatusCode,
	})
}

func (s *Server) getPublicPayStatus(w http.ResponseWriter, r *http.Request) {
	v, err := s.O.GetPublicPayStatus(r.Context(), r.PathValue("status_code"))
	if err != nil {
		s.mapPayRequestErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) mapPayRequestErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, orchestrator.ErrLinkNotFound):
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "not found")
	case errors.Is(err, orchestrator.ErrLinkAlreadyPaid):
		writeErr(w, http.StatusConflict, "LINK_ALREADY_PAID", err.Error())
	case errors.Is(err, orchestrator.ErrLinkExpired):
		writeErr(w, http.StatusConflict, "LINK_EXPIRED", err.Error())
	case errors.Is(err, orchestrator.ErrLinkCancelled):
		writeErr(w, http.StatusConflict, "LINK_CANCELLED", err.Error())
	case errors.Is(err, orchestrator.ErrLinkUnusable):
		writeErr(w, http.StatusConflict, "LINK_UNUSABLE", err.Error())
	case errors.Is(err, payrequests.ErrInvalidNote),
		errors.Is(err, payrequests.ErrInvalidPayerName),
		errors.Is(err, payrequests.ErrInvalidTTL),
		errors.Is(err, payrequests.ErrInvalidMaxPays),
		errors.Is(err, payrequests.ErrInvalidMode),
		errors.Is(err, notifications.ErrInvalidEmail):
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
	default:
		s.mapErr(w, r, err)
	}
}
