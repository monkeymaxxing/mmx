package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	modulepages "mmx/docs/reference/modules"
	"mmx/internal/config"
	"mmx/internal/modules"
	"mmx/internal/protocol"
	"mmx/internal/release"
)

// Official extra modules that another MMX release shipped are refused with
// exit 4 and the message that names both versions, before validate reads
// the project, before dryrun contacts a node, and before describe-module
// lists or describes anything.
func TestOfficialExtraModulesFromAnotherReleaseAreRefused(t *testing.T) {
	initializedProject(t)
	releaseDirectory := t.TempDir()
	t.Setenv("MMX_RUNNER_DIR", releaseDirectory)
	extras := filepath.Join(releaseDirectory, "official_extra_modules")
	writeProjectFile(t, filepath.Join(extras, config.OfficialExtraModulesVersionFile), "9.9.9\n")
	want := "the official extra modules in " + extras + " were shipped with MMX 9.9.9, but the mmx program is version " + release.Version + "; reinstall the mmx program and official_extra_modules/ from a single MMX release"
	for _, args := range [][]string{{"validate"}, {"validate", "example"}, {"dryrun", "example"}, {"describe-module"}, {"describe-module", "file"}} {
		if _, code, err := captureCommand(t, args...); code != 4 || err == nil || err.Error() != want {
			t.Errorf("%v: got %d %v, want 4 %q", args, code, err, want)
		}
	}
}

func describeOutput(t *testing.T, args ...string) (int, string, error) {
	t.Helper()
	stdout, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = stdout
	code, err := run(context.Background(), append([]string{"describe-module"}, args...))
	os.Stdout = saved
	stdout.Close()
	b, _ := os.ReadFile(stdout.Name())
	return code, string(b), err
}

func TestDescribeListsEntriesAndProjectModules(t *testing.T) {
	t.Setenv("MMX_RUNNER_DIR", t.TempDir())
	dir := t.TempDir()
	t.Chdir(dir)
	files := map[string]string{
		"mmx_project_settings.yml":                      "project_name: test\ndefault_inventory: example\n",
		"external_modules/probe/module.star":            "def run_node_task(inputs):\n    return None\n",
		"external_modules/probe/module_description.yml": "summary: Probes the node.\nexamples: [{description: x, config: 'probe: {}'}]\n",
		"external_modules/probe/README.md":              "# `probe`\n\nThe page of the probe module.\n",
	}
	for path, text := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, path)), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, path), []byte(text), 0644); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := config.Descriptions()
	if err != nil {
		t.Fatal(err)
	}
	code, human, err := describeOutput(t)
	if code != 0 || err != nil {
		t.Fatal(code, err)
	}
	for _, d := range entries {
		if !strings.Contains(human, "  "+d.Name+" ") || !strings.Contains(human, d.Summary) {
			t.Fatal(d.Name, human)
		}
	}
	if !strings.Contains(human, "The project's own modules\n  probe ") || !strings.Contains(human, "Core modules (built into MMX), by topic\n") {
		t.Fatal(human)
	}
	// The core modules are grouped by topic, in the order of the topics.
	position := 0
	for _, topic := range modules.CoreModuleTopics {
		found := strings.Index(human[position:], "\n"+topic+"\n")
		if found < 0 {
			t.Fatalf("topic %q is missing or out of order in\n%s", topic, human)
		}
		position += found + len(topic) + 2
		for _, m := range modules.CoreModules() {
			if m.Description.Topic == topic && !strings.HasPrefix(human[position:], "  ") {
				t.Fatalf("topic %q has no module below it", topic)
			}
		}
	}
	if code, _, err := describeOutput(t, "--reference-only"); code != 4 || err == nil || !strings.Contains(err.Error(), "needs a module name") {
		t.Fatal(code, err)
	}
	code, out, err := describeOutput(t, "--json")
	var list struct {
		SchemaVersion int `json:"schema_version"`
		Entries       []struct{ Name, Kind, Topic, Summary string }
	}
	if code != 0 || err != nil || json.Unmarshal([]byte(out), &list) != nil {
		t.Fatal(code, err, out)
	}
	last := list.Entries[len(list.Entries)-1]
	if list.SchemaVersion != 1 || len(list.Entries) != len(entries)+1 || last.Name != "probe" || last.Kind != "project_starlark" {
		t.Fatal(out)
	}
	for _, entry := range list.Entries {
		if (entry.Kind == modules.KindCore) != (entry.Topic != "") {
			t.Fatalf("%s of kind %s has the topic %q", entry.Name, entry.Kind, entry.Topic)
		}
	}
	code, out, err = describeOutput(t, "probe", "--json")
	var module struct {
		config.NonCoreModule
		Page *string `json:"page"`
	}
	if code != 0 || err != nil || json.Unmarshal([]byte(out), &module) != nil || module.Kind != "project_starlark" || module.Directory != "external_modules/probe" || module.Summary != "Probes the node." ||
		module.Page == nil || *module.Page != files["external_modules/probe/README.md"] {
		t.Fatal(code, err, out)
	}
	// The project's own module: its reference, then its README.md.
	code, human, err = describeOutput(t, "probe")
	if code != 0 || err != nil || !strings.HasPrefix(human, "probe: the project's own module, Starlark, in external_modules/probe/\nProbes the node.") ||
		!strings.HasSuffix(human, "\n\n"+files["external_modules/probe/README.md"]) {
		t.Fatal(code, err, human)
	}
	code, reference, err := describeOutput(t, "probe", "--reference-only")
	if code != 0 || err != nil || strings.Contains(reference, "The page of the probe module.") || !strings.HasPrefix(human, reference) {
		t.Fatal(code, err, reference)
	}
	code, out, err = describeOutput(t, "probe", "--json", "--reference-only")
	if code != 0 || err != nil || strings.Contains(out, `"page"`) {
		t.Fatal(code, err, out)
	}
}

