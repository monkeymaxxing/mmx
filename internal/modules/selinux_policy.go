package modules

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"mmx/internal/facts"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

// The SELinux modules change the running node and the stored policy
// together, through semanage, setsebool, and restorecon, and read the mode
// from the kernel and /etc/selinux/config. Tests point these paths elsewhere.
var (
	selinuxEnforceFile = "/sys/fs/selinux/enforce"
	selinuxContextFile = "/proc/self/attr/current"
	selinuxConfigFile  = "/etc/selinux/config"
	selinuxAutorelabel = "/.autorelabel"
	kernelCommandLine  = "/proc/cmdline"
)

// The kernel's enforce file holds 1 while SELinux enforces and 0 while it only
// reports.
var (
	enforceValue = map[string]string{"enforcing": "1", "permissive": "0"}
	enforceMode  = map[string]string{"1": "enforcing", "0": "permissive"}
)

// selinuxNotEnabled is the result of an SELinux module on a node where the
// kernel runs without SELinux or SELinux is not installed.
func selinuxNotEnabled() protocol.Result {
	return protocol.Result{Status: "skipped", Change: "unchanged", Reason: "SELinux is not enabled on this node"}
}

// selinuxRunning reports whether the kernel runs SELinux with a loaded
// policy. When the node's files cannot be read, it reports true, so that the
// SELinux programs' own errors reach the result.
func selinuxRunning() bool {
	runs, err := facts.SELinuxRuns(os.ReadFile, selinuxEnforceFile, selinuxContextFile)
	return runs || err != nil
}

// selinuxTools reports the first missing program, with the package that
// provides it.
func (run Runtime) selinuxTools(ctx context.Context, names ...string) *protocol.Result {
	for _, name := range names {
		if _, err := lookPath(ctx, name); err != nil {
			r := run.notYet(missingProgramError(name))
			return &r
		}
	}
	return nil
}

// semanage runs semanage and returns its standard output.
func semanage(ctx context.Context, args ...string) (string, error) {
	r := Command(ctx, nil, "", "semanage", args...)
	if r.Status != "succeeded" {
		return "", fmt.Errorf("semanage %s: %s: %s%s", strings.Join(args, " "), r.Reason, r.Stderr, r.Stdout)
	}
	return r.Stdout, nil
}

// selinuxFailed is a failure that keeps what the task changed before it.
func selinuxFailed(err error, changed bool) protocol.Result {
	r := protocol.Failed(err)
	if changed {
		r.Change = "changed"
	}
	return r
}

// semanageBoolean is one line of semanage boolean --list: the running value
// and the value stored in the policy.
var semanageBoolean = regexp.MustCompile(`^(\S+)\s+\(\s*(on|off)\s*,\s*(on|off)\s*\)`)

func readSELinuxBoolean(ctx context.Context, name string) (running, stored string, err error) {
	out, err := semanage(ctx, "boolean", "--list", "--noheading")
	if err != nil {
		return "", "", err
	}
	for _, line := range strings.Split(out, "\n") {
		if m := semanageBoolean.FindStringSubmatch(line); m != nil && m[1] == name {
			return m[2], m[3], nil
		}
	}
	return "", "", nil
}

