// Package session is an in-memory session store with idle and absolute
// expiry. Sessions are lost on restart; that is acceptable for Plan 1.
package session

import (
	"crypto/rand"
	"encoding/base64"
	"slices"
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
}

// Store holds sessions in memory.
type Store struct {
	mu       sync.Mutex
	m        map[string]*Session
	idle     time.Duration
	absolute time.Duration
	now      func() time.Time
}

// NewStore returns an empty store. now is injectable for tests.
func NewStore(idle, absolute time.Duration, now func() time.Time) *Store {
	return &Store{m: map[string]*Session{}, idle: idle, absolute: absolute, now: now}
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
	s.m[sess.ID] = stored
	s.mu.Unlock()
	return clone(stored)
}

// Get returns a copy of the session and refreshes its idle timer. Unknown or
// expired sessions return false; expired ones are deleted.
func (s *Store) Get(id string) (*Session, bool) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.m[id]
	if !ok {
		return nil, false
	}
	if s.expired(sess, now) {
		delete(s.m, id)
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
	if !ok || s.expired(sess, now) {
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

// Sweep deletes expired sessions.
func (s *Store) Sweep() {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, sess := range s.m {
		if s.expired(sess, now) {
			delete(s.m, id)
		}
	}
}

// Len returns the number of stored sessions, including expired ones not yet swept.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}
