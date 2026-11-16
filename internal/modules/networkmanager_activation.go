package modules

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"mmx/internal/protocol"
)

type networkManagerActivation struct {
	destination, cursor string
	data                networkManagerRestoreData
}

func newNetworkManagerActivation(destination string, saved json.RawMessage) (ChangeActivation, error) {
	if filepath.Dir(destination) != networkManagerDirectory || networkManagerFileName(filepath.Base(destination)) != nil {
		return nil, fmt.Errorf("Cannot activate or restore %s: the saved destination must be a .nmconnection file in %s.", destination, networkManagerDirectory)
	}
	adapter := &networkManagerActivation{destination: destination}
	if err := protocol.Decode(saved, &adapter.data); err != nil {
		return nil, err
	}
	for _, name := range []string{adapter.data.DeviceToRemove, adapter.data.DeviceToRemoveOnRestore} {
		if !networkManagerInterfaceName(name) {
			return nil, fmt.Errorf("The saved NetworkManager interface name %q is invalid.", name)
		}
	}
	for _, uuid := range adapter.data.PreviouslyActivatedProfiles {
		if !networkManagerUUID.MatchString(uuid) {
			return nil, fmt.Errorf("A saved NetworkManager profile UUID is invalid.")
		}
	}
	return adapter, nil
}
func (a *networkManagerActivation) rejected(err error) error {
	return fmt.Errorf("NetworkManager did not accept %s: %s.", a.destination, strings.TrimSuffix(err.Error(), "."))
}
func (a *networkManagerActivation) unchecked(err error) error {
	return fmt.Errorf("Cannot check whether NetworkManager accepted %s: %s.", a.destination, strings.TrimSuffix(err.Error(), "."))
}
func (a *networkManagerActivation) PrepareActivation(ctx context.Context, run Runtime, restoring bool) error {
	if restoring {
		return nil
	}
	cursor, err := networkJournalPosition(ctx, run)
	if err != nil {
		return a.unchecked(err)
	}
	a.cursor = cursor
	return nil
}

var networkManagerKeyfileWarning = regexp.MustCompile(`^<[a-z]+>[ \t]+\[[0-9]+\.[0-9]+\][ \t]+keyfile:`)

func (a *networkManagerActivation) Activate(ctx context.Context, run Runtime, restoring bool) error {
	output := run.RunCommandThatChangesTheNode(ctx, []string{"nmcli", "connection", "load", a.destination}, "")
	if err := commandFailure(output, "Loading the NetworkManager file"); err != nil {
		return a.rejected(err)
	}
	// An existing, previously unloaded file may be broken. Putting it back
	// must not require NetworkManager to accept what it never loaded before.
	previousFileWasUnloaded := restoring && a.data.PreviousFilePresent && !a.data.PreviousProfileLoaded
	if !previousFileWasUnloaded && (output.StandardOutput != "" || output.StandardError != "") {
		text := strings.TrimSpace(output.StandardOutput + " " + output.StandardError)
		if text == "" {
			text = "the load command produced unexpected output"
		}
		return a.rejected(fmt.Errorf("%s", text))
	}
	profiles, err := networkManagerProfiles(ctx, run)
	if err != nil {
		return a.unchecked(err)
	}
	uuid := ""
	for _, profile := range profiles {
		if profile.Filename == a.destination {
			if uuid != "" {
				return a.unchecked(fmt.Errorf("more than one profile is loaded from the file"))
			}
			uuid = profile.UUID
		}
	}
	present := a.data.ProposedFilePresent
	if restoring {
		present = a.data.PreviousFilePresent
	}
	if !previousFileWasUnloaded && present != (uuid != "") {
		if present {
			return a.rejected(fmt.Errorf("the file has no loaded profile"))
		}
		return a.rejected(fmt.Errorf("the removed file still has a loaded profile"))
	}
	if !restoring {
		entries, err := readNetworkServiceJournal(ctx, run, "NetworkManager.service", a.cursor)
		if err != nil {
			return a.unchecked(err)
		}
		for _, entry := range entries {
			if entry.Priority <= 4 && (networkdMessageNamesFile(entry.Message, a.destination) || entry.ConfigurationFile == a.destination || networkManagerKeyfileWarning.MatchString(entry.Message)) {
				return a.rejected(fmt.Errorf("%s", entry.Message))
			}
		}
		if present {
			return a.activateProfile(ctx, run, uuid, false)
		}
		return nil
	}
	if present && a.data.PreviousProfileActivated {
		if err := a.activateProfile(ctx, run, uuid, false); err != nil {
			return err
		}
	} else if uuid != "" {
		activity, err := networkManagerProfileActivity(ctx, run)
		if err != nil {
			return a.unchecked(err)
		}
		if activity[uuid].inUse {
			output := run.RunCommandThatChangesTheNode(ctx, []string{"nmcli", "connection", "down", "uuid", uuid}, "")
			if err := networkManagerConnectionFailure(output); err != nil {
				return fmt.Errorf("NetworkManager could not deactivate the profile of %s: %s.", a.destination, strings.TrimSuffix(err.Error(), "."))
			}
			activity, err = networkManagerProfileActivity(ctx, run)
			if err != nil {
				return a.unchecked(err)
			}
			if activity[uuid].inUse {
				return fmt.Errorf("NetworkManager did not deactivate the profile of %s: the profile is still in use.", a.destination)
			}
		}
	}
	loaded := map[string]bool{}
	for _, profile := range profiles {
		loaded[profile.UUID] = true
	}
	for _, previous := range a.data.PreviouslyActivatedProfiles {
		if previous == uuid || !loaded[previous] {
			continue
		}
		// A preceding activation may also have brought this profile back.
		activity, err := networkManagerProfileActivity(ctx, run)
		if err != nil {
			return a.unchecked(err)
		}
		if !activity[previous].activated {
			if err := a.activateProfile(ctx, run, previous, true); err != nil {
				return err
			}
		}
	}
	return nil
}
func (a *networkManagerActivation) activateProfile(ctx context.Context, run Runtime, uuid string, otherProfile bool) error {
	output := run.RunCommandThatChangesTheNode(ctx, []string{"nmcli", "connection", "up", "uuid", uuid}, "")
	if err := networkManagerConnectionFailure(output); err != nil {
		if otherProfile {
			return fmt.Errorf("NetworkManager could not activate profile %s again after the restore of %s: %s.", uuid, a.destination, strings.TrimSuffix(err.Error(), "."))
		}
		return fmt.Errorf("NetworkManager could not activate the profile of %s: %s.", a.destination, strings.TrimSuffix(err.Error(), "."))
	}
	return nil
}

