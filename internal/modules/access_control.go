package modules

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"mmx/internal/protocol"
)

// aclEntry is a named entry of an access control list: a user account or a
// group, by its numeric ID as the file system stores it.
type aclEntry struct {
	group bool
	id    uint32
}

// spec is the entry as setfacl reads it, without permissions.
func (e aclEntry) spec() string {
	if e.group {
		return fmt.Sprintf("g:%d", e.id)
	}
	return fmt.Sprintf("u:%d", e.id)
}

// name is the entry as results show it: the task's field and the name.
func (e aclEntry) name() string {
	id := strconv.FormatUint(uint64(e.id), 10)
	if e.group {
		if g, err := user.LookupGroupId(id); err == nil {
			id = g.Name
		}
		return "group " + id
	}
	if u, err := user.LookupId(id); err == nil {
		id = u.Username
	}
	return "user_account " + id
}

const (
	aclAccess  = "system.posix_acl_access"
	aclDefault = "system.posix_acl_default"
	// Tags and permission bits of the kernel's extended attribute format
	// (posix_acl_xattr.h): a version 2 header, then 8-byte entries.
	aclUser    = 0x02
	aclGroup   = 0x08
	aclVersion = 2
)

// readACL returns the named entries of path's access or default list, as
// getfacl would print them, from the file system's own record of them.
func readACL(path, attribute string) (map[aclEntry]string, error) {
	size, err := unix.Lgetxattr(path, attribute, nil)
	if errors.Is(err, unix.ENODATA) {
		return map[aclEntry]string{}, nil
	}
	if errors.Is(err, unix.EOPNOTSUPP) {
		return nil, fmt.Errorf("the file system of %s does not support access control lists", path)
	}
	if err != nil {
		return nil, fmt.Errorf("read the access control list of %s: %w", path, err)
	}
	b := make([]byte, size)
	if size, err = unix.Lgetxattr(path, attribute, b); err != nil {
		return nil, fmt.Errorf("read the access control list of %s: %w", path, err)
	}
	return parseACL(b[:size], path)
}

func parseACL(b []byte, path string) (map[aclEntry]string, error) {
	if len(b) < 4 || len(b)%8 != 4 || binary.LittleEndian.Uint32(b) != aclVersion {
		return nil, fmt.Errorf("the access control list of %s has a format MMX does not read", path)
	}
	named := map[aclEntry]string{}
	for b = b[4:]; len(b) > 0; b = b[8:] {
		tag, bits, id := binary.LittleEndian.Uint16(b), binary.LittleEndian.Uint16(b[2:]), binary.LittleEndian.Uint32(b[4:])
		if tag != aclUser && tag != aclGroup {
			continue
		}
		permissions := []byte("---")
		for i, letter := range "rwx" {
			if bits&(4>>i) != 0 {
				permissions[i] = byte(letter)
			}
		}
		named[aclEntry{tag == aclGroup, id}] = string(permissions)
	}
	return named, nil
}

// aclPlan is what a file_access_control_list task changes: setfacl entries to
// set and remove in the access and default lists, and the result's value.
type aclPlan struct {
	set, remove               []string
	setDefault, removeDefault []string
	// clearMask and clearDefault make a list minimal again once it has no
	// named entry left: setfacl keeps the mask, and the default list's base entries.
	clearMask, clearDefault bool
	paths                   int
	changes                 map[string]any
}

