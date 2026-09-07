package reactivecommit

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/config"
)

const maxMonitorFlipIDs = 1000

type MonitorFlipRequest struct {
	TargetName string
	Kind       string
	IDs        []int
	Rate       int
	Interval   time.Duration
}

type MonitorFlipResult struct {
	Flipped  int
	Searched int
}

// PacedMonitorFlip explicitly monitors and searches a bounded Arr selection.
// Completed items remain monitored if the caller cancels between batches.
func (c *ArrClient) PacedMonitorFlip(ctx context.Context, req MonitorFlipRequest) (MonitorFlipResult, error) {
	var result MonitorFlipResult
	if req.TargetName == "" || (req.Kind != "episode" && req.Kind != "movie") ||
		len(req.IDs) == 0 || len(req.IDs) > maxMonitorFlipIDs || req.Rate < 1 ||
		req.Rate > 100 || req.Interval < time.Second || req.Interval > time.Hour {
		return result, errors.New("reactivecommit: invalid monitor flip request")
	}
	target, err := c.monitorFlipTarget(req.TargetName)
	if err != nil {
		return result, err
	}
	seen := make(map[int]struct{}, len(req.IDs))
	for _, id := range req.IDs {
		if id < 1 {
			return result, errors.New("reactivecommit: invalid monitor flip ID")
		}
		if _, exists := seen[id]; exists {
			return result, errors.New("reactivecommit: duplicate monitor flip ID")
		}
		seen[id] = struct{}{}
		monitored, _, err := c.monitorFlipState(ctx, target, req.Kind, id)
		if err != nil {
			return result, err
		}
		if monitored {
			return result, errors.New("reactivecommit: monitor flip item is already monitored")
		}
	}

	for i, id := range req.IDs {
		if i > 0 && i%req.Rate == 0 {
			if err := c.sleep(ctx, req.Interval); err != nil {
				return result, err
			}
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
		monitored, body, err := c.monitorFlipState(ctx, target, req.Kind, id)
		if err != nil {
			return result, err
		}
		if monitored {
			return result, errors.New("reactivecommit: monitor flip state changed")
		}
		body["monitored"] = true
		path := "/api/v3/episode/" + strconv.Itoa(id)
		if req.Kind == "movie" {
			path = "/api/v3/movie/" + strconv.Itoa(id)
		}
		status, err := c.doJSON(ctx, target, http.MethodPut, path, body, &map[string]any{})
		if err != nil || (status != http.StatusOK && status != http.StatusAccepted) {
			return result, errors.New("reactivecommit: monitor flip update failed")
		}
		result.Flipped++

		command := map[string]any{"name": "EpisodeSearch", "episodeIds": []int{id}}
		if req.Kind == "movie" {
			command = map[string]any{"name": "MoviesSearch", "movieIds": []int{id}}
		}
		status, err = c.doJSON(ctx, target, http.MethodPost, "/api/v3/command", command, nil)
		if err != nil || (status != http.StatusOK && status != http.StatusCreated && status != http.StatusAccepted) {
			return result, errors.New("reactivecommit: monitor flip search failed")
		}
		result.Searched++
	}
	return result, nil
}

func (c *ArrClient) monitorFlipTarget(name string) (config.ArrTarget, error) {
	var target *config.ArrTarget
	for i := range c.targets {
		if c.targets[i].Name != name {
			continue
		}
		if target != nil {
			return config.ArrTarget{}, errors.New("reactivecommit: duplicate monitor flip target")
		}
		target = &c.targets[i]
	}
	if target == nil {
		return config.ArrTarget{}, errors.New("reactivecommit: monitor flip target unavailable")
	}
	return *target, nil
}

func (c *ArrClient) monitorFlipState(ctx context.Context, target config.ArrTarget, kind string, id int) (bool, map[string]any, error) {
	path := "/api/v3/episode/" + strconv.Itoa(id)
	if kind == "movie" {
		path = "/api/v3/movie/" + strconv.Itoa(id)
	}
	var body map[string]any
	status, err := c.doJSON(ctx, target, http.MethodGet, path, nil, &body)
	if err != nil || status != http.StatusOK {
		return false, nil, errors.New("reactivecommit: monitor flip lookup failed")
	}
	monitored, ok := body["monitored"].(bool)
	if !ok {
		return false, nil, errors.New("reactivecommit: malformed monitor flip state")
	}
	return monitored, body, nil
}
