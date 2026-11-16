package modules

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"mmx/internal/assets"
	"mmx/internal/protocol"
)

var ifupdownMainFile = "/etc/network/interfaces"
var ifupdownDirectory = "/etc/network/interfaces.d"
var ifupdownLinksDirectory = "/sys/class/net"
var ifupdownVirtualDirectory = "/sys/devices/virtual/net"
var ifupdownRootUser, ifupdownRootGroup = Account("0"), Account("0")

// IfupdownInterfacesFile owns /etc/network/interfaces or one file directly in
// /etc/network/interfaces.d. Name holds the path and is written `path`.
type IfupdownInterfacesFile struct {
	Name                                  string                    `yaml:"path" json:"path"`
	Mode                                  string                    `yaml:"permissions_mode,omitempty" json:"permissions_mode,omitempty"`
	Content                               *string                   `yaml:"content,omitempty" json:"content,omitempty"`
	Template                              string                    `yaml:"template,omitempty" json:"template,omitempty"`
	State                                 string                    `yaml:"state,omitempty" json:"state,omitempty"`
	RestoreUnlessControllerConfirmsWithin string                    `yaml:"restore_unless_controller_confirms_within,omitempty" json:"restore_unless_controller_confirms_within,omitempty"`
	TemplateText                          string                    `yaml:"-" json:"template_text,omitempty"`
	Asset                                 *assets.Ref               `yaml:"-" json:"asset,omitempty"`
	LocalPath                             string                    `yaml:"-" json:"-"`
	SourceBase                            string                    `yaml:"-" json:"-"`
	Variants                              map[string]*SourceVariant `yaml:"-" json:"variants,omitempty"`
}

// The names that both `source /etc/network/interfaces.d/*` and
// `source-directory /etc/network/interfaces.d` read: run-parts names.
var ifupdownFileName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func ifupdownPathRule(path, state string) error {
	if path == ifupdownMainFile {
		if state == "absent" {
			return fmt.Errorf("ifupdown_interfaces_file cannot remove %s: ifupdown and networking.service need the main interfaces file; removal would lose the main interface settings at the next boot.", path)
		}
		return nil
	}
	if filepath.Dir(path) != ifupdownDirectory || !ifupdownFileName.MatchString(filepath.Base(path)) {
		return fmt.Errorf("The path must be %s or %s/<name>, where the name has only letters, digits, underscores, and hyphens, because source-directory reads only such names.", ifupdownMainFile, ifupdownDirectory)
	}
	return nil
}

func (n *IfupdownInterfacesFile) Prepare(base string) error {
	if n.Mode != "" && !modePattern.MatchString(n.Mode) {
		return invalid("permissions_mode", `permissions_mode must be a quoted octal string, such as "0644"`)
	}
	input := NetworkFile{Name: n.Name, Content: n.Content, Template: n.Template, State: n.State,
		RestoreUnlessControllerConfirmsWithin: n.RestoreUnlessControllerConfirmsWithin}
	if err := input.prepare(base, "path"); err != nil {
		return err
	}
	n.SourceBase, n.LocalPath, n.TemplateText = input.SourceBase, input.LocalPath, input.TemplateText
	return nil
}
func (n *IfupdownInterfacesFile) Arguments(arguments *ArgumentList) {
	state := n.State
	arguments.Public("path", &n.Name, parsed(func(path string) error { return ifupdownPathRule(path, state) }))
	if n.Content != nil {
		arguments.Private("content", *n.Content, scalar)
	}
}
func (n *IfupdownInterfacesFile) Reads() Reads {
	if n.Template == "" {
		return Reads{}
	}
	return Reads{Templates: map[string]string{"template": n.TemplateText}}
}
func (n *IfupdownInterfacesFile) ControllerFiles() []ControllerFile {
	if n.Template == "" {
		return nil
	}
	return []ControllerFile{{Field: "template", Kind: SourceTemplate, Path: &n.Template, Base: n.SourceBase,
		Variants: &n.Variants, Asset: &n.Asset, Template: &n.TemplateText, Local: &n.LocalPath}}
}
func (n *IfupdownInterfacesFile) file() File {
	file := File{Path: n.Name, State: n.State}
	if n.State != "absent" {
		file.Content, file.Template, file.TemplateText = n.Content, n.Template, n.TemplateText
		file.Asset, file.LocalPath = n.Asset, n.LocalPath
		file.Owner, file.Group, file.Mode = ifupdownRootUser, ifupdownRootGroup, n.Mode
	}
	return file
}

