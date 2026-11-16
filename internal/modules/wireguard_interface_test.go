package modules

import (
	"context"
	"crypto/ecdh"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"mmx/internal/assets"
	"mmx/internal/protocol"
	"mmx/internal/value"
)

// Public keys of example peers, as wg pubkey prints them.
const (
	wireguardPeerB = "xntON0r+ip4v53R2PDYOEKeq0RH91Y9x307uPnUQOTE="
	wireguardPeerC = "urzzcMKwVZ9bM84MmgWUjMRxOTNn2PDtEF5it/uZFQ4="
)

// wireguardTestFile is the file of an interface with peer B, as a task
// writes it.
const wireguardTestFile = "[Interface]\nAddress = 10.0.0.1/24\nListenPort = 51820\n\n[Peer]\nPublicKey = " + wireguardPeerB +
	"\nAllowedIPs = 10.0.0.2/32\nEndpoint = web02.example.net:51820\n"

func TestWireguardFileChecks(t *testing.T) {
	t.Parallel()
	peer := "[Peer]\nPublicKey = " + wireguardPeerB + "\nAllowedIPs = 10.0.0.2/32\n"
	for _, c := range []struct{ text, refusal string }{
		{"[Interface]\nPrivateKey = " + wireguardPeerB + "\n", "line 2: PrivateKey is refused: MMX adds the node's own private key"},
		{"[Interface]\nSaveConfig = true\n", "line 2: SaveConfig = true is refused"},
		{"[Interface]\nSaveConfig = maybe\n", "must be true or false"},
		{"[Interface]\n[Interface]\n", "line 2: a second [Interface] section"},
		{peer, "the file has no [Interface] section"},
		{"[Interface]\n[Peer]\nAllowedIPs = 10.0.0.2/32\n", "the [Peer] section on line 2 has no PublicKey"},
		{"[Interface]\n" + peer + "[Peer]\nPublicKey = " + wireguardPeerB + "\n", "the [Peer] section on line 5 repeats the PublicKey " + wireguardPeerB},
		{"[Interface]\n" + peer + "[Peer]\nPublicKey = " + wireguardPeerC + "\nAllowedIPs = 10.0.0.2\n", "the allowed IP 10.0.0.2/32 belongs to the peers"},
		{"[Interface]\n[Peer]\nPublicKey = not-a-key\n", "line 3: PublicKey must be a WireGuard key"},
		{"[Interface]\nAddress = 10.0.0.300/24\n", `line 2: Address "10.0.0.300/24" is not an IP address or network`},
		{"[Interface]\n" + peer + "AllowedIPs = 10.0.0.0/33\n", "line 5: AllowedIPs"},
		{"[Interface]\nListenPort = 70000\n", "line 2: ListenPort must be a port number"},
		{"[Interface]\nAllowedIP = 10.0.0.2/32\n", "line 2: AllowedIP is not a setting in [Interface]; supported settings are Address, DNS"},
		{"[Interface]\n[Peer]\nAddress = 10.0.0.2/32\n", "line 3: Address is not a setting in [Peer]; supported settings are PublicKey"},
		{"Address = 10.0.0.1/24\n[Interface]\n", "line 1: Address comes before the [Interface] or [Peer] section"},
		{"[Interface]\n[Peers]\n", "line 2: [Peers] is not a wg-quick section"},
		{"[Interface]\nAddress\n", "line 2 is not a section, comment, or setting"},
		{"[Interface]\n[Peer]\nPublicKey = " + wireguardPeerB + "\nPersistentKeepalive = soon\n", "PersistentKeepalive must be off or a number"},
	} {
		if _, err := parseWireguardFile(c.text, false); err == nil || !strings.Contains(err.Error(), c.refusal) {
			t.Errorf("%q: %v, want %q", c.text, err, c.refusal)
		}
	}
	// Names have any case, comments start anywhere, SaveConfig = false is
	// accepted, and an address without a prefix is a single address.
	f, err := parseWireguardFile("# wg0\n[interface]  # the node\naddress = 10.0.0.1/24, fd00::1/64\nmtu = 1380\nSaveConfig = false\nPostUp = echo up # a comment\n"+
		"[PEER]\npublickey = "+wireguardPeerB+"\nallowedips = 10.0.0.2, 10.20.1.0/16\npersistentkeepalive = 25\n", false)
	if err != nil {
		t.Fatal(err)
	}
	if f.interfaceLine != 1 || f.mtu != 1380 || f.listenPort != nil || fmt.Sprint(f.addresses) != "[10.0.0.1/24 fd00::1/64]" ||
		!reflect.DeepEqual(f.quickSettings, [][2]string{{"address", "10.0.0.1/24, fd00::1/64"}, {"mtu", "1380"}, {"saveconfig", "false"}, {"postup", "echo up"}}) {
		t.Fatalf("%+v", f)
	}
	if p := f.peers[wireguardPeerB]; p.allowed() != "10.0.0.2/32, 10.20.0.0/16" || p.keepalive != 25 {
		t.Fatalf("%+v", p)
	}
	// The node's own file holds the key, and may hold SaveConfig = true.
	if _, err := parseWireguardFile("[Interface]\nPrivateKey = "+wireguardPeerB+"\nSaveConfig = true\n", true); err != nil {
		t.Fatal(err)
	}
}

func TestWireguardInterfaceNameAndInputs(t *testing.T) {
	t.Parallel()
	for name, valid := range map[string]bool{"wg0": true, "wg-office.1_a": true, "abcdefghijklmno": true, "abcdefghijklmnop": false, "wg+0": false, "wg=0": false, "": false, "wg 0": false, "wg/0": false} {
		if err := wireguardNameRule(name); (err == nil) != valid {
			t.Errorf("%q: %v", name, err)
		}
	}
	content := wireguardTestFile
	for _, c := range []struct {
		w       WireguardInterface
		refusal string
	}{
		{WireguardInterface{Name: "wg0"}, "exactly one of content or template"},
		{WireguardInterface{Name: "wg0", Content: &content, Template: "wg0.tmpl"}, "exactly one of content or template"},
		{WireguardInterface{Name: "wg0", State: "absent", Content: &content}, "takes only name, state"},
		{WireguardInterface{Name: "wg0", State: "down", Content: &content}, "present or absent"},
		{WireguardInterface{Name: "wg0", Content: &content, RestoreUnlessControllerConfirmsWithin: "0s"}, "must be positive"},
		{WireguardInterface{Name: "wg0", Content: &content, RestoreUnlessControllerConfirmsWithin: "-1s"}, "must be positive"},
		{WireguardInterface{Name: "wg0", Content: &content, RestoreUnlessControllerConfirmsWithin: "later"}, "must be positive"},
	} {
		if err := c.w.Prepare(t.TempDir()); err == nil || !strings.Contains(err.Error(), c.refusal) {
			t.Errorf("%+v: %v", c.w, err)
		}
	}
	// The rule of content refuses the file before it reaches the node.
	refused := "[Interface]\nPrivateKey = " + wireguardPeerB + "\n"
	w := WireguardInterface{Name: "wg0", Content: &refused}
	var arguments ArgumentList
	w.Arguments(&arguments)
	if err := arguments.List[1].Rule(refused); err == nil || !strings.Contains(err.Error(), "PrivateKey is refused") {
		t.Fatal(err)
	}
	if err := arguments.List[1].Rule(content); err != nil {
		t.Fatal(err)
	}
}

// The module's page, embedded so that the test binary also runs outside the
// repository, as linux_code.py runs it on a test node.
//
//go:embed wireguard_interface.md
var wireguardInterfacePage string

// The template of the module's page: one [Peer] for each other node of the
// group whose public key the inventory has.
func TestWireguardInterfaceTemplateFromTheInventory(t *testing.T) {
	t.Parallel()
	_, after, _ := strings.Cut(wireguardInterfacePage, "# templates/wg0.conf.tmpl\n")
	template, _, _ := strings.Cut(after, "```")
	node := func(address, private string, public string) map[string]any {
		vars := map[string]any{"wireguard_address": private}
		if public != "" {
			vars["wireguard_public_key"] = public
		}
		return map[string]any{"address": address, "vars": vars}
	}
	scope := value.Scope{"node": map[string]any{"name": "web01"}, "vars": map[string]any{"wireguard_address": "10.0.0.1"},
		"inventory": map[string]any{"groups": map[string]any{"wireguard_mesh": []any{"web01", "web02", "db01"}},
			"nodes": map[string]any{"web01": node("203.0.113.11", "10.0.0.1", "PFHMlKh61wvN0/mEnwE2mCdTk3PCQmr/u0lh3F8JkT0="),
				"web02": node("203.0.113.12", "10.0.0.2", wireguardPeerB), "db01": node("203.0.113.21", "10.0.0.3", "")}}}
	text, err := scope.Template(template)
	if err != nil {
		t.Fatal(err)
	}
	f, err := parseWireguardFile(text, false)
	if err != nil {
		t.Fatal(err, text)
	}
	if len(f.peers) != 1 || f.peers[wireguardPeerB].allowed() != "10.0.0.2/32" || fmt.Sprint(f.addresses) != "[10.0.0.1/24]" || !strings.Contains(text, "Endpoint = 203.0.113.12:51820") {
		t.Fatal(text)
	}
}

