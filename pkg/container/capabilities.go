package container

import (
	"context"
	"errors"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	sdkClient "github.com/moby/moby/client"
)

// CapabilityID names a single Docker API operation Watchtower depends on. It
// doubles as the stable key callers (preflight orchestration, docs) use to
// request and report probe results.
type CapabilityID string

// The capability catalog. Each ID maps 1:1 to a Docker engine API endpoint
// Watchtower calls during a scan/update cycle. Grouped by lifecycle phase so
// the docs page reads in execution order.
const (
	// Always-on reads: every poll lists and inspects containers and their
	// images before deciding anything.
	CapPing             CapabilityID = "ping"
	CapContainerList    CapabilityID = "container_list"
	CapContainerInspect CapabilityID = "container_inspect"
	CapImageInspect     CapabilityID = "image_inspect"

	// Staleness detection pulls the candidate image (skipped under --no-pull).
	CapImagePull CapabilityID = "image_pull"

	// The recreate write set: stop (kill + remove), recreate (create + start),
	// re-tag to the resolved digest, re-attach networks, and rename for the
	// self-update dance. Skipped wholesale under --monitor-only.
	CapContainerKill       CapabilityID = "container_kill"
	CapContainerRemove     CapabilityID = "container_remove"
	CapContainerCreate     CapabilityID = "container_create"
	CapContainerStart      CapabilityID = "container_start"
	CapImageTag            CapabilityID = "image_tag"
	CapNetworkConnect      CapabilityID = "network_connect"
	CapNetworkDisconnect   CapabilityID = "network_disconnect"
	CapContainerRename     CapabilityID = "container_rename"
	CapContainerExecCreate CapabilityID = "container_exec_create"

	// Conditional writes.
	CapImageRemove   CapabilityID = "image_remove"
	CapContainerWait CapabilityID = "container_wait"

	// Optional: the event stream is a nice-to-have accelerator, never required.
	CapEvents CapabilityID = "events"
)

// CapabilityKind classifies a capability as a read-only or mutating operation,
// so the docs and preflight summary can group them and operators can reason
// about which socket-proxy permission bit each one needs.
type CapabilityKind string

const (
	// KindRead is a read-only Docker API operation (GET / inspect / list).
	KindRead CapabilityKind = "read"
	// KindWrite is a mutating Docker API operation (create / start / remove / …).
	KindWrite CapabilityKind = "write"
)

// Capability describes one Docker API operation Watchtower depends on: the
// engine endpoint it hits, the socket-proxy environment variable that gates
// it (for the common tecnativa/docker-socket-proxy and compatible proxies),
// whether it reads or writes, and a doc-quality reason explaining why
// Watchtower needs it. The Reason fields drive the published capability table,
// so keep them operator-facing and specific.
type Capability struct {
	ID       CapabilityID
	Endpoint string
	ProxyVar string
	Kind     CapabilityKind
	Reason   string
}

