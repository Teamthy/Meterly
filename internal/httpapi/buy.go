package httpapi

import (
	"context"
	"net/http"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/orchestrator"
	tx "github.com/teamthy/meterly/internal/transactions"
)

func (s *Server) getCatalog(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.O.Catalog())
}

func (s *Server) getBuyOptions(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("meter_id"))
	if err != nil {
		writeErr(w, 404, "METER_NOT_FOUND", "meter not found")
		return
	}
	opts, err := s.O.BuyOptions(r.Context(), userID(r), id, r.URL.Query().Get("band"))
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, opts)
}

func quoteView(q *tx.Quote, conf orchestrator.Screen) map[string]any {
	return map[string]any{
		"quote_id":            q.ID,
		"meter_id":            q.MeterID,
		"amount":              q.Amount,
		"fee":                 q.Fee,
		"total":               q.Total,
		"amount_minor":        q.Amount.Minor,
		"fee_minor":           q.Fee.Minor,
		"total_minor":         q.Total.Minor,
		"currency":            q.Total.Currency,
		"pack_id":             q.PackID,
		"disco_code":          q.DiscoCode,
		"tariff_band":         q.TariffBand,
		"kobo_per_kwh":        q.KoboPerKWh,
		"estimated_units_kwh": orchestrator.EstimatedUnitsKWh(q),
		"credit_minor":        q.CreditMinor,
		"discount_minor":      q.DiscountMinor,
		"delivery_method":     q.DeliveryMethod,
		"delivery_options":    q.DeliveryOptions,
		"expires_at":          q.ExpiresAt,
		"confirmation":        conf,
	}
}

func (s *Server) viewTxn(ctx context.Context, t *tx.Transaction) map[string]any {
	v := view(t)
	m, _ := s.O.Meters.Get(ctx, t.MeterID)
	q, err := s.O.GetQuote(ctx, t.CustomerID, t.QuoteID)
	if err != nil {
		q = nil
	}
	v["checkout"] = orchestrator.CheckoutScreen(t, q, m)
	if q != nil {
		v["estimated_units_kwh"] = orchestrator.EstimatedUnitsKWh(q)
		v["kobo_per_kwh"] = q.KoboPerKWh
		v["tariff_band"] = q.TariffBand
		v["pack_id"] = q.PackID
	}
	return v
}