// FileAccessControlList manages the named entries of a file's or directory's
// access control list with setfacl. It reads the lists from the file system's
// extended attributes, where getfacl reads them, so a large tree is inspected
// without holding getfacl's output.
func (run Runtime) FileAccessControlList(ctx context.Context, a FileAccessControlList) (result protocol.Result) {
	if err := ValidateFileAccessControlList(a); err != nil {
		return protocol.ConfigurationError(err)
	}
	for _, tool := range []string{"getfacl", "setfacl"} {
		if _, err := lookPath(ctx, tool); err != nil {
			return run.notYet(missingProgramError(tool))
		}
	}
	info, err := os.Lstat(a.Path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return run.notYet(fmt.Errorf("%s does not exist yet", a.Path))
	case err != nil:
		return protocol.Failed(err)
	case info.Mode()&os.ModeSymlink != 0:
		return protocol.Failed(fmt.Errorf("%s is a symbolic link; give the path of the file or directory itself", a.Path))
	case !info.IsDir() && (a.AlsoApplyToEverythingInside || a.AlsoUseForNewFilesInside):
		return protocol.Failed(fmt.Errorf("also_apply_to_everything_inside and also_use_for_new_files_inside require a directory, but %s is not a directory", a.Path))
	}
	wanted := map[aclEntry]string{}
	names := map[aclEntry]string{}
	for _, e := range a.Entries {
		entry, err := lookupACLEntry(e)
		if err != nil {
			return run.accountFailure(err)
		}
		if _, repeated := wanted[entry]; repeated {
			return protocol.ConfigurationError(fmt.Errorf("file_access_control_list lists %s twice", entry.name()))
		}
		wanted[entry] = e.Permissions
		names[entry] = "user_account " + e.UserAccount
		if e.Group != "" {
			names[entry] = "group " + e.Group
		}
	}
	plan, err := planACL(a, wanted, names)
	if err != nil {
		return protocol.Failed(err)
	}
	if len(plan.changes) == 0 {
		return protocol.Success("unchanged")
	}
	if run.Dryrun {
		r := protocol.Success("would_change")
		r.Value = plan.changes
		return r
	}
	if err := run.Mutation(a.Path); err != nil {
		return protocol.Failed(err)
	}
	// From here the value lists only what the setfacl calls did, so that a
	// failed task never reports a change that did not happen.
	done := map[string]any{}
	defer func() {
		if len(done) > 0 {
			result.Value = done
		}
	}()
	change := "unknown"
	recursive := []string{}
	if a.AlsoApplyToEverythingInside {
		// setfacl skips symbolic links below path, as the inspection does.
		recursive = append(recursive, "-R")
	}
	setfacl := func(args ...string) error {
		r := Command(ctx, nil, "", "setfacl", append(append(recursive, args...), "--", a.Path)...)
		if r.Status != "succeeded" {
			return fmt.Errorf("setfacl %s %s: %s: %s", strings.Join(args, " "), a.Path, r.Reason, r.Stderr)
		}
		change = "changed"
		return nil
	}
	// Each setfacl call changes the entries of its own kinds, in the access
	// list or in the default list.
	type step struct {
		args        []string
		kinds       []string
		defaultList bool
	}
	steps := []step{}
	if len(plan.set) > 0 {
		steps = append(steps, step{[]string{"-m", strings.Join(plan.set, ",")}, []string{"added", "updated"}, false})
	}
	if len(plan.remove) > 0 || plan.clearMask {
		specs := plan.remove
		if plan.clearMask {
			specs = append(slices.Clone(specs), "m::")
		}
		steps = append(steps, step{[]string{"-x", strings.Join(specs, ",")}, []string{"removed"}, false})
	}
	if len(plan.setDefault) > 0 {
		steps = append(steps, step{[]string{"-d", "-m", strings.Join(plan.setDefault, ",")}, []string{"added", "updated"}, true})
	}
	if plan.clearDefault {
		steps = append(steps, step{[]string{"-k"}, []string{"removed"}, true})
	} else if len(plan.removeDefault) > 0 {
		steps = append(steps, step{[]string{"-d", "-x", strings.Join(plan.removeDefault, ",")}, []string{"removed"}, true})
	}
	for _, s := range steps {
		if err := setfacl(s.args...); err != nil {
			r := protocol.Failed(err)
			r.Change = change
			return r
		}
		for _, kind := range s.kinds {
			texts, _ := plan.changes[kind].([]string)
			for _, text := range texts {
				if strings.HasPrefix(text, "default ") == s.defaultList {
					done[kind] = append(listed(done[kind]), text)
				}
			}
		}
	}
	if paths, ok := plan.changes["number_of_paths_changed"]; ok {
		done["number_of_paths_changed"] = paths
	}
	after, err := planACL(a, wanted, names)
	if err == nil && len(after.changes) > 0 {
		err = fmt.Errorf("the access control list of %s does not match the requested entries", a.Path)
	}
	if err != nil {
		r := protocol.Failed(err)
		r.Change = change
		return r
	}
	return protocol.Success(change)
}

