package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const secTestKey = "correct horse battery staple"

func newSealTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "sec01.db"), 5*time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatalf("RunMigrationsFS: %v", err)
	}
	return New(db)
}

// sealTestCred is a realistic provider credential shape: the TorBox News
// Server record is what SEC-01 exists to protect.
func sealTestCred() string {
	b, _ := json.Marshal(map[string]any{
		"Host": "nntp.example.invalid", "Port": 563, "TLS": true,
		"Username": "d290f1ee-6c54-4b01-90e6-d701748f0851",
		"Password": "s3cr3t-value-that-must-not-appear", "Connections": 5,
	})
	return string(b)
}

// sealRawColumn reads the stored column WITHOUT the accessor, so a test can
// assert what actually landed on disk rather than what the API hands back.
func sealRawColumn(t *testing.T, s *Store, provider string) string {
	t.Helper()
	var raw string
	if err := s.db.QueryRow(
		`SELECT credentials_json FROM provider_credentials WHERE provider_name=?`, provider,
	).Scan(&raw); err != nil {
		t.Fatalf("read raw column: %v", err)
	}
	return raw
}

func TestProviderCredentialSealRoundTrip(t *testing.T) {
	s := newSealTestStore(t)
	s.SetCredentialSealKey(secTestKey)
	ctx := context.Background()

	want := sealTestCred()
	if err := s.SetProviderCredential(ctx, "torbox-nntp", want); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, found, err := s.GetProviderCredential(ctx, "torbox-nntp")
	if err != nil || !found {
		t.Fatalf("get: err=%v found=%v", err, found)
	}
	if got != want {
		t.Fatalf("round-trip mismatch")
	}
}

// The column must be OPAQUE. This is the property the row exists for: a
// leaked backup must not be a credential leak.
func TestProviderCredentialColumnIsOpaque(t *testing.T) {
	s := newSealTestStore(t)
	s.SetCredentialSealKey(secTestKey)
	ctx := context.Background()

	if err := s.SetProviderCredential(ctx, "torbox-nntp", sealTestCred()); err != nil {
		t.Fatalf("set: %v", err)
	}
	raw := sealRawColumn(t, s, "torbox-nntp")
	for _, leak := range []string{
		"s3cr3t-value-that-must-not-appear",
		"nntp.example.invalid",
		"d290f1ee-6c54-4b01-90e6-d701748f0851",
		"Password", "Username", "Host",
	} {
		if strings.Contains(raw, leak) {
			t.Fatalf("stored column leaks %q", leak)
		}
	}
	if !strings.HasPrefix(raw, sealedCredentialPrefix) {
		t.Fatalf("stored column is not marked sealed")
	}
}

// A pre-existing plaintext row must migrate transparently on first read, with
// no operator action and no flag day.
func TestProviderCredentialLegacyPlaintextMigratesOnRead(t *testing.T) {
	s := newSealTestStore(t)
	ctx := context.Background()

	want := sealTestCred()
	if err := s.SetProviderCredential(ctx, "torbox-nntp", want); err != nil {
		t.Fatalf("legacy set: %v", err)
	}
	if raw := sealRawColumn(t, s, "torbox-nntp"); !strings.Contains(raw, "Password") {
		t.Fatalf("precondition failed: legacy row should be plaintext")
	}

	s.SetCredentialSealKey(secTestKey)
	got, found, err := s.GetProviderCredential(ctx, "torbox-nntp")
	if err != nil || !found || got != want {
		t.Fatalf("first read after upgrade: err=%v found=%v match=%v", err, found, got == want)
	}
	if raw := sealRawColumn(t, s, "torbox-nntp"); !strings.HasPrefix(raw, sealedCredentialPrefix) {
		t.Fatalf("row was not re-sealed in place on first read")
	}
	got2, _, err := s.GetProviderCredential(ctx, "torbox-nntp")
	if err != nil || got2 != want {
		t.Fatalf("second read after migration: err=%v match=%v", err, got2 == want)
	}
}

// A wrong key must FAIL CLOSED. Returning ciphertext-as-JSON would be far
// worse than an error: the caller would hand garbage to a provider.
func TestProviderCredentialWrongKeyFailsClosed(t *testing.T) {
	s := newSealTestStore(t)
	s.SetCredentialSealKey(secTestKey)
	ctx := context.Background()
	if err := s.SetProviderCredential(ctx, "torbox-nntp", sealTestCred()); err != nil {
		t.Fatalf("set: %v", err)
	}

	s.SetCredentialSealKey("not-the-right-key")
	got, found, err := s.GetProviderCredential(ctx, "torbox-nntp")
	if err == nil {
		t.Fatalf("expected error on wrong key, got value=%q found=%v", got, found)
	}
	if got != "" || found {
		t.Fatalf("wrong key must return no value; got %q found=%v", got, found)
	}
	if strings.Contains(err.Error(), secTestKey) || strings.Contains(err.Error(), "not-the-right-key") {
		t.Fatalf("error text leaks key material: %v", err)
	}
}

// A sealed row with sealing switched OFF must also fail closed, and must say
// why rather than returning base64 to the caller.
func TestProviderCredentialSealedRowWithNoKeyFailsClosed(t *testing.T) {
	s := newSealTestStore(t)
	s.SetCredentialSealKey(secTestKey)
	ctx := context.Background()
	if err := s.SetProviderCredential(ctx, "torbox-nntp", sealTestCred()); err != nil {
		t.Fatalf("set: %v", err)
	}

	s.SetCredentialSealKey("")
	_, found, err := s.GetProviderCredential(ctx, "torbox-nntp")
	if err == nil || found {
		t.Fatalf("expected fail-closed when a sealed row has no key")
	}
	if !strings.Contains(err.Error(), "sealed at rest") {
		t.Fatalf("error should name the cause, got: %v", err)
	}
}

// Sealing OFF must behave exactly as before: an upgrade cannot brick a
// deployment that never adopted sealed secrets.
func TestProviderCredentialSealingOffIsUnchanged(t *testing.T) {
	s := newSealTestStore(t)
	ctx := context.Background()
	want := sealTestCred()
	if err := s.SetProviderCredential(ctx, "torbox-nntp", want); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, found, err := s.GetProviderCredential(ctx, "torbox-nntp")
	if err != nil || !found || got != want {
		t.Fatalf("plaintext path changed: err=%v found=%v match=%v", err, found, got == want)
	}
	if raw := sealRawColumn(t, s, "torbox-nntp"); strings.HasPrefix(raw, sealedCredentialPrefix) {
		t.Fatalf("sealing was off but the row is marked sealed")
	}
}
