package hls

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"github.com/darkharrbor/darkharrbor/internal/contentproof"
)

const (
	MaxSteeringBytes    = 64 << 10
	MaxSteeringPathways = 128
	MaxSteeringTTL      = 24 * 60 * 60
)

var ErrSteeringManifest = errors.New("hls: invalid steering manifest")

// SteeringManifest is the safe subset DH consumes and emits. Reload URIs and
// clone URI replacements remain transient upstream data and are never copied.
type SteeringManifest struct {
	Version         int
	TTL             int
	PathwayPriority []string
}

type steeringWire struct {
	Version         json.Number     `json:"VERSION"`
	TTL             json.Number     `json:"TTL"`
	PathwayPriority []string        `json:"PATHWAY-PRIORITY"`
	PathwayClones   json.RawMessage `json:"PATHWAY-CLONES"`
}

// ParseSteeringManifest parses the VERSION-1 safe subset with strict bounds.
// Unknown keys are ignored as required by the Content Steering protocol.
func ParseSteeringManifest(body []byte) (SteeringManifest, error) {
	var out SteeringManifest
	if len(body) == 0 || len(body) > MaxSteeringBytes || !json.Valid(body) ||
		hasDuplicateJSONKey(body) {
		return out, ErrSteeringManifest
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var wire steeringWire
	if err := dec.Decode(&wire); err != nil {
		return out, ErrSteeringManifest
	}
	version, err := wire.Version.Int64()
	if err != nil || version != 1 {
		return out, ErrSteeringManifest
	}
	ttl, err := wire.TTL.Int64()
	if err != nil || ttl <= 0 || ttl > MaxSteeringTTL {
		return out, ErrSteeringManifest
	}
	if len(wire.PathwayPriority) == 0 || len(wire.PathwayPriority) > MaxSteeringPathways {
		return out, ErrSteeringManifest
	}
	seen := make(map[string]struct{}, len(wire.PathwayPriority))
	for _, id := range wire.PathwayPriority {
		if !ValidPathwayID(id) {
			return out, ErrSteeringManifest
		}
		if _, ok := seen[id]; ok {
			return out, ErrSteeringManifest
		}
		seen[id] = struct{}{}
	}
	out.Version = 1
	out.TTL = int(ttl)
	out.PathwayPriority = append([]string(nil), wire.PathwayPriority...)
	return out, nil
}

// PathwayCandidate describes one pathway the caller can actually serve.
// Explicit origin pathways need no cross-source proof. A synthesized
// candidate is eligible only after the shared proof graph returns Proven.
type PathwayCandidate struct {
	ID             string
	OriginSupplied bool
	Proof          contentproof.Relation
}

// SynthesizeSteeringManifest filters unavailable/unproved pathways and then
// applies the passive scorer. Stable scorer ties preserve origin order.
func SynthesizeSteeringManifest(
	manifest SteeringManifest,
	candidates []PathwayCandidate,
	rank func([]string) []string,
) ([]byte, error) {
	allowed := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		if !ValidPathwayID(candidate.ID) {
			return nil, ErrSteeringManifest
		}
		if candidate.OriginSupplied || candidate.Proof == contentproof.RelationProven {
			allowed[candidate.ID] = struct{}{}
		}
	}
	priority := make([]string, 0, len(manifest.PathwayPriority))
	for _, id := range manifest.PathwayPriority {
		if _, ok := allowed[id]; ok {
			priority = append(priority, id)
		}
	}
	if len(priority) == 0 {
		return nil, ErrSteeringManifest
	}
	if rank != nil {
		priority = rank(priority)
	}
	wire := struct {
		Version         int      `json:"VERSION"`
		TTL             int      `json:"TTL"`
		PathwayPriority []string `json:"PATHWAY-PRIORITY"`
	}{Version: 1, TTL: manifest.TTL, PathwayPriority: priority}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, ErrSteeringManifest
	}
	return append(body, '\n'), nil
}

func hasDuplicateJSONKey(body []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	token, err := dec.Token()
	if err != nil || duplicateInJSONValue(dec, token) {
		return true
	}
	_, err = dec.Token()
	return !errors.Is(err, io.EOF)
}

func duplicateInJSONValue(dec *json.Decoder, token json.Token) bool {
	delim, ok := token.(json.Delim)
	if !ok {
		return false
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for dec.More() {
			key, err := dec.Token()
			name, ok := key.(string)
			if err != nil || !ok {
				return true
			}
			if _, exists := seen[name]; exists {
				return true
			}
			seen[name] = struct{}{}
			value, err := dec.Token()
			if err != nil || duplicateInJSONValue(dec, value) {
				return true
			}
		}
		end, err := dec.Token()
		return err != nil || end != json.Delim('}')
	case '[':
		for dec.More() {
			value, err := dec.Token()
			if err != nil || duplicateInJSONValue(dec, value) {
				return true
			}
		}
		end, err := dec.Token()
		return err != nil || end != json.Delim(']')
	default:
		return true
	}
}
