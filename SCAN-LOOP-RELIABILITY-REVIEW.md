# Scan-loop reliability review — watchdog & rerun-init-deps cluster

> **Status (2026-07-24):** bugs 1–9 FIXED, and **step 4 (flaw A) is
> IMPLEMENTED**: the entire update call tree now propagates a run-scoped
> `context.Context`, the watchdog is two-stage (cancel the wedged run first —
> lock released, partial report emitted, process stays up — exit only if the
> cancellation doesn't drain it within 2m), and the stuck rule keys on
> `RunProgress` heartbeats, not run age. The recreate executor's restart half
> runs under `context.WithoutCancel`, closing bug 3's stop-phase blast-radius
> amplifier. Bug 5's optional follow-up also landed: stranded recovery is now
> opt-in via `--rerun-stranded-init-deps` (detection warn+gauge stays
> always-on). New flags: `--watchdog-stuck-deadline`, `--watchdog-tick-grace`,
> `--rerun-stranded-init-deps`. Fixing bug 6's tests also surfaced and fixed a
> reporting bug not in this list: containers whose staleness was gated off
> post-scan (image cooldown, rollback cooldown, rejected digest) were
> notified as *updated* every poll. Still open: flaw B (`Daemon` state
> consolidation — softened: false watchdog kills are gone and the rejection
> cache has a TTL, but the five package-level maps remain) and flaw C (the
> scan→plan→execute pipeline; its bug classes 4/5 are fixed, the monolith
> refactor is not).

**Date:** 2026-07-22
**Reviewer:** analysis pass (no code changed)
**Scope:** the three most recent "keep the scan loop alive / correct" commits and the
code they landed in.

| Commit | Title |
|---|---|
| `5b40952` | feat(watchdog): fail-fast when the scan loop verifiably stops |
| `eded6e0` | fix(rerun-init-deps): recover stranded targets instead of only warning |
| `69c170e` | feat(metrics): watchtower_stranded_init_deps gauge for a non-resolving alert |

**Files read:** `cmd/root.go`, `internal/watchdog/watchdog.go` (+ test),
`internal/actions/update.go`, `internal/initrerun/rerun.go`, `pkg/api/api.go`,
`pkg/api/update/update.go`, `pkg/metrics/metrics.go`, `pkg/container/metadata.go`.

---

## TL;DR

One **critical** bug: the new watchdog crash-loops every HTTP-API-only deployment
(`--http-api-update` without `--http-api-periodic-polls`) roughly every 15 minutes.
Two further watchdog false-kill scenarios exist (irregular cron schedules; long
`--update-on-start` runs). In the rerun-init-deps area, monitor-only / `--no-restart`
targets can still trigger migration runs, the stranded heuristic misfires on
legitimately-declared `service_started` / `service_healthy` dependencies, and the
rejected-digest cache is poisoned by transient and orchestration failures.

Underneath the individual bugs there is a **fundamental flaw**: the watchdog is a
process-suicide compensating control bolted onto a call tree that has no
context/deadline propagation, and it silently invalidates the "restart = operator
touch-point" premise that five in-memory state caches are built on.

---

## Bugs (severity-ordered)

### 1. CRITICAL — HTTP-API-only mode is killed every ~15 minutes

> ✅ **FIXED** — `DisableTickRule()` disarms the tick rule before the blocking
> `httpAPI.Start(true)`; the stuck rule stays armed for API-triggered runs.

`cmd/root.go:325` — when the update API is enabled and periodic polls are **not**
(`--http-api-update` without `--http-api-periodic-polls`), `httpAPI.Start(true)`
blocks forever serving HTTP. `runUpgradesOnSchedule` is therefore never reached, so
`SetTickGrace` is never called and **no `Tick()` ever fires**. But the watchdog was
armed earlier (`cmd/root.go:201`) with the 15-minute default grace, so `probe`
(`internal/watchdog/watchdog.go:179`) trips "no scheduler tick" and `os.Exit(1)`s a
perfectly healthy process. The restart policy revives it; the cycle repeats forever.
If an API-triggered update happens to be in flight at the 15-minute mark, it is killed
mid-update.

- **Mode is first-class and documented** (`docs/http-api-mode.md`); the new
  `--no-watchdog` docs don't mention this interaction.
- **Fix direction:** don't arm the tick rule when the scheduler will never run (keep
  the stuck-update rule, which is fine there), or tick from the API handler.

