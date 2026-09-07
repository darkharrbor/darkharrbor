package torbox

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/provider"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestC02HTTPClientRedactsRequestErrors(t *testing.T) {
	var logs bytes.Buffer
	c := NewHTTPClient(slog.New(slog.NewTextHandler(&logs, nil)), "https://provider.invalid", "secret", "test", time.Second, nil)
	c.httpClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial https://provider.invalid/file?token=must-not-appear")
	})

	_, err := c.do(context.Background(), provider.OpQuery, http.MethodGet, "/test", nil, "")
	if err == nil {
		t.Fatal("request unexpectedly succeeded")
	}
	for _, got := range []string{logs.String(), err.Error()} {
		if strings.Contains(got, "must-not-appear") || strings.Contains(got, "https://") || strings.Contains(got, "token=") {
			t.Fatalf("request error leaked upstream URL: %s", got)
		}
	}
}
