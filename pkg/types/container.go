// Package types defines the data-transfer and interface types shared across watchtower packages.
package types

import (
	"strings"
	"time"

	dc "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
)

// ImageID is a hash string representing a container image
type ImageID string

// ContainerID is a hash string representing a container instance
type ContainerID string

// ImageEvent is a minimal image-lifecycle event surfaced by the Docker engine.
// It is intentionally a watchtower-owned type (not a Docker SDK alias) so the
// Client interface stays decoupled from the SDK and tests can synthesize events
// without a running daemon.
type ImageEvent struct {
	// Action is the Docker event action (e.g. "tag", "load"). Callers decide
	// which actions are interesting; the stream pre-filters to image events
	// but doesn't assume a single consumer policy.
	Action string
	// ImageID is the actor ID from the event (a "sha256:..." digest string).
	ImageID ImageID
	// ImageName is the human-readable reference from the actor's attributes
	// (e.g. "foo:latest"), if the daemon attached one. Empty for events that
	// only carry the digest.
	ImageName string
}

// ShortID returns the 12-character (hex) short version of an image ID hash, removing any "sha256:" prefix if present
func (id ImageID) ShortID() (short string) {
	return shortID(string(id))
}

// ShortID returns the 12-character (hex) short version of a container ID hash, removing any "sha256:" prefix if present
func (id ContainerID) ShortID() (short string) {
	return shortID(string(id))
}

func shortID(longID string) string {
	prefixSep := strings.IndexRune(longID, ':')
	offset := 0
	length := 12
	if prefixSep >= 0 {
		if longID[0:prefixSep] == "sha256" {
			offset = prefixSep + 1
		} else {
			length += prefixSep + 1
		}
	}

	if len(longID) >= offset+length {
		return longID[offset : offset+length]
	}

	return longID
}

// Container is a docker container running an image
type Container interface {
	ContainerInfo() *dc.InspectResponse
	ID() ContainerID
	IsRunning() bool
	Name() string
	ImageID() ImageID
	SafeImageID() ImageID
	SourceImageID() ImageID
	ImageName() string
	Enabled() (bool, bool)
	IsMonitorOnly(UpdateParams) bool
	Scope() (string, bool)
	Links() []string
	ToRestart() bool
	IsWatchtower() bool
	HasPublishedPorts() bool
	StopSignal() string
	StopTimeout() time.Duration
	HasImageInfo() bool
	ImageIsLocal() bool
	ImageInfo() *image.InspectResponse
	HealthCheckTimeout() (time.Duration, bool)
	ImageCooldown() (time.Duration, bool)
	IsInfrastructure() bool
	ComposeProject() string
	ComposeService() string
	ComposeDependencies() []string
	GetLifecyclePreCheckCommand() string
	GetLifecyclePostCheckCommand() string
	GetLifecyclePreUpdateCommand() string
	GetLifecyclePostUpdateCommand() string
	VerifyConfiguration() error
	SetStale(bool)
	IsStale() bool
	IsNoPull(UpdateParams) bool
	SetLinkedToRestarting(bool)
	IsLinkedToRestarting() bool
	PreUpdateTimeout() int
	PostUpdateTimeout() int
	IsRestarting() bool
	GetCreateConfig() *dc.Config
	GetCreateHostConfig() *dc.HostConfig
	SetTargetImageID(ImageID)
	TargetImageID() ImageID
}
