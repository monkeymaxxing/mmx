package modules

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

func (run Runtime) WaitFor(ctx context.Context, w WaitFor, s value.Scope) protocol.Result {
	return run.waitFor(ctx, w, s, waitObservation)
}

// waitFor accepts the observation function so tests can end a wait only after
// the observation they need has finished, without racing a real deadline.
func (run Runtime) waitFor(ctx context.Context, w WaitFor, s value.Scope, observe func(context.Context, WaitFor, *waitCommand) protocol.Result) protocol.Result {
	if err := ValidateWaitFor(w); err != nil {
		return protocol.ConfigurationError(err)
	}
	var command *waitCommand
	if w.CommandSucceeds != "" {
		env, account, stop := run.commandEnvironment("wait_for", w.User, w.Env, s)
		if stop != nil {
			return *stop
		}
		if missing := run.missingRunFromDirectory(w.Cwd); missing != nil {
			return *missing
		}
		command = &waitCommand{env, account}
	}
	duration, _ := w.Wait()
	ctx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()
	var finished *protocol.Result
	for {
		observation := observe(ctx, w, command)
		if observation.Status == "succeeded" {
			return observation
		}
		// A check that the end of the wait cut short can return a moment
		// before ctx reports the end, as a TCP connection attempt whose own
		// deadline fired first. Wait for ctx, so that the check counts as
		// cut short and the last finished one is reported.
		if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
			<-ctx.Done()
		}
		// A script that the end of the wait cut short says nothing; report
		// the last one that finished. The same holds for every other
		// check: a request or a docker or podman command that was still
		// running when the wait ended says only that it was interrupted.
		if command != nil && observation.ExitCode >= 0 {
			finished = &observation
		} else if command != nil && finished != nil && ctx.Err() != nil {
			observation = *finished
		} else if command == nil && ctx.Err() == nil {
			finished = &observation
		} else if command == nil && finished != nil {
			observation = *finished
		}
		// A wait for Docker's or Podman's health that cannot succeed, as for
		// a container without a health check, ends at once.
		if (w.waitsForDockerHealth() || w.waitsForPodmanHealth()) && observation.FailureKind != "" && !run.Dryrun {
			return observation
		}
		if run.Dryrun {
			observation.Status = "not_checked"
			return observation
		}
		timer := time.NewTimer(waitForCheckInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			observation.Reason = fmt.Sprintf("wait_for ended after at most %s: %v; last observation: %s", duration, ctx.Err(), observation.Reason)
			return observation
		case <-timer.C:
		}
	}
}

// waitForCheckInterval is the time between two checks of a wait_for: a
// second. Only the tests of this package make it shorter, once for all of
// them, where the interval is not what a test studies.
var waitForCheckInterval = time.Second

// waitCommand is what a wait_for script runs with, prepared once for the wait.
type waitCommand struct {
	env     map[string]string
	account *syscall.Credential
}

