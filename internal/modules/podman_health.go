package modules

import (
	"context"
	"encoding/json"
	"fmt"

	"mmx/internal/protocol"
)

// The Podman form of wait_for: podman_container_is_healthy, with
// podman_container_of_user_account for an ordinary account's container. It reads
// the health that Podman records from the container's own health check, and
// changes nothing.

// waitsForPodmanHealth reports whether the wait is the Podman form.
func (w WaitFor) waitsForPodmanHealth() bool {
	return w.PodmanContainerIsHealthy != ""
}

// podmanContainer is what MMX reads of podman container inspect.
type podmanContainer struct {
	State struct {
		Status string
		// Podman before 4.0 names the health Healthcheck.
		Health, Healthcheck *podmanHealth
	}
	Config struct {
		Healthcheck *struct {
			Test []string
		}
	}
}

// podmanHealth is a container's health as podman container inspect prints it.
type podmanHealth struct {
	Status string
	Log    []healthCheckRun
}

// podmanHealthObservation reads the health once. A result with a failure
// kind says that the wait cannot succeed, so it ends at once: the podman
// command or the account is missing, or the container declares no health
// check.
func podmanHealthObservation(ctx context.Context, w WaitFor) protocol.Result {
	r := protocol.Result{Status: "failed", Change: "unchanged"}
	name := w.PodmanContainerIsHealthy
	tool, stop := Runtime{}.podman(ctx, "wait_for", "podman_container_of_user_account", w.PodmanContainerOfUserAccount)
	if stop != nil {
		r.Reason, r.FailureKind = stop.Reason, stop.FailureKind
		return r
	}
	of := ""
	//nolint:nilaway // The preceding stop branch returns before accessing a failed constructor's tool.
	if account := tool.manager.Account; account != "" {
		of = " of account " + account
		// An account's containers run under its own systemd, and podman
		// needs the account's runtime directory, which is there with it.
		//nolint:nilaway // The preceding stop branch guarantees that tool exists.
		if !tool.manager.Running() {
			r.Reason = fmt.Sprintf("the systemd of account %s does not run, so the account's containers do not run", account)
			return r
		}
	}
	exists := []string{"container", "exists", name}
	if found := tool.command(ctx, exists...); found.Status != "succeeded" {
		if found.ExitCode == 1 && ctx.Err() == nil {
			r.Reason = "podman has no container named " + name + of
		} else {
			r.Reason = tool.failure(exists, found).Error()
		}
		return r
	}
	inspect := []string{"container", "inspect", "--format", "json", name}
	inspected := tool.command(ctx, inspect...)
	if inspected.Status != "succeeded" {
		r.Reason = tool.failure(inspect, inspected).Error()
		return r
	}
	var containers []podmanContainer
	if err := json.Unmarshal([]byte(inspected.Stdout), &containers); err != nil || len(containers) != 1 {
		r.Reason = "wait_for: podman container inspect printed no state for " + name
		return r
	}
	container := containers[0]
	health := container.State.Health
	if health == nil {
		health = container.State.Healthcheck
	}
	declared := container.Config.Healthcheck
	switch {
	case declared == nil || len(declared.Test) == 0 || declared.Test[0] == "NONE":
		r.Reason = fmt.Sprintf("the container %s declares no health check, so Podman never reports it healthy; declare one with HealthCmd= in its Quadlet file, or wait for a port or a url instead", name+of)
		r.FailureKind = "missing_prerequisite"
	case health != nil && health.Status == "healthy":
		r.Status, r.Reason = "succeeded", "the container "+name+of+" is healthy"
	default:
		r.Reason = "the container " + podmanHealthStatus(name+of, container, health)
	}
	return r
}

// podmanHealthStatus describes a container that is not healthy: its name,
// what Podman reports, and what its last health check printed.
func podmanHealthStatus(name string, container podmanContainer, health *podmanHealth) string {
	status := "not known yet"
	if health != nil && health.Status != "" {
		status = health.Status
	}
	text := fmt.Sprintf("%s is %s", name, status)
	if container.State.Status != "running" {
		text += " (the container is " + container.State.Status + ")"
	}
	if health != nil {
		text += lastHealthCheck(health.Log)
	}
	return text
}
