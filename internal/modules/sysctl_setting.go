package modules

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

// The kernel keeps its settings under /proc/sys. At boot, systemd-sysctl
// applies the .conf files of sysctlDirectories: a file name in an earlier
// directory hides the same name in a later one, the files that remain apply
// in the order of their names, whatever their directory, and a later
// assignment of a setting wins. procps's sysctl --system reads the same files
// and then /etc/sysctl.conf, which some distributions also link into
// /etc/sysctl.d as 99-sysctl.conf. Tests point these paths elsewhere.
var (
	kernelSettingsDirectory = "/proc/sys"
	sysctlDirectories       = []string{"/etc/sysctl.d", "/run/sysctl.d", "/usr/local/lib/sysctl.d", "/usr/lib/sysctl.d"}
	sysctlConfFile          = "/etc/sysctl.conf"
)

// writeKernelSetting sets a setting now: one write of the value to its file
// under /proc/sys, which the kernel checks. Tests replace it to refuse.
var writeKernelSetting = func(path, text string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	_, err = f.Write([]byte(text))
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}

// SysctlSetting sets one kernel setting now and keeps it for the next boot in
// MMX's own file in /etc/sysctl.d. Only this setting is applied; restarting
// systemd-sysctl would apply every file again.
func (run Runtime) SysctlSetting(ctx context.Context, s SysctlSetting) (result protocol.Result) {
	if err := ValidateSysctlSetting(s); err != nil {
		return protocol.ConfigurationError(err)
	}
	file := File{Path: sysctlFilePath(s.Name), State: "absent"}
	if s.State == "boot_setting_removed_live_value_kept" {
		// The live value stays: MMX does not know the value the node would have
		// without the file, so the next boot decides it.
		return run.File(ctx, file, value.Scope{})
	}
	key := sysctlKey(s.Name)
	live := filepath.Join(kernelSettingsDirectory, filepath.FromSlash(key))
	info, err := os.Stat(live)
	if errors.Is(err, os.ErrNotExist) {
		// An earlier kernel_module task may add it.
		return run.notYet(fmt.Errorf("the kernel has no setting %s (%s does not exist); check the setting name, or add a kernel_module task before the sysctl_setting task to load the kernel module that provides the setting", s.Name, live))
	}
	if err != nil {
		return protocol.Failed(err)
	}
	if info.IsDir() {
		return protocol.ConfigurationError(fmt.Errorf("%s names a group of kernel settings; set sysctl_setting.name to a single kernel setting listed by sysctl -a", s.Name))
	}
	before := run.ReadFile(live)
	if before.Error != "" {
		return protocol.Failed(fmt.Errorf("read the kernel setting %s: %s", s.Name, before.Error))
	}
	want := strings.TrimSpace(s.Value)
	conflict, err := sysctlFileAfter(file.Path, key, want)
	if err != nil {
		return protocol.Failed(err)
	}
	if conflict != nil {
		// Another try would find the same file.
		return protocol.ConfigurationError(conflict)
	}
	line := s.Name + " = " + want + "\n"
	file = File{Path: file.Path, Literal: &line, Mode: "0644"}
	liveDiffers := !slices.Equal(strings.Fields(before.Content), strings.Fields(want))
	var values map[string]any
	if liveDiffers {
		values = map[string]any{"live_value_before": strings.Join(strings.Fields(before.Content), " "), "live_value_after": want}
	}
	if run.Dryrun {
		r := run.File(ctx, file, value.Scope{})
		if r.Status == "succeeded" && liveDiffers {
			r.Change = "would_change"
			r.Value = values
		}
		return r
	}
	if liveDiffers {
		if err := run.Mutation("kernel setting " + s.Name); err != nil {
			return protocol.Failed(err)
		}
		if err := writeKernelSetting(live, want); err != nil {
			// The kernel checks the value the same way on every try.
			return protocol.ConfigurationError(fmt.Errorf("the kernel refused %s = %s: %w", s.Name, want, err))
		}
		after := run.ReadFile(live)
		if after.Error == "" && !slices.Equal(strings.Fields(after.Content), strings.Fields(want)) {
			after.Error = fmt.Sprintf("the kernel accepted %s = %s, but now shows %q", s.Name, want, strings.TrimSpace(after.Content))
		}
		if after.Error != "" {
			r := protocol.Failed(errors.New(after.Error))
			r.Change = "changed"
			return r
		}
	}
	r := run.File(ctx, file, value.Scope{})
	if liveDiffers {
		r.Change = "changed"
		r.Value = values
	}
	return r
}

