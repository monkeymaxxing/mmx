package modules

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// kernelFixture stands in for the kernel and the node's configuration
// directories: proc/sys holds settings as files, proc/modules the loaded
// modules, sys/module their options, and a fake modprobe on PATH loads a
// module listed in available/<name> ("option value" lines give its defaults)
// with the options lines of etc/modprobe.d. in_use/<name> makes modprobe -r
// refuse, blocked/<name> makes modprobe succeed without loading, as an
// "install <name> /bin/true" line does. Every call is recorded in calls.
type kernelFixture struct{ dir string }

const fakeModprobe = `
echo "modprobe $*" >> "$dir/calls"
not_found() { echo "modprobe: FATAL: Module $1 not found in directory /lib/modules/6.1.0-test" >&2; exit 1; }
case "$1" in
--dry-run) [ -e "$dir/available/$2" ] || not_found "$2"; exit 0;;
-r)
  [ ! -e "$dir/in_use/$2" ] || { echo "modprobe: FATAL: Module $2 is in use." >&2; exit 1; }
  grep -v "^$2 " "$dir/proc/modules" > "$dir/proc/modules.new"; mv "$dir/proc/modules.new" "$dir/proc/modules"
  rm -rf "$dir/sys/module/$2"; exit 0;;
esac
[ -e "$dir/available/$1" ] || not_found "$1"
[ ! -e "$dir/blocked/$1" ] || exit 0
echo "$1 16384 0 - Live 0x0000000000000000" >> "$dir/proc/modules"
mkdir -p "$dir/sys/module/$1/parameters"
while read -r option value; do echo "$value" > "$dir/sys/module/$1/parameters/$option"; done < "$dir/available/$1"
for word in $(cat "$dir"/etc/modprobe.d/*.conf 2>/dev/null | awk -v m="$1" '$1 == "options" && $2 == m {for (i = 3; i <= NF; i++) print $i}'); do
  echo "${word#*=}" > "$dir/sys/module/$1/parameters/${word%%=*}"
done`

func newKernelFixture(t *testing.T) kernelFixture {
	t.Helper()
	dir := t.TempDir()
	f := kernelFixture{dir}
	for _, d := range []string{"proc/sys/kernel", "sys/module", "etc/sysctl.d", "usr/lib/sysctl.d", "etc/modules-load.d", "etc/modprobe.d", "lib/modules/6.1.0-test", "available", "in_use", "blocked", "bin"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0755); err != nil {
			t.Fatal(err)
		}
	}
	f.set(t, "proc/sys/kernel/osrelease", "6.1.0-test\n")
	f.set(t, "proc/modules", "ext4 1052672 1 - Live 0x0000000000000000\n")
	f.set(t, "lib/modules/6.1.0-test/modules.builtin", "kernel/drivers/block/loop.ko\nkernel/net/unix/unix.ko\n")
	f.set(t, "calls", "")
	writeStandInProgram(t, filepath.Join(dir, "bin/modprobe"), "#!/bin/sh\ndir="+dir+"\n"+fakeModprobe+"\n")
	t.Setenv("PATH", filepath.Join(dir, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, p := range []struct {
		variable *string
		path     string
	}{
		{&kernelSettingsDirectory, "proc/sys"}, {&sysctlConfFile, "etc/sysctl.conf"}, {&procModulesFile, "proc/modules"},
		{&moduleParametersDirectory, "sys/module"}, {&kernelModulesDirectory, "lib/modules"},
		{&modulesLoadDirectory, "etc/modules-load.d"}, {&modprobeDirectory, "etc/modprobe.d"},
	} {
		previous := *p.variable
		*p.variable = filepath.Join(dir, p.path)
		t.Cleanup(func() { *p.variable = previous })
	}
	previousDirectories, previousWrite := sysctlDirectories, writeKernelSetting
	sysctlDirectories = []string{filepath.Join(dir, "etc/sysctl.d"), filepath.Join(dir, "run/sysctl.d"), filepath.Join(dir, "usr/local/lib/sysctl.d"), filepath.Join(dir, "usr/lib/sysctl.d")}
	t.Cleanup(func() { sysctlDirectories, writeKernelSetting = previousDirectories, previousWrite })
	return f
}

func (f kernelFixture) path(name string) string { return filepath.Join(f.dir, name) }

func (f kernelFixture) set(t *testing.T, name, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(f.path(name)), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.path(name), []byte(contents), 0644); err != nil {
		t.Fatal(err)
	}
}

