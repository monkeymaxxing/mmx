package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"mmx/internal/assets"
	"mmx/internal/config"
	"mmx/internal/modules"
	"mmx/internal/value"
)

func TestSourceRemovalAndLinkActivationRecovery(t *testing.T) {
	for _, kind := range []string{"source", "download", "remove", "link", "delete-unlisted", "delete-directory", "create-missing-directory"} {
		t.Run(kind, func(t *testing.T) {
			f := newServiceFixture(t)
			var initial, next config.Task
			loaded := "A"
			switch kind {
			case "source":
				initial = config.Task{Module: "file", ModuleInputs: &modules.File{Mode: "0644", Path: f.path, Source: "frozen", Asset: fileAsset(t, []byte("A"))}}
				next = config.Task{Module: "file", ModuleInputs: &modules.File{Mode: "0644", Path: f.path, Source: "frozen", Asset: fileAsset(t, []byte("B"))}}
				loaded = "B"
			case "download":
				makeTask := func(data string) config.Task {
					ref := fileAsset(t, []byte(data))
					return config.Task{Module: "file", ModuleInputs: &modules.File{Mode: "0644", URL: "https://example.com/config", SHA256: ref.SHA256, Path: f.path, Asset: ref}}
				}
				initial, next = makeTask("A"), makeTask("B")
				loaded = "B"
			case "remove":
				path := filepath.Join(f.dir, "extra")
				put(t, path, "obsolete")
				initial = taskFile(path, "obsolete")
				next = config.Task{Module: "file", ModuleInputs: &modules.File{Path: path, State: "absent"}}
			case "link":
				put(t, filepath.Join(f.dir, "a"), "A")
				put(t, filepath.Join(f.dir, "b"), "B")
				if err := os.Remove(f.path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("a", f.path); err != nil {
					t.Fatal(err)
				}
				initial = config.Task{Module: "symlink", ModuleInputs: &modules.Symlink{Path: f.path, Target: "a"}}
				next = config.Task{Module: "symlink", ModuleInputs: &modules.Symlink{Path: f.path, Target: "b"}}
				loaded = "B"
			case "delete-unlisted":
				// A deletion alone refreshes the service.
				releases := filepath.Join(f.dir, "releases")
				for _, dir := range []string{"1.0/bin", "2.0"} {
					if err := os.MkdirAll(filepath.Join(releases, dir), 0755); err != nil {
						t.Fatal(err)
					}
				}
				initial = config.Task{Module: "directory", ModuleInputs: &modules.Directory{Path: releases, DeleteEverythingInsideExcept: []string{"1.0", "2.0"}}}
				next = config.Task{Module: "directory", ModuleInputs: &modules.Directory{Path: releases, DeleteEverythingInsideExcept: []string{"2.0"}}}
			case "delete-directory":
				old := filepath.Join(f.dir, "old-api")
				if err := os.MkdirAll(filepath.Join(old, "data"), 0755); err != nil {
					t.Fatal(err)
				}
				initial = config.Task{Module: "directory", ModuleInputs: &modules.Directory{Path: old, DeleteEverythingInsideExcept: []string{"data"}}}
				next = config.Task{Module: "directory", ModuleInputs: &modules.Directory{Path: old, State: "absent", AlsoDeleteEverythingInside: true}}
			case "create-missing-directory":
				// The block tracks a missing directory; then
				// delete_everything_inside_except creates it, which refreshes
				// the service once.
				releases := filepath.Join(f.dir, "releases")
				initial = config.Task{Module: "directory", ModuleInputs: &modules.Directory{Path: releases, State: "absent"}}
				next = config.Task{Module: "directory", ModuleInputs: &modules.Directory{Path: releases, DeleteEverythingInsideExcept: []string{"2.0"}}}
			}
			op := f.op("A")
			op.TaskBlock.Changes = []config.Task{initial}
			f.run(t, op, "succeeded")
			f.assert(t, "A", "")
			op.TaskBlock.Changes = []config.Task{next}
			preview := job(op)
			preview.Dryrun = true
			r := f.e.Run(context.Background(), preview)
			requireStatus(t, r, "succeeded")
			// The service's own result predicts the refresh.
			if r.Change != "would_change" || r.Results[1].Task != "server/activate/task_1" || r.Results[1].Change != "would_change" {
				t.Fatal(r)
			}
			f.assert(t, "A", "")
			op.TaskBlock.Validate = []config.Task{{Module: "shell", ModuleInputs: &modules.Shell{Script: "exit 1"}}}
			f.run(t, op, "failed")
			f.assert(t, "A", "")
			op.TaskBlock.Validate = nil
			f.run(t, op, "succeeded")
			f.assert(t, loaded, "reload\n")
			f.run(t, op, "succeeded")
			f.assert(t, loaded, "reload\n")
		})
	}
}

func TestLinkLoopsAndDuplicatePaths(t *testing.T) {
	f := newServiceFixture(t)
	op := f.op("A")
	op.TaskBlock.Changes = []config.Task{{Loop: []any{"one", "two"}, As: "item", Module: "symlink", ModuleInputs: &modules.Symlink{Path: f.dir + "/{{ .item }}", Target: "{{ .item }}-target"}}}
	f.run(t, op, "succeeded")
	for _, name := range []string{"one", "two"} {
		target, err := os.Readlink(filepath.Join(f.dir, name))
		if err != nil || target != name+"-target" {
			t.Fatal(target, err)
		}
	}
	f.assert(t, "A", "reload\n")
	f.run(t, op, "succeeded")
	f.assert(t, "A", "reload\n")
	duplicate := filepath.Join(f.dir, "duplicate")
	op.TaskBlock.Changes = []config.Task{taskFile(duplicate, "a"), {Module: "symlink", ModuleInputs: &modules.Symlink{Path: duplicate, Target: "b"}}}
	r := f.e.Run(context.Background(), job(op))
	if r.Status != "failed" || !strings.Contains(r.Error, "duplicate") {
		t.Fatal(r)
	}
	if _, err := os.Lstat(duplicate); !os.IsNotExist(err) {
		t.Fatal("mutated duplicate path", err)
	}
}

func TestSecretDiffNeverEntersSavedResults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "public-mode-secret")
	put(t, path, "old-sensitive-value\n")
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	j := job(taskFile(path, "{{ .secrets.value }}\n"))
	config.InputsOf[modules.File](j.NodeTasks[0]).DangerouslyLetEveryAccountReadSecret = true
	j.Scope = map[string]any{"secrets": map[string]any{"value": value.Sensitive("new-sensitive-value")}}
	for _, dryrun := range []bool{true, false} {
		j.Dryrun = dryrun
		r := (&Engine{}).Run(context.Background(), j)
		requireStatus(t, r, "succeeded")
		change := r.Results[0].FileChange
		if change == nil || change.Diff != "" || !strings.Contains(change.DiffOmitted, "secrets") {
			t.Fatal(r)
		}
		data, err := json.Marshal(r)
		if err != nil || strings.Contains(string(data), "sensitive-value") {
			t.Fatal("secret in diagnostics", err)
		}
	}
}

