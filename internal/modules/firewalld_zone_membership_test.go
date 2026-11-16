package modules

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mmx/internal/protocol"
	"mmx/internal/testfirewalld"
)

// The stand-in implements firewalld's two independent layers. State assertions
// read its data directly, independently of the module's parser and results.

type membershipFixture struct {
	root  string
	state map[string]map[string]map[string][]string
}

func newMembershipFixture(t *testing.T) *membershipFixture {
	t.Helper()
	f := &membershipFixture{root: t.TempDir(), state: map[string]map[string]map[string][]string{}}
	for _, layer := range []string{"running", "stored"} {
		f.state[layer] = map[string]map[string][]string{}
		for _, zone := range []string{"public", "internal", "trusted", "drop"} {
			f.state[layer][zone] = map[string][]string{"interface": {}, "source": {}}
		}
	}
	f.save(t)
	testfirewalld.Install(t, f.root)
	writeStandInProgram(t, filepath.Join(f.root, "systemctl"), "#!/bin/sh\necho inactive\n")
	setWorkerEnvironment(t, "PATH", f.root)
	return f
}
func (f *membershipFixture) save(t *testing.T) {
	t.Helper()
	data, _ := json.Marshal(f.state)
	writeState(t, filepath.Join(f.root, "state"), string(data))
}
func (f *membershipFixture) read(t *testing.T) {
	t.Helper()
	if err := json.Unmarshal([]byte(readState(t, filepath.Join(f.root, "state"))), &f.state); err != nil {
		t.Fatal(err)
	}
}
func (f *membershipFixture) assert(t *testing.T, layer, zone, kind string, names ...string) {
	t.Helper()
	f.read(t)
	if got := f.state[layer][zone][kind]; !reflect.DeepEqual(got, append([]string{}, names...)) {
		t.Fatalf("%s %s %s: got %v want %v", layer, zone, kind, got, names)
	}
}
func (f *membershipFixture) calls(t *testing.T) string {
	return readState(t, filepath.Join(f.root, "calls"))
}
func (f *membershipFixture) nm(t *testing.T, filename string) {
	script := "#!/bin/sh\ncase \"$*\" in\n '-t -f RUNNING general') echo running;;\n '-g GENERAL.CON-UUID device show eth1') echo profile-uuid;;\n '-t -f UUID,FILENAME connection show') echo 'profile-uuid:" + filename + "';;\n *) exit 10;;\nesac\n"
	writeStandInProgram(t, filepath.Join(f.root, "nmcli"), script)
}

type membershipServiceForTest struct {
	saved   json.RawMessage
	calls   int
	result  *protocol.Result
	refusal error
	runtime Runtime
}

