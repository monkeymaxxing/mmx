package modules

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"mmx/internal/protocol"
)

// A stand-in docker for docker_unused_images keeps the node's images in
// $dir/images, one line "<image ID> <names joined by , or -> <size>", and
// its containers in $dir/containers, one line "<container ID> <image ID>
// <the name of the image as the container was created with it>".
// Its daemon answers POST /images/prune on a Unix socket as Docker's does:
// it deletes the images that the filter selects and no container uses.
const fakeDockerOfUnusedImages = `
case "$1" in
info)
  store=overlay2; [ ! -e "$dir/containerd" ] || store=io.containerd.snapshotter.v1
  echo "{\"ServerVersion\":\"29.9.0\",\"DriverStatus\":[[\"driver-type\",\"$store\"]]}";;
context) echo "unix://$(cat "$dir/socket")";;
image)
  case "$2" in
  ls)
    while read id names size; do
      [ "$6" != dangling=true ] || [ "$names" = - ] || [ "$id" = "` + dockerAlsoNamed + `" ] || continue
      echo "$id"
    done < "$dir/images";;
  inspect)
    shift 2; separator=; printf '['
    for wanted; do
      while read id names size; do
        [ "$id" = "$wanted" ] || continue
        tags=; [ "$names" = - ] || tags="\"$(echo "$names" | sed 's/,/","/g')\""
        printf '%s{"Id":"%s","RepoTags":[%s],"Size":%s}' "$separator" "$id" "$tags" "$size"; separator=,
      done < "$dir/images"
    done
    echo ']';;
  esac;;
ps) [ ! -f "$dir/containers" ] || while read id image name; do echo "$id"; done < "$dir/containers";;
container)
  shift 2; separator=; printf '['
  for wanted; do
    while read id image name; do [ "$id" != "$wanted" ] || { printf '%s{"Id":"%s","Image":"%s","Config":{"Image":"%s"}}' "$separator" "$id" "$image" "$name"; separator=,; }; done < "$dir/containers"
  done
  echo ']';;
*) echo "unexpected docker $*" >&2; exit 64;;
esac`

type unusedDockerImagesFixture struct {
	t   *testing.T
	dir string
	// filters are the filters of each removal that the daemon was asked for.
	mutex   sync.Mutex
	filters []string
}

