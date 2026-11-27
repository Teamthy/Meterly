package httpapi_test

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/auth"
	"github.com/teamthy/meterly/internal/httpapi"
	"github.com/teamthy/meterly/internal/ledger"
	"github.com/teamthy/meterly/internal/money"
	"github.com/teamthy/meterly/internal/orchestrator"
	"github.com/teamthy/meterly/internal/pots"
	"github.com/teamthy/meterly/internal/powerdesk"
	"github.com/teamthy/meterly/internal/providers/mock"
	"github.com/teamthy/meterly/internal/registry"
	"github.com/teamthy/meterly/internal/secure"
	"github.com/teamthy/meterly/internal/store"
	"github.com/teamthy/meterly/internal/store/memstore"
	"github.com/teamthy/meterly/internal/store/pgstore"
	"github.com/teamthy/meterly/internal/store/pgstore/pgtest"
	tx "github.com/teamthy/meterly/internal/transactions"
)

type syncLogBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncLogBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncLogBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type pdEnv struct {
	t      *testing.T
	st     store.Store
	pay    *mock.Payments
	vend   *mock.Vending
	remote *mock.RemoteCredit
	orch   *orchestrator.Orchestrator
	srv    *httpapi.Server
	h      http.Handler
	logs   *syncLogBuf

	mu  sync.Mutex
	now time.Time
}

func newPDEnv(t *testing.T, st store.Store, disabled bool) *pdEnv {
	t.Helper()
	box, err := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "v1")
	if err != nil {
		t.Fatal(err)
	}
	pay := mock.NewPayments()
	vend := mock.NewVending()
	remote := mock.NewRemoteCredit()
	logs := &syncLogBuf{}
	log := slog.New(slog.NewJSONHandler(logs, nil))

	e := &pdEnv{
		t:      t,
		st:     st,
		pay:    pay,
		vend:   vend,
		remote: remote,
		logs:   logs,
		now:    time.Now().UTC().Truncate(time.Second),
	}

	reg := registry.New(st, box, vend, remote)
	reg.Now = e.clock
	cfg := orchestrator.DefaultConfig()
	orch := orchestrator.New(cfg, st, reg, box, pay, vend, remote, log)
	orch.Now = e.clock
	e.orch = orch

	e.srv = &httpapi.Server{
		O:                 orch,
		Auth:              auth.DevAuth{},
		Payments:          pay,
		Log:               log,
		PowerDeskDisabled: disabled,
		Limits:            httpapi.Limits{WritesPerMinute: 6000},
		Dev:               true,
		DevPay:            pay,
		DevRemote:         remote,
	}
	e.h = e.srv.Routes()
	return e
}

func (e *pdEnv) clock() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.now
}

func (e *pdEnv) advanceClock(d time.Duration) {
	e.mu.Lock()
	e.now = e.now.Add(d)
	e.mu.Unlock()
}

func (e *pdEnv) do(method, path, bearer string, body any, headers map[string]string) *httptest.ResponseRecorder {
	e.t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			e.t.Fatal(err)
		}
		rdr = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, rdr)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	e.h.ServeHTTP(rr, req)
	return rr
}

func (e *pdEnv) ensureUser(subject string) string {
	e.t.Helper()
	uid, err := e.st.Repos().Users().EnsureByExternalID(context.Background(), subject)
	if err != nil {
		e.t.Fatal(err)
	}
	return uid
}

func forEachPDStore(t *testing.T, fn func(t *testing.T, st store.Store)) {
	t.Helper()
	t.Run("memstore", func(t *testing.T) {
		fn(t, memstore.New())
	})
	t.Run("pgstore", func(t *testing.T) {
		fn(t, pgstore.New(pgtest.NewPool(t), "SANDBOX"))
	})
}

func TestPowerDeskFeatureGateDefaultDisabled(t *testing.T) {
	env := newPDEnv(t, memstore.New(), true)
	id := uuid.New()
	checks := []struct {
		method string
		path   string
		bearer string
	}{
		{"POST", "/v1/orgs", "u1"},
		{"GET", "/v1/orgs", "u1"},
		{"GET", "/v1/orgs/" + id.String(), "u1"},
		{"PATCH", "/v1/orgs/" + id.String(), "u1"},
		{"POST", "/v1/orgs/" + id.String() + "/members", "u1"},
		{"PATCH", "/v1/orgs/" + id.String() + "/members/" + id.String(), "u1"},
		{"DELETE", "/v1/orgs/" + id.String() + "/members/" + id.String(), "u1"},
		{"POST", "/v1/orgs/" + id.String() + "/properties", "u1"},
		{"POST", "/v1/properties/" + id.String() + "/buildings", "u1"},
		{"GET", "/v1/properties/" + id.String() + "/buildings", "u1"},
		{"POST", "/v1/properties/" + id.String() + "/units", "u1"},
		{"POST", "/v1/units/" + id.String() + "/meters", "u1"},
		{"GET", "/v1/units/" + id.String() + "/meters/" + id.String() + "/history", "u1"},
		{"POST", "/v1/units/" + id.String() + "/meters/" + id.String() + "/delegate", "u1"},
		{"POST", "/v1/admin/unit-meters/" + id.String() + "/verify", "operator:op1"},
		{"POST", "/v1/orgs/" + id.String() + "/bulk-topups", "u1"},
		{"POST", "/v1/orgs/" + id.String() + "/bulk-topups/" + id.String() + "/approve", "u1"},
		{"GET", "/v1/orgs/" + id.String() + "/bulk-topups/" + id.String(), "u1"},
		{"GET", "/v1/orgs/" + id.String() + "/statements?month=2026-11", "u1"},
	}
	for i, tc := range checks {
		bearer := tc.bearer
		if bearer == "u1" {
			bearer = fmt.Sprintf("u-%d", i)
		}
		rr := env.do(tc.method, tc.path, bearer, map[string]any{}, nil)
		if rr.Code != http.StatusNotFound || !strings.Contains(rr.Body.String(), "FEATURE_DISABLED") {
			t.Fatalf("%s %s when disabled got %d %s, want 404 FEATURE_DISABLED", tc.method, tc.path, rr.Code, rr.Body.String())
		}
	}
}

