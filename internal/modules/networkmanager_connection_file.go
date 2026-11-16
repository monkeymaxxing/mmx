package modules

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"unicode"

	"mmx/internal/protocol"
)

var networkManagerDirectory = "/etc/NetworkManager/system-connections"
var networkManagerLinksDirectory = "/sys/class/net"
var networkManagerVirtualDirectory = "/sys/devices/virtual/net"
var networkManagerRootUser, networkManagerRootGroup = Account("0"), Account("0")

// NetworkManagerConnectionFile owns one keyfile and its loaded profile.
type NetworkManagerConnectionFile NetworkFile

func networkManagerFileName(name string) error {
	if name == "" || strings.HasPrefix(name, ".") || strings.Contains(name, "/") || !strings.HasSuffix(name, ".nmconnection") || strings.ContainsFunc(name, unicode.IsControl) {
		return fmt.Errorf("The NetworkManager file name must be one filename ending in .nmconnection, without a leading dot or control characters.")
	}
	return nil
}
func (n *NetworkManagerConnectionFile) Prepare(base string) error {
	return (*NetworkFile)(n).prepare(base, "name")
}
func (n *NetworkManagerConnectionFile) Arguments(arguments *ArgumentList) {
	arguments.Public("name", &n.Name, parsed(networkManagerFileName))
	if n.Content != nil {
		arguments.Private("content", *n.Content, scalar)
	}
}
func (n *NetworkManagerConnectionFile) Reads() Reads { return (*NetworkFile)(n).Reads() }
func (n *NetworkManagerConnectionFile) ControllerFiles() []ControllerFile {
	return (*NetworkFile)(n).ControllerFiles()
}
func (n *NetworkManagerConnectionFile) file() File {
	file := File{Path: filepath.Join(networkManagerDirectory, n.Name), State: n.State}
	if n.State != "absent" {
		file.Content, file.Template, file.TemplateText = n.Content, n.Template, n.TemplateText
		file.Asset, file.LocalPath = n.Asset, n.LocalPath
		file.Owner, file.Group, file.Mode = networkManagerRootUser, networkManagerRootGroup, "0600"
	}
	return file
}
func (n *NetworkManagerConnectionFile) Run(ctx context.Context, task NodeTask) protocol.Result {
	run, file := task.Runtime, n.file()
	refusal := func(err error) protocol.Result {
		result := protocol.Failed(err)
		result.Change = "unchanged"
		result.NoOtherTry = true
		return result
	}
	if task.OwedRestores == nil {
		return refusal(fmt.Errorf("Changing a network configuration file requires the node's owed restore service."))
	}
	if err := task.OwedRestores.CheckChangeAllowed(ctx, file.Path); err != nil {
		return refusal(err)
	}
	var proposed string
	if n.State != "absent" {
		contents, err := OpenFileContents(file, task.Scope)
		if err != nil {
			return refusal(err)
		}
		data, err := io.ReadAll(contents)
		contents.Close()
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
		compared.Reason = fmt.Sprintf("Cannot change %s: %s", file.Path, compared.Reason)
		return compared
	}
	if compared.Change == "unchanged" {
		compared.Reason = fmt.Sprintf("Network configuration file %s is unchanged; no activation or confirmation wait was needed.", file.Path)
		return compared
	}
	// The offline check does not need a running daemon or the other tools.
	if !run.FindProgram(ctx, "nmcli").Found {
		return networkManagerNotYet(run, file.Path, missingProgramError("nmcli"))
	}
	candidate := networkManagerProfile{}
	if n.State != "absent" {
		var err error
		candidate, err = checkNetworkManagerCandidate(ctx, run, n.Name, proposed)
		if err != nil {
			return refusal(fmt.Errorf("NetworkManager does not accept the proposed %s: %s.", file.Path, strings.TrimSuffix(err.Error(), ".")))
		}
	}
	for _, program := range []string{"journalctl", "systemctl"} {
		if !run.FindProgram(ctx, program).Found {
			return networkManagerNotYet(run, file.Path, fmt.Errorf("%s is not installed.", program))
		}
	}
	output := run.RunCommand(ctx, []string{"systemctl", "is-active", "NetworkManager.service"}, "")
	if output.Error != "" || output.ExitCode != 0 || strings.TrimSpace(output.StandardOutput) != "active" {
		return networkManagerNotYet(run, file.Path, fmt.Errorf("NetworkManager.service is not running."))
	}
	data, err := planNetworkManagerChange(ctx, run, file.Path, candidate, n.State != "absent", compared.FileChange.Before != nil)
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
		compared.Reason += " The service's acceptance and reachability after activation were not checked."
		if data.DeviceToRemove != "" {
			compared.Reason += " Would remove this declared virtual device: " + data.DeviceToRemove + "."
		}
		return compared
	}
	parent, err := run.openDefinitionDirectory(ctx, UnitManager{}, networkManagerDirectory, nil, n.State == "absent")
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
	data.PreviousFilePresent = prepared.previous.Existed
	saved, err := json.Marshal(data)
	if err != nil {
		return refusal(err)
	}
	wait, _ := NetworkFile(*n).confirmationWait()
	return task.OwedRestores.ApplyChange(ctx, ChangeWithOwedRestore{PreparedFile: prepared, RestoreConfirmationWait: wait, ModuleRestoreData: saved})
}
func networkManagerNotYet(run Runtime, path string, err error) protocol.Result {
	result := run.notYet(fmt.Errorf("Cannot change %s: %w", path, err))
	if !run.Dryrun {
		result.Change = "unchanged"
	}
	return result
}

var networkManagerConnectionFileDescription = Description{
	Name: "networkmanager_connection_file", Topic: TopicNetworkConfiguration, Schema: reflect.TypeOf(NetworkManagerConnectionFile{}),
	Summary: "Manages one NetworkManager keyfile and activates its profile. The node restores an unconfirmed change and the previously activated profiles by itself.",
	Fields: []Field{
		{Name: "name", Required: true, Notes: "One .nmconnection filename in /etc/NetworkManager/system-connections. Spaces are allowed; a leading dot and control characters are refused. Files use root:root and mode 0600."},
		{Name: "content", Notes: "The complete keyfile text; exactly one of content and template is required for a present file."},
		{Name: "template", Notes: "A Go template on the controller machine, relative to the project or role directory."},
		{Name: "state", Values: presenceStates, Default: "present", Notes: "Absent removes the file, its loaded profile and running configuration, and its unshared virtual device. Physical cards and other profile files stay."},
		{Name: "restore_unless_controller_confirms_within", Default: "2m", Notes: "How long the node waits after the change for the controller process to confirm reachability before restoring the previous file. Activation uses part of this time. The wait before the change has the same limit."},
	},
	Example:   "- networkmanager_connection_file:\n    name: second.nmconnection\n    content: |\n      [connection]\n      id=second\n      type=ethernet\n      interface-name=eth1\n      [ipv4]\n      method=manual\n      address1=10.0.0.5/24\n      [ipv6]\n      method=disabled\n",
	Results:   []string{"status", "change", "reason"},
	Dryrun:    "Checks the candidate with nmcli offline and checks profile conflicts, without installing or activating anything. Missing prerequisites are not checked. NetworkManager's offline check does not reject unknown keys and sections.",
	WaitLimit: "restore_unless_controller_confirms_within bounds the change",
}
