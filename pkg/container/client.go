package container

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"os"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/versions"
	sdkClient "github.com/docker/docker/client"
	log "github.com/sirupsen/logrus"

	"github.com/openserbia/watchtower/pkg/metrics"
	"github.com/openserbia/watchtower/pkg/registry"
	"github.com/openserbia/watchtower/pkg/registry/digest"
	t "github.com/openserbia/watchtower/pkg/types"
)

const defaultStopSignal = "SIGTERM"

// A Client is the interface through which watchtower interacts with the
// Docker API.
type Client interface {
	ListContainers(t.Filter) ([]t.Container, error)
	GetContainer(containerID t.ContainerID) (t.Container, error)
	StopContainer(t.Container, time.Duration) error
	StartContainer(t.Container) (t.ContainerID, error)
	RenameContainer(t.Container, string) error
	IsContainerStale(t.Container, t.UpdateParams) (stale bool, latestImage t.ImageID, err error)
	ExecuteCommand(containerID t.ContainerID, command string, timeout int) (skipUpdate bool, err error)
	RemoveImageByID(t.ImageID) error
	WarnOnHeadPullFailed(container t.Container) bool
	// WatchImageEvents opens a stream of image-lifecycle events (tag, load)
	// from the Docker daemon. The caller cancels the ctx to close the stream;
	// the error channel emits once and is closed when the stream terminates.
	// Reconnection is the caller's responsibility.
	WatchImageEvents(ctx context.Context) (<-chan t.ImageEvent, <-chan error)
}

// NewClient returns a new Client instance which can be used to interact with
// the Docker API.
// The client reads its configuration from the following environment variables:
//   - DOCKER_HOST			the docker-engine host to send api requests to
//   - DOCKER_TLS_VERIFY		whether to verify tls certificates
//   - DOCKER_API_VERSION	the docker api version to pin the client to (skips negotiation when set)
//
// When DOCKER_API_VERSION is unset, the client negotiates down to the daemon's
// reported version on first use so the same binary works against both older and
// newer daemons (including Docker Engine 29+, whose minimum API floor is 1.44).
// We then opportunistically raise the negotiated version to reach fields the
// vendored SDK's DefaultVersion can't; see upgradeAPIVersionForFeatures.
func NewClient(opts ClientOptions) Client {
	cli, err := sdkClient.NewClientWithOpts(sdkClient.FromEnv, sdkClient.WithAPIVersionNegotiation())
	if err != nil {
		log.Fatalf("Error instantiating Docker client: %s", err)
	}

	upgradeAPIVersionForFeatures(context.Background(), cli)

	return dockerClient{
		api:           cli,
		ClientOptions: opts,
	}
}

const (
	// minFeatureAPIVersion is the lowest Docker API version that exposes
	// fields Watchtower consumes through raw JSON decoding (currently: the
	// Identity provenance record on /images/{id}/json, which lets the
	// containerd image store distinguish locally-built images from Hub
	// pulls). Below this, there is no point raising the version.
	minFeatureAPIVersion = "1.53"
	// preferredFeatureAPIVersion caps the opportunistic raise to a version
	// we have tested against. Keeping it tight avoids drifting into
	// API territory that might introduce request-body changes the vendored
	// SDK can't formulate correctly. Revisit when the SDK is bumped.
	preferredFeatureAPIVersion = "1.54"

	// dockerAPITimeout is an upper bound on individual Docker daemon
	// management calls (list / inspect / create / kill / remove / rename /
	// network attach / image-tag / start). Healthy daemons answer in
	// milliseconds; this exists only to keep a single hung syscall from
	// wedging the scan loop forever — typically when the daemon socket
	// accepts the connection but doesn't respond, e.g. during a partial
	// engine upgrade or a hung containerd snapshotter. Set generously
	// because the SDK call returning doesn't cancel the daemon's work
	// (the daemon may still create / remove the resource), so a too-tight
	// timeout can leave torn state without recovering anything.
	dockerAPITimeout = 2 * time.Minute

	// imageRemoveTimeout grants extra headroom over dockerAPITimeout because
	// ImageRemove on disk-heavy multi-layer images can take noticeably
	// longer than other API calls — the daemon's snapshot teardown is
	// disk-bound, not network-bound, and slower hosts have legitimately
	// hit the dockerAPITimeout default during routine --cleanup runs.
	imageRemoveTimeout = 5 * time.Minute

	// imagePullTimeout bounds the IsContainerStale path, which streams the
	// pull. Generous because legitimate pulls of multi-GB images on slow
	// links can take a long time; the goal is only to recover from a
	// truly hung daemon, not to time out real-world pulls. Operators with
	// pulls that consistently exceed this window will see staleness
	// detection back off, which is the correct signal to size the link
	// or the image — not to silently extend forever.
	imagePullTimeout = 30 * time.Minute
)

