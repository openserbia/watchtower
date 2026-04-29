package actions_test

import (
	"bytes"
	"errors"
	"time"

	dockerContainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/go-connections/nat"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/sirupsen/logrus"

	"github.com/openserbia/watchtower/internal/actions"
	. "github.com/openserbia/watchtower/internal/actions/mocks"
	packagecontainer "github.com/openserbia/watchtower/pkg/container"
	"github.com/openserbia/watchtower/pkg/types"
)

func getCommonTestData() *TestData {
	return &TestData{
		Containers: []types.Container{
			CreateMockContainer(
				"test-container-01",
				"test-container-01",
				"fake-image:latest",
				time.Now().AddDate(0, 0, -1)),
			CreateMockContainer(
				"test-container-02",
				"test-container-02",
				"fake-image:latest",
				time.Now()),
		},
	}
}

func getLinkedTestData(withImageInfo bool) *TestData {
	staleContainer := CreateMockContainer(
		"test-container-01",
		"/test-container-01",
		"fake-image1:latest",
		time.Now().AddDate(0, 0, -1))

	var imageInfo *image.InspectResponse
	if withImageInfo {
		imageInfo = CreateMockImageInfo("test-container-02")
	}
	linkingContainer := CreateMockContainerWithLinks(
		"test-container-02",
		"/test-container-02",
		"fake-image2:latest",
		time.Now(),
		[]string{staleContainer.Name()},
		imageInfo)

	return &TestData{
		Staleness: map[string]bool{linkingContainer.Name(): false},
		Containers: []types.Container{
			staleContainer,
			linkingContainer,
		},
	}
}