func TestPowerDeskZeroIDORAndRBACMatrix(t *testing.T) {
	forEachPDStore(t, func(t *testing.T, st store.Store) {
		env := newPDEnv(t, st, false)

		ownerASub := "owner-a"
		mgrASub := "mgr-a"
		viewerASub := "viewer-a"
		tenantASub := "tenant-a"
		ownerBSub := "owner-b"

		_ = env.ensureUser(ownerASub)
		mgrAUID := env.ensureUser(mgrASub)
		viewerAUID := env.ensureUser(viewerASub)
		tenantAUID := env.ensureUser(tenantASub)
		ownerBUID := env.ensureUser(ownerBSub)

		// Create Org A (with approval threshold ₦10,000 = 1_000_000 kobo) and Org B.
		threshold := int64(1_000_000)
		rr := env.do("POST", "/v1/orgs", ownerASub, map[string]any{
			"name":                             "Org A Estate",
			"legal_name":                       "TEST CUSTOMER 1111",
			"organisation_type":                "ESTATE",
			"manager_approval_threshold_minor": threshold,
		}, nil)
		if rr.Code != http.StatusCreated {
			t.Fatalf("create orgA: %d %s", rr.Code, rr.Body.String())
		}
		var orgA orchestrator.OrgDetailView
		_ = json.Unmarshal(rr.Body.Bytes(), &orgA)

		rr = env.do("POST", "/v1/orgs", ownerBSub, map[string]any{
			"name":                             "Org B Secret Towers",
			"legal_name":                       "TEST CUSTOMER 2222",
			"organisation_type":                "LANDLORD",
			"manager_approval_threshold_minor": threshold,
		}, nil)
		if rr.Code != http.StatusCreated {
			t.Fatalf("create orgB: %d %s", rr.Code, rr.Body.String())
		}
		var orgB orchestrator.OrgDetailView
		_ = json.Unmarshal(rr.Body.Bytes(), &orgB)

		// Add MANAGER and VIEWER to Org A.
		if rr := env.do("POST", "/v1/orgs/"+orgA.ID.String()+"/members", ownerASub, map[string]any{
			"user_id": mgrAUID, "role": "MANAGER",
		}, nil); rr.Code != http.StatusCreated {
			t.Fatalf("add mgrA: %d %s", rr.Code, rr.Body.String())
		}
		if rr := env.do("POST", "/v1/orgs/"+orgA.ID.String()+"/members", ownerASub, map[string]any{
			"user_id": viewerAUID, "role": "VIEWER",
		}, nil); rr.Code != http.StatusCreated {
			t.Fatalf("add viewerA: %d %s", rr.Code, rr.Body.String())
		}

		// Setup Property, Unit, UnitMeter and BulkTopup on both Org A and Org B.
		rr = env.do("POST", "/v1/orgs/"+orgA.ID.String()+"/properties", mgrASub, map[string]any{
			"name": "Prop A", "city": "Lagos", "state": "LA",
		}, nil)
		if rr.Code != http.StatusCreated {
			t.Fatalf("create propA: %d %s", rr.Code, rr.Body.String())
		}
		var propA powerdesk.Property
		_ = json.Unmarshal(rr.Body.Bytes(), &propA)

		rr = env.do("POST", "/v1/properties/"+propA.ID.String()+"/units", mgrASub, map[string]any{
			"unit_name": "Flat A1",
		}, nil)
		if rr.Code != http.StatusCreated {
			t.Fatalf("create unitA: %d %s", rr.Code, rr.Body.String())
		}
		var unitA powerdesk.Unit
		_ = json.Unmarshal(rr.Body.Bytes(), &unitA)

		rr = env.do("POST", "/v1/units/"+unitA.ID.String()+"/meters", mgrASub, map[string]any{
			"meter_number": "45012341111", "disco_code": "IKEDC",
		}, nil)
		if rr.Code != http.StatusCreated {
			t.Fatalf("attach umA: %d %s", rr.Code, rr.Body.String())
		}
		var umA orchestrator.UnitMeterView
		_ = json.Unmarshal(rr.Body.Bytes(), &umA)

		rr = env.do("POST", "/v1/orgs/"+orgB.ID.String()+"/properties", ownerBSub, map[string]any{
			"name": "Prop B", "city": "Lagos",
		}, nil)
		if rr.Code != http.StatusCreated {
			t.Fatalf("create propB: %d %s", rr.Code, rr.Body.String())
		}
		var propB powerdesk.Property
		_ = json.Unmarshal(rr.Body.Bytes(), &propB)

		rr = env.do("POST", "/v1/properties/"+propB.ID.String()+"/units", ownerBSub, map[string]any{
			"unit_name": "Flat B1",
		}, nil)
		if rr.Code != http.StatusCreated {
			t.Fatalf("create unitB: %d %s", rr.Code, rr.Body.String())
		}
		var unitB powerdesk.Unit
		_ = json.Unmarshal(rr.Body.Bytes(), &unitB)

		rr = env.do("POST", "/v1/units/"+unitB.ID.String()+"/meters", ownerBSub, map[string]any{
			"meter_number": "45012342222", "disco_code": "IKEDC",
		}, nil)
		if rr.Code != http.StatusCreated {
			t.Fatalf("attach umB: %d %s", rr.Code, rr.Body.String())
		}
		var umB orchestrator.UnitMeterView
		_ = json.Unmarshal(rr.Body.Bytes(), &umB)

		rr = env.do("POST", "/v1/orgs/"+orgB.ID.String()+"/bulk-topups", ownerBSub, map[string]any{
			"email": "ownerb@example.com",
			"items": []map[string]any{{"unit_id": unitB.ID.String(), "amount_minor": 500_000}},
		}, map[string]string{"Idempotency-Key": "idem-orgb-bulk-topup-001"})
		if rr.Code != http.StatusCreated {
			t.Fatalf("create bulkB: %d %s", rr.Code, rr.Body.String())
		}
		var bulkB orchestrator.BulkTopupDetailView
		_ = json.Unmarshal(rr.Body.Bytes(), &bulkB)

		// ---- 1. Cross-Organisation IDOR Matrix: EVERY endpoint called with Org B's IDs MUST return 404 ----
		idorTargets := []struct {
			name    string
			method  string
			path    string
			body    any
			headers map[string]string
		}{
			{"getOrgB", "GET", "/v1/orgs/" + orgB.ID.String(), nil, nil},
			{"patchOrgB", "PATCH", "/v1/orgs/" + orgB.ID.String(), map[string]any{"name": "Pwned"}, nil},
			{"addMemberOrgB", "POST", "/v1/orgs/" + orgB.ID.String() + "/members", map[string]any{"user_id": mgrAUID, "role": "OWNER"}, nil},
			{"patchMemberOrgB", "PATCH", "/v1/orgs/" + orgB.ID.String() + "/members/" + ownerBUID, map[string]any{"role": "VIEWER"}, nil},
			{"deleteMemberOrgB", "DELETE", "/v1/orgs/" + orgB.ID.String() + "/members/" + ownerBUID, nil, nil},
			{"createPropOrgB", "POST", "/v1/orgs/" + orgB.ID.String() + "/properties", map[string]any{"name": "Sneaky"}, nil},
			{"createUnitPropB", "POST", "/v1/properties/" + propB.ID.String() + "/units", map[string]any{"unit_name": "Sneaky Unit"}, nil},
			{"attachMeterUnitB", "POST", "/v1/units/" + unitB.ID.String() + "/meters", map[string]any{"meter_number": "45012343333", "disco_code": "IKEDC"}, nil},
			{"historyUnitMeterB", "GET", "/v1/units/" + unitB.ID.String() + "/meters/" + umB.ID.String() + "/history", nil, nil},
			{"delegateUnitMeterB", "POST", "/v1/units/" + unitB.ID.String() + "/meters/" + umB.ID.String() + "/delegate", map[string]any{"user_id": tenantAUID, "permission": "VIEW"}, nil},
			{"createBulkOrgB", "POST", "/v1/orgs/" + orgB.ID.String() + "/bulk-topups", map[string]any{"items": []map[string]any{{"unit_id": unitB.ID.String(), "amount_minor": 500_000}}}, map[string]string{"Idempotency-Key": "idem-idor-bulk-orgb-0001"}},
			{"approveBulkOrgB", "POST", "/v1/orgs/" + orgB.ID.String() + "/bulk-topups/" + bulkB.PotID.String() + "/approve", map[string]any{}, nil},
			{"getBulkOrgB", "GET", "/v1/orgs/" + orgB.ID.String() + "/bulk-topups/" + bulkB.PotID.String(), nil, nil},
			{"statementsOrgB", "GET", "/v1/orgs/" + orgB.ID.String() + "/statements?month=2026-11", nil, nil},
			// Cross-resource IDOR inside Org A's URL path using Org B's child IDs:
			{"bulkOrgAWithUnitB", "POST", "/v1/orgs/" + orgA.ID.String() + "/bulk-topups", map[string]any{"items": []map[string]any{{"unit_id": unitB.ID.String(), "amount_minor": 500_000}}}, map[string]string{"Idempotency-Key": "idem-idor-mixed-unitb-01"}},
			{"getBulkBViaOrgA", "GET", "/v1/orgs/" + orgA.ID.String() + "/bulk-topups/" + bulkB.PotID.String(), nil, nil},
			{"approveBulkBViaOrgA", "POST", "/v1/orgs/" + orgA.ID.String() + "/bulk-topups/" + bulkB.PotID.String() + "/approve", map[string]any{}, nil},
			{"statementsOrgAWithPropB", "GET", "/v1/orgs/" + orgA.ID.String() + "/statements?property_id=" + propB.ID.String() + "&month=2026-11", nil, nil},
			{"historyUnitAWithUmB", "GET", "/v1/units/" + unitA.ID.String() + "/meters/" + umB.ID.String() + "/history", nil, nil},
		}

		// Use separate cross-org attacker tokens per chunk so per-user write burst (10) never triggers 429.
		for idx, tc := range idorTargets {
			caller := ownerASub
			if tc.method != "GET" {
				// Add a fresh Org-A OWNER helper every 6 writes to stay under per-user burst=10.
				sub := fmt.Sprintf("idor-caller-%d", idx/6)
				uid := env.ensureUser(sub)
				_ = env.st.Repos().PowerDesk().UpsertMember(context.Background(), &powerdesk.Member{
					OrganisationID: orgA.ID, UserID: uid, Role: powerdesk.RoleOwner,
				})
				caller = sub
			}
			res := env.do(tc.method, tc.path, caller, tc.body, tc.headers)
			if res.Code != http.StatusNotFound {
				t.Fatalf("IDOR check %s (%s %s) returned %d %s, want 404", tc.name, tc.method, tc.path, res.Code, res.Body.String())
			}
			if strings.Contains(res.Body.String(), "Org B Secret Towers") {
				t.Fatalf("IDOR check %s leaked Org B name", tc.name)
			}
		}

		// ---- 2. Intra-Organisation RBAC Matrix & Approval Rule ----
		// VIEWER can read org & statements, but gets 403 FORBIDDEN on writes.
		if rr := env.do("GET", "/v1/orgs/"+orgA.ID.String(), viewerASub, nil, nil); rr.Code != http.StatusOK {
			t.Fatalf("viewer getOrg: %d %s", rr.Code, rr.Body.String())
		}
		if rr := env.do("GET", "/v1/orgs/"+orgA.ID.String()+"/statements?month=2026-11", viewerASub, nil, nil); rr.Code != http.StatusOK {
			t.Fatalf("viewer statements: %d %s", rr.Code, rr.Body.String())
		}
		viewerForbidden := []struct {
			method string
			path   string
			body   any
			hdrs   map[string]string
		}{
			{"PATCH", "/v1/orgs/" + orgA.ID.String(), map[string]any{"name": "No"}, nil},
			{"POST", "/v1/orgs/" + orgA.ID.String() + "/members", map[string]any{"user_id": tenantAUID, "role": "VIEWER"}, nil},
			{"POST", "/v1/orgs/" + orgA.ID.String() + "/properties", map[string]any{"name": "No"}, nil},
			{"POST", "/v1/properties/" + propA.ID.String() + "/units", map[string]any{"unit_name": "No"}, nil},
			{"POST", "/v1/units/" + unitA.ID.String() + "/meters", map[string]any{"meter_number": "45012344444", "disco_code": "IKEDC"}, nil},
			{"POST", "/v1/orgs/" + orgA.ID.String() + "/bulk-topups", map[string]any{"items": []map[string]any{{"unit_id": unitA.ID.String(), "amount_minor": 500_000}}}, map[string]string{"Idempotency-Key": "idem-viewer-forbidden-01"}},
		}
		for _, tc := range viewerForbidden {
			res := env.do(tc.method, tc.path, viewerASub, tc.body, tc.hdrs)
			if res.Code != http.StatusForbidden {
				t.Fatalf("VIEWER %s %s got %d %s, want 403 FORBIDDEN", tc.method, tc.path, res.Code, res.Body.String())
			}
		}

		// MANAGER creating bulk top-up over threshold (₦15,000 + ₦100 = ₦15,100 > ₦10,000) enters PENDING_APPROVAL without charging.
		rr = env.do("POST", "/v1/orgs/"+orgA.ID.String()+"/bulk-topups", mgrASub, map[string]any{
			"email": "mgra@example.com",
			"items": []map[string]any{{"unit_id": unitA.ID.String(), "amount_minor": 1_500_000}},
		}, map[string]string{"Idempotency-Key": "idem-mgr-over-threshold-1"})
		if rr.Code != http.StatusCreated {
			t.Fatalf("mgr over-threshold bulk: %d %s", rr.Code, rr.Body.String())
		}
		var pendingBulk orchestrator.BulkTopupDetailView
		_ = json.Unmarshal(rr.Body.Bytes(), &pendingBulk)
		if pendingBulk.PotStatus != pots.StatusPendingApproval || pendingBulk.ApprovalStatus != powerdesk.ApprovalPending {
			t.Fatalf("expected PENDING_APPROVAL, got pot=%s approval=%s", pendingBulk.PotStatus, pendingBulk.ApprovalStatus)
		}
		if pendingBulk.CheckoutURL != "" || pendingBulk.PaymentReference != "" {
			t.Fatalf("over-threshold MANAGER bulk top-up must NOT initialize checkout before OWNER approval: %+v", pendingBulk)
		}

		// MANAGER cannot self-approve, manage members, or patch org settings -> 403 FORBIDDEN.
		if rr := env.do("POST", "/v1/orgs/"+orgA.ID.String()+"/bulk-topups/"+pendingBulk.PotID.String()+"/approve", mgrASub, map[string]any{}, nil); rr.Code != http.StatusForbidden {
			t.Fatalf("MANAGER self-approve must return 403, got %d %s", rr.Code, rr.Body.String())
		}
		if rr := env.do("POST", "/v1/orgs/"+orgA.ID.String()+"/members", mgrASub, map[string]any{"user_id": tenantAUID, "role": "VIEWER"}, nil); rr.Code != http.StatusForbidden {
			t.Fatalf("MANAGER add member must return 403, got %d %s", rr.Code, rr.Body.String())
		}
		if rr := env.do("PATCH", "/v1/orgs/"+orgA.ID.String(), mgrASub, map[string]any{"name": "No"}, nil); rr.Code != http.StatusForbidden {
			t.Fatalf("MANAGER patch org must return 403, got %d %s", rr.Code, rr.Body.String())
		}

		// OWNER approves -> transitions PENDING_APPROVAL -> OPEN and returns checkout URL.
		rr = env.do("POST", "/v1/orgs/"+orgA.ID.String()+"/bulk-topups/"+pendingBulk.PotID.String()+"/approve", ownerASub, map[string]any{
			"email": "ownera@example.com",
		}, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("OWNER approve bulk: %d %s", rr.Code, rr.Body.String())
		}
		var approvedBulk orchestrator.BulkTopupDetailView
		_ = json.Unmarshal(rr.Body.Bytes(), &approvedBulk)
		if approvedBulk.PotStatus != pots.StatusOpen || approvedBulk.ApprovalStatus != powerdesk.ApprovalApproved || approvedBulk.CheckoutURL == "" {
			t.Fatalf("unexpected approved bulk state: %+v", approvedBulk)
		}

		// Last-owner protection: sole OWNER cannot remove or demote themselves (409 LAST_OWNER).
		ownerAUID := env.ensureUser(ownerASub)
		if rr := env.do("PATCH", "/v1/orgs/"+orgB.ID.String()+"/members/"+ownerBUID, ownerBSub, map[string]any{"role": "MANAGER"}, nil); rr.Code != http.StatusConflict {
			t.Fatalf("demoting last OWNER must return 409, got %d %s", rr.Code, rr.Body.String())
		}
		if rr := env.do("DELETE", "/v1/orgs/"+orgB.ID.String()+"/members/"+ownerBUID, ownerBSub, nil, nil); rr.Code != http.StatusConflict {
			t.Fatalf("removing last OWNER must return 409, got %d %s", rr.Code, rr.Body.String())
		}
		_ = ownerAUID

		// ---- 3. Unit-Meter Verification Gate (unverified can purchase, cannot view history or delegate) ----
		if rr := env.do("GET", "/v1/units/"+unitA.ID.String()+"/meters/"+umA.ID.String()+"/history", viewerASub, nil, nil); rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "UNIT_METER_UNVERIFIED") {
			t.Fatalf("unverified unit_meter history must return 403 UNIT_METER_UNVERIFIED, got %d %s", rr.Code, rr.Body.String())
		}
		if rr := env.do("POST", "/v1/units/"+unitA.ID.String()+"/meters/"+umA.ID.String()+"/delegate", mgrASub, map[string]any{
			"user_id": tenantAUID, "permission": "PURCHASE",
		}, nil); rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "UNIT_METER_UNVERIFIED") {
			t.Fatalf("unverified unit_meter delegate must return 403 UNIT_METER_UNVERIFIED, got %d %s", rr.Code, rr.Body.String())
		}

		// Operator verification requires staff role + mandatory reason + valid method/evidence.
		if rr := env.do("POST", "/v1/admin/unit-meters/"+umA.ID.String()+"/verify", ownerASub, map[string]any{
			"method": "ACCOUNT_NAME_MATCH", "reason": "Customer trying to self-verify",
		}, nil); rr.Code != http.StatusForbidden {
			t.Fatalf("customer calling admin verify must get 403, got %d", rr.Code)
		}
		if rr := env.do("POST", "/v1/admin/unit-meters/"+umA.ID.String()+"/verify", "operator:op1", map[string]any{
			"method": "ACCOUNT_NAME_MATCH", "reason": "bad",
		}, nil); rr.Code != http.StatusBadRequest {
			t.Fatalf("short reason must get 400, got %d", rr.Code)
		}
		if rr := env.do("POST", "/v1/admin/unit-meters/"+umA.ID.String()+"/verify", "operator:op1", map[string]any{
			"method": "EVIDENCE_APPROVED", "reason": "Missing evidence ref check",
		}, nil); rr.Code != http.StatusUnprocessableEntity {
			t.Fatalf("EVIDENCE_APPROVED without evidence_reference must get 422, got %d %s", rr.Code, rr.Body.String())
		}
		rr = env.do("POST", "/v1/admin/unit-meters/"+umA.ID.String()+"/verify", "operator:op1", map[string]any{
			"method": "ACCOUNT_NAME_MATCH", "reason": "Verified IKEDC customer name matches org legal_name",
		}, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("operator ACCOUNT_NAME_MATCH verify failed: %d %s", rr.Code, rr.Body.String())
		}
		audits, err := env.st.Repos().Audit().ForResource(context.Background(), "unit_meter", umA.ID)
		if err != nil || len(audits) < 2 {
			t.Fatalf("expected unit_meter audit trail, got %+v (%v)", audits, err)
		}

		// Once verified, history and tenant delegation both succeed!
		if rr := env.do("GET", "/v1/units/"+unitA.ID.String()+"/meters/"+umA.ID.String()+"/history", viewerASub, nil, nil); rr.Code != http.StatusOK {
			t.Fatalf("verified unit_meter history failed: %d %s", rr.Code, rr.Body.String())
		}
		if rr := env.do("POST", "/v1/units/"+unitA.ID.String()+"/meters/"+umA.ID.String()+"/delegate", mgrASub, map[string]any{
			"user_id": tenantAUID, "permission": "PURCHASE", "label": "Flat A1 Tenant",
		}, nil); rr.Code != http.StatusOK {
			t.Fatalf("verified unit_meter delegate failed: %d %s", rr.Code, rr.Body.String())
		}
		if rr := env.do("GET", "/v1/meters/"+umA.MeterID.String(), tenantASub, nil, nil); rr.Code != http.StatusOK {
			t.Fatalf("delegated tenant should see meter: %d %s", rr.Code, rr.Body.String())
		}

		// Removed member loses access immediately (404) AND cannot bypass via /v1/pots/{id}.
		if rr := env.do("DELETE", "/v1/orgs/"+orgA.ID.String()+"/members/"+mgrAUID, ownerASub, nil, nil); rr.Code != http.StatusNoContent {
			t.Fatalf("remove mgrA: %d %s", rr.Code, rr.Body.String())
		}
		if rr := env.do("GET", "/v1/orgs/"+orgA.ID.String()+"/bulk-topups/"+pendingBulk.PotID.String(), mgrASub, nil, nil); rr.Code != http.StatusNotFound {
			t.Fatalf("removed mgrA reading bulk topup must get 404, got %d", rr.Code)
		}
		if rr := env.do("GET", "/v1/pots/"+pendingBulk.PotID.String(), mgrASub, nil, nil); rr.Code != http.StatusNotFound {
			t.Fatalf("removed mgrA reading LANDLORD_BULK pot via /v1/pots/{id} must get 404, got %d", rr.Code)
		}
	})
}