// sysctlKey is a setting's path below /proc/sys, as systemd-sysctl reads a
// name: when the first separator is a dot, dots separate the parts and a
// slash stands for a dot inside a part; when it is a slash, slashes separate
// the parts.
func sysctlKey(name string) string {
	key := []byte(name)
	if i := strings.IndexAny(name, "./"); i >= 0 && name[i] == '.' {
		for j, c := range key {
			switch c {
			case '.':
				key[j] = '/'
			case '/':
				key[j] = '.'
			}
		}
	}
	parts := strings.FieldsFunc(string(key), func(r rune) bool { return r == '/' })
	return strings.Join(parts, "/")
}

// sysctlFilePath is MMX's file for a setting. A slash in the name becomes a
// dot, so both ways of writing a name whose parts have no dot share a file.
func sysctlFilePath(name string) string {
	return filepath.Join(sysctlDirectories[0], "90-mmx-"+strings.ReplaceAll(name, "/", ".")+".conf")
}

// sysctlFileAfter fails when a file that is applied after MMX's own sets the
// setting to another value, which the node would then boot with. It follows
// systemd-sysctl's order and then reads /etc/sysctl.conf as sysctl --system
// does, unless a link already read it. A pattern such as
// net.ipv4.conf.*.rp_filter does not count: an explicit setting wins over it.
func sysctlFileAfter(ours, key, want string) (conflict, err error) {
	chosen := map[string]string{}
	for _, dir := range sysctlDirectories {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if _, hidden := chosen[e.Name()]; !hidden && strings.HasSuffix(e.Name(), ".conf") {
				chosen[e.Name()] = filepath.Join(dir, e.Name())
			}
		}
	}
	var files []string
	for _, name := range slices.Sorted(maps.Keys(chosen)) {
		if name > filepath.Base(ours) {
			files = append(files, chosen[name])
		}
	}
	conf, _ := filepath.EvalSymlinks(sysctlConfFile)
	readsConf := false
	for _, path := range files {
		if resolved, err := filepath.EvalSymlinks(path); err == nil && resolved == conf {
			readsConf = true
		}
	}
	if !readsConf {
		files = append(files, sysctlConfFile)
	}
	lastFile, lastValue := "", ""
	for _, path := range files {
		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) || (err == nil && info.IsDir()) {
			continue // a dangling link or a directory holds no settings
		}
		if err != nil {
			return nil, err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || line[0] == '#' || line[0] == ';' {
				continue
			}
			name, setting, ok := strings.Cut(strings.TrimPrefix(line, "-"), "=")
			name = strings.TrimSpace(name)
			if ok && !strings.ContainsAny(name, "*?[") && sysctlKey(name) == key {
				lastFile, lastValue = path, strings.TrimSpace(setting)
			}
		}
	}
	if lastFile != "" && !slices.Equal(strings.Fields(lastValue), strings.Fields(want)) {
		return fmt.Errorf("%s sets the managed kernel setting to %q after MMX's file %s is applied, so the node would boot with %q; remove the conflicting kernel setting from %s, or manage the kernel setting in the conflicting file instead of with MMX", lastFile, lastValue, ours, lastValue, lastFile), nil
	}
	return nil, nil
}

// SysctlSetting is one kernel setting, as sysctl names and shows it.
type SysctlSetting struct {
	Name  string `yaml:"name" json:"name"`
	Value string `yaml:"value,omitempty" json:"value,omitempty"`
	State string `yaml:"state,omitempty" json:"state,omitempty"`
}

var sysctlSettingStates = []string{"present", "boot_setting_removed_live_value_kept"}

