// Package vtpasstest is a stateful fake of the VTpass electricity endpoints
// the adapter uses. It enforces api-key/secret-key headers, request_id rules,
// sandbox meter behaviours from the public docs, and can inject 5xx-after-apply
// faults so lost-response tests match production semantics.
package vtpasstest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
)

const (
	PrepaidMeter    = "1111111111111"
	PostpaidMeter   = "1010101010101"
	PendingMeter    = "201000000000"
	UnexpectedMeter = "500000000000"
	NoBodyMeter     = "400000000000"
)

type record struct {
	RequestID     string
	ServiceID     string
	Meter         string
	Variation     string
	Status        string // initiated, pending, delivered, failed
	Code          string
	TransactionID string
	Token         string
	Units         string
}

type Fake struct {
	*httptest.Server
	APIKey    string
	SecretKey string

	mu             sync.Mutex
	nextID         int64
	byRequest      map[string]*record
	failAfterApply map[string]int
	Requests       map[string]int
}

func New(apiKey, secretKey string) *Fake {
	f := &Fake{
		APIKey:         apiKey,
		SecretKey:      secretKey,
		nextID:         1700000000000,
		byRequest:      map[string]*record{},
		failAfterApply: map[string]int{},
		Requests:       map[string]int{},
	}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

func (f *Fake) FailAfterApply(prefix string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failAfterApply[prefix] = n
}

func (f *Fake) SetStatus(requestID, status, code string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.byRequest[requestID]; ok {
		r.Status = status
		if code != "" {
			r.Code = code
		}
	}
}

func (f *Fake) Has(requestID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.byRequest[requestID]
	return ok
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	key := r.Method + " " + r.URL.Path
	f.Requests[key]++

	if r.Header.Get("api-key") != f.APIKey || r.Header.Get("secret-key") != f.SecretKey {
		writeJSON(w, 200, map[string]any{"code": "087", "response_description": "INVALID CREDENTIALS"})
		return
	}

	lose := false
	for p, n := range f.failAfterApply {
		if n > 0 && strings.HasPrefix(key, p) {
			f.failAfterApply[p] = n - 1
			lose = true
		}
	}
	reply := func(v any) {
		if lose {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"code":"083","response_description":"SYSTEM ERROR"}`))
			return
		}
		writeJSON(w, 200, v)
	}

	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var payload map[string]any
	_ = json.Unmarshal(body, &payload)

	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/merchant-verify"):
		meter := stringify(payload["billersCode"])
		sid := stringify(payload["serviceID"])
		typ := stringify(payload["type"])
		if meter == "" || sid == "" || typ == "" {
			reply(map[string]any{"code": "011", "response_description": "INVALID ARGUMENTS"})
			return
		}
		switch {
		case meter == PrepaidMeter && typ == "prepaid":
			reply(map[string]any{
				"code": "000",
				"content": map[string]any{
					"Customer_Name":       "TESTMETER1",
					"Address":             "ABULE EGBA BU ABULE",
					"Meter_Number":        meter,
					"Meter_Type":          "PREPAID",
					"Can_Vend":            "yes",
					"Min_Purchase_Amount": "500",
					"WrongBillersCode":    false,
				},
			})
		case meter == PostpaidMeter && typ == "postpaid":
			reply(map[string]any{
				"code": "000",
				"content": map[string]any{
					"Customer_Name":    "NP NGEMA",
					"Address":          "6 ABIODUN ODESEYE Shomolu BU",
					"Meter_Number":     meter,
					"Meter_Type":       "POSTPAID",
					"Can_Vend":         "yes",
					"WrongBillersCode": false,
				},
			})
		default:
			reply(map[string]any{
				"code": "000",
				"content": map[string]any{
					"WrongBillersCode": true,
				},
			})
		}

	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pay"):
		rid := stringify(payload["request_id"])
		sid := stringify(payload["serviceID"])
		meter := stringify(payload["billersCode"])
		variation := stringify(payload["variation_code"])
		phone := stringify(payload["phone"])
		if rid == "" || sid == "" || meter == "" || variation == "" || phone == "" {
			reply(map[string]any{"code": "011", "response_description": "INVALID ARGUMENTS"})
			return
		}
		if !validRequestID(rid) {
			reply(map[string]any{"code": "085", "response_description": "IMPROPER REQUEST ID: DOES NOT CONTAIN DATE"})
			return
		}
		if _, dup := f.byRequest[rid]; dup {
			reply(map[string]any{"code": "014", "response_description": "REQUEST ID ALREADY EXIST", "requestId": rid})
			return
		}
		f.nextID++
		rec := &record{
			RequestID:     rid,
			ServiceID:     sid,
			Meter:         meter,
			Variation:     variation,
			TransactionID: strconv.FormatInt(f.nextID, 10),
			Code:          "000",
			Status:        "delivered",
		}
		switch meter {
		case PrepaidMeter:
			rec.Token = "26362054405982757802"
			rec.Units = "79.9 kWh"
			rec.Status = "delivered"
		case PostpaidMeter:
			rec.Status = "delivered"
		case PendingMeter:
			rec.Status = "pending"
			rec.Code = "099"
		case UnexpectedMeter:
			f.byRequest[rid] = rec
			if lose {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`not-json`))
			return
		case NoBodyMeter:
			f.byRequest[rid] = rec
			if lose {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.WriteHeader(200)
			return
		default:
			rec.Code = "016"
			rec.Status = "failed"
		}
		f.byRequest[rid] = rec
		reply(payBody(rec))

	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/requery"):
		rid := stringify(payload["request_id"])
		rec, ok := f.byRequest[rid]
		if !ok {
			reply(map[string]any{"code": "015", "response_description": "INVALID REQUEST ID"})
			return
		}
		reply(payBody(rec))

	default:
		http.NotFound(w, r)
	}
}

func payBody(rec *record) map[string]any {
	desc := "TRANSACTION SUCCESSFUL"
	if rec.Code == "099" {
		desc = "TRANSACTION IS PROCESSING"
	} else if rec.Code != "000" {
		desc = "TRANSACTION FAILED"
	}
	tx := map[string]any{
		"status":         rec.Status,
		"transactionId":  rec.TransactionID,
		"unique_element": rec.Meter,
	}
	body := map[string]any{
		"code":                 rec.Code,
		"response_description": desc,
		"requestId":            rec.RequestID,
		"content":              map[string]any{"transactions": tx},
	}
	if rec.Token != "" {
		tok := "Token : " + rec.Token
		body["purchased_code"] = tok
		body["token"] = tok
		body["units"] = rec.Units
		tx["extras"] = tok
	}
	return body
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func validRequestID(s string) bool {
	if len(s) < 12 {
		return false
	}
	for i := 0; i < 12; i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func stringify(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case json.Number:
		return t.String()
	default:
		b, _ := json.Marshal(v)
		return strings.Trim(string(b), `"`)
	}
}
