package actions

import (
	"errors"
	"fmt"
	"sync"
	"time"

	dockercontainer "github.com/docker/docker/api/types/container"
	log "github.com/sirupsen/logrus"

	"github.com/openserbia/watchtower/internal/util"
	"github.com/openserbia/watchtower/pkg/container"
	"github.com/openserbia/watchtower/pkg/lifecycle"
	"github.com/openserbia/watchtower/pkg/metrics"
	"github.com/openserbia/watchtower/pkg/session"
	"github.com/openserbia/watchtower/pkg/sorter"
	"github.com/openserbia/watchtower/pkg/types"
)

const (
	// healthPollInterval is how often the gating loop re-checks a container's
	// health status. Short enough that a 30-second startup doesn't get flagged
	// late; long enough that we don't hammer the Docker socket.
	healthPollInterval = 2 * time.Second
	// defaultHealthCheckTimeout mirrors the default on --health-check-timeout
	// in internal/flags. Only used as a belt-and-braces fallback if the param
	// arrives zero (e.g. old callers constructing UpdateParams by hand).
	defaultHealthCheckTimeout = 60 * time.Second
	// rollbackCooldown is how long we skip a container after its last rollback.
	// Prevents the poll → stop → start → fail → rollback loop from thrashing
	// every poll when an image author has pushed a broken version.
	rollbackCooldown = 1 * time.Hour
)

var (
	failedRollbacksMu sync.Mutex
	// failedRollbacks tracks the last rollback time per container name. An
	// in-memory map is intentional: watchtower state resets on restart, and
	// operators who want to force a retry after a restart are exactly who
	// should benefit from that behavior.
	failedRollbacks = make(map[string]time.Time)

	pendingImagesMu sync.Mutex
	// pendingImages tracks digests first seen during an active --image-cooldown
	// window but not yet applied. Keyed by container name. When the digest
	// matches the map entry and the cooldown has elapsed, the update proceeds;
	// if the registry serves a different digest during the window (author
	// re-pushed), the entry resets so the clock restarts.
	pendingImages = make(map[string]pendingImage)

	previousImagesMu sync.Mutex
	// previousImages keeps the digest a container was running on *before* its
	// last successful update, keyed by container name. The next successful
	// update of that container hands the recorded digest to --cleanup and
	// rotates this slot to the digest just retired. Net effect: at any moment
	// each managed container has exactly one prior generation preserved on
	// disk, so a failed ContainerCreate (e.g. tag-race / image GC) leaves a
	// recoverable starting point. Survives only within a process — on
	// watchtower restart this resets, which under-cleans by one generation
	// for the next update of each container; that's a deliberate trade in
	// favor of zero persistent state.
	previousImages = make(map[string]types.ImageID)
)

type pendingImage struct {
	digest    types.ImageID
	firstSeen time.Time
}

// rollbackCooldownRemaining reports how long the container should continue to
// be skipped for being on post-rollback cooldown. Zero = not on cooldown.
func rollbackCooldownRemaining(containerName string) time.Duration {
	failedRollbacksMu.Lock()
	defer failedRollbacksMu.Unlock()
	last, ok := failedRollbacks[containerName]
	if !ok {
		return 0
	}
	remaining := rollbackCooldown - time.Since(last)
	if remaining <= 0 {
		delete(failedRollbacks, containerName)
		return 0
	}
	return remaining
}

func recordRollback(containerName string) {
	failedRollbacksMu.Lock()
	defer failedRollbacksMu.Unlock()
	failedRollbacks[containerName] = time.Now()
}

// resolveImageCooldown picks the effective cooldown for a container:
// per-container label > global flag > 0 (disabled). Zero means "apply
// immediately" — the pre-v1.12 behavior.
func resolveImageCooldown(c types.Container, params types.UpdateParams) time.Duration {
	if override, ok := c.ImageCooldown(); ok {
		return override
	}
	return params.ImageCooldown
}

