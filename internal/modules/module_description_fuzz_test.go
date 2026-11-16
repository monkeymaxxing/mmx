package modules

import (
	_ "embed"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// FuzzModuleDescriptionFile feeds untrusted bytes to the reader of
// module_description.yml. Up to the file limit, the file reader and the text
// parser must agree, and a description that reads without error must keep
// the rules the parser promises. Above the limit, the file reader must refuse
// the file; TestModuleDescriptionFileLimit checks the exact limit.

// requireValidDeclaredEntry fails the test unless one declared input or output
// has a valid name, not yet used by another entry of its kind, and a known type.
func requireValidDeclaredEntry(t *testing.T, entryNamesSeen map[string]bool, entryKind, entryName, entryType string) {
	t.Helper()
	if !ModuleName.MatchString(entryName) {
		t.Fatalf("accepted the %s name %q", entryKind, entryName)
	}
	if entryNamesSeen[entryName] {
		t.Fatalf("accepted the %s name %q twice", entryKind, entryName)
	}
	entryNamesSeen[entryName] = true
	if !slices.Contains(ModuleValueTypes, entryType) {
		t.Fatalf("%s %s: accepted the type %q", entryKind, entryName, entryType)
	}
}

// greetingDescriptionFile is a copy of the greeting example's module
// description. It is built into the test program, because the Go tests also
// run inside Linux guests that have only the test program and no source tree.
//
//go:embed testdata/fuzz_seeds/greeting_module_description.yml
var greetingDescriptionFile []byte

func FuzzModuleDescriptionFile(f *testing.F) {
	f.Add(greetingDescriptionFile)
	f.Add([]byte(greetingDescription))
	f.Add([]byte("summary: x\ninputs:\n  name: {type: string, required: true, description: d}\n  password: {type: string, required: false, private_value: true, description: d}\n  port: {type: integer, required: false, allowed_values: [80, 443], description: d}\nexamples: [{description: x, config: 'greeting: {name: shop}'}]"))
	f.Add([]byte("summary: [x\nexamples: [{description: x, config: 'greeting: {}'}]"))
	f.Add([]byte("summary: x\nsummary: y\nexamples: [{description: x, config: 'greeting: {}'}]"))
	f.Add([]byte("summary: x\nused_as: check\nexamples: [{description: x, config: 'greeting: {}'}]"))
	f.Add([]byte("summary: x\ninputs: {path: {type: text, required: true, description: d}}\noutputs: {status: {type: string, description: d}}\nexamples: [{description: x, config: 'greeting: {}'}]"))
	f.Add([]byte("summary: x\ninputs:\n  path: &path_input {type: string, required: true, description: d}\n  other_path: *path_input\nexamples: [{description: x, config: 'greeting: {path: /a, other_path: /b}'}]"))
	f.Add([]byte("summary: x\nexamples: []"))
	f.Add([]byte("summary: x\noutputs: {status: {type: string, description: d}}\nexamples: [{description: x, config: 'greeting: {}'}]"))
	f.Fuzz(func(t *testing.T, moduleDescriptionFileContents []byte) {
		moduleDirectory := t.TempDir()
		if err := os.WriteFile(filepath.Join(moduleDirectory, ModuleDescriptionFile), moduleDescriptionFileContents, 0600); err != nil {
			t.Fatal(err)
		}
		descriptionFromFile, fileReaderError := ReadModuleDescription(moduleDirectory, "greeting")
		if fileReaderError != nil && descriptionFromFile != nil {
			t.Fatalf("the file reader returned a description together with the error %v", fileReaderError)
		}
		if len(moduleDescriptionFileContents) > moduleDescriptionLimit {
			if fileReaderError == nil {
				t.Fatalf("the file reader accepted %d bytes, more than the limit of %d", len(moduleDescriptionFileContents), moduleDescriptionLimit)
			}
			return
		}
		// The text parser may return a partial description with its error.
		descriptionFromText, textParserError := ParseModuleDescription(moduleDescriptionFileContents, "greeting")
		if (fileReaderError == nil) != (textParserError == nil) {
			t.Fatalf("the file reader returned the error %v, but the text parser returned the error %v", fileReaderError, textParserError)
		}
		if textParserError != nil {
			return
		}
		if !reflect.DeepEqual(descriptionFromFile, descriptionFromText) {
			t.Fatalf("the file reader returned %+v, but the text parser returned %+v", descriptionFromFile, descriptionFromText)
		}
		if strings.TrimSpace(descriptionFromText.Summary) == "" {
			t.Fatal("accepted a blank summary")
		}
		if len(descriptionFromText.Examples) == 0 {
			t.Fatal("accepted a description without examples")
		}
		for exampleIndex, example := range descriptionFromText.Examples {
			if strings.TrimSpace(example.Description) == "" || strings.TrimSpace(example.Config) == "" {
				t.Fatalf("examples.%d: accepted a blank description or config: %+v", exampleIndex, example)
			}
		}
		inputNamesSeen := map[string]bool{}
		for _, input := range descriptionFromText.Inputs {
			requireValidDeclaredEntry(t, inputNamesSeen, "input", input.Name, input.Type)
		}
		outputNamesSeen := map[string]bool{}
		for _, output := range descriptionFromText.Outputs {
			requireValidDeclaredEntry(t, outputNamesSeen, "output", output.Name, output.Type)
			if slices.Contains(reservedOutputNames, output.Name) {
				t.Fatalf("accepted the reserved output name %q", output.Name)
			}
		}
	})
}
