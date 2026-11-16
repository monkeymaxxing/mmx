package modules

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"mmx/internal/protocol"
)

// The four RabbitMQ modules, against stand-ins of RabbitMQ's programs. A
// stand-in prints what the real program printed on a test node, measured on
// 2026-10-10 in MMX's own command environment: RabbitMQ 3.9.27, older than
// the 3.10.8 of Debian 12, the oldest version that the integration suite
// meets, and RabbitMQ 4.3.6 on Rocky Linux 9, the newest. Only the node's
// host name is another one. tests/integration/rabbitmq.py proves the modules
// against real servers.

// rabbitmqctlProgram is the real rabbitmqctl program, which the scripts run
// through runuser.
const rabbitmqctlProgram = "/usr/lib/rabbitmq/bin/rabbitmqctl"

// asRabbitmq starts every call of rabbitmqctl that a script makes.
const asRabbitmq = "runuser -u rabbitmq -- " + rabbitmqctlProgram + " "

// rabbitmqPassword is the value of the secret shop_rabbitmq_password in
// these tests. It is made up.
const rabbitmqPassword = "Sentinel p4ss'word \"with\" $HOME `id` \\ ü"

// toolAnswer is what a stand-in prints for one call, and its exit code.
type toolAnswer struct {
	stdout, stderr string
	exitCode       int
}

// What the real programs printed.
const (
	rabbitmqRuns          = "RabbitMQ on node rabbit@node1 is fully booted and running\n"
	rabbitmqStillStarts   = "Error:\nRabbitMQ on node rabbit@node1 is not running or has not fully booted yet (check with is_booting)\n"
	rabbitmqServerStopped = `Error: unable to perform an operation on node 'rabbit@node1'. Please see diagnostics information and suggestions below.

Most common reasons for this are:

 * Target node is unreachable (e.g. due to hostname resolution, TCP connection or firewall issues)
 * CLI tool fails to authenticate with the server (e.g. due to CLI tool's Erlang cookie not matching that of the server)
 * Target node is not running

In addition to the diagnostics info below:

 * See the CLI, clustering and networking guides on https://rabbitmq.com/documentation.html to learn more
 * Consult server logs on node rabbit@node1
 * If target node is configured to use long node names, don't forget to use --longnames with CLI tools

DIAGNOSTICS
===========

attempted to contact: ['rabbit@node1']

rabbit@node1:
  * connected to epmd (port 4369) on node1
  * epmd reports: node 'rabbit' not running at all
                  no other nodes on node1
  * suggestion: start the node

Current node details:
 * node name: 'rabbitmqcli-416-rabbit@node1'
 * effective user's home directory: /var/lib/rabbitmq
 * Erlang cookie hash: made-up-cookie-hash==

`
	rabbitmqServerStoppedFirstLine = "Error: unable to perform an operation on node 'rabbit@node1'. Please see diagnostics information and suggestions below."

	onlyTheDefaultVirtualHost = "[\n{\"name\":\"/\"}\n]\n"
	virtualHostsWithShop      = "[\n{\"name\":\"/\"}\n,{\"name\":\"shop\"}\n]\n"
	onlyTheUserGuest          = "[\n{\"user\":\"guest\",\"tags\":[\"administrator\"]}\n]\n"

	addUserPrinted            = "Adding user \"shop\" ...\nPassword: \nDone. Don't forget to grant the user permissions to some virtual hosts! See 'rabbitmqctl help set_permissions' to learn more.\n"
	changePasswordPrinted     = "Changing password for user \"shop\" ...\nPassword: \n"
	authenticatePrinted       = "Authenticating user \"shop\" ...\nPassword: \n"
	authenticateRefusedStderr = "Error:\nError: failed to authenticate user \"shop\"\nuser 'shop' - invalid credentials\n"
)

var (
	checkRunning     = "rabbitmq-diagnostics -q check_running"
	listVirtualHosts = "rabbitmqctl -q list_vhosts --formatter json"
	listUsers        = "rabbitmqctl -q list_users --formatter json"
	listPlugins      = "rabbitmq-plugins -q list -v --formatter json"
	theServerRuns    = toolAnswer{stdout: rabbitmqRuns}
	passwordAccepted = toolAnswer{stdout: authenticatePrinted + "Success\n"}
	passwordRefused  = toolAnswer{stdout: authenticatePrinted, stderr: authenticateRefusedStderr, exitCode: 65}
)

// skipOutsideTheRepository skips a test where the scripts are not beside the
// test binary: linux_code.py runs the compiled tests on a test node, which
// has no repository.
func skipOutsideTheRepository(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(officialExtraModulesDirectory, "rabbitmq_user", ModuleScriptFile)); os.IsNotExist(err) {
		t.Skip("the test binary runs outside the repository, as on a test node")
	}
}

// rabbitmqNode is a node whose RabbitMQ programs are stand-ins.
type rabbitmqNode struct {
	t         *testing.T
	directory string
	recorded  func() []string
}

// rabbitmqNodes holds the node of each running test that has all four
// stand-ins. A test makes its stand-ins once and gives them new answers for
// each case: on macOS the first start of a new program file takes a third
// of a second.
var rabbitmqNodes sync.Map

// rabbitmqStandIn is the stand-in of one of RabbitMQ's programs. It reads the
// test's answers from the files that newRabbitmqNode writes: a shell case
// statement that chooses the answer by the call's arguments, and for each
// answer its standard output, its standard error, and its exit code. The
// three password commands of rabbitmqctl note what they read from their
// standard input.
const rabbitmqStandIn = `tool=TOOL
arguments="$*"
case "$1" in add_user|change_password|authenticate_user) { printf '%s <- ' "$arguments"; cat; } >> "$STUBS/standard_input";; esac
answer=''
if [ -e "$STUBS/changed" ] && [ -e "$STUBS/$tool.after_a_change.cases" ]; then . "$STUBS/$tool.after_a_change.cases"; fi
if [ -z "$answer" ] && [ -e "$STUBS/$tool.answer.cases" ]; then . "$STUBS/$tool.answer.cases"; fi
if [ -z "$answer" ]; then echo "the test expects no call $tool $arguments" >&2; exit 99; fi
cat "$STUBS/$answer.stdout"
cat "$STUBS/$answer.stderr" >&2
code=$(cat "$STUBS/$answer.exit_code")
if [ "$tool" = rabbitmq-plugins ] && [ "$code" = 0 ]; then
case " $arguments " in *' enable '*|*' disable '*|*' set '*) : > "$STUBS/changed";; esac
fi
exit "$code"`