// SELinuxBoolean sets a boolean now and in the stored policy when either
// differs.
func (run Runtime) SELinuxBoolean(ctx context.Context, b SELinuxBoolean) protocol.Result {
	if err := ValidateSELinuxBoolean(b); err != nil {
		return protocol.ConfigurationError(err)
	}
	if !selinuxRunning() {
		return selinuxNotEnabled()
	}
	if missing := run.selinuxTools(ctx, "semanage", "setsebool"); missing != nil {
		return *missing
	}
	want := map[bool]string{true: "on", false: "off"}[*b.Enabled]
	running, stored, err := readSELinuxBoolean(ctx, b.Name)
	if err != nil {
		return protocol.Failed(err)
	}
	if running == "" {
		// A policy package an earlier task installs may define it.
		return run.notYet(fmt.Errorf("the SELinux policy on this node has no boolean %s", b.Name))
	}
	if running == want && stored == want {
		return protocol.Success("unchanged")
	}
	if run.Dryrun {
		return protocol.Success("would_change")
	}
	if err := run.Mutation("selinux_boolean"); err != nil {
		return protocol.Failed(err)
	}
	if r := Command(ctx, nil, "", "setsebool", "-P", b.Name, want); r.Status != "succeeded" {
		return protocol.Failed(fmt.Errorf("setsebool -P %s %s: %s: %s%s", b.Name, want, r.Reason, r.Stderr, r.Stdout))
	}
	running, stored, err = readSELinuxBoolean(ctx, b.Name)
	if err == nil && (running != want || stored != want) {
		err = fmt.Errorf("SELinux boolean %s is %s now and %s in the stored policy after setsebool, not %s", b.Name, running, stored, want)
	}
	if err != nil {
		return selinuxFailed(err, true)
	}
	return protocol.Success("changed")
}

// semanageRecord is one line of semanage port or fcontext --list: the first
// field (a protocol's ports, or a path pattern), and the SELinux type.
type semanageRecord struct{ key, kind, selinuxType string }

var (
	semanagePortLine     = regexp.MustCompile(`^(\S+)\s+(tcp|udp|sctp|dccp)\s+(.+?)\s*$`)
	semanageFcontextLine = regexp.MustCompile(`^(\S+)\s+(all files|regular file|directory|character device|block device|socket|symbolic link|named pipe)\s+(\S+)\s*$`)
)

// readSemanage lists the local definitions, which this node added or
// changed, and all definitions, the base policy's included.
func readSemanage(ctx context.Context, object string) (local, all []semanageRecord, err error) {
	read := func(args ...string) ([]semanageRecord, error) {
		out, err := semanage(ctx, append([]string{object, "--list", "--noheading"}, args...)...)
		if err != nil {
			return nil, err
		}
		var records []semanageRecord
		for _, line := range strings.Split(out, "\n") {
			if object == "port" {
				// http_port_t   tcp   80, 81, 8100-8102
				if m := semanagePortLine.FindStringSubmatch(line); m != nil {
					for _, ports := range strings.Split(m[3], ",") {
						records = append(records, semanageRecord{strings.TrimSpace(ports), m[2], m[1]})
					}
				}
				continue
			}
			// /var/www(/.*)?   all files   system_u:object_r:httpd_sys_content_t:s0
			if m := semanageFcontextLine.FindStringSubmatch(line); m != nil {
				context := strings.Split(m[3], ":")
				if len(context) >= 3 {
					records = append(records, semanageRecord{m[1], m[2], context[2]})
				}
			}
		}
		return records, nil
	}
	if local, err = read("--locallist"); err != nil {
		return nil, nil, err
	}
	all, err = read()
	return local, all, err
}

// semanageOperation decides how semanage brings key of kind to selinuxType:
// "" when it has that type already, --modify when a definition exists, local
// or from the base policy, and --add otherwise. For absent, --delete removes
// the local definition; the base policy's cannot be removed.
func semanageOperation(local, all []semanageRecord, key, kind, selinuxType string, absent bool) string {
	match := func(records []semanageRecord) []string {
		var types []string
		for _, r := range records {
			if r.key == key && r.kind == kind {
				types = append(types, r.selinuxType)
			}
		}
		return types
	}
	localTypes, allTypes := match(local), match(all)
	switch {
	case absent && len(localTypes) > 0:
		return "--delete"
	case absent:
		return ""
	case len(localTypes) > 0 && localTypes[0] == selinuxType:
		return ""
	case len(localTypes) > 0:
		return "--modify"
	case slices.Contains(allTypes, selinuxType):
		return ""
	case len(allTypes) > 0:
		return "--modify"
	}
	return "--add"
}

