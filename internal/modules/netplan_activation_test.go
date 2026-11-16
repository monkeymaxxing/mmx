package modules

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func netplanTestLink(t *testing.T, name, index, file, state string) {
	t.Helper()
	networkdTestDevice(t, name, false)
	writeState(t, filepath.Join(networkdLinksDirectory, name, "ifindex"), index)
	writeState(t, filepath.Join(netplanLinkStateDirectory, index), "NETWORK_FILE="+filepath.Join(netplanGeneratedDirectory, file)+"\nADMIN_STATE="+state+"\n")
}
func TestNetplanFreshActivationAndRestoreFlushOnlyReleasedLinks(t *testing.T) {
	for _, restoring := range []bool{false, true} {
		t.Run(map[bool]string{false: "activation", true: "restore"}[restoring], func(t *testing.T) {
			netplanFixture(t)
			tools := netplanStandIns(t)
			netplanTestLink(t, "released0", "3", "10-netplan-old.network", "configured")
			netplanTestLink(t, "introduced0", "6", "10-netplan-new.network", "configured")
			file, released, index, obsolete, retained := "10-netplan-old.network", "released0", "3", "obsolete0", "introduced1"
			if restoring {
				file, released, index, obsolete, retained = "10-netplan-new.network", "introduced0", "6", "introduced1", "obsolete0"
			}
			netplanTestLink(t, "adopted0", "4", file, "configured")
			netplanTestLink(t, "untouched0", "5", "10-netplan-stays.network", "unmanaged")
			// systemd can keep this filename after a link is unmanaged, as 249 did.
			netplanTestLink(t, "stale0", "7", file, "unmanaged")
			networkdTestDevice(t, "obsolete0", true)
			networkdTestDevice(t, "introduced1", true)
			writeState(t, filepath.Join(networkdLinksDirectory, "bonding_masters"), "")
			writeState(t, filepath.Join(tools, "delete-paths"), networkdLinksDirectory+"\n")
			writeState(t, filepath.Join(tools, "on-apply"), "printf 'ADMIN_STATE=unmanaged\\n' > '"+filepath.Join(netplanLinkStateDirectory, index)+"'\n"+
				"printf 'ADMIN_STATE=configured\\nNETWORK_FILE="+filepath.Join(netplanGeneratedDirectory, "10-netplan-replacement.network")+"\\n' > '"+filepath.Join(netplanLinkStateDirectory, "4")+"'\n")
			waitProgram := filepath.Join(tools, "wait-online")
			writeStandInProgram(t, waitProgram, "#!/bin/sh\necho 'wait-online --timeout=10' >> '"+filepath.Join(tools, "calls")+"'\nexit 1\n")
			networkdWaitOnlinePrograms = []string{waitProgram}
			data := netplanRestoreData{DevicesToRemove: []string{"obsolete0"}, DevicesToRemoveOnRestore: []string{"introduced1"}, NetworkFilesToRemove: []string{"10-netplan-old.network"}, NetworkFilesToRemoveOnRestore: []string{"10-netplan-new.network"}}
			saved, _ := json.Marshal(data)
			adapter, err := newNetplanActivation(filepath.Join(netplanDirectories[0], "60-test.yaml"), saved)
			if err != nil {
				t.Fatal(err)
			}
			for _, call := range []func(context.Context, Runtime, bool) error{adapter.PrepareActivation, adapter.Activate, adapter.RemoveObsoleteVirtualDevices, adapter.CheckActivatedConfiguration} {
				if err := call(testContext(t), Runtime{}, restoring); err != nil {
					t.Fatal(err)
				}
			}
			calls := takeCalls(t, tools)
			want := []string{"netplan apply", "networkctl delete " + obsolete, "ip address flush dev " + released + " scope global", "ip -o address show dev " + released + " scope global"}
			if restoring {
				want = append([]string{"wait-online --timeout=10"}, want...)
			}
			if !reflect.DeepEqual(calls, want) {
				t.Fatal(calls, "want", want)
			}
			if exists, err := networkdDeviceExists(retained); err != nil || !exists {
				t.Fatal("removed the other direction's device", retained, err)
			}
		})
	}
}
func TestNetplanActivationCommandFailuresAndRemainingAddressesFail(t *testing.T) {
	for _, failure := range []string{"fail-apply", "fail-delete", "fail-flush", "addresses"} {
		t.Run(failure, func(t *testing.T) {
			netplanFixture(t)
			tools := netplanStandIns(t)
			netplanTestLink(t, "released0", "3", "10-netplan-old.network", "configured")
			writeState(t, filepath.Join(tools, "on-apply"), "printf 'ADMIN_STATE=unmanaged\\n' > '"+filepath.Join(netplanLinkStateDirectory, "3")+"'\n")
			networkdTestDevice(t, "obsolete0", true)
			writeState(t, filepath.Join(tools, "delete-paths"), networkdLinksDirectory+"\n")
			writeState(t, filepath.Join(tools, failure), "remaining address\n")
			adapter := &netplanActivation{destination: "/etc/netplan/60-test.yaml", data: netplanRestoreData{DevicesToRemove: []string{"obsolete0"}, NetworkFilesToRemove: []string{"10-netplan-old.network"}}}
			var err error
			for _, call := range []func(context.Context, Runtime, bool) error{adapter.PrepareActivation, adapter.Activate, adapter.RemoveObsoleteVirtualDevices, adapter.CheckActivatedConfiguration} {
				if err = call(testContext(t), Runtime{}, false); err != nil {
					break
				}
			}
			prefix := "Cannot remove the IP addresses on network interface released0 configured by /etc/netplan/60-test.yaml:"
			if failure == "fail-apply" {
				prefix = "Cannot apply /etc/netplan/60-test.yaml with Netplan:"
			}
			if failure == "fail-delete" {
				prefix = "Cannot remove virtual network device obsolete0 declared by /etc/netplan/60-test.yaml:"
			}
			if err == nil || !strings.HasPrefix(err.Error(), prefix) {
				t.Fatal(err)
			}
		})
	}
}
func TestNetplanDeletionRechecksPhysicalStatusAndGeneratedOwnership(t *testing.T) {
	for _, guard := range []string{"physical", "generated", "late peer"} {
		t.Run(guard, func(t *testing.T) {
			netplanFixture(t)
			tools := netplanStandIns(t)
			networkdTestDevice(t, "first0", true)
			networkdTestDevice(t, "veth0", guard != "physical")
			networkdTestDevice(t, "veth1", true)
			if guard == "generated" {
				writeState(t, filepath.Join(netplanGeneratedDirectory, "10-netplan-pair.netdev"), netdev("veth0", "veth")+"[Peer]\nName=veth1\n")
			}
			if guard == "late peer" {
				writeState(t, filepath.Join(tools, "delete-paths"), networkdLinksDirectory+"\n")
				writeState(t, filepath.Join(tools, "after-delete"), "printf '[NetDev]\\nName=veth1\\nKind=dummy\\n' > '"+filepath.Join(netplanGeneratedDirectory, "10-netplan-other.netdev")+"'")
			}
			adapter := &netplanActivation{destination: "/etc/netplan/60-test.yaml", data: netplanRestoreData{DevicesToRemove: []string{"first0", "veth0", "veth1"}}}
			if err := adapter.RemoveObsoleteVirtualDevices(testContext(t), Runtime{}, false); err == nil {
				t.Fatal("deleted an unowned device")
			}
			calls := takeCalls(t, tools)
			if guard == "late peer" {
				if !reflect.DeepEqual(calls, []string{"networkctl delete first0"}) {
					t.Fatal(calls)
				}
			} else if len(calls) != 0 {
				t.Fatal(calls)
			}
		})
	}
}
func TestNetplanSuccessfulDeleteMustRemoveTheDevice(t *testing.T) {
	netplanFixture(t)
	netplanStandIns(t)
	networkdTestDevice(t, "bridge0", true)
	adapter := &netplanActivation{destination: "/etc/netplan/60-test.yaml", data: netplanRestoreData{DevicesToRemoveOnRestore: []string{"bridge0"}}}
	if err := adapter.RemoveObsoleteVirtualDevices(testContext(t), Runtime{}, true); err == nil || !strings.Contains(err.Error(), "still exists") {
		t.Fatal(err)
	}
}
func TestNetplanCleanupWaitHonorsCancellationAndLinkIdentity(t *testing.T) {
	netplanFixture(t)
	tools := netplanStandIns(t)
	netplanTestLink(t, "second0", "3", "10-netplan-old.network", "configuring")
	adapter := &netplanActivation{destination: "/etc/netplan/60-test.yaml", data: netplanRestoreData{NetworkFilesToRemove: []string{"10-netplan-old.network"}}}
	if err := adapter.PrepareActivation(testContext(t), Runtime{}, false); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(testContext(t))
	cancel()
	if err := adapter.RemoveObsoleteVirtualDevices(ctx, Runtime{}, false); err == nil {
		t.Fatal("ignored cancellation")
	}
	writeState(t, filepath.Join(networkdLinksDirectory, "second0", "ifindex"), "4")
	if err := adapter.RemoveObsoleteVirtualDevices(testContext(t), Runtime{}, false); err != nil {
		t.Fatal(err)
	}
	if calls := takeCalls(t, tools); len(calls) != 0 {
		t.Fatal("flushed a replacement link", calls)
	}
}

