// Package arrsetup implements idempotent S9 Arr registration. The complete
// topology has two client protocols and three indexers, while goal-first setup
// installs only the selected lane surfaces. NFO extra-file import and the
// torrent-only uncached derank custom format are present via
// search-before-create against each arr's API
// (HR0.3 — LC-03 three logical lanes over two physical client contracts).
// All operations are safe to re-run — existing entries matching DH's host/port
// are detected and left untouched; missing entries are created.
package arrsetup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

const (
	dhHost = "darkharrbor"
	dhPort = 8381

	torznabBase    = "http://darkharrbor:8381/torznab/torrent-only"
	newznabBase    = "http://darkharrbor:8381/torznab/usenet-only"
	httpStreamBase = "http://darkharrbor:8381/torznab/http-stream"
	apiPath        = "/api"

	// Newznab/Torznab categories: TV-HD, TV-SD, TV-Other (Sonarr) / Movie-HD,
	// Movie-SD, Movie-Other (Radarr). Arr will fetch real caps from /api?t=caps.
	catTV    = 5000
	catTVHD  = 5040
	catTVSD  = 5045
	catMovie = 2000
	catMovHD = 2040
	catMovSD = 2030

	nameQBit       = "darkharrbor"
	nameSAB        = "darkharrbor-usenet"
	nameTorznab    = "darkharrbor"
	nameNewznab    = "darkharrbor-usenet"
	nameHTTPStream = "darkharrbor-http-stream"

	uncachedCustomFormatName  = "DarkHarrbor Uncached"
	uncachedCustomFormatToken = "DH-UNCACHED"
	uncachedCustomFormatScore = -1
)

// Creds holds the DH internal credentials needed to register download clients.
type Creds struct {
	QBitPassword string // HARRBOR_QBIT_PASSWORD
	SABAPIKey    string // HARRBOR_SAB_API_KEY
}

// Lanes selects the logical Arr surfaces to register. qBittorrent is shared by
// torrent and HTTP, while SABnzbd belongs only to NNTP.
type Lanes struct {
	Torrent bool
	NNTP    bool
	HTTP    bool
}

// Result reports what happened for one arr instance.
type Result struct {
	Name    string
	AppType string // "sonarr" or "radarr"
	Actions []string
	Err     error
}

// Register wires the legacy full 5-entry topology into every target.
// Each arr is processed independently; an error on one does not stop others.
func Register(ctx context.Context, targets []ArrTarget, creds Creds) []Result {
	return RegisterSelected(ctx, targets, creds, Lanes{Torrent: true, NNTP: true, HTTP: true})
}

// RegisterSelected wires only the acquisition lanes selected by setup.
func RegisterSelected(ctx context.Context, targets []ArrTarget, creds Creds, lanes Lanes) []Result {
	return register(ctx, targets, creds, lanes, true)
}

// Reconcile repairs missing DarkHarrbor topology entries without rewriting
// existing download clients. It is the safe existing-install counterpart to
// Register, whose setup --force contract intentionally rotates client secrets.
func Reconcile(ctx context.Context, targets []ArrTarget, creds Creds) []Result {
	return ReconcileSelected(ctx, targets, creds, Lanes{Torrent: true, NNTP: true, HTTP: true})
}

// ReconcileSelected repairs only the lane surfaces in the applied goal plan.
func ReconcileSelected(ctx context.Context, targets []ArrTarget, creds Creds, lanes Lanes) []Result {
	return register(ctx, targets, creds, lanes, false)
}

func register(ctx context.Context, targets []ArrTarget, creds Creds, lanes Lanes, rotateCredentials bool) []Result {
	out := make([]Result, 0, len(targets))
	for _, t := range targets {
		r := registerOne(ctx, t, creds, lanes, rotateCredentials)
		out = append(out, r)
	}
	return out
}

// ArrTarget is one Sonarr or Radarr instance to wire.
type ArrTarget struct {
	Name    string
	URL     string // e.g. http://sonarr-modern:8989
	AppType string // "sonarr" or "radarr"
	APIKey  string
}

