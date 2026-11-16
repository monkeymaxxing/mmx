package modules

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"mmx/internal/assets"
	"mmx/internal/value"
)

// Cleanup failure must report the installed package and the policy left behind.
func TestAPTServicePolicyThatStaysFailsTheTask(t *testing.T) {
	t.Parallel()
	dir := aptFixture(t)
	program := filepath.Join(dir, "root/usr/sbin/policy-rc.d")
	run := Runtime{BeforeMutation: func(path string) error {
		if path == "remove "+program {
			return errors.New("service policy removal refused")
		}
		return nil
	}}
	r := aptIn(dir).Ensure(testContext(t), run, Package{Manager: "apt", Packages: []PackageEntry{{Name: "stuck", Version: newestWhenFirstInstalled}}})
	if installed, err := os.ReadFile(filepath.Join(dir, "stuck")); err != nil || string(installed) != "1\tinstalled\n" {
		t.Fatal("package command did not install the package", string(installed), err)
	}
	if policy, err := os.ReadFile(program); err != nil || string(policy) != servicePolicy {
		t.Fatal("cleanup failure did not leave MMX's policy program in place", string(policy), err)
	}
	if r.Status != "failed" || r.Change != "changed" {
		t.Fatalf("cleanup failure was not reported with the package change: %+v", r)
	}
	if !r.NoOtherTry {
		t.Fatal("an automatic retry could report the installed package unchanged while leaving the policy in place")
	}
	for _, want := range []string{program, "still in place", "Remove", "service policy removal refused"} {
		if !strings.Contains(r.Reason, want) {
			t.Fatal(want, r.Reason)
		}
	}
}

// aptFixture fakes dpkg-query, dpkg, dpkg-deb, apt-cache, and apt-get. A
// package's dpkg status is the file named for it; <name>.available lists the
// versions its package sources offer; plan is what a simulation (-s) prints.
// calls records each command that changes packages, marked [policy] while
// MMX's service policy program was in place. A package file holds its name and
// version, as dpkg-deb shows them.
func aptFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, d := range []string{"root/usr/sbin", "root/etc/apt/preferences.d"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0755); err != nil {
			t.Fatal(err)
		}
	}
	setWorkerEnvironment(t, "MMX_APT_FIXTURE", dir)
	putProgramsFirst(t, dir)
	policy := `policy=
[ -f "$MMX_APT_FIXTURE/root/usr/sbin/policy-rc.d" ] && policy=' [policy]'
`
	scripts := map[string]string{
		"dpkg-query": `for name; do :; done
test -f "$MMX_APT_FIXTURE/$name" || exit 1
cat "$MMX_APT_FIXTURE/$name"
`,
		"dpkg-deb": `for file; do :; done
cat "$file"
`,
		"apt-cache": `for name; do :; done
test -f "$MMX_APT_FIXTURE/$name.available" || exit 0
while read -r version; do echo " $name | $version | https://deb.example stable/main amd64 Packages"; done < "$MMX_APT_FIXTURE/$name.available"
`,
		"apt-get": policy + `case " $* " in
*" update "*) printf 'apt-get %s\n' "$*" >> "$MMX_APT_FIXTURE/calls"; exit 0;;
esac
if [ "$1" = -s ]; then
[ ! -x "$MMX_APT_FIXTURE/preview-check" ] || "$MMX_APT_FIXTURE/preview-check" "$@"
cat "$MMX_APT_FIXTURE/plan" 2>/dev/null || true; exit 0; fi
printf 'apt-get %s%s\n' "$*" "$policy" >> "$MMX_APT_FIXTURE/calls"
case " $* " in
*" install "*) ;;
*) exit 2;;
esac
while [ "$1" != -- ]; do shift; done
shift
for spec; do
	case "$spec" in
	/*) name=$(cut -f1 "$spec") version=$(cut -f2 "$spec");;
	*) name=${spec%%=*} version=1; [ "$name" = "$spec" ] || version=${spec#*=};;
	esac
	printf '%s\tinstalled\n' "$version" > "$MMX_APT_FIXTURE/$name"
done
[ ! -x "$MMX_APT_FIXTURE/after-install" ] || "$MMX_APT_FIXTURE/after-install"
`,
		"dpkg": policy + `if [ "$1" = --compare-versions ]; then
	[ "$2" != "$4" ] && [ "$(printf '%s\n%s\n' "$2" "$4" | sort -V | tail -1)" = "$2" ]
	exit
fi
printf 'dpkg %s%s\n' "$*" "$policy" >> "$MMX_APT_FIXTURE/calls"
while [ "$1" != -- ]; do shift; done
shift
for name; do printf '%s\tinstalled\n' "$(cut -f1 "$MMX_APT_FIXTURE/$name")" > "$MMX_APT_FIXTURE/$name"; done
`,
	}
	for name, script := range scripts {
		writeStandInProgram(t, filepath.Join(dir, name), "#!/bin/sh\nset -eu\n"+script)
	}
	return dir
}

// aptIn is the apt manager of one node job, with its own files under the fixture's root.
func aptIn(dir string) *APT { return &APT{Root: filepath.Join(dir, "root")} }

func put(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}

