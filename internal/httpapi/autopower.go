package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/autopower"
	"github.com/teamthy/meterly/internal/orchestrator"
)

func (s *Server) requireAutoPower(w http.ResponseWriter) bool {
	if s.AutoPowerDisabled {
		writeErr(w, http.StatusNotFound, "FEATURE_DISABLED", "autopower is not enabled")
		return false
	}
	return true
}

func setNoStoreHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
}

// POST /v1/me/payment-methods/{transaction_id}/save
func (s *Server) savePaymentMethod(w http.ResponseWriter, r *http.Request) {
	setNoStoreHeaders(w)
	if !s.requireAutoPower(w) {
		return
	}
	txnID, err := uuid.Parse(r.PathValue("transaction_id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "TRANSACTION_NOT_FOUND", "transaction not found")
		return
	}
	var req struct {
		Consent     bool   `json:"consent"`
		ConsentText string `json:"consent_text"`
	}
	if !decode(r, &req) {
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid request body")
		return
	}
	if !s.requireAction(w, r, "SAVE_CARD") {
		return
	}
	pm, err := s.O.SavePaymentMethod(r.Context(), orchestrator.SavePaymentMethodParams{
		UserID:        userID(r),
		TransactionID: txnID,
		Consent:       req.Consent,
		ConsentText:   req.ConsentText,
		IP:            s.clientIP(r),
		UserAgent:     r.UserAgent(),
	})
	if err != nil {
		s.mapAutoPowerErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, pm)
}

// GET /v1/me/payment-methods
func (s *Server) listPaymentMethods(w http.ResponseWriter, r *http.Request) {
	setNoStoreHeaders(w)
	if !s.requireAutoPower(w) {
		return
	}
	list, err := s.O.ListPaymentMethods(r.Context(), userID(r))
	if err != nil {
		s.mapAutoPowerErr(w, r, err)
		return
	}
	if list == nil {
		list = []*autopower.SavedPaymentMethod{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"payment_methods": list})
}

// DELETE /v1/me/payment-methods/{id}
func (s *Server) deletePaymentMethod(w http.ResponseWriter, r *http.Request) {
	setNoStoreHeaders(w)
	if !s.requireAutoPower(w) {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "PAYMENT_METHOD_NOT_FOUND", "payment method not found")
		return
	}
	if err := s.O.DeletePaymentMethod(r.Context(), userID(r), id); err != nil {
		s.mapAutoPowerErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /v1/autopower/rules
func (s *Server) createAutoPowerRule(w http.ResponseWriter, r *http.Request) {
	setNoStoreHeaders(w)
	if !s.requireAutoPower(w) {
		return
	}
	var req struct {
		MeterID                uuid.UUID             `json:"meter_id"`
		PaymentMethodID        uuid.UUID             `json:"payment_method_id"`
		TriggerType            autopower.TriggerType `json:"trigger_type"`
		AmountMinor            int64                 `json:"amount_minor"`
		MonthlyCapMinor        int64                 `json:"monthly_cap_minor"`
		DeliveryPreference     string                `json:"delivery_preference"`
		Frequency              autopower.Frequency   `json:"frequency"`
		DaysOfWeek             []int                 `json:"days_of_week"`
		DaysOfMonth            []int                 `json:"days_of_month"`
		TimeOfDay              string                `json:"time_of_day"`
		Timezone               string                `json:"timezone"`
		EndsAt                 string                `json:"ends_at"`
		LowBalanceThresholdKWh *float64              `json:"low_balance_threshold_kwh"`
	}
	if !decode(r, &req) || req.MeterID == uuid.Nil || req.PaymentMethodID == uuid.Nil || req.AmountMinor <= 0 || req.MonthlyCapMinor <= 0 {
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "meter_id, payment_method_id, positive amount_minor and positive monthly_cap_minor are required")
		return
	}
	var endsAt *time.Time
	if strings.TrimSpace(req.EndsAt) != "" {
		parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(req.EndsAt))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "ends_at must be an RFC3339 timestamp")
			return
		}
		t := parsed.UTC()
		endsAt = &t
	}

	view, err := s.O.CreateAutoPowerRule(r.Context(), orchestrator.CreateRuleParams{
		UserID:                 userID(r),
		MeterID:                req.MeterID,
		PaymentMethodID:        req.PaymentMethodID,
		IP:                     s.clientIP(r),
		UserAgent:              r.UserAgent(),
		TriggerType:            req.TriggerType,
		AmountMinor:            req.AmountMinor,
		MonthlyCapMinor:        req.MonthlyCapMinor,
		DeliveryPreference:     orchestrator.DeliveryPreference(strings.ToUpper(strings.TrimSpace(req.DeliveryPreference))),
		Frequency:              autopower.Frequency(strings.ToUpper(strings.TrimSpace(string(req.Frequency)))),
		DaysOfWeek:             req.DaysOfWeek,
		DaysOfMonth:            req.DaysOfMonth,
		TimeOfDay:              req.TimeOfDay,
		Timezone:               req.Timezone,
		EndsAt:                 endsAt,
		LowBalanceThresholdKWh: req.LowBalanceThresholdKWh,
	})
	if err != nil {
		s.mapAutoPowerErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, view)
}