func lookupACLEntry(e AccessControlEntry) (aclEntry, error) {
	if e.Group != "" {
		g, err := user.LookupGroup(e.Group)
		if err != nil {
			return aclEntry{}, err
		}
		id, err := strconv.ParseUint(g.Gid, 10, 32)
		return aclEntry{true, uint32(id)}, err
	}
	u, err := user.Lookup(e.UserAccount)
	if err != nil {
		return aclEntry{}, err
	}
	id, err := strconv.ParseUint(u.Uid, 10, 32)
	return aclEntry{false, uint32(id)}, err
}

// planACL compares path, and with also_apply_to_everything_inside every file
// and directory below it, with the wanted entries. Symbolic links below path
// are skipped, as setfacl skips them.
func planACL(a FileAccessControlList, wanted, names map[aclEntry]string) (aclPlan, error) {
	plan := aclPlan{changes: map[string]any{}}
	removeOthers := a.State == "absent" || a.EntriesNotListed == "remove"
	noted := map[string]bool{}
	note := func(kind, text string) {
		if !noted[kind+text] {
			noted[kind+text] = true
			list, _ := plan.changes[kind].([]string)
			plan.changes[kind] = append(list, text)
		}
	}
	add := func(list *[]string, spec string) {
		if !slices.Contains(*list, spec) {
			*list = append(*list, spec)
		}
	}
	label := func(e aclEntry) string {
		if name, ok := names[e]; ok {
			return name
		}
		return e.name()
	}
	compare := func(path, attribute, prefix string, set, remove *[]string) (bool, error) {
		current, err := readACL(path, attribute)
		if err != nil {
			return false, err
		}
		changed := false
		for _, e := range sortedEntries(wanted) {
			permissions := wanted[e]
			switch have, ok := current[e]; {
			case !ok:
				note("added", prefix+label(e)+": "+permissions)
			case have != permissions:
				note("updated", prefix+label(e)+": "+permissions)
			default:
				continue
			}
			add(set, e.spec()+":"+permissions)
			changed = true
		}
		if removeOthers {
			for _, e := range sortedEntries(current) {
				if _, listed := wanted[e]; !listed {
					note("removed", prefix+label(e)+": "+current[e])
					add(remove, e.spec())
					changed = true
				}
			}
		}
		return changed, nil
	}
	visit := func(path string, directory bool) error {
		changed, err := compare(path, aclAccess, "", &plan.set, &plan.remove)
		if err != nil {
			return err
		}
		if a.AlsoUseForNewFilesInside && directory {
			changedDefault, err := compare(path, aclDefault, "default ", &plan.setDefault, &plan.removeDefault)
			if err != nil {
				return err
			}
			changed = changed || changedDefault
		}
		if changed {
			plan.paths++
		}
		return nil
	}
	var err error
	if a.AlsoApplyToEverythingInside {
		err = filepath.WalkDir(a.Path, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.Type()&fs.ModeSymlink != 0 {
				return nil
			}
			return visit(path, d.IsDir())
		})
	} else {
		info, statErr := os.Lstat(a.Path)
		if statErr != nil {
			return plan, statErr
		}
		err = visit(a.Path, info.IsDir())
	}
	if err != nil {
		return plan, err
	}
	// Once no named entry remains, the mask and the default list's base entries go too.
	plan.clearMask = len(plan.remove) > 0 && removeOthers && len(wanted) == 0
	plan.clearDefault = len(plan.removeDefault) > 0 && removeOthers && len(wanted) == 0
	if plan.paths > 0 && a.AlsoApplyToEverythingInside {
		plan.changes["number_of_paths_changed"] = plan.paths
	}
	return plan, nil
}

func sortedEntries(m map[aclEntry]string) []aclEntry {
	out := make([]aclEntry, 0, len(m))
	for e := range m {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].group != out[j].group {
			return !out[i].group
		}
		return out[i].id < out[j].id
	})
	return out
}

