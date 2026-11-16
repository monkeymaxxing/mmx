package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"

	"mmx/internal/access"
	"mmx/internal/config"
	"mmx/internal/engine"
	"mmx/internal/modules"
	"mmx/internal/protocol"
	"mmx/internal/value"
)

// rebootNodes is a run of one fleet stage on the nodes a and b, both reached
// over verified SSH with an installed node runner executable. The stand-in
// for ssh writes each runner command to <node>.trace in dir and what the
// command received to <node>.<command>.input. It answers a reboot-refusal
// request with the file <node>.reboot-refusal, or fails with the file's
// text when the file is named <node>.reboot-refusal.error, and admits a
// submitted job as succeeded.
type rebootNodes struct {
	c   Controller
	run *Run
	dir string
}

var testControllerMachine = protocol.ControllerMachine{BootID: "boot-of-the-controller-machine", MachineIDHash: "hash-of-the-controller-machine"}

func newRebootNodes(t *testing.T, dryrun bool, nodeTasks ...config.Task) *rebootNodes {
	t.Helper()
	binary := []byte("runner")
	sum := sha256.Sum256(binary)
	f := &rebootNodes{dir: t.TempDir()}
	bin := fakeSSH(t, `case "$1" in -G) printf 'hostname fixture\nuser root\nport 22\n'; exit 0;; -F) exit 0;; esac
node=; command=
for argument; do node=$command; command=$argument; done
dir=`+quote(f.dir)+`
case "$command" in
*machine-id*)
  # Each node boots its own kernel unless the file same-boot says that both
  # names reach one machine in one boot.
  boot=$node; test -e "$dir/same-boot" && boot=a
  case "$boot" in a) boot=aaaaaaaa-0000-4000-8000-000000000000;; *) boot=bbbbbbbb-0000-4000-8000-000000000000;; esac
  printf 'Linux\nx86_64\n0\nroot\n%s\nsystemd\n0123456789abcdef0123456789abcdef\n\n10.0.0.1 5000 10.0.0.2 22\n' "$boot"; exit 0;;
*sha256sum*) echo `+hex.EncodeToString(sum[:])+`; exit 0;;
esac
name=${command##*mmx-runner\'\"\'\"\' }
name=${name%%[ \']*}
echo "$name" >> "$dir/$node.trace"
cat > "$dir/$node.$name.input"
case "$name" in
reboot-refusal)
  if [ -e "$dir/$node.reboot-refusal.error" ]; then cat "$dir/$node.reboot-refusal.error" >&2; exit 1; fi
  cat "$dir/$node.reboot-refusal";;
submit) cat "$dir/$node.submit";;
*) echo "unexpected runner command $name" >&2; exit 1;;
esac
`)
	if err := os.WriteFile(filepath.Join(bin, "mmx-runner-linux-amd64"), binary, 0700); err != nil {
		t.Fatal(err)
	}
	f.c = Controller{State: t.TempDir(), RunnerDir: bin, controllerMachine: func() (protocol.ControllerMachine, error) { return testControllerMachine, nil }}
	nodes := map[string]config.Node{"a": {Address: "a", Transport: "ssh_only"}, "b": {Address: "b", Transport: "ssh_only"}}
	project := config.Project{Project: "fleet", MaxNodesAtOnce: 2, FleetStages: []config.FleetStage{{ID: "patch", Nodes: "all"}}}
	var err error
	if f.run, err = f.c.New(context.Background(), project, config.Inventory{Nodes: nodes}, nil, dryrun); err != nil {
		t.Fatal(err)
	}
	f.run.Project.FleetStages[0].NodeTasks = nodeTasks
	for _, node := range []string{"a", "b"} {
		f.answer(t, node, "")
		f.admit(t, node, 1)
	}
	return f
}

