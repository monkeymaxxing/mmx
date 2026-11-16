package modules

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

// DockerComposeProject is the containers of one Docker Compose project:
// running, stopped, or removed. Every container setting is in the Compose
// file, which a file task writes; this module only brings the project to
// its state with docker compose.
type DockerComposeProject struct {
	ComposeProjectDir           string                `yaml:"compose_project_dir" json:"compose_project_dir"`
	State                       string                `yaml:"state" json:"state"`
	PullImages                  string                `yaml:"pull_images,omitempty" json:"pull_images,omitempty"`
	ContainersOfRemovedServices string                `yaml:"containers_of_removed_services,omitempty" json:"containers_of_removed_services,omitempty"`
	ComposeFiles                []string              `yaml:"compose_files,omitempty" json:"compose_files,omitempty"`
	ComposeProjectName          string                `yaml:"compose_project_name,omitempty" json:"compose_project_name,omitempty"`
	RegistryLogins              []DockerRegistryLogin `yaml:"registry_logins,omitempty" json:"registry_logins,omitempty"`
}

// DockerRegistryLogin is one entry of registry_logins. RegistryUsername and
// RegistryPassword are private values that the module hands only to docker
// as a login (resolveRegistryLogin).
type DockerRegistryLogin struct {
	Registry         string `yaml:"registry" json:"registry"`
	RegistryUsername string `yaml:"registry_username" json:"registry_username"`
	RegistryPassword string `yaml:"registry_password" json:"registry_password"`
}

// The states of a Compose project. The two removal states say in their
// names whether the data in named volumes stays.
const (
	composeProjectRunning                  = "running"
	composeProjectStopped                  = "stopped"
	composeProjectContainersRemoved        = "containers_removed_named_volumes_kept"
	composeProjectContainersVolumesRemoved = "containers_and_named_volumes_removed"
	// composeDefaultFile is the Compose file of a task without compose_files.
	composeDefaultFile = "compose.yaml"
)

var (
	dockerComposeProjectStates         = []string{composeProjectRunning, composeProjectStopped, composeProjectContainersRemoved, composeProjectContainersVolumesRemoved}
	containersOfRemovedServicesChoices = []string{"remove", "keep"}
)

// dockerComposeProjectDescription is what mmx describe-module shows for
// docker_compose_project.
var dockerComposeProjectDescription = Description{
	Name: "docker_compose_project", Topic: TopicContainers, Schema: reflect.TypeOf(DockerComposeProject{}),
	Summary: "the containers of one Docker Compose project: running, stopped, or removed; the Compose file is written by a file task and holds every container setting",
	Fields: []Field{
		{Name: "compose_project_dir", Required: true, Notes: "absolute path of the directory on the node that holds the Compose file; relative paths inside the Compose file start in compose_project_dir"},
		{Name: "state", Required: true, Values: dockerComposeProjectStates, Notes: "running runs docker compose up: creates missing containers, recreates containers whose definition or image differs, and starts stopped containers. stopped stops and keeps the containers. containers_removed_named_volumes_kept removes containers and networks, and keeps named volumes and volume data. containers_and_named_volumes_removed also deletes named volumes and volume data. No state deletes images, the Compose file, or directories on the node."},
		{Name: "pull_images", Values: pullImageChoices, Notes: "required with state: running, no default; when_missing pulls an image only when the node does not have it; always asks the registry on every run which image each reference names now and recreates the containers whose image differs"},
		{Name: "containers_of_removed_services", Values: containersOfRemovedServicesChoices, Notes: "required with state: running, no default; what happens to a container of this project whose service is no longer in the Compose file"},
		{Name: "compose_files", Default: "[compose.yaml]", Notes: "relative paths inside compose_project_dir, including subdirectories, in Compose's order; a .. path that leaves the project directory is refused"},
		{Name: "compose_project_name", Required: true, Notes: "required in every state. Use lowercase letters, digits, - and _, starting with a letter or digit. Give every Compose project a separate name. If another directory uses the requested project name, the node task fails and changes nothing."},
		{Name: "registry_logins", Notes: "only with state: running; a list of {registry, registry_username, registry_password} for private registries, used for this task's pulls and for a dryrun's question to the registry; registry is the registry's host as image references name it, with its port if they have one; registry_username and registry_password are private values, the password normally a secret; in a run they reach docker through a private directory under /run that the task removes, and a dryrun writes them nowhere; a secret in a login does not hide the task's result: MMX shows [redacted] wherever a text of the result holds the secret"},
	},
	Example: "- docker_compose_project:\n    compose_project_name: shop\n    compose_project_dir: /opt/shop\n    state: running\n    pull_images: when_missing\n    containers_of_removed_services: remove\n",
	Results: []string{"status", "change", "reason", "value.services", "value.images"},
	Dryrun:  "changes nothing and writes no login; asks Docker Compose's own dry run (docker compose --dry-run up --no-start) what a run would do to each container; value.services lists for each service what a run would do (would_create, would_recreate, would_start, would_stop, would_remove, or unchanged) with the reason, which names both Compose versions when another Compose version made the container, or not_checked with the reason when the dryrun cannot tell: with pull_images: always, when an image's reference names a list of images for several platforms and the registry serves another list, the dryrun cannot see whether the image for the node's platform changed; the entry of such an image in value.images then has a reason too, unless the node's image store gives the image another ID for certain; the task reports would_change when something else is certain to change and not_checked otherwise; when an earlier node task of the same dryrun would change a file in compose_project_dir, or any file in the same task_block, every service reports would_change with a reason that says so; with pull_images: always it asks each registry without downloading, with the task's own logins only, and reports not_checked with the error when one does not answer; not_checked with the reason when Compose's dry run fails or prints a line that MMX does not know; a removal names the volumes that it would delete; not_checked while the docker command, the Compose plugin, the Docker daemon, or the Compose file is missing",
}

