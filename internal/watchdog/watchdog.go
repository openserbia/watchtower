// Package watchdog fail-fasts a watchtower process whose scan loop has stopped
// making progress, so the container restart policy can revive it instead of the
// process lingering for days as a silently-wedged (but "healthy") daemon.
//
// It guards against two observed failure shapes:
//
//   - A wedged update run: one scheduled/event/API-triggered update blocks
//     forever (a stalled registry pull, a notification send without a timeout,
//     a dead Docker socket). The update lock is never returned, every later
//     tick skips at debug level, and scanning stops with no visible signal.
//   - A wedged process: the scan scheduler never fires at all — e.g. a boot
//     that blocks before the first schedule (stale socket mount after a
//     rootless daemon restart) or a died cron goroutine.
//
// The watchdog deliberately does NOT trust the rest of the process to be
// functional when it trips: the exit path never depends on logrus (whose
// notification hook can itself be the wedge) or on stderr being writable —
// the reason line is written best-effort from separate goroutines, and
// os.Exit(1) (which writes nothing) is the only guaranteed action.
package watchdog

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	// DefaultTickGrace bounds how long the scheduler may go without a tick
	// before the process is declared wedged. It also serves as the boot
	// deadline: the baseline is set when the watchdog is armed (process
	// start), so a boot that never reaches its first tick trips it. Widened
	// via SetTickGrace once the actual schedule cadence is known.
	DefaultTickGrace = 15 * time.Minute
	// DefaultStuckUpdateDeadline bounds a single update run. Generous on
	// purpose: a full cycle legitimately spans many image pulls on a slow
	// link, and a false trip aborts (and restarts) a working update.
	DefaultStuckUpdateDeadline = time.Hour
	// tickGraceFactor scales the schedule cadence into a tick grace: the
	// tick must arrive every interval, so missing several in a row means
	// the scheduler is gone, not slow.
	tickGraceFactor = 3
	// defaultProbeInterval is how often the watchdog re-evaluates.
	defaultProbeInterval = 30 * time.Second
	// exitReasonGrace is how long the best-effort reason writers get before
	// os.Exit. Short: the exit must not hang behind a blocked stderr.
	exitReasonGrace = 3 * time.Second
)

// Watchdog watches scan-loop liveness. The zero value is not usable; construct
// with New. All methods are safe on a nil receiver so call sites don't need to
// guard for the --no-watchdog case.
type Watchdog struct {
	// lastTick is the unix-nano time of the latest scheduler tick (or process
	// start before the first tick).
	lastTick atomic.Int64
	// tickGrace is the current allowed gap between ticks, in nanoseconds.
	tickGrace atomic.Int64
	// updateStart is the unix-nano time the in-flight update run began, or 0
	// when no update is running.
	updateStart atomic.Int64
	// stuckDeadline bounds a single update run, in nanoseconds.
	stuckDeadline time.Duration
	probeInterval time.Duration
	now           func() time.Time
	exit          func()
}

// New constructs a watchdog with the tick baseline set to now. Arm it with
// go Run(ctx).
func New() *Watchdog {
	w := &Watchdog{
		stuckDeadline: DefaultStuckUpdateDeadline,
		probeInterval: defaultProbeInterval,
		now:           time.Now,
		exit:          func() { os.Exit(1) },
	}
	w.tickGrace.Store(int64(DefaultTickGrace))
	w.lastTick.Store(w.now().UnixNano())
	return w
}

// Tick records a scheduler tick. Call it from the cron callback regardless of
// whether the tick runs or skips an update — a skipped tick still proves the
// scheduler is alive.
func (w *Watchdog) Tick() {
	if w == nil {
		return
	}
	w.lastTick.Store(w.now().UnixNano())
}

// SetTickGrace widens the tick grace to match the actual schedule cadence
// (several missed intervals, floored at DefaultTickGrace so sub-minute dev
// schedules don't turn scheduling jitter into an exit) and resets the tick
// baseline, marking the scheduler as armed from this instant.
func (w *Watchdog) SetTickGrace(interval time.Duration) {
	if w == nil {
		return
	}
	grace := interval * tickGraceFactor
	if grace < DefaultTickGrace {
		grace = DefaultTickGrace
	}
	w.tickGrace.Store(int64(grace))
	w.lastTick.Store(w.now().UnixNano())
}

// UpdateStarted records that an update run took the update lock. Paired with
// UpdateFinished; every trigger path funnels through
// runUpdatesWithNotifications, so instrumenting that chokepoint covers the
// scheduler, the Docker-event watcher, --update-on-start, and the HTTP API.
func (w *Watchdog) UpdateStarted() {
	if w == nil {
		return
	}
	w.updateStart.Store(w.now().UnixNano())
}

// UpdateFinished clears the in-flight update marker.
func (w *Watchdog) UpdateFinished() {
	if w == nil {
		return
	}
	w.updateStart.Store(0)
}

// UpdateRunningFor reports how long the in-flight update run has been going,
// or 0 when none is running. Used by the scheduler's skip branch to log a
// visible warning instead of the historical debug-level silence.
func (w *Watchdog) UpdateRunningFor() time.Duration {
	if w == nil {
		return 0
	}
	start := w.updateStart.Load()
	if start == 0 {
		return 0
	}
	return w.now().Sub(time.Unix(0, start))
}

// Run probes liveness until ctx is cancelled, and terminates the process when
// a probe trips. Start it in its own goroutine as early as possible — it must
// be running before anything that can block.
func (w *Watchdog) Run(ctx context.Context) {
	if w == nil {
		return
	}
	ticker := time.NewTicker(w.probeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if reason := w.probe(w.now()); reason != "" {
				w.terminate(reason)
				return
			}
		}
	}
}

// probe evaluates the liveness rules and returns a non-empty reason when the
// process should terminate. Pure with respect to the clock, for tests.
func (w *Watchdog) probe(now time.Time) string {
	if start := w.updateStart.Load(); start != 0 && w.stuckDeadline > 0 {
		if age := now.Sub(time.Unix(0, start)); age > w.stuckDeadline {
			return fmt.Sprintf("update run stuck for %s (deadline %s) — assuming a wedged pull/notification/docker call", age.Round(time.Second), w.stuckDeadline)
		}
	}
	// A running update legitimately delays ticks only in schedulers that run
	// jobs inline; robfig/cron ticks regardless, so no update exemption here.
	grace := time.Duration(w.tickGrace.Load())
	if age := now.Sub(time.Unix(0, w.lastTick.Load())); grace > 0 && age > grace {
		return fmt.Sprintf("no scheduler tick for %s (grace %s) — assuming a wedged boot or dead scheduler", age.Round(time.Second), grace)
	}
	return ""
}

// terminate reports the reason best-effort and exits. The stderr write and the
// logrus call (which feeds the notification hook, when it still works) each
// run in their own goroutine so a blocked pipe or a deadlocked log hook cannot
// stop the exit; os.Exit itself writes nothing and cannot block.
func (w *Watchdog) terminate(reason string) {
	go fmt.Fprintf(os.Stderr, "watchtower watchdog: %s; exiting so the restart policy can revive a working process\n", reason)
	go log.WithField("reason", reason).Error("watchtower watchdog tripped; exiting for restart")
	time.Sleep(exitReasonGrace)
	w.exit()
}
