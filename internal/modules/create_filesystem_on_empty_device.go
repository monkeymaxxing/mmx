package modules

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"syscall"

	"mmx/internal/protocol"
)

// devicesMustBeBlockDevices is false only in tests, which format a regular
// file because they cannot make a block device.
var devicesMustBeBlockDevices = true

// CreateFilesystemOnEmptyDevice creates a filesystem on a device that holds
// nothing yet. Its one job is to never destroy data.
type CreateFilesystemOnEmptyDevice struct {
	Device         string   `yaml:"device" json:"device"`
	FilesystemType string   `yaml:"filesystem_type" json:"filesystem_type"`
	MkfsOptions    []string `yaml:"mkfs_options,omitempty" json:"mkfs_options,omitempty"`
}

// mkfsForceOptions are the options with which each mkfs program writes over
// what it finds on a device. The module never passes one, and refuses them in
// mkfs_options: short options as single letters, long ones whole.
var mkfsForceOptions = map[string][]string{
	"ext2": {"F"}, "ext3": {"F"}, "ext4": {"F"}, "xfs": {"f"}, "btrfs": {"f", "--force"}, "vfat": {"I"}, "f2fs": {"f"},
}

// deviceSignature is one signature wipefs --no-act finds: a filesystem, swap,
// a partition table, an LVM physical volume, a RAID member, an encrypted
// volume, and anything else libblkid knows.
type deviceSignature struct {
	Type   string `json:"type"`
	Offset string `json:"offset"`
}

// probeDevice lists every signature on the device. wipefs --no-act probes
// for all of them, including those with a bad checksum, and changes nothing.
func (run Runtime) probeDevice(ctx context.Context, device string) ([]deviceSignature, error) {
	out := run.RunCommand(ctx, []string{"wipefs", "--no-act", "--json", device}, "")
	if out.Error != "" || out.ExitCode != 0 {
		return nil, fmt.Errorf("wipefs --no-act could not probe %s: %s", device, strings.TrimSpace(out.Error+" "+out.StandardError))
	}
	if strings.TrimSpace(out.StandardOutput) == "" {
		return nil, nil
	}
	var listing struct {
		Signatures []deviceSignature `json:"signatures"`
	}
	if err := json.Unmarshal([]byte(out.StandardOutput), &listing); err != nil {
		return nil, fmt.Errorf("wipefs --no-act returned unreadable device information for %s: %w", device, err)
	}
	return listing.Signatures, nil
}

// describeSignatures names what was found, in plain words, with the type
// wipefs gives and where it sits.
func describeSignatures(signatures []deviceSignature) string {
	kinds := map[string]string{
		"swap": "swap area", "LVM2_member": "LVM physical volume", "linux_raid_member": "Linux RAID member",
		"crypto_LUKS": "LUKS encrypted volume", "gpt": "GPT partition table", "dos": "MBR partition table", "PMBR": "protective MBR of a GPT partition table",
	}
	var parts []string
	for _, s := range signatures {
		kind, ok := kinds[s.Type]
		if !ok {
			kind = s.Type + " filesystem"
		}
		parts = append(parts, fmt.Sprintf("%s (%s signature at offset %s)", kind, s.Type, s.Offset))
	}
	return strings.Join(parts, ", ")
}

// CreateFilesystemOnEmptyDevice probes the device: with no signature of any
// kind it creates the filesystem, with only the requested type it is
// unchanged, and with anything else it fails naming what it found. It never
// passes a force option.
func (run Runtime) CreateFilesystemOnEmptyDevice(ctx context.Context, c CreateFilesystemOnEmptyDevice) protocol.Result {
	if err := ValidateCreateFilesystemOnEmptyDevice(c); err != nil {
		return protocol.ConfigurationError(err)
	}
	info, err := os.Stat(c.Device)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return run.notYet(fmt.Errorf("the device %s does not exist yet", c.Device))
	case err != nil:
		return protocol.Failed(err)
	case devicesMustBeBlockDevices && (info.Mode()&os.ModeDevice == 0 || info.Mode()&os.ModeCharDevice != 0):
		return refused(fmt.Errorf("%s is not a block device", c.Device))
	}
	if r := run.storageTool(ctx, "wipefs"); r != nil {
		return *r
	}
	signatures, err := run.probeDevice(ctx, c.Device)
	if err != nil {
		return protocol.Failed(err)
	}
	if len(signatures) > 0 {
		for _, s := range signatures {
			if s.Type != c.FilesystemType {
				return refused(fmt.Errorf("%s is not empty; found: %s. create_filesystem_on_empty_device creates %s only on a device that holds nothing, and never destroys data. To use this device, first check that nothing on it is needed, then wipe it by hand, such as with wipefs --all %s",
					c.Device, describeSignatures(signatures), c.FilesystemType, c.Device))
			}
		}
		return protocol.Success("unchanged")
	}
	mkfs := "mkfs." + c.FilesystemType
	if _, err := lookPath(ctx, mkfs); err != nil {
		return run.notYet(missingProgramError(mkfs))
	}
	// A device that holds no signature can still be in use: mounted, an
	// active swap area, or held by device mapper or RAID. The kernel then
	// refuses an exclusive open.
	if f, err := os.OpenFile(c.Device, os.O_RDONLY|syscall.O_EXCL, 0); err != nil {
		if errors.Is(err, syscall.EBUSY) {
			return refused(fmt.Errorf("%s has no device signature but is in use as a mounted filesystem, an active swap area, or part of another device such as LVM or RAID; no changes were made", c.Device))
		}
		return protocol.Failed(err)
	} else {
		f.Close()
	}
	if run.Dryrun {
		r := protocol.Success("would_change")
		r.Reason = "would create " + c.FilesystemType + " on " + c.Device
		return r
	}
	out := run.RunCommandThatChangesTheNode(ctx, append(append([]string{mkfs}, c.MkfsOptions...), c.Device), "")
	if out.Error != "" || out.ExitCode != 0 {
		return protocol.Failed(fmt.Errorf("%s %s failed: %s", mkfs, c.Device, strings.TrimSpace(out.Error+" "+out.StandardError+" "+out.StandardOutput)))
	}
	after, err := run.probeDevice(ctx, c.Device)
	if err == nil && (len(after) == 0 || slices.ContainsFunc(after, func(s deviceSignature) bool { return s.Type != c.FilesystemType })) {
		err = fmt.Errorf("%s reported success, but %s holds %s", mkfs, c.Device, cmp.Or(describeSignatures(after), "no signature"))
	}
	if err != nil {
		r := protocol.Failed(err)
		r.Change = "changed"
		return r
	}
	r := protocol.Success("changed")
	r.Reason = "created " + c.FilesystemType + " on " + c.Device
	return r
}

