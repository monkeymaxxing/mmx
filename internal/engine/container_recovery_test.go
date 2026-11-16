package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mmx/internal/config"
	"mmx/internal/modules"
)

// standInPodman puts a podman and a skopeo on PATH. The image store is the
// file store (an image ID, absent when the store has no image) and the
// registry the file registry in dir; a pull copies the one to the other.
func standInPodman(t *testing.T, dir string) (store, registry string) {
	t.Helper()
	store, registry = filepath.Join(dir, "image-store"), filepath.Join(dir, "image-registry")
	bin := filepath.Join(dir, "bin")
	writeProgramFileThroughShellChild(t, filepath.Join(bin, "podman"), `#!/bin/sh
case "$1 $2" in
"image exists") [ -f `+store+` ];;
"image inspect") printf '[{"Id": "%s"}]\n' "$(cat `+store+`)";;
"pull --quiet") cp `+registry+` `+store+`; cat `+store+`;;
*) echo "unexpected podman $*" >&2; exit 2;;
esac
`)
	writeProgramFileThroughShellChild(t, filepath.Join(bin, "skopeo"), `#!/bin/sh
printf '{"schemaVersion": 2, "config": {"digest": "sha256:%s"}}\n' "$(cat `+registry+`)"
`)
	return store, registry
}

// The ID of a podman_image is a tracked input of its node task block: the
// service restarts once when the ID differs from the one it started with,
// also when the run that pulled the image stopped before the restart, and
// also when the task says refresh: reload.
func TestPodmanImageIDRestartsItsServiceAlsoAfterAnInterruptedRun(t *testing.T) {
	f := newServiceFixture(t)
	store, registry := standInPodman(t, f.dir)
	one, two, three, four := strings.Repeat("1", 64), strings.Repeat("2", 64), strings.Repeat("3", 64), strings.Repeat("4", 64)
	const image = "registry.example.com/shop/web:2"
	op := func() config.Task {
		return config.Task{TaskBlock: &config.TaskBlock{ID: "shop",
			Changes:  []config.Task{{ID: "image", Module: "podman_image", ModuleInputs: &modules.PodmanImage{Image: image, PullImage: "always"}}},
			Activate: []config.Task{{Module: "systemd_service", ModuleInputs: &modules.Service{State: "running", Name: "demo", Refresh: "reload"}}}}}
	}
	// The node gets the image for the first time: the running service does
	// not have it yet.
	put(t, registry, one)
	f.run(t, op(), "succeeded")
	f.assert(t, "A", "restart\n")
	if read(t, store) != one {
		t.Fatal("the image was not pulled")
	}
	f.run(t, op(), "succeeded")
	f.assert(t, "A", "restart\n")

	// The tag moves. A dryrun says so with both IDs and predicts the
	// restart; it pulls nothing.
	put(t, registry, two)
	j := job(op())
	j.Dryrun = true
	preview := f.e.Run(context.Background(), j)
	requireStatus(t, preview, "succeeded")
	ids, _ := preview.Results[0].Value.(map[string]any)
	if preview.Results[0].Change != "would_change" || ids["image_id"] != "sha256:"+two || ids["previous_image_id"] != "sha256:"+one {
		t.Fatalf("%+v", preview.Results[0])
	}
	if r := preview.Results[1]; r.Change != "would_change" || r.Reason != "installed software version changed" {
		t.Fatalf("%+v", r)
	}
	if read(t, store) != one {
		t.Fatal("the dryrun pulled the image")
	}
	f.assert(t, "A", "restart\n")
	f.run(t, op(), "succeeded")
	f.assert(t, "A", "restart\nrestart\n")

	// A run pulls the next image and stops before the restart. The next run
	// finds the image unchanged and still restarts, once.
	put(t, registry, three)
	interrupted := op()
	interrupted.TaskBlock.Validate = []config.Task{{Module: "shell", ModuleInputs: &modules.Shell{Script: "exit 1"}}}
	f.run(t, interrupted, "failed")
	if read(t, store) != three {
		t.Fatal("the image was not pulled before the block failed")
	}
	f.assert(t, "A", "restart\nrestart\n")
	r := f.e.Run(context.Background(), job(op()))
	requireStatus(t, r, "succeeded")
	if r.Results[0].Change != "unchanged" || r.Results[1].Change != "changed" {
		t.Fatalf("%+v", r.Results)
	}
	f.assert(t, "A", "restart\nrestart\nrestart\n")
	f.run(t, op(), "succeeded")
	f.assert(t, "A", "restart\nrestart\nrestart\n")

	// when_missing never follows a tag that moved.
	put(t, registry, four)
	kept := op()
	config.InputsOf[modules.PodmanImage](kept.TaskBlock.Changes[0]).PullImage = "when_missing"
	f.run(t, kept, "succeeded")
	if read(t, store) != three {
		t.Fatal("when_missing pulled an image the node had")
	}
	f.assert(t, "A", "restart\nrestart\nrestart\n")
}

