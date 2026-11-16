package engine

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mmx/internal/config"
	"mmx/internal/modules"
)

// A stand-in docker keeps the node's image ID in $MMX_TEST_SERVICE/image and
// the registry's in $MMX_TEST_SERVICE/registry-image; a pull copies it. Its
// daemon listens on the Unix socket that $MMX_TEST_SERVICE/socket names.
const fakeDockerForImages = `#!/bin/sh
image="$MMX_TEST_SERVICE/image"
case "$1 $2" in
"context inspect") echo "unix://$(cat "$MMX_TEST_SERVICE/socket")";;
"info --format") echo '{"ServerVersion": "29.9.0", "ClientInfo": {"Plugins": []}}';;
"version --format") echo 29.9.0;;
"image inspect") if [ -f "$image" ]; then printf '[{"Id": "%s"}]\n' "$(cat "$image")"; else echo '[]'; exit 1; fi;;
"pull --quiet") cp "$MMX_TEST_SERVICE/registry-image" "$image";;
*) echo "unexpected docker $*" >&2; exit 64;;
esac
`

// The image ID of a docker_image task is a tracked input of its node task
// block: the block's service restarts once when the ID differs from the one
// it last started with, also when the run that pulled the image ended before
// the restart, and a dryrun predicts the restart.
func TestDockerImageIDIsATrackedInput(t *testing.T) {
	f := newServiceFixture(t)
	writeProgramFileThroughShellChild(t, filepath.Join(f.dir, "bin", "docker"), fakeDockerForImages)
	image := filepath.Join(f.dir, "image")
	registry := filepath.Join(f.dir, "registry-image")
	op := func(pull string) config.Task {
		return config.Task{TaskBlock: &config.TaskBlock{ID: "converter",
			Changes:  []config.Task{{Module: "docker_image", ModuleInputs: &modules.DockerImage{Image: "registry.example.com/audio/converter:3.1", PullImage: pull}}},
			Activate: []config.Task{{Module: "systemd_service", ModuleInputs: &modules.Service{State: "running", Name: "demo"}}}}}
	}
	// The service runs and the node has the image: nothing restarts.
	put(t, image, "sha256:a")
	put(t, registry, "sha256:a")
	f.run(t, op("always"), "succeeded")
	f.assert(t, "A", "")
	// The registry names a newer image: one restart, and none on the rerun.
	put(t, registry, "sha256:b")
	r := f.e.Run(context.Background(), job(op("always")))
	requireStatus(t, r, "succeeded")
	if r.Results[0].Change != "changed" || r.Results[1].Change != "changed" || r.Results[1].Reason != "installed software version changed" {
		t.Fatalf("%+v", r.Results)
	}
	f.assert(t, "A", "restart\n")
	f.run(t, op("always"), "succeeded")
	f.assert(t, "A", "restart\n")
	// A run pulls the next image and ends before the restart. The next run
	// finds the image unchanged and still restarts the service once.
	put(t, registry, "sha256:c")
	interrupted := op("always")
	interrupted.TaskBlock.Validate = []config.Task{{Module: "shell", ModuleInputs: &modules.Shell{Script: "exit 1"}}}
	f.run(t, interrupted, "failed")
	if read(t, image) != "sha256:c" {
		t.Fatal("the image was not pulled")
	}
	f.assert(t, "A", "restart\n")
	r = f.e.Run(context.Background(), job(op("always")))
	requireStatus(t, r, "succeeded")
	if r.Results[0].Change != "unchanged" || r.Results[1].Change != "changed" {
		t.Fatalf("%+v", r.Results)
	}
	f.assert(t, "A", "restart\nrestart\n")
	f.run(t, op("always"), "succeeded")
	f.assert(t, "A", "restart\nrestart\n")
	// A dryrun that would pull a missing image predicts the restart and
	// changes nothing.
	if err := os.Remove(image); err != nil {
		t.Fatal(err)
	}
	j := job(op("when_missing"))
	j.Dryrun = true
	preview := f.e.Run(context.Background(), j)
	requireStatus(t, preview, "succeeded")
	if preview.Results[0].Change != "would_change" || preview.Results[1].Change != "would_change" {
		t.Fatalf("%+v", preview.Results)
	}
	if _, err := os.Stat(image); err == nil {
		t.Fatal("the dryrun pulled the image")
	}
	f.assert(t, "A", "restart\nrestart\n")
}

