package premiumize

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"
)

const (
	maxTransfers      = 4096
	maxFiles          = 8192
	maxFolderDepth    = 32
	maxComponentBytes = 1024
	maxIDBytes        = 1024
	maxNameBytes      = 4096
)

type envelope struct {
	Status string `json:"status"`
	Code   string `json:"code"`
}

type APIError struct{ Code string }

func (e *APIError) Error() string { return "premiumize: api error " + e.Code }

func checkEnvelope(body []byte) error {
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("premiumize: malformed response")
	}
	switch env.Status {
	case "success":
		return nil
	case "error":
		code := "unknown_error"
		if validCode(env.Code) {
			code = env.Code
		}
		return &APIError{Code: code}
	default:
		return fmt.Errorf("premiumize: invalid response status")
	}
}

func validCode(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
}

func validID(s string) bool {
	if s == "" || len(s) > maxIDBytes || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r < 0x21 || r > 0x7e {
			return false
		}
	}
	return true
}

func validName(s string) bool {
	if s == "" || len(s) > maxNameBytes || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func validComponent(s string) bool {
	return validName(s) && len(s) <= maxComponentBytes && s != "." && s != ".." &&
		!strings.ContainsAny(s, "/\\")
}

func validLink(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.User == nil && u.Host != "" && (u.Scheme == "http" || u.Scheme == "https")
}

func parseAccount(body []byte) (*AccountInfo, error) {
	if err := checkEnvelope(body); err != nil {
		return nil, err
	}
	var out struct {
		PremiumUntil  *int64  `json:"premium_until"`
		LimitUsed     float64 `json:"limit_used"`
		BoosterPoints float64 `json:"booster_points"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.LimitUsed < 0 || out.LimitUsed > 1 || out.BoosterPoints < 0 {
		return nil, fmt.Errorf("premiumize: malformed account response")
	}
	var premiumUntil int64
	if out.PremiumUntil != nil {
		premiumUntil = *out.PremiumUntil
	}
	return &AccountInfo{PremiumUntil: premiumUntil, LimitUsed: out.LimitUsed, BoosterPoints: out.BoosterPoints}, nil
}

func parseCache(body []byte) (bool, error) {
	if err := checkEnvelope(body); err != nil {
		return false, err
	}
	var out struct {
		Response []bool `json:"response"`
	}
	if err := json.Unmarshal(body, &out); err != nil || len(out.Response) != 1 {
		return false, fmt.Errorf("premiumize: malformed cache response")
	}
	return out.Response[0], nil
}

func parseCreate(body []byte) (*Transfer, error) {
	if err := checkEnvelope(body); err != nil {
		return nil, err
	}
	var out struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Type == "container" ||
		!validID(out.ID) || !validName(strings.TrimSpace(out.Name)) {
		return nil, fmt.Errorf("premiumize: malformed create response")
	}
	return &Transfer{ID: out.ID, Name: strings.TrimSpace(out.Name), Status: "queued"}, nil
}

func parseTransfers(body []byte) ([]Transfer, error) {
	if err := checkEnvelope(body); err != nil {
		return nil, err
	}
	var out struct {
		Transfers []struct {
			ID       string  `json:"id"`
			Name     string  `json:"name"`
			Status   string  `json:"status"`
			Progress float64 `json:"progress"`
			FolderID *string `json:"folder_id"`
			FileID   *string `json:"file_id"`
		} `json:"transfers"`
	}
	if err := json.Unmarshal(body, &out); err != nil || len(out.Transfers) > maxTransfers {
		return nil, fmt.Errorf("premiumize: malformed transfer response")
	}
	seen := make(map[string]struct{}, len(out.Transfers))
	result := make([]Transfer, 0, len(out.Transfers))
	for _, raw := range out.Transfers {
		if !validID(raw.ID) || !validName(strings.TrimSpace(raw.Name)) || raw.Progress < 0 || raw.Progress > 1 {
			return nil, fmt.Errorf("premiumize: invalid transfer")
		}
		if _, exists := seen[raw.ID]; exists {
			return nil, fmt.Errorf("premiumize: duplicate transfer id")
		}
		seen[raw.ID] = struct{}{}
		tr := Transfer{ID: raw.ID, Name: strings.TrimSpace(raw.Name), Status: raw.Status, Progress: raw.Progress}
		if raw.FolderID != nil {
			tr.FolderID = *raw.FolderID
			if !validID(tr.FolderID) {
				return nil, fmt.Errorf("premiumize: invalid transfer folder id")
			}
		}
		if raw.FileID != nil {
			tr.FileID = *raw.FileID
			if !validID(tr.FileID) {
				return nil, fmt.Errorf("premiumize: invalid transfer file id")
			}
		}
		switch tr.Status {
		case "queued", "running", "error":
			if tr.FolderID != "" || tr.FileID != "" {
				return nil, fmt.Errorf("premiumize: contradictory transfer state")
			}
		case "finished", "seeding":
			if tr.FolderID == "" && tr.FileID == "" {
				return nil, fmt.Errorf("premiumize: finished transfer has no content")
			}
		default:
			return nil, fmt.Errorf("premiumize: unknown transfer state")
		}
		result = append(result, tr)
	}
	return result, nil
}

type folderEntry struct {
	ID   string
	Name string
	Type string
	Size int64
	link string
}

type folderPage struct {
	ID      string
	Entries []folderEntry
}

func parseFolder(body []byte, wantID string) (*folderPage, error) {
	if err := checkEnvelope(body); err != nil {
		return nil, err
	}
	var out struct {
		FolderID string `json:"folder_id"`
		Content  []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Type string `json:"type"`
			Size int64  `json:"size"`
			Link string `json:"link"`
		} `json:"content"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.FolderID != wantID || len(out.Content) > maxFiles {
		return nil, fmt.Errorf("premiumize: malformed folder response")
	}
	page := &folderPage{ID: out.FolderID, Entries: make([]folderEntry, 0, len(out.Content))}
	seen := make(map[string]struct{}, len(out.Content))
	for _, raw := range out.Content {
		if !validID(raw.ID) || !validComponent(strings.TrimSpace(raw.Name)) {
			return nil, fmt.Errorf("premiumize: invalid folder entry")
		}
		if _, exists := seen[raw.ID]; exists {
			return nil, fmt.Errorf("premiumize: duplicate folder entry id")
		}
		seen[raw.ID] = struct{}{}
		entry := folderEntry{ID: raw.ID, Name: strings.TrimSpace(raw.Name), Type: raw.Type, Size: raw.Size, link: raw.Link}
		switch entry.Type {
		case "folder":
			if entry.Size != 0 || entry.link != "" {
				return nil, fmt.Errorf("premiumize: contradictory folder entry")
			}
		case "file":
			if entry.Size < 0 || !validLink(entry.link) {
				return nil, fmt.Errorf("premiumize: invalid file entry")
			}
		default:
			return nil, fmt.Errorf("premiumize: unknown folder entry type")
		}
		page.Entries = append(page.Entries, entry)
	}
	return page, nil
}

func parseItem(body []byte, wantID string) (*File, error) {
	if err := checkEnvelope(body); err != nil {
		return nil, err
	}
	var out struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Size int64  `json:"size"`
		Link string `json:"link"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.ID != wantID || !validID(out.ID) ||
		!validComponent(strings.TrimSpace(out.Name)) || out.Size < 0 || !validLink(out.Link) {
		return nil, fmt.Errorf("premiumize: malformed item response")
	}
	return &File{ID: out.ID, Name: strings.TrimSpace(out.Name), RelativePath: strings.TrimSpace(out.Name), Size: out.Size, link: out.Link}, nil
}

func parseDelete(body []byte) error {
	return checkEnvelope(body)
}

func isAPIErrorCode(err error, code string) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Code == code
}
