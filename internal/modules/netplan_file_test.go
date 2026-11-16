package modules

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

func netplanFixture(t *testing.T) (string, Runtime) {
	t.Helper()
	root := networkdFixture(t)
	oldDirectories, oldGenerated, oldStates := netplanDirectories, netplanGeneratedDirectory, netplanLinkStateDirectory
	t.Cleanup(func() {
		netplanDirectories, netplanGeneratedDirectory, netplanLinkStateDirectory = oldDirectories, oldGenerated, oldStates
	})
	netplanDirectories = []string{filepath.Join(root, "etc/netplan"), filepath.Join(root, "run/netplan"), filepath.Join(root, "lib/netplan")}
	netplanGeneratedDirectory, netplanLinkStateDirectory = filepath.Join(root, "generated"), filepath.Join(root, "states")
	for _, path := range append(append([]string{}, netplanDirectories...), netplanGeneratedDirectory, netplanLinkStateDirectory) {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	private := filepath.Join(root, "private")
	if err := os.Mkdir(private, 0700); err != nil {
		t.Fatal(err)
	}
	return root, Runtime{PrivateTaskDirectoryRoot: func() (string, error) { return private, nil }}
}
func netplanStandIns(t *testing.T) string {
	t.Helper()
	return standInPrograms(t, map[string]string{
		"systemctl": `if [ -f "$dir/inactive" ]; then echo inactive; exit 3; fi; echo active`,
		"networkctl": `if [ "$1" = delete ]; then
[ ! -e "$dir/fail-delete" ] || { echo 'native deletion error' >&2; exit 9; }
[ ! -e "$dir/delete-paths" ] || while IFS= read -r path; do rm -f "$path/$2"; done < "$dir/delete-paths"
[ ! -e "$dir/after-delete" ] || /bin/sh "$dir/after-delete"
fi`,
		"ip": `case "$*" in
'address flush '*) [ ! -e "$dir/fail-flush" ] || { echo 'native flush error' >&2; exit 9; };;
*) [ ! -e "$dir/addresses" ] || cat "$dir/addresses";; esac`,
		"netplan": `if [ "$1" = apply ]; then
[ ! -e "$dir/fail-apply" ] || { cat "$dir/fail-apply" >&2; exit 8; }
[ ! -e "$dir/on-apply" ] || /bin/sh "$dir/on-apply"
exit 0
fi
[ "$1" = generate ] && [ "$2" = --root-dir ] || exit 90
root=$3
printf '%s' "$root" > "$dir/last-tree"
phase=before
if [ -e "$root/generated-once" ]; then phase=after; fi
touch "$root/generated-once"
[ ! -f "$root/etc/netplan/60-test.yaml" ] || cp "$root/etc/netplan/60-test.yaml" "$dir/$phase-input"
if [ -f "$dir/fail-$phase" ]; then echo "$root/etc/netplan/60-test.yaml: Error in network definition: unknown key 'invalid'" >&2; exit 1; fi
mkdir -p "$root/run/systemd/network"
if [ -d "$dir/$phase" ]; then cp -R "$dir/$phase/." "$root/run/systemd/network/"; fi
if [ "$phase" = after ] && [ -f "$dir/renderer-nm" ]; then mkdir -p "$root/run/NetworkManager/system-connections"; touch "$root/run/NetworkManager/system-connections/profile"; fi
if [ -f "$dir/on-generate" ]; then /bin/sh "$dir/on-generate" "$root" "$phase" || exit $?; fi
echo 'a warning with exit zero' >&2`,
	})
}
func requireNetplanPrivateTreeGone(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, "private"))
	if err != nil || len(entries) != 0 {
		t.Fatal("private configuration tree retained", entries, err)
	}
}

