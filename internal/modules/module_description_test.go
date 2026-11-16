package modules

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

const greetingDescription = `summary: Makes sure a file holds a greeting for one person.
inputs:
  path:
    type: string
    required: true
    description: The absolute path of the file that holds the greeting.
  person:
    type: string
    required: true
    description: The name of the person to greet, such as Barbara.
  style:
    type: string
    required: false
    allowed_values: [plain, formal]
    description: How the greeting reads.
outputs:
  greeting:
    type: string
    description: The greeting the file holds.
examples:
  - description: Greet Barbara in /tmp/greeting.txt.
    config: |
      greeting:
        path: /tmp/greeting.txt
        person: Barbara
`

func TestModuleDescriptionChecksCallsAndResults(t *testing.T) {
	t.Parallel()
	d, err := ParseModuleDescription([]byte(greetingDescription), "greeting")
	if err != nil {
		t.Fatal(err)
	}
	for inputs, want := range map[string]string{
		"path: /tmp/x\nperson: Barbara":              "",
		"path: /tmp/x\nperson: '{{ .vars.person }}'": "",
		"path: /tmp/x":                               "person: the input person is required: The name of the person",
		"path: /tmp/x\nperson: Barbara\nage: 3":      `unknown input "age"`,
		"path: /tmp/x\nperson: 3":                    "person: expected a string",
		"path: /tmp/x\nperson: Barbara\nstyle: loud": "style: expected one of plain, formal",
	} {
		var parsed map[string]any
		if err := decodeYAML(inputs, &parsed); err != nil {
			t.Fatal(err)
		}
		problem := d.InputsProblem("greeting", parsed)
		if (problem == nil) != (want == "") || problem != nil && !strings.Contains(problem.Error(), want) {
			t.Errorf("%q: got %v, want %q", inputs, problem, want)
		}
	}
	if err := d.OutputsProblem("greeting", map[string]any{"greeting": "Hello, Barbara!\n"}); err != nil {
		t.Fatal(err)
	}
	if err := d.OutputsProblem("greeting", map[string]any{"farewell": "bye"}); err == nil || !strings.Contains(err.Error(), "does not list") {
		t.Fatal(err)
	}
	if err := d.OutputsProblem("greeting", map[string]any{"greeting": 3}); err == nil {
		t.Fatal("accepted an output of the wrong type")
	}
}

func TestModuleDescriptionRefusesInvalidFiles(t *testing.T) {
	t.Parallel()
	for text, want := range map[string]string{
		"inputs: {}\nexamples: [{description: x, config: 'greeting: {}'}]": "summary is required",
		"summary: x\nexamples: []": "examples is required",
		"summary: x\ninputs: {Path: {type: string, required: true, description: d}}\nexamples: [{description: x, config: 'greeting: {}'}]":                                     "the name of an input",
		"summary: x\ninputs: {path: {type: text, required: true, description: d}}\nexamples: [{description: x, config: 'greeting: {}'}]":                                       "type: expected one of",
		"summary: x\ninputs: {path: {type: string, description: d}}\nexamples: [{description: x, config: 'greeting: {}'}]":                                                     "the required field must be set",
		"summary: x\noutputs: {status: {type: string, description: d}}\nexamples: [{description: x, config: 'greeting: {}'}]":                                                  "every result already has status",
		"summary: x\nexamples: [{description: x, config: 'other: {}'}]":                                                                                                        "does not name the module greeting",
		"summary: x\ninputs: {path: {type: string, required: true, description: d}}\nexamples: [{description: x, config: 'greeting: {}'}]":                                     "greeting.path: the input path is required: d",
		"summary: x\nused_as: check\nexamples: [{description: x, config: 'greeting: {}'}]":                                                                                     `unknown field "used_as"`,
		"summary: x\ninputs: {password: {type: string, required: false, private_value: yes please, description: d}}\nexamples: [{description: x, config: 'greeting: {}'}]":     "private_value: expected true or false",
		"summary: x\ninputs: {passwords: {type: list_of_strings, required: false, private_value: true, description: d}}\nexamples: [{description: x, config: 'greeting: {}'}]": "only an input of the type string can be a private value",
	} {
		if _, err := ParseModuleDescription([]byte(text), "greeting"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", text, err, want)
		}
	}
}

// TestModuleDescriptionFileLimit reads a description that holds exactly the
// file limit, padded with spaces after its summary, which YAML drops, and one
// a byte longer.
func TestModuleDescriptionFileLimit(t *testing.T) {
	t.Parallel()
	for _, length := range []int{moduleDescriptionLimit, moduleDescriptionLimit + 1} {
		spacesAfterSummary := strings.Repeat(" ", length-len(greetingDescription))
		text := strings.Replace(greetingDescription, "\n", spacesAfterSummary+"\n", 1)
		directory := t.TempDir()
		if err := os.WriteFile(filepath.Join(directory, ModuleDescriptionFile), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := ReadModuleDescription(directory, "greeting")
		if length <= moduleDescriptionLimit && err != nil {
			t.Errorf("%d bytes: got %v, want the description", length, err)
		}
		if length > moduleDescriptionLimit && (err == nil || !strings.Contains(err.Error(), "more than 256 KiB")) {
			t.Errorf("%d bytes: got %v, want the limit error", length, err)
		}
	}
}

func TestModuleDescriptionReadsAPrivateValue(t *testing.T) {
	t.Parallel()
	d, err := ParseModuleDescription([]byte("summary: x\ninputs:\n  name: {type: string, required: true, description: d}\n  password: {type: string, required: false, private_value: true, description: d}\nexamples: [{description: x, config: 'greeting: {name: shop}'}]"), "greeting")
	if err != nil {
		t.Fatal(err)
	}
	if name, _ := d.input("name"); name.PrivateValue {
		t.Error("an input without private_value is private")
	}
	if password, _ := d.input("password"); !password.PrivateValue {
		t.Error("private_value: true was not read")
	}
}

func decodeYAML(text string, v any) error {
	return yaml.Unmarshal([]byte(text), v)
}
