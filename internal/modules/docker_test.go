package modules

import (
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mmx/internal/value"
)

// A stand-in docker keeps a node's Docker in files below $dir:
//
//	images/<reference>      "image-ID digest" of an image on the node
//	registry/<reference>    the same for what the registry serves, with the
//	                        word "list" at the end when the reference names a
//	                        list of images for several platforms and the
//	                        digest is the list's; a file <reference>.login
//	                        holds the login it needs, as docker's
//	                        configuration file writes it
//	platform/<reference>    the ID of the image for the node's platform, which
//	                        Compose compares with a container's instead of
//	                        the image ID when the file exists, as Compose 5
//	                        does in the containerd image store
//	containers/<id>/        one file for each property of a container
//	volumes/<name>          a named volume
//	compose-version         the Compose plugin's version; no file, no plugin
//	containerd              the daemon uses the containerd image store
//	daemon-down, registry-down, fail-up   make the matching command fail
//	dry-run-prints          what docker compose --dry-run up prints on
//	                        standard error, in place of its own lines
//	dry-run-fails           the same, and the dry run fails
//	daemon-stops-before     a docker command, such as "container": the daemon
//	                        stops answering when that command is called
//	pull-fails-saying       a pull fails with this text and the text of the
//	                        login file that docker was given; the daemon
//	                        refuses a question about an image with this text
//	                        and the login header that it was sent
//	files-do-not-load       docker compose fails to load the project's files,
//	                        as it does when an environment file is missing
//	config-prints-and-fails docker compose config prints the definition and
//	                        fails without a word on standard error
//
// In a reference's file name, "/", ":", and "@" are "_". A Compose file of
// the stand-in has one line for each service, "service <name> <image>
// <definition hash> [<containers> [build]]", and for each named volume,
// "volume <key> <name> [external]"; the word "invalid" makes it invalid.
// docker compose --dry-run up prints its steps as Compose 2 does, or as
// Compose 5 does when compose-version names version 5; Compose 2.26 also
// lists a container that does not run.
// Every call appends its environment to $dir/environment.
const fakeDocker = `
env >> "$dir/environment"
key() { printf '%s' "$1" | tr '/:@' '___'; }
# field prints the one line that the stand-in keeps of a container's field,
# with the shell's own read: cat would be one more program start for each.
field() { IFS= read -r REPLY < "$1"; printf '%s' "$REPLY"; }
# last_part prints the last part of a path, as basename does, without
# starting a program.
last_part() { set -- "${1%/}"; printf '%s\n' "${1##*/}"; }
tick() { n=$(($(cat "$dir/tick" 2>/dev/null || echo 0)+1)); echo $n > "$dir/tick"; echo $n; }
[ "$(cat "$dir/daemon-stops-before" 2>/dev/null)" != "$1" ] || touch "$dir/daemon-down"
daemon() { [ ! -e "$dir/daemon-down" ] || { echo "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?" >&2; return 1; }; }
pull_image() {
  k=$(key "$1")
  [ ! -e "$dir/registry-down" ] || { echo "Error response from daemon: Get \"https://registry.example.com/v2/\": dial tcp 192.0.2.1:443: connect: connection refused" >&2; return 1; }
  [ -f "$dir/registry/$k" ] || { echo "Error response from daemon: manifest for $1 not found: manifest unknown: manifest unknown" >&2; return 1; }
  [ ! -f "$dir/pull-fails-saying" ] || { echo "Error response from daemon: $(cat "$dir/pull-fails-saying") $(cat "${DOCKER_CONFIG:-/nonexistent}/config.json")" >&2; return 1; }
  if [ -f "$dir/registry/$k.login" ] && ! grep -q "$(cat "$dir/registry/$k.login")" "${DOCKER_CONFIG:-/nonexistent}/config.json" 2>/dev/null; then
    echo "Error response from daemon: pull access denied for $1, repository does not exist or may require 'docker login'" >&2; return 1
  fi
  mkdir -p "$dir/images"; cp "$dir/registry/$k" "$dir/images/$k"
}
container_json() {
  c="$dir/containers/$1"; health=null
  check=; [ "$(field "$c/health")" = none ] || check=',"Healthcheck":{"Test":["CMD","true"]}'
  [ "$(field "$c/health")" = none ] || [ "$(field "$c/health")" = created ] || health="{\"Status\":\"$(field "$c/health")\",\"Log\":[{\"ExitCode\":1,\"Output\":\"old\"},{\"ExitCode\":$(field "$c/health-exit-code"),\"Output\":\"$(field "$c/health-output")\"}]}"
  printf '{"Id":"%s","Name":"/%s","Image":"%s","State":{"Status":"%s","ExitCode":%s,"StartedAt":"%s","Health":%s},"HostConfig":{"RestartPolicy":{"Name":"%s"}},"Config":{"Image":"%s"%s,"Labels":{"com.docker.compose.project":"%s","com.docker.compose.project.working_dir":"%s","com.docker.compose.service":"%s","com.docker.compose.config-hash":"%s","com.docker.compose.image":"%s","com.docker.compose.oneoff":"%s","com.docker.compose.version":"%s"}}}' \
    "$1" "$(field "$c/name")" "$(field "$c/image")" "$(field "$c/status")" "$(field "$c/exit-code")" "$(field "$c/started")" "$health" "$(field "$c/restart-policy")" "$(field "$c/reference")" "$check" \
    "$(field "$c/project")" "$(field "$c/directory")" "$(field "$c/service")" "$(field "$c/hash")" "$(field "$c/image-label")" "$(field "$c/one-off")" "$(field "$c/made-by")"
}
new_container() { # project directory service image-ID definition-hash image-reference
  t=$(tick); c="$dir/containers/$(printf '%064d' $t)"; mkdir -p "$c"
  echo "$1-$3-$t" > "$c/name"; echo "$1" > "$c/project"; echo "$2" > "$c/directory"; echo "$3" > "$c/service"
  echo "$4" > "$c/image"; echo "$4" > "$c/image-label"; echo "$5" > "$c/hash"; echo "$6" > "$c/reference"
  echo running > "$c/status"; echo 0 > "$c/exit-code"; echo $t > "$c/started"; echo False > "$c/one-off"
  echo healthy > "$c/health"; echo 0 > "$c/health-exit-code"; echo ok > "$c/health-output"; echo no > "$c/restart-policy"
  sed 's/^v//' "$dir/compose-version" > "$c/made-by"
}
project_containers() { for c in "$dir"/containers/*/; do [ -d "$c" ] && [ "$(field "$c/project")" = "$1" ] && echo "${c%/}"; done; true; }
case "$1" in
info)
  plugins=
  [ ! -f "$dir/compose-version" ] || plugins="{\"Name\":\"compose\",\"Path\":\"$dir/docker-compose-plugin\",\"Version\":\"$(cat "$dir/compose-version")\"}"
  if ! daemon; then echo "{\"ServerVersion\":\"\",\"ClientInfo\":{\"Plugins\":[$plugins]}}"; exit 1; fi
  store=overlay2; [ ! -e "$dir/containerd" ] || store=io.containerd.snapshotter.v1
  echo "{\"ServerVersion\":\"29.9.0\",\"DriverStatus\":[[\"driver-type\",\"$store\"]],\"ClientInfo\":{\"Plugins\":[$plugins]}}";;
version) daemon || exit 1; echo linux/amd64;;
context) echo "unix://$(cat "$dir/socket")";;
image)
  daemon || { echo '[]'; exit 1; }
  k=$(key "$3")
  [ -f "$dir/images/$k" ] || { echo '[]'; echo "Error response from daemon: No such image: $3" >&2; exit 1; }
  read id digest kind < "$dir/images/$k"
  echo "[{\"Id\":\"$id\",\"RepoDigests\":[\"${3%%[:@]*}@$digest\"]}]";;
pull) daemon || exit 1; pull_image "$3" || exit 1; echo "$3";;
ps)
  daemon || exit 1
  shift 4
  for c in "$dir"/containers/*/; do
    [ -d "$c" ] || continue
    match=yes
    for filter; do
      case "$filter" in
      label=com.docker.compose.project.working_dir=*) [ "$(field "$c/directory")" = "${filter#label=com.docker.compose.project.working_dir=}" ] || match=no;;
      label=com.docker.compose.project=*) [ "$(field "$c/project")" = "${filter#label=com.docker.compose.project=}" ] || match=no;;
      esac
    done
    [ $match = no ] || last_part "$c"
  done;;
container)
  daemon || { echo '[]'; exit 1; }
  shift 2; missing=0; separator=; printf '['
  for wanted; do
    found=
    for c in "$dir"/containers/*/; do
      [ -d "$c" ] || continue
      if [ "$(last_part "$c")" = "$wanted" ] || [ "$(field "$c/name")" = "$wanted" ]; then found=$(last_part "$c"); fi
    done
    if [ -z "$found" ]; then missing=1; echo "Error response from daemon: No such container: $wanted" >&2; continue; fi
    printf '%s' "$separator"; container_json "$found"; separator=,
  done
  echo ']'; exit $missing;;
volume)
  daemon || { [ "$2" = ls ] || echo '[]'; exit 1; }
  if [ "$2" = ls ]; then
    project=${5#label=com.docker.compose.project=}
    for v in "$dir"/volumes/"$project"_*; do [ ! -e "$v" ] || last_part "$v"; done; exit 0
  fi
  shift 2; missing=0; separator=; printf '['
  for wanted; do
    if [ -e "$dir/volumes/$wanted" ]; then printf '%s{"Name":"%s"}' "$separator" "$wanted"; separator=,; else missing=1; fi
  done
  echo ']'; exit $missing;;
compose)
  daemon || exit 1
  shift; directory=; project=; files=; dry=no
  while :; do
    case "$1" in
    --project-directory) directory=$2; shift 2;;
    --project-name) project=$2; shift 2;;
    --file) files="$files $2"; shift 2;;
    --dry-run) dry=yes; shift;;
    --progress) shift 2;;
    *) break;;
    esac
  done
  [ -n "$project" ] || project=$(last_part "$directory")
  definition="$dir/definition"; : > "$definition"
  for f in $files; do
    cat "$f" >> "$definition" 2>/dev/null || { echo "open $f: no such file or directory" >&2; exit 14; }
  done
  if grep -q invalid "$definition"; then echo "validating $files: services.web Additional property invalid is not allowed" >&2; exit 15; fi
  if [ -n "$files" ] && [ -e "$dir/files-do-not-load" ]; then echo "env file $directory/web.env not found: stat $directory/web.env: no such file or directory" >&2; exit 14; fi
  command=$1; shift
  case "$command" in
  config)
    services=; volumes=
    while read kind name third fourth fifth sixth; do
      case "$kind" in
      service)
        entry="\"$name\":{\"image\":\"$third\",\"environment\":{\"DEFINITION\":\"$fourth\"},\"deploy\":{\"replicas\":${fifth:-1}}"
        [ "$sixth" != build ] || entry="$entry,\"build\":{\"context\":\"$directory\"}"
        services="$services${services:+,}$entry}";;
      volume)
        entry="\"$name\":{\"name\":\"$third\""
        [ "$fourth" != external ] || entry="$entry,\"external\":true"
        volumes="$volumes${volumes:+,}$entry}";;
      esac
    done < "$definition"
    echo "{\"name\":\"$project\",\"services\":{$services},\"volumes\":{$volumes}}"
    [ ! -e "$dir/config-prints-and-fails" ] || exit 1;;
  up)
    pull=missing; orphans=no
    while [ $# -gt 0 ]; do case "$1" in --pull) pull=$2; shift;; --remove-orphans) orphans=yes;; esac; shift; done
    if [ $dry = yes ]; then
      # Compose 2 writes the state of its build tool in a dry run, into the
      # account's home directory unless BUILDX_CONFIG names another place.
      case "$BUILDX_CONFIG" in "$dir"/private_task_directories/docker_compose_project-*) touch "$BUILDX_CONFIG/current";; *) echo "the dry run would write into the home directory" >&2; exit 1;; esac
      [ ! -e "$dir/dry-run-fails" ] || { cat "$dir/dry-run-fails" >&2; exit 1; }
      [ ! -e "$dir/dry-run-prints" ] || { cat "$dir/dry-run-prints" >&2; exit 0; }
      version=$(sed 's/^v//' "$dir/compose-version")
      step() { case "$version" in 5*) echo " $1 $2 $3 " >&2;; *) echo " DRY-RUN MODE -  $1 $2  $3" >&2;; esac; }
      echo "time=\"2026-10-09T02:15:45Z\" level=warning msg=\"$files: the attribute version is obsolete, it will be ignored, please remove it to avoid potential confusion\"" >&2
      [ -n "$(project_containers "$project")" ] || { step Network "${project}_default" Creating; step Network "${project}_default" Created; }
      while read kind name image hash wanted build; do
        [ "$kind" = service ] || continue
        k=$(key "$image"); id=an-image-that-the-node-does-not-have
        [ ! -f "$dir/images/$k" ] || read id digest kind < "$dir/images/$k"
        [ ! -f "$dir/platform/$k" ] || id=$(cat "$dir/platform/$k")
        have=0
        for c in $(project_containers "$project"); do
          [ "$(field "$c/service")" = "$name" ] || continue
          n=$(field "$c/name")
          if [ $have -ge ${wanted:-1} ]; then for s in Stopping Stopped Removing Removed; do step Container "$n" $s; done; continue; fi
          have=$((have+1))
          if [ "$(field "$c/hash")" != "$hash" ] || [ "$(field "$c/image-label")" != "$id" ]; then step Container "$n" Recreate; step Container "$n" Recreated; continue; fi
          if [ "$(field "$c/status")" = running ]; then step Container "$n" Running; else case "$version" in 2.26*) step Container "$n" Created;; esac; fi
        done
        while [ $have -lt ${wanted:-1} ]; do have=$((have+1)); step Container "$project-$name-$have" Creating; step Container "$project-$name-$have" Created; done
      done < "$definition"
      for c in $(project_containers "$project"); do
        grep -q "^service $(field "$c/service") " "$definition" && continue
        n=$(field "$c/name")
        if [ $orphans = yes ]; then for s in Stopping Stopped Removing Removed; do step Container "$n" $s; done
        else echo "time=\"2026-10-09T02:15:45Z\" level=warning msg=\"Found orphan containers ([$n]) for this project. If you removed or renamed this service in your compose file, you can run this command with the --remove-orphans flag to clean it up.\"" >&2; fi
      done
      while read kind key name rest; do [ "$kind" != volume ] || [ -e "$dir/volumes/$name" ] || { step Volume "\"$name\"" Creating; step Volume "\"$name\"" Created; }; done < "$definition"
      exit 0
    fi
    count=0
    while read kind name image hash wanted build; do
      [ "$kind" = service ] || continue
      count=$((count+1))
      k=$(key "$image")
      if [ "$build" != build ]; then
        if [ "$pull" = always ] || [ ! -f "$dir/images/$k" ]; then pull_image "$image" || exit 1; fi
      fi
      read id digest kind < "$dir/images/$k"
      compared=$id
      [ ! -f "$dir/platform/$k" ] || compared=$(cat "$dir/platform/$k")
      have=0
      for c in $(project_containers "$project"); do
        [ "$(field "$c/service")" = "$name" ] || continue
        if [ "$(field "$c/hash")" != "$hash" ] || [ "$(field "$c/image-label")" != "$compared" ] || [ $have -ge ${wanted:-1} ]; then rm -r "$c"; continue; fi
        have=$((have+1))
        [ "$(field "$c/status")" = running ] || { echo running > "$c/status"; tick > "$c/started"; }
      done
      while [ $have -lt ${wanted:-1} ]; do have=$((have+1)); new_container "$project" "$directory" "$name" "$id" "$hash" "$image"; done
      if [ $count -eq 1 ] && [ -e "$dir/fail-up" ]; then echo "Error response from daemon: driver failed programming external connectivity on endpoint $project-$name-1: Bind for 0.0.0.0:8080 failed: port is already allocated" >&2; exit 1; fi
    done < "$definition"
    for c in $(project_containers "$project"); do
      grep -q "^service $(field "$c/service") " "$definition" || [ $orphans = no ] || rm -r "$c"
    done
    while read kind key name rest; do [ "$kind" != volume ] || { mkdir -p "$dir/volumes"; touch "$dir/volumes/$name"; }; done < "$definition";;
  stop)
    for c in $(project_containers "$project"); do echo exited > "$c/status"; done;;
  down)
    orphans=no; volumes=no
    for option; do case "$option" in --remove-orphans) orphans=yes;; --volumes) volumes=yes;; esac; done
    for c in $(project_containers "$project"); do
      [ -z "$files" ] || [ $orphans = yes ] || grep -q "^service $(field "$c/service") " "$definition" || continue
      rm -r "$c"
    done
    if [ $volumes = yes ]; then
      if [ -n "$files" ]; then
        while read kind key name external; do [ "$kind" != volume ] || [ "$external" = external ] || rm -f "$dir/volumes/$name"; done < "$definition"
      else
        rm -f "$dir"/volumes/"$project"_*
      fi
    fi;;
  *) echo "unexpected docker compose $command" >&2; exit 64;;
  esac;;
*) echo "unexpected docker $*" >&2; exit 64;;
esac`

