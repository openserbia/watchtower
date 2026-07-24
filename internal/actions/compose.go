package actions

import (
	"sort"
	"strings"

	log "github.com/sirupsen/logrus"

	"github.com/openserbia/watchtower/pkg/metrics"
	"github.com/openserbia/watchtower/pkg/types"
)

// composeIndex is the per-scan Compose-project model: the unfiltered daemon
// container list grouped by com.docker.compose.project exactly once, so the
// compose heuristics (stranded detection, sibling ordering) query a prebuilt
// structure instead of each re-scanning a flat []Container. Build it from the
// same every-state, no-filter list the init-dep lookup uses — the heuristics
// must see one-shot carcasses the scan filter hides.
type composeIndex struct {
	byProject map[string][]types.Container
}

// newComposeIndex groups the given containers by compose project. Containers
// without a project label are irrelevant to every compose heuristic and are
// dropped.
func newComposeIndex(all []types.Container) *composeIndex {
	ix := &composeIndex{byProject: make(map[string][]types.Container)}
	for _, c := range all {
		if project := c.ComposeProject(); project != "" {
			ix.byProject[project] = append(ix.byProject[project], c)
		}
	}
	return ix
}

// strandedInitSiblings returns the one-shot init siblings (migrate, pg-ready,
// bootstrap) that `target` should gate on but no longer declares. It is
// non-empty exactly when target is a stranded-init-deps case: compose-managed,
// a PRESENT but EMPTY com.docker.compose.depends_on, no no-init-deps opt-out,
// yet its project still holds a one-shot init sibling. The usual cause is a
// `docker compose up --no-deps <svc>` in-place recreate (the .env-only update
// pattern), which stamps an EMPTY depends_on because --no-deps tells Compose to
// disregard the dependency graph. Blue-green is NOT the cause: green faithfully
// inherits blue's labels (GetCreateConfig copies them verbatim), so it only
// propagates an already-empty value — a peer whose label was populated at its
// last full `compose up` keeps it across every cutover. Once empty,
// --rerun-init-deps would skip the target and the new image would run against
// the old schema; the recovery path re-runs these siblings.
//
// Single source of truth for the stranded predicate: the per-target warn/rerun
// (warnStrandedInitDeps) and the per-scan gauge (watchtower_stranded_init_deps).
// Re-arming via `docker compose up -d --force-recreate <service>` rewrites the
// label, so the raw value becomes non-empty and this returns nil — which is
// what lets the gauge drop to 0 and the alert resolve on remediation.
func (ix *composeIndex) strandedInitSiblings(target types.Container) []string {
	// Opt-out: a target that legitimately has no init deps — e.g. a frontend
	// sharing a Compose project with migrate/pg-ready one-shots owned by a
	// sibling API tier — sets com.centurylinklabs.watchtower.no-init-deps=true
	// to affirm the empty depends_on is by design, not a dropped-label signal.
	// A backend that genuinely lost its label won't carry the opt-out.
	if target.HasNoInitDepsLabel() {
		return nil
	}

	// A one-shot init container is never itself a stranded parent — it is the
	// gate, not the gated service. Skip it so the per-scan gauge doesn't
	// double-count sibling one-shots that see each other (migrate <-> pg-ready)
	// when WATCHTOWER_INCLUDE_STOPPED surfaces their exited carcasses in the
	// scan list. Without this, a project with two one-shots reads 2 even when
	// nothing is stranded — a false alert.
	if isOneShotInitSibling(target) {
		return nil
	}

	// The dropped-label signal is the raw label being PRESENT and EMPTY —
	// exactly the stamp `docker compose up --no-deps` leaves. Anything else is
	// not stranded:
	//   - absent: the container never went through a label-stamping compose
	//     create; there is no dropped contract to recover.
	//   - non-empty: the declared contract is intact (or already re-armed) —
	//     this is the check that makes the gauge resolve-on-fix. Crucially
	//     that includes labels holding only service_started / service_healthy
	//     entries: such targets legitimately declare no *init* deps, and
	//     treating their intact label as "emptied" executed unrelated
	//     one-shots against targets that never gated on them.
	raw, present := target.ComposeDependsOnRaw()
	if !present || strings.TrimSpace(raw) != "" {
		return nil
	}

	project := target.ComposeProject()
	if project == "" {
		return nil
	}

	var siblings []string
	for _, c := range ix.byProject[project] {
		if c.ComposeService() == target.ComposeService() {
			continue
		}
		if isOneShotInitSibling(c) {
			siblings = append(siblings, c.ComposeService())
		}
	}
	return siblings
}

