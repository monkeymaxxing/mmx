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
	"strings"
	"testing"

	"mmx/internal/value"
)

func requireNetworkdRunRefusal(t *testing.T, path, proposed, reason string) {
	t.Helper()
	tools := standInPrograms(t, map[string]string{
		"systemctl":  `[ "$*" = 'is-active systemd-networkd.service' ] || exit 90; echo active`,
		"networkctl": "exit 91", "journalctl": "exit 92",
	})
	before := readState(t, path)
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, dryrun := range []bool{false, true} {
		service := &networkdOwedRestoreForTest{}
		input := &SystemdNetworkdFile{Name: filepath.Base(path), Content: &proposed}
		if proposed == "" {
			input.State, input.Content = "absent", nil
		}
		run := Runtime{Dryrun: dryrun, BeforeMutation: func(resource string) error {
			t.Fatalf("refused task reached mutation: %s", resource)
			return errors.New("unexpected mutation")
		}}
		result := input.Run(testContext(t), NodeTask{Runtime: run, OwedRestores: service})
		if result.Status != "failed" || result.Change != "unchanged" || !strings.Contains(result.Reason, reason) || service.applied {
			t.Fatalf("dryrun=%t: %+v; applied=%t", dryrun, result, service.applied)
		}
		if readState(t, path) != before {
			t.Fatal("refusal changed destination bytes")
		}
		after, err := os.ReadDir(filepath.Dir(path))
		if err != nil || !reflect.DeepEqual(entries, after) {
			t.Fatal("refusal created or changed directory entries", after, err)
		}
		for _, call := range takeCalls(t, tools) {
			if call != "systemctl is-active systemd-networkd.service" {
				t.Fatal("unexpected native command", call)
			}
		}
	}
}

func TestNetworkdBOMReferenceRefusesRemovalThroughRun(t *testing.T) {
	networkdFixture(t)
	path := filepath.Join(networkdDirectories[0], "20-bridge.netdev")
	writeState(t, path, netdev("mmxbr0", "bridge"))
	networkdTestDevice(t, "mmxbr0", true)
	reference := filepath.Join(networkdDirectories[1], "port.network.d", "port.conf")
	writeState(t, reference, "\ufeff[Network]\nBridge=mmxbr0\n")
	requireNetworkdRunRefusal(t, path, "", reference+" still refers to the device (Bridge=mmxbr0)")
}

func TestNetworkdOtherPeerDeclarationRefusesDeletionWithoutItsOwnKind(t *testing.T) {
	for _, dropIn := range []bool{false, true} {
		t.Run(fmt.Sprint(dropIn), func(t *testing.T) {
			networkdFixture(t)
			tools := networkdStandIns(t)
			networkdTestDevice(t, "mmxpeer0", true)
			other := filepath.Join(networkdDirectories[2], "other.netdev")
			if dropIn {
				other = filepath.Join(other+".d", "peer.conf")
			}
			writeState(t, other, "\ufeff[Peer]\nName=mmxpeer0\n")
			path := filepath.Join(networkdDirectories[0], "20-pair.netdev")
			for _, restoring := range []bool{false, true} {
				data := networkdRestoreData{DevicesToRemove: []string{"mmxpeer0"}, DevicesToRemoveOnRestore: []string{"mmxpeer0"}}
				saved, _ := json.Marshal(data)
				adapter, err := newNetworkdActivation(path, saved)
				if err != nil {
					t.Fatal(err)
				}
				if err := adapter.RemoveObsoleteVirtualDevices(testContext(t), Runtime{}, restoring); err == nil || !strings.Contains(err.Error(), "owned exclusively by the .netdev file") {
					t.Fatal(err)
				}
				if calls := takeCalls(t, tools); len(calls) != 0 {
					t.Fatal(calls)
				}
			}
		})
	}
}

