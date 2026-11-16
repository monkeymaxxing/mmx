package modules

import (
	"reflect"
	"strings"
	"testing"
)

const pacmanModule = "pacman_package"

// pacmanNode gives pacman_package a stand-in pacman. The files of the
// stand-in's directory are the node: installed holds what pacman -Q prints,
// preview what a --print command prints, and installed-after what pacman -Q
// prints once a changing command ran. The lines are those that pacman 7.1
// printed on an Arch Linux test node. preview-error, refresh-fails, and
// change-fails make a command fail with pacman's own words;
// preview-error-before-refresh only until a refresh, and conflict makes
// the transaction fail with the conflict that pacman found. A file
// group-NAME makes NAME a group with installed members (pacman -Qg), and
// provided-NAME a name that an installed package provides (pacman -T).
func pacmanNode(t *testing.T, installed string) func() []string {
	t.Helper()
	calls := stubTools(t, map[string]string{"pacman": `
case "$1" in
-Q) cat "$STUBS/installed" ;;
-Qg) if [ -e "$STUBS/group-$2" ]; then echo "$2 member"; else echo "error: group '$2' was not found" >&2; exit 1; fi ;;
-T) if [ ! -e "$STUBS/provided-$2" ]; then echo "$2"; exit 127; fi ;;
-Sy) if [ -e "$STUBS/refresh-fails" ]; then echo 'error: failed to synchronize all databases (unexpected error)' >&2; exit 1; fi
     touch "$STUBS/refreshed" ;;
-Sup|-Rcp) if [ -e "$STUBS/preview-error" ]; then cat "$STUBS/preview-error" >&2; exit 1; fi
           if [ -e "$STUBS/preview-error-before-refresh" ] && [ ! -e "$STUBS/refreshed" ]; then cat "$STUBS/preview-error-before-refresh" >&2; exit 1; fi
           cat "$STUBS/preview" ;;
-Su|-R) if [ -e "$STUBS/conflict" ]; then
          printf ':: Starting full system upgrade...\n:: nginx-mainline-1.31.8-2 and nginx-1.30.6-3 are in conflict. Remove nginx? [y/N] \n:: nginx-mainline-1.31.8-2 and nginx-1.30.6-3 are in conflict\n'
          printf 'error: unresolvable package conflicts detected\nerror: failed to prepare transaction (conflicting dependencies)\n' >&2; exit 1
        fi
        if [ -e "$STUBS/change-fails" ]; then echo 'error: failed to commit transaction (conflicting files)' >&2; exit 1; fi
        cp "$STUBS/installed-after" "$STUBS/installed"
        if [ -e "$STUBS/read-after-fails" ]; then echo 'error: could not open database' > "$STUBS/installed"; fi ;;
esac`})
	storageStubFile(t, "installed", installed)
	return calls
}

const pacmanBase = "bash 5.3.20-1\nglibc 2.44+r50+g1848099f063e-1\n"

// The commands, as the stand-in notes them.
const (
	pacmanRead    = "pacman -Q"
	pacmanRefresh = "pacman -Sy --noconfirm"
)

func pacmanPrint(arguments string) string {
	return "pacman " + arguments + " --noconfirm --print-format %n %v"
}

func TestOfficialExtraPacmanRefusesInputs(t *testing.T) {
	t.Parallel()
	calls := pacmanNode(t, pacmanBase)
	for _, c := range []struct {
		inputs map[string]any
		reason string
	}{
		{map[string]any{}, "pacman_package.packages: is required, or upgrade_all_packages: true"},
		{map[string]any{"packages": []any{}}, "pacman_package.packages: must not be empty"},
		{map[string]any{"packages": []any{"nginx", "nginx"}}, "pacman_package.packages: nginx is listed twice"},
		{map[string]any{"packages": []any{"-nginx"}}, "pacman_package.packages: -nginx must not start with a hyphen or a dot"},
		{map[string]any{"packages": []any{"extra/nginx"}}, "pacman_package.packages: extra/nginx may hold only letters, digits, and @ . _ + -; a repository in front of the name, such as extra/nginx, is not accepted"},
		{map[string]any{"packages": []any{"nginx"}, "also_remove_packages_that_depend_on_these": true}, "pacman_package.also_remove_packages_that_depend_on_these: requires state: absent"},
		{map[string]any{"upgrade_all_packages": true, "packages": []any{"nginx"}}, "pacman_package.upgrade_all_packages: takes no other input; remove packages"},
		{map[string]any{"upgrade_all_packages": true, "state": "absent"}, "pacman_package.upgrade_all_packages: takes no other input; remove state"},
	} {
		for _, dryrun := range []bool{true, false} {
			storageTaskIsRefused(t, runOfficialExtraModule(t, pacmanModule, c.inputs, dryrun), "unchanged", c.reason)
		}
	}
	if got := calls(); got != nil {
		t.Fatalf("a refused task ran %q", got)
	}
}

