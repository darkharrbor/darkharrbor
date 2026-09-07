package reactivequeue

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

const maxArrTargets = 64
const maxArrBody = 8 << 20
const maxArrRows = 10000

type AssignmentRepository interface {
	GetReactiveAssignment(context.Context, string, string, string) (store.ReactiveAssignment, bool, error)
	PutReactiveAssignment(context.Context, store.ReactiveAssignment, time.Time) error
}

type Router struct {
	repo    AssignmentRepository
	targets []config.ArrTarget
	// defaults are RD-34 D3's OPTIONAL per-kind fallback destinations, keyed
	// "series" and "movie". Empty is the shipping default.
	defaults map[string]string
	client   *http.Client
	now      func() time.Time
}

type RouteResult struct {
	ArrName string
	Reason  string
	Tier    int
	Owned   bool
	// Declared is set when RX-9.4 selected this target from its DECLARED
	// scope for an identity NO Arr owns. RootFolder and QualityProfile are
	// then the declared values automatic dispatch must pass to `addOwner`.
	// They are populated only in that case and are empty for an owned route,
	// where the Arr already holds the item and nothing is added.
	Declared       bool
	RootFolder     string
	QualityProfile int
}

type arrRow struct {
	ID     int    `json:"id"`
	TVDBID int    `json:"tvdbId"`
	TMDBID int    `json:"tmdbId"`
	IMDBID string `json:"imdbId"`
}

func NewRouter(repo AssignmentRepository, targets []config.ArrTarget, now func() time.Time) (*Router, error) {
	if repo == nil || len(targets) > maxArrTargets {
		return nil, errors.New("reactivequeue: invalid routing dependencies")
	}
	if now == nil {
		now = time.Now
	}
	return &Router{repo: repo, targets: append([]config.ArrTarget(nil), targets...), defaults: map[string]string{}, client: &http.Client{Timeout: 10 * time.Second}, now: now}, nil
}

// SetDefaultDestinations records RD-34 D3's optional per-kind fallback. It is
// applied ONLY when nothing owns the identity and more than one instance of the
// kind exists; a named instance that is not configured, or cannot add, is
// ignored so a stale name parks rather than misfiling.
func (r *Router) SetDefaultDestinations(series, movie string) {
	r.defaults = map[string]string{
		"series": strings.TrimSpace(series),
		"movie":  strings.TrimSpace(movie),
	}
}

func (r *Router) Resolve(ctx context.Context, identity *store.ProviderIdentity) (RouteResult, error) {
	provider, value, ok := routingKey(identity)
	if !ok {
		return RouteResult{Reason: "routing_ambiguous", Tier: 3}, nil
	}
	if assignment, found, err := r.repo.GetReactiveAssignment(ctx, identity.Kind, provider, value); err != nil {
		return RouteResult{}, err
	} else if found {
		for _, target := range r.targets {
			if target.Name != assignment.ArrName {
				continue
			}
			owns, app, err := r.targetState(ctx, target, identity)
			if err != nil {
				return RouteResult{}, err
			}
			if app {
				result := RouteResult{ArrName: assignment.ArrName, Tier: 2, Owned: owns}
				if !owns {
					// RX-9.4: a remembered assignment names the Arr but does
					// not carry a root folder or quality profile. Only a
					// DECLARED target supplies those, so an undeclared one
					// still parks rather than adding blind.
					result = addableRoute(target, result)
				}
				return result, nil
			}
			return RouteResult{Reason: "routing_unresolved", Tier: 2}, nil
		}
		return RouteResult{Reason: "routing_unresolved", Tier: 2}, nil
	}
	type targetState struct {
		target config.ArrTarget
		owns   bool
	}
	var compatible []targetState
	for _, target := range r.targets {
		owns, app, err := r.targetState(ctx, target, identity)
		if err != nil {
			return RouteResult{}, err
		}
		if app {
			compatible = append(compatible, targetState{target: target, owns: owns})
		}
	}
	if len(compatible) == 1 {
		// Tier and ArrName are UNCHANGED from before RX-9.4, so RX-4.2's queue
		// still sees the same suggestion. Only the declared add fields are
		// added, and only when this target has declared the kind; an
		// undeclared one still carries Owned=false and Declared=false and is
		// parked by the dispatcher exactly as it was.
		result := RouteResult{ArrName: compatible[0].target.Name, Tier: 0, Owned: compatible[0].owns}
		if !compatible[0].owns {
			result = addableRoute(compatible[0].target, result)
		}
		return result, nil
	}
	var owners []config.ArrTarget
	for _, candidate := range compatible {
		if candidate.owns {
			owners = append(owners, candidate.target)
		}
	}
	if len(owners) == 1 {
		assignment := store.ReactiveAssignment{Kind: identity.Kind, Provider: provider, Value: value, ArrName: owners[0].Name}
		if err := r.repo.PutReactiveAssignment(ctx, assignment, r.now().UTC()); err != nil {
			return RouteResult{}, err
		}
		return RouteResult{ArrName: owners[0].Name, Tier: 1, Owned: true}, nil
	}
	if len(owners) > 1 {
		return RouteResult{Reason: "routing_ambiguous", Tier: 3}, nil
	}
	// RX-9.4 / RD-34 D2: nothing owns this identity and MORE THAN ONE instance
	// of its kind exists, so the destination is NOT DETERMINED and never will
	// be -- nothing in the Stremio coordinate maps to a user's private split,
	// and DarkHarrbor must never interpret what their labels MEAN. The single
	// exception is RD-34 D3's explicitly named default, which is a destination
	// the operator chose, not an inference. Absent that, park for a human.
	if name := r.defaults[identity.Kind]; name != "" {
		for _, candidate := range compatible {
			if candidate.target.Name != name || !candidate.target.CanAdd() {
				continue
			}
			return addableRoute(candidate.target, RouteResult{
				ArrName: candidate.target.Name, Tier: 2, Owned: false,
			}), nil
		}
	}
	return RouteResult{Reason: "routing_unresolved", Tier: 2}, nil
}