func TestNetplanInputsMetadataAndTemplate(t *testing.T) {
	_, run := netplanFixture(t)
	netplanStandIns(t)
	for _, name := range []string{"../a.yaml", "/a.yaml", "a.yml", ".a.yaml", "a.yaml/child", "a.yaml\n"} {
		if err := netplanFileNameRule(name); err == nil {
			t.Fatal(name)
		}
	}
	for _, name := range []string{"60-second-card.yaml", "bridge_config.yaml"} {
		if err := netplanFileNameRule(name); err != nil {
			t.Fatal(err)
		}
	}
	input := &NetplanFile{Name: "60-test.yaml", Template: "card.tmpl"}
	directory := t.TempDir()
	writeState(t, filepath.Join(directory, "card.tmpl"), "network:\n  ethernets:\n    {{ .vars.card }}: {}\n")
	if err := input.Prepare(directory); err != nil {
		t.Fatal(err)
	}
	if len(input.ControllerFiles()) != 1 || input.Reads().Templates["template"] == "" {
		t.Fatal("missing template hooks")
	}
	file := input.file()
	if file.Mode != "0600" || file.Owner != networkdRootUser || file.Group != networkdRootGroup {
		t.Fatal(file)
	}
	run.Dryrun = true
	result := input.Run(testContext(t), NodeTask{Runtime: run, OwedRestores: &networkdOwedRestoreForTest{}, Scope: value.Scope{"vars": map[string]any{"card": "second0"}}})
	if result.Change != "would_change" || result.FileChange == nil || result.FileChange.After.Size != int64(len("network:\n  ethernets:\n    second0: {}\n")) {
		t.Fatal(result)
	}
}
func TestNetplanDryrunWritesNothingAndUnchangedNeverApplies(t *testing.T) {
	root, run := netplanFixture(t)
	tools := netplanStandIns(t)
	contents := "network: {version: 2}\n"
	input := &NetplanFile{Name: "60-test.yaml", Content: &contents}
	service := &networkdOwedRestoreForTest{}
	for _, action := range []string{"write", "replace", "remove"} {
		if action == "replace" {
			writeState(t, input.file().Path, "before")
		}
		if action == "remove" {
			input.State, input.Content = "absent", nil
		}
		result := input.Run(testContext(t), NodeTask{Runtime: noMutation(t), OwedRestores: service})
		if result.Change != "would_change" || !strings.HasPrefix(result.Reason, "Would "+action+" ") || !strings.Contains(result.Reason, "A dryrun does not check the proposed configuration with Netplan, apply the configuration, or check reachability after the change.") {
			t.Fatal(result)
		}
		if action == "remove" && !strings.Contains(result.Reason, "virtual network devices declared only by the file") {
			t.Fatal(result)
		}
	}
	input.State, input.Content = "", &contents
	writeState(t, input.file().Path, contents)
	if err := os.Chmod(input.file().Path, 0600); err != nil {
		t.Fatal(err)
	}
	for _, dryrun := range []bool{false, true} {
		run.Dryrun = dryrun
		result := input.Run(testContext(t), NodeTask{Runtime: run, OwedRestores: service})
		if result.Status != "succeeded" || result.Change != "unchanged" {
			t.Fatal(result)
		}
		service.checkError = errors.New("a previous node job owes this file")
		result = input.Run(testContext(t), NodeTask{Runtime: run, OwedRestores: service})
		if result.Status != "failed" || result.Change != "unchanged" {
			t.Fatal(result)
		}
		service.checkError = nil
	}
	if service.applied {
		t.Fatal("preview applied a change")
	}
	for _, call := range takeCalls(t, tools) {
		if call != "systemctl is-active systemd-networkd.service" {
			t.Fatal(call)
		}
	}
	requireNetplanPrivateTreeGone(t, root)
}
func TestNetplanMissingPrerequisitesAndStoppedRenderer(t *testing.T) {
	for _, missing := range []string{"netplan", "networkctl", "systemctl", "ip", "active service"} {
		t.Run(missing, func(t *testing.T) {
			root, run := netplanFixture(t)
			tools := netplanStandIns(t)
			if missing == "active service" {
				writeState(t, filepath.Join(tools, "inactive"), "")
			} else {
				setWorkerEnvironment(t, "PATH", tools)
				if err := os.Remove(filepath.Join(tools, missing)); err != nil {
					t.Fatal(err)
				}
			}
			contents := "network: {version: 2}"
			for _, dryrun := range []bool{false, true} {
				run.Dryrun = dryrun
				result := (&NetplanFile{Name: "60-test.yaml", Content: &contents}).Run(testContext(t), NodeTask{Runtime: run, OwedRestores: &networkdOwedRestoreForTest{}})
				want := "failed"
				if dryrun {
					want = "not_checked"
				}
				if result.Status != want {
					t.Fatal(result)
				}
				if missing == "active service" && !strings.Contains(result.Reason, "supports only the systemd-networkd renderer") {
					t.Fatal(result)
				}
			}
			requireNetplanPrivateTreeGone(t, root)
		})
	}
}
func TestNetplanGenerationCopiesBeforeAndAfterAndCleansEveryOutcome(t *testing.T) {
	for _, outcome := range []string{"success", "fail-before", "fail-after", "renderer-nm", "cancel"} {
		t.Run(outcome, func(t *testing.T) {
			root, run := netplanFixture(t)
			tools := netplanStandIns(t)
			path := filepath.Join(netplanDirectories[0], "60-test.yaml")
			writeState(t, path, "before")
			writeState(t, filepath.Join(netplanDirectories[1], "runtime.yaml"), "runtime secret")
			writeState(t, filepath.Join(netplanDirectories[2], "vendor.yaml"), "vendor secret")
			writeState(t, filepath.Join(tools, "before/10-netplan-old.network"), "[Match]\nName=second0\n")
			writeState(t, filepath.Join(tools, "after/10-netplan-new.network"), "[Match]\nName=second1\n")
			// This runs within generation, while the directory still exists.
			writeState(t, filepath.Join(tools, "on-generate"), `test "$(cat "$1/run/netplan/runtime.yaml")" = 'runtime secret' || exit 81
 test "$(cat "$1/lib/netplan/vendor.yaml")" = 'vendor secret' || exit 82`)
			if strings.HasPrefix(outcome, "fail-") || outcome == "renderer-nm" {
				writeState(t, filepath.Join(tools, outcome), "")
			}
			ctx, cancel := context.WithCancel(testContext(t))
			defer cancel()
			if outcome == "cancel" {
				cancel()
			}
			data, err := planNetplanChange(ctx, run, path, "after", false)
			if outcome == "success" {
				if err != nil || !reflect.DeepEqual(data.NetworkFilesToRemove, []string{"10-netplan-old.network"}) || !reflect.DeepEqual(data.NetworkFilesToRemoveOnRestore, []string{"10-netplan-new.network"}) {
					t.Fatal(data, err)
				}
				if readState(t, filepath.Join(tools, "before-input")) != "before" || readState(t, filepath.Join(tools, "after-input")) != "after" {
					t.Fatal("wrong generated candidate")
				}
			} else {
				prefix := "Netplan rejected the network configuration while checking " + path + ":"
				if outcome == "renderer-nm" {
					prefix = "The proposed Netplan configuration uses the NetworkManager renderer, which netplan_file does not support."
				} else if outcome == "cancel" {
					prefix = "Cannot check the proposed " + path + " with Netplan:"
				}
				if err == nil || !strings.HasPrefix(err.Error(), prefix) {
					t.Fatal(err)
				}
				if strings.HasPrefix(outcome, "fail-") && (!strings.Contains(err.Error(), "/etc/netplan/60-test.yaml: Error in network definition") || strings.Contains(err.Error(), readState(t, filepath.Join(tools, "last-tree"))+"/")) {
					t.Fatal(err)
				}
			}
			if readState(t, path) != "before" {
				t.Fatal("candidate escaped its tree")
			}
			requireNetplanPrivateTreeGone(t, root)
		})
	}
}

