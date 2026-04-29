// Package container contains code related to dealing with docker containers
package container

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	dockercontainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/go-connections/nat"
	"github.com/sirupsen/logrus"

	"github.com/openserbia/watchtower/internal/util"
	wt "github.com/openserbia/watchtower/pkg/types"
)

// NewContainer returns a new Container instance instantiated with the
// specified ContainerInfo and ImageInfo structs.
func NewContainer(containerInfo *dockercontainer.InspectResponse, imageInfo *image.InspectResponse) *Container {
	return &Container{
		containerInfo: containerInfo,
		imageInfo:     imageInfo,
	}
}

// Container represents a running Docker container.
type Container struct {
	LinkedToRestarting bool
	Stale              bool

	containerInfo *dockercontainer.InspectResponse
	imageInfo     *image.InspectResponse
	// imageIdentity is the daemon's per-image provenance record, populated
	// from the extended fields of /images/{id}/json when the daemon runs the
	// containerd image store. Absent (nil) on the classic docker-image-store
	// path, where the len(RepoDigests)==0 heuristic in ImageIsLocal still
	// carries the signal.
	imageIdentity *ImageIdentity
	// targetImageID, when non-empty, overrides ImageName() in GetCreateConfig
	// so ContainerCreate references the image by digest instead of by tag.
	// Set by actions.Update after IsContainerStale resolves the new image,
	// closing the race where an external rebuild untags `name:latest`
	// between the scan and the recreate. The classic upstream flow used the
	// tag here and would surface "No such image" if the tag moved mid-scan,
	// leaving the old container already removed and the service down.
	targetImageID wt.ImageID
}

// ImageIdentity captures the Docker engine's per-image provenance record as
// returned in the raw inspect JSON under the top-level "Identity" key. Only
// the fields Watchtower acts on are decoded; everything else is dropped. The
// field is populated by the containerd image store (Docker 25+ with
// containerd-snapshotter) and absent on the classic docker-image-store path,
// so callers must treat a nil ImageIdentity as "signal unavailable", not
// "image has no provenance".
type ImageIdentity struct {
	// Build lists local build provenance entries. Non-empty means the image
	// was produced by `docker build` / `docker buildx build` / `docker load`
	// against this daemon.
	Build []ImageBuildIdentity `json:"Build,omitempty"`
	// Pull lists registry pull provenance entries. Non-empty means at least
	// one registry hands out the same manifest digest, so Watchtower can
	// meaningfully attempt a pull.
	Pull []ImagePullIdentity `json:"Pull,omitempty"`
}

// ImageBuildIdentity is one entry in ImageIdentity.Build. Only the fields
// Watchtower needs are decoded.
type ImageBuildIdentity struct {
	Ref       string `json:"Ref,omitempty"`
	CreatedAt string `json:"CreatedAt,omitempty"`
}

// ImagePullIdentity is one entry in ImageIdentity.Pull.
type ImagePullIdentity struct {
	Repository string `json:"Repository,omitempty"`
}

// SetImageIdentity attaches provenance data decoded from the extended image
// inspect JSON. Safe to pass nil — ImageIsLocal falls back to RepoDigests.
func (c *Container) SetImageIdentity(identity *ImageIdentity) {
	c.imageIdentity = identity
}

// SetTargetImageID records the digest the next recreate should reference.
// Non-empty values override the tag in GetCreateConfig; an empty value clears
// the override and falls back to ImageName(). Idempotent.
func (c *Container) SetTargetImageID(id wt.ImageID) {
	c.targetImageID = id
}

// TargetImageID returns the digest that will be used for the next recreate,
// or an empty string when no override has been set (in which case the
// container's tag is used).
func (c Container) TargetImageID() wt.ImageID {
	return c.targetImageID
}

// ImageIdentity returns the per-image provenance record, or nil if the daemon
// did not expose one (classic docker-image-store or an older engine).
func (c Container) ImageIdentity() *ImageIdentity {
	return c.imageIdentity
}

// IsLinkedToRestarting returns the current value of the LinkedToRestarting field for the container
func (c *Container) IsLinkedToRestarting() bool {
	return c.LinkedToRestarting
}

