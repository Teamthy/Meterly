package httpapi

import (
	"bytes"
	"fmt"
	"html/template"
	"net/http"

	"github.com/teamthy/meterly/internal/receipts"
)

var receiptHTML = template.Must(template.New("receipt").Funcs(template.FuncMap{"minor": func(v int64) string { return fmt.Sprintf("%d.%02d", v/100, v%100) }}).Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Meterly transaction record</title></head>
<body><main><h1>Meterly transaction record</h1>
<p>This record reflects the current transaction states. A remote delivery is confirmed only when the meter has acknowledged it.</p>
<dl><dt>Reference</dt><dd>{{.TransactionID}}</dd><dt>Meter</dt><dd>{{.MaskedMeterNumber}}</dd><dt>Distribution company</dt><dd>{{.Disco}}</dd><dt>Purchase amount</dt><dd>{{.Currency}} {{minor .AmountMinor}}</dd><dt>Fee</dt><dd>{{.Currency}} {{minor .FeeMinor}}</dd><dt>Total</dt><dd>{{.Currency}} {{minor .TotalMinor}}</dd><dt>Payment</dt><dd>{{.PaymentStatus}}</dd><dt>Vending</dt><dd>{{.VendingStatus}}</dd><dt>Delivery</dt><dd>{{.DeliveryStatus}}</dd><dt>Delivery method</dt><dd>{{.DeliveryMethod}}</dd><dt>Overall</dt><dd>{{.OverallStatus}}</dd><dt>Created (UTC)</dt><dd>{{.CreatedAt.Format "2006-01-02 15:04:05 UTC"}}</dd></dl></main></body></html>`))

// receipt is purchaser-only and never reveals the STS token. The JSON record
// is available while processing too; the separate states remain explicit.
func (s *Server) receipt(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	format := r.URL.Query().Get("format")
	if format != "" && format != "json" && format != "html" {
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "unsupported receipt format")
		return
	}
	t, ok := s.txnFromPath(w, r)
	if !ok {
		return
	}
	meter, err := s.O.Meters.Get(r.Context(), t.MeterID)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	receipt, err := receipts.Build(t, meter)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "INTERNAL_ERROR", "receipt unavailable")
		return
	}
	if format != "html" {
		writeJSON(w, http.StatusOK, receipt)
		return
	}
	var buf bytes.Buffer
	if err := receiptHTML.Execute(&buf, receipt); err != nil {
		writeErr(w, http.StatusInternalServerError, "INTERNAL_ERROR", "receipt unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; base-uri 'none'; frame-ancestors 'none'")
	_, _ = w.Write(buf.Bytes())
}
