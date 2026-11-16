package modules

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"mmx/internal/value"
)

// A stand-in for Podman's Quadlet generator in its check-only mode. Like the
// real one it reads the directories of QUADLET_UNIT_DIRS in turn, takes the
// first file of each name that it can read, prints ---<service>--- and what
// the service runs for each file it accepts, and exits with 1 when it
// rejects one. It rejects a file that holds the word INVALID, and knows no
// .pod files, as Podman 4.9 does not. A file that holds the word UNREADABLE
// is one that the parser cannot read: the generator passes over it and reads
// the next file of the same name, as the real one does. Podman 5 says why and
// exits with 1; Podman 4 says nothing and exits with 0, which the words
// UNREADABLE SILENTLY ask for. While $dir/generator-waits exists the
// generator does not end, as the real one on a named pipe.
const fakeQuadletGenerator = `
echo "$QUADLET_UNIT_DIRS" > "$dir/generator-directories"
/usr/bin/id -u > "$dir/generator-user"
[ ! -e "$dir/generator-waits" ] || /bin/sleep 20
status=0; seen=" "
old_ifs=$IFS; IFS=:
for directory in $QUADLET_UNIT_DIRS; do
  IFS=$old_ifs
  for file in "$directory"/*.*; do
    [ -f "$file" ] || continue
    name=${file##*/}
    case "$seen" in *" $name "*) continue;; esac
    base=${name%.*}
    case "$name" in
    *.container|*.kube) unit=$base.service;;
    *.volume) unit=$base-volume.service;;
    *.network) unit=$base-network.service;;
    *.image) unit=$base-image.service;;
    *.build) unit=$base-build.service;;
    *) continue;;
    esac
    echo "quadlet-generator[4242]: Loading source unit file $file" >&2
    if grep -q UNREADABLE "$file"; then
      if ! grep -q SILENTLY "$file"; then
        echo "quadlet-generator[4242]: error loading \"$file\", file contains line 2: UNREADABLE which is not a key-value pair, group, or comment" >&2
        status=1
      fi
      continue
    fi
    seen="$seen$name "
    if grep -q INVALID "$file"; then
      echo "quadlet-generator[4242]: converting \"$name\": unsupported key 'INVALID' in group 'Container' in $file" >&2
      status=1
      continue
    fi
    printf -- '---%s---\n[Unit]\nSourcePath=%s\n\n[Service]\n' "$unit" "$file"
    grep -E '^(Image|VolumeName|NetworkName|Yaml|ImageTag)=' "$file" | sed 's/^/ExecStart=podman /'
  done
done
exit $status`

// A stand-in systemctl for the generated services. A reload makes a service
// for each installed Quadlet file, as the generator does: every unit is
// loaded while $dir/loaded exists. $dir/active holds the units that run.
// Stopping a unit also stops the unit that $dir/required-by-<unit> names, as
// systemd stops a container's service with the service of the volume file
// that the container names; show prints that unit as RequiredBy.
const fakeQuadletSystemctl = `
touch "$dir/active"
case "$1" in
show)
  for unit; do :; done
  load=not-found; [ ! -e "$dir/loaded" ] || load=loaded
  active=inactive; instance=
  if grep -qx "$unit" "$dir/active"; then active=active; instance=1; fi
  printf 'Id=%s\nNames=%s\nLoadState=%s\nActiveState=%s\nUnitFileState=generated\nInvocationID=%s\nNeedDaemonReload=%s\nRequiredBy=%s\nBoundBy=\nConsistsOf=\n' "$unit" "$unit" "$load" "$active" "$instance" "$(cat "$dir/need-reload" 2>/dev/null || echo no)" "$(cat "$dir/required-by-$unit" 2>/dev/null | tr '\n' ' ')";;
daemon-reload)
  if ls "$MMX_TEST_QUADLETS"/*.* >/dev/null 2>&1; then touch "$dir/loaded"; else rm -f "$dir/loaded"; fi
  echo no > "$dir/need-reload";;
stop)
  for unit; do :; done
  for stopped in "$unit" $(cat "$dir/required-by-$unit" 2>/dev/null); do
    grep -vx "$stopped" "$dir/active" > "$dir/active.kept" || true; mv "$dir/active.kept" "$dir/active"
  done;;
esac`

type quadletFixture struct {
	dir, quadlets string
	// privateTaskDirectoryRoot is where a task's private directory for
	// Podman's check goes, as the node hands it to a task.
	privateTaskDirectoryRoot string
}

// run and dryrun are the Runtime of a run's node task and of a dryrun's, as
// the node task execution engine hands it to a module.
func (f quadletFixture) run() Runtime {
	return withPrivateTaskDirectoryRoot(Runtime{}, f.privateTaskDirectoryRoot)
}

func (f quadletFixture) dryrun(t *testing.T) Runtime {
	return withPrivateTaskDirectoryRoot(noMutation(t), f.privateTaskDirectoryRoot)
}

// newQuadletFixture puts the stand-ins in place and the system's Quadlet
// directory and MMX's record of kept volumes under a test directory; the
// directories themselves do not exist yet.
func newQuadletFixture(t *testing.T) quadletFixture {
	t.Helper()
	dir := podmanFixture(t, map[string]string{"podman": fakePodman, "systemctl": fakeQuadletSystemctl, "podman-system-generator": fakeQuadletGenerator})
	quadlets := filepath.Join(t.TempDir(), "containers", "systemd")
	if err := os.Mkdir(filepath.Dir(quadlets), 0755); err != nil {
		t.Fatal(err)
	}
	previousDirectory, previousGenerator, previousOwner, previousGroup, previousRecords := quadletSystemDirectory, quadletGenerator, quadletSystemOwner, quadletSystemGroup, quadletKeptVolumeRecordRoot
	quadletSystemDirectory, quadletGenerator = quadlets, filepath.Join(dir, "podman-system-generator")
	quadletKeptVolumeRecordRoot = filepath.Join(t.TempDir(), "podman_volumes_of_removed_quadlet_files")
	// The test's own account and group own the files; only root can give
	// them to root.
	quadletSystemOwner, quadletSystemGroup = Account(strconv.Itoa(os.Geteuid())), Account(strconv.Itoa(os.Getegid()))
	t.Cleanup(func() {
		quadletSystemDirectory, quadletGenerator, quadletSystemOwner, quadletSystemGroup, quadletKeptVolumeRecordRoot = previousDirectory, previousGenerator, previousOwner, previousGroup, previousRecords
	})
	t.Setenv("MMX_TEST_QUADLETS", quadlets)
	return quadletFixture{dir, quadlets, t.TempDir()}
}

func (f quadletFixture) installed(t *testing.T, name string) string {
	t.Helper()
	return readState(t, filepath.Join(f.quadlets, name))
}

