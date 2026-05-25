package container

import (
	dockerContainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/go-connections/nat"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
)

type MockContainerUpdate func(*dockerContainer.InspectResponse, *image.InspectResponse)

func MockContainer(updates ...MockContainerUpdate) *Container {
	containerInfo := dockerContainer.InspectResponse{
		ContainerJSONBase: &dockerContainer.ContainerJSONBase{
			ID:         "container_id",
			Image:      "image",
			Name:       "test-containrrr",
			HostConfig: &dockerContainer.HostConfig{},
		},
		Config: &dockerContainer.Config{
			Labels: map[string]string{},
		},
	}
	imageInfo := image.InspectResponse{
		ID:     "image_id",
		Config: &dockerspec.DockerOCIImageConfig{},
	}

	for _, update := range updates {
		update(&containerInfo, &imageInfo)
	}
	return NewContainer(&containerInfo, &imageInfo)
}

func WithPortBindings(portBindingSources ...string) MockContainerUpdate {
	return func(c *dockerContainer.InspectResponse, _ *image.InspectResponse) {
		portBindings := nat.PortMap{}
		for _, pbs := range portBindingSources {
			portBindings[nat.Port(pbs)] = []nat.PortBinding{}
		}
		c.HostConfig.PortBindings = portBindings
	}
}

func WithImageName(name string) MockContainerUpdate {
	return func(c *dockerContainer.InspectResponse, i *image.InspectResponse) {
		c.Config.Image = name
		i.RepoTags = append(i.RepoTags, name)
	}
}

func WithLinks(links []string) MockContainerUpdate {
	return func(c *dockerContainer.InspectResponse, _ *image.InspectResponse) {
		c.HostConfig.Links = links
	}
}

func WithLabels(labels map[string]string) MockContainerUpdate {
	return func(c *dockerContainer.InspectResponse, _ *image.InspectResponse) {
		c.Config.Labels = labels
	}
}

func WithContainerState(state dockerContainer.State) MockContainerUpdate {
	return func(cnt *dockerContainer.InspectResponse, _ *image.InspectResponse) {
		cnt.State = &state
	}
}

func WithHealthcheck(healthConfig dockerContainer.HealthConfig) MockContainerUpdate {
	return func(cnt *dockerContainer.InspectResponse, _ *image.InspectResponse) {
		cnt.Config.Healthcheck = &healthConfig
	}
}

func WithImageHealthcheck(healthConfig dockerContainer.HealthConfig) MockContainerUpdate {
	return func(_ *dockerContainer.InspectResponse, img *image.InspectResponse) {
		img.Config.Healthcheck = &healthConfig
	}
}

// WithHostname sets containerInfo.Config.Hostname so tests can exercise the
// hostname-clear path on GetCreateConfig (used by the self-update branch in
// restartStaleContainer to break os.Hostname() drift across self-updates).
func WithHostname(hostname string) MockContainerUpdate {
	return func(c *dockerContainer.InspectResponse, _ *image.InspectResponse) {
		c.Config.Hostname = hostname
	}
}

// WithUser sets the container's effective User (as docker materializes an
// image's USER directive into Config.User) and the image's own USER, so tests
// can exercise the User-clear paths in GetCreateConfig — including the distroless
// base-image switch (USER app -> numeric nonroot) that motivated the fallback fix.
func WithUser(containerUser, imageUser string) MockContainerUpdate {
	return func(c *dockerContainer.InspectResponse, i *image.InspectResponse) {
		c.Config.User = containerUser
		i.Config.User = imageUser
	}
}