func (s *membershipServiceForTest) CheckChangeAllowed(_ context.Context, destination string) error {
	if destination != "" {
		return errors.New("membership passed a destination")
	}
	return s.refusal
}
func (s *membershipServiceForTest) ApplyChange(ctx context.Context, change ChangeWithOwedRestore) protocol.Result {
	s.calls++
	if change.ChangeSubject != "firewalld zone membership" || change.RestoreAfterUnit != "firewalld.service" ||
		change.RestoreSuccessSentence != "The previous firewalld zone membership is restored in the running firewall and stored configuration." {
		return protocol.Failed(errors.New("membership did not supply its restore descriptions and ordering"))
	}
	if s.result != nil {
		return *s.result
	}
	s.saved = change.ModuleRestoreData
	if change.PreparedFile != nil {
		return protocol.Failed(errors.New("membership prepared a file"))
	}
	activation, err := newFirewalldMembershipActivation("", s.saved)
	if err == nil {
		err = activation.PrepareActivation(ctx, s.runtime, false)
	}
	if err == nil {
		err = activation.Activate(ctx, s.runtime, false)
	}
	if err == nil {
		err = activation.CheckActivatedConfiguration(ctx, s.runtime, false)
	}
	if err != nil {
		return protocol.Failed(err)
	}
	return protocol.Success("changed")
}
func runMembership(t *testing.T, f FirewalldZoneMembership, service *membershipServiceForTest, dryrun bool) protocol.Result {
	t.Helper()
	if err := f.Prepare(""); err != nil {
		t.Fatal(err)
	}
	return f.Run(testContext(t), NodeTask{Runtime: Runtime{Dryrun: dryrun}, OwedRestores: service})
}
func TestFirewalldMembershipBothLayersMovesRemovalsAndRestore(t *testing.T) {
	t.Parallel()
	fixture := newMembershipFixture(t)
	fixture.state["running"]["trusted"]["source"] = []string{"10.0.0.0/8"}
	fixture.state["stored"]["public"]["source"] = []string{"10.0.0.0/8"}
	fixture.state["running"]["internal"]["source"] = []string{"192.0.2.0/24"}
	fixture.state["stored"]["internal"]["interface"] = []string{"eth9"}
	fixture.save(t)
	before := readState(t, filepath.Join(fixture.root, "state"))
	service := &membershipServiceForTest{}
	input := FirewalldZoneMembership{Zone: "internal", Interfaces: []string{"eth1"}, SourceNetworks: []string{"10.0.0.0/8"}, InterfacesNotListed: "remove", SourceNetworksNotListed: "remove"}
	preview := runMembership(t, input, service, true)
	if preview.Change != "would_change" || service.calls != 0 || !strings.Contains(preview.Reason, "Reachability after the change was not checked") {
		t.Fatal(preview)
	}
	if readState(t, filepath.Join(fixture.root, "state")) != before {
		t.Fatal("dryrun changed membership")
	}
	result := runMembership(t, input, service, false)
	if result.Status != "succeeded" || result.Change != "changed" || !reflect.DeepEqual(preview.Value, result.Value) {
		t.Fatal(result)
	}
	for _, layer := range []string{"running", "stored"} {
		fixture.assert(t, layer, "internal", "interface", "eth1")
		fixture.assert(t, layer, "internal", "source", "10.0.0.0/8")
	}
	calls := fixture.calls(t)
	if strings.Contains(calls, "--reload") || strings.Contains(calls, "--add-source") || strings.Index(calls, "--remove-") < strings.LastIndex(calls, "--change-") {
		t.Fatal(calls)
	}
	if unchanged := runMembership(t, input, service, false); unchanged.Change != "unchanged" || service.calls != 1 {
		t.Fatal(unchanged, service.calls)
	}
	// A restart copied permanent membership into runtime. The saved differing
	// layers must be restored, including the runtime-only removed network.
	if err := firewalldWrite(testContext(t), Runtime{}, "--reload"); err != nil {
		t.Fatal(err)
	}
	activation, err := newFirewalldMembershipActivation("", service.saved)
	if err != nil {
		t.Fatal(err)
	}
	if err = activation.PrepareActivation(testContext(t), Runtime{}, true); err != nil {
		t.Fatal(err)
	}
	if err = activation.Activate(testContext(t), Runtime{}, true); err != nil {
		t.Fatal(err)
	}
	if err = activation.CheckActivatedConfiguration(testContext(t), Runtime{}, true); err != nil {
		t.Fatal(err)
	}
	fixture.assert(t, "running", "trusted", "source", "10.0.0.0/8")
	fixture.assert(t, "stored", "public", "source", "10.0.0.0/8")
	fixture.assert(t, "running", "internal", "source", "192.0.2.0/24")
	fixture.assert(t, "stored", "internal", "interface", "eth9")
	fixture.assert(t, "running", "internal", "interface")
}
func TestFirewalldMembershipOnlyOneLayerChangedStillRestoresBoth(t *testing.T) {
	t.Parallel()
	fixture := newMembershipFixture(t)
	fixture.state["stored"]["internal"]["source"] = []string{"10.0.0.0/8"}
	fixture.save(t)
	service := &membershipServiceForTest{}
	result := runMembership(t, FirewalldZoneMembership{Zone: "internal", SourceNetworks: []string{"10.0.0.0/8"}, SourceNetworksNotListed: "keep"}, service, false)
	if result.Change != "changed" {
		t.Fatal(result)
	}
	var data firewalldMembershipRestoreData
	json.Unmarshal(service.saved, &data)
	if data.Changes[0].Previous[0].Zone != "" || data.Changes[0].Previous[1].Zone != "internal" {
		t.Fatal(data)
	}
	activation, _ := newFirewalldMembershipActivation("", service.saved)
	if err := activation.Activate(testContext(t), Runtime{}, true); err != nil {
		t.Fatal(err)
	}
	fixture.assert(t, "running", "internal", "source")
	fixture.assert(t, "stored", "internal", "source", "10.0.0.0/8")
}
func TestFirewalldMembershipAbsentTouchesOnlyNamedZone(t *testing.T) {
	t.Parallel()
	fixture := newMembershipFixture(t)
	fixture.state["running"]["internal"]["interface"] = []string{"eth1"}
	fixture.state["stored"]["trusted"]["interface"] = []string{"eth1"}
	fixture.save(t)
	result := runMembership(t, FirewalldZoneMembership{Zone: "internal", Interfaces: []string{"eth1"}, State: "absent"}, &membershipServiceForTest{}, false)
	if result.Change != "changed" {
		t.Fatal(result)
	}
	fixture.assert(t, "running", "internal", "interface")
	fixture.assert(t, "stored", "trusted", "interface", "eth1")
}
func TestFirewalldMembershipOwnershipRefusalsBeforeMutation(t *testing.T) {
	t.Parallel()
	for _, filename := range []string{"/etc/NetworkManager/system-connections/card.nmconnection", "/run/NetworkManager/system-connections/card.nmconnection", "/etc/sysconfig/network-scripts/ifcfg-eth1"} {
		for _, mode := range []string{"present", "absent", "remove"} {
			for _, dryrun := range []bool{false, true} {
				t.Run(filename+mode+fmtBool(dryrun), func(t *testing.T) {
					fixture := newMembershipFixture(t)
					fixture.nm(t, filename)
					fixture.state["running"]["internal"]["interface"] = []string{"eth1"}
					fixture.state["stored"]["trusted"]["interface"] = []string{"eth1"}
					fixture.save(t)
					input := FirewalldZoneMembership{Zone: "internal", Interfaces: []string{"eth1"}, InterfacesNotListed: "keep"}
					if mode == "absent" {
						input.State = "absent"
						input.InterfacesNotListed = ""
					}
					if mode == "remove" {
						input.Interfaces = []string{}
						input.InterfacesNotListed = "remove"
					}
					service := &membershipServiceForTest{}
					result := runMembership(t, input, service, dryrun)
					if result.Status != "failed" || result.Change != "unchanged" || service.calls != 0 || !strings.Contains(result.Reason, "eth1") || !strings.Contains(result.Reason, "networkmanager_connection_file") {
						t.Fatal(result)
					}
					if strings.HasPrefix(filename, "/etc/") && (!strings.Contains(result.Reason, filename) || !strings.Contains(result.Reason, "zone=")) {
						t.Fatal(result)
					}
					if strings.Contains(filename, "ifcfg-") && !strings.Contains(result.Reason, "nmcli connection migrate --plugin keyfile uuid profile-uuid") {
						t.Fatal(result)
					}
					if strings.HasPrefix(filename, "/run/") && !strings.Contains(result.Reason, "made its active profile itself") {
						t.Fatal(result)
					}
					if strings.Contains(fixture.calls(t), "--change-") || strings.Contains(fixture.calls(t), "--remove-") {
						t.Fatal("changed on refusal")
					}
				})
			}
		}
	}
}
func fmtBool(value bool) string {
	if value {
		return "/dryrun"
	}
	return "/run"
}
func TestFirewalldMembershipMissingZoneUnsupportedSourcesAndOwedRestore(t *testing.T) {
	t.Parallel()
	fixture := newMembershipFixture(t)
	for _, layer := range []string{"running", "stored"} {
		t.Run(layer, func(t *testing.T) {
			zone := fixture.state[layer]["internal"]
			delete(fixture.state[layer], "internal")
			fixture.save(t)
			result := runMembership(t, FirewalldZoneMembership{Zone: "internal", Interfaces: []string{}, InterfacesNotListed: "remove"}, &membershipServiceForTest{}, true)
			if result.Status != "failed" || !strings.Contains(result.Reason, "firewalld_zone") {
				t.Fatal(result)
			}
			fixture.state[layer]["internal"] = zone
		})
	}
	fixture.state["running"]["internal"]["source"] = []string{"ipset:some-set"}
	fixture.save(t)
	result := runMembership(t, FirewalldZoneMembership{Zone: "internal", SourceNetworks: []string{}, SourceNetworksNotListed: "remove"}, &membershipServiceForTest{}, true)
	if result.Status != "failed" || !strings.Contains(result.Reason, "unsupported source") {
		t.Fatal(result)
	}
	result = runMembership(t, FirewalldZoneMembership{Zone: "internal", Interfaces: []string{}, InterfacesNotListed: "keep"}, &membershipServiceForTest{refusal: errors.New("membership restore still owed")}, true)
	if result.Status != "failed" || !strings.Contains(result.Reason, "still owed") {
		t.Fatal(result)
	}
}
func TestFirewalldMembershipFailedAndIgnoredCommands(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"ignore", "fail"} {
		t.Run(failure, func(t *testing.T) {
			fixture := newMembershipFixture(t)
			writeState(t, filepath.Join(fixture.root, failure), "--permanent")
			service := &membershipServiceForTest{}
			result := runMembership(t, FirewalldZoneMembership{Zone: "internal", SourceNetworks: []string{"10.0.0.0/8"}, SourceNetworksNotListed: "keep"}, service, false)
			if result.Status != "failed" || result.Value != nil {
				t.Fatal(result)
			}
			os.Remove(filepath.Join(fixture.root, failure))
			activation, err := newFirewalldMembershipActivation("", service.saved)
			if err != nil {
				t.Fatal(err)
			}
			if err := activation.Activate(testContext(t), Runtime{}, true); err != nil {
				t.Fatal(err)
			}
			fixture.assert(t, "running", "internal", "source")
			fixture.assert(t, "stored", "internal", "source")
		})
	}
}
func TestFirewalldMembershipSavedDataValidation(t *testing.T) {
	t.Parallel()
	for _, data := range []string{`{}`, `{"membership_changes":[{"member":{"member_kind":"rule","member_name":"eth1"}}]}`, `{"membership_changes":[{"member":{"member_kind":"interface","member_name":"eth1"},"previous_bindings":[{"zone":"trusted","member_spelling":"eth2"},{}]}]}`} {
		if _, err := newFirewalldMembershipActivation("", json.RawMessage(data)); err == nil {
			t.Fatal(data)
		}
	}
	if _, err := newFirewalldMembershipActivation("/etc/anything", json.RawMessage(`{}`)); err == nil {
		t.Fatal("destination accepted")
	}
}

