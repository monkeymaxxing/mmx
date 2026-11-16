package modules

import (
	"context"
	"fmt"
	"mmx/internal/assets"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

type recordingManager struct {
	input  Package
	calls  int
	dryrun bool
}

func (m *recordingManager) Name() string { return "apt" }
func (m *recordingManager) Inspect(context.Context, string) (PackageState, error) {
	return PackageState{}, nil
}
func (m *recordingManager) Ensure(_ context.Context, r Runtime, p Package) protocol.Result {
	m.input, m.dryrun = p, r.Dryrun
	m.calls++
	return protocol.Success("would_change")
}
func (m *recordingManager) Source(context.Context, Runtime, Repository, value.Scope) protocol.Result {
	return protocol.Success("unchanged")
}
func (m *recordingManager) PathChanged(string) {}

// Names and version arrive resolved; native name rules belong to the manager.
func TestPackagePassesInputsWithoutImposingAPTNames(t *testing.T) {
	t.Parallel()
	m := &recordingManager{}
	p := Package{Manager: "apt", Packages: []PackageEntry{{Name: "vendor/Tool", Version: "2.1"}}}
	r := (Runtime{Dryrun: true}).Package(context.Background(), m, p)
	if r.Status != "succeeded" || m.calls != 1 || !m.dryrun || m.input.Packages[0].Name != "vendor/Tool" || m.input.Packages[0].Version != "2.1" {
		t.Fatal(r, m)
	}
	p.State = "absent"
	if r := (Runtime{}).Package(context.Background(), m, p); r.FailureKind != "config" || m.calls != 1 {
		t.Fatal("dispatched invalid package input", r)
	}
}

// Related packages are checked together, all rules precede the one install
// command, and reversing the entry order does not change the installed versions.
func TestPackageEntriesInstallTogetherAfterAllVersionRules(t *testing.T) {
	for _, managerName := range []string{"apt", "dnf"} {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reverse=%t", managerName, reverse), func(t *testing.T) {
				var manager PackageManager
				var dir string
				if managerName == "apt" {
					dir = aptFixture(t)
					manager = aptIn(dir)
					put(t, filepath.Join(dir, "daemon.available"), "1.2\n2.0\n")
					put(t, filepath.Join(dir, "client.available"), "1.2\n2.0\n")
				} else {
					dir = dnfFixture(t)
					withLockPlugin(t, dir)
					manager = dnfIn(dir)
					put(t, filepath.Join(dir, "daemon.available"), "0:1.2-1\n0:2.0-1\n")
					put(t, filepath.Join(dir, "client.available"), "0:1.2-1\n0:2.0-1\n")
				}
				entries := []PackageEntry{{Name: "daemon", Version: "1.*"}, {Name: "client", Version: "1.*"}}
				if reverse {
					entries[0], entries[1] = entries[1], entries[0]
				}
				packageCommands := 0
				run := Runtime{BeforeMutation: func(resource string) error {
					if resource != "packages" {
						return nil
					}
					packageCommands++
					if managerName == "apt" {
						for _, name := range []string{"daemon", "client"} {
							text := readState(t, aptIn(dir).versionRulePath(name))
							if !strings.Contains(text, "Pin: version 1.*\n") {
								t.Fatalf("%s rule was not written before installation: %s", name, text)
							}
						}
					} else {
						text := readState(t, filepath.Join(dir, "root", versionLockList))
						for _, name := range []string{"daemon", "client"} {
							if !strings.Contains(text, versionLockMark(name)+"\n"+name+"-0:1.*\n") {
								t.Fatalf("%s lock was not written before installation: %s", name, text)
							}
						}
					}
					return nil
				}}
				p := Package{Manager: managerName, Packages: entries}
				if r := manager.Ensure(testContext(t), run, p); r.Status != "succeeded" || r.Change != "changed" {
					t.Fatal(r)
				}
				calls := fixtureCalls(t, dir)
				if packageCommands != 1 || strings.Count(calls, " install ") != 1 {
					t.Fatalf("expected one install transaction: %s", calls)
				}
				want := "1.2"
				if managerName == "dnf" {
					want = "1.2-1"
				}
				for _, entry := range entries {
					state, err := manager.Inspect(testContext(t), entry.Name)
					if err != nil || state.Version != want {
						t.Fatal(state, err)
					}
				}
				before := fixtureCalls(t, dir)
				if r := manager.Ensure(testContext(t), run, p); r.Status != "succeeded" || r.Change != "unchanged" {
					t.Fatal(r)
				}
				if fixtureCalls(t, dir) != before {
					t.Fatal("unchanged rerun changed packages")
				}
			})
		}
	}
}

