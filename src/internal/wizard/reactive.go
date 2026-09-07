package wizard

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/store"
	"github.com/darkharrbor/darkharrbor/internal/topology"
)

const maxReactiveArrProbes = 64

type arrReachability func(context.Context, config.ArrTarget) bool

func anyReachableArr(targets []config.ArrTarget, probe arrReachability) bool {
	if probe == nil {
		probe = probeArr
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for i, target := range targets {
		if i >= maxReactiveArrProbes || ctx.Err() != nil {
			break
		}
		if probe(ctx, target) {
			return true
		}
	}
	return false
}

func probeArr(ctx context.Context, target config.ArrTarget) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.BaseURL+"/api/v3/system/status", nil)
	if err != nil {
		return false
	}
	req.Header.Set("X-Api-Key", target.APIKey)
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func configureReactive(p *configurePrompter, cfg *config.Config, values map[string]string, probe arrReachability, destinations arrDestinationStore, promotionSelected, topologyLocked bool) error {
	fmt.Fprintln(p.out, "\nReactive library")
	if !anyReachableArr(cfg.Arrs, probe) {
		values["HARRBOR_REACTIVE_ENABLED"] = "false"
		values["HARRBOR_REACTIVE_COMMIT_MODE"] = "off"
		fmt.Fprintln(p.out, "No reachable Sonarr or Radarr instance; reactive library remains OFF.")
		return nil
	}

	active := cfg.Topology.Active
	if _, ok := topology.Profile(active); !ok {
		active = topology.T1
	}
	// A stale/manual t3+off combination must not turn Enter into consent.
	// Require the operator to type t3 whenever the current lane is OFF.
	if active == topology.T3 && cfg.Reactive.CommitMode == "off" {
		active = topology.T1
	}
	selectedTopology := active
	if !topologyLocked {
		selected, err := p.text("Deployment topology (t1 arr-native|t2 aggregate playback|t3 full reactive)", string(active))
		if err != nil {
			return err
		}
		var ok bool
		selectedTopology, ok = topology.Parse(selected)
		if !ok {
			return fmt.Errorf("deployment topology must be one of t1|t2|t3")
		}
	} else {
		fmt.Fprintln(p.out, "Deployment behavior was derived from the approved first-run choices.")
	}
	values["HARRBOR_TOPOLOGY"] = string(selectedTopology)
	if selectedTopology != topology.T3 {
		values["HARRBOR_REACTIVE_ENABLED"] = "false"
		values["HARRBOR_REACTIVE_COMMIT_MODE"] = "off"
		values["HARRBOR_REACTIVE_PROMOTION_ENABLED"] = "false"
		return nil
	}

	mode := cfg.Reactive.CommitMode
	if mode == "off" || !config.ValidReactiveCommitMode(mode) {
		mode = "supervised"
	}
	mode, err := p.text("Commit mode (supervised|auto)", mode)
	if err != nil {
		return err
	}
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode != "supervised" && mode != "auto" {
		return fmt.Errorf("commit mode must be supervised or auto")
	}
	threshold, err := p.fraction("Playback threshold", cfg.Reactive.Threshold)
	if err != nil {
		return err
	}
	monitorEpisodes, err := p.yesNo("Monitor committed episodes", cfg.Reactive.MonitorEpisodes)
	if err != nil {
		return err
	}
	monitorMovies, err := p.yesNo("Monitor committed movies", cfg.Reactive.MonitorMovies)
	if err != nil {
		return err
	}
	floor := cfg.Reactive.PromotionFreePercent
	if promotionSelected {
		floor, _, err = p.integer("Promotion free-space floor (percent)", floor, 1)
		if err != nil || floor > 99 {
			return fmt.Errorf("promotion free-space floor must be from 1 through 99")
		}
	} else {
		fmt.Fprintln(p.out, "Promotion settings skipped: promotion was declined in the goal plan.")
	}
	values["HARRBOR_REACTIVE_ENABLED"] = "true"
	values["HARRBOR_REACTIVE_COMMIT_MODE"] = mode
	values["HARRBOR_REACTIVE_THRESHOLD"] = strconv.FormatFloat(threshold, 'f', -1, 64)
	values["HARRBOR_REACTIVE_MONITOR_EPISODES"] = strconv.FormatBool(monitorEpisodes)
	values["HARRBOR_REACTIVE_MONITOR_MOVIES"] = strconv.FormatBool(monitorMovies)
	values["HARRBOR_REACTIVE_PROMOTION_ENABLED"] = strconv.FormatBool(promotionSelected)
	values["HARRBOR_REACTIVE_PROMOTION_FREE_PERCENT"] = strconv.Itoa(floor)
	if err := captureReactiveDestinations(p, cfg, values, destinations); err != nil {
		return err
	}
	return nil
}

