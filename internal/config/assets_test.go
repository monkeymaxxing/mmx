package config

import (
	"slices"
	"strings"
	"testing"

	"mmx/internal/assets"
	"mmx/internal/modules"
)

// The files that a node keeps between runs: the files of modules that are not
// core, and the files from a web address of download and unpack_archive, also
// one that a template gives, whose web address the job that the node receives
// does not carry; not a file from the project, which may hold a secret.
func TestKeptFileAssets(t *testing.T) {
	ref := func(c string) assets.Ref { return assets.Ref{SHA256: strings.Repeat(c, 64), Size: 1} }
	program, templated, archive, key, script := ref("a"), ref("b"), ref("c"), ref("d"), ref("e")
	tasks := []Task{
		{Module: "download", ModuleInputs: &modules.File{URL: "https://downloads.example.com/app", SHA256: program.SHA256, Path: "/usr/local/bin/app", Mode: "0755", Asset: &program}},
		{Module: "download", ModuleInputs: &modules.File{URL: "{{ .vars.tool_url }}", SHA256: "{{ .vars.tool_sha256 }}", Path: "/usr/local/bin/tool", Mode: "0755",
			Variants: map[string]*modules.SourceVariant{"tool": {Asset: &templated, URL: "https://downloads.example.com/tool-1.2", SHA256: templated.SHA256}}}},
		{Module: "unpack_archive", ModuleInputs: &modules.Archive{URL: "https://downloads.example.com/app.tar.gz", SHA256: archive.SHA256, Path: "/srv/app/releases/1", Asset: &archive}},
		{Module: "file", ModuleInputs: &modules.File{Source: "files/key", LocalPath: "/project/files/key", Path: "/etc/app/key", Mode: "0600", Asset: &key}},
	}
	catalog := map[string]modules.Module{"greeting": {Kind: modules.KindProjectStarlark, Files: []modules.ModuleFile{{Path: modules.ModuleScriptFile, Mode: 0644, Asset: script}}}}
	refs, err := KeptFileAssets(tasks, catalog)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, r := range refs {
		got = append(got, r.SHA256)
	}
	if want := []string{program.SHA256, templated.SHA256, archive.SHA256, script.SHA256}; !slices.Equal(got, want) {
		t.Errorf("kept %q, want %q", got, want)
	}
}

// A node's job names the files of the fleet stage that the node keeps and
// that this job uses: also a file whose web address a template gives, which
// the node's own copy of the tasks does not carry, and not another node's.
func TestKeptFilesOfANodeJob(t *testing.T) {
	ref := func(c string) assets.Ref { return assets.Ref{SHA256: strings.Repeat(c, 64), Size: 1} }
	mine, theirs, key := ref("a"), ref("b"), ref("c")
	tasks := []Task{
		{Module: "download", ModuleInputs: &modules.File{URL: "{{ .vars.tool_url }}", SHA256: "{{ .vars.tool_sha256 }}", Path: "/usr/local/bin/tool", Mode: "0755",
			Variants: map[string]*modules.SourceVariant{
				"web1": {Asset: &mine, Nodes: []string{"web1"}, URL: "https://downloads.example.com/tool-1.2", SHA256: mine.SHA256},
				"web2": {Asset: &theirs, Nodes: []string{"web2"}, URL: "https://downloads.example.com/tool-1.3", SHA256: theirs.SHA256}}}},
		{Module: "file", ModuleInputs: &modules.File{Source: "files/key", LocalPath: "/project/files/key", Path: "/etc/app/key", Mode: "0600", Asset: &key}},
	}
	own, err := NodeTasksFor(tasks, "web1", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	kept, err := KeptFilesOfANodeJob(tasks, Job{NodeTasks: own})
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 1 || kept[0].SHA256 != mine.SHA256 {
		t.Errorf("kept %v, want only the file of web1's web address", kept)
	}
}
