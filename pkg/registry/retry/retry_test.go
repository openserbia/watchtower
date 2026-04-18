package retry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testBackoffBase = 10 * time.Millisecond
	testBackoffMax  = 40 * time.Millisecond
)

func TestDoHTTP_SucceedsAfterTransientFailure(t *testing.T) {
	defer withBackoff(testBackoffBase, testBackoffMax)()

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < int32(maxAttempts) {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	res, err := DoHTTP(srv.Client(), req, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 after retry, got %d", res.StatusCode)
	}
	if got := calls.Load(); got != int32(maxAttempts) {
		t.Fatalf("expected %d upstream calls, got %d", maxAttempts, got)
	}
}

func TestDoHTTP_GivesUpAfterBudget(t *testing.T) {
	defer withBackoff(testBackoffBase, testBackoffMax)()

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	res, err := DoHTTP(srv.Client(), req, nil)
	if err == nil {
		_ = res.Body.Close()
		t.Fatal("expected error after exhausting retry budget")
	}
	if got := calls.Load(); got != int32(maxAttempts) {
		t.Fatalf("expected %d upstream calls, got %d", maxAttempts, got)
	}
}

func TestDoHTTP_DoesNotRetryNonTransientStatus(t *testing.T) {
	defer withBackoff(testBackoffBase, testBackoffMax)()

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	res, err := DoHTTP(srv.Client(), req, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 returned verbatim, got %d", res.StatusCode)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected 1 upstream call for non-transient status, got %d", got)
	}
}

func withBackoff(base, upper time.Duration) func() {
	prevBase, prevMax := backoffBase, backoffMax
	backoffBase, backoffMax = base, upper
	return func() {
		backoffBase, backoffMax = prevBase, prevMax
	}
}
