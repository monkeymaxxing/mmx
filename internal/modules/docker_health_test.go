package modules

import (
	"context"
	"strings"
	"testing"
	"time"

	"mmx/internal/protocol"
)

func TestWaitForDockerHealth(t *testing.T) {
	t.Parallel()
	f := newDockerFixture(t)
	ctx := testContext(t)
	f.shop("service web "+testImage+" web-1", "service worker "+testDatabaseImage+" worker-1")
	f.set("registry/"+testImageKey, "sha256:web1 sha256:web-manifest1")
	f.set("registry/"+testDatabaseImageKey, "sha256:worker1 sha256:worker-manifest1")
	project := DockerComposeProject{ComposeProjectName: "shop", ComposeProjectDir: f.project, State: "running", PullImages: "when_missing", ContainersOfRemovedServices: "remove"}
	projectWait := WaitFor{DockerComposeProjectIsReady: f.project, WaitAtMost: "30s"}
	containerWait := WaitFor{DockerContainerIsHealthy: "shop-web-1", WaitAtMost: "30s"}
	for _, w := range []WaitFor{projectWait, containerWait} {
		if err := w.Prepare(""); err != nil {
			t.Fatal(err)
		}
	}
	// Nothing to wait for yet: a dryrun cannot check, a run waits and fails.
	if r := (Runtime{Dryrun: true}).WaitFor(ctx, projectWait, nil); r.Status != "not_checked" || r.Reason != "the Compose project in "+f.project+" has no containers" {
		t.Fatalf("%+v", r)
	}
	if r := waitForOneFinishedObservation(Runtime{}, ctx, containerWait, nil); r.Status != "failed" || !strings.Contains(r.Reason, "wait_for ended after at most 30s") || !strings.Contains(r.Reason, "last observation: docker has no container named shop-web-1") {
		t.Fatalf("%+v", r)
	}
	if r := f.compose(Runtime{}, project, nil); r.Status != "succeeded" {
		t.Fatalf("%+v", r)
	}
	web, worker := f.container("web"), f.container("worker")
	// worker declares no health check, so it must run; web must be healthy.
	f.set(worker+"/health", "none")
	takeCalls(t, f.dir)
	for _, w := range []WaitFor{projectWait, containerWait} {
		for _, run := range []Runtime{{}, noMutation(t)} {
			if r := run.WaitFor(ctx, w, nil); r.Status != "succeeded" || r.Change != "unchanged" {
				t.Fatalf("%+v", r)
			}
		}
	}
	// The waits only read.
	if calls := changing(takeCalls(t, f.dir), "docker ps", "docker container inspect"); calls != nil {
		t.Fatal(calls)
	}
	// An unhealthy container fails the wait with its status and what its
	// last health check printed.
	f.set(web+"/health", "unhealthy")
	f.set(web+"/health-exit-code", "7")
	f.set(web+"/health-output", "connection to the application refused")
	for _, w := range []WaitFor{projectWait, containerWait} {
		r := waitForOneFinishedObservation(Runtime{}, ctx, w, nil)
		if r.Status != "failed" || !strings.Contains(r.Reason, "shop-web-1 is unhealthy, and its last health check exited with code 7 and printed: connection to the application refused") {
			t.Fatalf("%+v", r)
		}
		if r := (Runtime{Dryrun: true}).WaitFor(ctx, w, nil); r.Status != "not_checked" {
			t.Fatalf("%+v", r)
		}
	}
	if r := waitForOneFinishedObservation(Runtime{}, ctx, projectWait, nil); !strings.Contains(r.Reason, "1 of the 2 containers of the Compose project in "+f.project+" are not ready") {
		t.Fatalf("%+v", r)
	}
	// The wait goes on while a container starts, and ends when it is
	// healthy.
	f.set(web+"/health", "starting")
	observedStarting := false
	r := (Runtime{}).waitFor(ctx, projectWait, nil, func(ctx context.Context, w WaitFor, command *waitCommand) protocol.Result {
		observation := waitObservation(ctx, w, command)
		if !observedStarting {
			if observation.Status != "failed" || !strings.Contains(observation.Reason, "is starting") {
				t.Fatal(observation)
			}
			observedStarting = true
			f.set(web+"/health", "healthy")
		}
		return observation
	})
	if r.Status != "succeeded" || !observedStarting || !strings.Contains(r.Reason, "each of the 2 containers") {
		t.Fatalf("%+v", r)
	}
	// A container without a health check that does not run.
	f.set(worker+"/status", "exited")
	f.set(worker+"/exit-code", "3")
	if r := waitForOneFinishedObservation(Runtime{}, ctx, projectWait, nil); r.Status != "failed" || !strings.Contains(r.Reason, "shop-worker-2 declares no health check and is exited with code 3, not running") {
		t.Fatalf("%+v", r)
	}
	// A container without a health check that did its work and ended, as a
	// service that runs once does: it exited with code 0 and its restart
	// policy does not start it again. With a policy that restarts it, it
	// was stopped, and the wait fails.
	f.set(worker+"/exit-code", "0")
	for policy, done := range map[string]bool{"no": true, "on-failure": true, "always": false, "unless-stopped": false} {
		f.set(worker+"/restart-policy", policy)
		r := waitForOneFinishedObservation(Runtime{}, ctx, projectWait, nil)
		if done != (r.Status == "succeeded") || (done && !strings.Contains(r.Reason, "each of the 2 containers of the Compose project in "+f.project+" is healthy or, without a health check, runs or has completed")) ||
			(!done && !strings.Contains(r.Reason, "shop-worker-2 declares no health check and is exited with code 0, not running")) {
			t.Fatalf("%s: %+v", policy, r)
		}
	}
	f.set(worker+"/restart-policy", "no")
	// The directory with a slash at its end is the same project.
	if r := (Runtime{}).WaitFor(ctx, WaitFor{DockerComposeProjectIsReady: f.project + "/", WaitAtMost: "30s"}, nil); r.Status != "succeeded" {
		t.Fatalf("%+v", r)
	}
	// A daemon that stops answering is an error, not a missing container.
	f.set("daemon-stops-before", "container")
	if r := waitForOneFinishedObservation(Runtime{}, ctx, WaitFor{DockerContainerIsHealthy: "shop-web-1", WaitAtMost: "30s"}, nil); r.Status != "failed" || strings.Contains(r.Reason, "docker has no container named") || !strings.Contains(r.Reason, "wait_for: docker container inspect shop-web-1: Cannot connect to the Docker daemon") {
		t.Fatalf("%+v", r)
	}
	f.remove("daemon-stops-before")
	f.remove("daemon-down")
	f.set(worker+"/exit-code", "3")
	// A container that declares a health check but has not started has no
	// recorded health yet (State.Health is null): the wait goes on.
	f.set(web+"/health", "created")
	f.set(web+"/status", "created")
	for _, w := range []WaitFor{projectWait, containerWait} {
		r := waitForOneFinishedObservation(Runtime{}, ctx, w, nil)
		if r.Status != "failed" || r.FailureKind != "" || !strings.Contains(r.Reason, "shop-web-1 is starting") {
			t.Fatalf("%+v", r)
		}
	}
	f.set(web+"/health", "healthy")
	f.set(web+"/status", "running")
	// A container that declares no health check never reports healthy: the
	// wait for it fails at once, long before wait_at_most.
	started := time.Now()
	r = (Runtime{}).WaitFor(ctx, WaitFor{DockerContainerIsHealthy: "shop-worker-2", WaitAtMost: "30s"}, nil)
	if r.Status != "failed" || r.FailureKind != "missing_prerequisite" || !strings.HasPrefix(r.Reason, "the container shop-worker-2 declares no health check, so Docker never reports it healthy") || time.Since(started) > 10*time.Second {
		t.Fatalf("%+v", r)
	}
	if r := (Runtime{Dryrun: true}).WaitFor(ctx, WaitFor{DockerContainerIsHealthy: "shop-worker-2"}, nil); r.Status != "not_checked" {
		t.Fatalf("%+v", r)
	}
	// Without the docker command the wait ends at once too.
	setWorkerEnvironment(t, "PATH", t.TempDir())
	started = time.Now()
	r = (Runtime{}).WaitFor(ctx, WaitFor{DockerComposeProjectIsReady: f.project, WaitAtMost: "30s"}, nil)
	if r.Status != "failed" || r.FailureKind != "missing_prerequisite" || r.Reason != "wait_for: "+missingProgramError("docker").Error()+"; docker-ce comes from Docker's own package repository" || time.Since(started) > 10*time.Second {
		t.Fatalf("%+v", r)
	}
}