func newUnusedDockerImagesFixture(t *testing.T, images, containers string) *unusedDockerImagesFixture {
	t.Helper()
	f := &unusedDockerImagesFixture{t: t, dir: standInPrograms(t, map[string]string{"docker": fakeDockerOfUnusedImages})}
	writeState(t, filepath.Join(f.dir, "images"), images)
	writeState(t, filepath.Join(f.dir, "containers"), containers)
	short, err := os.MkdirTemp("/tmp", "mmx-docker-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(short) })
	socket := filepath.Join(short, "docker.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	writeState(t, filepath.Join(f.dir, "socket"), socket)
	server := &http.Server{Handler: http.HandlerFunc(f.prune)}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	return f
}

// prune deletes the images that the filter selects and that no container
// uses, and answers what Docker answers: the names it removed, the images
// it deleted with one layer each, and the bytes freed, which the daemon
// counts by layer and the test sets in $dir/freed.
func (f *unusedDockerImagesFixture) prune(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/images/prune" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.filters = append(f.filters, r.URL.Query().Get("filters"))
	var filters struct{ Dangling []string }
	json.Unmarshal([]byte(r.URL.Query().Get("filters")), &filters)
	used := readState(f.t, filepath.Join(f.dir, "containers"))
	// The containerd image store also keeps an image whose name a
	// container was created with.
	_, err := os.Stat(filepath.Join(f.dir, "containerd"))
	containerd := err == nil
	type step struct {
		Untagged string `json:",omitempty"`
		Deleted  string `json:",omitempty"`
	}
	var steps []step
	var kept []string
	for _, line := range strings.Split(strings.TrimSpace(readState(f.t, filepath.Join(f.dir, "images"))), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		namedByAContainer := slices.ContainsFunc(strings.Split(fields[1], ","), func(name string) bool { return strings.Contains(used, " "+name+"\n") })
		if strings.Contains(used, " "+fields[0]+" ") || (filters.Dangling[0] == "true" && fields[1] != "-") || (containerd && namedByAContainer) {
			kept = append(kept, line)
			continue
		}
		if fields[1] != "-" {
			for _, name := range strings.Split(fields[1], ",") {
				steps = append(steps, step{Untagged: name})
			}
		}
		steps = append(steps, step{Deleted: fields[0]}, step{Deleted: "sha256:layer-of-" + strings.TrimPrefix(fields[0], "sha256:")})
	}
	writeState(f.t, filepath.Join(f.dir, "images"), strings.Join(kept, "\n")+"\n")
	freed := int64(0)
	if len(steps) > 0 {
		json.Unmarshal([]byte(readState(f.t, filepath.Join(f.dir, "freed"))), &freed)
	}
	json.NewEncoder(w).Encode(map[string]any{"ImagesDeleted": steps, "SpaceReclaimed": freed})
}

func unusedImagesValue(t *testing.T, r protocol.Result) (ids []string, freed any) {
	t.Helper()
	v, ok := r.Value.(map[string]any)
	if !ok {
		t.Fatalf("value %#v", r.Value)
	}
	for _, image := range v["removed_images"].([]any) {
		ids = append(ids, image.(map[string]any)["image_id"].(string))
	}
	return ids, v["freed_bytes"]
}

const (
	dockerOldWeb    = "sha256:" + "1111111111111111111111111111111111111111111111111111111111111111"
	dockerDangling  = "sha256:" + "2222222222222222222222222222222222222222222222222222222222222222"
	dockerCurrent   = "sha256:" + "3333333333333333333333333333333333333333333333333333333333333333"
	dockerStoppedOn = "sha256:" + "4444444444444444444444444444444444444444444444444444444444444444"
	dockerUsedOld   = "sha256:" + "5555555555555555555555555555555555555555555555555555555555555555"
	dockerAlsoNamed = "sha256:" + "6666666666666666666666666666666666666666666666666666666666666666"
	dockerNewWorker = "sha256:" + "7777777777777777777777777777777777777777777777777777777777777777"
)

// The dryrun lists the images that the removal would remove, without the
// bytes it frees; the run asks the daemon with Docker's own filter and
// reports the images it deleted and the bytes it freed; then nothing is
// left. An image that a container uses stays, running or stopped, also
// when it is dangling.
func TestDockerUnusedImages(t *testing.T) {
	t.Parallel()
	// In the containerd image store, docker image ls --filter dangling=true
	// also lists an image whose dangling record stays beside named ones:
	// the stand-in lists dockerAlsoNamed as dangling, and inspect gives its
	// names.
	images := dockerOldWeb + " registry.example.com/shop/web:2.3,registry.example.com/shop/web:previous 1000\n" +
		dockerDangling + " - 2000\n" +
		dockerCurrent + " registry.example.com/shop/web:2.4 3000\n" +
		dockerStoppedOn + " registry.example.com/shop/migrate:2.4 4000\n" +
		dockerUsedOld + " - 5000\n" +
		dockerAlsoNamed + " registry.example.com/shop/base:1 6000\n" +
		dockerNewWorker + " registry.example.com/shop/worker:1 7000\n"
	// c3 was created from registry.example.com/shop/worker:1, which names
	// dockerNewWorker since; c3's image dockerUsedOld lost the name.
	containers := "c1 " + dockerCurrent + " registry.example.com/shop/web:2.4\nc2 " + dockerStoppedOn + " registry.example.com/shop/migrate:2.4\n" +
		"c3 " + dockerUsedOld + " registry.example.com/shop/worker:1\n"
	for _, test := range []struct {
		remove, filter string
		containerd     bool
		removed        []string
	}{
		{"dangling_images_only", `{"dangling":["true"]}`, false, []string{dockerDangling}},
		{"every_image_no_container_uses", `{"dangling":["false"]}`, false, []string{dockerOldWeb, dockerDangling, dockerAlsoNamed, dockerNewWorker}},
		{"dangling_images_only", `{"dangling":["true"]}`, true, []string{dockerDangling}},
		{"every_image_no_container_uses", `{"dangling":["false"]}`, true, []string{dockerOldWeb, dockerDangling, dockerAlsoNamed}},
	} {
		f := newUnusedDockerImagesFixture(t, images, containers)
		if test.containerd {
			writeState(t, filepath.Join(f.dir, "containerd"), "")
		}
		writeState(t, filepath.Join(f.dir, "freed"), "1234567")
		task := DockerUnusedImages{Remove: test.remove}
		r := Runtime{Dryrun: true}.DockerUnusedImages(testContext(t), task)
		if ids, freed := unusedImagesValue(t, r); r.Status != "succeeded" || r.Change != "would_change" || !slices.Equal(ids, test.removed) || freed != nil {
			t.Fatalf("%+v: %+v", test, r)
		}
		if unknown := (&task).UnknownInADryrun(r); unknown["value"] == "" {
			t.Fatalf("%s: a dryrun claims to know the freed bytes", test.remove)
		}
		if len(f.filters) != 0 {
			t.Fatalf("%s: a dryrun removed images", test.remove)
		}
		r = Runtime{}.DockerUnusedImages(testContext(t), task)
		if ids, freed := unusedImagesValue(t, r); r.Status != "succeeded" || r.Change != "changed" || !slices.Equal(ids, test.removed) || freed != int64(1234567) {
			t.Fatalf("%+v: %+v", test, r)
		}
		if !strings.Contains(r.Reason, "and freed 1.2 MiB (1234567 bytes)") {
			t.Fatalf("%s: %q", test.remove, r.Reason)
		}
		if !slices.Equal(f.filters, []string{test.filter}) {
			t.Fatalf("%s: filters %v, want %s", test.remove, f.filters, test.filter)
		}
		for _, id := range []string{dockerCurrent, dockerStoppedOn, dockerUsedOld} {
			if !strings.Contains(readState(t, filepath.Join(f.dir, "images")), id) {
				t.Fatalf("%s: an image that a container uses is gone: %s", test.remove, id)
			}
		}
		for _, dryrun := range []bool{true, false} {
			r = Runtime{Dryrun: dryrun}.DockerUnusedImages(testContext(t), task)
			if ids, _ := unusedImagesValue(t, r); r.Change != "unchanged" || len(ids) != 0 || r.Reason != "the node's Docker images has no image that the task removes" {
				t.Fatalf("%s, dryrun %t: %+v", test.remove, dryrun, r)
			}
		}
		if len(f.filters) != 1 {
			t.Fatalf("%s: an unchanged run asked the daemon to remove images", test.remove)
		}
	}
}

// Without docker, or with a daemon that does not answer, a dryrun cannot
// check the task; validation requires remove.
func TestDockerUnusedImagesNeedsDockerAndRemove(t *testing.T) {
	t.Parallel()
	setWorkerEnvironment(t, "PATH", t.TempDir())
	if r := (Runtime{Dryrun: true}).DockerUnusedImages(testContext(t), DockerUnusedImages{Remove: "dangling_images_only"}); r.Status != "not_checked" || !strings.Contains(r.Reason, "docker is not installed") {
		t.Fatalf("%+v", r)
	}
	for _, remove := range []string{"", "everything"} {
		if err := (&DockerUnusedImages{Remove: remove}).Prepare(""); err == nil || !strings.Contains(err.Error(), "docker_unused_images.remove is required and must be dangling_images_only or every_image_no_container_uses") {
			t.Fatalf("%q: %v", remove, err)
		}
	}
}

// A container's image name as written compares with docker's RepoTags.
func TestFamiliarImageName(t *testing.T) {
	t.Parallel()
	for written, familiar := range map[string]string{
		"nginx":                         "nginx:latest",
		"docker.io/library/nginx:1.27":  "nginx:1.27",
		"docker.io/shop/web":            "shop/web:latest",
		"127.0.0.1:5000/mmx-test/app":   "127.0.0.1:5000/mmx-test/app:latest",
		"127.0.0.1:5000/mmx-test/app:2": "127.0.0.1:5000/mmx-test/app:2",
		"registry.example.com/web@sha256:" + strings.Repeat("a", 64): "registry.example.com/web@sha256:" + strings.Repeat("a", 64),
	} {
		if got := familiarImageName(written); got != familiar {
			t.Errorf("%s: %s, want %s", written, got, familiar)
		}
	}
}