// The comparison of the running interface with its file, and the action
// each difference needs.
func TestWireguardDifferencesChooseStartReloadOrRestart(t *testing.T) {
	t.Parallel()
	key, err := newWireguardPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	own, _ := wireguardPublicKey(key)
	file := func(text string) wireguardFile {
		f, err := parseWireguardFile(text, false)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	base := file(wireguardTestFile)
	routes := []wireguardRoute{{wireguardPrefix("10.0.0.0/24"), true}}
	matching := func() wireguardRunning {
		return wireguardRunning{exists: true, privateKey: key, listenPort: 51820, mtu: 1420, addresses: []netip.Prefix{wireguardPrefix("10.0.0.1/24")},
			peers: map[string]wireguardPeer{wireguardPeerB: {allowedIPs: []netip.Prefix{wireguardPrefix("10.0.0.2/32")}}}, routes: routes}
	}
	active := ServiceState{Active: "active", Enabled: "enabled"}
	plan := func(f wireguardFile, fileChanged bool, quick []string, running wireguardRunning, service ServiceState) wireguardPlan {
		return planWireguard(fileChanged, quick, wireguardDifferences("wg0", f, key, running), service)
	}
	if p := plan(base, false, nil, matching(), active); p.verb != "" || p.enable {
		t.Fatalf("a matching interface: %+v", p)
	}
	if p := plan(base, true, nil, matching(), active); p.verb != "reload" || len(p.why) != 0 {
		t.Fatalf("a changed file alone, such as an endpoint: %+v", p)
	}
	if p := plan(base, false, nil, matching(), ServiceState{Active: "active", Enabled: "disabled"}); p.verb != "" || !p.enable {
		t.Fatalf("a running service that is not enabled: %+v", p)
	}
	for _, service := range []ServiceState{{Active: "inactive", Enabled: "enabled"}, {Active: "failed", Enabled: "disabled"}} {
		if p := plan(base, true, nil, wireguardRunning{}, service); p.verb != "start" || p.enable {
			t.Fatalf("%+v: %+v", service, p)
		}
	}
	two := file(wireguardTestFile + "\n[Peer]\nPublicKey = " + wireguardPeerC + "\nAllowedIPs = 10.0.0.3/32, 10.30.0.0/16\n")
	for _, c := range []struct {
		name    string
		f       wireguardFile
		quick   []string
		running func(*wireguardRunning)
		verb    string
		why     string
	}{
		{"a peer to add", two, nil, func(r *wireguardRunning) {
			r.routes = append(r.routes, wireguardRoute{wireguardPrefix("10.30.0.0/16"), false})
		}, "reload", "added peer " + wireguardPeerC},
		{"a peer to remove", base, nil, func(r *wireguardRunning) {
			r.peers[wireguardPeerC] = wireguardPeer{allowedIPs: []netip.Prefix{wireguardPrefix("10.0.0.3/32")}}
		}, "reload", "removed peer " + wireguardPeerC},
		{"other allowed IPs", base, nil, func(r *wireguardRunning) {
			r.peers[wireguardPeerB] = wireguardPeer{allowedIPs: []netip.Prefix{wireguardPrefix("10.0.0.9/32")}}
		}, "reload", "changed the allowed IPs of peer " + wireguardPeerB},
		{"another keepalive", file(wireguardTestFile + "PersistentKeepalive = 25\n"), nil, func(r *wireguardRunning) {
			r.peers[wireguardPeerB] = wireguardPeer{allowedIPs: []netip.Prefix{wireguardPrefix("10.0.0.2/32")}, keepalive: 10}
		}, "reload", "changed the keepalive of peer " + wireguardPeerB},
		{"a keepalive that the file no longer sets", base, nil, func(r *wireguardRunning) {
			r.peers[wireguardPeerB] = wireguardPeer{allowedIPs: []netip.Prefix{wireguardPrefix("10.0.0.2/32")}, keepalive: 25}
		}, "restart", "peer " + wireguardPeerB + " has a keepalive of 25 seconds that the file does not set"},
		{"a preshared key to set", file(wireguardTestFile + "PresharedKey = " + wireguardPeerC + "\n"), nil, func(*wireguardRunning) {},
			"reload", "set the preshared key of peer " + wireguardPeerB},
		{"a preshared key that the file no longer sets", base, nil, func(r *wireguardRunning) {
			r.peers[wireguardPeerB] = wireguardPeer{allowedIPs: []netip.Prefix{wireguardPrefix("10.0.0.2/32")}, presharedKey: true}
		}, "restart", "peer " + wireguardPeerB + " has a preshared key that the file does not set"},
		{"another listen port", base, nil, func(r *wireguardRunning) { r.listenPort = 40000 }, "reload", "changed the listen port to 51820"},
		{"another private key", base, nil, func(r *wireguardRunning) { r.privateKey = wireguardPeerB }, "reload", "set the node's private key"},
		{"an Address line changed", base, []string{"Address changed in the file"}, func(*wireguardRunning) {}, "restart", "Address changed in the file"},
		{"a missing address", base, nil, func(r *wireguardRunning) { r.addresses = nil }, "restart", "the address 10.0.0.1/24 is missing on wg0"},
		{"an address not in the file", base, nil, func(r *wireguardRunning) { r.addresses = append(r.addresses, wireguardPrefix("10.9.0.1/24")) }, "restart", "the address 10.9.0.1/24 is on wg0 but not in the file"},
		{"another MTU", file(strings.Replace(wireguardTestFile, "ListenPort = 51820", "ListenPort = 51820\nMTU = 1380", 1)), nil, func(*wireguardRunning) {}, "restart", "the MTU of wg0 is 1420, not 1380"},
		{"a new peer that needs a route", two, nil, func(*wireguardRunning) {}, "restart", "the allowed IP 10.30.0.0/16 of peer " + wireguardPeerC + " has no route on wg0"},
		{"a leaving peer's route", base, nil, func(r *wireguardRunning) {
			r.peers[wireguardPeerC] = wireguardPeer{allowedIPs: []netip.Prefix{wireguardPrefix("10.30.0.0/16")}}
			r.routes = append(r.routes, wireguardRoute{wireguardPrefix("10.30.0.0/16"), false})
		}, "restart", "the route of 10.30.0.0/16 on wg0, for an allowed IP that leaves the interface, stays until a restart"},
		{"an interface that does not exist", base, nil, func(r *wireguardRunning) { *r = wireguardRunning{} }, "restart", "the interface wg0 does not exist"},
	} {
		running := matching()
		c.running(&running)
		p := plan(c.f, false, c.quick, running, active)
		if p.verb != c.verb || !slices.Contains(p.why, c.why) {
			t.Errorf("%s: %+v", c.name, p)
		}
	}
	// A peer with the node's own public key is left out, as WireGuard
	// leaves it out.
	withOwn := file(wireguardTestFile + "\n[Peer]\nPublicKey = " + own + "\nAllowedIPs = 10.0.0.1/32, 10.40.0.0/16\n")
	if d := wireguardDifferences("wg0", withOwn, key, matching()); len(d) != 0 {
		t.Errorf("a peer with the node's own key: %+v", d)
	}
	// wg-quick routes an allowed IP of /0 in the main table through its own
	// rules, and adds no routes for Table = off.
	everything := file(strings.Replace(wireguardTestFile, "AllowedIPs = 10.0.0.2/32", "AllowedIPs = 0.0.0.0/0", 1))
	running := matching()
	running.peers[wireguardPeerB] = everything.peers[wireguardPeerB]
	if d := wireguardDifferences("wg0", everything, key, running); len(d) != 0 {
		t.Errorf("an allowed IP of /0: %+v", d)
	}
	tableOff := file(strings.NewReplacer("ListenPort = 51820", "ListenPort = 51820\nTable = off", "AllowedIPs = 10.0.0.2/32", "AllowedIPs = 10.0.0.2/32, 10.60.0.0/16").Replace(wireguardTestFile))
	running = matching()
	running.routes = nil
	running.peers[wireguardPeerB] = tableOff.peers[wireguardPeerB]
	if d := wireguardDifferences("wg0", tableOff, key, running); len(d) != 0 {
		t.Errorf("Table = off: %+v", d)
	}
	// wg show wg0 dump prints a peer's preshared key in its second field.
	dumped := wireguardRunning{peers: map[string]wireguardPeer{}}
	if err := dumped.readDump("(none)\t" + own + "\t51820\toff\n" + wireguardPeerB + "\t" + wireguardPeerC + "\t(none)\t10.0.0.2/32\t0\t0\t0\toff\n"); err != nil {
		t.Fatal(err)
	}
	if !dumped.peers[wireguardPeerB].presharedKey {
		t.Errorf("a dumped preshared key: %+v", dumped.peers)
	}
}

func wireguardPrefix(text string) netip.Prefix {
	prefix, err := wireguardAddress(text, false)
	if err != nil {
		panic(err)
	}
	return prefix
}

// wireguardNode is a node's stand-in programs for wireguard_interface:
// systemctl, wg, wg-quick, ip, and journalctl, with the interface's running
// state in files of dir. A start, reload, or restart copies the files of
// dir/on-start, dir/on-reload, or dir/on-restart into dir, as wg-quick
// would bring the interface to its file; without them the interface stays
// as it is.
type wireguardNode struct {
	t              *testing.T
	dir            string
	places         wireguardPlaces
	restoreService *wireguardOwedRestoreForTest
}

// wireguardState is the running interface as the stand-ins print it.
type wireguardState struct {
	listenPort int
	peers      []string // public key, allowed IPs, and keepalive of each peer
	addresses  []string
	routes     []string // a destination, and "kernel" for the kernel's own route
}

func newWireguardNode(t *testing.T) *wireguardNode {
	t.Helper()
	root := t.TempDir()
	places := wireguardPlaces{directory: filepath.Join(root, "wireguard"), owner: Account(strconv.Itoa(os.Getuid())), group: Account(strconv.Itoa(os.Getgid()))}
	configFile := places.file("wg0")
	dir := standInPrograms(t, map[string]string{
		"systemctl": `
# wg-quick takes the key from the file at every start, reload, and restart.
apply() { rm -f "$dir/running_key"; [ ! -d "$dir/$1" ] || cp "$dir/$1"/* "$dir/"; }
case "$1" in
show)
	load=loaded; [ -e "$dir/unit" ] || load=not-found
	active=inactive; invocation=; [ ! -e "$dir/failed" ] || active=failed; [ ! -e "$dir/active" ] || { active=active; invocation=4f1c; }
	enabled=disabled; [ ! -e "$dir/enabled" ] || enabled=enabled
	printf 'Id=wg-quick@wg0.service\nNames=wg-quick@wg0.service\nLoadState=%s\nActiveState=%s\nUnitFileState=%s\nInvocationID=%s\nNeedDaemonReload=no\nType=oneshot\nRemainAfterExit=yes\n' $load $active $enabled "$invocation" ;;
enable)
	touch "$dir/enabled"
	if [ "$2" = --now ]; then
		[ ! -e "$dir/refuse-start" ] || { echo "Job for wg-quick@wg0.service failed." >&2; exit 1; }
		touch "$dir/active" "$dir/link"; apply on-start
	fi ;;
disable)
 rm -f "$dir/enabled"
 if [ "$2" = --now ]; then
  # Without the file, the packaged ExecStop fails and the interface stays up,
  # but systemctl still exits 0 and leaves the unit failed.
  if [ -e "$dir/active" ] && [ ! -e "` + configFile + `" ]; then touch "$dir/failed"; else rm -f "$dir/link"; fi
  rm -f "$dir/active"
 fi ;;
reset-failed) rm -f "$dir/failed" ;;
reload) apply on-reload ;;
restart) touch "$dir/active" "$dir/link"; apply on-restart ;;
*) echo "unexpected systemctl $*" >&2; exit 2 ;;
esac`,
		"wg": `
[ "$*" = "show wg0 dump" ] || { echo "unexpected wg $*" >&2; exit 2; }
[ -e "$dir/link" ] || { echo "Unable to access interface: No such device" >&2; exit 1; }
key=$(sed -n 's/^PrivateKey = //p' "` + configFile + `" 2>/dev/null)
[ ! -e "$dir/running_key" ] || key=$(cat "$dir/running_key")
printf '%s\t%s\t%s\toff\n' "${key:-(none)}" "(public)" "$(cat "$dir/listen_port")"
cat "$dir/peers"`,
		"wg-quick": `
[ "$1" = down ] || exit 2
# Ubuntu 26.04's AppArmor profile lets wg-quick read files only in the
# WireGuard directory.
case "$2" in "` + places.directory + `"/*/wg0.conf) ;; *) echo "$2: Permission denied" >&2; exit 3 ;; esac
[ -f "$2" ] || exit 3
[ ! -e "$dir/refuse-down" ] || { echo "wg-quick down failed" >&2; exit 1; }
rm -f "$dir/link"`,
		"ip": `
case "$*" in
"-j address show dev wg0") [ -e "$dir/link" ] || { echo 'Device "wg0" does not exist.' >&2; exit 1; }; cat "$dir/addresses.json" ;;
"-j -4 route show table main dev wg0") cat "$dir/routes4.json" ;;
"-j -6 route show table main dev wg0") echo '[]' ;;
*) echo "unexpected ip $*" >&2; exit 2 ;;
esac`,
		"journalctl": fakeJournalctl,
	})
	writeState(t, filepath.Join(dir, "unit"), "")
	n := &wireguardNode{t: t, dir: dir, places: places}
	n.set("", wireguardState{})
	return n
}

// set writes a running state into dir, or into the directory that a start,
// reload, or restart copies from.
func (n *wireguardNode) set(stage string, s wireguardState) {
	n.t.Helper()
	directory := filepath.Join(n.dir, stage)
	var peers, addresses, routes strings.Builder
	for _, peer := range s.peers {
		fields := strings.Fields(peer)
		fmt.Fprintf(&peers, "%s\t(none)\t(none)\t%s\t0\t0\t0\t%s\n", fields[0], fields[1], fields[2])
	}
	for i, address := range s.addresses {
		local, length, _ := strings.Cut(address, "/")
		fmt.Fprintf(&addresses, `%s{"family":"inet","local":"%s","prefixlen":%s,"scope":"global"}`, map[bool]string{true: "", false: ","}[i == 0], local, length)
	}
	for i, route := range s.routes {
		destination, kernel, _ := strings.Cut(route, " ")
		protocol := ""
		if kernel == "kernel" {
			protocol = `,"protocol":"kernel"`
		}
		fmt.Fprintf(&routes, `%s{"dst":"%s"%s,"scope":"link","flags":[]}`, map[bool]string{true: "", false: ","}[i == 0], destination, protocol)
	}
	writeState(n.t, filepath.Join(directory, "listen_port"), strconv.Itoa(s.listenPort))
	writeState(n.t, filepath.Join(directory, "peers"), peers.String())
	writeState(n.t, filepath.Join(directory, "addresses.json"), `[{"ifindex":5,"ifname":"wg0","mtu":1420,"addr_info":[`+addresses.String()+`]}]`)
	writeState(n.t, filepath.Join(directory, "routes4.json"), "["+routes.String()+"]")
}

// The running state of wireguardTestFile.
var wireguardTestState = wireguardState{listenPort: 51820, peers: []string{wireguardPeerB + " 10.0.0.2/32 off"},
	addresses: []string{"10.0.0.1/24"}, routes: []string{"10.0.0.0/24 kernel"}}

// changes lists the calls of the stand-ins that change the node, and
// forgets every call so far.
func (n *wireguardNode) changes() []string {
	n.t.Helper()
	calls := fixtureCalls(n.t, n.dir)
	writeState(n.t, filepath.Join(n.dir, "calls"), "")
	var changing []string
	for _, call := range strings.Split(strings.TrimSpace(calls), "\n") {
		if strings.HasPrefix(call, "wg-quick down ") {
			changing = append(changing, "wg-quick down <saved>/wg0.conf")
			continue
		}
		if strings.HasPrefix(call, "systemctl ") && !strings.HasPrefix(call, "systemctl show") {
			changing = append(changing, call)
		}
	}
	return changing
}

func (n *wireguardNode) run(dryrun bool, content string) (protocol.Result, value.Scope) {
	n.t.Helper()
	scope, _ := value.Scope{}.Trace()
	w := WireguardInterface{Name: "wg0", Content: &content}
	n.owed().dryrun = dryrun
	return (Runtime{Dryrun: dryrun}).wireguardInterface(testContext(n.t), w, scope, n.places, n.owed()), scope
}

func (n *wireguardNode) key() string {
	n.t.Helper()
	text, err := os.ReadFile(n.places.key("wg0"))
	if err != nil {
		n.t.Fatal(err)
	}
	return strings.TrimSpace(string(text))
}

// noWireguardKey fails when key shows in any form a result is shown or
// saved in.
func noWireguardKey(t *testing.T, key string, results ...protocol.Result) {
	t.Helper()
	for _, r := range results {
		for _, saved := range []protocol.Result{r, r.Safe()} {
			text, err := json.Marshal(saved)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(text), key) || strings.Contains(string(text), clampedWireguardKey(key)) {
				t.Fatalf("the result holds the private key: %s", text)
			}
		}
	}
}