func registerOne(ctx context.Context, t ArrTarget, creds Creds, lanes Lanes, rotateCredentials bool) Result {
	r := Result{Name: t.Name, AppType: t.AppType}
	c := &arrClient{baseURL: strings.TrimRight(t.URL, "/"), apiKey: t.APIKey, hc: newHTTPClient()}

	isSonarr := strings.EqualFold(t.AppType, "sonarr")

	var action string
	var err error
	if lanes.Torrent || lanes.HTTP {
		action, err = ensureQBitClient(ctx, c, creds.QBitPassword, isSonarr, rotateCredentials)
		if err != nil {
			r.Err = fmt.Errorf("qbit: %w", err)
			return r
		}
		r.Actions = append(r.Actions, "qbit:"+action)
	}

	if lanes.NNTP {
		action, err = ensureSABClient(ctx, c, creds.SABAPIKey, isSonarr, rotateCredentials)
		if err != nil {
			r.Err = fmt.Errorf("sab: %w", err)
			return r
		}
		r.Actions = append(r.Actions, "sab:"+action)
	}

	if lanes.Torrent {
		action, err = ensureTorznabIndexer(ctx, c, isSonarr)
		if err != nil {
			r.Err = fmt.Errorf("torznab: %w", err)
			return r
		}
		r.Actions = append(r.Actions, "torznab:"+action)
	}

	if lanes.NNTP {
		action, err = ensureNewznabIndexer(ctx, c, isSonarr)
		if err != nil {
			r.Err = fmt.Errorf("newznab: %w", err)
			return r
		}
		r.Actions = append(r.Actions, "newznab:"+action)
	}

	if lanes.HTTP {
		action, err = ensureHTTPStreamIndexer(ctx, c, isSonarr)
		if err != nil {
			r.Err = fmt.Errorf("http-stream: %w", err)
			return r
		}
		r.Actions = append(r.Actions, "http-stream:"+action)
	}

	// --- ID-02 NFO sidecar import ---
	action, err = ensureNFOImport(ctx, c)
	if err != nil {
		r.Err = fmt.Errorf("nfo import: %w", err)
		return r
	}
	r.Actions = append(r.Actions, "nfo-import:"+action)

	if lanes.Torrent {
		action, err = ensureUncachedCustomFormat(ctx, c)
		if err != nil {
			r.Err = fmt.Errorf("uncached custom format: %w", err)
			return r
		}
		r.Actions = append(r.Actions, "uncached-format:"+action)
	}

	return r
}

func ensureNFOImport(ctx context.Context, c *arrClient) (string, error) {
	var cfg map[string]json.RawMessage
	if err := c.get(ctx, "/api/v3/config/mediamanagement", &cfg); err != nil {
		return "", err
	}
	var id int
	var enabled bool
	var extensions string
	_ = json.Unmarshal(cfg["id"], &id)
	_ = json.Unmarshal(cfg["importExtraFiles"], &enabled)
	_ = json.Unmarshal(cfg["extraFileExtensions"], &extensions)
	if id < 1 {
		return "", fmt.Errorf("media management config has no id")
	}
	hasNFO := false
	parts := strings.Split(extensions, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
		if strings.EqualFold(parts[i], "nfo") {
			hasNFO = true
		}
	}
	if enabled && hasNFO {
		return "already-present", nil
	}
	if !hasNFO {
		parts = append(parts, "nfo")
	}
	clean := parts[:0]
	for _, part := range parts {
		if part != "" {
			clean = append(clean, part)
		}
	}
	cfg["importExtraFiles"], _ = json.Marshal(true)
	cfg["extraFileExtensions"], _ = json.Marshal(strings.Join(clean, ","))
	if err := c.put(ctx, fmt.Sprintf("/api/v3/config/mediamanagement/%d", id), cfg); err != nil {
		return "", err
	}
	return "updated", nil
}

// ── qBittorrent ──────────────────────────────────────────────────────────────

