package auth

import (
	"net/http"
	"strings"

	"github.com/teamthy/meterly/internal/identity"
)

// SessionThen authenticates Meterly passkey sessions (token prefix mls_)
// before falling through to Next (OIDC or DevAuth). A mls_ token is never
// treated as a DevAuth subject.
func SessionThen(next Authenticator, eng *identity.Engine) Authenticator {
	return chainAuth{Next: next, Engine: eng}
}

type chainAuth struct {
	Next   Authenticator
	Engine *identity.Engine
}

func (c chainAuth) Authenticate(r *http.Request) (Principal, error) {
	tok, ok := bearer(r)
	if ok && strings.HasPrefix(tok, identity.SessionPrefix) {
		if c.Engine == nil {
			return Principal{}, ErrUnauthenticated
		}
		sess, err := c.Engine.LookupSession(r.Context(), tok)
		if err != nil {
			return Principal{}, ErrUnauthenticated
		}
		ext, err := c.Engine.Users.ExternalID(r.Context(), sess.UserID)
		if err != nil || ext == "" {
			return Principal{}, ErrUnauthenticated
		}
		p := Principal{Subject: ext}
		if prof, err := c.Engine.Users.Profile(r.Context(), sess.UserID); err == nil {
			p.DisplayName = prof.DisplayName
		}
		return p, nil
	}
	if c.Next == nil {
		return Principal{}, ErrUnauthenticated
	}
	return c.Next.Authenticate(r)
}
