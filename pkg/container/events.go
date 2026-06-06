package container

import (
	"context"

	"github.com/moby/moby/api/types/events"
	sdkClient "github.com/moby/moby/client"

	"github.com/openserbia/watchtower/pkg/metrics"
	t "github.com/openserbia/watchtower/pkg/types"
)

// WatchImageEvents opens a Docker engine event stream filtered to image
// tag/load actions. It translates SDK messages into watchtower's ImageEvent
// type so the consumer doesn't need the Docker types on its signature.
//
// The stream terminates when ctx is cancelled or the daemon closes the
// connection. The caller is expected to reconnect on transient errors —
// see internal/events for the reconnect loop.
func (client dockerClient) WatchImageEvents(ctx context.Context) (<-chan t.ImageEvent, <-chan error) {
	out := make(chan t.ImageEvent)
	errs := make(chan error, 1)

	filterArgs := sdkClient.Filters{}.Add("type", string(events.ImageEventType))
	// Tag fires on `docker build -t`, `docker pull` (after write), and
	// `docker tag`; load fires on `docker load -i`. Those cover the local
	// rebuild use-case. We intentionally skip "pull" because the poll loop
	// already handles registry-driven updates and we don't want to re-scan
	// when watchtower itself triggered the pull.
	filterArgs.Add("event", string(events.ActionTag), string(events.ActionLoad))

	stream := client.api.Events(ctx, sdkClient.EventsListOptions{Filters: filterArgs})
	msgs, streamErrs := stream.Messages, stream.Err

	go func() {
		defer close(out)
		defer close(errs)
		for {
			select {
			case <-ctx.Done():
				errs <- ctx.Err()
				return
			case err := <-streamErrs:
				if err != nil {
					metrics.RegisterDockerAPIError("events")
					errs <- err
				}
				return
			case msg, ok := <-msgs:
				if !ok {
					return
				}
				event := t.ImageEvent{
					Action:  string(msg.Action),
					ImageID: t.ImageID(msg.Actor.ID),
				}
				if name, exists := msg.Actor.Attributes["name"]; exists {
					event.ImageName = name
				}
				select {
				case out <- event:
				case <-ctx.Done():
					errs <- ctx.Err()
					return
				}
			}
		}
	}()

	return out, errs
}
