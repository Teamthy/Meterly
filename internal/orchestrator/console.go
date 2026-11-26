package orchestrator

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/audit"
	"github.com/teamthy/meterly/internal/ledger"
	"github.com/teamthy/meterly/internal/meters"
	"github.com/teamthy/meterly/internal/notifications"
	"github.com/teamthy/meterly/internal/store"
	"github.com/teamthy/meterly/internal/support"
	"github.com/teamthy/meterly/internal/tariffs"
	tx "github.com/teamthy/meterly/internal/transactions"
)

// westAfrica is Nigeria local time for operator-facing timestamps.
var westAfrica = time.FixedZone("WAT", 3600)

// ConsoleAction is a mutation the UI may offer. Enabled is decided here so
// the client does not re-derive operator rules from raw status fields.
type ConsoleAction struct {
	ID           string `json:"id"`
	Label        string `json:"label"`
	Method       string `json:"method"`
	Path         string `json:"path,omitempty"`
	PathTemplate string `json:"path_template,omitempty"`
	NeedsReason  bool   `json:"needs_reason"`
	NeedsAdmin   bool   `json:"needs_admin,omitempty"`
	Enabled      bool   `json:"enabled"`
	Hint         string `json:"hint,omitempty"`
}

// TxnRow is one transaction as the ops console lists it.
type TxnRow struct {
	TransactionID  uuid.UUID `json:"transaction_id"`
	Status         string    `json:"status"`
	StatusLabel    string    `json:"status_label"`
	Summary        string    `json:"summary"`
	AmountLabel    string    `json:"amount_label"`
	TotalMinor     int64     `json:"total_minor"`
	Currency       string    `json:"currency"`
	Payment        string    `json:"payment_status"`
	Vending        string    `json:"vending_status"`
	Delivery       string    `json:"delivery_status"`
	Method         string    `json:"delivery_method"`
	MethodLabel    string    `json:"method_label"`
	ReviewReason   string    `json:"review_reason,omitempty"`
	CreditOnMeter  bool      `json:"credit_on_meter"`
	TokenAvailable bool      `json:"token_available"`
	CustomerID     string    `json:"customer_id"`
	MeterID        uuid.UUID `json:"meter_id"`
	CreatedAt      time.Time `json:"created_at"`
	WhenLabel      string    `json:"when_label"`
}

// ProviderRow is a circuit-switch row on the ops console.
type ProviderRow struct {
	Name        string        `json:"name"`
	Type        string        `json:"type"`
	Enabled     bool          `json:"enabled"`
	StatusLabel string        `json:"status_label"`
	Action      ConsoleAction `json:"action"`
}

// CohortRow is an attested remote-credit cohort as the console lists it.
type CohortRow struct {
	ID       uuid.UUID      `json:"id"`
	Summary  string         `json:"summary"`
	Evidence string         `json:"evidence"`
	Revoked  bool           `json:"revoked"`
	Action   *ConsoleAction `json:"action,omitempty"`
}

// ReasonSpec tells the UI how to collect the mandatory audit reason.
type ReasonSpec struct {
	Label       string `json:"label"`
	Hint        string `json:"hint"`
	Placeholder string `json:"placeholder"`
	MinLength   int    `json:"min_length"`
	MaxLength   int    `json:"max_length"`
}

// Option is a labelled identifier for a form control.
type Option struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// UnitMeterForm is the Power Desk verification panel.
type UnitMeterForm struct {
	Title   string        `json:"title"`
	Hint    string        `json:"hint"`
	Methods []Option      `json:"methods"`
	Action  ConsoleAction `json:"action"`
}

// SupportSummary is inbox counts for the ops home screen.
type SupportSummary struct {
	Title        string `json:"title"`
	WaitingHuman int    `json:"waiting_human"`
	Assigned     int    `json:"assigned"`
	Open         int    `json:"open"`
	InboxPath    string `json:"inbox_path"`
	Summary      string `json:"summary"`
}

