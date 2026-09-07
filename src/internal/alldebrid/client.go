package alldebrid

import "context"

const ActiveMagnetLimit = 30

type Client interface {
	GetUser(context.Context) (*AccountInfo, error)
	UploadMagnet(context.Context, string) (*Upload, error)
	GetMagnet(context.Context, string) (*Magnet, error)
	GetFiles(context.Context, string) ([]File, error)
	UnlockLink(context.Context, string) (string, error)
	DeleteMagnet(context.Context, string) error
	ActiveCount(context.Context) (int, error)
}

type AccountInfo struct {
	Premium      bool
	PremiumUntil int64
}

type Upload struct {
	ID    string
	Hash  string
	Name  string
	Size  int64
	Ready bool
}

type Magnet struct {
	ID         string
	Name       string
	Size       int64
	Status     string
	StatusCode int
	Downloaded int64
	Seeders    int
}

type File struct {
	ID           string
	Name         string
	RelativePath string
	Size         int64
	link         string
}