func waitObservation(ctx context.Context, w WaitFor, command *waitCommand) protocol.Result {
	r := protocol.Success("unchanged")
	r.Status = "failed"
	switch {
	case command != nil:
		// The command is a check: exit code 0 means ready, and it never
		// counts as a change. ctx ends it with the wait.
		r = runProcess(ctx, command.env, w.Cwd, nil, command.account, "/bin/bash", "-c", w.CommandSucceeds).Result
		r.Change = "unchanged"
		if r.Status == "succeeded" {
			r.Reason = "the wait_for.command_succeeds script exited with code 0"
		} else {
			r.Status = "failed"
			r.Reason = fmt.Sprintf("the wait_for.command_succeeds script exited with code %d", r.ExitCode)
			if r.ExitCode < 0 {
				r.Reason = "the wait_for.command_succeeds script did not finish"
			}
		}
	case w.waitsForDockerHealth():
		r = dockerHealthObservation(ctx, w)
	case w.waitsForPodmanHealth():
		r = podmanHealthObservation(ctx, w)
	case w.TCPPortAcceptsConnections != nil || w.TCPPortRefusesConnections != nil:
		port := w.TCPPortAcceptsConnections
		if port == nil {
			port = w.TCPPortRefusesConnections
		}
		host := w.TCPHost
		if host == "" {
			host = "127.0.0.1"
		}
		address := net.JoinHostPort(host, strconv.Itoa(*port))
		connection, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", address)
		if err == nil {
			connection.Close()
			r.Reason = "TCP port " + address + " accepts connections"
			if w.TCPPortAcceptsConnections != nil {
				r.Status = "succeeded"
			}
		} else {
			r.Reason = err.Error()
			if w.TCPPortRefusesConnections != nil && errors.Is(err, syscall.ECONNREFUSED) {
				r.Status = "succeeded"
			}
		}
	case w.PathExists != "" || w.PathIsMissing != "":
		path := w.PathExists
		if path == "" {
			path = w.PathIsMissing
		}
		_, err := os.Stat(path)
		switch {
		case err == nil:
			r.Reason = "path " + path + " exists"
			if w.PathExists != "" {
				r.Status = "succeeded"
			}
		case errors.Is(err, os.ErrNotExist):
			r.Reason = "path " + path + " is missing"
			if w.PathIsMissing != "" {
				r.Status = "succeeded"
			}
		default:
			r.Reason = err.Error()
		}
	case w.URLResponds != "":
		requestContext, cancel := context.WithTimeout(ctx, 10*time.Second)
		r = SendHTTPRequest(requestContext, HTTP{URL: w.URLResponds, ExpectedHTTPStatusCodes: w.ExpectedHTTPStatusCodes, TrustedCertificateAuthorityFile: w.TrustedCertificateAuthorityFile, DangerouslySkipServerCertificateCheck: w.DangerouslySkipServerCertificateCheck})
		cancel()
		if r.Status == "succeeded" && w.ResponseBodyContains != nil {
			if r.ResponseBodyTruncated || !strings.Contains(r.ResponseBody, *w.ResponseBodyContains) {
				r.Status = "failed"
				r.Reason = fmt.Sprintf("HTTP status %d; response_body does not contain the required text", r.HTTPStatusCode)
				if r.ResponseBodyTruncated {
					r.Reason = fmt.Sprintf("HTTP status %d; response_body is truncated; cannot check response_body_contains", r.HTTPStatusCode)
				}
			}
		}
	}
	return r
}

// WaitFor observes one readiness condition from the node.
type WaitFor struct {
	TCPPortAcceptsConnections             *int   `yaml:"tcp_port_accepts_connections,omitempty" json:"tcp_port_accepts_connections,omitempty"`
	TCPPortRefusesConnections             *int   `yaml:"tcp_port_refuses_connections,omitempty" json:"tcp_port_refuses_connections,omitempty"`
	TCPHost                               string `yaml:"tcp_host,omitempty" json:"tcp_host,omitempty"`
	PathExists                            string `yaml:"path_exists,omitempty" json:"path_exists,omitempty"`
	PathIsMissing                         string `yaml:"path_is_missing,omitempty" json:"path_is_missing,omitempty"`
	URLResponds                           string `yaml:"url_responds,omitempty" json:"url_responds,omitempty"`
	TrustedCertificateAuthorityFile       string `yaml:"trusted_certificate_authority_file,omitempty" json:"trusted_certificate_authority_file,omitempty"`
	DangerouslySkipServerCertificateCheck bool   `yaml:"dangerously_skip_server_certificate_check,omitempty" json:"dangerously_skip_server_certificate_check,omitempty"`
	// CommandSucceeds is a read-only command; exit code 0 means ready.
	CommandSucceeds         string         `yaml:"command_succeeds,omitempty" json:"command_succeeds,omitempty"`
	User                    string         `yaml:"run_as_user_account,omitempty" json:"run_as_user_account,omitempty"`
	Cwd                     string         `yaml:"run_from_directory,omitempty" json:"run_from_directory,omitempty"`
	Env                     map[string]any `yaml:"env,omitempty" json:"env,omitempty"`
	ExpectedHTTPStatusCodes []int          `yaml:"expected_http_status_codes,omitempty" json:"expected_http_status_codes,omitempty"`
	ResponseBodyContains    *string        `yaml:"response_body_contains,omitempty" json:"response_body_contains,omitempty"`
	WaitAtMost              string         `yaml:"wait_at_most,omitempty" json:"wait_at_most,omitempty"`

	// DockerContainerIsHealthy names a Docker container, and
	// DockerComposeProjectIsReady the directory of a Compose project;
	// docker_health.go reads their health.
	DockerContainerIsHealthy    string `yaml:"docker_container_is_healthy,omitempty" json:"docker_container_is_healthy,omitempty"`
	DockerComposeProjectIsReady string `yaml:"docker_compose_project_is_ready,omitempty" json:"docker_compose_project_is_ready,omitempty"`

	// PodmanContainerIsHealthy names a Podman container, and
	// PodmanContainerOfUserAccount the ordinary account whose container it is
	// (without it, the system's); podman_health.go reads its health.
	PodmanContainerIsHealthy     string `yaml:"podman_container_is_healthy,omitempty" json:"podman_container_is_healthy,omitempty"`
	PodmanContainerOfUserAccount string `yaml:"podman_container_of_user_account,omitempty" json:"podman_container_of_user_account,omitempty"`
}