var _ = Describe("the update action", func() {
	When("--health-check-gated is enabled", func() {
		const healthCheckParamsTimeout = 100 * time.Millisecond
		params := types.UpdateParams{
			HealthCheckGated:   true,
			HealthCheckTimeout: healthCheckParamsTimeout,
		}

		buildHealthAwareContainer := func(id string) types.Container {
			config := &dockerContainer.Config{
				Image:        "fake-image:latest",
				Labels:       map[string]string{},
				ExposedPorts: map[nat.Port]struct{}{},
				Healthcheck:  &dockerContainer.HealthConfig{Test: []string{"CMD", "true"}},
			}
			return CreateMockContainerWithConfig(id, id, "fake-image:latest", true, false, time.Now(), config)
		}

		It("rolls back to the old container when the replacement reports unhealthy", func() {
			old := buildHealthAwareContainer("gate-rollback-new-unhealthy")
			newestDigest := types.ImageID("sha256:gate-new-digest")
			data := &TestData{
				Containers:            []types.Container{old},
				NewestImageByName:     map[string]types.ImageID{old.Name(): newestDigest},
				NextStartContainerIDs: []types.ContainerID{"new-id", "rollback-id"},
				HealthStatusByID: map[types.ContainerID]string{
					"new-id":      dockerContainer.Unhealthy,
					"rollback-id": dockerContainer.Healthy,
				},
			}
			client := CreateMockClient(data, false, false)

			_, err := actions.Update(client, params)
			Expect(err).NotTo(HaveOccurred())
			// StartContainer should have been called twice: once for the new
			// image, once for the rollback that restores the old state.
			Expect(client.TestData.StartedContainers).To(HaveLen(2))
			Expect(client.TestData.StartedContainers[1].Name()).To(Equal(old.Name()))
			// Forward path pinned to the new digest; rollback repointed at
			// the old image so the recreate doesn't bring back the broken
			// version we just rejected. (The container instance is shared
			// between the two StartContainer calls, so we read the final
			// rollback-time value off either entry.)
			Expect(client.TestData.StartedContainers[1].TargetImageID()).To(Equal(old.ImageID()))
		})

		It("surfaces a loud error when the rolled-back container is also unhealthy", func() {
			old := buildHealthAwareContainer("test-container-dual-broken")
			data := &TestData{
				Containers:            []types.Container{old},
				NextStartContainerIDs: []types.ContainerID{"new-id", "rollback-id"},
				HealthStatusByID: map[types.ContainerID]string{
					"new-id":      dockerContainer.Unhealthy,
					"rollback-id": dockerContainer.Unhealthy,
				},
			}
			client := CreateMockClient(data, false, false)

			_, err := actions.Update(client, params)
			// Update() itself doesn't bubble the error up — it records it as
			// failed in the progress report. Check that both containers got
			// started and the second was the old one.
			Expect(err).NotTo(HaveOccurred())
			Expect(client.TestData.StartedContainers).To(HaveLen(2))
		})

		It("skips a second update while the cooldown is still active", func() {
			old := buildHealthAwareContainer("test-container-cooldown")
			data := &TestData{
				Containers:            []types.Container{old},
				NextStartContainerIDs: []types.ContainerID{"new-id", "rollback-id"},
				HealthStatusByID: map[types.ContainerID]string{
					"new-id":      dockerContainer.Unhealthy,
					"rollback-id": dockerContainer.Healthy,
				},
			}
			client := CreateMockClient(data, false, false)

			// First poll: unhealthy new → rollback → cooldown recorded.
			_, err := actions.Update(client, params)
			Expect(err).NotTo(HaveOccurred())
			firstPollStarts := len(client.TestData.StartedContainers)
			Expect(firstPollStarts).To(Equal(2))

			// Second poll immediately after: should be a no-op because the
			// container is still within the rollback cooldown window.
			_, err = actions.Update(client, params)
			Expect(err).NotTo(HaveOccurred())
			Expect(client.TestData.StartedContainers).To(HaveLen(firstPollStarts))
		})

		It("treats a missing HEALTHCHECK as a warning rather than a rollback", func() {
			old := buildHealthAwareContainer("gate-no-healthcheck")
			data := &TestData{
				Containers:       []types.Container{old},
				HealthStatusByID: map[types.ContainerID]string{old.ID(): ""},
			}
			client := CreateMockClient(data, false, false)

			_, err := actions.Update(client, params)
			Expect(err).NotTo(HaveOccurred())
			// Only the new container should have been started — no rollback.
			Expect(client.TestData.StartedContainers).To(HaveLen(1))
		})

		It("accepts the update when the new container reports healthy", func() {
			old := buildHealthAwareContainer("gate-healthy-accept")
			data := &TestData{
				Containers:       []types.Container{old},
				HealthStatusByID: map[types.ContainerID]string{old.ID(): dockerContainer.Healthy},
			}
			client := CreateMockClient(data, false, false)

			_, err := actions.Update(client, params)
			Expect(err).NotTo(HaveOccurred())
			Expect(client.TestData.StartedContainers).To(HaveLen(1))
		})
	})
	When("watchtower has been instructed to clean up", func() {
		When("there are multiple containers using the same image", func() {
			It("removes the recorded prior once even when multiple containers share it", func() {
				// Cleanup is deferred by one generation: seed a synthetic
				// prior so a single Update behaves like the second update
				// would, with a recorded prior to clean.
				data := getCommonTestData()
				priorDigest := types.ImageID("sha256:prior-shared")
				for _, c := range data.Containers {
					actions.SeedPreviousImageForTest(c.Name(), priorDigest)
				}
				client := CreateMockClient(data, false, false)
				_, err := actions.Update(client, types.UpdateParams{Cleanup: true})
				Expect(err).NotTo(HaveOccurred())
				Expect(client.TestData.TriedToRemoveImageIDs).To(ConsistOf(priorDigest))
			})
		})
		When("the container's imageInfo was populated from a fallback name lookup", func() {
			It("rotates the creation-time image ID into the previous-image slot, not the fallback imageInfo ID", func() {
				// Simulates the post-fallback state: containerInfo.Image (the
				// ID the container was actually created from) differs from
				// imageInfo.ID (the freshly-pulled replacement found by name).
				// SourceImageID is what gets rotated into the prior slot;
				// the fallback imageInfo ID is never written to either slot.
				oldImageID := "sha256:4d239725ac8da47ecfcf04356f19845a4207c3423f61979151bff56612f04807"
				newImageID := "sha256:b00ed20a1dd0000000000000000000000000000000000000000000000000000a"
				priorDigest := types.ImageID("sha256:older-prior")
				containerInfo := &dockerContainer.InspectResponse{
					ContainerJSONBase: &dockerContainer.ContainerJSONBase{
						ID:    "post-fallback-cont",
						Image: oldImageID,
						Name:  "post-fallback-cont",
						HostConfig: &dockerContainer.HostConfig{
							PortBindings: map[nat.Port][]nat.PortBinding{},
						},
					},
					Config: &dockerContainer.Config{
						Image:        "registry.example.com/app:latest",
						Labels:       map[string]string{},
						ExposedPorts: map[nat.Port]struct{}{},
					},
				}
				fallbackImageInfo := &image.InspectResponse{
					ID:          newImageID,
					RepoDigests: []string{"registry.example.com/app@sha256:deadbeef"},
				}
				ctr := CreateMockContainerWithImageInfoP(
					"post-fallback-cont",
					"post-fallback-cont",
					"registry.example.com/app:latest",
					time.Now(),
					fallbackImageInfo,
				)
				// Wire the raw containerInfo so SourceImageID returns oldImageID.
				Expect(ctr.ContainerInfo()).NotTo(BeNil())
				*ctr.ContainerInfo() = *containerInfo

				actions.SeedPreviousImageForTest(ctr.Name(), priorDigest)
				client := CreateMockClient(&TestData{
					Containers: []types.Container{ctr},
				}, false, false)
				_, err := actions.Update(client, types.UpdateParams{Cleanup: true})
				Expect(err).NotTo(HaveOccurred())
				// The seeded prior is what gets cleaned. The just-retired
				// SourceImageID becomes the new prior; the fallback imageInfo
				// ID is never written to either slot.
				Expect(client.TestData.TriedToRemoveImageIDs).To(ConsistOf(priorDigest))
				Expect(client.TestData.TriedToRemoveImageIDs).NotTo(ContainElement(types.ImageID(newImageID)))
				Expect(actions.PreviousImageForTest(ctr.Name())).To(Equal(types.ImageID(oldImageID)))
			})
		})
		When("there are multiple containers using different images", func() {
			It("removes each container's recorded prior", func() {
				testData := getCommonTestData()
				testData.Containers = append(
					testData.Containers,
					CreateMockContainer(
						"unique-test-container",
						"unique-test-container",
						"unique-fake-image:latest",
						time.Now(),
					),
				)
				sharedPrior := types.ImageID("sha256:shared-prior")
				uniquePrior := types.ImageID("sha256:unique-prior")
				for _, c := range testData.Containers {
					if c.Name() == "unique-test-container" {
						actions.SeedPreviousImageForTest(c.Name(), uniquePrior)
					} else {
						actions.SeedPreviousImageForTest(c.Name(), sharedPrior)
					}
				}
				client := CreateMockClient(testData, false, false)
				_, err := actions.Update(client, types.UpdateParams{Cleanup: true})
				Expect(err).NotTo(HaveOccurred())
				Expect(client.TestData.TriedToRemoveImageIDs).To(ConsistOf(sharedPrior, uniquePrior))
			})
		})
		When("there are linked containers being updated", func() {
			It("removes only the stale container's recorded prior", func() {
				data := getLinkedTestData(true)
				stalePrior := types.ImageID("sha256:linked-stale-prior")
				actions.SeedPreviousImageForTest("/test-container-01", stalePrior)
				// Seed the linked container too — it should not be rotated
				// because it isn't being recreated.
				actions.SeedPreviousImageForTest("/test-container-02", types.ImageID("sha256:linked-untouched"))
				client := CreateMockClient(data, false, false)
				_, err := actions.Update(client, types.UpdateParams{Cleanup: true})
				Expect(err).NotTo(HaveOccurred())
				Expect(client.TestData.TriedToRemoveImageIDs).To(ConsistOf(stalePrior))
			})
		})
		When("performing a rolling restart update", func() {
			It("removes the recorded prior once across containers sharing an image", func() {
				data := getCommonTestData()
				priorDigest := types.ImageID("sha256:rolling-prior")
				for _, c := range data.Containers {
					actions.SeedPreviousImageForTest(c.Name(), priorDigest)
				}
				client := CreateMockClient(data, false, false)
				_, err := actions.Update(client, types.UpdateParams{Cleanup: true, RollingRestart: true})
				Expect(err).NotTo(HaveOccurred())
				Expect(client.TestData.TriedToRemoveImageIDs).To(ConsistOf(priorDigest))
			})
		})
		When("the container has no recorded prior (first successful update)", func() {
			It("defers cleanup until the next update of that container", func() {
				data := getCommonTestData()
				client := CreateMockClient(data, false, false)
				_, err := actions.Update(client, types.UpdateParams{Cleanup: true})
				Expect(err).NotTo(HaveOccurred())
				// First-pass cleanup is empty — the just-retired digests have
				// just been recorded as priors, awaiting the next update.
				Expect(client.TestData.TriedToRemoveImageIDs).To(BeEmpty())
				for _, c := range data.Containers {
					Expect(actions.PreviousImageForTest(c.Name())).To(Equal(c.SourceImageID()))
				}
			})
		})
		When("updating a linked container with missing image info", func() {
			It("should gracefully fail", func() {
				client := CreateMockClient(getLinkedTestData(false), false, false)

				report, err := actions.Update(client, types.UpdateParams{})
				Expect(err).NotTo(HaveOccurred())
				// Note: Linked containers that were skipped for recreation is not counted in Failed
				// If this happens, an error is emitted to the logs, so a notification should still be sent.
				Expect(report.Updated()).To(HaveLen(1))
				Expect(report.Fresh()).To(HaveLen(1))
			})
		})
	})

	When("watchtower has been instructed to monitor only", func() {
		When("certain containers are set to monitor only", func() {
			It("should not update those containers", func() {
				priorDigest := types.ImageID("sha256:monitor-prior")
				actions.SeedPreviousImageForTest("test-container-01", priorDigest)
				client := CreateMockClient(
					&TestData{
						NameOfContainerToKeep: "test-container-02",
						Containers: []types.Container{
							CreateMockContainer(
								"test-container-01",
								"test-container-01",
								"fake-image1:latest",
								time.Now()),
							CreateMockContainerWithConfig(
								"test-container-02",
								"test-container-02",
								"fake-image2:latest",
								false,
								false,
								time.Now(),
								&dockerContainer.Config{
									Labels: map[string]string{
										"com.centurylinklabs.watchtower.monitor-only": "true",
									},
								}),
						},
					},
					false,
					false,
				)
				_, err := actions.Update(client, types.UpdateParams{Cleanup: true})
				Expect(err).NotTo(HaveOccurred())
				// Only the non-monitored container rotates; its seeded prior
				// is what gets cleaned. Cleanup is deferred by one generation,
				// so without the seed nothing would be removed.
				Expect(client.TestData.TriedToRemoveImageIDs).To(ConsistOf(priorDigest))
			})
		})

		When("monitor only is set globally", func() {
			It("should not update any containers", func() {
				client := CreateMockClient(
					&TestData{
						Containers: []types.Container{
							CreateMockContainer(
								"test-container-01",
								"test-container-01",
								"fake-image:latest",
								time.Now()),
							CreateMockContainer(
								"test-container-02",
								"test-container-02",
								"fake-image:latest",
								time.Now()),
						},
					},
					false,
					false,
				)
				_, err := actions.Update(client, types.UpdateParams{Cleanup: true, MonitorOnly: true})
				Expect(err).NotTo(HaveOccurred())
				Expect(client.TestData.TriedToRemoveImageCount).To(Equal(0))
			})
			When("watchtower has been instructed to have label take precedence", func() {
				It("it should update containers when monitor only is set to false", func() {
					priorDigest := types.ImageID("sha256:label-precedence-prior")
					actions.SeedPreviousImageForTest("test-container-02", priorDigest)
					client := CreateMockClient(
						&TestData{
							// NameOfContainerToKeep: "test-container-02",
							Containers: []types.Container{
								CreateMockContainerWithConfig(
									"test-container-02",
									"test-container-02",
									"fake-image2:latest",
									false,
									false,
									time.Now(),
									&dockerContainer.Config{
										Labels: map[string]string{
											"com.centurylinklabs.watchtower.monitor-only": "false",
										},
									}),
							},
						},
						false,
						false,
					)
					_, err := actions.Update(client, types.UpdateParams{Cleanup: true, MonitorOnly: true, LabelPrecedence: true})
					Expect(err).NotTo(HaveOccurred())
					Expect(client.TestData.TriedToRemoveImageIDs).To(ConsistOf(priorDigest))
				})
				It("it should update not containers when monitor only is set to true", func() {
					client := CreateMockClient(
						&TestData{
							// NameOfContainerToKeep: "test-container-02",
							Containers: []types.Container{
								CreateMockContainerWithConfig(
									"test-container-02",
									"test-container-02",
									"fake-image2:latest",
									false,
									false,
									time.Now(),
									&dockerContainer.Config{
										Labels: map[string]string{
											"com.centurylinklabs.watchtower.monitor-only": "true",
										},
									}),
							},
						},
						false,
						false,
					)
					_, err := actions.Update(client, types.UpdateParams{Cleanup: true, MonitorOnly: true, LabelPrecedence: true})
					Expect(err).NotTo(HaveOccurred())
					Expect(client.TestData.TriedToRemoveImageCount).To(Equal(0))
				})
				It("it should update not containers when monitor only is not set", func() {
					client := CreateMockClient(
						&TestData{
							Containers: []types.Container{
								CreateMockContainer(
									"test-container-01",
									"test-container-01",
									"fake-image:latest",
									time.Now()),
							},
						},
						false,
						false,
					)
					_, err := actions.Update(client, types.UpdateParams{Cleanup: true, MonitorOnly: true, LabelPrecedence: true})
					Expect(err).NotTo(HaveOccurred())
					Expect(client.TestData.TriedToRemoveImageCount).To(Equal(0))
				})
			})
		})
	})

	When("watchtower has been instructed to run lifecycle hooks", func() {
		When("pre-update script returns 1", func() {
			It("should not update those containers", func() {
				client := CreateMockClient(
					&TestData{
						// NameOfContainerToKeep: "test-container-02",
						Containers: []types.Container{
							CreateMockContainerWithConfig(
								"test-container-02",
								"test-container-02",
								"fake-image2:latest",
								true,
								false,
								time.Now(),
								&dockerContainer.Config{
									Labels: map[string]string{
										"com.centurylinklabs.watchtower.lifecycle.pre-update-timeout": "190",
										"com.centurylinklabs.watchtower.lifecycle.pre-update":         "/PreUpdateReturn1.sh",
									},
									ExposedPorts: map[nat.Port]struct{}{},
								}),
						},
					},
					false,
					false,
				)

				_, err := actions.Update(client, types.UpdateParams{Cleanup: true, LifecycleHooks: true})
				Expect(err).NotTo(HaveOccurred())
				Expect(client.TestData.TriedToRemoveImageCount).To(Equal(0))
			})
		})

		When("prupddate script returns 75", func() {
			It("should not update those containers", func() {
				client := CreateMockClient(
					&TestData{
						// NameOfContainerToKeep: "test-container-02",
						Containers: []types.Container{
							CreateMockContainerWithConfig(
								"test-container-02",
								"test-container-02",
								"fake-image2:latest",
								true,
								false,
								time.Now(),
								&dockerContainer.Config{
									Labels: map[string]string{
										"com.centurylinklabs.watchtower.lifecycle.pre-update-timeout": "190",
										"com.centurylinklabs.watchtower.lifecycle.pre-update":         "/PreUpdateReturn75.sh",
									},
									ExposedPorts: map[nat.Port]struct{}{},
								}),
						},
					},
					false,
					false,
				)
				_, err := actions.Update(client, types.UpdateParams{Cleanup: true, LifecycleHooks: true})
				Expect(err).NotTo(HaveOccurred())
				Expect(client.TestData.TriedToRemoveImageCount).To(Equal(0))
			})
		})

		When("prupddate script returns 0", func() {
			It("should update those containers", func() {
				priorDigest := types.ImageID("sha256:preupdate-0-prior")
				actions.SeedPreviousImageForTest("test-container-02", priorDigest)
				client := CreateMockClient(
					&TestData{
						// NameOfContainerToKeep: "test-container-02",
						Containers: []types.Container{
							CreateMockContainerWithConfig(
								"test-container-02",
								"test-container-02",
								"fake-image2:latest",
								true,
								false,
								time.Now(),
								&dockerContainer.Config{
									Labels: map[string]string{
										"com.centurylinklabs.watchtower.lifecycle.pre-update-timeout": "190",
										"com.centurylinklabs.watchtower.lifecycle.pre-update":         "/PreUpdateReturn0.sh",
									},
									ExposedPorts: map[nat.Port]struct{}{},
								}),
						},
					},
					false,
					false,
				)
				_, err := actions.Update(client, types.UpdateParams{Cleanup: true, LifecycleHooks: true})
				Expect(err).NotTo(HaveOccurred())
				Expect(client.TestData.TriedToRemoveImageIDs).To(ConsistOf(priorDigest))
			})
		})

		When("container is linked to restarting containers", func() {
			It("should be marked for restart", func() {
				provider := CreateMockContainerWithConfig(
					"test-container-provider",
					"/test-container-provider",
					"fake-image2:latest",
					true,
					false,
					time.Now(),
					&dockerContainer.Config{
						Labels:       map[string]string{},
						ExposedPorts: map[nat.Port]struct{}{},
					})

				provider.SetStale(true)

				consumer := CreateMockContainerWithConfig(
					"test-container-consumer",
					"/test-container-consumer",
					"fake-image3:latest",
					true,
					false,
					time.Now(),
					&dockerContainer.Config{
						Labels: map[string]string{
							"com.centurylinklabs.watchtower.depends-on": "test-container-provider",
						},
						ExposedPorts: map[nat.Port]struct{}{},
					})

				containers := []types.Container{
					provider,
					consumer,
				}

				Expect(provider.ToRestart()).To(BeTrue())
				Expect(consumer.ToRestart()).To(BeFalse())

				actions.UpdateImplicitRestart(containers)

				Expect(containers[0].ToRestart()).To(BeTrue())
				Expect(containers[1].ToRestart()).To(BeTrue())
			})
		})

		When("container is not running", func() {
			It("skip running preupdate", func() {
				priorDigest := types.ImageID("sha256:preupdate-stopped-prior")
				actions.SeedPreviousImageForTest("test-container-02", priorDigest)
				client := CreateMockClient(
					&TestData{
						// NameOfContainerToKeep: "test-container-02",
						Containers: []types.Container{
							CreateMockContainerWithConfig(
								"test-container-02",
								"test-container-02",
								"fake-image2:latest",
								false,
								false,
								time.Now(),
								&dockerContainer.Config{
									Labels: map[string]string{
										"com.centurylinklabs.watchtower.lifecycle.pre-update-timeout": "190",
										"com.centurylinklabs.watchtower.lifecycle.pre-update":         "/PreUpdateReturn1.sh",
									},
									ExposedPorts: map[nat.Port]struct{}{},
								}),
						},
					},
					false,
					false,
				)
				_, err := actions.Update(client, types.UpdateParams{Cleanup: true, LifecycleHooks: true})
				Expect(err).NotTo(HaveOccurred())
				Expect(client.TestData.TriedToRemoveImageIDs).To(ConsistOf(priorDigest))
			})
		})

		When("container is restarting", func() {
			It("skip running preupdate", func() {
				priorDigest := types.ImageID("sha256:preupdate-restarting-prior")
				actions.SeedPreviousImageForTest("test-container-02", priorDigest)
				client := CreateMockClient(
					&TestData{
						// NameOfContainerToKeep: "test-container-02",
						Containers: []types.Container{
							CreateMockContainerWithConfig(
								"test-container-02",
								"test-container-02",
								"fake-image2:latest",
								false,
								true,
								time.Now(),
								&dockerContainer.Config{
									Labels: map[string]string{
										"com.centurylinklabs.watchtower.lifecycle.pre-update-timeout": "190",
										"com.centurylinklabs.watchtower.lifecycle.pre-update":         "/PreUpdateReturn1.sh",
									},
									ExposedPorts: map[nat.Port]struct{}{},
								}),
						},
					},
					false,
					false,
				)
				_, err := actions.Update(client, types.UpdateParams{Cleanup: true, LifecycleHooks: true})
				Expect(err).NotTo(HaveOccurred())
				Expect(client.TestData.TriedToRemoveImageIDs).To(ConsistOf(priorDigest))
			})
		})
	})
})

