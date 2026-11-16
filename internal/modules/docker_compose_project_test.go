package modules

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

const (
	testDatabaseImage    = "registry.example.com/shop/database:17"
	testDatabaseImageKey = "registry.example.com_shop_database_17"
)

// shop writes the stand-in's Compose file of the project: one line for each
// service or volume.
func (f *dockerFixture) shop(lines ...string) {
	f.t.Helper()
	writeState(f.t, filepath.Join(f.project, "compose.yaml"), strings.Join(lines, "\n")+"\n")
}

// compose runs a docker_compose_project task as the engine runs it.
func (f *dockerFixture) compose(run Runtime, p DockerComposeProject, scope value.Scope) protocol.Result {
	f.t.Helper()
	if err := p.Prepare(""); err != nil {
		f.t.Fatal(err)
	}
	return p.Run(testContext(f.t), NodeTask{Runtime: f.withPrivateTaskDirectoryRoot(run), Scope: scope})
}

// services is a result's services as "name action" or, in a dryrun, "name
// action: reason".
func composeServices(t *testing.T, r protocol.Result) []string {
	t.Helper()
	var out []string
	for _, entry := range dockerValue(t, r.Value)["services"].([]any) {
		service := entry.(map[string]any)
		text := fmt.Sprint(service["name"], " ", service["action"])
		if reason, set := service["reason"]; set {
			text += ": " + reason.(string)
		}
		out = append(out, text)
	}
	return out
}

func composeImages(t *testing.T, r protocol.Result) []any {
	t.Helper()
	return dockerValue(t, r.Value)["images"].([]any)
}

// containers lists the stand-in's containers as "name status".
func (f *dockerFixture) containers() []string {
	f.t.Helper()
	entries, _ := os.ReadDir(filepath.Join(f.dir, "containers"))
	var out []string
	for _, entry := range entries {
		out = append(out, f.get("containers/"+entry.Name()+"/name")+" "+f.get("containers/"+entry.Name()+"/status"))
	}
	slices.Sort(out)
	return out
}

// container finds the directory of the stand-in's container of a service.
func (f *dockerFixture) container(service string) string {
	f.t.Helper()
	entries, _ := os.ReadDir(filepath.Join(f.dir, "containers"))
	for _, entry := range entries {
		if f.get("containers/"+entry.Name()+"/service") == service {
			return "containers/" + entry.Name()
		}
	}
	f.t.Fatal("no container of service", service)
	return ""
}

func TestDockerComposeProjectRunning(t *testing.T) {
	t.Parallel()
	f := newDockerFixture(t)
	project := DockerComposeProject{ComposeProjectName: "shop", ComposeProjectDir: f.project, State: "running", PullImages: "when_missing", ContainersOfRemovedServices: "remove"}
	web, database := "service web "+testImage+" web-1", "service database "+testDatabaseImage+" database-1"
	f.shop(web, database, "volume database_data shop_database_data")
	f.set("registry/"+testImageKey, "sha256:web1 sha256:web-manifest1")
	f.set("registry/"+testDatabaseImageKey, "sha256:database1 sha256:database-manifest1")

	// A dryrun says what a run would do and why, and changes nothing.
	r := f.compose(noMutation(t), project, nil)
	if r.Status != "succeeded" || r.Change != "would_change" || !reflect.DeepEqual(composeServices(t, r), []string{"database would_create: the container is missing", "web would_create: the container is missing"}) {
		t.Fatalf("%+v", r)
	}
	if want := []any{
		map[string]any{"image": testDatabaseImage, "previous_image_id": nil, "image_id": nil},
		map[string]any{"image": testImage, "previous_image_id": nil, "image_id": nil},
	}; !reflect.DeepEqual(composeImages(t, r), want) || f.containers() != nil || f.get("images/"+testImageKey) != "" {
		t.Fatalf("%+v %v", r.Value, f.containers())
	}
	// The first run pulls both images and creates both containers.
	r = f.compose(Runtime{}, project, nil)
	if r.Status != "succeeded" || r.Change != "changed" || !reflect.DeepEqual(composeServices(t, r), []string{"database created", "web created"}) {
		t.Fatalf("%+v", r)
	}
	if want := []any{
		map[string]any{"image": testDatabaseImage, "previous_image_id": nil, "image_id": "sha256:database1"},
		map[string]any{"image": testImage, "previous_image_id": nil, "image_id": "sha256:web1"},
	}; !reflect.DeepEqual(composeImages(t, r), want) || !reflect.DeepEqual(f.containers(), []string{"shop-database-2 running", "shop-web-1 running"}) {
		t.Fatalf("%+v %v", r.Value, f.containers())
	}
	// A rerun and a dryrun find every service unchanged. The rerun lets
	// docker compose up decide that; the dryrun asks Compose's own dry run
	// and runs nothing else that could change the node.
	for _, run := range []Runtime{{}, noMutation(t)} {
		takeCalls(t, f.dir)
		r := f.compose(run, project, nil)
		if r.Change != "unchanged" || !reflect.DeepEqual(composeServices(t, r), []string{"database unchanged", "web unchanged"}) || len(composeImages(t, r)) != 0 {
			t.Fatalf("%+v", r)
		}
	}
	for _, call := range takeCalls(t, f.dir) {
		if (strings.Contains(call, " up ") && !strings.Contains(call, " --dry-run --progress plain up --no-start --pull never --remove-orphans")) || strings.Contains(call, " pull ") || strings.Contains(call, " down") || strings.Contains(call, " stop") {
			t.Fatal("the dryrun ran", call)
		}
	}
	// What Compose's dry run wrote went into a private directory, which is
	// gone.
	if left, _ := os.ReadDir(f.privateTaskDirectoryRoot); len(left) != 0 {
		t.Fatal(left)
	}

	// A changed definition recreates only its service.
	f.shop("service web "+testImage+" web-2", database, "volume database_data shop_database_data")
	r = f.compose(noMutation(t), project, nil)
	if r.Change != "would_change" || !reflect.DeepEqual(composeServices(t, r), []string{"database unchanged", "web would_recreate: the definition differs"}) {
		t.Fatalf("%+v", r)
	}
	r = f.compose(Runtime{}, project, nil)
	if r.Change != "changed" || !reflect.DeepEqual(composeServices(t, r), []string{"database unchanged", "web recreated"}) || !reflect.DeepEqual(f.containers(), []string{"shop-database-2 running", "shop-web-3 running"}) {
		t.Fatalf("%+v %v", r, f.containers())
	}

	// The registry moves the tag. when_missing never follows it; always asks
	// the registry in a dryrun, and a run pulls and recreates.
	f.set("registry/"+testImageKey, "sha256:web2 sha256:web-manifest2")
	if r := f.compose(Runtime{}, project, nil); r.Change != "unchanged" {
		t.Fatalf("%+v", r)
	}
	project.PullImages = "always"
	r = f.compose(noMutation(t), project, nil)
	if r.Change != "would_change" || !reflect.DeepEqual(composeServices(t, r), []string{"database unchanged", "web would_recreate: the image differs: the registry names another image for " + testImage + " now"}) ||
		!reflect.DeepEqual(composeImages(t, r), []any{map[string]any{"image": testImage, "previous_image_id": "sha256:web1", "image_id": nil}}) {
		t.Fatalf("%+v", r)
	}
	if !reflect.DeepEqual(f.asked, []string{testDatabaseImage, testImage}) || f.get("images/"+testImageKey) != "sha256:web1 sha256:web-manifest1" {
		t.Fatal(f.asked)
	}
	r = f.compose(Runtime{}, project, nil)
	if r.Change != "changed" || !reflect.DeepEqual(composeServices(t, r), []string{"database unchanged", "web recreated"}) ||
		!reflect.DeepEqual(composeImages(t, r), []any{map[string]any{"image": testImage, "previous_image_id": "sha256:web1", "image_id": "sha256:web2"}}) {
		t.Fatalf("%+v", r)
	}
	for _, run := range []Runtime{{}, noMutation(t)} {
		if r := f.compose(run, project, nil); r.Change != "unchanged" || !reflect.DeepEqual(composeServices(t, r), []string{"database unchanged", "web unchanged"}) {
			t.Fatalf("%+v", r)
		}
	}
	// In the containerd image store a dryrun knows the new image ID.
	f.set("containerd", "")
	f.set("registry/"+testImageKey, "sha256:web3 sha256:web-manifest3")
	if r := f.compose(noMutation(t), project, nil); !reflect.DeepEqual(composeImages(t, r), []any{map[string]any{"image": testImage, "previous_image_id": "sha256:web2", "image_id": "sha256:web-manifest3"}}) {
		t.Fatalf("%+v", r)
	}
	// A reference with a digest names one image forever, and in the
	// containerd image store that digest is the image ID: a dryrun reports
	// it for an image that the node does not have yet.
	pinned := "registry.example.com/shop/cache@sha256:" + strings.Repeat("cd", 32)
	f.shop("service web "+testImage+" web-2", database, "service cache "+pinned+" cache-1")
	f.set("registry/"+strings.NewReplacer("/", "_", ":", "_", "@", "_").Replace(pinned), "sha256:"+strings.Repeat("cd", 32)+" sha256:"+strings.Repeat("cd", 32))
	f.asked = nil
	r = f.compose(noMutation(t), project, nil)
	if !slices.Contains(composeServices(t, r), "cache would_create: the container is missing") || !slices.ContainsFunc(composeImages(t, r), func(image any) bool {
		return reflect.DeepEqual(image, map[string]any{"image": pinned, "previous_image_id": nil, "image_id": "sha256:" + strings.Repeat("cd", 32)})
	}) || slices.Contains(f.asked, pinned) {
		t.Fatalf("%+v %v", r, f.asked)
	}
	f.shop("service web "+testImage+" web-2", database, "volume database_data shop_database_data")
	// A registry that does not answer: the dryrun cannot check.
	f.set("registry-down", "")
	if r := f.compose(noMutation(t), project, nil); r.Status != "not_checked" || !strings.HasPrefix(r.Reason, "docker_compose_project: the registry does not say which image "+testDatabaseImage+" names: ") || !strings.Contains(r.Reason, "connection refused") {
		t.Fatalf("%+v", r)
	}
	f.remove("registry-down")
	f.remove("containerd")
	f.set("registry/"+testImageKey, "sha256:web2 sha256:web-manifest2")
	project.PullImages = "when_missing"

	// A stopped container is started. Compose 2.26 lists such a container
	// in its dry run; later versions do not.
	f.set(f.container("database")+"/status", "exited")
	for _, version := range []string{"2.26.1-4", "v2.40.0"} {
		f.set("compose-version", version)
		if r := f.compose(noMutation(t), project, nil); !reflect.DeepEqual(composeServices(t, r), []string{"database would_start: the container is stopped", "web unchanged"}) {
			t.Fatalf("%s: %+v", version, r)
		}
	}
	if r := f.compose(Runtime{}, project, nil); r.Change != "changed" || !reflect.DeepEqual(composeServices(t, r), []string{"database started", "web unchanged"}) {
		t.Fatalf("%+v", r)
	}

	// A service with more containers gets them.
	f.shop("service web "+testImage+" web-2 2", database)
	if r := f.compose(noMutation(t), project, nil); !reflect.DeepEqual(composeServices(t, r), []string{"database unchanged", "web would_create: the service has 1 of its 2 containers"}) {
		t.Fatalf("%+v", r)
	}
	if r := f.compose(Runtime{}, project, nil); !reflect.DeepEqual(composeServices(t, r), []string{"database unchanged", "web created"}) || len(f.containers()) != 3 {
		t.Fatalf("%+v %v", r, f.containers())
	}
	f.shop("service web "+testImage+" web-2", database)
	if r := f.compose(Runtime{}, project, nil); !reflect.DeepEqual(composeServices(t, r), []string{"database unchanged", "web removed"}) || len(f.containers()) != 2 {
		t.Fatalf("%+v %v", r, f.containers())
	}

	// A service that left the Compose file: keep leaves its container,
	// remove removes it.
	f.shop("service web " + testImage + " web-2")
	project.ContainersOfRemovedServices = "keep"
	for _, run := range []Runtime{{}, noMutation(t)} {
		if r := f.compose(run, project, nil); r.Change != "unchanged" || !reflect.DeepEqual(composeServices(t, r), []string{"database unchanged", "web unchanged"}) || len(f.containers()) != 2 {
			t.Fatalf("%+v", r)
		}
	}
	project.ContainersOfRemovedServices = "remove"
	if r := f.compose(noMutation(t), project, nil); !reflect.DeepEqual(composeServices(t, r), []string{"database would_remove: the service is no longer in the Compose file", "web unchanged"}) {
		t.Fatalf("%+v", r)
	}
	if r := f.compose(Runtime{}, project, nil); r.Change != "changed" || !reflect.DeepEqual(composeServices(t, r), []string{"database removed", "web unchanged"}) || len(f.containers()) != 1 {
		t.Fatalf("%+v %v", r, f.containers())
	}
}