func TestFirewalldMembershipMovesCanonicalSourceUsingItsExistingSpelling(t *testing.T) {
	t.Parallel()
	fixture := newMembershipFixture(t)
	fixture.state["running"]["trusted"]["source"] = []string{"192.0.2.1/32"}
	fixture.state["stored"]["trusted"]["source"] = []string{"192.0.2.1"}
	fixture.save(t)
	service := &membershipServiceForTest{}
	result := runMembership(t, FirewalldZoneMembership{Zone: "internal", SourceNetworks: []string{"192.0.2.1"}, SourceNetworksNotListed: "keep"}, service, false)
	if result.Status != "succeeded" {
		t.Fatal(result)
	}
	fixture.assert(t, "running", "internal", "source", "192.0.2.1/32")
	fixture.assert(t, "running", "trusted", "source")
	fixture.assert(t, "stored", "internal", "source", "192.0.2.1")
	if err := firewalldWrite(testContext(t), Runtime{}, "--reload"); err != nil {
		t.Fatal(err)
	}
	activation, err := newFirewalldMembershipActivation("", service.saved)
	if err != nil {
		t.Fatal(err)
	}
	if err = activation.PrepareActivation(testContext(t), Runtime{}, true); err != nil {
		t.Fatal(err)
	}
	if err = activation.Activate(testContext(t), Runtime{}, true); err != nil {
		t.Fatal(err)
	}
	if err = activation.CheckActivatedConfiguration(testContext(t), Runtime{}, true); err != nil {
		t.Fatal(err)
	}
	fixture.assert(t, "running", "trusted", "source", "192.0.2.1/32")
	fixture.assert(t, "stored", "trusted", "source", "192.0.2.1")
	fixture.assert(t, "running", "internal", "source")
	fixture.assert(t, "stored", "internal", "source")
}

