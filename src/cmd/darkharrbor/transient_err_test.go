package main

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/provider"
)

func TestTransientProviderErr(t *testing.T) {
	live := context.Background()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	hold := fmt.Errorf("torbox policy: hold wait (query): %w", context.DeadlineExceeded)
	for _, tt := range []struct {
		name   string
		parent context.Context
		err    error
		want   bool
	}{
		{"nil", live, nil, false},
		{"429", live, provider.MarkRetryable(errors.New("torbox status 429")), true},
		{"own timeout behind hold", live, hold, true},
		{"shutdown", cancelled, hold, false},
		{"permanent", live, errors.New("torbox status 403: AUTH_ERROR"), false},
	} {
		if got := transientProviderErr(tt.parent, tt.err); got != tt.want {
			t.Errorf("%s: got %v, want %v", tt.name, got, tt.want)
		}
	}
}