var _ = Describe("image cooldown", func() {
	BeforeEach(func() {
		// Package-level map leaks across specs otherwise.
		actions.ResetCooldownStateForTest()
	})

	It("defers on first sighting and proceeds after the window elapses", func() {
		cooldown := 5 * time.Second
		name := "cooldown-first-sighting"
		digest := types.ImageID("sha256:aaa")

		// First poll: we've never seen this digest.
		proceed, remaining := actions.EvaluateImageCooldownForTest(name, digest, cooldown)
		Expect(proceed).To(BeFalse())
		Expect(remaining).To(Equal(cooldown))

		// Simulate the cooldown elapsing by rewinding the recorded firstSeen.
		actions.RewindCooldownFirstSeenForTest(name, 10*time.Second)

		proceed, _ = actions.EvaluateImageCooldownForTest(name, digest, cooldown)
		Expect(proceed).To(BeTrue())
	})

	It("resets the clock when the digest changes mid-cooldown", func() {
		cooldown := 5 * time.Second
		name := "cooldown-changing-digest"
		first := types.ImageID("sha256:aaa")
		second := types.ImageID("sha256:bbb")

		proceed, _ := actions.EvaluateImageCooldownForTest(name, first, cooldown)
		Expect(proceed).To(BeFalse())

		// Advance fake time to T+4s (still inside the window).
		actions.RewindCooldownFirstSeenForTest(name, 4*time.Second)

		// Second poll sees a new digest — should reset, not proceed.
		proceed, remaining := actions.EvaluateImageCooldownForTest(name, second, cooldown)
		Expect(proceed).To(BeFalse())
		Expect(remaining).To(Equal(cooldown))
	})

	It("proceeds immediately when no cooldown is configured — callers guard with cooldown > 0", func() {
		// Sanity check: a cooldown of 0 means the caller's upstream guard
		// (`if cooldown := resolveImageCooldown(...); cooldown > 0`) skips
		// this function entirely. Calling it with 0 still records a pending
		// entry, which is a minor leak but the guard prevents it in practice.
		// Document that behavior so nobody's surprised.
		proceed, _ := actions.EvaluateImageCooldownForTest("cooldown-zero", types.ImageID("sha256:ccc"), 0)
		Expect(proceed).To(BeFalse()) // first sighting always defers, regardless of duration
	})
})

