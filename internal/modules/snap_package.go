package modules

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

// SnapPackage installs snaps, which the snap service on the node keeps up
// to date inside their channel.
type SnapPackage struct {
	Packages []SnapPackageEntry `yaml:"packages" json:"packages"`
	State    string             `yaml:"state,omitempty" json:"state,omitempty"`
}

// SnapPackageEntry names a snap and its channel and access choice.
type SnapPackageEntry struct {
	Name                          string `yaml:"name" json:"name"`
	Channel                       string `yaml:"channel,omitempty" json:"channel,omitempty"`
	AllowAccessOutsideSnapSandbox bool   `yaml:"allow_access_outside_snap_sandbox,omitempty" json:"allow_access_outside_snap_sandbox,omitempty"`
}

func (p *SnapPackage) names() []string {
	names := make([]string, len(p.Packages))
	for index, entry := range p.Packages {
		names[index] = entry.Name
	}
	return names
}

var snapPackageDescription = Description{
	Name: "snap_package", Topic: TopicLanguageAndToolInstallers, Schema: reflect.TypeOf(SnapPackage{}),
	Summary: "snaps installed by the snap service (the package snapd), which then keeps them up to date inside their channel; value lists each snap that changed as {name, from, to}, each a channel and revision",
	Fields: []Field{
		{Name: "packages", Required: true, Notes: "each entry requires name and channel with state: present; allow_access_outside_snap_sandbox: true gives that snap ordinary-program access outside its sandbox; state: absent takes name only; no version, because snaps follow channels"},
		{Name: "state", Values: presenceStates, Default: "present", Notes: "present installs missing snaps and switches installed snaps to the requested channel; absent removes the listed snaps"},
	},
	Example: "- snap_package:\n    packages:\n      - name: certbot\n        channel: latest/stable\n        allow_access_outside_snap_sandbox: true\n",
	Results: []string{"status", "change", "value"},
	Dryrun:  "with state: present, checks every entry's confinement; lists the snaps it would install, switch to the channel, or remove",
}

// snapRisks are the risk levels a channel names.
var snapRisks = []string{"stable", "candidate", "beta", "edge"}

// Prepare checks the inputs when the project loads.
func (p *SnapPackage) Prepare(base string) error {
	if len(p.Packages) == 0 {
		return invalid("packages", "packages is required: list the snaps this task manages")
	}
	if p.State != "" && !slices.Contains(presenceStates, p.State) {
		return invalid("state", "state must be present or absent")
	}
	seen := map[string]bool{}
	for index, entry := range p.Packages {
		field := fmt.Sprintf("packages[%d]", index)
		if entry.Name == "" {
			return invalid(field+".name", "name is required for each snap")
		}
		if seen[entry.Name] {
			return invalid(field+".name", "the name "+entry.Name+" is listed twice")
		}
		seen[entry.Name] = true
		if p.State == "absent" && (entry.Channel != "" || entry.AllowAccessOutsideSnapSandbox) {
			return invalid(field, "state: absent takes name only")
		}
		if p.State != "absent" && entry.Channel == "" {
			return invalid(field+".channel", "channel is required: the channel this snap follows, such as latest/stable")
		}
	}

	return nil
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (p *SnapPackage) Arguments(arguments *ArgumentList) {
	for index := range p.Packages {
		field := fmt.Sprintf("packages[%d]", index)
		arguments.Public(field+".name", &p.Packages[index].Name, installerNames("name"))
		if p.Packages[index].Channel != "" {
			arguments.Public(field+".channel", &p.Packages[index].Channel, installerNames("channel"))
		}
	}
}

// Run does the module's work on the node.
func (p *SnapPackage) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.Runtime.SnapPackage(ctx, *p)
}

// TrackedInputs lists the named snaps, which count by their revisions. A
// snap that updated itself restarts the block's service at its next
// activation.
func (p *SnapPackage) TrackedInputs() []TrackedInput {
	return []TrackedInput{{Kind: "installed", Path: "snap_package " + strings.Join(p.names(), ", "), State: p}}
}

// ReadState lists the installed channel and revision of the named snaps.
func (p *SnapPackage) ReadState(ctx context.Context, _ value.Scope) (string, error) {
	return namedStateReader(ctx, snapSettings, func(ctx context.Context, i *installer) (map[string]string, error) {
		installed, err := snapsInstalled(ctx, i, p.names())
		return snapListing(installed), err
	})
}