// Docker Compose decides whether an image differs. In the containerd image
// store, an image for several platforms gets a new ID when the image of any
// platform changes; Compose 5 compares the image for the container's own
// platform and keeps the container when that one stayed, and MMX then
// reports unchanged too.
func TestDockerComposeProjectKeepsAContainerWhoseImageForItsPlatformStayed(t *testing.T) {
	t.Parallel()
	f := newDockerFixture(t)
	project := DockerComposeProject{ComposeProjectName: "shop", ComposeProjectDir: f.project, State: "running", PullImages: "when_missing", ContainersOfRemovedServices: "remove"}
	f.shop("service web " + testImage + " web-1")
	f.set("registry/"+testImageKey, "sha256:index1 sha256:index1")
	if r := f.compose(Runtime{}, project, nil); r.Change != "changed" {
		t.Fatalf("%+v", r)
	}
	// The node's image gets a new ID and its image for the platform stays.
	f.set(f.container("web")+"/image-label", "sha256:platform1")
	f.set("images/"+testImageKey, "sha256:index2 sha256:index2")
	if r := f.compose(noMutation(t), project, nil); !reflect.DeepEqual(composeServices(t, r), []string{"web would_recreate: the image differs: the container was created from another image than " + testImage + " names on the node now"}) {
		t.Fatalf("%+v", r)
	}
	f.set("platform/"+testImageKey, "sha256:platform1")
	containers := f.containers()
	for _, run := range []Runtime{noMutation(t), {}} {
		if r := f.compose(run, project, nil); r.Change != "unchanged" || !reflect.DeepEqual(f.containers(), containers) {
			t.Fatalf("%+v", r)
		}
	}
}

// The lines that docker compose --dry-run up --no-start printed in the
// measurements, on Compose 2.26.1, 2.40.3, and 5.6.0.
func TestReadComposeDryRun(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct {
		text string
		want composeDryRun
	}{
		"Compose 2.26.1": {
			"time=\"2026-10-09T02:15:45Z\" level=warning msg=\"/opt/shop/compose.yaml: the attribute `version` is obsolete, it will be ignored, please remove it to avoid potential confusion\"\n" +
				" DRY-RUN MODE -  Network shop_default  Creating\n" +
				" DRY-RUN MODE -  Network shop_default  Created\n" +
				" DRY-RUN MODE -  Volume \"shop_named_by_hand\"  Creating\n" +
				" DRY-RUN MODE -  Volume \"shop_named_by_hand\"  Created\n" +
				" DRY-RUN MODE -  Container shop-worker-1  Running\n" +
				" DRY-RUN MODE -  Container shop_frontend  Recreate\n" +
				" DRY-RUN MODE -  Container shop-worker-2  Stopping\n" +
				" DRY-RUN MODE -  Container shop-worker-2  Stopped\n" +
				" DRY-RUN MODE -  Container shop-worker-2  Removing\n" +
				" DRY-RUN MODE -  Container shop-worker-2  Removed\n" +
				" DRY-RUN MODE -  Container shop_frontend  Recreated\n" +
				" DRY-RUN MODE -  Container shop-job-1  Created\n",
			composeDryRun{"shop-worker-1": {"Running"}, "shop_frontend": {"Recreate", "Recreated"}, "shop-worker-2": {"Stopping", "Stopped", "Removing", "Removed"}, "shop-job-1": {"Created"}},
		},
		"Compose 2.40.3": {
			" DRY-RUN MODE -  Container shop-web-1  Stopping\n" +
				" DRY-RUN MODE -  Container shop-web-1  Stopped\n" +
				" DRY-RUN MODE -  Network shop_backend  Removed\n" +
				" DRY-RUN MODE -  Volume shop_named_by_hand  Creating\n" +
				" DRY-RUN MODE -  Volume shop_named_by_hand  Created\n" +
				" DRY-RUN MODE -  Container shop-web-1  Running\n" +
				" DRY-RUN MODE -  Container shop-worker-3  Creating\n" +
				" DRY-RUN MODE -  Container shop-worker-3  Created\n",
			composeDryRun{"shop-web-1": {"Stopping", "Stopped", "Running"}, "shop-worker-3": {"Creating", "Created"}},
		},
		"Compose 5.6.0": {
			"time=\"2026-10-09T02:06:11Z\" level=warning msg=\"Found orphan containers (shop-worker-1) for this project. If you removed or renamed this service in your compose file, you can run this command with the --remove-orphans flag to clean it up.\"\n" +
				" Container shop-web-1 Stopping \n" +
				" Container shop-web-1 Stopped \n" +
				" Network shop_backend Removing \n" +
				" Network shop_backend Removed \n" +
				" Network shop_backend Creating \n" +
				" Network shop_backend Creating \n" +
				" Network shop_backend Created \n" +
				" Network shop_backend Created \n" +
				" Volume shop_data Creating \n" +
				" Volume shop_data Created \n" +
				" Container shop-store-1 Recreate \n" +
				" Container shop-store-1 Recreated \n",
			composeDryRun{"shop-web-1": {"Stopping", "Stopped"}, "shop-store-1": {"Recreate", "Recreated"}},
		},
		"nothing": {"\n", composeDryRun{}},
	} {
		if got, err := readComposeDryRun(test.text); err != nil || !reflect.DeepEqual(got, test.want) {
			t.Fatalf("%s: %v %v", name, got, err)
		}
	}
	// A line that MMX does not know is never read as something else: steps
	// of the start step, a build, a pull, an error, another form.
	for _, line := range []string{
		" DRY-RUN MODE -  Container shop-store-1  Waiting",
		" DRY-RUN MODE -  Container shop-store-1  Replaced",
		" DRY-RUN MODE -    build service web ",
		" DRY-RUN MODE -  ==> ==> writing image dryRun-ca84d1343b96baa8137c943ed1860e522cacb238 ",
		" DRY-RUN MODE -  web Pulling ",
		" Image shop-built:1 Building ",
		" Container shop-store-1 Error dependency store failed to start",
		" Secret shop_token Creating ",
		`{"dry-run":true,"id":"Container shop-web-1","status":"Recreate"}`,
		`time="2026-10-09T02:06:11Z" level=error msg="something failed"`,
		"dependency failed to start: container shop-store-1 is unhealthy",
	} {
		_, err := readComposeDryRun(" DRY-RUN MODE -  Container shop-web-1  Running\n" + line + "\n")
		if err == nil || !strings.Contains(err.Error(), "printed a line that MMX does not know: "+strings.TrimSpace(line)) {
			t.Fatalf("%q: %v", line, err)
		}
	}
}

