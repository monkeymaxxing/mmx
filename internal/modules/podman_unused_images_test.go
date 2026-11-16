package modules

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A stand-in podman for podman_unused_images keeps the store's images in
// $dir/images, one line "<ID> <names joined by , or -> <size> <containers>
// <layers joined by :, the lowest first>", and each layer as a directory
// $dir/storage/overlay/<layer>/diff. Its prune removes what Podman's does:
// the images that no container uses, with --all also the named ones, and
// the layers that no image which stays uses.
const fakePodmanOfUnusedImages = `
qualifies() { # names containers all
  [ "$2" = 0 ] && { [ "$3" = yes ] || [ "$1" = - ]; }
}
case "$1" in
images)
  dangling=no; unused=no
  for option; do case "$option" in dangling=true) dangling=yes;; containers=false) unused=yes;; esac; done
  separator=; printf '['
  while read id names size containers layers; do
    [ $unused = no ] || [ "$containers" = 0 ] || continue
    [ $dangling = no ] || [ "$names" = - ] || continue
    list=; [ "$names" = - ] || list="\"$(echo "$names" | sed 's/,/","/g')\""
    printf '%s{"Id":"%s","Names":[%s],"Size":%s,"Containers":%s}' "$separator" "$id" "$list" "$size" "$containers"; separator=,
  done < "$dir/images"
  echo ']';;
image)
  case "$2" in
  inspect)
    shift 4; separator=; printf '['
    for wanted; do
      while read id names size containers layers; do
        [ "$id" = "$wanted" ] || continue
        upper=${layers##*:}; lower=
        for layer in $(echo "$layers" | tr ':' ' '); do [ "$layer" = "$upper" ] || lower="$dir/storage/overlay/$layer/diff${lower:+:}$lower"; done
        printf '%s{"Id":"%s","GraphDriver":{"Name":"%s","Data":{"UpperDir":"%s","LowerDir":"%s"}}}' "$separator" "$id" "$(cat "$dir/driver")" "$dir/storage/overlay/$upper/diff" "$lower"; separator=,
      done < "$dir/images"
    done
    echo ']';;
  prune)
    all=no; [ "$4" != --all ] || all=yes
    : > "$dir/kept"
    while read id names size containers layers; do
      if qualifies "$names" "$containers" "$all"; then echo "$id"; else echo "$id $names $size $containers $layers" >> "$dir/kept"; fi
    done < "$dir/images"
    mv "$dir/kept" "$dir/images"
    for layer in "$dir"/storage/overlay/*; do
      grep -q "$(basename "$layer")" "$dir/images" || rm -r "$layer"
    done
    [ ! -f "$dir/prune-fails" ] || { echo "Error: some images could not be removed" >&2; exit 2; };;
  esac;;
*) echo "unexpected podman $*" >&2; exit 64;;
esac`

const (
	podmanBase    = "1111111111111111111111111111111111111111111111111111111111111111"
	podmanOldWeb  = "2222222222222222222222222222222222222222222222222222222222222222"
	podmanWeb     = "3333333333333333333333333333333333333333333333333333333333333333"
	podmanUnnamed = "4444444444444444444444444444444444444444444444444444444444444444"
)

// newUnusedPodmanImagesFixture is a store of four images: the base, which
// only the others use; the old web image, which no container uses and
// which shares the base layer with the web image; the web image, which a
// container uses; and an image without a name, whose only layer is its own.
func newUnusedPodmanImagesFixture(t *testing.T) (dir string, layerBytes map[string]int64) {
	t.Helper()
	dir = podmanFixture(t, map[string]string{"podman": fakePodmanOfUnusedImages})
	writeState(t, filepath.Join(dir, "driver"), "overlay")
	writeState(t, filepath.Join(dir, "images"),
		podmanBase+" registry.example.com/base:12 100 0 base\n"+
			podmanOldWeb+" registry.example.com/shop/web:2.3 300 0 base:old-web\n"+
			podmanWeb+" registry.example.com/shop/web:2.4 300 1 base:web\n"+
			podmanUnnamed+" - 150 0 unnamed\n")
	layerBytes = map[string]int64{}
	for layer, size := range map[string]int{"base": 100000, "old-web": 200000, "web": 200000, "unnamed": 50000} {
		diff := filepath.Join(dir, "storage/overlay", layer, "diff")
		if err := os.MkdirAll(diff, 0755); err != nil {
			t.Fatal(err)
		}
		writeState(t, filepath.Join(diff, "file"), strings.Repeat("x", size))
		// Another name of the same file takes no more room.
		if err := os.Link(filepath.Join(diff, "file"), filepath.Join(diff, "same-file")); err != nil {
			t.Fatal(err)
		}
		bytes, err := bytesOnDisk(filepath.Dir(diff))
		if err != nil {
			t.Fatal(err)
		}
		layerBytes[layer] = bytes
	}
	return dir, layerBytes
}