// dockerFixture is a node with the stand-in docker: its state directory, a
// project directory, and the stand-in daemon that answers which image a
// registry names.
type dockerFixture struct {
	t       *testing.T
	dir     string
	project string
	// privateTaskDirectoryRoot is where a task's private directory goes, as
	// the node hands it to a task.
	privateTaskDirectoryRoot string
	// asked lists the references the daemon was asked about.
	asked []string
}

const (
	testImage    = "registry.example.com/shop/web:2.4"
	testImageKey = "registry.example.com_shop_web_2.4"
)

func newDockerFixture(t *testing.T) *dockerFixture {
	t.Helper()
	f := &dockerFixture{t: t, dir: standInPrograms(t, map[string]string{"docker": fakeDocker})}
	f.project = filepath.Join(f.dir, "shop")
	if err := os.Mkdir(f.project, 0755); err != nil {
		t.Fatal(err)
	}
	f.set("compose-version", "v2.40.0")
	f.privateTaskDirectoryRoot = filepath.Join(f.dir, "private_task_directories")
	if err := os.Mkdir(f.privateTaskDirectoryRoot, 0711); err != nil {
		t.Fatal(err)
	}
	// The daemon's socket: a Unix socket's path is short, so it has a
	// directory of its own.
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
	f.set("socket", socket)
	server := &http.Server{Handler: http.HandlerFunc(f.daemon)}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	return f
}