func (n *IfupdownInterfacesFile) Run(ctx context.Context, task NodeTask) protocol.Result {
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
	removal := n.State == "absent"
	var proposed string
	if !removal {
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
		compared.Reason = fmt.Sprintf("Network configuration file %s is unchanged; no activation or confirmation wait was needed.", file.Path)
		return compared
	}
	if err := ifupdownPrerequisites(ctx, run); err != nil {
		result := run.notYet(fmt.Errorf("Cannot change %s: %w", file.Path, err))
		if !run.Dryrun {
			result.Change = "unchanged"
		}
		return result
	}
	if err := ifupdownIsTheDebianPackage(ctx, run); err != nil {
		return refusal(err)
	}
	if !removal {
		output := run.RunCommand(ctx, []string{"ifquery", "-i", "/dev/stdin", "--list"}, proposed)
		if err := ifupdownNativeCheck(output, "ifquery", file.Path); err != nil {
			return refusal(err)
		}
	}
	data, err := planIfupdownChange(file.Path, proposed, removal)
	if err != nil {
		return refusal(err)
	}
	// A restore brings an interface up again only from this file's auto or
	// allow-hotplug line; without one, an interface the change takes down
	// would stay down.
	for _, name := range data.TakeDownWithPreviousFile {
		if slices.Contains(interfacesToBringUp(data.BringUpWithPreviousFileOnRestore), name) {
			continue
		}
		up, err := ifupdownListsAsUp(ctx, run, name)
		if err != nil {
			return refusal(fmt.Errorf("Cannot change %s: %w", file.Path, err))
		}
		if up {
			return refusal(fmt.Errorf("Cannot change %s: interface %s is up, but the file has no auto or allow-hotplug line for the interface, so a restore could not bring the interface up again; add an auto or allow-hotplug line next to the interface's iface stanza in the file.", file.Path, name))
		}
	}
	if len(data.BringUpWithProposedFile) > 0 {
		// --force: an interface that is already up would otherwise end the check
		// with "already configured". Standard output lists the commands with
		// their values and is discarded.
		arguments := []string{"ifup", "--no-act", "--force", "-i", "/dev/stdin"}
		for _, up := range data.BringUpWithProposedFile {
			arguments = append(arguments, up.Interface)
		}
		if err := ifupdownNativeCheck(run.RunCommand(ctx, arguments, proposed), "ifup", file.Path); err != nil {
			return refusal(err)
		}
	}
	if run.Dryrun {
		action := "replace"
		if compared.FileChange.Before == nil {
			action = "write"
		}
		compared.Reason = fmt.Sprintf("Would %s %s and activate it.", action, file.Path)
		if removal {
			compared.Reason = fmt.Sprintf("Would remove %s and its running configuration.", file.Path)
		}
		compared.Reason += " ifupdown's acceptance and reachability after activation were not checked."
		if len(data.TakeDownWithPreviousFile) > 0 {
			compared.Reason += " Would take down: " + strings.Join(data.TakeDownWithPreviousFile, ", ") + "."
		}
		if len(data.BringUpWithProposedFile) > 0 {
			var names []string
			for _, up := range data.BringUpWithProposedFile {
				names = append(names, up.Interface)
			}
			compared.Reason += " Would bring up: " + strings.Join(names, ", ") + "."
		}
		return compared
	}
	parent, err := run.openDefinitionDirectory(ctx, UnitManager{}, filepath.Dir(file.Path), nil, removal)
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
		return refusal(fmt.Errorf("Cannot change %s: the file changed while MMX read which interfaces the change takes down.", file.Path))
	}
	saved, err := json.Marshal(data)
	if err != nil {
		return refusal(err)
	}
	wait, _ := (NetworkFile{RestoreUnlessControllerConfirmsWithin: n.RestoreUnlessControllerConfirmsWithin}).confirmationWait()
	return task.OwedRestores.ApplyChange(ctx, ChangeWithOwedRestore{PreparedFile: prepared, RestoreConfirmationWait: wait, ModuleRestoreData: saved})
}

