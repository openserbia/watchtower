package update

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestHandle_ReturnsJSONOnCompletion(t *testing.T) {
	var called atomic.Int32
	h := New(func(images []string) Response {
		called.Add(1)
		return Response{Status: "completed", Scanned: 3, Updated: 1, Failed: 0}
	}, nil)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, h.Path, nil)
	rec := httptest.NewRecorder()
	h.Handle(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatalf("expected JSON content type, got %q", ct)
	}
	var body Response
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "completed" || body.Scanned != 3 || body.Updated != 1 {
		t.Fatalf("unexpected body: %+v", body)
	}
	if called.Load() != 1 {
		t.Fatalf("expected updateFn called once, got %d", called.Load())
	}
}

func TestHandle_Returns429WhenAnotherUpdateRunning(t *testing.T) {
	// Pre-seed the lock as "held" — simulates an update in progress.
	busy := make(chan bool, 1)
	// Intentionally leave the channel empty (no value inside) so the
	// non-blocking receive in the handler falls through to the default
	// branch.

	h := New(func(images []string) Response {
		t.Fatal("updateFn must not be called when lock is held")
		return Response{}
	}, busy)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, h.Path, nil)
	rec := httptest.NewRecorder()
	h.Handle(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", rec.Code)
	}
	var body Response
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "skipped" {
		t.Fatalf("expected Status=skipped, got %q", body.Status)
	}
	if body.Reason == "" {
		t.Fatal("expected Reason to be populated on skip")
	}
}

func TestHandle_TargetedUpdateBlocksInsteadOf429(t *testing.T) {
	// For targeted updates (?image=foo) the handler should wait for the
	// lock instead of immediately 429-ing — a caller explicitly asking
	// for foo usually wants foo to be updated, even if they have to wait.
	ready := make(chan bool, 1)
	ready <- true // lock is available

	h := New(func(images []string) Response {
		if len(images) != 1 || images[0] != "foo/bar" {
			t.Fatalf("expected images=[foo/bar], got %v", images)
		}
		return Response{Status: "completed", Scanned: 1, Updated: 1}
	}, ready)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, h.Path+"?image=foo/bar", nil)
	rec := httptest.NewRecorder()
	h.Handle(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}
