package actions

import (
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"time"

	dockercontainer "github.com/docker/docker/api/types/container"
	log "github.com/sirupsen/logrus"

	"github.com/openserbia/watchtower/internal/initrerun"
	"github.com/openserbia/watchtower/internal/util"
	"github.com/openserbia/watchtower/pkg/container"
	"github.com/openserbia/watchtower/pkg/lifecycle"
	"github.com/openserbia/watchtower/pkg/metrics"
	"github.com/openserbia/watchtower/pkg/session"
	"github.com/openserbia/watchtower/pkg/sorter"
	"github.com/openserbia/watchtower/pkg/types"
)

const (
	// defaultHealthCheckTimeout mirrors the default on --health-check-timeout
	// in internal/flags. Only used as a belt-and-braces fallback if the param
	// arrives zero (e.g. old callers constructing UpdateParams by hand).
	defaultHealthCheckTimeout = 60 * time.Second
	// rollbackCooldown is how long we skip a container after its last rollback.
	// Prevents the poll → stop → start → fail → rollback loop from thrashing
	// every poll when an image author has pushed a broken version.
	rollbackCooldown = 1 * time.Hour
	// defaultBlueGreenDrain is the fallback drain window kept between the new
	// and old container after the new one reports healthy, when neither the
	// container label nor the global flag specifies one.
	defaultBlueGreenDrain = 10 * time.Second
)

// healthPollInterval is how often the gating loop re-checks a container's
// health status. Short enough that a 30-second startup doesn't get flagged
// late; long enough that we don't hammer the Docker socket. It is a var (not
// a const) so tests can lower it.
var healthPollInterval = 2 * time.Second

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

	rejectedInitDigestsMu sync.Mutex
	// rejectedInitDigests holds image digests whose --rerun-init-deps init
	// container exited non-zero in the current Watchtower process. Keyed by
	// digest, so a target whose newest image still resolves to a rejected
	// digest stays skipped until the registry serves a different digest
	// (operator pushed a fix). In-memory only — clears on Watchtower
	// restart, which is intentional: a restart represents an operator
	// touch-point that should trigger one retry.
	rejectedInitDigests = make(map[types.ImageID]rejectionReason)

	selfStartFailMu sync.Mutex
	// selfStartFailures tracks the last time we *notified* about a self-update
	// start failure, keyed by container name + a short signature of the error.
	// A wedged self-update re-logs the same start failure every poll (default
	// 60s); promoting each repeat to an Error turns logrus's notification hook
	// into a per-poll storm. We notify on the first occurrence (and on any
	// genuinely different failure) and suppress identical repeats within
	// selfStartFailCooldown, logging them at Debug instead. In-memory only —
	// resets on restart, which is the right behavior: a restart is an operator
	// touch-point that should surface the next failure loudly. Mirrors the
	// knownUnmanaged + auditMu dedup pattern in check.go.
	selfStartFailures = make(map[string]time.Time)
)

// selfStartFailCooldown is the window during which an identical self-update
// start failure is suppressed (logged at Debug) after it has already notified
// once. A var so tests can shrink it.
var selfStartFailCooldown = 1 * time.Hour

type pendingImage struct {
	digest    types.ImageID
	firstSeen time.Time
}

