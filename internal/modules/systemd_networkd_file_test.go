package modules

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

func networkdFixture(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	oldDirectories, oldLinks, oldVirtual := networkdDirectories, networkdLinksDirectory, networkdVirtualDirectory
	oldWaitOnlinePrograms := networkdWaitOnlinePrograms
	networkdWaitOnlinePrograms = []string{filepath.Join(root, "usr-wait-online"), filepath.Join(root, "lib-wait-online")}
	oldLookup := lookupGroup
	oldRootUser, oldRootGroup := networkdRootUser, networkdRootGroup
	networkdRootUser, networkdRootGroup = Account(strconv.Itoa(os.Geteuid())), Account(strconv.Itoa(os.Getegid()))
	lookupGroup = func(name string) (*user.Group, error) {
		if name == "systemd-network" {
			return &user.Group{Name: name, Gid: strconv.Itoa(os.Getegid())}, nil
		}
		return oldLookup(name)
	}
	t.Cleanup(func() {
		networkdDirectories, networkdLinksDirectory, networkdVirtualDirectory = oldDirectories, oldLinks, oldVirtual
		networkdWaitOnlinePrograms = oldWaitOnlinePrograms
		lookupGroup = oldLookup
		networkdRootUser, networkdRootGroup = oldRootUser, oldRootGroup
	})
	networkdDirectories = []string{filepath.Join(root, "etc"), filepath.Join(root, "run"), filepath.Join(root, "usr")}
	networkdLinksDirectory, networkdVirtualDirectory = filepath.Join(root, "links"), filepath.Join(root, "virtual")
	for _, path := range append(append([]string{}, networkdDirectories...), networkdLinksDirectory, networkdVirtualDirectory) {
		if err := os.MkdirAll(path, 0755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}
func networkdTestDevice(t *testing.T, name string, virtual bool) {
	t.Helper()
	directory := networkdVirtualDirectory
	if !virtual {
		directory = filepath.Join(filepath.Dir(directory), "physical")
	}
	if err := os.MkdirAll(filepath.Join(directory, name), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(directory, name), filepath.Join(networkdLinksDirectory, name)); err != nil {
		t.Fatal(err)
	}
}
func netdev(name, kind string) string { return "[NetDev]\nName=" + name + "\nKind=" + kind + "\n" }

func TestNetworkdRefusesAnExistingDeviceBeforeAnyChange(t *testing.T) {
	for _, test := range []struct{ name, previous, proposed, reason string }{
		{"changed settings", netdev("mmxbr0", "bridge"), netdev("mmxbr0", "bridge") + "[Bridge]\nSTP=yes\n", "Cannot change"},
		{"another creator", "", netdev("mmxbr0", "bridge"), "Cannot add"},
		{"renamed to existing", netdev("mmxold", "bridge"), netdev("mmxbr0", "bridge"), "Cannot add"},
	} {
		t.Run(test.name, func(t *testing.T) {
			networkdFixture(t)
			networkdTestDevice(t, "mmxbr0", false)
			path := filepath.Join(networkdDirectories[0], "20-bridge.netdev")
			if test.previous != "" {
				writeState(t, path, test.previous)
			}
			requireNetworkdRunRefusal(t, path, test.proposed, test.reason)
			if got := readState(t, path); got != test.previous {
				t.Fatal("changed before refusal", got)
			}
			if exists, err := networkdDeviceExists("mmxbr0"); !exists || err != nil {
				t.Fatal(exists, err)
			}
		})
	}
}
func TestNetworkdAllowsOnlyCommentsAndAssignmentSpacingForExistingDevices(t *testing.T) {
	networkdFixture(t)
	networkdTestDevice(t, "mmxbr0", true)
	path := filepath.Join(networkdDirectories[0], "20-bridge.netdev")
	writeState(t, path, netdev("mmxbr0", "bridge"))
	data, err := planNetworkdDevices(path, "# new comment\n[NetDev]\n Name = mmxbr0\n\nKind = bridge\n; another comment\n")
	if err != nil || len(data.DevicesToRemove)+len(data.DevicesToRemoveOnRestore) != 0 {
		t.Fatal(data, err)
	}
	if _, err := planNetworkdDevices(path, "[NetDev]\nKind=bridge\nName=mmxbr0\n"); err == nil {
		t.Fatal("accepted reordered settings")
	}
}
func TestNetworkdRefusesReferencesInEveryDirectoryAndDropIn(t *testing.T) {
	for directory := 0; directory < 3; directory++ {
		for _, dropIn := range []bool{false, true} {
			for _, target := range []string{"", netdev("mmxbr1", "bridge")} {
				t.Run(fmt.Sprintf("directory%d/dropin%t/rename%t", directory, dropIn, target != ""), func(t *testing.T) {
					networkdFixture(t)
					networkdTestDevice(t, "mmxbr0", true)
					path := filepath.Join(networkdDirectories[0], "20-bridge.netdev")
					writeState(t, path, netdev("mmxbr0", "bridge"))
					reference := filepath.Join(networkdDirectories[directory], "10-parent.network")
					if dropIn {
						reference = filepath.Join(reference+".d", "20-device.conf")
					}
					writeState(t, reference, "[Match]\nName=eth1\n[Network]\nFutureDeviceKind=other \\\n mmxbr0\n")
					requireNetworkdRunRefusal(t, path, target, reference+" still refers to the device (FutureDeviceKind=other  mmxbr0)")
				})
			}
		}
	}
}
func TestNetworkdDeletesOnlyUnsharedVirtualDevicesAndRestoresOnlyNewDevices(t *testing.T) {
	for _, test := range []struct {
		name            string
		virtual, shared bool
		removals        int
	}{
		{"virtual", true, false, 1}, {"physical", false, false, 0}, {"shared", true, true, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			networkdFixture(t)
			networkdTestDevice(t, "mmxbr0", test.virtual)
			path := filepath.Join(networkdDirectories[0], "20-bridge.netdev")
			writeState(t, path, netdev("mmxbr0", "bridge"))
			writeState(t, filepath.Join(networkdDirectories[1], "20-address.network"), "[Match]\nName=mmxbr0\n[Network]\nAddress=10.0.0.5/24\n")
			if test.shared {
				writeState(t, filepath.Join(networkdDirectories[2], "other.netdev"), netdev("mmxbr0", "bridge"))
			}
			data, err := planNetworkdDevices(path, netdev("mmxbr1", "bridge"))
			if err != nil || len(data.DevicesToRemove) != test.removals || !reflect.DeepEqual(data.DevicesToRemoveOnRestore, []string{"mmxbr1"}) {
				t.Fatal(data, err)
			}
		})
	}
}
func TestNetworkdVethDeclaresBothEnds(t *testing.T) {
	networkdFixture(t)
	path := filepath.Join(networkdDirectories[0], "20-peer.netdev")
	text := netdev("mmxveth0", "veth") + "[Peer]\nName=mmxveth1\n"
	data, err := planNetworkdDevices(path, text)
	if err != nil || !reflect.DeepEqual(data.DevicesToRemoveOnRestore, []string{"mmxveth0", "mmxveth1"}) {
		t.Fatal(data, err)
	}
	networkdTestDevice(t, "mmxveth1", true)
	if _, err := planNetworkdDevices(path, text); err == nil {
		t.Fatal("accepted an existing peer")
	}
}
func TestNetworkdRefusesIndirectDeviceDeclarations(t *testing.T) {
	networkdFixture(t)
	path := filepath.Join(networkdDirectories[0], "20-bridge.netdev")
	requireNetworkdRunRefusal(t, path, netdev("bridge%i", "bridge"), "without systemd specifiers")
	writeState(t, filepath.Join(networkdDirectories[1], "20-bridge.netdev.d", "override.conf"), "[NetDev]\nName=other\n")
	requireNetworkdRunRefusal(t, path, netdev("mmxbr0", "bridge"), "drop-in")
}

func TestNetworkdJournalMatchesOnlyTheDestination(t *testing.T) {
	t.Parallel()
	path := "/etc/systemd/network/20-a.network"
	for _, message := range []string{path + ":6: Unknown key", `File "` + path + `" is invalid`, "File '" + path + "' is invalid", "No valid settings configured in " + path + ".", path} {
		if !networkdMessageNamesFile(message, path) {
			t.Fatal("missed", message)
		}
	}
	for _, message := range []string{path + ".d/extra.conf:1: invalid", path + "~old", path + ".backup", strings.ReplaceAll(path, "20-a", "120-a"), "prefix" + path, path + "2", path + "/child"} {
		if networkdMessageNamesFile(message, path) {
			t.Fatal("false match", message)
		}
	}
	for priority := 0; priority <= 7; priority++ {
		line := fmt.Sprintf(`{"__CURSOR":"after-position","PRIORITY":%q,"MESSAGE":%q,"CONFIG_FILE":%q,"CONFIG_LINE":"6"}`, fmt.Sprint(priority), "Unknown key", path)
		if diagnostic, err := checkNetworkdJournal(path, "before-position", line); err != nil || (diagnostic != "") != (priority <= 4) {
			t.Fatal(priority, diagnostic, err)
		}
	}
	// A structured field for another file wins over the path in MESSAGE.
	line := fmt.Sprintf(`{"__CURSOR":"after-position","PRIORITY":"4","MESSAGE":%q,"CONFIG_FILE":"/elsewhere"}`, path)
	if diagnostic, err := checkNetworkdJournal(path, "before-position", line); err != nil || diagnostic != "" {
		t.Fatal(diagnostic, err)
	}
	line = fmt.Sprintf(`{"__CURSOR":"after-position","PRIORITY":"4","MESSAGE":%q}`, path+": outside section")
	if diagnostic, err := checkNetworkdJournal(path, "before-position", line); err != nil || diagnostic == "" {
		t.Fatal("ignored unstructured warning")
	}
	if diagnostic, err := checkNetworkdJournal(path, "before-position", `{"__CURSOR":"after-position","PRIORITY":"4","MESSAGE":"a warning with no file name"}`); err != nil || diagnostic != "" {
		t.Fatal(diagnostic, err)
	}
}
func TestNetworkdJournalRefusesUnreadableOrIncompleteEntries(t *testing.T) {
	t.Parallel()
	for _, text := range []string{"bad JSON", `null`, `{}`, `{"__CURSOR":"after-position","PRIORITY":4,"MESSAGE":"x"}`, `{"__CURSOR":"after-position","PRIORITY":"8","MESSAGE":"x"}`, `{"__CURSOR":"after-position","PRIORITY":"4","MESSAGE":null}`, `{"__CURSOR":"after-position","PRIORITY":"4","MESSAGE":"x","CONFIG_FILE":[]}`, `{"__CURSOR":"after-position","PRIORITY":"4","MESSAGE":"x","CONFIG_LINE":null}`, protocol.TruncationMarker, strings.Repeat("{}\n", 200)} {
		if _, err := checkNetworkdJournal("/example", "before-position", text); err == nil {
			t.Fatal("accepted", text)
		}
	}
	if _, err := checkNetworkdJournal("/example", "before-position", ""); err != nil {
		t.Fatal(err)
	}
}
func networkdStandIns(t *testing.T) string {
	tools := standInPrograms(t, map[string]string{
		"usr-wait-online": `echo 'ignored wait diagnostic' >&2; [ ! -e "$dir/fail-wait" ] || exit 1`,
		"lib-wait-online": `echo 'ignored wait diagnostic' >&2; [ ! -e "$dir/fail-wait" ] || exit 1`,
		"networkctl": `[ ! -e "$dir/fail-reload" ] || exit 7
if [ "$1" = delete ] && [ -e "$dir/device-path" ]; then rm -f "$(cat "$dir/device-path")"; fi`,
		"journalctl": `case " $* " in
 *' --show-cursor '*) [ ! -e "$dir/fail-position" ] || exit 2; echo '-- cursor: cursor-before-reload';;
 *' --sync '*) [ ! -e "$dir/fail-sync" ] || exit 3;;
 *) [ ! -e "$dir/fail-read" ] || exit 4; test ! -f "$dir/journal" || cat "$dir/journal";;
esac`,
		"systemctl": `echo active`,
	})
	networkdWaitOnlinePrograms = []string{filepath.Join(tools, "usr-wait-online"), filepath.Join(tools, "lib-wait-online")}
	return tools
}
func TestNetworkdActivationUsesReloadAndOneJournalReadInOrder(t *testing.T) {
	networkdFixture(t)
	tools := networkdStandIns(t)
	adapter := &networkdActivation{destination: filepath.Join(networkdDirectories[0], "20-a.network")}
	ctx, run := testContext(t), Runtime{}
	for _, call := range []func(context.Context, Runtime, bool) error{adapter.PrepareActivation, adapter.Activate, adapter.RemoveObsoleteVirtualDevices, adapter.CheckActivatedConfiguration} {
		if err := call(ctx, run, false); err != nil {
			t.Fatal(err)
		}
	}
	calls := takeCalls(t, tools)
	want := []string{"journalctl --no-pager -n 1 -o cat --show-cursor", "networkctl reload", "journalctl --sync", "journalctl --no-pager --cursor cursor-before-reload -u systemd-networkd.service -n 200 -o json --output-fields=__CURSOR,PRIORITY,MESSAGE,CONFIG_FILE,CONFIG_LINE,SYSLOG_IDENTIFIER,_SYSTEMD_UNIT"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatal(calls)
	}
	// A pre-existing warning must not turn restoration into a failure.
	writeState(t, filepath.Join(tools, "fail-position"), "")
	writeState(t, filepath.Join(tools, "fail-read"), "")
	for _, call := range []func(context.Context, Runtime, bool) error{adapter.PrepareActivation, adapter.Activate, adapter.RemoveObsoleteVirtualDevices, adapter.CheckActivatedConfiguration} {
		if err := call(ctx, run, true); err != nil {
			t.Fatal(err)
		}
	}
	if calls := takeCalls(t, tools); !reflect.DeepEqual(calls, []string{"usr-wait-online --timeout=10", "networkctl reload"}) {
		t.Fatal(calls)
	}
}

func TestNetworkdRestoreContinuesAfterFailedOrMissingOnlineWait(t *testing.T) {
	for _, test := range []struct {
		name            string
		missingPrograms int
		failWait        bool
		wantWait        string
	}{
		{"first program fails without trying the second", 0, true, "usr-wait-online --timeout=10"},
		{"second program when first is missing", 1, false, "lib-wait-online --timeout=10"},
		{"second program fails", 1, true, "lib-wait-online --timeout=10"},
		{"both programs missing", 2, false, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			networkdFixture(t)
			tools := networkdStandIns(t)
			for _, program := range networkdWaitOnlinePrograms[:test.missingPrograms] {
				if err := os.Remove(program); err != nil {
					t.Fatal(err)
				}
			}
			if test.failWait {
				writeState(t, filepath.Join(tools, "fail-wait"), "")
			}
			adapter := &networkdActivation{destination: filepath.Join(networkdDirectories[0], "20-a.network")}
			mutations := 0
			run := Runtime{BeforeMutation: func(string) error { mutations++; return nil }}
			if err := adapter.PrepareActivation(testContext(t), run, true); err != nil {
				t.Fatal(err)
			}
			if mutations != 0 {
				t.Fatal("the online wait was marked as changing the node")
			}
			if err := adapter.Activate(testContext(t), run, true); err != nil {
				t.Fatal(err)
			}
			if mutations != 1 {
				t.Fatal("restore did not reload", mutations)
			}
			want := []string{"networkctl reload"}
			if test.wantWait != "" {
				want = append([]string{test.wantWait}, want...)
			}
			if calls := takeCalls(t, tools); !reflect.DeepEqual(calls, want) {
				t.Fatal("restore commands", calls, "want", want)
			}
		})
	}
}
func TestNetworkdActivationFailsForEachFailedNativeCommand(t *testing.T) {
	for _, failure := range []string{"position", "reload", "sync", "read"} {
		t.Run(failure, func(t *testing.T) {
			networkdFixture(t)
			tools := networkdStandIns(t)
			writeState(t, filepath.Join(tools, "fail-"+failure), "")
			adapter := &networkdActivation{destination: filepath.Join(networkdDirectories[0], "20-a.network")}
			var err error
			for _, call := range []func(context.Context, Runtime, bool) error{adapter.PrepareActivation, adapter.Activate, adapter.RemoveObsoleteVirtualDevices, adapter.CheckActivatedConfiguration} {
				err = call(testContext(t), Runtime{}, false)
				if err != nil {
					break
				}
			}
			prefix := "Cannot check whether systemd-networkd accepted "
			if failure == "reload" {
				prefix = "systemd-networkd did not accept "
			}
			if err == nil || !strings.HasPrefix(err.Error(), prefix+adapter.destination+":") {
				t.Fatal(err)
			}
		})
	}
}
func TestNetworkdRestoreDeletesIntroducedDevicesAndChecksTheyAreGone(t *testing.T) {
	networkdFixture(t)
	tools := networkdStandIns(t)
	networkdTestDevice(t, "mmxbr0", true)
	path := filepath.Join(networkdDirectories[0], "20-bridge.netdev")
	saved, _ := json.Marshal(networkdRestoreData{DevicesToRemoveOnRestore: []string{"mmxbr0"}})
	adapter, err := newNetworkdActivation(path, saved)
	if err != nil {
		t.Fatal(err)
	}
	// A successful command alone does not prove deletion.
	if err := adapter.RemoveObsoleteVirtualDevices(testContext(t), Runtime{}, true); err == nil || !strings.HasPrefix(err.Error(), "Cannot remove virtual device mmxbr0 declared in "+path+":") {
		t.Fatal("missing deletion failure", err)
	}
	writeState(t, filepath.Join(tools, "device-path"), filepath.Join(networkdLinksDirectory, "mmxbr0"))
	if err := adapter.RemoveObsoleteVirtualDevices(testContext(t), Runtime{}, true); err != nil {
		t.Fatal(err)
	}
	if err := adapter.CheckActivatedConfiguration(testContext(t), Runtime{}, true); err != nil {
		t.Fatal(err)
	}
	if err := adapter.RemoveObsoleteVirtualDevices(testContext(t), Runtime{}, true); err != nil {
		t.Fatal("repeated removal", err)
	}
}
func TestNetworkdRestoreRechecksDeviceOwnership(t *testing.T) {
	for _, physical := range []bool{false, true} {
		t.Run(fmt.Sprint(physical), func(t *testing.T) {
			networkdFixture(t)
			tools := networkdStandIns(t)
			networkdTestDevice(t, "mmxbr0", !physical)
			path := filepath.Join(networkdDirectories[0], "20-bridge.netdev")
			if !physical {
				writeState(t, filepath.Join(networkdDirectories[2], "other.netdev"), netdev("mmxbr0", "bridge"))
			}
			adapter := &networkdActivation{destination: path, data: networkdRestoreData{DevicesToRemoveOnRestore: []string{"mmxbr0"}}}
			if err := adapter.RemoveObsoleteVirtualDevices(testContext(t), Runtime{}, true); err == nil {
				t.Fatal("deleted unowned device")
			}
			if calls := takeCalls(t, tools); len(calls) > 0 {
				t.Fatal(calls)
			}
		})
	}
}
func TestNetworkdRestoreDataRejectsOtherPathsAndCommands(t *testing.T) {
	networkdFixture(t)
	path := filepath.Join(networkdDirectories[0], "20-bridge.netdev")
	for _, raw := range []string{`{"command":"reboot"}`, `{"virtual_devices_to_remove_after_file_change":["../eth0"]}`, `{"virtual_devices_to_remove_after_file_change":["x","x"]}`} {
		if _, err := newNetworkdActivation(path, []byte(raw)); err == nil {
			t.Fatal(raw)
		}
	}
	if _, err := newNetworkdActivation("/tmp/20-bridge.netdev", []byte(`{}`)); err == nil {
		t.Fatal("accepted another directory")
	}
}