// leftovers lists what is in the Quadlet directory besides Quadlet files: a
// candidate directory must never stay.
func (f quadletFixture) leftovers(t *testing.T) []string {
	t.Helper()
	entries, _ := os.ReadDir(f.quadlets)
	var out []string
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			out = append(out, entry.Name())
		}
	}
	return out
}

func quadletContainer(text string) PodmanQuadlet {
	return PodmanQuadlet{Name: "shop-web.container", Content: &text}
}

const quadletText = "[Container]\nContainerName=shop-web\nImage=registry.example.com/shop/web:2.4\n\n[Install]\nWantedBy=multi-user.target\n"

func TestPodmanQuadletRejectedInputs(t *testing.T) {
	t.Parallel()
	text := quadletText
	for message, q := range map[string]PodmanQuadlet{
		"name: podman_quadlet.name is required: the Quadlet filename, such as shop-web.container":                                                {Content: &text},
		"state: podman_quadlet.state must be present, file_and_service_removed_named_volumes_kept, or volume_and_its_data_removed":               {Name: "shop-web.container", Content: &text, State: "absent"},
		"podman_quadlet requires exactly one of content or template":                                                                             {Name: "shop-web.container"},
		"podman_quadlet requires exactly one of content or template ":                                                                            {Name: "shop-web.container", Content: &text, Template: "files/shop-web.container.tmpl"},
		"state: podman_quadlet with state: file_and_service_removed_named_volumes_kept accepts only name, in_systemd_of_user_account, and state": {Name: "shop-web.container", Content: &text, State: QuadletFileAndServiceRemovedNamedVolumesKept},
		"state: podman_quadlet with state: volume_and_its_data_removed accepts only name, in_systemd_of_user_account, and state":                 {Name: "shop-web-data.volume", Template: "files/x", State: QuadletVolumeAndItsDataRemoved},
	} {
		if err := ValidatePodmanQuadlet(q); err == nil || err.Error() != strings.TrimSpace(message) {
			t.Errorf("%+v: %v; want %s", q, err, message)
		}
	}
	for _, q := range []PodmanQuadlet{quadletContainer(text), {Name: "shop-web.container", State: QuadletFileAndServiceRemovedNamedVolumesKept}, {Name: "shop-web-data.volume", State: QuadletVolumeAndItsDataRemoved}} {
		if err := ValidatePodmanQuadlet(q); err != nil {
			t.Errorf("%+v: %v", q, err)
		}
	}
	for _, name := range []string{"shop-web.service", "shop-web", "shop web.container", "../shop-web.container", ".container", "shop-web.containers"} {
		if err := quadletNameFor("")(name); err == nil || !strings.Contains(err.Error(), "name must be a Quadlet filename that ends in .container, .volume, .network, .pod, .kube, .image, or .build") {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Only a volume can be removed with its data.
	if err := quadletNameFor(QuadletVolumeAndItsDataRemoved)("shop-web.container"); err == nil || err.Error() != "state: volume_and_its_data_removed is only for a .volume file; remove shop-web.container with state: file_and_service_removed_named_volumes_kept" {
		t.Error(err)
	}
	if err := quadletNameFor(QuadletVolumeAndItsDataRemoved)("shop-web-data.volume"); err != nil {
		t.Error(err)
	}
}

func TestQuadletGeneratedServiceNames(t *testing.T) {
	t.Parallel()
	for name, service := range map[string]string{
		"shop-web.container": "shop-web.service", "shop-web-data.volume": "shop-web-data-volume.service", "shop.network": "shop-network.service",
		"shop.pod": "shop-pod.service", "shop.kube": "shop.service", "shop-web.image": "shop-web-image.service", "shop-web.build": "shop-web-build.service",
		"worker@.container": "worker@.service",
	} {
		if err := quadletNameFor("")(name); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if got := (PodmanQuadlet{Name: name}).GeneratedService(); got != service {
			t.Errorf("%s generates %s; want %s", name, got, service)
		}
	}
}

// Quadlet needs Podman 4.4: without podman, or with an older one, a run
// fails without another try and names both versions; a dryrun is not
// checked. Nothing is written.
func TestPodmanQuadletNeedsPodman44(t *testing.T) {
	ctx := testContext(t)
	f := newQuadletFixture(t)
	writeState(t, filepath.Join(f.dir, "version"), "4.3.1")
	const old = "podman_quadlet: Podman 4.3.1 is installed, and Quadlet files need Podman 4.4 or newer; install a newer Podman in an earlier node task"
	if r := f.run().PodmanQuadlet(ctx, quadletContainer(quadletText), value.Scope{}); r.Status != "failed" || r.FailureKind != "missing_prerequisite" || r.Reason != old {
		t.Fatalf("%+v", r)
	}
	if r := f.dryrun(t).PodmanQuadlet(ctx, quadletContainer(quadletText), value.Scope{}); r.Status != "not_checked" || r.Reason != old {
		t.Fatalf("%+v", r)
	}
	writeState(t, filepath.Join(f.dir, "version"), "4.4.0")
	if r := f.dryrun(t).PodmanQuadlet(ctx, quadletContainer(quadletText), value.Scope{}); r.Status != "succeeded" || r.Change != "would_change" {
		t.Fatalf("%+v", r)
	}
	// The generator comes with the package.
	writeState(t, filepath.Join(f.dir, "version"), "5.4.2")
	generator := quadletGenerator
	quadletGenerator = filepath.Join(f.dir, "no-such-generator")
	if r := f.run().PodmanQuadlet(ctx, quadletContainer(quadletText), value.Scope{}); r.FailureKind != "missing_prerequisite" || r.Reason != "podman_quadlet: Podman's Quadlet generator "+quadletGenerator+" is missing; install the podman package in an earlier node task" {
		t.Fatalf("%+v", r)
	}
	quadletGenerator = generator
	setWorkerEnvironment(t, "PATH", t.TempDir())
	missing := "podman_quadlet: " + missingProgramError("podman").Error()
	if r := f.run().PodmanQuadlet(ctx, quadletContainer(quadletText), value.Scope{}); r.FailureKind != "missing_prerequisite" || r.Reason != missing {
		t.Fatalf("%+v", r)
	}
	if r := f.dryrun(t).PodmanQuadlet(ctx, quadletContainer(quadletText), value.Scope{}); r.Status != "not_checked" || r.Reason != missing {
		t.Fatalf("%+v", r)
	}
	if _, err := os.Stat(f.quadlets); !os.IsNotExist(err) {
		t.Fatal("something was written", err)
	}
}

// An older Podman ignores a Quadlet file of a kind it does not know, without
// a word. The task refuses the kind and names the installed version and the
// one that the kind needs; a dryrun is not checked. Nothing is installed.
func TestPodmanQuadletRefusesAKindThisPodmanDoesNotKnow(t *testing.T) {
	ctx := testContext(t)
	f := newQuadletFixture(t)
	text := "[Pod]\n"
	for _, tc := range []struct{ name, installed, needed string }{
		{"shop.pod", "4.9.3", "5.0"}, {"shop-web.build", "5.1.0", "5.2"}, {"shop-web.image", "4.7.2", "4.8"}, {"shop-web.build", "4.9.3", "5.2"},
	} {
		writeState(t, filepath.Join(f.dir, "version"), tc.installed)
		q := PodmanQuadlet{Name: tc.name, Content: &text}
		kind := filepath.Ext(tc.name)
		want := "podman_quadlet: Podman " + tc.installed + " is installed, and " + kind + " Quadlet files need Podman " + tc.needed + " or newer; Podman " + tc.installed + " would ignore " + tc.name + " without a word"
		if r := f.run().PodmanQuadlet(ctx, q, nil); r.Status != "failed" || r.FailureKind != "missing_prerequisite" || r.Reason != want {
			t.Errorf("%s on %s: %+v", tc.name, tc.installed, r)
		}
		if r := f.dryrun(t).PodmanQuadlet(ctx, q, nil); r.Status != "not_checked" || r.Reason != want {
			t.Errorf("%s on %s, dryrun: %+v", tc.name, tc.installed, r)
		}
	}
	if _, err := os.Stat(f.quadlets); !os.IsNotExist(err) {
		t.Fatal("something was written", err)
	}
	// Each kind is taken from the version that added it; the four first
	// kinds from 4.4.
	for name, version := range map[string]string{"a.container": "4.4.0", "a.volume": "4.4.0", "a.kube": "4.4.0", "a.network": "4.4.0", "a.image": "4.8.0", "a.pod": "5.0.0", "a.build": "5.2.0"} {
		writeState(t, filepath.Join(f.dir, "version"), version)
		if r := f.dryrun(t).PodmanQuadlet(ctx, PodmanQuadlet{Name: name, Content: &text}, nil); r.Status != "succeeded" || r.Change != "would_change" {
			t.Errorf("%s on %s: %+v", name, version, r)
		}
	}
	// Every kind the name rule accepts has its version.
	for kind := range quadletServiceSuffixes {
		if quadletKindVersions[kind] == nil {
			t.Errorf("%s has no first version", kind)
		}
	}
}

// The first run makes the directory, checks the new file together with the
// installed ones, installs it, and reloads systemd. It starts nothing. The
// next run changes nothing, and a dryrun writes nothing.
func TestPodmanQuadletInstallsChecksAndReloads(t *testing.T) {
	ctx := testContext(t)
	f := newQuadletFixture(t)
	q := quadletContainer(quadletText)
	generated := map[string]any{"generated_service": "shop-web.service"}

	r := f.dryrun(t).PodmanQuadlet(ctx, q, nil)
	if r.Status != "succeeded" || r.Change != "would_change" || r.FileChange == nil || !reflect.DeepEqual(r.Value, generated) ||
		r.CheckNewFile == nil || r.CheckNewFile.Status != "skipped" || r.CheckNewFile.Reason != "not checked: Podman's generator checks the Quadlet file when it is installed" {
		t.Fatalf("%+v", r)
	}
	if _, err := os.Stat(f.quadlets); !os.IsNotExist(err) {
		t.Fatal("the dryrun made the directory", err)
	}
	if calls := changing(takeCalls(t, f.dir), "podman version", "systemctl show"); calls != nil {
		t.Fatal("the dryrun ran", calls)
	}

	r = f.run().PodmanQuadlet(ctx, q, nil)
	if r.Status != "succeeded" || r.Change != "changed" || r.FileChange == nil || r.FileChange.Path != filepath.Join(f.quadlets, "shop-web.container") || !reflect.DeepEqual(r.Value, generated) {
		t.Fatalf("%+v", r)
	}
	if f.installed(t, "shop-web.container") != quadletText {
		t.Fatal("the file was not installed")
	}
	for _, path := range []string{f.quadlets, filepath.Join(f.quadlets, "shop-web.container")} {
		want := os.FileMode(0644)
		if path == f.quadlets {
			want = 0755
		}
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != want {
			t.Fatal(path, info, err)
		}
	}
	// The generator saw a copy of the new file in a private directory first,
	// then the stand-in of the same name, then the installed files. The
	// private directory is gone.
	directories := strings.Split(strings.TrimSpace(readState(t, filepath.Join(f.dir, "generator-directories"))), ":")
	private := filepath.Dir(directories[0])
	if len(directories) != 3 || filepath.Dir(private) != f.privateTaskDirectoryRoot || !strings.HasPrefix(filepath.Base(private), "podman_quadlet-") ||
		directories[0] != filepath.Join(private, "new-file") || directories[1] != filepath.Join(private, "stand-in") || directories[2] != f.quadlets {
		t.Fatal(directories)
	}
	if left, _ := os.ReadDir(f.privateTaskDirectoryRoot); len(left) != 0 {
		t.Fatal("the check left its private directory behind", left)
	}
	calls := changing(takeCalls(t, f.dir), "podman version", "systemctl show")
	if !reflect.DeepEqual(calls, []string{"podman-system-generator -dryrun", "systemctl daemon-reload"}) {
		t.Fatal(calls)
	}
	if left := f.leftovers(t); left != nil {
		t.Fatal(left)
	}

	// Unchanged: the file is checked again, and nothing is reloaded.
	for _, run := range []Runtime{f.run(), f.dryrun(t)} {
		if r := run.PodmanQuadlet(ctx, q, nil); r.Status != "succeeded" || r.Change != "unchanged" || !reflect.DeepEqual(r.Value, generated) {
			t.Fatalf("%+v", r)
		}
	}
	if calls := changing(takeCalls(t, f.dir), "podman version", "systemctl show", "podman-system-generator -dryrun"); calls != nil {
		t.Fatal(calls)
	}

	// A changed file is replaced and loaded; a dryrun shows the difference.
	changed := quadletContainer(strings.Replace(quadletText, "web:2.4", "web:2.5", 1))
	r = f.dryrun(t).PodmanQuadlet(ctx, changed, nil)
	if r.Change != "would_change" || !strings.Contains(r.FileChange.Diff, "+Image=registry.example.com/shop/web:2.5") || f.installed(t, "shop-web.container") != quadletText {
		t.Fatalf("%+v", r)
	}
	if r := f.run().PodmanQuadlet(ctx, changed, nil); r.Change != "changed" || !strings.Contains(f.installed(t, "shop-web.container"), "web:2.5") {
		t.Fatalf("%+v", r)
	}
}

// A file the generator rejects is not installed, the live file stays, and
// the task fails with the generator's message about the file where it goes.
// A good new file beside another installed file that the generator rejects
// is installed.
func TestPodmanQuadletCheckRejectsABadFile(t *testing.T) {
	ctx := testContext(t)
	f := newQuadletFixture(t)
	if r := f.run().PodmanQuadlet(ctx, quadletContainer(quadletText), nil); r.Status != "succeeded" {
		t.Fatalf("%+v", r)
	}
	takeCalls(t, f.dir)
	r := f.run().PodmanQuadlet(ctx, quadletContainer(quadletText+"INVALID=1\n"), nil)
	const failed = "podman_quadlet: Podman's check of the new Quadlet file failed: the Quadlet generator of Podman 5.4.2 makes no "
	want := failed + "shop-web.service from shop-web.container: converting \"shop-web.container\": unsupported key 'INVALID' in group 'Container' in " + filepath.Join(f.quadlets, "shop-web.container")
	if r.Status != "failed" || r.FailureKind != "validation" || r.Change != "unchanged" || r.Reason != want {
		t.Fatalf("%+v", r)
	}
	rejected := func(what string) {
		t.Helper()
		if f.installed(t, "shop-web.container") != quadletText || f.leftovers(t) != nil {
			t.Fatal(what, "reached the directory", f.leftovers(t))
		}
		if calls := changing(takeCalls(t, f.dir), "podman version", "systemctl show", "podman-system-generator -dryrun"); calls != nil {
			t.Fatal(what, calls)
		}
		if left, _ := os.ReadDir(f.privateTaskDirectoryRoot); len(left) != 0 {
			t.Fatal(what, "left the check's private directory behind", left)
		}
	}
	rejected("the rejected file")
	// A new file that the generator cannot read at all. The generator
	// passes over it and reads the next file of the same name; that must
	// never be the good installed file. Podman 5 says what it cannot read.
	const cannotRead = "the generator cannot read the file as a Quadlet file, in which each line is a [Section] name, a Key=value pair, or a comment"
	r = f.run().PodmanQuadlet(ctx, quadletContainer("[Container]\nUNREADABLE\n"), nil)
	want = failed + "shop-web.service from shop-web.container: " + cannotRead + "; it says: error loading \"" + filepath.Join(f.quadlets, "shop-web.container") + "\", file contains line 2: UNREADABLE which is not a key-value pair, group, or comment"
	if r.Status != "failed" || r.FailureKind != "validation" || r.Change != "unchanged" || r.Reason != want {
		t.Fatalf("%+v", r)
	}
	rejected("the unreadable file")
	// Podman 4 says nothing about it and exits with 0.
	r = f.run().PodmanQuadlet(ctx, quadletContainer("[Container]\nUNREADABLE SILENTLY\n"), nil)
	if r.Status != "failed" || r.FailureKind != "validation" || r.Change != "unchanged" || r.Reason != failed+"shop-web.service from shop-web.container: "+cannotRead {
		t.Fatalf("%+v", r)
	}
	rejected("the unreadable file about which the generator says nothing")
	// The same for a file of another kind, and for a file that is not
	// installed yet.
	for name, text := range map[string]string{"shop-web-data.volume": "[Volume]\nUNREADABLE SILENTLY\n", "shop.network": "[Network]\nUNREADABLE SILENTLY\n", "other.container": "UNREADABLE SILENTLY\n"} {
		r = f.run().PodmanQuadlet(ctx, PodmanQuadlet{Name: name, Content: &text}, nil)
		if r.Status != "failed" || r.FailureKind != "validation" || !strings.HasSuffix(r.Reason, " from "+name+": "+cannotRead) {
			t.Fatalf("%s: %+v", name, r)
		}
		if _, err := os.Stat(filepath.Join(f.quadlets, name)); !os.IsNotExist(err) {
			t.Fatal(name, "was installed", err)
		}
	}
	takeCalls(t, f.dir)
	// A file from which the generator makes no service of the expected name
	// and says nothing, as for a file that sets ServiceName=: the stand-in
	// makes nothing from a .pod file.
	pod := "[Pod]\nPodName=shop\n"
	r = f.run().PodmanQuadlet(ctx, PodmanQuadlet{Name: "shop.pod", Content: &pod}, nil)
	if r.Status != "failed" || r.FailureKind != "validation" || !strings.Contains(r.Reason, "makes no shop-pod.service from shop.pod: Podman's Quadlet generator gave no reason; MMX expects the service name derived from the Quadlet filename, so the Quadlet file must not set ServiceName=") {
		t.Fatalf("%+v", r)
	}
	if _, err := os.Stat(filepath.Join(f.quadlets, "shop.pod")); !os.IsNotExist(err) {
		t.Fatal("a file that becomes no service was installed", err)
	}
	// Another installed file is bad: the generator exits with 1, and the
	// good new file is still accepted.
	writeState(t, filepath.Join(f.quadlets, "other.container"), "[Container]\nINVALID=1\n")
	volume := "[Volume]\nVolumeName=shop-web-data\n"
	r = f.run().PodmanQuadlet(ctx, PodmanQuadlet{Name: "shop-web-data.volume", Content: &volume}, nil)
	if r.Status != "succeeded" || r.Change != "changed" || !reflect.DeepEqual(r.Value, map[string]any{"generated_service": "shop-web-data-volume.service"}) {
		t.Fatalf("%+v", r)
	}
}

// A run that installed the file and stopped before systemd's reload: the
// failure keeps the change, and the next run finds the file unchanged, sees
// that systemd has no service for it yet, and reloads. So does a run that
// finds systemd's own flag set. A dryrun reports the reload that the run
// would make.
func TestPodmanQuadletFinishesAReloadThatARunMissed(t *testing.T) {
	ctx := testContext(t)
	f := newQuadletFixture(t)
	q := quadletContainer(quadletText)
	stopped := f.run()
	stopped.BeforeMutation = func(resource string) error {
		if resource == "systemd manager" {
			return errors.New("the node job was stopped")
		}
		return nil
	}
	r := stopped.PodmanQuadlet(ctx, q, nil)
	if r.Status != "failed" || r.Change != "changed" || r.FileChange == nil || f.installed(t, "shop-web.container") != quadletText {
		t.Fatalf("%+v", r)
	}
	takeCalls(t, f.dir)
	wouldReload := func() {
		t.Helper()
		r := f.dryrun(t).PodmanQuadlet(ctx, q, nil)
		if r.Status != "succeeded" || r.Change != "would_change" || r.FileChange != nil || r.Reason != "would reload systemd, which has not yet read the changed unit definitions" {
			t.Fatalf("%+v", r)
		}
	}
	wouldReload()
	r = f.run().PodmanQuadlet(ctx, q, nil)
	if r.Status != "succeeded" || r.Change != "changed" || r.FileChange != nil {
		t.Fatalf("%+v", r)
	}
	if calls := changing(takeCalls(t, f.dir), "podman version", "systemctl show", "podman-system-generator -dryrun"); !reflect.DeepEqual(calls, []string{"systemctl daemon-reload"}) {
		t.Fatal(calls)
	}
	for _, run := range []Runtime{f.run(), f.dryrun(t)} {
		if r := run.PodmanQuadlet(ctx, q, nil); r.Change != "unchanged" || r.Reason != "" {
			t.Fatalf("%+v", r)
		}
	}
	writeState(t, filepath.Join(f.dir, "need-reload"), "yes\n")
	wouldReload()
	if r := f.run().PodmanQuadlet(ctx, q, nil); r.Status != "succeeded" || r.Change != "changed" {
		t.Fatalf("%+v", r)
	}
	if r := f.run().PodmanQuadlet(ctx, q, nil); r.Change != "unchanged" {
		t.Fatalf("%+v", r)
	}
}

// Podman's generator reads every file of the directories it is given, and
// waits without end on one that is not a regular file. The check ends it
// after its time limit and says what makes it wait; nothing is installed.
func TestPodmanQuadletCheckEndsAGeneratorThatWaits(t *testing.T) {
	f := newQuadletFixture(t)
	previous := quadletGeneratorTimeLimit
	quadletGeneratorTimeLimit = 300 * time.Millisecond
	t.Cleanup(func() { quadletGeneratorTimeLimit = previous })
	writeState(t, filepath.Join(f.dir, "generator-waits"), "")
	r := f.run().PodmanQuadlet(testContext(t), quadletContainer(quadletText), nil)
	want := "podman_quadlet: Podman's check of the new Quadlet file failed: the Quadlet generator of Podman 5.4.2 makes no shop-web.service from shop-web.container: Podman's generator did not end within 300ms; it reads every file in " + f.quadlets + " and waits without end on one that is not a regular file, such as a named pipe"
	if r.Status != "failed" || r.FailureKind != "validation" || r.Change != "unchanged" || r.Reason != want {
		t.Fatalf("%+v", r)
	}
	if _, err := os.Stat(filepath.Join(f.quadlets, "shop-web.container")); !os.IsNotExist(err) {
		t.Fatal("the file was installed", err)
	}
}

// The check of an account's file runs the generator as that account, on
// private directories that are the account's: root never reads the account's
// Quadlet directory for it. This test's own account stands in for the
// ordinary account.
func TestPodmanQuadletCheckRunsTheGeneratorAsTheAccount(t *testing.T) {
	f := newQuadletFixture(t)
	home := t.TempDir()
	uid, gid := os.Geteuid(), os.Getegid()
	m := UnitManager{Account: "shop", Home: home, UID: uid, GID: gid,
		credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), NoSetGroups: true}, env: map[string]string{"HOME": home, "MMX_TEST_ACCOUNT_ENVIRONMENT": "yes"}}
	text := quadletText
	q := locateQuadlet(PodmanQuadlet{Name: "shop-web.container", Content: &text, InSystemdOfUserAccount: "shop"}, m)
	directory := filepath.Dir(q.File().Path)
	// The new file as the file module leaves it for the check.
	writeState(t, filepath.Join(directory, ".mmx-candidate-test", "shop-web.container"), quadletText)
	parent, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	r := f.run().checkQuadletCandidate(testContext(t), m, q, "5.4.2", parent, filepath.Join(directory, ".mmx-candidate-test", "shop-web.container"))
	if r.Status != "succeeded" {
		t.Fatalf("%+v", r)
	}
	if calls := takeCalls(t, f.dir); !reflect.DeepEqual(calls, []string{"podman-system-generator -dryrun -user"}) {
		t.Fatal(calls)
	}
	if user := strings.TrimSpace(readState(t, filepath.Join(f.dir, "generator-user"))); user != strconv.Itoa(uid) {
		t.Fatal("the generator ran as", user)
	}
	directories := strings.Split(strings.TrimSpace(readState(t, filepath.Join(f.dir, "generator-directories"))), ":")
	if len(directories) != 3 || directories[2] != directory || filepath.Dir(filepath.Dir(directories[0])) != f.privateTaskDirectoryRoot {
		t.Fatal(directories)
	}
	if left, _ := os.ReadDir(f.privateTaskDirectoryRoot); len(left) != 0 {
		t.Fatal("the check left its private directory behind", left)
	}
	// A new file that is not a regular file is not read.
	if err := os.Remove(filepath.Join(directory, ".mmx-candidate-test", "shop-web.container")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(f.dir, "version"), filepath.Join(directory, ".mmx-candidate-test", "shop-web.container")); err != nil {
		t.Fatal(err)
	}
	r = f.run().checkQuadletCandidate(testContext(t), m, q, "5.4.2", parent, filepath.Join(directory, ".mmx-candidate-test", "shop-web.container"))
	if r.Status != "failed" || !strings.HasPrefix(r.Reason, "podman_quadlet: the private directory for Podman's check of the new Quadlet file: ") || strings.Contains(r.Reason, "5.4.2") {
		t.Fatalf("%+v", r)
	}
	if calls := takeCalls(t, f.dir); calls != nil {
		t.Fatal(calls)
	}
}