// IsStale returns the current value of the Stale field for the container
func (c *Container) IsStale() bool {
	return c.Stale
}

// SetLinkedToRestarting sets the LinkedToRestarting field for the container
func (c *Container) SetLinkedToRestarting(value bool) {
	c.LinkedToRestarting = value
}

// SetStale implements sets the Stale field for the container
func (c *Container) SetStale(value bool) {
	c.Stale = value
}

// ContainerInfo fetches JSON info for the container
func (c Container) ContainerInfo() *dockercontainer.InspectResponse {
	return c.containerInfo
}

// ID returns the Docker container ID.
func (c Container) ID() wt.ContainerID {
	return wt.ContainerID(c.containerInfo.ID)
}

// IsRunning returns a boolean flag indicating whether or not the current
// container is running. The status is determined by the value of the
// container's "State.Running" property.
func (c Container) IsRunning() bool {
	return c.containerInfo.State.Running
}

// IsRestarting returns a boolean flag indicating whether or not the current
// container is restarting. The status is determined by the value of the
// container's "State.Restarting" property.
func (c Container) IsRestarting() bool {
	return c.containerInfo.State.Restarting
}

// Name returns the Docker container name.
func (c Container) Name() string {
	return c.containerInfo.Name
}

// ImageID returns the ID of the Docker image that was used to start the
// container. May cause nil dereference if imageInfo is not set!
func (c Container) ImageID() wt.ImageID {
	return wt.ImageID(c.imageInfo.ID)
}

// SafeImageID returns the ID of the Docker image that was used to start the container if available,
// otherwise returns an empty string
func (c Container) SafeImageID() wt.ImageID {
	if c.imageInfo == nil {
		return ""
	}
	return wt.ImageID(c.imageInfo.ID)
}

// SourceImageID returns the ID Docker recorded against the container itself at
// creation time, not the ID of whichever image is currently tagged. This stays
// stable even when the image has been garbage-collected and GetContainer fell
// back to the name-resolved imageInfo, so it's the correct ID for --cleanup
// (we want to remove the *old* image, not the freshly-pulled replacement).
func (c Container) SourceImageID() wt.ImageID {
	return wt.ImageID(c.containerInfo.Image)
}

// ImageName returns the name of the Docker image that was used to start the
// container. If the original image was specified without a particular tag, the
// "latest" tag is assumed.
//
// When Config.Image holds a bare digest reference (e.g. "sha256:abc..."), we
// fall back to the first entry in imageInfo.RepoTags. That state is mostly a
// recovery path for containers that were recreated by an early version of the
// digest-pinning fix (commit 178bd7a) which incorrectly wrote the digest into
// Config.Image — those containers would otherwise be stuck forever, since
// inspecting a digest against itself never reports a new image and the
// targeted-scan filter would never match them. The fallback returns the live
// canonical tag, which is what callers (registry auth, HasNewImage,
// FilterByImage) actually want.
func (c Container) ImageName() string {
	// Compatibility w/ Zodiac deployments
	imageName, ok := c.getLabelValue(zodiacLabel)
	if !ok {
		imageName = c.containerInfo.Config.Image
	}

	if strings.HasPrefix(imageName, "sha256:") && c.imageInfo != nil {
		for _, tag := range c.imageInfo.RepoTags {
			if tag != "" && !strings.HasPrefix(tag, "sha256:") {
				return tag
			}
		}
	}

	if !strings.Contains(imageName, ":") {
		imageName = fmt.Sprintf("%s:latest", imageName)
	}

	return imageName
}

// Enabled returns the value of the container enabled label and if the label
// was set.
func (c Container) Enabled() (bool, bool) {
	rawBool, ok := c.getLabelValue(enableLabel)
	if !ok {
		return false, false
	}

	parsedBool, err := strconv.ParseBool(rawBool)
	if err != nil {
		return false, false
	}

	return parsedBool, true
}

// IsMonitorOnly returns whether the container should only be monitored based on values of
// the monitor-only label, the monitor-only argument and the label-take-precedence argument.
func (c Container) IsMonitorOnly(params wt.UpdateParams) bool {
	return c.getContainerOrGlobalBool(params.MonitorOnly, monitorOnlyLabel, params.LabelPrecedence)
}

