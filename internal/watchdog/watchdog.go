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
// Three rules implement that, evaluated by probe:
//
//   - Stuck-update: a single run may not exceed the stuck deadline. While a
//     run is in flight this is the only active rule — an --update-on-start
//     scan runs before the scheduler has even started, so "no ticks" is not
//     evidence of anything while an update is legitimately progressing.
//   - Schedule-anchored tick: once SetSchedule has been called, the scheduler
//     is declared dead only when it verifiably missed a fire — the first fire
//     due after the last observed tick lies more than the tick grace in the
//     past. Irregular cron specs (nights, weekends) are handled exactly: during
//     a legitimate gap the next due fire is still in the future.
//   - Boot: before SetSchedule, the arming baseline acts as a deadline for
//     reaching the scheduler at all, using the tick grace as the allowance.
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
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	// DefaultTickGrace is how far past its due point a scheduler fire (or the
	// boot phase, before a schedule is known) may run before the process is
	// declared wedged. Overridable via --watchdog-tick-grace.
	DefaultTickGrace = 15 * time.Minute
	// DefaultStuckUpdateDeadline bounds a single update run. Generous on
	// purpose: a full cycle legitimately spans many image pulls on a slow
	// link, and a false trip aborts (and restarts) a working update.
	// Overridable via --watchdog-stuck-deadline.
	DefaultStuckUpdateDeadline = time.Hour
	// defaultProbeInterval is how often the watchdog re-evaluates.
	defaultProbeInterval = 30 * time.Second
	// exitReasonGrace is how long the best-effort reason writers get before
	// os.Exit. Short: the exit must not hang behind a blocked stderr.
	exitReasonGrace = 3 * time.Second
)

// Schedule is the subset of cron.Schedule the watchdog consumes: the first
// activation time strictly after the given instant, or the zero time when the
// spec never fires again.
type Schedule interface {
	Next(time.Time) time.Time
}

// Options tunes a watchdog at construction. Zero (or negative) values fall
// back to the package defaults.
type Options struct {
	// StuckDeadline bounds a single update run (DefaultStuckUpdateDeadline).
	StuckDeadline time.Duration
	// TickGrace is the allowance past a due scheduler fire, and the boot
	// deadline before a schedule is known (DefaultTickGrace).
	TickGrace time.Duration
}

// Watchdog watches scan-loop liveness. The zero value is not usable; construct
// with New. All methods are safe on a nil receiver so call sites don't need to
// guard for the --no-watchdog case.
type Watchdog struct {
	// lastTick is the unix-nano time of the latest scheduler tick (or the
	// arming/re-arming baseline before the first tick).
	lastTick atomic.Int64
	// tickGrace is the current allowance in nanoseconds; 0 disables the
	// tick/boot rules entirely (HTTP-API-only mode).
	tickGrace atomic.Int64
	// updateStart is the unix-nano time the in-flight update run began, or 0
	// when no update is running.
	updateStart atomic.Int64
	// stuckDeadline bounds a single update run. Set once at construction,
	// before the Run goroutine starts.
	stuckDeadline time.Duration
	probeInterval time.Duration

	// scheduleMu guards schedule, which is set from the main goroutine after
	// the Run goroutine is already probing.
	scheduleMu sync.Mutex
	schedule   Schedule

	now  func() time.Time
	exit func()
}

// New constructs a watchdog with the tick baseline set to now. Arm it with
// go Run(ctx).
func New(opts Options) *Watchdog {
	if opts.StuckDeadline <= 0 {
		opts.StuckDeadline = DefaultStuckUpdateDeadline
	}
	if opts.TickGrace <= 0 {
		opts.TickGrace = DefaultTickGrace
	}
	w := &Watchdog{
		stuckDeadline: opts.StuckDeadline,
		probeInterval: defaultProbeInterval,
		now:           time.Now,
		exit:          func() { os.Exit(1) },
	}
	w.tickGrace.Store(int64(opts.TickGrace))
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

// SetSchedule anchors the tick rule to the actual cron schedule and resets the
// tick baseline, marking the scheduler as armed from this instant. From here on
// the rule is "a due fire was missed by more than the grace", not a fixed gap —
// so a business-hours spec's overnight and weekend silences never trip it.
func (w *Watchdog) SetSchedule(s Schedule) {
	if w == nil {
		return
	}
	w.scheduleMu.Lock()
	w.schedule = s
	w.scheduleMu.Unlock()
	// Fires due before this instant belong to a scheduler that hadn't been
	// started yet; only misses from here on are evidence.
	w.lastTick.Store(w.now().UnixNano())
}

// DisableTickRule turns the tick and boot rules off for deployments where the
// scheduler never runs — HTTP-API-only mode (--http-api-update without
// --http-api-periodic-polls) blocks serving HTTP before a scheduler is ever
// constructed, so "no tick" is that mode's healthy steady state, not a wedge.
// The stuck-update rule stays active and still bounds API-triggered runs.
func (w *Watchdog) DisableTickRule() {
	if w == nil {
		return
	}
	w.tickGrace.Store(0)
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

// UpdateFinished clears the in-flight update marker and re-baselines the tick
// clock: a long run (--update-on-start in particular executes before
// scheduler.Start) delays the scheduler's first fire past its precomputed
// time, and "the run just ended" is exactly when cron starts counting again.
func (w *Watchdog) UpdateFinished() {
	if w == nil {
		return
	}
	w.updateStart.Store(0)
	w.lastTick.Store(w.now().UnixNano())
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
// be running before anything that can block. Cancel ctx on the graceful
// shutdown path: after SIGTERM the scheduler stops ticking on purpose while
// the in-flight update drains, and that drain must not be killed.
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
	if start := w.updateStart.Load(); start != 0 {
		if age := now.Sub(time.Unix(0, start)); w.stuckDeadline > 0 && age > w.stuckDeadline {
			return fmt.Sprintf("update run stuck for %s (deadline %s) — assuming a wedged pull/notification/docker call", age.Round(time.Second), w.stuckDeadline)
		}
		// An in-flight run is bounded by the stuck rule alone. The tick rules
		// are suspended: an --update-on-start scan runs before the scheduler
		// has started, so no tick can arrive however healthy the process is,
		// and a slow-but-working run must not be converted into an outage by
		// the (much tighter) tick deadline.
		return ""
	}
	grace := time.Duration(w.tickGrace.Load())
	if grace <= 0 {
		return ""
	}
	lastTick := time.Unix(0, w.lastTick.Load())
	if sched := w.currentSchedule(); sched != nil {
		due := sched.Next(lastTick)
		if due.IsZero() {
			// The spec never fires again; there is nothing to enforce.
			return ""
		}
		if overdue := now.Sub(due); overdue > grace {
			return fmt.Sprintf("scheduler missed its %s fire by %s (grace %s) — assuming a dead scheduler", due.Format(time.RFC3339), overdue.Round(time.Second), grace)
		}
		return ""
	}
	if age := now.Sub(lastTick); age > grace {
		return fmt.Sprintf("no scheduler tick for %s (grace %s) — assuming a wedged boot or dead scheduler", age.Round(time.Second), grace)
	}
	return ""
}

func (w *Watchdog) currentSchedule() Schedule {
	w.scheduleMu.Lock()
	defer w.scheduleMu.Unlock()
	return w.schedule
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
