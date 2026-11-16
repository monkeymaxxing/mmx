package modules

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.starlark.net/starlark"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

// The official extra modules are Starlark scripts in the repository's
// official_extra_modules directory. Their tools are not on the machine that
// runs the Go tests, so a test gives a module stand-in programs (stubTools)
// that print what the real tool printed on a test node, and reads which
// commands the module ran. The integration suites prove each module on real
// nodes; these tests cover what is slow or hard to reach there: rejected
// inputs, the outputs of old and new versions of a tool, and failures.

// officialExtraModulesDirectory is the repository's directory of official
// extra modules, from this package.
const officialExtraModulesDirectory = "../../official_extra_modules"

// officialExtraModule loads one official extra module from the repository.
func officialExtraModule(t *testing.T, name string) Module {
	t.Helper()
	directory, err := filepath.Abs(filepath.Join(officialExtraModulesDirectory, name))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(officialExtraModulesDirectory); errors.Is(err, fs.ErrNotExist) {
		t.Skip("the test binary runs outside the repository, as on a test node")
	}
	description, err := ReadModuleDescription(directory, name)
	if err != nil {
		t.Fatal(err)
	}
	return Module{Kind: KindOfficialExtra, Description: description, Directory: directory}
}

// runOfficialExtraModule runs one node task of an official extra module, in
// a dryrun or in a run, and returns the result as the node task execution
// engine reports it.
func runOfficialExtraModule(t *testing.T, name string, inputs map[string]any, dryrun bool) protocol.Result {
	t.Helper()
	scope, _ := value.Scope{}.Trace()
	r := Runtime{Dryrun: dryrun}.Starlark(testContext(t), name, officialExtraModule(t, name), inputs, scope)
	return reportedByTheEngine(r, scope)
}

// stubToolsDirectory is the directory of the stand-ins that stubTools made
// for the test or the closest test that it is a subtest of.
func stubToolsDirectory(t *testing.T) string {
	t.Helper()
	path, set := workerEnvironmentOfTest(t.Name())["PATH"]
	if !set {
		t.Fatal("the test has no stand-in programs")
	}
	directory, _, _ := strings.Cut(path, string(os.PathListSeparator))
	return directory
}