// IsNoPull returns whether the image should be pulled based on values of
// the no-pull label, the no-pull argument and the label-take-precedence argument.
func (c Container) IsNoPull(params wt.UpdateParams) bool {
	return c.getContainerOrGlobalBool(params.NoPull, noPullLabel, params.LabelPrecedence)
}

// ImageIsLocal reports whether the container's image was produced by this
// daemon (`docker build`, `docker buildx build`, `docker load`) rather than
// pulled from a registry.
//
// Watchtower uses this as a signal to skip the registry roundtrip: there's
// nothing to pull for a locally-built image, and attempting one only produces
// a noisy "No such image" or "pull access denied" error from the daemon every
// poll. The locally-tagged image is still picked up by HasNewImage when a
// rebuild changes the tag's image ID, so `docker build -t app:latest .`
// followed by a poll triggers the expected recreate.
//
// Two signals, in order of precedence:
//
//  1. The containerd-snapshotter "Identity" record (Docker 25+). If the
//     engine recorded a Build entry and no Pull entry, the image is local.
//     This is the authoritative signal — on the containerd image store a
//     real registry pull and a local `docker build` both appear as
//     "name@sha256:..." in RepoDigests and are otherwise indistinguishable,
//     so the heuristic below would false-positive on every Docker Hub image.
//  2. Empty RepoDigests (the classic docker-image-store path). Fallback for
//     daemons that don't populate Identity — there, a local build genuinely
//     has no manifest digest because it never went through a registry push.
//
// Returns false when image info isn't available — be conservative, let the
// existing pull path surface the real error.
func (c Container) ImageIsLocal() bool {
	if c.imageInfo == nil {
		return false
	}
	if c.imageIdentity != nil {
		// A Pull entry means the image is in at least one registry — try
		// the pull even if a Build entry is also present (build-then-push).
		if len(c.imageIdentity.Pull) > 0 {
			return false
		}
		if len(c.imageIdentity.Build) > 0 {
			return true
		}
		// Identity present but both slices empty — unusual, fall through.
	}
	return len(c.imageInfo.RepoDigests) == 0
}

func (c Container) getContainerOrGlobalBool(globalVal bool, label string, contPrecedence bool) bool {
	contVal, err := c.getBoolLabelValue(label)
	if err != nil {
		if !errors.Is(err, errLabelNotFound) {
			logrus.WithField("error", err).WithField("label", label).Warn("Failed to parse label value")
		}
		return globalVal
	}
	if contPrecedence {
		return contVal
	}
	return contVal || globalVal
}

// Scope returns the value of the scope UID label and if the label
// was set.
func (c Container) Scope() (string, bool) {
	rawString, ok := c.getLabelValue(scope)
	if !ok {
		return "", false
	}

	return rawString, true
}

// Links returns a list containing the names of all the containers to which
// this container is linked.
func (c Container) Links() []string {
	var links []string

	dependsOnLabelValue := c.getLabelValueOrEmpty(dependsOnLabel)

	if dependsOnLabelValue != "" {
		for _, link := range strings.Split(dependsOnLabelValue, ",") {
			// Since the container names need to start with '/', let's prepend it if it's missing
			if !strings.HasPrefix(link, "/") {
				link = "/" + link
			}
			links = append(links, link)
		}

		return links
	}

	if (c.containerInfo != nil) && (c.containerInfo.HostConfig != nil) {
		for _, link := range c.containerInfo.HostConfig.Links {
			name := strings.Split(link, ":")[0]
			links = append(links, name)
		}

		// If the container uses another container for networking, it can be considered an implicit link
		// since the container would stop working if the network supplier were to be recreated
		networkMode := c.containerInfo.HostConfig.NetworkMode
		if networkMode.IsContainer() {
			links = append(links, networkMode.ConnectedContainer())
		}
	}

	return links
}

// ToRestart return whether the container should be restarted, either because
// is stale or linked to another stale container.
func (c Container) ToRestart() bool {
	return c.Stale || c.LinkedToRestarting
}

