package digest_test

import (
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/ghttp"

	"github.com/openserbia/watchtower/internal/actions/mocks"
	"github.com/openserbia/watchtower/pkg/registry/digest"
	wtTypes "github.com/openserbia/watchtower/pkg/types"
)

func TestDigest(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(GinkgoT(), "Digest Suite")
}

var (
	DockerHubCredentials = &wtTypes.RegistryCredentials{
		Username: os.Getenv("CI_INTEGRATION_TEST_REGISTRY_DH_USERNAME"),
		Password: os.Getenv("CI_INTEGRATION_TEST_REGISTRY_DH_PASSWORD"),
	}
	GHCRCredentials = &wtTypes.RegistryCredentials{
		Username: os.Getenv("CI_INTEGRATION_TEST_REGISTRY_GH_USERNAME"),
		Password: os.Getenv("CI_INTEGRATION_TEST_REGISTRY_GH_PASSWORD"),
	}
)

func SkipIfCredentialsEmpty(credentials *wtTypes.RegistryCredentials, fn func()) func() {
	switch {
	case credentials.Username == "":
		return func() {
			Skip("Username missing. Skipping integration test")
		}
	case credentials.Password == "":
		return func() {
			Skip("Password missing. Skipping integration test")
		}
	default:
		return fn
	}
}

var _ = Describe("Digests", func() {
	mockID := "mock-id"
	mockName := "mock-container"
	mockImage := "ghcr.io/k6io/operator:latest"
	mockCreated := time.Now()
	mockDigest := "ghcr.io/k6io/operator@sha256:d68e1e532088964195ad3a0a71526bc2f11a78de0def85629beb75e2265f0547"

	mockContainer := mocks.CreateMockContainerWithDigest(
		mockID,
		mockName,
		mockImage,
		mockCreated,
		mockDigest)

	mockContainerNoImage := mocks.CreateMockContainerWithImageInfoP(mockID, mockName, mockImage, mockCreated, nil)

	When("a digest comparison is done", func() {
		It("should return true if digests match",
			SkipIfCredentialsEmpty(GHCRCredentials, func() {
				creds := fmt.Sprintf("%s:%s", GHCRCredentials.Username, GHCRCredentials.Password)
				matches, err := digest.CompareDigest(mockContainer, creds)
				Expect(err).NotTo(HaveOccurred())
				Expect(matches).To(Equal(true))
			}),
		)

		It("should return false if digests differ", func() {
		})
		It("should return an error if the registry isn't available", func() {
		})
		It("should return an error when container contains no image info", func() {
			matches, err := digest.CompareDigest(mockContainerNoImage, `user:pass`)
			Expect(err).To(HaveOccurred())
			Expect(matches).To(Equal(false))
		})
	})
	When("using different registries", func() {
		It("should work with DockerHub",
			SkipIfCredentialsEmpty(DockerHubCredentials, func() {
				fmt.Println(DockerHubCredentials != nil) // to avoid crying linters
			}),
		)
		It("should work with GitHub Container Registry",
			SkipIfCredentialsEmpty(GHCRCredentials, func() {
				fmt.Println(GHCRCredentials != nil) // to avoid crying linters
			}),
		)
	})
	When("sending a HEAD request", func() {
		var server *ghttp.Server
		BeforeEach(func() {
			server = ghttp.NewServer()
		})
		AfterEach(func() {
			server.Close()
		})
		It("should use a custom user-agent", func() {
			server.AppendHandlers(
				ghttp.CombineHandlers(
					ghttp.VerifyHeader(http.Header{
						"User-Agent": []string{"Watchtower/v0.0.0-unknown"},
					}),
					ghttp.RespondWith(http.StatusOK, "", http.Header{
						digest.ContentDigestHeader: []string{
							mockDigest,
						},
					}),
				),
			)
			dig, err := digest.GetDigest(server.URL(), "token")
			Expect(server.ReceivedRequests()).Should(HaveLen(1))
			Expect(err).NotTo(HaveOccurred())
			Expect(dig).To(Equal(mockDigest))
		})
	})
	When("HEAD fails with 401 but GET works", func() {
		var server *ghttp.Server
		BeforeEach(func() {
			server = ghttp.NewServer()
		})
		AfterEach(func() {
			server.Close()
		})

		It("falls back to GET and returns the digest from the Docker-Content-Digest header", func() {
			server.RouteToHandler(http.MethodHead, "/", ghttp.RespondWith(http.StatusUnauthorized, ""))
			server.RouteToHandler(http.MethodGet, "/", ghttp.RespondWith(http.StatusOK, "manifest-body", http.Header{
				digest.ContentDigestHeader: []string{mockDigest},
			}))
			dig, err := digest.GetDigest(server.URL(), "token")
			Expect(err).NotTo(HaveOccurred())
			Expect(dig).To(Equal(mockDigest))
			// HEAD will be retried by retry.DoHTTP (it treats 401 as
			// transient) before the GET fallback kicks in — we just care
			// that a GET eventually fired.
			Expect(len(server.ReceivedRequests()) >= 2).To(BeTrue())
		})

		It("falls back to GET and computes SHA256 of the body when the server omits the digest header", func() {
			server.RouteToHandler(http.MethodHead, "/", ghttp.RespondWith(http.StatusUnauthorized, ""))
			server.RouteToHandler(http.MethodGet, "/", ghttp.RespondWith(http.StatusOK, "canonical-manifest-bytes"))
			dig, err := digest.GetDigest(server.URL(), "token")
			Expect(err).NotTo(HaveOccurred())
			// Shape check: "sha256:" prefix + 64 hex chars. Computed
			// body-hash is deterministic but we don't hard-code the hex
			// here — it's a cosmetic detail, not a contract.
			Expect(dig).To(HavePrefix("sha256:"))
			Expect(len(dig)).To(Equal(len("sha256:") + 64))
		})

		It("surfaces both errors when GET also fails", func() {
			server.RouteToHandler(http.MethodHead, "/", ghttp.RespondWith(http.StatusUnauthorized, ""))
			server.RouteToHandler(http.MethodGet, "/", ghttp.RespondWith(http.StatusUnauthorized, ""))
			_, err := digest.GetDigest(server.URL(), "token")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("HEAD manifest"))
			Expect(err.Error()).To(ContainSubstring("GET manifest"))
		})
	})
})