func TestNetworkdRefusesAdditionBeforeExistingReferencesThroughRun(t *testing.T) {
	for directory := 0; directory < 3; directory++ {
		for _, dropIn := range []bool{false, true} {
			for _, rename := range []bool{false, true} {
				t.Run(fmt.Sprintf("directory%d/dropin%t/rename%t", directory, dropIn, rename), func(t *testing.T) {
					networkdFixture(t)
					path := filepath.Join(networkdDirectories[0], "20-bridge.netdev")
					if rename {
						writeState(t, path, netdev("oldbridge", "bridge"))
					}
					reference := filepath.Join(networkdDirectories[directory], "port.network")
					if dropIn {
						reference = filepath.Join(reference+".d", "device.conf")
					}
					writeState(t, reference, "[Network]\nFutureDeviceKind=other mmxbr0\n")
					want := "Cannot add " + path + ": " + reference + " already refers to device mmxbr0 (FutureDeviceKind=other mmxbr0). Add the .netdev file before installing the file that refers to the device."
					requireNetworkdRunRefusal(t, path, netdev("mmxbr0", "bridge"), want)
				})
			}
		}
	}
}

func TestNetworkdJournalDropsOnlyTheExactPosition(t *testing.T) {
	t.Parallel()
	path := "/etc/systemd/network/20-a.network"
	anchor := `{"__CURSOR":"anchor","PRIORITY":"4","MESSAGE":"old warning","CONFIG_FILE":"` + path + `"}`
	next := `{"__CURSOR":"next","PRIORITY":"4","MESSAGE":"new warning","CONFIG_FILE":"` + path + `"}`
	for _, text := range []string{anchor + "\n" + next, next} {
		diagnostic, err := checkNetworkdJournal(path, "anchor", text)
		if err != nil || diagnostic != "new warning" {
			t.Fatal(diagnostic, err)
		}
	}
	if diagnostic, err := checkNetworkdJournal(path, "anchor", anchor); err != nil || diagnostic != "" {
		t.Fatal(diagnostic, err)
	}
	for _, text := range []string{`{"PRIORITY":"4","MESSAGE":"warning"}`, `{"__CURSOR":null}`, `{"__CURSOR":[]}`, `{"__CURSOR":""}`} {
		if _, err := checkNetworkdJournal(path, "anchor", text); err == nil {
			t.Fatal("accepted unreadable journal position", text)
		}
	}
}

func TestNetworkdFactoryRefusesDeviceNamesForNetworkFiles(t *testing.T) {
	networkdFixture(t)
	for _, field := range []string{"virtual_devices_to_remove_after_file_change", "virtual_devices_to_remove_on_restore"} {
		_, err := newNetworkdActivation(filepath.Join(networkdDirectories[0], "20-a.network"), []byte(`{"`+field+`":["mmxbr0"]}`))
		if err == nil || !strings.Contains(err.Error(), "a .network file cannot declare devices to delete") {
			t.Fatal(err)
		}
	}
}

func TestNetworkdRechecksTheFullGuardBeforeEachDeletion(t *testing.T) {
	for _, restoring := range []bool{false, true} {
		for _, change := range []string{"other declaration", "physical", "reference"} {
			if restoring && change == "reference" {
				continue
			} // Restoring deliberately ignores references.
			t.Run(fmt.Sprintf("restore%t/%s", restoring, change), func(t *testing.T) {
				root := networkdFixture(t)
				for _, name := range []string{"first", "second"} {
					networkdTestDevice(t, name, true)
				}
				alter := ""
				switch change {
				case "other declaration":
					alter = "printf '[NetDev]\\nName=second\\nKind=bridge\\n' > '" + networkdDirectories[2] + "/other.netdev'"
				case "reference":
					alter = "printf '[Network]\\nBridge=second\\n' > '" + networkdDirectories[2] + "/port.network'"
				case "physical":
					alter = "mkdir -p '" + root + "/physical/second'; rm '" + networkdLinksDirectory + "/second'; ln -s '" + root + "/physical/second' '" + networkdLinksDirectory + "/second'"
				}
				tools := standInPrograms(t, map[string]string{"networkctl": "rm '" + networkdLinksDirectory + "/'\"$2\"\n" + alter})
				adapter := &networkdActivation{destination: filepath.Join(networkdDirectories[0], "20-devices.netdev"), data: networkdRestoreData{DevicesToRemove: []string{"first", "second"}, DevicesToRemoveOnRestore: []string{"first", "second"}}}
				if err := adapter.RemoveObsoleteVirtualDevices(testContext(t), Runtime{}, restoring); err == nil || !strings.HasPrefix(err.Error(), "Cannot remove virtual device second declared in ") {
					t.Fatal(err)
				}
				if calls := takeCalls(t, tools); !reflect.DeepEqual(calls, []string{"networkctl delete first"}) {
					t.Fatal(calls)
				}
			})
		}
	}
}