// ifupdownPrerequisites names what an earlier node task may still provide.
func ifupdownPrerequisites(ctx context.Context, run Runtime) error {
	for _, tool := range []string{"ifup", "ifdown", "ifquery", "systemctl"} {
		if !run.FindProgram(ctx, tool).Found {
			return missingProgramError(tool)
		}
	}
	output := run.RunCommand(ctx, []string{"systemctl", "is-enabled", "networking.service"}, "")
	if output.Error != "" {
		return commandFailure(output, "systemctl is-enabled networking.service")
	}
	if strings.TrimSpace(output.StandardOutput) != "enabled" {
		return fmt.Errorf("networking.service is not enabled, so ifupdown would not apply the network configuration file at boot; enable networking.service.")
	}
	return nil
}

// ifupdownIsTheDebianPackage refuses ifupdown2, which installs programs of
// the same names with another activation.
func ifupdownIsTheDebianPackage(ctx context.Context, run Runtime) error {
	output := run.RunCommand(ctx, []string{"ifup", "--version"}, "")
	if output.Error != "" {
		return commandFailure(output, "ifup --version")
	}
	line, _, _ := strings.Cut(strings.TrimSpace(output.StandardOutput), "\n")
	if !strings.HasPrefix(line, "ifup version ") {
		return fmt.Errorf("ifupdown_interfaces_file needs ifupdown, the Debian package; this node's ifup reports %q.", line)
	}
	return nil
}

// ifupdownNativeCheck turns a failed check of the proposed file, which the
// program read as /dev/stdin, into a refusal that names the destination.
func ifupdownNativeCheck(output CommandOutput, program, path string) error {
	if err := ifupdownStandardError(output, program); err != nil {
		return fmt.Errorf("ifupdown does not accept the proposed %s: %s.", path, strings.TrimSuffix(strings.ReplaceAll(err.Error(), "/dev/stdin", path), "."))
	}
	return nil
}

// ifupdownStandardError is a command's failure with the tool's standard
// error, or nil when it exited 0. Standard output can list option values.
func ifupdownStandardError(output CommandOutput, command string) error {
	if output.Error != "" {
		return fmt.Errorf("%s: %s", command, output.Error)
	}
	if output.ExitCode == 0 {
		return nil
	}
	if diagnostic := strings.TrimSpace(output.StandardError); diagnostic != "" {
		return errors.New(diagnostic)
	}
	return fmt.Errorf("%s exited with %d without a message", command, output.ExitCode)
}

// readIfupdownText reads a file in ifupdown's format; a missing file is
// empty.
func readIfupdownText(path string) (text string, exists bool, err error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return string(data), true, nil
}

