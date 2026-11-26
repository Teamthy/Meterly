package orchestrator_test

import (
	"errors"
	"testing"
	"time"

	"github.com/teamthy/meterly/internal/meters"
	"github.com/teamthy/meterly/internal/orchestrator"
	"github.com/teamthy/meterly/internal/tariffs"
)

func TestBuyOptionsRecommendedPackAndUnitsFromServer(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		uid := e.user(t, "ada")
		m, err := e.o.Meters.Register(e.ctx, uid, "70011112222", "IKEDC", "Home")
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		e.readings.SetReading("70011112222", 4, now)
		if err := e.st.Repos().Readings().InsertSample(e.ctx, meters.ReadingSample{
			MeterID: m.ID, MilliKWh: 16000, ReadAt: now.Add(-3 * time.Hour), Provider: "mockreadings",
		}); err != nil {
			t.Fatal(err)
		}
		if err := e.st.Repos().Readings().InsertSample(e.ctx, meters.ReadingSample{
			MeterID: m.ID, MilliKWh: 4000, ReadAt: now, Provider: "mockreadings",
		}); err != nil {
			t.Fatal(err)
		}

		opts, err := e.o.BuyOptions(e.ctx, uid, m.ID, "")
		if err != nil {
			t.Fatal(err)
		}
		if opts.Disco.Code != "IKEDC" || opts.Tariff.KoboPerKWh != 20950 {
			t.Fatalf("tariff %+v disco %+v", opts.Tariff, opts.Disco)
		}
		if opts.Tariff.NairaPerKWh != 209.5 {
			t.Fatalf("naira/kWh %v", opts.Tariff.NairaPerKWh)
		}
		if len(opts.Packs) != 6 {
			t.Fatalf("packs %d", len(opts.Packs))
		}
		var rec orchestrator.PackView
		for _, p := range opts.Packs {
			if p.ID == "ngn-500" && (p.PayMinor != 48_000 || p.DiscountMinor != 2_000 || p.EstimatedUnitsKWh <= 0) {
				t.Fatalf("ngn-500 %+v", p)
			}
			if p.Recommended {
				rec = p
			}
		}
		if rec.ID == "" || rec.ID != opts.RecommendedPackID {
			t.Fatalf("recommended %q vs %q", rec.ID, opts.RecommendedPackID)
		}
		// 4 kWh remaining at 4 kWh/h → 48h cover needs ~188 kWh → largest pack.
		if rec.ID != "ngn-2000" {
			t.Fatalf("expected ngn-2000 for 48h cover, got %s (usage %+v)", rec.ID, opts.Usage)
		}
	})
}

func TestQuoteGatesCannotVendMinNameInFlightSandbox(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		uid := e.user(t, "ada")

		blocked, err := e.o.Meters.Register(e.ctx, uid, "45011110001", "IKEDC", "Blocked")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.o.IssueQuote(e.ctx, orchestrator.QuoteSpec{
			CustomerID: uid, MeterID: blocked.ID, AmountMinor: 500000, Pref: orchestrator.PrefToken,
		}); !errors.Is(err, meters.ErrCannotVend) {
			t.Fatalf("cannot vend: %v", err)
		}

		floor, err := e.o.Meters.Register(e.ctx, uid, "45011110002", "IKEDC", "Floor")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.o.IssueQuote(e.ctx, orchestrator.QuoteSpec{
			CustomerID: uid, MeterID: floor.ID, AmountMinor: 50_000, Pref: orchestrator.PrefToken,
		}); !errors.Is(err, orchestrator.ErrAmountOutOfRange) {
			t.Fatalf("min purchase: %v", err)
		}

		named, err := e.o.Meters.Register(e.ctx, uid, "45011112222", "IKEDC", "Named")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.o.IssueQuote(e.ctx, orchestrator.QuoteSpec{
			CustomerID: uid, MeterID: named.ID, AmountMinor: 500000, Pref: orchestrator.PrefToken,
			RequireNameConfirm: true,
		}); !errors.Is(err, orchestrator.ErrNameNotConfirmed) {
			t.Fatalf("name confirm: %v", err)
		}
		if _, err := e.o.IssueQuote(e.ctx, orchestrator.QuoteSpec{
			CustomerID: uid, MeterID: named.ID, AmountMinor: 500000, Pref: orchestrator.PrefToken,
			RequireNameConfirm: true, ConfirmCustomerName: true,
		}); err != nil {
			t.Fatalf("confirmed name: %v", err)
		}

		open := e.buy(t, "ada", "45011118888", orchestrator.PrefToken)
		if _, err := e.o.IssueQuote(e.ctx, orchestrator.QuoteSpec{
			CustomerID: uid, MeterID: open.MeterID, AmountMinor: 500000, Pref: orchestrator.PrefToken,
		}); !errors.Is(err, orchestrator.ErrInFlight) {
			t.Fatalf("in flight: %v status=%s", err, open.Status())
		}

		spaced, err := e.o.Meters.Register(e.ctx, uid, "4501 111 3333", "IKEDC", "Spaced")
		if err != nil {
			t.Fatal(err)
		}
		if spaced.Masked == "" {
			t.Fatal("normalised meter")
		}

		e.o.Meters.Live = true
		if _, err := e.o.Meters.Register(e.ctx, uid, "1111111111111", "IKEDC", "Sandbox"); !errors.Is(err, meters.ErrSandboxMeter) {
			t.Fatalf("sandbox live: %v", err)
		}
		e.o.Meters.Live = false
	})
}

