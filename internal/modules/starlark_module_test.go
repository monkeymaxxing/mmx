package modules

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

// starlarkGreeting is the module the design documents show.
const starlarkGreeting = `def run_node_task(inputs):
    greeting = "Hello, " + inputs["person"] + "!\n"
    written = write_file(inputs["path"], greeting, owner = inputs["owner"], group = inputs["group"])
    if written.error:
        return {"status": "failed", "reason": "MMX could not write the greeting: " + written.error}
    print("dryrun:", is_dryrun())
    return {"outputs": {"greeting": greeting}}
`

const starlarkGreetingDescription = `summary: Makes sure a file holds a greeting for one person.
inputs:
  path: {type: string, required: true, description: The file.}
  person: {type: string, required: true, description: The person.}
  owner: {type: string, required: true, description: The owner.}
  group: {type: string, required: true, description: The group.}
outputs:
  greeting: {type: string, description: The greeting.}
examples:
  - description: Greet Barbara.
    config: |
      greeting: {path: /tmp/greeting.txt, person: Barbara, owner: root, group: root}
`

func starlarkModule(t *testing.T, script, description string) Module {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ModuleScriptFile), []byte(script), 0644); err != nil {
		t.Fatal(err)
	}
	d, err := ParseModuleDescription([]byte(description), "greeting")
	if err != nil {
		t.Fatal(err)
	}
	return Module{Kind: KindProjectStarlark, Description: d, Directory: dir}
}

func TestStarlarkModuleDryrunRunAndRerun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	module := starlarkModule(t, starlarkGreeting, starlarkGreetingDescription)
	owner, group := currentAccount(t)
	path := filepath.Join(traversableTempDir(t), "greeting.txt")
	inputs := map[string]any{"path": path, "person": "Barbara", "owner": owner, "group": group}
	dryrun := Runtime{Dryrun: true, BeforeMutation: func(string) error { t.Fatal("a dryrun of a script reached the mutation boundary"); return nil }}

	r := dryrun.Starlark(ctx, "greeting", module, inputs, value.Scope{})
	if r.Status != "succeeded" || r.Change != "would_change" || !strings.Contains(r.Diff, "Hello, Barbara!") || r.Stderr != "dryrun: True\n" {
		t.Fatalf("dryrun: %+v", r)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("a dryrun of a script wrote the file")
	}
	first := (Runtime{}).Starlark(ctx, "greeting", module, inputs, value.Scope{})
	if first.Status != "succeeded" || first.Change != "changed" || first.ModuleOutputs["greeting"] != "Hello, Barbara!\n" {
		t.Fatalf("run: %+v", first)
	}
	if b, _ := os.ReadFile(path); string(b) != "Hello, Barbara!\n" {
		t.Fatalf("file holds %q", b)
	}
	again := (Runtime{}).Starlark(ctx, "greeting", module, inputs, value.Scope{})
	if again.Change != "unchanged" || again.Diff != "" || again.Fingerprint != first.Fingerprint {
		t.Fatalf("rerun: %+v", again)
	}
	inputs["person"] = "Ann"
	if other := dryrun.Starlark(ctx, "greeting", module, inputs, value.Scope{}); other.Change != "would_change" || other.Fingerprint == first.Fingerprint {
		t.Fatalf("a changed greeting kept its fingerprint: %+v", other)
	}
	delete(inputs, "person")
	if r := dryrun.Starlark(ctx, "greeting", module, inputs, value.Scope{}); r.FailureKind != "config" || !strings.Contains(r.Reason, "greeting.person: the input person is required") {
		t.Fatalf("missing input: %+v", r)
	}
}