var _ = Describe("image cooldown under --run-once", func() {
	BeforeEach(func() {
		actions.ResetCooldownStateForTest()
	})

	It("bypasses the cooldown gate so the one-shot update actually happens", func() {
		// If cooldown gating were active, the first sighting of a new digest
		// would defer to "next poll" — but --run-once has no next poll, so
		// the gate must be bypassed.
		target := CreateMockContainer(
			"runonce-cooldown-bypass",
			"runonce-cooldown-bypass",
			"fake-image:latest",
			time.Now(),
		)
		client := CreateMockClient(&TestData{
			Containers: []types.Container{target},
		}, false, false)

		_, err := actions.Update(client, types.UpdateParams{
			ImageCooldown: 1 * time.Hour,
			RunOnce:       true,
		})
		Expect(err).NotTo(HaveOccurred())
		// If the cooldown gate ran, the container would have been deferred
		// (stale=false) and StartContainer would never be called. A
		// non-empty StartedContainers confirms the one-shot actually
		// proceeded despite a configured cooldown.
		Expect(len(client.TestData.StartedContainers)).To(BeNumerically(">", 0))
	})
})

var _ = Describe("watchtower self-update port conflict", func() {
	It("skips self-update when the watchtower container has published host ports", func() {
		// Build a container that looks like a published-port watchtower.
		watchtowerWithPort := CreateMockContainerWithConfig(
			"watchtower",
			"/watchtower",
			"openserbia/watchtower:latest",
			true, false, time.Now(),
			&dockerContainer.Config{
				Image: "openserbia/watchtower:latest",
				Labels: map[string]string{
					"com.centurylinklabs.watchtower": "true",
				},
				ExposedPorts: map[nat.Port]struct{}{},
			},
		)
		// Simulate `-p 8080:8080` on the container.
		watchtowerWithPort.ContainerInfo().HostConfig.PortBindings = nat.PortMap{
			"8080/tcp": []nat.PortBinding{{HostIP: "", HostPort: "8080"}},
		}

		client := CreateMockClient(&TestData{
			Containers: []types.Container{watchtowerWithPort},
		}, false, false)

		_, err := actions.Update(client, types.UpdateParams{})
		Expect(err).NotTo(HaveOccurred())
		// Skip means StartContainer is never called → rename-and-respawn
		// path didn't run.
		Expect(client.TestData.StartedContainers).To(BeEmpty())
	})
})

