package ratelimit

import (
	"sync"
	"time"
)

type Limiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	seen   map[string][]time.Time
}

func New(limit int, window time.Duration) *Limiter {
	return &Limiter{limit: limit, window: window, seen: make(map[string][]time.Time)}
}

func (l *Limiter) Allow(key string, now time.Time) bool {
	if l == nil || l.limit <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := now.Add(-l.window)
	entries := l.seen[key]
	first := 0
	for first < len(entries) && !entries[first].After(cutoff) {
		first++
	}
	entries = entries[first:]
	if len(entries) >= l.limit {
		l.seen[key] = entries
		return false
	}
	l.seen[key] = append(entries, now)
	return true
}

func (l *Limiter) Len(key string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.seen[key])
}