// file_and_service_removed_named_volumes_kept stops the generated service if it runs,
// deletes the file, and reloads systemd; a rerun is unchanged. A dryrun
// says what it would do and does none of it.
func TestPodmanQuadletRemoval(t *testing.T) {
	ctx := testContext(t)
	f := newQuadletFixture(t)
	if r := f.run().PodmanQuadlet(ctx, quadletContainer(quadletText), nil); r.Status != "succeeded" {
		t.Fatalf("%+v", r)
	}
	writeState(t, filepath.Join(f.dir, "active"), "shop-web.service\n")
	path := filepath.Join(f.quadlets, "shop-web.container")
	removal := PodmanQuadlet{Name: "shop-web.container", State: QuadletFileAndServiceRemovedNamedVolumesKept}
	takeCalls(t, f.dir)
	r := f.dryrun(t).PodmanQuadlet(ctx, removal, nil)
	if r.Status != "succeeded" || r.Change != "would_change" || r.Reason != "would stop shop-web.service, then delete "+path+", then reload systemd" || !reflect.DeepEqual(r.Value, map[string]any{"generated_service": "shop-web.service"}) {
		t.Fatalf("%+v", r)
	}
	if calls := changing(takeCalls(t, f.dir), "systemctl show"); calls != nil || f.installed(t, "shop-web.container") != quadletText {
		t.Fatal("the dryrun changed something", calls)
	}
	r = f.run().PodmanQuadlet(ctx, removal, nil)
	if r.Status != "succeeded" || r.Change != "changed" || r.FileChange == nil || r.FileChange.Action != "remove" || r.Reason != "stopped shop-web.service, then deleted "+path {
		t.Fatalf("%+v", r)
	}
	// A container's removal needs no podman command: stopping the service
	// removes the container.
	if calls := changing(takeCalls(t, f.dir), "systemctl show"); !reflect.DeepEqual(calls, []string{"systemctl stop -- shop-web.service", "systemctl daemon-reload"}) {
		t.Fatal(calls)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("the file is still there", err)
	}
	for _, run := range []Runtime{f.run(), f.dryrun(t)} {
		if r := run.PodmanQuadlet(ctx, removal, nil); r.Status != "succeeded" || r.Change != "unchanged" || r.Reason != "" {
			t.Fatalf("%+v", r)
		}
	}
	// Nothing was ever installed, not even the directory.
	empty := newQuadletFixture(t)
	if r := empty.run().PodmanQuadlet(ctx, removal, nil); r.Status != "succeeded" || r.Change != "unchanged" {
		t.Fatalf("%+v", r)
	}
	if _, err := os.Stat(empty.quadlets); !os.IsNotExist(err) {
		t.Fatal("a removal made the directory", err)
	}
}

