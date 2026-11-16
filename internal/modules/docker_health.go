package modules

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"mmx/internal/protocol"
)

// The Docker forms of wait_for: docker_container_is_healthy and
// docker_compose_project_is_ready. Both read the health that Docker
// records from each container's own health check, and change nothing.

// waitsForDockerHealth reports whether the wait is one of the Docker forms.
func (w WaitFor) waitsForDockerHealth() bool {
	return w.DockerContainerIsHealthy != "" || w.DockerComposeProjectIsReady != ""
}

// dockerHealthObservation reads the health once. A result with a failure
// kind says that the wait cannot succeed, so it ends at once: the docker
// command is missing, or the container declares no health check.
func dockerHealthObservation(ctx context.Context, w WaitFor) protocol.Result {
	r := protocol.Result{Status: "failed", Change: "unchanged"}
	d := Runtime{}.findDocker(ctx, "wait_for")
	if d == nil {
		r.Reason, r.FailureKind = "wait_for: "+missingProgramError("docker").Error()+"; docker-ce comes from Docker's own package repository", "missing_prerequisite"
		return r
	}
	if name := w.DockerContainerIsHealthy; name != "" {
		container, found, err := d.container(ctx, name)
		switch {
		case err != nil:
			r.Reason = err.Error()
		case !found:
			r.Reason = "docker has no container named " + name
		case !container.declaresHealthCheck():
			r.Reason = fmt.Sprintf("the container %s declares no health check, so Docker never reports it healthy; declare one in its image, its docker run command, or its Compose file, or wait for a port or a url instead", name)
			r.FailureKind = "missing_prerequisite"
		case container.State.Health != nil && container.State.Health.Status == "healthy":
			r.Status, r.Reason = "succeeded", "the container "+name+" is healthy"
		default:
			r.Reason = "the container " + dockerHealthStatus(container)
		}
		return r
	}
	// The path as docker_compose_project cleans it and Compose labels the
	// containers with it: /opt/shop/ is /opt/shop.
	directory := filepath.Clean(w.DockerComposeProjectIsReady)
	all, err := d.containersWithLabels(ctx, map[string]string{composeLabelProjectDirectory: directory})
	if err != nil {
		r.Reason = err.Error()
		return r
	}
	var containers []dockerContainer
	for _, list := range serviceContainers(all, false) {
		containers = append(containers, list...)
	}
	if len(containers) == 0 {
		r.Reason = "the Compose project in " + directory + " has no containers"
		return r
	}
	var notReady []string
	for _, container := range all {
		switch {
		case strings.EqualFold(container.Config.Labels[composeLabelOneOff], "true"):
		case container.State.Health != nil && container.State.Health.Status != "healthy":
			notReady = append(notReady, dockerHealthStatus(container))
		case container.State.Health == nil && container.declaresHealthCheck() && !container.completed():
			notReady = append(notReady, dockerHealthStatus(container))
		case container.State.Health == nil && container.completed():
		case container.State.Health == nil && !container.declaresHealthCheck() && container.State.Status != "running":
			notReady = append(notReady, fmt.Sprintf("%s declares no health check and is %s, not running", container.name(), dockerContainerStatus(container)))
		}
	}
	if len(notReady) == 0 {
		r.Status = "succeeded"
		r.Reason = fmt.Sprintf("each of the %d containers of the Compose project in %s is healthy or, without a health check, runs or has completed", len(containers), directory)
		return r
	}
	r.Reason = fmt.Sprintf("%d of the %d containers of the Compose project in %s are not ready: %s", len(notReady), len(containers), directory, strings.Join(notReady, "; "))
	return r
}

// completed reports whether a container did its work and ended: its program
// exited with code 0, and its restart policy does not start it again. A
// service that runs once, such as a database migration, is in this state
// after docker compose up. A container that ended with another code, or that
// Docker restarts by its policy, is not.
func (c dockerContainer) completed() bool {
	policy := c.HostConfig.RestartPolicy.Name
	return c.State.Status == "exited" && c.State.ExitCode == 0 && (policy == "" || policy == "no" || policy == "on-failure")
}

// dockerContainerStatus is a container's state in docker's words, with the
// exit code of one that ended.
func dockerContainerStatus(container dockerContainer) string {
	if container.State.Status == "exited" || container.State.Status == "dead" {
		return fmt.Sprintf("%s with code %d", container.State.Status, container.State.ExitCode)
	}
	return container.State.Status
}

// dockerHealthStatus describes a container that is not healthy: its name,
// what Docker reports, and what its last health check printed.
func dockerHealthStatus(container dockerContainer) string {
	// Docker records no health until the container starts.
	status, log := "starting", []healthCheckRun(nil)
	if health := container.State.Health; health != nil {
		status, log = health.Status, health.Log
	}
	text := fmt.Sprintf("%s is %s", container.name(), status)
	if !container.runs() || container.State.Status != "running" {
		text += " (the container is " + dockerContainerStatus(container) + ")"
	}
	return text + lastHealthCheck(log)
}
