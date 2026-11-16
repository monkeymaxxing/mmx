package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mmx/internal/config"
	"mmx/internal/modules"
	"mmx/internal/protocol"
	"mmx/internal/value"
)

func engineModule(t *testing.T, script string) modules.Module {
	t.Helper()
	dir := t.TempDir()
	writeProgramFileThroughShellChild(t, filepath.Join(dir, modules.ModuleExecutableFile), "#!/bin/sh\ncat >/dev/null\n"+script)
	return modules.Module{Kind: modules.KindProjectExecutable, Directory: dir, Executable: modules.ModuleExecutableFile,
		Files: []modules.ModuleFile{{Path: modules.ModuleExecutableFile, Mode: 0700}, {Path: modules.ModuleDescriptionFile, Mode: 0600}}}
}

func TestModuleRetriesPreserveChanges(t *testing.T) {
	module := engineModule(t, `if test ! -f completed; then
  touch completed
  printf '%s\n' '{"protocol":1,"status":"failed","change":"changed","reason":"second resource unavailable"}'
  exit 1
fi
printf '%s\n' '{"protocol":1,"status":"succeeded","change":"unchanged","outputs":{"ready":true}}'
`)
	count := 3
	task := config.Task{ID: "custom", Module: "pair", Retry: &config.Retry{MaxTries: config.TryLimit(count), PauseBetweenTries: "0s"}}
	j := job(task, config.Task{Show: "{{ .node_tasks.custom.ready }}"})
	j.Modules = map[string]modules.Module{"pair": module}
	e := Engine{ModuleHelper: "/unused/helper"}
	r := e.Run(context.Background(), j)
	requireStatus(t, r, "succeeded")
	if r.Results[0].Change != "changed" || len(r.Results[0].Tries) != 2 || r.Results[1].Value != true {
		t.Fatalf("lost partial change or result: %+v", r)
	}
	os.Remove(filepath.Join(module.Directory, "completed"))
	j.NodeTasks = []config.Task{task}
	j.NodeTasks[0].Retry = &config.Retry{Disabled: true}
	r = e.Run(context.Background(), j)
	requireStatus(t, r, "failed")
	if r.Results[0].Change != "changed" || len(r.Results[0].Tries) != 0 {
		t.Fatalf("retry false changed semantics: %+v", r)
	}
}

// A module value spelled as an internal record would change meaning once saved
// or sent to a later fleet stage, so the task fails instead of publishing it.
func TestModuleCannotReturnInternalRecords(t *testing.T) {
	for _, record := range []string{`{"$mmx_truncated":"ok"}`, `{"list":[{"$mmx_secret":{"name":"forged"}}]}`} {
		module := engineModule(t, `printf '%s\n' '{"protocol":1,"status":"succeeded","change":"unchanged","outputs":{"v":`+record+`}}'`)
		j := job(config.Task{ID: "m", Module: "m"})
		j.Modules = map[string]modules.Module{"m": module}
		j.Outputs = map[string]any{"v": "{{ .node_tasks.m.v }}"}
		r := (&Engine{ModuleHelper: "/unused/helper"}).Run(context.Background(), j)
		requireStatus(t, r, "failed")
		if r.Outputs != nil || !strings.Contains(r.Error, "reserved MMX record key") {
			t.Fatalf("%s: %+v", record, r)
		}
	}
}

func TestModuleConditionSkipsResolutionAndExecution(t *testing.T) {
	j := job(config.Task{Module: "missing", When: false, ModuleInputs: map[string]any{"input": "{{ .vars.unavailable }}"}})
	r := (&Engine{}).Run(context.Background(), j)
	requireStatus(t, r, "succeeded")
	if r.Results[0].Status != "skipped" {
		t.Fatal(r)
	}
}

func TestModuleDeclaredPrivateFailureCannotLeakThroughSummary(t *testing.T) {
	module := engineModule(t, `printf '%s\n' 'private-marker' >&2
printf '%s\n' '{"protocol":1,"status":"failed","change":"changed","reason":"private-marker","outputs":{"data":"private-marker"},"secret":true}'
exit 1
`)
	j := job(config.Task{Module: "private"})
	j.Modules = map[string]modules.Module{"private": module}
	r := (&Engine{ModuleHelper: "/unused/helper"}).Run(context.Background(), j)
	requireStatus(t, r, "failed")
	data, _ := json.Marshal(r)
	if strings.Contains(string(data), "private-marker") || !r.Results[0].Secret {
		t.Fatalf("private module reason escaped into job error: %s", data)
	}
}