// On a node without pacman, a dryrun does not check the task, and a run
// fails once; the reason says which nodes the module is for.
func TestOfficialExtraPacmanWithoutPacman(t *testing.T) {
	t.Parallel()
	stubTools(t, map[string]string{})
	reason := "pacman_package: the program pacman is not installed; pacman_package works only on a node whose package manager is pacman, such as Arch Linux; give the node task a condition on the node fact package_manager"
	storageResultIs(t, runOfficialExtraModule(t, pacmanModule, map[string]any{"packages": []any{"nginx"}}, true), "not_checked", "unknown", reason)
	r := runOfficialExtraModule(t, pacmanModule, map[string]any{"packages": []any{"nginx"}}, false)
	storageResultIs(t, r, "failed", "unchanged", reason)
	if r.FailureKind != "missing_prerequisite" {
		t.Fatalf("failure kind %q", r.FailureKind)
	}
}

// Installed packages, and absent ones in a removal, need nothing: only
// reads run (for an absent name, whether it is a group or a provided
// name), the node's package databases are not refreshed, and the
// fingerprint holds the versions, or absent.
func TestOfficialExtraPacmanUnchanged(t *testing.T) {
	t.Parallel()
	calls := pacmanNode(t, pacmanBase+"nginx 1.30.6-3\n")
	for _, inputs := range []map[string]any{
		{"packages": []any{"nginx", "bash"}},
		{"packages": []any{"apache"}, "state": "absent"},
	} {
		for _, dryrun := range []bool{true, false} {
			before := len(calls())
			r := runOfficialExtraModule(t, pacmanModule, inputs, dryrun)
			storageResultIs(t, r, "succeeded", "unchanged", "")
			want := []string{pacmanRead}
			if inputs["state"] == "absent" {
				want = append(want, "pacman -Qg apache", "pacman -T apache")
			}
			if got := calls()[before:]; !reflect.DeepEqual(got, want) {
				t.Fatalf("calls %q", got)
			}
			if packages := r.ModuleOutputs["changed_packages"]; !reflect.DeepEqual(packages, []any{}) {
				t.Fatalf("changed_packages %#v", packages)
			}
		}
	}
}

// A missing package is installed with a full system upgrade: a dryrun
// previews the transaction against the databases the node has, and a run
// refreshes them, previews, and installs. The result covers every package
// that the transaction changed.
func TestOfficialExtraPacmanInstalls(t *testing.T) {
	t.Parallel()
	calls := pacmanNode(t, pacmanBase)
	storageStubFile(t, "preview", "glibc 2.44+r52+g0a1b2c3d4e5f-1\nmailcap 2.1.54-3\nnginx 1.30.6-3\n")
	storageStubFile(t, "installed-after", "bash 5.3.20-1\nglibc 2.44+r52+g0a1b2c3d4e5f-1\nmailcap 2.1.54-3\nnginx 1.30.6-3\n")
	inputs := map[string]any{"packages": []any{"nginx", "bash"}}
	changes := []any{
		map[string]any{"name": "glibc", "from": "2.44+r50+g1848099f063e-1", "to": "2.44+r52+g0a1b2c3d4e5f-1"},
		map[string]any{"name": "mailcap", "to": "2.1.54-3"},
		map[string]any{"name": "nginx", "to": "1.30.6-3"},
	}
	what := "install mailcap 2.1.54-3, nginx 1.30.6-3; %s glibc from 2.44+r50+g1848099f063e-1 to 2.44+r52+g0a1b2c3d4e5f-1"
	preview := pacmanPrint("-Sup --needed nginx")

	dryrun := runOfficialExtraModule(t, pacmanModule, inputs, true)
	storageResultIs(t, dryrun, "succeeded", "would_change", "would "+strings.Replace(what, "%s", "would upgrade", 1))
	if got := calls(); !reflect.DeepEqual(got, []string{pacmanRead, preview}) {
		t.Fatalf("dryrun calls %q", got)
	}
	if !reflect.DeepEqual(dryrun.ModuleOutputs["changed_packages"], changes) || !strings.Contains(dryrun.Diff, "would run: pacman -Su --needed nginx --noconfirm") {
		t.Fatalf("dryrun %+v", dryrun)
	}

	run := runOfficialExtraModule(t, pacmanModule, inputs, false)
	storageResultIs(t, run, "succeeded", "changed", "installed mailcap 2.1.54-3, nginx 1.30.6-3; upgraded glibc from 2.44+r50+g1848099f063e-1 to 2.44+r52+g0a1b2c3d4e5f-1")
	if got := calls()[2:]; !reflect.DeepEqual(got, []string{pacmanRead, preview, pacmanRefresh, preview, "pacman -Su --needed nginx --noconfirm", pacmanRead}) {
		t.Fatalf("run calls %q", got)
	}
	if !reflect.DeepEqual(run.ModuleOutputs["changed_packages"], changes) {
		t.Fatalf("run %+v", run)
	}
	// The dryrun predicted the versions that the run installed, so a node
	// task block's service restarts in both; the next run is unchanged
	// with the same fingerprint, so the service does not restart again.
	again := runOfficialExtraModule(t, pacmanModule, inputs, false)
	storageResultIs(t, again, "succeeded", "unchanged", "")
	if dryrun.Fingerprint != run.Fingerprint || again.Fingerprint != run.Fingerprint {
		t.Fatalf("fingerprints: dryrun %s, run %s, next run %s", dryrun.Fingerprint, run.Fingerprint, again.Fingerprint)
	}
	// Another version of the package is another fingerprint.
	storageStubFile(t, "installed", pacmanBase+"mailcap 2.1.54-3\nnginx 1.30.7-1\n")
	if other := runOfficialExtraModule(t, pacmanModule, inputs, false); other.Change != "unchanged" || other.Fingerprint == run.Fingerprint {
		t.Fatalf("a newer nginx: %+v", other)
	}
}

