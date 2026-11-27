package httpapi

import (
	"context"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/meters"
	"github.com/teamthy/meterly/internal/notifications"
	"github.com/teamthy/meterly/internal/orchestrator"
	"github.com/teamthy/meterly/internal/providers"
	"github.com/teamthy/meterly/internal/store"
)

// DevConsole is the MODE=dev sandbox screen. Never registered in production.
type DevConsole struct {
	Title       string                       `json:"title"`
	Subtitle    string                       `json:"subtitle"`
	Copy        string                       `json:"copy"`
	Enabled     bool                         `json:"enabled"`
	MetersTitle string                       `json:"meters_title"`
	MetersEmpty string                       `json:"meters_empty"`
	Meters      []DevMeterRow                `json:"meters"`
	PayTitle    string                       `json:"payments_title"`
	PayEmpty    string                       `json:"payments_empty"`
	Payments    []DevPaymentRow              `json:"payments"`
	Remote      DevRemotePanel               `json:"remote"`
	RecentTitle string                       `json:"recent_title"`
	RecentEmpty string                       `json:"recent_empty"`
	Recent      []orchestrator.TxnRow        `json:"recent"`
	Actions     []orchestrator.ConsoleAction `json:"actions"`
}

type DevMeterRow struct {
	MeterID      uuid.UUID                  `json:"meter_id"`
	Masked       string                     `json:"masked_meter_number"`
	Disco        string                     `json:"disco"`
	MeterType    string                     `json:"meter_type"`
	DeliveryHint string                     `json:"delivery_hint"`
	RemoteUsable bool                       `json:"remote_usable"`
	ReadingLabel string                     `json:"reading_label"`
	ReadingHint  string                     `json:"reading_hint"`
	SetReading   orchestrator.ConsoleAction `json:"set_reading"`
}

type DevPaymentRow struct {
	Reference   string                     `json:"reference"`
	Status      string                     `json:"status"`
	StatusLabel string                     `json:"status_label"`
	AmountLabel string                     `json:"amount_label"`
	Summary     string                     `json:"summary"`
	Complete    orchestrator.ConsoleAction `json:"complete"`
	Fail        orchestrator.ConsoleAction `json:"fail"`
}

type DevRemotePanel struct {
	Title        string                     `json:"title"`
	Copy         string                     `json:"copy"`
	Pending      int                        `json:"pending"`
	PendingLabel string                     `json:"pending_label"`
	Settle       orchestrator.ConsoleAction `json:"settle"`
}

func (s *Server) devConsole(w http.ResponseWriter, r *http.Request) {
	c, err := s.buildDevConsole(r.Context())
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, c)
}

func (s *Server) buildDevConsole(ctx context.Context) (DevConsole, error) {
	metersList, err := s.O.Store().Repos().Meters().ListAll(ctx, 50)
	if err != nil {
		return DevConsole{}, err
	}
	recent, err := s.O.Search(ctx, store.TxnFilter{Limit: 50})
	if err != nil {
		return DevConsole{}, err
	}

	payOK := s.DevPay != nil
	remoteOK := s.DevRemote != nil
	readOK := s.DevReadings != nil

	c := DevConsole{
		Title:       "Sandbox console",
		Subtitle:    "Simulate payments, remote credit and meter readings. Never enabled in production.",
		Copy:        "MODE=dev only. Prefix-7 meters can take remote credit on the mock HES. Complete a mock payment, then settle remote credit when the meter should acknowledge. Credit on the meter; token only if the meter cannot take it.",
		Enabled:     true,
		MetersTitle: "Registered meters",
		MetersEmpty: "No meters yet. Register one from On the meter — prefix 7 for remote credit.",
		Meters:      s.devMeterRows(ctx, metersList, readOK),
		PayTitle:    "Mock payments",
		PayEmpty:    "No mock checkouts yet.",
		Payments:    s.devPaymentRows(payOK),
		Remote:      s.devRemotePanel(remoteOK),
		RecentTitle: "Transactions",
		RecentEmpty: "No transactions yet.",
		Recent:      orchestrator.TxnRows(recent),
		Actions: []orchestrator.ConsoleAction{
			{
				ID: "reconcile", Label: "Run resume worker", Method: "POST", Path: "/v1/dev/reconcile",
				Enabled: true, Hint: "Process due transactions once, the same pass the worker runs.",
			},
			{
				ID: "resume", Label: "Resume one transaction", Method: "POST",
				PathTemplate: "/v1/dev/transactions/{transaction_id}/resume",
				Enabled:      true, Hint: "Continue one purchase from its last committed state.",
			},
			{
				ID: "complete-payment", Label: "Complete mock payment", Method: "POST",
				PathTemplate: "/v1/dev/payments/{ref}/complete",
				Enabled:      payOK, Hint: "Marks the mock checkout succeeded (or failed if success=false).",
			},
			{
				ID: "settle-remote", Label: "Settle remote credit", Method: "POST", Path: "/v1/dev/remote/settle",
				Enabled: remoteOK, Hint: "Mock meters acknowledge queued ACCEPTED commands as CONFIRMED.",
			},
			{
				ID: "set-reading", Label: "Set mock reading", Method: "POST",
				PathTemplate: "/v1/dev/meters/{meter_id}/reading",
				Enabled:      readOK, Hint: "kWh remaining on a prefix-7 meter for usage and low-unit alerts.",
			},
		},
	}
	if !payOK {
		c.PayEmpty = "Mock payments are not wired in this process."
	}
	return c, nil
}