// composeProjectName is Compose's own rule for a project name.
var composeProjectName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// Prepare checks the inputs when the project loads.
func (p *DockerComposeProject) Prepare(base string) error {
	switch {
	case p.State == "":
		return invalid("state", "docker_compose_project needs state: "+strings.Join(dockerComposeProjectStates, ", "))
	case !slices.Contains(dockerComposeProjectStates, p.State):
		return invalid("state", "state must be "+strings.Join(dockerComposeProjectStates, ", "))
	}
	if p.State == composeProjectRunning {
		switch {
		case p.PullImages == "":
			return invalid("pull_images", "state: running needs pull_images: when_missing or always; there is no default, because pull_images decides whether a rerun deploys newer images")
		case !slices.Contains(pullImageChoices, p.PullImages):
			return invalid("pull_images", "pull_images must be "+strings.Join(pullImageChoices, " or "))
		case p.ContainersOfRemovedServices == "":
			return invalid("containers_of_removed_services", "state: running needs containers_of_removed_services: remove or keep; there is no default, because containers_of_removed_services decides whether a rerun removes containers")
		case !slices.Contains(containersOfRemovedServicesChoices, p.ContainersOfRemovedServices):
			return invalid("containers_of_removed_services", "containers_of_removed_services must be "+strings.Join(containersOfRemovedServicesChoices, " or "))
		}
	} else {
		for field, set := range map[string]bool{"pull_images": p.PullImages != "", "containers_of_removed_services": p.ContainersOfRemovedServices != "", "registry_logins": len(p.RegistryLogins) > 0} {
			if set {
				return invalid(field, "state: "+p.State+" accepts only compose_project_dir, compose_files, and compose_project_name; remove "+field)
			}
		}

	}
	if p.ComposeProjectName == "" {
		return invalid("compose_project_name", "compose_project_name is required in every state; add compose_project_name with this project's name so directories cannot silently share containers or volumes")
	}

	if p.ComposeFiles != nil && len(p.ComposeFiles) == 0 {
		return invalid("compose_files", "compose_files needs at least one file name; omit compose_files to use compose.yaml")
	}
	for i, name := range p.ComposeFiles {
		if slices.Contains(p.ComposeFiles[:i], name) {
			return invalid("compose_files", "compose_files lists "+name+" twice")
		}
	}
	for i, login := range p.RegistryLogins {
		if login.Registry == "" || login.RegistryUsername == "" || login.RegistryPassword == "" {
			return invalid(fmt.Sprintf("registry_logins[%d]", i), "each entry in registry_logins needs registry, registry_username, and registry_password")
		}
		for _, earlier := range p.RegistryLogins[:i] {
			if earlier.Registry == login.Registry {
				return invalid(fmt.Sprintf("registry_logins[%d].registry", i), "registry_logins names the registry "+login.Registry+" twice")
			}
		}
	}
	return nil
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (p *DockerComposeProject) Arguments(arguments *ArgumentList) {
	arguments.Public("compose_project_dir", &p.ComposeProjectDir, absolute)
	for i := range p.ComposeFiles {
		arguments.Public(fmt.Sprintf("compose_files[%d]", i), &p.ComposeFiles[i], rule("compose_files must be relative paths inside compose_project_dir", func(path string) bool {
			clean := filepath.Clean(path)
			return path != "" && clean != "." && !filepath.IsAbs(path) && clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator)) && !strings.ContainsRune(path, 0)
		}))
	}
	if p.ComposeProjectName != "" {
		arguments.Public("compose_project_name", &p.ComposeProjectName, rule("compose_project_name must use lowercase letters, digits, - and _, and start with a letter or digit", composeProjectName.MatchString))
	}
	for i := range p.RegistryLogins {
		login := &p.RegistryLogins[i]
		arguments.Public(fmt.Sprintf("registry_logins[%d].registry", i), &login.Registry, rule("registry must be a registry's host name with an optional port, such as registry.example.com or registry.example.com:5000", imageRegistry.MatchString))
		arguments.LoginHandedOnlyToATool(fmt.Sprintf("registry_logins[%d].registry_username", i), login.RegistryUsername, scalar)
		arguments.LoginHandedOnlyToATool(fmt.Sprintf("registry_logins[%d].registry_password", i), login.RegistryPassword, scalar)
	}
}

// CleanPaths cleans the resolved path.
func (p *DockerComposeProject) CleanPaths() {
	p.ComposeProjectDir = filepath.Clean(p.ComposeProjectDir)
}

// Run does the module's work on the node.
func (p *DockerComposeProject) Run(ctx context.Context, task NodeTask) protocol.Result {
	if p.State == composeProjectRunning {
		return task.Runtime.dockerComposeProjectRunning(ctx, *p, task)
	}
	return task.Runtime.dockerComposeProjectEnded(ctx, *p)
}

// logins resolves the task's registry logins.
func (p DockerComposeProject) logins(s value.Scope) ([]registryLogin, error) {
	var logins []registryLogin
	for i, written := range p.RegistryLogins {
		login, err := resolveRegistryLogin(s, dockerLoginName(written.Registry), fmt.Sprintf("docker_compose_project.registry_logins[%d]", i), written.RegistryUsername, written.RegistryPassword)
		if err != nil {
			return nil, err
		}
		logins = append(logins, login)
	}
	return logins, nil
}

// composeProject is one task's project with the docker command that works
// on it.
type composeProject struct {
	d *docker
	p DockerComposeProject
}

// files are the project's Compose files. MMX always names them, so that
// Compose neither searches the directories above nor adds an override file
// that the task does not list.
func (c composeProject) files() []string {
	names := c.p.ComposeFiles
	if len(names) == 0 {
		names = []string{composeDefaultFile}
	}
	files := make([]string, len(names))
	for i, name := range names {
		files[i] = filepath.Join(c.p.ComposeProjectDir, name)
	}
	return files
}

