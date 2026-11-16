package modules

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// emptyDevice is a regular file that stands in for a block device; the
// stand-in wipefs reports the signatures the fixture keeps for it.
func emptyDevice(t *testing.T) (storageFixture, CreateFilesystemOnEmptyDevice) {
	t.Helper()
	f := newStorageFixture(t)
	previous := devicesMustBeBlockDevices
	devicesMustBeBlockDevices = false
	t.Cleanup(func() { devicesMustBeBlockDevices = previous })
	device := filepath.Join(f.dir, "sdb1")
	if err := os.WriteFile(device, make([]byte, 4096), 0600); err != nil {
		t.Fatal(err)
	}
	return f, CreateFilesystemOnEmptyDevice{Device: device, FilesystemType: "ext4", MkfsOptions: []string{"-L", "data"}}
}

func TestCreateFilesystemOnAnEmptyDevice(t *testing.T) {
	ctx := context.Background()
	f, c := emptyDevice(t)
	requireResult(t, (Runtime{Dryrun: true}).CreateFilesystemOnEmptyDevice(ctx, c), "succeeded", "would_change", "would create ext4 on "+c.Device)
	if calls := f.calls(t); len(calls) != 0 {
		t.Fatal(calls)
	}
	requireResult(t, (Runtime{}).CreateFilesystemOnEmptyDevice(ctx, c), "succeeded", "changed", "created ext4 on "+c.Device)
	if calls := f.calls(t); len(calls) != 1 || calls[0] != "mkfs.ext4 -L data "+c.Device {
		t.Fatal(calls)
	}
	for _, dryrun := range []bool{false, true} {
		requireResult(t, (Runtime{Dryrun: dryrun}).CreateFilesystemOnEmptyDevice(ctx, c), "succeeded", "unchanged", "")
	}
	if calls := f.calls(t); len(calls) != 0 {
		t.Fatal(calls)
	}
}

// Anything on the device other than the requested filesystem fails the task,
// in a dryrun too, naming what was found, and mkfs never runs.
func TestCreateFilesystemRefusesADeviceThatHoldsSomething(t *testing.T) {
	ctx := context.Background()
	f, c := emptyDevice(t)
	for _, tc := range []struct{ signatures, want string }{
		{`{"type": "xfs", "offset": "0x0"}`, "found: xfs filesystem (xfs signature at offset 0x0)"},
		{`{"type": "swap", "offset": "0xff6"}`, "found: swap area (swap signature at offset 0xff6)"},
		{`{"type": "gpt", "offset": "0x200"}, {"type": "PMBR", "offset": "0x1fe"}`, "found: GPT partition table (gpt signature at offset 0x200), protective MBR"},
		{`{"type": "LVM2_member", "offset": "0x218"}`, "found: LVM physical volume"},
		{`{"type": "ext4", "offset": "0x438"}, {"type": "dos", "offset": "0x1fe"}`, "found: ext4 filesystem (ext4 signature at offset 0x438), MBR partition table"},
	} {
		f.set(t, "signatures/sdb1", `{"signatures": [`+tc.signatures+`]}`)
		for _, dryrun := range []bool{true, false} {
			r := (Runtime{Dryrun: dryrun}).CreateFilesystemOnEmptyDevice(ctx, c)
			requireResult(t, r, "failed", "unchanged", c.Device+" is not empty; "+tc.want)
			if !strings.Contains(r.Reason, "never destroys data") || !strings.Contains(r.Reason, "wipefs --all "+c.Device) {
				t.Fatal(r.Reason)
			}
		}
	}
	if calls := f.calls(t); len(calls) != 0 {
		t.Fatal("mkfs ran:", calls)
	}
}

// A failed mkfs fails the task; the next try finds the device still empty
// and creates the filesystem.
func TestCreateFilesystemAfterAFailedMkfs(t *testing.T) {
	ctx := context.Background()
	f, c := emptyDevice(t)
	f.set(t, "mkfs.ext4-fails", "")
	requireResult(t, (Runtime{}).CreateFilesystemOnEmptyDevice(ctx, c), "failed", "unknown", "mkfs.ext4 "+c.Device+" failed: mkfs.ext4: injected failure")
	f.remove(t, "mkfs.ext4-fails")
	requireResult(t, (Runtime{}).CreateFilesystemOnEmptyDevice(ctx, c), "succeeded", "changed", "created ext4")
	refusing := Runtime{BeforeMutation: func(string) error { return os.ErrPermission }}
	c.Device = filepath.Join(f.dir, "sdc1")
	os.WriteFile(c.Device, nil, 0600)
	requireResult(t, refusing.CreateFilesystemOnEmptyDevice(ctx, c), "failed", "unknown", "permission denied")
}