func TestStarlarkModuleFailures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	description := "summary: x\noutputs: {greeting: {type: string, description: d}}\nexamples: [{description: x, config: 'greeting: {}'}]\n"
	for script, want := range map[string]string{
		"def run_node_task(inputs):\n    return inputs[\"missing\"]\n":                                        `module/greeting/module.star:2:18: key "missing" not in dict`,
		"def run_node_task(inputs):\n    return {\"status\": \"failed\"}\n":                                   "a failed result needs a reason",
		"def run_node_task(inputs):\n    return {\"status\": \"failed\", \"reason\": \"no\"}\n":               "no",
		"def run_node_task(inputs):\n    return {\"outputs\": {\"other\": 1}}\n":                              "does not list",
		"def run_node_task(inputs):\n    return {\"outputs\": {\"greeting\": 1}}\n":                           "expected a string",
		"def run_node_task(inputs):\n    return 3\n":                                                          "must return None or a dictionary",
		"def run_node_task(inputs):\n    return {\"outputs\": {\"greeting\": {\"$mmx_binary\": \"aGk=\"}}}\n": "outputs use a reserved MMX record key ($mmx_secret, $mmx_truncated, or $mmx_binary)",
		"def other(inputs):\n    return None\n":                                                               "must define run_node_task(inputs)",
		"def run_node_task(inputs):\n    for i in range(100000000):\n        pass\n":                          "too many steps",
		"load(\"x.star\", \"y\")\n":                                                                           "load",
		"def run_node_task(inputs):\n    return open(\"/etc/passwd\")\n":                                      "undefined: open",
	} {
		r := (Runtime{Dryrun: true}).Starlark(ctx, "greeting", starlarkModule(t, script, description), nil, value.Scope{})
		if r.Status != "failed" || !strings.Contains(r.Reason, want) {
			t.Errorf("%q: got %q, want %q", script, r.Reason, want)
		}
	}
}

// A command that changes the node does not run in a dryrun; MMX records that
// it would run, and the task would change.
func TestStarlarkModuleCommandsThatChangeTheNode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	marker := filepath.Join(t.TempDir(), "ran")
	script := "def run_node_task(inputs):\n    out = run_command_that_changes_the_node([\"touch\", \"" + marker + "\"])\n    read = run_command([\"echo\", \"hello\"])\n    return {\"outputs\": {\"ran\": out.ran, \"said\": read.standard_output}}\n"
	description := "summary: x\noutputs: {ran: {type: boolean, description: d}, said: {type: string, description: d}}\nexamples: [{description: x, config: 'greeting: {}'}]\n"
	module := starlarkModule(t, script, description)
	r := (Runtime{Dryrun: true}).Starlark(ctx, "greeting", module, nil, value.Scope{})
	if r.Change != "would_change" || r.ModuleOutputs["ran"] != false || r.ModuleOutputs["said"] != "hello\n" || !strings.Contains(r.Diff, "would run: touch") {
		t.Fatalf("dryrun: %+v", r)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("a dryrun ran a command that changes the node")
	}
	r = (Runtime{}).Starlark(ctx, "greeting", module, nil, value.Scope{})
	if r.Change != "changed" || r.ModuleOutputs["ran"] != true {
		t.Fatalf("run: %+v", r)
	}
}

