package httpstream

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Regression: torrentio.strem.fun 403s Go's default UA; every client built
// here must send DefaultUserAgent unless the caller set one, on redirect hops too.
func TestNewHTTPClientSetsUserAgent(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.UserAgent())
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/end", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewHTTPClient(NewSecurityPolicy(true), TrustBackend, TransportProbe, srv.URL)
	resp, err := c.Get(srv.URL + "/start")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(seen) != 2 {
		t.Fatalf("want 2 requests (redirect followed), got %d", len(seen))
	}
	for i, ua := range seen {
		if ua != DefaultUserAgent {
			t.Fatalf("hop %d UA = %q, want %q", i, ua, DefaultUserAgent)
		}
	}

	seen = nil
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/end", nil)
	req.Header.Set("User-Agent", "Caller/1")
	resp, err = c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if seen[0] != "Caller/1" {
		t.Fatalf("caller UA overridden: %q", seen[0])
	}
}