type networkdOwedRestoreForTest struct {
	checkError error
	applied    bool
}

func (f *networkdOwedRestoreForTest) CheckChangeAllowed(context.Context, string) error {
	return f.checkError
}
func (f *networkdOwedRestoreForTest) ApplyChange(context.Context, ChangeWithOwedRestore) protocol.Result {
	f.applied = true
	return protocol.Success("changed")
}
func TestNetworkdDryrunAndSameFileRefusalWriteNothing(t *testing.T) {
	networkdFixture(t)
	tools := networkdStandIns(t)
	text := "[Match]\nName=eth1\n[Network]\nAddress=10.0.0.5/24\n"
	input := &SystemdNetworkdFile{Name: "20-second.network", Content: &text}
	service := &networkdOwedRestoreForTest{}
	result := input.Run(testContext(t), NodeTask{Runtime: noMutation(t), OwedRestores: service, Scope: value.Scope{}})
	if result.Status != "succeeded" || result.Change != "would_change" || result.FileChange == nil || service.applied {
		t.Fatal(result)
	}
	if entries, err := os.ReadDir(networkdDirectories[0]); err != nil || len(entries) != 0 {
		t.Fatal(entries, err)
	}
	if calls := takeCalls(t, tools); !reflect.DeepEqual(calls, []string{"systemctl is-active systemd-networkd.service"}) {
		t.Fatal(calls)
	}
	service.checkError = errors.New("Cannot change the file: restore service still owns it.")
	// The check precedes both the unchanged/absent return and dryrun.
	input.State, input.Content = "absent", nil
	result = input.Run(testContext(t), NodeTask{Runtime: noMutation(t), OwedRestores: service})
	if result.Status != "failed" || result.Change != "unchanged" || result.Reason != service.checkError.Error() {
		t.Fatal(result)
	}
}
func TestNetworkdMissingPrerequisitesAreNotCheckedOnlyInDryrun(t *testing.T) {
	networkdFixture(t)
	setWorkerEnvironment(t, "PATH", t.TempDir())
	text := "[Match]\nName=eth1\n"
	input := &SystemdNetworkdFile{Name: "20-second.network", Content: &text}
	for _, dryrun := range []bool{true, false} {
		result := input.Run(testContext(t), NodeTask{Runtime: Runtime{Dryrun: dryrun}, OwedRestores: &networkdOwedRestoreForTest{}})
		want := "failed"
		if dryrun {
			want = "not_checked"
		}
		if result.Status != want || !strings.Contains(result.Reason, missingProgramError("networkctl").Error()) {
			t.Fatal(result)
		}
	}
	input.State, input.Content = "absent", nil
	result := input.Run(testContext(t), NodeTask{Runtime: Runtime{}, OwedRestores: &networkdOwedRestoreForTest{}})
	if result.Status != "succeeded" || result.Change != "unchanged" || !strings.Contains(result.Reason, "no activation or confirmation wait") {
		t.Fatal(result)
	}
}
func TestNetworkFileInputsAndFixedMetadata(t *testing.T) {
	t.Parallel()
	text := "contents"
	for _, input := range []NetworkFile{{}, {Content: &text, Template: "both"}, {State: "absent", Content: &text}, {State: "other", Content: &text}, {Content: &text, RestoreUnlessControllerConfirmsWithin: "0s"}, {Content: &text, RestoreUnlessControllerConfirmsWithin: "-1s"}} {
		if err := input.prepare(t.TempDir(), "name"); err == nil {
			t.Fatal(input)
		}
	}
	input := &NetworkFile{Content: &text}
	if wait, err := input.confirmationWait(); err != nil || wait != 2*time.Minute {
		t.Fatal(wait, err)
	}
	for _, name := range []string{"../20.network", "/20.network", "20.link", "", "20.conf"} {
		if err := networkdFileNameRule(name); err == nil {
			t.Fatal(name)
		}
	}
	for name, mode := range map[string]string{"20-a.network": "0644", "20-a.netdev": "0640"} {
		file := (&SystemdNetworkdFile{Name: name, Content: &text}).file()
		if file.Mode != mode || file.Owner != "0" {
			t.Fatal(file)
		}
		if strings.HasSuffix(name, ".netdev") && file.Group != "systemd-network" {
			t.Fatal(file)
		}
	}
}