// countStranded reports how many of the given scan containers are stranded
// RIGHT NOW. Feeds the watchtower_stranded_init_deps gauge, recomputed every
// scan so the alert resolves on the first scan after the operator re-arms.
func (ix *composeIndex) countStranded(containers []types.Container) int {
	stranded := 0
	for i := range containers {
		if len(ix.strandedInitSiblings(containers[i])) > 0 {
			stranded++
		}
	}
	return stranded
}

// orderInitSiblings best-effort orders recovered one-shot siblings so each runs
// after any sibling it itself gates on. The declared depends_on order is lost
// with the emptied label, so we approximate from each sibling's OWN init-dep
// count: a leaf (e.g. pg-ready, 0 deps) sorts before a migrate that declares it,
// with the service name as a stable tiebreak. One-shot siblings are created by
// the full `compose up` (not the --no-deps recreate that stranded the target),
// so their own labels are usually intact and this reproduces the declared order;
// re-arming the target restores it exactly. Deterministic in all cases.
func (ix *composeIndex) orderInitSiblings(siblings []string, project string) []string {
	ownDepCount := func(svc string) int {
		for _, c := range ix.byProject[project] {
			if c.ComposeService() == svc {
				return len(c.ComposeInitDependencies())
			}
		}
		return 0
	}
	ordered := append([]string(nil), siblings...)
	sort.SliceStable(ordered, func(a, b int) bool {
		if ca, cb := ownDepCount(ordered[a]), ownDepCount(ordered[b]); ca != cb {
			return ca < cb
		}
		return ordered[a] < ordered[b]
	})
	return ordered
}

// isOneShotInitSibling reports whether c looks like a Compose one-shot init
// container (migrate / pg-ready / bootstrap): compose-managed with a "no"
// restart policy. Long-lived services carry unless-stopped/always/on-failure,
// so the restart policy discriminates init gates from a project's main
// service(s) without hardcoding service names.
func isOneShotInitSibling(c types.Container) bool {
	if c.ComposeService() == "" {
		return false
	}

	info := c.ContainerInfo()
	if info == nil || info.HostConfig == nil {
		return false
	}

	switch info.HostConfig.RestartPolicy.Name {
	case "", "no":
		return true
	default:
		return false
	}
}

// warnStrandedInitDeps records the stranded-init-deps history counter and logs
// the actionable line for a stale target whose com.docker.compose.depends_on was
// emptied out-of-band (detection in strandedInitSiblings; siblings is its
// non-empty result). With --rerun-stranded-init-deps the siblings are re-run as
// a safety net (recovering=true selects that wording); without it the warning
// says plainly that migrations will NOT run. Either way the empty label needs
// re-arming to restore the declared contract (and exact ordering), which is why
// the companion per-scan gauge (watchtower_stranded_init_deps) keeps the alert
// firing until an operator redeploys with deps.
func warnStrandedInitDeps(target types.Container, siblings []string, recovering bool) {
	metrics.RegisterStrandedInitDeps()
	entry := log.WithFields(log.Fields{
		fieldContainer: target.Name(),
		"project":      target.ComposeProject(),
		"siblings":     strings.Join(siblings, ","),
	})
	if recovering {
		entry.Warn("--rerun-init-deps: target's com.docker.compose.depends_on is empty but its compose project still holds one-shot init siblings (typically a `docker compose up --no-deps <svc>` recreate emptied the label); re-running the detected siblings as a safety net so the new image does not start on an un-migrated schema. Re-arm with `docker compose up -d --force-recreate <service>` to restore the declared depends_on.")
		return
	}
	entry.Warn("--rerun-init-deps: target's com.docker.compose.depends_on is empty but its compose project still holds one-shot init siblings (typically a `docker compose up --no-deps <svc>` recreate emptied the label); they will NOT be re-run automatically, so the new image may start on an un-migrated schema. Re-arm with `docker compose up -d --force-recreate <service>` to restore the declared depends_on, or opt in to automatic recovery with --rerun-stranded-init-deps.")
}