func TestIssueQuotePackConfirmation(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		uid := e.user(t, "ada")
		m, err := e.o.Meters.Register(e.ctx, uid, "45011112222", "EKEDC", "Keypad")
		if err != nil {
			t.Fatal(err)
		}
		q, err := e.o.IssueQuote(e.ctx, orchestrator.QuoteSpec{
			CustomerID: uid, MeterID: m.ID, PackID: "ngn-500", Pref: orchestrator.PrefToken,
		})
		if err != nil {
			t.Fatal(err)
		}
		if q.Amount.Minor != 48_000 || q.Fee.Minor != 0 || q.Total.Minor != 48_000 {
			t.Fatalf("money %+v %+v %+v", q.Amount, q.Fee, q.Total)
		}
		if q.PackID != "ngn-500" || q.DiscoCode != "EKEDC" || q.KoboPerKWh != tariffs.KoboPerKWh("EKEDC", tariffs.BandA) {
			t.Fatalf("snapshot pack=%s disco=%s kobo=%d", q.PackID, q.DiscoCode, q.KoboPerKWh)
		}
		if q.EstimatedUnitsMilli <= 0 || q.DiscountMinor != 2_000 {
			t.Fatalf("units=%d discount=%d", q.EstimatedUnitsMilli, q.DiscountMinor)
		}
		conf := orchestrator.QuoteConfirmation(q, m)
		if conf.TotalMinor != 48_000 || conf.CtaLabel == "" || len(conf.Lines) < 5 {
			t.Fatalf("confirmation %+v", conf)
		}
		foundUnits := false
		for _, line := range conf.Lines {
			if line.Label == "Units that will be served" && line.Value != "" {
				foundUnits = true
			}
		}
		if !foundUnits {
			t.Fatalf("confirmation missing units: %+v", conf.Lines)
		}

		if _, err := e.o.IssueQuote(e.ctx, orchestrator.QuoteSpec{
			CustomerID: uid, MeterID: m.ID, PackID: "nope", Pref: orchestrator.PrefToken,
		}); err == nil {
			t.Fatal("expected invalid pack")
		}
	})
}

func TestPollLowUnitsIdempotent(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		uid := e.user(t, "ada")
		m, err := e.o.Meters.Register(e.ctx, uid, "70011112222", "IKEDC", "Home")
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		e.readings.SetReading("70011112222", 2, now)
		if err := e.st.Repos().Readings().InsertSample(e.ctx, meters.ReadingSample{
			MeterID: m.ID, MilliKWh: 2000, ReadAt: now, Provider: "mockreadings",
		}); err != nil {
			t.Fatal(err)
		}
		n, err := e.o.PollLowUnits(e.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("first poll notified %d", n)
		}
		n, err = e.o.PollLowUnits(e.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("cooldown should suppress, got %d", n)
		}
		e.later(25 * time.Hour)
		n, err = e.o.PollLowUnits(e.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("after cooldown notified %d", n)
		}
	})
}
