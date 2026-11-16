package modules

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"

	"mmx/internal/protocol"
)

var netplanDirectories = []string{"/etc/netplan", "/run/netplan", "/lib/netplan"}
var netplanGeneratedDirectory = "/run/systemd/network"
var netplanLinkStateDirectory = "/run/systemd/netif/links"

// NetplanFile owns one YAML file. Netplan itself interprets the whole tree.
type NetplanFile NetworkFile

var netplanFileName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*\.yaml$`)
var netplanFileNameRule = parsed(func(name string) error {
	if !netplanFileName.MatchString(name) {
		return fmt.Errorf("The Netplan file name must be one filename ending in .yaml.")
	}
	return nil
})

func (n *NetplanFile) Prepare(base string) error { return (*NetworkFile)(n).prepare(base, "name") }
func (n *NetplanFile) Arguments(arguments *ArgumentList) {
	arguments.Public("name", &n.Name, netplanFileNameRule)
	if n.Content != nil {
		arguments.Private("content", *n.Content, scalar)
	}
}
func (n *NetplanFile) Reads() Reads                      { return (*NetworkFile)(n).Reads() }
func (n *NetplanFile) ControllerFiles() []ControllerFile { return (*NetworkFile)(n).ControllerFiles() }
func (n *NetplanFile) file() File {
	file := File{Path: filepath.Join(netplanDirectories[0], n.Name), State: n.State}
	if n.State != "absent" {
		file.Content, file.Template, file.TemplateText = n.Content, n.Template, n.TemplateText
		file.Asset, file.LocalPath = n.Asset, n.LocalPath
		file.Owner, file.Group, file.Mode = networkdRootUser, networkdRootGroup, "0600"
	}
	return file
}

func (n *NetplanFile) Run(ctx context.Context, task NodeTask) protocol.Result {
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
		compared.Reason = fmt.Sprintf("Cannot change %s: %s", file.Path, compared.Reason)
		return compared
	}
	if compared.Change == "unchanged" {
		compared.Reason = fmt.Sprintf("Network configuration file %s is unchanged; no activation or reachability confirmation wait was needed.", file.Path)
		return compared
	}
	if err := netplanPrerequisites(ctx, run); err != nil {
		result := run.notYet(fmt.Errorf("Cannot change %s: %w", file.Path, err))
		if !run.Dryrun {
			result.Change = "unchanged"
		}
		return result
	}
	if run.Dryrun {
		action := "replace"
		if compared.FileChange.Before == nil {
			action = "write"
		}
		compared.Reason = fmt.Sprintf("Would %s %s and apply the file with Netplan.", action, file.Path)
		if n.State == "absent" {
			compared.Reason = fmt.Sprintf("Would remove %s, the network configuration applied from the file, and the virtual network devices declared only by the file.", file.Path)
		}
		compared.Reason += " A dryrun does not check the proposed configuration with Netplan, apply the configuration, or check reachability after the change."
		return compared
	}
	data, err := planNetplanChange(ctx, run, file.Path, proposed, n.State == "absent")
	if err != nil {
		return refusal(err)
	}
	parent, err := run.openDefinitionDirectory(ctx, UnitManager{}, netplanDirectories[0], nil, n.State == "absent")
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
	if prepared.previous.Existed != data.previousExisted || (prepared.previous.Existed && prepared.previous.ContentsSHA256 != data.previousContentsSHA256) {
		return refusal(netplanCheckFailure(file.Path, fmt.Errorf("The existing Netplan file changed while MMX checked the proposed configuration.")))
	}
	saved, err := json.Marshal(data)
	if err != nil {
		return refusal(err)
	}
	wait, _ := NetworkFile(*n).confirmationWait()
	result = task.OwedRestores.ApplyChange(ctx, ChangeWithOwedRestore{PreparedFile: prepared, RestoreConfirmationWait: wait, ModuleRestoreData: saved})
	return result
}

func netplanPrerequisites(ctx context.Context, run Runtime) error {
	for _, tool := range []string{"netplan", "networkctl", "systemctl", "ip"} {
		if _, err := lookPath(ctx, tool); err != nil {
			return missingProgramError(tool)
		}
	}
	output := run.RunCommand(ctx, []string{"systemctl", "is-active", "systemd-networkd.service"}, "")
	if output.Error != "" {
		return commandFailure(output, "Checking whether systemd-networkd is running")
	}
	if output.ExitCode != 0 || strings.TrimSpace(output.StandardOutput) != "active" {
		return fmt.Errorf("systemd-networkd.service is not running; netplan_file supports only the systemd-networkd renderer.")
	}
	return nil
}

var netplanFileDescription = Description{
	Name: "netplan_file", Topic: TopicNetworkConfiguration, Schema: reflect.TypeOf(NetplanFile{}),
	Summary: "This module manages one Netplan YAML file with the systemd-networkd renderer. The node restores an unconfirmed change by itself.",
	Fields: []Field{
		{Name: "name", Required: true, Notes: "Choose one filename ending in .yaml in /etc/netplan/. The file belongs to root:root with mode 0600."},
		{Name: "content", Notes: "Supply the complete Netplan configuration text. With state: present, provide exactly one of content or template."},
		{Name: "template", Notes: "Use a Go template on the controller machine, relative to the project or role directory."},
		{Name: "state", Values: presenceStates, Default: "present", Notes: "With state: absent, removes the Netplan file, the network configuration applied from the file, and virtual network devices declared only by the file. Removing a bridge also removes the bridge address and detaches the bridge ports, disconnecting the attached networks from the bridge."},
		{Name: "restore_unless_controller_confirms_within", Default: "2m", Notes: "Choose a positive duration for reachability confirmation after the change before the node restores the previous file. Activation uses part of this time. The wait before the change has the same limit."},
	},
	Example:   "- netplan_file:\n    name: 60-second-card.yaml\n    content: |\n      network:\n        version: 2\n        renderer: networkd\n        ethernets:\n          enp6s0:\n            dhcp4: false\n            addresses: [10.0.0.5/24]\n",
	Results:   []string{"status", "change", "reason"},
	Dryrun:    "Dryrun compares the file without writing or applying anything. Netplan's check of the proposed configuration, activation, and reachability are not checked. Dryrun does not list the virtual devices that a removal would delete, because their names come from Netplan's own generation, which dryrun does not run. Missing prerequisites are not checked.",
	WaitLimit: "restore_unless_controller_confirms_within bounds the change",
}
