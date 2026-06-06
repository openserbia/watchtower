package actions_test

import (
	"bytes"
	"time"

	dockerContainer "github.com/docker/docker/api/types/container"
	"github.com/docker/go-connections/nat"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/sirupsen/logrus"

	"github.com/openserbia/watchtower/internal/actions"
	. "github.com/openserbia/watchtower/internal/actions/mocks"
	"github.com/openserbia/watchtower/pkg/types"
)

var _ = Describe("AuditUnmanaged", func() {
	var logBuf *bytes.Buffer

	BeforeEach(func() {
		actions.ResetAuditStateForTest()
		logBuf = &bytes.Buffer{}
		logrus.SetOutput(logBuf)
		logrus.SetLevel(logrus.InfoLevel)
	})

	AfterEach(func() {
		logrus.SetOutput(GinkgoWriter)
	})

	newMock := func(name string, labels map[string]string) types.Container {
		return CreateMockContainerWithConfig(
			name, name, "fake-image:latest", true, false, time.Now(),
			&dockerContainer.Config{
				Image:        "fake-image:latest",
				Labels:       labels,
				ExposedPorts: map[nat.Port]struct{}{},
			},
		)
	}

	It("treats Docker infrastructure (buildkit) as its own bucket, not unmanaged", func() {
		buildkit := CreateMockContainerWithConfig(
			"buildx_buildkit_default0", "buildx_buildkit_default0",
			"moby/buildkit:v0.12.0", true, false, time.Now(),
			&dockerContainer.Config{
				Image:        "moby/buildkit:v0.12.0",
				Labels:       map[string]string{},
				ExposedPorts: map[nat.Port]struct{}{},
			},
		)
		testData := &TestData{
			Containers: []types.Container{
				buildkit,
				newMock("real-svc", map[string]string{}),
			},
		}
		client := CreateMockClient(testData, false, false)

		Expect(actions.AuditUnmanaged(client, "", true)).To(Succeed())
		Expect(logBuf.String()).NotTo(ContainSubstring("buildx_buildkit"))
		Expect(logBuf.String()).To(ContainSubstring("real-svc"))
	})

	It("warns about containers without the enable label", func() {
		testData := &TestData{
			Containers: []types.Container{
				newMock("unlabeled-svc", map[string]string{}),
				newMock("opted-in", map[string]string{"com.centurylinklabs.watchtower.enable": "true"}),
				newMock("opted-out", map[string]string{"com.centurylinklabs.watchtower.enable": "false"}),
			},
		}
		client := CreateMockClient(testData, false, false)

		Expect(actions.AuditUnmanaged(client, "", true)).To(Succeed())
		Expect(logBuf.String()).To(ContainSubstring("unlabeled-svc"))
		Expect(logBuf.String()).NotTo(ContainSubstring("opted-in"))
		Expect(logBuf.String()).NotTo(ContainSubstring("opted-out"))
	})

	It("does not warn when every container is labeled", func() {
		testData := &TestData{
			Containers: []types.Container{
				newMock("a", map[string]string{"com.centurylinklabs.watchtower.enable": "true"}),
				newMock("b", map[string]string{"com.centurylinklabs.watchtower.enable": "false"}),
			},
		}
		client := CreateMockClient(testData, false, false)

		Expect(actions.AuditUnmanaged(client, "", true)).To(Succeed())
		Expect(logBuf.String()).To(BeEmpty())
	})

	It("stays silent on repeated polls when the unmanaged set is unchanged", func() {
		testData := &TestData{
			Containers: []types.Container{
				newMock("stable-unlabeled", map[string]string{}),
			},
		}
		client := CreateMockClient(testData, false, false)

		Expect(actions.AuditUnmanaged(client, "", true)).To(Succeed())
		Expect(logBuf.String()).To(ContainSubstring("stable-unlabeled"))

		logBuf.Reset()
		Expect(actions.AuditUnmanaged(client, "", true)).To(Succeed())
		Expect(logBuf.String()).To(BeEmpty())
	})

	It("warns only about newly-appeared unmanaged containers on subsequent polls", func() {
		baseline := &TestData{
			Containers: []types.Container{
				newMock("existing-unlabeled", map[string]string{}),
			},
		}
		client := CreateMockClient(baseline, false, false)
		Expect(actions.AuditUnmanaged(client, "", true)).To(Succeed())
		logBuf.Reset()

		// Second poll: a new unmanaged container shows up.
		baseline.Containers = append(baseline.Containers, newMock("newly-deployed", map[string]string{}))
		Expect(actions.AuditUnmanaged(client, "", true)).To(Succeed())
		Expect(logBuf.String()).To(ContainSubstring("newly-deployed"))
		Expect(logBuf.String()).NotTo(ContainSubstring("existing-unlabeled"))
	})

	It("publishes gauges but stays silent when logWarnings is false", func() {
		testData := &TestData{
			Containers: []types.Container{
				newMock("silent-unlabeled", map[string]string{}),
			},
		}
		client := CreateMockClient(testData, false, false)

		Expect(actions.AuditUnmanaged(client, "", false)).To(Succeed())
		Expect(logBuf.String()).To(BeEmpty())
	})

	It("logs once when a previously-unmanaged container gets labeled", func() {
		testData := &TestData{
			Containers: []types.Container{
				newMock("will-be-labeled", map[string]string{}),
			},
		}
		client := CreateMockClient(testData, false, false)
		Expect(actions.AuditUnmanaged(client, "", true)).To(Succeed())
		logBuf.Reset()

		// Second poll: operator added the enable label.
		testData.Containers = []types.Container{
			newMock("will-be-labeled", map[string]string{"com.centurylinklabs.watchtower.enable": "true"}),
		}
		Expect(actions.AuditUnmanaged(client, "", true)).To(Succeed())
		Expect(logBuf.String()).To(ContainSubstring("will-be-labeled"))
		Expect(logBuf.String()).To(ContainSubstring("audit cleared"))
	})
})

