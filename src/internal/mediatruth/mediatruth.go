// Package mediatruth owns DarkHarrbor's lane-neutral bounded media truth and
// compact seek index (HR5.2; HTTP-plan HR5.2, torrent T10, NNTP N11c).
//
// It consumes ONLY the TS-1.1 bytesource.ByteSource contract, so the same
// bytes yield the same facts and the same index whether they came from NNTP
// segments, a debrid CDN window, or an HTTP-stream relay. There is exactly one
// implementation of "what is this file, and where do I seek to" in the program
// (master plan §3, "Media truth and seek index"); TS-3.3, NS-5.3, TS-1.4 and
// the HTTP adapters consume this surface rather than growing parallel ones.
//
// Bounded by construction (LC-01, C0.3, DG-09): every read is charged against
// an explicit byte budget, every read takes the caller's context, no goroutine
// is spawned, and any temporary file is removed on every exit path. This is
// preparation work — it is never full-file pre-verification (§6.2), and its
// facts never rerank or substitute a release (LC-08).
//
// Secret discipline (DG-04/LC-02): nothing this package emits — facts, index,
// or error strings — carries an upstream URL, token, credential, or header.
// Error text is drawn from a fixed vocabulary defined here.
//
// This row establishes the surface and its parsers. It has no live call site
// by design; persistence and wiring belong to the consuming rows.
package mediatruth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/bytesource"
	"github.com/darkharrbor/darkharrbor/internal/probe"
)

// Container names are stable, lowercase, and lane-independent.
const (
	ContainerMatroska = "matroska"
	ContainerMP4      = "mp4"
)

// Index sources name which container structure produced the seek index.
const (
	IndexSourceMatroskaCues = "matroska_cues"
	IndexSourceMP4Moov      = "mp4_moov"
)

// Fatal errors. A caller distinguishes "this source cannot be analyzed" from
// "analysis succeeded but one half is absent" — the latter is reported in
// Result.FactsError / Result.IndexError, never by returning a partial Result
// with a nil error and fabricated contents.
var (
	// ErrNoSource is returned for a nil ByteSource.
	ErrNoSource = errors.New("mediatruth: nil source")
	// ErrBudgetExhausted is returned when the configured byte budget cannot
	// cover a required read. It is a bound, not a source failure.
	ErrBudgetExhausted = errors.New("mediatruth: byte budget exhausted")
	// ErrUnsupportedContainer is returned when the leading bytes match no
	// container this package can reason about.
	ErrUnsupportedContainer = errors.New("mediatruth: unsupported container")
	// ErrTruncatedSource is returned when the source ends before a structure
	// its own headers declared.
	ErrTruncatedSource = errors.New("mediatruth: source truncated before declared structure")
)

// Fixed, secret-free reason vocabulary for a half-absent result.
const (
	reasonNoIndexStructure = "no seek index structure within bounded window"
	reasonIndexUnparseable = "seek index structure present but unparseable"
	reasonIndexBudget      = "seek index beyond byte budget"
	reasonIndexTruncated   = "source truncated before seek index"
	reasonProbeFailed      = "media probe did not produce facts"
	reasonProbeUnavailable = "no media prober configured"
	// reasonProbePrepFailed is distinct from reasonProbeFailed (TS-3.3): it
	// covers a LOCAL temp-file preparation error (create/write/truncate/sync)
	// on the DarkHarrbor host, never a prober verdict about the source bytes
	// themselves. The two must never collapse into one reason -- a consumer
	// routing a genuine broken-media signal into repair (Result.ProbeFailed)
	// would otherwise re-add or eventually blacklist a perfectly good item
	// on nothing more than a full disk or a permissions error.
	reasonProbePrepFailed = "local probe preparation failed"
	reasonSourceRead      = "source read failed"
	reasonSourceMutated   = "source representation changed mid-analysis"
	// reasonIndexPointerDrift is specific to a lane whose offset table is
	// partly estimated (Caps().ExactSize == false): a pointer the container
	// expresses in true-file coordinates cannot be resolved exactly, so the
	// targeted read lands off-structure. It is reported distinctly from a
	// genuinely absent index so a consumer can tell "this file has no index"
	// from "this lane cannot reach the index yet".
	reasonIndexPointerDrift = "seek index pointer unresolvable on an estimated-offset source"
)

