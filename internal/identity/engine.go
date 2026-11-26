package identity

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/secure"
)

type UserDirectory interface {
	EnsureByExternalID(ctx context.Context, externalID string) (string, error)
	Profile(ctx context.Context, userID string) (Profile, error)
	SetDisplayName(ctx context.Context, userID, name string) error
	SetOnboarding(ctx context.Context, userID, step string, completedAt *time.Time) error
	ExternalID(ctx context.Context, userID string) (string, error)
}

type CredentialStore interface {
	PutChallenge(ctx context.Context, c Challenge) error
	TakeChallenge(ctx context.Context, challenge []byte, now time.Time) (Challenge, error)
	InsertCredential(ctx context.Context, c Credential) error
	GetCredentialByCredID(ctx context.Context, credID []byte) (Credential, error)
	ListCredentials(ctx context.Context, userID string) ([]Credential, error)
	UpdateCredentialUse(ctx context.Context, credID []byte, signCount uint32, at time.Time) error
	DeleteCredential(ctx context.Context, userID string, id uuid.UUID) error
	InsertSession(ctx context.Context, s Session) error
	GetSessionByHMAC(ctx context.Context, hmac string) (Session, error)
	RevokeSession(ctx context.Context, id uuid.UUID, at time.Time) error
}

type Engine struct {
	Users   UserDirectory
	Auth    CredentialStore
	Box     *secure.Box
	RPName  string
	Origins []string
	Dev     bool
	Now     func() time.Time
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now().UTC()
	}
	return time.Now().UTC()
}

