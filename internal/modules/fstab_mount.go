package modules

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

// The files the storage modules read and write; tests point them elsewhere.
var (
	fstabPath     = "/etc/fstab"
	mountinfoPath = "/proc/self/mountinfo"
	// systemdBootedMarker exists when systemd runs the node (sd_booted).
	systemdBootedMarker = "/run/systemd/system"
)

// fstabMountStates are fstab_mount's states.
var fstabMountStates = []string{"mounted", "unmounted_and_removed_from_fstab"}

// FstabMount keeps one line of /etc/fstab, identified by its mount point,
// and the live mount in step with it.
type FstabMount struct {
	MountPoint            string   `yaml:"mount_point" json:"mount_point"`
	MountSource           string   `yaml:"mount_source,omitempty" json:"mount_source,omitempty"`
	FilesystemType        string   `yaml:"filesystem_type,omitempty" json:"filesystem_type,omitempty"`
	Options               []string `yaml:"options,omitempty" json:"options,omitempty"`
	CheckFilesystemAtBoot string   `yaml:"check_filesystem_at_boot,omitempty" json:"check_filesystem_at_boot,omitempty"`
	State                 string   `yaml:"state" json:"state"`
}

// fstabLine is one line of /etc/fstab with its fields unescaped. A field the
// line leaves out has fstab's default.
type fstabLine struct {
	source, mountPoint, filesystemType, options, dump, checkOrder string
}

// parseFstabLine reads one line; false for a comment, an empty line, or a line
// too short to name a mount point and a type.
func parseFstabLine(line string) (fstabLine, bool) {
	text := strings.TrimSpace(line)
	if text == "" || strings.HasPrefix(text, "#") {
		return fstabLine{}, false
	}
	fields := strings.Fields(text)
	if len(fields) < 3 {
		return fstabLine{}, false
	}
	l := fstabLine{source: fstabUnescape(fields[0]), mountPoint: fstabUnescape(fields[1]), filesystemType: fields[2], options: "defaults", dump: "0", checkOrder: "0"}
	for i, field := range []*string{&l.options, &l.dump, &l.checkOrder} {
		if len(fields) > 3+i {
			*field = fields[3+i]
		}
	}
	return l, true
}

// text is the line as MMX writes it.
func (l fstabLine) text() string {
	return strings.Join([]string{fstabEscape(l.source), fstabEscape(l.mountPoint), l.filesystemType, l.options, l.dump, l.checkOrder}, " ")
}