// upgradeAPIVersionForFeatures opportunistically raises the client's API
// version above the SDK's DefaultVersion so we can access response fields
// added after the SDK was vendored — currently Identity on the image-inspect
// response (v1.53+). The mechanism is safe in the narrow sense we use it:
//   - URLs already use client.version as a plain path component, so any
//     well-formed version string the daemon accepts works.
//   - Unknown fields in JSON responses are silently dropped by the SDK's
//     typed Unmarshal; code that needs the new fields uses the raw-response
//     option instead.
//   - Explicit user pins via DOCKER_API_VERSION are left untouched.
//
// No-op when the daemon doesn't advertise a version, the ping fails, or the
// daemon's max is already below minFeatureAPIVersion. Any error leaves the
// negotiated version in place — best-effort only.
func upgradeAPIVersionForFeatures(ctx context.Context, cli *sdkClient.Client) {
	if os.Getenv(sdkClient.EnvOverrideAPIVersion) != "" {
		// Explicit pin; respect the operator's choice even if it forgoes
		// newer features.
		return
	}
	ping, err := cli.Ping(ctx)
	if err != nil {
		log.WithError(err).Debug("Daemon ping failed; leaving negotiated API version in place")
		return
	}
	if ping.APIVersion == "" {
		return
	}
	if versions.LessThan(ping.APIVersion, minFeatureAPIVersion) {
		return
	}
	target := preferredFeatureAPIVersion
	if versions.LessThan(ping.APIVersion, target) {
		target = ping.APIVersion
	}
	if err := sdkClient.WithVersion(target)(cli); err != nil {
		log.WithError(err).Debugf("Could not pin client to API %s; leaving negotiated version", target)
		return
	}
	log.Debugf("Pinned Docker client to API v%s for containerd-snapshotter Identity support (daemon advertises v%s)", target, ping.APIVersion)
}

// ClientOptions contains the options for how the docker client wrapper should behave
type ClientOptions struct {
	RemoveVolumes     bool
	IncludeStopped    bool
	ReviveStopped     bool
	IncludeRestarting bool
	// DisableMemorySwappiness, when set, nils HostConfig.MemorySwappiness on
	// recreate. Podman + crun on cgroupv2 rejects the implicit `0` Docker
	// writes when the field is unset, so a Watchtower recreate that copies
	// the inspected HostConfig back through ContainerCreate fails with
	// `swappiness must be in the range [0, 100]`. Opt-in: Docker behavior
	// is unchanged when this is left false.
	DisableMemorySwappiness bool
	WarnOnHeadFailed        WarningStrategy
}

// WarningStrategy is a value determining when to show warnings
type WarningStrategy string

const (
	// WarnAlways warns whenever the problem occurs
	WarnAlways WarningStrategy = "always"
	// WarnNever never warns when the problem occurs
	WarnNever WarningStrategy = "never"
	// WarnAuto skips warning when the problem was expected
	WarnAuto WarningStrategy = "auto"
)

type dockerClient struct {
	api sdkClient.APIClient
	ClientOptions
}

func (client dockerClient) WarnOnHeadPullFailed(container t.Container) bool {
	if client.WarnOnHeadFailed == WarnAlways {
		return true
	}
	if client.WarnOnHeadFailed == WarnNever {
		return false
	}

	return registry.WarnOnAPIConsumption(container)
}

func (client dockerClient) ListContainers(fn t.Filter) ([]t.Container, error) {
	cs := []t.Container{}
	ctx, cancel := context.WithTimeout(context.Background(), dockerAPITimeout)
	defer cancel()

	switch {
	case client.IncludeStopped && client.IncludeRestarting:
		log.Debug("Retrieving running, stopped, restarting and exited containers")
	case client.IncludeStopped:
		log.Debug("Retrieving running, stopped and exited containers")
	case client.IncludeRestarting:
		log.Debug("Retrieving running and restarting containers")
	default:
		log.Debug("Retrieving running containers")
	}

	filter := client.createListFilter()
	containers, err := listContainersWithRetry(ctx, client.api, container.ListOptions{Filters: filter})
	if err != nil {
		metrics.RegisterDockerAPIError("list")
		return nil, err
	}

	for _, runningContainer := range containers {
		c, err := client.GetContainer(t.ContainerID(runningContainer.ID))
		if err != nil {
			// Container vanished between list and inspect — typically a manual
			// `docker compose up` recreated it under a new ID. The next poll
			// will pick the replacement up; aborting the whole scan here would
			// drown genuine failures in churn-induced noise.
			if cerrdefs.IsNotFound(err) {
				log.Debugf("Container %s disappeared between list and inspect, skipping.", t.ContainerID(runningContainer.ID).ShortID())
				continue
			}
			return nil, err
		}

		if fn(c) {
			cs = append(cs, c)
		}
	}

	return cs, nil
}

// List-retry tunables — mirror pkg/registry/retry so the retry windows feel
// the same across the two surfaces. Kept as vars so tests can shrink them
// without waiting out real backoff.
var (
	listBackoffBase   = 500 * time.Millisecond
	listBackoffMax    = 4 * time.Second
	listBackoffJitter = 0.25
)

const listMaxAttempts = 3