func (s *Server) devMeterRows(ctx context.Context, list []*meters.Meter, readOK bool) []DevMeterRow {
	out := make([]DevMeterRow, 0, len(list))
	for _, m := range list {
		if m == nil {
			continue
		}
		remote := m.Capabilities.RemoteCreditUsable()
		hint := "STS token — keypad meter"
		if remote {
			hint = "Credit on the meter — mock HES"
		}
		reading := "No mock reading"
		if readOK {
			if number, err := s.O.Box().Decrypt(m.NumberEncrypted); err == nil {
				if kwh, at, err := s.DevReadings.LatestReading(ctx, number); err == nil {
					reading = fmt.Sprintf("%.1f kWh · %s", kwh, at.UTC().Format("15:04 MST"))
				}
			}
		} else {
			reading = "Mock readings are not wired"
		}
		out = append(out, DevMeterRow{
			MeterID: m.ID, Masked: m.Masked, Disco: m.DiscoCode, MeterType: m.MeterType,
			DeliveryHint: hint, RemoteUsable: remote, ReadingLabel: reading,
			ReadingHint: "Sets remaining kWh on the mock AMI. Prefix-7 meters only report a reading after you set one.",
			SetReading: orchestrator.ConsoleAction{
				ID: "set-reading", Label: "Set reading", Method: "POST",
				Path:    "/v1/dev/meters/" + m.ID.String() + "/reading",
				Enabled: readOK, Hint: "Body: {\"kwh\": 82}",
			},
		})
	}
	return out
}

func (s *Server) devPaymentRows(payOK bool) []DevPaymentRow {
	if !payOK {
		return []DevPaymentRow{}
	}
	snaps := s.DevPay.Snapshot()
	out := make([]DevPaymentRow, 0, len(snaps))
	for _, p := range snaps {
		amount := notifications.FormatNairaMinor(p.Amount.Minor)
		statusLabel := mockPayLabel(p.Status)
		pending := p.Status == providers.PaymentPending
		out = append(out, DevPaymentRow{
			Reference: p.ProviderRef, Status: string(p.Status), StatusLabel: statusLabel,
			AmountLabel: amount, Summary: amount + " · " + statusLabel + " · " + p.ProviderRef,
			Complete: orchestrator.ConsoleAction{
				ID: "complete", Label: "Mark paid", Method: "POST",
				Path:    "/v1/dev/payments/" + p.ProviderRef + "/complete",
				Enabled: pending, Hint: "Customer finished checkout successfully.",
			},
			Fail: orchestrator.ConsoleAction{
				ID: "fail", Label: "Mark failed", Method: "POST",
				Path:    "/v1/dev/payments/" + p.ProviderRef + "/complete",
				Enabled: pending, Hint: "Body: {\"success\": false}",
			},
		})
	}
	return out
}

func (s *Server) devRemotePanel(ok bool) DevRemotePanel {
	p := DevRemotePanel{
		Title: "Mock remote credit",
		Copy:  "ACCEPTED is not delivery. Settle to confirm credit on the meter.",
		Settle: orchestrator.ConsoleAction{
			ID: "settle", Label: "Settle pending commands", Method: "POST", Path: "/v1/dev/remote/settle",
			Enabled: ok, Hint: "Every ACCEPTED mock command becomes CONFIRMED.",
		},
		PendingLabel: "Mock remote credit is not wired.",
	}
	if !ok {
		return p
	}
	pending := 0
	for _, c := range s.DevRemote.Snapshot() {
		if c.Status == providers.CreditAccepted {
			pending++
		}
	}
	p.Pending = pending
	p.PendingLabel = fmt.Sprintf("%d command(s) waiting for the meter to acknowledge", pending)
	return p
}

func mockPayLabel(st providers.PaymentStatus) string {
	switch st {
	case providers.PaymentPending:
		return "Waiting at checkout"
	case providers.PaymentSucceeded:
		return "Paid"
	case providers.PaymentFailed:
		return "Failed"
	default:
		return string(st)
	}
}
