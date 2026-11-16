package modules

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

// DockerImage is one Docker image on the node, pulled from a registry. The
// two login fields are private values that the module hands only to docker
// as a login (resolveRegistryLogin).
type DockerImage struct {
	Image            string `yaml:"image" json:"image"`
	PullImage        string `yaml:"pull_image" json:"pull_image"`
	RegistryUsername string `yaml:"registry_username,omitempty" json:"registry_username,omitempty"`
	RegistryPassword string `yaml:"registry_password,omitempty" json:"registry_password,omitempty"`
}

// dockerImageDescription is what mmx describe-module shows for docker_image.
var dockerImageDescription = Description{
	Name: "docker_image", Topic: TopicContainers, Schema: reflect.TypeOf(DockerImage{}),
	Summary: "one Docker image on the node, pulled from a registry; never removes an image",
	Fields: []Field{
		{Name: "image", Required: true, Notes: "the image reference with a tag or a digest, such as registry.example.com/audio/converter:3.1; a reference without either is rejected"},
		{Name: "pull_image", Required: true, Values: pullImageChoices, Notes: "no default; when_missing pulls only when the node does not have the image, so a rerun never follows a tag that moved; always asks the registry on every run which image the reference names now and pulls it when it differs from the node's"},
		{Name: "registry_username", Notes: "with registry_password: the login for the registry named in image; a private value"},
		{Name: "registry_password", Notes: "with registry_username; a private value, normally a secret; it reaches docker through a private directory under /run that the task removes, never through a command line or an account's home directory; a secret in the login does not hide the task's result: MMX shows [redacted] wherever a text of the result holds the secret"},
	},
	Example: "- docker_image:\n    image: registry.example.com/audio/converter:3.1\n    pull_image: when_missing\n",
	Results: []string{"status", "change", "reason", "value.image_id", "value.previous_image_id"},
	Dryrun:  "pulls nothing and writes no login; reports would_change for an image the node does not have; with pull_image: always it asks the registry which image the reference names now without downloading it, with the task's own login only, and reports would_change with both image IDs and, in reason, the digest under which the registry serves the image when that differs from the node's, or not_checked with the error when the registry does not answer, and then predicts no restart of a service that follows an image the node has; when the reference names a list of images for several platforms, the registry serves another list, and the node's Docker uses the classic image store, it reports not_checked and predicts no restart of a service that follows the image, because it cannot see whether the image for the node's platform changed; value.image_id is null where it is known only after the pull: on a node whose Docker uses the classic image store, and for a missing image with when_missing whose reference has a tag; not_checked while the docker command or the Docker daemon is missing",
}

// Prepare checks the inputs when the project loads.
func (i *DockerImage) Prepare(base string) error {
	switch {
	case i.PullImage == "":
		return invalid("pull_image", "docker_image needs pull_image: when_missing or always; there is no default, because pull_image decides whether a rerun deploys a newer image")
	case !slices.Contains(pullImageChoices, i.PullImage):
		return invalid("pull_image", "pull_image must be "+strings.Join(pullImageChoices, " or "))
	case (i.RegistryUsername == "") != (i.RegistryPassword == ""):
		return invalid("registry_username", "set registry_username and registry_password together, or omit registry_username and registry_password")
	}
	return nil
}

// dockerImageReference is the rule for a reference a Docker task pulls: it
// names a tag or a digest. A reference without a registry names Docker Hub.
var dockerImageReference = parsed(func(s string) error {
	_, err := checkImageReference(s, false)
	return err
})

// Arguments lists the inputs that may hold a reference, with their rules.
func (i *DockerImage) Arguments(arguments *ArgumentList) {
	arguments.Public("image", &i.Image, dockerImageReference)
	for _, login := range []struct{ field, raw string }{{"registry_username", i.RegistryUsername}, {"registry_password", i.RegistryPassword}} {
		if login.raw != "" {
			arguments.LoginHandedOnlyToATool(login.field, login.raw, scalar)
		}
	}
}

// Run does the module's work on the node.
func (i *DockerImage) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.Runtime.DockerImage(ctx, *i, task.Scope)
}

// TrackedInputs lists the image, which counts by its image ID.
func (i *DockerImage) TrackedInputs() []TrackedInput {
	return []TrackedInput{{Kind: "installed", Path: "docker_image " + i.Image, State: i}}
}

// ReadState is the ID of the image the reference names on the node now, ""
// when the node does not have it or has no Docker yet.
func (i *DockerImage) ReadState(ctx context.Context, _ value.Scope) (string, error) {
	d := Runtime{}.findDocker(ctx, "docker_image")
	if d == nil {
		return "", nil
	}
	image, _, err := d.image(ctx, i.Image)
	if err != nil {
		return "", nil // no daemon yet, so no image
	}
	return image.ID, nil
}

// StateAfter is the image ID a result says the node has, or would have in a
// dryrun (imageStateAfter). A dryrun cannot see whether a pull would change
// the node's image when the registry does not answer, or serves another list
// of images in the classic image store.
func (i *DockerImage) StateAfter(r protocol.Result) (string, bool) { return imageStateAfter(r) }