// AdminConsole is the composed ops home screen. The UI renders these fields;
// it must not recompute queues, counts, or which switches staff may flip.
type AdminConsole struct {
	Title        string         `json:"title"`
	Subtitle     string         `json:"subtitle"`
	Copy         string         `json:"copy"`
	Role         string         `json:"role"`
	CanAdmin     bool           `json:"can_admin"`
	Reason       ReasonSpec     `json:"reason"`
	Summary      []ScreenLine   `json:"summary"`
	ReviewTitle  string         `json:"review_title"`
	ReviewEmpty  string         `json:"review_empty"`
	Review       []TxnRow       `json:"review"`
	RecentTitle  string         `json:"recent_title"`
	RecentEmpty  string         `json:"recent_empty"`
	Recent       []TxnRow       `json:"recent"`
	AuditTitle   string         `json:"audit_title"`
	AuditEmpty   string         `json:"audit_empty"`
	Audit        []ScreenLine   `json:"audit"`
	Providers    []ProviderRow  `json:"providers"`
	CohortsTitle string         `json:"cohorts_title"`
	CohortsCopy  string         `json:"cohorts_copy"`
	Cohorts      []CohortRow    `json:"cohorts"`
	Discos       []DiscoView    `json:"discos"`
	UnitMeter    UnitMeterForm  `json:"unit_meter"`
	CohortAttest ConsoleAction  `json:"cohort_attest"`
	Support      SupportSummary `json:"support"`
}

// Investigation is the composed transaction inspection pane.
type Investigation struct {
	Title              string          `json:"title"`
	Subtitle           string          `json:"subtitle"`
	TransactionID      uuid.UUID       `json:"transaction_id"`
	Status             string          `json:"status"`
	StatusLabel        string          `json:"status_label"`
	Payment            string          `json:"payment_status"`
	Vending            string          `json:"vending_status"`
	Delivery           string          `json:"delivery_status"`
	Method             string          `json:"delivery_method"`
	MethodLabel        string          `json:"method_label"`
	CreditOnMeter      bool            `json:"credit_on_meter"`
	TokenAvailable     bool            `json:"token_available"`
	Lines              []ScreenLine    `json:"lines"`
	Ledger             []ScreenLine    `json:"ledger"`
	LedgerEmpty        string          `json:"ledger_empty"`
	Notifications      []ScreenLine    `json:"notifications"`
	NotificationsEmpty string          `json:"notifications_empty"`
	Audit              []ScreenLine    `json:"audit"`
	AuditEmpty         string          `json:"audit_empty"`
	Events             []ScreenLine    `json:"events"`
	History            []ScreenLine    `json:"history"`
	Actions            []ConsoleAction `json:"actions"`
}