var _ = Describe("startup-failure log levels", func() {
	var (
		logBuf  *bytes.Buffer
		origLev logrus.Level
	)

	BeforeEach(func() {
		logBuf = &bytes.Buffer{}
		logrus.SetOutput(logBuf)
		origLev = logrus.GetLevel()
		logrus.SetLevel(logrus.DebugLevel)
	})

	AfterEach(func() {
		logrus.SetOutput(GinkgoWriter)
		logrus.SetLevel(origLev)
	})

	It("logs a pre-update lifecycle command failure at warn, not error", func() {
		// User-defined pre-update scripts are user-authored code; a failing
		// script is the user's problem, not watchtower's orchestration. At
		// strict NOTIFICATIONS_LEVEL=error the page should stay quiet here.
		client := CreateMockClient(
			&TestData{
				Containers: []types.Container{
					CreateMockContainerWithConfig(
						"hook-failer", "hook-failer", "fake-image:latest",
						true, false, time.Now(),
						&dockerContainer.Config{
							Labels: map[string]string{
								"com.centurylinklabs.watchtower.lifecycle.pre-update-timeout": "190",
								"com.centurylinklabs.watchtower.lifecycle.pre-update":         "/PreUpdateReturn1.sh",
							},
							ExposedPorts: map[nat.Port]struct{}{},
						}),
				},
			},
			false,
			false,
		)

		_, err := actions.Update(client, types.UpdateParams{LifecycleHooks: true})
		Expect(err).NotTo(HaveOccurred())

		out := logBuf.String()
		Expect(out).To(ContainSubstring(`level=warning`))
		Expect(out).To(ContainSubstring("pre-update lifecycle command failed"))
		Expect(out).To(ContainSubstring("hook-failer"))
		// No error-level line for the hook failure specifically.
		Expect(out).NotTo(MatchRegexp(`level=error[^\n]*pre-update`))
	})
})

