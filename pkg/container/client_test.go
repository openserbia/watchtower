package container

import (
	"context"
	"errors"
	"net/http"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/backend"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	cli "github.com/docker/docker/client"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gbytes"
	"github.com/onsi/gomega/ghttp"
	gt "github.com/onsi/gomega/types"
	"github.com/sirupsen/logrus"

	"github.com/openserbia/watchtower/internal/util"
	"github.com/openserbia/watchtower/pkg/container/mocks"
	"github.com/openserbia/watchtower/pkg/filters"
	t "github.com/openserbia/watchtower/pkg/types"
)

var _ = Describe("the client", func() {
	var docker *cli.Client
	var mockServer *ghttp.Server
	BeforeEach(func() {
		mockServer = ghttp.NewServer()
		docker, _ = cli.NewClientWithOpts(
			cli.WithHost(mockServer.URL()),
			cli.WithHTTPClient(mockServer.HTTPTestServer.Client()),
			// Pin to the API version NewClient opportunistically upgrades
			// to in production so the Identity-decoding path exercised by
			// the GetContainer tests below matches real daemon behavior.
			// Tests that want to cover the pre-v1.53 short-circuit set
			// their own version.
			cli.WithVersion("1.54"))
	})
	AfterEach(func() {
		mockServer.Close()
	})
	Describe("WarnOnHeadPullFailed", func() {
		containerUnknown := MockContainer(WithImageName("unknown.repo/prefix/imagename:latest"))
		containerKnown := MockContainer(WithImageName("docker.io/prefix/imagename:latest"))

		When(`warn on head failure is set to "always"`, func() {
			c := dockerClient{ClientOptions: ClientOptions{WarnOnHeadFailed: WarnAlways}}
			It("should always return true", func() {
				Expect(c.WarnOnHeadPullFailed(containerUnknown)).To(BeTrue())
				Expect(c.WarnOnHeadPullFailed(containerKnown)).To(BeTrue())
			})
		})
		When(`warn on head failure is set to "auto"`, func() {
			c := dockerClient{ClientOptions: ClientOptions{WarnOnHeadFailed: WarnAuto}}
			It("should return false for unknown repos", func() {
				Expect(c.WarnOnHeadPullFailed(containerUnknown)).To(BeFalse())
			})
			It("should return true for known repos", func() {
				Expect(c.WarnOnHeadPullFailed(containerKnown)).To(BeTrue())
			})
		})
		When(`warn on head failure is set to "never"`, func() {
			c := dockerClient{ClientOptions: ClientOptions{WarnOnHeadFailed: WarnNever}}
			It("should never return true", func() {
				Expect(c.WarnOnHeadPullFailed(containerUnknown)).To(BeFalse())
				Expect(c.WarnOnHeadPullFailed(containerKnown)).To(BeFalse())
			})
		})
	})
	When("pulling the latest image", func() {
		When("the image consist of a pinned hash", func() {
			It("should gracefully fail with the typed ErrPinnedImage sentinel", func() {
				c := dockerClient{}
				pinnedContainer := MockContainer(WithImageName("sha256:fa5269854a5e615e51a72b17ad3fd1e01268f278a6684c8ed3c5f0cdce3f230b"))
				err := c.PullImage(context.Background(), pinnedContainer)
				Expect(err).To(MatchError(ErrPinnedImage))
				// Message preserved verbatim from the pre-typed-error wording so
				// existing notification templates and operator log greps keep working.
				Expect(err).To(MatchError(`container uses a pinned image, and cannot be updated by watchtower`))
			})
		})
	})
	When("removing a running container", func() {
		When("the container still exist after stopping", func() {
			It("should attempt to remove the container", func() {
				ctr := MockContainer(WithContainerState(container.State{Running: true}))
				containerStopped := MockContainer(WithContainerState(container.State{Running: false}))

				cid := ctr.ContainerInfo().ID
				mockServer.AppendHandlers(
					mocks.KillContainerHandler(cid, mocks.Found),
					mocks.GetContainerHandler(cid, containerStopped.ContainerInfo()),
					mocks.RemoveContainerHandler(cid, mocks.Found),
					mocks.GetContainerHandler(cid, nil),
				)

				Expect(dockerClient{api: docker}.StopContainer(ctr, time.Minute)).To(Succeed())
			})
		})
		When("the container does not exist after stopping", func() {
			It("should signal mid-scan disappearance via ErrContainerNotFound", func() {
				ctr := MockContainer(WithContainerState(container.State{Running: true}))

				cid := ctr.ContainerInfo().ID
				mockServer.AppendHandlers(
					mocks.KillContainerHandler(cid, mocks.Found),
					mocks.GetContainerHandler(cid, nil),
					mocks.RemoveContainerHandler(cid, mocks.Missing),
				)

				err := dockerClient{api: docker}.StopContainer(ctr, time.Minute)
				Expect(err).To(MatchError(ErrContainerNotFound))
			})
		})
		When("the container is already gone before we try to kill it", func() {
			It("should signal mid-scan disappearance via ErrContainerNotFound", func() {
				ctr := MockContainer(WithContainerState(container.State{Running: true}))

				cid := ctr.ContainerInfo().ID
				mockServer.AppendHandlers(
					mocks.KillContainerHandler(cid, mocks.Missing),
				)

				err := dockerClient{api: docker}.StopContainer(ctr, time.Minute)
				Expect(err).To(MatchError(ErrContainerNotFound))
			})
		})
	})
	When("removing a image", func() {
		When("debug logging is enabled", func() {
			It("should log removed and untagged images", func() {
				imageA := util.GenerateRandomSHA256()
				imageAParent := util.GenerateRandomSHA256()
				images := map[string][]string{imageA: {imageAParent}}
				mockServer.AppendHandlers(mocks.RemoveImageHandler(images))
				c := dockerClient{api: docker}

				resetLogrus, logbuf := captureLogrus(logrus.DebugLevel)
				defer resetLogrus()

				Expect(c.RemoveImageByID(t.ImageID(imageA))).To(Succeed())

				shortA := t.ImageID(imageA).ShortID()
				shortAParent := t.ImageID(imageAParent).ShortID()

				Eventually(logbuf).Should(gbytes.Say(`deleted="%v, %v" untagged="?%v"?`, shortA, shortAParent, shortA))
			})
		})
		When("image is not found", func() {
			It("should treat NotFound as success since the end state already matches", func() {
				image := util.GenerateRandomSHA256()
				mockServer.AppendHandlers(mocks.RemoveImageHandler(nil))
				c := dockerClient{api: docker}

				Expect(c.RemoveImageByID(t.ImageID(image))).To(Succeed())
			})
		})
		When("the daemon reports the image is still in use by another container", func() {
			It("defers silently instead of erroring so the alert doesn't churn on self-update or shared-base-image overlap", func() {
				image := util.GenerateRandomSHA256()
				mockServer.AppendHandlers(
					ghttp.CombineHandlers(
						ghttp.VerifyRequest("DELETE", HaveSuffix("/images/"+image)),
						ghttp.RespondWithJSONEncoded(http.StatusConflict, struct{ Message string }{
							Message: "conflict: unable to delete " + image[:12] + " (cannot be forced) - image is being used by running container abc123",
						}),
					),
				)
				c := dockerClient{api: docker}
				Expect(c.RemoveImageByID(t.ImageID(image))).To(Succeed())
			})
		})
	})
	When("listing containers", func() {
		When("no filter is provided", func() {
			It("should return all available containers", func() {
				mockServer.AppendHandlers(mocks.ListContainersHandler("running"))
				mockServer.AppendHandlers(mocks.GetContainerHandlers(&mocks.Watchtower, &mocks.Running)...)
				client := dockerClient{
					api:           docker,
					ClientOptions: ClientOptions{},
				}
				containers, err := client.ListContainers(filters.NoFilter)
				Expect(err).NotTo(HaveOccurred())
				Expect(containers).To(HaveLen(2))
			})
		})
		When("a filter matching nothing", func() {
			It("should return an empty array", func() {
				mockServer.AppendHandlers(mocks.ListContainersHandler("running"))
				mockServer.AppendHandlers(mocks.GetContainerHandlers(&mocks.Watchtower, &mocks.Running)...)
				filter := filters.FilterByNames([]string{"lollercoaster"}, filters.NoFilter)
				client := dockerClient{
					api:           docker,
					ClientOptions: ClientOptions{},
				}
				containers, err := client.ListContainers(filter)
				Expect(err).NotTo(HaveOccurred())
				Expect(containers).To(BeEmpty())
			})
		})
		When("a watchtower filter is provided", func() {
			It("should return only the watchtower container", func() {
				mockServer.AppendHandlers(mocks.ListContainersHandler("running"))
				mockServer.AppendHandlers(mocks.GetContainerHandlers(&mocks.Watchtower, &mocks.Running)...)
				client := dockerClient{
					api:           docker,
					ClientOptions: ClientOptions{},
				}
				containers, err := client.ListContainers(filters.WatchtowerContainersFilter)
				Expect(err).NotTo(HaveOccurred())
				Expect(containers).To(ConsistOf(withContainerImageName(Equal("containrrr/watchtower:latest"))))
			})
		})
		When(`include stopped is enabled`, func() {
			It("should return both stopped and running containers", func() {
				mockServer.AppendHandlers(mocks.ListContainersHandler("running", "exited", "created"))
				mockServer.AppendHandlers(mocks.GetContainerHandlers(&mocks.Stopped, &mocks.Watchtower, &mocks.Running)...)
				client := dockerClient{
					api:           docker,
					ClientOptions: ClientOptions{IncludeStopped: true},
				}
				containers, err := client.ListContainers(filters.NoFilter)
				Expect(err).NotTo(HaveOccurred())
				Expect(containers).To(ContainElement(havingRunningState(false)))
			})
		})
		When(`include restarting is enabled`, func() {
			It("should return both restarting and running containers", func() {
				mockServer.AppendHandlers(mocks.ListContainersHandler("running", "restarting"))
				mockServer.AppendHandlers(mocks.GetContainerHandlers(&mocks.Watchtower, &mocks.Running, &mocks.Restarting)...)
				client := dockerClient{
					api:           docker,
					ClientOptions: ClientOptions{IncludeRestarting: true},
				}
				containers, err := client.ListContainers(filters.NoFilter)
				Expect(err).NotTo(HaveOccurred())
				Expect(containers).To(ContainElement(havingRestartingState(true)))
			})
		})
		When(`include restarting is disabled`, func() {
			It("should not return restarting containers", func() {
				mockServer.AppendHandlers(mocks.ListContainersHandler("running"))
				mockServer.AppendHandlers(mocks.GetContainerHandlers(&mocks.Watchtower, &mocks.Running)...)
				client := dockerClient{
					api:           docker,
					ClientOptions: ClientOptions{IncludeRestarting: false},
				}
				containers, err := client.ListContainers(filters.NoFilter)
				Expect(err).NotTo(HaveOccurred())
				Expect(containers).NotTo(ContainElement(havingRestartingState(true)))
			})
		})
		When(`a container is recreated between list and inspect`, func() {
			It(`skips the vanished container instead of aborting the scan`, func() {
				mockServer.AppendHandlers(mocks.ListContainersHandler("running"))
				mockServer.AppendHandlers(mocks.GetContainerHandlers(&mocks.Watchtower)...)
				mockServer.AppendHandlers(mocks.GetContainerHandler(string(mocks.Running.ContainerID()), nil))

				client := dockerClient{api: docker, ClientOptions: ClientOptions{}}
				containers, err := client.ListContainers(filters.NoFilter)
				Expect(err).NotTo(HaveOccurred())
				Expect(containers).To(HaveLen(1))
			})
		})
		When(`the Docker daemon returns a transient error`, func() {
			var origBase, origMax time.Duration
			BeforeEach(func() {
				// Shrink backoff so the 3-attempt test doesn't cost seconds.
				origBase, origMax = listBackoffBase, listBackoffMax
				listBackoffBase = time.Millisecond
				listBackoffMax = 2 * time.Millisecond
			})
			AfterEach(func() {
				listBackoffBase, listBackoffMax = origBase, origMax
			})
			It(`retries and succeeds when the second attempt returns cleanly`, func() {
				mockServer.AppendHandlers(ghttp.RespondWith(http.StatusServiceUnavailable, `{"message":"daemon restarting"}`))
				mockServer.AppendHandlers(mocks.ListContainersHandler("running"))
				mockServer.AppendHandlers(mocks.GetContainerHandlers(&mocks.Watchtower, &mocks.Running)...)

				client := dockerClient{api: docker, ClientOptions: ClientOptions{}}
				containers, err := client.ListContainers(filters.NoFilter)
				Expect(err).NotTo(HaveOccurred())
				Expect(containers).To(HaveLen(2))
			})
			It(`surfaces the error after exhausting all attempts`, func() {
				mockServer.AppendHandlers(ghttp.RespondWith(http.StatusServiceUnavailable, `{"message":"daemon restarting"}`))
				mockServer.AppendHandlers(ghttp.RespondWith(http.StatusServiceUnavailable, `{"message":"daemon restarting"}`))
				mockServer.AppendHandlers(ghttp.RespondWith(http.StatusServiceUnavailable, `{"message":"daemon restarting"}`))

				client := dockerClient{api: docker, ClientOptions: ClientOptions{}}
				_, err := client.ListContainers(filters.NoFilter)
				Expect(err).To(HaveOccurred())
			})
			It(`does not retry on 400 InvalidArgument`, func() {
				// Single handler only — retrying would cause the test to fail
				// because ghttp.Server panics when more handlers are consumed
				// than registered.
				mockServer.AppendHandlers(ghttp.RespondWith(http.StatusBadRequest, `{"message":"invalid filter"}`))

				client := dockerClient{api: docker, ClientOptions: ClientOptions{}}
				_, err := client.ListContainers(filters.NoFilter)
				Expect(err).To(HaveOccurred())
			})
		})
		When(`a container uses container network mode`, func() {
			When(`the network container can be resolved`, func() {
				It("should return the container name instead of the ID", func() {
					consumerContainerRef := mocks.NetConsumerOK
					mockServer.AppendHandlers(mocks.GetContainerHandlers(&consumerContainerRef)...)
					client := dockerClient{
						api:           docker,
						ClientOptions: ClientOptions{},
					}
					container, err := client.GetContainer(consumerContainerRef.ContainerID())
					Expect(err).NotTo(HaveOccurred())
					networkMode := container.ContainerInfo().HostConfig.NetworkMode
					Expect(networkMode.ConnectedContainer()).To(Equal(mocks.NetSupplierContainerName))
				})
			})
			When(`the network container cannot be resolved`, func() {
				It("should still return the container ID", func() {
					consumerContainerRef := mocks.NetConsumerInvalidSupplier
					mockServer.AppendHandlers(mocks.GetContainerHandlers(&consumerContainerRef)...)
					client := dockerClient{
						api:           docker,
						ClientOptions: ClientOptions{},
					}
					container, err := client.GetContainer(consumerContainerRef.ContainerID())
					Expect(err).NotTo(HaveOccurred())
					networkMode := container.ContainerInfo().HostConfig.NetworkMode
					Expect(networkMode.ConnectedContainer()).To(Equal(mocks.NetSupplierNotFoundID))
				})
			})
		})
	})
	Describe(`ExecuteCommand`, func() {
		When(`logging`, func() {
			It("should include container id field", func() {
				client := dockerClient{
					api:           docker,
					ClientOptions: ClientOptions{},
				}

				// Capture logrus output in buffer
				resetLogrus, logbuf := captureLogrus(logrus.DebugLevel)
				defer resetLogrus()

				user := ""
				containerID := t.ContainerID("ex-cont-id")
				execID := "ex-exec-id"
				cmd := "exec-cmd"

				mockServer.AppendHandlers(
					// API.ContainerExecCreate
					ghttp.CombineHandlers(
						ghttp.VerifyRequest("POST", HaveSuffix("containers/%v/exec", containerID)),
						ghttp.VerifyJSONRepresenting(container.ExecOptions{
							User:   user,
							Detach: false,
							Tty:    true,
							Cmd: []string{
								"sh",
								"-c",
								cmd,
							},
						}),
						ghttp.RespondWithJSONEncoded(http.StatusOK, container.ExecCreateResponse{ID: execID}),
					),
					// API.ContainerExecStart
					ghttp.CombineHandlers(
						ghttp.VerifyRequest("POST", HaveSuffix("exec/%v/start", execID)),
						ghttp.VerifyJSONRepresenting(container.ExecStartOptions{
							Detach: false,
							Tty:    true,
						}),
						ghttp.RespondWith(http.StatusOK, nil),
					),
					// API.ContainerExecInspect
					ghttp.CombineHandlers(
						ghttp.VerifyRequest("GET", HaveSuffix("exec/ex-exec-id/json")),
						ghttp.RespondWithJSONEncoded(http.StatusOK, backend.ExecInspect{
							ID:       execID,
							Running:  false,
							ExitCode: nil,
							ProcessConfig: &backend.ExecProcessConfig{
								Entrypoint: "sh",
								Arguments:  []string{"-c", cmd},
								User:       user,
							},
							ContainerID: string(containerID),
						}),
					),
				)

				_, err := client.ExecuteCommand(containerID, cmd, 1)
				Expect(err).NotTo(HaveOccurred())
				// Note: Since Execute requires opening up a raw TCP stream to the daemon for the output, this will fail
				// when using the mock API server. Regardless of the outcome, the log should include the container ID
				Eventually(logbuf).Should(gbytes.Say(`containerID="?ex-cont-id"?`))
			})
		})
	})
	Describe(`GetNetworkConfig`, func() {
		When(`providing a container with network aliases`, func() {
			It(`should omit the container ID alias`, func() {
				client := dockerClient{
					api:           docker,
					ClientOptions: ClientOptions{IncludeRestarting: false},
				}
				ctr := MockContainer(WithImageName("docker.io/prefix/imagename:latest"))

				aliases := []string{"One", "Two", ctr.ID().ShortID(), "Four"}
				endpoints := map[string]*network.EndpointSettings{
					`test`: {Aliases: aliases},
				}
				ctr.containerInfo.NetworkSettings = &container.NetworkSettings{Networks: endpoints}
				Expect(ctr.ContainerInfo().NetworkSettings.Networks[`test`].Aliases).To(Equal(aliases))
				Expect(client.GetNetworkConfig(ctr).EndpointsConfig[`test`].Aliases).To(Equal([]string{"One", "Two", "Four"}))
			})
		})
	})
	Describe(`GetContainer`, func() {
		When(`the image referenced by the container has been removed locally`, func() {
			missingImageID := "sha256:4d239725ac8da47ecfcf04356f19845a4207c3423f61979151bff56612f04807"
			imageRef := "testimage:latest"

			newContainerInfo := func(id string) *container.InspectResponse {
				return &container.InspectResponse{
					ContainerJSONBase: &container.ContainerJSONBase{
						ID:         id,
						Image:      missingImageID,
						Name:       "/" + id,
						HostConfig: &container.HostConfig{},
					},
					Config: &container.Config{Image: imageRef},
				}
			}
			notFound := ghttp.RespondWithJSONEncoded(http.StatusNotFound, struct{ Message string }{Message: "No such image"})

			It(`falls back to inspecting by the image reference so updates can proceed`, func() {
				containerID := "fallback-cont-id"
				fallbackID := "sha256:b00ed20a1dd0000000000000000000000000000000000000000000000000000a"
				fallbackImage := &image.InspectResponse{ID: fallbackID, RepoTags: []string{imageRef}}

				mockServer.AppendHandlers(
					mocks.GetContainerHandler(containerID, newContainerInfo(containerID)),
					ghttp.CombineHandlers(
						ghttp.VerifyRequest("GET", HaveSuffix("/images/%s/json", missingImageID)),
						notFound,
					),
					ghttp.CombineHandlers(
						ghttp.VerifyRequest("GET", HaveSuffix("/images/%s/json", imageRef)),
						ghttp.RespondWithJSONEncoded(http.StatusOK, fallbackImage),
					),
				)

				result, err := dockerClient{api: docker}.GetContainer(t.ContainerID(containerID))
				Expect(err).NotTo(HaveOccurred())
				Expect(result.ImageInfo()).NotTo(BeNil())
				Expect(string(result.SafeImageID())).To(Equal(fallbackID))
			})

			It(`returns a nil imageInfo when the fallback lookup also fails`, func() {
				containerID := "fallback-cont-miss"
				mockServer.AppendHandlers(
					mocks.GetContainerHandler(containerID, newContainerInfo(containerID)),
					ghttp.CombineHandlers(
						ghttp.VerifyRequest("GET", HaveSuffix("/images/%s/json", missingImageID)),
						notFound,
					),
					ghttp.CombineHandlers(
						ghttp.VerifyRequest("GET", HaveSuffix("/images/%s/json", imageRef)),
						notFound,
					),
				)

				result, err := dockerClient{api: docker}.GetContainer(t.ContainerID(containerID))
				Expect(err).NotTo(HaveOccurred())
				Expect(result.ImageInfo()).To(BeNil())
			})
		})

		When(`the daemon exposes an Identity field in the raw inspect response`, func() {
			containerID := "identity-cont"
			imageID := "sha256:1819191d2b49b6b6f21d3179cfcae0228390728031eccee76b9e21e7e65490c5"
			newContainerInfo := func(id string) *container.InspectResponse {
				return &container.InspectResponse{
					ContainerJSONBase: &container.ContainerJSONBase{
						ID:         id,
						Image:      imageID,
						Name:       "/" + id,
						HostConfig: &container.HostConfig{},
					},
					Config: &container.Config{Image: "tg-antispam:latest"},
				}
			}

			It(`decodes Build provenance so ImageIsLocal reports true`, func() {
				rawBody := []byte(`{
					"Id": "` + imageID + `",
					"RepoTags": ["tg-antispam:latest"],
					"RepoDigests": ["tg-antispam@sha256:1819191d2b49b6b6f21d3179cfcae0228390728031eccee76b9e21e7e65490c5"],
					"Identity": {"Build": [{"Ref": "xtjrtadzig3i", "CreatedAt": "2026-04-19T20:28:03+02:00"}]}
				}`)

				mockServer.AppendHandlers(
					mocks.GetContainerHandler(containerID, newContainerInfo(containerID)),
					ghttp.CombineHandlers(
						ghttp.VerifyRequest("GET", HaveSuffix("/images/%s/json", imageID)),
						ghttp.RespondWith(http.StatusOK, rawBody, http.Header{"Content-Type": []string{"application/json"}}),
					),
				)

				result, err := dockerClient{api: docker}.GetContainer(t.ContainerID(containerID))
				Expect(err).NotTo(HaveOccurred())
				Expect(result.ImageInfo()).NotTo(BeNil())
				Expect(result.(*Container).ImageIdentity()).NotTo(BeNil())
				Expect(result.(*Container).ImageIdentity().Build).To(HaveLen(1))
				Expect(result.(*Container).ImageIsLocal()).To(BeTrue())
			})

			It(`decodes Pull provenance so ImageIsLocal reports false even with a bare-name RepoDigest`, func() {
				rawBody := []byte(`{
					"Id": "` + imageID + `",
					"RepoTags": ["postgres:18"],
					"RepoDigests": ["postgres@sha256:52e6ffd11fddd081ae63880b635b2a61c14008c17fc98cdc7ce5472265516dd0"],
					"Identity": {"Pull": [{"Repository": "docker.io/library/postgres"}]}
				}`)

				mockServer.AppendHandlers(
					mocks.GetContainerHandler(containerID, newContainerInfo(containerID)),
					ghttp.CombineHandlers(
						ghttp.VerifyRequest("GET", HaveSuffix("/images/%s/json", imageID)),
						ghttp.RespondWith(http.StatusOK, rawBody, http.Header{"Content-Type": []string{"application/json"}}),
					),
				)

				result, err := dockerClient{api: docker}.GetContainer(t.ContainerID(containerID))
				Expect(err).NotTo(HaveOccurred())
				Expect(result.(*Container).ImageIdentity()).NotTo(BeNil())
				Expect(result.(*Container).ImageIdentity().Pull).To(HaveLen(1))
				Expect(result.(*Container).ImageIsLocal()).To(BeFalse())
			})

			It(`leaves ImageIdentity nil when the daemon omits the field (classic image store)`, func() {
				rawBody := []byte(`{
					"Id": "` + imageID + `",
					"RepoTags": ["legacy:latest"],
					"RepoDigests": []
				}`)

				mockServer.AppendHandlers(
					mocks.GetContainerHandler(containerID, newContainerInfo(containerID)),
					ghttp.CombineHandlers(
						ghttp.VerifyRequest("GET", HaveSuffix("/images/%s/json", imageID)),
						ghttp.RespondWith(http.StatusOK, rawBody, http.Header{"Content-Type": []string{"application/json"}}),
					),
				)

				result, err := dockerClient{api: docker}.GetContainer(t.ContainerID(containerID))
				Expect(err).NotTo(HaveOccurred())
				Expect(result.(*Container).ImageIdentity()).To(BeNil())
				// Falls through to the legacy empty-RepoDigests heuristic.
				Expect(result.(*Container).ImageIsLocal()).To(BeTrue())
			})

			It(`short-circuits the Identity decode when the negotiated API is below v1.53`, func() {
				// Older client: even if the daemon response happened to
				// include Identity, we shouldn't waste an unmarshal on it.
				legacyClient, _ := cli.NewClientWithOpts(
					cli.WithHost(mockServer.URL()),
					cli.WithHTTPClient(mockServer.HTTPTestServer.Client()),
					cli.WithVersion("1.50"))

				rawBody := []byte(`{
					"Id": "` + imageID + `",
					"RepoTags": ["tg-antispam:latest"],
					"RepoDigests": ["tg-antispam@sha256:1819191d2b49b6b6f21d3179cfcae0228390728031eccee76b9e21e7e65490c5"],
					"Identity": {"Build": [{"Ref": "ignored-on-old-api"}]}
				}`)

				mockServer.AppendHandlers(
					mocks.GetContainerHandler(containerID, newContainerInfo(containerID)),
					ghttp.CombineHandlers(
						ghttp.VerifyRequest("GET", HaveSuffix("/images/%s/json", imageID)),
						ghttp.RespondWith(http.StatusOK, rawBody, http.Header{"Content-Type": []string{"application/json"}}),
					),
				)

				result, err := dockerClient{api: legacyClient}.GetContainer(t.ContainerID(containerID))
				Expect(err).NotTo(HaveOccurred())
				Expect(result.(*Container).ImageIdentity()).To(BeNil())
				// ImageIsLocal's Identity branch doesn't fire; the
				// RepoDigests-based fallback says "has digest → not local",
				// so the safeguard in IsContainerStale has to carry older
				// daemons.
				Expect(result.(*Container).ImageIsLocal()).To(BeFalse())
			})
		})
	})

	Describe(`decodeImageIdentity`, func() {
		It(`returns nil on an empty payload`, func() {
			Expect(decodeImageIdentity(nil)).To(BeNil())
			Expect(decodeImageIdentity([]byte{})).To(BeNil())
		})
		It(`returns nil when the Identity field is absent`, func() {
			Expect(decodeImageIdentity([]byte(`{"Id":"sha256:abc"}`))).To(BeNil())
		})
		It(`returns nil when the Identity field is present but empty`, func() {
			Expect(decodeImageIdentity([]byte(`{"Identity":{}}`))).To(BeNil())
			Expect(decodeImageIdentity([]byte(`{"Identity":{"Build":[],"Pull":[]}}`))).To(BeNil())
		})
		It(`returns nil on malformed JSON without panicking`, func() {
			Expect(decodeImageIdentity([]byte(`{not json`))).To(BeNil())
		})
		It(`decodes a Build entry`, func() {
			id := decodeImageIdentity([]byte(`{"Identity":{"Build":[{"Ref":"r","CreatedAt":"t"}]}}`))
			Expect(id).NotTo(BeNil())
			Expect(id.Build).To(HaveLen(1))
			Expect(id.Build[0].Ref).To(Equal("r"))
		})
		It(`decodes a Pull entry`, func() {
			id := decodeImageIdentity([]byte(`{"Identity":{"Pull":[{"Repository":"ghcr.io/example/app"}]}}`))
			Expect(id).NotTo(BeNil())
			Expect(id.Pull).To(HaveLen(1))
			Expect(id.Pull[0].Repository).To(Equal("ghcr.io/example/app"))
		})
	})

	Describe(`imageRefHasRegistryHost`, func() {
		It(`returns false for a bare name`, func() {
			Expect(imageRefHasRegistryHost("tg-antispam:latest")).To(BeFalse())
			Expect(imageRefHasRegistryHost("nginx")).To(BeFalse())
			Expect(imageRefHasRegistryHost("nginx@sha256:aa0afebbb3cfa473099a62c4b32e9b3fb73ed23f2a75a65ce1d4b4f55a5c2ef2")).To(BeFalse())
		})
		It(`returns false for a bare two-segment name (no hostname marker)`, func() {
			// "myorg/app" has a "/" but no "." or ":" in the first segment,
			// so Docker treats "myorg" as a user namespace on Hub, not a host.
			Expect(imageRefHasRegistryHost("myorg/app:latest")).To(BeFalse())
		})
		It(`returns true when the first segment is a FQDN`, func() {
			Expect(imageRefHasRegistryHost("ghcr.io/openserbia/watchtower:latest")).To(BeTrue())
			Expect(imageRefHasRegistryHost("registry.example.com/foo/bar:tag")).To(BeTrue())
		})
		It(`returns true when the first segment has a port`, func() {
			Expect(imageRefHasRegistryHost("registry.local:5000/app:v1")).To(BeTrue())
		})
		It(`returns true for localhost (reserved namespace)`, func() {
			Expect(imageRefHasRegistryHost("localhost/app:v1")).To(BeTrue())
			Expect(imageRefHasRegistryHost("localhost:5000/app:v1")).To(BeTrue())
		})
		It(`returns false for an unparseable reference (conservative — allow safeguard)`, func() {
			// A pinned "sha256:..." isn't a parseable image name by the
			// reference grammar; treated as no-host so the safeguard can
			// still fall through to HasNewImage for bad inputs.
			Expect(imageRefHasRegistryHost("sha256:abc")).To(BeFalse())
		})
	})

	Describe(`classifyPullError`, func() {
		It(`returns nil for a nil error`, func() {
			Expect(classifyPullError("foo:latest", nil)).To(BeNil())
		})
		It(`wraps unauthorized errors with ErrPullImageUnauthorized`, func() {
			wrapped := classifyPullError("private/app:latest", cerrdefs.ErrUnauthenticated)
			Expect(errors.Is(wrapped, ErrPullImageUnauthorized)).To(BeTrue())
			// Underlying classification stays detectable so cerrdefs-based
			// callers (like pullFailureLooksLocal) keep working.
			Expect(cerrdefs.IsUnauthorized(wrapped)).To(BeTrue())
			Expect(wrapped).To(MatchError(ContainSubstring("private/app:latest")))
		})
		It(`wraps not-found errors with ErrPullImageNotFound`, func() {
			wrapped := classifyPullError("ghcr.io/missing/app:latest", cerrdefs.ErrNotFound)
			Expect(errors.Is(wrapped, ErrPullImageNotFound)).To(BeTrue())
			// pullFailureLooksLocal's IsNotFound check must keep firing on
			// the wrapped value or the local-build safeguard regresses.
			Expect(cerrdefs.IsNotFound(wrapped)).To(BeTrue())
			Expect(wrapped).To(MatchError(ContainSubstring("ghcr.io/missing/app:latest")))
		})
		It(`returns transient errors untouched (no typed sentinel)`, func() {
			transient := errors.New("connection reset")
			wrapped := classifyPullError("app:latest", transient)
			Expect(wrapped).To(Equal(transient))
			Expect(errors.Is(wrapped, ErrPullImageUnauthorized)).To(BeFalse())
			Expect(errors.Is(wrapped, ErrPullImageNotFound)).To(BeFalse())
		})
	})

	Describe(`pullFailureLooksLocal`, func() {
		notFoundErr := cerrdefs.ErrNotFound
		otherErr := errors.New("connection reset")

		It(`returns false for a nil error`, func() {
			Expect(pullFailureLooksLocal("tg-antispam:latest", nil)).To(BeFalse())
		})
		It(`returns false for a non-NotFound error`, func() {
			Expect(pullFailureLooksLocal("tg-antispam:latest", otherErr)).To(BeFalse())
		})
		It(`returns false for a hostname-qualified reference (no silent masking)`, func() {
			Expect(pullFailureLooksLocal("ghcr.io/foo/bar:latest", notFoundErr)).To(BeFalse())
			Expect(pullFailureLooksLocal("registry.local:5000/app", notFoundErr)).To(BeFalse())
		})
		It(`returns true for a bare-name reference with a NotFound pull error`, func() {
			Expect(pullFailureLooksLocal("tg-antispam:latest", notFoundErr)).To(BeTrue())
			Expect(pullFailureLooksLocal("myorg/app:latest", notFoundErr)).To(BeTrue())
		})
		It(`still recognises NotFound after classifyPullError wraps it`, func() {
			// IsContainerStale receives the wrapped error from PullImage now;
			// the local-build safeguard must keep firing on bare-name refs.
			wrapped := classifyPullError("tg-antispam:latest", notFoundErr)
			Expect(pullFailureLooksLocal("tg-antispam:latest", wrapped)).To(BeTrue())
		})
	})
})

// Capture logrus output in buffer
func captureLogrus(level logrus.Level) (func(), *gbytes.Buffer) {
	logbuf := gbytes.NewBuffer()

	origOut := logrus.StandardLogger().Out
	logrus.SetOutput(logbuf)

	origLev := logrus.StandardLogger().Level
	logrus.SetLevel(level)

	return func() {
		logrus.SetOutput(origOut)
		logrus.SetLevel(origLev)
	}, logbuf
}

// Gomega matcher helpers

func withContainerImageName(matcher gt.GomegaMatcher) gt.GomegaMatcher {
	return WithTransform(containerImageName, matcher)
}

func containerImageName(container t.Container) string {
	return container.ImageName()
}

func havingRestartingState(expected bool) gt.GomegaMatcher {
	return WithTransform(func(container t.Container) bool {
		return container.ContainerInfo().State.Restarting
	}, Equal(expected))
}

func havingRunningState(expected bool) gt.GomegaMatcher {
	return WithTransform(func(container t.Container) bool {
		return container.ContainerInfo().State.Running
	}, Equal(expected))
}
