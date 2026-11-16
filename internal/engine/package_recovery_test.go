package engine

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"mmx/internal/config"
	"mmx/internal/modules"
	"mmx/internal/protocol"
	"mmx/internal/value"
)

type versionManager struct {
	version string
	names   []string
}

func (m *versionManager) Name() string { return "apt" }

func (m *versionManager) Inspect(_ context.Context, name string) (modules.PackageState, error) {
	m.names = append(m.names, name)
	return modules.PackageState{Name: name, Version: m.version, State: "installed"}, nil
}
func (m *versionManager) Ensure(context.Context, modules.Runtime, modules.Package) protocol.Result {
	panic("hashing must not apply packages")
}
func (m *versionManager) Source(context.Context, modules.Runtime, modules.Repository, value.Scope) protocol.Result {
	panic("hashing must not change sources")
}
func (m *versionManager) PathChanged(string) {}

func TestServiceSoftwareHashUsesPackageManager(t *testing.T) {
	m := &versionManager{version: "1"}
	names := []string{"vendor/Tool"}
	first, err := softwareHash(context.Background(), m, names, nil)
	if err != nil {
		t.Fatal(err)
	}
	m.version = "2"
	next, err := softwareHash(context.Background(), m, names, nil)
	if err != nil || next == first || len(m.names) != 2 || m.names[0] != "vendor/Tool" {
		t.Fatal("service recovery bypassed its package manager", err)
	}
}

type pathManager struct{ changed []string }

func (m *pathManager) Name() string { return "apt" }
func (m *pathManager) Inspect(_ context.Context, name string) (modules.PackageState, error) {
	return modules.PackageState{Name: name, State: "absent"}, nil
}
func (m *pathManager) Ensure(context.Context, modules.Runtime, modules.Package) protocol.Result {
	return protocol.Success("unchanged")
}
func (m *pathManager) Source(context.Context, modules.Runtime, modules.Repository, value.Scope) protocol.Result {
	return protocol.Success("unchanged")
}
func (m *pathManager) PathChanged(path string) { m.changed = append(m.changed, path) }

// Every managed path a task changes reaches the job's package manager, which
// decides whether its indexes read it (apt: /etc/apt/). Previews and
// unchanged paths do not.
func TestChangedPathsReachPackageManager(t *testing.T) {
	dir := t.TempDir()
	m := &pathManager{}
	e := Engine{RuntimeRoot: dir, newPackages: func() modules.PackageManager { return m }}
	source := filepath.Join(dir, "source.list")
	link := filepath.Join(dir, "vendor.list")
	nodeTasks := []config.Task{taskFile(source, "deb https://example.com stable main\n"), {Module: "symlink", ModuleInputs: &modules.Symlink{Path: link, Target: "source.list"}}}
	j := job(nodeTasks...)
	j.Dryrun = true
	requireStatus(t, e.Run(context.Background(), j), "succeeded")
	if len(m.changed) != 0 {
		t.Fatal("a preview reported changed paths", m.changed)
	}
	j.Dryrun = false
	requireStatus(t, e.Run(context.Background(), j), "succeeded")
	if !slices.Equal(m.changed, []string{source, link}) {
		t.Fatal(m.changed)
	}
	requireStatus(t, e.Run(context.Background(), job(nodeTasks[0], config.Task{Module: "symlink", ModuleInputs: &modules.Symlink{Path: link, State: "absent"}})), "succeeded")
	if !slices.Equal(m.changed, []string{source, link, link}) {
		t.Fatal("unchanged node_tasks or removal were reported wrongly", m.changed)
	}
}

// A whole-node upgrade restarts nothing itself. In a patching run, the
// task block after it that tracks an upgraded package restarts its service
// once; the next patching run finds nothing to upgrade and restarts nothing.
func TestWholeNodeUpgradeRestartsTrackingServiceOnce(t *testing.T) {
	f := newServiceFixture(t)
	put(t, filepath.Join(f.dir, "package-demo"), "1\tinstalled\n")
	aptGet := filepath.Join(f.dir, "bin/apt-get")
	writeProgramFileThroughShellChild(t, aptGet, `#!/bin/sh
case " $* " in
*" update "*) ;;
*" -s full-upgrade "*) grep -q '^1' "$MMX_TEST_SERVICE/package-demo" && echo 'Inst demo [1] (2 Test:1/stable [all])'; exit 0 ;;
*" full-upgrade "*) printf '2\tinstalled\n' > "$MMX_TEST_SERVICE/package-demo" ;;
*) exit 2 ;;
esac
`)
	op := f.op("A")
	op.TaskBlock.Changes = append(op.TaskBlock.Changes, config.Task{Module: "apt_package", ModuleInputs: &modules.Package{Manager: "apt", Packages: []modules.PackageEntry{{Name: "demo", Version: "newest_when_first_installed"}}}})
	f.run(t, op, "succeeded")
	upgrade := config.Task{Module: "apt_package", ModuleInputs: &modules.Package{Manager: "apt", UpgradeAll: true}}
	r := f.e.Run(context.Background(), job(upgrade))
	requireStatus(t, r, "succeeded")
	if r.Results[0].Change != "changed" || read(t, filepath.Join(f.dir, "package-demo")) != "2\tinstalled\n" {
		t.Fatal(r.Results)
	}
	f.assert(t, "A", "")
	requireStatus(t, f.e.Run(context.Background(), job(upgrade, op)), "succeeded")
	f.assert(t, "A", "restart\n")
	r = f.e.Run(context.Background(), job(upgrade, op))
	requireStatus(t, r, "succeeded")
	if r.Change != "unchanged" {
		t.Fatal(r.Results)
	}
	f.assert(t, "A", "restart\n")
}

// In dryrun, a block that upgrades a tracked package predicts the restart from
// the version apt's simulation names; a version pattern that the installed
// version already matches predicts none.
func TestDryrunPredictsRestartFromPackageResults(t *testing.T) {
	f := newServiceFixture(t)
	put(t, filepath.Join(f.dir, "package-demo"), "1\tinstalled\n")
	aptGet := filepath.Join(f.dir, "bin/apt-get")
	writeProgramFileThroughShellChild(t, aptGet, "#!/bin/sh\ncase \" $* \" in *\" install \"*) echo 'Inst demo [1] (2 Test:1/stable [all])';; esac\n")
	block := func(p modules.Package) config.Task {
		op := f.op("A")
		op.TaskBlock.Changes = append(op.TaskBlock.Changes, config.Task{Module: "apt_package", ModuleInputs: &p})
		return op
	}
	f.run(t, block(modules.Package{Manager: "apt", Packages: []modules.PackageEntry{{Name: "demo", Version: "newest_when_first_installed"}}}), "succeeded")
	for _, tc := range []struct {
		p       modules.Package
		restart string
	}{
		{modules.Package{Manager: "apt", Packages: []modules.PackageEntry{{Name: "demo", Version: "newest_on_every_run"}}}, "would_change"},
		{modules.Package{Manager: "apt", Packages: []modules.PackageEntry{{Name: "demo", Version: "1*"}}}, "unchanged"},
	} {
		j := job(block(tc.p))
		j.Dryrun = true
		r := f.e.Run(context.Background(), j)
		requireStatus(t, r, "succeeded")
		for _, result := range r.Results {
			if result.Task == "server/activate/task_1" && result.Change != tc.restart {
				t.Fatal(tc.p, result.Change, r.Results)
			}
		}
	}
	f.assert(t, "A", "")
}