// IsWatchtower returns a boolean flag indicating whether or not the current
// container is the watchtower container itself. The watchtower container is
// identified by the presence of the "com.centurylinklabs.watchtower" label in
// the container metadata.
func (c Container) IsWatchtower() bool {
	return ContainsWatchtowerLabel(c.containerInfo.Config.Labels)
}

// HasPublishedPorts reports whether any of the container's ports are bound
// to a host port. Used to detect the "watchtower publishes its /v1/* API on
// the host and wants to self-update" case — Watchtower's rename-and-respawn
// self-update pattern can't work when the old and new containers would both
// try to bind the same host port during the brief overlap window.
//
// A binding "counts" as published when at least one entry in HostConfig
// specifies a non-empty HostPort. Ports that are only EXPOSE'd (no host
// binding) don't collide with anything, so they don't count.
func (c Container) HasPublishedPorts() bool {
	if c.containerInfo == nil || c.containerInfo.HostConfig == nil {
		return false
	}
	for _, bindings := range c.containerInfo.HostConfig.PortBindings {
		for _, b := range bindings {
			if b.HostPort != "" {
				return true
			}
		}
	}
	return false
}

// PreUpdateTimeout checks whether a container has a specific timeout set
// for how long the pre-update command is allowed to run. This value is expressed
// either as an integer, in minutes, or as 0 which will allow the command/script
// to run indefinitely. Users should be cautious with the 0 option, as that
// could result in watchtower waiting forever.
func (c Container) PreUpdateTimeout() int {
	var minutes int
	var err error

	val := c.getLabelValueOrEmpty(preUpdateTimeoutLabel)

	minutes, err = strconv.Atoi(val)
	if err != nil || val == "" {
		return 1
	}

	return minutes
}

// PostUpdateTimeout checks whether a container has a specific timeout set
// for how long the post-update command is allowed to run. This value is expressed
// either as an integer, in minutes, or as 0 which will allow the command/script
// to run indefinitely. Users should be cautious with the 0 option, as that
// could result in watchtower waiting forever.
func (c Container) PostUpdateTimeout() int {
	var minutes int
	var err error

	val := c.getLabelValueOrEmpty(postUpdateTimeoutLabel)

	minutes, err = strconv.Atoi(val)
	if err != nil || val == "" {
		return 1
	}

	return minutes
}

// StopSignal returns the custom stop signal (if any) that is encoded in the
// container's metadata. If the container has not specified a custom stop
// signal, the empty string "" is returned.
func (c Container) StopSignal() string {
	return c.getLabelValueOrEmpty(signalLabel)
}

// StopTimeout returns the container's configured SIGTERM-to-SIGKILL grace
// period if one was set on the container itself (via `docker run
// --stop-timeout` or Compose's `stop_grace_period`). Returns 0 when the
// container carries no explicit timeout, in which case the caller should
// fall back to the global --stop-timeout flag. Matches upstream Docker's
// precedence of per-container timeout over daemon default.
func (c Container) StopTimeout() time.Duration {
	if c.containerInfo == nil || c.containerInfo.Config == nil {
		return 0
	}
	secs := c.containerInfo.Config.StopTimeout
	if secs == nil || *secs <= 0 {
		return 0
	}
	return time.Duration(*secs) * time.Second
}

