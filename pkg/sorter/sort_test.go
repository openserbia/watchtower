package sorter_test

import (
	"testing"
	"time"

	dockerContainer "github.com/docker/docker/api/types/container"
	"github.com/docker/go-connections/nat"

	"github.com/openserbia/watchtower/internal/actions/mocks"
	"github.com/openserbia/watchtower/pkg/sorter"
	"github.com/openserbia/watchtower/pkg/types"
)

// newComposeMock builds a mock container with the given Compose-style labels.
// Everything else (image, running state) is a fixed placeholder.
func newComposeMock(name, project, service, dependsOn string) types.Container {
	labels := map[string]string{
		"com.docker.compose.project": project,
		"com.docker.compose.service": service,
	}
	if dependsOn != "" {
		labels["com.docker.compose.depends_on"] = dependsOn
	}
	return mocks.CreateMockContainerWithConfig(
		name, name, "img:latest", true, false, time.Now(),
		&dockerContainer.Config{
			Image:        "img:latest",
			Labels:       labels,
			ExposedPorts: map[nat.Port]struct{}{},
		},
	)
}

// assertOrder asserts that `before` appears earlier than `after` in the
// result. Reports both names on mismatch.
func assertOrder(t *testing.T, containers []types.Container, before, after string) {
	t.Helper()
	beforeIdx, afterIdx := -1, -1
	for i, c := range containers {
		switch c.Name() {
		case before:
			beforeIdx = i
		case after:
			afterIdx = i
		}
	}
	if beforeIdx < 0 || afterIdx < 0 {
		t.Fatalf("expected both %q (idx=%d) and %q (idx=%d) in result", before, beforeIdx, after, afterIdx)
	}
	if beforeIdx >= afterIdx {
		t.Fatalf("expected %q before %q, got order with indexes %d then %d", before, after, beforeIdx, afterIdx)
	}
}

func TestSortByDependencies_NoComposeDepsWhenDisabled(t *testing.T) {
	// Even if Compose labels are present, leaving the flag off must not
	// augment the graph — preserves pre-v1.12 behavior for operators who
	// don't opt in.
	api := newComposeMock("api", "myapp", "api", "db")
	db := newComposeMock("db", "myapp", "db", "")

	sorted, err := sorter.SortByDependencies([]types.Container{api, db}, false)
	if err != nil {
		t.Fatalf("sort: %v", err)
	}
	if len(sorted) != 2 {
		t.Fatalf("expected 2 containers, got %d", len(sorted))
	}
	// Without the flag, the input order is preserved (api first, db second).
	if sorted[0].Name() != "api" || sorted[1].Name() != "db" {
		t.Fatalf("unexpected order: %s, %s", sorted[0].Name(), sorted[1].Name())
	}
}

func TestSortByDependencies_ResolvesComposeDeps(t *testing.T) {
	// Input has api listed first; db must end up before api because api
	// depends on db.
	api := newComposeMock("api", "myapp", "api", "db")
	db := newComposeMock("db", "myapp", "db", "")

	sorted, err := sorter.SortByDependencies([]types.Container{api, db}, true)
	if err != nil {
		t.Fatalf("sort: %v", err)
	}
	assertOrder(t, sorted, "db", "api")
}

func TestSortByDependencies_ComposeDepsIsolatedByProject(t *testing.T) {
	// Two projects, both with a service called "api". Project A's "api"
	// depends on "db" — that must resolve to project A's db, not project B's.
	apiA := newComposeMock("a-api", "projA", "api", "db")
	dbA := newComposeMock("a-db", "projA", "db", "")
	apiB := newComposeMock("b-api", "projB", "api", "")
	dbB := newComposeMock("b-db", "projB", "db", "")

	sorted, err := sorter.SortByDependencies([]types.Container{apiA, dbA, apiB, dbB}, true)
	if err != nil {
		t.Fatalf("sort: %v", err)
	}
	// Project A edge must be respected; project B's untouched.
	assertOrder(t, sorted, "a-db", "a-api")
}

func TestSortByDependencies_StripsComposeModifiers(t *testing.T) {
	// Compose v2 depends_on serialises with optional modifiers after the
	// first colon. Sort should ignore them.
	api := newComposeMock("api", "myapp", "api", "db:service_healthy:true,cache:service_started:false")
	db := newComposeMock("db", "myapp", "db", "")
	cache := newComposeMock("cache", "myapp", "cache", "")

	sorted, err := sorter.SortByDependencies([]types.Container{api, db, cache}, true)
	if err != nil {
		t.Fatalf("sort: %v", err)
	}
	assertOrder(t, sorted, "db", "api")
	assertOrder(t, sorted, "cache", "api")
}

func TestSortByDependencies_UnknownComposeDepIsSilentlyDropped(t *testing.T) {
	// api depends on "ghost" which isn't in the current scan set (maybe
	// filtered out by --label-enable). The sort must not panic or error.
	api := newComposeMock("api", "myapp", "api", "ghost,db")
	db := newComposeMock("db", "myapp", "db", "")

	sorted, err := sorter.SortByDependencies([]types.Container{api, db}, true)
	if err != nil {
		t.Fatalf("sort: %v", err)
	}
	assertOrder(t, sorted, "db", "api")
}
