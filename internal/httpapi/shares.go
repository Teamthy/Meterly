package httpapi

import (
	"net/http"

	"github.com/teamthy/meterly/internal/tokenshares"
)

func (s *Server) recordTokenCopy(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	id, ok := txnID(w, r)
	if !ok {
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) < 16 || len(key) > 128 {
		writeErr(w, 400, "INVALID_REQUEST", "Idempotency-Key header (16–128 chars) is required")
		return
	}
	var body struct {
		Channel     string `json:"channel"`
		Destination string `json:"destination"`
	}
	if !decode(r, &body) {
		writeErr(w, 400, "INVALID_REQUEST", "invalid request body")
		return
	}
	ch, err := tokenshares.ParseChannel(body.Channel)
	if err != nil {
		writeErr(w, 400, "INVALID_REQUEST", "channel must be COPY, SMS, EMAIL or WHATSAPP")
		return
	}
	if ch == tokenshares.Copy {
		if body.Destination != "" {
			writeErr(w, 400, "INVALID_REQUEST", "COPY does not accept a destination")
			return
		}
		share, err := s.O.RecordTokenCopy(r.Context(), userID(r), id, key)
		if err != nil {
			s.mapErr(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{
			"share": share,
			"note":  "Copy action reported by the purchaser. This does not confirm copying or delivery. The token works only on its own meter; sharing does not transfer credit between meters.",
		})
		return
	}
	share, err := s.O.RecordTokenSend(r.Context(), userID(r), id, key, ch, body.Destination)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"share": share,
		"note":  "Token send accepted. Delivery is tracked separately and is not proof the recipient received or entered the token. The token works only on its own meter; sharing does not transfer credit between meters.",
	})
}
func (s *Server) listTokenCopies(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	id, ok := txnID(w, r)
	if !ok {
		return
	}
	shares, err := s.O.ListTokenCopies(r.Context(), userID(r), id)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": shares})
}