// rejectionReason captures why a digest is in the rejected-digest cache.
// Surfaced in skip-reason logs so operators can correlate a stuck container
// with the upstream init-container failure.
type rejectionReason struct {
	Container string
	Dep       string
	ExitCode  int
	At        time.Time
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

// isRejectedInitDigest reports whether the given digest previously failed
// --rerun-init-deps in this Watchtower process. Used to short-circuit
// stale-checks without flooding logs every poll.
func isRejectedInitDigest(id types.ImageID) (rejectionReason, bool) {
	rejectedInitDigestsMu.Lock()
	defer rejectedInitDigestsMu.Unlock()
	r, ok := rejectedInitDigests[id]
	return r, ok
}

func rejectInitDigest(id types.ImageID, reason rejectionReason) {
	rejectedInitDigestsMu.Lock()
	defer rejectedInitDigestsMu.Unlock()
	rejectedInitDigests[id] = reason
}

// ResetRejectedInitDigestsForTest clears the in-memory rejected-digest cache.
// Test-only helper, parallel to the cooldown ResetFor… utilities above.
func ResetRejectedInitDigestsForTest() {
	rejectedInitDigestsMu.Lock()
	defer rejectedInitDigestsMu.Unlock()
	rejectedInitDigests = make(map[types.ImageID]rejectionReason)
}

// shouldNotifySelfStartFailure reports whether a self-update start failure for
// the given container name and error should be logged at Error (which the
// logrus notification hook turns into a notification) or suppressed to Debug.
// It returns true — notify — on the first sighting of a (name, error-signature)
// pair and again once selfStartFailCooldown has elapsed, recording the
// notification time when it does. A genuinely different failure has a different
// signature and so always notifies. Identical repeats inside the cooldown
// return false. Mirrors the knownUnmanaged dedup in check.go.
func shouldNotifySelfStartFailure(containerName string, err error) bool {
	key := containerName + "\x00" + selfStartFailSignature(err)
	now := time.Now()

	selfStartFailMu.Lock()
	defer selfStartFailMu.Unlock()

	if last, seen := selfStartFailures[key]; seen && now.Sub(last) < selfStartFailCooldown {
		return false
	}
	selfStartFailures[key] = now
	return true
}

// selfStartFailSignature reduces an error to a short, stable key so that
// repeated identical failures dedup but a different failure mode still
// notifies. The full error string already varies little poll-to-poll for the
// same root cause (e.g. a fixed "address already in use" bind), so we key on
// it directly; an empty error never occurs on this path but maps to a constant
// for safety.
func selfStartFailSignature(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// ResetSelfStartFailuresForTest clears the in-memory self-update start-failure
// dedup cache. Test-only helper, parallel to ResetAuditStateForTest.
func ResetSelfStartFailuresForTest() {
	selfStartFailMu.Lock()
	defer selfStartFailMu.Unlock()
	selfStartFailures = make(map[string]time.Time)
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

// resolveStrategy picks the effective update strategy for a container: the
// per-container label override if present, otherwise the global default,
// falling back to recreate.
func resolveStrategy(c types.Container, params types.UpdateParams) types.UpdateStrategy {
	if v, ok := c.UpdateStrategyLabel(); ok {
		return types.UpdateStrategy(v)
	}
	if params.Strategy != "" {
		return params.Strategy
	}
	return types.StrategyRecreate
}

// resolveBlueGreenDrain picks the effective blue-green drain window for a
// container: the per-container label override if present, otherwise the global
// default, falling back to defaultBlueGreenDrain.
func resolveBlueGreenDrain(c types.Container, params types.UpdateParams) time.Duration {
	if d, ok := c.BlueGreenDrain(); ok {
		return d
	}
	if params.BlueGreenDrain > 0 {
		return params.BlueGreenDrain
	}
	return defaultBlueGreenDrain
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
//
// rejected-init-digest cache, pre-update hooks, health-gating, lifecycle
// hooks, rollback, and self-update interleave intentionally. A refactor
// here is tracked separately and risks behavior drift on the hot path.
//
//nolint:cyclop // central update orchestration: image-cooldown checks,
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
				}).Debug("Skipping update: container is on post-rollback cooldown")
				stale = false
			}
		}
		// --rerun-init-deps rejection gate: a digest that previously failed
		// its init container stays skipped until the registry serves a new
		// one. We check the resolved newestImage, not the container's
		// currently-running digest, so an operator's pushed-fix is detected
		// by digest change at the next poll.
		if stale && err == nil && params.RerunInitDeps && newestImage != "" {
			if reason, rejected := isRejectedInitDigest(newestImage); rejected {
				log.WithFields(log.Fields{
					"container":  targetContainer.Name(),
					"digest":     newestImage.ShortID(),
					"failed_dep": reason.Dep,
					"exit_code":  reason.ExitCode,
					"at":         reason.At.Round(time.Second),
				}).Debug("Skipping update: digest previously failed --rerun-init-deps")
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
					}).Debug("Skipping update: image cooldown window has not elapsed")
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
				log.WithError(err).WithFields(log.Fields{
					"container": targetContainer.Name(),
					"image":     targetContainer.ImageName(),
				}).Warn("Unable to update container — proceeding to next")
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

	// --rerun-init-deps: re-execute compose service_completed_successfully
	// siblings against the new image *before* the target is recreated. If
	// the init container fails the target keeps its old image (Stale
	// unset) and the new digest is cached so it isn't retried until a
	// newer one appears. Runs after the staleness loop so each target's
	// TargetImageID is already pinned to a resolved digest.
	if params.RerunInitDeps {
		for i := range containers {
			if !containers[i].IsStale() {
				continue
			}
			target := containers[i]
			if len(target.ComposeInitDependencies()) == 0 {
				continue
			}
			results := initrerun.Run(client, target, containers, initrerun.DefaultTimeout)
			if len(results) == 0 {
				continue
			}
			last := results[len(results)-1]
			if last.Succeeded() {
				continue
			}
			rejectInitDigest(target.TargetImageID(), rejectionReason{
				Container: target.Name(),
				Dep:       last.DepName,
				ExitCode:  last.ExitCode,
				At:        time.Now(),
			})
			containers[i].SetStale(false)
			staleCount--
			progress.AddSkipped(target, fmt.Errorf("--rerun-init-deps: init container %q failed (exit %d); old container kept", last.DepName, last.ExitCode))
			log.WithFields(log.Fields{
				"container": target.Name(),
				"dep":       last.DepName,
				"exit_code": last.ExitCode,
				"digest":    target.TargetImageID().ShortID(),
			}).Warn("--rerun-init-deps: skipping target update, digest cached as rejected")
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

	recreateSet, rollingSet, blueGreenSet := partitionByStrategy(containersToUpdate, params)

	for _, c := range blueGreenSet {
		if err := performBlueGreenUpdate(c, containers, client, params); err != nil {
			progress.UpdateFailed(map[types.ContainerID]error{c.ID(): err})
		}
	}

	if len(rollingSet) > 0 {
		progress.UpdateFailed(performRollingRestart(rollingSet, containers, client, params, progress))
	}

	if len(recreateSet) > 0 {
		failedStop, stoppedImages := stopContainersInReversedOrder(recreateSet, client, params, progress)
		progress.UpdateFailed(failedStop)
		progress.UpdateFailed(restartContainersInSortedOrder(recreateSet, containers, client, params, stoppedImages))
	}

	if params.LifecycleHooks {
		lifecycle.ExecutePostChecks(client, params)
	}
	return progress.Report(), nil
}

// partitionByStrategy splits the containers slated for update into the
// recreate, rolling-restart, and blue-green buckets according to each
// container's resolved strategy. Watchtower's own container, structurally
// ineligible blue-green candidates, and containers that publish host ports
// always fall back to recreate so default behavior is unchanged.
func partitionByStrategy(containersToUpdate []types.Container, params types.UpdateParams) (recreateSet, rollingSet, blueGreenSet []types.Container) {
	for _, c := range containersToUpdate {
		if isRunningSelf(c, params) {
			recreateSet = append(recreateSet, c)

			continue
		}

		switch resolveStrategy(c, params) {
		case types.StrategyBlueGreen:
			switch {
			case !c.ToRestart() || !c.IsStale() || len(c.Links()) > 0:
				recreateSet = append(recreateSet, c) // structural ineligibility; no warning
			case c.HasPublishedPorts():
				log.WithField("container", c.Name()).Warn("blue-green is not possible for a container that publishes host ports (two copies cannot bind the same port) — falling back to recreate. Route through a dynamic reverse proxy on the docker network instead of host ports to enable blue-green.")
				recreateSet = append(recreateSet, c)
			default:
				blueGreenSet = append(blueGreenSet, c)
			}
		case types.StrategyRollingRestart:
			rollingSet = append(rollingSet, c)
		default:
			recreateSet = append(recreateSet, c)
		}
	}

	return recreateSet, rollingSet, blueGreenSet
}

// performBlueGreenUpdate brings up a new "green" container alongside the running
// "blue" one, waits for green to report healthy, lets a drain window elapse so a
// dynamic label-based reverse proxy (e.g. Traefik) can register green and in-flight
// requests on blue can finish, then retires blue and renames green to blue's
// canonical name. On a failed health check it removes green and leaves blue serving.
// The caller guarantees the container has no published host ports and is not self.
func performBlueGreenUpdate(blue types.Container, scanView []types.Container, client container.Client, params types.UpdateParams) error {
	if params.LifecycleHooks {
		if err := runBlueGreenPreUpdate(client, blue); err != nil {
			return err
		}
	}

	canonical := blue.CreateName()
	bareName := strings.TrimPrefix(canonical, "/")
	greenName := bareName + "-wt-bluegreen-" + util.RandName()[:8]

	// Start green from blue's config under a temporary unique name so both run
	// side by side. StartContainer honors SetCreateName and the TargetImageID
	// retag, so green comes up on the new image carrying blue's labels (the proxy
	// treats them as one service with two backends).
	blue.SetCreateName(greenName)
	greenID, err := client.StartContainer(blue)
	blue.SetCreateName(canonical) // restore so later log lines use the real name
	if err != nil {
		log.WithError(err).WithFields(log.Fields{"container": bareName, "image": blue.ImageName()}).Errorf("blue-green: failed to start green container %s (image %s): %v; old container left untouched", bareName, blue.ImageName(), err)

		return err
	}
	log.WithFields(log.Fields{"container": bareName, "green": greenName}).Info("blue-green: started green container, waiting for it to become healthy")

	if err := awaitBlueGreenHealthy(client, blue, greenID, greenName, bareName, params); err != nil {
		return err
	}

	if drain := resolveBlueGreenDrain(blue, params); drain > 0 {
		log.WithFields(log.Fields{"container": bareName, "drain": drain}).Debug("blue-green: draining before retiring the old container")
		time.Sleep(drain)
	}

	if err := client.StopContainer(blue, params.Timeout); err != nil && !errors.Is(err, container.ErrContainerNotFound) {
		log.WithError(err).WithFields(log.Fields{"container": bareName, "image": blue.ImageName(), "green": greenName}).Errorf("blue-green: failed to stop the old container %s (image %s): %v; green is live but the old one remains and green keeps its temporary name", bareName, blue.ImageName(), err)

		return err
	}

	if greenSnap, gerr := client.GetContainer(greenID); gerr == nil {
		if rerr := client.RenameContainer(greenSnap, bareName); rerr != nil {
			log.WithError(rerr).WithFields(log.Fields{"from": greenName, "to": bareName, "image": blue.ImageName()}).Warnf("blue-green: failed to rename green to the canonical name %s (image %s): %v; it keeps its temporary name until the next update", bareName, blue.ImageName(), rerr)
		}
	}

	if params.LifecycleHooks {
		lifecycle.ExecutePostUpdateCommand(client, greenID)
	}

	if params.Cleanup {
		if prior := rotatePreviousImage(blue.Name(), blue.SourceImageID()); prior != "" {
			cleanupImages(client, map[types.ImageID]bool{prior: true}, scanView)
		}
	}

	log.WithFields(log.Fields{"container": bareName, "image": blue.ImageName()}).Info("blue-green: cutover complete")

	return nil
}

// runBlueGreenPreUpdate runs the pre-update lifecycle command before a
// blue-green cutover. It returns a non-nil error to abort the update when the
// command failed or signaled EX_TEMPFAIL (exit 75).
func runBlueGreenPreUpdate(client container.Client, blue types.Container) error {
	skipUpdate, err := lifecycle.ExecutePreUpdateCommand(client, blue)
	if err != nil {
		log.WithError(err).WithFields(log.Fields{"container": blue.Name(), "image": blue.ImageName()}).Warn("Skipping blue-green update: pre-update lifecycle command failed")

		return err
	}
	if skipUpdate {
		log.Debug("Skipping blue-green update: pre-update command returned exit code 75 (EX_TEMPFAIL)")

		return errors.New("skipping container as the pre-update command returned exit code 75 (EX_TEMPFAIL)")
	}

	return nil
}

// awaitBlueGreenHealthy waits for the green container to report healthy. A
// missing HEALTHCHECK is a warning (the drain window is the only gate). A real
// health failure removes green, arms the rollback cooldown, and returns an
// error so blue keeps serving.
func awaitBlueGreenHealthy(client container.Client, blue types.Container, greenID types.ContainerID, greenName, bareName string, params types.UpdateParams) error {
	timeout := resolveHealthCheckTimeout(blue, params)
	werr := waitForHealthy(client, greenID, timeout)
	switch {
	case errors.Is(werr, errNoHealthcheck):
		log.WithField("container", bareName).Warn("blue-green: green container has no HEALTHCHECK — readiness cannot be verified; relying on the drain window only. Add a HEALTHCHECK for a real readiness gate.")

		return nil
	case werr == nil:
		return nil
	}

	fields := log.Fields{"container": bareName, "image": blue.ImageName(), "green": greenName}
	maps.Copy(fields, healthFailureContext(client, greenID))
	log.WithError(werr).WithFields(fields).Errorf("blue-green: green container %s (image %s) failed health check (%v) — removing green and keeping the old container", bareName, blue.ImageName(), werr)
	if greenSnap, gerr := client.GetContainer(greenID); gerr == nil {
		if serr := client.StopContainer(greenSnap, params.Timeout); serr != nil && !errors.Is(serr, container.ErrContainerNotFound) {
			log.WithError(serr).WithFields(log.Fields{"green": greenName, "image": blue.ImageName()}).Errorf("blue-green: failed to remove the failed green container %s (image %s): %v", greenName, blue.ImageName(), serr)
		}
	}
	recordRollback(blue.Name())
	metrics.RegisterRollback()

	return fmt.Errorf("blue-green update of %s aborted after failed health check: %w", blue.Name(), werr)
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

// isRunningSelf reports whether cont is the watchtower process's own
// container. When SelfContainerID was successfully detected at startup,
// only the container with that exact ID counts as self. When detection
// failed (watchtower running outside a container, or --hostname overridden
// off the short ID) we fall back to the legacy IsWatchtower label check —
// which loses the orphan-vs-self distinction but matches upstream behavior.
func isRunningSelf(cont types.Container, params types.UpdateParams) bool {
	if params.SelfContainerID != "" {
		return cont.ID() == params.SelfContainerID
	}
	return cont.IsWatchtower()
}

func stopStaleContainer(cont types.Container, client container.Client, params types.UpdateParams) error {
	// Only the actual running self skips the stop — it can't kill itself, so
	// restartStaleContainer's rename-and-respawn pattern handles it instead.
	// Other watchtower-labeled containers (orphans from a prior self-update
	// whose post-restart cleanup did not finish) take the normal stop+remove
	// path so they don't accumulate or get re-spawned with random names.
	if isRunningSelf(cont, params) {
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
			log.WithError(err).WithFields(log.Fields{
				"container": cont.Name(),
				"image":     cont.ImageName(),
			}).Warn("Skipping container: pre-update lifecycle command failed")
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
		log.WithError(err).WithFields(log.Fields{
			"container": cont.Name(),
			"image":     cont.ImageName(),
		}).Errorf("Failed to stop container %s (image %s): %v", cont.Name(), cont.ImageName(), err)
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
			log.WithError(err).WithField("image", imageID.ShortID()).Error("Failed to remove image")
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

// restartStaleContainer is the rename-and-respawn coordinator for one
// container the scan flagged as stale. Self-update gating, name-rescue
// heuristics, hostname-clear flag wiring, lifecycle hooks, and the
// post-create safety net all live here intentionally — each branch is
// the recovery path for one well-understood operational failure mode.
//
// state-machine across helpers. Refactor tracked separately.
//
//nolint:cyclop // linear orchestrator: split would scatter the
func restartStaleContainer(container types.Container, client container.Client, params types.UpdateParams) error {
	// Orphan watchtower-labeled containers (random-named leftovers from a
	// previous self-update whose CheckForMultipleWatchtowerInstances pass did
	// not complete) reach this point already stopped+removed by
	// stopStaleContainer. Re-creating them would just resurrect the orphan
	// under its random name, so skip the recreate entirely. The actual
	// running self still goes through the rename-respawn branch below.
	if container.IsWatchtower() && !isRunningSelf(container, params) {
		log.WithField("container", container.Name()).Info(
			"Skipping respawn of orphan watchtower-labeled container — it was stopped+removed; recreating would revive the orphan with its random name.",
		)
		return nil
	}

	// Since we can't shutdown a watchtower container immediately, we need to
	// start the new one while the old one is still running. This prevents us
	// from re-using the same container name so we first rename the current
	// instance so that the new one can adopt the old name.
	wasRunningSelf := isRunningSelf(container, params)

	if wasRunningSelf {
		// When the cached Name looks like the output of util.RandName() (32
		// chars of [a-zA-Z]), it was set by a *previous* self-update's
		// rename-and-respawn — not by the operator. Faithfully propagating
		// that random name forward is the bug class the safety net was
		// supposed to catch but can't, because it compares the new
		// container's name to a cached "original" that is itself random.
		// Recover the canonical name from the compose service label, which
		// survives docker rename, when available.
		cachedName := container.Name()
		trimmed := strings.TrimPrefix(cachedName, "/")
		if util.IsRandName(trimmed) {
			if service := container.ComposeService(); service != "" {
				canonical := "/" + service
				log.WithFields(log.Fields{
					"cached_name": cachedName,
					"canonical":   canonical,
				}).Info("Self-update: cached name looks like a previous rename target; deriving canonical name from compose service label")
				container.SetCreateName(canonical)
			}
		}
	}
	originalName := container.CreateName()

	if wasRunningSelf {
		// The rename-and-respawn pattern briefly overlaps the old and new
		// containers. That works fine for most setups, but if the current
		// watchtower is publishing host ports (e.g. --http-api-* mapped to
		// :8080 on the host), the new container's create call would fail
		// with "address already in use". Skip the self-update with a loud
		// warning so the operator knows to stop/pull/recreate manually
		// instead of silently wedging the update path. See upstream#1481.
		if container.HasPublishedPorts() {
			log.WithField("container", originalName).Warn(
				"Skipping self-update: watchtower has published host port bindings that would conflict with the rename-and-respawn pattern during the old/new overlap window. Update manually by stopping and recreating this container with the new image.",
			)
			return nil
		}
		// Break the os.Hostname()-drift chain that otherwise propagates the
		// founding container's short ID into every subsequent self-update
		// and silently degrades DetectSelfContainerID + the startup-time
		// CheckForMultipleWatchtowerInstances to label-only matching. With
		// Hostname cleared on this recreate, the new container's hostname
		// equals its own short ID and self-detection stays accurate.
		container.SetClearHostnameOnRecreate(true)
		if err := client.RenameContainer(container, util.RandName()); err != nil {
			log.WithError(err).WithFields(log.Fields{
				"container": originalName,
				"image":     container.ImageName(),
			}).Errorf("Failed to rename container %s (image %s): %v", originalName, container.ImageName(), err)
			return nil
		}
	}

	if !params.NoRestart {
		newContainerID, err := client.StartContainer(container)
		if err != nil {
			entry := log.WithError(err).WithFields(log.Fields{
				"container": originalName,
				"image":     container.ImageName(),
			})
			// A self-update that keeps failing re-enters this branch every poll
			// (default 60s) with the same error, and each Error becomes a
			// notification via the logrus hook. Dedup the self path so only the
			// first occurrence — and any genuinely different failure — notifies;
			// identical repeats inside the cooldown drop to Debug. Non-self
			// failures are unaffected and always log at Error.
			if wasRunningSelf && !shouldNotifySelfStartFailure(originalName, err) {
				entry.Debugf("Failed to start container %s (image %s) again; suppressing repeat notification: %v", originalName, container.ImageName(), err)
				return err
			}
			entry.Errorf("Failed to start container %s (image %s): %v", originalName, container.ImageName(), err)
			return err
		}
		if wasRunningSelf {
			verifySelfContainerName(client, newContainerID, originalName)
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

// verifySelfContainerName is the belt-and-suspenders companion to the
// SelfContainerID-based orphan-skip in restartStaleContainer. The primary
// mechanism (DetectSelfContainerID at startup + orphan-skip at restart
// time) is meant to keep the rename-and-respawn pattern from firing on
// orphan watchtower-labeled containers — when it works, the new self
// container always inherits the canonical name (e.g. "watchtower" from a
// compose container_name: directive) via StartContainer's c.Name() path.
//
// In practice, the primary mechanism degrades silently after the first
// self-update: ContainerCreate carries the old container's Hostname into
// the new one, so os.Hostname() inside the new self no longer matches
// any live container's short ID and DetectSelfContainerID returns "" —
// pushing isRunningSelf back to the label-only fallback that treats
// every watchtower-labeled container as self. A transient orphan during
// that window can then have its random name copied onto the new
// container.
//
// This check inspects the freshly-created container by ID and renames
// it back to the canonical name when it diverged. It is intentionally
// best-effort: failures log a warning but never abort the update, since
// the new container is already running the right image and an
// out-of-band rename is a cosmetic concern rather than a service one.
func verifySelfContainerName(client container.Client, newID types.ContainerID, expected string) {
	fresh, err := client.GetContainer(newID)
	if err != nil {
		log.WithError(err).WithField("id", newID.ShortID()).Warn(
			"Self-update safety net: GetContainer failed; cannot verify new container's name",
		)
		return
	}
	actual := fresh.Name()
	if actual == expected {
		return
	}
	canonical := strings.TrimPrefix(expected, "/")
	log.WithFields(log.Fields{
		"expected": expected,
		"actual":   actual,
		"id":       newID.ShortID(),
	}).Warn("Self-update safety net: new container's name diverged from the original; renaming back to canonical")
	if err := client.RenameContainer(fresh, canonical); err != nil {
		log.WithError(err).WithFields(log.Fields{
			"from": actual,
			"to":   canonical,
		}).Error("Self-update safety net: rename-back failed; container will keep the wrong name until next manual fix")
	}
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

	fields := log.Fields{
		"container":  old.Name(),
		"image":      old.ImageName(),
		"new_digest": old.TargetImageID().ShortID(),
		"old_digest": old.ImageID().ShortID(),
	}
	// Inspect the new container *before* rollback tears it down to capture
	// why the gate failed — OOM-kill, non-zero exit, or the trimmed output of
	// the last failing probe. Without this an operator only sees "rolling
	// back" and has to reproduce locally to find the cause.
	maps.Copy(fields, healthFailureContext(client, newID))
	log.WithError(err).WithFields(fields).Errorf("Health check failed after updating %s (image %s): %v — rolling back to the previous image", old.Name(), old.ImageName(), err)
	if rbErr := rollback(client, old, newID, params); rbErr != nil {
		return fmt.Errorf("rollback failed for %s: %w (original health-check error: %w)", old.Name(), rbErr, err)
	}
	return fmt.Errorf("update of %s rolled back after failed health check: %w", old.Name(), err)
}

// healthFailureContext gathers actionable diagnostics for an unhealthy
// container: the kernel OOM-kill flag, the container's last exit code, and
// the trimmed output of its most recent failing healthcheck probe. Folded
// into the rollback log so an operator can see *why* the new image failed
// without re-inspecting the container — which is impossible after rollback,
// since the new container has been stopped and removed by then.
func healthFailureContext(client container.Client, id types.ContainerID) log.Fields {
	fields := log.Fields{}
	c, err := client.GetContainer(id)
	if err != nil {
		return fields
	}
	info := c.ContainerInfo()
	if info == nil || info.State == nil {
		return fields
	}
	if info.State.OOMKilled {
		fields["oom_killed"] = true
	}
	if info.State.ExitCode != 0 {
		fields["exit_code"] = info.State.ExitCode
	}
	if info.State.Health != nil && len(info.State.Health.Log) > 0 {
		last := info.State.Health.Log[len(info.State.Health.Log)-1]
		fields["probe_exit_code"] = last.ExitCode
		// Healthcheck probe stdout/stderr is unbounded — trim so a chatty
		// probe doesn't blow up the log line or the notification body.
		const maxProbeOutput = 240
		out := strings.TrimSpace(last.Output)
		if out != "" {
			if len(out) > maxProbeOutput {
				out = out[:maxProbeOutput] + "…"
			}
			fields["probe_output"] = out
		}
	}
	return fields
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
		log.WithError(err).WithFields(log.Fields{
			"container": old.Name(),
			"image":     old.ImageName(),
			"new_id":    newID.ShortID(),
		}).Warn("rollback: could not inspect new container, attempting restart of old anyway")
	} else if stopErr := client.StopContainer(newSnapshot, params.Timeout); stopErr != nil {
		log.WithError(stopErr).WithFields(log.Fields{
			"container": newSnapshot.Name(),
			"image":     newSnapshot.ImageName(),
		}).Warn("rollback: failed to stop unhealthy new container")
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
				"image":       old.ImageName(),
				"old_digest":  old.ImageID().ShortID(),
				"rollback":    true,
				"rollback_ok": true,
			}).Warn("Rollback complete — previous image restored (no HEALTHCHECK to verify)")
			return nil
		}
		log.WithError(err).WithFields(log.Fields{
			"container":       old.Name(),
			"image":           old.ImageName(),
			"old_digest":      old.ImageID().ShortID(),
			"rollback":        true,
			"rollback_failed": true,
		}).Error("Rollback restored the previous image but it is also unhealthy — manual intervention required")
		return fmt.Errorf("rolled-back container %s is also unhealthy: %w", old.Name(), err)
	}

	log.WithFields(log.Fields{
		"container":   old.Name(),
		"image":       old.ImageName(),
		"old_digest":  old.ImageID().ShortID(),
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
