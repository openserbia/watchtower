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
// service name and restart policy. Only the fields strandedInitSiblings /
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

func TestWarnStrandedInitDeps(t *testing.T) {
	// warnStrandedInitDeps no longer detects — the caller passes the siblings
	// that strandedInitSiblings already found — so it always logs. The message
	// must name the recovered siblings, explain the --no-deps cause, and give
	// the re-arm remedy. The old "migrations will NOT be re-run" wording is gone:
	// they ARE re-run now, as a safety net.
	t.Run("logs the safety-net recovery line with siblings, cause and remedy", func(t *testing.T) {
		target := newStrandedFixture("api", "unless-stopped")

		out := captureWarn(t, func() {
			warnStrandedInitDeps(target, []string{"migrate", "pg-ready"})
		})
		for _, want := range []string{"safety net", "migrate", "pg-ready", "--no-deps", "--force-recreate"} {
			if !strings.Contains(out, want) {
				t.Fatalf("expected stranded-init warning to contain %q, got: %q", want, out)
			}
		}
		if strings.Contains(out, "will NOT be re-run") {
			t.Fatalf("stale wording — siblings are re-run now, got: %q", out)
		}
	})
}

func TestOrderInitSiblings(t *testing.T) {
	// A leaf one-shot (pg-ready, no own init deps) must sort before a one-shot
	// that declares it (migrate: depends_on pg-ready), regardless of input order,
	// so the recovered rerun applies them in a runnable sequence. The declared
	// order is lost with the emptied target label, so we reconstruct it from each
	// sibling's OWN init-dep count.
	migrate := newRearmedFixture("migrate", "pg-ready") // 1 own init dep
	pgReady := newStrandedFixture("pg-ready", "no")     // 0 own init deps
	all := []types.Container{migrate, pgReady}

	for _, in := range [][]string{{"migrate", "pg-ready"}, {"pg-ready", "migrate"}} {
		got := orderInitSiblings(in, all, strandedTestProject)
		if len(got) != 2 || got[0] != "pg-ready" || got[1] != "migrate" {
			t.Fatalf("orderInitSiblings(%v) = %v, want [pg-ready migrate]", in, got)
		}
	}

	t.Run("stable name tiebreak when own dep counts are equal", func(t *testing.T) {
		a := newStrandedFixture("a-init", "no")
		b := newStrandedFixture("b-init", "no")
		got := orderInitSiblings([]string{"b-init", "a-init"}, []types.Container{a, b}, strandedTestProject)
		if len(got) != 2 || got[0] != "a-init" || got[1] != "b-init" {
			t.Fatalf("expected name-sorted [a-init b-init], got %v", got)
		}
	})
}

// newRearmedFixture builds a compose-managed service that DECLARES its init
// deps — i.e. a target whose com.docker.compose.depends_on label is intact
// (the state produced by `docker compose up -d --force-recreate`). This is the
// not-stranded case that must drop the gauge back to 0.
func newRearmedFixture(service string, initDeps ...string) types.Container {
	entries := make([]string, 0, len(initDeps))
	for _, d := range initDeps {
		entries = append(entries, d+":service_completed_successfully:false")
	}

	return container.NewContainer(&dockercontainer.InspectResponse{
		ID:   service,
		Name: service,
		HostConfig: &dockercontainer.HostConfig{
			RestartPolicy: dockercontainer.RestartPolicy{Name: "unless-stopped"},
		},
		Config: &dockercontainer.Config{Labels: map[string]string{
			"com.docker.compose.project":    strandedTestProject,
			"com.docker.compose.service":    service,
			"com.docker.compose.depends_on": strings.Join(entries, ","),
		}},
	}, nil)
}

// TestStrandedInitSiblings exercises the predicate that both the warning and the
// watchtower_stranded_init_deps gauge are built on. countStranded mirrors the
// per-scan gauge loop in Update, so these cases pin the gauge's value.
func TestStrandedInitSiblings(t *testing.T) {
	countStranded := func(all []types.Container) int {
		n := 0
		for i := range all {
			if len(strandedInitSiblings(all[i], all)) > 0 {
				n++
			}
		}
		return n
	}

	t.Run("stranded target names its dropped init sibling", func(t *testing.T) {
		target := newStrandedFixture("api", "unless-stopped") // empty depends_on
		migrate := newStrandedFixture("migrate", "no")

		got := strandedInitSiblings(target, []types.Container{target, migrate})
		if len(got) != 1 || got[0] != "migrate" {
			t.Fatalf("expected [migrate], got %v", got)
		}
	})

	t.Run("re-armed target with intact depends_on is NOT stranded", func(t *testing.T) {
		// The resolve-on-fix case: after `compose up --force-recreate` the
		// label is back, ComposeInitDependencies() is non-empty, so the gauge
		// must read 0 and the alert clears.
		target := newRearmedFixture("api", "migrate", "pg-ready")
		migrate := newStrandedFixture("migrate", "no")
		pgReady := newStrandedFixture("pg-ready", "no")

		if got := strandedInitSiblings(target, []types.Container{target, migrate, pgReady}); got != nil {
			t.Fatalf("expected nil for a re-armed target, got %v", got)
		}
		if n := countStranded([]types.Container{target, migrate, pgReady}); n != 0 {
			t.Fatalf("expected gauge count 0 after re-arm, got %d", n)
		}
	})

	t.Run("opt-out and no-sibling targets are not counted", func(t *testing.T) {
		optOut := container.NewContainer(&dockercontainer.InspectResponse{
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

		if got := strandedInitSiblings(optOut, []types.Container{optOut, migrate}); got != nil {
			t.Fatalf("expected nil for a no-init-deps opt-out, got %v", got)
		}
	})

	t.Run("long-lived-only siblings are not stranded", func(t *testing.T) {
		target := newStrandedFixture("api", "unless-stopped")
		web := newStrandedFixture("web", "unless-stopped")

		if got := strandedInitSiblings(target, []types.Container{target, web}); got != nil {
			t.Fatalf("expected nil when no one-shot siblings exist, got %v", got)
		}
	})

	t.Run("non-compose target is not stranded", func(t *testing.T) {
		bare := container.NewContainer(&dockercontainer.InspectResponse{
			ID:         "bare",
			Name:       "bare",
			HostConfig: &dockercontainer.HostConfig{RestartPolicy: dockercontainer.RestartPolicy{Name: "unless-stopped"}},
			Config:     &dockercontainer.Config{Labels: map[string]string{}},
		}, nil)
		migrate := newStrandedFixture("migrate", "no")

		if got := strandedInitSiblings(bare, []types.Container{bare, migrate}); got != nil {
			t.Fatalf("expected nil for a non-compose target, got %v", got)
		}
	})

	t.Run("gauge counts each stranded parent once", func(t *testing.T) {
		// One stranded API + its migrate one-shot: exactly one stranded target.
		// The migrate one-shot itself has no siblings of its own to gate on.
		api := newStrandedFixture("api", "unless-stopped")
		migrate := newStrandedFixture("migrate", "no")

		if n := countStranded([]types.Container{api, migrate}); n != 1 {
			t.Fatalf("expected exactly 1 stranded target, got %d", n)
		}
	})
}
