package engine

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mmx/internal/config"
	"mmx/internal/modules"
)

func TestDryrunCandidateValidation(t *testing.T) {
	for _, tt := range []struct {
		name, inspection, content, validation, status string
		missingDir                                    bool
	}{
		{"valid", "test \"$(cat \"$CANDIDATE\")\" = proposed", "proposed", "succeeded", "succeeded", false},
		{"invalid", "echo bad-config >&2; exit 1", "proposed", "failed", "failed", false},
		{"unchanged-invalid", "exit 1", "live", "failed", "failed", false},
		{"no-inspection", "", "proposed", "skipped", "succeeded", false},
		{"missing-command", "mmx-nonexistent-validator", "proposed", "skipped", "succeeded", false},
		{"missing-directory", "exit 1", "proposed", "skipped", "succeeded", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config")
			put(t, path, "live")
			before, _ := os.Stat(path)
			if tt.missingDir {
				path = filepath.Join(dir, "missing", "config")
			}
			marker := filepath.Join(dir, "apply-ran")
			task := taskFile(path, tt.content)
			config.InputsOf[modules.File](task).CheckNewFile = &modules.Check{Shell: &modules.Shell{Script: "touch \"$MARKER\"", Dryrun: tt.inspection, Env: map[string]any{"CANDIDATE": "{{ .new_file.path }}", "MARKER": marker}}}
			if tt.content == "live" {
				config.InputsOf[modules.File](task).Mode = "0600"
			}
			j := job(task)
			if tt.missingDir {
				// An earlier node task would create the directory, so the
				// dryrun previews the file as new.
				j = job(config.Task{Module: "directory", ModuleInputs: &modules.Directory{Path: filepath.Dir(path)}}, task)
			}
			j.Dryrun = true
			e := Engine{BeforeMutation: func(string) error { t.Fatal("dryrun attempted managed mutation"); return nil }}
			r := e.Run(context.Background(), j)
			requireStatus(t, r, tt.status)
			got := r.Results[len(r.Results)-1]
			change := "would_change"
			if tt.content == "live" {
				change = "unchanged"
			}
			if got.Change != change || got.CheckNewFile == nil || got.CheckNewFile.Status != tt.validation {
				t.Fatalf("result: %+v, validation: %+v", got, got.CheckNewFile)
			}
			if tt.name == "invalid" && !strings.Contains(got.CheckNewFile.Stderr, "bad-config") {
				t.Fatal("lost validation output")
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatal("apply validator ran in dryrun")
			}
			if read(t, filepath.Join(dir, "config")) != "live" {
				t.Fatal("dryrun changed live content")
			}
			after, _ := os.Stat(filepath.Join(dir, "config"))
			if !os.SameFile(before, after) || before.Mode() != after.Mode() || !before.ModTime().Equal(after.ModTime()) {
				t.Fatal("dryrun changed live metadata")
			}
			files, err := os.ReadDir(dir)
			if err != nil || len(files) != 1 {
				t.Fatal("candidate or destination directory left behind", files, err)
			}
		})
	}
}

func packageRemovalFixture(t *testing.T) *serviceFixture {
	t.Helper()
	f := newServiceFixture(t)
	// A simulation (-s) changes nothing and lists what apt would remove.
	script := `#!/bin/sh
set -eu
if [ "$1" = -s ]; then
 [ "$2" = remove ] && [ "$3" = -y ] && [ "$4" = -- ]
 shift 4
 for argument; do echo "Remv ${argument%-} [1.0]"; done
 exit 0
fi
printf '%s\n' "$*" >> "$MMX_TEST_SERVICE/apt-calls"
[ "$1" = -o ] && [ "$2" = DPkg::Lock::Timeout=900 ] && [ "$3" = remove ] && [ "$4" = -y ] && [ "$5" = -- ]
shift 5
for argument; do
 name=${argument%-}
 printf '1.0\tconfig-files\n' > "$MMX_TEST_SERVICE/package-$name"
done
`
	writeProgramFileThroughShellChild(t, filepath.Join(f.dir, "bin/apt-get"), script)
	return f
}

