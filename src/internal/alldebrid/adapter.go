package alldebrid

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/provider"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

type Adapter struct {
	name    string
	client  Client
	caps    provider.Capabilities
	premium bool
}

func NewAdapter(name string, client Client) *Adapter {
	return &Adapter{name: name, client: client}
}

func (a *Adapter) SetCapabilities(caps provider.Capabilities) *Adapter {
	a.caps = caps
	return a
}

func (a *Adapter) SetPremium(premium bool) *Adapter {
	a.premium = premium
	return a
}

var _ provider.Provider = (*Adapter)(nil)
var _ provider.SlotSource = (*Adapter)(nil)

func (a *Adapter) Name() string { return a.name }

func (a *Adapter) Capabilities() provider.Capabilities { return a.caps }

func (a *Adapter) SlotStatus(ctx context.Context) (*provider.SlotStatus, error) {
	active, err := a.client.ActiveCount(ctx)
	if err != nil {
		return nil, fmt.Errorf("alldebrid: active magnet count: %w", err)
	}
	return &provider.SlotStatus{AllowedActiveSlots: ActiveMagnetLimit, ActiveCount: active}, nil
}

func (a *Adapter) CheckCached(context.Context, *store.Item) (*provider.CheckCachedResult, error) {
	return &provider.CheckCachedResult{Cached: false}, nil
}

func (a *Adapter) Submit(ctx context.Context, item *store.Item, opts provider.SubmitOptions) (*provider.CreateTaskResponse, error) {
	if !a.premium {
		return nil, fmt.Errorf("alldebrid: premium account required")
	}
	if item == nil || item.SourceType != store.SourceTypeTorrent {
		return nil, fmt.Errorf("alldebrid: only torrent submissions are supported")
	}
	magnet := itemMagnet(item)
	if magnet == "" {
		return nil, fmt.Errorf("alldebrid: torrent has no usable infohash")
	}
	upload, err := a.client.UploadMagnet(ctx, magnet)
	if err != nil {
		if isTransientAPIError(err) {
			return nil, provider.MarkRetryable(err)
		}
		return nil, err
	}
	if opts.AddOnlyIfCached && !upload.Ready {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := a.client.DeleteMagnet(cleanupCtx, upload.ID); err != nil {
			return nil, provider.MarkRetryable(fmt.Errorf("alldebrid: cached-only cleanup failed"))
		}
		return nil, fmt.Errorf("alldebrid: cached-only submission was not immediately ready")
	}
	return &provider.CreateTaskResponse{
		RemoteID: upload.ID, RemoteHash: upload.Hash, DisplayName: upload.Name,
	}, nil
}

func (a *Adapter) Poll(ctx context.Context, item *store.Item) (*provider.TaskStatus, error) {
	id, err := remoteID(item)
	if err != nil {
		return nil, err
	}
	magnet, err := a.client.GetMagnet(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("alldebrid: poll failed: %w", err)
	}
	status := magnetToTaskStatus(magnet)
	if magnet.StatusCode == 4 {
		files, err := a.client.GetFiles(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("alldebrid: ready magnet files: %w", err)
		}
		status.Files = make([]provider.RemoteFile, 0, len(files))
		for _, file := range files {
			status.Files = append(status.Files, provider.RemoteFile{
				FileID: file.ID, Name: file.Name, RelativePath: file.RelativePath, Size: file.Size,
			})
		}
	}
	return status, nil
}

func (a *Adapter) RequestDownloadURL(ctx context.Context, item *store.Item, fileID string) (string, error) {
	id, err := remoteID(item)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(fileID) == "" {
		return "", fmt.Errorf("alldebrid: invalid file id")
	}
	files, err := a.client.GetFiles(ctx, id)
	if err != nil {
		return "", fmt.Errorf("alldebrid: refresh files: %w", err)
	}
	var selected *File
	for i := range files {
		if files[i].ID == fileID {
			if selected != nil {
				return "", fmt.Errorf("alldebrid: file id no longer maps uniquely")
			}
			selected = &files[i]
		}
	}
	if selected == nil || selected.link == "" {
		return "", fmt.Errorf("alldebrid: file id no longer maps uniquely")
	}
	return a.client.UnlockLink(ctx, selected.link)
}

func (a *Adapter) Remove(ctx context.Context, item *store.Item) error {
	if item == nil || item.RemoteID == nil || strings.TrimSpace(*item.RemoteID) == "" {
		return nil
	}
	if err := a.client.DeleteMagnet(ctx, strings.TrimSpace(*item.RemoteID)); err != nil {
		return fmt.Errorf("alldebrid: remove failed: %w", err)
	}
	return nil
}

func magnetToTaskStatus(m *Magnet) *provider.TaskStatus {
	status := &provider.TaskStatus{
		RemoteID: m.ID, Name: m.Name, State: strings.ToLower(strings.TrimSpace(m.Status)),
		Seeds: m.Seeders,
	}
	if m.Size > 0 {
		status.Progress = float64(m.Downloaded) / float64(m.Size)
		if status.Progress > 1 {
			status.Progress = 1
		}
	}
	switch m.StatusCode {
	case 0, 1, 2, 3:
		status.Outcome = provider.TorrentOutcomeUnknown
	case 4:
		status.DownloadReady = true
		status.Outcome = provider.TorrentOutcomeReady
	case 7, 10, 14, 15:
		status.Failed = true
		status.Outcome = provider.TorrentOutcomeTerminalDeadSource
		status.FailureCode = provider.TorrentFailureTerminalDeadSource
	case 5, 6, 8, 9, 11, 12, 13:
		status.Outcome = provider.TorrentOutcomeTransientError
		status.FailureCode = provider.TorrentFailureProviderUnavailable
	default:
		status.Outcome = provider.TorrentOutcomeTransientError
		status.FailureCode = provider.TorrentFailureProviderUnavailable
	}
	return status
}

func remoteID(item *store.Item) (string, error) {
	if item == nil || item.RemoteID == nil || strings.TrimSpace(*item.RemoteID) == "" {
		return "", fmt.Errorf("alldebrid: item has no remote id")
	}
	return strings.TrimSpace(*item.RemoteID), nil
}

func itemMagnet(item *store.Item) string {
	if item.SourceURI != nil {
		source := strings.TrimSpace(*item.SourceURI)
		if strings.HasPrefix(strings.ToLower(source), "magnet:") {
			return source
		}
	}
	hash := strings.TrimSpace(item.Metadata.RealInfoHash)
	if hash == "" && item.InfoHash != nil {
		hash = strings.TrimSpace(*item.InfoHash)
	}
	if len(hash) != 40 {
		return ""
	}
	for _, r := range hash {
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return ""
		}
	}
	return "magnet:?xt=urn:btih:" + strings.ToLower(hash)
}
