package actions

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	dockercontainer "github.com/moby/moby/api/types/container"
	log "github.com/sirupsen/logrus"

	"github.com/openserbia/watchtower/internal/initrerun"
	"github.com/openserbia/watchtower/internal/util"
	"github.com/openserbia/watchtower/pkg/container"
	"github.com/openserbia/watchtower/pkg/filters"
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

// errNoHealthcheck signals that the container has no HEALTHCHECK defined and
// health gating cannot be enforced. Treated as a warning, not a failure.
var errNoHealthcheck = errors.New("container has no HEALTHCHECK; gating skipped")

// Update looks at the running Docker containers to see if any of the images
// used to start those containers have been updated. If a change is detected in
// any of the images, the associated containers are stopped and restarted with
// the new image.
//
// The run is a pipeline of phases over the scanned container list:
//
//	scan -> init-reruns -> plan -> execute -> report
//
// scanPhase is the only writer of the per-container Stale flag (initRerunPhase
// may veto it back to false for a failed migration — the one sanctioned
// reversal); planPhase then answers "what will this run actually touch"
// exactly once, and executePhase acts on that immutable plan. The willUpdate
// predicate is the single source of truth for update eligibility, shared by
// the scan and init-rerun phases, so the two can never drift apart again (the
// bug class where monitor-only targets had their migrations executed).
func Update(ctx context.Context, client container.Client, params types.UpdateParams, state *State) (types.Report, error) {
	log.Debug("Checking containers for updated images")
	start := time.Now()
	defer func() {
		metrics.SetLastScanTimestamp(time.Now())
		metrics.ObservePollDuration(time.Since(start))
		metrics.SetContainersInCooldown(state.pendingImageCount())
	}()
	progress := &session.Progress{}

	if params.LifecycleHooks {
		lifecycle.ExecutePreChecks(ctx, client, params)
	}

	containers, err := client.ListContainers(ctx, params.Filter)
	if err != nil {
		return nil, err
	}

	scanPhase(ctx, client, containers, params, state, progress)

	if params.RerunInitDeps {
		initRerunPhase(ctx, client, containers, params, state, progress)
	}

	plan, err := planPhase(containers, params, progress)
	if err != nil {
		return nil, err
	}

	executePhase(ctx, client, plan, params, state, progress)

	if params.LifecycleHooks {
		lifecycle.ExecutePostChecks(ctx, client, params)
	}
	return progress.Report(), nil
}

// willUpdate reports whether this run will actually recreate the container:
// it survived the scan gates as stale AND the executor is allowed to touch
// it. This is THE update-eligibility predicate — every phase that needs the
// answer asks here, so side-effecting work (init-dep reruns) can never run
// for a target the executor would then skip. Only meaningful once scanPhase
// has stamped the Stale flag. (The plan's optimistic report-marking
// deliberately asks the broader ToRestart instead: a container restarted
// because a linked dependency restarts is touched without being updated.)
func willUpdate(c types.Container, params types.UpdateParams) bool {
	return c.IsStale() && !params.NoRestart && !c.IsMonitorOnly(params)
}

// scanPhase runs the staleness check and the post-scan gates for every
// container, records each outcome in the progress report, and stamps the
// per-container Stale flag that the rest of the pipeline treats as input.
func scanPhase(ctx context.Context, client container.Client, containers []types.Container, params types.UpdateParams, state *State, progress *session.Progress) {
	for i, targetContainer := range containers {
		stale, newestImage, err := client.IsContainerStale(ctx, targetContainer, params)
		if stale && err == nil && newestImage != "" {
			// Pin the upcoming recreate to the digest we just resolved so a
			// concurrent rebuild that untags name:latest between scan and
			// recreate doesn't trip ContainerCreate with "No such image".
			targetContainer.SetTargetImageID(newestImage)
		}
		if stale && err == nil {
			stale = applyScanGates(targetContainer, newestImage, params, state)
		}
		containers[i].SetStale(stale)

		if err == nil && willUpdate(targetContainer, params) {
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
				log.WithField(fieldContainer, targetContainer.Name()).Debug("Skipping container with pinned image")
			} else {
				log.WithError(err).WithFields(log.Fields{
					fieldContainer: targetContainer.Name(),
					fieldImage:     targetContainer.ImageName(),
				}).Warn("Unable to update container — proceeding to next")
			}
			containers[i].SetStale(false)
			progress.AddSkipped(targetContainer, err)
		} else {
			progress.AddScanned(targetContainer, newestImage)
		}
		beat(params)
	}
}

