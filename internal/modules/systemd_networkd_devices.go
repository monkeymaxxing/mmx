package modules

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

// Preserve order, repeated sections, and repeated assignments. The comparison
// ignores only comments, empty lines, and whitespace around assignments. It is
// not a second syntax validator: networkd diagnoses malformed lines itself.
type networkdSetting struct{ section, key, value string }

func networkdSettings(text string) []networkdSetting {
	text = strings.TrimPrefix(text, "\ufeff")
	var settings []networkdSetting
	section, continued := "", ""
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		line = continued + line
		if strings.HasSuffix(line, "\\") {
			continued = strings.TrimSuffix(line, "\\") + " "
			continue
		}
		continued = ""
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			settings = append(settings, networkdSetting{section: section})
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			settings = append(settings, networkdSetting{section: section, value: line})
			continue
		}
		settings = append(settings, networkdSetting{section, strings.TrimSpace(key), strings.TrimSpace(value)})
	}
	if continued != "" {
		settings = append(settings, networkdSetting{section: section, value: continued})
	}
	return settings
}

func networkdValue(value string) string {
	if strings.HasPrefix(value, `"`) {
		if decoded, err := strconv.Unquote(value); err == nil {
			return decoded
		}
	}
	if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
		return value[1 : len(value)-1]
	}
	return value
}
func networkdDeviceName(name string) bool {
	return name != "" && name != "." && name != ".." && len(name) <= 15 && !strings.ContainsAny(name, "/:%\\ \t\r\n\x00\"'")
}
func networkdDeclaredDevices(settings []networkdSetting, otherFile bool) ([]string, error) {
	var name, peer, kind string
	for _, setting := range settings {
		value := networkdValue(setting.value)
		if setting.section == "NetDev" && setting.key == "Name" {
			name = value
		}
		if setting.section == "NetDev" && setting.key == "Kind" {
			kind = value
		}
		if setting.section == "Peer" && setting.key == "Name" {
			peer = value
		}
	}
	var names []string
	// Another file's drop-in can inherit Kind=veth from its main file.
	// An extra protective declaration is safe; a missed peer is not.
	if kind != "veth" && !otherFile {
		peer = ""
	}
	for _, name := range []string{name, peer} {
		// A specifier can name a real device without a literal declaration. MMX
		// cannot establish ownership of it before installation.
		if strings.Contains(name, "%") {
			return nil, fmt.Errorf("a managed .netdev file must use literal device names, without systemd specifiers.")
		}
		if networkdDeviceName(name) && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return names, nil
}

func readNetworkdFile(path string) (string, error) {
	parent, info, err := openParent(path, true)
	if parent != nil {
		defer parent.Close()
	}
	if err != nil {
		return "", err
	}
	if info == nil {
		return "", nil
	}
	return readTextAt(parent, filepath.Base(path))
}

// Scan all three directories, including shadowed files. A conservative refusal
// is safer than deleting a device that another installed declaration names.
func networkdFiles(suffix string) ([]string, error) {
	var paths []string
	for _, directory := range networkdDirectories {
		entries, err := os.ReadDir(directory)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			path := filepath.Join(directory, entry.Name())
			if strings.HasSuffix(entry.Name(), suffix) {
				paths = append(paths, path)
			}
			if strings.HasSuffix(entry.Name(), suffix+".d") {
				children, err := os.ReadDir(path)
				if err != nil {
					return nil, err
				}
				for _, child := range children {
					if strings.HasSuffix(child.Name(), ".conf") {
						paths = append(paths, filepath.Join(path, child.Name()))
					}
				}
			}
		}
	}
	return paths, nil
}

func readNetworkdDeclaration(path string) (string, error) {
	// The native mask is not a declaration. Other symlinks are followed for
	// inspection only; the module never writes or deletes through them.
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	if resolved == "/dev/null" {
		return "", nil
	}
	return readNetworkdFile(resolved)
}

func networkdReferences(destination, name string, adding bool) error {
	paths, err := networkdFiles(".network")
	if err != nil {
		return err
	}
	for _, path := range paths {
		text, err := readNetworkdDeclaration(path)
		if err != nil {
			return err
		}
		for _, setting := range networkdSettings(text) {
			if setting.section != "Network" || setting.key == "" {
				continue
			}
			for _, word := range strings.Fields(setting.value) {
				if networkdValue(word) == name {
					if adding {
						return fmt.Errorf("Cannot add %s: %s already refers to device %s (%s=%s). Add the .netdev file before installing the file that refers to the device.", destination, path, name, setting.key, setting.value)
					}
					return fmt.Errorf("Cannot remove device %s declared in %s: %s still refers to the device (%s=%s). Remove the device reference from the referring file first.", name, destination, path, setting.key, setting.value)
				}
			}
		}
	}
	return nil
}

