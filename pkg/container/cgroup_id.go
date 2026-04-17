package container

import (
	"fmt"
	"os"
	"regexp"

	"github.com/openserbia/watchtower/pkg/types"
)

var dockerContainerPattern = regexp.MustCompile(`[0-9]+:.*:/docker/([a-f|0-9]{64})`)

// GetRunningContainerID tries to resolve the current container ID from the current process cgroup information
func GetRunningContainerID() (cid types.ContainerID, err error) {
	file, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", os.Getpid()))
	if err != nil {
		return cid, err
	}

	return getRunningContainerIDFromString(string(file)), nil
}

func getRunningContainerIDFromString(s string) types.ContainerID {
	const expectedMatchCount = 2
	matches := dockerContainerPattern.FindStringSubmatch(s)
	if len(matches) < expectedMatchCount {
		return ""
	}
	return types.ContainerID(matches[1])
}
