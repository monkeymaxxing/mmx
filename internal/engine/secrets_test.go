package engine

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mmx/internal/config"
	"mmx/internal/modules"
	"mmx/internal/protocol"
	"mmx/internal/value"
)

func TestSecretRoleFileResultsAndOutputs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "private.conf")
	tsk := config.Task{ID: "app", Role: "app", Inputs: map[string]any{"password": "{{ .secrets.password }}"}, RoleInputs: map[string]config.Input{"password": {Type: "string", Required: true, Secret: true}}, RoleNodeTasks: []config.Task{
		{ID: "write", Module: "file", ModuleInputs: &modules.File{Path: path, Template: "inline", TemplateText: `password={{ printf "%s" .inputs.password }}`, Mode: "0600"}},
		{ID: "derived", Module: "shell", ModuleInputs: &modules.Shell{Script: `printf '%s-derived' "$PASSWORD"`, Env: map[string]any{"PASSWORD": "{{ .inputs.password }}"}}},
		{SetVars: map[string]any{"copied": "{{ .node_tasks.derived.stdout }}"}},
		{Show: map[string]any{"public": "label", "secret": "{{ .vars.copied }}"}},
	}, RoleOutputs: map[string]any{"password": "{{ .inputs.password }}", "computed": "{{ .vars.copied }}"}}
	j := job(tsk)
	j.Scope = map[string]any{"secrets": map[string]any{"password": &value.Secret{Name: "password", Data: "engine-sentinel", Available: true}}}
	j.Outputs = map[string]any{"password": "{{ .node_tasks.app.outputs.password }}", "computed": "{{ .node_tasks.app.outputs.computed }}"}
	reports := 0
	e := Engine{Report: func(observation protocol.Response) error {
		reports++
		b, err := json.Marshal(observation)
		if strings.Contains(string(b), "engine-sentinel") {
			t.Error("secret in progress record")
		}
		return err
	}}
	r := e.Run(context.Background(), j)
	requireStatus(t, r, "succeeded")
	if reports < 4 {
		t.Fatal("progress was not exercised")
	}
	if read(t, path) != "password=engine-sentinel" {
		t.Fatal("file lacks actual secret")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal(info.Mode())
	}
	if value.Plain(r.Outputs["computed"]) != "engine-sentinel-derived" {
		t.Fatal("derived output was lost")
	}
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), "engine-sentinel") {
		t.Fatal("secret in durable response")
	}
	wire, _ := json.Marshal(r.Transport())
	var received protocol.Response
	if err := protocol.Decode(wire, &received); err != nil {
		t.Fatal(err)
	}
	if value.Plain(received.Outputs["computed"]) != "engine-sentinel-derived" {
		t.Fatal("private output was not transported")
	}
	var retained protocol.Response
	if err := protocol.Decode(b, &retained); err != nil {
		t.Fatal(err)
	}
	if err := value.RequireAvailable(retained.Outputs); err == nil {
		t.Fatal("redacted outputs claim to be available")
	}
	j.NodeTasks[0].RoleNodeTasks = append(j.NodeTasks[0].RoleNodeTasks, config.Task{Module: "shell", ModuleInputs: &modules.Shell{Script: `cat "$CONFIG"; exit 4`, Env: map[string]any{"CONFIG": path}}})
	r = e.Run(context.Background(), j)
	requireStatus(t, r, "failed")
	b, _ = json.Marshal(r)
	if strings.Contains(string(b), "engine-sentinel") {
		t.Fatal("failed role leaked file contents")
	}
}
func TestSecretCandidateFailureAndIdentifierProtection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private")
	put(t, path, "old")
	j := job(config.Task{Module: "file", ModuleInputs: &modules.File{Mode: "0600", Path: path, Content: text("{{ .secrets.password }}"), CheckNewFile: &modules.Check{Shell: &modules.Shell{Script: `cat "$CANDIDATE"; exit 1`, Env: map[string]any{"CANDIDATE": "{{ .new_file.path }}"}}}}})
	j.Scope = map[string]any{"secrets": map[string]any{"password": value.Sensitive("rejected-secret")}}
	r := (&Engine{}).Run(context.Background(), j)
	requireStatus(t, r, "failed")
	if read(t, path) != "old" {
		t.Fatal("rejected candidate installed")
	}
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), "rejected-secret") {
		t.Fatal("candidate validator leaked secret")
	}
	config.InputsOf[modules.File](j.NodeTasks[0]).Path = "/tmp/{{ .secrets.password }}"
	r = (&Engine{}).Run(context.Background(), j)
	requireStatus(t, r, "failed")
	b, _ = json.Marshal(r)
	if strings.Contains(string(b), "rejected-secret") {
		t.Fatal("identifier error leaked secret")
	}
}

func TestLoopResultsKeepPrivateValuesForLaterNodeTasks(t *testing.T) {
	target := filepath.Join(t.TempDir(), "derived")
	j := job(config.Task{ID: "tokens", Loop: []any{"one"}, As: "name", Module: "shell", ModuleInputs: &modules.Shell{Script: `printf '%s-loop' "$TOKEN"`, Env: map[string]any{"TOKEN": "{{ .secrets.token }}"}}}, taskFile(target, "{{ .node_tasks.tokens.items[0].stdout }}"))
	config.InputsOf[modules.File](j.NodeTasks[1]).Mode = "0600"
	j.Scope = map[string]any{"secrets": map[string]any{"token": value.Sensitive("loop-secret")}}
	r := (&Engine{}).Run(context.Background(), j)
	requireStatus(t, r, "succeeded")
	if read(t, target) != "loop-secret-loop" {
		t.Fatal("redacted diagnostic was used as a real task input")
	}
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), "loop-secret") {
		t.Fatal("loop result leaked")
	}
}

func TestPrivateOutputPredicatesAndTypeErrorsStayPrivate(t *testing.T) {
	j := job(config.Task{ID: "private", Module: "shell", ModuleInputs: &modules.Shell{Script: `printf '%s' "$INPUT"`, Env: map[string]any{"INPUT": "{{ .secrets.token }}"}, ChangedWhen: map[string]any{"contains": []any{"{{ .this_node_task.stdout }}", "sentinel"}}}},
		config.Task{Show: "matched", When: map[string]any{"matches": []any{"{{ .node_tasks.private.stdout }}", "^sentinel"}}})
	j.Scope = map[string]any{"secrets": map[string]any{"token": value.Sensitive("sentinel-private-value")}}
	j.Outputs = map[string]any{"matched": map[string]any{"contains": []any{"{{ .node_tasks.private.stdout }}", "sentinel"}}}
	e := Engine{}
	r := e.Run(context.Background(), j)
	requireStatus(t, r, "succeeded")
	if !r.Results[0].Secret || !r.Results[1].Secret || !value.HasSecrets(r.Outputs["matched"]) || value.Plain(r.Outputs["matched"]) != true {
		t.Fatal("sensitivity lost", r)
	}
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), "sentinel-private-value") {
		t.Fatal("secret persisted")
	}
	j.NodeTasks = append(j.NodeTasks, config.Task{Role: "typed", Inputs: map[string]any{"number": "{{ .secrets.token }}"}, RoleInputs: map[string]config.Input{"number": {Type: "integer", Secret: true}}, Sources: config.Sources{"inputs.number": {File: "mmx.yml", Line: 12, Column: 9, Field: "fleet_stages[0].node_tasks[2].inputs.number"}}})
	r = e.Run(context.Background(), j)
	requireStatus(t, r, "failed")
	b, _ = json.Marshal(r)
	if strings.Contains(string(b), "sentinel-private-value") || !strings.Contains(r.Error, "mmx.yml:12:9") || !strings.Contains(r.Error, "must be integer") {
		t.Fatal(r.Error)
	}
}

