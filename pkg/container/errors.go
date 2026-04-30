package container

import "errors"

var (
	errNoImageInfo     = errors.New("no available image info")
	errNoContainerInfo = errors.New("no available container info")
	errInvalidConfig   = errors.New("container configuration missing or invalid")
	errLabelNotFound   = errors.New("label was not found in container")

	// ErrContainerNotFound is returned by Client operations (StopContainer,
	// future StartContainer) when the daemon reports the container no longer
	// exists. Lets callers distinguish a benign mid-scan disappearance from
	// a real failure so the scan can skip the container and continue.
	ErrContainerNotFound = errors.New("container not found")

	// ErrPinnedImage is returned by PullImage when the container's image is
	// pinned to a content-addressable digest (sha256:...) — there's no tag
	// to follow, so there's nothing for watchtower to update. Lets the
	// scan-loop demote the resulting "skipped" log line to debug instead of
	// firing every poll with a warn that operators can't act on. Message
	// preserved verbatim from the pre-typed-error wording so existing
	// downstream parsers and notification templates keep matching.
	ErrPinnedImage = errors.New("container uses a pinned image, and cannot be updated by watchtower")

	// ErrPullImageUnauthorized is returned by PullImage when the registry
	// rejects the request with HTTP 401. Distinct from a transient daemon
	// error so operators can wire alerts on persistent auth failure (rotated
	// credentials, expired token) without drowning in DNS / timeout noise.
	// The original cerrdefs error stays in the chain via fmt.Errorf("%w: %w"),
	// so cerrdefs.IsUnauthorized still returns true on the wrapped value.
	ErrPullImageUnauthorized = errors.New("failed to pull image: authentication required")

	// ErrPullImageNotFound is returned by PullImage when the registry
	// reports the manifest is missing (HTTP 404 / cerrdefs.ErrNotFound).
	// Kept as a distinct sentinel so the scan-loop's local-build safeguard
	// in pullFailureLooksLocal can keep recognising the case via
	// cerrdefs.IsNotFound (which walks both single- and multi-unwrap chains).
	ErrPullImageNotFound = errors.New("failed to pull image: image not found in registry")
)
