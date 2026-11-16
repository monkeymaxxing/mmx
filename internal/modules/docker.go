package modules

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"mmx/internal/protocol"
)

// What docker_image, docker_compose_project, and the Docker forms of
// wait_for share: finding the docker command, asking it what is installed,
// running it with a task's private logins, and reading images and
// containers. MMX reads only JSON that docker prints (docker info, docker
// inspect), never the text of its progress. The one exception is the list of
// steps that Compose's own dry run prints; docker_compose_project.go says why
// and how.

// lowestDockerComposeVersion is the lowest Docker Compose that MMX is tested
// with: the lowest that a supported distribution packages (Debian 13).
// Older versions have faults that break the module's contract, such as
// 2.8.0 to 2.12.2, which recreate a container on the run after the one that
// pulled its image.
var lowestDockerComposeVersion = []int{2, 26, 1}

// dockerHubLoginName is the name under which docker keeps the login for
// Docker Hub, the registry of a reference that names none.
const dockerHubLoginName = "https://index.docker.io/v1/"

// docker is the docker command of one task.
type docker struct {
	run Runtime
	// module names the task's module in failures.
	module  string
	program string
	env     map[string]string
	// What docker info reported.
	plugins map[string]string // each command line plugin's program, by its name
	// composeVersion is the Compose plugin's version, "" without the plugin.
	composeVersion string
	// containerdImageStore: the daemon keeps images in containerd, where an
	// image's ID is the digest of its manifest. In the classic store it is
	// the digest of the image's configuration.
	containerdImageStore bool
	// socket is the daemon's Unix socket, once asked for.
	socket string
}

// findDocker finds the docker command; it returns nil when the node has
// none.
func (run Runtime) findDocker(ctx context.Context, module string) *docker {
	program, err := lookPath(ctx, "docker")
	if err != nil {
		return nil
	}
	return &docker{run: run, module: module, program: program, env: map[string]string{
		"DOCKER_CLI_HINTS": "false", "COMPOSE_ANSI": "never", "COMPOSE_PROGRESS": "plain", "COMPOSE_MENU": "false",
	}}
}

// dockerIsMissing is the result for a node without the docker command, which
// an earlier node task may still install.
func (run Runtime) dockerIsMissing(module string) protocol.Result {
	return run.notYet(fmt.Errorf("%s: %w; docker-ce comes from Docker's own package repository", module, missingProgramError("docker")))
}

// newDocker finds the docker command and checks that the Docker daemon
// answers, and with compose also that the Compose plugin is installed in a
// version MMX can use. Each of the three may still come from an earlier node
// task.
func (run Runtime) newDocker(ctx context.Context, module string, compose bool) (*docker, *protocol.Result) {
	d := run.findDocker(ctx, module)
	if d == nil {
		r := run.dockerIsMissing(module)
		return nil, &r
	}
	if stop := d.inspectInstallation(ctx); stop != nil {
		return nil, stop
	}
	if compose {
		if stop := d.requireCompose(); stop != nil {
			return nil, stop
		}
	}
	return d, nil
}

// inspectInstallation asks docker info for the daemon, the command line
// plugins, and the image store.
func (d *docker) inspectInstallation(ctx context.Context) *protocol.Result {
	// docker info exits with a failure when the daemon does not answer, and
	// still prints what the command itself knows.
	r := runProcess(ctx, d.env, "/", nil, nil, d.program, "info", "--format", "{{json .}}").Result
	var info struct {
		ServerVersion string
		ServerErrors  []string
		DriverStatus  [][]string
		ClientInfo    struct {
			Plugins []struct{ Name, Path, Version, Err string }
		}
	}
	if err := json.Unmarshal([]byte(r.Stdout), &info); err != nil {
		failed := protocol.Failed(fmt.Errorf("%s: docker info printed no report MMX can read: %s", d.module, failureTextFromStandardError(r)))
		return &failed
	}
	if info.ServerVersion == "" || len(info.ServerErrors) > 0 {
		detail := strings.Join(info.ServerErrors, "; ")
		if detail == "" {
			detail = lastLines(strings.TrimSpace(r.Stderr), 3)
		}
		stop := d.run.notYet(fmt.Errorf("%s: the Docker daemon does not answer (%s); start it in an earlier node task, for example with systemd_service and name: docker", d.module, detail))
		return &stop
	}
	d.plugins = map[string]string{}
	for _, plugin := range info.ClientInfo.Plugins {
		if plugin.Err != "" || plugin.Path == "" {
			continue
		}
		d.plugins[plugin.Name] = plugin.Path
		if plugin.Name == "compose" {
			d.composeVersion = plugin.Version
		}
	}
	for _, status := range info.DriverStatus {
		if len(status) == 2 && status[0] == "driver-type" && strings.HasPrefix(status[1], "io.containerd.snapshotter") {
			d.containerdImageStore = true
		}
	}
	return nil
}

