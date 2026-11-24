package pots_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/payrequests"
	"github.com/teamthy/meterly/internal/pots"
	"github.com/teamthy/meterly/internal/secure"
)

func TestPotAndContributionTransitions(t *testing.T) {
	now := time.Now().UTC()

	p := &pots.Pot{Status: pots.StatusOpen}
	if err := p.Transition(pots.StatusSettled, now); err == nil {
		t.Fatal("OPEN -> SETTLED must be rejected")
	}
	for _, to := range []pots.Status{pots.StatusFunded, pots.StatusAllocating, pots.StatusSettled} {
		if err := p.Transition(to, now); err != nil {
			t.Fatalf("unexpected transition error to %s: %v", to, err)
		}
	}
	if err := p.Transition(pots.StatusRefunding, now); err == nil {
		t.Fatal("SETTLED -> REFUNDING must be rejected")
	}

	p2 := &pots.Pot{Status: pots.StatusOpen}
	for _, to := range []pots.Status{pots.StatusExpired, pots.StatusRefunding, pots.StatusRefunded} {
		if err := p2.Transition(to, now); err != nil {
			t.Fatalf("expiry refund path to %s failed: %v", to, err)
		}
	}

	cid := uuid.New()
	c := &pots.Contribution{ID: cid, Status: pots.ContribPending, RefundStep: 1}
	if got := c.RefundReference(); got != "refund:contribution:"+cid.String() {
		t.Fatalf("unexpected step-1 RefundReference %q", got)
	}
	c.RefundStep = 2
	if got := c.RefundReference(); got != "refund:contribution:"+cid.String()+":2" {
		t.Fatalf("unexpected step-2 RefundReference %q", got)
	}
	if err := c.Transition(pots.ContribRefunded, now); err == nil {
		t.Fatal("PENDING -> REFUNDED without REFUND_PENDING must fail")
	}
	if err := c.Transition(pots.ContribRefundPending, now); err != nil {
		t.Fatal(err)
	}
	if err := c.Transition(pots.ContribPaid, now); err != nil {
		t.Fatalf("partial overfund REFUND_PENDING -> PAID should succeed: %v", err)
	}
	if err := c.Transition(pots.ContribRefundPending, now); err != nil {
		t.Fatal(err)
	}
	if err := c.Transition(pots.ContribRefunded, now); err != nil {
		t.Fatal(err)
	}
}

func TestFormatInitialsAndCodeDomainSeparation(t *testing.T) {
	cases := map[string]string{
		"":                        "Anonymous",
		"   ":                     "Anonymous",
		"Ada":                     "A.",
		"Tunde Okafor":            "T.O.",
		"Chinedu Obi Nwosu Extra": "C.O.N.",
		"🙏":                       "Anonymous",
	}
	for in, want := range cases {
		if got := pots.FormatInitials(in); got != want {
			t.Errorf("FormatInitials(%q) = %q, want %q", in, got, want)
		}
	}

	box, err := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "v1")
	if err != nil {
		t.Fatal(err)
	}
	code, err := pots.GenerateCode()
	if err != nil {
		t.Fatal(err)
	}
	potHMAC, ok := pots.CodeHMAC(box, code)
	if !ok {
		t.Fatal("expected valid pot code HMAC")
	}
	payReqHMAC, _ := payrequests.LinkCodeHMAC(box, code)
	if potHMAC == payReqHMAC {
		t.Fatal("pot code HMAC must be domain-separated from payrequest code HMAC")
	}
}