// applyScanGates runs the post-staleness gates in order — rollback cooldown,
// rejected init digest, image cooldown — and reports whether the container is
// still due for an update after them.
func applyScanGates(c types.Container, newestImage types.ImageID, params types.UpdateParams, state *State) bool {
	if remaining := state.rollbackCooldownRemaining(c.Name()); remaining > 0 {
		log.WithFields(log.Fields{
			fieldContainer: c.Name(),
			fieldRetryIn:   remaining.Round(time.Second),
		}).Debug("Skipping update: container is on post-rollback cooldown")
		return false
	}
	// --rerun-init-deps rejection gate: a digest that recently failed its
	// init container stays skipped until the registry serves a new one
	// (operator pushed a fix) or the rejection outlives its TTL (a
	// transient dep failure heals itself). We check the resolved
	// newestImage, not the container's currently-running digest, so a
	// pushed fix is detected by digest change at the next poll.
	if params.RerunInitDeps && newestImage != "" {
		if reason, rejected := state.isRejectedInitDigest(newestImage); rejected {
			log.WithFields(log.Fields{
				fieldContainer: c.Name(),
				fieldDigest:    newestImage.ShortID(),
				"failed_dep":   reason.Dep,
				"exit_code":    reason.ExitCode,
				fieldRetryIn:   (rejectedInitDigestTTL - time.Since(reason.At)).Round(time.Second),
			}).Debug("Skipping update: digest recently failed --rerun-init-deps")
			return false
		}
	}
	// Supply-chain gate: hold the update until the new digest has been
	// stable for the configured cooldown window. Runs after the rollback
	// cooldown so a freshly-rolled-back container doesn't also get
	// gated twice. Bypassed under --run-once because "defer to next
	// poll" is meaningless when the daemon exits after this cycle —
	// the operator explicitly asked for an immediate, one-shot update.
	if !params.RunOnce {
		if cooldown := resolveImageCooldown(c, params); cooldown > 0 {
			if proceed, remaining := state.evaluateImageCooldown(c.Name(), newestImage, cooldown); !proceed {
				log.WithFields(log.Fields{
					fieldContainer: c.Name(),
					fieldDigest:    newestImage.ShortID(),
					fieldRetryIn:   remaining.Round(time.Second),
				}).Debug("Skipping update: image cooldown window has not elapsed")
				return false
			}
		}
	}
	return true
}

// initRerunPhase re-executes compose service_completed_successfully siblings
// against the new image *before* each eligible target is recreated. If an
// init container fails, the target's Stale flag is vetoed back to false (the
// pipeline's one sanctioned reversal) and — for genuine non-zero exits — the
// digest is cached as rejected. Runs after scanPhase so each target's
// TargetImageID is already pinned to a resolved digest.
func initRerunPhase(ctx context.Context, client container.Client, containers []types.Container, params types.UpdateParams, state *State, progress *session.Progress) {
	// Dep-lookup view and its compose index, fetched lazily on first need.
	// The scan list (`containers`) is shaped by enable-label/scope/name
	// filters — operators deliberately run migrate/pg-ready one-shots with
	// com.centurylinklabs.watchtower.enable=false so they are neither
	// update candidates nor unmanaged-container noise, which makes them
	// invisible HERE. Resolving deps against the scan list therefore
	// rejected every digest of such a target (dep "not found", exit -1)
	// even though the init container sat Exited(0) on the daemon. Dep
	// DISCOVERY must see the whole daemon: every state, no filter.
	var depLookup []types.Container
	ensureDepLookup := func() []types.Container {
		if depLookup == nil {
			all, listErr := client.ListAllContainers(ctx, filters.NoFilter)
			if listErr != nil {
				log.WithError(listErr).Warn("--rerun-init-deps: listing all containers for dep lookup failed; falling back to the scan-filtered list")
				all = containers
			}
			depLookup = all
		}
		return depLookup
	}
	var index *composeIndex
	ensureIndex := func() *composeIndex {
		if index == nil {
			index = newComposeIndex(ensureDepLookup())
		}
		return index
	}

	for i := range containers {
		// Same eligibility the executor uses — Stale is also set for
		// containers the executor will never touch (monitor-only, global
		// --no-restart) so reports can say "update available", and running
		// their migrations would apply the new schema while deliberately
		// never updating the app.
		if !willUpdate(containers[i], params) {
			continue
		}
		target := containers[i]
		initDeps := target.ComposeInitDependencies()
		if len(initDeps) == 0 {
			// No declared init deps is normal for most services. But if the
			// target is compose-managed and its project still holds one-shot
			// init siblings (migrate/pg-ready), its
			// com.docker.compose.depends_on was emptied out-of-band — most
			// often a `docker compose up --no-deps <svc>` in-place recreate
			// (the .env-only update pattern), which stamps an EMPTY
			// depends_on that is then carried verbatim across every later
			// blue-green cutover. Left alone the rerun would skip the target
			// and the new image would start on the old schema. Detection
			// always warns and drives the stranded gauge; actually
			// EXECUTING the recovered siblings is gated behind
			// --rerun-stranded-init-deps, because running containers the
			// target never declared is a judgment call the labels alone
			// cannot settle (a dep-less service sharing a project with an
			// unrelated one-shot looks identical to a stranded target).
			siblings := ensureIndex().strandedInitSiblings(target)
			if len(siblings) == 0 {
				continue
			}
			warnStrandedInitDeps(target, siblings, params.RerunStrandedInitDeps)
			if !params.RerunStrandedInitDeps {
				continue
			}
			initDeps = ensureIndex().orderInitSiblings(siblings, target.ComposeProject())
		}
		results := initrerun.RunDeps(ctx, client, target, initDeps, ensureDepLookup(), initrerun.DefaultTimeout)
		beat(params)
		if len(results) == 0 {
			continue
		}
		last := results[len(results)-1]
		if last.Succeeded() {
			continue
		}
		containers[i].SetStale(false)
		progress.AddSkipped(target, fmt.Errorf("--rerun-init-deps: init container %q failed (exit %d); old container kept", last.DepName, last.ExitCode))
		if last.ExitCode < 0 {
			// Orchestration failure — the dep's carcass vanished (a routine
			// `docker system prune`), or the daemon refused a stop/create/
			// start. That is no verdict on the digest: caching it would
			// block this target's perfectly good image until a new push,
			// and a permanently-missing dep would re-poison every future
			// digest on first sight. Skip this cycle only; the next poll
			// retries, and the Warn below fires every scan until the
			// operator restores the dep (e.g. `docker compose up -d`).
			log.WithFields(log.Fields{
				fieldContainer: target.Name(),
				"dep":          last.DepName,
				fieldDigest:    target.TargetImageID().ShortID(),
			}).WithError(last.Err).Warn("--rerun-init-deps: init dep could not be run; skipping target update this cycle, retrying next poll")
			continue
		}
		state.rejectInitDigest(target.TargetImageID(), rejectionReason{
			Container: target.Name(),
			Dep:       last.DepName,
			ExitCode:  last.ExitCode,
			At:        time.Now(),
		})
		log.WithFields(log.Fields{
			fieldContainer: target.Name(),
			"dep":          last.DepName,
			"exit_code":    last.ExitCode,
			fieldDigest:    target.TargetImageID().ShortID(),
			fieldRetryIn:   rejectedInitDigestTTL,
		}).Warn("--rerun-init-deps: skipping target update, digest cached as rejected")
	}

	// Per-scan stranded gauge: recompute how many targets are stranded RIGHT
	// NOW across ALL scanned containers, not just the ones stale this cycle.
	// Published as watchtower_stranded_init_deps so the alert tracks live
	// state: it stays firing until the operator re-arms the service and drops
	// to 0 on the very next scan. The _total counter records per-detection
	// history; this gauge is what resolves-on-fix. Reuses the memoized index
	// so a scan makes at most one extra ListAllContainers call regardless of
	// container count.
	metrics.SetStrandedInitDeps(ensureIndex().countStranded(containers))
}