// file_and_service_removed_named_volumes_kept on a .volume file keeps the volume and its
// data, and asks podman nothing. systemd stops the container that uses the
// volume together with the volume's service; a dryrun and the run name both.
func TestPodmanQuadletRemovalKeepsANamedVolume(t *testing.T) {
	ctx := testContext(t)
	f := newQuadletFixture(t)
	volume := "[Volume]\nVolumeName=shop-web-data\n"
	if r := f.run().PodmanQuadlet(ctx, PodmanQuadlet{Name: "shop-web-data.volume", Content: &volume}, nil); r.Status != "succeeded" {
		t.Fatalf("%+v", r)
	}
	path := filepath.Join(f.quadlets, "shop-web-data.volume")
	podmanVolume := filepath.Join(f.dir, "volume-shop-web-data")
	writeState(t, podmanVolume, "")
	// The container runs and requires the volume's service; another unit
	// that requires it does not run.
	writeState(t, filepath.Join(f.dir, "active"), "shop-web-data-volume.service\nshop-web.service\n")
	writeState(t, filepath.Join(f.dir, "required-by-shop-web-data-volume.service"), "shop-web.service\nshop-report.service\n")
	takeCalls(t, f.dir)
	removal := PodmanQuadlet{Name: "shop-web-data.volume", State: QuadletFileAndServiceRemovedNamedVolumesKept}
	const stops = "shop-web-data-volume.service and with it shop-web.service, which systemd stops when shop-web-data-volume.service stops"
	if r := f.dryrun(t).PodmanQuadlet(ctx, removal, nil); r.Change != "would_change" || r.Reason != "would stop "+stops+", then delete "+path+", then reload systemd" {
		t.Fatalf("%+v", r)
	}
	if r := f.run().PodmanQuadlet(ctx, removal, nil); r.Status != "succeeded" || r.Change != "changed" || r.Reason != "stopped "+stops+", then deleted "+path {
		t.Fatalf("%+v", r)
	}
	if calls := changing(takeCalls(t, f.dir), "systemctl"); calls != nil {
		t.Fatal(calls)
	}
	if _, err := os.Stat(podmanVolume); err != nil {
		t.Fatal("the volume was removed", err)
	}
}