// answer sets what the node answers to the reboot-refusal request.
func (f *rebootNodes) answer(t *testing.T, node, refusal string) {
	t.Helper()
	data, err := json.Marshal(protocol.RebootRefusalResponse{Refusal: refusal})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.dir, node+".reboot-refusal"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

// admit makes the node answer that the job of this attempt succeeded.
func (f *rebootNodes) admit(t *testing.T, node string, attempt int) {
	t.Helper()
	id := sha256.Sum256([]byte(fmt.Sprintf("%s/patch/%s/%d", f.run.ID, node, attempt)))
	data, err := json.Marshal(protocol.Response{ID: hex.EncodeToString(id[:]), Status: "succeeded", Change: "unchanged"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.dir, node+".submit"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

// commands names the runner commands that the node received, in order.
func (f *rebootNodes) commands(t *testing.T, node string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.dir, node+".trace"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(data))
}

func (f *rebootNodes) received(t *testing.T, node, command string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.dir, node+"."+command+".input"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

var (
	patchTask  = config.Task{ID: "patch", Module: "shell", ModuleInputs: &modules.Shell{Script: "true"}}
	rebootTask = config.Task{ID: "restart", Module: "reboot", ModuleInputs: &modules.Reboot{}}
)

// Two inventory names that reach one machine in one boot, such as after a
// typo in an address, fail: one name's node job would run on the server the
// other name means, and the intended server would never be touched. The
// first name keeps the machine; the other fails before anything reaches the
// node, and the failure names both and says what to check. Two names with
// the same address are a deliberate choice and both run. Virtual machines
// cloned from one image share a machine ID but not a boot ID, so they run as
// two nodes.
func TestTwoInventoryNamesThatReachOneMachineFail(t *testing.T) {
	clones := newRebootNodes(t, false, patchTask)
	execute(t, clones.c, clones.run, false)
	if clones.run.Status != "succeeded" {
		t.Fatalf("clones with one machine ID and their own boot IDs: %s %+v", clones.run.Status, clones.run.Entries)
	}

	f := newRebootNodes(t, false, patchTask)
	if err := os.WriteFile(filepath.Join(f.dir, "same-boot"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	execute(t, f.c, f.run, false)
	a, b := entry(t, f.run, "patch", "a"), entry(t, f.run, "patch", "b")
	kept, refused := a, b
	if a.Response.Status == "failed" {
		kept, refused = b, a
	}
	want := "inventory nodes " + kept.Node + " and " + refused.Node + " reach the same machine: both answered with the same systemd machine ID and the same boot ID. " +
		"MMX ran nothing for " + refused.Node + " on that machine, because the machine is " + kept.Node + "'s in this run. " +
		"Check the addresses of " + kept.Node + " and " + refused.Node + " in the inventory: one of them probably names the wrong server; two names for one server need the same address. " +
		"When an address is a Host alias in an SSH configuration file (~/.ssh/config, or the node's ssh_config), the wrong value may be that alias's HostName. " +
		"Some containers share the boot ID of their host, so each container needs its own /etc/machine-id"
	if f.run.Status != "failed" || kept.Response.Status != "succeeded" || refused.Response.Status != "failed" || refused.Response.Error != want {
		t.Fatalf("run %s\nkept %+v\nrefused %+v", f.run.Status, kept.Response, refused.Response)
	}
	if got := f.commands(t, refused.Node); got != nil {
		t.Fatalf("%s received %q", refused.Node, got)
	}

}

// Two names with one address are a choice the inventory shows, such as a load
// balancer on an application server: both keep the machine.
func TestTwoInventoryNamesWithOneAddressShareTheirMachine(t *testing.T) {
	r := &Run{Inventory: config.Inventory{Nodes: map[string]config.Node{
		"app02": {Address: "10.0.0.2"}, "loadbalancer": {Address: "10.0.0.2"}, "typo": {Address: "app02.example.com"}}}, machines: &sync.Map{}}
	conn := &Connection{NodeID: "machine", BootID: "boot"}
	for _, name := range []string{"app02", "loadbalancer"} {
		if err := r.recordConnection(name, conn); err != nil {
			t.Fatal(name, err)
		}
	}
	if err := r.recordConnection("typo", conn); err == nil || !strings.Contains(err.Error(), "inventory nodes app02 and typo reach the same machine") {
		t.Fatal(err)
	}
}

// Before the node job of a fleet stage that ends with a reboot task, the
// controller process asks the node whether it is the controller machine,
// with its own boot ID and the hash of its machine ID. A node that refuses
// fails with the node's message before anything of the job reaches it; the
// other node goes on, and its job carries the same two values for the
// reboot task's own check. It is the same in a dryrun and in a run.
func TestControllerMachineRefusesItsRebootBeforeTheNodeJobAndOtherNodesGoOn(t *testing.T) {
	for _, dryrun := range []bool{false, true} {
		name := map[bool]string{false: "run", true: "dryrun"}[dryrun]
		t.Run(name, func(t *testing.T) {
			f := newRebootNodes(t, dryrun, patchTask, rebootTask)
			f.answer(t, "a", engine.RebootOfControllerMachineRefused)
			execute(t, f.c, f.run, false)

			a := entry(t, f.run, "patch", "a")
			if a.Response.Status != "failed" || a.Response.Error != "MMX does not reboot the controller machine, the machine where this mmx command runs. Run the playbook from another machine, or reboot this machine by hand after MMX finishes." ||
				a.NodeJobAttempt != 1 || len(a.Response.Results) != 0 {
				t.Fatalf("%+v", a)
			}
			if got := f.commands(t, "a"); !reflect.DeepEqual(got, []string{"reboot-refusal"}) {
				t.Fatalf("the refusing node received %q", got)
			}
			var request protocol.RebootRefusalRequest
			if err := protocol.Decode(f.received(t, "a", "reboot-refusal"), &request); err != nil || request.ControllerMachine != testControllerMachine {
				t.Fatal(request, err)
			}

			b := entry(t, f.run, "patch", "b")
			if b.Response.Status != "succeeded" || f.run.Status != "failed" {
				t.Fatalf("%s: %+v", f.run.Status, b)
			}
			if got := f.commands(t, "b"); !reflect.DeepEqual(got, []string{"reboot-refusal", "submit"}) {
				t.Fatalf("the other node received %q", got)
			}
			var job config.Job
			if err := protocol.Decode(f.received(t, "b", "submit"), &job); err != nil || job.ControllerMachine == nil || *job.ControllerMachine != testControllerMachine || job.Dryrun != dryrun {
				t.Fatalf("%+v %v", job.ControllerMachine, err)
			}
		})
	}
}

// The refusal is not tried again within the run, and mmx resume meets it
// again: the node is asked once more and refuses once more, and the node
// that succeeded is not contacted.
func TestResumeMeetsTheRebootRefusalAgain(t *testing.T) {
	f := newRebootNodes(t, false, patchTask, rebootTask)
	f.answer(t, "a", engine.RebootOfControllerMachineRefused)
	execute(t, f.c, f.run, false)
	first := entry(t, f.run, "patch", "a")
	execute(t, f.c, f.run, true)
	a := entry(t, f.run, "patch", "a")
	if a.Response.Status != "failed" || a.Response.Error != engine.RebootOfControllerMachineRefused || a.NodeJobAttempt != 2 ||
		!reflect.DeepEqual(a.PreviousNodeJobAttempts, []Attempt{{JobID: first.JobID, Response: first.Response}}) {
		t.Fatalf("%+v", a)
	}
	if got := f.commands(t, "a"); !reflect.DeepEqual(got, []string{"reboot-refusal", "reboot-refusal"}) {
		t.Fatalf("the refusing node received %q", got)
	}
	if got := f.commands(t, "b"); !reflect.DeepEqual(got, []string{"reboot-refusal", "submit"}) {
		t.Fatalf("the node that had succeeded received %q", got)
	}
	// From another machine, the same run goes through.
	f.answer(t, "a", "")
	f.admit(t, "a", 3)
	execute(t, f.c, f.run, true)
	if a := entry(t, f.run, "patch", "a"); a.Response.Status != "succeeded" || f.run.Status != "succeeded" {
		t.Fatalf("%s: %+v", f.run.Status, a)
	}
}

// A fleet stage that does not end with a reboot task asks no node, and its
// node jobs carry no controller machine.
func TestOnlyAFleetStageThatEndsWithARebootAsksTheNode(t *testing.T) {
	f := newRebootNodes(t, false, patchTask)
	execute(t, f.c, f.run, false)
	for _, node := range []string{"a", "b"} {
		if got := f.commands(t, node); !reflect.DeepEqual(got, []string{"submit"}) {
			t.Fatalf("%s received %q", node, got)
		}
		if strings.Contains(string(f.received(t, node, "submit")), "controller_machine") {
			t.Fatalf("the node job of %s carries a controller machine", node)
		}
	}
	if f.run.Status != "succeeded" {
		t.Fatal(f.run.Status)
	}
}

// A node that cannot answer the question gets no node job: without the
// answer the node could be the controller machine. The job was never sent,
// so the node job is failed, not unconfirmed.
func TestNodeThatCannotAnswerTheRebootQuestionGetsNoNodeJob(t *testing.T) {
	f := newRebootNodes(t, false, patchTask, rebootTask)
	if err := os.WriteFile(filepath.Join(f.dir, "a.reboot-refusal.error"), []byte("finding out whether this node is a container: systemd-detect-virt --container: the command printed nothing\n"), 0600); err != nil {
		t.Fatal(err)
	}
	execute(t, f.c, f.run, false)
	a := entry(t, f.run, "patch", "a")
	if a.Response.Status != "failed" || a.Response.Error != "could not make sure that this node is not the machine that runs the controller process, which MMX never reboots: "+
		"SSH a: exit status 1: finding out whether this node is a container: systemd-detect-virt --container: the command printed nothing" {
		t.Fatalf("%+v", a.Response)
	}
	if got := f.commands(t, "a"); !reflect.DeepEqual(got, []string{"reboot-refusal"}) {
		t.Fatalf("the node received %q", got)
	}
	if b := entry(t, f.run, "patch", "b"); b.Response.Status != "succeeded" {
		t.Fatalf("%+v", b.Response)
	}
}

// On Linux the controller process knows its machine's boot ID, without
// which no node would ever recognize the controller machine. A machine
// that does not run Linux, such as a Mac, has none and sends none.
func TestThisControllerMachineHasABootIDOnLinux(t *testing.T) {
	machine, err := thisControllerMachine()
	if linux := runtime.GOOS == "linux"; err != nil || linux != (machine.BootID != "") {
		t.Fatalf("%s: %+v %v", runtime.GOOS, machine, err)
	}
	if machine.MachineIDHash != "" && len(machine.MachineIDHash) != 64 {
		t.Fatalf("%+v", machine)
	}
}

// A Linux controller machine that cannot read its boot ID does not send an
// empty one, which would switch the guard off without a word: the read is
// an error that names what could not be read. A machine that does not run
// Linux has no boot ID, and sends none. A machine ID that cannot be read
// stays empty on every system, because the boot ID alone can decide.
func TestControllerMachineWithoutABootID(t *testing.T) {
	unreadable := func() (string, error) {
		return "", errors.New("open /proc/sys/kernel/random/boot_id: permission denied")
	}
	bootID := func() (string, error) { return "boot-1", nil }
	machineIDHash := func() (string, error) { return "hash-1", nil }
	noMachineID := func() (string, error) { return "", os.ErrNotExist }

	machine, err := readControllerMachine("linux", unreadable, machineIDHash)
	if machine != (protocol.ControllerMachine{}) || err == nil ||
		err.Error() != "reading the boot ID of the controller machine: open /proc/sys/kernel/random/boot_id: permission denied" {
		t.Fatalf("Linux: %+v %v", machine, err)
	}
	machine, err = readControllerMachine("darwin", unreadable, noMachineID)
	if machine != (protocol.ControllerMachine{}) || err != nil {
		t.Fatalf("macOS: %+v %v", machine, err)
	}
	machine, err = readControllerMachine("linux", bootID, noMachineID)
	if machine != (protocol.ControllerMachine{BootID: "boot-1"}) || err != nil {
		t.Fatalf("Linux without a machine ID: %+v %v", machine, err)
	}
	machine, err = readControllerMachine("linux", bootID, machineIDHash)
	if machine != (protocol.ControllerMachine{BootID: "boot-1", MachineIDHash: "hash-1"}) || err != nil {
		t.Fatalf("Linux: %+v %v", machine, err)
	}
}

// When the controller process cannot say which machine it runs on, the node
// job of a fleet stage that ends with a reboot task fails with the reason,
// and no node is asked or sent anything. A fleet stage without a reboot
// task does not need the boot ID and runs.
func TestControllerMachineThatCannotReadItsBootIDStartsNoRebootingNodeJob(t *testing.T) {
	unreadable := func() (protocol.ControllerMachine, error) {
		return protocol.ControllerMachine{}, errors.New("reading the boot ID of the controller machine: open /proc/sys/kernel/random/boot_id: permission denied")
	}
	for _, dryrun := range []bool{false, true} {
		f := newRebootNodes(t, dryrun, patchTask, rebootTask)
		f.c.controllerMachine = unreadable
		execute(t, f.c, f.run, false)
		for _, node := range []string{"a", "b"} {
			en := entry(t, f.run, "patch", node)
			if en.Response.Status != "failed" || en.Response.Error != "could not make sure that this node is not the machine that runs the controller process, which MMX never reboots: "+
				"reading the boot ID of the controller machine: open /proc/sys/kernel/random/boot_id: permission denied" {
				t.Fatalf("dryrun %v, %s: %+v", dryrun, node, en.Response)
			}
			if got := f.commands(t, node); got != nil {
				t.Fatalf("dryrun %v: %s received %q", dryrun, node, got)
			}
		}
	}
	f := newRebootNodes(t, false, patchTask)
	f.c.controllerMachine = unreadable
	execute(t, f.c, f.run, false)
	if f.run.Status != "succeeded" || !reflect.DeepEqual(f.commands(t, "a"), []string{"submit"}) {
		t.Fatalf("a fleet stage without a reboot task: %s, %q", f.run.Status, f.commands(t, "a"))
	}
}

// firstAnswer is the failure of a node job whose node answers the first SSH
// command of a session with this text, over connection: ssh_only.
func firstAnswer(t *testing.T, answer string) string {
	t.Helper()
	return firstAnswerOver(t, "ssh_only", answer)
}

// firstAnswerOver is firstAnswer over the connection kind transport, where
// "" is the default connection, which sets up HTTPS through SSH.
func firstAnswerOver(t *testing.T, transport, answer string) string {
	t.Helper()
	dir := fakeSSH(t, `case "$1" in -G) printf 'hostname fixture\nuser dev\nport 22\n'; exit 0;; -F) exit 0;; esac
for command; do :; done
case "$command" in
*machine-id*) cat "$(dirname "$0")/first-answer";;
*) echo "unexpected command after a first answer that MMX refuses: $command" >&2; exit 1;;
esac
`)
	if err := os.WriteFile(filepath.Join(dir, "first-answer"), []byte(answer), 0600); err != nil {
		t.Fatal(err)
	}
	c, r := fleet(t, config.FleetStage{ID: "setup", Nodes: "a"})
	r.Inventory.Nodes["a"] = config.Node{Address: "a", Transport: transport}
	en := r.Entries[0]
	en.scope = value.Scope{}
	en, err := c.executeEntry(context.Background(), r, r.Project.FleetStages[0], en, false, func(Entry) error { return nil })
	if err != nil || en.Response.Status != "failed" || en.JobID != "" {
		t.Fatalf("%+v %v", en.Response, err)
	}
	return en.Response.Error
}

// A Linux node that systemd does not run, such as Alpine Linux with
// OpenRC, is refused with one failure that says so, over both connection
// kinds, before MMX installs or sets up anything: MMX manages only servers
// that run systemd. Such a node usually has no /etc/machine-id either, and
// the advice to run systemd-machine-id-setup would be wrong there; a node
// that systemd runs and that lacks the file still gets that advice.
func TestNodeThatSystemdDoesNotRunIsRefused(t *testing.T) {
	const notSystemd = "this node does not run systemd: the directory /run/systemd/system, which exists on every machine that systemd started, is not there. " +
		"MMX manages only Linux servers that run systemd. Remove this node from the inventory, or leave it out of the fleet stage with except_nodes"
	answer := func(systemd, machineIDFile string) string {
		return "Linux\nx86_64\n0\nroot\n0b0b0b0b-0000-4000-8000-000000000000\n" + systemd + "\n" + machineIDFile + "\n10.0.0.1 5000 10.0.0.2 22\n"
	}
	for _, transport := range []string{"", "ssh_only"} {
		for _, machineIDFile := range []string{"", "0123456789abcdef0123456789abcdef\n"} {
			if got := firstAnswerOver(t, transport, answer("no_systemd", machineIDFile)); got != notSystemd {
				t.Fatalf("connection %q, machine ID file %q: %s", transport, machineIDFile, got)
			}
		}
		if got := firstAnswerOver(t, transport, answer("systemd", "")); !strings.HasPrefix(got, "this node has no systemd machine ID: its file /etc/machine-id is missing or empty.") ||
			!strings.Contains(got, "Run systemd-machine-id-setup as root on the node") {
			t.Fatalf("connection %q: %s", transport, got)
		}
	}
}

// A machine that answers uname -s with one word other than Linux cannot be
// a node. The failure names the answer and says that MMX manages Linux
// nodes only. Only the answer Darwin says that the machine is a Mac, and
// only then the failure speaks of a macOS controller machine.
func TestNodeThatIsNotLinuxIsRefusedWithTheSystemThatAnswered(t *testing.T) {
	// A Mac has no boot ID and no /etc/machine-id, and systemd does not run
	// it, so its answer has two lines less and says no_systemd.
	if got := firstAnswer(t, "Darwin\narm64\n501\ndev\nno_systemd\n\n10.0.0.1 5000 10.0.0.2 22\n"); got != `this machine does not run Linux: it answered "Darwin" (macOS) to the command uname -s. `+
		"MMX manages Linux nodes only, and a macOS controller machine cannot be a node. Remove this node from the inventory, or leave it out of the fleet stage with except_nodes" {
		t.Fatal(got)
	}
	if got := firstAnswer(t, "FreeBSD\namd64\n0\nroot\nno_systemd\n\n10.0.0.1 5000 10.0.0.2 22\n"); got != `this machine does not run Linux: it answered "FreeBSD" to the command uname -s. `+
		"MMX manages Linux nodes only. Remove this node from the inventory, or leave it out of the fleet stage with except_nodes" {
		t.Fatal(got)
	}
}

// Text before the answer to uname -s, such as a login script prints, is not
// the name of another system: the failure quotes how the answer started and
// says what MMX expects there. A node that prints nothing is told so.
func TestTextBeforeTheFirstAnswerIsNotTakenForAnotherSystem(t *testing.T) {
	linux := "Linux\nx86_64\n0\nroot\n0b0b0b0b-0000-4000-8000-000000000000\nsystemd\n0123456789abcdef0123456789abcdef\n\n10.0.0.1 5000 10.0.0.2 22\n"
	if got := firstAnswer(t, "Welcome to web01\n"+linux); got != `the answer of this node to the command uname -s started with "Welcome to web01", and MMX expects the one word Linux there. `+
		"If a login script of the SSH account prints this text, make it print nothing when the session has no terminal" {
		t.Fatal(got)
	}
	long := strings.Repeat("0123456789", 9)
	if got := firstAnswer(t, "Last login: "+long+"\n"+linux); !strings.HasPrefix(got, `the answer of this node to the command uname -s started with "Last login: `+long[:68]+`" and more, and MMX expects`) {
		t.Fatal(got)
	}
	if got := firstAnswer(t, "\n\n"); got != "this node answered nothing to the command uname -s, and MMX expects the one word Linux there. The SSH account must run the commands that MMX sends" {
		t.Fatal(got)
	}
}

// A Linux node whose /etc/machine-id is missing, empty, or not a machine ID
// fails with the file, what MMX found in it, and the way to make a valid
// one. For a file that holds something else the way starts with removing
// the file: systemd-machine-id-setup of systemd 257 and newer leaves the
// word "uninitialized" in place.
func TestLinuxNodeWithoutAValidMachineIDIsToldWhatToDo(t *testing.T) {
	answer := func(machineIDFile string) string {
		return "Linux\nx86_64\n0\nroot\n0b0b0b0b-0000-4000-8000-000000000000\nsystemd\n" + machineIDFile + "\n10.0.0.1 5000 10.0.0.2 22\n"
	}
	const missingOrEmpty = "this node has no systemd machine ID: its file /etc/machine-id is missing or empty. MMX identifies a node by the systemd machine ID. " +
		"Run systemd-machine-id-setup as root on the node: it writes a new ID into the file. Then run MMX again"
	// cat prints nothing for a file that is not there and for an empty one.
	if got := firstAnswer(t, answer("")); got != missingOrEmpty {
		t.Fatal(got)
	}
	notValid := func(found string) string {
		return "this node's systemd machine ID is not valid: its file /etc/machine-id holds " + found + ", not 32 hexadecimal digits. MMX identifies a node by the systemd machine ID. " +
			"As root on the node, remove the file and run systemd-machine-id-setup: it creates the file with a new ID. Then run MMX again"
	}
	for file, found := range map[string]string{
		"uninitialized\n":                    `"uninitialized"`,
		"not a machine id\n":                 `"not a machine id"`,
		"0123456789abcdef0123456789abcde\n":  `"0123456789abcdef0123456789abcde"`,
		"01234567-89ab-cdef-0123-456789ab\n": `"01234567-89ab-cdef-0123-456789ab"`,
	} {
		if got := firstAnswer(t, answer(file)); got != notValid(found) {
			t.Fatalf("%q: %s", file, got)
		}
	}
	if got := firstAnswer(t, "Linux\nx86_64\n0\n"); got != "unexpected answer of this node to the first SSH command of MMX, which asks for the system, the architecture, "+
		`the user ID, the user name, the boot ID, whether systemd runs the node, the machine ID, and the SSH connection: "Linux x86_64 0"` {
		t.Fatal(got)
	}
	// Every Linux kernel has a boot ID; an answer without one is not taken
	// for a valid answer with its words in other places.
	if got := firstAnswer(t, "Linux\nx86_64\n0\nroot\n\nsystemd\n0123456789abcdef0123456789abcdef\n\n10.0.0.1 5000 10.0.0.2 22\n"); got != "this node did not answer with the kernel's boot ID from "+
		`/proc/sys/kernel/random/boot_id, which MMX reads to tell a machine from a clone of it: it answered "systemd"` {
		t.Fatal(got)
	}
}

// The answers that MMX took before are taken as before: a machine ID file
// with or without a final line break, and empty lines before the answer.
func TestFirstAnswerOfALinuxNode(t *testing.T) {
	want := []string{"Linux", "x86_64", "1000", "deploy", "0b0b0b0b-0000-4000-8000-000000000000", "0123456789abcdef0123456789abcdef", "10.0.0.1", "5000", "10.0.0.2", "22"}
	for name, output := range map[string]string{
		"a file that ends with a line break": "Linux\nx86_64\n1000\ndeploy\n0b0b0b0b-0000-4000-8000-000000000000\nsystemd\n0123456789abcdef0123456789abcdef\n\n10.0.0.1 5000 10.0.0.2 22\n",
		"a file without a final line break":  "Linux\nx86_64\n1000\ndeploy\n0b0b0b0b-0000-4000-8000-000000000000\nsystemd\n0123456789abcdef0123456789abcdef\n10.0.0.1 5000 10.0.0.2 22\n",
		"empty lines first":                  "\n\nLinux\nx86_64\n1000\ndeploy\n0b0b0b0b-0000-4000-8000-000000000000\nsystemd\n0123456789abcdef0123456789abcdef\n\n10.0.0.1 5000 10.0.0.2 22\n",
	} {
		if got, err := probeAnswer(output); err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: %q %v", name, got, err)
		}
	}
}

// An SSH account whose login shell refuses commands, such as /usr/sbin/nologin
// or /bin/false, makes ssh exit 1 and print nothing. The failure must say what
// MMX saw and the likely cause, not end after "exit status 1: ".
func TestSSHAccountWhoseShellRefusesCommandsIsToldTheCause(t *testing.T) {
	fakeSSH(t, `case "$1" in -G) printf 'hostname fixture\nuser dev\nport 22\n'; exit 0;; -F) exit 0;; esac
exit 1
`)
	c, r := fleet(t, config.FleetStage{ID: "setup", Nodes: "a"})
	r.Inventory.Nodes["a"] = config.Node{Address: "a", Transport: "ssh_only"}
	en := r.Entries[0]
	en.scope = value.Scope{}
	en, err := c.executeEntry(context.Background(), r, r.Project.FleetStages[0], en, false, func(Entry) error { return nil })
	if err != nil || en.Response.Status != "failed" {
		t.Fatalf("%+v %v", en.Response, err)
	}
	const want = "SSH a: exit status 1: the first command of MMX printed nothing and exited with status 1. " +
		"The likely cause is that the login shell of the SSH account refuses commands, as /usr/sbin/nologin and /bin/false do. " +
		"Give the account a shell such as /bin/sh, or use another ssh_user"
	if !strings.Contains(en.Response.Error, want) {
		t.Fatal(en.Response.Error)
	}
}

// MMX says "setting up MMX through verified SSH" only for a node whose first
// SSH check succeeded: a node that fails the check is not set up.
func TestNoSetupNoticeForANodeThatFailsTheFirstCheck(t *testing.T) {
	fakeSSH(t, `case "$1" in -G) printf 'hostname fixture\nuser dev\nport 22\n'; exit 0;; -F) exit 0;; esac
exit 1
`)
	c, r := fleet(t, config.FleetStage{ID: "setup", Nodes: "a"})
	var notices []string
	c.Credentials = &access.Credentials{Notify: func(message string) { notices = append(notices, message) }}
	r.Inventory.Nodes["a"] = config.Node{Address: "a"}
	en := r.Entries[0]
	en.scope = value.Scope{}
	en, err := c.executeEntry(context.Background(), r, r.Project.FleetStages[0], en, false, func(Entry) error { return nil })
	if err != nil || en.Response.Status != "failed" {
		t.Fatalf("%+v %v", en.Response, err)
	}
	if got := strings.Join(notices, "\n"); strings.Contains(got, "setting up MMX") {
		t.Fatal(got)
	}
}
