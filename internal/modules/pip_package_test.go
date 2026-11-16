package modules

import (
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"mmx/internal/value"
)

// A stand-in python3: "-m venv DIR" copies itself to DIR/bin/python (its
// link and its script, as writeStandInProgram makes them), and
// "-m pip" keeps the installed packages in $dir/installed, one name==version
// line each. A name without a version installs the newest version,
// $dir/<name>.newest or 1.0; only --upgrade replaces an installed package.
// A file "fail" makes an installation fail, and "old_pip" makes pip older
// than 22.2.
const fakePython = `
if [ "$1" = -c ]; then
 exec "$MMX_TEST_PYTHON" "$@"
fi
if [ "$1" = -m ] && [ "$2" = venv ]; then
  mkdir -p "$3/bin/.stand-in-scripts"; cp -P "$0" "$3/bin/python"; cp "${0%/*}/.stand-in-scripts/${0##*/}" "$3/bin/.stand-in-scripts/python"; exit 0
fi
shift 4
touch "$dir/installed"
case "$1" in
--version) if [ -e "$dir/old_pip" ]; then echo "pip 21.2.3 from /usr/lib/python3.9/site-packages/pip (python 3.9)"; else echo "pip 23.0.1 from /usr/lib/python3/dist-packages/pip (python 3.11)"; fi;;
freeze) cat "$dir/installed";;
uninstall) shift 2; for n in "$@"; do grep -v "^$n==" "$dir/installed" > "$dir/kept" || true; mv "$dir/kept" "$dir/installed"; done;;
install)
  shift
  case " $* " in *" --dry-run "*) echo '{"version": "1", "install": [{"metadata": {"name": "Gunicorn", "version": "23.0.0"}}]}'; exit 0;; esac
  [ ! -e "$dir/fail" ] || { echo "ERROR: No matching distribution found for nothing-here" >&2; exit 1; }
  upgrade=no
  for r in "$@"; do
    case "$r" in --upgrade) upgrade=yes; continue;; --*) continue;; esac
    n=${r%%==*}; v=${r#*==}; [ "$v" != "$r" ] || v=$(cat "$dir/$n.newest" 2>/dev/null || echo 1.0)
    if grep -q "^$n==" "$dir/installed"; then
      [ $upgrade = yes ] || continue
      grep -v "^$n==" "$dir/installed" > "$dir/kept" || true; mv "$dir/kept" "$dir/installed"
    fi
    echo "$n==$v" >> "$dir/installed"
  done;;
esac`

// pythonWithPip returns python3 when it can run pip. The helpers that these
// tests run import pip's own version parser, which every virtual environment
// that pip_package uses has. A Linux guest of linux_code.py has python3
// without pip; language_installers.py runs the helpers with pip on every
// target distribution.
func pythonWithPip(t *testing.T) string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err == nil {
		err = exec.CommandContext(testContext(t), python, "-m", "pip", "--version").Run()
	}
	if err != nil {
		t.Skip("needs python3 with pip; language_installers.py runs these helpers with pip on every target distribution")
	}
	return python
}

func pipFixture(t *testing.T) (dir string, p PipPackage) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	setWorkerEnvironment(t, "MMX_TEST_PYTHON", python)
	dir = standInPrograms(t, map[string]string{"python3": fakePython})
	venv := filepath.Join(t.TempDir(), "venv")
	return dir, PipPackage{RunAsUserAccount: currentInstallerAccount(t), Packages: []InstallerPackage{{Name: "gunicorn", Version: "==23.0.0"}}, VirtualEnvironment: venv, PythonProgramPath: filepath.Join(dir, "python3")}
}