// SELinuxPort adds the local definition of a port, or changes it when the
// port has another type, including one from the base policy.
func (run Runtime) SELinuxPort(ctx context.Context, p SELinuxPort) protocol.Result {
	if err := ValidateSELinuxPort(p); err != nil {
		return protocol.ConfigurationError(err)
	}
	if !selinuxRunning() {
		return selinuxNotEnabled()
	}
	if missing := run.selinuxTools(ctx, "semanage"); missing != nil {
		return *missing
	}
	port := dashedPort(p.Port) // as semanage lists it
	plan := func() (string, error) {
		local, all, err := readSemanage(ctx, "port")
		if err != nil {
			return "", err
		}
		return semanageOperation(local, all, port, p.Proto, p.SELinuxType, p.State == "absent"), nil
	}
	operation, err := plan()
	if err != nil {
		return protocol.Failed(err)
	}
	if operation == "" {
		return protocol.Success("unchanged")
	}
	if run.Dryrun {
		return protocol.Success("would_change")
	}
	if err := run.Mutation("selinux_port"); err != nil {
		return protocol.Failed(err)
	}
	args := []string{"port", operation, "--proto", p.Proto, port}
	if p.State != "absent" {
		args = []string{"port", operation, "--type", p.SELinuxType, "--proto", p.Proto, port}
	}
	if _, err := semanage(ctx, args...); err != nil {
		return protocol.Failed(err)
	}
	if remaining, err := plan(); err != nil || remaining != "" {
		if err == nil {
			err = fmt.Errorf("SELinux port %s/%s did not reach the requested state after semanage %s", port, p.Proto, operation)
		}
		return selinuxFailed(err, true)
	}
	return protocol.Success("changed")
}

// selinuxPathPattern is the semanage file context pattern for a path, or
// with everything inside for it and everything below it.
func selinuxPathPattern(path string, everythingInside bool) string {
	pattern := regexp.QuoteMeta(path)
	if everythingInside {
		pattern += "(/.*)?"
	}
	return pattern
}

// pathsToRelabel counts the existing paths that carry another label than the
// policy gives them. restorecon prints one line for each.
func pathsToRelabel(ctx context.Context, path string, everythingInside bool) (int, error) {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	args := []string{"-n", "-v", path}
	if everythingInside {
		args = []string{"-n", "-v", "-R", path}
	}
	lines := &lineCounter{}
	var stderr capBuffer
	c := exec.CommandContext(ctx, "restorecon", args...)
	c.Stdout, c.Stderr = lines, &stderr
	if err := c.Run(); err != nil {
		return 0, fmt.Errorf("restorecon %s: %w: %s", strings.Join(args, " "), err, stderr.Text())
	}
	return lines.count, nil
}

// lineCounter counts the newlines written to it, so a large tree's list of
// paths is never held in memory.
type lineCounter struct{ count int }

func (c *lineCounter) Write(p []byte) (int, error) {
	c.count += bytes.Count(p, []byte("\n"))
	return len(p), nil
}