// evaluateImageCooldown is the single decision point for "should this digest
// apply now?" under an active cooldown. Returns:
//   - proceed=true when the cooldown has elapsed (and clears the pending entry)
//   - proceed=false otherwise, with remaining giving the operator a useful
//     "retry in N" number for the log line
//
// Resets the pending entry when the registry serves a different digest —
// that's the signal the image author pushed a follow-up, so the clock should
// restart with the new digest.
func evaluateImageCooldown(containerName string, newest types.ImageID, cooldown time.Duration) (proceed bool, remaining time.Duration) {
	pendingImagesMu.Lock()
	defer pendingImagesMu.Unlock()

	entry, tracked := pendingImages[containerName]
	now := time.Now()

	switch {
	case !tracked, entry.digest != newest:
		// First sighting of this digest (either no prior record, or the
		// digest changed mid-cooldown). Record and defer.
		pendingImages[containerName] = pendingImage{digest: newest, firstSeen: now}
		return false, cooldown
	case now.Sub(entry.firstSeen) >= cooldown:
		// Stable long enough — apply and clear state.
		delete(pendingImages, containerName)
		return true, 0
	default:
		return false, cooldown - now.Sub(entry.firstSeen)
	}
}

// pendingImageCount reports the number of containers currently inside an
// active cooldown window. Used for watchtower_containers_in_cooldown.
func pendingImageCount() int {
	pendingImagesMu.Lock()
	defer pendingImagesMu.Unlock()
	return len(pendingImages)
}

// EvaluateImageCooldownForTest exposes the cooldown decision function for
// external tests. Not for production use.
func EvaluateImageCooldownForTest(containerName string, newest types.ImageID, cooldown time.Duration) (bool, time.Duration) {
	return evaluateImageCooldown(containerName, newest, cooldown)
}

// ResetCooldownStateForTest clears the in-memory cooldown map. External
// test-only helper.
func ResetCooldownStateForTest() {
	pendingImagesMu.Lock()
	defer pendingImagesMu.Unlock()
	pendingImages = make(map[string]pendingImage)
}

// rotatePreviousImage records the just-retired digest for a container and
// returns whatever digest was previously held in that slot. The returned ID
// is the one safe to hand to --cleanup; the freshly-stored ID stays on disk
// as the rollback target until the next update of this container rotates it
// out. An empty stored or rotated value is a normal first-pass condition,
// not an error.
func rotatePreviousImage(containerName string, justRetired types.ImageID) types.ImageID {
	previousImagesMu.Lock()
	defer previousImagesMu.Unlock()
	prior := previousImages[containerName]
	if justRetired == "" {
		// Don't blow away a known prior with a sentinel. Leaves the map slot
		// alone so the next real rotation still finds the recorded digest.
		return prior
	}
	previousImages[containerName] = justRetired
	return prior
}

// ResetPreviousImagesForTest clears the in-memory previous-image map.
// External test-only helper.
func ResetPreviousImagesForTest() {
	previousImagesMu.Lock()
	defer previousImagesMu.Unlock()
	previousImages = make(map[string]types.ImageID)
}

// SeedPreviousImageForTest pre-records a prior digest for a container so a
// test's first Update behaves as if it were the second (cleanup of the
// previous-previous fires immediately). External test-only helper.
func SeedPreviousImageForTest(containerName string, id types.ImageID) {
	previousImagesMu.Lock()
	defer previousImagesMu.Unlock()
	previousImages[containerName] = id
}

// PreviousImageForTest exposes the recorded prior digest for a container,
// for assertions in external tests.
func PreviousImageForTest(containerName string) types.ImageID {
	previousImagesMu.Lock()
	defer previousImagesMu.Unlock()
	return previousImages[containerName]
}

// RewindCooldownFirstSeenForTest shifts a container's recorded firstSeen
// backwards by the given amount, simulating the passage of wall-clock time
// without making tests sleep.
func RewindCooldownFirstSeenForTest(containerName string, by time.Duration) {
	pendingImagesMu.Lock()
	defer pendingImagesMu.Unlock()
	if entry, ok := pendingImages[containerName]; ok {
		entry.firstSeen = entry.firstSeen.Add(-by)
		pendingImages[containerName] = entry
	}
}

// errNoHealthcheck signals that the container has no HEALTHCHECK defined and
// health gating cannot be enforced. Treated as a warning, not a failure.
var errNoHealthcheck = errors.New("container has no HEALTHCHECK; gating skipped")

