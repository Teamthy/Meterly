package httpapi

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/audit"
	"github.com/teamthy/meterly/internal/idempotency"
	"github.com/teamthy/meterly/internal/orchestrator"
	"github.com/teamthy/meterly/internal/partners"
	"github.com/teamthy/meterly/internal/security"
	tx "github.com/teamthy/meterly/internal/transactions"
)

func (s *Server) requirePartnerFloat(w http.ResponseWriter) bool {
	if s.PartnerFloatDisabled {
		writeErr(w, http.StatusNotFound, "FEATURE_DISABLED", "holding partner float is not enabled")
		return false
	}
	return true
}

func partnerIDOf(r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(callerOf(r).Principal.PartnerID)
	return id, err == nil
}

func (s *Server) partnerCreateQuote(w http.ResponseWriter, r *http.Request) {
	pid, ok := partnerIDOf(r)
	if !ok {
		writeErr(w, 401, "UNAUTHENTICATED", "authentication required")
		return
	}
	var req struct {
		MeterID            uuid.UUID                       `json:"meter_id"`
		MeterNumber        string                          `json:"meter_number"`
		DiscoCode          string                          `json:"disco_code"`
		AmountMinor        int64                           `json:"amount_minor"`
		PackID             string                          `json:"pack_id"`
		Currency           string                          `json:"currency"`
		DeliveryPreference orchestrator.DeliveryPreference `json:"delivery_preference"`
	}
	if !decode(r, &req) || req.Currency != "NGN" {
		writeErr(w, 400, "INVALID_REQUEST", "currency=NGN and meter_id or meter_number+disco_code are required")
		return
	}
	if req.PackID == "" && req.AmountMinor <= 0 {
		writeErr(w, 400, "INVALID_REQUEST", "pack_id or amount_minor is required")
		return
	}
	q, err := s.O.IssuePartnerQuote(r.Context(), orchestrator.PartnerQuoteRequest{
		PartnerID: pid, MeterID: req.MeterID, MeterNumber: req.MeterNumber, DiscoCode: req.DiscoCode,
		AmountMinor: req.AmountMinor, PackID: req.PackID, Pref: req.DeliveryPreference,
	})
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	m, _ := s.O.Meters.Get(r.Context(), q.MeterID)
	writeJSON(w, 201, quoteView(q, orchestrator.QuoteConfirmation(q, m)))
}

func (s *Server) partnerCreatePurchase(w http.ResponseWriter, r *http.Request) {
	if !s.requirePartnerFloat(w) {
		return
	}
	pid, ok := partnerIDOf(r)
	if !ok {
		writeErr(w, 401, "UNAUTHENTICATED", "authentication required")
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) < 16 || len(key) > 128 {
		writeErr(w, 400, "INVALID_REQUEST", "Idempotency-Key header (16–128 chars) is required")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, 400, "INVALID_REQUEST", "unreadable body")
		return
	}
	var req struct {
		QuoteID    uuid.UUID `json:"quote_id"`
		PartnerRef string    `json:"partner_ref"`
	}
	if json.Unmarshal(body, &req) != nil || req.QuoteID == uuid.Nil {
		writeErr(w, 400, "INVALID_REQUEST", "quote_id and partner_ref are required")
		return
	}
	t, replayed, err := s.O.CreatePartnerPurchase(r.Context(), orchestrator.PartnerPurchaseRequest{
		PartnerID: pid, QuoteID: req.QuoteID, PartnerRef: req.PartnerRef,
		IdempotencyKey: key, RequestHash: idempotency.HashRequest(body),
	})
	if err != nil && t == nil {
		s.mapErr(w, r, err)
		return
	}
	code := 201
	if replayed {
		code = 200
	}
	writeJSON(w, code, s.partnerPurchaseView(r, t, true))
}

func (s *Server) partnerGetPurchase(w http.ResponseWriter, r *http.Request) {
	pid, ok := partnerIDOf(r)
	if !ok {
		writeErr(w, 401, "UNAUTHENTICATED", "authentication required")
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, "TRANSACTION_NOT_FOUND", "transaction not found")
		return
	}
	t, err := s.O.GetPartnerPurchase(r.Context(), pid, id)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, s.partnerPurchaseView(r, t, true))
}

func (s *Server) partnerLookupPurchase(w http.ResponseWriter, r *http.Request) {
	pid, ok := partnerIDOf(r)
	if !ok {
		writeErr(w, 401, "UNAUTHENTICATED", "authentication required")
		return
	}
	ref := r.URL.Query().Get("partner_ref")
	if ref == "" {
		writeErr(w, 400, "INVALID_REQUEST", "partner_ref is required")
		return
	}
	t, err := s.O.GetPartnerPurchaseByRef(r.Context(), pid, ref)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, s.partnerPurchaseView(r, t, true))
}

func (s *Server) partnerGetFloat(w http.ResponseWriter, r *http.Request) {
	if !s.requirePartnerFloat(w) {
		return
	}
	pid, ok := partnerIDOf(r)
	if !ok {
		writeErr(w, 401, "UNAUTHENTICATED", "authentication required")
		return
	}
	f, err := s.O.PartnerFloat(r.Context(), pid)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	p, err := s.O.Store().Repos().Partners().GetPartner(r.Context(), pid)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, floatView(f, p))
}