// What MMX reports for each service from the steps of Compose's dry run.
func TestComposeServicesOfADryRun(t *testing.T) {
	t.Parallel()
	const image = "registry.example.com/shop/web:2.4"
	container := func(name, service, status string) dockerContainer {
		c := dockerContainer{ID: "id-of-" + name, Name: "/" + name, Image: "sha256:web1"}
		c.State.Status = status
		c.Config.Image = image
		c.Config.Labels = map[string]string{composeLabelService: service, composeLabelVersion: "2.40.3", composeLabelOneOff: "False"}
		return c
	}
	service := func(dependsOn ...string) composeServiceDefinition {
		d := composeServiceDefinition{Image: image, DependsOn: map[string]json.RawMessage{}}
		for _, name := range dependsOn {
			d.DependsOn[name] = json.RawMessage(`{"condition":"service_started"}`)
		}
		return d
	}
	named := func(d composeServiceDefinition, name string) composeServiceDefinition {
		d.ContainerName = name
		return d
	}
	oneOff := container("shop-web-run-8ee04a3349f8", "web", "running")
	oneOff.Config.Labels[composeLabelOneOff] = "True"
	madeByCompose2 := container("shop-web-1", "web", "running")
	madeByCompose2.Config.Labels[composeLabelVersion] = "2.26.1"
	fromAnotherImage := container("shop-web-1", "web", "running")
	fromAnotherImage.Image = "sha256:web0"
	fromAnotherTag := container("shop-web-1", "web", "running")
	fromAnotherTag.Config.Image = "registry.example.com/shop/web:2.3"
	onNode := map[string]dockerImageState{image: {ID: "sha256:web1"}}
	restart := "Docker Compose stops and restarts the container because Compose recreates a service or network used by the container"

	for name, test := range map[string]struct {
		services   map[string]composeServiceDefinition
		containers []dockerContainer
		steps      composeDryRun
		node       map[string]dockerImageState
		newer      map[string]string
		unseen     map[string]string
		want       []string
		unknown    string
	}{
		// A dryrun cannot tell whether the run's pull changes the image:
		// the service that Compose would keep is not checked. What is
		// certain stays: a container to create, a stopped one to start, a
		// definition that differs.
		"an image of which the dryrun cannot tell whether a pull changes it": {
			services:   map[string]composeServiceDefinition{"web": service(), "worker": service(), "idle": service(), "new": service(), "changed": service()},
			containers: []dockerContainer{container("shop-web-1", "web", "running"), container("shop-worker-1", "worker", "exited"), container("shop-changed-1", "changed", "running")},
			steps:      composeDryRun{"shop-web-1": {"Running"}, "shop-new-1": {"Creating", "Created"}, "shop-changed-1": {"Recreate", "Recreated"}},
			node:       onNode,
			unseen:     map[string]string{image: "the registry now serves another list of images"},
			want:       []string{"changed recreated: the definition differs", "idle unchanged", "new created: the container is missing", "web not_checked: the registry now serves another list of images", "worker started: the container is stopped"},
		},
		"a project that does not exist": {
			services: map[string]composeServiceDefinition{"web": named(service(), "shop_frontend"), "worker": service(), "idle": service()},
			steps:    composeDryRun{"shop_frontend": {"Creating", "Created"}, "shop-worker-1": {"Creating", "Created"}, "shop-worker-2": {"Creating", "Created"}},
			// Compose creates no container for a service with no containers
			// to run, and MMX claims none.
			want: []string{"idle unchanged", "web created: the container is missing", "worker created: the container is missing"},
		},
		"nothing to do, beside a container of docker compose run": {
			services:   map[string]composeServiceDefinition{"web": service()},
			containers: []dockerContainer{container("shop-web-1", "web", "running"), oneOff},
			steps:      composeDryRun{"shop-web-1": {"Running"}},
			want:       []string{"web unchanged"},
		},
		"more and fewer containers": {
			services:   map[string]composeServiceDefinition{"web": service(), "worker": service()},
			containers: []dockerContainer{container("shop-web-1", "web", "running"), container("shop-web-2", "web", "running"), container("shop-worker-1", "worker", "running"), container("shop-worker-2", "worker", "running")},
			steps:      composeDryRun{"shop-web-1": {"Running"}, "shop-web-2": {"Running"}, "shop-web-3": {"Creating", "Created"}, "shop-worker-1": {"Running"}, "shop-worker-2": {"Stopping", "Stopped", "Removing", "Removed"}},
			want:       []string{"web created: the service has 2 of its 3 containers", "worker removed: the service has 2 containers and needs 1"},
		},
		"Compose 2.26 recreates a service together with the one it depends on": {
			services:   map[string]composeServiceDefinition{"store": service(), "web": service("store"), "worker": service()},
			containers: []dockerContainer{container("shop-store-1", "store", "running"), container("shop-web-1", "web", "running"), container("shop-worker-1", "worker", "exited")},
			steps:      composeDryRun{"shop-store-1": {"Recreate", "Recreated"}, "shop-web-1": {"Recreate", "Recreated"}, "shop-worker-1": {"Created"}},
			node:       onNode,
			want:       []string{"store recreated: the definition differs", "web recreated: the definition differs, or Docker Compose recreates the service together with store, which it depends on", "worker started: the container is stopped"},
		},
		"later versions stop and start it": {
			services:   map[string]composeServiceDefinition{"store": service(), "web": service("store"), "worker": service()},
			containers: []dockerContainer{container("shop-store-1", "store", "running"), container("shop-web-1", "web", "running"), container("shop-worker-1", "worker", "exited")},
			steps:      composeDryRun{"shop-store-1": {"Recreate", "Recreated"}, "shop-web-1": {"Stopping", "Stopped", "Created"}},
			node:       onNode,
			want:       []string{"store recreated: the definition differs", "web started: " + restart, "worker started: the container is stopped"},
		},
		"a network that Compose recreates": {
			services:   map[string]composeServiceDefinition{"web": service()},
			containers: []dockerContainer{container("shop-web-1", "web", "running")},
			steps:      composeDryRun{"shop-web-1": {"Stopping", "Stopped", "Running"}},
			want:       []string{"web started: " + restart},
		},
		"a container that another Compose version made": {
			services:   map[string]composeServiceDefinition{"web": service()},
			containers: []dockerContainer{madeByCompose2},
			steps:      composeDryRun{"shop-web-1": {"Recreate", "Recreated"}},
			node:       onNode,
			want:       []string{"web recreated: the definition differs: Docker Compose 2.26.1 made the container, and Docker Compose 2.40.3, which the node has now, may give the same definition another hash"},
		},
		"another image on the node": {
			services:   map[string]composeServiceDefinition{"web": service()},
			containers: []dockerContainer{fromAnotherImage},
			steps:      composeDryRun{"shop-web-1": {"Recreate", "Recreated"}},
			node:       onNode,
			want:       []string{"web recreated: the image differs: the container was created from another image than " + image + " names on the node now"},
		},
		"another image in the registry, with pull_images: always": {
			services:   map[string]composeServiceDefinition{"web": service(), "worker": service()},
			containers: []dockerContainer{container("shop-web-1", "web", "running")},
			steps:      composeDryRun{"shop-web-1": {"Running"}, "shop-worker-1": {"Creating", "Created"}},
			node:       onNode,
			newer:      map[string]string{image: "the registry names another image for " + image + " now"},
			want:       []string{"web recreated: the image differs: the registry names another image for " + image + " now", "worker created: the container is missing"},
		},
		"the Compose file names an image that the node does not have": {
			services:   map[string]composeServiceDefinition{"web": service()},
			containers: []dockerContainer{fromAnotherTag},
			steps:      composeDryRun{"shop-web-1": {"Recreate", "Recreated"}},
			want:       []string{"web recreated: the definition differs: the Compose file names the image " + image},
		},
		// Compose's dry run recreates a container whose image is gone from
		// the node; a run pulls the image first and may keep the container.
		"the node no longer has the container's image": {
			services:   map[string]composeServiceDefinition{"web": service()},
			containers: []dockerContainer{container("shop-web-1", "web", "running")},
			steps:      composeDryRun{"shop-web-1": {"Recreate", "Recreated"}},
			unknown:    "MMX cannot tell whether a run recreates the containers of service web: the node no longer has their image " + image,
		},
		"services that left the Compose file": {
			services:   map[string]composeServiceDefinition{"web": service()},
			containers: []dockerContainer{container("shop-web-1", "web", "running"), container("shop-old-1", "old", "running"), container("shop-kept-1", "kept", "exited")},
			steps:      composeDryRun{"shop-web-1": {"Running"}, "shop-old-1": {"Stopping", "Stopped", "Removing", "Removed"}},
			want:       []string{"kept unchanged", "old removed: the service is no longer in the Compose file", "web unchanged"},
		},
		"a running container that the dry run does not list": {
			services:   map[string]composeServiceDefinition{"web": service(), "worker": service()},
			containers: []dockerContainer{container("shop-web-1", "web", "running"), container("shop-worker-1", "worker", "running")},
			steps:      composeDryRun{"shop-web-1": {"Running"}},
			unknown:    "does not say what happens to the container shop-worker-1 of service worker",
		},
		"a running container that is only listed as Created": {
			services:   map[string]composeServiceDefinition{"web": service()},
			containers: []dockerContainer{container("shop-web-1", "web", "running")},
			steps:      composeDryRun{"shop-web-1": {"Created"}},
			unknown:    "does not say what happens to the container shop-web-1 of service web; its steps for it: Created",
		},
		"a container name of no service": {
			services:   map[string]composeServiceDefinition{"web": service()},
			containers: []dockerContainer{container("shop-web-1", "web", "running")},
			steps:      composeDryRun{"shop-web-1": {"Running"}, "shop-cache-1": {"Creating", "Created"}},
			unknown:    "names the container shop-cache-1, which MMX cannot match to a service of the Compose file",
		},
		"a container name that fits two services": {
			services: map[string]composeServiceDefinition{"web": named(service(), "shop-api-1"), "api": service()},
			steps:    composeDryRun{"shop-api-1": {"Creating", "Created"}},
			unknown:  "names the container shop-api-1, which MMX cannot match",
		},
		"a step for a container that does not exist and is not created": {
			services: map[string]composeServiceDefinition{"web": service()},
			steps:    composeDryRun{"shop-web-1": {"Running"}},
			unknown:  "names the container shop-web-1, which MMX cannot match",
		},
	} {
		c := composeProject{d: &docker{composeVersion: "2.40.3+ds1-0ubuntu1~24.04.1"}}
		services, err := c.servicesOfDryRun(composeDefinition{Name: "shop", Services: test.services}, test.containers, test.steps, test.node, test.newer, test.unseen)
		if test.unknown != "" {
			if err == nil || !strings.Contains(err.Error(), test.unknown) || !strings.HasPrefix(err.Error(), "docker_compose_project: MMX cannot tell ") {
				t.Fatalf("%s: %v %v", name, services, err)
			}
			continue
		}
		var got []string
		for _, service := range services {
			text := service.Name + " " + service.Action
			if service.Reason != "" {
				text += ": " + service.Reason
			}
			got = append(got, text)
		}
		if err != nil || !reflect.DeepEqual(got, test.want) {
			t.Fatalf("%s:\n got %q\nwant %q\n%v", name, got, test.want, err)
		}
	}
}