// Update looks at the running Docker containers to see if any of the images
// used to start those containers have been updated. If a change is detected in
// any of the images, the associated containers are stopped and restarted with
// the new image.
func Update(client container.Client, params types.UpdateParams) (types.Report, error) {
	log.Debug("Checking containers for updated images")
	start := time.Now()
	defer func() {
		metrics.SetLastScanTimestamp(time.Now())
		metrics.ObservePollDuration(time.Since(start))
		metrics.SetContainersInCooldown(pendingImageCount())
	}()
	progress := &session.Progress{}
	staleCount := 0

	if params.LifecycleHooks {
		lifecycle.ExecutePreChecks(client, params)
	}

	containers, err := client.ListContainers(params.Filter)
	if err != nil {
		return nil, err
	}

	staleCheckFailed := 0

	for i, targetContainer := range containers {
		stale, newestImage, err := client.IsContainerStale(targetContainer, params)
		if stale && err == nil && newestImage != "" {
			// Pin the upcoming recreate to the digest we just resolved so a
			// concurrent rebuild that untags name:latest between scan and
			// recreate doesn't trip ContainerCreate with "No such image".
			targetContainer.SetTargetImageID(newestImage)
		}
		if stale && err == nil {
			if remaining := rollbackCooldownRemaining(targetContainer.Name()); remaining > 0 {
				log.WithFields(log.Fields{
					"container": targetContainer.Name(),
					"retry_in":  remaining.Round(time.Second),
				}).Info("Skipping update: container is on post-rollback cooldown")
				stale = false
			}
		}
		// Supply-chain gate: hold the update until the new digest has been
		// stable for the configured cooldown window. Runs after the rollback
		// cooldown so a freshly-rolled-back container doesn't also get
		// gated twice. Bypassed under --run-once because "defer to next
		// poll" is meaningless when the daemon exits after this cycle —
		// the operator explicitly asked for an immediate, one-shot update.
		if stale && err == nil && !params.RunOnce {
			if cooldown := resolveImageCooldown(targetContainer, params); cooldown > 0 {
				if proceed, remaining := evaluateImageCooldown(targetContainer.Name(), newestImage, cooldown); !proceed {
					log.WithFields(log.Fields{
						"container": targetContainer.Name(),
						"digest":    newestImage.ShortID(),
						"retry_in":  remaining.Round(time.Second),
					}).Info("Skipping update: image cooldown window has not elapsed")
					stale = false
				}
			}
		}
		shouldUpdate := stale && !params.NoRestart && !targetContainer.IsMonitorOnly(params)
		if err == nil && shouldUpdate {
			// Check to make sure we have all the necessary information for recreating the container
			err = targetContainer.VerifyConfiguration()
			// If the image information is incomplete and trace logging is enabled, log it for further diagnosis
			if err != nil && log.IsLevelEnabled(log.TraceLevel) {
				imageInfo := targetContainer.ImageInfo()
				log.Tracef("Image info: %#v", imageInfo)
				log.Tracef("Container info: %#v", targetContainer.ContainerInfo())
				if imageInfo != nil {
					log.Tracef("Image config: %#v", imageInfo.Config)
				}
			}
		}

		if err != nil {
			if errors.Is(err, container.ErrPinnedImage) {
				// Pinned-tag containers are unupdatable by design — there's
				// no moving target. Drop to debug so steady-state digest-pinned
				// stacks don't spam every poll with a warn the operator can't
				// act on. Surrounding handling stays identical: still skipped,
				// still recorded in the progress report.
				log.WithField("container", targetContainer.Name()).Debug("Skipping container with pinned image")
			} else {
				log.Warnf("Unable to update container %q: %v. Proceeding to next.", targetContainer.Name(), err)
			}
			stale = false
			staleCheckFailed++
			progress.AddSkipped(targetContainer, err)
		} else {
			progress.AddScanned(targetContainer, newestImage)
		}
		containers[i].SetStale(stale)

		if stale {
			staleCount++
		}
	}

	containers, err = sorter.SortByDependencies(containers, params.ComposeDependsOn)
	if err != nil {
		return nil, err
	}

	UpdateImplicitRestart(containers)

	var containersToUpdate []types.Container
	for _, c := range containers {
		if !c.IsMonitorOnly(params) {
			containersToUpdate = append(containersToUpdate, c)
			progress.MarkForUpdate(c.ID())
		}
	}

	if params.RollingRestart {
		progress.UpdateFailed(performRollingRestart(containersToUpdate, containers, client, params, progress))
	} else {
		failedStop, stoppedImages := stopContainersInReversedOrder(containersToUpdate, client, params, progress)
		progress.UpdateFailed(failedStop)
		failedStart := restartContainersInSortedOrder(containersToUpdate, containers, client, params, stoppedImages)
		progress.UpdateFailed(failedStart)
	}

	if params.LifecycleHooks {
		lifecycle.ExecutePostChecks(client, params)
	}
	return progress.Report(), nil
}