// A refusal on a later entry must neither write an earlier rule nor delete
// an earlier MMX rule. This holds in previews and runs.
func TestPackageEntryRefusalChangesNoEarlierEntry(t *testing.T) {
	for _, managerName := range []string{"apt", "dnf"} {
		for _, firstVersion := range []string{"1.*", newestWhenFirstInstalled} {
			t.Run(managerName+"/"+firstVersion, func(t *testing.T) {
				var manager PackageManager
				var dir, path, original string
				if managerName == "apt" {
					dir = aptFixture(t)
					manager = aptIn(dir)
					put(t, filepath.Join(dir, "first"), "1.0\tinstalled\n")
					put(t, filepath.Join(dir, "second"), "2.0\tinstalled\n")
					put(t, filepath.Join(dir, "second.available"), "1.0\n")
					path = aptIn(dir).versionRulePath("first")
					original = "original MMX rule\n"
				} else {
					dir = dnfFixture(t)
					withLockPlugin(t, dir)
					manager = dnfIn(dir)
					put(t, filepath.Join(dir, "pkg-first"), "1.0-1\n")
					put(t, filepath.Join(dir, "pkg-second"), "2.0-1\n")
					put(t, filepath.Join(dir, "second.available"), "0:1.0-1\n")
					path = filepath.Join(dir, "root", versionLockList)
					original = versionLockMark("first") + "\nfirst-1.0-1.*\n"
				}
				put(t, path, original)
				p := Package{Manager: managerName, Packages: []PackageEntry{{Name: "first", Version: firstVersion}, {Name: "second", Version: "1.*"}}}
				for _, dryrun := range []bool{true, false} {
					r := manager.Ensure(testContext(t), Runtime{Dryrun: dryrun}, p)
					if r.Status != "failed" || !strings.Contains(r.Reason, "add allow_downgrade: true") {
						t.Fatal(r)
					}
					if readState(t, path) != original {
						t.Fatal("refused task changed an earlier rule")
					}
					if strings.Contains(fixtureCalls(t, dir), " install ") {
						t.Fatal("refused task installed a package")
					}
				}
			})
		}
	}
}

func TestPackageFileVersionMustMatchItsEntryBeforeAnyRuleChanges(t *testing.T) {
	for _, managerName := range []string{"apt", "dnf"} {
		t.Run(managerName, func(t *testing.T) {
			var manager PackageManager
			var dir, version string
			if managerName == "apt" {
				dir = aptFixture(t)
				manager = aptIn(dir)
				version = "1.0"
			} else {
				dir = dnfFixture(t)
				withLockPlugin(t, dir)
				manager = dnfIn(dir)
				version = "1.0-1"
			}
			file := filepath.Join(dir, "package-file")
			put(t, file, "tool\t"+version)
			p := Package{Manager: managerName, Packages: []PackageEntry{{Name: "tool", Version: "9.0", PackageFileSource: "package-file", PackageFile: &assets.Ref{Path: file}}}}
			for _, dryrun := range []bool{true, false} {
				writes := 0
				r := manager.Ensure(testContext(t), Runtime{Dryrun: dryrun, BeforeMutation: func(string) error { writes++; return nil }}, p)
				if r.Status != "failed" || !strings.Contains(r.Reason, "holds version "+version+", not the entry's version 9.0") || writes != 0 {
					t.Fatal(r, writes)
				}
				if strings.Contains(fixtureCalls(t, dir), " install ") {
					t.Fatal("mismatched package file was installed")
				}
			}
		})
	}
}