// GET /v1/autopower/rules
func (s *Server) listAutoPowerRules(w http.ResponseWriter, r *http.Request) {
	setNoStoreHeaders(w)
	if !s.requireAutoPower(w) {
		return
	}
	list, err := s.O.ListAutoPowerRules(r.Context(), userID(r))
	if err != nil {
		s.mapAutoPowerErr(w, r, err)
		return
	}
	if list == nil {
		list = []*orchestrator.RuleView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"rules": list})
}

type optionalRFC3339 struct {
	present bool
	null    bool
	value   string
}

func (o *optionalRFC3339) UnmarshalJSON(data []byte) error {
	o.present = true
	trimmed := bytes.TrimSpace(data)
	if bytes.Equal(trimmed, []byte("null")) {
		o.null = true
		return nil
	}
	var s string
	if err := json.Unmarshal(trimmed, &s); err != nil {
		return err
	}
	o.value = strings.TrimSpace(s)
	if o.value == "" {
		o.null = true
	}
	return nil
}

// PATCH /v1/autopower/rules/{id}
func (s *Server) updateAutoPowerRule(w http.ResponseWriter, r *http.Request) {
	setNoStoreHeaders(w)
	if !s.requireAutoPower(w) {
		return
	}
	ruleID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "not found")
		return
	}
	var req struct {
		PaymentMethodID        *uuid.UUID      `json:"payment_method_id"`
		TriggerType            *string         `json:"trigger_type"`
		AmountMinor            *int64          `json:"amount_minor"`
		MonthlyCapMinor        *int64          `json:"monthly_cap_minor"`
		DeliveryPreference     *string         `json:"delivery_preference"`
		Frequency              *string         `json:"frequency"`
		DaysOfWeek             *[]int          `json:"days_of_week"`
		DaysOfMonth            *[]int          `json:"days_of_month"`
		TimeOfDay              *string         `json:"time_of_day"`
		EndsAt                 optionalRFC3339 `json:"ends_at"`
		LowBalanceThresholdKWh *float64        `json:"low_balance_threshold_kwh"`
		Status                 *string         `json:"status"`
	}
	if !decode(r, &req) {
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid request body")
		return
	}

	params := orchestrator.UpdateRuleParams{
		UserID:                 userID(r),
		RuleID:                 ruleID,
		PaymentMethodID:        req.PaymentMethodID,
		AmountMinor:            req.AmountMinor,
		MonthlyCapMinor:        req.MonthlyCapMinor,
		DaysOfWeek:             req.DaysOfWeek,
		DaysOfMonth:            req.DaysOfMonth,
		TimeOfDay:              req.TimeOfDay,
		LowBalanceThresholdKWh: req.LowBalanceThresholdKWh,
	}
	if req.TriggerType != nil {
		tt := autopower.TriggerType(strings.ToUpper(strings.TrimSpace(*req.TriggerType)))
		params.TriggerType = &tt
	}
	if req.DeliveryPreference != nil {
		dp := orchestrator.DeliveryPreference(strings.ToUpper(strings.TrimSpace(*req.DeliveryPreference)))
		params.DeliveryPreference = &dp
	}
	if req.Frequency != nil {
		fr := autopower.Frequency(strings.ToUpper(strings.TrimSpace(*req.Frequency)))
		params.Frequency = &fr
	}
	if req.Status != nil {
		st := autopower.RuleStatus(strings.ToUpper(strings.TrimSpace(*req.Status)))
		params.Status = &st
	}
	if req.EndsAt.present {
		if req.EndsAt.null {
			params.ClearEndsAt = true
		} else {
			parsed, err := time.Parse(time.RFC3339Nano, req.EndsAt.value)
			if err != nil {
				writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "ends_at must be an RFC3339 timestamp or null")
				return
			}
			t := parsed.UTC()
			params.EndsAt = &t
		}
	}

	view, err := s.O.UpdateAutoPowerRule(r.Context(), params)
	if err != nil {
		s.mapAutoPowerErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// DELETE /v1/autopower/rules/{id}
func (s *Server) deleteAutoPowerRule(w http.ResponseWriter, r *http.Request) {
	setNoStoreHeaders(w)
	if !s.requireAutoPower(w) {
		return
	}
	ruleID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "not found")
		return
	}
	if err := s.O.DeleteAutoPowerRule(r.Context(), userID(r), ruleID); err != nil {
		s.mapAutoPowerErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /v1/autopower/runs/{id}/skip
func (s *Server) skipAutoPowerRun(w http.ResponseWriter, r *http.Request) {
	setNoStoreHeaders(w)
	if !s.requireAutoPower(w) {
		return
	}
	runID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "not found")
		return
	}
	rn, err := s.O.SkipAutoPowerRun(r.Context(), userID(r), runID)
	if err != nil {
		s.mapAutoPowerErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rn)
}

