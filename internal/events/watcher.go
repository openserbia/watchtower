// Package events bridges Docker engine image events to targeted watchtower
// scans, so local `docker build` / `docker load` rebuilds trigger an update
// without waiting for the next scheduled poll. It is opt-in via
// --watch-docker-events and runs alongside (not in place of) the poll loop —
// the poll remains the safety net for missed events during reconnects and
// the authoritative mechanism for registry-backed images.
package events

import (
	"context"
	"math/rand"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/openserbia/watchtower/pkg/container"
	"github.com/openserbia/watchtower/pkg/metrics"
	t "github.com/openserbia/watchtower/pkg/types"
)

// Config tunes the watcher. Zero values fall back to defaults picked for a
// typical `docker build` burst (multiple tag events within a few hundred ms).
type Config struct {
	// Debounce collapses a burst of events into a single scan. Measured from
	// the first event in the burst; subsequent events reset the timer.
	Debounce time.Duration
	// ReconnectBase is the initial backoff after a stream error.
	ReconnectBase time.Duration
	// ReconnectMax caps the backoff.
	ReconnectMax time.Duration
	// Trigger is called once per debounced burst with the set of image names
	// seen during the window. The caller is responsible for acquiring the
	// shared updateLock — passing nil is a programming error.
	Trigger func(imageNames []string)
}

// Watcher consumes image events from a container.Client and fires debounced
// triggers. Constructed with NewWatcher, started with Run, stopped by
// cancelling the context passed to Run.
type Watcher struct {
	client container.Client
	cfg    Config
	// burst holds image names seen since the current debounce window opened.
	// Guarded by mu.
	mu    sync.Mutex
	burst map[string]struct{}
	// trigger is indirected through the struct to let tests observe it.
	timer *time.Timer
}

const (
	defaultDebounce      = 2 * time.Second
	defaultReconnectBase = 500 * time.Millisecond
	defaultReconnectMax  = 30 * time.Second
	backoffMultiplier    = 2
)

// NewWatcher constructs a Watcher. Call Run to start consuming.
func NewWatcher(client container.Client, cfg Config) *Watcher {
	if cfg.Debounce <= 0 {
		cfg.Debounce = defaultDebounce
	}
	if cfg.ReconnectBase <= 0 {
		cfg.ReconnectBase = defaultReconnectBase
	}
	if cfg.ReconnectMax <= 0 {
		cfg.ReconnectMax = defaultReconnectMax
	}
	return &Watcher{
		client: client,
		cfg:    cfg,
		burst:  make(map[string]struct{}),
	}
}

// Run blocks until ctx is cancelled. It reconnects to the Docker event stream
// with bounded exponential backoff after transient errors, so a daemon restart
// doesn't leave the watcher permanently detached.
func (w *Watcher) Run(ctx context.Context) {
	backoff := w.cfg.ReconnectBase
	for {
		if ctx.Err() != nil {
			return
		}
		err := w.consume(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.WithError(err).Warn("Docker event stream ended; reconnecting")
		} else {
			log.Debug("Docker event stream closed; reconnecting")
		}
		metrics.RegisterEventReconnect()
		// Full jitter: sleep between 0 and backoff. Prevents a fleet of
		// watchtower instances from stampeding a daemon coming back online.
		sleep := time.Duration(rand.Int63n(int64(backoff) + 1))
		select {
		case <-ctx.Done():
			return
		case <-time.After(sleep):
		}
		backoff = minDuration(backoff*backoffMultiplier, w.cfg.ReconnectMax)
	}
}

// consume subscribes to the event stream for one connection lifetime. It
// returns only when the stream ends — either via ctx cancellation or a
// transport error. The debounce timer is owned by this method so a reconnect
// never leaves a dangling pending trigger from the previous stream.
func (w *Watcher) consume(ctx context.Context) error {
	msgs, errs := w.client.WatchImageEvents(ctx)

	// Inner ctx lets us stop the debounce goroutine when this connection
	// ends, without cancelling the caller's ctx.
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err, ok := <-errs:
			if !ok {
				return nil
			}
			return err
		case msg, ok := <-msgs:
			if !ok {
				return nil
			}
			w.record(streamCtx, msg)
		}
	}
}

// record adds the event's image name to the pending burst and (re)arms the
// debounce timer. Image events without a name attribute are still counted for
// metrics but skipped from the burst set — a nameless event would degenerate
// to a full scan, which defeats the targeted-trigger purpose.
func (w *Watcher) record(ctx context.Context, msg t.ImageEvent) {
	metrics.RegisterEventReceived(msg.Action)

	if msg.ImageName == "" {
		log.WithField("action", msg.Action).
			WithField("image", msg.ImageID.ShortID()).
			Debug("Docker image event lacks name; ignoring for targeted scan")
		return
	}

	w.mu.Lock()
	w.burst[stripTag(msg.ImageName)] = struct{}{}
	if w.timer == nil {
		w.timer = time.AfterFunc(w.cfg.Debounce, func() { w.fire(ctx) })
	} else {
		// Reset returns whether the timer had been active; we don't care
		// either way because the callback drains the burst atomically.
		w.timer.Reset(w.cfg.Debounce)
	}
	w.mu.Unlock()
}

// fire drains the pending burst and hands off to the configured Trigger. The
// ctx lets the connection-scoped goroutine avoid calling Trigger after the
// stream has ended, which would race with the parent shutdown.
func (w *Watcher) fire(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	w.mu.Lock()
	if len(w.burst) == 0 {
		w.mu.Unlock()
		return
	}
	names := make([]string, 0, len(w.burst))
	for name := range w.burst {
		names = append(names, name)
	}
	w.burst = make(map[string]struct{})
	w.timer = nil
	w.mu.Unlock()

	metrics.RegisterEventTriggeredScan()
	log.WithField("images", names).Info("Docker image event burst debounced — triggering targeted scan")
	w.cfg.Trigger(names)
}

// stripTag normalizes a Docker image reference by trimming the trailing tag so
// FilterByImage can match on the repository segment. "foo:latest" -> "foo",
// "ghcr.io/org/bar:sha-1234" -> "ghcr.io/org/bar". Preserves digest refs as-is
// because @ can't be the first character and FilterByImage splits only on ":".
func stripTag(ref string) string {
	// Walk from the right: stop at the first ":" that isn't preceded by "/"
	// (port in the registry host). Matches the logic callers rely on when
	// comparing container.ImageName().
	for i := len(ref) - 1; i >= 0; i-- {
		switch ref[i] {
		case '/':
			return ref
		case ':':
			return ref[:i]
		}
	}
	return ref
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
