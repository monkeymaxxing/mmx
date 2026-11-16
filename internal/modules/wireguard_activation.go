package modules

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"mmx/internal/protocol"
)

// Only a compiled action and the previous service switches survive into
// another process. File contents, including keys, use root-private saved files.
type wireguardRestoreData struct {
	Action                 string   `json:"service_action"`
	Enable                 bool     `json:"enable_service"`
	Reasons                []string `json:"service_action_reasons,omitempty"`
	PreviousServiceActive  bool     `json:"previous_service_active"`
	PreviousServiceEnabled bool     `json:"previous_service_enabled"`
}

type wireguardActivation struct {
	destination, name          string
	data                       wireguardRestoreData
	previousFile, proposedFile string
	target                     wireguardFile
	checkFullConfiguration     bool
	plan                       wireguardPlan
}

func newWireguardActivation(destination string, saved json.RawMessage) (ChangeActivation, error) {
	name := strings.TrimSuffix(filepath.Base(destination), ".conf")
	if destination != wireguardOnANode.file(name) || !wireguardInterfaceName.MatchString(name) {
		return nil, fmt.Errorf("Cannot activate or restore %s: the saved destination must name a WireGuard .conf file in /etc/wireguard/.", destination)
	}
	a := &wireguardActivation{destination: destination, name: name}
	if err := protocol.Decode(saved, &a.data); err != nil {
		return nil, err
	}
	if !slices.Contains([]string{"", "start", "reload", "restart", "stop"}, a.data.Action) || (a.data.Enable && (a.data.Action == "start" || a.data.Action == "stop")) {
		return nil, fmt.Errorf("The saved WireGuard service action is invalid.")
	}
	return a, nil
}

func (a *wireguardActivation) UseSavedFiles(previousFileContents, proposedFileContents string) {
	a.previousFile, a.proposedFile = previousFileContents, proposedFileContents
}

// A previous file may come from a first-boot script. Parsing determines
// whether MMX can compare the configuration, never whether native restore
// may use a file that wg-quick accepts but the module itself would refuse.
func readSavedWireguardFile(path string) (file wireguardFile, comparable bool, err error) {
	if path == "" {
		return file, false, nil
	}
	text, err := os.ReadFile(path)
	if err != nil {
		return file, false, err
	}
	file, parseError := parseWireguardFile(string(text), true)
	comparable = parseError == nil && wireguardKey(file.privateKey) && !slices.ContainsFunc(file.quickSettings, func(setting [2]string) bool {
		return setting[0] == "saveconfig" && strings.EqualFold(setting[1], "true")
	})
	return file, comparable, nil
}

func (a *wireguardActivation) PrepareActivation(ctx context.Context, run Runtime, restoring bool) (err error) {
	defer func() { err = a.withoutKeys(err) }()
	target, source := a.proposedFile, a.previousFile
	if restoring {
		target, source = source, target
	}
	a.plan = wireguardPlan{verb: a.data.Action, enable: a.data.Enable, why: a.data.Reasons}
	if target == "" {
		if !restoring && a.data.Action != "stop" {
			return fmt.Errorf("The saved proposed WireGuard configuration file is missing.")
		}
		a.plan = wireguardPlan{verb: "stop"}
		return nil
	}
	if !restoring && a.data.Action == "stop" {
		return fmt.Errorf("A WireGuard removal cannot have a saved proposed configuration file.")
	}
	a.target, a.checkFullConfiguration, err = readSavedWireguardFile(target)
	if err != nil {
		return err
	}
	if !restoring {
		if !a.checkFullConfiguration {
			return fmt.Errorf("The saved WireGuard configuration file is invalid.")
		}
		return nil
	}
	if !a.data.PreviousServiceActive {
		a.plan = wireguardPlan{verb: "stop"}
		return nil
	}
	if !a.checkFullConfiguration {
		a.plan = wireguardPlan{verb: "restart"}
		return nil
	}
	previous, comparable, err := readSavedWireguardFile(source)
	if err != nil {
		return err
	}
	if source != "" && !comparable {
		a.plan = wireguardPlan{verb: "restart"}
		return nil
	}
	quickChanges := wireguardQuickChanges(previous, a.target)
	if source == "" {
		quickChanges = []string{"the proposed WireGuard configuration file is absent"}
	}
	service, err := (UnitManager{}).Inspect(ctx, wireguardUnit(a.name))
	if err != nil {
		return err
	}
	running, err := run.readWireguardInterface(ctx, a.name, a.target)
	if err != nil {
		return err
	}
	// The forward and restore directions share the complete decision table:
	// wg-quick-only settings, addresses, MTU, and routes require a restart.
	a.plan = planWireguard(true, quickChanges, wireguardDifferences(a.name, a.target, a.target.privateKey, running), service)
	a.plan.enable = false // The restore sets the recorded enabled state after activation.
	return nil
}