// The first run creates the key on the node, writes it into the file, and
// starts the service; the next run reads the same key and changes nothing.
func TestWireguardInterfaceCreatesTheKeyAndStartsTheInterface(t *testing.T) {
	t.Parallel()
	n := newWireguardNode(t)
	n.set("on-start", wireguardTestState)
	var retained []string
	scope, _ := value.Scope{}.Trace()
	w := WireguardInterface{Name: "wg0", Content: new(wireguardTestFile)}
	run := Runtime{RetainDiff: func(text string) (*assets.Ref, string) { retained = append(retained, text); return nil, "" }}
	r := run.wireguardInterface(testContext(t), w, scope, n.places, n.owed())
	path, keyPath := n.places.file("wg0"), n.places.key("wg0")
	want := "created " + n.places.directory + ", created the private key " + keyPath + ", wrote " + path + ", started wg-quick@wg0.service"
	if r.Status != "succeeded" || r.Change != "changed" || r.Reason != want {
		t.Fatalf("%+v", r)
	}
	key := n.key()
	raw, _ := base64.StdEncoding.DecodeString(key)
	private, err := ecdh.X25519().NewPrivateKey(raw)
	if err != nil || len(raw) != 32 || raw[0]&7 != 0 || raw[31]&0xc0 != 0x40 {
		t.Fatal("the key file holds no clamped WireGuard key")
	}
	if r.PublicKey != base64.StdEncoding.EncodeToString(private.PublicKey().Bytes()) {
		t.Fatalf("public_key %q is not the key file's", r.PublicKey)
	}
	for path, want := range map[string]os.FileMode{n.places.directory: 0700, keyPath: 0600, path: 0600} {
		if got := mode(t, path); got != want {
			t.Errorf("%s has mode %o, want %o", path, got, want)
		}
	}
	if text := readState(t, path); text != "[Interface]\nPrivateKey = "+key+"\n"+strings.TrimPrefix(wireguardTestFile, "[Interface]\n") {
		t.Fatal(text)
	}
	if calls := n.changes(); !reflect.DeepEqual(calls, []string{"systemctl enable --now -- wg-quick@wg0.service"}) {
		t.Fatal(calls)
	}

	// The key never shows: the difference has a note in its place, no full
	// difference is kept, and the engine replaces the key in any other text.
	c := r.FileChange
	if c == nil || c.Path != path || c.Action != "create" || !strings.Contains(c.Diff, "+[Interface]\n+PrivateKey = (the node's private key, not shown)\n+Address = 10.0.0.1/24\n") ||
		c.FullDiff != nil || c.FullDiffOmitted != "full difference not kept: the file holds the node's private key" {
		t.Fatalf("%+v", c)
	}
	if !slices.Contains(scope.LoginsHandedOnlyToATool(), key) {
		t.Fatal("the node task execution engine does not know the key")
	}
	if replaced := (protocol.Result{Reason: "wg: " + key}).WithoutLoginsHandedOnlyToATool(scope.LoginsHandedOnlyToATool()); replaced.Reason != "wg: [redacted]" {
		t.Fatal(replaced.Reason)
	}
	for _, text := range retained {
		if strings.Contains(text, key) {
			t.Fatal("a full difference holds the key")
		}
	}

	again, _ := n.run(false, wireguardTestFile)
	if again.Status != "succeeded" || again.Change != "unchanged" || again.Reason != "" || again.PublicKey != r.PublicKey || again.FileChange != nil || n.key() != key {
		t.Fatalf("%+v", again)
	}
	if calls := n.changes(); len(calls) != 0 {
		t.Fatal(calls)
	}
	preview, _ := n.run(true, wireguardTestFile)
	if preview.Status != "succeeded" || preview.Change != "unchanged" || preview.PublicKey != r.PublicKey {
		t.Fatalf("%+v", preview)
	}
	noWireguardKey(t, key, r, again, preview)

	// A key file that MMX made private again, and one that holds no key.
	if err := os.Chmod(keyPath, 0644); err != nil {
		t.Fatal(err)
	}
	if r, _ := n.run(false, wireguardTestFile); r.Status != "succeeded" || r.Reason != "made "+keyPath+" private to root" || mode(t, keyPath) != 0600 {
		t.Fatalf("%+v", r)
	}
	writeState(t, keyPath, "a forgotten note\n")
	if r, _ := n.run(false, wireguardTestFile); r.Status != "failed" || r.Change != "unchanged" || strings.Contains(r.Reason, "forgotten") ||
		r.Reason != keyPath+" holds no WireGuard private key; remove it with a file task (state: absent), and the next run creates a new key" {
		t.Fatalf("%+v", r)
	}
}