func ensureQBitClient(ctx context.Context, c *arrClient, password string, isSonarr, rotateCredentials bool) (string, error) {
	clients, err := listDownloadClients(ctx, c)
	if err != nil {
		return "", err
	}
	for _, cl := range clients {
		if cl.Implementation == "QBittorrent" && fieldStr(cl.Fields, "host") == dhHost && fieldInt(cl.Fields, "port") == dhPort {
			if !rotateCredentials {
				return "already-present", nil
			}
			// Update password in case secrets were rotated (setup --force).
			setField(cl.Fields, "password", password)
			if err := updateDownloadClient(ctx, c, cl); err != nil {
				return "", fmt.Errorf("update qbit password: %w", err)
			}
			return "updated-password", nil
		}
	}
	schema, err := downloadClientSchema(ctx, c, "QBittorrent")
	if err != nil {
		return "", err
	}
	catField := "tvCategory"
	catVal := "tv"
	if !isSonarr {
		catField = "movieCategory"
		catVal = "movies"
	}
	setField(schema.Fields, "host", dhHost)
	setField(schema.Fields, "port", dhPort)
	setField(schema.Fields, "useSsl", false)
	setField(schema.Fields, "username", "admin")
	setField(schema.Fields, "password", password)
	setField(schema.Fields, catField, catVal)
	schema.Name = nameQBit
	schema.Enable = true
	schema.RemoveCompletedDownloads = true
	if err := createDownloadClient(ctx, c, schema); err != nil {
		return "", err
	}
	return "created", nil
}

// ── SABnzbd ──────────────────────────────────────────────────────────────────

func ensureSABClient(ctx context.Context, c *arrClient, sabKey string, isSonarr, rotateCredentials bool) (string, error) {
	clients, err := listDownloadClients(ctx, c)
	if err != nil {
		return "", err
	}
	for _, cl := range clients {
		if cl.Implementation == "Sabnzbd" && fieldStr(cl.Fields, "host") == dhHost && fieldInt(cl.Fields, "port") == dhPort {
			if !rotateCredentials {
				return "already-present", nil
			}
			// Update apiKey in case secrets were rotated (setup --force).
			setField(cl.Fields, "apiKey", sabKey)
			if err := updateDownloadClient(ctx, c, cl); err != nil {
				return "", fmt.Errorf("update sab apiKey: %w", err)
			}
			return "updated-apikey", nil
		}
	}
	schema, err := downloadClientSchema(ctx, c, "Sabnzbd")
	if err != nil {
		return "", err
	}
	catField := "tvCategory"
	catVal := "tv"
	if !isSonarr {
		catField = "movieCategory"
		catVal = "movies"
	}
	setField(schema.Fields, "host", dhHost)
	setField(schema.Fields, "port", dhPort)
	setField(schema.Fields, "useSsl", false)
	setField(schema.Fields, "apiKey", sabKey)
	setField(schema.Fields, catField, catVal)
	schema.Name = nameSAB
	schema.Enable = true
	if err := createDownloadClient(ctx, c, schema); err != nil {
		return "", err
	}
	return "created", nil
}

// ── Torznab indexer ──────────────────────────────────────────────────────────

func ensureTorznabIndexer(ctx context.Context, c *arrClient, isSonarr bool) (string, error) {
	return ensureIndexer(ctx, c, "Torznab", torznabBase, nameTorznab, isSonarr)
}

// ensureIndexer reconciles one DH indexer registration identified by
// implementation + baseUrl. An existing registration (any name) matching that
// identity is left untouched; a missing one is created from the arr's schema.
func ensureIndexer(ctx context.Context, c *arrClient, impl, baseURL, name string, isSonarr bool) (string, error) {
	indexers, err := listIndexers(ctx, c)
	if err != nil {
		return "", err
	}
	for _, idx := range indexers {
		if idx.Implementation == impl && fieldStr(idx.Fields, "baseUrl") == baseURL {
			return "already-present", nil
		}
	}
	schema, err := indexerSchema(ctx, c, impl)
	if err != nil {
		return "", err
	}
	cats := []int{catTV, catTVHD, catTVSD}
	if !isSonarr {
		cats = []int{catMovie, catMovHD, catMovSD}
	}
	setField(schema.Fields, "baseUrl", baseURL)
	setField(schema.Fields, "apiPath", apiPath)
	setField(schema.Fields, "apiKey", "")
	setField(schema.Fields, "categories", cats)
	schema.Name = name
	schema.EnableRss = true
	schema.EnableAutomaticSearch = true
	schema.EnableInteractiveSearch = true
	if err := createIndexer(ctx, c, schema); err != nil {
		return "", err
	}
	return "created", nil
}