func TestDescribeShowsOneEntryAsTextAndJSON(t *testing.T) {
	code, human, err := describeOutput(t, "apt_package", "--reference-only")
	if code != 0 || err != nil {
		t.Fatal(code, err)
	}
	for _, want := range []string{
		"apt_package: core module, built into MMX\napt packages on Debian and Ubuntu",
		"Fields\n  packages",
		"list of mappings   required; one entry per package:",
		"  upgrade_all_packages_within_their_version_rules",
		"newest_when_first_installed",
		"newest_on_every_run",
		"Example\n  - apt_package:\n      packages:\n        - name: nginx\n          version: newest_when_first_installed\n",
		"Result: status, change, value\n",
		"Dryrun: supported;",
		"Retry: bounded automatic retries by default",
	} {
		if !strings.Contains(human, want) {
			t.Fatalf("missing %q in\n%s", want, human)
		}
	}
	code, out, err := describeOutput(t, "--json", "shell")
	var entry modules.Description
	if code != 0 || err != nil || json.Unmarshal([]byte(out), &entry) != nil {
		t.Fatal(code, err, out)
	}
	if entry.Name != "shell" || entry.Fields[0].Name != "script" || !entry.Fields[0].Required || !strings.HasPrefix(entry.Retry, "runs once") {
		t.Fatal(out)
	}
	if entry.Topic != modules.TopicCommandsChecksAndReboot {
		t.Fatal(out)
	}
	for _, args := range [][]string{{"lineinfile"}, {"apt_package", "file"}, {"missing"}} {
		if code, _, err := describeOutput(t, args...); code != 4 || err == nil {
			t.Fatal(args, code, err)
		}
	}
}