func ValidateSysctlSetting(s SysctlSetting) error {
	if s.State != "" && !slices.Contains(sysctlSettingStates, s.State) {
		return invalid("state", "sysctl_setting.state must be present or boot_setting_removed_live_value_kept")
	}
	if s.Name == "" {
		return invalid("name", "sysctl_setting.name is required, such as net.ipv4.ip_forward")
	}
	if s.State == "boot_setting_removed_live_value_kept" {
		if s.Value != "" {
			return fmt.Errorf("boot_setting_removed_live_value_kept sysctl_setting accepts only name and state")
		}
		return nil
	}
	if s.Value == "" {
		return invalid("value", `sysctl_setting.value is required: the value as sysctl shows it, such as "1"`)
	}
	return nil
}

var (
	// A name as sysctl writes it. Parts are separated by dots, or by slashes
	// when a part has a dot in it; a slash form starts with a slash-separated
	// part, so that systemd-sysctl reads it the same way.
	sysctlNameRule = parsed(func(s string) error {
		message := "sysctl_setting.name must name a kernel setting, such as net.ipv4.ip_forward; when a name component contains a dot, separate all name components with slashes, such as net/ipv4/conf/eth0.10/rp_filter"
		separator := "."
		if strings.Contains(s, "/") {
			separator = "/"
			if i := strings.IndexAny(s, "./"); s[i] == '.' {
				return errors.New(message)
			}
		}
		if strings.ContainsFunc(s, func(r rune) bool { return r <= ' ' || r == 0x7f || strings.ContainsRune("=*?[]\\", r) }) {
			return errors.New(message)
		}
		for _, part := range strings.Split(s, separator) {
			if part == "" || part == "." || part == ".." {
				return errors.New(message)
			}
		}
		// The file name MMX keeps it in must fit the filesystem's limit.
		if len(s) > 240 {
			return errors.New("sysctl_setting.name exceeds the 240-byte limit needed for the generated configuration filename")
		}
		return nil
	})
	sysctlValueRule = rule(`value must be one line of text, as sysctl shows it, such as "1" or "32768 60999"`, func(s string) bool {
		return strings.TrimSpace(s) != "" && !strings.ContainsAny(s, "\x00\r\n")
	})
)

// sysctlSettingDescription is what mmx describe-module shows for sysctl_setting.
var sysctlSettingDescription = Description{
	Name: "sysctl_setting", Topic: TopicKernelSettingsAndKernelModules, Schema: reflect.TypeOf(SysctlSetting{}),
	Summary: "one kernel setting under /proc/sys, set now and kept for the next boot in /etc/sysctl.d/90-mmx-<name>.conf",
	Fields: []Field{
		{Name: "name", Required: true, Notes: "as sysctl writes it, such as net.ipv4.ip_forward; when a part has a dot in it, such as the interface eth0.10, write slashes between all the parts: net/ipv4/conf/eth0.10/rp_filter. A name the kernel does not have fails the task before any change"},
		{Name: "value", Required: true, Notes: `the value as sysctl shows it, such as "1"; compared word by word, so "32768 60999" matches the kernel's tab; a value the kernel refuses fails the task with its message; omit with boot_setting_removed_live_value_kept`},
		{Name: "state", Values: sysctlSettingStates, Default: "present", Notes: "boot_setting_removed_live_value_kept deletes MMX's configuration file and accepts only name; the current kernel value stays until boot-time configuration is applied at the next boot"},
	},
	Example: "- sysctl_setting:\n    name: net.ipv4.ip_forward\n    value: \"1\"\n",
	Results: []string{"status", "change", "value.live_value_before", "value.live_value_after"},
	Dryrun:  "supported; reports the old and the new live value and the difference of MMX's file; a name the kernel does not have yet is not checked, because an earlier kernel_module task may add it. A file applied at boot after MMX's that sets the setting to another value fails the task, in a dryrun too",
}

// Prepare checks the inputs when the project loads.
func (s *SysctlSetting) Prepare(base string) error {
	return ValidateSysctlSetting(*s)
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (s *SysctlSetting) Arguments(arguments *ArgumentList) {
	arguments.Public("name", &s.Name, sysctlNameRule)
	if s.State != "boot_setting_removed_live_value_kept" {
		arguments.Public("value", &s.Value, sysctlValueRule)
	}
}

// Run does the module's work on the node.
func (s *SysctlSetting) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.Runtime.SysctlSetting(ctx, *s)
}