func TestCreateFilesystemWaitsForTheDeviceAndPrograms(t *testing.T) {
	ctx := context.Background()
	f, c := emptyDevice(t)
	missing := c
	missing.Device = filepath.Join(f.dir, "sdz")
	requireResult(t, (Runtime{Dryrun: true}).CreateFilesystemOnEmptyDevice(ctx, missing), "not_checked", "unknown", "the device "+missing.Device+" does not exist yet")
	requireResult(t, (Runtime{}).CreateFilesystemOnEmptyDevice(ctx, missing), "failed", "unknown", "does not exist yet")
	// Only the fixture's stand-ins, so that a machine that has btrfs-progs
	// does not answer for them.
	t.Setenv("PATH", filepath.Join(f.dir, "bin"))
	for _, tc := range []struct{ filesystem, want string }{
		{"btrfs", missingProgramError("mkfs.btrfs").Error()},
		{"zfs", missingProgramError("mkfs.zfs").Error()},
	} {
		task := c
		task.FilesystemType, task.MkfsOptions = tc.filesystem, nil
		requireResult(t, (Runtime{Dryrun: true}).CreateFilesystemOnEmptyDevice(ctx, task), "not_checked", "unknown", tc.want)
		requireResult(t, (Runtime{}).CreateFilesystemOnEmptyDevice(ctx, task), "failed", "unknown", tc.want)
	}
	t.Setenv("PATH", t.TempDir())
	requireResult(t, (Runtime{Dryrun: true}).CreateFilesystemOnEmptyDevice(ctx, c), "not_checked", "unknown", missingProgramError("wipefs").Error())
	devicesMustBeBlockDevices = true
	requireResult(t, (Runtime{}).CreateFilesystemOnEmptyDevice(ctx, c), "failed", "unchanged", c.Device+" is not a block device")
}

func TestCreateFilesystemRejectsInvalidInput(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		c    CreateFilesystemOnEmptyDevice
		want string
	}{
		{CreateFilesystemOnEmptyDevice{FilesystemType: "ext4"}, "device is required"},
		{CreateFilesystemOnEmptyDevice{Device: "/dev/sdb1"}, "type is required"},
		{CreateFilesystemOnEmptyDevice{Device: "/dev/sdb1", FilesystemType: "Ext 4"}, "filesystem_type is required"},
		{CreateFilesystemOnEmptyDevice{Device: "/dev/sdb1", FilesystemType: "ext4", MkfsOptions: []string{"-F"}}, "-F makes mkfs.ext4 overwrite existing device data"},
		{CreateFilesystemOnEmptyDevice{Device: "/dev/sdb1", FilesystemType: "ext4", MkfsOptions: []string{"-qF"}}, "-qF makes mkfs.ext4 overwrite existing device data"},
		{CreateFilesystemOnEmptyDevice{Device: "/dev/sdb1", FilesystemType: "xfs", MkfsOptions: []string{"-f"}}, "-f makes mkfs.xfs overwrite existing device data"},
		{CreateFilesystemOnEmptyDevice{Device: "/dev/sdb1", FilesystemType: "btrfs", MkfsOptions: []string{"--force"}}, "--force makes mkfs.btrfs overwrite existing device data"},
		{CreateFilesystemOnEmptyDevice{Device: "/dev/sdb1", FilesystemType: "ext4", MkfsOptions: []string{""}}, "each mkfs option is one item"},
	} {
		r := (Runtime{}).CreateFilesystemOnEmptyDevice(context.Background(), tc.c)
		if r.Status != "failed" || r.FailureKind != "config" || !strings.Contains(r.Reason, tc.want) {
			t.Errorf("%+v: %+v", tc.c, r)
		}
	}
	// -f is not a force option of mkfs.ext4, and -L takes a label.
	if err := ValidateCreateFilesystemOnEmptyDevice(CreateFilesystemOnEmptyDevice{Device: "/dev/sdb1", FilesystemType: "ext4", MkfsOptions: []string{"-L", "Fast", "-E", "lazy_itable_init=0"}}); err != nil {
		t.Fatal(err)
	}
}