// missingFile is the first Compose file that does not exist, "" when all do.
func (c composeProject) missingFile() string {
	for _, file := range c.files() {
		if _, err := os.Stat(file); err != nil {
			return file
		}
	}
	return ""
}

// withFiles is a docker compose command on the project as its files define
// it.
func (c composeProject) withFiles(args ...string) []string {
	command := []string{"compose", "--project-directory", c.p.ComposeProjectDir, "--project-name", c.p.ComposeProjectName}
	for _, file := range c.files() {
		command = append(command, "--file", file)
	}
	return append(command, args...)
}

// byName is a docker compose command on a project that Compose finds by the
// labels of its containers, without a Compose file.
func byName(project string, args ...string) []string {
	return append([]string{"compose", "--project-name", project}, args...)
}

// labels select the project's containers. Compose labels each container
// with its project's directory, so the project is found without its Compose
// file, also when the file or the directory is gone.
func (c composeProject) labels(name string) map[string]string {
	labels := map[string]string{composeLabelProjectDirectory: c.p.ComposeProjectDir}
	if c.p.ComposeProjectName != "" {
		name = c.p.ComposeProjectName
	}
	if name != "" {
		labels[composeLabelProject] = name
	}
	return labels
}

// composeDefinition is the project as docker compose config resolves it.
type composeDefinition struct {
	Name     string
	Services map[string]composeServiceDefinition
	Volumes  map[string]struct {
		Name     string
		External json.RawMessage
	}
}

type composeServiceDefinition struct {
	Image string
	Build json.RawMessage
	// ContainerName is the name that the Compose file gives the service's
	// container; without it Compose names a container
	// <project>-<service>-<number>.
	ContainerName string `json:"container_name"`
	// DependsOn holds the services that this one depends on, by name.
	DependsOn map[string]json.RawMessage `json:"depends_on"`
}

// pulled reports whether the service's image comes from a registry: it
// names one and is not built on the node.
func (s composeServiceDefinition) pulled() bool {
	return s.Image != "" && (len(s.Build) == 0 || string(s.Build) == "null")
}

// definition reads the project's resolved definition.
func (c composeProject) definition(ctx context.Context) (composeDefinition, error) {
	var definition composeDefinition
	out, err := c.d.read(ctx, c.withFiles("config", "--format", "json")...)
	if err != nil {
		return definition, err
	}
	if err := json.Unmarshal([]byte(out), &definition); err != nil {
		return definition, fmt.Errorf("docker_compose_project: docker compose config printed a definition MMX cannot read: %w", err)
	}
	return definition, nil
}

// serviceContainers groups containers by their service. With oneOff, the
// containers that docker compose run started count too.
func serviceContainers(containers []dockerContainer, oneOff bool) map[string][]dockerContainer {
	services := map[string][]dockerContainer{}
	for _, container := range containers {
		if !oneOff && strings.EqualFold(container.Config.Labels[composeLabelOneOff], "true") {
			continue
		}
		service := container.Config.Labels[composeLabelService]
		services[service] = append(services[service], container)
	}
	return services
}

// composeService is one entry of the result's services: what a run did to
// the service's containers or, in a dryrun, would do and why.
type composeService struct {
	Name, Action, Reason string
}

// composeServiceActions compares the project's containers before and after
// a docker compose command: one action for each service.
func composeServiceActions(before, after map[string][]dockerContainer, defined []string) []composeService {
	names := map[string]bool{}
	for _, name := range defined {
		names[name] = true
	}
	for name := range before {
		names[name] = true
	}
	for name := range after {
		names[name] = true
	}
	var services []composeService
	for _, name := range slices.Sorted(maps.Keys(names)) {
		was := map[string]dockerContainer{}
		for _, container := range before[name] {
			was[container.ID] = container
		}
		created, kept, started, stopped := 0, 0, false, false
		for _, container := range after[name] {
			earlier, existed := was[container.ID]
			if !existed {
				created++
				continue
			}
			kept++
			// A container that ran once more has a new start time, also when
			// it has ended again since.
			started = started || container.State.StartedAt != earlier.State.StartedAt || (container.runs() && !earlier.runs())
			stopped = stopped || (earlier.runs() && !container.runs())
		}
		gone := len(was) - kept
		action := "unchanged"
		switch {
		case created > 0 && gone > 0:
			action = "recreated"
		case created > 0:
			action = "created"
		case gone > 0:
			action = "removed"
		case started:
			action = "started"
		case stopped:
			action = "stopped"
		}
		services = append(services, composeService{Name: name, Action: action})
	}
	return services
}

// composeDryrunActions are a dryrun's words for what a run would do to a
// service. A run's own words, such as recreated, would read as done.
var composeDryrunActions = map[string]string{
	"created": "would_create", "recreated": "would_recreate", "started": "would_start", "stopped": "would_stop", "removed": "would_remove",
}

// composeNotChecked is a service's action in a dryrun that cannot tell what
// a run would do to the service; its reason says why.
const composeNotChecked = "not_checked"

// composeProjectResult is the task's result. In a dryrun each service that
// would change carries its reason, and its action says "would". A service
// that a dryrun could not check, and an image of which it cannot tell whether
// a pull changes it (an entry with a reason), are no change.
func composeProjectResult(run Runtime, services []composeService, images []any) protocol.Result {
	change := "unchanged"
	list := []any{}
	for _, service := range services {
		entry := map[string]any{"name": service.Name, "action": service.Action}
		switch {
		case service.Action == composeNotChecked:
			entry["reason"] = service.Reason
		case service.Action != "unchanged":
			change = "changed"
			if run.Dryrun {
				entry["reason"] = service.Reason
				if would, known := composeDryrunActions[service.Action]; known {
					entry["action"] = would
				}
			}
		}
		list = append(list, entry)
	}
	if images == nil {
		images = []any{}
	}
	for _, image := range images {
		if entry, _ := image.(map[string]any); entry["reason"] == nil {
			change = "changed"
		}
	}
	if change == "changed" && run.Dryrun {
		change = "would_change"
	}
	r := protocol.Success(change)
	r.Value = map[string]any{"services": list, "images": images}
	return r
}

