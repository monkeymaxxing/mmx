package facts

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// root is empty on a node; tests supply a tree containing sys, dev, and run.
// read uses the same logical absolute paths as gatherSystem.
func gatherBlockDevices(s *Snapshot, root string, read func(string) ([]byte, error), mountinfo string, mountErr error) {
	devices, err := blockDevices(root, read, mountinfo, mountErr)
	s.put("storage.block_devices", devices, err)
}

func blockDevices(root string, read func(string) ([]byte, error), mountinfo string, mountErr error) ([]any, error) {
	if root != "" {
		resolved, err := filepath.EvalSymlinks(root)
		if err != nil {
			return nil, err
		}
		root = resolved
	}
	if mountErr != nil {
		return nil, mountErr
	}
	mounts, err := parseMounts(mountinfo)
	if err != nil {
		return nil, err
	}
	// ReadDir sorts names, including the by-id links and slaves below.
	entries, err := os.ReadDir(filepath.Join(root, "/sys/class/block"))
	if err != nil {
		return nil, err
	}
	byID, err := os.ReadDir(filepath.Join(root, "/dev/disk/by-id"))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	stablePaths := map[string][]any{}
	for _, link := range byID {
		if link.Type()&os.ModeSymlink == 0 {
			continue
		}
		path := "/dev/disk/by-id/" + link.Name()
		target, err := filepath.EvalSymlinks(filepath.Join(root, path))
		if os.IsNotExist(err) {
			// A stale link does not identify a device.
			continue
		}
		if err != nil {
			return nil, err
		}
		stablePaths[target] = append(stablePaths[target], path)
	}
	text := func(path string) (string, error) {
		data, err := read(path)
		return strings.TrimSpace(string(data)), err
	}
	exists := func(path string) (bool, error) {
		_, err := os.Stat(filepath.Join(root, path))
		if os.IsNotExist(err) {
			return false, nil
		}
		return err == nil, err
	}
	values := []any{}
	for _, entry := range entries {
		name := entry.Name()
		base := "/sys/class/block/" + name
		rawSize, err := text(base + "/size")
		if os.IsNotExist(err) {
			// The device went away while MMX read the list, as a loop
			// device does when it is detached.
			continue
		}
		if err != nil {
			return nil, err
		}
		sectors, err := strconv.ParseInt(rawSize, 10, 64)
		if err != nil || sectors < 0 || sectors > math.MaxInt64/512 {
			return nil, fmt.Errorf("invalid block device size in %s/size", base)
		}
		if sectors == 0 {
			continue
		}
		rawRO, err := text(base + "/ro")
		if err != nil {
			return nil, err
		}
		if rawRO != "0" && rawRO != "1" {
			return nil, fmt.Errorf("invalid block device read-only value in %s/ro", base)
		}
		number, err := text(base + "/dev")
		if err != nil {
			return nil, err
		}
		parts := strings.Split(number, ":")
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid block device number in %s/dev", base)
		}
		for _, part := range parts {
			if _, err := strconv.ParseUint(part, 10, 32); err != nil {
				return nil, fmt.Errorf("invalid block device number in %s/dev", base)
			}
		}
		item := map[string]any{"name": name, "path": "/dev/" + name, "kind": "disk", "size_bytes": sectors * 512, "read_only": rawRO == "1"}
		for _, marker := range []string{"partition", "dm", "loop", "md"} {
			present, err := exists(base + "/" + marker)
			if err != nil {
				return nil, err
			}
			if !present {
				continue
			}
			switch marker {
			case "partition":
				item["kind"] = "partition"
				target, err := filepath.EvalSymlinks(filepath.Join(root, base))
				if err != nil {
					return nil, err
				}
				item["part_of_disk"] = filepath.Base(filepath.Dir(target))
			case "dm":
				item["kind"] = "device_mapper_device"
				uuid, err := text(base + "/dm/uuid")
				if err != nil && !os.IsNotExist(err) {
					return nil, err
				}
				if strings.HasPrefix(uuid, "LVM-") {
					item["kind"] = "lvm_logical_volume"
				}
				mapperName, err := text(base + "/dm/name")
				if err != nil && !os.IsNotExist(err) {
					return nil, err
				}
				if mapperName != "" {
					item["device_mapper_name"] = mapperName
				}
			case "loop":
				if !strings.HasPrefix(name, "loop") {
					continue
				}
				item["kind"] = "loop_device"
			case "md":
				item["kind"] = "raid_array"
			}
			break
		}
		if item["kind"] == "device_mapper_device" || item["kind"] == "lvm_logical_volume" || item["kind"] == "raid_array" {
			slaves, err := os.ReadDir(filepath.Join(root, base, "slaves"))
			if err != nil && !os.IsNotExist(err) {
				return nil, err
			}
			names := []any{}
			for _, slave := range slaves {
				names = append(names, slave.Name())
			}
			if len(names) > 0 {
				item["built_on_devices"] = names
			}
		}
		if paths := stablePaths[filepath.Join(root, "/dev/"+name)]; len(paths) > 0 {
			item["stable_paths_by_id"] = paths
		}
		// parseMounts sorts the mounts by path.
		points := []any{}
		for _, mount := range mounts {
			if mount.deviceNumber == number {
				points = append(points, mount.path)
			}
		}
		if len(points) > 0 {
			item["mount_points"] = points
		}
		data, err := read("/run/udev/data/b" + number)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		properties := map[string]string{}
		for _, line := range strings.Split(string(data), "\n") {
			if !strings.HasPrefix(line, "E:") {
				continue
			}
			key, value, ok := strings.Cut(line[2:], "=")
			if ok && value != "" {
				properties[key] = value
			}
		}
		serial := properties["ID_SERIAL_SHORT"]
		if serial == "" {
			serial = properties["ID_SERIAL"]
		}
		if serial != "" {
			item["serial"] = serial
		}
		for _, field := range []struct{ key, property string }{{"model", "ID_MODEL"}, {"content_type", "ID_FS_TYPE"}, {"content_uuid", "ID_FS_UUID"}, {"content_label", "ID_FS_LABEL"}} {
			if value := properties[field.property]; value != "" {
				item[field.key] = value
			}
		}
		// udev gives a partition the table type of its disk; only the device
		// that holds the table reports it here.
		if table := properties["ID_PART_TABLE_TYPE"]; table != "" && item["kind"] != "partition" {
			item["partition_table_type"] = table
		}
		values = append(values, item)
	}
	return values, nil
}
