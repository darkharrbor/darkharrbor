package arrsetup

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// DetectAppType authenticates to the common v3 system-status endpoint used by
// Sonarr and Radarr. It lets legacy environment-declared targets participate in
// reconciliation and diagnostics without guessing their type from a name.
func DetectAppType(ctx context.Context, baseURL, apiKey string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/api/v3/system/status", nil)
	if err != nil {
		return "", fmt.Errorf("build status request: %w", err)
	}
	req.Header.Set("X-Api-Key", strings.TrimSpace(apiKey))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("status request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status request returned HTTP %d", resp.StatusCode)
	}
	var status struct {
		AppName string `json:"appName"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&status); err != nil {
		return "", fmt.Errorf("decode status response: %w", err)
	}
	appType := strings.ToLower(strings.TrimSpace(status.AppName))
	if appType != "sonarr" && appType != "radarr" {
		return "", fmt.Errorf("status response is not Sonarr or Radarr")
	}
	return appType, nil
}
