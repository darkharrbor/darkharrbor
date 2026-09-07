package api

import (
	"sync"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/httpstream"
)

const (
	rx93SelectorTTL  = 24 * time.Hour
	rx93MaxSelectors = 512
)

type rx93SelectorKey struct {
	representationID string
	filename         string
	total            int64
}

type rx93SelectorEntry struct {
	result httpstream.SearchResult
	at     time.Time
}

type rx93SelectorStore struct {
	mu      sync.Mutex
	entries map[rx93SelectorKey]rx93SelectorEntry
}

func newRX93SelectorStore() *rx93SelectorStore {
	return &rx93SelectorStore{entries: make(map[rx93SelectorKey]rx93SelectorEntry)}
}

func rx93SelectorCacheKey(representationID, filename string, total int64) (rx93SelectorKey, bool) {
	filename, ok := rx93Filename(filename)
	if !ok || representationID == "" || total <= 0 {
		return rx93SelectorKey{}, false
	}
	return rx93SelectorKey{representationID: representationID, filename: filename, total: total}, true
}

func (s *rx93SelectorStore) Put(key rx93SelectorKey, result httpstream.SearchResult, now time.Time) {
	if s == nil || key.representationID == "" || key.filename == "" || key.total <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	if _, exists := s.entries[key]; !exists && len(s.entries) >= rx93MaxSelectors {
		return
	}
	s.entries[key] = rx93SelectorEntry{result: result, at: now}
}

func (s *rx93SelectorStore) Get(key rx93SelectorKey, now time.Time) (httpstream.SearchResult, bool) {
	if s == nil {
		return httpstream.SearchResult{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	entry, ok := s.entries[key]
	if !ok {
		return httpstream.SearchResult{}, false
	}
	return entry.result, true
}

func (s *rx93SelectorStore) Delete(key rx93SelectorKey) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.entries, key)
	s.mu.Unlock()
}

func (s *rx93SelectorStore) pruneLocked(now time.Time) {
	for key, entry := range s.entries {
		if now.Sub(entry.at) > rx93SelectorTTL {
			delete(s.entries, key)
		}
	}
}