// daemon answers GET /distribution/<reference>/json from the registry files
// of the stand-in.
func (f *dockerFixture) daemon(w http.ResponseWriter, r *http.Request) {
	reference := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/distribution/"), "/json")
	f.asked = append(f.asked, reference)
	key := strings.NewReplacer("/", "_", ":", "_", "@", "_").Replace(reference)
	answer := func(status int, body any) {
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(body)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "registry-down")); err == nil {
		answer(500, map[string]string{"message": `Get "https://registry.example.com/v2/": dial tcp 192.0.2.1:443: connect: connection refused`})
		return
	}
	served, err := os.ReadFile(filepath.Join(f.dir, "registry", key))
	if err != nil {
		answer(404, map[string]string{"message": "manifest unknown: manifest unknown"})
		return
	}
	if saying, err := os.ReadFile(filepath.Join(f.dir, "pull-fails-saying")); err == nil {
		answer(401, map[string]string{"message": string(saying) + " " + r.Header.Get("X-Registry-Auth")})
		return
	}
	if needed, err := os.ReadFile(filepath.Join(f.dir, "registry", key+".login")); err == nil {
		var login struct{ Username, Password string }
		header, _ := base64.URLEncoding.DecodeString(r.Header.Get("X-Registry-Auth"))
		json.Unmarshal(header, &login)
		if base64.StdEncoding.EncodeToString([]byte(login.Username+":"+login.Password)) != strings.TrimSpace(string(needed)) {
			answer(401, map[string]string{"message": "unauthorized: authentication required"})
			return
		}
	}
	// The media type says whether the reference names one image or a list
	// of images for several platforms.
	fields := strings.Fields(string(served))
	mediaType := "application/vnd.oci.image.manifest.v1+json"
	if len(fields) > 2 && fields[2] == "list" {
		mediaType = "application/vnd.oci.image.index.v1+json"
	}
	answer(200, map[string]any{"Descriptor": map[string]string{"mediaType": mediaType, "digest": fields[1]}})
}

