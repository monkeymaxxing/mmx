package modules

import (
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"mmx/internal/value"
)

// Stand-ins for the installers keep what is installed in $store, one
// "name version" line each: $dir/installed, unless a stand-in sets store
// from its tool's own environment variable; "fail" makes a change fail.
const fakeInstalled = `
store=${store:-$dir/installed}
touch "$store"
installed() { grep "^$1 " "$store" | cut -d' ' -f2; }
add() { echo "$1 $2" >> "$store"; }
drop() { grep -v "^$1 ${2:-}" "$store" > "$store.kept" || true; mv "$store.kept" "$store"; }
refuse() { [ ! -e "$dir/fail" ] || { echo "ERROR: the installer failed" >&2; exit 1; }; }
`

// gem lists "name (v1, v2)" and installs the newest version, 2.0, unless
// told one. With GEM_HOME, it keeps the gems there, as RubyGems does.
const fakeGem = `store=${GEM_HOME:+$GEM_HOME/installed}` + fakeInstalled + `
case "$1" in
list) shift 3; for n in "$@"; do v=$(installed $n | sort | paste -sd, - | sed 's/,/, /g'); [ -z "$v" ] || echo "$n ($v)"; done;;
outdated) cat "$dir/outdated" 2>/dev/null; true;;
install) refuse; shift 2; v=2.0; names=; while [ $# -gt 0 ]; do case "$1" in --version) v=$2; shift;; *) names="$names $1";; esac; shift; done
  for spec in $names; do n=${spec%%:*}; v=${spec#*:}; [ "$n" != "$v" ] || v=2.0; drop $n $v; add $n $v; done;;
uninstall) refuse; n=$2; if [ "$3" = --version ]; then drop $n $4; else drop $n; fi;;
esac`

func TestGemPackage(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	dir := standInPrograms(t, map[string]string{"gem": fakeGem})
	g := GemPackage{RunAsUserAccount: currentInstallerAccount(t), Packages: []InstallerPackage{{Name: "bundler", Version: "2.5.23"}}, GemProgramPath: filepath.Join(dir, "gem")}
	if err := g.Prepare(""); err != nil {
		t.Fatal(err)
	}
	writeState(t, filepath.Join(dir, "installed"), "bundler 2.4.0\n")
	want := []any{installedChange("bundler", "2.4.0", "2.4.0, 2.5.23")}
	if r := noMutation(t).GemPackage(ctx, g, value.Scope{}); r.Change != "would_change" || !reflect.DeepEqual(r.Value, want) {
		t.Fatalf("%+v", r)
	}
	if r := (Runtime{}).GemPackage(ctx, g, value.Scope{}); r.Change != "changed" || !reflect.DeepEqual(r.Value, want) {
		t.Fatalf("%+v", r)
	}
	if state, _ := g.ReadState(ctx, nil); state != "bundler 2.4.0, 2.5.23\n" {
		t.Fatalf("%q", state)
	}
	for _, run := range []Runtime{{}, noMutation(t)} {
		if r := run.GemPackage(ctx, g, value.Scope{}); r.Change != "unchanged" {
			t.Fatalf("%+v", r)
		}
	}
	// absent with a version removes only that one.
	g.State = "absent"
	if r := (Runtime{}).GemPackage(ctx, g, value.Scope{}); r.Change != "changed" || !reflect.DeepEqual(r.Value, []any{installedChange("bundler", "2.4.0, 2.5.23", "2.4.0")}) {
		t.Fatalf("%+v", r)
	}
	// The first-install keyword accepts any installed version. Removal
	// with name only removes every installed version.
	g.Packages[0].Version, g.State = newestWhenFirstInstalled, ""
	if r := (Runtime{}).GemPackage(ctx, g, value.Scope{}); r.Change != "unchanged" {
		t.Fatalf("%+v", r)
	}
	g.State = "absent"
	g.Packages[0].Version = ""
	if r := (Runtime{}).GemPackage(ctx, g, value.Scope{}); r.Change != "changed" || readState(t, filepath.Join(dir, "installed")) != "" {
		t.Fatalf("%+v", r)
	}
	// upgrade installs a missing gem and upgrades an outdated one only.
	writeState(t, filepath.Join(dir, "installed"), "rake 13.0.6\nrack 3.1.0\n")
	writeState(t, filepath.Join(dir, "outdated"), "rake (13.0.6 < 13.2.1)\n")
	up := GemPackage{RunAsUserAccount: currentInstallerAccount(t), Packages: []InstallerPackage{{Name: "rake", Version: newestOnEveryRun}, {Name: "rack", Version: newestOnEveryRun}, {Name: "puma", Version: newestOnEveryRun}}, GemProgramPath: g.GemProgramPath}
	want = []any{installedChange("rake", "13.0.6", "13.2.1"), installedChange("puma", nil, newestVersion)}
	if r := noMutation(t).GemPackage(ctx, up, value.Scope{}); r.Change != "would_change" || !reflect.DeepEqual(r.Value, want) {
		t.Fatalf("%+v", r)
	}
	takeCalls(t, dir)
	if r := (Runtime{}).GemPackage(ctx, up, value.Scope{}); r.Change != "changed" {
		t.Fatalf("%+v", r)
	}
	if calls := changing(takeCalls(t, dir), "gem list", "gem outdated"); !reflect.DeepEqual(calls, []string{"gem install --no-document rake puma"}) {
		t.Fatal(calls)
	}
	// A failure keeps the reason.
	writeState(t, filepath.Join(dir, "fail"), "")
	if r := (Runtime{}).GemPackage(ctx, GemPackage{RunAsUserAccount: currentInstallerAccount(t), Packages: []InstallerPackage{{Name: "sinatra", Version: newestWhenFirstInstalled}}, GemProgramPath: g.GemProgramPath}, value.Scope{}); r.Status != "failed" || !strings.Contains(r.Reason, "the installer failed") {
		t.Fatalf("%+v", r)
	}
	if err := (&GemPackage{RunAsUserAccount: currentInstallerAccount(t), Packages: []InstallerPackage{{Name: "a"}}}).Prepare(""); err == nil || !strings.Contains(err.Error(), "version is required") {
		t.Fatal(err)
	}
}