func (s *Server) partnerPurchaseView(r *http.Request, t *tx.Transaction, allowToken bool) map[string]any {
	v := view(t)
	v["partner_ref"] = t.PartnerRef
	v["funding_source"] = string(t.EffectiveFundingSource())
	delete(v, "checkout_url")
	if !allowToken {
		return v
	}
	p := callerOf(r).Principal
	if !p.HasScope(partners.ScopePurchasesCreate) && !p.HasScope(partners.ScopePurchasesRead) {
		return v
	}
	if t.DeliveryMethod != tx.MethodToken || t.Vending != tx.VendConfirmed || len(t.TokenCiphertext) == 0 {
		return v
	}
	pid, err := uuid.Parse(p.PartnerID)
	if err != nil {
		return v
	}
	tok, _, err := s.O.PartnerToken(r.Context(), pid, t.ID)
	if err != nil || tok == "" {
		return v
	}
	v["token"] = tok
	return v
}

func floatView(f partners.Float, p partners.Partner) map[string]any {
	cur := f.Currency
	if cur == "" {
		cur = "NGN"
	}
	return map[string]any{
		"partner_id":            p.ID.String(),
		"available_minor":       f.AvailableMinor,
		"currency":              cur,
		"daily_value_cap_minor": p.DailyValueCapMinor,
	}
}

func topupView(t partners.FloatTopup) map[string]any {
	out := map[string]any{
		"id":               t.ID.String(),
		"partner_id":       t.PartnerID.String(),
		"receipt_ref":      t.ReceiptRef,
		"amount_minor":     t.AmountMinor,
		"currency":         t.Currency,
		"status":           t.Status,
		"proposed_by":      t.ProposedBy,
		"proposed_at":      t.ProposedAt.UTC(),
		"approved_by":      t.ApprovedBy,
		"rejected_by":      t.RejectedBy,
		"ledger_reference": t.LedgerReference,
	}
	if t.ApprovedAt != nil {
		out["approved_at"] = t.ApprovedAt.UTC()
	}
	if t.RejectedAt != nil {
		out["rejected_at"] = t.RejectedAt.UTC()
	}
	return out
}

func (s *Server) adminGetPartnerFloat(w http.ResponseWriter, r *http.Request) {
	if !s.requirePartnerAPI(w) || !s.requirePartnerFloat(w) {
		return
	}
	id, ok := s.parsePartnerID(w, r)
	if !ok {
		return
	}
	f, err := s.O.PartnerFloat(r.Context(), id)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	p, err := s.O.Store().Repos().Partners().GetPartner(r.Context(), id)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, floatView(f, p))
}

func (s *Server) adminProposePartnerFloatTopup(w http.ResponseWriter, r *http.Request) {
	if !s.requirePartnerAPI(w) || !s.requirePartnerFloat(w) || !s.requireAction(w, r, security.ActionPartnerAdmin) {
		return
	}
	id, ok := s.parsePartnerID(w, r)
	if !ok {
		return
	}
	var body struct {
		Reason      string `json:"reason"`
		ReceiptRef  string `json:"receipt_ref"`
		AmountMinor int64  `json:"amount_minor"`
		Currency    string `json:"currency"`
	}
	if !decode(r, &body) || len(body.Reason) < 5 || len(body.Reason) > 500 {
		writeErr(w, 400, "INVALID_REQUEST", "a reason (5–500 characters) is required")
		return
	}
	actor := audit.Actor{UserID: userID(r), Type: audit.ActorOperator, Reason: body.Reason, CorrelationID: audit.CorrelationFrom(r.Context())}
	t, err := s.O.ProposePartnerFloatTopup(r.Context(), actor, id, body.ReceiptRef, body.AmountMinor, body.Currency)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 201, topupView(t))
}

func (s *Server) parseTopupID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("topup_id"))
	if err != nil {
		writeErr(w, 404, "NOT_FOUND", "not found")
		return uuid.Nil, false
	}
	return id, true
}

func (s *Server) adminApprovePartnerFloatTopup(w http.ResponseWriter, r *http.Request) {
	if !s.requirePartnerAPI(w) || !s.requirePartnerFloat(w) || !s.requireAction(w, r, security.ActionPartnerAdmin) {
		return
	}
	actor, ok := s.actor(w, r)
	if !ok {
		return
	}
	pid, ok := s.parsePartnerID(w, r)
	if !ok {
		return
	}
	tid, ok := s.parseTopupID(w, r)
	if !ok {
		return
	}
	existing, err := s.O.Store().Repos().Partners().GetTopup(r.Context(), tid)
	if err != nil || existing.PartnerID != pid {
		s.mapErr(w, r, partners.ErrNotFound)
		return
	}
	t, err := s.O.ApprovePartnerFloatTopup(r.Context(), actor, tid)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, topupView(t))
}

func (s *Server) adminRejectPartnerFloatTopup(w http.ResponseWriter, r *http.Request) {
	if !s.requirePartnerAPI(w) || !s.requirePartnerFloat(w) || !s.requireAction(w, r, security.ActionPartnerAdmin) {
		return
	}
	actor, ok := s.actor(w, r)
	if !ok {
		return
	}
	pid, ok := s.parsePartnerID(w, r)
	if !ok {
		return
	}
	tid, ok := s.parseTopupID(w, r)
	if !ok {
		return
	}
	existing, err := s.O.Store().Repos().Partners().GetTopup(r.Context(), tid)
	if err != nil || existing.PartnerID != pid {
		s.mapErr(w, r, partners.ErrNotFound)
		return
	}
	t, err := s.O.RejectPartnerFloatTopup(r.Context(), actor, tid)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, topupView(t))
}
