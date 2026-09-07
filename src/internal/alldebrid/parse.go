package alldebrid

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path"
	"strconv"
	"strings"
)

const (
	maxFiles         = 8192
	maxFileDepth     = 32
	maxComponentSize = 1024
)

type envelope struct {
	Status string          `json:"status"`
	Data   json.RawMessage `json:"data"`
	Error  *apiError       `json:"error"`
}

type apiError struct {
	Code string `json:"code"`
}

type fileNode struct {
	Name     string     `json:"n"`
	Size     int64      `json:"s"`
	Link     string     `json:"l"`
	Children []fileNode `json:"e"`
}

func dataFrom(body []byte) (json.RawMessage, error) {
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("alldebrid: malformed response")
	}
	switch env.Status {
	case "success":
		if len(env.Data) == 0 || string(env.Data) == "null" {
			return nil, fmt.Errorf("alldebrid: missing response data")
		}
		return env.Data, nil
	case "error":
		code := "UNKNOWN"
		if env.Error != nil && validCode(env.Error.Code) {
			code = env.Error.Code
		}
		return nil, &APIError{Code: code}
	default:
		return nil, fmt.Errorf("alldebrid: invalid response status")
	}
}

type APIError struct{ Code string }

func (e *APIError) Error() string { return "alldebrid: api error " + e.Code }

func validCode(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
}

func parseUser(body []byte) (*AccountInfo, error) {
	data, err := dataFrom(body)
	if err != nil {
		return nil, err
	}
	var out struct {
		User struct {
			Premium      bool  `json:"isPremium"`
			PremiumUntil int64 `json:"premiumUntil"`
		} `json:"user"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("alldebrid: malformed user response")
	}
	return &AccountInfo{Premium: out.User.Premium, PremiumUntil: out.User.PremiumUntil}, nil
}

func parseUpload(body []byte) (*Upload, error) {
	data, err := dataFrom(body)
	if err != nil {
		return nil, err
	}
	var out struct {
		Magnets []struct {
			ID    json.Number `json:"id"`
			Hash  string      `json:"hash"`
			Name  string      `json:"name"`
			Size  int64       `json:"size"`
			Ready bool        `json:"ready"`
			Error *apiError   `json:"error"`
		} `json:"magnets"`
	}
	if err := json.Unmarshal(data, &out); err != nil || len(out.Magnets) != 1 {
		return nil, fmt.Errorf("alldebrid: malformed upload response")
	}
	m := out.Magnets[0]
	if m.Error != nil {
		code := "UNKNOWN"
		if validCode(m.Error.Code) {
			code = m.Error.Code
		}
		return nil, &APIError{Code: code}
	}
	id := m.ID.String()
	if id == "" || id == "0" {
		return nil, fmt.Errorf("alldebrid: upload returned empty id")
	}
	return &Upload{ID: id, Hash: strings.ToLower(m.Hash), Name: m.Name, Size: m.Size, Ready: m.Ready}, nil
}

func parseMagnets(body []byte) ([]Magnet, error) {
	data, err := dataFrom(body)
	if err != nil {
		return nil, err
	}
	var out struct {
		Magnets []struct {
			ID         json.Number `json:"id"`
			Name       string      `json:"filename"`
			Size       int64       `json:"size"`
			Status     string      `json:"status"`
			StatusCode int         `json:"statusCode"`
			Downloaded int64       `json:"downloaded"`
			Seeders    int         `json:"seeders"`
		} `json:"magnets"`
	}
	if err := json.Unmarshal(data, &out); err != nil || len(out.Magnets) > 1000 {
		return nil, fmt.Errorf("alldebrid: malformed status response")
	}
	result := make([]Magnet, 0, len(out.Magnets))
	for _, m := range out.Magnets {
		id := m.ID.String()
		if id == "" || id == "0" || m.StatusCode < 0 || m.StatusCode > 15 || m.Size < 0 || m.Downloaded < 0 {
			return nil, fmt.Errorf("alldebrid: invalid magnet status")
		}
		result = append(result, Magnet{ID: id, Name: m.Name, Size: m.Size, Status: m.Status, StatusCode: m.StatusCode, Downloaded: m.Downloaded, Seeders: m.Seeders})
	}
	return result, nil
}

func parseFiles(body []byte, wantID string) ([]File, error) {
	data, err := dataFrom(body)
	if err != nil {
		return nil, err
	}
	var out struct {
		Magnets []struct {
			ID    json.Number `json:"id"`
			Files []fileNode  `json:"files"`
			Error *apiError   `json:"error"`
		} `json:"magnets"`
	}
	if err := json.Unmarshal(data, &out); err != nil || len(out.Magnets) != 1 {
		return nil, fmt.Errorf("alldebrid: malformed files response")
	}
	m := out.Magnets[0]
	if m.ID.String() != wantID || m.Error != nil {
		return nil, fmt.Errorf("alldebrid: files response does not match requested magnet")
	}
	files := make([]File, 0)
	if err := flattenFiles(m.Files, "", 0, &files); err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("alldebrid: magnet has no files")
	}
	seen := make(map[string]struct{}, len(files))
	for _, file := range files {
		if _, exists := seen[file.ID]; exists {
			return nil, fmt.Errorf("alldebrid: ambiguous duplicate file identity")
		}
		seen[file.ID] = struct{}{}
	}
	return files, nil
}

func flattenFiles(nodes []fileNode, parent string, depth int, out *[]File) error {
	if depth > maxFileDepth {
		return fmt.Errorf("alldebrid: file tree exceeds depth limit")
	}
	for _, n := range nodes {
		name := strings.TrimSpace(n.Name)
		if name == "" || len(name) > maxComponentSize || name == "." || name == ".." || strings.ContainsAny(name, "/\\") {
			return fmt.Errorf("alldebrid: invalid file-tree component")
		}
		rel := path.Join(parent, name)
		isDir := len(n.Children) > 0
		if isDir {
			if n.Link != "" || n.Size != 0 {
				return fmt.Errorf("alldebrid: contradictory file-tree node")
			}
			if err := flattenFiles(n.Children, rel, depth+1, out); err != nil {
				return err
			}
			continue
		}
		if n.Link == "" || n.Size < 0 {
			return fmt.Errorf("alldebrid: invalid file node")
		}
		if len(*out) >= maxFiles {
			return fmt.Errorf("alldebrid: file tree exceeds file limit")
		}
		identity := sha256.Sum256([]byte(rel + "\x00" + strconv.FormatInt(n.Size, 10)))
		id := fmt.Sprintf("%x", identity)
		*out = append(*out, File{ID: id, Name: name, RelativePath: rel, Size: n.Size, link: n.Link})
	}
	return nil
}

func parseUnlock(body []byte) (string, error) {
	data, err := dataFrom(body)
	if err != nil {
		return "", err
	}
	var out struct {
		Link string `json:"link"`
	}
	if err := json.Unmarshal(data, &out); err != nil || strings.TrimSpace(out.Link) == "" {
		return "", fmt.Errorf("alldebrid: unlock returned no direct link")
	}
	return out.Link, nil
}
