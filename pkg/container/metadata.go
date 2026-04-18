package container

import (
	"strconv"
	"strings"
	"time"
)

const (
	watchtowerLabel         = "com.centurylinklabs.watchtower"
	signalLabel             = "com.centurylinklabs.watchtower.stop-signal"
	enableLabel             = "com.centurylinklabs.watchtower.enable"
	monitorOnlyLabel        = "com.centurylinklabs.watchtower.monitor-only"
	noPullLabel             = "com.centurylinklabs.watchtower.no-pull"
	dependsOnLabel          = "com.centurylinklabs.watchtower.depends-on"
	zodiacLabel             = "com.centurylinklabs.zodiac.original-image"
	scope                   = "com.centurylinklabs.watchtower.scope"
	preCheckLabel           = "com.centurylinklabs.watchtower.lifecycle.pre-check"
	postCheckLabel          = "com.centurylinklabs.watchtower.lifecycle.post-check"
	preUpdateLabel          = "com.centurylinklabs.watchtower.lifecycle.pre-update"
	postUpdateLabel         = "com.centurylinklabs.watchtower.lifecycle.post-update"
	preUpdateTimeoutLabel   = "com.centurylinklabs.watchtower.lifecycle.pre-update-timeout"
	postUpdateTimeoutLabel  = "com.centurylinklabs.watchtower.lifecycle.post-update-timeout"
	healthCheckTimeoutLabel = "com.centurylinklabs.watchtower.health-check-timeout"
)

// GetLifecyclePreCheckCommand returns the pre-check command set in the container metadata or an empty string
func (c Container) GetLifecyclePreCheckCommand() string {
	return c.getLabelValueOrEmpty(preCheckLabel)
}

// GetLifecyclePostCheckCommand returns the post-check command set in the container metadata or an empty string
func (c Container) GetLifecyclePostCheckCommand() string {
	return c.getLabelValueOrEmpty(postCheckLabel)
}

// GetLifecyclePreUpdateCommand returns the pre-update command set in the container metadata or an empty string
func (c Container) GetLifecyclePreUpdateCommand() string {
	return c.getLabelValueOrEmpty(preUpdateLabel)
}

// GetLifecyclePostUpdateCommand returns the post-update command set in the container metadata or an empty string
func (c Container) GetLifecyclePostUpdateCommand() string {
	return c.getLabelValueOrEmpty(postUpdateLabel)
}

// infrastructureImagePrefixes lists image-name prefixes that identify
// containers Docker's own tooling spawns (buildx builder, Docker Desktop
// internals). Matched case-insensitively against the container's image
// reference. Kept conservative — generic container managers (Portainer,
// Watchtower-adjacent) aren't in here because operators often *want* them
// watched.
var infrastructureImagePrefixes = []string{
	"moby/buildkit",
	"docker/desktop-",
}

// infrastructureLabelPrefixes lists label-key prefixes that mark a
// container as Docker-managed infrastructure. Buildkit containers carry
// com.docker.buildx.*; Docker Desktop internals carry com.docker.desktop.*.
var infrastructureLabelPrefixes = []string{
	"com.docker.buildx.",
	"com.docker.desktop.",
}

// IsInfrastructure reports whether this container is Docker-managed
// scaffolding (buildx builder, Desktop internals) rather than a user
// workload. Such containers come and go on their own cadence and shouldn't
// count toward the "unmanaged" audit bucket — they'd otherwise generate
// noise every `docker buildx build` invocation.
func (c Container) IsInfrastructure() bool {
	imageName := strings.ToLower(c.ImageName())
	for _, prefix := range infrastructureImagePrefixes {
		if strings.HasPrefix(imageName, prefix) {
			return true
		}
	}
	for key := range c.containerInfo.Config.Labels {
		lowered := strings.ToLower(key)
		for _, prefix := range infrastructureLabelPrefixes {
			if strings.HasPrefix(lowered, prefix) {
				return true
			}
		}
	}
	return false
}

// HealthCheckTimeout returns the per-container override for --health-check-gated
// parsed from the com.centurylinklabs.watchtower.health-check-timeout label.
// Second return is false when the label is absent or unparseable, so callers
// can fall back to a computed-from-HEALTHCHECK default or the global flag.
func (c Container) HealthCheckTimeout() (time.Duration, bool) {
	raw, ok := c.getLabelValue(healthCheckTimeoutLabel)
	if !ok || raw == "" {
		return 0, false
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}

// ContainsWatchtowerLabel takes a map of labels and values and tells
// the consumer whether it contains a valid watchtower instance label
func ContainsWatchtowerLabel(labels map[string]string) bool {
	val, ok := labels[watchtowerLabel]
	return ok && val == "true"
}

func (c Container) getLabelValueOrEmpty(label string) string {
	if val, ok := c.containerInfo.Config.Labels[label]; ok {
		return val
	}
	return ""
}

func (c Container) getLabelValue(label string) (string, bool) {
	val, ok := c.containerInfo.Config.Labels[label]
	return val, ok
}

func (c Container) getBoolLabelValue(label string) (bool, error) {
	if strVal, ok := c.containerInfo.Config.Labels[label]; ok {
		value, err := strconv.ParseBool(strVal)
		return value, err
	}
	return false, errLabelNotFound
}
