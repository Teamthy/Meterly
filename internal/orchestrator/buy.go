package orchestrator

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/meters"
	"github.com/teamthy/meterly/internal/money"
	"github.com/teamthy/meterly/internal/notifications"
	"github.com/teamthy/meterly/internal/store"
	"github.com/teamthy/meterly/internal/tariffs"
	tx "github.com/teamthy/meterly/internal/transactions"
)

const recommendHorizonHours = 48.0

type QuoteSpec struct {
	CustomerID          string
	MeterID             uuid.UUID
	AmountMinor         int64
	PackID              string
	Pref                DeliveryPreference
	ConfirmCustomerName bool
	RequireNameConfirm  bool
	// PartnerID, when set, marks a partner-API quote. SkipAuthorize lets
	// partners buy for any validated meter (no consumer grant).
	PartnerID     uuid.UUID
	SkipAuthorize bool
}

type ScreenLine struct {
	Label    string `json:"label"`
	Value    string `json:"value"`
	Emphasis bool   `json:"emphasis,omitempty"`
}

type Screen struct {
	Title      string       `json:"title"`
	Subtitle   string       `json:"subtitle"`
	Lines      []ScreenLine `json:"lines"`
	CtaLabel   string       `json:"cta_label"`
	TotalMinor int64        `json:"total_minor"`
	Currency   string       `json:"currency"`
}

type DiscoView struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

type TariffView struct {
	Band        string  `json:"band"`
	KoboPerKWh  int64   `json:"kobo_per_kwh"`
	NairaPerKWh float64 `json:"naira_per_kwh"`
	Label       string  `json:"label"`
}

type PackView struct {
	ID                string  `json:"id"`
	CreditMinor       int64   `json:"credit_minor"`
	PayMinor          int64   `json:"pay_minor"`
	DiscountMinor     int64   `json:"discount_minor"`
	Label             string  `json:"label"`
	PayLabel          string  `json:"pay_label"`
	EstimatedUnitsKWh float64 `json:"estimated_units_kwh"`
	Recommended       bool    `json:"recommended"`
}

type CustomAmountView struct {
	MinAmountMinor int64  `json:"min_amount_minor"`
	MaxAmountMinor int64  `json:"max_amount_minor"`
	FeeMinor       int64  `json:"fee_minor"`
	Label          string `json:"label"`
}

type BuyOptions struct {
	MeterID            uuid.UUID           `json:"meter_id"`
	MaskedMeterNumber  string              `json:"masked_meter_number"`
	Disco              DiscoView           `json:"disco"`
	Tariff             TariffView          `json:"tariff"`
	Usage              MeterUsage          `json:"usage"`
	Packs              []PackView          `json:"packs"`
	RecommendedPackID  string              `json:"recommended_pack_id"`
	Custom             CustomAmountView    `json:"custom"`
	DeliveryPreference DeliveryPreference  `json:"delivery_preference"`
	DeliveryOptions    []tx.DeliveryMethod `json:"delivery_options"`
}

type ElectricityCatalog struct {
	Discos []DiscoView      `json:"discos"`
	Packs  []PackView       `json:"packs"`
	Custom CustomAmountView `json:"custom"`
}

func purchaseInFlight(t *tx.Transaction) bool {
	if t == nil || t.Settled() {
		return false
	}
	switch t.Payment {
	case tx.PayConfirmed, tx.PayRefundPending:
		return true
	}
	return false
}

func (o *Orchestrator) ensureNoInFlight(ctx context.Context, repos store.Repos, customerID string, meterID uuid.UUID) error {
	if repos == nil {
		repos = o.st.Repos()
	}
	list, err := repos.Transactions().ListForCustomer(ctx, customerID)
	if err != nil {
		return err
	}
	for _, t := range list {
		if t.MeterID == meterID && purchaseInFlight(t) {
			return ErrInFlight
		}
	}
	return nil
}

func (o *Orchestrator) Catalog() ElectricityCatalog {
	kobo := tariffs.KoboPerKWh("IKEDC", tariffs.DefaultBand)
	packs := make([]PackView, 0, len(tariffs.Packs()))
	for _, p := range tariffs.Packs() {
		packs = append(packs, packView(p, kobo, p.ID == tariffs.DefaultPackID))
	}
	discos := make([]DiscoView, 0, len(tariffs.Discos()))
	for _, d := range tariffs.Discos() {
		discos = append(discos, DiscoView{Code: d.Code, Name: d.Name})
	}
	return ElectricityCatalog{
		Discos: discos,
		Packs:  packs,
		Custom: CustomAmountView{
			MinAmountMinor: o.cfg.MinAmountMinor,
			MaxAmountMinor: o.cfg.MaxAmountMinor,
			FeeMinor:       o.cfg.FeeMinor,
			Label:          "Enter amount",
		},
	}
}

