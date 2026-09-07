package httpstream

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/outcome"
)

func TestWrapErrorPreservesCauseWithoutRenderingIt(t *testing.T) {
	cause := fmt.Errorf(`Get "https://user:secret@example.invalid/stream?token=leak": %w`, context.DeadlineExceeded)
	err := WrapError(ClassBackendUnavailable, "stremio backend request failed", cause)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("wrapped transport cause must remain available to diagnostics")
	}
	for _, leak := range []string{"secret", "token=leak", "example.invalid", "https://"} {
		if strings.Contains(err.Error(), leak) || strings.Contains(Sanitize(err), leak) {
			t.Fatalf("wrapped error rendered %q: %s", leak, err)
		}
	}
}

// TestErrorOutcomeClass proves SF-01's shared vocabulary covers every
// existing httpstream.ErrClass value with no ClassUnknown fallthrough, and
// that outcome.Classify(err) reaches the same result via the Classifier
// interface without httpstream needing to know about outcome's internals.
func TestErrorOutcomeClass(t *testing.T) {
	cases := []struct {
		class ErrClass
		want  outcome.Class
	}{
		{ClassInvalidKey, outcome.ClassPermanentHere},
		{ClassNoHandler, outcome.ClassPermanentHere},
		{ClassBackendUnavailable, outcome.ClassTransientHere},
		{ClassRateLimited, outcome.ClassAccountLevel},
		{ClassNoSource, outcome.ClassPermanentHere},
		{ClassRepresentationLost, outcome.ClassPermanentHere},
		{ClassUpstreamMalformed, outcome.ClassPermanentHere},
	}
	for _, tc := range cases {
		e := NewError(tc.class, "detail")
		if got := e.OutcomeClass(); got != tc.want {
			t.Errorf("(%s).OutcomeClass() = %q, want %q", tc.class, got, tc.want)
		}
		if got := outcome.Classify(e); got != tc.want {
			t.Errorf("outcome.Classify(%s) = %q, want %q", tc.class, got, tc.want)
		}
	}
}

func TestErrorOutcomeClassUnknownForUnrecognizedClass(t *testing.T) {
	e := NewError(ErrClass("some_future_class"), "detail")
	if got := e.OutcomeClass(); got != outcome.ClassUnknown {
		t.Fatalf("unrecognized ErrClass should map to ClassUnknown (soak-observable taxonomy gap), got %q", got)
	}
}