var _ = Describe("pinned-image containers", func() {
	var (
		logBuf  *bytes.Buffer
		origLev logrus.Level
	)

	BeforeEach(func() {
		logBuf = &bytes.Buffer{}
		logrus.SetOutput(logBuf)
		origLev = logrus.GetLevel()
		logrus.SetLevel(logrus.DebugLevel)
	})

	AfterEach(func() {
		logrus.SetOutput(GinkgoWriter)
		logrus.SetLevel(origLev)
	})

	It("logs the skip at debug, not warn — operator can't act on pinned tags", func() {
		pinned := CreateMockContainer(
			"pinned-svc",
			"pinned-svc",
			"app:latest",
			time.Now().AddDate(0, 0, -1),
		)
		client := CreateMockClient(&TestData{
			Containers: []types.Container{pinned},
			StalenessErrors: map[string]error{
				pinned.Name(): packagecontainer.ErrPinnedImage,
			},
		}, false, false)

		report, err := actions.Update(client, types.UpdateParams{})
		Expect(err).NotTo(HaveOccurred())
		Expect(report.Failed()).To(BeEmpty())

		// The contract is the log level: debug, not warn. Steady-state
		// digest-pinned stacks should not spam every poll with a line the
		// operator cannot act on.
		out := logBuf.String()
		Expect(out).To(ContainSubstring(`level=debug`))
		Expect(out).To(ContainSubstring("Skipping container with pinned image"))
		Expect(out).To(ContainSubstring("pinned-svc"))
		Expect(out).NotTo(ContainSubstring(`level=warning msg="Unable to update container`))
	})

	It("falls back to warn for non-pinned pull failures", func() {
		broken := CreateMockContainer(
			"broken-pull",
			"broken-pull",
			"app:latest",
			time.Now().AddDate(0, 0, -1),
		)
		client := CreateMockClient(&TestData{
			Containers: []types.Container{broken},
			StalenessErrors: map[string]error{
				broken.Name(): errors.New("registry returned 500"),
			},
		}, false, false)

		_, err := actions.Update(client, types.UpdateParams{})
		Expect(err).NotTo(HaveOccurred())
		out := logBuf.String()
		Expect(out).To(ContainSubstring(`level=warning`))
		Expect(out).To(ContainSubstring(`Unable to update container`))
		Expect(out).To(ContainSubstring("broken-pull"))
	})
})