func TestPercentTemplatePreservesSecretSensitivity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sizing")
	j := job(config.Task{Module: "file", ModuleInputs: &modules.File{Mode: "0600", Path: path, Template: "sizing", TemplateText: `private_capacity={{ percent .secrets.capacity 25 }}`}})
	j.Scope = map[string]any{"secrets": map[string]any{"capacity": value.Sensitive(int64(31111))}}
	r := (&Engine{}).Run(context.Background(), j)
	requireStatus(t, r, "succeeded")
	if read(t, path) != "private_capacity=7777" || !r.Results[0].Secret {
		t.Fatal(r)
	}
	saved, err := json.Marshal(r)
	if err != nil || strings.Contains(string(saved), "private_capacity=7777") {
		t.Fatal(string(saved), err)
	}
}

func TestChainedTemplateSecretSuppressesDifferences(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "private.conf")
	put(t, path, "password=old\n")
	e := Engine{RuntimeRoot: root, DiffDir: filepath.Join(root, "diffs")}
	j := job(config.Task{Module: "file", ModuleInputs: &modules.File{Path: path, Template: "private", TemplateText: "password={{ (.secrets.creds).password }}\n", Mode: "0600"}})
	j.Scope = map[string]any{"secrets": map[string]any{"creds": &value.Secret{Name: "creds", Data: map[string]any{"password": "chained-sentinel"}, Available: true}}}
	r := e.Run(context.Background(), j)
	requireStatus(t, r, "succeeded")
	if read(t, path) != "password=chained-sentinel\n" || !r.Results[0].Secret {
		t.Fatal("chained secret read did not mark the task secret", r.Results)
	}
	saved, _ := json.Marshal(r)
	if strings.Contains(string(saved), "chained-sentinel") {
		t.Fatal("chained secret in saved result")
	}
	if files, _ := os.ReadDir(e.DiffDir); len(files) != 0 {
		t.Fatal("chained secret difference was saved")
	}
}

// Result.Safe alone suppresses a secret task's reason; the job error and a
// loop's summary are built from that safe form.
func TestSecretFailureReasonStaysOutOfJobError(t *testing.T) {
	dir := t.TempDir()
	// The module reports its whole input, including the secret, as its reason.
	script := "#!/bin/sh\nprintf '{\"protocol\":1,\"status\":\"failed\",\"change\":\"unchanged\",\"reason\":\"%s\"}\\n' \"$(tr -d '\"\\\\\\n')\"\n"
	writeProgramFileThroughShellChild(t, filepath.Join(dir, modules.ModuleExecutableFile), script)
	module := modules.Module{Kind: modules.KindProjectExecutable, Directory: dir, Executable: modules.ModuleExecutableFile,
		Files: []modules.ModuleFile{{Path: modules.ModuleExecutableFile, Mode: 0700}, {Path: modules.ModuleDescriptionFile, Mode: 0600}}}
	echo := config.Task{Module: "echo", ModuleInputs: map[string]any{"token": "{{ .secrets.token }}"}, Retry: &config.Retry{Disabled: true}}
	looped := echo
	looped.Loop, looped.As = []any{"one"}, "item"
	for _, task := range []config.Task{echo, looped} {
		j := job(task)
		j.Modules = map[string]modules.Module{"echo": module}
		j.Scope = map[string]any{"secrets": map[string]any{"token": value.Sensitive("reason-sentinel")}}
		r := (&Engine{ModuleHelper: "/unused/helper"}).Run(context.Background(), j)
		requireStatus(t, r, "failed")
		saved, _ := json.Marshal(r)
		if !r.Results[0].Secret || strings.Contains(r.Error, "reason-sentinel") || strings.Contains(string(saved), "reason-sentinel") {
			t.Fatal("secret failure reason escaped", r.Error)
		}
	}
}

// A stand-in docker for a task with a registry login. The node's image is
// the file $MMX_TEST_DOCKER/image. With the file
// $MMX_TEST_DOCKER/pull-fails-saying, a pull fails with that file's text and
// the text of the login file that docker was given.
const fakeDockerThatTellsItsLogin = `#!/bin/sh
case "$1 $2" in
"info --format") echo '{"ServerVersion": "29.9.0", "ClientInfo": {"Plugins": []}}';;
"version --format") echo 29.9.0;;
"image inspect") if [ -f "$MMX_TEST_DOCKER/image" ]; then printf '[{"Id": "%s"}]\n' "$(cat "$MMX_TEST_DOCKER/image")"; else echo '[]'; exit 1; fi;;
"pull --quiet")
  if [ -f "$MMX_TEST_DOCKER/pull-fails-saying" ]; then echo "Error response from daemon: $(cat "$MMX_TEST_DOCKER/pull-fails-saying") $(cat "$DOCKER_CONFIG/config.json")" >&2; exit 1; fi
  echo sha256:pulled > "$MMX_TEST_DOCKER/image";;
*) echo "unexpected docker $*" >&2; exit 64;;
esac
`

// loginFixture is a node with the stand-in docker, an engine that fails the
// test when a progress record holds the registry password in a form that the
// test names, and jobs whose .secrets hold that password and one more secret.
type loginFixture struct {
	dir      string
	e        Engine
	password string
	// stored is the login as docker's login file holds it.
	stored string
}

func newLoginFixture(t *testing.T) *loginFixture {
	t.Helper()
	f := &loginFixture{dir: t.TempDir(), password: "sentinel p/w&4711"}
	f.stored = base64.StdEncoding.EncodeToString([]byte("deploy:" + f.password))
	bin, private := filepath.Join(f.dir, "bin"), filepath.Join(f.dir, "private_task_directories")
	for _, directory := range []string{bin, private} {
		if err := os.Mkdir(directory, 0711); err != nil {
			t.Fatal(err)
		}
	}
	writeProgramFileThroughShellChild(t, filepath.Join(bin, "docker"), fakeDockerThatTellsItsLogin)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("MMX_TEST_DOCKER", f.dir)
	f.e = Engine{PrivateTaskDirectoryRoot: func() (string, error) { return private, nil }, Report: func(observation protocol.Response) error {
		f.requireNoLogin(t, "a progress record", observation)
		return nil
	}}
	return f
}

// requireNoLogin fails the test when the saved form of v holds the password
// as its plain text or as docker's login file holds it.
func (f *loginFixture) requireNoLogin(t *testing.T, what string, v any) string {
	t.Helper()
	saved, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	// In JSON, the & of the password is \u0026.
	for _, form := range []string{f.password, strings.ReplaceAll(f.password, "&", `\u0026`), f.stored} {
		if at := strings.Index(string(saved), form); at >= 0 {
			t.Fatalf("the registry password is in %s: ...%s", what, saved[max(0, at-200):at+len(form)])
		}
	}
	return string(saved)
}

func (f *loginFixture) job(tasks ...config.Task) config.Job {
	j := job(tasks...)
	j.Scope = map[string]any{"secrets": map[string]any{
		"registry_password": &value.Secret{Name: "registry_password", Data: f.password, Available: true},
		"other":             &value.Secret{Name: "other", Data: "yes", Available: true}}}
	return j
}

// image is a docker_image task whose password comes from written.
func (f *loginFixture) image(written string) config.Task {
	return config.Task{ID: "image", Module: "docker_image", ModuleInputs: &modules.DockerImage{Image: "registry.example.com/shop/web:2.4", PullImage: "always",
		RegistryUsername: "deploy", RegistryPassword: written}}
}

// refuse makes the stand-in docker refuse a pull and tell the login.
func (f *loginFixture) refuse(t *testing.T) (reason string) {
	t.Helper()
	put(t, filepath.Join(f.dir, "pull-fails-saying"), "pull access denied for shop/web: the login deploy:"+f.password+" is refused;")
	return `docker_image: docker pull --quiet registry.example.com/shop/web:2.4: Error response from daemon: pull access denied for shop/web: the login deploy:[redacted] is refused; {"auths":{"registry.example.com":{"auth":"[redacted]"}}}`
}

