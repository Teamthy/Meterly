// Package dataexport builds NDPA subject-access archives. The JSON never
// includes STS tokens, raw meter numbers, or another user's personal data.
package dataexport

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/identity"
	"github.com/teamthy/meterly/internal/meters"
	"github.com/teamthy/meterly/internal/notifications"
	"github.com/teamthy/meterly/internal/privacy"
	"github.com/teamthy/meterly/internal/receipts"
	"github.com/teamthy/meterly/internal/secure"
	"github.com/teamthy/meterly/internal/security"
	"github.com/teamthy/meterly/internal/store"
	"github.com/teamthy/meterly/internal/support"
)

// Archive is the JSON document handed to the data subject.
type Archive struct {
	GeneratedAt               time.Time                 `json:"generated_at"`
	Profile                   identity.Profile          `json:"profile"`
	Email                     string                    `json:"email,omitempty"`
	Phone                     string                    `json:"phone,omitempty"`
	Meters                    []MeterView               `json:"meters"`
	Transactions              []TxnView                 `json:"transactions"`
	Receipts                  []receipts.Receipt        `json:"receipts"`
	Consents                  privacy.Snapshot          `json:"consents"`
	NotificationPreferences   notifications.Preferences `json:"notification_preferences"`
	Households                []HouseholdView           `json:"households"`
	PaymentRequests           []PayRequestView          `json:"payment_requests"`
	SupportThreads            []ThreadView              `json:"support_threads"`
	SecurityEvents            []security.Event          `json:"security_events"`
	HumanMessagesEndToEndNote string                    `json:"human_messages_note,omitempty"`
}

type MeterView struct {
	MeterID           uuid.UUID `json:"meter_id"`
	MaskedMeterNumber string    `json:"masked_meter_number"`
	Disco             string    `json:"disco"`
	Label             string    `json:"label,omitempty"`
	Permission        string    `json:"permission,omitempty"`
	CustomerName      string    `json:"customer_name,omitempty"`
	Address           string    `json:"address,omitempty"`
}

