package httpapi

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/audit"
	"github.com/teamthy/meterly/internal/authz"
	"github.com/teamthy/meterly/internal/idempotency"
	"github.com/teamthy/meterly/internal/meters"
	"github.com/teamthy/meterly/internal/notifications"
	"github.com/teamthy/meterly/internal/orchestrator"
	"github.com/teamthy/meterly/internal/powerdesk"
	"github.com/teamthy/meterly/internal/store"
)

func (s *Server) powerDeskRoutes(mux *patternMux, user, write func(http.HandlerFunc) http.Handler) {
	mux.Handle("POST /v1/orgs", write(s.createOrg))
	mux.Handle("GET /v1/orgs/{id}", user(s.getOrg))
	mux.Handle("PATCH /v1/orgs/{id}", write(s.updateOrg))
	mux.Handle("POST /v1/orgs/{id}/members", write(s.addOrgMember))
	mux.Handle("PATCH /v1/orgs/{id}/members/{uid}", write(s.updateOrgMemberRole))
	mux.Handle("DELETE /v1/orgs/{id}/members/{uid}", write(s.removeOrgMember))
	mux.Handle("POST /v1/orgs/{id}/properties", write(s.createProperty))
	mux.Handle("POST /v1/properties/{id}/units", write(s.createUnit))
	mux.Handle("POST /v1/units/{id}/meters", write(s.attachUnitMeter))
	mux.Handle("GET /v1/units/{id}/meters/{unit_meter_id}/history", user(s.getUnitMeterHistory))
	mux.Handle("POST /v1/units/{id}/meters/{unit_meter_id}/delegate", write(s.delegateUnitMeter))
	mux.Handle("POST /v1/orgs/{id}/bulk-topups", write(s.createBulkTopup))
	mux.Handle("POST /v1/orgs/{id}/bulk-topups/{pot_id}/approve", write(s.approveBulkTopup))
	mux.Handle("GET /v1/orgs/{id}/bulk-topups/{pot_id}", user(s.getBulkTopup))
	mux.Handle("GET /v1/orgs/{id}/statements", user(s.getOrgStatements))
}

func (s *Server) requirePowerDesk(w http.ResponseWriter) bool {
	if s.PowerDeskDisabled {
		writeErr(w, http.StatusNotFound, "FEATURE_DISABLED", "landlord power desk is not enabled")
		return false
	}
	return true
}

func (s *Server) pdCtx(r *http.Request) *http.Request {
	return r.WithContext(s.O.WithPowerDeskCaller(r.Context(), userID(r)))
}

func parsePathUUID(w http.ResponseWriter, r *http.Request, param string) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue(param))
	if err != nil {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "not found")
		return uuid.Nil, false
	}
	return id, true
}

// POST /v1/orgs
func (s *Server) createOrg(w http.ResponseWriter, r *http.Request) {
	setNoStoreHeaders(w)
	if !s.requirePowerDesk(w) {
		return
	}
	r = s.pdCtx(r)
	var req struct {
		Name                          string `json:"name"`
		LegalName                     string `json:"legal_name"`
		RegistrationNumber            string `json:"registration_number"`
		OrganisationType              string `json:"organisation_type"`
		ManagerApprovalThresholdMinor *int64 `json:"manager_approval_threshold_minor"`
	}
	if !decode(r, &req) {
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid request body")
		return
	}
	org, err := s.O.CreateOrg(r.Context(), orchestrator.CreateOrgParams{
		Name:                          req.Name,
		LegalName:                     req.LegalName,
		RegistrationNumber:            req.RegistrationNumber,
		OrganisationType:              powerdesk.OrganisationType(strings.ToUpper(strings.TrimSpace(req.OrganisationType))),
		ManagerApprovalThresholdMinor: req.ManagerApprovalThresholdMinor,
	})
	if err != nil {
		s.mapPowerDeskErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, org)
}

// GET /v1/orgs/{id}
func (s *Server) getOrg(w http.ResponseWriter, r *http.Request) {
	setNoStoreHeaders(w)
	if !s.requirePowerDesk(w) {
		return
	}
	r = s.pdCtx(r)
	orgID, ok := parsePathUUID(w, r, "id")
	if !ok {
		return
	}
	org, err := s.O.GetOrg(r.Context(), orgID)
	if err != nil {
		s.mapPowerDeskErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, org)
}