// LinuxCapabilityOnProgramFile makes the capabilities of a program file, as
// getcap prints them, exactly the listed ones, permitted and effective, with
// setcap.
func (run Runtime) LinuxCapabilityOnProgramFile(ctx context.Context, c LinuxCapabilityOnProgramFile) (result protocol.Result) {
	if err := ValidateLinuxCapabilityOnProgramFile(c); err != nil {
		return protocol.ConfigurationError(err)
	}
	for _, tool := range []string{"getcap", "setcap"} {
		if _, err := lookPath(ctx, tool); err != nil {
			return run.notYet(missingProgramError(tool))
		}
	}
	info, err := os.Lstat(c.Path)
	switch {
	case errors.Is(err, os.ErrNotExist) && c.State == "absent":
		return protocol.Success("unchanged")
	case errors.Is(err, os.ErrNotExist):
		return run.notYet(fmt.Errorf("%s does not exist yet", c.Path))
	case err != nil:
		return protocol.Failed(err)
	case info.Mode()&os.ModeSymlink != 0:
		return protocol.Failed(fmt.Errorf("%s is a symbolic link; give the path of the program file itself", c.Path))
	case !info.Mode().IsRegular():
		return protocol.Failed(fmt.Errorf("%s is not a regular file", c.Path))
	}
	wanted := map[string]string{}
	for _, name := range c.Capabilities {
		wanted[name] = "ep"
	}
	before, current, err := readCapabilities(ctx, c.Path)
	if err != nil {
		return protocol.Failed(err)
	}
	if c.CapabilitiesNotListed == "keep" {
		for name, flags := range current {
			if _, listed := wanted[name]; !listed {
				wanted[name] = flags
			}
		}
	}
	if maps.Equal(current, wanted) {
		return protocol.Success("unchanged")
	}
	after := capabilityText(wanted)
	changes := map[string]any{"capabilities_before": before, "capabilities_after": after}
	if run.Dryrun {
		for _, flags := range wanted {
			if !strings.Contains(flags, "e") {
				return protocol.Failed(errors.New("Linux stores one effective flag for the whole file; setcap cannot keep an unlisted capability without that flag while making listed capabilities effective"))
			}
		}
		r := protocol.Success("would_change")
		r.Value = changes
		return r
	}
	if err := run.Mutation(c.Path); err != nil {
		return protocol.Failed(err)
	}
	args := []string{"-r", c.Path}
	if len(wanted) > 0 {
		args = []string{after, c.Path}
	}
	if r := Command(ctx, nil, "", "setcap", args...); r.Status != "succeeded" {
		return protocol.Failed(fmt.Errorf("setcap %s: %s: %s", strings.Join(args, " "), r.Reason, r.Stderr))
	}
	// From here setcap did its work, so the value says what it changed.
	_, current, err = readCapabilities(ctx, c.Path)
	if err == nil && !maps.Equal(current, wanted) {
		err = fmt.Errorf("%s did not reach the capabilities %s", c.Path, after)
	}
	r := protocol.Success("changed")
	if err != nil {
		r = protocol.Failed(err)
		r.Change = "changed"
	}
	r.Value = changes
	return r
}

// readCapabilities returns getcap's text for path, without the path, and the
// capabilities it names with their sorted flags (e, i, p).
func readCapabilities(ctx context.Context, path string) (string, map[string]string, error) {
	r := Command(ctx, nil, "", "getcap", path)
	if r.Status != "succeeded" {
		return "", nil, fmt.Errorf("getcap %s: %s: %s", path, r.Reason, r.Stderr)
	}
	// libcap 2.30 and later print "path cap_net_raw=ep"; earlier ones "path = cap_net_raw+ep".
	text := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(r.Stdout), path))
	text = strings.TrimSpace(strings.TrimPrefix(text, "= "))
	current, err := parseCapabilities(text)
	if err != nil {
		return "", nil, fmt.Errorf("getcap printed %q for %s: %w", text, path, err)
	}
	return text, current, nil
}