func TestPackageRemovalRequiresConsentForEveryUnnamedRemoval(t *testing.T) {
	for _, managerName := range []string{"apt", "dnf"} {
		t.Run(managerName, func(t *testing.T) {
			var manager PackageManager
			var dir string
			if managerName == "apt" {
				dir = aptFixture(t)
				manager = aptIn(dir)
				put(t, filepath.Join(dir, "library"), "1\tinstalled\n")
				put(t, filepath.Join(dir, "plan"), "Remv library [1]\nRemv application [1]\n")
				writeStandInProgram(t, filepath.Join(dir, "apt-get"), `#!/bin/sh
if [ "$1" = -s ]; then cat "$MMX_APT_FIXTURE/plan"; exit 0; fi
printf '\tnot-installed\n' > "$MMX_APT_FIXTURE/library"
`)
			} else {
				dir = dnfFixture(t)
				manager = dnfIn(dir)
				put(t, filepath.Join(dir, "pkg-library"), "1\n")
				put(t, filepath.Join(dir, "plan"), "Removing:\n library x86_64 1 @base 1 k\n application x86_64 1 @base 1 k\nTransaction Summary\n")
			}
			p := Package{Manager: managerName, State: "absent", Packages: []PackageEntry{{Name: "library"}}}
			for _, dryrun := range []bool{true, false} {
				writes := 0
				r := manager.Ensure(testContext(t), Runtime{Dryrun: dryrun, BeforeMutation: func(string) error { writes++; return nil }}, p)
				if r.Status != "failed" || r.FailureKind != "config" || !strings.Contains(r.Reason, "also remove application") || !strings.Contains(r.Reason, "add also_remove_packages_that_depend_on_these: true") || writes != 0 {
					t.Fatal(r, writes)
				}
				state, err := manager.Inspect(testContext(t), "library")
				if err != nil || state.State != "installed" {
					t.Fatal(state, err)
				}
			}
			p.AlsoRemovePackagesThatDependOnThese = true
			if r := manager.Ensure(testContext(t), Runtime{Dryrun: true}, p); r.Status != "succeeded" || r.Change != "would_change" {
				t.Fatal(r)
			}
			if r := manager.Ensure(testContext(t), Runtime{}, p); r.Status != "succeeded" || r.Change != "changed" {
				t.Fatal(r)
			}
		})
	}
}

func TestPackageRefusalsNameTheRequiredTaskSettings(t *testing.T) {
	for _, tc := range []struct {
		field, requires string
		change          func(*Package)
	}{
		{"only_security_updates", "upgrade_all_packages_within_their_version_rules: true", func(p *Package) { yes := true; p.OnlySecurityUpdates = &yes }},
		{"also_delete_its_config_files", "state: absent", func(p *Package) { p.Manager = "apt"; p.AlsoDeleteItsConfigFiles = true }},
		{"also_remove_packages_only_it_needed", "state: absent", func(p *Package) { p.AlsoRemovePackagesOnlyItNeeded = true }},
		{"also_remove_packages_that_depend_on_these", "state: absent", func(p *Package) { p.AlsoRemovePackagesThatDependOnThese = true }},
		{"also_remove_packages_no_longer_needed", "state: absent or upgrade_all_packages_within_their_version_rules: true", func(p *Package) { p.Manager = "apt"; p.AlsoRemovePackagesNoLongerNeeded = true }},
		{"allow_package_to_start_its_services", "state: present", func(p *Package) {
			p.Manager = "apt"
			p.State = "absent"
			p.Packages[0].Version = ""
			p.AllowPackageToStartItsServices = true
		}},
		{"enable_dnf_module_stream", "state: present with packages", func(p *Package) {
			p.State = "absent"
			p.Packages[0].Version = ""
			p.EnableDnfModuleStream = "postgresql:16"
		}},
		{"repositories_enabled_only_for_this_task", "state: present or upgrade_all_packages_within_their_version_rules: true, and at least one repository ID", func(p *Package) { p.RepositoriesEnabledOnlyForThisTask = []string{} }},
	} {
		t.Run(tc.field, func(t *testing.T) {
			p := Package{Manager: "dnf", Packages: []PackageEntry{{Name: "tool", Version: newestWhenFirstInstalled}}}
			tc.change(&p)
			err := ValidatePackage(p)
			if err == nil || !strings.Contains(err.Error(), tc.field+" requires "+tc.requires) {
				t.Fatal(err)
			}
		})
	}
}