// PATCH /v1/orgs/{id}
func (s *Server) updateOrg(w http.ResponseWriter, r *http.Request) {
	setNoStoreHeaders(w)
	if !s.requirePowerDesk(w) {
		return
	}
	r = s.pdCtx(r)
	orgID, ok := parsePathUUID(w, r, "id")
	if !ok {
		return
	}
	var req struct {
		Name                          *string `json:"name"`
		LegalName                     *string `json:"legal_name"`
		RegistrationNumber            *string `json:"registration_number"`
		ManagerApprovalThresholdMinor *int64  `json:"manager_approval_threshold_minor"`
		ClearManagerApprovalThreshold bool    `json:"clear_manager_approval_threshold"`
	}
	if !decode(r, &req) {
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid request body")
		return
	}
	org, err := s.O.UpdateOrg(r.Context(), orgID, orchestrator.UpdateOrgParams{
		Name:                          req.Name,
		LegalName:                     req.LegalName,
		RegistrationNumber:            req.RegistrationNumber,
		ManagerApprovalThresholdMinor: req.ManagerApprovalThresholdMinor,
		ClearManagerApprovalThreshold: req.ClearManagerApprovalThreshold,
	})
	if err != nil {
		s.mapPowerDeskErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, org)
}

// POST /v1/orgs/{id}/members
func (s *Server) addOrgMember(w http.ResponseWriter, r *http.Request) {
	setNoStoreHeaders(w)
	if !s.requirePowerDesk(w) {
		return
	}
	r = s.pdCtx(r)
	orgID, ok := parsePathUUID(w, r, "id")
	if !ok {
		return
	}
	var req struct {
		UserID string `json:"user_id"`
		Role   string `json:"role"`
	}
	if !decode(r, &req) || strings.TrimSpace(req.UserID) == "" {
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "user_id and role are required")
		return
	}
	if _, err := uuid.Parse(strings.TrimSpace(req.UserID)); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "user_id must be a UUID")
		return
	}
	m, err := s.O.AddOrgMember(r.Context(), orgID, strings.TrimSpace(req.UserID), powerdesk.Role(strings.ToUpper(strings.TrimSpace(req.Role))))
	if err != nil {
		s.mapPowerDeskErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, m)
}

// PATCH /v1/orgs/{id}/members/{uid}
func (s *Server) updateOrgMemberRole(w http.ResponseWriter, r *http.Request) {
	setNoStoreHeaders(w)
	if !s.requirePowerDesk(w) {
		return
	}
	r = s.pdCtx(r)
	orgID, ok := parsePathUUID(w, r, "id")
	if !ok {
		return
	}
	uidUUID, ok := parsePathUUID(w, r, "uid")
	if !ok {
		return
	}
	var req struct {
		Role string `json:"role"`
	}
	if !decode(r, &req) || strings.TrimSpace(req.Role) == "" {
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "role is required")
		return
	}
	m, err := s.O.UpdateOrgMemberRole(r.Context(), orgID, uidUUID.String(), powerdesk.Role(strings.ToUpper(strings.TrimSpace(req.Role))))
	if err != nil {
		s.mapPowerDeskErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// DELETE /v1/orgs/{id}/members/{uid}
func (s *Server) removeOrgMember(w http.ResponseWriter, r *http.Request) {
	setNoStoreHeaders(w)
	if !s.requirePowerDesk(w) {
		return
	}
	r = s.pdCtx(r)
	orgID, ok := parsePathUUID(w, r, "id")
	if !ok {
		return
	}
	uidUUID, ok := parsePathUUID(w, r, "uid")
	if !ok {
		return
	}
	if err := s.O.RemoveOrgMember(r.Context(), orgID, uidUUID.String()); err != nil {
		s.mapPowerDeskErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /v1/orgs/{id}/properties
func (s *Server) createProperty(w http.ResponseWriter, r *http.Request) {
	setNoStoreHeaders(w)
	if !s.requirePowerDesk(w) {
		return
	}
	r = s.pdCtx(r)
	orgID, ok := parsePathUUID(w, r, "id")
	if !ok {
		return
	}
	var req struct {
		Name    string `json:"name"`
		Address string `json:"address"`
		City    string `json:"city"`
		State   string `json:"state"`
		Country string `json:"country"`
	}
	if !decode(r, &req) {
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid request body")
		return
	}
	prop, err := s.O.CreateProperty(r.Context(), orgID, orchestrator.CreatePropertyParams{
		Name:    req.Name,
		Address: req.Address,
		City:    req.City,
		State:   req.State,
		Country: req.Country,
	})
	if err != nil {
		s.mapPowerDeskErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, prop)
}

// POST /v1/properties/{id}/units
func (s *Server) createUnit(w http.ResponseWriter, r *http.Request) {
	setNoStoreHeaders(w)
	if !s.requirePowerDesk(w) {
		return
	}
	r = s.pdCtx(r)
	propID, ok := parsePathUUID(w, r, "id")
	if !ok {
		return
	}
	var req struct {
		BuildingName    string `json:"building_name"`
		UnitName        string `json:"unit_name"`
		OccupancyStatus string `json:"occupancy_status"`
	}
	if !decode(r, &req) {
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid request body")
		return
	}
	unit, err := s.O.CreateUnit(r.Context(), propID, orchestrator.CreateUnitParams{
		BuildingName:    req.BuildingName,
		UnitName:        req.UnitName,
		OccupancyStatus: req.OccupancyStatus,
	})
	if err != nil {
		s.mapPowerDeskErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, unit)
}

// POST /v1/units/{id}/meters
func (s *Server) attachUnitMeter(w http.ResponseWriter, r *http.Request) {
	setNoStoreHeaders(w)
	if !s.requirePowerDesk(w) {
		return
	}
	r = s.pdCtx(r)
	unitID, ok := parsePathUUID(w, r, "id")
	if !ok {
		return
	}
	var req struct {
		MeterID           string `json:"meter_id"`
		MeterNumber       string `json:"meter_number"`
		DiscoCode         string `json:"disco_code"`
		Label             string `json:"label"`
		EvidenceReference string `json:"evidence_reference"`
	}
	if !decode(r, &req) {
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid request body")
		return
	}
	var meterID uuid.UUID
	if strings.TrimSpace(req.MeterID) != "" {
		parsed, err := uuid.Parse(strings.TrimSpace(req.MeterID))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "meter_id must be a UUID")
			return
		}
		meterID = parsed
	}
	if meterID == uuid.Nil && (strings.TrimSpace(req.MeterNumber) == "" || strings.TrimSpace(req.DiscoCode) == "") {
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "either meter_id or meter_number and disco_code are required")
		return
	}
	um, err := s.O.AttachUnitMeter(r.Context(), unitID, orchestrator.AttachUnitMeterParams{
		MeterID:           meterID,
		MeterNumber:       req.MeterNumber,
		DiscoCode:         req.DiscoCode,
		Label:             req.Label,
		EvidenceReference: req.EvidenceReference,
	})
	if err != nil {
		s.mapPowerDeskErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, um)
}