func TestPowerDeskBulkTopupFiveUnitsPartialFailureUnknownAndLedgerStatements(t *testing.T) {
	forEachPDStore(t, func(t *testing.T, st store.Store) {
		env := newPDEnv(t, st, false)
		ctx := context.Background()

		ownerSub := "landlord-owner"
		viewerSub := "landlord-accountant"
		_ = env.ensureUser(ownerSub)
		viewerUID := env.ensureUser(viewerSub)

		// Create organisation and property.
		rr := env.do("POST", "/v1/orgs", ownerSub, map[string]any{
			"name":              "Victoria Island Suites",
			"legal_name":        "Victoria Island Suites Ltd",
			"organisation_type": "LANDLORD",
		}, nil)
		if rr.Code != http.StatusCreated {
			t.Fatalf("create org: %d %s", rr.Code, rr.Body.String())
		}
		var org orchestrator.OrgDetailView
		_ = json.Unmarshal(rr.Body.Bytes(), &org)

		if rr := env.do("POST", "/v1/orgs/"+org.ID.String()+"/members", ownerSub, map[string]any{
			"user_id": viewerUID, "role": "VIEWER",
		}, nil); rr.Code != http.StatusCreated {
			t.Fatalf("add accountant viewer: %d %s", rr.Code, rr.Body.String())
		}

		rr = env.do("POST", "/v1/orgs/"+org.ID.String()+"/properties", ownerSub, map[string]any{
			"name": "=VI Court", // leading '=' exercises CSV injection protection
			"city": "Lagos", "state": "LA",
		}, nil)
		if rr.Code != http.StatusCreated {
			t.Fatalf("create property: %d %s", rr.Code, rr.Body.String())
		}
		var prop powerdesk.Property
		_ = json.Unmarshal(rr.Body.Bytes(), &prop)

		// Attach 5 meters across 5 units (note: unverified meters CAN be topped up in bulk!):
		//   Units 1, 2, 5 -> succeed immediately
		//   Unit 3 (ends with 0000) -> definitive VendFailed -> refunded immediately
		//   Unit 4 (ends with 9999) -> provider timeout -> VendUnknown -> NEVER refunded while UNKNOWN
		meterSpecs := []struct {
			unitName string
			number   string
		}{
			{"Flat 1", "45012341111"},
			{"Flat 2", "45012342222"},
			{"Flat 3", "45012340000"}, // VendFailed
			{"Flat 4", "45012349999"}, // VendUnknown on first call, VendSucceeded on GetVendStatus
			{"+Flat 5", "45012345555"},
		}
		unitIDs := make([]uuid.UUID, 0, len(meterSpecs))
		for idx, ms := range meterSpecs {
			sub := ownerSub
			if idx >= 2 {
				// Use a second OWNER actor so we stay well below the per-user write burst of 10.
				sub = "landlord-coowner"
				if idx == 2 {
					coUID := env.ensureUser(sub)
					if rr := env.do("POST", "/v1/orgs/"+org.ID.String()+"/members", ownerSub, map[string]any{
						"user_id": coUID, "role": "OWNER",
					}, nil); rr.Code != http.StatusCreated {
						t.Fatalf("add coowner: %d %s", rr.Code, rr.Body.String())
					}
				}
			}
			uRR := env.do("POST", "/v1/properties/"+prop.ID.String()+"/units", sub, map[string]any{
				"unit_name": ms.unitName,
			}, nil)
			if uRR.Code != http.StatusCreated {
				t.Fatalf("create unit %s: %d %s", ms.unitName, uRR.Code, uRR.Body.String())
			}
			var u powerdesk.Unit
			_ = json.Unmarshal(uRR.Body.Bytes(), &u)
			unitIDs = append(unitIDs, u.ID)

			mRR := env.do("POST", "/v1/units/"+u.ID.String()+"/meters", sub, map[string]any{
				"meter_number":       ms.number,
				"disco_code":         "EKEDC",
				"evidence_reference": fmt.Sprintf("LEASE-VI-%d", idx+1),
			}, nil)
			if mRR.Code != http.StatusCreated {
				t.Fatalf("attach meter %s: %d %s", ms.number, mRR.Code, mRR.Body.String())
			}
		}

		// Create bulk top-up of 5 units x ₦5,000 (500,000 kobo electricity + 10,000 kobo fee = 510,000 each; total = 2,550,000).
		bulkItems := make([]map[string]any, 0, len(unitIDs))
		for _, uid := range unitIDs {
			bulkItems = append(bulkItems, map[string]any{
				"unit_id":      uid.String(),
				"amount_minor": 500_000,
			})
		}
		idemHdr := map[string]string{"Idempotency-Key": "bulk-5-units-partial-test-01"}
		rr = env.do("POST", "/v1/orgs/"+org.ID.String()+"/bulk-topups", ownerSub, map[string]any{
			"email": "landlord-billing@example.com",
			"items": bulkItems,
		}, idemHdr)
		if rr.Code != http.StatusCreated {
			t.Fatalf("create 5-unit bulk topup: %d %s", rr.Code, rr.Body.String())
		}
		var created orchestrator.BulkTopupDetailView
		_ = json.Unmarshal(rr.Body.Bytes(), &created)
		if created.ElectricityTotalMinor != 2_500_000 || created.FeeTotalMinor != 50_000 || created.TotalMinor != 2_550_000 {
			t.Fatalf("unexpected bulk totals: %+v", created)
		}

		// Idempotent replay with same Idempotency-Key returns 200 and same pot_id without double checkout.
		replayRR := env.do("POST", "/v1/orgs/"+org.ID.String()+"/bulk-topups", ownerSub, map[string]any{
			"email": "landlord-billing@example.com",
			"items": bulkItems,
		}, idemHdr)
		if replayRR.Code != http.StatusOK {
			t.Fatalf("idempotent replay expected 200, got %d %s", replayRR.Code, replayRR.Body.String())
		}

		// Complete the landlord's single payment (₦25,500 = 2,550,000 kobo) and deliver signed webhook.
		completeRR := env.do("POST", "/v1/dev/payments/"+created.PaymentReference+"/complete", "", map[string]any{
			"success": true,
		}, nil)
		if completeRR.Code != http.StatusOK {
			t.Fatalf("complete bulk payment: %d %s", completeRR.Code, completeRR.Body.String())
		}
		hookRR := env.do("POST", "/v1/payments/webhooks/mockpay", "", map[string]any{
			"reference": created.PaymentReference,
		}, map[string]string{"X-Mock-Signature": "dev-signature"})
		if hookRR.Code != http.StatusOK {
			t.Fatalf("webhook bulk payment: %d %s", hookRR.Code, hookRR.Body.String())
		}

		// Inspect status immediately after initial vend pass:
		//   3 delivered (Units 1, 2, 5)
		//   1 failed and refunded (Unit 3 -> 510,000 refunded to landlord!)
		//   1 pending (Unit 4 -> VendUnknown -> MUST NOT be refunded!)
		statusRR := env.do("GET", "/v1/orgs/"+org.ID.String()+"/bulk-topups/"+created.PotID.String(), viewerSub, nil, nil)
		if statusRR.Code != http.StatusOK {
			t.Fatalf("getBulkTopup: %d %s", statusRR.Code, statusRR.Body.String())
		}
		var mid orchestrator.BulkTopupDetailView
		_ = json.Unmarshal(statusRR.Body.Bytes(), &mid)

		if mid.PotStatus != pots.StatusAllocating {
			t.Fatalf("pot must stay ALLOCATING while 1 unit is UNKNOWN, got %s", mid.PotStatus)
		}
		if mid.DeliveredCount != 3 || mid.FailedRefundedCount != 1 || mid.PendingCount != 1 {
			t.Fatalf("expected (3 delivered, 1 failed, 1 pending), got (%d, %d, %d): %+v",
				mid.DeliveredCount, mid.FailedRefundedCount, mid.PendingCount, mid)
		}
		if mid.Summary != "3 delivered, 1 failed and refunded, 1 pending" {
			t.Fatalf("unexpected honest partial-success summary: %q", mid.Summary)
		}
		if mid.RefundedMinor != 510_000 {
			t.Fatalf("expected exact partial refund of 510_000 (1 failed unit, 0 for UNKNOWN unit), got %d", mid.RefundedMinor)
		}
		if got := len(env.pay.Refunds); got != 1 || env.pay.Refunds[0].Amount.Minor != 510_000 {
			t.Fatalf("PSP refunds while Unit 4 is UNKNOWN must be exactly [510_000], got %+v", env.pay.Refunds)
		}

		// Verify double-entry ledger invariants while Unit 4 is UNKNOWN:
		//   TrialBalance == 0
		//   POT_FUNDS_HELD == 0
		//   REFUND_PAYABLE == 0
		//   CUSTOMER_FUNDS_CLEARING == 510_000 (Unit 4's funds held safely!)
		//   PROVIDER_PAYABLE == -1_500_000 (credit balance = 3 * 500_000; Balance() returns Debits - Credits)
		tb, err := st.Repos().Ledger().TrialBalance(ctx)
		if err != nil || tb != 0 {
			t.Fatalf("ledger trial balance must be 0, got %d (%v)", tb, err)
		}
		potHeld, _ := st.Repos().Ledger().Balance(ctx, ledger.PotFundsHeld, money.NGN)
		refundPayable, _ := st.Repos().Ledger().Balance(ctx, ledger.RefundPayable, money.NGN)
		clearing, _ := st.Repos().Ledger().Balance(ctx, ledger.CustomerFundsClearing, money.NGN)
		if potHeld != 0 || refundPayable != 0 || clearing != -510_000 {
			t.Fatalf("unexpected holding balances while UNKNOWN: pot_held=%d refund_payable=%d clearing=%d", potHeld, refundPayable, clearing)
		}

		// Check statement while Unit 4 is UNKNOWN: only the 3 vended units count as purchased, and only Unit 3 counts as refunded!
		stmtMidRR := env.do("GET", "/v1/orgs/"+org.ID.String()+"/statements?month=2026-11", viewerSub, nil, nil)
		if stmtMidRR.Code != http.StatusOK {
			t.Fatalf("mid statement: %d %s", stmtMidRR.Code, stmtMidRR.Body.String())
		}
		var stmtMid powerdesk.MonthlyStatement
		_ = json.Unmarshal(stmtMidRR.Body.Bytes(), &stmtMid)
		if stmtMid.ElectricityPurchasedMinor != 1_500_000 || stmtMid.FeesMinor != 30_000 || stmtMid.RefundsMinor != 510_000 || stmtMid.NetChargedMinor != 1_530_000 {
			t.Fatalf("mid statement mismatch vs ledger: %+v", stmtMid)
		}

		// Now advance clock past ResumeStaleAfter and reconcile: Unit 4's GetVendStatus resolves to VendSucceeded + DelConfirmed,
		// and the LANDLORD_BULK pot settles cleanly (ALLOCATING -> SETTLED)!
		env.advanceClock(2 * time.Minute)
		var unknownTxnID uuid.UUID
		for _, it := range mid.Items {
			if it.OverallStatus == tx.OverallReconciliation && it.TransactionID != nil {
				unknownTxnID = *it.TransactionID
			}
		}
		if unknownTxnID == uuid.Nil {
			t.Fatal("expected Unit 4 transaction in RECONCILIATION_PENDING")
		}
		if _, err := env.orch.Resume(ctx, unknownTxnID); err != nil {
			t.Fatalf("resume unknown unit txn: %v", err)
		}

		finalRR := env.do("GET", "/v1/orgs/"+org.ID.String()+"/bulk-topups/"+created.PotID.String(), viewerSub, nil, nil)
		if finalRR.Code != http.StatusOK {
			t.Fatalf("final getBulkTopup: %d %s", finalRR.Code, finalRR.Body.String())
		}
		var finalView orchestrator.BulkTopupDetailView
		_ = json.Unmarshal(finalRR.Body.Bytes(), &finalView)
		if finalView.PotStatus != pots.StatusSettled {
			t.Fatalf("expected SETTLED after Unit 4 reconciled, got %s", finalView.PotStatus)
		}
		if finalView.DeliveredCount != 4 || finalView.FailedRefundedCount != 1 || finalView.PendingCount != 0 {
			t.Fatalf("expected (4 delivered, 1 failed, 0 pending), got (%d, %d, %d)",
				finalView.DeliveredCount, finalView.FailedRefundedCount, finalView.PendingCount)
		}
		if finalView.Summary != "4 delivered, 1 failed and refunded, 0 pending" {
			t.Fatalf("unexpected final summary: %q", finalView.Summary)
		}

		// Verify all holding accounts are strictly 0 and statement totals match the ledger to the exact kobo!
		tb, _ = st.Repos().Ledger().TrialBalance(ctx)
		potHeld, _ = st.Repos().Ledger().Balance(ctx, ledger.PotFundsHeld, money.NGN)
		refundPayable, _ = st.Repos().Ledger().Balance(ctx, ledger.RefundPayable, money.NGN)
		clearing, _ = st.Repos().Ledger().Balance(ctx, ledger.CustomerFundsClearing, money.NGN)
		provPayable, _ := st.Repos().Ledger().Balance(ctx, ledger.ProviderPayable, money.NGN)
		feeRevenue, _ := st.Repos().Ledger().Balance(ctx, ledger.PlatformFeeRevenue, money.NGN)
		pspReceivable, _ := st.Repos().Ledger().Balance(ctx, ledger.PSPSettlementReceivable, money.NGN)
		if tb != 0 || potHeld != 0 || refundPayable != 0 || clearing != 0 {
			t.Fatalf("settled bulk pot left non-zero holding/trial balance: tb=%d pot=%d ref=%d clr=%d", tb, potHeld, refundPayable, clearing)
		}

		stmtRR := env.do("GET", "/v1/orgs/"+org.ID.String()+"/statements?property_id="+prop.ID.String()+"&month=2026-11", viewerSub, nil, nil)
		if stmtRR.Code != http.StatusOK {
			t.Fatalf("final JSON statement: %d %s", stmtRR.Code, stmtRR.Body.String())
		}
		var stmt powerdesk.MonthlyStatement
		_ = json.Unmarshal(stmtRR.Body.Bytes(), &stmt)

		// Ledger credits are negative in Balance() (Debits - Credits), so -provPayable == 2_000_000 and -feeRevenue == 40_000.
		if stmt.ElectricityPurchasedMinor != -provPayable || stmt.FeesMinor != -feeRevenue {
			t.Fatalf("statement principal/fees (%d / %d) != ledger (%d / %d)",
				stmt.ElectricityPurchasedMinor, stmt.FeesMinor, -provPayable, -feeRevenue)
		}
		if stmt.RefundsMinor != 510_000 || stmt.NetChargedMinor != pspReceivable {
			t.Fatalf("statement refunds/net (%d / %d) != expected (510000 / %d)",
				stmt.RefundsMinor, stmt.NetChargedMinor, pspReceivable)
		}
		if stmt.Description != "electricity purchased" {
			t.Fatalf("statement description must be 'electricity purchased', got %q", stmt.Description)
		}

		// Verify CSV export headers, rows, "electricity purchased" wording, and formula-injection sanitization.
		csvRR := env.do("GET", "/v1/orgs/"+org.ID.String()+"/statements?property_id="+prop.ID.String()+"&month=2026-11&format=csv", viewerSub, nil, nil)
		if csvRR.Code != http.StatusOK || !strings.HasPrefix(csvRR.Header().Get("Content-Type"), "text/csv") {
			t.Fatalf("CSV statement failed: %d ct=%q body=%s", csvRR.Code, csvRR.Header().Get("Content-Type"), csvRR.Body.String())
		}
		csvBody := csvRR.Body.String()
		if !strings.Contains(csvBody, "electricity_purchased_minor") || !strings.Contains(csvBody, "electricity purchased") {
			t.Fatalf("CSV missing 'electricity purchased' label: %s", csvBody)
		}
		if strings.Contains(strings.ToLower(csvBody), "electricity used") || strings.Contains(strings.ToLower(csvBody), "electricity_used") {
			t.Fatalf("CSV must never label vending as 'used': %s", csvBody)
		}
		recs, err := csv.NewReader(strings.NewReader(csvBody)).ReadAll()
		if err != nil || len(recs) != 6 { // header + 5 units
			t.Fatalf("expected 6 CSV rows, got %d (%v)", len(recs), err)
		}
		if recs[1][2] != "'=VI Court" {
			t.Fatalf("expected formula-injection-sanitized property cell, got %q", recs[1][2])
		}
		foundSanitizedUnit := false
		for _, row := range recs[1:] {
			if row[4] == "'+Flat 5" {
				foundSanitizedUnit = true
			}
		}
		if !foundSanitizedUnit {
			t.Fatalf("expected formula-injection-sanitized unit cell '+Flat 5 in CSV rows: %+v", recs[1:])
		}

		// Verify logs never leaked raw meter numbers, billing emails, or STS tokens.
		logsOut := env.logs.String()
		for _, secret := range []string{"45012341111", "45012340000", "45012349999", "landlord-billing@example.com"} {
			if strings.Contains(logsOut, secret) {
				t.Fatalf("structured logs leaked sensitive value %q: %s", secret, logsOut)
			}
		}
	})
}

