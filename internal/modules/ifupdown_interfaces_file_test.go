package modules

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

func ifupdownFixture(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	oldMain, oldDirectory, oldLinks, oldVirtual := ifupdownMainFile, ifupdownDirectory, ifupdownLinksDirectory, ifupdownVirtualDirectory
	oldUser, oldGroup := ifupdownRootUser, ifupdownRootGroup
	t.Cleanup(func() {
		ifupdownMainFile, ifupdownDirectory, ifupdownLinksDirectory, ifupdownVirtualDirectory = oldMain, oldDirectory, oldLinks, oldVirtual
		ifupdownRootUser, ifupdownRootGroup = oldUser, oldGroup
	})
	ifupdownMainFile, ifupdownDirectory = filepath.Join(root, "etc/network/interfaces"), filepath.Join(root, "etc/network/interfaces.d")
	ifupdownLinksDirectory, ifupdownVirtualDirectory = filepath.Join(root, "links"), filepath.Join(root, "virtual")
	ifupdownRootUser, ifupdownRootGroup = Account(strconv.Itoa(os.Geteuid())), Account(strconv.Itoa(os.Getegid()))
	for _, directory := range []string{ifupdownDirectory, ifupdownLinksDirectory, ifupdownVirtualDirectory} {
		if err := os.MkdirAll(directory, 0755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// ifupdownStandIns keeps ifupdown's state as files up-<interface> in $dir.
// A file declared-<interface> makes ifquery find the interface in another
// file, and removes-<interface> names a path that ifdown deletes.
func ifupdownStandIns(t *testing.T) string {
	t.Helper()
	return standInPrograms(t, map[string]string{
		"systemctl": `[ "$*" = 'is-enabled networking.service' ] || exit 90
if [ -f "$dir/disabled" ]; then echo disabled; exit 1; fi
echo enabled`,
		"ifup": `case "$1" in
--version) if [ -f "$dir/version" ]; then cat "$dir/version"; else echo 'ifup version 0.8.44+deb13u1'; fi; exit 0;;
--no-act) cat > "$dir/no-act-input"
  if [ -f "$dir/fail-no-act" ]; then echo 'ifup: /dev/stdin: missing required variable: address' >&2; exit 1; fi
  echo 'ip addr add value-of-an-option'; exit 0;;
esac
for name; do :; done
if [ -f "$dir/fail-ifup" ]; then echo "ifup: failed to bring up $name" >&2; exit 1; fi
touch "$dir/up-$name"`,
		"ifdown": `for name; do :; done
rm -f "$dir/up-$name"
[ ! -f "$dir/removes-$name" ] || rm -rf "$(cat "$dir/removes-$name")"`,
		"ifquery": `case "$1" in
-i) cat > "$dir/ifquery-input"
  if [ -f "$dir/fail-syntax" ]; then echo 'ifquery: /dev/stdin:2: misplaced option' >&2; exit 1; fi
  echo lo;;
--state) [ -f "$dir/up-$2" ] && echo "$2=$2";;
*) [ -f "$dir/declared-$1" ];;
esac`,
	})
}

func ifupdownMainFileThatReadsTheDirectory() string {
	return "source " + ifupdownDirectory + "/*\n\nauto lo\niface lo inet loopback\n"
}

const ifupdownSecondCard = "auto enp6s0\niface enp6s0 inet static\n    address 10.0.0.5/24\n"

func TestIfupdownPathRuleAndInputs(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"/etc/network/interfaces", "/etc/network/interfaces.d/60-second", "/etc/network/interfaces.d/60_second"} {
		if err := ifupdownPathRule(path, "present"); err != nil {
			t.Fatal(path, err)
		}
	}
	for _, path := range []string{"/etc/network/interfaces.d/60-second.cfg", "/etc/network/interfaces.d/.hidden", "/etc/network/interfaces.d/60~old", "/etc/network/interfaces.d/a/b", "/etc/network/interfaces.d", "/etc/network/other", "/etc/network/interfaces.d/", "interfaces"} {
		if err := ifupdownPathRule(path, "present"); err == nil || !strings.Contains(err.Error(), "source-directory reads only such names") {
			t.Fatal(path, err)
		}
	}
	if err := ifupdownPathRule("/etc/network/interfaces", "absent"); err == nil || !strings.Contains(err.Error(), "cannot remove /etc/network/interfaces") {
		t.Fatal(err)
	}
	if err := ifupdownPathRule("/etc/network/interfaces.d/60-second", "absent"); err != nil {
		t.Fatal(err)
	}
	text := "auto lo\n"
	removal := &IfupdownInterfacesFile{Name: "/etc/network/interfaces.d/60-second", State: "absent", Content: &text}
	if err := removal.Prepare(t.TempDir()); err == nil || !strings.Contains(err.Error(), "accepts only path, state") {
		t.Fatal(err)
	}
	if err := (&IfupdownInterfacesFile{Name: "/etc/network/interfaces"}).Prepare(t.TempDir()); err == nil {
		t.Fatal("accepted a file without content or template")
	}
	file := (&IfupdownInterfacesFile{Name: "/etc/network/interfaces", Content: &text}).file()
	if file.Path != "/etc/network/interfaces" || file.Mode != "" || file.Owner != "0" || file.Group != "0" {
		t.Fatal(file)
	}
}

func TestIfupdownReadsLogicalLinesClassesFlagsAndFileLevelLines(t *testing.T) {
	t.Parallel()
	file := readIfupdownFile(`# a comment
source /etc/network/interfaces.d/*
source-directory   interfaces.d
rename enp9s0=uplink

allow-auto enp6s0
auto enp6s0
iface enp6s0 inet static \
    inherits base
  address 10.0.0.5/24   # not a comment
    # an indented comment
  up ip route add 198.51.100.0/24 \
     via 10.0.0.1
allow-hotplug enp7s0
no-auto-down enp6s0
no-scripts enp6s0
  mtu 9000
iface base inet static
  netmask 255.255.255.0
`)
	if !reflect.DeepEqual(file.fileLevelLines, []string{"source /etc/network/interfaces.d/*", "source-directory interfaces.d", "rename enp9s0=uplink"}) ||
		!reflect.DeepEqual(file.sourcePatterns, []string{"/etc/network/interfaces.d/*"}) || !reflect.DeepEqual(file.sourceDirectories, []string{"interfaces.d"}) {
		t.Fatal(file)
	}
	if !reflect.DeepEqual(file.order, []string{"enp6s0", "enp7s0", "base"}) || file.hasMapping {
		t.Fatal(file.order)
	}
	second := file.interfaces["enp6s0"]
	// An option after an auto or no-scripts line belongs to no stanza; ifquery refuses it.
	if !reflect.DeepEqual(second.stanzaLines, []string{"iface enp6s0 inet static inherits base", "address 10.0.0.5/24 # not a comment", "up ip route add 198.51.100.0/24 via 10.0.0.1"}) {
		t.Fatal(second.stanzaLines)
	}
	if !reflect.DeepEqual(second.bases, []string{"base"}) || !reflect.DeepEqual(second.classes, []string{"auto"}) || !reflect.DeepEqual(second.flags, []string{"no-auto-down", "no-scripts"}) {
		t.Fatal(second)
	}
	if file.declares("enp7s0") || file.bringUpClass("enp7s0") != "allow-hotplug" || !file.declares("base") {
		t.Fatal(file.interfaces["enp7s0"])
	}
	if !readIfupdownFile("mapping eth0\n  script /usr/local/sbin/map-scheme\n").hasMapping {
		t.Fatal("missed a mapping stanza")
	}
}

func TestIfupdownInterfacesThatChange(t *testing.T) {
	t.Parallel()
	base := "auto lo\niface lo inet loopback\nauto enp6s0\niface enp6s0 inet static\n  address 10.0.0.5/24\n"
	for _, test := range []struct {
		name, previous, proposed string
		takeDown                 []string
		bringUp                  []ifupdownInterfaceToBringUp
		takeDownOnRestore        []string
		bringUpOnRestore         []ifupdownInterfaceToBringUp
	}{
		{"added", "", base, []string{}, []ifupdownInterfaceToBringUp{{"enp6s0", "auto"}}, []string{"enp6s0"}, []ifupdownInterfaceToBringUp{}},
		{"removed", base, "", []string{"enp6s0"}, []ifupdownInterfaceToBringUp{}, []string{}, []ifupdownInterfaceToBringUp{{"enp6s0", "auto"}}},
		{"changed", base, strings.Replace(base, "10.0.0.5", "10.0.0.6", 1), []string{"enp6s0"}, []ifupdownInterfaceToBringUp{{"enp6s0", "auto"}}, []string{"enp6s0"}, []ifupdownInterfaceToBringUp{{"enp6s0", "auto"}}},
		{"comment and spacing only", base, "# new comment\n" + strings.Replace(base, "  address", "\taddress  ", 1), []string{}, []ifupdownInterfaceToBringUp{}, []string{}, []ifupdownInterfaceToBringUp{}},
		{"class only", base, strings.Replace(base, "auto enp6s0", "allow-hotplug enp6s0", 1), []string{"enp6s0"}, []ifupdownInterfaceToBringUp{{"enp6s0", "allow-hotplug"}}, []string{"enp6s0"}, []ifupdownInterfaceToBringUp{{"enp6s0", "auto"}}},
		{"no class any more", base, strings.Replace(base, "auto enp6s0\n", "", 1), []string{"enp6s0"}, []ifupdownInterfaceToBringUp{}, []string{"enp6s0"}, []ifupdownInterfaceToBringUp{{"enp6s0", "auto"}}},
		{"lo left alone", base, strings.Replace(base, "iface lo inet loopback", "iface lo inet loopback\n  mtu 1500", 1), []string{}, []ifupdownInterfaceToBringUp{}, []string{}, []ifupdownInterfaceToBringUp{}},
		{"inherited base changed", base + "iface base inet static\n  netmask 255.255.255.0\nauto enp7s0\niface enp7s0 inet static inherits base\n  address 10.0.1.5\n",
			base + "iface base inet static\n  netmask 255.255.0.0\nauto enp7s0\niface enp7s0 inet static inherits base\n  address 10.0.1.5\n",
			[]string{"enp7s0", "base"}, []ifupdownInterfaceToBringUp{{"enp7s0", "auto"}}, []string{"enp7s0", "base"}, []ifupdownInterfaceToBringUp{{"enp7s0", "auto"}}},
		{"order of first appearance", "", "auto enp6s0 enp6s0.100\niface enp6s0.100 inet static\n  address 10.0.100.5/24\niface enp6s0 inet manual\n",
			[]string{}, []ifupdownInterfaceToBringUp{{"enp6s0", "auto"}, {"enp6s0.100", "auto"}}, []string{"enp6s0.100", "enp6s0"}, []ifupdownInterfaceToBringUp{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := ifupdownInterfacesThatChange(readIfupdownFile(test.previous), readIfupdownFile(test.proposed))
			if !reflect.DeepEqual(data.TakeDownWithPreviousFile, test.takeDown) || !reflect.DeepEqual(data.BringUpWithProposedFile, test.bringUp) ||
				!reflect.DeepEqual(data.TakeDownWithProposedFileOnRestore, test.takeDownOnRestore) || !reflect.DeepEqual(data.BringUpWithPreviousFileOnRestore, test.bringUpOnRestore) {
				t.Fatalf("%+v", data)
			}
		})
	}
}

// requireIfupdownRefusal runs the module as a run and as a dryrun and
// requires the same refusal before any change.
func requireIfupdownRefusal(t *testing.T, path, proposed, reason string) {
	t.Helper()
	before := readState(t, path)
	for _, dryrun := range []bool{false, true} {
		service := &networkdOwedRestoreForTest{}
		input := &IfupdownInterfacesFile{Name: path, Content: &proposed}
		if proposed == "" {
			input.State, input.Content = "absent", nil
		}
		run := Runtime{Dryrun: dryrun, BeforeMutation: func(resource string) error {
			t.Fatalf("refused task reached mutation: %s", resource)
			return nil
		}}
		result := input.Run(testContext(t), NodeTask{Runtime: run, OwedRestores: service})
		if result.Status != "failed" || result.Change != "unchanged" || !result.NoOtherTry || service.applied || !strings.Contains(result.Reason, reason) {
			t.Fatalf("dryrun %t: %+v", dryrun, result)
		}
		if readState(t, path) != before {
			t.Fatal("changed the file before the refusal")
		}
	}
}

func TestIfupdownRefusesBeforeAnyChange(t *testing.T) {
	t.Run("mapping in the proposed file", func(t *testing.T) {
		ifupdownFixture(t)
		ifupdownStandIns(t)
		writeState(t, ifupdownMainFile, ifupdownMainFileThatReadsTheDirectory())
		requireIfupdownRefusal(t, filepath.Join(ifupdownDirectory, "60-second"), "mapping enp6s0\n  script /usr/local/sbin/choose\n", "uses a mapping stanza; MMX cannot know which interface to take down and bring up until the mapping script runs")
	})
	t.Run("mapping in the existing file", func(t *testing.T) {
		ifupdownFixture(t)
		ifupdownStandIns(t)
		writeState(t, ifupdownMainFile, ifupdownMainFileThatReadsTheDirectory())
		path := filepath.Join(ifupdownDirectory, "60-second")
		writeState(t, path, "mapping enp6s0\n  script /usr/local/sbin/choose\n")
		requireIfupdownRefusal(t, path, "", "uses a mapping stanza")
	})
	t.Run("changed source line of the main file", func(t *testing.T) {
		ifupdownFixture(t)
		ifupdownStandIns(t)
		writeState(t, ifupdownMainFile, ifupdownMainFileThatReadsTheDirectory())
		requireIfupdownRefusal(t, ifupdownMainFile, "auto lo\niface lo inet loopback\n", "the proposed file changes source, source-directory, or rename lines")
	})
	t.Run("source line in a new interfaces.d file", func(t *testing.T) {
		ifupdownFixture(t)
		ifupdownStandIns(t)
		writeState(t, ifupdownMainFile, ifupdownMainFileThatReadsTheDirectory())
		requireIfupdownRefusal(t, filepath.Join(ifupdownDirectory, "60-second"), "source /etc/network/other\n"+ifupdownSecondCard, "leave the source, source-directory, and rename lines unchanged")
	})
	for _, main := range []string{"", "auto lo\niface lo inet loopback\n", "source " + ifupdownDirectory + "/50-*\n", "source-directory /etc/network/other.d\n"} {
		t.Run("not read by "+strconv.Quote(main), func(t *testing.T) {
			ifupdownFixture(t)
			ifupdownStandIns(t)
			if main != "" {
				writeState(t, ifupdownMainFile, strings.ReplaceAll(main, "/etc/network/other.d", filepath.Join(filepath.Dir(ifupdownMainFile), "other.d")))
			}
			path := filepath.Join(ifupdownDirectory, "60-second")
			requireIfupdownRefusal(t, path, ifupdownSecondCard, "Cannot change "+path+": the main ifupdown file "+ifupdownMainFile+" does not include the proposed file; add an include line such as `source "+ifupdownDirectory+"/*` to the main ifupdown file.")
		})
	}
	t.Run("an interface that is up with its auto line in another file", func(t *testing.T) {
		ifupdownFixture(t)
		tools := ifupdownStandIns(t)
		writeState(t, ifupdownMainFile, ifupdownMainFileThatReadsTheDirectory()+"auto enp6s0\n")
		path := filepath.Join(ifupdownDirectory, "60-second")
		writeState(t, path, "iface enp6s0 inet static\n    address 10.0.0.5/24\n")
		writeState(t, filepath.Join(tools, "up-enp6s0"), "")
		requireIfupdownRefusal(t, path, "iface enp6s0 inet static\n    address 10.0.0.6/24\n", "Cannot change "+path+": interface enp6s0 is up, but the file has no auto or allow-hotplug line for the interface, so a restore could not bring the interface up again")
	})
	t.Run("an interface name ifup cannot take", func(t *testing.T) {
		ifupdownFixture(t)
		ifupdownStandIns(t)
		requireIfupdownRefusal(t, ifupdownMainFile, "auto a-name-longer-than-15\niface a-name-longer-than-15 inet manual\n", `interface name "a-name-longer-than-15" cannot be taken down and brought up by MMX`)
	})
}

func TestIfupdownMainFileReadsAnInterfacesDirectoryFile(t *testing.T) {
	ifupdownFixture(t)
	path := filepath.Join(ifupdownDirectory, "60-second")
	for _, main := range []string{"source " + ifupdownDirectory + "/*\n", "source interfaces.d/*\n", "source " + ifupdownDirectory + "/60-*\n", "source-directory " + ifupdownDirectory + "\n", "source-directory interfaces.d/\n"} {
		if !readIfupdownFile(main).reads(path) {
			t.Fatal(main)
		}
	}
	// Sources inside sourced files are not followed.
	writeState(t, filepath.Join(ifupdownDirectory, "10-sources"), "source "+path+"\n")
	if readIfupdownFile("source " + filepath.Join(ifupdownDirectory, "10-sources") + "\n").reads(path) {
		t.Fatal("followed a nested source line")
	}
}

func TestIfupdownNativeChecksReadTheCandidateAndNameThePath(t *testing.T) {
	for _, test := range []struct{ failure, reason string }{
		{"fail-syntax", "ifupdown does not accept the proposed %s: ifquery: %s:2: misplaced option."},
		{"fail-no-act", "ifupdown does not accept the proposed %s: ifup: %s: missing required variable: address."},
	} {
		t.Run(test.failure, func(t *testing.T) {
			ifupdownFixture(t)
			tools := ifupdownStandIns(t)
			writeState(t, ifupdownMainFile, ifupdownMainFileThatReadsTheDirectory())
			writeState(t, filepath.Join(tools, test.failure), "")
			path := filepath.Join(ifupdownDirectory, "60-second")
			requireIfupdownRefusal(t, path, ifupdownSecondCard, strings.ReplaceAll(test.reason, "%s", path))
			if readState(t, filepath.Join(tools, "ifquery-input")) != ifupdownSecondCard {
				t.Fatal("ifquery did not read the candidate on standard input")
			}
			if test.failure == "fail-no-act" && readState(t, filepath.Join(tools, "no-act-input")) != ifupdownSecondCard {
				t.Fatal("ifup --no-act did not read the candidate on standard input")
			}
		})
	}
}

func TestIfupdownPrerequisites(t *testing.T) {
	ifupdownFixture(t)
	writeState(t, ifupdownMainFile, ifupdownMainFileThatReadsTheDirectory())
	input := &IfupdownInterfacesFile{Name: filepath.Join(ifupdownDirectory, "60-second"), Content: func() *string { s := ifupdownSecondCard; return &s }()}
	setWorkerEnvironment(t, "PATH", t.TempDir())
	for _, dryrun := range []bool{true, false} {
		result := input.Run(testContext(t), NodeTask{Runtime: Runtime{Dryrun: dryrun}, OwedRestores: &networkdOwedRestoreForTest{}})
		want := "failed"
		if dryrun {
			want = "not_checked"
		}
		if result.Status != want || result.Reason != "Cannot change "+input.Name+": "+missingProgramError("ifup").Error() {
			t.Fatal(result)
		}
	}
	tools := ifupdownStandIns(t)
	writeState(t, filepath.Join(tools, "disabled"), "")
	for _, dryrun := range []bool{true, false} {
		result := input.Run(testContext(t), NodeTask{Runtime: Runtime{Dryrun: dryrun}, OwedRestores: &networkdOwedRestoreForTest{}})
		if (result.Status == "not_checked") != dryrun || result.Reason != "Cannot change "+input.Name+": networking.service is not enabled, so ifupdown would not apply the network configuration file at boot; enable networking.service." {
			t.Fatal(result)
		}
	}
	os.Remove(filepath.Join(tools, "disabled"))
	writeState(t, filepath.Join(tools, "version"), "ifupdown2:3.0.0-1\n")
	requireIfupdownRefusal(t, input.Name, ifupdownSecondCard, `ifupdown_interfaces_file needs ifupdown, the Debian package; this node's ifup reports "ifupdown2:3.0.0-1".`)
}

func TestIfupdownDryrunWritesNothingAndListsInterfaces(t *testing.T) {
	ifupdownFixture(t)
	tools := ifupdownStandIns(t)
	writeState(t, ifupdownMainFile, ifupdownMainFileThatReadsTheDirectory())
	path := filepath.Join(ifupdownDirectory, "60-second")
	text := ifupdownSecondCard
	input := &IfupdownInterfacesFile{Name: path, Content: &text}
	service := &networkdOwedRestoreForTest{}
	for _, test := range []struct{ action, existing, reason string }{
		{"write", "", "Would write " + path + " and activate it. ifupdown's acceptance and reachability after activation were not checked. Would bring up: enp6s0."},
		{"replace", strings.Replace(ifupdownSecondCard, "10.0.0.5", "10.0.0.4", 1), "Would replace " + path + " and activate it. ifupdown's acceptance and reachability after activation were not checked. Would take down: enp6s0. Would bring up: enp6s0."},
		{"remove", ifupdownSecondCard, "Would remove " + path + " and its running configuration. ifupdown's acceptance and reachability after activation were not checked. Would take down: enp6s0."},
	} {
		if test.existing != "" {
			writeState(t, path, test.existing)
		}
		if test.action == "remove" {
			input.State, input.Content = "absent", nil
		}
		result := input.Run(testContext(t), NodeTask{Runtime: noMutation(t), OwedRestores: service, Scope: value.Scope{}})
		if result.Status != "succeeded" || result.Change != "would_change" || result.Reason != test.reason || result.FileChange == nil || service.applied {
			t.Fatal(result)
		}
		for _, call := range takeCalls(t, tools) {
			if (strings.HasPrefix(call, "ifup ") && !strings.HasPrefix(call, "ifup --version") && !strings.HasPrefix(call, "ifup --no-act ")) || strings.HasPrefix(call, "ifdown") {
				t.Fatal("dryrun ran a changing command", call)
			}
		}
		if readState(t, path) != test.existing {
			t.Fatal("dryrun changed the file")
		}
	}
	// An unchanged file runs no ifupdown command at all.
	writeState(t, path, ifupdownSecondCard)
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	input.State, input.Content = "", &text
	for _, dryrun := range []bool{false, true} {
		result := input.Run(testContext(t), NodeTask{Runtime: Runtime{Dryrun: dryrun}, OwedRestores: service})
		if result.Status != "succeeded" || result.Change != "unchanged" || result.Reason != "Network configuration file "+path+" is unchanged; no activation or confirmation wait was needed." {
			t.Fatal(result)
		}
	}
	if calls := takeCalls(t, tools); len(calls) != 0 {
		t.Fatal(calls)
	}
}

type ifupdownApplyInspection struct {
	t           *testing.T
	destination string
	data        string
}

func (s *ifupdownApplyInspection) CheckChangeAllowed(context.Context, string) error { return nil }
func (s *ifupdownApplyInspection) ApplyChange(ctx context.Context, change ChangeWithOwedRestore) protocol.Result {
	if change.RestoreConfirmationWait != 3*time.Second || change.PreparedFile.DestinationFile() != s.destination || !change.PreparedFile.InstallsAFile() {
		s.t.Fatal(change)
	}
	s.data = string(change.ModuleRestoreData)
	directory := s.t.TempDir()
	if _, err := change.PreparedFile.SavePreviousFile(ctx, filepath.Join(directory, "previous")); err != nil {
		s.t.Fatal(err)
	}
	if err := change.PreparedFile.SaveProposedFile(ctx, filepath.Join(directory, "proposed")); err != nil {
		s.t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(directory, "proposed")); err != nil || info.Mode().Perm() != 0600 || readState(s.t, filepath.Join(directory, "proposed")) != "auto enp6s0\niface enp6s0 inet static\n    address 10.0.0.6/24\n" {
		s.t.Fatal("proposed copy", info, err)
	}
	return change.PreparedFile.Install(ctx)
}

func TestIfupdownRestoreDataHoldsInterfaceNamesOnly(t *testing.T) {
	ifupdownFixture(t)
	tools := ifupdownStandIns(t)
	writeState(t, ifupdownMainFile, ifupdownMainFileThatReadsTheDirectory())
	path := filepath.Join(ifupdownDirectory, "60-second")
	writeState(t, path, ifupdownSecondCard+"allow-hotplug enp7s0\niface enp7s0 inet dhcp\n")
	text := strings.Replace(ifupdownSecondCard, "10.0.0.5", "10.0.0.6", 1)
	input := &IfupdownInterfacesFile{Name: path, Content: &text, RestoreUnlessControllerConfirmsWithin: "3s"}
	service := &ifupdownApplyInspection{t: t, destination: path}
	result := input.Run(testContext(t), NodeTask{OwedRestores: service, Scope: value.Scope{}})
	if result.Status != "succeeded" || result.Change != "changed" || readState(t, path) != text {
		t.Fatal(result)
	}
	want := `{"interfaces_to_take_down_with_previous_file":["enp7s0","enp6s0"],"interfaces_to_bring_up_with_proposed_file":[{"interface":"enp6s0","listed_in":"auto"}],` +
		`"interfaces_to_take_down_with_proposed_file_on_restore":["enp6s0"],"interfaces_to_bring_up_with_previous_file_on_restore":[{"interface":"enp6s0","listed_in":"auto"},{"interface":"enp7s0","listed_in":"allow-hotplug"}]}`
	if service.data != want {
		t.Fatal(service.data)
	}
	// Run only checks; the activation process changes the interfaces.
	for _, call := range takeCalls(t, tools) {
		if strings.HasPrefix(call, "ifdown") || (strings.HasPrefix(call, "ifup ") && !strings.HasPrefix(call, "ifup --version") && !strings.HasPrefix(call, "ifup --no-act ")) {
			t.Fatal(call)
		}
	}
}

func ifupdownTestActivation(t *testing.T, data ifupdownRestoreData) (*ifupdownActivation, string, string) {
	t.Helper()
	saved, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	activation, err := newIfupdownActivation(filepath.Join(ifupdownDirectory, "60-second"), saved)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	previous, proposed := filepath.Join(directory, "previous_file_contents"), filepath.Join(directory, "proposed_file_contents")
	activation.(*ifupdownActivation).UseSavedFiles(previous, proposed)
	return activation.(*ifupdownActivation), previous, proposed
}

func ifupdownTestDevice(t *testing.T, name string, virtual bool) {
	t.Helper()
	directory := filepath.Join(filepath.Dir(ifupdownVirtualDirectory), "physical", name)
	if virtual {
		directory = filepath.Join(ifupdownVirtualDirectory, name)
	}
	if err := os.MkdirAll(directory, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(directory, filepath.Join(ifupdownLinksDirectory, name)); err != nil {
		t.Fatal(err)
	}
}

func runIfupdownActivation(t *testing.T, activation *ifupdownActivation, restoring bool) error {
	t.Helper()
	for _, step := range []func(context.Context, Runtime, bool) error{activation.PrepareActivation, activation.Activate, activation.RemoveObsoleteVirtualDevices, activation.CheckActivatedConfiguration} {
		if err := step(testContext(t), Runtime{}, restoring); err != nil {
			return err
		}
	}
	return nil
}

func TestIfupdownActivationCommandsInOrder(t *testing.T) {
	ifupdownFixture(t)
	tools := ifupdownStandIns(t)
	data := ifupdownRestoreData{
		TakeDownWithPreviousFile:          []string{"enp6s0.100", "enp6s0"},
		BringUpWithProposedFile:           []ifupdownInterfaceToBringUp{{"enp6s0", "auto"}, {"enp7s0", "allow-hotplug"}},
		TakeDownWithProposedFileOnRestore: []string{"enp7s0", "enp6s0"},
		BringUpWithPreviousFileOnRestore:  []ifupdownInterfaceToBringUp{{"enp6s0", "auto"}, {"enp6s0.100", "auto"}},
	}
	activation, previous, proposed := ifupdownTestActivation(t, data)
	ifupdownTestDevice(t, "enp6s0", false)
	writeState(t, filepath.Join(tools, "up-enp6s0"), "")
	if err := runIfupdownActivation(t, activation, false); err != nil {
		t.Fatal(err)
	}
	// enp6s0.100 is not up, so only enp6s0 is taken down; enp7s0 has no
	// device, so the check after activation skips it.
	want := []string{"ifquery --state enp6s0.100", "ifquery --state enp6s0", "ifdown -i " + previous + " enp6s0", "ifup enp6s0", "ifup --allow=hotplug enp7s0", "ifquery --state enp6s0"}
	if calls := takeCalls(t, tools); !reflect.DeepEqual(calls, want) {
		t.Fatal(calls)
	}
	// The restore takes down enp7s0, which ifupdown lists as up, and also
	// enp6s0, which it does not list but whose device exists: a failed ifup
	// leaves its address.
	os.Remove(filepath.Join(tools, "up-enp6s0"))
	if err := runIfupdownActivation(t, activation, true); err != nil {
		t.Fatal(err)
	}
	want = []string{"ifquery --state enp7s0", "ifdown --force -i " + proposed + " enp7s0", "ifquery --state enp6s0", "ifdown --force -i " + proposed + " enp6s0", "ifup enp6s0", "ifup enp6s0.100"}
	if calls := takeCalls(t, tools); !reflect.DeepEqual(calls, want) {
		t.Fatal(calls)
	}
}

func TestIfupdownActivationTakesTheVLANsOfAnInterfaceDownAndUpAroundIt(t *testing.T) {
	ifupdownFixture(t)
	tools := ifupdownStandIns(t)
	data := ifupdownRestoreData{
		TakeDownWithPreviousFile:          []string{"enp6s0"},
		BringUpWithProposedFile:           []ifupdownInterfaceToBringUp{{"enp6s0", "auto"}},
		TakeDownWithProposedFileOnRestore: []string{"enp6s0"},
		BringUpWithPreviousFileOnRestore:  []ifupdownInterfaceToBringUp{{"enp6s0", "auto"}},
	}
	activation, previous, proposed := ifupdownTestActivation(t, data)
	ifupdownTestDevice(t, "enp6s0", false)
	// The kernel links each device stacked on enp6s0 as upper_<name>; only
	// the VLAN is taken down and up, not the bridge.
	parent := filepath.Join(filepath.Dir(ifupdownVirtualDirectory), "physical", "enp6s0")
	for name, kind := range map[string]string{"enp6s0.100": "vlan", "br-app": "bridge"} {
		ifupdownTestDevice(t, name, true)
		writeState(t, filepath.Join(ifupdownVirtualDirectory, name, "uevent"), "DEVTYPE="+kind+"\nINTERFACE="+name+"\n")
		if err := os.Symlink(filepath.Join(ifupdownVirtualDirectory, name), filepath.Join(parent, "upper_"+name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, restoring := range []bool{false, true} {
		for _, name := range []string{"enp6s0", "enp6s0.100", "br-app"} {
			writeState(t, filepath.Join(tools, "up-"+name), "")
		}
		if err := runIfupdownActivation(t, activation, restoring); err != nil {
			t.Fatal(err)
		}
		want := []string{"ifquery --state enp6s0", "ifquery --state enp6s0.100", "ifdown enp6s0.100", "ifdown -i " + previous + " enp6s0", "ifup enp6s0", "ifup enp6s0.100", "ifquery --state enp6s0", "ifquery --state enp6s0.100"}
		if restoring {
			want = []string{"ifquery --state enp6s0", "ifquery --state enp6s0.100", "ifdown enp6s0.100", "ifdown --force -i " + proposed + " enp6s0", "ifup enp6s0", "ifup enp6s0.100"}
		}
		if calls := takeCalls(t, tools); !reflect.DeepEqual(calls, want) {
			t.Fatalf("restoring %t: %q", restoring, calls)
		}
	}
}

func TestIfupdownActivationFailuresGiveTheToolsStandardError(t *testing.T) {
	ifupdownFixture(t)
	tools := ifupdownStandIns(t)
	data := ifupdownRestoreData{BringUpWithProposedFile: []ifupdownInterfaceToBringUp{{"enp6s0", "auto"}}, BringUpWithPreviousFileOnRestore: []ifupdownInterfaceToBringUp{{"enp6s0", "auto"}}}
	activation, _, _ := ifupdownTestActivation(t, data)
	writeState(t, filepath.Join(tools, "fail-ifup"), "")
	if err := runIfupdownActivation(t, activation, false); err == nil || err.Error() != "ifupdown did not accept "+activation.destination+": ifup: failed to bring up enp6s0." {
		t.Fatal(err)
	}
	if err := runIfupdownActivation(t, activation, true); err == nil || err.Error() != "ifup enp6s0 failed: ifup: failed to bring up enp6s0" {
		t.Fatal(err)
	}
	// ifup that ends without an error but leaves the interface down fails the check.
	os.Remove(filepath.Join(tools, "fail-ifup"))
	writeStandInProgram(t, filepath.Join(tools, "ifup"), "exit 0\n")
	if err := runIfupdownActivation(t, activation, false); err == nil || err.Error() != "ifupdown did not accept "+activation.destination+": ifup ended without an error, but ifupdown does not list enp6s0 as up." {
		t.Fatal(err)
	}
}

func TestIfupdownActivationNeedsTheSavedFiles(t *testing.T) {
	ifupdownFixture(t)
	ifupdownStandIns(t)
	activation, _, _ := ifupdownTestActivation(t, ifupdownRestoreData{TakeDownWithPreviousFile: []string{"enp6s0"}, TakeDownWithProposedFileOnRestore: []string{"enp6s0"}})
	activation.UseSavedFiles("", "")
	for _, restoring := range []bool{false, true} {
		if err := activation.Activate(testContext(t), Runtime{}, restoring); err == nil || !strings.Contains(err.Error(), "the saved copy of the file that brought up enp6s0 is missing") {
			t.Fatal(err)
		}
	}
}

func TestIfupdownCheckFindsAVirtualDeviceThatIfdownLeft(t *testing.T) {
	ifupdownFixture(t)
	tools := ifupdownStandIns(t)
	activation, _, _ := ifupdownTestActivation(t, ifupdownRestoreData{TakeDownWithPreviousFile: []string{"enp6s0.100"}})
	ifupdownTestDevice(t, "enp6s0.100", true)
	writeState(t, filepath.Join(tools, "up-enp6s0.100"), "")
	if err := runIfupdownActivation(t, activation, false); err == nil || err.Error() != "ifdown left the virtual device enp6s0.100 that "+activation.destination+" declared." {
		t.Fatal(err)
	}
	// Another file that ifupdown reads still declares it.
	writeState(t, filepath.Join(tools, "up-enp6s0.100"), "")
	writeState(t, filepath.Join(tools, "declared-enp6s0.100"), "")
	if err := runIfupdownActivation(t, activation, false); err != nil {
		t.Fatal(err)
	}
	// ifdown removed the device itself.
	os.Remove(filepath.Join(tools, "declared-enp6s0.100"))
	writeState(t, filepath.Join(tools, "up-enp6s0.100"), "")
	writeState(t, filepath.Join(tools, "removes-enp6s0.100"), filepath.Join(ifupdownVirtualDirectory, "enp6s0.100"))
	if err := runIfupdownActivation(t, activation, false); err != nil {
		t.Fatal(err)
	}
}

func TestIfupdownActivationFactoryRefusesOtherPathsNamesAndCommands(t *testing.T) {
	ifupdownFixture(t)
	path := filepath.Join(ifupdownDirectory, "60-second")
	for _, raw := range []string{
		`{"command":"reboot"}`,
		`{"interfaces_to_take_down_with_previous_file":["../eth0"]}`,
		`{"interfaces_to_take_down_with_previous_file":["enp6s0","enp6s0"]}`,
		`{"interfaces_to_take_down_with_previous_file":["-a"]}`,
		`{"interfaces_to_take_down_with_proposed_file_on_restore":["two words"]}`,
		`{"interfaces_to_bring_up_with_proposed_file":[{"interface":"a-name-longer-than-15","listed_in":"auto"}]}`,
		`{"interfaces_to_bring_up_with_previous_file_on_restore":[{"interface":"enp6s0","listed_in":"allow-ovs"}]}`,
	} {
		if _, err := newIfupdownActivation(path, []byte(raw)); err == nil {
			t.Fatal(raw)
		}
	}
	for _, destination := range []string{"/tmp/60-second", filepath.Join(ifupdownDirectory, "60.cfg")} {
		if _, err := newIfupdownActivation(destination, []byte(`{}`)); err == nil {
			t.Fatal("accepted", destination)
		}
	}
	for _, destination := range []string{path, ifupdownMainFile} {
		if _, err := newIfupdownActivation(destination, []byte(`{"interfaces_to_take_down_with_previous_file":["eth0:1"]}`)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestIfupdownFilePermissionsPreservedCreatedAndExplicit(t *testing.T) {
	for _, test := range []struct {
		name, mode string
		exists     bool
		want       os.FileMode
	}{
		{"existing mode stays", "", true, 0600},
		{"new file defaults to 0644", "", false, 0644},
		{"existing explicit mode", "0640", true, 0640},
		{"new explicit mode", "0600", false, 0600},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "interfaces")
			if test.exists {
				if err := os.WriteFile(path, []byte("# previous\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			text := "# proposed\n"
			input := IfupdownInterfacesFile{Name: path, Content: &text, Mode: test.mode}
			file := input.file()
			file.Owner, file.Group = "", ""
			result := (Runtime{}).File(testContext(t), file, value.Scope{})
			if result.Status != "succeeded" {
				t.Fatal(result)
			}
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != test.want {
				t.Fatal(info, err)
			}
		})
	}
}