// A dryrun writes nothing. Without a key it cannot know the public key; with
// one it reports the action it would take.
func TestWireguardInterfaceDryrun(t *testing.T) {
	t.Parallel()
	n := newWireguardNode(t)
	preview, scope := n.run(true, wireguardTestFile)
	want := "would create " + n.places.directory + ", would create the private key " + n.places.key("wg0") + ", would write " + n.places.file("wg0") + ", would start wg-quick@wg0.service"
	if preview.Status != "succeeded" || preview.Change != "would_change" || preview.Reason != want+wireguardDryrunReachabilityNote || preview.PublicKey != "" ||
		preview.FileChange == nil || !strings.Contains(preview.FileChange.Diff, "+PrivateKey = (the node's private key, not shown)") {
		t.Fatalf("%+v", preview)
	}
	if _, err := os.Lstat(n.places.directory); !os.IsNotExist(err) {
		t.Fatal("the dryrun made the directory")
	}
	if calls := n.changes(); len(calls) != 0 {
		t.Fatal(calls)
	}
	w := WireguardInterface{Name: "wg0"}
	if unknown := w.UnknownInADryrun(preview); unknown["public_key"] == "" {
		t.Fatal(unknown)
	}
	noWireguardKey(t, scope.LoginsHandedOnlyToATool()[0], preview)

	n.set("on-start", wireguardTestState)
	made, _ := n.run(false, wireguardTestFile)
	if made.Status != "succeeded" {
		t.Fatalf("%+v", made)
	}
	n.changes()
	before := readState(t, n.places.file("wg0"))
	added := wireguardTestFile + "\n[Peer]\nPublicKey = " + wireguardPeerC + "\nAllowedIPs = 10.0.0.3/32\n"
	preview, _ = n.run(true, added)
	if preview.Status != "succeeded" || preview.Change != "would_change" || preview.PublicKey != made.PublicKey ||
		preview.Reason != "would write "+n.places.file("wg0")+", would reload wg-quick@wg0.service: added peer "+wireguardPeerC+wireguardDryrunReachabilityNote {
		t.Fatalf("%+v", preview)
	}
	if w.UnknownInADryrun(preview) != nil || readState(t, n.places.file("wg0")) != before || len(n.changes()) != 0 {
		t.Fatal("the dryrun changed the node")
	}
}

// A peer added and removed by a reload, and a check after the reload that
// finds the peer missing.
func TestWireguardInterfaceReloadsForPeersAndChecksTheResult(t *testing.T) {
	t.Parallel()
	n := newWireguardNode(t)
	n.set("on-start", wireguardTestState)
	if r, _ := n.run(false, wireguardTestFile); r.Status != "succeeded" {
		t.Fatalf("%+v", r)
	}
	n.changes()
	two := wireguardState{listenPort: 51820, peers: []string{wireguardPeerB + " 10.0.0.2/32 off", wireguardPeerC + " 10.0.0.3/32 25"},
		addresses: []string{"10.0.0.1/24"}, routes: []string{"10.0.0.0/24 kernel"}}
	added := wireguardTestFile + "\n[Peer]\nPublicKey = " + wireguardPeerC + "\nAllowedIPs = 10.0.0.3/32\nPersistentKeepalive = 25\n"
	n.set("on-reload", two)
	r, _ := n.run(false, added)
	if r.Status != "succeeded" || r.Change != "changed" || r.Reason != "wrote "+n.places.file("wg0")+", reloaded wg-quick@wg0.service: added peer "+wireguardPeerC {
		t.Fatalf("%+v", r)
	}
	if !strings.Contains(r.FileChange.Diff, "+PublicKey = "+wireguardPeerC) || strings.Contains(r.FileChange.Diff, "PrivateKey") {
		t.Fatalf("%+v", r.FileChange)
	}
	if calls := n.changes(); !reflect.DeepEqual(calls, []string{"systemctl reload -- wg-quick@wg0.service"}) {
		t.Fatal(calls)
	}
	// Someone removed the peer by hand with wg set: the next run puts it back.
	n.set("", wireguardTestState)
	if r, _ := n.run(false, added); r.Status != "succeeded" || r.Reason != "reloaded wg-quick@wg0.service: added peer "+wireguardPeerC {
		t.Fatalf("%+v", r)
	}
	n.changes()
	n.set("on-reload", wireguardTestState)
	if r, _ := n.run(false, wireguardTestFile); r.Status != "succeeded" || r.Reason != "wrote "+n.places.file("wg0")+", reloaded wg-quick@wg0.service: removed peer "+wireguardPeerC {
		t.Fatalf("%+v", r)
	}
	// A reload that leaves the interface as it was fails the task.
	if err := os.RemoveAll(filepath.Join(n.dir, "on-reload")); err != nil {
		t.Fatal(err)
	}
	r, _ = n.run(false, added)
	if r.Status != "failed" || r.Change != "changed" ||
		r.Reason != "Cannot activate or restore WireGuard interface wg0: wg0 still differs from "+n.places.file("wg0")+": peer "+wireguardPeerC+" is missing on wg0." {
		t.Fatalf("%+v", r)
	}
}

// An Address change restarts, and so does a peer whose allowed IP needs a
// route that the interface does not have.
func TestWireguardInterfaceRestartsForWhatAReloadDoesNotApply(t *testing.T) {
	t.Parallel()
	n := newWireguardNode(t)
	n.set("on-start", wireguardTestState)
	if r, _ := n.run(false, wireguardTestFile); r.Status != "succeeded" {
		t.Fatalf("%+v", r)
	}
	n.changes()
	moved := strings.Replace(wireguardTestFile, "10.0.0.1/24", "10.0.0.11/24", 1)
	n.set("on-restart", wireguardState{listenPort: 51820, peers: wireguardTestState.peers, addresses: []string{"10.0.0.11/24"}, routes: wireguardTestState.routes})
	r, _ := n.run(false, moved)
	if r.Status != "succeeded" || r.Reason != "wrote "+n.places.file("wg0")+", restarted wg-quick@wg0.service because Address changed in the file; "+
		"the address 10.0.0.11/24 is missing on wg0; the address 10.0.0.1/24 is on wg0 but not in the file" {
		t.Fatalf("%+v", r)
	}
	if !strings.Contains(r.FileChange.Diff, "-Address = 10.0.0.1/24\n+Address = 10.0.0.11/24\n") {
		t.Fatalf("%+v", r.FileChange)
	}
	if calls := n.changes(); !reflect.DeepEqual(calls, []string{"systemctl restart -- wg-quick@wg0.service"}) {
		t.Fatal(calls)
	}
	routed := strings.Replace(moved, "AllowedIPs = 10.0.0.2/32", "AllowedIPs = 10.0.0.2/32, 10.20.0.0/16", 1)
	n.set("on-restart", wireguardState{listenPort: 51820, peers: []string{wireguardPeerB + " 10.0.0.2/32,10.20.0.0/16 off"}, addresses: []string{"10.0.0.11/24"},
		routes: []string{"10.0.0.0/24 kernel", "10.20.0.0/16"}})
	if r, _ := n.run(false, routed); r.Status != "succeeded" || !strings.Contains(r.Reason, "restarted wg-quick@wg0.service because the allowed IP 10.20.0.0/16 of peer "+wireguardPeerB+" has no route on wg0") {
		t.Fatalf("%+v", r)
	}
	if r, _ := n.run(false, routed); r.Change != "unchanged" {
		t.Fatalf("%+v", r)
	}
	// A failed restart names systemd's message and the journal's lines.
	writeState(t, filepath.Join(n.dir, "journal/wg-quick@wg0.service"), journalEntryText(1<<62, `"MESSAGE":"[#] ip link add wg0 type wireguard"`))
	if err := os.Remove(filepath.Join(n.dir, "active")); err != nil {
		t.Fatal(err)
	}
	writeState(t, filepath.Join(n.dir, "refuse-start"), "")
	r, _ = n.run(false, routed)
	if r.Status != "failed" || r.Change != "unknown" || !strings.HasPrefix(r.Reason, "systemctl enable --now -- wg-quick@wg0.service: exit status 1: Job for wg-quick@wg0.service failed.") ||
		!strings.Contains(r.Reason, "[#] ip link add wg0 type wireguard") {
		t.Fatalf("%+v", r)
	}
}