// A package index refresh runs in a run and not in a dryrun, and it is no
// change of the task: a task that only refreshes is unchanged, and the
// refresh shows in no difference. A changing command beside it still
// changes the task.
func TestStarlarkModuleRefreshesAPackageIndex(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	directory := t.TempDir()
	refreshed, changed := filepath.Join(directory, "refreshed"), filepath.Join(directory, "changed")
	script := "def run_node_task(inputs):\n" +
		"    out = run_command_that_refreshes_a_package_index([\"touch\", \"" + refreshed + "\"])\n" +
		"    if inputs.get(\"change\"):\n" +
		"        run_command_that_changes_the_node([\"touch\", \"" + changed + "\"])\n" +
		"    return {\"outputs\": {\"ran\": out.ran, \"error\": out.error}}\n"
	description := "summary: x\ninputs: {change: {type: boolean, required: false, description: d}}\n" +
		"outputs: {ran: {type: boolean, description: d}, error: {type: string, description: d}}\nexamples: [{description: x, config: 'greeting: {}'}]\n"
	module := starlarkModule(t, script, description)

	r := (Runtime{Dryrun: true}).Starlark(ctx, "greeting", module, nil, value.Scope{})
	if r.Status != "succeeded" || r.Change != "unchanged" || r.ModuleOutputs["ran"] != false || r.Diff != "" {
		t.Fatalf("dryrun: %+v", r)
	}
	if _, err := os.Stat(refreshed); !os.IsNotExist(err) {
		t.Fatal("a dryrun refreshed a package index")
	}
	r = (Runtime{}).Starlark(ctx, "greeting", module, nil, value.Scope{})
	if r.Status != "succeeded" || r.Change != "unchanged" || r.ModuleOutputs["ran"] != true || r.ModuleOutputs["error"] != "" || r.Diff != "" {
		t.Fatalf("run: %+v", r)
	}
	if _, err := os.Stat(refreshed); err != nil {
		t.Fatal("a run did not refresh the package index:", err)
	}
	r = (Runtime{}).Starlark(ctx, "greeting", module, map[string]any{"change": true}, value.Scope{})
	if r.Change != "changed" || strings.Contains(r.Diff, refreshed) || !strings.Contains(r.Diff, "ran: touch "+changed) {
		t.Fatalf("run with a change: %+v", r)
	}
}

// A changing command that started, or a changing request that was sent, may
// have changed the node even when it ended with an error, such as at the
// node task's time limit: the task's change is unknown then, never
// unchanged. A command that did not start changed nothing. Each case ends
// the time limit once the command runs or the server has the request.
func TestStarlarkModuleInterruptedChangeIsUnknown(t *testing.T) {
	t.Parallel()
	description := "summary: x\nexamples: [{description: x, config: 'greeting: {}'}]\n"
	started := filepath.Join(t.TempDir(), "started")
	received := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(received)
		<-r.Context().Done()
	}))
	defer server.Close()
	for _, tc := range []struct {
		name, call   string
		begun        func(ctx context.Context) bool
		change, diff string
	}{
		{"a command at the time limit", `run_command_that_changes_the_node(["sh", "-c", "touch ` + started + `; sleep 1000"])`,
			func(context.Context) bool { return fileExists(started) }, "unknown", "ran, then ended with an error: sh -c touch " + started + "; sleep 1000\n"},
		{"a request at the time limit", `http_request("POST", "` + server.URL + `/items")`,
			func(ctx context.Context) bool {
				select {
				case <-received:
					return true
				case <-ctx.Done():
					return false
				}
			}, "unknown", "sent POST to " + strings.TrimPrefix(server.URL, "http://") + ", then the answer failed\n"},
		{"a command that does not start", `run_command_that_changes_the_node(["/nonexistent/program"])`,
			nil, "unchanged", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.begun != nil {
				go func() {
					for ctx.Err() == nil && !tc.begun(ctx) {
						time.Sleep(10 * time.Millisecond)
					}
					cancel()
				}()
			}
			module := starlarkModule(t, "def run_node_task(inputs):\n    out = "+tc.call+"\n    return {\"status\": \"failed\", \"reason\": \"the call ended: \" + out.error}\n", description)
			r := (Runtime{}).Starlark(ctx, "greeting", module, nil, value.Scope{})
			if r.Status != "failed" || r.Change != tc.change || r.Diff != tc.diff {
				t.Fatalf("%+v", r)
			}
		})
	}
}

