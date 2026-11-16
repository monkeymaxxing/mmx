package modules

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"mmx/internal/moduleio"
	"mmx/internal/protocol"
	"mmx/internal/value"
)

// The current test executable is also a standalone JSON fixture process. Native
// Linux tests use it without installing a compiler or interpreter in the guest.
func TestModuleProcessFixture(t *testing.T) {
	if os.Getenv("MMX_PROCESS_FIXTURE") != "1" {
		return
	}
	request, err := moduleio.ReadRequest(os.Stdin)
	if err != nil {
		os.Exit(3)
	}
	r := moduleio.Result{Protocol: 1, Status: "succeeded", Change: "unchanged"}
	code := 0
	switch request.Inputs["case"] {
	case "echo":
		r.Outputs, _ = json.Marshal(map[string]any{"data": request.Inputs["data"]})
		r.Reason = "inputs preserved"
		fmt.Fprintln(os.Stderr, "diagnostic")
	case "environment":
		r.Outputs, _ = json.Marshal(map[string]any{"home": os.Getenv("HOME"), "logname": os.Getenv("LOGNAME")})
	case "partial", "false_success":
		r.Change, r.Reason, code = "changed", "second change failed", 7
		if request.Inputs["case"] == "partial" {
			r.Status = "failed"
		}
	case "not_checked":
		r.Status, r.Change, r.Reason = "not_checked", "unknown", "missing prerequisite"
	case "malformed":
		fmt.Fprint(os.Stdout, `{"protocol":1, BROKEN private-result`)
		os.Exit(0)
	case "oversize":
		moduleio.WriteResult(os.Stdout, request.Mode, r)
		io.WriteString(os.Stdout, strings.Repeat(" ", moduleio.MaxResultBytes))
		os.Exit(0)
	case "extra":
		moduleio.WriteResult(os.Stdout, request.Mode, r)
		moduleio.WriteResult(os.Stdout, request.Mode, r)
		os.Exit(0)
	case "no_result":
		os.Exit(4)
	case "fingerprint":
		// A fingerprint built from an input, as a module that reports
		// the state it manages may build it.
		r.Fingerprint = "state=" + request.Inputs["data"].(string)
	case "private":
		r.Secret, r.Reason = true, "private-result"
		r.Outputs = json.RawMessage(`{"data":"private-result"}`)
		fmt.Fprintln(os.Stderr, "private-result")
	case "forge_secret":
		r.Outputs = json.RawMessage(`{"data":[{"$mmx_secret":{"name":"unrequested"}}]}`)
	case "forge_truncated":
		r.Outputs = json.RawMessage(`{"note":{"$mmx_truncated":"ok"}}`)
	case "forge_mixed":
		r.Outputs = json.RawMessage(`{"$mmx_secret":"text","other":1}`)
	case "forge_binary":
		r.Outputs = json.RawMessage(`{"file":{"$mmx_binary":"aGk="}}`)
	case "timeout", "background", "pipe_holder":
		// A child that holds the test's FIFO open for as long as it lives,
		// which would be far longer than the module. The module goes on
		// once the child holds the FIFO.
		ready := request.Inputs["child_ready"].(string)
		child := exec.Command("/bin/sh", "-c", `exec 3>"$1" && : >"$2" && exec sleep 1000`, "sh", request.Inputs["child_alive"].(string), ready)
		if request.Inputs["case"] == "pipe_holder" {
			child.Stdout, child.Stderr = os.Stdout, os.Stderr
		}
		if err := child.Start(); err != nil {
			os.Exit(5)
		}
		for !fileExists(ready) {
			time.Sleep(10 * time.Millisecond)
		}
		fmt.Fprintln(os.Stderr, "written while the child ran")
		if request.Inputs["case"] == "timeout" {
			// Until the time limit ends the module.
			time.Sleep(time.Hour)
		}
	}
	if err := moduleio.WriteResult(os.Stdout, request.Mode, r); err != nil {
		os.Exit(6)
	}
	os.Exit(code)
}

