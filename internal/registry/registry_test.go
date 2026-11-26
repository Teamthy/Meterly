package registry_test

import (
	"context"
	"testing"
	"time"

	"github.com/teamthy/meterly/internal/meters"
	"github.com/teamthy/meterly/internal/providers/mock"
	"github.com/teamthy/meterly/internal/registry"
	"github.com/teamthy/meterly/internal/secure"
	"github.com/teamthy/meterly/internal/store/memstore"
)

func TestRemoteUsableRequiresProviderAndEligibility(t *testing.T) {
	ctx := context.Background()
	st := memstore.New()
	box, _ := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "t1")
	vend := mock.NewVending()

	none := registry.New(st, box, vend, nil)
	m, err := none.Register(ctx, "ada", "70011112222", "IKEDC", "smart")
	if err != nil {
		t.Fatal(err)
	}
	if m.Capabilities.RemoteCredit.Supported != true {
		t.Fatal("mock vending still reports 7… as supported")
	}
	if m.Capabilities.RemoteCredit.Authorised || m.Capabilities.RemoteCreditUsable() {
		t.Fatal("REMOTE_PROVIDER=none must never authorise remote credit")
	}

	st2 := memstore.New()
	with := registry.New(st2, box, vend, mock.NewRemoteCredit())
	m2, err := with.Register(ctx, "ada", "70011112222", "IKEDC", "smart")
	if err != nil {
		t.Fatal(err)
	}
	if !m2.Capabilities.RemoteCreditUsable() {
		t.Fatal("mock HES + 7… should be usable")
	}

	keypad, err := with.Register(ctx, "ada", "45011112222", "IKEDC", "keypad")
	if err != nil {
		t.Fatal(err)
	}
	if keypad.Capabilities.RemoteCreditUsable() {
		t.Fatal("keypad meter must not be remotely usable without a cohort")
	}
}

func TestCohortMarksSupportedThenHESAuthorises(t *testing.T) {
	ctx := context.Background()
	st := memstore.New()
	box, _ := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "t1")
	vend := mock.NewVending()
	remote := mock.NewRemoteCredit()
	g := registry.New(st, box, vend, remote)

	c, err := meters.NewRemoteCohort("IKEDC", "45", "Conlog", "operator matrix from MAP letter", "admin-1", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Repos().RemoteCohorts().Create(ctx, c); err != nil {
		t.Fatal(err)
	}

	// Mock HES only authorises prefix 7, so a 45… cohort is Supported but not Authorised.
	m, err := g.Register(ctx, "ada", "45011112222", "IKEDC", "cohort-keypad")
	if err != nil {
		t.Fatal(err)
	}
	if !m.Capabilities.RemoteCredit.Supported {
		t.Fatal("cohort must mark supported")
	}
	if m.Capabilities.RemoteCredit.Authorised || m.Capabilities.RemoteCreditUsable() {
		t.Fatal("HES eligibility still required for Authorised")
	}
}