// ── Newznab indexer ──────────────────────────────────────────────────────────

func ensureNewznabIndexer(ctx context.Context, c *arrClient, isSonarr bool) (string, error) {
	return ensureIndexer(ctx, c, "Newznab", newznabBase, nameNewznab, isSonarr)
}

// ── HTTP-stream Torznab indexer (third logical lane) ─────────────────────────

func ensureHTTPStreamIndexer(ctx context.Context, c *arrClient, isSonarr bool) (string, error) {
	return ensureIndexer(ctx, c, "Torznab", httpStreamBase, nameHTTPStream, isSonarr)
}

// ── Uncached-torrent Arr-native derank ───────────────────────────────────────

type customFormatSpecification struct {
	Name           string     `json:"name"`
	Implementation string     `json:"implementation"`
	Negate         bool       `json:"negate"`
	Required       bool       `json:"required"`
	Fields         []arrField `json:"fields"`
}

type customFormatDef struct {
	ID                              int                         `json:"id,omitempty"`
	Name                            string                      `json:"name"`
	IncludeCustomFormatWhenRenaming bool                        `json:"includeCustomFormatWhenRenaming"`
	Specifications                  []customFormatSpecification `json:"specifications"`
}

type qualityFormatItem struct {
	Format int    `json:"format"`
	Name   string `json:"name,omitempty"`
	Score  int    `json:"score"`
}

func ensureUncachedCustomFormat(ctx context.Context, c *arrClient) (string, error) {
	formats, err := listCustomFormats(ctx, c)
	if err != nil {
		return "", err
	}

	var managed customFormatDef
	created := false
	definitionUpdated := false
	for _, format := range formats {
		if format.Name == uncachedCustomFormatName {
			managed = format
			break
		}
	}

	if managed.ID == 0 || !uncachedCustomFormatMatches(managed) {
		spec, err := releaseTitleSpecification(ctx, c)
		if err != nil {
			return "", err
		}
		managed.Name = uncachedCustomFormatName
		managed.IncludeCustomFormatWhenRenaming = false
		managed.Specifications = []customFormatSpecification{spec}
		if managed.ID == 0 {
			if err := c.postInto(ctx, "/api/v3/customformat", managed, &managed); err != nil {
				return "", err
			}
			created = true
		} else if err := c.put(ctx, fmt.Sprintf("/api/v3/customformat/%d", managed.ID), managed); err != nil {
			return "", err
		} else {
			definitionUpdated = true
		}
	}
	if managed.ID == 0 {
		return "", fmt.Errorf("arr returned custom format without an id")
	}

	updatedProfiles, incompatibleProfiles, err := reconcileQualityProfiles(ctx, c, managed.ID)
	if err != nil {
		return "", err
	}
	state := "already-present"
	if created {
		state = "created"
	} else if definitionUpdated {
		state = "updated"
	}
	return fmt.Sprintf("%s,profiles-updated=%d,profile-floor-incompatible=%d",
		state, updatedProfiles, incompatibleProfiles), nil
}

func listCustomFormats(ctx context.Context, c *arrClient) ([]customFormatDef, error) {
	var out []customFormatDef
	return out, c.get(ctx, "/api/v3/customformat", &out)
}

func releaseTitleSpecification(ctx context.Context, c *arrClient) (customFormatSpecification, error) {
	var schemas []customFormatSpecification
	if err := c.get(ctx, "/api/v3/customformat/schema", &schemas); err != nil {
		return customFormatSpecification{}, err
	}
	for _, spec := range schemas {
		if spec.Implementation != "ReleaseTitleSpecification" {
			continue
		}
		spec.Name = uncachedCustomFormatName
		spec.Negate = false
		spec.Required = true
		for i := range spec.Fields {
			if spec.Fields[i].Name == "value" {
				spec.Fields[i].Value = `(?i)(?:^|[ ._-])DH-UNCACHED(?:$|[ ._-])`
				return spec, nil
			}
		}
		return customFormatSpecification{}, fmt.Errorf("release-title custom format schema has no value field")
	}
	return customFormatSpecification{}, fmt.Errorf("release-title custom format schema not found")
}