// The example's two removal playbooks in turn: file_and_service_removed_named_volumes_kept
// removes the .volume file, which alone held the volume's name, and keeps
// the volume. A later volume_and_its_data_removed for the same file still
// deletes that volume, because MMX recorded its name.
func TestPodmanQuadletDeletesTheVolumeThatAnEarlierRemovalKept(t *testing.T) {
	ctx := testContext(t)
	f := newQuadletFixture(t)
	volume := "[Volume]\nVolumeName=shop-web-data\n"
	present := PodmanQuadlet{Name: "shop-web-data.volume", Content: &volume}
	kept := PodmanQuadlet{Name: "shop-web-data.volume", State: QuadletFileAndServiceRemovedNamedVolumesKept}
	withData := PodmanQuadlet{Name: "shop-web-data.volume", State: QuadletVolumeAndItsDataRemoved}
	path := filepath.Join(f.quadlets, "shop-web-data.volume")
	podmanVolume := filepath.Join(f.dir, "volume-shop-web-data")
	record := filepath.Join(quadletKeptVolumeRecordRoot, "system", "shop-web-data.volume")
	install := func() {
		t.Helper()
		if r := f.run().PodmanQuadlet(ctx, present, nil); r.Status != "succeeded" || r.Change != "changed" {
			t.Fatalf("%+v", r)
		}
		writeState(t, podmanVolume, "")
	}
	install()
	// A dryrun of the removal records nothing.
	if r := f.dryrun(t).PodmanQuadlet(ctx, kept, nil); r.Change != "would_change" {
		t.Fatalf("%+v", r)
	}
	if _, err := os.Stat(quadletKeptVolumeRecordRoot); !os.IsNotExist(err) {
		t.Fatal("a dryrun wrote the record", err)
	}
	if r := f.run().PodmanQuadlet(ctx, kept, nil); r.Status != "succeeded" || r.Change != "changed" || r.Reason != "deleted "+path {
		t.Fatalf("%+v", r)
	}
	if recorded := readState(t, record); recorded != "shop-web-data\n" {
		t.Fatalf("the record holds %q", recorded)
	}
	if info, err := os.Stat(record); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
	// The file is gone and the volume is there. The removal with data finds
	// it, in a dryrun and in a run.
	takeCalls(t, f.dir)
	if r := f.dryrun(t).PodmanQuadlet(ctx, withData, nil); r.Status != "succeeded" || r.Change != "would_change" || r.Reason != "would delete the Podman volume shop-web-data with the data in it, then reload systemd" {
		t.Fatalf("%+v", r)
	}
	if calls := changing(takeCalls(t, f.dir), "systemctl show", "podman volume exists"); calls != nil || readState(t, record) == "" {
		t.Fatal("the dryrun changed something", calls)
	}
	r := f.run().PodmanQuadlet(ctx, withData, nil)
	if r.Status != "succeeded" || r.Change != "changed" || r.Reason != "deleted the Podman volume shop-web-data with the data in it" {
		t.Fatalf("%+v", r)
	}
	if calls := changing(takeCalls(t, f.dir), "systemctl show", "podman volume exists"); !reflect.DeepEqual(calls, []string{"podman volume rm shop-web-data", "systemctl daemon-reload"}) {
		t.Fatal(calls)
	}
	for _, gone := range []string{podmanVolume, record} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Fatal(gone, "is still there", err)
		}
	}
	// Complete: the file is gone, and so is the record, so only Podman's
	// default name can be checked, and the result says so.
	const onlyDefault = " is gone, so MMX could check only Podman's default name for its volume, and Podman has no volume named systemd-shop-web-data"
	for _, run := range []Runtime{f.run(), f.dryrun(t)} {
		if r := run.PodmanQuadlet(ctx, withData, nil); r.Status != "succeeded" || r.Change != "unchanged" || r.Reason != path+onlyDefault {
			t.Fatalf("%+v", r)
		}
	}
	// A file that is installed again names its volume itself: the record of
	// the earlier removal goes, and a changed name counts.
	install()
	if r := f.run().PodmanQuadlet(ctx, kept, nil); r.Status != "succeeded" {
		t.Fatalf("%+v", r)
	}
	renamed := "[Volume]\nVolumeName=shop-web-data-2\n"
	if r := f.run().PodmanQuadlet(ctx, PodmanQuadlet{Name: "shop-web-data.volume", Content: &renamed}, nil); r.Status != "succeeded" {
		t.Fatalf("%+v", r)
	}
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Fatal("the record outlived the file's new installation", err)
	}
	writeState(t, filepath.Join(f.dir, "volume-shop-web-data-2"), "")
	if r := f.run().PodmanQuadlet(ctx, withData, nil); r.Status != "succeeded" || r.Reason != "deleted the Podman volume shop-web-data-2 with the data in it, then deleted "+path {
		t.Fatalf("%+v", r)
	}
	if _, err := os.Stat(podmanVolume); err != nil {
		t.Fatal("the volume of the earlier name was deleted", err)
	}
	// A recorded volume that only podman could delete: without podman the
	// task does not report that nothing is left.
	install()
	if r := f.run().PodmanQuadlet(ctx, kept, nil); r.Status != "succeeded" {
		t.Fatalf("%+v", r)
	}
	// PATH keeps the stand-in systemctl and the tools it calls, and has no
	// podman, whatever this machine has installed.
	if err := os.Remove(filepath.Join(f.dir, "podman")); err != nil {
		t.Fatal(err)
	}
	tools := t.TempDir()
	for _, tool := range []string{"touch", "grep", "cat", "tr"} {
		found, err := exec.LookPath(tool)
		if err == nil {
			err = os.Symlink(found, filepath.Join(tools, tool))
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	setWorkerEnvironment(t, "PATH", f.dir+string(os.PathListSeparator)+tools)
	if r := f.run().PodmanQuadlet(ctx, withData, nil); r.Status != "failed" || r.FailureKind != "missing_prerequisite" || r.Reason != "podman_quadlet: "+missingProgramError("podman").Error() {
		t.Fatalf("%+v", r)
	}
	if readState(t, record) != "shop-web-data\n" {
		t.Fatal("the record went although the volume stays")
	}
	// Without a record nothing says that a volume is left; the result says
	// what could not be asked.
	if err := os.Remove(record); err != nil {
		t.Fatal(err)
	}
	if r := f.run().PodmanQuadlet(ctx, withData, nil); r.Status != "succeeded" || r.Change != "unchanged" || r.Reason != path+" is gone and the program podman is not installed, so MMX could not ask about a volume" {
		t.Fatalf("%+v", r)
	}
}

// A volume is removed with its data only by volume_and_its_data_removed,
// and before anything is stopped. While a container uses the volume, podman
// refuses: the task fails, the container keeps running, and the file stays.
// The container's service requires the volume file's service, so stopping
// that one first would stop the container and free the volume for deletion.
func TestPodmanQuadletVolumeAndItsDataRemoved(t *testing.T) {
	ctx := testContext(t)
	f := newQuadletFixture(t)
	volume := "[Volume]\nVolumeName=shop-web-data\n"
	if r := f.run().PodmanQuadlet(ctx, PodmanQuadlet{Name: "shop-web-data.volume", Content: &volume}, nil); r.Status != "succeeded" {
		t.Fatalf("%+v", r)
	}
	path := filepath.Join(f.quadlets, "shop-web-data.volume")
	podmanVolume := filepath.Join(f.dir, "volume-shop-web-data")
	writeState(t, podmanVolume, "")
	// The container runs, uses the volume, and requires the volume's service.
	writeState(t, filepath.Join(f.dir, "active"), "shop-web-data-volume.service\nshop-web.service\n")
	writeState(t, filepath.Join(f.dir, "required-by-shop-web-data-volume.service"), "shop-web.service\n")
	writeState(t, filepath.Join(f.dir, "used-by-container-of"), "shop-web.service\n")
	removal := PodmanQuadlet{Name: "shop-web-data.volume", State: QuadletVolumeAndItsDataRemoved}
	r := f.dryrun(t).PodmanQuadlet(ctx, removal, nil)
	if r.Change != "would_change" || r.Reason != "would delete the Podman volume shop-web-data with the data in it, then stop shop-web-data-volume.service and with it shop-web.service, which systemd stops when shop-web-data-volume.service stops, then delete "+path+", then reload systemd" {
		t.Fatalf("%+v", r)
	}
	takeCalls(t, f.dir)
	r = f.run().PodmanQuadlet(ctx, removal, nil)
	if r.Status != "failed" || r.Change != "unchanged" || !strings.Contains(r.Reason, "podman_quadlet: podman volume rm shop-web-data: Error: volume shop-web-data is being used by the following container(s)") {
		t.Fatalf("%+v", r)
	}
	if calls := changing(takeCalls(t, f.dir), "systemctl show", "podman volume exists"); !reflect.DeepEqual(calls, []string{"podman volume rm shop-web-data"}) {
		t.Fatal("something was stopped or deleted for a volume in use", calls)
	}
	if active := readState(t, filepath.Join(f.dir, "active")); !strings.Contains(active, "shop-web.service") || !strings.Contains(active, "shop-web-data-volume.service") {
		t.Fatal("the container that uses the volume was stopped", active)
	}
	for _, kept := range []string{path, podmanVolume} {
		if _, err := os.Stat(kept); err != nil {
			t.Fatal(kept, "went although the volume is in use", err)
		}
	}
	// The container's Quadlet file was removed first, as the playbook
	// must: its service does not run any more.
	writeState(t, filepath.Join(f.dir, "active"), "shop-web-data-volume.service\n")
	takeCalls(t, f.dir)
	r = f.run().PodmanQuadlet(ctx, removal, nil)
	if r.Status != "succeeded" || r.Change != "changed" || r.Reason != "deleted the Podman volume shop-web-data with the data in it, then stopped shop-web-data-volume.service, then deleted "+path {
		t.Fatalf("%+v", r)
	}
	if calls := changing(takeCalls(t, f.dir), "systemctl show", "podman volume exists"); !reflect.DeepEqual(calls, []string{"podman volume rm shop-web-data", "systemctl stop -- shop-web-data-volume.service", "systemctl daemon-reload"}) {
		t.Fatal(calls)
	}
	for _, gone := range []string{path, podmanVolume} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Fatal(gone, "is still there", err)
		}
	}
	for _, run := range []Runtime{f.run(), f.dryrun(t)} {
		if r := run.PodmanQuadlet(ctx, removal, nil); r.Status != "succeeded" || r.Change != "unchanged" {
			t.Fatalf("%+v", r)
		}
	}
	// The file is gone and a volume of Podman's default name for it is
	// there: it is that file's volume.
	writeState(t, filepath.Join(f.dir, "volume-systemd-shop-web-data"), "")
	if r := f.run().PodmanQuadlet(ctx, removal, nil); r.Status != "succeeded" || r.Change != "changed" || r.Reason != "deleted the Podman volume systemd-shop-web-data with the data in it" {
		t.Fatalf("%+v", r)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "volume-systemd-shop-web-data")); !os.IsNotExist(err) {
		t.Fatal("the volume of the default name is still there", err)
	}
}

