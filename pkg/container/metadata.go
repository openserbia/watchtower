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
	imageCooldownLabel      = "com.centurylinklabs.watchtower.image-cooldown"

	// composeProjectLabel / composeServiceLabel / composeDependsOnLabel are
	// written by `docker compose` (and Docker Desktop) on every container it
	// manages. Read-only from Watchtower's perspective — they let us reason
	// about Compose stacks without the operator having to duplicate the
	// graph in watchtower.depends-on labels.
	composeProjectLabel   = "com.docker.compose.project"
	composeServiceLabel   = "com.docker.compose.service"
	composeDependsOnLabel = "com.docker.compose.depends_on"
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

// ComposeProject returns the com.docker.compose.project label (or "" if the
// container wasn't created by Docker Compose).
func (c Container) ComposeProject() string {
	return c.getLabelValueOrEmpty(composeProjectLabel)
}

// ComposeService returns the com.docker.compose.service label (or "" if the
// container wasn't created by Docker Compose).
func (c Container) ComposeService() string {
	return c.getLabelValueOrEmpty(composeServiceLabel)
}

// ComposeDependencies returns the list of service names this container
// declared as `depends_on` in its Compose file. Empty when the container
// isn't Compose-managed or has no depends_on.
//
// Compose v2 serialises depends_on as a comma-separated list where each
// entry may carry optional modifiers: `service:service_started:required` or
// `service:service_healthy:false`. We drop the modifiers and return just the
// service names — Watchtower's sorter only cares about the graph edge; the
// condition/required bits govern Compose's own startup ordering which is
// orthogonal to updates.
func (c Container) ComposeDependencies() []string {
	raw := c.getLabelValueOrEmpty(composeDependsOnLabel)
	if raw == "" {
		return nil
	}
	deps := make([]string, 0)
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		// Strip modifiers after the first colon — the service name is
		// everything before.
		if i := strings.Index(entry, ":"); i >= 0 {
			entry = entry[:i]
		}
		if entry != "" {
			deps = append(deps, entry)
		}
	}
	return deps
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

// ImageCooldown returns the per-container override for --image-cooldown
// parsed from the com.centurylinklabs.watchtower.image-cooldown label. An
// operator sets this on individual services that warrant stricter (or looser)
// supply-chain gating than the global default — e.g. `24h` on a production
// database while dev containers inherit the fleet-wide policy.
//
// Second return is false when the label is absent or unparseable. `0` is
// treated as unset rather than "disable cooldown for this container" to avoid
// surprising the operator on typos; remove the label explicitly to opt out.
func (c Container) ImageCooldown() (time.Duration, bool) {
	raw, ok := c.getLabelValue(imageCooldownLabel)
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