// Native diagnostics may quote either file's key. New processes cannot rely
// on the node task's scope trace to remove keys from the durable outcome.
func (a *wireguardActivation) withoutKeys(err error) error {
	if err == nil {
		return nil
	}
	text := err.Error()
	for _, path := range []string{a.previousFile, a.proposedFile} {
		contents, readError := os.ReadFile(path)
		if readError != nil {
			continue
		}
		// A native previous file may contain settings MMX does not parse.
		// Read key lines independently so diagnostics still cannot expose keys.
		for _, line := range strings.Split(string(contents), "\n") {
			name, key, found := strings.Cut(strings.TrimLeft(line, " \t#"), "=")
			key, _, _ = strings.Cut(key, "#")
			key = strings.TrimSpace(key)
			if !found || !strings.EqualFold(strings.TrimSpace(name), "PrivateKey") || !wireguardKey(key) {
				continue
			}
			for _, form := range []string{key, clampedWireguardKey(key)} {
				text = strings.ReplaceAll(text, form, "[redacted]")
			}
		}
	}

	return fmt.Errorf("Cannot activate or restore WireGuard interface %s: %s.", a.name, strings.TrimSuffix(text, "."))
}

func (a *wireguardActivation) Activate(ctx context.Context, run Runtime, restoring bool) (err error) {
	defer func() { err = a.withoutKeys(err) }()
	unit, manager := wireguardUnit(a.name), UnitManager{}
	if a.plan.verb == "stop" {
		source := a.previousFile
		if restoring {
			source = a.proposedFile
		}
		service, err := manager.Inspect(ctx, unit)
		if err != nil {
			return err
		}
		if err := stopWireguardService(ctx, run, a.name, service); err != nil {
			return err
		}
		// The packaged ExecStop reads the managed file. Without it, ExecStop fails
		// and leaves the interface up, while systemctl still reports the stop as
		// done, so a correctly named private copy finishes native cleanup.
		if _, missing := os.Lstat(a.destination); service.Active == "active" && os.IsNotExist(missing) && source != "" {
			if err := a.finishStopWithSavedFile(ctx, run, source); err != nil {
				return err
			}
		}
	} else {
		result := run.actOnWireguardService(ctx, a.name, a.plan, &wireguardSteps{change: "unchanged"})
		if result.Status != "succeeded" {
			return fmt.Errorf("%s", result.Reason)
		}
	}
	if restoring && a.previousFile != "" {
		verb := "disable"
		if a.data.PreviousServiceEnabled {
			verb = "enable"
		}
		if err := run.Mutation(unit); err != nil {
			return err
		}
		if result := manager.systemctl(ctx, verb, "--", unit); result.Status != "succeeded" {
			return fmt.Errorf("Cannot restore the enabled state of %s: %s.", unit, strings.TrimSuffix(result.Reason, "."))
		}
	}
	return nil
}