func (o *Orchestrator) BuyOptions(ctx context.Context, userID string, meterID uuid.UUID, band string) (BuyOptions, error) {
	m, err := o.Meters.Authorize(ctx, userID, meterID, meters.PermView)
	if err != nil {
		return BuyOptions{}, err
	}
	b := tariffs.NormalizeBand(band)
	kobo := tariffs.KoboPerKWh(m.DiscoCode, b)
	usage := o.usageForMeter(ctx, userID, m)
	needed := neededPayMinor(usage, kobo)
	rec := tariffs.Recommend(needed)
	packs := make([]PackView, 0, len(tariffs.Packs()))
	for _, p := range tariffs.Packs() {
		packs = append(packs, packView(p, kobo, p.ID == rec.ID))
	}
	opts := []tx.DeliveryMethod{tx.MethodToken}
	pref := PrefToken
	if o.remote != nil && m.Capabilities.RemoteCreditUsable() {
		opts = append([]tx.DeliveryMethod{tx.MethodRemote}, opts...)
		pref = PrefAuto
	}
	return BuyOptions{
		MeterID:           m.ID,
		MaskedMeterNumber: m.Masked,
		Disco:             DiscoView{Code: m.DiscoCode, Name: tariffs.DiscoName(m.DiscoCode)},
		Tariff:            tariffView(m.DiscoCode, b, kobo),
		Usage:             usage,
		Packs:             packs,
		RecommendedPackID: rec.ID,
		Custom: CustomAmountView{
			MinAmountMinor: o.cfg.MinAmountMinor,
			MaxAmountMinor: o.cfg.MaxAmountMinor,
			FeeMinor:       o.cfg.FeeMinor,
			Label:          "Enter amount",
		},
		DeliveryPreference: pref,
		DeliveryOptions:    opts,
	}, nil
}

func neededPayMinor(u MeterUsage, koboPerKWh int64) int64 {
	if koboPerKWh <= 0 {
		return 0
	}
	if u.RemainingKWh == nil || u.Consumption == nil || u.Consumption.KWhPerHour <= 0 {
		return 0
	}
	neededKWh := u.Consumption.KWhPerHour*recommendHorizonHours - *u.RemainingKWh
	if neededKWh <= 0 {
		return 0
	}
	return int64(math.Ceil(neededKWh * float64(koboPerKWh)))
}

func packView(p tariffs.Pack, kobo int64, recommended bool) PackView {
	return PackView{
		ID:                p.ID,
		CreditMinor:       p.CreditMinor,
		PayMinor:          p.PayMinor,
		DiscountMinor:     tariffs.DiscountMinor(p),
		Label:             notifications.FormatNairaMinor(p.CreditMinor),
		PayLabel:          "Pay " + notifications.FormatNairaMinor(p.PayMinor),
		EstimatedUnitsKWh: tariffs.UnitsKWh(p.PayMinor, kobo),
		Recommended:       recommended,
	}
}

func tariffView(disco string, band tariffs.Band, kobo int64) TariffView {
	naira := tariffs.NairaPerKWh(kobo)
	return TariffView{
		Band:        string(band),
		KoboPerKWh:  kobo,
		NairaPerKWh: naira,
		Label:       fmt.Sprintf("%s Band %s · ₦%.2f/kWh", disco, band, naira),
	}
}

