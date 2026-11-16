package modules

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

// What the Docker modules and the Podman modules share: the values of
// pull_image, the parts of an image reference, a task's login for a registry
// and the text of the login file that both tools read, what a result of
// docker_image or podman_image says about the image for a service that
// follows it, and the choice and the result of docker_unused_images and
// podman_unused_images.

// pullImageChoices are the values of pull_image and pull_images.
var pullImageChoices = []string{"when_missing", "always"}

// imageReference is the parts of an image reference such as
// registry.example.com:5000/shop/web:2.4 or
// registry.example.com/shop/web@sha256:<digest>.
type imageReference struct {
	// Registry is the registry's host, with its port when the reference has
	// one. It is empty when the reference names no registry, as nginx:1.27.
	Registry string
	// Repository is the image's name inside the registry, such as shop/web.
	Repository string
	// Tag and Digest are empty when the reference has none.
	Tag, Digest string
}

var (
	imageRegistry   = regexp.MustCompile(`^(\[[0-9a-fA-F:]+\]|[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?)(:[0-9]+)?$`)
	imageRepository = regexp.MustCompile(`^[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*(/[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*)*$`)
	imageTag        = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	imageDigest     = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*([+._-][A-Za-z][A-Za-z0-9]*)*:[0-9a-fA-F]{32,}$`)
)

// parseImageReference splits a reference by the rule docker and podman use:
// the part before the first "/" is a registry when it has a "." or a ":" or
// is "localhost".
func parseImageReference(text string) (imageReference, error) {
	var reference imageReference
	rest := text
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		reference.Digest, rest = rest[at+1:], rest[:at]
		if !imageDigest.MatchString(reference.Digest) {
			return reference, fmt.Errorf("%q is not an image reference: %q is not a digest such as sha256: followed by 64 hexadecimal digits", text, reference.Digest)
		}
	}
	if colon := strings.LastIndex(rest, ":"); colon > strings.LastIndex(rest, "/") {
		reference.Tag, rest = rest[colon+1:], rest[:colon]
		if !imageTag.MatchString(reference.Tag) {
			return reference, fmt.Errorf("%q is not an image reference: %q is not a tag", text, reference.Tag)
		}
	}
	if first, remainder, found := strings.Cut(rest, "/"); found && (strings.ContainsAny(first, ".:") || first == "localhost") {
		reference.Registry, rest = first, remainder
		if !imageRegistry.MatchString(reference.Registry) {
			return reference, fmt.Errorf("%q is not an image reference: %q is not a registry's host name with an optional port", text, reference.Registry)
		}
	}
	reference.Repository = rest
	if !imageRepository.MatchString(rest) {
		return reference, fmt.Errorf(`%q is not an image reference: the image name %q must be lowercase letters and digits, with ".", "_", or "-" between them, in parts separated by "/"`, text, rest)
	}
	return reference, nil
}

// checkImageReference parses a reference that a task pulls. It must name a
// tag or a digest, so that the task says which image it means, and with
// registryRequired also the registry, because podman would have to guess it.
func checkImageReference(text string, registryRequired bool) (imageReference, error) {
	reference, err := parseImageReference(text)
	if err != nil {
		return reference, err
	}
	if reference.Tag == "" && reference.Digest == "" {
		return reference, fmt.Errorf("%q names no tag and no digest; write the tag, such as %s:2.4, or a digest", text, text)
	}
	if registryRequired && reference.Registry == "" {
		return reference, fmt.Errorf("%q names no registry; write the full reference, such as docker.io/library/%s", text, text)
	}
	return reference, nil
}

// registryLogin is one login for a registry.
type registryLogin struct {
	Registry, Username, Password string
}

// resolveRegistryLogin resolves the user name and the password that a task
// writes for registry. inputs names where the task writes the two, such as
// docker_image or docker_compose_project.registry_logins[0], for an error.
//
// The two are logins that MMX hands only to a tool: a secret in them does
// not hide the task's result, and MMX replaces them in what the task
// reports. A tool that fails may print a login in another form than its
// plain text. So for a login with a secret, the scope also learns the forms
// in which docker and podman get, keep, and send it: the text of the login
// file, the header for Docker's daemon, and the user name and the password
// as an address holds them.
func resolveRegistryLogin(s value.Scope, registry, inputs, username, password string) (registryLogin, error) {
	login := registryLogin{Registry: registry}
	secret := false
	for _, part := range []struct {
		field, written string
		text           *string
	}{{"registry_username", username, &login.Username}, {"registry_password", password, &login.Password}} {
		resolved, err := s.LoginHandedOnlyToATool(part.written)
		if err == nil {
			*part.text, err = value.Scalar(resolved)
		}
		if err != nil {
			return login, fmt.Errorf("%s.%s: %w", inputs, part.field, err)
		}
		if value.HasSecrets(resolved) {
			secret = true
			// docker and podman put the user name into the address of a
			// registry's token service (account=...), and their errors
			// quote that address. The same encoding carries the password
			// in a request for a token.
			s.FormOfLoginHandedOnlyToATool(url.QueryEscape(*part.text))
		}
	}
	if secret {
		s.FormOfLoginHandedOnlyToATool(login.textInLoginFile())
		s.FormOfLoginHandedOnlyToATool(login.headerForDockerDaemon())
	}
	return login, nil
}

// textInLoginFile is the login as the login file of docker and podman holds
// it: the Base64 text of the user name, a colon, and the password. It is
// also what both tools send a registry for HTTP basic authentication.
func (login registryLogin) textInLoginFile() string {
	return base64.StdEncoding.EncodeToString([]byte(login.Username + ":" + login.Password))
}

// headerForDockerDaemon is the login as the X-Registry-Auth header of a
// request to Docker's daemon holds it.
func (login registryLogin) headerForDockerDaemon() string {
	header, _ := json.Marshal(map[string]string{"username": login.Username, "password": login.Password, "serveraddress": login.Registry})
	return base64.URLEncoding.EncodeToString(header)
}

// registryLoginFile is the text of a file from which docker (config.json in
// the directory DOCKER_CONFIG names) and podman (--authfile) read logins.
// It holds the passwords, so its place must be private to the task.
func registryLoginFile(logins []registryLogin) []byte {
	auths := map[string]map[string]string{}
	for _, login := range logins {
		auths[login.Registry] = map[string]string{"auth": login.textInLoginFile()}
	}
	text, _ := json.Marshal(map[string]any{"auths": auths})
	return text
}

// imageStateAfter is the StateAfter of docker_image and podman_image, whose
// results name the image in the same two fields, value.image_id and
// value.previous_image_id. It is what a dryrun predicts for the image: the
// ID its result names, and a change when the task would pull an image whose
// ID it cannot know, or could not check an image that the node does not
// have. When the node has the image and the dryrun cannot see whether a pull
// would bring another, the result is not checked and names the image that
// the node has. Then nothing is predicted: neither a change nor a restart
// that follows it, because a dryrun never claims what a run may not do.
func imageStateAfter(r protocol.Result) (string, bool) {
	v, _ := r.Value.(map[string]any)
	if r.Status == "not_checked" {
		if _, has := v["previous_image_id"].(string); has {
			return "", false
		}
		return stateWouldChange, true
	}
	if r.Status != "succeeded" {
		return "", false
	}
	if id, known := v["image_id"].(string); known && id != "" {
		return id, true
	}
	if r.Change == "would_change" {
		return stateWouldChange, true
	}
	return "", false
}

// imageUnknownInADryrun lists the result fields that a dryrun of
// docker_image or podman_image cannot know: the ID of an image it would
// pull, where the tool tells it only after the pull.
func imageUnknownInADryrun(r protocol.Result) map[string]string {
	v, _ := r.Value.(map[string]any)
	if r.Status != "succeeded" || r.Change != "would_change" || v == nil || v["image_id"] != nil {
		return nil
	}
	return map[string]string{"value": "would pull the image, whose ID is not known in this dryrun"}
}

// unusedImageChoices are what docker_unused_images and podman_unused_images
// remove: images without a name, or every image, that no container uses.
var unusedImageChoices = []string{"dangling_images_only", "every_image_no_container_uses"}

// checkUnusedImageChoice is the check of the remove input of both modules.
func checkUnusedImageChoice(module, remove string) error {
	if !slices.Contains(unusedImageChoices, remove) {
		return invalid("remove", module+".remove is required and must be dangling_images_only or every_image_no_container_uses; there is no default, because every_image_no_container_uses also removes an image kept for a rollback")
	}
	return nil
}

// unusedImage is one image that an unused-images task removed, or would
// remove in a dryrun.
type unusedImage struct {
	// ID is the image ID as the tool reports it.
	ID string
	// Names are the image's references, such as registry.example.com/shop/web:2.3;
	// a dangling image has none.
	Names []string
	// Bytes is the image's size as the tool reports it: all of its layers,
	// also those that it shares with other images.
	Bytes int64
}

// unusedImagesResult reports the images a task removed, or would remove in
// a dryrun, from where, and the bytes freed; freed is nil when they are
// not known.
func unusedImagesResult(dryrun bool, where string, images []unusedImage, freed *int64) protocol.Result {
	removed := make([]any, 0, len(images))
	var shown []string
	for _, image := range images {
		names := make([]any, 0, len(image.Names))
		for _, name := range image.Names {
			names = append(names, name)
		}
		removed = append(removed, map[string]any{"image_id": image.ID, "names": names, "size_bytes": image.Bytes})
		if len(shown) < 5 {
			name := image.ID
			if len(image.Names) > 0 {
				name = strings.Join(image.Names, ", ")
			}
			shown = append(shown, name)
		}
	}
	if len(images) > len(shown) {
		shown = append(shown, fmt.Sprintf("and %d more", len(images)-len(shown)))
	}
	r := protocol.Success("unchanged")
	var freedValue any
	if freed != nil {
		freedValue = *freed
	}
	r.Value = map[string]any{"removed_images": removed, "freed_bytes": freedValue}
	if len(images) == 0 {
		r.Reason = where + " has no image that the task removes"
		return r
	}
	verb, freeing := "removed", ""
	if dryrun {
		verb = "would remove"
	}
	if freed != nil {
		freeing = " and free " + bytesText(*freed)
		if !dryrun {
			freeing = " and freed " + bytesText(*freed)
		}
	}
	r.Change = "changed"
	if dryrun {
		r.Change = "would_change"
	}
	r.Reason = fmt.Sprintf("%s %d images from %s%s: %s", verb, len(images), where, freeing, strings.Join(shown, "; "))
	return r
}

// unusedImagesUnknownInADryrun is what a dryrun of docker_unused_images or
// podman_unused_images cannot know: the bytes a removal would free, when
// its value.freed_bytes is null.
func unusedImagesUnknownInADryrun(r protocol.Result) map[string]string {
	v, _ := r.Value.(map[string]any)
	if r.Status != "succeeded" || r.Change != "would_change" || v == nil || v["freed_bytes"] != nil {
		return nil
	}
	return map[string]string{"value": "would remove images; the number of bytes freed is known only after image removal"}
}
