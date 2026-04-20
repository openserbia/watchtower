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
)