// A new key replaces a removed key file: the file and the interface get it.
func TestWireguardInterfaceReplacesARemovedKey(t *testing.T) {
	t.Parallel()
	n := newWireguardNode(t)
	n.set("on-start", wireguardTestState)
	first, _ := n.run(false, wireguardTestFile)
	old := n.key()
	n.changes()
	if err := os.Remove(n.places.key("wg0")); err != nil {
		t.Fatal(err)
	}
	// The interface runs with the old key until the reload.
	writeState(t, filepath.Join(n.dir, "running_key"), old)
	r, _ := n.run(false, wireguardTestFile)
	if r.Status != "succeeded" || r.PublicKey == first.PublicKey || n.key() == old ||
		r.Reason != "created the private key "+n.places.key("wg0")+", wrote "+n.places.file("wg0")+", reloaded wg-quick@wg0.service: set the node's private key" {
		t.Fatalf("%+v", r)
	}
	if !strings.Contains(r.FileChange.Diff, "-PrivateKey = (another private key, not shown)\n+PrivateKey = (the node's private key, not shown)\n") {
		t.Fatalf("%+v", r.FileChange)
	}
	noWireguardKey(t, old, r)
	noWireguardKey(t, n.key(), r)
}

// A preshared key of the previous file never shows in the difference.
func TestWireguardInterfaceHidesPresharedKeys(t *testing.T) {
	t.Parallel()
	preshared := "rB4vTxF7m8bQkz4vnI3Ucs9JmAi3wNfgVY4NDy+0x0E="
	key := "2D89IOrRKzQ4TJTt6ztZNniu52+CsaYP+R/u4BgwD2c="
	previous := "[Interface]\nPrivateKey = " + key + "\n# PrivateKey = " + wireguardPeerC + "\n[Peer]\nPublicKey = " + wireguardPeerB + "\nPresharedKey = " + preshared + "\n"
	shown := shownWireguardText(previous, key)
	if shown != "[Interface]\nPrivateKey = (the node's private key, not shown)\n# PrivateKey = (another private key, not shown)\n[Peer]\nPublicKey = "+wireguardPeerB+"\nPresharedKey = (a preshared key, not shown)\n" {
		t.Fatal(shown)
	}
}

// Removal stops and disables the service, then removes both files.
func TestWireguardInterfaceRemoval(t *testing.T) {
	t.Parallel()
	n := newWireguardNode(t)
	n.set("on-start", wireguardTestState)
	if r, _ := n.run(false, wireguardTestFile); r.Status != "succeeded" {
		t.Fatalf("%+v", r)
	}
	n.changes()
	w := WireguardInterface{Name: "wg0", State: "absent"}
	path, keyPath := n.places.file("wg0"), n.places.key("wg0")
	preview := (Runtime{Dryrun: true}).wireguardInterface(testContext(t), w, nil, n.places, n.owed())
	if preview.Status != "succeeded" || preview.Change != "would_change" ||
		preview.Reason != "would stop and disable wg-quick@wg0.service, would remove "+path+", would remove "+keyPath+wireguardDryrunReachabilityNote || len(n.changes()) != 0 {
		t.Fatalf("%+v", preview)
	}
	r := (Runtime{}).wireguardInterface(testContext(t), w, nil, n.places, n.owed())
	if r.Status != "succeeded" || r.Change != "changed" || r.Reason != "stopped and disabled wg-quick@wg0.service, removed "+path+", removed "+keyPath ||
		r.FileChange == nil || r.FileChange.Action != "remove" {
		t.Fatalf("%+v", r)
	}
	if calls := n.changes(); !reflect.DeepEqual(calls, []string{"systemctl disable --now -- wg-quick@wg0.service", "wg-quick down <saved>/wg0.conf", "systemctl reset-failed -- wg-quick@wg0.service"}) {
		t.Fatal(calls)
	}
	// The saved copy for wg-quick down is gone with the files.
	if left, err := os.ReadDir(n.places.directory); err != nil || len(left) != 0 {
		t.Fatal(left, err)
	}
	if r := (Runtime{}).wireguardInterface(testContext(t), w, nil, n.places, n.owed()); r.Status != "succeeded" || r.Change != "unchanged" || len(n.changes()) != 0 {
		t.Fatalf("%+v", r)
	}
	// An interface that the service did not bring up fails the removal.
	writeState(t, keyPath, "left\n")
	writeState(t, filepath.Join(n.dir, "link"), "")
	if r := (Runtime{}).wireguardInterface(testContext(t), w, nil, n.places, n.owed()); r.Status != "failed" || !strings.Contains(r.Reason, "The interface wg0 still exists") {
		t.Fatalf("%+v", r)
	}
}

// Without wireguard-tools a dryrun cannot check the task, and a run fails
// naming the package; removal refuses an unprotected configuration change.
func TestWireguardInterfaceWithoutWireguardTools(t *testing.T) {
	t.Parallel()
	n := newWireguardNode(t)
	if err := os.Remove(filepath.Join(n.dir, "unit")); err != nil {
		t.Fatal(err)
	}
	for dryrun, status := range map[bool]string{true: "not_checked", false: "failed"} {
		r, _ := n.run(dryrun, wireguardTestFile)
		if r.Status != status || !strings.Contains(r.Reason, "wg-quick@.service, which systemd does not find; install the package wireguard-tools") {
			t.Fatalf("%+v", r)
		}
	}
	setWorkerEnvironment(t, "PATH", t.TempDir())
	for dryrun, status := range map[bool]string{true: "not_checked", false: "failed"} {
		r, _ := n.run(dryrun, wireguardTestFile)
		if r.Status != status || r.Reason != "wireguard_interface: "+missingProgramError("wg").Error() {
			t.Fatalf("%+v", r)
		}
		if !dryrun && r.FailureKind != "missing_prerequisite" {
			t.Fatalf("%+v", r)
		}
	}
	writeState(t, n.places.file("wg0"), "[Interface]\n")
	r := (Runtime{}).wireguardInterface(testContext(t), WireguardInterface{Name: "wg0", State: "absent"}, nil, n.places, n.owed())
	if r.Status != "failed" || r.Change != "unchanged" || !strings.Contains(r.Reason, "wg is not installed") || readState(t, n.places.file("wg0")) != "[Interface]\n" {
		t.Fatalf("%+v", r)
	}
}

// The stand-in service saves files before installation and rebuilds activation
// for each process. The real node service owns timers and SSH/HTTPS waits.
type wireguardOwedRestoreForTest struct {
	node          *wireguardNode
	calls         int
	wait          time.Duration
	dryrun        bool
	restore       bool
	refuse        error
	beforeApply   func()
	beforeRestore func()
}

func (n *wireguardNode) owed() *wireguardOwedRestoreForTest {
	if n.restoreService == nil {
		n.restoreService = &wireguardOwedRestoreForTest{node: n}
	}
	return n.restoreService
}
func (s *wireguardOwedRestoreForTest) CheckChangeAllowed(context.Context, string) error {
	return s.refuse
}
func (s *wireguardOwedRestoreForTest) ApplyChange(ctx context.Context, change ChangeWithOwedRestore) protocol.Result {
	t, n := s.node.t, s.node
	s.calls++
	if s.dryrun || change.RestoreConfirmationWait <= 0 {
		return protocol.Failed(fmt.Errorf("a configuration change with an owed restore requires a positive reachability confirmation wait and cannot run during a dryrun."))
	}
	s.wait = change.RestoreConfirmationWait
	if s.beforeApply != nil {
		s.beforeApply()
	}
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	previous, proposed := filepath.Join(directory, "previous"), filepath.Join(directory, "proposed")
	metadata, err := change.PreparedFile.SavePreviousFile(ctx, previous)
	if err != nil {
		t.Fatal(err)
	}
	if !metadata.Existed {
		previous = ""
	}
	if change.PreparedFile.InstallsAFile() {
		if err := change.PreparedFile.SaveProposedFile(ctx, proposed); err != nil {
			t.Fatal(err)
		}
	} else {
		proposed = ""
	}
	for _, path := range []string{previous, proposed} {
		if path != "" && mode(t, path) != 0600 {
			t.Fatal("saved WireGuard key is not private", path)
		}
	}
	rebuild := func() ChangeActivation {
		a, err := newWireguardActivation("/etc/wireguard/wg0.conf", change.ModuleRestoreData)
		if err != nil {
			t.Fatal(err)
		}
		a.(*wireguardActivation).destination = n.places.file("wg0")
		a.(ChangeActivationWithSavedFiles).UseSavedFiles(previous, proposed)
		return a
	}
	result := change.PreparedFile.Install(ctx)
	if result.Status != "succeeded" {
		return result
	}
	runtime := Runtime{PrivateTaskDirectoryRoot: func() (string, error) { return directory, nil }}
	activate := func(a ChangeActivation, restoring bool) error {
		if err := a.PrepareActivation(ctx, runtime, restoring); err != nil {
			return err
		}
		if err := a.Activate(ctx, runtime, restoring); err != nil {
			return err
		}
		if err := a.RemoveObsoleteVirtualDevices(ctx, runtime, restoring); err != nil {
			return err
		}
		return a.CheckActivatedConfiguration(ctx, runtime, restoring)
	}
	if err := activate(rebuild(), false); err != nil {
		result.Status, result.Reason = "failed", err.Error()
	}
	if !s.restore {
		return result
	}
	if s.beforeRestore != nil {
		s.beforeRestore()
	}
	restoreActivation := rebuild()
	restored := (Runtime{}).RestorePreviousFile(ctx, n.places.file("wg0"), previous, metadata)
	if restored.Status != "succeeded" {
		t.Fatal(restored)
	}
	if err := activate(restoreActivation, true); err != nil {
		t.Fatal("restore activation", err)
	}
	result.Status, result.Change = "failed", "changed"
	result.Reason = "No reachability confirmation arrived over a new SSH connection within 2m. The previous WireGuard configuration is restored."
	return result
}