// listContainersWithRetry wraps ContainerList in a bounded exponential backoff
// so a single transient daemon flake (daemon restart during poll, socket blip,
// 5xx from the Docker engine API) doesn't fail the whole scan cycle. Retries
// on network errors and the Docker errdefs class of transient server/daemon
// errors. Bails immediately on context cancellation, deadline, or caller-fault
// errors that won't clear with a retry.
func listContainersWithRetry(ctx context.Context, api sdkClient.APIClient, opts container.ListOptions) ([]container.Summary, error) {
	var lastErr error
	for attempt := 0; attempt < listMaxAttempts; attempt++ {
		if attempt > 0 {
			metrics.RegisterDockerAPIRetry("list")
			delay := listBackoffFor(attempt)
			log.Debugf("Retrying Docker ContainerList after %s (attempt %d/%d)", delay, attempt+1, listMaxAttempts)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		containers, err := api.ContainerList(ctx, opts)
		if err == nil {
			return containers, nil
		}
		lastErr = err

		if !isTransientDockerErr(err) {
			return nil, err
		}
	}
	return nil, lastErr
}

func isTransientDockerErr(err error) bool {
	if err == nil {
		return false
	}
	// Caller-fault or explicit cancellation — no point retrying.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if cerrdefs.IsInvalidArgument(err) || cerrdefs.IsNotFound(err) || cerrdefs.IsPermissionDenied(err) {
		return false
	}
	// Transient classes: server-side errors, daemon unavailable, plus any
	// non-errdefs error (typically a raw network failure from the SDK's HTTP
	// layer before a structured error could be decoded).
	if cerrdefs.IsInternal(err) || cerrdefs.IsUnavailable(err) {
		return true
	}
	// Conservative default: network/IO errors from the SDK client are
	// retryable. The errdefs classifiers return false for these because
	// they're below the Docker error envelope.
	return true
}

func listBackoffFor(attempt int) time.Duration {
	delay := listBackoffBase * (1 << (attempt - 1))
	if delay > listBackoffMax {
		delay = listBackoffMax
	}
	jitter := time.Duration(float64(delay) * listBackoffJitter * (mrand.Float64()*2 - 1))
	return delay + jitter
}

func (client dockerClient) createListFilter() filters.Args {
	filterArgs := filters.NewArgs()
	filterArgs.Add("status", "running")

	if client.IncludeStopped {
		filterArgs.Add("status", "created")
		filterArgs.Add("status", "exited")
	}

	if client.IncludeRestarting {
		filterArgs.Add("status", "restarting")
	}

	return filterArgs
}

func (client dockerClient) GetContainer(containerID t.ContainerID) (t.Container, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dockerAPITimeout)
	defer cancel()

	containerInfo, err := client.api.ContainerInspect(ctx, string(containerID))
	if err != nil {
		recordDaemonError("inspect", err, cerrdefs.IsNotFound)
		return &Container{}, err
	}

	netType, netContainerID, found := strings.Cut(string(containerInfo.HostConfig.NetworkMode), ":")
	if found && netType == "container" {
		parentContainer, err := client.api.ContainerInspect(ctx, netContainerID)
		if err != nil {
			recordDaemonError("inspect", err, cerrdefs.IsNotFound)
			log.WithFields(map[string]interface{}{
				"container":         containerInfo.Name,
				"error":             err,
				"network-container": netContainerID,
			}).Warnf("Unable to resolve network container: %v", err)
		} else {
			// Replace the container ID with a container name to allow it to reference the re-created network container
			containerInfo.HostConfig.NetworkMode = container.NetworkMode(fmt.Sprintf("container:%s", parentContainer.Name))
		}
	}

	imageInfo, identity, err := client.inspectImageWithIdentity(ctx, containerInfo.Image)
	if err != nil {
		// The image the container was created from may have been garbage-collected
		// off disk (e.g. a previous --cleanup run after the tag was moved to a
		// newer digest). Fall back to inspecting by the image reference the
		// container was created with — usually a name:tag that now points at the
		// freshly-pulled digest — so updates can still proceed. Only count the
		// docker_api_errors metric on the final outcome so a recoverable GC
		// doesn't churn the WatchtowerDockerAPIErrorsSustained alert, and
		// NotFound responses never count (see recordDaemonInspectError).
		if ref := containerInfo.Config.Image; ref != "" && ref != containerInfo.Image && !strings.HasPrefix(ref, "sha256:") {
			if fallbackInfo, fallbackIdentity, fallbackErr := client.inspectImageWithIdentity(ctx, ref); fallbackErr == nil {
				metrics.RegisterImageFallback()
				log.Warnf("Image %s for container %s is missing locally; falling back to %q for config", containerInfo.Image, containerInfo.Name, ref)
				c := &Container{containerInfo: &containerInfo, imageInfo: &fallbackInfo}
				c.SetImageIdentity(fallbackIdentity)
				return c, nil
			}
		}
		recordDaemonError("image_inspect", err, cerrdefs.IsNotFound)
		log.Warnf("Failed to retrieve container image info: %v", err)
		return &Container{containerInfo: &containerInfo, imageInfo: nil}, nil
	}

	c := &Container{containerInfo: &containerInfo, imageInfo: &imageInfo}
	c.SetImageIdentity(identity)
	return c, nil
}