// A script says that it cannot check, or that something it needs is missing;
// MMX decides what that means in a dryrun and in a run.
func TestStarlarkModuleNotCheckedAndMissingPrerequisite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	description := "summary: x\nexamples: [{description: x, config: 'greeting: {}'}]\n"
	module := func(status string) Module {
		return starlarkModule(t, "def run_node_task(inputs):\n    return {\"status\": \""+status+"\", \"reason\": \"greeting: the program greet is not installed\"}\n", description)
	}
	if r := (Runtime{Dryrun: true}).Starlark(ctx, "greeting", module("not_checked"), nil, value.Scope{}); r.Status != "not_checked" || r.Change != "unknown" || r.Reason != "greeting: the program greet is not installed" {
		t.Errorf("not_checked in a dryrun: %+v", r)
	}
	if r := (Runtime{}).Starlark(ctx, "greeting", module("not_checked"), nil, value.Scope{}); r.Status != "failed" || !strings.Contains(r.Reason, "status: not_checked is allowed only in a dryrun") {
		t.Errorf("not_checked in a run: %+v", r)
	}
	if r := (Runtime{Dryrun: true}).Starlark(ctx, "greeting", module("missing_prerequisite"), nil, value.Scope{}); r.Status != "not_checked" || r.Change != "unknown" || r.FailureKind != "" {
		t.Errorf("missing_prerequisite in a dryrun: %+v", r)
	}
	if r := (Runtime{}).Starlark(ctx, "greeting", module("missing_prerequisite"), nil, value.Scope{}); r.Status != "failed" || r.FailureKind != "missing_prerequisite" || r.Change != "unchanged" || r.Reason != "greeting: the program greet is not installed" {
		t.Errorf("missing_prerequisite in a run: %+v", r)
	}
	for _, dryrun := range []bool{true, false} {
		if r := (Runtime{Dryrun: dryrun}).Starlark(ctx, "greeting", module("invalid_inputs"), nil, value.Scope{}); r.Status != "failed" || r.FailureKind != "config" || r.Change != "unchanged" || r.Reason != "greeting: the program greet is not installed" {
			t.Errorf("invalid_inputs with dryrun %v: %+v", dryrun, r)
		}
	}
	for _, status := range []string{"not_checked", "missing_prerequisite", "invalid_inputs"} {
		noReason := starlarkModule(t, "def run_node_task(inputs):\n    return {\"status\": \""+status+"\"}\n", description)
		if r := (Runtime{Dryrun: true}).Starlark(ctx, "greeting", noReason, nil, value.Scope{}); r.Status != "failed" || !strings.Contains(r.Reason, "needs a reason") {
			t.Errorf("%s without a reason: %+v", status, r)
		}
	}
	unknown := starlarkModule(t, "def run_node_task(inputs):\n    return {\"status\": \"skipped\", \"reason\": \"x\"}\n", description)
	if r := (Runtime{}).Starlark(ctx, "greeting", unknown, nil, value.Scope{}); !strings.Contains(r.Reason, "status must be one of succeeded, failed, not_checked, missing_prerequisite, invalid_inputs") {
		t.Errorf("an unknown status: %+v", r)
	}
}

// find_program tells a script whether a tool is installed, and a primitive's
// command gets a fixed environment with root's home directory.
func TestStarlarkModuleFindsProgramsAndRunsCommandsInAFixedEnvironment(t *testing.T) {
	t.Setenv("MMX_TEST_VALUE_OF_THE_WORKER", "must not reach a command")
	script := `def run_node_task(inputs):
    shell = find_program("sh")
    missing = find_program("mmx-no-such-program")
    environment = run_command(["env"])
    return {"outputs": {"found": shell.found, "path": shell.path, "missing": missing.found, "missing_error": missing.error, "environment": environment.standard_output}}
`
	description := "summary: x\noutputs:\n  found: {type: boolean, description: d}\n  path: {type: string, description: d}\n  missing: {type: boolean, description: d}\n  missing_error: {type: string, description: d}\n  environment: {type: string, description: d}\nexamples: [{description: x, config: 'greeting: {}'}]\n"
	r := (Runtime{Dryrun: true}).Starlark(context.Background(), "greeting", starlarkModule(t, script, description), nil, value.Scope{})
	if r.Status != "succeeded" || r.ModuleOutputs["found"] != true || !filepath.IsAbs(r.ModuleOutputs["path"].(string)) ||
		r.ModuleOutputs["missing"] != false || r.ModuleOutputs["missing_error"] != "" {
		t.Fatalf("find_program: %+v", r)
	}
	environment := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(r.ModuleOutputs["environment"].(string)), "\n") {
		name, text, _ := strings.Cut(line, "=")
		environment[name] = text
	}
	// The shell that sets the umask adds PWD and its own variables on some
	// systems; what MMX gives is fixed, and nothing of the worker's is there.
	if environment["HOME"] == "" || !filepath.IsAbs(environment["HOME"]) || environment["USER"] != "root" || environment["LOGNAME"] != "root" ||
		environment["LANG"] != "C.UTF-8" || environment["PATH"] != "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" {
		t.Errorf("the environment of a primitive's command: %v", environment)
	}
	if _, inherited := environment["MMX_TEST_VALUE_OF_THE_WORKER"]; inherited {
		t.Error("a primitive's command got a variable of the worker's environment")
	}
}

