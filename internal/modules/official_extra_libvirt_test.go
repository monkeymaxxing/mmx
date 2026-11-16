package modules

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

// libvirt_domain runs virsh. These tests give it a stand-in virsh that keeps
// the domains of one libvirt in files and prints what the real virsh printed
// on test nodes: libvirt 8.0.0, older than the 9.0.0 of Debian 12, the oldest
// of the six target distributions, and libvirt 12.8.0, newer than the 12.0.0
// of Ubuntu 26.04, the newest of the six.
// tests/integration/libvirt_domains.py proves the module against real
// libvirt.

const (
	oldestLibvirt = "8.0.0"
	newestLibvirt = "12.8.0"
	// libvirtDomainUUID is the UUID that the stand-in gives a new domain.
	libvirtDomainUUID = "30647678-0148-4cb0-ba3b-a75ac79c049a"
	// libvirtWentAway is what virsh printed on the test nodes when libvirt's
	// socket was open and its service could not start.
	libvirtWentAway  = "error: failed to connect to the hypervisor\nerror: Cannot recv data: Connection reset by peer"
	libvirtDomainXML = "<domain type='kvm'>\n  <name>shop-db</name>\n  <memory unit='MiB'>64</memory>\n  <os>\n    <type arch='x86_64'>hvm</type>\n  </os>\n</domain>\n"
)

// standInVirsh is the body of the stand-in program. It takes
// "virsh [-c URI] COMMAND ARGUMENTS" and keeps libvirt's state in the
// directory libvirt below the stand-ins' directory $STUBS:
//
//	libvirt_version               8.0.0 or 12.8.0
//	does_not_answer               while it exists, no command reaches libvirt
//	refuse_COMMAND                while it exists, COMMAND fails with this standard error
//	domains/NAME/state            running, paused, in shutdown, or shut off
//	domains/NAME/states_to_come   each virsh domstate takes the first line as the new state
//	domains/NAME/uuid, autostart (enable or disable), persistent (yes or no),
//	  managed_save (yes or no), record (the SHA-256 in the metadata), snapshots
//	  and checkpoints (one name in each line), tpm (the domain has an emulated TPM),
//	  answers_as (the name that virsh dominfo prints), more_messages (more
//	  lines of the block "Messages:" of virsh dominfo)
//	standard_input_of_define      what the last virsh define read
//	tpm_state_deleted             an undefine deleted the state of an emulated TPM
//	checkpoint_records_deleted    an undefine deleted libvirt's records of checkpoints
const standInVirsh = `STATE="$STUBS/libvirt"
if [ "$1" = -c ]; then shift 2; fi
command=$1
shift
version=$(cat "$STATE/libvirt_version")
for last; do :; done
domain="$STATE/domains/$last"

answers() {
	if [ -e "$STATE/does_not_answer" ]; then
		echo "error: failed to connect to the hypervisor" >&2
		echo "error: Failed to connect socket to '/var/run/libvirt/virtqemud-sock': No such file or directory" >&2
		exit 1
	fi
	if [ -e "$STATE/refuse_$command" ]; then
		echo
		cat "$STATE/refuse_$command" >&2
		exit 1
	fi
}
exists() {
	answers
	if [ ! -d "$domain" ]; then
		echo
		echo "error: failed to get domain '$last'" >&2
		exit 1
	fi
}
refused() {
	echo
	for line; do echo "error: $line" >&2; done
	exit 1
}

case "$command" in
list)
	answers
	ls "$STATE/domains"
	echo
	;;
dominfo)
	exists
	state=$(cat "$domain/state")
	answers_as=$last
	if [ -e "$domain/answers_as" ]; then answers_as=$(cat "$domain/answers_as"); fi
	if [ "$state" = "shut off" ]; then echo "Id:             -"; else echo "Id:             1"; fi
	echo "Name:           $answers_as"
	echo "UUID:           $(cat "$domain/uuid")"
	echo "OS Type:        hvm"
	echo "State:          $state"
	echo "CPU(s):         1"
	if [ "$state" != "shut off" ]; then echo "CPU time:       0.6s"; fi
	echo "Max memory:     65536 KiB"
	echo "Used memory:    65536 KiB"
	echo "Persistent:     $(cat "$domain/persistent")"
	echo "Autostart:      $(cat "$domain/autostart")"
	if [ "$version" != 8.0.0 ]; then echo "Autostart Once: disable"; fi
	echo "Managed save:   $(cat "$domain/managed_save")"
	if [ "$version" = 8.0.0 ]; then
		echo "Security model: none"
		echo "Security DOI:   0"
	else
		echo "Security model: selinux"
		echo "Security DOI:   0"
		if [ "$state" != "shut off" ]; then
			echo "Security label: system_u:system_r:svirt_t:s0:c520,c772 (enforcing)"
			echo "Messages:       tainted: use of deprecated configuration settings"
			if [ -e "$domain/more_messages" ]; then cat "$domain/more_messages"; fi
		fi
	fi
	echo
	;;
domstate)
	exists
	if [ -s "$domain/states_to_come" ]; then
		head -n 1 "$domain/states_to_come" > "$domain/state"
		sed -i.old 1d "$domain/states_to_come"
	fi
	cat "$domain/state"
	echo
	;;
metadata)
	domain="$STATE/domains/$1"
	last=$1
	exists
	if [ "$(cat "$domain/persistent")" = no ]; then
		refused "Requested operation is not valid: transient domains do not have any persistent config"
	fi
	case "$*" in
	*--set*)
		printf '%s\n' "$7" | sed "s/.*sha256='\([0-9a-f]*\)'.*/\1/" > "$domain/record"
		echo "Metadata modified"
		echo
		;;
	*)
		if [ -e "$domain/record" ]; then
			echo "<last_defined_file sha256=\"$(cat "$domain/record")\"/>"
			echo
		elif [ "$version" = 8.0.0 ]; then
			refused "metadata not found: Requested metadata element is not present"
		else
			echo
			echo
		fi
		;;
	esac
	;;
define)
	cat > "$STATE/standard_input_of_define"
	answers
	name=$(sed -n 's/.*<name>\(.*\)<\/name>.*/\1/p' "$STATE/standard_input_of_define" | head -n 1)
	uuid=$(sed -n 's/.*<uuid>\(.*\)<\/uuid>.*/\1/p' "$STATE/standard_input_of_define" | head -n 1)
	domain="$STATE/domains/$name"
	if [ -d "$domain" ]; then
		if [ "$uuid" != "$(cat "$domain/uuid")" ]; then
			refused "Failed to define domain from /dev/stdin" "operation failed: domain '$name' already exists with uuid $(cat "$domain/uuid")"
		fi
	else
		mkdir -p "$domain"
		echo "shut off" > "$domain/state"
		echo disable > "$domain/autostart"
		echo no > "$domain/managed_save"
		echo "${uuid:-` + libvirtDomainUUID + `}" > "$domain/uuid"
	fi
	echo yes > "$domain/persistent"
	rm -f "$domain/record"
	echo "Domain '$name' defined from /dev/stdin"
	echo
	;;
start)
	exists
	if [ "$(cat "$domain/state")" != "shut off" ]; then refused "Domain is already active"; fi
	echo running > "$domain/state"
	echo "Domain '$last' started"
	echo
	;;
resume)
	exists
	if [ "$(cat "$domain/state")" != paused ]; then refused "Requested operation is not valid: domain is already running"; fi
	echo running > "$domain/state"
	echo "Domain '$last' resumed"
	echo
	;;
shutdown)
	exists
	if [ "$(cat "$domain/state")" != running ]; then
		refused "Failed to shutdown domain '$last'" "Requested operation is not valid: domain is not running"
	fi
	echo "Domain '$last' is being shutdown"
	echo
	;;
destroy)
	exists
	if [ "$(cat "$domain/state")" = "shut off" ]; then
		refused "Failed to destroy domain '$last'" "Requested operation is not valid: domain is not running"
	fi
	echo "shut off" > "$domain/state"
	echo "Domain '$last' destroyed"
	echo
	;;
autostart)
	exists
	if [ "$1" = --disable ]; then
		echo disable > "$domain/autostart"
		echo "Domain '$last' unmarked as autostarted"
	else
		echo enable > "$domain/autostart"
		echo "Domain '$last' marked as autostarted"
	fi
	echo
	;;
snapshot-list | checkpoint-list)
	domain="$STATE/domains/$1"
	last=$1
	exists
	if [ -e "$domain/${command%-list}s" ]; then cat "$domain/${command%-list}s"; fi
	echo
	;;
undefine)
	exists
	case "$*" in
	*--keep-tpm*)
		if [ "$version" = 8.0.0 ]; then refused "command 'undefine' doesn't support option --keep-tpm"; fi
		;;
	*)
		if [ -e "$domain/tpm" ]; then touch "$STATE/tpm_state_deleted"; fi
		;;
	esac
	if [ "$(cat "$domain/managed_save")" = yes ]; then refused "Refusing to undefine while domain managed save image exists"; fi
	if [ -s "$domain/snapshots" ]; then
		refused "Failed to undefine domain '$last'" "Requested operation is not valid: cannot delete inactive domain with 1 snapshots"
	fi
	if [ "$(cat "$domain/state")" = "shut off" ]; then
		# libvirt removes its records of the domain's checkpoints without a word.
		if [ -s "$domain/checkpoints" ]; then touch "$STATE/checkpoint_records_deleted"; fi
		rm -r "$domain"
	else
		# libvirt removes the stored definition and leaves the domain running.
		echo no > "$domain/persistent"
	fi
	echo "Domain '$last' has been undefined"
	echo
	;;
*)
	echo "error: unknown command: '$command'" >&2
	exit 1
	;;
esac`

// libvirtStandIn is one libvirt behind the stand-in virsh.
type libvirtStandIn struct {
	t *testing.T
	// directory holds the state of libvirt.
	directory string
	// allCalls returns every call of virsh and sleep since the test began,
	// and forgotten is how many of them were made before the last restart.
	allCalls  func() []string
	forgotten int
}

// calls returns every call of virsh and sleep since the last restart, in
// order.
func (l *libvirtStandIn) calls() []string {
	return l.allCalls()[l.forgotten:]
}

// newLibvirtStandIn makes the stand-ins for virsh and sleep, with a libvirt
// of this version that has no domain.
func newLibvirtStandIn(t *testing.T, version string) *libvirtStandIn {
	t.Helper()
	calls := stubTools(t, map[string]string{"virsh": standInVirsh, "sleep": ""})
	l := &libvirtStandIn{t: t, directory: filepath.Join(stubToolsDirectory(t), "libvirt"), allCalls: calls}
	l.restart(version)
	return l
}

