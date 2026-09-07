package mediatruth

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/probe"
)

// parsedFFprobe mirrors the subset of the real ffprobe JSON shape a
// consumer (internal/probe.ProbeFromFile, mediatruth.factsFromProbe, and a
// real Sonarr/Radarr import) actually reads.
type parsedFFprobe struct {
	Streams []struct {
		Index     int    `json:"index"`
		CodecType string `json:"codec_type"`
		CodecName string `json:"codec_name"`
		Width     int    `json:"width"`
		Height    int    `json:"height"`
		Tags      struct {
			Language string `json:"language"`
		} `json:"tags"`
	} `json:"streams"`
	Format struct {
		FormatName string `json:"format_name"`
		Duration   string `json:"duration"`
		BitRate    string `json:"bit_rate"`
		Size       string `json:"size"`
	} `json:"format"`
}

func TestSynthesizeFFprobeJSON_ValidAlways(t *testing.T) {
	cases := []Facts{
		{},
		{Container: ContainerMatroska, DurationMS: 12500, BitRate: 4_000_000, VideoCodec: "h264", Width: 1920, Height: 1080, AudioCodec: "aac", AudioLangs: []string{"eng", "jpn"}, SubtitleLangs: []string{"eng"}, SizeBytes: 123456},
		{Container: ContainerMP4, VideoCodec: "hevc"},
		{AudioCodec: "ac3"},
	}
	for i, f := range cases {
		raw := SynthesizeFFprobeJSON(f)
		var out parsedFFprobe
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			t.Fatalf("case %d: output is not valid JSON: %v\nraw=%s", i, err, raw)
		}
	}
}

func TestSynthesizeFFprobeJSON_RoundTripFromRealProbeShape(t *testing.T) {
	// factsFromProbe reads VideoCodec/Width/Height/AudioCodec/BitRate/
	// DurationSecs from the ProbeResult's own typed fields (as
	// internal/probe.ProbeFromFile already populates them from the same
	// JSON) and only parses pr.JSON itself for per-stream language tags --
	// mirror that division here so this is a genuine round trip through
	// factsFromProbe, not just a JSON-shape check.
	pr := &probe.ProbeResult{
		JSON:         fakeProbeJSON,
		VideoCodec:   "h264",
		Width:        1920,
		Height:       1080,
		AudioCodec:   "aac",
		DurationSecs: 12.5,
		BitRate:      4_000_000,
	}
	facts := factsFromProbe(pr)

	raw := SynthesizeFFprobeJSON(facts)
	var out parsedFFprobe
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}

	var video, audio bool
	for _, s := range out.Streams {
		if s.CodecType == "video" {
			video = true
			if s.CodecName != "h264" || s.Width != 1920 || s.Height != 1080 {
				t.Errorf("video stream wrong: %+v", s)
			}
		}
		if s.CodecType == "audio" {
			audio = true
		}
	}
	if !video {
		t.Error("expected a video stream")
	}
	if !audio {
		t.Error("expected an audio stream")
	}
}

func TestSynthesizeFFprobeJSON_NoSecretMaterial(t *testing.T) {
	f := Facts{Container: ContainerMatroska, DurationMS: 1000, VideoCodec: "h264", Width: 1280, Height: 720}
	raw := SynthesizeFFprobeJSON(f)
	for _, forbidden := range []string{"http://", "https://", "tok=", "Authorization", "token"} {
		if strings.Contains(raw, forbidden) {
			t.Errorf("synthesized JSON unexpectedly contains %q: %s", forbidden, raw)
		}
	}
}

func TestSynthesizeFFprobeJSON_EmptyFactsStillParseable(t *testing.T) {
	raw := SynthesizeFFprobeJSON(Facts{})
	var out parsedFFprobe
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("empty facts must still produce valid JSON: %v", err)
	}
	if len(out.Streams) != 0 {
		t.Errorf("expected no streams for empty facts, got %d", len(out.Streams))
	}
}
