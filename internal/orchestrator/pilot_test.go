package orchestrator_test

import (
	"errors"
	"testing"
	"time"

	"github.com/teamthy/meterly/internal/audit"
	"github.com/teamthy/meterly/internal/orchestrator"
	"github.com/teamthy/meterly/internal/pilot"
)

func TestPilotAllowListAndCaps(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		uid := e.user(t, "ada")
		m := e.register(t, "ada", "45011118881")
		e.o.SetPilot(true, 0, 0)
		if _, err := e.o.IssueQuote(e.ctx, orchestrator.QuoteSpec{
			CustomerID: uid, MeterID: m.ID, AmountMinor: 500000, Pref: orchestrator.PrefToken,
		}); !errors.Is(err, orchestrator.ErrServicePaused) {
			t.Fatalf("unlisted user: %v", err)
		}
		actor := audit.Actor{UserID: "admin:test", Type: audit.ActorOperator, Reason: "pilot cohort household"}
		if err := e.o.AddPilotEntry(e.ctx, actor, pilot.Entry{Kind: pilot.KindUser, Value: uid}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.o.IssueQuote(e.ctx, orchestrator.QuoteSpec{
			CustomerID: uid, MeterID: m.ID, AmountMinor: 500000, Pref: orchestrator.PrefToken,
		}); !errors.Is(err, orchestrator.ErrServicePaused) {
			t.Fatalf("unlisted meter: %v", err)
		}
		if err := e.o.AddPilotEntry(e.ctx, actor, pilot.Entry{Kind: pilot.KindMeter, Value: m.ID.String()}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.o.IssueQuote(e.ctx, orchestrator.QuoteSpec{
			CustomerID: uid, MeterID: m.ID, AmountMinor: 500000, Pref: orchestrator.PrefToken,
		}); err != nil {
			t.Fatalf("allow-listed: %v", err)
		}
		if err := e.o.AddPilotEntry(e.ctx, actor, pilot.Entry{Kind: pilot.KindDisco, Value: "ekedc"}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.o.IssueQuote(e.ctx, orchestrator.QuoteSpec{
			CustomerID: uid, MeterID: m.ID, AmountMinor: 500000, Pref: orchestrator.PrefToken,
		}); !errors.Is(err, orchestrator.ErrServicePaused) {
			t.Fatalf("wrong disco: %v", err)
		}
		if err := e.o.AddPilotEntry(e.ctx, actor, pilot.Entry{Kind: pilot.KindDisco, Value: "IKEDC"}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.o.IssueQuote(e.ctx, orchestrator.QuoteSpec{
			CustomerID: uid, MeterID: m.ID, AmountMinor: 500000, Pref: orchestrator.PrefToken,
		}); err != nil {
			t.Fatalf("ikedc listed: %v", err)
		}
		e.o.SetPilot(true, 1000, 0)
		if _, err := e.o.IssueQuote(e.ctx, orchestrator.QuoteSpec{
			CustomerID: uid, MeterID: m.ID, AmountMinor: 500000, Pref: orchestrator.PrefToken,
		}); !errors.Is(err, orchestrator.ErrPilotCap) {
			t.Fatalf("cap: %v", err)
		}
	})
}

func TestServicePausedBlocksQuotes(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		uid := e.user(t, "ada")
		m := e.register(t, "ada", "45011118882")
		if err := e.o.PauseService(e.ctx, audit.Actor{UserID: "admin:test", Type: audit.ActorOperator, Reason: "disco float incident"}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.o.IssueQuote(e.ctx, orchestrator.QuoteSpec{
			CustomerID: uid, MeterID: m.ID, AmountMinor: 500000, Pref: orchestrator.PrefToken,
		}); !errors.Is(err, orchestrator.ErrServicePaused) {
			t.Fatalf("paused: %v", err)
		}
		if err := e.o.ResumeService(e.ctx, audit.Actor{UserID: "admin:test", Type: audit.ActorOperator, Reason: "float restored after topup"}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.o.IssueQuote(e.ctx, orchestrator.QuoteSpec{
			CustomerID: uid, MeterID: m.ID, AmountMinor: 500000, Pref: orchestrator.PrefToken,
		}); err != nil {
			t.Fatalf("resumed: %v", err)
		}
	})
}

func TestWalletPausesBelowThreeDayAverageSpend(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		paid := e.buy(t, "ada", "45011118883", orchestrator.PrefToken)
		if paid.Amount.Minor != 500000 {
			t.Fatalf("amount %d", paid.Amount.Minor)
		}
		e.vend.SetWalletMinor(100_000)
		e.o.SetWallet(e.vend, 1)
		uid := e.user(t, "ada")
		m := e.register(t, "ada", "45011118884")
		if _, err := e.o.IssueQuote(e.ctx, orchestrator.QuoteSpec{
			CustomerID: uid, MeterID: m.ID, AmountMinor: 500000, Pref: orchestrator.PrefToken,
		}); !errors.Is(err, orchestrator.ErrVendingPaused) {
			t.Fatalf("below 3-day avg: %v", err)
		}
		e.vend.SetWalletMinor(200_000)
		e.later(31 * time.Second)
		if _, err := e.o.IssueQuote(e.ctx, orchestrator.QuoteSpec{
			CustomerID: uid, MeterID: m.ID, AmountMinor: 500000, Pref: orchestrator.PrefToken,
		}); err != nil {
			t.Fatalf("above 3-day avg: %v", err)
		}
	})
}
