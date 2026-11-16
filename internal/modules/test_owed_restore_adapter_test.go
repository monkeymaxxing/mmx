//go:build mmx_owed_restore_test

package modules

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mmx/internal/protocol"
)

func TestOwedRestoreTestAdapterReconstructedFromRecord(t *testing.T) {
	t.Parallel()
	module, ok := LookUpCoreModule(testOwedRestoreModule)
	if !ok || module.NewChangeActivation == nil {
		t.Fatal("tagged module cannot be reconstructed in the restore process")
	}
	adapter, err := module.NewChangeActivation("/var/tmp/mmx-integration/owed-restores/example/configuration", json.RawMessage(`{"activation":"stop_ssh_listener","ssh_listener_units":["ssh.socket","ssh.service"]}`))
	if err != nil {
		t.Fatal(err)
	}
	saved := adapter.(*testChangeActivation)
	if saved.Activation != "stop_ssh_listener" || len(saved.SSHUnits) != 2 || saved.marker("release_change_activation") != "/var/tmp/mmx-integration/owed-restores/example/release_change_activation" {
		t.Fatal(saved)
	}
	for _, raw := range []string{`{"activation":"execute_command"}`, `{"activation":"succeed","command":"false"}`, `{"activation":"stop_ssh_listener","ssh_listener_units":["unrelated.service"]}`} {
		if _, err := module.NewChangeActivation(saved.destination, json.RawMessage(raw)); err == nil {
			t.Fatal("accepted unsupported restore instructions", raw)
		}
	}
	if _, err := module.NewChangeActivation("/etc/ssh/sshd_config", json.RawMessage(`{"activation":"succeed"}`)); err == nil {
		t.Fatal("accepted a non-test destination")
	}
}

type rejectTestOwedRestoreApply struct{ t *testing.T }

func (s rejectTestOwedRestoreApply) CheckChangeAllowed(context.Context, string) error { return nil }
func (s rejectTestOwedRestoreApply) ApplyChange(context.Context, ChangeWithOwedRestore) protocol.Result {
	s.t.Fatal("dryrun created an owed restore")
	return protocol.Result{}
}

func TestOwedRestoreTestAdapterDryrunDoesNotActivateOrArm(t *testing.T) {
	t.Parallel()
	in := testOwedRestoreInputs{Path: "/var/tmp/mmx-integration/owed-restores/dryrun-unit-test/configuration", Content: "proposed", Activation: "stop_ssh_listener", ConfirmReachabilityWithin: "8s"}
	result := in.Run(context.Background(), NodeTask{Runtime: Runtime{Dryrun: true}, OwedRestores: rejectTestOwedRestoreApply{t}})
	if result.Status != "succeeded" || result.Change != "would_change" {
		t.Fatal(result)
	}
}
func TestOwedRestoreTestAdapterRejectsUnboundedWait(t *testing.T) {
	t.Parallel()
	in := testOwedRestoreInputs{Path: "/var/tmp/mmx-integration/owed-restores/example/configuration", Activation: "succeed"}
	for _, duration := range []string{"", "0s", "-1s", "121s"} {
		in.ConfirmReachabilityWithin = duration
		if err := in.Prepare(""); err == nil || !strings.Contains(err.Error(), "confirm_reachability_within") {
			t.Fatal(duration, err)
		}
	}
	in.ConfirmReachabilityWithin = "2m"
	if err := in.Prepare(""); err != nil {
		t.Fatal("reboot case requires a two-minute allowance", err)
	}
}

func TestOwedRestoreTestAdapterRefusesLiveActivationBeforeFileRestore(t *testing.T) {
	t.Parallel()
	for _, live := range []string{"parent", "child"} {
		t.Run(live, func(t *testing.T) {
			markers, processes := t.TempDir(), t.TempDir()
			for index, role := range []string{"parent", "child"} {
				pid := []string{"123", "456"}[index]
				if err := os.WriteFile(filepath.Join(markers, "change_activation_"+role+"_process_id"), []byte(pid), 0600); err != nil {
					t.Fatal(err)
				}
				if role == live {
					if err := os.Mkdir(filepath.Join(processes, pid), 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(processes, pid, "stat"), []byte(pid+" (test child) S 1 2 3"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := checkTestActivationProcessesStopped(markers, processes); err == nil || !strings.Contains(err.Error(), "still runs before file restoration") {
				t.Fatal("accepted a live activation "+live, err)
			}
		})
	}
}

func TestOwedRestoreTestAdapterAllowsExitedActivationBeforeFileRestore(t *testing.T) {
	t.Parallel()
	markers, processes := t.TempDir(), t.TempDir()
	for _, role := range []string{"parent", "child"} {
		if err := os.WriteFile(filepath.Join(markers, "change_activation_"+role+"_process_id"), []byte("123\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := checkTestActivationProcessesStopped(markers, processes); err != nil {
		t.Fatal("exited processes prevented restoration", err)
	}
	if err := os.Mkdir(filepath.Join(processes, "123"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(processes, "123", "stat"), []byte("123 (test ) child) Z 1 2 3"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := checkTestActivationProcessesStopped(markers, processes); err != nil {
		t.Fatal("zombie processes prevented restoration", err)
	}
}
