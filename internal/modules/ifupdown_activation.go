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

// ifupdownActivation takes the changed interfaces down with the file that
// brought them up and brings them up with the file now in place. Its
// commands name only interfaces from the record; ifdown and ifup read the
// saved files themselves.
type ifupdownActivation struct {
	destination string
	data        ifupdownRestoreData
	// The saved copies of the previous and the proposed file, "" for a file
	// that did not exist.
	previousFile, proposedFile string
	// takenDown lists the interfaces that this process took down, and
	// vlansTakenDown the VLANs on them that it took down and brings up again.
	takenDown, vlansTakenDown []string
}

func newIfupdownActivation(destination string, saved json.RawMessage) (ChangeActivation, error) {
	if ifupdownPathRule(destination, "") != nil {
		return nil, fmt.Errorf("Cannot activate or restore %s: the saved destination must be %s or a file directly in %s.", destination, ifupdownMainFile, ifupdownDirectory)
	}
	activation := &ifupdownActivation{destination: destination}
	if err := protocol.Decode(saved, &activation.data); err != nil {
		return nil, err
	}
	for _, names := range [][]string{activation.data.TakeDownWithPreviousFile, activation.data.TakeDownWithProposedFileOnRestore, interfacesToBringUp(activation.data.BringUpWithProposedFile), interfacesToBringUp(activation.data.BringUpWithPreviousFileOnRestore)} {
		for i, name := range names {
			if !ifupdownInterfaceName(name) || slices.Contains(names[:i], name) {
				return nil, fmt.Errorf("The saved ifupdown interface name %q is invalid or repeated.", name)
			}
		}
	}
	for _, up := range slices.Concat(activation.data.BringUpWithProposedFile, activation.data.BringUpWithPreviousFileOnRestore) {
		if up.ListedIn != "auto" && up.ListedIn != "allow-hotplug" {
			return nil, fmt.Errorf("The saved ifupdown class %q of interface %s is neither auto nor allow-hotplug.", up.ListedIn, up.Interface)
		}
	}
	return activation, nil
}
func interfacesToBringUp(list []ifupdownInterfaceToBringUp) []string {
	var names []string
	for _, up := range list {
		names = append(names, up.Interface)
	}
	return names
}

func (a *ifupdownActivation) UseSavedFiles(previousFileContents, proposedFileContents string) {
	a.previousFile, a.proposedFile = previousFileContents, proposedFileContents
}

// direction gives the interfaces to take down, the saved file that brought
// them up, and the interfaces to bring up with the file in place.
func (a *ifupdownActivation) direction(restoring bool) ([]string, string, []ifupdownInterfaceToBringUp) {
	if restoring {
		return a.data.TakeDownWithProposedFileOnRestore, a.proposedFile, a.data.BringUpWithPreviousFileOnRestore
	}
	return a.data.TakeDownWithPreviousFile, a.previousFile, a.data.BringUpWithProposedFile
}

// The restore service runs after network.target and networking.service
// before it, so after a restart the restore follows boot's ifup -a.
func (a *ifupdownActivation) PrepareActivation(context.Context, Runtime, bool) error { return nil }

func (a *ifupdownActivation) Activate(ctx context.Context, run Runtime, restoring bool) (err error) {
	defer func() {
		if err != nil && !restoring {
			err = fmt.Errorf("ifupdown did not accept %s: %s.", a.destination, strings.TrimSuffix(err.Error(), "."))
		}
	}()
	a.takenDown, a.vlansTakenDown = nil, nil
	takeDown, savedFile, bringUp := a.direction(restoring)
	if len(takeDown) > 0 && savedFile == "" {
		return fmt.Errorf("the saved copy of the file that brought up %s is missing", strings.Join(takeDown, ", "))
	}
	for _, name := range takeDown {
		up, err := ifupdownListsAsUp(ctx, run, name)
		if err != nil {
			return err
		}
		arguments := []string{"ifdown", "-i", savedFile, name}
		if restoring {
			// A failed ifup leaves its address but does not list the
			// interface as up; ifdown --force removes the address.
			exists, err := pathExists(filepath.Join(ifupdownLinksDirectory, name))
			if err != nil {
				return err
			}
			up = up || exists
			arguments = []string{"ifdown", "--force", "-i", savedFile, name}
		}
		if !up {
			continue
		}
		vlans, err := vlansListedAsUpOn(ctx, run, name, takeDown)
		if err != nil {
			return err
		}
		for _, vlan := range vlans {
			if err := ifupdownCommand(ctx, run, []string{"ifdown", vlan}, restoring); err != nil {
				return err
			}
			a.vlansTakenDown = append(a.vlansTakenDown, vlan)
		}
		if err := ifupdownCommand(ctx, run, arguments, restoring); err != nil {
			return err
		}
		a.takenDown = append(a.takenDown, name)
	}
	for _, up := range bringUp {
		arguments := []string{"ifup", up.Interface}
		if up.ListedIn == "allow-hotplug" {
			// As at boot: an allow-hotplug interface without its device is skipped.
			arguments = []string{"ifup", "--allow=hotplug", up.Interface}
		}
		if err := ifupdownCommand(ctx, run, arguments, restoring); err != nil {
			return err
		}
	}
	for _, vlan := range a.vlansTakenDown {
		if err := ifupdownCommand(ctx, run, []string{"ifup", vlan}, restoring); err != nil {
			return err
		}
	}
	return nil
}

