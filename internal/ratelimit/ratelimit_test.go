package ratelimit

import (
	"testing"
	"time"
)

func TestBucket(t *testing.T) {
	now := time.Unix(0, 0)
	b := New(60, 3) // 1/s, burst 3
	b.Now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if ok, _ := b.Allow("u"); !ok {
			t.Fatalf("burst %d denied", i)
		}
	}
	ok, wait := b.Allow("u")
	if ok || wait <= 0 || wait > time.Second {
		t.Fatalf("over burst: ok=%v wait=%v", ok, wait)
	}
	if ok, _ := b.Allow("other"); !ok {
		t.Fatal("keys are independent")
	}
	now = now.Add(time.Second)
	if ok, _ := b.Allow("u"); !ok {
		t.Fatal("refilled after 1s")
	}
	now = now.Add(time.Hour) // idle buckets are collected
	b.Allow("x")
	if _, ok := b.m["u"]; ok {
		t.Fatal("idle bucket not collected")
	}
}
