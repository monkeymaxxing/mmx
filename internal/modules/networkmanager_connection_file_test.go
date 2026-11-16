package modules

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

const managerUUID = "11111111-1111-1111-1111-111111111111"
const otherManagerUUID = "22222222-2222-2222-2222-222222222222"

func managerKeyfile(name string) string {
	return "[connection]\nid=example\nuuid=" + managerUUID + "\ntype=ethernet\ninterface-name=" + name + "\n[ipv4]\nmethod=manual\naddress1=10.0.0.5/24\n"
}

func managerFixture(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	oldDir, oldLinks, oldVirtual, oldUser, oldGroup := networkManagerDirectory, networkManagerLinksDirectory, networkManagerVirtualDirectory, networkManagerRootUser, networkManagerRootGroup
	t.Cleanup(func() {
		networkManagerDirectory, networkManagerLinksDirectory, networkManagerVirtualDirectory, networkManagerRootUser, networkManagerRootGroup = oldDir, oldLinks, oldVirtual, oldUser, oldGroup
	})
	networkManagerDirectory, networkManagerLinksDirectory, networkManagerVirtualDirectory = filepath.Join(root, "connections"), filepath.Join(root, "links"), filepath.Join(root, "virtual")
	networkManagerRootUser, networkManagerRootGroup = Account(strconv.Itoa(os.Getuid())), Account(strconv.Itoa(os.Getgid()))
	for _, path := range []string{networkManagerDirectory, networkManagerLinksDirectory, networkManagerVirtualDirectory} {
		if err := os.MkdirAll(path, 0755); err != nil {
			t.Fatal(err)
		}
	}
	tools := managerStandInPrograms(t)
	writeState(t, filepath.Join(tools, "profiles"), "")
	writeState(t, filepath.Join(tools, "active"), "")
	return tools
}
func managerSettings(t *testing.T, tools, uuid, name string) {
	writeState(t, filepath.Join(tools, uuid+".settings"), "connection.type:ethernet\nconnection.interface-name:"+name+"\n")
}
func managerActive(name, uuid, state string) string {
	return "GENERAL.DEVICE:" + name + "\nGENERAL.STATE:" + state + "\nGENERAL.CON-UUID:" + uuid + "\n"
}
func TestNetworkManagerNamesAndFixedMetadata(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"System enp5s0.nmconnection", "a:b'\"$`\\.nmconnection", "-leading.nmconnection"} {
		if err := networkManagerFileName(name); err != nil {
			t.Fatal(name, err)
		}
	}
	for _, name := range []string{".hidden.nmconnection", "a\n.nmconnection", "../a.nmconnection", "a.conf", ""} {
		if networkManagerFileName(name) == nil {
			t.Fatal(name)
		}
	}
	file := (&NetworkManagerConnectionFile{Name: "System enp5s0.nmconnection"}).file()
	if file.Owner != "0" || file.Group != "0" || file.Mode != "0600" {
		t.Fatal(file)
	}
}
func TestNetworkManagerOfflineUsesCanonicalIdentityAndDeclaredUUIDOnly(t *testing.T) {
	tools := managerFixture(t)
	writeState(t, filepath.Join(tools, "canonical"), managerKeyfile(`nm\sbridge`))
	profile, err := checkNetworkManagerCandidate(context.Background(), Runtime{}, "file name.nmconnection", "[connection]\ntype=ignored\n")
	if err != nil || profile.Interface != "nm bridge" || profile.Type != "ethernet" || profile.UUID != "" {
		t.Fatal(profile, err)
	}
	profile, err = checkNetworkManagerCandidate(context.Background(), Runtime{}, "file", "\ufeff[connection]\n uuid = ignored\n")
	if err != nil || profile.UUID != managerUUID {
		t.Fatal(profile, err)
	}
	for _, text := range []string{"", protocol.TruncationMarker, "[connection]\ntype=ethernet\ninterface-name=../bad\n", "[connection]\ntype=ethernet\ninterface-name=bad\\q\n"} {
		writeState(t, filepath.Join(tools, "canonical"), text)
		if _, err := checkNetworkManagerCandidate(context.Background(), Runtime{}, "file", "candidate"); err == nil {
			t.Fatal(text)
		}
	}
}
func TestNetworkManagerRefusalsPrecedeEveryMutationInRunAndDryrun(t *testing.T) {
	for _, failure := range []string{"offline", "old inactive profile", "duplicate UUID", "profile query"} {
		t.Run(failure, func(t *testing.T) {
			tools := managerFixture(t)
			name := "System enp5s0.nmconnection"
			path := filepath.Join(networkManagerDirectory, name)
			content := managerKeyfile("eth0")
			reason := ""
			switch failure {
			case "offline":
				writeState(t, filepath.Join(tools, "fail-offline"), "native secret-bearing bad address")
				reason = "NetworkManager does not accept the proposed"
			case "old inactive profile":
				writeState(t, filepath.Join(tools, "profiles"), otherManagerUUID+":/etc/sysconfig/network-scripts/ifcfg-eth0\n")
				managerSettings(t, tools, otherManagerUUID, "eth0")
				reason = "nmcli connection migrate --plugin keyfile uuid " + otherManagerUUID
			case "duplicate UUID":
				writeState(t, filepath.Join(tools, "profiles"), managerUUID+":/another file.nmconnection\n")
				reason = "already loaded from /another file.nmconnection"
			case "profile query":
				writeState(t, filepath.Join(tools, "fail-query"), "")
				reason = "Reading NetworkManager"
			}
			for _, dryrun := range []bool{false, true} {
				service := &networkdOwedRestoreForTest{}
				run := Runtime{Dryrun: dryrun, BeforeMutation: func(string) error { t.Fatal("mutation after refusal"); return nil }}
				result := (&NetworkManagerConnectionFile{Name: name, Content: &content}).Run(context.Background(), NodeTask{Runtime: run, OwedRestores: service})
				if result.Status != "failed" || result.Change != "unchanged" || !strings.Contains(result.Reason, reason) || service.applied {
					t.Fatal(dryrun, result)
				}
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("file changed", err)
				}
				entries, _ := os.ReadDir(networkManagerDirectory)
				if len(entries) != 0 {
					t.Fatal("candidate written", entries)
				}
				for _, call := range takeCalls(t, tools) {
					if strings.HasPrefix(call, "nmcli connection ") {
						t.Fatal(call)
					}
				}
			}
		})
	}
}
func TestNetworkManagerUnchangedAndOwedRestore(t *testing.T) {
	tools := managerFixture(t)
	content := managerKeyfile("eth0")
	path := filepath.Join(networkManagerDirectory, "same.nmconnection")
	writeState(t, path, content)
	os.Chmod(path, 0600)
	input := &NetworkManagerConnectionFile{Name: filepath.Base(path), Content: &content}
	service := &networkdOwedRestoreForTest{}
	result := input.Run(context.Background(), NodeTask{OwedRestores: service})
	if result.Status != "succeeded" || result.Change != "unchanged" || len(takeCalls(t, tools)) != 0 {
		t.Fatal(result)
	}
	service.checkError = errors.New("restore service still owed")
	result = input.Run(context.Background(), NodeTask{OwedRestores: service})
	if result.Status != "failed" || !strings.Contains(result.Reason, "restore service still owed") {
		t.Fatal(result)
	}
}
func TestNetworkManagerRecordsOnlyConnectedProfilesAndNewDevices(t *testing.T) {
	tools := managerFixture(t)
	path := filepath.Join(networkManagerDirectory, "old.nmconnection")
	writeState(t, filepath.Join(tools, "profiles"), managerUUID+":"+path+"\n")
	managerSettings(t, tools, managerUUID, "eth0")
	for _, state := range []string{"70 (connecting)", "100 (connected)"} {
		writeState(t, filepath.Join(tools, "active"), managerActive("eth0", managerUUID, state))
		data, err := planNetworkManagerChange(context.Background(), Runtime{}, path, networkManagerProfile{Interface: "eth0"}, true, true)
		if err != nil || data.PreviousProfileActivated != (strings.HasPrefix(state, "100")) || data.DeviceToRemoveOnRestore != "eth0" {
			t.Fatal(data, err)
		}
		if !data.PreviousProfileLoaded || (len(data.PreviouslyActivatedProfiles) != 0) != data.PreviousProfileActivated {
			t.Fatal(data)
		}
	}
}
func TestNetworkManagerLoadChecksBeforeUpAndHandlesSpacedPaths(t *testing.T) {
	for _, failure := range []string{"none", "load exit", "load output", "whitespace output", "missing path", "journal position", "journal sync", "journal read", "malformed journal", "unknown section", "up"} {
		t.Run(failure, func(t *testing.T) {
			tools := managerFixture(t)
			path := filepath.Join(networkManagerDirectory, "a:b '$`.nmconnection")
			writeState(t, filepath.Join(tools, "after-load"), managerUUID+":"+path+"\n")
			switch failure {
			case "load exit":
				writeState(t, filepath.Join(tools, "fail-load"), "")
			case "load output":
				writeState(t, filepath.Join(tools, "load-output"), "Could not load file '"+path+"'")
			case "whitespace output":
				writeState(t, filepath.Join(tools, "load-output"), "\n")
			case "missing path":
				writeState(t, filepath.Join(tools, "after-load"), "")
			case "journal position":
				writeState(t, filepath.Join(tools, "fail-position"), "")
			case "journal sync":
				writeState(t, filepath.Join(tools, "fail-sync"), "")
			case "journal read":
				writeState(t, filepath.Join(tools, "fail-read"), "")
			case "malformed journal":
				writeState(t, filepath.Join(tools, "journal"), "bad json")
			case "unknown section":
				writeState(t, filepath.Join(tools, "journal"), `{"__CURSOR":"after","PRIORITY":"4","MESSAGE":"<warn>  [1791901610.2941] keyfile: unknown section"}`)
			case "up":
				writeState(t, filepath.Join(tools, "fail-up"), "No suitable device found")
			}
			adapter := &networkManagerActivation{destination: path, data: networkManagerRestoreData{ProposedFilePresent: true}}
			err := adapter.PrepareActivation(context.Background(), Runtime{}, false)
			if err == nil {
				err = adapter.Activate(context.Background(), Runtime{}, false)
			}
			if (err == nil) != (failure == "none") {
				t.Fatal(failure, err)
			}
			calls := strings.Join(takeCalls(t, tools), "\n")
			if failure != "none" && failure != "up" && strings.Contains(calls, "connection up") {
				t.Fatal("activated before rejection", calls)
			}
			if failure == "none" && strings.Index(calls, "journalctl --sync") > strings.Index(calls, "nmcli connection up") {
				t.Fatal(calls)
			}
			if strings.Contains(failure, "journal") && !strings.HasPrefix(err.Error(), "Cannot check whether NetworkManager accepted") {
				t.Fatal(err)
			}
		})
	}
}
func TestNetworkManagerJournalWarningScope(t *testing.T) {
	for _, test := range []struct {
		message string
		reject  bool
	}{
		{"<warn>  [1.23] keyfile: invalid setting", true}, {"<warn>  [1.23] dhcp4: other device failed", false},
		{"<warn>  [1.23] dhcp4: keyfile: inside another message", false}, {"<warn>  [1.23] audit: unrelated", false},
	} {
		t.Run(test.message, func(t *testing.T) {
			tools := managerFixture(t)
			path := filepath.Join(networkManagerDirectory, "file.nmconnection")
			writeState(t, filepath.Join(tools, "profiles"), managerUUID+":"+path+"\n")
			writeState(t, filepath.Join(tools, "journal"), fmt.Sprintf(`{"__CURSOR":"after","PRIORITY":"4","MESSAGE":%q}`, test.message))
			adapter := &networkManagerActivation{destination: path, cursor: "before", data: networkManagerRestoreData{ProposedFilePresent: true}}
			err := adapter.Activate(context.Background(), Runtime{}, false)
			if (err != nil) != test.reject {
				t.Fatal(err)
			}
		})
	}
}
func TestNetworkManagerRestoreFromDecodedRecord(t *testing.T) {
	for _, test := range []struct {
		name                                            string
		previousFile, previousLoaded, previousActivated bool
		saved                                           []string
		loadedOwn, loadedOther, activeOwn, activeOther  bool
		want                                            []string
	}{
		{"previous active", true, true, true, []string{managerUUID}, true, false, false, false, []string{"up uuid " + managerUUID}},
		{"previous inactive now active", true, true, false, nil, true, false, true, false, []string{"down uuid " + managerUUID}},
		{"displaced manual profile", false, false, false, []string{otherManagerUUID}, false, true, false, false, []string{"up uuid " + otherManagerUUID}},
		{"displaced automatically restored", false, false, false, []string{otherManagerUUID}, false, true, false, true, nil},
		{"removed bridge with port or VLAN", true, true, true, []string{managerUUID, otherManagerUUID}, true, true, false, false, []string{"up uuid " + managerUUID, "up uuid " + otherManagerUUID}},
		{"bridge dependent already returned", true, true, true, []string{managerUUID, otherManagerUUID}, true, true, false, true, []string{"up uuid " + managerUUID}},
		{"vanished saved profile", true, true, true, []string{otherManagerUUID, managerUUID}, true, false, false, false, []string{"up uuid " + managerUUID}},
	} {
		t.Run(test.name, func(t *testing.T) {
			tools := managerFixture(t)
			path := filepath.Join(networkManagerDirectory, "System eth0.nmconnection")
			profiles, active := "", ""
			if test.loadedOwn {
				profiles += managerUUID + ":" + path + "\n"
			}
			if test.loadedOther {
				profiles += otherManagerUUID + ":/another.nmconnection\n"
			}
			if test.activeOwn {
				active += managerActive("eth0", managerUUID, "100 (connected)")
			}
			if test.activeOther {
				active += managerActive("eth1", otherManagerUUID, "100 (connected)")
			}
			writeState(t, filepath.Join(tools, "profiles"), profiles)
			writeState(t, filepath.Join(tools, "active"), active)
			writeState(t, filepath.Join(tools, "fail-position"), "")
			writeState(t, filepath.Join(tools, "fail-read"), "")
			data := networkManagerRestoreData{PreviousFilePresent: test.previousFile, PreviousProfileLoaded: test.previousLoaded,
				PreviousProfileActivated: test.previousActivated, PreviouslyActivatedProfiles: test.saved}
			saved, _ := json.Marshal(data)
			adapter, err := newNetworkManagerActivation(path, saved)
			if err != nil {
				t.Fatal(err)
			}
			for _, call := range []func(context.Context, Runtime, bool) error{adapter.PrepareActivation, adapter.Activate, adapter.RemoveObsoleteVirtualDevices, adapter.CheckActivatedConfiguration} {
				if err := call(context.Background(), Runtime{}, true); err != nil {
					t.Fatal(err)
				}
			}
			if actions := managerConnectionActions(t, tools); !reflect.DeepEqual(actions, test.want) {
				t.Fatal(actions, test.want)
			}
			activity, err := networkManagerProfileActivity(context.Background(), Runtime{})
			if err != nil {
				t.Fatal(err)
			}
			if activity[managerUUID].activated != test.previousActivated {
				t.Fatal(activity)
			}
			if test.loadedOther && !activity[otherManagerUUID].activated {
				t.Fatal("saved loaded profile was not restored", activity)
			}
		})
	}
}