// A reference of a project may name a list of images, one for each platform.
// When the registry serves another list, a dryrun cannot see whether the
// image for the node's platform changed. It then reports the services that
// use the image as not checked, and claims neither a recreation nor a pull
// that changes the image.
func TestDockerComposeProjectWhoseImageIsAListOfImages(t *testing.T) {
	t.Parallel()
	list := "the registry now serves another list of images for " + testImage + "; whether the image for this node's platform changed, a dryrun cannot see"
	t.Run("the classic image store", func(t *testing.T) {
		t.Parallel()
		f := newDockerFixture(t)
		project := DockerComposeProject{ComposeProjectName: "shop", ComposeProjectDir: f.project, State: "running", PullImages: "always", ContainersOfRemovedServices: "remove"}
		f.shop("service web " + testImage + " web-1")
		f.set("registry/"+testImageKey, "sha256:web1 sha256:list1 list")
		if r := f.compose(Runtime{}, project, nil); r.Change != "changed" {
			t.Fatalf("%+v", r)
		}
		containers := f.containers()
		// The list changes and the image for the node's platform stays.
		f.set("registry/"+testImageKey, "sha256:web1 sha256:list2 list")
		r := f.compose(noMutation(t), project, nil)
		if r.Status != "not_checked" || r.Change != "unknown" || r.Reason != "docker_compose_project: "+list+". A run pulls, and Docker Compose recreates the container only if it did" {
			t.Fatalf("%+v", r)
		}
		if !reflect.DeepEqual(composeServices(t, r), []string{"web not_checked: " + list + ". A run pulls, and Docker Compose recreates the container only if it did"}) ||
			!reflect.DeepEqual(composeImages(t, r), []any{map[string]any{"image": testImage, "previous_image_id": "sha256:web1", "image_id": nil, "reason": list + ". A run pulls, and the image on the node changes only if it did"}}) {
			t.Fatalf("%+v", r.Value)
		}
		// The run pulls, and nothing changes.
		if r := f.compose(Runtime{}, project, nil); r.Status != "succeeded" || r.Change != "unchanged" || !reflect.DeepEqual(f.containers(), containers) {
			t.Fatalf("%+v %v", r, f.containers())
		}
		if r := f.compose(noMutation(t), project, nil); r.Status != "succeeded" || r.Change != "unchanged" {
			t.Fatalf("%+v", r)
		}
		// With something that is certain to change, the task would change,
		// and the service is still not checked.
		f.set("registry/"+testImageKey, "sha256:web2 sha256:list3 list")
		f.set("registry/"+testDatabaseImageKey, "sha256:database1 sha256:database-manifest1")
		f.shop("service web "+testImage+" web-1", "service database "+testDatabaseImage+" database-1")
		r = f.compose(noMutation(t), project, nil)
		if r.Status != "succeeded" || r.Change != "would_change" || r.Reason != "" || !reflect.DeepEqual(composeServices(t, r), []string{"database would_create: the container is missing", "web not_checked: " + list + ". A run pulls, and Docker Compose recreates the container only if it did"}) {
			t.Fatalf("%+v", r)
		}
		// This time the image for the node's platform changed: the run
		// recreates the container.
		if r := f.compose(Runtime{}, project, nil); !reflect.DeepEqual(composeServices(t, r), []string{"database created", "web recreated"}) {
			t.Fatalf("%+v", r)
		}
		// A reference that names one image: another digest is another
		// image, and the dryrun says so.
		f.set("registry/"+testImageKey, "sha256:web3 sha256:manifest3")
		if r := f.compose(noMutation(t), project, nil); !reflect.DeepEqual(composeServices(t, r), []string{"database unchanged", "web would_recreate: the image differs: the registry names another image for " + testImage + " now"}) {
			t.Fatalf("%+v", r)
		}
	})
	// The containerd image store names an image by the digest that the
	// registry serves, so the pull is certain to give the image another ID.
	// Whether Compose then recreates the container depends on its version.
	t.Run("the containerd image store", func(t *testing.T) {
		t.Parallel()
		f := newDockerFixture(t)
		f.set("containerd", "")
		project := DockerComposeProject{ComposeProjectName: "shop", ComposeProjectDir: f.project, State: "running", PullImages: "always", ContainersOfRemovedServices: "remove"}
		f.shop("service web " + testImage + " web-1")
		f.set("registry/"+testImageKey, "sha256:list1 sha256:list1 list")
		if r := f.compose(Runtime{}, project, nil); r.Change != "changed" {
			t.Fatalf("%+v", r)
		}
		f.set("registry/"+testImageKey, "sha256:list2 sha256:list2 list")
		r := f.compose(noMutation(t), project, nil)
		if r.Status != "succeeded" || r.Change != "would_change" ||
			!reflect.DeepEqual(composeServices(t, r), []string{"web not_checked: " + list + ". A run pulls the list, which gives the image on the node another ID, and Docker Compose then decides whether it recreates the container"}) ||
			!reflect.DeepEqual(composeImages(t, r), []any{map[string]any{"image": testImage, "previous_image_id": "sha256:list1", "image_id": "sha256:list2"}}) {
			t.Fatalf("%+v", r)
		}
	})
}

// When MMX cannot read what Compose's dry run says, the dryrun reports
// not_checked with the reason, and a run is not affected.
func TestDockerComposeProjectDryrunThatCannotTell(t *testing.T) {
	t.Parallel()
	f := newDockerFixture(t)
	project := DockerComposeProject{ComposeProjectName: "shop", ComposeProjectDir: f.project, State: "running", PullImages: "when_missing", ContainersOfRemovedServices: "remove"}
	f.shop("service web " + testImage + " web-1")
	f.set("registry/"+testImageKey, "sha256:web1 sha256:web-manifest1")
	if r := f.compose(Runtime{}, project, nil); r.Change != "changed" {
		t.Fatalf("%+v", r)
	}
	containers := f.containers()
	notChecked := func(want string) {
		t.Helper()
		r := f.compose(noMutation(t), project, nil)
		if r.Status != "not_checked" || r.Change != "unknown" || r.Value != nil || !strings.Contains(r.Reason, want) {
			t.Fatalf("%+v", r)
		}
		if r := f.compose(Runtime{}, project, nil); r.Status != "succeeded" || r.Change != "unchanged" || !reflect.DeepEqual(f.containers(), containers) {
			t.Fatalf("%+v", r)
		}
	}
	// A later Compose prints its steps in another form.
	f.set("dry-run-prints", " Container shop-web-1 Kept \n")
	notChecked("MMX cannot tell what a run would do, because Docker Compose's own dry run printed a line that MMX does not know: Container shop-web-1 Kept")
	// It prints nothing about a container that runs.
	f.set("dry-run-prints", "\n")
	notChecked("does not say what happens to the container shop-web-1 of service web")
	f.remove("dry-run-prints")
	// The dry run fails.
	f.set("dry-run-fails", "unknown flag: --dry-run\n")
	notChecked("MMX cannot tell what a run would do, because Docker Compose's own dry run failed: docker compose --project-directory " + f.project + " --project-name shop --file " + f.project + "/compose.yaml --dry-run --progress plain up --no-start --pull never --remove-orphans: unknown flag: --dry-run")
	f.remove("dry-run-fails")
	if r := f.compose(noMutation(t), project, nil); r.Change != "unchanged" {
		t.Fatalf("%+v", r)
	}

	// The node loses the image while the container runs. Compose's dry run
	// then recreates the container; a run pulls the image first and keeps a
	// container that was created from it.
	f.remove("images/" + testImageKey)
	r := f.compose(noMutation(t), project, nil)
	if r.Status != "not_checked" || !strings.Contains(r.Reason, "MMX cannot tell whether a run recreates the containers of service web: the node no longer has their image "+testImage) {
		t.Fatalf("%+v", r)
	}
	if r := f.compose(Runtime{}, project, nil); r.Status != "succeeded" || !reflect.DeepEqual(composeServices(t, r), []string{"web unchanged"}) || len(composeImages(t, r)) != 1 || !reflect.DeepEqual(f.containers(), containers) {
		t.Fatalf("%+v %v", r, f.containers())
	}
	// The Compose file names an image that the node does not have yet: the
	// definition differs, whatever the image is.
	f.shop("service web " + testDatabaseImage + " web-1")
	f.set("registry/"+testDatabaseImageKey, "sha256:database1 sha256:database-manifest1")
	r = f.compose(noMutation(t), project, nil)
	if r.Change != "would_change" || !reflect.DeepEqual(composeServices(t, r), []string{"web would_recreate: the definition differs: the Compose file names the image " + testDatabaseImage}) ||
		!reflect.DeepEqual(composeImages(t, r), []any{map[string]any{"image": testDatabaseImage, "previous_image_id": nil, "image_id": nil}}) {
		t.Fatalf("%+v", r)
	}
	if r := f.compose(Runtime{}, project, nil); !reflect.DeepEqual(composeServices(t, r), []string{"web recreated"}) {
		t.Fatalf("%+v", r)
	}
}

// Compose versions give the same definition different hashes, so an upgraded
// Compose recreates every container once. The dryrun's reason says so.
func TestDockerComposeProjectNamesAnotherComposeVersion(t *testing.T) {
	t.Parallel()
	f := newDockerFixture(t)
	project := DockerComposeProject{ComposeProjectName: "shop", ComposeProjectDir: f.project, State: "running", PullImages: "when_missing", ContainersOfRemovedServices: "remove"}
	f.shop("service web " + testImage + " hash-of-compose-2")
	f.set("registry/"+testImageKey, "sha256:web1 sha256:web-manifest1")
	f.set("compose-version", "2.40.3+ds1-0ubuntu1")
	if r := f.compose(Runtime{}, project, nil); r.Change != "changed" {
		t.Fatalf("%+v", r)
	}
	f.set("compose-version", "v5.6.0")
	f.shop("service web " + testImage + " hash-of-compose-5")
	want := "web would_recreate: the definition differs: Docker Compose 2.40.3 made the container, and Docker Compose 5.6.0, which the node has now, may give the same definition another hash"
	if r := f.compose(noMutation(t), project, nil); !reflect.DeepEqual(composeServices(t, r), []string{want}) {
		t.Fatalf("%+v", r)
	}
	if r := f.compose(Runtime{}, project, nil); !reflect.DeepEqual(composeServices(t, r), []string{"web recreated"}) {
		t.Fatalf("%+v", r)
	}
	for _, run := range []Runtime{{}, noMutation(t)} {
		if r := f.compose(run, project, nil); r.Change != "unchanged" {
			t.Fatalf("%+v", r)
		}
	}
}