// A secret in a login that a module hands only to its tool does not hide
// the task's result: the value shows, and a later node task that reads it
// is not secret because of the login. A failure keeps its reason, with
// [redacted] in place of the login in every form, in the task's result, its
// tries, its loop items, the progress records, and the node job's error.
func TestALoginHandedOnlyToAToolDoesNotHideTheTasksResult(t *testing.T) {
	ctx := context.Background()
	f := newLoginFixture(t)
	image := f.image("{{ .secrets.registry_password }}")
	j := f.job(image, config.Task{ID: "later", Show: "{{ .node_tasks.image.value.image_id }}"})
	j.Outputs = map[string]any{"image_id": "{{ .node_tasks.image.value.image_id }}"}
	r := f.e.Run(ctx, j)
	requireStatus(t, r, "succeeded")
	if saved := f.requireNoLogin(t, "the response", r); r.Results[0].Secret || !strings.Contains(saved, `"image_id":"sha256:pulled"`) {
		t.Fatalf("the result of the task with the login is hidden: %s", saved)
	}
	if r.Results[1].Secret || r.Results[1].Value != "sha256:pulled" || r.Outputs["image_id"] != "sha256:pulled" {
		t.Fatalf("a read of the task's result became secret: %+v %#v", r.Results[1], r.Outputs)
	}
	if left, _ := os.ReadDir(filepath.Join(f.dir, "private_task_directories")); len(left) != 0 {
		t.Fatal("the login directory is left behind", left)
	}

	// docker refuses the pull and tells the login.
	reason := f.refuse(t)
	r = f.e.Run(ctx, f.job(image))
	requireStatus(t, r, "failed")
	f.requireNoLogin(t, "the response", r)
	if got := r.Results[0]; got.Secret || got.Reason != reason || r.Error != "image: "+reason {
		t.Fatalf("reason: %s\nerror:  %s\nwant:   %s", got.Reason, r.Error, reason)
	}
	// Each try keeps its reason, and so does the reason that counts them.
	tried := image
	tried.Retry = &config.Retry{MaxTries: 2, PauseBetweenTries: "0s", RetryWhen: map[string]any{"contains": []any{"{{ .this_node_task.reason }}", "pull access denied"}}}
	r = f.e.Run(ctx, f.job(tried))
	requireStatus(t, r, "failed")
	f.requireNoLogin(t, "the response", r)
	if got := r.Results[0]; got.Secret || len(got.Tries) != 2 || got.Tries[0].Reason != reason || got.Reason != "node task did not finish after 2 tries, because each try failed or left retry.until false: "+reason {
		t.Fatalf("%+v", got)
	}
	// A retry condition that cannot be evaluated quotes the try's reason in
	// its own error, and the login is not in it.
	tried.Retry.RetryWhen = map[string]any{"number_at_least": []any{"{{ .this_node_task.reason }}", 1}}
	r = f.e.Run(ctx, f.job(tried))
	requireStatus(t, r, "failed")
	if saved := f.requireNoLogin(t, "the response", r); !strings.Contains(r.Error, "is not a number") || !strings.Contains(saved, "[redacted] is refused") {
		t.Fatalf("%s", r.Error)
	}
	// A loop's items and its summary.
	looped := image
	looped.Loop, looped.As = []any{"one"}, "item"
	r = f.e.Run(ctx, f.job(looped))
	requireStatus(t, r, "failed")
	f.requireNoLogin(t, "the response", r)
	if summary := r.Results[len(r.Results)-1]; summary.Secret || len(summary.Items) != 1 || summary.Items[0].Reason != reason || summary.Reason != reason || !strings.HasSuffix(r.Error, reason) {
		t.Fatalf("%+v\n%s", summary, r.Error)
	}
	// A password that is written in the playbook is no secret: nothing is
	// replaced.
	f.e.Report = nil
	r = f.e.Run(ctx, f.job(f.image(f.password)))
	requireStatus(t, r, "failed")
	if !strings.Contains(r.Error, "the login deploy:"+f.password+" is refused") || !strings.Contains(r.Error, f.stored) {
		t.Fatal(r.Error)
	}
}

// Every other read of a secret still hides the result of a task with a
// login: a secret in the task's own when, and an earlier node task of the
// same node task block that read one.
func TestATaskWithALoginThatReadsAnotherSecretIsHiddenAsBefore(t *testing.T) {
	ctx := context.Background()
	f := newLoginFixture(t)
	const hidden = "details suppressed for a task using secrets"
	image := f.image("{{ .secrets.registry_password }}")
	withWhen := image
	withWhen.When = map[string]any{"equals": []any{"{{ .secrets.other }}", "yes"}}
	private := filepath.Join(f.dir, "private.conf")
	afterASecret := config.Task{TaskBlock: &config.TaskBlock{ID: "shop", Changes: []config.Task{
		{Module: "file", ModuleInputs: &modules.File{Path: private, Content: text("other={{ .secrets.other }}\n"), Mode: "0600"}}, image}}}
	alone := config.Task{TaskBlock: &config.TaskBlock{ID: "shop", Changes: []config.Task{image}}}
	result := func(r protocol.Response) protocol.Result {
		t.Helper()
		return resultsByTask(t, r, "image", "shop/changes/image")["image"]
	}
	cases := map[string]config.Task{"a secret in when": withWhen, "after a node task of the block that read a secret": afterASecret}
	// The pull succeeds: the hidden result shows no value.
	for name, task := range cases {
		r := f.e.Run(ctx, f.job(task))
		requireStatus(t, r, "succeeded")
		if saved := f.requireNoLogin(t, "the response", r); !result(r).Secret || strings.Contains(saved, "sha256:pulled") {
			t.Fatalf("%s: the result is not hidden: %s", name, saved)
		}
	}
	// A node task block does not hide the task by itself.
	r := f.e.Run(ctx, f.job(alone))
	requireStatus(t, r, "succeeded")
	if result(r).Secret {
		t.Fatalf("a node task block hid the result of its task with a login: %+v", r.Results)
	}
	// The pull is refused: the hidden result shows no reason.
	f.refuse(t)
	for name, task := range cases {
		r := f.e.Run(ctx, f.job(task))
		requireStatus(t, r, "failed")
		if saved := f.requireNoLogin(t, "the response", r); !result(r).Secret || !strings.Contains(r.Error, hidden) || strings.Contains(saved, "pull access denied") {
			t.Fatalf("%s: the failure is not hidden: %s", name, saved)
		}
	}
}

// resultsByTask finds, for each of names, the one result whose task is that
// name or ends with a slash and that name, and returns them by the last part
// of the name.
func resultsByTask(t *testing.T, r protocol.Response, names ...string) map[string]protocol.Result {
	t.Helper()
	found := map[string]protocol.Result{}
	for _, result := range r.Results {
		for _, name := range names {
			if result.Task == name || strings.HasSuffix(result.Task, "/"+name) {
				found[name[strings.LastIndex(name, "/")+1:]] = result
			}
		}
	}
	return found
}