func networkdDeviceExists(name string) (bool, error) {
	_, err := os.Stat(filepath.Join(networkdLinksDirectory, name))
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}
func networkdMayDelete(destination, name string) (bool, error) {
	resolved, err := filepath.EvalSymlinks(filepath.Join(networkdLinksDirectory, name))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !strings.HasPrefix(resolved, networkdVirtualDirectory+string(os.PathSeparator)) {
		return false, nil
	}
	shared, err := networkdDeclaredElsewhere(destination, name)
	return !shared, err
}
func networkdDeclaredElsewhere(destination, name string) (bool, error) {
	paths, err := networkdFiles(".netdev")
	if err != nil {
		return false, err
	}
	for _, path := range paths {
		if path == destination {
			continue
		}
		text, err := readNetworkdDeclaration(path)
		if err != nil {
			return false, err
		}
		names, err := networkdDeclaredDevices(networkdSettings(text), true)
		if err != nil {
			return false, err
		}
		if slices.Contains(names, name) {
			return true, nil
		}
	}
	return false, nil
}
func networkdCanDelete(destination, name string, restoring bool) error {
	safe, err := networkdMayDelete(destination, name)
	if err != nil {
		return err
	}
	if !safe {
		return fmt.Errorf("Cannot remove device %s declared in %s: the device is not a virtual device owned exclusively by the .netdev file.", name, destination)
	}
	if restoring {
		return nil
	}
	return networkdReferences(destination, name, false)
}

func planNetworkdDevices(destination, proposed string) (networkdRestoreData, error) {
	var data networkdRestoreData
	if !strings.HasSuffix(destination, ".netdev") {
		return data, nil
	}
	previous, err := readNetworkdFile(destination)
	if err != nil {
		return data, err
	}
	data.previousContentsSHA256 = fmt.Sprintf("%x", sha256.Sum256([]byte(previous)))
	before, after := networkdSettings(previous), networkdSettings(proposed)
	// Drop-ins can change Name= or any setting behind the module's declaration.
	// Refuse them rather than pretending to own the complete device definition.
	for _, directory := range networkdDirectories {
		entries, err := os.ReadDir(filepath.Join(directory, filepath.Base(destination)+".d"))
		if err != nil && !os.IsNotExist(err) {
			return data, err
		}
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".conf") {
				return data, fmt.Errorf("Cannot manage %s: drop-in file %s changes the device definition. Put the complete device definition in the managed .netdev file first.", destination, filepath.Join(directory, filepath.Base(destination)+".d", entry.Name()))
			}
		}
	}
	oldNames, err := networkdDeclaredDevices(before, false)
	if err != nil {
		return data, err
	}
	newNames, err := networkdDeclaredDevices(after, false)
	if err != nil {
		return data, err
	}
	for _, name := range newNames {
		if !slices.Contains(oldNames, name) {
			if err := networkdReferences(destination, name, true); err != nil {
				return data, err
			}
		}
		exists, err := networkdDeviceExists(name)
		if err != nil {
			return data, err
		}
		if !exists {
			shared, err := networkdDeclaredElsewhere(destination, name)
			if err != nil {
				return data, err
			}
			if !shared {
				data.DevicesToRemoveOnRestore = append(data.DevicesToRemoveOnRestore, name)
			}
			continue
		}
		if !slices.Contains(oldNames, name) {
			return data, fmt.Errorf("Cannot add %s: device %s already exists and was not created by the .netdev file.", destination, name)
		}
		if !reflect.DeepEqual(before, after) {
			return data, fmt.Errorf("Cannot change %s while device %s exists: systemd-networkd applies a .netdev file only when systemd-networkd creates the device. Remove the device first with state: absent, then add the new .netdev file.", destination, name)
		}
	}
	for _, name := range oldNames {
		if slices.Contains(newNames, name) {
			continue
		}
		if err := networkdReferences(destination, name, false); err != nil {
			return data, err
		}
		safe, err := networkdMayDelete(destination, name)
		if err != nil {
			return data, err
		}
		if safe {
			data.DevicesToRemove = append(data.DevicesToRemove, name)
		}
	}
	// Deleting either veth end deletes its peer too. If one end is shared,
	// neither deletion belongs to this file alone.
	if len(oldNames) == 2 && len(data.DevicesToRemove) != 2 {
		data.DevicesToRemove = nil
	}
	if len(newNames) == 2 && len(data.DevicesToRemoveOnRestore) != 2 {
		data.DevicesToRemoveOnRestore = nil
	}
	return data, nil
}
