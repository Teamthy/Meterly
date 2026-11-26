package httpapi_test

import (
	"encoding/base64"
	"encoding/json"
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
	"github.com/teamthy/meterly/internal/store"
	"github.com/teamthy/meterly/internal/store/memstore"
	"github.com/teamthy/meterly/internal/store/pgstore"
	"github.com/teamthy/meterly/internal/store/pgstore/pgtest"
)

func TestSupportAgentAndHumanEscalation(t *testing.T) {
	for _, b := range []struct {
		name string
		open func(*testing.T) store.Store
	}{
		{"memory", func(*testing.T) store.Store { return memstore.New() }},
		{"postgres", func(t *testing.T) store.Store { return pgstore.New(pgtest.NewPool(t), "SANDBOX") }},
	} {
		t.Run(b.name, func(t *testing.T) { supportFlow(t, b.open(t)) })
	}
}

func supportFlow(t *testing.T, st store.Store) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	box, _ := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "t1")
	pay, vend, remote := mock.NewPayments(), mock.NewVending(), mock.NewRemoteCredit()
	o := orchestrator.New(orchestrator.DefaultConfig(), st, registry.New(st, box, vend, remote), box, pay, vend, remote, log)
	s := &httpapi.Server{O: o, Auth: auth.DevAuth{}, Payments: pay, Log: log, Dev: true}
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	c := client{t, srv}

	code, created := c.do("POST", "/v1/support/threads", "ada", map[string]any{}, nil)
	if code != 201 {
		t.Fatalf("create %d %v", code, created)
	}
	thread := created["thread"].(map[string]any)
	id := thread["id"].(string)
	if thread["mode"] != "AGENT" {
		t.Fatalf("want AGENT got %v", thread["mode"])
	}
	msgs := created["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("welcome missing: %v", created)
	}
	welcome := msgs[0].(map[string]any)
	if welcome["e2e"] != false || welcome["body"] == nil {
		t.Fatalf("welcome must be server-readable: %v", welcome)
	}

	code, posted := c.do("POST", "/v1/support/threads/"+id+"/messages", "ada", map[string]any{"body": "how do refunds work?"}, nil)
	if code != 201 {
		t.Fatalf("agent post %d %v", code, posted)
	}
	agent := posted["agent_message"].(map[string]any)
	body, _ := agent["body"].(string)
	if body == "" || !containsFold(body, "UNKNOWN") {
		t.Fatalf("agent refund answer missing UNKNOWN: %v", agent)
	}

	code, other := c.do("GET", "/v1/support/threads/"+id, "bola", nil, nil)
	if code != 404 {
		t.Fatalf("bola must not see ada's thread: %d %v", code, other)
	}

	pub := base64.StdEncoding.EncodeToString(make([]byte, 91))
	code, key := c.do("PUT", "/v1/support/keys", "operator:kemi", map[string]any{"public_key": pub, "algorithm": "P-256"}, nil)
	if code != 200 {
		t.Fatalf("operator key %d %v", code, key)
	}
	if key["staff"] != true {
		t.Fatalf("operator key must be staff: %v", key)
	}
	code, roster := c.do("GET", "/v1/support/roster", "ada", nil, nil)
	if code != 200 {
		t.Fatalf("roster %d %v", code, roster)
	}
	data := roster["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("roster=%v", roster)
	}

	ct := base64.StdEncoding.EncodeToString([]byte("cipher-not-plaintext"))
	nonce := base64.StdEncoding.EncodeToString([]byte("123456789012"))
	code, esc := c.do("POST", "/v1/support/threads/"+id+"/escalate", "ada", map[string]any{
		"wrapped_keys": []map[string]any{{
			"holder_user_id": key["user_id"],
			"wrapped_key":    ct,
			"nonce":          nonce,
		}},
	}, nil)
	if code != 200 {
		t.Fatalf("escalate %d %v", code, esc)
	}
	eth := esc["thread"].(map[string]any)
	if eth["mode"] != "HUMAN" || eth["status"] != "WAITING_HUMAN" {
		t.Fatalf("escalate state %v", eth)
	}

	code, inbox := c.do("GET", "/v1/admin/support/threads?status=WAITING_HUMAN", "operator:kemi", nil, nil)
	if code != 200 {
		t.Fatalf("inbox %d %v", code, inbox)
	}
	if len(inbox["data"].([]any)) != 1 {
		t.Fatalf("inbox %v", inbox)
	}

	code, claimed := c.do("POST", "/v1/admin/support/threads/"+id+"/claim", "operator:kemi", map[string]any{"reason": "Customer asked for a person"}, nil)
	if code != 200 {
		t.Fatalf("claim %d %v", code, claimed)
	}

	code, opMsg := c.do("POST", "/v1/admin/support/threads/"+id+"/messages", "operator:kemi", map[string]any{
		"ciphertext": ct, "nonce": nonce,
	}, nil)
	if code != 201 {
		t.Fatalf("op message %d %v", code, opMsg)
	}
	raw, _ := json.Marshal(opMsg)
	if containsFold(string(raw), "cipher-not-plaintext") {
		t.Fatal("plaintext e2e payload leaked in JSON")
	}
	msg := opMsg["message"].(map[string]any)
	if msg["e2e"] != true || msg["body"] != nil {
		t.Fatalf("human message must be ciphertext only: %v", msg)
	}
}

func TestSupportFeatureDisabled(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	box, _ := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "t1")
	st := memstore.New()
	pay, vend, remote := mock.NewPayments(), mock.NewVending(), mock.NewRemoteCredit()
	o := orchestrator.New(orchestrator.DefaultConfig(), st, registry.New(st, box, vend, remote), box, pay, vend, remote, log)
	s := &httpapi.Server{O: o, Auth: auth.DevAuth{}, Payments: pay, Log: log, SupportDisabled: true}
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	c := client{t, srv}
	code, body := c.do("POST", "/v1/support/threads", "ada", map[string]any{}, nil)
	if code != 404 {
		t.Fatalf("disabled want 404 got %d %v", code, body)
	}
	errObj := body["error"].(map[string]any)
	if errObj["code"] != "FEATURE_DISABLED" {
		t.Fatalf("%v", body)
	}
}

func containsFold(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		(func() bool {
			ls, lsub := []byte(s), []byte(sub)
			// cheap ASCII fold
			fold := func(b byte) byte {
				if b >= 'A' && b <= 'Z' {
					return b + 32
				}
				return b
			}
			for i := 0; i+len(lsub) <= len(ls); i++ {
				ok := true
				for j := 0; j < len(lsub); j++ {
					if fold(ls[i+j]) != fold(lsub[j]) {
						ok = false
						break
					}
				}
				if ok {
					return true
				}
			}
			return false
		})())
}
