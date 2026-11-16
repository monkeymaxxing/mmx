package modules

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"mmx/internal/protocol"
)

var networkManagerUUID = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type networkManagerProfile struct{ UUID, Filename, Type, Interface string }
type networkManagerRestoreData struct {
	PreviousFilePresent         bool     `json:"previous_file_present"`
	ProposedFilePresent         bool     `json:"proposed_file_present"`
	PreviousProfileLoaded       bool     `json:"previous_file_profile_was_loaded"`
	PreviousProfileActivated    bool     `json:"previous_file_profile_was_activated"`
	DeviceToRemove              string   `json:"virtual_device_to_remove_after_file_change,omitempty"`
	DeviceToRemoveOnRestore     string   `json:"introduced_device_to_remove_on_restore,omitempty"`
	PreviouslyActivatedProfiles []string `json:"previously_activated_profile_uuids,omitempty"`
}

// This reads only identity fields from nmcli's canonical keyfile output.
// For the original candidate, only the presence of uuid= is used: nmcli may
// supply a UUID when none was written, which is not a declared duplicate.
func networkManagerConnectionFields(text string) map[string]string {
	fields := map[string]string{}
	section := ""
	for _, line := range strings.Split(strings.TrimPrefix(text, "\ufeff"), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = line[1 : len(line)-1]
			continue
		}
		if section == "connection" {
			key, value, ok := strings.Cut(line, "=")
			if ok {
				fields[strings.TrimSpace(key)] = strings.TrimSpace(value)
			}
		}
	}
	return fields
}
func networkManagerKeyfileValue(text string) (string, error) {
	var value strings.Builder
	for i := 0; i < len(text); i++ {
		if text[i] != '\\' {
			value.WriteByte(text[i])
			continue
		}
		i++
		if i == len(text) {
			return "", fmt.Errorf("NetworkManager returned an incomplete keyfile escape.")
		}
		switch text[i] {
		case 's':
			value.WriteByte(' ')
		case 'n':
			value.WriteByte('\n')
		case 't':
			value.WriteByte('\t')
		case 'r':
			value.WriteByte('\r')
		case '\\':
			value.WriteByte('\\')
		default:
			return "", fmt.Errorf("NetworkManager returned an unreadable keyfile escape.")
		}
	}
	return value.String(), nil
}
func networkManagerInterfaceName(name string) bool {
	return name == "" || (len(name) <= 15 && name != "." && name != ".." && !strings.Contains(name, "/") && !strings.ContainsFunc(name, unicode.IsControl))
}
func checkNetworkManagerCandidate(ctx context.Context, run Runtime, id, candidate string) (networkManagerProfile, error) {
	output := run.RunCommand(ctx, []string{"nmcli", "--offline", "connection", "modify", "connection.id", id}, candidate)
	if err := commandFailure(output, "Checking the NetworkManager keyfile"); err != nil {
		return networkManagerProfile{}, err
	}
	if strings.Contains(output.StandardOutput, protocol.TruncationMarker) {
		return networkManagerProfile{}, fmt.Errorf("NetworkManager's reading of the proposed file exceeded the output limit.")
	}
	fields := networkManagerConnectionFields(output.StandardOutput)
	kind, err := networkManagerKeyfileValue(fields["type"])
	if err != nil {
		return networkManagerProfile{}, err
	}
	name, err := networkManagerKeyfileValue(fields["interface-name"])
	if err != nil {
		return networkManagerProfile{}, err
	}
	if kind == "" || !networkManagerInterfaceName(name) {
		return networkManagerProfile{}, fmt.Errorf("NetworkManager did not return a readable connection type and interface name.")
	}
	profile := networkManagerProfile{Type: kind, Interface: name}
	if _, declared := networkManagerConnectionFields(candidate)["uuid"]; declared {
		profile.UUID = fields["uuid"]
		if !networkManagerUUID.MatchString(profile.UUID) {
			return profile, fmt.Errorf("NetworkManager did not return a readable declared profile UUID.")
		}
	}
	return profile, nil
}
func networkManagerQuery(ctx context.Context, run Runtime, arguments ...string) (string, error) {
	output := run.RunCommand(ctx, append([]string{"nmcli"}, arguments...), "")
	if err := commandFailure(output, "Reading NetworkManager's profiles and devices"); err != nil {
		return "", err
	}
	if strings.Contains(output.StandardOutput, protocol.TruncationMarker) {
		return "", fmt.Errorf("NetworkManager's profile or device list exceeded the output limit.")
	}
	return strings.TrimSuffix(output.StandardOutput, "\n"), nil
}
func networkManagerProfiles(ctx context.Context, run Runtime) ([]networkManagerProfile, error) {
	text, err := networkManagerQuery(ctx, run, "-t", "--escape", "no", "-f", "UUID,FILENAME", "connection", "show")
	if err != nil {
		return nil, err
	}
	var profiles []networkManagerProfile
	for _, line := range strings.Split(text, "\n") {
		if line == "" {
			continue
		}
		uuid, path, ok := strings.Cut(line, ":")
		if !ok || !networkManagerUUID.MatchString(uuid) {
			return nil, fmt.Errorf("NetworkManager returned an unreadable profile and file list.")
		}
		profiles = append(profiles, networkManagerProfile{UUID: uuid, Filename: path})
	}
	return profiles, nil
}
func networkManagerProfileDetails(ctx context.Context, run Runtime, profile networkManagerProfile) (networkManagerProfile, error) {
	text, err := networkManagerQuery(ctx, run, "-t", "--escape", "no", "-f", "connection.type,connection.interface-name", "connection", "show", "uuid", profile.UUID)
	if err != nil {
		return profile, err
	}
	fields := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return profile, fmt.Errorf("NetworkManager returned unreadable profile settings.")
		}
		fields[key] = value
	}
	kind, hasType := fields["connection.type"]
	name, hasName := fields["connection.interface-name"]
	if !hasType || !hasName || kind == "" || !networkManagerInterfaceName(name) {
		return profile, fmt.Errorf("NetworkManager returned unreadable profile settings.")
	}
	profile.Type, profile.Interface = kind, name
	return profile, nil
}

