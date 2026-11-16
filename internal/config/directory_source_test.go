package config

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"mmx/internal/assets"
	"mmx/internal/modules"
)

func TestDirectorySourceSnapshot(t *testing.T) {
	project := fixture(t, `fleet_stages: [{id: setup, nodes: all, node_tasks: [{id: first, role: assets}, {id: second, role: assets}]}]`)
	base := filepath.Dir(project)
	role := filepath.Join(base, "roles/assets")
	write(t, filepath.Join(role, "main.yml"), "node_tasks: [{directory: {path: /srv/app, source: files/, file_permissions_mode: '0640'}}]\n")
	literal := "{{ .secrets.not_an_input }}"
	for _, name := range []string{".hidden", "nested/" + literal} {
		write(t, filepath.Join(role, "files", name), literal)
	}
	if err := os.Mkdir(filepath.Join(role, "files/empty"), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(role, "files/.hidden"), 0755); err != nil {
		t.Fatal(err)
	}
	p, _, err := Load(project, "example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(SecretNames(p)) != 0 {
		t.Fatal("literal source paths were interpreted as references")
	}
	entries := InputsOf[modules.Directory](p.FleetStages[0].NodeTasks[0].RoleNodeTasks[0]).Entries
	if len(entries) != 5 || entries[0].Path != "." || entries[1].Mode != 0755 {
		t.Fatal(entries)
	}
	frozen := t.TempDir()
	if err := FreezeAssets(context.Background(), &p, frozen, nil); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(role, "files")); err != nil {
		t.Fatal(err)
	}
	var saved Project
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if err := BindProjectAssets(&saved, frozen); err != nil {
		t.Fatal(err)
	}
	refs, err := RequiredAssets(saved.FleetStages[0].NodeTasks, nil)
	if err != nil || len(refs) != 1 {
		t.Fatalf("deduplication: %v %v", refs, err)
	}
	b, err := os.ReadFile(refs[0].Path)
	if err != nil || string(b) != literal {
		t.Fatal("lost frozen source", err)
	}
	entries = InputsOf[modules.Directory](saved.FleetStages[0].NodeTasks[0].RoleNodeTasks[0]).Entries
	names := []string{}
	for _, e := range entries {
		names = append(names, e.Path)
	}
	if !reflect.DeepEqual(names, []string{".", ".hidden", "empty", "nested", "nested/" + literal}) {
		t.Fatal(names)
	}
}

// A missing source directory refuses the playbook, naming the setting that
// changes that; with if_source_directory_is_missing: copy_nothing it is an
// empty source, and a source that appears later is copied as usual.
func TestDirectorySourceMissingCopiesNothingOnlyWhenAsked(t *testing.T) {
	project := fixture(t, "")
	base := filepath.Dir(project)
	load := func(directory string) (Project, error) {
		t.Helper()
		write(t, project, "project_name: test\ndefault_inventory: example\nfleet_stages: [{id: setup, nodes: all, node_tasks: [{directory: "+directory+"}]}]\n")
		p, _, err := Load(project, "example", "", nil)
		return p, err
	}
	if _, err := load("{path: /srv/modules, source: missing_modules}"); err == nil || !strings.Contains(err.Error(), "set if_source_directory_is_missing: copy_nothing") {
		t.Fatalf("a missing source must fail and name the setting: %v", err)
	}
	if _, err := load("{path: /srv/modules, source: missing_modules, if_source_directory_is_missing: fail}"); err == nil {
		t.Fatal("fail accepted a missing source")
	}
	p, err := load("{path: /srv/modules, source: missing_modules, if_source_directory_is_missing: copy_nothing}")
	if err != nil {
		t.Fatal(err)
	}
	entries := InputsOf[modules.Directory](p.FleetStages[0].NodeTasks[0]).Entries
	if !reflect.DeepEqual(entries, []modules.DirectoryEntry{{Path: ".", Directory: true, Mode: 0755}}) {
		t.Fatal(entries)
	}
	if err := FreezeAssets(context.Background(), &p, t.TempDir(), nil); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(base, "missing_modules/check.sh"), "#!/bin/sh\n")
	if p, err = load("{path: /srv/modules, source: missing_modules, if_source_directory_is_missing: copy_nothing}"); err != nil {
		t.Fatal(err)
	}
	if entries := InputsOf[modules.Directory](p.FleetStages[0].NodeTasks[0]).Entries; len(entries) != 2 || entries[1].Path != "check.sh" {
		t.Fatal("a source that exists is copied as usual", entries)
	}
	// Only a missing source counts as empty: a file in its place still fails.
	if err := os.RemoveAll(filepath.Join(base, "missing_modules")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(base, "missing_modules"), "not a directory\n")
	if _, err := load("{path: /srv/modules, source: missing_modules, if_source_directory_is_missing: copy_nothing}"); err == nil || !strings.Contains(err.Error(), "real directory") {
		t.Fatalf("a file in place of the source must fail: %v", err)
	}
	for directory, wants := range map[string]string{
		"{path: /srv/modules, source: missing_modules, if_source_directory_is_missing: skip}": "must be fail or copy_nothing",
		"{path: /srv/modules, if_source_directory_is_missing: copy_nothing}":                  "requires source",
	} {
		if _, err := load(directory); err == nil || !strings.Contains(err.Error(), wants) {
			t.Errorf("%s: want %q, got %v", directory, wants, err)
		}
	}
}

