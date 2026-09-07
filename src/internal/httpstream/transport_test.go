package httpstream

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestNewHTTPClientBudgets(t *testing.T) {
	policy := NewSecurityPolicy(false)

	metadata := NewHTTPClient(policy, TrustBackend, TransportMetadata)
	if metadata.Timeout != metadataRequestTimeout {
		t.Fatalf("metadata timeout = %s, want %s", metadata.Timeout, metadataRequestTimeout)
	}
	metadataTransport, ok := metadata.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("metadata transport type = %T", metadata.Transport)
	}
	if metadataTransport.MaxConnsPerHost != 8 {
		t.Fatalf("metadata MaxConnsPerHost = %d, want 8", metadataTransport.MaxConnsPerHost)
	}

	aggregate := NewHTTPClient(policy, TrustBackend, TransportAggregateMetadata)
	if aggregate.Timeout != aggregateMetadataRequestTimeout {
		t.Fatalf("aggregate metadata timeout = %s, want %s", aggregate.Timeout, aggregateMetadataRequestTimeout)
	}
	aggregateTransport, ok := aggregate.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("aggregate metadata transport type = %T", aggregate.Transport)
	}
	if aggregateTransport.ResponseHeaderTimeout != aggregateMetadataRequestTimeout {
		t.Fatalf("aggregate metadata ResponseHeaderTimeout = %s, want %s", aggregateTransport.ResponseHeaderTimeout, aggregateMetadataRequestTimeout)
	}

	probe := NewHTTPClient(policy, TrustSource, TransportProbe)
	if probe.Timeout != probeRequestTimeout {
		t.Fatalf("probe timeout = %s, want %s", probe.Timeout, probeRequestTimeout)
	}

	stream := NewHTTPClient(policy, TrustSource, TransportStream)
	if stream.Timeout != 0 {
		t.Fatalf("stream timeout = %s, want no whole-request deadline", stream.Timeout)
	}
	streamTransport, ok := stream.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("stream transport type = %T", stream.Transport)
	}
	if streamTransport.MaxConnsPerHost != 4 {
		t.Fatalf("stream MaxConnsPerHost = %d, want 4", streamTransport.MaxConnsPerHost)
	}
	if streamTransport.DialContext == nil {
		t.Fatal("stream DialContext is nil")
	}
}

func TestBackendHTTPClientRedirectApprovalIsExplicitAndHeaderSafe(t *testing.T) {
	var reached atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Store(true)
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Referer") != "" {
			t.Error("cross-origin metadata redirect leaked sensitive headers")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/metadata", http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	denied := NewHTTPClient(NewSecurityPolicy(false), TrustBackend, TransportMetadata)
	req, _ := http.NewRequest(http.MethodGet, source.URL, nil)
	req.Header.Set("Authorization", "Bearer secret")
	if _, err := denied.Do(req); ClassOf(err) != ClassAddressDenied {
		t.Fatalf("unapproved backend redirect was not denied: %v", err)
	}
	if reached.Load() {
		t.Fatal("unapproved backend redirect reached its target")
	}

	approved := NewHTTPClient(NewSecurityPolicy(false), TrustBackend, TransportMetadata, target.URL)
	req, _ = http.NewRequest(http.MethodGet, source.URL, nil)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Cookie", "session=secret")
	resp, err := approved.Do(req)
	if err != nil {
		t.Fatalf("approved backend redirect failed: %v", err)
	}
	resp.Body.Close()
	if !reached.Load() {
		t.Fatal("approved backend redirect did not reach its target")
	}
}
