package config

import (
	"strings"
	"testing"
)

// The storage modules load from their written form; each rejected input
// names its field and what to write.
func TestStorageModulesLoad(t *testing.T) {
	valid := []string{
		"- swap_file:\n    path: /swapfile\n    size: 4GiB\n",
		"- swap_file:\n    path: /swapfile\n    state: absent\n",
		"- swap_file:\n    path: \"/swap/{{ .node.name }}\"\n    size: 512MiB\n",
		"- create_filesystem_on_empty_device:\n    device: /dev/sdb1\n    filesystem_type: xfs\n    mkfs_options: [-L, data]\n",
		"- fstab_mount:\n    mount_point: /srv/data\n    mount_source: UUID=0a1b2c3d\n    filesystem_type: ext4\n    options: [noatime, nodev]\n    check_filesystem_at_boot: after_root_filesystem\n    state: mounted\n",
		"- fstab_mount:\n    check_filesystem_at_boot: never\n    options: [defaults]\n    mount_point: /srv/data\n    mount_source: nfs01:/export/data\n    filesystem_type: nfs\n    state: mounted\n",
		"- fstab_mount:\n    mount_point: /srv/data\n    state: unmounted_and_removed_from_fstab\n",
	}
	for _, tasks := range valid {
		if err := loadExample(t, tasks); err != nil {
			t.Errorf("%s: %v", tasks, err)
		}
	}
	for _, tc := range []struct{ tasks, want string }{
		{"- swap_file:\n    path: /swapfile\n", "size is required"},
		{"- swap_file:\n    path: /swapfile\n    size: 0MiB\n", "size must be a whole number"},
		{"- swap_file:\n    path: /swapfile\n    size: 4MiBG\n", "size"},
		{"- swap_file:\n    path: swapfile\n    size: 1MiB\n", "path must be an absolute path of a file"},
		{"- swap_file:\n    path: /swapfile\n    size: 1MiB\n    state: absent\n", "accepts only path and state"},
		{"- swap_file:\n    path: /swapfile\n    size: 1MiB\n    state: on\n", "state must be present or absent"},
		{"- create_filesystem_on_empty_device:\n    filesystem_type: ext4\n", "device is required"},
		{"- create_filesystem_on_empty_device:\n    device: dev/sdb1\n    filesystem_type: ext4\n", "path must be absolute"},
		{"- create_filesystem_on_empty_device:\n    device: /dev/sdb1\n", "filesystem_type is required"},
		{"- create_filesystem_on_empty_device:\n    device: /dev/sdb1\n    filesystem_type: ext4\n    mkfs_options: [-F]\n", "rejects force options"},
		{"- create_filesystem_on_empty_device:\n    device: /dev/sdb1\n    filesystem_type: ext4\n    force: true\n", "force"},
		{"- fstab_mount:\n    check_filesystem_at_boot: never\n    options: [defaults]\n    mount_point: /srv/data\n    mount_source: /dev/sdb1\n    filesystem_type: ext4\n", "state is required: mounted or unmounted_and_removed_from_fstab"},
		{"- fstab_mount:\n    check_filesystem_at_boot: never\n    options: [defaults]\n    mount_point: /srv/data\n    filesystem_type: ext4\n    state: mounted\n", "mount_source is required with state: mounted"},
		{"- fstab_mount:\n    check_filesystem_at_boot: never\n    options: [defaults]\n    mount_point: /srv/data\n    mount_source: /dev/sdb1\n    state: mounted\n", "filesystem_type is required with state: mounted"},
		{"- fstab_mount:\n    check_filesystem_at_boot: never\n    options: [defaults]\n    mount_point: /\n    mount_source: /dev/sdb1\n    filesystem_type: ext4\n    state: mounted\n", "mount_point must be an absolute path other than /"},
		{"- fstab_mount:\n    check_filesystem_at_boot: never\n    mount_point: /srv/data\n    mount_source: /dev/sdb1\n    filesystem_type: ext4\n    options: [\"noatime,nodev\"]\n    state: mounted\n", "is not one mount option"},
		{"- fstab_mount:\n    check_filesystem_at_boot: never\n    mount_point: /srv/data\n    mount_source: /srv/real\n    filesystem_type: none\n    options: [bind]\n    state: mounted\n", "does not manage bind mounts"},
		{"- fstab_mount:\n    options: [defaults]\n    mount_point: /srv/data\n    mount_source: /dev/sdb1\n    filesystem_type: ext4\n    check_filesystem_at_boot: sometimes\n    state: mounted\n", "check_filesystem_at_boot is required"},
		{"- fstab_mount:\n    mount_point: /srv/data\n    mount_source: /dev/sdb1\n    state: unmounted_and_removed_from_fstab\n", "accepts only mount_point and state"},
	} {
		err := loadExample(t, tc.tasks)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: %v", tc.tasks, err)
		}
	}
}
