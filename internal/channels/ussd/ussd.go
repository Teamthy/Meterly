// Package ussd adapts Africa's Talking USSD HTTP callbacks to
// conversation.USSDTurnInput and enforces aggregator authentication (IP/CIDR
// allowlist + HMAC-SHA256 shared secret) and the 182-character CON/END reply
// contract.
//
// Official Africa's Talking USSD callback specification:
//   - Request fields (form-urlencoded or JSON): sessionId, serviceCode,
//     phoneNumber, text (cumulative *-separated hops).
//   - Plain-text response starting with "CON " to continue the session or
//     "END " to close it (<= 182 characters).
package ussd

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"

	"github.com/teamthy/meterly/internal/conversation"
	"github.com/teamthy/meterly/internal/notifications"
	"github.com/teamthy/meterly/internal/providers"
)

var (
	ErrForbiddenIP    = errors.New("ussd: remote IP not in aggregator allowlist")
	ErrInvalidPayload = errors.New("ussd: malformed callback payload")
)

type Config struct {
	SharedSecret string
	AllowedCIDRs []string // IP addresses or CIDR prefixes (e.g. "127.0.0.1/32", "10.0.0.0/8")
}

type Adapter struct {
	secret   string
	prefixes []netip.Prefix
}

func New(cfg Config) (*Adapter, error) {
	secret := strings.TrimSpace(cfg.SharedSecret)
	if secret == "" {
		return nil, errors.New("ussd: shared secret is required")
	}
	prefixes, err := ParseAllowlist(cfg.AllowedCIDRs)
	if err != nil {
		return nil, err
	}
	if len(prefixes) == 0 {
		return nil, errors.New("ussd: at least one allowed IP or CIDR is required")
	}
	return &Adapter{secret: secret, prefixes: prefixes}, nil
}

// ParseAllowlist parses a slice of IP literals ("127.0.0.1") or CIDR prefixes ("10.0.0.0/8").
func ParseAllowlist(entries []string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, raw := range entries {
		for _, part := range strings.Split(raw, ",") {
			s := strings.TrimSpace(part)
			if s == "" {
				continue
			}
			if strings.Contains(s, "/") {
				p, err := netip.ParsePrefix(s)
				if err != nil {
					return nil, fmt.Errorf("ussd: invalid CIDR %q: %w", s, err)
				}
				out = append(out, p.Masked())
			} else {
				addr, err := netip.ParseAddr(s)
				if err != nil {
					return nil, fmt.Errorf("ussd: invalid IP %q: %w", s, err)
				}
				out = append(out, netip.PrefixFrom(addr.Unmap(), addr.Unmap().BitLen()))
			}
		}
	}
	return out, nil
}

// Sign computes the HMAC-SHA256 signature expected in X-USSD-Signature or
// X-AfricasTalking-Signature ("sha256=<hex>").
func Sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// ParseRequest authenticates the aggregator's source IP and HMAC signature,
// then parses Africa's Talking USSD parameters (sessionId, serviceCode,
// phoneNumber, text). Both IP failure and signature failure return errors
// mapped to HTTP 401 UNAUTHENTICATED.
func (a *Adapter) ParseRequest(remoteAddr string, h http.Header, body []byte) (conversation.USSDTurnInput, error) {
	if !a.ipAllowed(remoteAddr) {
		return conversation.USSDTurnInput{}, ErrForbiddenIP
	}
	sigHdr := strings.TrimSpace(h.Get("X-USSD-Signature"))
	if sigHdr == "" {
		sigHdr = strings.TrimSpace(h.Get("X-AfricasTalking-Signature"))
	}
	if !strings.HasPrefix(sigHdr, "sha256=") {
		return conversation.USSDTurnInput{}, providers.ErrBadSignature
	}
	got, err := hex.DecodeString(strings.TrimPrefix(sigHdr, "sha256="))
	if err != nil || len(got) == 0 {
		return conversation.USSDTurnInput{}, providers.ErrBadSignature
	}
	mac := hmac.New(sha256.New, []byte(a.secret))
	mac.Write(body)
	if !hmac.Equal(got, mac.Sum(nil)) {
		return conversation.USSDTurnInput{}, providers.ErrBadSignature
	}

	var sessionID, serviceCode, phoneNumber, text string
	ct := strings.ToLower(h.Get("Content-Type"))
	if strings.Contains(ct, "application/json") {
		var payload struct {
			SessionID   string `json:"sessionId"`
			ServiceCode string `json:"serviceCode"`
			PhoneNumber string `json:"phoneNumber"`
			Text        string `json:"text"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return conversation.USSDTurnInput{}, ErrInvalidPayload
		}
		sessionID = payload.SessionID
		serviceCode = payload.ServiceCode
		phoneNumber = payload.PhoneNumber
		text = payload.Text
	} else {
		vals, err := url.ParseQuery(string(body))
		if err != nil {
			return conversation.USSDTurnInput{}, ErrInvalidPayload
		}
		sessionID = vals.Get("sessionId")
		serviceCode = vals.Get("serviceCode")
		phoneNumber = vals.Get("phoneNumber")
		text = vals.Get("text")
	}

	sessionID = strings.TrimSpace(sessionID)
	serviceCode = strings.TrimSpace(serviceCode)
	rawPhone := strings.TrimSpace(phoneNumber)
	if rawPhone != "" && !strings.HasPrefix(rawPhone, "+") {
		rawPhone = "+" + rawPhone
	}
	normPhone, err := notifications.NormalizePhone(rawPhone)
	if err != nil || sessionID == "" {
		return conversation.USSDTurnInput{}, ErrInvalidPayload
	}
	return conversation.USSDTurnInput{
		SessionID:   sessionID,
		ServiceCode: serviceCode,
		PhoneNumber: normPhone,
		RawText:     text,
	}, nil
}

// WriteReply writes a text/plain CON/END USSD response bounded to <= 182 characters.
func WriteReply(w http.ResponseWriter, reply string) {
	out := strings.TrimSpace(reply)
	cont := strings.HasPrefix(out, "CON ")
	body := strings.TrimPrefix(strings.TrimPrefix(out, "CON "), "END ")
	formatted := conversation.FormatUSSDResponse(cont, body)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(formatted))
}

func (a *Adapter) ipAllowed(remoteAddr string) bool {
	host := strings.TrimSpace(remoteAddr)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	for _, p := range a.prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}