func processModule(t *testing.T) Module {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := "#!/bin/sh\nexport MMX_PROCESS_FIXTURE=1 GORACE=atexit_sleep_ms=0\nexec '" + strings.ReplaceAll(binary, "'", "'\\''") + "' -test.run='^TestModuleProcessFixture$'\n"
	// A stand-in, not a program file written here: a program that another
	// test starts meanwhile would inherit the file open for writing until
	// it runs its own program, and running the module would fail with
	// "text file busy" until then.
	writeStandInProgram(t, filepath.Join(dir, ModuleExecutableFile), script)
	if err := os.WriteFile(filepath.Join(dir, ModuleDescriptionFile), []byte("summary: a test fixture\nexamples: [{description: x, config: 'fixture: {}'}]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return Module{Kind: KindProjectExecutable, Directory: dir, Executable: ModuleExecutableFile,
		Files: []ModuleFile{{Path: ModuleExecutableFile, Mode: 0700}, {Path: ModuleDescriptionFile, Mode: 0600}}}
}

func TestExternalResultMapping(t *testing.T) {
	t.Parallel()
	module := processModule(t)
	for _, tc := range []struct {
		kind, status, change string
		check                bool
	}{
		{"echo", "succeeded", "unchanged", false},
		{"partial", "failed", "changed", false},
		{"false_success", "failed", "changed", false},
		{"not_checked", "not_checked", "unknown", true},
		{"not_checked", "failed", "unknown", false},
		{"malformed", "failed", "unknown", false},
		{"oversize", "failed", "unknown", false},
		{"extra", "failed", "unknown", false},
		{"no_result", "failed", "unknown", false},
		{"forge_secret", "failed", "unchanged", false},
		{"forge_truncated", "failed", "unchanged", false},
		{"forge_mixed", "failed", "unchanged", false},
		{"forge_binary", "failed", "unchanged", false},
	} {
		t.Run(tc.kind+fmt.Sprint(tc.check), func(t *testing.T) {
			r := (Runtime{Dryrun: tc.check}).External(context.Background(), "fixture", module, map[string]any{"case": tc.kind}, value.Scope{}, "/unused/helper")
			if r.Status != tc.status || r.Change != tc.change || r.Stdout != "" || r.FailureKind != "" {
				t.Fatalf("unexpected result: %+v", r)
			}
			if strings.HasPrefix(tc.kind, "forge_") && !strings.Contains(r.Reason, "($mmx_secret, $mmx_truncated, or $mmx_binary)") {
				t.Fatalf("the reason does not name every reserved key: %q", r.Reason)
			}
		})
	}
}

func TestExternalTypedValuesAndSecrets(t *testing.T) {
	t.Parallel()
	module := processModule(t)
	for _, data := range []any{nil, false, 0, "", "quote\" slash\\ 日本語\n\n", []any{}, map[string]any{}, json.Number("9007199254740993")} {
		r := (Runtime{}).External(context.Background(), "fixture", module, map[string]any{"case": "echo", "data": data}, value.Scope{}, "/unused/helper")
		actual, _ := json.Marshal(r.ModuleOutputs["data"])
		expected, _ := json.Marshal(data)
		if _, returned := r.ModuleOutputs["data"]; r.Status != "succeeded" || !returned || string(actual) != string(expected) || r.Stderr != "diagnostic\n" {
			t.Fatalf("value lost: %+v", r)
		}
	}
	scope := value.Scope{"inputs": map[string]any{"token": value.Sensitive("private-input")}}
	r := (Runtime{}).External(context.Background(), "fixture", module, map[string]any{"case": "echo", "data": "{{ .inputs.token }}"}, scope, "/unused/helper")
	if !r.Secret || r.ModuleOutputs["data"] != "private-input" {
		t.Fatalf("sensitivity or private delivery lost: %+v", r)
	}
	raw, _ := json.Marshal(r)
	if strings.Contains(string(raw), "private-input") || strings.Contains(string(raw), "diagnostic") {
		t.Fatal("input-derived output leaked")
	}
	r = (Runtime{}).External(context.Background(), "fixture", module, map[string]any{"case": "private"}, value.Scope{}, "/unused/helper")
	raw, _ = json.Marshal(r)
	if !r.Secret || strings.Contains(string(raw), "private-result") {
		t.Fatal("module-declared private output leaked")
	}
	// A fingerprint built from a secret input stays out of the result, and
	// still changes with the state that it describes.
	fingerprint := func(data string) string {
		r := (Runtime{}).External(context.Background(), "fixture", module, map[string]any{"case": "fingerprint", "data": data}, scope, "/unused/helper")
		raw, _ := json.Marshal(r)
		if r.Status != "succeeded" || r.Fingerprint == "" || strings.Contains(r.Fingerprint, "private-input") || strings.Contains(string(raw), "private-input") {
			t.Fatalf("the fingerprint of a secret input leaked or was lost: %s", raw)
		}
		return r.Fingerprint
	}
	if first := fingerprint("{{ .inputs.token }}"); first != fingerprint("{{ .inputs.token }}") || first == fingerprint("other state") {
		t.Fatal("the fingerprint does not follow the state that the module reports")
	}
}

// A module's child ends with the module, whether the module ends at its
// time limit (timeout), returns while the child runs (background), or
// returns while the child holds the module's output pipes (pipe_holder),
// and the module's result does not wait for such a child. A child that
// holds the module's output fails the module, with a reason that says so and
// all that the module wrote. The child holds a FIFO of the test open for as
// long as it lives, so the test waits for the FIFO to close, not for a time.
func TestExternalCancellationAndChildren(t *testing.T) {
	t.Parallel()
	module := processModule(t)
	for _, kind := range []string{"timeout", "background", "pipe_holder"} {
		t.Run(kind, func(t *testing.T) {
			directory := t.TempDir()
			alive, ready := filepath.Join(directory, "child_alive"), filepath.Join(directory, "child_ready")
			if err := syscall.Mkfifo(alive, 0600); err != nil {
				t.Fatal(err)
			}
			// A reader that does not wait for a writer, so the child can open
			// the FIFO, then reads that wait for the end of every writer.
			fd, err := syscall.Open(alive, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
			if err != nil {
				t.Fatal(err)
			}
			if err := syscall.SetNonblock(fd, false); err != nil {
				t.Fatal(err)
			}
			fifo := os.NewFile(uintptr(fd), alive)
			defer fifo.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if kind == "timeout" {
				// The time limit ends the module while its child runs.
				go func() {
					for ctx.Err() == nil && !fileExists(ready) {
						time.Sleep(10 * time.Millisecond)
					}
					cancel()
				}()
			}
			returned := make(chan protocol.Result, 1)
			go func() {
				returned <- (Runtime{}).External(ctx, "fixture", module, map[string]any{"case": kind, "child_alive": alive, "child_ready": ready}, value.Scope{}, "/unused/helper")
			}()
			r := waitAtMostAMinute(t, returned, "the module's result, while its child ran")
			if kind == "background" && r.Status != "succeeded" || kind != "background" && r.Status != "failed" {
				t.Fatalf("unexpected status: %+v", r)
			}
			if kind == "pipe_holder" && !strings.Contains(r.Reason, "a child process kept the command's standard output and standard error open") {
				t.Fatalf("the reason does not say that a child held the output: %+v", r)
			}
			if kind != "timeout" && r.Stderr != "written while the child ran\n" {
				t.Fatalf("output lost: %+v", r)
			}
			if !fileExists(ready) {
				t.Fatalf("the module ended before its child held the FIFO: %+v", r)
			}
			childEnded := make(chan error, 1)
			go func() {
				_, err := io.ReadAll(fifo)
				childEnded <- err
			}()
			if err := waitAtMostAMinute(t, childEnded, "the end of the module's child"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A module whose output MMX could not read whole fails, though the module
// reported success, and the failure is not taken for a result over 1 MiB.
func TestExternalFailsWhenItsOutputCouldNotBeRead(t *testing.T) {
	t.Parallel()
	module := processModule(t)
	for _, failingOutput := range []string{"standard output", "standard error"} {
		ctx := context.WithValue(context.Background(), testProgramOutputHooksKey{}, testProgramOutputHooks{
			readFails: func(what string) error {
				if what == failingOutput {
					return errors.New("the test's read failure")
				}
				return nil
			},
		})
		r := (Runtime{}).External(ctx, "fixture", module, map[string]any{"case": "echo", "data": "x"}, value.Scope{}, "/unused/helper")
		if r.Status != "failed" || r.Reason != "could not read all of the program's "+failingOutput+": the test's read failure" || r.Change != "unknown" || r.Stderr != "diagnostic\n" || r.ModuleOutputs != nil {
			t.Fatalf("%s: %+v", failingOutput, r)
		}
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// waitAtMostAMinute returns what done delivers. The minute only turns a
// hang into a failure; the waits it guards take far less on any machine.
func waitAtMostAMinute[T any](t *testing.T, done <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-done:
		return v
	case <-time.After(time.Minute):
		t.Fatalf("%s did not come within a minute", what)
		panic("unreachable")
	}
}

// A reference in an input is checked against the module's description once
// it resolves on the node, as for a Starlark script, and a value that breaks
// the description never reaches the executable.
func TestExternalChecksResolvedInputs(t *testing.T) {
	t.Parallel()
	module := processModule(t)
	description, err := ParseModuleDescription([]byte(`summary: a test fixture
inputs:
  case: {type: string, required: true, description: The case of the fixture.}
  color: {type: string, required: false, allowed_values: [red, blue], description: A color.}
outputs: {data: {type: any, description: The data that the fixture echoes.}}
examples: [{description: x, config: 'fixture: {case: echo}'}]
`), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	module.Description = description
	scope := value.Scope{"vars": map[string]any{"number": 5, "word": "green"}}
	for _, tc := range []struct {
		inputs map[string]any
		want   string
	}{
		{map[string]any{"case": "{{ .vars.number }}"}, "fixture.case: expected a string"},
		{map[string]any{"case": "echo", "color": "{{ .vars.word }}"}, "fixture.color: expected one of red, blue"},
	} {
		r := (Runtime{}).External(context.Background(), "fixture", module, tc.inputs, scope, "/unused/helper")
		if r.Status != "failed" || r.FailureKind != "config" || r.Reason != tc.want || r.Stderr != "" {
			t.Errorf("%v: %+v", tc.inputs, r)
		}
	}
	if r := (Runtime{}).External(context.Background(), "fixture", module, map[string]any{"case": "echo", "color": "{{ .vars.word }}"}, value.Scope{"vars": map[string]any{"word": "red"}}, "/unused/helper"); r.Status != "succeeded" {
		t.Fatalf("an allowed value: %+v", r)
	}
}

func TestExternalDoesNotLaunchInvalidRequest(t *testing.T) {
	t.Parallel()
	module := processModule(t)
	r := (Runtime{}).External(context.Background(), "fixture", module, map[string]any{"data": strings.Repeat("x", moduleio.MaxRequestBytes)}, value.Scope{}, "/unused/helper")
	if r.Status != "failed" || r.FailureKind != "config" || !strings.Contains(r.Reason, "exceeds") {
		t.Fatalf("oversized request: %+v", r)
	}
	os.Remove(filepath.Join(module.Directory, ModuleExecutableFile))
	r = (Runtime{}).External(context.Background(), "fixture", module, nil, value.Scope{}, "/unused/helper")
	if r.Status != "failed" || r.Change != "unchanged" {
		t.Fatalf("launch failure claimed effects: %+v", r)
	}
}

// An executable gets the HOME and LOGNAME that a primitive's command gets.
func TestExternalSeesHomeAndLogname(t *testing.T) {
	t.Parallel()
	r := (Runtime{}).External(context.Background(), "fixture", processModule(t), map[string]any{"case": "environment"}, value.Scope{}, "/unused/helper")
	want := map[string]string{}
	for _, entry := range primitiveCommandEnvironment() {
		name, value, _ := strings.Cut(entry, "=")
		want[name] = value
	}
	if r.Status != "succeeded" || r.ModuleOutputs["home"] != want["HOME"] || r.ModuleOutputs["logname"] != "root" || want["HOME"] == "" {
		t.Fatalf("home %q, logname %q, want %q and root: %+v", r.ModuleOutputs["home"], r.ModuleOutputs["logname"], want["HOME"], r)
	}
}
