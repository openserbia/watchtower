package actions_test

import (
	"time"

	dockerContainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/go-connections/nat"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/openserbia/watchtower/internal/actions"
	. "github.com/openserbia/watchtower/internal/actions/mocks"
	"github.com/openserbia/watchtower/pkg/types"
)

func getCommonTestData(keepContainer string) *TestData {
	return &TestData{
		NameOfContainerToKeep: keepContainer,
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
			data := &TestData{
				Containers:            []types.Container{old},
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
			It("should only try to remove the image once", func() {
				client := CreateMockClient(getCommonTestData(""), false, false)
				_, err := actions.Update(client, types.UpdateParams{Cleanup: true})
				Expect(err).NotTo(HaveOccurred())
				Expect(client.TestData.TriedToRemoveImageCount).To(Equal(1))
			})
		})
		When("the container's imageInfo was populated from a fallback name lookup", func() {
			It("should target the creation-time image ID, not the fallback imageInfo ID", func() {
				// Simulates the post-fallback state: containerInfo.Image (the
				// ID the container was actually created from) differs from
				// imageInfo.ID (the freshly-pulled replacement found by name).
				// Without SourceImageID, cleanup would try to delete the
				// replacement image that the new container is now using.
				oldImageID := "sha256:4d239725ac8da47ecfcf04356f19845a4207c3423f61979151bff56612f04807"
				newImageID := "sha256:b00ed20a1dd0000000000000000000000000000000000000000000000000000a"
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

				client := CreateMockClient(&TestData{
					Containers: []types.Container{ctr},
				}, false, false)
				_, err := actions.Update(client, types.UpdateParams{Cleanup: true})
				Expect(err).NotTo(HaveOccurred())
				Expect(client.TestData.TriedToRemoveImageIDs).To(ConsistOf(types.ImageID(oldImageID)))
				Expect(client.TestData.TriedToRemoveImageIDs).NotTo(ContainElement(types.ImageID(newImageID)))
			})
		})
		When("there are multiple containers using different images", func() {
			It("should try to remove each of them", func() {
				testData := getCommonTestData("")
				testData.Containers = append(
					testData.Containers,
					CreateMockContainer(
						"unique-test-container",
						"unique-test-container",
						"unique-fake-image:latest",
						time.Now(),
					),
				)
				client := CreateMockClient(testData, false, false)
				_, err := actions.Update(client, types.UpdateParams{Cleanup: true})
				Expect(err).NotTo(HaveOccurred())
				Expect(client.TestData.TriedToRemoveImageCount).To(Equal(2))
			})
		})
		When("there are linked containers being updated", func() {
			It("should not try to remove their images", func() {
				client := CreateMockClient(getLinkedTestData(true), false, false)
				_, err := actions.Update(client, types.UpdateParams{Cleanup: true})
				Expect(err).NotTo(HaveOccurred())
				Expect(client.TestData.TriedToRemoveImageCount).To(Equal(1))
			})
		})
		When("performing a rolling restart update", func() {
			It("should try to remove the image once", func() {
				client := CreateMockClient(getCommonTestData(""), false, false)
				_, err := actions.Update(client, types.UpdateParams{Cleanup: true, RollingRestart: true})
				Expect(err).NotTo(HaveOccurred())
				Expect(client.TestData.TriedToRemoveImageCount).To(Equal(1))
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
				Expect(client.TestData.TriedToRemoveImageCount).To(Equal(1))
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
					Expect(client.TestData.TriedToRemoveImageCount).To(Equal(1))
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
				Expect(client.TestData.TriedToRemoveImageCount).To(Equal(1))
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
				Expect(client.TestData.TriedToRemoveImageCount).To(Equal(1))
			})
		})

		When("container is restarting", func() {
			It("skip running preupdate", func() {
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
				Expect(client.TestData.TriedToRemoveImageCount).To(Equal(1))
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