// parseCapabilities reads capability clauses such as "cap_a,cap_b=ep cap_c+i".
func parseCapabilities(text string) (map[string]string, error) {
	flags := map[string]map[rune]bool{}
	for _, clause := range strings.Fields(text) {
		at := strings.IndexAny(clause, "=+-")
		if at <= 0 {
			return nil, errors.New("MMX compares only clauses that name their capabilities")
		}
		names := strings.Split(clause[:at], ",")
		operations := clause[at:]
		for len(operations) > 0 {
			op := operations[0]
			end := strings.IndexAny(operations[1:], "=+-")
			letters := operations[1:]
			if end >= 0 {
				letters, operations = operations[1:end+1], operations[end+1:]
			} else {
				operations = ""
			}
			for _, name := range names {
				if name == "all" || !strings.HasPrefix(name, "cap_") {
					return nil, fmt.Errorf("MMX does not compare the capability %q", name)
				}
				if flags[name] == nil || op == '=' {
					flags[name] = map[rune]bool{}
				}
				for _, letter := range letters {
					if !strings.ContainsRune("eip", letter) {
						return nil, fmt.Errorf("unknown capability flag %q", letter)
					}
					flags[name][letter] = op != '-'
				}
			}
		}
	}
	out := map[string]string{}
	for name, set := range flags {
		text := ""
		for _, letter := range "eip" {
			if set[letter] {
				text += string(letter)
			}
		}
		if text != "" {
			out[name] = text
		}
	}
	return out, nil
}

// capabilityText writes sorted clauses, retaining the flags of unlisted capabilities.
func capabilityText(wanted map[string]string) string {
	groups := map[string][]string{}
	for name, flags := range wanted {
		groups[flags] = append(groups[flags], name)
	}
	var clauses []string
	for flags, names := range groups {
		sort.Strings(names)
		clauses = append(clauses, strings.Join(names, ",")+"="+flags)
	}
	sort.Strings(clauses)
	return strings.Join(clauses, " ")
}

// FileAccessControlList is the extra access entries (named accounts and
// groups) of one file or directory, managed with getfacl and setfacl.
type FileAccessControlList struct {
	Path                        string               `yaml:"path" json:"path"`
	Entries                     []AccessControlEntry `yaml:"entries,omitempty" json:"entries"`
	EntriesNotListed            string               `yaml:"entries_not_listed,omitempty" json:"entries_not_listed,omitempty"`
	AlsoApplyToEverythingInside bool                 `yaml:"also_apply_to_everything_inside,omitempty" json:"also_apply_to_everything_inside,omitempty"`
	AlsoUseForNewFilesInside    bool                 `yaml:"also_use_for_new_files_inside,omitempty" json:"also_use_for_new_files_inside,omitempty"`
	State                       string               `yaml:"state,omitempty" json:"state,omitempty"`
}

// AccessControlEntry names exactly one of an account or a group, and its
// permissions as getfacl prints them, such as r-x.
type AccessControlEntry struct {
	UserAccount string `yaml:"user_account,omitempty" json:"user_account,omitempty"`
	Group       string `yaml:"group,omitempty" json:"group,omitempty"`
	Permissions string `yaml:"permissions" json:"permissions"`
}

// LinuxCapabilityOnProgramFile manages listed file capabilities and the
// explicit policy for unlisted capabilities, through getcap and setcap.
type LinuxCapabilityOnProgramFile struct {
	Path                  string   `yaml:"path" json:"path"`
	CapabilitiesNotListed string   `yaml:"capabilities_not_listed,omitempty" json:"capabilities_not_listed,omitempty"`
	Capabilities          []string `yaml:"capabilities,omitempty" json:"capabilities,omitempty"`
	State                 string   `yaml:"state,omitempty" json:"state,omitempty"`
}

var aclPermissions = regexp.MustCompile(`^[r-][w-][x-]$`)

