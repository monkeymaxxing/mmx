package modules

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"mmx/internal/assets"
	"mmx/internal/protocol"
	"mmx/internal/value"
)

// WireguardInterface manages one WireGuard interface that wg-quick brings
// up: its file <name>.conf in /etc/wireguard, its private key beside it in
// <name>.private_key, and the service wg-quick@<name>.service. The task
// writes the file without its private key; MMX adds the key, which the node
// creates and which never leaves it.
type WireguardInterface struct {
	Name                                  string                    `yaml:"name" json:"name"`
	Content                               *string                   `yaml:"content,omitempty" json:"content,omitempty"`
	Template                              string                    `yaml:"template,omitempty" json:"template,omitempty"`
	State                                 string                    `yaml:"state,omitempty" json:"state,omitempty"`
	RestoreUnlessControllerConfirmsWithin string                    `yaml:"restore_unless_controller_confirms_within,omitempty" json:"restore_unless_controller_confirms_within,omitempty"`
	TemplateText                          string                    `yaml:"-" json:"template_text,omitempty"`
	Asset                                 *assets.Ref               `yaml:"-" json:"asset,omitempty"`
	LocalPath                             string                    `yaml:"-" json:"-"`
	SourceBase                            string                    `yaml:"-" json:"-"`
	Variants                              map[string]*SourceVariant `yaml:"-" json:"variants,omitempty"`
}

// wireguardPlaces is where a wireguard_interface task keeps its files, and
// who owns them: /etc/wireguard and root on a node. Tests give their own.
// The AppArmor paths are the kernel's list of loaded profiles and the
// profile that confines wg-quick; empty in tests that leave AppArmor out.
type wireguardPlaces struct {
	directory                        string
	owner, group                     Account
	apparmorProfiles, wgQuickProfile string
}

var wireguardOnANode = wireguardPlaces{directory: "/etc/wireguard", owner: "0", group: "0",
	apparmorProfiles: "/sys/kernel/security/apparmor/profiles", wgQuickProfile: "/etc/apparmor.d/wg-quick"}

func (p wireguardPlaces) file(name string) string { return filepath.Join(p.directory, name+".conf") }
func (p wireguardPlaces) key(name string) string {
	return filepath.Join(p.directory, name+".private_key")
}

func wireguardUnit(name string) string { return "wg-quick@" + name + ".service" }