// npm keeps global packages in $dir/installed, or with NPM_CONFIG_PREFIX
// in that prefix, as npm does, and installs version 5.4.3 unless told one;
// ci records its arguments and makes node_modules.
const fakeNpm = `store=${NPM_CONFIG_PREFIX:+$NPM_CONFIG_PREFIX/installed}` + fakeInstalled + `
case "$1" in
--version) echo 10.8.2;;
ls) printf '{"dependencies": {'; sep=; while read n v; do printf '%s"%s": {"version": "%s"}' "$sep" "$n" "$v"; sep=,; done < "$store"; echo '}}';;
outdated) if [ -s "$dir/outdated" ]; then cat "$dir/outdated"; exit 1; fi;;
install) refuse; shift 4; for a in "$@"; do n=${a%@*}; v=${a##*@}; [ "$n" != "" ] && [ "$a" != "$v" ] || { n=$a; v=5.4.3; }; [ "$v" != latest ] || v=6.0.0; drop $n; add $n $v; done;;
uninstall) refuse; shift 4; for n in "$@"; do drop $n; done;;
ci) refuse; echo "$*" >> "$dir/ci"; mkdir -p node_modules;;
esac`

func TestNpmGlobalPackage(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	dir := standInPrograms(t, map[string]string{"npm": fakeNpm})
	n := NpmGlobalPackage{RunAsUserAccount: currentInstallerAccount(t), Packages: []InstallerPackage{{Name: "pm2", Version: newestWhenFirstInstalled}, {Name: "@angular/cli", Version: newestWhenFirstInstalled}}, NpmProgramPath: filepath.Join(dir, "npm")}
	writeState(t, filepath.Join(dir, "installed"), "pm2 5.0.0\n")
	want := []any{installedChange("@angular/cli", nil, newestVersion)}
	if r := noMutation(t).NpmGlobalPackage(ctx, n, value.Scope{}); r.Change != "would_change" || !reflect.DeepEqual(r.Value, want) {
		t.Fatalf("%+v", r)
	}
	takeCalls(t, dir)
	if r := (Runtime{}).NpmGlobalPackage(ctx, n, value.Scope{}); r.Change != "changed" || !reflect.DeepEqual(r.Value, []any{installedChange("@angular/cli", nil, "5.4.3")}) {
		t.Fatalf("%+v", r)
	}
	if calls := changing(takeCalls(t, dir), "npm ls"); !reflect.DeepEqual(calls, []string{"npm install --global --no-fund --no-audit @angular/cli"}) {
		t.Fatal(calls)
	}
	if r := (Runtime{}).NpmGlobalPackage(ctx, n, value.Scope{}); r.Change != "unchanged" {
		t.Fatalf("%+v", r)
	}
	// A version is installed exactly; upgrade follows npm outdated.
	pinned := NpmGlobalPackage{RunAsUserAccount: currentInstallerAccount(t), Packages: []InstallerPackage{{Name: "pm2", Version: "5.4.3"}}, NpmProgramPath: n.NpmProgramPath}
	if r := (Runtime{}).NpmGlobalPackage(ctx, pinned, value.Scope{}); r.Change != "changed" || !reflect.DeepEqual(r.Value, []any{installedChange("pm2", "5.0.0", "5.4.3")}) {
		t.Fatalf("%+v", r)
	}
	// npm lists 5.4.3 for a version written as v5.4.3, as npm's release notes write it.
	pinned.Packages[0].Version = "v5.4.3"
	if r := noMutation(t).NpmGlobalPackage(ctx, pinned, value.Scope{}); r.Change != "unchanged" {
		t.Fatalf("%+v", r)
	}
	writeState(t, filepath.Join(dir, "outdated"), `{"pm2": {"current": "5.4.3", "wanted": "5.4.3", "latest": "6.0.0"}}`)
	up := NpmGlobalPackage{RunAsUserAccount: currentInstallerAccount(t), Packages: []InstallerPackage{{Name: "pm2", Version: newestOnEveryRun}, {Name: "@angular/cli", Version: newestOnEveryRun}}, NpmProgramPath: n.NpmProgramPath}
	if r := (Runtime{}).NpmGlobalPackage(ctx, up, value.Scope{}); r.Change != "changed" || !reflect.DeepEqual(r.Value, []any{installedChange("pm2", "5.4.3", "6.0.0")}) {
		t.Fatalf("%+v", r)
	}
	absent := NpmGlobalPackage{RunAsUserAccount: currentInstallerAccount(t), Packages: []InstallerPackage{{Name: "pm2"}, {Name: "nodemon"}}, State: "absent", NpmProgramPath: n.NpmProgramPath}
	if r := (Runtime{}).NpmGlobalPackage(ctx, absent, value.Scope{}); r.Change != "changed" || !reflect.DeepEqual(r.Value, []any{installedChange("pm2", "6.0.0", nil)}) {
		t.Fatalf("%+v", r)
	}
	if err := npmPackageName("names")("pm2@5"); err == nil {
		t.Error("accepted a name with a version")
	}
	if err := npmPackageName("names")("@angular/cli"); err != nil {
		t.Error(err)
	}
}

// gem_package and npm_global_package read their tracked input with the
// task's env, resolved in the task's scope: with GEM_HOME or
// NPM_CONFIG_PREFIX, the tool lists what the task installed there, so a
// new version changes the state that a service of the node task block
// follows.
func TestNamedInstallersReadTheirStateWithTheTasksEnv(t *testing.T) {
	ctx := testContext(t)
	dir := standInPrograms(t, map[string]string{"gem": fakeGem, "npm": fakeNpm})
	scope := value.Scope{"vars": map[string]any{"gem_home": t.TempDir(), "npm_prefix": t.TempDir()}}
	gem := GemPackage{RunAsUserAccount: currentInstallerAccount(t), Packages: []InstallerPackage{{Name: "rack", Version: "3.1.7"}}, GemProgramPath: filepath.Join(dir, "gem"), Env: map[string]any{"GEM_HOME": "{{ .vars.gem_home }}"}}
	npm := NpmGlobalPackage{RunAsUserAccount: currentInstallerAccount(t), Packages: []InstallerPackage{{Name: "pm2", Version: "5.4.2"}}, NpmProgramPath: filepath.Join(dir, "npm"), Env: map[string]any{"NPM_CONFIG_PREFIX": "{{ .vars.npm_prefix }}"}}
	install := func() {
		t.Helper()
		if r := (Runtime{}).GemPackage(ctx, gem, scope); r.Change != "changed" {
			t.Fatalf("%+v", r)
		}
		if r := (Runtime{}).NpmGlobalPackage(ctx, npm, scope); r.Change != "changed" {
			t.Fatalf("%+v", r)
		}
	}
	install()
	if state, err := gem.ReadState(ctx, scope); state != "rack 3.1.7\n" || err != nil {
		t.Fatalf("gem state %q, %v", state, err)
	}
	if state, err := npm.ReadState(ctx, scope); state != "pm2 5.4.2\n" || err != nil {
		t.Fatalf("npm state %q, %v", state, err)
	}
	gem.Packages[0].Version, npm.Packages[0].Version = "3.1.8", "5.4.3"
	install()
	if state, err := gem.ReadState(ctx, scope); state != "rack 3.1.7, 3.1.8\n" || err != nil {
		t.Fatalf("gem state after the new version %q, %v", state, err)
	}
	if state, err := npm.ReadState(ctx, scope); state != "pm2 5.4.3\n" || err != nil {
		t.Fatalf("npm state after the new version %q, %v", state, err)
	}
}

