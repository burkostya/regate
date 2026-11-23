package server

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	proxySessionLimit = 1024
	proxySessionBytes = 32 << 20
	proxySessionIdle  = 30 * time.Minute
)

type proxyOwner struct {
	issuer, audience, subject string
	inbound, outbound         [32]byte
	query                     [32]byte
}

type proxyBinding struct {
	id         string
	generation uint64
	stale      bool
	cursorKey  [32]byte
}

func (b proxyBinding) cursor(id string) string {
	generation := strconv.FormatUint(b.generation, 10)
	mac := hmac.New(sha256.New, b.cursorKey[:])
	_, _ = io.WriteString(mac, generation+":"+id)
	return generation + ":" + hex.EncodeToString(mac.Sum(nil)) + ":" + id
}

func (b proxyBinding) upstreamCursor(cursor string) string {
	generation, rest, ok := strings.Cut(cursor, ":")
	if !ok || generation != strconv.FormatUint(b.generation, 10) {
		return ""
	}
	_, id, ok := strings.Cut(rest, ":")
	if !ok || id == "" || !hmac.Equal([]byte(cursor), []byte(b.cursor(id))) {
		return ""
	}
	return id
}

type proxyRecovery struct {
	done    chan struct{}
	binding proxyBinding
	ok      bool
}

type proxySession struct {
	owner             proxyOwner
	initialize        []byte
	initializeVersion string
	// Only the session cache accesses lastUsed, under its lock.
	lastUsed time.Time

	mu       sync.Mutex
	binding  proxyBinding
	recovery *proxyRecovery
}

func (s *proxySession) current() proxyBinding {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.binding
}

func (s *proxySession) invalidate(binding proxyBinding) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.binding.generation == binding.generation {
		s.binding.stale = true
	}
}

func (s *proxySession) recover(ctx context.Context, observed proxyBinding, initialize func() (string, bool)) (proxyBinding, bool) {
	s.mu.Lock()
	if s.binding.generation != observed.generation && !s.binding.stale {
		binding := s.binding
		s.mu.Unlock()
		return binding, true
	}
	if pending := s.recovery; pending != nil {
		s.mu.Unlock()
		select {
		case <-pending.done:
			return pending.binding, pending.ok
		case <-ctx.Done():
			return proxyBinding{}, false
		}
	}
	s.binding.stale = true
	pending := &proxyRecovery{done: make(chan struct{})}
	s.recovery = pending
	s.mu.Unlock()

	id, ok := initialize()

	s.mu.Lock()
	defer s.mu.Unlock()
	if ok {
		s.binding.id = id
		s.binding.generation++
		s.binding.stale = false
	}
	pending.binding, pending.ok = s.binding, ok
	s.recovery = nil
	close(pending.done)
	return pending.binding, ok
}

type proxySessions struct {
	mu       sync.Mutex
	sessions map[string]*proxySession
	bytes    int
}

func (c *proxySessions) add(owner proxyOwner, upstreamID string, initialize []byte, version string) (string, proxyBinding, bool) {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, session := range c.sessions {
		if now.Sub(session.lastUsed) >= proxySessionIdle {
			delete(c.sessions, id)
			c.bytes -= len(session.initialize) + len(session.initializeVersion)
		}
	}
	size := len(initialize) + len(version)
	if len(c.sessions) >= proxySessionLimit || c.bytes+size > proxySessionBytes {
		return "", proxyBinding{}, false
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", proxyBinding{}, false
	}
	binding := proxyBinding{id: upstreamID, generation: 1}
	if _, err := rand.Read(binding.cursorKey[:]); err != nil {
		return "", proxyBinding{}, false
	}
	id := hex.EncodeToString(random[:])
	if c.sessions == nil {
		c.sessions = make(map[string]*proxySession)
	}
	c.sessions[id] = &proxySession{
		owner: owner, initialize: initialize, initializeVersion: version, lastUsed: now,
		binding: binding,
	}
	c.bytes += size
	return id, binding, true
}

func (c *proxySessions) find(id string, owner proxyOwner) *proxySession {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.sessions[id]
	if s == nil || s.owner != owner {
		return nil
	}
	now := time.Now()
	if now.Sub(s.lastUsed) >= proxySessionIdle {
		delete(c.sessions, id)
		c.bytes -= len(s.initialize) + len(s.initializeVersion)
		return nil
	}
	s.lastUsed = now
	return s
}

func (c *proxySessions) remove(id string, session *proxySession) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sessions[id] == session {
		delete(c.sessions, id)
		c.bytes -= len(session.initialize) + len(session.initializeVersion)
	}
}