// Default bounds. They are package constants rather than configuration: this
// row introduces no config key (HR7.1 enumerates the program's env surface,
// and the consuming rows own their own wiring). HeadBytes matches the shipped
// Probe.ProbeBytes default so a wired consumer transfers no more than today's
// resolve-time probe already does.
const (
	DefaultHeadBytes       = 1 << 20 // 1 MiB
	DefaultTailBytes       = 1 << 20 // 1 MiB
	DefaultIndexBytes      = 2 << 20 // 2 MiB of targeted index reads
	DefaultMaxIndexEntries = 2048
	DefaultMaxTopLevelBox  = 64
	DefaultProbeTimeout    = 30 * time.Second
	// defaultSparseMaxBytes caps the apparent size of the sparse temp file
	// used to give a prober true byte offsets for a tail-located header. A
	// source larger than this is probed head-only rather than risking a
	// large allocation on a filesystem without sparse support.
	defaultSparseMaxBytes = 64 << 30 // 64 GiB
)

// ProbeFunc runs a media prober against a local file path. It is injected so
// deterministic tests never depend on an ffprobe binary being present, and so
// the program keeps exactly one ffprobe invoker (internal/probe).
type ProbeFunc func(ctx context.Context, path string) (*probe.ProbeResult, error)

// Options bound one Analyze call. The zero value is valid and uses defaults.
type Options struct {
	// HeadBytes caps the leading read. <=0 uses DefaultHeadBytes.
	HeadBytes int64
	// TailBytes caps the trailing read used for tail-located headers
	// (MP4 moov-at-end, Matroska Cues at EOF). 0 uses DefaultTailBytes;
	// a NEGATIVE value disables tail reads entirely.
	TailBytes int64
	// IndexBytes caps targeted index reads made at an offset the container's
	// own directory pointed to. 0 uses DefaultIndexBytes; a NEGATIVE value
	// disables them.
	IndexBytes int64
	// MaxIndexEntries caps the retained index size. <=0 uses the default.
	// A larger keyframe set is evenly downsampled and marked incomplete.
	MaxIndexEntries int
	// MaxTopLevelBoxes caps the MP4 top-level box walk. <=0 uses the default.
	MaxTopLevelBoxes int
	// Probe runs the media prober. nil uses internal/probe's ffprobe wrapper.
	Probe ProbeFunc
	// ProbeTimeout bounds one prober invocation when Probe is nil.
	ProbeTimeout time.Duration
	// TempDir holds the temporary probe file. Empty uses the system default.
	TempDir string
	// SkipProbe builds the index only and reports no facts. Useful for a
	// seek-only consumer that already holds facts.
	SkipProbe bool
	// Now is the injectable program clock (standing convention, master §5.0).
	Now func() time.Time
}