### 2. HIGH — irregular cron schedules cause recurring false-positive exits

> ✅ **FIXED** — the tick rule is schedule-anchored via `SetSchedule` (trips only
> when `now > schedule.Next(lastTick) + grace`), exactly the direction below.

`cmd/root.go:522` derives the cadence from the gap between the **first two** fires and
sets grace to `3×` that (`SetTickGrace`, `watchdog.go:101`). A business-hours schedule
like `0 */15 8-18 * * 1-5` yields a ~45-minute grace, but the overnight gap is ~14h and
the weekend gap ~62h. The watchdog kills the process roughly every 48 minutes all
weekend, each revival re-emitting the startup notification and incurring restart-policy
backoff.

- The interval was introduced for a staleness **metric** ("best-effort approximation…
  good enough", `cmd/root.go:521`); it is now gating process death, which it was never
  designed for.
- **Fix direction:** anchor the deadline to the schedule itself.
  `scheduler.Entries()[0].Next` gives the actual next fire — the rule should be
  "now > next-scheduled-fire + slack", not a fixed grace derived from one sample.

### 3. MEDIUM-HIGH — `--update-on-start` runs before the scheduler, under the tick rule

> ✅ **FIXED** — the tick rule suspends while a run is in flight (the stuck rule
> alone bounds it) and `UpdateFinished` re-baselines the tick clock. The
> blast-radius amplifier is closed too: the restart phase runs under
> `context.WithoutCancel`, so no remedy can strand stopped containers.

The initial scan (`cmd/root.go:533-548`) runs synchronously **before**
`scheduler.Start()` (`cmd/root.go:550`), so no ticks arrive during it and the baseline
was just reset by `SetTickGrace`. The stuck-update rule allows a generous 1h, but the
tick rule does not care that an update is running (the "robfig/cron ticks regardless"
comment at `watchdog.go:176` only holds once the scheduler is started). A first deploy
pulling a large stack over a slow link for >15 minutes (with a short poll interval) is
killed mid-update.

- **Blast-radius amplifier:** the recreate path is stop-all-then-start-all
  (`stopContainersInReversedOrder` → `restartContainersInSortedOrder`,
  `update.go:609-611`). A kill landing between the phases leaves containers stopped, and
  without `--include-stopped` the revived process never restarts them — a slow-but-working
  update is converted into an outage.

### 4. MEDIUM-HIGH — monitor-only / `--no-restart` targets still trigger init-sibling reruns

> ✅ **FIXED** — the rerun loop gates on the executor's eligibility
> (`params.NoRestart || IsMonitorOnly`), per the direction below.

The init-rerun loop gates only on `IsStale()` (`update.go:514`), but `SetStale(true)`
(`update.go:478`) is reached for stale containers that are monitor-only or under
`--no-restart` — those are filtered out later, via `containersToUpdate` / `ToRestart`
(`update.go:588-594`, `443`). So with `--rerun-init-deps` **plus** a `monitor-only`
label (or the global flag), watchtower **executes the migration container against the
new image** while deliberately never updating the app — producing exactly the
new-schema-under-old-code state the feature exists to prevent, and doing side-effecting
work under a flag operators read as "observe only".

- **Fix direction:** the rerun loop must use the same eligibility predicate the
  executor uses (`ToRestart` / `!IsMonitorOnly`), not raw staleness.

### 5. MEDIUM — stranded detection fires on intact, legitimately-declared labels

> ✅ **FIXED** — the discriminator is now the present-but-EMPTY raw label
> (`ComposeDependsOnRaw`); absent or populated labels are never flagged. The
> suggested opt-in flag for the recovery also landed: `--rerun-stranded-init-deps`.

`strandedInitSiblings` (`update.go:662`) treats `ComposeInitDependencies() == 0` as
"label emptied", but that function (`pkg/container/metadata.go:122`) also returns nil
when `depends_on` is **present and populated** with only `service_started` /
`service_healthy` conditions (it filters to `service_completed_successfully`). A target
declaring `depends_on: db: {condition: service_healthy}` — a contract that was never
stripped — in a project that also holds any `restart: no` container (a manually-run
backup one-shot, a debug job) is flagged stranded: a warning every scan, a firing gauge
alert the operator cannot resolve except by adding the opt-out label, and — worse since
`eded6e0` — watchtower now **runs** that unrelated one-shot before every update of the
target.

- The heuristic also cannot distinguish "never declared deps at all" from "deps stripped
  by `--no-deps`".
- **Fix direction:** the discriminator should at minimum be "raw
  `com.docker.compose.depends_on` label is the empty string" (dropped) vs. "absent"
  (never had deps). Given the false-positive cost is now *executing containers*, the
  recovery (as opposed to the warn+gauge) arguably deserves its own opt-in flag.

### 6. MEDIUM — the rejected-digest cache is poisoned by transient & orchestration failures

> ✅ **FIXED** — orchestration failures (exit −1) are never cached and re-warn
> every scan; genuine non-zero exits expire after a 1h TTL.

`update.go:546` caches the **target's** digest as rejected whenever the last dep result
failed, without distinguishing why. Two bad shapes:

- **(a) transient different-image dep:** pg-ready exits non-zero because postgres was
  briefly down → the target's perfectly good new image is blocked until a process
  restart *or* a new push of the target's image (possibly weeks).
- **(b) "dep not found" (exit −1):** after `docker system prune` removes the exited
  one-shot carcasses, the dep is missing (`initrerun/rerun.go:84-92`), the digest is
  rejected, and after any restart the dep is **still** missing so every future digest is
  re-rejected on first sight. A routine prune silently and permanently disables updates
  for compose targets with declared init deps — with only a one-time Warn (later polls
  log at Debug, `update.go:413-424`).

- **Fix direction:** orchestration errors (dep-not-found, exit −1) must not be cached as
  digest rejections; add a TTL or retry-with-backoff for genuine non-zero exits.

### 7. LOW — graceful shutdown can be watchdog-killed

> ✅ **FIXED** — the watchdog context is cancelled on the SIGTERM path before
> waiting on the drain.

After SIGTERM the cron stops ticking while `runUpgradesOnSchedule` waits on `<-lock` for
the in-flight update to drain (`cmd/root.go:557-560`). A drain longer than the tick
grace trips the watchdog mid-drain. The watchdog context (`context.Background()`,
`cmd/root.go:203`) is never cancelled — it should be cancelled on the signal path.

### 8. LOW — no tunability

> ✅ **FIXED** — `--watchdog-stuck-deadline` and `--watchdog-tick-grace` flags.

The 1h stuck deadline and 15m floor are compile-time constants (`watchdog.go:38-42`);
the only knob is all-or-nothing `--no-watchdog`, and the docs literally instruct people
with >1h legitimate cycles to disable the whole protection. `--watchdog-stuck-deadline`
/ `--watchdog-tick-grace` duration flags would keep protection for exactly the users who
need longer allowances.

### 9. NIT — dead variable

> ✅ **FIXED** — removed.

`staleCount` (`update.go:378,481,553`) is written but never read; dead since the metrics
refactor.

---

## Fundamental-flaw assessment

The last three commits confirm the area is being **built up** rather than architected,
in two distinct ways.

### Flaw A — the watchdog is a compensating control for a context-less pipeline

> ✅ **IMPLEMENTED** — run-scoped ctx through Client/actions/registry/lifecycle,
> two-stage cancel-then-exit watchdog, progress-based stuck rule (all three
> bullets below).

The root cause of the wedges `5b40952` describes (stalled pull, notification send without
timeout, dead socket) is that the entire call tree (`actions.Update` → `container.Client`
→ registry / notifications) is synchronous with **no `context.Context` or deadline
propagation anywhere**. Because a stuck call cannot be cancelled, the only available
remedy is process suicide — with all its collateral: mid-update kills can leave
containers stopped (bug 3), and every kill wipes the in-memory state below.

The moby v29 migration (already merged to main) means every client call **already
accepts a context** — the seam exists now and didn't before.

**Proper architecture:**
- A run-scoped context with per-phase deadlines (pull, notify, per-container recreate).
- A **two-stage** watchdog: first *cancel* the stuck run (lock released via `defer`,
  partial report emitted, process stays healthy); escalate to `os.Exit` only if
  cancellation itself doesn't complete within a grace period. The watchdog stays — it's
  the right tool for a dead scheduler or wedged boot — but its blast radius shrinks from
  "kill the process mid-update" to "abort one run cleanly".
- The stuck rule should track **progress**, not run age. `session.Progress` is already
  updated per container and is a natural heartbeat: it distinguishes a slow 200-container
  run (progressing, fine at 3h) from a stuck pull (no progress in 20 min, kill sooner) —
  better detection *and* fewer false kills.

### Flaw B — the watchdog silently broke the "restart = operator touch-point" contract

> ⏳ **OPEN** (softened): false watchdog kills are gone, restarts are rare again,
> and `rejectedInitDigests` now has a TTL regardless — but the five
> package-level maps and the `Daemon` consolidation remain future work.

`internal/actions` now holds five package-level mutex-guarded maps — `failedRollbacks`,
`pendingImages`, `previousImages`, `rejectedInitDigests`, `selfStartFailures`
(`update.go:47-98`), plus `knownUnmanaged` in `check.go` — each added by one feature,
each with a `Reset…ForTest` helper, and each documented with a variant of *"clears on
restart, which is intentional: a restart is an operator touch-point."*

The watchdog makes restarts **automatic and frequent** — that premise is now false:
- A watchdog restart re-runs a known-failing migration container (side effects!) because
  `rejectedInitDigests` cleared.
- Restarts reset `pendingImages` clocks, so the supply-chain cooldown window silently
  re-extends.
- `previousImages` loses the rollback generation it was preserving.

None is individually severe, but the pattern is the "building up" smell: every feature
adds a global + mutex + test-reset, and no single component owns run/daemon state.

**Refactor shape:** a `Daemon` / `runtimeState` struct constructed once in `cmd.Run`
(client, notifier, watchdog, and the state maps behind one small type), passed into
`Update`. This also fixes the nil-receiver-watchdog pattern, unblocks test parallelism,
and provides exactly one place to add optional state *persistence* if watchdog restarts
make it necessary.

### Flaw C (smaller) — eligibility is computed twice, differently

> ⏳ **OPEN** (defanged): the rerun loop now uses the executor's predicate, so
> bugs 4/5's class is closed; the scan→plan→execute pipeline itself is not built.

Bugs 4 and 5 share a root: `Update` is a monolith (two `//nolint:cyclop` with "refactor
tracked separately", `update.go:368`, `1176`) where the staleness loop mutates
`stale` / `SetStale` as control flow, the init-rerun loop later *reverses* decisions, and
the executor uses a *different* predicate (`ToRestart` / `IsMonitorOnly` / partition)
than the rerun loop (`IsStale`). A `scan → gates → immutable plan → execute → report`
pipeline, where "will this container actually be updated" is answered **exactly once**,
makes bug 4's class impossible. Likewise the compose heuristics (`strandedInitSiblings`,
`orderInitSiblings`, `findInProject`) each re-scan a flat `[]Container`; a `ComposeProject`
model built once per scan would make the stranded predicate testable in isolation and
would have surfaced bug 5 at design time.

---

## Suggested sequencing

1. **Hotfix bug 1 now** — breaks a documented mode, needs a one-line guard (don't arm the
   tick rule when the scheduler won't run). Bugs 2 and 4 are close behind.
2. **Schedule-anchored tick deadline + watchdog duration flags** — fixes 2, 3, 8 together.
3. **rerun-init-deps corrections (4, 5, 6)** — small, local, test-backed.
4. **Context propagation + two-stage watchdog** — its own tracked effort; the real fix for
   the class `5b40952` treats symptomatically. The moby v29 base makes this the right
   moment.
5. **`Daemon` state consolidation** — can ride along with 4 or follow it.