// GET /v1/units/{id}/meters/{unit_meter_id}/history
func (s *Server) getUnitMeterHistory(w http.ResponseWriter, r *http.Request) {
	setNoStoreHeaders(w)
	if !s.requirePowerDesk(w) {
		return
	}
	r = s.pdCtx(r)
	unitID, ok := parsePathUUID(w, r, "id")
	if !ok {
		return
	}
	umID, ok := parsePathUUID(w, r, "unit_meter_id")
	if !ok {
		return
	}
	items, err := s.O.GetUnitMeterHistory(r.Context(), unitID, umID)
	if err != nil {
		s.mapPowerDeskErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"transactions": items})
}

// POST /v1/units/{id}/meters/{unit_meter_id}/delegate
func (s *Server) delegateUnitMeter(w http.ResponseWriter, r *http.Request) {
	setNoStoreHeaders(w)
	if !s.requirePowerDesk(w) {
		return
	}
	r = s.pdCtx(r)
	unitID, ok := parsePathUUID(w, r, "id")
	if !ok {
		return
	}
	umID, ok := parsePathUUID(w, r, "unit_meter_id")
	if !ok {
		return
	}
	var req struct {
		UserID     string `json:"user_id"`
		Permission string `json:"permission"`
		Label      string `json:"label"`
	}
	if !decode(r, &req) || strings.TrimSpace(req.UserID) == "" || strings.TrimSpace(req.Permission) == "" {
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "user_id and permission are required")
		return
	}
	if _, err := uuid.Parse(strings.TrimSpace(req.UserID)); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "user_id must be a UUID")
		return
	}
	perm := meters.Permission(strings.ToUpper(strings.TrimSpace(req.Permission)))
	if err := s.O.DelegateUnitMeter(r.Context(), unitID, umID, strings.TrimSpace(req.UserID), perm, req.Label); err != nil {
		s.mapPowerDeskErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "delegated"})
}