// withPrivateTaskDirectoryRoot is run as the node task execution engine
// hands it to a module: it says where a task's private directory goes.
func (f *dockerFixture) withPrivateTaskDirectoryRoot(run Runtime) Runtime {
	return withPrivateTaskDirectoryRoot(run, f.privateTaskDirectoryRoot)
}

func (f *dockerFixture) set(name, contents string) {
	f.t.Helper()
	writeState(f.t, filepath.Join(f.dir, name), contents)
}

func (f *dockerFixture) remove(name string) {
	f.t.Helper()
	if err := os.RemoveAll(filepath.Join(f.dir, name)); err != nil {
		f.t.Fatal(err)
	}
}

func (f *dockerFixture) get(name string) string {
	f.t.Helper()
	return strings.TrimSpace(readState(f.t, filepath.Join(f.dir, name)))
}

// noSecret fails the test when text holds the password.
func (f *dockerFixture) noSecretOnCommandLinesOrInEnvironments(password string) {
	f.t.Helper()
	for _, name := range []string{"calls", "environment"} {
		if strings.Contains(readState(f.t, filepath.Join(f.dir, name)), password) {
			f.t.Fatalf("the password is in the stand-in's %s", name)
		}
	}
	if left, _ := os.ReadDir(f.privateTaskDirectoryRoot); len(left) != 0 {
		f.t.Fatalf("a private configuration directory is left: %v", left)
	}
}

func dockerValue(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("value is %#v", v)
	}
	return m
}