// updatePlan is the run's execution decision, computed exactly once by
// planPhase after the scan and init-rerun phases have finalized every
// container's Stale flag: the dependency-sorted scan view plus the
// strategy-partitioned sets the executor will touch. executePhase consumes it
// verbatim — nothing after planPhase re-decides eligibility.
type updatePlan struct {
	// sorted is the full dependency-sorted scan view; the executor helpers
	// need it to resolve links and shared images.
	sorted []types.Container
	// recreate, rolling and blueGreen partition the executor-eligible
	// containers by their resolved update strategy.
	recreate  []types.Container
	rolling   []types.Container
	blueGreen []types.Container
}

// planPhase sorts the scan view by dependencies, propagates implicit restarts
// to linked containers, records the optimistic updated-marking in the report,
// and partitions the executor's work by strategy.
func planPhase(containers []types.Container, params types.UpdateParams, progress *session.Progress) (*updatePlan, error) {
	sorted, err := sorter.SortByDependencies(containers, params.ComposeDependsOn)
	if err != nil {
		return nil, err
	}

	UpdateImplicitRestart(sorted)

	var containersToUpdate []types.Container
	for _, c := range sorted {
		if !c.IsMonitorOnly(params) {
			containersToUpdate = append(containersToUpdate, c)
			// Optimistically mark only containers the executor will actually
			// touch (failures are downgraded later). A container whose
			// staleness was gated off post-scan (image cooldown, rollback
			// cooldown, rejected init digest) still carries a differing
			// newImage — marking it here made the report, notifications, and
			// metrics claim an update that was deliberately withheld. Left as
			// scanned, it truthfully classifies as stale.
			if c.ToRestart() {
				progress.MarkForUpdate(c.ID())
			}
		}
	}

	recreate, rolling, blueGreen := partitionByStrategy(containersToUpdate, params)
	return &updatePlan{sorted: sorted, recreate: recreate, rolling: rolling, blueGreen: blueGreen}, nil
}