// stubTools makes the programs that a module finds and runs in this test:
// each entry of tools is a program's name and the body of a /bin/sh script.
// PATH holds only the stand-ins and the system's own /usr/bin and /bin, so a
// tool that is installed on this machine never answers in place of one.
// The PATH holds for the test and its subtests (setWorkerEnvironment). A
// body reads its arguments as "$@" and can keep files in the directory
// $STUBS, such as the standard input it got. calls returns one line for each
// call so far, in order: the program's name and its arguments.
//
// Every program is a link to one script of the repository, which notes the
// call and runs the body from the directory's subdirectory "bodies". A new executable file
// can take half a second for its first start, as on macOS, and these tests
// start hundreds of stand-ins; a link to a file that has started before
// starts at once.
func stubTools(t *testing.T, tools map[string]string) (calls func() []string) {
	t.Helper()
	launcher, err := filepath.Abs("testdata/official_extra_modules/stand_in_program.sh")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(launcher); errors.Is(err, fs.ErrNotExist) {
		t.Skip("the test binary runs outside the repository, as on a test node")
	}
	directory := t.TempDir()
	if err := os.Mkdir(filepath.Join(directory, "bodies"), 0755); err != nil {
		t.Fatal(err)
	}
	for name, body := range tools {
		if err := os.WriteFile(filepath.Join(directory, "bodies", name), []byte(body+"\n"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(launcher, filepath.Join(directory, name)); err != nil {
			t.Fatal(err)
		}
	}
	setWorkerEnvironment(t, "PATH", directory+":/usr/bin:/bin")
	return func() []string {
		b, err := os.ReadFile(filepath.Join(directory, "calls"))
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			t.Fatal(err)
		}
		return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	}
}

// Every official extra module has its script, its description, and its
// page; the description and its examples are valid; the script compiles and
// defines run_node_task(inputs); and the page names every input.
func TestOfficialExtraModulesAreComplete(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(officialExtraModulesDirectory)
	if errors.Is(err, fs.ErrNotExist) {
		t.Skip("the test binary runs outside the repository, as on a test node")
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		t.Run(name, func(t *testing.T) {
			if !ModuleName.MatchString(name) {
				t.Fatalf("%s is no module name", name)
			}
			if _, core := LookUpCoreModule(name); core {
				t.Fatalf("%s is also a core module", name)
			}
			module := officialExtraModule(t, name)
			source, err := os.ReadFile(filepath.Join(module.Directory, ModuleScriptFile))
			if err != nil {
				t.Fatal(err)
			}
			script := &starlarkScript{run: Runtime{Dryrun: true}, ctx: context.Background()}
			globals, err := starlark.ExecFileOptions(starlarkFileOptions, &starlark.Thread{Name: name}, ModuleScriptFile, source, script.predeclared())
			if err != nil {
				t.Fatal(err)
			}
			if function, ok := globals["run_node_task"].(*starlark.Function); !ok || function.NumParams() != 1 {
				t.Fatal("the script does not define run_node_task(inputs)")
			}
			page, err := os.ReadFile(filepath.Join(module.Directory, "README.md"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(string(page), "# `"+name+"`") {
				t.Errorf("README.md does not start with the heading # `%s`", name)
			}
			for _, input := range module.Description.Inputs {
				if !strings.Contains(string(page), "`"+input.Name+"`") {
					t.Errorf("README.md does not name the input %s", input.Name)
				}
			}
			for _, output := range module.Description.Outputs {
				if !strings.Contains(string(page), "`"+output.Name+"`") {
					t.Errorf("README.md does not name the output %s", output.Name)
				}
			}
		})
	}
}

// The helpers of this file work: a stand-in program answers a module, and
// its calls are recorded in order.
func TestStubToolsAnswerAModule(t *testing.T) {
	t.Parallel()
	calls := stubTools(t, map[string]string{"greet": `cat > "$STUBS/input"; echo "hello $1"`})
	script := `def run_node_task(inputs):
    if not find_program("greet").found:
        return {"status": "missing_prerequisite", "reason": "greet is missing"}
    said = run_command(["greet", inputs["person"]], standard_input = "from the module")
    return {"outputs": {"greeting": said.standard_output}}
`
	module := starlarkModule(t, script, "summary: x\ninputs: {person: {type: string, required: true, description: d}}\noutputs: {greeting: {type: string, description: d}}\nexamples: [{description: x, config: 'greeting: {person: Ann}'}]\n")
	r := (Runtime{Dryrun: true}).Starlark(testContext(t), "greeting", module, map[string]any{"person": "Barbara"}, value.Scope{})
	if r.Status != "succeeded" || r.ModuleOutputs["greeting"] != "hello Barbara\n" {
		t.Fatalf("%+v", r)
	}
	if got := calls(); len(got) != 1 || got[0] != "greet Barbara" {
		t.Fatalf("calls: %q", got)
	}
}

// The owner chooses these values; removal tasks do not need creation settings.
func TestOfficialExtraModulesRequireExplicitChoices(t *testing.T) {
	t.Parallel()
	calls := stubTools(t, map[string]string{})
	for _, c := range []struct {
		module string
		inputs map[string]any
		field  string
		reason string
	}{
		{"incus_instance", map[string]any{"name": "shop", "state": "running", "profiles": []any{}}, "instance_type", "is required for state: running or stopped"},
		{"incus_instance", map[string]any{"name": "shop", "state": "running", "instance_type": "container"}, "profiles", "is required for state: running or stopped"},
		{"incus_instance", map[string]any{"name": "shop", "state": "stopped", "profiles": []any{}}, "instance_type", "is required for state: running or stopped"},
		{"incus_instance", map[string]any{"name": "shop", "state": "stopped", "instance_type": "container"}, "profiles", "is required for state: running or stopped"},
		{"lvm_logical_volume", map[string]any{"name": "data", "volume_group": "shop", "size": "1GiB"}, "grow_filesystem_with_volume", "is required for state: present; write true to grow the filesystem with the volume or false to leave the filesystem unchanged"},
		{"lvm_volume_group", map[string]any{"name": "shop", "physical_volumes": []any{"/dev/sdb"}, "physical_volumes_not_listed": "keep"}, "grow_physical_volumes_with_their_devices", "is required for state: present; write true to grow physical volumes with their devices or false to leave physical volumes unchanged"},
		{"rabbitmq_policy", map[string]any{"name": "shop", "virtual_host": "/", "pattern": ".*", "apply_to": "queues", "definition": map[string]any{"max-length": 5}}, "priority", "is required for state: present"},
	} {
		t.Run(c.module+"/"+c.field+"/"+fmt.Sprint(c.inputs["state"]), func(t *testing.T) {
			for _, dryrun := range []bool{false, true} {
				r := runOfficialExtraModule(t, c.module, c.inputs, dryrun)
				if r.Status != "failed" || r.FailureKind != "config" || r.Change != "unchanged" || r.Reason != c.module+"."+c.field+": "+c.reason {
					t.Fatalf("dryrun %v: %+v", dryrun, r)
				}
			}
		})
	}
	if got := calls(); len(got) != 0 {
		t.Fatalf("programs ran before validation: %v", got)
	}
}

func TestOfficialExtraShutdownDurations(t *testing.T) {
	t.Parallel()
	calls := stubTools(t, map[string]string{})
	for _, module := range []string{"incus_instance", "libvirt_domain"} {
		for _, duration := range []string{"soon", "1500ms", "-1s"} {
			inputs := map[string]any{"name": "shop", "state": "stopped", "wait_for_shutdown_at_most": duration}
			rule := "must be a nonnegative duration of whole seconds, such as 30s or 2m; shutdown checks repeat each second"
			if module == "incus_instance" {
				inputs["instance_type"] = "container"
				inputs["profiles"] = []any{}
				rule = "must be a positive duration of whole seconds, such as 30s or 2m; incus stop takes whole seconds"
			}
			for _, dryrun := range []bool{false, true} {
				r := runOfficialExtraModule(t, module, inputs, dryrun)
				if r.FailureKind != "config" || r.Change != "unchanged" || r.Reason != module+".wait_for_shutdown_at_most: "+rule {
					t.Fatalf("%s %s: %+v", module, duration, r)
				}
			}
		}
	}
	if got := calls(); len(got) != 0 {
		t.Fatalf("programs ran before validation: %v", got)
	}
}