func (e *Engine) originAllowed(origin string) bool {
	origin = strings.TrimSpace(origin)
	if origin == "" {
		return false
	}
	want := strings.TrimRight(origin, "/")
	for _, o := range e.Origins {
		if strings.EqualFold(strings.TrimRight(o, "/"), want) {
			return true
		}
	}
	if !e.Dev {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return host == "localhost" || host == "127.0.0.1" ||
		strings.HasSuffix(host, ".e2b.app") || strings.HasSuffix(host, ".vercel.app")
}

func rpIDFromOrigin(origin string) string {
	u, err := url.Parse(origin)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

type PublicKeyOptions struct {
	RP                     map[string]string `json:"rp,omitempty"`
	User                   map[string]any    `json:"user,omitempty"`
	Challenge              string            `json:"challenge"`
	PubKeyCredParams       []map[string]any  `json:"pubKeyCredParams"`
	Timeout                int               `json:"timeout"`
	Attestation            string            `json:"attestation,omitempty"`
	AuthenticatorSelection map[string]any    `json:"authenticatorSelection,omitempty"`
	UserVerification       string            `json:"userVerification,omitempty"`
	RPID                   string            `json:"rpId,omitempty"`
	ExcludeCredentials     []map[string]any  `json:"excludeCredentials,omitempty"`
	AllowCredentials       []map[string]any  `json:"allowCredentials,omitempty"`
}

func (e *Engine) BeginRegister(ctx context.Context, origin, displayName, existingUserID string) (PublicKeyOptions, string, error) {
	if e == nil || e.Users == nil || e.Auth == nil {
		return PublicKeyOptions{}, "", ErrNoPasskeys
	}
	if !e.originAllowed(origin) {
		return PublicKeyOptions{}, "", ErrOrigin
	}
	rpID := rpIDFromOrigin(origin)
	userID := existingUserID
	if userID == "" {
		ext := "passkey:" + uuid.NewString()
		id, err := e.Users.EnsureByExternalID(ctx, ext)
		if err != nil {
			return PublicKeyOptions{}, "", err
		}
		userID = id
		if displayName != "" {
			_ = e.Users.SetDisplayName(ctx, userID, displayName)
		}
	}
	name := displayName
	if name == "" {
		if p, err := e.Users.Profile(ctx, userID); err == nil && p.DisplayName != "" {
			name = p.DisplayName
		} else {
			name = "Meterly user"
		}
	}
	ch := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, ch); err != nil {
		return PublicKeyOptions{}, "", err
	}
	if err := e.Auth.PutChallenge(ctx, Challenge{
		ID: uuid.New(), Challenge: ch, Purpose: "register", UserID: userID, ExpiresAt: e.now().Add(ChallengeTTL),
	}); err != nil {
		return PublicKeyOptions{}, "", err
	}
	uid, err := uuid.Parse(userID)
	if err != nil {
		return PublicKeyOptions{}, "", err
	}
	opt := PublicKeyOptions{
		RP: map[string]string{"id": rpID, "name": e.rpName()},
		User: map[string]any{
			"id": b64u(uid[:]), "name": name, "displayName": name,
		},
		Challenge:        b64u(ch),
		PubKeyCredParams: []map[string]any{{"type": "public-key", "alg": coseAlgES256}},
		Timeout:          60000,
		Attestation:      "none",
		AuthenticatorSelection: map[string]any{
			"authenticatorAttachment": "platform",
			"residentKey":             "required",
			"requireResidentKey":      true,
			"userVerification":        "required",
		},
	}
	if creds, err := e.Auth.ListCredentials(ctx, userID); err == nil {
		for _, c := range creds {
			opt.ExcludeCredentials = append(opt.ExcludeCredentials, map[string]any{
				"type": "public-key", "id": b64u(c.CredentialID),
			})
		}
	}
	return opt, userID, nil
}

type AttestationResponse struct {
	ID                string `json:"id"`
	RawID             string `json:"rawId"`
	Type              string `json:"type"`
	AuthenticatorData string `json:"authenticatorData"`
	ClientDataJSON    string `json:"clientDataJSON"`
	AttestationObject string `json:"attestationObject"`
	PublicKey         string `json:"publicKey"`
}

func (e *Engine) FinishRegister(ctx context.Context, origin string, att AttestationResponse, name string) (string, Session, error) {
	if e == nil || e.Users == nil || e.Auth == nil || e.Box == nil {
		return "", Session{}, ErrNoPasskeys
	}
	if !e.originAllowed(origin) {
		return "", Session{}, ErrOrigin
	}
	rpID := rpIDFromOrigin(origin)
	cd, err := b64uDecode(att.ClientDataJSON)
	if err != nil {
		return "", Session{}, ErrBadAttestation
	}
	var parsed clientData
	if err := json.Unmarshal(cd, &parsed); err != nil {
		return "", Session{}, ErrBadAttestation
	}
	chBytes, err := b64uDecode(parsed.Challenge)
	if err != nil {
		return "", Session{}, ErrChallenge
	}
	ch, err := e.Auth.TakeChallenge(ctx, chBytes, e.now())
	if err != nil || ch.Purpose != "register" || ch.UserID == "" {
		return "", Session{}, ErrChallenge
	}
	if err := parseClientData(cd, "webauthn.create", origin, ch.Challenge); err != nil {
		return "", Session{}, err
	}
	credID, err := b64uDecode(att.RawID)
	if err != nil || len(credID) == 0 {
		credID, err = b64uDecode(att.ID)
	}
	if err != nil || len(credID) == 0 {
		return "", Session{}, ErrBadAttestation
	}
	var pub []byte
	var sc uint32
	var aaguid []byte
	if att.PublicKey != "" {
		if p, err := parseECPoint(att.PublicKey); err == nil {
			pub = p
		}
	}
	if authData, err := b64uDecode(att.AuthenticatorData); err == nil && len(authData) >= 37 {
		if subtleEqual(authData[:32], rpIDHash(rpID)) != 1 {
			return "", Session{}, ErrOrigin
		}
		sc = binary.BigEndian.Uint32(authData[33:37])
		if at, err := parseAttested(authData, rpID); err == nil {
			if len(pub) == 0 {
				pub = at.PublicKey
			}
			aaguid = at.AAGUID
			if len(at.CredID) > 0 {
				credID = at.CredID
			}
		}
	}
	if len(pub) != 64 {
		return "", Session{}, ErrBadAttestation
	}
	if name == "" {
		name = "This device"
	}
	cred := Credential{
		ID: uuid.New(), UserID: ch.UserID, CredentialID: credID, PublicKey: pub,
		SignCount: sc, AAGUID: aaguid, Name: name, CreatedAt: e.now(), LastUsedAt: e.now(),
	}
	if err := e.Auth.InsertCredential(ctx, cred); err != nil {
		return "", Session{}, err
	}
	if p, err := e.Users.Profile(ctx, ch.UserID); err == nil && (p.OnboardingStep == "" || p.OnboardingStep == StepWelcome) {
		_ = e.Users.SetOnboarding(ctx, ch.UserID, StepAccount, nil)
	}
	return e.issueSession(ctx, ch.UserID)
}

func (e *Engine) BeginLogin(ctx context.Context, origin string) (PublicKeyOptions, error) {
	if e == nil || e.Auth == nil {
		return PublicKeyOptions{}, ErrNoPasskeys
	}
	if !e.originAllowed(origin) {
		return PublicKeyOptions{}, ErrOrigin
	}
	rpID := rpIDFromOrigin(origin)
	ch := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, ch); err != nil {
		return PublicKeyOptions{}, err
	}
	if err := e.Auth.PutChallenge(ctx, Challenge{
		ID: uuid.New(), Challenge: ch, Purpose: "login", ExpiresAt: e.now().Add(ChallengeTTL),
	}); err != nil {
		return PublicKeyOptions{}, err
	}
	return PublicKeyOptions{
		Challenge:        b64u(ch),
		Timeout:          60000,
		UserVerification: "required",
		RPID:             rpID,
		PubKeyCredParams: []map[string]any{{"type": "public-key", "alg": coseAlgES256}},
	}, nil
}