// restart gives the stand-in a libvirt of this version that has no domain,
// and forgets the calls so far. One test uses one stand-in for all of its
// cases.
func (l *libvirtStandIn) restart(version string) {
	l.t.Helper()
	if err := os.RemoveAll(l.directory); err != nil {
		l.t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(l.directory, "domains"), 0755); err != nil {
		l.t.Fatal(err)
	}
	l.forgotten = len(l.allCalls())
	l.write("libvirt_version", version)
}

func (l *libvirtStandIn) write(file, content string) {
	l.t.Helper()
	path := filepath.Join(l.directory, file)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		l.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content+"\n"), 0644); err != nil {
		l.t.Fatal(err)
	}
}

// read returns a file of the stand-in without its last line end, or "" when
// the file does not exist.
func (l *libvirtStandIn) read(file string) string {
	l.t.Helper()
	b, err := os.ReadFile(filepath.Join(l.directory, file))
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		l.t.Fatal(err)
	}
	return strings.TrimSuffix(string(b), "\n")
}

// domain gives libvirt a stored domain in this state that does not start at
// boot. settings are more files of the domain, such as "record".
func (l *libvirtStandIn) domain(name, state string, settings map[string]string) {
	l.t.Helper()
	files := map[string]string{"state": state, "uuid": libvirtDomainUUID, "autostart": "disable", "persistent": "yes", "managed_save": "no"}
	for file, content := range settings {
		files[file] = content
	}
	for file, content := range files {
		l.write(filepath.Join("domains", name, file), content)
	}
}

// changingCalls returns the calls of virsh that change libvirt, without
// "virsh -c qemu:///system " before each.
func (l *libvirtStandIn) changingCalls() []string {
	var changing []string
	for _, call := range l.calls() {
		command := strings.TrimPrefix(call, "virsh -c qemu:///system ")
		reads := false
		for _, read := range []string{"list ", "dominfo ", "domstate ", "snapshot-list ", "checkpoint-list ", "sleep "} {
			reads = reads || strings.HasPrefix(command, read)
		}
		if strings.HasPrefix(command, "metadata ") && !strings.Contains(command, "--set") {
			reads = true
		}
		if !reads {
			changing = append(changing, command)
		}
	}
	return changing
}

// libvirtDomainFile writes the XML file of a task and returns its path and
// the SHA-256 that the module records for it.
func libvirtDomainFile(t *testing.T, content string) (path, sha256 string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "shop-db.xml")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path, Runtime{}.SHA256(content)
}

func libvirtRecordCommand(sha256 string) string {
	return "metadata shop-db --uri urn:mmx:libvirt_domain --key mmx --set <last_defined_file sha256='" + sha256 + "'/> --config"
}

// expectLibvirtResult fails the test unless the result has this status,
// change, and reason, and this value of the output created.
func expectLibvirtResult(t *testing.T, r protocol.Result, status, change, reason string, created any) {
	t.Helper()
	if r.Status != status || r.Change != change || r.Reason != reason {
		t.Fatalf("status %q, change %q, reason %q;\nwant   %q, %q, %q", r.Status, r.Change, r.Reason, status, change, reason)
	}
	if got := r.ModuleOutputs["created"]; got != created {
		t.Fatalf("created is %v, want %v: %+v", got, created, r)
	}
}

// expectLibvirtCannotSucceed fails the test unless the result is a failure that
// another try cannot fix, which MMX tries once: the script said
// invalid_inputs.
func expectLibvirtCannotSucceed(t *testing.T, r protocol.Result, change, reason string) {
	t.Helper()
	if r.Status != "failed" || r.FailureKind != "config" || r.Change != change || r.Reason != reason {
		t.Fatalf("status %q, failure kind %q, change %q, reason %q;\nwant   \"failed\", \"configuration\", %q, %q", r.Status, r.FailureKind, r.Change, r.Reason, change, reason)
	}
}

// expectLibvirtMissingState fails the test unless the task met a state that
// an earlier node task may change: the script said missing_prerequisite, so
// a dryrun is not checked and a run fails once.
func expectLibvirtMissingState(t *testing.T, inputs map[string]any, reason string) {
	t.Helper()
	if r := runOfficialExtraModule(t, "libvirt_domain", inputs, true); r.Status != "not_checked" || r.Change != "unknown" || r.Reason != reason {
		t.Fatalf("dryrun: status %q, change %q, reason %q;\nwant the reason %q", r.Status, r.Change, r.Reason, reason)
	}
	if r := runOfficialExtraModule(t, "libvirt_domain", inputs, false); r.Status != "failed" || r.FailureKind != "missing_prerequisite" || r.Change != "unchanged" || r.Reason != reason {
		t.Fatalf("run: status %q, failure kind %q, change %q, reason %q;\nwant the reason %q", r.Status, r.FailureKind, r.Change, r.Reason, reason)
	}
}