func TestNetworkdParserPreservesOrderedSettingsAndDeviceIdentities(t *testing.T) {
	text := "\ufeff# comment\n[NetDev]\nName=old\nName=\nName=\"bridge0\"\nKind=bridge\n[Peer]\nName='peer0'\n[Network]\nVLAN=one \\\n # continued comment\n two\nVLAN=\nVLAN=three\n"
	want := []networkdSetting{{section: "NetDev"}, {"NetDev", "Name", "old"}, {"NetDev", "Name", ""}, {"NetDev", "Name", `"bridge0"`}, {"NetDev", "Kind", "bridge"}, {section: "Peer"}, {"Peer", "Name", "'peer0'"}, {section: "Network"}, {"Network", "VLAN", "one  two"}, {"Network", "VLAN", ""}, {"Network", "VLAN", "three"}}
	if got := networkdSettings(text); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v; want %#v", got, want)
	}
	for _, other := range []bool{false, true} {
		names, err := networkdDeclaredDevices(networkdSettings(text), other)
		want := []string{"bridge0"}
		if other {
			want = append(want, "peer0")
		}
		if err != nil || !reflect.DeepEqual(names, want) {
			t.Fatal(names, err)
		}
	}
	if names, err := networkdDeclaredDevices(networkdSettings("[NetDev]\nName=first\nName=\n"), false); err != nil || len(names) != 0 {
		t.Fatal(names, err)
	}
	// Even reset references are conservatively protected, including quoted values.
	networkdFixture(t)
	path := filepath.Join(networkdDirectories[0], "20-a.netdev")
	writeState(t, path, netdev("bridge0", "bridge"))
	writeState(t, filepath.Join(networkdDirectories[1], "port.network"), "[Network]\nBridge=\"bridge0\"\nBridge=\n")
	requireNetworkdRunRefusal(t, path, "", "still refers to the device (Bridge=\"bridge0\")")
}

func TestNetworkdUnchangedPresentFileStillRefusesAnOwedRestore(t *testing.T) {
	networkdFixture(t)
	tools := networkdStandIns(t)
	contents := "[Match]\nName=eth1\n"
	path := filepath.Join(networkdDirectories[0], "20-a.network")
	writeState(t, path, contents)
	for _, dryrun := range []bool{false, true} {
		input := &SystemdNetworkdFile{Name: filepath.Base(path), Content: &contents}
		service := &networkdOwedRestoreForTest{}
		run := Runtime{Dryrun: dryrun, BeforeMutation: func(string) error { t.Fatal("unchanged file mutated"); return nil }}
		result := input.Run(testContext(t), NodeTask{Runtime: run, OwedRestores: service})
		if result.Status != "succeeded" || result.Change != "unchanged" || service.applied {
			t.Fatal("fixture must already match bytes and fixed metadata", result)
		}
		service.checkError = errors.New("the restore service still owes this file")
		result = input.Run(testContext(t), NodeTask{Runtime: run, OwedRestores: service})
		if result.Status != "failed" || result.Change != "unchanged" || result.Reason != service.checkError.Error() || service.applied {
			t.Fatal(result)
		}
	}
	if calls := takeCalls(t, tools); len(calls) != 0 {
		t.Fatal(calls)
	}
}