// The dryrun lists what podman image prune removes and predicts the bytes
// of the layers that no image which stays uses; the run removes them with
// podman image prune and reports what the store lost; then nothing is left.
func TestPodmanUnusedImages(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		remove  string
		removed []string
		freed   []string
		prune   string
	}{
		// The base image has a name, so only every_image_no_container_uses
		// removes it, and its layer stays all the same: the web image uses it.
		{"dangling_images_only", []string{"sha256:" + podmanUnnamed}, []string{"unnamed"}, "podman image prune --force"},
		{"every_image_no_container_uses", []string{"sha256:" + podmanBase, "sha256:" + podmanOldWeb, "sha256:" + podmanUnnamed}, []string{"old-web", "unnamed"}, "podman image prune --force --all"},
	} {
		dir, layerBytes := newUnusedPodmanImagesFixture(t)
		var freed int64
		for _, layer := range test.freed {
			freed += layerBytes[layer]
		}
		task := PodmanUnusedImages{Remove: test.remove}
		r := noMutation(t).PodmanUnusedImages(testContext(t), task)
		if ids, bytes := unusedImagesValue(t, r); r.Status != "succeeded" || r.Change != "would_change" || !slices.Equal(ids, test.removed) || bytes != freed {
			t.Fatalf("%s: %+v; want %v and %d bytes", test.remove, r, test.removed, freed)
		}
		if unknown := (&task).UnknownInADryrun(r); unknown != nil {
			t.Fatalf("%s: %v", test.remove, unknown)
		}
		if strings.Contains(readState(t, filepath.Join(dir, "calls")), "prune") {
			t.Fatalf("%s: a dryrun pruned", test.remove)
		}
		r = Runtime{}.PodmanUnusedImages(testContext(t), task)
		if ids, bytes := unusedImagesValue(t, r); r.Status != "succeeded" || r.Change != "changed" || !slices.Equal(ids, test.removed) || bytes != freed {
			t.Fatalf("%s: %+v; want %v and %d bytes", test.remove, r, test.removed, freed)
		}
		if !strings.HasPrefix(r.Reason, fmt.Sprintf("removed %d images from the system's image store and freed %s: ", len(test.removed), bytesText(freed))) {
			t.Fatalf("%s: %q", test.remove, r.Reason)
		}
		if calls := readState(t, filepath.Join(dir, "calls")); !strings.Contains(calls, test.prune+"\n") {
			t.Fatalf("%s: %s", test.remove, calls)
		}
		for _, dryrun := range []bool{true, false} {
			r = Runtime{Dryrun: dryrun}.PodmanUnusedImages(testContext(t), task)
			if ids, _ := unusedImagesValue(t, r); r.Change != "unchanged" || len(ids) != 0 || r.Reason != "the system's image store has no image that the task removes" {
				t.Fatalf("%s, dryrun %t: %+v", test.remove, dryrun, r)
			}
		}
		if !strings.Contains(readState(t, filepath.Join(dir, "images")), podmanWeb) {
			t.Fatalf("%s: the image that a container uses is gone", test.remove)
		}
	}
}

// With a storage driver other than overlay, podman names no layer
// directories: the bytes freed are not known, and a dryrun says so. A prune
// that fails fails the task, which still reports what the store lost.
func TestPodmanUnusedImagesWithoutOverlayAndAFailure(t *testing.T) {
	t.Parallel()
	dir, _ := newUnusedPodmanImagesFixture(t)
	writeState(t, filepath.Join(dir, "driver"), "btrfs")
	task := PodmanUnusedImages{Remove: "dangling_images_only"}
	r := noMutation(t).PodmanUnusedImages(testContext(t), task)
	if ids, bytes := unusedImagesValue(t, r); r.Change != "would_change" || len(ids) != 1 || bytes != nil || (&task).UnknownInADryrun(r)["value"] == "" {
		t.Fatalf("%+v", r)
	}
	writeState(t, filepath.Join(dir, "prune-fails"), "")
	r = Runtime{}.PodmanUnusedImages(testContext(t), task)
	if ids, bytes := unusedImagesValue(t, r); r.Status != "failed" || r.Change != "changed" || len(ids) != 1 || bytes != nil || !strings.Contains(r.Reason, "some images could not be removed") {
		t.Fatalf("%+v", r)
	}
}

func TestPodmanUnusedImagesRejectsInputs(t *testing.T) {
	t.Parallel()
	for _, remove := range []string{"", "dangling"} {
		if err := (&PodmanUnusedImages{Remove: remove}).Prepare(""); err == nil || !strings.Contains(err.Error(), "podman_unused_images.remove is required and must be dangling_images_only or every_image_no_container_uses") {
			t.Fatalf("%q: %v", remove, err)
		}
	}
	setWorkerEnvironment(t, "PATH", t.TempDir())
	if r := (Runtime{Dryrun: true}).PodmanUnusedImages(testContext(t), PodmanUnusedImages{Remove: "dangling_images_only"}); r.Status != "not_checked" || !strings.Contains(r.Reason, "podman is not installed") {
		t.Fatalf("%+v", r)
	}
}