func uncachedCustomFormatMatches(format customFormatDef) bool {
	if format.Name != uncachedCustomFormatName ||
		format.IncludeCustomFormatWhenRenaming ||
		len(format.Specifications) != 1 {
		return false
	}
	spec := format.Specifications[0]
	if spec.Implementation != "ReleaseTitleSpecification" || spec.Negate || !spec.Required {
		return false
	}
	for _, field := range spec.Fields {
		if field.Name == "value" {
			value, _ := field.Value.(string)
			return strings.Contains(value, uncachedCustomFormatToken)
		}
	}
	return false
}

func reconcileQualityProfiles(ctx context.Context, c *arrClient, formatID int) (int, int, error) {
	var profiles []map[string]json.RawMessage
	if err := c.get(ctx, "/api/v3/qualityprofile", &profiles); err != nil {
		return 0, 0, err
	}
	updated := 0
	incompatible := 0
	for _, profile := range profiles {
		var id int
		if err := json.Unmarshal(profile["id"], &id); err != nil || id == 0 {
			return updated, incompatible, fmt.Errorf("quality profile has invalid id")
		}
		var minimum int
		if raw, ok := profile["minFormatScore"]; ok {
			if err := json.Unmarshal(raw, &minimum); err != nil {
				return updated, incompatible, fmt.Errorf("quality profile %d has invalid minimum format score", id)
			}
		}
		if minimum > uncachedCustomFormatScore {
			incompatible++
		}

		var items []qualityFormatItem
		if raw, ok := profile["formatItems"]; ok {
			if err := json.Unmarshal(raw, &items); err != nil {
				return updated, incompatible, fmt.Errorf("quality profile %d has invalid format items", id)
			}
		}
		changed := false
		found := false
		for i := range items {
			if items[i].Format != formatID {
				continue
			}
			found = true
			if items[i].Score != uncachedCustomFormatScore {
				items[i].Score = uncachedCustomFormatScore
				changed = true
			}
		}
		if !found {
			items = append(items, qualityFormatItem{
				Format: formatID,
				Name:   uncachedCustomFormatName,
				Score:  uncachedCustomFormatScore,
			})
			changed = true
		}
		if !changed {
			continue
		}
		raw, err := json.Marshal(items)
		if err != nil {
			return updated, incompatible, err
		}
		profile["formatItems"] = raw
		if err := c.put(ctx, fmt.Sprintf("/api/v3/qualityprofile/%d", id), profile); err != nil {
			return updated, incompatible, err
		}
		updated++
	}
	return updated, incompatible, nil
}

// ── arr API types ─────────────────────────────────────────────────────────────

type arrField struct {
	Name  string      `json:"name"`
	Value interface{} `json:"value,omitempty"`
}

type downloadClientDef struct {
	ID                       int        `json:"id,omitempty"`
	Name                     string     `json:"name"`
	Enable                   bool       `json:"enable"`
	Priority                 int        `json:"priority"`
	Implementation           string     `json:"implementation"`
	ConfigContract           string     `json:"configContract"`
	Protocol                 string     `json:"protocol"`
	RemoveCompletedDownloads bool       `json:"removeCompletedDownloads"`
	Fields                   []arrField `json:"fields"`
}

type indexerDef struct {
	ID                      int        `json:"id,omitempty"`
	Name                    string     `json:"name"`
	Priority                int        `json:"priority"`
	Implementation          string     `json:"implementation"`
	ConfigContract          string     `json:"configContract"`
	Protocol                string     `json:"protocol"`
	EnableRss               bool       `json:"enableRss"`
	EnableAutomaticSearch   bool       `json:"enableAutomaticSearch"`
	EnableInteractiveSearch bool       `json:"enableInteractiveSearch"`
	Fields                  []arrField `json:"fields"`
}