// SELinuxFileContext owns the node's local label rule for a path, or for a
// path and everything below it, and relabels on every run when an existing
// path carries another label than the policy gives it.
func (run Runtime) SELinuxFileContext(ctx context.Context, c SELinuxFileContext) (result protocol.Result) {
	if err := ValidateSELinuxFileContext(c); err != nil {
		return protocol.ConfigurationError(err)
	}
	if !selinuxRunning() {
		return selinuxNotEnabled()
	}
	if missing := run.selinuxTools(ctx, "semanage", "restorecon"); missing != nil {
		return *missing
	}
	pattern := selinuxPathPattern(c.Path, *c.Recurse)
	plan := func() (string, error) {
		local, all, err := readSemanage(ctx, "fcontext")
		if err != nil {
			return "", err
		}
		return semanageOperation(local, all, pattern, "all files", c.SELinuxType, c.State == "absent"), nil
	}
	operation, err := plan()
	if err != nil {
		return protocol.Failed(err)
	}
	changes := map[string]any{}
	if operation != "" {
		changes["rule"] = map[string]string{"--add": "added", "--modify": "updated", "--delete": "removed"}[operation]
	}
	// changes is what the task changes or, in a dryrun, would change; done
	// is what a run did, so that a failed task never reports a change that
	// did not happen.
	done := map[string]any{}
	defer func() {
		switch {
		case run.Dryrun && len(changes) > 0:
			result.Value = changes
		case !run.Dryrun && len(done) > 0:
			result.Value = done
		}
	}()
	if run.Dryrun && operation != "" {
		r := protocol.Success("would_change")
		r.Reason = "the paths to relabel are not checked until the rule is in the policy"
		return r
	}
	if operation != "" {
		if err := run.Mutation("selinux_file_context"); err != nil {
			return protocol.Failed(err)
		}
		args := []string{"fcontext", operation, pattern}
		if c.State != "absent" {
			args = []string{"fcontext", operation, "--type", c.SELinuxType, pattern}
		}
		if _, err := semanage(ctx, args...); err != nil {
			return protocol.Failed(err)
		}
		done["rule"] = changes["rule"]
		if remaining, err := plan(); err != nil || remaining != "" {
			if err == nil {
				err = fmt.Errorf("the SELinux file context rule for %s did not reach the requested state after semanage %s", pattern, operation)
			}
			return selinuxFailed(err, true)
		}
	}
	count, err := pathsToRelabel(ctx, c.Path, *c.Recurse)
	if err != nil {
		return selinuxFailed(err, operation != "")
	}
	if count > 0 {
		changes["paths_relabeled"] = count
	}
	switch {
	case count == 0 && operation == "":
		return protocol.Success("unchanged")
	case count == 0:
		return protocol.Success("changed")
	case run.Dryrun:
		return protocol.Success("would_change")
	}
	if err := run.Mutation("selinux_file_context"); err != nil {
		return protocol.Failed(err)
	}
	args := []string{c.Path}
	if *c.Recurse {
		args = []string{"-R", c.Path}
	}
	if r := Command(ctx, nil, "", "restorecon", args...); r.Status != "succeeded" {
		return selinuxFailed(fmt.Errorf("restorecon %s: %s: %s", strings.Join(args, " "), r.Reason, r.Stderr), true)
	}
	if remaining, err := pathsToRelabel(ctx, c.Path, *c.Recurse); err != nil || remaining > 0 {
		if err == nil {
			err = fmt.Errorf("%d paths below %s still carry another label than the policy gives them after restorecon", remaining, c.Path)
		}
		return selinuxFailed(err, true)
	}
	done["paths_relabeled"] = count
	return protocol.Success("changed")
}

// storedSELinuxMode is the SELINUX= value of /etc/selinux/config.
func storedSELinuxMode() (string, error) {
	b, err := os.ReadFile(selinuxConfigFile)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "SELINUX="); ok {
			return strings.Trim(strings.TrimSpace(v), `"`), nil
		}
	}
	return "", nil
}

