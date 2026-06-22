package container

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/network"
	cli "github.com/moby/moby/client"
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
		docker, _ = cli.New(
			cli.WithHost(mockServer.URL()),
			cli.WithHTTPClient(mockServer.HTTPTestServer.Client()),
			// Pin to the API version NewClient opportunistically upgrades
			// to in production so the Identity-decoding path exercised by
			// the GetContainer tests below matches real daemon behavior.
			// Tests that want to cover the pre-v1.53 short-circuit set
			// their own version.
			cli.WithAPIVersion("1.54"),
		)
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
	Describe("ProbeCapabilities image_pull classification", func() {
		// Regression: a bare-name pull probe hits Docker Hub, which answers a
		// nonexistent docker.io/library/<name> repo with 401. That 401 proves the
		// request traversed the proxy and the daemon attempted the pull, so the
		// capability is Present — classifying it Unreachable would log.Fatal a
		// healthy daemon at startup under --preflight.
		It("treats a registry 401 as Present, not Unreachable", func() {
			mockServer.AppendHandlers(ghttp.CombineHandlers(
				ghttp.VerifyRequest("POST", HaveSuffix("/images/create")),
				ghttp.RespondWithJSONEncoded(http.StatusUnauthorized, struct{ Message string }{Message: "pull access denied"}),
			))
			c := dockerClient{api: docker}
			results := c.ProbeCapabilities(context.Background(), []CapabilityID{CapImagePull})
			Expect(results).To(HaveLen(1))
			Expect(results[0].ID).To(Equal(CapImagePull))
			Expect(results[0].Status).To(Equal(StatusPresent))
		})
		It("treats a proxy 403 as Blocked", func() {
			mockServer.AppendHandlers(ghttp.CombineHandlers(
				ghttp.VerifyRequest("POST", HaveSuffix("/images/create")),
				ghttp.RespondWithJSONEncoded(http.StatusForbidden, struct{ Message string }{Message: "forbidden"}),
			))
			c := dockerClient{api: docker}
			results := c.ProbeCapabilities(context.Background(), []CapabilityID{CapImagePull})
			Expect(results).To(HaveLen(1))
			Expect(results[0].Status).To(Equal(StatusBlocked))
		})
	})
	Describe("ProbeCapabilities container_create classification", func() {
		// Regression: the probe must pass a non-nil *container.Config. The SDK
		// writes config.MacAddress unconditionally on API >= 1.44, so a nil
		// config is a client-side nil-pointer panic before any request — which
		// crash-looped a production container under --preflight.
		It("does not panic and treats a daemon 404 (no such image) as Present", func() {
			mockServer.AppendHandlers(ghttp.CombineHandlers(
				ghttp.VerifyRequest("POST", HaveSuffix("/containers/create")),
				ghttp.RespondWithJSONEncoded(http.StatusNotFound, struct{ Message string }{Message: "no such image: probe"}),
			))
			c := dockerClient{api: docker}
			var results []ProbeResult
			Expect(func() {
				results = c.ProbeCapabilities(context.Background(), []CapabilityID{CapContainerCreate})
			}).NotTo(Panic())
			Expect(results).To(HaveLen(1))
			Expect(results[0].ID).To(Equal(CapContainerCreate))
			Expect(results[0].Status).To(Equal(StatusPresent))
		})
		It("treats a proxy 403 as Blocked", func() {
			mockServer.AppendHandlers(ghttp.CombineHandlers(
				ghttp.VerifyRequest("POST", HaveSuffix("/containers/create")),
				ghttp.RespondWithJSONEncoded(http.StatusForbidden, struct{ Message string }{Message: "forbidden"}),
			))
			c := dockerClient{api: docker}
			results := c.ProbeCapabilities(context.Background(), []CapabilityID{CapContainerCreate})
			Expect(results).To(HaveLen(1))
			Expect(results[0].Status).To(Equal(StatusBlocked))
		})
	})
	Describe("ProbeCapabilities over every capability", func() {
		// Regression guard for the nil-config SIGSEGV: exercise the REAL probe
		// for every capability (not just via the mock Client) so a probe that
		// hands the SDK a nil pointer it dereferences client-side is caught here
		// instead of crash-looping in production under --preflight.
		It("never panics, regardless of what the daemon returns", func() {
			mockServer.SetAllowUnhandledRequests(true)
			mockServer.SetUnhandledRequestStatusCode(http.StatusNotFound)

			ids := make([]CapabilityID, 0, len(AllCapabilities()))
			for _, capb := range AllCapabilities() {
				ids = append(ids, capb.ID)
			}

			c := dockerClient{api: docker}
			var results []ProbeResult
			Expect(func() {
				results = c.ProbeCapabilities(context.Background(), ids)
			}).NotTo(Panic())
			Expect(results).To(HaveLen(len(ids)))
			for _, r := range results {
				Expect(r.Status).NotTo(BeEmpty())
			}
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
	Describe(`killContainerWithRetry`, func() {
		const containerID = "c79e9b3b9b3bdeadbeef"
		var origBase, origMax time.Duration
		BeforeEach(func() {
			// Shrink backoff so the 3-attempt path doesn't cost real time.
			origBase, origMax = listBackoffBase, listBackoffMax
			listBackoffBase = time.Millisecond
			listBackoffMax = 2 * time.Millisecond
		})
		AfterEach(func() {
			listBackoffBase, listBackoffMax = origBase, origMax
		})
		When(`the kill POST fails transiently then succeeds`, func() {
			It(`retries and returns no error`, func() {
				// First attempt mimics the proxy keep-alive reap surfacing as a
				// transient failure; the kill lands cleanly on the retry.
				mockServer.AppendHandlers(ghttp.RespondWith(http.StatusServiceUnavailable, `{"message":"daemon restarting"}`))
				mockServer.AppendHandlers(mocks.KillContainerHandler(containerID, mocks.Found))

				err := killContainerWithRetry(context.Background(), docker, containerID, "SIGTERM")
				Expect(err).NotTo(HaveOccurred())
				Expect(mockServer.ReceivedRequests()).To(HaveLen(2))
			})
		})
		When(`the kill POST fails transiently on every attempt`, func() {
			It(`surfaces the error after exhausting all attempts`, func() {
				for range killMaxAttempts {
					mockServer.AppendHandlers(ghttp.RespondWith(http.StatusServiceUnavailable, `{"message":"daemon restarting"}`))
				}

				err := killContainerWithRetry(context.Background(), docker, containerID, "SIGTERM")
				Expect(err).To(HaveOccurred())
				Expect(mockServer.ReceivedRequests()).To(HaveLen(killMaxAttempts))
			})
		})
		When(`the container is already gone`, func() {
			It(`returns NotFound without retrying`, func() {
				// Single handler only — a retry would consume a second handler
				// and ghttp panics when more are consumed than registered, so
				// this also asserts the non-transient bail-out.
				mockServer.AppendHandlers(mocks.KillContainerHandler(containerID, mocks.Missing))

				err := killContainerWithRetry(context.Background(), docker, containerID, "SIGTERM")
				Expect(cerrdefs.IsNotFound(err)).To(BeTrue())
				Expect(mockServer.ReceivedRequests()).To(HaveLen(1))
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
						ghttp.VerifyJSONRepresenting(container.ExecCreateRequest{
							Tty: true,
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
						ghttp.VerifyJSONRepresenting(container.ExecStartRequest{
							Detach: false,
							Tty:    true,
						}),
						ghttp.RespondWith(http.StatusOK, nil),
					),
					// API.ContainerExecInspect
					ghttp.CombineHandlers(
						ghttp.VerifyRequest("GET", HaveSuffix("exec/ex-exec-id/json")),
						ghttp.RespondWithJSONEncoded(http.StatusOK, container.ExecInspectResponse{
							ID:       execID,
							Running:  false,
							ExitCode: nil,
							ProcessConfig: &container.ExecProcessConfig{
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
	Describe(`StartContainer`, func() {
		// runningHostNetContainer is a started container on host networking, so
		// StartContainer skips the network disconnect/connect loop and goes
		// straight from ContainerCreate to ContainerStart — the shortest path
		// to exercise the post-create failure cleanup.
		runningHostNetContainer := func() *Container {
			ctr := MockContainer(
				WithImageName("openserbia/watchtower:latest"),
				WithContainerState(container.State{Running: true}),
			)
			ctr.containerInfo.HostConfig.NetworkMode = "host"
			ctr.containerInfo.NetworkSettings = &container.NetworkSettings{
				Networks: map[string]*network.EndpointSettings{},
			}
			return ctr
		}

		When(`ContainerStart fails after ContainerCreate succeeded`, func() {
			It(`force-removes the just-created container so no orphan is left behind`, func() {
				const createdID = "created-orphan-id"
				removed := false

				mockServer.AppendHandlers(
					// ContainerCreate succeeds, handing back the new container ID.
					ghttp.CombineHandlers(
						ghttp.VerifyRequest("POST", HaveSuffix("/containers/create")),
						ghttp.RespondWithJSONEncoded(http.StatusCreated, container.CreateResponse{ID: createdID}),
					),
					// ContainerStart fails — the daemon rejected the start.
					ghttp.CombineHandlers(
						ghttp.VerifyRequest("POST", HaveSuffix("/containers/%s/start", createdID)),
						ghttp.RespondWithJSONEncoded(http.StatusInternalServerError, struct{ Message string }{Message: "driver failed programming external connectivity"}),
					),
					// The cleanup force-remove of exactly the created container.
					ghttp.CombineHandlers(
						ghttp.VerifyRequest("DELETE", HaveSuffix("/containers/%s", createdID)),
						func(w http.ResponseWriter, r *http.Request) {
							removed = true
							ghttp.RespondWith(http.StatusNoContent, nil)(w, r)
						},
					),
				)

				_, err := dockerClient{api: docker}.StartContainer(runningHostNetContainer())
				Expect(err).To(HaveOccurred())
				Expect(removed).To(BeTrue(), "expected the partially-created container to be force-removed")
				// All three handlers must have been consumed in order.
				Expect(mockServer.ReceivedRequests()).To(HaveLen(3))
			})
		})

		// selfRecreate builds the watchtower container being recreated during a
		// self-update: watchtower-labeled, host-networked (so StartContainer
		// skips the network attach loop), and Exited so the create returns right
		// after ContainerCreate without a ContainerStart — keeping the handler
		// list focused on the name-conflict recovery path.
		selfRecreate := func() *Container {
			ctr := MockContainer(
				WithImageName("ghcr.io/openserbia/watchtower:latest-dev"),
				WithLabels(map[string]string{"com.centurylinklabs.watchtower": "true"}),
				WithContainerState(container.State{Running: false}),
			)
			ctr.containerInfo.ID = "new-self-id"
			ctr.containerInfo.Name = "/watchtower"
			ctr.containerInfo.HostConfig.NetworkMode = "host"
			ctr.containerInfo.NetworkSettings = &container.NetworkSettings{
				Networks: map[string]*network.EndpointSettings{},
			}
			return ctr
		}

		When(`ContainerCreate conflicts with a stale watchtower container holding the name`, func() {
			It(`force-removes the stale blocker and retries the create so the self-update lands`, func() {
				removed := false
				blocker := container.InspectResponse{
					ID:   "stale-orphan-id",
					Name: "/watchtower",
					Config: &container.Config{
						Labels: map[string]string{"com.centurylinklabs.watchtower": "true"},
					},
				}

				mockServer.AppendHandlers(
					// First create fails: the canonical name is already taken.
					ghttp.CombineHandlers(
						ghttp.VerifyRequest("POST", HaveSuffix("/containers/create")),
						ghttp.RespondWithJSONEncoded(http.StatusConflict, struct{ Message string }{
							Message: `Conflict. The container name "/watchtower" is already in use by container "stale-orphan-id".`,
						}),
					),
					// Inspect whoever currently holds the canonical name.
					ghttp.CombineHandlers(
						ghttp.VerifyRequest("GET", HaveSuffix("/containers/watchtower/json")),
						ghttp.RespondWithJSONEncoded(http.StatusOK, blocker),
					),
					// Force-remove the stale watchtower blocker.
					ghttp.CombineHandlers(
						ghttp.VerifyRequest("DELETE", HaveSuffix("/containers/stale-orphan-id")),
						func(w http.ResponseWriter, r *http.Request) {
							removed = true
							ghttp.RespondWith(http.StatusNoContent, nil)(w, r)
						},
					),
					// Retry create now succeeds under the freed name.
					ghttp.CombineHandlers(
						ghttp.VerifyRequest("POST", HaveSuffix("/containers/create")),
						ghttp.RespondWithJSONEncoded(http.StatusCreated, container.CreateResponse{ID: "new-self-id"}),
					),
				)

				id, err := dockerClient{api: docker}.StartContainer(selfRecreate())
				Expect(err).NotTo(HaveOccurred())
				Expect(removed).To(BeTrue(), "expected the stale watchtower blocker to be force-removed")
				Expect(id).To(BeEquivalentTo("new-self-id"))
				// create(409) → inspect → remove → create(201), all consumed.
				Expect(mockServer.ReceivedRequests()).To(HaveLen(4))
			})
		})

		When(`ContainerCreate conflicts with a non-watchtower container`, func() {
			It(`leaves the other container untouched and surfaces the conflict`, func() {
				blocker := container.InspectResponse{
					ID:     "operator-container-id",
					Name:   "/watchtower",
					Config: &container.Config{Labels: map[string]string{}},
				}

				mockServer.AppendHandlers(
					ghttp.CombineHandlers(
						ghttp.VerifyRequest("POST", HaveSuffix("/containers/create")),
						ghttp.RespondWithJSONEncoded(http.StatusConflict, struct{ Message string }{
							Message: `Conflict. The container name "/watchtower" is already in use by container "operator-container-id".`,
						}),
					),
					ghttp.CombineHandlers(
						ghttp.VerifyRequest("GET", HaveSuffix("/containers/watchtower/json")),
						ghttp.RespondWithJSONEncoded(http.StatusOK, blocker),
					),
				)

				_, err := dockerClient{api: docker}.StartContainer(selfRecreate())
				Expect(err).To(HaveOccurred())
				// Inspect happened, but no DELETE and no retry create: we bailed
				// the moment the blocker proved not to be a watchtower container.
				Expect(mockServer.ReceivedRequests()).To(HaveLen(2))
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
					ID:         id,
					Image:      missingImageID,
					Name:       "/" + id,
					HostConfig: &container.HostConfig{},
					Config:     &container.Config{Image: imageRef},
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
					ID:         id,
					Image:      imageID,
					Name:       "/" + id,
					HostConfig: &container.HostConfig{},
					Config:     &container.Config{Image: "tg-antispam:latest"},
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
				legacyClient, _ := cli.New(
					cli.WithHost(mockServer.URL()),
					cli.WithHTTPClient(mockServer.HTTPTestServer.Client()),
					cli.WithAPIVersion("1.50"),
				)

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

	Describe(`applyRecreatePolicy`, func() {
		It(`is a no-op on a nil HostConfig`, func() {
			Expect(func() { applyRecreatePolicy(ClientOptions{DisableMemorySwappiness: true}, nil) }).
				ToNot(Panic())
		})
		It(`leaves MemorySwappiness untouched when the flag is off`, func() {
			val := int64(0)
			hc := &container.HostConfig{Resources: container.Resources{MemorySwappiness: &val}}
			applyRecreatePolicy(ClientOptions{DisableMemorySwappiness: false}, hc)
			Expect(hc.MemorySwappiness).ToNot(BeNil())
			Expect(*hc.MemorySwappiness).To(Equal(int64(0)))
		})
		It(`nils MemorySwappiness when the flag is on (Podman/cgroupv2 compat)`, func() {
			val := int64(0)
			hc := &container.HostConfig{Resources: container.Resources{MemorySwappiness: &val}}
			applyRecreatePolicy(ClientOptions{DisableMemorySwappiness: true}, hc)
			Expect(hc.MemorySwappiness).To(BeNil())
		})
		It(`leaves a non-zero MemorySwappiness alone too — fix is unconditional when opted in`, func() {
			// The Podman host objects to any inspected MemorySwappiness echoed
			// back through ContainerCreate, not just 0. We strip
			// unconditionally when the flag is set.
			val := int64(60)
			hc := &container.HostConfig{Resources: container.Resources{MemorySwappiness: &val}}
			applyRecreatePolicy(ClientOptions{DisableMemorySwappiness: true}, hc)
			Expect(hc.MemorySwappiness).To(BeNil())
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
		It(`treats a Hub 401 / "pull access denied" on a bare name as local`, func() {
			// Docker Hub answers a non-existent docker.io/library/<bare-name>
			// repo with a 401, not a 404 — exactly what a locally-built
			// bare-name image hits on the containerd image store.
			Expect(pullFailureLooksLocal("tg-antispam:latest", cerrdefs.ErrUnauthenticated)).To(BeTrue())
			Expect(pullFailureLooksLocal("myorg/app:latest",
				errors.New(`pull access denied for myorg/app, repository does not exist or may require 'docker login': denied: requested access to the resource is denied`))).To(BeTrue())
			Expect(pullFailureLooksLocal("tg-antispam:latest",
				errors.New(`errorDetail: insufficient_scope: authorization failed`))).To(BeTrue())
		})
		It(`still fails loudly on a 401 for a hostname-qualified reference`, func() {
			// A real private-registry auth failure must surface, never be
			// masked as a local build.
			Expect(pullFailureLooksLocal("ghcr.io/foo/bar:latest", cerrdefs.ErrUnauthenticated)).To(BeFalse())
			Expect(pullFailureLooksLocal("registry.local:5000/app",
				errors.New(`pull access denied, repository does not exist or may require 'docker login'`))).To(BeFalse())
		})
	})

	Describe(`pullErrorLooksUnauthorized`, func() {
		It(`matches the cerrdefs unauthorized classification`, func() {
			Expect(pullErrorLooksUnauthorized(cerrdefs.ErrUnauthenticated)).To(BeTrue())
		})
		It(`matches the human-readable Hub strings case-insensitively`, func() {
			Expect(pullErrorLooksUnauthorized(errors.New("PULL ACCESS DENIED"))).To(BeTrue())
			Expect(pullErrorLooksUnauthorized(errors.New("insufficient_scope: authorization failed"))).To(BeTrue())
			Expect(pullErrorLooksUnauthorized(errors.New("repository does not exist or may require 'docker login'"))).To(BeTrue())
			Expect(pullErrorLooksUnauthorized(errors.New("unauthorized: authentication required"))).To(BeTrue())
		})
		It(`does not match unrelated failures`, func() {
			Expect(pullErrorLooksUnauthorized(errors.New("connection reset"))).To(BeFalse())
			Expect(pullErrorLooksUnauthorized(cerrdefs.ErrNotFound)).To(BeFalse())
		})
	})

	Describe(`PullImage in-stream errors`, func() {
		// Docker reports manifest/layer pull failures as newline-delimited
		// JSONMessages carrying an errorDetail, NOT as the immediate error
		// from ImagePull. PullImage must decode the stream and surface those.
		newDockerFor := func(srv *ghttp.Server) *cli.Client {
			c, _ := cli.New(
				cli.WithHost(srv.URL()),
				cli.WithHTTPClient(srv.HTTPTestServer.Client()),
				cli.WithAPIVersion("1.54"),
			)
			return c
		}

		// localImageContainer builds a container whose registry equals the mock
		// server, so the digest pre-check and the daemon pull both stay on the
		// hermetic server. RepoDigests is populated so the pre-check has
		// something to (fail to) match and PullImage falls through to the pull.
		localImageContainer := func(host string) *Container {
			ref := host + "/library/app"
			c := MockContainer(WithImageName(ref + ":latest"))
			c.imageInfo.RepoDigests = []string{ref + "@sha256:1819191d2b49b6b6f21d3179cfcae0228390728031eccee76b9e21e7e65490c5"}
			return c
		}

		It(`surfaces an in-stream errorDetail as an error`, func() {
			srv := ghttp.NewServer()
			defer srv.Close()
			host := strings.TrimPrefix(srv.URL(), "http://")
			// Registry token-challenge probe and manifest HEAD/GET — answer so
			// the digest pre-check resolves without matching, then falls through.
			srv.RouteToHandler("GET", "/v2/", ghttp.RespondWith(http.StatusOK, ""))
			srv.RouteToHandler("HEAD", regexp.MustCompile(`/manifests/`), ghttp.RespondWith(http.StatusNotFound, ""))
			srv.RouteToHandler("GET", regexp.MustCompile(`/manifests/`), ghttp.RespondWith(http.StatusNotFound, ""))
			// Daemon pull: HTTP 200 but the stream carries a failure.
			srv.RouteToHandler("POST", "/v1.54/images/create", ghttp.RespondWith(
				http.StatusOK,
				`{"status":"Pulling from library/app"}`+"\n"+
					`{"errorDetail":{"message":"manifest unknown: manifest unknown"},"error":"manifest unknown: manifest unknown"}`+"\n",
			))

			err := dockerClient{api: newDockerFor(srv)}.PullImage(context.Background(), localImageContainer(host))
			Expect(err).To(HaveOccurred())
			Expect(err).To(MatchError(ContainSubstring("manifest unknown")))
		})
	})

	Describe(`IsContainerStale pull-failure handling`, func() {
		newDockerFor := func(srv *ghttp.Server) *cli.Client {
			c, _ := cli.New(
				cli.WithHost(srv.URL()),
				cli.WithHTTPClient(srv.HTTPTestServer.Client()),
				cli.WithAPIVersion("1.54"),
			)
			return c
		}

		// staleContainer returns a container with a RepoDigest set so
		// ImageIsLocal() is false and IsContainerStale takes the pull path.
		staleContainer := func(imageRef string) *Container {
			c := MockContainer(WithImageName(imageRef))
			c.containerInfo.Image = "sha256:current00000000000000000000000000000000000000000000000000000000"
			c.imageInfo.RepoDigests = []string{strings.SplitN(imageRef, ":", 2)[0] + "@sha256:1819191d2b49b6b6f21d3179cfcae0228390728031eccee76b9e21e7e65490c5"}
			return c
		}

		// accessDeniedPull routes the daemon pull to a Hub-style 401 delivered
		// in-stream (the realistic shape: HTTP 200 + an errorDetail), then
		// serves HasNewImage's image inspect so the fall-through can complete.
		accessDeniedPull := func(srv *ghttp.Server, imageRef, newID string) {
			srv.RouteToHandler("GET", "/v2/", ghttp.RespondWith(http.StatusOK, ""))
			srv.RouteToHandler("HEAD", regexp.MustCompile(`/manifests/`), ghttp.RespondWith(http.StatusNotFound, ""))
			srv.RouteToHandler("GET", regexp.MustCompile(`/manifests/`), ghttp.RespondWith(http.StatusNotFound, ""))
			srv.RouteToHandler("POST", "/v1.54/images/create", ghttp.RespondWith(
				http.StatusOK,
				`{"errorDetail":{"message":"pull access denied for `+imageRef+`, repository does not exist or may require 'docker login': denied: requested access to the resource is denied"},"error":"pull access denied"}`+"\n",
			))
			srv.RouteToHandler("GET", regexp.MustCompile(`/v1.54/images/.+/json`),
				ghttp.RespondWithJSONEncoded(http.StatusOK, image.InspectResponse{ID: newID, RepoTags: []string{imageRef}}))
		}

		It(`treats a bare-name 401 as locally built and falls through to HasNewImage`, func() {
			srv := ghttp.NewServer()
			defer srv.Close()
			const newID = "sha256:new00000000000000000000000000000000000000000000000000000000000"
			accessDeniedPull(srv, "tg-antispam", newID)

			stale, latest, err := dockerClient{api: newDockerFor(srv)}.
				IsContainerStale(staleContainer("tg-antispam:latest"), t.UpdateParams{})
			// The 401 must NOT propagate: it falls through to HasNewImage,
			// which finds a different image ID and reports stale.
			Expect(err).NotTo(HaveOccurred())
			Expect(stale).To(BeTrue())
			Expect(string(latest)).To(Equal(newID))
		})

		It(`still returns the 401 error for a hostname-qualified reference`, func() {
			srv := ghttp.NewServer()
			defer srv.Close()
			host := strings.TrimPrefix(srv.URL(), "http://")
			imageRef := host + "/foo/app:latest"
			accessDeniedPull(srv, host+"/foo/app", "sha256:irrelevant")

			stale, _, err := dockerClient{api: newDockerFor(srv)}.
				IsContainerStale(staleContainer(imageRef), t.UpdateParams{})
			// Hostname-qualified: a real auth failure must surface loudly.
			Expect(err).To(HaveOccurred())
			Expect(err).To(MatchError(ContainSubstring("pull access denied")))
			Expect(stale).To(BeFalse())
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
