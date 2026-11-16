package node

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"mmx/internal/assets"
	"mmx/internal/config"
	"mmx/internal/modules"
)

func TestModuleStagingKeepsFrozenAssetsSeparate(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(t.TempDir(), "program")
	contents := []byte("#!/bin/sh\nexit 0\n")
	if err := os.WriteFile(source, contents, 0700); err != nil {
		t.Fatal(err)
	}
	ref, err := assets.Snapshot(context.Background(), root, source)
	if err != nil {
		t.Fatal(err)
	}
	modules := map[string]modules.Module{
		"one": {Kind: modules.KindProjectExecutable, Executable: modules.ModuleExecutableFile, Files: []modules.ModuleFile{{Path: modules.ModuleExecutableFile, Mode: 0755, Asset: ref}, {Path: modules.ModuleDescriptionFile, Mode: 0640, Asset: ref}, {Path: "nested/support", Mode: 0640, Asset: ref}}},
		"two": {Kind: modules.KindProjectExecutable, Executable: modules.ModuleExecutableFile, Files: []modules.ModuleFile{{Path: modules.ModuleExecutableFile, Mode: 0700, Asset: ref}, {Path: modules.ModuleDescriptionFile, Mode: 0640, Asset: ref}}},
	}
	if err := stageModules(context.Background(), modules, root); err != nil {
		t.Fatal(err)
	}
	for _, module := range modules {
		if module.Directory == "" {
			t.Fatal("missing directory binding")
		}
		for _, file := range module.Files {
			path := filepath.Join(module.Directory, file.Path)
			data, err := os.ReadFile(path)
			if err != nil || string(data) != string(contents) {
				t.Fatal("staged bytes changed", err)
			}
			info, err := os.Stat(path)
			if err != nil || uint32(info.Mode().Perm()) != file.Mode {
				t.Fatal("staged mode changed", err)
			}
		}
	}
	if err := os.WriteFile(filepath.Join(modules["one"].Directory, "module_executable"), []byte("edited"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := assets.Verify(context.Background(), ref); err != nil {
		t.Fatal("module edited frozen bytes", err)
	}
	other, _ := os.ReadFile(filepath.Join(modules["two"].Directory, "module_executable"))
	if string(other) != string(contents) {
		t.Fatal("module edited another module's working file")
	}
}

func TestModuleAdmissionRejectsUnsafeCatalogs(t *testing.T) {
	s := Server{Root: t.TempDir(), RuntimeRoot: t.TempDir()}
	for _, modules := range []map[string]modules.Module{
		{},
		{"pair": {Executable: "../escape", Files: []modules.ModuleFile{{Path: "run", Mode: 0700}}}},
		{"pair": {Executable: "run", Files: []modules.ModuleFile{{Path: "run", Mode: 0700}, {Path: "../escape", Mode: 0600}}}},
		{"pair": {Executable: "run", Files: []modules.ModuleFile{{Path: "run", Mode: 04755}}}},
		{"pair": {Executable: "run", Files: []modules.ModuleFile{{Path: "run", Mode: 0700}}}},
	} {
		if err := s.verifyAssets([]config.Task{{Module: "pair"}}, modules, "unused"); err == nil {
			t.Fatal("accepted invalid or unverified module assets")
		}
	}
}