// GetCreateConfig returns the container's current Config converted into a format
// that can be re-submitted to the Docker create API.
//
// Ideally, we'd just be able to take the ContainerConfig from the old container
// and use it as the starting point for creating the new container; however,
// the ContainerConfig that comes back from the Inspect call merges the default
// configuration (the stuff specified in the metadata for the image itself)
// with the overridden configuration (the stuff that you might specify as part
// of the "docker run").
//
// In order to avoid unintentionally overriding the
// defaults in the new image we need to separate the override options from the
// default options. To do this we have to compare the ContainerConfig for the
// running container with the ContainerConfig from the image that container was
// started from. This function returns a ContainerConfig which contains just
// the options overridden at runtime.
func (c Container) GetCreateConfig() *dockercontainer.Config {
	config := c.containerInfo.Config
	hostConfig := c.containerInfo.HostConfig
	imageConfig := c.imageInfo.Config

	if config.WorkingDir == imageConfig.WorkingDir {
		config.WorkingDir = ""
	}

	if config.User == imageConfig.User {
		config.User = ""
	}

	if hostConfig.NetworkMode.IsContainer() {
		config.Hostname = ""
	}

	if util.SliceEqual(config.Entrypoint, imageConfig.Entrypoint) {
		config.Entrypoint = nil
		if util.SliceEqual(config.Cmd, imageConfig.Cmd) {
			config.Cmd = nil
		}
	}

	// Clear HEALTHCHECK configuration (if default)
	if config.Healthcheck != nil && imageConfig.Healthcheck != nil {
		if util.SliceEqual(config.Healthcheck.Test, imageConfig.Healthcheck.Test) {
			config.Healthcheck.Test = nil
		}

		if config.Healthcheck.Retries == imageConfig.Healthcheck.Retries {
			config.Healthcheck.Retries = 0
		}

		if config.Healthcheck.Interval == imageConfig.Healthcheck.Interval {
			config.Healthcheck.Interval = 0
		}

		if config.Healthcheck.Timeout == imageConfig.Healthcheck.Timeout {
			config.Healthcheck.Timeout = 0
		}

		if config.Healthcheck.StartPeriod == imageConfig.Healthcheck.StartPeriod {
			config.Healthcheck.StartPeriod = 0
		}
	}

	config.Env = util.SliceSubtract(config.Env, imageConfig.Env)

	config.Labels = util.StringMapSubtract(config.Labels, imageConfig.Labels)

	config.Volumes = util.StructMapSubtract(config.Volumes, imageConfig.Volumes)

	// subtract ports exposed in image from container. The image config's ExposedPorts
	// is keyed by string (OCI image-spec), while the container config uses nat.Port.
	for k := range config.ExposedPorts {
		if _, ok := imageConfig.ExposedPorts[string(k)]; ok {
			delete(config.ExposedPorts, k)
		}
	}
	for p := range c.containerInfo.HostConfig.PortBindings {
		config.ExposedPorts[p] = struct{}{}
	}

	// Always emit the human-readable tag here. The race against a CI rebuild
	// that may have untagged or moved name:latest between IsContainerStale
	// and ContainerCreate is closed inside dockerClient.StartContainer, where
	// targetImageID is re-bound to the original tag via ImageTag immediately
	// before create. Writing the digest into Config.Image instead is unsafe:
	// every downstream reader (HasNewImage, PullImage, FilterByImage,
	// registry auth) treats Config.Image as a tag and breaks on a digest.
	config.Image = c.ImageName()
	return config
}

// GetCreateHostConfig returns the container's current HostConfig with any links
// re-written so that they can be re-submitted to the Docker create API.
func (c Container) GetCreateHostConfig() *dockercontainer.HostConfig {
	hostConfig := c.containerInfo.HostConfig

	for i, link := range hostConfig.Links {
		name := link[0:strings.Index(link, ":")]
		alias := link[strings.LastIndex(link, "/"):]

		hostConfig.Links[i] = fmt.Sprintf("%s:%s", name, alias)
	}

	return hostConfig
}

// HasImageInfo returns whether image information could be retrieved for the container
func (c Container) HasImageInfo() bool {
	return c.imageInfo != nil
}

// ImageInfo fetches the ImageInspect data of the current container
func (c Container) ImageInfo() *image.InspectResponse {
	return c.imageInfo
}

// VerifyConfiguration checks the container and image configurations for nil references to make sure
// that the container can be recreated once deleted
func (c Container) VerifyConfiguration() error {
	if c.imageInfo == nil {
		return errNoImageInfo
	}

	containerInfo := c.ContainerInfo()
	if containerInfo == nil {
		return errNoContainerInfo
	}

	containerConfig := containerInfo.Config
	if containerConfig == nil {
		return errInvalidConfig
	}

	hostConfig := containerInfo.HostConfig
	if hostConfig == nil {
		return errInvalidConfig
	}

	// Instead of returning an error here, we just create an empty map
	// This should allow for updating containers where the exposed ports are missing
	if len(hostConfig.PortBindings) > 0 && containerConfig.ExposedPorts == nil {
		containerConfig.ExposedPorts = make(map[nat.Port]struct{})
	}

	return nil
}
