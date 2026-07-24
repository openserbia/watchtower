package watchdog

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/sirupsen/logrus"
)

func TestWatchdog(t *testing.T) {
	RegisterFailHandler(Fail)
	logrus.SetOutput(GinkgoWriter)
	RunSpecs(t, "Watchdog Suite")
}

// newTestDog returns a watchdog on a controllable clock. Tests drive probe
// directly (it is the pure decision core); Run is covered separately.
func newTestDog(start time.Time, opts Options) (*Watchdog, *time.Time) {
	now := start
	w := New(opts)
	w.now = func() time.Time { return now }
	// Re-baseline with the fake clock (New baselined with the real one).
	w.lastTick.Store(now.UnixNano())
	return w, &now
}

// probeReason asserts the probe returns the given action and hands back the
// reason for content checks.
func probeReason(w *Watchdog, now time.Time, want probeAction) string {
	GinkgoHelper()
	action, reason := w.probe(now)
	Expect(action).To(Equal(want), "probe reason: %s", reason)
	return reason
}

// intervalSchedule mimics cron's fixed-cadence schedules: the next fire is
// always one interval after the given instant.
type intervalSchedule struct{ every time.Duration }

func (s intervalSchedule) Next(t time.Time) time.Time { return t.Add(s.every) }

// neverSchedule models a spec with no future activations (cron returns the
// zero time).
type neverSchedule struct{}

func (neverSchedule) Next(time.Time) time.Time { return time.Time{} }

