package httpstream

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/darkharrbor/darkharrbor/internal/mediaidentity"
)

// GrabEnvelope is the stateless signed grab token payload (D1/HS-1.3).
// /torznab/http-stream results carry /grab/http/{token} enclosures; the
// token round-trips arr → /grab/http (302 → synthetic magnet) → qBit add,
// where the HMAC is validated before an item is created. The envelope is
// the enqueue integrity boundary (D15) and carries no server state, so it
// is restart-safe by construction.
type GrabEnvelope struct {
	V     int        `json:"v"`
	Key   ResolveKey `json:"key"`
	Hash  string     `json:"hash"`  // domain-separated synthetic 40-hex digest of Key
	Title string     `json:"title"` // display title (arr-parseable release name)
	// ProviderIdentity is authoritative Arr catalog context, never an
	// upstream source descriptor or credential (ID-02).
	ProviderIdentity *mediaidentity.ProviderIdentity `json:"provider_identity,omitempty"`
}

// MaxGrabTokenBytes caps the encoded token size before any parse work (HS-1.3).
const MaxGrabTokenBytes = 2048

var b64 = base64.RawURLEncoding

// EncodeGrabToken signs env with secret and returns the wire token:
// base64url(payload) + "." + base64url(HMAC-SHA256(secret, payload)).
func EncodeGrabToken(secret string, env GrabEnvelope) (string, error) {
	if secret == "" {
		return "", NewError(ClassInvalidKey, "grab token signing secret not configured")
	}
	env.V = ResolveKeyVersion
	digest, err := env.Key.Digest()
	if err != nil {
		return "", err
	}
	env.Hash = digest
	if err := mediaidentity.Validate(env.ProviderIdentity); err != nil {
		return "", NewError(ClassInvalidKey, "grab provider identity invalid")
	}
	payload, err := json.Marshal(env)
	if err != nil {
		return "", NewError(ClassInvalidKey, "encode grab envelope failed")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	tok := b64.EncodeToString(payload) + "." + b64.EncodeToString(mac.Sum(nil))
	if len(tok) > MaxGrabTokenBytes {
		return "", NewError(ClassInvalidKey, "grab token exceeds size cap")
	}
	return tok, nil
}

// DecodeGrabToken validates size, HMAC, structure, and digest consistency,
// returning the envelope. All failure modes are ClassInvalidKey — callers
// must treat a bad token as a rejected submission, never a retry.
func DecodeGrabToken(secret, token string) (GrabEnvelope, error) {
	var env GrabEnvelope
	if secret == "" {
		return env, NewError(ClassInvalidKey, "grab token signing secret not configured")
	}
	if len(token) == 0 || len(token) > MaxGrabTokenBytes {
		return env, NewError(ClassInvalidKey, "grab token missing or exceeds size cap")
	}
	dot := strings.IndexByte(token, '.')
	if dot <= 0 || dot == len(token)-1 {
		return env, NewError(ClassInvalidKey, "grab token malformed")
	}
	payload, err := b64.DecodeString(token[:dot])
	if err != nil {
		return env, NewError(ClassInvalidKey, "grab token payload not base64url")
	}
	sig, err := b64.DecodeString(token[dot+1:])
	if err != nil {
		return env, NewError(ClassInvalidKey, "grab token signature not base64url")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return env, NewError(ClassInvalidKey, "grab token signature invalid")
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		return env, NewError(ClassInvalidKey, "grab envelope not valid JSON")
	}
	if err := env.Key.Validate(); err != nil {
		return env, err
	}
	if err := mediaidentity.Validate(env.ProviderIdentity); err != nil {
		return env, NewError(ClassInvalidKey, "grab provider identity invalid")
	}
	digest, err := env.Key.Digest()
	if err != nil {
		return env, err
	}
	if digest != strings.ToLower(strings.TrimSpace(env.Hash)) {
		return env, NewError(ClassInvalidKey, "grab envelope hash does not match key digest")
	}
	env.Hash = digest
	return env, nil
}