func TestWireguardFileChangesWaitButRuntimeRepairsDoNot(t *testing.T) {
	t.Parallel()
	n := newWireguardNode(t)
	n.set("on-start", wireguardTestState)
	n.owed().beforeApply = func() {
		if _, err := os.Stat(n.places.file("wg0")); !os.IsNotExist(err) {
			t.Fatal("configuration installed before handover", err)
		}
	}
	r, _ := n.run(false, wireguardTestFile)
	if r.Status != "succeeded" || n.owed().calls != 1 || n.owed().wait != 2*time.Minute {
		t.Fatal(r, n.owed().calls, n.owed().wait)
	}
	n.owed().beforeApply = nil
	original := readState(t, n.places.file("wg0"))
	// A peer removed by hand: the file does not change, so no wait can help.
	n.set("", wireguardState{listenPort: 51820, addresses: wireguardTestState.addresses, routes: wireguardTestState.routes})
	n.set("on-reload", wireguardTestState)
	r, _ = n.run(false, wireguardTestFile)
	if r.Status != "succeeded" || r.Change != "changed" || n.owed().calls != 1 || readState(t, n.places.file("wg0")) != original {
		t.Fatal(r)
	}
	for _, marker := range []string{"active", "enabled"} {
		if err := os.Rename(filepath.Join(n.dir, marker), filepath.Join(t.TempDir(), marker)); err != nil {
			t.Fatal(err)
		}
		r, _ = n.run(false, wireguardTestFile)
		if r.Status != "succeeded" || r.Change != "changed" || n.owed().calls != 1 {
			t.Fatal(marker, r)
		}
	}
	// A metadata-only replacement still owes a restore.
	if err := os.Chmod(n.places.file("wg0"), 0644); err != nil {
		t.Fatal(err)
	}
	w := WireguardInterface{Name: "wg0", Content: new(wireguardTestFile), RestoreUnlessControllerConfirmsWithin: "7s"}
	r = (Runtime{}).wireguardInterface(testContext(t), w, nil, n.places, n.owed())
	if r.Status != "succeeded" || n.owed().calls != 2 || n.owed().wait != 7*time.Second || mode(t, n.places.file("wg0")) != 0600 {
		t.Fatal(r)
	}
}

func TestWireguardRestoresNewFileToAbsenceAndKeepsGeneratedKey(t *testing.T) {
	t.Parallel()
	n := newWireguardNode(t)
	n.set("on-start", wireguardTestState)
	n.owed().restore = true
	r, _ := n.run(false, wireguardTestFile)
	if r.Status != "failed" || r.Change != "changed" || !strings.Contains(r.Reason, "SSH") || r.PublicKey == "" || !r.NoOtherTry {
		t.Fatal(r)
	}
	if _, err := os.Stat(n.places.file("wg0")); !os.IsNotExist(err) {
		t.Fatal(err)
	}
	key := n.key()
	want, _ := wireguardPublicKey(key)
	if r.PublicKey != want {
		t.Fatal("public key lost after restore")
	}
	if calls := n.changes(); !reflect.DeepEqual(calls, []string{"systemctl enable --now -- wg-quick@wg0.service", "systemctl disable --now -- wg-quick@wg0.service", "wg-quick down <saved>/wg0.conf", "systemctl reset-failed -- wg-quick@wg0.service"}) {
		t.Fatal(calls)
	}
	n.owed().restore = false
	again, _ := n.run(false, wireguardTestFile)
	if again.Status != "succeeded" || again.PublicKey != want || n.key() != key {
		t.Fatal(again)
	}
}

func TestWireguardRestoreUsesReloadOrRestartAndPreviousServiceState(t *testing.T) {
	for _, c := range []struct {
		name, content, action string
		next                  wireguardState
	}{
		{"reload", strings.Replace(wireguardTestFile, "51820", "51821", 1), "reload", wireguardState{listenPort: 51821, peers: wireguardTestState.peers, addresses: wireguardTestState.addresses, routes: wireguardTestState.routes}},
		{"address", strings.Replace(wireguardTestFile, "10.0.0.1/24", "10.0.0.11/24", 1), "restart", wireguardState{listenPort: 51820, peers: wireguardTestState.peers, addresses: []string{"10.0.0.11/24"}, routes: wireguardTestState.routes}},
		{"route", strings.Replace(wireguardTestFile, "10.0.0.2/32", "10.0.0.2/32, 10.20.0.0/16", 1), "restart", wireguardState{listenPort: 51820, peers: []string{wireguardPeerB + " 10.0.0.2/32,10.20.0.0/16 off"}, addresses: wireguardTestState.addresses, routes: []string{"10.0.0.0/24 kernel", "10.20.0.0/16"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			n := newWireguardNode(t)
			n.set("on-start", wireguardTestState)
			first, _ := n.run(false, wireguardTestFile)
			if first.Status != "succeeded" {
				t.Fatal(first)
			}
			previous := readState(t, n.places.file("wg0"))
			n.changes()
			n.set("on-"+c.action, c.next)
			n.owed().restore = true
			n.owed().beforeRestore = func() { n.set("on-"+c.action, wireguardTestState) }
			r, _ := n.run(false, c.content)
			if r.Status != "failed" || r.PublicKey != first.PublicKey || readState(t, n.places.file("wg0")) != previous {
				t.Fatal(r)
			}
			calls := n.changes()
			if len(calls) != 3 || calls[0] != "systemctl "+c.action+" -- wg-quick@wg0.service" || calls[1] != calls[0] || calls[2] != "systemctl enable -- wg-quick@wg0.service" {
				t.Fatal(calls)
			}
		})
	}
	for _, active := range []bool{false, true} {
		for _, enabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("previous_active_%t_enabled_%t", active, enabled), func(t *testing.T) {
				t.Parallel()
				n := newWireguardNode(t)
				n.set("on-start", wireguardTestState)
				first, _ := n.run(false, wireguardTestFile)
				if first.Status != "succeeded" {
					t.Fatal(first)
				}
				for marker, keep := range map[string]bool{"active": active, "link": active, "enabled": enabled} {
					if !keep {
						if err := os.Rename(filepath.Join(n.dir, marker), filepath.Join(t.TempDir(), marker)); err != nil {
							t.Fatal(err)
						}
					}
				}
				next := strings.Replace(wireguardTestFile, "51820", "51821", 1)
				nextState := wireguardTestState
				nextState.listenPort = 51821
				n.set("on-start", nextState)
				n.set("on-reload", nextState)
				n.owed().restore = true
				n.owed().beforeRestore = func() { n.set("on-reload", wireguardTestState) }
				r, _ := n.run(false, next)
				if r.Status != "failed" {
					t.Fatal(r)
				}
				service, err := (UnitManager{}).Inspect(testContext(t), wireguardUnit("wg0"))
				if err != nil || (service.Active == "active") != active || (service.Enabled == "enabled") != enabled {
					t.Fatal(service, err)
				}
			})
		}
	}
}

func TestWireguardRemovalKeepsKeyOnRestoreAndDeletesKeyAfterConfirmation(t *testing.T) {
	t.Parallel()
	n := newWireguardNode(t)
	n.set("on-start", wireguardTestState)
	first, _ := n.run(false, wireguardTestFile)
	if first.Status != "succeeded" {
		t.Fatal(first)
	}
	keyText := readState(t, n.places.key("wg0"))
	previous := readState(t, n.places.file("wg0"))
	w := WireguardInterface{Name: "wg0", State: "absent"}
	n.owed().restore = true
	restored := (Runtime{}).wireguardInterface(testContext(t), w, nil, n.places, n.owed())
	if restored.Status != "failed" || restored.PublicKey != first.PublicKey || readState(t, n.places.key("wg0")) != keyText || readState(t, n.places.file("wg0")) != previous {
		t.Fatal(restored)
	}
	n.owed().restore = false
	n.owed().beforeApply = func() {
		if readState(t, n.places.key("wg0")) != keyText {
			t.Fatal("key removed before confirmation")
		}
	}
	removed := (Runtime{}).wireguardInterface(testContext(t), w, nil, n.places, n.owed())
	if removed.Status != "succeeded" || removed.PublicKey != first.PublicKey {
		t.Fatal(removed)
	}
	if _, err := os.Stat(n.places.key("wg0")); !os.IsNotExist(err) {
		t.Fatal("key remains after confirmed removal", err)
	}
}

