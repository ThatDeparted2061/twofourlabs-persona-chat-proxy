// Package persona stores character prompts and serves them as the stable
// prefix of every request.
//
// Clients send persona_id instead of re-uploading a 2-3k token character card
// every turn, and the proxy always renders the same bytes for it. That byte
// stability is what lets provider prompt caches hit: one changed character in
// the prefix invalidates everything after it.
package persona

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// ErrNotFound means no persona has that ID.
var ErrNotFound = errors.New("persona not found")

// Store keeps personas in Redis with an in-process read-through cache, so the
// hot path does not pay a Redis round trip per turn.
type Store struct {
	rdb    *redis.Client
	prefix string
	ttl    time.Duration // how long a local copy is trusted

	mu    sync.RWMutex
	local map[string]cached
}

type cached struct {
	p  Persona
	at time.Time
}

// Persona is a character card.
type Persona struct {
	ID     string `json:"id"`
	Name   string `json:"name,omitempty"`
	Prompt string `json:"prompt"`
	Hash   string `json:"hash"` // first 16 hex chars of SHA-256(prompt); changes when the prompt does
}

func New(rdb *redis.Client, prefix string) *Store {
	return &Store{rdb: rdb, prefix: prefix + "persona:", ttl: time.Minute, local: map[string]cached{}}
}

// Put creates or replaces a persona.
func (s *Store) Put(ctx context.Context, id, name, prompt string) (Persona, error) {
	sum := sha256.Sum256([]byte(prompt))
	p := Persona{ID: id, Name: name, Prompt: prompt, Hash: hex.EncodeToString(sum[:8])}
	if err := s.rdb.HSet(ctx, s.prefix+id, "name", name, "prompt", prompt, "hash", p.Hash).Err(); err != nil {
		return Persona{}, err
	}
	s.mu.Lock()
	s.local[id] = cached{p, time.Now()}
	s.mu.Unlock()
	return p, nil
}

// Get returns a persona, from memory when the local copy is fresh.
// ponytail: other proxy replicas see an updated prompt within ttl (1 minute).
// Add Redis pub/sub invalidation if edits must apply instantly everywhere.
func (s *Store) Get(ctx context.Context, id string) (Persona, error) {
	s.mu.RLock()
	c, ok := s.local[id]
	s.mu.RUnlock()
	if ok && time.Since(c.at) < s.ttl {
		return c.p, nil
	}
	v, err := s.rdb.HGetAll(ctx, s.prefix+id).Result()
	if err != nil {
		return Persona{}, err
	}
	if v["prompt"] == "" {
		return Persona{}, ErrNotFound
	}
	p := Persona{ID: id, Name: v["name"], Prompt: v["prompt"], Hash: v["hash"]}
	s.mu.Lock()
	s.local[id] = cached{p, time.Now()}
	s.mu.Unlock()
	return p, nil
}
