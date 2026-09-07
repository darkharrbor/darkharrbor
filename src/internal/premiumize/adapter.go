package premiumize

import (
	"context"
	"errors"
	"fmt"
	"strings"

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

func (a *Adapter) Name() string { return a.name }

func (a *Adapter) Capabilities() provider.Capabilities { return a.caps }

func (a *Adapter) CheckCached(ctx context.Context, item *store.Item) (*provider.CheckCachedResult, error) {
	magnet, err := torrentMagnet(item)
	if err != nil {
		return nil, err
	}
	cached, err := a.client.CheckCache(ctx, magnet)
	if err != nil {
		return nil, fmt.Errorf("premiumize: cache check: %w", err)
	}
	return &provider.CheckCachedResult{Cached: cached}, nil
}

func (a *Adapter) Submit(ctx context.Context, item *store.Item, opts provider.SubmitOptions) (*provider.CreateTaskResponse, error) {
	if !a.premium {
		return nil, fmt.Errorf("premiumize: premium account required")
	}
	magnet, err := torrentMagnet(item)
	if err != nil {
		return nil, err
	}
	if opts.AddOnlyIfCached {
		cached, cacheErr := a.client.CheckCache(ctx, magnet)
		if cacheErr != nil {
			return nil, fmt.Errorf("premiumize: cached-only recheck: %w", cacheErr)
		}
		if !cached {
			return nil, fmt.Errorf("premiumize: cached-only submission is not cached")
		}
	}
	transfer, err := a.client.CreateTransfer(ctx, magnet)
	if err != nil {
		if isTransientAPIError(err) {
			return nil, provider.MarkRetryable(err)
		}
		return nil, err
	}
	return &provider.CreateTaskResponse{RemoteID: transfer.ID, DisplayName: transfer.Name}, nil
}

func (a *Adapter) Poll(ctx context.Context, item *store.Item) (*provider.TaskStatus, error) {
	id, err := remoteID(item)
	if err != nil {
		return nil, err
	}
	transfer, err := a.client.GetTransfer(ctx, id)
	if err != nil {
		if errors.Is(err, ErrTransferNotFound) {
			return &provider.TaskStatus{
				RemoteID: id, State: "removed", Outcome: provider.TorrentOutcomeRemoteRemoved,
				FailureCode: provider.TorrentFailureRemoteRemoved,
			}, nil
		}
		return nil, fmt.Errorf("premiumize: poll failed: %w", err)
	}
	status := &provider.TaskStatus{
		RemoteID: transfer.ID,
		Name:     transfer.Name,
		State:    transfer.Status,
		Progress: transfer.Progress,
	}
	switch transfer.Status {
	case "queued", "running":
		status.Outcome = provider.TorrentOutcomeUnknown
	case "finished", "seeding":
		files, filesErr := a.client.FilesForTransfer(ctx, transfer)
		if filesErr != nil {
			return nil, fmt.Errorf("premiumize: ready transfer files: %w", filesErr)
		}
		status.DownloadReady = true
		status.Outcome = provider.TorrentOutcomeReady
		status.Files = make([]provider.RemoteFile, 0, len(files))
		for _, file := range files {
			status.Files = append(status.Files, provider.RemoteFile{
				FileID: file.ID, Name: file.Name, RelativePath: file.RelativePath, Size: file.Size,
			})
		}
	case "error":
		status.Outcome = provider.TorrentOutcomeTransientError
		status.FailureCode = provider.TorrentFailureProviderUnavailable
	default:
		return nil, fmt.Errorf("premiumize: unsupported transfer state")
	}
	return status, nil
}

func (a *Adapter) RequestDownloadURL(ctx context.Context, item *store.Item, fileID string) (string, error) {
	id, err := remoteID(item)
	if err != nil {
		return "", err
	}
	if !validID(fileID) {
		return "", fmt.Errorf("premiumize: invalid file id")
	}
	transfer, err := a.client.GetTransfer(ctx, id)
	if err != nil {
		return "", fmt.Errorf("premiumize: refresh transfer: %w", err)
	}
	if transfer.Status != "finished" && transfer.Status != "seeding" {
		return "", fmt.Errorf("premiumize: transfer is not ready")
	}
	files, err := a.client.FilesForTransfer(ctx, transfer)
	if err != nil {
		return "", fmt.Errorf("premiumize: refresh transfer files: %w", err)
	}
	var selected *File
	for i := range files {
		if files[i].ID == fileID {
			if selected != nil {
				return "", fmt.Errorf("premiumize: file id is ambiguous")
			}
			selected = &files[i]
		}
	}
	if selected == nil || !validLink(selected.link) {
		return "", fmt.Errorf("premiumize: file id no longer maps uniquely")
	}
	return selected.link, nil
}

func (a *Adapter) Remove(ctx context.Context, item *store.Item) error {
	if item == nil || item.RemoteID == nil || strings.TrimSpace(*item.RemoteID) == "" {
		return nil
	}
	id := strings.TrimSpace(*item.RemoteID)
	transfer, err := a.client.GetTransfer(ctx, id)
	if errors.Is(err, ErrTransferNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("premiumize: inspect transfer for removal: %w", err)
	}
	switch {
	case transfer.FileID != "":
		if err := a.client.DeleteItem(ctx, transfer.FileID); err != nil {
			return fmt.Errorf("premiumize: delete transfer file: %w", err)
		}
	case (transfer.Status == "finished" || transfer.Status == "seeding") && transfer.FolderID != "":
		if err := a.client.DeleteFolder(ctx, transfer.FolderID); err != nil {
			return fmt.Errorf("premiumize: delete transfer folder: %w", err)
		}
	}
	if err := a.client.DeleteTransfer(ctx, id); err != nil {
		return fmt.Errorf("premiumize: delete transfer: %w", err)
	}
	return nil
}

func remoteID(item *store.Item) (string, error) {
	if item == nil || item.RemoteID == nil || !validID(strings.TrimSpace(*item.RemoteID)) {
		return "", fmt.Errorf("premiumize: item has no valid remote id")
	}
	return strings.TrimSpace(*item.RemoteID), nil
}

func torrentMagnet(item *store.Item) (string, error) {
	if item == nil || item.SourceType != store.SourceTypeTorrent {
		return "", fmt.Errorf("premiumize: only torrent submissions are supported")
	}
	if item.SourceURI != nil {
		source := strings.TrimSpace(*item.SourceURI)
		if strings.HasPrefix(strings.ToLower(source), "magnet:") {
			return source, nil
		}
	}
	hash := strings.TrimSpace(item.Metadata.RealInfoHash)
	if hash == "" && item.InfoHash != nil {
		hash = strings.TrimSpace(*item.InfoHash)
	}
	if len(hash) != 40 {
		return "", fmt.Errorf("premiumize: torrent has no usable infohash")
	}
	for _, r := range hash {
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return "", fmt.Errorf("premiumize: torrent has no usable infohash")
		}
	}
	return "magnet:?xt=urn:btih:" + strings.ToLower(hash), nil
}
