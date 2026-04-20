// Package actions orchestrates the high-level watchtower update flow:
// listing containers, running sanity and duplicate-instance checks, and
// driving the stop/start sequence through pkg/container.
package actions

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/openserbia/watchtower/pkg/container"
	"github.com/openserbia/watchtower/pkg/filters"
	"github.com/openserbia/watchtower/pkg/metrics"
	"github.com/openserbia/watchtower/pkg/sorter"
	"github.com/openserbia/watchtower/pkg/types"
)

// CheckForSanity makes sure everything is sane before starting
func CheckForSanity(client container.Client, filter types.Filter, rollingRestarts bool) error {
	log.Debug("Making sure everything is sane before starting")

	if rollingRestarts {
		containers, err := client.ListContainers(filter)
		if err != nil {
			return err
		}
		for _, c := range containers {
			if len(c.Links()) > 0 {
				return fmt.Errorf(
					"%q is depending on at least one other container. This is not compatible with rolling restarts",
					c.Name(),
				)
			}
		}
	}
	return nil
}

// unmanagedState tracks which containers we've already warned about across
// polls. The audit deliberately emits at startup (empty known set → everything
// new) and then stays quiet unless the set changes: a new unlabeled container
// shows up (warn) or a previously-unlabeled one gets labeled or removed
// (info). That turns a spammy per-poll warning into a steady-state signal
// operators can actually act on.
var (
	auditMu        sync.Mutex
	knownUnmanaged = make(map[string]struct{})
)

// ResetAuditStateForTest clears the in-memory audit cache. Intended for use
// between test specs — callers in production code have no reason to invoke it.
func ResetAuditStateForTest() {
	auditMu.Lock()
	defer auditMu.Unlock()
	knownUnmanaged = make(map[string]struct{})
}

// AuditUnmanaged classifies every container visible to the daemon into
// managed / excluded / unmanaged buckets, publishes those counts to
// Prometheus so the Grafana dashboard can show them, and — when logWarnings
// is true — logs change-detected warnings the first time each unmanaged
// container is seen. Steady state is silent: subsequent polls with the same
// set emit nothing unless the set changes (a new unlabeled container
// appears, or a previously-unlabeled one gets labeled or removed).
//
// Metrics publication is unconditional — Prometheus gauges are always-on
// observability. The logWarnings flag gates only the `docker logs` output,
// typically wired to --audit-unmanaged.
func AuditUnmanaged(client container.Client, scope string, logWarnings bool) error {
	filter := filters.NoFilter
	if scope != "" {
		filter = filters.FilterByScope(scope, filter)
	}
	containers, err := client.ListContainers(filter)
	if err != nil {
		return err
	}

	var managed, excluded, infrastructure int
	unmanagedNow := make(map[string]types.Container, len(containers))
	for _, c := range containers {
		if c.IsWatchtower() {
			continue
		}
		enabled, labeled := c.Enabled()
		switch {
		case c.IsInfrastructure():
			// Docker-managed scaffolding (buildx, Desktop). Labelling these
			// manually is pointless because they're recreated on every build
			// — bucket them separately so they stop inflating "unmanaged".
			infrastructure++
		case !labeled:
			unmanagedNow[c.Name()] = c
		case enabled:
			managed++
		default:
			excluded++
		}
	}
	metrics.SetAuditCounts(managed, excluded, len(unmanagedNow), infrastructure)

	if !logWarnings {
		return nil
	}

	auditMu.Lock()
	defer auditMu.Unlock()

	for name, c := range unmanagedNow {
		if _, seen := knownUnmanaged[name]; seen {
			continue
		}
		log.WithFields(log.Fields{
			"container": name,
			"image":     c.ImageName(),
		}).Warn("Container has no com.centurylinklabs.watchtower.enable label — silently skipped under --label-enable. Set the label to true or false to make the intent explicit.")
	}

	for name := range knownUnmanaged {
		if _, still := unmanagedNow[name]; still {
			continue
		}
		log.WithField("container", name).Info("Previously-unmanaged container is now labeled or removed — audit cleared")
	}

	knownUnmanaged = make(map[string]struct{}, len(unmanagedNow))
	for name := range unmanagedNow {
		knownUnmanaged[name] = struct{}{}
	}

	return nil
}

// CheckForMultipleWatchtowerInstances will ensure that there are not multiple instances of the
// watchtower running simultaneously. If multiple watchtower containers are detected, this function
// will stop and remove all but the most recently started container. This behaviour can be bypassed
// if a scope UID is defined.
func CheckForMultipleWatchtowerInstances(client container.Client, cleanup bool, scope string) error {
	filter := filters.WatchtowerContainersFilter
	if scope != "" {
		filter = filters.FilterByScope(scope, filter)
	}
	containers, err := client.ListContainers(filter)
	if err != nil {
		return err
	}

	if len(containers) <= 1 {
		log.Debug("There are no additional watchtower containers")
		return nil
	}

	log.Info("Found multiple running watchtower instances. Cleaning up.")
	return cleanupExcessWatchtowers(containers, client, cleanup)
}

const stopTimeout = 10 * time.Minute

func cleanupExcessWatchtowers(containers []types.Container, client container.Client, cleanup bool) error {
	var stopErrors int

	sort.Sort(sorter.ByCreated(containers))
	allContainersExceptLast := containers[0 : len(containers)-1]
	keep := containers[len(containers)-1]

	for _, c := range allContainersExceptLast {
		if err := client.StopContainer(c, stopTimeout); err != nil {
			if errors.Is(err, container.ErrContainerNotFound) {
				log.WithField("container", c.Name()).Debug("Excess watchtower vanished before stop — skipping")
				continue
			}
			// logging the original here as we're just returning a count
			log.WithError(err).Error("Could not stop a previous watchtower instance.")
			stopErrors++
			continue
		}

		if cleanup {
			// Skip if the kept watchtower runs the same image — force-removing
			// it now would yank the image out from under the surviving
			// instance and break its next restart.
			if c.SourceImageID() == keep.SourceImageID() || c.SourceImageID() == keep.SafeImageID() {
				log.WithField("image", c.SourceImageID().ShortID()).Debug("Skipping image cleanup: still in use by the kept watchtower instance")
				continue
			}
			if err := client.RemoveImageByID(c.SourceImageID()); err != nil {
				log.WithError(err).Warning("Could not cleanup watchtower images, possibly because of other watchtowers instances in other scopes.")
			}
		}
	}

	if stopErrors > 0 {
		return fmt.Errorf("%d errors while stopping watchtower containers", stopErrors)
	}

	return nil
}
