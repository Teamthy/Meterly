package certify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/teamthy/meterly/internal/auth"
)

type OIDCConfig struct {
	Issuer     string
	Audience   string
	JWKSURL    string
	RolesClaim string
	MFAValues  []string
	StaffToken string // never logged
	HTTPClient *http.Client
}

func (c OIDCConfig) skipReason() string {
	if c.Issuer == "" || c.Audience == "" {
		return "OIDC_ISSUER/OIDC_AUDIENCE not set"
	}
	return ""
}

func OIDC(ctx context.Context, cfg OIDCConfig) []Result {
	const prov = "oidc"
	if reason := cfg.skipReason(); reason != "" {
		return []Result{
			skipped(prov, "discovery", reason),
			skipped(prov, "jwks", reason),
			skipped(prov, "staff_mfa_amr", reason),
			skipped(prov, "roles_claim", reason),
			skipped(prov, "key_rotation", reason),
		}
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}

	var jwksURI string
	out := []Result{}
	out = append(out, timed(prov, "discovery", func() ([]string, error) {
		url := strings.TrimRight(cfg.Issuer, "/") + "/.well-known/openid-configuration"
		var disc struct {
			Issuer  string `json:"issuer"`
			JWKSURI string `json:"jwks_uri"`
		}
		if err := getJSON(ctx, hc, url, &disc); err != nil {
			return nil, err
		}
		if disc.Issuer != cfg.Issuer && disc.Issuer != strings.TrimRight(cfg.Issuer, "/") && disc.Issuer != cfg.Issuer+"/" {
			return nil, fmt.Errorf("discovery issuer mismatch")
		}
		if disc.JWKSURI == "" {
			return nil, fmt.Errorf("discovery missing jwks_uri")
		}
		jwksURI = disc.JWKSURI
		return []string{disc.JWKSURI}, nil
	}))

	if cfg.JWKSURL != "" {
		jwksURI = cfg.JWKSURL
	}
	out = append(out, timed(prov, "jwks", func() ([]string, error) {
		if jwksURI == "" {
			return nil, fmt.Errorf("no jwks_uri")
		}
		var set struct {
			Keys []struct {
				Kid string `json:"kid"`
				Kty string `json:"kty"`
			} `json:"keys"`
		}
		if err := getJSON(ctx, hc, jwksURI, &set); err != nil {
			return nil, err
		}
		if len(set.Keys) == 0 {
			return nil, fmt.Errorf("JWKS has no keys")
		}
		ids := make([]string, 0, len(set.Keys))
		for _, k := range set.Keys {
			if k.Kid != "" {
				ids = append(ids, k.Kid)
			}
		}
		return ids, nil
	}))

	if cfg.StaffToken == "" {
		out = append(out, skipped(prov, "staff_mfa_amr", "CERTIFY_OIDC_STAFF_TOKEN not set"))
		out = append(out, skipped(prov, "roles_claim", "CERTIFY_OIDC_STAFF_TOKEN not set"))
	} else {
		mfa := cfg.MFAValues
		if len(mfa) == 0 {
			mfa = []string{"mfa", "otp", "hwk", "swk"}
		}
		rolesClaim := cfg.RolesClaim
		if rolesClaim == "" {
			rolesClaim = "roles"
		}
		v, err := auth.NewOIDC(auth.OIDCConfig{
			Issuer: cfg.Issuer, Audience: cfg.Audience, JWKSURL: cfg.JWKSURL,
			RolesClaim: rolesClaim, MFAValues: mfa, HTTPClient: hc,
		})
		if err != nil {
			out = append(out, failed(prov, "staff_mfa_amr", err.Error()))
			out = append(out, failed(prov, "roles_claim", err.Error()))
		} else {
			out = append(out, timed(prov, "staff_mfa_amr", func() ([]string, error) {
				p, err := v.Verify(ctx, cfg.StaffToken)
				if err != nil {
					return nil, err
				}
				if !p.Has(auth.RoleOperator) && !p.Has(auth.RoleAdmin) {
					return nil, fmt.Errorf("staff token has no MFA-backed staff role")
				}
				return nil, nil
			}))
			out = append(out, timed(prov, "roles_claim", func() ([]string, error) {
				p, err := v.Verify(ctx, cfg.StaffToken)
				if err != nil {
					return nil, err
				}
				if len(p.Roles) == 0 {
					return nil, fmt.Errorf("roles claim produced no staff roles")
				}
				return nil, nil
			}))
		}
	}

	out = append(out, timed(prov, "key_rotation", func() ([]string, error) {
		uri := jwksURI
		if uri == "" {
			uri = cfg.JWKSURL
		}
		if uri == "" {
			return nil, fmt.Errorf("no jwks_uri")
		}
		var set struct {
			Keys []struct {
				Kid string `json:"kid"`
			} `json:"keys"`
		}
		if err := getJSON(ctx, hc, uri, &set); err != nil {
			return nil, err
		}
		if err := getJSON(ctx, hc, uri, &set); err != nil {
			return nil, err
		}
		if len(set.Keys) == 0 {
			return nil, fmt.Errorf("JWKS empty on refresh")
		}
		return nil, nil
	}))
	return out
}

func getJSON(ctx context.Context, hc *http.Client, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	res, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("GET HTTP %d", res.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(v)
}