func TestNetworkdTemplateRendersThroughTheFileHelper(t *testing.T) {
	networkdFixture(t)
	networkdStandIns(t)
	directory := t.TempDir()
	writeState(t, filepath.Join(directory, "second.tmpl"), "[Match]\nName={{ .vars.interface }}\n[Network]\nAddress={{ .vars.address }}/24\n")
	input := &SystemdNetworkdFile{Name: "20-template.network", Template: "second.tmpl"}
	if err := input.Prepare(directory); err != nil {
		t.Fatal(err)
	}
	if input.ControllerFiles()[0].Local == nil || input.Reads().Templates["template"] == "" {
		t.Fatal("template not handed to controller snapshot")
	}
	result := input.Run(testContext(t), NodeTask{Runtime: noMutation(t), OwedRestores: &networkdOwedRestoreForTest{}, Scope: value.Scope{"vars": map[string]any{"interface": "eth1", "address": "10.0.0.5"}}})
	if result.Status != "succeeded" || result.FileChange == nil || !strings.Contains(result.FileChange.Diff, "Address=10.0.0.5/24") || strings.Contains(result.FileChange.Diff, "{{") {
		t.Fatal(result)
	}
}
func TestNetworkdStoppedServicePreventsPreparation(t *testing.T) {
	networkdFixture(t)
	tools := networkdStandIns(t)
	writeStandInProgram(t, filepath.Join(tools, "systemctl"), "#!/bin/sh\necho inactive\nexit 3\n")
	text := "[Match]\nName=eth1\n"
	input := &SystemdNetworkdFile{Name: "20-stopped.network", Content: &text}
	for _, dryrun := range []bool{false, true} {
		result := input.Run(testContext(t), NodeTask{Runtime: Runtime{Dryrun: dryrun}, OwedRestores: &networkdOwedRestoreForTest{}})
		want := "failed"
		if dryrun {
			want = "not_checked"
		}
		if result.Status != want || !strings.Contains(result.Reason, "systemd-networkd.service is not running.") {
			t.Fatal(result)
		}
	}
	if entries, err := os.ReadDir(networkdDirectories[0]); err != nil || len(entries) != 0 {
		t.Fatal(entries, err)
	}
}

