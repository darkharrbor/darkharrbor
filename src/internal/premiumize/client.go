package premiumize

import (
	"context"
	"errors"
	"time"
)

var ErrTransferNotFound = errors.New("premiumize: transfer not found")

type Client interface {
	GetAccount(context.Context) (*AccountInfo, error)
	CheckCache(context.Context, string) (bool, error)
	CreateTransfer(context.Context, string) (*Transfer, error)
	GetTransfer(context.Context, string) (*Transfer, error)
	FilesForTransfer(context.Context, *Transfer) ([]File, error)
	GetFile(context.Context, string) (*File, error)
	DeleteTransfer(context.Context, string) error
	DeleteItem(context.Context, string) error
	DeleteFolder(context.Context, string) error
}

type AccountInfo struct {
	PremiumUntil  int64
	LimitUsed     float64
	BoosterPoints float64
}

func PremiumAt(info *AccountInfo, now time.Time) bool {
	return info != nil && info.PremiumUntil > now.Unix()
}

type Transfer struct {
	ID       string
	Name     string
	Status   string
	Progress float64
	FolderID string
	FileID   string
}

type File struct {
	ID           string
	Name         string
	RelativePath string
	Size         int64
	link         string
}