func (o *Orchestrator) IssueQuote(ctx context.Context, spec QuoteSpec) (*tx.Quote, error) {
	var (
		amountMinor   = spec.AmountMinor
		feeMinor      = o.cfg.FeeMinor
		creditMinor   = spec.AmountMinor
		discountMinor int64
		packID        string
	)
	if spec.PackID != "" {
		p, ok := tariffs.PackByID(spec.PackID)
		if !ok {
			return nil, fmt.Errorf("%w %q", ErrInvalidPack, spec.PackID)
		}
		packID = p.ID
		amountMinor = p.PayMinor
		feeMinor = 0
		creditMinor = p.CreditMinor
		discountMinor = tariffs.DiscountMinor(p)
	} else if amountMinor < o.cfg.MinAmountMinor || amountMinor > o.cfg.MaxAmountMinor {
		return nil, fmt.Errorf("%w: %d–%d kobo", ErrAmountOutOfRange, o.cfg.MinAmountMinor, o.cfg.MaxAmountMinor)
	}
	if spec.SkipAuthorize {
		if err := o.ensurePurchasesOpen(ctx); err != nil {
			return nil, err
		}
	} else {
		if err := o.ensurePurchaseAllowed(ctx, spec.CustomerID, spec.MeterID, amountMinor); err != nil {
			return nil, err
		}
		if err := o.ensurePurchaserActive(ctx, o.st.Repos(), spec.CustomerID); err != nil {
			return nil, err
		}
	}
	var (
		m   *meters.Meter
		err error
	)
	if spec.SkipAuthorize {
		m, err = o.Meters.Get(ctx, spec.MeterID)
	} else {
		m, err = o.Meters.Authorize(ctx, spec.CustomerID, spec.MeterID, meters.PermPurchase)
	}
	if err != nil {
		return nil, err
	}
	number, err := o.Meters.Number(m)
	if err != nil {
		return nil, err
	}
	fresh, err := o.Meters.Validate(ctx, number, m.DiscoCode)
	if err != nil {
		return nil, err
	}
	m = fresh
	if m.Capabilities.CannotVend {
		return nil, meters.ErrCannotVend
	}
	if spec.RequireNameConfirm && strings.TrimSpace(m.CustomerName) != "" && !spec.ConfirmCustomerName {
		return nil, ErrNameNotConfirmed
	}
	if m.Capabilities.MinPurchaseMinor > 0 && amountMinor < m.Capabilities.MinPurchaseMinor {
		return nil, fmt.Errorf("%w: disco minimum %d kobo", ErrAmountOutOfRange, m.Capabilities.MinPurchaseMinor)
	}
	if err := o.ensureNoInFlight(ctx, o.st.Repos(), spec.CustomerID, spec.MeterID); err != nil {
		return nil, err
	}
	options := []tx.DeliveryMethod{tx.MethodToken}
	remoteOK := o.remote != nil && m.Capabilities.RemoteCreditUsable()
	if remoteOK {
		options = append([]tx.DeliveryMethod{tx.MethodRemote}, options...)
	}
	method := tx.MethodToken
	switch spec.Pref {
	case PrefRemote:
		if !remoteOK {
			return nil, ErrNotEligible
		}
		method = tx.MethodRemote
	case PrefAuto, "":
		if remoteOK {
			method = tx.MethodRemote
		}
	case PrefToken:
	default:
		return nil, fmt.Errorf("%w %q", ErrInvalidPreference, spec.Pref)
	}
	band := tariffs.DefaultBand
	kobo := tariffs.KoboPerKWh(m.DiscoCode, band)
	units := tariffs.UnitsKWh(amountMinor, kobo)
	amt := money.New(amountMinor, money.NGN)
	fee := money.New(feeMinor, money.NGN)
	total, _ := amt.Add(fee)
	now := o.Now()
	q := &tx.Quote{
		ID: uuid.New(), CustomerID: spec.CustomerID, PartnerID: spec.PartnerID, MeterID: spec.MeterID,
		Amount: amt, Fee: fee, Total: total,
		DeliveryMethod: method, DeliveryOptions: options, ExpiresAt: now.Add(o.cfg.QuoteTTL), CreatedAt: now,
		PackID: packID, DiscoCode: m.DiscoCode, TariffBand: string(band), KoboPerKWh: kobo,
		EstimatedUnitsMilli: int64(math.Round(units * 1000)), CreditMinor: creditMinor, DiscountMinor: discountMinor,
	}
	if err := o.st.Repos().Quotes().Create(ctx, q); err != nil {
		return nil, err
	}
	return q, nil
}