// POST /v1/admin/unit-meters/{id}/verify
func (s *Server) adminVerifyUnitMeter(w http.ResponseWriter, r *http.Request) {
	setNoStoreHeaders(w)
	if !s.requirePowerDesk(w) {
		return
	}
	umID, ok := parsePathUUID(w, r, "id")
	if !ok {
		return
	}
	var body struct {
		Method            string `json:"method"`
		EvidenceReference string `json:"evidence_reference"`
		Reason            string `json:"reason"`
	}
	if !decode(r, &body) {
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid request body")
		return
	}
	reason := strings.TrimSpace(body.Reason)
	if len(reason) < 5 || len(reason) > 500 {
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "a reason (5–500 characters) is required")
		return
	}
	actor := audit.Actor{
		UserID:        userID(r),
		Type:          audit.ActorOperator,
		Reason:        reason,
		CorrelationID: audit.CorrelationFrom(r.Context()),
	}
	um, err := s.O.VerifyUnitMeter(r.Context(), umID, orchestrator.VerifyUnitMeterParams{
		Method:            powerdesk.VerificationMethod(strings.ToUpper(strings.TrimSpace(body.Method))),
		EvidenceReference: body.EvidenceReference,
	}, actor)
	if err != nil {
		s.mapPowerDeskErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, um)
}

// POST /v1/orgs/{id}/bulk-topups
func (s *Server) createBulkTopup(w http.ResponseWriter, r *http.Request) {
	setNoStoreHeaders(w)
	if !s.requirePowerDesk(w) {
		return
	}
	r = s.pdCtx(r)
	orgID, ok := parsePathUUID(w, r, "id")
	if !ok {
		return
	}
	idemKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if len(idemKey) < 16 || len(idemKey) > 128 {
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "Idempotency-Key header (16–128 chars) is required")
		return
	}
	var req struct {
		Email string `json:"email"`
		Items []struct {
			UnitID      string `json:"unit_id"`
			AmountMinor int64  `json:"amount_minor"`
		} `json:"items"`
	}
	if !decode(r, &req) {
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid request body")
		return
	}
	items := make([]orchestrator.BulkTopupItemInput, 0, len(req.Items))
	for _, it := range req.Items {
		uid, err := uuid.Parse(strings.TrimSpace(it.UnitID))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "each item unit_id must be a UUID")
			return
		}
		items = append(items, orchestrator.BulkTopupItemInput{
			UnitID:      uid,
			AmountMinor: it.AmountMinor,
		})
	}
	view, replayed, err := s.O.CreateBulkTopup(r.Context(), orgID, orchestrator.CreateBulkTopupRequest{
		Items:          items,
		Email:          req.Email,
		IdempotencyKey: idemKey,
	})
	if err != nil {
		s.mapPowerDeskErr(w, r, err)
		return
	}
	status := http.StatusCreated
	if replayed {
		status = http.StatusOK
	}
	writeJSON(w, status, view)
}

