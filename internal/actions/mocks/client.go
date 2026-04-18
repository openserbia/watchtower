package mocks

import (
	"errors"
	"fmt"
	"time"

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
	if len(client.TestData.NextStartContainerIDs) > 0 {
		id := client.TestData.NextStartContainerIDs[0]
		client.TestData.NextStartContainerIDs = client.TestData.NextStartContainerIDs[1:]
		return id, nil
	}
	return c.ID(), nil
}

// RenameContainer is a mock method
func (client MockClient) RenameContainer(_ t.Container, _ string) error {
	return nil
}

// RemoveImageByID records each image ID it was asked to remove so tests can
// assert that cleanup targets the old image rather than the newly-pulled one.
func (client MockClient) RemoveImageByID(id t.ImageID) error {
	client.TestData.TriedToRemoveImageCount++
	client.TestData.TriedToRemoveImageIDs = append(client.TestData.TriedToRemoveImageIDs, id)
	return nil
}

// GetContainer is a mock method. When HealthStatusByID is populated, returns a
// container whose State.Health.Status reflects the configured value, so tests
// can drive --health-check-gated rollback paths deterministically.
func (client MockClient) GetContainer(id t.ContainerID) (t.Container, error) {
	if status, ok := client.TestData.HealthStatusByID[id]; ok {
		return ContainerWithHealthStatus(id, status), nil
	}
	return client.TestData.Containers[0], nil
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
	stale, found := client.TestData.Staleness[cont.Name()]
	if !found {
		stale = true
	}
	return stale, "", nil
}

// WarnOnHeadPullFailed is always true for the mock client
func (client MockClient) WarnOnHeadPullFailed(_ t.Container) bool {
	return true
}
