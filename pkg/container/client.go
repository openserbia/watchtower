package container

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
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
func NewClient(opts ClientOptions) Client {
	cli, err := sdkClient.NewClientWithOpts(sdkClient.FromEnv, sdkClient.WithAPIVersionNegotiation())
	if err != nil {
		log.Fatalf("Error instantiating Docker client: %s", err)
	}

	return dockerClient{
		api:           cli,
		ClientOptions: opts,
	}
}

// ClientOptions contains the options for how the docker client wrapper should behave
type ClientOptions struct {
	RemoveVolumes     bool
	IncludeStopped    bool
	ReviveStopped     bool
	IncludeRestarting bool
	WarnOnHeadFailed  WarningStrategy
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
	bg := context.Background()

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
	containers, err := client.api.ContainerList(
		bg,
		container.ListOptions{
			Filters: filter,
		})
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
	bg := context.Background()

	containerInfo, err := client.api.ContainerInspect(bg, string(containerID))
	if err != nil {
		metrics.RegisterDockerAPIError("inspect")
		return &Container{}, err
	}

	netType, netContainerID, found := strings.Cut(string(containerInfo.HostConfig.NetworkMode), ":")
	if found && netType == "container" {
		parentContainer, err := client.api.ContainerInspect(bg, netContainerID)
		if err != nil {
			metrics.RegisterDockerAPIError("inspect")
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

	imageInfo, err := client.api.ImageInspect(bg, containerInfo.Image)
	if err != nil {
		metrics.RegisterDockerAPIError("image_inspect")
		// The image the container was created from may have been garbage-collected
		// off disk (e.g. a previous --cleanup run after the tag was moved to a
		// newer digest). Fall back to inspecting by the image reference the
		// container was created with — usually a name:tag that now points at the
		// freshly-pulled digest — so updates can still proceed.
		if ref := containerInfo.Config.Image; ref != "" && ref != containerInfo.Image && !strings.HasPrefix(ref, "sha256:") {
			if fallbackInfo, fallbackErr := client.api.ImageInspect(bg, ref); fallbackErr == nil {
				metrics.RegisterImageFallback()
				log.Warnf("Image %s for container %s is missing locally; falling back to %q for config", containerInfo.Image, containerInfo.Name, ref)
				return &Container{containerInfo: &containerInfo, imageInfo: &fallbackInfo}, nil
			}
			metrics.RegisterDockerAPIError("image_inspect")
		}
		log.Warnf("Failed to retrieve container image info: %v", err)
		return &Container{containerInfo: &containerInfo, imageInfo: nil}, nil
	}

	return &Container{containerInfo: &containerInfo, imageInfo: &imageInfo}, nil
}

func (client dockerClient) StopContainer(c t.Container, timeout time.Duration) error {
	bg := context.Background()
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

	if c.IsRunning() {
		log.Infof("Stopping %s (%s) with %s", c.Name(), shortID, signal)
		if err := client.api.ContainerKill(bg, idStr, signal); err != nil {
			if cerrdefs.IsNotFound(err) {
				log.Debugf("Container %s already gone before kill, nothing to stop.", shortID)
				return nil
			}
			metrics.RegisterDockerAPIError("kill")
			return err
		}
	}

	// TODO: This should probably be checked.
	_ = client.waitForStopOrTimeout(c, timeout)

	if c.ContainerInfo().HostConfig.AutoRemove {
		log.Debugf("AutoRemove container %s, skipping ContainerRemove call.", shortID)
	} else {
		log.Debugf("Removing container %s", shortID)

		if err := client.api.ContainerRemove(bg, idStr, container.RemoveOptions{Force: true, RemoveVolumes: client.RemoveVolumes}); err != nil {
			if cerrdefs.IsNotFound(err) {
				log.Debugf("Container %s not found, skipping removal.", shortID)
				return nil
			}
			metrics.RegisterDockerAPIError("remove")
			return err
		}
	}

	// Wait for container to be removed. In this case an error is a good thing
	if err := client.waitForStopOrTimeout(c, timeout); err == nil {
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
	bg := context.Background()
	config := c.GetCreateConfig()
	hostConfig := c.GetCreateHostConfig()
	networkConfig := client.GetNetworkConfig(c)

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

	log.Infof("Creating %s", name)

	createdContainer, err := client.api.ContainerCreate(bg, config, hostConfig, simpleNetworkConfig, nil, name)
	if err != nil {
		metrics.RegisterDockerAPIError("create")
		return "", err
	}

	if !(hostConfig.NetworkMode.IsHost()) {
		for k := range simpleNetworkConfig.EndpointsConfig {
			err = client.api.NetworkDisconnect(bg, k, createdContainer.ID, true)
			if err != nil {
				metrics.RegisterDockerAPIError("network_disconnect")
				return "", err
			}
		}

		for k, v := range networkConfig.EndpointsConfig {
			err = client.api.NetworkConnect(bg, k, createdContainer.ID, v)
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

	return createdContainerID, client.doStartContainer(bg, c, createdContainer)
}

func (client dockerClient) doStartContainer(bg context.Context, c t.Container, creation container.CreateResponse) error {
	name := c.Name()

	log.Debugf("Starting container %s (%s)", name, t.ContainerID(creation.ID).ShortID())
	err := client.api.ContainerStart(bg, creation.ID, container.StartOptions{})
	if err != nil {
		metrics.RegisterDockerAPIError("start")
		return err
	}
	return nil
}

func (client dockerClient) RenameContainer(c t.Container, newName string) error {
	bg := context.Background()
	log.Debugf("Renaming container %s (%s) to %s", c.Name(), c.ID().ShortID(), newName)
	if err := client.api.ContainerRename(bg, string(c.ID()), newName); err != nil {
		metrics.RegisterDockerAPIError("rename")
		return err
	}
	return nil
}

func (client dockerClient) IsContainerStale(container t.Container, params t.UpdateParams) (stale bool, latestImage t.ImageID, err error) {
	ctx := context.Background()

	switch {
	case container.IsNoPull(params):
		log.Debugf("Skipping image pull.")
	case container.ImageIsLocal():
		// Image has no RepoDigests — locally built via `docker build` or
		// loaded via `docker load`, never came from a registry. Pulling is
		// guaranteed to fail ("No such image") and only produces log
		// noise. HasNewImage still works: rebuilds retag the image, the
		// ID behind the tag changes, and the next poll picks up the
		// difference. Out-of-the-box replacement for the older workaround
		// of setting --no-pull or the per-container no-pull label.
		log.Debugf("Skipping image pull for %s: no registry digest (locally built or loaded).", container.Name())
	default:
		if err := client.PullImage(ctx, container); err != nil {
			return false, container.SafeImageID(), err
		}
	}

	return client.HasNewImage(ctx, container)
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
		return fmt.Errorf("container uses a pinned image, and cannot be updated by watchtower")
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
		metrics.RegisterDockerAPIError("image_pull")
		log.Debugf("Error pulling image %s, %s", imageName, err)
		return err
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

	items, err := client.api.ImageRemove(
		context.Background(),
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
	if err != nil {
		metrics.RegisterDockerAPIError("image_remove")
	}

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

func (client dockerClient) waitForStopOrTimeout(c t.Container, waitTime time.Duration) error {
	bg := context.Background()
	timeout := time.After(waitTime)

	for {
		select {
		case <-timeout:
			return nil
		default:
			if ci, err := client.api.ContainerInspect(bg, string(c.ID())); err != nil {
				return err
			} else if !ci.State.Running {
				return nil
			}
		}
		time.Sleep(1 * time.Second)
	}
}