func TestWaitForDockerHealthRejectsInputs(t *testing.T) {
	t.Parallel()
	port := 80
	for want, w := range map[string]WaitFor{
		"wait_for requires exactly one of tcp_port_accepts_connections, tcp_port_refuses_connections, path_exists, path_is_missing, url_responds, command_succeeds, docker_container_is_healthy, docker_compose_project_is_ready, or podman_container_is_healthy": {DockerContainerIsHealthy: "web", DockerComposeProjectIsReady: "/opt/shop"},
		"wait_for requires exactly one of tcp_port_accepts_connections":                      {DockerContainerIsHealthy: "web", TCPPortAcceptsConnections: &port},
		"wait_for.run_as_user_account, run_from_directory, and env require command_succeeds": {DockerContainerIsHealthy: "web", User: "app"},
		"wait_for.wait_at_most must be a positive duration":                                  {DockerContainerIsHealthy: "web", WaitAtMost: "soon"},
	} {
		if err := ValidateWaitFor(w); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatal(want, err)
		}
	}
	w := WaitFor{DockerContainerIsHealthy: "web server", DockerComposeProjectIsReady: "shop"}
	var arguments ArgumentList
	w.Arguments(&arguments)
	want := map[string]string{
		"docker_container_is_healthy":     "docker_container_is_healthy must be a container's name",
		"docker_compose_project_is_ready": "path must be absolute",
	}
	for _, argument := range arguments.List {
		if err := argument.Rule(argument.Raw); err == nil || !strings.Contains(err.Error(), want[argument.Field]) {
			t.Fatal(argument.Field, err)
		}
		delete(want, argument.Field)
	}
	if len(want) != 0 {
		t.Fatal("no argument for", want)
	}
	for _, name := range []string{"audio-converter", "shop-web-1", "a.b_c"} {
		if err := containerNameRule("docker_container_is_healthy")(name); err != nil {
			t.Fatal(name, err)
		}
	}
}
