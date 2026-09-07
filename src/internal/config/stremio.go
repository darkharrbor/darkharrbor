package config

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"strings"
)

const (
	StremioEdgeInternal  = "internal"
	StremioEdgeLAN       = "lan"
	StremioEdgeTailscale = "tailscale"
	StremioEdgeCustom    = "custom"
	StremioEdgeFunnel    = "funnel"
)

type StremioConfig struct {
	EdgeMode      string
	ClientAddress string
	ClientBaseURL string
	InstallToken  string
}

func ValidStremioEdgeMode(mode string) bool {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case StremioEdgeInternal, StremioEdgeLAN, StremioEdgeTailscale, StremioEdgeCustom, StremioEdgeFunnel:
		return true
	default:
		return false
	}
}

func (c StremioConfig) Enabled() bool {
	mode := strings.ToLower(strings.TrimSpace(c.EdgeMode))
	return mode != "" && mode != StremioEdgeInternal
}

func (c StremioConfig) EffectiveInstallToken(streamSecret string) string {
	if token := strings.TrimSpace(c.InstallToken); token != "" {
		return token
	}
	mac := hmac.New(sha256.New, []byte(streamSecret))
	_, _ = mac.Write([]byte("darkharrbor-stremio-install-v1"))
	return hex.EncodeToString(mac.Sum(nil))
}

func (c StremioConfig) Validate(streamSecret string) error {
	mode := strings.ToLower(strings.TrimSpace(c.EdgeMode))
	if !ValidStremioEdgeMode(mode) {
		return fmt.Errorf("HARRBOR_STREMIO_EDGE_MODE must be one of internal|lan|tailscale|custom|funnel")
	}
	if mode == StremioEdgeInternal {
		return nil
	}
	if strings.TrimSpace(c.ClientAddress) == "" {
		return fmt.Errorf("HARRBOR_STREMIO_CLIENT_ADDRESS is required when the client edge is enabled")
	}
	if _, _, err := net.SplitHostPort(strings.TrimSpace(c.ClientAddress)); err != nil {
		return fmt.Errorf("HARRBOR_STREMIO_CLIENT_ADDRESS must be a host:port listener")
	}
	u, err := ParseStremioClientBaseURL(c.ClientBaseURL)
	if err != nil {
		return fmt.Errorf("HARRBOR_STREMIO_CLIENT_BASE_URL: %w", err)
	}
	if u.Scheme != "https" && !stremioLoopbackHost(u.Hostname()) {
		return fmt.Errorf("HARRBOR_STREMIO_CLIENT_BASE_URL must use HTTPS unless its host is localhost or loopback")
	}
	if !validStremioInstallToken(c.EffectiveInstallToken(streamSecret)) {
		return fmt.Errorf("HARRBOR_STREMIO_INSTALL_TOKEN must be 32-128 URL-safe characters")
	}
	return nil
}

func ParseStremioClientBaseURL(raw string) (*url.URL, error) {
	if len(raw) == 0 || len(raw) > 2048 || strings.TrimSpace(raw) != raw {
		return nil, fmt.Errorf("must be a non-empty absolute origin of at most 2048 bytes")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return nil, fmt.Errorf("must be an absolute HTTP(S) origin without userinfo")
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("must contain only scheme and authority")
	}
	u.Path = ""
	return u, nil
}

func validStremioInstallToken(token string) bool {
	if len(token) < 32 || len(token) > 128 {
		return false
	}
	for _, r := range token {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

func stremioLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