type netplanApplyInspection struct {
	t      *testing.T
	path   string
	called bool
}

func (s *netplanApplyInspection) CheckChangeAllowed(context.Context, string) error { return nil }
func (s *netplanApplyInspection) ApplyChange(ctx context.Context, change ChangeWithOwedRestore) protocol.Result {
	s.called = true
	if readState(s.t, s.path) != "previous" || change.RestoreConfirmationWait != 3*time.Second {
		s.t.Fatal("changed before protection", change)
	}
	var data netplanRestoreData
	if err := protocol.Decode(change.ModuleRestoreData, &data); err != nil || !reflect.DeepEqual(data.DevicesToRemove, []string{"oldbridge"}) || !reflect.DeepEqual(data.DevicesToRemoveOnRestore, []string{"newbridge"}) {
		s.t.Fatal(data, err)
	}
	if _, err := newNetplanActivation(s.path, change.ModuleRestoreData); err != nil {
		s.t.Fatal(err)
	}
	if _, err := change.PreparedFile.SavePreviousFile(ctx, filepath.Join(s.t.TempDir(), "previous")); err != nil {
		s.t.Fatal(err)
	}
	return change.PreparedFile.Install(ctx)
}
func TestNetplanRunHandsPreparedFileAndBothRemovalListsToTheMechanism(t *testing.T) {
	root, run := netplanFixture(t)
	tools := netplanStandIns(t)
	path := filepath.Join(netplanDirectories[0], "60-test.yaml")
	writeState(t, path, "previous")
	writeState(t, filepath.Join(tools, "before/10-netplan-old.netdev"), netdev("oldbridge", "bridge"))
	writeState(t, filepath.Join(tools, "after/10-netplan-new.netdev"), netdev("newbridge", "bridge"))
	networkdTestDevice(t, "oldbridge", true)
	service := &netplanApplyInspection{t: t, path: path}
	proposed := "proposed"
	result := (&NetplanFile{Name: "60-test.yaml", Content: &proposed, RestoreUnlessControllerConfirmsWithin: "3s"}).Run(testContext(t), NodeTask{Runtime: run, OwedRestores: service})
	if !service.called || result.Status != "succeeded" || readState(t, path) != proposed {
		t.Fatal(result)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
	for _, call := range takeCalls(t, tools) {
		if call == "netplan apply" {
			t.Fatal("Run activated in the worker")
		}
	}
	requireNetplanPrivateTreeGone(t, root)
}
func TestNetplanRefusesChangedSourceAndUnsafeDestination(t *testing.T) {
	for _, kind := range []string{"source changed", "symlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			root, run := netplanFixture(t)
			tools := netplanStandIns(t)
			path := filepath.Join(netplanDirectories[0], "60-test.yaml")
			switch kind {
			case "symlink":
				writeState(t, filepath.Join(root, "target"), "previous")
				if err := os.Symlink(filepath.Join(root, "target"), path); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			default:
				writeState(t, path, "previous")
				writeState(t, filepath.Join(tools, "on-generate"), "printf changed > '"+path+"'")
			}
			proposed := "proposed"
			service := &networkdOwedRestoreForTest{}
			result := (&NetplanFile{Name: "60-test.yaml", Content: &proposed}).Run(testContext(t), NodeTask{Runtime: run, OwedRestores: service})
			if result.Status != "failed" || result.Change != "unchanged" || service.applied {
				t.Fatal(result)
			}
			if kind == "source changed" && !strings.HasPrefix(result.Reason, "Cannot check the proposed "+path+" with Netplan:") {
				t.Fatal(result)
			}
			requireNetplanPrivateTreeGone(t, root)
		})
	}
}
func TestNetplanDevicePlansUseWholeGeneratedTrees(t *testing.T) {
	for _, kind := range []string{"changed settings", "changed settings without device", "existing outsider", "shared declaration", "removed device", "new pair", "existing peer"} {
		t.Run(kind, func(t *testing.T) {
			netplanFixture(t)
			before, after := netplanGeneratedFiles{}, netplanGeneratedFiles{}
			settings := networkdSettings(netdev("bridge0", "bridge"))
			before["10-netplan-bridge.netdev"] = settings
			after["10-netplan-bridge.netdev"] = settings
			wantError := false
			switch kind {
			case "changed settings", "changed settings without device":
				if kind == "changed settings" {
					networkdTestDevice(t, "bridge0", true)
				}
				after["10-netplan-bridge.netdev"] = networkdSettings(netdev("bridge0", "bridge") + "[Bridge]\nSTP=yes\n")
				wantError = true
			case "existing outsider":
				before = netplanGeneratedFiles{}
				networkdTestDevice(t, "bridge0", true)
				wantError = true
			case "shared declaration":
				networkdTestDevice(t, "bridge0", true)
			case "removed device":
				after = netplanGeneratedFiles{}
			case "new pair", "existing peer":
				before = netplanGeneratedFiles{}
				after = netplanGeneratedFiles{"10-netplan-pair.netdev": networkdSettings(netdev("veth0", "veth") + "[Peer]\nName=veth1\n")}
				if kind == "existing peer" {
					networkdTestDevice(t, "veth1", true)
					wantError = true
				}
			}
			data, err := planNetplanDevices("/etc/netplan/test.yaml", before, after)
			if (err != nil) != wantError {
				t.Fatal(data, err)
			}
			if strings.HasPrefix(kind, "changed settings") && (!strings.HasPrefix(err.Error(), "Cannot change the settings of network device bridge0 in /etc/netplan/test.yaml:") || !strings.Contains(err.Error(), "a restore could not undo the change")) {
				t.Fatal(err)
			}
			if kind == "shared declaration" && len(data.DevicesToRemove)+len(data.DevicesToRemoveOnRestore) != 0 {
				t.Fatal(data)
			}
			if kind == "new pair" && !reflect.DeepEqual(data.DevicesToRemoveOnRestore, []string{"veth0", "veth1"}) {
				t.Fatal(data)
			}
			if kind == "removed device" && !reflect.DeepEqual(data.DevicesToRemove, []string{"bridge0"}) {
				t.Fatal(data)
			}
		})
	}
}
func TestNetplanSavedDataRejectsCommandsPathsAndRepeatedIdentities(t *testing.T) {
	netplanFixture(t)
	path := filepath.Join(netplanDirectories[0], "60-test.yaml")
	for _, data := range []string{`{"command":"reboot"}`, `{"virtual_devices_to_remove_on_restore":["../physical"]}`, `{"virtual_devices_to_remove_after_file_change":["same","same"]}`, `{"generated_network_files_removed_on_restore":["../10-netplan-a.network"]}`, `{"generated_network_files_removed_after_file_change":["10-netplan-a.network","10-netplan-a.network"]}`} {
		if _, err := newNetplanActivation(path, json.RawMessage(data)); err == nil {
			t.Fatal(data)
		}
	}
	if _, err := newNetplanActivation("/tmp/60-test.yaml", json.RawMessage(`{}`)); err == nil {
		t.Fatal("wrong destination")
	}
}

