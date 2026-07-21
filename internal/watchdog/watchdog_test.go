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
func newTestDog(start time.Time) (*Watchdog, *time.Time) {
	now := start
	w := New()
	w.now = func() time.Time { return now }
	// Re-baseline with the fake clock (New baselined with the real one).
	w.lastTick.Store(now.UnixNano())
	return w, &now
}

var _ = Describe("the watchdog", func() {
	epoch := time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC)

	When("an update run exceeds the stuck deadline", func() {
		It("trips with a stuck-update reason", func() {
			w, now := newTestDog(epoch)
			w.UpdateStarted()
			*now = epoch.Add(DefaultStuckUpdateDeadline + time.Minute)
			w.Tick() // scheduler alive: only the stuck rule should trip
			Expect(w.probe(*now)).To(ContainSubstring("update run stuck"))
		})

		It("stays quiet while the run is within the deadline", func() {
			w, now := newTestDog(epoch)
			w.UpdateStarted()
			*now = epoch.Add(DefaultStuckUpdateDeadline - time.Minute)
			w.Tick()
			Expect(w.probe(*now)).To(BeEmpty())
		})

		It("stays quiet after the run finishes, however late", func() {
			w, now := newTestDog(epoch)
			w.UpdateStarted()
			*now = epoch.Add(2 * DefaultStuckUpdateDeadline)
			w.UpdateFinished()
			w.Tick()
			Expect(w.probe(*now)).To(BeEmpty())
		})
	})

	When("the scheduler stops ticking", func() {
		It("trips after the tick grace, covering a wedged boot", func() {
			w, now := newTestDog(epoch)
			// No Tick ever recorded beyond the arming baseline.
			*now = epoch.Add(DefaultTickGrace + time.Minute)
			Expect(w.probe(*now)).To(ContainSubstring("no scheduler tick"))
		})

		It("stays quiet while ticks keep arriving", func() {
			w, now := newTestDog(epoch)
			for i := range 10 {
				*now = epoch.Add(time.Duration(i+1) * 5 * time.Minute)
				w.Tick()
			}
			Expect(w.probe(*now)).To(BeEmpty())
		})

		It("scales the grace to the schedule cadence", func() {
			w, now := newTestDog(epoch)
			w.SetTickGrace(12 * time.Hour)
			// Well past the default grace but within 3x the cadence.
			*now = epoch.Add(20 * time.Hour)
			Expect(w.probe(*now)).To(BeEmpty())
			*now = epoch.Add(36*time.Hour + time.Minute)
			Expect(w.probe(*now)).To(ContainSubstring("no scheduler tick"))
		})

		It("floors the grace for sub-minute dev schedules", func() {
			w, now := newTestDog(epoch)
			w.SetTickGrace(10 * time.Second)
			*now = epoch.Add(DefaultTickGrace - time.Minute)
			Expect(w.probe(*now)).To(BeEmpty())
		})
	})

	Describe("UpdateRunningFor", func() {
		It("reports the in-flight age and 0 when idle", func() {
			w, now := newTestDog(epoch)
			Expect(w.UpdateRunningFor()).To(BeZero())
			w.UpdateStarted()
			*now = epoch.Add(90 * time.Second)
			Expect(w.UpdateRunningFor()).To(Equal(90 * time.Second))
			w.UpdateFinished()
			Expect(w.UpdateRunningFor()).To(BeZero())
		})
	})

	Describe("Run", func() {
		It("terminates through the injected exit when a probe trips", func() {
			w, _ := newTestDog(epoch)
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

		It("stops quietly when the context is cancelled", func() {
			w, _ := newTestDog(epoch)
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
			w.SetTickGrace(time.Hour)
			w.UpdateStarted()
			w.UpdateFinished()
			Expect(w.UpdateRunningFor()).To(BeZero())
			w.Run(context.Background())
		})
	})
})
