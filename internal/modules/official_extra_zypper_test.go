package modules

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

const zypperPackageModule = "zypper_package"

// zypperNode gives zypper_package a stand-in rpm and a stand-in zypper. The
// files of the stand-ins' directory are the node: installed holds what
// rpm -qa prints (name, a tab, and the version), preview what a zypper
// command with --dry-run prints, and installed-after what rpm -qa prints
// once a changing command ran. preview-exit and preview-output make the
// dry run end with another exit code and text; refresh-fails,
// transaction-exit, and transaction-output make the refresh or the
// transaction fail with the exit code and words that zypper 1.14 printed on
// an openSUSE Leap 16.1 test node. A file provided-NAME makes NAME a name
// that an installed package provides (rpm -q --whatprovides).
func zypperNode(t *testing.T, installed string) func() []string {
	t.Helper()
	calls := stubTools(t, map[string]string{
		"rpm": `
case "$1" in
-qa) cat "$STUBS/installed" ;;
-q) if [ "$2" = --whatprovides ]; then name="$3"; file="provided-$3"; else name="$2"; file="named-$2"; fi
    if [ -e "$STUBS/$file" ]; then echo "package-of-$name-1.0-1.noarch"; else echo "package $name is not installed"; exit 1; fi ;;
esac`,
		"zypper": `
case " $* " in
*" --dry-run "*) if [ -e "$STUBS/preview-exit" ]; then cat "$STUBS/preview-output"; exit "$(cat "$STUBS/preview-exit")"; fi
                 cat "$STUBS/preview" ;;
*" refresh"*) if [ -e "$STUBS/refresh-fails" ]; then echo "<message type=\"error\">Repository 'vendor' is not reachable: Could not connect</message>"; exit 4; fi
              touch "$STUBS/refreshed" ;;
*) if [ -e "$STUBS/transaction-output" ]; then cat "$STUBS/transaction-output"; fi
   code=0; if [ -e "$STUBS/transaction-exit" ]; then code="$(cat "$STUBS/transaction-exit")"; fi
   if [ "$code" != 8 ] && [ "$code" != 7 ]; then cp "$STUBS/installed-after" "$STUBS/installed"; fi
   if [ -e "$STUBS/read-after-fails" ]; then echo 'error: cannot open the database' > "$STUBS/installed"; fi
   exit "$code" ;;
esac`})
	storageStubFile(t, "installed", installed)
	return calls
}

const zypperBase = "bash\t5.2.37-160100.2.1\nglibc\t2.42-160100.1.1\n"

// zypperSummary prints a dry run's summary as zypper --xmlout does: the
// sections to-install, to-upgrade, to-downgrade, and to-remove, each with
// "name edition" entries.
func zypperSummary(sections map[string][]string) string {
	var b strings.Builder
	b.WriteString("<?xml version='1.0'?>\n<stream>\n<message type=\"info\">Loading repository data...</message>\n")
	for _, note := range sections["notes"] {
		fmt.Fprintf(&b, "<message type=\"info\">%s</message>\n", note)
	}
	b.WriteString("<install-summary download-size=\"0\">\n")
	for _, section := range []string{"to-install", "to-upgrade", "to-downgrade", "to-remove"} {
		if len(sections[section]) == 0 {
			continue
		}
		fmt.Fprintf(&b, "<%s>\n", section)
		for _, entry := range sections[section] {
			name, edition, _ := strings.Cut(entry, " ")
			fmt.Fprintf(&b, "<solvable type=\"package\" name=\"%s\" edition=\"%s\" arch=\"noarch\" repository=\"vendor\" summary=\"A summary with name=&quot;x&quot;\">\n<description>Text\nmore text</description></solvable>\n", name, edition)
		}
		fmt.Fprintf(&b, "</%s>\n", section)
	}
	b.WriteString("</install-summary>\n</stream>\n")
	return b.String()
}

// The commands, as the stand-ins note them.
const (
	zypperRead    = "rpm -qa --queryformat %{NAME}\\t%{EVR}\\n"
	zypperRefresh = "zypper --non-interactive --xmlout refresh"
)

func zypperDryrunCall(arguments string) string {
	command, names, _ := strings.Cut(arguments, " ")
	return strings.TrimSuffix("zypper --non-interactive --no-refresh --xmlout "+command+" --dry-run "+names, " ")
}