func TestNetplanCopyDereferencesFilesWithoutWritingThroughLiveSymlinks(t *testing.T) {
	root, run := netplanFixture(t)
	netplanStandIns(t)
	writeState(t, filepath.Join(root, "live-secret"), "keep me")
	if err := os.Symlink(filepath.Join(root, "live-secret"), filepath.Join(netplanDirectories[2], "vendor.yaml")); err != nil {
		t.Fatal(err)
	}
	_, err := planNetplanChange(testContext(t), run, filepath.Join(netplanDirectories[0], "60-test.yaml"), "proposed", false)
	if err != nil || readState(t, filepath.Join(root, "live-secret")) != "keep me" {
		t.Fatal(err)
	}
	requireNetplanPrivateTreeGone(t, root)
}

func TestNetplanAbsentGeneratesTheTreeWithoutTheCandidate(t *testing.T) {
	root, run := netplanFixture(t)
	tools := netplanStandIns(t)
	path := filepath.Join(netplanDirectories[0], "60-test.yaml")
	writeState(t, path, "previous")
	writeState(t, filepath.Join(tools, "before/10-netplan-second.network"), "[Match]\nName=second0\n")
	data, err := planNetplanChange(testContext(t), run, path, "", true)
	if err != nil || !data.previousExisted || !reflect.DeepEqual(data.NetworkFilesToRemove, []string{"10-netplan-second.network"}) {
		t.Fatal(data, err)
	}
	if _, err := os.Stat(filepath.Join(tools, "after-input")); !os.IsNotExist(err) {
		t.Fatal("removed candidate reached generation", err)
	}
	if readState(t, path) != "previous" {
		t.Fatal("removed live file before protection")
	}
	requireNetplanPrivateTreeGone(t, root)
}