// capabilityCatalog is the single source of truth describing every Docker API
// operation Watchtower can issue. Ordered by lifecycle phase to match the docs
// page and the preflight log output.
var capabilityCatalog = []Capability{
	{
		ID:       CapPing,
		Endpoint: "GET /_ping",
		ProxyVar: "PING",
		Kind:     KindRead,
		Reason:   "Confirms the daemon socket is reachable and negotiates the API version on startup.",
	},
	{
		ID:       CapContainerList,
		Endpoint: "GET /containers/json",
		ProxyVar: "CONTAINERS",
		Kind:     KindRead,
		Reason:   "Lists running containers each poll to decide which ones Watchtower manages.",
	},
	{
		ID:       CapContainerInspect,
		Endpoint: "GET /containers/{id}/json",
		ProxyVar: "CONTAINERS",
		Kind:     KindRead,
		Reason:   "Inspects each container to read its image, config, labels, and network attachments.",
	},
	{
		ID:       CapImageInspect,
		Endpoint: "GET /images/{name}/json",
		ProxyVar: "IMAGES",
		Kind:     KindRead,
		Reason:   "Resolves the local image ID behind a container so staleness can be compared against the registry digest.",
	},
	{
		ID:       CapImagePull,
		Endpoint: "POST /images/create",
		ProxyVar: proxyVarImagesPost,
		Kind:     KindWrite,
		Reason:   "Pulls the candidate image during staleness detection. Not needed with --no-pull (or WATCHTOWER_NO_PULL).",
	},
	{
		ID:       CapContainerKill,
		Endpoint: "POST /containers/{id}/kill",
		ProxyVar: proxyVarContainersPost,
		Kind:     KindWrite,
		Reason:   "Sends the stop signal to a stale container before it is removed and recreated.",
	},
	{
		ID:       CapContainerRemove,
		Endpoint: "DELETE /containers/{id}",
		ProxyVar: proxyVarContainersPost,
		Kind:     KindWrite,
		Reason:   "Removes the old container after it has stopped so the replacement can take its name.",
	},
	{
		ID:       CapContainerCreate,
		Endpoint: "POST /containers/create",
		ProxyVar: proxyVarContainersPost,
		Kind:     KindWrite,
		Reason:   "Recreates the container from the new image, carrying its previous config forward.",
	},
	{
		ID:       CapContainerStart,
		Endpoint: "POST /containers/{id}/start",
		ProxyVar: proxyVarContainersPost,
		Kind:     KindWrite,
		Reason:   "Starts the freshly recreated container.",
	},
	{
		ID:       CapImageTag,
		Endpoint: "POST /images/{name}/tag",
		ProxyVar: proxyVarImagesPost,
		Kind:     KindWrite,
		Reason:   "Re-binds the original tag to the resolved digest just before recreate, so a CI retag between scan and create cannot strand the container on a missing image.",
	},
	{
		ID:       CapNetworkConnect,
		Endpoint: "POST /networks/{id}/connect",
		ProxyVar: "NETWORKS+POST",
		Kind:     KindWrite,
		Reason:   "Re-attaches the recreated container to each of its original networks with the original aliases.",
	},
	{
		ID:       CapNetworkDisconnect,
		Endpoint: "POST /networks/{id}/disconnect",
		ProxyVar: "NETWORKS+POST",
		Kind:     KindWrite,
		Reason:   "Detaches the single network ContainerCreate auto-attached so the full original network set can be restored cleanly.",
	},
	{
		ID:       CapContainerRename,
		Endpoint: "POST /containers/{id}/rename",
		ProxyVar: proxyVarContainersPost,
		Kind:     KindWrite,
		Reason:   "Renames Watchtower's own container during a self-update so the replacement can claim the canonical name.",
	},
	{
		ID:       CapContainerExecCreate,
		Endpoint: "POST /containers/{id}/exec",
		ProxyVar: "EXEC+POST",
		Kind:     KindWrite,
		Reason:   "Runs user-defined lifecycle hook commands inside containers. Only needed when a watched container declares a com.centurylinklabs.watchtower.lifecycle.* label and --enable-lifecycle-hooks is set.",
	},
	{
		ID:       CapImageRemove,
		Endpoint: "DELETE /images/{name}",
		ProxyVar: proxyVarImagesPost,
		Kind:     KindWrite,
		Reason:   "Deletes the superseded image after a successful update. Only needed with --cleanup (or WATCHTOWER_CLEANUP).",
	},
	{
		ID:       CapContainerWait,
		Endpoint: "POST /containers/{id}/wait",
		ProxyVar: proxyVarContainersPost,
		Kind:     KindWrite,
		Reason:   "Blocks until a re-run Compose init container exits. Only needed with --rerun-init-deps (or WATCHTOWER_RERUN_INIT_DEPS).",
	},
	{
		ID:       CapEvents,
		Endpoint: "GET /events",
		ProxyVar: "EVENTS",
		Kind:     KindRead,
		Reason:   "Subscribes to the engine event stream to trigger targeted scans on local image rebuilds. Optional accelerator for --watch-docker-events; Watchtower degrades to scheduled polling without it.",
	},
}

// capabilityByID indexes capabilityCatalog for O(1) descriptor lookup.
var capabilityByID = func() map[CapabilityID]Capability {
	m := make(map[CapabilityID]Capability, len(capabilityCatalog))
	for _, c := range capabilityCatalog {
		m[c.ID] = c
	}
	return m
}()

// LookupCapability returns the descriptor for an ID and whether it is known.
func LookupCapability(id CapabilityID) (Capability, bool) {
	c, ok := capabilityByID[id]
	return c, ok
}

// AllCapabilities returns the full catalog in lifecycle order. The returned
// slice is a copy, safe for callers to iterate or sort.
func AllCapabilities() []Capability {
	out := make([]Capability, len(capabilityCatalog))
	copy(out, capabilityCatalog)
	return out
}

// ProbeStatus is the verdict for a single capability probe.
type ProbeStatus string

const (
	// StatusPresent means the daemon answered the probe with a logical result
	// (success, not-found, bad-request, or conflict) — the operation is
	// permitted and reachable.
	StatusPresent ProbeStatus = "present"
	// StatusBlocked means an interposing socket proxy or the daemon itself
	// rejected the operation with a permission error (HTTP 403) — the endpoint
	// is filtered out.
	StatusBlocked ProbeStatus = "blocked"
	// StatusUnreachable means the probe could not reach a daemon able to
	// answer at all: connection refused, TLS failure, or socket-level
	// unauthorized. Distinct from Blocked because the fix is connectivity /
	// credentials, not a proxy allow-list entry.
	StatusUnreachable ProbeStatus = "unreachable"
)