func TestDockerComposeProjectFailureAfterPartialWork(t *testing.T) {
	t.Parallel()
	f := newDockerFixture(t)
	project := DockerComposeProject{ComposeProjectName: "shop", ComposeProjectDir: f.project, State: "running", PullImages: "when_missing", ContainersOfRemovedServices: "remove"}
	f.shop("service database "+testDatabaseImage+" database-1", "service web "+testImage+" web-1")
	f.set("registry/"+testImageKey, "sha256:web1 sha256:web-manifest1")
	f.set("registry/"+testDatabaseImageKey, "sha256:database1 sha256:database-manifest1")
	// docker compose up fails after the first service: the result keeps what
	// changed, and the next try finishes.
	f.set("fail-up", "")
	r := f.compose(Runtime{}, project, nil)
	if r.Status != "failed" || r.Change != "changed" || !strings.Contains(r.Reason, "port is already allocated") || !reflect.DeepEqual(composeServices(t, r), []string{"database created", "web unchanged"}) {
		t.Fatalf("%+v", r)
	}
	if !reflect.DeepEqual(composeImages(t, r), []any{map[string]any{"image": testDatabaseImage, "previous_image_id": nil, "image_id": "sha256:database1"}}) {
		t.Fatalf("%+v", r.Value)
	}
	f.remove("fail-up")
	r = f.compose(Runtime{}, project, nil)
	if r.Status != "succeeded" || r.Change != "changed" || !reflect.DeepEqual(composeServices(t, r), []string{"database unchanged", "web created"}) {
		t.Fatalf("%+v", r)
	}
	// A Compose file that docker compose rejects fails the task with its
	// message, and a failure that changed nothing reports no change.
	f.shop("invalid")
	if r := f.compose(Runtime{}, project, nil); r.Status != "failed" || !strings.Contains(r.Reason, "Additional property invalid is not allowed") {
		t.Fatalf("%+v", r)
	}
}

func withPreviewPaths(run Runtime, paths []string) Runtime {
	run.PreviewPaths = paths
	return run
}

// In a dryrun, the services can be compared only once the project's files
// are installed.
func TestDockerComposeProjectBeforeItsFilesChange(t *testing.T) {
	t.Parallel()
	f := newDockerFixture(t)
	ctx := testContext(t)
	project := &DockerComposeProject{ComposeProjectName: "shop", ComposeProjectDir: f.project, State: "running", PullImages: "when_missing", ContainersOfRemovedServices: "remove"}
	file := filepath.Join(f.project, "compose.yaml")
	wouldChange := func(services ...any) map[string]any {
		return map[string]any{"services": append([]any{}, services...), "images": []any{}}
	}
	entry := func(name string) map[string]any {
		return map[string]any{"name": name, "action": "would_change", "reason": filesWouldChange}
	}
	// No Compose file yet: an earlier node task of the dryrun would write
	// it, or nothing would, and then a run fails without another try.
	r := project.Run(ctx, NodeTask{Runtime: withPreviewPaths(f.withPrivateTaskDirectoryRoot(noMutation(t)), []string{"/etc/hosts", file})})
	if r.Status != "succeeded" || r.Change != "would_change" || !reflect.DeepEqual(r.Value, wouldChange()) || !strings.HasPrefix(r.Reason, filesWouldChange+"; ") {
		t.Fatalf("%+v", r)
	}
	if r := project.Run(ctx, NodeTask{Runtime: withPreviewPaths(f.withPrivateTaskDirectoryRoot(noMutation(t)), []string{f.project})}); r.Change != "would_change" {
		t.Fatalf("%+v", r)
	}
	// A Git checkout of a directory that holds the project would change the
	// project's files as well.
	if r := project.Run(ctx, NodeTask{Runtime: withPreviewPaths(f.withPrivateTaskDirectoryRoot(noMutation(t)), []string{filepath.Dir(f.project)})}); r.Change != "would_change" {
		t.Fatalf("%+v", r)
	}
	if r := project.Run(ctx, NodeTask{Runtime: withPreviewPaths(f.withPrivateTaskDirectoryRoot(noMutation(t)), []string{f.project + "-other/compose.yaml"})}); r.Status != "not_checked" {
		t.Fatalf("%+v", r)
	}
	r = project.Run(ctx, NodeTask{Runtime: f.withPrivateTaskDirectoryRoot(Runtime{})})
	if r.Status != "failed" || r.FailureKind != "missing_prerequisite" || r.Reason != "docker_compose_project: the Compose file "+file+" does not exist; an earlier node task, such as a file task, may create it" {
		t.Fatalf("%+v", r)
	}
	// With the project running: a file of the project that would change, or
	// any file of the same node task block, which may be an environment
	// file.
	f.shop("service web "+testImage+" web-1", "service database "+testDatabaseImage+" database-1")
	f.set("registry/"+testImageKey, "sha256:web1 sha256:web-manifest1")
	f.set("registry/"+testDatabaseImageKey, "sha256:database1 sha256:database-manifest1")
	if r := f.compose(Runtime{}, *project, nil); r.Change != "changed" {
		t.Fatalf("%+v", r)
	}
	f.shop("service web " + testImage + " web-1")
	for _, task := range []NodeTask{
		{Runtime: withPreviewPaths(f.withPrivateTaskDirectoryRoot(noMutation(t)), []string{filepath.Join(f.project, "web.env")})},
		{Runtime: f.withPrivateTaskDirectoryRoot(noMutation(t)), PreviewPathsOfTaskBlock: []string{"/etc/shop/web.env"}},
	} {
		if r := project.Run(ctx, task); r.Change != "would_change" || !reflect.DeepEqual(r.Value, wouldChange(entry("database"), entry("web"))) {
			t.Fatalf("%+v", r)
		}
	}
	// Something else that would change in the block, such as an image that
	// an earlier node task would pull, is no file: the services are compared.
	if r := project.Run(ctx, NodeTask{Runtime: f.withPrivateTaskDirectoryRoot(noMutation(t)), PreviewChanged: true}); !reflect.DeepEqual(composeServices(t, r), []string{"database would_remove: the service is no longer in the Compose file", "web unchanged"}) {
		t.Fatalf("%+v", r)
	}
	// A run compares, whatever the engine predicted in an earlier dryrun.
	if r := project.Run(ctx, NodeTask{Runtime: f.withPrivateTaskDirectoryRoot(Runtime{}), PreviewPathsOfTaskBlock: []string{file}}); !reflect.DeepEqual(composeServices(t, r), []string{"database removed", "web unchanged"}) {
		t.Fatalf("%+v", r)
	}
}

func TestDockerComposeProjectLogins(t *testing.T) {
	t.Parallel()
	f := newDockerFixture(t)
	const password = "sentinel-password-0815"
	f.shop("service web " + testImage + " web-1")
	f.set("registry/"+testImageKey, "sha256:web1 sha256:web-manifest1")
	f.set("registry/"+testImageKey+".login", base64.StdEncoding.EncodeToString([]byte("deploy:"+password)))
	project := DockerComposeProject{ComposeProjectName: "shop", ComposeProjectDir: f.project, State: "running", PullImages: "always", ContainersOfRemovedServices: "remove"}
	scope := value.Scope{"secrets": map[string]any{"registry_password": password}}
	if r := f.compose(Runtime{}, project, scope); r.Status != "failed" || !strings.Contains(r.Reason, "pull access denied") {
		t.Fatalf("%+v", r)
	}
	project.RegistryLogins = []DockerRegistryLogin{{Registry: "registry.example.com", RegistryUsername: "deploy", RegistryPassword: "{{ .secrets.registry_password }}"}}
	// Without the login a dryrun cannot check, and says what lets it.
	if r := f.compose(noMutation(t), DockerComposeProject{ComposeProjectName: "shop", ComposeProjectDir: f.project, State: "running", PullImages: "always", ContainersOfRemovedServices: "remove"}, scope); r.Status != "not_checked" ||
		r.Reason != "docker_compose_project: the registry does not say which image "+testImage+" names: unauthorized: authentication required. If the image is private, a dryrun needs the registry login from registry_logins on the node task" {
		t.Fatalf("%+v", r)
	}
	if r := f.compose(noMutation(t), project, scope); r.Status != "succeeded" || r.Change != "would_change" {
		t.Fatalf("%+v", r)
	}
	// The dryrun asked the registry with the login and wrote it nowhere.
	if strings.Contains(readState(t, filepath.Join(f.dir, "environment")), "DOCKER_CONFIG=") {
		t.Fatal("a dryrun gave docker a private configuration directory")
	}
	r := f.compose(Runtime{}, project, scope)
	if r.Status != "succeeded" || r.Change != "changed" {
		t.Fatalf("%+v", r)
	}
	if text, _ := json.Marshal(r); strings.Contains(string(text), password) {
		t.Fatal("the result holds the password")
	}
	f.noSecretOnCommandLinesOrInEnvironments(password)
	// The Compose plugin is still found with the private configuration
	// directory: docker ran with a link to it there.
	if !strings.Contains(readState(t, filepath.Join(f.dir, "environment")), "DOCKER_CONFIG="+f.privateTaskDirectoryRoot+"/docker_compose_project-") {
		t.Fatal("docker did not run with the private configuration directory")
	}
}