func TestDockerImage(t *testing.T) {
	t.Parallel()
	f := newDockerFixture(t)
	ctx := testContext(t)
	image := DockerImage{Image: testImage, PullImage: "when_missing"}
	if err := image.Prepare(""); err != nil {
		t.Fatal(err)
	}
	f.set("registry/"+testImageKey, "sha256:config1 sha256:manifest1")
	// A dryrun reports the missing image and pulls nothing.
	r := noMutation(t).DockerImage(ctx, image, value.Scope{})
	if r.Status != "succeeded" || r.Change != "would_change" || !reflect.DeepEqual(r.Value, map[string]any{"image_id": nil, "previous_image_id": nil}) || f.get("images/"+testImageKey) != "" {
		t.Fatalf("%+v", r)
	}
	if unknown := image.UnknownInADryrun(r); unknown["value"] == "" {
		t.Fatal("a dryrun that would pull must mark the image ID unknown", unknown)
	}
	if state, known := image.StateAfter(r); !known || state != stateWouldChange {
		t.Fatal(state, known)
	}
	// The first run pulls; a rerun and a dryrun then change nothing.
	if r := (Runtime{}).DockerImage(ctx, image, value.Scope{}); r.Change != "changed" || !reflect.DeepEqual(r.Value, map[string]any{"image_id": "sha256:config1", "previous_image_id": nil}) {
		t.Fatalf("%+v", r)
	}
	takeCalls(t, f.dir)
	for _, run := range []Runtime{{}, noMutation(t)} {
		r := run.DockerImage(ctx, image, value.Scope{})
		if r.Change != "unchanged" || !reflect.DeepEqual(r.Value, map[string]any{"image_id": "sha256:config1", "previous_image_id": "sha256:config1"}) {
			t.Fatalf("%+v", r)
		}
	}
	if calls := changing(takeCalls(t, f.dir), "docker info", "docker image inspect"); calls != nil {
		t.Fatal(calls)
	}
	if state, err := image.ReadState(ctx, nil); err != nil || state != "sha256:config1" {
		t.Fatal(state, err)
	}
	// The tag moves. when_missing never follows it.
	f.set("registry/"+testImageKey, "sha256:config2 sha256:manifest2")
	if r := (Runtime{}).DockerImage(ctx, image, value.Scope{}); r.Change != "unchanged" {
		t.Fatalf("%+v", r)
	}
	// always asks the registry: a dryrun reports the change without a pull.
	// The classic image store learns the new image ID only with the pull.
	image.PullImage = "always"
	r = noMutation(t).DockerImage(ctx, image, value.Scope{})
	if r.Change != "would_change" || !reflect.DeepEqual(r.Value, map[string]any{"image_id": nil, "previous_image_id": "sha256:config1"}) || r.Reason != "the registry now serves "+testImage+" under the digest sha256:manifest2" {
		t.Fatalf("%+v", r)
	}
	if !reflect.DeepEqual(f.asked, []string{testImage}) || f.get("images/"+testImageKey) != "sha256:config1 sha256:manifest1" {
		t.Fatal(f.asked, f.get("images/"+testImageKey))
	}
	if r := (Runtime{}).DockerImage(ctx, image, value.Scope{}); r.Change != "changed" || !reflect.DeepEqual(r.Value, map[string]any{"image_id": "sha256:config2", "previous_image_id": "sha256:config1"}) {
		t.Fatalf("%+v", r)
	}
	for _, run := range []Runtime{{}, noMutation(t)} {
		if r := run.DockerImage(ctx, image, value.Scope{}); r.Change != "unchanged" {
			t.Fatalf("%+v", r)
		}
	}
	// In the containerd image store the image ID is the digest that the
	// registry names, so a dryrun knows both IDs.
	f.set("containerd", "")
	f.set("images/"+testImageKey, "sha256:manifest2 sha256:manifest2")
	f.set("registry/"+testImageKey, "sha256:manifest3 sha256:manifest3")
	r = noMutation(t).DockerImage(ctx, image, value.Scope{})
	if r.Change != "would_change" || !reflect.DeepEqual(r.Value, map[string]any{"image_id": "sha256:manifest3", "previous_image_id": "sha256:manifest2"}) || image.UnknownInADryrun(r) != nil {
		t.Fatalf("%+v", r)
	}
	if state, known := image.StateAfter(r); !known || state != "sha256:manifest3" {
		t.Fatal(state, known)
	}
	// A registry that does not answer: a dryrun cannot check, a run fails
	// and keeps the image, and the next try succeeds.
	f.set("registry-down", "")
	r = noMutation(t).DockerImage(ctx, image, value.Scope{})
	if r.Status != "not_checked" || !strings.Contains(r.Reason, "connection refused") {
		t.Fatalf("%+v", r)
	}
	// The node has the image, so nothing is predicted for what follows it.
	if state, known := image.StateAfter(r); known {
		t.Fatal(state, known)
	}
	if r := (Runtime{}).DockerImage(ctx, image, value.Scope{}); r.Status != "failed" || r.Change != "unchanged" || !strings.Contains(r.Reason, "docker pull --quiet "+testImage) || !strings.Contains(r.Reason, "connection refused") {
		t.Fatalf("%+v", r)
	}
	f.remove("registry-down")
	if r := (Runtime{}).DockerImage(ctx, image, value.Scope{}); r.Status != "succeeded" || r.Change != "changed" {
		t.Fatalf("%+v", r)
	}
	// A digest names one image forever: always has nothing to ask.
	pinned := DockerImage{Image: "registry.example.com/shop/web@sha256:" + strings.Repeat("ab", 32), PullImage: "always"}
	f.set("images/"+strings.NewReplacer("/", "_", ":", "_", "@", "_").Replace(pinned.Image), "sha256:config9 sha256:"+strings.Repeat("ab", 32))
	f.asked = nil
	if r := noMutation(t).DockerImage(ctx, pinned, value.Scope{}); r.Change != "unchanged" || f.asked != nil {
		t.Fatalf("%+v %v", r, f.asked)
	}
	// A node without that image: in the containerd image store the digest
	// is the image ID, so a dryrun reports it; in the classic store docker
	// learns the ID with the pull.
	f.remove("images/" + strings.NewReplacer("/", "_", ":", "_", "@", "_").Replace(pinned.Image))
	if r := noMutation(t).DockerImage(ctx, pinned, value.Scope{}); r.Change != "would_change" || !reflect.DeepEqual(r.Value, map[string]any{"image_id": "sha256:" + strings.Repeat("ab", 32), "previous_image_id": nil}) || f.asked != nil {
		t.Fatalf("%+v %v", r, f.asked)
	}
	f.remove("containerd")
	if r := noMutation(t).DockerImage(ctx, pinned, value.Scope{}); r.Change != "would_change" || !reflect.DeepEqual(r.Value, map[string]any{"image_id": nil, "previous_image_id": nil}) {
		t.Fatalf("%+v", r)
	}
}

