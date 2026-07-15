// Package initrerun orchestrates re-running Docker Compose
// service_completed_successfully siblings (typically migration / schema-init
// containers) against the new image *before* the target container is recreated.
//
// Why this exists: Watchtower operates at the container level — it stops and
// recreates a target container when its image digest changes. Compose's
// depends_on with `service_completed_successfully` is only evaluated by
// `docker compose up`, not by Watchtower. Stacks that moved bootstrap logic
// out of an entrypoint.sh wrapper and into a sibling init container therefore
// regress to "new code, old schema" on every Watchtower-driven update.
//
// This package restores the every-restart-runs-migrations contract while
// keeping the old container serving traffic until migrations succeed. The
// caller (internal/actions.Update) consults the [Result.Succeeded] of each
// returned entry to decide whether to proceed with the target's recreate or
// cache the digest as rejected and leave the old container running.
package initrerun

import (
	"fmt"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/openserbia/watchtower/pkg/container"
	t "github.com/openserbia/watchtower/pkg/types"
)

// DefaultTimeout caps how long any single init container is given to exit.
// Migrations are normally fast; this is the runaway-prevention bound.
const DefaultTimeout = 10 * time.Minute

// Result captures one init-rerun attempt's outcome.
type Result struct {
	TargetName string    // main container whose deps were being run
	DepName    string    // the init container that ran (or was supposed to)
	NewImageID t.ImageID // digest the rerun was pinned to
	ExitCode   int       // container exit code; -1 on orchestration error
	Err        error     // non-nil for any failure (orchestration or non-zero exit)
}

// Succeeded reports whether this init container ran and exited cleanly.
func (r Result) Succeeded() bool { return r.Err == nil && r.ExitCode == 0 }

// Run iterates the target's service_completed_successfully deps in order,
// re-creating each one against the resolved new image and waiting for clean
// exit. Stops at the first failure — remaining deps are not attempted, and
// the caller is expected to abort the target's update.
//
// Deps whose image differs from the target's keep their own pinning untouched
// — only same-image init containers (the migrate-sibling-of-same-image
// pattern) inherit the target's freshly-resolved digest.
func Run(client container.Client, target t.Container, allContainers []t.Container, timeout time.Duration) []Result {
	return RunDeps(client, target, target.ComposeInitDependencies(), allContainers, timeout)
}

// RunDeps is Run with an explicit list of init-dep service names, for callers
// that discovered the siblings by means other than the depends_on label. The
// stranded-init-deps recovery uses it: a `docker compose up --no-deps` recreate
// (the .env-only in-place update pattern) stamps an EMPTY
// com.docker.compose.depends_on, so ComposeInitDependencies() returns nothing
// even though the project still holds one-shot init siblings. depSvcs are
// compose service names in the target's project, run in the given order.
func RunDeps(client container.Client, target t.Container, depSvcs []string, allContainers []t.Container, timeout time.Duration) []Result {
	if len(depSvcs) == 0 {
		return nil
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	project := target.ComposeProject()
	if project == "" {
		// Defensive: shouldn't happen for compose-managed containers, but
		// without a project label we can't disambiguate dep service names.
		log.WithField("container", target.Name()).Warn("--rerun-init-deps: target has init deps but no compose project label; skipping")
		return nil
	}

	results := make([]Result, 0, len(depSvcs))

	for _, depSvc := range depSvcs {
		dep, ok := findInProject(allContainers, project, depSvc)
		if !ok {
			results = append(results, Result{
				TargetName: target.Name(),
				DepName:    depSvc,
				ExitCode:   -1,
				Err:        fmt.Errorf("init dep %q not found in compose project %q", depSvc, project),
			})
			return results
		}

		// Same-image deps inherit the target's resolved digest so the rerun
		// uses exactly the bits we resolved during the staleness check.
		// Different-image deps (e.g. pg-ready: postgres:18 when target uses
		// myapp:latest) keep their own pinning.
		if dep.ImageName() == target.ImageName() && target.TargetImageID() != "" {
			dep.SetTargetImageID(target.TargetImageID())
		}

		log.WithFields(log.Fields{
			"target":  target.Name(),
			"dep":     depSvc,
			"project": project,
			"digest":  target.TargetImageID().ShortID(),
		}).Info("--rerun-init-deps: re-running init container against new image")

		exitCode, err := client.RerunInitContainer(dep, timeout)
		result := Result{
			TargetName: target.Name(),
			DepName:    depSvc,
			NewImageID: target.TargetImageID(),
			ExitCode:   exitCode,
			Err:        err,
		}
		if err == nil && exitCode != 0 {
			result.Err = fmt.Errorf("init container %q exited with code %d", depSvc, exitCode)
		}
		results = append(results, result)

		if !result.Succeeded() {
			log.WithFields(log.Fields{
				"target":    target.Name(),
				"dep":       depSvc,
				"exit_code": exitCode,
			}).WithError(result.Err).Error("--rerun-init-deps: aborting update; old container keeps running")
			return results
		}
	}

	return results
}

// findInProject returns the container in `containers` whose compose project +
// service labels match the given pair.
func findInProject(containers []t.Container, project, service string) (t.Container, bool) {
	for _, c := range containers {
		if c.ComposeProject() == project && c.ComposeService() == service {
			return c, true
		}
	}
	return nil, false
}