// LinuxCapabilities are the capability names setcap accepts, as in
// capabilities(7).
var LinuxCapabilities = []string{
	"cap_audit_control", "cap_audit_read", "cap_audit_write", "cap_block_suspend", "cap_bpf",
	"cap_checkpoint_restore", "cap_chown", "cap_dac_override", "cap_dac_read_search", "cap_fowner",
	"cap_fsetid", "cap_ipc_lock", "cap_ipc_owner", "cap_kill", "cap_lease", "cap_linux_immutable",
	"cap_mac_admin", "cap_mac_override", "cap_mknod", "cap_net_admin", "cap_net_bind_service",
	"cap_net_broadcast", "cap_net_raw", "cap_perfmon", "cap_setfcap", "cap_setgid", "cap_setpcap",
	"cap_setuid", "cap_sys_admin", "cap_sys_boot", "cap_sys_chroot", "cap_sys_module", "cap_sys_nice",
	"cap_sys_pacct", "cap_sys_ptrace", "cap_sys_rawio", "cap_sys_resource", "cap_sys_time",
	"cap_sys_tty_config", "cap_syslog", "cap_wake_alarm",
}

func ValidateFileAccessControlList(a FileAccessControlList) error {
	if err := validatePresence(a.State); err != nil {
		return err
	}
	if a.State == "absent" {
		if a.Entries != nil || a.EntriesNotListed != "" {
			return fmt.Errorf("file_access_control_list with state: absent removes every named entry and accepts only path, also_apply_to_everything_inside, also_use_for_new_files_inside, and state")
		}
		return nil
	}
	if a.Entries == nil {
		return invalid("entries", "file_access_control_list.entries is required (use [] with entries_not_listed: remove to remove every named entry)")
	}
	if err := validateNotListed("entries", true, a.EntriesNotListed); err != nil {
		return err
	}
	seen := map[string]bool{}
	for i, e := range a.Entries {
		field := fmt.Sprintf("entries[%d]", i)
		if (e.UserAccount == "") == (e.Group == "") {
			return invalid(field, "each entry names exactly one of user_account or group")
		}
		if !aclPermissions.MatchString(e.Permissions) {
			return invalid(field+".permissions", "permissions are three characters as getfacl prints them: r or -, w or -, x or -, such as r-x")
		}
		// Written names are checked by their argument rule; this adds repeats.
		key := "user_account " + e.UserAccount
		if e.Group != "" {
			key = "group " + e.Group
		}
		if !Dynamic(e.UserAccount+e.Group) && seen[key] {
			return invalid(field, key+" is listed twice")
		}
		seen[key] = true
	}
	return nil
}

func ValidateLinuxCapabilityOnProgramFile(c LinuxCapabilityOnProgramFile) error {
	if err := validatePresence(c.State); err != nil {
		return err
	}
	if c.State == "absent" {
		if c.Capabilities != nil || c.CapabilitiesNotListed != "" {
			return fmt.Errorf("linux_capability_on_program_file with state: absent removes every capability and accepts only path and state")
		}
		return nil
	}
	if len(c.Capabilities) == 0 {
		return invalid("capabilities", "linux_capability_on_program_file.capabilities is required; to remove every capability, use state: absent")
	}
	if err := validateNotListed("capabilities", true, c.CapabilitiesNotListed); err != nil {
		return err
	}
	for i, name := range c.Capabilities {
		field := fmt.Sprintf("capabilities[%d]", i)
		if !slices.Contains(LinuxCapabilities, name) {
			return invalid(field, fmt.Sprintf("%q is not a Linux capability; write the name in lower case as capabilities(7) lists it, such as cap_net_raw or cap_net_bind_service", name))
		}
		if slices.Contains(c.Capabilities[:i], name) {
			return invalid(field, name+" is listed twice")
		}
	}
	return nil
}

