package modules

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
)

func moduleOptionText(value string) *string { return &value }

func TestKernelModuleLoadsNowAndAtBoot(t *testing.T) {
	ctx := context.Background()
	f := newKernelFixture(t)
	f.set(t, "available/br_netfilter", "")
	task := KernelModule{Name: "br_netfilter"}

	preview := (Runtime{Dryrun: true}).KernelModule(ctx, task)
	if preview.Status != "succeeded" || preview.Change != "would_change" || !strings.Contains(preview.Diff, "+br_netfilter") ||
		!reflect.DeepEqual(preview.Value, map[string]any{"loaded_before": false, "loaded_after": true, "files_changed": []string{f.path("etc/modules-load.d/mmx-br_netfilter.conf")}}) {
		t.Fatalf("%+v", preview)
	}
	if calls := f.changes(t); len(calls) != 0 || f.exists("etc/modules-load.d/mmx-br_netfilter.conf") {
		t.Fatal("a dryrun changed the node", calls)
	}
	r := (Runtime{}).KernelModule(ctx, task)
	if r.Status != "succeeded" || r.Change != "changed" || !reflect.DeepEqual(r.Value, preview.Value) || r.Diff != preview.Diff {
		t.Fatalf("%+v", r)
	}
	if calls := f.changes(t); !reflect.DeepEqual(calls, []string{"modprobe br_netfilter"}) || !strings.Contains(f.read(t, "proc/modules"), "br_netfilter ") ||
		f.read(t, "etc/modules-load.d/mmx-br_netfilter.conf") != "br_netfilter\n" || f.exists("etc/modprobe.d/mmx-br_netfilter.conf") {
		t.Fatal(calls, f.read(t, "proc/modules"))
	}
	for _, dryrun := range []bool{false, true} {
		if r := (Runtime{Dryrun: dryrun}).KernelModule(ctx, task); r.Status != "succeeded" || r.Change != "unchanged" || r.Value != nil || r.Diff != "" {
			t.Fatalf("dryrun %v: %+v", dryrun, r)
		}
	}
	// modprobe and the kernel write a dash as an underscore.
	if r := (Runtime{}).KernelModule(ctx, KernelModule{Name: "br-netfilter"}); r.Change != "unchanged" || len(f.changes(t)) != 0 {
		t.Fatalf("%+v", r)
	}
}