var _ = Describe("CleanupOrphanSelf", func() {
	// buildSelfTemp builds a watchtower-labeled container (so the IsWatchtower
	// guard passes) with no compose label, at a given creation time — the shape
	// CleanupOrphanSelf reconciles.
	buildSelfTemp := func(id, name string, created time.Time) types.Container {
		return CreateMockContainerWithConfig(
			id, name, "openserbia/watchtower:latest", true, false, created,
			&dockerContainer.Config{
				Image: "openserbia/watchtower:latest",
				Labels: map[string]string{
					"com.centurylinklabs.watchtower": "true",
				},
				ExposedPorts: map[nat.Port]struct{}{},
			},
		)
	}

	It("promotes a stranded self-temp to its canonical name when no canonical sibling exists (no compose label)", func() {
		orphan := buildSelfTemp("orphan-id", "/watchtower-wt-self-AbCdEfGh", time.Now())
		client := CreateMockClient(&TestData{Containers: []types.Container{orphan}}, false, false)

		Expect(actions.CleanupOrphanSelf(client, "", "")).To(Succeed())

		Expect(client.TestData.StoppedContainers).To(BeEmpty())
		Expect(client.TestData.RenameCalls).To(HaveLen(1))
		Expect(client.TestData.RenameCalls[0].NewName).To(Equal("watchtower"))
	})

	It("removes a stale self-temp when the canonical self is already present", func() {
		canonical := buildSelfTemp("canon-id", "/watchtower", time.Now())
		orphan := buildSelfTemp("orphan-id", "/watchtower-wt-self-AbCdEfGh", time.Now())
		client := CreateMockClient(&TestData{Containers: []types.Container{canonical, orphan}}, false, false)

		Expect(actions.CleanupOrphanSelf(client, "", "")).To(Succeed())

		Expect(client.TestData.RenameCalls).To(BeEmpty())
		Expect(client.TestData.StoppedContainers).To(HaveLen(1))
		Expect(client.TestData.StoppedContainers[0].Name()).To(Equal("/watchtower-wt-self-AbCdEfGh"))
	})

	It("promotes to the container name embedded in the temp, not the compose service", func() {
		// "myproj-watchtower-1-wt-self-XXXX" must promote to "myproj-watchtower-1"
		// (the embedded capture group), proving recovery uses the structured
		// name, not ComposeService() which would yield the bare "watchtower".
		orphan := CreateMockContainerWithConfig(
			"orphan-id", "/myproj-watchtower-1-wt-self-AbCdEfGh", "openserbia/watchtower:latest",
			true, false, time.Now(),
			&dockerContainer.Config{
				Image: "openserbia/watchtower:latest",
				Labels: map[string]string{
					"com.centurylinklabs.watchtower":   "true",
					"com.docker.compose.service":       "watchtower",
					"com.docker.compose.project":       "myproj",
					"com.docker.compose.container-num": "1",
				},
				ExposedPorts: map[nat.Port]struct{}{},
			},
		)
		client := CreateMockClient(&TestData{Containers: []types.Container{orphan}}, false, false)

		Expect(actions.CleanupOrphanSelf(client, "", "")).To(Succeed())

		Expect(client.TestData.RenameCalls).To(HaveLen(1))
		Expect(client.TestData.RenameCalls[0].NewName).To(Equal("myproj-watchtower-1"))
	})

	It("ignores a non-watchtower container that merely matches the -wt-self- name shape", func() {
		app := CreateMockContainerWithConfig(
			"app-id", "/app-wt-self-AbCdEfGh", "fake-image:latest",
			true, false, time.Now(),
			&dockerContainer.Config{
				Image:        "fake-image:latest",
				Labels:       map[string]string{},
				ExposedPorts: map[nat.Port]struct{}{},
			},
		)
		client := CreateMockClient(&TestData{Containers: []types.Container{app}}, false, false)

		Expect(actions.CleanupOrphanSelf(client, "", "")).To(Succeed())

		Expect(client.TestData.RenameCalls).To(BeEmpty())
		Expect(client.TestData.StoppedContainers).To(BeEmpty())
	})

	It("promotes the newest of several self-temps for one canonical and stops the rest", func() {
		older := buildSelfTemp("older-id", "/watchtower-wt-self-AaAaAaAa", time.Now())
		newer := buildSelfTemp("newer-id", "/watchtower-wt-self-BbBbBbBb", time.Now().Add(time.Hour))
		client := CreateMockClient(&TestData{Containers: []types.Container{older, newer}}, false, false)

		Expect(actions.CleanupOrphanSelf(client, "", "")).To(Succeed())

		Expect(client.TestData.RenameCalls).To(HaveLen(1))
		Expect(client.TestData.RenameCalls[0].ContainerID).To(Equal(types.ContainerID("newer-id")))
		Expect(client.TestData.RenameCalls[0].NewName).To(Equal("watchtower"))
		Expect(client.TestData.StoppedContainers).To(HaveLen(1))
		Expect(client.TestData.StoppedContainers[0].Name()).To(Equal("/watchtower-wt-self-AaAaAaAa"))
	})

	It("never stops or renames the live self", func() {
		liveSelf := buildSelfTemp("live-self-id", "/watchtower-wt-self-AbCdEfGh", time.Now())
		client := CreateMockClient(&TestData{Containers: []types.Container{liveSelf}}, false, false)

		Expect(actions.CleanupOrphanSelf(client, "", types.ContainerID("live-self-id"))).To(Succeed())

		Expect(client.TestData.RenameCalls).To(BeEmpty())
		Expect(client.TestData.StoppedContainers).To(BeEmpty())
	})
})