func performRollingRestart(containers, scanView []types.Container, client container.Client, params types.UpdateParams, progress *session.Progress) map[types.ContainerID]error {
	cleanupImageIDs := make(map[types.ImageID]bool, len(containers))
	failed := make(map[types.ContainerID]error, len(containers))

	for i := len(containers) - 1; i >= 0; i-- {
		if containers[i].ToRestart() {
			err := stopStaleContainer(containers[i], client, params)
			switch {
			case errors.Is(err, container.ErrContainerNotFound):
				// Vanished mid-scan (typically a Compose recreate beat us to it).
				// Don't restart, don't fail — drop from the run with a Skipped
				// marker so the next scan can pick up whatever's there now.
				if progress != nil {
					progress.MarkSkipped(containers[i].ID(), err)
				}
			case err != nil:
				failed[containers[i].ID()] = err
			default:
				if err := restartStaleContainer(containers[i], client, params); err != nil {
					failed[containers[i].ID()] = err
				} else if containers[i].IsStale() {
					// Defer cleanup by one generation: hand --cleanup the
					// image this container was on *before* the previous
					// successful update, not the one we just retired.
					// The just-retired image stays on disk as the rollback
					// target until the next update of this container rotates
					// it out. SourceImageID stays the right field — the old
					// container was created from it, even if imageInfo now
					// holds a freshly-pulled replacement.
					if prior := rotatePreviousImage(containers[i].Name(), containers[i].SourceImageID()); prior != "" {
						cleanupImageIDs[prior] = true
					}
				}
			}
		}
	}

	if params.Cleanup {
		cleanupImages(client, cleanupImageIDs, scanView)
	}
	return failed
}

func stopContainersInReversedOrder(containers []types.Container, client container.Client, params types.UpdateParams, progress *session.Progress) (failed map[types.ContainerID]error, stopped map[types.ImageID]bool) {
	failed = make(map[types.ContainerID]error, len(containers))
	stopped = make(map[types.ImageID]bool, len(containers))
	for i := len(containers) - 1; i >= 0; i-- {
		err := stopStaleContainer(containers[i], client, params)
		switch {
		case errors.Is(err, container.ErrContainerNotFound):
			// Vanished mid-scan — most often a Compose recreate that beat us
			// to the punch. Skip both the failure tally and the stopped-images
			// set so the restart phase doesn't try to recreate a name that
			// already belongs to whatever Compose put there.
			if progress != nil {
				progress.MarkSkipped(containers[i].ID(), err)
			}
		case err != nil:
			failed[containers[i].ID()] = err
		default:
			// NOTE: If a container is restarted due to a dependency this might be empty
			stopped[containers[i].SafeImageID()] = true
		}
	}
	return failed, stopped
}

func stopStaleContainer(cont types.Container, client container.Client, params types.UpdateParams) error {
	if cont.IsWatchtower() {
		log.Debugf("This is the watchtower container %s", cont.Name())
		return nil
	}

	if !cont.ToRestart() {
		return nil
	}

	// Perform an additional check here to prevent us from stopping a linked container we cannot restart
	if cont.IsLinkedToRestarting() {
		if err := cont.VerifyConfiguration(); err != nil {
			return err
		}
	}

	if params.LifecycleHooks {
		skipUpdate, err := lifecycle.ExecutePreUpdateCommand(client, cont)
		if err != nil {
			// Pre-update command is a user-defined hook running inside the
			// container; a failure reflects on the hook script, not on
			// watchtower's orchestration. Warn is the right level — strict
			// NOTIFICATIONS_LEVEL=error shouldn't fire for user-script flakes.
			log.WithError(err).WithField("container", cont.Name()).Warn(
				"Skipping container: pre-update lifecycle command failed",
			)
			return err
		}
		if skipUpdate {
			log.Debug("Skipping container as the pre-update command returned exit code 75 (EX_TEMPFAIL)")
			return errors.New("skipping container as the pre-update command returned exit code 75 (EX_TEMPFAIL)")
		}
	}

	if err := client.StopContainer(cont, params.Timeout); err != nil {
		if errors.Is(err, container.ErrContainerNotFound) {
			log.WithField("container", cont.Name()).Debug("Container vanished before stop — skipping")
			return err
		}
		log.Error(err)
		return err
	}
	return nil
}

