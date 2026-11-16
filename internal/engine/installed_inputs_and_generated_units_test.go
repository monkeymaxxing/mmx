package engine

import (
	"context"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"mmx/internal/config"
	"mmx/internal/modules"
)

// What an installer installed restarts a running service of its node task
// block, also when the service task says refresh: reload: the running
// process has the old software until it starts again.
func TestAChangedInstallationRestartsAService(t *testing.T) {
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	f := newServiceFixture(t)
	installed := filepath.Join(f.dir, "installed")
	python := filepath.Join(f.dir, "python3")
	writeProgramFileThroughShellChild(t, python, `#!/bin/sh
if [ "$1" = -m ] && [ "$2" = venv ]; then mkdir -p "$3/bin"; cp "$0" "$3/bin/python"; exit 0; fi
shift 4
touch `+installed+`
case "$1" in
--version) echo "pip 23.0.1 from /usr/lib/python3/dist-packages/pip (python 3.11)";;
freeze) cat `+installed+`;;
install)
  shift
  for r in "$@"; do
    case "$r" in --*|-) continue;; esac
    grep -v "^${r%%==*}==" `+installed+` > `+installed+`.kept || true; mv `+installed+`.kept `+installed+`; echo "$r" >> `+installed+`
  done;;
esac
`)
	venv := filepath.Join(f.dir, "venv")
	op := func(requirement string) config.Task {
		return config.Task{TaskBlock: &config.TaskBlock{ID: "myapp",
			Changes:  []config.Task{{Module: "pip_package", ModuleInputs: &modules.PipPackage{RunAsUserAccount: account.Username, Packages: []modules.InstallerPackage{{Name: "gunicorn", Version: strings.TrimPrefix(requirement, "gunicorn")}}, VirtualEnvironment: venv, PythonProgramPath: python}}},
			Activate: []config.Task{{Module: "systemd_service", ModuleInputs: &modules.Service{State: "running", Name: "demo", Refresh: "reload"}}}}}
	}
	// The first installation differs from the baseline, which had no
	// virtual environment.
	f.run(t, op("gunicorn==22.0.0"), "succeeded")
	f.assert(t, "A", "restart\n")
	f.run(t, op("gunicorn==22.0.0"), "succeeded")
	f.assert(t, "A", "restart\n")
	r := f.e.Run(context.Background(), job(op("gunicorn==23.0.0")))
	requireStatus(t, r, "succeeded")
	f.assert(t, "A", "restart\nrestart\n")
	if reason := r.Results[1].Reason; reason != "installed software version changed" {
		t.Fatalf("reason %q", reason)
	}
}

// systemd cannot enable or disable a unit that a generator made, such as the
// service of a Quadlet file. A task that says enabled fails with what to do
// instead and names the file the unit comes from, when systemd says it; it
// changes nothing. Without enabled it manages the state.
func TestAGeneratedUnitTakesNoEnabled(t *testing.T) {
	f := newServiceFixture(t)
	put(t, filepath.Join(f.dir, "demo.service/enabled"), "generated\n")
	put(t, filepath.Join(f.dir, "demo.service/active"), "inactive\n")
	for _, enabled := range []bool{true, false} {
		r := f.e.Run(context.Background(), job(config.Task{Module: "systemd_service", ModuleInputs: &modules.Service{State: "running", Name: "demo", StartAtBoot: &enabled}}))
		requireStatus(t, r, "failed")
		got := r.Results[0]
		if got.FailureKind != "config" || !strings.Contains(got.Reason, "unit demo.service is generated from another file, such as a Podman Quadlet file, so systemd can neither enable nor disable it; remove start_at_boot from this task") ||
			!strings.Contains(got.Reason, "WantedBy=multi-user.target") {
			t.Fatalf("%+v", got)
		}
	}
	enabled := true
	for source, want := range map[string]string{
		"/etc/containers/systemd/demo.container": "unit demo.service is generated from the Quadlet file /etc/containers/systemd/demo.container, so systemd can neither enable nor disable it; remove start_at_boot from this task. Whether the unit starts at boot is the [Install] section of that file",
		"/etc/crypttab":                          "unit demo.service is generated from /etc/crypttab, so systemd can neither enable nor disable it; remove start_at_boot from this task",
	} {
		put(t, filepath.Join(f.dir, "demo.service/source"), source)
		r := f.e.Run(context.Background(), job(config.Task{Module: "systemd_service", ModuleInputs: &modules.Service{State: "running", Name: "demo", StartAtBoot: &enabled}}))
		requireStatus(t, r, "failed")
		if got := r.Results[0]; got.FailureKind != "config" || !strings.HasPrefix(got.Reason, want) {
			t.Fatalf("%+v", got)
		}
	}
	f.assert(t, "A", "")
	f.run(t, config.Task{Module: "systemd_service", ModuleInputs: &modules.Service{State: "running", Name: "demo"}}, "succeeded")
	f.assert(t, "A", "start\n")
}