// Removing a .network file also removes its Podman network, whichever
// removal state the task names.
func TestPodmanQuadletNetworkRemoval(t *testing.T) {
	ctx := testContext(t)
	f := newQuadletFixture(t)
	network := "[Network]\nSubnet=10.89.7.0/24\n"
	if r := f.run().PodmanQuadlet(ctx, PodmanQuadlet{Name: "shop.network", Content: &network}, nil); r.Status != "succeeded" || !reflect.DeepEqual(r.Value, map[string]any{"generated_service": "shop-network.service"}) {
		t.Fatalf("%+v", r)
	}
	// Without NetworkName= the network has Podman's default name.
	writeState(t, filepath.Join(f.dir, "network-systemd-shop"), "")
	takeCalls(t, f.dir)
	r := f.run().PodmanQuadlet(ctx, PodmanQuadlet{Name: "shop.network", State: QuadletFileAndServiceRemovedNamedVolumesKept}, nil)
	if r.Status != "succeeded" || r.Change != "changed" || r.Reason != "removed the Podman network systemd-shop, then deleted "+filepath.Join(f.quadlets, "shop.network") {
		t.Fatalf("%+v", r)
	}
	if calls := changing(takeCalls(t, f.dir), "systemctl show", "podman network exists"); !reflect.DeepEqual(calls, []string{"podman network rm systemd-shop", "systemctl daemon-reload"}) {
		t.Fatal(calls)
	}
	if r := f.run().PodmanQuadlet(ctx, PodmanQuadlet{Name: "shop.network", State: QuadletFileAndServiceRemovedNamedVolumesKept}, nil); r.Change != "unchanged" {
		t.Fatalf("%+v", r)
	}
}