// A reference may name a list of images, one for each platform, as Docker's
// official images do. The list's digest changes when the image for any
// platform changes, and a registry's answer does not say whether the image
// for the node's own platform is among them. A dryrun then claims no change
// that it cannot know.
func TestDockerImageWhoseReferenceNamesAListOfImages(t *testing.T) {
	t.Parallel()
	f := newDockerFixture(t)
	ctx := testContext(t)
	image := DockerImage{Image: testImage, PullImage: "always"}
	f.set("registry/"+testImageKey, "sha256:config1 sha256:list1 list")
	if r := (Runtime{}).DockerImage(ctx, image, value.Scope{}); r.Change != "changed" {
		t.Fatalf("%+v", r)
	}
	// The classic image store names an image by the image for the node's
	// platform. The list changes and that image stays: a pull changes
	// nothing, and the dryrun says that it cannot see which it is.
	f.set("registry/"+testImageKey, "sha256:config1 sha256:list2 list")
	notChecked := func() {
		t.Helper()
		r := noMutation(t).DockerImage(ctx, image, value.Scope{})
		want := "docker_image: the registry now serves another list of images for " + testImage + " (digest sha256:list2); a dryrun cannot tell whether the image for the node's platform changed: a run pulls the image, and the image on the node changes only if the platform's image changed"
		if r.Status != "not_checked" || r.Change != "unknown" || r.Reason != want || !reflect.DeepEqual(r.Value, map[string]any{"image_id": nil, "previous_image_id": "sha256:config1"}) {
			t.Fatalf("%+v", r)
		}
		// Nothing is predicted for a service that follows the image.
		if state, known := image.StateAfter(r); known || state != "" {
			t.Fatal(state, known)
		}
		if unknown := image.UnknownInADryrun(r); unknown != nil {
			t.Fatal(unknown)
		}
	}
	notChecked()
	if r := (Runtime{}).DockerImage(ctx, image, value.Scope{}); r.Status != "succeeded" || r.Change != "unchanged" {
		t.Fatalf("%+v", r)
	}
	if r := noMutation(t).DockerImage(ctx, image, value.Scope{}); r.Change != "unchanged" {
		t.Fatalf("%+v", r)
	}
	// The image for the node's platform changes with the list: the dryrun
	// sees the same as before, and the run pulls the new image.
	f.set("registry/"+testImageKey, "sha256:config2 sha256:list2 list")
	f.set("images/"+testImageKey, "sha256:config1 sha256:list1")
	notChecked()
	if r := (Runtime{}).DockerImage(ctx, image, value.Scope{}); r.Change != "changed" || !reflect.DeepEqual(r.Value, map[string]any{"image_id": "sha256:config2", "previous_image_id": "sha256:config1"}) {
		t.Fatalf("%+v", r)
	}
	// A reference that names one image: another digest is another image.
	f.set("registry/"+testImageKey, "sha256:config3 sha256:manifest3")
	if r := noMutation(t).DockerImage(ctx, image, value.Scope{}); r.Change != "would_change" || r.Reason != "the registry now serves "+testImage+" under the digest sha256:manifest3" {
		t.Fatalf("%+v", r)
	}
	// The containerd image store names an image by the digest that the
	// registry serves, so the ID of a list changes with the list, and the
	// dryrun knows the new one.
	f.set("containerd", "")
	f.set("images/"+testImageKey, "sha256:list2 sha256:list2")
	f.set("registry/"+testImageKey, "sha256:list3 sha256:list3 list")
	r := noMutation(t).DockerImage(ctx, image, value.Scope{})
	if r.Change != "would_change" || !reflect.DeepEqual(r.Value, map[string]any{"image_id": "sha256:list3", "previous_image_id": "sha256:list2"}) {
		t.Fatalf("%+v", r)
	}
	if state, known := image.StateAfter(r); !known || state != "sha256:list3" {
		t.Fatal(state, known)
	}
}

// docker inspect fails in the same way for an image that the node does not
// have and for a daemon that does not answer. A daemon that stops between
// two commands of a task is an error, never "the node has no such image".
func TestDockerInspectWhenTheDaemonStops(t *testing.T) {
	t.Parallel()
	f := newDockerFixture(t)
	ctx := testContext(t)
	f.set("registry/"+testImageKey, "sha256:config1 sha256:manifest1")
	f.set("images/"+testImageKey, "sha256:config1 sha256:manifest1")
	image := DockerImage{Image: testImage, PullImage: "when_missing"}
	if r := (Runtime{}).DockerImage(ctx, image, value.Scope{}); r.Change != "unchanged" {
		t.Fatalf("%+v", r)
	}
	f.set("daemon-stops-before", "image")
	takeCalls(t, f.dir)
	for _, run := range []Runtime{{}, noMutation(t)} {
		f.remove("daemon-down")
		r := run.DockerImage(ctx, image, value.Scope{})
		if r.Status != "failed" || !strings.HasPrefix(r.Reason, "docker_image: docker image inspect "+testImage+": Cannot connect to the Docker daemon") {
			t.Fatalf("%+v", r)
		}
	}
	if calls := changing(takeCalls(t, f.dir), "docker info", "docker image inspect", "docker version"); calls != nil {
		t.Fatal("the task went on after the daemon stopped:", calls)
	}
	// An image that the node does not have, with a daemon that answers.
	f.remove("daemon-stops-before")
	f.remove("daemon-down")
	f.remove("images/" + testImageKey)
	if r := noMutation(t).DockerImage(ctx, image, value.Scope{}); r.Status != "succeeded" || r.Change != "would_change" {
		t.Fatalf("%+v", r)
	}
}