// filesWouldChange is the reason of every service in a dryrun whose earlier
// node tasks would change the project's files, or a file of the same node
// task block, which may be an environment file that a service reads.
const filesWouldChange = "an earlier node task of this dryrun would change a file of this Compose project or of this node task block"

// definitionChanges reports whether an earlier node task of this dryrun
// would change the project's definition: a file in the project's directory,
// or any file of the same node task block, where MMX cannot tell an
// environment file that a service reads from another file.
func (p DockerComposeProject) definitionChanges(task NodeTask) bool {
	if !task.Runtime.Dryrun {
		return false
	}
	return len(task.PreviewPathsOfTaskBlock) > 0 || task.Runtime.EarlierNodeTaskWouldChange(p.ComposeProjectDir)
}

// dockerComposeProjectRunning brings the project's containers to the state
// its Compose file defines.
func (run Runtime) dockerComposeProjectRunning(ctx context.Context, p DockerComposeProject, task NodeTask) protocol.Result {
	d, stop := run.newDocker(ctx, "docker_compose_project", true)
	if stop != nil {
		return *stop
	}
	c := composeProject{d: d, p: p}
	if p.definitionChanges(task) {
		return c.resultBeforeDefinitionChanges(ctx)
	}
	if missing := c.missingFile(); missing != "" {
		return run.notYet(fmt.Errorf("docker_compose_project: the Compose file %s does not exist; an earlier node task, such as a file task, may create it", missing))
	}
	logins, err := p.logins(task.Scope)
	if err != nil {
		return protocol.Failed(err)
	}
	definition, err := c.definition(ctx)
	if err != nil {
		return protocol.Failed(err)
	}
	if stop := c.nameSharedWithAnotherDirectory(ctx, definition.Name); stop != nil {
		return *stop
	}
	before, err := d.containersWithLabels(ctx, c.labels(definition.Name))
	if err != nil {
		return protocol.Failed(err)
	}
	// The images that come from a registry, as the node has them now.
	var references []string
	for _, service := range definition.Services {
		if service.pulled() && !slices.Contains(references, service.Image) {
			references = append(references, service.Image)
		}
	}
	slices.Sort(references)
	node := map[string]dockerImageState{}
	for _, reference := range references {
		image, found, err := d.image(ctx, reference)
		if err != nil {
			return protocol.Failed(err)
		}
		if found {
			node[reference] = image
		}
	}
	// Compose alone knows every part of its own rule for what differs. A run
	// lets docker compose up decide and reports what it did from the
	// containers before and after; a dryrun asks Compose's own dry run.
	if run.Dryrun {
		return c.predict(ctx, definition, before, node, references, logins)
	}
	// Only a run pulls, so only a run writes the task's logins for docker.
	cleanup, err := d.useLogins(logins)
	if err != nil {
		return protocol.Failed(fmt.Errorf("docker_compose_project: the private directory for the registry logins: %w", err))
	}
	defer cleanup()
	up := []string{"up", "--detach", "--quiet-pull", "--pull", map[string]string{"always": "always", "when_missing": "missing"}[p.PullImages]}
	if p.ContainersOfRemovedServices == "remove" {
		up = append(up, "--remove-orphans")
	}
	failed := d.change(ctx, "project "+p.ComposeProjectDir, c.withFiles(up...)...)
	after, err := d.containersWithLabels(ctx, c.labels(definition.Name))
	if err != nil {
		return protocol.Failed(errors.Join(failed, err))
	}
	var pulled []any
	for _, reference := range references {
		image, found, err := d.image(ctx, reference)
		if err != nil {
			return protocol.Failed(errors.Join(failed, err))
		}
		earlier, had := node[reference]
		if !found || (had && earlier.ID == image.ID) {
			continue
		}
		var previous any
		if had {
			previous = earlier.ID
		}
		pulled = append(pulled, map[string]any{"image": reference, "previous_image_id": previous, "image_id": image.ID})
	}
	r := composeProjectResult(run, composeServiceActions(serviceContainers(before, false), serviceContainers(after, false), slices.Collect(maps.Keys(definition.Services))), pulled)
	if failed != nil {
		// What the command changed before it failed stays in the result.
		failure := protocol.Failed(failed)
		if r.Change == "changed" {
			failure.Change, failure.Value = "changed", r.Value
		}
		return failure
	}
	return r
}

