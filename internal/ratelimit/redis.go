package ratelimit

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	redisKeyPrefix = "meterly:rl:"
	redisIdle      = 10 * time.Minute
	redisDialWait  = 2 * time.Second
	redisCmdWait   = 400 * time.Millisecond
)

// Token-bucket Lua. KEYS[1] is the only key (cluster-safe). ARGV: rate/sec,
// burst, now_ms, idle_ms. Returns {allowed (0|1), retry_ms}.
const bucketScript = `
local rate = tonumber(ARGV[1])
local burst = tonumber(ARGV[2])
local now = tonumber(ARGV[3])
local idle = tonumber(ARGV[4])
local data = redis.call('HMGET', KEYS[1], 'tokens', 'ts')
local tokens = tonumber(data[1])
local ts = tonumber(data[2])
if tokens == nil then
  tokens = burst
  ts = now
end
local elapsed = (now - ts) / 1000.0
if elapsed < 0 then elapsed = 0 end
tokens = math.min(burst, tokens + elapsed * rate)
local allowed = 0
local retry_ms = 0
if tokens >= 1.0 then
  tokens = tokens - 1.0
  allowed = 1
else
  if rate > 0 then
    retry_ms = math.ceil((1.0 - tokens) / rate * 1000.0)
  else
    retry_ms = 3600000
  end
end
redis.call('HSET', KEYS[1], 'tokens', tokens, 'ts', now)
redis.call('PEXPIRE', KEYS[1], idle)
return {allowed, retry_ms}
`

// Client is a tiny Redis RESP client used only for shared rate limits.
type Client struct {
	addr     string
	password string
	db       int
	useTLS   bool
	failOpen bool
	now      func() time.Time

	mu sync.Mutex
	c  net.Conn
	r  *bufio.Reader
}

// Dial connects and PINGs. rawURL is redis://[:password@]host:port[/db]
// or rediss:// for TLS. Empty URL is invalid; callers should skip Dial.
func Dial(rawURL string) (*Client, error) {
	cl, err := parseRedisURL(rawURL)
	if err != nil {
		return nil, err
	}
	cl.failOpen = true
	cl.now = time.Now
	if err := cl.hello(); err != nil {
		cl.Close()
		return nil, err
	}
	return cl, nil
}

func parseRedisURL(raw string) (*Client, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("empty redis url")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("redis url: %w", err)
	}
	useTLS := false
	switch strings.ToLower(u.Scheme) {
	case "redis":
	case "rediss":
		useTLS = true
	default:
		return nil, fmt.Errorf("redis url: unsupported scheme %q", u.Scheme)
	}
	host := u.Host
	if host == "" {
		return nil, errors.New("redis url: host required")
	}
	if u.Port() == "" {
		if useTLS {
			host = net.JoinHostPort(u.Hostname(), "6380")
		} else {
			host = net.JoinHostPort(u.Hostname(), "6379")
		}
	}
	pass := ""
	if u.User != nil {
		pass, _ = u.User.Password()
	}
	db := 0
	if p := strings.TrimPrefix(u.Path, "/"); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("redis url: invalid db %q", p)
		}
		db = n
	}
	return &Client{addr: host, password: pass, db: db, useTLS: useTLS}, nil
}

func (c *Client) Close() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropLocked()
}

func (c *Client) dropLocked() {
	if c.c != nil {
		_ = c.c.Close()
		c.c = nil
		c.r = nil
	}
}

// Limiter returns a named token-bucket that shares budget across processes
// using this Redis. name is the limiter class (writes, auth, …).
func (c *Client) Limiter(name string, perMinute, burst int) Limiter {
	if burst < 1 {
		burst = 1
	}
	if perMinute < 0 {
		perMinute = 0
	}
	if name == "" {
		name = "default"
	}
	return &redisLimiter{
		client: c,
		name:   name,
		rate:   float64(perMinute) / 60,
		burst:  float64(burst),
		idleMS: redisIdle.Milliseconds(),
	}
}

type redisLimiter struct {
	client *Client
	name   string
	rate   float64
	burst  float64
	idleMS int64
}

func (l *redisLimiter) Allow(key string) (bool, time.Duration) {
	if key == "" {
		key = "_"
	}
	now := l.client.now()
	if now.IsZero() {
		now = time.Now()
	}
	rk := redisKeyPrefix + l.name + ":" + key
	v, err := l.client.eval(bucketScript, rk,
		fmt.Sprintf("%g", l.rate),
		fmt.Sprintf("%g", l.burst),
		strconv.FormatInt(now.UnixMilli(), 10),
		strconv.FormatInt(l.idleMS, 10),
	)
	if err != nil {
		if l.client.failOpen {
			return true, 0
		}
		return false, time.Second
	}
	allowed, retryMS, err := parseEvalPair(v)
	if err != nil {
		if l.client.failOpen {
			return true, 0
		}
		return false, time.Second
	}
	if allowed {
		return true, 0
	}
	wait := time.Duration(retryMS) * time.Millisecond
	if wait < time.Millisecond {
		wait = time.Millisecond
	}
	return false, wait
}

