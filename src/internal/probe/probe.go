package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"time"
)

// Prober runs byte-range probes against TorBox CDN URLs and writes stub video
type Prober struct {
	log     *slog.Logger
	timeout time.Duration
}

func New(log *slog.Logger, timeout time.Duration) *Prober {
	return &Prober{log: log, timeout: timeout}
}

type ProbeResult struct {
	JSON         string
	VideoCodec   string
	Width        int
	Height       int
	AudioCodec   string
	DurationSecs float64
	BitRate      int64
}

// RangeProbe issues a Range GET for the first probeBytes bytes of cdnURL,
// writes them to a temp file, runs ffprobe against it, and returns parsed
// stream metadata. The temp file is deleted after ffprobe exits.
// cdnURL may be a TorBox requestdl permalink — the 307 redirect to the CDN
// is followed automatically.
func (p *Prober) RangeProbe(ctx context.Context, cdnURL string, probeBytes int64) (*ProbeResult, error) {
	if probeBytes <= 0 {
		return nil, fmt.Errorf("range probe: probe bytes must be positive")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cdnURL, nil)
	if err != nil {
		return nil, fmt.Errorf("range probe: invalid request URL")
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", probeBytes-1))

	httpClient := &http.Client{
		Timeout:       p.timeout,
		CheckRedirect: nil, // follow redirects (TorBox requestdl → 307 → CDN)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("range probe: get: %w", ctx.Err())
		}
		return nil, fmt.Errorf("range probe: request failed")
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return nil, fmt.Errorf("range probe: unexpected status %d", resp.StatusCode)
	}

	tmpFile, err := os.CreateTemp("", "darkharrbor-probe-*")
	if err != nil {
		return nil, fmt.Errorf("range probe: create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	written, err := io.Copy(tmpFile, io.LimitReader(resp.Body, probeBytes+1))
	if err != nil {
		_ = tmpFile.Close()
		return nil, fmt.Errorf("range probe: write temp file: %w", err)
	}
	if written > probeBytes {
		_ = tmpFile.Close()
		return nil, fmt.Errorf("range probe: response exceeded %d-byte limit", probeBytes)
	}
	if err := tmpFile.Close(); err != nil {
		return nil, fmt.Errorf("range probe: close temp file: %w", err)
	}

	return p.ProbeFromFile(ctx, tmpPath)
}

// ProbeFromFile runs ffprobe against an existing file at path and returns
// parsed stream metadata. Used by RangeProbe (torrent path) and by the NZB
// ingest path after NNTPProvider.Probe() writes a temp file.
func (p *Prober) ProbeFromFile(ctx context.Context, path string) (*ProbeResult, error) {
	jsonBytes, err := runFfprobe(ctx, p.timeout, path)
	if err != nil {
		return nil, fmt.Errorf("probe from file: ffprobe: %w", err)
	}

	result := &ProbeResult{JSON: string(jsonBytes)}

	var ffOut struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
			CodecName string `json:"codec_name"`
			Width     int    `json:"width"`
			Height    int    `json:"height"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
			BitRate  string `json:"bit_rate"`
		} `json:"format"`
	}
	if err := json.Unmarshal(jsonBytes, &ffOut); err == nil {
		if d, err := strconv.ParseFloat(ffOut.Format.Duration, 64); err == nil {
			result.DurationSecs = d
		}
		if b, err := strconv.ParseInt(ffOut.Format.BitRate, 10, 64); err == nil {
			result.BitRate = b
		}
		for _, s := range ffOut.Streams {
			if s.CodecType == "video" && result.VideoCodec == "" {
				result.VideoCodec = s.CodecName
				result.Width = s.Width
				result.Height = s.Height
			}
			if s.CodecType == "audio" && result.AudioCodec == "" {
				result.AudioCodec = s.CodecName
			}
		}
	}

	return result, nil
}

func runFfprobe(ctx context.Context, timeout time.Duration, path string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffprobe",
		"-v", "quiet",
		"-print_format", "json",
		"-show_streams",
		"-show_format",
		path,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("ffprobe timed out after %v", timeout)
		}
		return nil, fmt.Errorf("ffprobe: %s: %w", stderr.String(), err)
	}
	return stdout.Bytes(), nil
}