func TestDockerImageLogin(t *testing.T) {
	t.Parallel()
	f := newDockerFixture(t)
	ctx := testContext(t)
	const password = "sentinel-password-4711"
	login := base64.StdEncoding.EncodeToString([]byte("deploy:" + password))
	f.set("registry/"+testImageKey, "sha256:config1 sha256:manifest1")
	f.set("registry/"+testImageKey+".login", login)
	scope := value.Scope{"secrets": map[string]any{"registry_password": password}}
	image := DockerImage{Image: testImage, PullImage: "always"}
	// Without the login, a dryrun cannot check and a run fails.
	if r := noMutation(t).DockerImage(ctx, image, scope); r.Status != "not_checked" || r.Reason != "docker_image: the registry does not say which image "+testImage+" names: unauthorized: authentication required. If the image is private, a dryrun needs the registry login from registry_username and registry_password on the node task" {
		t.Fatalf("%+v", r)
	}
	if r := (Runtime{}).DockerImage(ctx, image, scope); r.Status != "failed" || !strings.Contains(r.Reason, "pull access denied") {
		t.Fatalf("%+v", r)
	}
	// With it, the dryrun asks and the run pulls; the password is on no
	// command line and in no environment, and its directory is gone.
	image.RegistryUsername, image.RegistryPassword = "deploy", "{{ .secrets.registry_password }}"
	if err := image.Prepare(""); err != nil {
		t.Fatal(err)
	}
	if r := noMutation(t).DockerImage(ctx, image, scope); r.Status != "succeeded" || r.Change != "would_change" {
		t.Fatalf("%+v", r)
	}
	// The dryrun asked the registry with the login and wrote it nowhere: no
	// docker command of it ran with a private configuration directory.
	if strings.Contains(readState(t, filepath.Join(f.dir, "environment")), "DOCKER_CONFIG=") {
		t.Fatal("a dryrun gave docker a private configuration directory")
	}
	r := f.withPrivateTaskDirectoryRoot(Runtime{}).DockerImage(ctx, image, scope)
	if r.Status != "succeeded" || r.Change != "changed" {
		t.Fatalf("%+v", r)
	}
	if !strings.Contains(readState(t, filepath.Join(f.dir, "environment")), "DOCKER_CONFIG="+f.privateTaskDirectoryRoot+"/docker_image-") {
		t.Fatal("the run did not give docker the private configuration directory")
	}
	// A wrong login on the task: the dryrun says to check it.
	wrong := value.Scope{"secrets": map[string]any{"registry_password": "wrong-" + password}}
	f.set("registry/"+testImageKey, "sha256:config2 sha256:manifest2")
	if r := noMutation(t).DockerImage(ctx, image, wrong); r.Status != "not_checked" || !strings.HasSuffix(r.Reason, "unauthorized: authentication required. Check the login that the task gives for the registry") {
		t.Fatalf("%+v", r)
	}
	f.set("registry/"+testImageKey, "sha256:config1 sha256:manifest1")
	if text, _ := json.Marshal(r); strings.Contains(string(text), password) {
		t.Fatal("the result holds the password")
	}
	f.noSecretOnCommandLinesOrInEnvironments(password)
	// A wrong password fails the run, and the directory is removed again.
	f.remove("images/" + testImageKey)
	scope = value.Scope{"secrets": map[string]any{"registry_password": "wrong-" + password}}
	if r := f.withPrivateTaskDirectoryRoot(Runtime{}).DockerImage(ctx, image, scope); r.Status != "failed" || !strings.Contains(r.Reason, "pull access denied") || strings.Contains(r.Reason, password) {
		t.Fatalf("%+v", r)
	}
	f.noSecretOnCommandLinesOrInEnvironments(password)
	// The login of a reference without a registry is Docker Hub's.
	if logins, err := (DockerImage{Image: "postgres:17.2", RegistryUsername: "u", RegistryPassword: "p"}).logins(value.Scope{}); err != nil || logins[0].Registry != dockerHubLoginName {
		t.Fatal(logins, err)
	}
}

