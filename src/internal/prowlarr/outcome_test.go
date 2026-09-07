package prowlarr

import (
	"errors"
	"net/http"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/outcome"
)

func TestClassifiedHTTPError(t *testing.T) {
	base := errors.New("safe test error")
	if got := outcome.Classify(classifiedHTTPError(http.StatusTooManyRequests, base)); got != outcome.ClassAccountLevel {
		t.Fatalf("429 class = %q", got)
	}
	if got := outcome.Classify(classifiedHTTPError(http.StatusBadGateway, base)); got != outcome.ClassTransientHere {
		t.Fatalf("502 class = %q", got)
	}
}