func TestPipPackageCreatesTheEnvironmentAndInstalls(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	dir, p := pipFixture(t)
	if err := p.Prepare(""); err != nil {
		t.Fatal(err)
	}
	preview := noMutation(t).PipPackage(ctx, p, value.Scope{})
	if preview.Status != "succeeded" || preview.Change != "would_change" || !strings.Contains(preview.Reason, "would create the virtual environment") {
		t.Fatalf("%+v", preview)
	}
	if executableFile(filepath.Join(p.VirtualEnvironment, "bin/python")) {
		t.Fatal("dryrun created the virtual environment")
	}
	mutations := 0
	r := (Runtime{BeforeMutation: func(string) error { mutations++; return nil }}).PipPackage(ctx, p, value.Scope{})
	want := []any{installedChange("gunicorn", nil, "23.0.0")}
	if r.Status != "succeeded" || r.Change != "changed" || !reflect.DeepEqual(r.Value, want) || mutations != 2 {
		t.Fatalf("%+v %d", r, mutations)
	}
	if state, _ := p.ReadState(ctx, nil); state != "gunicorn 23.0.0\n" {
		t.Fatalf("%q", state)
	}
	takeCalls(t, dir)
	for _, run := range []Runtime{{}, noMutation(t)} {
		if r := run.PipPackage(ctx, p, value.Scope{}); r.Status != "succeeded" || r.Change != "unchanged" || !reflect.DeepEqual(r.Value, []any{}) {
			t.Fatalf("%+v", r)
		}
	}
	// The existing virtual environment is used, not created again.
	if calls := strings.Join(takeCalls(t, dir), "\n"); strings.Contains(calls, "-m venv") {
		t.Fatal(calls)
	}
	// A dryrun lists what pip would install.
	writeState(t, filepath.Join(dir, "installed"), "gunicorn==22.0.0\n")
	if r := noMutation(t).PipPackage(ctx, p, value.Scope{}); r.Change != "would_change" || !reflect.DeepEqual(r.Value, []any{installedChange("gunicorn", "22.0.0", "23.0.0")}) {
		t.Fatalf("%+v", r)
	}
	// absent uninstalls only what is installed.
	p.Packages, p.State = []InstallerPackage{{Name: "Gunicorn"}, {Name: "requests"}}, "absent"
	if r := noMutation(t).PipPackage(ctx, p, value.Scope{}); r.Change != "would_change" || !reflect.DeepEqual(r.Value, []any{installedChange("gunicorn", "22.0.0", nil)}) {
		t.Fatalf("%+v", r)
	}
	if r := (Runtime{}).PipPackage(ctx, p, value.Scope{}); r.Change != "changed" || readState(t, filepath.Join(dir, "installed")) != "" {
		t.Fatalf("%+v", r)
	}
	if r := (Runtime{}).PipPackage(ctx, p, value.Scope{}); r.Change != "unchanged" {
		t.Fatalf("%+v", r)
	}
}

func TestPipPackageFailures(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	dir, p := pipFixture(t)
	// An installation that fails keeps what changed before it failed.
	writeState(t, filepath.Join(dir, "fail"), "")
	r := (Runtime{}).PipPackage(ctx, p, value.Scope{})
	if r.Status != "failed" || !strings.Contains(r.Reason, "No matching distribution") {
		t.Fatalf("%+v", r)
	}
	// An older pip cannot say what it would install.
	writeState(t, filepath.Join(dir, "old_pip"), "")
	if r := noMutation(t).PipPackage(ctx, p, value.Scope{}); r.Status != "not_checked" || !strings.Contains(r.Reason, "pip 21.2.3 cannot say") {
		t.Fatalf("%+v", r)
	}
	// A requirements file that is not there yet, and a Python that is not
	// installed yet, are not checked in a dryrun and fail a run.
	p.Packages, p.RequirementsFileOnNode = nil, filepath.Join(t.TempDir(), "requirements.txt")
	if r := noMutation(t).PipPackage(ctx, p, value.Scope{}); r.Status != "not_checked" || !strings.Contains(r.Reason, "requirements file") {
		t.Fatalf("%+v", r)
	}
	other := PipPackage{RunAsUserAccount: currentInstallerAccount(t), Packages: []InstallerPackage{{Name: "requests", Version: newestWhenFirstInstalled}}, VirtualEnvironment: filepath.Join(t.TempDir(), "venv"), PythonProgramPath: "/nonexistent/python3"}
	for run, status := range map[*Runtime]string{{Dryrun: true}: "not_checked", {}: "failed"} {
		if r := run.PipPackage(ctx, other, value.Scope{}); r.Status != status || !strings.Contains(r.Reason, "python_program_path /nonexistent/python3 is not an executable program") {
			t.Fatalf("%+v", r)
		}
	}
	if r := (Runtime{}).PipPackage(ctx, other, value.Scope{}); r.FailureKind != "missing_prerequisite" {
		t.Fatalf("%+v", r)
	}
	// A directory that is not a virtual environment fails.
	notVenv := PipPackage{RunAsUserAccount: currentInstallerAccount(t), Packages: []InstallerPackage{{Name: "requests", Version: newestWhenFirstInstalled}}, VirtualEnvironment: t.TempDir(), PythonProgramPath: filepath.Join(dir, "python3")}
	if r := (Runtime{}).PipPackage(ctx, notVenv, value.Scope{}); r.Status != "failed" || !strings.Contains(r.Reason, "is not a Python virtual environment") {
		t.Fatalf("%+v", r)
	}
}