func (w WaitFor) Wait() (time.Duration, error) {
	if w.WaitAtMost == "" {
		return time.Minute, nil
	}
	duration, err := time.ParseDuration(w.WaitAtMost)
	if err != nil || duration <= 0 {
		return 0, invalid("wait_at_most", "wait_for.wait_at_most must be a positive duration")
	}
	return duration, nil
}

func ValidateWaitFor(w WaitFor) error {
	targets := 0
	for _, present := range []bool{w.TCPPortAcceptsConnections != nil, w.TCPPortRefusesConnections != nil, w.PathExists != "", w.PathIsMissing != "", w.URLResponds != "", w.CommandSucceeds != "", w.DockerContainerIsHealthy != "", w.DockerComposeProjectIsReady != "", w.PodmanContainerIsHealthy != ""} {
		if present {
			targets++
		}
	}
	if targets != 1 {
		return fmt.Errorf("wait_for requires exactly one of tcp_port_accepts_connections, tcp_port_refuses_connections, path_exists, path_is_missing, url_responds, command_succeeds, docker_container_is_healthy, docker_compose_project_is_ready, or podman_container_is_healthy")
	}
	if w.PodmanContainerOfUserAccount != "" && !w.waitsForPodmanHealth() {
		return invalid("podman_container_of_user_account", "wait_for.podman_container_of_user_account requires podman_container_is_healthy; it names the account whose Podman container the task waits for")
	}
	if w.CommandSucceeds == "" && (w.User != "" || w.Cwd != "" || w.Env != nil) {
		return fmt.Errorf("wait_for.run_as_user_account, run_from_directory, and env require command_succeeds")
	}
	if w.CommandSucceeds != "" {
		if err := validateEnvNames(w.Env); err != nil {
			return err
		}
	}
	if w.TCPPortAcceptsConnections == nil && w.TCPPortRefusesConnections == nil && w.TCPHost != "" {
		return invalid("tcp_host", "wait_for.tcp_host requires tcp_port_accepts_connections or tcp_port_refuses_connections")
	}
	if w.URLResponds == "" && (w.ExpectedHTTPStatusCodes != nil || w.ResponseBodyContains != nil || w.TrustedCertificateAuthorityFile != "" || w.DangerouslySkipServerCertificateCheck) {
		return fmt.Errorf("wait_for.expected_http_status_codes, response_body_contains, trusted_certificate_authority_file, and dangerously_skip_server_certificate_check require url_responds")
	}
	for name, port := range map[string]*int{"tcp_port_accepts_connections": w.TCPPortAcceptsConnections, "tcp_port_refuses_connections": w.TCPPortRefusesConnections} {
		if port != nil && (*port < 1 || *port > 65535) {
			return invalid(name, "wait_for."+name+" must be between 1 and 65535")
		}
	}
	if w.URLResponds != "" {
		if err := validateHTTPStatusCodes(w.ExpectedHTTPStatusCodes); err != nil {
			return err
		}
	}
	if !writtenDuration(w.WaitAtMost) {
		return nil
	}
	_, err := w.Wait()
	return err
}