func TestNetworkdDoesNotClaimAnAbsentDeviceAnotherFileDeclares(t *testing.T) {
	networkdFixture(t)
	path := filepath.Join(networkdDirectories[0], "20-bridge.netdev")
	writeState(t, filepath.Join(networkdDirectories[2], "other.netdev"), netdev("mmxbr0", "bridge"))
	data, err := planNetworkdDevices(path, netdev("mmxbr0", "bridge"))
	if err != nil || len(data.DevicesToRemoveOnRestore) != 0 {
		t.Fatal(data, err)
	}
}

func TestNetworkdNeverDeletesAVethWhosePeerAnotherFileDeclares(t *testing.T) {
	networkdFixture(t)
	tools := networkdStandIns(t)
	path := filepath.Join(networkdDirectories[0], "20-pair.netdev")
	pair := netdev("mmxveth0", "veth") + "[Peer]\nName=mmxveth1\n"
	writeState(t, filepath.Join(networkdDirectories[2], "other.netdev"), netdev("mmxveth1", "dummy"))
	data, err := planNetworkdDevices(path, pair)
	if err != nil || len(data.DevicesToRemoveOnRestore) != 0 {
		t.Fatal(data, err)
	}
	writeState(t, path, pair)
	networkdTestDevice(t, "mmxveth0", true)
	networkdTestDevice(t, "mmxveth1", true)
	data, err = planNetworkdDevices(path, "")
	if err != nil || len(data.DevicesToRemove) != 0 {
		t.Fatal(data, err)
	}
	// An additional declaration after preparation must prevent the FIRST
	// deletion, not only a second delete that would be too late for the peer.
	adapter := &networkdActivation{destination: path, data: networkdRestoreData{DevicesToRemove: []string{"mmxveth0", "mmxveth1"}}}
	if err := adapter.RemoveObsoleteVirtualDevices(testContext(t), Runtime{}, false); err == nil {
		t.Fatal("deleted pair with an unowned peer")
	}
	if calls := takeCalls(t, tools); len(calls) != 0 {
		t.Fatal(calls)
	}
}