// A dryrun predicts the restart of a service that follows an image only when
// it knows that the image changes. A reference may name a list of images,
// one for each platform; when the registry serves another list, a node with
// the classic image store may keep its image, and the registry's answer does
// not say. The dryrun then reports the image as not checked and predicts no
// restart. For a reference that names one image, another digest is another
// image, and the dryrun predicts the restart.
func TestDockerImageDryrunPredictsNoRestartThatItCannotKnow(t *testing.T) {
	f := newServiceFixture(t)
	writeProgramFileThroughShellChild(t, filepath.Join(f.dir, "bin", "docker"), fakeDockerForImages)
	// The stand-in daemon answers the question which image a reference
	// names in its registry. A Unix socket's path is short, so it has a
	// directory of its own.
	served := map[string]string{}
	short, err := os.MkdirTemp("/tmp", "mmx-engine-docker-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(short) })
	listener, err := net.Listen("unix", filepath.Join(short, "docker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"Descriptor": served})
	})}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	put(t, filepath.Join(f.dir, "socket"), filepath.Join(short, "docker.sock"))

	block := config.Task{TaskBlock: &config.TaskBlock{ID: "converter",
		Changes:  []config.Task{{Module: "docker_image", ModuleInputs: &modules.DockerImage{Image: "registry.example.com/audio/converter:3.1", PullImage: "always"}}},
		Activate: []config.Task{{Module: "systemd_service", ModuleInputs: &modules.Service{State: "running", Name: "demo"}}}}}
	put(t, filepath.Join(f.dir, "image"), "sha256:a")
	put(t, filepath.Join(f.dir, "registry-image"), "sha256:a")
	f.run(t, block, "succeeded")
	f.assert(t, "A", "")
	// The results of the image task and of the service task in a dryrun.
	dryrun := func() []string {
		j := job(block)
		j.Dryrun = true
		var out []string
		for _, result := range f.e.Run(context.Background(), j).Results[:2] {
			out = append(out, result.Task+" "+result.Status+" "+result.Change)
		}
		return out
	}
	served["mediaType"], served["digest"] = "application/vnd.oci.image.index.v1+json", "sha256:another-list"
	if got := dryrun(); got[0] != "converter/changes/task_1 not_checked unknown" || got[1] != "converter/activate/task_1 succeeded unchanged" {
		t.Fatal(got)
	}
	served["mediaType"], served["digest"] = "application/vnd.oci.image.manifest.v1+json", "sha256:another-image"
	if got := dryrun(); got[0] != "converter/changes/task_1 succeeded would_change" || got[1] != "converter/activate/task_1 succeeded would_change" {
		t.Fatal(got)
	}
	f.assert(t, "A", "")
}

// A stand-in docker with the Compose plugin and no containers.
const fakeDockerForProjects = `#!/bin/sh
case "$1" in
info) echo '{"ServerVersion": "29.9.0", "ClientInfo": {"Plugins": [{"Name": "compose", "Path": "/usr/libexec/docker/cli-plugins/docker-compose", "Version": "v5.6.0"}]}}';;
ps) ;;
*) echo "unexpected docker $*" >&2; exit 64;;
esac
`

// In a dryrun, docker_compose_project learns from the engine that an earlier
// node task would write its Compose file: in the same node task block, or as
// an ordinary node task before it. A file elsewhere does not count.
func TestDockerComposeProjectDryrunKnowsThatItsFilesWouldChange(t *testing.T) {
	f := newServiceFixture(t)
	writeProgramFileThroughShellChild(t, filepath.Join(f.dir, "bin", "docker"), fakeDockerForProjects)
	project := filepath.Join(f.dir, "shop")
	if err := os.Mkdir(project, 0755); err != nil {
		t.Fatal(err)
	}
	compose := config.Task{Module: "docker_compose_project", ModuleInputs: &modules.DockerComposeProject{
		ComposeProjectName: "shop", ComposeProjectDir: project, State: "running", PullImages: "when_missing", ContainersOfRemovedServices: "remove"}}
	dryrun := func(tasks ...config.Task) []string {
		t.Helper()
		j := job(tasks...)
		j.Dryrun = true
		r := f.e.Run(context.Background(), j)
		requireStatus(t, r, "succeeded")
		var out []string
		for _, result := range r.Results {
			out = append(out, result.Task+" "+result.Status+" "+result.Change+" "+result.Reason)
		}
		return out
	}
	wouldChange := "succeeded would_change an earlier node task of this dryrun would change a file of this Compose project or of this node task block; MMX can compare the services with the containers only once the files are installed"
	file := taskFile(filepath.Join(project, "compose.yaml"), "services: {}\n")
	got := dryrun(config.Task{TaskBlock: &config.TaskBlock{ID: "shop", Changes: []config.Task{file}, Activate: []config.Task{compose}}})
	if len(got) != 3 || got[1] != "shop/activate/task_1 "+wouldChange {
		t.Fatal(got)
	}
	if got := dryrun(file, compose); len(got) != 2 || got[1] != "task_2 "+wouldChange {
		t.Fatal(got)
	}
	got = dryrun(taskFile(filepath.Join(f.dir, "elsewhere.conf"), "x\n"), compose)
	if len(got) != 2 || !strings.HasPrefix(got[1], "task_2 not_checked unknown docker_compose_project: the Compose file "+project+"/compose.yaml does not exist") ||
		!strings.HasSuffix(got[1], "The earlier node task task_1 would change this node first and may provide what is missing; if the run then succeeds, change nothing.") {
		t.Fatal(got)
	}
	if _, err := os.Stat(filepath.Join(project, "compose.yaml")); err == nil {
		t.Fatal("the dryrun wrote the Compose file")
	}
}