// The every-run keyword installs a missing package and upgrades an
// installed package. The first-install keyword leaves its version alone.
func TestPipPackageUpgrade(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	dir, p := pipFixture(t)
	p.Packages = []InstallerPackage{{Name: "gunicorn", Version: "==22.0.0"}}
	if r := (Runtime{}).PipPackage(ctx, p, value.Scope{}); r.Change != "changed" {
		t.Fatalf("%+v", r)
	}
	writeState(t, filepath.Join(dir, "gunicorn.newest"), "23.0.0\n")
	p.Packages = []InstallerPackage{{Name: "gunicorn", Version: newestWhenFirstInstalled}}
	if r := (Runtime{}).PipPackage(ctx, p, value.Scope{}); r.Change != "unchanged" {
		t.Fatalf("%+v", r)
	}
	up := PipPackage{RunAsUserAccount: currentInstallerAccount(t), Packages: []InstallerPackage{{Name: "gunicorn", Version: newestOnEveryRun}, {Name: "requests", Version: newestOnEveryRun}}, VirtualEnvironment: p.VirtualEnvironment, PythonProgramPath: p.PythonProgramPath}
	if err := up.Prepare(""); err != nil {
		t.Fatal(err)
	}
	if r := noMutation(t).PipPackage(ctx, up, value.Scope{}); r.Change != "would_change" || !reflect.DeepEqual(r.Value, []any{installedChange("gunicorn", "22.0.0", "23.0.0")}) {
		t.Fatalf("%+v", r)
	}
	takeCalls(t, dir)
	if r := (Runtime{}).PipPackage(ctx, up, value.Scope{}); r.Change != "changed" || !reflect.DeepEqual(r.Value, []any{installedChange("gunicorn", "22.0.0", "23.0.0"), installedChange("requests", nil, "1.0")}) {
		t.Fatalf("%+v", r)
	}
	if calls := strings.Join(takeCalls(t, dir), "\n"); !strings.Contains(calls, "python3 -m pip --disable-pip-version-check --no-input install --upgrade gunicorn requests") {
		t.Fatal(calls)
	}
	if r := (Runtime{}).PipPackage(ctx, up, value.Scope{}); r.Change != "unchanged" {
		t.Fatalf("%+v", r)
	}
}

// install_system_wide installs into the given Python itself, with no
// virtual environment, and follows it as the tracked input.
func TestPipPackageInstallsSystemWide(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	dir, _ := pipFixture(t)
	p := PipPackage{RunAsUserAccount: currentInstallerAccount(t), Packages: []InstallerPackage{{Name: "requests", Version: newestWhenFirstInstalled}}, InstallSystemWide: true, PythonProgramPath: filepath.Join(dir, "python3"), Env: map[string]any{"PIP_BREAK_SYSTEM_PACKAGES": "1"}}
	if err := p.Prepare(""); err != nil {
		t.Fatal(err)
	}
	if r := (Runtime{}).PipPackage(ctx, p, value.Scope{}); r.Status != "succeeded" || r.Change != "changed" || !reflect.DeepEqual(r.Value, []any{installedChange("requests", nil, "1.0")}) {
		t.Fatalf("%+v", r)
	}
	calls := takeCalls(t, dir)
	if strings.Contains(strings.Join(calls, "\n"), "-m venv") || !slices.Contains(calls, "python3 -m pip --disable-pip-version-check --no-input install requests") {
		t.Fatal(calls)
	}
	if in := p.TrackedInputs(); len(in) != 1 || in[0].Path != "pip_package in the system's Python" {
		t.Fatal(in)
	}
	if state, err := p.ReadState(ctx, nil); state != "requests 1.0\n" || err != nil {
		t.Fatalf("%q, %v", state, err)
	}
	if r := (Runtime{}).PipPackage(ctx, p, value.Scope{}); r.Change != "unchanged" {
		t.Fatalf("%+v", r)
	}
}