func TestKernelModuleOptions(t *testing.T) {
	ctx := context.Background()
	f := newKernelFixture(t)
	f.set(t, "available/vxlan", "udp_port 8472\nlog_ecn_error Y\n")
	task := KernelModule{Name: "vxlan", Options: map[string]*string{"udp_port": moduleOptionText("4789"), "log_ecn_error": moduleOptionText("0")}}
	r := (Runtime{}).KernelModule(ctx, task)
	if r.Status != "succeeded" || r.Change != "changed" || len(r.Value.(map[string]any)["files_changed"].([]string)) != 2 {
		t.Fatalf("%+v", r)
	}
	if f.read(t, "etc/modprobe.d/mmx-vxlan.conf") != "options vxlan log_ecn_error=0 udp_port=4789\n" ||
		f.read(t, "sys/module/vxlan/parameters/udp_port") != "4789\n" || f.read(t, "sys/module/vxlan/parameters/log_ecn_error") != "0\n" {
		t.Fatal(f.read(t, "etc/modprobe.d/mmx-vxlan.conf"))
	}
	// The kernel shows a yes-or-no option as Y or N.
	f.set(t, "sys/module/vxlan/parameters/log_ecn_error", "N\n")
	if r := (Runtime{}).KernelModule(ctx, task); r.Status != "succeeded" || r.Change != "unchanged" {
		t.Fatalf("%+v", r)
	}

	// A loaded module keeps its options: the task writes them and fails,
	// saying to unload or reboot, and never unloads the module itself.
	f.changes(t)
	other := KernelModule{Name: "vxlan", Options: map[string]*string{"udp_port": moduleOptionText("8472")}}
	preview := (Runtime{Dryrun: true}).KernelModule(ctx, other)
	if preview.Status != "failed" || preview.Change != "would_change" || !strings.Contains(preview.Reason, "udp_port is 4789, not 8472") || !strings.Contains(preview.Reason, "MMX would write the requested options") ||
		f.read(t, "etc/modprobe.d/mmx-vxlan.conf") != "options vxlan log_ecn_error=0 udp_port=4789\n" {
		t.Fatalf("%+v", preview)
	}
	r = (Runtime{}).KernelModule(ctx, other)
	if r.Status != "failed" || r.FailureKind != "config" || r.Change != "changed" || !strings.Contains(r.Reason, "modprobe -r vxlan") || !strings.Contains(r.Reason, "reboot the node") ||
		f.read(t, "etc/modprobe.d/mmx-vxlan.conf") != "options vxlan udp_port=8472\n" || len(f.changes(t)) != 0 {
		t.Fatalf("%+v", r)
	}
	if r := (Runtime{}).KernelModule(ctx, other); r.Status != "failed" || r.Change == "changed" {
		t.Fatalf("%+v", r)
	}
	// Unloaded and loaded again, the module uses the new options.
	if r := (Runtime{}).KernelModule(ctx, KernelModule{Name: "vxlan", State: "unloaded"}); r.Status != "succeeded" || r.Change != "changed" {
		t.Fatalf("%+v", r)
	}
	if r := (Runtime{}).KernelModule(ctx, other); r.Status != "succeeded" || r.Change != "changed" || f.read(t, "sys/module/vxlan/parameters/udp_port") != "8472\n" {
		t.Fatalf("%+v", r)
	}
	// Omitting options preserves the saved file in runs and previews.
	for _, dryrun := range []bool{true, false} {
		r := (Runtime{Dryrun: dryrun}).KernelModule(ctx, KernelModule{Name: "vxlan"})
		if r.Status != "succeeded" || r.Change != "unchanged" || f.read(t, "etc/modprobe.d/mmx-vxlan.conf") != "options vxlan udp_port=8472\n" {
			t.Fatalf("%+v", r)
		}
	}
	// An explicit empty mapping deletes the saved file.
	if r := (Runtime{}).KernelModule(ctx, KernelModule{Name: "vxlan", Options: map[string]*string{}}); r.Change != "changed" || f.exists("etc/modprobe.d/mmx-vxlan.conf") || !strings.Contains(r.Diff, "remove "+f.path("etc/modprobe.d/mmx-vxlan.conf")) {
		t.Fatalf("%+v", r)
	}
	// Another options line that modprobe also reads wins over MMX's.
	f.set(t, "etc/modprobe.d/zz-site.conf", "options vxlan udp_port=1\n")
	if r := (Runtime{}).KernelModule(ctx, KernelModule{Name: "vxlan", State: "unloaded"}); r.Status != "succeeded" {
		t.Fatalf("%+v", r)
	}
	if r := (Runtime{}).KernelModule(ctx, other); r.Status != "failed" || r.Change != "changed" || !strings.Contains(r.Reason, "another options line") {
		t.Fatalf("%+v", r)
	}
}