func restartContainersInSortedOrder(containers, scanView []types.Container, client container.Client, params types.UpdateParams, stoppedImages map[types.ImageID]bool) map[types.ContainerID]error {
	cleanupImageIDs := make(map[types.ImageID]bool, len(containers))
	failed := make(map[types.ContainerID]error, len(containers))

	for _, c := range containers {
		if !c.ToRestart() {
			continue
		}
		if stoppedImages[c.SafeImageID()] {
			if err := restartStaleContainer(c, client, params); err != nil {
				failed[c.ID()] = err
			} else if c.IsStale() {
				// Defer cleanup by one generation — see performRollingRestart
				// for the rationale. SourceImageID is still the right key.
				if prior := rotatePreviousImage(c.Name(), c.SourceImageID()); prior != "" {
					cleanupImageIDs[prior] = true
				}
			}
		}
	}

	if params.Cleanup {
		cleanupImages(client, cleanupImageIDs, scanView)
	}

	return failed
}

// cleanupImages removes each image in imageIDs unless another container in
// the scan view still references it. Force-removing an image that's pinning a
// non-stale container would break that container's next restart, so defer
// instead — the next scan will retry once the last referrer is gone or
// recreated. The check only sees containers visible to the active filter
// (label-enable/scope), so containers outside the scan are not protected
// here; that's the cost of avoiding an extra unfiltered Docker API call.
func cleanupImages(client container.Client, imageIDs map[types.ImageID]bool, scanView []types.Container) {
	for imageID := range imageIDs {
		if imageID == "" {
			continue
		}
		if isImageStillReferenced(scanView, imageID) {
			log.WithField("image", imageID.ShortID()).Debug("Image still referenced by an active container — deferring cleanup")
			continue
		}
		if err := client.RemoveImageByID(imageID); err != nil {
			log.Error(err)
		}
	}
}

// isImageStillReferenced reports whether any non-restart-marked container in
// the scan view references imageID. Containers marked ToRestart() are
// excluded because their old image reference is about to be replaced by the
// new one — they're being recreated as we speak. Non-restart containers
// (non-stale, monitor-only, dependency-skipped) DO count: their reference is
// load-bearing.
func isImageStillReferenced(scanView []types.Container, imageID types.ImageID) bool {
	for _, c := range scanView {
		if c.ToRestart() {
			continue
		}
		if c.SourceImageID() == imageID || c.SafeImageID() == imageID {
			return true
		}
	}
	return false
}

func restartStaleContainer(container types.Container, client container.Client, params types.UpdateParams) error {
	// Since we can't shutdown a watchtower container immediately, we need to
	// start the new one while the old one is still running. This prevents us
	// from re-using the same container name so we first rename the current
	// instance so that the new one can adopt the old name.
	if container.IsWatchtower() {
		// The rename-and-respawn pattern briefly overlaps the old and new
		// containers. That works fine for most setups, but if the current
		// watchtower is publishing host ports (e.g. --http-api-* mapped to
		// :8080 on the host), the new container's create call would fail
		// with "address already in use". Skip the self-update with a loud
		// warning so the operator knows to stop/pull/recreate manually
		// instead of silently wedging the update path. See upstream#1481.
		if container.HasPublishedPorts() {
			log.WithField("container", container.Name()).Warn(
				"Skipping self-update: watchtower has published host port bindings that would conflict with the rename-and-respawn pattern during the old/new overlap window. Update manually by stopping and recreating this container with the new image.",
			)
			return nil
		}
		if err := client.RenameContainer(container, util.RandName()); err != nil {
			log.Error(err)
			return nil
		}
	}

	if !params.NoRestart {
		newContainerID, err := client.StartContainer(container)
		if err != nil {
			log.Error(err)
			return err
		}
		if container.ToRestart() && params.LifecycleHooks {
			lifecycle.ExecutePostUpdateCommand(client, newContainerID)
		}
		if params.HealthCheckGated {
			if err := gateOnHealthCheck(client, container, newContainerID, params); err != nil {
				return err
			}
		}
	}
	return nil
}

