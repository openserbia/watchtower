package audit_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	dockerContainer "github.com/docker/docker/api/types/container"
	"github.com/docker/go-connections/nat"

	"github.com/openserbia/watchtower/internal/actions/mocks"
	apiAudit "github.com/openserbia/watchtower/pkg/api/audit"
	"github.com/openserbia/watchtower/pkg/types"
)

func newMock(name string, labels map[string]string) types.Container {
	return mocks.CreateMockContainerWithConfig(
		name, name, "fake-image:latest", true, false, time.Now(),
		&dockerContainer.Config{
			Image:        "fake-image:latest",
			Labels:       labels,
			ExposedPorts: map[nat.Port]struct{}{},
		},
	)
}

func TestAudit_ReturnsCategorizedReport(t *testing.T) {
	enableLabel := "com.centurylinklabs.watchtower.enable"
	containers := []types.Container{
		newMock("managed-svc", map[string]string{enableLabel: "true"}),
		newMock("excluded-svc", map[string]string{enableLabel: "false"}),
		newMock("unmanaged-svc", map[string]string{}),
	}
	client := mocks.CreateMockClient(&mocks.TestData{Containers: containers}, false, false)
	handler := apiAudit.New(client, "")

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, apiAudit.Path, nil)
	rec := httptest.NewRecorder()
	handler.Handle(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatalf("expected JSON content type, got %q", ct)
	}

	var report apiAudit.Report
	if err := json.NewDecoder(rec.Body).Decode(&report); err != nil {
		t.Fatalf("decode report: %v", err)
	}

	if len(report.Containers) != 3 {
		t.Fatalf("expected 3 container entries, got %d", len(report.Containers))
	}
	if report.Summary.Managed != 1 || report.Summary.Excluded != 1 || report.Summary.Unmanaged != 1 || report.Summary.Total != 3 {
		t.Fatalf("unexpected summary: %+v", report.Summary)
	}

	byName := map[string]apiAudit.Status{}
	for _, e := range report.Containers {
		byName[e.Name] = e.Status
	}
	if byName["managed-svc"] != apiAudit.StatusManaged {
		t.Errorf("managed-svc: got %q", byName["managed-svc"])
	}
	if byName["excluded-svc"] != apiAudit.StatusExcluded {
		t.Errorf("excluded-svc: got %q", byName["excluded-svc"])
	}
	if byName["unmanaged-svc"] != apiAudit.StatusUnmanaged {
		t.Errorf("unmanaged-svc: got %q", byName["unmanaged-svc"])
	}
}

func TestAudit_ClassifiesInfrastructureSeparately(t *testing.T) {
	enableLabel := "com.centurylinklabs.watchtower.enable"
	// buildkit: matched by image prefix
	buildkit := mocks.CreateMockContainerWithConfig(
		"buildx_buildkit_default0", "buildx_buildkit_default0",
		"moby/buildkit:v0.12.0", true, false, time.Now(),
		&dockerContainer.Config{Image: "moby/buildkit:v0.12.0", Labels: map[string]string{}, ExposedPorts: map[nat.Port]struct{}{}},
	)
	// desktop: matched by image prefix
	desktop := mocks.CreateMockContainerWithConfig(
		"docker-desktop-kubernetes", "docker-desktop-kubernetes",
		"docker/desktop-kubernetes:v1.28.2", true, false, time.Now(),
		&dockerContainer.Config{Image: "docker/desktop-kubernetes:v1.28.2", Labels: map[string]string{}, ExposedPorts: map[nat.Port]struct{}{}},
	)
	// labeled infrastructure: matched by label prefix, even if image is unrelated
	buildxLabeled := mocks.CreateMockContainerWithConfig(
		"some-buildx-helper", "some-buildx-helper",
		"alpine:3.18", true, false, time.Now(),
		&dockerContainer.Config{Image: "alpine:3.18", Labels: map[string]string{"com.docker.buildx.refresh": "true"}, ExposedPorts: map[nat.Port]struct{}{}},
	)
	containers := []types.Container{
		buildkit, desktop, buildxLabeled,
		newMock("real-svc", map[string]string{enableLabel: "true"}),
		newMock("unlabeled-svc", map[string]string{}),
	}
	client := mocks.CreateMockClient(&mocks.TestData{Containers: containers}, false, false)
	handler := apiAudit.New(client, "")

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, apiAudit.Path, nil)
	rec := httptest.NewRecorder()
	handler.Handle(rec, req)

	var report apiAudit.Report
	if err := json.NewDecoder(rec.Body).Decode(&report); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if report.Summary.Infrastructure != 3 {
		t.Fatalf("expected 3 infrastructure entries, got %d (%+v)", report.Summary.Infrastructure, report.Summary)
	}
	if report.Summary.Unmanaged != 1 {
		t.Fatalf("expected 1 unmanaged, got %d", report.Summary.Unmanaged)
	}
	if report.Summary.Managed != 1 {
		t.Fatalf("expected 1 managed, got %d", report.Summary.Managed)
	}
}

func TestAudit_SortsAlphabetically(t *testing.T) {
	containers := []types.Container{
		newMock("charlie", map[string]string{}),
		newMock("alpha", map[string]string{}),
		newMock("bravo", map[string]string{}),
	}
	client := mocks.CreateMockClient(&mocks.TestData{Containers: containers}, false, false)
	handler := apiAudit.New(client, "")

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, apiAudit.Path, nil)
	rec := httptest.NewRecorder()
	handler.Handle(rec, req)

	var report apiAudit.Report
	if err := json.NewDecoder(rec.Body).Decode(&report); err != nil {
		t.Fatalf("decode: %v", err)
	}

	want := []string{"alpha", "bravo", "charlie"}
	for i, entry := range report.Containers {
		if entry.Name != want[i] {
			t.Fatalf("position %d: want %q, got %q", i, want[i], entry.Name)
		}
	}
}
