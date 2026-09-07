package nntp

import (
	"strings"
	"testing"
)

func TestParseNZBMetadataAndFiles(t *testing.T) {
	data := []byte(`<?xml version="1.0"?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
  <head>
    <meta type="password"> archive-passphrase </meta>
    <meta type="title"> Example Title </meta>
    <meta type="category"> TV &amp; Video </meta>
    <meta type="ignored">not-used</meta>
  </head>
  <file poster="poster" date="2" subject="small.mkv">
    <groups><group>alt.test</group></groups>
    <segments><segment bytes="10" number="2"> &lt;second@example&gt; </segment></segments>
  </file>
  <file poster="poster" date="1" subject="large.mkv">
    <groups><group>alt.test</group></groups>
    <segments>
      <segment bytes="20" number="2">second-large@example</segment>
      <segment bytes="20" number="1">first-large@example</segment>
    </segments>
  </file>
</nzb>`)

	nzb, err := ParseNZB(data)
	if err != nil {
		t.Fatal(err)
	}
	if nzb.Password != "archive-passphrase" || nzb.Title != "Example Title" || nzb.Category != "TV & Video" {
		t.Fatal("NZB head metadata was not parsed")
	}
	if len(nzb.Files) != 2 || nzb.Files[0].Subject != "large.mkv" {
		t.Fatal("NZB files were not sorted by size")
	}
	if got := nzb.Files[0].Segments[0].MessageID; got != "first-large@example" {
		t.Fatalf("first segment message ID = %q", got)
	}
	for _, seg := range nzb.Files[0].Segments {
		if seg.PostedAt != nzb.Files[0].Date || seg.PostedAt != 1 {
			t.Fatalf("segment posted_at = %d, want parent file date 1", seg.PostedAt)
		}
	}
}

func TestParseNZBMetadataOptionalAndFirstValueWins(t *testing.T) {
	nzb, err := ParseNZB([]byte(`<nzb><head>
<meta type=" PASSWORD ">first</meta><meta type="password">second</meta>
<meta type="title"> </meta><meta type="title">title</meta>
</head></nzb>`))
	if err != nil {
		t.Fatal(err)
	}
	if nzb.Password != "first" || nzb.Title != "title" || nzb.Category != "" {
		t.Fatal("optional or repeated NZB metadata was not handled deterministically")
	}
}

func TestParseNZBErrorDoesNotExposeMetadata(t *testing.T) {
	const password = "archive-passphrase"
	_, err := ParseNZB([]byte(`<nzb><head><meta type="password">` + password + `</meta></head>`))
	if err == nil {
		t.Fatal("malformed NZB unexpectedly parsed")
	}
	if strings.Contains(err.Error(), password) {
		t.Fatal("parse error exposed NZB password")
	}
}

func TestContentKeyIgnoresNZBMetadata(t *testing.T) {
	const file = `<file subject="video.mkv"><segments><segment bytes="1" number="1">id@example</segment></segments></file>`
	withoutPassword := []byte(`<nzb><head><meta type="title">one</meta></head>` + file + `</nzb>`)
	withPassword := []byte(`<nzb><head><meta type="password">archive-passphrase</meta><meta type="title">two</meta></head>` + file + `</nzb>`)
	if ContentKey(withoutPassword) != ContentKey(withPassword) {
		t.Fatal("NZB metadata changed the article content key")
	}
}

func FuzzParseNZB(f *testing.F) {
	f.Add([]byte(`<nzb><head><meta type="password">seed</meta><meta type="title">title</meta><meta type="category">tv</meta></head><file subject="video.mkv"><segments><segment bytes="1" number="1">id@example</segment></segments></file></nzb>`))
	f.Add([]byte(`<nzb><head><meta type="password">seed</meta></head>`))
	f.Add([]byte(`<nzb><head><meta type="password">seed</meta></head`))

	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = ParseNZB(data)
	})
}