func TestNetplanPrivateCopyIsRootOnlyAndNativeRefusalNeverPreparesAChange(t *testing.T) {
	root, run := netplanFixture(t)
	tools := netplanStandIns(t)
	writeState(t, filepath.Join(netplanDirectories[2], "vendor.yaml"), "secret")
	writeState(t, filepath.Join(tools, "fail-after"), "")
	checks := 0
	run.BeforeMutation = func(string) error {
		entries, err := os.ReadDir(filepath.Join(root, "private"))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			err := filepath.WalkDir(filepath.Join(root, "private", entry.Name()), func(path string, entry os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				// Netplan's generated output is protected by the enclosing
				// private directory; its own generated modes can be broader.
				if entry.Name() == "generated-once" || strings.Contains(path, "/run/systemd") {
					return nil
				}
				info, err := entry.Info()
				if err == nil && info.Mode().Perm()&0077 != 0 {
					t.Errorf("private copy is accessible to others: %s %v", path, info.Mode())
				}
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			checks++
		}
		return nil
	}
	contents := "proposed"
	service := &networkdOwedRestoreForTest{}
	result := (&NetplanFile{Name: "60-test.yaml", Content: &contents}).Run(testContext(t), NodeTask{Runtime: run, OwedRestores: service})
	if result.Status != "failed" || result.Change != "unchanged" || service.applied || checks == 0 {
		t.Fatal(result, checks)
	}
	if _, err := os.Stat(filepath.Join(netplanDirectories[0], "60-test.yaml")); !os.IsNotExist(err) {
		t.Fatal("native refusal wrote a live candidate", err)
	}
	requireNetplanPrivateTreeGone(t, root)
}

func TestNetplanGeneratedSettingsRefusalsNeverPrepareAChange(t *testing.T) {
	for _, kind := range []string{"absent VLAN", "existing outsider", "changed match"} {
		t.Run(kind, func(t *testing.T) {
			root, run := netplanFixture(t)
			tools := netplanStandIns(t)
			path := filepath.Join(netplanDirectories[0], "60-test.yaml")
			writeState(t, path, "previous")
			prefix := ""
			switch kind {
			case "absent VLAN":
				writeState(t, filepath.Join(tools, "before/10-netplan-vlan0.netdev"), netdev("vlan0", "vlan")+"[VLAN]\nId=101\n")
				writeState(t, filepath.Join(tools, "after/10-netplan-vlan0.netdev"), netdev("vlan0", "vlan")+"[VLAN]\nId=102\n")
				prefix = "Cannot change the settings of network device vlan0 in " + path + ":"
			case "existing outsider":
				networkdTestDevice(t, "bridge0", true)
				writeState(t, filepath.Join(tools, "after/10-netplan-bridge0.netdev"), netdev("bridge0", "bridge"))
				prefix = "Cannot add " + path + ": network device bridge0 already exists"
			case "changed match":
				writeState(t, filepath.Join(tools, "before/10-netplan-application-lan.network"), "[Match]\nName=second0\n[Network]\nAddress=10.0.0.5/24\n")
				writeState(t, filepath.Join(tools, "after/10-netplan-application-lan.network"), "[Match]\nName=second1\n[Network]\nAddress=10.0.0.5/24\n")
				prefix = "Cannot change the network interfaces matched by Netplan definition application-lan in " + path + ":"
			}
			proposed := "proposed"
			service := &networkdOwedRestoreForTest{}
			result := (&NetplanFile{Name: "60-test.yaml", Content: &proposed}).Run(testContext(t), NodeTask{Runtime: run, OwedRestores: service})
			if result.Status != "failed" || result.Change != "unchanged" || service.applied || !strings.HasPrefix(result.Reason, prefix) {
				t.Fatal(result)
			}
			if kind == "changed match" && !strings.Contains(result.Reason, "give the definition a new name, or remove the definition and apply the file before adding the definition again.") {
				t.Fatal(result)
			}
			if readState(t, path) != "previous" {
				t.Fatal("refused change wrote the live file")
			}
			requireNetplanPrivateTreeGone(t, root)
		})
	}
}

func TestNetplanMatchComparisonIgnoresOtherSectionsAndFormatting(t *testing.T) {
	netplanFixture(t)
	before := netplanGeneratedFiles{"10-netplan-lan.network": networkdSettings("[Match]\nName=second0\n[Network]\nAddress=10.0.0.5/24\n")}
	for _, test := range []struct {
		name, contents string
		refused        bool
	}{
		{"address only", "# comment\n[Match]\n Name = second0\n[Network]\nAddress=10.0.0.6/24\n", false},
		{"removed match", "[Network]\nAddress=10.0.0.5/24\n", true},
		{"additional match", "[Match]\nName=second0\nMACAddress=02:00:00:00:00:01\n", true},
		{"repeated section", "[Match]\nName=second0\n[Network]\nAddress=10.0.0.5/24\n[Match]\nName=second1\n", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			after := netplanGeneratedFiles{"10-netplan-lan.network": networkdSettings(test.contents)}
			_, err := planNetplanDevices("/etc/netplan/60-test.yaml", before, after)
			if (err != nil) != test.refused {
				t.Fatal(err)
			}
		})
	}
}

