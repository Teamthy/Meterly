package httpapi

import (
	"errors"
	"io"
	"net/http"

	"github.com/teamthy/meterly/internal/channels/ussd"
	"github.com/teamthy/meterly/internal/conversation"
	"github.com/teamthy/meterly/internal/notifications"
	"github.com/teamthy/meterly/internal/offline"
	"github.com/teamthy/meterly/internal/providers"
)

func (s *Server) offlineRoutes(mux *patternMux, write func(http.HandlerFunc) http.Handler) {
	mux.HandleFunc("POST /v1/channels/ussd/inbound", s.limited(s.hooks, s.ussdInbound))
	mux.HandleFunc("GET /v1/channels/whatsapp/inbound", s.limited(s.hooks, s.whatsAppInboundChallenge))
	mux.HandleFunc("POST /v1/channels/whatsapp/inbound", s.limited(s.hooks, s.whatsAppInbound))
	mux.Handle("POST /v1/channels/phone/link/start", write(s.startPhoneLink))
	mux.Handle("POST /v1/channels/phone/link/verify", write(s.verifyPhoneLink))
}

func (s *Server) ussdInbound(w http.ResponseWriter, r *http.Request) {
	if s.USSDDisabled || s.Offline == nil {
		writeErr(w, http.StatusNotFound, "FEATURE_DISABLED", "USSD channel is not enabled")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "unreadable request body")
		return
	}
	reply, err := s.Offline.HandleUSSDCallback(r.Context(), s.clientIP(r), r.Header, body)
	if err != nil {
		switch {
		case errors.Is(err, ussd.ErrForbiddenIP), errors.Is(err, providers.ErrBadSignature):
			writeErr(w, http.StatusUnauthorized, "UNAUTHENTICATED", "invalid aggregator IP or signature")
		case errors.Is(err, ussd.ErrInvalidPayload), errors.Is(err, notifications.ErrInvalidPhone):
			writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "malformed USSD callback")
		default:
			s.Log.Error("ussd callback failed", "err", err)
			ussd.WriteReply(w, "END Service temporarily unavailable. Please try again.")
		}
		return
	}
	ussd.WriteReply(w, reply)
}

func (s *Server) whatsAppInboundChallenge(w http.ResponseWriter, r *http.Request) {
	if s.WhatsAppDisabled || s.Offline == nil {
		writeErr(w, http.StatusNotFound, "FEATURE_DISABLED", "WhatsApp channel is not enabled")
		return
	}
	q := r.URL.Query()
	ch, err := s.Offline.VerifyWhatsAppChallenge(
		q.Get("hub.mode"),
		q.Get("hub.verify_token"),
		q.Get("hub.challenge"),
	)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "UNAUTHENTICATED", "invalid webhook challenge")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(ch))
}

func (s *Server) whatsAppInbound(w http.ResponseWriter, r *http.Request) {
	if s.WhatsAppDisabled || s.Offline == nil {
		writeErr(w, http.StatusNotFound, "FEATURE_DISABLED", "WhatsApp channel is not enabled")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "unreadable request body")
		return
	}
	if err := s.Offline.HandleWhatsAppWebhook(r.Context(), r.Header, body); err != nil {
		if errors.Is(err, providers.ErrBadSignature) {
			writeErr(w, http.StatusUnauthorized, "UNAUTHENTICATED", "invalid webhook signature")
			return
		}
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "malformed WhatsApp webhook")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "received"})
}

func (s *Server) startPhoneLink(w http.ResponseWriter, r *http.Request) {
	if (s.USSDDisabled && s.WhatsAppDisabled) || s.Offline == nil {
		writeErr(w, http.StatusNotFound, "FEATURE_DISABLED", "offline channels are not enabled")
		return
	}
	var req struct {
		Phone string `json:"phone"`
	}
	if !decode(r, &req) {
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "malformed JSON")
		return
	}
	masked, err := s.Offline.StartPhoneLink(r.Context(), userID(r), req.Phone)
	if err != nil {
		switch {
		case errors.Is(err, notifications.ErrInvalidPhone):
			writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
		case errors.Is(err, offline.ErrPhoneRateLimited):
			writeErr(w, http.StatusTooManyRequests, "RATE_LIMITED", "too many verification codes requested; try later")
		default:
			s.mapErr(w, r, err)
		}
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{
		"status":       "otp_sent",
		"phone_masked": masked,
	})
}

func (s *Server) verifyPhoneLink(w http.ResponseWriter, r *http.Request) {
	if (s.USSDDisabled && s.WhatsAppDisabled) || s.Offline == nil {
		writeErr(w, http.StatusNotFound, "FEATURE_DISABLED", "offline channels are not enabled")
		return
	}
	var req struct {
		Phone string `json:"phone"`
		Code  string `json:"code"`
	}
	if !decode(r, &req) {
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "malformed JSON")
		return
	}
	masked, err := s.Offline.VerifyPhoneLink(r.Context(), userID(r), req.Phone, req.Code)
	if err != nil {
		switch {
		case errors.Is(err, notifications.ErrInvalidPhone):
			writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
		case errors.Is(err, offline.ErrOTPLockedOut):
			writeErr(w, http.StatusTooManyRequests, "OTP_LOCKED", "too many incorrect verification attempts")
		case errors.Is(err, conversation.ErrInvalidOTP):
			writeErr(w, http.StatusUnprocessableEntity, "INVALID_OTP", "invalid or expired verification code")
		default:
			s.mapErr(w, r, err)
		}
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{
		"status":       "verified",
		"phone_masked": masked,
	})
}