// AdminConsole builds the ops home screen for the caller's role.
func (o *Orchestrator) AdminConsole(ctx context.Context, isAdmin bool) (AdminConsole, error) {
	r := o.st.Repos()
	reviewN, err := r.Transactions().Count(ctx, store.TxnFilter{ReviewOnly: true})
	if err != nil {
		return AdminConsole{}, err
	}
	totalN, err := r.Transactions().Count(ctx, store.TxnFilter{})
	if err != nil {
		return AdminConsole{}, err
	}
	payPending, err := r.Transactions().Count(ctx, store.TxnFilter{Payment: tx.PayPending})
	if err != nil {
		return AdminConsole{}, err
	}
	payCreated, err := r.Transactions().Count(ctx, store.TxnFilter{Payment: tx.PayCreated})
	if err != nil {
		return AdminConsole{}, err
	}

	reviewTxns, err := r.Transactions().Search(ctx, store.TxnFilter{ReviewOnly: true, Limit: 50})
	if err != nil {
		return AdminConsole{}, err
	}
	recentTxns, err := r.Transactions().Search(ctx, store.TxnFilter{Limit: 50})
	if err != nil {
		return AdminConsole{}, err
	}
	auditEvs, err := r.Audit().Recent(ctx, 25)
	if err != nil {
		return AdminConsole{}, err
	}
	st, err := o.ProviderStatus(ctx)
	if err != nil {
		return AdminConsole{}, err
	}
	cohorts, err := o.ListRemoteCohorts(ctx)
	if err != nil {
		return AdminConsole{}, err
	}

	threads, err := r.Support().ListThreadsForStaff(ctx, "", 200)
	if err != nil {
		return AdminConsole{}, err
	}
	waiting, assigned, open := 0, 0, 0
	for _, th := range threads {
		switch th.Status {
		case support.StatusWaitingHuman:
			waiting++
		case support.StatusAssigned:
			assigned++
		case support.StatusOpen:
			open++
		}
	}

	role := "OPERATOR"
	if isAdmin {
		role = "ADMIN"
	}
	review := txnRows(reviewTxns)
	recent := txnRows(recentTxns)
	discos := make([]DiscoView, 0, len(tariffs.Discos()))
	for _, d := range tariffs.Discos() {
		discos = append(discos, DiscoView{Code: d.Code, Name: d.Name})
	}

	c := AdminConsole{
		Title:    "Ops console",
		Subtitle: "Reconciliation, double-entry ledger inspection and audited actions.",
		Copy:     "Every staff mutation needs a 5–500 character reason and writes an audit_events row in the same database transaction as the change.",
		Role:     role,
		CanAdmin: isAdmin,
		Reason: ReasonSpec{
			Label:       "Mandatory audit reason",
			Hint:        "5–500 characters. Stored on the audit event; never put tokens, meter numbers or contact details here.",
			Placeholder: "Operator investigation per runbook",
			MinLength:   5,
			MaxLength:   500,
		},
		Summary: []ScreenLine{
			{Label: "Needs review", Value: fmt.Sprintf("%d", reviewN), Emphasis: reviewN > 0},
			{Label: "Transactions", Value: fmt.Sprintf("%d", totalN)},
			{Label: "Waiting on payment", Value: fmt.Sprintf("%d", payCreated+payPending)},
			{Label: "Support waiting for a human", Value: fmt.Sprintf("%d", waiting)},
		},
		ReviewTitle:  "Manual review",
		ReviewEmpty:  "Nothing in manual review.",
		Review:       review,
		RecentTitle:  "Recent transactions",
		RecentEmpty:  "No transactions yet.",
		Recent:       recent,
		AuditTitle:   "Recent audit",
		AuditEmpty:   "No audit events yet.",
		Audit:        auditLines(auditEvs),
		Providers:    o.providerRows(st, isAdmin),
		CohortsTitle: "Remote-credit cohorts",
		CohortsCopy:  "Attest a DisCo + digit prefix after a MAP/DisCo letter. This marks supported, not a live HES session. Do not treat meter prefix 7 as production AMI.",
		Cohorts:      cohortRows(cohorts, isAdmin),
		Discos:       discos,
		CohortAttest: ConsoleAction{
			ID: "attest-cohort", Label: "Attest cohort", Method: "POST",
			Path: "/v1/admin/remote-credit/cohorts", NeedsAdmin: true, Enabled: isAdmin,
			Hint: "Evidence 5–500 characters. This is an attestation, not a HES client.",
		},
		UnitMeter: UnitMeterForm{
			Title: "Verify Power Desk unit meter",
			Hint:  "Paste a unit-meter UUID. Evidence is audited with the verification.",
			Methods: []Option{
				{ID: "EVIDENCE_APPROVED", Label: "Evidence approved"},
				{ID: "ACCOUNT_NAME_MATCH", Label: "Account name match"},
			},
			Action: ConsoleAction{
				ID: "verify-unit-meter", Label: "Approve & audit unit meter", Method: "POST",
				PathTemplate: "/v1/admin/unit-meters/{id}/verify", NeedsReason: true, Enabled: true,
			},
		},
		Support: SupportSummary{
			Title:        "Support inbox",
			WaitingHuman: waiting,
			Assigned:     assigned,
			Open:         open,
			InboxPath:    "/admin/support",
			Summary:      fmt.Sprintf("%d waiting for a human · %d assigned · %d open", waiting, assigned, open),
		},
	}
	return c, nil
}