func zypperTransactionCall(arguments string) string {
	return "zypper --non-interactive --no-refresh --xmlout " + arguments
}

func zypperEntries(names ...string) []any {
	var entries []any
	for _, name := range names {
		entries = append(entries, map[string]any{"name": name, "version": "newest_when_first_installed"})
	}
	return entries
}

func TestOfficialExtraZypperRefusesInputs(t *testing.T) {
	t.Parallel()
	calls := zypperNode(t, zypperBase)
	nginx := map[string]any{"name": "nginx", "version": "newest_when_first_installed"}
	for _, c := range []struct {
		inputs map[string]any
		reason string
	}{
		{map[string]any{}, "zypper_package.packages: is required, or upgrade_all_packages: true"},
		{map[string]any{"packages": []any{}}, "zypper_package.packages: must not be empty"},
		{map[string]any{"packages": []any{"nginx"}}, "zypper_package.packages: each entry is a mapping with a name, such as {name: nginx, version: newest_when_first_installed}"},
		{map[string]any{"packages": []any{map[string]any{"name": "nginx"}}}, "zypper_package.packages: nginx: version is required: newest_when_first_installed, newest_on_every_run, or a version"},
		{map[string]any{"packages": []any{nginx, nginx}}, "zypper_package.packages: nginx is listed twice"},
		{map[string]any{"packages": []any{map[string]any{"name": "-nginx", "version": "newest_on_every_run"}}}, "zypper_package.packages: -nginx must not start with a hyphen or a dot"},
		{map[string]any{"packages": []any{map[string]any{"name": "php*", "version": "newest_on_every_run"}}}, "zypper_package.packages: php* may hold only letters, digits, and . _ + -; a version goes in version, and a pattern, a capability, and a repository in front of the name are not accepted"},
		{map[string]any{"packages": []any{map[string]any{"name": "/usr/bin/php", "version": "newest_on_every_run"}}}, "zypper_package.packages: /usr/bin/php may hold only letters, digits, and . _ + -; a version goes in version, and a pattern, a capability, and a repository in front of the name are not accepted"},
		{map[string]any{"packages": []any{map[string]any{"name": "nginx", "version": ">=1.0"}}}, "zypper_package.packages: nginx: version may hold only letters, digits, and . _ + ~ ^ : -; zypper's operators such as >= are not accepted"},
		{map[string]any{"packages": []any{map[string]any{"name": "nginx", "version": "newest_on_every_run", "pin": true}}}, "zypper_package.packages: nginx has the unknown key pin; an entry holds name and version"},
		{map[string]any{"packages": []any{nginx}, "state": "absent"}, "zypper_package.packages: nginx: state: absent accepts only name; remove version"},
		{map[string]any{"packages": []any{nginx}, "also_remove_packages_that_depend_on_these": true}, "zypper_package.also_remove_packages_that_depend_on_these: requires state: absent"},
		{map[string]any{"upgrade_all_packages": true, "packages": []any{nginx}}, "zypper_package.upgrade_all_packages: takes no other input; remove packages"},
	} {
		for _, dryrun := range []bool{true, false} {
			storageTaskIsRefused(t, runOfficialExtraModule(t, zypperPackageModule, c.inputs, dryrun), "unchanged", c.reason)
		}
	}
	if got := calls(); got != nil {
		t.Fatalf("a refused task ran %q", got)
	}
}

// On a node without zypper, a dryrun does not check the task, and a run
// fails once; the reason names the node fact that tells the nodes apart.
func TestOfficialExtraZypperWithoutZypper(t *testing.T) {
	t.Parallel()
	stubTools(t, map[string]string{})
	reason := "zypper_package: the program zypper is not installed; zypper_package works only on a node whose package manager is zypper, such as openSUSE; give the node task a condition on the node fact package_manager"
	inputs := map[string]any{"packages": zypperEntries("nginx")}
	storageResultIs(t, runOfficialExtraModule(t, zypperPackageModule, inputs, true), "not_checked", "unknown", reason)
	r := runOfficialExtraModule(t, zypperPackageModule, inputs, false)
	storageResultIs(t, r, "failed", "unchanged", reason)
	if r.FailureKind != "missing_prerequisite" {
		t.Fatalf("failure kind %q", r.FailureKind)
	}
}