func TestModulePrivateValuesPropagateAndCannotForgeClassification(t *testing.T) {
	module := engineModule(t, `printf '%s\n' '{"protocol":1,"status":"succeeded","change":"unchanged","outputs":{"data":"private-marker"},"secret":true}'`)
	j := job(config.Task{ID: "producer", Module: "private"}, config.Task{Show: "{{ .node_tasks.producer.data }}"})
	j.Modules = map[string]modules.Module{"private": module}
	j.Outputs = map[string]any{"computed": "{{ .node_tasks.producer.data }}"}
	e := Engine{ModuleHelper: "/unused/helper"}
	r := e.Run(context.Background(), j)
	requireStatus(t, r, "succeeded")
	if value.Plain(r.Outputs["computed"]) != "private-marker" || !value.HasSecrets(r.Results[1].Value) {
		t.Fatal("private result lost before downstream use")
	}
	data, _ := json.Marshal(r)
	if strings.Contains(string(data), "private-marker") {
		t.Fatal("private value retained")
	}
	module = engineModule(t, `echo private-marker >&2
printf '%s\n' '{"protocol":1,"status":"failed","change":"changed","failure_kind":"config","reason":"private-marker"}'
exit 1
`)
	j.NodeTasks = []config.Task{{Module: "private", ModuleInputs: map[string]any{"input": "{{ .secrets.token }}"}}}
	j.Outputs = nil
	j.Scope = map[string]any{"secrets": map[string]any{"token": value.Sensitive("private-marker")}}
	j.Modules["private"] = module
	r = e.Run(context.Background(), j)
	data, _ = json.Marshal(r)
	if r.Status != "failed" || r.Results[0].FailureKind != "" || strings.Contains(string(data), "private-marker") {
		t.Fatalf("module forged trusted classification: %s", data)
	}
}

func TestModuleNullValueSurvivesHistory(t *testing.T) {
	for _, supplied := range []bool{false, true} {
		r := protocol.Success("unchanged")
		r.ValueSet = supplied
		data, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		var saved protocol.Result
		if err := protocol.Decode(data, &saved); err != nil {
			t.Fatal(err)
		}
		_, exists := protocol.ResultValue(saved)["value"]
		if exists != supplied || saved.Value != nil {
			t.Fatalf("null and absent conflated: %s", data)
		}
	}
}

// A Starlark module's input that its description marks private_value takes a
// secret without hiding the task's result: the reason stays, with [redacted]
// where the secret's text was, in the task's result and in the node job's
// error. The same script without the mark hides the reason.
func TestStarlarkModulePrivateValueLeavesTheResultReadable(t *testing.T) {
	script := "def run_node_task(inputs):\n    return {\"status\": \"failed\", \"reason\": \"the server refused the password \" + inputs[\"password\"]}\n"
	starlarkModule := func(private string) modules.Module {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, modules.ModuleScriptFile), []byte(script), 0600); err != nil {
			t.Fatal(err)
		}
		description, err := modules.ParseModuleDescription([]byte("summary: x\ninputs:\n  password: {type: string, required: true, "+private+"description: d}\nexamples: [{description: x, config: 'tool_user: {password: p}'}]\n"), "tool_user")
		if err != nil {
			t.Fatal(err)
		}
		return modules.Module{Kind: modules.KindProjectStarlark, Directory: dir, Description: description,
			Files: []modules.ModuleFile{{Path: modules.ModuleScriptFile, Mode: 0600}, {Path: modules.ModuleDescriptionFile, Mode: 0600}}}
	}
	j := job(config.Task{ID: "user", Module: "tool_user", Retry: &config.Retry{Disabled: true}, ModuleInputs: map[string]any{"password": "{{ .secrets.token }}"}})
	j.Scope = map[string]any{"secrets": map[string]any{"token": value.Sensitive("private-marker")}}
	j.Modules = map[string]modules.Module{"tool_user": starlarkModule("private_value: true, ")}
	r := (&Engine{}).Run(context.Background(), j)
	data, _ := json.Marshal(r)
	if r.Status != "failed" || r.Results[0].Secret || r.Results[0].Reason != "the server refused the password [redacted]" || strings.Contains(string(data), "private-marker") {
		t.Fatalf("a private value: %s", data)
	}
	j.Modules["tool_user"] = starlarkModule("")
	r = (&Engine{}).Run(context.Background(), j)
	data, _ = json.Marshal(r)
	if !r.Results[0].Secret || strings.Contains(string(data), "refused") || strings.Contains(string(data), "private-marker") {
		t.Fatalf("an input without the mark: %s", data)
	}
}