// In a dryrun, a requirements file that an earlier node task would change,
// such as a git checkout that holds it, is not checked.
func TestPipPackageRequirementsFileThatAnEarlierNodeTaskWouldChange(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	_, p := pipFixture(t)
	checkout := t.TempDir()
	p.Packages, p.RequirementsFileOnNode = nil, filepath.Join(checkout, "requirements.txt")
	writeState(t, p.RequirementsFileOnNode, "gunicorn==23.0.0\n")
	if r := noMutation(t).PipPackage(ctx, p, value.Scope{}); r.Status != "succeeded" || r.Change != "would_change" {
		t.Fatalf("%+v", r)
	}
	dryrun := noMutation(t)
	dryrun.PreviewPaths = []string{checkout}
	if r := dryrun.PipPackage(ctx, p, value.Scope{}); r.Status != "not_checked" || r.Reason != "an earlier node task would change the requirements file "+p.RequirementsFileOnNode+", so a dryrun cannot tell what pip would install" {
		t.Fatalf("%+v", r)
	}
}

// pip_package reads its tracked input with the task's env, resolved in the
// task's scope, as the task runs pip.
func TestPipPackageReadsItsStateWithTheTasksEnv(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	_, p := pipFixture(t)
	p.Env = map[string]any{"PIP_INDEX_URL": "{{ .vars.index }}"}
	scope := value.Scope{"vars": map[string]any{"index": "https://pypi.example.com/simple"}}
	if r := (Runtime{}).PipPackage(ctx, p, scope); r.Change != "changed" {
		t.Fatalf("%+v", r)
	}
	if state, err := p.ReadState(ctx, scope); state != "gunicorn 23.0.0\n" || err != nil {
		t.Fatalf("%q, %v", state, err)
	}
}

func TestPipPackageRejectedInputs(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		p    PipPackage
		want string
	}{
		{PipPackage{VirtualEnvironment: "/v"}, "exactly one of packages and requirements_file_on_node"},
		{PipPackage{Packages: []InstallerPackage{{Name: "a", Version: newestWhenFirstInstalled}}, RequirementsFileOnNode: "/requirements.txt", VirtualEnvironment: "/v"}, "exactly one of packages"},
		{PipPackage{Packages: []InstallerPackage{{Name: "a", Version: newestWhenFirstInstalled}}}, "exactly one of virtual_environment"},
		{PipPackage{Packages: []InstallerPackage{{Name: "a", Version: newestWhenFirstInstalled}}, VirtualEnvironment: "/v", InstallSystemWide: true}, "exactly one of virtual_environment"},
		{PipPackage{RequirementsFileOnNode: "/requirements.txt", VirtualEnvironment: "/v", State: "absent"}, "state goes with packages"},
		{PipPackage{Packages: []InstallerPackage{{Name: "a", Version: newestWhenFirstInstalled}}, VirtualEnvironment: "/v", State: "latest"}, "state must be present or absent"},
		{PipPackage{Packages: []InstallerPackage{{Name: "a", Version: newestWhenFirstInstalled}, {Name: "a", Version: newestOnEveryRun}}, VirtualEnvironment: "/v"}, "listed twice"},
		{PipPackage{Packages: []InstallerPackage{{Name: "a"}}, VirtualEnvironment: "/v"}, "version is required"},
		{PipPackage{Packages: []InstallerPackage{{Version: newestWhenFirstInstalled}}, VirtualEnvironment: "/v"}, "name is required"},
		{PipPackage{Packages: []InstallerPackage{{Name: "a", Version: "==1.0"}}, State: "absent", VirtualEnvironment: "/v"}, "state: absent takes name only"},
		{PipPackage{Packages: []InstallerPackage{{Name: "a", Version: newestWhenFirstInstalled}}, VirtualEnvironment: "/v"}, "run_as_user_account is required"},
		{PipPackage{RequirementsFileOnNode: "/requirements.txt", VirtualEnvironment: "/v"}, "run_as_user_account is required"},
	} {
		if err := c.p.Prepare(""); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: %v", c.p, err)
		}
	}

	for requirement, accepted := range map[string]bool{"==23.0.0": true, ">=2.32,<3": true, "--index-url=x": false, "==1\nb": false} {
		if err := pipVersionRequirement(requirement); (err == nil) != accepted {
			t.Error(requirement, err)
		}
	}

}