// An input that the description marks private_value does not hide the result
// when it holds a secret: the engine replaces its text instead. A secret in
// another input still hides the result.
func TestStarlarkModulePrivateValue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	script := `def run_node_task(inputs):
    return {"status": "failed", "reason": "the server refused the password " + inputs["password"] + " of " + inputs["name"]}
`
	description := "summary: x\ninputs:\n  name: {type: string, required: true, description: d}\n  password: {type: string, required: true, private_value: true, description: d}\nexamples: [{description: x, config: 'greeting: {name: shop, password: p}'}]\n"
	module := starlarkModule(t, script, description)
	scope, trace := taskScope(map[string]string{"shop_password": "s3cret-sentinel", "shop_name": "name-sentinel"})
	r := (Runtime{}).Starlark(ctx, "greeting", module, map[string]any{"name": "shop", "password": "{{ .secrets.shop_password }}"}, scope)
	if r.Secret || trace.Used {
		t.Fatalf("a secret in a private value hid the result: %+v", r)
	}
	if reported := reportedByTheEngine(r, scope); reported.Reason != "the server refused the password [redacted] of shop" {
		t.Errorf("the reported reason: %q", reported.Reason)
	}
	scope, _ = taskScope(map[string]string{"shop_password": "s3cret-sentinel", "shop_name": "name-sentinel"})
	if r := (Runtime{}).Starlark(ctx, "greeting", module, map[string]any{"name": "{{ .secrets.shop_name }}", "password": "{{ .secrets.shop_password }}"}, scope); !r.Secret {
		t.Errorf("a secret in an input that is no private value did not hide the result: %+v", r)
	} // The reason of a kind of failure that the script chose shows for a
	// task without secrets, and with a secret only in a private value. With
	// a secret in another input it does not show: the script wrote it, and
	// it can hold that input's value. The failure keeps its kind, so MMX
	// still does not try again.
	for _, status := range []string{"invalid_inputs", "missing_prerequisite"} {
		telling := starlarkModule(t, "def run_node_task(inputs):\n    return {\"status\": \""+status+"\", \"reason\": \"greeting.name: \" + inputs[\"name\"] + \" is no name\"}\n", description)
		scope, _ = taskScope(map[string]string{"shop_password": "s3cret-sentinel", "shop_name": "name-sentinel"})
		r := reportedByTheEngine((Runtime{}).Starlark(ctx, "greeting", telling, map[string]any{"name": "shop", "password": "{{ .secrets.shop_password }}"}, scope), scope)
		if r.Secret || r.Reason != "greeting.name: shop is no name" || r.FailureKind == "" {
			t.Errorf("%s with a secret only in a private value: %+v", status, r)
		}
		scope, _ = taskScope(map[string]string{"shop_password": "s3cret-sentinel", "shop_name": "name-sentinel"})
		r = (Runtime{}).Starlark(ctx, "greeting", telling, map[string]any{"name": "{{ .secrets.shop_name }}", "password": "{{ .secrets.shop_password }}"}, scope)
		if !r.Secret || strings.Contains(r.Reason, "name-sentinel") || r.FailureKind == "" || r.Safe().Reason != "details suppressed for a task using secrets" {
			t.Errorf("%s with a secret in another input: %+v", status, r)
		}
	}
}