// A name that the databases do not have leaves a dryrun not checked, since
// a run refreshes them first; a run refuses it after the refresh, before any
// change. A group's name, or a name that another package provides, would be
// installed again on every run, so it is refused, before a refresh when the
// databases the node has know the name.
func TestOfficialExtraPacmanRefusesUnknownNamesAndGroups(t *testing.T) {
	t.Parallel()
	calls := pacmanNode(t, pacmanBase)
	storageStubFile(t, "preview-error", "error: target not found: nginx-full\n")
	inputs := map[string]any{"packages": []any{"nginx-full"}}
	storageResultIs(t, runOfficialExtraModule(t, pacmanModule, inputs, true), "not_checked", "unknown",
		"pacman_package: the node's package databases have no package nginx-full; a run refreshes them first, and an earlier node task may add the repository that has it")
	storageTaskIsRefused(t, runOfficialExtraModule(t, pacmanModule, inputs, false), "unchanged",
		"pacman_package.packages: pacman's package databases have no package nginx-full; pacman -Ss searches them, and a repository of your own goes into /etc/pacman.conf in an earlier node task")
	preview := pacmanPrint("-Sup --needed nginx-full")
	if got := calls()[2:]; !reflect.DeepEqual(got, []string{pacmanRead, preview, pacmanRefresh, preview}) {
		t.Fatalf("run calls %q", got)
	}

	// A name that only the refreshed databases have is installed.
	pacmanNode(t, pacmanBase)
	storageStubFile(t, "preview-error-before-refresh", "error: target not found: nginx\n")
	storageStubFile(t, "preview", "nginx 1.30.6-3\n")
	storageStubFile(t, "installed-after", pacmanBase+"nginx 1.30.6-3\n")
	storageResultIs(t, runOfficialExtraModule(t, pacmanModule, map[string]any{"packages": []any{"nginx"}}, false), "succeeded", "changed", "installed nginx 1.30.6-3")

	// The databases the node has know sh: the refusal comes before a
	// refresh, which would leave the databases newer than the packages.
	calls = pacmanNode(t, pacmanBase)
	storageStubFile(t, "preview", "bash 5.3.20-1\n")
	reason := "pacman_package.packages: sh is a group or a name that another package provides, not a package of its own; name each package itself"
	for _, dryrun := range []bool{true, false} {
		storageTaskIsRefused(t, runOfficialExtraModule(t, pacmanModule, map[string]any{"packages": []any{"sh"}}, dryrun), "unchanged", reason)
	}
	if got := calls(); strings.Contains(strings.Join(got, "\n"), pacmanRefresh) {
		t.Fatalf("a refused name refreshed the databases: %q", got)
	}
	// With bash installed, pacman would install nothing for sh, as on an
	// Arch Linux node.
	storageStubFile(t, "preview", "")
	storageTaskIsRefused(t, runOfficialExtraModule(t, pacmanModule, map[string]any{"packages": []any{"sh"}}, false), "unchanged", reason)

	// Uppercase letters are allowed in a name, as PKGBUILD(5) allows them.
	pacmanNode(t, pacmanBase+"Pillow-SIMD 9.0.0-1\n")
	storageResultIs(t, runOfficialExtraModule(t, pacmanModule, map[string]any{"packages": []any{"Pillow-SIMD"}}, false), "succeeded", "unchanged", "")
}

