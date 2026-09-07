package torbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/darkharrbor/darkharrbor/internal/provider"
)

// UsenetProviderAccount holds TorBox NNTP News Server credentials
// (v9.0.0, "Brings back NNTP News Server"). This is a real, standard NNTP
// endpoint TorBox runs for Pro-plan accounts, distinct from TorBox's own
// usenet-download handling (createusenetdownload/mylist) — downloads made
// directly against this server bypass TorBox's dashboard/queue entirely.
//
// Per REDESIGN §16's multi-provider principle: this struct and the fetch
// below are the ONLY TorBox-specific code in this feature. Once the
// credentials are in hand, main.go hands them to the same global
// internal/nntp pool used for Newshosting — nothing downstream of that
// handoff knows or cares that the source is TorBox.
type UsenetProviderAccount struct {
	Host        string
	Port        int
	TLS         bool
	Connections int
	Username    string
	Password    string
}

// maskedPasswordPlaceholder is the literal value TorBox returns in the
// `password` field of GET /usenet/provider/account once the real value has
// already been revealed once. Live-confirmed 2026-07-02: the FIRST call
// after account provisioning returns a real password; every call after
// that — even immediately, even from a different process — returns exactly
// this 8-asterisk placeholder instead. Only POST .../resetpw reveals a real
// value again, and it does so by generating a brand-new one (invalidating
// whatever was live before). Corrects an earlier, incorrect assumption
// (from the first live probe, which happened to be the one-time real-value
// call) that this endpoint returns the real password on every call.
const maskedPasswordPlaceholder = "********"

// IsMaskedPassword reports whether a password value returned by
// GetUsenetProviderAccount is the masked placeholder rather than a usable
// real credential.
func IsMaskedPassword(password string) bool {
	return password == maskedPasswordPlaceholder
}

// GetUsenetProviderAccount fetches the account's News Server credentials via
// GET /api/usenet/provider/account. Live-confirmed (2026-07-02) behavior:
//   - This call auto-provisions the News Server account on first use (the
//     response `detail` reads "Usenet Server account created successfully"
//     even when no account previously existed).
//   - The real password is only ever returned by the FIRST successful call
//     after provisioning (or after a resetpw). Every subsequent call to
//     THIS endpoint returns the masked placeholder ("********") — check
//     IsMaskedPassword on the result before treating Password as usable.
//     Callers must persist a real value the first time they see one; this
//     endpoint cannot be relied on as a repeatable credential source.
//   - Username is the account's Auth ID (UUID) — the same identifier used
//     for T3.
//
// Gated by the caller: this should only be attempted when NewsServerCapable
// (plan.go) reports true for the account's discovered plan. TorBox itself
// will presumably reject the call for non-Pro accounts, but the capability
// check happens before ever making this request so a non-Pro account never
// generates a confusing TorBox-side error at startup.
func (c *HTTPClient) GetUsenetProviderAccount(ctx context.Context) (*UsenetProviderAccount, error) {
	return c.usenetProviderAccountRequest(ctx, http.MethodGet, "/api/usenet/provider/account")
}

// ResetUsenetProviderPassword rotates the News Server password via
// POST /api/usenet/provider/account/resetpw and returns the fresh, real
// (non-masked) credentials. This INVALIDATES whatever password was
// previously live for this account — call it only when there is genuinely
// no other way to obtain a usable credential (no persisted copy exists, and
// GetUsenetProviderAccount returned the masked placeholder), never
// routinely or in a retry loop. Every call rotates the password again.
func (c *HTTPClient) ResetUsenetProviderPassword(ctx context.Context) (*UsenetProviderAccount, error) {
	return c.usenetProviderAccountRequest(ctx, http.MethodPost, "/api/usenet/provider/account/resetpw")
}

func (c *HTTPClient) usenetProviderAccountRequest(ctx context.Context, method, path string) (*UsenetProviderAccount, error) {
	env, err := c.do(ctx, provider.OpQuery, method, path, nil, "")
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if env == nil {
		return nil, fmt.Errorf("%s: empty response envelope", path)
	}
	if !env.Success {
		return nil, fmt.Errorf("%s: %v", path, env.Error)
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return nil, fmt.Errorf("%s: empty data", path)
	}

	var data struct {
		Host        string `json:"host"`
		Port        int    `json:"port"`
		SSL         bool   `json:"ssl"`
		Connections int    `json:"connections"`
		Username    string `json:"username"`
		Password    string `json:"password"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return nil, fmt.Errorf("%s decode: %w", path, err)
	}
	if data.Host == "" || data.Username == "" || data.Password == "" {
		return nil, fmt.Errorf("%s: incomplete credentials in response", path)
	}

	return &UsenetProviderAccount{
		Host:        data.Host,
		Port:        data.Port,
		TLS:         data.SSL,
		Connections: data.Connections,
		Username:    data.Username,
		Password:    data.Password,
	}, nil
}
