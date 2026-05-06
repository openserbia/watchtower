package actions

import (
	"os"
	"strings"

	log "github.com/sirupsen/logrus"

	"github.com/openserbia/watchtower/pkg/container"
	"github.com/openserbia/watchtower/pkg/filters"
	"github.com/openserbia/watchtower/pkg/types"
)

// DetectSelfContainerID returns the container ID of the watchtower process
// itself, or "" if it cannot be determined. The lookup matches `os.Hostname()`
// (which Docker sets to the container's short ID by default) against the IDs
// of all watchtower-labeled containers visible to the client.
//
// Used to distinguish the actual running self from any other watchtower-
// labeled containers seen during a scan (typically orphans left over from a
// previous self-update whose post-restart cleanup did not complete). Without
// this distinction the rename-and-respawn pattern in restartStaleContainer
// fires for every label-matched container, which on each scan multiplies
// random-named replacements until the periodic CheckForMultipleWatchtowerInstances
// happens to keep the wrong survivor.
//
// Returns "" if the hostname cannot be read or matches no live container —
// e.g. watchtower is running outside a container, or the operator overrode
// --hostname so it no longer matches the container's short ID. Callers fall
// back to the legacy IsWatchtower label match in that case.
func DetectSelfContainerID(client container.Client) types.ContainerID {
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		log.Debug("Self-detection: os.Hostname() returned empty or errored; rename-respawn will fall back to IsWatchtower label match")
		return ""
	}

	candidates, err := client.ListContainers(filters.WatchtowerContainersFilter)
	if err != nil {
		log.WithError(err).Debug("Self-detection: ListContainers failed; rename-respawn will fall back to IsWatchtower label match")
		return ""
	}

	for _, c := range candidates {
		if strings.HasPrefix(string(c.ID()), hostname) {
			log.WithFields(log.Fields{
				"container": c.Name(),
				"id":        c.ID().ShortID(),
			}).Debug("Self-detection: identified own container")
			return c.ID()
		}
	}

	log.WithField("hostname", hostname).Debug("Self-detection: no watchtower-labeled container matches hostname; rename-respawn will fall back to IsWatchtower label match")
	return ""
}