// planIfupdownChange applies the refusals that need the existing file and
// finds the interfaces that the change takes down and brings up.
func planIfupdownChange(path, proposedText string, removal bool) (ifupdownRestoreData, error) {
	previousText, previousExists, err := readIfupdownText(path)
	if err != nil {
		return ifupdownRestoreData{}, fmt.Errorf("Cannot change %s: %w", path, err)
	}
	previous, proposed := readIfupdownFile(previousText), readIfupdownFile(proposedText)
	if previous.hasMapping || proposed.hasMapping {
		return ifupdownRestoreData{}, fmt.Errorf("Cannot change %s: the existing or proposed file uses a mapping stanza; MMX cannot know which interface to take down and bring up until the mapping script runs.", path)
	}
	if !reflect.DeepEqual(previous.fileLevelLines, proposed.fileLevelLines) {
		return ifupdownRestoreData{}, fmt.Errorf("Cannot change %s: the proposed file changes source, source-directory, or rename lines; MMX activates only interfaces declared in the file, so changes affecting interfaces in included files or renamed interfaces would take effect only at the next boot; leave the source, source-directory, and rename lines unchanged.", path)
	}
	if !removal && path != ifupdownMainFile {
		mainText, _, err := readIfupdownText(ifupdownMainFile)
		if err != nil {
			return ifupdownRestoreData{}, fmt.Errorf("Cannot change %s: %w", path, err)
		}
		if !readIfupdownFile(mainText).reads(path) {
			return ifupdownRestoreData{}, fmt.Errorf("Cannot change %s: the main ifupdown file %s does not include the proposed file; add an include line such as `source %s/*` to the main ifupdown file.", path, ifupdownMainFile, ifupdownDirectory)
		}
	}
	data := ifupdownInterfacesThatChange(previous, proposed)
	for _, name := range data.names() {
		if !ifupdownInterfaceName(name) {
			return ifupdownRestoreData{}, fmt.Errorf("Cannot change %s: interface name %q cannot be taken down and brought up by MMX; interface names must be at most 15 bytes, contain no spaces, slash, or equals sign, and must not start with a hyphen.", path, name)
		}
	}
	data.previousExisted = previousExists
	if previousExists {
		digest := sha256.Sum256([]byte(previousText))
		data.previousContentsSHA256 = hex.EncodeToString(digest[:])
	}
	return data, nil
}

var ifupdownInterfacesFileDescription = Description{
	Name: "ifupdown_interfaces_file", Topic: TopicNetworkConfiguration, Schema: reflect.TypeOf(IfupdownInterfacesFile{}),
	Summary: "Manages /etc/network/interfaces or one file in /etc/network/interfaces.d for ifupdown, the Debian package, and takes the changed interfaces down and up. The node restores an unconfirmed change by itself.",
	Fields: []Field{
		{Name: "path", Required: true, Notes: "/etc/network/interfaces, or /etc/network/interfaces.d/<name> where the name has only letters, digits, underscores, and hyphens. The file belongs to root:root."},
		{Name: "permissions_mode", Notes: `Quoted octal, such as "0600". Omit to keep an existing file's mode; a new file gets 0644. Ignored with state: absent.`},
		{Name: "content", Notes: "The complete file text in ifupdown's format; exactly one of content and template is required for a present file."},
		{Name: "template", Notes: "A Go template on the controller machine, relative to the project or role directory."},
		{Name: "state", Values: presenceStates, Default: "present", Notes: "Absent takes the file's interfaces down, with the virtual devices they created, and removes the file. /etc/network/interfaces cannot be removed."},
		{Name: "restore_unless_controller_confirms_within", Default: "2m", Notes: "How long the node waits after the change for the controller process to confirm reachability before restoring the previous file. Activation uses part of this time. The wait before the change has the same limit."},
	},
	Example:   "- ifupdown_interfaces_file:\n    path: /etc/network/interfaces.d/60-second\n    content: |\n      auto enp6s0\n      iface enp6s0 inet static\n          address 10.0.0.5/24\n",
	Results:   []string{"status", "change", "reason"},
	Dryrun:    "Compares the file and checks the proposed file with ifquery and ifup --no-act, which read it from standard input, without writing or activating anything. Lists the interfaces it would take down and bring up. Missing prerequisites are not checked.",
	WaitLimit: "restore_unless_controller_confirms_within bounds the change",
}