// predict is a dryrun's result: for each service, what docker compose up
// would do and why, and the images it would pull.
func (c composeProject) predict(ctx context.Context, definition composeDefinition, containers []dockerContainer, node map[string]dockerImageState, references []string, logins []registryLogin) protocol.Result {
	notChecked := func(err error) protocol.Result {
		return protocol.Result{Status: "not_checked", Change: "unknown", Reason: err.Error()}
	}
	// What a run would pull: an image the node does not have and, with
	// pull_images: always, one for which the registry names another image
	// now. A digest names one image forever, so there is nothing to ask. In
	// the containerd image store the image ID is the digest under which the
	// registry serves the image, so a dryrun knows it before the pull; in
	// the classic store docker learns the ID with the pull.
	//
	// A reference may name a list of images, one for each platform. When the
	// registry serves another list than the node's image came from, its
	// answer does not say whether the image for the node's platform changed.
	// In the classic image store a pull then may change nothing. In the
	// containerd image store the image ID is the list's digest and does
	// change, and Compose decides whether that recreates a container:
	// Compose 2.40 does, Compose 5 only when the platform's image changed.
	// So the services that use such an image are not checked.
	var wouldPull []any
	newer, unseen := map[string]string{}, map[string]string{}
	for _, reference := range references {
		image, found := node[reference]
		parsed, _ := parseImageReference(reference)
		var previous, predicted any
		if found {
			previous = image.ID
		}
		pull := !found
		if pull && parsed.Digest != "" && c.d.containerdImageStore {
			predicted = parsed.Digest
		}
		if c.p.PullImages == "always" && parsed.Digest == "" {
			served, err := c.d.registryDigest(ctx, reference, loginFor(logins, reference))
			if err != nil {
				return notChecked(err)
			}
			anotherList := "the registry now serves another list of images for " + reference + "; whether the image for this node's platform changed, a dryrun cannot see"
			switch {
			case !found || image.came(served.Digest):
			case !served.ListOfImages:
				pull, newer[reference] = true, "the registry names another image for "+reference+" now"
			case c.d.containerdImageStore:
				pull = true
				unseen[reference] = anotherList + ". A run pulls the list, which gives the image on the node another ID, and Docker Compose then decides whether it recreates the container"
			default:
				unseen[reference] = anotherList + ". A run pulls, and Docker Compose recreates the container only if it did"
				wouldPull = append(wouldPull, map[string]any{"image": reference, "previous_image_id": previous, "image_id": nil,
					"reason": anotherList + ". A run pulls, and the image on the node changes only if it did"})
			}
			if pull && c.d.containerdImageStore {
				predicted = served.Digest
			}
		}
		if pull {
			wouldPull = append(wouldPull, map[string]any{"image": reference, "previous_image_id": previous, "image_id": predicted})
		}
	}
	steps, err := c.dryRun(ctx)
	if err != nil {
		return notChecked(err)
	}
	services, err := c.servicesOfDryRun(definition, containers, steps, node, newer, unseen)
	if err != nil {
		return notChecked(err)
	}
	r := composeProjectResult(c.d.run, services, wouldPull)
	// Nothing is certain to change, and of something the dryrun cannot tell:
	// the task is not checked. Its value still says which services and
	// images that is.
	if r.Change == "unchanged" && len(unseen) > 0 {
		var reasons []string
		for _, reference := range slices.Sorted(maps.Keys(unseen)) {
			reasons = append(reasons, unseen[reference])
		}
		r.Status, r.Change, r.Reason = "not_checked", "unknown", "docker_compose_project: "+strings.Join(reasons, "; ")
	}
	return r
}

// resultBeforeDefinitionChanges is a dryrun's result when an earlier node
// task would change the project's files: the services can be compared only
// once the files are installed, so each one MMX knows of, from the files as
// they are and from the containers, reports would_change.
func (c composeProject) resultBeforeDefinitionChanges(ctx context.Context) protocol.Result {
	names := map[string]bool{}
	name := ""
	if c.missingFile() == "" {
		if definition, err := c.definition(ctx); err == nil {
			name = definition.Name
			for service := range definition.Services {
				names[service] = true
			}
		}
	}
	if containers, err := c.d.containersWithLabels(ctx, c.labels(name)); err == nil {
		for service := range serviceContainers(containers, false) {
			names[service] = true
		}
	}
	list := []any{}
	for _, service := range slices.Sorted(maps.Keys(names)) {
		list = append(list, map[string]any{"name": service, "action": "would_change", "reason": filesWouldChange})
	}
	r := protocol.Success("would_change")
	r.Reason = filesWouldChange + "; MMX can compare the services with the containers only once the files are installed"
	r.Value = map[string]any{"services": list, "images": []any{}}
	return r
}

// Docker Compose's own dry run
//
// A dryrun asks Docker Compose itself what docker compose up would do to the
// containers: docker compose --dry-run up --no-start. Compose alone knows
// its rule for recreating a container: its hash of the service's definition
// with the values of the environment files, the image, a service that this
// one depends on, a changed network. MMX cannot compute that rule: docker
// compose config --hash does not print the hash that up stores on every
// version (measured for a service with env_file on Compose 2.23.3 to 2.40.3
// and on 5.0.0 and 5.1.0), and the rule itself differs between versions.
//
// The dry run prints one line for each step, as text. Compose 2.26.1, the
// lowest version MMX supports, has no other form: --progress json exists
// from 2.29.0, and its fields changed with 5.0.0. So MMX reads the text, with
// a closed list of words. A line that MMX does not know, or a container that
// the lines do not account for, makes the task not_checked: what a later
// Compose prints differently is never read as a wrong prediction.
//
// --no-start asks for the creation step alone, which decides what happens to
// each container. The start step of a dry run waits for the real containers
// that a service depends on: on Compose 2 it never returns when a service
// must complete first (service_completed_successfully) and its container is
// new, and it fails while such a container is unhealthy. What the start step
// adds, MMX sees itself: up starts each container that does not run.
//
// --pull never keeps the dry run away from the registries: it would treat
// every image that it pulls as another image and recreate each container.
// Which images a run would pull, MMX works out itself.
//
// Compose 2 prepares its build tool (buildx) in a dry run and so creates
// that tool's state files in docker's configuration directory, in the home
// directory of the account. BUILDX_CONFIG sends them to a private directory
// of the task (makePrivateTaskDirectory), which goes when the command ends: the
// dry run then leaves nothing on the node.

// composeDryRunSteps are the words of Compose's dry run that MMX knows, for
// each kind of thing that a line names.
var composeDryRunSteps = map[string][]string{
	"Container": {"Running", "Recreate", "Recreated", "Creating", "Created", "Stopping", "Stopped", "Removing", "Removed"},
	"Network":   {"Creating", "Created", "Removing", "Removed"},
	"Volume":    {"Creating", "Created"},
}

// composeDryRun holds the steps that Compose's dry run lists for each
// container, by the container's name.
type composeDryRun map[string][]string