func TestFirewalldMembershipRefusesStalePlanAndNewNetworkManagerOwner(t *testing.T) {
	t.Parallel()
	for _, ownership := range []bool{false, true} {
		t.Run(fmtBool(ownership), func(t *testing.T) {
			fixture := newMembershipFixture(t)
			snapshot, err := inspectFirewalldMembership(testContext(t), Runtime{})
			if err != nil {
				t.Fatal(err)
			}
			input := FirewalldZoneMembership{Zone: "internal", Interfaces: []string{"eth1"}, InterfacesNotListed: "keep"}
			data, err := input.plan(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			saved, _ := json.Marshal(data)
			activation, err := newFirewalldMembershipActivation("", saved)
			if err != nil {
				t.Fatal(err)
			}
			if ownership {
				fixture.nm(t, "/etc/NetworkManager/system-connections/eth1.nmconnection")
			} else {
				fixture.state["stored"]["trusted"]["interface"] = []string{"eth1"}
				fixture.save(t)
			}
			if err := activation.PrepareActivation(testContext(t), Runtime{}, false); err == nil {
				t.Fatal("accepted changed ownership or membership")
			}
			if strings.Contains(fixture.calls(t), "--change-") {
				t.Fatal("preparation changed membership")
			}
		})
	}
}
func TestFirewalldMembershipMissingPrerequisite(t *testing.T) {
	t.Parallel()
	service := &membershipServiceForTest{}
	input := FirewalldZoneMembership{Zone: "internal", Interfaces: []string{"eth1"}, InterfacesNotListed: "keep"}
	setWorkerEnvironment(t, "PATH", t.TempDir())
	if result := runMembership(t, input, service, true); result.Status != "not_checked" || !strings.Contains(result.Reason, missingProgramError("firewall-cmd").Error()) {
		t.Fatal(result)
	}
}

func TestFirewalldMembershipNetworkManagerInspection(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, state, device string
		refused             bool
	}{
		{"stopped state", "echo 'not running'", "exit 1", false},
		{"stopped exit status", "exit 8", "exit 1", false},
		{"missing device", "echo running", "exit 10", false},
		{"unmanaged device", "echo running", "echo --", false},
		{"empty active profile", "echo running", "exit 0", false},
		{"failed running inspection", "exit 1", "exit 0", true},
		{"unknown running state", "echo unknown", "exit 0", true},
		{"failed device inspection", "echo running", "exit 1", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newMembershipFixture(t)
			fixture.nm(t, "")
			script := "#!/bin/sh\ncase \"$*\" in\n '-t -f RUNNING general') " + test.state + ";;\n *) " + test.device + ";;\nesac\n"
			writeStandInProgram(t, filepath.Join(fixture.root, "nmcli"), script)
			err := refuseNetworkManagerInterfaces(testContext(t), Runtime{}, []string{"eth1"}, "internal", false)
			if (err != nil) != test.refused {
				t.Fatalf("refused = %v; error = %v", test.refused, err)
			}
		})
	}
}