// A role call hands a secret to its role through an input. That alone hides
// nothing. A role whose node tasks hand the secret only to a tool as a login
// shows every result, also when a second role stands between, and a failure
// keeps its reason with [redacted] in place of the login.
func TestARoleThatHandsItsSecretInputOnlyToAToolAsALoginHidesNothing(t *testing.T) {
	ctx := context.Background()
	f := newLoginFixture(t)
	passwordInput := map[string]config.Input{"registry_password": {Type: "string", Required: true, Secret: true}}
	deploy := config.Task{ID: "deploy", Role: "deploy", Inputs: map[string]any{"registry_password": "{{ .secrets.registry_password }}"}, RoleInputs: passwordInput,
		RoleNodeTasks: []config.Task{f.image("{{ .inputs.registry_password }}"), {ID: "later", Show: "{{ .node_tasks.image.value.image_id }}"}},
		RoleOutputs:   map[string]any{"image_id": "{{ .node_tasks.image.value.image_id }}"}}
	inner := deploy
	inner.Inputs = map[string]any{"registry_password": "{{ .inputs.registry_password }}"}
	nested := config.Task{ID: "shop", Role: "shop", Inputs: map[string]any{"registry_password": "{{ .secrets.registry_password }}"}, RoleInputs: passwordInput,
		RoleNodeTasks: []config.Task{inner}, RoleOutputs: map[string]any{"image_id": "{{ .node_tasks.deploy.outputs.image_id }}"}}
	for name, call := range map[string]config.Task{"a role": deploy, "a role inside a role": nested} {
		os.Remove(filepath.Join(f.dir, "image"))
		os.Remove(filepath.Join(f.dir, "pull-fails-saying"))
		j := f.job(call)
		j.Outputs = map[string]any{"image_id": "{{ .node_tasks." + call.ID + ".outputs.image_id }}"}
		r := f.e.Run(ctx, j)
		requireStatus(t, r, "succeeded")
		saved := f.requireNoLogin(t, "the response", r)
		for _, result := range r.Results {
			if result.Secret {
				t.Fatalf("%s: the result of %s is hidden: %s", name, result.Task, saved)
			}
		}
		results := resultsByTask(t, r, "image", "later", call.ID)
		if !reflect.DeepEqual(results["image"].Value, map[string]any{"image_id": "sha256:pulled", "previous_image_id": nil}) || results["later"].Value != "sha256:pulled" ||
			results[call.ID].Outputs["image_id"] != "sha256:pulled" || r.Outputs["image_id"] != "sha256:pulled" {
			t.Fatalf("%s: %+v\noutputs %#v", name, results, r.Outputs)
		}
		// docker refuses the pull and tells the login.
		reason := f.refuse(t)
		r = f.e.Run(ctx, f.job(call))
		requireStatus(t, r, "failed")
		f.requireNoLogin(t, "the response", r)
		results = resultsByTask(t, r, "image", call.ID)
		if results["image"].Secret || results["image"].Reason != reason || results[call.ID].Secret || !strings.HasSuffix(results[call.ID].Reason, "image: "+reason) || !strings.HasSuffix(r.Error, "image: "+reason) {
			t.Fatalf("%s: %+v\nerror %s", name, results, r.Error)
		}
	}
}

// roleFixture is a role call that hands the secret token, and the secret
// list tokens, to its role, and the job that holds both secrets.
type roleFixture struct {
	dir string
	e   Engine
}

const (
	roleToken       = "role-sentinel-token"
	roleTokenInList = "role-sentinel-in-a-list"
)

func (f roleFixture) call(id string, inputs map[string]any, tasks ...config.Task) config.Task {
	return config.Task{ID: id, Role: id, Inputs: inputs, RoleNodeTasks: tasks, RoleInputs: map[string]config.Input{
		"token": {Type: "string", Required: true, Secret: true}, "tokens": {Type: "list", Required: true, Secret: true}}}
}

func (f roleFixture) job(tasks ...config.Task) config.Job {
	j := job(tasks...)
	j.Scope = map[string]any{"secrets": map[string]any{
		"token":  &value.Secret{Name: "token", Data: roleToken, Available: true},
		"tokens": &value.Secret{Name: "tokens", Data: []any{roleTokenInList}, Available: true}}}
	return j
}

// said is a node task that reads no secret and prints word.
func said(word string) config.Task {
	return config.Task{ID: word, Module: "shell", ModuleInputs: &modules.Shell{Script: "echo " + word}}
}

// requireHidden fails the test unless exactly the results named in hidden
// are hidden and those named in shown are not, and the saved response holds
// what each shown node task printed and no secret.
func (f roleFixture) requireHidden(t *testing.T, what string, r protocol.Response, hidden, shown []string) {
	t.Helper()
	saved, err := json.Marshal(r)
	if err != nil || strings.Contains(string(saved), "role-sentinel") {
		t.Fatalf("%s: a secret is in the saved response: %s %v", what, saved, err)
	}
	byTask := map[string]protocol.Result{}
	for _, result := range r.Results {
		byTask[result.Task] = result
	}
	for _, name := range hidden {
		if result, found := byTask[name]; !found || !result.Secret {
			t.Errorf("%s: the result of %s is not hidden (found: %v)", what, name, found)
		}
	}
	for _, name := range shown {
		result, found := byTask[name]
		if !found || result.Secret {
			t.Errorf("%s: the result of %s is hidden or missing (found: %v)", what, name, found)
		}
		if word := name[strings.LastIndex(name, "/")+1:]; result.Stdout != "" && !strings.Contains(string(saved), `"stdout":"`+word+`\n"`) {
			t.Errorf("%s: the saved response does not show what %s printed", what, name)
		}
	}
	if len(hidden)+len(shown) != len(r.Results) {
		names := []string{}
		for _, result := range r.Results {
			names = append(names, result.Task)
		}
		t.Errorf("%s: the test names %d results, and the response has %v", what, len(hidden)+len(shown), names)
	}
}

// A role whose second of three node tasks writes its secret input into a
// file: the first node task shows its result, the second and the third are
// hidden, and so is the role call. The third prints the file, so its output
// must not show.
func TestARoleHidesItsNodeTasksFromTheFirstThatReadsTheSecretInput(t *testing.T) {
	f := roleFixture{dir: t.TempDir()}
	private := filepath.Join(f.dir, "private.conf")
	inputs := map[string]any{"token": "{{ .secrets.token }}", "tokens": "{{ .secrets.tokens }}"}
	call := f.call("deploy", inputs,
		said("before"),
		config.Task{ID: "write", Module: "file", ModuleInputs: &modules.File{Path: private, Content: text("token={{ .inputs.token }}\n"), Mode: "0600"}},
		config.Task{ID: "print", Module: "shell", ModuleInputs: &modules.Shell{Script: "cat " + private}})
	r := f.e.Run(context.Background(), f.job(call, said("afterwards")))
	requireStatus(t, r, "succeeded")
	if read(t, private) != "token="+roleToken+"\n" {
		t.Fatal("the file does not hold the secret")
	}
	// A node task after the role call is outside the role: it is not hidden.
	f.requireHidden(t, "a role", r, []string{"deploy/write", "deploy/print", "deploy"}, []string{"deploy/before", "afterwards"})
	// The same when the node task that prints the file fails.
	call.RoleNodeTasks[2] = config.Task{ID: "print", Module: "shell", ModuleInputs: &modules.Shell{Script: "cat " + private + "; exit 4"}, Retry: &config.Retry{Disabled: true}}
	r = f.e.Run(context.Background(), f.job(call))
	requireStatus(t, r, "failed")
	f.requireHidden(t, "a role whose last node task fails", r, []string{"deploy/write", "deploy/print", "deploy"}, []string{"deploy/before"})
	if !strings.HasSuffix(r.Error, "details suppressed for a task using secrets") {
		t.Fatal(r.Error)
	}
}

