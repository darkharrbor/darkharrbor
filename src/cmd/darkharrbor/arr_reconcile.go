package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/arrsetup"
	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

const maxArrReconcileTargets = 32

type configLookup func(string) (string, bool)

func resolveArrReconcileTargets(instances []store.ArrInstance, lookup configLookup) ([]arrsetup.ArrTarget, error) {
	targets := make([]arrsetup.ArrTarget, 0, len(instances))
	for _, inst := range instances {
		appType := strings.ToLower(strings.TrimSpace(inst.AppType))
		if appType != "sonarr" && appType != "radarr" {
			continue
		}
		if len(targets) == maxArrReconcileTargets {
			return nil, fmt.Errorf("more than %d supported Arr instances", maxArrReconcileTargets)
		}
		base, err := url.Parse(strings.TrimSpace(inst.URL))
		if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" || base.User != nil {
			return nil, fmt.Errorf("stored %s instance has an invalid URL", appType)
		}
		apiKey, ok := lookup(strings.TrimSpace(inst.APIKeyRef))
		if !ok || strings.TrimSpace(apiKey) == "" {
			return nil, fmt.Errorf("stored %s instance has no resolvable API key", appType)
		}
		targets = append(targets, arrsetup.ArrTarget{
			Name: inst.Name, URL: base.String(), AppType: appType, APIKey: apiKey,
		})
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("no stored Sonarr/Radarr instances; run setup first")
	}
	return targets, nil
}

func resolveEffectiveArrReconcileTargets(ctx context.Context, instances []store.ArrInstance, envTargets []config.ArrTarget, lookup configLookup) ([]arrsetup.ArrTarget, error) {
	if len(instances) > 0 {
		return resolveArrReconcileTargets(instances, lookup)
	}
	targets := make([]arrsetup.ArrTarget, 0, len(envTargets))
	for _, envTarget := range envTargets {
		if len(targets) == maxArrReconcileTargets {
			return nil, fmt.Errorf("more than %d supported Arr instances", maxArrReconcileTargets)
		}
		base, err := url.Parse(strings.TrimSpace(envTarget.BaseURL))
		if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" || base.User != nil {
			return nil, fmt.Errorf("environment-declared Arr instance has an invalid URL")
		}
		if strings.TrimSpace(envTarget.APIKey) == "" {
			return nil, fmt.Errorf("environment-declared Arr instance has no API key")
		}
		appType, err := arrsetup.DetectAppType(ctx, base.String(), envTarget.APIKey)
		if err != nil {
			return nil, fmt.Errorf("environment-declared Arr %q type detection failed", envTarget.Name)
		}
		targets = append(targets, arrsetup.ArrTarget{Name: envTarget.Name, URL: base.String(), AppType: appType, APIKey: envTarget.APIKey})
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("no Sonarr/Radarr instances; run setup or configure HARRBOR_ARR_NAMES")
	}
	return targets, nil
}

func safeArrReconcileError(err error) string {
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline exceeded"
	}
	stage, _, _ := strings.Cut(err.Error(), ":")
	switch stage {
	case "qbit", "sab", "torznab", "newznab", "http-stream", "nfo import", "uncached custom format":
		return stage + " failed"
	default:
		return "reconciliation failed"
	}
}

func reconcileStoredArrs(ctx context.Context, out io.Writer, instances []store.ArrInstance, cfg *config.Config, lanes arrsetup.Lanes) error {
	targets, err := resolveEffectiveArrReconcileTargets(ctx, instances, cfg.Arrs, cfg.Lookup)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	results := arrsetup.ReconcileSelected(ctx, targets, arrsetup.Creds{
		QBitPassword: cfg.Auth.QBitPassword,
		SABAPIKey:    cfg.Auth.SABAPIKey,
	}, lanes)
	failed := false
	for _, result := range results {
		if result.Err != nil {
			fmt.Fprintf(out, "FAIL %s (%s): %s\n", result.Name, result.AppType, safeArrReconcileError(result.Err))
			failed = true
			continue
		}
		fmt.Fprintf(out, "OK %s (%s): %s\n", result.Name, result.AppType, strings.Join(result.Actions, ", "))
	}
	if failed {
		return fmt.Errorf("one or more Arr instances failed reconciliation")
	}
	return nil
}

func reconcileLanes(db *sql.DB) arrsetup.Lanes {
	full := arrsetup.Lanes{Torrent: true, NNTP: true, HTTP: true}
	var raw string
	if err := db.QueryRow(`SELECT value FROM app_config WHERE key = 'onboarding_plan'`).Scan(&raw); err != nil {
		return full
	}
	var plan struct {
		BaseArrTopology  bool     `json:"base_arr_topology"`
		AcquisitionLanes []string `json:"acquisition_lanes"`
	}
	if json.Unmarshal([]byte(raw), &plan) != nil || !plan.BaseArrTopology {
		return arrsetup.Lanes{}
	}
	var lanes arrsetup.Lanes
	for _, lane := range plan.AcquisitionLanes {
		switch lane {
		case "torrent":
			lanes.Torrent = true
		case "nntp":
			lanes.NNTP = true
		case "http":
			lanes.HTTP = true
		}
	}
	return lanes
}

// runArrReconcileCommand repairs the supported three-lane topology on an
// existing install without rerunning setup or modifying unrelated Arr entries.
func runArrReconcileCommand(args []string) int {
	fs := flag.NewFlagSet("reconcile-arr", flag.ContinueOnError)
	cfgDir := fs.String("config", "/config", "path to the DarkHarrbor config directory")
	timeout := fs.Duration("timeout", 5*time.Minute, "whole-reconciliation timeout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *timeout <= 0 || *timeout > 10*time.Minute {
		fmt.Fprintln(os.Stderr, "reconcile-arr: timeout must be between 1ns and 10m")
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	db, err := store.Open(ctx, strings.TrimRight(*cfgDir, "/")+"/darkharrbor.db", 5*time.Second)
	if err != nil {
		fmt.Fprintln(os.Stderr, "reconcile-arr: database open failed")
		return 1
	}
	defer func() { _ = db.Close() }()

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "reconcile-arr: configuration load failed")
		return 1
	}
	instances, err := store.New(db).ListArrInstances(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "reconcile-arr: stored Arr discovery failed")
		return 1
	}
	lanes := reconcileLanes(db)
	if !lanes.Torrent && !lanes.NNTP && !lanes.HTTP {
		fmt.Fprintln(os.Stdout, "SKIP base Arr search/grab topology was intentionally declined")
		return 0
	}
	if err := reconcileStoredArrs(ctx, os.Stdout, instances, cfg, lanes); err != nil {
		fmt.Fprintf(os.Stderr, "reconcile-arr: %s\n", safeArrReconcileError(err))
		return 1
	}
	if _, err := db.Exec(
		`INSERT INTO app_config(key, value, updated_at)
		 VALUES('s9_complete', ?, strftime('%Y-%m-%dT%H:%M:%SZ','now'))
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		time.Now().UTC().Format(time.RFC3339),
	); err != nil {
		fmt.Fprintln(os.Stderr, "reconcile-arr: completion marker failed")
		return 1
	}
	return 0
}
