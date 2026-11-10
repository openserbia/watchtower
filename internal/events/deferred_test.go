package events

import (
	"context"
	"slices"
	"testing"
	"time"
)

func newLock() chan bool {
	lock := make(chan bool, 1)
	lock <- true
	return lock
}

func TestDeferredRunsAtOnceWhenLockFree(t *testing.T) {
	lock := newLock()
	var got []string
	(&Deferred{}).Run(t.Context(), lock, []string{"app"}, func(names []string) { got = names })
	if !slices.Equal(got, []string{"app"}) {
		t.Fatalf("run got %v, want [app]", got)
	}
	if len(lock) != 1 {
		t.Fatal("lock not given back")
	}
}

// Triggers that arrive while a poll holds the lock run once, merged, after it.
func TestDeferredRunsHeldTriggersAfterTheLockFrees(t *testing.T) {
	lock := newLock()
	held := <-lock
	runs := make(chan []string, 4)
	d := &Deferred{}
	run := func(names []string) { runs <- names }

	d.Run(t.Context(), lock, []string{"media-puller"}, run)
	d.Run(t.Context(), lock, []string{"camofox", "media-puller"}, run)
	select {
	case names := <-runs:
		t.Fatalf("ran %v while the lock was held", names)
	case <-time.After(20 * time.Millisecond):
	}

	lock <- held
	select {
	case names := <-runs:
		if !slices.Equal(names, []string{"camofox", "media-puller"}) {
			t.Fatalf("run got %v, want [camofox media-puller]", names)
		}
	case <-time.After(time.Second):
		t.Fatal("held trigger never ran")
	}
	select {
	case names := <-runs:
		t.Fatalf("ran twice, second time with %v", names)
	case <-time.After(20 * time.Millisecond):
	}
	// The lock comes back, and a later trigger runs at once again.
	var later []string
	d.Run(t.Context(), lock, []string{"app"}, func(names []string) { later = names })
	if !slices.Equal(later, []string{"app"}) {
		t.Fatalf("later run got %v, want [app]", later)
	}
}

// Shutdown while waiting drops the kept names without running them.
func TestDeferredDropsOnCancel(t *testing.T) {
	lock := newLock()
	held := <-lock
	ctx, cancel := context.WithCancel(t.Context())
	ran := make(chan struct{}, 1)
	d := &Deferred{}
	d.Run(ctx, lock, []string{"app"}, func([]string) { ran <- struct{}{} })
	cancel()
	time.Sleep(20 * time.Millisecond)
	lock <- held
	select {
	case <-ran:
		t.Fatal("ran after cancel")
	case <-time.After(50 * time.Millisecond):
	}
	if len(lock) != 1 {
		t.Fatal("lock taken after cancel")
	}
}
