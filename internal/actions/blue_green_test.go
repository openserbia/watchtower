package actions_test

import (
	"errors"
	"time"

	dockerContainer "github.com/docker/docker/api/types/container"
	"github.com/docker/go-connections/nat"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/openserbia/watchtower/internal/actions"
	. "github.com/openserbia/watchtower/internal/actions/mocks"
	"github.com/openserbia/watchtower/pkg/types"
)

var _ = Describe("the blue-green update strategy", func() {
	const greenID = types.ContainerID("green-container-id")

	BeforeEach(func() {
		// The cooldown and previous-image maps are package-level and leak across
		// specs otherwise. Reset what has a public test hook; specs additionally
		// use distinct container names so the rollback-cooldown map (no public
		// reset) from one spec cannot mask another.
		actions.ResetCooldownStateForTest()
		actions.ResetPreviousImagesForTest()
	})

	// buildBlueGreenContainer builds a running, blue-green-eligible container:
	// no published host ports, no links, drain set to 0s so the cutover never
	// sleeps. The update-strategy label is left unset so callers can choose
	// between the global Strategy param and a per-container label.
	buildBlueGreenContainer := func(id, name string, extraLabels map[string]string) types.Container {
		labels := map[string]string{
			"com.centurylinklabs.watchtower.blue-green.drain": "0s",
		}
		for k, v := range extraLabels {
			labels[k] = v
		}
		config := &dockerContainer.Config{
			Image:        "fake-image:latest",
			Labels:       labels,
			ExposedPorts: map[nat.Port]struct{}{},
		}

		return CreateMockContainerWithConfig(id, name, "fake-image:latest", true, false, time.Now(), config)
	}

	blueGreenParams := types.UpdateParams{Strategy: types.StrategyBlueGreen}

	When("the green container becomes healthy", func() {
		It("cuts over: starts green and renames it to the canonical name", func() {
			blue := buildBlueGreenContainer("blue-healthy-id", "web-healthy", nil)
			data := &TestData{
				Containers:            []types.Container{blue},
				NextStartContainerIDs: []types.ContainerID{greenID},
				HealthStatusByID: map[types.ContainerID]string{
					greenID: dockerContainer.Healthy,
				},
			}
			client := CreateMockClient(data, false, false)

			report, err := actions.Update(client, blueGreenParams)
			Expect(err).NotTo(HaveOccurred())

			// Green was started under a temporary name, then renamed back to
			// the bare canonical name once blue was retired.
			Expect(client.TestData.StartedContainers).To(HaveLen(1))
			Expect(client.TestData.RenameCalls).To(HaveLen(1))
			Expect(client.TestData.RenameCalls[0].ContainerID).To(Equal(greenID))
			Expect(client.TestData.RenameCalls[0].NewName).To(Equal("web-healthy"))

			Expect(report.Failed()).To(BeEmpty())
		})
	})

	When("the green container reports unhealthy", func() {
		It("rolls back: removes green, keeps blue and records the container as failed", func() {
			blue := buildBlueGreenContainer("blue-unhealthy-id", "web-unhealthy", nil)
			data := &TestData{
				Containers:            []types.Container{blue},
				NextStartContainerIDs: []types.ContainerID{greenID},
				HealthStatusByID: map[types.ContainerID]string{
					greenID: dockerContainer.Unhealthy,
				},
			}
			client := CreateMockClient(data, false, false)

			report, err := actions.Update(client, blueGreenParams)
			Expect(err).NotTo(HaveOccurred())

			// Green was started but never promoted — no rename to the canonical name.
			Expect(client.TestData.StartedContainers).To(HaveLen(1))
			Expect(client.TestData.RenameCalls).To(BeEmpty())
			// The container is recorded as failed in the report.
			Expect(report.Failed()).To(HaveLen(1))
		})

		It("skips the next immediate update because the rollback cooldown is active", func() {
			blue := buildBlueGreenContainer("blue-cooldown-id", "web-cooldown", nil)
			data := &TestData{
				Containers:            []types.Container{blue},
				NextStartContainerIDs: []types.ContainerID{greenID},
				HealthStatusByID: map[types.ContainerID]string{
					greenID: dockerContainer.Unhealthy,
				},
			}
			client := CreateMockClient(data, false, false)

			// First poll: unhealthy green -> rollback -> cooldown armed.
			_, err := actions.Update(client, blueGreenParams)
			Expect(err).NotTo(HaveOccurred())
			firstPollStarts := len(client.TestData.StartedContainers)
			Expect(firstPollStarts).To(Equal(1))

			// Second poll immediately after: the container is on cooldown, so
			// it is never marked stale and no green is started.
			_, err = actions.Update(client, blueGreenParams)
			Expect(err).NotTo(HaveOccurred())
			Expect(client.TestData.StartedContainers).To(HaveLen(firstPollStarts))
		})
	})

	When("the green container has no HEALTHCHECK", func() {
		It("still completes the cutover, relying on the drain window with a warning", func() {
			blue := buildBlueGreenContainer("blue-nohc-id", "web-nohc", nil)
			data := &TestData{
				Containers:            []types.Container{blue},
				NextStartContainerIDs: []types.ContainerID{greenID},
				// Empty status simulates a container with no HEALTHCHECK:
				// waitForHealthy returns errNoHealthcheck.
				HealthStatusByID: map[types.ContainerID]string{
					greenID: "",
				},
			}
			client := CreateMockClient(data, false, false)

			report, err := actions.Update(client, blueGreenParams)
			Expect(err).NotTo(HaveOccurred())

			Expect(client.TestData.StartedContainers).To(HaveLen(1))
			Expect(client.TestData.RenameCalls).To(HaveLen(1))
			Expect(client.TestData.RenameCalls[0].NewName).To(Equal("web-nohc"))
			Expect(report.Failed()).To(BeEmpty())
		})
	})

	When("a blue-green container publishes host ports", func() {
		It("falls back to recreate — no green handoff and no rename happens", func() {
			blue := buildBlueGreenContainer("blue-ports-id", "web-ports", nil)
			// Simulate `-p 8080:8080`: two copies cannot bind the same port,
			// so blue-green is impossible and the container falls back to
			// recreate.
			blue.ContainerInfo().HostConfig.PortBindings = nat.PortMap{
				"8080/tcp": []nat.PortBinding{{HostIP: "", HostPort: "8080"}},
			}
			data := &TestData{
				Containers: []types.Container{blue},
			}
			client := CreateMockClient(data, false, false)

			report, err := actions.Update(client, blueGreenParams)
			Expect(err).NotTo(HaveOccurred())

			// Recreate path: blue is stopped then restarted under its own name,
			// never renamed to a green/canonical handoff.
			Expect(client.TestData.RenameCalls).To(BeEmpty())
			Expect(client.TestData.StartedContainers).To(HaveLen(1))
			Expect(client.TestData.StartedContainers[0].Name()).To(Equal("web-ports"))
			Expect(report.Failed()).To(BeEmpty())
		})
	})

	When("blue-green is opted in per container while the global strategy is recreate", func() {
		It("cuts over only the labeled container and recreates the rest", func() {
			labeled := buildBlueGreenContainer("labeled-id", "labeled", map[string]string{
				"com.centurylinklabs.watchtower.update-strategy": "blue-green",
			})
			plain := buildBlueGreenContainer("plain-id", "plain", nil)
			data := &TestData{
				Containers:            []types.Container{labeled, plain},
				NextStartContainerIDs: []types.ContainerID{greenID},
				HealthStatusByID: map[types.ContainerID]string{
					greenID: dockerContainer.Healthy,
				},
			}
			// Global strategy is recreate; only the labeled container opts in.
			client := CreateMockClient(data, false, false)

			report, err := actions.Update(client, types.UpdateParams{Strategy: types.StrategyRecreate})
			Expect(err).NotTo(HaveOccurred())

			// Exactly one blue-green cutover happened — the labeled container's
			// green was renamed to its canonical name. The plain container took
			// the recreate path and was never renamed.
			Expect(client.TestData.RenameCalls).To(HaveLen(1))
			Expect(client.TestData.RenameCalls[0].ContainerID).To(Equal(greenID))
			Expect(client.TestData.RenameCalls[0].NewName).To(Equal("labeled"))
			Expect(report.Failed()).To(BeEmpty())
		})
	})

	When("the watchtower container itself is labeled blue-green", func() {
		It("never goes blue-green — it takes the self-update recreate path", func() {
			self := CreateMockContainerWithConfig(
				"self-id",
				"/watchtower",
				"openserbia/watchtower:latest",
				true, false, time.Now(),
				&dockerContainer.Config{
					Image: "openserbia/watchtower:latest",
					Labels: map[string]string{
						"com.centurylinklabs.watchtower":                  "true",
						"com.centurylinklabs.watchtower.update-strategy":  "blue-green",
						"com.centurylinklabs.watchtower.blue-green.drain": "0s",
					},
					ExposedPorts: map[nat.Port]struct{}{},
				},
			)
			data := &TestData{
				Containers: []types.Container{self},
			}
			client := CreateMockClient(data, false, false)

			report, err := actions.Update(client, blueGreenParams)
			Expect(err).NotTo(HaveOccurred())

			// Self-update is the rename-and-respawn path: exactly one rename to
			// a random (non-canonical, non-blue-green) name, and the start uses
			// the canonical /watchtower create name — not a temporary green name.
			Expect(client.TestData.StartedContainers).To(HaveLen(1))
			Expect(client.TestData.StartedContainers[0].CreateName()).To(Equal("/watchtower"))
			Expect(client.TestData.RenameCalls).To(HaveLen(1))
			Expect(client.TestData.RenameCalls[0].NewName).NotTo(Equal("watchtower"))
			Expect(report.Failed()).To(BeEmpty())
		})
	})

	When("green is healthy but the old container refuses to stop", func() {
		It("arms the rollback cooldown so the next poll does not re-run the cutover", func() {
			blue := buildBlueGreenContainer("blue-stopfail-id", "web-stopfail", nil)
			data := &TestData{
				Containers:            []types.Container{blue},
				NextStartContainerIDs: []types.ContainerID{greenID},
				HealthStatusByID:      map[types.ContainerID]string{greenID: dockerContainer.Healthy},
				StopContainerErrors:   map[string]error{"web-stopfail": errors.New("daemon refused to stop the old container")},
			}
			client := CreateMockClient(data, false, false)

			// First poll: green comes up healthy, blue fails to stop -> cooldown armed.
			_, err := actions.Update(client, blueGreenParams)
			Expect(err).NotTo(HaveOccurred())
			Expect(client.TestData.StartedContainers).To(HaveLen(1))

			// Second poll immediately after: the container is on cooldown, so no
			// second green is started — no thrash, no orphan accumulation.
			_, err = actions.Update(client, blueGreenParams)
			Expect(err).NotTo(HaveOccurred())
			Expect(client.TestData.StartedContainers).To(HaveLen(1))
		})
	})

	When("--no-restart is set", func() {
		It("never starts a green container", func() {
			blue := buildBlueGreenContainer("blue-norestart-id", "web-norestart", nil)
			data := &TestData{
				Containers:            []types.Container{blue},
				NextStartContainerIDs: []types.ContainerID{greenID},
				HealthStatusByID:      map[types.ContainerID]string{greenID: dockerContainer.Healthy},
			}
			client := CreateMockClient(data, false, false)

			_, err := actions.Update(client, types.UpdateParams{Strategy: types.StrategyBlueGreen, NoRestart: true})
			Expect(err).NotTo(HaveOccurred())
			Expect(client.TestData.StartedContainers).To(BeEmpty())
			Expect(client.TestData.RenameCalls).To(BeEmpty())
		})
	})

	Describe("CleanupOrphanBlueGreen", func() {
		It("removes an orphan green when its canonical container still exists", func() {
			canonical := buildBlueGreenContainer("blue-id", "web", nil)
			orphan := buildBlueGreenContainer("green-id", "web-wt-bluegreen-AbCdEfGh", nil)
			data := &TestData{Containers: []types.Container{canonical, orphan}}
			client := CreateMockClient(data, false, false)

			Expect(actions.CleanupOrphanBlueGreen(client, "")).To(Succeed())

			// Green is removed; the canonical container (blue) is left serving.
			Expect(client.TestData.StoppedContainers).To(HaveLen(1))
			Expect(client.TestData.StoppedContainers[0].Name()).To(Equal("web-wt-bluegreen-AbCdEfGh"))
			Expect(client.TestData.RenameCalls).To(BeEmpty())
		})

		It("promotes an orphan green to the canonical name when no canonical container exists", func() {
			orphan := buildBlueGreenContainer("green-id", "web-wt-bluegreen-AbCdEfGh", nil)
			data := &TestData{Containers: []types.Container{orphan}}
			client := CreateMockClient(data, false, false)

			Expect(actions.CleanupOrphanBlueGreen(client, "")).To(Succeed())

			Expect(client.TestData.StoppedContainers).To(BeEmpty())
			Expect(client.TestData.RenameCalls).To(HaveLen(1))
			Expect(client.TestData.RenameCalls[0].NewName).To(Equal("web"))
		})

		It("ignores containers that are not blue-green temporaries", func() {
			normal := buildBlueGreenContainer("plain-id", "web", nil)
			data := &TestData{Containers: []types.Container{normal}}
			client := CreateMockClient(data, false, false)

			Expect(actions.CleanupOrphanBlueGreen(client, "")).To(Succeed())

			Expect(client.TestData.StoppedContainers).To(BeEmpty())
			Expect(client.TestData.RenameCalls).To(BeEmpty())
		})
	})
})