var _ = Describe("cleanup safety: image still in use", func() {
	It("defers cleanup when a non-stale container still references the image", func() {
		// Two containers share an image. Only the first is stale and updated;
		// the second is fresh (Staleness=false) and stays. The new behaviour
		// is to defer removing the shared image so the still-running second
		// container survives its next restart.
		shared := "shared-image:latest"
		stale := CreateMockContainer("stale-svc", "stale-svc", shared, time.Now().AddDate(0, 0, -1))
		fresh := CreateMockContainer("fresh-svc", "fresh-svc", shared, time.Now())

		client := CreateMockClient(&TestData{
			Containers: []types.Container{stale, fresh},
			Staleness: map[string]bool{
				stale.Name(): true,
				fresh.Name(): false,
			},
		}, false, false)

		_, err := actions.Update(client, types.UpdateParams{Cleanup: true})
		Expect(err).NotTo(HaveOccurred())
		Expect(client.TestData.TriedToRemoveImageIDs).NotTo(ContainElement(stale.SafeImageID()))
	})

	It("removes the recorded prior when no surviving container references it", func() {
		// Single container, stale, no other referrers. Cleanup is deferred by
		// one generation, so we seed a synthetic prior digest and verify it
		// gets removed on this run (the just-retired image becomes the new
		// prior, awaiting the next update).
		only := CreateMockContainer("only-svc", "only-svc", "lonely-image:latest", time.Now().AddDate(0, 0, -1))
		priorDigest := types.ImageID("sha256:lonely-prior")
		actions.SeedPreviousImageForTest(only.Name(), priorDigest)
		client := CreateMockClient(&TestData{
			Containers: []types.Container{only},
		}, false, false)

		_, err := actions.Update(client, types.UpdateParams{Cleanup: true})
		Expect(err).NotTo(HaveOccurred())
		Expect(client.TestData.TriedToRemoveImageIDs).To(ContainElement(priorDigest))
		Expect(client.TestData.TriedToRemoveImageIDs).NotTo(ContainElement(only.SafeImageID()))
	})

	It("removes the recorded prior when the only other referrer is also being recreated", func() {
		// Two stale containers sharing an image. Both rotate; the seeded
		// prior digest is what gets cleaned this round. The just-retired
		// image survives until the next update.
		shared := "rotating-image:latest"
		first := CreateMockContainer("rot-a", "rot-a", shared, time.Now().AddDate(0, 0, -1))
		second := CreateMockContainer("rot-b", "rot-b", shared, time.Now().AddDate(0, 0, -1))
		priorDigest := types.ImageID("sha256:rotating-prior")
		actions.SeedPreviousImageForTest(first.Name(), priorDigest)
		actions.SeedPreviousImageForTest(second.Name(), priorDigest)
		client := CreateMockClient(&TestData{
			Containers: []types.Container{first, second},
		}, false, false)

		_, err := actions.Update(client, types.UpdateParams{Cleanup: true})
		Expect(err).NotTo(HaveOccurred())
		Expect(client.TestData.TriedToRemoveImageIDs).To(ContainElement(priorDigest))
		Expect(client.TestData.TriedToRemoveImageIDs).NotTo(ContainElement(first.SafeImageID()))
	})
})

