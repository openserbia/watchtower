// Package lifecycle runs user-defined pre/post check and update commands
// (configured via com.centurylinklabs.watchtower.lifecycle.* labels) inside
// target containers.
package lifecycle

import (
	"context"

	log "github.com/sirupsen/logrus"

	"github.com/openserbia/watchtower/pkg/container"
	"github.com/openserbia/watchtower/pkg/types"
)

// ExecutePreChecks tries to run the pre-check lifecycle hook for all containers included by the current filter.
func ExecutePreChecks(ctx context.Context, client container.Client, params types.UpdateParams) {
	containers, err := client.ListContainers(ctx, params.Filter)
	if err != nil {
		return
	}
	for _, currentContainer := range containers {
		ExecutePreCheckCommand(ctx, client, currentContainer)
	}
}

// ExecutePostChecks tries to run the post-check lifecycle hook for all containers included by the current filter.
func ExecutePostChecks(ctx context.Context, client container.Client, params types.UpdateParams) {
	containers, err := client.ListContainers(ctx, params.Filter)
	if err != nil {
		return
	}
	for _, currentContainer := range containers {
		ExecutePostCheckCommand(ctx, client, currentContainer)
	}
}

// ExecutePreCheckCommand tries to run the pre-check lifecycle hook for a single container.
func ExecutePreCheckCommand(ctx context.Context, client container.Client, container types.Container) {
	clog := log.WithField("container", container.Name())
	command := container.GetLifecyclePreCheckCommand()
	if command == "" {
		clog.Debug("No pre-check command supplied. Skipping")
		return
	}

	clog.Debug("Executing pre-check command.")
	_, err := client.ExecuteCommand(ctx, container.ID(), command, 1)
	if err != nil {
		clog.Error(err)
	}
}

// ExecutePostCheckCommand tries to run the post-check lifecycle hook for a single container.
func ExecutePostCheckCommand(ctx context.Context, client container.Client, container types.Container) {
	clog := log.WithField("container", container.Name())
	command := container.GetLifecyclePostCheckCommand()
	if command == "" {
		clog.Debug("No post-check command supplied. Skipping")
		return
	}

	clog.Debug("Executing post-check command.")
	_, err := client.ExecuteCommand(ctx, container.ID(), command, 1)
	if err != nil {
		clog.Error(err)
	}
}

// ExecutePreUpdateCommand tries to run the pre-update lifecycle hook for a single container.
func ExecutePreUpdateCommand(ctx context.Context, client container.Client, container types.Container) (skipUpdate bool, err error) {
	timeout := container.PreUpdateTimeout()
	command := container.GetLifecyclePreUpdateCommand()
	clog := log.WithField("container", container.Name())

	if command == "" {
		clog.Debug("No pre-update command supplied. Skipping")
		return false, nil
	}

	if !container.IsRunning() || container.IsRestarting() {
		clog.Debug("Container is not running. Skipping pre-update command.")
		return false, nil
	}

	clog.Debug("Executing pre-update command.")
	return client.ExecuteCommand(ctx, container.ID(), command, timeout)
}

// ExecutePostUpdateCommand tries to run the post-update lifecycle hook for a single container.
func ExecutePostUpdateCommand(ctx context.Context, client container.Client, newContainerID types.ContainerID) {
	newContainer, err := client.GetContainer(ctx, newContainerID)
	timeout := newContainer.PostUpdateTimeout()

	if err != nil {
		log.WithField("containerID", newContainerID.ShortID()).Error(err)
		return
	}
	clog := log.WithField("container", newContainer.Name())

	command := newContainer.GetLifecyclePostUpdateCommand()
	if command == "" {
		clog.Debug("No post-update command supplied. Skipping")
		return
	}

	clog.Debug("Executing post-update command.")
	_, err = client.ExecuteCommand(ctx, newContainerID, command, timeout)
	if err != nil {
		clog.Error(err)
	}
}
