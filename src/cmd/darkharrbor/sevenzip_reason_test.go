package main

import (
	"errors"
	"fmt"
	"testing"

	archiveparser "github.com/darkharrbor/darkharrbor/internal/archive"
	"github.com/darkharrbor/darkharrbor/internal/nntp"
)

func TestSevenZipUnsupportedReason(t *testing.T) {
	cases := map[error]string{
		fmt.Errorf("%w: encoded next header", archiveparser.ErrCompressed7z): "compressed",
		archiveparser.ErrSolid7z:                 "solid",
		archiveparser.ErrEncryptedHeader7z:       "encrypted_header",
		nntp.ErrAmbiguousSevenZipVolumes:         "volume_layout",
		errors.New("nntp: article fetch failed"): "",
	}
	for err, want := range cases {
		if got := sevenZipUnsupportedReason(err); got != want {
			t.Errorf("%v: got %q want %q", err, got, want)
		}
	}
}
