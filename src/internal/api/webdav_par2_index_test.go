package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

func TestNZBPersistedFileIndex(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "Recovered.Show.S01E01.strm")
	item := &store.Item{StrmPath: &first}

	for _, test := range []struct {
		name    string
		stem    string
		content string
		write   bool
		wantIdx int
		wantOK  bool
		wantErr bool
	}{
		{name: "exact", stem: "Recovered.Show.S01E01", content: "http://darkharrbor:8381/dav/tv/release/file.mkv?nzb_file_index=3", write: true, wantIdx: 3, wantOK: true},
		{name: "ordinary", stem: "Ordinary.Show.S01E01", content: "http://darkharrbor:8381/dav/tv/release/file.mkv", write: true},
		{name: "missing", stem: "Missing.Show.S01E01"},
		{name: "malformed", stem: "Bad.Show.S01E01", content: "http://darkharrbor:8381/dav/tv/release/file.mkv?nzb_file_index=bad", write: true, wantErr: true},
		{name: "negative", stem: "Negative.Show.S01E01", content: "http://darkharrbor:8381/dav/tv/release/file.mkv?nzb_file_index=-1", write: true, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(dir, test.stem+".strm")
			if test.write {
				if err := os.WriteFile(path, []byte(test.content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			idx, ok, err := nzbPersistedFileIndex(item, test.stem)
			if (err != nil) != test.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, test.wantErr)
			}
			if idx != test.wantIdx || ok != test.wantOK {
				t.Fatalf("index=%d ok=%v, want index=%d ok=%v", idx, ok, test.wantIdx, test.wantOK)
			}
		})
	}
}

func TestNZBPersistedFileIndexNoItemPath(t *testing.T) {
	idx, ok, err := nzbPersistedFileIndex(&store.Item{}, "anything")
	if err != nil || ok || idx != 0 {
		t.Fatalf("index=%d ok=%v error=%v, want zero/false/nil", idx, ok, err)
	}
}

const webDAVExactIndexTestNZB = `<nzb>
  <file poster="poster" date="1" subject="large-obfuscated">
    <groups><group>alt.test</group></groups>
    <segments><segment bytes="200" number="1">large@example</segment></segments>
  </file>
  <file poster="poster" date="1" subject="small-obfuscated">
    <groups><group>alt.test</group></groups>
    <segments><segment bytes="100" number="1">small@example</segment></segments>
  </file>
</nzb>`

func newWebDAVExactIndexServer(t *testing.T, artifactURL string) (*Server, string) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	stem := "Recovered.Show.S01E01"
	releaseDir := filepath.Join(root, "tv", "release")
	if err := os.MkdirAll(releaseDir, 0o750); err != nil {
		t.Fatal(err)
	}
	strmPath := filepath.Join(releaseDir, stem+".strm")
	if err := os.WriteFile(strmPath, []byte(artifactURL), 0o644); err != nil {
		t.Fatal(err)
	}

	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "webdav-index.db"), 5*time.Second)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatalf("RunMigrationsFS: %v", err)
	}
	st := store.New(db)
	source := webDAVExactIndexTestNZB
	now := time.Now().UTC()
	item := &store.Item{
		ID:            "webdav-index",
		PublicID:      "webdav-index",
		SourceType:    store.SourceTypeNZB,
		ClientKind:    store.ClientKindSAB,
		Category:      "tv",
		State:         store.StateReady,
		SubmissionKey: "webdav-index",
		DisplayName:   "release",
		SourceURI:     &source,
		StrmPath:      &strmPath,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := st.CreateItem(ctx, item); err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	cfg := &config.Config{}
	cfg.Data.Root = root
	return &Server{cfg: cfg, store: st}, stem
}

func TestWebDAVResolveItemBindsExactIndexToArtifact(t *testing.T) {
	for _, test := range []struct {
		name        string
		artifactURL string
		requestRaw  string
		wantIndex   int
		wantErr     bool
	}{
		{
			name:        "ordinary omitted",
			artifactURL: "http://darkharrbor:8381/dav/tv/release/Recovered.Show.S01E01.mkv",
			wantIndex:   0,
		},
		{
			name:        "ordinary rejects injected index",
			artifactURL: "http://darkharrbor:8381/dav/tv/release/Recovered.Show.S01E01.mkv",
			requestRaw:  "?nzb_file_index=1",
			wantErr:     true,
		},
		{
			name:        "exact omitted",
			artifactURL: "http://darkharrbor:8381/dav/tv/release/Recovered.Show.S01E01.mkv?nzb_file_index=1",
			wantIndex:   1,
		},
		{
			name:        "exact matching",
			artifactURL: "http://darkharrbor:8381/dav/tv/release/Recovered.Show.S01E01.mkv?nzb_file_index=1",
			requestRaw:  "?nzb_file_index=1",
			wantIndex:   1,
		},
		{
			name:        "exact conflicting",
			artifactURL: "http://darkharrbor:8381/dav/tv/release/Recovered.Show.S01E01.mkv?nzb_file_index=1",
			requestRaw:  "?nzb_file_index=0",
			wantErr:     true,
		},
		{
			name:        "exact malformed",
			artifactURL: "http://darkharrbor:8381/dav/tv/release/Recovered.Show.S01E01.mkv?nzb_file_index=1",
			requestRaw:  "?nzb_file_index=bad",
			wantErr:     true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv, stem := newWebDAVExactIndexServer(t, test.artifactURL)
			req := httptest.NewRequest(http.MethodGet, "/dav/tv/release/"+stem+".mkv"+test.requestRaw, nil)
			item, index, err := srv.webdavResolveItem(req)
			if (err != nil) != test.wantErr {
				t.Fatalf("error=%v, wantErr=%v", err, test.wantErr)
			}
			if test.wantErr {
				return
			}
			if item == nil || index != test.wantIndex {
				t.Fatalf("item=%v index=%d, want non-nil index=%d", item, index, test.wantIndex)
			}
		})
	}
}