// applyRecreatePolicy applies client-level recreate-time mutations to the
// HostConfig before ContainerCreate. The inspected HostConfig that comes
// back from the daemon is mostly safe to round-trip, but a few fields need
// host-runtime-specific cleanup so they don't trip ContainerCreate. Today
// this is just the Podman/cgroupv2 MemorySwappiness fix; future
// host-targeted carve-outs belong here so StartContainer stays linear.
func applyRecreatePolicy(opts ClientOptions, hc *container.HostConfig) {
	if hc == nil {
		return
	}
	if opts.DisableMemorySwappiness {
		// Podman + crun on cgroupv2 rejects MemorySwappiness=0 when the
		// field was never explicitly set. Docker writes 0 anyway as the
		// inspected default, so we'd otherwise feed a value back in that
		// the host runtime considers invalid. A nil pointer signals
		// "operator did not set this," which Podman accepts.
		hc.MemorySwappiness = nil
	}
}

// recordDaemonError increments docker_api_errors_total only when the error
// signals a daemon-health problem. Expected-error classes passed via the
// variadic predicates represent logical answers from the daemon — not
// infrastructure failures — and counting them would churn
// WatchtowerDockerAPIErrorsSustained with routine Compose recreations,
// --cleanup retags, self-update overlaps, and shared-base-image races.
// The alert's threat model is daemon *health* (socket permission drift,
// overload, partial upgrade), not logical misses.
//
// Typical usage:
//
//	recordDaemonError("inspect", err, cerrdefs.IsNotFound)       // container vanished
//	recordDaemonError("image_inspect", err, cerrdefs.IsNotFound) // image GC'd
//	recordDaemonError("image_remove", err, cerrdefs.IsConflict)  // image still in use
func recordDaemonError(operation string, err error, expected ...func(error) bool) {
	if err == nil {
		return
	}
	for _, isExpected := range expected {
		if isExpected(err) {
			return
		}
	}
	metrics.RegisterDockerAPIError(operation)
}

// inspectImageWithIdentity wraps ImageInspect with the raw-response option so
// the Identity field (populated only by the containerd image store) can be
// decoded separately. The vendored Docker SDK's typed InspectResponse omits
// it, so we parse a narrow shim struct from the raw JSON. Returns a nil
// *ImageIdentity when the field is absent or blank — callers treat that as
// "signal unavailable" and fall back to the RepoDigests heuristic.
//
// Short-circuits when the negotiated API version is below the minimum that
// exposes the field (v1.53). No point capturing the raw body or running a
// second JSON unmarshal to look for something the daemon can't return at
// that version — the RepoDigests fallback in ImageIsLocal and the
// pull-error safeguard in IsContainerStale both fire without it.
func (client dockerClient) inspectImageWithIdentity(ctx context.Context, imageRef string) (image.InspectResponse, *ImageIdentity, error) {
	if versions.LessThan(client.api.ClientVersion(), minFeatureAPIVersion) {
		info, err := client.api.ImageInspect(ctx, imageRef)
		return info, nil, err
	}
	var raw bytes.Buffer
	info, err := client.api.ImageInspect(ctx, imageRef, sdkClient.ImageInspectWithRawResponse(&raw))
	if err != nil {
		return info, nil, err
	}
	return info, decodeImageIdentity(raw.Bytes()), nil
}