type AssertionResponse struct {
	ID                string `json:"id"`
	RawID             string `json:"rawId"`
	AuthenticatorData string `json:"authenticatorData"`
	ClientDataJSON    string `json:"clientDataJSON"`
	Signature         string `json:"signature"`
}

func (e *Engine) FinishLogin(ctx context.Context, origin string, as AssertionResponse) (string, Session, error) {
	if e == nil || e.Auth == nil || e.Box == nil {
		return "", Session{}, ErrNoPasskeys
	}
	if !e.originAllowed(origin) {
		return "", Session{}, ErrOrigin
	}
	rpID := rpIDFromOrigin(origin)
	cd, err := b64uDecode(as.ClientDataJSON)
	if err != nil {
		return "", Session{}, ErrBadAssertion
	}
	var parsed clientData
	if err := json.Unmarshal(cd, &parsed); err != nil {
		return "", Session{}, ErrBadAssertion
	}
	chBytes, err := b64uDecode(parsed.Challenge)
	if err != nil {
		return "", Session{}, ErrChallenge
	}
	ch, err := e.Auth.TakeChallenge(ctx, chBytes, e.now())
	if err != nil || ch.Purpose != "login" {
		return "", Session{}, ErrChallenge
	}
	if err := parseClientData(cd, "webauthn.get", origin, ch.Challenge); err != nil {
		return "", Session{}, err
	}
	rawID, err := b64uDecode(as.RawID)
	if err != nil {
		rawID, err = b64uDecode(as.ID)
	}
	if err != nil {
		return "", Session{}, ErrUnknownCredential
	}
	cred, err := e.Auth.GetCredentialByCredID(ctx, rawID)
	if err != nil {
		return "", Session{}, ErrUnknownCredential
	}
	authData, err := b64uDecode(as.AuthenticatorData)
	if err != nil {
		return "", Session{}, ErrBadAssertion
	}
	sc, err := parseAssertionAuthData(authData, rpID)
	if err != nil {
		return "", Session{}, err
	}
	if sc > 0 && sc <= cred.SignCount {
		return "", Session{}, ErrClonedKey
	}
	sig, err := b64uDecode(as.Signature)
	if err != nil {
		return "", Session{}, ErrBadAssertion
	}
	if err := verifyES256(cred.PublicKey, authData, cd, sig); err != nil {
		return "", Session{}, err
	}
	_ = e.Auth.UpdateCredentialUse(ctx, cred.CredentialID, sc, e.now())
	return e.issueSession(ctx, cred.UserID)
}

func (e *Engine) issueSession(ctx context.Context, userID string) (string, Session, error) {
	raw := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		return "", Session{}, err
	}
	token := SessionPrefix + b64u(raw)
	sess := Session{
		ID: uuid.New(), UserID: userID, TokenHMAC: e.Box.Hash("auth-session:" + token),
		ExpiresAt: e.now().Add(SessionTTL), CreatedAt: e.now(),
	}
	if err := e.Auth.InsertSession(ctx, sess); err != nil {
		return "", Session{}, err
	}
	return token, sess, nil
}

func (e *Engine) LookupSession(ctx context.Context, token string) (Session, error) {
	if e == nil || e.Auth == nil || e.Box == nil || !strings.HasPrefix(token, SessionPrefix) {
		return Session{}, ErrSession
	}
	s, err := e.Auth.GetSessionByHMAC(ctx, e.Box.Hash("auth-session:"+token))
	if err != nil {
		return Session{}, ErrSession
	}
	if s.RevokedAt != nil || !s.ExpiresAt.After(e.now()) {
		return Session{}, ErrSession
	}
	return s, nil
}

func (e *Engine) Logout(ctx context.Context, token string) error {
	s, err := e.LookupSession(ctx, token)
	if err != nil {
		return err
	}
	return e.Auth.RevokeSession(ctx, s.ID, e.now())
}

func (e *Engine) AdvanceOnboarding(ctx context.Context, userID, to string) (Profile, error) {
	if e == nil || e.Users == nil {
		return Profile{}, ErrNoPasskeys
	}
	p, err := e.Users.Profile(ctx, userID)
	if err != nil {
		return Profile{}, err
	}
	if !CanAdvance(p.OnboardingStep, to) {
		return Profile{}, ErrStepOrder
	}
	var done *time.Time
	if to == StepDone {
		t := e.now()
		done = &t
	}
	if err := e.Users.SetOnboarding(ctx, userID, to, done); err != nil {
		return Profile{}, err
	}
	return e.Users.Profile(ctx, userID)
}