// A script says that a failure must not be tried again; only a failed result
// can say so.
func TestStarlarkModuleAsksForNoOtherTry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	description := "summary: x\nexamples: [{description: x, config: 'greeting: {}'}]\n"
	run := func(returned string) protocol.Result {
		return (Runtime{}).Starlark(ctx, "greeting", starlarkModule(t, "def run_node_task(inputs):\n    return "+returned+"\n", description), nil, value.Scope{})
	}
	if r := run(`{"status": "failed", "reason": "the second step failed", "try_again": False}`); r.Status != "failed" || !r.NoOtherTry || r.FailureKind != "" || r.Reason != "the second step failed" {
		t.Errorf("try_again: False: %+v", r)
	}
	if r := run(`{"status": "failed", "reason": "the second step failed"}`); r.NoOtherTry {
		t.Errorf("a failure without try_again: %+v", r)
	}
	if r := run(`{"status": "failed", "reason": "the second step failed", "try_again": True}`); r.NoOtherTry || r.Status != "failed" {
		t.Errorf("try_again: True: %+v", r)
	}
	if r := run(`{"try_again": False}`); r.Status != "failed" || !strings.Contains(r.Reason, "try_again: False requires status: failed; the returned status is succeeded") {
		t.Errorf("try_again: False on a success: %+v", r)
	}
	if r := run(`{"status": "failed", "reason": "x", "try_again": "no"}`); !strings.Contains(r.Reason, `try_again is "no"; try_again must be True or False`) {
		t.Errorf("try_again that is no boolean: %+v", r)
	}
}

// In a dryrun a script learns that an earlier node task would change a path
// it reads: the path itself, a directory that holds it, or something inside
// it. In a run the answer is always no.
func TestStarlarkModuleAsksWhetherAnEarlierNodeTaskWouldChangeAPath(t *testing.T) {
	t.Parallel()
	script := `def run_node_task(inputs):
    return {"outputs": {"answers": [earlier_node_task_would_change(path) for path in inputs["paths"]]}}
`
	description := "summary: x\ninputs: {paths: {type: list_of_strings, required: true, description: d}}\noutputs: {answers: {type: any, description: d}}\nexamples: [{description: x, config: 'greeting: {paths: [/a]}'}]\n"
	module := starlarkModule(t, script, description)
	inputs := map[string]any{"paths": []any{"/etc/shop/domain.xml", "/etc/shop", "/etc/shop/other.xml", "/etc/shopping", "/srv/release/current/app", "/srv"}}
	changing := []string{"/etc/shop/domain.xml", "/srv/release/"}
	r := (Runtime{Dryrun: true, PreviewPaths: changing}).Starlark(context.Background(), "greeting", module, inputs, value.Scope{})
	if got := fmt.Sprint(r.ModuleOutputs["answers"]); got != "[true true false false true true]" {
		t.Errorf("in a dryrun: %s (%+v)", got, r)
	}
	r = (Runtime{PreviewPaths: changing}).Starlark(context.Background(), "greeting", module, inputs, value.Scope{})
	if got := fmt.Sprint(r.ModuleOutputs["answers"]); got != "[false false false false false false]" {
		t.Errorf("in a run: %s (%+v)", got, r)
	}
}