func TestFirewalldMembershipRejectsIncompleteBindingsBeforeCommands(t *testing.T) {
	t.Parallel()
	fixture := newMembershipFixture(t)
	binding := map[string]any{"zone": "trusted", "member_spelling": "192.0.2.1"}
	malformedLists := []any{
		nil, []any{}, []any{binding}, []any{binding, binding, binding},
		[]any{nil, binding}, []any{binding, nil},
		[]any{map[string]any{}, binding},
		[]any{map[string]any{"zone": "trusted"}, binding},
		[]any{map[string]any{"member_spelling": "192.0.2.1"}, binding},
		[]any{map[string]any{"zone": nil, "member_spelling": "192.0.2.1"}, binding},
		[]any{map[string]any{"zone": "trusted", "member_spelling": nil}, binding},
	}
	for _, field := range []string{"previous_bindings", "proposed_bindings"} {
		for _, malformed := range malformedLists {
			change := map[string]any{"member": map[string]string{"member_kind": "source", "member_name": "192.0.2.1"}, "previous_bindings": []any{binding, binding}, "proposed_bindings": []any{binding, binding}}
			change[field] = malformed
			for _, omitted := range []bool{false, true} {
				if omitted {
					delete(change, field)
				}
				data, _ := json.Marshal(map[string]any{"membership_changes": []any{change}})
				if _, err := newFirewalldMembershipActivation("", data); err == nil {
					t.Fatalf("accepted %s", data)
				}
			}
		}
	}
	if _, err := os.Stat(filepath.Join(fixture.root, "calls")); !os.IsNotExist(err) {
		t.Fatal("validation ran a command", err)
	}
}