func fixtureCalls(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "calls"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

func TestAPTNormalizesInspectionForServiceRecovery(t *testing.T) {
	t.Parallel()
	dir := aptFixture(t)
	for _, tc := range []struct{ raw, state, version string }{
		{"config-files", "absent", ""},
		{"not-installed", "absent", ""},
		{"unpacked", "incomplete", "1"},
		{"half-configured", "incomplete", "1"},
		{"installed", "installed", "1"},
	} {
		if err := os.WriteFile(filepath.Join(dir, "demo"), []byte("1\t"+tc.raw), 0600); err != nil {
			t.Fatal(err)
		}
		p, err := (&APT{}).Inspect(testContext(t), "demo")
		if err != nil || p.State != tc.state || p.Version != tc.version {
			t.Fatal(tc, p, err)
		}
	}
	for _, raw := range []string{"\tnot-installed", "\tinstalled", "1\t"} {
		if err := os.WriteFile(filepath.Join(dir, "demo"), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		p, err := (&APT{}).Inspect(testContext(t), "demo")
		if purged := raw == "\tnot-installed"; (err == nil) != purged || (purged && p.State != "absent") {
			t.Fatal(raw, p, err)
		}
	}
	p, err := (&APT{}).Inspect(testContext(t), "missing")
	if err != nil || p.State != "absent" || p.Version != "" {
		t.Fatal(p, err)
	}
}

func TestAPTRefreshBelongsToOneNodeJob(t *testing.T) {
	t.Parallel()
	dir := aptFixture(t)
	manager := aptIn(dir)
	ctx := testContext(t)
	for _, name := range []string{"one", "two"} {
		p := Package{Manager: "apt", Packages: []PackageEntry{{Name: name, Version: newestWhenFirstInstalled}}}
		if r := manager.Ensure(ctx, Runtime{Dryrun: true}, p); r.Change != "would_change" {
			t.Fatal(r)
		}
		if r := manager.Ensure(ctx, Runtime{}, p); r.Status != "succeeded" || r.Change != "changed" {
			t.Fatal(r)
		}
		if r := manager.Ensure(ctx, Runtime{}, p); r.Change != "unchanged" {
			t.Fatal(r)
		}
	}
	readCalls := func() string {
		b, err := os.ReadFile(filepath.Join(dir, "calls"))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	calls := readCalls()
	if strings.Count(calls, " update\n") != 1 || strings.Count(calls, " install ") != 2 {
		t.Fatal(calls)
	}
	// A new job does not inherit the old job's refreshed-index flag.
	if r := aptIn(dir).Ensure(ctx, Runtime{}, Package{Manager: "apt", Packages: []PackageEntry{{Name: "three", Version: newestWhenFirstInstalled}}}); r.Status != "succeeded" {
		t.Fatal(r)
	}
	if calls := readCalls(); strings.Count(calls, " update\n") != 2 {
		t.Fatal(calls)
	}
}

// apt-get update fails at once while another apt holds the package lists'
// lock, as apt's daily timer does after a first boot: the refresh tries
// again until the lock is free, and names the lock when it never is.
func TestAPTRefreshWaitsForTheListsLock(t *testing.T) {
	dir := aptFixture(t)
	// The first two updates find the lock held; the "held" file holds it for good.
	script := `printf 'apt-get %s\n' "$*" >> "$MMX_APT_FIXTURE/calls"
case " $* " in
*" update "*)
	tries=$(grep -c ' update$' "$MMX_APT_FIXTURE/calls")
	if [ -e "$MMX_APT_FIXTURE/held" ] || [ "$tries" -le 2 ]; then
		echo 'E: Could not get lock /var/lib/apt/lists/lock. It is held by process 2016 (apt-get)' >&2
		exit 100
	fi
	exit 0;;
esac
`
	writeStandInProgram(t, filepath.Join(dir, "apt-get"), "#!/bin/sh\nset -eu\n"+script)
	pause, wait := aptLockPause, aptLockWait
	// Enough time for three tries on a busy machine.
	aptLockPause, aptLockWait = 10*time.Millisecond, 5*time.Second
	t.Cleanup(func() { aptLockPause, aptLockWait = pause, wait })
	ctx := testContext(t)
	if r := aptIn(dir).refresh(ctx); r.Status != "succeeded" {
		t.Fatal(r)
	}
	if calls, _ := os.ReadFile(filepath.Join(dir, "calls")); strings.Count(string(calls), " update\n") != 3 {
		t.Fatal(string(calls))
	}
	if err := os.WriteFile(filepath.Join(dir, "held"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	aptLockWait = time.Second
	r := aptIn(dir).refresh(ctx)
	if r.Status != "failed" || r.Change != "unknown" || !strings.Contains(r.Reason, "another apt process still holds the lock on the package lists after 1s") {
		t.Fatal(r)
	}
}

// A source added under /etc/apt/ by an earlier task in the job is refreshed
// once more before the next installation; other paths keep the refresh.
func TestAPTRefreshesAgainAfterSourceChange(t *testing.T) {
	t.Parallel()
	dir := aptFixture(t)
	// install finds only packages the last update indexed from "sources".
	script := `if [ "$1" = -s ]; then exit 0; fi
printf 'apt-get %s\n' "$*" >> "$MMX_APT_FIXTURE/calls"
case " $* " in
*" update "*) cat "$MMX_APT_FIXTURE/sources" > "$MMX_APT_FIXTURE/index"; exit 0;;
esac
while [ "$1" != -- ]; do shift; done
shift
for spec; do
	grep -qx "$spec" "$MMX_APT_FIXTURE/index" || { echo "E: Unable to locate package $spec" >&2; exit 100; }
	printf '1\tinstalled\n' > "$MMX_APT_FIXTURE/$spec"
done
`
	writeStandInProgram(t, filepath.Join(dir, "apt-get"), "#!/bin/sh\nset -eu\n"+script)
	if err := os.WriteFile(filepath.Join(dir, "sources"), []byte("curl\nless\ndocker-ce\n"), 0600); err != nil {
		t.Fatal(err)
	}
	manager, ctx := aptIn(dir), testContext(t)
	install := func(name string) {
		t.Helper()
		if r := manager.Ensure(ctx, Runtime{}, Package{Manager: "apt", Packages: []PackageEntry{{Name: name, Version: newestWhenFirstInstalled}}}); r.Status != "succeeded" {
			t.Fatal(name, r.Reason, r.Stderr)
		}
	}
	install("curl")
	manager.PathChanged("/etc/aptitude.conf")
	install("less")
	manager.PathChanged("/etc/apt/sources.list.d/vendor.list")
	install("docker-ce")
	calls, err := os.ReadFile(filepath.Join(dir, "calls"))
	if err != nil || strings.Count(string(calls), " update\n") != 2 {
		t.Fatal(string(calls), err)
	}
}

// A source that appears outside MMX's tracked files, such as one that a
// vendor's release package or a shell task writes, is refreshed once more
// before the next installation; a source rewritten unchanged keeps the refresh.
func TestAPTRefreshesAgainAfterAnUntrackedSourceChange(t *testing.T) {
	t.Parallel()
	dir := aptFixture(t)
	// install finds only packages the last update indexed from sources.list.d.
	script := `if [ "$1" = -s ]; then exit 0; fi
printf 'apt-get %s\n' "$*" >> "$MMX_APT_FIXTURE/calls"
case " $* " in
*" update "*) cat "$MMX_APT_FIXTURE"/root/etc/apt/sources.list.d/* > "$MMX_APT_FIXTURE/index"; exit 0;;
esac
while [ "$1" != -- ]; do shift; done
shift
for spec; do
	grep -qx "$spec" "$MMX_APT_FIXTURE/index" || { echo "E: Unable to locate package $spec" >&2; exit 100; }
	printf '1\tinstalled\n' > "$MMX_APT_FIXTURE/$spec"
done
`
	writeStandInProgram(t, filepath.Join(dir, "apt-get"), "#!/bin/sh\nset -eu\n"+script)
	sources := filepath.Join(dir, "root/etc/apt/sources.list.d")
	if err := os.MkdirAll(sources, 0755); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(sources, "debian.sources"), "curl\nless\n")
	manager, ctx := aptIn(dir), testContext(t)
	install := func(name string) {
		t.Helper()
		if r := manager.Ensure(ctx, Runtime{}, Package{Manager: "apt", Packages: []PackageEntry{{Name: name, Version: newestWhenFirstInstalled}}}); r.Status != "succeeded" {
			t.Fatal(name, r.Reason, r.Stderr)
		}
	}
	install("curl")
	// The vendor's release package adds its source; no tracked path changed.
	put(t, filepath.Join(sources, "vendor.sources"), "docker-ce\n")
	install("docker-ce")
	put(t, filepath.Join(sources, "debian.sources"), "curl\nless\n")
	install("less")
	calls, err := os.ReadFile(filepath.Join(dir, "calls"))
	if err != nil || strings.Count(string(calls), " update\n") != 2 {
		t.Fatal(string(calls), err)
	}
}

// A partly installed package is configured without prompts: stdin is
// /dev/null, so dpkg keeps a locally changed conffile, which a declared file
// task owns.
func TestAPTConfiguresAPartlyInstalledPackage(t *testing.T) {
	t.Parallel()
	dir := aptFixture(t)
	put(t, filepath.Join(dir, "partial"), "1\thalf-configured")
	writeStandInProgram(t, filepath.Join(dir, "preview-check"), `#!/bin/sh
set -eu
case " $* " in *" partial=1 "*) :;; *) echo 'repair must keep the partly installed version' >&2; exit 100;; esac
`)

	manager, ctx, p := aptIn(dir), testContext(t), Package{Manager: "apt", Packages: []PackageEntry{{Name: "partial", Version: newestWhenFirstInstalled}}}
	if r := manager.Ensure(ctx, Runtime{}, p); r.Status != "succeeded" || r.Change != "changed" {
		t.Fatal(r)
	}
	if r := manager.Ensure(ctx, Runtime{}, p); r.Change != "unchanged" {
		t.Fatal(r)
	}
	if calls := fixtureCalls(t, dir); calls != "dpkg --force-confdef --force-confold --configure -- partial [policy]\n" {
		t.Fatal(calls)
	}
}

// version keeps a package inside a pattern with apt's preferences file: the
// newest matching version is installed, the file stays, and a rerun is
// unchanged. A task for the package without version deletes the file.
func TestAPTKeepsAPackageInsideAVersionPattern(t *testing.T) {
	t.Parallel()
	dir := aptFixture(t)
	put(t, filepath.Join(dir, "docker-ce.available"), "5:27.3.10-1\n5:27.3.9-1\n5:28.0.1-1\n")
	manager, ctx := aptIn(dir), testContext(t)
	p := Package{Manager: "apt", Packages: []PackageEntry{{Name: "docker-ce", Version: "5:27.3.*"}}}
	preview := manager.Ensure(ctx, Runtime{Dryrun: true}, p)
	if preview.Change != "would_change" || preview.FileChange == nil || fixtureCalls(t, dir) != "" {
		t.Fatal(preview, fixtureCalls(t, dir))
	}
	r := manager.Ensure(ctx, Runtime{}, p)
	want := []any{map[string]any{"name": "docker-ce", "to": "5:27.3.10-1"}}
	if r.Status != "succeeded" || r.Change != "changed" || !reflect.DeepEqual(r.Value, want) {
		t.Fatal(r)
	}
	rule := filepath.Join(dir, "root/etc/apt/preferences.d/mmx-docker-ce.pref")
	text := "Explanation: Written by MMX for an apt_package task with a version.\nPackage: docker-ce\nPin: version 5:27.3.*\nPin-Priority: 999\n\n" +
		"Explanation: Versions outside the pattern are never installed.\nPackage: docker-ce\nPin: version *\nPin-Priority: -1\n"
	if b, err := os.ReadFile(rule); err != nil || string(b) != text {
		t.Fatal(string(b), err)
	}
	if r := manager.Ensure(ctx, Runtime{}, p); r.Status != "succeeded" || r.Change != "unchanged" {
		t.Fatal(r)
	}
	wantCalls := "apt-get -o DPkg::Lock::Timeout=60 update\n" +
		"apt-get -o DPkg::Lock::Timeout=60 -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold install -y -- docker-ce=5:27.3.10-1 [policy]\n"
	if calls := fixtureCalls(t, dir); calls != wantCalls {
		t.Fatalf("calls:\n%s\nwant:\n%s", calls, wantCalls)
	}
	if _, err := os.Stat(filepath.Join(dir, "root/usr/sbin/policy-rc.d")); !os.IsNotExist(err) {
		t.Fatal("the service policy program stayed after the apt command", err)
	}
	// The same package without version: another node job deletes the rule.
	if r := aptIn(dir).Ensure(ctx, Runtime{}, Package{Manager: "apt", Packages: []PackageEntry{{Name: "docker-ce", Version: newestWhenFirstInstalled}}}); r.Change != "changed" {
		t.Fatal(r)
	}
	if _, err := os.Stat(rule); !os.IsNotExist(err) {
		t.Fatal("the version rule stayed", err)
	}
}

// Every refusal is decided before anything is written: an older matching
// version needs allow_downgrade, and a pattern nothing matches lists what is
// available. Dryrun cannot tell before the index is refreshed.
func TestAPTVersionRefusalsWriteNothing(t *testing.T) {
	t.Parallel()
	dir := aptFixture(t)
	put(t, filepath.Join(dir, "tool"), "2.0\tinstalled")
	put(t, filepath.Join(dir, "tool.available"), "1.1\n1.2\n2.0\n")
	rule := filepath.Join(dir, "root/etc/apt/preferences.d/mmx-tool.pref")
	ctx := testContext(t)
	older := Package{Manager: "apt", Packages: []PackageEntry{{Name: "tool", Version: "1.*"}}}
	if r := aptIn(dir).Ensure(ctx, Runtime{}, older); r.Status != "failed" || !strings.Contains(r.Reason, "2.0 is installed, which is newer than 1.2; to install the older version, add allow_downgrade: true") {
		t.Fatal(r)
	}
	none := Package{Manager: "apt", Packages: []PackageEntry{{Name: "tool", Version: "3.*"}}}
	if r := aptIn(dir).Ensure(ctx, Runtime{}, none); r.Status != "failed" || !strings.Contains(r.Reason, "no version of tool in the node's package sources matches 3.*; available: 1.1, 1.2, 2.0") {
		t.Fatal(r)
	}
	if r := aptIn(dir).Ensure(ctx, Runtime{Dryrun: true}, none); r.Status != "not_checked" {
		t.Fatal(r)
	}
	if _, err := os.Stat(rule); !os.IsNotExist(err) {
		t.Fatal("a refused task wrote the version rule", err)
	}
	if strings.Contains(fixtureCalls(t, dir), "install") {
		t.Fatal(fixtureCalls(t, dir))
	}
	older.Packages[0].AllowDowngrade = true
	if r := aptIn(dir).Ensure(ctx, Runtime{}, older); r.Status != "succeeded" || r.Change != "changed" {
		t.Fatal(r)
	}
	if calls := fixtureCalls(t, dir); !strings.Contains(calls, "install -y --allow-downgrades -- tool=1.2 [policy]") {
		t.Fatal(calls)
	}
}

// One node job may not keep a package at a version in one task and name it
// without one in another: the second task fails before it changes anything.
func TestAPTRefusesAPackageKeptAndNotKeptInOneJob(t *testing.T) {
	t.Parallel()
	dir := aptFixture(t)
	put(t, filepath.Join(dir, "tool"), "1.2\tinstalled")
	manager, ctx := aptIn(dir), testContext(t)
	if r := manager.Ensure(ctx, Runtime{}, Package{Manager: "apt", Packages: []PackageEntry{{Name: "tool", Version: "1.*"}}}); r.Status != "succeeded" {
		t.Fatal(r)
	}
	r := manager.Ensure(ctx, Runtime{}, Package{Manager: "apt", Packages: []PackageEntry{{Name: "tool", Version: newestOnEveryRun}}})
	if r.Status != "failed" || !strings.Contains(r.Reason, "tool has a version rule in one node task and a version keyword in another node task of the node job") {
		t.Fatal(r)
	}
}

// upgrade with names lists what apt's simulation installs or upgrades, and
// installs them with one apt command.
func TestAPTUpgradesNamedPackages(t *testing.T) {
	t.Parallel()
	dir := aptFixture(t)
	put(t, filepath.Join(dir, "plan"), "Inst openssl [3.0.1] (3.0.2 Debian:13/stable [amd64])\nInst tzdata (2026a Debian:13/stable [all])\n")
	manager, ctx, p := aptIn(dir), testContext(t), Package{Manager: "apt", Packages: []PackageEntry{{Name: "openssl", Version: newestOnEveryRun}, {Name: "tzdata", Version: newestOnEveryRun}}}
	want := []any{map[string]any{"name": "openssl", "from": "3.0.1", "to": "3.0.2"}, map[string]any{"name": "tzdata", "to": "2026a"}}
	if r := manager.Ensure(ctx, Runtime{Dryrun: true}, p); r.Change != "would_change" || !reflect.DeepEqual(r.Value, want) || fixtureCalls(t, dir) != "" {
		t.Fatal(r, fixtureCalls(t, dir))
	}
	if r := manager.Ensure(ctx, Runtime{}, p); r.Change != "changed" || !reflect.DeepEqual(r.Value, want) {
		t.Fatal(r)
	}
	put(t, filepath.Join(dir, "plan"), "")
	if r := manager.Ensure(ctx, Runtime{}, p); r.Change != "unchanged" {
		t.Fatal(r)
	}
	wantCalls := "apt-get -o DPkg::Lock::Timeout=60 update\n" +
		"apt-get -o DPkg::Lock::Timeout=60 -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold install -y -- openssl=3.0.2 tzdata=2026a [policy]\n"
	if calls := fixtureCalls(t, dir); calls != wantCalls {
		t.Fatalf("calls:\n%s\nwant:\n%s", calls, wantCalls)
	}
}

// A package file is installed by its path, checked against the task's name,
// and kept at its version; a newer installed version needs allow_downgrade.
func TestAPTInstallsAPackageFile(t *testing.T) {
	t.Parallel()
	dir := aptFixture(t)
	file := filepath.Join(dir, "rclone.deb")
	put(t, file, "rclone\t1.68.2")
	ref := &assets.Ref{Path: file}
	manager, ctx := aptIn(dir), testContext(t)
	p := Package{Manager: "apt", Packages: []PackageEntry{{Name: "rclone", Version: "1.68.2", PackageFileSource: "rclone.deb", PackageFile: ref}}}
	if r := manager.Ensure(ctx, Runtime{}, p); r.Status != "succeeded" || r.Change != "changed" {
		t.Fatal(r)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "rclone")); err != nil || string(b) != "1.68.2\tinstalled\n" {
		t.Fatal(string(b), err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "root/etc/apt/preferences.d/mmx-rclone.pref")); err != nil || !strings.Contains(string(b), "Pin: version 1.68.2\n") {
		t.Fatal(string(b), err)
	}
	if r := manager.Ensure(ctx, Runtime{}, p); r.Change != "unchanged" {
		t.Fatal(r)
	}
	put(t, filepath.Join(dir, "rclone"), "1.69.0\tinstalled")
	if r := manager.Ensure(ctx, Runtime{}, p); r.Status != "failed" || !strings.Contains(r.Reason, "add allow_downgrade: true") {
		t.Fatal(r)
	}
	p.Packages[0].Name = "restic"
	if r := aptIn(dir).Ensure(ctx, Runtime{}, p); r.Status != "failed" || !strings.Contains(r.Reason, "the package file holds the package rclone, not restic") {
		t.Fatal(r)
	}
}

// The removal settings become apt's --purge and --autoremove; purging also
// removes a package whose configuration files remain.
func TestAPTRemovalSettings(t *testing.T) {
	t.Parallel()
	dir := aptFixture(t)
	put(t, filepath.Join(dir, "agent"), "1.0\tconfig-files")
	put(t, filepath.Join(dir, "plan"), "Purg agent [1.0]\nPurg agent-lib [1.0]\n")
	script := `printf 'apt-get %s\n' "$*" >> "$MMX_APT_FIXTURE/calls"
[ "$1" = -s ] && { cat "$MMX_APT_FIXTURE/plan"; exit 0; }
printf '\tnot-installed\n' > "$MMX_APT_FIXTURE/agent"
`
	writeStandInProgram(t, filepath.Join(dir, "apt-get"), "#!/bin/sh\nset -eu\n"+script)
	manager, ctx := aptIn(dir), testContext(t)
	plain := Package{Manager: "apt", Packages: []PackageEntry{{Name: "agent"}}, State: "absent"}
	if r := manager.Ensure(ctx, Runtime{}, plain); r.Change != "unchanged" {
		t.Fatal("configuration files alone are absent without purge", r)
	}
	purge := plain
	purge.AlsoDeleteItsConfigFiles, purge.AlsoRemovePackagesNoLongerNeeded = true, true
	purge.AlsoRemovePackagesThatDependOnThese = true
	want := []any{map[string]any{"name": "agent", "from": "1.0"}, map[string]any{"name": "agent-lib", "from": "1.0"}}
	if r := manager.Ensure(ctx, Runtime{}, purge); r.Status != "succeeded" || r.Change != "changed" || !reflect.DeepEqual(r.Value, want) {
		t.Fatal(r)
	}
	wantCalls := "apt-get -s remove -y --purge --autoremove -- agent-\n" +
		"apt-get -o DPkg::Lock::Timeout=60 remove -y --purge --autoremove -- agent-\n"
	if calls := fixtureCalls(t, dir); calls != wantCalls {
		t.Fatalf("calls:\n%s\nwant:\n%s", calls, wantCalls)
	}
}

// MMX's policy program lets a package restart a running service but not start
// a stopped one; allow_package_to_start_its_services runs without it, and a
// policy program that MMX did not write stays and decides.
func TestAPTServicePolicy(t *testing.T) {
	t.Parallel()
	dir := aptFixture(t)
	manager, ctx := aptIn(dir), testContext(t)
	if r := manager.Ensure(ctx, Runtime{}, Package{Manager: "apt", Packages: []PackageEntry{{Name: "one", Version: newestWhenFirstInstalled}}, AllowPackageToStartItsServices: true}); r.Status != "succeeded" {
		t.Fatal(r)
	}
	if calls := fixtureCalls(t, dir); strings.Contains(calls, "[policy]") {
		t.Fatal(calls)
	}
	program := filepath.Join(dir, "root/usr/sbin/policy-rc.d")
	put(t, program, "#!/bin/sh\nexit 101\n")
	r := manager.Ensure(ctx, Runtime{}, Package{Manager: "apt", Packages: []PackageEntry{{Name: "two", Version: newestWhenFirstInstalled}}})
	if r.Status != "succeeded" || !strings.Contains(r.Reason, "The node's own /usr/sbin/policy-rc.d decided") {
		t.Fatal(r)
	}
	if b, err := os.ReadFile(program); err != nil || string(b) != "#!/bin/sh\nexit 101\n" {
		t.Fatal("MMX replaced the node's own policy program", string(b), err)
	}
	// A leftover of MMX's own is replaced for the command, then removed.
	put(t, program, servicePolicy)
	if r := manager.Ensure(ctx, Runtime{}, Package{Manager: "apt", Packages: []PackageEntry{{Name: "three", Version: newestWhenFirstInstalled}}}); r.Status != "succeeded" || r.Reason != "" {
		t.Fatal(r)
	}
	if _, err := os.Stat(program); !os.IsNotExist(err) {
		t.Fatal("MMX's policy program stayed", err)
	}
}

// The policy program refuses to start a stopped service and allows every
// action on a running one.
func TestAPTServicePolicyProgram(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeStandInProgram(t, filepath.Join(dir, "systemctl"), "#!/bin/sh\n[ \"$3\" = running.service ]\n")
	program := filepath.Join(dir, "policy-rc.d")
	writeStandInProgram(t, program, servicePolicy)
	for _, tc := range []struct {
		args []string
		exit int
	}{
		{[]string{"stopped.service", "start"}, 101},
		{[]string{"--quiet", "stopped.service", "restart"}, 101},
		{[]string{"stopped.service", "stop"}, 0},
		{[]string{"running.service", "restart"}, 0},
		{[]string{"running.service", "start"}, 0},
	} {
		r := Command(testContext(t), map[string]string{"PATH": dir + string(os.PathListSeparator) + os.Getenv("PATH")}, "", program, tc.args...)
		if r.ExitCode != tc.exit {
			t.Error(tc.args, r)
		}
	}
}

// A whole-node upgrade previews apt's simulation against the current indexes,
// then refreshes once per job and applies it, reporting the same packages.
// With also_remove_packages_no_longer_needed, autoremove follows, and the
// run reports what it removes after the upgrade. The stand-in's full-upgrade
// makes an old kernel removable.
func TestAPTUpgradePreviewsThenAppliesFullUpgrade(t *testing.T) {
	t.Parallel()
	dir := aptFixture(t)
	script := `printf 'apt-get %s\n' "$*" >> "$MMX_APT_FIXTURE/calls"
case " $* " in
*" update "*) exit 0;;
*" -s full-upgrade "*) cat "$MMX_APT_FIXTURE/plan"; exit 0;;
*" full-upgrade "*) [ ! -s "$MMX_APT_FIXTURE/plan" ] || echo "Remv linux-image-old [6.12.100-1]" >> "$MMX_APT_FIXTURE/removable"; : > "$MMX_APT_FIXTURE/plan"; exit 0;;
*" -s autoremove "*) cat "$MMX_APT_FIXTURE/removable" 2>/dev/null || true; exit 0;;
*" autoremove "*) : > "$MMX_APT_FIXTURE/removable"; exit 0;;
esac
exit 2
`
	writeStandInProgram(t, filepath.Join(dir, "apt-get"), "#!/bin/sh\nset -eu\n"+script)
	plan := "Reading package lists...\n" +
		"Inst libc6 [2.41-12+deb13u3] (2.41-12+deb13u4 Debian:13.7/stable [arm64])\n" +
		"Conf libc6 (2.41-12+deb13u4 Debian:13.7/stable [arm64])\n" +
		"Inst linux-image-6.12.107+deb13-cloud-arm64 (6.12.107-1 Debian:13.7/stable [arm64])\n" +
		"Remv old-tool [1.0]\n"
	if err := os.WriteFile(filepath.Join(dir, "plan"), []byte(plan), 0600); err != nil {
		t.Fatal(err)
	}
	want := []any{
		map[string]any{"name": "libc6", "from": "2.41-12+deb13u3", "to": "2.41-12+deb13u4"},
		map[string]any{"name": "linux-image-6.12.107+deb13-cloud-arm64", "to": "6.12.107-1"},
		map[string]any{"name": "old-tool", "from": "1.0"},
	}
	calls := func() string {
		b, _ := os.ReadFile(filepath.Join(dir, "calls"))
		return string(b)
	}
	manager, ctx, upgrade := aptIn(dir), testContext(t), Package{Manager: "apt", UpgradeAll: true}
	preview := manager.Ensure(ctx, Runtime{Dryrun: true}, upgrade)
	if preview.Status != "succeeded" || preview.Change != "would_change" || !reflect.DeepEqual(preview.Value, want) {
		t.Fatal(preview)
	}
	if calls() != "apt-get -s full-upgrade\n" {
		t.Fatal("dryrun refreshed indexes or changed packages:", calls())
	}
	applied := manager.Ensure(ctx, Runtime{}, upgrade)
	if applied.Status != "succeeded" || applied.Change != "changed" || !reflect.DeepEqual(applied.Value, want) {
		t.Fatal(applied)
	}
	if r := manager.Ensure(ctx, Runtime{}, upgrade); r.Status != "succeeded" || r.Change != "unchanged" || r.Value != nil {
		t.Fatal(r)
	}
	wantCalls := "apt-get -s full-upgrade\n" +
		"apt-get -o DPkg::Lock::Timeout=60 update\n" +
		"apt-get -s full-upgrade\n" +
		"apt-get -o DPkg::Lock::Timeout=60 -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold full-upgrade -y\n" +
		"apt-get -s full-upgrade\n"
	if calls() != wantCalls {
		t.Fatalf("calls:\n%s\nwant:\n%s", calls(), wantCalls)
	}
	// The same node job, with autoremove: the old kernel that the upgrade
	// leaves is removed, and a package that was no longer needed before is
	// listed once.
	if err := os.WriteFile(filepath.Join(dir, "plan"), []byte(plan), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "removable"), []byte("Remv libold1 [1.2-3]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "calls"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	upgrade.AlsoRemovePackagesNoLongerNeeded = true
	libold1 := map[string]any{"name": "libold1", "from": "1.2-3"}
	if r := manager.Ensure(ctx, Runtime{Dryrun: true}, upgrade); r.Change != "would_change" || !reflect.DeepEqual(r.Value, append(want, libold1)) {
		t.Fatal(r)
	}
	applied = manager.Ensure(ctx, Runtime{}, upgrade)
	if applied.Status != "succeeded" || applied.Change != "changed" || !reflect.DeepEqual(applied.Value, append(want, libold1, map[string]any{"name": "linux-image-old", "from": "6.12.100-1"})) {
		t.Fatal(applied)
	}
	// With nothing to upgrade, autoremove still runs.
	if err := os.WriteFile(filepath.Join(dir, "removable"), []byte("Remv libold2 [2.0]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if r := manager.Ensure(ctx, Runtime{}, upgrade); r.Change != "changed" || !reflect.DeepEqual(r.Value, []any{map[string]any{"name": "libold2", "from": "2.0"}}) {
		t.Fatal(r)
	}
	if r := manager.Ensure(ctx, Runtime{}, upgrade); r.Change != "unchanged" {
		t.Fatal(r)
	}
	change := "apt-get -o DPkg::Lock::Timeout=60 -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold "
	wantCalls = "apt-get -s full-upgrade\napt-get -s autoremove\n" +
		"apt-get -s full-upgrade\napt-get -s autoremove\n" + change + "full-upgrade -y\napt-get -s autoremove\n" + change + "autoremove -y\n" +
		"apt-get -s full-upgrade\napt-get -s autoremove\n" + change + "full-upgrade -y\napt-get -s autoremove\n" + change + "autoremove -y\n" +
		"apt-get -s full-upgrade\napt-get -s autoremove\n"
	if calls() != wantCalls {
		t.Fatalf("calls:\n%s\nwant:\n%s", calls(), wantCalls)
	}
}

// A repository is a root-owned key named for its format and a deb822 source
// file that names that key.
func TestAPTRepositoryFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	armored, binary := filepath.Join(dir, "armored"), filepath.Join(dir, "binary")
	if err := os.WriteFile(armored, []byte("\n-----BEGIN PGP PUBLIC KEY BLOCK-----\n\nmDMEZ...\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, []byte{0x99, 0x01, 0x0d, 0x04}, 0600); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{armored: ".asc", binary: ".gpg"} {
		if ext, err := keyExtension(&assets.Ref{Path: path}); err != nil || ext != want {
			t.Fatal(path, ext, err)
		}
	}
	if _, err := keyExtension(nil); err == nil {
		t.Fatal("accepted a repository without its key asset")
	}
	ref := &assets.Ref{Path: armored}
	r := Repository{Manager: "apt", Name: "docker", URIs: []string{"https://download.docker.com/linux/debian"}, Suites: []string{"trixie"},
		Components: []string{"stable"}, SigningKeyURL: "https://download.docker.com/linux/debian/gpg", SigningKeySHA256: strings.Repeat("a", 64), Key: ref}
	key, source := repositoryFiles(r, ".asc")
	if key.Path != "/etc/apt/keyrings/docker.asc" || key.URL != r.SigningKeyURL || key.SHA256 != r.SigningKeySHA256 || key.Asset != ref ||
		key.Owner != "root" || key.Group != "root" || key.Mode != "0644" || ValidateFile(key) != nil {
		t.Fatal(key)
	}
	want := "Types: deb\nURIs: https://download.docker.com/linux/debian\nSuites: trixie\nComponents: stable\nSigned-By: /etc/apt/keyrings/docker.asc\n"
	if source.Path != "/etc/apt/sources.list.d/docker.sources" || *source.Literal != want || source.Mode != "0644" || source.Owner != "root" || ValidateFile(source) != nil {
		t.Fatal(source.Path, *source.Literal)
	}
	flat := Repository{Manager: "apt", Name: "local", URIs: []string{"file:/srv/a", "file:/srv/b"}, Suites: []string{"./"}, Architectures: []string{"amd64", "arm64"}, SigningKeySource: "local.gpg", Key: ref}
	key, source = repositoryFiles(flat, ".gpg")
	want = "Types: deb\nURIs: file:/srv/a file:/srv/b\nSuites: ./\nArchitectures: amd64 arm64\nSigned-By: /etc/apt/keyrings/local.gpg\n"
	if key.Path != "/etc/apt/keyrings/local.gpg" || key.Source != "local.gpg" || *source.Literal != want {
		t.Fatal(key, *source.Literal)
	}
}

// A private repository's login is a root-only file with one https entry for
// each address; the login's values come from the scope, where a secret is
// recorded as read.
func TestAPTRepositoryLogin(t *testing.T) {
	t.Parallel()
	r := Repository{Manager: "apt", Name: "nginx-plus", URIs: []string{"https://pkgs.nginx.com/plus/debian/", "https://mirror.example:8443/plus"},
		Username: "{{ .vars.user }}", Password: "{{ .secrets.repository_password }}"}
	s := value.Scope{"vars": map[string]any{"user": "customer"}, "secrets": map[string]any{"repository_password": value.Sensitive("p4ss")}}
	login, err := repositoryLogin(r, s)
	if err != nil {
		t.Fatal(err)
	}
	want := "machine pkgs.nginx.com/plus/debian login customer password p4ss\nmachine mirror.example:8443/plus login customer password p4ss\n"
	if login.Path != "/etc/apt/auth.conf.d/nginx-plus.conf" || *login.Literal != want || login.Mode != "0600" || login.Owner != "root" || ValidateFile(login) != nil {
		t.Fatal(login.Path, *login.Literal)
	}
	s["vars"] = map[string]any{"user": "two words"}
	if _, err := repositoryLogin(r, s); err == nil || !strings.Contains(err.Error(), "apt_repository.username: must not contain spaces") {
		t.Fatal(err)
	}
}

// upgrade with names deletes MMX's version rule before apt decides, so the
// rule cannot hold the package back; dryrun works out the newest version
// itself while the rule is still there.
func TestAPTUpgradeIgnoresItsOwnVersionRule(t *testing.T) {
	t.Parallel()
	dir := aptFixture(t)
	put(t, filepath.Join(dir, "tool"), "1.0\tinstalled")
	put(t, filepath.Join(dir, "tool.available"), "1.0\n2.0\n")
	rule := filepath.Join(dir, "root/etc/apt/preferences.d/mmx-tool.pref")
	put(t, rule, "Package: tool\nPin: version 1.0\nPin-Priority: 999\n")
	put(t, filepath.Join(dir, "plan"), "Inst tool [1.0] (2.0 Test:1/stable [all])\n")
	p := Package{Manager: "apt", Packages: []PackageEntry{{Name: "tool", Version: newestOnEveryRun}}}
	want := []any{map[string]any{"name": "tool", "from": "1.0", "to": "2.0"}}
	if r := aptIn(dir).Ensure(testContext(t), Runtime{Dryrun: true}, p); r.Change != "would_change" || !reflect.DeepEqual(r.Value, want) {
		t.Fatal(r)
	}
	if _, err := os.Stat(rule); err != nil {
		t.Fatal("dryrun deleted the rule", err)
	}
	put(t, filepath.Join(dir, "plan"), "Inst tool [1.0] (2.0 Test:1/stable [all])\n")
	if r := aptIn(dir).Ensure(testContext(t), Runtime{}, p); r.Change != "changed" || !reflect.DeepEqual(r.Value, want) {
		t.Fatal(r)
	}
	if _, err := os.Stat(rule); !os.IsNotExist(err) {
		t.Fatal("the rule stayed", err)
	}
}

// Dropping MMX's old pin must leave the administrator's pin in candidate
// selection. Madison offers 3, but the administrator permits only 2.
func TestAPTUpgradeKeepsAdministratorPreferences(t *testing.T) {
	t.Parallel()
	dir := aptFixture(t)
	put(t, filepath.Join(dir, "tool"), "1\tinstalled")
	put(t, filepath.Join(dir, "tool.available"), "1\n2\n3\n")
	mmx := aptIn(dir).versionRulePath("tool")
	put(t, mmx, "Package: tool\nPin: version 1\nPin-Priority: 999\n")
	administrator := "Package: tool\nPin: version 2\nPin-Priority: 1001\n"
	put(t, filepath.Join(dir, "root/etc/apt/preferences.d/administrator.pref"), administrator)
	put(t, filepath.Join(dir, "root/etc/apt/preferences"), "# administrator preferences file\n")
	writeStandInProgram(t, filepath.Join(dir, "preview-check"), `#!/bin/sh
set -eu
parts= preferences=
for arg; do case "$arg" in
Dir::Etc::preferencesparts=*) parts=${arg#*=};;
Dir::Etc::preferences=*) preferences=${arg#*=};;
esac; done
[ ! -e "$parts/mmx-tool.pref" ]
cmp "$parts/administrator.pref" "$MMX_APT_FIXTURE/root/etc/apt/preferences.d/administrator.pref"
grep -q 'administrator preferences file' "$preferences"
case " $* " in *" tool "*) :;; *) exit 100;; esac
printf 'Inst tool [1] (2 Test:1/stable [all])\n'
`)
	p := Package{Manager: "apt", Packages: []PackageEntry{{Name: "tool", Version: newestOnEveryRun}}}
	for _, dryrun := range []bool{true, false} {
		result := aptIn(dir).Ensure(testContext(t), Runtime{Dryrun: dryrun}, p)
		if result.Status != "succeeded" || len(result.Value.([]any)) != 1 || result.Value.([]any)[0].(map[string]any)["to"] != "2" {
			t.Fatal(result)
		}
		if dryrun && readState(t, mmx) == "" {
			t.Fatal("dryrun removed MMX's pin")
		}
	}
	state, err := aptIn(dir).Inspect(testContext(t), "tool")
	if err != nil || state.Version != "2" {
		t.Fatal(state, err)
	}
	if readState(t, filepath.Join(dir, "root/etc/apt/preferences.d/administrator.pref")) != administrator {
		t.Fatal("administrator pin changed")
	}
}

// A dryrun cannot use a source file that an earlier task only previews.
// Native apt reports an unknown package until a real run refreshes indexes.
func TestAPTUnknownPackageInCurrentIndexesIsNotCheckedInDryrun(t *testing.T) {
	for _, policy := range []string{newestWhenFirstInstalled, newestOnEveryRun} {
		t.Run(policy, func(t *testing.T) {
			for _, availableAfterRefresh := range []bool{true, false} {
				t.Run(map[bool]string{true: "source supplies package", false: "package truly missing"}[availableAfterRefresh], func(t *testing.T) {
					dir := aptFixture(t)
					manager := aptIn(dir)
					manager.PathChanged("/etc/apt/sources.list.d/bookworm-backports.sources")
					originalRule := "Package: incus-base\nPin: version 6.*\nPin-Priority: 1001\n"
					put(t, manager.versionRulePath("incus-base"), originalRule)
					put(t, filepath.Join(dir, "plan"), "Inst incus-base (6.0-1 Backports:stable [amd64])\n")
					if availableAfterRefresh {
						put(t, filepath.Join(dir, "available-after-refresh"), "")
					}
					writeStandInProgram(t, filepath.Join(dir, "preview-check"), `#!/bin/sh
set -eu
F=$MMX_APT_FIXTURE
if [ -f "$F/available-after-refresh" ] && grep -q 'apt-get .*update' "$F/calls" 2>/dev/null; then
 exit 0
fi
echo 'E: Unable to locate package incus-base' >&2
exit 100
`)
					task := Package{Manager: "apt", Packages: []PackageEntry{{Name: "incus-base", Version: policy}}}
					preview := manager.Ensure(testContext(t), Runtime{Dryrun: true}, task)
					if preview.Status != "not_checked" || preview.Change != "unknown" || !strings.Contains(preview.Reason, "current package indexes") || !strings.Contains(preview.Reason, "Unable to locate package incus-base") {
						t.Fatal(preview)
					}
					if calls := fixtureCalls(t, dir); calls != "" {
						t.Fatal("dryrun changed packages or indexes", calls)
					}
					if rule := readState(t, manager.versionRulePath("incus-base")); rule != originalRule {
						t.Fatal("dryrun changed the rule", rule)
					}
					for _, path := range []string{"incus-base", "root/usr/sbin/policy-rc.d"} {
						if _, err := os.Stat(filepath.Join(dir, path)); !os.IsNotExist(err) {
							t.Fatal("dryrun wrote", path, err)
						}
					}
					applied := manager.Ensure(testContext(t), Runtime{}, task)
					if calls := fixtureCalls(t, dir); !strings.Contains(calls, "update") {
						t.Fatal("real run did not refresh indexes", calls)
					}
					if availableAfterRefresh {
						if applied.Status != "succeeded" || applied.Change != "changed" {
							t.Fatal(applied)
						}
						installed, err := manager.Inspect(testContext(t), "incus-base")
						if err != nil || installed.Version != "6.0-1" {
							t.Fatal(installed, err)
						}
					} else {
						if applied.Status != "failed" || applied.ExitCode != 100 || !strings.Contains(applied.Stderr, "Unable to locate package incus-base") {
							t.Fatal(applied)
						}
						if rule := readState(t, manager.versionRulePath("incus-base")); rule != originalRule {
							t.Fatal("refusal changed the rule", rule)
						}
						if _, err := os.Stat(filepath.Join(dir, "incus-base")); !os.IsNotExist(err) {
							t.Fatal("refusal installed package", err)
						}
					}
				})
			}
		})
	}
}

func TestAPTDependencyRefusalStillFailsInDryrun(t *testing.T) {
	dir := aptFixture(t)
	writeStandInProgram(t, filepath.Join(dir, "preview-check"), `#!/bin/sh
echo 'E: Unable to correct problems, you have held broken packages.' >&2
exit 100
`)
	task := Package{Manager: "apt", Packages: []PackageEntry{{Name: "application", Version: newestWhenFirstInstalled}}}
	result := aptIn(dir).Ensure(testContext(t), Runtime{Dryrun: true}, task)
	if result.Status != "failed" || result.ExitCode != 100 || !strings.Contains(result.Stderr, "held broken packages") || fixtureCalls(t, dir) != "" {
		t.Fatal(result, fixtureCalls(t, dir))
	}
}

// A node that stopped inside dpkg fails every apt-get install until a person
// runs dpkg --configure -a, so a retry cannot help: the failure is a missing
// prerequisite, which MMX does not retry, and it says what to run first.
func TestAPTInterruptedDPKGIsNotRetriedAndNamesTheCommand(t *testing.T) {
	t.Parallel()
	dir := aptFixture(t)
	script := `[ "$1" = -s ] && exit 0
case " $* " in
*" update "*) exit 0;;
esac
echo "E: dpkg was interrupted, you must manually run 'dpkg --configure -a' to correct the problem." >&2
exit 100
`
	writeStandInProgram(t, filepath.Join(dir, "apt-get"), "#!/bin/sh\nset -eu\n"+script)
	r := aptIn(dir).Ensure(testContext(t), Runtime{}, Package{Manager: "apt", Packages: []PackageEntry{{Name: "stuck", Version: newestWhenFirstInstalled}}})
	if r.Status != "failed" || r.FailureKind != "missing_prerequisite" {
		t.Fatalf("an interrupted dpkg is retried: %+v", r)
	}
	if !strings.Contains(r.Reason, "Run dpkg --configure -a on the node first") || !strings.Contains(r.Stderr, "dpkg was interrupted") {
		t.Fatal(r.Reason, r.Stderr)
	}
}
