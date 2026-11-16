package modules

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

// A stand-in podman. The image store is the file $dir/store (an image ID;
// absent when the store has no image) and the registry the file
// $dir/registry (absent when the registry does not answer). The file
// $dir/store-digests holds the digests under which a registry served the
// store's image, as the body of a JSON list; a pull removes it. With the
// file $dir/registry-refuses-saying, a pull with a login file fails with the
// file's text and the text of the login file. It uses only the shell's own
// commands and /bin programs, so a test can leave nothing else on PATH.
const fakePodman = `
case "$1 $2" in
"version --format") printf '{"Client":{"APIVersion":"%s","Version":"%s"}}\n' "$(/bin/cat "$dir/version")" "$(/bin/cat "$dir/version")";;
"image exists") [ -f "$dir/store" ];;
"image inspect") printf '[{"Id": "%s", "RepoDigests": [%s]}]\n' "$(/bin/cat "$dir/store")" "$(/bin/cat "$dir/store-digests" 2>/dev/null)";;
"pull --quiet")
  shift 2
  if [ "$1" = --authfile ]; then
    /bin/cp "$2" "$dir/login-file-seen"; /bin/ls -ld "$2" "${2%/*}" > "$dir/login-file-modes"
    [ ! -f "$dir/registry-refuses-saying" ] || { echo "Error: initializing source docker://$3: $(/bin/cat "$dir/registry-refuses-saying") $(/bin/cat "$2")" >&2; exit 125; }
    shift 2
  fi
  [ -f "$dir/registry" ] || { echo "Error: initializing source docker://$1: pinging container registry: connection refused" >&2; exit 125; }
  /bin/cp "$dir/registry" "$dir/store"; /bin/rm -f "$dir/store-digests"
  [ ! -e "$dir/pull-fails-after-storing" ] || { echo "Error: writing manifest: unexpected EOF" >&2; exit 125; }
  /bin/cat "$dir/store";;
"container exists") [ -f "$dir/container" ];;
"container inspect") /bin/cat "$dir/container"; [ ! -e "$dir/container-inspect-fails" ] || exit 125;;
"volume exists"|"network exists") [ -f "$dir/$1-$3" ];;
"volume rm"|"network rm")
  # $dir/used-by-container-of names the service whose running container
  # uses every volume and network.
  if [ -f "$dir/used-by-container-of" ] && grep -qx "$(/bin/cat "$dir/used-by-container-of")" "$dir/active" 2>/dev/null; then
    echo "Error: $1 $3 is being used by the following container(s): 4f1d: $1 is being used" >&2; exit 2
  fi
  /bin/rm "$dir/$1-$3";;
*) echo "unexpected podman $*" >&2; exit 2;;
esac`

// A stand-in skopeo prints the manifest of the image the registry file
// names, or the list in $dir/manifest-list for a reference with a tag. The
// file $dir/layers holds the manifest's layers, as the body of a JSON list.
// With the file $dir/registry-refuses-saying, a question with a login file
// fails with the file's text and the text of the login file.
const fakeSkopeo = `
for reference; do :; done
if [ "$3" = --authfile ] && [ -f "$dir/registry-refuses-saying" ]; then
  echo 'time="2026-10-08T23:23:58Z" level=fatal msg="Error parsing image name \"'"$reference"'\": '"$(/bin/cat "$dir/registry-refuses-saying") $(/bin/cat "$4")"'"' >&2; exit 1
fi
[ -f "$dir/registry" ] || { echo 'time="2026-10-08T23:23:58Z" level=fatal msg="Error parsing image name \"'"$reference"'\": pinging container registry: connection refused"' >&2; exit 1; }
case "$reference" in
*@sha256:*) ;;
*) if [ -f "$dir/manifest-list" ]; then /bin/cat "$dir/manifest-list"; exit 0; fi;;
esac
printf '{"schemaVersion": 2, "config": {"digest": "sha256:%s"}, "layers": [%s]}\n' "$(/bin/cat "$dir/registry")" "$(/bin/cat "$dir/layers" 2>/dev/null)"`

// podmanZstdChunkedLayer is a layer of a manifest as a registry serves it
// for an image that was pushed in the zstd:chunked format.
const podmanZstdChunkedLayer = `{"mediaType": "application/vnd.oci.image.layer.v1.tar+zstd", "annotations": {"io.github.containers.zstd-chunked.manifest-checksum": "sha256:77"}}`

const podmanTestImage = "registry.example.com/shop/web:2.4"

var (
	podmanImageOne = strings.Repeat("1", 64)
	podmanImageTwo = strings.Repeat("2", 64)
)

// podmanFixture puts the stand-ins on PATH.
func podmanFixture(t *testing.T, programs map[string]string) string {
	t.Helper()
	dir := standInPrograms(t, programs)
	writeState(t, filepath.Join(dir, "version"), "5.4.2")
	return dir
}