// docker_compose_project hands its registry logins only to docker. A secret
// in a login marks no trace, so the result lists what happened to each
// service, and a failure that tells a login reports [redacted] in its place
// and keeps the rest of its reason.
func TestDockerComposeProjectLoginsHandedOnlyToDockerLeaveTheResultReadable(t *testing.T) {
	t.Parallel()
	f := newDockerFixture(t)
	const user, password = "robot$shop+deploy", "sentinel p/w&0815"
	secrets := map[string]string{"registry_username": user, "registry_password": password}
	stored := base64.StdEncoding.EncodeToString([]byte(user + ":" + password))
	f.shop("service web " + testImage + " web-1")
	f.set("registry/"+testImageKey, "sha256:web1 sha256:web-manifest1")
	f.set("registry/"+testImageKey+".login", stored)
	project := DockerComposeProject{ComposeProjectName: "shop", ComposeProjectDir: f.project, State: "running", PullImages: "always", ContainersOfRemovedServices: "remove",
		RegistryLogins: []DockerRegistryLogin{{Registry: "registry.example.com", RegistryUsername: "{{ .secrets.registry_username }}", RegistryPassword: "{{ .secrets.registry_password }}"}}}
	// The project starts: the result names the service and the image.
	s, trace := taskScope(secrets)
	r := reportedByTheEngine(f.compose(Runtime{}, project, s), s)
	if r.Status != "succeeded" || !reflect.DeepEqual(composeServices(t, r), []string{"web created"}) || len(composeImages(t, r)) != 1 || trace.Used {
		t.Fatalf("%+v, the logins marked the trace: %v", r, trace.Used)
	}
	// docker compose up fails in its pull and tells the login.
	f.set("registry/"+testImageKey, "sha256:web2 sha256:web-manifest2")
	f.set("pull-fails-saying", `Get "https://registry.example.com/token?account=robot%24shop%2Bdeploy": unauthorized: `+user+":"+password+" is not a login;")
	s, trace = taskScope(secrets)
	told := f.compose(Runtime{}, project, s)
	for _, form := range []string{user, password, "robot%24shop%2Bdeploy", stored} {
		if !strings.Contains(told.Reason, form) {
			t.Fatalf("the stand-in docker did not tell %q: %s", form, told.Reason)
		}
	}
	r = reportedByTheEngine(told, s)
	if want := `: Error response from daemon: Get "https://registry.example.com/token?account=[redacted]": unauthorized: [redacted]:[redacted] is not a login; {"auths":{"registry.example.com":{"auth":"[redacted]"}}}`; r.Status != "failed" ||
		!strings.HasPrefix(r.Reason, "docker_compose_project: docker compose ") || !strings.HasSuffix(r.Reason, want) || trace.Used {
		t.Fatalf("%+v\nwant a reason that ends with %s", r, want)
	}
	// A dryrun asks Docker's daemon, which refuses and tells the header that
	// carried the login.
	s, trace = taskScope(secrets)
	told = f.compose(noMutation(t), project, s)
	header := (registryLogin{Registry: "registry.example.com", Username: user, Password: password}).headerForDockerDaemon()
	if !strings.Contains(told.Reason, header) {
		t.Fatalf("the stand-in daemon did not tell the login: %s", told.Reason)
	}
	r = reportedByTheEngine(told, s)
	if want := "docker_compose_project: the registry does not say which image " + testImage + ` names: Get "https://registry.example.com/token?account=[redacted]": unauthorized: [redacted]:[redacted] is not a login; [redacted]. Check the login that the task gives for the registry`; r.Status != "not_checked" || r.Reason != want || trace.Used {
		t.Fatalf("%+v\nwant reason %s", r, want)
	}
	// A user name that is written in the playbook is no secret and stays.
	project.RegistryLogins[0].RegistryUsername = user
	s, _ = taskScope(secrets)
	r = reportedByTheEngine(f.compose(Runtime{}, project, s), s)
	if want := `: Error response from daemon: Get "https://registry.example.com/token?account=robot%24shop%2Bdeploy": unauthorized: ` + user + `:[redacted] is not a login; {"auths":{"registry.example.com":{"auth":"[redacted]"}}}`; !strings.HasSuffix(r.Reason, want) {
		t.Fatalf("%+v\nwant a reason that ends with %s", r, want)
	}
}

func TestDockerComposeProjectNeedsDockerComposeAndTheDaemon(t *testing.T) {
	t.Parallel()
	f := newDockerFixture(t)
	f.shop("service web " + testImage + " web-1")
	project := DockerComposeProject{ComposeProjectName: "shop", ComposeProjectDir: f.project, State: "running", PullImages: "when_missing", ContainersOfRemovedServices: "remove"}
	check := func(want string) {
		t.Helper()
		r := f.compose(Runtime{}, project, nil)
		if r.Status != "failed" || r.FailureKind != "missing_prerequisite" || !strings.Contains(r.Reason, want) {
			t.Fatalf("%+v", r)
		}
		if r := f.compose(noMutation(t), project, nil); r.Status != "not_checked" || !strings.Contains(r.Reason, want) {
			t.Fatalf("%+v", r)
		}
	}
	f.set("compose-version", "2.24.6+ds1-0ubuntu2")
	check("docker_compose_project: Docker Compose 2.24.6+ds1-0ubuntu2 is installed, and MMX needs Docker Compose 2.26.1 or newer")
	f.set("compose-version", "2.26.1-4")
	f.set("registry/"+testImageKey, "sha256:web1 sha256:web-manifest1")
	if r := f.compose(noMutation(t), project, nil); r.Status != "succeeded" {
		t.Fatalf("%+v", r)
	}
	f.remove("compose-version")
	check("docker_compose_project: the Docker Compose plugin is not installed (the docker command has no compose command)")
	f.set("daemon-down", "")
	check("docker_compose_project: the Docker daemon does not answer (Cannot connect to the Docker daemon")
	setWorkerEnvironment(t, "PATH", t.TempDir())
	check("docker_compose_project: " + missingProgramError("docker").Error())
}

func TestDockerComposeProjectStoppedAndRemoved(t *testing.T) {
	t.Parallel()
	f := newDockerFixture(t)
	running := DockerComposeProject{ComposeProjectName: "shop", ComposeProjectDir: f.project, State: "running", PullImages: "when_missing", ContainersOfRemovedServices: "remove"}
	// A removal that deletes named volumes names its project: shop, the name
	// that Docker Compose gives f.project, whose last part is shop.
	in := func(state string) DockerComposeProject {
		p := DockerComposeProject{ComposeProjectName: "shop", ComposeProjectDir: f.project, State: state}
		if state == composeProjectContainersVolumesRemoved {
			p.ComposeProjectName = "shop"
		}
		return p
	}
	f.shop("service web "+testImage+" web-1", "service database "+testDatabaseImage+" database-1", "volume database_data shop_database_data", "volume shared shared_data external")
	f.set("registry/"+testImageKey, "sha256:web1 sha256:web-manifest1")
	f.set("registry/"+testDatabaseImageKey, "sha256:database1 sha256:database-manifest1")
	f.set("volumes/shared_data", "")
	start := func() {
		t.Helper()
		if r := f.compose(Runtime{}, running, nil); r.Status != "succeeded" {
			t.Fatalf("%+v", r)
		}
	}
	start()

	// Stopped: the containers stay, and running starts them again.
	r := f.compose(noMutation(t), in("stopped"), nil)
	if r.Change != "would_change" || !reflect.DeepEqual(composeServices(t, r), []string{"database would_stop: the container runs", "web would_stop: the container runs"}) {
		t.Fatalf("%+v", r)
	}
	r = f.compose(Runtime{}, in("stopped"), nil)
	if r.Change != "changed" || !reflect.DeepEqual(composeServices(t, r), []string{"database stopped", "web stopped"}) || !reflect.DeepEqual(f.containers(), []string{"shop-database-2 exited", "shop-web-1 exited"}) {
		t.Fatalf("%+v %v", r, f.containers())
	}
	for _, run := range []Runtime{{}, noMutation(t)} {
		if r := f.compose(run, in("stopped"), nil); r.Change != "unchanged" || !reflect.DeepEqual(composeServices(t, r), []string{"database unchanged", "web unchanged"}) {
			t.Fatalf("%+v", r)
		}
	}
	if r := f.compose(Runtime{}, running, nil); !reflect.DeepEqual(composeServices(t, r), []string{"database started", "web started"}) {
		t.Fatalf("%+v", r)
	}

	// Removed with the named volumes kept. A service that left the Compose
	// file is removed too.
	f.shop("service web "+testImage+" web-1", "volume database_data shop_database_data", "volume shared shared_data external")
	r = f.compose(noMutation(t), in("containers_removed_named_volumes_kept"), nil)
	if r.Change != "would_change" || !reflect.DeepEqual(composeServices(t, r), []string{"database would_remove: the service has containers", "web would_remove: the service has containers"}) || len(f.containers()) != 2 {
		t.Fatalf("%+v", r)
	}
	r = f.compose(Runtime{}, in("containers_removed_named_volumes_kept"), nil)
	if r.Change != "changed" || !reflect.DeepEqual(composeServices(t, r), []string{"database removed", "web removed"}) || f.containers() != nil {
		t.Fatalf("%+v %v", r, f.containers())
	}
	if _, err := os.Stat(filepath.Join(f.dir, "volumes/shop_database_data")); err != nil {
		t.Fatal("the named volume is gone", err)
	}
	for _, run := range []Runtime{{}, noMutation(t)} {
		for _, state := range []string{"stopped", "containers_removed_named_volumes_kept"} {
			if r := f.compose(run, in(state), nil); r.Status != "succeeded" || r.Change != "unchanged" || len(composeServices(t, r)) != 0 {
				t.Fatalf("%+v", r)
			}
		}
	}

	// Removed with the named volumes: the volumes the Compose file declares
	// go, also when the containers are gone already; an external one stays.
	r = f.compose(noMutation(t), in("containers_and_named_volumes_removed"), nil)
	if r.Change != "would_change" || r.Reason != "would delete the named volumes shop_database_data with the data in them" {
		t.Fatalf("%+v", r)
	}
	r = f.compose(Runtime{}, in("containers_and_named_volumes_removed"), nil)
	if r.Status != "succeeded" || r.Change != "changed" || r.Reason != "deleted the named volumes shop_database_data with the data in them" {
		t.Fatalf("%+v", r)
	}
	if left, _ := os.ReadDir(filepath.Join(f.dir, "volumes")); len(left) != 1 || left[0].Name() != "shared_data" {
		t.Fatal(left)
	}
	for _, run := range []Runtime{{}, noMutation(t)} {
		if r := f.compose(run, in("containers_and_named_volumes_removed"), nil); r.Change != "unchanged" {
			t.Fatalf("%+v", r)
		}
	}

	// With containers and volumes at once.
	start()
	r = f.compose(Runtime{}, in("containers_and_named_volumes_removed"), nil)
	if r.Change != "changed" || !reflect.DeepEqual(composeServices(t, r), []string{"web removed"}) || f.containers() != nil {
		t.Fatalf("%+v", r)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "volumes/shop_database_data")); err == nil {
		t.Fatal("the named volume stayed")
	}

	// The project is found without its Compose file and without its
	// directory, and a removal that is run again reports unchanged.
	start()
	if err := os.Remove(filepath.Join(f.project, "compose.yaml")); err != nil {
		t.Fatal(err)
	}
	if r := f.compose(Runtime{}, in("stopped"), nil); !reflect.DeepEqual(composeServices(t, r), []string{"web stopped"}) {
		t.Fatalf("%+v", r)
	}
	if err := os.Remove(f.project); err != nil {
		t.Fatal(err)
	}
	r = f.compose(Runtime{}, in("containers_and_named_volumes_removed"), nil)
	if r.Change != "changed" || !reflect.DeepEqual(composeServices(t, r), []string{"web removed"}) || f.containers() != nil {
		t.Fatalf("%+v", r)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "volumes/shop_database_data")); err == nil {
		t.Fatal("the named volume stayed")
	}
	for _, state := range dockerComposeProjectStates[1:] {
		if r := f.compose(Runtime{}, in(state), nil); r.Status != "succeeded" || r.Change != "unchanged" {
			t.Fatalf("%+v", r)
		}
	}
}

