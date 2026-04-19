// Package digest compares the current image digest to the latest one published
// in the registry to decide whether a container is stale.
package digest

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strings"

	"github.com/sirupsen/logrus"

	"github.com/openserbia/watchtower/internal/meta"
	"github.com/openserbia/watchtower/pkg/metrics"
	"github.com/openserbia/watchtower/pkg/registry/auth"
	"github.com/openserbia/watchtower/pkg/registry/manifest"
	"github.com/openserbia/watchtower/pkg/registry/retry"
	"github.com/openserbia/watchtower/pkg/registry/transport"
	"github.com/openserbia/watchtower/pkg/types"
)

// ContentDigestHeader is the key for the key-value pair containing the digest header
const ContentDigestHeader = "Docker-Content-Digest"

// manifestAccept is the set of media types we announce on both HEAD and GET
// manifest requests. The order is significant — registries pick the first
// one they support.
var manifestAccept = []string{
	"application/vnd.docker.distribution.manifest.v2+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.docker.distribution.manifest.v1+json",
	"application/vnd.oci.image.index.v1+json",
}

// CompareDigest ...
func CompareDigest(container types.Container, registryAuth string) (bool, error) {
	if !container.HasImageInfo() {
		return false, errors.New("container image info missing")
	}

	var digest string

	registryAuth = TransformAuth(registryAuth)
	token, err := auth.GetToken(container, registryAuth)
	if err != nil {
		return false, err
	}

	digestURL, err := manifest.BuildManifestURL(container)
	if err != nil {
		return false, err
	}

	if digest, err = GetDigest(digestURL, token); err != nil {
		return false, err
	}

	logrus.WithField("remote", digest).Debug("Found a remote digest to compare with")

	for _, dig := range container.ImageInfo().RepoDigests {
		localDigest := strings.Split(dig, "@")[1]
		fields := logrus.Fields{"local": localDigest, "remote": digest}
		logrus.WithFields(fields).Debug("Comparing")

		if localDigest == digest {
			logrus.Debug("Found a match")
			return true, nil
		}
	}

	return false, nil
}

// TransformAuth from a base64 encoded json object to base64 encoded string
func TransformAuth(registryAuth string) string {
	b, _ := base64.StdEncoding.DecodeString(registryAuth)
	credentials := &types.RegistryCredentials{}
	_ = json.Unmarshal(b, credentials)

	if credentials.Username != "" && credentials.Password != "" {
		ba := []byte(fmt.Sprintf("%s:%s", credentials.Username, credentials.Password))
		registryAuth = base64.StdEncoding.EncodeToString(ba)
	}

	return registryAuth
}

// GetDigest fetches the remote manifest digest for the given URL. Tries a
// HEAD request first — cheapest on bandwidth and friendliest to registry
// rate-limits — then falls back to GET if HEAD fails.
//
// The fallback matters: some registries (notably GHCR and Docker Hub under
// certain anonymous-pull conditions) answer HEAD with 401/405 for manifests
// that GET serves fine with the exact same token. Without the fallback we'd
// kick over to a full `docker pull` to compare digests, which is dramatically
// more expensive than a GET of the manifest alone.
//
// On GET, if the registry omits the `Docker-Content-Digest` header, we
// compute the digest as sha256 of the manifest body ourselves — the OCI
// image-spec guarantees the two match for the canonical bytes the registry
// returns.
func GetDigest(url, token string) (string, error) {
	if token == "" {
		return "", errors.New("could not fetch token")
	}

	digest, headErr := fetchManifestDigest(url, token, http.MethodHead)
	if headErr == nil {
		return digest, nil
	}

	logrus.WithError(headErr).WithField("url", url).Debug("HEAD manifest failed — falling back to GET")
	digest, getErr := fetchManifestDigest(url, token, http.MethodGet)
	if getErr != nil {
		// Surface both errors so operators can tell whether the issue was
		// HEAD-specific (recoverable, GET may have worked once) or a real
		// registry/credential problem.
		return "", fmt.Errorf("HEAD manifest: %w (then GET manifest: %v)", headErr, getErr)
	}
	return digest, nil
}

// fetchManifestDigest runs a single manifest request with the given HTTP
// method and returns the digest from the response. GET requests carry the
// full retry budget; HEAD is best-effort with a single attempt because the
// GET fallback path handles any real failure, and retrying HEAD on a
// registry that just doesn't speak HEAD is pure latency.
func fetchManifestDigest(url, token, method string) (string, error) {
	parsed, err := neturl.Parse(url)
	if err != nil {
		return "", fmt.Errorf("parse manifest URL %q: %w", url, err)
	}
	client := transport.Client(parsed.Host)

	req, err := http.NewRequestWithContext(context.Background(), method, url, nil)
	if err != nil {
		return "", fmt.Errorf("build %s request: %w", method, err)
	}
	req.Header.Set("User-Agent", meta.UserAgent)
	req.Header.Add("Authorization", token)
	for _, mediaType := range manifestAccept {
		req.Header.Add("Accept", mediaType)
	}

	operation := "digest_head"
	if method == http.MethodGet {
		operation = "digest_get"
	}

	logrus.WithFields(logrus.Fields{"url": url, "method": method}).Debug("Fetching manifest digest")

	var res *http.Response
	if method == http.MethodHead {
		// Best-effort single attempt. Any non-2xx (including 401/405) falls
		// straight through to the GET path in GetDigest. Emit the metric
		// manually since we're bypassing retry.DoHTTP's instrumentation.
		res, err = client.Do(req)
		if err != nil {
			metrics.RegisterRegistryRequest(parsed.Host, operation, "error")
		} else {
			outcome := "success"
			if res.StatusCode != http.StatusOK {
				outcome = "error"
			}
			metrics.RegisterRegistryRequest(parsed.Host, operation, outcome)
		}
	} else {
		res, err = retry.DoHTTP(client, req, operation, logrus.WithField("url", url))
	}
	if err != nil {
		return "", err
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusOK {
		wwwAuthHeader := res.Header.Get("www-authenticate")
		if wwwAuthHeader == "" {
			wwwAuthHeader = "not present"
		}
		return "", fmt.Errorf("registry responded to %s request with %q, auth: %q", method, res.Status, wwwAuthHeader)
	}

	if d := res.Header.Get(ContentDigestHeader); d != "" {
		return d, nil
	}

	// No Docker-Content-Digest. For GET we can recover by hashing the body.
	// HEAD has no body, so if the header is missing on HEAD the caller has
	// to retry with GET (which this function wouldn't know to do on its own
	// — the orchestrator in GetDigest handles that).
	if method != http.MethodGet {
		return "", errors.New("registry returned 200 but no Docker-Content-Digest header")
	}

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return "", fmt.Errorf("read manifest body: %w", err)
	}
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