type networkManagerActivity struct {
	activated bool
	inUse     bool
}

// NMDeviceState 40..90 is activating, 100 is activated, and 110 is
// deactivating. Only 100 belongs in the snapshot of working profiles;
// an in-progress connection must not pass a restore's inactivity check.
// https://networkmanager.dev/docs/api/latest/nm-dbus-types.html#NMDeviceState
func networkManagerProfileActivity(ctx context.Context, run Runtime) (map[string]networkManagerActivity, error) {
	text, err := networkManagerQuery(ctx, run, "-t", "--escape", "no", "-f", "GENERAL.DEVICE,GENERAL.STATE,GENERAL.CON-UUID", "device", "show")
	if err != nil {
		return nil, err
	}
	activity := map[string]networkManagerActivity{}
	device := ""
	state := -1
	hasUUID := false
	finish := func() error {
		if device != "" && (state < 0 || !hasUUID) {
			return fmt.Errorf("NetworkManager returned an incomplete device state.")
		}
		return nil
	}
	for _, line := range strings.Split(text, "\n") {
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("NetworkManager returned an unreadable device state.")
		}
		switch key {
		case "GENERAL.DEVICE":
			if err := finish(); err != nil {
				return nil, err
			}
			device, state, hasUUID = value, -1, false
		case "GENERAL.STATE":
			parts := strings.Fields(value)
			if len(parts) == 0 {
				return nil, fmt.Errorf("NetworkManager returned an unreadable device state.")
			}
			state, err = strconv.Atoi(parts[0])
			if err != nil {
				return nil, fmt.Errorf("NetworkManager returned an unreadable device state.")
			}
		case "GENERAL.CON-UUID":
			hasUUID = true
			if device == "" || state < 0 {
				return nil, fmt.Errorf("NetworkManager returned an incomplete device state.")
			}
			if state >= 40 && state <= 110 {
				if !networkManagerUUID.MatchString(value) {
					return nil, fmt.Errorf("NetworkManager returned an unreadable active profile UUID.")
				}
				previous := activity[value]
				activity[value] = networkManagerActivity{activated: previous.activated || state == 100, inUse: true}
			}
		default:
			return nil, fmt.Errorf("NetworkManager returned an unexpected device state field.")
		}
	}
	if err := finish(); err != nil {
		return nil, err
	}
	return activity, nil
}
func networkManagerDevice(name string) (exists, virtual bool, err error) {
	if name == "" {
		return false, false, nil
	}
	path, err := filepath.EvalSymlinks(filepath.Join(networkManagerLinksDirectory, name))
	if os.IsNotExist(err) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	relative, err := filepath.Rel(networkManagerVirtualDirectory, path)
	return true, err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)), err
}

func planNetworkManagerChange(ctx context.Context, run Runtime, destination string, candidate networkManagerProfile, present, previous bool) (networkManagerRestoreData, error) {
	data := networkManagerRestoreData{PreviousFilePresent: previous, ProposedFilePresent: present}
	profiles, err := networkManagerProfiles(ctx, run)
	if err != nil {
		return data, err
	}
	old := networkManagerProfile{}
	for _, profile := range profiles {
		if profile.Filename == destination {
			old = profile
		}
		if candidate.UUID != "" && strings.EqualFold(profile.UUID, candidate.UUID) && profile.Filename != destination {
			return data, fmt.Errorf("Cannot add %s: profile UUID %s is already loaded from %s. Each file must have its own profile UUID.", destination, candidate.UUID, profile.Filename)
		}
		if candidate.Interface != "" && strings.HasPrefix(profile.Filename, "/etc/sysconfig/network-scripts/") {
			profile, err = networkManagerProfileDetails(ctx, run, profile)
			if err != nil {
				return data, err
			}
			if profile.Interface == candidate.Interface {
				return data, fmt.Errorf("Cannot configure interface %s with %s: the old-format profile %s is still loaded. Run nmcli connection migrate --plugin keyfile uuid %s on the node, read the resulting file, then put its name and content into the playbook.", candidate.Interface, destination, profile.Filename, profile.UUID)
			}
		}
	}
	activity, err := networkManagerProfileActivity(ctx, run)
	if err != nil {
		return data, err
	}
	for uuid, state := range activity {
		if state.activated {
			data.PreviouslyActivatedProfiles = append(data.PreviouslyActivatedProfiles, uuid)
		}
	}
	sort.Strings(data.PreviouslyActivatedProfiles)
	data.PreviousProfileLoaded = old.UUID != ""
	data.PreviousProfileActivated = activity[old.UUID].activated
	if old.UUID != "" {
		old, err = networkManagerProfileDetails(ctx, run, old)
		if err != nil {
			return data, err
		}
		if old.Interface != candidate.Interface {
			_, virtual, err := networkManagerDevice(old.Interface)
			if err != nil {
				return data, err
			}
			if virtual {
				data.DeviceToRemove = old.Interface
			}
		}
	}
	if candidate.Interface != "" {
		exists, _, err := networkManagerDevice(candidate.Interface)
		if err != nil {
			return data, err
		}
		if !exists {
			data.DeviceToRemoveOnRestore = candidate.Interface
		}
	}
	return data, nil
}