// dryRun asks Docker Compose what the creation step of docker compose up
// would do, with the images that the node has now. It changes nothing.
func (c composeProject) dryRun(ctx context.Context) (composeDryRun, error) {
	up := []string{"--dry-run", "--progress", "plain", "up", "--no-start", "--pull", "never"}
	if c.p.ContainersOfRemovedServices == "remove" {
		up = append(up, "--remove-orphans")
	}
	scratch, remove, err := c.d.run.makePrivateTaskDirectory(c.d.module, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("docker_compose_project: MMX cannot tell what a run would do: %w", err)
	}
	defer remove()
	c.d.env["BUILDX_CONFIG"] = scratch
	defer delete(c.d.env, "BUILDX_CONFIG")
	r, err := c.d.command(ctx, nil, c.withFiles(up...)...)
	if err != nil {
		return nil, fmt.Errorf("docker_compose_project: MMX cannot tell what a run would do, because Docker Compose's own dry run failed: %s", strings.TrimPrefix(err.Error(), c.d.module+": "))
	}
	return readComposeDryRun(r.Stderr)
}

// readComposeDryRun reads the lines of Compose's dry run. Compose 2 prints
// " DRY-RUN MODE -  Container shop-web-1  Recreate", Compose 5 prints
// " Container shop-web-1 Recreate ", and Compose 2.26.1 puts a volume's name
// in quotation marks. A warning is a line of its own and names no step.
func readComposeDryRun(text string) (composeDryRun, error) {
	steps := composeDryRun{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || (strings.HasPrefix(line, "time=") && strings.Contains(line, " level=warning ")) {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "DRY-RUN MODE -"))
		if len(fields) != 3 || !slices.Contains(composeDryRunSteps[fields[0]], fields[2]) {
			return nil, fmt.Errorf("docker_compose_project: MMX cannot tell what a run would do, because Docker Compose's own dry run printed a line that MMX does not know: %s", line)
		}
		if fields[0] == "Container" {
			steps[fields[1]] = append(steps[fields[1]], fields[2])
		}
	}
	return steps, nil
}

// serviceOfNewContainer finds the service whose container a run would
// create under a name: the name that the service's definition gives its
// container, or Compose's own, <project>-<service>-<number>.
func (definition composeDefinition) serviceOfNewContainer(name string) (string, bool) {
	var found []string
	for service, d := range definition.Services {
		number, numbered := strings.CutPrefix(name, definition.Name+"-"+service+"-")
		if _, err := strconv.Atoi(number); name == d.ContainerName || (numbered && err == nil) {
			found = append(found, service)
		}
	}
	if len(found) != 1 {
		return "", false
	}
	return found[0], true
}

// servicesOfDryRun is one predicted action for each service, from the steps
// of Compose's dry run: the services of the Compose file, and those that
// have containers and are no longer in it. node holds the images the node
// has, by reference; newer holds, by reference, why a run would pull another
// image than the node has; unseen holds, by reference, why a dryrun cannot
// tell whether a run's pull changes the image of a container.
func (c composeProject) servicesOfDryRun(definition composeDefinition, containers []dockerContainer, steps composeDryRun, node map[string]dockerImageState, newer, unseen map[string]string) ([]composeService, error) {
	unknown := func(format string, args ...any) error {
		return fmt.Errorf("docker_compose_project: MMX cannot tell what a run would do, because Docker Compose's own dry run "+format, args...)
	}
	// The steps of each container that exists. The names that are left are
	// those of containers that a run would create.
	listed := map[string][]string{}
	for _, container := range containers {
		listed[container.ID] = steps[container.name()]
		delete(steps, container.name())
	}
	created := map[string]int{}
	for _, name := range slices.Sorted(maps.Keys(steps)) {
		service, known := definition.serviceOfNewContainer(name)
		if !known || !slices.Contains(steps[name], "Creating") {
			return nil, unknown("names the container %s, which MMX cannot match to a service of the Compose file", name)
		}
		created[service]++
	}

	byService := serviceContainers(containers, false)
	names := map[string]bool{}
	for name := range definition.Services {
		names[name] = true
	}
	for name := range byService {
		names[name] = true
	}
	var services []composeService
	recreated := map[string][]dockerContainer{}
	for _, name := range slices.Sorted(maps.Keys(names)) {
		d, defined := definition.Services[name]
		existing := byService[name]
		removed, restarted, stopped := 0, 0, 0
		for _, container := range existing {
			of := listed[container.ID]
			has := func(step string) bool { return slices.Contains(of, step) }
			switch {
			case defined && has("Recreate"):
				recreated[name] = append(recreated[name], container)
			case has("Removing"):
				removed++
			// Compose stops a container and starts it again when it recreates
			// a service that the container's service depends on, or a network.
			case defined && has("Stopping"):
				restarted++
			case defined && has("Running"):
			// A container that exists and does not run: Compose 2.26.1 lists
			// it as Created, later versions do not list it.
			case defined && !container.runs() && !slices.ContainsFunc(of, func(step string) bool { return step != "Created" }):
				stopped++
			// A container whose service left the Compose file stays unless
			// Compose removes it.
			case !defined && len(of) == 0:
			default:
				return nil, unknown("does not say what happens to the container %s of service %s; its steps for it: %s", container.name(), name, strings.Join(append([]string{}, of...), ", "))
			}
		}
		service := composeService{Name: name, Action: "unchanged"}
		switch {
		case len(recreated[name]) > 0:
			// The reason follows below, when every service has its action.
			service.Action = "recreated"
		case defined && d.pulled() && newer[d.Image] != "" && len(existing) > removed:
			service.Action, service.Reason = "recreated", "the image differs: "+newer[d.Image]
		case created[name] > 0 && len(existing) == 0:
			service.Action, service.Reason = "created", "the container is missing"
		case created[name] > 0:
			service.Action, service.Reason = "created", fmt.Sprintf("the service has %d of its %d containers", len(existing)-removed, len(existing)-removed+created[name])
		case removed > 0 && !defined:
			service.Action, service.Reason = "removed", "the service is no longer in the Compose file"
		case removed > 0:
			service.Action, service.Reason = "removed", fmt.Sprintf("the service has %d containers and needs %d", len(existing), len(existing)-removed)
		case restarted > 0:
			service.Action, service.Reason = "started", "Docker Compose stops and restarts the container because Compose recreates a service or network used by the container"
		case stopped > 0:
			service.Action, service.Reason = "started", "the container is stopped"
		// Compose would keep the containers with the image that the node
		// has now, and whether the run's pull changes that image, the dryrun
		// cannot tell.
		case defined && d.pulled() && unseen[d.Image] != "" && len(existing) > 0:
			service.Action, service.Reason = composeNotChecked, unseen[d.Image]
		}
		services = append(services, service)
	}

	for i := range services {
		service := &services[i]
		if len(recreated[service.Name]) == 0 {
			continue
		}
		d := definition.Services[service.Name]
		image, onNode := node[d.Image]
		switch {
		// Compose's dry run recreates every container whose image the node
		// does not have. That is right when the Compose file names another
		// image than the container's. When it names the same one, a run
		// pulls the image first and keeps the container if the image is the
		// one it was created from.
		case d.pulled() && !onNode && !slices.ContainsFunc(recreated[service.Name], func(container dockerContainer) bool { return container.Config.Image != d.Image }):
			return nil, fmt.Errorf("docker_compose_project: MMX cannot tell whether a run recreates the containers of service %s: the node no longer has their image %s, and Docker Compose decides once the run has pulled it", service.Name, d.Image)
		case d.pulled() && !onNode:
			service.Reason = "the definition differs: the Compose file names the image " + d.Image
		case d.pulled() && slices.ContainsFunc(recreated[service.Name], func(container dockerContainer) bool { return container.Image != image.ID }):
			service.Reason = "the image differs: the container was created from another image than " + d.Image + " names on the node now"
		default:
			var alsoRecreated []string
			for _, other := range slices.Sorted(maps.Keys(d.DependsOn)) {
				if len(recreated[other]) > 0 {
					alsoRecreated = append(alsoRecreated, other)
				}
			}
			service.Reason = c.whyRecreated(recreated[service.Name], alsoRecreated)
		}
	}
	return services, nil
}