// dockerComposeVersionNumber is the numbers a Compose version starts with,
// as in v5.6.0 and 2.40.3+ds1-0ubuntu1.
var dockerComposeVersionNumber = regexp.MustCompile(`^v?([0-9]+(\.[0-9]+)*)`)

// requireCompose checks the Compose plugin and its version.
func (d *docker) requireCompose() *protocol.Result {
	if d.plugins["compose"] == "" {
		stop := d.run.notYet(fmt.Errorf("%s: the Docker Compose plugin is not installed (the docker command has no compose command); install the Docker Compose plugin in an earlier node task, for example the package docker-compose-plugin", d.module))
		return &stop
	}
	lowest := make([]string, len(lowestDockerComposeVersion))
	for i, n := range lowestDockerComposeVersion {
		lowest[i] = strconv.Itoa(n)
	}
	number := dockerComposeVersionNumber.FindStringSubmatch(d.composeVersion)
	if number == nil || !versionAtLeast(number[1], lowestDockerComposeVersion) {
		stop := d.run.notYet(fmt.Errorf("%s: Docker Compose %s is installed, and MMX needs Docker Compose %s or newer; install a newer Compose plugin in an earlier node task", d.module, strings.TrimPrefix(d.composeVersion, "v"), strings.Join(lowest, ".")))
		return &stop
	}
	return nil
}

// failure is the error of a docker command that failed; it starts with the
// task's module, as every failure of these modules does. Standard output
// never goes into it (failureTextFromStandardError): docker compose config
// and docker inspect print the values of an application's environment
// there.
func (d *docker) failure(args []string, r protocol.Result) error {
	return fmt.Errorf("%s: docker %s: %s", d.module, strings.Join(args, " "), failureTextFromStandardError(r)) //nolint:nilaway // Both callers return on newDocker's non-nil stop; successful construction supplies a non-nil receiver.
}

// command runs docker with arguments that never hold a private value, and
// input on its standard input when there is one.
func (d *docker) command(ctx context.Context, input io.Reader, args ...string) (protocol.Result, error) {
	r := runProcess(ctx, d.env, "/", input, nil, d.program, args...).Result
	if err := ctx.Err(); err != nil {
		return r, err
	}
	if r.Status != "succeeded" {
		return r, d.failure(args, r)
	}
	if r.StdoutTruncated {
		return r, fmt.Errorf("%s: docker %s printed more than 1 MiB", d.module, strings.Join(args, " "))
	}
	return r, nil
}

// read runs a docker command that only reads and returns its standard
// output.
func (d *docker) read(ctx context.Context, args ...string) (string, error) {
	r, err := d.command(ctx, nil, args...)
	return r.Stdout, err
}

// change runs a docker command that changes the node, after the mutation
// boundary; resource names what changes, in the boundary's words.
func (d *docker) change(ctx context.Context, resource string, args ...string) error {
	if err := d.run.Mutation(d.module + " " + resource); err != nil {
		return err
	}
	_, err := d.command(ctx, nil, args...)
	return err
}

