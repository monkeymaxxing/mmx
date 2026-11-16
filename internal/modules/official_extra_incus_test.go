package modules

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"mmx/internal/protocol"
)

// The official extra modules incus_instance and incus_profile, against a
// stand-in incus program. Everything the stand-in prints is what the real
// incus printed on a test guest: the files in testdata/incus/<version> hold
// the standard output and the standard error of Incus 6.0.0 (Ubuntu 24.04),
// the oldest version that the suite incus_instances.py runs on, and of Incus
// 7.5.1, the newest that a test guest had. In them only the names of the
// measuring instance and of its profile were replaced, by shop-test, shop-vm,
// and web.
// The suite proves both modules on real Incus servers; these tests cover the
// rejected inputs, each command with its standard input, and the failures.
//
// MMX tries a failed task of a script again, and the script then reads the
// node again. So a test of a failure after a change also runs the script a
// second time against what the first run left: that is MMX's next try, and
// the test shows what it would have reported.

// incusVersions are the versions whose output the tests have.
var incusVersions = []string{"6.0.0", "7.5.1"}

const (
	incusInstancePath = "/1.0/instances/shop-test?project=default"
	incusProfilePath  = "/1.0/profiles/web?project=default"
	incusIsMissing    = "the program incus is not installed; install the package incus-base (Debian) or incus " +
		"(Ubuntu and the Red Hat family) in an earlier node task"
)

// incusScene is the stand-in incus program of one test: what it prints for
// each "incus query --raw", and what each changing command does.
type incusScene struct {
	t         *testing.T
	version   string
	directory string
	calls     func() []string
}

var notLetterOrDigit = regexp.MustCompile(`[^A-Za-z0-9]`)

// needsTheIncusModules skips a test where the scripts are not: they and the
// measured outputs are files of the repository, and the suite linux_code.py
// runs this test program on a test node without the repository.
func needsTheIncusModules(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(officialExtraModulesDirectory, "incus_instance", ModuleScriptFile)); errors.Is(err, fs.ErrNotExist) {
		t.Skip("the test binary runs outside the repository, as on a test node")
	}
}

func newIncusScene(t *testing.T, version string) *incusScene {
	t.Helper()
	needsTheIncusModules(t)
	s := &incusScene{t: t, version: version, directory: t.TempDir()}
	// The stand-in finds what to do for its arguments in a file named for
	// them, and keeps the standard input it got beside it. Arguments that
	// the test did not expect fail the command and name themselves.
	s.calls = stubTools(t, map[string]string{"incus": `SCENE='` + s.directory + `'
if [ -e "$SCENE/the-server-does-not-answer" ]; then . "$SCENE/the-server-does-not-answer"; fi
key=$(printf '%s' "$*" | tr -c 'A-Za-z0-9' '_')
if [ ! -e "$SCENE/do-$key" ]; then
  echo "the test has no stand-in for: incus $*" >&2
  exit 64
fi
cat > "$SCENE/stdin-$key"
. "$SCENE/do-$key"`})
	return s
}

// printed is the path of what the real incus of the scene's version printed
// for the case name.
func (s *incusScene) printed(name string) string {
	s.t.Helper()
	path, err := filepath.Abs(filepath.Join("testdata", "incus", s.version, name))
	if err != nil {
		s.t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		s.t.Fatal(err)
	}
	return path
}

func (s *incusScene) write(file, content string) {
	s.t.Helper()
	if err := os.WriteFile(filepath.Join(s.directory, file), []byte(content), 0o644); err != nil {
		s.t.Fatal(err)
	}
}

func incusKey(command string) string { return notLetterOrDigit.ReplaceAllString(command, "_") }

// answers makes "incus query --raw local:<path>" print the measured answer
// of the case name, with the exit code 0.
func (s *incusScene) answers(path, name string) {
	s.t.Helper()
	b, err := os.ReadFile(s.printed(name + ".json"))
	if err != nil {
		s.t.Fatal(err)
	}
	s.answersText(path, string(b))
}

// answersText makes the query print text: a measured answer in which a test
// changed one value that no test guest had, such as a status.
func (s *incusScene) answersText(path, text string) {
	s.t.Helper()
	key := incusKey("query --raw local:" + path)
	s.write("out-"+key, text)
	s.write("do-"+key, `cat "$SCENE/out-`+key+`"`)
}

// measured returns the measured answer of the case name, to change a value.
func (s *incusScene) measured(name string) string {
	s.t.Helper()
	b, err := os.ReadFile(s.printed(name + ".json"))
	if err != nil {
		s.t.Fatal(err)
	}
	return string(b)
}

// then is the shell text that changes what later queries print: pairs of a
// query path and a case name.
func (s *incusScene) then(changes []string) string {
	s.t.Helper()
	text := ""
	for i := 0; i+1 < len(changes); i += 2 {
		text += `cp '` + s.printed(changes[i+1]+".json") + `' "$SCENE/out-` + incusKey("query --raw local:"+changes[i]) + `"` + "\n"
	}
	return text
}

// succeeds makes the command, written as its arguments, exit 0 and print
// nothing, as the real commands of the modules do. changes are pairs of a
// query path and the case that the query answers with afterwards.
func (s *incusScene) succeeds(command string, changes ...string) {
	s.t.Helper()
	s.write("do-"+incusKey(command), s.then(changes)+"exit 0")
}

// fails makes the command print what the real incus printed for the failure
// name and exit 1.
func (s *incusScene) fails(command, name string, changes ...string) {
	s.t.Helper()
	text := s.then(changes)
	if _, err := os.Stat(filepath.Join("testdata", "incus", s.version, name+".stdout")); err == nil {
		text += "cat '" + s.printed(name+".stdout") + "'\n"
	}
	s.write("do-"+incusKey(command), text+"cat '"+s.printed(name+".stderr")+"' >&2\nexit 1")
}

// theServerDoesNotAnswer makes every command fail as while the Incus server
// is not started.
func (s *incusScene) theServerDoesNotAnswer() {
	s.t.Helper()
	text := ""
	if _, err := os.Stat(filepath.Join("testdata", "incus", s.version, "the_server_does_not_answer.stdout")); err == nil {
		text = "cat '" + s.printed("the_server_does_not_answer.stdout") + "'\n"
	}
	s.write("the-server-does-not-answer", text+"cat '"+s.printed("the_server_does_not_answer.stderr")+"' >&2\nexit 1")
}

// theServerStopsAnsweringAt makes the command, and every command after it,
// fail as while the Incus server is not started: the server went away during
// a task. theServerAnswersAgain ends that.
func (s *incusScene) theServerStopsAnsweringAt(command string) {
	s.t.Helper()
	s.write("do-"+incusKey(command), s.fromThenOnTheServerDoesNotAnswer()+`. "$SCENE/the-server-does-not-answer"`)
}

// theServerStopsAnsweringAfter makes the command succeed and every command
// after it fail as while the Incus server is not started.
func (s *incusScene) theServerStopsAnsweringAfter(command string) {
	s.t.Helper()
	s.write("do-"+incusKey(command), s.fromThenOnTheServerDoesNotAnswer()+"exit 0")
}

// fromThenOnTheServerDoesNotAnswer is the shell text that makes every later
// command fail as while the Incus server is not started.
func (s *incusScene) fromThenOnTheServerDoesNotAnswer() string {
	s.t.Helper()
	s.theServerDoesNotAnswer()
	stopped := filepath.Join(s.directory, "the-server-does-not-answer")
	if err := os.Rename(stopped, stopped+"-from-then-on"); err != nil {
		s.t.Fatal(err)
	}
	return `cp "$SCENE/the-server-does-not-answer-from-then-on" "$SCENE/the-server-does-not-answer"` + "\n"
}

func (s *incusScene) theServerAnswersAgain() {
	s.t.Helper()
	if err := os.Remove(filepath.Join(s.directory, "the-server-does-not-answer")); err != nil {
		s.t.Fatal(err)
	}
}

// oneTry checks a failure that the script asked MMX not to try again
// (try_again: False): MMX's next try would find what this try left and hide
// the failure. Such a failure is an ordinary one, of no failure kind.
func oneTry(t *testing.T, r protocol.Result) {
	t.Helper()
	if r.Status != "failed" || !r.NoOtherTry || r.FailureKind != "" {
		t.Fatalf("status %s, no other try %v, failure kind %q, reason %q; want a failure with try_again: False", r.Status, r.NoOtherTry, r.FailureKind, r.Reason)
	}
}

// triedAgain checks an ordinary failure, which MMX tries again.
func triedAgain(t *testing.T, r protocol.Result) {
	t.Helper()
	if r.Status != "failed" || r.NoOtherTry || r.FailureKind != "" {
		t.Fatalf("status %s, no other try %v, failure kind %q, reason %q; want a failure that MMX tries again", r.Status, r.NoOtherTry, r.FailureKind, r.Reason)
	}
}

// refusedForGood checks a failure that another try cannot fix
// (invalid_inputs): MMX tries a task with this failure kind once.
func refusedForGood(t *testing.T, r protocol.Result) {
	t.Helper()
	if r.Status != "failed" || r.FailureKind != "config" {
		t.Fatalf("status %s, failure kind %q, reason %q; want the failure kind configuration", r.Status, r.FailureKind, r.Reason)
	}
}