// SELinuxMode sets the running mode and the SELINUX= line of
// /etc/selinux/config. On a kernel that runs without SELinux it writes the
// configuration, creates /.autorelabel so the next boot labels every file,
// and reports that the node needs a reboot.
func (run Runtime) SELinuxMode(ctx context.Context, m SELinuxMode) (result protocol.Result) {
	if err := ValidateSELinuxMode(m); err != nil {
		return protocol.ConfigurationError(err)
	}
	kernel := selinuxRunning()
	if _, err := os.Stat(selinuxConfigFile); !kernel && errors.Is(err, os.ErrNotExist) {
		return selinuxNotEnabled()
	}
	stored, err := storedSELinuxMode()
	if err != nil {
		return protocol.Failed(err)
	}
	running := ""
	if kernel {
		b, err := os.ReadFile(selinuxEnforceFile)
		if err != nil {
			return protocol.Failed(err)
		}
		running = enforceMode[strings.TrimSpace(string(b))]
	} else {
		cmdline, err := os.ReadFile(kernelCommandLine)
		if err != nil {
			return protocol.Failed(err)
		}
		if slices.Contains(strings.Fields(string(cmdline)), "selinux=0") {
			return protocol.ConfigurationError(fmt.Errorf("the kernel command line has selinux=0, which keeps SELinux off at every boot whatever /etc/selinux/config says; remove it first, for example with grubby --update-kernel ALL --remove-args selinux"))
		}
	}
	_, statErr := os.Stat(selinuxAutorelabel)
	relabelPlanned := statErr == nil
	if !kernel {
		defer func() {
			if result.Status != "failed" {
				result.Value = map[string]any{"reboot_required": true}
			}
		}()
	}
	if stored == m.Mode && (running == m.Mode || (!kernel && relabelPlanned)) {
		return protocol.Success("unchanged")
	}
	if run.Dryrun {
		return protocol.Success("would_change")
	}
	changed := false
	if stored != m.Mode {
		line := Line{InsertAtEndOfFile: true, Path: selinuxConfigFile, Match: `^\s*SELINUX=`, Text: "SELINUX=" + m.Mode}
		if r := run.Line(ctx, line, value.Scope{}); r.Status != "succeeded" {
			return selinuxFailed(fmt.Errorf("write SELINUX=%s in %s: %s", m.Mode, selinuxConfigFile, r.Reason), false)
		}
		changed = true
	}
	if !kernel && !relabelPlanned {
		empty := ""
		if r := run.File(ctx, File{Path: selinuxAutorelabel, Literal: &empty}, value.Scope{}); r.Status != "succeeded" {
			return selinuxFailed(fmt.Errorf("create %s: %s", selinuxAutorelabel, r.Reason), changed)
		}
	}
	if kernel && running != m.Mode {
		if err := run.Mutation("selinux_mode"); err != nil {
			return selinuxFailed(err, changed)
		}
		if err := os.WriteFile(selinuxEnforceFile, []byte(enforceValue[m.Mode]), 0); err != nil {
			return selinuxFailed(fmt.Errorf("set the running SELinux mode to %s: %w", m.Mode, err), changed)
		}
		b, err := os.ReadFile(selinuxEnforceFile)
		if err == nil && strings.TrimSpace(string(b)) != enforceValue[m.Mode] {
			err = fmt.Errorf("SELinux still runs %s after the change to %s", enforceMode[strings.TrimSpace(string(b))], m.Mode)
		}
		if err != nil {
			return selinuxFailed(err, true)
		}
	}
	return protocol.Success("changed")
}

// SELinuxBoolean sets one SELinux boolean, now and in the stored policy.
type SELinuxBoolean struct {
	Name    string `yaml:"name" json:"name"`
	Enabled *bool  `yaml:"enabled" json:"enabled"`
}

// SELinuxFileContext owns the node's local label rule for Path, or with
// Recurse for Path and everything below it.
type SELinuxFileContext struct {
	Path        string `yaml:"path" json:"path"`
	Recurse     *bool  `yaml:"also_apply_to_everything_inside" json:"also_apply_to_everything_inside"`
	SELinuxType string `yaml:"selinux_type,omitempty" json:"selinux_type,omitempty"`
	State       string `yaml:"state,omitempty" json:"state,omitempty"`
}

// SELinuxPort gives a network port, or a range, an SELinux type.
type SELinuxPort struct {
	Port        PortOrRange `yaml:"port" json:"port"`
	Proto       string      `yaml:"protocol" json:"protocol"`
	SELinuxType string      `yaml:"selinux_type,omitempty" json:"selinux_type,omitempty"`
	State       string      `yaml:"state,omitempty" json:"state,omitempty"`
}

// SELinuxMode is whether SELinux enforces its policy or only reports.
type SELinuxMode struct {
	Mode string `yaml:"mode" json:"mode"`
}

