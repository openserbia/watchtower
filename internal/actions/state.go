package actions

import (
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/openserbia/watchtower/pkg/types"
)

// State is the single owner of every piece of cross-run, daemon-lifetime
// update state. One instance is constructed in cmd.Run and threaded through
// Update / AuditUnmanaged; tests construct their own per spec, which is what
// replaced the historical package-level maps and their Reset…ForTest helpers
// (and what allows specs to run in parallel).
//
// Everything here is deliberately in-memory: a process restart clears it,
// and a restart is an operator touch-point that should re-arm one retry of
// anything suppressed (a rejected init digest, a rollback cooldown, a
// deduplicated warning). Since the liveness watchdog cancels wedged runs
// instead of exiting, restarts are rare enough for that contract to hold
// again. If persistence ever becomes necessary, this type is the one place
// to add it.
type State struct {
	mu sync.Mutex

	// failedRollbacks tracks the last rollback time per container name, so
	// the poll → stop → start → fail → rollback loop doesn't thrash every
	// poll when an image author has pushed a broken version.
	failedRollbacks map[string]time.Time

	// pendingImages tracks digests first seen during an active
	// --image-cooldown window but not yet applied. Keyed by container name.
	// When the digest matches the entry and the cooldown has elapsed, the
	// update proceeds; a different digest mid-window (author re-pushed)
	// resets the clock.
	pendingImages map[string]pendingImage

	// previousImages keeps the digest a container was running on *before*
	// its last successful update, keyed by container name. The next
	// successful update hands the recorded digest to --cleanup and rotates
	// this slot to the digest just retired, so each managed container keeps
	// exactly one prior generation on disk as a rollback target. Restart
	// under-cleans by one generation — a deliberate trade for zero
	// persistent state.
	previousImages map[string]types.ImageID

	// rejectedInitDigests holds image digests whose --rerun-init-deps init
	// container exited non-zero. Keyed by digest; entries expire after
	// rejectedInitDigestTTL so a transient dep failure self-heals without a
	// new push or a restart. Orchestration failures (exit -1) are never
	// cached: they say nothing about the digest.
	rejectedInitDigests map[types.ImageID]rejectionReason

	// selfStartFailures tracks the last time we *notified* about a
	// self-update start failure, keyed by container name + error signature,
	// so a wedged self-update doesn't turn the logrus notification hook into
	// a per-poll storm. Identical repeats inside selfStartFailCooldown log
	// at Debug instead.
	selfStartFailures map[string]time.Time

	// knownUnmanaged tracks which unlabeled containers the audit has already
	// warned about, so the warning fires on set changes rather than every
	// poll.
	knownUnmanaged map[string]struct{}
}

// NewState returns an empty State. Construct exactly one for the daemon (in
// cmd.Run) and a fresh one per test spec.
func NewState() *State {
	return &State{
		failedRollbacks:     make(map[string]time.Time),
		pendingImages:       make(map[string]pendingImage),
		previousImages:      make(map[string]types.ImageID),
		rejectedInitDigests: make(map[types.ImageID]rejectionReason),
		selfStartFailures:   make(map[string]time.Time),
		knownUnmanaged:      make(map[string]struct{}),
	}
}

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

// selfStartFailCooldown is the window during which an identical self-update
// start failure is suppressed (logged at Debug) after it has already notified
// once. A var so tests can shrink it.
var selfStartFailCooldown = 1 * time.Hour

// rejectedInitDigestTTL bounds how long a non-zero init-container exit keeps
// its digest rejected before the rerun is retried. Mirrors rollbackCooldown's
// thrash-prevention role: hourly retries of a genuinely broken migration are
// bounded work (operators using --rerun-init-deps run idempotent migrations by
// contract), while a transiently-failing dep unblocks itself within the hour.
var rejectedInitDigestTTL = 1 * time.Hour

// rollbackCooldownRemaining reports how long the container should continue to
// be skipped for being on post-rollback cooldown. Zero = not on cooldown.
func (s *State) rollbackCooldownRemaining(containerName string) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	last, ok := s.failedRollbacks[containerName]
	if !ok {
		return 0
	}
	remaining := rollbackCooldown - time.Since(last)
	if remaining <= 0 {
		delete(s.failedRollbacks, containerName)
		return 0
	}
	return remaining
}

func (s *State) recordRollback(containerName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failedRollbacks[containerName] = time.Now()
}

// isRejectedInitDigest reports whether the given digest recently failed
// --rerun-init-deps. Used to short-circuit stale-checks without flooding logs
// every poll. Entries past rejectedInitDigestTTL are dropped lazily here,
// arming a retry.
func (s *State) isRejectedInitDigest(id types.ImageID) (rejectionReason, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rejectedInitDigests[id]
	if !ok {
		return r, false
	}
	if time.Since(r.At) >= rejectedInitDigestTTL {
		delete(s.rejectedInitDigests, id)
		return rejectionReason{}, false
	}
	return r, true
}

func (s *State) rejectInitDigest(id types.ImageID, reason rejectionReason) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rejectedInitDigests[id] = reason
}