// resolveHealthCheckTimeout picks the timeout to use for gating a specific
// container. Priority, highest first:
//  1. The container's own `com.centurylinklabs.watchtower.health-check-timeout`
//     label (operator override).
//  2. The timeout implied by its HEALTHCHECK config: start_period +
//     retries * (interval + timeout). This believes the image author's own
//     declaration of how long the container needs.
//  3. The global --health-check-timeout flag (params.HealthCheckTimeout).
//  4. defaultHealthCheckTimeout as a final belt-and-braces.
func resolveHealthCheckTimeout(c types.Container, params types.UpdateParams) time.Duration {
	if override, ok := c.HealthCheckTimeout(); ok {
		return override
	}
	if derived := derivedHealthCheckTimeout(c); derived > 0 {
		return derived
	}
	if params.HealthCheckTimeout > 0 {
		return params.HealthCheckTimeout
	}
	return defaultHealthCheckTimeout
}

// derivedHealthCheckTimeout computes an upper bound from the HEALTHCHECK
// config: start_period + retries * (interval + timeout). Returns 0 when
// neither the container override nor the image default define a HEALTHCHECK,
// or when the declared retries is zero.
func derivedHealthCheckTimeout(c types.Container) time.Duration {
	hc := containerOrImageHealthcheck(c)
	if hc == nil {
		return 0
	}
	retries := hc.Retries
	if retries <= 0 {
		// Docker default; see docker/docker/api/types/container.HealthConfig.
		const dockerDefaultRetries = 3
		retries = dockerDefaultRetries
	}
	interval := hc.Interval
	if interval <= 0 {
		const dockerDefaultInterval = 30 * time.Second
		interval = dockerDefaultInterval
	}
	perCheck := hc.Timeout
	if perCheck <= 0 {
		const dockerDefaultPerCheck = 30 * time.Second
		perCheck = dockerDefaultPerCheck
	}
	return hc.StartPeriod + time.Duration(retries)*(interval+perCheck)
}

func containerOrImageHealthcheck(c types.Container) *dockercontainer.HealthConfig {
	if info := c.ContainerInfo(); info != nil && info.Config != nil && info.Config.Healthcheck != nil {
		return info.Config.Healthcheck
	}
	if img := c.ImageInfo(); img != nil && img.Config != nil && img.Config.Healthcheck != nil {
		// image.InspectResponse.Config.Healthcheck is the same HealthcheckConfig
		// type under the hood; promote it to the container's shape so the
		// caller only has to handle one struct.
		return &dockercontainer.HealthConfig{
			Test:        img.Config.Healthcheck.Test,
			Interval:    img.Config.Healthcheck.Interval,
			Timeout:     img.Config.Healthcheck.Timeout,
			StartPeriod: img.Config.Healthcheck.StartPeriod,
			Retries:     img.Config.Healthcheck.Retries,
		}
	}
	return nil
}

// gateOnHealthCheck blocks until the replacement container is healthy or until
// the resolved timeout elapses. On failure it stops the new container and
// restarts the old one from the same config+image that were running before.
func gateOnHealthCheck(client container.Client, old types.Container, newID types.ContainerID, params types.UpdateParams) error {
	timeout := resolveHealthCheckTimeout(old, params)
	err := waitForHealthy(client, newID, timeout)
	if errors.Is(err, errNoHealthcheck) {
		log.WithField("container", old.Name()).Warn(
			"--health-check-gated is set but the container has no HEALTHCHECK — update proceeded without gating. Add a HEALTHCHECK or remove the flag to silence this warning.",
		)
		return nil
	}
	if err == nil {
		log.WithField("container", old.Name()).Debug("Container reported healthy; update accepted")
		return nil
	}

	log.WithError(err).WithField("container", old.Name()).Error(
		"Health check failed after update — rolling back to the previous image",
	)
	if rbErr := rollback(client, old, newID, params); rbErr != nil {
		return fmt.Errorf("rollback failed for %s: %w (original health-check error: %v)", old.Name(), rbErr, err)
	}
	return fmt.Errorf("update of %s rolled back after failed health check: %w", old.Name(), err)
}

// waitForHealthy polls the container's State.Health.Status every
// healthPollInterval. Returns nil once the status is Healthy, errNoHealthcheck
// if the container has no HEALTHCHECK, or an error on Unhealthy / timeout.
func waitForHealthy(client container.Client, id types.ContainerID, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = defaultHealthCheckTimeout
	}
	deadline := time.Now().Add(timeout)
	for {
		c, err := client.GetContainer(id)
		if err != nil {
			return fmt.Errorf("inspect new container: %w", err)
		}
		info := c.ContainerInfo()
		if info == nil || info.State == nil || info.State.Health == nil {
			return errNoHealthcheck
		}
		switch info.State.Health.Status {
		case dockercontainer.Healthy:
			return nil
		case dockercontainer.Unhealthy:
			return fmt.Errorf("container reported unhealthy after %s", time.Since(deadline.Add(-timeout)).Round(time.Second))
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for healthy (last status: %s)", timeout, info.State.Health.Status)
		}
		time.Sleep(healthPollInterval)
	}
}