// A core module prints its generated page, without the comment line that
// says it is generated, and its reference when it has no page yet. The JSON
// entry carries the page in the field page.
func TestDescribePrintsTheWholePageOfACoreModule(t *testing.T) {
	for _, m := range modules.CoreModules() {
		page, hasPage := modulepages.CoreModulePage(m.Name)
		_, reference, _ := describeOutput(t, m.Name, "--reference-only")
		code, human, err := describeOutput(t, m.Name)
		if code != 0 || err != nil {
			t.Fatal(m.Name, code, err)
		}
		want := reference
		if hasPage {
			want = page
			if !strings.HasPrefix(page, "# `"+m.Name+"`\n") || strings.Contains(page, "<!-- Generated") {
				t.Fatalf("the page of %s does not start with its title:\n%s", m.Name, page)
			}
		}
		if human != want {
			t.Fatalf("describe-module %s printed\n%s\nwant\n%s", m.Name, human, want)
		}
		code, out, err := describeOutput(t, m.Name, "--json")
		var entry struct {
			Name string
			Kind string  `json:"kind"`
			Page *string `json:"page"`
		}
		if code != 0 || err != nil || json.Unmarshal([]byte(out), &entry) != nil || entry.Name != m.Name || entry.Kind != "core" || (entry.Page != nil) != hasPage || (hasPage && *entry.Page != page) {
			t.Fatal(m.Name, code, err, out)
		}
	}
}

// A node task form prints its reference and the section of the configuration
// specification that explains it.
func TestDescribeNamesTheSectionOfANodeTaskForm(t *testing.T) {
	for _, form := range []string{"task", "task_block", "role", "show", "set_vars"} {
		_, reference, _ := describeOutput(t, form, "--reference-only")
		code, human, err := describeOutput(t, form)
		if code != 0 || err != nil || !strings.HasPrefix(human, reference) ||
			human[len(reference):] != "\nThe configuration specification explains "+form+" in its section "+nodeTaskFormSections[form]+".\n" {
			t.Fatal(form, code, err, human)
		}
		if _, out, _ := describeOutput(t, form, "--json"); strings.Contains(out, `"page"`) || !strings.Contains(out, `"kind": "node_task_form"`) {
			t.Fatal(form, out)
		}
	}
}

// Every listed result field is one a later task can read.
func TestDescribedResultsAreTaskResultFields(t *testing.T) {
	fields := protocol.ResultValue(protocol.Result{ValueSet: true, HTTPStatusCode: 200, PublicKey: "ssh-ed25519 AAAA", Content: ""})
	entries, err := config.Descriptions()
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range entries {
		for _, name := range d.Results {
			if _, ok := fields[strings.Split(name, ".")[0]]; !ok {
				t.Errorf("%s lists result %s, which node_tasks do not expose", d.Name, name)
			}
		}
	}
}

// An official extra module is described from its files beside mmx, and an
// input that its description marks private_value says what that means.
func TestDescribeShowsAPrivateValueOfAnOfficialExtraModule(t *testing.T) {
	repository, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repository, "official_extra_modules", "mongodb_user")); err != nil {
		t.Skip("the test binary runs outside the repository, as on a test node")
	}
	t.Setenv("MMX_RUNNER_DIR", repository)
	readme, err := os.ReadFile(filepath.Join(repository, "official_extra_modules", "mongodb_user", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if code, page, err := describeOutput(t, "mongodb_user"); code != 0 || err != nil || page != string(readme) {
		t.Fatal("describe-module mongodb_user does not print its README.md", code, err)
	}
	code, human, err := describeOutput(t, "mongodb_user", "--reference-only")
	if code != 0 || err != nil {
		t.Fatal(code, err)
	}
	password := ""
	for _, line := range strings.Split(human, "\n") {
		if strings.HasPrefix(line, "  password ") {
			password = line
		}
	}
	if !strings.Contains(human, "mongodb_user: official extra module, shipped with MMX") ||
		!strings.Contains(password, "optional; a private value: a secret in the input does not hide the node task's result; when the input holds a secret, MMX shows [redacted] wherever the result's text holds the input's value; ") {
		t.Fatalf("the line of password: %q in\n%s", password, human)
	}
	code, out, err := describeOutput(t, "--json", "mongodb_user")
	var described struct {
		config.NonCoreModule
		Page string `json:"page"`
	}
	if code != 0 || err != nil || json.Unmarshal([]byte(out), &described) != nil || described.Page != string(readme) {
		t.Fatal(code, err, out)
	}
	private := map[string]bool{}
	for _, input := range described.Inputs {
		private[input.Name] = input.PrivateValue
	}
	if !private["password"] || !private["login_password"] || private["name"] {
		t.Fatalf("private values in the JSON: %v", private)
	}
}