func TestFirewalldMembershipWithoutNmcliChecksSystemd(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"inactive", "failed", "active", "activating", "unknown", "error", "missing"} {
		t.Run(state, func(t *testing.T) {
			fixture := newMembershipFixture(t)
			script := "#!/bin/sh\necho " + state + "\n"
			if state == "error" {
				script = "#!/bin/sh\necho inactive\nexit 1\n"
			}
			writeStandInProgram(t, filepath.Join(fixture.root, "systemctl"), script)
			if state == "missing" {
				os.Remove(filepath.Join(fixture.root, "systemctl"))
			}
			for _, dryrun := range []bool{false, true} {
				result := runMembership(t, FirewalldZoneMembership{Zone: "internal", Interfaces: []string{"eth1"}, InterfacesNotListed: "keep"}, &membershipServiceForTest{}, dryrun)
				refused := state != "inactive" && state != "failed"
				if (result.Status == "failed") != refused {
					t.Fatal(result)
				}
				if refused && (!strings.Contains(result.Reason, "nmcli") || strings.Contains(fixture.calls(t), "--change-")) {
					t.Fatal(result, fixture.calls(t))
				}
			}
		})
	}
}

func TestFirewalldMembershipFailedResultsHaveNoOperationLists(t *testing.T) {
	t.Parallel()
	newMembershipFixture(t)
	for _, change := range []string{"unchanged", "unknown", "changed"} {
		result := protocol.Failed(errors.New("The change failed; any applied membership was restored."))
		result.Change = change
		service := &membershipServiceForTest{result: &result}
		got := runMembership(t, FirewalldZoneMembership{Zone: "internal", SourceNetworks: []string{"192.0.2.1"}, SourceNetworksNotListed: "keep"}, service, false)
		if got.Status != "failed" || got.Value != nil {
			t.Fatal(got)
		}
	}
}