var _ = Describe("the watchdog", func() {
	epoch := time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC)
	noopCancel := func() {}

	When("an update run stops making progress", func() {
		It("stage 1: cancels the run instead of exiting", func() {
			w, now := newTestDog(epoch, Options{})
			var cancelled atomic.Bool
			w.UpdateStarted(func() { cancelled.Store(true) })
			*now = epoch.Add(DefaultStuckUpdateDeadline + time.Minute)
			reason := probeReason(w, *now, probeCancelRun)
			Expect(reason).To(ContainSubstring("no progress"))

			w.cancelRun(reason)
			Expect(cancelled.Load()).To(BeTrue())
			// Within the escalation grace the (cancelled, draining) run is
			// tolerated.
			*now = now.Add(cancelEscalationGrace - time.Second)
			probeReason(w, *now, probeHealthy)
		})

		It("stage 2: exits only when cancellation does not drain the run", func() {
			w, now := newTestDog(epoch, Options{})
			w.UpdateStarted(noopCancel)
			*now = epoch.Add(DefaultStuckUpdateDeadline + time.Minute)
			w.cancelRun(probeReason(w, *now, probeCancelRun))
			*now = now.Add(cancelEscalationGrace + time.Second)
			reason := probeReason(w, *now, probeExit)
			Expect(reason).To(ContainSubstring("after its context was cancelled"))
		})

		It("stays quiet while progress keeps arriving, however old the run", func() {
			// A slow 200-container run legitimately outlives the deadline as
			// long as containers keep completing.
			w, now := newTestDog(epoch, Options{})
			w.UpdateStarted(noopCancel)
			for i := range 5 {
				*now = epoch.Add(time.Duration(i+1) * 50 * time.Minute)
				w.RunProgress()
				probeReason(w, *now, probeHealthy)
			}
			// Progress stops: caught one deadline after the LAST step.
			*now = now.Add(DefaultStuckUpdateDeadline + time.Minute)
			Expect(probeReason(w, *now, probeCancelRun)).To(ContainSubstring("no progress"))
		})

		It("stays quiet after the run finishes, however late", func() {
			w, now := newTestDog(epoch, Options{})
			w.UpdateStarted(noopCancel)
			*now = epoch.Add(2 * DefaultStuckUpdateDeadline)
			w.UpdateFinished()
			probeReason(w, *now, probeHealthy)
		})

		It("honors a custom stuck deadline", func() {
			w, now := newTestDog(epoch, Options{StuckDeadline: 3 * time.Hour})
			w.UpdateStarted(noopCancel)
			*now = epoch.Add(2 * time.Hour)
			probeReason(w, *now, probeHealthy)
			*now = epoch.Add(3*time.Hour + time.Minute)
			probeReason(w, *now, probeCancelRun)
		})

		It("resets the cancelled state for the next run", func() {
			w, now := newTestDog(epoch, Options{})
			w.UpdateStarted(noopCancel)
			*now = epoch.Add(DefaultStuckUpdateDeadline + time.Minute)
			w.cancelRun(probeReason(w, *now, probeCancelRun))
			w.UpdateFinished()
			// A fresh run must start uncancelled with a fresh progress clock.
			w.UpdateStarted(noopCancel)
			*now = now.Add(30 * time.Minute)
			probeReason(w, *now, probeHealthy)
		})
	})

	When("no schedule is known yet (boot phase)", func() {
		It("trips after the tick grace, covering a wedged boot", func() {
			w, now := newTestDog(epoch, Options{})
			// No Tick ever recorded beyond the arming baseline.
			*now = epoch.Add(DefaultTickGrace + time.Minute)
			Expect(probeReason(w, *now, probeExit)).To(ContainSubstring("no scheduler tick"))
		})

		It("honors a custom tick grace for the boot deadline", func() {
			w, now := newTestDog(epoch, Options{TickGrace: time.Hour})
			*now = epoch.Add(DefaultTickGrace + time.Minute)
			probeReason(w, *now, probeHealthy)
			*now = epoch.Add(time.Hour + time.Minute)
			probeReason(w, *now, probeExit)
		})
	})

	When("the tick rule is anchored to a schedule", func() {
		It("stays quiet while ticks keep arriving on cadence", func() {
			w, now := newTestDog(epoch, Options{})
			w.SetSchedule(intervalSchedule{5 * time.Minute})
			for i := range 10 {
				*now = epoch.Add(time.Duration(i+1) * 5 * time.Minute)
				w.Tick()
			}
			probeReason(w, *now, probeHealthy)
		})

		It("trips only once a due fire is missed by more than the grace", func() {
			w, now := newTestDog(epoch, Options{})
			w.SetSchedule(intervalSchedule{5 * time.Minute})
			// Next fire is due at +5m; quiet until +5m+grace.
			*now = epoch.Add(5*time.Minute + DefaultTickGrace - time.Second)
			probeReason(w, *now, probeHealthy)
			*now = epoch.Add(5*time.Minute + DefaultTickGrace + time.Second)
			Expect(probeReason(w, *now, probeExit)).To(ContainSubstring("missed its"))
		})

		It("sits out an irregular schedule's long legitimate gap", func() {
			// The weekend of a business-hours spec: the next fire after the
			// last Friday tick is ~62h away. The fixed-grace design killed the
			// process all weekend; the anchored rule must stay quiet until the
			// Monday fire is actually missed.
			w, now := newTestDog(epoch, Options{})
			w.SetSchedule(intervalSchedule{62 * time.Hour})
			*now = epoch.Add(30 * time.Hour)
			probeReason(w, *now, probeHealthy)
			*now = epoch.Add(62*time.Hour + DefaultTickGrace + time.Minute)
			Expect(probeReason(w, *now, probeExit)).To(ContainSubstring("missed its"))
		})

		It("suspends the tick rule while an update run is in flight", func() {
			// --update-on-start runs before scheduler.Start: no tick can arrive
			// during the run, however long it takes. Only the stuck rule may
			// bound it.
			w, now := newTestDog(epoch, Options{})
			w.SetSchedule(intervalSchedule{time.Minute})
			w.UpdateStarted(noopCancel)
			*now = epoch.Add(40 * time.Minute)
			probeReason(w, *now, probeHealthy)
			// Finishing re-baselines: the next fire counts from run end, so the
			// probe stays quiet immediately after and trips only once that new
			// fire is missed.
			w.UpdateFinished()
			probeReason(w, *now, probeHealthy)
			*now = epoch.Add(41*time.Minute + DefaultTickGrace + time.Second)
			probeReason(w, *now, probeExit)
		})

		It("never trips for a schedule with no future fires", func() {
			w, now := newTestDog(epoch, Options{})
			w.SetSchedule(neverSchedule{})
			*now = epoch.Add(240 * time.Hour)
			probeReason(w, *now, probeHealthy)
		})
	})

	When("the tick rule is disabled (HTTP-API-only mode)", func() {
		It("never trips on missing ticks but keeps the stuck rule", func() {
			w, now := newTestDog(epoch, Options{})
			w.DisableTickRule()
			*now = epoch.Add(24 * time.Hour)
			probeReason(w, *now, probeHealthy)
			// An API-triggered update that wedges must still be caught.
			w.UpdateStarted(noopCancel)
			*now = now.Add(DefaultStuckUpdateDeadline + time.Minute)
			probeReason(w, *now, probeCancelRun)
		})
	})

	Describe("UpdateRunningFor", func() {
		It("reports the in-flight age and 0 when idle", func() {
			w, now := newTestDog(epoch, Options{})
			Expect(w.UpdateRunningFor()).To(BeZero())
			w.UpdateStarted(func() {})
			*now = epoch.Add(90 * time.Second)
			Expect(w.UpdateRunningFor()).To(Equal(90 * time.Second))
			w.UpdateFinished()
			Expect(w.UpdateRunningFor()).To(BeZero())
		})
	})

	Describe("Run", func() {
		It("terminates through the injected exit when a probe trips", func() {
			w, _ := newTestDog(epoch, Options{})
			w.now = time.Now // real clock for the loop
			w.lastTick.Store(time.Now().Add(-DefaultTickGrace - time.Minute).UnixNano())
			w.probeInterval = 5 * time.Millisecond
			var exited atomic.Bool
			w.exit = func() { exited.Store(true) }

			done := make(chan struct{})
			go func() {
				defer GinkgoRecover()
				w.Run(context.Background())
				close(done)
			}()
			// terminate sleeps exitReasonGrace before exiting.
			Eventually(done, exitReasonGrace+2*time.Second).Should(BeClosed())
			Expect(exited.Load()).To(BeTrue())
		})

		It("cancels a wedged run without terminating", func() {
			w, _ := newTestDog(epoch, Options{StuckDeadline: 10 * time.Millisecond})
			w.now = time.Now
			w.DisableTickRule() // isolate the stuck rule
			w.probeInterval = 5 * time.Millisecond
			w.exit = func() { Fail("stage 1 must not exit the process") }

			cancelled := make(chan struct{})
			w.UpdateStarted(func() { close(cancelled) })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go w.Run(ctx)
			Eventually(cancelled, time.Second).Should(BeClosed())
			// The drained run clears the marker before the escalation grace.
			w.UpdateFinished()
		})

		It("stops quietly when the context is cancelled", func() {
			w, _ := newTestDog(epoch, Options{})
			w.now = time.Now
			w.lastTick.Store(time.Now().UnixNano())
			w.probeInterval = 5 * time.Millisecond
			w.exit = func() { Fail("exit must not be called for a live process") }

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() {
				defer GinkgoRecover()
				w.Run(ctx)
				close(done)
			}()
			cancel()
			Eventually(done, time.Second).Should(BeClosed())
		})
	})

	Describe("a nil watchdog", func() {
		It("accepts every call without panicking", func() {
			var w *Watchdog
			w.Tick()
			w.SetSchedule(intervalSchedule{time.Hour})
			w.DisableTickRule()
			w.UpdateStarted(func() {})
			w.RunProgress()
			w.UpdateFinished()
			Expect(w.UpdateRunningFor()).To(BeZero())
			w.Run(context.Background())
		})
	})
})
