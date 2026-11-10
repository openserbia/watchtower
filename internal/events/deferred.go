package events

import (
	"context"
	"slices"
	"sync"
)

// Deferred holds the image names of event-triggered scans that arrived while
// another update held the lock, and runs them once, together, when it frees.
//
// Dropping such a trigger left the poll as the only way the update happened.
// When a build finishes as a poll starts, that poll can check the container
// before the tag moves and see no new image, so the rebuild waited a full poll
// interval (2026-11-10 on the Pi: media-puller's build event at 12:04:24
// landed in the 12:04:24 poll and was dropped).
type Deferred struct {
	mu      sync.Mutex
	names   map[string]struct{}
	waiting bool
}

// Run takes lock and calls run with imageNames when the lock is free. When it
// is held, the names are kept and one goroutine waits for the lock, then calls
// run with every name kept meanwhile. ctx ending drops what is kept.
func (d *Deferred) Run(ctx context.Context, lock chan bool, imageNames []string, run func([]string)) {
	select {
	case v := <-lock:
		defer func() { lock <- v }()
		run(imageNames)
		return
	default:
	}

	d.mu.Lock()
	if d.names == nil {
		d.names = make(map[string]struct{})
	}
	for _, name := range imageNames {
		d.names[name] = struct{}{}
	}
	if d.waiting {
		d.mu.Unlock()
		return
	}
	d.waiting = true
	d.mu.Unlock()

	go func() {
		select {
		case <-ctx.Done():
			d.take()
		case v := <-lock:
			defer func() { lock <- v }()
			if names := d.take(); len(names) > 0 {
				run(names)
			}
		}
	}()
}

// take empties the kept names and returns them sorted.
func (d *Deferred) take() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	names := make([]string, 0, len(d.names))
	for name := range d.names {
		names = append(names, name)
	}
	clear(d.names)
	d.waiting = false
	slices.Sort(names)
	return names
}
