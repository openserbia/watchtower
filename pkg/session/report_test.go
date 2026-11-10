package session_test

import (
	"testing"

	dockerContainer "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"

	"github.com/openserbia/watchtower/pkg/container"
	"github.com/openserbia/watchtower/pkg/session"
	"github.com/openserbia/watchtower/pkg/types"
)

const (
	oldImageID = "sha256:0000000000000000000000000000000000000000000000000000000000000001"
	newImageID = "sha256:0000000000000000000000000000000000000000000000000000000000000002"
)

// newContainer builds a container created from createdFrom whose image info
// resolved to resolved: equal in the normal case, the new image after
// GetContainer's missing-image fallback.
func newContainer(createdFrom, resolved string) types.Container {
	info := dockerContainer.InspectResponse{
		ID:     "cid",
		Name:   "/app",
		Image:  createdFrom,
		Config: &dockerContainer.Config{Image: "app:latest", Labels: map[string]string{}},
	}
	c := container.NewContainer(&info, &image.InspectResponse{ID: resolved})
	c.SetImageInfoFallback(createdFrom != resolved)
	return c
}

// A rebuild on the containerd image store removes the running image at once,
// so GetContainer resolves the container to the new image. The update it then
// runs must still be reported as one (2026-11-10: every media-puller roll on the
// Pi reported Updated=0).
func TestReportCountsUpdateAfterImageFallback(t *testing.T) {
	progress := session.Progress{}
	cont := newContainer(oldImageID, newImageID)
	progress.AddScanned(cont, newImageID)
	progress.MarkForUpdate(cont.ID())

	report := session.NewReport(progress)
	if got := len(report.Updated()); got != 1 {
		t.Fatalf("updated = %d, want 1 (fresh %d)", got, len(report.Fresh()))
	}
}

// A container already on the newest image stays fresh.
func TestReportKeepsUpToDateContainerFresh(t *testing.T) {
	progress := session.Progress{}
	cont := newContainer(newImageID, newImageID)
	progress.AddScanned(cont, newImageID)

	report := session.NewReport(progress)
	if len(report.Fresh()) != 1 || len(report.Updated()) != 0 {
		t.Fatalf("fresh = %d, updated = %d; want 1, 0", len(report.Fresh()), len(report.Updated()))
	}
}