// A dryrun claims only what a run would do. When it cannot ask the registry
// about an image that the store has, the image task is not checked, and no
// restart of the block's service is predicted from it: a run may find the
// same image. For an image that the store does not have, a pull is certain,
// and the restart is predicted.
func TestPodmanImageDryrunPredictsNoRestartThatItCannotKnow(t *testing.T) {
	f := newServiceFixture(t)
	store, registry := standInPodman(t, f.dir)
	block := config.Task{TaskBlock: &config.TaskBlock{ID: "shop",
		Changes:  []config.Task{{ID: "image", Module: "podman_image", ModuleInputs: &modules.PodmanImage{Image: "registry.example.com/shop/web:2", PullImage: "always"}}},
		Activate: []config.Task{{ID: "service", Module: "systemd_service", ModuleInputs: &modules.Service{State: "running", Name: "demo"}}}}}
	put(t, registry, strings.Repeat("1", 64))
	f.run(t, block, "succeeded")
	f.assert(t, "A", "restart\n")
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
	if got := dryrun(); got[0] != "shop/changes/image succeeded unchanged" || got[1] != "shop/activate/service succeeded unchanged" {
		t.Fatal(got)
	}
	// The registry does not answer.
	writeProgramFileThroughShellChild(t, filepath.Join(f.dir, "bin", "skopeo"), "#!/bin/sh\necho 'pinging container registry: connection refused' >&2\nexit 1\n")
	if got := dryrun(); got[0] != "shop/changes/image not_checked unknown" || got[1] != "shop/activate/service succeeded unchanged" {
		t.Fatal(got)
	}
	if err := os.Remove(store); err != nil {
		t.Fatal(err)
	}
	if got := dryrun(); got[0] != "shop/changes/image not_checked unknown" || got[1] != "shop/activate/service succeeded would_change" {
		t.Fatal(got)
	}
	f.assert(t, "A", "restart\n")
}

// A Quadlet file is the definition of the service Podman generates from it,
// as a unit file is of its unit: the service task on shop-web.service
// follows shop-web.container, and not the volume file beside it, which
// defines shop-web-data-volume.service.
func TestAQuadletFileIsTheDefinitionOfItsGeneratedService(t *testing.T) {
	f := newServiceFixture(t)
	f.addService(t, "shop-web.service")
	quadlet := func(name string) config.Task {
		return config.Task{Module: "podman_quadlet", ModuleInputs: &modules.PodmanQuadlet{Name: name, Content: text("[Container]\nImage=registry.example.com/shop/web:2\n")}}
	}
	op := config.Task{TaskBlock: &config.TaskBlock{ID: "shop", Changes: []config.Task{quadlet("shop-web.container"), quadlet("shop-web-data.volume")},
		Activate: []config.Task{{Module: "systemd_service", ModuleInputs: &modules.Service{State: "running", Name: "shop-web.service", Refresh: "reload"}}}}}
	f.e.job = job(op)
	state, _, err := f.e.prepareServices(context.Background(), op, scope(), "shop")
	if err != nil {
		t.Fatal(err)
	}
	units := state.services["shop-web.service"].units
	if len(units) != 1 || units[0].Kind != "unit" || units[0].Path != "/etc/containers/systemd/shop-web.container" || units[0].UnitName != "shop-web.service" {
		t.Fatalf("%+v", units)
	}
	// A command that follows tracked inputs follows both files.
	if len(state.inputs.units) != 2 {
		t.Fatalf("%+v", state.inputs.units)
	}
}