// StateAfter is what a dryrun predicts for the named snaps.
func (p *SnapPackage) StateAfter(r protocol.Result) (string, bool) { return installerStateAfter(r) }

// Snaps are installed by root, through the snap service.
var snapSettings = installerSettings{program: "snap"}

// installedSnap is one snap in snap list.
type installedSnap struct {
	revision, tracking string
}

func (s installedSnap) String() string { return s.tracking + " revision " + s.revision }

// snapsInstalled reads snap list: a header line, then one line for each
// snap with its name, version, revision, the channel it tracks, its
// publisher, and notes.
func snapsInstalled(ctx context.Context, i *installer, names []string) (map[string]installedSnap, error) {
	out, err := i.read(ctx, "", i.program, "list")
	if err != nil {
		return nil, err
	}
	installed := map[string]installedSnap{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[0] == "Name" || !slices.Contains(names, fields[0]) {
			continue
		}
		installed[fields[0]] = installedSnap{revision: fields[2], tracking: fields[3]}
	}
	return installed, nil
}

func snapListing(installed map[string]installedSnap) map[string]string {
	out := map[string]string{}
	for name, snap := range installed {
		out[name] = snap.String()
	}
	return out
}

// snapChannel is a channel as snap list shows the one a snap tracks: a
// channel of only a risk, such as stable, is on the track latest, and one
// of only a track, such as 24, follows its stable risk.
func snapChannel(channel string) string {
	if strings.Contains(channel, "/") {
		return channel
	}
	if slices.Contains(snapRisks, channel) {
		return "latest/" + channel
	}
	return channel + "/stable"
}

// SnapPackage installs, switches, or removes the named snaps.
func (run Runtime) SnapPackage(ctx context.Context, p SnapPackage) protocol.Result {
	i, stop := run.newInstaller("snap_package", snapSettings)
	if stop != nil {
		return *stop
	}
	before, err := snapsInstalled(ctx, i, p.names())
	if err != nil {
		return protocol.Failed(err)
	}
	var planned []any
	var commands [][]string
	if p.State == "absent" {
		var remove []string
		for _, name := range p.names() {
			if snap, installed := before[name]; installed {
				planned = append(planned, installedChange(name, snap.String(), nil))
				remove = append(remove, name)
			}
		}
		if len(remove) > 0 {
			commands = append(commands, append([]string{"remove"}, remove...))
		}
	} else {
		// Check every access choice before any install or refresh, also in dryrun.
		for _, entry := range p.Packages {
			out, err := i.read(ctx, "", i.program, "info", "--verbose", entry.Name)
			if err != nil {
				return protocol.Failed(err)
			}
			var info struct {
				Notes struct {
					Confinement string `yaml:"confinement"`
				} `yaml:"notes"`
			}
			if err := yaml.Unmarshal([]byte(out), &info); err != nil {
				return protocol.Failed(fmt.Errorf("read confinement of %s from snap info: %w", entry.Name, err))
			}
			switch info.Notes.Confinement {
			case "classic":
				if !entry.AllowAccessOutsideSnapSandbox {
					return protocol.ConfigurationError(fmt.Errorf("installing snap %s allows access outside the snap sandbox, like an ordinary program. To allow access, add allow_access_outside_snap_sandbox: true to the snap's packages entry", entry.Name))
				}
			case "strict", "devmode":
			default:
				return protocol.Failed(fmt.Errorf("snap info did not report a known confinement for %s", entry.Name))
			}
		}
		for _, entry := range p.Packages {
			name := entry.Name
			channel := snapChannel(entry.Channel)
			snap, installed := before[name]
			command := "install"
			switch {
			case !installed:
				planned = append(planned, installedChange(name, nil, channel))
			case snap.tracking != channel:
				planned = append(planned, installedChange(name, snap.String(), channel))
				command = "refresh"
			default:
				continue
			}
			// snap takes the channel for one snap at a time.
			args := []string{command, name, "--channel=" + entry.Channel}
			if entry.AllowAccessOutsideSnapSandbox {
				args = append(args, "--classic")
			}
			commands = append(commands, args)
		}
	}
	return installNamed(ctx, run, snapListing(before), planned, func() error {
		for _, args := range commands {
			if _, err := i.change(ctx, args[0], "", i.program, args...); err != nil {
				return err
			}
		}
		return nil
	}, func(ctx context.Context) (map[string]string, error) {
		after, err := snapsInstalled(ctx, i, p.names())
		return snapListing(after), err
	})
}