func TestKernelModuleBuiltIntoTheKernel(t *testing.T) {
	ctx := context.Background()
	f := newKernelFixture(t)
	task := KernelModule{Name: "loop"}
	if r := (Runtime{}).KernelModule(ctx, task); r.Status != "succeeded" || r.Change != "changed" || r.Value.(map[string]any)["loaded_after"] != nil {
		t.Fatalf("%+v", r)
	}
	if r := (Runtime{}).KernelModule(ctx, task); r.Change != "unchanged" || len(f.changes(t)) != 0 {
		t.Fatalf("%+v", r)
	}
	r := (Runtime{}).KernelModule(ctx, KernelModule{Name: "loop", Options: map[string]*string{"max_loop": moduleOptionText("8")}})
	if r.Status != "failed" || !strings.Contains(r.Reason, "kernel command line instead, as loop.max_loop=8") || f.exists("etc/modprobe.d/mmx-loop.conf") {
		t.Fatalf("%+v", r)
	}
	r = (Runtime{}).KernelModule(ctx, KernelModule{Name: "loop", State: "unloaded"})
	if r.Status != "failed" || !strings.Contains(r.Reason, "built into the kernel and cannot be unloaded") || !f.exists("etc/modules-load.d/mmx-loop.conf") {
		t.Fatalf("%+v", r)
	}
	// Without modules.builtin, as in a container, nothing counts as built in.
	os.Remove(f.path("lib/modules/6.1.0-test/modules.builtin"))
	if r := (Runtime{Dryrun: true}).KernelModule(ctx, task); r.Status != "not_checked" {
		t.Fatalf("%+v", r)
	}
}

func TestKernelModuleThatDoesNotExistOrDoesNotLoad(t *testing.T) {
	ctx := context.Background()
	f := newKernelFixture(t)
	task := KernelModule{Name: "br_netfiltr"}
	if r := (Runtime{Dryrun: true}).KernelModule(ctx, task); r.Status != "not_checked" || !strings.Contains(r.Reason, "Module br_netfiltr not found") {
		t.Fatalf("%+v", r)
	}
	r := (Runtime{}).KernelModule(ctx, task)
	if r.Status != "failed" || r.Reason != "modprobe cannot load the kernel module br_netfiltr for the running kernel: modprobe: FATAL: Module br_netfiltr not found in directory /lib/modules/6.1.0-test" {
		t.Fatalf("%+v", r)
	}
	if f.exists("etc/modules-load.d/mmx-br_netfiltr.conf") {
		t.Fatal("kept a module that does not exist for the next boot")
	}
	// An install line that runs /bin/true keeps a module from loading.
	f.set(t, "available/usb_storage", "")
	f.set(t, "blocked/usb_storage", "")
	r = (Runtime{}).KernelModule(ctx, KernelModule{Name: "usb_storage"})
	if r.Status != "failed" || r.Change != "changed" || !strings.Contains(r.Reason, "install usb_storage /bin/true") {
		t.Fatalf("%+v", r)
	}
}

func TestKernelModuleUnloaded(t *testing.T) {
	ctx := context.Background()
	f := newKernelFixture(t)
	f.set(t, "available/br_netfilter", "")
	f.set(t, "available/vxlan", "udp_port 8472\n")
	for _, m := range []KernelModule{{Name: "br_netfilter"}, {Name: "vxlan", Options: map[string]*string{"udp_port": moduleOptionText("4789")}}} {
		if r := (Runtime{}).KernelModule(ctx, m); r.Status != "succeeded" {
			t.Fatalf("%+v", r)
		}
	}
	f.changes(t)
	task := KernelModule{Name: "vxlan", State: "unloaded"}
	preview := (Runtime{Dryrun: true}).KernelModule(ctx, task)
	if preview.Change != "would_change" || preview.Value.(map[string]any)["loaded_after"] != false || len(preview.Value.(map[string]any)["files_changed"].([]string)) != 2 ||
		len(f.changes(t)) != 0 || !f.exists("etc/modprobe.d/mmx-vxlan.conf") {
		t.Fatalf("%+v", preview)
	}
	if r := (Runtime{}).KernelModule(ctx, task); r.Status != "succeeded" || r.Change != "changed" || !reflect.DeepEqual(f.changes(t), []string{"modprobe -r vxlan"}) {
		t.Fatalf("%+v", r)
	}
	if strings.Contains(f.read(t, "proc/modules"), "vxlan") || f.exists("etc/modprobe.d/mmx-vxlan.conf") || f.exists("etc/modules-load.d/mmx-vxlan.conf") {
		t.Fatal("vxlan is still there")
	}
	for _, dryrun := range []bool{false, true} {
		if r := (Runtime{Dryrun: dryrun}).KernelModule(ctx, task); r.Change != "unchanged" || r.Value != nil {
			t.Fatalf("%+v", r)
		}
	}
	// A module in use fails with modprobe's message, after MMX's files are gone.
	f.set(t, "in_use/br_netfilter", "")
	r := (Runtime{}).KernelModule(ctx, KernelModule{Name: "br_netfilter", State: "unloaded"})
	if r.Status != "failed" || r.Change != "changed" || !strings.Contains(r.Reason, "modprobe: FATAL: Module br_netfilter is in use.") || f.exists("etc/modules-load.d/mmx-br_netfilter.conf") {
		t.Fatalf("%+v", r)
	}
	// Another try, once nothing uses it, unloads it.
	os.Remove(f.path("in_use/br_netfilter"))
	if r := (Runtime{}).KernelModule(ctx, KernelModule{Name: "br_netfilter", State: "unloaded"}); r.Status != "succeeded" || r.Change != "changed" || r.Value.(map[string]any)["files_changed"] != nil {
		t.Fatalf("%+v", r)
	}
}