// executePhase carries out the plan: blue-green cutovers first (each
// self-contained), then the rolling-restart set, then the classic
// stop-all-then-start-all recreate set.
func executePhase(ctx context.Context, client container.Client, plan *updatePlan, params types.UpdateParams, state *State, progress *session.Progress) {
	for _, c := range plan.blueGreen {
		if err := performBlueGreenUpdate(ctx, c, plan.sorted, client, params, state); err != nil {
			progress.UpdateFailed(map[types.ContainerID]error{c.ID(): err})
		}
		beat(params)
	}

	if len(plan.rolling) > 0 {
		progress.UpdateFailed(performRollingRestart(ctx, plan.rolling, plan.sorted, client, params, progress, state))
	}

	if len(plan.recreate) > 0 {
		failedStop, stoppedImages := stopContainersInReversedOrder(ctx, plan.recreate, client, params, progress)
		progress.UpdateFailed(failedStop)
		// The restart half must complete even if the run's context is
		// cancelled mid-flight (the watchdog's stage-1 remedy): aborting
		// between the stop and the start phases would strand every
		// already-stopped container. WithoutCancel keeps each call bounded by
		// its per-call timeout; a wedge inside this phase escalates to the
		// watchdog's stage-2 process exit instead.
		progress.UpdateFailed(restartContainersInSortedOrder(context.WithoutCancel(ctx), plan.recreate, plan.sorted, client, params, stoppedImages, state))
	}
}