// A removal of a group with installed members, or of a name that an
// installed package provides, would remove nothing and report unchanged;
// it is refused, in a dryrun and in a run.
func TestOfficialExtraPacmanRefusesToRemoveGroupsAndProvidedNames(t *testing.T) {
	t.Parallel()
	calls := pacmanNode(t, pacmanBase+"pacquery 0.4.0-1\n")
	storageStubFile(t, "group-archlinux-tools", "")
	storageStubFile(t, "provided-sh", "")
	for _, name := range []string{"archlinux-tools", "sh"} {
		for _, dryrun := range []bool{true, false} {
			storageTaskIsRefused(t, runOfficialExtraModule(t, pacmanModule, map[string]any{"packages": []any{name}, "state": "absent"}, dryrun), "unchanged",
				"pacman_package.packages: "+name+" is a group or a name that another package provides, not a package of its own; name each package itself")
		}
	}
	for _, call := range calls() {
		if strings.HasPrefix(call, "pacman -R") {
			t.Fatalf("a refused removal ran %q", call)
		}
	}
}

// A removal that would take other packages with it fails before any change,
// unless the task allows it; then pacman removes them with -c.
func TestOfficialExtraPacmanRemoves(t *testing.T) {
	t.Parallel()
	calls := pacmanNode(t, pacmanBase+"libyaml 0.2.5-3\npython-yaml 6.0.3-2\n")
	storageStubFile(t, "preview", "python-yaml 6.0.3-2\nlibyaml 0.2.5-3\n")
	storageStubFile(t, "installed-after", pacmanBase)
	inputs := map[string]any{"packages": []any{"libyaml"}, "state": "absent"}
	for _, dryrun := range []bool{true, false} {
		storageTaskIsRefused(t, runOfficialExtraModule(t, pacmanModule, inputs, dryrun), "unchanged",
			"pacman_package: removing libyaml would also remove python-yaml, which depend on them; to remove these packages too, add also_remove_packages_that_depend_on_these: true")
	}
	if got := calls(); !reflect.DeepEqual(got, []string{pacmanRead, pacmanPrint("-Rcp libyaml"), pacmanRead, pacmanPrint("-Rcp libyaml")}) {
		t.Fatalf("calls %q", got)
	}

	inputs["also_remove_packages_that_depend_on_these"] = true
	storageResultIs(t, runOfficialExtraModule(t, pacmanModule, inputs, true), "succeeded", "would_change", "would remove libyaml 0.2.5-3, python-yaml 6.0.3-2")
	r := runOfficialExtraModule(t, pacmanModule, inputs, false)
	storageResultIs(t, r, "succeeded", "changed", "removed libyaml 0.2.5-3, python-yaml 6.0.3-2")
	if got := calls()[6:]; !reflect.DeepEqual(got, []string{pacmanRead, pacmanPrint("-Rcp libyaml"), "pacman -R -c libyaml --noconfirm", pacmanRead}) {
		t.Fatalf("run calls %q", got)
	}
	if !reflect.DeepEqual(r.ModuleOutputs["changed_packages"], []any{
		map[string]any{"name": "libyaml", "from": "0.2.5-3"}, map[string]any{"name": "python-yaml", "from": "6.0.3-2"}}) {
		t.Fatalf("%+v", r)
	}

	// A package that nothing needs goes with plain -R.
	calls = pacmanNode(t, pacmanBase+"nginx 1.30.6-3\n")
	storageStubFile(t, "preview", "nginx 1.30.6-3\n")
	storageStubFile(t, "installed-after", pacmanBase)
	storageResultIs(t, runOfficialExtraModule(t, pacmanModule, map[string]any{"packages": []any{"nginx", "apache"}, "state": "absent"}, false), "succeeded", "changed", "removed nginx 1.30.6-3")
	if got := calls(); !reflect.DeepEqual(got, []string{pacmanRead, "pacman -Qg apache", "pacman -T apache", pacmanPrint("-Rcp nginx"), "pacman -R nginx --noconfirm", pacmanRead}) {
		t.Fatalf("calls %q", got)
	}
}