func TestQuadletObjectName(t *testing.T) {
	t.Parallel()
	volume := quadletObject{"volume", "Volume", "VolumeName"}
	for text, want := range map[string]string{
		"":                           "systemd-shop-web-data",
		"[Volume]\nLabel=app=shop\n": "systemd-shop-web-data",
		"[Unit]\nDescription=x\n\n[Volume]\n# VolumeName=old\nVolumeName = shop\n": "shop",
		"[Volume]\nVolumeName=first\nVolumeName=second\n":                          "second",
		"[Container]\nVolumeName=not-this\n[Volume]\n":                             "systemd-shop-web-data",
	} {
		if got, err := quadletObjectName(text, volume, "shop-web-data.volume"); got != want || err != nil {
			t.Errorf("%q: %q, %v; want %q", text, got, err, want)
		}
	}
	_, err := quadletObjectName("[Volume]\nVolumeName=%N-data\n", volume, "shop-web-data.volume")
	if err == nil || err.Error() != "VolumeName=%N-data in shop-web-data.volume uses a systemd specifier, so MMX cannot tell the volume's name; remove the volume with a shell task that runs podman volume rm" {
		t.Error(err)
	}
}

func TestQuadletGeneratorMessages(t *testing.T) {
	t.Parallel()
	output := "quadlet-generator[1448]: Loading source unit file /etc/containers/systemd/badkey.container\n" +
		"quadlet-generator[1448]: converting \"badkey.container\": unsupported key 'NoSuchKey' in group 'Container' in /etc/containers/systemd/badkey.container\n\n"
	if got := quadletGeneratorMessages(output); got != "converting \"badkey.container\": unsupported key 'NoSuchKey' in group 'Container' in /etc/containers/systemd/badkey.container" {
		t.Fatal(got)
	}
	if got := quadletGeneratorMessages("quadlet-generator[9]: Loading source unit file /x/a.container\n"); got != "" {
		t.Fatal(got)
	}
}

