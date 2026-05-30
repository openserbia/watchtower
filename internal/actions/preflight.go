package actions

import (
	"context"
	"fmt"

	log "github.com/sirupsen/logrus"

	"github.com/openserbia/watchtower/pkg/container"
	"github.com/openserbia/watchtower/pkg/types"
)

// PreflightConfig captures the subset of the running configuration that decides
// which Docker API capabilities a given Watchtower invocation actually
// exercises. It mirrors the flags read in cmd.PreRun so RequiredCapabilities
// can compute a minimal probe set instead of demanding every capability
// regardless of how Watchtower was started.
type PreflightConfig struct {
	// MonitorOnly drops the entire recreate write set — Watchtower only
	// detects staleness and notifies, never stops or starts anything.
	MonitorOnly bool
	// NoPull skips image pulls during staleness detection.
	NoPull bool
	// Cleanup removes the superseded image after an update.
	Cleanup bool
	// RerunInitDeps re-runs Compose init containers and waits for them to exit.
	RerunInitDeps bool
	// LifecycleHooks enables the exec-based pre/post lifecycle commands. Exec
	// is still only required when a watched container actually declares a
	// lifecycle label (see RequiredCapabilities).
	LifecycleHooks bool
	// WatchDockerEvents subscribes to the engine event stream. The stream is
	// an optional accelerator, so it only ever produces a warning.
	WatchDockerEvents bool
}

// baseReadCapabilities are exercised on every poll regardless of configuration:
// reach the daemon, list containers, inspect each one, and inspect its image.
var baseReadCapabilities = []container.CapabilityID{
	container.CapPing,
	container.CapContainerList,
	container.CapContainerInspect,
	container.CapImageInspect,
}

// recreateWriteCapabilities are the mutating operations a recreate performs:
// stop (kill + remove), recreate (create + start), re-tag to the resolved
// digest, and re-attach networks. Skipped wholesale under --monitor-only.
var recreateWriteCapabilities = []container.CapabilityID{
	container.CapContainerKill,
	container.CapContainerRemove,
	container.CapContainerCreate,
	container.CapContainerStart,
	container.CapImageTag,
	container.CapNetworkConnect,
	container.CapNetworkDisconnect,
	container.CapContainerRename,
}

// RequiredCapabilities returns the minimal set of Docker capabilities the given
// configuration will exercise. containers is the already-listed watched set —
// it is consulted only to decide whether the exec capability is needed (some
// container declares a lifecycle label). The returned slice is deduplicated and
// kept in catalog order so preflight output reads predictably.
//
// The optional Events capability is always part of the probe set so operators
// see its status, but Preflight only warns when it is missing (see Preflight).
func RequiredCapabilities(cfg PreflightConfig, containers []types.Container) []container.CapabilityID {
	wanted := make(map[container.CapabilityID]struct{})
	for _, id := range baseReadCapabilities {
		wanted[id] = struct{}{}
	}

	if !cfg.NoPull {
		wanted[container.CapImagePull] = struct{}{}
	}

	if !cfg.MonitorOnly {
		for _, id := range recreateWriteCapabilities {
			wanted[id] = struct{}{}
		}
		// Image cleanup and init-container re-runs only happen as part of an
		// actual update, so they are meaningless under --monitor-only.
		if cfg.Cleanup {
			wanted[container.CapImageRemove] = struct{}{}
		}
		if cfg.RerunInitDeps {
			wanted[container.CapContainerWait] = struct{}{}
		}
		if cfg.LifecycleHooks && anyLifecycleLabel(containers) {
			wanted[container.CapContainerExecCreate] = struct{}{}
		}
	}

	// Events is optional but always probed so its status is reported.
	if cfg.WatchDockerEvents {
		wanted[container.CapEvents] = struct{}{}
	}

	// Emit in catalog order for stable, lifecycle-ordered output.
	required := make([]container.CapabilityID, 0, len(wanted))
	for _, descriptor := range container.AllCapabilities() {
		if _, ok := wanted[descriptor.ID]; ok {
			required = append(required, descriptor.ID)
		}
	}
	return required
}

// anyLifecycleLabel reports whether any watched container declares a
// pre/post check or update lifecycle command — the only situation in which
// Watchtower needs the container-exec capability.
func anyLifecycleLabel(containers []types.Container) bool {
	for _, c := range containers {
		if c.GetLifecyclePreCheckCommand() != "" ||
			c.GetLifecyclePostCheckCommand() != "" ||
			c.GetLifecyclePreUpdateCommand() != "" ||
			c.GetLifecyclePostUpdateCommand() != "" {
			return true
		}
	}
	return false
}

// Preflight probes the required Docker capabilities and reports, per capability,
// whether it is Present, Blocked, or Unreachable. It logs a concise line per
// capability (endpoint, socket-proxy variable, reason) and returns an error
// naming the endpoint AND proxy variable for the first required capability that
// is Blocked or Unreachable, so the operator can fix the socket-proxy
// allow-list or connectivity in one shot. The optional Events capability only
// warns when missing — Watchtower degrades to scheduled polling without it.
func Preflight(client container.Client, required []container.CapabilityID) error {
	log.Debug("Running Docker capability preflight")

	results := client.ProbeCapabilities(context.Background(), required)

	var firstFailure *container.Capability
	var firstFailureStatus container.ProbeStatus
	for _, res := range results {
		descriptor, known := container.LookupCapability(res.ID)
		if !known {
			// An unknown capability means the probe set drifted from the
			// catalog — treat defensively as a hard failure rather than skip.
			return fmt.Errorf("preflight requested unknown capability %q", res.ID)
		}

		fields := log.Fields{
			"capability": res.ID,
			"endpoint":   descriptor.Endpoint,
			"proxy_var":  descriptor.ProxyVar,
			"kind":       descriptor.Kind,
		}

		switch res.Status {
		case container.StatusPresent:
			log.WithFields(fields).Debugf("Capability available: %s", descriptor.Reason)
		case container.StatusBlocked, container.StatusUnreachable:
			if descriptor.ID == container.CapEvents {
				// Optional accelerator: warn and carry on.
				log.WithFields(fields).Warnf(
					"Optional capability %s (%s, socket-proxy %s) is unavailable; %s Falling back to scheduled polling.",
					res.ID, descriptor.Endpoint, descriptor.ProxyVar, descriptor.Reason,
				)
				continue
			}
			log.WithFields(fields).Errorf(
				"Required capability %s (%s, socket-proxy %s) is %s: %s",
				res.ID, descriptor.Endpoint, descriptor.ProxyVar, res.Status, descriptor.Reason,
			)
			if firstFailure == nil {
				failed := descriptor
				firstFailure = &failed
				firstFailureStatus = res.Status
			}
		}
	}

	if firstFailure != nil {
		return fmt.Errorf(
			"docker capability preflight failed: %s (%s) is %s — grant it on the socket proxy (variable %s) or fix daemon connectivity",
			firstFailure.ID, firstFailure.Endpoint, firstFailureStatus, firstFailure.ProxyVar,
		)
	}

	log.Info("Docker capability preflight passed")
	return nil
}