// beat records one unit of scan/update progress with the process watchdog via
// the optional params callback. See types.UpdateParams.Heartbeat.
func beat(params types.UpdateParams) {
	if params.Heartbeat != nil {
		params.Heartbeat()
	}
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
				log.WithField(fieldContainer, c.Name()).Warn("blue-green is not possible for a container that publishes host ports (two copies cannot bind the same port) — falling back to recreate. Route through a dynamic reverse proxy on the docker network instead of host ports to enable blue-green.")
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
func performBlueGreenUpdate(ctx context.Context, blue types.Container, scanView []types.Container, client container.Client, params types.UpdateParams, state *State) error {
	if params.NoRestart {
		// --no-restart means "update images but never start containers". A
		// blue-green cutover exists only to start green, so there is nothing to
		// do. Mirrors the !params.NoRestart gate in restartStaleContainer; the
		// main loop already filters these out, so this is a defensive guard that
		// keeps the two strategies consistent.
		log.WithField(fieldContainer, blue.Name()).Debug("blue-green: skipping cutover because --no-restart is set")

		return nil
	}

	if params.LifecycleHooks {
		if err := runBlueGreenPreUpdate(ctx, client, blue); err != nil {
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
	greenID, err := client.StartContainer(ctx, blue)
	blue.SetCreateName(canonical) // restore so later log lines use the real name
	if err != nil {
		log.WithError(err).WithFields(log.Fields{fieldContainer: bareName, fieldImage: blue.ImageName()}).Errorf("blue-green: failed to start green container %s (image %s): %v; old container left untouched", bareName, blue.ImageName(), err)

		return err
	}
	log.WithFields(log.Fields{fieldContainer: bareName, fieldGreen: greenName}).Info("blue-green: started green container, waiting for it to become healthy")

	if err := awaitBlueGreenHealthy(ctx, client, blue, greenID, greenName, bareName, params, state); err != nil {
		return err
	}

	if drain := resolveBlueGreenDrain(blue, params); drain > 0 {
		log.WithFields(log.Fields{fieldContainer: bareName, "drain": drain}).Debug("blue-green: draining before retiring the old container")
		time.Sleep(drain)
	}

	if err := client.StopContainer(ctx, blue, params.Timeout); err != nil && !errors.Is(err, container.ErrContainerNotFound) {
		// Green is live and healthy but blue refuses to stop (StopContainer
		// already exhausted its transient-error retries). Without arming the
		// cooldown here the next poll re-runs the entire cutover — starting yet
		// another green — and orphan greens pile up. Back this container off like
		// a failed rollback so the poll loop skips it for rollbackCooldown; the
		// startup CleanupOrphanBlueGreen sweep reconciles the leftover green.
		// This is a promotion abort, NOT a rollback: the new image is already
		// live via green, so it increments watchtower_promotion_aborts_total
		// (not watchtower_rollbacks_total). recordRollback only arms the shared
		// per-container cooldown — the name is historical, not the metric.
		state.recordRollback(blue.Name())
		metrics.RegisterPromotionAbort()
		log.WithError(err).WithFields(log.Fields{fieldContainer: bareName, fieldImage: blue.ImageName(), fieldGreen: greenName}).Errorf("blue-green: failed to stop the old container %s (image %s): %v; green is live but the old one remains and green keeps its temporary name — backing off for %s", bareName, blue.ImageName(), err, rollbackCooldown)

		return err
	}

	if greenSnap, gerr := client.GetContainer(ctx, greenID); gerr == nil {
		if rerr := client.RenameContainer(ctx, greenSnap, bareName); rerr != nil {
			log.WithError(rerr).WithFields(log.Fields{"from": greenName, "to": bareName, fieldImage: blue.ImageName()}).Warnf("blue-green: failed to rename green to the canonical name %s (image %s): %v; it keeps its temporary name until the next update", bareName, blue.ImageName(), rerr)
		}
	}

	if params.LifecycleHooks {
		lifecycle.ExecutePostUpdateCommand(ctx, client, greenID)
	}

	if params.Cleanup {
		if prior := state.rotatePreviousImage(blue.Name(), blue.SourceImageID()); prior != "" {
			cleanupImages(ctx, client, map[types.ImageID]bool{prior: true}, scanView)
		}
	}

	log.WithFields(log.Fields{fieldContainer: bareName, fieldImage: blue.ImageName()}).Info("blue-green: cutover complete")

	return nil
}

// runBlueGreenPreUpdate runs the pre-update lifecycle command before a
// blue-green cutover. It returns a non-nil error to abort the update when the
// command failed or signaled EX_TEMPFAIL (exit 75).
func runBlueGreenPreUpdate(ctx context.Context, client container.Client, blue types.Container) error {
	skipUpdate, err := lifecycle.ExecutePreUpdateCommand(ctx, client, blue)
	if err != nil {
		log.WithError(err).WithFields(log.Fields{fieldContainer: blue.Name(), fieldImage: blue.ImageName()}).Warn("Skipping blue-green update: pre-update lifecycle command failed")

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
func awaitBlueGreenHealthy(ctx context.Context, client container.Client, blue types.Container, greenID types.ContainerID, greenName, bareName string, params types.UpdateParams, state *State) error {
	timeout := resolveHealthCheckTimeout(blue, params)
	werr := waitForHealthy(ctx, client, greenID, timeout)
	switch {
	case errors.Is(werr, errNoHealthcheck):
		log.WithField(fieldContainer, bareName).Warn("blue-green: green container has no HEALTHCHECK — readiness cannot be verified; relying on the drain window only. Add a HEALTHCHECK for a real readiness gate.")

		return nil
	case werr == nil:
		return nil
	}

	fields := log.Fields{fieldContainer: bareName, fieldImage: blue.ImageName(), fieldGreen: greenName}
	maps.Copy(fields, healthFailureContext(ctx, client, greenID))
	log.WithError(werr).WithFields(fields).Errorf("blue-green: green container %s (image %s) failed health check (%v) — removing green and keeping the old container", bareName, blue.ImageName(), werr)
	if greenSnap, gerr := client.GetContainer(ctx, greenID); gerr == nil {
		if serr := client.StopContainer(ctx, greenSnap, params.Timeout); serr != nil && !errors.Is(serr, container.ErrContainerNotFound) {
			log.WithError(serr).WithFields(log.Fields{fieldGreen: greenName, fieldImage: blue.ImageName()}).Errorf("blue-green: failed to remove the failed green container %s (image %s): %v", greenName, blue.ImageName(), serr)
		}
	}
	state.recordRollback(blue.Name())
	metrics.RegisterRollback()

	return fmt.Errorf("blue-green update of %s aborted after failed health check: %w", blue.Name(), werr)
}

func performRollingRestart(ctx context.Context, containers, scanView []types.Container, client container.Client, params types.UpdateParams, progress *session.Progress, state *State) map[types.ContainerID]error {
	cleanupImageIDs := make(map[types.ImageID]bool, len(containers))
	failed := make(map[types.ContainerID]error, len(containers))

	for i := len(containers) - 1; i >= 0; i-- {
		beat(params)
		if containers[i].ToRestart() {
			err := stopStaleContainer(ctx, containers[i], client, params)
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
				if err := restartStaleContainer(ctx, containers[i], client, params, state); err != nil {
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
					if prior := state.rotatePreviousImage(containers[i].Name(), containers[i].SourceImageID()); prior != "" {
						cleanupImageIDs[prior] = true
					}
				}
			}
		}
	}

	if params.Cleanup {
		cleanupImages(ctx, client, cleanupImageIDs, scanView)
	}
	return failed
}

func stopContainersInReversedOrder(ctx context.Context, containers []types.Container, client container.Client, params types.UpdateParams, progress *session.Progress) (failed map[types.ContainerID]error, stopped map[types.ImageID]bool) {
	failed = make(map[types.ContainerID]error, len(containers))
	stopped = make(map[types.ImageID]bool, len(containers))
	for i := len(containers) - 1; i >= 0; i-- {
		beat(params)
		err := stopStaleContainer(ctx, containers[i], client, params)
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

func stopStaleContainer(ctx context.Context, cont types.Container, client container.Client, params types.UpdateParams) error {
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
		skipUpdate, err := lifecycle.ExecutePreUpdateCommand(ctx, client, cont)
		if err != nil {
			// Pre-update command is a user-defined hook running inside the
			// container; a failure reflects on the hook script, not on
			// watchtower's orchestration. Warn is the right level — strict
			// NOTIFICATIONS_LEVEL=error shouldn't fire for user-script flakes.
			log.WithError(err).WithFields(log.Fields{
				fieldContainer: cont.Name(),
				fieldImage:     cont.ImageName(),
			}).Warn("Skipping container: pre-update lifecycle command failed")
			return err
		}
		if skipUpdate {
			log.Debug("Skipping container as the pre-update command returned exit code 75 (EX_TEMPFAIL)")
			return errors.New("skipping container as the pre-update command returned exit code 75 (EX_TEMPFAIL)")
		}
	}

	if err := client.StopContainer(ctx, cont, params.Timeout); err != nil {
		if errors.Is(err, container.ErrContainerNotFound) {
			log.WithField(fieldContainer, cont.Name()).Debug("Container vanished before stop — skipping")
			return err
		}
		log.WithError(err).WithFields(log.Fields{
			fieldContainer: cont.Name(),
			fieldImage:     cont.ImageName(),
		}).Errorf("Failed to stop container %s (image %s): %v", cont.Name(), cont.ImageName(), err)
		return err
	}
	return nil
}

func restartContainersInSortedOrder(ctx context.Context, containers, scanView []types.Container, client container.Client, params types.UpdateParams, stoppedImages map[types.ImageID]bool, state *State) map[types.ContainerID]error {
	cleanupImageIDs := make(map[types.ImageID]bool, len(containers))
	failed := make(map[types.ContainerID]error, len(containers))

	for _, c := range containers {
		beat(params)
		if !c.ToRestart() {
			continue
		}
		if stoppedImages[c.SafeImageID()] {
			if err := restartStaleContainer(ctx, c, client, params, state); err != nil {
				failed[c.ID()] = err
			} else if c.IsStale() {
				// Defer cleanup by one generation — see performRollingRestart
				// for the rationale. SourceImageID is still the right key.
				if prior := state.rotatePreviousImage(c.Name(), c.SourceImageID()); prior != "" {
					cleanupImageIDs[prior] = true
				}
			}
		}
	}

	if params.Cleanup {
		cleanupImages(ctx, client, cleanupImageIDs, scanView)
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
func cleanupImages(ctx context.Context, client container.Client, imageIDs map[types.ImageID]bool, scanView []types.Container) {
	for imageID := range imageIDs {
		if imageID == "" {
			continue
		}
		if isImageStillReferenced(scanView, imageID) {
			log.WithField(fieldImage, imageID.ShortID()).Debug("Image still referenced by an active container — deferring cleanup")
			continue
		}
		if err := client.RemoveImageByID(ctx, imageID); err != nil {
			log.WithError(err).WithField(fieldImage, imageID.ShortID()).Error("Failed to remove image")
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

// selfCanonicalName recovers the operator-chosen ("canonical") bare name of the
// running watchtower self for a self-update recreate, in descending order of
// trust:
//
//  1. the name embedded in a "<canonical>-wt-self-XXXXXXXX" temp left by a
//     prior interrupted self-update — recoverable with zero compose/hostname
//     dependence, which is the whole point of the structured temp name. Checked
//     first so a drifted cached name is never mistaken for an operator name.
//  2. the current name, when it is not a bare util.RandName() — i.e. the real
//     name in every healthy deployment shape: plain `docker run --name`,
//     default-compose "<project>-<service>-N", or a container_name override.
//  3. the compose service label, but ONLY as a legacy bridge when the cached
//     name is a bare RandName from a pre-structured-name self-update. Imperfect
//     for default-compose names (it drops the project/ordinal), but it matches
//     the previous behavior, so it is no regression.
//  4. the bare current name as a terminal fallback — a legacy random-named,
//     non-compose self cannot be auto-healed, exactly as before this change.
func selfCanonicalName(c types.Container) string {
	bare := strings.TrimPrefix(c.Name(), "/")
	if m := selfTempNameRe.FindStringSubmatch(bare); m != nil {
		return m[1]
	}
	if !util.IsRandName(bare) {
		return bare
	}
	if service := c.ComposeService(); service != "" {
		return service
	}

	return bare
}

// selfTempName builds the temporary name the outgoing self wears during a
// self-update recreate: "<canonical>-wt-self-XXXXXXXX". It is the deliberate
// twin of greenName in performBlueGreenUpdate, and MUST be built from the
// already-recovered bare canonical (selfCanonicalName) — never a raw cached
// name — so a prior "-wt-self-XXXXXXXX" suffix is never re-embedded and the
// name cannot grow on each cycle.
func selfTempName(canonical string) string {
	return canonical + "-wt-self-" + util.RandName()[:8]
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
func restartStaleContainer(ctx context.Context, container types.Container, client container.Client, params types.UpdateParams, state *State) error {
	// Orphan watchtower-labeled containers (random-named leftovers from a
	// previous self-update whose CheckForMultipleWatchtowerInstances pass did
	// not complete) reach this point already stopped+removed by
	// stopStaleContainer. Re-creating them would just resurrect the orphan
	// under its random name, so skip the recreate entirely. The actual
	// running self still goes through the rename-respawn branch below.
	if container.IsWatchtower() && !isRunningSelf(container, params) {
		log.WithField(fieldContainer, container.Name()).Info(
			"Skipping respawn of orphan watchtower-labeled container — it was stopped+removed; recreating would revive the orphan with its random name.",
		)
		return nil
	}

	// Since we can't shutdown a watchtower container immediately, we need to
	// start the new one while the old one is still running. This prevents us
	// from re-using the same container name so we first rename the current
	// instance so that the new one can adopt the old name.
	wasRunningSelf := isRunningSelf(container, params)

	// Recover the operator-chosen canonical name (compose-, hostname-, and
	// random-name-independent — see selfCanonicalName) and pin it as the create
	// name so the respawned self always reclaims it even when the cached name
	// has already drifted to a previous-cycle "-wt-self-" temp or a legacy
	// random name. selfCanonical (bare) is reused below to build the temp name
	// and to restore the canonical name should the respawn fail.
	var selfCanonical string
	if wasRunningSelf {
		selfCanonical = selfCanonicalName(container)
		container.SetCreateName("/" + selfCanonical)
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
			log.WithField(fieldContainer, originalName).Warn(
				"Skipping self-update: watchtower has published host port bindings that would conflict with the rename-and-respawn pattern during the old/new overlap window. Update manually by stopping and recreating this container with the new image.",
			)
			return nil
		}
		// --no-restart updates images but never (re)starts containers. The
		// self-rename is only the first half of rename-and-respawn; with no
		// respawn to follow (the StartContainer block below is gated on
		// !NoRestart) renaming the live self to a temp name would strand it
		// under "<canonical>-wt-self-XXXXXXXX" with no replacement. Skip the
		// whole dance — the live self keeps its canonical name. Mirrors the
		// NoRestart short-circuit in performBlueGreenUpdate.
		if params.NoRestart {
			log.WithField(fieldContainer, originalName).Debug("Self-update: skipping rename-and-respawn because --no-restart is set")

			return nil
		}
		// Break the os.Hostname()-drift chain that otherwise propagates the
		// founding container's short ID into every subsequent self-update
		// and silently degrades DetectSelfContainerID + the startup-time
		// CheckForMultipleWatchtowerInstances to label-only matching. With
		// Hostname cleared on this recreate, the new container's hostname
		// equals its own short ID and self-detection stays accurate.
		container.SetClearHostnameOnRecreate(true)
		// Rename the outgoing self to a STRUCTURED temp name that embeds the
		// canonical name, so the name survives docker rename in a recoverable
		// form: selfCanonicalName re-derives it on the next poll and the
		// startup CleanupOrphanSelf sweep promotes it. Built from the recovered
		// bare canonical, so a prior "-wt-self-" suffix is never re-embedded.
		if err := client.RenameContainer(ctx, container, selfTempName(selfCanonical)); err != nil {
			log.WithError(err).WithFields(log.Fields{
				fieldContainer: originalName,
				fieldImage:     container.ImageName(),
			}).Errorf("Failed to rename container %s (image %s): %v", originalName, container.ImageName(), err)

			return nil
		}
	}

	if !params.NoRestart {
		newContainerID, err := client.StartContainer(ctx, container)
		if err != nil {
			if wasRunningSelf {
				// The destructive rename already moved the live self to its temp
				// name, but the respawn failed — restore the canonical name so
				// the still-running self keeps serving as "<canonical>" instead
				// of being stranded as "<canonical>-wt-self-XXXXXXXX". Best-
				// effort: on failure selfCanonicalName (next poll) and the
				// startup CleanupOrphanSelf sweep still reconcile it.
				if rerr := client.RenameContainer(ctx, container, selfCanonical); rerr != nil {
					log.WithError(rerr).WithField(fieldContainer, originalName).Warn(
						"Self-update: failed to restore the canonical name after a failed respawn; it will be reconciled on the next poll or startup sweep",
					)
				}
			}
			entry := log.WithError(err).WithFields(log.Fields{
				fieldContainer: originalName,
				fieldImage:     container.ImageName(),
			})
			// A self-update that keeps failing re-enters this branch every poll
			// (default 60s) with the same error, and each Error becomes a
			// notification via the logrus hook. Dedup the self path so only the
			// first occurrence — and any genuinely different failure — notifies;
			// identical repeats inside the cooldown drop to Debug. Non-self
			// failures are unaffected and always log at Error.
			if wasRunningSelf && !state.shouldNotifySelfStartFailure(originalName, err) {
				entry.Debugf("Failed to start container %s (image %s) again; suppressing repeat notification: %v", originalName, container.ImageName(), err)
				return err
			}
			entry.Errorf("Failed to start container %s (image %s): %v", originalName, container.ImageName(), err)
			return err
		}
		if container.ToRestart() && params.LifecycleHooks {
			lifecycle.ExecutePostUpdateCommand(ctx, client, newContainerID)
		}
		if params.HealthCheckGated {
			if err := gateOnHealthCheck(ctx, client, container, newContainerID, params, state); err != nil {
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
func gateOnHealthCheck(ctx context.Context, client container.Client, old types.Container, newID types.ContainerID, params types.UpdateParams, state *State) error {
	timeout := resolveHealthCheckTimeout(old, params)
	err := waitForHealthy(ctx, client, newID, timeout)
	if errors.Is(err, errNoHealthcheck) {
		log.WithField(fieldContainer, old.Name()).Warn(
			"--health-check-gated is set but the container has no HEALTHCHECK — update proceeded without gating. Add a HEALTHCHECK or remove the flag to silence this warning.",
		)
		return nil
	}
	if err == nil {
		log.WithField(fieldContainer, old.Name()).Debug("Container reported healthy; update accepted")
		return nil
	}

	fields := log.Fields{
		fieldContainer: old.Name(),
		fieldImage:     old.ImageName(),
		"new_digest":   old.TargetImageID().ShortID(),
		fieldOldDigest: old.ImageID().ShortID(),
	}
	// Inspect the new container *before* rollback tears it down to capture
	// why the gate failed — OOM-kill, non-zero exit, or the trimmed output of
	// the last failing probe. Without this an operator only sees "rolling
	// back" and has to reproduce locally to find the cause.
	maps.Copy(fields, healthFailureContext(ctx, client, newID))
	log.WithError(err).WithFields(fields).Errorf("Health check failed after updating %s (image %s): %v — rolling back to the previous image", old.Name(), old.ImageName(), err)
	if rbErr := rollback(ctx, client, old, newID, params, state); rbErr != nil {
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
func healthFailureContext(ctx context.Context, client container.Client, id types.ContainerID) log.Fields {
	fields := log.Fields{}
	c, err := client.GetContainer(ctx, id)
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
func waitForHealthy(ctx context.Context, client container.Client, id types.ContainerID, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = defaultHealthCheckTimeout
	}
	deadline := time.Now().Add(timeout)
	for {
		c, err := client.GetContainer(ctx, id)
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
		case dockercontainer.Starting, dockercontainer.NoHealthcheck:
			// Not a terminal verdict yet — keep polling until Healthy/Unhealthy
			// or the deadline below fires.
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
func rollback(ctx context.Context, client container.Client, old types.Container, newID types.ContainerID, params types.UpdateParams, state *State) error {
	defer state.recordRollback(old.Name())

	newSnapshot, err := client.GetContainer(ctx, newID)
	if err != nil {
		log.WithError(err).WithFields(log.Fields{
			fieldContainer: old.Name(),
			fieldImage:     old.ImageName(),
			"new_id":       newID.ShortID(),
		}).Warn("rollback: could not inspect new container, attempting restart of old anyway")
	} else if stopErr := client.StopContainer(ctx, newSnapshot, params.Timeout); stopErr != nil {
		log.WithError(stopErr).WithFields(log.Fields{
			fieldContainer: newSnapshot.Name(),
			fieldImage:     newSnapshot.ImageName(),
		}).Warn("rollback: failed to stop unhealthy new container")
	}

	// Repoint the recreate at the old image's digest. Without this, the
	// targetImageID set on the forward path would still hold the *new* image
	// and rollback would re-create with the broken version we just rejected.
	old.SetTargetImageID(old.ImageID())
	rolledBackID, err := client.StartContainer(ctx, old)
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
	if err := waitForHealthy(ctx, client, rolledBackID, rollbackTimeout); err != nil {
		if errors.Is(err, errNoHealthcheck) {
			log.WithFields(log.Fields{
				fieldContainer: old.Name(),
				fieldImage:     old.ImageName(),
				fieldOldDigest: old.ImageID().ShortID(),
				fieldRollback:  true,
				"rollback_ok":  true,
			}).Warn("Rollback complete — previous image restored (no HEALTHCHECK to verify)")
			return nil
		}
		log.WithError(err).WithFields(log.Fields{
			fieldContainer:    old.Name(),
			fieldImage:        old.ImageName(),
			fieldOldDigest:    old.ImageID().ShortID(),
			fieldRollback:     true,
			"rollback_failed": true,
		}).Error("Rollback restored the previous image but it is also unhealthy — manual intervention required")
		return fmt.Errorf("rolled-back container %s is also unhealthy: %w", old.Name(), err)
	}

	log.WithFields(log.Fields{
		fieldContainer: old.Name(),
		fieldImage:     old.ImageName(),
		fieldOldDigest: old.ImageID().ShortID(),
		fieldRollback:  true,
		"rollback_ok":  true,
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
