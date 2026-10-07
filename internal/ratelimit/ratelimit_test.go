package ratelimit

import (
	"fmt"
	"testing"
	"time"
)

func TestAllowWithinAndOverLimit(t *testing.T) {
	w := New(2, time.Minute)
	now := time.Unix(1000, 0)
	for i := 0; i < 2; i++ {
		if ok, _ := w.Allow("a", now.Add(time.Duration(i)*time.Second)); !ok {
			t.Fatalf("event %d must fit", i)
		}
	}
	ok, retry := w.Allow("a", now.Add(10*time.Second))
	if ok || retry != 50*time.Second {
		t.Fatalf("third event must be refused with retry 50s, got ok=%v retry=%v", ok, retry)
	}
	if ok, _ := w.Allow("b", now); !ok {
		t.Fatalf("other key must be independent")
	}
	if ok, _ := w.Allow("a", now.Add(61*time.Second)); !ok {
		t.Fatalf("after the window the key must be allowed again")
	}
}

func TestCheckDoesNotRecord(t *testing.T) {
	w := New(1, time.Minute)
	now := time.Unix(1000, 0)
	for i := 0; i < 3; i++ {
		if ok, _ := w.Check("a", now); !ok {
			t.Fatalf("check must not consume the budget")
		}
	}
	w.Record("a", now)
	if ok, retry := w.Check("a", now.Add(time.Second)); ok || retry != 59*time.Second {
		t.Fatalf("after Record the key must be full, got ok=%v retry=%v", ok, retry)
	}
}

func TestIdleKeysAreSwept(t *testing.T) {
	w := New(5, time.Second)
	now := time.Unix(1000, 0)
	for i := 0; i < 2*sweepEvery; i++ {
		w.Record(fmt.Sprintf("k%d", i), now.Add(time.Duration(i)*time.Millisecond*10))
	}
	if n := w.Keys(); n >= 2*sweepEvery {
		t.Fatalf("expired keys must be swept, still tracking %d", n)
	}
}

func TestRetrySeconds(t *testing.T) {
	for d, want := range map[time.Duration]int{0: 1, time.Millisecond: 1, time.Second: 1, 1500 * time.Millisecond: 2} {
		if got := RetrySeconds(d); got != want {
			t.Fatalf("RetrySeconds(%v)=%d want %d", d, got, want)
		}
	}
}