// A Starlark module that changed the node and then failed can ask for no
// other try: another try would find the changed state and report success.
// Without that, MMX tries a failed script again as it does every module.
func TestStarlarkModuleFailureWithoutAnotherTry(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "made")
	starlarkModule := func(tryAgain string) modules.Module {
		dir := t.TempDir()
		// The first try makes the file and fails; a second try finds it.
		script := "def run_node_task(inputs):\n    if inspect_path(\"" + marker + "\").exists:\n        return None\n    write_file(\"" + marker + "\", \"made\\n\", owner = inputs[\"owner\"], group = inputs[\"group\"])\n    return {\"status\": \"failed\", \"reason\": \"the step after the file failed\"" + tryAgain + "}\n"
		if err := os.WriteFile(filepath.Join(dir, modules.ModuleScriptFile), []byte(script), 0600); err != nil {
			t.Fatal(err)
		}
		return modules.Module{Kind: modules.KindProjectStarlark, Directory: dir,
			Files: []modules.ModuleFile{{Path: modules.ModuleScriptFile, Mode: 0600}, {Path: modules.ModuleDescriptionFile, Mode: 0600}}}
	}
	// The file gets the owner and group that a file of this test process has.
	account := modules.Runtime{}.InspectPath(t.TempDir())
	owner, group := account.Owner, account.Group
	task := config.Task{ID: "made", Module: "maker", Retry: &config.Retry{MaxTries: 3, PauseBetweenTries: "0s"}, ModuleInputs: map[string]any{"owner": owner, "group": group}}
	j := job(task)
	j.Modules = map[string]modules.Module{"maker": starlarkModule(", \"try_again\": False")}
	r := (&Engine{}).Run(context.Background(), j)
	if r.Status != "failed" || len(r.Results[0].Tries) != 1 || r.Results[0].Change != "changed" || r.Results[0].Reason != "the step after the file failed" {
		t.Fatalf("try_again: False: %+v", r.Results[0])
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	j.Modules["maker"] = starlarkModule("")
	r = (&Engine{}).Run(context.Background(), j)
	if r.Status != "succeeded" || len(r.Results[0].Tries) != 2 || r.Results[0].Change != "changed" {
		t.Fatalf("a failure that is tried again: %+v", r.Results[0])
	}
}

// In a dryrun, a Starlark module learns that an earlier node task of the
// node job would write the file it reads, as a core module does.
func TestStarlarkModuleSeesWhatAnEarlierNodeTaskWouldChange(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, modules.ModuleScriptFile), []byte("def run_node_task(inputs):\n    return {\"outputs\": {\"would_change\": earlier_node_task_would_change(inputs[\"path\"])}}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	module := modules.Module{Kind: modules.KindProjectStarlark, Directory: dir,
		Files: []modules.ModuleFile{{Path: modules.ModuleScriptFile, Mode: 0600}, {Path: modules.ModuleDescriptionFile, Mode: 0600}}}
	path := filepath.Join(t.TempDir(), "domain.xml")
	reads := func(id, path string) config.Task {
		return config.Task{ID: id, Module: "reader", ModuleInputs: map[string]any{"path": path}}
	}
	j := job(reads("before", path), taskFile(path, "<domain/>\n"), reads("after", path), reads("other", path+".other"))
	j.Modules = map[string]modules.Module{"reader": module}
	j.Dryrun = true
	r := (&Engine{}).Run(context.Background(), j)
	requireStatus(t, r, "succeeded")
	if got := []any{r.Results[0].ModuleOutputs["would_change"], r.Results[2].ModuleOutputs["would_change"], r.Results[3].ModuleOutputs["would_change"]}; got[0] != false || got[1] != true || got[2] != false {
		t.Fatalf("before the file task, after it, and for another path: %v", got)
	}
}