func fileAsset(t *testing.T, data []byte) *assets.Ref {
	t.Helper()
	source := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(source, data, 0600); err != nil {
		t.Fatal(err)
	}
	ref, err := assets.Snapshot(context.Background(), t.TempDir(), source)
	if err != nil {
		t.Fatal(err)
	}
	return &ref
}

func TestDownloadHashesLikeAnyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app")
	ref := fileAsset(t, []byte("bytes"))
	hash := func(task config.Task) string {
		t.Helper()
		v, err := configurationHash(context.Background(), config.TrackedInputs(task), map[string]appliedFile{path: {content: config.InputsOf[modules.File](task), scope: scope()}})
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	want := hash(taskFile(path, "bytes"))
	for _, f := range []modules.File{{Mode: "0644", Path: path, Source: "local", Asset: ref}, {Mode: "0644", Path: path, URL: "https://example.com/app", SHA256: ref.SHA256, Asset: ref}} {
		if got := hash(config.Task{Module: "file", ModuleInputs: &f}); got != want {
			t.Fatal(f, got, want)
		}
	}
}

func TestDownloadLoopsAndDuplicateServicePaths(t *testing.T) {
	f := newServiceFixture(t)
	ref := fileAsset(t, []byte("bytes"))
	d := modules.File{Mode: "0644", URL: "https://example.com/file", SHA256: ref.SHA256, Asset: ref, Path: f.dir + "/{{ .item }}"}
	op := f.op("A")
	op.TaskBlock.Changes = []config.Task{{Loop: []any{"one", "two"}, As: "item", Module: "file", ModuleInputs: &d}}
	f.run(t, op, "succeeded")
	for _, name := range []string{"one", "two"} {
		data, err := os.ReadFile(filepath.Join(f.dir, name))
		if err != nil || string(data) != "bytes" {
			t.Fatal(string(data), err)
		}
	}
	f.assert(t, "A", "reload\n")
	f.run(t, op, "succeeded")
	f.assert(t, "A", "reload\n")
	d.Path = filepath.Join(f.dir, "duplicate")
	op.TaskBlock.Changes = []config.Task{{Module: "file", ModuleInputs: &d}, taskFile(d.Path, "other")}
	r := f.e.Run(context.Background(), job(op))
	if r.Status != "failed" || !strings.Contains(r.Error, "duplicate") {
		t.Fatal(r)
	}
	if _, err := os.Stat(d.Path); !os.IsNotExist(err) {
		t.Fatal("mutated duplicate path", err)
	}
}

