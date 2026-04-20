package actions_test

import (
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/sirupsen/logrus"

	"github.com/openserbia/watchtower/internal/actions"
	. "github.com/openserbia/watchtower/internal/actions/mocks"
	"github.com/openserbia/watchtower/pkg/types"
)

func TestActions(t *testing.T) {
	RegisterFailHandler(Fail)
	logrus.SetOutput(GinkgoWriter)
	RunSpecs(t, "Actions Suite")
}

var _ = Describe("the actions package", func() {
	Describe("the check prerequisites method", func() {
		When("given an empty array", func() {
			It("should not do anything", func() {
				client := CreateMockClient(
					&TestData{},
					// pullImages:
					false,
					// removeVolumes:
					false,
				)
				Expect(actions.CheckForMultipleWatchtowerInstances(client, false, "")).To(Succeed())
			})
		})
		When("given an array of one", func() {
			It("should not do anything", func() {
				client := CreateMockClient(
					&TestData{
						Containers: []types.Container{
							CreateMockContainer(
								"test-container",
								"test-container",
								"watchtower",
								time.Now()),
						},
					},
					// pullImages:
					false,
					// removeVolumes:
					false,
				)
				Expect(actions.CheckForMultipleWatchtowerInstances(client, false, "")).To(Succeed())
			})
		})
		When("given multiple containers", func() {
			var client MockClient
			BeforeEach(func() {
				client = CreateMockClient(
					&TestData{
						NameOfContainerToKeep: "test-container-02",
						Containers: []types.Container{
							CreateMockContainer(
								"test-container-01",
								"test-container-01",
								"watchtower",
								time.Now().AddDate(0, 0, -1)),
							CreateMockContainer(
								"test-container-02",
								"test-container-02",
								"watchtower",
								time.Now()),
						},
					},
					// pullImages:
					false,
					// removeVolumes:
					false,
				)
			})

			It("should stop all but the latest one", func() {
				err := actions.CheckForMultipleWatchtowerInstances(client, false, "")
				Expect(err).NotTo(HaveOccurred())
			})
		})
		When("deciding whether to cleanup images", func() {
			It("should delete the old image when the kept watchtower runs a different one", func() {
				client := CreateMockClient(
					&TestData{
						Containers: []types.Container{
							CreateMockContainer(
								"test-container-01",
								"test-container-01",
								"watchtower:old",
								time.Now().AddDate(0, 0, -1)),
							CreateMockContainer(
								"test-container-02",
								"test-container-02",
								"watchtower:new",
								time.Now()),
						},
					},
					false,
					false,
				)
				err := actions.CheckForMultipleWatchtowerInstances(client, true, "")
				Expect(err).NotTo(HaveOccurred())
				Expect(client.TestData.TriedToRemoveImage()).To(BeTrue())
			})
			It("should skip cleanup when the kept watchtower runs the same image", func() {
				// Force-removing the shared image would yank it out from under
				// the surviving instance and break its next restart.
				client := CreateMockClient(
					&TestData{
						Containers: []types.Container{
							CreateMockContainer(
								"test-container-01",
								"test-container-01",
								"watchtower",
								time.Now().AddDate(0, 0, -1)),
							CreateMockContainer(
								"test-container-02",
								"test-container-02",
								"watchtower",
								time.Now()),
						},
					},
					false,
					false,
				)
				err := actions.CheckForMultipleWatchtowerInstances(client, true, "")
				Expect(err).NotTo(HaveOccurred())
				Expect(client.TestData.TriedToRemoveImage()).To(BeFalse())
			})
			It("should not try to delete the image if the cleanup flag is false", func() {
				client := CreateMockClient(
					&TestData{
						Containers: []types.Container{
							CreateMockContainer(
								"test-container-01",
								"test-container-01",
								"watchtower:old",
								time.Now().AddDate(0, 0, -1)),
							CreateMockContainer(
								"test-container-02",
								"test-container-02",
								"watchtower:new",
								time.Now()),
						},
					},
					false,
					false,
				)
				err := actions.CheckForMultipleWatchtowerInstances(client, false, "")
				Expect(err).NotTo(HaveOccurred())
				Expect(client.TestData.TriedToRemoveImage()).To(BeFalse())
			})
		})
	})
})