// Every way in which a node task inside a role reads a role input that holds
// a secret marks that node task: its result is hidden, and so are the
// results of the role's node tasks after it and of the role call. The node
// task before it read nothing and shows its result.
func TestEveryReadOfASecretRoleInputHidesTheReadingNodeTask(t *testing.T) {
	f := roleFixture{dir: t.TempDir(), e: Engine{ModuleHelper: "/unused/helper"}}
	path := filepath.Join(f.dir, "written")
	// A module of the project's own that reports nothing of its inputs.
	moduleDirectory := filepath.Join(f.dir, "quiet")
	if err := os.Mkdir(moduleDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	writeProgramFileThroughShellChild(t, filepath.Join(moduleDirectory, modules.ModuleExecutableFile), "#!/bin/sh\ncat >/dev/null\nprintf '{\"protocol\":1,\"status\":\"succeeded\",\"change\":\"unchanged\"}\\n'\n")
	quiet := modules.Module{Kind: modules.KindProjectExecutable, Directory: moduleDirectory, Executable: modules.ModuleExecutableFile,
		Files: []modules.ModuleFile{{Path: modules.ModuleExecutableFile, Mode: 0700}, {Path: modules.ModuleDescriptionFile, Mode: 0600}}}
	startsWithRole := func(reference string) map[string]any {
		return map[string]any{"matches": []any{reference, "^role-sentinel"}}
	}
	readers := map[string]config.Task{
		"an input of a core module":             {Module: "shell", ModuleInputs: &modules.Shell{Script: `test -n "$TOKEN"`, Env: map[string]any{"TOKEN": "{{ .inputs.token }}"}}},
		"an input of a module that is not core": {Module: "quiet", ModuleInputs: map[string]any{"token": "{{ .inputs.token }}"}},
		"a file's content":                      {Module: "file", ModuleInputs: &modules.File{Path: path, Content: text("token={{ .inputs.token }}\n"), Mode: "0600"}},
		"a template":                            {Module: "file", ModuleInputs: &modules.File{Path: path, Template: "inline", TemplateText: `token={{ printf "%s" .inputs.token }}`, Mode: "0600"}},
		"a template that reads a part":          {Module: "file", ModuleInputs: &modules.File{Path: path, Template: "inline", TemplateText: `{{ range .inputs.tokens }}{{ . }}{{ end }}`, Mode: "0600"}},
		"when":                                  {Module: "shell", ModuleInputs: &modules.Shell{Script: "true"}, When: startsWithRole("{{ .inputs.token }}")},
		"a when that is false":                  {Module: "shell", ModuleInputs: &modules.Shell{Script: "true"}, When: map[string]any{"equals": []any{"{{ .inputs.token }}", "another"}}},
		"set_vars":                              {SetVars: map[string]any{"copied": "{{ .inputs.token }}"}},
		"set_vars with a condition":             {SetVars: map[string]any{"is_set": startsWithRole("{{ .inputs.token }}")}},
		"show":                                  {Show: map[string]any{"label": "token", "token": "{{ .inputs.token }}"}},
		"assert":                                {Module: "assert", ModuleInputs: &modules.Assert{That: startsWithRole("{{ .inputs.token }}")}},
		"the message of an assert":              {Module: "assert", ModuleInputs: &modules.Assert{That: true, Message: "{{ .inputs.token }}"}},
		"retry.until":                           {Module: "shell", ModuleInputs: &modules.Shell{Script: "true"}, Retry: &config.Retry{MaxTries: 2, PauseBetweenTries: "0s", Until: startsWithRole("{{ .inputs.token }}")}},
		"changed_when of a command":             {Module: "shell", ModuleInputs: &modules.Shell{Script: "true", ChangedWhen: startsWithRole("{{ .inputs.token }}")}},
		"check_new_file":                        {Module: "file", ModuleInputs: &modules.File{Path: path, Content: text("public\n"), Mode: "0644", CheckNewFile: &modules.Check{Shell: &modules.Shell{Script: `test -n "$TOKEN"`, Env: map[string]any{"TOKEN": "{{ .inputs.token }}"}}}}},
		"one entry of a secret list":            {Module: "shell", ModuleInputs: &modules.Shell{Script: `test -n "$TOKEN"`, Env: map[string]any{"TOKEN": "{{ .inputs.tokens[0] }}"}}},
	}
	inputs := map[string]any{"token": "{{ .secrets.token }}", "tokens": "{{ .secrets.tokens }}"}
	run := func(tasks ...config.Task) protocol.Response {
		os.Remove(path)
		j := f.job(tasks...)
		j.Modules = map[string]modules.Module{"quiet": quiet}
		return f.e.Run(context.Background(), j)
	}
	for name, reader := range readers {
		reader.ID = "reader"
		r := run(f.call("deploy", inputs, said("before"), reader, said("after")))
		requireStatus(t, r, "succeeded")
		// A node task that runs on the node may leave the secret there for
		// the next one to read back, so the next one is hidden too. set_vars,
		// show, and assert run nothing on the node: the secret stays in
		// values, and the next node task, which reads none, shows its result.
		if reader.SetVars != nil || reader.Show != nil || reader.Module == "assert" {
			f.requireHidden(t, name, r, []string{"deploy/reader", "deploy"}, []string{"deploy/before", "deploy/after"})
			continue
		}
		f.requireHidden(t, name, r, []string{"deploy/reader", "deploy/after", "deploy"}, []string{"deploy/before"})
	}

	// A loop over the input: the read of its items hides every iteration,
	// also one that does not read its item, and what comes after the loop.
	looped := config.Task{ID: "reader", Loop: "{{ .inputs.tokens }}", As: "item", Module: "shell", ModuleInputs: &modules.Shell{Script: "true"}}
	r := run(f.call("deploy", inputs, said("before"), looped, said("after")))
	requireStatus(t, r, "succeeded")
	// The loop's own result holds only its iterations' hidden results.
	f.requireHidden(t, "a loop's items", r, []string{"deploy/reader[0]", "deploy/after", "deploy"}, []string{"deploy/before", "deploy/reader"})

	// A node task block inside the role: its node task that reads the input
	// hides what follows in the block, the block, and what follows in the
	// role.
	block := config.Task{TaskBlock: &config.TaskBlock{ID: "shop", Changes: []config.Task{said("first"),
		{ID: "reader", Module: "file", ModuleInputs: &modules.File{Path: path, Content: text("token={{ .inputs.token }}\n"), Mode: "0600"}}, said("last")}}}
	r = run(f.call("deploy", inputs, said("before"), block, said("after")))
	requireStatus(t, r, "succeeded")
	f.requireHidden(t, "a node task block in a role", r, []string{"deploy/shop/changes/reader", "deploy/shop/changes/last", "deploy/shop", "deploy/after", "deploy"},
		[]string{"deploy/before", "deploy/shop/changes/first"})

	// A role inside the role: the outer role passes the input on, which is no
	// read. The inner role's node task that reads it marks every level above
	// it.
	passedOn := map[string]any{"token": "{{ .inputs.token }}", "tokens": "{{ .inputs.tokens }}"}
	reader := readers["an input of a core module"]
	reader.ID = "reader"
	inner := f.call("inner", passedOn, said("first"), reader, said("last"))
	r = run(f.call("outer", inputs, said("before"), inner, said("after")), said("afterwards"))
	requireStatus(t, r, "succeeded")
	f.requireHidden(t, "a role in a role", r, []string{"outer/inner/reader", "outer/inner/last", "outer/inner", "outer/after", "outer"},
		[]string{"outer/before", "outer/inner/first", "afterwards"})

	// A role output that reads the input: no node task of the role read it,
	// so each shows its result. The role call's result is hidden, and the
	// output stays a secret value that a later node task can use.
	withOutputs := f.call("deploy", inputs, said("before"), said("after"))
	withOutputs.RoleOutputs = map[string]any{"token": "{{ .inputs.token }}", "is_set": startsWithRole("{{ .inputs.token }}")}
	uses := config.Task{ID: "uses", Module: "file", ModuleInputs: &modules.File{Path: path, Content: text("{{ .node_tasks.deploy.outputs.token }}"), Mode: "0600"}}
	j := f.job(withOutputs, uses)
	j.Outputs = map[string]any{"token": "{{ .node_tasks.deploy.outputs.token }}", "is_set": "{{ .node_tasks.deploy.outputs.is_set }}"}
	r = f.e.Run(context.Background(), j)
	requireStatus(t, r, "succeeded")
	f.requireHidden(t, "a role output", r, []string{"deploy", "uses"}, []string{"deploy/before", "deploy/after"})
	if read(t, path) != roleToken {
		t.Fatal("the later node task did not get the role's secret output")
	}
	for name, want := range map[string]any{"token": roleToken, "is_set": true} {
		if !value.HasSecrets(r.Outputs[name]) || value.Plain(r.Outputs[name]) != want {
			t.Fatalf("the fleet stage output %s is %#v: it must stay a secret value", name, r.Outputs[name])
		}
	}
}

// A node task that reads a value derived from a secret reads the secret:
// a set_vars value copied from it, one computed from that value, a
// condition computed from it, and the output of a node task that read it.
// Each reader is hidden, also outside a role, where no earlier node task
// hides it; a node task between them that reads none shows its result; and
// the secret reaches nothing MMX reports (requireHidden).
func TestANodeTaskThatReadsAValueDerivedFromASecretIsHidden(t *testing.T) {
	f := roleFixture{dir: t.TempDir()}
	inputs := map[string]any{"token": "{{ .secrets.token }}", "tokens": "{{ .secrets.tokens }}"}
	prints := func(id, reference string) config.Task {
		return config.Task{ID: id, Module: "shell", ModuleInputs: &modules.Shell{Script: `printf '%s\n' "$VALUE"`, Env: map[string]any{"VALUE": reference}}}
	}
	readers := map[string]config.Task{
		"a copied value":   prints("reader", "{{ .vars.copied }}"),
		"a computed value": prints("reader", "{{ .vars.computed }}"),
		"a condition":      {ID: "reader", Module: "shell", ModuleInputs: &modules.Shell{Script: "true"}, When: map[string]any{"equals": []any{"{{ .vars.is_set }}", true}}},
	}
	for name, reader := range readers {
		r := f.e.Run(context.Background(), f.job(f.call("deploy", inputs,
			config.Task{ID: "copy", SetVars: map[string]any{"copied": "{{ .inputs.token }}"}},
			config.Task{ID: "compute", SetVars: map[string]any{"computed": "prefix-{{ .vars.copied }}", "is_set": map[string]any{"matches": []any{"{{ .vars.copied }}", "^role-sentinel"}}}},
			said("between"), reader)))
		requireStatus(t, r, "succeeded")
		f.requireHidden(t, name, r, []string{"deploy/copy", "deploy/compute", "deploy/reader", "deploy"}, []string{"deploy/between"})
	}
	// Outside a role: the output of a node task that read the secret.
	r := f.e.Run(context.Background(), f.job(prints("reader", "{{ .secrets.token }}"), said("between"), prints("uses", "{{ .node_tasks.reader.stdout }}")))
	requireStatus(t, r, "succeeded")
	f.requireHidden(t, "the output of a node task that read the secret", r, []string{"reader", "uses"}, []string{"between"})
}

// A node task that starts after one that read a secret and may have left it
// on the node is hidden, so its output cannot name a file. The error names
// the output it read and the node task that read the secret, and points at
// no other value the field reads; the result says the same in
// secret_because.
func TestAnIdentifierFromAHiddenOutputNamesTheNodeTaskThatReadTheSecret(t *testing.T) {
	f := roleFixture{dir: t.TempDir()}
	inputs := map[string]any{"token": "{{ .secrets.token }}", "tokens": "{{ .secrets.tokens }}"}
	call := f.call("deploy", inputs,
		config.Task{ID: "write", Module: "file", ModuleInputs: &modules.File{Path: filepath.Join(f.dir, "private.conf"), Content: text("token={{ .inputs.token }}\n"), Mode: "0600"}},
		config.Task{ID: "name", Module: "shell", ModuleInputs: &modules.Shell{Script: "printf name"}},
		config.Task{ID: "uses", Module: "file", ModuleInputs: &modules.File{Path: f.dir + "/{{ .node_tasks.name.stdout }}", Content: text("public\n"), Mode: "0644"}})
	r := f.e.Run(context.Background(), f.job(call))
	requireStatus(t, r, "failed")
	want := "it reads .node_tasks.name.stdout, which holds a secret: the result of that node task is hidden because node task deploy/write read a secret before it started"
	results := resultsByTask(t, r, "deploy/name", "deploy/uses")
	if uses := results["uses"]; !strings.Contains(uses.Reason, want) || strings.Contains(uses.Reason, "value supplied at") {
		t.Fatalf("%+v", uses)
	}
	if !strings.HasPrefix(results["name"].SecretBecause, "node task deploy/write read a secret") {
		t.Fatalf("%+v", results["name"])
	}
}

// A failed node task whose result is hidden hands the node what the result
// leaves out, the secret included, and its reason and the node job's error
// name the file that keeps it; nothing that MMX reports holds the secret. A
// role call around such a node task names the same file and keeps nothing
// itself. A node task that reads a secret and succeeds, and a failed one
// that reads none, keep nothing.
func TestAFailedHiddenNodeTaskKeepsItsDetailsOnTheNodeForRoot(t *testing.T) {
	const file = "/run/mmx/private/test-hidden-details-of-failed-node-tasks.txt"
	const named = "details suppressed for a task using secrets; root can read them on the node in " + file + " for 72 hours, or until the node reboots"
	f := roleFixture{dir: t.TempDir()}
	var kept []protocol.Result
	f.e.KeepHiddenDetails = func(r protocol.Result) (string, error) {
		kept = append(kept, r)
		return file, nil
	}
	insert := config.Task{ID: "insert_rows", Module: "shell", Retry: &config.Retry{Disabled: true}, ModuleInputs: &modules.Shell{
		Script: `echo "ERROR:  invalid input syntax: $TOKEN" >&2; echo 'COPY 0'; exit 1`, Env: map[string]any{"TOKEN": "{{ .secrets.token }}"}}}

	r := f.e.Run(context.Background(), f.job(insert))
	requireStatus(t, r, "failed")
	saved, _ := json.Marshal(r)
	if len(kept) != 1 || kept[0].Task != "insert_rows" || kept[0].ExitCode != 1 || kept[0].Stderr != "ERROR:  invalid input syntax: "+roleToken+"\n" || kept[0].Stdout != "COPY 0\n" {
		t.Fatalf("kept %+v", kept)
	}
	if got := r.Results[0]; got.Reason != named || got.HiddenDetailsFile != file || got.Stderr != "[redacted]" || !strings.HasSuffix(r.Error, named) || strings.Contains(string(saved), roleToken) {
		t.Fatalf("result %+v\nerror %s", got, r.Error)
	}

	// In a role, the node task keeps its details, and the role call names them.
	kept = nil
	r = f.e.Run(context.Background(), f.job(f.call("load", map[string]any{"token": "{{ .secrets.token }}", "tokens": "{{ .secrets.tokens }}"},
		config.Task{ID: "insert_rows", Module: "shell", Retry: &config.Retry{Disabled: true}, ModuleInputs: &modules.Shell{
			Script: `echo "ERROR: $TOKEN" >&2; exit 1`, Env: map[string]any{"TOKEN": "{{ .inputs.token }}"}}})))
	requireStatus(t, r, "failed")
	byTask := map[string]protocol.Result{}
	for _, result := range r.Results {
		byTask[result.Task] = result
	}
	if len(kept) != 1 || kept[0].Task != "load/insert_rows" || byTask["load/insert_rows"].Reason != named || byTask["load"].Reason != named || !strings.HasSuffix(r.Error, named) {
		t.Fatalf("kept %d, results %+v, error %s", len(kept), r.Results, r.Error)
	}

	kept = nil
	succeeding := config.Task{ID: "print", Module: "shell", ModuleInputs: &modules.Shell{Script: `echo "$TOKEN" >&2`, Env: map[string]any{"TOKEN": "{{ .secrets.token }}"}}}
	requireStatus(t, f.e.Run(context.Background(), f.job(succeeding)), "succeeded")
	r = f.e.Run(context.Background(), f.job(config.Task{ID: "plain", Module: "shell", Retry: &config.Retry{Disabled: true}, ModuleInputs: &modules.Shell{Script: "echo plain >&2; exit 1"}}))
	requireStatus(t, r, "failed")
	if len(kept) != 0 || r.Results[0].HiddenDetailsFile != "" || r.Results[0].Stderr != "plain\n" {
		t.Fatalf("kept %d, result %+v", len(kept), r.Results[0])
	}
}

// An assert's failure text comes only from values, never from a file on the
// node, so it shows when it reads no secret value, also after a node task
// of its role that read the secret and when its condition reads a hidden
// result. The role call, node task block, and loop that fail because of it
// show the same reason, after the response traveled to the controller
// machine, and the node keeps nothing for root. An assert whose message
// reads the secret, or a result of a hidden node task, stays hidden.
func TestAnAssertShowsAFailureTextThatReadsNoSecretAfterARead(t *testing.T) {
	const suppressed = "details suppressed for a task using secrets"
	f := roleFixture{dir: t.TempDir()}
	var kept []protocol.Result
	f.e.KeepHiddenDetails = func(r protocol.Result) (string, error) {
		kept = append(kept, r)
		return "/run/mmx/private/test-hidden-details-of-failed-node-tasks.txt", nil
	}
	inputs := map[string]any{"token": "{{ .secrets.token }}", "tokens": "{{ .secrets.tokens }}"}
	write := config.Task{ID: "write", Module: "file", ModuleInputs: &modules.File{Path: filepath.Join(f.dir, "private.conf"), Content: text("token={{ .inputs.token }}\n"), Mode: "0600"}}
	// print comes after write, so its result is hidden.
	print := config.Task{ID: "print", Module: "shell", ModuleInputs: &modules.Shell{Script: "echo pending"}}
	check := func(message string) config.Task {
		return config.Task{ID: "check", Module: "assert", ModuleInputs: &modules.Assert{
			That: map[string]any{"equals": []any{"{{ .node_tasks.print.stdout }}", "approved\n"}}, Message: message}}
	}
	// run runs the role call on the node db01 and returns each result as the
	// controller process shows it: decoded from the saved response, in its
	// safe form.
	run := func(call config.Task) (map[string]protocol.Result, string) {
		t.Helper()
		kept = nil
		j := f.job(call)
		j.Node = "db01"
		r := f.e.Run(context.Background(), j)
		requireStatus(t, r, "failed")
		saved, err := json.Marshal(r)
		if err != nil || strings.Contains(string(saved), "role-sentinel") {
			t.Fatalf("a secret is in the saved response: %s %v", saved, err)
		}
		var received protocol.Response
		if err := protocol.Decode(saved, &received); err != nil {
			t.Fatal(err)
		}
		byTask := map[string]protocol.Result{}
		for _, result := range received.Results {
			byTask[result.Task] = result.Safe()
		}
		return byTask, received.Error
	}

	const message = "Postgres on db01 must restart to load its memory settings"
	defaultText := `assert.that is false: {"equals":["{{ .node_tasks.print.stdout }}","approved\n"]}`
	block := func(tasks ...config.Task) config.Task {
		return config.Task{TaskBlock: &config.TaskBlock{ID: "memory_settings", Changes: tasks}}
	}
	looped := check("Postgres on {{ .node.name }} must restart to load its memory settings")
	looped.Loop, looped.As = []any{"main"}, "cluster"
	for name, c := range map[string]struct {
		call config.Task
		// reasons are the results that fail, each with its whole reason.
		reasons map[string]string
	}{
		"a message that reads the node's name": {
			call: f.call("deploy", inputs, write, print, check("Postgres on {{ .node.name }} must restart to load its memory settings")),
			reasons: map[string]string{
				"deploy/check": message,
				"deploy":       "deploy/check: " + message,
			},
		},
		"no message": {
			call: f.call("deploy", inputs, write, print, check("")),
			reasons: map[string]string{
				"deploy/check": defaultText,
				"deploy":       "deploy/check: " + defaultText,
			},
		},
		"a node task block in the role": {
			call: f.call("deploy", inputs, write, print, block(check("Postgres on {{ .node.name }} must restart to load its memory settings"))),
			reasons: map[string]string{
				"deploy/memory_settings/changes/check": message,
				"deploy/memory_settings":               "deploy/memory_settings/changes/check: " + message,
				"deploy":                               "deploy/memory_settings: deploy/memory_settings/changes/check: " + message,
			},
		},
		"a loop in the role": {
			call: f.call("deploy", inputs, write, print, looped),
			reasons: map[string]string{
				"deploy/check[0]": message,
				"deploy/check":    message,
				"deploy":          "deploy/check: " + message,
			},
		},
	} {
		results, jobError := run(c.call)
		for task, want := range c.reasons {
			if got := results[task]; got.Status != "failed" || got.Reason != want {
				t.Errorf("%s: %s fails with %q (status %s), want %q", name, task, got.Reason, got.Status, want)
			}
		}
		// Apart from its reason, the role call stays hidden, and so does the
		// node task between the read and the assert.
		if !results["deploy"].Secret || !results["deploy/print"].Secret || jobError != "deploy: "+c.reasons["deploy"] || len(kept) != 0 {
			t.Errorf("%s: role call %+v, print %+v, node job error %q, kept %d", name, results["deploy"], results["deploy/print"], jobError, len(kept))
		}
	}

	for name, message := range map[string]string{
		"the secret":                     "The token {{ .inputs.token }} is refused on {{ .node.name }}",
		"a result of a hidden node task": "Postgres on {{ .node.name }} reports {{ .node_tasks.print.stdout }}",
	} {
		results, jobError := run(f.call("deploy", inputs, write, print, check(message)))
		for _, task := range []string{"deploy/check", "deploy"} {
			if !strings.HasPrefix(results[task].Reason, suppressed) {
				t.Errorf("an assert whose message reads %s: %s shows %q", name, task, results[task].Reason)
			}
		}
		if !strings.HasSuffix(jobError, results["deploy/check"].Reason) || len(kept) != 1 || kept[0].Task != "deploy/check" {
			t.Errorf("an assert whose message reads %s: node job error %q, kept %+v", name, jobError, kept)
		}
	}
}

// The promise reveals only command diagnostics; the task and values derived
// from its stdout remain secret. Both command modes use the same protection.
func TestShellOutputHoldsNoSecret(t *testing.T) {
	for _, dryrun := range []bool{false, true} {
		for _, promise := range []bool{false, true} {
			sh := &modules.Shell{Script: `printf '%s\npublic diagnostic\n' "$TOKEN"`, Dryrun: `printf '%s\npublic diagnostic\n' "$TOKEN"`, Env: map[string]any{"TOKEN": "{{ .secrets.token }}"}, OutputHoldsNoSecret: promise, ChangedWhen: false}
			j := job(config.Task{ID: "command", Module: "shell", ModuleInputs: sh}, config.Task{ID: "derived", Show: "{{ .node_tasks.command.stdout }}"})
			j.Dryrun = dryrun
			j.Scope = map[string]any{"secrets": map[string]any{"token": value.Sensitive("command-secret-value")}}
			j.Outputs = map[string]any{"printed": "{{ .node_tasks.command.stdout }}"}
			r := (&Engine{}).Run(context.Background(), j)
			requireStatus(t, r, "succeeded")
			got := r.Results[0].Safe()
			want := "[redacted]"
			if promise {
				want = "[redacted]\npublic diagnostic\n"
			}
			if !got.Secret || got.Stdout != want || !value.HasSecrets(r.Outputs["printed"]) || !r.Results[1].Secret {
				t.Fatalf("dryrun=%v promise=%v: command=%+v, outputs=%v", dryrun, promise, got, r.Outputs)
			}
			b, err := json.Marshal(r)
			if err != nil || strings.Contains(string(b), "command-secret-value") {
				t.Fatalf("secret in reported response: %s, %v", b, err)
			}
		}
	}
}

func TestSecretFileCheckOutputHoldsNoSecret(t *testing.T) {
	for _, dryrun := range []bool{false, true} {
		for _, promise := range []bool{false, true} {
			dir := t.TempDir()
			path := filepath.Join(dir, "settings")
			put(t, path, "original contents")
			check := `printf 'invalid setting: '; cat "$NEW_FILE"; printf 'check error: ' >&2; cat "$NEW_FILE" >&2; printf '%s' "$TOKEN" >&2; exit 1`
			j := job(config.Task{ID: "settings", Retry: &config.Retry{Disabled: true}, Module: "file", ModuleInputs: &modules.File{
				Path: path, Mode: "0600", Template: "settings.tmpl", TemplateText: "{{ .secrets.template_token }}",
				CheckNewFile: &modules.Check{Shell: &modules.Shell{Script: check, Dryrun: check, OutputHoldsNoSecret: promise, Env: map[string]any{"NEW_FILE": "{{ .new_file.path }}", "TOKEN": "{{ .secrets.env_token }}"}}},
			}})
			j.Scope = map[string]any{"secrets": map[string]any{"template_token": value.Sensitive("template-secret-value"), "env_token": value.Sensitive("env-secret-value")}}
			j.Dryrun = dryrun
			r := (&Engine{}).Run(context.Background(), j)
			requireStatus(t, r, "failed")
			got := r.Results[0].Safe()
			if !got.Secret || got.CheckNewFile == nil || got.FileChange == nil || got.FileChange.Diff != "" || got.FileChange.FullDiff != nil {
				t.Fatalf("dryrun=%v promise=%v: %+v", dryrun, promise, got)
			}
			if promise {
				if got.CheckNewFile.Stderr != "check error: [redacted]" || !strings.Contains(got.Reason, "check_new_file failed: ") || !strings.Contains(got.Reason, "check error: [redacted]") || !strings.Contains(r.Error, "check error: [redacted]") {
					t.Fatalf("dryrun=%v: reason=%q check=%+v error=%q", dryrun, got.Reason, got.CheckNewFile, r.Error)
				}
			} else if got.CheckNewFile.Stderr != "[redacted]" || !strings.Contains(got.Reason, "details suppressed") {
				t.Fatalf("without promise: %+v", got)
			}
			b, err := json.Marshal(r)
			if err != nil || strings.Contains(string(b), "template-secret-value") || strings.Contains(string(b), "env-secret-value") || read(t, path) != "original contents" {
				t.Fatalf("unsafe report or failed check installed file: %s, %v", b, err)
			}
		}
	}
}

func TestSecretFileSuccessfulCheckOutputHoldsNoSecret(t *testing.T) {
	for _, dryrun := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "settings")
		j := job(config.Task{Module: "file", ModuleInputs: &modules.File{
			Path: path, Mode: "0600", Template: "settings.tmpl", TemplateText: "{{ .secrets.token }}",
			CheckNewFile: &modules.Check{Shell: &modules.Shell{Script: "echo settings-valid", Dryrun: "echo settings-valid", OutputHoldsNoSecret: true}},
		}})
		j.Scope = map[string]any{"secrets": map[string]any{"token": value.Sensitive("template-secret-value")}}
		j.Dryrun = dryrun
		r := (&Engine{}).Run(context.Background(), j)
		requireStatus(t, r, "succeeded")
		got := r.Results[0].Safe()
		if !got.Secret || got.CheckNewFile == nil || got.CheckNewFile.Stdout != "settings-valid\n" || got.FileChange.Diff != "" {
			t.Fatalf("dryrun=%v: %+v", dryrun, got)
		}
	}
}

