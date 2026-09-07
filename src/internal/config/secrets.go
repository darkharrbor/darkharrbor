package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/darkharrbor/darkharrbor/internal/secretinput"
	"github.com/darkharrbor/darkharrbor/internal/securefile"
	"github.com/darkharrbor/darkharrbor/internal/util"
)

const (
	maxSealedSecretsBytes = 16 << 20
	maxSecretsKeyBytes    = 4096
)

// VerifySealedSecretsKey confirms that an owner-private sealed store can be
// opened by an owner-private key file and contains valid KEY=VALUE plaintext.
// It deliberately returns one opaque error so callers cannot distinguish key,
// ciphertext, or plaintext failures.
func VerifySealedSecretsKey(secretsPath, keyPath string) error {
	blob, err := securefile.Read(secretsPath, maxSealedSecretsBytes)
	if err != nil {
		return errors.New("sealed secrets verification failed")
	}
	rawKey, err := securefile.Read(keyPath, maxSecretsKeyBytes)
	if err != nil {
		return errors.New("sealed secrets verification failed")
	}
	defer clear(rawKey)
	password := strings.TrimSpace(string(rawKey))
	if password == "" {
		return errors.New("sealed secrets verification failed")
	}
	plain, err := util.UnsealSecrets(blob, password)
	if err != nil {
		return errors.New("sealed secrets verification failed")
	}
	defer clear(plain)
	if _, err := ValidateSecretLines(plain); err != nil {
		return errors.New("sealed secrets verification failed")
	}
	return nil
}

// UpdateSealedSecrets atomically replaces selected keys while preserving every
// unrelated secret and the existing unlock key. Values never enter tuning.
func UpdateSealedSecrets(path string, store *SecretStore, updates map[string]string) error {
	password, ok := store.SealKey()
	if !ok {
		return fmt.Errorf("sealed secret update requires the existing unlock key")
	}
	existing, err := securefile.Read(path, maxSealedSecretsBytes)
	if err != nil {
		return fmt.Errorf("read existing sealed secrets: %w", err)
	}
	if _, err := util.UnsealSecrets(existing, password); err != nil {
		return fmt.Errorf("existing sealed secrets do not match the retained unlock key")
	}
	values := make(map[string]string, len(store.values)+len(updates))
	for key, value := range store.values {
		values[key] = value.Value()
	}
	for key, value := range updates {
		if strings.TrimSpace(key) == "" || strings.ContainsAny(key, "=\r\n") {
			return fmt.Errorf("invalid secret key")
		}
		if strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("invalid secret value for %s", key)
		}
		values[key] = strings.TrimSpace(value)
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var plain strings.Builder
	for _, key := range keys {
		fmt.Fprintf(&plain, "%s=%s\n", key, values[key])
	}
	blob, err := util.SealSecrets([]byte(plain.String()), password)
	if err != nil {
		return fmt.Errorf("seal updated secrets: %w", err)
	}
	if err := securefile.AtomicWrite(path, blob); err != nil {
		return fmt.Errorf("replace sealed secrets: %w", err)
	}
	return nil
}

//
// Provider credentials may live in an encrypted secrets file
// (util.SealSecrets format: argon2id + AES-256-GCM, env-style KEY=VALUE
// plaintext, created by `darkharrbor seal`). At startup the file is unlocked and
// its credential values are applied OVER the plaintext environment. The
// strict non-secret tuning file may override only its allowlisted knobs.
// Decrypted values are held only in the process — never exported via
// os.Setenv — so child processes (ffprobe) cannot inherit them (D-3).
// Plaintext env vars remain a fully supported legacy path.
//
// Env surface:
//
//	HARRBOR_SECRETS_FILE      path to the sealed file (absent = feature off)
//	HARRBOR_SECRETS_UNLOCK    env (default) | keyfile | prompt
//	HARRBOR_SECRETS_PASSWORD  password for unlock=env
//	HARRBOR_SECRETS_KEY_FILE  file whose trimmed contents are the password
//	                          for unlock=keyfile

