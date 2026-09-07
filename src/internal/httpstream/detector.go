package httpstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const DetectorVersion = 1
const maxDetectorBody = 1 << 20

var (
	ErrDetectionNoMatch        = errors.New("backend protocol not recognized")
	ErrDetectionSchemaMismatch = errors.New("backend protocol schema mismatch")
)

type stremioManifest struct {
	ID        string            `json:"id"`
	Version   string            `json:"version"`
	Resources []json.RawMessage `json:"resources"`
}

type stremioResource struct {
	Name       string   `json:"name"`
	Types      []string `json:"types"`
	IDPrefixes []string `json:"idPrefixes"`
}

type omssHealth struct {
	Spec      string          `json:"spec"`
	Version   string          `json:"version"`
	Status    string          `json:"status"`
	Endpoints json.RawMessage `json:"endpoints"`
}

// DetectBackend probes only protocol identity surfaces. It does not perform
// search, resolve, source preflight, or source-CDN classification.
func DetectBackend(ctx context.Context, client *http.Client, baseURL string) (string, error) {
	if client == nil {
		return "", errors.New("detect backend: nil client")
	}
	base := strings.TrimRight(baseURL, "/")
	matched, err := probeStremio(ctx, client, base+"/manifest.json")
	if err != nil {
		return "", err
	}
	if matched {
		return "stremio", nil
	}
	matched, err = probeOMSS(ctx, client, base+"/v1")
	if err != nil {
		return "", err
	}
	if matched {
		return "omss", nil
	}
	return "", ErrDetectionNoMatch
}

func detectorGET(ctx context.Context, client *http.Client, endpoint string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("detector request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("detector request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDetectorBody+1))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("detector response read: %w", err)
	}
	if len(body) > maxDetectorBody {
		return nil, resp.StatusCode, fmt.Errorf("%w: response body exceeds limit", ErrDetectionSchemaMismatch)
	}
	return body, resp.StatusCode, nil
}

func probeStremio(ctx context.Context, client *http.Client, endpoint string) (bool, error) {
	body, status, err := detectorGET(ctx, client, endpoint)
	if err != nil {
		return false, err
	}
	if status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
		return false, nil
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusTooManyRequests || status >= 500 {
		return false, fmt.Errorf("stremio detector status %d", status)
	}
	if status < 200 || status >= 300 {
		return false, nil
	}
	var manifest stremioManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return false, nil
	}
	// A 200 response at /manifest.json is not sufficient identity. Only a
	// document claiming both Stremio manifest identity fields is classified as
	// Stremio; unrelated JSON must not prevent the subsequent OMSS probe.
	if strings.TrimSpace(manifest.ID) == "" || strings.TrimSpace(manifest.Version) == "" {
		return false, nil
	}
	for _, raw := range manifest.Resources {
		var name string
		if err := json.Unmarshal(raw, &name); err == nil {
			if name == "stream" {
				return true, nil
			}
			continue
		}
		var resource stremioResource
		if err := json.Unmarshal(raw, &resource); err == nil && resource.Name == "stream" && compatibleStremioResource(resource) {
			return true, nil
		}
	}
	return false, fmt.Errorf("%w: stremio manifest lacks compatible stream resource", ErrDetectionSchemaMismatch)
}

func compatibleStremioResource(r stremioResource) bool {
	typeOK := len(r.Types) == 0
	for _, typ := range r.Types {
		if typ == "movie" || typ == "series" {
			typeOK = true
		}
	}
	prefixOK := len(r.IDPrefixes) == 0
	for _, prefix := range r.IDPrefixes {
		if prefix == "tt" {
			prefixOK = true
		}
	}
	return typeOK && prefixOK
}

func probeOMSS(ctx context.Context, client *http.Client, endpoint string) (bool, error) {
	body, status, err := detectorGET(ctx, client, endpoint)
	if err != nil {
		return false, err
	}
	if status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
		return false, nil
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusTooManyRequests || status >= 500 {
		return false, fmt.Errorf("omss detector status %d", status)
	}
	if status < 200 || status >= 300 {
		return false, nil
	}
	var health omssHealth
	if err := json.Unmarshal(body, &health); err != nil {
		return false, nil
	}
	if health.Spec != "omss" {
		return false, nil
	}
	if strings.TrimSpace(health.Version) == "" || len(health.Endpoints) == 0 {
		return false, fmt.Errorf("%w: invalid omss health identity", ErrDetectionSchemaMismatch)
	}
	switch health.Status {
	case "operational", "degraded":
		return true, nil
	case "down":
		return false, errors.New("omss backend reports down")
	default:
		return false, fmt.Errorf("%w: invalid omss status", ErrDetectionSchemaMismatch)
	}
}