// standardInput is what the command got on its standard input, decoded.
func (s *incusScene) standardInput(command string) map[string]any {
	s.t.Helper()
	b, err := os.ReadFile(filepath.Join(s.directory, "stdin-"+incusKey(command)))
	if err != nil {
		s.t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(b, &document); err != nil {
		s.t.Fatalf("the standard input of incus %s is no JSON document: %q", command, b)
	}
	return document
}

func (s *incusScene) run(module string, inputs map[string]any, dryrun bool) protocol.Result {
	s.t.Helper()
	return runOfficialExtraModule(s.t, module, inputs, dryrun)
}

// ran checks the calls of incus so far, in order.
func (s *incusScene) ran(want ...string) {
	s.t.Helper()
	var got []string
	for _, call := range s.calls() {
		got = append(got, strings.TrimPrefix(call, "incus "))
	}
	if !slices.Equal(got, want) {
		s.t.Fatalf("incus was called with\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

func eachIncusVersion(t *testing.T, test func(t *testing.T, s *incusScene)) {
	t.Helper()
	for _, version := range incusVersions {
		t.Run("Incus "+version, func(t *testing.T) { test(t, newIncusScene(t, version)) })
	}
}

func incusResultIs(t *testing.T, r protocol.Result, status, change, reason string) {
	t.Helper()
	if r.Status != status || r.Change != change || r.Reason != reason {
		t.Fatalf("status %s, change %s, reason %q\nwant status %s, change %s, reason %q", r.Status, r.Change, r.Reason, status, change, reason)
	}
}

func query(path string) string { return "query --raw local:" + path }

// incusDoesNotAnswer is what incus prints while its server is not started.
const incusDoesNotAnswer = "Error: The incus daemon doesn't appear to be started (socket path: /var/lib/incus/unix.socket)"

func createdIs(t *testing.T, r protocol.Result, want bool) {
	t.Helper()
	if r.ModuleOutputs["created"] != want {
		t.Fatalf("the output created is %v, want %v; the reason is %q", r.ModuleOutputs["created"], want, r.Reason)
	}
}

// Both scripts hold the same helper functions, because a script has no load.
// The copies must stay identical.
func TestIncusScriptsShareIdenticalHelperFunctions(t *testing.T) {
	t.Parallel()
	needsTheIncusModules(t)
	text := func(module, function string) string {
		b, err := os.ReadFile(filepath.Join(officialExtraModulesDirectory, module, ModuleScriptFile))
		if err != nil {
			t.Fatal(err)
		}
		_, after, found := strings.Cut(string(b), "\ndef "+function+"(")
		if !found {
			t.Fatalf("%s has no function %s", module, function)
		}
		body, _, _ := strings.Cut(after, "\ndef ")
		body, _, _ = strings.Cut(body, "\n# ")
		return strings.TrimSpace(body)
	}
	for _, function := range []string{"problem_with_config_and_devices", "project_is_missing", "result_without_the_program", "percent_encoded", "message_of", "read_from_incus",
		"reading_failed", "change", "failed", "said", "document_for_the_task", "differences_between", "differences_of_a_mapping"} {
		if text("incus_instance", function) != text("incus_profile", function) {
			t.Errorf("the function %s differs between the scripts of incus_instance and incus_profile", function)
		}
	}
}

// The page of each module shows, under "Set up", exactly the node tasks of the
// first fleet stage of the example project incus-host, which the suite
// incus_instances.py runs on every distribution: so the pages are tested.
// The file that keeps apt from installing recommended packages is the one
// that the suite writes.
func TestIncusPagesShowTheSetUpThatTheSuiteRuns(t *testing.T) {
	t.Parallel()
	needsTheIncusModules(t)
	read := func(path string) string {
		b, err := os.ReadFile(filepath.Join(officialExtraModulesDirectory, "..", path))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	_, stage, _ := strings.Cut(read("examples/incus-host/playbooks/incus_host.yml"), "    node_tasks:\n")
	stage, _, found := strings.Cut(stage, "\n\n  # One profile and one instance.")
	if !found {
		t.Fatal("the example's playbook has no second fleet stage under the comment that this test looks for")
	}
	var tasks []string
	for _, line := range strings.Split(stage, "\n") {
		tasks = append(tasks, strings.TrimPrefix(line, "      "))
	}
	suite := read("tests/integration/incus_instances.py")
	for _, module := range []string{"incus_instance", "incus_profile"} {
		_, setUp, _ := strings.Cut(read("official_extra_modules/"+module+"/README.md"), "\n## Set up\n")
		setUp, _, _ = strings.Cut(setUp, "\n## ")
		blocks := strings.Split(setUp, "```yaml\n")
		if len(blocks) != 3 {
			t.Fatalf("%s: the section Set up has %d YAML blocks, want the node tasks and the apt file", module, len(blocks)-1)
		}
		shown, _, _ := strings.Cut(blocks[1], "\n```")
		if shown != strings.Join(tasks, "\n") {
			t.Errorf("%s: the node tasks under Set up are not those of examples/incus-host", module)
		}
		for _, line := range []string{"path: /etc/apt/apt.conf.d/90-install-no-recommended-packages", `APT::Install-Recommends "false";`} {
			if !strings.Contains(blocks[2], line) || !strings.Contains(suite, strings.TrimPrefix(line, "path: ")) {
				t.Errorf("%s: the page and the suite do not both hold %s", module, line)
			}
		}
	}
}

// A rule that the description cannot state fails the task before any command,
// with the input's name, and is not tried again.
func TestIncusInstanceRefusesInputsThatBreakARule(t *testing.T) {
	t.Parallel()
	needsTheIncusModules(t)
	calls := stubTools(t, map[string]string{"incus": `echo "incus must not run" >&2; exit 64`})
	device := map[string]any{"web": map[string]any{"type": "proxy"}}
	for reason, inputs := range map[string]map[string]any{
		"name: must not be empty":               {"instance_type": "container", "profiles": []any{"default"}, "name": "", "state": "running"},
		"image_when_created: must not be empty": {"instance_type": "container", "profiles": []any{"default"}, "name": "shop-test", "state": "running", "image_when_created": ""},
		"incus_project: must not be empty":      {"instance_type": "container", "profiles": []any{"default"}, "name": "shop-test", "state": "running", "incus_project": ""},
		"image_when_created: must not start with a hyphen, which the program incus would read as one of its options":                          {"instance_type": "container", "profiles": []any{"default"}, "name": "shop-test", "state": "running", "image_when_created": "--project"},
		"incus_project: must not start with a hyphen, which the program incus would read as one of its options":                               {"instance_type": "container", "profiles": []any{"default"}, "name": "shop-test", "state": "running", "incus_project": "-shop"},
		"state: deleted_with_all_its_data accepts only name and incus_project; remove image_when_created":                                     {"name": "shop-test", "state": "deleted_with_all_its_data", "image_when_created": "images:debian/13"},
		"wait_for_shutdown_at_most: must be a positive duration of whole seconds, such as 30s or 2m; incus stop takes whole seconds":          {"instance_type": "container", "profiles": []any{"default"}, "name": "shop-test", "state": "stopped", "wait_for_shutdown_at_most": "0s"},
		"config_keys_not_listed: is required with config: remove removes every key that config does not list, and keep leaves those keys":     {"instance_type": "container", "profiles": []any{"default"}, "name": "shop-test", "state": "running", "config": map[string]any{"limits.cpu": "2"}},
		"config: is required with config_keys_not_listed":                                                                                     {"instance_type": "container", "profiles": []any{"default"}, "name": "shop-test", "state": "running", "config_keys_not_listed": "keep"},
		"devices_not_listed: is required with devices: remove removes every device that devices does not list, and keep leaves those devices": {"instance_type": "container", "profiles": []any{"default"}, "name": "shop-test", "state": "running", "devices": device},
		"devices: is required with devices_not_listed":                                                                                        {"instance_type": "container", "profiles": []any{"default"}, "name": "shop-test", "state": "running", "devices_not_listed": "remove"},
		"config: the key user.note has an empty value, and Incus stores no key without a value; leave the key out, and write config_keys_not_listed: remove to remove a key that the task does not list": {"instance_type": "container", "profiles": []any{"default"},
			"name": "shop-test", "state": "running", "config": map[string]any{"user.note": ""}, "config_keys_not_listed": "keep"},
		"devices: expected a mapping of device names, each with a mapping of Incus device options": {"instance_type": "container", "profiles": []any{"default"},
			"name": "shop-test", "state": "running", "devices": []any{"web"}, "devices_not_listed": "keep"},
		"devices: the device web must be a mapping of Incus device options, such as type: disk": {"instance_type": "container", "profiles": []any{"default"},
			"name": "shop-test", "state": "running", "devices": map[string]any{"web": "proxy"}, "devices_not_listed": "keep"},
		`devices: the option readonly of the device data must be text; write a number or true and false in quotes, such as "true"`: {"instance_type": "container", "profiles": []any{"default"},
			"name": "shop-test", "state": "running", "devices": map[string]any{"data": map[string]any{"type": "disk", "readonly": true}}, "devices_not_listed": "keep"},
		"devices: the option readonly of the device data has an empty value, and Incus stores no option without a value; leave the option out": {"instance_type": "container", "profiles": []any{"default"},
			"name": "shop-test", "state": "running", "devices": map[string]any{"data": map[string]any{"type": "disk", "readonly": ""}}, "devices_not_listed": "keep"},
	} {
		for _, dryrun := range []bool{true, false} {
			r := runOfficialExtraModule(t, "incus_instance", inputs, dryrun)
			if r.Status != "failed" || r.FailureKind != "config" || r.Change != "unchanged" || r.Reason != "incus_instance."+reason {
				t.Errorf("dryrun %v: %s %s %s %q\nwant the reason %q", dryrun, r.Status, r.FailureKind, r.Change, r.Reason, "incus_instance."+reason)
			}
		}
	}
	if got := calls(); got != nil {
		t.Fatalf("incus ran: %q", got)
	}
}

// Without the program incus, a run fails once as a missing prerequisite and a
// dryrun is not checked: an earlier node task may install the package.
func TestIncusModulesNameTheMissingProgram(t *testing.T) {
	t.Parallel()
	needsTheIncusModules(t)
	setWorkerEnvironment(t, "PATH", t.TempDir())
	for module, inputs := range map[string]map[string]any{
		"incus_instance": {"instance_type": "container", "profiles": []any{"default"}, "name": "shop-test", "state": "running", "image_when_created": "images:debian/13"},
		"incus_profile":  {"name": "web"},
	} {
		r := runOfficialExtraModule(t, module, inputs, false)
		if r.Status != "failed" || r.FailureKind != "missing_prerequisite" || r.Reason != module+": "+incusIsMissing {
			t.Errorf("%s: %s %s %q", module, r.Status, r.FailureKind, r.Reason)
		}
		r = runOfficialExtraModule(t, module, inputs, true)
		incusResultIs(t, r, "not_checked", "unknown", module+": "+incusIsMissing)
	}
}

// While the Incus server does not answer, a dryrun is not checked and a run
// fails with incus's own message. From 6.23 on, incus also prints its usage
// on standard output, which is not part of the message.
func TestIncusModulesWhileTheServerDoesNotAnswer(t *testing.T) {
	t.Parallel()
	eachIncusVersion(t, func(t *testing.T, s *incusScene) {
		s.theServerDoesNotAnswer()
		message := "the Incus server on this node does not answer: Error: The incus daemon doesn't appear to be started (socket path: /var/lib/incus/unix.socket)"
		for _, module := range []string{"incus_instance", "incus_profile"} {
			inputs := map[string]any{"name": "web"}
			if module == "incus_instance" {
				inputs = map[string]any{"instance_type": "container", "profiles": []any{"default"}, "name": "shop-test", "state": "running", "image_when_created": "images:debian/13"}
			}
			incusResultIs(t, s.run(module, inputs, true), "not_checked", "unknown", module+": "+message)
			r := s.run(module, inputs, false)
			incusResultIs(t, r, "failed", "unchanged", module+": "+message)
			if r.FailureKind != "" {
				t.Fatalf("a server that does not answer is an ordinary failure, which is tried again; got %s", r.FailureKind)
			}
		}
		s.ran(query(incusInstancePath), query(incusInstancePath), query(incusProfilePath), query(incusProfilePath))
	})

	// The first incus command of the account root prints hints for a new
	// user before its error (Incus 6.23 on Rocky Linux 9): the reason holds
	// the error alone.
	s := newIncusScene(t, "6.23")
	s.theServerDoesNotAnswer()
	incusResultIs(t, s.run("incus_profile", map[string]any{"name": "web"}, true), "not_checked", "unknown",
		"incus_profile: the Incus server on this node does not answer: Error: The incus daemon doesn't appear to be started (socket path: /var/lib/incus/unix.socket)")
}

// An instance that is as the task says: one read and no changing command.
func TestIncusInstanceUnchanged(t *testing.T) {
	t.Parallel()
	eachIncusVersion(t, func(t *testing.T, s *incusScene) {
		s.answers(incusInstancePath, "instance_running")
		inputs := map[string]any{"instance_type": "container",
			"name": "shop-test", "state": "running", "image_when_created": "mmx-test", "profiles": []any{"default"},
			"config":                 map[string]any{"limits.memory": "128MiB", "raw.lxc": "lxc.signal.halt=SIGTERM", "environment.IMAGE_VERSION": "7"},
			"config_keys_not_listed": "remove",
			"devices":                map[string]any{"web": map[string]any{"type": "proxy", "listen": "tcp:127.0.0.1:18080", "connect": "tcp:127.0.0.1:8080"}},
			"devices_not_listed":     "remove",
		}
		for _, dryrun := range []bool{true, false} {
			r := s.run("incus_instance", inputs, dryrun)
			incusResultIs(t, r, "succeeded", "unchanged", "")
			if r.ModuleOutputs["created"] != false || r.Fingerprint == "" {
				t.Fatalf("%+v", r)
			}
		}
		s.ran(query(incusInstancePath), query(incusInstancePath))
	})
}

// A missing instance is created from its image with its profiles,
// configuration, and devices in one document on standard input, and started.
func TestIncusInstanceCreatesAndStartsAnInstance(t *testing.T) {
	t.Parallel()
	eachIncusVersion(t, func(t *testing.T, s *incusScene) {
		alias := "/1.0/images/aliases/mmx-test?project=default"
		s.answers(incusInstancePath, "instance_missing")
		s.answers(alias, "image_alias_of_a_container")
		initialize := "init -q --project default local:mmx-test local:shop-test"
		s.succeeds(initialize, incusInstancePath, "instance_new")
		s.succeeds("start --project default local:shop-test", incusInstancePath, "instance_running")
		inputs := map[string]any{"instance_type": "container",
			"name": "shop-test", "state": "running", "image_when_created": "mmx-test", "profiles": []any{"default"},
			"config":                 map[string]any{"limits.memory": "128MiB", "environment.DATABASE_PASSWORD": "sentinel-value"},
			"config_keys_not_listed": "remove",
			"devices":                map[string]any{"web": map[string]any{"type": "proxy", "listen": "tcp:127.0.0.1:18080", "connect": "tcp:127.0.0.1:8080"}},
			"devices_not_listed":     "remove",
		}

		// A dryrun reads, says what a run would do, and runs no changing command.
		r := s.run("incus_instance", inputs, true)
		incusResultIs(t, r, "succeeded", "would_change", "would create the container shop-test from the image mmx-test; would start it")
		if r.ModuleOutputs["created"] != true {
			t.Fatalf("%+v", r.ModuleOutputs)
		}
		s.ran(query(incusInstancePath), query(alias))

		r = s.run("incus_instance", inputs, false)
		incusResultIs(t, r, "succeeded", "changed", "created the container shop-test from the image mmx-test; started it")
		if r.ModuleOutputs["created"] != true {
			t.Fatalf("%+v", r.ModuleOutputs)
		}
		s.ran(query(incusInstancePath), query(alias),
			query(incusInstancePath), query(alias), initialize, query(incusInstancePath), "start --project default local:shop-test")
		document := s.standardInput(initialize)
		want := map[string]any{"profiles": inputs["profiles"], "config": inputs["config"], "devices": inputs["devices"]}
		if a, b := mustJSON(t, document), mustJSON(t, want); a != b {
			t.Fatalf("incus init got the document %s, want %s", a, b)
		}
		// The value is on standard input only: on no command line, and so
		// not in the difference that MMX shows.
		if strings.Contains(strings.Join(s.calls(), "\n")+r.Diff+r.Reason, "sentinel-value") {
			t.Fatalf("a configuration value is outside standard input: %q %q", s.calls(), r.Diff)
		}
	})
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// An image of another server is not asked for its type before; an empty
// list of profiles is the option --no-profiles and no profiles in the
// document; state stopped does not start the new instance; and an Incus
// project is in every command.
func TestIncusInstanceCreatesAnInstanceWithoutProfilesInAProject(t *testing.T) {
	t.Parallel()
	eachIncusVersion(t, func(t *testing.T, s *incusScene) {
		path := "/1.0/instances/shop-test?project=shop"
		s.answers("/1.0/projects/shop", "project_shop")
		s.answers(path, "instance_missing")
		initialize := "init -q --project shop images:debian/13 local:shop-test --no-profiles"
		s.succeeds(initialize, path, "instance_new")
		root := map[string]any{"root": map[string]any{"type": "disk", "path": "/", "pool": "default"}}
		r := s.run("incus_instance", map[string]any{"instance_type": "container", "name": "shop-test", "state": "stopped", "image_when_created": "images:debian/13", "incus_project": "shop",
			"profiles": []any{}, "devices": root, "devices_not_listed": "remove"}, false)
		incusResultIs(t, r, "succeeded", "changed", "created the container shop-test from the image images:debian/13")
		s.ran(query("/1.0/projects/shop"), query(path), initialize, query(path))
		if a, b := mustJSON(t, s.standardInput(initialize)), mustJSON(t, map[string]any{"devices": root}); a != b {
			t.Fatalf("incus init got the document %s, want %s", a, b)
		}
	})
}

// A missing Incus project is a missing prerequisite: an earlier node task may
// create it.
func TestIncusModulesNameAMissingProject(t *testing.T) {
	t.Parallel()
	eachIncusVersion(t, func(t *testing.T, s *incusScene) {
		s.answers("/1.0/projects/staging", "project_missing")
		reason := "the Incus project staging does not exist; create it in an earlier node task, such as a shell task that runs incus project create"
		for module, inputs := range map[string]map[string]any{
			"incus_instance": {"instance_type": "container", "profiles": []any{"default"}, "name": "shop-test", "state": "running", "image_when_created": "images:debian/13", "incus_project": "staging"},
			"incus_profile":  {"name": "web", "incus_project": "staging"},
		} {
			incusResultIs(t, s.run(module, inputs, true), "not_checked", "unknown", module+": "+reason)
			if r := s.run(module, inputs, false); r.Status != "failed" || r.FailureKind != "missing_prerequisite" || r.Reason != module+": "+reason {
				t.Fatalf("%s: %s %s %q", module, r.Status, r.FailureKind, r.Reason)
			}
		}
	})
}

// A task that would create an instance and names no image fails before any
// change, and so does an image of the other type.
func TestIncusInstanceRefusesToCreateAnInstanceItCannotCreate(t *testing.T) {
	t.Parallel()
	eachIncusVersion(t, func(t *testing.T, s *incusScene) {
		s.answers(incusInstancePath, "instance_missing")
		for _, dryrun := range []bool{true, false} {
			r := s.run("incus_instance", map[string]any{"instance_type": "container", "profiles": []any{"default"}, "name": "shop-test", "state": "running"}, dryrun)
			incusResultIs(t, r, "failed", "unchanged", "incus_instance.image_when_created: is required to create the instance shop-test")
			if r.FailureKind != "config" {
				t.Fatal(r.FailureKind)
			}
		}
		alias := "/1.0/images/aliases/mmx-test?project=default"
		s.answers(alias, "image_alias_of_a_container")
		r := s.run("incus_instance", map[string]any{"profiles": []any{"default"}, "name": "shop-test", "state": "running", "image_when_created": "local:mmx-test", "instance_type": "virtual_machine"}, false)
		incusResultIs(t, r, "failed", "unchanged",
			"incus_instance.instance_type: the task asks for a virtual machine, and the image mmx-test is the image of a container; Incus creates an instance of the type of its image")
		s.ran(query(incusInstancePath), query(incusInstancePath), query(incusInstancePath), query(alias))
	})
}

// An image of the node's own Incus that Incus does not have is a missing
// prerequisite, because an earlier node task may import it: a dryrun is not
// checked, and a run fails once and creates nothing. The module asks for the
// image by its alias and then by its fingerprint.
func TestIncusInstanceNamesALocalImageThatIncusDoesNotHave(t *testing.T) {
	t.Parallel()
	eachIncusVersion(t, func(t *testing.T, s *incusScene) {
		alias := "/1.0/images/aliases/nothing?project=default"
		image := "/1.0/images/nothing?project=default"
		s.answers(incusInstancePath, "instance_missing")
		s.answers(alias, "image_alias_missing")
		s.answers(image, "image_missing")
		reason := "incus_instance: the Incus server on this node has no image nothing; import it in an earlier node task, or name an image of another server, such as images:debian/13"
		var want []string
		for _, name := range []string{"nothing", "local:nothing"} {
			inputs := map[string]any{"instance_type": "container", "profiles": []any{"default"}, "name": "shop-test", "state": "running", "image_when_created": name}
			incusResultIs(t, s.run("incus_instance", inputs, true), "not_checked", "unknown", reason)
			r := s.run("incus_instance", inputs, false)
			incusResultIs(t, r, "failed", "unchanged", reason)
			if r.FailureKind != "missing_prerequisite" {
				t.Fatal(r.FailureKind)
			}
			want = append(want, query(incusInstancePath), query(alias), query(image), query(incusInstancePath), query(alias), query(image))
		}
		s.ran(want...)

		// An image by the first characters of its fingerprint: no alias has
		// that name, and the image says its type.
		var answer struct {
			Metadata struct {
				Fingerprint string `json:"fingerprint"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal([]byte(s.measured("image_of_a_container")), &answer); err != nil || len(answer.Metadata.Fingerprint) != 64 {
			t.Fatalf("%v %q", err, answer.Metadata.Fingerprint)
		}
		fingerprint := answer.Metadata.Fingerprint[:12]
		s.answers("/1.0/images/aliases/"+fingerprint+"?project=default", "image_alias_missing")
		s.answers("/1.0/images/"+fingerprint+"?project=default", "image_of_a_container")
		inputs := map[string]any{"profiles": []any{"default"}, "name": "shop-test", "state": "stopped", "image_when_created": fingerprint, "instance_type": "virtual_machine"}
		r := s.run("incus_instance", inputs, false)
		incusResultIs(t, r, "failed", "unchanged",
			"incus_instance.instance_type: the task asks for a virtual machine, and the image "+fingerprint+" is the image of a container; Incus creates an instance of the type of its image")
		initialize := "init -q --project default local:" + fingerprint + " local:shop-test"
		s.succeeds(initialize, incusInstancePath, "instance_new")
		inputs["instance_type"] = "container"
		r = s.run("incus_instance", inputs, false)
		incusResultIs(t, r, "succeeded", "changed", "created the container shop-test from the image "+fingerprint)
	})
}

// Incus 7.5.1 ran virtual machine instances on its test guest; the guest of
// 6.0.0 has no QEMU. Without --vm, Incus creates a virtual machine from the
// image of one: the task that asks for a container fails before, and for an
// image of another server right after, when the new instance is deleted.
func TestIncusInstanceChecksTheTypeOfAVirtualMachine(t *testing.T) {
	t.Parallel()
	s := newIncusScene(t, "7.5.1")
	alias := "/1.0/images/aliases/mmx-test-vm?project=default"
	path := "/1.0/instances/shop-vm?project=default"
	s.answers(path, "instance_missing")
	s.answers(alias, "image_alias_of_a_virtual_machine")
	r := s.run("incus_instance", map[string]any{"instance_type": "container", "profiles": []any{"default"}, "name": "shop-vm", "state": "running", "image_when_created": "mmx-test-vm"}, true)
	incusResultIs(t, r, "failed", "unchanged",
		"incus_instance.instance_type: the task asks for a container, and the image mmx-test-vm is the image of a virtual machine; Incus creates an instance of the type of its image")

	// With the type: --vm, and the instance is started.
	initialize := "init -q --project default local:mmx-test-vm local:shop-vm --vm"
	s.succeeds(initialize, path, "virtual_machine_running")
	s.succeeds("start --project default local:shop-vm")
	r = s.run("incus_instance", map[string]any{"profiles": []any{"default"}, "name": "shop-vm", "state": "running", "image_when_created": "mmx-test-vm", "instance_type": "virtual_machine"}, false)
	incusResultIs(t, r, "succeeded", "changed", "created the virtual machine shop-vm from the image mmx-test-vm; started it")

	// The instance exists as a virtual machine, and a task asks for a container.
	r = s.run("incus_instance", map[string]any{"instance_type": "container", "profiles": []any{"default"}, "name": "shop-vm", "state": "running"}, false)
	incusResultIs(t, r, "failed", "unchanged",
		"incus_instance.instance_type: the task asks for a container, and the instance shop-vm is of the type virtual machine; Incus cannot change the type of an instance")

	// An image of another server says its type only after incus init.
	s.answers(path, "instance_missing")
	remote := "init -q --project default images:debian/13 local:shop-vm"
	s.succeeds(remote, path, "virtual_machine_running")
	s.succeeds("delete --project default local:shop-vm", path, "instance_missing")
	r = s.run("incus_instance", map[string]any{"instance_type": "container", "profiles": []any{"default"}, "name": "shop-vm", "state": "running", "image_when_created": "images:debian/13"}, false)
	incusResultIs(t, r, "failed", "changed",
		"incus_instance.instance_type: the task asks for a container, and the image images:debian/13 created an instance of the type virtual machine; the task deleted the new instance again, which never ran")
	refusedForGood(t, r)
	calls := s.calls()
	if got := calls[len(calls)-4:]; !slices.Equal(got, []string{"incus " + query(path), "incus " + remote, "incus " + query(path), "incus delete --project default local:shop-vm"}) {
		t.Fatalf("%q", got)
	}

	// The new instance cannot be deleted again, here because a profile
	// protects it: the instance is there, so the task has one try and says
	// that it created the instance.
	s.answers(path, "instance_missing")
	s.fails("delete --project default local:shop-vm", "delete_protected")
	r = s.run("incus_instance", map[string]any{"instance_type": "container", "profiles": []any{"default"}, "name": "shop-vm", "state": "running", "image_when_created": "images:debian/13"}, false)
	incusResultIs(t, r, "failed", "changed",
		"incus_instance.instance_type: the task asks for a container, and the image images:debian/13 created an instance of the type virtual machine; "+
			`deleting the new instance again failed: Error: Failed deleting instance shop-test in project "default": Instance has delete protection enabled`)
	oneTry(t, r)
	createdIs(t, r, true)
}

// An existing instance whose configuration differs: the module reads it,
// reads it again right before the write, and writes the whole document back,
// with Incus's own keys, the description, and everything else as read.
func TestIncusInstanceWritesTheWholeDocument(t *testing.T) {
	t.Parallel()
	eachIncusVersion(t, func(t *testing.T, s *incusScene) {
		// The instance has the keys boot.autostart, environment.IMAGE_VERSION,
		// limits.memory, raw.lxc, and user.a, and the devices data and web.
		s.answers(incusInstancePath, "instance_running_with_more")
		edit := "config edit --project default local:shop-test"
		s.succeeds(edit)
		var before map[string]any
		if err := json.Unmarshal([]byte(s.measured("instance_running_with_more")), &before); err != nil {
			t.Fatal(err)
		}
		before = before["metadata"].(map[string]any)

		inputs := map[string]any{"instance_type": "container",
			"name": "shop-test", "state": "running", "profiles": []any{"web", "default"},
			"config":                 map[string]any{"limits.memory": "256MiB", "raw.lxc": "lxc.signal.halt=SIGTERM", "user.b": "2"},
			"config_keys_not_listed": "remove",
			"devices":                map[string]any{"web": map[string]any{"type": "proxy", "listen": "tcp:127.0.0.1:18081", "connect": "tcp:127.0.0.1:8080"}},
			"devices_not_listed":     "remove",
		}
		reason := "set the profiles to web, default; set the configuration keys limits.memory, user.b; " +
			"removed the configuration keys boot.autostart, environment.IMAGE_VERSION, user.a; set the device web; removed the device data"
		r := s.run("incus_instance", inputs, true)
		incusResultIs(t, r, "succeeded", "would_change", "would "+strings.ReplaceAll(strings.ReplaceAll(reason, "; removed", "; would remove"), "; set", "; would set"))
		s.ran(query(incusInstancePath), query(incusInstancePath))

		r = s.run("incus_instance", inputs, false)
		incusResultIs(t, r, "succeeded", "changed", reason)
		s.ran(query(incusInstancePath), query(incusInstancePath), query(incusInstancePath), query(incusInstancePath), edit)

		written := s.standardInput(edit)
		config := written["config"].(map[string]any)
		for key, value := range before["config"].(map[string]any) {
			own := !strings.HasPrefix(key, "volatile.") && !strings.HasPrefix(key, "image.")
			if _, kept := config[key]; own && kept && key != "limits.memory" && key != "raw.lxc" {
				t.Errorf("the own key %s was not removed", key)
			}
			if !own && config[key] != value {
				t.Errorf("Incus's own key %s is %v, was %v", key, config[key], value)
			}
		}
		if config["limits.memory"] != "256MiB" || config["user.b"] != "2" || config["raw.lxc"] != "lxc.signal.halt=SIGTERM" {
			t.Errorf("the configuration is %v", config)
		}
		if a, b := mustJSON(t, written["devices"]), mustJSON(t, inputs["devices"]); a != b {
			t.Errorf("the devices are %s, want %s", a, b)
		}
		if a, b := mustJSON(t, written["profiles"]), `["web","default"]`; a != b {
			t.Errorf("the profiles are %s", a)
		}
		for _, key := range []string{"architecture", "description", "ephemeral", "stateful"} {
			if _, there := written[key]; !there || mustJSON(t, written[key]) != mustJSON(t, before[key]) {
				t.Errorf("the document's %s is %v, was %v", key, written[key], before[key])
			}
		}

		// keep: only the listed key and device are set; the required
		// profiles list states the existing ordered list.
		keep := map[string]any{"instance_type": "container", "profiles": []any{"default"}, "name": "shop-test", "state": "running", "config": map[string]any{"user.b": "2"}, "config_keys_not_listed": "keep",
			"devices": map[string]any{"logs": map[string]any{"type": "disk", "source": "/var/log/shop", "path": "/mnt/logs"}}, "devices_not_listed": "keep"}
		r = s.run("incus_instance", keep, false)
		incusResultIs(t, r, "succeeded", "changed", "set the configuration key user.b; set the device logs")
		written = s.standardInput(edit)
		config = written["config"].(map[string]any)
		if len(config) != len(before["config"].(map[string]any))+1 || config["user.a"] != "1" {
			t.Errorf("keep changed other keys: %v", config)
		}
		if devices := written["devices"].(map[string]any); len(devices) != 3 || devices["data"] == nil || devices["web"] == nil {
			t.Errorf("keep changed other devices: %v", devices)
		}
		if a, b := mustJSON(t, written["profiles"]), mustJSON(t, before["profiles"]); a != b {
			t.Errorf("the profiles are %s, were %s", a, b)
		}
	})
}

// running starts a stopped instance and resumes a frozen one; stopped asks a
// running one to shut down with a bounded wait.
func TestIncusInstanceStartsResumesAndShutsDown(t *testing.T) {
	t.Parallel()
	eachIncusVersion(t, func(t *testing.T, s *incusScene) {
		start := "start --project default local:shop-test"
		s.answers(incusInstancePath, "instance_stopped")
		s.succeeds(start, incusInstancePath, "instance_running_with_more")
		r := s.run("incus_instance", map[string]any{"instance_type": "container", "profiles": []any{"default"}, "name": "shop-test", "state": "running"}, true)
		incusResultIs(t, r, "succeeded", "would_change", "would start the instance shop-test")
		r = s.run("incus_instance", map[string]any{"instance_type": "container", "profiles": []any{"default"}, "name": "shop-test", "state": "running"}, false)
		incusResultIs(t, r, "succeeded", "changed", "started the instance shop-test")
		s.ran(query(incusInstancePath), query(incusInstancePath), start)

		s.answers(incusInstancePath, "instance_frozen")
		r = s.run("incus_instance", map[string]any{"instance_type": "container", "profiles": []any{"default"}, "name": "shop-test", "state": "running"}, false)
		incusResultIs(t, r, "succeeded", "changed", "resumed the frozen instance shop-test")

		// The wait is 120 seconds unless the task gives another.
		stop := "stop --project default local:shop-test --timeout 120"
		s.succeeds(stop, incusInstancePath, "instance_stopped")
		r = s.run("incus_instance", map[string]any{"instance_type": "container", "profiles": []any{"default"}, "name": "shop-test", "state": "stopped"}, true)
		incusResultIs(t, r, "succeeded", "would_change", "would shut down the instance shop-test")
		r = s.run("incus_instance", map[string]any{"instance_type": "container", "profiles": []any{"default"}, "name": "shop-test", "state": "stopped"}, false)
		incusResultIs(t, r, "succeeded", "changed", "shut down the instance shop-test")
		calls := s.calls()
		if got := calls[len(calls)-3:]; !slices.Equal(got, []string{"incus " + query(incusInstancePath), "incus " + query(incusInstancePath), "incus " + stop}) {
			t.Fatalf("%q", got)
		}
		r = s.run("incus_instance", map[string]any{"instance_type": "container", "profiles": []any{"default"}, "name": "shop-test", "state": "stopped"}, false)
		incusResultIs(t, r, "succeeded", "unchanged", "")
	})
}

// An instance that does not shut down within the wait fails the task, which
// names the input that stops it at once; with that input the task stops it.
// The module reads the instance's status and not incus's message.
func TestIncusInstanceWhenTheShutdownTakesLonger(t *testing.T) {
	t.Parallel()
	eachIncusVersion(t, func(t *testing.T, s *incusScene) {
		s.answers(incusInstancePath, "instance_running")
		stop := "stop --project default local:shop-test --timeout 5"
		s.fails(stop, "stop_wait_ended")
		inputs := map[string]any{"instance_type": "container", "profiles": []any{"default"}, "name": "shop-test", "state": "stopped", "wait_for_shutdown_at_most": "5s"}
		r := s.run("incus_instance", inputs, false)
		hint := "`incus info --show-log local:shop-test`"
		if s.version == "7.5.1" {
			hint = "`incus info --show-log shop-test`"
		}
		incusResultIs(t, r, "failed", "changed", "incus_instance: shutting down the instance shop-test, with a wait of 5 seconds: "+
			`Error: Failed shutting down instance, status is "Running": context deadline exceeded Try `+hint+" for more info; "+
			"the status of the instance is now Running. To stop the instance at once when the wait ends, write power_off_when_shutdown_takes_longer: true")
		// The wait is the task's own limit, so the task has one try. MMX's
		// next try would ask and wait again, and report the same.
		oneTry(t, r)
		s.ran(query(incusInstancePath), stop, query(incusInstancePath))
		again := s.run("incus_instance", inputs, false)
		if again.Reason != r.Reason {
			t.Fatalf("the next try reports %q", again.Reason)
		}
		s.ran(query(incusInstancePath), stop, query(incusInstancePath), query(incusInstancePath), stop, query(incusInstancePath))

		force := "stop --force --project default local:shop-test"
		s.succeeds(force, incusInstancePath, "instance_stopped")
		inputs["power_off_when_shutdown_takes_longer"] = true
		r = s.run("incus_instance", inputs, false)
		incusResultIs(t, r, "succeeded", "changed", "stopped the instance shop-test at once, because it did not shut down within 5 seconds")
		calls := s.calls()
		if got := calls[len(calls)-4:]; !slices.Equal(got, []string{"incus " + query(incusInstancePath), "incus " + stop, "incus " + query(incusInstancePath), "incus " + force}) {
			t.Fatalf("%q", got)
		}

		// A stop that fails while the instance is stopped all the same, as
		// when another program stopped it first, is no failure: neither the
		// request to shut down, nor the stop at once after the wait.
		s.answers(incusInstancePath, "instance_running")
		s.fails(stop, "stop_already_stopped", incusInstancePath, "instance_stopped")
		inputs["power_off_when_shutdown_takes_longer"] = false
		r = s.run("incus_instance", inputs, false)
		incusResultIs(t, r, "succeeded", "changed", "shut down the instance shop-test")
		s.answers(incusInstancePath, "instance_running")
		s.fails(stop, "stop_wait_ended")
		s.fails(force, "stop_already_stopped", incusInstancePath, "instance_stopped")
		inputs["power_off_when_shutdown_takes_longer"] = true
		r = s.run("incus_instance", inputs, false)
		incusResultIs(t, r, "succeeded", "changed", "stopped the instance shop-test at once, because it did not shut down within 5 seconds")

		// The stop at once fails, here because the server went away: one
		// try, because the next one would wait again first.
		s.answers(incusInstancePath, "instance_running")
		s.theServerStopsAnsweringAt(force)
		r = s.run("incus_instance", inputs, false)
		incusResultIs(t, r, "failed", "changed",
			"incus_instance: stopping the instance shop-test at once: "+incusDoesNotAnswer)
		oneTry(t, r)
	})
}

// stopped never stops a frozen instance, and never an ephemeral one, which
// Incus deletes when it stops. A status that the module does not know fails
// the task and is quoted.
func TestIncusInstanceRefusesStatesItMustNotChange(t *testing.T) {
	t.Parallel()
	eachIncusVersion(t, func(t *testing.T, s *incusScene) {
		stopped := map[string]any{"instance_type": "container", "profiles": []any{"default"}, "name": "shop-test", "state": "stopped"}
		s.answers(incusInstancePath, "instance_frozen")
		for _, dryrun := range []bool{true, false} {
			r := s.run("incus_instance", stopped, dryrun)
			incusResultIs(t, r, "failed", "unchanged",
				"incus_instance.state: the instance shop-test is frozen, and Incus does not shut down a frozen instance; resume it first, with incus resume shop-test")
			if r.FailureKind != "config" {
				t.Fatal(r.FailureKind)
			}
		}

		s.answers(incusInstancePath, "instance_ephemeral_running")
		r := s.run("incus_instance", stopped, false)
		incusResultIs(t, r, "failed", "unchanged",
			"incus_instance.state: the instance shop-test is ephemeral, and Incus deletes an ephemeral instance when it stops; to delete it, write state: deleted_with_all_its_data")
		// The same instance stays as it is for state running.
		incusResultIs(t, s.run("incus_instance", map[string]any{"instance_type": "container", "profiles": []any{"default"}, "name": "shop-test", "state": "running"}, false), "succeeded", "unchanged", "")

		// No test guest showed a status other than Stopped, Running, and
		// Frozen: this answer is the measured one with another status.
		other := strings.NewReplacer(`"status": "Running"`, `"status": "Error"`, `"status_code": 103`, `"status_code": 112`).Replace(s.measured("instance_running"))
		s.answersText(incusInstancePath, other)
		r = s.run("incus_instance", stopped, false)
		incusResultIs(t, r, "failed", "unchanged",
			"incus_instance: the instance shop-test has the status Error (code 112), and this module acts only on the statuses Stopped, Running, and Frozen")
		if got := s.calls(); len(got) != 5 {
			t.Fatalf("a changing command ran: %q", got)
		}
	})
}

// A change that Incus refuses fails the task with Incus's message and the
// status that the instance has afterwards; a failure after an earlier change
// still reports the change.
func TestIncusInstanceReportsWhatIncusRefuses(t *testing.T) {
	t.Parallel()
	eachIncusVersion(t, func(t *testing.T, s *incusScene) {
		// Shut down first, then a configuration that Incus refuses. Another
		// try would send the same document and get the same answer.
		s.answers(incusInstancePath, "instance_running")
		stop := "stop --project default local:shop-test --timeout 120"
		edit := "config edit --project default local:shop-test"
		s.succeeds(stop, incusInstancePath, "instance_stopped")
		s.fails(edit, "config_edit_unknown_key")
		r := s.run("incus_instance", map[string]any{"instance_type": "container", "profiles": []any{"default"}, "name": "shop-test", "state": "stopped",
			"config": map[string]any{"limits.nonsense": "sentinel-value"}, "config_keys_not_listed": "keep"}, false)
		incusResultIs(t, r, "failed", "changed", "incus_instance: writing the configuration of the instance shop-test: "+
			"Error: Invalid config: Unknown configuration key: limits.nonsense; the status of the instance is now Stopped")
		refusedForGood(t, r)
		s.ran(query(incusInstancePath), stop, query(incusInstancePath), edit, query(incusInstancePath))

		// incus init that fails creates no instance, and MMX tries again:
		// the module does not know why Incus refused, and an image server
		// that does not answer is one of the reasons.
		s.answers(incusInstancePath, "instance_missing")
		initialize := "init -q --project default images:debian/13 local:shop-test"
		s.fails(initialize, "init_unknown_key")
		r = s.run("incus_instance", map[string]any{"instance_type": "container", "profiles": []any{"default"}, "name": "shop-test", "state": "running", "image_when_created": "images:debian/13",
			"config": map[string]any{"limits.nonsense": "sentinel-value"}, "config_keys_not_listed": "keep"}, false)
		incusResultIs(t, r, "failed", "changed", "incus_instance: creating the instance shop-test from the image images:debian/13: "+
			"Error: Failed creating instance record: Unknown configuration key: limits.nonsense")
		triedAgain(t, r)
	})
}

// A new device that cannot start, here a proxy device whose port is in use:
// Incus refuses the whole document and stores none of it, and the instance
// runs on. Another try would send the same document.
func TestIncusInstanceWithANewDeviceThatCannotStart(t *testing.T) {
	t.Parallel()
	eachIncusVersion(t, func(t *testing.T, s *incusScene) {
		s.answers(incusInstancePath, "instance_running_without_devices")
		edit := "config edit --project default local:shop-test"
		s.fails(edit, "config_edit_device_cannot_start")
		inputs := map[string]any{"instance_type": "container", "profiles": []any{"default"}, "name": "shop-test", "state": "running", "devices_not_listed": "remove",
			"devices": map[string]any{"web": map[string]any{"type": "proxy", "listen": "tcp:127.0.0.1:18080", "connect": "tcp:127.0.0.1:8080"}}}
		r := s.run("incus_instance", inputs, false)
		incusResultIs(t, r, "failed", "changed", "incus_instance: writing the configuration of the instance shop-test: "+
			`Error: Failed to start device "web": Error occurred when starting proxy device: Error: Failed to receive fd from listener process: `+
			"Failed to receive file descriptor via abstract unix socket; the status of the instance is now Running")
		refusedForGood(t, r)
		s.ran(query(incusInstancePath), query(incusInstancePath), edit, query(incusInstancePath))

		// The server goes away during the write: the module cannot read what
		// the instance has now, and MMX tries again.
		s.theServerStopsAnsweringAt(edit)
		r = s.run("incus_instance", inputs, false)
		incusResultIs(t, r, "failed", "changed", "incus_instance: writing the configuration of the instance shop-test: "+incusDoesNotAnswer)
		triedAgain(t, r)
	})
}

// startFailed is what incus start printed for an instance whose proxy device
// wants a port that is in use, on one line.
func startFailed(s *incusScene) string {
	hint := "`incus info --show-log local:shop-test`"
	if s.version == "7.5.1" {
		hint = "`incus info --show-log shop-test`"
	}
	return "Error: Error occurred when starting proxy device: Error: Failed to listen on 127.0.0.1:18080: listen tcp 127.0.0.1:18080: bind: address already in use Try " + hint + " for more info"
}

// A new instance that does not start, here because its proxy device wants a
// port that is in use. From incus init on the instance exists, so every
// failure after it has one try and says that the task created the instance.
// MMX's next try would find the instance and report created: false for an
// instance that this task created.
func TestIncusInstanceCreatedAndNotStartedHasOneTry(t *testing.T) {
	t.Parallel()
	eachIncusVersion(t, func(t *testing.T, s *incusScene) {
		alias := "/1.0/images/aliases/mmx-test?project=default"
		initialize := "init -q --project default local:mmx-test local:shop-test"
		start := "start --project default local:shop-test"
		web := map[string]any{"web": map[string]any{"type": "proxy", "listen": "tcp:127.0.0.1:18080", "connect": "tcp:127.0.0.1:8080"}}
		s.answers(alias, "image_alias_of_a_container")

		// What the test guests of Incus 6.0.0, 6.0.4, 6.23, and 7.5.1 showed:
		// the instance is stopped after the failed start, with its device.
		s.answers(incusInstancePath, "instance_missing")
		s.succeeds(initialize, incusInstancePath, "instance_new")
		s.fails(start, "start_port_in_use", incusInstancePath, "instance_stopped_after_a_failed_start")
		inputs := map[string]any{"instance_type": "container", "profiles": []any{"default"}, "name": "shop-test", "state": "running", "image_when_created": "mmx-test",
			"config": map[string]any{"raw.lxc": "lxc.signal.halt=SIGTERM"}, "config_keys_not_listed": "remove", "devices": web, "devices_not_listed": "remove"}
		r := s.run("incus_instance", inputs, false)
		incusResultIs(t, r, "failed", "changed", "incus_instance: created the instance shop-test, and starting it failed: "+startFailed(s)+"; the status of the instance is now Stopped")
		oneTry(t, r)
		createdIs(t, r, true)
		s.ran(query(incusInstancePath), query(alias), initialize, query(incusInstancePath), start, query(incusInstancePath))
		// MMX's next try, when the port is free by then: it starts the
		// instance and says that the task created nothing.
		s.succeeds(start)
		next := s.run("incus_instance", inputs, false)
		incusResultIs(t, next, "succeeded", "changed", "started the instance shop-test")
		createdIs(t, next, false)

		// The module decides by the status that it reads, so it also covers
		// an instance that runs after the failed start, without the device
		// that failed. No measuring guest showed that state: this answer is
		// the measured one of a running instance. MMX's next try would find
		// the instance running and as the task writes it, and report success.
		s.answers(incusInstancePath, "instance_missing")
		s.fails(start, "start_port_in_use", incusInstancePath, "instance_running")
		inputs["config"] = map[string]any{"limits.memory": "128MiB", "raw.lxc": "lxc.signal.halt=SIGTERM", "environment.IMAGE_VERSION": "7"}
		r = s.run("incus_instance", inputs, false)
		incusResultIs(t, r, "failed", "changed", "incus_instance: created the instance shop-test, and starting it failed: "+startFailed(s)+"; the status of the instance is now Running")
		oneTry(t, r)
		createdIs(t, r, true)
		next = s.run("incus_instance", inputs, false)
		incusResultIs(t, next, "succeeded", "unchanged", "")
		createdIs(t, next, false)

		// The server goes away right after incus init, before the module
		// has read the new instance.
		s.answers(incusInstancePath, "instance_missing")
		s.theServerStopsAnsweringAfter(initialize)
		r = s.run("incus_instance", inputs, false)
		incusResultIs(t, r, "failed", "changed", "incus_instance: reading the new instance shop-test from the Incus server on this node: "+incusDoesNotAnswer)
		oneTry(t, r)
		createdIs(t, r, true)
		// MMX's next try, when the server answers again: the instance is
		// there, and the try starts it.
		s.theServerAnswersAgain()
		s.answers(incusInstancePath, "instance_new")
		s.succeeds(start)
		next = s.run("incus_instance", inputs, false)
		incusResultIs(t, next, "succeeded", "changed", "started the instance shop-test")
		createdIs(t, next, false)
	})
}

// A start of an instance that existed before fails. While the instance is
// stopped afterwards, MMX tries again, and each try fails in the same way.
// When the instance runs afterwards, the task has one try: the next one would
// find the instance running and report success.
func TestIncusInstanceWhenTheStartOfAnExistingInstanceFails(t *testing.T) {
	t.Parallel()
	eachIncusVersion(t, func(t *testing.T, s *incusScene) {
		start := "start --project default local:shop-test"
		inputs := map[string]any{"instance_type": "container", "profiles": []any{"default"}, "name": "shop-test", "state": "running"}
		s.answers(incusInstancePath, "instance_stopped_after_a_failed_start")
		s.fails(start, "start_port_in_use")
		r := s.run("incus_instance", inputs, false)
		incusResultIs(t, r, "failed", "changed", "incus_instance: starting the instance shop-test: "+startFailed(s)+"; the status of the instance is now Stopped")
		triedAgain(t, r)
		s.ran(query(incusInstancePath), start, query(incusInstancePath))

		s.fails(start, "start_port_in_use", incusInstancePath, "instance_running")
		r = s.run("incus_instance", inputs, false)
		incusResultIs(t, r, "failed", "changed", "incus_instance: starting the instance shop-test: "+startFailed(s)+"; the status of the instance is now Running")
		oneTry(t, r)
		incusResultIs(t, s.run("incus_instance", inputs, false), "succeeded", "unchanged", "")
	})
}

// deleted_with_all_its_data deletes the instance, running or not, and is
// unchanged without one. An instance that Incus protects fails the task with
// Incus's message, which differs between the versions.
func TestIncusInstanceDeletesWithAllItsData(t *testing.T) {
	t.Parallel()
	eachIncusVersion(t, func(t *testing.T, s *incusScene) {
		inputs := map[string]any{"name": "shop-test", "state": "deleted_with_all_its_data"}
		remove := "delete --force --project default local:shop-test"
		s.answers(incusInstancePath, "instance_missing")
		for _, dryrun := range []bool{true, false} {
			incusResultIs(t, s.run("incus_instance", inputs, dryrun), "succeeded", "unchanged", "")
		}
		s.ran(query(incusInstancePath), query(incusInstancePath))

		s.answers(incusInstancePath, "instance_running")
		s.succeeds(remove, incusInstancePath, "instance_missing")
		r := s.run("incus_instance", inputs, true)
		incusResultIs(t, r, "succeeded", "would_change", "would delete the instance shop-test with all its data")
		r = s.run("incus_instance", inputs, false)
		incusResultIs(t, r, "succeeded", "changed", "deleted the instance shop-test with all its data")
		if r.ModuleOutputs["created"] != false {
			t.Fatalf("%+v", r.ModuleOutputs)
		}

		s.answers(incusInstancePath, "instance_stopped")
		r = s.run("incus_instance", inputs, false)
		incusResultIs(t, r, "succeeded", "changed", "deleted the instance shop-test with all its data")

		s.answers(incusInstancePath, "instance_with_protection_from_a_profile")
		s.fails(remove, "delete_protected")
		message := `Error: Failed deleting instance "shop-test" in project "default": Instance is protected`
		if s.version == "7.5.1" {
			message = `Error: Failed deleting instance shop-test in project "default": Instance has delete protection enabled`
		}
		r = s.run("incus_instance", inputs, false)
		incusResultIs(t, r, "failed", "changed", "incus_instance: deleting the instance shop-test: "+message+"; the status of the instance is now Stopped")
		triedAgain(t, r)
	})
}

// An answer of the server that is neither the thing nor "not found" fails
// the task with the server's words, in a dryrun too.
func TestIncusInstanceReportsARefusedRead(t *testing.T) {
	t.Parallel()
	eachIncusVersion(t, func(t *testing.T, s *incusScene) {
		s.answers("/1.0/instances/a%2Fb?project=default", "instance_name_refused")
		for _, dryrun := range []bool{true, false} {
			r := s.run("incus_instance", map[string]any{"instance_type": "container", "profiles": []any{"default"}, "name": "a/b", "state": "running", "image_when_created": "images:debian/13"}, dryrun)
			if r.Status != "failed" || r.Change != "unchanged" || !strings.HasPrefix(r.Reason, "incus_instance: reading the instance a/b from the Incus server on this node: ") ||
				!strings.HasSuffix(r.Reason, " (error code 400)") {
				t.Fatalf("%s %s %q", r.Status, r.Change, r.Reason)
			}
		}
	})
}

func TestIncusProfileRefusesInputsThatBreakARule(t *testing.T) {
	t.Parallel()
	needsTheIncusModules(t)
	calls := stubTools(t, map[string]string{"incus": `echo "incus must not run" >&2; exit 64`})
	for reason, inputs := range map[string]map[string]any{
		"name: must not be empty":          {"name": ""},
		"incus_project: must not be empty": {"name": "web", "incus_project": ""},
		"incus_project: must not start with a hyphen, which the program incus would read as one of its options":                               {"name": "web", "incus_project": "-shop"},
		"state: absent accepts only name and incus_project; remove description":                                                               {"name": "web", "state": "absent", "description": "Limits"},
		"config_keys_not_listed: is required with config: remove removes every key that config does not list, and keep leaves those keys":     {"name": "web", "config": map[string]any{"limits.cpu": "2"}},
		"config: is required with config_keys_not_listed":                                                                                     {"name": "web", "config_keys_not_listed": "remove"},
		"devices_not_listed: is required with devices: remove removes every device that devices does not list, and keep leaves those devices": {"name": "web", "devices": map[string]any{"eth0": map[string]any{"type": "nic"}}},
		"devices: is required with devices_not_listed":                                                                                        {"name": "web", "devices_not_listed": "keep"},
		"devices: the device eth0 must be a mapping of Incus device options, such as type: disk":                                              {"name": "web", "devices": map[string]any{"eth0": "nic"}, "devices_not_listed": "keep"},
		"config: the key limits.cpu has an empty value, and Incus stores no key without a value; leave the key out, and write config_keys_not_listed: remove to remove a key that the task does not list": {
			"name": "web", "config": map[string]any{"limits.cpu": ""}, "config_keys_not_listed": "remove"},
	} {
		for _, dryrun := range []bool{true, false} {
			r := runOfficialExtraModule(t, "incus_profile", inputs, dryrun)
			if r.Status != "failed" || r.FailureKind != "config" || r.Change != "unchanged" || r.Reason != "incus_profile."+reason {
				t.Errorf("dryrun %v: %s %s %s %q\nwant the reason %q", dryrun, r.Status, r.FailureKind, r.Change, r.Reason, "incus_profile."+reason)
			}
		}
	}
	if got := calls(); got != nil {
		t.Fatalf("incus ran: %q", got)
	}
}

// A missing profile is created empty and then written, in two commands: Incus
// 6.0.0 ignores a document that incus profile create gets. The profile of
// Incus 6.0.0 has no key project, and the document is written all the same.
func TestIncusProfileCreatesAProfile(t *testing.T) {
	t.Parallel()
	eachIncusVersion(t, func(t *testing.T, s *incusScene) {
		s.answers(incusProfilePath, "profile_missing")
		create := "profile create -q --project default local:web"
		edit := "profile edit --project default local:web"
		s.succeeds(create, incusProfilePath, "profile_new")
		s.succeeds(edit, incusProfilePath, "profile_with_a_key")
		inputs := map[string]any{"name": "web", "description": "Limits for web",
			"config": map[string]any{"security.protection.delete": "true"}, "config_keys_not_listed": "remove",
			"devices": map[string]any{"eth0": map[string]any{"type": "nic", "network": "incusbr0"}}, "devices_not_listed": "remove"}

		r := s.run("incus_profile", inputs, true)
		incusResultIs(t, r, "succeeded", "would_change",
			"would create the profile web; would set the description; would set the configuration key security.protection.delete; would set the device eth0")
		createdIs(t, r, true)
		s.ran(query(incusProfilePath))

		r = s.run("incus_profile", inputs, false)
		incusResultIs(t, r, "succeeded", "changed", "created the profile web; set the description; set the configuration key security.protection.delete; set the device eth0")
		createdIs(t, r, true)
		if r.Fingerprint == "" {
			t.Fatalf("%+v", r)
		}
		s.ran(query(incusProfilePath), query(incusProfilePath), create, query(incusProfilePath), edit)
		if got := s.standardInput(edit); got["description"] != "Limits for web" || mustJSON(t, got["config"]) != `{"security.protection.delete":"true"}` ||
			mustJSON(t, got["devices"]) != `{"eth0":{"network":"incusbr0","type":"nic"}}` || got["name"] != "web" {
			t.Fatalf("incus profile edit got %v", got)
		}
		if b, err := os.ReadFile(filepath.Join(s.directory, "stdin-"+incusKey(create))); err != nil || len(b) != 0 {
			t.Fatalf("incus profile create got a standard input: %q %v", b, err)
		}
	})
}

// A profile without a description, a configuration, or devices is only
// created. A new profile that Incus refuses is removed again, and another try
// would create it, be refused, and remove it in the same way.
func TestIncusProfileLeavesNothingAfterARefusedNewProfile(t *testing.T) {
	t.Parallel()
	eachIncusVersion(t, func(t *testing.T, s *incusScene) {
		s.answers(incusProfilePath, "profile_missing")
		create := "profile create -q --project default local:web"
		edit := "profile edit --project default local:web"
		remove := "profile delete --project default local:web"
		s.succeeds(create, incusProfilePath, "profile_new")
		r := s.run("incus_profile", map[string]any{"name": "web"}, false)
		incusResultIs(t, r, "succeeded", "changed", "created the profile web")
		createdIs(t, r, true)
		s.ran(query(incusProfilePath), create, query(incusProfilePath))

		s.answers(incusProfilePath, "profile_missing")
		s.fails(edit, "profile_edit_unknown_key")
		s.succeeds(remove, incusProfilePath, "profile_missing")
		refused := map[string]any{"name": "web", "config": map[string]any{"limits.nonsense": "sentinel-value"}, "config_keys_not_listed": "remove"}
		r = s.run("incus_profile", refused, false)
		if r.Status != "failed" || r.Change != "changed" || !strings.HasPrefix(r.Reason, "incus_profile: writing the new profile web: Error: ") ||
			!strings.Contains(r.Reason, "limits.nonsense") || !strings.HasSuffix(r.Reason, "; the task removed the new profile again") {
			t.Fatalf("%s %s %q", r.Status, r.Change, r.Reason)
		}
		refusedForGood(t, r)
		if r.ModuleOutputs != nil {
			t.Fatalf("a profile that the task removed again is not created: %v", r.ModuleOutputs)
		}
		calls := s.calls()
		if got := calls[len(calls)-5:]; !slices.Equal(got, []string{"incus " + query(incusProfilePath), "incus " + create, "incus " + query(incusProfilePath), "incus " + edit, "incus " + remove}) {
			t.Fatalf("%q", got)
		}

		// The profile cannot be removed again: the reason says both, and
		// the profile is there.
		s.answers(incusProfilePath, "profile_missing")
		s.fails(remove, "profile_delete_in_use")
		r = s.run("incus_profile", refused, false)
		if !strings.HasSuffix(r.Reason, "; removing the new profile again failed: Error: Profile is currently in use") {
			t.Fatalf("%q", r.Reason)
		}
		oneTry(t, r)
		createdIs(t, r, true)

		// The server goes away right after incus profile create: the
		// profile exists, so the task has one try and says that it created
		// the profile. MMX's next try, when the server answers again, would
		// write the profile and say that the task created nothing.
		s.answers(incusProfilePath, "profile_missing")
		s.theServerStopsAnsweringAfter(create)
		wanted := map[string]any{"name": "web", "description": "Limits for web"}
		r = s.run("incus_profile", wanted, false)
		incusResultIs(t, r, "failed", "changed", "incus_profile: reading the new profile web from the Incus server on this node: "+incusDoesNotAnswer)
		oneTry(t, r)
		createdIs(t, r, true)
		s.theServerAnswersAgain()
		s.answers(incusProfilePath, "profile_new")
		s.succeeds(edit)
		next := s.run("incus_profile", wanted, false)
		incusResultIs(t, next, "succeeded", "changed", "set the description")
		createdIs(t, next, false)
	})
}

// An existing profile: unchanged with one read; a difference is written as
// the whole document, read again right before the write.
func TestIncusProfileWritesAnExistingProfile(t *testing.T) {
	t.Parallel()
	eachIncusVersion(t, func(t *testing.T, s *incusScene) {
		// The profile has the description "Limits for web" and the key
		// security.protection.delete.
		s.answers(incusProfilePath, "profile_with_a_key")
		edit := "profile edit --project default local:web"
		s.succeeds(edit)
		same := map[string]any{"name": "web", "description": "Limits for web", "config": map[string]any{"security.protection.delete": "true"}, "config_keys_not_listed": "remove"}
		for _, dryrun := range []bool{true, false} {
			r := s.run("incus_profile", same, dryrun)
			incusResultIs(t, r, "succeeded", "unchanged", "")
			createdIs(t, r, false)
		}
		// A task that names only the profile leaves all of it alone.
		incusResultIs(t, s.run("incus_profile", map[string]any{"name": "web"}, false), "succeeded", "unchanged", "")
		s.ran(query(incusProfilePath), query(incusProfilePath), query(incusProfilePath))

		keep := map[string]any{"name": "web", "config": map[string]any{"limits.cpu": "2"}, "config_keys_not_listed": "keep"}
		incusResultIs(t, s.run("incus_profile", keep, true), "succeeded", "would_change", "would set the configuration key limits.cpu")
		incusResultIs(t, s.run("incus_profile", keep, false), "succeeded", "changed", "set the configuration key limits.cpu")
		got := s.standardInput(edit)
		if got["description"] != "Limits for web" || mustJSON(t, got["config"]) != `{"limits.cpu":"2","security.protection.delete":"true"}` || mustJSON(t, got["devices"]) != `{}` {
			t.Fatalf("incus profile edit got %v", got)
		}
		calls := s.calls()
		if got := calls[len(calls)-3:]; !slices.Equal(got, []string{"incus " + query(incusProfilePath), "incus " + query(incusProfilePath), "incus " + edit}) {
			t.Fatalf("%q", got)
		}

		remove := map[string]any{"name": "web", "description": "", "config": map[string]any{"limits.cpu": "2"}, "config_keys_not_listed": "remove",
			"devices": map[string]any{}, "devices_not_listed": "remove"}
		incusResultIs(t, s.run("incus_profile", remove, false), "succeeded", "changed",
			"set the description; set the configuration key limits.cpu; removed the configuration key security.protection.delete")
		if got := s.standardInput(edit); got["description"] != "" || mustJSON(t, got["config"]) != `{"limits.cpu":"2"}` {
			t.Fatalf("incus profile edit got %v", got)
		}

		// A write that Incus refuses: the module reads the profile again,
		// finds it as before, and another try would get the same answer.
		s.fails(edit, "profile_edit_unknown_key")
		r := s.run("incus_profile", keep, false)
		incusResultIs(t, r, "failed", "changed", "incus_profile: writing the profile web: Error: Unknown configuration key: limits.nonsense")
		refusedForGood(t, r)
		calls = s.calls()
		if got := calls[len(calls)-4:]; !slices.Equal(got, []string{"incus " + query(incusProfilePath), "incus " + query(incusProfilePath), "incus " + edit, "incus " + query(incusProfilePath)}) {
			t.Fatalf("%q", got)
		}

		// The server goes away during the write: the module cannot read
		// what the profile holds now, and MMX tries again.
		s.theServerStopsAnsweringAt(edit)
		r = s.run("incus_profile", keep, false)
		incusResultIs(t, r, "failed", "changed", "incus_profile: writing the profile web: "+incusDoesNotAnswer)
		triedAgain(t, r)
	})
}

// incus profile edit can save a profile and still exit 1: an instance that
// uses the profile could not take the change, here a proxy device whose port
// is in use. MMX's next try would read the saved profile, find no difference,
// and report success, and Incus's message would be lost. So the module reads
// the profile after the failed write, and the task has one try.
func TestIncusProfileThatIncusSavedAndReportedAsFailed(t *testing.T) {
	t.Parallel()
	eachIncusVersion(t, func(t *testing.T, s *incusScene) {
		// The profile has the description "Limits for web" and the key
		// limits.cpu, and the running instance shop-test uses it.
		s.answers(incusProfilePath, "profile_in_use")
		edit := "profile edit --project default local:web"
		s.fails(edit, "profile_edit_still_saved", incusProfilePath, "profile_in_use_with_a_device")
		inputs := map[string]any{"name": "web", "description": "Limits for web", "config": map[string]any{"limits.cpu": "1"}, "config_keys_not_listed": "remove",
			"devices": map[string]any{"web": map[string]any{"type": "proxy", "listen": "tcp:127.0.0.1:18080", "connect": "tcp:127.0.0.1:8080"}}, "devices_not_listed": "remove"}
		r := s.run("incus_profile", inputs, false)
		incusResultIs(t, r, "failed", "changed", "incus_profile: writing the profile web: "+
			"Error: The following instances failed to update (profile change still saved): "+
			`- Project: default, Instance: shop-test: Failed to start device "web": Error occurred when starting proxy device: `+
			"Error: Failed to listen on 127.0.0.1:18080: listen tcp 127.0.0.1:18080: bind: address already in use. "+
			"Incus saved the profile all the same, so the profile is as the task writes it, and the next run finds nothing to write; "+
			"an instance that uses the profile did not take the change")
		oneTry(t, r)
		s.ran(query(incusProfilePath), query(incusProfilePath), edit, query(incusProfilePath))

		// MMX's next try: one read, no difference, and success.
		next := s.run("incus_profile", inputs, false)
		incusResultIs(t, next, "succeeded", "unchanged", "")
		s.ran(query(incusProfilePath), query(incusProfilePath), edit, query(incusProfilePath), query(incusProfilePath))
	})
}

// absent removes the profile; Incus refuses while an instance uses it, and
// the task fails with Incus's message.
func TestIncusProfileRemovesAProfile(t *testing.T) {
	t.Parallel()
	eachIncusVersion(t, func(t *testing.T, s *incusScene) {
		inputs := map[string]any{"name": "web", "state": "absent"}
		remove := "profile delete --project default local:web"
		s.answers(incusProfilePath, "profile_missing")
		for _, dryrun := range []bool{true, false} {
			incusResultIs(t, s.run("incus_profile", inputs, dryrun), "succeeded", "unchanged", "")
		}
		s.answers(incusProfilePath, "profile_with_a_key")
		s.succeeds(remove, incusProfilePath, "profile_missing")
		incusResultIs(t, s.run("incus_profile", inputs, true), "succeeded", "would_change", "would remove the profile web")
		r := s.run("incus_profile", inputs, false)
		incusResultIs(t, r, "succeeded", "changed", "removed the profile web")
		createdIs(t, r, false)
		s.ran(query(incusProfilePath), query(incusProfilePath), query(incusProfilePath), query(incusProfilePath), remove)

		// Incus refuses, and nothing changed: MMX tries again, as for
		// every command that failed.
		s.answers(incusProfilePath, "profile_with_a_key")
		s.fails(remove, "profile_delete_in_use")
		r = s.run("incus_profile", inputs, false)
		incusResultIs(t, r, "failed", "changed", "incus_profile: removing the profile web: Error: Profile is currently in use")
		triedAgain(t, r)

		// Incus never deletes its profile default.
		s.answers("/1.0/profiles/default?project=default", "profile_default")
		s.fails("profile delete --project default local:default", "profile_delete_default")
		incusResultIs(t, s.run("incus_profile", map[string]any{"name": "default", "state": "absent"}, false), "failed", "changed",
			`incus_profile: removing the profile default: Error: The "default" profile cannot be deleted`)
	})
}

// An Incus project that uses the profiles of the project default has none of
// its own: the task fails and says to write it without incus_project. A
// project with its own profiles is in every command.
func TestIncusProfileInAProject(t *testing.T) {
	t.Parallel()
	eachIncusVersion(t, func(t *testing.T, s *incusScene) {
		s.answers("/1.0/projects/flat", "project_flat")
		for _, dryrun := range []bool{true, false} {
			r := s.run("incus_profile", map[string]any{"name": "web", "incus_project": "flat"}, dryrun)
			incusResultIs(t, r, "failed", "unchanged", "incus_profile.incus_project: the Incus project flat uses the profiles of the project default "+
				"(its features.profiles is not true), so it has no profile of its own; write this task without incus_project")
			if r.FailureKind != "config" {
				t.Fatal(r.FailureKind)
			}
		}
		s.ran(query("/1.0/projects/flat"), query("/1.0/projects/flat"))

		// The project default has profiles of its own, also when the task
		// names it.
		s.answers("/1.0/projects/default", "project_default")
		s.answers("/1.0/profiles/default?project=default", "profile_default")
		incusResultIs(t, s.run("incus_profile", map[string]any{"name": "default", "incus_project": "default"}, false), "succeeded", "unchanged", "")

		path := "/1.0/profiles/web?project=shop"
		s.answers("/1.0/projects/shop", "project_shop")
		s.answers(path, "profile_missing")
		create := "profile create -q --project shop local:web"
		s.succeeds(create, path, "profile_new")
		incusResultIs(t, s.run("incus_profile", map[string]any{"name": "web", "incus_project": "shop"}, false), "succeeded", "changed", "created the profile web")
		calls := s.calls()
		if got := calls[len(calls)-4:]; !slices.Equal(got, []string{"incus " + query("/1.0/projects/shop"), "incus " + query(path), "incus " + create, "incus " + query(path)}) {
			t.Fatalf("%q", got)
		}
	})
}

// A name reaches the server's path with every byte that is not a letter, a
// digit, a hyphen, or an underscore written as % and two hexadecimal digits,
// so no name can change the path, and the commands get the name as it is,
// behind "local:".
func TestIncusProfileNameCannotChangeThePathOfARead(t *testing.T) {
	t.Parallel()
	s := newIncusScene(t, "6.0.0")
	path := "/1.0/profiles/web%202%3Fproject%3Dother%23%2F%2E%2E?project=default"
	s.answers(path, "profile_missing")
	create := "profile create -q --project default local:web 2?project=other#/.."
	s.fails(create, "profile_create_name_with_a_slash")
	r := s.run("incus_profile", map[string]any{"name": "web 2?project=other#/.."}, false)
	incusResultIs(t, r, "failed", "changed", "incus_profile: creating the profile web 2?project=other#/..: Error: Profile names may not contain slashes")
	s.ran(query(path), create)
}