func TestFirewalldMembershipRestoreRepairsMissingZoneAndPartialFailure(t *testing.T) {
	t.Parallel()
	fixture := newMembershipFixture(t)
	for _, layer := range []string{"running", "stored"} {
		fixture.state[layer]["trusted"]["source"] = []string{"192.0.2.1", "192.0.2.2"}
	}
	fixture.save(t)
	service := &membershipServiceForTest{}
	result := runMembership(t, FirewalldZoneMembership{Zone: "internal", SourceNetworks: []string{"192.0.2.1", "192.0.2.2"}, SourceNetworksNotListed: "keep"}, service, false)
	if result.Status != "succeeded" {
		t.Fatal(result)
	}
	activation, err := newFirewalldMembershipActivation("", service.saved)
	if err != nil {
		t.Fatal(err)
	}
	fixture.read(t)
	delete(fixture.state["running"], "trusted")
	delete(fixture.state["stored"], "trusted")
	fixture.save(t)
	err = activation.PrepareActivation(testContext(t), Runtime{}, true)
	for _, command := range []string{"firewall-cmd --permanent --new-zone=trusted", "firewall-cmd --reload", "restore service named by mmx status"} {
		if err == nil || !strings.Contains(err.Error(), command) {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"--permanent", "--new-zone=trusted"}, {"--reload"}} {
		if err := firewalldWrite(testContext(t), Runtime{}, args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := activation.PrepareActivation(testContext(t), Runtime{}, true); err != nil {
		t.Fatal(err)
	}
	writeState(t, filepath.Join(fixture.root, "fail"), "--change-source=192.0.2.2")
	if err := activation.Activate(testContext(t), Runtime{}, true); err == nil {
		t.Fatal("restore succeeded despite a command failure")
	}
	for _, layer := range []string{"running", "stored"} {
		fixture.assert(t, layer, "trusted", "source", "192.0.2.1")
		fixture.assert(t, layer, "internal", "source", "192.0.2.2")
	}
	os.Remove(filepath.Join(fixture.root, "fail"))
	if err := activation.Activate(testContext(t), Runtime{}, true); err != nil {
		t.Fatal(err)
	}
	if err := activation.CheckActivatedConfiguration(testContext(t), Runtime{}, true); err != nil {
		t.Fatal(err)
	}
	for _, layer := range []string{"running", "stored"} {
		fixture.assert(t, layer, "trusted", "source", "192.0.2.1", "192.0.2.2")
	}
}

func TestFirewalldMembershipOmittedListsStayAndPoliciesAreIndependent(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name                            string
		manageInterfaces, keepOtherList bool
	}{
		{"remove_interfaces_sources_omitted", true, false},
		{"remove_sources_interfaces_omitted", false, false},
		{"remove_interfaces_keep_sources", true, true},
		{"remove_sources_keep_interfaces", false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newMembershipFixture(t)
			for _, layer := range []string{"running", "stored"} {
				fixture.state[layer]["internal"]["interface"] = []string{"eth9"}
				fixture.state[layer]["internal"]["source"] = []string{"192.0.2.0/24"}
			}
			fixture.save(t)
			input := FirewalldZoneMembership{Zone: "internal"}
			if test.manageInterfaces {
				input.Interfaces, input.InterfacesNotListed = []string{}, "remove"
				if test.keepOtherList {
					input.SourceNetworks, input.SourceNetworksNotListed = []string{}, "keep"
				}
			} else {
				input.SourceNetworks, input.SourceNetworksNotListed = []string{}, "remove"
				if test.keepOtherList {
					input.Interfaces, input.InterfacesNotListed = []string{}, "keep"
				}
			}
			if result := runMembership(t, input, &membershipServiceForTest{}, false); result.Change != "changed" {
				t.Fatal(result)
			}
			for _, layer := range []string{"running", "stored"} {
				if test.manageInterfaces {
					fixture.assert(t, layer, "internal", "interface")
					fixture.assert(t, layer, "internal", "source", "192.0.2.0/24")
				} else {
					fixture.assert(t, layer, "internal", "interface", "eth9")
					fixture.assert(t, layer, "internal", "source")
				}
			}
		})
	}
}