// selinuxName is an SELinux boolean or type name.
var selinuxName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func ValidateSELinuxBoolean(b SELinuxBoolean) error {
	if b.Enabled == nil {
		return invalid("enabled", "selinux_boolean.enabled is required: true or false")
	}
	return nil
}

func ValidateSELinuxFileContext(c SELinuxFileContext) error {
	if err := validatePresence(c.State); err != nil {
		return err
	}
	if c.Recurse == nil {
		return invalid("also_apply_to_everything_inside", "selinux_file_context.also_apply_to_everything_inside is required: true for path and everything below it, false for path alone")
	}
	if c.State == "absent" {
		if c.SELinuxType != "" {
			return fmt.Errorf("absent selinux_file_context accepts only path, also_apply_to_everything_inside, and state")
		}
	} else if c.SELinuxType == "" {
		return invalid("selinux_type", "selinux_file_context.selinux_type is required, such as httpd_sys_content_t")
	}
	return nil
}

func ValidateSELinuxPort(p SELinuxPort) error {
	if err := validatePresence(p.State); err != nil {
		return err
	}
	if !slices.Contains(selinuxPortProtocols, p.Proto) {
		return invalid("protocol", "selinux_port.protocol must be tcp or udp")
	}
	if p.State == "absent" {
		if p.SELinuxType != "" {
			return fmt.Errorf("absent selinux_port accepts only port, protocol, and state")
		}
	} else if p.SELinuxType == "" {
		return invalid("selinux_type", "selinux_port.selinux_type is required, such as http_port_t")
	}
	return nil
}

func ValidateSELinuxMode(m SELinuxMode) error {
	if !slices.Contains(selinuxModes, m.Mode) {
		return invalid("mode", "selinux_mode.mode must be enforcing or permissive; MMX does not disable SELinux")
	}
	return nil
}

// selinuxBooleanDescription is what mmx describe-module shows for selinux_boolean.
var selinuxBooleanDescription = Description{
	Name: "selinux_boolean", Topic: TopicSELinuxAccessControlListsAndCapabilities, Schema: reflect.TypeOf(SELinuxBoolean{}),
	Summary: "one SELinux boolean, now and in the stored policy; skipped where SELinux is not enabled",
	Fields: []Field{
		{Name: "name", Required: true, Notes: "such as httpd_can_network_connect"},
		{Name: "enabled", Required: true, Notes: "true or false; set now and in the stored policy when either differs"},
	},
	Example: "- selinux_boolean:\n    name: httpd_can_network_connect\n    enabled: true\n",
	Results: resultBasic,
	Dryrun:  "supported; reads the running and stored value; needs semanage from policycoreutils-python-utils",
}

// selinuxFileContextDescription is what mmx describe-module shows for selinux_file_context.
var selinuxFileContextDescription = Description{
	Name: "selinux_file_context", Topic: TopicSELinuxAccessControlListsAndCapabilities, Schema: reflect.TypeOf(SELinuxFileContext{}),
	Summary: "the node's local SELinux label rule for a path, and the labels of the files there; skipped where SELinux is not enabled",
	Fields: []Field{
		{Name: "path", Required: true, Notes: "absolute; a missing path is not an error"},
		{Name: "also_apply_to_everything_inside", Required: true, Notes: "true: the rule covers path and everything below it; false: path alone"},
		{Name: "selinux_type", Required: true, Notes: "the SELinux type, such as httpd_sys_content_t; omit with absent"},
		{Name: "state", Values: presenceStates, Default: "present", Notes: "absent removes the rule and accepts only path and also_apply_to_everything_inside"},
	},
	Example: "- selinux_file_context:\n    path: /srv/www\n    also_apply_to_everything_inside: true\n    selinux_type: httpd_sys_content_t\n",
	Results: []string{"status", "change", "value.rule", "value.paths_relabeled"},
	Dryrun:  "supported; reports the rule change, and the number of paths to relabel once the rule is in the policy; every run relabels the existing paths whose label differs from the policy's",
}

