package httpstream

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/outcome"
)

// ErrClass is a coarse, URL-free failure classification (HS-1.9). Only the
// class and a static, sanitized detail string ever reach logs, error bodies,
// or the DB error_message column.
type ErrClass string

const (
	// ClassInvalidKey — resolve key/envelope structurally invalid or tampered.
	ClassInvalidKey ErrClass = "invalid_key"
	// ClassNoHandler — the key's protocol handler is not registered/configured.
	ClassNoHandler ErrClass = "no_handler"
	// ClassBackendUnavailable — backend transport/auth/5xx failure (transient).
	ClassBackendUnavailable ErrClass = "backend_unavailable"
	// ClassRateLimited — backend 429 response; transient and carries only a
	// bounded numeric cooldown, never the raw header value.
	ClassRateLimited ErrClass = "rate_limited"
	// ClassNoSource — backend answered authoritatively with no usable source.
	ClassNoSource ErrClass = "no_source"
	// ClassRepresentationLost — the grabbed representation (Selector) is no
	// longer present in a fresh resolve; never silently downgraded (D10).
	ClassRepresentationLost ErrClass = "representation_lost"
	// ClassUpstreamMalformed — source responded with malformed range/media
	// semantics (maps to 502 at the stream surface).
	ClassUpstreamMalformed ErrClass = "upstream_malformed"
)

// Error is the sanitized error type used across the HTTP stream lane.
// Detail must be static text — never interpolate URLs, query strings, or
// header values into it.
type Error struct {
	Class             ErrClass
	Detail            string
	RetryAfterSeconds int
	cause             error
}

func (e *Error) Error() string {
	if e.Detail == "" {
		return "httpstream: " + string(e.Class)
	}
	return "httpstream: " + string(e.Class) + ": " + e.Detail
}

// Unwrap preserves an underlying cause for in-process diagnosis without ever
// rendering its potentially credential-bearing text through Error.
func (e *Error) Unwrap() error { return e.cause }

// OutcomeClass implements outcome.Classifier (SF-01), mapping this lane's own
// detailed ErrClass into the shared permanent-here/transient-here/account-
// level vocabulary used for cross-lane rate-limited logging and the
// zero-unknown soak gate. ErrClass remains the detailed, lane-owned reason;
// this is purely an additional coarse view of it and changes no existing
// behavior.
func (e *Error) OutcomeClass() outcome.Class {
	switch e.Class {
	case ClassRateLimited:
		return outcome.ClassAccountLevel
	case ClassBackendUnavailable:
		return outcome.ClassTransientHere
	case ClassInvalidKey, ClassNoHandler, ClassNoSource, ClassRepresentationLost, ClassUpstreamMalformed:
		return outcome.ClassPermanentHere
	default:
		return outcome.ClassUnknown
	}
}

// NewError constructs a sanitized error. Detail is scrubbed defensively.
func NewError(class ErrClass, detail string) *Error {
	return &Error{Class: class, Detail: ScrubURLs(detail)}
}

// WrapError constructs a sanitized error while retaining cause for errors.Is
// and errors.As. Cause is deliberately absent from Error's rendered text.
func WrapError(class ErrClass, detail string, cause error) *Error {
	e := NewError(class, detail)
	e.cause = cause
	return e
}

// NewRateLimitError constructs a sanitized backend cooldown error. Only the
// delta-seconds form is accepted and it is capped at five minutes.
func NewRateLimitError(rawRetryAfter string) *Error {
	seconds, err := strconv.Atoi(strings.TrimSpace(rawRetryAfter))
	if err != nil || seconds < 1 {
		seconds = 30
	}
	if seconds > 300 {
		seconds = 300
	}
	return &Error{Class: ClassRateLimited, Detail: "backend rate limited", RetryAfterSeconds: seconds}
}

// RetryAfter returns the bounded cooldown carried by a rate-limit error.
func RetryAfter(err error) time.Duration {
	var he *Error
	if errors.As(err, &he) && he.RetryAfterSeconds > 0 {
		return time.Duration(he.RetryAfterSeconds) * time.Second
	}
	return 30 * time.Second
}

// ClassOf returns the ErrClass of err when it is (or wraps) an *Error,
// otherwise "".
func ClassOf(err error) ErrClass {
	var he *Error
	if errors.As(err, &he) {
		return he.Class
	}
	return ""
}

// Transient reports whether the class is a bounded-retry candidate.
func (c ErrClass) Transient() bool {
	return c == ClassBackendUnavailable || c == ClassRateLimited
}

// urlishRe matches URL-shaped substrings (scheme://... or host:port/path
// query fragments) so accidental interpolation never persists a source URL.
var urlishRe = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://\S+`)

// ScrubURLs replaces any URL-shaped substring with a redaction marker and
// bounds the result. Use it on any string of non-static provenance before it
// reaches a log line, error body, or DB column (HS-1.9).
func ScrubURLs(s string) string {
	s = urlishRe.ReplaceAllString(s, "[url-redacted]")
	const max = 300
	if len(s) > max {
		s = s[:max]
	}
	return s
}

// Sanitize renders any error as a persistence-safe string: *Error passes
// through (already clean); everything else — including url.Error values that
// embed the full request URL — is scrubbed.
func Sanitize(err error) string {
	if err == nil {
		return ""
	}
	var he *Error
	if errors.As(err, &he) {
		return he.Error()
	}
	return ScrubURLs(err.Error())
}

// Wrapf builds a sanitized error with a static format and pre-scrubbed args.
func Wrapf(class ErrClass, format string, args ...any) *Error {
	return NewError(class, fmt.Sprintf(format, args...))
}
