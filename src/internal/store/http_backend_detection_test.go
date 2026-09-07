package store

import (
	"context"
	"testing"
	"time"
)

func TestHTTPBackendDetectionRoundTripReplaceDelete(t *testing.T) {
	s := newTestStoreCOR(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 13, 20, 30, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return now })

	if err := s.PutHTTPBackendDetection(ctx, HTTPBackendDetection{
		BackendID: "living-room", BaseURL: "http://backend:7000",
		DetectorVersion: 1, BackendType: "stremio",
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := s.GetHTTPBackendDetection(ctx, "living-room")
	if err != nil || got == nil {
		t.Fatalf("get: %v %+v", err, got)
	}
	if got.BackendType != "stremio" || got.BaseURL != "http://backend:7000" || !got.DetectedAt.Equal(now) {
		t.Fatalf("wrong row: %+v", got)
	}

	if err := s.PutHTTPBackendDetection(ctx, HTTPBackendDetection{
		BackendID: "living-room", BaseURL: "http://backend:8000",
		DetectorVersion: 2, BackendType: "omss",
	}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	got, _ = s.GetHTTPBackendDetection(ctx, "living-room")
	if got.BackendType != "omss" || got.DetectorVersion != 2 || got.BaseURL != "http://backend:8000" {
		t.Fatalf("replacement not visible: %+v", got)
	}

	if err := s.DeleteHTTPBackendDetection(ctx, "living-room"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got, err = s.GetHTTPBackendDetection(ctx, "living-room")
	if err != nil || got != nil {
		t.Fatalf("row remains after delete: %v %+v", err, got)
	}
}
