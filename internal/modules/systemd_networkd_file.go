package modules

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"mmx/internal/protocol"
)

var networkdDirectories = []string{"/etc/systemd/network", "/run/systemd/network", "/usr/lib/systemd/network"}
var networkdLinksDirectory = "/sys/class/net"
var networkdVirtualDirectory = "/sys/devices/virtual/net"
var networkdWaitOnlinePrograms = []string{"/usr/lib/systemd/systemd-networkd-wait-online", "/lib/systemd/systemd-networkd-wait-online"}

// Tests replace the root identities along with the node's directories, so
// they can compare unchanged files without requiring root on the test host.
var networkdRootUser, networkdRootGroup = Account("0"), Account("0")

// SystemdNetworkdFile owns one .network or .netdev file, not the other
// configuration files read by the service.
type SystemdNetworkdFile NetworkFile

var networkdFileName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*\.(network|netdev)$`)
var networkdFileNameRule = parsed(func(name string) error {
	if strings.HasSuffix(name, ".link") {
		return fmt.Errorf("systemd_networkd_file refuses %s: .link files cannot be activated and restored by reloading systemd-networkd.", name)
	}
	if !networkdFileName.MatchString(name) {
		return fmt.Errorf("The systemd-networkd file name must be one filename ending in .network or .netdev.")
	}
	return nil
})

func (n *SystemdNetworkdFile) Prepare(base string) error {
	return (*NetworkFile)(n).prepare(base, "name")
}
func (n *SystemdNetworkdFile) Arguments(arguments *ArgumentList) {
	arguments.Public("name", &n.Name, networkdFileNameRule)
	if n.Content != nil {
		arguments.Private("content", *n.Content, scalar)
	}
}
func (n *SystemdNetworkdFile) Reads() Reads { return (*NetworkFile)(n).Reads() }
func (n *SystemdNetworkdFile) ControllerFiles() []ControllerFile {
	return (*NetworkFile)(n).ControllerFiles()
}

func (n *SystemdNetworkdFile) file() File {
	file := File{Path: filepath.Join(networkdDirectories[0], n.Name), State: n.State}
	if n.State != "absent" {
		file.Content, file.Template, file.TemplateText = n.Content, n.Template, n.TemplateText
		file.Asset, file.LocalPath = n.Asset, n.LocalPath
		file.Owner, file.Group, file.Mode = networkdRootUser, networkdRootGroup, "0644"
		if strings.HasSuffix(n.Name, ".netdev") {
			file.Group, file.Mode = "systemd-network", "0640"
		}
	}
	return file
}

func (n *SystemdNetworkdFile) Run(ctx context.Context, task NodeTask) protocol.Result {
	run, file := task.Runtime, n.file()
	refusal := func(err error) protocol.Result {
		result := protocol.Failed(err)
		result.Change = "unchanged"
		result.NoOtherTry = true
		return result
	}
	// Check even when the file is already absent or unchanged, including dryrun.
	if task.OwedRestores == nil {
		return refusal(fmt.Errorf("Changing a network configuration file requires the node's owed restore service."))
	}
	if err := task.OwedRestores.CheckChangeAllowed(ctx, file.Path); err != nil {
		return refusal(err)
	}
	var proposed string
	if n.State != "absent" {
		content, err := OpenFileContents(file, task.Scope)
		if err != nil {
			return refusal(err)
		}
		data, err := io.ReadAll(content)
		content.Close()
		if err != nil {
			return refusal(err)
		}
		proposed = string(data)
		file.Content, file.Template, file.Literal = nil, "", &proposed
	}
	preview := run
	preview.Dryrun = true
	_, compared := preview.PrepareFileChange(ctx, file, task.Scope, nil)
	if compared.Status != "succeeded" {
		if compared.Status == "not_checked" && !run.Dryrun {
			compared.Status = "failed"
		}
		if compared.Status == "failed" {
			compared.Change = "unchanged"
		}
		compared.Reason = fmt.Sprintf("Cannot change network configuration file %s: %s", file.Path, compared.Reason)
		return compared
	}
	if compared.Change == "unchanged" {
		compared.Reason = fmt.Sprintf("Network configuration file %s is unchanged; no activation or confirmation wait was needed.", file.Path)
		return compared
	}
	if err := networkdPrerequisites(ctx, run); err != nil {
		result := run.notYet(fmt.Errorf("Cannot change network configuration file %s: %w", file.Path, err))
		if !run.Dryrun {
			result.Change = "unchanged"
		}
		return result
	}
	data, err := planNetworkdDevices(file.Path, proposed)
	if err != nil {
		return refusal(err)
	}
	if run.Dryrun {
		action := "replace"
		if compared.FileChange.Before == nil {
			action = "write"
		}
		compared.Reason = fmt.Sprintf("Would %s %s and activate it.", action, file.Path)
		if n.State == "absent" {
			compared.Reason = fmt.Sprintf("Would remove %s and its running configuration.", file.Path)
		}
		compared.Reason += " Whether systemd-networkd accepts the proposed network configuration change and whether the node remains reachable after activation were not checked."
		if len(data.DevicesToRemove) > 0 {
			compared.Reason += " Would remove these declared virtual devices: " + strings.Join(data.DevicesToRemove, ", ") + "."
		}
		return compared
	}
	parent, err := run.openDefinitionDirectory(ctx, UnitManager{}, networkdDirectories[0], nil, n.State == "absent")
	if err != nil {
		return refusal(err)
	}
	if parent != nil {
		parent.Close()
	}
	prepared, result := run.PrepareFileChange(ctx, file, task.Scope, nil)
	if prepared == nil {
		return result
	}
	// Closing the prepared change only cleans temporary candidates and directory handles.
	defer func() { _ = prepared.Close() }()
	if strings.HasSuffix(file.Path, ".netdev") && prepared.previous.Existed && prepared.previous.ContentsSHA256 != data.previousContentsSHA256 {
		return refusal(fmt.Errorf("Network configuration file %s changed while MMX was checking devices declared by the file.", file.Path))
	}
	saved, err := json.Marshal(data)
	if err != nil {
		return refusal(err)
	}
	wait, _ := NetworkFile(*n).confirmationWait()
	result = task.OwedRestores.ApplyChange(ctx, ChangeWithOwedRestore{PreparedFile: prepared, RestoreConfirmationWait: wait, ModuleRestoreData: saved})
	return result
}

func networkdPrerequisites(ctx context.Context, run Runtime) error {
	for _, tool := range []string{"networkctl", "journalctl", "systemctl"} {
		if _, err := lookPath(ctx, tool); err != nil {
			return missingProgramError(tool)
		}
	}
	output := run.RunCommand(ctx, []string{"systemctl", "is-active", "systemd-networkd.service"}, "")
	if output.Error != "" {
		return commandFailure(output, "systemctl is-active systemd-networkd.service")
	}
	if output.ExitCode != 0 || strings.TrimSpace(output.StandardOutput) != "active" {
		return fmt.Errorf("systemd-networkd.service is not running.")
	}
	return nil
}

// Only device identities survive into the restore process. Its compiled
// implementation always reloads the service, never a command from this data.
type networkdRestoreData struct {
	previousContentsSHA256   string
	DevicesToRemove          []string `json:"virtual_devices_to_remove_after_file_change,omitempty"`
	DevicesToRemoveOnRestore []string `json:"virtual_devices_to_remove_on_restore,omitempty"`
}

type networkdActivation struct {
	destination string
	data        networkdRestoreData
	cursor      string
}

func newNetworkdActivation(destination string, saved json.RawMessage) (ChangeActivation, error) {
	if filepath.Dir(destination) != networkdDirectories[0] || !networkdFileName.MatchString(filepath.Base(destination)) {
		return nil, fmt.Errorf("Cannot activate or restore %s: the saved destination must be a .network or .netdev file in %s.", destination, networkdDirectories[0])
	}
	activation := &networkdActivation{destination: destination}
	if err := protocol.Decode(saved, &activation.data); err != nil {
		return nil, err
	}
	for _, names := range [][]string{activation.data.DevicesToRemove, activation.data.DevicesToRemoveOnRestore} {
		for i, name := range names {
			if !networkdDeviceName(name) || slices.Contains(names[:i], name) {
				return nil, fmt.Errorf("Saved systemd-networkd device name %q is invalid or repeated.", name)
			}
		}
	}
	if strings.HasSuffix(destination, ".network") && len(activation.data.DevicesToRemove)+len(activation.data.DevicesToRemoveOnRestore) != 0 {
		return nil, fmt.Errorf("Cannot activate or restore %s: a .network file cannot declare devices to delete.", destination)
	}
	return activation, nil
}
func (a *networkdActivation) PrepareActivation(ctx context.Context, run Runtime, restoring bool) (err error) {
	defer func() {
		if err != nil && !restoring {
			err = fmt.Errorf("Cannot check whether systemd-networkd accepted %s: %s.", a.destination, strings.TrimSuffix(err.Error(), "."))
		}
	}()
	if restoring {
		waitForNetworkdStartup(ctx, run)
		return nil
	}
	output := run.RunCommand(ctx, []string{"journalctl", "--no-pager", "-n", "1", "-o", "cat", "--show-cursor"}, "")
	if err := commandFailure(output, "Reading the journal position before the network change"); err != nil {
		return err
	}
	for _, line := range strings.Split(output.StandardOutput, "\n") {
		if strings.HasPrefix(line, "-- cursor: ") {
			a.cursor = strings.TrimPrefix(line, "-- cursor: ")
		}
	}
	if a.cursor == "" || strings.Contains(output.StandardOutput, protocol.TruncationMarker) {
		return fmt.Errorf("The systemd journal did not provide a cursor marking the position before the network change.")
	}
	return nil
}
func (a *networkdActivation) Activate(ctx context.Context, run Runtime, restoring bool) (err error) {
	defer func() {
		if err != nil && !restoring {
			err = fmt.Errorf("systemd-networkd did not accept %s: %s.", a.destination, strings.TrimSuffix(err.Error(), "."))
		}
	}()
	output := run.RunCommandThatChangesTheNode(ctx, []string{"networkctl", "reload"}, "")
	return commandFailure(output, "Reloading systemd-networkd")
}
func (a *networkdActivation) devices(restoring bool) []string {
	if restoring {
		return a.data.DevicesToRemoveOnRestore
	}
	return a.data.DevicesToRemove
}
func (a *networkdActivation) removalFailure(name string, err error) error {
	return fmt.Errorf("Cannot remove virtual device %s declared in %s: %s.", name, a.destination, strings.TrimSuffix(err.Error(), "."))
}
func (a *networkdActivation) deviceRemoved(name string) error {
	if exists, err := networkdDeviceExists(name); err != nil {
		return a.removalFailure(name, err)
	} else if exists {
		return a.removalFailure(name, fmt.Errorf("the virtual device still exists after networkctl delete"))
	}
	return nil
}
func (a *networkdActivation) RemoveObsoleteVirtualDevices(ctx context.Context, run Runtime, restoring bool) error {
	// Check every identity before the first deletion: deleting a veth end
	// removes its peer, which must pass the same ownership checks first.
	for _, name := range a.devices(restoring) {
		exists, err := networkdDeviceExists(name)
		if err != nil {
			return a.removalFailure(name, err)
		}
		if exists {
			if err := networkdCanDelete(a.destination, name, restoring); err != nil {
				return a.removalFailure(name, err)
			}
		}
	}
	for _, name := range a.devices(restoring) {
		exists, err := networkdDeviceExists(name)
		if err != nil {
			return a.removalFailure(name, err)
		}
		if !exists {
			continue
		}
		// The previous deletion may have changed ownership or references.
		if err := networkdCanDelete(a.destination, name, restoring); err != nil {
			return a.removalFailure(name, err)
		}
		output := run.RunCommandThatChangesTheNode(ctx, []string{"networkctl", "delete", name}, "")
		if err := commandFailure(output, "Removing virtual device "+name); err != nil {
			return a.removalFailure(name, err)
		}
		if err := a.deviceRemoved(name); err != nil {
			return err
		}
	}
	return nil
}
func (a *networkdActivation) CheckActivatedConfiguration(ctx context.Context, run Runtime, restoring bool) error {
	for _, name := range a.devices(restoring) {
		if err := a.deviceRemoved(name); err != nil {
			return err
		}
	}
	if restoring {
		return nil
	}
	cannotCheck := func(err error) error {
		return fmt.Errorf("Cannot check whether systemd-networkd accepted %s: %s.", a.destination, strings.TrimSuffix(err.Error(), "."))
	}
	output := run.RunCommand(ctx, []string{"journalctl", "--sync"}, "")
	if err := commandFailure(output, "Synchronizing the journal after the network change"); err != nil {
		return cannotCheck(err)
	}
	if a.cursor == "" {
		return cannotCheck(fmt.Errorf("the journal position before the network change is missing"))
	}
	output = run.RunCommand(ctx, []string{"journalctl", "--no-pager", "--cursor", a.cursor, "-u", "systemd-networkd.service", "-n", "200", "-o", "json", "--output-fields=__CURSOR,PRIORITY,MESSAGE,CONFIG_FILE,CONFIG_LINE,SYSLOG_IDENTIFIER,_SYSTEMD_UNIT"}, "")
	if err := commandFailure(output, "Reading the journal after the network change"); err != nil {
		return cannotCheck(err)
	}
	diagnostic, err := checkNetworkdJournal(a.destination, a.cursor, output.StandardOutput)
	if err != nil {
		return cannotCheck(err)
	}
	if diagnostic != "" {
		return fmt.Errorf("systemd-networkd did not accept %s: %s.", a.destination, strings.TrimSuffix(diagnostic, "."))
	}
	return nil
}

var systemdNetworkdFileDescription = Description{
	Name: "systemd_networkd_file", Topic: TopicNetworkConfiguration, Schema: reflect.TypeOf(SystemdNetworkdFile{}),
	Summary: "Manages one .network or .netdev file and reloads systemd-networkd. The node restores an unconfirmed change by itself.",
	Fields: []Field{
		{Name: "name", Required: true, Notes: "One .network or .netdev filename in /etc/systemd/network; .link files are refused."},
		{Name: "content", Notes: "The complete file text; exactly one of content and template is required for a present file."},
		{Name: "template", Notes: "A Go template on the controller machine, relative to the project or role directory."},
		{Name: "state", Values: presenceStates, Default: "present", Notes: "Absent removes the file, its running configuration, and its declared virtual devices that no other file declares. Remove references from other .network files first."},
		{Name: "restore_unless_controller_confirms_within", Default: "2m", Notes: "How long the node waits after the change for the controller process to confirm reachability before restoring the previous file. Activation uses part of this time. The wait before the change has the same limit."},
	},
	Example:   "- systemd_networkd_file:\n    name: 20-second.network\n    content: |\n      [Match]\n      Name=eth1\n      [Network]\n      DHCP=no\n      Address=10.0.0.5/24\n",
	Results:   []string{"status", "change", "reason"},
	Dryrun:    "Compares the file and reports device removals without writing or activating anything. Missing prerequisites are not checked.",
	WaitLimit: "restore_unless_controller_confirms_within bounds the change",
}

func waitForNetworkdStartup(ctx context.Context, run Runtime) {
	// On systemd 252.23, reloading during initial link setup can leave a
	// working link permanently configuring; Debian 12 runs systemd 252 too.
	// Give startup a bounded wait;
	// a missing program or a link that never becomes online must not stop restoration.
	for _, program := range networkdWaitOnlinePrograms {
		if run.FindProgram(ctx, program).Found {
			run.RunCommand(ctx, []string{program, "--timeout=10"}, "")
			break
		}
	}
}