func (o Options) resolved() Options {
	if o.HeadBytes <= 0 {
		o.HeadBytes = DefaultHeadBytes
	}
	// Zero means "default", not "disabled". The inverse convention was a
	// live-gate finding: Options{} silently produced a facts-only result with
	// no seek index at all, because both index-bearing reads were switched
	// off by the zero value. Disabling is now an explicit negative.
	switch {
	case o.TailBytes == 0:
		o.TailBytes = DefaultTailBytes
	case o.TailBytes < 0:
		o.TailBytes = 0
	}
	switch {
	case o.IndexBytes == 0:
		o.IndexBytes = DefaultIndexBytes
	case o.IndexBytes < 0:
		o.IndexBytes = 0
	}
	if o.MaxIndexEntries <= 0 {
		o.MaxIndexEntries = DefaultMaxIndexEntries
	}
	if o.MaxTopLevelBoxes <= 0 {
		o.MaxTopLevelBoxes = DefaultMaxTopLevelBox
	}
	if o.ProbeTimeout <= 0 {
		o.ProbeTimeout = DefaultProbeTimeout
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

// Facts are compact, typed, secret-free media truth. Every field is optional:
// a source that yields only some of them yields exactly those.
type Facts struct {
	Container string `json:"container,omitempty"`
	// SizeBytes is the source's own reported total. It is recorded alongside
	// the probed facts so a consumer can derive size-dependent values itself
	// rather than trusting a value a prober inferred from a partial file.
	SizeBytes     int64    `json:"size_bytes,omitempty"`
	DurationMS    int64    `json:"duration_ms,omitempty"`
	BitRate       int64    `json:"bit_rate,omitempty"`
	VideoCodec    string   `json:"video_codec,omitempty"`
	Width         int      `json:"width,omitempty"`
	Height        int      `json:"height,omitempty"`
	AudioCodec    string   `json:"audio_codec,omitempty"`
	AudioLangs    []string `json:"audio_langs,omitempty"`
	SubtitleLangs []string `json:"subtitle_langs,omitempty"`
}

// Empty reports whether no fact was established.
func (f Facts) Empty() bool {
	return f.Container == "" && f.SizeBytes == 0 && f.DurationMS == 0 && f.BitRate == 0 &&
		f.VideoCodec == "" && f.Width == 0 && f.Height == 0 &&
		f.AudioCodec == "" && len(f.AudioLangs) == 0 && len(f.SubtitleLangs) == 0
}

// Entry is one seek point: a presentation time and the byte offset in the
// source at which a decoder may begin.
type Entry struct {
	TimeMS int64 `json:"t"`
	Offset int64 `json:"o"`
}

// Index is a compact, monotonic seek index.
type Index struct {
	// Source names the container structure that produced the entries.
	Source string `json:"source,omitempty"`
	// Entries are sorted by TimeMS, strictly increasing in both fields.
	Entries []Entry `json:"entries,omitempty"`
	// Complete reports whether every seek point the container declared is
	// present. False means the set was evenly downsampled to the cap; the
	// retained entries remain exact.
	Complete bool `json:"complete"`
	// Declared is how many seek points the container declared before capping.
	Declared int `json:"declared,omitempty"`
	// Estimated reports that the source's offset space is not exact
	// (Caps().ExactSize == false), so an entry's Offset is approximate
	// wherever the lane's own offset table is estimated rather than
	// recorded. On the NNTP lane, decoded segment lengths diverge from the
	// NZB-declared sizes, so offsets are exact across the region already
	// streamed and drift beyond it. A consumer that prefetches or pins bytes
	// at these offsets must treat them as a starting hint on such a source
	// and resynchronise on the container's own framing, exactly as the
	// cold-seek path already does.
	Estimated bool `json:"estimated,omitempty"`
}

// Empty reports whether the index carries no seek point.
func (i Index) Empty() bool { return len(i.Entries) == 0 }

// SeekPoint returns the last entry at or before timeMS, and whether one
// exists. The name avoids io.Seeker's signature convention deliberately: this
// is an index lookup, not a stream position change.
func (i Index) SeekPoint(timeMS int64) (Entry, bool) {
	if len(i.Entries) == 0 {
		return Entry{}, false
	}
	n := sort.Search(len(i.Entries), func(k int) bool { return i.Entries[k].TimeMS > timeMS })
	if n == 0 {
		return Entry{}, false
	}
	return i.Entries[n-1], true
}

// Budget records what the analysis actually cost.
type Budget struct {
	BytesRead  int64         `json:"bytes_read"`
	BytesLimit int64         `json:"bytes_limit"`
	Reads      int           `json:"reads"`
	Elapsed    time.Duration `json:"elapsed_ns"`
}

// Result is one bounded analysis. Facts and Index are independent: either may
// be absent with the other present, and the reason is stated in the matching
// error string drawn from this package's fixed vocabulary.
type Result struct {
	Facts      Facts  `json:"facts"`
	Index      Index  `json:"index"`
	Budget     Budget `json:"budget"`
	FactsError string `json:"facts_error,omitempty"`
	IndexError string `json:"index_error,omitempty"`
	// SourceKey is the ByteSource's own secret-free identity (TS-1.1).
	SourceKey string `json:"source_key,omitempty"`
}

// Compact returns the canonical compact JSON encoding a consumer persists.
// It contains no URL, token, or credential by construction.
func (r *Result) Compact() ([]byte, error) { return json.Marshal(r) }

// ProbeFailed reports whether this Result is genuine broken-media evidence:
// the shared prober actually ran against the real source bytes and rejected
// them (TS-3.3, frozen T10/T4 -- "probe failure ⇒ T5"). It is deliberately
// narrower than "no facts present" -- an unsupported/truncated container
// (Analyze's own returned error, never reaching a Result at all), a merely
// unconfigured prober (reasonProbeUnavailable), and a local DH-host
// temp-file preparation error (reasonProbePrepFailed) are all legitimate
// bounded non-failures and must never be reported here. This keeps the
// reason vocabulary itself private (callers get a stable boolean, not a
// string to match against) while giving TS-3.3's resolve-time consumer the
// one signal T10 actually asks for.
func (r *Result) ProbeFailed() bool {
	return r != nil && r.FactsError == reasonProbeFailed
}

// reader charges every read against one budget and counts them. It performs
// no concurrency of its own: a ByteSource read is the caller's read.
type reader struct {
	src   bytesource.ByteSource
	limit int64
	used  int64
	reads int
}

// readAt returns up to n bytes at off, clamped to the source size. A short
// read at EOF is not an error here; the caller decides whether the structure
// it wanted was truncated.
func (r *reader) readAt(ctx context.Context, off, n int64) ([]byte, error) {
	if n <= 0 {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if off < 0 {
		return nil, bytesource.ErrNegativeOffset
	}
	size := r.src.Size()
	if size > 0 && off >= size {
		return nil, io.EOF
	}
	if size > 0 && off+n > size {
		n = size - off
	}
	if r.used+n > r.limit {
		return nil, ErrBudgetExhausted
	}
	buf := make([]byte, n)
	got, err := r.src.ReadAt(ctx, buf, off)
	if got < 0 {
		got = 0
	}
	if got > len(buf) {
		got = len(buf)
	}
	r.used += int64(got)
	r.reads++
	if err != nil && !errors.Is(err, io.EOF) {
		return buf[:got], err
	}
	return buf[:got], nil
}

// Analyze performs one bounded, cancellable, lane-neutral analysis of src.
//
// It reads the leading window, identifies the container from its own magic
// bytes, builds the compact seek index from that container's directory
// structure (following the container's own pointer to a tail-located
// directory rather than guessing), and derives facts by running the shared
// prober over a temporary file that carries the bytes actually read at their
// true offsets. Nothing outside the budget is transferred, and the temporary
// file is removed on every exit path.
func Analyze(ctx context.Context, src bytesource.ByteSource, opts Options) (*Result, error) {
	if src == nil {
		return nil, ErrNoSource
	}
	opts = opts.resolved()
	start := opts.Now()

	r := &reader{src: src, limit: opts.HeadBytes + opts.TailBytes + opts.IndexBytes}
	res := &Result{SourceKey: src.Key()}
	defer func() {
		res.Budget = Budget{
			BytesRead:  r.used,
			BytesLimit: r.limit,
			Reads:      r.reads,
			Elapsed:    opts.Now().Sub(start),
		}
	}()

	head, err := r.readAt(ctx, 0, opts.HeadBytes)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, ErrTruncatedSource
		}
		return nil, fmt.Errorf("mediatruth: head read: %w", err)
	}
	if len(head) == 0 {
		return nil, ErrTruncatedSource
	}

	container := detectContainer(head)
	if container == "" {
		return nil, ErrUnsupportedContainer
	}
	res.Facts.Container = container
	res.Facts.SizeBytes = src.Size()

	// Index first: it may pull the tail region that the prober then reuses,
	// so a tail-located header costs one read, not two.
	var tail []byte
	var tailOff int64
	switch container {
	case ContainerMatroska:
		idx, tb, to, ierr := matroskaIndex(ctx, r, head, opts)
		res.Index, tail, tailOff = idx, tb, to
		res.IndexError = ierr
	case ContainerMP4:
		idx, tb, to, ierr := mp4Index(ctx, r, head, opts)
		res.Index, tail, tailOff = idx, tb, to
		res.IndexError = ierr
	}
	if !res.Index.Empty() && !src.Caps().ExactSize {
		res.Index.Estimated = true
	}

	if opts.SkipProbe {
		res.FactsError = reasonProbeUnavailable
		return res, nil
	}
	facts, ferr := probeFacts(ctx, src.Size(), head, tail, tailOff, opts)
	if ferr != "" {
		res.FactsError = ferr
		return res, nil
	}
	facts.Container = container
	facts.SizeBytes = src.Size()
	res.Facts = facts
	return res, nil
}

// detectContainer identifies the container from leading magic bytes only. It
// never trusts a filename, a Content-Type, or a release name.
func detectContainer(head []byte) string {
	if len(head) >= 4 && head[0] == 0x1A && head[1] == 0x45 && head[2] == 0xDF && head[3] == 0xA3 {
		return ContainerMatroska
	}
	if len(head) >= 12 && string(head[4:8]) == "ftyp" {
		return ContainerMP4
	}
	return ""
}

// probeFacts writes the bytes already read into a temporary file at their true
// offsets and runs the shared prober over it. When a tail region is present
// the file is created sparse at the source's full size so byte offsets inside
// the container remain truthful; otherwise it is a small contiguous head file,
// exactly the shape today's resolve-time probe already uses.
func probeFacts(ctx context.Context, size int64, head, tail []byte, tailOff int64, opts Options) (Facts, string) {
	run := opts.Probe
	if run == nil {
		run = func(ctx context.Context, path string) (*probe.ProbeResult, error) {
			return probe.New(nil, opts.ProbeTimeout).ProbeFromFile(ctx, path)
		}
	}

	f, err := os.CreateTemp(opts.TempDir, "darkharrbor-mediatruth-*")
	if err != nil {
		return Facts{}, reasonProbePrepFailed
	}
	path := f.Name()
	defer func() {
		_ = f.Close()
		_ = os.Remove(path)
	}()

	// The temp file is created SPARSE at the source's true length whenever
	// that length is known, not merely when a tail region exists. A prober
	// derives container-level facts such as bitrate from the file size it can
	// see, so a contiguous head-only file yields a bitrate computed over the
	// probe window rather than the media -- wrong by construction, and by a
	// factor equal to the sampling ratio. Sparse allocation writes only the
	// blocks actually populated, so this costs no additional bytes on disk or
	// on the wire; the cap guards a filesystem without sparse support.
	useSparse := size > 0 && size <= defaultSparseMaxBytes
	if _, err := f.WriteAt(head, 0); err != nil {
		return Facts{}, reasonProbePrepFailed
	}
	if useSparse {
		if err := f.Truncate(size); err != nil {
			return Facts{}, reasonProbePrepFailed
		}
		if len(tail) > 0 && tailOff > int64(len(head)) {
			if _, err := f.WriteAt(tail, tailOff); err != nil {
				return Facts{}, reasonProbePrepFailed
			}
		}
	}
	if err := f.Sync(); err != nil {
		return Facts{}, reasonProbePrepFailed
	}

	// Only a real invocation of the shared prober against the actual source
	// bytes -- success or failure -- is genuine broken-media evidence. Every
	// branch above this point is local DH-host preparation and is reported
	// under the distinct reasonProbePrepFailed vocabulary instead (TS-3.3;
	// see the reason's own doc comment for why the split matters).
	pr, err := run(ctx, path)
	if err != nil || pr == nil {
		return Facts{}, reasonProbeFailed
	}
	return factsFromProbe(pr), ""
}

// factsFromProbe converts the shared prober's result into compact facts,
// enriching it with per-stream language tags the prober does not itself
// surface. Unknown/undefined language tags are dropped rather than recorded.
func factsFromProbe(pr *probe.ProbeResult) Facts {
	f := Facts{
		BitRate:    pr.BitRate,
		VideoCodec: pr.VideoCodec,
		Width:      pr.Width,
		Height:     pr.Height,
		AudioCodec: pr.AudioCodec,
	}
	if pr.DurationSecs > 0 {
		f.DurationMS = int64(pr.DurationSecs * 1000)
	}
	var out struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
			Tags      struct {
				Language string `json:"language"`
			} `json:"tags"`
		} `json:"streams"`
	}
	if json.Unmarshal([]byte(pr.JSON), &out) != nil {
		return f
	}
	seenAudio := map[string]bool{}
	seenSub := map[string]bool{}
	for _, s := range out.Streams {
		lang := strings.ToLower(strings.TrimSpace(s.Tags.Language))
		if lang == "" || lang == "und" || lang == "unknown" {
			continue
		}
		switch s.CodecType {
		case "audio":
			if !seenAudio[lang] {
				seenAudio[lang] = true
				f.AudioLangs = append(f.AudioLangs, lang)
			}
		case "subtitle":
			if !seenSub[lang] {
				seenSub[lang] = true
				f.SubtitleLangs = append(f.SubtitleLangs, lang)
			}
		}
	}
	return f
}