// snap lists a header and "name version revision tracking publisher notes".
const fakeSnap = fakeInstalled + `
case "$1" in
info) echo 'notes:'; if [ "$3" = certbot ]; then echo '  confinement: classic'; else echo '  confinement: strict'; fi;;
list) echo "Name Version Rev Tracking Publisher Notes"; while read n r t; do echo "$n 1.0 $r $t canonical** -"; done < "$store";;
install) refuse; drop $2; add $2 "10 ${3#--channel=}";;
refresh) refuse; drop $2; add $2 "11 ${3#--channel=}";;
remove) refuse; shift; for n in "$@"; do drop $n; done;;
esac`

func TestSnapPackage(t *testing.T) {
	ctx := testContext(t)
	dir := standInPrograms(t, map[string]string{"snap": fakeSnap})
	previous := standardPath
	standardPath = dir + string(os.PathListSeparator) + previous
	t.Cleanup(func() { standardPath = previous })
	p := SnapPackage{Packages: []SnapPackageEntry{{Name: "certbot", Channel: "latest/stable", AllowAccessOutsideSnapSandbox: true}}}
	if err := p.Prepare(""); err != nil {
		t.Fatal(err)
	}
	if r := noMutation(t).SnapPackage(ctx, p); r.Change != "would_change" || !reflect.DeepEqual(r.Value, []any{installedChange("certbot", nil, "latest/stable")}) {
		t.Fatalf("%+v", r)
	}
	takeCalls(t, dir)
	if r := (Runtime{}).SnapPackage(ctx, p); r.Change != "changed" || !reflect.DeepEqual(r.Value, []any{installedChange("certbot", nil, "latest/stable revision 10")}) {
		t.Fatalf("%+v", r)
	}
	if calls := changing(takeCalls(t, dir), "snap list", "snap info"); !reflect.DeepEqual(calls, []string{"snap install certbot --channel=latest/stable --classic"}) {
		t.Fatal(calls)
	}
	if r := (Runtime{}).SnapPackage(ctx, p); r.Change != "unchanged" {
		t.Fatalf("%+v", r)
	}
	if state, _ := p.ReadState(ctx, nil); state != "certbot latest/stable revision 10\n" {
		t.Fatalf("%q", state)
	}
	// Another channel switches the snap; "stable" is on the track latest.
	p.Packages[0].Channel = "latest/edge"
	if r := (Runtime{}).SnapPackage(ctx, p); r.Change != "changed" || !reflect.DeepEqual(r.Value, []any{installedChange("certbot", "latest/stable revision 10", "latest/edge revision 11")}) {
		t.Fatalf("%+v", r)
	}
	for channel, tracked := range map[string]string{"stable": "latest/stable", "24": "24/stable", "24/edge": "24/edge"} {
		if got := snapChannel(channel); got != tracked {
			t.Error(channel, got)
		}
	}
	// Each entry carries its own channel and access choice. snap's native
	// command accepts one channel selection per call.
	p.Packages = append(p.Packages, SnapPackageEntry{Name: "hello-world", Channel: "24/stable"})
	takeCalls(t, dir)
	if result := (Runtime{}).SnapPackage(ctx, p); result.Change != "changed" {
		t.Fatalf("%+v", result)
	}
	if calls := changing(takeCalls(t, dir), "snap list", "snap info"); !reflect.DeepEqual(calls, []string{"snap install hello-world --channel=24/stable"}) {
		t.Fatal(calls)
	}
	p.Packages[0].Channel, p.Packages[1].Channel = "latest/candidate", "24/edge"
	if result := (Runtime{}).SnapPackage(ctx, p); result.Change != "changed" {
		t.Fatalf("%+v", result)
	}
	if calls := changing(takeCalls(t, dir), "snap list", "snap info"); !reflect.DeepEqual(calls, []string{"snap refresh certbot --channel=latest/candidate --classic", "snap refresh hello-world --channel=24/edge"}) {
		t.Fatal(calls)
	}
	if r := (Runtime{}).SnapPackage(ctx, SnapPackage{Packages: []SnapPackageEntry{{Name: "certbot"}, {Name: "hello-world"}}, State: "absent"}); r.Change != "changed" || readState(t, filepath.Join(dir, "installed")) != "" {
		t.Fatalf("%+v", r)
	}
	// Without snap, a dryrun cannot check, and a run names the package.
	standardPath = t.TempDir()
	if r := noMutation(t).SnapPackage(ctx, p); r.Status != "not_checked" || !strings.Contains(r.Reason, missingProgramError("snap").Error()) {
		t.Fatalf("%+v", r)
	}
	for _, bad := range []SnapPackage{{Packages: []SnapPackageEntry{{Name: "a"}}}, {Packages: []SnapPackageEntry{{Name: "a", Channel: "stable"}}, State: "absent"}} {
		if err := bad.Prepare(""); err == nil {
			t.Errorf("%+v", bad)
		}
	}
}

