package modules

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"

	"mmx/internal/protocol"
)

// DockerUnusedImages removes the images that no container uses, as docker
// image prune does: those without a name, or every one.
type DockerUnusedImages struct {
	Remove string `yaml:"remove" json:"remove"`
}

// dockerUnusedImagesDescription is what mmx describe-module shows for
// docker_unused_images.
var dockerUnusedImagesDescription = Description{
	Name: "docker_unused_images", Topic: TopicContainers, Schema: reflect.TypeOf(DockerUnusedImages{}),
	Summary: "removes the Docker images that no container, running or stopped, uses, as docker image prune does: only those without a name, or every one; value has removed_images and freed_bytes",
	Fields: []Field{
		{Name: "remove", Required: true, Values: unusedImageChoices, Notes: "no default: dangling_images_only removes the images that have no name left, such as the one a pull of a moved tag replaced (docker image prune); every_image_no_container_uses also removes named images that no container uses (docker image prune --all), such as a previous version that a deploy keeps for a quick rollback"},
	},
	Example: "- docker_unused_images:\n    remove: dangling_images_only\n",
	Results: []string{"status", "change", "reason", "value.removed_images", "value.freed_bytes"},
	Dryrun:  "removes nothing; lists the images that a run would remove, each with its image_id, names, and size_bytes, as docker image inspect reports them; freed_bytes is null, because only the Docker daemon's removal says what it frees; unchanged when no image qualifies; not_checked while the docker command or the Docker daemon is missing",
}

// Prepare checks the inputs when the project loads.
func (i *DockerUnusedImages) Prepare(base string) error {
	return checkUnusedImageChoice("docker_unused_images", i.Remove)
}

// Run does the module's work on the node.
func (i *DockerUnusedImages) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.Runtime.DockerUnusedImages(ctx, *i)
}

// UnknownInADryrun lists the result fields a dryrun cannot know: the bytes
// that a removal frees, which only the daemon's removal says.
func (i *DockerUnusedImages) UnknownInADryrun(r protocol.Result) map[string]string {
	return unusedImagesUnknownInADryrun(r)
}

// dockerInspectedImage is what MMX reads of docker image inspect.
type dockerInspectedImage struct {
	ID       string `json:"Id"`
	RepoTags []string
	Size     int64
}

// DockerUnusedImages lists the images that docker image prune removes and,
// in a run, removes them with the request that docker image prune sends to
// the daemon, whose answer says what it removed and freed.
func (run Runtime) DockerUnusedImages(ctx context.Context, i DockerUnusedImages) protocol.Result {
	d, stop := run.newDocker(ctx, "docker_unused_images", false)
	if stop != nil {
		return *stop
	}
	removable, err := d.unusedImages(ctx, i.Remove == "every_image_no_container_uses")
	if err != nil {
		return protocol.Failed(err)
	}
	const where = "the node's Docker images"
	if len(removable) == 0 || run.Dryrun {
		return unusedImagesResult(run.Dryrun, where, dockerUnusedImageList(removable, nil), nil)
	}
	if err := run.Mutation("unused images"); err != nil {
		return protocol.Failed(err)
	}
	answer, err := d.pruneImages(ctx, i.Remove == "every_image_no_container_uses")
	if err != nil {
		// The daemon refused before it removed anything, or its answer did
		// not come.
		r := protocol.Failed(err)
		r.Change = "unknown"
		return r
	}
	// What the node lost decides which listed images the removal removed:
	// the daemon's answer names deleted layers and manifests too, in a form
	// that depends on the image store.
	left, err := d.read(ctx, "image", "ls", "--all", "--quiet", "--no-trunc")
	if err != nil {
		r := protocol.Failed(err)
		r.Change = "changed"
		return r
	}
	freed := answer.SpaceReclaimed
	removed := dockerUnusedImageList(removable, strings.Fields(left))
	r := unusedImagesResult(false, where, removed, &freed)
	if len(removed) == 0 && len(answer.ImagesDeleted) > 0 {
		// Names went, or images that the listing did not name, such as the
		// parent of an image that a build with Docker's former builder made.
		r.Change = "changed"
		r.Reason = fmt.Sprintf("removed %d names or images that the listing did not name, and freed %s", len(answer.ImagesDeleted), bytesText(freed))
	}
	return r
}

// dockerUnusedImageList is the images in the form of a result: those of
// listed whose IDs are not in left, the IDs that the node has after the
// removal, or all of listed when left is nil.
func dockerUnusedImageList(listed []dockerInspectedImage, left []string) []unusedImage {
	var list []unusedImage
	for _, image := range listed {
		if left == nil || !slices.Contains(left, image.ID) {
			list = append(list, unusedImage{ID: image.ID, Names: image.RepoTags, Bytes: image.Size})
		}
	}
	return list
}