func TestWireguardReusesAnExternallyWrittenKeyAndFixesMetadata(t *testing.T) {
	t.Parallel()
	for _, permissions := range []os.FileMode{0600, 0644} {
		t.Run(fmt.Sprintf("%o", permissions), func(t *testing.T) {
			n := newWireguardNode(t)
			n.set("on-start", wireguardTestState)
			// A known X25519 private key in wg genkey's base64-plus-newline format.
			key := "2D89IOrRKzQ4TJTt6ztZNniu52+CsaYP+R/u4BgwD2c="
			path := n.places.key("wg0")
			writeState(t, path, key+"\n")
			if err := os.Chmod(path, permissions); err != nil {
				t.Fatal(err)
			}
			if os.Geteuid() == 0 {
				if err := os.Chown(path, 12345, 23456); err != nil {
					t.Fatal(err)
				}
			}
			r, _ := n.run(false, wireguardTestFile)
			raw, _ := base64.StdEncoding.DecodeString(key)
			private, err := ecdh.X25519().NewPrivateKey(raw)
			if err != nil {
				t.Fatal(err)
			}
			want := base64.StdEncoding.EncodeToString(private.PublicKey().Bytes())
			if r.Status != "succeeded" || r.PublicKey != want || readState(t, path) != key+"\n" || mode(t, path) != 0600 || strings.Contains(r.Reason, "created the private key") {
				t.Fatal(r)
			}
			info, err := os.Stat(path)
			if err != nil || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) || info.Sys().(*syscall.Stat_t).Gid != uint32(os.Getegid()) {
				t.Fatal(info, err)
			}
		})
	}
}

func TestWireguardActivationRejectsInvalidSavedData(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ destination, data string }{
		{"/etc/wireguard/../wg0.conf", `{"service_action":"reload"}`},
		{"/etc/wireguard/wg+0.conf", `{"service_action":"reload"}`},
		{"/etc/wireguard/wg0;id.conf", `{"service_action":"reload"}`},
		{"/etc/wireguard/wg0.conf", `{"service_action":"reload; id"}`},
		{"/etc/wireguard/wg0.conf", `{"service_action":"reload","command":"id"}`},
		{"/etc/wireguard/wg0.conf", `{"service_action":"stop","enable_service":true}`},
		{"/etc/wireguard/wg0.conf", `{"service_action":"reload"} {}`},
	} {
		if _, err := newWireguardActivation(c.destination, json.RawMessage(c.data)); err == nil {
			t.Fatal("accepted", c)
		}
	}
}

func TestWireguardUnresolvedRestoreBlocksUnchangedAndDryrun(t *testing.T) {
	t.Parallel()
	n := newWireguardNode(t)
	n.set("on-start", wireguardTestState)
	first, _ := n.run(false, wireguardTestFile)
	if first.Status != "succeeded" {
		t.Fatal(first)
	}
	n.changes()
	calls := n.owed().calls
	n.owed().refuse = fmt.Errorf("A previous WireGuard file restore is still owed.")
	for _, dryrun := range []bool{false, true} {
		r, _ := n.run(dryrun, wireguardTestFile)
		if r.Status != "failed" || r.Change != "unchanged" || r.PublicKey != first.PublicKey || n.owed().calls != calls || len(n.changes()) != 0 {
			t.Fatal(r)
		}
	}
}

func TestWireguardDryrunNeverAppliesAFileChange(t *testing.T) {
	for _, scenario := range []string{"new file", "changed file", "removed file"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			n := newWireguardNode(t)
			n.set("on-start", wireguardTestState)
			var beforeFile, beforeKey, publicKey string
			if scenario != "new file" {
				first, _ := n.run(false, wireguardTestFile)
				if first.Status != "succeeded" {
					t.Fatal(first)
				}
				publicKey = first.PublicKey
				beforeFile = readState(t, n.places.file("wg0"))
				beforeKey = readState(t, n.places.key("wg0"))
			}
			n.changes()
			calls := n.owed().calls
			n.owed().dryrun = true
			n.owed().beforeApply = func() { t.Fatal("a dryrun called ApplyChange") }
			w := WireguardInterface{Name: "wg0", Content: new(strings.Replace(wireguardTestFile, "51820", "51821", 1))}
			if scenario == "removed file" {
				w.State, w.Content = "absent", nil
			}
			result := (Runtime{Dryrun: true}).wireguardInterface(testContext(t), w, nil, n.places, n.owed())
			if result.Status != "succeeded" || result.Change != "would_change" || result.PublicKey != publicKey || result.FileChange == nil || n.owed().calls != calls || len(n.changes()) != 0 || !strings.HasSuffix(result.Reason, wireguardDryrunReachabilityNote) {
				t.Fatal(result)
			}
			if scenario == "new file" {
				if _, err := os.Stat(n.places.directory); !os.IsNotExist(err) {
					t.Fatal("dryrun created the WireGuard directory", err)
				}
			} else if readState(t, n.places.file("wg0")) != beforeFile || readState(t, n.places.key("wg0")) != beforeKey {
				t.Fatal("dryrun changed WireGuard files")
			}
			if scenario == "new file" && !strings.Contains(result.FileChange.Diff, "PrivateKey = (the node's private key, not shown)") {
				t.Fatal(result.FileChange)
			}
		})
	}
}

func TestWireguardOwedRestoreStandInRefusesDryrun(t *testing.T) {
	t.Parallel()
	service := newWireguardNode(t).owed()
	service.dryrun = true
	result := service.ApplyChange(testContext(t), ChangeWithOwedRestore{RestoreConfirmationWait: 2 * time.Minute})
	if result.Status != "failed" || result.Reason != "a configuration change with an owed restore requires a positive reachability confirmation wait and cannot run during a dryrun." {
		t.Fatal(result)
	}
}

func TestWireguardRestoresFirstBootConfiguration(t *testing.T) {
	for _, style := range []string{"PostUp key", "native peer overlap"} {
		for _, active := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/active_%t", style, active), func(t *testing.T) {
				t.Parallel()
				n := newWireguardNode(t)
				key := "2D89IOrRKzQ4TJTt6ztZNniu52+CsaYP+R/u4BgwD2c="
				writeState(t, n.places.key("wg0"), key+"\n")
				previous := strings.Replace(wireguardTestFile, "ListenPort = 51820", "ListenPort = 51820\nPostUp = wg set %i private-key /etc/wireguard/%i.private_key", 1)
				if style == "native peer overlap" {
					// wg accepts overlapping AllowedIPs and assigns an address to the
					// last peer; MMX's own file parser rejects duplicate ownership.
					previous = "[Interface]\nPrivateKey = " + key + "\n" + strings.TrimPrefix(wireguardTestFile, "[Interface]\n") +
						"\n[Peer]\nPublicKey = " + wireguardPeerC + "\nAllowedIPs = 10.0.0.2/32\n"
				}
				writeState(t, n.places.file("wg0"), previous)
				writeState(t, filepath.Join(n.dir, "enabled"), "")
				if active {
					writeState(t, filepath.Join(n.dir, "active"), "")
					writeState(t, filepath.Join(n.dir, "link"), "")
					writeState(t, filepath.Join(n.dir, "running_key"), key)
					n.set("", wireguardTestState)
				}
				next := strings.Replace(wireguardTestFile, "51820", "51821", 1)
				nextState := wireguardTestState
				nextState.listenPort = 51821
				n.set("on-restart", nextState)
				n.set("on-start", nextState)
				n.owed().restore = true
				n.owed().beforeRestore = func() {
					// A native previous file is checked by service state and interface
					// existence, rather than MMX's full interpretation of its settings.
					restoredState := wireguardTestState
					restoredState.listenPort = 51822
					n.set("on-restart", restoredState)
					writeState(t, filepath.Join(n.dir, "on-restart/running_key"), key)
					n.changes()
				}
				result, _ := n.run(false, next)
				want, _ := wireguardPublicKey(key)
				if result.Status != "failed" || !strings.Contains(result.Reason, "configuration is restored") || result.PublicKey != want || readState(t, n.places.file("wg0")) != previous || readState(t, n.places.key("wg0")) != key+"\n" {
					t.Fatal(result)
				}
				service, err := (UnitManager{}).Inspect(testContext(t), wireguardUnit("wg0"))
				if err != nil || (service.Active == "active") != active || service.Enabled != "enabled" {
					t.Fatal(service, err)
				}
				calls := n.changes()
				action := "systemctl restart -- wg-quick@wg0.service"
				if !active {
					action = "systemctl disable --now -- wg-quick@wg0.service"
				}
				expected := []string{action, "systemctl enable -- wg-quick@wg0.service"}
				if !reflect.DeepEqual(calls, expected) {
					t.Fatal(calls)
				}
			})
		}
	}
}

func TestWireguardForwardActivationRejectsInvalidSavedProposedFile(t *testing.T) {
	t.Parallel()
	for _, contents := range []string{
		"[Interface]\nPostUp = wg set %i private-key /etc/wireguard/%i.private_key\n",
		"[Interface]\nPrivateKey = " + wireguardPeerB + "\nSaveConfig = true\n",
		"[Interface]\nPrivateKey = " + wireguardPeerB + "\nUnsupportedSetting = value\n",
	} {
		path := filepath.Join(t.TempDir(), "proposed")
		writeState(t, path, contents)
		activation, err := newWireguardActivation("/etc/wireguard/wg0.conf", json.RawMessage(`{"service_action":"restart"}`))
		if err != nil {
			t.Fatal(err)
		}
		activation.(ChangeActivationWithSavedFiles).UseSavedFiles("", path)
		if err := activation.PrepareActivation(testContext(t), Runtime{}, false); err == nil || !strings.Contains(err.Error(), "saved WireGuard configuration file is invalid") {
			t.Fatal(err)
		}
		// Even a saved native file that MMX cannot parse must not expose its key.
		hidden := activation.(*wireguardActivation).withoutKeys(fmt.Errorf("Native diagnostic: %s", wireguardPeerB))
		if strings.Contains(contents, "PrivateKey") && strings.Contains(hidden.Error(), wireguardPeerB) {
			t.Fatal("a saved-file diagnostic exposed a private key")
		}
	}
}