// addableRoute fills RX-9.4's add fields when the instance carries the
// wizard-captured root folder and quality profile, and otherwise leaves the
// result unchanged so the dispatcher parks it exactly as it did before RX-9.4.
func addableRoute(target config.ArrTarget, result RouteResult) RouteResult {
	if !target.CanAdd() {
		return result
	}
	result.Declared = true
	result.RootFolder = target.RootFolder
	result.QualityProfile = target.QualityProfile
	return result
}

func (r *Router) Remember(ctx context.Context, identity *store.ProviderIdentity, arrName string) error {
	provider, value, ok := routingKey(identity)
	if !ok || !r.hasTarget(arrName) {
		return errors.New("reactivequeue: invalid explicit assignment")
	}
	return r.repo.PutReactiveAssignment(ctx, store.ReactiveAssignment{Kind: identity.Kind, Provider: provider, Value: value, ArrName: arrName}, r.now().UTC())
}

func (r *Router) hasTarget(name string) bool {
	for _, target := range r.targets {
		if target.Name == name {
			return true
		}
	}
	return false
}

func (r *Router) targetState(ctx context.Context, target config.ArrTarget, identity *store.ProviderIdentity) (owns, app bool, err error) {
	provider, value, ok := routingKey(identity)
	if !ok {
		return false, false, nil
	}
	resource := "movie"
	queryName := "tmdbId"
	if identity.Kind == "series" {
		resource = "series"
		queryName = "tvdbId"
	}
	path := "/api/v3/" + resource
	if provider != "imdb" {
		path += "?" + queryName + "=" + url.QueryEscape(value)
	}
	status, rows, err := r.doArrRows(ctx, target, path)
	if err != nil {
		return false, false, err
	}
	if status == http.StatusNotFound {
		return false, false, nil
	}
	if status != http.StatusOK {
		return false, false, nil
	}
	if len(rows) > maxArrRows {
		return false, false, errors.New("reactivequeue: Arr row limit exceeded")
	}
	if provider != "imdb" {
		return len(rows) > 0, true, nil
	}
	for _, row := range rows {
		if strings.EqualFold(strings.TrimSpace(row.IMDBID), value) {
			return true, true, nil
		}
	}
	return false, true, nil
}

func (r *Router) doArrRows(ctx context.Context, target config.ArrTarget, path string) (int, []arrRow, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.BaseURL+path, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("X-Api-Key", target.APIKey)
	resp, err := r.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, nil, nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxArrBody+1))
	if err != nil || len(data) > maxArrBody {
		return resp.StatusCode, nil, errors.New("reactivequeue: bounded Arr response failed")
	}
	rows, err := decodeArrRows(data)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, rows, nil
}

func decodeArrRows(data []byte) ([]arrRow, error) {
	var rows []arrRow
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, errors.New("reactivequeue: malformed Arr response")
	}
	if len(rows) > maxArrRows {
		return nil, errors.New("reactivequeue: Arr row limit exceeded")
	}
	return rows, nil
}

func routingKey(identity *store.ProviderIdentity) (string, string, bool) {
	if identity == nil {
		return "", "", false
	}
	switch identity.Kind {
	case "series":
		if value := strings.TrimSpace(identity.IDs.TVDB); value != "" {
			return "tvdb", value, true
		}
		if value := strings.TrimSpace(identity.IDs.IMDB); value != "" {
			return "imdb", value, true
		}
	case "movie":
		if value := strings.TrimSpace(identity.IDs.TMDB); value != "" {
			return "tmdb", value, true
		}
		if value := strings.TrimSpace(identity.IDs.IMDB); value != "" {
			return "imdb", value, true
		}
	}
	return "", "", false
}
