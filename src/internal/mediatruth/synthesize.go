package mediatruth

import (
	"encoding/json"
	"fmt"
)

// SynthesizeFFprobeJSON builds ffprobe-compatible JSON text from previously
// computed Facts (SF-04) -- the exact `-show_format -show_streams
// -print_format json` shape internal/probe.ProbeFromFile already produces
// and factsFromProbe already consumes, run in reverse. It is a pure,
// deterministic, secret-free function: Facts carries no upstream URL,
// token, or credential by construction (see Facts' own doc comment), so
// nothing this function emits can either.
//
// The output is intentionally minimal: only fields Facts actually holds are
// populated, and format_name is fixed from Facts.Container (the only
// container identity mediatruth ever establishes) rather than guessed. A
// consumer parsing this the same way Sonarr/Radarr parse a real ffprobe
// import (format.duration, format.bit_rate, streams[].codec_type/
// codec_name/width/height, streams[].tags.language) gets every field a real
// probe would have supplied for those keys; fields Facts never captured
// (e.g. pix_fmt, sample_rate, per-stream bit_rate) are simply absent, the
// same shape a partial real probe already produces today for some sources.
func SynthesizeFFprobeJSON(f Facts) string {
	type tags struct {
		Language string `json:"language,omitempty"`
	}
	type stream struct {
		Index     int    `json:"index"`
		CodecType string `json:"codec_type"`
		CodecName string `json:"codec_name,omitempty"`
		Width     int    `json:"width,omitempty"`
		Height    int    `json:"height,omitempty"`
		Tags      *tags  `json:"tags,omitempty"`
	}
	type format struct {
		FormatName string `json:"format_name,omitempty"`
		Duration   string `json:"duration,omitempty"`
		BitRate    string `json:"bit_rate,omitempty"`
		Size       string `json:"size,omitempty"`
	}
	out := struct {
		Streams []stream `json:"streams"`
		Format  format   `json:"format"`
	}{}

	idx := 0
	if f.VideoCodec != "" || f.Width > 0 || f.Height > 0 {
		out.Streams = append(out.Streams, stream{
			Index: idx, CodecType: "video", CodecName: f.VideoCodec,
			Width: f.Width, Height: f.Height,
		})
		idx++
	}
	if f.AudioCodec != "" || len(f.AudioLangs) > 0 {
		var t *tags
		if len(f.AudioLangs) > 0 {
			t = &tags{Language: f.AudioLangs[0]}
		}
		out.Streams = append(out.Streams, stream{
			Index: idx, CodecType: "audio", CodecName: f.AudioCodec, Tags: t,
		})
		idx++
	}
	for _, lang := range f.SubtitleLangs {
		out.Streams = append(out.Streams, stream{
			Index: idx, CodecType: "subtitle", Tags: &tags{Language: lang},
		})
		idx++
	}

	switch f.Container {
	case ContainerMatroska:
		out.Format.FormatName = "matroska,webm"
	case ContainerMP4:
		out.Format.FormatName = "mov,mp4,m4a,3gp,3g2,mj2"
	}
	if f.DurationMS > 0 {
		out.Format.Duration = fmt.Sprintf("%.6f", float64(f.DurationMS)/1000.0)
	}
	if f.BitRate > 0 {
		out.Format.BitRate = fmt.Sprintf("%d", f.BitRate)
	}
	if f.SizeBytes > 0 {
		out.Format.Size = fmt.Sprintf("%d", f.SizeBytes)
	}

	raw, err := json.Marshal(out)
	if err != nil {
		// Facts is a plain typed struct with no cyclic or unmarshalable
		// field; Marshal cannot fail on it in practice. A defensive empty
		// object keeps the caller's contract (always valid JSON) rather
		// than panicking or returning an error type the relay would have
		// to special-case.
		return `{"streams":[],"format":{}}`
	}
	return string(raw)
}
