package api

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

func TestNZBBlacklistKey(t *testing.T) {
	a := NZBBlacklistKey("raw nzb content A")
	b := NZBBlacklistKey("raw nzb content B")
	aAgain := NZBBlacklistKey("raw nzb content A")

	if a == "" {
		t.Fatal("NZBBlacklistKey returned empty string")
	}
	if a != aAgain {
		t.Errorf("NZBBlacklistKey not deterministic: %q != %q", a, aAgain)
	}
	if a == b {
		t.Errorf("NZBBlacklistKey collided for different content: %q", a)
	}
	const prefix = "nzb:"
	if len(a) <= len(prefix) || a[:len(prefix)] != prefix {
		t.Errorf("NZBBlacklistKey %q missing %q namespace prefix", a, prefix)
	}
}

func TestNZBBlacklistKeyCanonicalizesEquivalentNZBXML(t *testing.T) {
	first := `<?xml version="1.0"?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
  <file poster="poster-a" date="100" subject="release-a">
    <groups><group>alt.binaries.example</group></groups>
    <segments>
      <segment bytes="200" number="2">&lt;second@example&gt;</segment>
      <segment bytes="100" number="1">&lt;first@example&gt;</segment>
    </segments>
  </file>
</nzb>`
	second := `<nzb>
  <file subject="different wrapper metadata" date="999" poster="poster-b">
    <segments>
      <segment number="1" bytes="100"> first@example </segment>
      <segment number="2" bytes="200"> second@example </segment>
    </segments>
    <groups><group>alt.binaries.other</group></groups>
  </file>
</nzb>`

	firstKey := NZBBlacklistKey(first)
	secondKey := NZBBlacklistKey(second)
	if firstKey != secondKey {
		t.Fatalf("equivalent NZB segment identities produced different keys: %q != %q", firstKey, secondKey)
	}
}

func TestNZBBlacklistKeyDistinguishesDifferentSegments(t *testing.T) {
	first := `<nzb><file><segments><segment bytes="100" number="1">first@example</segment></segments></file></nzb>`
	second := `<nzb><file><segments><segment bytes="100" number="1">different@example</segment></segments></file></nzb>`

	if NZBBlacklistKey(first) == NZBBlacklistKey(second) {
		t.Fatal("different NZB segment identities produced the same blacklist key")
	}
}

func TestEnqueueSubmissionRejectsBlacklistedNZBBeforeItemCreation(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "nzb_enqueue_blacklist.db")
	db, err := store.Open(ctx, dbPath, 5*time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatalf("RunMigrationsFS: %v", err)
	}

	st := store.New(db)
	rawNZB := "https://indexer.example/api?t=get&id=dead-release&apikey=super-secret-token"
	if _, err := st.BlacklistImmediately(ctx, NZBBlacklistKey(rawNZB)); err != nil {
		t.Fatalf("BlacklistImmediately: %v", err)
	}

	cfg := &config.Config{}
	cfg.Routing.Preference = []string{config.LaneNNTPNZB}
	cfg.Compatibility.DefaultCategory = "darkharrbor"
	var logs bytes.Buffer
	server := &Server{
		cfg:   cfg,
		log:   slog.New(slog.NewTextHandler(&logs, nil)),
		store: st,
	}

	item, duplicate, err := server.enqueueSubmission(ctx, SubmissionRequest{
		SourceType:  store.SourceTypeNZB,
		ClientKind:  store.ClientKindSAB,
		Category:    "tv",
		DisplayName: "Dead.Release.S01E01",
		SourceURI:   rawNZB,
	})
	if err == nil || !strings.Contains(err.Error(), "temporarily blacklisted") {
		t.Fatalf("enqueue error = %v, want temporary-blacklist rejection", err)
	}
	if item != nil {
		t.Fatalf("rejected enqueue returned item %q", item.ID)
	}
	if duplicate {
		t.Fatal("blacklist rejection was incorrectly reported as an active-item duplicate")
	}

	items, err := st.ListVisibleClientItems(ctx, store.ClientKindSAB, "", 100)
	if err != nil {
		t.Fatalf("ListVisibleClientItems: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("blacklist rejection created %d items, want 0", len(items))
	}

	differentBlacklisted, err := st.IsBlacklisted(ctx, NZBBlacklistKey("different nzb content"))
	if err != nil {
		t.Fatalf("IsBlacklisted for different NZB: %v", err)
	}
	if differentBlacklisted {
		t.Fatal("blacklist for exact NZB content incorrectly matched different NZB content")
	}

	gotLogs := logs.String()
	for _, want := range []string{
		"event=enqueue_rejected",
		"reason=temporarily_blacklisted",
		"nzb_key=" + NZBBlacklistKey(rawNZB),
		"display_name=Dead.Release.S01E01",
		"category=tv",
	} {
		if !strings.Contains(gotLogs, want) {
			t.Fatalf("logs missing %q: %s", want, gotLogs)
		}
	}
	for _, forbidden := range []string{rawNZB, "super-secret-token"} {
		if strings.Contains(gotLogs, forbidden) {
			t.Fatalf("logs exposed sensitive NZB source %q: %s", forbidden, gotLogs)
		}
	}
}
