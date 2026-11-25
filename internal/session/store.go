// Package session is an in-memory session store with idle and absolute
// expiry. Sessions are lost on restart; that is acceptable for Plan 1.
package session

import (
	"cmp"
	"crypto/rand"
	"encoding/base64"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jhyoong/RANK-proxy-webapp/internal/auth"
	"github.com/jhyoong/RANK-proxy-webapp/internal/clusters"
)

// Session is one logged-in user. It holds Rancher tokens inside Sites.
type Session struct {
	ID, CSRF            string
	Username, Principal string
	Sites               []auth.SiteResult
	Warnings            []string
	Created, LastSeen   time.Time
	// Clusters is the cached cluster list. The pointer is shared and the
	// Listing is never mutated; SetClusters replaces it.
	Clusters *clusters.Listing

	seq uint64 // creation order, breaks Created ties when evicting
}

// Store holds sessions in memory.
type Store struct {
	mu         sync.Mutex
	m          map[string]*Session
	ended      []Ended
	idle       time.Duration
	absolute   time.Duration
	maxPerUser int
	seq        uint64
	now        func() time.Time
}

// Ended is a session that left the store and whose tokens still need
// revoking. Reason is "expired", "evicted", "relogin" or "shutdown".
type Ended struct {
	Session *Session
	Reason  string
}

// UserKey normalises a typed username to the account it names, so that the
// forms Rancher's AD provider accepts for one account (jdoe, CORP\jdoe,
// jdoe@corp.example) map to one key: lowercase, trim, drop everything up to
// the last backslash and from the first "@". If that leaves nothing, the
// lowercased trimmed input is returned.
func UserKey(username string) string {
	base := strings.ToLower(strings.TrimSpace(username))
	k := base
	if i := strings.LastIndexByte(k, '\\'); i >= 0 {
		k = k[i+1:]
	}
	if i := strings.IndexByte(k, '@'); i >= 0 {
		k = k[:i]
	}
	k = strings.TrimSpace(k)
	if k == "" {
		return base
	}
	return k
}

// capKey is the identity the per-user session cap counts against: the AD
// principal when known (case-folded), else the normalised username.
func capKey(sess *Session) string {
	if sess.Principal != "" {
		return strings.ToLower(sess.Principal)
	}
	return UserKey(sess.Username)
}

// Key is the identity this session counts against for per-user limits:
// the AD principal when known (case-folded), else the normalised username.
func (sess *Session) Key() string { return capKey(sess) }

// endLocked moves the session to the ended queue. s.mu must be held.
func (s *Store) endLocked(id, reason string) {
	sess, ok := s.m[id]
	if !ok {
		return
	}
	delete(s.m, id)
	s.ended = append(s.ended, Ended{Session: sess, Reason: reason})
}

// NewStore returns an empty store. maxPerUser caps live sessions per user
// (see capKey); zero or less means no cap. now is injectable for tests.
func NewStore(idle, absolute time.Duration, maxPerUser int, now func() time.Time) *Store {
	return &Store{m: map[string]*Session{}, idle: idle, absolute: absolute, maxPerUser: maxPerUser, now: now}
}

func randomID() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func clone(s *Session) *Session {
	c := *s
	c.Sites = slices.Clone(s.Sites)
	c.Warnings = slices.Clone(s.Warnings)
	return &c
}

func (s *Store) expired(sess *Session, now time.Time) bool {
	return now.Sub(sess.LastSeen) > s.idle || now.Sub(sess.Created) > s.absolute
}

// Create stores sess with a new ID and CSRF token and returns a copy.
func (s *Store) Create(sess Session) *Session {
	now := s.now()
	sess.ID, sess.CSRF = randomID(), randomID()
	sess.Created, sess.LastSeen = now, now
	stored := clone(&sess)
	s.mu.Lock()
	s.seq++
	stored.seq = s.seq
	if s.maxPerUser > 0 {
		key := capKey(&sess)
		var live []*Session
		for id, o := range s.m {
			if capKey(o) != key {
				continue
			}
			if s.expired(o, now) {
				s.endLocked(id, "expired")
				continue
			}
			live = append(live, o)
		}
		slices.SortFunc(live, func(a, b *Session) int { return cmp.Compare(a.seq, b.seq) })
		for len(live) >= s.maxPerUser {
			s.endLocked(live[0].ID, "evicted")
			live = live[1:]
		}
	}
	s.m[sess.ID] = stored
	s.mu.Unlock()
	return clone(stored)
}

// Get returns a copy of the session and refreshes its idle timer. Unknown or
// expired sessions return false; expired ones are moved to the ended queue so
// Sweep revokes their tokens.
func (s *Store) Get(id string) (*Session, bool) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.m[id]
	if !ok {
		return nil, false
	}
	if s.expired(sess, now) {
		s.endLocked(id, "expired")
		return nil, false
	}
	sess.LastSeen = now
	return clone(sess), true
}

// SetClusters replaces the session's cached cluster list. It returns false if
// the session is gone or expired.
func (s *Store) SetClusters(id string, l *clusters.Listing) bool {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.m[id]
	if !ok {
		return false
	}
	if s.expired(sess, now) {
		s.endLocked(id, "expired")
		return false
	}
	sess.Clusters = l
	return true
}

// Delete removes the session and returns it so its tokens can be revoked.
func (s *Store) Delete(id string) (*Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.m[id]
	if !ok {
		return nil, false
	}
	delete(s.m, id)
	return sess, true
}

// End moves the session to the ended queue with reason. Unknown IDs are ignored.
func (s *Store) End(id, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.endLocked(id, reason)
}

// Sweep moves expired sessions to the ended queue, then empties the queue
// and returns it. Each ended session is returned exactly once.
func (s *Store) Sweep() []Ended {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, sess := range s.m {
		if s.expired(sess, now) {
			s.endLocked(id, "expired")
		}
	}
	out := s.ended
	s.ended = nil
	return out
}

// Drain removes every remaining session with reason "shutdown" and returns
// them together with whatever was already in the ended queue.
func (s *Store) Drain() []Ended {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.m {
		s.endLocked(id, "shutdown")
	}
	out := s.ended
	s.ended = nil
	return out
}

// Len returns the number of stored sessions, including expired ones not yet swept.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}