func TestNpmProjectDependenciesFollowTheirRecord(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	dir := standInPrograms(t, map[string]string{"npm": fakeNpm, "node": `echo v20.18.0`})
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(t.TempDir(), "app")
	production := false
	n := NpmProjectDependencies{LockFileDir: project, RunAsUserAccount: me.Username, IncludeDevelopmentDependencies: &production, NpmProgramPath: filepath.Join(dir, "npm")}
	if err := n.Prepare(""); err != nil {
		t.Fatal(err)
	}
	// A lock file directory that does not exist yet: an earlier checkout may
	// create it.
	if r := noMutation(t).installProjectDependencies(ctx, n.task(value.Scope{})); r.Status != "not_checked" || !strings.Contains(r.Reason, "does not exist yet") {
		t.Fatalf("%+v", r)
	}
	writeState(t, filepath.Join(project, "package.json"), "{}\n")
	if r := (Runtime{}).installProjectDependencies(ctx, n.task(value.Scope{})); r.Status != "failed" || r.FailureKind != "config" || !strings.Contains(r.Reason, "has no package-lock.json or npm-shrinkwrap.json") {
		t.Fatalf("%+v", r)
	}
	writeState(t, filepath.Join(project, "package-lock.json"), `{"lockfileVersion": 3}`)
	if r := noMutation(t).installProjectDependencies(ctx, n.task(value.Scope{})); r.Change != "would_change" || r.Reason != "would install: there is no record of an earlier installation" {
		t.Fatalf("%+v", r)
	}
	r := (Runtime{}).installProjectDependencies(ctx, n.task(value.Scope{}))
	if r.Status != "succeeded" || r.Change != "changed" || readState(t, filepath.Join(dir, "ci")) != "ci --omit=dev --no-fund --no-audit\n" {
		t.Fatalf("%+v", r)
	}
	record, _ := n.ReadState(ctx, nil)
	if !strings.Contains(record, `"lock_file": "package-lock.json"`) || !strings.Contains(record, `"npm 10.8.2"`) || !strings.Contains(record, `"v20.18.0"`) {
		t.Fatal(record)
	}
	for _, run := range []Runtime{{}, noMutation(t)} {
		if r := run.installProjectDependencies(ctx, n.task(value.Scope{})); r.Change != "unchanged" {
			t.Fatalf("%+v", r)
		}
	}
	// In a dryrun, a lock file that an earlier node task would change, such
	// as a git checkout of the directory, is not checked; an earlier change
	// to another file in the directory leaves the task unchanged.
	checkout := noMutation(t)
	checkout.PreviewPaths = []string{project}
	if r := checkout.installProjectDependencies(ctx, n.task(value.Scope{})); r.Status != "not_checked" || r.Reason != "an earlier node task would change the dependency file "+filepath.Join(project, "package-lock.json")+", so a dryrun cannot tell what npm_project_dependencies would install" {
		t.Fatalf("%+v", r)
	}
	elsewhere := noMutation(t)
	elsewhere.PreviewPaths = []string{filepath.Join(project, "config.json")}
	if r := elsewhere.installProjectDependencies(ctx, n.task(value.Scope{})); r.Change != "unchanged" {
		t.Fatalf("%+v", r)
	}
	// A changed lock file, a changed setting, and missing packages each
	// install again.
	writeState(t, filepath.Join(project, "package-lock.json"), `{"lockfileVersion": 3, "packages": {}}`)
	if r := noMutation(t).installProjectDependencies(ctx, n.task(value.Scope{})); r.Reason != "would install: package-lock.json changed since the last installation" {
		t.Fatalf("%+v", r)
	}
	if r := (Runtime{}).installProjectDependencies(ctx, n.task(value.Scope{})); r.Change != "changed" {
		t.Fatalf("%+v", r)
	}
	development := true
	n.IncludeDevelopmentDependencies = &development
	if r := noMutation(t).installProjectDependencies(ctx, n.task(value.Scope{})); r.Reason != "would install: the task's settings changed since the last installation" {
		t.Fatalf("%+v", r)
	}
	n.IncludeDevelopmentDependencies = &production
	if err := os.RemoveAll(filepath.Join(project, "node_modules")); err != nil {
		t.Fatal(err)
	}
	if r := noMutation(t).installProjectDependencies(ctx, n.task(value.Scope{})); r.Reason != "would install: node_modules is missing" {
		t.Fatalf("%+v", r)
	}
	// A failed installation keeps the old record, so the next run installs
	// again.
	record, _ = n.ReadState(ctx, nil)
	writeState(t, filepath.Join(dir, "fail"), "")
	if r := (Runtime{}).installProjectDependencies(ctx, n.task(value.Scope{})); r.Status != "failed" || r.Change != "unknown" {
		t.Fatalf("%+v", r)
	}
	if after, _ := n.ReadState(ctx, nil); after != record {
		t.Fatal("the record changed after a failed installation")
	}
}