// finalizeIndex sorts, de-duplicates, and caps a raw seek-point set. Capping
// downsamples evenly: retained entries stay exact, and Complete records that
// the set is a subset of what the container declared.
func finalizeIndex(source string, raw []Entry, max int) Index {
	if len(raw) == 0 {
		return Index{}
	}
	sort.Slice(raw, func(i, j int) bool {
		if raw[i].TimeMS != raw[j].TimeMS {
			return raw[i].TimeMS < raw[j].TimeMS
		}
		return raw[i].Offset < raw[j].Offset
	})
	// Retain only entries that advance in BOTH dimensions. A seek index is
	// consumed as "to reach time T, begin reading at byte O", so an entry
	// whose offset moves backwards as time moves forwards is not a usable
	// seek point — it would send a prefetch behind the playhead. Real
	// out-of-order interleaving and corrupt or hostile sample tables can both
	// produce them (found by FuzzParseMP4Moov), so the invariant is enforced
	// here rather than merely asserted by callers.
	dedup := raw[:0:0]
	var lastT, lastO int64 = -1, -1
	for _, e := range raw {
		if e.TimeMS < 0 || e.Offset < 0 {
			continue
		}
		if lastT >= 0 && e.TimeMS <= lastT {
			continue
		}
		if lastO >= 0 && e.Offset <= lastO {
			continue
		}
		dedup = append(dedup, e)
		lastT, lastO = e.TimeMS, e.Offset
	}
	if len(dedup) == 0 {
		return Index{}
	}
	declared := len(dedup)
	if declared <= max {
		return Index{Source: source, Entries: dedup, Complete: true, Declared: declared}
	}
	stride := float64(declared-1) / float64(max-1)
	out := make([]Entry, 0, max)
	prev := -1
	for i := 0; i < max; i++ {
		k := int(float64(i)*stride + 0.5)
		if k >= declared {
			k = declared - 1
		}
		if k == prev {
			continue
		}
		prev = k
		out = append(out, dedup[k])
	}
	return Index{Source: source, Entries: out, Complete: false, Declared: declared}
}

// readErrReason maps a source read failure to this package's fixed, secret-
// free reason vocabulary. It never embeds the underlying error text, which
// may carry an upstream URL or header (DG-04).
func readErrReason(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrBudgetExhausted):
		return reasonIndexBudget
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return reasonIndexTruncated
	case isMutationErr(err):
		return reasonSourceMutated
	default:
		return reasonSourceRead
	}
}

// isMutationErr recognizes the HTTP adapter's representation-mutation refusal
// (HR1.1, LC-05) without importing it: the adapter lives in a package that
// would create an import cycle, and the sentinel is matched by its stable
// message rather than by type.
func isMutationErr(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "source mutated")
}