func (e *Engine) ListPasskeys(ctx context.Context, userID string) ([]CredentialView, error) {
	if e == nil || e.Auth == nil {
		return nil, ErrNoPasskeys
	}
	creds, err := e.Auth.ListCredentials(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]CredentialView, 0, len(creds))
	for _, c := range creds {
		out = append(out, CredentialView{
			ID: c.ID, Name: c.Name, CreatedAt: c.CreatedAt, LastUsedAt: c.LastUsedAt, SignCount: c.SignCount,
		})
	}
	return out, nil
}

func (e *Engine) DeletePasskey(ctx context.Context, userID string, id uuid.UUID) error {
	if e == nil || e.Auth == nil {
		return ErrNoPasskeys
	}
	return e.Auth.DeleteCredential(ctx, userID, id)
}

func (e *Engine) BeginStepUp(ctx context.Context, origin, userID string) (PublicKeyOptions, error) {
	if e == nil || e.Auth == nil {
		return PublicKeyOptions{}, ErrNoPasskeys
	}
	if userID == "" {
		return PublicKeyOptions{}, ErrSession
	}
	if !e.originAllowed(origin) {
		return PublicKeyOptions{}, ErrOrigin
	}
	rpID := rpIDFromOrigin(origin)
	ch := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, ch); err != nil {
		return PublicKeyOptions{}, err
	}
	if err := e.Auth.PutChallenge(ctx, Challenge{
		ID: uuid.New(), Challenge: ch, Purpose: "step-up", UserID: userID, ExpiresAt: e.now().Add(ChallengeTTL),
	}); err != nil {
		return PublicKeyOptions{}, err
	}
	opt := PublicKeyOptions{
		Challenge:        b64u(ch),
		Timeout:          60000,
		UserVerification: "required",
		RPID:             rpID,
		PubKeyCredParams: []map[string]any{{"type": "public-key", "alg": coseAlgES256}},
	}
	if creds, err := e.Auth.ListCredentials(ctx, userID); err == nil {
		for _, c := range creds {
			opt.AllowCredentials = append(opt.AllowCredentials, map[string]any{
				"type": "public-key", "id": b64u(c.CredentialID),
			})
		}
	}
	return opt, nil
}

func (e *Engine) FinishStepUp(ctx context.Context, origin, userID string, as AssertionResponse) error {
	if e == nil || e.Auth == nil {
		return ErrNoPasskeys
	}
	if !e.originAllowed(origin) {
		return ErrOrigin
	}
	rpID := rpIDFromOrigin(origin)
	cd, err := b64uDecode(as.ClientDataJSON)
	if err != nil {
		return ErrBadAssertion
	}
	var parsed clientData
	if err := json.Unmarshal(cd, &parsed); err != nil {
		return ErrBadAssertion
	}
	chBytes, err := b64uDecode(parsed.Challenge)
	if err != nil {
		return ErrChallenge
	}
	ch, err := e.Auth.TakeChallenge(ctx, chBytes, e.now())
	if err != nil || ch.Purpose != "step-up" || ch.UserID != userID {
		return ErrChallenge
	}
	if err := parseClientData(cd, "webauthn.get", origin, ch.Challenge); err != nil {
		return err
	}
	rawID, err := b64uDecode(as.RawID)
	if err != nil {
		rawID, err = b64uDecode(as.ID)
	}
	if err != nil {
		return ErrUnknownCredential
	}
	cred, err := e.Auth.GetCredentialByCredID(ctx, rawID)
	if err != nil || cred.UserID != userID {
		return ErrUnknownCredential
	}
	authData, err := b64uDecode(as.AuthenticatorData)
	if err != nil {
		return ErrBadAssertion
	}
	sc, err := parseAssertionAuthData(authData, rpID)
	if err != nil {
		return err
	}
	if sc > 0 && sc <= cred.SignCount {
		return ErrClonedKey
	}
	sig, err := b64uDecode(as.Signature)
	if err != nil {
		return ErrBadAssertion
	}
	if err := verifyES256(cred.PublicKey, authData, cd, sig); err != nil {
		return err
	}
	return e.Auth.UpdateCredentialUse(ctx, cred.CredentialID, sc, e.now())
}

func (e *Engine) rpName() string {
	if e != nil && e.RPName != "" {
		return e.RPName
	}
	return "Meterly"
}