// uv syncs with the lock file as it is and never downloads a Python; a
// named Python is passed to uv and recorded with its version.
func TestUvProjectDependenciesFollowTheirRecord(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	dir := standInPrograms(t, map[string]string{
		"uv":      fakeInstalled + `case "$1" in --version) echo "uv 0.13.0 (1a2b3c4d 2026-10-09)";; sync) refuse; echo "$*" >> "$dir/sync"; mkdir -p .venv;; esac`,
		"python3": `echo "Python 3.14.8"`,
	})
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	project := t.TempDir()
	writeState(t, filepath.Join(project, "pyproject.toml"), "[project]\nname = \"app\"\n")
	writeState(t, filepath.Join(project, "uv.lock"), "version = 1\n")
	python := filepath.Join(t.TempDir(), "cpython-3.14.8-linux-x86_64-gnu", "bin", "python3")
	production := false
	u := UvProjectDependencies{LockFileDir: project, RunAsUserAccount: me.Username, IncludeDevelopmentDependencies: &production,
		UvProgramPath: filepath.Join(dir, "uv"), PythonProgramPath: python}
	if err := u.Prepare(""); err != nil {
		t.Fatal(err)
	}
	// A Python that an earlier task installs is not checked yet.
	if r := noMutation(t).installProjectDependencies(ctx, u.task(value.Scope{})); r.Status != "not_checked" || r.Reason != "uv_project_dependencies: python_program_path "+python+" is not an executable program; an earlier task may install it" {
		t.Fatalf("%+v", r)
	}
	writeStandInProgram(t, python, `echo "Python 3.14.8"`)
	if r := noMutation(t).installProjectDependencies(ctx, u.task(value.Scope{})); r.Change != "would_change" || r.Reason != "would install: there is no record of an earlier installation" {
		t.Fatalf("%+v", r)
	}
	r := (Runtime{}).installProjectDependencies(ctx, u.task(value.Scope{}))
	if r.Status != "succeeded" || r.Change != "changed" || readState(t, filepath.Join(dir, "sync")) != "sync --locked --no-python-downloads --no-dev --python "+python+"\n" {
		t.Fatalf("%+v %q", r, readState(t, filepath.Join(dir, "sync")))
	}
	record, _ := u.ReadState(ctx, nil)
	for _, want := range []string{`"lock_file": "uv.lock"`, `"uv uv 0.13.0 (1a2b3c4d 2026-10-09)"`, `"Python 3.14.8"`, `"python_program_path": "` + python + `"`} {
		if !strings.Contains(record, want) {
			t.Fatal(want, record)
		}
	}
	if r := noMutation(t).installProjectDependencies(ctx, u.task(value.Scope{})); r.Change != "unchanged" {
		t.Fatalf("%+v", r)
	}
	// Another Python is another setting; without one, uv chooses and the
	// record names python3 on PATH.
	u.PythonProgramPath = ""
	if r := noMutation(t).installProjectDependencies(ctx, u.task(value.Scope{})); r.Reason != "would install: the task's settings changed since the last installation" {
		t.Fatalf("%+v", r)
	}
	development := true
	u.IncludeDevelopmentDependencies = &development
	if r := (Runtime{}).installProjectDependencies(ctx, u.task(value.Scope{})); r.Change != "changed" {
		t.Fatalf("%+v", r)
	}
	if calls := strings.Split(strings.TrimSpace(readState(t, filepath.Join(dir, "sync"))), "\n"); calls[len(calls)-1] != "sync --locked --no-python-downloads --dev" {
		t.Fatal(calls)
	}
	for _, missing := range []UvProjectDependencies{{RunAsUserAccount: "app", IncludeDevelopmentDependencies: &production}, {LockFileDir: "/srv/app", IncludeDevelopmentDependencies: &production}, {LockFileDir: "/srv/app", RunAsUserAccount: "app"}} {
		if err := missing.Prepare(""); err == nil {
			t.Errorf("%+v", missing)
		}
	}
}

func TestLockFileModulesChooseTheirInstallCommands(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	// Yarn 1 and Yarn 2 and newer refuse to change yarn.lock differently.
	production, development := false, true
	y := YarnProjectDependencies{LockFileDir: "/srv/app", RunAsUserAccount: "app", IncludeDevelopmentDependencies: &production}
	for version, want := range map[string]string{
		"1.22.22": "install --frozen-lockfile --non-interactive --production=true",
		"4.5.3":   "workspaces focus --all --production",
	} {
		if got := strings.Join(y.task(nil).install(version), " "); got != want {
			t.Error(version, got)
		}
	}
	y.IncludeDevelopmentDependencies = &development
	if got := strings.Join(y.task(nil).install("4.5.3"), " "); got != "install --immutable" {
		t.Error(got)
	}
	// Bundler's settings reach it through its environment, and the result
	// lists them.
	b := BundlerProjectDependencies{LockFileDir: "/srv/app", RunAsUserAccount: "app", WithoutGemfileGroups: []string{"development", "test"}}
	task := b.task(nil)
	if task.env["BUNDLE_WITHOUT"] != "development:test" || task.env["BUNDLE_PATH"] != "vendor/bundle" || task.env["BUNDLE_DEPLOYMENT"] != "true" {
		t.Fatal(task.env)
	}
	if !reflect.DeepEqual(task.value["bundler_settings"], task.chosen) {
		t.Fatal(task.value)
	}
	if err := (&BundlerProjectDependencies{LockFileDir: "/srv/app", RunAsUserAccount: "app"}).Prepare(""); err == nil || !strings.Contains(err.Error(), "without_gemfile_groups is required") {
		t.Fatal(err)
	}
	// Composer leaves out require-dev without development dependencies, and
	// a dryrun lists composer's operations.
	dir := standInPrograms(t, map[string]string{"composer": `case " $* " in *" --dry-run "*) echo "Installing dependencies from lock file" >&2; echo "  - Installing psr/log (3.0.2)" >&2;; *" --version "*) echo "Composer version 2.8.4 2024-12-11 11:57:47";; esac`, "php": `echo "PHP 8.2.26 (cli)"`})
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	project := t.TempDir()
	writeState(t, filepath.Join(project, "composer.lock"), "{}\n")
	writeState(t, filepath.Join(project, "composer.json"), "{}\n")
	c := ComposerProjectDependencies{LockFileDir: project, RunAsUserAccount: me.Username, IncludeDevelopmentDependencies: &production, ComposerProgramPath: filepath.Join(dir, "composer")}
	r := noMutation(t).installProjectDependencies(ctx, c.task(value.Scope{}))
	if r.Change != "would_change" || !reflect.DeepEqual(r.Value.(map[string]any)["operations"], []any{"Installing psr/log (3.0.2)"}) {
		t.Fatalf("%+v", r)
	}
	if calls := takeCalls(t, dir); !strings.Contains(strings.Join(calls, "\n"), "composer install --no-interaction --no-progress --no-dev --dry-run") {
		t.Fatal(calls)
	}
	for _, missing := range []NpmProjectDependencies{{RunAsUserAccount: "app", IncludeDevelopmentDependencies: &production}, {LockFileDir: "/srv/app", IncludeDevelopmentDependencies: &production}, {LockFileDir: "/srv/app", RunAsUserAccount: "app"}} {
		if err := missing.Prepare(""); err == nil {
			t.Errorf("%+v", missing)
		}
	}
}

