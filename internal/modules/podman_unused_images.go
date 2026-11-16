package modules

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"

	"mmx/internal/protocol"
)

// PodmanUnusedImages removes the images of a Podman image store that no
// container uses, as podman image prune does: those without a name, or
// every one.
type PodmanUnusedImages struct {
	Remove                    string `yaml:"remove" json:"remove"`
	InImageStoreOfUserAccount string `yaml:"in_image_store_of_user_account,omitempty" json:"in_image_store_of_user_account,omitempty"`
}

// podmanUnusedImagesDescription is what mmx describe-module shows for
// podman_unused_images.
var podmanUnusedImagesDescription = Description{
	Name: "podman_unused_images", Topic: TopicContainers, Schema: reflect.TypeOf(PodmanUnusedImages{}),
	Summary: "removes the images of a Podman image store that no container, running or stopped, uses, with podman image prune: only those without a name, or every one; value has removed_images and freed_bytes",
	Fields: []Field{
		{Name: "remove", Required: true, Values: unusedImageChoices, Notes: "no default: dangling_images_only removes the images that have no name left, such as the one a pull of a moved tag replaced (podman image prune); every_image_no_container_uses also removes named images that no container uses (podman image prune --all), such as a previous version that a deploy keeps for a quick rollback"},
		{Name: "in_image_store_of_user_account", Notes: "an account other than root: the task removes images from that account's own image store, as podman_image pulls into it, and MMX turns on lingering for the account, which Podman needs to run as the account; without it, the system's (root's) store"},
	},
	Example: "- podman_unused_images:\n    remove: dangling_images_only\n",
	Results: []string{"status", "change", "reason", "value.removed_images", "value.freed_bytes"},
	Dryrun:  "removes nothing; lists the images that a run would remove, each with its image_id, names, and size_bytes, as podman images lists them, and the bytes their layers free; unchanged when no image qualifies; not_checked while podman or the account is missing",
}

// Prepare checks the inputs when the project loads.
func (p *PodmanUnusedImages) Prepare(base string) error {
	return checkUnusedImageChoice("podman_unused_images", p.Remove)
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (p *PodmanUnusedImages) Arguments(arguments *ArgumentList) {
	if p.InImageStoreOfUserAccount != "" {
		arguments.Public("in_image_store_of_user_account", &p.InImageStoreOfUserAccount, ordinaryContainerAccount)
	}
}

// Run does the module's work on the node.
func (p *PodmanUnusedImages) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.Runtime.PodmanUnusedImages(ctx, *p)
}

// UnknownInADryrun lists the result fields a dryrun cannot know: the bytes
// that a removal frees, where podman's storage driver does not show the
// layers.
func (p *PodmanUnusedImages) UnknownInADryrun(r protocol.Result) map[string]string {
	return unusedImagesUnknownInADryrun(r)
}

// PodmanUnusedImages finds podman and the account's store as podman_image
// does, then removes the unused images.
func (run Runtime) PodmanUnusedImages(ctx context.Context, p PodmanUnusedImages) protocol.Result {
	tool, stop := run.podman(ctx, "podman_unused_images", "in_image_store_of_user_account", p.InImageStoreOfUserAccount)
	if stop != nil {
		return *stop
	}
	//nolint:nilaway // The preceding stop check excludes every nil return from podman.
	if stop := run.checkContainerAccount("podman_unused_images", tool.manager); stop != nil {
		return *stop
	}
	reportLingering, stop := run.keepAccountServicesRunning(ctx, tool.manager) //nolint:nilaway // The preceding constructor failure check guarantees that tool exists.
	if stop != nil {
		return *stop
	}
	var result protocol.Result
	switch {
	//nolint:nilaway // The preceding constructor failure check guarantees that tool exists.
	case tool.manager.Running():
		result = run.podmanUnusedImages(ctx, tool, p)
	//nolint:nilaway // The preceding constructor failure check guarantees that tool exists.
	case run.Dryrun && !tool.manager.Lingering():
		result = protocol.Success("unchanged")
		result.Value = map[string]any{"removed_images": []any{}, "freed_bytes": nil}
		reportLingering(&result)
		result.Reason += "; the account's image store can be read only after that"
		return result
	default:
		result = run.notYet(fmt.Errorf("podman_unused_images: the systemd of account %s does not run, so the account has no runtime directory %s for Podman; start it with systemctl start user@%d.service", tool.manager.Account, tool.manager.runtimeDirectory(), tool.manager.UID)) //nolint:nilaway // The preceding constructor failure check guarantees that tool exists.
	}
	reportLingering(&result)
	return result
}