// Docker Compose acts on a project by its name and names a project after
// the last part of its directory's path. Two directories with the same last
// part are one project to Compose: a task on one of them would remove the
// other's containers and volumes. It fails instead and changes nothing.
func TestDockerComposeProjectsOfTwoDirectoriesWithOneName(t *testing.T) {
	t.Parallel()
	f := newDockerFixture(t)
	other := filepath.Join(f.dir, "blog", "shop")
	if err := os.MkdirAll(other, 0755); err != nil {
		t.Fatal(err)
	}
	f.shop("service web "+testImage+" web-1", "volume data shop_data")
	writeState(t, filepath.Join(other, "compose.yaml"), "service web "+testImage+" web-1\nvolume data shop_data\n")
	f.set("registry/"+testImageKey, "sha256:web1 sha256:web-manifest1")
	first := DockerComposeProject{ComposeProjectName: "shop", ComposeProjectDir: f.project, State: "running", PullImages: "when_missing", ContainersOfRemovedServices: "remove"}
	if r := f.compose(Runtime{}, first, nil); r.Change != "changed" {
		t.Fatalf("%+v", r)
	}
	containers := f.containers()
	untouched := func(r protocol.Result, why string) {
		t.Helper()
		want := "docker_compose_project: the project of " + other + " and the project of " + f.project + " have the same project name, shop (" + why + "; the container shop-web-1 belongs to the second). Docker Compose would treat the containers and volumes of both as one project's. Give each project its own compose_project_name; MMX changed nothing"
		if r.Status != "failed" || r.FailureKind != "config" || r.Reason != want {
			t.Fatalf("%+v", r)
		}
		if _, err := os.Stat(filepath.Join(f.dir, "volumes/shop_data")); err != nil || !reflect.DeepEqual(f.containers(), containers) {
			t.Fatal("the first project was touched", f.containers(), err)
		}
	}
	second := DockerComposeProject{ComposeProjectName: "shop", ComposeProjectDir: other, State: "running", PullImages: "when_missing", ContainersOfRemovedServices: "remove"}
	takeCalls(t, f.dir)
	for _, run := range []Runtime{{}, noMutation(t)} {
		untouched(f.compose(run, second, nil), "the task's compose_project_name")
		// The removal with volumes would delete the first project's volume,
		// which has the name that the second project's files declare. It
		// names the project, and the name is the first project's.
		untouched(f.compose(run, DockerComposeProject{ComposeProjectDir: other, ComposeProjectName: "shop", State: "containers_and_named_volumes_removed"}, nil), "the task's compose_project_name")
	}
	for _, call := range takeCalls(t, f.dir) {
		if strings.Contains(call, " up ") || strings.Contains(call, " down") || strings.Contains(call, " stop") {
			t.Fatal("a command acted on the shared name:", call)
		}
	}
	// A removal that has nothing to remove runs no command and so reports
	// unchanged, as a removal that is run again must.
	for _, state := range []string{"stopped", "containers_removed_named_volumes_kept"} {
		if r := f.compose(Runtime{}, DockerComposeProject{ComposeProjectName: "shop", ComposeProjectDir: other, State: state}, nil); r.Status != "succeeded" || r.Change != "unchanged" {
			t.Fatalf("%s: %+v", state, r)
		}
	}
	// The same name through compose_project_name is the same mistake.
	second.ComposeProjectName = "shop"
	if r := f.compose(Runtime{}, second, nil); r.FailureKind != "config" || !strings.Contains(r.Reason, "have the same project name, shop (the task's compose_project_name; ") {
		t.Fatalf("%+v", r)
	}
	// With its own name the second project runs beside the first, with its
	// own volume, and each removal takes only its own project.
	second.ComposeProjectName = "blog-shop"
	writeState(t, filepath.Join(other, "compose.yaml"), "service web "+testImage+" web-1\nvolume data blog-shop_data\n")
	if r := f.compose(Runtime{}, second, nil); r.Status != "succeeded" || !reflect.DeepEqual(composeServices(t, r), []string{"web created"}) || len(f.containers()) != 2 {
		t.Fatalf("%+v %v", r, f.containers())
	}
	for _, run := range []Runtime{noMutation(t), {}} {
		if r := f.compose(run, first, nil); r.Change != "unchanged" {
			t.Fatalf("%+v", r)
		}
	}
	r := f.compose(Runtime{}, DockerComposeProject{ComposeProjectDir: other, ComposeProjectName: "blog-shop", State: "containers_and_named_volumes_removed"}, nil)
	if r.Change != "changed" || r.Reason != "deleted the named volumes blog-shop_data with the data in them" || !reflect.DeepEqual(f.containers(), containers) {
		t.Fatalf("%+v %v", r, f.containers())
	}
	if _, err := os.Stat(filepath.Join(f.dir, "volumes/shop_data")); err != nil {
		t.Fatal("the first project's volume is gone", err)
	}
}

// A removal must not depend on files that Compose can still load, and it
// names every volume that it deletes.
func TestDockerComposeProjectRemovedWithoutFilesThatLoad(t *testing.T) {
	t.Parallel()
	f := newDockerFixture(t)
	running := DockerComposeProject{ComposeProjectName: "shop", ComposeProjectDir: f.project, State: "running", PullImages: "when_missing", ContainersOfRemovedServices: "remove"}
	// A removal that deletes named volumes names its project: shop, the name
	// that Docker Compose gives f.project, whose last part is shop.
	in := func(state string) DockerComposeProject {
		p := DockerComposeProject{ComposeProjectName: "shop", ComposeProjectDir: f.project, State: state}
		if state == composeProjectContainersVolumesRemoved {
			p.ComposeProjectName = "shop"
		}
		return p
	}
	f.shop("service web "+testImage+" web-1", "volume data shop_data", "volume shared shared_data external")
	f.set("registry/"+testImageKey, "sha256:web1 sha256:web-manifest1")
	f.set("volumes/shared_data", "")
	if r := f.compose(Runtime{}, running, nil); r.Status != "succeeded" {
		t.Fatalf("%+v", r)
	}
	// The Compose file exists and Compose cannot load it: an environment
	// file that it names is gone, as after a removal playbook that deleted
	// it first or a run that stopped halfway.
	f.set("files-do-not-load", "")
	notLoaded := "Docker Compose cannot load the project's files (docker compose --project-directory " + f.project + " --project-name shop --file " + f.project + "/compose.yaml config --format json: env file " + f.project + "/web.env not found: stat " + f.project + "/web.env: no such file or directory), so the project is removed by its name"
	// The task that deletes the volumes names the project, and so does its command.
	namedNotLoaded := notLoaded
	r := f.compose(noMutation(t), in("containers_removed_named_volumes_kept"), nil)
	if r.Change != "would_change" || !reflect.DeepEqual(composeServices(t, r), []string{"web would_remove: the service has containers"}) || r.Reason != notLoaded {
		t.Fatalf("%+v", r)
	}
	// With the volumes: the dryrun names the volume that Compose labelled
	// with the project's name, and the run names it when it is gone.
	r = f.compose(noMutation(t), in("containers_and_named_volumes_removed"), nil)
	if r.Change != "would_change" || r.Reason != "would delete the named volumes shop_data with the data in them; "+namedNotLoaded || len(f.containers()) != 1 {
		t.Fatalf("%+v", r)
	}
	r = f.compose(Runtime{}, in("containers_and_named_volumes_removed"), nil)
	if r.Status != "succeeded" || r.Change != "changed" || !reflect.DeepEqual(composeServices(t, r), []string{"web removed"}) || r.Reason != "deleted the named volumes shop_data with the data in them; "+namedNotLoaded || f.containers() != nil {
		t.Fatalf("%+v %v", r, f.containers())
	}
	if left, _ := os.ReadDir(filepath.Join(f.dir, "volumes")); len(left) != 1 || left[0].Name() != "shared_data" {
		t.Fatal(left)
	}
	// Run again: nothing is left that MMX can find, and the reason says
	// what it could not look for.
	for _, run := range []Runtime{{}, noMutation(t)} {
		r := f.compose(run, in("containers_and_named_volumes_removed"), nil)
		if r.Status != "succeeded" || r.Change != "unchanged" || !strings.HasPrefix(r.Reason, "the project has no containers, and MMX cannot look for the named volumes that its files declare: Docker Compose cannot load the project's files (") {
			t.Fatalf("%+v", r)
		}
		if r := f.compose(run, in("containers_removed_named_volumes_kept"), nil); r.Change != "unchanged" || r.Reason != "" {
			t.Fatalf("%+v", r)
		}
	}
	f.remove("files-do-not-load")

	// Without the Compose file: the volumes with the project's label go, and
	// the dryrun and the run name them.
	if r := f.compose(Runtime{}, running, nil); r.Status != "succeeded" {
		t.Fatalf("%+v", r)
	}
	f.set("volumes/shop_uploads", "")
	if err := os.Remove(filepath.Join(f.project, "compose.yaml")); err != nil {
		t.Fatal(err)
	}
	r = f.compose(noMutation(t), in("containers_and_named_volumes_removed"), nil)
	if r.Change != "would_change" || r.Reason != "would delete the named volumes shop_data, shop_uploads with the data in them" {
		t.Fatalf("%+v", r)
	}
	r = f.compose(Runtime{}, in("containers_and_named_volumes_removed"), nil)
	if r.Change != "changed" || r.Reason != "deleted the named volumes shop_data, shop_uploads with the data in them" || f.containers() != nil {
		t.Fatalf("%+v", r)
	}
	if r := f.compose(Runtime{}, in("containers_and_named_volumes_removed"), nil); r.Change != "unchanged" {
		t.Fatalf("%+v", r)
	}
}