// InvestigationFrom formats Detail for the ops inspection pane.
func InvestigationFrom(d *Detail, isAdmin bool) Investigation {
	if d == nil || d.Transaction == nil {
		return Investigation{
			LedgerEmpty:        "No ledger entries posted yet.",
			NotificationsEmpty: "No notification rows recorded.",
			AuditEmpty:         "No audit events on this transaction.",
			Actions:            []ConsoleAction{},
			Lines:              []ScreenLine{},
			Ledger:             []ScreenLine{},
			Notifications:      []ScreenLine{},
			Audit:              []ScreenLine{},
			Events:             []ScreenLine{},
			History:            []ScreenLine{},
		}
	}
	t := d.Transaction
	row := txnRow(t)
	lines := []ScreenLine{
		{Label: "Amount", Value: row.AmountLabel, Emphasis: true},
		{Label: "How credit arrives", Value: row.MethodLabel},
		{Label: "Payment", Value: string(t.Payment)},
		{Label: "Vending", Value: string(t.Vending)},
		{Label: "Delivery", Value: string(t.Delivery)},
		{Label: "Customer", Value: t.CustomerID},
		{Label: "Meter", Value: t.MeterID.String()},
		{Label: "Opened", Value: row.WhenLabel},
	}
	if t.ReviewReason != "" {
		lines = append(lines, ScreenLine{Label: "Review reason", Value: t.ReviewReason, Emphasis: true})
	}
	if t.FailureCode != "" {
		lines = append(lines, ScreenLine{Label: "Failure code", Value: t.FailureCode})
	}
	if t.PaymentProviderRef != "" {
		lines = append(lines, ScreenLine{Label: "Payment reference", Value: t.PaymentProviderRef})
	}
	if t.UnitsKWh != "" {
		lines = append(lines, ScreenLine{Label: "Units", Value: t.UnitsKWh + " kWh"})
	}
	if t.RefundAmount.Minor > 0 {
		lines = append(lines, ScreenLine{Label: "Refund", Value: notifications.FormatNairaMinor(t.RefundAmount.Minor)})
	}
	return Investigation{
		Title:              row.AmountLabel + " · " + row.StatusLabel,
		Subtitle:           t.ID.String(),
		TransactionID:      t.ID,
		Status:             row.Status,
		StatusLabel:        row.StatusLabel,
		Payment:            row.Payment,
		Vending:            row.Vending,
		Delivery:           row.Delivery,
		Method:             row.Method,
		MethodLabel:        row.MethodLabel,
		CreditOnMeter:      row.CreditOnMeter,
		TokenAvailable:     row.TokenAvailable,
		Lines:              lines,
		Ledger:             ledgerLines(d.Ledger),
		LedgerEmpty:        "No ledger entries posted yet.",
		Notifications:      notifLines(d.Notifications),
		NotificationsEmpty: "No notification rows recorded. Destinations and message bodies are never shown here.",
		Audit:              auditLines(d.Audit),
		AuditEmpty:         "No audit events on this transaction.",
		Events:             eventLines(d),
		History:            historyLines(t),
		Actions:            investigationActions(t, isAdmin),
	}
}

// TxnRows formats transactions for operator and sandbox lists.
func TxnRows(list []*tx.Transaction) []TxnRow {
	return txnRows(list)
}

func txnRows(list []*tx.Transaction) []TxnRow {
	out := make([]TxnRow, 0, len(list))
	for _, t := range list {
		if t == nil {
			continue
		}
		out = append(out, txnRow(t))
	}
	return out
}

func txnRow(t *tx.Transaction) TxnRow {
	st := t.Status()
	amount := notifications.FormatNairaMinor(t.Total.Minor)
	label := overallLabel(st)
	method := methodLabel(t)
	summary := amount + " · " + label
	if t.ReviewReason != "" {
		summary += " · " + t.ReviewReason
	}
	cur := string(t.Total.Currency)
	if cur == "" {
		cur = "NGN"
	}
	return TxnRow{
		TransactionID:  t.ID,
		Status:         string(st),
		StatusLabel:    label,
		Summary:        summary,
		AmountLabel:    amount,
		TotalMinor:     t.Total.Minor,
		Currency:       cur,
		Payment:        string(t.Payment),
		Vending:        string(t.Vending),
		Delivery:       string(t.Delivery),
		Method:         string(t.DeliveryMethod),
		MethodLabel:    method,
		ReviewReason:   t.ReviewReason,
		CreditOnMeter:  t.DeliveryMethod == tx.MethodRemote && t.Delivery == tx.DelConfirmed,
		TokenAvailable: len(t.TokenCiphertext) > 0,
		CustomerID:     t.CustomerID,
		MeterID:        t.MeterID,
		CreatedAt:      t.CreatedAt,
		WhenLabel:      whenLabel(t.CreatedAt),
	}
}

