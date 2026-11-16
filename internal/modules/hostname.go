package modules

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"

	"mmx/internal/protocol"
)

// runningHostnameFile is where the kernel keeps the running host name. Tests
// point it elsewhere.
var runningHostnameFile = "/proc/sys/kernel/hostname"

// Hostname sets the node's host name, stored and running, through hostnamectl.
// It never edits /etc/hosts.
func (run Runtime) Hostname(ctx context.Context, h Hostname) (result protocol.Result) {
	if err := ValidateHostname(h); err != nil {
		return protocol.ConfigurationError(err)
	}
	if _, err := lookPath(ctx, "hostnamectl"); err != nil {
		return run.notYet(missingProgramError("hostnamectl"))
	}
	stored, running, err := run.readHostname(ctx)
	if err != nil {
		return protocol.Failed(err)
	}
	if stored == h.Name && running == h.Name {
		return protocol.Success("unchanged")
	}
	before := stored
	if before == "" {
		before = running
	}
	defer func() {
		if result.Change == "changed" || result.Change == "would_change" {
			result.Value = map[string]any{"hostname_before": before, "hostname_after": h.Name}
		}
	}()
	if run.Dryrun {
		return protocol.Success("would_change")
	}
	set := run.RunCommandThatChangesTheNode(ctx, []string{"hostnamectl", "set-hostname", h.Name}, "")
	if err := commandFailure(set, "hostnamectl set-hostname "+h.Name); err != nil {
		return protocol.Failed(err)
	}
	stored, running, err = run.readHostname(ctx)
	if err == nil && (stored != h.Name || running != h.Name) {
		err = fmt.Errorf("after hostnamectl set-hostname, the stored host name is %q and the running host name is %q; the requested host name is %q", stored, running, h.Name)
	}
	if err != nil {
		r := protocol.Failed(err)
		r.Change = "changed"
		return r
	}
	return protocol.Success("changed")
}

// readHostname reads the stored host name, which hostnamectl --static prints
// from /etc/hostname, and the running one the kernel holds.
func (run Runtime) readHostname(ctx context.Context) (stored, running string, err error) {
	out := run.RunCommand(ctx, []string{"hostnamectl", "--static"}, "")
	if err := commandFailure(out, "hostnamectl --static"); err != nil {
		return "", "", err
	}
	kernel := run.ReadFile(runningHostnameFile)
	if kernel.Error != "" {
		return "", "", fmt.Errorf("read the running host name from %s: %s", runningHostnameFile, kernel.Error)
	}
	return strings.TrimSpace(out.StandardOutput), strings.TrimSpace(kernel.Content), nil
}

// commandFailure is a primitive command's failure, with what the program
// printed, or nil when it ran and exited 0.
func commandFailure(out CommandOutput, command string) error {
	switch {
	case out.Error != "":
		return fmt.Errorf("%s: %s", command, out.Error)
	case out.ExitCode != 0:
		return fmt.Errorf("%s exited with %d: %s", command, out.ExitCode, strings.TrimSpace(out.StandardError+" "+out.StandardOutput))
	}
	return nil
}

// Hostname is the node's host name, a short or a fully qualified one.
type Hostname struct {
	Name string `yaml:"name" json:"name"`
}

// hostnameName is a host name as hostnamectl stores it: labels of letters,
// digits, and hyphens, separated by dots, at most 64 characters, which is the
// most the kernel holds.
var hostnameName = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$`)

var hostnameRule = parsed(func(s string) error {
	if len(s) > 64 || !hostnameName.MatchString(s) {
		return errors.New("hostname.name must be a host name of at most 64 characters: letters, digits, and hyphens, in parts separated by dots, such as web1 or web1.example.com")
	}
	return nil
})

func ValidateHostname(h Hostname) error {
	if h.Name == "" {
		return invalid("name", "hostname.name is required, such as web1 or web1.example.com")
	}
	return nil
}

// hostnameDescription is what mmx describe-module shows for hostname.
var hostnameDescription = Description{
	Name: "hostname", Topic: TopicHostNameTimeZoneLocaleAndAlternatives, Schema: reflect.TypeOf(Hostname{}),
	Summary: "the node's host name, stored and running, set through hostnamectl; /etc/hosts is not changed",
	Fields: []Field{
		{Name: "name", Required: true, Notes: "a short name or a fully qualified one, at most 64 characters; own /etc/hosts with a file task"},
	},
	Example: "- hostname:\n    name: \"{{ .node.name }}.example.com\"\n",
	Results: []string{"status", "change", "value.hostname_before", "value.hostname_after"},
	Dryrun:  "supported; reads the stored and the running host name and reports the name that would be set",
}

// Prepare checks the inputs when the project loads.
func (h *Hostname) Prepare(base string) error {
	return ValidateHostname(*h)
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (h *Hostname) Arguments(arguments *ArgumentList) {
	arguments.Public("name", &h.Name, hostnameRule)
}

// Run does the module's work on the node.
func (h *Hostname) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.Runtime.Hostname(ctx, *h)
}