// selinuxPortDescription is what mmx describe-module shows for selinux_port.
var selinuxPortDescription = Description{
	Name: "selinux_port", Topic: TopicSELinuxAccessControlListsAndCapabilities, Schema: reflect.TypeOf(SELinuxPort{}),
	Summary: "the SELinux type of a network port or range; skipped where SELinux is not enabled",
	Fields: []Field{
		{Name: "port", Required: true, Notes: "a number or first:last"},
		{Name: "protocol", Values: selinuxPortProtocols, Required: true},
		{Name: "selinux_type", Required: true, Notes: "such as http_port_t; adds the local definition, or changes the type the port has, also one from the base policy; omit with absent"},
		{Name: "state", Values: presenceStates, Default: "present", Notes: "absent removes the local definition and accepts only port and protocol; the base policy's own definition stays"},
	},
	Example: "- selinux_port:\n    port: 8443\n    protocol: tcp\n    selinux_type: http_port_t\n",
	Results: resultBasic,
	Dryrun:  "supported; reads the local and base policy definitions",
}

// selinuxModeDescription is what mmx describe-module shows for selinux_mode.
var selinuxModeDescription = Description{
	Name: "selinux_mode", Topic: TopicSELinuxAccessControlListsAndCapabilities, Schema: reflect.TypeOf(SELinuxMode{}),
	Summary: "whether SELinux enforces its policy or only reports, now and in /etc/selinux/config; skipped where SELinux is not installed",
	Fields: []Field{
		{Name: "mode", Values: selinuxModes, Required: true, Notes: "sets the running mode and the SELINUX= line; on a kernel that runs without SELinux, writes the line, creates /.autorelabel, and reports value.reboot_required: true"},
	},
	Example: "- selinux_mode:\n    mode: enforcing\n",
	Results: []string{"status", "change", "value.reboot_required"},
	Dryrun:  "supported; reads the running mode and the configuration",
}

// Prepare checks the inputs when the project loads.
func (x *SELinuxBoolean) Prepare(base string) error {
	return ValidateSELinuxBoolean(*x)
}

// Prepare checks the inputs when the project loads.
func (x *SELinuxFileContext) Prepare(base string) error {
	return ValidateSELinuxFileContext(*x)
}

// Prepare checks the inputs when the project loads.
func (x *SELinuxPort) Prepare(base string) error {
	return ValidateSELinuxPort(*x)
}

// Prepare checks the inputs when the project loads.
func (x *SELinuxMode) Prepare(base string) error {
	return ValidateSELinuxMode(*x)
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (b *SELinuxBoolean) Arguments(arguments *ArgumentList) {
	arguments.Public("name", &b.Name, selinuxNameRule("name"))
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (c *SELinuxFileContext) Arguments(arguments *ArgumentList) {
	arguments.Public("path", &c.Path, absolute)
	if c.SELinuxType != "" {
		arguments.Public("selinux_type", &c.SELinuxType, selinuxNameRule("selinux_type"))
	}
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (p *SELinuxPort) Arguments(arguments *ArgumentList) {
	arguments.Public("port", (*string)(&p.Port), ufwPortRule(true))
	if p.SELinuxType != "" {
		arguments.Public("selinux_type", &p.SELinuxType, selinuxNameRule("selinux_type"))
	}
}

// CleanPaths cleans the resolved path.
func (c *SELinuxFileContext) CleanPaths() {
	c.Path = filepath.Clean(c.Path)
}

// Run does the module's work on the node.
func (x *SELinuxBoolean) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.Runtime.SELinuxBoolean(ctx, *x)
}

// Run does the module's work on the node.
func (x *SELinuxFileContext) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.Runtime.SELinuxFileContext(ctx, *x)
}

// Run does the module's work on the node.
func (x *SELinuxPort) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.Runtime.SELinuxPort(ctx, *x)
}

// Run does the module's work on the node.
func (x *SELinuxMode) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.Runtime.SELinuxMode(ctx, *x)
}