// shouldNotifySelfStartFailure reports whether a self-update start failure for
// the given container name and error should be logged at Error (which the
// logrus notification hook turns into a notification) or suppressed to Debug.
// It returns true — notify — on the first sighting of a (name, error-signature)
// pair and again once selfStartFailCooldown has elapsed, recording the
// notification time when it does. A genuinely different failure has a different
// signature and so always notifies.
func (s *State) shouldNotifySelfStartFailure(containerName string, err error) bool {
	key := containerName + "\x00" + selfStartFailSignature(err)
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	if last, seen := s.selfStartFailures[key]; seen && now.Sub(last) < selfStartFailCooldown {
		return false
	}
	s.selfStartFailures[key] = now
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

// evaluateImageCooldown is the single decision point for "should this digest
// apply now?" under an active cooldown. Returns:
//   - proceed=true when the cooldown has elapsed (and clears the pending entry)
//   - proceed=false otherwise, with remaining giving the operator a useful
//     "retry in N" number for the log line
//
// Resets the pending entry when the registry serves a different digest —
// that's the signal the image author pushed a follow-up, so the clock should
// restart with the new digest.
func (s *State) evaluateImageCooldown(containerName string, newest types.ImageID, cooldown time.Duration) (proceed bool, remaining time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, tracked := s.pendingImages[containerName]
	now := time.Now()

	switch {
	case !tracked, entry.digest != newest:
		// First sighting of this digest (either no prior record, or the
		// digest changed mid-cooldown). Record and defer.
		s.pendingImages[containerName] = pendingImage{digest: newest, firstSeen: now}
		return false, cooldown
	case now.Sub(entry.firstSeen) >= cooldown:
		// Stable long enough — apply and clear state.
		delete(s.pendingImages, containerName)
		return true, 0
	default:
		return false, cooldown - now.Sub(entry.firstSeen)
	}
}

// pendingImageCount reports the number of containers currently inside an
// active cooldown window. Used for watchtower_containers_in_cooldown.
func (s *State) pendingImageCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pendingImages)
}

// rotatePreviousImage records the just-retired digest for a container and
// returns whatever digest was previously held in that slot. The returned ID
// is the one safe to hand to --cleanup; the freshly-stored ID stays on disk
// as the rollback target until the next update of this container rotates it
// out. An empty stored or rotated value is a normal first-pass condition,
// not an error.
func (s *State) rotatePreviousImage(containerName string, justRetired types.ImageID) types.ImageID {
	s.mu.Lock()
	defer s.mu.Unlock()
	prior := s.previousImages[containerName]
	if justRetired == "" {
		// Don't blow away a known prior with a sentinel. Leaves the map slot
		// alone so the next real rotation still finds the recorded digest.
		return prior
	}
	s.previousImages[containerName] = justRetired
	return prior
}

// noteUnmanaged logs change-detected audit output for the current unmanaged
// set — a warning the first time each unlabeled container is seen, an info
// when a previously-unmanaged one gets labeled or removed — and records the
// set for the next poll. Steady state emits nothing.
func (s *State) noteUnmanaged(unmanagedNow map[string]types.Container) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for name, c := range unmanagedNow {
		if _, seen := s.knownUnmanaged[name]; seen {
			continue
		}
		log.WithFields(log.Fields{
			fieldContainer: name,
			fieldImage:     c.ImageName(),
		}).Warn("Container has no com.centurylinklabs.watchtower.enable label — silently skipped under --label-enable. Set the label to true or false to make the intent explicit.")
	}

	for name := range s.knownUnmanaged {
		if _, still := unmanagedNow[name]; still {
			continue
		}
		log.WithField(fieldContainer, name).Info("Previously-unmanaged container is now labeled or removed — audit cleared")
	}

	s.knownUnmanaged = make(map[string]struct{}, len(unmanagedNow))
	for name := range unmanagedNow {
		s.knownUnmanaged[name] = struct{}{}
	}
}

// EvaluateImageCooldownForTest exposes the cooldown decision function for
// external tests. Not for production use.
func (s *State) EvaluateImageCooldownForTest(containerName string, newest types.ImageID, cooldown time.Duration) (bool, time.Duration) {
	return s.evaluateImageCooldown(containerName, newest, cooldown)
}

// SeedPreviousImageForTest pre-records a prior digest for a container so a
// test's first Update behaves as if it were the second (cleanup of the
// previous-previous fires immediately). External test-only helper.
func (s *State) SeedPreviousImageForTest(containerName string, id types.ImageID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.previousImages[containerName] = id
}

// PreviousImageForTest exposes the recorded prior digest for a container,
// for assertions in external tests.
func (s *State) PreviousImageForTest(containerName string) types.ImageID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.previousImages[containerName]
}

// RewindCooldownFirstSeenForTest shifts a container's recorded firstSeen
// backwards by the given amount, simulating the passage of wall-clock time
// without making tests sleep.
func (s *State) RewindCooldownFirstSeenForTest(containerName string, by time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry, ok := s.pendingImages[containerName]; ok {
		entry.firstSeen = entry.firstSeen.Add(-by)
		s.pendingImages[containerName] = entry
	}
}

// RewindRejectedInitDigestForTest shifts a rejected digest's recorded time
// backwards by the given amount, simulating TTL expiry without making tests
// sleep. Parallel to RewindCooldownFirstSeenForTest.
func (s *State) RewindRejectedInitDigestForTest(id types.ImageID, by time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.rejectedInitDigests[id]; ok {
		r.At = r.At.Add(-by)
		s.rejectedInitDigests[id] = r
	}
}
