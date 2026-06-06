package actions_test

import (
	"time"

	dockerContainer "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/openserbia/watchtower/internal/actions"
	. "github.com/openserbia/watchtower/internal/actions/mocks"
	"github.com/openserbia/watchtower/pkg/container"
	"github.com/openserbia/watchtower/pkg/types"
)

var _ = Describe("Preflight", func() {
	// contains reports whether the required set includes a capability.
	contains := func(ids []container.CapabilityID, want container.CapabilityID) bool {
		for _, id := range ids {
			if id == want {
				return true
			}
		}
		return false
	}

	plainContainer := func(name string) types.Container {
		return CreateMockContainer(name, name, "fake-image:latest", time.Now())
	}

	lifecycleContainer := func(name string) types.Container {
		return CreateMockContainerWithConfig(
			name, name, "fake-image:latest", true, false, time.Now(),
			&dockerContainer.Config{
				Image: "fake-image:latest",
				Labels: map[string]string{
					"com.centurylinklabs.watchtower.lifecycle.pre-update": "/pre.sh",
				},
				ExposedPorts: network.PortSet{},
			},
		)
	}

	Describe("RequiredCapabilities", func() {
		When("running with the default configuration", func() {
			It("requires the base reads, image pull, and the full write set", func() {
				required := actions.RequiredCapabilities(actions.PreflightConfig{}, nil)

				// Base reads.
				Expect(contains(required, container.CapPing)).To(BeTrue())
				Expect(contains(required, container.CapContainerList)).To(BeTrue())
				Expect(contains(required, container.CapContainerInspect)).To(BeTrue())
				Expect(contains(required, container.CapImageInspect)).To(BeTrue())
				// Pull on by default.
				Expect(contains(required, container.CapImagePull)).To(BeTrue())
				// Write set on by default.
				Expect(contains(required, container.CapContainerKill)).To(BeTrue())
				Expect(contains(required, container.CapContainerRemove)).To(BeTrue())
				Expect(contains(required, container.CapContainerCreate)).To(BeTrue())
				Expect(contains(required, container.CapContainerStart)).To(BeTrue())
				Expect(contains(required, container.CapImageTag)).To(BeTrue())
				Expect(contains(required, container.CapNetworkConnect)).To(BeTrue())
				Expect(contains(required, container.CapNetworkDisconnect)).To(BeTrue())
				Expect(contains(required, container.CapContainerRename)).To(BeTrue())
				// Conditional capabilities off by default.
				Expect(contains(required, container.CapImageRemove)).To(BeFalse())
				Expect(contains(required, container.CapContainerWait)).To(BeFalse())
				Expect(contains(required, container.CapContainerExecCreate)).To(BeFalse())
				Expect(contains(required, container.CapEvents)).To(BeFalse())
			})
		})

		When("running monitor-only", func() {
			It("drops the entire write set but keeps the reads and pull", func() {
				required := actions.RequiredCapabilities(
					actions.PreflightConfig{MonitorOnly: true, Cleanup: true, RerunInitDeps: true},
					[]types.Container{lifecycleContainer("svc")},
				)

				Expect(contains(required, container.CapContainerInspect)).To(BeTrue())
				Expect(contains(required, container.CapImagePull)).To(BeTrue())

				for _, write := range []container.CapabilityID{
					container.CapContainerKill,
					container.CapContainerRemove,
					container.CapContainerCreate,
					container.CapContainerStart,
					container.CapImageTag,
					container.CapNetworkConnect,
					container.CapNetworkDisconnect,
					container.CapContainerRename,
					// Cleanup / rerun-init / exec are part of an update and so
					// are meaningless under monitor-only even when their flags
					// (or labels) are present.
					container.CapImageRemove,
					container.CapContainerWait,
					container.CapContainerExecCreate,
				} {
					Expect(contains(required, write)).To(BeFalse(), "monitor-only must not require %s", write)
				}
			})
		})

		When("running with --no-pull", func() {
			It("omits the image-pull capability", func() {
				required := actions.RequiredCapabilities(actions.PreflightConfig{NoPull: true}, nil)
				Expect(contains(required, container.CapImagePull)).To(BeFalse())
				Expect(contains(required, container.CapContainerCreate)).To(BeTrue())
			})
		})

		When("running with --cleanup", func() {
			It("adds the image-remove capability", func() {
				required := actions.RequiredCapabilities(actions.PreflightConfig{Cleanup: true}, nil)
				Expect(contains(required, container.CapImageRemove)).To(BeTrue())
			})
		})

		When("running with --rerun-init-deps", func() {
			It("adds the container-wait capability", func() {
				required := actions.RequiredCapabilities(actions.PreflightConfig{RerunInitDeps: true}, nil)
				Expect(contains(required, container.CapContainerWait)).To(BeTrue())
			})
		})

		When("lifecycle hooks are enabled", func() {
			It("requires exec only when a watched container declares a lifecycle label", func() {
				without := actions.RequiredCapabilities(
					actions.PreflightConfig{LifecycleHooks: true},
					[]types.Container{plainContainer("plain")},
				)
				Expect(contains(without, container.CapContainerExecCreate)).To(BeFalse())

				with := actions.RequiredCapabilities(
					actions.PreflightConfig{LifecycleHooks: true},
					[]types.Container{plainContainer("plain"), lifecycleContainer("hooked")},
				)
				Expect(contains(with, container.CapContainerExecCreate)).To(BeTrue())
			})

			It("does not require exec when no container has a lifecycle label", func() {
				required := actions.RequiredCapabilities(
					actions.PreflightConfig{LifecycleHooks: true},
					[]types.Container{plainContainer("a"), plainContainer("b")},
				)
				Expect(contains(required, container.CapContainerExecCreate)).To(BeFalse())
			})
		})

		When("watching docker events", func() {
			It("includes the optional events capability in the probe set", func() {
				required := actions.RequiredCapabilities(actions.PreflightConfig{WatchDockerEvents: true}, nil)
				Expect(contains(required, container.CapEvents)).To(BeTrue())
			})
		})
	})

	Describe("Preflight probing", func() {
		newClient := func(statuses map[container.CapabilityID]container.ProbeStatus) MockClient {
			return CreateMockClient(&TestData{ProbeStatuses: statuses}, false, false)
		}

		It("succeeds when every required capability is present", func() {
			client := newClient(nil)
			required := actions.RequiredCapabilities(actions.PreflightConfig{}, nil)
			Expect(actions.Preflight(client, required)).To(Succeed())
		})

		It("returns an error naming the endpoint and proxy var when a required capability is blocked", func() {
			client := newClient(map[container.CapabilityID]container.ProbeStatus{
				container.CapContainerCreate: container.StatusBlocked,
			})
			required := actions.RequiredCapabilities(actions.PreflightConfig{}, nil)

			err := actions.Preflight(client, required)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(string(container.CapContainerCreate)))
			Expect(err.Error()).To(ContainSubstring("POST /containers/create"))
			Expect(err.Error()).To(ContainSubstring("CONTAINERS+POST"))
		})

		It("returns an error when a required capability is unreachable", func() {
			client := newClient(map[container.CapabilityID]container.ProbeStatus{
				container.CapPing: container.StatusUnreachable,
			})
			required := actions.RequiredCapabilities(actions.PreflightConfig{}, nil)

			err := actions.Preflight(client, required)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("GET /_ping"))
		})

		It("only warns (no error) when the optional events capability is blocked", func() {
			client := newClient(map[container.CapabilityID]container.ProbeStatus{
				container.CapEvents: container.StatusBlocked,
			})
			required := actions.RequiredCapabilities(actions.PreflightConfig{WatchDockerEvents: true}, nil)

			Expect(actions.Preflight(client, required)).To(Succeed())
		})

		It("still fails on a required capability even when events is also blocked", func() {
			client := newClient(map[container.CapabilityID]container.ProbeStatus{
				container.CapEvents:       container.StatusBlocked,
				container.CapImageInspect: container.StatusBlocked,
			})
			required := actions.RequiredCapabilities(actions.PreflightConfig{WatchDockerEvents: true}, nil)

			err := actions.Preflight(client, required)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("GET /images/{name}/json"))
		})
	})
})