func TestNetworkdMissingGroupPreventsEveryMutation(t *testing.T) {
	networkdFixture(t)
	tools := networkdStandIns(t)
	lookupGroup = func(name string) (*user.Group, error) {
		if name != "systemd-network" {
			t.Fatal(name)
		}
		return nil, user.UnknownGroupError(name)
	}
	text := netdev("mmxbr0", "bridge")
	for _, dryrun := range []bool{false, true} {
		service := &networkdOwedRestoreForTest{}
		run := Runtime{Dryrun: dryrun, BeforeMutation: func(string) error { t.Fatal("mutation without group"); return nil }}
		result := (&SystemdNetworkdFile{Name: "20-a.netdev", Content: &text}).Run(testContext(t), NodeTask{Runtime: run, OwedRestores: service})
		want := "failed"
		if dryrun {
			want = "not_checked"
		}
		if result.Status != want || !strings.Contains(result.Reason, "systemd-network") || service.applied {
			t.Fatal(result)
		}
	}
	if calls := takeCalls(t, tools); len(calls) != 0 {
		t.Fatal(calls)
	}
	if entries, err := os.ReadDir(networkdDirectories[0]); err != nil || len(entries) != 0 {
		t.Fatal(entries, err)
	}
}

func TestNetworkdDryrunUsesWriteReplaceAndRemoveSentences(t *testing.T) {
	networkdFixture(t)
	networkdStandIns(t)
	contents := "[Match]\nName=eth1\n"
	path := filepath.Join(networkdDirectories[0], "20-a.network")
	input := &SystemdNetworkdFile{Name: filepath.Base(path), Content: &contents}
	for _, action := range []string{"write", "replace", "remove"} {
		if action == "replace" {
			writeState(t, path, "previous")
		}
		if action == "remove" {
			input.Content = nil
			input.State = "absent"
		}
		result := input.Run(testContext(t), NodeTask{Runtime: noMutation(t), OwedRestores: &networkdOwedRestoreForTest{}})
		want := "Would " + action + " " + path + " and activate it."
		if action == "remove" {
			want = "Would remove " + path + " and its running configuration."
		}
		if result.Status != "succeeded" || !strings.HasPrefix(result.Reason, want) {
			t.Fatal(result)
		}
	}
}