// A dryrun lists the gems that bundle check reports missing: it exits 1 and
// prints them on standard error, as Bundler does. Any other failure of
// bundle check fails the dryrun with what bundle check printed.
func TestBundlerProjectDependenciesDryrunListsMissingGems(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	dir := standInPrograms(t, map[string]string{
		"bundle": `case "$1" in
--version) echo "Bundler version 2.5.22";;
check) cat "$dir/check-output" >&2; exit "$(cat "$dir/check-exit-status")";;
esac`,
		"ruby": `echo "ruby 3.3.6 (2024-11-05 revision 75015d4c1f) [x86_64-linux]"`,
	})
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	project := t.TempDir()
	writeState(t, filepath.Join(project, "Gemfile"), "source \"https://rubygems.org\"\ngem \"rack\"\n")
	writeState(t, filepath.Join(project, "Gemfile.lock"), "GEM\n  specs:\n    rack (3.1.8)\n")
	b := BundlerProjectDependencies{LockFileDir: project, RunAsUserAccount: me.Username, WithoutGemfileGroups: []string{}, BundleProgramPath: filepath.Join(dir, "bundle")}
	if err := b.Prepare(""); err != nil {
		t.Fatal(err)
	}
	check := func(exitStatus, output string) {
		writeState(t, filepath.Join(dir, "check-exit-status"), exitStatus)
		writeState(t, filepath.Join(dir, "check-output"), output)
	}
	dryrun := noMutation(t)
	missing := "The following gems are missing\n * rack (3.1.8)\n * puma (6.4.3)\nInstall missing gems with `bundle install`\n"
	for _, failure := range []struct{ exitStatus, output string }{
		{"1", "There was an error parsing `Gemfile`: syntax error, unexpected end-of-input.\n"},
		{"7", missing},
	} {
		check(failure.exitStatus, failure.output)
		r := dryrun.installProjectDependencies(ctx, b.task(value.Scope{}))
		want := "The bundle check command failed in " + project + " with exit status " + failure.exitStatus + ";"
		firstLine, _, _ := strings.Cut(failure.output, "\n")
		if r.Status != "failed" || !strings.Contains(r.Reason, want) || !strings.Contains(r.Reason, firstLine) {
			t.Fatalf("%+v", r)
		}
	}
	check("1", missing)
	r := dryrun.installProjectDependencies(ctx, b.task(value.Scope{}))
	if r.Status != "succeeded" || r.Change != "would_change" || !reflect.DeepEqual(r.Value.(map[string]any)["missing_gems"], []any{"rack (3.1.8)", "puma (6.4.3)"}) {
		t.Fatalf("%+v", r)
	}
	check("0", "The Gemfile's dependencies are satisfied\n")
	r = dryrun.installProjectDependencies(ctx, b.task(value.Scope{}))
	if r.Status != "succeeded" || r.Change != "would_change" || !reflect.DeepEqual(r.Value.(map[string]any)["missing_gems"], []any{}) {
		t.Fatalf("%+v", r)
	}
	for _, report := range []struct {
		output string
		gems   []any
	}{
		{"The following gems are missing\n", []any{}},
		{" * rack (3.1.8)\n", []any{"rack (3.1.8)"}},
	} {
		check("1", report.output)
		r = dryrun.installProjectDependencies(ctx, b.task(value.Scope{}))
		if r.Status != "succeeded" || !reflect.DeepEqual(r.Value.(map[string]any)["missing_gems"], report.gems) {
			t.Fatalf("missing-gem report %q: %+v", report.output, r)
		}
	}
}

func currentInstallerAccount(t *testing.T) string {
	t.Helper()
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	return account.Username
}

// A manifest-only change must reach the native installer's lock-file check,
// including in a directory with a successful record and installed packages.
func TestLockFileModulesRecordAndCompareTheirManifest(t *testing.T) {
	t.Parallel()
	for _, module := range []string{"npm", "yarn", "bundler", "composer", "uv"} {
		t.Run(module, func(t *testing.T) {
			t.Parallel()
			ctx := testContext(t)
			project := t.TempDir()
			production := false
			var task projectDependencies
			switch module {
			case "npm":
				task = (&NpmProjectDependencies{LockFileDir: project, RunAsUserAccount: currentInstallerAccount(t), IncludeDevelopmentDependencies: &production}).task(nil)
			case "yarn":
				task = (&YarnProjectDependencies{LockFileDir: project, RunAsUserAccount: currentInstallerAccount(t), IncludeDevelopmentDependencies: &production}).task(nil)
			case "bundler":
				task = (&BundlerProjectDependencies{LockFileDir: project, RunAsUserAccount: currentInstallerAccount(t), WithoutGemfileGroups: []string{}}).task(nil)
			case "composer":
				task = (&ComposerProjectDependencies{LockFileDir: project, RunAsUserAccount: currentInstallerAccount(t), IncludeDevelopmentDependencies: &production}).task(nil)
			case "uv":
				task = (&UvProjectDependencies{LockFileDir: project, RunAsUserAccount: currentInstallerAccount(t), IncludeDevelopmentDependencies: &production}).task(nil)
			}
			expectedManifest := map[string]string{"npm": "package.json", "yarn": "package.json", "bundler": "Gemfile", "composer": "composer.json", "uv": "pyproject.toml"}[module]
			expectedLock := map[string]string{"npm": "package-lock.json", "yarn": "yarn.lock", "bundler": "Gemfile.lock", "composer": "composer.lock", "uv": "uv.lock"}[module]
			script := `case "$1" in
--version) echo '4.0.0';;
check) exit 0;;
*) case " $* " in *" --dry-run "*) exit 0;; esac
 if grep -q incompatible ` + expectedManifest + `; then echo 'lock file does not fit manifest' >&2; exit 1; fi
 mkdir -p node_modules vendor/bundle .venv;;
esac`
			dir := standInPrograms(t, map[string]string{task.settings.program: script})
			task.settings.programPath = filepath.Join(dir, task.settings.program)
			writeState(t, filepath.Join(project, expectedManifest), "original\n")
			writeState(t, filepath.Join(project, expectedLock), "locked\n")
			if result := (Runtime{}).installProjectDependencies(ctx, task); result.Change != "changed" || result.Status != "succeeded" {
				t.Fatalf("%+v", result)
			}
			before, err := readDependencyRecord(task.recordPath())
			if err != nil {
				t.Fatal(err)
			}
			if result := noMutation(t).installProjectDependencies(ctx, task); result.Change != "unchanged" {
				t.Fatalf("%+v", result)
			}
			// Preview paths must include the manifest, even with the old bytes still present.
			preview := noMutation(t)
			preview.PreviewPaths = []string{filepath.Join(project, expectedManifest)}
			if result := preview.installProjectDependencies(ctx, task); result.Status != "not_checked" || !strings.Contains(result.Reason, expectedManifest) {
				t.Fatalf("%+v", result)
			}
			writeState(t, filepath.Join(project, expectedManifest), "changed\n")
			if result := noMutation(t).installProjectDependencies(ctx, task); result.Change != "would_change" || result.Reason != "would install: "+expectedManifest+" changed since the last installation" {
				t.Fatalf("%+v", result)
			}
			if result := (Runtime{}).installProjectDependencies(ctx, task); result.Change != "changed" || result.Status != "succeeded" {
				t.Fatalf("%+v", result)
			}
			after, _ := readDependencyRecord(task.recordPath())
			if after == before {
				t.Fatal("manifest-only change did not update the installation record")
			}
			writeState(t, filepath.Join(project, expectedManifest), "incompatible\n")
			if result := (Runtime{}).installProjectDependencies(ctx, task); result.Status != "failed" || !strings.Contains(result.Reason, "lock file does not fit manifest") {
				t.Fatalf("%+v", result)
			}
			if failed, _ := readDependencyRecord(task.recordPath()); failed != after {
				t.Fatal("failed installation replaced the successful record")
			}
		})
	}
}