// validateHTTPStatusCodes checks a written list of accepted codes; an omitted
// list means [200].
func validateHTTPStatusCodes(codes []int) error {
	if codes != nil && len(codes) == 0 {
		return invalid("expected_http_status_codes", "expected_http_status_codes needs at least one code; omit it for [200]")
	}
	for _, code := range codes {
		if code < 100 || code > 599 {
			return invalid("expected_http_status_codes", "each expected_http_status_codes entry must be between 100 and 599")
		}
	}
	return nil
}

// waitForDescription is what mmx describe-module shows for wait_for.
var waitForDescription = Description{
	Name: "wait_for", Topic: TopicCommandsChecksAndReboot, Schema: reflect.TypeOf(WaitFor{}),
	Summary: "waits for a TCP port, a path, an HTTP response, a command, or the health of a Docker container, a Docker Compose project, or a Podman container to report ready, from the node",
	Fields: []Field{
		{Name: "tcp_port_accepts_connections", Notes: "TCP port from 1 to 65535; ready when a connection succeeds; exactly one readiness form is required"},
		{Name: "tcp_port_refuses_connections", Notes: "TCP port from 1 to 65535; ready when a connection is refused; a lookup failure or timeout does not count"},
		{Name: "tcp_host", Default: "127.0.0.1", Notes: "only with tcp_port_accepts_connections or tcp_port_refuses_connections; a machine the node can reach"},
		{Name: "path_exists", Notes: "absolute path; ready when the path exists"},
		{Name: "path_is_missing", Notes: "absolute path; ready when the path is missing"},
		{Name: "url_responds", Notes: "HTTP or HTTPS; read-only GET, no redirects; 10-second request limit bounded by the remaining wait"},
		{Name: "command_succeeds", Notes: "a read-only command run with bash -c; exit code 0 means ready, any other code means not yet; also runs once during dryrun"},
		{Name: "trusted_certificate_authority_file", Notes: "only with url_responds; absolute path on the node of a certificate authority's certificate, or a self-signed certificate, to trust"},
		{Name: "dangerously_skip_server_certificate_check", Default: "false", Notes: "only with url_responds; true removes verification of the HTTPS server's identity; prefer trusted_certificate_authority_file for a private authority"},
		{Name: "run_as_user_account", Default: "root", Notes: "only with command_succeeds; as on shell"},
		{Name: "run_from_directory", Default: "/", Notes: "only with command_succeeds; as on shell"},
		{Name: "env", Notes: "only with command_succeeds; environment variables, as on shell"},
		{Name: "docker_container_is_healthy", Notes: "the name of a Docker container; ready when Docker reports it healthy by the container's own health check; a container that declares no health check fails the task at once; one form of wait_for, instead of another readiness form"},
		{Name: "docker_compose_project_is_ready", Notes: "absolute path of a Docker Compose project directory. Ready when every container with a health check is healthy and every container without a health check is running or has completed. Completed means the container exited with code 0 and the restart policy does not start the container again, as for a service that runs once. Choose docker_compose_project_is_ready instead of another readiness form"},
		{Name: "podman_container_is_healthy", Notes: "the name of a Podman container (ContainerName= in its Quadlet file); ready when Podman reports it healthy by the container's own health check (HealthCmd=); a container that declares no health check fails the task at once; one form of wait_for, instead of another readiness form"},
		{Name: "podman_container_of_user_account", Notes: "only with podman_container_is_healthy: the account other than root whose container it is; without it, the container is the system's (root's)"},
		{Name: "expected_http_status_codes", Default: "[200]", Notes: "the list of accepted HTTP status codes; only with url_responds"},
		{Name: "response_body_contains", Notes: "optional; plain text (not a pattern) the response body must contain; references resolve; only with url_responds"},
		{Name: "wait_at_most", Default: "60s", Notes: "positive duration for the whole wait; checks repeat every second"},
	},
	Example:   "- wait_for:\n    url_responds: http://127.0.0.1:8080/health\n    response_body_contains: ready\n    wait_at_most: 30s\n",
	Results:   []string{"status", "change", "reason", "http_status_code", "response_body", "response_body_json", "exit_code", "stdout", "stderr"},
	Dryrun:    "checks once without waiting; reports not_checked when not ready; always unchanged",
	WaitLimit: "wait_at_most bounds the wait",
}