func managerConnectionActions(t *testing.T, tools string) []string {
	t.Helper()
	var actions []string
	for _, call := range takeCalls(t, tools) {
		if strings.HasPrefix(call, "journalctl") {
			t.Fatal("restore checked logs", call)
		}
		if strings.HasPrefix(call, "nmcli connection up") || strings.HasPrefix(call, "nmcli connection down") {
			actions = append(actions, strings.TrimPrefix(call, "nmcli connection "))
		}
	}
	return actions
}
func managerDevice(t *testing.T, name string, virtual bool) string {
	target := filepath.Join(filepath.Dir(networkManagerVirtualDirectory), "physical", name)
	if virtual {
		target = filepath.Join(networkManagerVirtualDirectory, name)
	}
	if err := os.MkdirAll(target, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(networkManagerLinksDirectory, name)
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	return path
}
func TestNetworkManagerVirtualDeviceCleanup(t *testing.T) {
	for _, mode := range []string{"native removal", "explicit deletion", "physical", "shared", "delete failed", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			// Advance the real five-second limit on Go's fake clock. External
			// commands still run normally once the device wait has finished.
			synctest.Test(t, func(t *testing.T) {
				tools := managerFixture(t)
				path := managerDevice(t, "mmxdummy0", mode != "physical")
				adapter := &networkManagerActivation{destination: filepath.Join(networkManagerDirectory, "device.nmconnection"), data: networkManagerRestoreData{DeviceToRemoveOnRestore: "mmxdummy0"}}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if mode == "native removal" {
					go func() {
						time.Sleep(50 * time.Millisecond)
						if err := os.Remove(path); err != nil {
							t.Error(err)
						}
					}()
				}
				if mode == "explicit deletion" {
					writeState(t, filepath.Join(tools, "delete-path"), path)
				}
				if mode == "shared" {
					writeState(t, filepath.Join(tools, "profiles"), otherManagerUUID+":/other.nmconnection\n")
					managerSettings(t, tools, otherManagerUUID, "mmxdummy0")
				}
				if mode == "canceled" {
					cancel()
				}
				start := time.Now()
				err := adapter.RemoveObsoleteVirtualDevices(ctx, Runtime{}, true)
				wantWait := 5 * time.Second
				switch mode {
				case "physical", "canceled":
					wantWait = 0
				case "native removal":
					wantWait = 100 * time.Millisecond
				}
				if elapsed := time.Since(start); elapsed != wantWait {
					t.Fatalf("device cleanup waited %s; want %s", elapsed, wantWait)
				}
				wantError := mode == "shared" || mode == "delete failed" || mode == "canceled"
				if (err != nil) != wantError {
					t.Fatal(mode, err)
				}
				calls := strings.Join(takeCalls(t, tools), "\n")
				if (mode == "physical" || mode == "shared" || mode == "native removal" || mode == "canceled") && strings.Contains(calls, "device delete") {
					t.Fatal(calls)
				}
			})
		})
	}
}
func TestNetworkManagerTemplateAndMissingPrerequisiteDryrun(t *testing.T) {
	tools := managerFixture(t)
	input := &NetworkManagerConnectionFile{Name: "template.nmconnection", Template: "example.tmpl", TemplateText: strings.ReplaceAll(managerKeyfile("eth0"), "eth0", "{{ .vars.interface }}")}
	result := input.Run(context.Background(), NodeTask{Runtime: Runtime{Dryrun: true}, OwedRestores: &networkdOwedRestoreForTest{}, Scope: value.Scope{"vars": map[string]any{"interface": "eth1"}}})
	if result.Status != "succeeded" || result.Change != "would_change" || !strings.Contains(readState(t, filepath.Join(tools, "candidate")), "interface-name=eth1") {
		t.Fatal(result)
	}
	writeState(t, filepath.Join(tools, "inactive-service"), "")
	result = input.Run(context.Background(), NodeTask{Runtime: Runtime{Dryrun: true}, OwedRestores: &networkdOwedRestoreForTest{}, Scope: value.Scope{"vars": map[string]any{"interface": "eth1"}}})
	if result.Status != "not_checked" {
		t.Fatal(result)
	}
}