func overallLabel(s tx.Overall) string {
	switch s {
	case tx.OverallManualReview:
		return "Manual review"
	case tx.OverallPaymentPending:
		return "Waiting for payment"
	case tx.OverallVendingPending:
		return "Vending"
	case tx.OverallDeliveryPending:
		return "Delivering"
	case tx.OverallReconciliation:
		return "Reconciliation"
	case tx.OverallRefundPending:
		return "Refund pending"
	case tx.OverallRefunded:
		return "Refunded"
	case tx.OverallCompleted:
		return "Completed"
	case tx.OverallFailed:
		return "Failed"
	default:
		return string(s)
	}
}

func methodLabel(t *tx.Transaction) string {
	if t.DeliveryMethod == tx.MethodRemote {
		if t.Delivery == tx.DelConfirmed {
			return "Credit on the meter"
		}
		return "Remote credit · " + string(t.Delivery)
	}
	return "STS token"
}

func investigationActions(t *tx.Transaction, isAdmin bool) []ConsoleAction {
	base := "/v1/admin/transactions/" + t.ID.String()
	return []ConsoleAction{
		{
			ID: "reconcile", Label: "Reconcile", Method: "POST", Path: base + "/reconcile",
			NeedsReason: true, Enabled: true,
			Hint: "Ask the providers again and continue from the last committed state.",
		},
		{
			ID: "fallback-to-token", Label: "Fallback to token", Method: "POST", Path: base + "/fallback-to-token",
			NeedsReason: true, Enabled: t.Delivery == tx.DelFailed,
			Hint: "Only after confirmed remote failure — never while ACCEPTED.",
		},
		{
			ID: "token-reveal", Label: "Audited token reveal", Method: "POST", Path: base + "/token-reveal",
			NeedsReason: true, Enabled: len(t.TokenCiphertext) > 0,
			Hint: "Masked everywhere else. Every reveal is audited.",
		},
		{
			ID: "requeue", Label: "Requeue", Method: "POST", Path: base + "/requeue",
			NeedsReason: true, NeedsAdmin: true, Enabled: isAdmin && t.ReviewReason != "",
			Hint: "Admin only. Resume automatic processing after a provider outage.",
		},
		{
			ID: "refund", Label: "Operator refund", Method: "POST", Path: base + "/refund",
			NeedsReason: true, NeedsAdmin: true,
			Enabled: isAdmin && t.ReviewReason != "" && (t.Vending == tx.VendNotStarted || t.Vending == tx.VendFailed),
			Hint:    "Admin only. Refund from review when electricity was never vended.",
		},
	}
}

func (o *Orchestrator) providerRows(st map[string]bool, isAdmin bool) []ProviderRow {
	names := make([]string, 0, len(st))
	for n := range st {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]ProviderRow, 0, len(names))
	for _, name := range names {
		enabled := st[name]
		status := "Paused"
		actionLabel, verb := "Enable", "enable"
		if enabled {
			status = "Enabled"
			actionLabel, verb = "Pause", "disable"
		}
		out = append(out, ProviderRow{
			Name: name, Type: o.providerKind(name), Enabled: enabled, StatusLabel: status,
			Action: ConsoleAction{
				ID: name + "-" + verb, Label: actionLabel, Method: "POST",
				Path:        "/v1/admin/providers/" + name + "/" + verb,
				NeedsReason: true, NeedsAdmin: true, Enabled: isAdmin,
				Hint: "Admin only. Pausing stops new work on this provider.",
			},
		})
	}
	return out
}

