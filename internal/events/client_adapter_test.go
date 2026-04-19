package events_test

import (
	"context"
	"time"

	t "github.com/openserbia/watchtower/pkg/types"
)

// clientAdapter satisfies container.Client by forwarding WatchImageEvents to
// the fake and panicking on every other call. The watcher never invokes the
// other methods — a panic here is a fast-failing signal that a future refactor
// broke that assumption.
type clientAdapter struct{ fake *fakeClient }

func (c *clientAdapter) ListContainers(_ t.Filter) ([]t.Container, error) {
	panic("unexpected call: ListContainers")
}

func (c *clientAdapter) GetContainer(_ t.ContainerID) (t.Container, error) {
	panic("unexpected call: GetContainer")
}

func (c *clientAdapter) StopContainer(_ t.Container, _ time.Duration) error {
	panic("unexpected call: StopContainer")
}

func (c *clientAdapter) StartContainer(_ t.Container) (t.ContainerID, error) {
	panic("unexpected call: StartContainer")
}

func (c *clientAdapter) RenameContainer(_ t.Container, _ string) error {
	panic("unexpected call: RenameContainer")
}

func (c *clientAdapter) IsContainerStale(_ t.Container, _ t.UpdateParams) (bool, t.ImageID, error) {
	panic("unexpected call: IsContainerStale")
}

func (c *clientAdapter) ExecuteCommand(_ t.ContainerID, _ string, _ int) (bool, error) {
	panic("unexpected call: ExecuteCommand")
}

func (c *clientAdapter) RemoveImageByID(_ t.ImageID) error {
	panic("unexpected call: RemoveImageByID")
}

func (c *clientAdapter) WarnOnHeadPullFailed(_ t.Container) bool {
	panic("unexpected call: WarnOnHeadPullFailed")
}

func (c *clientAdapter) WatchImageEvents(ctx context.Context) (<-chan t.ImageEvent, <-chan error) {
	return c.fake.WatchImageEvents(ctx)
}
