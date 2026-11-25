package session

import (
	"sync"
	"testing"
	"time"

	"github.com/jhyoong/RANK-proxy-webapp/internal/clusters"
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
	s := NewStore(30*time.Minute, 8*time.Hour, 0, c.now)
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
	s := NewStore(time.Minute, time.Hour, 0, newClock().now)
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
	s := NewStore(30*time.Minute, 8*time.Hour, 0, c.now)
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
	s := NewStore(30*time.Minute, 8*time.Hour, 0, c.now)
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
	s := NewStore(30*time.Minute, 2*time.Hour, 0, c.now)
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
	s := NewStore(time.Minute, time.Hour, 0, newClock().now)
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
	s := NewStore(time.Minute, time.Hour, 0, c.now)
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
	s := NewStore(time.Minute, time.Hour, 0, time.Now)
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

func TestSetClusters(t *testing.T) {
	s := NewStore(time.Minute, time.Hour, 0, newClock().now)
	a := s.Create(Session{Username: "jdoe"})
	l := &clusters.Listing{Rows: []clusters.Row{{Site: "site-1"}}}
	if !s.SetClusters(a.ID, l) {
		t.Fatal("SetClusters false for live session")
	}
	got, _ := s.Get(a.ID)
	if got.Clusters != l {
		t.Fatal("listing not visible on next Get")
	}
	if s.SetClusters("nope", l) {
		t.Fatal("SetClusters true for unknown id")
	}
}

func TestSweep_ReturnsExpiredWithReason(t *testing.T) {
	c := newClock()
	s := NewStore(time.Minute, time.Hour, 0, c.now)
	old := s.Create(Session{Username: "a"})
	c.add(2 * time.Minute)
	fresh := s.Create(Session{Username: "b"})
	got := s.Sweep()
	if len(got) != 1 || got[0].Session.ID != old.ID || got[0].Reason != "expired" {
		t.Fatalf("got %+v", got)
	}
	if _, ok := s.Get(fresh.ID); !ok {
		t.Fatal("fresh session gone")
	}
}

func TestSweep_ReturnsEndedOnce(t *testing.T) {
	c := newClock()
	s := NewStore(time.Minute, time.Hour, 0, c.now)
	a := s.Create(Session{Username: "a"})
	c.add(2 * time.Minute)
	if _, ok := s.Get(a.ID); ok {
		t.Fatal("expired session returned")
	}
	if got := s.Sweep(); len(got) != 1 || got[0].Session.ID != a.ID || got[0].Reason != "expired" {
		t.Fatalf("first sweep %+v", got)
	}
	if got := s.Sweep(); len(got) != 0 {
		t.Fatalf("second sweep %+v", got)
	}
}

func TestCreate_EvictsOldestPerUser(t *testing.T) {
	c := newClock()
	s := NewStore(time.Hour, 8*time.Hour, 2, c.now)
	other := s.Create(Session{Username: "other"})
	a := s.Create(Session{Username: "JDoe"})
	b := s.Create(Session{Username: "jdoe"})
	d := s.Create(Session{Username: " jdoe "})
	if _, ok := s.Get(a.ID); ok {
		t.Fatal("oldest not evicted")
	}
	for _, id := range []string{b.ID, d.ID, other.ID} {
		if _, ok := s.Get(id); !ok {
			t.Fatalf("session %s missing", id)
		}
	}
	got := s.Sweep()
	if len(got) != 1 || got[0].Session.ID != a.ID || got[0].Reason != "evicted" {
		t.Fatalf("got %+v", got)
	}
}

func TestCreate_ExpiredDoNotCountTowardCap(t *testing.T) {
	c := newClock()
	s := NewStore(time.Minute, time.Hour, 1, c.now)
	a := s.Create(Session{Username: "jdoe"})
	c.add(2 * time.Minute)
	b := s.Create(Session{Username: "jdoe"})
	if _, ok := s.Get(b.ID); !ok {
		t.Fatal("new session missing")
	}
	got := s.Sweep()
	if len(got) != 1 || got[0].Session.ID != a.ID || got[0].Reason != "expired" {
		t.Fatalf("got %+v", got)
	}
}

func TestEnd_Relogin(t *testing.T) {
	s := NewStore(time.Minute, time.Hour, 0, newClock().now)
	a := s.Create(Session{Username: "a"})
	s.End("unknown", "relogin")
	s.End(a.ID, "relogin")
	if _, ok := s.Get(a.ID); ok {
		t.Fatal("still present")
	}
	got := s.Sweep()
	if len(got) != 1 || got[0].Session.ID != a.ID || got[0].Reason != "relogin" {
		t.Fatalf("got %+v", got)
	}
}

func TestDrain_AllShutdown(t *testing.T) {
	c := newClock()
	s := NewStore(time.Minute, time.Hour, 0, c.now)
	a := s.Create(Session{Username: "a"})
	b := s.Create(Session{Username: "b"})
	s.End(a.ID, "relogin")
	got := s.Drain()
	reasons := map[string]string{}
	for _, e := range got {
		reasons[e.Session.ID] = e.Reason
	}
	if len(got) != 2 || reasons[a.ID] != "relogin" || reasons[b.ID] != "shutdown" {
		t.Fatalf("got %+v", got)
	}
	if s.Len() != 0 || len(s.Drain()) != 0 {
		t.Fatal("not empty")
	}
}

func TestUserKey(t *testing.T) {
	for in, want := range map[string]string{
		"jdoe":                   "jdoe",
		" JDoe ":                 "jdoe",
		`CORP\jdoe`:              "jdoe",
		"jdoe@corp.example":      "jdoe",
		`\jdoe`:                  "jdoe",
		"jdoe@":                  "jdoe",
		`a\b\c`:                  "c",
		"@x":                     "@x",
		`corp\`:                  `corp\`,
		`CORP\JDoe@Corp.Example`: "jdoe",
		`CORP\ jdoe`:             "jdoe",
		"jdoe @corp.example":     "jdoe",
		` CORP\ JDoe @x `:        "jdoe",
	} {
		if got := UserKey(in); got != want {
			t.Errorf("UserKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCreate_CapByPrincipal(t *testing.T) {
	c := newClock()
	s := NewStore(time.Hour, 8*time.Hour, 1, c.now)
	a := s.Create(Session{Username: "jdoe", Principal: "activedirectory_user://CN=JDoe"})
	b := s.Create(Session{Username: "someone-else", Principal: "activedirectory_user://cn=jdoe"})
	if _, ok := s.Get(a.ID); ok {
		t.Fatal("same principal did not share the cap")
	}
	if _, ok := s.Get(b.ID); !ok {
		t.Fatal("new session missing")
	}
	// No principal: fall back to the normalised username.
	d := s.Create(Session{Username: "mary"})
	e := s.Create(Session{Username: `CORP\Mary`})
	if _, ok := s.Get(d.ID); ok {
		t.Fatal("domain form of username did not share the cap")
	}
	if _, ok := s.Get(e.ID); !ok {
		t.Fatal("new session missing")
	}
}

func TestSessionKey(t *testing.T) {
	if got := (&Session{Principal: "AD://X", Username: "jdoe"}).Key(); got != "ad://x" {
		t.Errorf("principal: %q", got)
	}
	if got := (&Session{Username: `CORP\JDoe`}).Key(); got != "jdoe" {
		t.Errorf("username: %q", got)
	}
}
