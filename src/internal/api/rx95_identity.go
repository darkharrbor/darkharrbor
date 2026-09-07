package api

import (
	"sync"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/mediaidentity"
	"github.com/darkharrbor/darkharrbor/internal/output"
)

// RX-9.5 (RD-31) binds an aggregator playback batch to the Stremio coordinate
// observed just before it.
//
// The mechanism is specified from a CAPTURED payload, not an assumed shape --
// the two prior attempts failed because they assumed. Observed 2026-08-18 on
// tt0093629: the addon was queried TWICE, then ONE /generate_urls batch of 92
// URLs arrived 9.7s later spanning 3ms. Only 36 of those URLs were
// aggregator-owned and carried a metadata_id; the other 56 were direct
// external addon links carrying no identity marker at all. So the binding is
// PER BATCH -- one request is one identity, covering all 92 -- while
// metadata_id CORROBORATES it rather than carrying it. Per-URL correlation
// would have reached 39% of sources and failed the any-source requirement.
//
// Everything here abstains rather than guesses. An ambiguous window binds
// nothing and playback falls back to the random representation ID: the same
// abstain-on-unknown rule used across this campaign, because a wrong identity
// commits the wrong file to an Arr.

const (
	// rx95ObservationWindow bounds how long an addon observation may bind a
	// later batch. The captured gap was 9.7s; this allows margin for a slow
	// fan-out while expiring quickly enough that an unrelated later batch
	// cannot inherit a stale identity.
	rx95ObservationWindow = 90 * time.Second
	// rx95MaxObservations bounds memory: an unbounded map fed by an external
	// caller is a liability.
	rx95MaxObservations = 512
	// rx95MaxBindings bounds the learned metadata_id -> identity map.
	rx95MaxBindings = 512
	// rx95BindingTTL expires a learned binding so it cannot outlive its use.
	rx95BindingTTL = 24 * time.Hour
)

type rx95Identity struct {
	Kind    string `json:"k"`
	IMDb    string `json:"i"`
	Season  int    `json:"s,omitempty"`
	Episode int    `json:"e,omitempty"`
}

func (i rx95Identity) valid() bool {
	if mediaidentity.NormalizedIDs(mediaidentity.ProviderIDs{IMDB: i.IMDb}).IMDB != i.IMDb {
		return false
	}
	switch i.Kind {
	case "movie":
		return i.Season == 0 && i.Episode == 0
	case "series":
		return i.Season >= 0 && i.Season <= 9999 && i.Episode >= 1 && i.Episode <= 9999
	default:
		return false
	}
}

type rx95Observation struct {
	identity rx95Identity
	at       time.Time
}

type rx95Binding struct {
	identity rx95Identity
	at       time.Time
}

// rx95Store holds recent addon observations and learned metadata_id bindings.
// Construct with newRX95Store; the zero value is not usable.
type rx95Store struct {
	mu sync.Mutex
	// observations is keyed BY IDENTITY so a repeated addon query for the same
	// title is IDEMPOTENT rather than counting as a second identity. The
	// aggregator was observed querying twice for one resolution, so treating
	// repeats as ambiguity would defeat the binding entirely.
	observations map[rx95Identity]rx95Observation
	bindings     map[string]rx95Binding
}

func newRX95Store() *rx95Store {
	return &rx95Store{
		observations: make(map[rx95Identity]rx95Observation),
		bindings:     make(map[string]rx95Binding),
	}
}

// Observe records a coordinate seen at the addon boundary. Repeats of the same
// identity refresh the timestamp instead of adding an entry.
func (s *rx95Store) Observe(identity rx95Identity, now time.Time) {
	if s == nil || !identity.valid() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	if _, exists := s.observations[identity]; !exists && len(s.observations) >= rx95MaxObservations {
		return
	}
	s.observations[identity] = rx95Observation{identity: identity, at: now}
}

// Resolve returns the identity a batch belongs to, and whether it is bound.
//
// metadataID is the value carried by aggregator-owned URLs in the batch and
// may be empty, which is the common case for external addon links. A known
// metadata_id binds directly. Otherwise exactly ONE unexpired observation must
// be present: zero means nothing was seen, and two or more means concurrent
// resolutions are in flight and the correct identity is genuinely unknown.
// Both abstain.
func (s *rx95Store) Resolve(metadataID string, now time.Time) (rx95Identity, bool) {
	if s == nil {
		return rx95Identity{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)

	if metadataID != "" {
		if b, ok := s.bindings[metadataID]; ok {
			return b.identity, true
		}
	}
	if len(s.observations) != 1 {
		return rx95Identity{}, false
	}
	var only rx95Identity
	for identity := range s.observations {
		only = identity
	}
	// Learn the binding so later batches for the same title resolve directly
	// even once the window has moved on or another resolution is concurrent.
	if metadataID != "" && len(s.bindings) < rx95MaxBindings {
		s.bindings[metadataID] = rx95Binding{identity: only, at: now}
	}
	return only, true
}

// rx95Contradicted reports whether a release title positively DISAGREES with a
// bound series coordinate. Per the standing rule a filename may only
// contradict an identity and may NEVER establish one, so this is consulted
// only after a binding exists and returns false whenever it cannot tell.
func rx95Contradicted(identity rx95Identity, filename string) bool {
	if identity.Kind != "series" || filename == "" {
		return false
	}
	season, episode, ok := output.EpisodeNumbers(filename)
	if !ok {
		return false
	}
	return season != identity.Season || episode != identity.Episode
}

func (s *rx95Store) pruneLocked(now time.Time) {
	for identity, obs := range s.observations {
		if now.Sub(obs.at) > rx95ObservationWindow {
			delete(s.observations, identity)
		}
	}
	for key, b := range s.bindings {
		if now.Sub(b.at) > rx95BindingTTL {
			delete(s.bindings, key)
		}
	}
}
