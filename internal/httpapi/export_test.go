package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/auth"
	"github.com/teamthy/meterly/internal/dataexport"
	"github.com/teamthy/meterly/internal/households"
	"github.com/teamthy/meterly/internal/httpapi"
	"github.com/teamthy/meterly/internal/identity"
	"github.com/teamthy/meterly/internal/notifications"
	"github.com/teamthy/meterly/internal/orchestrator"
	"github.com/teamthy/meterly/internal/payrequests"
	"github.com/teamthy/meterly/internal/providers/mock"
	"github.com/teamthy/meterly/internal/registry"
	"github.com/teamthy/meterly/internal/secure"
	"github.com/teamthy/meterly/internal/security"
	"github.com/teamthy/meterly/internal/store"
	"github.com/teamthy/meterly/internal/store/memstore"
	"github.com/teamthy/meterly/internal/store/pgstore"
	"github.com/teamthy/meterly/internal/store/pgstore/pgtest"
	"github.com/teamthy/meterly/internal/support"
)

func TestDataExportIsolationSingleUseAndExpiry(t *testing.T) {
	for _, b := range []struct {
		name string
		open func(*testing.T) store.Store
	}{
		{"memory", func(*testing.T) store.Store { return memstore.New() }},
		{"postgres", func(t *testing.T) store.Store { return pgstore.New(pgtest.NewPool(t), "SANDBOX") }},
	} {
		t.Run(b.name, func(t *testing.T) {
			st := b.open(t)
			ctx := context.Background()
			box, _ := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "t1")
			pay, vend, remote := mock.NewPayments(), mock.NewVending(), mock.NewRemoteCredit()
			log := slog.New(slog.NewTextHandler(io.Discard, nil))
			o := orchestrator.New(orchestrator.DefaultConfig(), st, registry.New(st, box, vend, remote), box, pay, vend, remote, log)
			s := &httpapi.Server{O: o, Auth: auth.DevAuth{}, Payments: pay, Log: log, Dev: true, DevPay: pay, DevRemote: remote}
			srv := httptest.NewServer(s.Routes())
			defer srv.Close()
			c := client{t, srv}

			const adaMeter, bobMeter = "45012345678", "61100000999"
			const adaEmail, bobEmail = "ada-export-subject@example.com", "bob-export-isolation@example.com"
			code, mAda := c.do("POST", "/v1/meters", "ada", map[string]any{"meter_number": adaMeter, "disco_code": "IKEDC", "label": "AdaHome"}, nil)
			if code != 201 {
				t.Fatalf("ada meter %d %v", code, mAda)
			}
			code, mBob := c.do("POST", "/v1/meters", "bob", map[string]any{"meter_number": bobMeter, "disco_code": "EKEDC", "label": "BobHome"}, nil)
			if code != 201 {
				t.Fatalf("bob meter %d %v", code, mBob)
			}

			code, q := c.do("POST", "/v1/electricity/quotes", "ada", map[string]any{"meter_id": mAda["meter_id"], "amount_minor": 500000, "currency": "NGN"}, nil)
			if code != 201 {
				t.Fatalf("quote %d %v", code, q)
			}
			code, txn := c.do("POST", "/v1/electricity/transactions", "ada", map[string]any{"quote_id": q["quote_id"], "email": adaEmail}, map[string]string{"Idempotency-Key": "export-ada-0123456789ab"})
			if code != 202 {
				t.Fatalf("txn %d %v", code, txn)
			}
			ref := txn["checkout_url"].(string)[strings.LastIndex(txn["checkout_url"].(string), "/")+1:]
			c.do("POST", "/v1/dev/payments/"+ref+"/complete", "", map[string]any{}, nil)
			if code, _ := c.do("POST", "/v1/payments/webhooks/mockpay", "", map[string]any{"reference": ref}, map[string]string{"X-Mock-Signature": "dev-signature"}); code != 200 {
				t.Fatalf("webhook %d", code)
			}
			code, tok := c.do("GET", "/v1/electricity/transactions/"+txn["transaction_id"].(string)+"/token", "ada", nil, nil)
			if code != 200 {
				t.Fatalf("token %d %v", code, tok)
			}
			rawToken, _ := tok["token"].(string)
			if rawToken == "" {
				t.Fatal("empty token")
			}

			adaUID, err := st.Repos().Users().EnsureByExternalID(ctx, "ada")
			if err != nil {
				t.Fatal(err)
			}
			bobUID, err := st.Repos().Users().EnsureByExternalID(ctx, "bob")
			if err != nil {
				t.Fatal(err)
			}
			ctAda, hmAda, maskAda, err := notifications.SealUserEmail(box, adaUID, adaEmail)
			if err != nil {
				t.Fatal(err)
			}
			ctBob, hmBob, maskBob, err := notifications.SealUserEmail(box, bobUID, bobEmail)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.Repos().Users().UpsertContacts(ctx, notifications.UserContacts{
				UserID: adaUID, EmailCiphertext: ctAda, EmailHMAC: hmAda, EmailMasked: maskAda, EmailKeyVersion: box.KeyVersion, DisplayName: "Ada Subject",
			}); err != nil {
				t.Fatal(err)
			}
			if err := st.Repos().Users().UpsertContacts(ctx, notifications.UserContacts{
				UserID: bobUID, EmailCiphertext: ctBob, EmailHMAC: hmBob, EmailMasked: maskBob, EmailKeyVersion: box.KeyVersion, DisplayName: "BobExportIsolation",
			}); err != nil {
				t.Fatal(err)
			}

			now := time.Now().UTC()
			hid := uuid.New()
			if err := st.Repos().Households().Create(ctx, households.Household{ID: hid, OwnerID: adaUID, Name: "SharedFlat", CreatedAt: now, UpdatedAt: now}); err != nil {
				t.Fatal(err)
			}
			if err := st.Repos().Households().UpsertMember(ctx, households.Member{
				ID: uuid.New(), HouseholdID: hid, UserID: adaUID, Role: households.RoleOwner, Status: households.StatusActive, InvitedAt: now,
			}); err != nil {
				t.Fatal(err)
			}
			if err := st.Repos().Households().UpsertMember(ctx, households.Member{
				ID: uuid.New(), HouseholdID: hid, UserID: bobUID, Role: households.RoleMember, Status: households.StatusActive, InvitedAt: now,
			}); err != nil {
				t.Fatal(err)
			}

			code, preq := c.do("POST", "/v1/payment-requests", "ada", map[string]any{
				"meter_id": mAda["meter_id"], "amount_mode": "FIXED", "amount_minor": 200000, "expires_in_hours": 24,
			}, nil)
			if code != 201 {
				t.Fatalf("pay request %d %v", code, preq)
			}
			prID, _ := uuid.Parse(preq["id"].(string))
			txnID, _ := uuid.Parse(txn["transaction_id"].(string))
			payerCT, err := box.EncryptBound(bobEmail, []byte("payer-email:"+txnID.String()))
			if err != nil {
				t.Fatal(err)
			}
			if err := st.Repos().PayRequests().CreatePayer(ctx, payrequests.Payer{
				TransactionID: txnID, PaymentRequestID: prID,
				EmailCiphertext: payerCT, EmailKeyVersion: box.KeyVersion,
				StatusCodeHMAC: "payer-hmac-" + uuid.NewString(), CreatedAt: now,
			}); err != nil {
				t.Fatal(err)
			}

			thAda := &support.Thread{ID: uuid.New(), CustomerID: adaUID, Mode: support.ModeAgent, Status: support.StatusOpen, CreatedAt: now, UpdatedAt: now, LastMessageAt: now}
			thBob := &support.Thread{ID: uuid.New(), CustomerID: bobUID, Mode: support.ModeAgent, Status: support.StatusOpen, CreatedAt: now, UpdatedAt: now, LastMessageAt: now}
			if err := st.Repos().Support().CreateThread(ctx, thAda); err != nil {
				t.Fatal(err)
			}
			if err := st.Repos().Support().CreateThread(ctx, thBob); err != nil {
				t.Fatal(err)
			}
			msgID := uuid.New()
			bodyEnc, err := box.EncryptBound("ada agent reply about units", []byte("support-msg:"+thAda.ID.String()+":"+msgID.String()))
			if err != nil {
				t.Fatal(err)
			}
			if err := st.Repos().Support().AddMessage(ctx, &support.Message{
				ID: msgID, ThreadID: thAda.ID, SenderKind: support.SenderAgent, BodyEnc: bodyEnc, CreatedAt: now,
			}); err != nil {
				t.Fatal(err)
			}
			if err := st.Repos().Auth().InsertSecurityEvent(ctx, security.Event{
				ID: uuid.New(), UserID: adaUID, Action: security.ActionLogin, Outcome: security.OutcomeAllow, Reason: "ok", CreatedAt: now,
			}); err != nil {
				t.Fatal(err)
			}

			code, job := c.do("POST", "/v1/me/data-export", "ada", map[string]any{}, nil)
			if code != 201 && code != 200 {
				t.Fatalf("create export %d %v", code, job)
			}
			if job["status"] != "PENDING" {
				t.Fatalf("want PENDING, got %v", job)
			}
			exportID := job["export_id"].(string)
			dlTok, _ := job["download_token"].(string)
			if dlTok == "" {
				t.Fatal("missing download token")
			}
			code, again := c.do("POST", "/v1/me/data-export", "ada", map[string]any{}, nil)
			if code != 200 || again["export_id"] != exportID {
				t.Fatalf("idempotent create %d %v", code, again)
			}

			n, err := dataexport.Process(ctx, st, box, time.Now().UTC(), 20, log)
			if err != nil || n < 1 {
				t.Fatalf("process n=%d err=%v", n, err)
			}
			code, ready := c.do("GET", "/v1/me/data-export/"+exportID, "ada", nil, nil)
			if code != 200 || ready["status"] != "READY" {
				t.Fatalf("ready %d %v", code, ready)
			}
			if code, _ := c.do("GET", "/v1/me/data-export/"+exportID, "bob", nil, nil); code != 404 {
				t.Fatalf("bob must not read ada export: %d", code)
			}

			code, arch := c.do("GET", "/v1/me/data-export/"+exportID+"/download", "ada", nil, map[string]string{"X-Export-Token": dlTok})
			if code != 200 {
				t.Fatalf("download %d %v", code, arch)
			}
			raw, _ := json.Marshal(arch)
			blob := string(raw)
			if !strings.Contains(blob, `"email":"`+adaEmail+`"`) && !strings.Contains(blob, adaEmail) {
				t.Fatalf("ada archive missing ada email: %s", blob)
			}
			if !strings.Contains(blob, "ada agent reply about units") {
				t.Fatalf("missing decrypted support body: %s", blob)
			}
			if !strings.Contains(blob, "SharedFlat") {
				t.Fatalf("missing household: %s", blob)
			}
			forbidden := []string{bobEmail, bobMeter, adaMeter, rawToken, "BobExportIsolation", bobUID}
			for _, f := range forbidden {
				if f != "" && strings.Contains(blob, f) {
					t.Fatalf("ada archive leaked %q", f)
				}
			}
			if strings.Contains(strings.ToLower(blob), "token") && strings.Contains(blob, rawToken) {
				t.Fatal("token in archive")
			}

			code, used := c.do("GET", "/v1/me/data-export/"+exportID+"/download", "ada", nil, map[string]string{"X-Export-Token": dlTok})
			if code != 410 {
				t.Fatalf("second download want 410, got %d %v", code, used)
			}

			code, bobJob := c.do("POST", "/v1/me/data-export", "bob", map[string]any{}, nil)
			if code != 201 && code != 200 {
				t.Fatalf("bob export %d %v", code, bobJob)
			}
			if _, err := dataexport.Process(ctx, st, box, time.Now().UTC(), 20, log); err != nil {
				t.Fatal(err)
			}
			bobTok, _ := bobJob["download_token"].(string)
			code, bobArch := c.do("GET", "/v1/me/data-export/"+bobJob["export_id"].(string)+"/download", "bob", nil, map[string]string{"X-Export-Token": bobTok})
			if code != 200 {
				t.Fatalf("bob download %d %v", code, bobArch)
			}
			bobRaw, _ := json.Marshal(bobArch)
			bobBlob := string(bobRaw)
			if strings.Contains(bobBlob, adaEmail) || strings.Contains(bobBlob, adaMeter) || strings.Contains(bobBlob, rawToken) || strings.Contains(bobBlob, "ada agent reply") {
				t.Fatalf("bob archive leaked ada data: %s", bobBlob)
			}
			if !strings.Contains(bobBlob, bobEmail) {
				t.Fatalf("bob archive missing bob email: %s", bobBlob)
			}

			recent, err := st.Repos().Audit().Recent(ctx, 40)
			if err != nil {
				t.Fatal(err)
			}
			var sawReq, sawDL bool
			for _, e := range recent {
				meta, _ := json.Marshal(e.Metadata)
				ms := string(meta)
				if strings.Contains(ms, adaEmail) || strings.Contains(ms, bobEmail) || strings.Contains(ms, adaMeter) || strings.Contains(ms, rawToken) {
					t.Fatalf("audit PII in %s: %s", e.Action, ms)
				}
				if e.Action == "data_export.request" {
					sawReq = true
				}
				if e.Action == "data_export.download" {
					sawDL = true
				}
			}
			if !sawReq || !sawDL {
				t.Fatalf("missing export audit rows req=%v dl=%v", sawReq, sawDL)
			}

			code, camJob := c.do("POST", "/v1/me/data-export", "cam", map[string]any{}, nil)
			if code != 201 && code != 200 {
				t.Fatalf("cam export %d %v", code, camJob)
			}
			camTok, _ := camJob["download_token"].(string)
			if _, err := dataexport.Process(ctx, st, box, time.Now().UTC(), 20, log); err != nil {
				t.Fatal(err)
			}
			if _, err := st.Repos().Privacy().ExpireDueExports(ctx, time.Now().UTC().Add(25*time.Hour), 50); err != nil {
				t.Fatal(err)
			}
			code, gone := c.do("GET", "/v1/me/data-export/"+camJob["export_id"].(string)+"/download", "cam", nil, map[string]string{"X-Export-Token": camTok})
			if code != 410 {
				t.Fatalf("expired download want 410, got %d %v", code, gone)
			}
		})
	}
}

