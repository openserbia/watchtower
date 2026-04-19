package events_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/sirupsen/logrus"

	"github.com/openserbia/watchtower/internal/events"
	t "github.com/openserbia/watchtower/pkg/types"
)

func TestEvents(t *testing.T) {
	RegisterFailHandler(Fail)
	logrus.SetOutput(GinkgoWriter)
	RunSpecs(t, "Events Suite")
}

// fakeClient is a test double for container.Client that only satisfies the
// WatchImageEvents method — the watcher never calls anything else, so the
// other methods are intentionally omitted via method stubs in a helper type.
type fakeClient struct {
	// events is consumed by successive WatchImageEvents calls; each call takes
	// the head of the slice as its stream. Guarded by mu.
	mu       sync.Mutex
	streams  []fakeStream
	calls    int32
	consumed chan struct{}
}

type fakeStream struct {
	msgs []t.ImageEvent
	// err, if non-nil, is sent on the error channel after draining msgs.
	err error
	// hold keeps the stream open after msgs drain (no error). Used to test
	// that debounce fires within an active stream.
	hold bool
}

func (f *fakeClient) WatchImageEvents(ctx context.Context) (<-chan t.ImageEvent, <-chan error) {
	atomic.AddInt32(&f.calls, 1)

	f.mu.Lock()
	var stream fakeStream
	if len(f.streams) > 0 {
		stream = f.streams[0]
		f.streams = f.streams[1:]
	}
	f.mu.Unlock()

	out := make(chan t.ImageEvent)
	errs := make(chan error, 1)

	go func() {
		defer close(out)
		defer close(errs)
		for _, msg := range stream.msgs {
			select {
			case out <- msg:
			case <-ctx.Done():
				errs <- ctx.Err()
				return
			}
		}
		if f.consumed != nil {
			select {
			case f.consumed <- struct{}{}:
			default:
			}
		}
		if stream.err != nil {
			errs <- stream.err
			return
		}
		if stream.hold {
			<-ctx.Done()
			errs <- ctx.Err()
			return
		}
	}()

	return out, errs
}

// wrap builds an adapter satisfying container.Client via method stubs. The
// watcher only calls WatchImageEvents, so every other method panics if a
// future refactor tries to call it from the watcher path.
func wrap(f *fakeClient) *clientAdapter { return &clientAdapter{fake: f} }

var _ = Describe("events watcher", func() {
	var (
		ctx    context.Context
		cancel context.CancelFunc
		fake   *fakeClient
	)

	BeforeEach(func() {
		ctx, cancel = context.WithCancel(context.Background())
		fake = &fakeClient{consumed: make(chan struct{}, 4)}
	})

	AfterEach(func() {
		cancel()
	})

	It("debounces a burst of tag events into a single targeted trigger", func() {
		var triggerCalls int32
		triggered := make(chan []string, 1)

		fake.streams = []fakeStream{{
			msgs: []t.ImageEvent{
				{Action: "tag", ImageName: "foo:v1"},
				{Action: "tag", ImageName: "foo:v2"},
				{Action: "tag", ImageName: "foo:v3"},
			},
			hold: true,
		}}

		w := events.NewWatcher(wrap(fake), events.Config{
			Debounce: 50 * time.Millisecond,
			Trigger: func(names []string) {
				atomic.AddInt32(&triggerCalls, 1)
				triggered <- names
			},
		})
		go w.Run(ctx)

		Eventually(triggered, time.Second).Should(Receive(ConsistOf("foo")))
		Consistently(func() int32 { return atomic.LoadInt32(&triggerCalls) },
			150*time.Millisecond, 30*time.Millisecond).Should(Equal(int32(1)))
	})

	It("skips events with no image name", func() {
		triggered := make(chan []string, 1)

		fake.streams = []fakeStream{{
			msgs: []t.ImageEvent{
				{Action: "tag", ImageID: "sha256:abc"},
				{Action: "tag", ImageName: "bar:latest"},
			},
			hold: true,
		}}

		w := events.NewWatcher(wrap(fake), events.Config{
			Debounce: 50 * time.Millisecond,
			Trigger:  func(names []string) { triggered <- names },
		})
		go w.Run(ctx)

		Eventually(triggered, time.Second).Should(Receive(ConsistOf("bar")))
	})

	It("reconnects after a stream error", func() {
		triggered := make(chan []string, 2)

		fake.streams = []fakeStream{
			{err: errors.New("daemon blip")},
			{
				msgs: []t.ImageEvent{{Action: "tag", ImageName: "baz:latest"}},
				hold: true,
			},
		}

		w := events.NewWatcher(wrap(fake), events.Config{
			Debounce:      30 * time.Millisecond,
			ReconnectBase: 10 * time.Millisecond,
			ReconnectMax:  20 * time.Millisecond,
			Trigger:       func(names []string) { triggered <- names },
		})
		go w.Run(ctx)

		Eventually(triggered, 2*time.Second).Should(Receive(ConsistOf("baz")))
		Expect(atomic.LoadInt32(&fake.calls)).To(BeNumerically(">=", 2))
	})

	It("stops when the context is cancelled", func() {
		fake.streams = []fakeStream{{hold: true}}
		done := make(chan struct{})
		w := events.NewWatcher(wrap(fake), events.Config{
			Debounce: 50 * time.Millisecond,
			Trigger:  func(_ []string) {},
		})
		go func() {
			w.Run(ctx)
			close(done)
		}()

		// Let the watcher subscribe before cancelling so we exercise the
		// in-flight cancellation path, not the pre-loop ctx check.
		Eventually(func() int32 { return atomic.LoadInt32(&fake.calls) },
			time.Second).Should(Equal(int32(1)))

		cancel()
		Eventually(done, time.Second).Should(BeClosed())
	})
})