// What a failed docker command printed on standard output never reaches a
// result: docker compose config prints the values of the environment there.
// And a daemon that stops between two commands is an error, not "the
// project has no containers".
func TestDockerComposeProjectFailuresOfDockerCommands(t *testing.T) {
	t.Parallel()
	f := newDockerFixture(t)
	running := DockerComposeProject{ComposeProjectName: "shop", ComposeProjectDir: f.project, State: "running", PullImages: "when_missing", ContainersOfRemovedServices: "remove"}
	f.shop("service web " + testImage + " value-of-the-environment")
	f.set("registry/"+testImageKey, "sha256:web1 sha256:web-manifest1")
	if r := f.compose(Runtime{}, running, nil); r.Status != "succeeded" {
		t.Fatalf("%+v", r)
	}
	f.set("config-prints-and-fails", "")
	for _, run := range []Runtime{{}, noMutation(t)} {
		r := f.compose(run, running, nil)
		if r.Status != "failed" || strings.Contains(r.Reason, "value-of-the-environment") || !strings.HasPrefix(r.Reason, "docker_compose_project: docker compose --project-directory "+f.project+" --project-name shop --file "+f.project+"/compose.yaml config --format json: ") {
			t.Fatalf("%+v", r)
		}
	}
	f.remove("config-prints-and-fails")

	containers := f.containers()
	f.set("daemon-stops-before", "container")
	for _, state := range dockerComposeProjectStates {
		for _, run := range []Runtime{{}, noMutation(t)} {
			f.remove("daemon-down")
			project := DockerComposeProject{ComposeProjectName: "shop", ComposeProjectDir: f.project, State: state}
			switch state {
			case composeProjectRunning:
				project = running
			case composeProjectContainersVolumesRemoved:
				project.ComposeProjectName = "shop"
			}
			r := f.compose(run, project, nil)
			if r.Status != "failed" || !strings.HasPrefix(r.Reason, "docker_compose_project: docker container inspect ") || !strings.Contains(r.Reason, "Cannot connect to the Docker daemon") {
				t.Fatalf("%s: %+v", state, r)
			}
		}
	}
	f.remove("daemon-stops-before")
	f.remove("daemon-down")
	if !reflect.DeepEqual(f.containers(), containers) {
		t.Fatal(f.containers())
	}
}

func TestDockerComposeProjectEndsWithoutDockerOrCompose(t *testing.T) {
	t.Parallel()
	f := newDockerFixture(t)
	removed := DockerComposeProject{ComposeProjectName: "shop", ComposeProjectDir: f.project, State: "containers_removed_named_volumes_kept"}
	f.shop("service web " + testImage + " web-1")
	f.set("registry/"+testImageKey, "sha256:web1 sha256:web-manifest1")
	if r := f.compose(Runtime{}, DockerComposeProject{ComposeProjectName: "shop", ComposeProjectDir: f.project, State: "running", PullImages: "when_missing", ContainersOfRemovedServices: "remove"}, nil); r.Status != "succeeded" {
		t.Fatalf("%+v", r)
	}
	// Containers that only docker compose can end, and no Compose plugin.
	f.remove("compose-version")
	if r := f.compose(Runtime{}, removed, nil); r.Status != "failed" || r.FailureKind != "missing_prerequisite" || !strings.Contains(r.Reason, "the Docker Compose plugin is not installed") {
		t.Fatalf("%+v", r)
	}
	// A daemon that does not answer may still have the containers.
	f.set("daemon-down", "")
	if r := f.compose(Runtime{}, removed, nil); r.Status != "failed" || r.FailureKind != "missing_prerequisite" || !strings.Contains(r.Reason, "the Docker daemon does not answer") {
		t.Fatalf("%+v", r)
	}
	f.remove("daemon-down")
	// No containers: nothing to end, with or without the plugin.
	f.remove("containers")
	if r := f.compose(Runtime{}, removed, nil); r.Status != "succeeded" || r.Change != "unchanged" {
		t.Fatalf("%+v", r)
	}
	// Without Docker, a project has no containers.
	setWorkerEnvironment(t, "PATH", t.TempDir())
	for _, run := range []Runtime{{}, noMutation(t)} {
		if r := f.compose(run, removed, nil); r.Status != "succeeded" || r.Change != "unchanged" || r.Reason != "the node has no docker command, so the project has no containers" {
			t.Fatalf("%+v", r)
		}
	}
}

// Every state names the project, including states that keep volumes.
func TestDockerComposeProjectRequiresNameInEveryState(t *testing.T) {
	t.Parallel()
	for _, state := range dockerComposeProjectStates {
		project := DockerComposeProject{ComposeProjectDir: "/opt/shop", State: state, PullImages: "when_missing", ContainersOfRemovedServices: "keep"}
		if state != composeProjectRunning {
			project.PullImages, project.ContainersOfRemovedServices = "", ""
		}
		if err := project.Prepare(""); err == nil || !strings.Contains(err.Error(), "compose_project_name is required in every state") {
			t.Fatal(state, err)
		}
		project.ComposeProjectName = "shop"
		if err := project.Prepare(""); err != nil {
			t.Fatal(state, err)
		}
	}
}

func TestDockerComposeProjectFilesStayInsideDirectory(t *testing.T) {
	t.Parallel()
	for path, accepted := range map[string]bool{"nested/compose.yaml": true, "nested/../compose.yaml": true, "../compose.yaml": false, "nested/../../compose.yaml": false, "/compose.yaml": false, "": false, ".": false} {
		project := DockerComposeProject{ComposeFiles: []string{path}}
		var arguments ArgumentList
		project.Arguments(&arguments)
		for _, argument := range arguments.List {
			if argument.Field == "compose_files[0]" && (argument.Rule(argument.Raw) == nil) != accepted {
				t.Fatal(path, accepted)
			}
		}
	}
	named := composeProject{p: DockerComposeProject{ComposeProjectName: "shop", ComposeProjectDir: "/opt/shop", ComposeFiles: []string{"nested/compose.yaml"}}}
	if got := named.files(); !reflect.DeepEqual(got, []string{"/opt/shop/nested/compose.yaml"}) {
		t.Fatal(got)
	}
}

func TestDockerComposeProjectRejectsInputs(t *testing.T) {
	t.Parallel()
	login := []DockerRegistryLogin{{Registry: "registry.example.com", RegistryUsername: "deploy", RegistryPassword: "x"}}
	for want, project := range map[string]DockerComposeProject{
		"docker_compose_project needs state: running, stopped, containers_removed_named_volumes_kept, containers_and_named_volumes_removed":                             {},
		"state must be running, stopped, containers_removed_named_volumes_kept, containers_and_named_volumes_removed":                                                   {State: "absent"},
		"state: running needs pull_images: when_missing or always; there is no default":                                                                                 {State: "running", ContainersOfRemovedServices: "remove"},
		"pull_images must be when_missing or always":                                                                                                                    {State: "running", PullImages: "never", ContainersOfRemovedServices: "remove"},
		"state: running needs containers_of_removed_services: remove or keep; there is no default":                                                                      {State: "running", PullImages: "always"},
		"containers_of_removed_services must be remove or keep":                                                                                                         {State: "running", PullImages: "always", ContainersOfRemovedServices: "stop"},
		"state: stopped accepts only compose_project_dir, compose_files, and compose_project_name; remove pull_images":                                                  {State: "stopped", PullImages: "always"},
		"state: containers_removed_named_volumes_kept accepts only compose_project_dir, compose_files, and compose_project_name; remove containers_of_removed_services": {State: "containers_removed_named_volumes_kept", ContainersOfRemovedServices: "remove"},
		"state: containers_and_named_volumes_removed accepts only compose_project_dir, compose_files, and compose_project_name; remove registry_logins":                 {State: "containers_and_named_volumes_removed", RegistryLogins: login},
		"compose_files needs at least one file name; omit compose_files to use compose.yaml":                                                                            {State: "stopped", ComposeFiles: []string{}},
		"compose_files lists compose.yaml twice":                                                                                                                        {State: "stopped", ComposeFiles: []string{"compose.yaml", "compose.yaml"}},
		"each entry in registry_logins needs registry, registry_username, and registry_password":                                                                        {State: "running", PullImages: "always", ContainersOfRemovedServices: "keep", RegistryLogins: []DockerRegistryLogin{{Registry: "registry.example.com", RegistryUsername: "deploy"}}},
		"registry_logins names the registry registry.example.com twice":                                                                                                 {State: "running", PullImages: "always", ContainersOfRemovedServices: "keep", RegistryLogins: append(login, login...)},
	} {
		project.ComposeProjectName = "shop"
		if err := project.Prepare(""); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatal(want, err)
		}
	}
	// The rules of the inputs that may hold a reference.
	project := DockerComposeProject{ComposeProjectDir: "shop", State: "running", ComposeFiles: []string{"../compose.yaml"}, ComposeProjectName: "Shop", RegistryLogins: []DockerRegistryLogin{{Registry: "https://registry.example.com", RegistryUsername: "u", RegistryPassword: "p"}}}
	var arguments ArgumentList
	project.Arguments(&arguments)
	want := map[string]string{
		"compose_project_dir":         "path must be absolute",
		"compose_files[0]":            "compose_files must be relative paths inside compose_project_dir",
		"compose_project_name":        "compose_project_name must use lowercase letters, digits, - and _, and start with a letter or digit",
		"registry_logins[0].registry": "registry must be a registry's host name with an optional port",
	}
	for _, argument := range arguments.List {
		message, rejected := want[argument.Field]
		err := argument.Rule(argument.Raw)
		if rejected != (err != nil) || (rejected && !strings.Contains(err.Error(), message)) {
			t.Fatal(argument.Field, err)
		}
		delete(want, argument.Field)
	}
	if len(want) != 0 {
		t.Fatal("no argument for", want)
	}
	// A project with other file names and its own name.
	named := composeProject{p: DockerComposeProject{ComposeProjectDir: "/opt/shop", ComposeFiles: []string{"base.yaml", "production.yaml"}, ComposeProjectName: "shop-production"}}
	if got := named.withFiles("config"); !reflect.DeepEqual(got, []string{"compose", "--project-directory", "/opt/shop", "--project-name", "shop-production", "--file", "/opt/shop/base.yaml", "--file", "/opt/shop/production.yaml", "config"}) {
		t.Fatal(got)
	}
	if labels := named.labels("ignored"); !reflect.DeepEqual(labels, map[string]string{composeLabelProjectDirectory: "/opt/shop", composeLabelProject: "shop-production"}) {
		t.Fatal(labels)
	}
}