// A Quadlet file of an account goes into the account's home; before the
// node looked the account up, the file has no path and is not tracked.
func TestPodmanQuadletOfAnAccountIsLocatedOnTheNode(t *testing.T) {
	t.Parallel()
	text := quadletText
	q := &PodmanQuadlet{Name: "shop-web.container", Content: &text, InSystemdOfUserAccount: "shop"}
	if q.Located() || q.TrackedInputs() != nil {
		t.Fatal("an account's file has a path before the account is known")
	}
	*q = locateQuadlet(*q, UnitManager{Account: "shop", Home: "/home/shop", UID: 1002, GID: 1003})
	file := q.File()
	if file.Path != "/home/shop/.config/containers/systemd/shop-web.container" || file.Owner != "1002" || file.Group != "1003" || file.Mode != "0644" {
		t.Fatalf("%+v", file)
	}
	in := q.TrackedInputs()
	if len(in) != 1 || in[0].Kind != "unit" || in[0].UnitAccount != "shop" || in[0].UnitName != "shop-web.service" || in[0].Path != file.Path {
		t.Fatalf("%+v", in)
	}
	q.State = QuadletFileAndServiceRemovedNamedVolumesKept
	if file := q.File(); file.State != "absent" || file.HasContents() {
		t.Fatalf("%+v", file)
	}
	system := PodmanQuadlet{Name: "shop-web.container", Content: &text}
	if file := system.File(); file.Path != "/etc/containers/systemd/shop-web.container" || file.Owner != "root" || file.Group != "root" || !system.Located() {
		t.Fatalf("%+v", file)
	}
}