// newRabbitmqNode gives the test the stand-ins runuser, rabbitmqctl,
// rabbitmq-diagnostics, and rabbitmq-plugins, except the programs that
// without names, and their answers for the next case. answers maps a call,
// the program's name and its arguments as in "rabbitmqctl -q list_vhosts
// --formatter json", to what the program prints; a call that answers does
// not have fails the stand-in with exit code 99. afterAChange holds the
// answers of rabbitmq-plugins from the moment one of its commands enable,
// disable, or set has returned 0. The stand-in of runuser runs the program
// that follows "-u rabbitmq --", as runuser does.
func newRabbitmqNode(t *testing.T, answers, afterAChange map[string]toolAnswer, without ...string) *rabbitmqNode {
	t.Helper()
	skipOutsideTheRepository(t)
	var node *rabbitmqNode
	if kept, ok := rabbitmqNodes.Load(t); ok {
		node = kept.(*rabbitmqNode)
	}
	if node == nil || len(without) > 0 {
		tools := map[string]string{"runuser": `if [ "$1 $2 $3" != '-u rabbitmq --' ]; then echo "the stand-in of runuser got: $*" >&2; exit 98; fi
shift 3
exec "$@"`}
		for _, tool := range []string{"rabbitmqctl", "rabbitmq-diagnostics", "rabbitmq-plugins"} {
			tools[tool] = strings.Replace(rabbitmqStandIn, "TOOL", tool, 1)
		}
		for _, program := range without {
			delete(tools, program)
		}
		node = &rabbitmqNode{t: t, recorded: stubTools(t, tools)}
		node.directory = stubToolsDirectory(t)
		if len(without) == 0 {
			rabbitmqNodes.Store(t, node)
			t.Cleanup(func() { rabbitmqNodes.Delete(t) })
		}
	}
	setWorkerEnvironment(t, "PATH", node.directory+":/usr/bin:/bin")

	// The answers of the case before go, with what the stand-ins noted.
	old, err := os.ReadDir(node.directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range old {
		if name := entry.Name(); strings.Contains(name, ".") || name == "changed" || name == "calls" || name == "standard_input" {
			if err := os.Remove(filepath.Join(node.directory, name)); err != nil {
				t.Fatal(err)
			}
		}
	}
	files := map[string]string{}
	for set, from := range map[string]map[string]toolAnswer{"answer": answers, "after_a_change": afterAChange} {
		keys := make([]string, 0, len(from))
		for key := range from {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for i, key := range keys {
			tool, arguments, _ := strings.Cut(key, " ")
			name := fmt.Sprintf("%s.%s.%d", tool, set, i)
			files[name+".stdout"], files[name+".stderr"], files[name+".exit_code"] = from[key].stdout, from[key].stderr, fmt.Sprint(from[key].exitCode)
			files[tool+"."+set+".cases"] += "case \"$arguments\" in '" + strings.ReplaceAll(arguments, "'", `'\''`) + "') answer=" + name + ";; esac\n"
		}
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(node.directory, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return node
}

// module loads an official extra module as officialExtraModule does, from a
// copy whose script names the stand-in of rabbitmqctl in place of the real
// program's path: the machine that runs the Go tests cannot have a file at
// /usr/lib/rabbitmq/bin. Nothing else of the script differs.
func (n *rabbitmqNode) module(name string) Module {
	n.t.Helper()
	source := filepath.Join(officialExtraModulesDirectory, name)
	script, err := os.ReadFile(filepath.Join(source, ModuleScriptFile))
	if err != nil {
		n.t.Fatal(err)
	}
	line := `RABBITMQCTL_PROGRAM = "` + rabbitmqctlProgram + `"`
	if count := strings.Count(string(script), line); name != "rabbitmq_plugin" && count != 1 {
		n.t.Fatalf("the script of %s holds the line %s %d times; this test replaces it once", name, line, count)
	}
	description, err := os.ReadFile(filepath.Join(source, ModuleDescriptionFile))
	if err != nil {
		n.t.Fatal(err)
	}
	directory := filepath.Join(n.t.TempDir(), name)
	if err := os.Mkdir(directory, 0755); err != nil {
		n.t.Fatal(err)
	}
	replaced := strings.Replace(string(script), line, `RABBITMQCTL_PROGRAM = "`+n.directory+`/rabbitmqctl"`, 1)
	for file, content := range map[string]string{ModuleScriptFile: replaced, ModuleDescriptionFile: string(description)} {
		if err := os.WriteFile(filepath.Join(directory, file), []byte(content), 0644); err != nil {
			n.t.Fatal(err)
		}
	}
	read, err := ReadModuleDescription(directory, name)
	if err != nil {
		n.t.Fatal(err)
	}
	return Module{Kind: KindOfficialExtra, Description: read, Directory: directory}
}

// real writes the real program's path where a text names the stand-in.
func (n *rabbitmqNode) real(text string) string {
	return strings.ReplaceAll(text, n.directory+"/rabbitmqctl", rabbitmqctlProgram)
}

// run runs one node task as the node task execution engine reports it. The
// task's scope has the secret shop_rabbitmq_password.
func (n *rabbitmqNode) run(name string, inputs map[string]any, dryrun bool) protocol.Result {
	n.t.Helper()
	scope, trace := taskScope(map[string]string{"shop_rabbitmq_password": rabbitmqPassword})
	r := Runtime{Dryrun: dryrun}.Starlark(testContext(n.t), name, n.module(name), inputs, scope)
	if r.Secret || trace.Used {
		n.t.Errorf("the task's result is hidden as a secret: %+v", r)
	}
	r = reportedByTheEngine(r, scope)
	r.Reason, r.Diff = n.real(r.Reason), n.real(r.Diff)
	// The three modules whose task can create its thing return the output
	// created: true exactly when the task created the thing, or in a dryrun
	// when a run would. rabbitmq_plugin has no output.
	if r.Status == "succeeded" {
		created, has := r.ModuleOutputs["created"]
		creates := strings.HasPrefix(r.Reason, "created ") || strings.HasPrefix(r.Reason, "would create ")
		if name == "rabbitmq_plugin" && len(r.ModuleOutputs) != 0 {
			n.t.Errorf("rabbitmq_plugin returned the outputs %v", r.ModuleOutputs)
		}
		if name != "rabbitmq_plugin" && (!has || created != creates || len(r.ModuleOutputs) != 1) {
			n.t.Errorf("%s: the outputs are %v for the reason %q", name, r.ModuleOutputs, r.Reason)
		}
	}
	if whole, _ := json.Marshal(r); strings.Contains(string(whole), "Sentinel") {
		n.t.Errorf("the result holds the password: %s", whole)
	}
	return r
}

// calls returns the commands that the script ran since the last call of
// calls, in order, each as the script wrote it.
func (n *rabbitmqNode) calls() []string {
	n.t.Helper()
	var calls []string
	for _, line := range n.recorded() {
		// The stand-in of runuser starts the stand-in of rabbitmqctl, which
		// notes the same call once more.
		if !strings.HasPrefix(line, "rabbitmqctl ") {
			calls = append(calls, n.real(line))
		}
	}
	if strings.Contains(strings.Join(calls, "\n"), "Sentinel") {
		n.t.Errorf("a command line holds the password: %q", calls)
	}
	if err := os.Remove(filepath.Join(n.directory, "calls")); err != nil && !os.IsNotExist(err) {
		n.t.Fatal(err)
	}
	return calls
}

// standardInput returns what the password commands of rabbitmqctl read from
// their standard input: one line for each call, as "add_user shop <- text".
func (n *rabbitmqNode) standardInput() string {
	n.t.Helper()
	b, err := os.ReadFile(filepath.Join(n.directory, "standard_input"))
	if err != nil && !os.IsNotExist(err) {
		n.t.Fatal(err)
	}
	return string(b)
}

// expectResult fails the test unless r has this status, change, and reason.
func expectResult(t *testing.T, r protocol.Result, status, change, reason string) {
	t.Helper()
	if r.Status != status || r.Change != change || r.Reason != reason {
		t.Errorf("status %q, change %q, reason %q\n   want %q, %q, %q", r.Status, r.Change, r.Reason, status, change, reason)
	}
}

func expectCalls(t *testing.T, node *rabbitmqNode, want ...string) {
	t.Helper()
	if got := node.calls(); !slices.Equal(got, want) {
		t.Errorf("the script ran:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// expectFailedAndTriedAgain fails the test unless the result is an ordinary
// failure with this change and reason: MMX may try the task again.
func expectFailedAndTriedAgain(t *testing.T, r protocol.Result, change, reason string) {
	t.Helper()
	expectResult(t, r, "failed", change, reason)
	if r.FailureKind != "" || r.NoOtherTry {
		t.Errorf("failure kind %q, no other try %v for %q; MMX tries a failed command again", r.FailureKind, r.NoOtherTry, reason)
	}
}

// expectInvalidInputs fails the test unless the task fails as a configuration
// error with this reason, in a dryrun and in a run, before the script looks
// for a program.
func expectInvalidInputs(t *testing.T, module string, inputs map[string]any, reason string) {
	t.Helper()
	for _, dryrun := range []bool{true, false} {
		node := newRabbitmqNode(t, nil, nil, "runuser", "rabbitmqctl", "rabbitmq-diagnostics", "rabbitmq-plugins")
		r := node.run(module, inputs, dryrun)
		expectResult(t, r, "failed", "unchanged", reason)
		if r.FailureKind != "config" {
			t.Errorf("failure kind %q for %s", r.FailureKind, reason)
		}
	}
}

// The scripts cannot load a shared file, so each holds a copy of the
// functions that they share. The copies are the same text.
func TestRabbitmqScriptsHoldTheSameSharedFunctions(t *testing.T) {
	t.Parallel()
	skipOutsideTheRepository(t)
	const ownPart = "# This script's own part.\n"
	const forTheServer = "# The same in rabbitmq_virtual_host, rabbitmq_user, and rabbitmq_policy"
	shared := map[string]string{}
	for _, name := range []string{"rabbitmq_virtual_host", "rabbitmq_user", "rabbitmq_policy", "rabbitmq_plugin"} {
		script, err := os.ReadFile(filepath.Join(officialExtraModulesDirectory, name, ModuleScriptFile))
		if err != nil {
			t.Fatal(err)
		}
		_, after, found := strings.Cut(string(script), "# The same in all four rabbitmq_* scripts.")
		block, _, foundEnd := strings.Cut(after, ownPart)
		if !found || !foundEnd {
			t.Fatalf("%s: the script lacks the comments that mark its shared functions", name)
		}
		shared[name] = block
	}
	forAll, _, found := strings.Cut(shared["rabbitmq_virtual_host"], forTheServer)
	if !found {
		t.Fatal("rabbitmq_virtual_host: the script lacks the comment that marks the functions for the server")
	}
	for name, block := range shared {
		want := shared["rabbitmq_virtual_host"]
		if name == "rabbitmq_plugin" {
			// The plugin script asks no server, so it holds only the first part.
			want = forAll
		}
		if block != want {
			t.Errorf("%s: its copy of the shared functions differs from the copy of rabbitmq_virtual_host", name)
		}
	}
}

// Each module's page shows, under "Set up", the example project's playbook
// from its first line to the task that starts the server, word for word. The
// integration suite runs the example project, so the page's node tasks are
// tested.
func TestRabbitmqPagesShowTheSetUpOfTheExampleProject(t *testing.T) {
	t.Parallel()
	skipOutsideTheRepository(t)
	playbook, err := os.ReadFile(filepath.Join(officialExtraModulesDirectory, "..", "examples", "rabbitmq-server-for-one-application", "playbooks", "configure.yml"))
	if err != nil {
		t.Fatal(err)
	}
	setUp, _, found := strings.Cut(string(playbook), "\n      # RabbitMQ's web interface")
	if !found || !strings.HasSuffix(setUp, "          state: running\n          start_at_boot: true\n") {
		t.Fatal("the example playbook no longer has the comment about RabbitMQ's web interface directly after the task that starts the server")
	}
	for _, name := range []string{"rabbitmq_virtual_host", "rabbitmq_user", "rabbitmq_policy", "rabbitmq_plugin"} {
		page, err := os.ReadFile(filepath.Join(officialExtraModulesDirectory, name, "README.md"))
		if err != nil {
			t.Fatal(err)
		}
		_, afterHeading, found := strings.Cut(string(page), "\n## Set up\n")
		section, _, _ := strings.Cut(afterHeading, "\n## ")
		if !found || !strings.Contains(section, "```yaml\n"+setUp+"```\n") {
			t.Errorf("README.md of %s does not show the example playbook's tasks that install and start RabbitMQ as they are", name)
		}
	}
}

func TestRabbitmqVirtualHostRejectsInputs(t *testing.T) {
	t.Parallel()
	for reason, name := range map[string]string{
		"rabbitmq_virtual_host.name: must not be empty":                                                             "",
		"rabbitmq_virtual_host.name: must not start with a hyphen (-), which RabbitMQ's programs read as an option": "-shop",
	} {
		expectInvalidInputs(t, "rabbitmq_virtual_host", map[string]any{"name": name}, reason)
	}
}

// A program that is not on the node is a missing prerequisite, which names
// the program and its package: a dryrun is not checked, and a run fails
// without another try.
func TestRabbitmqModulesNameAMissingProgram(t *testing.T) {
	t.Parallel()
	const fromPackage = " is not installed; install the package rabbitmq-server in an earlier node task"
	policy := map[string]any{"priority": 0, "name": "limit", "virtual_host": "shop", "pattern": ".*", "apply_to": "queues", "definition": map[string]any{"max-length": 5}}
	for _, c := range []struct {
		module  string
		inputs  map[string]any
		without string
		reason  string
	}{
		{"rabbitmq_virtual_host", map[string]any{"name": "shop"}, "runuser", "rabbitmq_virtual_host: the program runuser is not installed; install the package util-linux in an earlier node task"},
		{"rabbitmq_virtual_host", map[string]any{"name": "shop"}, "rabbitmqctl", "rabbitmq_virtual_host: the program " + rabbitmqctlProgram + fromPackage},
		{"rabbitmq_virtual_host", map[string]any{"name": "shop"}, "rabbitmq-diagnostics", "rabbitmq_virtual_host: the program rabbitmq-diagnostics" + fromPackage},
		{"rabbitmq_user", map[string]any{"name": "shop", "state": "absent"}, "rabbitmqctl", "rabbitmq_user: the program " + rabbitmqctlProgram + fromPackage},
		{"rabbitmq_policy", policy, "rabbitmqctl", "rabbitmq_policy: the program " + rabbitmqctlProgram + fromPackage},
		{"rabbitmq_plugin", map[string]any{"name": "rabbitmq_management", "state": "enabled"}, "rabbitmq-plugins", "rabbitmq_plugin: the program rabbitmq-plugins" + fromPackage},
	} {
		node := newRabbitmqNode(t, nil, nil, c.without)
		expectResult(t, node.run(c.module, c.inputs, true), "not_checked", "unknown", c.reason)
		r := node.run(c.module, c.inputs, false)
		expectResult(t, r, "failed", "unchanged", c.reason)
		if r.FailureKind != "missing_prerequisite" {
			t.Errorf("%s without %s: failure kind %q", c.module, c.without, r.FailureKind)
		}
		expectCalls(t, node)
	}
}

// While the server does not answer, a dryrun is not checked and a run fails
// as an ordinary failure, which MMX tries again. The script asks only
// rabbitmq-diagnostics check_running; a server that still starts counts as
// not answering.
func TestRabbitmqModulesAskFirstWhetherTheServerRuns(t *testing.T) {
	t.Parallel()
	policy := map[string]any{"name": "limit", "virtual_host": "shop", "state": "absent"}
	for said, stderr := range map[string]string{
		rabbitmqServerStoppedFirstLine: rabbitmqServerStopped,
		"RabbitMQ on node rabbit@node1 is not running or has not fully booted yet (check with is_booting)": rabbitmqStillStarts,
	} {
		for module, inputs := range map[string]map[string]any{
			"rabbitmq_virtual_host": {"name": "shop"},
			"rabbitmq_user":         {"name": "shop", "password": "{{ .secrets.shop_rabbitmq_password }}"},
			"rabbitmq_policy":       policy,
		} {
			node := newRabbitmqNode(t, map[string]toolAnswer{checkRunning: {stderr: stderr, exitCode: 69}}, nil)
			reason := module + ": the RabbitMQ server on this node does not answer; start its service in an earlier node task (rabbitmq-diagnostics check_running said: " + said + ")"
			expectResult(t, node.run(module, inputs, true), "not_checked", "unknown", reason)
			expectCalls(t, node, checkRunning)
			r := node.run(module, inputs, false)
			expectResult(t, r, "failed", "unchanged", reason)
			if r.FailureKind != "" {
				t.Errorf("%s: failure kind %q; an ordinary failure is tried again", module, r.FailureKind)
			}
			expectCalls(t, node, checkRunning)
		}
	}
}

func TestRabbitmqVirtualHost(t *testing.T) {
	t.Parallel()
	shop := map[string]any{"name": "shop"}
	deleted := map[string]any{"name": "shop", "state": "deleted_with_all_its_data"}
	read := []string{checkRunning, asRabbitmq + "-q list_vhosts --formatter json"}
	withChange := func(command string) []string { return append(slices.Clone(read), asRabbitmq+command) }

	// The virtual host is missing.
	missing := map[string]toolAnswer{checkRunning: theServerRuns, listVirtualHosts: {stdout: onlyTheDefaultVirtualHost},
		"rabbitmqctl -q add_vhost shop": {}}
	node := newRabbitmqNode(t, missing, nil)
	expectResult(t, node.run("rabbitmq_virtual_host", shop, true), "succeeded", "would_change", "would create the virtual host shop")
	expectCalls(t, node, read...)
	created := node.run("rabbitmq_virtual_host", shop, false)
	expectResult(t, created, "succeeded", "changed", "created the virtual host shop")
	if created.Diff != "ran: "+asRabbitmq+"-q add_vhost shop\n" {
		t.Errorf("diff %q", created.Diff)
	}
	expectCalls(t, node, withChange("-q add_vhost shop")...)
	for _, dryrun := range []bool{true, false} {
		expectResult(t, node.run("rabbitmq_virtual_host", deleted, dryrun), "succeeded", "unchanged", "")
		expectCalls(t, node, read...)
	}

	// The virtual host exists.
	exists := map[string]toolAnswer{checkRunning: theServerRuns, listVirtualHosts: {stdout: virtualHostsWithShop},
		"rabbitmqctl -q delete_vhost shop": {}}
	node = newRabbitmqNode(t, exists, nil)
	for _, dryrun := range []bool{true, false} {
		unchanged := node.run("rabbitmq_virtual_host", shop, dryrun)
		expectResult(t, unchanged, "succeeded", "unchanged", "")
		expectCalls(t, node, read...)
		// The fingerprint is the task's wanted state, whatever the node has.
		if unchanged.Fingerprint == "" || unchanged.Fingerprint != created.Fingerprint {
			t.Errorf("fingerprint %q, and %q when the task created the virtual host", unchanged.Fingerprint, created.Fingerprint)
		}
	}
	expectResult(t, node.run("rabbitmq_virtual_host", deleted, true), "succeeded", "would_change", "would delete the virtual host shop with all its data")
	expectCalls(t, node, read...)
	expectResult(t, node.run("rabbitmq_virtual_host", deleted, false), "succeeded", "changed", "deleted the virtual host shop with all its data")
	expectCalls(t, node, withChange("-q delete_vhost shop")...)
}

func TestRabbitmqVirtualHostFailures(t *testing.T) {
	t.Parallel()
	shop := map[string]any{"name": "shop"}

	// The server stopped between the first question and the read.
	node := newRabbitmqNode(t, map[string]toolAnswer{checkRunning: theServerRuns, listVirtualHosts: {stderr: rabbitmqServerStopped, exitCode: 69}}, nil)
	for _, dryrun := range []bool{true, false} {
		expectResult(t, node.run("rabbitmq_virtual_host", shop, dryrun), "failed", "unchanged",
			"rabbitmq_virtual_host: reading the virtual hosts: "+rabbitmqServerStoppedFirstLine)
	}
	// A read that prints something else than a JSON list.
	node = newRabbitmqNode(t, map[string]toolAnswer{checkRunning: theServerRuns, listVirtualHosts: {stdout: "Listing vhosts ...\n"}}, nil)
	expectResult(t, node.run("rabbitmq_virtual_host", shop, false), "failed", "unchanged",
		"rabbitmq_virtual_host: reading the virtual hosts: rabbitmqctl printed no JSON list of objects: Listing vhosts ...\n")
	// A changing command that fails: it ran, so the change is not "unchanged".
	node = newRabbitmqNode(t, map[string]toolAnswer{checkRunning: theServerRuns, listVirtualHosts: {stdout: onlyTheDefaultVirtualHost},
		"rabbitmqctl -q add_vhost shop": {stderr: rabbitmqServerStopped, exitCode: 69}}, nil)
	expectFailedAndTriedAgain(t, node.run("rabbitmq_virtual_host", shop, false), "changed",
		"rabbitmq_virtual_host: creating the virtual host shop: "+rabbitmqServerStoppedFirstLine)
	// MMX's next try reads the server again, finds no virtual host, and
	// repeats the command that failed.
	node.calls()
	node = newRabbitmqNode(t, map[string]toolAnswer{checkRunning: theServerRuns, listVirtualHosts: {stdout: onlyTheDefaultVirtualHost}, "rabbitmqctl -q add_vhost shop": {}}, nil)
	expectResult(t, node.run("rabbitmq_virtual_host", shop, false), "succeeded", "changed", "created the virtual host shop")
	expectCalls(t, node, checkRunning, asRabbitmq+"-q list_vhosts --formatter json", asRabbitmq+"-q add_vhost shop")
	// A command that prints nothing and fails.
	node = newRabbitmqNode(t, map[string]toolAnswer{checkRunning: theServerRuns, listVirtualHosts: {exitCode: 70}}, nil)
	expectResult(t, node.run("rabbitmq_virtual_host", shop, false), "failed", "unchanged",
		"rabbitmq_virtual_host: reading the virtual hosts: the command ended with exit code 70 and printed no message")
}

func TestRabbitmqUserRejectsInputs(t *testing.T) {
	t.Parallel()
	entry := map[string]any{"virtual_host": "shop", "configure": ".*", "write": ".*", "read": ".*"}
	without := func(key string) map[string]any {
		copy := map[string]any{}
		for k, v := range entry {
			if k != key {
				copy[k] = v
			}
		}
		return copy
	}
	with := func(key string, v any) map[string]any {
		copy := without("")
		copy[key] = v
		return copy
	}
	for reason, inputs := range map[string]map[string]any{
		"rabbitmq_user.name: must not be empty": {"name": ""},
		"rabbitmq_user.name: must not start with a hyphen (-), which RabbitMQ's programs read as an option":                                                                                         {"name": "--help"},
		"rabbitmq_user.state: absent accepts only name; remove password":                                                                                                                            {"name": "shop", "state": "absent", "password": "p"},
		"rabbitmq_user.state: absent accepts only name; remove tags":                                                                                                                                {"name": "shop", "state": "absent", "tags": []any{}, "tags_not_listed": "remove"},
		"rabbitmq_user.state: absent accepts only name; remove permissions_not_listed":                                                                                                              {"name": "shop", "state": "absent", "permissions_not_listed": "keep"},
		"rabbitmq_user.password: must not be empty":                                                                                                                                                 {"name": "shop", "password": ""},
		"rabbitmq_user.password: must not hold a line break: RabbitMQ would take only the first line as the password":                                                                               {"name": "shop", "password": "Sentinel one\nSentinel two"},
		"rabbitmq_user.password: must not start or end with white space: RabbitMQ would cut it off and store another password":                                                                      {"name": "shop", "password": " Sentinel"},
		"rabbitmq_user.tags_not_listed: is required with tags: remove takes away every tag of the user that tags does not list, and keep leaves the user's other tags alone":                        {"name": "shop", "tags": []any{"monitoring"}},
		"rabbitmq_user.tags: is required with tags_not_listed":                                                                                                                                      {"name": "shop", "tags_not_listed": "remove"},
		"rabbitmq_user.permissions_not_listed: is required with permissions: remove clears the user's permissions on every virtual host that permissions does not list, and keep leaves them alone": {"name": "shop", "permissions": []any{entry}},
		"rabbitmq_user.permissions: is required with permissions_not_listed":                                                                                                                        {"name": "shop", "permissions_not_listed": "keep"},
		"rabbitmq_user.tags: a tag must not be empty":                                                                                                                                               {"name": "shop", "tags": []any{"monitoring", ""}, "tags_not_listed": "keep"},
		"rabbitmq_user.tags: a tag must not start with a hyphen (-), which RabbitMQ's programs read as an option":                                                                                   {"name": "shop", "tags": []any{"-q"}, "tags_not_listed": "keep"},
		"rabbitmq_user.permissions: must be a list; each entry names a virtual_host and the three regular expressions configure, write, and read":                                                   {"name": "shop", "permissions": entry, "permissions_not_listed": "keep"},
		"rabbitmq_user.permissions: entry 2 must be a mapping with virtual_host, configure, write, and read":                                                                                        {"name": "shop", "permissions": []any{entry, "shop"}, "permissions_not_listed": "keep"},
		"rabbitmq_user.permissions: entry 1 needs read as a text; virtual_host, configure, write, and read are all required":                                                                        {"name": "shop", "permissions": []any{without("read")}, "permissions_not_listed": "keep"},
		"rabbitmq_user.permissions: entry 1 needs configure as a text; virtual_host, configure, write, and read are all required":                                                                   {"name": "shop", "permissions": []any{with("configure", 7)}, "permissions_not_listed": "keep"},
		"rabbitmq_user.permissions: entry 1 has the unknown key vhost; the keys are virtual_host, configure, write, and read":                                                                       {"name": "shop", "permissions": []any{with("vhost", "shop")}, "permissions_not_listed": "keep"},
		"rabbitmq_user.permissions: entry 1: virtual_host must not start with a hyphen (-), which RabbitMQ's programs read as an option":                                                            {"name": "shop", "permissions": []any{with("virtual_host", "-p")}, "permissions_not_listed": "keep"},
		"rabbitmq_user.permissions: the virtual host shop is listed twice":                                                                                                                          {"name": "shop", "permissions": []any{entry, entry}, "permissions_not_listed": "keep"},
	} {
		expectInvalidInputs(t, "rabbitmq_user", inputs, reason)
	}
}

// The first run of a user task: the user, its tags, and its permissions. The
// password reaches rabbitmqctl through its standard input, in the plain form
// of the command, and is on no command line and in no result.
func TestRabbitmqUserIsCreated(t *testing.T) {
	t.Parallel()
	inputs := map[string]any{"name": "shop", "password": "{{ .secrets.shop_rabbitmq_password }}",
		"tags": []any{"monitoring", "management"}, "tags_not_listed": "remove",
		"permissions": []any{
			map[string]any{"virtual_host": "shop", "configure": "^shop\\.", "write": "", "read": "-.*"},
			map[string]any{"virtual_host": "/", "configure": ".*", "write": ".*", "read": ".*"}},
		"permissions_not_listed": "remove"}
	answers := map[string]toolAnswer{checkRunning: theServerRuns, listUsers: {stdout: onlyTheUserGuest}, listVirtualHosts: {stdout: virtualHostsWithShop},
		"rabbitmqctl add_user shop":                                    {stdout: addUserPrinted},
		"rabbitmqctl -q set_user_tags shop monitoring management":      {},
		"rabbitmqctl -q set_permissions -p shop -- shop ^shop\\.  -.*": {},
		"rabbitmqctl -q set_permissions -p / -- shop .* .* .*":         {},
	}
	read := []string{checkRunning, asRabbitmq + "-q list_users --formatter json", asRabbitmq + "-q list_vhosts --formatter json"}
	node := newRabbitmqNode(t, answers, nil)
	expectResult(t, node.run("rabbitmq_user", inputs, true), "succeeded", "would_change",
		"would create the user shop; would add the tags monitoring, management to the user shop; "+
			"would set the permissions of the user shop on the virtual host shop; would set the permissions of the user shop on the virtual host /")
	expectCalls(t, node, read...)
	if node.standardInput() != "" {
		t.Errorf("a dryrun handed the password to a command: %q", node.standardInput())
	}
	expectResult(t, node.run("rabbitmq_user", inputs, false), "succeeded", "changed",
		"created the user shop; added the tags monitoring, management to the user shop; "+
			"set the permissions of the user shop on the virtual host shop; set the permissions of the user shop on the virtual host /")
	expectCalls(t, node, append(read, asRabbitmq+"add_user shop", asRabbitmq+"-q set_user_tags shop monitoring management",
		asRabbitmq+"-q set_permissions -p shop -- shop ^shop\\.  -.*", asRabbitmq+"-q set_permissions -p / -- shop .* .* .*")...)
	if got := node.standardInput(); got != "add_user shop <- "+rabbitmqPassword+"\n" {
		t.Errorf("standard input: %q", got)
	}

	// Without a password the task cannot create the user. An earlier node
	// task of the same playbook may create it, so this is a missing
	// prerequisite: a dryrun is not checked, and a run fails once. The task
	// says so once it knows that the user is missing.
	delete(inputs, "password")
	const noUser = "rabbitmq_user: the user shop does not exist, and the task has no password to create it; " +
		"add password, or create the user with a rabbitmq_user task in an earlier node task"
	expectResult(t, node.run("rabbitmq_user", inputs, true), "not_checked", "unknown", noUser)
	expectCalls(t, node, checkRunning, asRabbitmq+"-q list_users --formatter json")
	withoutPassword := node.run("rabbitmq_user", inputs, false)
	expectResult(t, withoutPassword, "failed", "unchanged", noUser)
	if withoutPassword.FailureKind != "missing_prerequisite" {
		t.Errorf("failure kind %q", withoutPassword.FailureKind)
	}
	expectCalls(t, node, checkRunning, asRabbitmq+"-q list_users --formatter json")

	// A permission on a virtual host that does not exist: nothing is created.
	inputs["password"] = "{{ .secrets.shop_rabbitmq_password }}"
	answers[listVirtualHosts] = toolAnswer{stdout: onlyTheDefaultVirtualHost}
	node = newRabbitmqNode(t, answers, nil)
	const noVirtualHost = "rabbitmq_user: the virtual host shop does not exist; create it with a rabbitmq_virtual_host task in an earlier node task"
	expectResult(t, node.run("rabbitmq_user", inputs, true), "not_checked", "unknown", noVirtualHost)
	r := node.run("rabbitmq_user", inputs, false)
	expectResult(t, r, "failed", "unchanged", noVirtualHost)
	if r.FailureKind != "missing_prerequisite" {
		t.Errorf("failure kind %q", r.FailureKind)
	}
	node.calls()
	if node.standardInput() != "" {
		t.Errorf("the password reached a command: %q", node.standardInput())
	}
}

// An existing user: unchanged with the lists of the oldest and of the newest
// RabbitMQ, whose rows have their keys in another order, and then each
// difference alone.
func TestRabbitmqUserIsCompared(t *testing.T) {
	t.Parallel()
	const (
		shopWithTags      = "[\n{\"user\":\"guest\",\"tags\":[\"administrator\"]}\n,{\"user\":\"shop\",\"tags\":[\"administrator\",\"management\"]}\n]\n"
		permissionsOf3927 = "[\n{\"vhost\":\"shop\",\"configure\":\".*\",\"write\":\".*\",\"read\":\".*\"}\n,{\"vhost\":\"/\",\"configure\":\"^mmx-.*\",\"write\":\"\",\"read\":\"-dash.*\"}\n]\n"
		permissionsOf436  = "[\n{\"read\":\"-dash.*\",\"write\":\"\",\"configure\":\"^mmx-.*\",\"vhost\":\"/\"}\n,{\"read\":\".*\",\"write\":\".*\",\"configure\":\".*\",\"vhost\":\"shop\"}\n]\n"
		listPermissions   = "rabbitmqctl -q list_user_permissions shop --formatter json"
		authenticate      = "rabbitmqctl authenticate_user shop"
	)
	asListed := map[string]any{"name": "shop", "password": "{{ .secrets.shop_rabbitmq_password }}",
		"tags": []any{"management", "administrator"}, "tags_not_listed": "remove",
		"permissions": []any{
			map[string]any{"virtual_host": "/", "configure": "^mmx-.*", "write": "", "read": "-dash.*"},
			map[string]any{"virtual_host": "shop", "configure": ".*", "write": ".*", "read": ".*"}},
		"permissions_not_listed": "remove"}
	read := []string{checkRunning, asRabbitmq + "-q list_users --formatter json", asRabbitmq + "-q list_user_permissions shop --formatter json", asRabbitmq + "authenticate_user shop"}
	for version, permissions := range map[string]string{"3.9.27": permissionsOf3927, "4.3.6": permissionsOf436} {
		node := newRabbitmqNode(t, map[string]toolAnswer{checkRunning: theServerRuns, listUsers: {stdout: shopWithTags},
			listPermissions: {stdout: permissions}, authenticate: passwordAccepted}, nil)
		for _, dryrun := range []bool{true, false} {
			expectResult(t, node.run("rabbitmq_user", asListed, dryrun), "succeeded", "unchanged", "")
			expectCalls(t, node, read...)
		}
		if got := node.standardInput(); got != strings.Repeat("authenticate_user shop <- "+rabbitmqPassword+"\n", 2) {
			t.Errorf("RabbitMQ %s: standard input: %q", version, got)
		}
	}

	for name, c := range map[string]struct {
		change       map[string]any
		authenticate toolAnswer
		commands     []string
		reason       string
	}{
		"another password": {nil, passwordRefused, []string{"change_password shop"}, "set the password of the user shop"},
		"a tag more, with keep": {map[string]any{"tags": []any{"monitoring"}, "tags_not_listed": "keep"}, passwordAccepted,
			[]string{"-q set_user_tags shop administrator management monitoring"}, "added the tag monitoring to the user shop"},
		"other tags, with remove": {map[string]any{"tags": []any{"monitoring", "management"}}, passwordAccepted,
			[]string{"-q set_user_tags shop management monitoring"}, "added the tag monitoring to the user shop; removed the tag administrator from the user shop"},
		"no tag at all": {map[string]any{"tags": []any{}}, passwordAccepted,
			[]string{"-q set_user_tags shop"}, "removed the tags administrator, management from the user shop"},
		"another expression, and one virtual host less": {map[string]any{"permissions": []any{map[string]any{"virtual_host": "shop", "configure": ".*", "write": ".*", "read": "^$"}}}, passwordAccepted,
			[]string{"-q set_permissions -p shop -- shop .* .* ^$", "-q clear_permissions -p / shop"},
			"set the permissions of the user shop on the virtual host shop; cleared the permissions of the user shop on the virtual host /"},
		"one virtual host less, with keep": {map[string]any{"permissions": []any{map[string]any{"virtual_host": "shop", "configure": ".*", "write": ".*", "read": ".*"}}, "permissions_not_listed": "keep"},
			passwordAccepted, nil, ""},
		"an empty list of permissions": {map[string]any{"permissions": []any{}}, passwordAccepted,
			[]string{"-q clear_permissions -p shop shop", "-q clear_permissions -p / shop"},
			"cleared the permissions of the user shop on the virtual host shop; cleared the permissions of the user shop on the virtual host /"},
	} {
		inputs := map[string]any{}
		for k, v := range asListed {
			inputs[k] = v
		}
		for k, v := range c.change {
			inputs[k] = v
		}
		answers := map[string]toolAnswer{checkRunning: theServerRuns, listUsers: {stdout: shopWithTags}, listPermissions: {stdout: permissionsOf3927}, authenticate: c.authenticate}
		ran := slices.Clone(read)
		for _, command := range c.commands {
			answers["rabbitmqctl "+command] = toolAnswer{stdout: map[bool]string{true: changePasswordPrinted}[command == "change_password shop"]}
			ran = append(ran, asRabbitmq+command)
		}
		node := newRabbitmqNode(t, answers, nil)
		if len(c.commands) == 0 {
			expectResult(t, node.run("rabbitmq_user", inputs, false), "succeeded", "unchanged", "")
			expectCalls(t, node, read...)
			continue
		}
		expectResult(t, node.run("rabbitmq_user", inputs, true), "succeeded", "would_change", wouldForm(c.reason))
		expectCalls(t, node, read...)
		expectResult(t, node.run("rabbitmq_user", inputs, false), "succeeded", "changed", c.reason)
		expectCalls(t, node, ran...)
		want := strings.Repeat("authenticate_user shop <- "+rabbitmqPassword+"\n", 2)
		if name == "another password" {
			want += "change_password shop <- " + rabbitmqPassword + "\n"
		}
		if got := node.standardInput(); got != want {
			t.Errorf("%s: standard input: %q", name, got)
		}
	}

	// A task without password asks nothing about the password, and a task
	// without tags and permissions reads neither.
	node := newRabbitmqNode(t, map[string]toolAnswer{checkRunning: theServerRuns, listUsers: {stdout: shopWithTags}}, nil)
	expectResult(t, node.run("rabbitmq_user", map[string]any{"name": "shop"}, false), "succeeded", "unchanged", "")
	expectCalls(t, node, checkRunning, asRabbitmq+"-q list_users --formatter json")
}

// wouldForm turns a run's reason into the reason of a dryrun.
func wouldForm(reason string) string {
	parts := strings.Split(reason, "; ")
	for i, part := range parts {
		verb, rest, _ := strings.Cut(part, " ")
		infinitive := map[string]string{"set": "set", "added": "add", "removed": "remove", "cleared": "clear", "created": "create", "deleted": "delete", "changed": "change"}[verb]
		parts[i] = "would " + infinitive + " " + rest
	}
	return strings.Join(parts, "; ")
}

func TestRabbitmqUserFailures(t *testing.T) {
	t.Parallel()
	const shopExists = "[\n{\"user\":\"guest\",\"tags\":[\"administrator\"]}\n,{\"user\":\"shop\",\"tags\":[]}\n]\n"
	inputs := map[string]any{"name": "shop", "password": "{{ .secrets.shop_rabbitmq_password }}",
		"permissions":            []any{map[string]any{"virtual_host": "shop", "configure": "*[", "write": ".*", "read": ".*"}},
		"permissions_not_listed": "keep"}

	// RabbitMQ refuses a regular expression after the user was created: the
	// result fails with RabbitMQ's own message and says that the node
	// changed. Another try would get the same answer, so the failure is one
	// of the task's inputs and is not tried again. RabbitMQ 3.9 prints a log
	// line before its message.
	for version, c := range map[string]struct{ stderr, message string }{
		"3.9.27": {"\n06:41:59.758 [warn]  Failed to set permissions for 'shop' in virtual host 'shop': regular expression '*[' is invalid\nError:\n{:invalid_regexp, '*[', {'nothing to repeat', 0}}\n",
			"06:41:59.758 [warn]  Failed to set permissions for 'shop' in virtual host 'shop': regular expression '*[' is invalid {:invalid_regexp, '*[', {'nothing to repeat', 0}}"},
		"4.3.6": {"Error:\n{:invalid_regexp, ~c\"*[\", {~c\"nothing to repeat\", 0}}\n", "{:invalid_regexp, ~c\"*[\", {~c\"nothing to repeat\", 0}}"},
	} {
		node := newRabbitmqNode(t, map[string]toolAnswer{checkRunning: theServerRuns, listUsers: {stdout: onlyTheUserGuest}, listVirtualHosts: {stdout: virtualHostsWithShop},
			"rabbitmqctl add_user shop":                               {stdout: addUserPrinted},
			"rabbitmqctl -q set_permissions -p shop -- shop *[ .* .*": {stderr: c.stderr, exitCode: 69}}, nil)
		r := node.run("rabbitmq_user", inputs, false)
		expectResult(t, r, "failed", "changed", "rabbitmq_user: RabbitMQ refuses a regular expression of the permissions of the user shop on the virtual host shop: "+c.message)
		if r.FailureKind != "config" || !strings.Contains(r.Diff, "add_user shop") {
			t.Errorf("RabbitMQ %s: failure kind %q, diff %q", version, r.FailureKind, r.Diff)
		}
		expectCalls(t, node, checkRunning, asRabbitmq+"-q list_users --formatter json", asRabbitmq+"-q list_vhosts --formatter json",
			asRabbitmq+"add_user shop", asRabbitmq+"-q set_permissions -p shop -- shop *[ .* .*")
	}

	// The question about the password gets an answer that is neither yes nor
	// no: the task fails and sets no password.
	node := newRabbitmqNode(t, map[string]toolAnswer{checkRunning: theServerRuns, listUsers: {stdout: shopExists},
		"rabbitmqctl -q list_user_permissions shop --formatter json": {stdout: "[]\n"}, listVirtualHosts: {stdout: virtualHostsWithShop},
		"rabbitmqctl authenticate_user shop": {stdout: authenticatePrinted, stderr: rabbitmqServerStopped, exitCode: 69}}, nil)
	for _, dryrun := range []bool{true, false} {
		expectResult(t, node.run("rabbitmq_user", inputs, dryrun), "failed", "unchanged",
			"rabbitmq_user: asking the server whether the password is the one of the user shop: "+rabbitmqServerStoppedFirstLine)
	}
	// A read that fails is no "the user has no permissions".
	node = newRabbitmqNode(t, map[string]toolAnswer{checkRunning: theServerRuns, listUsers: {stdout: shopExists},
		"rabbitmqctl -q list_user_permissions shop --formatter json": {stderr: "Error:\n{:no_such_user, \"shop\"}\n", exitCode: 69}}, nil)
	expectResult(t, node.run("rabbitmq_user", inputs, false), "failed", "unchanged",
		"rabbitmq_user: reading the permissions of the user shop: {:no_such_user, \"shop\"}")
	// A permission that cannot be set for a reason that the script does not
	// know, after this try created the user. MMX's next try would find the
	// user and report created: false for what this task created. So the
	// script asks for no other try, its reason says first what was done, and
	// its output says that the user was created.
	setPermissions := "rabbitmqctl -q set_permissions -p shop -- shop *[ .* .*"
	node = newRabbitmqNode(t, map[string]toolAnswer{checkRunning: theServerRuns, listUsers: {stdout: onlyTheUserGuest}, listVirtualHosts: {stdout: virtualHostsWithShop},
		"rabbitmqctl add_user shop": {stdout: addUserPrinted}, setPermissions: {stderr: rabbitmqServerStopped, exitCode: 69}}, nil)
	r := node.run("rabbitmq_user", inputs, false)
	expectResult(t, r, "failed", "changed",
		"rabbitmq_user: created the user shop, and then this failed: setting the permissions of the user shop on the virtual host shop: "+rabbitmqServerStoppedFirstLine)
	if r.FailureKind != "" || !r.NoOtherTry || r.ModuleOutputs["created"] != true {
		t.Errorf("failure kind %q, no other try %v, outputs %v; a failed result with created: true has try_again: False", r.FailureKind, r.NoOtherTry, r.ModuleOutputs)
	}
	// What another try would report on the server as the first try left it:
	// the user with the task's password and without a permission.
	afterTheCreation := map[string]toolAnswer{checkRunning: theServerRuns, listUsers: {stdout: shopExists}, listVirtualHosts: {stdout: virtualHostsWithShop},
		"rabbitmqctl -q list_user_permissions shop --formatter json": {stdout: "[]\n"}, "rabbitmqctl authenticate_user shop": passwordAccepted, setPermissions: {}}
	node = newRabbitmqNode(t, afterTheCreation, nil)
	another := node.run("rabbitmq_user", inputs, false)
	expectResult(t, another, "succeeded", "changed", "set the permissions of the user shop on the virtual host shop")
	if another.ModuleOutputs["created"] != false {
		t.Errorf("the outputs of a try that finds the user: %v", another.ModuleOutputs)
	}

	// The same failure on a user that existed before is an ordinary failure:
	// MMX tries again, and the next try repeats exactly the step that failed
	// and creates nothing.
	afterTheCreation[setPermissions] = toolAnswer{stderr: rabbitmqServerStopped, exitCode: 69}
	node = newRabbitmqNode(t, afterTheCreation, nil)
	steps := []string{checkRunning, asRabbitmq + "-q list_users --formatter json", asRabbitmq + "-q list_user_permissions shop --formatter json",
		asRabbitmq + "-q list_vhosts --formatter json", asRabbitmq + "authenticate_user shop", asRabbitmq + "-q set_permissions -p shop -- shop *[ .* .*"}
	for try := 1; try <= 2; try++ {
		r = node.run("rabbitmq_user", inputs, false)
		expectFailedAndTriedAgain(t, r, "changed", "rabbitmq_user: setting the permissions of the user shop on the virtual host shop: "+rabbitmqServerStoppedFirstLine)
		if r.ModuleOutputs["created"] != false {
			t.Errorf("the outputs of a failed step on a user that existed: %v", r.ModuleOutputs)
		}
		expectCalls(t, node, steps...)
	}
	// The password cannot be set.
	delete(inputs, "permissions")
	delete(inputs, "permissions_not_listed")
	node = newRabbitmqNode(t, map[string]toolAnswer{checkRunning: theServerRuns, listUsers: {stdout: shopExists},
		"rabbitmqctl authenticate_user shop": passwordRefused,
		"rabbitmqctl change_password shop":   {stdout: changePasswordPrinted, stderr: "Error:\nUser \"shop\" does not exist\n", exitCode: 67}}, nil)
	expectResult(t, node.run("rabbitmq_user", inputs, false), "failed", "changed",
		"rabbitmq_user: setting the password of the user shop: User \"shop\" does not exist")
}

func TestRabbitmqUserIsDeleted(t *testing.T) {
	t.Parallel()
	absent := map[string]any{"name": "shop", "state": "absent"}
	read := []string{checkRunning, asRabbitmq + "-q list_users --formatter json"}
	node := newRabbitmqNode(t, map[string]toolAnswer{checkRunning: theServerRuns,
		listUsers: {stdout: "[\n{\"user\":\"guest\",\"tags\":[\"administrator\"]}\n,{\"user\":\"shop\",\"tags\":[]}\n]\n"}, "rabbitmqctl -q delete_user shop": {}}, nil)
	expectResult(t, node.run("rabbitmq_user", absent, true), "succeeded", "would_change", "would remove the user shop")
	expectCalls(t, node, read...)
	expectResult(t, node.run("rabbitmq_user", absent, false), "succeeded", "changed", "removed the user shop")
	expectCalls(t, node, append(read, asRabbitmq+"-q delete_user shop")...)

	// RabbitMQ before 3.12 fails delete_user for a user that does not exist;
	// the script reads first and runs no command for it.
	node = newRabbitmqNode(t, map[string]toolAnswer{checkRunning: theServerRuns, listUsers: {stdout: onlyTheUserGuest}}, nil)
	for _, dryrun := range []bool{true, false} {
		expectResult(t, node.run("rabbitmq_user", absent, dryrun), "succeeded", "unchanged", "")
		expectCalls(t, node, read...)
	}
}

func TestRabbitmqPolicyRejectsInputs(t *testing.T) {
	t.Parallel()
	whole := map[string]any{"priority": 0, "name": "limit", "virtual_host": "shop", "pattern": ".*", "apply_to": "queues", "definition": map[string]any{"max-length": 5}}
	changed := func(key string, v any) map[string]any {
		copy := map[string]any{}
		for k, old := range whole {
			copy[k] = old
		}
		if v == nil {
			delete(copy, key)
		} else {
			copy[key] = v
		}
		return copy
	}
	absent := func(key string, v any) map[string]any {
		return map[string]any{"name": "limit", "virtual_host": "shop", "state": "absent", key: v}
	}
	for reason, inputs := range map[string]map[string]any{
		"rabbitmq_policy.name: must not be empty": changed("name", ""),
		"rabbitmq_policy.virtual_host: must not start with a hyphen (-), which RabbitMQ's programs read as an option":                       changed("virtual_host", "-p"),
		"rabbitmq_policy.state: absent accepts only name and virtual_host; remove pattern":                                                  absent("pattern", ".*"),
		"rabbitmq_policy.state: absent accepts only name and virtual_host; remove priority":                                                 absent("priority", 1),
		"rabbitmq_policy.pattern: is required for state: present":                                                                           changed("pattern", nil),
		"rabbitmq_policy.apply_to: is required for state: present":                                                                          changed("apply_to", nil),
		"rabbitmq_policy.definition: is required for state: present":                                                                        changed("definition", nil),
		"rabbitmq_policy.definition: must be a mapping of RabbitMQ's policy keys to their values, such as max-length: 100000":               changed("definition", "max-length=5"),
		"rabbitmq_policy.definition: must hold at least one policy key, such as max-length: 100000; RabbitMQ accepts no policy without one": changed("definition", map[string]any{}),
	} {
		expectInvalidInputs(t, "rabbitmq_policy", inputs, reason)
	}
}

func TestRabbitmqPolicy(t *testing.T) {
	t.Parallel()
	const (
		listPolicies = "rabbitmqctl -q list_policies -p shop --formatter json"
		// RabbitMQ gives the definition as a text that holds JSON, with its
		// keys in alphabetical order; 4.3.6 prints the row's keys in another
		// order than 3.9.27.
		policyOf3927 = "[\n{\"vhost\":\"shop\",\"name\":\"limit-queue-length\",\"pattern\":\"^mmx\\\\.\",\"apply-to\":\"queues\",\"definition\":\"{\\\"max-length\\\":100000,\\\"message-ttl\\\":60000,\\\"overflow\\\":\\\"reject-publish\\\"}\",\"priority\":7}\n]\n"
		policyOf436  = "[\n{\"name\":\"limit-queue-length\",\"priority\":7,\"pattern\":\"^mmx\\\\.\",\"vhost\":\"shop\",\"apply-to\":\"queues\",\"definition\":\"{\\\"max-length\\\":100000,\\\"message-ttl\\\":60000,\\\"overflow\\\":\\\"reject-publish\\\"}\"}\n]\n"
	)
	// The numbers are float64 values, as a node gets them from the controller
	// process; the JSON for RabbitMQ holds them without a fraction.
	policy := map[string]any{"name": "limit-queue-length", "virtual_host": "shop", "pattern": "^mmx\\.", "apply_to": "queues", "priority": float64(7),
		"definition": map[string]any{"overflow": "reject-publish", "message-ttl": float64(60000), "max-length": float64(100000)}}
	absent := map[string]any{"name": "limit-queue-length", "virtual_host": "shop", "state": "absent"}
	read := []string{checkRunning, asRabbitmq + "-q list_vhosts --formatter json", asRabbitmq + "-q list_policies -p shop --formatter json"}
	const set = `-q set_policy -p shop --priority 7 --apply-to queues -- limit-queue-length ^mmx\. {"max-length":100000,"message-ttl":60000,"overflow":"reject-publish"}`

	// No policy yet.
	node := newRabbitmqNode(t, map[string]toolAnswer{checkRunning: theServerRuns, listVirtualHosts: {stdout: virtualHostsWithShop}, listPolicies: {stdout: "[]\n"},
		"rabbitmqctl " + set: {}}, nil)
	expectResult(t, node.run("rabbitmq_policy", policy, true), "succeeded", "would_change", "would create the policy limit-queue-length of the virtual host shop")
	expectCalls(t, node, read...)
	expectResult(t, node.run("rabbitmq_policy", policy, false), "succeeded", "changed", "created the policy limit-queue-length of the virtual host shop")
	expectCalls(t, node, append(slices.Clone(read), asRabbitmq+set)...)
	for _, dryrun := range []bool{true, false} {
		expectResult(t, node.run("rabbitmq_policy", absent, dryrun), "succeeded", "unchanged", "")
		expectCalls(t, node, read...)
	}

	// The policy as the task writes it, on the oldest and the newest RabbitMQ.
	for _, stored := range []string{policyOf3927, policyOf436} {
		node = newRabbitmqNode(t, map[string]toolAnswer{checkRunning: theServerRuns, listVirtualHosts: {stdout: virtualHostsWithShop}, listPolicies: {stdout: stored},
			"rabbitmqctl -q clear_policy -p shop limit-queue-length": {},
			"rabbitmqctl -q set_policy -p shop --priority -3 --apply-to all -- limit-queue-length -x.* {\"max-length\":100000,\"message-ttl\":60001,\"overflow\":\"reject-publish\"}": {}}, nil)
		for _, dryrun := range []bool{true, false} {
			expectResult(t, node.run("rabbitmq_policy", policy, dryrun), "succeeded", "unchanged", "")
			expectCalls(t, node, read...)
		}
		// Every part differs. A pattern and a priority may start with a hyphen.
		other := map[string]any{"name": "limit-queue-length", "virtual_host": "shop", "pattern": "-x.*", "apply_to": "all", "priority": -3,
			"definition": map[string]any{"overflow": "reject-publish", "message-ttl": 60001, "max-length": 100000}}
		const differs = "the policy limit-queue-length of the virtual host shop: pattern, apply_to, definition, priority"
		expectResult(t, node.run("rabbitmq_policy", other, true), "succeeded", "would_change", "would change "+differs)
		expectCalls(t, node, read...)
		expectResult(t, node.run("rabbitmq_policy", other, false), "succeeded", "changed", "changed "+differs)
		expectCalls(t, node, append(slices.Clone(read), asRabbitmq+"-q set_policy -p shop --priority -3 --apply-to all -- limit-queue-length -x.* {\"max-length\":100000,\"message-ttl\":60001,\"overflow\":\"reject-publish\"}")...)
		// An explicit zero changes the policy priority.
		zeroPriority := map[string]any{"priority": 0, "name": "limit-queue-length", "virtual_host": "shop", "pattern": "^mmx\\.", "apply_to": "queues", "definition": policy["definition"]}
		expectResult(t, node.run("rabbitmq_policy", zeroPriority, true), "succeeded", "would_change", "would change the policy limit-queue-length of the virtual host shop: priority")
		node.calls()
		expectResult(t, node.run("rabbitmq_policy", absent, true), "succeeded", "would_change", "would remove the policy limit-queue-length of the virtual host shop")
		expectCalls(t, node, read...)
		expectResult(t, node.run("rabbitmq_policy", absent, false), "succeeded", "changed", "removed the policy limit-queue-length of the virtual host shop")
		expectCalls(t, node, append(slices.Clone(read), asRabbitmq+"-q clear_policy -p shop limit-queue-length")...)
	}

	// A definition with a list and a text that needs escaping in JSON.
	mirrored := map[string]any{"priority": 0, "name": "mirror", "virtual_host": "/", "pattern": ".*", "apply_to": "all",
		"definition": map[string]any{"ha-mode": "nodes", "ha-params": []any{"rabbit@a", "rabbit@b"}, "dead-letter-exchange": "mmx a\\b \"q\" $HOME `id` ü"}}
	stored := "[\n{\"vhost\":\"/\",\"name\":\"mirror\",\"pattern\":\".*\",\"apply-to\":\"all\",\"definition\":\"{\\\"dead-letter-exchange\\\":\\\"mmx a\\\\\\\\b \\\\\\\"q\\\\\\\" $HOME `id` ü\\\",\\\"ha-mode\\\":\\\"nodes\\\",\\\"ha-params\\\":[\\\"rabbit@a\\\",\\\"rabbit@b\\\"]}\",\"priority\":0}\n]\n"
	node = newRabbitmqNode(t, map[string]toolAnswer{checkRunning: theServerRuns, listVirtualHosts: {stdout: virtualHostsWithShop},
		"rabbitmqctl -q list_policies -p / --formatter json": {stdout: stored}}, nil)
	expectResult(t, node.run("rabbitmq_policy", mirrored, false), "succeeded", "unchanged", "")
}

func TestRabbitmqPolicyFailures(t *testing.T) {
	t.Parallel()
	const listPolicies = "rabbitmqctl -q list_policies -p shop --formatter json"
	policy := map[string]any{"priority": 0, "name": "limit", "virtual_host": "shop", "pattern": ".*", "apply_to": "classic_queues", "definition": map[string]any{"made-up-key": 1}}
	absent := map[string]any{"name": "limit", "virtual_host": "shop", "state": "absent"}

	// The virtual host does not exist: an earlier node task may create it, and
	// a policy that must be absent is absent.
	node := newRabbitmqNode(t, map[string]toolAnswer{checkRunning: theServerRuns, listVirtualHosts: {stdout: onlyTheDefaultVirtualHost}}, nil)
	const noVirtualHost = "rabbitmq_policy: the virtual host shop does not exist; create it with a rabbitmq_virtual_host task in an earlier node task"
	expectResult(t, node.run("rabbitmq_policy", policy, true), "not_checked", "unknown", noVirtualHost)
	r := node.run("rabbitmq_policy", policy, false)
	expectResult(t, r, "failed", "unchanged", noVirtualHost)
	if r.FailureKind != "missing_prerequisite" {
		t.Errorf("failure kind %q", r.FailureKind)
	}
	expectResult(t, node.run("rabbitmq_policy", absent, false), "succeeded", "unchanged", "")
	expectCalls(t, node, checkRunning, asRabbitmq+"-q list_vhosts --formatter json", checkRunning, asRabbitmq+"-q list_vhosts --formatter json",
		checkRunning, asRabbitmq+"-q list_vhosts --formatter json")

	// RabbitMQ refuses the policy: 3.9.27 the value of apply_to, 4.3.6 the
	// key of the definition. A dryrun cannot know either. Another try would
	// get the same answer, so the failure is one of the task's inputs.
	const set = `rabbitmqctl -q set_policy -p shop --priority 0 --apply-to classic_queues -- limit .* {"made-up-key":1}`
	for stderr, message := range map[string]string{
		"Error:\nValidation failed\n\napply-to 'classic_queues' unrecognised; should be 'queues', 'exchanges' or 'all'\n\n": "Validation failed: apply-to 'classic_queues' unrecognised; should be 'queues', 'exchanges' or 'all'",
		"Error:\nValidation failed\n\n[{<<\"made-up-key\">>,1}] are not recognised policy settings\n\n":                     "Validation failed: [{<<\"made-up-key\">>,1}] are not recognised policy settings",
	} {
		node = newRabbitmqNode(t, map[string]toolAnswer{checkRunning: theServerRuns, listVirtualHosts: {stdout: virtualHostsWithShop}, listPolicies: {stdout: "[]\n"},
			set: {stderr: stderr, exitCode: 69}}, nil)
		expectResult(t, node.run("rabbitmq_policy", policy, true), "succeeded", "would_change", "would create the policy limit of the virtual host shop")
		r := node.run("rabbitmq_policy", policy, false)
		expectResult(t, r, "failed", "changed", "rabbitmq_policy: RabbitMQ refuses the policy limit of the virtual host shop: "+message)
		if r.FailureKind != "config" {
			t.Errorf("failure kind %q", r.FailureKind)
		}
	}
	// A policy that cannot be set for a reason that the script does not know
	// is an ordinary failure, which MMX tries again. The next try reads the
	// server again, finds no policy, and repeats the command that failed.
	node = newRabbitmqNode(t, map[string]toolAnswer{checkRunning: theServerRuns, listVirtualHosts: {stdout: virtualHostsWithShop}, listPolicies: {stdout: "[]\n"},
		set: {stderr: rabbitmqServerStopped, exitCode: 69}}, nil)
	expectFailedAndTriedAgain(t, node.run("rabbitmq_policy", policy, false), "changed",
		"rabbitmq_policy: setting the policy limit of the virtual host shop: "+rabbitmqServerStoppedFirstLine)
	node = newRabbitmqNode(t, map[string]toolAnswer{checkRunning: theServerRuns, listVirtualHosts: {stdout: virtualHostsWithShop}, listPolicies: {stdout: "[]\n"}, set: {}}, nil)
	expectResult(t, node.run("rabbitmq_policy", policy, false), "succeeded", "changed", "created the policy limit of the virtual host shop")
	expectCalls(t, node, checkRunning, asRabbitmq+"-q list_vhosts --formatter json", asRabbitmq+"-q list_policies -p shop --formatter json",
		asRabbitmq+strings.TrimPrefix(set, "rabbitmqctl "))

	// A read that fails.
	node = newRabbitmqNode(t, map[string]toolAnswer{checkRunning: theServerRuns, listVirtualHosts: {stdout: virtualHostsWithShop},
		listPolicies: {stderr: "Virtual host 'shop' does not exist\n", exitCode: 65}}, nil)
	expectResult(t, node.run("rabbitmq_policy", absent, false), "failed", "unchanged",
		"rabbitmq_policy: reading the policies of the virtual host shop: Virtual host 'shop' does not exist")
}

// pluginLists returns what "rabbitmq-plugins -q list -v --formatter json"
// printed on a test node of this RabbitMQ version, by the state of the node:
// whether the server ran, and which plugins its file of enabled plugins
// named. The lists hold six of the node's plugins.
func pluginLists(t *testing.T, version string) map[string]toolAnswer {
	t.Helper()
	skipOutsideTheRepository(t)
	b, err := os.ReadFile(filepath.Join("testdata", "rabbitmq", "plugin_lists_of_rabbitmq_"+version+".tsv"))
	if err != nil {
		t.Fatal(err)
	}
	lists := map[string]toolAnswer{}
	for _, line := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		state, text, _ := strings.Cut(line, "\t")
		lists[state] = toolAnswer{stdout: text + "\n"}
	}
	return lists
}

func TestRabbitmqPluginRejectsInputs(t *testing.T) {
	t.Parallel()
	expectInvalidInputs(t, "rabbitmq_plugin", map[string]any{"name": "", "state": "enabled"}, "rabbitmq_plugin.name: must not be empty")
	expectInvalidInputs(t, "rabbitmq_plugin", map[string]any{"name": "--offline", "state": "disabled_with_plugins_that_depend_on_it"},
		"rabbitmq_plugin.name: must not start with a hyphen (-), which RabbitMQ's programs read as an option")
}

func TestRabbitmqPlugin(t *testing.T) {
	t.Parallel()
	const stopped = "; the RabbitMQ server on this node does not run, so the change holds from its next start"
	plugin := func(name, state string) map[string]any { return map[string]any{"name": name, "state": state} }
	for _, version := range []string{"3.9.27", "4.3.6"} {
		lists := pluginLists(t, version)
		for name, c := range map[string]struct {
			before, after string
			inputs        map[string]any
			command       string
			reason        string
			dryrunReason  string
		}{
			"a plugin that is off": {"running_none", "running_management", plugin("rabbitmq_management", "enabled"),
				"enable rabbitmq_management", "enabled the plugin rabbitmq_management", "would enable the plugin rabbitmq_management"},
			"a plugin that is off, server stopped": {"stopped_none", "stopped_management", plugin("rabbitmq_management", "enabled"),
				"enable rabbitmq_management", "enabled the plugin rabbitmq_management" + stopped, "would enable the plugin rabbitmq_management" + stopped},
			// "enable" changes nothing for such a plugin, so the script
			// writes the whole list with "set".
			"a plugin that is on because another needs it": {"running_management", "running_management_and_agent", plugin("rabbitmq_management_agent", "enabled"),
				"set rabbitmq_management rabbitmq_management_agent",
				"enabled the plugin rabbitmq_management_agent by name, which was on only because another plugin needs it",
				"would enable the plugin rabbitmq_management_agent by name, which was on only because another plugin needs it"},
			"disable a plugin that others need": {"running_management_agent_shovel_management", "running_none", plugin("rabbitmq_management_agent", "disabled_with_plugins_that_depend_on_it"),
				"disable rabbitmq_management_agent",
				"disabled the plugin rabbitmq_management_agent; RabbitMQ also disabled the plugins that need it: rabbitmq_management, rabbitmq_shovel_management",
				"would disable the plugin rabbitmq_management_agent; RabbitMQ would also disable the plugins that need it: rabbitmq_management, rabbitmq_shovel_management"},
			"disable a plugin that is on because another needs it": {"running_management", "running_none", plugin("rabbitmq_web_dispatch", "disabled_with_plugins_that_depend_on_it"),
				"disable rabbitmq_web_dispatch",
				"disabled the plugin rabbitmq_web_dispatch; RabbitMQ also disabled the plugins that need it: rabbitmq_management",
				"would disable the plugin rabbitmq_web_dispatch; RabbitMQ would also disable the plugins that need it: rabbitmq_management"},
			"disable a plugin that nothing needs": {"running_management_and_agent", "running_agent", plugin("rabbitmq_management", "disabled_with_plugins_that_depend_on_it"),
				"disable rabbitmq_management", "disabled the plugin rabbitmq_management", "would disable the plugin rabbitmq_management"},
			"disable with the server stopped": {"stopped_management", "stopped_none", plugin("rabbitmq_management", "disabled_with_plugins_that_depend_on_it"),
				"disable rabbitmq_management", "disabled the plugin rabbitmq_management" + stopped, "would disable the plugin rabbitmq_management" + stopped},
		} {
			node := newRabbitmqNode(t, map[string]toolAnswer{listPlugins: lists[c.before], "rabbitmq-plugins -q " + c.command: {stdout: "Applying plugin configuration to rabbit@node1...\n"}},
				map[string]toolAnswer{listPlugins: lists[c.after]})
			expectResult(t, node.run("rabbitmq_plugin", c.inputs, true), "succeeded", "would_change", c.dryrunReason)
			expectCalls(t, node, listPlugins)
			expectResult(t, node.run("rabbitmq_plugin", c.inputs, false), "succeeded", "changed", c.reason)
			expectCalls(t, node, listPlugins, "rabbitmq-plugins -q "+c.command, listPlugins)
			if t.Failed() {
				t.Fatalf("RabbitMQ %s: %s", version, name)
			}
		}

		// Nothing to do.
		for state, inputs := range map[string]map[string]any{
			"running_management":           plugin("rabbitmq_management", "enabled"),
			"stopped_management":           plugin("rabbitmq_management", "enabled"),
			"running_management_and_agent": plugin("rabbitmq_management_agent", "enabled"),
			"running_none":                 plugin("rabbitmq_management", "disabled_with_plugins_that_depend_on_it"),
			"running_agent":                plugin("rabbitmq_mqtt", "disabled_with_plugins_that_depend_on_it"),
		} {
			node := newRabbitmqNode(t, map[string]toolAnswer{listPlugins: lists[state]}, nil)
			for _, dryrun := range []bool{true, false} {
				expectResult(t, node.run("rabbitmq_plugin", inputs, dryrun), "succeeded", "unchanged", "")
				expectCalls(t, node, listPlugins)
			}
		}

		// A name that RabbitMQ does not have. RabbitMQ 4.3.6 would write any
		// name into its file of enabled plugins, so the script runs no command.
		node := newRabbitmqNode(t, map[string]toolAnswer{listPlugins: lists["running_none"]}, nil)
		const unknown = "rabbitmq_plugin: RabbitMQ on this node has no plugin mmx_made_up_plugin; the command rabbitmq-plugins list shows the names. " +
			"A plugin that is not part of RabbitMQ needs its file in RabbitMQ's plugins directory, from an earlier node task"
		for _, state := range []string{"enabled", "disabled_with_plugins_that_depend_on_it"} {
			expectResult(t, node.run("rabbitmq_plugin", plugin("mmx_made_up_plugin", state), true), "not_checked", "unknown", unknown)
			r := node.run("rabbitmq_plugin", plugin("mmx_made_up_plugin", state), false)
			expectResult(t, r, "failed", "unchanged", unknown)
			if r.FailureKind != "missing_prerequisite" {
				t.Errorf("failure kind %q", r.FailureKind)
			}
			expectCalls(t, node, listPlugins, listPlugins)
		}
	}
}

func TestRabbitmqPluginFailures(t *testing.T) {
	t.Parallel()
	lists := pluginLists(t, "4.3.6")
	management := map[string]any{"name": "rabbitmq_management", "state": "enabled"}

	disabled := map[string]any{"name": "rabbitmq_management", "state": "disabled_with_plugins_that_depend_on_it"}

	// The command reports no error and the plugin is as before. MMX's next
	// try reads the plugins again, finds the plugin as it was, and repeats
	// the command: an ordinary failure.
	node := newRabbitmqNode(t, map[string]toolAnswer{listPlugins: lists["running_none"], "rabbitmq-plugins -q enable rabbitmq_management": {}}, nil)
	for try := 1; try <= 2; try++ {
		expectFailedAndTriedAgain(t, node.run("rabbitmq_plugin", management, false), "changed",
			"rabbitmq_plugin: enabling the plugin rabbitmq_management: rabbitmq-plugins reported no error, and the plugin is still not enabled by name")
		expectCalls(t, node, listPlugins, "rabbitmq-plugins -q enable rabbitmq_management", listPlugins)
	}
	node = newRabbitmqNode(t, map[string]toolAnswer{listPlugins: lists["running_management"], "rabbitmq-plugins -q disable rabbitmq_management": {}}, nil)
	for try := 1; try <= 2; try++ {
		expectFailedAndTriedAgain(t, node.run("rabbitmq_plugin", disabled, false), "changed",
			"rabbitmq_plugin: disabling the plugin rabbitmq_management: rabbitmq-plugins reported no error, and the plugin is still enabled")
		expectCalls(t, node, listPlugins, "rabbitmq-plugins -q disable rabbitmq_management", listPlugins)
	}

	// The command worked, and the read after it fails. MMX's next try would
	// find the plugin as the task wants it and report an unchanged task, and
	// nobody would read that a step failed. So the script asks for no other
	// try, and its reason says what was done and what failed.
	for _, c := range []struct {
		before, after string
		inputs        map[string]any
		command, done string
	}{
		{"running_none", "running_management", management, "enable rabbitmq_management", "enabled the plugin rabbitmq_management"},
		{"running_management", "running_none", disabled, "disable rabbitmq_management", "disabled the plugin rabbitmq_management"},
	} {
		node = newRabbitmqNode(t, map[string]toolAnswer{listPlugins: lists[c.before], "rabbitmq-plugins -q " + c.command: {}},
			map[string]toolAnswer{listPlugins: {exitCode: 69}})
		r := node.run("rabbitmq_plugin", c.inputs, false)
		expectResult(t, r, "failed", "changed", "rabbitmq_plugin: "+c.done+", and then reading the plugins failed: the command ended with exit code 69 and printed no message")
		if r.FailureKind != "" || !r.NoOtherTry {
			t.Errorf("%s: failure kind %q, no other try %v; a failure that another try would hide has try_again: False", c.command, r.FailureKind, r.NoOtherTry)
		}
		expectCalls(t, node, listPlugins, "rabbitmq-plugins -q "+c.command, listPlugins)
		// What the next try would report on the node as the first try left it.
		node = newRabbitmqNode(t, map[string]toolAnswer{listPlugins: lists[c.after]}, nil)
		expectResult(t, node.run("rabbitmq_plugin", c.inputs, false), "succeeded", "unchanged", "")
	}

	// The command fails with RabbitMQ's message: what RabbitMQ 3.9.27 and
	// 4.0.5 print for a plugin whose file is gone.
	node = newRabbitmqNode(t, map[string]toolAnswer{listPlugins: lists["running_none"],
		"rabbitmq-plugins -q enable rabbitmq_management": {stderr: "Error:\n{:plugins_not_found, [:rabbitmq_management]}\n", exitCode: 70}}, nil)
	expectResult(t, node.run("rabbitmq_plugin", management, false), "failed", "changed",
		"rabbitmq_plugin: enabling the plugin rabbitmq_management: {:plugins_not_found, [:rabbitmq_management]}")

	// A read that fails, one that prints no JSON, and a word that the script
	// does not know.
	for stdout, reason := range map[string]string{
		"": "rabbitmq_plugin: reading the plugins: the command ended with exit code 69 and printed no message",
		"Listing plugins with pattern \".*\" ...\n": "rabbitmq_plugin: reading the plugins: rabbitmq-plugins printed no JSON object with a list of plugins: Listing plugins with pattern \".*\" ...\n",
		"{\"status\":\"running\",\"format\":\"verbose\",\"plugins\":[{\"enabled\":\"half_enabled\",\"name\":\"rabbitmq_management\"}]}\n": "rabbitmq_plugin: reading the plugins: rabbitmq-plugins says about the plugin rabbitmq_management that it is half_enabled, which this module does not know",
	} {
		exitCode := 0
		if stdout == "" {
			exitCode = 69
		}
		node = newRabbitmqNode(t, map[string]toolAnswer{listPlugins: {stdout: stdout, exitCode: exitCode}}, nil)
		for _, dryrun := range []bool{true, false} {
			expectResult(t, node.run("rabbitmq_plugin", management, dryrun), "failed", "unchanged", reason)
		}
	}
}