func TestWireguardNativeRestoreChecksServiceStateAndInterfaceExistence(t *testing.T) {
	t.Parallel()
	n := newWireguardNode(t)
	previous := n.places.file("wg0")
	writeState(t, previous, "[Interface]\nPostUp = wg set %i private-key /etc/wireguard/%i.private_key\n")
	for _, marker := range []string{"active", "enabled", "link"} {
		writeState(t, filepath.Join(n.dir, marker), "")
	}
	activation, err := newWireguardActivation("/etc/wireguard/wg0.conf", json.RawMessage(`{"service_action":"restart","previous_service_active":true,"previous_service_enabled":true}`))
	if err != nil {
		t.Fatal(err)
	}
	activation.(ChangeActivationWithSavedFiles).UseSavedFiles(previous, "")
	if err := activation.PrepareActivation(testContext(t), Runtime{}, true); err != nil {
		t.Fatal(err)
	}
	if err := activation.CheckActivatedConfiguration(testContext(t), Runtime{}, true); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(n.dir, "link")
	hiddenLink := filepath.Join(t.TempDir(), "link")
	if err := os.Rename(link, hiddenLink); err != nil {
		t.Fatal(err)
	}
	if err := activation.CheckActivatedConfiguration(testContext(t), Runtime{}, true); err == nil || !strings.Contains(err.Error(), "restored WireGuard interface wg0 does not exist") {
		t.Fatal(err)
	}
	if err := os.Rename(hiddenLink, link); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(n.dir, "enabled"), filepath.Join(t.TempDir(), "enabled")); err != nil {
		t.Fatal(err)
	}
	if err := activation.CheckActivatedConfiguration(testContext(t), Runtime{}, true); err == nil || !strings.Contains(err.Error(), "active or enabled state") {
		t.Fatal(err)
	}
}

func TestWireguardReadsNativeSaveConfigSwitch(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		text  string
		saves bool
	}{
		{"[Interface]\nSaveConfig = true\n", true},
		{" \t[inTERface]\t# interface\r\n\tSaVeCoNfIg\t=\tTrUe \t# enabled\r\n", true},
		{"[INTERFACE]\nSaveConfig=true\n[Peer]\n", true},
		{"[Interface]\nAnUnparsedNativeSetting = value\nSaveConfig = TRUE\n", true},
		{"[Interface]\n# SaveConfig = true\nSaveConfig = false\n", false},
		{"[Interface]\nPostUp = echo SaveConfig = true\n", false},
		{"[Peer]\nSaveConfig = true\n", false},
		{"SaveConfig = true\n[Interface]\n", false},
		{"[Interface]\nSaveConfig = true\nSaveConfig = false\n", false},
		{"[Interface]\nSaveConfig = false\nSaveConfig = true\n", true},
	} {
		if got := wireguardSavesRunningConfiguration(c.text); got != c.saves {
			t.Errorf("%q: %t, want %t", c.text, got, c.saves)
		}
	}
}

func TestWireguardRefusesExistingSaveConfigBeforeAnyChange(t *testing.T) {
	for _, dryrun := range []bool{false, true} {
		for _, state := range []string{"present", "absent"} {
			for _, existingKey := range []bool{false, true} {
				t.Run(fmt.Sprintf("dryrun_%t/state_%s/key_%t", dryrun, state, existingKey), func(t *testing.T) {
					t.Parallel()
					n := newWireguardNode(t)
					path, keyPath := n.places.file("wg0"), n.places.key("wg0")
					previous := "[inTerFace]\nPostUp = wg set %i private-key /etc/wireguard/%i.private_key\n\tSaVeCoNfIg \t=\t TRUE # native switch\n"
					writeState(t, path, previous)
					if err := os.Chmod(n.places.directory, 0755); err != nil {
						t.Fatal(err)
					}
					if err := os.Chmod(path, 0644); err != nil {
						t.Fatal(err)
					}
					key := "2D89IOrRKzQ4TJTt6ztZNniu52+CsaYP+R/u4BgwD2c="
					publicKey := ""
					if existingKey {
						writeState(t, keyPath, key+"\n")
						if err := os.Chmod(keyPath, 0644); err != nil {
							t.Fatal(err)
						}
						publicKey, _ = wireguardPublicKey(key)
					}
					for _, marker := range []string{"active", "enabled", "link"} {
						writeState(t, filepath.Join(n.dir, marker), "")
					}
					before, err := os.Stat(path)
					if err != nil {
						t.Fatal(err)
					}
					owed := n.owed()
					owed.dryrun = dryrun
					w := WireguardInterface{Name: "wg0", State: state}
					if state == "present" {
						w.Content = new(wireguardTestFile)
					}
					result := (Runtime{Dryrun: dryrun}).wireguardInterface(testContext(t), w, nil, n.places, owed)
					want := "WireGuard configuration file " + path + " sets SaveConfig = true, so wg-quick would write the running settings over the file that MMX restores if the change is not confirmed. Remove the SaveConfig line from " + path + ", then run again."
					if result.Status != "failed" || result.Change != "unchanged" || result.FailureKind != "config" || !result.NoOtherTry || result.Reason != want || result.PublicKey != publicKey || result.FileChange != nil || owed.calls != 0 || len(n.changes()) != 0 {
						t.Fatal(result)
					}
					after, err := os.Stat(path)
					if err != nil || !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) || mode(t, path) != 0644 || mode(t, n.places.directory) != 0755 || readState(t, path) != previous {
						t.Fatal("refusal changed the existing configuration", err)
					}
					if existingKey {
						if mode(t, keyPath) != 0644 || readState(t, keyPath) != key+"\n" {
							t.Fatal("refusal changed the private key")
						}
					} else if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
						t.Fatal("refusal created a private key", err)
					}
					for _, marker := range []string{"active", "enabled", "link"} {
						if _, err := os.Stat(filepath.Join(n.dir, marker)); err != nil {
							t.Fatal("refusal changed service state", marker, err)
						}
					}
				})
			}
		}
	}
}

// Ubuntu 26.04's wg-quick profile: its ip child, which brings the interface
// up and so opens the port, lacks capability net_bind_service. The parent's
// rule after the child must not count.
const wireguardUbuntuProfile = `profile wg-quick /usr/bin/wg-quick flags=(attach_disconnected) {
  capability net_admin,
  file mrCx /usr/bin/ip,
  profile ip /usr/bin/ip {
    include <abstractions/base>

    capability net_admin,
    capability sys_module,

    file r @{run}/netns/{,**},
  }
  capability net_bind_service,
  include if exists <local/wg-quick>
}
`

func TestWireguardRefusesALowPortThatAppArmorKeepsFromOpening(t *testing.T) {
	t.Parallel()
	low := strings.Replace(wireguardTestFile, "ListenPort = 51820", "ListenPort = 443", 1)
	enforced := "wg-quick (enforce)\nwg-quick//ip (enforce)\nwg (enforce)\n"
	for _, c := range []struct {
		name, loaded, profile, file string
		refused                     bool
	}{
		{"an enforced ip profile without the capability", enforced, wireguardUbuntuProfile, low, true},
		{"a port of 1024 or above", enforced, wireguardUbuntuProfile, wireguardTestFile, false},
		{"a profile in complain mode", "wg-quick (complain)\nwg-quick//ip (complain)\n", wireguardUbuntuProfile, low, false},
		{"an ip profile with the capability", enforced, strings.Replace(wireguardUbuntuProfile, "capability sys_module,", "capability sys_module, net_bind_service,", 1), low, false},
		{"an ip profile with every capability", enforced, strings.Replace(wireguardUbuntuProfile, "capability sys_module,", "audit capability,", 1), low, false},
	} {
		for _, dryrun := range []bool{false, true} {
			n := newWireguardNode(t)
			apparmor := t.TempDir()
			n.places.apparmorProfiles, n.places.wgQuickProfile = filepath.Join(apparmor, "profiles"), filepath.Join(apparmor, "wg-quick")
			writeState(t, n.places.apparmorProfiles, c.loaded)
			writeState(t, n.places.wgQuickProfile, c.profile)
			r, _ := n.run(dryrun, c.file)
			if !c.refused {
				if dryrun && r.Status != "succeeded" {
					t.Errorf("%s: %+v", c.name, r)
				}
				continue
			}
			want := "ListenPort = 443 is below 1024, but on this node the AppArmor profile wg-quick//ip, in " + n.places.wgQuickProfile +
				", lacks capability net_bind_service. wg-quick brings wg0 up through that profile, so the interface could not open UDP port 443. " +
				"Use a ListenPort of 1024 or above, or change the node's AppArmor policy so that wg-quick//ip may bind the port, then run again."
			if r.Status != "failed" || r.FailureKind != "config" || r.Change != "unchanged" || r.Reason != want || n.owed().calls != 0 || len(n.changes()) != 0 {
				t.Errorf("%s, dryrun %v: %+v", c.name, dryrun, r)
			}
			if _, err := os.Stat(n.places.file("wg0")); !os.IsNotExist(err) {
				t.Errorf("%s: the refusal wrote the file: %v", c.name, err)
			}
		}
	}
}