type TxnView struct {
	TransactionID     uuid.UUID `json:"transaction_id"`
	MeterID           uuid.UUID `json:"meter_id"`
	MaskedMeterNumber string    `json:"masked_meter_number,omitempty"`
	AmountMinor       int64     `json:"amount_minor"`
	FeeMinor          int64     `json:"fee_minor"`
	TotalMinor        int64     `json:"total_minor"`
	Currency          string    `json:"currency"`
	PaymentStatus     string    `json:"payment_status"`
	VendingStatus     string    `json:"vending_status"`
	DeliveryStatus    string    `json:"delivery_status"`
	OverallStatus     string    `json:"overall_status"`
	IsGift            bool      `json:"is_gift,omitempty"`
	RecipientName     string    `json:"recipient_name,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
}

type HouseholdView struct {
	HouseholdID uuid.UUID   `json:"household_id"`
	Name        string      `json:"name"`
	Role        string      `json:"role"`
	Meters      []MeterView `json:"meters"`
}

type PayRequestView struct {
	ID                uuid.UUID `json:"id"`
	MeterID           uuid.UUID `json:"meter_id"`
	MaskedMeterNumber string    `json:"masked_meter_number,omitempty"`
	AmountMinor       int64     `json:"amount_minor,omitempty"`
	Status            string    `json:"status"`
	CreatedAt         time.Time `json:"created_at"`
}

type ThreadView struct {
	ID        uuid.UUID     `json:"id"`
	Mode      string        `json:"mode"`
	Status    string        `json:"status"`
	CreatedAt time.Time     `json:"created_at"`
	Messages  []MessageView `json:"messages"`
}

type MessageView struct {
	ID         uuid.UUID `json:"id"`
	SenderKind string    `json:"sender_kind"`
	Body       string    `json:"body,omitempty"`
	EndToEnd   bool      `json:"end_to_end,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

func Build(ctx context.Context, st store.Store, box *secure.Box, userID string, now time.Time) (Archive, error) {
	if st == nil {
		return Archive{}, errors.New("dataexport: store required")
	}
	now = now.UTC()
	rp := st.Repos()
	a := Archive{
		GeneratedAt:             now,
		Meters:                  []MeterView{},
		Transactions:            []TxnView{},
		Receipts:                []receipts.Receipt{},
		Households:              []HouseholdView{},
		PaymentRequests:         []PayRequestView{},
		SupportThreads:          []ThreadView{},
		SecurityEvents:          []security.Event{},
		NotificationPreferences: notifications.DefaultPreferences(),
	}

	if p, err := rp.Users().Profile(ctx, userID); err == nil {
		a.Profile = p
	} else if !errors.Is(err, store.ErrNotFound) {
		return Archive{}, err
	}
	if box != nil {
		if c, err := rp.Users().Contacts(ctx, userID); err == nil {
			if len(c.EmailCiphertext) > 0 {
				if email, err := notifications.OpenUserEmail(box, userID, c.EmailCiphertext); err == nil {
					a.Email = email
				}
			}
			if len(c.PhoneCiphertext) > 0 {
				if phone, err := notifications.OpenUserPhone(box, userID, c.PhoneCiphertext); err == nil {
					a.Phone = phone
				}
			}
		} else if !errors.Is(err, store.ErrNotFound) {
			return Archive{}, err
		}
	}

	listed, err := rp.Meters().ListForUser(ctx, userID)
	if err != nil {
		return Archive{}, err
	}
	metersByID := map[uuid.UUID]MeterView{}
	for _, m := range listed {
		v := MeterView{
			MeterID: m.ID, MaskedMeterNumber: m.Masked, Disco: m.DiscoCode,
			Label: m.Label, Permission: string(m.Permission),
			CustomerName: m.CustomerName, Address: m.Address,
		}
		a.Meters = append(a.Meters, v)
		metersByID[m.ID] = v
	}

	txns, err := rp.Transactions().ListForCustomer(ctx, userID)
	if err != nil {
		return Archive{}, err
	}
	for _, t := range txns {
		if t == nil {
			continue
		}
		tv := TxnView{
			TransactionID: t.ID, MeterID: t.MeterID,
			AmountMinor: t.Amount.Minor, FeeMinor: t.Fee.Minor, TotalMinor: t.Total.Minor,
			Currency: string(t.Total.Currency), PaymentStatus: string(t.Payment),
			VendingStatus: string(t.Vending), DeliveryStatus: string(t.Delivery),
			OverallStatus: string(t.Status()), IsGift: t.IsGift, RecipientName: t.RecipientName,
			CreatedAt: t.CreatedAt.UTC(),
		}
		m := (*meters.Meter)(nil)
		if mv, ok := metersByID[t.MeterID]; ok {
			tv.MaskedMeterNumber = mv.MaskedMeterNumber
		}
		if got, err := rp.Meters().Get(ctx, t.MeterID); err == nil {
			m = got
			if tv.MaskedMeterNumber == "" && m != nil {
				tv.MaskedMeterNumber = m.Masked
			}
		}
		if rec, err := receipts.Build(t, m); err == nil {
			a.Receipts = append(a.Receipts, rec)
		}
		a.Transactions = append(a.Transactions, tv)
	}

	prefs, err := rp.Notifications().GetPreferences(ctx, userID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return Archive{}, err
	}
	if err == nil {
		a.NotificationPreferences = prefs
	}
	hist, err := rp.Privacy().ListConsents(ctx, userID)
	if err != nil {
		return Archive{}, err
	}
	a.Consents = privacy.SnapshotFrom(userID, a.NotificationPreferences, hist)

	hhIDs, err := rp.Households().HouseholdIDsForUser(ctx, userID)
	if err != nil {
		return Archive{}, err
	}
	for _, hid := range hhIDs {
		h, err := rp.Households().Get(ctx, hid)
		if err != nil {
			continue
		}
		view := HouseholdView{HouseholdID: h.ID, Name: h.Name, Meters: []MeterView{}}
		if mem, err := rp.Households().Member(ctx, hid, userID); err == nil {
			view.Role = string(mem.Role)
		}
		links, err := rp.Households().ListMeters(ctx, hid)
		if err == nil {
			for _, link := range links {
				if mv, ok := metersByID[link.MeterID]; ok {
					view.Meters = append(view.Meters, MeterView{
						MeterID: mv.MeterID, MaskedMeterNumber: mv.MaskedMeterNumber, Disco: mv.Disco,
					})
					continue
				}
				if m, err := rp.Meters().Get(ctx, link.MeterID); err == nil && m != nil {
					view.Meters = append(view.Meters, MeterView{
						MeterID: m.ID, MaskedMeterNumber: m.Masked, Disco: m.DiscoCode,
					})
				}
			}
		}
		a.Households = append(a.Households, view)
	}

	reqs, err := rp.PayRequests().ListForRequester(ctx, userID)
	if err != nil {
		return Archive{}, err
	}
	for _, p := range reqs {
		if p == nil {
			continue
		}
		pv := PayRequestView{ID: p.ID, MeterID: p.MeterID, AmountMinor: p.AmountMinor, Status: string(p.Status), CreatedAt: p.CreatedAt.UTC()}
		if mv, ok := metersByID[p.MeterID]; ok {
			pv.MaskedMeterNumber = mv.MaskedMeterNumber
		} else if m, err := rp.Meters().Get(ctx, p.MeterID); err == nil && m != nil {
			pv.MaskedMeterNumber = m.Masked
		}
		a.PaymentRequests = append(a.PaymentRequests, pv)
	}

	threads, err := rp.Support().ListThreadsForCustomer(ctx, userID)
	if err != nil {
		return Archive{}, err
	}
	var sawE2E bool
	for _, th := range threads {
		if th == nil {
			continue
		}
		tv := ThreadView{ID: th.ID, Mode: string(th.Mode), Status: string(th.Status), CreatedAt: th.CreatedAt.UTC(), Messages: []MessageView{}}
		msgs, err := rp.Support().ListMessages(ctx, th.ID, time.Time{}, support.MaxListLimit)
		if err != nil {
			return Archive{}, err
		}
		for _, m := range msgs {
			if m == nil {
				continue
			}
			mv := MessageView{ID: m.ID, SenderKind: string(m.SenderKind), CreatedAt: m.CreatedAt.UTC()}
			if m.EndToEnd() {
				mv.EndToEnd = true
				sawE2E = true
			} else if box != nil && len(m.BodyEnc) > 0 {
				if body, err := box.DecryptBound(m.BodyEnc, []byte("support-msg:"+m.ThreadID.String()+":"+m.ID.String())); err == nil {
					mv.Body = body
				}
			}
			tv.Messages = append(tv.Messages, mv)
		}
		a.SupportThreads = append(a.SupportThreads, tv)
	}
	if sawE2E {
		a.HumanMessagesEndToEndNote = "Human-operator messages are end-to-end encrypted; the server never holds their plaintext. Ciphertext is omitted from this archive."
	}

	if ev, err := rp.Auth().ListSecurityEvents(ctx, userID, 50); err == nil {
		if ev != nil {
			a.SecurityEvents = ev
		}
	} else {
		return Archive{}, err
	}
	return a, nil
}

func Marshal(a Archive) ([]byte, error) {
	return json.Marshal(a)
}