func ValidateCreateFilesystemOnEmptyDevice(c CreateFilesystemOnEmptyDevice) error {
	if c.Device == "" {
		return invalid("device", "create_filesystem_on_empty_device.device is required: the block device, such as /dev/sdb1 or /dev/disk/by-id/...")
	}
	if !filesystemTypeName.MatchString(c.FilesystemType) {
		return invalid("filesystem_type", "create_filesystem_on_empty_device.filesystem_type is required: the filesystem as its mkfs program is named, such as ext4 for mkfs.ext4 or xfs for mkfs.xfs")
	}
	force := mkfsForceOptions[c.FilesystemType]
	for i, option := range c.MkfsOptions {
		field := fmt.Sprintf("mkfs_options[%d]", i)
		if option == "" || strings.ContainsAny(option, "\x00\n") {
			return invalid(field, "each mkfs option is one item of text, such as -L or data")
		}
		forced := slices.Contains(force, option)
		if !strings.HasPrefix(option, "--") && strings.HasPrefix(option, "-") {
			for _, letter := range force {
				forced = forced || (len(letter) == 1 && strings.Contains(option[1:], letter))
			}
		}
		if forced {
			return invalid(field, fmt.Sprintf("%s makes mkfs.%s overwrite existing device data; create_filesystem_on_empty_device rejects force options and formats only an empty device", option, c.FilesystemType))
		}
	}
	return nil
}

var filesystemTypeName = regexp.MustCompile(`^[a-z0-9][a-z0-9_]*$`)

// createFilesystemOnEmptyDeviceDescription is what mmx describe-module shows.
var createFilesystemOnEmptyDeviceDescription = Description{
	Name: "create_filesystem_on_empty_device", Topic: TopicSwapFilesystemsAndMounts, Schema: reflect.TypeOf(CreateFilesystemOnEmptyDevice{}),
	Summary: "a filesystem on a block device that holds nothing yet, made with mkfs.<filesystem_type>; a device that holds anything else fails the task, and nothing ever forces it",
	Fields: []Field{
		{Name: "device", Required: true, Notes: "absolute path of the block device, such as /dev/sdb1; wipefs --no-act (package util-linux) probes it for every kind of signature"},
		{Name: "filesystem_type", Required: true, Notes: "the filesystem as its mkfs program is named, such as ext4 (mkfs.ext4, package e2fsprogs) or xfs (mkfs.xfs, package xfsprogs); a device that already holds only this type is unchanged"},
		{Name: "mkfs_options", Type: "list", Notes: "options passed to mkfs.<filesystem_type> before the device, such as [-L, data]; a force option is refused"},
	},
	Example: "- create_filesystem_on_empty_device:\n    device: /dev/sdb1\n    filesystem_type: ext4\n",
	Results: resultBasic,
	Dryrun:  "supported; probes the device and reports \"would create ext4 on /dev/sdb1\" or unchanged; not checked while the device or a program is missing",
}

// Prepare checks the inputs when the project loads.
func (c *CreateFilesystemOnEmptyDevice) Prepare(base string) error {
	return ValidateCreateFilesystemOnEmptyDevice(*c)
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (c *CreateFilesystemOnEmptyDevice) Arguments(arguments *ArgumentList) {
	arguments.Public("device", &c.Device, absolute)
}

// CleanPaths cleans the resolved path.
func (c *CreateFilesystemOnEmptyDevice) CleanPaths() {
	c.Device = filepath.Clean(c.Device)
}

// Run does the module's work on the node.
func (c *CreateFilesystemOnEmptyDevice) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.Runtime.CreateFilesystemOnEmptyDevice(ctx, *c)
}
