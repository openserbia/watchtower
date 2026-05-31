package mocks

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/openserbia/watchtower/pkg/container"
	t "github.com/openserbia/watchtower/pkg/types"
)

// MockClient is a mock that passes as a watchtower Client
type MockClient struct {
	TestData      *TestData
	pullImages    bool
	removeVolumes bool
}

// TestData is the data used to perform the test
type TestData struct {
	TriedToRemoveImageCount int
	TriedToRemoveImageIDs   []t.ImageID
	NameOfContainerToKeep   string
	Containers              []t.Container
	Staleness               map[string]bool
	// HealthStatusByID lets tests return specific health states from
	// GetContainer — used by --health-check-gated rollback tests.
	HealthStatusByID  map[t.ContainerID]string
	StartedContainers []t.Container
	// NextStartContainerIDs, if non-empty, is consumed in order by
	// StartContainer and returned instead of the container's own ID. Lets
	// rollback tests distinguish the "new" container from the "rolled-back"
	// one even though both come from the same underlying Container struct.
	NextStartContainerIDs []t.ContainerID
	// VanishedContainers names containers whose StopContainer call should
	// return container.ErrContainerNotFound, simulating a Compose recreate
	// that beat watchtower to the punch between the scan list and stop.
	VanishedContainers map[string]bool
	// StalenessErrors lets tests force IsContainerStale to return a specific
	// error (e.g. container.ErrPinnedImage) for a named container, exercising
	// the typed-error skip paths in actions.Update.
	StalenessErrors map[string]error
	// NewestImageByName lets tests override the digest returned by
	// IsContainerStale's newestImage out-parameter, so they can verify
	// downstream pinning of ContainerCreate to the resolved image ID.
	NewestImageByName map[string]t.ImageID
	// RerunInitContainers records each container that RerunInitContainer
	// was called with, in order — used by --rerun-init-deps tests to
	// assert which deps ran and in what sequence.
	RerunInitContainers []t.Container
	// InitExitByName lets tests force RerunInitContainer to return a specific
	// exit code for a named init container, exercising the failure path that
	// caches the digest and skips the target update.
	InitExitByName map[string]int
	// RenameCalls records every RenameContainer invocation in order — used
	// by self-update tests to assert both the initial random rename and any
	// follow-up safety-net rename back to the canonical name.
	RenameCalls []RenameCall
	// ContainerByID, when populated, overrides GetContainer to return the
	// mapped container for matching IDs. Lets a test return a deliberately
	// wrong-named container from GetContainer(newContainerID) to drive the
	// post-recreate name-divergence safety net in restartStaleContainer.
	ContainerByID map[t.ContainerID]t.Container
	// StartContainerErrors lets tests force StartContainer to fail for a named
	// container, exercising the self-update start-failure dedup/backoff path in
	// restartStaleContainer.
	StartContainerErrors map[string]error
	// StopContainerErrors lets tests force StopContainer to fail for a named
	// container, exercising the blue-green stop-failure backoff and the
	// orphan-green cleanup sweep.
	StopContainerErrors map[string]error
	// StoppedContainers records every StopContainer invocation in order — used
	// to assert that the orphan-green cleanup sweep removed the leftover green.
	StoppedContainers []t.Container
	// ProbeStatuses lets preflight tests force a specific ProbeStatus per
	// capability. Capabilities absent from the map default to
	// container.StatusPresent, so a test only has to enumerate the ones it
	// wants Blocked or Unreachable.
	ProbeStatuses map[container.CapabilityID]container.ProbeStatus
}

// RenameCall captures one invocation of MockClient.RenameContainer so tests
// can assert both the order and arguments of rename activity (e.g. the
// initial random rename followed by a safety-net restore of the canonical
// name).
type RenameCall struct {
	ContainerID t.ContainerID
	OldName     string
	NewName     string
}

// TriedToRemoveImage is a test helper function to check whether RemoveImageByID has been called
func (testdata *TestData) TriedToRemoveImage() bool {
	return testdata.TriedToRemoveImageCount > 0
}

// CreateMockClient creates a mock watchtower Client for usage in tests
func CreateMockClient(data *TestData, pullImages, removeVolumes bool) MockClient {
	return MockClient{
		data,
		pullImages,
		removeVolumes,
	}
}

// ListContainers is a mock method returning the provided container testdata
func (client MockClient) ListContainers(_ t.Filter) ([]t.Container, error) {
	return client.TestData.Containers, nil
}

// StopContainer is a mock method
func (client MockClient) StopContainer(c t.Container, _ time.Duration) error {
	client.TestData.StoppedContainers = append(client.TestData.StoppedContainers, c)
	if client.TestData.VanishedContainers[c.Name()] {
		return container.ErrContainerNotFound
	}
	if err, ok := client.TestData.StopContainerErrors[c.Name()]; ok {
		return err
	}
	if c.Name() == client.TestData.NameOfContainerToKeep {
		return errors.New("tried to stop the instance we want to keep")
	}
	return nil
}

