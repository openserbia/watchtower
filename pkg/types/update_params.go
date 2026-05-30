package types

import (
	"time"
)

// UpdateStrategy selects how Watchtower replaces a stale container.
type UpdateStrategy string

const (
	// StrategyRecreate stops the old container, then creates and starts a replacement.
	StrategyRecreate UpdateStrategy = "recreate"
	// StrategyRollingRestart updates eligible containers one at a time.
	StrategyRollingRestart UpdateStrategy = "rolling-restart"
	// StrategyBlueGreen starts the new container alongside the old one, waits until
	// it is healthy, drains, then retires the old container for zero-downtime updates.
	StrategyBlueGreen UpdateStrategy = "blue-green"
)

// IsRollingOrBlueGreen reports whether the strategy replaces containers
// incrementally (rolling-restart) or with an overlap (blue-green) rather than
// the default stop-then-recreate. Both are incompatible with the global
// monitor-only flag and with legacy container links, so the two startup guards
// share this predicate instead of repeating the comparison.
func (s UpdateStrategy) IsRollingOrBlueGreen() bool {
	return s == StrategyRollingRestart || s == StrategyBlueGreen
}

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
	// Strategy is the global default update strategy applied when a container does
	// not declare its own via the update-strategy label.
	Strategy UpdateStrategy
	// BlueGreenDrain is the global default drain window kept between the new and old
	// container after the new one reports healthy, when using the blue-green strategy.
	BlueGreenDrain time.Duration
	// RerunInitDeps re-creates compose `service_completed_successfully`
	// init containers against the new image before recreating the target.
	// Restores the every-restart-runs-migrations contract that an entrypoint.sh
	// wrapper used to provide, for stacks that moved bootstrap (goose, schema
	// init) into a sibling Compose service. See internal/initrerun. Opt-in:
	// requires backwards-compatible migrations because the old target keeps
	// serving traffic while the new image's init container runs.
	RerunInitDeps bool
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