func TestNetplanPreparationReadFailuresDoNotClaimAnApply(t *testing.T) {
	for _, kind := range []string{"link directory", "link index", "link state"} {
		t.Run(kind, func(t *testing.T) {
			netplanFixture(t)
			tools := netplanStandIns(t)
			netplanTestLink(t, "second0", "3", "10-netplan-old.network", "configured")
			switch kind {
			case "link directory":
				if err := os.RemoveAll(networkdLinksDirectory); err != nil {
					t.Fatal(err)
				}
			case "link index":
				writeState(t, filepath.Join(networkdLinksDirectory, "second0", "ifindex"), "invalid")
			case "link state":
				path := filepath.Join(netplanLinkStateDirectory, "3")
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			adapter := &netplanActivation{destination: "/etc/netplan/60-test.yaml", data: netplanRestoreData{NetworkFilesToRemove: []string{"10-netplan-old.network"}}}
			if err := adapter.PrepareActivation(testContext(t), Runtime{}, false); err == nil || !strings.HasPrefix(err.Error(), "Cannot find the network interfaces configured by /etc/netplan/60-test.yaml:") {
				t.Fatal(err)
			}
			if calls := takeCalls(t, tools); len(calls) != 0 {
				t.Fatal("preparation activated configuration", calls)
			}
		})
	}
}

func TestNetplanApplyAndRestoreDiagnosticsDoNotDiscloseSourceLines(t *testing.T) {
	for _, restoring := range []bool{false, true} {
		t.Run(map[bool]string{false: "activation", true: "restore"}[restoring], func(t *testing.T) {
			netplanFixture(t)
			tools := netplanStandIns(t)
			message := "/etc/netplan/other.yaml:1:128: Error in network definition: interface 'bridge0' is not defined"
			writeState(t, filepath.Join(tools, "fail-apply"), message+"\n{password: another-files-secret}\n             ^\n")
			adapter := &netplanActivation{destination: "/etc/netplan/60-test.yaml"}
			err := adapter.Activate(testContext(t), Runtime{}, restoring)
			if err == nil || err.Error() != "Cannot apply /etc/netplan/60-test.yaml with Netplan: "+message {
				t.Fatal(err)
			}
		})
	}
}

func TestNetplanCleanupWaitsForDelayedUnmanagedState(t *testing.T) {
	netplanFixture(t)
	tools := netplanStandIns(t)
	netplanTestLink(t, "second0", "3", "10-netplan-old.network", "configured")
	adapter := &netplanActivation{destination: "/etc/netplan/60-test.yaml", data: netplanRestoreData{NetworkFilesToRemove: []string{"10-netplan-old.network"}}}
	ctx, cancel := context.WithTimeout(testContext(t), 3*time.Second)
	defer cancel()
	if err := adapter.PrepareActivation(ctx, Runtime{}, false); err != nil {
		t.Fatal(err)
	}
	completed := make(chan error, 1)
	go func() { completed <- adapter.RemoveObsoleteVirtualDevices(ctx, Runtime{}, false) }()
	select {
	case err := <-completed:
		t.Fatal("cleanup did not wait for the managed link to be released", err)
	case <-time.After(100 * time.Millisecond):
	}
	// Replace atomically, as networkd does; a stale selected file is allowed
	// after apply, once a previously managed link becomes unmanaged.
	state := filepath.Join(netplanLinkStateDirectory, "3")
	writeState(t, state+".next", "ADMIN_STATE=unmanaged\nNETWORK_FILE="+filepath.Join(netplanGeneratedDirectory, "10-netplan-old.network")+"\n")
	if err := os.Rename(state+".next", state); err != nil {
		t.Fatal(err)
	}
	if err := <-completed; err != nil {
		t.Fatal(err)
	}
	if err := adapter.CheckActivatedConfiguration(ctx, Runtime{}, false); err != nil {
		t.Fatal(err)
	}
	if calls := takeCalls(t, tools); !reflect.DeepEqual(calls, []string{"ip address flush dev second0 scope global", "ip -o address show dev second0 scope global"}) {
		t.Fatal(calls)
	}
}