// Prepare checks the inputs when the project loads.
func (w *WaitFor) Prepare(base string) error {
	return ValidateWaitFor(*w)
}

// Reads lists the script, which runs as written: values reach it through env.
func (w *WaitFor) Reads() Reads {
	return Reads{ShellScripts: map[string]string{"command_succeeds": w.CommandSucceeds}}
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (w *WaitFor) Arguments(arguments *ArgumentList) {
	if w.URLResponds != "" {
		arguments.Public("url_responds", &w.URLResponds, httpURL)
	}
	if w.TrustedCertificateAuthorityFile != "" {
		arguments.Public("trusted_certificate_authority_file", &w.TrustedCertificateAuthorityFile, absolute)
	}
	if w.PathExists != "" {
		arguments.Public("path_exists", &w.PathExists, absolute)
	}
	if w.PathIsMissing != "" {
		arguments.Public("path_is_missing", &w.PathIsMissing, absolute)
	}
	arguments.Text("tcp_host", &w.TCPHost)
	arguments.Text("run_from_directory", &w.Cwd)
	if w.User != "" {
		arguments.Public("run_as_user_account", &w.User, accountNameRule)
	}
	for _, k := range slices.Sorted(maps.Keys(w.Env)) {
		arguments.Private("env."+k, w.Env[k], environmentValue)
	}
	if w.ResponseBodyContains != nil {
		arguments.Text("response_body_contains", w.ResponseBodyContains)
	}
	if w.DockerContainerIsHealthy != "" {
		arguments.Public("docker_container_is_healthy", &w.DockerContainerIsHealthy, containerNameRule("docker_container_is_healthy"))
	}
	if w.DockerComposeProjectIsReady != "" {
		arguments.Public("docker_compose_project_is_ready", &w.DockerComposeProjectIsReady, absolute)
	}
	if w.PodmanContainerIsHealthy != "" {
		arguments.Public("podman_container_is_healthy", &w.PodmanContainerIsHealthy, containerNameRule("podman_container_is_healthy"))
	}
	if w.PodmanContainerOfUserAccount != "" {
		arguments.Public("podman_container_of_user_account", &w.PodmanContainerOfUserAccount, ordinaryContainerAccount)
	}
}

// Run does the module's work on the node.
func (w *WaitFor) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.Runtime.WaitFor(ctx, *w, task.Scope)
}

// containerNameRule is the rule that Docker and Podman both have for a
// container's name; field is the wait_for field that holds the name.
func containerNameRule(field string) func(any) error {
	const lettersAndDigits = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	return rule(field+" must be a container's name: letters, digits, _, . and -, starting with a letter or digit", func(s string) bool {
		return s != "" && strings.ContainsAny(s[:1], lettersAndDigits) && strings.Trim(s, lettersAndDigits+"_.-") == ""
	})
}

// healthCheckRun is one run of a container's own health check, as docker
// container inspect and podman container inspect both print it in the
// container's health log.
type healthCheckRun struct {
	ExitCode int
	Output   string
}

// lastHealthCheck ends the description of a container that is not healthy
// with what the last run in its health log exited with and printed, cut to
// 2000 bytes. It is empty for an empty log.
func lastHealthCheck(log []healthCheckRun) string {
	if len(log) == 0 {
		return ""
	}
	last := log[len(log)-1]
	output := strings.TrimSpace(last.Output)
	if len(output) > 2000 {
		output = strings.ToValidUTF8(output[:2000], "") + " ..."
	}
	text := fmt.Sprintf(", and its last health check exited with code %d", last.ExitCode)
	if output != "" {
		text += " and printed: " + output
	}
	return text
}
