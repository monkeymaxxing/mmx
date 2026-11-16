package facts

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

type mount struct {
	id                       int
	path, source, filesystem string
	deviceNumber             string
	readOnly                 bool
}

func parseMounts(text string) ([]mount, error) {
	out := []mount{}
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		if line == "" {
			continue
		}
		left, right, ok := strings.Cut(line, " - ")
		a, b := strings.Fields(left), strings.Fields(right)
		if !ok || len(a) < 6 || len(b) < 3 {
			return nil, fmt.Errorf("invalid /proc/self/mountinfo record")
		}
		id, err := strconv.Atoi(a[0])
		if err != nil || id < 1 {
			return nil, fmt.Errorf("invalid mount ID in /proc/self/mountinfo")
		}
		path, err := mountText(a[4])
		if err != nil {
			return nil, err
		}
		source, err := mountText(b[1])
		if err != nil {
			return nil, err
		}
		ro := false
		for _, options := range []string{a[5], b[2]} {
			for _, option := range strings.Split(options, ",") {
				if option == "ro" {
					ro = true
				}
			}
		}
		out = append(out, mount{id: id, deviceNumber: a[2], path: path, source: source, filesystem: b[0], readOnly: ro})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].path != out[j].path {
			return out[i].path < out[j].path
		}
		return out[i].id < out[j].id
	})
	return out, nil
}
func mountText(s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			b.WriteByte(s[i])
			continue
		}
		if i+3 >= len(s) {
			return "", fmt.Errorf("invalid escape sequence in /proc/self/mountinfo")
		}
		n, err := strconv.ParseUint(s[i+1:i+4], 8, 8)
		if err != nil {
			return "", fmt.Errorf("invalid escape sequence in /proc/self/mountinfo")
		}
		b.WriteByte(byte(n))
		i += 3
	}
	return b.String(), nil
}
func localFilesystem(name string) bool {
	switch name {
	case "ext2", "ext3", "ext4", "xfs", "btrfs", "f2fs", "vfat", "exfat", "ntfs", "ntfs3", "squashfs", "iso9660", "udf", "jfs", "reiserfs", "zfs":
		return true
	}
	return false
}
func gatherMounts(s *Snapshot, text string, capacity func(string) (int64, int64, error)) {
	mounts, err := parseMounts(text)
	if err != nil {
		s.put("storage.mounts", nil, err)
		return
	}
	values := []any{}
	for i, m := range mounts {
		item := map[string]any{"id": m.id, "path": m.path, "source": m.source, "filesystem": m.filesystem, "read_only": m.readOnly}
		// No statfs calls for network, FUSE, automount, overlay, or pseudo filesystems.
		// Stacked mounts at one path cannot be queried independently through that path.
		stacked := (i > 0 && mounts[i-1].path == m.path) || (i+1 < len(mounts) && mounts[i+1].path == m.path)
		if localFilesystem(m.filesystem) && !stacked {
			total, available, err := capacity(m.path)
			if err != nil {
				for _, key := range []string{"total_bytes", "available_bytes"} {
					s.Unavailable[fmt.Sprintf("/node/storage/mounts/%d/%s", i, key)] = err.Error()
				}
			} else {
				item["total_bytes"], item["available_bytes"] = total, available
			}
		}
		values = append(values, item)
	}
	s.put("storage.mounts", values, nil)
}
