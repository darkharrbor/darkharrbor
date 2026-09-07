package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/provider"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

// fakeUsenetProvider is a minimal provider.UsenetProvider stand-in for
// pickUsenetProvider tests — only Name() is exercised, the rest are
// unreachable stubs satisfying the interface.
type fakeUsenetProvider struct{ name string }

func (f *fakeUsenetProvider) Name() string              { return f.name }
func (f *fakeUsenetProvider) Governor() provider.Policy { return nil }
func (f *fakeUsenetProvider) Probe(context.Context, []byte, int64) (string, error) {
	return "", nil
}
func (f *fakeUsenetProvider) Stream(context.Context, string, []byte, int, http.ResponseWriter, string) error {
	return nil
}
func (f *fakeUsenetProvider) CorrectedTotal(context.Context, string, []byte, int) int64 { return 0 }
func (f *fakeUsenetProvider) StreamRARManifest(context.Context, string, int64, http.ResponseWriter, string, string, string) error {
	return nil
}
func (*fakeUsenetProvider) StreamZIPEntry(_ context.Context, _ string, _ int, _ http.ResponseWriter, _, _, _ string) error {
	return nil
}
func (*fakeUsenetProvider) GrabTimeHealthCheck(context.Context, []byte, int) (bool, int, error) {
	return false, 0, nil
}

func testMultiProviderServer() *Server {
	newshosting := &fakeUsenetProvider{name: "newshosting"}
	torboxNNTP := &fakeUsenetProvider{name: "torbox"}
	return &Server{
		usenetProviders: map[string]provider.UsenetProvider{
			"newshosting": newshosting,
			"torbox":      torboxNNTP,
		},
		usenetOrder: []string{"newshosting", "torbox"},
	}
}

// BUGFIX regression test (2026-07-02): before pickUsenetProvider existed,
// the Server held a single provider.UsenetProvider field populated from
// only the FIRST configured usenet provider (provSet.DefaultUsenet()) —
// every other configured provider (e.g. TorBox's NNTP News Server
// alongside Newshosting) built a live connection pool that nothing ever
// used. These tests assert the fix: every configured provider is reachable
// by name, and an item's persisted Provider field is honored.
func TestPickUsenetProvider_DefaultsToFirstInOrder(t *testing.T) {
	s := testMultiProviderServer()
	item := &store.Item{} // no Provider set

	name, up, ok := s.pickUsenetProvider(item)
	if !ok {
		t.Fatal("pickUsenetProvider() ok = false, want true")
	}
	if name != "newshosting" {
		t.Errorf("name = %q, want newshosting (first in usenetOrder)", name)
	}
	if up.Name() != "newshosting" {
		t.Errorf("provider.Name() = %q, want newshosting", up.Name())
	}
}

func TestPickUsenetProvider_HonorsItemProvider(t *testing.T) {
	s := testMultiProviderServer()
	providerName := "torbox"
	item := &store.Item{Provider: &providerName}

	name, up, ok := s.pickUsenetProvider(item)
	if !ok {
		t.Fatal("pickUsenetProvider() ok = false, want true")
	}
	if name != "torbox" {
		t.Errorf("name = %q, want torbox (item's persisted provider, not the default)", name)
	}
	if up.Name() != "torbox" {
		t.Errorf("provider.Name() = %q, want torbox", up.Name())
	}
}

func TestPickUsenetProvider_FallsBackWhenItemProviderNoLongerConfigured(t *testing.T) {
	s := testMultiProviderServer()
	providerName := "some-removed-provider"
	item := &store.Item{Provider: &providerName}

	name, up, ok := s.pickUsenetProvider(item)
	if !ok {
		t.Fatal("pickUsenetProvider() ok = false, want true (should fall back, not fail)")
	}
	if name != "newshosting" {
		t.Errorf("name = %q, want newshosting (fallback to default when item's provider is gone)", name)
	}
	if up.Name() != "newshosting" {
		t.Errorf("provider.Name() = %q, want newshosting", up.Name())
	}
}

func TestPickUsenetProvider_NoneConfigured(t *testing.T) {
	s := &Server{}
	item := &store.Item{}

	_, _, ok := s.pickUsenetProvider(item)
	if ok {
		t.Error("pickUsenetProvider() ok = true, want false when no usenet provider is configured")
	}
}

func TestPickUsenetProvider_NilItem(t *testing.T) {
	s := testMultiProviderServer()

	name, up, ok := s.pickUsenetProvider(nil)
	if !ok {
		t.Fatal("pickUsenetProvider(nil) ok = false, want true (falls back to default)")
	}
	if name != "newshosting" || up.Name() != "newshosting" {
		t.Errorf("nil item should resolve to the default provider, got %q", name)
	}
}
