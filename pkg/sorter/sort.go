// Package sorter topologically orders containers by their declared
// depends-on and links graph so updates happen in a safe order.
package sorter

import (
	"fmt"
	"time"

	"github.com/openserbia/watchtower/pkg/types"
)

// ByCreated allows a list of Container structs to be sorted by the container's
// created date.
type ByCreated []types.Container

func (c ByCreated) Len() int      { return len(c) }
func (c ByCreated) Swap(i, j int) { c[i], c[j] = c[j], c[i] }

// Less will compare two elements (identified by index) in the Container
// list by created-date.
func (c ByCreated) Less(i, j int) bool {
	t1, err := time.Parse(time.RFC3339Nano, c[i].ContainerInfo().Created)
	if err != nil {
		t1 = time.Now()
	}

	t2, _ := time.Parse(time.RFC3339Nano, c[j].ContainerInfo().Created)
	if err != nil {
		t1 = time.Now()
	}

	return t1.Before(t2)
}

// SortByDependencies will sort the list of containers taking into account any
// links between containers. Container with no outgoing links will be sorted to
// the front of the list while containers with links will be sorted after all
// of their dependencies. This sort order ensures that linked containers can
// be started in the correct order.
//
// When includeComposeDepends is true, Watchtower also augments the graph with
// edges derived from Docker Compose's com.docker.compose.depends_on labels —
// service names resolved to container names within the same Compose project.
// Opt-in because it changes the order (and therefore stop/start behaviour)
// for Compose-managed stacks that currently rely on watchtower only seeing
// the explicit com.centurylinklabs.watchtower.depends-on label.
func SortByDependencies(containers []types.Container, includeComposeDepends bool) ([]types.Container, error) {
	ds := dependencySorter{}
	if includeComposeDepends {
		ds.composeIndex = buildComposeIndex(containers)
	}
	return ds.Sort(containers)
}

type dependencySorter struct {
	unvisited []types.Container
	marked    map[string]bool
	sorted    []types.Container
	// composeIndex maps Compose project → service name → container name.
	// Nil when compose-depends resolution is disabled.
	composeIndex map[string]map[string]string
}

// buildComposeIndex walks the container set once and builds the
// project→service→container lookup used to resolve depends_on service names
// to real container names. Containers without the Compose labels are
// skipped — they can't be targets of a Compose-level dependency anyway.
func buildComposeIndex(containers []types.Container) map[string]map[string]string {
	index := make(map[string]map[string]string)
	for _, c := range containers {
		project := c.ComposeProject()
		service := c.ComposeService()
		if project == "" || service == "" {
			continue
		}
		if _, ok := index[project]; !ok {
			index[project] = make(map[string]string)
		}
		index[project][service] = c.Name()
	}
	return index
}

// dependencyNames returns the full set of outgoing dependency names for a
// container — the existing Links() graph plus any Compose depends_on edges
// when the index is available. Resolves service names to container names;
// silently drops deps whose target isn't in the current scan set (may have
// been filtered out by --label-enable / --scope / name args).
func (ds *dependencySorter) dependencyNames(c types.Container) []string {
	names := c.Links()
	if ds.composeIndex == nil {
		return names
	}
	project := c.ComposeProject()
	if project == "" {
		return names
	}
	services, ok := ds.composeIndex[project]
	if !ok {
		return names
	}
	for _, service := range c.ComposeDependencies() {
		if target, resolved := services[service]; resolved && target != c.Name() {
			names = append(names, target)
		}
	}
	return names
}

func (ds *dependencySorter) Sort(containers []types.Container) ([]types.Container, error) {
	ds.unvisited = containers
	ds.marked = map[string]bool{}

	for len(ds.unvisited) > 0 {
		if err := ds.visit(ds.unvisited[0]); err != nil {
			return nil, err
		}
	}

	return ds.sorted, nil
}

func (ds *dependencySorter) visit(c types.Container) error {
	if _, ok := ds.marked[c.Name()]; ok {
		return fmt.Errorf("circular reference to %s", c.Name())
	}

	// Mark any visited node so that circular references can be detected
	ds.marked[c.Name()] = true
	defer delete(ds.marked, c.Name())

	// Recursively visit links (including Compose depends_on edges when enabled)
	for _, linkName := range ds.dependencyNames(c) {
		if linkedContainer := ds.findUnvisited(linkName); linkedContainer != nil {
			if err := ds.visit(*linkedContainer); err != nil {
				return err
			}
		}
	}

	// Move container from unvisited to sorted
	ds.removeUnvisited(c)
	ds.sorted = append(ds.sorted, c)

	return nil
}

func (ds *dependencySorter) findUnvisited(name string) *types.Container {
	for _, c := range ds.unvisited {
		if c.Name() == name {
			return &c
		}
	}

	return nil
}

func (ds *dependencySorter) removeUnvisited(c types.Container) {
	var idx int
	for i := range ds.unvisited {
		if ds.unvisited[i].Name() == c.Name() {
			idx = i
			break
		}
	}

	ds.unvisited = append(ds.unvisited[0:idx], ds.unvisited[idx+1:]...)
}
