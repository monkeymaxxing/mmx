package modules

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// A stand-in update-alternatives keeps each name in $dir/admin/<name>: a
// "mode auto|manual" line, a "current <path>" line, and one "choice <path>
// <priority>" line for each choice. --display prints dpkg's form, or with a
// file "redhat" the Red Hat family's. A file "fail" makes --set fail.
const fakeUpdateAlternatives = `
best() { awk '$1 == "choice" && ($3 + 0 > top || best == "") {top = $3 + 0; best = $2} END {print best}' "$1"; }
state() { printf 'mode %s\ncurrent %s\n' "$2" "$3" > "$1.new"; grep '^choice ' "$1" >> "$1.new"; mv "$1.new" "$1"; }
case "$1" in
--display)
  f="$dir/admin/$2"
  [ -e "$f" ] || { echo "update-alternatives: error: no alternatives for $2" >&2; exit 2; }
  mode=$(sed -n 's/^mode //p' "$f"); current=$(sed -n 's/^current //p' "$f")
  if [ -e "$dir/redhat" ]; then
    echo "$2 - status is $mode."
    echo " link currently points to $current"
    awk '$1 == "choice" {print $2 " - family x priority " $3}' "$f"
    echo "Current \` + "`" + `best' version is $(best "$f")."
  else
    echo "$2 - $mode mode"
    echo "  link best version is $(best "$f")"
    echo "  link currently points to $current"
    echo "  link $2 is /usr/bin/$2"
    awk '$1 == "choice" {print $2 " - priority " $3}' "$f"
  fi;;
--set)
  f="$dir/admin/$2"
  [ ! -e "$dir/fail" ] || { echo "update-alternatives: error: injected failure" >&2; exit 2; }
  grep -q "^choice $3 " "$f" || { echo "update-alternatives: error: alternative $3 for $2 not registered; not setting" >&2; exit 2; }
  state "$f" manual "$3";;
--auto)
  f="$dir/admin/$2"
  state "$f" auto "$(best "$f")";;
--install)
  f="$dir/admin/$3"
  [ -e "$f" ] || printf 'mode auto\ncurrent %s\n' "$4" > "$f"
  echo "choice $4 $5" >> "$f"
  [ "$(sed -n 's/^mode //p' "$f")" = manual ] || state "$f" auto "$(best "$f")";;