// Upgrading one entry must not upgrade a neighbouring first-install entry
// or an already-satisfied pip version range. One install sees all choices.
func TestPipPackageMixesVersionChoicesInOneCommand(t *testing.T) {
	t.Parallel()
	pythonWithPip(t)
	ctx := testContext(t)
	dir, packageTask := pipFixture(t)
	packageTask.InstallSystemWide, packageTask.VirtualEnvironment = true, ""
	packageTask.Packages = []InstallerPackage{
		{Name: "gunicorn", Version: newestOnEveryRun},
		{Name: "requests", Version: ">=2.0,<3"},
		{Name: "certifi", Version: newestWhenFirstInstalled},
		{Name: "idna", Version: "==3.7"},
	}
	// idna is missing; the installed range and first-install entries stay.
	writeState(t, filepath.Join(dir, "installed"), "gunicorn==22.0.0\nrequests==2.32.0\ncertifi==2024.1.1\n")
	writeState(t, filepath.Join(dir, "gunicorn.newest"), "23.0.0\n")
	if result := (Runtime{}).PipPackage(ctx, packageTask, nil); result.Status != "succeeded" || result.Change != "changed" {
		t.Fatalf("%+v", result)
	}
	if state, _ := packageTask.ReadState(ctx, nil); state != "certifi 2024.1.1\ngunicorn 23.0.0\nidna 3.7\nrequests 2.32.0\n" {
		t.Fatal(state)
	}
	var installs []string
	for _, call := range takeCalls(t, dir) {
		if strings.Contains(call, "--no-input install ") {
			installs = append(installs, call)
		}
	}
	if len(installs) != 1 || !strings.HasSuffix(installs[0], "install --upgrade gunicorn requests==2.32.0 certifi==2024.1.1 idna==3.7") {
		t.Fatal(installs)
	}

	slices.Reverse(packageTask.Packages)
	if result := (Runtime{}).PipPackage(ctx, packageTask, nil); result.Status != "succeeded" || result.Change != "unchanged" {
		t.Fatalf("%+v", result)
	}
}

// The same helper that protects a version rule beside an upgrading entry
// runs with Python and pip's actual version parser.
func TestPipVersionSpecifierUsesPython(t *testing.T) {
	t.Parallel()
	python := pythonWithPip(t)
	for _, test := range []struct{ specifier, version, want string }{
		{">=2.32,<3", "2.32.3", "satisfied"},
		{">=2.32,<3", "3.0.0", "unsatisfied"},
		{"==23.0.0", "23.0.0", "satisfied"},
		{"==23.0.0", "23.0.1", "unsatisfied"},
	} {
		out, err := exec.CommandContext(testContext(t), python, "-c", pipRequirementSatisfied, test.specifier, test.version).CombinedOutput()
		if err != nil || strings.TrimSpace(string(out)) != test.want {
			t.Fatalf("%s with %s: %s, %v", test.specifier, test.version, out, err)
		}
	}
}

func TestPipPackageKeepsAURLInstalledDistribution(t *testing.T) {
	t.Parallel()
	python := pythonWithPip(t)
	// Python recognizes installed distributions through their dist-info
	// metadata. pip freeze reports this one's URL instead of its version.
	sitePackages := t.TempDir()
	metadata := filepath.Join(sitePackages, "mmx_url_fixture-23.0.0.dist-info")
	writeState(t, filepath.Join(metadata, "METADATA"), "Metadata-Version: 2.1\nName: mmx-url-fixture\nVersion: 23.0.0\n")
	writeState(t, filepath.Join(metadata, "direct_url.json"), `{"url":"https://example.com/mmx_url_fixture-23.0.0-py3-none-any.whl","archive_info":{}}`)
	p := PipPackage{
		Packages:          []InstallerPackage{{Name: "mmx-url-fixture", Version: newestWhenFirstInstalled}},
		PythonProgramPath: python, InstallSystemWide: true, RunAsUserAccount: currentInstallerAccount(t),
		Env: map[string]any{"PYTHONPATH": sitePackages, "PIP_NO_INDEX": "1", "PIP_BREAK_SYSTEM_PACKAGES": "1"},
	}
	ctx := testContext(t)
	before, err := p.ReadState(ctx, nil)
	if err != nil || !strings.Contains(before, "mmx-url-fixture @ https://example.com/mmx_url_fixture-23.0.0-py3-none-any.whl") {
		t.Fatalf("%s, %v", before, err)
	}
	result := noMutation(t).PipPackage(ctx, p, nil)
	if result.Status != "succeeded" || result.Change != "unchanged" || !reflect.DeepEqual(result.Value, []any{}) {
		t.Fatalf("%+v", result)
	}
	after, err := p.ReadState(ctx, nil)
	if err != nil || before != after {
		t.Fatalf("dryrun changed installed distributions: %v", err)
	}
}
