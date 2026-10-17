package torbox

import (
	"context"
	"errors"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/provider"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

type exportCountClient struct {
	Client
	exports int
}

func (c *exportCountClient) CreateTorrentTask(context.Context, CreateTorrentTaskRequest) (*CreateTaskResponse, error) {
	return &CreateTaskResponse{RemoteID: "42"}, nil
}

func (c *exportCountClient) ExportTorrentFile(context.Context, string) ([]byte, error) {
	c.exports++
	return []byte("d4:infod4:name1:xee"), nil
}

// A torrent added cache-only never has exportable metainfo at TorBox, so the
// adapter must not spend an API call asking.
func TestFetchTorrentFileSkipsCachedAdds(t *testing.T) {
	cc := &exportCountClient{}
	a := &Adapter{name: "torbox", client: cc}
	item := &store.Item{ID: "i", SourceType: store.SourceTypeTorrent, DisplayName: "x", SourceURI: strp("magnet:?xt=urn:btih:00")}
	if _, err := a.Submit(context.Background(), item, provider.SubmitOptions{AddOnlyIfCached: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.FetchTorrentFile(context.Background(), "42"); !errors.Is(err, provider.ErrMetainfoUnavailable) {
		t.Fatalf("err=%v, want ErrMetainfoUnavailable", err)
	}
	if cc.exports != 0 {
		t.Fatalf("exportdata called %d times for a cached add", cc.exports)
	}
	if _, err := a.FetchTorrentFile(context.Background(), "99"); err != nil || cc.exports != 1 {
		t.Fatalf("uncached remote id should still be fetched: err=%v exports=%d", err, cc.exports)
	}
}