// podmanListedImage is what podman images --format json says of one image;
// its ID is the 64 hexadecimal digits that podman prints.
type podmanListedImage struct {
	ID    string `json:"Id"`
	Names []string
	Size  int64
}

// podmanUnusedImageList is the images in the form of a result.
func podmanUnusedImageList(images []podmanListedImage) []unusedImage {
	var list []unusedImage
	for _, image := range images {
		list = append(list, unusedImage{ID: imageID(image.ID), Names: image.Names, Bytes: image.Size})
	}
	return list
}

// podmanInspectedImage is what MMX reads of podman image inspect: where the
// image's layers are.
type podmanInspectedImage struct {
	ID          string `json:"Id"`
	GraphDriver struct {
		Name string
		Data map[string]string
	}
}

// podmanUnusedImages lists the images that podman image prune removes, with
// the filters that it applies itself, and in a run removes them with it.
// What a run removed and freed comes from the store before and after, not
// from what prune prints.
func (run Runtime) podmanUnusedImages(ctx context.Context, tool *podmanTool, p PodmanUnusedImages) protocol.Result {
	where := tool.where("image store")
	every, err := tool.listImages(ctx)
	if err != nil {
		return protocol.Failed(err)
	}
	filters := []string{"--filter", "containers=false", "--filter", "readonly=false"}
	if p.Remove == "dangling_images_only" {
		filters = append(filters, "--filter", "dangling=true")
	}
	removable, err := tool.listImages(ctx, filters...)
	if err != nil {
		return protocol.Failed(err)
	}
	if len(removable) == 0 {
		return unusedImagesResult(run.Dryrun, where, nil, nil)
	}
	layers, err := tool.layersFreedByRemoving(ctx, every, removable)
	if err != nil {
		return protocol.Failed(err)
	}
	if run.Dryrun {
		return unusedImagesResult(true, where, podmanUnusedImageList(removable), layers.total(nil))
	}
	if err := run.Mutation("unused images of " + where); err != nil {
		return protocol.Failed(err)
	}
	prune := []string{"image", "prune", "--force"}
	if p.Remove == "every_image_no_container_uses" {
		prune = append(prune, "--all")
	}
	pruned := tool.command(ctx, prune...)
	after, err := tool.listImages(ctx)
	if err != nil {
		r := protocol.Failed(err)
		r.Change = "unknown"
		return r
	}
	var removed []podmanListedImage
	for _, image := range every {
		if !slices.ContainsFunc(after, func(left podmanListedImage) bool { return left.ID == image.ID }) {
			removed = append(removed, image)
		}
	}
	result := unusedImagesResult(false, where, podmanUnusedImageList(removed), layers.total(func(path string) bool {
		_, err := os.Lstat(path)
		return os.IsNotExist(err)
	}))
	if pruned.Status != "succeeded" {
		result.Status = "failed"
		result.Reason = tool.failure(prune, pruned).Error() + "; " + result.Reason
	}
	return result
}