// rollback tears down the unhealthy replacement and re-creates the previous
// container from the preserved Container struct. The old image is still on
// disk at this point because cleanup runs only after all restarts complete.
// The rolled-back container is itself health-gated with a shorter timeout
// (half the effective); if it too reports unhealthy, rollback logs at error
// and leaves the container in place rather than tearing the service down.
// Every rollback — successful or not — arms the cooldown so the same
// container isn't stopped and started every poll while the image author
// re-pushes broken versions.
func rollback(client container.Client, old types.Container, newID types.ContainerID, params types.UpdateParams) error {
	defer recordRollback(old.Name())

	newSnapshot, err := client.GetContainer(newID)
	if err != nil {
		log.WithError(err).Warnf("rollback: could not inspect new container %s, attempting restart of old anyway", newID.ShortID())
	} else if stopErr := client.StopContainer(newSnapshot, params.Timeout); stopErr != nil {
		log.WithError(stopErr).Warnf("rollback: failed to stop unhealthy new container %s", newSnapshot.Name())
	}

	// Repoint the recreate at the old image's digest. Without this, the
	// targetImageID set on the forward path would still hold the *new* image
	// and rollback would re-create with the broken version we just rejected.
	old.SetTargetImageID(old.ImageID())
	rolledBackID, err := client.StartContainer(old)
	if err != nil {
		return err
	}
	metrics.RegisterRollback()

	// Gate the rolled-back container with a shorter timeout. If the previous
	// image is also unhealthy (shared root cause: env changed, dependency
	// dropped, volume corrupted) there's nothing automation can do; surface
	// it loudly for the operator.
	const (
		rollbackTimeoutDivisor = 2
		rollbackFloorPolls     = 2
	)
	rollbackTimeout := resolveHealthCheckTimeout(old, params) / rollbackTimeoutDivisor
	floor := healthPollInterval * rollbackFloorPolls
	if rollbackTimeout < floor {
		rollbackTimeout = floor
	}
	if err := waitForHealthy(client, rolledBackID, rollbackTimeout); err != nil {
		if errors.Is(err, errNoHealthcheck) {
			log.WithFields(log.Fields{
				"container":   old.Name(),
				"rollback":    true,
				"rollback_ok": true,
			}).Warn("Rollback complete — previous image restored (no HEALTHCHECK to verify)")
			return nil
		}
		log.WithError(err).WithFields(log.Fields{
			"container":       old.Name(),
			"rollback":        true,
			"rollback_failed": true,
		}).Error("Rollback restored the previous image but it is also unhealthy — manual intervention required")
		return fmt.Errorf("rolled-back container %s is also unhealthy: %w", old.Name(), err)
	}

	log.WithFields(log.Fields{
		"container":   old.Name(),
		"rollback":    true,
		"rollback_ok": true,
	}).Warn("Rollback complete — previous image restored")
	return nil
}

// UpdateImplicitRestart iterates through the passed containers, setting the
// `LinkedToRestarting` flag if any of it's linked containers are marked for restart
func UpdateImplicitRestart(containers []types.Container) {
	for ci, c := range containers {
		if c.ToRestart() {
			// The container is already marked for restart, no need to check
			continue
		}

		if link := linkedContainerMarkedForRestart(c.Links(), containers); link != "" {
			log.WithFields(log.Fields{
				"restarting": link,
				"linked":     c.Name(),
			}).Debug("container is linked to restarting")
			// NOTE: To mutate the array, the `c` variable cannot be used as it's a copy
			containers[ci].SetLinkedToRestarting(true)
		}
	}
}

// linkedContainerMarkedForRestart returns the name of the first link that matches a
// container marked for restart
func linkedContainerMarkedForRestart(links []string, containers []types.Container) string {
	for _, linkName := range links {
		for _, candidate := range containers {
			if candidate.Name() == linkName && candidate.ToRestart() {
				return linkName
			}
		}
	}
	return ""
}