// ProbeResult pairs a capability ID with the verdict of probing it and the
// underlying error (nil for Present).
type ProbeResult struct {
	ID     CapabilityID
	Status ProbeStatus
	Err    error
}

// probeTimeout bounds each individual capability probe. Probes are cheap
// logical round-trips (bogus targets that the daemon rejects immediately), so
// a tight bound keeps a hung socket from stalling startup without risking a
// false Unreachable on a healthy-but-busy daemon.
const probeTimeout = 15 * time.Second

// eventsProbeWindow bounds the events-stream probe specifically. A reachable
// daemon with no activity legitimately sends nothing on /events, so we keep
// this short: surviving the window without an error means the stream opened
// and is permitted.
const eventsProbeWindow = 2 * time.Second

// ProbeCapabilities implements the Client interface. See Client.ProbeCapabilities.
func (client dockerClient) ProbeCapabilities(ctx context.Context, ids []CapabilityID) []ProbeResult {
	results := make([]ProbeResult, 0, len(ids))
	for _, id := range ids {
		err := client.probe(ctx, id)
		results = append(results, ProbeResult{
			ID:     id,
			Status: classifyProbe(err),
			Err:    err,
		})
	}
	return results
}

// classifyProbe maps a raw probe error to a ProbeStatus. A nil error or any
// daemon-logical rejection (not-found, bad-request, conflict) means the
// endpoint is reachable and permitted. A permission denial (HTTP 403) means a
// proxy or the daemon filtered it. Everything else — connection refused, TLS,
// socket-level unauthorized — means we could not reach a daemon that could
// answer.
func classifyProbe(err error) ProbeStatus {
	switch {
	case err == nil:
		return StatusPresent
	case cerrdefs.IsNotFound(err),
		cerrdefs.IsInvalidArgument(err),
		cerrdefs.IsConflict(err):
		// The daemon understood the call and rejected the bogus target on its
		// merits — the endpoint exists and we are allowed to call it.
		return StatusPresent
	case cerrdefs.IsPermissionDenied(err):
		// HTTP 403 from a socket proxy (or the daemon's own authz plugin):
		// the endpoint is filtered out of the allow-list.
		return StatusBlocked
	case cerrdefs.IsUnauthorized(err),
		sdkClient.IsErrConnectionFailed(err):
		// Socket-level 401, connection refused, or TLS handshake failure: no
		// daemon we can talk to is answering.
		return StatusUnreachable
	default:
		// Unclassified errors are treated as Unreachable so a required
		// capability fails closed rather than being silently assumed present.
		return StatusUnreachable
	}
}