// inspected runs a docker inspect command, which prints a JSON list of what
// it found, and decodes the list. What does not exist is left out, without
// an error.
//
// docker inspect fails in the same way when something it was asked for does
// not exist and when the daemon does not answer, and only its error text
// tells the two apart. MMX does not read that text: after a failed inspect it
// asks the daemon for its version. A daemon that answers means that the
// inspect's own answer counts; one that does not is an error, so that a
// daemon which stopped is never read as "there is no such container".
func (d *docker) inspected(ctx context.Context, list any, args ...string) error {
	r := runProcess(ctx, d.env, "/", nil, nil, d.program, args...).Result
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.StdoutTruncated {
		return fmt.Errorf("%s: docker %s printed more than 1 MiB", d.module, strings.Join(args, " ")) //nolint:nilaway // Image and container inspections occur only after the caller checks newDocker's stop.
	}
	if r.Status != "succeeded" {
		if _, err := d.command(ctx, nil, "version", "--format", "{{.Server.Version}}"); err != nil {
			return d.failure(args, r)
		}
	}
	if err := json.Unmarshal([]byte(r.Stdout), list); err != nil {
		return d.failure(args, r)
	}
	return nil
}

// useLogins gives docker the task's logins through a private configuration
// directory of the task (makePrivateTaskDirectory), which holds nothing else but
// links to the command line plugins that docker found before. The returned
// function removes the directory; a password is then nowhere else: not on a
// command line, in an environment, or in an account's home directory. Only a
// run calls it: no command of a dryrun pulls, and the dryrun's question to a
// registry carries the login itself.
func (d *docker) useLogins(logins []registryLogin) (func(), error) {
	if len(logins) == 0 {
		return func() {}, nil
	}
	directory, remove, err := d.run.makePrivateTaskDirectory(d.module, nil, func(directory string) error {
		err := os.WriteFile(filepath.Join(directory, "config.json"), registryLoginFile(logins), 0600)
		// docker looks for a plugin in its configuration directory first and
		// then in the system's directories. A plugin that an administrator
		// put into root's own configuration directory is found here again.
		if err == nil {
			err = os.Mkdir(filepath.Join(directory, "cli-plugins"), 0700)
		}
		for name, program := range d.plugins {
			if err == nil {
				err = os.Symlink(program, filepath.Join(directory, "cli-plugins", "docker-"+name))
			}
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	d.env["DOCKER_CONFIG"] = directory //nolint:nilaway // Both callers check stop; findDocker also initializes the receiver's env map.
	return remove, nil
}

// dockerLoginName is the name under which docker keeps the login of a
// registry: the registry's host, or Docker Hub's own name.
func dockerLoginName(registry string) string {
	switch registry {
	case "", "docker.io", "index.docker.io", "registry-1.docker.io":
		return dockerHubLoginName
	}
	return registry
}

// loginFor finds the task's login for the registry of an image reference.
func loginFor(logins []registryLogin, reference string) *registryLogin {
	parsed, err := parseImageReference(reference)
	if err != nil {
		return nil
	}
	name := dockerLoginName(parsed.Registry)
	for i := range logins {
		if logins[i].Registry == name {
			return &logins[i]
		}
	}
	return nil
}

// dockerImageState is one image of the node.
type dockerImageState struct {
	// ID is the image ID as docker reports it.
	ID string `json:"Id"`
	// RepoDigests are the digests under which registries served the image,
	// each as repository@digest.
	RepoDigests []string
}

// came reports whether the image is the one that a registry serves under
// digest.
func (image dockerImageState) came(digest string) bool {
	if image.ID == digest {
		return true
	}
	for _, served := range image.RepoDigests {
		if _, d, found := strings.Cut(served, "@"); found && d == digest {
			return true
		}
	}
	return false
}

// image reads the image that a reference names on the node; found is false
// when the node has none.
func (d *docker) image(ctx context.Context, reference string) (image dockerImageState, found bool, err error) {
	var list []dockerImageState
	if err := d.inspected(ctx, &list, "image", "inspect", reference); err != nil {
		return image, false, err
	}
	if len(list) == 0 {
		return image, false, nil
	}
	return list[0], true, nil
}

// dockerContainer is one container, as docker container inspect reports it.
type dockerContainer struct {
	ID   string `json:"Id"`
	Name string
	// Image is the ID of the image the container was created from.
	Image string
	State struct {
		Status    string
		ExitCode  int
		StartedAt string
		Health    *struct {
			Status string
			Log    []healthCheckRun
		}
	}
	Config struct {
		// Image is the image reference the container was created with.
		Image  string
		Labels map[string]string
		// Healthcheck is the health check the container declares, from its
		// image, docker run, or Compose file. Docker leaves State.Health
		// null until the container starts, so this says whether one exists.
		Healthcheck *struct{ Test []string }
	}
	HostConfig struct {
		// RestartPolicy says when Docker starts the container again by
		// itself: no, on-failure, always, or unless-stopped.
		RestartPolicy struct{ Name string }
	}
}

// name is the container's name without docker's leading slash.
func (c dockerContainer) name() string { return strings.TrimPrefix(c.Name, "/") }

// declaresHealthCheck reports whether the container has a health check,
// even before Docker records any health for it.
func (c dockerContainer) declaresHealthCheck() bool {
	declared := c.Config.Healthcheck
	return declared != nil && len(declared.Test) > 0 && declared.Test[0] != "NONE"
}

// runs reports whether the container's process runs or docker is about to
// start it again.
func (c dockerContainer) runs() bool {
	return c.State.Status == "running" || c.State.Status == "restarting" || c.State.Status == "paused"
}

// The labels Docker Compose gives each container it creates.
const (
	composeLabelProject          = "com.docker.compose.project"
	composeLabelProjectDirectory = "com.docker.compose.project.working_dir"
	composeLabelService          = "com.docker.compose.service"
	composeLabelVersion          = "com.docker.compose.version"
	composeLabelOneOff           = "com.docker.compose.oneoff"
)

// containersWithLabels reads every container, running or not, that has all
// of the labels, sorted by name.
func (d *docker) containersWithLabels(ctx context.Context, labels map[string]string) ([]dockerContainer, error) {
	args := []string{"ps", "--all", "--quiet", "--no-trunc"}
	for _, name := range slices.Sorted(maps.Keys(labels)) {
		args = append(args, "--filter", "label="+name+"="+labels[name])
	}
	out, err := d.read(ctx, args...)
	if err != nil {
		return nil, err
	}
	ids := strings.Fields(out)
	if len(ids) == 0 {
		return nil, nil
	}
	var list []dockerContainer
	// A container that was removed since the listing is left out.
	if err := d.inspected(ctx, &list, append([]string{"container", "inspect"}, ids...)...); err != nil {
		return nil, err
	}
	slices.SortFunc(list, func(a, b dockerContainer) int { return strings.Compare(a.Name, b.Name) })
	return list, nil
}

// container reads one container by its name; found is false when there is
// none.
func (d *docker) container(ctx context.Context, name string) (container dockerContainer, found bool, err error) {
	var list []dockerContainer
	if err := d.inspected(ctx, &list, "container", "inspect", name); err != nil {
		return container, false, err
	}
	if len(list) == 0 {
		return container, false, nil
	}
	return list[0], true, nil
}

// dockerLoginFields names, for each module, the inputs that give a task its
// login for a registry.
var dockerLoginFields = map[string]string{
	"docker_image":           "registry_username and registry_password",
	"docker_compose_project": "registry_logins",
}

// registryAnswer is what a registry serves under a reference now.
type registryAnswer struct {
	// Digest is the digest of what the reference names: the manifest of one
	// image, or a list of images.
	Digest string
	// ListOfImages: the reference names a list of images, one for each
	// platform, and Digest is the digest of that list. The list changes
	// when the image for any platform changes, and a registry's answer does
	// not hold the digest of the image for one platform. So another digest
	// than the node's does not say whether the image for the node's own
	// platform changed. Docker's official images are such lists, and their
	// platforms are rebuilt at different times.
	ListOfImages bool
}

// The media types of the manifest of one image. Everything else that a
// registry serves under a reference counts as a list of images, about which
// MMX knows less.
var manifestsOfOneImage = []string{
	"application/vnd.docker.distribution.manifest.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
}

// daemonClient is an HTTP client that asks the daemon itself, on the Unix
// socket that the docker command reaches it on, for what the command has no
// command for; what names that, for an error.
func (d *docker) daemonClient(ctx context.Context, what string) (*http.Client, error) {
	if d.socket == "" {
		out, err := d.read(ctx, "context", "inspect", "--format", "{{.Endpoints.docker.Host}}")
		if err != nil {
			return nil, err
		}
		address := strings.TrimSpace(out)
		path, isSocket := strings.CutPrefix(address, "unix://")
		if !isSocket {
			return nil, fmt.Errorf("%s: the docker command reaches the Docker daemon at %s; MMX can %s only through a Docker daemon on a Unix socket on the node", d.module, address, what)
		}
		d.socket = path //nolint:nilaway // Every caller returns on newDocker's non-nil stop; successful construction supplies a non-nil receiver.
	}
	return &http.Client{Timeout: time.Minute, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", d.socket)
		},
	}}, nil
}

// registryDigest asks which image a reference names in its registry now,
// without downloading the image. The docker command has no command for this
// question that follows the daemon's settings for registries (which ones it
// trusts without TLS, their certificates, mirrors), so MMX asks the daemon
// itself on its Unix socket, with the request that docker pull starts with.
// The answer is the digest of the manifest or of the list of images that the
// reference names, and which of the two it is. login is the task's login for
// the registry, nil without one.
func (d *docker) registryDigest(ctx context.Context, reference string, login *registryLogin) (registryAnswer, error) {
	client, err := d.daemonClient(ctx, "ask a registry without downloading")
	if err != nil {
		return registryAnswer{}, err
	}
	defer client.CloseIdleConnections()
	address := url.URL{Scheme: "http", Host: "docker", Path: "/distribution/" + reference + "/json"}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address.String(), nil)
	if err != nil {
		return registryAnswer{}, err
	}
	if login != nil {
		// The login travels in a header on the node's own socket, as docker
		// pull sends it.
		request.Header.Set("X-Registry-Auth", login.headerForDockerDaemon())
	}
	response, err := client.Do(request)
	if err != nil {
		return registryAnswer{}, fmt.Errorf("%s: asking the Docker daemon about %s: %w", d.module, reference, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, outputLimit))
	if err != nil {
		return registryAnswer{}, fmt.Errorf("%s: asking the Docker daemon about %s: %w", d.module, reference, err)
	}
	var answer struct {
		Descriptor struct{ Digest, MediaType string }
		Message    string `json:"message"`
	}
	_ = json.Unmarshal(body, &answer)
	if response.StatusCode == http.StatusOK && answer.Descriptor.Digest != "" {
		return registryAnswer{Digest: answer.Descriptor.Digest, ListOfImages: !slices.Contains(manifestsOfOneImage, answer.Descriptor.MediaType)}, nil
	}
	detail := answer.Message
	if detail == "" {
		detail = fmt.Sprintf("the Docker daemon answered with HTTP status %d", response.StatusCode)
	}
	// A registry refuses a question about a private image, or answers that
	// there is no such image. The status tells; MMX does not read the text.
	refused := response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden
	switch {
	case refused && login != nil:
		detail += ". Check the login that the task gives for the registry"
	case login == nil && (refused || response.StatusCode == http.StatusNotFound):
		detail += ". If the image is private, a dryrun needs the registry login from " + dockerLoginFields[d.module] + " on the node task"
	}
	return registryAnswer{}, fmt.Errorf("%s: the registry does not say which image %s names: %s", d.module, reference, detail) //nolint:nilaway // DockerImage returns on constructor failure before calling registryDigest.
}