func TestNetplanRemovalDiagnosticDoesNotDiscloseAnotherFilesSecret(t *testing.T) {
	root, run := netplanFixture(t)
	tools := netplanStandIns(t)
	path := filepath.Join(netplanDirectories[0], "60-test.yaml")
	writeState(t, path, "previous bridge")
	secret := "private-password-in-another-file"
	otherFile := filepath.Join(netplanDirectories[0], "61-dependent-vlan.yaml")
	writeState(t, otherFile, `{"network":{"vlans":{"vlan0":{"link":"bridge0"}},"wifis":{"wifi0":{"password":"`+secret+`"}}}}`+"\n")
	if err := os.Chmod(otherFile, 0600); err != nil {
		t.Fatal(err)
	}
	writeState(t, filepath.Join(tools, "on-generate"), `if [ "$2" = after ]; then
printf "%s:1:128: Error in network definition: vlan0: interface 'bridge0' is not defined\n" "$1/etc/netplan/61-dependent-vlan.yaml" >&2
cat "$1/etc/netplan/61-dependent-vlan.yaml" >&2
printf '                                                                                                                               ^\n' >&2
exit 1
fi`)
	service := &networkdOwedRestoreForTest{}
	result := (&NetplanFile{Name: "60-test.yaml", State: "absent"}).Run(testContext(t), NodeTask{Runtime: run, OwedRestores: service})
	serialized, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "failed" || result.Change != "unchanged" || service.applied || readState(t, path) != "previous bridge" {
		t.Fatal(result)
	}
	if !strings.HasPrefix(result.Reason, "Netplan rejected the network configuration while checking "+path+": /etc/netplan/61-dependent-vlan.yaml:1:128: Error in network definition:") || strings.Contains(string(serialized), secret) || strings.Contains(result.Reason, "^") {
		t.Fatal(result)
	}
	requireNetplanPrivateTreeGone(t, root)
}