// vlansListedAsUpOn lists the VLAN devices on an interface that ifupdown
// lists as up, other than those the record takes down itself. Taking an
// interface down takes its VLANs down too, and bringing it up brings them
// back without the routes their stanzas add, so MMX takes them down before
// the interface and brings them up after it. ifdown and ifup read their
// stanzas from the files in place, whichever file declares them.
func vlansListedAsUpOn(ctx context.Context, run Runtime, name string, takeDown []string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(ifupdownLinksDirectory, name))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var vlans []string
	for _, entry := range entries {
		vlan, isUpper := strings.CutPrefix(entry.Name(), "upper_")
		if !isUpper || !ifupdownInterfaceName(vlan) || slices.Contains(takeDown, vlan) {
			continue
		}
		uevent, err := os.ReadFile(filepath.Join(ifupdownLinksDirectory, vlan, "uevent"))
		if err != nil || !slices.Contains(strings.Split(string(uevent), "\n"), "DEVTYPE=vlan") {
			continue
		}
		up, err := ifupdownListsAsUp(ctx, run, vlan)
		if err != nil {
			return nil, err
		}
		if up {
			vlans = append(vlans, vlan)
		}
	}
	return vlans, nil
}

func ifupdownCommand(ctx context.Context, run Runtime, arguments []string, restoring bool) error {
	command := strings.Join(arguments, " ")
	err := ifupdownStandardError(run.RunCommandThatChangesTheNode(ctx, arguments, ""), command)
	if err != nil && restoring {
		return fmt.Errorf("%s failed: %w", command, err)
	}
	return err
}

// ifupdownListsAsUp says whether ifupdown's state lists the interface as up.
func ifupdownListsAsUp(ctx context.Context, run Runtime, name string) (bool, error) {
	output := run.RunCommand(ctx, []string{"ifquery", "--state", name}, "")
	if output.Error != "" {
		return false, commandFailure(output, "ifquery --state "+name)
	}
	return output.ExitCode == 0, nil
}

func pathExists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}

// ifdown with the file that declared a virtual device removes the device
// itself, as measured for a VLAN and a bridge;
// CheckActivatedConfiguration checks that it did.
func (a *ifupdownActivation) RemoveObsoleteVirtualDevices(context.Context, Runtime, bool) error {
	return nil
}

func (a *ifupdownActivation) CheckActivatedConfiguration(ctx context.Context, run Runtime, restoring bool) error {
	_, _, bringUp := a.direction(restoring)
	cannotCheck := func(err error) error {
		return fmt.Errorf("Cannot check whether ifupdown accepted %s: %s.", a.destination, strings.TrimSuffix(err.Error(), "."))
	}
	if !restoring {
		for _, up := range bringUp {
			exists, err := pathExists(filepath.Join(ifupdownLinksDirectory, up.Interface))
			if err != nil {
				return cannotCheck(err)
			}
			if up.ListedIn == "allow-hotplug" && !exists {
				continue
			}
			listed, err := ifupdownListsAsUp(ctx, run, up.Interface)
			if err != nil {
				return cannotCheck(err)
			}
			if !listed {
				return fmt.Errorf("ifupdown did not accept %s: ifup ended without an error, but ifupdown does not list %s as up.", a.destination, up.Interface)
			}
		}
		for _, vlan := range a.vlansTakenDown {
			listed, err := ifupdownListsAsUp(ctx, run, vlan)
			if err != nil {
				return cannotCheck(err)
			}
			if !listed {
				return fmt.Errorf("ifupdown did not accept %s: ifup ended without an error, but ifupdown does not list the VLAN %s as up again.", a.destination, vlan)
			}
		}
	}
	for _, name := range a.takenDown {
		if slices.Contains(interfacesToBringUp(bringUp), name) {
			continue
		}
		virtual, err := pathExists(filepath.Join(ifupdownVirtualDirectory, name))
		if err != nil {
			return cannotCheck(err)
		}
		if !virtual {
			continue
		}
		// Another file that ifupdown reads may still declare the device.
		output := run.RunCommand(ctx, []string{"ifquery", name}, "")
		if output.Error != "" {
			return cannotCheck(commandFailure(output, "ifquery "+name))
		}
		if output.ExitCode != 0 {
			return fmt.Errorf("ifdown left the virtual device %s that %s declared.", name, a.destination)
		}
	}
	return nil
}