// docker_image hands its login only to docker. A secret in the login marks
// no trace, so the task's result shows. A failure that tells the login, as
// its plain text or in a form in which docker gets, keeps, or sends it,
// reports [redacted] in its place and keeps the rest of its reason.
func TestDockerImageLoginHandedOnlyToDockerLeavesTheResultReadable(t *testing.T) {
	t.Parallel()
	f := newDockerFixture(t)
	ctx := testContext(t)
	const user, password = "robot$shop+deploy", "sentinel p/w&4711"
	secrets := map[string]string{"registry_username": user, "registry_password": password}
	stored := base64.StdEncoding.EncodeToString([]byte(user + ":" + password))
	f.set("registry/"+testImageKey, "sha256:config1 sha256:manifest1")
	f.set("registry/"+testImageKey+".login", stored)
	image := DockerImage{Image: testImage, PullImage: "always", RegistryUsername: "{{ .secrets.registry_username }}", RegistryPassword: "{{ .secrets.registry_password }}"}
	run, dryrun := f.withPrivateTaskDirectoryRoot(Runtime{}), f.withPrivateTaskDirectoryRoot(noMutation(t))
	// A pull with the login: the result names the image.
	s, trace := taskScope(secrets)
	r := reportedByTheEngine(run.DockerImage(ctx, image, s), s)
	if r.Status != "succeeded" || r.Change != "changed" || !reflect.DeepEqual(r.Value, map[string]any{"image_id": "sha256:config1", "previous_image_id": nil}) || trace.Used {
		t.Fatalf("%+v, the login marked the trace: %v", r, trace.Used)
	}
	// docker refuses the pull and tells the login: in the address of the
	// registry's token service, as its plain text, and as the text of the
	// login file.
	f.set("registry/"+testImageKey, "sha256:config2 sha256:manifest2")
	f.set("pull-fails-saying", `Get "https://registry.example.com/token?account=robot%24shop%2Bdeploy": unauthorized: `+user+":"+password+" is not a login;")
	s, trace = taskScope(secrets)
	told := run.DockerImage(ctx, image, s)
	for _, form := range []string{user, password, "robot%24shop%2Bdeploy", stored} {
		if !strings.Contains(told.Reason, form) {
			t.Fatalf("the stand-in docker did not tell %q: %s", form, told.Reason)
		}
	}
	r = reportedByTheEngine(told, s)
	if want := "docker_image: docker pull --quiet " + testImage + `: Error response from daemon: Get "https://registry.example.com/token?account=[redacted]": unauthorized: [redacted]:[redacted] is not a login; {"auths":{"registry.example.com":{"auth":"[redacted]"}}}`; r.Status != "failed" || r.Reason != want || trace.Used {
		t.Fatalf("%+v\nwant reason %s", r, want)
	}
	// A dryrun asks Docker's daemon, which refuses and tells the header that
	// carried the login.
	s, trace = taskScope(secrets)
	told = dryrun.DockerImage(ctx, image, s)
	header := (registryLogin{Registry: "registry.example.com", Username: user, Password: password}).headerForDockerDaemon()
	if !strings.Contains(told.Reason, header) || !strings.Contains(told.Reason, password) {
		t.Fatalf("the stand-in daemon did not tell the login: %s", told.Reason)
	}
	r = reportedByTheEngine(told, s)
	if want := "docker_image: the registry does not say which image " + testImage + ` names: Get "https://registry.example.com/token?account=[redacted]": unauthorized: [redacted]:[redacted] is not a login; [redacted]. Check the login that the task gives for the registry`; r.Status != "not_checked" || r.Reason != want || trace.Used {
		t.Fatalf("%+v\nwant reason %s", r, want)
	}
	// A user name that is written in the playbook is no secret and stays;
	// the password and the forms that hold it are replaced.
	image.RegistryUsername = user
	s, _ = taskScope(secrets)
	r = reportedByTheEngine(run.DockerImage(ctx, image, s), s)
	if want := "docker_image: docker pull --quiet " + testImage + `: Error response from daemon: Get "https://registry.example.com/token?account=robot%24shop%2Bdeploy": unauthorized: ` + user + `:[redacted] is not a login; {"auths":{"registry.example.com":{"auth":"[redacted]"}}}`; r.Reason != want {
		t.Fatalf("%+v\nwant reason %s", r, want)
	}
	// A login without any secret is replaced nowhere.
	image.RegistryPassword = password
	s, _ = taskScope(secrets)
	if r = reportedByTheEngine(run.DockerImage(ctx, image, s), s); !strings.Contains(r.Reason, user+":"+password+" is not a login") || !strings.Contains(r.Reason, stored) {
		t.Fatalf("%+v", r)
	}
}

func TestDockerImageNeedsDockerAndItsDaemon(t *testing.T) {
	t.Parallel()
	f := newDockerFixture(t)
	ctx := testContext(t)
	image := DockerImage{Image: testImage, PullImage: "when_missing"}
	f.set("daemon-down", "")
	r := (Runtime{}).DockerImage(ctx, image, value.Scope{})
	if r.Status != "failed" || r.FailureKind != "missing_prerequisite" || !strings.Contains(r.Reason, "docker_image: the Docker daemon does not answer (Cannot connect to the Docker daemon") {
		t.Fatalf("%+v", r)
	}
	if r := noMutation(t).DockerImage(ctx, image, value.Scope{}); r.Status != "not_checked" {
		t.Fatalf("%+v", r)
	}
	if state, err := image.ReadState(ctx, nil); err != nil || state != "" {
		t.Fatal(state, err)
	}
	// Without the docker command.
	setWorkerEnvironment(t, "PATH", t.TempDir())
	r = (Runtime{}).DockerImage(ctx, image, value.Scope{})
	if r.Status != "failed" || r.FailureKind != "missing_prerequisite" || r.Reason != "docker_image: "+missingProgramError("docker").Error()+"; docker-ce comes from Docker's own package repository" {
		t.Fatalf("%+v", r)
	}
	if r := noMutation(t).DockerImage(ctx, image, value.Scope{}); r.Status != "not_checked" {
		t.Fatalf("%+v", r)
	}
	if state, err := image.ReadState(ctx, nil); err != nil || state != "" {
		t.Fatal(state, err)
	}
	if state, known := image.StateAfter(r); known || state != "" {
		t.Fatal(state, known)
	}
}

func TestDockerImageRejectsInputs(t *testing.T) {
	t.Parallel()
	for want, image := range map[string]DockerImage{
		"docker_image needs pull_image: when_missing or always; there is no default":                            {Image: testImage},
		"pull_image must be when_missing or always":                                                             {Image: testImage, PullImage: "missing"},
		"set registry_username and registry_password together, or omit registry_username and registry_password": {Image: testImage, PullImage: "always", RegistryUsername: "deploy"},
	} {
		if err := image.Prepare(""); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatal(want, err)
		}
	}
	for reference, want := range map[string]string{
		"registry.example.com/shop/web": "names no tag and no digest",
		"Registry.example.com/Shop:1":   "is not an image reference",
		"":                              "is not an image reference",
	} {
		if err := dockerImageReference(reference); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatal(reference, err)
		}
	}
	for _, reference := range []string{"postgres:17.2", testImage, "127.0.0.1:5000/mmx-test/app:1", "registry.example.com/shop/web@sha256:" + strings.Repeat("0", 64)} {
		if err := dockerImageReference(reference); err != nil {
			t.Fatal(reference, err)
		}
	}
}