// The module dialect permits sets but rejects unbounded control flow and
// reassignment of global names.
func TestStarlarkModuleDialect(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, script, status string
	}{
		{"set", "def run_node_task(inputs):\n    return {\"outputs\": {\"greeting\": str(len(set([1, 1])))}}\n", "succeeded"},
		{"while", "def run_node_task(inputs):\n    while False:\n        pass\n", "failed"},
		{"recursion", "def run_node_task(inputs):\n    return run_node_task(inputs)\n", "failed"},
		{"global reassignment", "name = 1\nname = 2\ndef run_node_task(inputs):\n    return None\n", "failed"},
		{"top-level control", "if True:\n    name = 1\ndef run_node_task(inputs):\n    return None\n", "failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := (Runtime{}).Starlark(context.Background(), "greeting", starlarkModule(t, test.script, "summary: x\noutputs: {greeting: {type: string, description: d}}\nexamples: [{description: x, config: 'greeting: {}'}]\n"), nil, value.Scope{})
			if result.Status != test.status {
				t.Fatalf("status = %s, want %s: %s", result.Status, test.status, result.Reason)
			}
			if test.status == "succeeded" && result.ModuleOutputs["greeting"] != "1" {
				t.Fatalf("set result: %+v", result.ModuleOutputs)
			}
		})
	}
}

func TestStarlarkDurationParser(t *testing.T) {
	t.Parallel()
	module := starlarkModule(t, `
def run_node_task(inputs):
    duration = parse_duration(inputs["person"])
    if duration.error:
        return {"status": "invalid_inputs", "reason": duration.error}
    return {"outputs": {"greeting": "matches" if duration.seconds == float(inputs["owner"]) else "different"}}
`, starlarkGreetingDescription)
	for _, c := range []struct{ text, seconds string }{
		{"2m", "120"}, {"1m30s", "90"}, {"1500ms", "1.5"}, {"0s", "0"}, {"-1s", "-1"}, {"1h", "3600"},
	} {
		t.Run(c.text, func(t *testing.T) {
			r := (Runtime{}).Starlark(context.Background(), "greeting", module, map[string]any{"person": c.text, "owner": c.seconds, "path": "/unused", "group": "unused"}, value.Scope{})
			if r.Status != "succeeded" || r.ModuleOutputs["greeting"] != "matches" {
				t.Fatalf("%+v", r)
			}
		})
	}
	r := (Runtime{}).Starlark(context.Background(), "greeting", module, map[string]any{"person": "soon", "owner": "0", "path": "/unused", "group": "unused"}, value.Scope{})
	if r.FailureKind != "config" || r.Reason != "expected a duration, such as 30s or 2m" {
		t.Fatalf("%+v", r)
	}
}

// A script that does not parse, and a script whose command has no program on
// the node, fail the same way on every try, so the engine must not retry them.
func TestStarlarkModuleFailuresThatNoRetryCanFixAreNotRetried(t *testing.T) {
	t.Parallel()
	description := "summary: x\nexamples: [{description: x, config: 'greeting: {}'}]\n"
	syntaxError := starlarkModule(t, "def run_node_task(inputs):\n    return {\n", description)
	missingProgram := starlarkModule(t, "def run_node_task(inputs):\n    out = run_command([\"mmx-no-such-program\", \"-Q\"])\n    return {\"status\": \"failed\", \"reason\": \"reading the packages: \" + out.error}\n", description)
	for _, dryrun := range []bool{true, false} {
		r := (Runtime{Dryrun: dryrun}).Starlark(context.Background(), "greeting", syntaxError, nil, value.Scope{})
		if r.Status != "failed" || r.FailureKind != "config" {
			t.Errorf("syntax error, dryrun %v: %+v", dryrun, r)
		}
		r = (Runtime{Dryrun: dryrun}).Starlark(context.Background(), "greeting", missingProgram, nil, value.Scope{})
		if !strings.Contains(r.Reason, "mmx-no-such-program is not installed") || (dryrun && r.Status != "not_checked") || (!dryrun && (r.Status != "failed" || r.FailureKind != "missing_prerequisite")) {
			t.Errorf("missing program, dryrun %v: %+v", dryrun, r)
		}
	}
}
