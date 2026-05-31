// Package actions orchestrates the high-level watchtower update flow:
// listing containers, running sanity and duplicate-instance checks, and
// driving the stop/start sequence through pkg/container.
package actions

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/openserbia/watchtower/pkg/container"
	"github.com/openserbia/watchtower/pkg/filters"
	"github.com/openserbia/watchtower/pkg/metrics"
	"github.com/openserbia/watchtower/pkg/sorter"
	"github.com/openserbia/watchtower/pkg/types"
)

// blueGreenTempName matches the temporary name performBlueGreenUpdate gives a
// green container: "<canonical>-wt-bluegreen-XXXXXXXX", where the suffix is the
// first 8 characters of util.RandName() (the [A-Za-z] alphabet). The capture
// group is the canonical bare name the green was cloned from.
var blueGreenTempName = regexp.MustCompile(`^(.+)-wt-bluegreen-[A-Za-z]{8}$`)

// CheckForSanity makes sure everything is sane before starting. When
// requireNoLinks is set (the rolling-restart and blue-green strategies, which
// update containers one at a time and cannot honor inter-container links),
// it rejects any configuration that declares container links.
func CheckForSanity(client container.Client, filter types.Filter, requireNoLinks bool) error {
	log.Debug("Making sure everything is sane before starting")

	if requireNoLinks {
		containers, err := client.ListContainers(filter)
		if err != nil {
			return err
		}
		for _, c := range containers {
			if len(c.Links()) > 0 {
				return fmt.Errorf(
					"%q depends on at least one other container; this is not compatible with --update-strategy=rolling-restart or blue-green",
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

// CleanupOrphanBlueGreen reconciles "green" containers left behind by an
// interrupted blue-green cutover — either a blue stop that failed (green is
// live under its temporary name while the old container still runs) or a
// watchtower crash between stopping blue and renaming green. It runs once at
// startup, before any update is scheduled, so it never races a live cutover:
// the update lock serializes runs and none is in flight yet.
//
// For each "<name>-wt-bluegreen-XXXXXXXX" container:
//   - if a container with the canonical "<name>" still exists, the green is a
//     leftover from a cutover whose blue refused to stop — remove it. Blue keeps
//     serving and the next eligible poll retries the cutover cleanly.
//   - if no canonical sibling exists, the green IS the live service that never
//     got renamed — promote it by renaming to the canonical name.
//
// Failures are logged and skipped rather than returned, so one stuck container
// can't block startup; the function only returns an error if the initial list
// fails.
func CleanupOrphanBlueGreen(client container.Client, scope string) error {
	filter := filters.NoFilter
	if scope != "" {
		filter = filters.FilterByScope(scope, filter)
	}
	containers, err := client.ListContainers(filter)
	if err != nil {
		return err
	}

	present := make(map[string]struct{}, len(containers))
	for _, c := range containers {
		present[strings.TrimPrefix(c.Name(), "/")] = struct{}{}
	}

	for _, c := range containers {
		bare := strings.TrimPrefix(c.Name(), "/")
		match := blueGreenTempName.FindStringSubmatch(bare)
		if match == nil {
			continue
		}
		canonical := match[1]
		clog := log.WithFields(log.Fields{"green": bare, "canonical": canonical})

		if _, ok := present[canonical]; ok {
			clog.Info("blue-green: removing orphan green container left by an interrupted cutover; the canonical container is still present")
			if err := client.StopContainer(c, stopTimeout); err != nil && !errors.Is(err, container.ErrContainerNotFound) {
				clog.WithError(err).Warn("blue-green: failed to remove orphan green container; it will be retried on the next startup")
			}
			continue
		}

		clog.Info("blue-green: promoting orphan green to its canonical name; an earlier cutover stopped the old container but did not finish the rename")
		if err := client.RenameContainer(c, canonical); err != nil {
			clog.WithError(err).Warn("blue-green: failed to rename orphan green to its canonical name; it keeps the temporary name until the next update")
			continue
		}
		// Treat the promoted name as present so a second leftover green for the
		// same canonical name (rare: two interrupted cutovers) takes the remove
		// branch instead of colliding on another rename.
		present[canonical] = struct{}{}
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
