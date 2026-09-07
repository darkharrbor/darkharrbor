package hls

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/contentproof"
)

func TestContentSteeringTagAndPathwayCoordinates(t *testing.T) {
	body := "#EXTM3U\n" +
		"#EXT-X-CONTENT-STEERING:SERVER-URI=\"steering.json?tok=secret\",PATHWAY-ID=\"A\"\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=1,PATHWAY-ID=\"A\",STABLE-VARIANT-ID=\"v1\"\na.m3u8\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=1,PATHWAY-ID=\"B\",STABLE-VARIANT-ID=\"v1\"\nb.m3u8\n"
	tag, found, err := ParseContentSteering(body)
	if err != nil || !found || tag.ServerURI != "steering.json?tok=secret" || tag.PathwayID != "A" {
		t.Fatalf("ParseContentSteering() = (%+v,%t,%v)", tag, found, err)
	}
	entries, err := ParseMaster(body)
	if err != nil || len(entries) != 2 || entries[0].PathwayID != "A" || entries[1].PathwayID != "B" {
		t.Fatalf("ParseMaster pathways = %+v, %v", entries, err)
	}
	rewritten, err := RewriteContentSteering(body, "/hls/hz000000000000.steering.json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rewritten, "tok=secret") || !strings.Contains(rewritten, `SERVER-URI="/hls/hz000000000000.steering.json"`) {
		t.Fatalf("unsafe steering rewrite:\n%s", rewritten)
	}
	ids, err := MasterPathwayIDs(body)
	if err != nil || strings.Join(ids, ",") != "A,B" {
		t.Fatalf("MasterPathwayIDs() = %v, %v", ids, err)
	}
}

func TestContentSteeringMalformedTagsFailClosed(t *testing.T) {
	for _, body := range []string{
		"#EXTM3U\n#EXT-X-CONTENT-STEERING:PATHWAY-ID=\"A\"\n#EXT-X-STREAM-INF:BANDWIDTH=1\nx\n",
		"#EXTM3U\n#EXT-X-CONTENT-STEERING:SERVER-URI=\"\"\n#EXT-X-STREAM-INF:BANDWIDTH=1\nx\n",
		"#EXTM3U\n#EXT-X-CONTENT-STEERING:SERVER-URI=\"a\",SERVER-URI=\"b\"\n#EXT-X-STREAM-INF:BANDWIDTH=1\nx\n",
		"#EXTM3U\n#EXT-X-CONTENT-STEERING:SERVER-URI=\"a\",PATHWAY-ID=\"bad/path\"\n#EXT-X-STREAM-INF:BANDWIDTH=1\nx\n",
		"#EXTM3U\n#EXT-X-CONTENT-STEERING:SERVER-URI=\"a\"\n#EXT-X-CONTENT-STEERING:SERVER-URI=\"b\"\n#EXT-X-STREAM-INF:BANDWIDTH=1\nx\n",
	} {
		if _, _, err := ParseContentSteering(body); !errors.Is(err, ErrSteering) {
			t.Fatalf("ParseContentSteering(%q) error=%v", body, err)
		}
	}
}

func TestParseAndSynthesizeSteeringProofRestrictions(t *testing.T) {
	body := []byte(`{
		"VERSION":1,
		"TTL":300,
		"RELOAD-URI":"https://origin.invalid/reload?tok=secret",
		"PATHWAY-PRIORITY":["origin","proved","conflict","unknown"],
		"PATHWAY-CLONES":[{"ID":"proved","URI-REPLACEMENT":{"HOST":"secret.invalid"}}],
		"UNKNOWN":"ignored"
	}`)
	manifest, err := ParseSteeringManifest(body)
	if err != nil {
		t.Fatal(err)
	}
	out, err := SynthesizeSteeringManifest(manifest, []PathwayCandidate{
		{ID: "origin", OriginSupplied: true},
		{ID: "proved", Proof: contentproof.RelationProven},
		{ID: "conflict", Proof: contentproof.RelationConflict},
		{ID: "unknown", Proof: contentproof.RelationNoProof},
	}, func(ids []string) []string {
		return []string{ids[1], ids[0]}
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "{\"VERSION\":1,\"TTL\":300,\"PATHWAY-PRIORITY\":[\"proved\",\"origin\"]}\n" {
		t.Fatalf("unexpected synthesized manifest: %s", out)
	}
	for _, leaked := range []string{"origin.invalid", "secret.invalid", "tok=secret", "RELOAD-URI", "PATHWAY-CLONES", "conflict", "unknown"} {
		if bytes.Contains(out, []byte(leaked)) {
			t.Fatalf("synthesized manifest leaked %q: %s", leaked, out)
		}
	}
}

func TestSteeringManifestMalformedAndBounds(t *testing.T) {
	cases := [][]byte{
		nil,
		[]byte(`{`),
		[]byte(`{"VERSION":2,"TTL":1,"PATHWAY-PRIORITY":["A"]}`),
		[]byte(`{"VERSION":1.5,"TTL":1,"PATHWAY-PRIORITY":["A"]}`),
		[]byte(`{"VERSION":1,"TTL":0,"PATHWAY-PRIORITY":["A"]}`),
		[]byte(`{"VERSION":1,"TTL":86401,"PATHWAY-PRIORITY":["A"]}`),
		[]byte(`{"VERSION":1,"TTL":1,"PATHWAY-PRIORITY":[]}`),
		[]byte(`{"VERSION":1,"TTL":1,"PATHWAY-PRIORITY":["A","A"]}`),
		[]byte(`{"VERSION":1,"TTL":1,"PATHWAY-PRIORITY":["bad/path"]}`),
		[]byte(`{"VERSION":1,"VERSION":1,"TTL":1,"PATHWAY-PRIORITY":["A"]}`),
	}
	for _, body := range cases {
		if _, err := ParseSteeringManifest(body); !errors.Is(err, ErrSteeringManifest) {
			t.Fatalf("ParseSteeringManifest(%q) error=%v", body, err)
		}
	}
	if _, err := ParseSteeringManifest(bytes.Repeat([]byte("x"), MaxSteeringBytes+1)); !errors.Is(err, ErrSteeringManifest) {
		t.Fatalf("oversized error=%v", err)
	}
}