// fileAccessControlListDescription is what mmx describe-module shows for file_access_control_list.
var fileAccessControlListDescription = Description{
	Name: "file_access_control_list", Topic: TopicSELinuxAccessControlListsAndCapabilities, Schema: reflect.TypeOf(FileAccessControlList{}),
	Summary: "the named account and group entries of a file's or directory's access control list, through getfacl and setfacl (package acl)",
	Fields: []Field{
		{Name: "path", Required: true, Notes: "absolute; must exist and must not be a symbolic link"},
		{Name: "entries", Type: "list", Notes: "each {user_account: name, permissions: r-x} or {group: name, permissions: r-x}; exactly one of user_account or group; permissions are r or -, w or -, then x or -; required unless state: absent"},
		{Name: "entries_not_listed", Values: notListed, Notes: "required with entries; remove makes entries the whole list of named accounts and groups, and entries: [] removes them all; keep adds or corrects the listed entries only"},
		{Name: "also_apply_to_everything_inside", Default: "false", Notes: "directories only: the same entries for every file and directory below path; symbolic links inside are skipped"},
		{Name: "also_use_for_new_files_inside", Default: "false", Notes: "directories only: also sets the entries as the directory's default entries, which new files and directories inside inherit"},
		{Name: "state", Values: presenceStates, Default: "present", Notes: "absent removes every named entry and accepts only path and the two also_ settings"},
	},
	Example: "- file_access_control_list:\n    path: /var/log/app\n    entries:\n      - {user_account: promtail, permissions: r-x}\n    entries_not_listed: remove\n",
	Results: []string{"status", "change", "value.added", "value.updated", "value.removed", "value.number_of_paths_changed"},
	Dryrun:  "supported; lists the entries to add, update, or remove; not checked while the acl package, the path, or a named account is missing",
}

// linuxCapabilityOnProgramFileDescription is what mmx describe-module shows for linux_capability_on_program_file.
var linuxCapabilityOnProgramFileDescription = Description{
	Name: "linux_capability_on_program_file", Topic: TopicSELinuxAccessControlListsAndCapabilities, Schema: reflect.TypeOf(LinuxCapabilityOnProgramFile{}),
	Summary: "the Linux capabilities of a program file, through getcap and setcap (package libcap2-bin on Debian and Ubuntu, libcap on the Red Hat family)",
	Fields: []Field{
		{Name: "path", Required: true, Notes: "absolute path of a regular file, not a symbolic link"},
		{Name: "capabilities", Type: "list", Notes: "names as capabilities(7) lists them, such as cap_net_raw; listed capabilities become permitted and effective; required unless state: absent"},
		{Name: "capabilities_not_listed", Required: true, Values: notListed, Notes: "required unless state: absent; remove removes unlisted capabilities, keep preserves their existing flags. Linux stores one effective flag for the whole file, so when an unlisted capability lacks it, setcap refuses the result and the task fails with setcap's message"},
		{Name: "state", Values: presenceStates, Default: "present", Notes: "absent removes every capability and accepts only path"},
	},
	Example: "- linux_capability_on_program_file:\n    path: /usr/local/bin/pinger\n    capabilities: [cap_net_raw]\n    capabilities_not_listed: remove\n",
	Results: []string{"status", "change", "value.capabilities_before", "value.capabilities_after"},
	Dryrun:  "supported; shows the capabilities before and after; fails if keep would leave an unlisted capability without the file-wide effective flag; not checked while the file or setcap is missing",
}

// Prepare checks the inputs when the project loads.
func (a *FileAccessControlList) Prepare(base string) error {
	return ValidateFileAccessControlList(*a)
}

// Prepare checks the inputs when the project loads.
func (c *LinuxCapabilityOnProgramFile) Prepare(base string) error {
	return ValidateLinuxCapabilityOnProgramFile(*c)
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (a *FileAccessControlList) Arguments(arguments *ArgumentList) {
	arguments.Public("path", &a.Path, absolute)
	for i := range a.Entries {
		e := &a.Entries[i]
		if e.UserAccount != "" {
			arguments.Public(fmt.Sprintf("entries[%d].user_account", i), &e.UserAccount, accountNameRule)
		}
		if e.Group != "" {
			arguments.Public(fmt.Sprintf("entries[%d].group", i), &e.Group, accountNameRule)
		}
	}
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (c *LinuxCapabilityOnProgramFile) Arguments(arguments *ArgumentList) {
	arguments.Public("path", &c.Path, absolute)
}

// Run does the module's work on the node.
func (a *FileAccessControlList) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.Runtime.FileAccessControlList(ctx, *a)
}

// Run does the module's work on the node.
func (c *LinuxCapabilityOnProgramFile) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.Runtime.LinuxCapabilityOnProgramFile(ctx, *c)
}
