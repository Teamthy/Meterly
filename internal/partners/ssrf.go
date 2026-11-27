package partners

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// Resolver looks up host addresses. Tests inject a fake to prove DNS pinning.
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

type Guard struct {
	Resolver      Resolver
	AllowLoopback bool
	Timeout       time.Duration
	MaxBody       int64
	// DialContext, when set, is used by the pinned client (tests record the
	// address actually dialed).
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
}

type Target struct {
	URL  *url.URL
	IP   net.IP
	Host string
	Port string
}

func (g *Guard) lookup() Resolver {
	if g != nil && g.Resolver != nil {
		return g.Resolver
	}
	return net.DefaultResolver
}

func (g *Guard) timeout() time.Duration {
	if g != nil && g.Timeout > 0 {
		return g.Timeout
	}
	return WebhookTimeout
}

func (g *Guard) maxBody() int64 {
	if g != nil && g.MaxBody > 0 {
		return g.MaxBody
	}
	return int64(WebhookMaxBody)
}

func (g *Guard) allowLoopback() bool {
	return g != nil && g.AllowLoopback
}

var blockedMetadataHosts = map[string]struct{}{
	"metadata.google.internal": {},
	"metadata.google.com":      {},
	"metadata":                 {},
	"instance-data":            {},
}

func (g *Guard) Check(ctx context.Context, raw string) (*Target, error) {
	if len(raw) == 0 || len(raw) > MaxWebhookURLBytes {
		return nil, ErrInvalidInput
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Opaque != "" {
		return nil, ErrInvalidInput
	}
	if u.User != nil {
		return nil, ErrBlockedURL
	}
	u.Fragment = ""
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(strings.TrimSpace(u.Hostname()))
	port := u.Port()
	allowLB := g.allowLoopback()
	switch scheme {
	case "https":
		if port == "" {
			port = "443"
		}
	case "http":
		if !allowLB {
			return nil, ErrNotHTTPS
		}
		if port == "" {
			port = "80"
		}
	default:
		return nil, ErrNotHTTPS
	}
	if host == "" {
		return nil, ErrInvalidInput
	}
	if _, ok := blockedMetadataHosts[host]; ok {
		return nil, ErrBlockedURL
	}
	if host == "localhost" && !allowLB {
		return nil, ErrBlockedURL
	}

	ips, err := g.resolve(ctx, host)
	if err != nil {
		return nil, ErrBlockedURL
	}
	var chosen net.IP
	for _, ip := range ips {
		if blockedIP(ip) {
			if allowLB && ip.IsLoopback() && chosen == nil {
				chosen = ip
			}
			continue
		}
		if chosen == nil {
			chosen = ip
		}
	}
	if chosen == nil {
		return nil, ErrBlockedURL
	}
	if scheme == "http" && !chosen.IsLoopback() {
		return nil, ErrNotHTTPS
	}
	u.Host = net.JoinHostPort(host, port)
	return &Target{URL: u, IP: chosen, Host: host, Port: port}, nil
}

func (g *Guard) resolve(ctx context.Context, host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	addrs, err := g.lookup().LookupIPAddr(ctx, host)
	if err != nil || len(addrs) == 0 {
		return nil, ErrBlockedURL
	}
	out := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		if a.IP != nil {
			out = append(out, a.IP)
		}
	}
	if len(out) == 0 {
		return nil, ErrBlockedURL
	}
	return out, nil
}

func blockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip4 := ip.To4(); ip4 != nil {
		return blockedIPv4(ip4)
	}
	return blockedIPv6(ip)
}

func blockedIPv4(ip net.IP) bool {
	ip4 := ip.To4()
	if ip4 == nil {
		return true
	}
	if ip4.IsLoopback() || ip4.IsPrivate() || ip4.IsUnspecified() || ip4.IsMulticast() ||
		ip4.IsLinkLocalUnicast() || ip4.IsLinkLocalMulticast() {
		return true
	}
	if ip4[0] == 0 || ip4[0] >= 240 {
		return true
	}
	return false
}

func blockedIPv6(ip net.IP) bool {
	if ip4 := ip.To4(); ip4 != nil {
		return blockedIPv4(ip4)
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() {
		return true
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	addr = addr.Unmap()
	if addr.Is4() {
		return blockedIPv4(addr.AsSlice())
	}
	// NAT64 well-known prefix 64:ff9b::/96
	a := addr.As16()
	if a[0] == 0x00 && a[1] == 0x64 && a[2] == 0xff && a[3] == 0x9b && a[4] == 0 && a[5] == 0 {
		return blockedIPv4(a[12:16])
	}
	return false
}

func (g *Guard) Client(t *Target) *http.Client {
	timeout := g.timeout()
	dial := g.DialContext
	if dial == nil {
		d := &net.Dialer{Timeout: timeout, KeepAlive: 0}
		dial = d.DialContext
	}
	pinned := t.IP
	port := t.Port
	host := t.Host
	allowLB := g.allowLoopback()
	transport := &http.Transport{
		Proxy:               nil,
		DisableKeepAlives:   true,
		TLSHandshakeTimeout: timeout,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dial(ctx, network, net.JoinHostPort(pinned.String(), port))
		},
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			ServerName: host,
		},
	}
	if allowLB && pinned.IsLoopback() && t.URL.Scheme == "http" {
		transport.TLSClientConfig = nil
	}
	if allowLB && pinned.IsLoopback() && t.URL.Scheme == "https" {
		transport.TLSClientConfig.InsecureSkipVerify = true
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return ErrRedirect
		},
	}
}

func (g *Guard) Post(ctx context.Context, rawURL string, headers map[string]string, body []byte) (int, error) {
	t, err := g.Check(ctx, rawURL)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.URL.String(), strings.NewReader(string(body)))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Meterly-Webhooks/1.0")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := g.Client(t)
	res, err := client.Do(req)
	if err != nil {
		if errors.Is(err, ErrRedirect) {
			return 0, ErrRedirect
		}
		return 0, fmt.Errorf("webhook post: %w", err)
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, g.maxBody()+1))
	return res.StatusCode, nil
}
