package partners

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

type staticResolver struct {
	mu    sync.Mutex
	n     int
	first []net.IPAddr
	rest  []net.IPAddr
}

func (s *staticResolver) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	if s.n == 1 || len(s.rest) == 0 {
		return s.first, nil
	}
	return s.rest, nil
}

func TestSSRFMatrix(t *testing.T) {
	g := &Guard{}
	ctx := context.Background()
	deny := []string{
		"http://example.com/hooks",
		"https://127.0.0.1/hooks",
		"https://localhost/hooks",
		"https://10.0.0.1/hooks",
		"https://192.168.1.20/hooks",
		"https://172.16.5.1/hooks",
		"https://169.254.169.254/latest/meta-data",
		"https://[::1]/hooks",
		"https://[fc00::1]/hooks",
		"https://[fe80::1]/hooks",
		"https://[::ffff:127.0.0.1]/hooks",
		"https://[::ffff:10.0.0.1]/hooks",
		"https://[::ffff:169.254.169.254]/hooks",
		"https://user:pass@example.com/hooks",
		"https://metadata.google.internal/computeMetadata/v1",
		"ftp://8.8.8.8/hooks",
	}
	for _, u := range deny {
		if _, err := g.Check(ctx, u); err == nil {
			t.Errorf("expected deny %s", u)
		}
	}
	if _, err := g.Check(ctx, "https://8.8.8.8/hooks"); err != nil {
		t.Fatalf("public ipv4: %v", err)
	}
	g.AllowLoopback = true
	if _, err := g.Check(ctx, "http://127.0.0.1:9/hooks"); err != nil {
		t.Fatalf("dev loopback http: %v", err)
	}
	if _, err := g.Check(ctx, "http://8.8.8.8/hooks"); err == nil {
		t.Fatal("http to public must stay denied even in dev")
	}
}

func TestSSRFPinsIPAgainstDNSRebinding(t *testing.T) {
	public := net.ParseIP("8.8.8.8")
	loop := net.ParseIP("127.0.0.1")
	res := &staticResolver{
		first: []net.IPAddr{{IP: public}},
		rest:  []net.IPAddr{{IP: loop}},
	}
	var dialed string
	g := &Guard{
		Resolver: res,
		DialContext: func(_ context.Context, network, addr string) (net.Conn, error) {
			dialed = addr
			return nil, errors.New("stopped")
		},
	}
	ctx := context.Background()
	target, err := g.Check(ctx, "https://evil.example/hooks")
	if err != nil {
		t.Fatal(err)
	}
	if !target.IP.Equal(public) {
		t.Fatalf("pinned %s", target.IP)
	}
	client := g.Client(target)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://evil.example/hooks", nil)
	_, _ = client.Do(req)
	if dialed != net.JoinHostPort(public.String(), "443") {
		t.Fatalf("dialed %q, want pinned public IP (resolver lookups=%d)", dialed, res.n)
	}
	if res.n != 1 {
		t.Fatalf("lookup count %d: client must not resolve again", res.n)
	}
}

func TestSSRFDoesNotFollowRedirects(t *testing.T) {
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("redirect was followed")
		w.WriteHeader(200)
	}))
	defer final.Close()
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL, http.StatusFound)
	}))
	defer first.Close()
	g := &Guard{AllowLoopback: true}
	code, err := g.Post(context.Background(), first.URL+"/hooks", nil, []byte(`{}`))
	if !errors.Is(err, ErrRedirect) {
		t.Fatalf("code=%d err=%v", code, err)
	}
}

func TestSSRFCapsResponseBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = io.CopyN(w, zeroReader{}, 1<<20)
	}))
	defer srv.Close()
	g := &Guard{AllowLoopback: true, MaxBody: 1024}
	code, err := g.Post(context.Background(), srv.URL, nil, []byte(`{}`))
	if err != nil || code != 200 {
		t.Fatalf("code=%d err=%v", code, err)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}