// ── arr API client ────────────────────────────────────────────────────────────

type arrClient struct {
	baseURL string
	apiKey  string
	hc      *http.Client
}

func newHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 10 * time.Second,
			}).DialContext,
		},
	}
}

func (c *arrClient) get(ctx context.Context, path string, out interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", c.apiKey)
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("GET %s: status %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *arrClient) put(ctx context.Context, path string, body interface{}) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.baseURL+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("PUT %s: status %d: %s", path, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}

func (c *arrClient) post(ctx context.Context, path string, body interface{}) error {
	return c.postInto(ctx, path, body, nil)
}

func (c *arrClient) postInto(ctx context.Context, path string, body, out interface{}) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("POST %s: status %d", path, resp.StatusCode)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func listDownloadClients(ctx context.Context, c *arrClient) ([]downloadClientDef, error) {
	var out []downloadClientDef
	return out, c.get(ctx, "/api/v3/downloadclient", &out)
}

func downloadClientSchema(ctx context.Context, c *arrClient, impl string) (downloadClientDef, error) {
	var schemas []downloadClientDef
	if err := c.get(ctx, "/api/v3/downloadclient/schema", &schemas); err != nil {
		return downloadClientDef{}, err
	}
	for _, s := range schemas {
		if s.Implementation == impl {
			return s, nil
		}
	}
	return downloadClientDef{}, fmt.Errorf("schema not found for implementation %q", impl)
}

func createDownloadClient(ctx context.Context, c *arrClient, def downloadClientDef) error {
	// DarkHarrbor is not serving yet during first-use setup, so the arr cannot
	// live-test this otherwise valid client until the deployment starts.
	return c.post(ctx, "/api/v3/downloadclient?forceSave=true", def)
}

func updateDownloadClient(ctx context.Context, c *arrClient, def downloadClientDef) error {
	// forceSave=true skips the live connection test during PUT — required when
	// updating credentials before the DH container has restarted with new secrets.
	if def.Priority == 0 {
		def.Priority = 1 // arr rejects priority=0; default to 1
	}
	return c.put(ctx, fmt.Sprintf("/api/v3/downloadclient/%d?forceSave=true", def.ID), def)
}

func listIndexers(ctx context.Context, c *arrClient) ([]indexerDef, error) {
	var out []indexerDef
	return out, c.get(ctx, "/api/v3/indexer", &out)
}

func indexerSchema(ctx context.Context, c *arrClient, impl string) (indexerDef, error) {
	var schemas []indexerDef
	if err := c.get(ctx, "/api/v3/indexer/schema", &schemas); err != nil {
		return indexerDef{}, err
	}
	for _, s := range schemas {
		if s.Implementation == impl {
			return s, nil
		}
	}
	return indexerDef{}, fmt.Errorf("schema not found for implementation %q", impl)
}

func createIndexer(ctx context.Context, c *arrClient, def indexerDef) error {
	// forceSave=true skips the arr's live test-search during POST. DH's feeds
	// legitimately answer window/test queries with an authoritative empty feed
	// (DH is not a discovery indexer), which some arr versions surface as a
	// blocking validation failure without forceSave.
	return c.post(ctx, "/api/v3/indexer?forceSave=true", def)
}

// ── field helpers ─────────────────────────────────────────────────────────────

func fieldStr(fields []arrField, name string) string {
	for _, f := range fields {
		if f.Name == name {
			if s, ok := f.Value.(string); ok {
				return s
			}
		}
	}
	return ""
}

func fieldInt(fields []arrField, name string) int {
	for _, f := range fields {
		if f.Name == name {
			switch v := f.Value.(type) {
			case float64:
				return int(v)
			case int:
				return v
			}
		}
	}
	return 0
}

func setField(fields []arrField, name string, value interface{}) {
	for i, f := range fields {
		if f.Name == name {
			fields[i].Value = value
			return
		}
	}
	// Field not in schema (e.g. Radarr vs Sonarr category name) — append it.
	// This handles cases where the schema omits optional fields.
}