// A repository package can depend on a file supplied by the same task. The
// native preview must see both entries and the rules that apply at installation.
func TestPackagePreviewIncludesFileDependenciesAndProposedRules(t *testing.T) {
	for _, managerName := range []string{"apt", "dnf"} {
		t.Run(managerName, func(t *testing.T) {
			var manager PackageManager
			var dir, version, file string
			if managerName == "apt" {
				dir = aptFixture(t)
				manager = aptIn(dir)
				version = "2"
				file = filepath.Join(dir, "common.deb")
				put(t, filepath.Join(dir, "plan"), "Inst common (2 Test:1/stable [all])\nInst application (2 Test:1/stable [all])\n")
				writeStandInProgram(t, filepath.Join(dir, "preview-check"), `#!/bin/sh
set -eu
parts=
for arg; do case "$arg" in Dir::Etc::preferencesparts=*) parts=${arg#*=};; esac; done
[ -n "$parts" ] && grep -q 'Pin: version 2$' "$parts/mmx-common.pref"
file=
for arg; do case "$arg" in /*.deb) file=$arg;; esac; done
[ -n "$file" ] && [ "$(cut -f1 "$file")" = common ] || { echo 'application depends on common, supplied by its package file' >&2; exit 100; }
case " $* " in *" application "*) :;; *) exit 100;; esac
`)
			} else {
				dir = dnfFixture(t)
				manager = dnfIn(dir)
				withLockPlugin(t, dir)
				version = "2-1"
				file = filepath.Join(dir, "common.rpm")
				put(t, filepath.Join(dir, "application.available"), "2-1\n")
				put(t, filepath.Join(dir, "plan"), "Installing:\n common x86_64 2-1 @commandline 1 k\n application x86_64 2-1 appstream 1 k\nTransaction Summary\n")
				writeStandInProgram(t, filepath.Join(dir, "preview-check"), `#!/bin/sh
set -eu
plugins=
for arg; do case "$arg" in --setopt=pluginconfpath=*) plugins=${arg#*=}; plugins=${plugins#*=};; esac; done
list=$(sed -n 's/^locklist=//p' "$plugins/versionlock.conf")
grep -q '^common-0:2-1$' "$list"
file=
for arg; do case "$arg" in /*.rpm) file=$arg;; esac; done
[ -n "$file" ] && [ "$(cut -f1 "$file")" = common ] || { echo 'Error: application depends on common, supplied by its package file' >&2; exit 1; }
case " $* " in *" application "*) :;; *) exit 1;; esac
`)
			}
			put(t, file, "common\t"+version)
			p := Package{Manager: managerName, Packages: []PackageEntry{
				{Name: "application", Version: newestOnEveryRun},
				{Name: "common", Version: version, PackageFileSource: filepath.Base(file), PackageFile: &assets.Ref{Path: file}},
			}}
			for _, dryrun := range []bool{true, false} {
				result := manager.Ensure(testContext(t), Runtime{Dryrun: dryrun}, p)
				if result.Status != "succeeded" {
					t.Fatal(result)
				}
			}
			for _, name := range []string{"common", "application"} {
				state, err := manager.Inspect(testContext(t), name)
				if err != nil || state.Version != version {
					t.Fatal(state, err)
				}
			}
		})
	}
}