func TestNetworkdFreshAdapterRestoresFilesAndDeviceState(t *testing.T) {
	for _, test := range []struct {
		name, extension, previous, proposed string
		before, after                       []string
	}{
		{"new network", "network", "", "[Match]\nName=eth1\n", nil, nil},
		{"changed network", "network", "[Match]\nName=eth1\n", "[Match]\nName=eth2\n", nil, nil},
		{"removed network", "network", "[Match]\nName=eth1\n", "", nil, nil},
		{"new device", "netdev", "", netdev("newbridge", "bridge"), nil, nil},
		{"renamed device", "netdev", netdev("oldbridge", "bridge"), netdev("newbridge", "bridge"), []string{"oldbridge"}, []string{"oldbridge"}},
		{"removed device", "netdev", netdev("oldbridge", "bridge"), "", []string{"oldbridge"}, []string{"oldbridge"}},
		{"same settings existing device", "netdev", netdev("oldbridge", "bridge"), "# comment\n" + netdev("oldbridge", "bridge"), []string{"oldbridge"}, []string{"oldbridge"}},
		{"same declaration initially absent device", "netdev", netdev("oldbridge", "bridge"), "# comment\n" + netdev("oldbridge", "bridge"), nil, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			networkdFixture(t)
			path := filepath.Join(networkdDirectories[0], "20-file."+test.extension)
			if test.previous != "" {
				writeState(t, path, test.previous)
			}
			for _, name := range test.before {
				networkdTestDevice(t, name, true)
			}
			data, err := planNetworkdDevices(path, test.proposed)
			if err != nil {
				t.Fatal(err)
			}
			saved, err := json.Marshal(data)
			if err != nil {
				t.Fatal(err)
			}
			file := File{Path: path, Literal: &test.proposed}
			if test.proposed == "" {
				file.State = "absent"
				file.Literal = nil
			}
			run := Runtime{}
			ctx := testContext(t)
			prepared, result := run.PrepareFileChange(ctx, file, value.Scope{}, nil)
			if prepared == nil {
				t.Fatal(result)
			}
			defer prepared.Close()
			previousPath := filepath.Join(t.TempDir(), "saved-previous")
			metadata, err := prepared.SavePreviousFile(ctx, previousPath)
			if err != nil {
				t.Fatal(err)
			}
			if result := prepared.Install(ctx); result.Status != "succeeded" {
				t.Fatal(result)
			}
			// The stand-in reads the actual installed file and models reload creation.
			tools := standInPrograms(t, map[string]string{
				"journalctl": `case "$*" in *--show-cursor*) echo '-- cursor: anchor';; esac`,
				"networkctl": "case \"$1\" in\nreload)\n" +
					"if [ -f '" + path + "' ] && [ '" + test.extension + "' = netdev ]; then\n" +
					"name=$(sed -n 's/^Name=//p' '" + path + "'); mkdir -p '" + networkdVirtualDirectory + "/'\"$name\"; test -e '" + networkdLinksDirectory + "/'\"$name\" || ln -s '" + networkdVirtualDirectory + "/'\"$name\" '" + networkdLinksDirectory + "/'\"$name\"; fi;;\n" +
					"delete) rm '" + networkdLinksDirectory + "/'\"$2\";; esac",
			})
			forward, err := newNetworkdActivation(path, saved)
			if err != nil {
				t.Fatal(err)
			}
			for _, call := range []func(context.Context, Runtime, bool) error{forward.PrepareActivation, forward.Activate, forward.RemoveObsoleteVirtualDevices, forward.CheckActivatedConfiguration} {
				if err := call(ctx, run, false); err != nil {
					t.Fatal(err)
				}
			}
			// A later reference must not block removal of a newly introduced device.
			for _, name := range data.DevicesToRemoveOnRestore {
				writeState(t, filepath.Join(networkdDirectories[1], "late.network"), "[Network]\nBridge="+name+"\n")
			}
			if result := run.RestorePreviousFile(ctx, path, previousPath, metadata); result.Status != "succeeded" {
				t.Fatal(result)
			}
			restore, err := newNetworkdActivation(path, append([]byte(nil), saved...))
			if err != nil {
				t.Fatal(err)
			}
			takeCalls(t, tools)
			for _, call := range []func(context.Context, Runtime, bool) error{restore.PrepareActivation, restore.Activate, restore.RemoveObsoleteVirtualDevices, restore.CheckActivatedConfiguration} {
				if err := call(ctx, run, true); err != nil {
					t.Fatal(err)
				}
			}
			if readState(t, path) != test.previous {
				t.Fatal("previous file not restored")
			}
			entries, err := os.ReadDir(networkdLinksDirectory)
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, entry := range entries {
				names = append(names, entry.Name())
			}
			if !reflect.DeepEqual(names, test.after) {
				t.Fatal("restored devices", names, "want", test.after)
			}
			for _, call := range takeCalls(t, tools) {
				if strings.HasPrefix(call, "journalctl") {
					t.Fatal("restore read journal", call)
				}
			}
		})
	}
}

func TestNetworkdJournalErrorsDistinguishRejectionFromAnUncheckedFile(t *testing.T) {
	for _, rejection := range []bool{false, true} {
		t.Run(fmt.Sprint(rejection), func(t *testing.T) {
			networkdFixture(t)
			tools := networkdStandIns(t)
			path := filepath.Join(networkdDirectories[0], "20-a.network")
			journal := `{"__CURSOR":"after","PRIORITY":"4","MESSAGE":[]}`
			if rejection {
				journal = fmt.Sprintf(`{"__CURSOR":"after","PRIORITY":"4","MESSAGE":%q}`, path+": Unknown key")
			}
			writeState(t, filepath.Join(tools, "journal"), journal)
			adapter := &networkdActivation{destination: path, cursor: "before"}
			err := adapter.CheckActivatedConfiguration(testContext(t), Runtime{}, false)
			prefix := "Cannot check whether systemd-networkd accepted "
			if rejection {
				prefix = "systemd-networkd did not accept "
			}
			if err == nil || !strings.HasPrefix(err.Error(), prefix+path+":") {
				t.Fatal(err)
			}
		})
	}
}
