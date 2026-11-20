package session

import (
	"sync"
	"testing"
	"time"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.t = c.t.Add(d) }
func newClock() *clock               { return &clock{t: time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)} }

func TestCreateGet(t *testing.T) {
	c := newClock()
	s := NewStore(30*time.Minute, 8*time.Hour, c.now)
	a := s.Create(Session{Username: "jdoe", Principal: "p"})
	b := s.Create(Session{Username: "x"})
	if len(a.ID) != 43 || len(a.CSRF) != 43 || a.ID == b.ID || a.CSRF == b.CSRF || a.ID == a.CSRF {
		t.Fatalf("bad ids: %q %q %q %q", a.ID, a.CSRF, b.ID, b.CSRF)
	}
	got, ok := s.Get(a.ID)
	if !ok || got.Username != "jdoe" || got.Principal != "p" || !got.Created.Equal(c.now()) {
		t.Fatalf("got %+v %v", got, ok)
	}
	if _, ok := s.Get("nope"); ok {
		t.Fatal("unknown id found")
	}
}

func TestGetReturnsCopy(t *testing.T) {
	s := NewStore(time.Minute, time.Hour, newClock().now)
	a := s.Create(Session{Username: "jdoe"})
	got, _ := s.Get(a.ID)
	got.Username = "mutated"
	again, _ := s.Get(a.ID)
	if again.Username != "jdoe" {
		t.Fatal("store value was mutated through Get")
	}
}

func TestIdleExpiry(t *testing.T) {
	c := newClock()
	s := NewStore(30*time.Minute, 8*time.Hour, c.now)
	a := s.Create(Session{})
	c.add(30*time.Minute + time.Second)
	if _, ok := s.Get(a.ID); ok {
		t.Fatal("expected idle expiry")
	}
	c.add(-time.Hour) // even if the clock goes back, it was deleted
	if _, ok := s.Get(a.ID); ok {
		t.Fatal("expired session should have been deleted")
	}
}

func TestIdleTouch(t *testing.T) {
	c := newClock()
	s := NewStore(30*time.Minute, 8*time.Hour, c.now)
	a := s.Create(Session{})
	c.add(29 * time.Minute)
	if _, ok := s.Get(a.ID); !ok {
		t.Fatal("expired too early")
	}
	c.add(29 * time.Minute)
	if _, ok := s.Get(a.ID); !ok {
		t.Fatal("Get did not refresh idle timer")
	}
}

func TestAbsoluteExpiry(t *testing.T) {
	c := newClock()
	s := NewStore(30*time.Minute, 2*time.Hour, c.now)
	a := s.Create(Session{})
	for range 120 {
		c.add(time.Minute)
		if _, ok := s.Get(a.ID); !ok {
			t.Fatal("expired before absolute timeout")
		}
	}
	c.add(time.Second)
	if _, ok := s.Get(a.ID); ok {
		t.Fatal("expected absolute expiry")
	}
}

func TestDeleteReturnsSession(t *testing.T) {
	s := NewStore(time.Minute, time.Hour, newClock().now)
	a := s.Create(Session{Username: "jdoe"})
	got, ok := s.Delete(a.ID)
	if !ok || got.Username != "jdoe" {
		t.Fatalf("got %+v %v", got, ok)
	}
	if _, ok := s.Get(a.ID); ok {
		t.Fatal("still present")
	}
	if _, ok := s.Delete(a.ID); ok {
		t.Fatal("double delete returned ok")
	}
}

func TestSweep(t *testing.T) {
	c := newClock()
	s := NewStore(time.Minute, time.Hour, c.now)
	old := s.Create(Session{})
	c.add(2 * time.Minute)
	fresh := s.Create(Session{})
	s.Sweep()
	if s.Len() != 1 {
		t.Fatalf("len = %d", s.Len())
	}
	if _, ok := s.Get(fresh.ID); !ok {
		t.Fatal("fresh swept")
	}
	_ = old
}

func TestConcurrent(t *testing.T) {
	s := NewStore(time.Minute, time.Hour, time.Now)
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				a := s.Create(Session{})
				s.Get(a.ID)
				s.Sweep()
				s.Delete(a.ID)
			}
		}()
	}
	wg.Wait()
}