func TestLanguageInstallersRejectUnstatedChoices(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		inputs Inputs
		want   string
	}{
		{&GemPackage{}, "packages is required"},
		{&GemPackage{Packages: []InstallerPackage{{Name: "rack"}}}, "version is required"},
		{&GemPackage{Packages: []InstallerPackage{{Name: "rack", Version: newestWhenFirstInstalled}}}, "run_as_user_account is required"},
		{&GemPackage{Packages: []InstallerPackage{{Name: "rack", Version: newestOnEveryRun}}, State: "absent"}, "state: absent takes name only"},
		{&NpmGlobalPackage{}, "packages is required"},
		{&NpmGlobalPackage{Packages: []InstallerPackage{{Name: "pm2"}}}, "version is required"},
		{&NpmGlobalPackage{Packages: []InstallerPackage{{Name: "pm2", Version: newestWhenFirstInstalled}}}, "run_as_user_account is required"},
		{&NpmGlobalPackage{Packages: []InstallerPackage{{Name: "pm2", Version: "5.4.3"}}, State: "absent"}, "state: absent takes name only"},
		{&SnapPackage{}, "packages is required"},
		{&SnapPackage{Packages: []SnapPackageEntry{{Channel: "stable"}}}, "name is required"},
		{&SnapPackage{Packages: []SnapPackageEntry{{Name: "certbot"}}}, "channel is required"},
		{&SnapPackage{Packages: []SnapPackageEntry{{Name: "certbot"}, {Name: "certbot"}}, State: "absent"}, "listed twice"},
		{&SnapPackage{Packages: []SnapPackageEntry{{Name: "certbot", AllowAccessOutsideSnapSandbox: true}}, State: "absent"}, "state: absent takes name only"},
	} {
		if err := test.inputs.Prepare(""); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("%T: %v; want %s", test.inputs, err, test.want)
		}
	}
}

func TestLanguageInstallersMixVersionsInOneCommand(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	for _, reversed := range []bool{false, true} {
		t.Run(map[bool]string{false: "forward", true: "reverse"}[reversed], func(t *testing.T) {
			dir := standInPrograms(t, map[string]string{"gem": fakeGem, "npm": fakeNpm})
			gems := []InstallerPackage{{Name: "rack", Version: "3.1.8"}, {Name: "rake", Version: "13.2.1"}, {Name: "bundler", Version: newestWhenFirstInstalled}}
			writeState(t, filepath.Join(dir, "installed"), "bundler 2.4.0\n")
			if reversed {
				slices.Reverse(gems)
			}
			gem := GemPackage{Packages: gems, RunAsUserAccount: currentInstallerAccount(t), GemProgramPath: filepath.Join(dir, "gem")}
			if result := (Runtime{}).GemPackage(ctx, gem, nil); result.Status != "succeeded" || result.Change != "changed" {
				t.Fatalf("%+v", result)
			}
			if state, _ := gem.ReadState(ctx, nil); state != "bundler 2.4.0\nrack 3.1.8\nrake 13.2.1\n" {
				t.Fatal(state)
			}
			calls := changing(takeCalls(t, dir), "gem list")
			if len(calls) != 1 || !strings.Contains(calls[0], "rack:3.1.8") || !strings.Contains(calls[0], "rake:13.2.1") {
				t.Fatal(calls)
			}
			packages := []InstallerPackage{{Name: "pm2", Version: "5.4.3"}, {Name: "yarn", Version: "1.22.22"}, {Name: "nodemon", Version: newestWhenFirstInstalled}}
			if reversed {
				slices.Reverse(packages)
			}
			writeState(t, filepath.Join(dir, "installed"), "nodemon 2.0.0\n")
			npm := NpmGlobalPackage{Packages: packages, RunAsUserAccount: currentInstallerAccount(t), NpmProgramPath: filepath.Join(dir, "npm")}
			if result := (Runtime{}).NpmGlobalPackage(ctx, npm, nil); result.Status != "succeeded" || result.Change != "changed" {
				t.Fatalf("%+v", result)
			}
			if state, _ := npm.ReadState(ctx, nil); state != "nodemon 2.0.0\npm2 5.4.3\nyarn 1.22.22\n" {
				t.Fatal(state)
			}
			calls = changing(takeCalls(t, dir), "npm ls")
			if len(calls) != 1 || !strings.Contains(calls[0], "pm2@5.4.3") || !strings.Contains(calls[0], "yarn@1.22.22") {
				t.Fatal(calls)
			}
		})
	}
}

func TestSnapPackageRefusesAllEntriesBeforeInstalling(t *testing.T) {
	ctx := testContext(t)
	dir := standInPrograms(t, map[string]string{"snap": fakeSnap})
	previous := standardPath
	standardPath = dir + string(os.PathListSeparator) + previous
	t.Cleanup(func() { standardPath = previous })
	packageTask := SnapPackage{Packages: []SnapPackageEntry{
		{Name: "hello-world", Channel: "latest/stable"},
		{Name: "certbot", Channel: "latest/stable"},
	}}
	for _, dryrun := range []bool{false, true} {
		mutations := 0
		run := Runtime{Dryrun: dryrun, BeforeMutation: func(string) error { mutations++; return nil }}
		result := run.SnapPackage(ctx, packageTask)
		if result.Status != "failed" || !strings.Contains(result.Reason, "installing snap certbot allows access outside the snap sandbox, like an ordinary program") || !strings.Contains(result.Reason, "To allow access, add allow_access_outside_snap_sandbox: true") {
			t.Fatalf("dryrun %v: %+v", dryrun, result)
		}
		if mutations != 0 || readState(t, filepath.Join(dir, "installed")) != "" {
			t.Fatalf("dryrun %v: refused task installed a snap (%d mutations)", dryrun, mutations)
		}
		if calls := changing(takeCalls(t, dir), "snap list", "snap info"); len(calls) != 0 {
			t.Fatal(calls)
		}
	}
	packageTask.Packages[1].AllowAccessOutsideSnapSandbox = true
	if result := (Runtime{}).SnapPackage(ctx, packageTask); result.Status != "succeeded" || result.Change != "changed" {
		t.Fatalf("%+v", result)
	}
	if installed := readState(t, filepath.Join(dir, "installed")); !strings.Contains(installed, "hello-world ") || !strings.Contains(installed, "certbot ") {
		t.Fatal(installed)
	}
}