// A retry condition can read a secret after the command has already run.
// Redaction must cover the completed task and the retained tries too.
func TestPromisedOutputRedactsSecretReadByRetry(t *testing.T) {
	j := job(config.Task{Module: "shell", ModuleInputs: &modules.Shell{Script: "echo retry-secret-value", OutputHoldsNoSecret: true}, Retry: &config.Retry{MaxTries: 1, Until: map[string]any{"equals": []any{"{{ .secrets.token }}", "retry-secret-value"}}}})
	j.Scope = map[string]any{"secrets": map[string]any{"token": value.Sensitive("retry-secret-value")}}
	r := (&Engine{}).Run(context.Background(), j)
	requireStatus(t, r, "succeeded")
	got := r.Results[0].Safe()
	if !got.Secret || got.Stdout != "[redacted]\n" || len(got.Tries) != 1 || got.Tries[0].Stdout != "[redacted]\n" {
		t.Fatal(got)
	}
}

func TestPromisedFileCheckRedactsTrimmedSecretForms(t *testing.T) {
	for name, secret := range map[string]string{
		"trailing newline": "private-token\n",
		"three lines":      "  private-key-header\n private-key-body \n private-key-footer\n",
	} {
		for _, dryrun := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/dryrun=%v", name, dryrun), func(t *testing.T) {
				check := `printf 'check error: ' >&2; cat "$NEW_FILE" >&2; exit 1`
				if name == "three lines" {
					check = `printf 'check error: ' >&2; cat "$NEW_FILE" >&2; printf 'context\n%.0s' {1..19} >&2; exit 1`
				}
				j := job(config.Task{Module: "file", ModuleInputs: &modules.File{
					Path: filepath.Join(t.TempDir(), "settings"), Mode: "0600", Template: "settings.tmpl", TemplateText: "{{ .secrets.token }}",
					CheckNewFile: &modules.Check{Shell: &modules.Shell{Script: check, Dryrun: check, OutputHoldsNoSecret: true, Env: map[string]any{"NEW_FILE": "{{ .new_file.path }}"}}},
				}})
				j.Dryrun = dryrun
				j.Scope = map[string]any{"secrets": map[string]any{"token": value.Sensitive(secret)}}
				r := (&Engine{}).Run(context.Background(), j)
				requireStatus(t, r, "failed")
				got := r.Results[0].Safe()
				for _, text := range []string{got.Reason, got.CheckNewFile.Stderr, r.Error} {
					if !strings.Contains(text, "[redacted]") {
						t.Errorf("missing redaction: %q", text)
					}
					for _, line := range strings.Split(secret, "\n") {
						if line = strings.TrimSpace(line); line != "" && strings.Contains(text, line) {
							t.Errorf("secret line leaked: %q", text)
						}
					}
				}
			})
		}
	}
}