func TestNetplanDiagnosticMessagesExcludeSourceContext(t *testing.T) {
	t.Parallel()
	for _, message := range []string{
		"/etc/netplan/60-test.yaml:2:5: Error in network definition: unknown key 'invalid'",
		"/etc/netplan/60-test.yaml:2:5: Error in network definition: address 'not-an-address' is missing /prefixlength",
		"/etc/netplan/60-test.yaml:2:5: Error in network definition: expected mapping (check indentation)",
		"/etc/netplan/60-test.yaml:2:5: Invalid YAML: inconsistent indentation",
		"Error in network definition: interface 'bridge0' is not defined",
	} {
		if got := netplanDiagnosticMessages(message + "\nError in network definition: secret-source-line\n    ^\n"); got != message {
			t.Fatalf("got %q, want %q", got, message)
		}
		if got := netplanDiagnosticMessages(message + "\nError in network definition: truncated-secret-source"); got != message {
			t.Fatal(got)
		}
	}
	message := "/etc/netplan/60-test.yaml:2:5: Error in network definition: unknown key 'invalid'"
	warning := "** (generate:525): WARNING **: 14:12:16.463: Permissions for /etc/netplan/60-test.yaml are too open."
	if got := netplanDiagnosticMessages(warning + "\n" + message + "\n  secret-source\n  ^\n"); got != warning+"\n"+message {
		t.Fatal(got)
	}
	if err := netplanCommandFailure(CommandOutput{ExitCode: 1, StandardError: "{password: secret}\n  ^\n"}); err == nil || err.Error() != "Netplan exited with code 1 without an error message that MMX recognizes." {
		t.Fatal(err)
	}
	if err := netplanCommandFailure(CommandOutput{ExitCode: 0, StandardError: warning}); err != nil {
		t.Fatal("successful generation warning failed the task", err)
	}
}