func parseEvalPair(v any) (bool, int64, error) {
	arr, ok := v.([]any)
	if !ok || len(arr) < 2 {
		return false, 0, fmt.Errorf("redis eval: unexpected %T", v)
	}
	a, err := asInt(arr[0])
	if err != nil {
		return false, 0, err
	}
	r, err := asInt(arr[1])
	if err != nil {
		return false, 0, err
	}
	return a == 1, r, nil
}

func asInt(v any) (int64, error) {
	switch n := v.(type) {
	case int64:
		return n, nil
	case int:
		return int64(n), nil
	case string:
		return strconv.ParseInt(n, 10, 64)
	default:
		return 0, fmt.Errorf("not an int: %T", v)
	}
}

func (c *Client) hello() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureLocked(); err != nil {
		return err
	}
	v, err := c.roundTripLocked("PING")
	if err != nil {
		return err
	}
	if s, ok := v.(string); !ok || !strings.EqualFold(s, "PONG") {
		return fmt.Errorf("redis ping: %v", v)
	}
	return nil
}

func (c *Client) eval(script, key string, args ...string) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cmd := append([]string{"EVAL", script, "1", key}, args...)
	if err := c.ensureLocked(); err != nil {
		return nil, err
	}
	v, err := c.roundTripLocked(cmd...)
	if err != nil {
		c.dropLocked()
		if err2 := c.ensureLocked(); err2 != nil {
			return nil, err
		}
		return c.roundTripLocked(cmd...)
	}
	return v, nil
}

func (c *Client) ensureLocked() error {
	if c.c != nil {
		return nil
	}
	dialer := net.Dialer{Timeout: redisDialWait}
	var (
		conn net.Conn
		err  error
	)
	if c.useTLS {
		host, _, _ := net.SplitHostPort(c.addr)
		conn, err = tls.DialWithDialer(&dialer, "tcp", c.addr, &tls.Config{
			MinVersion: tls.VersionTLS12,
			ServerName: host,
		})
	} else {
		conn, err = dialer.Dial("tcp", c.addr)
	}
	if err != nil {
		return err
	}
	c.c = conn
	c.r = bufio.NewReader(conn)
	if c.password != "" {
		if _, err := c.roundTripLocked("AUTH", c.password); err != nil {
			c.dropLocked()
			return err
		}
	}
	if c.db != 0 {
		if _, err := c.roundTripLocked("SELECT", strconv.Itoa(c.db)); err != nil {
			c.dropLocked()
			return err
		}
	}
	return nil
}

func (c *Client) roundTripLocked(args ...string) (any, error) {
	_ = c.c.SetDeadline(time.Now().Add(redisCmdWait))
	if err := writeRESP(c.c, args...); err != nil {
		c.dropLocked()
		return nil, err
	}
	v, err := readRESP(c.r)
	if err != nil {
		c.dropLocked()
		return nil, err
	}
	return v, nil
}

func writeRESP(w io.Writer, args ...string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(a), a)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func readRESP(r *bufio.Reader) (any, error) {
	prefix, err := r.ReadByte()
	if err != nil {
		return nil, err
	}
	switch prefix {
	case '+':
		s, err := readLine(r)
		return s, err
	case '-':
		s, err := readLine(r)
		if err != nil {
			return nil, err
		}
		return nil, errors.New(s)
	case ':':
		s, err := readLine(r)
		if err != nil {
			return nil, err
		}
		n, err := strconv.ParseInt(s, 10, 64)
		return n, err
	case '$':
		s, err := readLine(r)
		if err != nil {
			return nil, err
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			return nil, err
		}
		if n < 0 {
			return nil, nil
		}
		buf := make([]byte, n+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		return string(buf[:n]), nil
	case '*':
		s, err := readLine(r)
		if err != nil {
			return nil, err
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			return nil, err
		}
		if n < 0 {
			return nil, nil
		}
		out := make([]any, n)
		for i := 0; i < n; i++ {
			v, err := readRESP(r)
			if err != nil {
				return nil, err
			}
			out[i] = v
		}
		return out, nil
	default:
		return nil, fmt.Errorf("redis: unknown prefix %q", prefix)
	}
}

func readLine(r *bufio.Reader) (string, error) {
	s, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(strings.TrimSuffix(s, "\n"), "\r"), nil
}
