package httpapi

import (
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/households"
)

func (s *Server) householdRoutes(mux *patternMux, user, write func(http.HandlerFunc) http.Handler) {
	mux.Handle("POST /v1/households", write(s.createHousehold))
	mux.Handle("GET /v1/households", user(s.listHouseholds))
	mux.Handle("GET /v1/households/{id}", user(s.getHousehold))
	mux.Handle("POST /v1/households/{id}/meters", write(s.addHouseholdMeter))
	mux.Handle("POST /v1/households/{id}/members", write(s.inviteHouseholdMember))
	mux.Handle("POST /v1/households/{id}/members/me/accept", write(s.acceptHouseholdInvite))
	mux.Handle("POST /v1/households/{id}/members/me/decline", write(s.declineHouseholdInvite))
	mux.Handle("DELETE /v1/households/{id}/members/{uid}", write(s.revokeHouseholdMember))
	mux.Handle("POST /v1/meters/{meter_id}/ownership/verify", write(s.verifyMeterOwnership))
	mux.Handle("GET /v1/meters/{meter_id}/ownership", user(s.listMeterOwnership))
}

func (s *Server) requireHouseholds(w http.ResponseWriter) bool {
	if s.HouseholdsDisabled {
		writeErr(w, http.StatusNotFound, "FEATURE_DISABLED", "households are not enabled")
		return false
	}
	return true
}

func (s *Server) createHousehold(w http.ResponseWriter, r *http.Request) {
	if !s.requireHouseholds(w) {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if !decode(r, &req) {
		writeErr(w, 400, "INVALID_REQUEST", "invalid request body")
		return
	}
	h, err := s.O.CreateHousehold(r.Context(), userID(r), req.Name)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, h)
}

func (s *Server) listHouseholds(w http.ResponseWriter, r *http.Request) {
	if !s.requireHouseholds(w) {
		return
	}
	list, err := s.O.ListHouseholds(r.Context(), userID(r))
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"data": list})
}

func (s *Server) getHousehold(w http.ResponseWriter, r *http.Request) {
	if !s.requireHouseholds(w) {
		return
	}
	id, ok := parsePathUUID(w, r, "id")
	if !ok {
		return
	}
	v, err := s.O.GetHousehold(r.Context(), userID(r), id)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, v)
}

func (s *Server) addHouseholdMeter(w http.ResponseWriter, r *http.Request) {
	if !s.requireHouseholds(w) {
		return
	}
	id, ok := parsePathUUID(w, r, "id")
	if !ok {
		return
	}
	var req struct {
		MeterID string `json:"meter_id"`
	}
	if !decode(r, &req) {
		writeErr(w, 400, "INVALID_REQUEST", "invalid request body")
		return
	}
	mid, err := uuid.Parse(req.MeterID)
	if err != nil {
		writeErr(w, 400, "INVALID_REQUEST", "meter_id is required")
		return
	}
	if err := s.O.AddHouseholdMeter(r.Context(), userID(r), id, mid); err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 201, map[string]any{"ok": true})
}

func (s *Server) inviteHouseholdMember(w http.ResponseWriter, r *http.Request) {
	if !s.requireHouseholds(w) {
		return
	}
	id, ok := parsePathUUID(w, r, "id")
	if !ok {
		return
	}
	var req struct {
		UserID     string `json:"user_id"`
		ExternalID string `json:"external_id"`
		Role       string `json:"role"`
	}
	if !decode(r, &req) {
		writeErr(w, 400, "INVALID_REQUEST", "invalid request body")
		return
	}
	invitee := req.UserID
	if req.ExternalID != "" {
		id, err := s.O.Store().Repos().Users().EnsureByExternalID(r.Context(), req.ExternalID)
		if err != nil {
			s.mapErr(w, r, err)
			return
		}
		invitee = id
	}
	role, err := households.ParseRole(req.Role)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	mem, err := s.O.InviteHouseholdMember(r.Context(), userID(r), id, invitee, role)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 201, mem)
}

func (s *Server) acceptHouseholdInvite(w http.ResponseWriter, r *http.Request) {
	if !s.requireHouseholds(w) {
		return
	}
	id, ok := parsePathUUID(w, r, "id")
	if !ok {
		return
	}
	mem, err := s.O.AcceptHouseholdInvite(r.Context(), userID(r), id)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, mem)
}

func (s *Server) declineHouseholdInvite(w http.ResponseWriter, r *http.Request) {
	if !s.requireHouseholds(w) {
		return
	}
	id, ok := parsePathUUID(w, r, "id")
	if !ok {
		return
	}
	if err := s.O.DeclineHouseholdInvite(r.Context(), userID(r), id); err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) revokeHouseholdMember(w http.ResponseWriter, r *http.Request) {
	if !s.requireHouseholds(w) {
		return
	}
	id, ok := parsePathUUID(w, r, "id")
	if !ok {
		return
	}
	uid := r.PathValue("uid")
	if err := s.O.RevokeHouseholdMember(r.Context(), userID(r), id, uid); err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) verifyMeterOwnership(w http.ResponseWriter, r *http.Request) {
	if !s.requireHouseholds(w) {
		return
	}
	mid, ok := parsePathUUID(w, r, "meter_id")
	if !ok {
		return
	}
	own, err := s.O.VerifyOwnershipByName(r.Context(), userID(r), mid)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 201, own)
}

func (s *Server) listMeterOwnership(w http.ResponseWriter, r *http.Request) {
	if !s.requireHouseholds(w) {
		return
	}
	mid, ok := parsePathUUID(w, r, "meter_id")
	if !ok {
		return
	}
	list, err := s.O.ListOwnership(r.Context(), userID(r), mid)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"data": list})
}

func mapHouseholdErr(err error) (int, string, string, bool) {
	switch {
	case errors.Is(err, households.ErrNotFound):
		return 404, "NOT_FOUND", "not found", true
	case errors.Is(err, households.ErrForbidden):
		return 403, "FORBIDDEN", "you cannot do that", true
	case errors.Is(err, households.ErrNotVerifiedOwner):
		return 403, "OWNERSHIP_REQUIRED", "verified ownership is required", true
	case errors.Is(err, households.ErrNameMismatch):
		return 422, "NAME_MISMATCH", "disco customer name does not match your verified name", true
	case errors.Is(err, households.ErrNoDisplayName):
		return 422, "DISPLAY_NAME_REQUIRED", "a verified display name is required", true
	case errors.Is(err, households.ErrAlreadyOwner), errors.Is(err, households.ErrAlreadyMember):
		return 409, "ALREADY_EXISTS", err.Error(), true
	case errors.Is(err, households.ErrInvalidRole), errors.Is(err, households.ErrInvalidInvite), errors.Is(err, households.ErrCannotModifyOwner):
		return 400, "INVALID_REQUEST", err.Error(), true
	default:
		return 0, "", "", false
	}
}