// Installed packages, and absent ones in a removal, need nothing: only reads
// run, the repositories are not refreshed, and the fingerprint holds the
// versions, or absent.
func TestOfficialExtraZypperUnchanged(t *testing.T) {
	t.Parallel()
	calls := zypperNode(t, zypperBase+"nginx\t1.28.1-1.1\n")
	for _, inputs := range []map[string]any{
		{"packages": zypperEntries("nginx", "bash")},
		{"packages": []any{map[string]any{"name": "nginx", "version": "1.28.1"}, map[string]any{"name": "bash", "version": "5.2.37-160100.2.1"}}},
		{"packages": []any{map[string]any{"name": "apache2"}}, "state": "absent"},
	} {
		for _, dryrun := range []bool{true, false} {
			before := len(calls())
			r := runOfficialExtraModule(t, zypperPackageModule, inputs, dryrun)
			storageResultIs(t, r, "succeeded", "unchanged", "")
			want := []string{zypperRead}
			if inputs["state"] == "absent" {
				want = append(want, "rpm -q apache2", "rpm -q --whatprovides apache2")
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

// A missing package is installed: a dryrun previews against the metadata the
// node has, and a run refreshes, previews, installs, and reads the packages
// again. The result covers every package that the transaction changed.
func TestOfficialExtraZypperInstalls(t *testing.T) {
	t.Parallel()
	calls := zypperNode(t, zypperBase)
	storageStubFile(t, "preview", zypperSummary(map[string][]string{"to-install": {"nginx 1.28.1-1.1", "nginx-config 1.0-1.1"}}))
	storageStubFile(t, "installed-after", zypperBase+"nginx\t1.28.1-1.1\nnginx-config\t1.0-1.1\n")
	inputs := map[string]any{"packages": zypperEntries("nginx", "bash")}
	changes := []any{
		map[string]any{"name": "nginx", "to": "1.28.1-1.1"},
		map[string]any{"name": "nginx-config", "to": "1.0-1.1"},
	}
	preview := zypperDryrunCall("install nginx")

	dryrun := runOfficialExtraModule(t, zypperPackageModule, inputs, true)
	storageResultIs(t, dryrun, "succeeded", "would_change", "would install nginx 1.28.1-1.1, nginx-config 1.0-1.1")
	if got := calls(); !reflect.DeepEqual(got, []string{zypperRead, "rpm -q nginx", "rpm -q --whatprovides nginx", preview}) {
		t.Fatalf("dryrun calls %q", got)
	}
	if !reflect.DeepEqual(dryrun.ModuleOutputs["changed_packages"], changes) || !strings.Contains(dryrun.Diff, "would run: env ZYPP_LOCK_TIMEOUT=60 ") || !strings.Contains(dryrun.Diff, "zypper --non-interactive --no-refresh --xmlout install nginx") {
		t.Fatalf("dryrun %+v", dryrun)
	}

	run := runOfficialExtraModule(t, zypperPackageModule, inputs, false)
	storageResultIs(t, run, "succeeded", "changed", "installed nginx 1.28.1-1.1, nginx-config 1.0-1.1")
	if got := calls()[4:]; !reflect.DeepEqual(got, []string{zypperRead, "rpm -q nginx", "rpm -q --whatprovides nginx", zypperRefresh, preview, zypperTransactionCall("install nginx"), zypperRead}) {
		t.Fatalf("run calls %q", got)
	}
	if !reflect.DeepEqual(run.ModuleOutputs["changed_packages"], changes) {
		t.Fatalf("run %+v", run)
	}
	// The dryrun predicted the versions that the run installed, so a node
	// task block's service restarts in both; the next run is unchanged with
	// the same fingerprint.
	again := runOfficialExtraModule(t, zypperPackageModule, inputs, false)
	storageResultIs(t, again, "succeeded", "unchanged", "")
	if dryrun.Fingerprint != run.Fingerprint || again.Fingerprint != run.Fingerprint {
		t.Fatalf("fingerprints: dryrun %s, run %s, next run %s", dryrun.Fingerprint, run.Fingerprint, again.Fingerprint)
	}
	storageStubFile(t, "installed", zypperBase+"nginx\t1.28.2-1.1\n")
	if other := runOfficialExtraModule(t, zypperPackageModule, inputs, false); other.Change != "unchanged" || other.Fingerprint == run.Fingerprint {
		t.Fatalf("a newer nginx: %+v", other)
	}
}

// A version rule installs the version with name=version, or updates to it;
// newest_on_every_run asks zypper to update an installed package and is
// unchanged when zypper has nothing newer. An exact version that is older
// than the installed one is refused, since zypper does nothing for it.
func TestOfficialExtraZypperVersions(t *testing.T) {
	t.Parallel()
	calls := zypperNode(t, zypperBase+"nginx\t1.28.1-1.1\n")
	storageStubFile(t, "preview", zypperSummary(map[string][]string{"to-upgrade": {"nginx 1.28.2-1.1"}}))
	storageStubFile(t, "installed-after", zypperBase+"nginx\t1.28.2-1.1\n")
	exact := map[string]any{"packages": []any{map[string]any{"name": "nginx", "version": "1.28.2"}}}
	storageResultIs(t, runOfficialExtraModule(t, zypperPackageModule, exact, false), "succeeded", "changed", "upgraded nginx from 1.28.1-1.1 to 1.28.2-1.1")
	if got := calls(); !reflect.DeepEqual(got, []string{zypperRead, zypperRefresh, zypperDryrunCall("install nginx=1.28.2"), zypperTransactionCall("install nginx=1.28.2"), zypperRead}) {
		t.Fatalf("calls %q", got)
	}

	// Nothing newer: unchanged, although the run refreshed.
	zypperNode(t, zypperBase+"nginx\t1.28.1-1.1\n")
	storageStubFile(t, "preview", zypperSummary(nil))
	newest := map[string]any{"packages": []any{map[string]any{"name": "nginx", "version": "newest_on_every_run"}}}
	storageResultIs(t, runOfficialExtraModule(t, zypperPackageModule, newest, false), "succeeded", "unchanged", "")
	if r := runOfficialExtraModule(t, zypperPackageModule, newest, true); r.Change != "unchanged" || r.Reason != "" {
		t.Fatalf("%+v", r)
	}

	// The installed 1.28.1 is newer than the wanted 1.20.0.
	older := map[string]any{"packages": []any{map[string]any{"name": "nginx", "version": "1.20.0"}}}
	for _, dryrun := range []bool{true, false} {
		storageTaskIsRefused(t, runOfficialExtraModule(t, zypperPackageModule, older, dryrun), "unchanged",
			"zypper_package.packages: nginx is installed at 1.28.1-1.1, a version that zypper does not replace by the version asked for; this module never downgrades")
	}
}

// A name that the repositories do not have leaves a dryrun not checked,
// since a run refreshes them first; a run refuses it after the refresh. A
// name that an installed package provides, or that zypper resolves to other
// packages, would be installed again on every run, so it is refused.
func TestOfficialExtraZypperRefusesUnknownAndProvidedNames(t *testing.T) {
	t.Parallel()
	calls := zypperNode(t, zypperBase)
	storageStubFile(t, "preview-exit", "104")
	storageStubFile(t, "preview-output", "<?xml version='1.0'?>\n<stream>\n<message type=\"info\">&apos;nginx-full&apos; not found in package names. Trying capabilities.</message>\n<message type=\"error\">No provider of &apos;nginx-full&apos; found.</message>\n</stream>\n")
	inputs := map[string]any{"packages": zypperEntries("nginx-full")}
	storageResultIs(t, runOfficialExtraModule(t, zypperPackageModule, inputs, true), "not_checked", "unknown",
		"zypper_package: the repositories the node has now offer no nginx-full; a run refreshes them first, and an earlier node task may add the repository that has it")
	storageTaskIsRefused(t, runOfficialExtraModule(t, zypperPackageModule, inputs, false), "unchanged",
		"zypper_package.packages: the repositories offer no nginx-full; zypper search finds packages, and a repository of your own comes from a zypper_repository task before this one")
	if got := calls()[4:]; !reflect.DeepEqual(got, []string{zypperRead, "rpm -q nginx-full", "rpm -q --whatprovides nginx-full", zypperRefresh, zypperDryrunCall("install nginx-full")}) {
		t.Fatalf("run calls %q", got)
	}

	// A name that an installed package provides: refused before a refresh.
	calls = zypperNode(t, zypperBase)
	storageStubFile(t, "provided-awk", "")
	reason := "zypper_package.packages: awk is a name that another package provides, or a package's name with a version or an architecture, not a package of its own; name each package itself"
	for _, dryrun := range []bool{true, false} {
		storageTaskIsRefused(t, runOfficialExtraModule(t, zypperPackageModule, map[string]any{"packages": zypperEntries("awk")}, dryrun), "unchanged", reason)
	}
	if strings.Contains(strings.Join(calls(), "\n"), zypperRefresh) {
		t.Fatalf("a refused name refreshed the repositories: %q", calls())
	}
	// A repository's package provides the name: zypper installs another
	// package for it.
	zypperNode(t, zypperBase)
	storageStubFile(t, "preview", zypperSummary(map[string][]string{"to-install": {"gawk 5.3-1"}}))
	storageTaskIsRefused(t, runOfficialExtraModule(t, zypperPackageModule, map[string]any{"packages": zypperEntries("awk")}, false), "unchanged", reason)
}

// zypper removes the packages that depend on the named ones by itself; the
// task previews the removal and fails before any change unless it allows
// them. A name that an installed package provides is refused.
func TestOfficialExtraZypperRemoves(t *testing.T) {
	t.Parallel()
	calls := zypperNode(t, zypperBase+"perl-base\t5.42.1-1\nmc\t4.8.33-1\n")
	storageStubFile(t, "preview", zypperSummary(map[string][]string{"to-remove": {"mc 4.8.33-1", "perl-base 5.42.1-1"}}))
	storageStubFile(t, "installed-after", zypperBase)
	inputs := map[string]any{"packages": []any{map[string]any{"name": "perl-base"}}, "state": "absent"}
	for _, dryrun := range []bool{true, false} {
		storageTaskIsRefused(t, runOfficialExtraModule(t, zypperPackageModule, inputs, dryrun), "unchanged",
			"zypper_package: removing perl-base would also remove mc, which depend on them; to remove these packages too, add also_remove_packages_that_depend_on_these: true")
	}
	if got := calls(); !reflect.DeepEqual(got, []string{zypperRead, zypperDryrunCall("remove perl-base"), zypperRead, zypperDryrunCall("remove perl-base")}) {
		t.Fatalf("calls %q", got)
	}

	inputs["also_remove_packages_that_depend_on_these"] = true
	storageResultIs(t, runOfficialExtraModule(t, zypperPackageModule, inputs, true), "succeeded", "would_change", "would remove mc 4.8.33-1, perl-base 5.42.1-1")
	r := runOfficialExtraModule(t, zypperPackageModule, inputs, false)
	storageResultIs(t, r, "succeeded", "changed", "removed mc 4.8.33-1, perl-base 5.42.1-1")
	if got := calls()[4:]; !reflect.DeepEqual(got, []string{zypperRead, zypperDryrunCall("remove perl-base"), zypperRead, zypperDryrunCall("remove perl-base"), zypperTransactionCall("remove perl-base"), zypperRead}) {
		t.Fatalf("calls %q", got)
	}

	calls = zypperNode(t, zypperBase)
	storageStubFile(t, "provided-awk", "")
	for _, dryrun := range []bool{true, false} {
		storageTaskIsRefused(t, runOfficialExtraModule(t, zypperPackageModule, map[string]any{"packages": []any{map[string]any{"name": "awk"}}, "state": "absent"}, dryrun), "unchanged",
			"zypper_package.packages: awk is a name that another package provides, or a package's name with a version or an architecture, not a package of its own; name each package itself")
	}
	for _, call := range calls() {
		if strings.Contains(call, "remove") {
			t.Fatalf("a refused removal ran %q", call)
		}
	}
}

// A whole-system update refreshes the repositories in a run and is unchanged
// when nothing is newer; the refresh alone is no change.
func TestOfficialExtraZypperUpdatesAllPackages(t *testing.T) {
	t.Parallel()
	calls := zypperNode(t, zypperBase)
	storageStubFile(t, "preview", zypperSummary(nil))
	inputs := map[string]any{"upgrade_all_packages": true}
	storageResultIs(t, runOfficialExtraModule(t, zypperPackageModule, inputs, false), "succeeded", "unchanged", "")
	if got := calls(); !reflect.DeepEqual(got, []string{zypperRead, zypperRefresh, zypperDryrunCall("update")}) {
		t.Fatalf("calls %q", got)
	}

	storageStubFile(t, "preview", zypperSummary(map[string][]string{"to-upgrade": {"glibc 2.42-160100.2.1"}}))
	storageStubFile(t, "installed-after", "bash\t5.2.37-160100.2.1\nglibc\t2.42-160100.2.1\n")
	storageResultIs(t, runOfficialExtraModule(t, zypperPackageModule, inputs, true), "succeeded", "would_change", "would upgrade glibc from 2.42-160100.1.1 to 2.42-160100.2.1")
	storageResultIs(t, runOfficialExtraModule(t, zypperPackageModule, inputs, false), "succeeded", "changed", "upgraded glibc from 2.42-160100.1.1 to 2.42-160100.2.1")
	if got := calls()[3:]; !reflect.DeepEqual(got, []string{zypperRead, zypperDryrunCall("update"), zypperRead, zypperRefresh, zypperDryrunCall("update"), zypperTransactionCall("update"), zypperRead}) {
		t.Fatalf("calls %q", got)
	}
}

// zypper's own words reach the reason. A failed refresh, a refused
// transaction, and a lock may be tried again; a script that failed after the
// packages changed, and a read that fails after the change, may not, because
// the next try would find nothing left to do.
func TestOfficialExtraZypperFailures(t *testing.T) {
	t.Parallel()
	inputs := map[string]any{"packages": zypperEntries("nginx")}
	preview := zypperSummary(map[string][]string{"to-install": {"nginx 1.28.1-1.1"}})
	fresh := func(files map[string]string) {
		zypperNode(t, zypperBase)
		storageStubFile(t, "preview", preview)
		storageStubFile(t, "installed-after", zypperBase+"nginx\t1.28.1-1.1\n")
		for name, content := range files {
			storageStubFile(t, name, content)
		}
	}

	fresh(map[string]string{"refresh-fails": ""})
	storageTaskFailed(t, runOfficialExtraModule(t, zypperPackageModule, inputs, false), "unchanged",
		"zypper_package: refreshing the repositories: Repository 'vendor' is not reachable: Could not connect")

	// The dry run finds a conflict between declarations.
	fresh(map[string]string{"preview-exit": "4", "preview-output": "<?xml version='1.0'?>\n<stream>\n<prompt id=\"1\">\n<description>Problem: 1: Detailed information: \n- the to be installed nginx conflicts with 'nginx-old' provided by the installed nginx-old\n Solution 1: deinstallation of nginx-old\n</description>\n</prompt>\n</stream>\n"})
	storageTaskFailed(t, runOfficialExtraModule(t, zypperPackageModule, inputs, true), "unchanged",
		"zypper_package: previewing the installation of nginx: Problem: 1: Detailed information: / - the to be installed nginx conflicts with 'nginx-old' provided by the installed nginx-old / Solution 1: deinstallation of nginx-old")

	// The dry run checks no files and no signatures; the transaction finds a
	// file conflict and installs nothing.
	fresh(map[string]string{"transaction-exit": "8", "transaction-output": "<?xml version='1.0'?>\n<stream>\n<install-summary>\n<to-install>\n<solvable type=\"package\" name=\"nginx\" edition=\"1.28.1-1.1\" summary=\"x\">\n<description>A description that is no problem</description></solvable>\n</to-install>\n</install-summary>\n<fileconflict-summary><fileconflicts size=\"1\"><fileconflict>\n<lhs><file>/usr/share/clash/f</file><checksum type=\"md5\">aa</checksum><solvable><kind>package</kind><name>nginx</name></solvable></lhs>\n<rhs><file>/usr/share/clash/f</file><checksum type=\"md5\">bb</checksum><solvable><kind>package</kind><name>nginx-config</name></solvable></rhs></fileconflict>\n</fileconflicts></fileconflict-summary>\n<message type=\"error\">Problem occurred during or after installation or removal of packages:\nInstallation has been aborted as directed.\n</message>\n</stream>\n"})
	storageResultIs(t, runOfficialExtraModule(t, zypperPackageModule, inputs, false), "failed", "changed",
		"zypper_package: installing nginx: the file /usr/share/clash/f is in both nginx and nginx-config, with different content")

	// Another zypper holds the lock; nothing changed.
	fresh(map[string]string{"transaction-exit": "7", "transaction-output": "<?xml version='1.0'?>\n<stream>\n<message type=\"error\">System management is locked by the application with pid 716 (zypper).\nClose this application before trying again.</message>\n</stream>\n"})
	storageTaskFailed(t, runOfficialExtraModule(t, zypperPackageModule, inputs, false), "changed",
		"zypper_package: installing nginx: another zypper still holds its lock /run/zypp.pid after 60 seconds: System management is locked by the application with pid 716 (zypper). / Close this application before trying again.")

	// The packages changed, and a script of a package failed.
	fresh(map[string]string{"transaction-exit": "107", "transaction-output": "<message type=\"warning\">warning: %post(nginx-1.28.1-1.1.noarch) scriptlet failed, exit status 1</message>\n"})
	r := runOfficialExtraModule(t, zypperPackageModule, inputs, false)
	storageResultIs(t, r, "failed", "changed", "zypper_package: installing nginx: the packages changed, and a script of a package failed (exit code 107): warning: %post(nginx-1.28.1-1.1.noarch) scriptlet failed, exit status 1; installed nginx 1.28.1-1.1")
	if !r.NoOtherTry {
		t.Fatal("a failed script after the change must not be tried again")
	}

	// The transaction worked, and the read after it failed.
	fresh(map[string]string{"read-after-fails": ""})
	r = runOfficialExtraModule(t, zypperPackageModule, inputs, false)
	storageResultIs(t, r, "failed", "changed", "zypper_package: installing nginx ended, and then reading the installed packages failed: rpm printed a line that is not a package name and a version: error: cannot open the database")
	if !r.NoOtherTry {
		t.Fatal("a read that fails after the change must not be tried again")
	}
}

// rpm and zypper read tree-2.2.1 and tree.x86_64 as the package tree, so a
// removal of such a name would report unchanged while the package stays; it
// is refused, in a dryrun and a run, and so is an installation of it.
func TestOfficialExtraZypperRefusesNamesWithAVersionOrArchitecture(t *testing.T) {
	t.Parallel()
	calls := zypperNode(t, zypperBase+"tree\t2.2.1-160100.2.1\n")
	reason := func(name string) string {
		return "zypper_package.packages: " + name + " is a name that another package provides, or a package's name with a version or an architecture, not a package of its own; name each package itself"
	}
	for _, name := range []string{"tree-2.2.1", "tree.x86_64"} {
		storageStubFile(t, "named-"+name, "")
		for _, dryrun := range []bool{true, false} {
			storageTaskIsRefused(t, runOfficialExtraModule(t, zypperPackageModule, map[string]any{"packages": []any{map[string]any{"name": name}}, "state": "absent"}, dryrun), "unchanged", reason(name))
			storageTaskIsRefused(t, runOfficialExtraModule(t, zypperPackageModule, map[string]any{"packages": zypperEntries(name)}, dryrun), "unchanged", reason(name))
		}
	}
	for _, call := range calls() {
		if strings.Contains(call, "remove") || strings.Contains(call, "refresh") {
			t.Fatalf("a refused name ran %q", call)
		}
	}
}

// zypper never takes a newer version from another vendor; it says so, and
// the task says so in its reason, although it is unchanged.
func TestOfficialExtraZypperSaysWhenANewerVersionIsFromAnotherVendor(t *testing.T) {
	t.Parallel()
	zypperNode(t, zypperBase+"mmxv\t1.0-1\n")
	note := "There is an update candidate for &apos;mmxv&apos; from vendor &apos;Vendor B&apos;, while the current vendor is &apos;Vendor A&apos;. Use &apos;zypper install mmxv-2.0-1.noarch&apos; to install this candidate."
	storageStubFile(t, "preview", zypperSummary(map[string][]string{"notes": {note}}))
	newest := map[string]any{"packages": []any{map[string]any{"name": "mmxv", "version": "newest_on_every_run"}}}
	want := "not updated: There is an update candidate for 'mmxv' from vendor 'Vendor B', while the current vendor is 'Vendor A'. Use 'zypper install mmxv-2.0-1.noarch' to install this candidate."
	storageResultIs(t, runOfficialExtraModule(t, zypperPackageModule, newest, false), "succeeded", "unchanged", want)
	storageResultIs(t, runOfficialExtraModule(t, zypperPackageModule, map[string]any{"upgrade_all_packages": true}, false), "succeeded", "unchanged", want)
}