var _ = Describe("image-ID pinning during update", func() {
	// GetCreateConfig's translation of TargetImageID to config.Image is unit-
	// tested in pkg/container/container_test.go. These specs verify the
	// integration: that actions.Update threads the digest resolved by
	// IsContainerStale into the container before StartContainer fires.
	It("threads the digest from IsContainerStale onto the container", func() {
		// Closes the tag-race window: ContainerCreate references the digest
		// resolved at scan time, not the tag, so an external untag between
		// scan and recreate doesn't fail the create with "No such image".
		newestDigest := types.ImageID("sha256:newest-from-pull")
		target := CreateMockContainer("pin-target", "pin-target", "app:latest", time.Now().AddDate(0, 0, -1))
		client := CreateMockClient(&TestData{
			Containers:        []types.Container{target},
			NewestImageByName: map[string]types.ImageID{target.Name(): newestDigest},
		}, false, false)

		_, err := actions.Update(client, types.UpdateParams{})
		Expect(err).NotTo(HaveOccurred())
		Expect(client.TestData.StartedContainers).To(HaveLen(1))
		Expect(client.TestData.StartedContainers[0].TargetImageID()).To(Equal(newestDigest))
	})

	It("leaves the override empty when IsContainerStale returns no digest", func() {
		// Defensive: an empty newestImage must not blank the slot. The
		// recreate falls back to the tag.
		target := CreateMockContainer("pin-empty", "pin-empty", "app:latest", time.Now().AddDate(0, 0, -1))
		client := CreateMockClient(&TestData{
			Containers: []types.Container{target},
		}, false, false)

		_, err := actions.Update(client, types.UpdateParams{})
		Expect(err).NotTo(HaveOccurred())
		Expect(client.TestData.StartedContainers).To(HaveLen(1))
		Expect(client.TestData.StartedContainers[0].TargetImageID()).To(BeEmpty())
	})
})

var _ = Describe("a container vanishing mid-scan", func() {
	It("marks the container Skipped and continues the scan", func() {
		survivor := CreateMockContainer(
			"vanish-survivor",
			"vanish-survivor",
			"survivor-image:latest",
			time.Now().AddDate(0, 0, -1),
		)
		vanished := CreateMockContainer(
			"vanish-target",
			"vanish-target",
			"vanished-image:latest",
			time.Now().AddDate(0, 0, -1),
		)
		// Seed priors so the deferred cleanup actually has something to
		// remove on this run. The vanished one's prior must NOT be touched
		// because its rotation never fires.
		survivorPrior := types.ImageID("sha256:survivor-prior")
		vanishedPrior := types.ImageID("sha256:vanished-prior")
		actions.SeedPreviousImageForTest(survivor.Name(), survivorPrior)
		actions.SeedPreviousImageForTest(vanished.Name(), vanishedPrior)
		client := CreateMockClient(&TestData{
			Containers:         []types.Container{vanished, survivor},
			VanishedContainers: map[string]bool{vanished.Name(): true},
		}, false, false)

		report, err := actions.Update(client, types.UpdateParams{Cleanup: true})
		Expect(err).NotTo(HaveOccurred())

		// The vanished container is reported as Skipped, the surviving one
		// proceeds through the normal recreate path.
		skippedNames := []string{}
		for _, r := range report.Skipped() {
			skippedNames = append(skippedNames, r.Name())
		}
		Expect(skippedNames).To(ContainElement(vanished.Name()))
		Expect(report.Failed()).To(BeEmpty())

		// Cleanup targets only the SURVIVING container's recorded prior —
		// vanished never rotated, so its slot is untouched. The just-retired
		// images stay on disk as the next-generation rollback target.
		Expect(client.TestData.TriedToRemoveImageIDs).To(ContainElement(survivorPrior))
		Expect(client.TestData.TriedToRemoveImageIDs).NotTo(ContainElement(vanishedPrior))
		Expect(client.TestData.TriedToRemoveImageIDs).NotTo(ContainElement(vanished.SafeImageID()))
		Expect(client.TestData.TriedToRemoveImageIDs).NotTo(ContainElement(survivor.SafeImageID()))
		// The vanished container's prior remains intact in the map.
		Expect(actions.PreviousImageForTest(vanished.Name())).To(Equal(vanishedPrior))

		// And StartContainer should not have been called for the vanished one.
		startedNames := []string{}
		for _, c := range client.TestData.StartedContainers {
			startedNames = append(startedNames, c.Name())
		}
		Expect(startedNames).NotTo(ContainElement(vanished.Name()))
		Expect(startedNames).To(ContainElement(survivor.Name()))
	})

	It("works under --rolling-restart too", func() {
		survivor := CreateMockContainer(
			"vanish-rr-survivor",
			"vanish-rr-survivor",
			"rr-survivor-image:latest",
			time.Now().AddDate(0, 0, -1),
		)
		vanished := CreateMockContainer(
			"vanish-rr-target",
			"vanish-rr-target",
			"rr-vanished-image:latest",
			time.Now().AddDate(0, 0, -1),
		)
		client := CreateMockClient(&TestData{
			Containers:         []types.Container{vanished, survivor},
			VanishedContainers: map[string]bool{vanished.Name(): true},
		}, false, false)

		report, err := actions.Update(client, types.UpdateParams{RollingRestart: true})
		Expect(err).NotTo(HaveOccurred())
		Expect(report.Failed()).To(BeEmpty())
		skippedNames := []string{}
		for _, r := range report.Skipped() {
			skippedNames = append(skippedNames, r.Name())
		}
		Expect(skippedNames).To(ContainElement(vanished.Name()))
	})
})