func (s *Server) mapAutoPowerErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, autopower.ErrConsentRequired):
		writeErr(w, http.StatusBadRequest, "CONSENT_REQUIRED", err.Error())
	case errors.Is(err, autopower.ErrPaymentMethodNotReusable), errors.Is(err, autopower.ErrInvalidCard):
		writeErr(w, http.StatusUnprocessableEntity, "PAYMENT_METHOD_NOT_REUSABLE", err.Error())
	case errors.Is(err, autopower.ErrCardExpired):
		writeErr(w, http.StatusUnprocessableEntity, "CARD_EXPIRED", err.Error())
	case errors.Is(err, autopower.ErrPaymentMethodNotFound), errors.Is(err, autopower.ErrPaymentMethodDeleted):
		writeErr(w, http.StatusNotFound, "PAYMENT_METHOD_NOT_FOUND", "payment method not found")
	case errors.Is(err, autopower.ErrRuleNotFound), errors.Is(err, autopower.ErrRunNotFound):
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "not found")
	case errors.Is(err, autopower.ErrLowBalanceIneligible):
		writeErr(w, http.StatusUnprocessableEntity, "METER_NOT_ELIGIBLE", err.Error())
	case errors.Is(err, autopower.ErrInvalidCap),
		errors.Is(err, autopower.ErrInvalidSchedule),
		errors.Is(err, autopower.ErrInvalidTrigger),
		errors.Is(err, autopower.ErrInvalidTimezone),
		errors.Is(err, autopower.ErrLowBalanceThreshold):
		writeErr(w, http.StatusUnprocessableEntity, "INVALID_REQUEST", err.Error())
	case errors.Is(err, autopower.ErrRunNotSkippable):
		writeErr(w, http.StatusConflict, "RUN_NOT_SKIPPABLE", err.Error())
	case errors.Is(err, autopower.ErrRuleNotEditable):
		writeErr(w, http.StatusConflict, "INVALID_REQUEST", err.Error())
	default:
		s.mapErr(w, r, err)
	}
}