func TestPowerDeskMonthBoundaryAfricaLagosAndAllUnitsFailed(t *testing.T) {
	forEachPDStore(t, func(t *testing.T, st store.Store) {
		env := newPDEnv(t, st, false)
		ctx := context.Background()

		ownerSub := "boundary-owner"
		_ = env.ensureUser(ownerSub)

		rr := env.do("POST", "/v1/orgs", ownerSub, map[string]any{
			"name": "Lagos Boundary Estates", "organisation_type": "LANDLORD",
		}, nil)
		if rr.Code != http.StatusCreated {
			t.Fatalf("create org: %d %s", rr.Code, rr.Body.String())
		}
		var org orchestrator.OrgDetailView
		_ = json.Unmarshal(rr.Body.Bytes(), &org)

		rr = env.do("POST", "/v1/orgs/"+org.ID.String()+"/properties", ownerSub, map[string]any{
			"name": "Ikoyi House", "city": "Lagos",
		}, nil)
		if rr.Code != http.StatusCreated {
			t.Fatalf("create prop: %d %s", rr.Code, rr.Body.String())
		}
		var prop powerdesk.Property
		_ = json.Unmarshal(rr.Body.Bytes(), &prop)

		rr = env.do("POST", "/v1/properties/"+prop.ID.String()+"/units", ownerSub, map[string]any{
			"unit_name": "Penthouse",
		}, nil)
		var u1 powerdesk.Unit
		_ = json.Unmarshal(rr.Body.Bytes(), &u1)
		_ = env.do("POST", "/v1/units/"+u1.ID.String()+"/meters", ownerSub, map[string]any{
			"meter_number": "45012347777", "disco_code": "EKEDC",
		}, nil)

		rr = env.do("POST", "/v1/properties/"+prop.ID.String()+"/units", ownerSub, map[string]any{
			"unit_name": "Blocked Unit",
		}, nil)
		var uFail powerdesk.Unit
		_ = json.Unmarshal(rr.Body.Bytes(), &uFail)
		_ = env.do("POST", "/v1/units/"+uFail.ID.String()+"/meters", ownerSub, map[string]any{
			"meter_number": "45012340000", "disco_code": "EKEDC",
		}, nil)

		// 1) Post a purchase at 2026-11-30T23:30:00Z = 2026-12-01T00:30:00+01:00 (Africa/Lagos):
		//    MUST belong to month=2026-12 and NOT month=2026-11!
		env.mu.Lock()
		env.now = time.Date(2026, 11, 30, 23, 30, 0, 0, time.UTC)
		env.mu.Unlock()

		rr = env.do("POST", "/v1/orgs/"+org.ID.String()+"/bulk-topups", ownerSub, map[string]any{
			"email": "boundary@example.com",
			"items": []map[string]any{{"unit_id": u1.ID.String(), "amount_minor": 300_000}},
		}, map[string]string{"Idempotency-Key": "idem-lagos-boundary-dec-01"})
		if rr.Code != http.StatusCreated {
			t.Fatalf("create boundary bulk: %d %s", rr.Code, rr.Body.String())
		}
		var bDec orchestrator.BulkTopupDetailView
		_ = json.Unmarshal(rr.Body.Bytes(), &bDec)
		_ = env.do("POST", "/v1/dev/payments/"+bDec.PaymentReference+"/complete", "", map[string]any{"success": true}, nil)
		_ = env.do("POST", "/v1/payments/webhooks/mockpay", "", map[string]any{"reference": bDec.PaymentReference}, map[string]string{"X-Mock-Signature": "dev-signature"})

		novRR := env.do("GET", "/v1/orgs/"+org.ID.String()+"/statements?month=2026-11", ownerSub, nil, nil)
		var novStmt powerdesk.MonthlyStatement
		_ = json.Unmarshal(novRR.Body.Bytes(), &novStmt)
		if novStmt.ElectricityPurchasedMinor != 0 || novStmt.FeesMinor != 0 {
			t.Fatalf("2026-11-30T23:30Z (= Dec 1 00:30 WAT) leaked into November statement: %+v", novStmt)
		}

		decRR := env.do("GET", "/v1/orgs/"+org.ID.String()+"/statements?month=2026-12", ownerSub, nil, nil)
		var decStmt powerdesk.MonthlyStatement
		_ = json.Unmarshal(decRR.Body.Bytes(), &decStmt)
		if decStmt.ElectricityPurchasedMinor != 300_000 || decStmt.FeesMinor != 10_000 || decStmt.NetChargedMinor != 310_000 {
			t.Fatalf("expected 300k/10k/310k in December WAT statement, got %+v", decStmt)
		}

		// 2) All-units-failed bulk top-up transitions ALLOCATING -> REFUNDING -> REFUNDED with 100% refund.
		rr = env.do("POST", "/v1/orgs/"+org.ID.String()+"/bulk-topups", ownerSub, map[string]any{
			"email": "boundary@example.com",
			"items": []map[string]any{{"unit_id": uFail.ID.String(), "amount_minor": 400_000}},
		}, map[string]string{"Idempotency-Key": "idem-all-units-failed-001"})
		if rr.Code != http.StatusCreated {
			t.Fatalf("create all-fail bulk: %d %s", rr.Code, rr.Body.String())
		}
		var bFail orchestrator.BulkTopupDetailView
		_ = json.Unmarshal(rr.Body.Bytes(), &bFail)
		_ = env.do("POST", "/v1/dev/payments/"+bFail.PaymentReference+"/complete", "", map[string]any{"success": true}, nil)
		_ = env.do("POST", "/v1/payments/webhooks/mockpay", "", map[string]any{"reference": bFail.PaymentReference}, map[string]string{"X-Mock-Signature": "dev-signature"})

		failStatusRR := env.do("GET", "/v1/orgs/"+org.ID.String()+"/bulk-topups/"+bFail.PotID.String(), ownerSub, nil, nil)
		var failView orchestrator.BulkTopupDetailView
		_ = json.Unmarshal(failStatusRR.Body.Bytes(), &failView)
		if failView.PotStatus != pots.StatusRefunded || failView.RefundedMinor != 410_000 || failView.FailedRefundedCount != 1 {
			t.Fatalf("expected all-fail bulk top-up REFUNDED with 410,000 refunded, got %+v", failView)
		}
		tb, _ := st.Repos().Ledger().TrialBalance(ctx)
		if tb != 0 {
			t.Fatalf("ledger trial balance non-zero: %d", tb)
		}
	})
}

