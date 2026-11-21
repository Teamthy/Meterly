// Package ratelimit is an in-memory token-bucket limiter keyed by string
// (user ID, client IP). It is per process: with several API replicas each
// enforces its own budget. Swap in a shared store (e.g. Redis) behind the
// Limiter interface when that matters.
package ratelimit

import (
	"math"
	"sync"
	"time"
)

type Limiter interface {
	// Allow takes one token for key. When it returns false, retryAfter is
	// how long until a token is available.
	Allow(key string) (ok bool, retryAfter time.Duration)
}

type bucket struct {
	tokens float64
	last   time.Time
}

type Buckets struct {
	rate  float64 // tokens per second
	burst float64
	idle  time.Duration
	Now   func() time.Time

	mu     sync.Mutex
	m      map[string]*bucket
	lastGC time.Time
}

// New allows perMinute requests per key on average, with bursts up to burst.
func New(perMinute, burst int) *Buckets {
	if burst < 1 {
		burst = 1
	}
	return &Buckets{rate: float64(perMinute) / 60, burst: float64(burst), idle: 10 * time.Minute,
		Now: time.Now, m: map[string]*bucket{}}
}

func (b *Buckets) Allow(key string) (bool, time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.Now()
	if now.Sub(b.lastGC) > b.idle {
		for k, v := range b.m {
			if now.Sub(v.last) > b.idle {
				delete(b.m, k)
			}
		}
		b.lastGC = now
	}
	bk, ok := b.m[key]
	if !ok {
		bk = &bucket{tokens: b.burst, last: now}
		b.m[key] = bk
	}
	bk.tokens = math.Min(b.burst, bk.tokens+now.Sub(bk.last).Seconds()*b.rate)
	bk.last = now
	if bk.tokens >= 1 {
		bk.tokens--
		return true, 0
	}
	if b.rate <= 0 {
		return false, time.Hour
	}
	wait := time.Duration((1 - bk.tokens) / b.rate * float64(time.Second))
	return false, wait
}