// SecretStore holds decrypted sealed secrets keyed by env-style name.
type SecretStore struct {
	values map[string]util.Redacted
	// sealKey is the password that unsealed this store, RETAINED so that
	// runtime-written credentials can be sealed at rest under the same key
	// (SEC-01). loadSealedSecrets previously used it and dropped it, which
	// left the store with nothing to encrypt with. It is deliberately NOT
	// exported and is wrapped in util.Redacted so it cannot be printed by
	// accident; SealKey() is the only accessor.
	sealKey util.Redacted
}

// SealKey returns the password that unsealed this store, and whether one is
// available. A nil store or an empty key means sealed secrets are OFF, and
// callers MUST degrade rather than fail: refusing to boot without sealing
// would brick every deployment that never adopted it.
func (s *SecretStore) SealKey() (string, bool) {
	if s == nil {
		return "", false
	}
	v := s.sealKey.Value()
	return v, v != ""
}

// Lookup returns the sealed value for key, if present.
func (s *SecretStore) Lookup(key string) (string, bool) {
	if s == nil {
		return "", false
	}
	v, ok := s.values[key]
	return v.Value(), ok
}

// loadSealedSecrets reads HARRBOR_SECRETS_FILE per the unlock mode and
// returns the parsed store, or (nil, nil) when the feature is unused.
// Every error names the FILE, never any secret value.
func loadSealedSecrets() (*SecretStore, error) {
	path := strings.TrimSpace(os.Getenv("HARRBOR_SECRETS_FILE"))
	if path == "" {
		return nil, nil
	}
	blob, err := securefile.Read(path, maxSealedSecretsBytes)
	if err != nil {
		return nil, fmt.Errorf("sealed secrets file %q: %w", path, err)
	}

	mode := strings.ToLower(strings.TrimSpace(os.Getenv("HARRBOR_SECRETS_UNLOCK")))
	if mode == "" {
		mode = "env"
	}
	var password string
	switch mode {
	case "env":
		password = os.Getenv("HARRBOR_SECRETS_PASSWORD")
		if password == "" {
			return nil, fmt.Errorf("sealed secrets file %q: HARRBOR_SECRETS_UNLOCK=env but HARRBOR_SECRETS_PASSWORD is empty", path)
		}
	case "keyfile":
		keyPath := strings.TrimSpace(os.Getenv("HARRBOR_SECRETS_KEY_FILE"))
		if keyPath == "" {
			return nil, fmt.Errorf("sealed secrets file %q: HARRBOR_SECRETS_UNLOCK=keyfile but HARRBOR_SECRETS_KEY_FILE is empty", path)
		}
		raw, err := securefile.Read(keyPath, maxSecretsKeyBytes)
		if err != nil {
			return nil, fmt.Errorf("sealed secrets file %q: read key file: %w", path, err)
		}
		password = strings.TrimSpace(string(raw))
		if password == "" {
			return nil, fmt.Errorf("sealed secrets file %q: key file %q is empty", path, keyPath)
		}
	case "prompt":
		reader := bufio.NewReader(os.Stdin)
		line, err := secretinput.ReadLine(os.Stderr, fmt.Sprintf("Dark Harrbor secrets password for %s: ", path), os.Stdin, reader)
		if err != nil && line == "" {
			return nil, fmt.Errorf("sealed secrets file %q: read password from stdin: %w", path, err)
		}
		password = strings.TrimSpace(line)
		if password == "" {
			return nil, fmt.Errorf("sealed secrets file %q: empty password from prompt", path)
		}
	default:
		return nil, fmt.Errorf("sealed secrets file %q: unknown HARRBOR_SECRETS_UNLOCK mode %q (env|keyfile|prompt)", path, mode)
	}

	plain, err := util.UnsealSecrets(blob, password)
	if err != nil {
		return nil, fmt.Errorf("sealed secrets file %q: %w", path, err)
	}
	store, err := parseSecretLines(plain)
	if err != nil {
		return nil, fmt.Errorf("sealed secrets file %q: %w", path, err)
	}
	store.sealKey = util.Redacted(password)
	return store, nil
}