// A whole-system upgrade refreshes the databases in a run and is unchanged
// when nothing is newer; the refresh alone is no change.
func TestOfficialExtraPacmanUpgradesAllPackages(t *testing.T) {
	t.Parallel()
	calls := pacmanNode(t, pacmanBase)
	storageStubFile(t, "preview", "")
	inputs := map[string]any{"upgrade_all_packages": true}
	storageResultIs(t, runOfficialExtraModule(t, pacmanModule, inputs, false), "succeeded", "unchanged", "")
	if got := calls(); !reflect.DeepEqual(got, []string{pacmanRead, pacmanRefresh, pacmanPrint("-Sup")}) {
		t.Fatalf("calls %q", got)
	}

	storageStubFile(t, "preview", "glibc 2.44+r52+g0a1b2c3d4e5f-1\n")
	storageStubFile(t, "installed-after", "bash 5.3.20-1\nglibc 2.44+r52+g0a1b2c3d4e5f-1\n")
	storageResultIs(t, runOfficialExtraModule(t, pacmanModule, inputs, true), "succeeded", "would_change",
		"would upgrade glibc from 2.44+r50+g1848099f063e-1 to 2.44+r52+g0a1b2c3d4e5f-1")
	storageResultIs(t, runOfficialExtraModule(t, pacmanModule, inputs, false), "succeeded", "changed",
		"upgraded glibc from 2.44+r50+g1848099f063e-1 to 2.44+r52+g0a1b2c3d4e5f-1")
	if got := calls()[3:]; !reflect.DeepEqual(got, []string{pacmanRead, pacmanPrint("-Sup"), pacmanRead, pacmanRefresh, pacmanPrint("-Sup"), "pacman -Su --noconfirm", pacmanRead}) {
		t.Fatalf("calls %q", got)
	}
}

// pacman's own words reach the reason. A failed refresh or change may be
// tried again; a read that fails after the change may not, because the
// next try would find nothing left to do.
func TestOfficialExtraPacmanFailures(t *testing.T) {
	t.Parallel()
	inputs := map[string]any{"packages": []any{"nginx"}}
	pacmanNode(t, pacmanBase)
	storageStubFile(t, "preview", "nginx 1.30.6-3\n")
	storageStubFile(t, "refresh-fails", "")
	storageResultIs(t, runOfficialExtraModule(t, pacmanModule, inputs, false), "failed", "unchanged",
		"pacman_package: refreshing the package databases: failed to synchronize all databases (unexpected error)")

	pacmanNode(t, pacmanBase)
	storageStubFile(t, "preview", "nginx 1.30.6-3\n")
	storageStubFile(t, "change-fails", "")
	r := runOfficialExtraModule(t, pacmanModule, inputs, false)
	storageResultIs(t, r, "failed", "changed", "pacman_package: installing nginx: failed to commit transaction (conflicting files)")
	if r.NoOtherTry {
		t.Fatal("a failed installation must be tried again")
	}

	// --print checks no conflict; the transaction finds it, and the reason
	// names the packages.
	pacmanNode(t, pacmanBase+"nginx 1.30.6-3\n")
	storageStubFile(t, "preview", "nginx-mainline 1.31.8-2\n")
	storageStubFile(t, "conflict", "")
	storageResultIs(t, runOfficialExtraModule(t, pacmanModule, map[string]any{"packages": []any{"nginx-mainline"}}, false), "failed", "changed",
		"pacman_package: installing nginx-mainline: nginx-mainline-1.31.8-2 and nginx-1.30.6-3 are in conflict; unresolvable package conflicts detected; failed to prepare transaction (conflicting dependencies)")

	pacmanNode(t, pacmanBase)
	storageStubFile(t, "preview", "nginx 1.30.6-3\n")
	storageStubFile(t, "installed-after", pacmanBase+"nginx 1.30.6-3\n")
	storageStubFile(t, "read-after-fails", "")
	r = runOfficialExtraModule(t, pacmanModule, inputs, false)
	storageResultIs(t, r, "failed", "changed", "pacman_package: installing nginx ended, and then reading the installed packages failed: pacman printed a line that is not a package name and a version: error: could not open database")
	if !r.NoOtherTry {
		t.Fatal("a read that fails after the change must not be tried again")
	}
}
