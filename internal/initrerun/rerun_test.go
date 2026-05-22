package initrerun_test

import (
	"testing"
	"time"

	dockerContainer "github.com/docker/docker/api/types/container"
	"github.com/docker/go-connections/nat"

	"github.com/openserbia/watchtower/internal/actions/mocks"
	"github.com/openserbia/watchtower/internal/initrerun"
	t "github.com/openserbia/watchtower/pkg/types"
)

// newComposeContainer builds a mock container with compose project/service
// labels, optional depends_on, and a fixed image name. Mirrors the helper in
// pkg/sorter/sort_test.go but exposes image name as an argument so we can
// exercise same-image vs different-image dep paths.
func newComposeContainer(name, project, service, image, dependsOn string) t.Container {
	labels := map[string]string{
		"com.docker.compose.project": project,
		"com.docker.compose.service": service,
	}
	if dependsOn != "" {
		labels["com.docker.compose.depends_on"] = dependsOn
	}
	return mocks.CreateMockContainerWithConfig(
		name, name, image, true, false, time.Now(),
		&dockerContainer.Config{
			Image:        image,
			Labels:       labels,
			ExposedPorts: map[nat.Port]struct{}{},
		},
	)
}

func TestRun_NoInitDeps_ReturnsNil(t1 *testing.T) {
	client := mocks.CreateMockClient(&mocks.TestData{}, false, false)
	target := newComposeContainer("api", "openserbia", "api", "myapp:latest", "")
	got := initrerun.Run(client, target, []t.Container{target}, time.Minute)
	if got != nil {
		t1.Fatalf("expected nil results for target with no init deps, got %d", len(got))
	}
}

func TestRun_HappyPath_OneSameImageDep(t1 *testing.T) {
	td := &mocks.TestData{}
	client := mocks.CreateMockClient(td, false, false)

	migrate := newComposeContainer("migrate", "openserbia", "migrate", "myapp:latest", "")
	target := newComposeContainer("api", "openserbia", "api", "myapp:latest",
		"migrate:service_completed_successfully:true")
	target.SetTargetImageID(t.ImageID("sha256:newdigest"))

	got := initrerun.Run(client, target, []t.Container{migrate, target}, time.Minute)
	if len(got) != 1 {
		t1.Fatalf("expected 1 result, got %d", len(got))
	}
	if !got[0].Succeeded() {
		t1.Fatalf("expected success, got err=%v exit=%d", got[0].Err, got[0].ExitCode)
	}
	if got[0].DepName != "migrate" || got[0].TargetName != "api" {
		t1.Fatalf("unexpected result fields: %+v", got[0])
	}
	if len(td.RerunInitContainers) != 1 {
		t1.Fatalf("expected exactly one rerun invocation, got %d", len(td.RerunInitContainers))
	}
	if td.RerunInitContainers[0].Name() != "migrate" {
		t1.Fatalf("expected migrate to be rerun, got %q", td.RerunInitContainers[0].Name())
	}
	if td.RerunInitContainers[0].TargetImageID() != "sha256:newdigest" {
		t1.Fatalf("same-image dep should inherit target digest, got %q",
			td.RerunInitContainers[0].TargetImageID())
	}
}

func TestRun_FailedInit_StopsAtFirstFailure(t1 *testing.T) {
	td := &mocks.TestData{
		InitExitByName: map[string]int{
			"migrate": 1, // non-zero exit
		},
	}
	client := mocks.CreateMockClient(td, false, false)

	migrate := newComposeContainer("migrate", "openserbia", "migrate", "myapp:latest", "")
	seed := newComposeContainer("seed", "openserbia", "seed", "myapp:latest", "")
	target := newComposeContainer("api", "openserbia", "api", "myapp:latest",
		"migrate:service_completed_successfully:true,seed:service_completed_successfully:true")

	got := initrerun.Run(client, target, []t.Container{migrate, seed, target}, time.Minute)
	if len(got) != 1 {
		t1.Fatalf("expected exactly 1 result (stopped at first failure), got %d", len(got))
	}
	if got[0].Succeeded() {
		t1.Fatalf("expected failure result")
	}
	if got[0].ExitCode != 1 {
		t1.Fatalf("expected exit code 1, got %d", got[0].ExitCode)
	}
	if len(td.RerunInitContainers) != 1 {
		t1.Fatalf("seed should not have been attempted after migrate failed; got %d reruns",
			len(td.RerunInitContainers))
	}
}

func TestRun_MissingDep_ReturnsError(t1 *testing.T) {
	client := mocks.CreateMockClient(&mocks.TestData{}, false, false)
	target := newComposeContainer("api", "openserbia", "api", "myapp:latest",
		"missing:service_completed_successfully:true")

	got := initrerun.Run(client, target, []t.Container{target}, time.Minute)
	if len(got) != 1 {
		t1.Fatalf("expected 1 error result, got %d", len(got))
	}
	if got[0].Succeeded() {
		t1.Fatalf("expected failure for missing dep")
	}
	if got[0].DepName != "missing" {
		t1.Fatalf("expected dep name to surface in result, got %q", got[0].DepName)
	}
}

func TestRun_DifferentImageDep_DoesNotInheritDigest(t1 *testing.T) {
	td := &mocks.TestData{}
	client := mocks.CreateMockClient(td, false, false)

	// pg-ready uses a different image (postgres) than the target (myapp).
	// The orchestrator should NOT pin pg-ready to target's digest — pg-ready
	// pulls its own image lifecycle.
	pgReady := newComposeContainer("pg-ready", "openserbia", "pg-ready", "postgres:18", "")
	pgReady.SetTargetImageID("") // explicit: no prior pinning

	target := newComposeContainer("api", "openserbia", "api", "myapp:latest",
		"pg-ready:service_completed_successfully:true")
	target.SetTargetImageID(t.ImageID("sha256:myapp-newdigest"))

	got := initrerun.Run(client, target, []t.Container{pgReady, target}, time.Minute)
	if len(got) != 1 || !got[0].Succeeded() {
		t1.Fatalf("expected one successful result, got %+v", got)
	}
	if len(td.RerunInitContainers) != 1 {
		t1.Fatalf("expected pg-ready to be rerun, got %d invocations", len(td.RerunInitContainers))
	}
	if got := td.RerunInitContainers[0].TargetImageID(); got != "" {
		t1.Fatalf("different-image dep should NOT inherit target digest; got %q", got)
	}
}

func TestRun_NonInitConditionsAreIgnored(t1 *testing.T) {
	td := &mocks.TestData{}
	client := mocks.CreateMockClient(td, false, false)

	// peer uses service_started (long-running peer), should NOT be re-run.
	peer := newComposeContainer("peer", "openserbia", "peer", "myapp:latest", "")
	migrate := newComposeContainer("migrate", "openserbia", "migrate", "myapp:latest", "")
	target := newComposeContainer("api", "openserbia", "api", "myapp:latest",
		"peer:service_started:true,migrate:service_completed_successfully:true")

	got := initrerun.Run(client, target, []t.Container{peer, migrate, target}, time.Minute)
	if len(got) != 1 || got[0].DepName != "migrate" {
		t1.Fatalf("expected only migrate to be considered; got %+v", got)
	}
	if len(td.RerunInitContainers) != 1 || td.RerunInitContainers[0].Name() != "migrate" {
		t1.Fatalf("expected only migrate rerun; got %d invocations", len(td.RerunInitContainers))
	}
}