type networkdApplyInspection struct {
	t           *testing.T
	destination string
	called      bool
}

func (s *networkdApplyInspection) CheckChangeAllowed(context.Context, string) error { return nil }
func (s *networkdApplyInspection) ApplyChange(ctx context.Context, change ChangeWithOwedRestore) protocol.Result {
	s.called = true
	if change.RestoreConfirmationWait != 3*time.Second || change.PreparedFile.DestinationFile() != s.destination {
		s.t.Fatal(change)
	}
	if readState(s.t, s.destination) == "" {
		s.t.Fatal("module removed the live file before handing it to the owed restore")
	}
	var data networkdRestoreData
	if err := protocol.Decode(change.ModuleRestoreData, &data); err != nil || !reflect.DeepEqual(data.DevicesToRemove, []string{"mmxbr0"}) || len(data.DevicesToRemoveOnRestore) != 0 {
		s.t.Fatal(data, err)
	}
	previous, err := change.PreparedFile.SavePreviousFile(ctx, filepath.Join(s.t.TempDir(), "previous"))
	if err != nil || !previous.Existed {
		s.t.Fatal(previous, err)
	}
	return change.PreparedFile.Install(ctx)
}
func TestNetworkdRemovalHandsAnUninstalledFileAndRecoveryDataToTheMechanism(t *testing.T) {
	networkdFixture(t)
	networkdStandIns(t)
	networkdTestDevice(t, "mmxbr0", true)
	path := filepath.Join(networkdDirectories[0], "20-bridge.netdev")
	writeState(t, path, netdev("mmxbr0", "bridge"))
	input := &SystemdNetworkdFile{Name: "20-bridge.netdev", State: "absent", RestoreUnlessControllerConfirmsWithin: "3s"}
	service := &networkdApplyInspection{t: t, destination: path}
	result := input.Run(testContext(t), NodeTask{OwedRestores: service})
	if !service.called || result.Status != "succeeded" || result.Change != "changed" {
		t.Fatal(result)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("file was not removed", err)
	}
	// Activation belongs to the separate process, not to Run.
	if exists, err := networkdDeviceExists("mmxbr0"); !exists || err != nil {
		t.Fatal(exists, err)
	}
}
func TestNetworkdRefusesASymlinkBeforePreviewOrRemoval(t *testing.T) {
	networkdFixture(t)
	target := filepath.Join(t.TempDir(), "target")
	writeState(t, target, "untouched")
	if err := os.Symlink(target, filepath.Join(networkdDirectories[0], "20-second.network")); err != nil {
		t.Fatal(err)
	}
	for _, dryrun := range []bool{false, true} {
		input := &SystemdNetworkdFile{Name: "20-second.network", State: "absent"}
		service := &networkdOwedRestoreForTest{}
		result := input.Run(testContext(t), NodeTask{Runtime: Runtime{Dryrun: dryrun}, OwedRestores: service})
		if result.Status != "failed" || result.Change != "unchanged" || service.applied {
			t.Fatal(result)
		}
		if readState(t, target) != "untouched" {
			t.Fatal("changed symlink target")
		}
	}
}