func TestPackageRemovalAndDryrun(t *testing.T) {
	f := packageRemovalFixture(t)
	for _, name := range []string{"demo", "addon+", "addon-"} {
		put(t, filepath.Join(f.dir, "package-"+name), "1.0\tinstalled\n")
	}
	put(t, filepath.Join(f.dir, "package-partial"), "1.0\tunpacked\n")
	put(t, filepath.Join(f.dir, "package-residual"), "1.0\tconfig-files\n")
	p := modules.Package{Manager: "apt", Packages: []modules.PackageEntry{{Name: "demo"}, {Name: "addon+"}, {Name: "addon-"}, {Name: "partial"}, {Name: "residual"}, {Name: "missing"}}, State: "absent"}
	j := job(config.Task{Module: "apt_package", ModuleInputs: &p})
	j.Dryrun = true
	r := f.e.Run(context.Background(), j)
	requireStatus(t, r, "succeeded")
	removed := []any{map[string]any{"name": "demo", "from": "1.0"}, map[string]any{"name": "addon+", "from": "1.0"},
		map[string]any{"name": "addon-", "from": "1.0"}, map[string]any{"name": "partial", "from": "1.0"}}
	if r.Results[0].Change != "would_change" || !reflect.DeepEqual(r.Results[0].Value, removed) {
		t.Fatal(r)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "apt-calls")); !os.IsNotExist(err) {
		t.Fatal("dryrun ran apt")
	}
	j.Dryrun = false
	r = f.e.Run(context.Background(), j)
	requireStatus(t, r, "succeeded")
	if r.Results[0].Change != "changed" {
		t.Fatal(r)
	}
	want := "-o DPkg::Lock::Timeout=900 remove -y -- demo- addon+- addon-- partial-\n"
	if read(t, filepath.Join(f.dir, "apt-calls")) != want {
		t.Fatal("unexpected apt action")
	}
	r = f.e.Run(context.Background(), j)
	requireStatus(t, r, "succeeded")
	if r.Results[0].Change != "unchanged" || read(t, filepath.Join(f.dir, "apt-calls")) != want {
		t.Fatal("absence was not a no-op")
	}
	p.Packages[0].Version = "1.0"
	requireStatus(t, f.e.Run(context.Background(), j), "failed")
}

func TestPackageRemovalRequiresVerifiedAbsence(t *testing.T) {
	f := packageRemovalFixture(t)
	put(t, filepath.Join(f.dir, "package-demo"), "1.0\tinstalled\n")
	writeProgramFileThroughShellChild(t, filepath.Join(f.dir, "bin/apt-get"), "#!/bin/sh\nexit 0\n")
	r := f.e.Run(context.Background(), job(config.Task{Module: "apt_package", ModuleInputs: &modules.Package{Manager: "apt", Packages: []modules.PackageEntry{{Name: "demo"}}, State: "absent"}}))
	requireStatus(t, r, "failed")
	if !strings.Contains(r.Error, "was not removed") {
		t.Fatal(r.Error)
	}
}

func TestRemovedPackageTriggersOneRestart(t *testing.T) {
	f := packageRemovalFixture(t)
	put(t, filepath.Join(f.dir, "package-addon"), "1.0\tinstalled\n")
	op := config.Task{TaskBlock: &config.TaskBlock{ID: "server", Changes: []config.Task{{Module: "apt_package", ModuleInputs: &modules.Package{Manager: "apt", Packages: []modules.PackageEntry{{Name: "addon"}}, State: "absent"}}}, Activate: []config.Task{{Module: "systemd_service", ModuleInputs: &modules.Service{State: "running", Name: "demo", Refresh: "restart"}}}}}
	j := job(op)
	j.Dryrun = true
	r := f.e.Run(context.Background(), j)
	requireStatus(t, r, "succeeded")
	if r.Results[len(r.Results)-1].Change != "would_change" {
		t.Fatal("removal did not predict restart", r.Results)
	}
	f.assert(t, "A", "")
	if _, err := os.Stat(f.e.servicePath("demo.service")); !os.IsNotExist(err) {
		t.Fatal("dryrun wrote service hashes")
	}
	f.run(t, op, "succeeded")
	f.assert(t, "A", "restart\n")
	f.run(t, op, "succeeded")
	f.assert(t, "A", "restart\n")
	// Residual conffiles and a missing package describe the same absent software.
	if err := os.Remove(filepath.Join(f.dir, "package-addon")); err != nil {
		t.Fatal(err)
	}
	f.run(t, op, "succeeded")
	f.assert(t, "A", "restart\n")
}

func TestMetadataOnlyFileDoesNotReloadService(t *testing.T) {
	f := newServiceFixture(t)
	op := f.op("A")
	f.run(t, op, "succeeded")
	f.assert(t, "A", "")
	config.InputsOf[modules.File](op.TaskBlock.Changes[0]).Content = nil
	config.InputsOf[modules.File](op.TaskBlock.Changes[0]).Mode = "0640"
	j := job(op)
	j.Dryrun = true
	r := f.e.Run(context.Background(), j)
	requireStatus(t, r, "succeeded")
	for _, result := range r.Results {
		if strings.Contains(result.Task, "/activate/") && result.Change != "unchanged" {
			t.Fatal("metadata-only preview requested a reload", result)
		}
	}
	f.run(t, op, "succeeded")
	f.assert(t, "A", "")
	info, err := os.Stat(f.path)
	if err != nil || info.Mode().Perm() != 0640 {
		t.Fatal(info, err)
	}
}
