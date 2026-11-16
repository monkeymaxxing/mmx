package node

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mmx/internal/modules"
	"mmx/internal/protocol"
	"mmx/internal/testfirewalld"
)

func TestMembershipRestoreWithoutFileUsesConfirmationsTimerAndCleanup(t *testing.T) {
	for _, outcome := range []string{"accepted", "expired", "arming failed"} {
		t.Run(outcome, func(t *testing.T) {
			f := newRestoreFixture(t)
			if outcome == "arming failed" {
				f.timerNext = ""
			}
			service := f.server.changeService(f.jobID, "membership", "firewalld_zone_membership", []protocol.ConnectionKind{protocol.ConnectionSSH}, nil, modules.Runtime{})
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			done := make(chan protocol.Result, 1)
			go func() {
				done <- service.ApplyChange(ctx, modules.ChangeWithOwedRestore{ChangeSubject: "firewalld zone membership", RestoreAfterUnit: "firewalld.service", RestoreSuccessSentence: "The previous firewalld zone membership is restored in the running firewall and stored configuration.", RestoreConfirmationWait: time.Minute, ModuleRestoreData: json.RawMessage(`{"saved_membership":true}`)})
			}()
			record := waitForRestorePhase(t, f.server, waitingBeforeChange)
			if record.Outcome.DestinationFile != "" || record.PreviousFile != nil || f.adapter.calls.Load() != 0 {
				t.Fatal(record)
			}
			if err := f.server.CheckChangeAllowed(ctx, ""); err == nil || strings.Contains(err.Error(), "file") {
				t.Fatal(err)
			}
			second := service.ApplyChange(ctx, modules.ChangeWithOwedRestore{ChangeSubject: "firewalld zone membership", RestoreAfterUnit: "firewalld.service", RestoreSuccessSentence: "The previous firewalld zone membership is restored in the running firewall and stored configuration.", RestoreConfirmationWait: time.Minute, ModuleRestoreData: json.RawMessage(`{}`)})
			if second.Status != "failed" || !strings.Contains(second.Reason, "still owes a restore") {
				t.Fatal("a second data-only change was admitted", second)
			}
			if err := f.server.CheckChangeAllowed(ctx, f.destination); err != nil {
				t.Fatal(err)
			}
			confirm(t, f.server, record, protocol.ConfirmationBeforeChange, protocol.ConnectionSSH)
			if outcome != "arming failed" {
				record = waitForRestorePhase(t, f.server, activatingConfig)
				units, err := f.server.changeUnits(record)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(units[restoreServiceUnit(record.Outcome.OwedRestoreID)], "After=network.target firewalld.service") {
					t.Fatal(units)
				}
				for _, unit := range units {
					if strings.Contains(unit, "Wants=") || strings.Contains(unit, "Requires=") {
						t.Fatal(unit)
					}
				}
				if _, err := os.Stat(filepath.Join(f.server.restoreDirectory(record.Outcome.OwedRestoreID), "previous_file_contents")); !os.IsNotExist(err) {
					t.Fatal("saved a file", err)
				}
				if err := f.server.ActivateChange(ctx, record.Outcome.OwedRestoreID); err != nil {
					t.Fatal(err)
				}
				if outcome == "accepted" {
					confirm(t, f.server, record, protocol.ConfirmationAfterChange, protocol.ConnectionSSH)
				} else {
					f.clock.advance(2 * time.Minute)
					if err := f.server.RunOwedRestore(ctx, record.Outcome.OwedRestoreID); err != nil {
						t.Fatal(err)
					}
				}
			}
			result := <-done
			if strings.Contains(result.Reason, "file") {
				t.Fatal(result)
			}
			if (result.Status == "succeeded") != (outcome == "accepted") {
				t.Fatal(result)
			}
			if outcome == "expired" && (result.Change != "changed" || !strings.Contains(result.Reason, "previous firewalld zone membership is restored")) {
				t.Fatal(result)
			}
			final := readRestore(t, f.server, record.Outcome.OwedRestoreID)
			if !final.CleanupComplete || final.PreviousFile != nil || final.Outcome.PreviousFileRestored {
				t.Fatal(final)
			}
			if err := f.server.CheckChangeAllowed(ctx, ""); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestMembershipRestoreFailureStaysOwedAndAdministratorCanRepair(t *testing.T) {
	f := newRestoreFixture(t)
	record := f.record(t, waitingAfterChange, protocol.ConnectionSSH)
	record.Outcome.ModuleName = "firewalld_zone_membership"
	record.Outcome.DestinationFile = ""
	record.Outcome.ChangeSubject = "firewalld zone membership"
	record.RestoreAfterUnit = "firewalld.service"
	record.RestoreSuccessSentence = "The previous firewalld zone membership is restored in the running firewall and stored configuration."
	record.PreviousFile = nil
	if err := f.server.withOwedRestores(context.Background(), func() error { return f.server.writeOwedRestore(&record) }); err != nil {
		t.Fatal(err)
	}
	f.stopWorker()
	f.adapter.activate = func(bool) error { return fmt.Errorf("Firewalld is not running.") }
	if err := f.server.RunOwedRestore(context.Background(), record.Outcome.OwedRestoreID); err == nil {
		t.Fatal("restore did not fail")
	}
	failed := readRestore(t, f.server, record.Outcome.OwedRestoreID)
	if failed.Phase != restoreFailed || failed.CleanupComplete || failed.Outcome.PreviousMembershipRestored {
		t.Fatal(failed)
	}
	if err := f.server.CheckChangeAllowed(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "Firewalld is not running") {
		t.Fatal(err)
	}
	f.adapter.activate = nil
	if err := f.server.RunOwedRestore(context.Background(), record.Outcome.OwedRestoreID); err != nil {
		t.Fatal(err)
	}
	restored := readRestore(t, f.server, record.Outcome.OwedRestoreID)
	if !restored.CleanupComplete || !restored.Outcome.PreviousMembershipRestored || restored.Outcome.PreviousFileRestored || restored.Outcome.ObsoleteVirtualDevicesRemoved {
		t.Fatal(restored)
	}
}

func TestMembershipRestoreOnNewBootBeforeDeadline(t *testing.T) {
	f := newRestoreFixture(t)
	record := f.record(t, waitingAfterChange, protocol.ConnectionSSH)
	record.Outcome.ModuleName = "firewalld_zone_membership"
	record.Outcome.DestinationFile = ""
	record.Outcome.ChangeSubject = "firewalld zone membership"
	record.RestoreAfterUnit = "firewalld.service"
	record.RestoreSuccessSentence = "The previous firewalld zone membership is restored in the running firewall and stored configuration."
	record.PreviousFile = nil
	if err := f.server.withOwedRestores(context.Background(), func() error { return f.server.writeOwedRestore(&record) }); err != nil {
		t.Fatal(err)
	}
	f.server.machine.BootID = func() (string, error) { return "next-boot", nil }
	if err := f.server.RunOwedRestore(context.Background(), record.Outcome.OwedRestoreID); err != nil {
		t.Fatal(err)
	}
	restored := readRestore(t, f.server, record.Outcome.OwedRestoreID)
	if restored.Phase != configRestored || !f.clock.read().Before(record.RestoreDeadline) || f.adapter.calls.Load() != 1 {
		t.Fatal(restored)
	}
}

// Exercise the registered production adapter through the real node service.
// Only systemd delivery and the firewall executable are stand-ins.
func TestMembershipProductionAdapterFailureAndBootRecovery(t *testing.T) {
	for _, scenario := range []string{"command failure", "new boot", "firewall stopped at boot"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			write := func(name, data string, mode os.FileMode) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(root, name), []byte(data), mode); err != nil {
					t.Fatal(err)
				}
			}
			testfirewalld.Install(t, root)
			before := `{"running":{"trusted":{"interface":[],"source":["192.0.2.1/32"]},"internal":{"interface":[],"source":[]}},"stored":{"trusted":{"interface":[],"source":["192.0.2.1"]},"internal":{"interface":[],"source":[]}}}`
			write("state", before, 0600)
			t.Setenv("PATH", root)
			f := newRestoreFixture(t)
			f.server.activationFactory = nil
			var restarted atomic.Bool
			f.server.machine.BootID = func() (string, error) {
				if restarted.Load() {
					return "new-boot", nil
				}
				return "original-boot", nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			requestedRestore := make(chan string, 4)
			f.server.systemctl = func(ctx context.Context, args ...string) (string, error) {
				output, err := f.systemctl(ctx, args...)
				if err == nil && len(args) == 3 && args[0] == "start" && args[1] == "--no-block" && strings.HasPrefix(args[2], "mmx-owed-restore-") {
					requestedRestore <- strings.TrimSuffix(strings.TrimPrefix(args[2], "mmx-owed-restore-"), ".service")
				}
				return output, err
			}
			restored := make(chan error, 1)
			if scenario == "command failure" {
				write("fail", "--permanent", 0600)
				go func() {
					select {
					case id := <-requestedRestore:
						restored <- f.server.RunOwedRestore(ctx, id)
					case <-ctx.Done():
						restored <- ctx.Err()
					}
				}()
			}
			done := make(chan protocol.Result, 1)
			go func() {
				input := modules.FirewalldZoneMembership{Zone: "internal", SourceNetworks: []string{"192.0.2.1"}, SourceNetworksNotListed: "keep", RestoreUnlessControllerConfirmsWithin: "1m"}
				done <- input.Run(ctx, modules.NodeTask{OwedRestores: f.server.changeService(f.jobID, "membership", "firewalld_zone_membership", []protocol.ConnectionKind{protocol.ConnectionSSH}, nil, modules.Runtime{})})
			}()
			record := waitForRestorePhase(t, f.server, waitingBeforeChange)
			confirm(t, f.server, record, protocol.ConfirmationBeforeChange, protocol.ConnectionSSH)
			record = waitForRestorePhase(t, f.server, activatingConfig)
			if err := f.server.ActivateChange(ctx, record.Outcome.OwedRestoreID); err != nil {
				t.Fatal(err)
			}
			if scenario == "command failure" {
				if err := <-restored; err != nil {
					t.Fatal(err)
				}
			} else {
				// Reload reproduces boot's copy of stored spelling into runtime.
				output := (modules.Runtime{}).RunCommandThatChangesTheNode(ctx, []string{"firewall-cmd", "--reload"}, "")
				if output.ExitCode != 0 || output.Error != "" {
					t.Fatal(output)
				}
				restarted.Store(true)
				if scenario == "firewall stopped at boot" {
					write("stopped", "", 0600)
					if err := f.server.RunOwedRestore(ctx, record.Outcome.OwedRestoreID); err == nil {
						t.Fatal("stopped firewall was accepted")
					}
					failed := readRestore(t, f.server, record.Outcome.OwedRestoreID)
					if failed.Phase != restoreFailed || failed.CleanupComplete || !strings.Contains(failed.Outcome.RestoreFailureReason, "systemctl start firewalld.service") || !strings.Contains(failed.Outcome.RestoreFailureReason, "restore service named by mmx status") {
						t.Fatal(failed)
					}
					// The administrator starts firewalld, then its owed restore.
					if err := os.Remove(filepath.Join(root, "stopped")); err != nil {
						t.Fatal(err)
					}
				}
				if err := f.server.RunOwedRestore(ctx, record.Outcome.OwedRestoreID); err != nil {
					t.Fatal(err)
				}
			}
			result := <-done
			if result.Status != "failed" || result.Value != nil {
				t.Fatal(result)
			}
			if scenario != "firewall stopped at boot" && !strings.Contains(result.Reason, "previous firewalld zone membership is restored") {
				t.Fatal(result)
			}
			final := readRestore(t, f.server, record.Outcome.OwedRestoreID)
			if final.Phase != configRestored || !final.CleanupComplete || !final.Outcome.PreviousMembershipRestored || !f.clock.read().Before(final.RestoreDeadline) {
				t.Fatal(final)
			}
			data, err := os.ReadFile(filepath.Join(root, "state"))
			if err != nil {
				t.Fatal(err)
			}
			var want, actual any
			json.Unmarshal([]byte(before), &want)
			json.Unmarshal(data, &actual)
			wantJSON, _ := json.Marshal(want)
			actualJSON, _ := json.Marshal(actual)
			if string(wantJSON) != string(actualJSON) {
				t.Fatalf("restored %s, want %s", actualJSON, wantJSON)
			}
		})
	}
}

func TestModuleDataRestoreUsesRecordedDescriptions(t *testing.T) {
	f := newRestoreFixture(t)
	record := f.record(t, waitingAfterChange, protocol.ConnectionSSH)
	record.Outcome.ModuleName = "example_configuration"
	record.Outcome.DestinationFile = ""
	record.Outcome.ChangeSubject = "example configuration"
	record.PreviousFile = nil
	record.RestoreAfterUnit = "example.service"
	record.RestoreSuccessSentence = "The previous example configuration is restored."
	if err := f.server.writeOwedRestore(&record); err != nil {
		t.Fatal(err)
	}
	// Reconstruct from disk, as a fresh service process does. No module-name
	// branch may select a subject, ordering dependency, or success sentence.
	record = readRestore(t, f.server, record.Outcome.OwedRestoreID)
	units, err := f.server.changeUnits(record)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{restoreServiceUnit(record.Outcome.OwedRestoreID), activationServiceUnit(record.Outcome.OwedRestoreID)} {
		if !strings.Contains(units[name], "After=network.target example.service\n") || strings.Contains(units[name], "firewalld") {
			t.Fatal(units[name])
		}
	}
	if err := f.server.CheckChangeAllowed(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "Cannot change example configuration:") {
		t.Fatal(err)
	}
	f.stopWorker()
	if err := f.server.RunOwedRestore(context.Background(), record.Outcome.OwedRestoreID); err != nil {
		t.Fatal(err)
	}
	restored := readRestore(t, f.server, record.Outcome.OwedRestoreID)
	result := changeResult(restored, protocol.Success("unknown"), nil)
	if !strings.HasSuffix(result.Reason, record.RestoreSuccessSentence) || !restored.Outcome.PreviousMembershipRestored {
		t.Fatal(result, restored)
	}
	data, err := os.ReadFile(filepath.Join(f.server.restoreDirectory(record.Outcome.OwedRestoreID), "owed_restore.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"destination_file", "previous_file_metadata", "previous_file_restored", "previous_config_activated", "obsolete_virtual_devices_removed"} {
		if strings.Contains(string(data), `"`+name+`"`) {
			t.Fatal("inapplicable field", name, string(data))
		}
	}
}

func TestModuleDataRestoreRejectsMissingDescriptionsAndUnsafeOrdering(t *testing.T) {
	f := newRestoreFixture(t)
	record := f.record(t, waitingAfterChange, protocol.ConnectionSSH)
	record.Outcome.DestinationFile = ""
	record.Outcome.ChangeSubject = "example configuration"
	record.PreviousFile = nil
	record.RestoreAfterUnit = "example.service"
	record.RestoreSuccessSentence = "The previous configuration is restored."
	for name, mutate := range map[string]func(*owedRestoreRecord){
		"missing subject":          func(r *owedRestoreRecord) { r.Outcome.ChangeSubject = "" },
		"missing success sentence": func(r *owedRestoreRecord) { r.RestoreSuccessSentence = "" },
		"missing unit":             func(r *owedRestoreRecord) { r.RestoreAfterUnit = "" },
		"unit directive injection": func(r *owedRestoreRecord) { r.RestoreAfterUnit = "example.service\nRequires=other.service" },
		"multiple units":           func(r *owedRestoreRecord) { r.RestoreAfterUnit = "one.service two.service" },
		"unit expansion":           func(r *owedRestoreRecord) { r.RestoreAfterUnit = "example-%n.service" },
		"subject line break":       func(r *owedRestoreRecord) { r.Outcome.ChangeSubject = "example\nconfiguration" },
		"success line break":       func(r *owedRestoreRecord) { r.RestoreSuccessSentence = "Restored.\nOther text." },
		"file metadata":            func(r *owedRestoreRecord) { r.PreviousFile = &modules.PreviousFileMetadata{} },
		"file outcome":             func(r *owedRestoreRecord) { r.Outcome.PreviousFileRestored = true },
		"mixed descriptions":       func(r *owedRestoreRecord) { r.Outcome.DestinationFile = f.destination },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := record
			mutate(&invalid)
			if err := invalid.validate(); err == nil {
				t.Fatal("accepted", invalid)
			}
		})
	}
}