// One download per processor architecture may write the same path, each with
// a when on .node.arch: only the one for this node runs, and the service
// follows what it wrote. Two that both run still conflict.
func TestDownloadPerArchitectureSharesItsPath(t *testing.T) {
	f := newServiceFixture(t)
	download := func(arch, data string) config.Task {
		ref := fileAsset(t, []byte(data))
		return config.Task{When: map[string]any{"equals": []any{"{{ .node.arch }}", arch}},
			Module: "file", ModuleInputs: &modules.File{Mode: "0644", URL: "https://example.com/" + arch, SHA256: ref.SHA256, Asset: ref, Path: f.path}}
	}
	op := func(data string) config.Task {
		o := f.op("")
		// The other architecture's task comes first: the path is hashed as
		// the task that runs writes it.
		o.TaskBlock.Changes = []config.Task{download("not-"+runtime.GOARCH, "other"), download(runtime.GOARCH, data)}
		return o
	}
	f.run(t, op("A"), "succeeded")
	f.assert(t, "A", "")
	preview := job(op("B"))
	preview.Dryrun = true
	r := f.e.Run(context.Background(), preview)
	requireStatus(t, r, "succeeded")
	if service := r.Results[len(r.Results)-2]; !strings.Contains(service.Task, "/activate/") || service.Change != "would_change" {
		t.Fatalf("dryrun did not predict the reload: %+v", r.Results)
	}
	f.assert(t, "A", "")
	f.run(t, op("B"), "succeeded")
	f.assert(t, "B", "reload\n")
	f.run(t, op("B"), "succeeded")
	f.assert(t, "B", "reload\n")

	both := op("C")
	both.TaskBlock.Changes[0] = download(runtime.GOARCH, "other")
	r = f.e.Run(context.Background(), job(both))
	if r.Status != "failed" || !strings.Contains(r.Error, "duplicate service configuration path") || read(t, f.path) != "B" {
		t.Fatal(r)
	}
}

// edit_file edits of a tracked file reload the service once, also after an
// attempt that stopped before activation; dryrun predicts the reload from the
// filter's output, and an unchanged rerun does nothing. An edit_file and a
// file task for the same path are rejected.
func TestEditFileReloadsOnceAndIsPredicted(t *testing.T) {
	f := newServiceFixture(t)
	put(t, f.path, "port=1\nmode=a\n")
	op := func(port string) config.Task {
		edit := config.Task{Module: "edit_file", ModuleInputs: &modules.EditFile{Path: f.path, ContentFilter: &modules.ContentFilter{Shell: &modules.FilterShell{
			Script: `sed "s/^port=.*/port=$PORT/"`, Env: map[string]any{"PORT": port}}}}}
		return config.Task{TaskBlock: &config.TaskBlock{ID: "server", Changes: []config.Task{edit,
			{Module: "line", ModuleInputs: &modules.Line{InsertAtEndOfFile: true, Path: f.path, Match: "^mode=", Text: "mode=a"}}},
			Activate: []config.Task{{Module: "systemd_service", ModuleInputs: &modules.Service{State: "running", Name: "demo", Refresh: "reload"}}}}}
	}
	f.run(t, op("1"), "succeeded")
	f.assert(t, "A", "")
	j := job(op("2"))
	j.Dryrun = true
	r := f.e.Run(context.Background(), j)
	requireStatus(t, r, "succeeded")
	if service := r.Results[len(r.Results)-2]; !strings.Contains(service.Task, "/activate/") || service.Change != "would_change" {
		t.Fatalf("dryrun did not predict the edit's reload: %+v", r.Results)
	}
	if read(t, f.path) != "port=1\nmode=a\n" {
		t.Fatal("dryrun changed the file")
	}
	failing := op("2")
	failing.TaskBlock.Validate = []config.Task{{Module: "shell", ModuleInputs: &modules.Shell{Script: "exit 1"}}}
	f.run(t, failing, "failed")
	f.assert(t, "A", "")
	f.run(t, op("2"), "succeeded")
	f.assert(t, "port=2\nmode=a\n", "reload\n")
	f.run(t, op("2"), "succeeded")
	f.assert(t, "port=2\nmode=a\n", "reload\n")

	both := op("3")
	both.TaskBlock.Changes = append(both.TaskBlock.Changes, taskFile(f.path, "port=4\n"))
	r = f.e.Run(context.Background(), job(both))
	if r.Status != "failed" || !strings.Contains(r.Error, "duplicate") || read(t, f.path) != "port=2\nmode=a\n" {
		t.Fatal(r)
	}
}

