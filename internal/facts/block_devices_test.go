package facts

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestBlockDevices(t *testing.T) {
	root := t.TempDir()
	write := func(path, text string) {
		t.Helper()
		path = filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0644); err != nil {
			t.Fatal(err)
		}
	}
	link := func(path, target string) {
		t.Helper()
		path = filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
	}
	for _, device := range []struct{ name, target, number, sectors string }{
		{"sdb", "/sys/devices/example/block/sdb", "8:16", "100000"},
		{"sdb1", "/sys/devices/example/block/sdb/sdb1", "8:17", "40000"},
		{"sdb2", "/sys/devices/example/block/sdb/sdb2", "8:18", "50000"},
		{"dm-0", "/sys/devices/virtual/block/dm-0", "253:0", "30000"},
		{"dm-1", "/sys/devices/virtual/block/dm-1", "253:1", "20000"},
		{"loop0", "/sys/devices/virtual/block/loop0", "7:0", "0"},
		{"loop1", "/sys/devices/virtual/block/loop1", "7:1", "10"},
		{"md0", "/sys/devices/virtual/block/md0", "9:0", "40000"},
	} {
		write(device.target+"/size", device.sectors+"\n")
		write(device.target+"/ro", "0\n")
		write(device.target+"/dev", device.number+"\n")
		write("/dev/"+device.name, "")
		link("/sys/class/block/"+device.name, "../../"+device.target[len("/sys/"):])
	}
	write("/sys/devices/example/block/sdb/ro", "1")
	for _, name := range []string{"sdb1", "sdb2"} {
		write("/sys/devices/example/block/sdb/"+name+"/partition", "1")
	}
	write("/sys/devices/virtual/block/dm-0/dm/uuid", "LVM-example")
	write("/sys/devices/virtual/block/dm-0/dm/name", "data-postgresql")
	write("/sys/devices/virtual/block/dm-1/dm/uuid", "CRYPT-example")
	write("/sys/devices/virtual/block/dm-1/dm/name", "encrypted")
	link("/sys/devices/virtual/block/dm-0/slaves/sdb2", "../../../../example/block/sdb/sdb2")
	write("/sys/devices/virtual/block/md0/md/level", "raid1")
	for _, name := range []string{"sdb2", "sdb1"} {
		link("/sys/devices/virtual/block/md0/slaves/"+name, "../../../../example/block/sdb/"+name)
	}
	for _, name := range []string{"loop0", "loop1"} {
		write("/sys/devices/virtual/block/"+name+"/loop/backing_file", "/image")
	}
	link("/dev/disk/by-id/wwn-example", "../../sdb")
	link("/dev/disk/by-id/ata-example", "../../sdb")
	link("/dev/disk/by-id/ata-example-part1", "../../sdb1")
	link("/dev/disk/by-id/stale", "../../gone")
	write("/run/udev/data/b8:16", "E:ID_SERIAL_SHORT=serial-short\nE:ID_SERIAL=serial-full\nE:ID_MODEL=Example_Disk\nE:ID_PART_TABLE_TYPE=gpt\n")
	write("/run/udev/data/b8:17", "E:ID_FS_TYPE=ext4\nE:ID_FS_UUID=ext4-uuid\nE:ID_FS_LABEL=data=label\n")
	write("/run/udev/data/b8:18", "E:ID_FS_TYPE=LVM2_member\nE:ID_SERIAL=partition-serial\n")
	write("/run/udev/data/b253:0", "E:ID_FS_TYPE=xfs\nE:ID_FS_UUID=xfs-uuid\n")
	mountinfo := "1 0 8:17 / /data\\040disk rw - ext4 /dev/root rw\n2 0 253:0 / /postgresql rw - xfs /dev/mapper/data-postgresql rw\n3 0 8:17 / /backup rw - ext4 /dev/root rw\n"
	read := func(path string) ([]byte, error) { return os.ReadFile(filepath.Join(root, path)) }
	collect := func() Snapshot {
		s := Snapshot{Values: map[string]any{}, Unavailable: map[string]string{}}
		gatherBlockDevices(&s, root, read, mountinfo, nil)
		return s
	}
	s := collect()
	if len(s.Unavailable) != 0 {
		t.Fatal(s.Unavailable)
	}
	devices := s.Values["storage"].(map[string]any)["block_devices"].([]any)
	wantNames := []string{"dm-0", "dm-1", "loop1", "md0", "sdb", "sdb1", "sdb2"}
	if len(devices) != len(wantNames) {
		t.Fatal(devices)
	}
	byName := map[string]map[string]any{}
	for index, value := range devices {
		item := value.(map[string]any)
		if item["name"] != wantNames[index] || item["path"] != "/dev/"+wantNames[index] || item["size_bytes"].(int64) <= 0 {
			t.Fatal(item)
		}
		byName[wantNames[index]] = item
	}
	checks := map[string]map[string]any{
		"sdb":   {"kind": "disk", "size_bytes": int64(51200000), "read_only": true, "serial": "serial-short", "model": "Example_Disk", "partition_table_type": "gpt", "stable_paths_by_id": []any{"/dev/disk/by-id/ata-example", "/dev/disk/by-id/wwn-example"}},
		"sdb1":  {"kind": "partition", "part_of_disk": "sdb", "content_type": "ext4", "content_uuid": "ext4-uuid", "content_label": "data=label", "mount_points": []any{"/backup", "/data disk"}},
		"sdb2":  {"kind": "partition", "part_of_disk": "sdb", "content_type": "LVM2_member", "serial": "partition-serial"},
		"dm-0":  {"kind": "lvm_logical_volume", "device_mapper_name": "data-postgresql", "built_on_devices": []any{"sdb2"}, "content_type": "xfs", "mount_points": []any{"/postgresql"}},
		"dm-1":  {"kind": "device_mapper_device", "device_mapper_name": "encrypted"},
		"loop1": {"kind": "loop_device"},
		"md0":   {"kind": "raid_array", "built_on_devices": []any{"sdb1", "sdb2"}},
	}
	for name, fields := range checks {
		for key, want := range fields {
			if !reflect.DeepEqual(byName[name][key], want) {
				t.Fatalf("%s.%s = %#v; want %#v", name, key, byName[name][key], want)
			}
		}
	}
	if _, exists := byName["sdb"]["mount_points"]; exists {
		t.Fatal(byName["sdb"])
	}
	if err := os.RemoveAll(filepath.Join(root, "/run/udev")); err != nil {
		t.Fatal(err)
	}
	s = collect()
	if len(s.Unavailable) != 0 {
		t.Fatal(s.Unavailable)
	}
	for _, value := range s.Values["storage"].(map[string]any)["block_devices"].([]any) {
		item := value.(map[string]any)
		for _, key := range []string{"serial", "model", "partition_table_type", "content_type", "content_uuid", "content_label"} {
			if _, exists := item[key]; exists {
				t.Fatal(item)
			}
		}
	}
	for _, bad := range []struct{ path, value string }{{"size", "-1"}, {"size", "9223372036854775807"}, {"ro", "2"}, {"dev", "bad"}} {
		path := "/sys/devices/example/block/sdb/" + bad.path
		before, err := read(path)
		if err != nil {
			t.Fatal(err)
		}
		write(path, bad.value)
		failed := collect()
		if failed.Unavailable["/node/storage/block_devices"] == "" {
			t.Fatal(failed)
		}
		if _, exists := failed.Values["storage"].(map[string]any)["block_devices"]; exists {
			t.Fatal(failed)
		}
		write(path, string(before))
	}
	failed := Snapshot{Values: map[string]any{}, Unavailable: map[string]string{}}
	gatherBlockDevices(&failed, root, read, "", errors.New("mountinfo unreadable"))
	if failed.Unavailable["/node/storage/block_devices"] != "mountinfo unreadable" {
		t.Fatal(failed)
	}
}

func TestBlockDevicesEmptyAndUnavailable(t *testing.T) {
	root := t.TempDir()
	read := func(path string) ([]byte, error) { return os.ReadFile(filepath.Join(root, path)) }
	s := Snapshot{Values: map[string]any{}, Unavailable: map[string]string{}}
	gatherBlockDevices(&s, root, read, "", nil)
	if s.Unavailable["/node/storage/block_devices"] == "" {
		t.Fatal(s)
	}
	if err := os.MkdirAll(filepath.Join(root, "/sys/class/block"), 0755); err != nil {
		t.Fatal(err)
	}
	s = Snapshot{Values: map[string]any{}, Unavailable: map[string]string{}}
	gatherBlockDevices(&s, root, read, "", nil)
	if len(s.Unavailable) != 0 {
		t.Fatal(s)
	}
	devices := s.Values["storage"].(map[string]any)["block_devices"].([]any)
	if devices == nil || len(devices) != 0 {
		t.Fatal(devices)
	}
	s = Snapshot{Values: map[string]any{}, Unavailable: map[string]string{}}
	gatherBlockDevices(&s, root, read, "invalid mountinfo", nil)
	if s.Unavailable["/node/storage/block_devices"] == "" {
		t.Fatal(s)
	}
}