func TestNetplanCopyRefusesDirectorySymlinksAndSpecialFiles(t *testing.T) {
	for _, kind := range []string{"directory symlink", "special file"} {
		t.Run(kind, func(t *testing.T) {
			root, run := netplanFixture(t)
			tools := netplanStandIns(t)
			path := filepath.Join(netplanDirectories[0], "60-test.yaml")
			entry := filepath.Join(netplanDirectories[1], "unsafe")
			if kind == "directory symlink" {
				if err := os.Symlink(netplanDirectories[2], entry); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := syscall.Mkfifo(entry, 0600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := planNetplanChange(testContext(t), run, path, "proposed", false)
			if err == nil || !strings.HasPrefix(err.Error(), "Cannot check the proposed "+path+" with Netplan:") || !strings.Contains(err.Error(), "the path is not a regular file or directory") {
				t.Fatal(err)
			}
			if calls := takeCalls(t, tools); len(calls) != 0 {
				t.Fatal("generation ran on an unsafe copy", calls)
			}
			requireNetplanPrivateTreeGone(t, root)
		})
	}
}

func TestNetplanPrivateTreeRemovedOnPanicAndCancellationDuringGeneration(t *testing.T) {
	for _, interruption := range []string{"panic", "cancel"} {
		t.Run(interruption, func(t *testing.T) {
			root, run := netplanFixture(t)
			netplanStandIns(t)
			ctx, cancel := context.WithCancel(testContext(t))
			defer cancel()
			interrupted := false
			run.BeforeMutation = func(path string) error {
				if path == "command netplan" {
					interrupted = true
					if interruption == "panic" {
						panic("generation interrupted")
					}
					cancel()
				}
				return nil
			}
			func() {
				if interruption == "panic" {
					defer func() {
						if recovered := recover(); recovered != "generation interrupted" {
							t.Fatal("unexpected panic", recovered)
						}
					}()
				}
				_, err := planNetplanChange(ctx, run, filepath.Join(netplanDirectories[0], "60-test.yaml"), "proposed", false)
				if err == nil {
					t.Fatal("interrupted generation succeeded")
				}
			}()
			if !interrupted {
				t.Fatal("did not reach generation")
			}
			requireNetplanPrivateTreeGone(t, root)
		})
	}
}