func (p *configurePrompter) fraction(label string, current float64) (float64, error) {
	raw, err := p.text(label, strconv.FormatFloat(current, 'f', -1, 64))
	if err != nil {
		return 0, err
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || value <= 0 || value > 1 {
		return 0, fmt.Errorf("%s must be greater than 0 and at most 1", label)
	}
	return value, nil
}

func runReactiveSetup(out io.Writer, cfgDir string, db *sql.DB, instances []discoveredArrInstance, plan onboardingPlan) error {
	path := filepath.Join(cfgDir, filepath.Base(config.DefaultTuningPath))
	values, err := config.ReadTuning(path)
	if err != nil {
		return err
	}
	current := config.Config{}
	for _, instance := range instances {
		if instance.AppType != "sonarr" && instance.AppType != "radarr" {
			continue
		}
		current.Arrs = append(current.Arrs, config.ArrTarget{Name: instance.Name, BaseURL: strings.TrimRight(instance.URL, "/"), APIKey: instance.APIKey})
	}
	if len(current.Arrs) == 0 {
		values["HARRBOR_REACTIVE_ENABLED"] = "false"
		values["HARRBOR_REACTIVE_COMMIT_MODE"] = "off"
		fmt.Fprintln(out, "\n── S7d Reactive Library ───────────────────────────────")
		fmt.Fprintln(out, "  No reachable Sonarr or Radarr instance; reactive library remains OFF.")
		return config.WriteTuning(path, values)
	}
	if db == nil {
		return fmt.Errorf("reactive destination storage is unavailable")
	}
	current.Topology.Active = topology.T1
	current.Reactive.CommitMode = "off"
	current.Reactive.Threshold = 0.5
	current.Reactive.MonitorEpisodes = true
	current.Reactive.MonitorMovies = true
	current.Reactive.PromotionEnabled = true
	current.Reactive.PromotionFreePercent = 10
	if value, ok := topology.Parse(values["HARRBOR_TOPOLOGY"]); ok {
		current.Topology.Active = value
	}
	if value := values["HARRBOR_REACTIVE_COMMIT_MODE"]; config.ValidReactiveCommitMode(value) {
		current.Reactive.CommitMode = value
	}
	if value, err := strconv.ParseFloat(values["HARRBOR_REACTIVE_THRESHOLD"], 64); err == nil && value > 0 && value <= 1 {
		current.Reactive.Threshold = value
	}
	if value, err := strconv.ParseBool(values["HARRBOR_REACTIVE_MONITOR_EPISODES"]); err == nil {
		current.Reactive.MonitorEpisodes = value
	}
	if value, err := strconv.ParseBool(values["HARRBOR_REACTIVE_MONITOR_MOVIES"]); err == nil {
		current.Reactive.MonitorMovies = value
	}
	if value, err := strconv.Atoi(values["HARRBOR_REACTIVE_PROMOTION_FREE_PERCENT"]); err == nil && value >= 1 && value <= 99 {
		current.Reactive.PromotionFreePercent = value
	}
	if value, err := strconv.ParseBool(values["HARRBOR_REACTIVE_PROMOTION_ENABLED"]); err == nil {
		current.Reactive.PromotionEnabled = value
	}
	if selected, ok := topology.Parse(plan.Topology); ok {
		current.Topology.Active = selected
	}
	if plan.ReactiveCommit && current.Reactive.CommitMode == "off" {
		current.Reactive.CommitMode = "supervised"
	}
	fmt.Fprintln(out, "\n── S7d Reactive Library ───────────────────────────────")
	p := configurePrompter{reader: stdinReader, out: out}
	destinationStage := newStagedArrDestinationStore(store.New(db))
	if err := configureReactive(&p, &current, values, func(context.Context, config.ArrTarget) bool { return true }, destinationStage, plan.Promotion, true); err != nil {
		return err
	}
	if err := config.WriteTuning(path, values); err != nil {
		return err
	}
	if len(destinationStage.pending) > 0 {
		if err := destinationStage.Apply(); err != nil {
			return fmt.Errorf("save reactive Arr destinations: %w", err)
		}
	}
	return nil
}
