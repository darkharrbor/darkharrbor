package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/util"
)

func privateSecretsDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestUpdateSealedSecretsPreservesUnrelatedValues(t *testing.T) {
	store, err := parseSecretLines([]byte("KEEP=unchanged\nREPLACE=old\n"))
	if err != nil {
		t.Fatal(err)
	}
	store.sealKey = util.Redacted("test-password")
	path := filepath.Join(privateSecretsDir(t), "secrets.sealed")
	existing, err := util.SealSecrets([]byte("KEEP=unchanged\nREPLACE=old\n"), "test-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, existing, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := UpdateSealedSecrets(path, store, map[string]string{"REPLACE": "new", "ADD": "value"}); err != nil {
		t.Fatal(err)
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := util.UnsealSecrets(blob, "test-password")
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseSecretLines(plain)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"KEEP": "unchanged", "REPLACE": "new", "ADD": "value"} {
		if value, ok := got.Lookup(key); !ok || value != want {
			t.Fatalf("%s=%q,%t want %q", key, value, ok, want)
		}
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%v err=%v", info.Mode().Perm(), err)
	}
}

func TestUpdateSealedSecretsRejectsLineInjection(t *testing.T) {
	store, err := parseSecretLines([]byte("KEEP=unchanged\n"))
	if err != nil {
		t.Fatal(err)
	}
	store.sealKey = util.Redacted("test-password")
	path := filepath.Join(privateSecretsDir(t), "secrets.sealed")
	existing, err := util.SealSecrets([]byte("KEEP=unchanged\n"), "test-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, existing, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := UpdateSealedSecrets(path, store, map[string]string{"SAFE": "value\nINJECTED=yes"}); err == nil {
		t.Fatal("expected line-injection value to be rejected")
	}
	got, err := os.ReadFile(path)
	if err != nil || !reflect.DeepEqual(got, existing) {
		t.Fatalf("sealed file changed after rejected update: err=%v", err)
	}
}

