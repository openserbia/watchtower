package session

import (
	"github.com/openserbia/watchtower/pkg/types"
)

// Progress contains the current session container status
type Progress map[types.ContainerID]*ContainerStatus

// UpdateFromContainer sets various status fields from their corresponding container equivalents
func UpdateFromContainer(cont types.Container, newImage types.ImageID, state State) *ContainerStatus {
	return &ContainerStatus{
		containerID:   cont.ID(),
		containerName: cont.Name(),
		imageName:     cont.ImageName(),
		oldImage:      runningImageID(cont),
		newImage:      newImage,
		state:         state,
	}
}

// runningImageID is the image the container runs. That is SafeImageID, except
// when GetContainer fell back to the image the tag names now because the one the
// container was created from is gone (on the containerd image store a rebuild
// of the tag removes it at once). Then SafeImageID is the update target, the
// report saw old == new, filed the update as fresh and counted Updated=0.
func runningImageID(cont types.Container) types.ImageID {
	if f, ok := cont.(interface{ ImageInfoIsFallback() bool }); ok && f.ImageInfoIsFallback() {
		if id := cont.SourceImageID(); id != "" {
			return id
		}
	}
	return cont.SafeImageID()
}

// AddSkipped adds a container to the Progress with the state set as skipped
func (m Progress) AddSkipped(cont types.Container, err error) {
	update := UpdateFromContainer(cont, cont.SafeImageID(), SkippedState)
	update.err = err
	m.Add(update)
}

// AddScanned adds a container to the Progress with the state set as scanned
func (m Progress) AddScanned(cont types.Container, newImage types.ImageID) {
	m.Add(UpdateFromContainer(cont, newImage, ScannedState))
}

// UpdateFailed updates the containers passed, setting their state as failed with the supplied error
func (m Progress) UpdateFailed(failures map[types.ContainerID]error) {
	for id, err := range failures {
		update := m[id]
		update.err = err
		update.state = FailedState
	}
}

// Add a container to the map using container ID as the key
func (m Progress) Add(update *ContainerStatus) {
	m[update.containerID] = update
}

// MarkForUpdate marks the container identified by containerID for update
func (m Progress) MarkForUpdate(containerID types.ContainerID) {
	m[containerID].state = UpdatedState
}

// MarkSkipped overrides a previously-recorded entry's state to skipped, with
// the supplied error as the explanatory note. Used when a container vanishes
// mid-scan (typically a Compose recreate) — the entry was already added as
// scanned in the first pass, so we reuse it rather than building a new one.
func (m Progress) MarkSkipped(containerID types.ContainerID, err error) {
	if entry, ok := m[containerID]; ok && entry != nil {
		entry.state = SkippedState
		entry.err = err
	}
}

// Report creates a new Report from a Progress instance
func (m Progress) Report() types.Report {
	return NewReport(m)
}