// unusedImages lists, as docker image inspect reports them, the images
// that docker image prune removes: those without a name, or with every
// every one, that no container uses, running or stopped.
func (d *docker) unusedImages(ctx context.Context, every bool) ([]dockerInspectedImage, error) {
	args := []string{"image", "ls", "--quiet", "--no-trunc"}
	if !every {
		args = append(args, "--filter", "dangling=true")
	}
	out, err := d.read(ctx, args...)
	if err != nil {
		return nil, err
	}
	containers, err := d.read(ctx, "ps", "--all", "--quiet", "--no-trunc")
	if err != nil {
		return nil, err
	}
	used, usedNames := map[string]bool{}, map[string]bool{}
	if ids := strings.Fields(containers); len(ids) > 0 {
		var list []dockerContainer
		// A container that was removed since the listing is left out.
		if err := d.inspected(ctx, &list, append([]string{"container", "inspect"}, ids...)...); err != nil {
			return nil, err
		}
		for _, container := range list {
			used[container.Image] = true
			usedNames[familiarImageName(container.Config.Image)] = true
		}
	}
	var ids []string
	for _, id := range strings.Fields(out) {
		if !used[id] && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	var images []dockerInspectedImage
	if err := d.inspected(ctx, &images, append([]string{"image", "inspect"}, ids...)...); err != nil {
		return nil, err
	}
	images = slices.DeleteFunc(images, func(image dockerInspectedImage) bool {
		// The containerd image store lists an image without a name as
		// dangling also while other records give the same image names;
		// prune then removes only that record, and the image stays.
		if !every && len(image.RepoTags) > 0 {
			return true
		}
		// The containerd image store also keeps the image that the name
		// of a container's image names now, when a newer image took the
		// name since the container was created.
		return d.containerdImageStore && slices.ContainsFunc(image.RepoTags, func(name string) bool { return usedNames[familiarImageName(name)] })
	})
	return images, nil
}

// familiarImageName is an image's name as docker image inspect writes it
// in RepoTags, from a name as a container was created with it: with its
// tag, latest when it names none, and without Docker Hub's own registry.
func familiarImageName(name string) string {
	if !strings.Contains(name[strings.LastIndex(name, "/")+1:], ":") && !strings.Contains(name, "@") {
		name += ":latest"
	}
	for _, hub := range []string{"docker.io/library/", "index.docker.io/library/", "docker.io/", "index.docker.io/"} {
		if rest, found := strings.CutPrefix(name, hub); found {
			return rest
		}
	}
	return name
}

// dockerPruneAnswer is the daemon's answer to a removal of unused images:
// each name it removed and each image it deleted, and the bytes freed.
type dockerPruneAnswer struct {
	ImagesDeleted []struct {
		Untagged, Deleted string
	}
	SpaceReclaimed int64
}

// pruneImages sends the daemon the request that docker image prune sends:
// unused images without a name, or with every, every one.
func (d *docker) pruneImages(ctx context.Context, every bool) (dockerPruneAnswer, error) {
	client, err := d.daemonClient(ctx, "remove unused images")
	if err != nil {
		return dockerPruneAnswer{}, err
	}
	defer client.CloseIdleConnections()
	filters, _ := json.Marshal(map[string][]string{"dangling": {fmt.Sprint(!every)}})
	address := url.URL{Scheme: "http", Host: "docker", Path: "/images/prune", RawQuery: url.Values{"filters": {string(filters)}}.Encode()}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, address.String(), nil)
	if err != nil {
		return dockerPruneAnswer{}, err
	}
	response, err := client.Do(request)
	if err != nil {
		return dockerPruneAnswer{}, fmt.Errorf("%s: asking the Docker daemon to remove unused images: %w", d.module, err) //nolint:nilaway // DockerUnusedImages returns on newDocker's non-nil stop before calling pruneImages.
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, outputLimit))
	if err != nil {
		return dockerPruneAnswer{}, fmt.Errorf("%s: the Docker daemon's answer to the removal of unused images: %w", d.module, err) //nolint:nilaway // DockerUnusedImages returns on newDocker's non-nil stop before calling pruneImages.
	}
	if response.StatusCode != http.StatusOK {
		var refused struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(body, &refused)
		if refused.Message == "" {
			refused.Message = fmt.Sprintf("HTTP status %d", response.StatusCode)
		}
		return dockerPruneAnswer{}, fmt.Errorf("%s: the Docker daemon refused to remove unused images: %s", d.module, refused.Message) //nolint:nilaway // DockerUnusedImages returns on newDocker's non-nil stop before calling pruneImages.
	}
	var answer dockerPruneAnswer
	if err := json.Unmarshal(body, &answer); err != nil {
		return dockerPruneAnswer{}, fmt.Errorf("%s: the Docker daemon's answer to the removal of unused images is not JSON: %w", d.module, err) //nolint:nilaway // DockerUnusedImages returns on newDocker's non-nil stop before calling pruneImages.
	}
	return answer, nil
}