func TestUpdateSealedSecretsRejectsWrongRetainedKeyWithoutOverwrite(t *testing.T) {
	path := filepath.Join(privateSecretsDir(t), "secrets.sealed")
	existing, err := util.SealSecrets([]byte("KEEP=unchanged\n"), "correct-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, existing, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := parseSecretLines([]byte("KEEP=unchanged\n"))
	if err != nil {
		t.Fatal(err)
	}
	store.sealKey = util.Redacted("wrong-password")
	if err := UpdateSealedSecrets(path, store, map[string]string{"ADD": "value"}); err == nil {
		t.Fatal("expected retained-key mismatch")
	}
	got, err := os.ReadFile(path)
	if err != nil || !reflect.DeepEqual(got, existing) {
		t.Fatalf("sealed file changed after wrong-key refusal: err=%v", err)
	}
}

func TestLoadSealedSecretsRequiresPrivateRegularFiles(t *testing.T) {
	dir := privateSecretsDir(t)
	sealedPath := filepath.Join(dir, "secrets.sealed")
	keyPath := filepath.Join(dir, "secrets.key")
	blob, err := util.SealSecrets([]byte("TEST_SECRET=value\n"), "test-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sealedPath, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("test-password\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARRBOR_SECRETS_FILE", sealedPath)
	t.Setenv("HARRBOR_SECRETS_UNLOCK", "keyfile")
	t.Setenv("HARRBOR_SECRETS_KEY_FILE", keyPath)
	store, err := loadSealedSecrets()
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := store.Lookup("TEST_SECRET"); !ok || value != "value" {
		t.Fatalf("loaded value=%q ok=%v", value, ok)
	}

	if err := os.Chmod(keyPath, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSealedSecrets(); err == nil {
		t.Fatal("group-readable key file was accepted")
	}
}

func TestLoadSealedSecretsRejectsSymlink(t *testing.T) {
	dir := privateSecretsDir(t)
	realPath := filepath.Join(dir, "real.sealed")
	blob, err := util.SealSecrets([]byte("TEST_SECRET=value\n"), "test-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(realPath, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(dir, "secrets.sealed")
	if err := os.Symlink(realPath, linkPath); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARRBOR_SECRETS_FILE", linkPath)
	t.Setenv("HARRBOR_SECRETS_UNLOCK", "env")
	t.Setenv("HARRBOR_SECRETS_PASSWORD", "test-password")
	if _, err := loadSealedSecrets(); err == nil {
		t.Fatal("symlinked sealed file was accepted")
	}
}

func TestVerifySealedSecretsKey(t *testing.T) {
	dir := privateSecretsDir(t)
	secretsPath := filepath.Join(dir, "secrets.sealed")
	keyPath := filepath.Join(dir, "secrets.key")
	writePair := func(plain []byte, password, key string) {
		t.Helper()
		blob, err := util.SealSecrets(plain, password)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(secretsPath, blob, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(keyPath, []byte(key+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	writePair([]byte("TEST_SECRET=value\n"), "correct-password", "correct-password")
	if err := VerifySealedSecretsKey(secretsPath, keyPath); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name     string
		plain    []byte
		password string
		key      string
	}{
		{name: "wrong key", plain: []byte("TEST_SECRET=value\n"), password: "correct-password", key: "wrong-password"},
		{name: "invalid plaintext", plain: []byte("not-an-env-line\n"), password: "correct-password", key: "correct-password"},
	} {
		t.Run(test.name, func(t *testing.T) {
			writePair(test.plain, test.password, test.key)
			if err := VerifySealedSecretsKey(secretsPath, keyPath); err == nil || err.Error() != "sealed secrets verification failed" {
				t.Fatalf("error = %v, want opaque verification failure", err)
			}
		})
	}

	writePair([]byte("TEST_SECRET=value\n"), "correct-password", "correct-password")
	blob, err := os.ReadFile(secretsPath)
	if err != nil {
		t.Fatal(err)
	}
	blob[len(blob)-1] ^= 0xff
	if err := os.WriteFile(secretsPath, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifySealedSecretsKey(secretsPath, keyPath); err == nil || err.Error() != "sealed secrets verification failed" {
		t.Fatalf("tamper error = %v, want opaque verification failure", err)
	}
}

func TestApplySecretsPreference(t *testing.T) {
	t.Run("sealed preference becomes explicit routing config", func(t *testing.T) {
		cfg := defaultConfig()
		cfg.Routing.Preference = []string{LaneTorBoxTorrent}
		store := &SecretStore{values: map[string]util.Redacted{
			"HARRBOR_PREFERENCE": util.Redacted("torbox_torrent,torbox_nzb"),
		}}

		cfg.applySecrets(store)

		want := []string{LaneTorBoxTorrent, LaneTorBoxNZB}
		if !reflect.DeepEqual(cfg.Routing.Preference, want) {
			t.Fatalf("Preference = %v, want %v", cfg.Routing.Preference, want)
		}
		if !cfg.Routing.PreferenceExplicit {
			t.Fatal("PreferenceExplicit = false, want true")
		}
	})

	t.Run("invalid sealed preference is ignored", func(t *testing.T) {
		cfg := defaultConfig()
		cfg.Routing.Preference = []string{LaneTorBoxTorrent}
		store := &SecretStore{values: map[string]util.Redacted{
			"HARRBOR_PREFERENCE": util.Redacted("not_a_lane"),
		}}

		cfg.applySecrets(store)

		want := []string{LaneTorBoxTorrent}
		if !reflect.DeepEqual(cfg.Routing.Preference, want) {
			t.Fatalf("Preference = %v, want %v", cfg.Routing.Preference, want)
		}
		if cfg.Routing.PreferenceExplicit {
			t.Fatal("PreferenceExplicit = true, want false")
		}
	})
}

func TestApplySecretsNNTPLegacyFields(t *testing.T) {
	cfg := defaultConfig()
	store := &SecretStore{values: map[string]util.Redacted{
		"HARRBOR_NNTP_HOST":        util.Redacted("news.example.net"),
		"HARRBOR_NNTP_PORT":        util.Redacted("119"),
		"HARRBOR_NNTP_TLS":         util.Redacted("false"),
		"HARRBOR_NNTP_CONNECTIONS": util.Redacted("12"),
	}}

	cfg.applySecrets(store)

	if cfg.NNTP.Host != "news.example.net" {
		t.Fatalf("NNTP.Host = %q, want news.example.net", cfg.NNTP.Host)
	}
	if cfg.NNTP.Port != 119 {
		t.Fatalf("NNTP.Port = %d, want 119", cfg.NNTP.Port)
	}
	if cfg.NNTP.TLS {
		t.Fatal("NNTP.TLS = true, want false")
	}
	if cfg.NNTP.Connections != 12 {
		t.Fatalf("NNTP.Connections = %d, want 12", cfg.NNTP.Connections)
	}
}
