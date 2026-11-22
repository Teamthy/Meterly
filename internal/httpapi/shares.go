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
		Channel string `json:"channel"`
	}
	if !decode(r, &body) || body.Channel != tokenshares.Copy {
		writeErr(w, 400, "INVALID_REQUEST", "only the COPY channel is supported; no message is sent")
		return
	}
	share, err := s.O.RecordTokenCopy(r.Context(), userID(r), id, key)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"share": share, "note": "Copy action reported by the purchaser. This does not confirm copying or delivery. The token works only on its own meter; sharing does not transfer credit between meters."})
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