// decodeImageIdentity pulls the "Identity" object out of an /images/{id}/json
// response body. Returns nil when the field is missing (older daemons), when
// it is present but structurally empty, or when the body is not decodable —
// the caller should then rely on the RepoDigests fallback.
func decodeImageIdentity(raw []byte) *ImageIdentity {
	if len(raw) == 0 {
		return nil
	}
	var envelope struct {
		Identity *ImageIdentity `json:"Identity,omitempty"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil
	}
	if envelope.Identity == nil {
		return nil
	}
	if len(envelope.Identity.Build) == 0 && len(envelope.Identity.Pull) == 0 {
		return nil
	}
	return envelope.Identity
}

func (client dockerClient) StopContainer(c t.Container, timeout time.Duration) error {
	signal := c.StopSignal()
	if signal == "" {
		signal = defaultStopSignal
	}

	idStr := string(c.ID())
	shortID := c.ID().ShortID()

	// Honor the container's own StopTimeout (from `docker run --stop-timeout`
	// or Compose's `stop_grace_period`) when set — matches Docker's precedence
	// of per-container over daemon default. Fall back to watchtower's global
	// --stop-timeout.
	if perContainer := c.StopTimeout(); perContainer > 0 {
		log.Debugf("Using per-container stop timeout of %s for %s (global was %s)", perContainer, c.Name(), timeout)
		timeout = perContainer
	}

	// Bound the entire stop+remove path. The grace period gates each of
	// the two in-loop waitForStopOrTimeout calls independently (one before
	// Remove, one after), so the outer ctx must cover *both* full grace
	// windows plus dockerAPITimeout of headroom for the Kill, Remove, and
	// any individual ContainerInspect that hangs. Without the headroom a
	// user-supplied grace of N minutes would leave zero slack for the
	// daemon calls and the outer ctx would race the second wait's normal
	// timeout to fire first.
	ctx, cancel := context.WithTimeout(context.Background(), 2*timeout+dockerAPITimeout)
	defer cancel()

	if c.IsRunning() {
		log.Infof("Stopping %s (%s) with %s", c.Name(), shortID, signal)
		if err := client.api.ContainerKill(ctx, idStr, signal); err != nil {
			if cerrdefs.IsNotFound(err) {
				log.Debugf("Container %s already gone before kill, nothing to stop.", shortID)
				return ErrContainerNotFound
			}
			metrics.RegisterDockerAPIError("kill")
			return err
		}
	}

	// TODO: This should probably be checked.
	_ = client.waitForStopOrTimeout(ctx, c, timeout)

	if c.ContainerInfo().HostConfig.AutoRemove {
		log.Debugf("AutoRemove container %s, skipping ContainerRemove call.", shortID)
	} else {
		log.Debugf("Removing container %s", shortID)

		if err := client.api.ContainerRemove(ctx, idStr, container.RemoveOptions{Force: true, RemoveVolumes: client.RemoveVolumes}); err != nil {
			if cerrdefs.IsNotFound(err) {
				log.Debugf("Container %s not found, skipping removal.", shortID)
				return ErrContainerNotFound
			}
			metrics.RegisterDockerAPIError("remove")
			return err
		}
	}

	// Wait for container to be removed. In this case an error is a good thing
	if err := client.waitForStopOrTimeout(ctx, c, timeout); err == nil {
		return fmt.Errorf("container %s (%s) could not be removed", c.Name(), shortID)
	}

	return nil
}

func (client dockerClient) GetNetworkConfig(c t.Container) *network.NetworkingConfig {
	config := &network.NetworkingConfig{
		EndpointsConfig: c.ContainerInfo().NetworkSettings.Networks,
	}

	for _, ep := range config.EndpointsConfig {
		aliases := make([]string, 0, len(ep.Aliases))
		cidAlias := c.ID().ShortID()

		// Remove the old container ID alias from the network aliases, as it would accumulate across updates otherwise
		for _, alias := range ep.Aliases {
			if alias == cidAlias {
				continue
			}
			aliases = append(aliases, alias)
		}

		ep.Aliases = aliases
	}
	return config
}

func (client dockerClient) StartContainer(c t.Container) (t.ContainerID, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dockerAPITimeout)
	defer cancel()

	config := c.GetCreateConfig()
	hostConfig := c.GetCreateHostConfig()
	networkConfig := client.GetNetworkConfig(c)

	applyRecreatePolicy(client.ClientOptions, hostConfig)

	// simpleNetworkConfig is a networkConfig with only 1 network.
	// see: https://github.com/docker/docker/issues/29265
	simpleNetworkConfig := func() *network.NetworkingConfig {
		oneEndpoint := make(map[string]*network.EndpointSettings)
		for k, v := range networkConfig.EndpointsConfig {
			oneEndpoint[k] = v
			// we only need 1
			break
		}
		return &network.NetworkingConfig{EndpointsConfig: oneEndpoint}
	}()

	name := c.Name()

	// Re-bind the original tag to the digest IsContainerStale resolved, so a
	// CI rebuild that untagged or moved name:latest between scan and create
	// can't leave us with "No such image" or with an unintended digest.
	// Idempotent and local-only — no-op if the tag still points where we
	// expect, instant overwrite otherwise. We call this after Stop has already
	// removed the previous container, so the tiny window between tag and
	// create can no longer be widened by a slow stop.
	if target := c.TargetImageID(); target != "" {
		if err := client.api.ImageTag(ctx, string(target), config.Image); err != nil {
			// A failure here is non-fatal: ContainerCreate will surface the
			// real problem (or succeed if the tag was already correct). Log
			// and count, but don't abort the recreate — the existing
			// behavior is to lean on ContainerCreate's error path.
			metrics.RegisterDockerAPIError("image_tag")
			log.WithError(err).WithFields(log.Fields{
				"image":  config.Image,
				"digest": string(target),
			}).Debug("Image retag before create failed; proceeding with original tag")
		}
	}

	log.Infof("Creating %s", name)

	createdContainer, err := client.api.ContainerCreate(ctx, config, hostConfig, simpleNetworkConfig, nil, name)
	if err != nil {
		metrics.RegisterDockerAPIError("create")
		return "", err
	}

	if !(hostConfig.NetworkMode.IsHost()) {
		for k := range simpleNetworkConfig.EndpointsConfig {
			err = client.api.NetworkDisconnect(ctx, k, createdContainer.ID, true)
			if err != nil {
				metrics.RegisterDockerAPIError("network_disconnect")
				return "", err
			}
		}

		for k, v := range networkConfig.EndpointsConfig {
			err = client.api.NetworkConnect(ctx, k, createdContainer.ID, v)
			if err != nil {
				metrics.RegisterDockerAPIError("network_connect")
				return "", err
			}
		}
	}

	createdContainerID := t.ContainerID(createdContainer.ID)
	if !c.IsRunning() && !client.ReviveStopped {
		return createdContainerID, nil
	}

	return createdContainerID, client.doStartContainer(ctx, c, createdContainer)
}

func (client dockerClient) doStartContainer(ctx context.Context, c t.Container, creation container.CreateResponse) error {
	name := c.Name()

	log.Debugf("Starting container %s (%s)", name, t.ContainerID(creation.ID).ShortID())
	err := client.api.ContainerStart(ctx, creation.ID, container.StartOptions{})
	if err != nil {
		metrics.RegisterDockerAPIError("start")
		return err
	}
	return nil
}

func (client dockerClient) RenameContainer(c t.Container, newName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), dockerAPITimeout)
	defer cancel()
	log.Debugf("Renaming container %s (%s) to %s", c.Name(), c.ID().ShortID(), newName)
	if err := client.api.ContainerRename(ctx, string(c.ID()), newName); err != nil {
		metrics.RegisterDockerAPIError("rename")
		return err
	}
	return nil
}

func (client dockerClient) IsContainerStale(container t.Container, params t.UpdateParams) (stale bool, latestImage t.ImageID, err error) {
	// Use the longer pull-bound timeout — IsContainerStale streams the pull
	// when the digest doesn't match, and that legitimately takes minutes for
	// large images. The dockerAPITimeout default would cut healthy pulls off
	// mid-stream, leaving the scan flapping rather than completing.
	ctx, cancel := context.WithTimeout(context.Background(), imagePullTimeout)
	defer cancel()

	switch {
	case container.IsNoPull(params):
		log.Debugf("Skipping image pull.")
	case container.ImageIsLocal():
		// Image was produced by this daemon (`docker build` / `docker load`)
		// and never came from a registry. Pulling is guaranteed to fail
		// ("No such image" on the classic docker-image-store; "pull access
		// denied" on the containerd image store, where local builds resolve
		// to a bare-name reference that Docker Hub rejects) and only produces
		// log noise. HasNewImage still works: rebuilds retag the image, the
		// ID behind the tag changes, and the next poll picks up the
		// difference. Out-of-the-box replacement for the older workaround of
		// setting --no-pull or the per-container no-pull label.
		log.Debugf("Skipping image pull for %s: locally built or loaded.", container.Name())
	default:
		if pullErr := client.PullImage(ctx, container); pullErr != nil {
			if pullFailureLooksLocal(container.ImageName(), pullErr) {
				// Safeguard: Identity provenance isn't available on every
				// Docker version (the response field only appears at API
				// v1.53+). For older daemons and edge cases where Identity
				// is absent, a pull failure on a bare-name reference ("app",
				// not "ghcr.io/foo/app") plus a definitive "not found" from
				// the registry is a strong signal that the image is local —
				// the only registry a bare name normalizes to is Docker Hub,
				// and if Hub says no such repo, it's locally built.
				// Hostname-qualified references never hit this path, so
				// typos and broken private-registry creds still surface
				// loudly instead of being silently masked. We deliberately
				// skip the docker_api_errors counter here: a local build
				// the daemon can't pull isn't an API failure, it's the
				// daemon correctly reporting the state.
				log.Debugf("Pull rejected for %s (%v); treating as locally built and falling back to HasNewImage", container.Name(), pullErr)
			} else {
				metrics.RegisterDockerAPIError("image_pull")
				return false, container.SafeImageID(), pullErr
			}
		}
	}

	return client.HasNewImage(ctx, container)
}

// classifyPullError wraps a daemon ImagePull error with a typed sentinel
// when it carries a recognisable category (HTTP 401 → unauthorized, HTTP 404
// → not-found). The original error is kept in the chain via
// fmt.Errorf("%w: %w") so cerrdefs.IsUnauthorized / cerrdefs.IsNotFound
// continue to detect the underlying classification — that's what
// pullFailureLooksLocal relies on for the local-build safeguard. Errors that
// don't match a known category are returned untouched so existing call-sites
// that inspect the raw daemon error keep behaving identically.
func classifyPullError(imageName string, err error) error {
	if err == nil {
		return nil
	}
	switch {
	case cerrdefs.IsUnauthorized(err):
		return fmt.Errorf("%w: %s: %w", ErrPullImageUnauthorized, imageName, err)
	case cerrdefs.IsNotFound(err):
		return fmt.Errorf("%w: %s: %w", ErrPullImageNotFound, imageName, err)
	default:
		return err
	}
}

// pullFailureLooksLocal reports whether a PullImage error for the given
// image reference should be treated as a locally-built image rather than a
// real failure. It fires only when the image reference lacks a registry
// hostname AND the daemon's error classifies as a not-found, which scopes
// the silent-skip to the exact case the containerd image store creates for
// `docker build -t app:latest .` on modern engines.
func pullFailureLooksLocal(imageRef string, pullErr error) bool {
	if pullErr == nil {
		return false
	}
	if !cerrdefs.IsNotFound(pullErr) {
		return false
	}
	return !imageRefHasRegistryHost(imageRef)
}

// imageRefHasRegistryHost reports whether an image reference includes an
// explicit registry hostname, mirroring the daemon's splitDockerDomain
// rules exactly. The first "/"-separated segment is a hostname iff it
// contains "." (FQDN / IP), contains ":" (hostname:port), equals
// "localhost" (reserved), or has uppercase letters (since namespace
// components must be lowercase). Anything else — "app", "myorg/app",
// "tg-antispam:latest" — is a bare name that the daemon normalizes to
// docker.io/library/ or docker.io/myorg/ when pulling.
//
// We inline the rules rather than using reference.Parse because Parse's
// non-normalizing splitDomain regex classifies a single lowercase segment
// ("myorg") as a domain, which disagrees with what the daemon actually
// does when resolving the pull target — and the daemon's behavior is what
// we need to match, since we're predicting what the daemon will do next.
func imageRefHasRegistryHost(imageRef string) bool {
	// Strip digest suffix first (digests contain ":" which would confuse
	// head-split downstream). Tags come after the final path segment and
	// can't precede the first "/", so they don't need special handling.
	if i := strings.IndexByte(imageRef, '@'); i >= 0 {
		imageRef = imageRef[:i]
	}
	slash := strings.IndexByte(imageRef, '/')
	if slash < 0 {
		return false
	}
	head := imageRef[:slash]
	if head == "localhost" {
		return true
	}
	if strings.ContainsAny(head, ".:") {
		return true
	}
	if strings.ToLower(head) != head {
		return true
	}
	return false
}

func (client dockerClient) HasNewImage(ctx context.Context, container t.Container) (hasNew bool, latestImage t.ImageID, err error) {
	currentImageID := t.ImageID(container.ContainerInfo().Image)
	imageName := container.ImageName()

	newImageInfo, err := client.api.ImageInspect(ctx, imageName)
	if err != nil {
		metrics.RegisterDockerAPIError("image_inspect")
		return false, currentImageID, err
	}

	newImageID := t.ImageID(newImageInfo.ID)
	if newImageID == currentImageID {
		log.Debugf("No new images found for %s", container.Name())
		return false, currentImageID, nil
	}

	log.Infof("Found new %s image (%s)", imageName, newImageID.ShortID())
	return true, newImageID, nil
}

// PullImage pulls the latest image for the supplied container, optionally skipping if it's digest can be confirmed
// to match the one that the registry reports via a HEAD request
func (client dockerClient) PullImage(ctx context.Context, container t.Container) error {
	containerName := container.Name()
	imageName := container.ImageName()

	fields := log.Fields{
		"image":     imageName,
		"container": containerName,
	}

	if strings.HasPrefix(imageName, "sha256:") {
		return ErrPinnedImage
	}

	log.WithFields(fields).Debugf("Trying to load authentication credentials.")
	opts, err := registry.GetPullOptions(imageName)
	if err != nil {
		log.Debugf("Error loading authentication credentials %s", err)
		return err
	}
	if opts.RegistryAuth != "" {
		log.Debug("Credentials loaded")
	}

	log.WithFields(fields).Debugf("Checking if pull is needed")

	if match, err := digest.CompareDigest(container, opts.RegistryAuth); err != nil {
		headLevel := log.DebugLevel
		if client.WarnOnHeadPullFailed(container) {
			headLevel = log.WarnLevel
		}
		log.WithFields(fields).Logf(headLevel, "Could not do a head request for %q, falling back to regular pull.", imageName)
		log.WithFields(fields).Log(headLevel, "Reason: ", err)
	} else if match {
		log.Debug("No pull needed. Skipping image.")
		return nil
	} else {
		log.Debug("Digests did not match, doing a pull.")
	}

	log.WithFields(fields).Debugf("Pulling image")

	response, err := client.api.ImagePull(ctx, imageName, opts)
	if err != nil {
		// Metric increment deferred to the caller: a bare-name-not-found
		// that we recover from via the local-build safeguard shouldn't
		// count as a Docker API error, because it isn't one — the daemon
		// correctly reported a registry miss for an image it never had.
		switch {
		case cerrdefs.IsUnauthorized(err):
			log.WithError(err).WithFields(fields).Warn("Image pull failed: authentication required")
		case cerrdefs.IsNotFound(err):
			log.WithError(err).WithFields(fields).Debug("Image pull failed: image not found in registry")
		default:
			log.Debugf("Error pulling image %s, %s", imageName, err)
		}
		return classifyPullError(imageName, err)
	}

	defer func() { _ = response.Close() }()
	// the pull request will be aborted prematurely unless the response is read
	if _, err = io.ReadAll(response); err != nil {
		log.Error(err)
		return err
	}
	return nil
}

func (client dockerClient) RemoveImageByID(id t.ImageID) error {
	log.Infof("Removing image %s", id.ShortID())

	ctx, cancel := context.WithTimeout(context.Background(), imageRemoveTimeout)
	defer cancel()

	items, err := client.api.ImageRemove(
		ctx,
		string(id),
		image.RemoveOptions{
			Force: true,
		})
	if err != nil && cerrdefs.IsNotFound(err) {
		// The old image was already gone (e.g. a previous --cleanup run or a
		// manual docker rmi removed it). Treat as success — the end state
		// matches what we were trying to achieve.
		log.Debugf("Image %s already removed, skipping.", id.ShortID())
		return nil
	}
	if err != nil && cerrdefs.IsConflict(err) {
		// Another container still references this image. Common causes:
		// a shared base image in a Compose stack where the last referrer
		// hasn't been recreated yet; a self-update where the old and new
		// watchtower containers overlap for a window. The cleanupImages
		// deferral logic (internal/actions/update.go) guards the typical
		// cases against the scan view, but races with containers outside
		// the scan view still land here. Next poll will retry naturally
		// once the last referrer is gone or recreated — surface as debug
		// rather than an error so strict NOTIFICATIONS_LEVEL=error feeds
		// don't page on routine overlap, and don't count it as a daemon
		// API failure.
		log.WithField("image", id.ShortID()).Debugf("Image still in use, deferring removal: %v", err)
		return nil
	}
	recordDaemonError("image_remove", err, cerrdefs.IsNotFound, cerrdefs.IsConflict)

	if log.IsLevelEnabled(log.DebugLevel) {
		deleted := strings.Builder{}
		untagged := strings.Builder{}
		for _, item := range items {
			if item.Deleted != "" {
				if deleted.Len() > 0 {
					deleted.WriteString(`, `)
				}
				deleted.WriteString(t.ImageID(item.Deleted).ShortID())
			}
			if item.Untagged != "" {
				if untagged.Len() > 0 {
					untagged.WriteString(`, `)
				}
				untagged.WriteString(t.ImageID(item.Untagged).ShortID())
			}
		}
		fields := log.Fields{`deleted`: deleted.String(), `untagged`: untagged.String()}
		log.WithFields(fields).Debug("Image removal completed")
	}

	return err
}

func (client dockerClient) ExecuteCommand(containerID t.ContainerID, command string, timeout int) (skipUpdate bool, err error) {
	bg := context.Background()
	clog := log.WithField("containerID", containerID)

	// Create the exec
	execConfig := container.ExecOptions{
		Tty:    true,
		Detach: false,
		Cmd:    []string{"sh", "-c", command},
	}

	exec, err := client.api.ContainerExecCreate(bg, string(containerID), execConfig)
	if err != nil {
		return false, err
	}

	response, attachErr := client.api.ContainerExecAttach(bg, exec.ID, container.ExecStartOptions{
		Tty:    true,
		Detach: false,
	})
	if attachErr != nil {
		clog.Errorf("Failed to extract command exec logs: %v", attachErr)
	}

	// Run the exec
	execStartCheck := container.ExecStartOptions{Detach: false, Tty: true}
	err = client.api.ContainerExecStart(bg, exec.ID, execStartCheck)
	if err != nil {
		return false, err
	}

	var output string
	if attachErr == nil {
		defer response.Close()
		var writer bytes.Buffer
		written, err := writer.ReadFrom(response.Reader)
		if err != nil {
			clog.Error(err)
		} else if written > 0 {
			output = strings.TrimSpace(writer.String())
		}
	}

	// Inspect the exec to get the exit code and print a message if the
	// exit code is not success.
	skip, err := client.waitForExecOrTimeout(bg, exec.ID, output, timeout)
	if err != nil {
		return true, err
	}

	return skip, nil
}

func (client dockerClient) waitForExecOrTimeout(bg context.Context, execID, execOutput string, timeout int) (skipUpdate bool, err error) {
	const exTempFail = 75
	var ctx context.Context
	var cancel context.CancelFunc

	if timeout > 0 {
		ctx, cancel = context.WithTimeout(bg, time.Duration(timeout)*time.Minute)
		defer cancel()
	} else {
		ctx = bg
	}

	for {
		execInspect, err := client.api.ContainerExecInspect(ctx, execID)

		//goland:noinspection GoNilness
		log.WithFields(log.Fields{
			"exit-code":    execInspect.ExitCode,
			"exec-id":      execInspect.ExecID,
			"running":      execInspect.Running,
			"container-id": execInspect.ContainerID,
		}).Debug("Awaiting timeout or completion")

		if err != nil {
			return false, err
		}
		if execInspect.Running {
			time.Sleep(1 * time.Second)
			continue
		}
		if len(execOutput) > 0 {
			log.Infof("Command output:\n%v", execOutput)
		}

		if execInspect.ExitCode == exTempFail {
			return true, nil
		}

		if execInspect.ExitCode > 0 {
			return false, fmt.Errorf("command exited with code %v  %s", execInspect.ExitCode, execOutput)
		}
		break
	}
	return false, nil
}

func (client dockerClient) waitForStopOrTimeout(ctx context.Context, c t.Container, waitTime time.Duration) error {
	timeout := time.After(waitTime)

	for {
		select {
		case <-timeout:
			return nil
		case <-ctx.Done():
			// Outer StopContainer ctx fired — usually means the daemon is
			// hung. Surface the cancellation so the scan loop knows the
			// stop didn't complete cleanly.
			return ctx.Err()
		default:
			if ci, err := client.api.ContainerInspect(ctx, string(c.ID())); err != nil {
				return err
			} else if !ci.State.Running {
				return nil
			}
		}
		time.Sleep(1 * time.Second)
	}
}
