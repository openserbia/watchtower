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