func TestPowerDeskListOrgsAndBuildings(t *testing.T) {
	env := newPDEnv(t, memstore.New(), false)
	rr := env.do("GET", "/v1/orgs", "owner-list", nil, nil)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"data"`) {
		t.Fatalf("empty list %d %s", rr.Code, rr.Body.String())
	}
	rr = env.do("POST", "/v1/orgs", "owner-list", map[string]any{
		"name": "Listed Estate", "organisation_type": "ESTATE",
	}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create org %d %s", rr.Code, rr.Body.String())
	}
	var org orchestrator.OrgDetailView
	_ = json.Unmarshal(rr.Body.Bytes(), &org)
	rr = env.do("GET", "/v1/orgs", "owner-list", nil, nil)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), org.ID.String()) {
		t.Fatalf("list orgs %d %s", rr.Code, rr.Body.String())
	}
	rr = env.do("POST", "/v1/orgs/"+org.ID.String()+"/properties", "owner-list", map[string]any{
		"name": "Lekki Court", "city": "Lagos", "state": "LA",
	}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create prop %d %s", rr.Code, rr.Body.String())
	}
	var prop powerdesk.Property
	_ = json.Unmarshal(rr.Body.Bytes(), &prop)
	rr = env.do("GET", "/v1/properties/"+prop.ID.String()+"/buildings", "owner-list", nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list default buildings %d %s", rr.Code, rr.Body.String())
	}
	rr = env.do("POST", "/v1/properties/"+prop.ID.String()+"/buildings", "owner-list", map[string]any{
		"name": "Block C",
	}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create building %d %s", rr.Code, rr.Body.String())
	}
	rr = env.do("GET", "/v1/properties/"+prop.ID.String()+"/buildings", "owner-list", nil, nil)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "Block C") {
		t.Fatalf("list buildings %d %s", rr.Code, rr.Body.String())
	}
}