var networkManagerJournalHint = regexp.MustCompile(`^Hint: use ['"]journalctl [^'"]*['"] to get more details\.?$`)

func networkManagerConnectionFailure(output CommandOutput) error {
	if output.Error != "" {
		return fmt.Errorf("%s", output.Error)
	}
	if output.ExitCode == 0 {
		return nil
	}
	var lines []string
	for _, line := range strings.Split(output.StandardError+"\n"+output.StandardOutput, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !networkManagerJournalHint.MatchString(line) {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return fmt.Errorf("nmcli exited with %d without a diagnostic", output.ExitCode)
	}
	return fmt.Errorf("%s", strings.Join(lines, "\n"))
}
func (a *networkManagerActivation) removalDevice(restoring bool) string {
	if restoring {
		return a.data.DeviceToRemoveOnRestore
	}
	return a.data.DeviceToRemove
}
func (a *networkManagerActivation) removalError(name string, err error) error {
	return fmt.Errorf("Cannot remove virtual device %s of %s: %s.", name, a.destination, strings.TrimSuffix(err.Error(), "."))
}
func (a *networkManagerActivation) RemoveObsoleteVirtualDevices(ctx context.Context, run Runtime, restoring bool) error {
	name := a.removalDevice(restoring)
	if name == "" {
		return nil
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(100 * time.Millisecond)
	defer poll.Stop()
	for {
		exists, virtual, err := networkManagerDevice(name)
		if err != nil {
			return a.removalError(name, err)
		}
		if !exists || !virtual {
			return nil
		}
		select {
		case <-ctx.Done():
			return a.removalError(name, ctx.Err())
		case <-poll.C:
			continue
		case <-deadline.C:
		}
		break
	}
	// Recheck ownership and virtual status immediately before explicit deletion.
	profiles, err := networkManagerProfiles(ctx, run)
	if err != nil {
		return a.removalError(name, err)
	}
	for _, profile := range profiles {
		if profile.Filename == a.destination {
			continue
		}
		profile, err = networkManagerProfileDetails(ctx, run, profile)
		if err != nil {
			return a.removalError(name, err)
		}
		if profile.Interface == name {
			return a.removalError(name, fmt.Errorf("another loaded profile in %s still declares it", profile.Filename))
		}
	}
	exists, virtual, err := networkManagerDevice(name)
	if err != nil {
		return a.removalError(name, err)
	}
	if !exists || !virtual {
		return nil
	}
	output := run.RunCommandThatChangesTheNode(ctx, []string{"nmcli", "device", "delete", name}, "")
	if err := commandFailure(output, "Deleting the virtual device"); err != nil {
		return a.removalError(name, err)
	}
	return a.CheckActivatedConfiguration(ctx, run, restoring)
}
func (a *networkManagerActivation) CheckActivatedConfiguration(ctx context.Context, run Runtime, restoring bool) error {
	name := a.removalDevice(restoring)
	if name == "" {
		return nil
	}
	exists, virtual, err := networkManagerDevice(name)
	if err != nil {
		return a.removalError(name, err)
	}
	if exists && virtual {
		return a.removalError(name, fmt.Errorf("the device still exists after removal"))
	}
	return nil
}