// whyRecreated is the reason of a service whose containers Docker Compose
// would recreate while their image stayed. Compose recreates a container
// when its hash of the service's definition differs from the hash that the
// container carries, and, on some versions, together with a service that
// this one depends on; alsoRecreated names those. Compose versions give the
// same definition different hashes, so after an upgrade of Compose every
// container is recreated once; the reason then names the two versions.
func (c composeProject) whyRecreated(containers []dockerContainer, alsoRecreated []string) string {
	reason := "the definition differs"
	if len(alsoRecreated) > 0 {
		reason += ", or Docker Compose recreates the service together with " + strings.Join(alsoRecreated, ", ") + ", which it depends on"
	}
	now := dockerComposeVersionNumber.FindString(strings.TrimPrefix(c.d.composeVersion, "v"))
	for _, container := range containers {
		made := dockerComposeVersionNumber.FindString(strings.TrimPrefix(container.Config.Labels[composeLabelVersion], "v"))
		if made != "" && now != "" && made != now {
			return fmt.Sprintf("%s: Docker Compose %s made the container, and Docker Compose %s, which the node has now, may give the same definition another hash", reason, made, now)
		}
	}
	return reason
}

// declaredVolumes are the named volumes that the definition declares and
// that Compose creates itself: not the external ones.
func (definition composeDefinition) declaredVolumes() []string {
	var declared []string
	for _, volume := range definition.Volumes {
		external := len(volume.External) > 0 && string(volume.External) != "false" && string(volume.External) != "null"
		if !external && volume.Name != "" {
			declared = append(declared, volume.Name)
		}
	}
	slices.Sort(declared)
	return declared
}

// existingVolumes reports which of the volumes the node has, sorted.
func (d *docker) existingVolumes(ctx context.Context, names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	var found []struct{ Name string }
	if err := d.inspected(ctx, &found, append([]string{"volume", "inspect"}, names...)...); err != nil {
		return nil, err
	}
	var existing []string
	for _, volume := range found {
		existing = append(existing, volume.Name)
	}
	slices.Sort(existing)
	return existing, nil
}

