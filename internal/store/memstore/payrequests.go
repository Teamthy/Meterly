package memstore

import (
	"context"
	"sort"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/payrequests"
	"github.com/teamthy/meterly/internal/store"
)

type payReqRepo struct{ *repos }

func copyPayRequest(p *payrequests.PaymentRequest) *payrequests.PaymentRequest {
	cp := *p
	if p.NoteCiphertext != nil {
		cp.NoteCiphertext = append([]byte(nil), p.NoteCiphertext...)
	}
	return &cp
}

func copyPayer(p payrequests.Payer) payrequests.Payer {
	cp := p
	if p.EmailCiphertext != nil {
		cp.EmailCiphertext = append([]byte(nil), p.EmailCiphertext...)
	}
	if p.PayerNameCiphertext != nil {
		cp.PayerNameCiphertext = append([]byte(nil), p.PayerNameCiphertext...)
	}
	if p.StatusCodeCiphertext != nil {
		cp.StatusCodeCiphertext = append([]byte(nil), p.StatusCodeCiphertext...)
	}
	return cp
}

func (r payReqRepo) Create(_ context.Context, p *payrequests.PaymentRequest) error {
	defer r.lock()()
	if !r.s.userIDs[p.RequesterID] {
		return store.ErrNotFound
	}
	if _, ok := r.s.meters[p.MeterID]; !ok {
		return store.ErrNotFound
	}
	if _, exists := r.s.payReqs[p.ID]; exists {
		return store.ErrDuplicate
	}
	if _, exists := r.s.payReqByCodeHMAC[p.CodeHMAC]; exists {
		return store.ErrDuplicate
	}
	id := p.ID
	ch := p.CodeHMAC
	r.s.payReqs[id] = copyPayRequest(p)
	r.s.payReqByCodeHMAC[ch] = id
	r.onUndo(func() {
		delete(r.s.payReqs, id)
		delete(r.s.payReqByCodeHMAC, ch)
	})
	return nil
}

func (r payReqRepo) Get(_ context.Context, id uuid.UUID) (*payrequests.PaymentRequest, error) {
	defer r.lock()()
	p, ok := r.s.payReqs[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return copyPayRequest(p), nil
}

func (r payReqRepo) GetForUpdate(ctx context.Context, id uuid.UUID) (*payrequests.PaymentRequest, error) {
	return r.Get(ctx, id)
}

func (r payReqRepo) GetByCodeHMAC(_ context.Context, codeHMAC string) (*payrequests.PaymentRequest, error) {
	defer r.lock()()
	id, ok := r.s.payReqByCodeHMAC[codeHMAC]
	if !ok {
		return nil, store.ErrNotFound
	}
	p, ok := r.s.payReqs[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	if !payrequests.ConstantTimeEqualHMAC(p.CodeHMAC, codeHMAC) {
		return nil, store.ErrNotFound
	}
	return copyPayRequest(p), nil
}

func (r payReqRepo) GetByCodeHMACForUpdate(ctx context.Context, codeHMAC string) (*payrequests.PaymentRequest, error) {
	return r.GetByCodeHMAC(ctx, codeHMAC)
}

func (r payReqRepo) Update(_ context.Context, p *payrequests.PaymentRequest) error {
	defer r.lock()()
	prev, ok := r.s.payReqs[p.ID]
	if !ok {
		return store.ErrNotFound
	}
	prevCopy := copyPayRequest(prev)
	r.s.payReqs[p.ID] = copyPayRequest(p)
	r.onUndo(func() {
		r.s.payReqs[p.ID] = prevCopy
	})
	return nil
}

func (r payReqRepo) ListForRequester(_ context.Context, requesterID string) ([]*payrequests.PaymentRequest, error) {
	defer r.lock()()
	var out []*payrequests.PaymentRequest
	for _, p := range r.s.payReqs {
		if p.RequesterID == requesterID {
			out = append(out, copyPayRequest(p))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID.String() < out[j].ID.String()
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

func (r payReqRepo) CreatePayer(_ context.Context, payer payrequests.Payer) error {
	defer r.lock()()
	if _, ok := r.s.txns[payer.TransactionID]; !ok {
		return store.ErrNotFound
	}
	if _, ok := r.s.payReqs[payer.PaymentRequestID]; !ok {
		return store.ErrNotFound
	}
	if _, exists := r.s.payers[payer.TransactionID]; exists {
		return store.ErrDuplicate
	}
	if _, exists := r.s.payerByStatusHMAC[payer.StatusCodeHMAC]; exists {
		return store.ErrDuplicate
	}
	txnID := payer.TransactionID
	sh := payer.StatusCodeHMAC
	r.s.payers[txnID] = copyPayer(payer)
	r.s.payerByStatusHMAC[sh] = txnID
	r.onUndo(func() {
		delete(r.s.payers, txnID)
		delete(r.s.payerByStatusHMAC, sh)
	})
	return nil
}

func (r payReqRepo) GetPayer(_ context.Context, txnID uuid.UUID) (payrequests.Payer, error) {
	defer r.lock()()
	p, ok := r.s.payers[txnID]
	if !ok {
		return payrequests.Payer{}, store.ErrNotFound
	}
	return copyPayer(p), nil
}

func (r payReqRepo) GetPayerForUpdate(ctx context.Context, txnID uuid.UUID) (payrequests.Payer, error) {
	return r.GetPayer(ctx, txnID)
}

func (r payReqRepo) GetPayerByStatusHMAC(_ context.Context, statusHMAC string) (payrequests.Payer, error) {
	defer r.lock()()
	txnID, ok := r.s.payerByStatusHMAC[statusHMAC]
	if !ok {
		return payrequests.Payer{}, store.ErrNotFound
	}
	p, ok := r.s.payers[txnID]
	if !ok {
		return payrequests.Payer{}, store.ErrNotFound
	}
	if !payrequests.ConstantTimeEqualHMAC(p.StatusCodeHMAC, statusHMAC) {
		return payrequests.Payer{}, store.ErrNotFound
	}
	return copyPayer(p), nil
}

func (r payReqRepo) MarkPayerSlotReleased(_ context.Context, txnID uuid.UUID) error {
	defer r.lock()()
	prev, ok := r.s.payers[txnID]
	if !ok {
		return store.ErrNotFound
	}
	prevCopy := copyPayer(prev)
	next := copyPayer(prev)
	next.SlotReleased = true
	r.s.payers[txnID] = next
	r.onUndo(func() {
		r.s.payers[txnID] = prevCopy
	})
	return nil
}
