package memstore

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/store"
	"github.com/teamthy/meterly/internal/support"
)

type supportRepo struct{ *repos }

func wrapKey(threadID uuid.UUID, holder string) string {
	return threadID.String() + ":" + holder
}

func copyIdent(k *support.IdentityKey) support.IdentityKey {
	c := *k
	c.PublicKey = append([]byte(nil), k.PublicKey...)
	return c
}

func copyThread(t *support.Thread) *support.Thread {
	c := *t
	return &c
}

func copyMsg(m *support.Message) *support.Message {
	c := *m
	c.Ciphertext = append([]byte(nil), m.Ciphertext...)
	c.Nonce = append([]byte(nil), m.Nonce...)
	c.BodyEnc = append([]byte(nil), m.BodyEnc...)
	return &c
}

func copyWrap(w *support.WrappedKey) support.WrappedKey {
	c := *w
	c.WrappedKey = append([]byte(nil), w.WrappedKey...)
	c.Nonce = append([]byte(nil), w.Nonce...)
	return c
}

func (r supportRepo) UpsertIdentityKey(_ context.Context, k support.IdentityKey) error {
	defer r.lock()()
	prev, ok := r.s.supportKeys[k.UserID]
	var prevCopy *support.IdentityKey
	if ok {
		cp := copyIdent(prev)
		prevCopy = &cp
	}
	cp := copyIdent(&k)
	r.s.supportKeys[k.UserID] = &cp
	r.onUndo(func() {
		if prevCopy == nil {
			delete(r.s.supportKeys, k.UserID)
		} else {
			r.s.supportKeys[k.UserID] = prevCopy
		}
	})
	return nil
}

func (r supportRepo) GetIdentityKey(_ context.Context, userID string) (support.IdentityKey, error) {
	defer r.lock()()
	k, ok := r.s.supportKeys[userID]
	if !ok {
		return support.IdentityKey{}, store.ErrNotFound
	}
	return copyIdent(k), nil
}

func (r supportRepo) ListStaffKeys(_ context.Context) ([]support.IdentityKey, error) {
	defer r.lock()()
	var out []support.IdentityKey
	for _, k := range r.s.supportKeys {
		if k.Staff {
			out = append(out, copyIdent(k))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UserID < out[j].UserID })
	return out, nil
}

func (r supportRepo) CreateThread(_ context.Context, t *support.Thread) error {
	defer r.lock()()
	if _, ok := r.s.supportThreads[t.ID]; ok {
		return store.ErrDuplicate
	}
	cp := copyThread(t)
	r.s.supportThreads[t.ID] = cp
	r.onUndo(func() { delete(r.s.supportThreads, t.ID) })
	return nil
}

func (r supportRepo) GetThread(_ context.Context, id uuid.UUID) (*support.Thread, error) {
	defer r.lock()()
	t, ok := r.s.supportThreads[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return copyThread(t), nil
}

func (r supportRepo) GetThreadForUpdate(ctx context.Context, id uuid.UUID) (*support.Thread, error) {
	return r.GetThread(ctx, id)
}

func (r supportRepo) UpdateThread(_ context.Context, t *support.Thread) error {
	defer r.lock()()
	prev, ok := r.s.supportThreads[t.ID]
	if !ok {
		return store.ErrNotFound
	}
	old := copyThread(prev)
	r.s.supportThreads[t.ID] = copyThread(t)
	r.onUndo(func() { r.s.supportThreads[t.ID] = old })
	return nil
}

func (r supportRepo) ListThreadsForCustomer(_ context.Context, customerID string) ([]*support.Thread, error) {
	defer r.lock()()
	var out []*support.Thread
	for _, t := range r.s.supportThreads {
		if t.CustomerID == customerID {
			out = append(out, copyThread(t))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastMessageAt.After(out[j].LastMessageAt) })
	return out, nil
}

func (r supportRepo) ListThreadsForStaff(_ context.Context, status support.ThreadStatus, limit int) ([]*support.Thread, error) {
	defer r.lock()()
	if limit <= 0 {
		limit = support.DefaultListLimit
	}
	var out []*support.Thread
	for _, t := range r.s.supportThreads {
		if status == "" || t.Status == status {
			out = append(out, copyThread(t))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastMessageAt.After(out[j].LastMessageAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r supportRepo) AddMessage(_ context.Context, m *support.Message) error {
	defer r.lock()()
	if _, ok := r.s.supportMessages[m.ID]; ok {
		return store.ErrDuplicate
	}
	cp := copyMsg(m)
	r.s.supportMessages[m.ID] = cp
	r.onUndo(func() { delete(r.s.supportMessages, m.ID) })
	return nil
}

func (r supportRepo) ListMessages(_ context.Context, threadID uuid.UUID, after time.Time, limit int) ([]*support.Message, error) {
	defer r.lock()()
	if limit <= 0 {
		limit = support.DefaultListLimit
	}
	var out []*support.Message
	for _, m := range r.s.supportMessages {
		if m.ThreadID != threadID {
			continue
		}
		if !after.IsZero() && !m.CreatedAt.After(after) {
			continue
		}
		out = append(out, copyMsg(m))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID.String() < out[j].ID.String()
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r supportRepo) UpsertWrappedKey(_ context.Context, w support.WrappedKey) error {
	defer r.lock()()
	key := wrapKey(w.ThreadID, w.HolderUserID)
	prev, ok := r.s.supportWraps[key]
	var prevCopy *support.WrappedKey
	if ok {
		cp := copyWrap(prev)
		prevCopy = &cp
	}
	cp := copyWrap(&w)
	r.s.supportWraps[key] = &cp
	r.onUndo(func() {
		if prevCopy == nil {
			delete(r.s.supportWraps, key)
		} else {
			r.s.supportWraps[key] = prevCopy
		}
	})
	return nil
}

func (r supportRepo) ListWrappedKeys(_ context.Context, threadID uuid.UUID) ([]support.WrappedKey, error) {
	defer r.lock()()
	var out []support.WrappedKey
	for _, w := range r.s.supportWraps {
		if w.ThreadID == threadID {
			out = append(out, copyWrap(w))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].HolderUserID < out[j].HolderUserID })
	return out, nil
}

func (r supportRepo) GetWrappedKey(_ context.Context, threadID uuid.UUID, holderUserID string) (support.WrappedKey, error) {
	defer r.lock()()
	w, ok := r.s.supportWraps[wrapKey(threadID, holderUserID)]
	if !ok {
		return support.WrappedKey{}, store.ErrNotFound
	}
	return copyWrap(w), nil
}
