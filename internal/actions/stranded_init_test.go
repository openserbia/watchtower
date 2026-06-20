package actions

import (
	"bytes"
	"strings"
	"testing"

	dockercontainer "github.com/moby/moby/api/types/container"
	log "github.com/sirupsen/logrus"

	"github.com/openserbia/watchtower/pkg/container"
	"github.com/openserbia/watchtower/pkg/types"
)

const strandedTestProject = "proj"

// newStrandedFixture builds a compose-managed container fixture with a given
// service name and restart policy. Only the fields warnIfStrandedInitDeps /
// isOneShotInitSibling read are populated; imageInfo is irrelevant and nil.
func newStrandedFixture(service, restart string) types.Container {
	labels := map[string]string{
		"com.docker.compose.project": strandedTestProject,
		"com.docker.compose.service": service,
	}

	return container.NewContainer(&dockercontainer.InspectResponse{
		ID:    service,
		Name:  service,
		Image: "img:latest",
		HostConfig: &dockercontainer.HostConfig{
			RestartPolicy: dockercontainer.RestartPolicy{
				Name: dockercontainer.RestartPolicyMode(restart),
			},
		},
		Config: &dockercontainer.Config{Image: "img:latest", Labels: labels},
	}, nil)
}

func TestIsOneShotInitSibling(t *testing.T) {
	cases := []struct {
		name    string
		service string
		restart string
		want    bool
	}{
		{"explicit no policy is one-shot", "migrate", "no", true},
		{"empty policy defaults to one-shot", "pg-ready", "", true},
		{"unless-stopped is long-lived", "api", "unless-stopped", false},
		{"always is long-lived", "api", "always", false},
		{"on-failure is long-lived", "worker", "on-failure", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isOneShotInitSibling(newStrandedFixture(tc.service, tc.restart))
			if got != tc.want {
				t.Fatalf("isOneShotInitSibling(%s/%s) = %v, want %v", tc.service, tc.restart, got, tc.want)
			}
		})
	}

	t.Run("non-compose container is never an init sibling", func(t *testing.T) {
		noService := container.NewContainer(&dockercontainer.InspectResponse{
			ID:         "x",
			Name:       "x",
			HostConfig: &dockercontainer.HostConfig{RestartPolicy: dockercontainer.RestartPolicy{Name: "no"}},
			Config:     &dockercontainer.Config{Labels: map[string]string{}},
		}, nil)
		if isOneShotInitSibling(noService) {
			t.Fatal("expected false for a container with no compose service label")
		}
	})

	t.Run("missing host config does not panic", func(t *testing.T) {
		noHostConfig := container.NewContainer(&dockercontainer.InspectResponse{
			ID:     "migrate",
			Name:   "migrate",
			Config: &dockercontainer.Config{Labels: map[string]string{"com.docker.compose.service": "migrate"}},
		}, nil)
		if isOneShotInitSibling(noHostConfig) {
			t.Fatal("expected false when host config is absent")
		}
	})
}

// captureWarn runs fn with the global logger redirected to a buffer and returns
// what it logged. Restores logger state afterwards.
func captureWarn(t *testing.T, fn func()) string {
	t.Helper()

	var buf bytes.Buffer

	origOut := log.StandardLogger().Out
	origLevel := log.GetLevel()

	log.SetOutput(&buf)
	log.SetLevel(log.WarnLevel)

	defer func() {
		log.SetOutput(origOut)
		log.SetLevel(origLevel)
	}()

	fn()

	return buf.String()
}

func TestWarnIfStrandedInitDeps(t *testing.T) {
	const marker = "migrations will NOT be re-run"

	t.Run("warns when a one-shot init sibling is present", func(t *testing.T) {
		target := newStrandedFixture("api", "unless-stopped") // no depends_on
		migrate := newStrandedFixture("migrate", "no")

		out := captureWarn(t, func() {
			warnIfStrandedInitDeps(target, []types.Container{target, migrate})
		})
		if !strings.Contains(out, marker) {
			t.Fatalf("expected stranded-init warning, got: %q", out)
		}
		if !strings.Contains(out, "migrate") {
			t.Fatalf("expected the offending sibling service in the warning, got: %q", out)
		}
	})

	t.Run("silent when the only siblings are long-lived", func(t *testing.T) {
		target := newStrandedFixture("api", "unless-stopped")
		web := newStrandedFixture("web", "unless-stopped")

		out := captureWarn(t, func() {
			warnIfStrandedInitDeps(target, []types.Container{target, web})
		})
		if strings.Contains(out, marker) {
			t.Fatalf("did not expect a warning when no one-shot siblings exist, got: %q", out)
		}
	})

	t.Run("silent when the target opts out via no-init-deps label", func(t *testing.T) {
		// A frontend in a project with migrate/pg-ready siblings owned by a
		// sibling API tier: empty depends_on is by design, not a dropped label.
		target := container.NewContainer(&dockercontainer.InspectResponse{
			ID:   "web",
			Name: "web",
			HostConfig: &dockercontainer.HostConfig{
				RestartPolicy: dockercontainer.RestartPolicy{Name: "unless-stopped"},
			},
			Config: &dockercontainer.Config{Labels: map[string]string{
				"com.docker.compose.project":                  strandedTestProject,
				"com.docker.compose.service":                  "web",
				"com.centurylinklabs.watchtower.no-init-deps": "true",
			}},
		}, nil)
		migrate := newStrandedFixture("migrate", "no")

		out := captureWarn(t, func() {
			warnIfStrandedInitDeps(target, []types.Container{target, migrate})
		})
		if strings.Contains(out, marker) {
			t.Fatalf("did not expect a warning for a no-init-deps opt-out target, got: %q", out)
		}
	})

	t.Run("silent when the target is not compose-managed", func(t *testing.T) {
		bare := container.NewContainer(&dockercontainer.InspectResponse{
			ID:         "bare",
			Name:       "bare",
			HostConfig: &dockercontainer.HostConfig{RestartPolicy: dockercontainer.RestartPolicy{Name: "unless-stopped"}},
			Config:     &dockercontainer.Config{Labels: map[string]string{}},
		}, nil)
		migrate := newStrandedFixture("migrate", "no")

		out := captureWarn(t, func() {
			warnIfStrandedInitDeps(bare, []types.Container{bare, migrate})
		})
		if strings.Contains(out, marker) {
			t.Fatalf("did not expect a warning for a non-compose target, got: %q", out)
		}
	})
}
