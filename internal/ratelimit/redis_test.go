package ratelimit

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseRedisURL(t *testing.T) {
	c, err := parseRedisURL("redis://:s3cret@10.0.0.8:6381/2")
	if err != nil {
		t.Fatal(err)
	}
	if c.addr != "10.0.0.8:6381" || c.password != "s3cret" || c.db != 2 || c.useTLS {
		t.Fatalf("%+v", c)
	}
	c, err = parseRedisURL("rediss://cache.internal")
	if err != nil {
		t.Fatal(err)
	}
	if !c.useTLS || c.addr != "cache.internal:6380" {
		t.Fatalf("%+v", c)
	}
	if _, err := parseRedisURL(""); err == nil {
		t.Fatal("empty url")
	}
	if _, err := parseRedisURL("http://localhost:6379"); err == nil {
		t.Fatal("bad scheme")
	}
}

func TestRedisLimiterSharedAndBurst(t *testing.T) {
	ln := startFakeRedis(t, "")
	url := "redis://" + ln.Addr().String()
	a, err := Dial(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	a.failOpen = false
	now := time.Unix(1_700_000_000, 0)
	a.now = func() time.Time { return now }

	b, err := Dial(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	b.failOpen = false
	b.now = func() time.Time { return now }

	la, lb := a.Limiter("writes", 60, 2), b.Limiter("writes", 60, 2)
	if ok, _ := la.Allow("ada"); !ok {
		t.Fatal("first")
	}
	if ok, _ := lb.Allow("ada"); !ok {
		t.Fatal("second from other process")
	}
	ok, wait := la.Allow("ada")
	if ok || wait <= 0 {
		t.Fatalf("over shared burst: ok=%v wait=%v", ok, wait)
	}
	if ok, _ := lb.Allow("bola"); !ok {
		t.Fatal("other key")
	}

	now = now.Add(time.Second)
	if ok, _ := la.Allow("ada"); !ok {
		t.Fatal("refilled")
	}
}

func TestRedisFailOpen(t *testing.T) {
	ln := startFakeRedis(t, "")
	cl, err := Dial("redis://" + ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cl.Close)
	_ = ln.Close()
	cl.Close()
	cl.failOpen = true
	lim := cl.Limiter("writes", 1, 1)
	if ok, _ := lim.Allow("x"); !ok {
		t.Fatal("fail-open should allow when redis is down")
	}
}

func TestRedisAuth(t *testing.T) {
	ln := startFakeRedis(t, "hunter2")
	if _, err := Dial("redis://" + ln.Addr().String()); err == nil {
		t.Fatal("missing password must fail")
	}
	cl, err := Dial("redis://:hunter2@" + ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	cl.Close()
}

type fakeRedis struct {
	mu      sync.Mutex
	pass    string
	buckets map[string]*bucket
}

func startFakeRedis(t *testing.T, pass string) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	f := &fakeRedis{pass: pass, buckets: map[string]*bucket{}}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return ln
}

func (f *fakeRedis) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	authed := f.pass == ""
	for {
		args, err := readRESPArray(r)
		if err != nil {
			return
		}
		if len(args) == 0 {
			return
		}
		cmd := strings.ToUpper(args[0])
		if !authed && cmd != "AUTH" {
			_, _ = io.WriteString(c, "-NOAUTH Authentication required\r\n")
			continue
		}
		switch cmd {
		case "PING":
			_, _ = io.WriteString(c, "+PONG\r\n")
		case "AUTH":
			if len(args) < 2 || args[len(args)-1] != f.pass {
				_, _ = io.WriteString(c, "-ERR invalid password\r\n")
				continue
			}
			authed = true
			_, _ = io.WriteString(c, "+OK\r\n")
		case "SELECT":
			_, _ = io.WriteString(c, "+OK\r\n")
		case "EVAL":
			if len(args) < 8 {
				_, _ = io.WriteString(c, "-ERR wrong number of arguments\r\n")
				continue
			}
			key := args[3]
			rate, _ := strconv.ParseFloat(args[4], 64)
			burst, _ := strconv.ParseFloat(args[5], 64)
			nowMS, _ := strconv.ParseInt(args[6], 10, 64)
			allowed, retry := f.take(key, rate, burst, nowMS)
			a, rm := int64(0), retry
			if allowed {
				a = 1
				rm = 0
			}
			fmt.Fprintf(c, "*2\r\n:%d\r\n:%d\r\n", a, rm)
		default:
			_, _ = io.WriteString(c, "-ERR unknown command\r\n")
		}
	}
}

func (f *fakeRedis) take(key string, rate, burst float64, nowMS int64) (bool, int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.UnixMilli(nowMS)
	bk, ok := f.buckets[key]
	if !ok {
		bk = &bucket{tokens: burst, last: now}
		f.buckets[key] = bk
	}
	elapsed := now.Sub(bk.last).Seconds()
	if elapsed < 0 {
		elapsed = 0
	}
	bk.tokens += elapsed * rate
	if bk.tokens > burst {
		bk.tokens = burst
	}
	bk.last = now
	if bk.tokens >= 1 {
		bk.tokens--
		return true, 0
	}
	if rate <= 0 {
		return false, 3600000
	}
	return false, int64((1 - bk.tokens) / rate * 1000)
}

func readRESPArray(r *bufio.Reader) ([]string, error) {
	prefix, err := r.ReadByte()
	if err != nil {
		return nil, err
	}
	if prefix != '*' {
		return nil, fmt.Errorf("want array, got %q", prefix)
	}
	line, err := readLine(r)
	if err != nil {
		return nil, err
	}
	n, err := strconv.Atoi(line)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		v, err := readRESP(r)
		if err != nil {
			return nil, err
		}
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("arg %d not bulk: %T", i, v)
		}
		out = append(out, s)
	}
	return out, nil
}