// parseSecretLines parses env-style KEY=VALUE lines (blank lines and
// #-comments ignored).
func parseSecretLines(plain []byte) (*SecretStore, error) {
	store := &SecretStore{values: map[string]util.Redacted{}}
	for i, line := range strings.Split(string(plain), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return nil, fmt.Errorf("malformed KEY=VALUE on line %d", i+1)
		}
		store.values[key] = util.Redacted(strings.TrimSpace(val))
	}
	return store, nil
}

// ValidateSecretLines parses env-style KEY=VALUE plaintext and returns the
// number of secret entries, for `darkharrbor seal` pre-flight validation.
func ValidateSecretLines(plain []byte) (int, error) {
	store, err := parseSecretLines(plain)
	if err != nil {
		return 0, err
	}
	return len(store.values), nil
}

// applySecrets maps known sealed keys onto Config fields (sealed wins over
// plaintext env). Unknown keys stay available through Config.Lookup for
// per-provider factory configuration.
func (c *Config) applySecrets(store *SecretStore) {
	if store == nil {
		return
	}
	c.Secrets = store
	apply := func(key string, dst *string) {
		if v, ok := store.Lookup(key); ok && v != "" {
			*dst = v
		}
	}
	apply("HARRBOR_TORBOX_API_TOKEN", &c.TorBox.APIToken)
	apply("HARRBOR_QBIT_PASSWORD", &c.Auth.QBitPassword)
	apply("HARRBOR_SAB_API_KEY", &c.Auth.SABAPIKey)
	apply("HARRBOR_SAB_NZB_KEY", &c.Auth.SABNZBKey)
	apply("HARRBOR_STREAM_SECRET", &c.Server.StreamSecret)
	apply("HARRBOR_STREMIO_INSTALL_TOKEN", &c.Stremio.InstallToken)
	apply("HARRBOR_MEDIAFLOW_PASSWORD", &c.MediaFlow.Password)
	apply("HARRBOR_PROWLARR_BASE_URL", &c.Prowlarr.BaseURL)
	apply("HARRBOR_PROWLARR_API_KEY", &c.Prowlarr.APIKey)
	apply("HARRBOR_TMDB_API_KEY", &c.HTTPStream.TMDBAPIKey)
	apply("HARRBOR_NNTP_HOST", &c.NNTP.Host)
	apply("HARRBOR_NNTP_USERNAME", &c.NNTP.Username)
	apply("HARRBOR_NNTP_PASSWORD", &c.NNTP.Password)
	if v, ok := store.Lookup("HARRBOR_NNTP_PORT"); ok && v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			c.NNTP.Port = n
		}
	}
	if v, ok := store.Lookup("HARRBOR_NNTP_TLS"); ok && v != "" {
		c.NNTP.TLS = strings.EqualFold(strings.TrimSpace(v), "true") || strings.TrimSpace(v) == "1"
	}
	if v, ok := store.Lookup("HARRBOR_NNTP_CONNECTIONS"); ok && v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			c.NNTP.Connections = n
		}
	}
	if v, ok := store.Lookup("HARRBOR_PREFERENCE"); ok && v != "" {
		pref, explicit := applyPreference(v)
		if explicit {
			c.Routing.Preference = slices.Clone(pref)
			c.Routing.PreferenceExplicit = true
		}
	}
}

// Lookup resolves a configuration key. The configure-managed non-secret file
// wins for its strict tuning allowlist; all other values retain sealed-first
// precedence over the plaintext environment.
func (c *Config) Lookup(key string) (string, bool) {
	if c != nil {
		if v := strings.TrimSpace(c.tuning[key]); v != "" {
			return v, true
		}
	}
	if v, ok := c.Secrets.Lookup(key); ok && v != "" {
		return v, true
	}
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v, true
	}
	return "", false
}