func (f kernelFixture) read(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(f.path(name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (f kernelFixture) exists(name string) bool {
	_, err := os.Lstat(f.path(name))
	return err == nil
}

// changes returns and forgets the modprobe calls that change something.
func (f kernelFixture) changes(t *testing.T) []string {
	var out []string
	for _, call := range strings.Split(strings.TrimSpace(f.read(t, "calls")), "\n") {
		if call != "" && !strings.Contains(call, "--dry-run") {
			out = append(out, call)
		}
	}
	f.set(t, "calls", "")
	return out
}

func TestSysctlSettingSetsTheLiveValueAndKeepsItForTheNextBoot(t *testing.T) {
	ctx := context.Background()
	f := newKernelFixture(t)
	f.set(t, "proc/sys/net/ipv4/ip_forward", "0\n")
	task := SysctlSetting{Name: "net.ipv4.ip_forward", Value: "1"}
	file := "etc/sysctl.d/90-mmx-net.ipv4.ip_forward.conf"

	preview := (Runtime{Dryrun: true}).SysctlSetting(ctx, task)
	if preview.Status != "succeeded" || preview.Change != "would_change" || preview.FileChange == nil || preview.FileChange.Action != "create" ||
		!reflect.DeepEqual(preview.Value, map[string]any{"live_value_before": "0", "live_value_after": "1"}) {
		t.Fatalf("%+v", preview)
	}
	if f.read(t, "proc/sys/net/ipv4/ip_forward") != "0\n" || f.exists(file) {
		t.Fatal("a dryrun changed the node")
	}
	r := (Runtime{}).SysctlSetting(ctx, task)
	if r.Status != "succeeded" || r.Change != "changed" || !reflect.DeepEqual(r.Value, preview.Value) || r.FileChange == nil {
		t.Fatalf("%+v", r)
	}
	if f.read(t, "proc/sys/net/ipv4/ip_forward") != "1" || f.read(t, file) != "net.ipv4.ip_forward = 1\n" {
		t.Fatal(f.read(t, "proc/sys/net/ipv4/ip_forward"), f.read(t, file))
	}
	if info, _ := os.Stat(f.path(file)); info.Mode().Perm() != 0644 {
		t.Fatal(info.Mode())
	}
	for _, dryrun := range []bool{false, true} {
		if r := (Runtime{Dryrun: dryrun}).SysctlSetting(ctx, task); r.Status != "succeeded" || r.Change != "unchanged" || r.Value != nil {
			t.Fatalf("dryrun %v: %+v", dryrun, r)
		}
	}

	// The kernel shows a tab where the task writes a space: the words match,
	// so only MMX's file is written.
	f.set(t, "proc/sys/net/ipv4/ip_local_port_range", "32768\t60999\n")
	ports := SysctlSetting{Name: "net.ipv4.ip_local_port_range", Value: "32768 60999"}
	if r := (Runtime{}).SysctlSetting(ctx, ports); r.Change != "changed" || r.Value != nil || f.read(t, "proc/sys/net/ipv4/ip_local_port_range") != "32768\t60999\n" {
		t.Fatalf("%+v", r)
	}
	if r := (Runtime{}).SysctlSetting(ctx, ports); r.Change != "unchanged" {
		t.Fatalf("%+v", r)
	}
	ports.Value = "1024  65000"
	if r := (Runtime{}).SysctlSetting(ctx, ports); r.Change != "changed" || !reflect.DeepEqual(r.Value, map[string]any{"live_value_before": "32768 60999", "live_value_after": "1024  65000"}) {
		t.Fatalf("%+v", r)
	}
}

func TestSysctlSettingNamesWithSlashes(t *testing.T) {
	ctx := context.Background()
	f := newKernelFixture(t)
	f.set(t, "proc/sys/net/ipv4/conf/eth0.10/rp_filter", "1\n")
	f.set(t, "proc/sys/net/ipv4/conf/eth0/rp_filter", "1\n")
	if r := (Runtime{}).SysctlSetting(ctx, SysctlSetting{Name: "net/ipv4/conf/eth0.10/rp_filter", Value: "2"}); r.Status != "succeeded" || r.Change != "changed" {
		t.Fatalf("%+v", r)
	}
	if f.read(t, "proc/sys/net/ipv4/conf/eth0.10/rp_filter") != "2" || f.read(t, "proc/sys/net/ipv4/conf/eth0/rp_filter") != "1\n" ||
		f.read(t, "etc/sysctl.d/90-mmx-net.ipv4.conf.eth0.10.rp_filter.conf") != "net/ipv4/conf/eth0.10/rp_filter = 2\n" {
		t.Fatal("the slash form set another setting")
	}
	for name, key := range map[string]string{
		"net.ipv4.ip_forward": "net/ipv4/ip_forward", "net/ipv4/conf/eth0.10/rp_filter": "net/ipv4/conf/eth0.10/rp_filter",
		"net.ipv4.conf.eth0/10.rp_filter": "net/ipv4/conf/eth0.10/rp_filter", "kernel//pid_max": "kernel/pid_max",
	} {
		if got := sysctlKey(name); got != key {
			t.Errorf("%s: %s", name, got)
		}
	}
}

func TestSysctlSettingTheKernelLacks(t *testing.T) {
	ctx := context.Background()
	f := newKernelFixture(t)
	task := SysctlSetting{Name: "net.bridge.bridge-nf-call-iptables", Value: "1"}
	if r := (Runtime{Dryrun: true}).SysctlSetting(ctx, task); r.Status != "not_checked" || !strings.Contains(r.Reason, "kernel_module") {
		t.Fatalf("%+v", r)
	}
	r := (Runtime{}).SysctlSetting(ctx, task)
	if r.Status != "failed" || !strings.Contains(r.Reason, "the kernel has no setting net.bridge.bridge-nf-call-iptables") || !strings.Contains(r.Reason, "check the setting name") {
		t.Fatalf("%+v", r)
	}
	if f.exists("etc/sysctl.d/90-mmx-net.bridge.bridge-nf-call-iptables.conf") {
		t.Fatal("kept a setting the kernel lacks in a file")
	}
	// Once the module that adds it is loaded, the setting exists.
	f.set(t, "proc/sys/net/bridge/bridge-nf-call-iptables", "0\n")
	if r := (Runtime{}).SysctlSetting(ctx, task); r.Status != "succeeded" || r.Change != "changed" {
		t.Fatalf("%+v", r)
	}
	f.set(t, "proc/sys/net/core/somaxconn", "4096\n")
	if r := (Runtime{}).SysctlSetting(ctx, SysctlSetting{Name: "net.core", Value: "1"}); r.Status != "failed" || !strings.Contains(r.Reason, "group of kernel settings") {
		t.Fatalf("%+v", r)
	}
}

func TestSysctlSettingTheKernelRefuses(t *testing.T) {
	ctx := context.Background()
	f := newKernelFixture(t)
	f.set(t, "proc/sys/net/ipv4/ip_forward", "0\n")
	writeKernelSetting = func(string, string) error {
		return errors.New("write /proc/sys/net/ipv4/ip_forward: read-only file system")
	}
	task := SysctlSetting{Name: "net.ipv4.ip_forward", Value: "1"}
	r := (Runtime{}).SysctlSetting(ctx, task)
	if r.Status != "failed" || r.FailureKind != "config" || r.Change == "changed" || r.Reason != "the kernel refused net.ipv4.ip_forward = 1: write /proc/sys/net/ipv4/ip_forward: read-only file system" {
		t.Fatalf("%+v", r)
	}
	if f.exists("etc/sysctl.d/90-mmx-net.ipv4.ip_forward.conf") {
		t.Fatal("kept a refused value for the next boot")
	}
	// The kernel takes the write but keeps another value, as a setting that
	// rounds or clamps does.
	writeKernelSetting = func(path, _ string) error { return os.WriteFile(path, []byte("2\n"), 0644) }
	r = (Runtime{}).SysctlSetting(ctx, task)
	if r.Status != "failed" || r.Change != "changed" || !strings.Contains(r.Reason, `now shows "2"`) {
		t.Fatalf("%+v", r)
	}
	if r := (Runtime{BeforeMutation: func(string) error { return errors.New("stopped") }}).SysctlSetting(ctx, task); r.Status != "failed" || r.Reason != "stopped" {
		t.Fatalf("%+v", r)
	}
}

func TestSysctlSettingAfterPartialWork(t *testing.T) {
	ctx := context.Background()
	f := newKernelFixture(t)
	f.set(t, "proc/sys/vm/swappiness", "60\n")
	os.Remove(f.path("etc/sysctl.d"))
	task := SysctlSetting{Name: "vm.swappiness", Value: "10"}
	r := (Runtime{}).SysctlSetting(ctx, task)
	if r.Status != "failed" || r.Change != "changed" || f.read(t, "proc/sys/vm/swappiness") != "10" {
		t.Fatalf("%+v", r)
	}
	// Another try finds the live value set and writes only the file.
	os.Mkdir(f.path("etc/sysctl.d"), 0755)
	if r := (Runtime{}).SysctlSetting(ctx, task); r.Status != "succeeded" || r.Change != "changed" || r.Value != nil || f.read(t, "etc/sysctl.d/90-mmx-vm.swappiness.conf") != "vm.swappiness = 10\n" {
		t.Fatalf("%+v", r)
	}
}

func TestSysctlSettingFailsWhenALaterFileSetsAnotherValue(t *testing.T) {
	ctx := context.Background()
	f := newKernelFixture(t)
	f.set(t, "proc/sys/net/ipv4/ip_forward", "0\n")
	task := SysctlSetting{Name: "net.ipv4.ip_forward", Value: "1"}
	fails := func(file string) {
		t.Helper()
		for _, dryrun := range []bool{true, false} {
			r := (Runtime{Dryrun: dryrun}).SysctlSetting(ctx, task)
			if r.Status != "failed" || r.FailureKind != "config" || !strings.HasPrefix(r.Reason, f.path(file)+" sets the managed kernel setting to") || !strings.Contains(r.Reason, "90-mmx-net.ipv4.ip_forward.conf") {
				t.Fatalf("%s: %+v", file, r)
			}
		}
		if f.read(t, "proc/sys/net/ipv4/ip_forward") != "0\n" || f.exists("etc/sysctl.d/90-mmx-net.ipv4.ip_forward.conf") {
			t.Fatal("changed the node before the check")
		}
	}
	succeeds := func() {
		t.Helper()
		if r := (Runtime{Dryrun: true}).SysctlSetting(ctx, task); r.Status != "succeeded" {
			t.Fatalf("%+v", r)
		}
	}
	// A vendor file named after MMX's, in any directory, sets it later.
	f.set(t, "usr/lib/sysctl.d/95-vendor.conf", "# vendor\nnet.ipv4.ip_forward = 0\n")
	fails("usr/lib/sysctl.d/95-vendor.conf")
	// The same value there does not matter, and neither does a value of an
	// earlier file or a pattern, which an explicit setting overrides.
	f.set(t, "usr/lib/sysctl.d/95-vendor.conf", "net.ipv4.ip_forward=1\nnet.ipv4.conf.*.forwarding = 0\nnet.ipv4.ip_forward_x = 0\n")
	f.set(t, "usr/lib/sysctl.d/10-early.conf", "net.ipv4.ip_forward = 0\n")
	f.set(t, "run/sysctl.d/90-mmx-net.ipv4.ip_forward.conf", "net.ipv4.ip_forward = 0\n") // hidden by MMX's own name
	succeeds()
	// The last assignment counts; a dash, a slash name, and ; comments are read
	// as systemd-sysctl reads them.
	f.set(t, "run/sysctl.d/97-runtime.conf", "; comment\n-net/ipv4/ip_forward = 0\n")
	fails("run/sysctl.d/97-runtime.conf")
	f.set(t, "usr/lib/sysctl.d/98-later.conf", "net.ipv4.ip_forward = 1\n")
	succeeds()
	// A file in /etc/sysctl.d of the same name hides a later one, as a link
	// to /dev/null masks a vendor file.
	f.set(t, "usr/lib/sysctl.d/98-later.conf", "net.ipv4.ip_forward = 0\n")
	fails("usr/lib/sysctl.d/98-later.conf")
	if err := os.Symlink(os.DevNull, f.path("etc/sysctl.d/98-later.conf")); err != nil {
		t.Fatal(err)
	}
	fails("run/sysctl.d/97-runtime.conf")
	f.set(t, "etc/sysctl.d/97-runtime.conf", "")
	succeeds()
	// /etc/sysctl.conf comes last, through the link 99-sysctl.conf where the
	// distribution has it, and otherwise as sysctl --system reads it.
	f.set(t, "etc/sysctl.conf", "net.ipv4.ip_forward = 0\n")
	fails("etc/sysctl.conf")
	if err := os.Symlink("../sysctl.conf", f.path("etc/sysctl.d/99-sysctl.conf")); err != nil {
		t.Fatal(err)
	}
	fails("etc/sysctl.d/99-sysctl.conf")
	f.set(t, "etc/sysctl.conf", "net.ipv4.ip_forward = 1\n")
	succeeds()
	if r := (Runtime{}).SysctlSetting(ctx, task); r.Status != "succeeded" || r.Change != "changed" {
		t.Fatalf("%+v", r)
	}
}

func TestSysctlSettingAbsentDeletesOnlyMMXsFile(t *testing.T) {
	ctx := context.Background()
	f := newKernelFixture(t)
	f.set(t, "proc/sys/net/ipv4/ip_forward", "1\n")
	f.set(t, "etc/sysctl.d/90-mmx-net.ipv4.ip_forward.conf", "net.ipv4.ip_forward = 1\n")
	task := SysctlSetting{Name: "net.ipv4.ip_forward", State: "boot_setting_removed_live_value_kept"}
	if r := (Runtime{Dryrun: true}).SysctlSetting(ctx, task); r.Change != "would_change" || r.FileChange == nil || r.FileChange.Action != "remove" || !f.exists("etc/sysctl.d/90-mmx-net.ipv4.ip_forward.conf") {
		t.Fatalf("%+v", r)
	}
	if r := (Runtime{}).SysctlSetting(ctx, task); r.Status != "succeeded" || r.Change != "changed" || f.exists("etc/sysctl.d/90-mmx-net.ipv4.ip_forward.conf") {
		t.Fatalf("%+v", r)
	}
	if f.read(t, "proc/sys/net/ipv4/ip_forward") != "1\n" {
		t.Fatal("absent changed the live value")
	}
	// A setting the kernel no longer has is no reason to keep the file.
	if r := (Runtime{}).SysctlSetting(ctx, SysctlSetting{Name: "net.bridge.bridge-nf-call-iptables", State: "boot_setting_removed_live_value_kept"}); r.Status != "succeeded" || r.Change != "unchanged" {
		t.Fatalf("%+v", r)
	}
}

func TestSysctlSettingInputs(t *testing.T) {
	t.Parallel()
	for task, want := range map[SysctlSetting]string{
		{Value: "1"}:                                 "sysctl_setting.name is required",
		{Name: "net.ipv4.ip_forward"}:                "sysctl_setting.value is required",
		{Name: "net.ipv4.ip_forward", State: "gone"}: "sysctl_setting.state must be present or boot_setting_removed_live_value_kept",
		{Name: "net.ipv4.ip_forward", Value: "1", State: "boot_setting_removed_live_value_kept"}: "boot_setting_removed_live_value_kept sysctl_setting accepts only name and state",
	} {
		if err := ValidateSysctlSetting(task); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%+v: %v", task, err)
		}
		if r := (Runtime{}).SysctlSetting(context.Background(), task); r.Status != "failed" || r.FailureKind != "config" {
			t.Errorf("%+v: %+v", task, r)
		}
	}
	for _, name := range []string{"net.ipv4.ip_forward", "net/ipv4/conf/eth0.10/rp_filter", "net.bridge.bridge-nf-call-iptables", "net/ipv4/ip_forward"} {
		if err := sysctlNameRule(name); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, name := range []string{"", "/proc/sys/net/ipv4/ip_forward", "net..ipv4", "net.ipv4.", "net/../../etc/passwd", "net/./ipv4", "net.ipv4/conf", "net.ipv4.conf.*.rp_filter", "net ipv4", "net.ipv4.ip_forward=1", strings.Repeat("a", 241)} {
		if err := sysctlNameRule(name); err == nil {
			t.Errorf("accepted %q", name)
		}
	}
	for value, ok := range map[string]bool{"1": true, "32768 60999": true, "|/usr/lib/systemd/systemd-coredump %P": true, "": false, "  ": false, "1\n2": false} {
		if err := sysctlValueRule(value); (err == nil) != ok {
			t.Errorf("%q: %v", value, err)
		}
	}
}