func TestPromisedShellRedactsSecretMappingKeysAndEnvironmentText(t *testing.T) {
	for _, secret := range []any{map[string]any{"private-api-token": map[string]any{}}, map[string]any{"private-\"token": []any{}}, []any{map[string]any{"private-list-token": map[string]any{}}}} {
		for _, dryrun := range []bool{false, true} {
			j := job(config.Task{Module: "shell", ModuleInputs: &modules.Shell{Script: `printf '%s\n' "$TOKEN"`, Dryrun: `printf '%s\n' "$TOKEN"`, OutputHoldsNoSecret: true, Env: map[string]any{"TOKEN": "{{ .secrets.token }}"}}})
			j.Dryrun = dryrun
			j.Scope = map[string]any{"secrets": map[string]any{"token": value.Sensitive(secret)}}
			r := (&Engine{}).Run(context.Background(), j)
			requireStatus(t, r, "succeeded")
			if got := r.Results[0].Safe().Stdout; got != "[redacted]\n" {
				t.Errorf("dryrun=%v: secret environment text leaked: %q", dryrun, got)
			}
		}
	}
}

func TestPromisedShellRedactsOtherJobSecrets(t *testing.T) {
	for _, dryrun := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "other-token")
		put(t, path, "other-job-secret")
		j := job(config.Task{Module: "shell", ModuleInputs: &modules.Shell{Script: `printf '%s\n' "$TOKEN"; cat "$OTHER_FILE"`, Dryrun: `printf '%s\n' "$TOKEN"; cat "$OTHER_FILE"`, OutputHoldsNoSecret: true, Env: map[string]any{"TOKEN": "{{ .secrets.used }}", "OTHER_FILE": path}}})
		j.Dryrun = dryrun
		j.Scope = map[string]any{"secrets": map[string]any{"used": value.Sensitive("used-job-secret"), "other": value.Sensitive("other-job-secret")}}
		r := (&Engine{}).Run(context.Background(), j)
		requireStatus(t, r, "succeeded")
		if got := r.Results[0].Safe().Stdout; got != "[redacted]\n[redacted]" {
			t.Errorf("dryrun=%v: other job secret leaked: %q", dryrun, got)
		}
	}
}
