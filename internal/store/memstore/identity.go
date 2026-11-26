package memstore

import (
	"context"
	"encoding/hex"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/identity"
	"github.com/teamthy/meterly/internal/store"
)

func (r *repos) Profile(_ context.Context, userID string) (identity.Profile, error) {
	defer r.lock()()
	p, ok := r.s.userProfiles[userID]
	if !ok {
		if !r.s.userIDs[userID] {
			return identity.Profile{}, store.ErrNotFound
		}
		p = identity.Profile{ID: userID, ExternalID: r.s.userExt[userID], OnboardingStep: identity.StepWelcome}
	}
	if c, ok := r.s.userContacts[userID]; ok {
		p.DisplayName = c.DisplayName
		p.EmailMasked = c.EmailMasked
		p.PhoneMasked = c.PhoneMasked
	}
	p.ExternalID = r.s.userExt[userID]
	p.ID = userID
	return p, nil
}

func (r *repos) SetDisplayName(_ context.Context, userID, name string) error {
	defer r.lock()()
	if !r.s.userIDs[userID] {
		return store.ErrNotFound
	}
	p := r.s.userProfiles[userID]
	p.ID = userID
	p.DisplayName = name
	r.s.userProfiles[userID] = p
	c := r.s.userContacts[userID]
	c.UserID = userID
	c.DisplayName = name
	r.s.userContacts[userID] = c
	return nil
}

func (r *repos) SetOnboarding(_ context.Context, userID, step string, completedAt *time.Time) error {
	defer r.lock()()
	if !r.s.userIDs[userID] {
		return store.ErrNotFound
	}
	p := r.s.userProfiles[userID]
	p.ID = userID
	p.OnboardingStep = step
	p.OnboardingCompletedAt = completedAt
	r.s.userProfiles[userID] = p
	return nil
}

func (r *repos) ExternalID(_ context.Context, userID string) (string, error) {
	defer r.lock()()
	ext, ok := r.s.userExt[userID]
	if !ok {
		return "", store.ErrNotFound
	}
	return ext, nil
}

func (r *repos) PutChallenge(_ context.Context, c identity.Challenge) error {
	defer r.lock()()
	r.s.authChallenges[hex.EncodeToString(c.Challenge)] = c
	return nil
}

func (r *repos) TakeChallenge(_ context.Context, challenge []byte, now time.Time) (identity.Challenge, error) {
	defer r.lock()()
	k := hex.EncodeToString(challenge)
	c, ok := r.s.authChallenges[k]
	if !ok || !c.ExpiresAt.After(now) {
		return identity.Challenge{}, store.ErrNotFound
	}
	delete(r.s.authChallenges, k)
	return c, nil
}

func (r *repos) InsertCredential(_ context.Context, c identity.Credential) error {
	defer r.lock()()
	k := hex.EncodeToString(c.CredentialID)
	if _, ok := r.s.authCreds[k]; ok {
		return store.ErrDuplicate
	}
	r.s.authCreds[k] = c
	return nil
}

func (r *repos) GetCredentialByCredID(_ context.Context, credID []byte) (identity.Credential, error) {
	defer r.lock()()
	c, ok := r.s.authCreds[hex.EncodeToString(credID)]
	if !ok {
		return identity.Credential{}, store.ErrNotFound
	}
	return c, nil
}

func (r *repos) ListCredentials(_ context.Context, userID string) ([]identity.Credential, error) {
	defer r.lock()()
	out := []identity.Credential{}
	for _, c := range r.s.authCreds {
		if c.UserID == userID {
			out = append(out, c)
		}
	}
	return out, nil
}

func (r *repos) UpdateCredentialUse(_ context.Context, credID []byte, signCount uint32, at time.Time) error {
	defer r.lock()()
	k := hex.EncodeToString(credID)
	c, ok := r.s.authCreds[k]
	if !ok {
		return store.ErrNotFound
	}
	c.SignCount = signCount
	c.LastUsedAt = at
	r.s.authCreds[k] = c
	return nil
}

func (r *repos) DeleteCredential(_ context.Context, userID string, id uuid.UUID) error {
	defer r.lock()()
	for k, c := range r.s.authCreds {
		if c.ID == id && c.UserID == userID {
			delete(r.s.authCreds, k)
			return nil
		}
	}
	return store.ErrNotFound
}

func (r *repos) InsertSession(_ context.Context, s identity.Session) error {
	defer r.lock()()
	r.s.authSessions[s.TokenHMAC] = s
	return nil
}

func (r *repos) GetSessionByHMAC(_ context.Context, hmac string) (identity.Session, error) {
	defer r.lock()()
	s, ok := r.s.authSessions[hmac]
	if !ok {
		return identity.Session{}, store.ErrNotFound
	}
	return s, nil
}

func (r *repos) RevokeSession(_ context.Context, id uuid.UUID, at time.Time) error {
	defer r.lock()()
	for k, s := range r.s.authSessions {
		if s.ID == id {
			s.RevokedAt = &at
			r.s.authSessions[k] = s
			return nil
		}
	}
	return store.ErrNotFound
}
