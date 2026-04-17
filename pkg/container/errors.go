package container

import "errors"

var (
	errNoImageInfo     = errors.New("no available image info")
	errNoContainerInfo = errors.New("no available container info")
	errInvalidConfig   = errors.New("container configuration missing or invalid")
	errLabelNotFound   = errors.New("label was not found in container")
)
