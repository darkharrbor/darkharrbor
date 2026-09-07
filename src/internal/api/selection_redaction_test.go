package api

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/prowlarr"
)

func TestSelectionSearchRedactsProwlarrRequestURL(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	baseURL := origin.URL + "/private?credential=do-not-log"
	origin.Close()

	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	cfg := &config.Config{}
	cfg.Server.BaseURL = "http://darkharrbor.invalid"
	s := &Server{
		cfg:      cfg,
		log:      log,
		prowlarr: prowlarr.New(baseURL, "secret", "test", time.Second, log),
	}
	req := httptest.NewRequest(http.MethodGet, "http://darkharrbor.invalid/api?t=tvsearch&q=test", nil)

	if got := s.selectionSearch(req, req.URL.Query(), "tv"); got.items != nil {
		t.Fatalf("selection result = %#v, want nil on transport failure", got)
	}
	rendered := logs.String()
	if strings.Contains(rendered, origin.URL) || strings.Contains(rendered, "do-not-log") {
		t.Fatalf("selection log leaked request URL: %s", rendered)
	}
	if !strings.Contains(rendered, "[url-redacted]") {
		t.Fatalf("selection log lacks redaction marker: %s", rendered)
	}
}
