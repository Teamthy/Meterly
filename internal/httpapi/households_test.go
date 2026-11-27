package httpapi_test

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/teamthy/meterly/internal/auth"
	"github.com/teamthy/meterly/internal/httpapi"
	"github.com/teamthy/meterly/internal/orchestrator"
	"github.com/teamthy/meterly/internal/providers/mock"
	"github.com/teamthy/meterly/internal/registry"
	"github.com/teamthy/meterly/internal/secure"
	"github.com/teamthy/meterly/internal/store/memstore"
)

func TestHouseholdOwnershipAndDelegation(t *testing.T) {
	st := memstore.New()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	box, _ := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "t1")
	pay, vend, remote := mock.NewPayments(), mock.NewVending(), mock.NewRemoteCredit()
	o := orchestrator.New(orchestrator.DefaultConfig(), st, registry.New(st, box, vend, remote), box, pay, vend, remote, log)
	o.SetOwnershipNameMatch(true) // dev default; see TestOwnershipNameMatchDisabledByDefault
	s := &httpapi.Server{O: o, Auth: auth.DevAuth{}, Payments: pay, Log: log, Dev: true}
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)
	c := client{t, srv}

	const meter = "45012345678"
	nameHdr := map[string]string{"X-Dev-Display-Name": "TEST CUSTOMER 5678"}
	code, m := c.do("POST", "/v1/meters", "ada", map[string]any{"meter_number": meter, "disco_code": "IKEDC", "label": "Home"}, nil)
	if code != 201 {
		t.Fatalf("register %d %v", code, m)
	}
	meterID := m["meter_id"].(string)

	if code, body := c.do("POST", "/v1/meters/"+meterID+"/ownership/verify", "ada", map[string]any{}, nil); code != 422 {
		t.Fatalf("no display name: %d %v", code, body)
	}
	code, own := c.do("POST", "/v1/meters/"+meterID+"/ownership/verify", "ada", map[string]any{}, nameHdr)
	if code != 201 {
		t.Fatalf("verify %d %v", code, own)
	}
	if own["method"] != "ACCOUNT_NAME_MATCH" {
		t.Fatalf("method %v", own)
	}

	code, hh := c.do("POST", "/v1/households", "ada", map[string]any{"name": "Ada house"}, nameHdr)
	if code != 201 {
		t.Fatalf("create hh %d %v", code, hh)
	}
	hid := hh["household_id"].(string)
	if code, body := c.do("POST", "/v1/households/"+hid+"/meters", "ada", map[string]any{"meter_id": meterID}, nameHdr); code != 201 {
		t.Fatalf("add meter %d %v", code, body)
	}

	// Bob exists after any authed call.
	if code, _ := c.do("GET", "/v1/meters", "bob", nil, nil); code != 200 {
		t.Fatal("bob list")
	}
	code, mem := c.do("POST", "/v1/households/"+hid+"/members", "ada", map[string]any{"external_id": "bob", "role": "MEMBER"}, nameHdr)
	if code != 201 {
		t.Fatalf("invite %d %v", code, mem)
	}
	if code, body := c.do("POST", "/v1/electricity/quotes", "bob", map[string]any{"meter_id": meterID, "amount_minor": 500000, "currency": "NGN"}, nil); code != 403 {
		t.Fatalf("invited-not-accepted must not purchase: %d %v", code, body)
	}
	if code, body := c.do("POST", "/v1/households/"+hid+"/members/me/accept", "bob", map[string]any{}, nil); code != 200 {
		t.Fatalf("accept %d %v", code, body)
	}
	code, q := c.do("POST", "/v1/electricity/quotes", "bob", map[string]any{"meter_id": meterID, "amount_minor": 500000, "currency": "NGN"}, nil)
	if code != 201 {
		t.Fatalf("household member quote %d %v", code, q)
	}

	code, listed := c.do("GET", "/v1/meters", "bob", nil, nil)
	if code != 200 {
		t.Fatalf("bob meters %d %v", code, listed)
	}

	if code, body := c.do("DELETE", "/v1/households/"+hid+"/members/"+mem["user_id"].(string), "ada", nil, nameHdr); code != 200 {
		t.Fatalf("revoke %d %v", code, body)
	}
	if code, _ := c.do("POST", "/v1/electricity/quotes", "bob", map[string]any{"meter_id": meterID, "amount_minor": 500000, "currency": "NGN"}, nil); code != 403 {
		t.Fatalf("revoked member must not purchase")
	}
}

func TestHouseholdsFeatureFlag(t *testing.T) {
	st := memstore.New()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	box, _ := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "t1")
	pay, vend, remote := mock.NewPayments(), mock.NewVending(), mock.NewRemoteCredit()
	o := orchestrator.New(orchestrator.DefaultConfig(), st, registry.New(st, box, vend, remote), box, pay, vend, remote, log)
	s := &httpapi.Server{O: o, Auth: auth.DevAuth{}, Payments: pay, Log: log, HouseholdsDisabled: true}
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)
	c := client{t, srv}
	if code, _ := c.do("POST", "/v1/households", "ada", map[string]any{"name": "x"}, nil); code != 404 {
		t.Fatalf("disabled: %d", code)
	}
}

// Display names are user-editable and meter validation reveals the DisCo
// customer name, so self-service name matching must be off unless enabled.
func TestOwnershipNameMatchDisabledByDefault(t *testing.T) {
	st := memstore.New()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	box, _ := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "t1")
	pay, vend, remote := mock.NewPayments(), mock.NewVending(), mock.NewRemoteCredit()
	o := orchestrator.New(orchestrator.DefaultConfig(), st, registry.New(st, box, vend, remote), box, pay, vend, remote, log)
	s := &httpapi.Server{O: o, Auth: auth.DevAuth{}, Payments: pay, Log: log, Dev: true}
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)
	c := client{t, srv}

	// Mallory learns the DisCo customer name from validation...
	code, v := c.do("POST", "/v1/meters/validate", "mallory", map[string]any{"meter_number": "45012345678", "disco_code": "IKEDC"}, nil)
	if code != 200 {
		t.Fatalf("validate %d %v", code, v)
	}
	_, m := c.do("POST", "/v1/meters", "mallory", map[string]any{"meter_number": "45012345678", "disco_code": "IKEDC"}, nil)
	// ...sets it as her display name, and tries to become the owner.
	spoof := map[string]string{"X-Dev-Display-Name": v["customer_name"].(string)}
	code, body := c.do("POST", "/v1/meters/"+m["meter_id"].(string)+"/ownership/verify", "mallory", map[string]any{}, spoof)
	if code != 403 {
		t.Fatalf("spoofed ownership verification must be refused, got %d %v", code, body)
	}
	if e, _ := body["error"].(map[string]any); e["code"] != "OWNERSHIP_REVIEW_REQUIRED" {
		t.Fatalf("error %v", body)
	}
}