// POST /v1/orgs/{id}/bulk-topups/{pot_id}/approve
func (s *Server) approveBulkTopup(w http.ResponseWriter, r *http.Request) {
	setNoStoreHeaders(w)
	if !s.requirePowerDesk(w) {
		return
	}
	r = s.pdCtx(r)
	orgID, ok := parsePathUUID(w, r, "id")
	if !ok {
		return
	}
	potID, ok := parsePathUUID(w, r, "pot_id")
	if !ok {
		return
	}
	var req struct {
		Email string `json:"email"`
	}
	if r.Body != nil {
		raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid request body")
			return
		}
		if strings.TrimSpace(string(raw)) != "" {
			r.Body = io.NopCloser(strings.NewReader(string(raw)))
			if !decode(r, &req) {
				writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid request body")
				return
			}
		}
	}
	view, err := s.O.ApproveBulkTopup(r.Context(), orgID, potID, req.Email)
	if err != nil {
		s.mapPowerDeskErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// GET /v1/orgs/{id}/bulk-topups/{pot_id}
func (s *Server) getBulkTopup(w http.ResponseWriter, r *http.Request) {
	setNoStoreHeaders(w)
	if !s.requirePowerDesk(w) {
		return
	}
	r = s.pdCtx(r)
	orgID, ok := parsePathUUID(w, r, "id")
	if !ok {
		return
	}
	potID, ok := parsePathUUID(w, r, "pot_id")
	if !ok {
		return
	}
	view, err := s.O.GetBulkTopup(r.Context(), orgID, potID)
	if err != nil {
		s.mapPowerDeskErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// GET /v1/orgs/{id}/statements?property_id=&month=YYYY-MM&format=csv
func (s *Server) getOrgStatements(w http.ResponseWriter, r *http.Request) {
	setNoStoreHeaders(w)
	if !s.requirePowerDesk(w) {
		return
	}
	r = s.pdCtx(r)
	orgID, ok := parsePathUUID(w, r, "id")
	if !ok {
		return
	}
	q := r.URL.Query()
	month := strings.TrimSpace(q.Get("month"))
	if month == "" {
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "month query parameter (YYYY-MM) is required")
		return
	}
	var propID *uuid.UUID
	if rawProp := strings.TrimSpace(q.Get("property_id")); rawProp != "" {
		parsed, err := uuid.Parse(rawProp)
		if err != nil {
			writeErr(w, http.StatusNotFound, "NOT_FOUND", "not found")
			return
		}
		propID = &parsed
	}
	format := strings.ToLower(strings.TrimSpace(q.Get("format")))
	if format != "" && format != "json" && format != "csv" {
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "format must be json or csv")
		return
	}

	st, err := s.O.GetMonthlyStatement(r.Context(), orgID, propID, month)
	if err != nil {
		s.mapPowerDeskErr(w, r, err)
		return
	}
	if format == "csv" {
		csvBytes, err := powerdesk.FormatStatementCSV(st)
		if err != nil {
			s.mapPowerDeskErr(w, r, err)
			return
		}
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="statement-%s-%s.csv"`, orgID, st.Month))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(csvBytes)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) mapPowerDeskErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, authz.ErrUnauthenticated):
		writeErr(w, http.StatusUnauthorized, "UNAUTHENTICATED", "authentication required")
	case errors.Is(err, authz.ErrNotFound),
		errors.Is(err, orchestrator.ErrUserNotFound),
		errors.Is(err, orchestrator.ErrMemberNotFound),
		errors.Is(err, orchestrator.ErrUnitMeterNotFound):
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "not found")
	case errors.Is(err, authz.ErrForbidden):
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "insufficient organisation role")
	case errors.Is(err, powerdesk.ErrUnitMeterUnverified):
		writeErr(w, http.StatusForbidden, "UNIT_METER_UNVERIFIED", err.Error())
	case errors.Is(err, powerdesk.ErrLastOwner):
		writeErr(w, http.StatusConflict, "LAST_OWNER", err.Error())
	case errors.Is(err, powerdesk.ErrNotPendingApproval):
		writeErr(w, http.StatusConflict, "NOT_PENDING_APPROVAL", err.Error())
	case errors.Is(err, store.ErrDuplicate):
		writeErr(w, http.StatusConflict, "DUPLICATE_RESOURCE", "resource already exists")
	case errors.Is(err, idempotency.ErrConflict), errors.Is(err, idempotency.ErrInProgress):
		writeErr(w, http.StatusConflict, "IDEMPOTENCY_CONFLICT", err.Error())
	case errors.Is(err, powerdesk.ErrAccountNameMismatch):
		writeErr(w, http.StatusUnprocessableEntity, "VERIFICATION_MISMATCH", err.Error())
	case errors.Is(err, powerdesk.ErrEvidenceRequired):
		writeErr(w, http.StatusUnprocessableEntity, "EVIDENCE_REQUIRED", err.Error())
	case errors.Is(err, orchestrator.ErrUnitHasNoMeter):
		writeErr(w, http.StatusUnprocessableEntity, "UNIT_HAS_NO_METER", err.Error())
	case errors.Is(err, powerdesk.ErrInvalidRole),
		errors.Is(err, powerdesk.ErrInvalidOrgName),
		errors.Is(err, powerdesk.ErrInvalidOrgType),
		errors.Is(err, powerdesk.ErrInvalidThreshold),
		errors.Is(err, powerdesk.ErrInvalidPropertyName),
		errors.Is(err, powerdesk.ErrInvalidUnitName),
		errors.Is(err, powerdesk.ErrInvalidEvidenceRef),
		errors.Is(err, powerdesk.ErrVerificationMethodInvalid),
		errors.Is(err, powerdesk.ErrEmptyBulkItems),
		errors.Is(err, powerdesk.ErrDuplicateUnitInBulk),
		errors.Is(err, powerdesk.ErrInvalidMonth),
		errors.Is(err, orchestrator.ErrInvalidCountryCode),
		errors.Is(err, orchestrator.ErrInvalidDelegation),
		errors.Is(err, notifications.ErrInvalidEmail):
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
	case errors.Is(err, meters.ErrNotFound):
		writeErr(w, http.StatusNotFound, "METER_NOT_FOUND", "meter not found")
	case errors.Is(err, meters.ErrForbidden):
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "no access to this meter")
	default:
		s.mapErr(w, r, err)
	}
}