type managerApplyInspection struct {
	t      *testing.T
	path   string
	called bool
}

func (s *managerApplyInspection) CheckChangeAllowed(context.Context, string) error { return nil }
func (s *managerApplyInspection) ApplyChange(ctx context.Context, change ChangeWithOwedRestore) protocol.Result {
	s.called = true
	if _, err := os.Stat(s.path); !os.IsNotExist(err) {
		s.t.Fatal("installed before handing to mechanism", err)
	}
	var data networkManagerRestoreData
	if err := json.Unmarshal(change.ModuleRestoreData, &data); err != nil || !reflect.DeepEqual(data.PreviouslyActivatedProfiles, []string{otherManagerUUID}) {
		s.t.Fatal(data, err)
	}
	if change.RestoreConfirmationWait != 3*time.Second {
		s.t.Fatal(change.RestoreConfirmationWait)
	}
	if _, err := change.PreparedFile.SavePreviousFile(ctx, filepath.Join(s.t.TempDir(), "previous")); err != nil {
		s.t.Fatal(err)
	}
	return change.PreparedFile.Install(ctx)
}
func TestNetworkManagerRunHandsPreparedFileAndActivatedProfilesToMechanism(t *testing.T) {
	tools := managerFixture(t)
	managerDevice(t, "eth1", false)
	writeState(t, filepath.Join(tools, "active"), managerActive("eth1", otherManagerUUID, "100 (connected)"))
	content := managerKeyfile("eth1")
	path := filepath.Join(networkManagerDirectory, "System eth1.nmconnection")
	service := &managerApplyInspection{t: t, path: path}
	result := (&NetworkManagerConnectionFile{Name: filepath.Base(path), Content: &content, RestoreUnlessControllerConfirmsWithin: "3s"}).Run(context.Background(), NodeTask{OwedRestores: service})
	if result.Status != "succeeded" || result.Change != "changed" || !service.called || readState(t, path) != content {
		t.Fatal(result)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
	for _, call := range takeCalls(t, tools) {
		if strings.HasPrefix(call, "nmcli connection ") {
			t.Fatal("Run activated", call)
		}
	}
}

func TestNetworkManagerUnloadIsTargetedAndRequiresProfileGone(t *testing.T) {
	for _, retained := range []bool{false, true} {
		t.Run(fmt.Sprint(retained), func(t *testing.T) {
			tools := managerFixture(t)
			path := filepath.Join(networkManagerDirectory, "removed profile.nmconnection")
			if retained {
				writeState(t, filepath.Join(tools, "profiles"), managerUUID+":"+path+"\n")
			}
			adapter := &networkManagerActivation{destination: path, cursor: "before"}
			err := adapter.Activate(context.Background(), Runtime{}, false)
			if (err != nil) != retained {
				t.Fatal(err)
			}
			calls := takeCalls(t, tools)
			if calls[0] != "nmcli connection load "+path {
				t.Fatal(calls)
			}
			for _, call := range calls {
				if strings.Contains(call, "connection reload") || strings.Contains(call, "connection up") {
					t.Fatal(call)
				}
			}
		})
	}
}
func TestNetworkManagerQueriesRejectIncompleteAnswers(t *testing.T) {
	tools := managerFixture(t)
	for _, text := range []string{"no colon", "not-a-uuid:/path", protocol.TruncationMarker} {
		writeState(t, filepath.Join(tools, "profiles"), text)
		if _, err := networkManagerProfiles(context.Background(), Runtime{}); err == nil {
			t.Fatal(text)
		}
	}
	for _, text := range []string{"GENERAL.DEVICE:eth0\n", managerActive("eth0", "bad uuid", "100 (connected)"), "GENERAL.STATE:unknown\n", protocol.TruncationMarker} {
		writeState(t, filepath.Join(tools, "active"), text)
		if _, err := networkManagerProfileActivity(context.Background(), Runtime{}); err == nil {
			t.Fatal(text)
		}
	}
}
func TestNetworkManagerJournalCursorAndPathWarnings(t *testing.T) {
	for _, anchor := range []bool{false, true} {
		t.Run(fmt.Sprint(anchor), func(t *testing.T) {
			tools := managerFixture(t)
			path := filepath.Join(networkManagerDirectory, "System eth1.nmconnection")
			writeState(t, filepath.Join(tools, "profiles"), managerUUID+":"+path+"\n")
			warning := fmt.Sprintf(`{"__CURSOR":"after","PRIORITY":"4","MESSAGE":%q}`, "Failed reading '"+path+"'.")
			if anchor {
				warning = `{"__CURSOR":"before","PRIORITY":"6","MESSAGE":"position"}` + "\n" + warning
			}
			writeState(t, filepath.Join(tools, "journal"), warning)
			adapter := &networkManagerActivation{destination: path, cursor: "before", data: networkManagerRestoreData{ProposedFilePresent: true}}
			if err := adapter.Activate(context.Background(), Runtime{}, false); err == nil || !strings.Contains(err.Error(), "Failed reading") {
				t.Fatal(err)
			}
		})
	}
}
func TestNetworkManagerWithoutInterfaceDoesNotInspectOldFormatProfiles(t *testing.T) {
	tools := managerFixture(t)
	writeState(t, filepath.Join(tools, "profiles"), otherManagerUUID+":/etc/sysconfig/network-scripts/ifcfg-eth0\n")
	if _, err := planNetworkManagerChange(context.Background(), Runtime{}, filepath.Join(networkManagerDirectory, "new.nmconnection"), networkManagerProfile{}, true, false); err != nil {
		t.Fatal(err)
	}
	for _, call := range takeCalls(t, tools) {
		if strings.Contains(call, "connection show uuid") {
			t.Fatal(call)
		}
	}
}

func TestNetworkManagerRestoreRecordRejectsUnsafeIdentities(t *testing.T) {
	managerFixture(t)
	for _, test := range []struct{ path, data string }{
		{"/tmp/elsewhere.nmconnection", `{}`},
		{filepath.Join(networkManagerDirectory, ".hidden.nmconnection"), `{}`},
		{filepath.Join(networkManagerDirectory, "good.nmconnection"), `{"virtual_device_to_remove_after_file_change":"../eth0"}`},
		{filepath.Join(networkManagerDirectory, "good.nmconnection"), `{"introduced_device_to_remove_on_restore":"../eth0"}`},
		{filepath.Join(networkManagerDirectory, "good.nmconnection"), `{"previously_activated_profile_uuids":["--bad-option"]}`},
		{filepath.Join(networkManagerDirectory, "good.nmconnection"), `{"unexpected":true}`},
	} {
		if _, err := newNetworkManagerActivation(test.path, []byte(test.data)); err == nil {
			t.Fatal(test)
		}
	}
}

const thirdManagerUUID = "33333333-3333-3333-3333-333333333333"
const fourthManagerUUID = "44444444-4444-4444-4444-444444444444"

func TestNetworkManagerRecordsEveryActivatedProfileWithoutDiscoveringDependencies(t *testing.T) {
	tools := managerFixture(t)
	path := filepath.Join(networkManagerDirectory, "bridge.nmconnection")
	writeState(t, filepath.Join(tools, "profiles"), managerUUID+":"+path+"\n")
	managerSettings(t, tools, managerUUID, "mmxbr0")
	writeState(t, filepath.Join(tools, "active"), managerActive("mmxbr0", managerUUID, "100 (connected)")+
		managerActive("port", otherManagerUUID, "100 (connected)")+managerActive("vlan", thirdManagerUUID, "100 (connected)")+
		managerActive("starting", fourthManagerUUID, "70 (connecting)")+managerActive("second-use", thirdManagerUUID, "100 (connected)"))
	data, err := planNetworkManagerChange(context.Background(), Runtime{}, path, networkManagerProfile{}, false, true)
	if err != nil || !data.PreviousProfileLoaded || !data.PreviousProfileActivated ||
		!reflect.DeepEqual(data.PreviouslyActivatedProfiles, []string{managerUUID, otherManagerUUID, thirdManagerUUID}) {
		t.Fatal(data, err)
	}
}

func TestNetworkManagerActivitySnapshotFailurePrecedesMutation(t *testing.T) {
	for _, malformed := range []bool{false, true} {
		t.Run(fmt.Sprint(malformed), func(t *testing.T) {
			tools := managerFixture(t)
			path := filepath.Join(networkManagerDirectory, "bridge.nmconnection")
			writeState(t, path, "previous contents")
			if malformed {
				writeState(t, filepath.Join(tools, "active"), "GENERAL.DEVICE:eth1\n")
			} else {
				writeState(t, filepath.Join(tools, "fail-activity"), "")
			}
			for _, dryrun := range []bool{false, true} {
				service := &networkdOwedRestoreForTest{}
				run := Runtime{Dryrun: dryrun, BeforeMutation: func(string) error { t.Fatal("mutation after snapshot failure"); return nil }}
				result := (&NetworkManagerConnectionFile{Name: filepath.Base(path), State: "absent"}).Run(context.Background(), NodeTask{Runtime: run, OwedRestores: service})
				if result.Status != "failed" || result.Change != "unchanged" || service.applied || readState(t, path) != "previous contents" {
					t.Fatal(result)
				}
			}
		})
	}
}

func TestNetworkManagerRestoreDeactivatesAndVerifiesPreviouslyInactiveProfile(t *testing.T) {
	for _, state := range []string{"40 (prepare)", "70 (connecting)", "100 (connected)"} {
		t.Run(state, func(t *testing.T) {
			tools := managerFixture(t)
			path := filepath.Join(networkManagerDirectory, "inactive.nmconnection")
			writeState(t, filepath.Join(tools, "profiles"), managerUUID+":"+path+"\n")
			writeState(t, filepath.Join(tools, "active"), managerActive("eth0", managerUUID, state))
			saved, _ := json.Marshal(networkManagerRestoreData{PreviousFilePresent: true, PreviousProfileLoaded: true})
			adapter, err := newNetworkManagerActivation(path, saved)
			if err != nil {
				t.Fatal(err)
			}
			if err := adapter.Activate(context.Background(), Runtime{}, true); err != nil {
				t.Fatal(err)
			}
			if got := managerConnectionActions(t, tools); !reflect.DeepEqual(got, []string{"down uuid " + managerUUID}) {
				t.Fatal(got)
			}
			activity, err := networkManagerProfileActivity(context.Background(), Runtime{})
			if err != nil || activity[managerUUID].inUse {
				t.Fatal(activity, err)
			}
		})
	}
}

func TestNetworkManagerRestoreRefusesFailedOrIncompleteDeactivation(t *testing.T) {
	for _, failure := range []string{"command failed", "still activating", "still deactivating"} {
		t.Run(failure, func(t *testing.T) {
			tools := managerFixture(t)
			path := filepath.Join(networkManagerDirectory, "inactive.nmconnection")
			writeState(t, filepath.Join(tools, "profiles"), managerUUID+":"+path+"\n")
			writeState(t, filepath.Join(tools, "active"), managerActive("eth0", managerUUID, "70 (connecting)"))
			if failure == "command failed" {
				writeState(t, filepath.Join(tools, "fail-down"), "native deactivation failure")
			} else {
				state := "70 (connecting)"
				if failure == "still deactivating" {
					state = "110 (deactivating)"
				}
				writeState(t, filepath.Join(tools, "after-down"), managerActive("eth0", managerUUID, state))
			}
			adapter := &networkManagerActivation{destination: path, data: networkManagerRestoreData{PreviousFilePresent: true, PreviousProfileLoaded: true}}
			if err := adapter.Activate(context.Background(), Runtime{}, true); err == nil || !strings.Contains(err.Error(), "deactivate the profile of "+path) {
				t.Fatal(err)
			}
		})
	}
}

func TestNetworkManagerRestoreOfPreviouslyUnloadedFile(t *testing.T) {
	for _, retainedCandidate := range []bool{false, true} {
		t.Run(fmt.Sprint(retainedCandidate), func(t *testing.T) {
			tools := managerFixture(t)
			path := filepath.Join(networkManagerDirectory, "broken.nmconnection")
			writeState(t, path, "this is not a keyfile\n")
			writeState(t, filepath.Join(tools, "profiles"), otherManagerUUID+":/other.nmconnection\n")
			writeState(t, filepath.Join(tools, "active"), managerActive("eth0", otherManagerUUID, "100 (connected)"))
			data, err := planNetworkManagerChange(context.Background(), Runtime{}, path, networkManagerProfile{UUID: managerUUID, Interface: "eth0"}, true, true)
			if err != nil || data.PreviousProfileLoaded || !data.PreviousFilePresent {
				t.Fatal(data, err)
			}
			profiles, active := otherManagerUUID+":/other.nmconnection\n", ""
			if retainedCandidate {
				profiles += managerUUID + ":" + path + "\n"
				active = managerActive("eth0", managerUUID, "70 (connecting)")
			}
			writeState(t, filepath.Join(tools, "profiles"), profiles)
			writeState(t, filepath.Join(tools, "active"), active)
			writeState(t, filepath.Join(tools, "load-output"), "Could not load file '"+path+"'\n")
			saved, _ := json.Marshal(data)
			adapter, err := newNetworkManagerActivation(path, saved)
			if err != nil {
				t.Fatal(err)
			}
			if err := adapter.Activate(context.Background(), Runtime{}, true); err != nil {
				t.Fatal(err)
			}
			activity, err := networkManagerProfileActivity(context.Background(), Runtime{})
			if err != nil || activity[managerUUID].inUse || !activity[otherManagerUUID].activated || readState(t, path) != "this is not a keyfile\n" {
				t.Fatal(activity, err)
			}
		})
	}
}

func TestNetworkManagerRestoreStillChecksLoadForPreviouslyLoadedOrAbsentFile(t *testing.T) {
	for _, present := range []bool{false, true} {
		for _, failure := range []string{"output", "mapping"} {
			t.Run(fmt.Sprint(present)+failure, func(t *testing.T) {
				tools := managerFixture(t)
				path := filepath.Join(networkManagerDirectory, "previous.nmconnection")
				if failure == "output" {
					writeState(t, filepath.Join(tools, "load-output"), "Could not load file")
				}
				if !present {
					writeState(t, filepath.Join(tools, "profiles"), managerUUID+":"+path+"\n")
				}
				adapter := &networkManagerActivation{destination: path, data: networkManagerRestoreData{PreviousFilePresent: present, PreviousProfileLoaded: present}}
				if err := adapter.Activate(context.Background(), Runtime{}, true); err == nil {
					t.Fatal("bad restored load accepted")
				}
			})
		}
	}
}

func TestNetworkManagerRestoreReactivatesLoadedProfilesAfterVanishedProfile(t *testing.T) {
	tools := managerFixture(t)
	path := filepath.Join(networkManagerDirectory, "absent.nmconnection")
	writeState(t, filepath.Join(tools, "profiles"), otherManagerUUID+":/port.nmconnection\n"+thirdManagerUUID+":/vlan.nmconnection\n")
	saved, _ := json.Marshal(networkManagerRestoreData{PreviouslyActivatedProfiles: []string{managerUUID, otherManagerUUID, thirdManagerUUID}})
	adapter, err := newNetworkManagerActivation(path, saved)
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.Activate(context.Background(), Runtime{}, true); err != nil {
		t.Fatal(err)
	}
	if got := managerConnectionActions(t, tools); !reflect.DeepEqual(got, []string{"up uuid " + otherManagerUUID, "up uuid " + thirdManagerUUID}) {
		t.Fatal(got)
	}
}

func TestNetworkManagerRestoreRechecksActivityBetweenProfileActivations(t *testing.T) {
	tools := managerFixture(t)
	path := filepath.Join(networkManagerDirectory, "absent.nmconnection")
	writeState(t, filepath.Join(tools, "profiles"), otherManagerUUID+":/bridge.nmconnection\n"+thirdManagerUUID+":/vlan.nmconnection\n")
	writeState(t, filepath.Join(tools, "after-up"), managerActive("bridge", otherManagerUUID, "100 (connected)")+managerActive("vlan", thirdManagerUUID, "100 (connected)"))
	adapter := &networkManagerActivation{destination: path, data: networkManagerRestoreData{PreviouslyActivatedProfiles: []string{otherManagerUUID, thirdManagerUUID}}}
	if err := adapter.Activate(context.Background(), Runtime{}, true); err != nil {
		t.Fatal(err)
	}
	if got := managerConnectionActions(t, tools); !reflect.DeepEqual(got, []string{"up uuid " + otherManagerUUID}) {
		t.Fatal(got)
	}
}

func TestNetworkManagerRestoreAfterInterfaceRename(t *testing.T) {
	tools := managerFixture(t)
	path := filepath.Join(networkManagerDirectory, "renamed.nmconnection")
	managerDevice(t, "mmxold0", true)
	managerSettings(t, tools, managerUUID, "mmxold0")
	writeState(t, filepath.Join(tools, "profiles"), managerUUID+":"+path+"\n")
	writeState(t, filepath.Join(tools, "active"), managerActive("mmxold0", managerUUID, "100 (connected)"))
	data, err := planNetworkManagerChange(context.Background(), Runtime{}, path, networkManagerProfile{UUID: managerUUID, Interface: "mmxnew0"}, true, true)
	if err != nil || data.DeviceToRemove != "mmxold0" || data.DeviceToRemoveOnRestore != "mmxnew0" {
		t.Fatal(data, err)
	}
	newDevice := managerDevice(t, "mmxnew0", true)
	writeState(t, filepath.Join(tools, "active"), managerActive("mmxnew0", managerUUID, "100 (connected)"))
	saved, _ := json.Marshal(data)
	adapter, err := newNetworkManagerActivation(path, saved)
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.Activate(context.Background(), Runtime{}, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readState(t, filepath.Join(tools, "active")), "GENERAL.DEVICE:mmxold0\n") {
		t.Fatal("old interface not activated")
	}
	// Native activation normally removes the obsolete device; exercise the
	// guarded explicit deletion as well when it remains after a rename.
	writeState(t, filepath.Join(tools, "delete-path"), newDevice)
	synctest.Test(t, func(t *testing.T) {
		if err := adapter.RemoveObsoleteVirtualDevices(context.Background(), Runtime{}, true); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := os.Stat(newDevice); !os.IsNotExist(err) {
		t.Fatal("introduced device remained", err)
	}
}

func TestNetworkManagerRestorePreservesPreexistingVirtualDevice(t *testing.T) {
	tools := managerFixture(t)
	path := filepath.Join(networkManagerDirectory, "new.nmconnection")
	device := managerDevice(t, "mmxexisting0", true)
	data, err := planNetworkManagerChange(context.Background(), Runtime{}, path, networkManagerProfile{UUID: managerUUID, Interface: "mmxexisting0"}, true, false)
	if err != nil || data.DeviceToRemoveOnRestore != "" {
		t.Fatal(data, err)
	}
	saved, _ := json.Marshal(data)
	adapter, err := newNetworkManagerActivation(path, saved)
	if err != nil {
		t.Fatal(err)
	}
	run := Runtime{BeforeMutation: func(string) error { t.Fatal("pre-existing device deletion"); return nil }}
	if err := adapter.RemoveObsoleteVirtualDevices(context.Background(), run, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(device); err != nil {
		t.Fatal(err)
	}
	for _, call := range takeCalls(t, tools) {
		if strings.Contains(call, "device delete") {
			t.Fatal(call)
		}
	}
}

func TestNetworkManagerActivationFailureTextAndJournalHint(t *testing.T) {
	for _, other := range []bool{false, true} {
		t.Run(fmt.Sprint(other), func(t *testing.T) {
			tools := managerFixture(t)
			path := filepath.Join(networkManagerDirectory, "failed.nmconnection")
			writeState(t, filepath.Join(tools, "profiles"), managerUUID+":"+path+"\n"+otherManagerUUID+":/other.nmconnection\n")
			diagnostic := "Error: Connection activation failed: No suitable device found."
			writeState(t, filepath.Join(tools, "fail-up"), diagnostic+"\nHint: use 'journalctl -xe NM_CONNECTION=example + NM_DEVICE=eth0' to get more details.\n")
			adapter := &networkManagerActivation{destination: path, cursor: "before", data: networkManagerRestoreData{ProposedFilePresent: true}}
			want := "NetworkManager could not activate the profile of " + path + ": " + diagnostic
			if other {
				adapter.data = networkManagerRestoreData{PreviousFilePresent: true, PreviousProfileLoaded: true, PreviouslyActivatedProfiles: []string{otherManagerUUID}}
				want = "NetworkManager could not activate profile " + otherManagerUUID + " again after the restore of " + path + ": " + diagnostic
			}
			if err := adapter.Activate(context.Background(), Runtime{}, other); err == nil || err.Error() != want {
				t.Fatal(err, want)
			}
		})
	}
	for _, diagnostic := range []string{"Hint: repair the address before reading journalctl.", "Error: journalctl failed to read the service log."} {
		err := networkManagerConnectionFailure(CommandOutput{ExitCode: 4, StandardError: diagnostic})
		if err == nil || err.Error() != diagnostic {
			t.Fatal(err)
		}
	}
}
