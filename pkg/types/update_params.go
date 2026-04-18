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
}
