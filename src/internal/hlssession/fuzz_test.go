package hlssession

import (
	"context"
	"testing"
	"time"
)

// FuzzValidSessionID and FuzzValidResourceID exercise the two ID validators
// against arbitrary attacker-controlled strings (DG-03's standing
// "every new parser ships a committed fuzz target" convention). These
// validators will eventually gate untrusted HTTP path segments once HR4.2+
// wires a real endpoint; today they already gate Graph.Resolve/Session, so
// they must never panic on any input.
func FuzzValidSessionID(f *testing.F) {
	seeds := []string{
		"", "hs000000000000", "hsGGGGGGGGGGGG", "hf000000000000",
		"hs00000000000", "hs0000000000000", "../../etc/passwd",
		"hs000000000000\x00", "hs000000000000://evil",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("ValidSessionID panicked on %q: %v", s, r)
			}
		}()
		_ = ValidSessionID(s)

		g, _ := testGraph(t, fixedClock(time.Now()))
		if _, err := g.Session(context.Background(), s); err == nil && !ValidSessionID(s) {
			t.Fatalf("Session accepted malformed id %q", s)
		}
	})
}

func FuzzValidResourceID(f *testing.F) {
	seeds := []string{
		"", "hz000000000000", "hzGGGGGGGGGGGG", "hs000000000000",
		"hz00000000000", "hz0000000000000", "../../etc/passwd",
		"hz000000000000\x00", "hz000000000000://evil",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("ValidResourceID panicked on %q: %v", s, r)
			}
		}()
		_ = ValidResourceID(s)

		g, _ := testGraph(t, fixedClock(time.Now()))
		if _, err := g.Resolve(context.Background(), s); err == nil && !ValidResourceID(s) {
			t.Fatalf("Resolve accepted malformed id %q", s)
		}
	})
}

// FuzzReferenceRoundTrip exercises the Reference JSON persistence helpers
// against arbitrary strings in every free-text field, proving
// marshal/unmarshal never panics and validateReference never accepts a
// URL-shaped or oversized field regardless of fuzzer-discovered input.
func FuzzReferenceRoundTrip(f *testing.F) {
	seeds := []string{"", "ia", "http://evil.example", "a://b", ":", "://", "\x00\x01\x02"}
	for _, s := range seeds {
		f.Add(s, s, s)
	}
	f.Fuzz(func(t *testing.T, handler, backendID, selector string) {
		ref := Reference{Handler: handler, BackendID: backendID, Selector: selector}
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("reference round trip panicked on %+v: %v", ref, r)
			}
		}()
		encoded, err := marshalReference(ref)
		if err != nil {
			return // oversized/unmarshalable inputs are expected rejections
		}
		if _, err := unmarshalReference(encoded); err != nil {
			t.Fatalf("unmarshal of just-marshaled reference failed: %v", err)
		}
		verr := validateReference(ref)
		if verr == nil {
			// If validation passed, none of the free-text fields may
			// contain a URL scheme separator (DG-04 by construction).
			for _, v := range []string{handler, backendID, selector} {
				if containsScheme(v) {
					t.Fatalf("validateReference accepted URL-shaped field %q", v)
				}
			}
		}
	})
}

func containsScheme(s string) bool {
	for i := 0; i+2 < len(s); i++ {
		if s[i] == ':' && s[i+1] == '/' && s[i+2] == '/' {
			return true
		}
	}
	return false
}