func TestDataExportRequiresStepUp(t *testing.T) {
	st := memstore.New()
	box, _ := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "v1")
	pay, vend, remote := mock.NewPayments(), mock.NewVending(), mock.NewRemoteCredit()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	orch := orchestrator.New(orchestrator.DefaultConfig(), st, registry.New(st, box, vend, remote), box, pay, vend, remote, log)
	eng := &identity.Engine{Users: st.Repos().Users(), Auth: st.Repos().Auth(), Box: box, Dev: true}
	sec := &security.Engine{Store: st.Repos().Auth(), Identity: eng, Box: box}
	s := &httpapi.Server{
		O: orch, Auth: auth.SessionThen(auth.DevAuth{}, eng), Identity: eng, Security: sec, Log: log,
		Limits: httpapi.Limits{WritesPerMinute: 6000},
	}
	h := s.Routes()
	const origin = "http://localhost:3000"
	key := newSoftKey(t, "export-key")
	begin := httptest.NewRequest("POST", "/v1/auth/passkeys/register/begin", strings.NewReader(`{"display_name":"Ada","origin":"`+origin+`"}`))
	begin.Header.Set("Content-Type", "application/json")
	begin.Header.Set("Origin", origin)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, begin)
	if rr.Code != 200 {
		t.Fatalf("begin %d %s", rr.Code, rr.Body.String())
	}
	var opt map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &opt)
	finBody, _ := json.Marshal(map[string]any{"credential": key.attestation(opt["challenge"].(string), origin)})
	fin := httptest.NewRequest("POST", "/v1/auth/passkeys/register/finish", strings.NewReader(string(finBody)))
	fin.Header.Set("Content-Type", "application/json")
	fin.Header.Set("Origin", origin)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, fin)
	if rr.Code != 200 {
		t.Fatalf("finish %d %s", rr.Code, rr.Body.String())
	}
	var reg map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &reg)
	tok := reg["token"].(string)

	req := httptest.NewRequest("POST", "/v1/me/data-export", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 403 || !strings.Contains(rr.Body.String(), "STEP_UP_REQUIRED") {
		t.Fatalf("want step-up, got %d %s", rr.Code, rr.Body.String())
	}

	// Dev bearer still works without a passkey session.
	dev := httptest.NewRequest("POST", "/v1/me/data-export", strings.NewReader("{}"))
	dev.Header.Set("Authorization", "Bearer ada")
	dev.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, dev)
	if rr.Code != 201 && rr.Code != 200 {
		t.Fatalf("dev bearer %d %s", rr.Code, rr.Body.String())
	}
}
