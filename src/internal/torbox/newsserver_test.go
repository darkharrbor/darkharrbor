package torbox

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) *HTTPClient {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewHTTPClient(nil, srv.URL, "test-token", "darkharrbor-test", 5*time.Second, nil)
}

func TestGetUsenetProviderAccount_Success(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/usenet/provider/account" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("unexpected method: %s", r.Method)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("missing/incorrect auth header: %s", r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"error":   nil,
			"detail":  "Usenet Server account created successfully.",
			"data": map[string]any{
				"host":        "nntp.torbox.app",
				"port":        563,
				"ssl":         true,
				"connections": 10,
				"username":    "00000000-0000-4000-8000-000000000000",
				"password":    "supersecret",
			},
		})
	})

	acct, err := c.GetUsenetProviderAccount(context.Background())
	if err != nil {
		t.Fatalf("GetUsenetProviderAccount() error = %v", err)
	}
	if acct.Host != "nntp.torbox.app" {
		t.Errorf("Host = %q, want nntp.torbox.app", acct.Host)
	}
	if acct.Port != 563 {
		t.Errorf("Port = %d, want 563", acct.Port)
	}
	if !acct.TLS {
		t.Error("TLS = false, want true")
	}
	if acct.Connections != 10 {
		t.Errorf("Connections = %d, want 10", acct.Connections)
	}
	if acct.Username != "00000000-0000-4000-8000-000000000000" {
		t.Errorf("Username = %q, want the auth_id UUID", acct.Username)
	}
	if acct.Password != "supersecret" {
		t.Errorf("Password not decoded correctly")
	}
	if IsMaskedPassword(acct.Password) {
		t.Error("IsMaskedPassword(supersecret) = true, want false")
	}
}

// TestGetUsenetProviderAccount_MaskedOnRepeatCall reproduces the real
// behavior found live 2026-07-02: every call after the first returns the
// literal 8-asterisk placeholder instead of a usable password. Callers must
// detect this via IsMaskedPassword rather than treating any 200 response as
// carrying a real credential.
func TestGetUsenetProviderAccount_MaskedOnRepeatCall(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"error":   nil,
			"detail":  "ok",
			"data": map[string]any{
				"host":        "nntp.torbox.app",
				"port":        563,
				"ssl":         true,
				"connections": 10,
				"username":    "00000000-0000-4000-8000-000000000000",
				"password":    "********",
			},
		})
	})

	acct, err := c.GetUsenetProviderAccount(context.Background())
	if err != nil {
		t.Fatalf("GetUsenetProviderAccount() error = %v", err)
	}
	if !IsMaskedPassword(acct.Password) {
		t.Error("IsMaskedPassword(********) = false, want true")
	}
}

func TestResetUsenetProviderPassword_Success(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/usenet/provider/account/resetpw" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method: %s, want POST", r.Method)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"error":   nil,
			"detail":  "Usenet Server password reset successfully.",
			"data": map[string]any{
				"host":        "nntp.torbox.app",
				"port":        563,
				"ssl":         true,
				"connections": 10,
				"username":    "00000000-0000-4000-8000-000000000000",
				"password":    "freshrealpassword123",
			},
		})
	})

	acct, err := c.ResetUsenetProviderPassword(context.Background())
	if err != nil {
		t.Fatalf("ResetUsenetProviderPassword() error = %v", err)
	}
	if IsMaskedPassword(acct.Password) {
		t.Error("reset should always return a real, non-masked password")
	}
	if acct.Password != "freshrealpassword123" {
		t.Errorf("Password = %q, want freshrealpassword123", acct.Password)
	}
}

func TestGetUsenetProviderAccount_APIError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": false,
			"error":   "PLAN_NOT_ELIGIBLE",
			"detail":  "News Server requires a Pro plan.",
		})
	})

	if _, err := c.GetUsenetProviderAccount(context.Background()); err == nil {
		t.Fatal("expected error for non-Pro account, got nil")
	}
}

func TestGetUsenetProviderAccount_IncompleteResponse(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"error":   nil,
			"detail":  "ok",
			"data": map[string]any{
				"host": "nntp.torbox.app",
				// username/password missing — should be treated as an error,
				// not silently returned as a usable-looking empty-credential account.
			},
		})
	})

	if _, err := c.GetUsenetProviderAccount(context.Background()); err == nil {
		t.Fatal("expected error for incomplete credentials, got nil")
	}
}