func (a *wireguardActivation) finishStopWithSavedFile(ctx context.Context, run Runtime, source string) error {
	_, exists, err := run.wireguardLink(ctx, a.name)
	if err != nil {
		return err
	}
	if exists {
		contents, err := os.ReadFile(source)
		if err != nil {
			return err
		}
		// wg-quick derives the interface name from the .conf filename. The
		// copy goes into a directory of its own in the WireGuard directory,
		// not a private task directory in /run/mmx: Ubuntu 26.04's AppArmor
		// profile for wg-quick lets it read files only in /etc/wireguard.
		// os.MkdirTemp makes the directory root's own with mode 0700.
		directory, err := os.MkdirTemp(filepath.Dir(a.destination), "mmx-stop-"+a.name+"-")
		if err != nil {
			return err
		}
		defer func() { _ = os.RemoveAll(directory) }()
		private := run
		private.RetainDiff = nil
		if result := private.WriteFile(ctx, filepath.Join(directory, a.name+".conf"), string(contents), "", "", "0600"); result.Error != "" {
			return fmt.Errorf("Cannot prepare the saved WireGuard configuration for removal: %s.", result.Error)
		}
		output := run.RunCommandThatChangesTheNode(ctx, []string{"wg-quick", "down", filepath.Join(directory, a.name+".conf")}, "")
		if err := commandFailure(output, "Taking down WireGuard interface "+a.name+" with the saved configuration"); err != nil {
			return err
		}
	}
	if err := checkWireguardAbsent(ctx, run, a.name); err != nil {
		return err
	}
	if err := run.Mutation(wireguardUnit(a.name)); err != nil {
		return err
	}
	result := (UnitManager{}).systemctl(ctx, "reset-failed", "--", wireguardUnit(a.name))
	if result.Status != "succeeded" {
		return fmt.Errorf("Cannot clear the missing-file stop failure of %s: %s.", wireguardUnit(a.name), result.Reason)
	}
	return nil
}

func stopWireguardService(ctx context.Context, run Runtime, name string, service ServiceState) error {
	if service.Load == "not-found" || (service.Active == "inactive" && service.Enabled != "enabled") {
		return nil
	}
	unit := wireguardUnit(name)
	if err := run.Mutation(unit); err != nil {
		return err
	}
	if result := (UnitManager{}).systemctl(ctx, "disable", "--now", "--", unit); result.Status != "succeeded" {
		return fmt.Errorf("Cannot stop and disable %s: %s: %s.", unit, result.Reason, strings.TrimSpace(result.Stderr))
	}
	return nil
}

func checkWireguardAbsent(ctx context.Context, run Runtime, name string) error {
	_, exists, err := run.wireguardLink(ctx, name)
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("The interface %s still exists; %s did not bring the interface up, so take the interface down by hand with ip link delete %s.", name, wireguardUnit(name), name)
	}
	return nil
}

func (*wireguardActivation) RemoveObsoleteVirtualDevices(context.Context, Runtime, bool) error {
	return nil
}

func (a *wireguardActivation) CheckActivatedConfiguration(ctx context.Context, run Runtime, restoring bool) (err error) {
	defer func() { err = a.withoutKeys(err) }()
	if !restoring && a.plan.verb != "stop" {
		result := run.checkWireguardInterface(ctx, a.name, a.destination, a.target, a.target.privateKey, &wireguardSteps{change: "changed"}, protocol.Success("changed"))
		if result.Status != "succeeded" {
			return fmt.Errorf("%s", result.Reason)
		}
		return nil
	}
	service, err := (UnitManager{}).Inspect(ctx, wireguardUnit(a.name))
	if err != nil {
		return err
	}
	active, enabled := false, false
	if restoring && a.previousFile != "" {
		active, enabled = a.data.PreviousServiceActive, a.data.PreviousServiceEnabled
	}
	if (service.Active == "active") != active || (service.Enabled == "enabled") != enabled {
		return fmt.Errorf("The active or enabled state of %s differs from the required WireGuard service state.", wireguardUnit(a.name))
	}
	if !active {
		return checkWireguardAbsent(ctx, run, a.name)
	}
	if !a.checkFullConfiguration {
		_, exists, err := run.wireguardLink(ctx, a.name)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("The restored WireGuard interface %s does not exist.", a.name)
		}
		return nil
	}
	running, err := run.readWireguardInterface(ctx, a.name, a.target)
	if err != nil {
		return err
	}
	if differences := wireguardDifferences(a.name, a.target, a.target.privateKey, running); len(differences) > 0 {
		var remains []string
		for _, difference := range differences {
			remains = append(remains, difference.remains)
		}
		return fmt.Errorf("WireGuard interface %s still differs from %s: %s.", a.name, a.destination, wireguardList(remains, "; "))
	}
	return nil
}