// A file that keeps its existing content is no tracked input: creating it or a
// program rewriting it never reloads the service.
func TestKeptFileIsNotTracked(t *testing.T) {
	f := newServiceFixture(t)
	kept := filepath.Join(f.dir, "sentinel.conf")
	content := "first\n"
	op := f.op("A")
	op.TaskBlock.Changes = append(op.TaskBlock.Changes, config.Task{Module: "file", ModuleInputs: &modules.File{Mode: "0644", Path: kept, Content: &content, IfFileExists: "keep_existing_content"}})
	f.run(t, op, "succeeded")
	f.assert(t, "A", "")
	if read(t, kept) != "first\n" {
		t.Fatal(read(t, kept))
	}
	put(t, kept, "rewritten by the program\n")
	j := job(op)
	j.Dryrun = true
	r := f.e.Run(context.Background(), j)
	requireStatus(t, r, "succeeded")
	if r.Change != "unchanged" {
		t.Fatal(r)
	}
	f.run(t, op, "succeeded")
	f.assert(t, "A", "")
	if read(t, kept) != "rewritten by the program\n" {
		t.Fatal("replaced the kept content")
	}
}

// A file from node paths is tracked by its destination: a renewed source
// reloads the service once, predicted by dryrun.
func TestContentFromNodePathsIsTracked(t *testing.T) {
	f := newServiceFixture(t)
	source := filepath.Join(f.dir, "fullchain.pem")
	put(t, source, "A")
	op := f.op("A")
	op.TaskBlock.Changes = []config.Task{{Module: "file", ModuleInputs: &modules.File{Path: f.path, ContentFromNodePaths: []string{source}, Mode: "0644"}}}
	f.run(t, op, "succeeded")
	f.assert(t, "A", "")
	put(t, source, "B")
	j := job(op)
	j.Dryrun = true
	r := f.e.Run(context.Background(), j)
	requireStatus(t, r, "succeeded")
	if service := r.Results[len(r.Results)-2]; service.Change != "would_change" {
		t.Fatalf("dryrun did not predict the reload: %+v", r.Results)
	}
	f.run(t, op, "succeeded")
	f.assert(t, "B", "reload\n")
	f.run(t, op, "succeeded")
	f.assert(t, "B", "reload\n")
}

// A tracked link that replaces a regular file hashes that file as something
// other than the link, so the switch reloads the service once.
func TestLinkReplacingAFileIsTracked(t *testing.T) {
	f := newServiceFixture(t)
	put(t, filepath.Join(f.dir, "b"), "B")
	op := f.op("A")
	op.TaskBlock.Changes = []config.Task{{Module: "symlink", ModuleInputs: &modules.Symlink{Path: f.path, Target: "b", ReplaceExistingFile: true}}}
	f.run(t, op, "succeeded")
	f.assert(t, "B", "reload\n")
	if target, err := os.Readlink(f.path); err != nil || target != "b" {
		t.Fatal(target, err)
	}
	f.run(t, op, "succeeded")
	f.assert(t, "B", "reload\n")
}

// A tracked node task whose when is false as its node task block starts
// writes nothing, so a value that only its arguments read need not exist,
// such as an input that another kind of server gets: the block runs without
// it. A when that a node task of the block makes true fails the task, instead
// of running a change that the block does not follow.
func TestATrackedTaskWhoseWhenIsFalseNeedsNoArgumentsThatResolve(t *testing.T) {
	f := newServiceFixture(t)
	jobsServers := config.Task{ID: "jobs_servers_lines", When: "{{ .vars.has_jobs_servers }}", Module: "file",
		ModuleInputs: &modules.File{Mode: "0644", Content: ptr("x"), Path: "{{ .vars.jobs_servers_file }}"}}
	op := f.op("A")
	op.TaskBlock.Changes = append(op.TaskBlock.Changes, jobsServers)
	j := job(op)
	j.Vars = map[string]any{"has_jobs_servers": false}
	r := f.e.Run(context.Background(), j)
	requireStatus(t, r, "succeeded")
	f.assert(t, "A", "")
	for _, result := range r.Results {
		if strings.HasSuffix(result.Task, "jobs_servers_lines") && (result.Status != "skipped" || result.Reason != "when is false") {
			t.Fatalf("%+v", result)
		}
	}
	// A set_vars of the block makes the when true.
	op.TaskBlock.Changes = append([]config.Task{{SetVars: map[string]any{"has_jobs_servers": true}}}, op.TaskBlock.Changes...)
	j = job(op)
	j.Vars = map[string]any{"has_jobs_servers": false}
	r = f.e.Run(context.Background(), j)
	if r.Status != "failed" || !strings.Contains(r.Error, "when condition was false when the node task block started") {
		t.Fatalf("%+v", r)
	}
}