func TestKernelModuleWithoutModprobe(t *testing.T) {
	ctx := context.Background()
	newKernelFixture(t)
	t.Setenv("PATH", t.TempDir())
	for _, dryrun := range []bool{true, false} {
		r := (Runtime{Dryrun: dryrun}).KernelModule(ctx, KernelModule{Name: "br_netfilter"})
		if (dryrun && r.Status != "not_checked") || (!dryrun && r.Status != "failed") || r.Reason != missingProgramError("modprobe").Error() {
			t.Fatalf("%+v", r)
		}
	}
}

func TestKernelModuleInputs(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		task KernelModule
		want string
	}{
		{KernelModule{}, "kernel_module.name is required"},
		{KernelModule{Name: "vxlan", State: "present"}, "kernel_module.state must be loaded or unloaded"},
		{KernelModule{Name: "vxlan", State: "unloaded", Options: map[string]*string{"udp_port": moduleOptionText("1")}}, "kernel_module with state: unloaded accepts only name and state"},
		{KernelModule{Name: "vxlan", Options: map[string]*string{"udp port": moduleOptionText("1")}}, `option "udp port" must be an option name`},
		{KernelModule{Name: "vxlan", Options: map[string]*string{"udp_port": nil}}, "kernel_module option udp_port needs a value"},
	} {
		if err := ValidateKernelModule(c.task); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: %v", c.task, err)
		}
		if r := (Runtime{}).KernelModule(context.Background(), c.task); r.Status != "failed" || r.FailureKind != "config" {
			t.Errorf("%+v: %+v", c.task, r)
		}
	}
	for name, ok := range map[string]bool{"br_netfilter": true, "nf-conntrack": true, "": false, "../x": false, "a b": false} {
		if err := kernelModuleNameRule(name); (err == nil) != ok {
			t.Errorf("%q: %v", name, err)
		}
	}
	for value, ok := range map[string]bool{"65536": true, "1,2,3": true, "": false, "a b": false, `"x"`: false} {
		if err := kernelModuleOptionRule(value); (err == nil) != ok {
			t.Errorf("%q: %v", value, err)
		}
	}
	for text, want := range map[string]int{"1": 1, "Y": 1, "on": 1, "0": 0, "n": 0, "off": 0, "x": -1, "o": -1} {
		yes, ok := kernelBool(text)
		if got := map[bool]int{true: 1, false: 0}[yes]; !ok {
			got = -1
		} else if got != want {
			t.Errorf("%q: %v", text, yes)
		}
		if !ok && want != -1 {
			t.Errorf("%q not read", text)
		}
	}
}
