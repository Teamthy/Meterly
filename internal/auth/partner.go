package auth

import (
	"net/http"
	"strings"

	"github.com/teamthy/meterly/internal/partners"
)

// PartnerThen authenticates Meterly partner API keys (mtr_test_ / mtr_live_)
// before falling through to Next. A mtr_ token is never treated as a
// DevAuth subject or an OIDC JWT.
func PartnerThen(next Authenticator, eng *partners.Engine) Authenticator {
	if p, ok := next.(partnerAuth); ok {
		p.Engine = eng
		return p
	}
	return partnerAuth{Next: next, Engine: eng}
}

type partnerAuth struct {
	Next   Authenticator
	Engine *partners.Engine
}

func (c partnerAuth) Authenticate(r *http.Request) (Principal, error) {
	tok, ok := bearer(r)
	if ok && strings.HasPrefix(tok, "mtr_") {
		if c.Engine == nil {
			return Principal{}, ErrUnauthenticated
		}
		id, err := c.Engine.Authenticate(r.Context(), tok)
		if err != nil {
			return Principal{}, ErrUnauthenticated
		}
		return Principal{
			Subject:       "partner:" + id.Partner.ID.String(),
			PartnerID:     id.Partner.ID.String(),
			PartnerKeyID:  id.Key.ID.String(),
			PartnerEnv:    id.Key.Environment,
			PartnerPrefix: id.Key.Prefix,
			Scopes:        append([]string(nil), id.Key.Scopes...),
			DisplayName:   id.Partner.Organisation,
		}, nil
	}
	if c.Next == nil {
		return Principal{}, ErrUnauthenticated
	}
	return c.Next.Authenticate(r)
}