// wg-quick accepts the characters a-z, A-Z, 0-9, _, =, +, ., and - in a
// name of at most 15, the longest interface name Linux takes. systemd takes
// no = and + in a unit name, and escapes them in wg-quick@<name>.service, so
// that wg-quick would look for another file.
var wireguardInterfaceName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,15}$`)
var wireguardNameRule = rule("wireguard_interface.name must be 1 to 15 of the characters a-z, A-Z, 0-9, _, ., and -, "+
	"such as wg0: an interface name that wg-quick and the unit wg-quick@<name>.service both take", wireguardInterfaceName.MatchString)

func (w *WireguardInterface) Prepare(base string) error {
	if err := validatePresence(w.State); err != nil {
		return invalid("state", "wireguard_interface.state must be present or absent")
	}
	if w.State == "absent" {
		if w.Content != nil || w.Template != "" {
			return fmt.Errorf("WireGuard interface removal takes only name, state, and restore_unless_controller_confirms_within.")
		}
	} else if (w.Content == nil) == (w.Template == "") {
		return fmt.Errorf("wireguard_interface requires exactly one of content or template")
	}
	input := NetworkFile{Name: w.Name, Content: w.Content, Template: w.Template, State: w.State, RestoreUnlessControllerConfirmsWithin: w.RestoreUnlessControllerConfirmsWithin}
	if err := input.prepare(base, "name"); err != nil {
		return err
	}
	w.SourceBase, w.LocalPath, w.TemplateText = input.SourceBase, input.LocalPath, input.TemplateText
	return nil
}

func (w *WireguardInterface) Arguments(arguments *ArgumentList) {
	arguments.Public("name", &w.Name, wireguardNameRule)
	if w.Content != nil {
		arguments.Private("content", *w.Content, parsed(func(text string) error {
			_, err := parseWireguardFile(text, false)
			return err
		}))
	}
}

func (w *WireguardInterface) Reads() Reads {
	if w.Template == "" {
		return Reads{}
	}
	return Reads{Templates: map[string]string{"template": w.TemplateText}}
}

func (w *WireguardInterface) ControllerFiles() []ControllerFile {
	if w.Template == "" {
		return nil
	}
	return []ControllerFile{{Field: "template", Kind: SourceTemplate, Path: &w.Template, Base: w.SourceBase,
		Variants: &w.Variants, Asset: &w.Asset, Template: &w.TemplateText, Local: &w.LocalPath}}
}

// ManagedPaths lists the two files the task manages; a dryrun cannot check
// a later task's path_exists on them.
func (w *WireguardInterface) ManagedPaths() []string {
	return []string{wireguardOnANode.file(w.Name), wireguardOnANode.key(w.Name)}
}

// UnknownInADryrun lists the result fields a dryrun cannot know: the public
// key of a node that has no private key yet.
func (w *WireguardInterface) UnknownInADryrun(r protocol.Result) map[string]string {
	if r.Status != "succeeded" || r.PublicKey != "" || w.State == "absent" {
		return nil
	}
	return map[string]string{"public_key": "would create the node's private key, so its public key is not known in a dryrun"}
}

func (w *WireguardInterface) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.Runtime.wireguardInterface(ctx, *w, task.Scope, wireguardOnANode, task.OwedRestores)
}

var wireguardInterfaceDescription = Description{
	Name: "wireguard_interface", Topic: TopicNetworkConfiguration, Schema: reflect.TypeOf(WireguardInterface{}),
	Summary: "Manages one WireGuard interface that wg-quick brings up: its file in /etc/wireguard, a private key that the node creates and keeps, " +
		"and the service wg-quick@<name>.service. The node restores an unconfirmed configuration file change by itself.",
	Fields: []Field{
		{Name: "name", Required: true, Notes: "the interface name, such as wg0: 1 to 15 of the characters a-z, A-Z, 0-9, _, ., and -; the file is /etc/wireguard/<name>.conf"},
		{Name: "content", Notes: "the whole wg-quick file without PrivateKey: one [Interface] section and a [Peer] section for each peer; exactly one of content and template is required for state: present"},
		{Name: "template", Notes: "a Go template on the controller machine, relative to the project or role directory, rendered with the node's values; it can build the [Peer] sections from the inventory"},
		{Name: "state", Values: presenceStates, Default: "present", Notes: "With state: present, writes the configuration file and brings the interface up. With state: absent, removes the configuration file and stops and disables the service, then removes the private key after confirmation."},
		{Name: "restore_unless_controller_confirms_within", Default: "2m", Notes: "Choose a positive duration for reachability confirmation after the change before the node restores the previous file. Activation uses part of this time. The wait before the change has the same limit."},
	},
	Example: "- wireguard_interface:\n    name: wg0\n    content: |\n      [Interface]\n      Address = 10.0.0.1/24\n      ListenPort = 51820\n\n" +
		"      [Peer]\n      PublicKey = xntON0r+ip4v53R2PDYOEKeq0RH91Y9x307uPnUQOTE=\n      AllowedIPs = 10.0.0.2/32\n      Endpoint = web02.example.net:51820\n",
	Results: []string{"status", "change", "reason", "public_key"},
	Dryrun: "compares the file, the running interface, and the service, and reports the start, reload, or restart it would make; writes nothing. " +
		"On a node without the private key file, public_key is not known, and tasks that read it report not checked. " +
		"Missing wireguard-tools is not checked, because an earlier node task may install it. Whether the node remains reachable after activation was not checked.",
	WaitLimit: "restore_unless_controller_confirms_within bounds the change",
}

// The settings of [Interface] that wg-quick handles itself and that
// wg-quick strip drops: only a restart applies them.
var wireguardQuickSettings = []string{"address", "dns", "mtu", "table", "preup", "predown", "postup", "postdown", "saveconfig"}

// The settings that wg reads, and that a reload applies.
var wireguardInterfaceSettings = []string{"privatekey", "listenport", "fwmark"}
var wireguardPeerSettings = []string{"publickey", "presharedkey", "allowedips", "endpoint", "persistentkeepalive"}

// wireguardFile is what MMX reads of a wg-quick file: the settings it
// compares with the running interface, and the lines of [Interface] that
// wg-quick strip drops.
type wireguardFile struct {
	// interfaceLine is the index of the [Interface] line, after which MMX
	// writes the private key.
	interfaceLine int
	privateKey    string
	listenPort    *int
	addresses     []netip.Prefix
	mtu           int // 0: not set
	table         string
	// quickSettings are the lines of [Interface] that only wg-quick reads,
	// as their setting's name in lower case and their value, in order.
	quickSettings [][2]string
	peers         map[string]wireguardPeer // by public key
}

type wireguardPeer struct {
	allowedIPs   []netip.Prefix // masked and sorted
	keepalive    int            // 0: off
	presharedKey bool           // whether one is set; its value is never kept
}

func (p wireguardPeer) allowed() string {
	if len(p.allowedIPs) == 0 {
		return "(none)"
	}
	texts := make([]string, len(p.allowedIPs))
	for i, prefix := range p.allowedIPs {
		texts[i] = prefix.String()
	}
	return strings.Join(texts, ", ")
}

// wireguardSavesRunningConfiguration reads only the wg-quick switch that
// can overwrite an owed restore. It follows wg-quick's section, comment,
// case, and whitespace rules without requiring MMX to parse the whole file.
func wireguardSavesRunningConfiguration(text string) bool {
	inInterface, saves := false, false
	for _, line := range strings.Split(text, "\n") {
		line, _, _ = strings.Cut(line, "#")
		key, setting, _ := strings.Cut(line, "=")
		key, setting = strings.TrimSpace(key), strings.TrimSpace(setting)
		if strings.HasPrefix(key, "[") {
			inInterface = strings.EqualFold(key, "[Interface]")
		}
		if inInterface && strings.EqualFold(key, "SaveConfig") {
			if strings.EqualFold(setting, "true") {
				saves = true
			}
			if strings.EqualFold(setting, "false") {
				saves = false
			}
		}
	}
	return saves
}

// parseWireguardFile reads text as wg-quick and wg read it: comments start
// with #, setting names have any case, and each setting belongs to the
// section above it. It refuses what MMX owns, PrivateKey and SaveConfig =
// true, unless nodeFile says that text is the file on the node, which holds
// the key. Errors name lines, and never a key.
func parseWireguardFile(text string, nodeFile bool) (wireguardFile, error) {
	f := wireguardFile{interfaceLine: -1, peers: map[string]wireguardPeer{}}
	section := ""
	var peer wireguardPeer
	peerKey, peerLine := "", 0
	owners := map[netip.Prefix]string{}
	finishPeer := func() error {
		if section != "peer" {
			return nil
		}
		if peerKey == "" {
			return fmt.Errorf("the [Peer] section on line %d has no PublicKey", peerLine)
		}
		if _, repeated := f.peers[peerKey]; repeated {
			return fmt.Errorf("the [Peer] section on line %d repeats the PublicKey %s; each peer appears once", peerLine, peerKey)
		}
		slices.SortFunc(peer.allowedIPs, func(a, b netip.Prefix) int { return strings.Compare(a.String(), b.String()) })
		peer.allowedIPs = slices.Compact(peer.allowedIPs)
		for _, prefix := range peer.allowedIPs {
			if other, taken := owners[prefix]; taken {
				return fmt.Errorf("the allowed IP %s belongs to the peers %s and %s; WireGuard keeps an allowed IP for one peer only", prefix, other, peerKey)
			}
			owners[prefix] = peerKey
		}
		f.peers[peerKey] = peer
		return nil
	}
	for i, line := range strings.Split(text, "\n") {
		number := i + 1
		stripped, _, _ := strings.Cut(line, "#")
		stripped = strings.TrimSpace(stripped)
		if stripped == "" {
			continue
		}
		if strings.HasPrefix(stripped, "[") {
			if err := finishPeer(); err != nil {
				return f, err
			}
			switch strings.ToLower(stripped) {
			case "[interface]":
				if f.interfaceLine >= 0 {
					return f, fmt.Errorf("line %d: a second [Interface] section; the file has exactly one", number)
				}
				f.interfaceLine, section = i, "interface"
			case "[peer]":
				section, peer, peerKey, peerLine = "peer", wireguardPeer{}, "", number
			default:
				return f, fmt.Errorf("line %d: %s is not a wg-quick section; use [Interface] or [Peer]", number, stripped)
			}
			continue
		}
		name, setting, ok := strings.Cut(stripped, "=")
		if !ok {
			return f, fmt.Errorf("line %d is not a section, comment, or setting of the form Name = value", number)
		}
		name, setting = strings.TrimSpace(name), strings.TrimSpace(setting)
		key := strings.ToLower(name)
		refuse := func(format string, a ...any) error {
			return fmt.Errorf("line %d: %s %s", number, name, fmt.Sprintf(format, a...))
		}
		switch section {
		case "":
			return f, fmt.Errorf("line %d: %s comes before the [Interface] or [Peer] section it belongs to", number, name)
		case "interface":
			switch key {
			case "privatekey":
				if !nodeFile {
					return f, refuse("is refused: MMX adds the node's own private key, which the node creates and keeps")
				}
				f.privateKey = setting
			case "listenport":
				port, err := strconv.ParseUint(setting, 10, 16)
				if err != nil {
					return f, refuse("must be a port number from 0 to 65535, not %q", setting)
				}
				f.listenPort = new(int(port))
			case "fwmark":
				if !strings.EqualFold(setting, "off") {
					if _, err := strconv.ParseUint(setting, 0, 32); err != nil {
						return f, refuse("must be off or a number, not %q", setting)
					}
				}
			case "address":
				for _, text := range strings.FieldsFunc(setting, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
					address, err := wireguardAddress(text, false)
					if err != nil {
						return f, refuse("%s", err)
					}
					f.addresses = append(f.addresses, address)
				}
			case "mtu":
				mtu, err := strconv.Atoi(setting)
				if err != nil || mtu <= 0 {
					return f, refuse("must be a positive number, not %q", setting)
				}
				f.mtu = mtu
			case "table":
				f.table = setting
			case "saveconfig":
				switch strings.ToLower(setting) {
				case "true":
					if !nodeFile {
						return f, refuse("= true is refused: wg-quick would write the running settings into the file that MMX owns when the interface goes down")
					}
				case "false":
				default:
					return f, refuse("must be true or false, not %q", setting)
				}
			case "dns", "preup", "predown", "postup", "postdown":
			default:
				return f, refuse("is not a setting in [Interface]; supported settings are %s", wireguardKnownSettings(wireguardQuickSettings, wireguardInterfaceSettings))
			}
			if slices.Contains(wireguardQuickSettings, key) {
				f.quickSettings = append(f.quickSettings, [2]string{key, setting})
			}
		case "peer":
			switch key {
			case "publickey":
				if peerKey != "" {
					return f, refuse("appears twice in the [Peer] section on line %d", peerLine)
				}
				if !wireguardKey(setting) {
					return f, refuse("must be a WireGuard key: 32 bytes in base64, as wg pubkey prints it")
				}
				peerKey = setting
			case "presharedkey":
				if !wireguardKey(setting) {
					return f, refuse("must be a WireGuard key: 32 bytes in base64, as wg genpsk prints it")
				}
				peer.presharedKey = true
			case "allowedips":
				for _, text := range strings.Split(setting, ",") {
					if text = strings.TrimSpace(text); text == "" {
						continue
					}
					prefix, err := wireguardAddress(text, true)
					if err != nil {
						return f, refuse("%s", err)
					}
					peer.allowedIPs = append(peer.allowedIPs, prefix)
				}
			case "endpoint":
				if setting == "" {
					return f, refuse("needs a host and port, such as web02.example.net:51820")
				}
			case "persistentkeepalive":
				if !strings.EqualFold(setting, "off") {
					seconds, err := strconv.ParseUint(setting, 10, 16)
					if err != nil {
						return f, refuse("must be off or a number of seconds from 0 to 65535, not %q", setting)
					}
					peer.keepalive = int(seconds)
				}
			default:
				return f, refuse("is not a setting in [Peer]; supported settings are %s", wireguardKnownSettings(wireguardPeerSettings))
			}
		}
	}
	if err := finishPeer(); err != nil {
		return f, err
	}
	if f.interfaceLine < 0 {
		return f, fmt.Errorf("the file has no [Interface] section")
	}
	return f, nil
}

// wireguardSettingNames spells each setting as wg-quick's manual does.
var wireguardSettingNames = map[string]string{"address": "Address", "dns": "DNS", "mtu": "MTU", "table": "Table", "preup": "PreUp",
	"predown": "PreDown", "postup": "PostUp", "postdown": "PostDown", "saveconfig": "SaveConfig", "privatekey": "PrivateKey",
	"listenport": "ListenPort", "fwmark": "FwMark", "publickey": "PublicKey", "presharedkey": "PresharedKey", "allowedips": "AllowedIPs",
	"endpoint": "Endpoint", "persistentkeepalive": "PersistentKeepalive"}

// wireguardKnownSettings lists the settings a task may write, in the order
// of lists, without PrivateKey, which MMX writes.
func wireguardKnownSettings(lists ...[]string) string {
	var known []string
	for _, list := range lists {
		for _, key := range list {
			if key != "privatekey" {
				known = append(known, wireguardSettingNames[key])
			}
		}
	}
	return strings.Join(known, ", ")
}

// wireguardAddress reads an address or network as wg-quick and wg do: one
// without a prefix length is a single address. An allowed IP is a network,
// whose host bits the kernel clears; an interface address keeps them.
func wireguardAddress(text string, network bool) (netip.Prefix, error) {
	prefix, err := netip.ParsePrefix(text)
	if err != nil {
		address, addressErr := netip.ParseAddr(text)
		if addressErr != nil || address.Zone() != "" {
			return netip.Prefix{}, fmt.Errorf("%q is not an IP address or network; use an address or network such as 10.0.0.2/32", text)
		}
		prefix = netip.PrefixFrom(address, address.BitLen())
	}
	if network {
		prefix = prefix.Masked()
	}
	return prefix, nil
}

// wireguardKey reports whether text is a WireGuard key: 32 bytes in base64.
func wireguardKey(text string) bool {
	raw, err := base64.StdEncoding.DecodeString(text)
	return err == nil && len(raw) == 32
}

// newWireguardPrivateKey makes a private key as wg genkey does: 32 random
// bytes, clamped as Curve25519 uses them.
func newWireguardPrivateKey() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	clampWireguardKey(raw)
	return base64.StdEncoding.EncodeToString(raw), nil
}

func clampWireguardKey(raw []byte) {
	raw[0] &= 248
	raw[31] = raw[31]&127 | 64
}

// clampedWireguardKey is the form in which the kernel keeps a private key and
// wg show prints it.
func clampedWireguardKey(private string) string {
	raw, _ := base64.StdEncoding.DecodeString(private)
	if len(raw) != 32 {
		return private
	}
	clampWireguardKey(raw)
	return base64.StdEncoding.EncodeToString(raw)
}

// wireguardPublicKey is the public key of a private key, as wg pubkey prints it.
func wireguardPublicKey(private string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(private)
	if err != nil || len(raw) != 32 {
		return "", errors.New("not a WireGuard private key")
	}
	key, err := ecdh.X25519().NewPrivateKey(raw)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(key.PublicKey().Bytes()), nil
}

// withWireguardPrivateKey is text with the line PrivateKey = key as the first
// line of its [Interface] section.
func withWireguardPrivateKey(text string, f wireguardFile, key string) string {
	lines := strings.SplitAfter(text, "\n")
	header := lines[f.interfaceLine]
	if !strings.HasSuffix(header, "\n") {
		header += "\n"
	}
	return strings.Join(lines[:f.interfaceLine], "") + header + "PrivateKey = " + key + "\n" + strings.Join(lines[f.interfaceLine+1:], "")
}

// shownWireguardText is a wg-quick file as its difference shows it: the
// value of every PrivateKey and PresharedKey line, also of one in a
// comment, replaced by a note.
func shownWireguardText(text, key string) string {
	lines := strings.SplitAfter(text, "\n")
	for i, line := range lines {
		body := strings.TrimLeft(line, " \t#")
		equals := strings.Index(body, "=")
		if equals < 0 {
			continue
		}
		var note string
		switch strings.ToLower(strings.TrimSpace(body[:equals])) {
		case "privatekey":
			note = "(another private key, not shown)"
			if setting, _, _ := strings.Cut(body[equals+1:], "#"); strings.TrimSpace(setting) == key {
				note = "(the node's private key, not shown)"
			}
		case "presharedkey":
			note = "(a preshared key, not shown)"
		default:
			continue
		}
		ending := ""
		if strings.HasSuffix(line, "\n") {
			ending = "\n"
		}
		lines[i] = line[:len(line)-len(body)+equals+1] + " " + note + ending
	}
	return strings.Join(lines, "")
}

// wireguardRunning is what MMX reads of a running interface.
type wireguardRunning struct {
	exists     bool
	privateKey string
	listenPort int
	peers      map[string]wireguardPeer
	// addresses leaves out the link-local addresses that the kernel may
	// give an interface itself.
	addresses     []netip.Prefix
	linkAddresses []netip.Prefix
	mtu           int
	routes        []wireguardRoute
}

type wireguardRoute struct {
	destination netip.Prefix
	// kernel: the kernel's own route for an address's network, not one that
	// wg-quick added for an allowed IP.
	kernel bool
}

// readWireguardInterface reads the running interface: its addresses and MTU
// from ip, its key, port, and peers from wg show dump, and the routes on it
// in the table that the file names. The dump holds the private key: no
// error quotes it.
func (run Runtime) readWireguardInterface(ctx context.Context, name string, f wireguardFile) (wireguardRunning, error) {
	running := wireguardRunning{peers: map[string]wireguardPeer{}}
	output, exists, err := run.wireguardLink(ctx, name)
	if err != nil || !exists {
		return running, err
	}
	var links []struct {
		MTU       int `json:"mtu"`
		Addresses []struct {
			Local     string `json:"local"`
			PrefixLen int    `json:"prefixlen"`
			Scope     string `json:"scope"`
		} `json:"addr_info"`
	}
	if err := json.Unmarshal([]byte(output.StandardOutput), &links); err != nil || len(links) != 1 {
		return running, fmt.Errorf("ip -j address show dev %s printed no interface that MMX can read", name)
	}
	running.exists, running.mtu = true, links[0].MTU
	for _, a := range links[0].Addresses {
		address, err := netip.ParseAddr(a.Local)
		if err != nil {
			return running, fmt.Errorf("ip -j address show dev %s printed the address %q, which MMX cannot read", name, a.Local)
		}
		prefix := netip.PrefixFrom(address, a.PrefixLen)
		if a.Scope == "link" {
			running.linkAddresses = append(running.linkAddresses, prefix)
		} else {
			running.addresses = append(running.addresses, prefix)
		}
	}
	output = run.RunCommand(ctx, []string{"wg", "show", name, "dump"}, "")
	if output.Error != "" || output.ExitCode != 0 {
		return running, fmt.Errorf("wg show %s dump: exit code %d: %s%s", name, output.ExitCode, strings.TrimSpace(output.StandardError), output.Error)
	}
	if err := running.readDump(output.StandardOutput); err != nil {
		return running, fmt.Errorf("wg show %s dump: %w", name, err)
	}
	table := strings.ToLower(f.table)
	if table == "off" {
		return running, nil
	}
	if table == "" || table == "auto" {
		table = "main"
	} else {
		table = f.table
	}
	families := []string{"-4"}
	if wireguardUsesIPv6(f, running) {
		families = append(families, "-6")
	}
	for _, family := range families {
		output = run.RunCommand(ctx, []string{"ip", "-j", family, "route", "show", "table", table, "dev", name}, "")
		if err := commandFailure(output, "ip "+family+" route show table "+table+" dev "+name); err != nil {
			return running, err
		}
		var routes []struct {
			Destination string `json:"dst"`
			Protocol    string `json:"protocol"`
		}
		if text := strings.TrimSpace(output.StandardOutput); text != "" {
			if err := json.Unmarshal([]byte(text), &routes); err != nil {
				return running, fmt.Errorf("ip -j %s route show table %s dev %s printed routes that MMX cannot read", family, table, name)
			}
		}
		for _, route := range routes {
			destination := route.Destination
			if destination == "default" {
				destination = map[string]string{"-4": "0.0.0.0/0", "-6": "::/0"}[family]
			}
			prefix, err := wireguardAddress(destination, true)
			if err != nil {
				return running, fmt.Errorf("ip -j %s route show table %s dev %s printed the route %q, which MMX cannot read", family, table, name, route.Destination)
			}
			running.routes = append(running.routes, wireguardRoute{prefix, route.Protocol == "kernel"})
		}
	}
	return running, nil
}

// wireguardLink asks ip for the interface: its listing, or that it does not
// exist.
func (run Runtime) wireguardLink(ctx context.Context, name string) (CommandOutput, bool, error) {
	output := run.RunCommand(ctx, []string{"ip", "-j", "address", "show", "dev", name}, "")
	if output.Error == "" && output.ExitCode != 0 && strings.Contains(output.StandardError, "does not exist") {
		return output, false, nil
	}
	return output, true, commandFailure(output, "ip address show dev "+name)
}

func wireguardUsesIPv6(f wireguardFile, running wireguardRunning) bool {
	for _, peers := range []map[string]wireguardPeer{f.peers, running.peers} {
		for _, peer := range peers {
			for _, prefix := range peer.allowedIPs {
				if prefix.Addr().Is6() {
					return true
				}
			}
		}
	}
	return false
}

// readDump reads wg show <name> dump: a line with the private key, the
// public key, the listen port, and the firewall mark, then one line for each
// peer with its public key, preshared key, endpoint, allowed IPs, latest
// handshake, bytes received and sent, and keepalive, separated by tabs.
func (r *wireguardRunning) readDump(text string) error {
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	device := strings.Split(lines[0], "\t")
	if len(device) != 4 {
		return errors.New("the first line of wg dump output must contain four tab-separated interface fields")
	}
	if device[0] != "(none)" {
		r.privateKey = device[0]
	}
	port, err := strconv.Atoi(device[2])
	if err != nil {
		return errors.New("the interface listen port in wg dump output is not an integer")
	}
	r.listenPort = port
	for i, line := range lines[1:] {
		fields := strings.Split(line, "\t")
		if len(fields) != 8 {
			return fmt.Errorf("line %d of wg dump output must contain eight tab-separated peer fields", i+2)
		}
		peer := wireguardPeer{presharedKey: fields[1] != "(none)"}
		if fields[3] != "(none)" {
			for _, text := range strings.Split(fields[3], ",") {
				prefix, err := wireguardAddress(text, true)
				if err != nil {
					return fmt.Errorf("line %d: %w", i+2, err)
				}
				peer.allowedIPs = append(peer.allowedIPs, prefix)
			}
		}
		slices.SortFunc(peer.allowedIPs, func(a, b netip.Prefix) int { return strings.Compare(a.String(), b.String()) })
		if fields[7] != "off" {
			if peer.keepalive, err = strconv.Atoi(fields[7]); err != nil {
				return fmt.Errorf("line %d of wg dump output: the peer keepalive interval is not an integer", i+2)
			}
		}
		r.peers[fields[0]] = peer
	}
	return nil
}

// wireguardDifference is one way in which the running interface differs
// from its file.
type wireguardDifference struct {
	// remains says what differs, as the check after a change reports it,
	// and why a restart is needed.
	remains string
	// reload is what a reload changes; it is empty for a difference that
	// only a restart applies.
	reload string
}

// wireguardDifferences compares the running interface with the file and
// the node's private key. A peer with the node's own public key is left out,
// as WireGuard leaves it out.
func wireguardDifferences(name string, f wireguardFile, privateKey string, running wireguardRunning) []wireguardDifference {
	if !running.exists {
		return []wireguardDifference{{remains: "the interface " + name + " does not exist"}}
	}
	var found []wireguardDifference
	if running.privateKey != clampedWireguardKey(privateKey) {
		found = append(found, wireguardDifference{name + " has another private key than the node's", "set the node's private key"})
	}
	if f.listenPort != nil && *f.listenPort != 0 && *f.listenPort != running.listenPort {
		found = append(found, wireguardDifference{fmt.Sprintf("%s listens on port %d, not %d", name, running.listenPort, *f.listenPort),
			fmt.Sprintf("changed the listen port to %d", *f.listenPort)})
	}
	own, _ := wireguardPublicKey(privateKey)
	for _, key := range slices.Sorted(func(yield func(string) bool) {
		for key := range f.peers {
			if key != own && !yield(key) {
				return
			}
		}
	}) {
		want, have := f.peers[key], running.peers[key]
		if _, there := running.peers[key]; !there {
			found = append(found, wireguardDifference{"peer " + key + " is missing on " + name, "added peer " + key})
			continue
		}
		if want.allowed() != have.allowed() {
			found = append(found, wireguardDifference{fmt.Sprintf("peer %s has the allowed IPs %s, not %s", key, have.allowed(), want.allowed()),
				"changed the allowed IPs of peer " + key})
		}
		// wg syncconf of wireguard-tools before May 2025 keeps a preshared
		// key, and before February 2026 a keepalive, that the file no longer
		// sets, so only a restart removes them.
		if want.keepalive != 0 && want.keepalive != have.keepalive {
			found = append(found, wireguardDifference{fmt.Sprintf("peer %s has a keepalive of %d seconds, not %d", key, have.keepalive, want.keepalive),
				"changed the keepalive of peer " + key})
		} else if have.keepalive != want.keepalive {
			found = append(found, wireguardDifference{remains: fmt.Sprintf("peer %s has a keepalive of %d seconds that the file does not set", key, have.keepalive)})
		}
		if want.presharedKey && !have.presharedKey {
			found = append(found, wireguardDifference{"peer " + key + " has no preshared key, but the file sets one", "set the preshared key of peer " + key})
		} else if have.presharedKey && !want.presharedKey {
			found = append(found, wireguardDifference{remains: "peer " + key + " has a preshared key that the file does not set"})
		}
	}
	for _, key := range slices.Sorted(func(yield func(string) bool) {
		for key := range running.peers {
			if !yield(key) {
				return
			}
		}
	}) {
		if _, listed := f.peers[key]; !listed || key == own {
			found = append(found, wireguardDifference{"peer " + key + " is on " + name + " but not in the file", "removed peer " + key})
		}
	}
	for _, address := range f.addresses {
		if !slices.Contains(running.addresses, address) && !slices.Contains(running.linkAddresses, address) {
			found = append(found, wireguardDifference{remains: "the address " + address.String() + " is missing on " + name})
		}
	}
	for _, address := range running.addresses {
		if !slices.Contains(f.addresses, address) {
			found = append(found, wireguardDifference{remains: "the address " + address.String() + " is on " + name + " but not in the file"})
		}
	}
	if f.mtu != 0 && f.mtu != running.mtu {
		found = append(found, wireguardDifference{remains: fmt.Sprintf("the MTU of %s is %d, not %d", name, running.mtu, f.mtu)})
	}
	return append(found, wireguardRouteDifferences(name, f, own, running)...)
}

// wireguardRouteDifferences finds the routes that wg-quick up would add and
// that a reload does not: a route on the interface for each allowed IP that
// no route there covers yet, in the main table unless Table names another.
// wg-quick routes an allowed IP of /0 in the main table through its own
// rules, which MMX does not compare. A route that wg-quick added for an
// allowed IP that leaves the interface stays after a reload.
func wireguardRouteDifferences(name string, f wireguardFile, own string, running wireguardRunning) []wireguardDifference {
	table := strings.ToLower(f.table)
	if table == "off" {
		return nil
	}
	var found []wireguardDifference
	listed := map[netip.Prefix]bool{}
	for _, key := range slices.Sorted(func(yield func(string) bool) {
		for key := range f.peers {
			if !yield(key) {
				return
			}
		}
	}) {
		if key == own {
			continue
		}
		for _, prefix := range f.peers[key].allowedIPs {
			listed[prefix] = true
			if prefix.Bits() == 0 && (table == "" || table == "auto") {
				continue
			}
			if !slices.ContainsFunc(running.routes, func(r wireguardRoute) bool {
				return r.destination.Addr().Is4() == prefix.Addr().Is4() && r.destination.Bits() <= prefix.Bits() && r.destination.Contains(prefix.Addr())
			}) {
				found = append(found, wireguardDifference{remains: fmt.Sprintf("the allowed IP %s of peer %s has no route on %s", prefix, key, name)})
			}
		}
	}
	for _, peer := range running.peers {
		for _, prefix := range peer.allowedIPs {
			if !listed[prefix] && slices.Contains(running.routes, wireguardRoute{destination: prefix}) {
				found = append(found, wireguardDifference{remains: fmt.Sprintf("the route of %s on %s, for an allowed IP that leaves the interface, stays until a restart", prefix, name)})
			}
		}
	}
	return found
}

// wireguardQuickChanges names the settings that only wg-quick reads and that
// changed from the previous file to the next.
func wireguardQuickChanges(previous, next wireguardFile) []string {
	var changed []string
	for _, key := range wireguardQuickSettings {
		values := func(f wireguardFile) []string {
			var found []string
			for _, setting := range f.quickSettings {
				if setting[0] == key {
					found = append(found, setting[1])
				}
			}
			return found
		}
		if !slices.Equal(values(previous), values(next)) {
			changed = append(changed, wireguardSettingNames[key]+" changed in the file")
		}
	}
	return changed
}

// wireguardPlan is what brings the running interface to its file through
// wg-quick@<name>.service.
type wireguardPlan struct {
	// verb is start (with enable --now), reload, restart, or empty.
	verb string
	// enable: the service is not enabled, and verb does not enable it.
	enable bool
	// why lists, for reload, what the reload changes, and for restart, why
	// a reload is not enough.
	why []string
}

// planWireguard chooses the action. A service that is not active starts. A
// running one restarts for what wg-quick strip drops, an address, the MTU,
// and a route, which only wg-quick up sets, and reloads otherwise: wg
// syncconf changes only what differs, and the other peers keep their
// sessions.
func planWireguard(fileChanged bool, quickChanges []string, differences []wireguardDifference, service ServiceState) wireguardPlan {
	if service.Active != "active" {
		return wireguardPlan{verb: "start"}
	}
	plan := wireguardPlan{enable: service.Enabled != "enabled"}
	var restart, reload []string
	restart = append(restart, quickChanges...)
	for _, d := range differences {
		if d.reload == "" {
			restart = append(restart, d.remains)
		} else {
			reload = append(reload, d.reload)
		}
	}
	switch {
	case len(restart) > 0:
		plan.verb, plan.why = "restart", restart
	case fileChanged || len(reload) > 0:
		plan.verb, plan.why = "reload", reload
	}
	return plan
}

// wireguardList joins up to ten items, then counts the rest.
func wireguardList(items []string, separator string) string {
	if len(items) > 10 {
		return strings.Join(items[:10], separator) + fmt.Sprintf("%sand %d more", separator, len(items)-10)
	}
	return strings.Join(items, separator)
}

// wireguardSteps gathers what a task did, or in a dryrun would do, into one
// result.
type wireguardSteps struct {
	dryrun  bool
	change  string
	actions []string
}

func (s *wireguardSteps) did(past, present string) {
	s.change = protocol.CombineChange(s.change, map[bool]string{false: "changed", true: "would_change"}[s.dryrun])
	if s.dryrun {
		s.actions = append(s.actions, "would "+present)
	} else {
		s.actions = append(s.actions, past)
	}
}

func (s *wireguardSteps) result() protocol.Result {
	r := protocol.Success(s.change)
	r.Reason = strings.Join(s.actions, ", ")
	return r
}

// failed is a failure after the steps so far; change is the failed step's.
func (s *wireguardSteps) failed(err error, change string) protocol.Result {
	r := protocol.Failed(err)
	r.Change = protocol.CombineChange(s.change, change)
	if len(s.actions) > 0 {
		r.Reason = strings.Join(s.actions, ", ") + "; then " + r.Reason
	}
	return r
}

// wireguardPortBlockedByAppArmor says why the interface cannot open a
// ListenPort below 1024, or returns "". Ubuntu 26.04's apparmor package
// confines wg-quick, and its child profile wg-quick//ip, which brings the
// interface up and so opens the port, lacks capability net_bind_service. A
// local include cannot add it, because only the parent profile has one.
// The check reads the profile, so a fixed or changed profile passes.
func wireguardPortBlockedByAppArmor(places wireguardPlaces, name string, port int) string {
	if port <= 0 || port >= 1024 || places.apparmorProfiles == "" {
		return ""
	}
	loaded, err := os.ReadFile(places.apparmorProfiles)
	if err != nil || !slices.Contains(strings.Split(string(loaded), "\n"), "wg-quick//ip (enforce)") {
		return ""
	}
	profile, err := os.ReadFile(places.wgQuickProfile)
	if err != nil || wireguardIPProfileBindsLowPorts(string(profile)) {
		return ""
	}
	return fmt.Sprintf("ListenPort = %d is below 1024, but on this node the AppArmor profile wg-quick//ip, in %s, lacks capability net_bind_service. wg-quick brings %s up through that profile, so the interface could not open UDP port %d. Use a ListenPort of 1024 or above, or change the node's AppArmor policy so that wg-quick//ip may bind the port, then run again.", port, places.wgQuickProfile, name, port)
}

// wireguardIPProfileBindsLowPorts reports whether the ip child profile of
// the wg-quick profile has a capability rule for net_bind_service or for
// every capability.
func wireguardIPProfileBindsLowPorts(profile string) bool {
	depth := 0
	for _, line := range strings.Split(profile, "\n") {
		line, _, _ = strings.Cut(line, "#")
		rule := strings.Fields(strings.ReplaceAll(line, ",", " , "))
		if depth == 0 {
			if len(rule) >= 2 && rule[0] == "profile" && rule[1] == "ip" {
				depth = strings.Count(line, "{")
			}
			continue
		}
		for len(rule) > 0 && (rule[0] == "audit" || rule[0] == "allow") {
			rule = rule[1:]
		}
		if len(rule) >= 2 && rule[0] == "capability" && (rule[1] == "," || slices.Contains(rule, "net_bind_service")) {
			return true
		}
		depth += strings.Count(line, "{") - strings.Count(line, "}")
	}
	return false
}

func wireguardPrerequisites() []string {
	return []string{"wg", "wg-quick", "ip", "systemctl"}
}

func (run Runtime) wireguardInterface(ctx context.Context, w WireguardInterface, s value.Scope, places wireguardPlaces, owed OwedRestoreService) (result protocol.Result) {
	publicKey := ""
	if key, _, created, err := wireguardPrivateKey(places, w.Name); err == nil && !created {
		publicKey, _ = wireguardPublicKey(key)
	}
	defer func() { result.PublicKey = publicKey; result.NoOtherTry = true }()
	if owed == nil {
		result = protocol.Failed(fmt.Errorf("Changing a WireGuard interface requires the node's owed restore service."))
		result.Change = "unchanged"
		return result
	}
	if err := owed.CheckChangeAllowed(ctx, places.file(w.Name)); err != nil {
		result = protocol.Failed(err)
		result.Change = "unchanged"
		return result
	}
	previousText, previousExists, err := readWireguardNodeFile(places, w.Name)
	if err != nil {
		result = protocol.Failed(err)
		result.Change = "unchanged"
		return result
	}
	if wireguardSavesRunningConfiguration(previousText) {
		path := places.file(w.Name)
		result = protocol.ConfigurationError(fmt.Errorf("WireGuard configuration file %s sets SaveConfig = true, so wg-quick would write the running settings over the file that MMX restores if the change is not confirmed. Remove the SaveConfig line from %s, then run again.", path, path))
		result.Change = "unchanged"
		return result
	}
	if w.State == "absent" {
		return run.removeWireguardInterface(ctx, w, places, owed)
	}
	unit, path, keyPath := wireguardUnit(w.Name), places.file(w.Name), places.key(w.Name)
	for _, program := range wireguardPrerequisites() {
		if _, err := lookPath(ctx, program); err != nil {
			return run.notYet(fmt.Errorf("wireguard_interface: %w", missingProgramError(program)))
		}
	}
	manager := UnitManager{}
	service, err := manager.Inspect(ctx, unit)
	if err != nil {
		return protocol.Failed(err)
	}
	if service.Load == "not-found" {
		return run.notYet(fmt.Errorf("wireguard_interface needs the unit wg-quick@.service, which systemd does not find; install the package wireguard-tools in an earlier node task"))
	}
	content, err := OpenFileContents(File{Content: w.Content, Template: w.Template, TemplateText: w.TemplateText}, s)
	if err != nil {
		return protocol.Failed(err)
	}
	data, err := io.ReadAll(content)
	content.Close()
	if err != nil {
		return protocol.Failed(err)
	}
	text := string(data)
	file, err := parseWireguardFile(text, false)
	if err != nil {
		return protocol.ConfigurationError(fmt.Errorf("wireguard_interface %s: %w", w.Name, err))
	}
	if file.listenPort != nil {
		if reason := wireguardPortBlockedByAppArmor(places, w.Name, *file.listenPort); reason != "" {
			result = protocol.ConfigurationError(errors.New(reason))
			result.Change = "unchanged"
			return result
		}
	}
	steps := &wireguardSteps{dryrun: run.Dryrun, change: "unchanged"}

	// The directory, then the private key. Neither file has a difference of
	// its own: the key's would show the key, and the file's is the one below.
	written := run
	written.RetainDiff = nil
	if info, err := os.Lstat(places.directory); err != nil && !errors.Is(err, os.ErrNotExist) {
		return protocol.Failed(err)
	} else if made := run.MakeDirectory(ctx, places.directory, string(places.owner), string(places.group), "0700"); made.Error != "" {
		return steps.failed(fmt.Errorf("%s: %s", places.directory, made.Error), "unknown")
	} else if made.Changed && info == nil {
		steps.did("created "+places.directory, "create "+places.directory)
		// A dryrun's file below goes into the directory that this node
		// task makes first, as into an earlier node task's.
		written.PreviewPaths = append(slices.Clone(run.PreviewPaths), places.directory)
	} else if made.Changed {
		steps.did("made "+places.directory+" private to root", "make "+places.directory+" private to root")
	}
	key, keyText, created, err := wireguardPrivateKey(places, w.Name)
	if err != nil {
		return steps.failed(err, "unchanged")
	}
	// The key never shows in what the task reports: the node task execution
	// engine replaces it wherever a text of the result holds it.
	s.FormOfLoginHandedOnlyToATool(key)
	s.FormOfLoginHandedOnlyToATool(clampedWireguardKey(key))
	if created && run.Dryrun {
		steps.did("", "create the private key "+keyPath)
	} else if r := written.WriteFile(ctx, keyPath, keyText, string(places.owner), string(places.group), "0600"); r.Error != "" {
		return steps.failed(fmt.Errorf("%s: %s", keyPath, r.Error), "unknown")
	} else if created {
		steps.did("created the private key "+keyPath, "")
	} else if r.Changed {
		steps.did("made "+keyPath+" private to root", "make "+keyPath+" private to root")
	}
	knownPublicKey, err := wireguardPublicKey(key)
	if err != nil {
		return steps.failed(err, "unchanged")
	}

	if !(run.Dryrun && created) {
		publicKey = knownPublicKey
	}
	// The file.
	next := withWireguardPrivateKey(text, file, key)
	managedFile := File{Path: path, Literal: &next, Owner: places.owner, Group: places.group, Mode: "0600"}
	preview := written
	preview.Dryrun = true
	_, fileResult := preview.PrepareFileChange(ctx, managedFile, nil, nil)
	change := fileResult.FileChange
	if change != nil && previousText != next {
		change.Diff, change.DiffOmitted, change.FullDiff, change.FullDiffOmitted = "", "", nil, ""
		shown := run
		shown.RetainDiff = func(string) (*assets.Ref, string) {
			return nil, "full difference not kept: the file holds the node's private key"
		}
		shown.shownTextDiff(change, []byte(shownWireguardText(previousText, key)), []byte(shownWireguardText(next, key)))
	}
	if fileResult.Status != "succeeded" {
		r := steps.failed(errors.New(fileResult.Reason), fileResult.Change)
		r.FileChange = change
		return r
	}
	recordFileChange := func() {
		if fileResult.Change != "unchanged" && previousText != next {
			steps.did("wrote "+path, "write "+path)
		} else if fileResult.Change != "unchanged" {
			steps.did("made "+path+" private to root", "make "+path+" private to root")
		}
	}

	// The running interface.
	var quickChanges []string
	if previous, err := parseWireguardFile(previousText, true); previousExists && err == nil {
		quickChanges = wireguardQuickChanges(previous, file)
	} else if service.Active == "active" {
		quickChanges = []string{"MMX cannot read the previous " + path}
		if !previousExists {
			quickChanges = []string{"the previous " + path + " is missing"}
		}
	}
	var differences []wireguardDifference
	if service.Active == "active" {
		running, err := run.readWireguardInterface(ctx, w.Name, file)
		if err != nil {
			r := steps.failed(err, "unchanged")
			r.FileChange = change
			return r
		}
		differences = wireguardDifferences(w.Name, file, key, running)
	}
	plan := planWireguard(previousText != next, quickChanges, differences, service)
	if run.Dryrun {
		recordFileChange()
		result = run.actOnWireguardService(ctx, w.Name, plan, steps)
		result.FileChange = change
		if result.Change == "would_change" {
			result.Reason += wireguardDryrunReachabilityNote
		}
		return result
	}
	if fileResult.Change != "unchanged" {
		prepared, result := written.PrepareFileChange(ctx, managedFile, nil, nil)
		if prepared == nil {
			return result
		}
		defer func() { _ = prepared.Close() }()
		// Only the masked comparison may leave the module, including through
		// the owed restore service's installed-file result.
		prepared.change = change
		saved, err := json.Marshal(wireguardRestoreData{Action: plan.verb, Enable: plan.enable, Reasons: plan.why,
			PreviousServiceActive: service.Active == "active", PreviousServiceEnabled: service.Enabled == "enabled"})
		if err != nil {
			return steps.failed(err, "unchanged")
		}
		wait, _ := (NetworkFile{RestoreUnlessControllerConfirmsWithin: w.RestoreUnlessControllerConfirmsWithin}).confirmationWait()
		result = owed.ApplyChange(ctx, ChangeWithOwedRestore{PreparedFile: prepared, RestoreConfirmationWait: wait, ModuleRestoreData: saved})
		if result.Status == "succeeded" {
			recordFileChange()
			for _, call := range wireguardServiceCalls(w.Name, plan) {
				steps.did(call.past, call.present)
			}
			result.Reason = strings.Join(append(steps.actions, result.Reason), ", ")
			result.Reason = strings.TrimSuffix(result.Reason, ", ")
		}
		result.Change = protocol.CombineChange(steps.change, result.Change)
		return result
	}
	recordFileChange()
	result = run.actOnWireguardService(ctx, w.Name, plan, steps)
	if result.Status == "succeeded" && !run.Dryrun && (plan.verb != "" || plan.enable) {
		result = run.checkWireguardInterface(ctx, w.Name, path, file, key, steps, result)
	}
	result.FileChange = change
	return result
}

// wireguardPrivateKey reads the interface's private key file, or makes a new
// key when there is none. text is what the file holds or will hold. An error
// never quotes the file.
func wireguardPrivateKey(places wireguardPlaces, name string) (key, text string, created bool, err error) {
	path := places.key(name)
	directory, err := os.OpenRoot(places.directory)
	if err == nil {
		text, err = readTextAt(directory, filepath.Base(path))
		directory.Close()
	}
	if errors.Is(err, os.ErrNotExist) {
		key, err = newWireguardPrivateKey()
		return key, key + "\n", true, err
	}
	if err != nil {
		return "", "", false, fmt.Errorf("%s: %w", path, err)
	}
	key = strings.TrimSpace(text)
	if !wireguardKey(key) {
		return "", "", false, fmt.Errorf("%s holds no WireGuard private key; remove it with a file task (state: absent), and the next run creates a new key", path)
	}
	return key, text, false, nil
}

// readWireguardNodeFile reads the interface's file on the node as it is now.
func readWireguardNodeFile(places wireguardPlaces, name string) (text string, exists bool, err error) {
	directory, err := os.OpenRoot(places.directory)
	if err == nil {
		text, err = readTextAt(directory, filepath.Base(places.file(name)))
		directory.Close()
	}
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("%s: %w", places.file(name), err)
	}
	return text, true, nil
}

// wireguardServiceCall is one native service command and its result text.
type wireguardServiceCall struct {
	past, present string
	arguments     []string
}

func wireguardServiceCalls(name string, plan wireguardPlan) []wireguardServiceCall {
	unit := wireguardUnit(name)
	var calls []wireguardServiceCall
	if plan.verb == "start" {
		calls = append(calls, wireguardServiceCall{"started " + unit, "start " + unit, []string{"enable", "--now", "--", unit}})
	} else if plan.enable {
		calls = append(calls, wireguardServiceCall{"enabled " + unit, "enable " + unit, []string{"enable", "--", unit}})
	}
	why := wireguardList(plan.why, "; ")
	switch plan.verb {
	case "reload":
		past, present := "reloaded "+unit, "reload "+unit
		if why != "" {
			past, present = past+": "+why, present+": "+why
		}
		calls = append(calls, wireguardServiceCall{past, present, []string{"reload", "--", unit}})
	case "restart":
		calls = append(calls, wireguardServiceCall{"restarted " + unit + " because " + why, "restart " + unit + " because " + why, []string{"restart", "--", unit}})
	}
	return calls
}

// actOnWireguardService carries out the plan, or in a dryrun reports it.
func (run Runtime) actOnWireguardService(ctx context.Context, name string, plan wireguardPlan, steps *wireguardSteps) protocol.Result {
	unit, manager := wireguardUnit(name), UnitManager{}
	for _, c := range wireguardServiceCalls(name, plan) {
		if !run.Dryrun {
			if err := run.Mutation(unit); err != nil {
				return steps.failed(err, "unchanged")
			}
			asked := JournalNow()
			if r := manager.systemctl(ctx, c.arguments...); r.Status != "succeeded" {
				verb := c.arguments[0]
				if verb == "enable" && len(c.arguments) > 2 {
					verb = "start"
				}
				return steps.failed(fmt.Errorf("systemctl %s: %s: %s%s", strings.Join(c.arguments, " "), r.Reason, strings.TrimSpace(r.Stderr),
					manager.JournalAfterFailedAction(ctx, verb, unit, asked)), "unknown")
			}
		}
		steps.did(c.past, c.present)
	}
	return steps.result()
}

// checkWireguardInterface reads the interface and the service again after a
// change and fails the task, naming what still differs from the file.
func (run Runtime) checkWireguardInterface(ctx context.Context, name, path string, f wireguardFile, key string, steps *wireguardSteps, result protocol.Result) protocol.Result {
	unit := wireguardUnit(name)
	service, err := UnitManager{}.Inspect(ctx, unit)
	if err != nil {
		return steps.failed(err, "changed")
	}
	var remains []string
	if service.Active != "active" {
		remains = append(remains, unit+" is "+service.Active+", not active")
	}
	if service.Enabled != "enabled" {
		remains = append(remains, unit+" is "+service.Enabled+", not enabled")
	}
	running, err := run.readWireguardInterface(ctx, name, f)
	if err != nil {
		return steps.failed(err, "changed")
	}
	for _, d := range wireguardDifferences(name, f, key, running) {
		remains = append(remains, d.remains)
	}
	if len(remains) > 0 {
		return steps.failed(fmt.Errorf("%s still differs from %s: %s", name, path, wireguardList(remains, "; ")), "changed")
	}
	return result
}

const wireguardDryrunReachabilityNote = ". Whether wg-quick accepts the proposed network configuration change and whether the node remains reachable after activation were not checked."

// removeWireguardInterface removes the key only after the configuration
// removal has been accepted. The key survives a restored removal.
func (run Runtime) removeWireguardInterface(ctx context.Context, w WireguardInterface, places wireguardPlaces, owed OwedRestoreService) protocol.Result {
	path, keyPath, unit := places.file(w.Name), places.key(w.Name), wireguardUnit(w.Name)
	steps := &wireguardSteps{dryrun: run.Dryrun, change: "unchanged"}
	managedFile := File{Path: path, State: "absent"}
	preview := run
	preview.Dryrun = true
	_, compared := preview.PrepareFileChange(ctx, managedFile, nil, nil)
	if compared.Status != "succeeded" {
		return compared
	}
	if compared.Change != "unchanged" {
		for _, program := range wireguardPrerequisites() {
			if _, err := lookPath(ctx, program); err != nil {
				result := run.notYet(fmt.Errorf("Cannot remove WireGuard configuration file %s: %w", path, missingProgramError(program)))
				if !run.Dryrun {
					result.Change = "unchanged"
				}
				return result
			}
		}
	}
	manager := UnitManager{}
	service := ServiceState{Active: "inactive", Enabled: "disabled"}
	if _, err := lookPath(ctx, "systemctl"); err == nil {
		var err error
		service, err = manager.Inspect(ctx, unit)
		if err != nil {
			return protocol.Failed(err)
		}
	}
	if run.Dryrun {
		if service.Load != "not-found" && (service.Active != "inactive" || service.Enabled == "enabled") {
			steps.did("", "stop and disable "+unit)
		}
		if compared.Change != "unchanged" {
			steps.did("", "remove "+path)
		}
		keyResult := run.File(ctx, File{Path: keyPath, State: "absent"}, nil)
		if keyResult.Status != "succeeded" {
			return keyResult
		}
		if keyResult.Change != "unchanged" {
			steps.did("", "remove "+keyPath)
		}
		result := steps.result()
		result.FileChange = compared.FileChange
		if result.Change == "would_change" {
			result.Reason += wireguardDryrunReachabilityNote
		}
		return result
	}
	result := compared
	if compared.Change != "unchanged" {
		prepared, preparedResult := run.PrepareFileChange(ctx, managedFile, nil, nil)
		if prepared == nil {
			return preparedResult
		}
		defer func() { _ = prepared.Close() }()
		saved, err := json.Marshal(wireguardRestoreData{Action: "stop", PreviousServiceActive: service.Active == "active", PreviousServiceEnabled: service.Enabled == "enabled"})
		if err != nil {
			return protocol.Failed(err)
		}
		wait, _ := (NetworkFile{RestoreUnlessControllerConfirmsWithin: w.RestoreUnlessControllerConfirmsWithin}).confirmationWait()
		result = owed.ApplyChange(ctx, ChangeWithOwedRestore{PreparedFile: prepared, RestoreConfirmationWait: wait, ModuleRestoreData: saved})
		if result.Status != "succeeded" {
			return result
		}
	} else {
		if err := stopWireguardService(ctx, run, w.Name, service); err != nil {
			return steps.failed(err, "unknown")
		}
	}
	if service.Load != "not-found" && (service.Active != "inactive" || service.Enabled == "enabled") {
		steps.did("stopped and disabled "+unit, "stop and disable "+unit)
	}
	if compared.Change != "unchanged" {
		steps.did("removed "+path, "remove "+path)
	}
	keyResult := run.File(ctx, File{Path: keyPath, State: "absent"}, nil)
	if keyResult.Status != "succeeded" {
		failed := steps.failed(errors.New(keyResult.Reason), keyResult.Change)
		failed.FileChange, failed.OwedRestore = result.FileChange, result.OwedRestore
		return failed
	}
	if keyResult.Change != "unchanged" {
		steps.did("removed "+keyPath, "remove "+keyPath)
	}
	result.Change = protocol.CombineChange(result.Change, steps.change)
	result.Reason = strings.TrimSuffix(strings.Join(append(steps.actions, result.Reason), ", "), ", ")
	if steps.change != "unchanged" {
		if err := checkWireguardAbsent(ctx, run, w.Name); err != nil {
			failed := steps.failed(err, "changed")
			failed.FileChange, failed.OwedRestore = result.FileChange, result.OwedRestore
			return failed
		}
	}
	return result
}
