package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/reactivecommit"
	"github.com/darkharrbor/darkharrbor/internal/securefile"
)

const maxMonitorFlipIDText = 16 << 10

func runMonitorFlipCommand(args []string) int {
	fs := flag.NewFlagSet("monitor-flip", flag.ContinueOnError)
	targetName := fs.String("arr", "", "configured Arr target name")
	kind := fs.String("kind", "episode", "item kind: episode or movie")
	rawIDs := fs.String("ids", "", "comma-separated positive Arr item IDs")
	rate := fs.Int("rate", 10, "maximum items started per interval (1-100)")
	interval := fs.Duration("interval", time.Minute, "pacing interval (1s-1h)")
	timeout := fs.Duration("timeout", time.Hour, "whole command deadline (1s-24h)")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return 2
	}
	ids, err := parseMonitorFlipIDs(*rawIDs)
	if err != nil || *timeout < time.Second || *timeout > 24*time.Hour {
		fmt.Fprintln(os.Stderr, "monitor-flip: invalid request")
		return 2
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "monitor-flip: configuration unavailable")
		return 1
	}
	lock, err := acquireMonitorFlipLock(filepath.Dir(cfg.Database.Path))
	if err != nil {
		fmt.Fprintln(os.Stderr, "monitor-flip: another run is active")
		return 1
	}
	defer releaseMonitorFlipLock(lock)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	result, err := reactivecommit.NewArrClient(cfg.Arrs).PacedMonitorFlip(ctx, reactivecommit.MonitorFlipRequest{
		TargetName: strings.TrimSpace(*targetName), Kind: strings.ToLower(strings.TrimSpace(*kind)),
		IDs: ids, Rate: *rate, Interval: *interval,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "monitor-flip: stopped flipped=%d searched=%d\n", result.Flipped, result.Searched)
		return 1
	}
	fmt.Fprintf(os.Stdout, "flipped=%d searched=%d\n", result.Flipped, result.Searched)
	return 0
}

func parseMonitorFlipIDs(raw string) ([]int, error) {
	if raw == "" || len(raw) > maxMonitorFlipIDText {
		return nil, errors.New("invalid monitor flip IDs")
	}
	parts := strings.Split(raw, ",")
	if len(parts) > 1000 {
		return nil, errors.New("too many monitor flip IDs")
	}
	ids := make([]int, 0, len(parts))
	seen := make(map[int]struct{}, len(parts))
	for _, part := range parts {
		if part == "" || strings.TrimSpace(part) != part {
			return nil, errors.New("invalid monitor flip ID")
		}
		id, err := strconv.Atoi(part)
		if err != nil || id < 1 {
			return nil, errors.New("invalid monitor flip ID")
		}
		if _, exists := seen[id]; exists {
			return nil, errors.New("duplicate monitor flip ID")
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids, nil
}

func acquireMonitorFlipLock(configDir string) (*os.File, error) {
	lock, err := securefile.OpenLock(filepath.Join(configDir, ".monitor-flip.lock"))
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		return nil, err
	}
	return lock, nil
}

func releaseMonitorFlipLock(lock *os.File) {
	if lock == nil {
		return
	}
	_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	_ = lock.Close()
}