esac`

func alternativesFixture(t *testing.T, redHat bool) string {
	t.Helper()
	dir := standInPrograms(t, map[string]string{"update-alternatives": fakeUpdateAlternatives})
	writeState(t, filepath.Join(dir, "admin", "editor"), "mode auto\ncurrent /usr/bin/nano\nchoice /usr/bin/nano 40\nchoice /usr/bin/vim.basic 30\n")
	if redHat {
		writeState(t, filepath.Join(dir, "redhat"), "")
	}
	previous := alternativesDirectories
	alternativesDirectories = map[string]string{"apt": filepath.Join(dir, "admin"), "dnf": filepath.Join(dir, "admin")}
	t.Cleanup(func() { alternativesDirectories = previous })
	return dir
}

func storedAlternatives(t *testing.T, dir, name string) string {
	t.Helper()
	lines := strings.Split(readState(t, filepath.Join(dir, "admin", name)), "\n")
	return strings.Join(lines[:2], "\n")
}

func TestUpdateAlternativesSetsAChoiceAndAutomaticMode(t *testing.T) {
	ctx := testContext(t)
	for manager, redHat := range map[string]bool{"apt": false, "dnf": true} {
		dir := alternativesFixture(t, redHat)
		a := UpdateAlternatives{Name: "editor", ProgramPath: "/usr/bin/vim.basic"}
		want := map[string]any{"program_path_before": "/usr/bin/nano", "automatic_before": true, "program_path_after": "/usr/bin/vim.basic", "automatic_after": false}
		preview := noMutation(t).UpdateAlternatives(ctx, a, manager)
		if preview.Status != "succeeded" || preview.Change != "would_change" || !reflect.DeepEqual(preview.Value, want) {
			t.Fatalf("%s %+v", manager, preview)
		}
		if calls := changing(takeCalls(t, dir), "update-alternatives --display"); calls != nil {
			t.Fatal(manager, calls)
		}
		made := (Runtime{}).UpdateAlternatives(ctx, a, manager)
		if made.Status != "succeeded" || made.Change != "changed" || !reflect.DeepEqual(made.Value, want) || storedAlternatives(t, dir, "editor") != "mode manual\ncurrent /usr/bin/vim.basic" {
			t.Fatalf("%s %+v", manager, made)
		}
		if calls := changing(takeCalls(t, dir), "update-alternatives --display"); !reflect.DeepEqual(calls, []string{"update-alternatives --set editor /usr/bin/vim.basic"}) {
			t.Fatal(manager, calls)
		}
		for _, run := range []Runtime{{}, noMutation(t)} {
			if r := run.UpdateAlternatives(ctx, a, manager); r.Status != "succeeded" || r.Change != "unchanged" || r.Value != nil {
				t.Fatalf("%s %+v", manager, r)
			}
		}
		// automatic: true gives the choice back to the highest priority.
		automatic := UpdateAlternatives{Name: "editor", Automatic: true}
		back := map[string]any{"program_path_before": "/usr/bin/vim.basic", "automatic_before": false, "program_path_after": "/usr/bin/nano", "automatic_after": true}
		if r := noMutation(t).UpdateAlternatives(ctx, automatic, manager); r.Change != "would_change" || !reflect.DeepEqual(r.Value, back) {
			t.Fatalf("%s %+v", manager, r)
		}
		if r := (Runtime{}).UpdateAlternatives(ctx, automatic, manager); r.Change != "changed" || !reflect.DeepEqual(r.Value, back) || storedAlternatives(t, dir, "editor") != "mode auto\ncurrent /usr/bin/nano" {
			t.Fatalf("%s %+v", manager, r)
		}
		if r := (Runtime{}).UpdateAlternatives(ctx, automatic, manager); r.Change != "unchanged" {
			t.Fatalf("%s %+v", manager, r)
		}
		// The program the name runs in automatic mode is set in manual mode,
		// so a package upgrade does not switch it back.
		takeCalls(t, dir)
		if r := (Runtime{}).UpdateAlternatives(ctx, UpdateAlternatives{Name: "editor", ProgramPath: "/usr/bin/nano"}, manager); r.Change != "changed" || storedAlternatives(t, dir, "editor") != "mode manual\ncurrent /usr/bin/nano" {
			t.Fatalf("%s %+v", manager, r)
		}
	}
}

func TestUpdateAlternativesRegistersAChoiceWithLinkAndPriority(t *testing.T) {
	ctx := testContext(t)
	dir := alternativesFixture(t, false)
	priority := 50
	a := UpdateAlternatives{Name: "mmx-tool", ProgramPath: "/usr/bin/true", GenericCommandPath: "/usr/local/bin/mmx-tool", PriorityWhenRegistered: &priority}
	preview := noMutation(t).UpdateAlternatives(ctx, a, "apt")
	if preview.Change != "would_change" || !reflect.DeepEqual(preview.Value, map[string]any{"program_path_before": "", "program_path_after": "/usr/bin/true", "automatic_after": false}) {
		t.Fatalf("%+v", preview)
	}
	made := (Runtime{}).UpdateAlternatives(ctx, a, "apt")
	if made.Status != "succeeded" || made.Change != "changed" || storedAlternatives(t, dir, "mmx-tool") != "mode manual\ncurrent /usr/bin/true" {
		t.Fatalf("%+v", made)
	}
	if calls := changing(takeCalls(t, dir), "update-alternatives --display"); !reflect.DeepEqual(calls, []string{
		"update-alternatives --install /usr/local/bin/mmx-tool mmx-tool /usr/bin/true 50", "update-alternatives --set mmx-tool /usr/bin/true"}) {
		t.Fatal(calls)
	}
	if r := (Runtime{}).UpdateAlternatives(ctx, a, "apt"); r.Change != "unchanged" {
		t.Fatalf("%+v", r)
	}
	// Registration worked and setting the choice failed: failed, changed; the
	// next try only sets it.
	writeState(t, filepath.Join(dir, "fail"), "")
	second := UpdateAlternatives{Name: "mmx-tool", ProgramPath: "/usr/bin/false", GenericCommandPath: "/usr/local/bin/mmx-tool", PriorityWhenRegistered: &priority}
	r := (Runtime{}).UpdateAlternatives(ctx, second, "apt")
	if r.Status != "failed" || r.Change != "changed" || !strings.Contains(r.Reason, "injected failure") {
		t.Fatalf("%+v", r)
	}
	// The program was never set, so the value names no program_path_after.
	if r.Value != nil {
		t.Fatalf("a failed run reports a change that did not happen: %v", r.Value)
	}
	os.Remove(filepath.Join(dir, "fail"))
	takeCalls(t, dir)
	if r := (Runtime{}).UpdateAlternatives(ctx, second, "apt"); r.Change != "changed" || storedAlternatives(t, dir, "mmx-tool") != "mode manual\ncurrent /usr/bin/false" {
		t.Fatalf("%+v", r)
	}
	if calls := changing(takeCalls(t, dir), "update-alternatives --display"); !reflect.DeepEqual(calls, []string{"update-alternatives --set mmx-tool /usr/bin/false"}) {
		t.Fatal(calls)
	}
}

func TestUpdateAlternativesFailures(t *testing.T) {
	ctx := testContext(t)
	dir := alternativesFixture(t, true)
	// A path that is not a registered choice and has no link: an earlier
	// package task may register it, so dryrun cannot check, and a run fails
	// listing the choices.
	for run, status := range map[*Runtime]string{{Dryrun: true}: "not_checked", {}: "failed"} {
		r := run.UpdateAlternatives(ctx, UpdateAlternatives{Name: "editor", ProgramPath: "/usr/bin/emacs"}, "dnf")
		if r.Status != status || !strings.Contains(r.Reason, "/usr/bin/emacs is not a registered choice for editor; the registered choices are: /usr/bin/nano, /usr/bin/vim.basic") {
			t.Fatalf("%+v", r)
		}
		r = run.UpdateAlternatives(ctx, UpdateAlternatives{Name: "java", Automatic: true}, "dnf")
		if r.Status != status || !strings.Contains(r.Reason, "no choices for java") {
			t.Fatalf("%+v", r)
		}
	}
	if calls := changing(takeCalls(t, dir), "update-alternatives --display"); calls != nil {
		t.Fatal(calls)
	}
	// A node whose package manager MMX has no module for fails, in a dryrun too.
	for _, run := range []Runtime{{}, {Dryrun: true}} {
		r := run.UpdateAlternatives(ctx, UpdateAlternatives{Name: "editor", ProgramPath: "/usr/bin/vim"}, "zypper")
		if r.Status != "failed" || !strings.Contains(r.Reason, "nodes with apt or dnf; the node's package manager is \"zypper\"") {
			t.Fatalf("%+v", r)
		}
	}
	// A failed --set changes nothing.
	writeState(t, filepath.Join(dir, "fail"), "")
	if r := (Runtime{}).UpdateAlternatives(ctx, UpdateAlternatives{Name: "editor", ProgramPath: "/usr/bin/vim.basic"}, "dnf"); r.Status != "failed" || r.Change != "unknown" || r.Value != nil {
		t.Fatalf("%+v", r)
	}
	setWorkerEnvironment(t, "PATH", t.TempDir())
	for _, manager := range []string{"apt"} {
		for run, status := range map[*Runtime]string{{Dryrun: true}: "not_checked", {}: "failed"} {
			if r := run.UpdateAlternatives(ctx, UpdateAlternatives{Name: "editor", ProgramPath: "/usr/bin/vim"}, manager); r.Status != status || !strings.Contains(r.Reason, missingProgramError("update-alternatives").Error()) {
				t.Fatalf("%+v", r)
			}
		}
	}
}

func TestUpdateAlternativesInputs(t *testing.T) {
	t.Parallel()
	priority := 10
	for a, message := range map[UpdateAlternatives]string{
		{ProgramPath: "/usr/bin/vim"}: "update_alternatives.name is required",
		{Name: "editor"}:              "update_alternatives.program_path is required unless automatic: true",
		{Name: "editor", Automatic: true, ProgramPath: "/usr/bin/vim"}:                       "takes no program_path with automatic: true",
		{Name: "editor", Automatic: true, GenericCommandPath: "/usr/bin/editor"}:             "they need program_path, not automatic: true",
		{Name: "editor", ProgramPath: "/usr/bin/vim", GenericCommandPath: "/usr/bin/editor"}: "update_alternatives.priority_when_registered is required with generic_command_path",
		{Name: "editor", ProgramPath: "/usr/bin/vim", PriorityWhenRegistered: &priority}:     "update_alternatives.generic_command_path is required with priority_when_registered",
	} {
		if err := ValidateUpdateAlternatives(a); err == nil || !strings.Contains(err.Error(), message) {
			t.Error(a, err)
		}
	}
	for name, ok := range map[string]bool{"editor": true, "java": true, "x-www-browser": true, "libnssckbi.so.x86_64": true, "c++": true, "": false, "a/b": false, "a b": false, "-x": false} {
		if err := alternativesNameRule(name); (err == nil) != ok {
			t.Error(name, err)
		}
	}
}