// fstabEscape writes the characters that would end a field, and the
// backslash, as the octal escapes fstab(5) reads.
func fstabEscape(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if c == ' ' || c == '\t' || c == '\n' || c == '\\' {
			fmt.Fprintf(&b, "\\%03o", c)
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// fstabUnescape reads the octal escapes of fstab and of the kernel's mount
// and swap tables, such as \040 for a space.
func fstabUnescape(s string) string {
	if !strings.Contains(s, "\\") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// editFstab returns text with the one line that picks selects replaced by
// want, removed when want is nil, or, when no line is selected, want added at
// the end. A selected line for which same is true stays as written. owner
// names the task's line in the error about several selected lines.
func editFstab(text string, picks func(fstabLine) bool, same func(fstabLine) bool, want *fstabLine, owner string) (string, error) {
	lines := strings.SplitAfter(text, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	var selected []int
	var numbers []string
	for i, line := range lines {
		if l, ok := parseFstabLine(line); ok && picks(l) {
			selected = append(selected, i)
			numbers = append(numbers, strconv.Itoa(i+1))
		}
	}
	switch {
	case len(selected) > 1:
		return "", fmt.Errorf("%s has %d lines for %s (lines %s); keep exactly one matching line and remove the extra matching lines by hand", fstabPath, len(selected), owner, strings.Join(numbers, ", "))
	case len(selected) == 1 && want == nil:
		lines = slices.Delete(lines, selected[0], selected[0]+1)
	case len(selected) == 1:
		l, _ := parseFstabLine(lines[selected[0]])
		if !same(l) {
			lines[selected[0]] = want.text() + "\n" //nolint:nilaway // The preceding switch case handles len(selected) == 1 && want == nil, excluding nil from this case.
		}
	case want != nil:
		if n := len(lines); n > 0 && !strings.HasSuffix(lines[n-1], "\n") {
			lines[n-1] += "\n"
		}
		lines = append(lines, want.text()+"\n")
	}
	return strings.Join(lines, ""), nil
}

// readFstab reads /etc/fstab; a missing file is empty.
func readFstab() (string, error) {
	parent, err := os.OpenRoot(filepath.Dir(fstabPath))
	if err != nil {
		return "", err
	}
	defer parent.Close()
	info, err := entryInfoAt(parent, filepath.Base(fstabPath), false)
	if err != nil || info == nil {
		return "", err
	}
	return readTextAt(parent, filepath.Base(fstabPath))
}

// writeFstab installs text as /etc/fstab through the file module: a new file
// beside the old one that findmnt --verify must accept before it replaces the
// old one, keeping its owner, group, and mode. A dryrun shows the difference
// and does not run the check, which needs that new file. It then asks
// systemd, which makes mount and swap units from /etc/fstab, to read it again.
func (run Runtime) writeFstab(ctx context.Context, text string) protocol.Result {
	parent, err := os.OpenRoot(filepath.Dir(fstabPath))
	if err != nil {
		return protocol.Failed(err)
	}
	defer parent.Close()
	var verify func(string) protocol.Result
	if !run.Dryrun {
		verify = func(candidate string) protocol.Result {
			out := run.RunCommand(ctx, []string{"findmnt", "--verify", "--tab-file", candidate}, "")
			if out.Error != "" || out.ExitCode != 0 {
				report := strings.TrimSpace(strings.ReplaceAll(out.StandardOutput+"\n"+out.StandardError+"\n"+out.Error, candidate, fstabPath))
				return protocol.Failed(fmt.Errorf("findmnt --verify did not accept the new %s, so it was not written: %s", fstabPath, report))
			}
			return protocol.Success("unchanged")
		}
	}
	result := run.fileAt(ctx, File{Path: fstabPath, Literal: &text}, value.Scope{}, parent, 0644, verify)
	if run.Dryrun || result.Status != "succeeded" || result.Change != "changed" {
		return result
	}
	if _, err := os.Stat(systemdBootedMarker); err == nil {
		reload := run.RunCommandThatChangesTheNode(ctx, []string{"systemctl", "daemon-reload"}, "")
		if reload.Error != "" || reload.ExitCode != 0 {
			result.Status = "failed"
			result.Reason = fmt.Sprintf("%s was written, but systemctl daemon-reload failed: %s", fstabPath, strings.TrimSpace(reload.Error+" "+reload.StandardError))
		}
	}
	return result
}

// liveMount is one mount of /proc/self/mountinfo, its fields unescaped:
// the mount's own options and its filesystem's options.
type liveMount struct {
	mountPoint, filesystemType, source, mountOptions, filesystemOptions string
}

// liveMountAt returns the mount whose mount point is path, the one on top
// when several are stacked there, or nil when nothing is mounted at path.
func liveMountAt(path string) (*liveMount, error) {
	f, err := os.Open(mountinfoPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var found *liveMount
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	for scanner.Scan() {
		// id parent major:minor root mount-point options [optional...] - type source filesystem-options
		fields := strings.Fields(scanner.Text())
		separator := slices.Index(fields, "-")
		if separator < 6 || len(fields) < separator+4 {
			continue
		}
		if fstabUnescape(fields[4]) == path {
			found = &liveMount{mountPoint: path, mountOptions: fields[5], filesystemType: fields[separator+1],
				source: fstabUnescape(fields[separator+2]), filesystemOptions: fields[separator+3]}
		}
	}
	return found, scanner.Err()
}

// mountFlags are the flags the kernel keeps for each mount, which
// /proc/self/mountinfo shows exactly and fstab_mount compares exactly. A
// filesystem's own options, such as commit=30, the kernel shows in its own
// forms and leaves out at their defaults, so they are not compared.
type mountFlags struct {
	readOnly, noSUID, noDev, noExec, noDirATime, noSymFollow bool
	// atime is relatime, noatime, or strictatime.
	atime string
}

// expectedMountFlags are the flags mount(8) gives a mount with these
// options, in order, as fstab lists them.
func expectedMountFlags(options []string) mountFlags {
	f := mountFlags{atime: "relatime"}
	for _, option := range options {
		switch option {
		case "defaults":
			f.readOnly, f.noSUID, f.noDev, f.noExec = false, false, false, false
		case "ro", "rw":
			f.readOnly = option == "ro"
		case "nosuid", "suid":
			f.noSUID = option == "nosuid"
		case "nodev", "dev":
			f.noDev = option == "nodev"
		case "noexec", "exec":
			f.noExec = option == "noexec"
		case "nodiratime", "diratime":
			f.noDirATime = option == "nodiratime"
		case "nosymfollow", "symfollow":
			f.noSymFollow = option == "nosymfollow"
		case "noatime", "relatime", "strictatime":
			f.atime = option
		case "atime":
			f.atime = "relatime"
		case "user", "users", "owner", "group":
			// These imply noexec, nosuid, and nodev unless later options say otherwise.
			f.noExec, f.noSUID, f.noDev = true, true, true
		}
	}
	return f
}

// liveMountFlags reads the flags of a mount's own options.
func liveMountFlags(mountOptions string) mountFlags {
	f := mountFlags{atime: "strictatime"}
	for _, option := range strings.Split(mountOptions, ",") {
		switch option {
		case "ro":
			f.readOnly = true
		case "nosuid":
			f.noSUID = true
		case "nodev":
			f.noDev = true
		case "noexec":
			f.noExec = true
		case "nodiratime":
			f.noDirATime = true
		case "nosymfollow":
			f.noSymFollow = true
		case "noatime", "relatime":
			f.atime = option
		}
	}
	return f
}

// corrections are the options that turn have's flags into want's.
func (want mountFlags) corrections(have mountFlags) []string {
	var options []string
	pick := func(differ, on bool, yes, no string) {
		if differ {
			options = append(options, map[bool]string{true: yes, false: no}[on])
		}
	}
	pick(want.readOnly != have.readOnly, want.readOnly, "ro", "rw")
	pick(want.noSUID != have.noSUID, want.noSUID, "nosuid", "suid")
	pick(want.noDev != have.noDev, want.noDev, "nodev", "dev")
	pick(want.noExec != have.noExec, want.noExec, "noexec", "exec")
	pick(want.noDirATime != have.noDirATime, want.noDirATime, "nodiratime", "diratime")
	pick(want.noSymFollow != have.noSymFollow, want.noSymFollow, "nosymfollow", "symfollow")
	if want.atime != have.atime {
		options = append(options, want.atime)
	}
	return options
}

// filesystemFlagsMissing lists the listed options that set or clear a flag
// of the whole filesystem (sync, dirsync, lazytime, mand) and that the live
// mount does not show.
func filesystemFlagsMissing(options []string, live *liveMount) []string {
	shown := strings.Split(live.filesystemOptions+","+live.mountOptions, ",")
	var missing []string
	for _, option := range options {
		switch option {
		case "sync", "dirsync", "lazytime", "mand":
			if !slices.Contains(shown, option) {
				missing = append(missing, option)
			}
		case "async", "nolazytime", "nomand":
			if slices.Contains(shown, map[string]string{"async": "sync", "nolazytime": "lazytime", "nomand": "mand"}[option]) {
				missing = append(missing, option)
			}
		}
	}
	return missing
}

// line is the task's line of /etc/fstab.
func (m FstabMount) line() fstabLine {
	return fstabLine{source: m.MountSource, mountPoint: m.MountPoint, filesystemType: m.FilesystemType, options: strings.Join(m.Options, ","), dump: "0", checkOrder: map[string]string{"never": "0", "as_root_filesystem": "1", "after_root_filesystem": "2"}[m.CheckFilesystemAtBoot]}
}

// sameLine reports whether an existing line says what the task's line says,
// however its fields are spaced.
func (m FstabMount) sameLine(l fstabLine) bool {
	return l == m.line()
}

// storageTool fails, or in a dryrun reports not checked, when one of the
// programs a storage module runs is not installed, naming its package.
func (run Runtime) storageTool(ctx context.Context, names ...string) *protocol.Result {
	for _, name := range names {
		if _, err := lookPath(ctx, name); err != nil {
			r := run.notYet(missingProgramError(name))
			return &r
		}
	}
	return nil
}

// FstabMount keeps the task's line of /etc/fstab and the mount at its path in
// step. A live mount of another source or type is a decision for a person,
// so the task then fails and changes nothing.
func (run Runtime) FstabMount(ctx context.Context, m FstabMount) protocol.Result {
	if err := ValidateFstabMount(m); err != nil {
		return protocol.ConfigurationError(err)
	}
	if r := run.storageTool(ctx, "findmnt", "mount", "umount"); r != nil {
		return *r
	}
	current, err := readFstab()
	if err != nil {
		return protocol.Failed(err)
	}
	if m.State == "unmounted_and_removed_from_fstab" {
		return run.unmountAndRemoveLine(ctx, m, current)
	}
	info, err := os.Lstat(m.MountPoint)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return run.notYet(fmt.Errorf("the mount point %s does not exist yet; create the directory first, such as with a directory task", m.MountPoint))
	case err != nil:
		return protocol.Failed(err)
	case info.Mode()&os.ModeSymlink != 0:
		return refused(fmt.Errorf("the mount point %s is a symbolic link; give the directory itself", m.MountPoint))
	case !info.IsDir():
		return refused(fmt.Errorf("the mount point %s is not a directory", m.MountPoint))
	}
	want := m.line()
	next, err := editFstab(current, func(l fstabLine) bool { return l.mountPoint == m.MountPoint }, m.sameLine, &want, m.MountPoint)
	if err != nil {
		return refused(err)
	}
	live, err := liveMountAt(m.MountPoint)
	if err != nil {
		return protocol.Failed(err)
	}
	var mountCommand []string
	var action string
	if live != nil {
		if r := run.checkMountedSource(ctx, m, live); r != nil {
			return *r
		}
		// The written options changed, or the live flags differ from them:
		// remount before the line is written, so an interrupted run finds the
		// line still old and remounts again.
		expected := expectedMountFlags(m.Options)
		have := liveMountFlags(live.mountOptions)
		corrections := expected.corrections(have)
		optionsChanged := true
		if old, ok := lineFor(current, m.MountPoint); ok {
			optionsChanged = old.options != want.options
		}
		if optionsChanged || len(corrections) > 0 || len(filesystemFlagsMissing(m.Options, live)) > 0 {
			// mount would merge the old line's options into a remount, and
			// an old noatime would win over the relatime of the new options.
			options := append([]string{"remount", want.options}, corrections...)
			mountCommand = []string{"mount", "--options-mode", "ignore", "-o", strings.Join(options, ","), m.MountPoint}
			action = "remount " + m.MountPoint + " with options " + want.options
		}
	}
	changed := next != current
	if !changed && mountCommand == nil && live != nil {
		return protocol.Success("unchanged")
	}
	if live == nil {
		action = "mount " + m.MountPoint
	}
	if run.Dryrun {
		r := protocol.Success("unchanged")
		if changed {
			r = run.writeFstab(ctx, next)
		}
		if r.Status == "succeeded" {
			r.Change = "would_change"
			r.Reason = "would " + action
			if action == "" {
				r.Reason = "would correct the line of " + m.MountPoint + " in " + fstabPath
			}
		}
		return r
	}
	done := []string{}
	if mountCommand != nil {
		if r := run.mountCommand(ctx, mountCommand, "remounted "+m.MountPoint, &done); r != nil {
			return *r
		}
		after, err := liveMountAt(m.MountPoint)
		if err != nil {
			return failedAfter(err, done)
		}
		if after == nil {
			return failedAfter(fmt.Errorf("%s is no longer mounted after the remount", m.MountPoint), done)
		}
		expected := expectedMountFlags(m.Options)
		if fix := expected.corrections(liveMountFlags(after.mountOptions)); len(fix) > 0 || len(filesystemFlagsMissing(m.Options, after)) > 0 {
			return failedAfter(fmt.Errorf("after the remount, the live mount of %s has the options %s, %s; it still needs %s, which a remount did not apply; some options take effect only when the filesystem is mounted: unmount it with state: unmounted_and_removed_from_fstab, then run again",
				m.MountPoint, after.mountOptions, after.filesystemOptions, strings.Join(append(fix, filesystemFlagsMissing(m.Options, after)...), ",")), done)
		}
	}
	var result protocol.Result
	if changed {
		result = run.writeFstab(ctx, next)
		if result.Status != "succeeded" {
			if len(done) > 0 {
				result.Change = "changed"
				result.Reason += "; already done: " + strings.Join(done, ", ")
			}
			return result
		}
		done = append(done, "wrote the line of "+m.MountPoint+" in "+fstabPath)
	}
	if live == nil {
		if r := run.mountCommand(ctx, []string{"mount", m.MountPoint}, "mounted "+m.MountPoint, &done); r != nil {
			r.FileChange = result.FileChange
			return *r
		}
		after, err := liveMountAt(m.MountPoint)
		if err == nil && after == nil {
			err = fmt.Errorf("mount reported success, but %s is not mounted", m.MountPoint)
		}
		if err != nil {
			r := failedAfter(err, done)
			r.FileChange = result.FileChange
			return r
		}
		if r := run.checkMountedSource(ctx, m, after); r != nil {
			r.Change, r.FileChange = "changed", result.FileChange
			return *r
		}
	}
	r := protocol.Success("changed")
	r.FileChange = result.FileChange
	r.Reason = strings.Join(done, ", ")
	return r
}

// lineFor returns the one line for path in text, if there is exactly one.
func lineFor(text, path string) (fstabLine, bool) {
	var found []fstabLine
	for _, line := range strings.Split(text, "\n") {
		if l, ok := parseFstabLine(line); ok && l.mountPoint == path {
			found = append(found, l)
		}
	}
	if len(found) != 1 {
		return fstabLine{}, false
	}
	return found[0], true
}

// unmountAndRemoveLine unmounts the path when something is mounted there,
// then removes the task's line. The directory stays.
func (run Runtime) unmountAndRemoveLine(ctx context.Context, m FstabMount, current string) protocol.Result {
	next, err := editFstab(current, func(l fstabLine) bool { return l.mountPoint == m.MountPoint }, nil, nil, m.MountPoint)
	if err != nil {
		return refused(err)
	}
	live, err := liveMountAt(m.MountPoint)
	if err != nil {
		return protocol.Failed(err)
	}
	changed := next != current
	if live == nil && !changed {
		return protocol.Success("unchanged")
	}
	var steps []string
	if live != nil {
		steps = append(steps, "unmount "+m.MountPoint)
	}
	if changed {
		steps = append(steps, "remove the line of "+m.MountPoint+" from "+fstabPath)
	}
	if run.Dryrun {
		r := protocol.Success("would_change")
		if changed {
			r = run.writeFstab(ctx, next)
			r.Change = "would_change"
		}
		r.Reason = "would " + strings.Join(steps, ", then ")
		return r
	}
	done := []string{}
	if live != nil {
		if r := run.mountCommand(ctx, []string{"umount", m.MountPoint}, "unmounted "+m.MountPoint, &done); r != nil {
			return *r
		}
		if after, err := liveMountAt(m.MountPoint); err != nil || after != nil {
			if err == nil {
				err = fmt.Errorf("%s still has %s (%s) mounted after the unmount; removing the top mount exposed another mount at the mount point; run again to unmount the remaining filesystem", m.MountPoint, after.source, after.filesystemType)
			}
			return failedAfter(err, done)
		}
	}
	if !changed {
		r := protocol.Success("changed")
		r.Reason = strings.Join(done, ", ")
		return r
	}
	result := run.writeFstab(ctx, next)
	if result.Status != "succeeded" {
		if len(done) > 0 {
			result.Change = "changed"
			result.Reason += "; already done: " + strings.Join(done, ", ")
		}
		return result
	}
	result.Reason = strings.Join(append(done, "removed the line of "+m.MountPoint+" from "+fstabPath), ", ")
	return result
}

// mountCommand runs mount or umount; a failure carries the system's message.
func (run Runtime) mountCommand(ctx context.Context, command []string, did string, done *[]string) *protocol.Result {
	out := run.RunCommandThatChangesTheNode(ctx, command, "")
	if out.Error != "" || out.ExitCode != 0 {
		r := failedAfter(fmt.Errorf("%s failed: %s", strings.Join(command, " "), strings.TrimSpace(out.Error+" "+out.StandardError+" "+out.StandardOutput)), *done)
		if len(*done) == 0 {
			// A failed mount, remount, or unmount leaves the mount as it was.
			r.Change = "unchanged"
		}
		return &r
	}
	*done = append(*done, did)
	return nil
}

// checkMountedSource fails when the live mount at the path has another source
// or type than the task's: changing what is mounted under a running
// application is a decision for a person.
func (run Runtime) checkMountedSource(ctx context.Context, m FstabMount, live *liveMount) *protocol.Result {
	sameType := live.filesystemType == m.FilesystemType || m.FilesystemType == "auto" || (m.FilesystemType == "nfs" && live.filesystemType == "nfs4") //nolint:nilaway // The initial caller checks live != nil; the post-mount caller converts nil after into an error and returns.
	//nolint:nilaway // Both callers exclude a nil mount before calling checkMountedSource.
	sameSource, err := run.sameMountSource(ctx, m.MountSource, live.source)
	if err != nil {
		r := protocol.Failed(err)
		r.Change = "unchanged"
		return &r
	}
	if sameType && sameSource {
		return nil
	}
	r := refused(fmt.Errorf("%s has %s (%s) mounted, and the task asks for %s (%s); changing what is mounted under a running application is a decision for a person: unmount it first with state: unmounted_and_removed_from_fstab",
		m.MountPoint, live.source, live.filesystemType, m.MountSource, m.FilesystemType)) //nolint:nilaway // Both callers exclude a nil mount before calling checkMountedSource.
	return &r
}

// sameMountSource reports whether the task's source is what the kernel shows
// as the live mount's source: the same device for a device path or a UUID=,
// LABEL=, PARTUUID=, or PARTLABEL= tag, which findfs resolves, and the same
// text for anything else, such as nfs01:/export/data or tmpfs.
func (run Runtime) sameMountSource(ctx context.Context, want, live string) (bool, error) {
	if want == live {
		return true, nil
	}
	device := want
	for _, tag := range []string{"UUID=", "LABEL=", "PARTUUID=", "PARTLABEL="} {
		if strings.HasPrefix(want, tag) {
			if r := run.storageTool(ctx, "findfs"); r != nil {
				return false, errors.New(r.Reason)
			}
			out := run.RunCommand(ctx, []string{"findfs", want}, "")
			if out.Error != "" || out.ExitCode != 0 {
				// The tag names no device on this node, so it is not the live one.
				return false, nil
			}
			device = strings.TrimSpace(out.StandardOutput)
		}
	}
	if !strings.HasPrefix(device, "/") || !strings.HasPrefix(live, "/") {
		return device == live, nil
	}
	wantInfo, wantErr := os.Stat(device)
	liveInfo, liveErr := os.Stat(live)
	if wantErr != nil || liveErr != nil {
		return filepath.Clean(device) == filepath.Clean(live), nil
	}
	wantDevice, wantOK := wantInfo.Sys().(*syscall.Stat_t)
	liveDevice, liveOK := liveInfo.Sys().(*syscall.Stat_t)
	if wantOK && liveOK && wantInfo.Mode()&os.ModeDevice != 0 && liveInfo.Mode()&os.ModeDevice != 0 {
		return wantDevice.Rdev == liveDevice.Rdev, nil
	}
	return os.SameFile(wantInfo, liveInfo), nil
}

// refused is a failure that changed nothing.
func refused(err error) protocol.Result {
	r := protocol.Failed(err)
	r.Change = "unchanged"
	return r
}

// failedAfter is a failure after the steps in done, which it names.
func failedAfter(err error, done []string) protocol.Result {
	r := protocol.Failed(err)
	if len(done) > 0 {
		r.Change = "changed"
		r.Reason += "; already done: " + strings.Join(done, ", ")
	}
	return r
}

var filesystemBootCheckChoices = []string{"never", "as_root_filesystem", "after_root_filesystem"}

func ValidateFstabMount(m FstabMount) error {
	if !slices.Contains(fstabMountStates, m.State) {
		return invalid("state", "fstab_mount.state is required: "+strings.Join(fstabMountStates, " or "))
	}
	if m.State == "unmounted_and_removed_from_fstab" {
		if m.MountSource != "" || m.FilesystemType != "" || m.Options != nil || m.CheckFilesystemAtBoot != "" {
			return fmt.Errorf("fstab_mount with state: unmounted_and_removed_from_fstab accepts only mount_point and state; the line is found by its mount point")
		}
		return nil
	}
	if m.MountSource == "" {
		return invalid("mount_source", "fstab_mount.mount_source is required with state: mounted: a device, UUID=..., LABEL=..., or a network location such as nfs01:/export/data")
	}
	if !fstabWord.MatchString(m.FilesystemType) {
		return invalid("filesystem_type", "fstab_mount.filesystem_type is required with state: mounted: the type as mount(8) takes it, such as ext4, xfs, or nfs")
	}
	if !slices.Contains(filesystemBootCheckChoices, m.CheckFilesystemAtBoot) {
		return invalid("check_filesystem_at_boot", "fstab_mount.check_filesystem_at_boot is required: never, as_root_filesystem, or after_root_filesystem")
	}
	if len(m.Options) == 0 {
		return invalid("options", "fstab_mount.options is required with state: mounted: list each mount option, or write options: [defaults]")
	}
	for i, option := range m.Options {
		field := fmt.Sprintf("options[%d]", i)
		if !mountOption.MatchString(option) {
			return invalid(field, fmt.Sprintf("%q is not one mount option; write each option as its own item without spaces or commas, such as noatime or commit=30", option))
		}
		switch name, _, _ := strings.Cut(option, "="); name {
		case "bind", "rbind", "move", "rmove":
			return invalid(field, "fstab_mount does not manage bind mounts or moved mounts; for a bind mount, write a .mount unit with systemd_unit")
		case "remount":
			return invalid(field, "do not include remount in fstab_mount.options; MMX remounts the filesystem when the active mount options differ from fstab_mount.options")
		}
	}
	return nil
}

var (
	fstabWord   = regexp.MustCompile(`^[A-Za-z0-9_.+-]+$`)
	mountOption = regexp.MustCompile(`^[^\s,#]+$`)
	// A mount point is never / itself.
	mountPointRule = rule("mount_point must be an absolute path other than /, without null bytes or line breaks", func(s string) bool {
		return filepath.IsAbs(s) && filepath.Clean(s) != "/" && !strings.ContainsAny(s, "\x00\n")
	})
	mountSourceRule = rule("mount_source must be one line of text without null bytes", func(s string) bool {
		return s != "" && !strings.ContainsAny(s, "\x00\n\r")
	})
)

// fstabMountDescription is what mmx describe-module shows for fstab_mount.
var fstabMountDescription = Description{
	Name: "fstab_mount", Topic: TopicSwapFilesystemsAndMounts, Schema: reflect.TypeOf(FstabMount{}),
	Summary: "one line of /etc/fstab, identified by its mount point, and the live mount in step with it, through mount, umount, and findmnt",
	Fields: []Field{
		{Name: "mount_point", Required: true, Notes: "the mount point, an absolute path other than /; with state: mounted the directory must exist; the task's line in /etc/fstab is the one with this mount point, and more than one such line fails the task"},
		{Name: "mount_source", Required: true, Notes: "required with state: mounted: what is mounted, such as /dev/sdb1, UUID=..., LABEL=..., or nfs01:/export/data"},
		{Name: "filesystem_type", Required: true, Notes: "required with state: mounted: the type as mount(8) takes it, such as ext4, xfs, or nfs"},
		{Name: "options", Type: "list", Required: true, Notes: "required with state: mounted; mount options, one per item, such as [noatime, nodev] or [defaults]; bind mounts are not managed here"},
		{Name: "check_filesystem_at_boot", Required: true, Values: filesystemBootCheckChoices, Notes: "required with state: mounted; never skips the boot check, as_root_filesystem checks first, after_root_filesystem checks after the root filesystem"},
		{Name: "state", Values: fstabMountStates, Required: true, Notes: "mounted writes or corrects the line, mounts the path, and remounts it when the listed options change or the live mount's flags (ro, nosuid, nodev, noexec, atime, ...) differ from them; a live mount of another source or type fails the task; unmounted_and_removed_from_fstab unmounts the path, removes the line, keeps the directory, and accepts only mount_point"},
	},
	Example: "- fstab_mount:\n    mount_point: /srv/data\n    mount_source: /dev/sdb1\n    filesystem_type: ext4\n    options: [noatime]\n    check_filesystem_at_boot: after_root_filesystem\n    state: mounted\n",
	Results: resultBasic,
	Dryrun:  "supported; shows the difference of /etc/fstab and says whether it would mount, remount, or unmount; findmnt --verify checks the new /etc/fstab only in a run; not checked while the mount point or a program is missing",
}

// Prepare checks the inputs when the project loads.
func (m *FstabMount) Prepare(base string) error {
	return ValidateFstabMount(*m)
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (m *FstabMount) Arguments(arguments *ArgumentList) {
	arguments.Public("mount_point", &m.MountPoint, mountPointRule)
	if m.MountSource != "" {
		arguments.Public("mount_source", &m.MountSource, mountSourceRule)
	}
}

// CleanPaths cleans the resolved path.
func (m *FstabMount) CleanPaths() {
	m.MountPoint = filepath.Clean(m.MountPoint)
}

// Run does the module's work on the node.
func (m *FstabMount) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.Runtime.FstabMount(ctx, *m)
}

// ManagedPaths lists the paths the task manages, as written; a dryrun cannot
// check a later task's path_exists on them.
func (m *FstabMount) ManagedPaths() []string {
	return []string{fstabPath}
}