// UnknownInADryrun lists the result fields a dryrun cannot know: the ID of
// an image it would pull, where Docker tells it only after the pull.
func (i *DockerImage) UnknownInADryrun(r protocol.Result) map[string]string {
	return imageUnknownInADryrun(r)
}

// logins resolves the task's login for the registry of its image.
func (i DockerImage) logins(s value.Scope) ([]registryLogin, error) {
	if i.RegistryUsername == "" {
		return nil, nil
	}
	reference, err := parseImageReference(i.Image)
	if err != nil {
		return nil, err
	}
	login, err := resolveRegistryLogin(s, dockerLoginName(reference.Registry), "docker_image", i.RegistryUsername, i.RegistryPassword)
	if err != nil {
		return nil, err
	}
	return []registryLogin{login}, nil
}

// DockerImage pulls the image when the node does not have it or, with
// pull_image: always, when the registry names another image now. It
// compares the image IDs that docker reports before and after.
func (run Runtime) DockerImage(ctx context.Context, i DockerImage, s value.Scope) protocol.Result {
	d, stop := run.newDocker(ctx, "docker_image", false)
	if stop != nil {
		return *stop
	}
	logins, err := i.logins(s)
	if err != nil {
		return protocol.Failed(err)
	}
	before, had, err := d.image(ctx, i.Image)
	if err != nil {
		return protocol.Failed(err)
	}
	var previous any // null when the node did not have the image
	if had {
		previous = before.ID
	}
	result := func(change string, id any) protocol.Result {
		r := protocol.Success(change)
		r.Value = map[string]any{"image_id": id, "previous_image_id": previous}
		return r
	}
	// A digest names one image forever, so the registry cannot move it.
	reference, _ := parseImageReference(i.Image)
	if had && (i.PullImage == "when_missing" || reference.Digest != "") {
		return result("unchanged", before.ID)
	}
	if run.Dryrun {
		// In the containerd image store the image ID is the digest under
		// which the registry serves the image. In the classic store it is
		// the digest of the image's configuration, which docker learns with
		// the pull.
		if i.PullImage == "when_missing" || reference.Digest != "" {
			var id any
			//nolint:nilaway // The preceding stop check excludes every nil return from newDocker.
			if reference.Digest != "" && d.containerdImageStore {
				id = reference.Digest
			}
			r := result("would_change", id)
			r.Reason = "the node does not have the image " + i.Image
			return r
		}
		served, err := d.registryDigest(ctx, i.Image, loginFor(logins, i.Image))
		if err != nil {
			// The result names the image that the node has, so that nothing
			// is predicted for a service that follows it.
			r := result("unknown", nil)
			r.Status, r.Reason = "not_checked", err.Error()
			return r
		}
		digest := served.Digest
		if had && before.came(digest) {
			return result("unchanged", before.ID)
		}
		// The registry serves another list of images than the node's image
		// came from. In the classic image store the image ID is that of the
		// image for the node's platform, which may be the same in the new
		// list: a pull then changes nothing. The registry's answer does not
		// say, so a dryrun does not claim a change, and it predicts no
		// restart of a service that follows the image. In the containerd
		// image store the ID is the list's digest, which a pull does change.
		//nolint:nilaway // The preceding stop check guarantees that d exists.
		if had && served.ListOfImages && !d.containerdImageStore {
			r := result("unknown", nil)
			r.Status = "not_checked"
			r.Reason = fmt.Sprintf("docker_image: the registry now serves another list of images for %s (digest %s); a dryrun cannot tell whether the image for the node's platform changed: a run pulls the image, and the image on the node changes only if the platform's image changed", i.Image, digest)
			return r
		}
		var id any
		//nolint:nilaway // The preceding stop check guarantees that d exists.
		if d.containerdImageStore {
			id = digest
		}
		r := result("would_change", id)
		r.Reason = fmt.Sprintf("the registry now serves %s under the digest %s", i.Image, digest)
		if !had {
			r.Reason += ", and the node does not have the image"
		}
		return r
	}
	// Only a run pulls, so only a run writes the task's login for docker.
	cleanup, err := d.useLogins(logins)
	if err != nil {
		return protocol.Failed(fmt.Errorf("docker_image: the private directory for the registry login: %w", err))
	}
	defer cleanup()
	pulled := d.change(ctx, "image "+i.Image, "pull", "--quiet", i.Image)
	after, has, err := d.image(ctx, i.Image)
	if err != nil {
		r := protocol.Failed(errors.Join(pulled, err))
		return r
	}
	if !has && pulled == nil {
		pulled = fmt.Errorf("docker_image: docker pull %s succeeded, but Docker still reports no image under the requested reference", i.Image)
	}
	change := "unchanged"
	if has && (!had || after.ID != before.ID) {
		change = "changed"
	}
	if pulled != nil {
		r := protocol.Failed(pulled)
		r.Change = change
		return r
	}
	return result(change, after.ID)
}
