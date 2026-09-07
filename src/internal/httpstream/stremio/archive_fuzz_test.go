package stremio

import (
	"encoding/json"
	"testing"
)

func FuzzRemoteArchiveDescriptor(f *testing.F) {
	f.Add([]byte(`{"zipUrls":["https://cdn.example/movie.zip"],"behaviorHints":{"filename":"movie.mkv"}}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		var stream streamObject
		if json.Unmarshal(data, &stream) != nil {
			return
		}
		descriptor, ok := remoteArchiveDescriptor(stream)
		if !ok {
			return
		}
		if len(descriptor.Parts) == 0 || len(descriptor.Parts) > maxRemoteArchiveParts {
			t.Fatalf("part count %d outside bound", len(descriptor.Parts))
		}
		for _, part := range descriptor.Parts {
			if !isFetchableURL(part.URL) {
				t.Fatal("descriptor retained an invalid URL")
			}
		}
	})
}