// probe issues a single side-effect-free request that exercises a capability's
// endpoint against a deliberately bogus target. Reads inspect a non-existent
// object; writes act on a non-existent id or an invalid spec the daemon
// rejects before it touches any real resource. Either way the call returns a
// logical error (not-found / bad-request / conflict) from a permitting daemon,
// a 403 from a filtering proxy, or a transport error when nothing answers —
// exactly the three signals classifyProbe distinguishes. Nothing is created,
// removed, or mutated.
//
// classify the daemon's response. A monolithic switch keeps every probe's
// bogus target visible in one place rather than scattered across helpers.
//
//nolint:cyclop // one branch per capability; the switch is the catalog made
func (client dockerClient) probe(ctx context.Context, id CapabilityID) error {
	const bogus = "watchtower-preflight-nonexistent"

	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	switch id {
	case CapPing:
		_, err := client.api.Ping(ctx, sdkClient.PingOptions{})
		return err
	case CapContainerList:
		_, err := client.api.ContainerList(ctx, sdkClient.ContainerListOptions{Limit: 1})
		return err
	case CapContainerInspect:
		_, err := client.api.ContainerInspect(ctx, bogus, sdkClient.ContainerInspectOptions{})
		return err
	case CapImageInspect:
		_, err := client.api.ImageInspect(ctx, bogus)
		return err
	case CapImagePull:
		// Probe POST /images/create. The goal is to confirm the daemon ACCEPTS
		// the request (the proxy permits IMAGES+POST), not that any image exists,
		// so the bogus repository never resolves to a real pull. A registry-level
		// 401 or 404 means the request traversed the proxy and the daemon actually
		// attempted the pull — capability confirmed. Only a proxy 403 (permission
		// denied) or a transport failure is a real gap.
		//
		// Docker Hub answers a nonexistent docker.io/library/<bare-name> repo with
		// 401, NOT 404 (see pullErrorLooksUnauthorized in client.go). Treating that
		// 401 as Unreachable would log.Fatal a perfectly healthy daemon at startup
		// — the exact bug this special-case guards against. The cost is one cheap
		// outbound auth round-trip when --preflight is set; probeTimeout bounds it.
		//
		// The auth failure can arrive either as the immediate ImagePull error or
		// as an in-stream JSON error, so drain with Wait to surface both; Wait
		// maps the in-stream error back through its HTTP status, preserving the
		// cerrdefs classification the checks below rely on.
		resp, err := client.api.ImagePull(ctx, bogus, sdkClient.ImagePullOptions{})
		if resp != nil {
			if werr := resp.Wait(ctx); err == nil {
				err = werr
			}
			_ = resp.Close()
		}
		if cerrdefs.IsUnauthorized(err) || cerrdefs.IsNotFound(err) {
			return nil
		}
		return err
	case CapContainerKill:
		_, err := client.api.ContainerKill(ctx, bogus, sdkClient.ContainerKillOptions{Signal: "SIGTERM"})
		return err
	case CapContainerRemove:
		_, err := client.api.ContainerRemove(ctx, bogus, sdkClient.ContainerRemoveOptions{})
		return err
	case CapContainerCreate:
		// Reference the bogus (nonexistent) image so a permitting daemon rejects
		// the create with 404 "no such image" before anything is created; a
		// filtering proxy still answers 403. The client tolerates a nil Config,
		// but we pass the image explicitly so the daemon has a concrete target to
		// reject.
		_, err := client.api.ContainerCreate(ctx, sdkClient.ContainerCreateOptions{Config: &container.Config{Image: bogus}, Name: bogus})
		return err
	case CapContainerStart:
		_, err := client.api.ContainerStart(ctx, bogus, sdkClient.ContainerStartOptions{})
		return err
	case CapImageTag:
		_, err := client.api.ImageTag(ctx, sdkClient.ImageTagOptions{Source: bogus, Target: bogus + ":preflight"})
		return err
	case CapNetworkConnect:
		_, err := client.api.NetworkConnect(ctx, bogus, sdkClient.NetworkConnectOptions{Container: bogus, EndpointConfig: &network.EndpointSettings{}})
		return err
	case CapNetworkDisconnect:
		_, err := client.api.NetworkDisconnect(ctx, bogus, sdkClient.NetworkDisconnectOptions{Container: bogus})
		return err
	case CapContainerRename:
		_, err := client.api.ContainerRename(ctx, bogus, sdkClient.ContainerRenameOptions{NewName: bogus + "-renamed"})
		return err
	case CapContainerExecCreate:
		_, err := client.api.ExecCreate(ctx, bogus, sdkClient.ExecCreateOptions{Cmd: []string{valueTrue}})
		return err
	case CapImageRemove:
		_, err := client.api.ImageRemove(ctx, bogus, sdkClient.ImageRemoveOptions{})
		return err
	case CapContainerWait:
		return waitProbe(ctx, client.api, bogus)
	case CapEvents:
		return eventsProbe(ctx, client.api)
	default:
		return errUnknownCapability(id)
	}
}

// waitProbe exercises POST /containers/{id}/wait against a bogus id. The SDK's
// ContainerWait returns its result and error over channels, so we read whichever
// fires first within the probe context.
func waitProbe(ctx context.Context, api sdkClient.APIClient, id string) error {
	result := api.ContainerWait(ctx, id, sdkClient.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case err := <-result.Error:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// eventsProbe exercises GET /events. The SDK streams over channels; we open the
// stream, take whichever of "first error" or "first message" arrives, then let
// the deferred cancel tear the connection down. A 403 surfaces on the error
// channel; a permitting daemon either sends a message or simply blocks (no
// events), which we treat as Present once the stream is established.
func eventsProbe(ctx context.Context, api sdkClient.APIClient) error {
	// A short window: a reachable daemon with no activity legitimately sends
	// nothing, so absence of an error within the window means the stream is
	// open and permitted.
	streamCtx, cancel := context.WithTimeout(ctx, eventsProbeWindow)
	defer cancel()

	result := api.Events(streamCtx, sdkClient.EventsListOptions{})
	select {
	case err := <-result.Err:
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			// The stream stayed open for the whole window without an error —
			// it is reachable and permitted.
			return nil
		}
		return err
	case <-result.Messages:
		return nil
	case <-streamCtx.Done():
		return nil
	}
}

// errUnknownCapability is returned when a caller probes an ID absent from the
// catalog. It is classified as Unreachable (fail-closed) so an out-of-sync
// caller never silently assumes an unknown capability is present.
func errUnknownCapability(id CapabilityID) error {
	return &unknownCapabilityError{id: id}
}

type unknownCapabilityError struct{ id CapabilityID }

func (e *unknownCapabilityError) Error() string {
	return "unknown capability: " + string(e.id)
}
