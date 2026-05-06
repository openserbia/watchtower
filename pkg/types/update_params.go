package types

import (
	"time"
)

// UpdateParams contains all different options available to alter the behavior of the Update func
type UpdateParams struct {
	Filter             Filter
	Cleanup            bool
	NoRestart          bool
	Timeout            time.Duration
	MonitorOnly        bool
	NoPull             bool
	LifecycleHooks     bool
	RollingRestart     bool
	LabelPrecedence    bool
	HealthCheckGated   bool
	HealthCheckTimeout time.Duration
	ImageCooldown      time.Duration
	ComposeDependsOn   bool
	// RunOnce is set when the caller is Watchtower's --run-once mode. Signals
	// to supply-chain gates like --image-cooldown that deferring an update
	// to "next poll" isn't meaningful — there is no next poll — so those
	// gates should fall through and apply immediately.
	RunOnce bool
	// SelfContainerID is the watchtower process's own container ID, detected
	// at startup by matching `os.Hostname()` against the running containers'
	// short IDs. When non-empty, restartStaleContainer uses ID equality (not
	// the broader IsWatchtower label match) to decide whether to do the
	// rename-and-respawn dance vs. stop+remove. Empty falls back to the
	// label-only check (legacy behavior; only happens if watchtower runs
	// outside a container or the operator overrode --hostname to a value
	// that no longer matches the container short ID).
	SelfContainerID ContainerID
}