func TestDirectorySourceRejectsLocalTypes(t *testing.T) {
	for _, kind := range []string{"missing", "file root", "root link", "file link", "directory link", "fifo", "special bits"} {
		t.Run(kind, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "tree")
			if err := os.Mkdir(root, 0755); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "missing":
				os.Remove(root)
			case "file root":
				os.Remove(root)
				os.WriteFile(root, []byte("file"), 0644)
			case "root link":
				os.Remove(root)
				os.Symlink(t.TempDir(), root)
			case "file link":
				os.WriteFile(filepath.Join(root, "file"), nil, 0644)
				os.Symlink("file", filepath.Join(root, "link"))
			case "directory link":
				os.Symlink(t.TempDir(), filepath.Join(root, "link"))
			case "fifo":
				if err := syscall.Mkfifo(filepath.Join(root, "fifo"), 0600); err != nil {
					t.Fatal(err)
				}
			case "special bits":
				if err := os.Chmod(root, 0755|os.ModeSticky); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := modules.ReadDirectorySource(root+"/", false); err == nil {
				t.Fatal("accepted", kind)
			}
		})
	}
}

func TestDirectorySourceLayoutAndSchema(t *testing.T) {
	valid := modules.Directory{Path: "/target", Source: "files", Entries: []modules.DirectoryEntry{{Path: ".", Directory: true, Mode: 0755}, {Path: "a", Mode: 0644, Asset: &assets.Ref{}}}}
	for _, name := range []string{"missing root", "root file", "traversal", "absolute", "backslash", "duplicate", "missing parent", "file parent", "special mode", "file without asset", "directory with asset", "source recurse", "dynamic source"} {
		t.Run(name, func(t *testing.T) {
			d := valid
			d.Entries = append([]modules.DirectoryEntry(nil), valid.Entries...)
			switch name {
			case "missing root":
				d.Entries = d.Entries[1:]
			case "root file":
				d.Entries[0].Directory = false
				d.Entries[0].Asset = &assets.Ref{}
			case "traversal":
				d.Entries[1].Path = "../escape"
			case "absolute":
				d.Entries[1].Path = "/escape"
			case "backslash":
				d.Entries[1].Path = "a\\b"
			case "duplicate":
				d.Entries = append(d.Entries, d.Entries[1])
			case "missing parent":
				d.Entries[1].Path = "missing/file"
			case "file parent":
				d.Entries = append(d.Entries, modules.DirectoryEntry{Path: "a/file", Asset: &assets.Ref{}})
			case "special mode":
				d.Entries[1].Mode = 04755
			case "file without asset":
				d.Entries[1].Asset = nil
			case "directory with asset":
				d.Entries[0].Asset = &assets.Ref{}
			case "source recurse":
				d.Recurse = true
			case "dynamic source":
				d.Source = "{{ .vars.source }}"
			}
			if err := modules.ValidateDirectorySource(d); err == nil {
				t.Fatal("accepted", name)
			}
		})
	}
	for _, fields := range []string{"entries: []", "source: '{{ .vars.path }}'", "source: files, also_apply_to_everything_inside: true", "file_permissions_mode: '0600'"} {
		p := fixture(t, "fleet_stages: [{id: setup, nodes: all, node_tasks: [{directory: {path: /target, "+fields+"}}]}]")
		if _, _, err := Load(p, "example", "", nil); err == nil {
			t.Fatal("accepted", fields)
		}
	}
	valid.Entries[1].Asset = &assets.Ref{SHA256: strings.Repeat("a", 64), Size: 1}
	if _, err := RequiredAssets([]Task{{Module: "directory", ModuleInputs: &valid}}, nil); err != nil {
		t.Fatal(err)
	}
	valid.Entries[1].Asset.Size = -1
	if _, err := RequiredAssets([]Task{{Module: "directory", ModuleInputs: &valid}}, nil); err == nil {
		t.Fatal("accepted invalid asset")
	}
}