// StartContainer is a mock method. Records each started container so tests
// that exercise rollback can assert the old container came back, and returns
// pre-seeded IDs from NextStartContainerIDs when set so "new" and "rolled-back"
// containers can be addressed separately.
func (client MockClient) StartContainer(c t.Container) (t.ContainerID, error) {
	client.TestData.StartedContainers = append(client.TestData.StartedContainers, c)
	if err, ok := client.TestData.StartContainerErrors[c.Name()]; ok {
		return "", err
	}
	if len(client.TestData.NextStartContainerIDs) > 0 {
		id := client.TestData.NextStartContainerIDs[0]
		client.TestData.NextStartContainerIDs = client.TestData.NextStartContainerIDs[1:]
		return id, nil
	}
	return c.ID(), nil
}

// RenameContainer is a mock method. Records each invocation in
// TestData.RenameCalls so self-update tests can assert both the initial
// random rename and any follow-up safety-net restore.
func (client MockClient) RenameContainer(c t.Container, newName string) error {
	client.TestData.RenameCalls = append(client.TestData.RenameCalls, RenameCall{
		ContainerID: c.ID(),
		OldName:     c.Name(),
		NewName:     newName,
	})
	return nil
}

// RemoveImageByID records each image ID it was asked to remove so tests can
// assert that cleanup targets the old image rather than the newly-pulled one.
func (client MockClient) RemoveImageByID(id t.ImageID) error {
	client.TestData.TriedToRemoveImageCount++
	client.TestData.TriedToRemoveImageIDs = append(client.TestData.TriedToRemoveImageIDs, id)
	return nil
}

// GetContainer is a mock method. Lookup precedence (highest first):
//  1. ContainerByID — a per-ID override used by self-update tests to make
//     the safety net see a container whose Name() differs from the
//     pre-rename canonical (driving the rename-back code path).
//  2. HealthStatusByID — returns a container with the configured health
//     state, used by --health-check-gated rollback tests.
//  3. Containers[0] — default fallback for legacy tests that only need
//     "some container" back from GetContainer.
func (client MockClient) GetContainer(id t.ContainerID) (t.Container, error) {
	if cont, ok := client.TestData.ContainerByID[id]; ok {
		return cont, nil
	}
	if status, ok := client.TestData.HealthStatusByID[id]; ok {
		return ContainerWithHealthStatus(id, status), nil
	}
	return client.TestData.Containers[0], nil
}

// RerunInitContainer is a mock method. Records each init container it was
// asked to re-run so tests can assert orchestration order, and returns a
// per-container exit code from InitExitByName (defaults to 0 if unset).
func (client MockClient) RerunInitContainer(c t.Container, _ time.Duration) (int, error) {
	client.TestData.RerunInitContainers = append(client.TestData.RerunInitContainers, c)
	if client.TestData.InitExitByName != nil {
		if code, ok := client.TestData.InitExitByName[c.Name()]; ok {
			return code, nil
		}
	}
	return 0, nil
}

// ExecuteCommand is a mock method
func (client MockClient) ExecuteCommand(_ t.ContainerID, command string, _ int) (skipUpdate bool, err error) {
	switch command {
	case "/PreUpdateReturn0.sh":
		return false, nil
	case "/PreUpdateReturn1.sh":
		return false, fmt.Errorf("command exited with code 1")
	case "/PreUpdateReturn75.sh":
		return true, nil
	default:
		return false, nil
	}
}

// IsContainerStale is true if not explicitly stated in TestData for the mock client
func (client MockClient) IsContainerStale(cont t.Container, _ t.UpdateParams) (bool, t.ImageID, error) {
	if err, ok := client.TestData.StalenessErrors[cont.Name()]; ok {
		return false, cont.SafeImageID(), err
	}
	stale, found := client.TestData.Staleness[cont.Name()]
	if !found {
		stale = true
	}
	newest := t.ImageID("")
	if id, ok := client.TestData.NewestImageByName[cont.Name()]; ok {
		newest = id
	}
	return stale, newest, nil
}

// WarnOnHeadPullFailed is always true for the mock client
func (client MockClient) WarnOnHeadPullFailed(_ t.Container) bool {
	return true
}

// ProbeCapabilities is a mock method. It returns one ProbeResult per requested
// capability, defaulting to container.StatusPresent and overriding from
// TestData.ProbeStatuses so preflight tests can drive Blocked/Unreachable per
// capability. Results preserve the requested order.
func (client MockClient) ProbeCapabilities(_ context.Context, ids []container.CapabilityID) []container.ProbeResult {
	results := make([]container.ProbeResult, 0, len(ids))
	for _, id := range ids {
		status := container.StatusPresent
		if s, ok := client.TestData.ProbeStatuses[id]; ok {
			status = s
		}
		results = append(results, container.ProbeResult{ID: id, Status: status})
	}
	return results
}

// WatchImageEvents is a mock method. Returns a closed message channel and an
// error channel that emits ctx.Err() on cancel, satisfying the Client interface
// without doing real work — tests that actually exercise event handling build
// their own stream against internal/events directly.
func (client MockClient) WatchImageEvents(ctx context.Context) (<-chan t.ImageEvent, <-chan error) {
	msgs := make(chan t.ImageEvent)
	errs := make(chan error, 1)
	go func() {
		defer close(msgs)
		defer close(errs)
		<-ctx.Done()
		errs <- ctx.Err()
	}()
	return msgs, errs
}