// volumesOfProjectName lists the volumes that Compose created for a project
// name: it labels each one with the name. docker compose down --volumes
// without a Compose file deletes these.
func (d *docker) volumesOfProjectName(ctx context.Context, project string) ([]string, error) {
	out, err := d.read(ctx, "volume", "ls", "--quiet", "--filter", "label="+composeLabelProject+"="+project)
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

// nameSharedWithAnotherDirectory stops a task whose project name another
// project directory uses too.
//
// Docker Compose acts on a project by its name, and without
// compose_project_name it names a project after the last part of its
// directory's path: /opt/shop/app and /opt/blog/app are both "app". Compose
// then takes the containers and volumes of both for one project: up
// --remove-orphans in one directory removes the other's containers, and down
// removes both. MMX finds a project's containers by its directory, so it
// sees the mistake before a command runs and changes nothing.
func (c composeProject) nameSharedWithAnotherDirectory(ctx context.Context, name string) *protocol.Result {
	containers, err := c.d.containersWithLabels(ctx, map[string]string{composeLabelProject: name})
	if err != nil {
		failed := protocol.Failed(err)
		return &failed
	}
	for _, container := range containers {
		other := container.Config.Labels[composeLabelProjectDirectory]
		if other == c.p.ComposeProjectDir {
			continue
		}
		why := "the task's compose_project_name"
		if other == "" {
			other = "a directory that its label does not name"
		}
		stop := protocol.ConfigurationError(fmt.Errorf("docker_compose_project: the project of %s and the project of %s have the same project name, %s (%s; the container %s belongs to the second). Docker Compose would treat the containers and volumes of both as one project's. Give each project its own compose_project_name; MMX changed nothing", c.p.ComposeProjectDir, other, name, why, container.name()))
		return &stop
	}
	return nil
}

// dockerComposeProjectEnded stops the project's containers or removes them,
// with or without the named volumes. The project is found by the labels of
// its containers, so a removal that is run again, after the Compose file,
// the directory, or Docker itself is gone, finds nothing and reports
// unchanged.
func (run Runtime) dockerComposeProjectEnded(ctx context.Context, p DockerComposeProject) protocol.Result {
	d := run.findDocker(ctx, "docker_compose_project")
	if d == nil {
		r := composeProjectResult(run, nil, nil)
		r.Reason = "the node has no docker command, so the project has no containers"
		return r
	}
	if stop := d.inspectInstallation(ctx); stop != nil {
		return *stop
	}
	c := composeProject{d: d, p: p}
	before, err := d.containersWithLabels(ctx, c.labels(""))
	if err != nil {
		return protocol.Failed(err)
	}
	removes := p.State != composeProjectStopped
	// Compose reads the project's files for a removal where they exist, it
	// is installed, and it can load them. With them it also removes what
	// they declare and no container holds any more: the named volumes and
	// the networks. A Compose file that cannot be loaded, for example
	// because an environment file that it names is gone already, must not
	// block the removal: the project is then removed by its name alone.
	var definition composeDefinition
	var notLoaded error
	withFiles := removes && c.missingFile() == "" && d.requireCompose() == nil
	if withFiles {
		if definition, notLoaded = c.definition(ctx); notLoaded != nil {
			withFiles = false
		}
	}
	// Each project name that a container of the directory carries. A command
	// with such a name needs no Compose file and reaches every container of
	// the project, also one whose service has left the file.
	var byNames, names []string
	for _, container := range before {
		if name := container.Config.Labels[composeLabelProject]; !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	for _, name := range names {
		if !withFiles || name != definition.Name {
			byNames = append(byNames, name)
		}
	}
	if withFiles && !slices.Contains(names, definition.Name) {
		names = append(names, definition.Name)
	}
	// The named volumes that the removal deletes: those that the files
	// declare and, for a project that is removed by its name, those that
	// carry the name as Compose's label.
	var volumes []string
	if p.State == composeProjectContainersVolumesRemoved {
		if withFiles {
			if volumes, err = d.existingVolumes(ctx, definition.declaredVolumes()); err != nil {
				return protocol.Failed(err)
			}
		}
		for _, name := range byNames {
			labelled, err := d.volumesOfProjectName(ctx, name)
			if err != nil {
				return protocol.Failed(err)
			}
			for _, volume := range labelled {
				if !slices.Contains(volumes, volume) {
					volumes = append(volumes, volume)
				}
			}
		}
		slices.Sort(volumes)
	}
	var planned []composeService
	existing := serviceContainers(before, true)
	for _, name := range slices.Sorted(maps.Keys(existing)) {
		service := composeService{Name: name, Action: "unchanged"}
		switch {
		case removes:
			service.Action, service.Reason = "removed", "the service has containers"
		case slices.ContainsFunc(existing[name], dockerContainer.runs):
			service.Action, service.Reason = "stopped", "the container runs"
		}
		planned = append(planned, service)
	}
	withoutFiles := ""
	if notLoaded != nil {
		withoutFiles = "Docker Compose cannot load the project's files (" + strings.TrimPrefix(notLoaded.Error(), d.module+": ") + ")"
	}
	plan := composeProjectResult(run, planned, nil)
	if plan.Change == "unchanged" && len(volumes) == 0 {
		if withoutFiles != "" && p.State == composeProjectContainersVolumesRemoved {
			plan.Reason = "the project has no containers, and MMX cannot look for the named volumes that its files declare: " + withoutFiles
		}
		return plan
	}
	if stop := d.requireCompose(); stop != nil {
		return *stop
	}
	for _, name := range names {
		if stop := c.nameSharedWithAnotherDirectory(ctx, name); stop != nil {
			return *stop
		}
	}
	if withoutFiles != "" {
		withoutFiles += ", so the project is removed by its name"
	}
	reason := func(volumes string) string {
		return strings.Join(slices.DeleteFunc([]string{volumes, withoutFiles}, func(part string) bool { return part == "" }), "; ")
	}
	if run.Dryrun {
		plan.Change = "would_change"
		if len(volumes) > 0 {
			plan.Reason = reason("would delete the named volumes " + strings.Join(volumes, ", ") + " with the data in them")
		} else {
			plan.Reason = reason("")
		}
		return plan
	}
	if err := run.Mutation("docker_compose_project project " + p.ComposeProjectDir); err != nil {
		return protocol.Failed(err)
	}
	end := []string{"stop"}
	if removes {
		end = []string{"down", "--remove-orphans"}
		if p.State == composeProjectContainersVolumesRemoved {
			end = append(end, "--volumes")
		}
	}
	var failed error
	if withFiles {
		_, failed = d.command(ctx, nil, c.withFiles(end...)...)
	}
	for _, name := range byNames {
		if failed != nil {
			break
		}
		_, failed = d.command(ctx, nil, byName(name, end...)...)
	}
	after, err := d.containersWithLabels(ctx, c.labels(""))
	if err != nil {
		return protocol.Failed(errors.Join(failed, err))
	}
	r := composeProjectResult(run, composeServiceActions(existing, serviceContainers(after, true), nil), nil)
	deleted := ""
	if len(volumes) > 0 {
		left, err := d.existingVolumes(ctx, volumes)
		if err != nil {
			return protocol.Failed(errors.Join(failed, err))
		}
		gone := slices.DeleteFunc(slices.Clone(volumes), func(volume string) bool { return slices.Contains(left, volume) })
		if len(gone) > 0 {
			r.Change, deleted = "changed", "deleted the named volumes "+strings.Join(gone, ", ")+" with the data in them"
		}
	}
	if r.Change == "changed" {
		r.Reason = reason(deleted)
	}
	if failed != nil {
		failure := protocol.Failed(failed)
		if r.Change == "changed" {
			failure.Change, failure.Value = "changed", r.Value
		}
		return failure
	}
	return r
}