func TestInstallerPackageArgumentsRejectEmbeddedOrNonExactVersions(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		inputs interface{ Arguments(*ArgumentList) }
		field  string
	}{
		{&GemPackage{Packages: []InstallerPackage{{Name: "rack:3.1.8", Version: newestWhenFirstInstalled}}}, "packages[0].name"},
		{&GemPackage{Packages: []InstallerPackage{{Name: "rack", Version: ">=3.1"}}}, "packages[0].version"},
		{&NpmGlobalPackage{Packages: []InstallerPackage{{Name: "pm2@5.4.3", Version: newestWhenFirstInstalled}}}, "packages[0].name"},
		{&NpmGlobalPackage{Packages: []InstallerPackage{{Name: "pm2", Version: "^5.4.3"}}}, "packages[0].version"},
		{&PipPackage{Packages: []InstallerPackage{{Name: "requests==2.32.0", Version: newestWhenFirstInstalled}}}, "packages[0].name"},
		{&PipPackage{Packages: []InstallerPackage{{Name: "requests", Version: "2.32.0"}}}, "packages[0].version"},
	} {
		var arguments ArgumentList
		test.inputs.Arguments(&arguments)
		found := false
		for _, argument := range arguments.List {
			if argument.Field != test.field {
				continue
			}
			found = true
			if err := argument.Rule(argument.Raw); err == nil {
				t.Fatalf("%T accepted %s: %v", test.inputs, test.field, argument.Raw)
			}
		}
		if !found {
			t.Fatalf("%T did not check %s", test.inputs, test.field)
		}
	}
}

func TestNpmGlobalPackageInstallsMissingEveryRunEntriesFromLatest(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	dir := standInPrograms(t, map[string]string{"npm": fakeNpm})
	packageTask := NpmGlobalPackage{Packages: []InstallerPackage{{Name: "pm2", Version: newestOnEveryRun}}, RunAsUserAccount: currentInstallerAccount(t), NpmProgramPath: filepath.Join(dir, "npm")}
	if result := (Runtime{}).NpmGlobalPackage(ctx, packageTask, nil); result.Status != "succeeded" || !reflect.DeepEqual(result.Value, []any{installedChange("pm2", nil, "6.0.0")}) {
		t.Fatalf("%+v", result)
	}
	if calls := changing(takeCalls(t, dir), "npm ls", "npm outdated"); !reflect.DeepEqual(calls, []string{"npm install --global --no-fund --no-audit pm2@latest"}) {
		t.Fatal(calls)
	}
	if result := (Runtime{}).NpmGlobalPackage(ctx, packageTask, nil); result.Change != "unchanged" {
		t.Fatalf("%+v", result)
	}
}

func TestSnapPackageStopsWhenConfinementCannotBeRead(t *testing.T) {
	ctx := testContext(t)
	dir := standInPrograms(t, map[string]string{"snap": `case "$1" in
list) echo 'Name Version Rev Tracking Publisher Notes';;
info) cat "$dir/info";;
*) echo 'changed' >> "$dir/installed";;
esac`})
	previous := standardPath
	standardPath = dir + string(os.PathListSeparator) + previous
	t.Cleanup(func() { standardPath = previous })
	p := SnapPackage{Packages: []SnapPackageEntry{{Name: "hello-world", Channel: "latest/stable"}}}
	for _, info := range []string{"notes: {}", "notes: {confinement: unknown}", "notes: ["} {
		writeState(t, filepath.Join(dir, "info"), info)
		for _, dryrun := range []bool{false, true} {
			mutations := 0
			run := Runtime{Dryrun: dryrun, BeforeMutation: func(string) error { mutations++; return nil }}
			if result := run.SnapPackage(ctx, p); result.Status != "failed" || mutations != 0 {
				t.Fatalf("%q, dryrun %v: %+v, %d mutations", info, dryrun, result, mutations)
			}
		}
	}
}

// When Gemfile.lock names another Bundler than the Ruby's own, installing
// makes the bundle program report the locked version. The record made by
// the installation holds that version, so the next run is unchanged.
func TestBundlerProjectDependenciesRecordsTheBundlerVersionInEffectAfterInstalling(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	dir := standInPrograms(t, map[string]string{
		"bundle": `case "$1" in
--version) echo "Bundler version $(cat "$dir/bundler-version")";;
install) echo 2.6.9 > "$dir/bundler-version"; mkdir -p vendor/bundle;;
esac`,
		"ruby": `echo "ruby 3.3.6 (2024-11-05 revision 75015d4c1f) [x86_64-linux]"`,
	})
	writeState(t, filepath.Join(dir, "bundler-version"), "2.6.7\n")
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	project := t.TempDir()
	writeState(t, filepath.Join(project, "Gemfile"), "source \"https://rubygems.org\"\ngem \"rack\"\n")
	writeState(t, filepath.Join(project, "Gemfile.lock"), "GEM\n  specs:\n    rack (3.1.8)\n\nBUNDLED WITH\n   2.6.9\n")
	b := BundlerProjectDependencies{LockFileDir: project, RunAsUserAccount: me.Username, WithoutGemfileGroups: []string{}, BundleProgramPath: filepath.Join(dir, "bundle")}
	if err := b.Prepare(""); err != nil {
		t.Fatal(err)
	}
	if r := (Runtime{}).installProjectDependencies(ctx, b.task(value.Scope{})); r.Status != "succeeded" || r.Change != "changed" {
		t.Fatalf("%+v", r)
	}
	if r := (Runtime{}).installProjectDependencies(ctx, b.task(value.Scope{})); r.Status != "succeeded" || r.Change != "unchanged" {
		t.Fatalf("the second run: %+v", r)
	}
}