func (o *Orchestrator) providerKind(name string) string {
	if o.payments != nil && name == o.payments.Name() {
		return "PAYMENT"
	}
	if o.vending != nil && name == o.vending.Name() {
		return "VENDING"
	}
	if o.remote != nil && name == o.remote.Name() {
		return "AMI"
	}
	if o.readings != nil && name == o.readings.Name() {
		return "AMI"
	}
	for _, np := range o.notifProviders {
		if name == np {
			return "NOTIFICATION"
		}
	}
	return ""
}

func cohortRows(list []meters.RemoteCohort, isAdmin bool) []CohortRow {
	out := make([]CohortRow, 0, len(list))
	for _, c := range list {
		state := "attested"
		if c.RevokedAt != nil {
			state = "revoked"
		} else if c.Authorised {
			state = "live"
		}
		summary := c.DiscoCode + " · " + c.NumberPrefix + "… · " + state
		if c.Manufacturer != "" {
			summary = c.DiscoCode + " · " + c.NumberPrefix + "… · " + c.Manufacturer + " · " + state
		}
		row := CohortRow{ID: c.ID, Summary: summary, Evidence: c.Evidence, Revoked: c.RevokedAt != nil}
		if isAdmin && c.RevokedAt == nil {
			row.Action = &ConsoleAction{
				ID: "revoke-cohort", Label: "Revoke", Method: "DELETE",
				Path:        "/v1/admin/remote-credit/cohorts/" + c.ID.String(),
				NeedsReason: true, NeedsAdmin: true, Enabled: true,
			}
		}
		out = append(out, row)
	}
	return out
}

func ledgerLines(posts []ledger.Transaction) []ScreenLine {
	out := make([]ScreenLine, 0, len(posts))
	for _, p := range posts {
		parts := make([]string, 0, len(p.Entries))
		for _, e := range p.Entries {
			parts = append(parts, fmt.Sprintf("%s %s %s", e.Direction, e.Account, notifications.FormatNairaMinor(e.Amount.Minor)))
		}
		label := p.Description
		if label == "" {
			label = p.Reference
		}
		out = append(out, ScreenLine{Label: label, Value: strings.Join(parts, " · ")})
	}
	return out
}

func notifLines(views []notifications.OperatorView) []ScreenLine {
	out := make([]ScreenLine, 0, len(views))
	for _, v := range views {
		label := fmt.Sprintf("%s · %s", v.Channel, v.TemplateID)
		val := string(v.Status)
		if v.Attempts > 0 {
			val += fmt.Sprintf(" · %d attempt", v.Attempts)
			if v.Attempts != 1 {
				val += "s"
			}
		}
		if v.LastErrorCode != "" {
			val += " · " + v.LastErrorCode
		}
		out = append(out, ScreenLine{Label: label, Value: val})
	}
	return out
}

func auditLines(evs []audit.Event) []ScreenLine {
	out := make([]ScreenLine, 0, len(evs))
	for _, e := range evs {
		val := string(e.ActorType)
		if reason, _ := e.Metadata["reason"].(string); reason != "" {
			val += " · " + reason
		}
		if !e.CreatedAt.IsZero() {
			val += " · " + whenLabel(e.CreatedAt)
		}
		out = append(out, ScreenLine{Label: e.Action, Value: val})
	}
	return out
}

func eventLines(d *Detail) []ScreenLine {
	out := make([]ScreenLine, 0, len(d.Events))
	for _, e := range d.Events {
		out = append(out, ScreenLine{Label: e.Type, Value: whenLabel(e.CreatedAt)})
	}
	return out
}

func historyLines(t *tx.Transaction) []ScreenLine {
	out := make([]ScreenLine, 0, len(t.History))
	for _, h := range t.History {
		label := fmt.Sprintf("%s %s → %s", h.Dimension, h.From, h.To)
		val := whenLabel(h.At)
		if h.Note != "" {
			val = h.Note + " · " + val
		}
		out = append(out, ScreenLine{Label: label, Value: val})
	}
	return out
}

func whenLabel(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.In(westAfrica).Format("2 Jan 2006, 15:04 MST")
}