func expectLibvirtCalls(t *testing.T, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("calls:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// The node tasks under "Set up" on the module's page are the first node
// tasks of the example project libvirt-host, comments aside. The integration
// suite runs the example project, so the page's node tasks are tested.
func TestLibvirtDomainPageShowsTheSetUpOfTheExampleProject(t *testing.T) {
	t.Parallel()
	directory := officialExtraModule(t, "libvirt_domain").Directory
	withoutComments := func(yaml string) []string {
		var lines []string
		for _, line := range strings.Split(yaml, "\n") {
			if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
				lines = append(lines, line)
			}
		}
		return lines
	}
	page, err := os.ReadFile(filepath.Join(directory, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, afterHeading, _ := strings.Cut(string(page), "\n## Set up\n")
	_, afterFence, _ := strings.Cut(afterHeading, "```yaml\n")
	block, _, found := strings.Cut(afterFence, "```")
	setUp := withoutComments(block)
	if !found || len(setUp) == 0 {
		t.Fatal("the page has no YAML block under Set up")
	}
	playbook, err := os.ReadFile(filepath.Join(directory, "..", "..", "examples", "libvirt-host", "playbooks", "configure.yml"))
	if err != nil {
		t.Fatal(err)
	}
	tasks := withoutComments(string(playbook))
	// The playbook's first four lines open its one fleet stage.
	if len(tasks) < 4+len(setUp) || !reflect.DeepEqual(tasks[4:4+len(setUp)], setUp) {
		t.Fatalf("the node tasks under Set up are not the first node tasks of examples/libvirt-host/playbooks/configure.yml:\n%s", strings.Join(setUp, "\n"))
	}
}

func TestLibvirtDomainRejectsInputsBeforeItRunsAnything(t *testing.T) {
	t.Parallel()
	libvirt := newLibvirtStandIn(t, newestLibvirt)
	for name, c := range map[string]struct {
		inputs map[string]any
		reason string
	}{
		"an empty name": {map[string]any{"name": "", "state": "absent"},
			"libvirt_domain.name: must not be empty"},
		"a name that virsh reads as an option": {map[string]any{"name": "--help", "state": "absent"},
			"libvirt_domain.name: must not start with a hyphen (-), which virsh would read as an option"},
		"a name that is a number": {map[string]any{"name": "12345", "state": "absent"},
			"libvirt_domain.name: must not be a number, and 12345 is one: virsh takes a number for the ID of a running domain before it looks for a name"},
		"a name in the form of a UUID": {map[string]any{"name": libvirtDomainUUID, "state": "absent"},
			"libvirt_domain.name: must not have the form of a UUID, and " + libvirtDomainUUID + " has it: virsh takes such a text for the UUID of a domain before it looks for a name"},
		"a path that is not absolute": {map[string]any{"name": "shop-db", "state": "running", "domain_xml_file": "shop-db.xml"},
			"libvirt_domain.domain_xml_file: must be an absolute path, one that starts with /"},
		"a negative wait": {map[string]any{"name": "shop-db", "state": "stopped", "domain_xml_file": "/etc/mmx-libvirt/shop-db.xml", "wait_for_shutdown_at_most": "-1s"},
			"libvirt_domain.wait_for_shutdown_at_most: must be a nonnegative duration of whole seconds, such as 30s or 2m; shutdown checks repeat each second"},
		"an empty connection URI": {map[string]any{"name": "shop-db", "state": "absent", "libvirt_connection_uri": ""},
			"libvirt_domain.libvirt_connection_uri: must not be empty"},
		"a file for a domain that is to be absent": {map[string]any{"name": "shop-db", "state": "absent", "domain_xml_file": "/etc/mmx-libvirt/shop-db.xml"},
			"libvirt_domain.state: absent accepts only name and libvirt_connection_uri; remove domain_xml_file"},
		"a start at boot for a domain that is to be absent": {map[string]any{"name": "shop-db", "state": "absent", "start_at_boot": false},
			"libvirt_domain.state: absent accepts only name and libvirt_connection_uri; remove start_at_boot"},
		"a wait and a power-off for a domain that is to be absent": {map[string]any{"name": "shop-db", "state": "absent", "libvirt_connection_uri": "qemu:///system",
			"wait_for_shutdown_at_most": "10s", "power_off_when_shutdown_takes_longer": true},
			"libvirt_domain.state: absent accepts only name and libvirt_connection_uri; remove power_off_when_shutdown_takes_longer"},
	} {
		t.Run(name, func(t *testing.T) {
			libvirt.restart(newestLibvirt)
			for _, dryrun := range []bool{true, false} {
				r := runOfficialExtraModule(t, "libvirt_domain", c.inputs, dryrun)
				if r.Status != "failed" || r.FailureKind != "config" || r.Reason != c.reason {
					t.Fatalf("dryrun %v: %+v", dryrun, r)
				}
			}
			expectLibvirtCalls(t, libvirt.calls(), nil)
		})
	}
	// The description checks the type and the allowed values.
	r := runOfficialExtraModule(t, "libvirt_domain", map[string]any{"name": "shop-db", "state": "sleeping"}, false)
	if r.FailureKind != "config" || !strings.Contains(r.Reason, "libvirt_domain.state") {
		t.Fatalf("%+v", r)
	}
}

// A name with 36 characters that is no UUID is a name.
func TestLibvirtDomainTakesALongNameThatIsNoUUID(t *testing.T) {
	t.Parallel()
	libvirt := newLibvirtStandIn(t, newestLibvirt)
	name := "shop-database-of-the-second-shop-001"
	if len(name) != 36 {
		t.Fatal(len(name))
	}
	r := runOfficialExtraModule(t, "libvirt_domain", map[string]any{"name": name, "state": "absent"}, false)
	expectLibvirtResult(t, r, "succeeded", "unchanged", "", false)
	expectLibvirtCalls(t, libvirt.calls(), []string{"virsh -c qemu:///system list --all --name"})
}

func TestLibvirtDomainNamesThePackageOfAMissingVirsh(t *testing.T) {
	t.Parallel()
	stubTools(t, map[string]string{"sleep": ""})
	inputs := map[string]any{"name": "shop-db", "state": "running", "domain_xml_file": "/etc/mmx-libvirt/shop-db.xml"}
	reason := "libvirt_domain: the program virsh is not installed; install the package libvirt-clients (Debian and Ubuntu) or " +
		"libvirt-client (the Red Hat family) in an earlier node task"
	if r := runOfficialExtraModule(t, "libvirt_domain", inputs, true); r.Status != "not_checked" || r.Change != "unknown" || r.Reason != reason {
		t.Fatalf("dryrun: %+v", r)
	}
	if r := runOfficialExtraModule(t, "libvirt_domain", inputs, false); r.Status != "failed" || r.FailureKind != "missing_prerequisite" || r.Reason != reason {
		t.Fatalf("run: %+v", r)
	}
	if r := runOfficialExtraModule(t, "libvirt_domain", map[string]any{"name": "shop-db", "state": "absent"}, false); r.FailureKind != "missing_prerequisite" || r.Reason != reason {
		t.Fatalf("absent: %+v", r)
	}
}

// A new domain: the file's text reaches virsh through standard input, the
// record follows the define, then the boot setting and the start.
func TestLibvirtDomainDefinesAndStartsANewDomain(t *testing.T) {
	t.Parallel()
	libvirt := newLibvirtStandIn(t, newestLibvirt)
	for _, version := range []string{oldestLibvirt, newestLibvirt} {
		t.Run(version, func(t *testing.T) {
			libvirt.restart(version)
			path, sha256 := libvirtDomainFile(t, libvirtDomainXML)
			inputs := map[string]any{"name": "shop-db", "state": "running", "domain_xml_file": path, "start_at_boot": true}

			previewed := runOfficialExtraModule(t, "libvirt_domain", inputs, true)
			expectLibvirtResult(t, previewed, "succeeded", "would_change",
				"would create the domain shop-db from "+path+"; would set the domain shop-db to start at boot; would start the domain shop-db", true)
			expectLibvirtCalls(t, libvirt.calls(), []string{"virsh -c qemu:///system list --all --name"})
			if !strings.Contains(previewed.Diff, "would run: virsh -c qemu:///system define /dev/stdin\n") || strings.Contains(previewed.Diff, "<name>") {
				t.Fatalf("diff: %q", previewed.Diff)
			}

			applied := runOfficialExtraModule(t, "libvirt_domain", inputs, false)
			expectLibvirtResult(t, applied, "succeeded", "changed",
				"created the domain shop-db from "+path+"; set the domain shop-db to start at boot; started the domain shop-db", true)
			expectLibvirtCalls(t, libvirt.changingCalls(), []string{"define /dev/stdin", libvirtRecordCommand(sha256), "autostart shop-db", "start shop-db"})
			// A new domain is defined from the file as it is.
			if got := libvirt.read("standard_input_of_define"); got+"\n" != libvirtDomainXML {
				t.Fatalf("virsh define read %q", got)
			}
			if libvirt.read("domains/shop-db/state") != "running" || libvirt.read("domains/shop-db/autostart") != "enable" || libvirt.read("domains/shop-db/record") != sha256 {
				t.Fatal("the domain is not as the task asked")
			}

			// Unchanged: only reads, in a run and in a dryrun.
			for _, dryrun := range []bool{false, true} {
				before := len(libvirt.calls())
				again := runOfficialExtraModule(t, "libvirt_domain", inputs, dryrun)
				expectLibvirtResult(t, again, "succeeded", "unchanged", "", false)
				expectLibvirtCalls(t, libvirt.calls()[before:], []string{
					"virsh -c qemu:///system list --all --name",
					"virsh -c qemu:///system dominfo shop-db",
					"virsh -c qemu:///system metadata shop-db --uri urn:mmx:libvirt_domain --config",
				})
				if again.Fingerprint != applied.Fingerprint || again.Diff != "" {
					t.Fatalf("fingerprint %q after %q, diff %q", again.Fingerprint, applied.Fingerprint, again.Diff)
				}
			}
		})
	}
}

// libvirt refuses a second definition of a name unless the XML holds the
// domain's UUID, so the module adds it. A running domain keeps its old
// definition, and the reason says so.
func TestLibvirtDomainDefinesAgainWhenTheFileChanged(t *testing.T) {
	t.Parallel()
	libvirt := newLibvirtStandIn(t, newestLibvirt)
	for _, version := range []string{oldestLibvirt, newestLibvirt} {
		t.Run(version, func(t *testing.T) {
			libvirt.restart(version)
			libvirt.domain("shop-db", "running", map[string]string{"record": Runtime{}.SHA256("the file before")})
			path, sha256 := libvirtDomainFile(t, libvirtDomainXML)
			inputs := map[string]any{"name": "shop-db", "state": "running", "domain_xml_file": path}

			previewed := runOfficialExtraModule(t, "libvirt_domain", inputs, true)
			expectLibvirtResult(t, previewed, "succeeded", "would_change",
				"would define the domain shop-db again from "+path+"; the running domain would keep the old definition until it is shut off and started again", false)
			expectLibvirtCalls(t, libvirt.changingCalls(), nil)

			applied := runOfficialExtraModule(t, "libvirt_domain", inputs, false)
			expectLibvirtResult(t, applied, "succeeded", "changed",
				"defined the domain shop-db again from "+path+"; the running domain keeps the old definition until it is shut off and started again", false)
			expectLibvirtCalls(t, libvirt.changingCalls(), []string{"define /dev/stdin", libvirtRecordCommand(sha256)})
			want := strings.Replace(libvirtDomainXML, "</domain>", "<uuid>"+libvirtDomainUUID+"</uuid>\n</domain>", 1)
			if got := libvirt.read("standard_input_of_define"); got+"\n" != want {
				t.Fatalf("virsh define read %q, want %q", got, want)
			}
			if libvirt.read("domains/shop-db/record") != sha256 {
				t.Fatal("the record is not the file's SHA-256")
			}
			if applied.Fingerprint != previewed.Fingerprint {
				t.Fatal("a dryrun and a run of the same task have different fingerprints")
			}
			expectLibvirtResult(t, runOfficialExtraModule(t, "libvirt_domain", inputs, false), "succeeded", "unchanged", "", false)
		})
	}
}

// previewLibvirtDomain runs one libvirt_domain task in a dryrun in which
// earlier node tasks would create or change these paths.
func previewLibvirtDomain(t *testing.T, inputs map[string]any, earlierNodeTasksWouldChange ...string) protocol.Result {
	t.Helper()
	scope, _ := value.Scope{}.Trace()
	module := officialExtraModule(t, "libvirt_domain")
	r := Runtime{Dryrun: true, PreviewPaths: earlierNodeTasksWouldChange}.Starlark(testContext(t), "libvirt_domain", module, inputs, scope)
	return reportedByTheEngine(r, scope)
}

// In a dryrun, the XML file on the node is not the run's file while an
// earlier node task would write it. The module then compares nothing of the
// old file: it reports that a run would define the domain from the changed
// file, and the rest of the task as always.
func TestLibvirtDomainDryrunWhenAnEarlierNodeTaskWouldChangeTheFile(t *testing.T) {
	t.Parallel()
	libvirt := newLibvirtStandIn(t, newestLibvirt)
	path, sha256 := libvirtDomainFile(t, libvirtDomainXML)
	inputs := map[string]any{"name": "shop-db", "state": "running", "domain_xml_file": path, "start_at_boot": true}
	changes := ", which an earlier node task of this dryrun would change"
	wouldRun := "would run: virsh -c qemu:///system define /dev/stdin\n" +
		"would run: virsh -c qemu:///system metadata shop-db --uri urn:mmx:libvirt_domain --key mmx --set <last_defined_file sha256='the SHA-256 of the changed file'/> --config\n"

	// The domain runs with the definition of the file as it is on the node.
	libvirt.domain("shop-db", "running", map[string]string{"record": sha256, "autostart": "enable"})
	unchanged := previewLibvirtDomain(t, inputs)
	expectLibvirtResult(t, unchanged, "succeeded", "unchanged", "", false)
	before := len(libvirt.calls())
	for _, earlier := range []string{path, filepath.Dir(path)} {
		r := previewLibvirtDomain(t, inputs, earlier)
		expectLibvirtResult(t, r, "succeeded", "would_change",
			"would define the domain shop-db again from "+path+changes+"; the running domain would keep the old definition until it is shut off and started again", false)
		if r.Diff != wouldRun || r.Fingerprint == unchanged.Fingerprint {
			t.Fatalf("diff %q, fingerprint %q as without the change: %v", r.Diff, r.Fingerprint, r.Fingerprint == unchanged.Fingerprint)
		}
	}
	// The module read neither the record nor anything else of the old file.
	expectLibvirtCalls(t, libvirt.calls()[before:], []string{
		"virsh -c qemu:///system list --all --name", "virsh -c qemu:///system dominfo shop-db",
		"virsh -c qemu:///system list --all --name", "virsh -c qemu:///system dominfo shop-db",
	})

	// The file on the node is still the file of another domain: its <name>
	// is not checked, because the earlier node task writes the file anew.
	if err := os.WriteFile(path, []byte(strings.Replace(libvirtDomainXML, "shop-db", "shop-web", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	if r := previewLibvirtDomain(t, inputs); r.FailureKind != "config" || !strings.Contains(r.Reason, "does not hold <name>shop-db</name>") {
		t.Fatalf("without the earlier node task: %+v", r)
	}
	r := previewLibvirtDomain(t, inputs, path)
	expectLibvirtResult(t, r, "succeeded", "would_change",
		"would define the domain shop-db again from "+path+changes+"; the running domain would keep the old definition until it is shut off and started again", false)

	// A first dryrun: libvirt has no such domain, and the file does not exist
	// yet. With the earlier node task that would create the file, the task
	// is checked: a run would define the domain, set its start at boot, and
	// start it.
	libvirt.restart(newestLibvirt)
	missing := filepath.Join(t.TempDir(), "mmx-libvirt", "shop-db.xml")
	inputs["domain_xml_file"] = missing
	if r := previewLibvirtDomain(t, inputs); r.Status != "not_checked" {
		t.Fatalf("without the earlier node task: %+v", r)
	}
	r = previewLibvirtDomain(t, inputs, filepath.Dir(missing), missing)
	expectLibvirtResult(t, r, "succeeded", "would_change",
		"would create the domain shop-db from "+missing+changes+"; would set the domain shop-db to start at boot; would start the domain shop-db", true)
	if r.Diff != wouldRun+"would run: virsh -c qemu:///system autostart shop-db\nwould run: virsh -c qemu:///system start shop-db\n" {
		t.Fatalf("diff %q", r.Diff)
	}
	expectLibvirtCalls(t, libvirt.changingCalls(), nil)

	// A run never asks: the paths of a dryrun do not count there.
	scope, _ := value.Scope{}.Trace()
	applied := Runtime{PreviewPaths: []string{missing}}.Starlark(testContext(t), "libvirt_domain", officialExtraModule(t, "libvirt_domain"), inputs, scope)
	if applied.FailureKind != "missing_prerequisite" {
		t.Fatalf("run: %+v", applied)
	}
}

// A file with its own <uuid> is used as it is, and the fingerprint follows
// the file, the wanted state, and start_at_boot.
func TestLibvirtDomainUsesAFileWithItsOwnUUIDAsItIs(t *testing.T) {
	t.Parallel()
	libvirt := newLibvirtStandIn(t, newestLibvirt)
	libvirt.domain("shop-db", "shut off", nil)
	content := strings.Replace(libvirtDomainXML, "<memory", "<uuid>"+libvirtDomainUUID+"</uuid>\n  <memory", 1)
	path, _ := libvirtDomainFile(t, content)
	inputs := map[string]any{"name": "shop-db", "state": "stopped", "domain_xml_file": path}
	first := runOfficialExtraModule(t, "libvirt_domain", inputs, false)
	expectLibvirtResult(t, first, "succeeded", "changed", "defined the domain shop-db again from "+path, false)
	if got := libvirt.read("standard_input_of_define"); got+"\n" != content {
		t.Fatalf("virsh define read %q", got)
	}
	fingerprints := map[string]bool{first.Fingerprint: true}
	for _, change := range []map[string]any{{"state": "running"}, {"start_at_boot": false}, {"start_at_boot": true}} {
		changed := map[string]any{"name": "shop-db", "state": "stopped", "domain_xml_file": path}
		for key, value := range change {
			changed[key] = value
		}
		fingerprints[runOfficialExtraModule(t, "libvirt_domain", changed, true).Fingerprint] = true
	}
	if err := os.WriteFile(path, []byte(strings.Replace(content, "64", "96", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	fingerprints[runOfficialExtraModule(t, "libvirt_domain", inputs, true).Fingerprint] = true
	if len(fingerprints) != 5 {
		t.Fatalf("%d different fingerprints for five different wanted states", len(fingerprints))
	}
}

// The oldest libvirt answers a domain without a record with an error, and
// the newest with empty output: both mean "define".
func TestLibvirtDomainDefinesADomainWithoutARecord(t *testing.T) {
	t.Parallel()
	libvirt := newLibvirtStandIn(t, newestLibvirt)
	for _, version := range []string{oldestLibvirt, newestLibvirt} {
		t.Run(version, func(t *testing.T) {
			libvirt.restart(version)
			libvirt.domain("shop-db", "shut off", nil)
			path, sha256 := libvirtDomainFile(t, libvirtDomainXML)
			r := runOfficialExtraModule(t, "libvirt_domain", map[string]any{"name": "shop-db", "state": "stopped", "domain_xml_file": path}, false)
			expectLibvirtResult(t, r, "succeeded", "changed", "defined the domain shop-db again from "+path, false)
			expectLibvirtCalls(t, libvirt.changingCalls(), []string{"define /dev/stdin", libvirtRecordCommand(sha256)})
		})
	}
	// A record that is not the module's own is no record.
	libvirt.restart(newestLibvirt)
	libvirt.domain("shop-db", "shut off", map[string]string{"record": "not-a-sha256"})
	path, sha256 := libvirtDomainFile(t, libvirtDomainXML)
	r := runOfficialExtraModule(t, "libvirt_domain", map[string]any{"name": "shop-db", "state": "stopped", "domain_xml_file": path}, false)
	expectLibvirtResult(t, r, "succeeded", "changed", "defined the domain shop-db again from "+path, false)
	if libvirt.read("domains/shop-db/record") != sha256 {
		t.Fatal(libvirt.read("domains/shop-db/record"))
	}
}

// A domain that runs without a stored definition has no record to read. The
// define stores it; it was not created.
func TestLibvirtDomainStoresADomainThatRunsWithoutADefinition(t *testing.T) {
	t.Parallel()
	libvirt := newLibvirtStandIn(t, newestLibvirt)
	libvirt.domain("shop-db", "running", map[string]string{"persistent": "no"})
	path, sha256 := libvirtDomainFile(t, libvirtDomainXML)
	inputs := map[string]any{"name": "shop-db", "state": "running", "domain_xml_file": path, "start_at_boot": true}
	r := runOfficialExtraModule(t, "libvirt_domain", inputs, false)
	expectLibvirtResult(t, r, "succeeded", "changed", "stored a definition for the domain shop-db, which ran without one, from "+path+
		"; set the domain shop-db to start at boot; the running domain keeps the old definition until it is shut off and started again", false)
	expectLibvirtCalls(t, libvirt.changingCalls(), []string{"define /dev/stdin", libvirtRecordCommand(sha256), "autostart shop-db"})
	for _, call := range libvirt.calls() {
		if strings.Contains(call, "metadata") && !strings.Contains(call, "--set") {
			t.Fatalf("the module read the record of a domain without a stored definition: %s", call)
		}
	}
	if libvirt.read("domains/shop-db/persistent") != "yes" {
		t.Fatal("the domain has no stored definition")
	}
}

// virsh dominfo of a running domain can end with a block whose later lines
// start with spaces, as libvirt 11.10.0 printed it on Rocky Linux 9. Such
// lines belong to the line before them.
func TestLibvirtDomainReadsDominfoWithLinesThatContinue(t *testing.T) {
	t.Parallel()
	libvirt := newLibvirtStandIn(t, newestLibvirt)
	path, sha256 := libvirtDomainFile(t, libvirtDomainXML)
	libvirt.domain("shop-db", "running", map[string]string{"record": sha256,
		"more_messages": "                deprecated configuration: machine type 'pc-i440fx-rhel7.6.0'\n                deprecated configuration: CPU model 'qemu64'"})
	r := runOfficialExtraModule(t, "libvirt_domain", map[string]any{"name": "shop-db", "state": "running", "domain_xml_file": path, "start_at_boot": false}, false)
	expectLibvirtResult(t, r, "succeeded", "unchanged", "", false)
	expectLibvirtCalls(t, libvirt.changingCalls(), nil)
}

func TestLibvirtDomainSetsOnlyWhatDiffers(t *testing.T) {
	t.Parallel()
	libvirt := newLibvirtStandIn(t, newestLibvirt)
	path, sha256 := libvirtDomainFile(t, libvirtDomainXML)
	for name, c := range map[string]struct {
		state    string
		settings map[string]string
		inputs   map[string]any
		calls    []string
		reason   string
	}{
		"start": {"shut off", nil, map[string]any{"state": "running"},
			[]string{"start shop-db"}, "started the domain shop-db"},
		"resume": {"paused", nil, map[string]any{"state": "running"},
			[]string{"resume shop-db"}, "resumed the paused domain shop-db"},
		"start at boot": {"running", nil, map[string]any{"state": "running", "start_at_boot": true},
			[]string{"autostart shop-db"}, "set the domain shop-db to start at boot"},
		"do not start at boot": {"shut off", map[string]string{"autostart": "enable"}, map[string]any{"state": "stopped", "start_at_boot": false},
			[]string{"autostart --disable shop-db"}, "set the domain shop-db not to start at boot"},
		"an omitted start_at_boot leaves the setting": {"shut off", map[string]string{"autostart": "enable"}, map[string]any{"state": "stopped"},
			nil, ""},
	} {
		t.Run(name, func(t *testing.T) {
			libvirt.restart(newestLibvirt)
			settings := map[string]string{"record": sha256}
			for file, content := range c.settings {
				settings[file] = content
			}
			libvirt.domain("shop-db", c.state, settings)
			inputs := map[string]any{"name": "shop-db", "domain_xml_file": path}
			for key, value := range c.inputs {
				inputs[key] = value
			}
			change, would := "changed", "would_change"
			if c.calls == nil {
				change, would = "unchanged", "unchanged"
			}
			previewed := runOfficialExtraModule(t, "libvirt_domain", inputs, true)
			wouldReason := c.reason
			for past, future := range map[string]string{"started": "would start", "resumed": "would resume", "set": "would set"} {
				wouldReason = strings.Replace(wouldReason, past+" the", future+" the", 1)
			}
			expectLibvirtResult(t, previewed, "succeeded", would, wouldReason, false)
			expectLibvirtCalls(t, libvirt.changingCalls(), nil)
			expectLibvirtResult(t, runOfficialExtraModule(t, "libvirt_domain", inputs, false), "succeeded", change, c.reason, false)
			expectLibvirtCalls(t, libvirt.changingCalls(), c.calls)
		})
	}
}

// stopped asks once, reads the state once in each second, and ends at "shut
// off"; "in shutdown" means that the wait goes on.
func TestLibvirtDomainWaitsForAShutdown(t *testing.T) {
	t.Parallel()
	path, sha256 := libvirtDomainFile(t, libvirtDomainXML)
	inputs := map[string]any{"name": "shop-db", "state": "stopped", "domain_xml_file": path, "wait_for_shutdown_at_most": "30s"}
	libvirt := newLibvirtStandIn(t, newestLibvirt)
	libvirt.domain("shop-db", "running", map[string]string{"record": sha256, "states_to_come": "running\nrunning\nin shutdown\nshut off"})

	previewed := runOfficialExtraModule(t, "libvirt_domain", inputs, true)
	expectLibvirtResult(t, previewed, "succeeded", "would_change", "would ask the domain shop-db to shut down and wait at most 30 seconds", false)
	withPowerOff := map[string]any{"name": "shop-db", "state": "stopped", "domain_xml_file": path, "power_off_when_shutdown_takes_longer": true}
	previewed = runOfficialExtraModule(t, "libvirt_domain", withPowerOff, true)
	expectLibvirtResult(t, previewed, "succeeded", "would_change",
		"would ask the domain shop-db to shut down and wait at most 120 seconds; would cut its power if it still runs then", false)
	for _, call := range libvirt.calls() {
		if strings.HasPrefix(call, "sleep") || strings.Contains(call, "domstate") {
			t.Fatalf("a dryrun waited: %s", call)
		}
	}

	before := len(libvirt.calls())
	applied := runOfficialExtraModule(t, "libvirt_domain", inputs, false)
	expectLibvirtResult(t, applied, "succeeded", "changed", "asked the domain shop-db to shut down; the domain shop-db was shut off after 3 seconds", false)
	expectLibvirtCalls(t, libvirt.calls()[before:], []string{
		"virsh -c qemu:///system list --all --name",
		"virsh -c qemu:///system dominfo shop-db",
		"virsh -c qemu:///system metadata shop-db --uri urn:mmx:libvirt_domain --config",
		"virsh -c qemu:///system shutdown shop-db",
		"virsh -c qemu:///system domstate shop-db", "sleep 1",
		"virsh -c qemu:///system domstate shop-db", "sleep 1",
		"virsh -c qemu:///system domstate shop-db", "sleep 1",
		"virsh -c qemu:///system domstate shop-db",
	})
	expectLibvirtResult(t, runOfficialExtraModule(t, "libvirt_domain", inputs, false), "succeeded", "unchanged", "", false)

	// A domain that already shuts down gets no second request.
	libvirt.restart(newestLibvirt)
	libvirt.domain("shop-db", "in shutdown", map[string]string{"record": sha256, "states_to_come": "in shutdown\nshut off"})
	applied = runOfficialExtraModule(t, "libvirt_domain", inputs, false)
	expectLibvirtResult(t, applied, "succeeded", "unchanged", "the domain shop-db was shut off after 1 seconds", false)
	expectLibvirtCalls(t, libvirt.changingCalls(), nil)
}

// When the wait ends and the domain still runs, the task fails, or cuts the
// power when the task says so.
func TestLibvirtDomainAfterAWaitThatEnded(t *testing.T) {
	t.Parallel()
	path, sha256 := libvirtDomainFile(t, libvirtDomainXML)
	inputs := map[string]any{"name": "shop-db", "state": "stopped", "domain_xml_file": path, "wait_for_shutdown_at_most": "2s"}

	libvirt := newLibvirtStandIn(t, newestLibvirt)
	libvirt.domain("shop-db", "running", map[string]string{"record": sha256})
	failed := runOfficialExtraModule(t, "libvirt_domain", inputs, false)
	expectLibvirtResult(t, failed, "failed", "changed", "libvirt_domain: the domain shop-db did not shut down within 2 seconds after the task asked it to (its state is running); "+
		"its operating system may ignore the request. Set power_off_when_shutdown_takes_longer: true to cut its power then, or give it more time with wait_for_shutdown_at_most", false)
	// Another try would send a second request and wait once more.
	if !failed.NoOtherTry || failed.FailureKind != "" {
		t.Fatalf("MMX would try the wait again: %+v", failed)
	}
	expectLibvirtCalls(t, libvirt.changingCalls(), []string{"shutdown shop-db"})
	if libvirt.read("domains/shop-db/state") != "running" {
		t.Fatal("the domain does not run any more")
	}

	inputs["power_off_when_shutdown_takes_longer"] = true
	before := len(libvirt.calls())
	cut := runOfficialExtraModule(t, "libvirt_domain", inputs, false)
	expectLibvirtResult(t, cut, "succeeded", "changed", "asked the domain shop-db to shut down; cut the power of the domain shop-db, which still ran after 2 seconds", false)
	expectLibvirtCalls(t, libvirt.calls()[before+3:], []string{
		"virsh -c qemu:///system shutdown shop-db",
		"virsh -c qemu:///system domstate shop-db", "sleep 1",
		"virsh -c qemu:///system domstate shop-db", "sleep 1",
		"virsh -c qemu:///system domstate shop-db",
		"virsh -c qemu:///system destroy shop-db",
	})

	// A wait of no seconds reads the state once.
	libvirt.restart(newestLibvirt)
	libvirt.domain("shop-db", "running", map[string]string{"record": sha256})
	inputs["wait_for_shutdown_at_most"] = "0s"
	cut = runOfficialExtraModule(t, "libvirt_domain", inputs, false)
	expectLibvirtResult(t, cut, "succeeded", "changed", "asked the domain shop-db to shut down; cut the power of the domain shop-db, which still ran after 0 seconds", false)
	expectLibvirtCalls(t, libvirt.changingCalls(), []string{"shutdown shop-db", "destroy shop-db"})

	// The domain shut down between the last read and virsh destroy: virsh
	// answers that it does not run, and the module reads the state once more.
	libvirt.restart(newestLibvirt)
	libvirt.domain("shop-db", "running", map[string]string{"record": sha256, "states_to_come": "running\nshut off"})
	libvirt.write("refuse_destroy", "error: Failed to destroy domain 'shop-db'\nerror: Requested operation is not valid: domain is not running")
	cut = runOfficialExtraModule(t, "libvirt_domain", inputs, false)
	expectLibvirtResult(t, cut, "succeeded", "changed", "asked the domain shop-db to shut down; the domain shop-db was shut off after 0 seconds", false)

	// virsh destroy fails for another reason, after the request changed the node.
	libvirt.restart(newestLibvirt)
	libvirt.domain("shop-db", "running", map[string]string{"record": sha256})
	libvirt.write("refuse_destroy", libvirtWentAway)
	cut = runOfficialExtraModule(t, "libvirt_domain", inputs, false)
	expectLibvirtResult(t, cut, "failed", "changed",
		"libvirt_domain: cutting the power of the domain shop-db: failed to connect to the hypervisor: Cannot recv data: Connection reset by peer", false)
	if cut.NoOtherTry {
		t.Fatal("a virsh destroy that failed is tried again")
	}

	// A domain that already shut down when the task began, and does not end
	// within the wait: the task sent no request, and waits once too.
	libvirt.restart(newestLibvirt)
	libvirt.domain("shop-db", "in shutdown", map[string]string{"record": sha256})
	delete(inputs, "power_off_when_shutdown_takes_longer")
	stuck := runOfficialExtraModule(t, "libvirt_domain", inputs, false)
	expectLibvirtResult(t, stuck, "failed", "unchanged", "libvirt_domain: the domain shop-db did not shut down within 0 seconds (its state is in shutdown); "+
		"its operating system may ignore the request. Set power_off_when_shutdown_takes_longer: true to cut its power then, or give it more time with wait_for_shutdown_at_most", false)
	if !stuck.NoOtherTry {
		t.Fatal("MMX would try the wait again")
	}
}

// States from which the module does not start or shut off a domain: another
// try would meet the same state, so MMX tries such a task once.
func TestLibvirtDomainFailsForAStateItCannotLeave(t *testing.T) {
	t.Parallel()
	libvirt := newLibvirtStandIn(t, newestLibvirt)
	path, sha256 := libvirtDomainFile(t, libvirtDomainXML)
	for name, c := range map[string]struct {
		state, wanted, reason string
	}{
		"shut off a paused domain": {"paused", "stopped",
			"libvirt_domain: the domain shop-db is paused, and a request to shut down cannot reach a paused domain; resume it first (virsh resume shop-db)"},
		"start a crashed domain": {"crashed", "running",
			`libvirt_domain: the domain shop-db is in the state "crashed", from which this module cannot make it run; look at it with virsh domstate --reason shop-db`},
		"shut off a suspended domain": {"pmsuspended", "stopped",
			`libvirt_domain: the domain shop-db is in the state "pmsuspended", from which this module cannot shut it off; look at it with virsh domstate --reason shop-db`},
	} {
		t.Run(name, func(t *testing.T) {
			libvirt.restart(newestLibvirt)
			libvirt.domain("shop-db", c.state, map[string]string{"record": sha256})
			inputs := map[string]any{"name": "shop-db", "state": c.wanted, "domain_xml_file": path, "power_off_when_shutdown_takes_longer": true}
			for _, dryrun := range []bool{true, false} {
				expectLibvirtCannotSucceed(t, runOfficialExtraModule(t, "libvirt_domain", inputs, dryrun), "unchanged", c.reason)
			}
			expectLibvirtCalls(t, libvirt.changingCalls(), nil)
		})
	}
	// A state word that virsh never printed on a test node.
	libvirt.restart(newestLibvirt)
	libvirt.domain("shop-db", "dreaming", map[string]string{"record": sha256})
	inputs := map[string]any{"name": "shop-db", "state": "running", "domain_xml_file": path}
	expectLibvirtCannotSucceed(t, runOfficialExtraModule(t, "libvirt_domain", inputs, false), "unchanged",
		`libvirt_domain: reading the domain shop-db: virsh dominfo printed "dreaming" for State, which this module does not know`)

	// A domain that shuts down at this moment is shut off at another try,
	// which can then start it: an ordinary failure.
	libvirt.restart(newestLibvirt)
	libvirt.domain("shop-db", "in shutdown", map[string]string{"record": sha256})
	r := runOfficialExtraModule(t, "libvirt_domain", inputs, false)
	expectLibvirtResult(t, r, "failed", "unchanged",
		`libvirt_domain: the domain shop-db shuts down at this moment (its state is "in shutdown"); the module starts it once it is shut off`, false)
	if r.FailureKind != "" {
		t.Fatalf("failure kind %q", r.FailureKind)
	}
	expectLibvirtCalls(t, libvirt.changingCalls(), nil)
}

// absent removes only the definition, and only of a domain that is shut off.
func TestLibvirtDomainRemovesTheDefinition(t *testing.T) {
	t.Parallel()
	libvirt := newLibvirtStandIn(t, newestLibvirt)
	inputs := map[string]any{"name": "shop-db", "state": "absent"}
	// A domain that libvirt does not have is unchanged.
	expectLibvirtResult(t, runOfficialExtraModule(t, "libvirt_domain", inputs, false), "succeeded", "unchanged", "", false)
	expectLibvirtCalls(t, libvirt.calls(), []string{"virsh -c qemu:///system list --all --name"})

	libvirt.domain("shop-db", "shut off", map[string]string{"autostart": "enable"})
	expectLibvirtResult(t, runOfficialExtraModule(t, "libvirt_domain", inputs, true), "succeeded", "would_change", "would remove the definition of the domain shop-db", false)
	expectLibvirtCalls(t, libvirt.changingCalls(), nil)
	expectLibvirtResult(t, runOfficialExtraModule(t, "libvirt_domain", inputs, false), "succeeded", "changed", "removed the definition of the domain shop-db", false)
	expectLibvirtCalls(t, libvirt.changingCalls(), []string{"undefine --keep-nvram --keep-tpm shop-db"})
	expectLibvirtResult(t, runOfficialExtraModule(t, "libvirt_domain", inputs, false), "succeeded", "unchanged", "", false)
}

// libvirt itself would remove the definition of a running domain and leave
// it running, so the module never asks. An earlier node task may shut the
// domain off: a dryrun is not checked, and a run fails once.
func TestLibvirtDomainDoesNotRemoveTheDefinitionOfADomainThatRuns(t *testing.T) {
	t.Parallel()
	libvirt := newLibvirtStandIn(t, newestLibvirt)
	for _, state := range []string{"running", "paused", "in shutdown"} {
		libvirt.restart(newestLibvirt)
		libvirt.domain("shop-db", state, nil)
		inputs := map[string]any{"name": "shop-db", "state": "absent"}
		reason := "libvirt_domain: the domain shop-db is not shut off (its state is " + state + "), and absent removes the definition only of a domain that is shut off. " +
			"Shut the domain off in an earlier node task, with state: stopped"
		expectLibvirtMissingState(t, inputs, reason)
		expectLibvirtCalls(t, libvirt.changingCalls(), nil)
		if libvirt.read("domains/shop-db/persistent") != "yes" {
			t.Fatal("the definition is gone")
		}
	}
}

// libvirt refuses to remove the definition of a domain with a saved memory
// image or with snapshots. The module reads both first, so that a dryrun
// claims no removal that a run would not do. An earlier node task may
// remove them: a dryrun is not checked, and a run fails once. More than
// five snapshots are named as the first five and a count.
func TestLibvirtDomainDoesNotRemoveADefinitionThatLibvirtWouldRefuse(t *testing.T) {
	t.Parallel()
	libvirt := newLibvirtStandIn(t, newestLibvirt)
	for name, c := range map[string]struct {
		settings map[string]string
		reason   string
	}{
		"a saved memory image": {map[string]string{"managed_save": "yes"},
			"libvirt_domain: the domain shop-db has a saved memory image, and libvirt does not remove the definition of such a domain. " +
				"Delete the image in an earlier node task (virsh managedsave-remove shop-db), or start the domain and shut it off"},
		"snapshots": {map[string]string{"snapshots": "before-upgrade\nsnap1"},
			"libvirt_domain: the domain shop-db has snapshots (before-upgrade, snap1), and libvirt does not remove the definition of such a domain. " +
				"Delete each of them in an earlier node task (virsh snapshot-delete shop-db NAME)"},
		"many snapshots": {map[string]string{"snapshots": "snap1\nsnap2\nsnap3\nsnap4\nsnap5\nsnap6"},
			"libvirt_domain: the domain shop-db has snapshots (snap1, snap2, snap3, snap4, snap5, and 1 more), and libvirt does not remove the definition of such a domain. " +
				"Delete each of them in an earlier node task (virsh snapshot-delete shop-db NAME)"},
	} {
		t.Run(name, func(t *testing.T) {
			libvirt.restart(newestLibvirt)
			libvirt.domain("shop-db", "shut off", c.settings)
			expectLibvirtMissingState(t, map[string]any{"name": "shop-db", "state": "absent"}, c.reason)
			expectLibvirtCalls(t, libvirt.changingCalls(), nil)
		})
	}
}

// libvirt removes the definition of a domain with checkpoints, and with it
// its record of each checkpoint, from which an incremental backup continues.
// absent removes a definition and nothing else, so the module refuses where
// libvirt would not. An earlier node task may remove the records: a dryrun
// is not checked, and a run fails once. The reason names the first five
// checkpoints and counts the rest.
func TestLibvirtDomainKeepsLibvirtsRecordsOfCheckpoints(t *testing.T) {
	t.Parallel()
	libvirt := newLibvirtStandIn(t, oldestLibvirt)
	inputs := map[string]any{"name": "shop-db", "state": "absent"}
	for checkpoints, named := range map[string]string{
		"before-backup": "before-backup",
		"monday\ntuesday\nwednesday\nthursday\nfriday":                   "monday, tuesday, wednesday, thursday, friday",
		"monday\ntuesday\nwednesday\nthursday\nfriday\nsaturday\nsunday": "monday, tuesday, wednesday, thursday, friday, and 2 more",
	} {
		for _, version := range []string{oldestLibvirt, newestLibvirt} {
			libvirt.restart(version)
			libvirt.domain("shop-db", "shut off", map[string]string{"checkpoints": checkpoints})
			expectLibvirtMissingState(t, inputs, "libvirt_domain: the domain shop-db has checkpoints ("+named+"). "+
				"libvirt would remove its record of each checkpoint with the definition, and absent removes nothing but the definition. "+
				"Remove the records in an earlier node task (virsh checkpoint-delete shop-db NAME --metadata)")
			expectLibvirtCalls(t, libvirt.changingCalls(), nil)
			if libvirt.read("checkpoint_records_deleted") != "" || libvirt.read("domains/shop-db/state") != "shut off" {
				t.Fatal("the records of the checkpoints are deleted, or the definition is gone")
			}
		}
	}

	// Once the records are gone, the task removes the definition.
	libvirt.restart(newestLibvirt)
	libvirt.domain("shop-db", "shut off", nil)
	expectLibvirtResult(t, runOfficialExtraModule(t, "libvirt_domain", inputs, false), "succeeded", "changed", "removed the definition of the domain shop-db", false)
	expectLibvirtCalls(t, libvirt.calls(), []string{
		"virsh -c qemu:///system list --all --name",
		"virsh -c qemu:///system dominfo shop-db",
		"virsh -c qemu:///system snapshot-list shop-db --name",
		"virsh -c qemu:///system checkpoint-list shop-db --name",
		"virsh -c qemu:///system undefine --keep-nvram --keep-tpm shop-db",
	})
}

// The state of an emulated TPM can hold a disk's key. libvirt keeps it with
// --keep-tpm. A libvirt older than any that a supported distribution ships
// has no --keep-tpm: virsh refuses the option, and the state stays.
func TestLibvirtDomainKeepsTheStateOfAnEmulatedTPM(t *testing.T) {
	t.Parallel()
	inputs := map[string]any{"name": "shop-db", "state": "absent"}

	libvirt := newLibvirtStandIn(t, newestLibvirt)
	libvirt.domain("shop-db", "shut off", map[string]string{"tpm": "yes"})
	expectLibvirtResult(t, runOfficialExtraModule(t, "libvirt_domain", inputs, false), "succeeded", "changed", "removed the definition of the domain shop-db", false)
	expectLibvirtCalls(t, libvirt.changingCalls(), []string{"undefine --keep-nvram --keep-tpm shop-db"})
	if libvirt.read("tpm_state_deleted") != "" || libvirt.read("domains/shop-db/state") != "" {
		t.Fatal("the TPM's state is deleted, or the domain is still defined")
	}

	libvirt.restart(oldestLibvirt)
	libvirt.domain("shop-db", "shut off", map[string]string{"tpm": "yes"})
	expectLibvirtResult(t, runOfficialExtraModule(t, "libvirt_domain", inputs, false), "failed", "changed",
		"libvirt_domain: removing the definition of the domain shop-db: command 'undefine' doesn't support option --keep-tpm", false)
	if libvirt.read("tpm_state_deleted") != "" || libvirt.read("domains/shop-db/state") != "shut off" {
		t.Fatal("the TPM's state is deleted, or the definition is gone")
	}
}

// While libvirt does not answer, a dryrun cannot check the task, because an
// earlier node task may start libvirt's socket unit. A run fails with
// virsh's message.
func TestLibvirtDomainWhileLibvirtDoesNotAnswer(t *testing.T) {
	t.Parallel()
	libvirt := newLibvirtStandIn(t, newestLibvirt)
	libvirt.write("does_not_answer", "")
	path, _ := libvirtDomainFile(t, libvirtDomainXML)
	reason := "libvirt_domain: libvirt at qemu:///system does not answer: failed to connect to the hypervisor: " +
		"Failed to connect socket to '/var/run/libvirt/virtqemud-sock': No such file or directory; start libvirt's socket unit in an earlier node task " +
		"(libvirtd.socket on Debian and Ubuntu, virtqemud.socket on the other distributions)"
	for _, inputs := range []map[string]any{
		{"name": "shop-db", "state": "running", "domain_xml_file": path},
		{"name": "shop-db", "state": "absent"},
	} {
		if r := runOfficialExtraModule(t, "libvirt_domain", inputs, true); r.Status != "not_checked" || r.Change != "unknown" || r.Reason != reason {
			t.Fatalf("dryrun: %+v", r)
		}
		if r := runOfficialExtraModule(t, "libvirt_domain", inputs, false); r.Status != "failed" || r.Change != "unchanged" || r.Reason != reason || r.FailureKind != "" {
			t.Fatalf("run: %+v", r)
		}
	}
	expectLibvirtCalls(t, libvirt.changingCalls(), nil)

	// Another libvirt than the default one.
	inputs := map[string]any{"name": "shop-db", "state": "absent", "libvirt_connection_uri": "qemu:///session"}
	if r := runOfficialExtraModule(t, "libvirt_domain", inputs, true); r.Status != "not_checked" || !strings.HasPrefix(r.Reason, "libvirt_domain: libvirt at qemu:///session does not answer: ") {
		t.Fatalf("%+v", r)
	}
	if calls := libvirt.calls(); calls[len(calls)-1] != "virsh -c qemu:///session list --all --name" {
		t.Fatalf("%q", calls)
	}
}

// A read that fails is a failure, never "the domain does not exist".
func TestLibvirtDomainFailsWhenAReadFails(t *testing.T) {
	t.Parallel()
	libvirt := newLibvirtStandIn(t, newestLibvirt)
	path, sha256 := libvirtDomainFile(t, libvirtDomainXML)
	inputs := map[string]any{"name": "shop-db", "state": "running", "domain_xml_file": path}
	for command, reason := range map[string]string{
		"dominfo":  "libvirt_domain: reading the domain shop-db: failed to connect to the hypervisor: Cannot recv data: Connection reset by peer",
		"metadata": "libvirt_domain: reading the record of the last defined file of the domain shop-db: failed to connect to the hypervisor: Cannot recv data: Connection reset by peer",
	} {
		t.Run(command, func(t *testing.T) {
			libvirt.restart(oldestLibvirt)
			libvirt.domain("shop-db", "shut off", map[string]string{"record": sha256})
			libvirt.write("refuse_"+command, libvirtWentAway)
			for _, dryrun := range []bool{true, false} {
				r := runOfficialExtraModule(t, "libvirt_domain", inputs, dryrun)
				if r.Status != "failed" || r.Change != "unchanged" || r.Reason != reason {
					t.Fatalf("dryrun %v: %+v", dryrun, r)
				}
			}
			expectLibvirtCalls(t, libvirt.changingCalls(), nil)
		})
	}

	// The reads of absent.
	for command, reason := range map[string]string{
		"snapshot-list":   "libvirt_domain: listing the snapshots of the domain shop-db: failed to connect to the hypervisor: Cannot recv data: Connection reset by peer",
		"checkpoint-list": "libvirt_domain: listing the checkpoints of the domain shop-db: failed to connect to the hypervisor: Cannot recv data: Connection reset by peer",
	} {
		libvirt.restart(oldestLibvirt)
		libvirt.domain("shop-db", "shut off", nil)
		libvirt.write("refuse_"+command, libvirtWentAway)
		expectLibvirtResult(t, runOfficialExtraModule(t, "libvirt_domain", map[string]any{"name": "shop-db", "state": "absent"}, false), "failed", "unchanged", reason, false)
		expectLibvirtCalls(t, libvirt.changingCalls(), nil)
	}

	// virsh answers for another domain than the one it was asked for.
	libvirt.restart(newestLibvirt)
	libvirt.domain("shop-db", "running", map[string]string{"answers_as": "web-1"})
	expectLibvirtCannotSucceed(t, runOfficialExtraModule(t, "libvirt_domain", map[string]any{"name": "shop-db", "state": "absent"}, false), "unchanged",
		"libvirt_domain: reading the domain shop-db: virsh answered for the domain web-1, so it does not take shop-db for a name; give the domain another name")
	expectLibvirtCalls(t, libvirt.changingCalls(), nil)
}

// The file of domain_xml_file.
func TestLibvirtDomainReadsTheXMLFile(t *testing.T) {
	t.Parallel()
	libvirt := newLibvirtStandIn(t, newestLibvirt)
	directory := t.TempDir()
	missing := filepath.Join(directory, "shop-db.xml")
	inputs := map[string]any{"name": "shop-db", "state": "running", "domain_xml_file": missing}

	// An earlier file task writes the file: a dryrun cannot check the task yet.
	reason := "libvirt_domain: the file " + missing + " of domain_xml_file does not exist; write it in an earlier node task, for example with file"
	if r := runOfficialExtraModule(t, "libvirt_domain", inputs, true); r.Status != "not_checked" || r.Reason != reason {
		t.Fatalf("dryrun: %+v", r)
	}
	if r := runOfficialExtraModule(t, "libvirt_domain", inputs, false); r.Status != "failed" || r.FailureKind != "missing_prerequisite" || r.Reason != reason {
		t.Fatalf("run: %+v", r)
	}

	inputs["domain_xml_file"] = directory
	if r := runOfficialExtraModule(t, "libvirt_domain", inputs, false); r.FailureKind != "config" ||
		r.Reason != "libvirt_domain.domain_xml_file: "+directory+" is a directory; give the path of the file that holds the domain's libvirt XML" {
		t.Fatalf("%+v", r)
	}

	// The file of another domain would define or replace that other domain.
	other, _ := libvirtDomainFile(t, strings.Replace(libvirtDomainXML, "shop-db", "shop-web", 1))
	inputs["domain_xml_file"] = other
	for _, dryrun := range []bool{true, false} {
		if r := runOfficialExtraModule(t, "libvirt_domain", inputs, dryrun); r.FailureKind != "config" ||
			r.Reason != "libvirt_domain.name: the file "+other+" does not hold <name>shop-db</name>; name must be the <name> of the domain in the XML file" {
			t.Fatalf("%+v", r)
		}
	}

	// An existing domain needs its UUID before </domain>.
	libvirt.domain("shop-db", "shut off", nil)
	open, _ := libvirtDomainFile(t, "<domain type='kvm'>\n  <name>shop-db</name>\n")
	inputs["domain_xml_file"] = open
	if r := runOfficialExtraModule(t, "libvirt_domain", inputs, false); r.FailureKind != "config" ||
		r.Reason != "libvirt_domain.domain_xml_file: the file "+open+" holds no </domain>, so it is not the libvirt XML of one domain" {
		t.Fatalf("%+v", r)
	}

	binary := filepath.Join(directory, "binary.xml")
	if err := os.WriteFile(binary, []byte("<name>shop-db</name>\xff\xfe"), 0600); err != nil {
		t.Fatal(err)
	}
	inputs["domain_xml_file"] = binary
	expectLibvirtCannotSucceed(t, runOfficialExtraModule(t, "libvirt_domain", inputs, false), "unchanged",
		"libvirt_domain.domain_xml_file: MMX cannot read the file as text: "+binary+" is not a UTF-8 text file")
	expectLibvirtCalls(t, libvirt.changingCalls(), nil)
}

// libvirt refuses a file. A newer libvirt quotes the line that it could not
// read, and a domain's XML can hold a password: only virsh's error lines
// reach the reason. A define that fails for another reason is an ordinary
// failure, which MMX tries again.
func TestLibvirtDomainReportsARefusedFileWithoutItsLines(t *testing.T) {
	t.Parallel()
	libvirt := newLibvirtStandIn(t, newestLibvirt)
	for version, refusal := range map[string]string{
		oldestLibvirt: "error: Failed to define domain from /dev/stdin\nerror: (domain_definition):4: Premature end of data in tag domain line 1\n\n",
		newestLibvirt: "error: Failed to define domain from /dev/stdin\nerror: at line 4: Premature end of data in tag domain line 1\n  <graphics type='vnc' passwd='sentinel-password'/>\n-------------------------------------------------^",
	} {
		t.Run(version, func(t *testing.T) {
			libvirt.restart(version)
			libvirt.write("refuse_define", refusal)
			path, _ := libvirtDomainFile(t, "<domain type='kvm'>\n  <name>shop-db</name>\n  <graphics type='vnc' passwd='sentinel-password'/>\n")
			r := runOfficialExtraModule(t, "libvirt_domain", map[string]any{"name": "shop-db", "state": "running", "domain_xml_file": path, "start_at_boot": true}, false)
			message := "at line 4: Premature end of data in tag domain line 1"
			if version == oldestLibvirt {
				message = "(domain_definition):4: Premature end of data in tag domain line 1"
			}
			// libvirt refuses the same file again, so MMX tries once. MMX
			// records every changing command that ran, also one that failed.
			expectLibvirtCannotSucceed(t, r, "changed", "libvirt_domain: defining the domain shop-db from "+path+": Failed to define domain from /dev/stdin: "+message)
			expectLibvirtCalls(t, libvirt.changingCalls(), []string{"define /dev/stdin"})
			if strings.Contains(r.Reason+r.Diff+r.Stderr+r.Stdout, "sentinel-password") {
				t.Fatalf("the result holds a line of the file: %+v", r)
			}
		})
	}
	// "Failed to define domain from" is the first line of every failed
	// define. Only a second line that shows an error of the XML makes the
	// task one that cannot succeed; a define that failed for another reason
	// is tried again.
	path, _ := libvirtDomainFile(t, libvirtDomainXML)
	inputs := map[string]any{"name": "shop-db", "state": "running", "domain_xml_file": path}
	for refusal, xml := range map[string]bool{
		"XML error: an os <type> must be specified":                                        true,
		"missing name information":                                                         true,
		"XML error: Invalid value 'many' for element or attribute './memory[1]'":           true,
		"unsupported configuration: invalid domain type nosuch":                            true,
		"unsupported configuration: UEFI requires ACPI on this architecture":               true,
		"XML error: expecting root element of 'domain', not 'network'":                     true,
		"at line 1: Document is empty":                                                     true,
		"operation failed: domain 'shop-db' already exists with uuid " + libvirtDomainUUID: true,
		"Cannot check QEMU binary /usr/bin/nosuch-emulator: No such file or directory":     false,
	} {
		libvirt.restart(newestLibvirt)
		libvirt.write("refuse_define", "error: Failed to define domain from /dev/stdin\nerror: "+refusal)
		r := runOfficialExtraModule(t, "libvirt_domain", inputs, false)
		reason := "libvirt_domain: defining the domain shop-db from " + path + ": Failed to define domain from /dev/stdin: " + refusal
		if xml {
			expectLibvirtCannotSucceed(t, r, "changed", reason)
		} else if expectLibvirtResult(t, r, "failed", "changed", reason, false); r.FailureKind != "" || r.NoOtherTry {
			t.Fatalf("%s: MMX would not try again: %+v", refusal, r)
		}
	}
	libvirt.restart(newestLibvirt)
	libvirt.write("refuse_define", libvirtWentAway)
	r := runOfficialExtraModule(t, "libvirt_domain", inputs, false)
	expectLibvirtResult(t, r, "failed", "changed", "libvirt_domain: defining the domain shop-db from "+path+
		": failed to connect to the hypervisor: Cannot recv data: Connection reset by peer", false)
	if r.FailureKind != "" || r.NoOtherTry {
		t.Fatalf("MMX would not try again: %+v", r)
	}
}

// A step that fails after the task created the domain is not tried again:
// the next try would find the domain and report created: false for a domain
// that this task created. The result is failed and changed, keeps created:
// true, and says what the task did before. The next run does what is
// missing.
func TestLibvirtDomainFailsAfterAnEarlierChangeAndTheNextRunFinishes(t *testing.T) {
	t.Parallel()
	libvirt := newLibvirtStandIn(t, newestLibvirt)
	path, sha256 := libvirtDomainFile(t, libvirtDomainXML)
	inputs := map[string]any{"name": "shop-db", "state": "running", "domain_xml_file": path, "start_at_boot": true}
	noImage := "Failed to start domain 'shop-db': Cannot access storage file '/var/lib/libvirt/images/shop-db.qcow2' (as uid:107, gid:107): No such file or directory"
	wentAway := "failed to connect to the hypervisor: Cannot recv data: Connection reset by peer"
	created := ". Before that, the task created the domain shop-db from " + path
	notTriedAgain := func(r protocol.Result) {
		t.Helper()
		if !r.NoOtherTry || r.FailureKind != "" {
			t.Fatalf("MMX would try this failure again, and the next try would report created: false: %+v", r)
		}
	}

	// The start fails.
	libvirt.write("refuse_start", "error: Failed to start domain 'shop-db'\nerror: Cannot access storage file '/var/lib/libvirt/images/shop-db.qcow2' (as uid:107, gid:107): No such file or directory")
	r := runOfficialExtraModule(t, "libvirt_domain", inputs, false)
	expectLibvirtResult(t, r, "failed", "changed", "libvirt_domain: starting the domain shop-db: "+noImage+created+"; set the domain shop-db to start at boot", true)
	notTriedAgain(r)
	expectLibvirtCalls(t, libvirt.changingCalls(), []string{"define /dev/stdin", libvirtRecordCommand(sha256), "autostart shop-db", "start shop-db"})

	// The domain is there now: a start that fails again was created by no
	// one in this task, and MMX tries it again as every failed command.
	before := len(libvirt.changingCalls())
	r = runOfficialExtraModule(t, "libvirt_domain", inputs, false)
	expectLibvirtResult(t, r, "failed", "changed", "libvirt_domain: starting the domain shop-db: "+noImage, false)
	if r.NoOtherTry {
		t.Fatal("a start of a domain that the task did not create is tried again")
	}
	if err := os.Remove(filepath.Join(libvirt.directory, "refuse_start")); err != nil {
		t.Fatal(err)
	}
	expectLibvirtResult(t, runOfficialExtraModule(t, "libvirt_domain", inputs, false), "succeeded", "changed", "started the domain shop-db", false)
	expectLibvirtCalls(t, libvirt.changingCalls()[before:], []string{"start shop-db", "start shop-db"})

	// virsh autostart fails after the task created the domain. The next run
	// sets the start at boot and starts the domain, and does not define it
	// again.
	libvirt.restart(newestLibvirt)
	libvirt.write("refuse_autostart", libvirtWentAway)
	r = runOfficialExtraModule(t, "libvirt_domain", inputs, false)
	expectLibvirtResult(t, r, "failed", "changed", "libvirt_domain: setting whether the domain shop-db starts at boot: "+wentAway+created, true)
	notTriedAgain(r)
	if err := os.Remove(filepath.Join(libvirt.directory, "refuse_autostart")); err != nil {
		t.Fatal(err)
	}
	before = len(libvirt.changingCalls())
	expectLibvirtResult(t, runOfficialExtraModule(t, "libvirt_domain", inputs, false), "succeeded", "changed",
		"set the domain shop-db to start at boot; started the domain shop-db", false)
	expectLibvirtCalls(t, libvirt.changingCalls()[before:], []string{"autostart shop-db", "start shop-db"})

	// The record fails after the task created the domain. The next run finds
	// no record and defines the domain again.
	libvirt.restart(newestLibvirt)
	libvirt.write("refuse_metadata", libvirtWentAway)
	r = runOfficialExtraModule(t, "libvirt_domain", inputs, false)
	expectLibvirtResult(t, r, "failed", "changed", "libvirt_domain: recording the defined file on the domain shop-db: "+wentAway+created, true)
	notTriedAgain(r)
	if err := os.Remove(filepath.Join(libvirt.directory, "refuse_metadata")); err != nil {
		t.Fatal(err)
	}
	before = len(libvirt.changingCalls())
	expectLibvirtResult(t, runOfficialExtraModule(t, "libvirt_domain", inputs, false), "succeeded", "changed",
		"defined the domain shop-db again from "+path+"; set the domain shop-db to start at boot; started the domain shop-db", false)
	expectLibvirtCalls(t, libvirt.changingCalls()[before:], []string{"define /dev/stdin", libvirtRecordCommand(sha256), "autostart shop-db", "start shop-db"})

	// The read after the define fails: the task created the domain before.
	libvirt.restart(newestLibvirt)
	libvirt.write("refuse_dominfo", libvirtWentAway)
	r = runOfficialExtraModule(t, "libvirt_domain", inputs, false)
	expectLibvirtResult(t, r, "failed", "changed", "libvirt_domain: reading the domain shop-db: "+wentAway+created, true)
	notTriedAgain(r)

	// A domain that libvirt has already: a define again and then a start
	// that fails. The task created nothing, so MMX tries again, and the next
	// try starts the domain without another define.
	libvirt.restart(newestLibvirt)
	libvirt.domain("shop-db", "shut off", nil)
	libvirt.write("refuse_start", libvirtWentAway)
	r = runOfficialExtraModule(t, "libvirt_domain", inputs, false)
	expectLibvirtResult(t, r, "failed", "changed", "libvirt_domain: starting the domain shop-db: "+wentAway, false)
	if r.NoOtherTry {
		t.Fatal("a failure after a define of a domain that existed is tried again")
	}
	if err := os.Remove(filepath.Join(libvirt.directory, "refuse_start")); err != nil {
		t.Fatal(err)
	}
	before = len(libvirt.changingCalls())
	expectLibvirtResult(t, runOfficialExtraModule(t, "libvirt_domain", inputs, false), "succeeded", "changed", "started the domain shop-db", false)
	expectLibvirtCalls(t, libvirt.changingCalls()[before:], []string{"start shop-db"})

	// Each of the other changing commands that libvirt refuses.
	for command, c := range map[string]struct {
		state  string
		inputs map[string]any
		reason string
	}{
		"autostart": {"shut off", map[string]any{"state": "stopped", "start_at_boot": true}, "libvirt_domain: setting whether the domain shop-db starts at boot: failed to connect to the hypervisor: Cannot recv data: Connection reset by peer"},
		"resume":    {"paused", map[string]any{"state": "running"}, "libvirt_domain: resuming the domain shop-db: failed to connect to the hypervisor: Cannot recv data: Connection reset by peer"},
		"shutdown":  {"running", map[string]any{"state": "stopped"}, "libvirt_domain: asking the domain shop-db to shut down: failed to connect to the hypervisor: Cannot recv data: Connection reset by peer"},
		"undefine":  {"shut off", map[string]any{"state": "absent"}, "libvirt_domain: removing the definition of the domain shop-db: failed to connect to the hypervisor: Cannot recv data: Connection reset by peer"},
	} {
		libvirt.restart(newestLibvirt)
		libvirt.domain("shop-db", c.state, map[string]string{"record": sha256})
		libvirt.write("refuse_"+command, libvirtWentAway)
		inputs := map[string]any{"name": "shop-db", "domain_xml_file": path}
		for key, value := range c.inputs {
			inputs[key] = value
		}
		if inputs["state"] == "absent" {
			delete(inputs, "domain_xml_file")
		}
		expectLibvirtResult(t, runOfficialExtraModule(t, "libvirt_domain", inputs, false), "failed", "changed", c.reason, false)
	}
}

// A file that names the task's domain in one place and defines another one:
// the text check passes, and libvirt's answer shows the other name.
func TestLibvirtDomainFailsWhenLibvirtDefinedAnotherDomain(t *testing.T) {
	t.Parallel()
	libvirt := newLibvirtStandIn(t, newestLibvirt)
	path, _ := libvirtDomainFile(t, "<domain type='kvm'>\n  <name>shop-web</name>\n  <description><name>shop-db</name></description>\n</domain>\n")
	r := runOfficialExtraModule(t, "libvirt_domain", map[string]any{"name": "shop-db", "state": "running", "domain_xml_file": path}, false)
	expectLibvirtCannotSucceed(t, r, "changed", "libvirt_domain: defining the domain shop-db from "+path+
		`: libvirt answered "Domain 'shop-web' defined from /dev/stdin", so the file defined another domain; name must be the <name> of the domain in the XML file`)
	expectLibvirtCalls(t, libvirt.changingCalls(), []string{"define /dev/stdin"})
}

func TestLibvirtDomainKeepsStoredDefinitionWithoutXMLFile(t *testing.T) {
	t.Parallel()
	for _, version := range []string{oldestLibvirt, newestLibvirt} {
		t.Run(version, func(t *testing.T) {
			libvirt := newLibvirtStandIn(t, version)
			libvirt.domain("shop-db", "shut off", map[string]string{"record": "keep-this-definition"})
			inputs := map[string]any{"name": "shop-db", "state": "running"}
			preview := runOfficialExtraModule(t, "libvirt_domain", inputs, true)
			expectLibvirtResult(t, preview, "succeeded", "would_change", "would start the domain shop-db", false)
			expectLibvirtCalls(t, libvirt.changingCalls(), nil)
			applied := runOfficialExtraModule(t, "libvirt_domain", inputs, false)
			expectLibvirtResult(t, applied, "succeeded", "changed", "started the domain shop-db", false)
			expectLibvirtCalls(t, libvirt.changingCalls(), []string{"start shop-db"})
			inputs["state"] = "stopped"
			inputs["wait_for_shutdown_at_most"] = "0s"
			inputs["power_off_when_shutdown_takes_longer"] = true
			stopped := runOfficialExtraModule(t, "libvirt_domain", inputs, false)
			expectLibvirtResult(t, stopped, "succeeded", "changed", "asked the domain shop-db to shut down; cut the power of the domain shop-db, which still ran after 0 seconds", false)
			expectLibvirtCalls(t, libvirt.changingCalls(), []string{"start shop-db", "shutdown shop-db", "destroy shop-db"})
			expectLibvirtResult(t, runOfficialExtraModule(t, "libvirt_domain", inputs, false), "succeeded", "unchanged", "", false)
		})
	}
}

func TestLibvirtDomainNeedsXMLFileToCreateDomain(t *testing.T) {
	t.Parallel()
	libvirt := newLibvirtStandIn(t, newestLibvirt)
	for _, state := range []string{"running", "stopped"} {
		t.Run(state, func(t *testing.T) {
			r := runOfficialExtraModule(t, "libvirt_domain", map[string]any{"name": "shop-db", "state": state}, false)
			if r.Status != "failed" || r.FailureKind != "missing_prerequisite" || r.Change != "unchanged" || r.Reason != "libvirt_domain: the domain shop-db does not exist; create it in an earlier node task with domain_xml_file, or add domain_xml_file to this task" {
				t.Fatalf("%s: %+v", state, r)
			}
		})
	}
	expectLibvirtCalls(t, libvirt.changingCalls(), nil)
}

func TestLibvirtDomainDryrunCreatesStoppedDomainThenStartsWithoutXML(t *testing.T) {
	t.Parallel()
	libvirt := newLibvirtStandIn(t, newestLibvirt)
	path, _ := libvirtDomainFile(t, libvirtDomainXML)
	tasks := []map[string]any{
		{"name": "shop-db", "state": "stopped", "domain_xml_file": path},
		{"name": "shop-db", "state": "running"},
	}
	defined := runOfficialExtraModule(t, "libvirt_domain", tasks[0], true)
	expectLibvirtResult(t, defined, "succeeded", "would_change", "would create the domain shop-db from "+path, true)
	started := runOfficialExtraModule(t, "libvirt_domain", tasks[1], true)
	if started.Status != "not_checked" || started.Change != "unknown" || started.Reason != "libvirt_domain: the domain shop-db does not exist; create it in an earlier node task with domain_xml_file, or add domain_xml_file to this task" {
		t.Fatalf("start after the earlier dryrun task would create the domain: %+v", started)
	}
	expectLibvirtCalls(t, libvirt.changingCalls(), nil)
	if libvirt.read("domains/shop-db/state") != "" {
		t.Fatal("the dryrun created the domain")
	}
	expectLibvirtResult(t, runOfficialExtraModule(t, "libvirt_domain", tasks[0], false), "succeeded", "changed", "created the domain shop-db from "+path, true)
	if libvirt.read("domains/shop-db/state") != "shut off" {
		t.Fatal("the creation task did not leave the domain stopped")
	}
	expectLibvirtResult(t, runOfficialExtraModule(t, "libvirt_domain", tasks[1], false), "succeeded", "changed", "started the domain shop-db", false)
	if libvirt.read("domains/shop-db/state") != "running" {
		t.Fatal("the later task did not start the domain")
	}
}
