package ratelimit

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func clock() (func() time.Time, *time.Time) {
	t := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return func() time.Time { return t }, &t
}

func TestReserve_LimitAndWindow(t *testing.T) {
	now, c := clock()
	l := New(3, 15*time.Minute, now)
	for i := 0; i < 3; i++ {
		if !l.Reserve("a") {
			t.Fatalf("attempt %d denied", i)
		}
	}
	if l.Reserve("a") {
		t.Fatal("4th allowed")
	}
	*c = c.Add(15 * time.Minute)
	if l.Reserve("a") {
		t.Fatal("allowed at exactly the window")
	}
	*c = c.Add(time.Nanosecond)
	if !l.Reserve("a") {
		t.Fatal("not allowed after window")
	}
}

func TestReserve_KeysIndependent(t *testing.T) {
	now, _ := clock()
	l := New(1, time.Minute, now)
	if !l.Reserve("a") || !l.Reserve("b") || l.Reserve("a") || l.Reserve("b") {
		t.Fatal("keys not independent")
	}
}

func TestRelease_GivesBackOne(t *testing.T) {
	now, _ := clock()
	l := New(2, time.Minute, now)
	l.Reserve("a")
	l.Reserve("a")
	l.Release("a")
	if !l.Reserve("a") {
		t.Fatal("release did not give back")
	}
	if l.Reserve("a") {
		t.Fatal("over limit")
	}
	for i := 0; i < 5; i++ {
		l.Release("a")
	}
	l.Release("never-seen")
	if !l.Reserve("a") || !l.Reserve("a") || l.Reserve("a") {
		t.Fatal("count went below zero")
	}
}

func TestReset(t *testing.T) {
	now, _ := clock()
	l := New(1, time.Minute, now)
	l.Reserve("a")
	l.Reset("a")
	if !l.Reserve("a") {
		t.Fatal("reset did not forget")
	}
}

func TestReserve_SweepsExpiredKeys(t *testing.T) {
	now, c := clock()
	l := New(3, time.Minute, now)
	l.Reserve("a")
	l.Reserve("b")
	l.Reserve("c")
	if l.Len() != 3 {
		t.Fatalf("len %d", l.Len())
	}
	*c = c.Add(2 * time.Minute)
	l.Reserve("new")
	if l.Len() != 1 {
		t.Fatalf("len %d after sweep", l.Len())
	}
}

func TestZeroLimitAllowsAll(t *testing.T) {
	now, _ := clock()
	l := New(0, time.Minute, now)
	for i := 0; i < 100; i++ {
		if !l.Reserve("a") {
			t.Fatal("denied")
		}
	}
	if l.Len() != 0 {
		t.Fatal("tracked keys with no limit")
	}
}

func TestReserve_Concurrent(t *testing.T) {
	now, _ := clock()
	l := New(3, time.Minute, now)
	var wg sync.WaitGroup
	var ok atomic.Int32
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l.Reserve("a") {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 3 {
		t.Fatalf("got %d trues", ok.Load())
	}
}

func TestReserve_WindowFromLastAttempt(t *testing.T) {
	now, c := clock()
	t0 := *c
	l := New(3, 15*time.Minute, now)
	l.Reserve("a")
	*c = t0.Add(14 * time.Minute)
	l.Reserve("a")
	l.Reserve("a")
	*c = t0.Add(15*time.Minute + time.Nanosecond)
	if l.Reserve("a") {
		t.Fatal("window must run from the last counted attempt")
	}
	*c = t0.Add(29*time.Minute + time.Nanosecond)
	if !l.Reserve("a") {
		t.Fatal("not allowed after quiet period")
	}
}

func TestReserve_RefusedDoesNotExtend(t *testing.T) {
	now, c := clock()
	t0 := *c
	l := New(3, 15*time.Minute, now)
	for i := 0; i < 3; i++ {
		l.Reserve("a")
	}
	*c = t0.Add(10 * time.Minute)
	if l.Reserve("a") {
		t.Fatal("4th allowed")
	}
	*c = t0.Add(15*time.Minute + time.Nanosecond)
	if !l.Reserve("a") {
		t.Fatal("refused call extended the window")
	}
}