func QuoteConfirmation(q *tx.Quote, m *meters.Meter) Screen {
	disco := q.DiscoCode
	masked := ""
	if m != nil {
		if disco == "" {
			disco = m.DiscoCode
		}
		masked = m.Masked
	}
	band := q.TariffBand
	if band == "" {
		band = string(tariffs.DefaultBand)
	}
	kobo := q.KoboPerKWh
	if kobo <= 0 {
		kobo = tariffs.KoboPerKWh(disco, tariffs.NormalizeBand(band))
	}
	units := tariffs.UnitsKWh(q.Amount.Minor, kobo)
	if q.EstimatedUnitsMilli > 0 {
		units = float64(q.EstimatedUnitsMilli) / 1000
	}
	credit := q.CreditMinor
	if credit <= 0 {
		credit = q.Amount.Minor
	}
	lines := []ScreenLine{
		{Label: "Meter", Value: masked},
		{Label: "Distribution company", Value: tariffs.DiscoName(disco)},
	}
	if m != nil && strings.TrimSpace(m.CustomerName) != "" {
		lines = append(lines, ScreenLine{Label: "Account name", Value: m.CustomerName})
	}
	if m != nil && m.Capabilities.ArrearsMinor > 0 {
		lines = append(lines, ScreenLine{Label: "Arrears on this meter", Value: notifications.FormatNairaMinor(m.Capabilities.ArrearsMinor)})
	}
	lines = append(lines,
		ScreenLine{Label: "Tariff", Value: tariffView(disco, tariffs.NormalizeBand(band), kobo).Label},
		ScreenLine{Label: "Electricity", Value: notifications.FormatNairaMinor(credit)},
		ScreenLine{Label: "Units that will be served", Value: fmt.Sprintf("%.3f kWh", units)},
	)
	if q.DiscountMinor > 0 {
		lines = append(lines, ScreenLine{Label: "Promo", Value: notifications.FormatNairaMinor(q.DiscountMinor) + " off"})
	}
	if q.Fee.Minor > 0 {
		lines = append(lines, ScreenLine{Label: "Platform fee", Value: notifications.FormatNairaMinor(q.Fee.Minor)})
	} else {
		lines = append(lines, ScreenLine{Label: "Platform fee", Value: "Waived"})
	}
	lines = append(lines, ScreenLine{Label: "You pay", Value: notifications.FormatNairaMinor(q.Total.Minor), Emphasis: true})
	sub := tariffs.DiscoName(disco)
	if masked != "" {
		sub = sub + " · " + masked
	}
	return Screen{
		Title:      "Confirm electricity purchase",
		Subtitle:   sub,
		Lines:      lines,
		CtaLabel:   "Pay " + notifications.FormatNairaMinor(q.Total.Minor),
		TotalMinor: q.Total.Minor,
		Currency:   string(q.Total.Currency),
	}
}

func CheckoutScreen(t *tx.Transaction, q *tx.Quote, m *meters.Meter) Screen {
	var (
		disco, band, masked string
		kobo, unitsMilli    int64
	)
	if m != nil {
		disco = m.DiscoCode
		masked = m.Masked
	}
	if q != nil {
		if q.DiscoCode != "" {
			disco = q.DiscoCode
		}
		band = q.TariffBand
		kobo = q.KoboPerKWh
		unitsMilli = q.EstimatedUnitsMilli
	}
	if band == "" {
		band = string(tariffs.DefaultBand)
	}
	if kobo <= 0 {
		kobo = tariffs.KoboPerKWh(disco, tariffs.NormalizeBand(band))
	}
	units := tariffs.UnitsKWh(t.Amount.Minor, kobo)
	if unitsMilli > 0 {
		units = float64(unitsMilli) / 1000
	}
	method := "STS token"
	if t.DeliveryMethod == tx.MethodRemote {
		method = "Credit on the meter"
	}
	lines := []ScreenLine{
		{Label: "Meter", Value: masked},
		{Label: "Distribution company", Value: tariffs.DiscoName(disco)},
		{Label: "How credit arrives", Value: method},
		{Label: "Tariff", Value: tariffView(disco, tariffs.NormalizeBand(band), kobo).Label},
		{Label: "Units that will be served", Value: fmt.Sprintf("%.3f kWh", units)},
		{Label: "Electricity", Value: notifications.FormatNairaMinor(t.Amount.Minor)},
	}
	if t.Fee.Minor > 0 {
		lines = append(lines, ScreenLine{Label: "Platform fee", Value: notifications.FormatNairaMinor(t.Fee.Minor)})
	}
	lines = append(lines, ScreenLine{Label: "Exact amount due", Value: notifications.FormatNairaMinor(t.Total.Minor), Emphasis: true})
	title := "Checkout"
	if masked != "" {
		title = "Checkout · " + masked
	}
	return Screen{
		Title:      title,
		Subtitle:   "Pay the exact quote total. Meterly vends only after the payment provider confirms.",
		Lines:      lines,
		CtaLabel:   "Pay " + notifications.FormatNairaMinor(t.Total.Minor),
		TotalMinor: t.Total.Minor,
		Currency:   string(t.Total.Currency),
	}
}

func EstimatedUnitsKWh(q *tx.Quote) float64 {
	if q == nil {
		return 0
	}
	if q.EstimatedUnitsMilli > 0 {
		return float64(q.EstimatedUnitsMilli) / 1000
	}
	return tariffs.UnitsKWh(q.Amount.Minor, q.KoboPerKWh)
}