// listImages lists the images of the store that podman images lists with
// filters, intermediate images included.
func (p *podmanTool) listImages(ctx context.Context, filters ...string) ([]podmanListedImage, error) {
	args := append([]string{"images", "--all", "--format", "json"}, filters...)
	r := p.command(ctx, args...)
	if r.Status != "succeeded" {
		return nil, p.failure(args, r)
	}
	var images []podmanListedImage
	if err := json.Unmarshal([]byte(r.Stdout), &images); err != nil {
		return nil, fmt.Errorf("%s: podman %s printed no list of images: %w", p.module, strings.Join(args, " "), err) //nolint:nilaway // PodmanUnusedImages returns on podman's non-nil stop before passing the tool on.
	}
	return images, nil
}

// freedLayers are the layer directories of the images that a removal
// removes and that no image which stays uses, with the bytes each takes on
// disk. They are not known (nil) for a storage driver other than overlay,
// whose image layers are not directories that podman names.
type freedLayers map[string]int64

// total is the bytes of the layer directories for which gone is true, or of
// all of them when gone is nil; nil when they are not known.
func (layers freedLayers) total(gone func(path string) bool) *int64 {
	if layers == nil {
		return nil
	}
	var bytes int64
	for path, size := range layers {
		if gone == nil || gone(path) {
			bytes += size
		}
	}
	return &bytes
}

// layersFreedByRemoving measures the layers that removing the images in
// removable from every frees: podman image inspect names each image's
// layer directories in its overlay data.
func (p *podmanTool) layersFreedByRemoving(ctx context.Context, every, removable []podmanListedImage) (freedLayers, error) {
	args := []string{"image", "inspect", "--format", "json"}
	for _, image := range every {
		args = append(args, image.ID)
	}
	r := p.command(ctx, args...)
	if r.Status != "succeeded" {
		return nil, p.failure(args[:4], r)
	}
	var inspected []podmanInspectedImage
	if err := json.Unmarshal([]byte(r.Stdout), &inspected); err != nil {
		return nil, fmt.Errorf("%s: podman image inspect printed no list of images: %w", p.module, err) //nolint:nilaway // PodmanUnusedImages returns on podman's non-nil stop before passing the tool on.
	}
	kept, freed := map[string]bool{}, map[string]bool{}
	for _, image := range inspected {
		if image.GraphDriver.Name != "overlay" {
			return nil, nil
		}
		removed := slices.ContainsFunc(removable, func(r podmanListedImage) bool { return r.ID == image.ID })
		for _, layer := range overlayLayerDirectories(image.GraphDriver.Data) {
			if removed {
				freed[layer] = true
			} else {
				kept[layer] = true
			}
		}
	}
	layers := freedLayers{}
	for layer := range freed {
		if kept[layer] {
			continue
		}
		size, err := bytesOnDisk(layer)
		if err != nil {
			return nil, fmt.Errorf("%s: measure the image layer %s: %w", p.module, layer, err) //nolint:nilaway // PodmanUnusedImages returns on podman's non-nil stop before passing the tool on.
		}
		layers[layer] = size
	}
	return layers, nil
}

// overlayLayerDirectories are the directories of an image's layers, from
// podman's overlay data: the directory of its upper layer and of each lower
// one, whose diff directory the data names.
func overlayLayerDirectories(data map[string]string) []string {
	var layers []string
	for _, diff := range append([]string{data["UpperDir"]}, strings.Split(data["LowerDir"], ":")...) {
		if filepath.IsAbs(diff) {
			layers = append(layers, filepath.Dir(diff))
		}
	}
	return layers
}

// bytesOnDisk is what the files below path take on disk, in allocated
// blocks, counting a file with several names once; nothing is followed.
func bytesOnDisk(path string) (int64, error) {
	var total int64
	seen := map[uint64]bool{}
	err := filepath.WalkDir(path, func(entry string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(entry)
		if err != nil {
			return err
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok && !seen[uint64(st.Ino)] {
			seen[uint64(st.Ino)] = true
			total += 512 * int64(st.Blocks)
		}
		return nil
	})
	return total, err
}