// podmanManifestDigest is the digest of the manifest that the stand-in
// skopeo prints for an image with these layers: what a registry serves the
// image under.
func podmanManifestDigest(image, layers string) string {
	sum := sha256.Sum256([]byte(`{"schemaVersion": 2, "config": {"digest": "sha256:` + image + `"}, "layers": [` + layers + `]}` + "\n"))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func podmanImageIDs(r protocol.Result) [2]any {
	v, _ := r.Value.(map[string]any)
	return [2]any{v["image_id"], v["previous_image_id"]}
}

func TestPodmanImageRejectedInputs(t *testing.T) {
	t.Parallel()
	for message, p := range map[string]PodmanImage{
		"image: podman_image.image is required: a full image reference, such as registry.example.com/shop/web:2.4":                                         {PullImage: "always"},
		"pull_image: podman_image needs pull_image: when_missing or always; there is no default, because it decides whether a rerun deploys a newer image": {Image: podmanTestImage},
		"pull_image: pull_image must be when_missing or always":                                                                                            {Image: podmanTestImage, PullImage: "if_newer"},
		"registry_username: registry_username and registry_password come together":                                                                         {Image: podmanTestImage, PullImage: "always", RegistryUsername: "deploy"},
		"registry_username: registry_username and registry_password come together ":                                                                        {Image: podmanTestImage, PullImage: "always", RegistryPassword: "secret"},
	} {
		if err := p.Prepare(""); err == nil || err.Error() != strings.TrimSpace(message) {
			t.Errorf("%+v: %v; want %s", p, err, message)
		}
	}
	if err := (&PodmanImage{Image: podmanTestImage, PullImage: "when_missing"}).Prepare(""); err != nil {
		t.Error(err)
	}
	for reference, message := range map[string]string{
		"nginx:1.27":                    `"nginx:1.27" names no registry; write the full reference, such as docker.io/library/nginx:1.27`,
		"registry.example.com/shop/web": `"registry.example.com/shop/web" names no tag and no digest; write the tag, such as registry.example.com/shop/web:2.4, or a digest`,
	} {
		if err := podmanImageRule(reference); err == nil || err.Error() != message {
			t.Errorf("%s: %v", reference, err)
		}
	}
	for account, message := range map[string]string{"root": "root uses the system's Podman containers and image store; omit the user account field to use the system's store", "no such": "invalid account name"} {
		if err := ordinaryContainerAccount(account); err == nil || err.Error() != message {
			t.Errorf("%s: %v", account, err)
		}
	}
}

// A missing podman is something an earlier task may still install: a run
// fails without another try and names the package, a dryrun is not checked.
func TestPodmanImageWithoutPodman(t *testing.T) {
	t.Parallel()
	setWorkerEnvironment(t, "PATH", t.TempDir())
	p := PodmanImage{Image: podmanTestImage, PullImage: "when_missing"}
	reason := "podman_image: " + missingProgramError("podman").Error()
	if r := (Runtime{}).PodmanImage(testContext(t), p, value.Scope{}); r.Status != "failed" || r.FailureKind != "missing_prerequisite" || r.Reason != reason {
		t.Fatalf("%+v", r)
	}
	if r := noMutation(t).PodmanImage(testContext(t), p, value.Scope{}); r.Status != "not_checked" || r.Reason != reason {
		t.Fatalf("%+v", r)
	}
	if state, err := p.ReadState(testContext(t), nil); state != "" || err != nil {
		t.Fatal(state, err)
	}
}

func TestPodmanImageWhenMissing(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	dir := podmanFixture(t, map[string]string{"podman": fakePodman})
	writeState(t, filepath.Join(dir, "registry"), podmanImageOne)
	p := PodmanImage{Image: podmanTestImage, PullImage: "when_missing"}

	// A dryrun of a missing image would pull it, asks no registry, and
	// pulls nothing.
	r := noMutation(t).PodmanImage(ctx, p, value.Scope{})
	if r.Change != "would_change" || podmanImageIDs(r) != [2]any{nil, nil} || r.Reason != "the system's image store does not have the image "+podmanTestImage {
		t.Fatalf("%+v", r)
	}
	// A later task of the dryrun cannot read the ID of an image that is not pulled yet.
	if unknown := p.UnknownInADryrun(r); unknown["value"] != "would pull the image, whose ID is not known in this dryrun" {
		t.Fatal(unknown)
	}
	if calls := takeCalls(t, dir); !reflect.DeepEqual(calls, []string{"podman image exists " + podmanTestImage}) {
		t.Fatal(calls)
	}
	if r := (Runtime{}).PodmanImage(ctx, p, value.Scope{}); r.Status != "succeeded" || r.Change != "changed" || podmanImageIDs(r) != [2]any{"sha256:" + podmanImageOne, nil} {
		t.Fatalf("%+v", r)
	}
	if calls := changing(takeCalls(t, dir), "podman image"); !reflect.DeepEqual(calls, []string{"podman pull --quiet " + podmanTestImage}) {
		t.Fatal(calls)
	}
	// The tag moves; when_missing does not follow it, in a run or a dryrun.
	writeState(t, filepath.Join(dir, "registry"), podmanImageTwo)
	for _, run := range []Runtime{{}, noMutation(t)} {
		if r := run.PodmanImage(ctx, p, value.Scope{}); r.Status != "succeeded" || r.Change != "unchanged" || podmanImageIDs(r) != [2]any{"sha256:" + podmanImageOne, "sha256:" + podmanImageOne} {
			t.Fatalf("%+v", r)
		}
	}
	if calls := changing(takeCalls(t, dir), "podman image"); calls != nil {
		t.Fatal(calls)
	}
	if state, err := p.ReadState(ctx, nil); state != "sha256:"+podmanImageOne || err != nil {
		t.Fatal(state, err)
	}
}

func TestPodmanImageAlways(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	dir := podmanFixture(t, map[string]string{"podman": fakePodman, "skopeo": fakeSkopeo})
	writeState(t, filepath.Join(dir, "registry"), podmanImageOne)
	p := PodmanImage{Image: podmanTestImage, PullImage: "always"}

	// A missing image: the dryrun asks the registry for the ID to come.
	r := noMutation(t).PodmanImage(ctx, p, value.Scope{})
	if r.Change != "would_change" || podmanImageIDs(r) != [2]any{"sha256:" + podmanImageOne, nil} || r.Reason != "the registry names the image sha256:"+podmanImageOne+" for "+podmanTestImage+" now, and the system's image store does not have the image" {
		t.Fatalf("%+v", r)
	}
	if unknown := p.UnknownInADryrun(r); unknown != nil {
		t.Fatal("the dryrun knows the ID", unknown)
	}
	if calls := takeCalls(t, dir); !reflect.DeepEqual(calls, []string{"podman image exists " + podmanTestImage, "skopeo inspect --raw docker://" + podmanTestImage}) {
		t.Fatal(calls)
	}
	if r := (Runtime{}).PodmanImage(ctx, p, value.Scope{}); r.Change != "changed" || podmanImageIDs(r) != [2]any{"sha256:" + podmanImageOne, nil} {
		t.Fatalf("%+v", r)
	}
	// An unchanged registry: a run asks it with a pull and reports
	// unchanged; a dryrun asks it with skopeo.
	takeCalls(t, dir)
	if r := (Runtime{}).PodmanImage(ctx, p, value.Scope{}); r.Change != "unchanged" || podmanImageIDs(r) != [2]any{"sha256:" + podmanImageOne, "sha256:" + podmanImageOne} {
		t.Fatalf("%+v", r)
	}
	if calls := changing(takeCalls(t, dir), "podman image"); !reflect.DeepEqual(calls, []string{"podman pull --quiet " + podmanTestImage}) {
		t.Fatal(calls)
	}
	if r := noMutation(t).PodmanImage(ctx, p, value.Scope{}); r.Status != "succeeded" || r.Change != "unchanged" {
		t.Fatalf("%+v", r)
	}
	// The tag moves: the dryrun reports both IDs and pulls nothing; the run
	// pulls.
	writeState(t, filepath.Join(dir, "registry"), podmanImageTwo)
	takeCalls(t, dir)
	r = noMutation(t).PodmanImage(ctx, p, value.Scope{})
	if r.Change != "would_change" || podmanImageIDs(r) != [2]any{"sha256:" + podmanImageTwo, "sha256:" + podmanImageOne} || r.Reason != "the registry names the image sha256:"+podmanImageTwo+" for "+podmanTestImage+" now" {
		t.Fatalf("%+v", r)
	}
	if calls := changing(takeCalls(t, dir), "podman image", "skopeo inspect"); calls != nil || readState(t, filepath.Join(dir, "store")) != podmanImageOne {
		t.Fatal("the dryrun pulled", calls)
	}
	if r := (Runtime{}).PodmanImage(ctx, p, value.Scope{}); r.Change != "changed" || podmanImageIDs(r) != [2]any{"sha256:" + podmanImageTwo, "sha256:" + podmanImageOne} {
		t.Fatalf("%+v", r)
	}
	// The dryrun's prediction for a service that follows the image.
	if state, known := p.StateAfter(noMutation(t).PodmanImage(ctx, p, value.Scope{})); state != "sha256:"+podmanImageTwo || !known {
		t.Fatal(state, known)
	}
}

// Whether a pull would bring another image is decided for the image of the
// node's platform: by the digest under which the registry serves it, which
// Podman recorded with the store's image, and by the image ID that its
// manifest names. The dryrun never reports a change that a run would not
// make; where only a pull can tell, it is not checked.
func TestPodmanImageAlwaysDryrunComparesTheImageForThisPlatform(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	dir := podmanFixture(t, map[string]string{"podman": fakePodman, "skopeo": fakeSkopeo})
	p := PodmanImage{Image: podmanTestImage, PullImage: "always"}
	recorded := func(digests ...string) {
		t.Helper()
		var list []string
		for _, digest := range digests {
			list = append(list, `"registry.example.com/shop/web@`+digest+`"`)
		}
		writeState(t, filepath.Join(dir, "store-digests"), strings.Join(list, ", "))
	}
	dryrun := func() protocol.Result { return noMutation(t).PodmanImage(ctx, p, value.Scope{}) }
	unchanged := func(what string) {
		t.Helper()
		if r := dryrun(); r.Status != "succeeded" || r.Change != "unchanged" || podmanImageIDs(r) != [2]any{"sha256:" + podmanImageOne, "sha256:" + podmanImageOne} {
			t.Fatalf("%s: %+v", what, r)
		}
	}
	other := "sha256:" + strings.Repeat("9", 64)
	writeState(t, filepath.Join(dir, "store"), podmanImageOne)

	// An image for several platforms: the registry serves a list, and Podman
	// records the digest of the list and of this platform's manifest.
	writeState(t, filepath.Join(dir, "registry"), podmanImageOne)
	platform := podmanManifestDigest(podmanImageOne, "")
	list := func(otherPlatform string) string {
		return `{"manifests": [{"digest": "sha256:` + otherPlatform + `", "platform": {"os": "linux", "architecture": "mips"}},
			{"digest": "` + platform + `", "platform": {"os": "linux", "architecture": "` + runtime.GOARCH + `"}}]}`
	}
	first := list(strings.Repeat("a", 64))
	writeState(t, filepath.Join(dir, "manifest-list"), first)
	sum := sha256.Sum256([]byte(first))
	recorded("sha256:"+hex.EncodeToString(sum[:]), platform)
	unchanged("the same list")
	// The registry changes the list, because another platform's image was
	// built again, and keeps this platform's image. A pull would leave the
	// image as it is: by its manifest's digest, and by its ID alone.
	writeState(t, filepath.Join(dir, "manifest-list"), list(strings.Repeat("b", 64)))
	unchanged("another list with the same image for this platform")
	recorded(other)
	unchanged("the same image ID under digests that Podman did not record")
	// This platform's image changed.
	writeState(t, filepath.Join(dir, "registry"), podmanImageTwo)
	if r := dryrun(); r.Change != "would_change" || podmanImageIDs(r) != [2]any{"sha256:" + podmanImageTwo, "sha256:" + podmanImageOne} || r.Reason != "the registry names the image sha256:"+podmanImageTwo+" for "+podmanTestImage+" now" {
		t.Fatalf("%+v", r)
	}
	if err := os.Remove(filepath.Join(dir, "manifest-list")); err != nil {
		t.Fatal(err)
	}

	// An image with zstd:chunked layers, for which Podman can compute
	// another ID than the manifest names. The store's image has the ID of
	// image one and came under the manifest that names image two: unchanged,
	// on every dryrun.
	writeState(t, filepath.Join(dir, "layers"), podmanZstdChunkedLayer)
	served := podmanManifestDigest(podmanImageTwo, podmanZstdChunkedLayer)
	recorded(other, served)
	unchanged("an image with another ID than its manifest names")
	// It came under another digest: only a pull can say whether the
	// registry's image is another one, so the dryrun claims no change.
	recorded(other)
	r := dryrun()
	want := "podman_image: the registry now serves " + podmanTestImage + " under the digest " + served + ", and the image in the system's image store came under another digest. " +
		"The image's layers are in the zstd:chunked format, for which Podman can compute another image ID than the registry's manifest names, so only a pull can say whether it brings another image"
	if r.Status != "not_checked" || r.Change != "unknown" || r.Reason != want || podmanImageIDs(r) != [2]any{nil, "sha256:" + podmanImageOne} {
		t.Fatalf("%+v", r)
	}
	if state, known := p.StateAfter(r); known {
		t.Fatal("a restart is predicted from an image that a pull may leave as it is", state)
	}
	// A store without the image gets one with the pull, whose ID the dryrun
	// does not know.
	if err := os.Remove(filepath.Join(dir, "store")); err != nil {
		t.Fatal(err)
	}
	r = dryrun()
	if r.Status != "succeeded" || r.Change != "would_change" || podmanImageIDs(r) != [2]any{nil, nil} || r.Reason != "the registry now serves "+podmanTestImage+" under the digest "+served+", and the system's image store does not have the image" {
		t.Fatalf("%+v", r)
	}
	if unknown := p.UnknownInADryrun(r); unknown["value"] != "would pull the image, whose ID is not known in this dryrun" {
		t.Fatal(unknown)
	}
}

// A reference for several platforms names a list; the dryrun reads the
// entry for this node's platform.
func TestPodmanImageDryrunReadsThisPlatformFromAManifestList(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	dir := podmanFixture(t, map[string]string{"podman": fakePodman, "skopeo": fakeSkopeo})
	writeState(t, filepath.Join(dir, "registry"), podmanImageTwo)
	entry := "sha256:" + strings.Repeat("a", 64)
	p := PodmanImage{Image: podmanTestImage, PullImage: "always"}
	writeState(t, filepath.Join(dir, "manifest-list"), `{"manifests": [{"digest": "sha256:`+strings.Repeat("b", 64)+`", "platform": {"os": "plan9", "architecture": "mips"}},
		{"digest": "`+entry+`", "platform": {"os": "linux", "architecture": "`+runtime.GOARCH+`"}}]}`)
	if r := noMutation(t).PodmanImage(ctx, p, value.Scope{}); r.Change != "would_change" || podmanImageIDs(r) != [2]any{"sha256:" + podmanImageTwo, nil} {
		t.Fatalf("%+v", r)
	}
	if calls := takeCalls(t, dir); !reflect.DeepEqual(calls[1:], []string{"skopeo inspect --raw docker://" + podmanTestImage, "skopeo inspect --raw docker://registry.example.com/shop/web@" + entry}) {
		t.Fatal(calls)
	}
	writeState(t, filepath.Join(dir, "manifest-list"), `{"manifests": [{"digest": "`+entry+`", "platform": {"os": "plan9", "architecture": "mips"}}]}`)
	if r := noMutation(t).PodmanImage(ctx, p, value.Scope{}); r.Status != "not_checked" || !strings.Contains(r.Reason, "has no image for this node's platform, linux/"+runtime.GOARCH) {
		t.Fatalf("%+v", r)
	}
}

// A dryrun that cannot ask the registry is not checked and says why; a
// service that follows the image is then predicted to restart.
func TestPodmanImageDryrunThatCannotAskTheRegistry(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	dir := podmanFixture(t, map[string]string{"podman": fakePodman, "skopeo": fakeSkopeo})
	writeState(t, filepath.Join(dir, "store"), podmanImageOne)
	p := PodmanImage{Image: podmanTestImage, PullImage: "always"}
	r := noMutation(t).PodmanImage(ctx, p, value.Scope{})
	if r.Status != "not_checked" || r.Change != "unknown" || !strings.HasPrefix(r.Reason, "podman_image: the registry does not say which image "+podmanTestImage+" names: skopeo inspect --raw docker://"+podmanTestImage+": ") || !strings.Contains(r.Reason, "connection refused") {
		t.Fatalf("%+v", r)
	}
	// The store has the image, and the result names it, so nothing is
	// predicted for what follows it: a run may leave the image as it is.
	if podmanImageIDs(r) != [2]any{nil, "sha256:" + podmanImageOne} {
		t.Fatalf("%+v", r)
	}
	if state, known := p.StateAfter(r); known {
		t.Fatal(state, known)
	}
	// The store does not have the image: a pull is certain, and the
	// prediction of a change stays.
	if err := os.Rename(filepath.Join(dir, "store"), filepath.Join(dir, "store.kept")); err != nil {
		t.Fatal(err)
	}
	r = noMutation(t).PodmanImage(ctx, p, value.Scope{})
	if r.Status != "not_checked" || podmanImageIDs(r) != [2]any{nil, nil} {
		t.Fatalf("%+v", r)
	}
	if state, known := p.StateAfter(r); state != stateWouldChange || !known {
		t.Fatal(state, known)
	}
	if err := os.Rename(filepath.Join(dir, "store.kept"), filepath.Join(dir, "store")); err != nil {
		t.Fatal(err)
	}
	// Without skopeo the dryrun names the package; a run needs only podman.
	setWorkerEnvironment(t, "PATH", dir)
	if err := os.Remove(filepath.Join(dir, "skopeo")); err != nil {
		t.Fatal(err)
	}
	writeState(t, filepath.Join(dir, "registry"), podmanImageOne)
	r = noMutation(t).PodmanImage(ctx, p, value.Scope{})
	if r.Status != "not_checked" || r.Reason != "podman_image: a dryrun of pull_image: always asks the registry with the program skopeo, which is not installed ("+missingProgramError("skopeo").Error()+"). A run needs only Podman" {
		t.Fatalf("%+v", r)
	}
	if state, known := p.StateAfter(r); known || podmanImageIDs(r) != [2]any{nil, "sha256:" + podmanImageOne} {
		t.Fatal(state, known, r.Value)
	}
	if r := (Runtime{}).PodmanImage(ctx, p, value.Scope{}); r.Status != "succeeded" || r.Change != "unchanged" {
		t.Fatalf("%+v", r)
	}
}

// A digest names one image forever: the node asks the registry only when
// its store lacks the image, also with pull_image: always.
func TestPodmanImageWithADigestAsksOnlyForAMissingImage(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	dir := podmanFixture(t, map[string]string{"podman": fakePodman, "skopeo": fakeSkopeo})
	writeState(t, filepath.Join(dir, "store"), podmanImageOne)
	p := PodmanImage{Image: "registry.example.com/shop/web@sha256:" + strings.Repeat("c", 64), PullImage: "always"}
	for _, run := range []Runtime{{}, noMutation(t)} {
		if r := run.PodmanImage(ctx, p, value.Scope{}); r.Status != "succeeded" || r.Change != "unchanged" {
			t.Fatalf("%+v", r)
		}
	}
	if calls := changing(takeCalls(t, dir), "podman image"); calls != nil {
		t.Fatal(calls)
	}
}

func TestPodmanImagePullFailures(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	dir := podmanFixture(t, map[string]string{"podman": fakePodman})
	p := PodmanImage{Image: podmanTestImage, PullImage: "always"}
	// The registry does not answer: nothing changed, and the reason is
	// podman's.
	r := (Runtime{}).PodmanImage(ctx, p, value.Scope{})
	if r.Status != "failed" || r.Change != "unchanged" || !strings.HasPrefix(r.Reason, "podman_image: podman pull --quiet "+podmanTestImage+": Error: initializing source") {
		t.Fatalf("%+v", r)
	}
	// A pull that stored the image and then failed: the failure keeps the
	// change, and another try finds the image and succeeds.
	writeState(t, filepath.Join(dir, "registry"), podmanImageOne)
	writeState(t, filepath.Join(dir, "pull-fails-after-storing"), "")
	r = (Runtime{}).PodmanImage(ctx, p, value.Scope{})
	if r.Status != "failed" || r.Change != "changed" || !strings.Contains(r.Reason, "unexpected EOF") {
		t.Fatalf("%+v", r)
	}
	if err := os.Remove(filepath.Join(dir, "pull-fails-after-storing")); err != nil {
		t.Fatal(err)
	}
	if r := (Runtime{}).PodmanImage(ctx, p, value.Scope{}); r.Status != "succeeded" || r.Change != "unchanged" || podmanImageIDs(r) != [2]any{"sha256:" + podmanImageOne, "sha256:" + podmanImageOne} {
		t.Fatalf("%+v", r)
	}
}

// The login reaches podman in a file only its account can read, in a
// private directory that is gone after the task, also after a failure. The
// password is on no command line.
func TestPodmanImageRegistryLogin(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	dir := podmanFixture(t, map[string]string{"podman": fakePodman, "skopeo": fakeSkopeo})
	privateTaskDirectoryRoot := t.TempDir()
	run, dryrun := withPrivateTaskDirectoryRoot(Runtime{}, privateTaskDirectoryRoot), withPrivateTaskDirectoryRoot(noMutation(t), privateTaskDirectoryRoot)
	const password = "sentinel-registry-password"
	p := PodmanImage{Image: podmanTestImage, PullImage: "always", RegistryUsername: "deploy", RegistryPassword: password}
	check := func(what string) {
		t.Helper()
		calls := strings.Join(takeCalls(t, dir), "\n")
		if strings.Contains(calls, password) || !strings.Contains(calls, " --authfile "+privateTaskDirectoryRoot+"/podman_image-") {
			t.Fatal(what, calls)
		}
		if left, _ := os.ReadDir(privateTaskDirectoryRoot); len(left) != 0 {
			t.Fatal(what, "left the login directory behind", left)
		}
	}
	// The registry does not answer. The failure names the command without
	// the login file.
	if r := run.PodmanImage(ctx, p, value.Scope{}); r.Status != "failed" || strings.Contains(r.Reason, password) || !strings.HasPrefix(r.Reason, "podman_image: podman pull --quiet "+podmanTestImage+": Error: initializing source") {
		t.Fatalf("%+v", r)
	}
	check("a failed pull")
	if r := dryrun.PodmanImage(ctx, p, value.Scope{}); r.Status != "not_checked" || !strings.HasPrefix(r.Reason, "podman_image: the registry does not say which image "+podmanTestImage+" names: skopeo inspect --raw docker://"+podmanTestImage+": ") {
		t.Fatalf("%+v", r)
	}
	check("a dryrun that the registry does not answer")
	writeState(t, filepath.Join(dir, "registry"), podmanImageOne)
	if r := dryrun.PodmanImage(ctx, p, value.Scope{}); r.Change != "would_change" {
		t.Fatalf("%+v", r)
	}
	check("a dryrun")
	if r := run.PodmanImage(ctx, p, value.Scope{}); r.Status != "succeeded" || r.Change != "changed" {
		t.Fatalf("%+v", r)
	}
	check("a pull")
	if seen := readState(t, filepath.Join(dir, "login-file-seen")); seen != string(registryLoginFile([]registryLogin{{"registry.example.com", "deploy", password}})) {
		t.Fatal(seen)
	}
	// What ls -ld printed for the file and its directory while podman ran.
	modes := map[bool]string{}
	for _, line := range strings.Split(strings.TrimSpace(readState(t, filepath.Join(dir, "login-file-modes"))), "\n") {
		modes[strings.HasSuffix(line, "/auth.json")] = strings.Fields(line)[0]
	}
	if !strings.HasPrefix(modes[true], "-rw-------") || !strings.HasPrefix(modes[false], "drwx------") {
		t.Fatal(modes)
	}
}

// podman_image hands its login only to podman and, in a dryrun, to skopeo. A
// secret in the login marks no trace, so the task's result shows. A failure
// that tells the login, as its plain text or in a form in which Podman
// gets, keeps, or sends it, reports [redacted] in its place and keeps the
// rest of its reason.
func TestPodmanImageLoginHandedOnlyToPodmanLeavesTheResultReadable(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	dir := podmanFixture(t, map[string]string{"podman": fakePodman, "skopeo": fakeSkopeo})
	privateTaskDirectoryRoot := t.TempDir()
	run, dryrun := withPrivateTaskDirectoryRoot(Runtime{}, privateTaskDirectoryRoot), withPrivateTaskDirectoryRoot(noMutation(t), privateTaskDirectoryRoot)
	const user, password = "robot$shop+deploy", "sentinel p/w&4711"
	secrets := map[string]string{"registry_username": user, "registry_password": password}
	stored := base64.StdEncoding.EncodeToString([]byte(user + ":" + password))
	p := PodmanImage{Image: podmanTestImage, PullImage: "always", RegistryUsername: "{{ .secrets.registry_username }}", RegistryPassword: "{{ .secrets.registry_password }}"}
	// A pull with the login: the result names the image.
	writeState(t, filepath.Join(dir, "registry"), podmanImageOne)
	s, trace := taskScope(secrets)
	r := reportedByTheEngine(run.PodmanImage(ctx, p, s), s)
	if r.Status != "succeeded" || r.Change != "changed" || podmanImageIDs(r) != [2]any{"sha256:" + podmanImageOne, nil} || trace.Used {
		t.Fatalf("%+v, the login marked the trace: %v", r, trace.Used)
	}
	// The registry refuses, and podman tells the login: in the address of
	// the registry's token service, as its plain text, and as the text of
	// the login file.
	const saying = `requesting bearer token: Get "https://registry.example.com/token?account=robot%24shop%2Bdeploy": unauthorized: `
	writeState(t, filepath.Join(dir, "registry-refuses-saying"), saying+user+":"+password+" is not a login;")
	const refusal = `requesting bearer token: Get "https://registry.example.com/token?account=[redacted]": unauthorized: [redacted]:[redacted] is not a login; {"auths":{"registry.example.com":{"auth":"[redacted]"}}}`
	s, trace = taskScope(secrets)
	told := run.PodmanImage(ctx, p, s)
	for _, form := range []string{user, password, "robot%24shop%2Bdeploy", stored} {
		if !strings.Contains(told.Reason, form) {
			t.Fatalf("the stand-in podman did not tell %q: %s", form, told.Reason)
		}
	}
	r = reportedByTheEngine(told, s)
	if want := "podman_image: podman pull --quiet " + podmanTestImage + ": Error: initializing source docker://" + podmanTestImage + ": " + refusal; r.Status != "failed" || r.Reason != want || trace.Used {
		t.Fatalf("%+v\nwant reason %s", r, want)
	}
	// A dryrun asks with skopeo, which tells the login in the same way.
	s, trace = taskScope(secrets)
	told = dryrun.PodmanImage(ctx, p, s)
	if !strings.Contains(told.Reason, password) || !strings.Contains(told.Reason, stored) {
		t.Fatalf("the stand-in skopeo did not tell the login: %s", told.Reason)
	}
	r = reportedByTheEngine(told, s)
	if want := `msg="Error parsing image name \"docker://` + podmanTestImage + `\": ` + refusal + `"`; r.Status != "not_checked" ||
		!strings.HasPrefix(r.Reason, "podman_image: the registry does not say which image "+podmanTestImage+" names: skopeo inspect --raw docker://"+podmanTestImage+": ") || !strings.HasSuffix(r.Reason, want) || trace.Used {
		t.Fatalf("%+v\nwant a reason that ends with %s", r, want)
	}
	// A user name that is written in the playbook is no secret and stays;
	// the password and the forms that hold it are replaced.
	p.RegistryUsername = user
	s, _ = taskScope(secrets)
	r = reportedByTheEngine(run.PodmanImage(ctx, p, s), s)
	if want := saying + user + `:[redacted] is not a login; {"auths":{"registry.example.com":{"auth":"[redacted]"}}}`; !strings.HasSuffix(r.Reason, want) {
		t.Fatalf("%+v\nwant a reason that ends with %s", r, want)
	}
	if left, _ := os.ReadDir(privateTaskDirectoryRoot); len(left) != 0 {
		t.Fatal("a login directory is left behind", left)
	}
}

func TestPodmanImageIsATrackedInput(t *testing.T) {
	t.Parallel()
	p := &PodmanImage{Image: podmanTestImage, PullImage: "always"}
	if in := p.TrackedInputs(); len(in) != 1 || in[0].Key() != "installed:podman_image "+podmanTestImage {
		t.Fatalf("%+v", in)
	}
	p.InImageStoreOfUserAccount = "shop"
	if in := p.TrackedInputs(); in[0].Key() != "installed:podman_image "+podmanTestImage+" of account shop" {
		t.Fatalf("%+v", in)
	}
	for _, tc := range []struct {
		result protocol.Result
		state  string
		known  bool
	}{
		{podmanImageResult("would_change", "sha256:"+podmanImageTwo, "sha256:"+podmanImageOne), "sha256:" + podmanImageTwo, true},
		{podmanImageResult("unchanged", "sha256:"+podmanImageOne, "sha256:"+podmanImageOne), "sha256:" + podmanImageOne, true},
		{podmanImageResult("would_change", "", ""), stateWouldChange, true},
		// Not checked without podman or the account: the store has no image
		// that the dryrun could name, and a pull is certain.
		{protocol.Result{Status: "not_checked"}, stateWouldChange, true},
		{podmanImageNotChecked("the registry does not answer", ""), stateWouldChange, true},
		// Not checked for an image that the store has: nothing is predicted.
		{podmanImageNotChecked("the registry does not answer", "sha256:"+podmanImageOne), "", false},
		{protocol.Result{Status: "failed"}, "", false},
	} {
		if state, known := p.StateAfter(tc.result); state != tc.state || known != tc.known {
			t.Errorf("%+v: %q %v", tc.result, state, known)
		}
	}
}

// What Podman needs of an ordinary account, each with what to do.
func TestContainerAccountNeedsAHomeAndSubordinateIDs(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	subuid, subgid, nsswitch := filepath.Join(dir, "subuid"), filepath.Join(dir, "subgid"), filepath.Join(dir, "nsswitch.conf")
	previousFiles, previousSwitch := subordinateIDFiles, nameServiceSwitchFile
	subordinateIDFiles = []struct{ path, ids string }{{subuid, "sub-user IDs"}, {subgid, "sub-group IDs"}}
	nameServiceSwitchFile = nsswitch
	t.Cleanup(func() { subordinateIDFiles, nameServiceSwitchFile = previousFiles, previousSwitch })
	m := UnitManager{Account: "shop", Home: home, UID: 1002, GID: 1002}

	if stop := (Runtime{}).checkContainerAccount("podman_image", UnitManager{}); stop != nil {
		t.Fatal("the system needs no account", stop)
	}
	stop := (Runtime{}).checkContainerAccount("podman_image", m)
	if stop == nil || stop.FailureKind != "missing_prerequisite" || !strings.HasPrefix(stop.Reason, "podman_image: account shop has no home directory at "+home+";") || !strings.Contains(stop.Reason, "create_home_directory: true") {
		t.Fatalf("%+v", stop)
	}
	if stop := noMutation(t).checkContainerAccount("podman_image", m); stop == nil || stop.Status != "not_checked" {
		t.Fatalf("%+v", stop)
	}
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	stop = (Runtime{}).checkContainerAccount("podman_quadlet", m)
	if stop == nil || stop.FailureKind != "missing_prerequisite" || !strings.HasPrefix(stop.Reason, "podman_quadlet: account shop has no range of sub-user IDs in "+subuid+";") || !strings.Contains(stop.Reason, "system: false") {
		t.Fatalf("%+v", stop)
	}
	writeState(t, subuid, "deploy:100000:65536\nshop:231072:65536\n")
	stop = (Runtime{}).checkContainerAccount("podman_quadlet", m)
	if stop == nil || !strings.HasPrefix(stop.Reason, "podman_quadlet: account shop has no range of sub-group IDs in "+subgid+";") {
		t.Fatalf("%+v", stop)
	}
	// A line may name the account by its user ID.
	writeState(t, subgid, "1002:231072:65536\n")
	if stop := (Runtime{}).checkContainerAccount("podman_quadlet", m); stop != nil {
		t.Fatalf("%+v", stop)
	}
	// A node that looks sub-IDs up elsewhere is not judged by the files.
	writeState(t, subgid, "")
	writeState(t, nsswitch, "passwd: files sss\nsubid: sss\n")
	if stop := (Runtime{}).checkContainerAccount("podman_quadlet", m); stop != nil {
		t.Fatalf("%+v", stop)
	}
	writeState(t, nsswitch, "passwd: files\nsubid: files\n")
	if stop := (Runtime{}).checkContainerAccount("podman_quadlet", m); stop == nil {
		t.Fatal("subid: files leaves the files in charge")
	}
}

// A dryrun for an account that does not linger yet changes nothing and runs
// no podman as the account: without lingering the account has no runtime
// directory, and podman would keep its state under /tmp from then on.
func TestPodmanImageDryrunForAnAccountThatDoesNotLingerYet(t *testing.T) {
	account, err := user.Current()
	if err != nil || account.Uid == "0" {
		t.Skip("needs an account other than root")
	}
	m, err := SystemdOf(account.Username)
	if err != nil || m.Lingering() || m.Running() {
		t.Skip("needs an account without lingering and without a running systemd of its own")
	}
	dir := podmanFixture(t, map[string]string{"podman": fakePodman})
	previousFiles := subordinateIDFiles
	subordinateIDFiles = nil
	t.Cleanup(func() { subordinateIDFiles = previousFiles })
	p := PodmanImage{Image: podmanTestImage, PullImage: "when_missing", InImageStoreOfUserAccount: account.Username}
	r := noMutation(t).PodmanImage(testContext(t), p, value.Scope{})
	want := "would enable lingering for account " + account.Username + ", so its services start at boot and keep running after logout; the account's image store can be read only after that"
	if r.Status != "succeeded" || r.Change != "would_change" || r.Reason != want || podmanImageIDs(r) != [2]any{nil, nil} {
		t.Fatalf("%+v", r)
	}
	if calls := takeCalls(t, dir); calls != nil {
		t.Fatal("the dryrun ran podman as the account", calls)
	}
	if state, known := p.StateAfter(r); state != stateWouldChange || !known {
		t.Fatal(state, known)
	}
	if state, err := p.ReadState(testContext(t), nil); state != "" || err != nil {
		t.Fatal(state, err)
	}
}

// The health of a container as podman container inspect prints it.
func podmanContainerState(status, health, healthField, healthcheck string) string {
	return `[{"State": {"Status": "` + status + `", "` + healthField + `": ` + health + `}, "Config": {"Healthcheck": ` + healthcheck + `}}]`
}

const podmanDeclaredHealthCheck = `{"Test": ["CMD", "/container-test-program", "check-health"], "Interval": 1000000000}`

func TestWaitForPodmanContainerIsHealthy(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	dir := podmanFixture(t, map[string]string{"podman": fakePodman})
	container := filepath.Join(dir, "container")
	w := WaitFor{PodmanContainerIsHealthy: "shop-web", WaitAtMost: "30s"}
	if err := ValidateWaitFor(w); err != nil {
		t.Fatal(err)
	}
	// No such container yet: a dryrun checks once; the wait goes on to its
	// end and says what it last saw.
	if r := noMutation(t).WaitFor(ctx, w, nil); r.Status != "not_checked" || r.Reason != "podman has no container named shop-web" {
		t.Fatalf("%+v", r)
	}
	r := waitForOneFinishedObservation(Runtime{}, ctx, w, nil)
	if r.Status != "failed" || r.Change != "unchanged" || !strings.Contains(r.Reason, "wait_for ended after at most 30s") || !strings.HasSuffix(r.Reason, "last observation: podman has no container named shop-web") {
		t.Fatalf("%+v", r)
	}
	// From here on no check is about the end of a wait, so a slow machine
	// must not end one.
	w.WaitAtMost = "1m"
	writeState(t, container, podmanContainerState("running", `{"Status": "healthy", "FailingStreak": 0, "Log": []}`, "Health", podmanDeclaredHealthCheck))
	for _, run := range []Runtime{{}, noMutation(t)} {
		if r := run.WaitFor(ctx, w, nil); r.Status != "succeeded" || r.Change != "unchanged" || r.Reason != "the container shop-web is healthy" {
			t.Fatalf("%+v", r)
		}
	}
	// Podman before 4.0 names the field Healthcheck.
	writeState(t, container, podmanContainerState("running", `{"Status": "healthy"}`, "Healthcheck", podmanDeclaredHealthCheck))
	if r := (Runtime{}).WaitFor(ctx, w, nil); r.Status != "succeeded" {
		t.Fatalf("%+v", r)
	}
	// Not healthy: the status and what the last health check printed.
	writeState(t, container, podmanContainerState("running", `{"Status": "unhealthy", "FailingStreak": 3, "Log": [{"ExitCode": 0, "Output": "ok"}, {"ExitCode": 1, "Output": "the health address answered 503\n"}]}`, "Health", podmanDeclaredHealthCheck))
	if r := noMutation(t).WaitFor(ctx, w, nil); r.Status != "not_checked" || r.Reason != "the container shop-web is unhealthy, and its last health check exited with code 1 and printed: the health address answered 503" {
		t.Fatalf("%+v", r)
	}
	writeState(t, container, podmanContainerState("exited", `{"Status": "starting", "Log": null}`, "Health", podmanDeclaredHealthCheck))
	if r := noMutation(t).WaitFor(ctx, w, nil); r.Status != "not_checked" || r.Reason != "the container shop-web is starting (the container is exited)" {
		t.Fatalf("%+v", r)
	}
	// A container without a health check ends the wait at once: no answer
	// would come. Podman 5 prints no health for it, Podman 4 an empty one.
	for _, state := range []string{podmanContainerState("running", "null", "Health", "null"), podmanContainerState("running", `{"Status": "", "Log": null}`, "Health", "null"),
		podmanContainerState("running", "null", "Health", `{"Test": ["NONE"]}`)} {
		writeState(t, container, state)
		r := (Runtime{}).WaitFor(ctx, w, nil)
		if r.Status != "failed" || r.FailureKind != "missing_prerequisite" || r.Change != "unchanged" || r.Reason != "the container shop-web declares no health check, so Podman never reports it healthy; declare one with HealthCmd= in its Quadlet file, or wait for a port or a url instead" {
			t.Fatalf("%+v", r)
		}
		// A dryrun only reports it: an earlier task may still add the health check.
		if r := noMutation(t).WaitFor(ctx, w, nil); r.Status != "not_checked" {
			t.Fatalf("%+v", r)
		}
	}
	if calls := changing(takeCalls(t, dir), "podman container exists shop-web", "podman container inspect --format json shop-web"); calls != nil {
		t.Fatal("the wait ran something that is not a read", calls)
	}
	// podman container inspect prints the container's environment values.
	// When it fails after printing, none of its output reaches the reason.
	writeState(t, container, `[{"Config": {"Env": ["DATABASE_PASSWORD=sentinel-environment-value"]}}]`)
	writeState(t, filepath.Join(dir, "container-inspect-fails"), "")
	if r := noMutation(t).WaitFor(ctx, w, nil); r.Status != "not_checked" || r.Reason != "wait_for: podman container inspect --format json shop-web: exit status 125" {
		t.Fatalf("%+v", r)
	}
}

func TestWaitForPodmanContainerRejectedInputs(t *testing.T) {
	t.Parallel()
	port := 80
	const forms = "wait_for requires exactly one of tcp_port_accepts_connections, tcp_port_refuses_connections, path_exists, path_is_missing, url_responds, command_succeeds, docker_container_is_healthy, docker_compose_project_is_ready, or podman_container_is_healthy"
	for message, w := range map[string]WaitFor{
		forms:       {PodmanContainerIsHealthy: "shop-web", TCPPortAcceptsConnections: &port},
		forms + " ": {PodmanContainerIsHealthy: "shop-web", DockerContainerIsHealthy: "shop-web"},
		"podman_container_of_user_account: wait_for.podman_container_of_user_account requires podman_container_is_healthy; it names the account whose Podman container the task waits for":  {TCPPortAcceptsConnections: &port, PodmanContainerOfUserAccount: "shop"},
		"podman_container_of_user_account: wait_for.podman_container_of_user_account requires podman_container_is_healthy; it names the account whose Podman container the task waits for ": {DockerContainerIsHealthy: "shop-web", PodmanContainerOfUserAccount: "shop"},
		"wait_for.run_as_user_account, run_from_directory, and env require command_succeeds":                                                                                                {PodmanContainerIsHealthy: "shop-web", User: "shop"},
		"tcp_host: wait_for.tcp_host requires tcp_port_accepts_connections or tcp_port_refuses_connections":                                                                                 {PodmanContainerIsHealthy: "shop-web", TCPHost: "127.0.0.1"},
	} {
		if err := ValidateWaitFor(w); err == nil || err.Error() != strings.TrimSpace(message) {
			t.Errorf("%+v: %v; want %s", w, err, message)
		}
	}
	if err := containerNameRule("podman_container_is_healthy")("shop web"); err == nil || !strings.Contains(err.Error(), "podman_container_is_healthy must be a container's name") {
		t.Error(err)
	}
	if err := containerNameRule("podman_container_is_healthy")("shop-web_1.0"); err != nil {
		t.Error(err)
	}
	// A missing podman, or account, is something an earlier task provides:
	// a run ends the wait at once, a dryrun is not checked.
	setWorkerEnvironment(t, "PATH", t.TempDir())
	w := WaitFor{PodmanContainerIsHealthy: "shop-web", WaitAtMost: "1m"}
	if r := (Runtime{}).WaitFor(testContext(t), w, nil); r.FailureKind != "missing_prerequisite" || r.Reason != "wait_for: "+missingProgramError("podman").Error() {
		t.Fatalf("%+v", r)
	}
	if r := noMutation(t).WaitFor(testContext(t), w, nil); r.Status != "not_checked" {
		t.Fatalf("%+v", r)
	}
	podmanFixture(t, map[string]string{"podman": fakePodman})
	w.PodmanContainerOfUserAccount = "mmx-no-such-account"
	if r := (Runtime{}).WaitFor(testContext(t), w, nil); r.FailureKind != "missing_prerequisite" || !strings.HasPrefix(r.Reason, "wait_for.podman_container_of_user_account: user: unknown user mmx-no-such-account") {
		t.Fatalf("%+v", r)
	}
	if r := noMutation(t).WaitFor(testContext(t), w, nil); r.Status != "not_checked" {
		t.Fatalf("%+v", r)
	}
}