// A native transaction may also upgrade dependencies that the task does not
// name. Both preview and apply report every changed package, not just entries.
func TestPackageValueIncludesDependenciesOutsideTheTask(t *testing.T) {
	for _, managerName := range []string{"apt", "dnf"} {
		t.Run(managerName, func(t *testing.T) {
			var manager PackageManager
			var dir, before, after string
			if managerName == "apt" {
				dir = aptFixture(t)
				manager = aptIn(dir)
				before, after = "1", "2"
				for _, name := range []string{"application", "dependency"} {
					put(t, filepath.Join(dir, name), before+"\tinstalled\n")
				}
				put(t, filepath.Join(dir, "application.available"), after+"\n")
				put(t, filepath.Join(dir, "plan"), "Inst application [1] (2 Test:1/stable [all])\nInst dependency [1] (2 Test:1/stable [all])\n")
				writeStandInProgram(t, filepath.Join(dir, "after-install"), "#!/bin/sh\nprintf '2\\tinstalled\\n' > \"$MMX_APT_FIXTURE/dependency\"\n")
			} else {
				dir = dnfFixture(t)
				manager = dnfIn(dir)
				before, after = "1-1", "2-1"
				withLockPlugin(t, dir)
				for _, name := range []string{"application", "dependency"} {
					put(t, filepath.Join(dir, "pkg-"+name), before+"\n")
				}
				put(t, filepath.Join(dir, "application.available"), after+"\n")
				put(t, filepath.Join(dir, "plan"), "Upgrading:\n application x86_64 2-1 appstream 1 k\n dependency x86_64 2-1 appstream 1 k\nTransaction Summary\n")
				writeStandInProgram(t, filepath.Join(dir, "after-install"), "#!/bin/sh\nprintf '2-1\\n' > \"$MMX_DNF_FIXTURE/pkg-dependency\"\n")
			}
			p := Package{Manager: managerName, Packages: []PackageEntry{{Name: "application", Version: after}}}
			expected := []any{
				map[string]any{"name": "application", "from": before, "to": after},
				map[string]any{"name": "dependency", "from": before, "to": after},
			}
			for _, dryrun := range []bool{true, false} {
				result := manager.Ensure(testContext(t), Runtime{Dryrun: dryrun}, p)
				change := "changed"
				installed := after
				if dryrun {
					change, installed = "would_change", before
				}
				if result.Status != "succeeded" || result.Change != change || !reflect.DeepEqual(result.Value, expected) {
					t.Fatal(result)
				}
				for _, name := range []string{"application", "dependency"} {
					state, err := manager.Inspect(testContext(t), name)
					if err != nil || state.Version != installed {
						t.Fatal(name, state, err)
					}
				}
			}
		})
	}
}

// apt_package and apt_repository on a node that uses another package manager
// fail with one sentence that names the module, the node's package manager,
// and the fact; no package module chooses a tool by distribution.
func TestAptModulesFailOnANodeWithAnotherPackageManager(t *testing.T) {
	t.Parallel()
	for _, manager := range []string{"dnf", "pacman", "zypper", ""} {
		node := NewPackageManager(manager)
		p := Package{Manager: "apt", Packages: []PackageEntry{{Name: "nginx", Version: "newest_when_first_installed"}}}
		r := (Runtime{}).Package(context.Background(), node, p)
		if r.Status != "failed" || !strings.Contains(r.Reason, "apt_package installs with apt") || strings.Contains(r.Reason, "\n") {
			t.Errorf("%q: %+v", manager, r)
		}
		if manager != "" && !strings.Contains(r.Reason, "package manager is "+manager) {
			t.Errorf("%q: the reason does not name the node's package manager: %s", manager, r.Reason)
		}
		repository := Repository{Manager: "apt", Name: "example", URIs: []string{"https://example.com/apt"}, Suites: []string{"stable"}, Components: []string{"main"}, SigningKeyURL: "https://example.com/key.asc", DangerouslySkipChecksum: true}
		if r := (Runtime{}).Repository(context.Background(), node, repository, nil); r.Status != "failed" || !strings.Contains(r.Reason, "apt_repository installs with apt") {
			t.Errorf("%q: %+v", manager, r)
		}
	}
}

// A package task on the wrong package manager fails at once: the node's
// package manager does not change between tries.
func TestPackageOnTheWrongPackageManagerStopsRetries(t *testing.T) {
	apt := Package{Manager: "apt", Packages: []PackageEntry{{Name: "nginx", Version: newestWhenFirstInstalled}}}
	for name, manager := range map[string]PackageManager{"dnf": NewPackageManager("dnf"), "pacman": NewPackageManager("pacman"), "unknown": NewPackageManager("")} {
		r := (Runtime{}).Package(testContext(t), manager, apt)
		if r.Status != "failed" || r.FailureKind != "config" || r.Reason == "" {
			t.Errorf("%s: %+v", name, r)
		}
	}
	repository := Repository{Manager: "apt"}
	if r := (Runtime{}).Repository(testContext(t), NewPackageManager("dnf"), repository, value.Scope{}); r.FailureKind != "config" {
		t.Errorf("repository: %+v", r)
	}
	// A node with no module for its package manager cannot change between tries either.
	if r := NewPackageManager("pacman").Ensure(testContext(t), Runtime{}, apt); r.FailureKind != "config" {
		t.Errorf("Ensure: %+v", r)
	}
}
